package refresh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

func newTestRegistry(t *testing.T, cloudPath string) (*registry.Registry, *testprovider.Provider) {
	t.Helper()
	reg := registry.New()
	prov := testprovider.New(cloudPath)
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg, prov
}

func createNetwork(t *testing.T, prov *testprovider.Provider, name string) *resource.ResourceState {
	t.Helper()
	st, err := prov.Create(context.Background(), &resource.DesiredResource{
		Address: address.Address{Name: name},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return st
}

func TestRefreshReadsCurrentProviderState(t *testing.T) {
	dir := t.TempDir()
	reg, prov := newTestRegistry(t, filepath.Join(dir, "cloud.json"))

	created := createNetwork(t, prov, "net")
	st := state.New("myapp", "dev")
	st.Set(created)

	obs, ds := Refresh(context.Background(), st, reg, 4, 4)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	o, ok := obs[created.Address.String()]
	if !ok {
		t.Fatal("missing observation for net")
	}
	if o.Err != nil {
		t.Fatalf("unexpected error: %v", o.Err)
	}
	if o.State == nil {
		t.Fatal("resource exists at the provider; State must not be nil")
	}
	if cidr, _ := o.State.Attributes["cidr"].AsString(); cidr != "10.0.0.0/16" {
		t.Errorf("cidr = %q", cidr)
	}
}

func TestRefreshDetectsDeletionOutsideInfra(t *testing.T) {
	dir := t.TempDir()
	reg, prov := newTestRegistry(t, filepath.Join(dir, "cloud.json"))

	created := createNetwork(t, prov, "net")
	st := state.New("myapp", "dev")
	st.Set(created)

	// Deleted by something other than infra: the same path a person
	// hand-editing the cloud file, or another tool, would take.
	if err := prov.Delete(context.Background(), created); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	obs, ds := Refresh(context.Background(), st, reg, 4, 4)
	if ds.HasErrors() {
		t.Fatalf("a deletion outside infra is not a planning error: %+v", ds)
	}
	o := obs[created.Address.String()]
	if o.State != nil {
		t.Error("State must be nil — the provider reported (nil, nil)")
	}
	if o.Err != nil {
		t.Errorf("Err must be nil — deletion is not a failure: %v", o.Err)
	}
}

func TestRefreshReadErrorIsADiagnosticAndNeverADeletion(t *testing.T) {
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	reg, prov := newTestRegistry(t, cloudPath)

	created := createNetwork(t, prov, "net")
	st := state.New("myapp", "dev")
	st.Set(created)

	// Inject a one-shot read failure directly into the cloud file, the
	// mechanism providers/test/cloud.go defines: a FailureRule keyed by op
	// and address that fires once (Fired: true) and then never again.
	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	cloud.Failures = append(cloud.Failures, testprovider.FailureRule{
		Op:      "read",
		Address: created.Address.String(),
		Nth:     1,
		Message: "simulated provider outage",
	})
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("Save: %v", err)
	}

	obs, ds := Refresh(context.Background(), st, reg, 4, 4)
	if !ds.HasErrors() {
		t.Fatal("a read failure must fail planning for that resource")
	}
	o := obs[created.Address.String()]
	if o.Err == nil {
		t.Fatal("Observation.Err must be set")
	}
	if o.State != nil {
		t.Error("State must not be populated when the read failed")
	}

	// The critical distinction: this resource was never deleted. The
	// injected rule is one-shot, so a second refresh must find the resource
	// exactly where it was — proving the first error was a transient read
	// failure, not the resource going away. Mistaking the first result for
	// absence would have proposed destroying live infrastructure.
	obs2, ds2 := Refresh(context.Background(), st, reg, 4, 4)
	if ds2.HasErrors() {
		t.Fatalf("the injected rule is one-shot; the second refresh must succeed: %+v", ds2)
	}
	o2 := obs2[created.Address.String()]
	if o2.State == nil {
		t.Fatal("the resource still exists; a transient read error must never be mistaken for deletion")
	}
}

func TestRefreshNeverWritesState(t *testing.T) {
	root := t.TempDir()
	cloudPath := filepath.Join(t.TempDir(), "cloud.json")
	reg, prov := newTestRegistry(t, cloudPath)
	created := createNetwork(t, prov, "net")

	backend := state.NewLocal(root)
	if _, err := backend.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(created)
	if err := backend.Put(context.Background(), "dev", st); err != nil {
		t.Fatalf("Put: %v", err)
	}

	statePath := filepath.Join(root, "state", "dev.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Run Refresh against the state Get returns — never against backend —
	// which is the point: Refresh has no way to write state even if it
	// wanted to.
	loaded, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ds := Refresh(context.Background(), loaded, reg, 4, 4); ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the state file changed — Refresh must never write state; only the refresh command in M3 persists observations")
	}
}

func TestRefreshUnregisteredTypeIsADiagnostic(t *testing.T) {
	reg := registry.New() // nothing registered

	st := state.New("myapp", "dev")
	st.Set(&resource.ResourceState{
		Address:    address.Address{Name: "ghost"},
		Type:       "ghost.thing",
		ProviderID: "ghost-1",
	})

	obs, ds := Refresh(context.Background(), st, reg, 4, 4)
	if !ds.HasErrors() {
		t.Fatal("a resource whose type is no longer registered must be a diagnostic, not silently skipped or treated as deleted")
	}
	o := obs["ghost"]
	if o.Err == nil {
		t.Error("Observation.Err must be set")
	}
	if o.State != nil {
		t.Error("State must be nil — nothing could be read")
	}
}

func TestRefreshEmptyStateReturnsEmptyObservations(t *testing.T) {
	reg := registry.New()
	st := state.New("myapp", "dev")

	obs, ds := Refresh(context.Background(), st, reg, 4, 4)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(obs) != 0 {
		t.Errorf("Observations = %v, want none", obs)
	}
}

func TestRefreshTreatsParallelismBelowOneAsOne(t *testing.T) {
	dir := t.TempDir()
	reg, prov := newTestRegistry(t, filepath.Join(dir, "cloud.json"))
	created := createNetwork(t, prov, "net")
	st := state.New("myapp", "dev")
	st.Set(created)

	// Both bounds, since both clamp: a zero-capacity channel would deadlock
	// on its first send, so "below 1 means 1" is the difference between a
	// misconfigured flag and a hang.
	for _, p := range []int{0, -1} {
		obs, ds := Refresh(context.Background(), st, reg, p, p)
		if ds.HasErrors() {
			t.Fatalf("parallelism %d: unexpected diagnostics: %+v", p, ds)
		}
		if len(obs) != 1 {
			t.Fatalf("parallelism %d: Observations = %v, want one entry", p, obs)
		}
	}
}

// delayedProvider is a minimal Provider double used to control read timing
// directly. The real fake provider (providers/test) holds one mutex across
// its entire Read call, which would serialize every read regardless of the
// bound Refresh itself applies — so it cannot prove Refresh's own
// parallelism bound is what is doing the limiting. This double has no such
// lock: only Refresh's semaphore governs how many of its Reads run at once.
type delayedProvider struct {
	resourceType string
	delay        time.Duration
	delays       map[string]time.Duration
	fail         map[string]bool

	mu            sync.Mutex
	concurrent    int
	maxConcurrent int
}

func (p *delayedProvider) Name() string { return "delayed" }

func (p *delayedProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}

func (p *delayedProvider) Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.concurrent++
	if p.concurrent > p.maxConcurrent {
		p.maxConcurrent = p.concurrent
	}
	p.mu.Unlock()

	d := p.delay
	if p.delays != nil {
		if perAddr, ok := p.delays[current.Address.String()]; ok {
			d = perAddr
		}
	}
	if d > 0 {
		time.Sleep(d)
	}

	p.mu.Lock()
	p.concurrent--
	p.mu.Unlock()

	if p.fail[current.Address.String()] {
		return nil, fmt.Errorf("delayed provider: simulated failure for %s", current.Address)
	}
	return current.Clone(), nil
}

