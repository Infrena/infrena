package executor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/retry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

func addr(name string) address.Address { return address.Address{Name: name} }

func op(a address.Address, resourceType string, kind planner.OpKind) planner.Operation {
	// Provider is the implicit instance every project without a `providers:`
	// block uses. Set here rather than left empty because dispatch is by
	// (type, instance), and an empty instance is refused, not guessed.
	return opIn(a, resourceType, kind, "test")
}

// opIn is op with an explicit provider INSTANCE, for the tests that register more
// than one — per-provider parallelism among them, which cannot be observed unless
// the two are distinguishable.
func opIn(a address.Address, resourceType string, kind planner.OpKind, instance string) planner.Operation {
	return planner.Operation{Address: a, Type: resourceType, Kind: kind, Provider: instance}
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
		// concurrently, and alpha — with no delay — would finish FIRST,
		// contradicting the [zeta alpha] this test asserts. That
		// contradiction is what makes the assertion prove the edge is
		// enforced, rather than agreeing with whatever order two zero-delay
		// roots happen to complete in.
		"zeta": 40 * time.Millisecond,
	}}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
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
	if err := reg.Register("test", prov); err != nil {
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
// A channel each Create sends on, drained one receive at a time, is NOT
// equivalent: it only requires that each Create eventually sends, not that
// both are in flight together, and Go's scheduler routinely runs a freshly
// spawned goroutine to completion — increment, send, receive, decrement,
// return — before its queued sibling gets a turn at all, so maxConcurrent
// reads 1 on a genuinely concurrent Apply. Wait() cannot return for either
// goroutine until both have called Done(), so the maxConcurrent recorded
// before the barrier must include both. A serial Apply deadlocks here
// instead, which the bounded select in the test turns into a fast failure.
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
	if err := reg.Register("test", prov); err != nil {
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
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var ops []planner.Operation
	for i := range 6 {
		ops = append(ops, op(addr(fmt.Sprintf("r%d", i)), "test.thing", planner.OpCreate))
	}
	plan := planWith(ops...)
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	// PerProvider is deliberately set higher than the number of operations
	// (6), so it can never be the binding constraint. Every op shares this
	// one provider, so with PerProvider equal to Parallelism the per-provider
	// check alone would cap concurrency at the same number, and removing the
	// global check from Apply entirely would still pass this test. Raising
	// PerProvider makes Parallelism the only thing that can be limiting.
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
	// Two providers are two INSTANCES, and per-provider parallelism is what
	// this test is about, so they must be distinguishable.
	if err := reg.Register("x", provX); err != nil {
		t.Fatalf("Register x: %v", err)
	}
	if err := reg.Register("y", provY); err != nil {
		t.Fatalf("Register y: %v", err)
	}

	plan := planWith(
		opIn(addr("x0"), "test.x", planner.OpCreate, "x"),
		opIn(addr("x1"), "test.x", planner.OpCreate, "x"),
		opIn(addr("y0"), "test.y", planner.OpCreate, "y"),
		opIn(addr("y1"), "test.y", planner.OpCreate, "y"),
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
	if err := reg.Register("test", prov); err != nil {
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
	if err := reg.Register("test", poisonProvider{t: t, resourceType: "test.thing"}); err != nil {
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
	if err := reg.Register("test", prov); err != nil {
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

// TestApplyRespectsCancellationDuringRetryBackoff pins the half of the SIGINT
// design that operationContext's own doc comment argues for: RetryPolicy.Sleep
// must keep receiving Apply's own cancellable ctx, never
// operationContext(ctx). execute calls Attempt(r.ctx, ...), and swapping in
// operationContext(r.ctx) there is invisible to every other test in the repo.
//
// Sleep is deliberately left nil so this runs the real defaultSleep rather
// than a stub: retry_test.go already proves Attempt stops when some Sleep
// returns an error and that defaultSleep honours its own ctx, but neither
// proves the ctx defaultSleep receives here is one a real cancellation can
// reach. Base is 5s so a resurrected uncancellable path is unmistakable —
// the hang guard (6s) is past the backoff, but the promptness assertion
// (under 1s) is not, so a broken implementation fails loudly rather than
// merely running slow.
//
// It also pins that a cancelled backoff makes no second provider call.
// flakyProvider fails exactly once, so a second Create would succeed; calls
// staying at 1 proves the retry never happened, rather than only that Apply
// returned quickly.
func TestApplyRespectsCancellationDuringRetryBackoff(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &flakyProvider{resourceType: "test.thing", failures: 1}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("a"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond) // well after attempt 1 fails and the 5s backoff begins
		cancel()
	}()

	type outcome struct {
		res Result
		ds  diag.Diagnostics
	}
	start := time.Now()
	done := make(chan outcome, 1)
	go func() {
		res, ds := Apply(ctx, plan, g, st, Options{
			Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
			Retry: RetryPolicy{
				MaxAttempts: 2, Base: 5 * time.Second, Max: 5 * time.Second,
				Jitter: func(d time.Duration) time.Duration { return d }, // deterministic: no jitter to shrink the window
				// Sleep left nil: the real defaultSleep, not a stub.
			},
		})
		done <- outcome{res, ds}
	}()

	select {
	case got := <-done:
		if elapsed := time.Since(start); elapsed > 1*time.Second {
			t.Fatalf("Apply took %v to return after a cancelled backoff with a 5s Base — RetryPolicy.Sleep is not receiving a cancellable context; check execute's call to Attempt in apply.go still passes r.ctx, not operationContext(r.ctx)", elapsed)
		}
		if len(got.res.Failed) != 1 {
			t.Fatalf("Failed = %v, want exactly one failed operation", got.res.Failed)
		}
		if len(got.res.Applied) != 0 {
			t.Fatalf("Applied = %v, want none", got.res.Applied)
		}

		prov.mu.Lock()
		calls := prov.calls
		prov.mu.Unlock()
		if calls != 1 {
			t.Fatalf("Create called %d times, want exactly 1 — a cancelled backoff must not lead to a second provider call", calls)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Apply did not return within 6s of a single cancelled 5s backoff")
	}
}

// lifecycleProvider succeeds at Create, Update and Delete, and records which
// addresses each was called for, plus the combined order calls were made
// in. Every other Provider double in this file stubs Update and Delete as
// provider.ErrNotImplemented (they exist only to exercise Create), which is
// not enough for a test that needs a destroy or a replace to actually
// complete end to end.
type lifecycleProvider struct {
	resourceType string
	// failDelete makes Delete fail, for the create_before_destroy case where
	// the new object exists and the old one could not be removed.
	failDelete bool

	mu      sync.Mutex
	created []string
	deleted []string
	// deletedIDs records WHICH OBJECT was deleted, by ProviderID, where
	// `deleted` records only the address. Under create_before_destroy both
	// objects share one address, so the address alone cannot tell "deleted the
	// old one" from "deleted the one we just built" — which is exactly the
	// mistake worth catching, since both calls succeed either way.
	deletedIDs []string
	order      []string
}

func (p *lifecycleProvider) Name() string { return "lifecycle" }
func (p *lifecycleProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *lifecycleProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.created = append(p.created, d.Address.String())
	p.order = append(p.order, "create:"+d.Address.String())
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
	p.deletedIDs = append(p.deletedIDs, current.ProviderID)
	p.order = append(p.order, "delete:"+current.Address.String())
	p.mu.Unlock()
	if p.failDelete {
		return errors.New("the store refused the delete")
	}
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

// TestApplyDestroyRemovesFromState: apply destroys a resource, the provider
// succeeds, and state must no longer list it. Almost every other test in this
// file exercises planner.OpCreate, so this is the only cover on record's
// res.removed branch for a plain destroy, as opposed to a replace's destroy
// phase.
func TestApplyDestroyRemovesFromState(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &lifecycleProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
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
		t.Error("state still holds the destroyed resource after Apply — the provider succeeded, so state must not still list it")
	}

	// isRemoval's whole purpose is exempting a successful destroy from the
	// "must have a state entry" check tracker.result otherwise applies
	// (isolation.go). A destroy is applied PRECISELY by being absent from
	// state, so Applied must still name it even though st.Get above just
	// confirmed state does not. Without this assertion, isRemoval returning
	// false for everything — silently dropping every destroy and forget from
	// Applied — leaves the suite green.
	if !reflect.DeepEqual(result.Applied, []address.Address{a}) {
		t.Errorf("Applied = %v, want exactly [%s] — a destroy is applied precisely by being absent from state", result.Applied, a)
	}
}

// TestApplyForgetNeverCallsProviderAndRemovesFromState is the only cover on
// OpForget reaching Apply. poisonProvider (dispatch_test.go) calls t.Fatal
// from inside Create/Update/Delete, so a regression routing a forget through
// any of those is caught directly rather than inferred from an absence of
// calls.
//
// It does NOT fail immediately, though: t.Fatal calls runtime.Goexit on the
// goroutine that calls it, and here that is a worker (run.launch's goroutine,
// not the test's own), so Goexit unwinds it without ever sending the
// nodeResult launch was about to send. Apply's owner is left blocked forever
// on <-r.results and the failure surfaces as `go test` hitting its own
// timeout. That is the production form of a real failure mode — see the
// corollary on launch's own doc comment — not just a test artefact.
func TestApplyForgetNeverCallsProviderAndRemovesFromState(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	reg := registry.New()
	if err := reg.Register("test", poisonProvider{t: t, resourceType: "test.thing"}); err != nil {
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
	// reaching this line at all already proves the provider was never called
	// for the forget. st.Get is the other half: a forget drops a resource
	// from management without touching the real infrastructure, so state must
	// lose it even though nothing was ever asked to delete anything.
	if _, ok := st.Get(a); ok {
		t.Error("state still holds the forgotten resource after Apply")
	}

	// See the matching assertion in TestApplyDestroyRemovesFromState for why
	// this is checked explicitly rather than inferred.
	if !reflect.DeepEqual(result.Applied, []address.Address{a}) {
		t.Errorf("Applied = %v, want exactly [%s] — a forget is applied precisely by being dropped from management, absent from state", result.Applied, a)
	}
}

// TestApplyReplaceDestroysThenCreatesSharingOneOperation covers OpReplace on
// the HAPPY path: its two nodes ("destroy:swap" then "create:swap") share the
// SAME *planner.Operation through r.ops, which is keyed by address rather
// than by node ID. Both phases must fire, in that order, and state must end
// up holding the NEW object under the address the old one occupied — not the
// destroyed original, and not both. Result.Applied is checked here too, since
// this is the only place in the package where a replace's address reaches
// Applied at all, and types.go documents that its two completed nodes
// collapse into a single entry.
//
// This test cannot distinguish "removed, then correctly re-added" from "never
// removed at all": the create phase's st.Set overwrites the address either
// way, so a missing intermediate st.Remove is invisible here.
// TestApplyReplaceLeavesNoStateWhenCreatePhaseFailsAfterDestroySucceeds below
// isolates that failure path instead.
func TestApplyReplaceDestroysThenCreatesSharingOneOperation(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &lifecycleProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("swap")
	replaceOp := planner.Operation{
		Provider: "test",
		Address:  a,
		Type:     "test.thing",
		Kind:     planner.OpReplace,
		Before:   map[string]value.Value{"x": value.String("old", value.SourceExplicit)},
		After:    map[string]value.Value{"x": value.String("new", value.SourceExplicit)},
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
	order := append([]string(nil), prov.order...)
	prov.mu.Unlock()

	if len(deleted) != 1 || deleted[0] != "swap" {
		t.Fatalf("Delete calls = %v, want exactly [swap]", deleted)
	}
	if len(created) != 1 || created[0] != "swap" {
		t.Fatalf("Create calls = %v, want exactly [swap] — both phases of the replace must fire, sharing one *planner.Operation through the address-keyed index", created)
	}

	wantOrder := []string{"delete:swap", "create:swap"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("call order = %v, want %v — the destroy phase must complete before the create phase begins", order, wantOrder)
	}

	got, ok := st.Get(a)
	if !ok {
		t.Fatal("state has no entry for swap after a replace — a replace must leave the address present, holding the freshly created object")
	}
	if got.ProviderID != "swap" {
		t.Errorf("ProviderID = %q, want %q — final state must reflect the NEW object from the create phase, not the destroyed original", got.ProviderID, "swap")
	}

	wantApplied := []address.Address{a}
	if !reflect.DeepEqual(result.Applied, wantApplied) {
		t.Errorf("Applied = %v, want exactly %v — the replace's two completed nodes at one address must collapse into a single entry, per Result.Applied's documented contract", result.Applied, wantApplied)
	}
}

// deleteSucceedsCreateFailsProvider succeeds at Delete and always fails at
// Create. It exists for one purpose: proving that a replace whose destroy
// phase succeeds and whose create phase then fails leaves state with NO
// entry for the address — never the destroyed original (which no longer
// exists at the provider) and never a half-formed new one.
type deleteSucceedsCreateFailsProvider struct {
	resourceType string

	mu      sync.Mutex
	deleted []string
}

func (p *deleteSucceedsCreateFailsProvider) Name() string { return "half-replace" }
func (p *deleteSucceedsCreateFailsProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *deleteSucceedsCreateFailsProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, fmt.Errorf("simulated create failure for %s", d.Address)
}
func (p *deleteSucceedsCreateFailsProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *deleteSucceedsCreateFailsProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *deleteSucceedsCreateFailsProvider) Delete(ctx context.Context, current *resource.ResourceState) error {
	p.mu.Lock()
	p.deleted = append(p.deleted, current.Address.String())
	p.mu.Unlock()
	return nil
}
func (p *deleteSucceedsCreateFailsProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *deleteSucceedsCreateFailsProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *deleteSucceedsCreateFailsProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = (*deleteSucceedsCreateFailsProvider)(nil)

// TestApplyReplaceLeavesNoStateWhenCreatePhaseFailsAfterDestroySucceeds
// isolates the path the happy-path replace test above cannot reach: the
// destroy phase succeeds — the old object is actually gone at the provider —
// and the create phase then fails. State must not still describe
// infrastructure that no longer exists, which is the worst moment for state
// to lie: a later apply, or a person reading `infrena state show`, would
// believe the OLD, genuinely deleted object is still live.
func TestApplyReplaceLeavesNoStateWhenCreatePhaseFailsAfterDestroySucceeds(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &deleteSucceedsCreateFailsProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("swap")
	replaceOp := planner.Operation{
		Provider: "test",
		Address:  a,
		Type:     "test.thing",
		Kind:     planner.OpReplace,
		Before:   map[string]value.Value{"x": value.String("old", value.SourceExplicit)},
		After:    map[string]value.Value{"x": value.String("new", value.SourceExplicit)},
	}
	plan := planWith(replaceOp)
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")
	st.Set(&resource.ResourceState{Address: a, Type: "test.thing", Provider: "half-replace", ProviderID: "swap-old"})

	result, _ := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})

	if len(result.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly one entry — the create phase must fail", result.Failed)
	}
	if _, ok := result.Failed["create:swap"]; !ok {
		t.Errorf("Failed = %v, want the key \"create:swap\"", result.Failed)
	}

	prov.mu.Lock()
	deleted := append([]string(nil), prov.deleted...)
	prov.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "swap" {
		t.Fatalf("Delete calls = %v, want exactly [swap] — the destroy phase must actually have run before the create phase could fail", deleted)
	}

	if _, ok := st.Get(a); ok {
		t.Error("state still holds an entry for swap after its destroy phase succeeded and its create phase failed — this describes deleted infrastructure as live, and state must not list a resource the provider destroyed")
	}

	// Without this, swap's destroy-phase recordSuccess leaves it in
	// tracker.applied with removal: true, which survives into Result.Applied
	// even though the create phase later failed — reporting swap as BOTH
	// applied and failed, with no state entry either way. A replace's unit of
	// success is the whole replacement, not its destroy half: the address
	// must not appear in Applied when the same run reports it in Failed.
	if len(result.Applied) != 0 {
		t.Errorf("Applied = %v, want empty — a replace whose create phase failed did not successfully change anything the user asked for, even though its destroy phase genuinely ran", result.Applied)
	}
}

// TestVerbForMapsEveryOpNodeShapeToTheRightVerb pins verbFor's (Kind, Phase)
// -> Verb mapping directly, over every shape dispatch recognizes. A live
// Apply run cannot do this job: it only retries under SafeToRetry, the one
// classification every verb retries under regardless of which verb it is (see
// retryable's table in retry.go), so it cannot tell a correct mapping from a
// broken one. A create misclassified as an update becomes retryable on
// ConditionallyRetryable too, and a retried create is how duplicate
// infrastructure appears.
func TestVerbForMapsEveryOpNodeShapeToTheRightVerb(t *testing.T) {
	cases := []struct {
		name     string
		kind     planner.OpKind
		phase    planner.Phase
		wantVerb retry.Verb
		wantOK   bool
	}{
		{"create", planner.OpCreate, planner.PhaseCreate, retry.VerbCreate, true},
		{"replace create phase", planner.OpReplace, planner.PhaseCreate, retry.VerbCreate, true},
		{"update", planner.OpUpdate, planner.PhaseCreate, retry.VerbUpdate, true},
		{"destroy", planner.OpDestroy, planner.PhaseDestroy, retry.VerbDelete, true},
		{"replace destroy phase", planner.OpReplace, planner.PhaseDestroy, retry.VerbDelete, true},
		{"forget", planner.OpForget, planner.PhaseDestroy, retry.VerbInvalid, false},
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
	if err := reg.Register("test", prov); err != nil {
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
	if err := reg.Register("test", prov); err != nil {
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

// TestApplyChainsCallerSuppliedOnRetryAlongsideEventRetrying pins the
// chaining behavior in execute's own local RetryPolicy copy: a caller's
// OnRetry must still fire, exactly once per retry, alongside — not instead
// of — the EventRetrying this run's own wrapper emits. It is the only test
// that sets Options.Retry.OnRetry at all, so without it, dropping the
// userOnRetry call in execute goes unnoticed.
func TestApplyChainsCallerSuppliedOnRetryAlongsideEventRetrying(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &flakyProvider{resourceType: "test.thing", failures: 2}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("a"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	var mu sync.Mutex
	var callerAttempts []int
	var retryingEvents int

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		Retry: RetryPolicy{
			MaxAttempts: 3,
			Sleep:       func(context.Context, time.Duration) error { return nil },
			OnRetry: func(attempt int, err error, delay time.Duration) {
				mu.Lock()
				callerAttempts = append(callerAttempts, attempt)
				mu.Unlock()
			},
		},
		OnEvent: func(e Event) {
			if e.Kind != EventRetrying {
				return
			}
			mu.Lock()
			retryingEvents++
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
	gotAttempts := append([]int(nil), callerAttempts...)
	gotRetrying := retryingEvents
	mu.Unlock()

	if want := []int{1, 2}; !reflect.DeepEqual(gotAttempts, want) {
		t.Fatalf("caller OnRetry attempts = %v, want %v — the caller's own hook must be called exactly once per retry, not skipped in favor of EventRetrying", gotAttempts, want)
	}
	if gotRetrying != 2 {
		t.Fatalf("EventRetrying count = %d, want 2 — must fire alongside the caller's own OnRetry, not instead of it", gotRetrying)
	}
}

// TestApplyCreateBeforeDestroyCreatesThenDeletesTheDeposedObject.
//
// The mirror of TestApplyReplaceDestroysThenCreatesSharingOneOperation, and the
// three things it asserts are the three that make the flag mean anything:
//
//   - the CREATE happens first, so there is no moment when nothing exists;
//   - the DELETE is handed the OLD object, not the new one — getting this wrong
//     destroys what was just built and leaves the original running, and both
//     calls succeed, so nothing reports it;
//   - the address is left present, holding the new object, with no deposed
//     record outstanding.
func TestApplyCreateBeforeDestroyCreatesThenDeletesTheDeposedObject(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &lifecycleProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("swap")
	plan := planWith(planner.Operation{
		Provider:  "test",
		Address:   a,
		Type:      "test.thing",
		Kind:      planner.OpReplace,
		Before:    map[string]value.Value{"x": value.String("old", value.SourceExplicit)},
		After:     map[string]value.Value{"x": value.String("new", value.SourceExplicit)},
		Lifecycle: resource.Lifecycle{CreateBeforeDestroy: true},
	})
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
	order := append([]string(nil), prov.order...)
	deleted := append([]string(nil), prov.deleted...)
	deletedIDs := append([]string(nil), prov.deletedIDs...)
	prov.mu.Unlock()

	wantOrder := []string{"create:swap", "delete:swap"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("call order = %v, want %v — the new object must exist before the old one is removed", order, wantOrder)
	}
	if len(deleted) != 1 {
		t.Fatalf("Delete calls = %v, want exactly one", deleted)
	}
	// BY PROVIDER ID, because both objects share the address: this is what
	// distinguishes removing the old one from removing the one just built.
	if len(deletedIDs) != 1 || deletedIDs[0] != "swap-old" {
		t.Errorf("deleted object = %v, want [swap-old] — deleting the NEW object would destroy what was just built and leave the original running", deletedIDs)
	}

	got, ok := st.Get(a)
	if !ok {
		t.Fatal("state has no entry after a create_before_destroy replace")
	}
	if got.ProviderID != "swap" {
		t.Errorf("ProviderID = %q, want the new object %q", got.ProviderID, "swap")
	}
	if len(got.Deposed) != 0 {
		t.Errorf("Deposed = %+v, want empty — the old object was deleted, so nothing is outstanding", got.Deposed)
	}
	if want := []address.Address{a}; !reflect.DeepEqual(result.Applied, want) {
		t.Errorf("Applied = %v, want %v — a completed replacement leaves the address PRESENT", result.Applied, want)
	}
}

// TestCreateBeforeDestroyKeepsTheOldObjectWhenItsDeleteFails.
//
// The failure that makes the deposed record worth writing to disk. The new
// object exists and is recorded; the old one is still real. Without the record,
// its ProviderID is gone the instant the create phase writes state, and a live
// resource nothing can name bills monthly until a human notices.
func TestCreateBeforeDestroyKeepsTheOldObjectWhenItsDeleteFails(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &lifecycleProvider{resourceType: "test.thing", failDelete: true}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("swap")
	plan := planWith(planner.Operation{
		Provider:  "test",
		Address:   a,
		Type:      "test.thing",
		Kind:      planner.OpReplace,
		After:     map[string]value.Value{"x": value.String("new", value.SourceExplicit)},
		Lifecycle: resource.Lifecycle{CreateBeforeDestroy: true},
	})
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")
	st.Set(&resource.ResourceState{Address: a, Type: "test.thing", Provider: "lifecycle", ProviderID: "swap-old"})

	Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})

	got, ok := st.Get(a)
	if !ok {
		t.Fatal("state lost the address when the deposed delete failed")
	}
	if got.ProviderID != "swap" {
		t.Errorf("ProviderID = %q, want the new object %q — it was created successfully", got.ProviderID, "swap")
	}
	if len(got.Deposed) != 1 {
		t.Fatalf("Deposed = %+v, want the old object kept so something can still name it", got.Deposed)
	}
	if got.Deposed[0].ProviderID != "swap-old" {
		t.Errorf("deposed ProviderID = %q, want %q", got.Deposed[0].ProviderID, "swap-old")
	}
}

// TestADeposedObjectIsCleanedUpByTheNextPlan.
//
// The other half of keeping the old object when its delete failed: keeping it
// is only useful if something later removes it. Without this, a deposed record
// is where leaks accumulate — real, billed, and named by nothing anybody runs.
//
// The address itself is untouched: the resource is present, healthy and
// described by configuration, and only a previous incarnation of it is deleted.
func TestADeposedObjectIsCleanedUpByTheNextPlan(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &lifecycleProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register("test", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("swap")
	plan := planWith(planner.Operation{
		Provider: "test",
		Address:  a,
		Type:     "test.thing",
		Kind:     planner.OpDestroyDeposed,
	})
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	st := state.New("proj", "dev")
	st.Set(&resource.ResourceState{
		Address: a, Type: "test.thing", Provider: "lifecycle", ProviderID: "swap-new",
		Deposed: []*resource.ResourceState{
			{Address: a, Type: "test.thing", Provider: "lifecycle", ProviderID: "swap-old"},
		},
	})

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
	deletedIDs := append([]string(nil), prov.deletedIDs...)
	prov.mu.Unlock()
	if len(deletedIDs) != 1 || deletedIDs[0] != "swap-old" {
		t.Errorf("deleted object = %v, want [swap-old] — the LIVE resource must not be touched", deletedIDs)
	}

	got, ok := st.Get(a)
	if !ok {
		t.Fatal("cleaning up a deposed object removed the live resource's state entry")
	}
	if got.ProviderID != "swap-new" {
		t.Errorf("ProviderID = %q, want the live object %q", got.ProviderID, "swap-new")
	}
	if len(got.Deposed) != 0 {
		t.Errorf("Deposed = %+v, want empty once the old object is gone", got.Deposed)
	}
}
