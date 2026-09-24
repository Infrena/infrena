package executor

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// recordingProvider records the *resource.ResourceState it is handed as
// `current`. The fake provider in providers/test cannot show what these tests
// need: its Update applies `desired` wholesale and never looks at `current`,
// so it behaves identically whether it is given the last-persisted state or
// the refreshed observation.
type recordingProvider struct {
	resourceType string
	// echoCurrent makes Update return the very *resource.ResourceState it was
	// handed, with only the desired attributes written onto it. That is the
	// shape every out-of-process provider persists: internal/pluginhost's
	// rebuild carries the bookkeeping fields off `current` onto what a plugin
	// returned.
	echoCurrent bool

	mu      sync.Mutex
	updates []*resource.ResourceState
	deletes []*resource.ResourceState
}

func (p *recordingProvider) Name() string { return "recording" }
func (p *recordingProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}

func (p *recordingProvider) Create(_ context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	return &resource.ResourceState{Address: d.Address, Type: d.Type, ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}

func (p *recordingProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *recordingProvider) Update(_ context.Context, current *resource.ResourceState, d *resource.DesiredResource) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.updates = append(p.updates, current.Clone())
	p.mu.Unlock()
	if p.echoCurrent {
		out := current.Clone()
		out.Attributes = d.Attrs
		return out, nil
	}
	return &resource.ResourceState{Address: d.Address, Type: d.Type, ProviderID: current.ProviderID, Attributes: d.Attrs}, nil
}

func (p *recordingProvider) Delete(_ context.Context, current *resource.ResourceState) error {
	p.mu.Lock()
	p.deletes = append(p.deletes, current.Clone())
	p.mu.Unlock()
	return nil
}

func (p *recordingProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}

func (p *recordingProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *recordingProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

func (p *recordingProvider) updated() []*resource.ResourceState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*resource.ResourceState(nil), p.updates...)
}

func (p *recordingProvider) deleted() []*resource.ResourceState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*resource.ResourceState(nil), p.deletes...)
}

var _ provider.Provider = (*recordingProvider)(nil)

var observedFixtureCreated = time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)

