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
	"infra/pkg/value"
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
	prov := &sequencedProvider{resourceType: "test.thing", delays: map[string]time.Duration{
		// zeta is deliberately given a delay and alpha none. Without the
		// dependency edge below, both would be roots free to run
		// concurrently, and alpha — with no delay — would then finish
		// FIRST, producing [alpha zeta], which contradicts the [zeta alpha]
		// this test asserts. That contradiction is what makes the
		// assertion actually prove the edge is enforced, rather than
		// merely agreeing with whatever order two zero-delay roots happen
		// to complete in. See the fix note below for why that distinction
		// is load-bearing, not decorative.
		"zeta": 40 * time.Millisecond,
	}}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// alpha depends on zeta, so zeta must run first — but "alpha" sorts
	// BEFORE "zeta" alphabetically, so an implementation dispatching in
	// address order instead of dependency order would still (wrongly) pass
	// a fixture that happened to agree with alphabetical order.
	//
	// Fix note: an earlier version of this fixture gave both addresses zero
	// delay. With zero delay, deleting the dependency edge entirely (noDeps
	// in place of depsFrom below) still left this test passing 20/20:
	// Walk.Ready() returns ids sorted, so "create:alpha" is spawned before
	// "create:zeta", and Go's scheduler (a freshly spawned goroutine's
	// preferential "runnext" placement) reliably ran the SECOND one first —
	// coincidentally producing exactly the [zeta alpha] this assertion
	// demands, with no dependency enforcement involved at all. Giving zeta
	// a real delay closes that gap: with the edge removed, alpha (no
	// delay) now genuinely finishes first every time, so the assertion
	// fires instead of passing by accident. Verified directly: temporarily
	// replacing depsFrom(...) below with noDeps and running this test 5
	// times produced "order = [alpha zeta], want [zeta alpha]" on every
	// run; restored immediately after.
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

// lifecycleProvider succeeds at Create, Update and Delete, and records which
// addresses each was called for. Every other Provider double in this file
// stubs Update and Delete as provider.ErrNotImplemented (they exist only to
// exercise Create), which is not enough for a test that needs a destroy or
// a replace to actually complete end to end.
type lifecycleProvider struct {
	resourceType string

	mu      sync.Mutex
	created []string
	deleted []string
}

func (p *lifecycleProvider) Name() string { return "lifecycle" }
func (p *lifecycleProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *lifecycleProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.created = append(p.created, d.Address.String())
	p.mu.Unlock()
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *lifecycleProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *lifecycleProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *lifecycleProvider) Delete(ctx context.Context, current *resource.ResourceState) error {
	p.mu.Lock()
	p.deleted = append(p.deleted, current.Address.String())
	p.mu.Unlock()
	return nil
}
func (p *lifecycleProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *lifecycleProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *lifecycleProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = (*lifecycleProvider)(nil)

// TestApplyDestroyRemovesFromState covers invariant 1's other half: apply
// destroys a resource, the provider succeeds, and state must no longer
// list it. Every other test in this file only ever exercises
// planner.OpCreate; before this test, OpDestroy never reached Apply
// anywhere in the repository, so record's res.removed branch for a plain
// destroy (as opposed to a replace's destroy phase) was entirely untested.
func TestApplyDestroyRemovesFromState(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &lifecycleProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("gone")
	plan := planWith(op(a, "test.thing", planner.OpDestroy))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")
	st.Set(&resource.ResourceState{Address: a, Type: "test.thing", Provider: "lifecycle", ProviderID: "gone-1"})

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v", result.Failed)
	}

	prov.mu.Lock()
	deleted := append([]string(nil), prov.deleted...)
	prov.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "gone" {
		t.Fatalf("Delete calls = %v, want exactly [gone]", deleted)
	}

	if _, ok := st.Get(a); ok {
		t.Error("state still holds the destroyed resource after Apply — invariant 1's other half: the provider succeeded, state must not still list it")
	}
}

