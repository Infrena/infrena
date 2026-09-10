package executor

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"infra/internal/planner"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
)

func addr(name string) address.Address { return address.Address{Name: name} }

func op(a address.Address, resourceType string, kind planner.OpKind) planner.Operation {
	return planner.Operation{Address: a, Type: resourceType, Kind: kind}
}

func planWith(ops ...planner.Operation) *planner.Plan {
	return &planner.Plan{Version: planner.PlanVersion, Operations: ops}
}

func noDeps(address.Address) []address.Address { return nil }

func depsFrom(m map[string][]string) func(address.Address) []address.Address {
	return func(a address.Address) []address.Address {
		var out []address.Address
		for _, name := range m[a.Name] {
			out = append(out, addr(name))
		}
		return out
	}
}

func newLockedBackend(t *testing.T, environment string) *state.Local {
	t.Helper()
	backend := state.NewLocal(t.TempDir())
	if _, err := backend.Lock(context.Background(), environment); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	return backend
}

// sequencedProvider is a Provider double whose Create can be given a
// per-address delay and records the order calls actually completed in, so
// tests can assert real ordering directly instead of inferring it from
// wall-clock timing.
type sequencedProvider struct {
	resourceType string
	delays       map[string]time.Duration

	mu    sync.Mutex
	order []string
}

