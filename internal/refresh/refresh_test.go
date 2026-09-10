package refresh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	obs, ds := Refresh(context.Background(), st, reg, 4)
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

	obs, ds := Refresh(context.Background(), st, reg, 4)
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

	obs, ds := Refresh(context.Background(), st, reg, 4)
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
	obs2, ds2 := Refresh(context.Background(), st, reg, 4)
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
	if _, ds := Refresh(context.Background(), loaded, reg, 4); ds.HasErrors() {
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

	obs, ds := Refresh(context.Background(), st, reg, 4)
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

	obs, ds := Refresh(context.Background(), st, reg, 4)
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

	for _, p := range []int{0, -1} {
		obs, ds := Refresh(context.Background(), st, reg, p)
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

	obs, ds := Refresh(context.Background(), st, reg, 3)
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
	_, ds := Refresh(context.Background(), st, reg, 2)
	if len(ds) != 2 {
		t.Fatalf("got %d diagnostics, want 2", len(ds))
	}
	if !strings.Contains(ds[0].Summary, "aaa") || !strings.Contains(ds[1].Summary, "zzz") {
		t.Errorf("diagnostics = %+v, want aaa before zzz — sorted by address, not by which read finished first", ds)
	}
}