// TestApplyForgetNeverCallsProviderAndRemovesFromState covers OpForget,
// which — like OpDestroy — never reached Apply in any test in the
// repository before this one. poisonProvider (dispatch_test.go, Task 6)
// fails the test immediately from inside Create/Update/Delete, so if a
// regression ever routed a forget through any of those, this test would
// catch it directly rather than by inference from an absence of calls.
func TestApplyForgetNeverCallsProviderAndRemovesFromState(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	reg := registry.New()
	if err := reg.Register(poisonProvider{t: t, resourceType: "test.thing"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("old")
	plan := planWith(op(a, "test.thing", planner.OpForget))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")
	st.Set(&resource.ResourceState{Address: a, Type: "test.thing", Provider: "poison", ProviderID: "old-1"})

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v — a forget must never call the provider, so it must never fail either", result.Failed)
	}

	// poisonProvider.Create/Update/Delete each call t.Fatal if invoked, so
	// reaching this line at all already proves the provider was never
	// called for the forget. st.Get is the other half: retain's whole
	// point (spec §11, invariant 1) is dropping a resource from management
	// without touching the real infrastructure — state must lose it even
	// though nothing was ever asked to delete anything.
	if _, ok := st.Get(a); ok {
		t.Error("state still holds the forgotten resource after Apply")
	}
}

// TestApplyReplaceDestroysThenCreatesSharingOneOperation covers OpReplace,
// the third operation kind that never reached Apply in any test before this
// round: its two nodes ("destroy:swap" then "create:swap") share the SAME
// *planner.Operation through r.ops, which is keyed by address, not by node
// ID. This proves that sharing works correctly end to end — both phases
// fire, in the right order, and state ends up holding the NEW object under
// the same address the old one occupied, not the destroyed original and not
// both.
func TestApplyReplaceDestroysThenCreatesSharingOneOperation(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &lifecycleProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("swap")
	replaceOp := planner.Operation{
		Address: a,
		Type:    "test.thing",
		Kind:    planner.OpReplace,
		Before:  map[string]value.Value{"x": value.String("old", value.SourceExplicit)},
		After:   map[string]value.Value{"x": value.String("new", value.SourceExplicit)},
	}
	plan := planWith(replaceOp)
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")
	st.Set(&resource.ResourceState{Address: a, Type: "test.thing", Provider: "lifecycle", ProviderID: "swap-old"})

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
	deleted := append([]string(nil), prov.deleted...)
	created := append([]string(nil), prov.created...)
	prov.mu.Unlock()

	if len(deleted) != 1 || deleted[0] != "swap" {
		t.Fatalf("Delete calls = %v, want exactly [swap]", deleted)
	}
	if len(created) != 1 || created[0] != "swap" {
		t.Fatalf("Create calls = %v, want exactly [swap] — both phases of the replace must fire, sharing one *planner.Operation through the address-keyed index", created)
	}

	got, ok := st.Get(a)
	if !ok {
		t.Fatal("state has no entry for swap after a replace — a replace must leave the address present, holding the freshly created object")
	}
	if got.ProviderID != "swap" {
		t.Errorf("ProviderID = %q, want %q — final state must reflect the NEW object from the create phase, not the destroyed original", got.ProviderID, "swap")
	}
}

// TestVerbForMapsEveryOpNodeShapeToTheRightVerb pins verbFor's (Kind, Phase)
// -> Verb mapping directly, over all five shapes dispatch itself
// recognizes. Before this test, nothing in the repository exercised
// verbFor in isolation: TestApplyRetriesAccordingToPolicy only ever
// retries under SafeToRetry, the one classification every verb retries
// under regardless of which verb it is (see retryable's table in
// retry.go), so it cannot distinguish a correct mapping from a broken one.
// Concretely: changing verbFor's create arm to return VerbUpdate, true
// still left every test in the package green, because a create classified
// as an update becomes retryable on ConditionallyRetryable too — precisely
// the case spec §15 forbids by name, since a retried create is how
// duplicate infrastructure appears (see retry.go's own doc on retryable).
// This test catches that mutation directly, without needing a provider or
// a live Apply run at all.
func TestVerbForMapsEveryOpNodeShapeToTheRightVerb(t *testing.T) {
	cases := []struct {
		name     string
		kind     planner.OpKind
		phase    planner.Phase
		wantVerb Verb
		wantOK   bool
	}{
		{"create", planner.OpCreate, planner.PhaseCreate, VerbCreate, true},
		{"replace create phase", planner.OpReplace, planner.PhaseCreate, VerbCreate, true},
		{"update", planner.OpUpdate, planner.PhaseCreate, VerbUpdate, true},
		{"destroy", planner.OpDestroy, planner.PhaseDestroy, VerbDelete, true},
		{"replace destroy phase", planner.OpReplace, planner.PhaseDestroy, VerbDelete, true},
		{"forget", planner.OpForget, planner.PhaseDestroy, VerbInvalid, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := planner.OpNode{Address: addr("x"), Kind: tc.kind, Phase: tc.phase}
			gotVerb, gotOK := verbFor(node)
			if gotVerb != tc.wantVerb || gotOK != tc.wantOK {
				t.Errorf("verbFor(Kind=%s, Phase=%d) = (%s, %v), want (%s, %v)", tc.kind, tc.phase, gotVerb, gotOK, tc.wantVerb, tc.wantOK)
			}
		})
	}
}