// observedFixture is the shape every test below starts from: one resource
// whose last-persisted attributes say size 10, whose refreshed observation
// says size 99 (somebody resized it outside infrena), and whose configuration
// still asks for 10. The plan is therefore a single update back to 10, and
// each test asks what `current` the provider sees while making it.
func observedFixture(t *testing.T, prov provider.Provider) (*state.State, *planner.Plan, *registry.Registry, *state.Local, address.Address) {
	t.Helper()
	backend := newLockedBackend(t, "dev")
	reg := registry.New()
	if err := reg.Register("recording", prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	a := addr("db")
	st := state.New("proj", "dev")
	st.Set(&resource.ResourceState{
		Address:      a,
		Type:         "test.thing",
		Provider:     "recording",
		ProviderID:   "db-1",
		Attributes:   map[string]value.Value{"size": value.Int(10, value.SourceExplicit)},
		Dependencies: []address.Address{addr("net")},
		Lifecycle:    resource.Lifecycle{PreventDestroy: true, IgnoreChanges: []string{"tags"}},
		CreatedAt:    observedFixtureCreated,
	})

	operation := planner.Operation{
		Address:   a,
		Type:      "test.thing",
		Kind:      planner.OpUpdate,
		Provider:  "recording",
		Before:    map[string]value.Value{"size": value.Int(10, value.SourceExplicit)},
		After:     map[string]value.Value{"size": value.Int(10, value.SourceExplicit)},
		Lifecycle: resource.Lifecycle{PreventDestroy: true, IgnoreChanges: []string{"tags"}},
		DependsOn: []address.Address{addr("net")},
	}
	return st, planWith(operation), reg, backend, a
}

// observedState is what refresh saw: size 99, and the same Address, Type and
// ProviderID, because refresh hands the provider the state it already holds.
// It deliberately carries neither Lifecycle, Dependencies nor CreatedAt: an
// in-process provider is under no obligation to return them, and the host is
// the thing that owns them.
func observedState(a address.Address) *resource.ResourceState {
	return &resource.ResourceState{
		Address:    a,
		Type:       "test.thing",
		ProviderID: "db-1",
		Attributes: map[string]value.Value{"size": value.Int(99, value.SourceProvider)},
	}
}

// An update must be handed the refreshed observation as `current`. Built from
// the state loaded off disk instead, a provider that diffs current against
// desired sees size 10 == 10, concludes nothing has changed, and skips the
// very drift the plan proposed to correct.
func TestApplyUpdateReceivesTheObservationNotTheLastPersistedState(t *testing.T) {
	prov := &recordingProvider{resourceType: "test.thing"}
	st, plan, reg, backend, a := observedFixture(t, prov)

	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		Observed: map[string]*resource.ResourceState{a.String(): observedState(a)},
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Applied) != 1 {
		t.Fatalf("Applied = %v, want [db]", result.Applied)
	}

	updates := prov.updated()
	if len(updates) != 1 {
		t.Fatalf("Update called %d times, want 1", len(updates))
	}
	got := updates[0]
	if got == nil {
		t.Fatal("Update received a nil current")
	}
	if n, _ := got.Attributes["size"].AsInt(); n != 99 {
		t.Errorf("Update saw current size = %d, want 99 — the executor must hand the provider the refreshed observation, not the last state it persisted", n)
	}
	if src := got.Attributes["size"].Source; src != value.SourceProvider {
		t.Errorf("Update saw current size sourced from %v, want %v — the observation must reach the provider unchanged, not re-derived", src, value.SourceProvider)
	}
}

// `current` is a merge, not a straight swap. An observation carries only what
// a provider owns — the provider ID and the attributes; every other field on a
// ResourceState is the host's. Replacing `current` outright would hand those
// fields to the provider empty, and for an out-of-process provider, whose
// persisted state is rebuilt from `current`, empty is what would reach disk: a
// vanished prevent_destroy guard and lost destroy-ordering edges.
func TestApplyUpdateKeepsHostBookkeepingOnTheObservedCurrent(t *testing.T) {
	prov := &recordingProvider{resourceType: "test.thing"}
	st, plan, reg, backend, a := observedFixture(t, prov)

	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	if _, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		Observed: map[string]*resource.ResourceState{a.String(): observedState(a)},
	}); ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	updates := prov.updated()
	if len(updates) != 1 {
		t.Fatalf("Update called %d times, want 1", len(updates))
	}
	got := updates[0]
	if !got.Lifecycle.PreventDestroy {
		t.Error("current.Lifecycle.PreventDestroy = false, want true — lifecycle is the host's bookkeeping and must survive the merge")
	}
	if !reflect.DeepEqual(got.Lifecycle.IgnoreChanges, []string{"tags"}) {
		t.Errorf("current.Lifecycle.IgnoreChanges = %v, want [tags]", got.Lifecycle.IgnoreChanges)
	}
	if !reflect.DeepEqual(got.Dependencies, []address.Address{addr("net")}) {
		t.Errorf("current.Dependencies = %v, want [net] — dependencies are the only source of destroy-ordering edges and no provider reports them", got.Dependencies)
	}
	if got.Provider != "recording" {
		t.Errorf("current.Provider = %q, want %q — the provider INSTANCE is the host's and says which account this resource belongs to", got.Provider, "recording")
	}
	if !got.CreatedAt.Equal(observedFixtureCreated) {
		t.Errorf("current.CreatedAt = %v, want %v", got.CreatedAt, observedFixtureCreated)
	}
	if got.ProviderID != "db-1" || got.Address.String() != a.String() || got.Type != "test.thing" {
		t.Errorf("current identity = (%q, %s, %q), want (db-1, db, test.thing)", got.ProviderID, got.Address, got.Type)
	}
}