func (p *sequencedProvider) Name() string { return "sequenced" }
func (p *sequencedProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *sequencedProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	time.Sleep(p.delays[d.Address.String()])
	p.mu.Lock()
	p.order = append(p.order, d.Address.String())
	p.mu.Unlock()
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *sequencedProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *sequencedProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *sequencedProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *sequencedProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *sequencedProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *sequencedProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = (*sequencedProvider)(nil)

func TestApplyRunsDependenciesBeforeDependents(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &sequencedProvider{resourceType: "test.thing", delays: map[string]time.Duration{}}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// alpha depends on zeta, so zeta must run first — but "alpha" sorts
	// BEFORE "zeta" alphabetically, so an implementation dispatching in
	// address order instead of dependency order would still (wrongly) pass
	// a fixture that happened to agree with alphabetical order.
	plan := planWith(
		op(addr("zeta"), "test.thing", planner.OpCreate),
		op(addr("alpha"), "test.thing", planner.OpCreate),
	)
	g, err := planner.BuildExecution(plan, depsFrom(map[string][]string{"zeta": {"alpha"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v", result.Failed)
	}

	prov.mu.Lock()
	order := append([]string(nil), prov.order...)
	prov.mu.Unlock()
	if len(order) != 2 || order[0] != "zeta" || order[1] != "alpha" {
		t.Fatalf("order = %v, want [zeta alpha] — a resource must be created after what it depends on", order)
	}
}

func TestApplyResultOrderDoesNotDependOnCompletionOrder(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &sequencedProvider{resourceType: "test.thing", delays: map[string]time.Duration{
		"a": 40 * time.Millisecond,
		"b": 0,
	}}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("a"), "test.thing", planner.OpCreate), op(addr("b"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	prov.mu.Lock()
	order := append([]string(nil), prov.order...)
	prov.mu.Unlock()
	if len(order) != 2 || order[0] != "b" || order[1] != "a" {
		t.Fatalf("test setup broken: completion order = %v, want [b a] — b must finish first for this test to prove anything", order)
	}

	want := []address.Address{addr("a"), addr("b")}
	if !reflect.DeepEqual(result.Applied, want) {
		t.Errorf("Applied = %v, want %v — sorted by address regardless of which finished first", result.Applied, want)
	}
}

// barrierProvider blocks inside Create on a two-party sync.WaitGroup
// rendezvous — Done() followed by Wait() — so that neither Create can
// proceed until BOTH have recorded their increment. This is a hard,
// race-free proof that two Creates were mid-flight at once, unlike an
// inference from elapsed time.
//
// An earlier version of this fixture used an unbuffered channel the test
// drained one receive at a time ("<-p.started" twice). That is NOT
// equivalent to a barrier: it only requires that each Create eventually
// sends, not that both are in flight together, and Go's scheduler
// routinely runs a freshly spawned goroutine (held in its P's `runnext`
// slot) to full completion — increment, send, receive, decrement, return —
// before the previously queued sibling goroutine gets a turn at all. That
// pattern was measured to report maxConcurrent == 1 in 19 of 20 runs under
// `go test -race`, including against this same, genuinely-concurrent Apply
// (confirmed separately via TestApplyBoundsGlobalParallelism, which uses
// time.Sleep instead and passes 10/10). A two-party WaitGroup closes that
// gap structurally: Wait() cannot return for either goroutine until both
// have called Done(), so the maxConcurrent recorded before the barrier is
// mechanically guaranteed to include both, every run — and a genuinely
// serial Apply would deadlock here instead of passing, which the test's
// bounded select below turns into a fast, clear failure rather than a hang.
type barrierProvider struct {
	resourceType string
	barrier      sync.WaitGroup

	mu            sync.Mutex
	concurrent    int
	maxConcurrent int
}

func (p *barrierProvider) Name() string { return "barrier" }
func (p *barrierProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *barrierProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.concurrent++
	if p.concurrent > p.maxConcurrent {
		p.maxConcurrent = p.concurrent
	}
	p.mu.Unlock()

	p.barrier.Done()
	p.barrier.Wait()

	p.mu.Lock()
	p.concurrent--
	p.mu.Unlock()
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *barrierProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *barrierProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *barrierProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *barrierProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *barrierProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *barrierProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = (*barrierProvider)(nil)

func TestApplyRunsIndependentOperationsConcurrently(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &barrierProvider{resourceType: "test.thing"}
	prov.barrier.Add(2)
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("a"), "test.thing", planner.OpCreate), op(addr("b"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	done := make(chan struct{})
	go func() {
		defer close(done)
		Apply(context.Background(), plan, g, st, Options{
			Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
		})
	}()

	// A genuinely serial Apply would launch only one Create, which would
	// then block forever in p.barrier.Wait() waiting for a second Done()
	// that never comes — this select turns that hang into a fast, clear
	// failure instead of waiting out the whole test binary's timeout.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Apply did not finish within 5s — the two Creates never both reached the barrier, meaning they were never in flight at the same time")
	}

	prov.mu.Lock()
	max := prov.maxConcurrent
	prov.mu.Unlock()
	if max < 2 {
		t.Fatalf("maxConcurrent = %d, want 2 — independent operations must overlap, not run one at a time", max)
	}
}

// countingProvider tracks concurrent Create calls with an artificial delay,
// for bounding tests where a hard barrier isn't needed.
type countingProvider struct {
	resourceType string
	delay        time.Duration

	mu            sync.Mutex
	concurrent    int
	maxConcurrent int
}

func (p *countingProvider) Name() string { return "counting" }
func (p *countingProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *countingProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.concurrent++
	if p.concurrent > p.maxConcurrent {
		p.maxConcurrent = p.concurrent
	}
	p.mu.Unlock()

	time.Sleep(p.delay)

	p.mu.Lock()
	p.concurrent--
	p.mu.Unlock()
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *countingProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *countingProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *countingProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *countingProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *countingProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *countingProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = (*countingProvider)(nil)

func TestApplyBoundsGlobalParallelism(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &countingProvider{resourceType: "test.thing", delay: 50 * time.Millisecond}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var ops []planner.Operation
	for i := 0; i < 6; i++ {
		ops = append(ops, op(addr(fmt.Sprintf("r%d", i)), "test.thing", planner.OpCreate))
	}
	plan := planWith(ops...)
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	// PerProvider is deliberately set higher than the number of operations
	// (6), so it can never be the binding constraint: every op shares this
	// one provider, so if PerProvider were left equal to Parallelism (as an
	// earlier version of this fixture had it, 3 and 3), a global-bound
	// regression that removed the Parallelism check entirely would still
	// pass this test, because the per-provider check alone would still cap
	// concurrency at 3 — confirmed by deliberately deleting the
	// r.inFlight >= opts.Parallelism check from Apply and re-running this
	// test unmodified: it still passed. Raising PerProvider here closes
	// that gap and makes Parallelism the only thing that can be limiting.
	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 3, PerProvider: 6, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Applied) != 6 {
		t.Fatalf("Applied = %v, want 6 addresses", result.Applied)
	}

	prov.mu.Lock()
	max := prov.maxConcurrent
	prov.mu.Unlock()
	if max > 3 {
		t.Errorf("max concurrent = %d, want at most 3", max)
	}
	if max < 2 {
		t.Errorf("max concurrent = %d, want at least 2 — operations should overlap, not run one at a time", max)
	}
}

// sharedCounters is shared by two boundedProviders standing in for two
// different providers, so a single test can observe both a per-provider
// cap and cross-provider overlap at once.
type sharedCounters struct {
	mu             sync.Mutex
	perProvider    map[string]int
	maxPerProvider map[string]int
	global         int
	maxGlobal      int
}

type boundedProvider struct {
	name         string
	resourceType string
	delay        time.Duration
	shared       *sharedCounters
}

func (p *boundedProvider) Name() string { return p.name }
func (p *boundedProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *boundedProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	s := p.shared
	s.mu.Lock()
	s.perProvider[p.name]++
	if s.perProvider[p.name] > s.maxPerProvider[p.name] {
		s.maxPerProvider[p.name] = s.perProvider[p.name]
	}
	s.global++
	if s.global > s.maxGlobal {
		s.maxGlobal = s.global
	}
	s.mu.Unlock()

	time.Sleep(p.delay)

	s.mu.Lock()
	s.perProvider[p.name]--
	s.global--
	s.mu.Unlock()
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *boundedProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *boundedProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *boundedProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *boundedProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *boundedProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *boundedProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = (*boundedProvider)(nil)

func TestApplyBoundsPerProviderIndependentlyOfGlobalParallelism(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	shared := &sharedCounters{perProvider: map[string]int{}, maxPerProvider: map[string]int{}}
	provX := &boundedProvider{name: "x", resourceType: "test.x", delay: 50 * time.Millisecond, shared: shared}
	provY := &boundedProvider{name: "y", resourceType: "test.y", delay: 50 * time.Millisecond, shared: shared}
	reg := registry.New()
	if err := reg.Register(provX); err != nil {
		t.Fatalf("Register x: %v", err)
	}
	if err := reg.Register(provY); err != nil {
		t.Fatalf("Register y: %v", err)
	}

	plan := planWith(
		op(addr("x0"), "test.x", planner.OpCreate),
		op(addr("x1"), "test.x", planner.OpCreate),
		op(addr("y0"), "test.y", planner.OpCreate),
		op(addr("y1"), "test.y", planner.OpCreate),
	)
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 4, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Applied) != 4 {
		t.Fatalf("Applied = %v, want 4 addresses", result.Applied)
	}

	shared.mu.Lock()
	maxX, maxY, maxGlobal := shared.maxPerProvider["x"], shared.maxPerProvider["y"], shared.maxGlobal
	shared.mu.Unlock()

	if maxX > 1 {
		t.Errorf("provider x max concurrent = %d, want at most 1 — PerProvider: 1", maxX)
	}
	if maxY > 1 {
		t.Errorf("provider y max concurrent = %d, want at most 1 — PerProvider: 1", maxY)
	}
	if maxGlobal < 2 {
		t.Errorf("global max concurrent = %d, want at least 2 — two different providers, each capped at 1, must still run at the same time as each other", maxGlobal)
	}
}

// failingProvider fails Create for a fixed set of addresses.
type failingProvider struct {
	resourceType string
	failAddrs    map[string]bool
}

func (p *failingProvider) Name() string { return "failing" }
func (p *failingProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *failingProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	if p.failAddrs[d.Address.String()] {
		return nil, fmt.Errorf("simulated failure for %s", d.Address)
	}
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *failingProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *failingProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *failingProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *failingProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *failingProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *failingProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = (*failingProvider)(nil)

func TestApplyFailureSkipsDependentsButNotIndependentBranches(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &failingProvider{resourceType: "test.thing", failAddrs: map[string]bool{"root": true}}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(
		op(addr("root"), "test.thing", planner.OpCreate),
		op(addr("child"), "test.thing", planner.OpCreate),
		op(addr("cousin"), "test.thing", planner.OpCreate),
	)
	g, err := planner.BuildExecution(plan, depsFrom(map[string][]string{"root": {"child"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
	})
	_ = ds

	if len(result.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly one entry", result.Failed)
	}
	if _, ok := result.Failed["create:root"]; !ok {
		t.Errorf("Failed = %v, want the key \"create:root\"", result.Failed)
	}

	wantSkipped := []string{"create:child"}
	if !reflect.DeepEqual(result.Skipped, wantSkipped) {
		t.Errorf("Skipped = %v, want exactly %v — not merely containing it", result.Skipped, wantSkipped)
	}

	wantApplied := []address.Address{addr("cousin")}
	if !reflect.DeepEqual(result.Applied, wantApplied) {
		t.Errorf("Applied = %v, want exactly %v — root failed and child was skipped, neither belongs here", result.Applied, wantApplied)
	}
}

func TestApplyDoesNotDispatchWhenContextIsAlreadyCancelled(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	reg := registry.New()
	if err := reg.Register(poisonProvider{t: t, resourceType: "test.thing"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("a"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, _ := Apply(ctx, plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})
	if len(result.Applied) != 0 {
		t.Errorf("Applied = %v, want none — the provider must never be called once ctx is already cancelled", result.Applied)
	}
	if _, ok := st.Get(addr("a")); ok {
		t.Error("state must not contain a — nothing was ever dispatched")
	}
}

// flakyProvider fails Create a fixed number of times, then succeeds.
type flakyProvider struct {
	resourceType string
	failures     int

	mu    sync.Mutex
	calls int
}

func (p *flakyProvider) Name() string { return "flaky" }
func (p *flakyProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *flakyProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n <= p.failures {
		return nil, fmt.Errorf("transient failure #%d", n)
	}
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *flakyProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *flakyProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *flakyProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *flakyProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *flakyProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *flakyProvider) ClassifyError(error) provider.Retryability { return provider.SafeToRetry }

var _ provider.Provider = (*flakyProvider)(nil)

func TestApplyRetriesAccordingToPolicy(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &flakyProvider{resourceType: "test.thing", failures: 2}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("a"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		Retry: RetryPolicy{MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v, want none — the third attempt should have succeeded", result.Failed)
	}
	if len(result.Applied) != 1 {
		t.Fatalf("Applied = %v, want one address", result.Applied)
	}

	prov.mu.Lock()
	calls := prov.calls
	prov.mu.Unlock()
	if calls != 3 {
		t.Errorf("Create called %d times, want 3 — two failures then a success, wired through opts.Retry", calls)
	}
}