// TestApplyEmitsRetryingAndCorrectAttemptCounts pins two fixes together:
// that opts.Retry.OnRetry is wired to emit EventRetrying even when the
// caller supplies no OnRetry of its own (proving Apply supplies its own
// rather than only forwarding one the caller happened to set), and that
// EventSucceeded carries the attempt that actually landed rather than the
// zero value types.go documents as meaning "never attempted."
func TestApplyEmitsRetryingAndCorrectAttemptCounts(t *testing.T) {
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

	var mu sync.Mutex
	var events []Event
	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		Retry: RetryPolicy{MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil }},
		OnEvent: func(e Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v", result.Failed)
	}

	mu.Lock()
	got := append([]Event(nil), events...)
	mu.Unlock()

	var retrying, succeeded []Event
	for _, e := range got {
		switch e.Kind {
		case EventRetrying:
			retrying = append(retrying, e)
		case EventSucceeded:
			succeeded = append(succeeded, e)
		}
	}

	if len(retrying) != 2 {
		t.Fatalf("EventRetrying count = %d, want 2 — two failures before the third attempt succeeded", len(retrying))
	}
	if retrying[0].Attempt != 1 || retrying[1].Attempt != 2 {
		t.Errorf("EventRetrying attempts = [%d %d], want [1 2]", retrying[0].Attempt, retrying[1].Attempt)
	}
	for _, e := range retrying {
		if e.Err == nil {
			t.Errorf("EventRetrying at attempt %d carries no Err", e.Attempt)
		}
	}

	if len(succeeded) != 1 {
		t.Fatalf("EventSucceeded count = %d, want 1", len(succeeded))
	}
	if succeeded[0].Attempt != 3 {
		t.Errorf("EventSucceeded.Attempt = %d, want 3 — the attempt that actually landed, not 0 (\"never attempted\" per types.go)", succeeded[0].Attempt)
	}
}

// TestApplyEmitsFailedWithFinalAttemptCount covers the failure-exhaustion
// half of the same fix: EventFailed must also carry the number of attempts
// actually made, not the zero value.
func TestApplyEmitsFailedWithFinalAttemptCount(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &flakyProvider{resourceType: "test.thing", failures: 100}
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

	var mu sync.Mutex
	var events []Event
	result, _ := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		Retry: RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
		OnEvent: func(e Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	})

	if len(result.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly one entry", result.Failed)
	}

	mu.Lock()
	got := append([]Event(nil), events...)
	mu.Unlock()

	var failed *Event
	for i := range got {
		if got[i].Kind == EventFailed {
			failed = &got[i]
		}
	}
	if failed == nil {
		t.Fatal("no EventFailed emitted")
	}
	if failed.Attempt != 2 {
		t.Errorf("EventFailed.Attempt = %d, want 2 — the number of attempts actually made, not 0 (\"never attempted\" per types.go)", failed.Attempt)
	}
}