func (p *delayedProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *delayedProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *delayedProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}

func (p *delayedProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}

func (p *delayedProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *delayedProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = (*delayedProvider)(nil)

func TestRefreshBoundsConcurrentReads(t *testing.T) {
	const resourceType = "delayed.thing"
	prov := &delayedProvider{resourceType: resourceType, delay: 50 * time.Millisecond}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	st := state.New("myapp", "dev")
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("r%d", i)
		st.Set(&resource.ResourceState{
			Address:    address.Address{Name: name},
			Type:       resourceType,
			ProviderID: name,
		})
	}

	obs, ds := Refresh(context.Background(), st, reg, 3, 3)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(obs) != 8 {
		t.Fatalf("Observations = %d, want 8", len(obs))
	}

	prov.mu.Lock()
	max := prov.maxConcurrent
	prov.mu.Unlock()
	if max > 3 {
		t.Errorf("max concurrent reads = %d, want at most the parallelism bound of 3", max)
	}
	if max < 2 {
		t.Errorf("max concurrent reads = %d, want at least 2 — reads should overlap, not run one at a time", max)
	}
}

// TestRefreshBoundsReadsPerProviderIndependentlyOfGlobalParallelism pins
// spec §10's second bound: refresh is "bounded by the same per-provider
// semaphore the executor uses (§15)".
//
// The bounds are set far apart (global 8, per-provider 2) on purpose. With
// them equal — the shape the executor's own bounds test originally had, and
// the shape both commands wired for the whole milestone — the global
// semaphore admits at most `parallelism` reads anyway, so the per-provider
// one can never fire and removing it entirely changes nothing observable.
// Only a per-provider bound strictly below the global one measures anything.
//
// Refresh is the widest fan-out in the product: it reads EVERY resource in
// state, so it is exactly the operation §34's throttling protection is for.
func TestRefreshBoundsReadsPerProviderIndependentlyOfGlobalParallelism(t *testing.T) {
	const resourceType = "delayed.thing"
	prov := &delayedProvider{resourceType: resourceType, delay: 50 * time.Millisecond}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	st := state.New("myapp", "dev")
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("r%d", i)
		st.Set(&resource.ResourceState{
			Address:    address.Address{Name: name},
			Type:       resourceType,
			ProviderID: name,
		})
	}

	obs, ds := Refresh(context.Background(), st, reg, 8, 2)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(obs) != 8 {
		t.Fatalf("Observations = %d, want 8 — the per-provider bound must throttle reads, never drop them", len(obs))
	}

	prov.mu.Lock()
	max := prov.maxConcurrent
	prov.mu.Unlock()
	if max > 2 {
		t.Errorf("max concurrent reads against one provider = %d, want at most the per-provider bound of 2 (global was 8)", max)
	}
	if max < 2 {
		t.Errorf("max concurrent reads = %d, want 2 — the bound must throttle, not serialize", max)
	}
}