// Whatever the executor persists must stay correct once observed values flow
// into `current`. echoCurrent models internal/pluginhost.rebuild, which builds
// the state the executor records out of `current`'s bookkeeping fields, so an
// observation-shaped `current` with an empty Lifecycle or a zero CreatedAt
// would not merely be handed to the provider wrong, it would be written to
// disk wrong.
func TestApplyUpdateWithObservedCurrentStillPersistsHostBookkeeping(t *testing.T) {
	prov := &recordingProvider{resourceType: "test.thing", echoCurrent: true}
	st, plan, reg, backend, a := observedFixture(t, prov)

	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		Observed: map[string]*resource.ResourceState{a.String(): observedState(a)},
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	stored, ok := result.State.Get(a)
	if !ok {
		t.Fatal("db is not in state after a successful update")
	}
	if !stored.Lifecycle.PreventDestroy {
		t.Error("persisted Lifecycle.PreventDestroy = false — a prevent_destroy guard that does not reach state is a guard that silently does nothing")
	}
	if !reflect.DeepEqual(stored.Dependencies, []address.Address{addr("net")}) {
		t.Errorf("persisted Dependencies = %v, want [net]", stored.Dependencies)
	}
	if !stored.CreatedAt.Equal(observedFixtureCreated) {
		t.Errorf("persisted CreatedAt = %v, want %v — feeding observed values into current must not erase when the resource was created", stored.CreatedAt, observedFixtureCreated)
	}
	if n, _ := stored.Attributes["size"].AsInt(); n != 10 {
		t.Errorf("persisted size = %d, want 10 — state records what the update made true, not what the drift was", n)
	}
}

// A nil observation must never reach Update. An address can lack a usable
// observation three ways — no entry at all (the saved-plan path does not
// refresh), an entry whose read failed, and an entry reporting the resource is
// gone — and dispatch refuses a nil `current` outright, so a merge that
// returned the observation unconditionally would turn every one of them into a
// failed operation.
func TestApplyUpdateFallsBackToStateWhenNothingWasObserved(t *testing.T) {
	cases := []struct {
		name     string
		observed func(address.Address) map[string]*resource.ResourceState
	}{
		{"no observations at all", func(address.Address) map[string]*resource.ResourceState { return nil }},
		{"no entry for this address", func(address.Address) map[string]*resource.ResourceState {
			return map[string]*resource.ResourceState{"other": observedState(addr("other"))}
		}},
		{"entry present but nothing was seen", func(a address.Address) map[string]*resource.ResourceState {
			return map[string]*resource.ResourceState{a.String(): nil}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProvider{resourceType: "test.thing"}
			st, plan, reg, backend, a := observedFixture(t, prov)

			g, err := planner.BuildExecution(plan, noDeps)
			if err != nil {
				t.Fatalf("BuildExecution: %v", err)
			}
			result, ds := Apply(context.Background(), plan, g, st, Options{
				Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
				Observed: tc.observed(a),
			})
			if ds.HasErrors() {
				t.Fatalf("unexpected diagnostics: %+v", ds)
			}
			if len(result.Applied) != 1 {
				t.Fatalf("Applied = %v, want [db] — an update with no observation must still run against the last persisted state", result.Applied)
			}
			updates := prov.updated()
			if len(updates) != 1 {
				t.Fatalf("Update called %d times, want 1", len(updates))
			}
			if n, _ := updates[0].Attributes["size"].AsInt(); n != 10 {
				t.Errorf("Update saw current size = %d, want the last persisted 10", n)
			}
			if updates[0].ProviderID != "db-1" {
				t.Errorf("current.ProviderID = %q, want db-1 — without it a provider cannot find the object it manages", updates[0].ProviderID)
			}
		})
	}
}

