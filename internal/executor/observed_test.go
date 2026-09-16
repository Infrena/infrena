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

// recordingProvider is the honest reproduction this bug needs. The fake
// provider in providers/test cannot catch it: its Update applies `desired`
// wholesale and never looks at `current` at all, so it behaves identically
// whether it is handed the last-persisted state or the refreshed
// observation. A provider that DIFFS the two — which is what the AWS
// plugin's Cloud Control path does, and what the provider contract invites
// ("current is given to you so you can USE it ... the attributes to diff
// against") — is the only kind that can tell the difference. This one does
// not diff; it simply records what it was handed, which is the same
// evidence without the indirection.
type recordingProvider struct {
	resourceType string
	// echoCurrent makes Update return the very *resource.ResourceState it
	// was handed, with only the desired attributes written onto it. That is
	// not a contrived shape: internal/pluginhost's rebuild carries the
	// bookkeeping fields off `current` onto what a plugin returned, so for
	// every out-of-process provider the state that gets PERSISTED is built
	// from `current` in exactly this way.
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
// whose LAST-PERSISTED attributes say size 10, whose refreshed OBSERVATION
// says size 99 (somebody resized it outside infrena), and whose
// configuration still asks for 10. The plan is therefore a single update
// back to 10, and the question each test asks is what `current` the
// provider sees while making it.
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

// observedState is what refresh saw: size 99, and — because refresh hands
// the provider the state it already holds and internal/pluginhost carries
// the bookkeeping fields back off it — the same Address, Type and
// ProviderID. It deliberately carries NEITHER Lifecycle, Dependencies nor
// CreatedAt, because an in-process provider is under no obligation to
// return them and the host is the thing that owns them.
func observedState(a address.Address) *resource.ResourceState {
	return &resource.ResourceState{
		Address:    a,
		Type:       "test.thing",
		ProviderID: "db-1",
		Attributes: map[string]value.Value{"size": value.Int(99, value.SourceProvider)},
	}
}

// TestApplyUpdateReceivesTheObservationNotTheLastPersistedState is the
// reproduction. Before the fix the executor built `current` from the state
// it loaded off disk, so a provider that diffs current against desired saw
// size 10 == 10, concluded nothing had changed, and skipped the very drift
// the plan had just correctly proposed to correct.
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

// TestApplyUpdateKeepsHostBookkeepingOnTheObservedCurrent pins the half of
// the fix that is NOT a straight swap. An observation carries only what a
// provider owns — the provider ID and the attributes; every other field on
// a ResourceState is the host's, and pkg/provider.Provider.Read says so
// explicitly. If `current` were simply replaced by the observation, those
// fields would arrive empty at the provider, and (for every out-of-process
// provider, whose persisted state is rebuilt from `current`) empty is what
// would then be written to disk: a vanished prevent_destroy guard and lost
// destroy-ordering edges, which is the worst failure this product has.
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

// TestApplyUpdateWithObservedCurrentStillPersistsHostBookkeeping is the
// third property the design note names: whatever the executor PERSISTS
// must stay correct once observed values start flowing into `current`.
// echoCurrent models internal/pluginhost.rebuild, which builds the state
// the executor records out of `current`'s bookkeeping fields — so an
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

// TestApplyUpdateFallsBackToStateWhenNothingWasObserved is the guard on
// the design note's second property: a nil observation must NEVER reach
// Update. There are three ways an address can have no usable observation —
// no entry at all (the saved-plan path does not refresh), an entry whose
// read failed, and an entry reporting the resource is gone — and dispatch
// refuses a nil `current` outright, so a merge that returned the
// observation unconditionally would turn every one of them into a failed
// operation.
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

// TestApplyDestroyReceivesTheObservation covers the other verb dispatch
// hands `current` to. A delete that needs more than the ID to find its
// object — a resource identified by a field the last apply did not set, say
// — has the same stale-input problem an update does.
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