func TestRefreshDiagnosticsAreSortedByAddressNotCompletionOrder(t *testing.T) {
	const resourceType = "delayed.thing"
	prov := &delayedProvider{
		resourceType: resourceType,
		delays: map[string]time.Duration{
			"zzz": 0,
			"aaa": 40 * time.Millisecond,
		},
		fail: map[string]bool{"zzz": true, "aaa": true},
	}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	st := state.New("myapp", "dev")
	for _, name := range []string{"zzz", "aaa"} {
		st.Set(&resource.ResourceState{Address: address.Address{Name: name}, Type: resourceType, ProviderID: name})
	}

	// zzz has no delay and fails almost immediately; aaa is deliberately
	// slower. If diagnostics reflected completion order, zzz would come
	// first despite sorting after aaa alphabetically.
	_, ds := Refresh(context.Background(), st, reg, 2, 2)
	if len(ds) != 2 {
		t.Fatalf("got %d diagnostics, want 2", len(ds))
	}
	if !strings.Contains(ds[0].Summary, "aaa") || !strings.Contains(ds[1].Summary, "zzz") {
		t.Errorf("diagnostics = %+v, want aaa before zzz — sorted by address, not by which read finished first", ds)
	}
}

// mutatingReadProvider is a Provider double whose Read mutates the
// *resource.ResourceState it is handed instead of treating it as read-only.
// It exists to prove Refresh clones state before handing it to a provider —
// pkg/resource.ResourceState.Clone's own doc comment names this package:
// "Refresh and planning must never mutate the state that was loaded from
// disk." If Refresh ever hands a provider the live pointer state.State
// holds, this double corrupts that state in memory with no write call
// anywhere in the trace.
type mutatingReadProvider struct {
	resourceType string
}