// Delete is the other verb dispatch hands `current` to. A delete that needs
// more than the ID to find its object — one identified by a field the last
// apply did not set, say — has the same stale-input problem an update does.
func TestApplyDestroyReceivesTheObservation(t *testing.T) {
	prov := &recordingProvider{resourceType: "test.thing"}
	st, _, reg, backend, a := observedFixture(t, prov)

	plan := planWith(planner.Operation{
		Address:  a,
		Type:     "test.thing",
		Kind:     planner.OpDestroy,
		Provider: "recording",
		Before:   map[string]value.Value{"size": value.Int(10, value.SourceExplicit)},
	})
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	if _, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		Observed: map[string]*resource.ResourceState{a.String(): observedState(a)},
	}); ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	deletes := prov.deleted()
	if len(deletes) != 1 {
		t.Fatalf("Delete called %d times, want 1", len(deletes))
	}
	if n, _ := deletes[0].Attributes["size"].AsInt(); n != 99 {
		t.Errorf("Delete saw current size = %d, want the observed 99", n)
	}
	if !deletes[0].Lifecycle.PreventDestroy {
		t.Error("Delete lost the host's lifecycle bookkeeping")
	}
}

// withDeposed gives the fixture's resource an object a failed
// create_before_destroy left behind, as state would hold it.
func withDeposed(t *testing.T, st *state.State, a address.Address) {
	t.Helper()
	cur, ok := st.Get(a)
	if !ok {
		t.Fatal("fixture has no resource to depose from")
	}
	next := cur.Clone()
	next.Deposed = []*resource.ResourceState{{Address: a, Type: "test.thing", Provider: "recording", ProviderID: "db-0"}}
	st.Set(next)
}

// Deposed is the host's, like Lifecycle and Dependencies, and an update is not
// a reason to forget it. It is the only record that a failed replacement left a
// real object running, and the plan that would clean it up is gated on it, so a
// provider that returns a state without it strands that object for good.
//
// Two ways to lose it, one case each. An in-process provider returns a fresh
// state with no Deposed; and an observation, which carries only what a provider
// owns, becomes `current` for a provider that builds its answer out of `current`.
func TestApplyUpdateKeepsTheDeposedRecord(t *testing.T) {
	cases := []struct {
		name        string
		echoCurrent bool
		observed    func(address.Address) map[string]*resource.ResourceState
	}{
		{"provider returns a fresh state", false, func(address.Address) map[string]*resource.ResourceState { return nil }},
		{"provider returns a fresh state after a refresh", false, func(a address.Address) map[string]*resource.ResourceState {
			return map[string]*resource.ResourceState{a.String(): observedState(a)}
		}},
		{"provider echoes an observed current", true, func(a address.Address) map[string]*resource.ResourceState {
			return map[string]*resource.ResourceState{a.String(): observedState(a)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProvider{resourceType: "test.thing", echoCurrent: tc.echoCurrent}
			st, plan, reg, backend, a := observedFixture(t, prov)
			withDeposed(t, st, a)

			g, err := planner.BuildExecution(plan, noDeps)
			if err != nil {
				t.Fatalf("BuildExecution: %v", err)
			}
			result, ds := Apply(context.Background(), plan, g, st, Options{
				Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
				Observed: tc.observed(a),
			})
			if ds.HasErrors() {
				t.Fatalf("unexpected diagnostics: %+v", ds)
			}

			if updates := prov.updated(); len(updates) != 1 || len(updates[0].Deposed) != 1 {
				t.Error("Update was handed a current without the deposed entry state holds")
			}
			stored, ok := result.State.Get(a)
			if !ok {
				t.Fatal("db is not in state after a successful update")
			}
			if len(stored.Deposed) != 1 || stored.Deposed[0].ProviderID != "db-0" {
				t.Errorf("persisted Deposed = %v, want [db-0] — the old object is still running and nothing else names it", stored.Deposed)
			}
		})
	}
}