func (p *mutatingReadProvider) Name() string { return "mutating" }

func (p *mutatingReadProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}

func (p *mutatingReadProvider) Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	current.ProviderID = "mutated-in-place"
	current.Attributes["injected"] = value.String("hacked", value.SourceProvider)
	return current.Clone(), nil
}

func (p *mutatingReadProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *mutatingReadProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *mutatingReadProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}

func (p *mutatingReadProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}

func (p *mutatingReadProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *mutatingReadProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = (*mutatingReadProvider)(nil)

func TestRefreshDoesNotExposeLiveStateToProviderRead(t *testing.T) {
	const resourceType = "mutating.thing"
	prov := &mutatingReadProvider{resourceType: resourceType}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	addr := address.Address{Name: "net"}
	st := state.New("myapp", "dev")
	st.Set(&resource.ResourceState{
		Address:    addr,
		Type:       resourceType,
		ProviderID: "orig-id",
		Attributes: map[string]value.Value{
			"cidr": value.String("10.0.0.0/16", value.SourceProvider),
		},
	})

	if _, ds := Refresh(context.Background(), st, reg, 1, 1); ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	// The state.State Refresh was handed must be exactly as it was before:
	// a provider that mutates the *resource.ResourceState it receives must
	// never be able to corrupt the state Refresh was given, because Refresh
	// never writes and ResourceState.Clone's contract applies to this
	// package by name.
	after, ok := st.Get(addr)
	if !ok {
		t.Fatal("resource vanished from state — Refresh must never mutate st")
	}
	if after.ProviderID != "orig-id" {
		t.Errorf("ProviderID = %q, want unchanged %q — the provider's Read mutated the live state pointer", after.ProviderID, "orig-id")
	}
	if _, injected := after.Attributes["injected"]; injected {
		t.Error("state gained an attribute the provider injected by mutating the live pointer in place")
	}
	if cidr, _ := after.Attributes["cidr"].AsString(); cidr != "10.0.0.0/16" {
		t.Errorf("cidr = %q, state was mutated", cidr)
	}
}

// countingReadProvider counts how many times Read is invoked, to prove
// Refresh skips calling a provider's Read entirely once its context is
// already cancelled, rather than depending on the provider to notice.
type countingReadProvider struct {
	resourceType string
	calls        int32
}

func (p *countingReadProvider) Name() string { return "counting" }

func (p *countingReadProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}

func (p *countingReadProvider) Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	atomic.AddInt32(&p.calls, 1)
	return current.Clone(), nil
}

func (p *countingReadProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *countingReadProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *countingReadProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}

func (p *countingReadProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}

func (p *countingReadProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *countingReadProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = (*countingReadProvider)(nil)

func TestRefreshSkipsProviderReadWhenContextAlreadyCancelled(t *testing.T) {
	const resourceType = "counting.thing"
	prov := &countingReadProvider{resourceType: resourceType}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	st := state.New("myapp", "dev")
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("r%d", i)
		st.Set(&resource.ResourceState{Address: address.Address{Name: name}, Type: resourceType, ProviderID: name})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	obs, ds := Refresh(ctx, st, reg, 4, 4)
	if !ds.HasErrors() {
		t.Fatal("a cancelled refresh must surface as diagnostics, not silently succeed")
	}
	if len(obs) != 4 {
		t.Fatalf("Observations = %d, want 4", len(obs))
	}
	for addr, o := range obs {
		if o.Err == nil {
			t.Errorf("%s: Err must be set when the context was already cancelled", addr)
		}
		if o.State != nil {
			t.Errorf("%s: State must be nil — cancellation is never treated as deletion", addr)
		}
	}
	if calls := atomic.LoadInt32(&prov.calls); calls != 0 {
		t.Errorf("provider Read was called %d times; want 0 — Refresh must check ctx before dispatching, not rely on the provider to notice", calls)
	}
}
