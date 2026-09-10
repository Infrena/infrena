package compiler

import (
	"context"
	"strings"
	"testing"

	"infra/internal/diag"
	"infra/internal/registry"
	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
	"infra/pkg/value"
)

func TestValidateGraphDetectsCycle(t *testing.T) {
	alpha := res("alpha", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	bravo := res("bravo", "test.network", map[string]value.Value{"cidr": value.String("10.0.1.0/16", value.SourceExplicit)})
	charlie := res("charlie", "test.network", map[string]value.Value{"cidr": value.String("10.0.2.0/16", value.SourceExplicit)})
	alpha.DependsOn = []address.Address{{Name: "bravo"}}
	bravo.DependsOn = []address.Address{{Name: "charlie"}}
	charlie.DependsOn = []address.Address{{Name: "alpha"}}

	graph := cfg(alpha, bravo, charlie)
	ds := validateGraph(&graph, testRegistry(t))
	if !ds.HasErrors() {
		t.Fatal("a dependency cycle must be an error")
	}

	var out strings.Builder
	ds.Render(&out)
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if !strings.Contains(out.String(), name) {
			t.Errorf("cycle diagnostic must name every member of the cycle, missing %q:\n%s", name, out.String())
		}
	}
}

func TestValidateGraphAcceptsAcyclicGraph(t *testing.T) {
	a := res("a", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	b := res("b", "test.network", map[string]value.Value{"cidr": value.String("10.0.1.0/16", value.SourceExplicit)})
	c := res("c", "test.network", map[string]value.Value{"cidr": value.String("10.0.2.0/16", value.SourceExplicit)})
	d := res("d", "test.network", map[string]value.Value{"cidr": value.String("10.0.3.0/16", value.SourceExplicit)})
	b.DependsOn = []address.Address{{Name: "a"}}
	c.DependsOn = []address.Address{{Name: "a"}}
	d.DependsOn = []address.Address{{Name: "b"}, {Name: "c"}}

	graph := cfg(a, b, c, d)
	if ds := validateGraph(&graph, testRegistry(t)); ds.HasErrors() {
		t.Errorf("a diamond dependency shape is not a cycle: %+v", ds)
	}
}

func TestValidateGraphReportsMissingRequiredInfrastructure(t *testing.T) {
	graph := oneResource("test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	})
	ds := validateGraph(graph, testRegistry(t))
	if !ds.HasErrors() {
		t.Fatal("a database with no network anywhere in configuration must be an error before any provider call")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "network") {
		t.Errorf("diagnostic must name the missing requirement:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "test.network") {
		t.Errorf("diagnostic must name what would satisfy it:\n%s", out.String())
	}
}

func TestValidateGraphAcceptsSatisfiedRequirement(t *testing.T) {
	network := res("network", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	database := res("database", "test.database", map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)})

	graph := cfg(network, database)
	if ds := validateGraph(&graph, testRegistry(t)); ds.HasErrors() {
		t.Errorf("a network exists in configuration, so the requirement is satisfied: %+v", ds)
	}
}

// stubProvider satisfies provider.Provider with exactly what this file needs:
// one resource type whose requirement is Optional, to prove stage 8 only
// errors on requirements that are not.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type: "stub.thing",
		Attributes: map[string]schema.Attribute{
			"name": {Kind: value.KindString, Required: true},
		},
		Requirements: []schema.Requirement{{
			Name:        "cache",
			Types:       []string{"stub.cache"},
			Optional:    true,
			Description: "A cache speeds up stub.thing but is not required.",
		}},
		Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true},
	}}
}
func (stubProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }
func (stubProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (stubProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (stubProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (stubProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (stubProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (stubProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func TestValidateGraphSkipsOptionalRequirement(t *testing.T) {
	reg := registry.New()
	if err := reg.Register(stubProvider{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	thing := res("thing", "stub.thing", map[string]value.Value{"name": value.String("widget", value.SourceExplicit)})
	graph := cfg(thing)

	if ds := validateGraph(&graph, reg); ds.HasErrors() {
		t.Errorf("an unsatisfied optional requirement must not be an error: %+v", ds)
	}
}

func TestValidateGraphRejectsPreventDestroyAndRetainTogether(t *testing.T) {
	guarded := res("guarded", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	guarded.Lifecycle = resource.Lifecycle{PreventDestroy: true, Retain: true}

	graph := cfg(guarded)
	ds := validateGraph(&graph, testRegistry(t))
	if !ds.HasErrors() {
		t.Fatal("prevent_destroy and retain together are contradictory and must be an error")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "prevent_destroy") || !strings.Contains(out.String(), "retain") {
		t.Errorf("diagnostic must name both contradictory settings:\n%s", out.String())
	}
}

func TestValidateGraphAcceptsLifecycleFlagsIndividually(t *testing.T) {
	cases := []struct {
		name      string
		lifecycle resource.Lifecycle
	}{
		{"neither set", resource.Lifecycle{}},
		{"prevent_destroy alone", resource.Lifecycle{PreventDestroy: true}},
		{"retain alone", resource.Lifecycle{Retain: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := res("net", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
			r.Lifecycle = tc.lifecycle

			graph := cfg(r)
			if ds := validateGraph(&graph, testRegistry(t)); ds.HasErrors() {
				t.Errorf("%s must not be a lifecycle error: %+v", tc.name, ds)
			}
		})
	}
}

func TestValidateGraphReportsEveryProblemAtOnce(t *testing.T) {
	// A 2-cycle of resources that each also lack their own required network,
	// alongside an unrelated resource with a lifecycle contradiction: one
	// category of problem must not mask the others.
	//
	// guarded is deliberately test.application, not test.network: its own
	// requirement (database) is satisfied by x and y, so it contributes no
	// requirement diagnostic of its own — but it also must not accidentally
	// satisfy x and y's network requirement, which a test.network resource
	// here would do regardless of any edge connecting it to them.
	x := res("x", "test.database", nil)
	y := res("y", "test.database", nil)
	x.DependsOn = []address.Address{{Name: "y"}}
	y.DependsOn = []address.Address{{Name: "x"}}

	guarded := res("guarded", "test.application", nil)
	guarded.Lifecycle = resource.Lifecycle{PreventDestroy: true, Retain: true}

	graph := cfg(x, y, guarded)
	ds := validateGraph(&graph, testRegistry(t))
	if len(ds) < 4 {
		t.Errorf("got %d diagnostics, want at least 4 (one cycle, two missing requirements, one lifecycle contradiction): %+v", len(ds), ds)
	}
}

// TestValidateGraphStillReportsACycleAfterAPartialFix uses a topology with two
// distinct simple cycles that share no edge: a→b→d→a and a→c→d→e→a (a review
// harness against the earlier all-cycles-enumerating detector found that it
// reported only the first pair, through b, and silently never named c at all —
// because d was already marked fully-explored by the time c's branch reached
// it, so c's own back edge to a was never checked). validateGraph now promises
// only ONE cycle, never "acyclic", so the only property that must hold is: a
// live cycle is still reported after removing the edge the first reported
// cycle depended on, exactly the situation a user hits when they "fix" what
// they were told about and re-run.
//
// Honest label: this passes against the pre-narrowing code too. DFS is sound
// for existence — a back edge is found whenever a cycle exists — so no input
// makes the old detector claim "acyclic" wrongly, and no test can be RED on
// that claim. This is a characterisation test of the single-cycle contract,
// not a regression test of a fixed false-negative.
func TestValidateGraphStillReportsACycleAfterAPartialFix(t *testing.T) {
	build := func() ResolvedConfig {
		a := res("a", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
		b := res("b", "test.network", map[string]value.Value{"cidr": value.String("10.0.1.0/16", value.SourceExplicit)})
		c := res("c", "test.network", map[string]value.Value{"cidr": value.String("10.0.2.0/16", value.SourceExplicit)})
		d := res("d", "test.network", map[string]value.Value{"cidr": value.String("10.0.3.0/16", value.SourceExplicit)})
		e := res("e", "test.network", map[string]value.Value{"cidr": value.String("10.0.4.0/16", value.SourceExplicit)})
		a.DependsOn = []address.Address{{Name: "b"}, {Name: "c"}}
		b.DependsOn = []address.Address{{Name: "d"}}
		c.DependsOn = []address.Address{{Name: "d"}}
		d.DependsOn = []address.Address{{Name: "a"}, {Name: "e"}}
		e.DependsOn = []address.Address{{Name: "a"}}
		return cfg(a, b, c, d, e)
	}

	graph := build()
	if ds := validateGraph(&graph, testRegistry(t)); !ds.HasErrors() {
		t.Fatal("this graph has a cycle; validateGraph must report it")
	}

	// Remove a -> b, the edge the first-found cycle (a→b→d→a) depends on.
	// a→c→d→e→a is untouched and still live.
	graph2 := build()
	graph2.Resources["a"].DependsOn = []address.Address{{Name: "c"}}
	if ds := validateGraph(&graph2, testRegistry(t)); !ds.HasErrors() {
		t.Fatal("a→c→d→e→a is still a live cycle; validateGraph must not report the graph as acyclic")
	}
}

// TestCycleDiagnosticShapeIsStable pins the two properties that survive the
// move to internal/graph but that the membership assertion above cannot see:
// the sequence reads in depends-on order and closes back on its first member,
// and Related therefore names every member except the one carrying Origin.
func TestCycleDiagnosticShapeIsStable(t *testing.T) {
	a := res("a", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	b := res("b", "test.network", map[string]value.Value{"cidr": value.String("10.0.1.0/16", value.SourceExplicit)})
	c := res("c", "test.network", map[string]value.Value{"cidr": value.String("10.0.2.0/16", value.SourceExplicit)})
	a.DependsOn = []address.Address{{Name: "b"}}
	b.DependsOn = []address.Address{{Name: "c"}}
	c.DependsOn = []address.Address{{Name: "a"}}
	graph := cfg(a, b, c)

	ds := validateGraph(&graph, testRegistry(t))
	var d diag.Diagnostic
	for _, cand := range ds {
		if strings.HasPrefix(cand.Summary, "dependency cycle: ") {
			d = cand
		}
	}
	if d.Summary == "" {
		t.Fatalf("no cycle diagnostic: %+v", ds)
	}

	// a depends on b depends on c depends on a: the sequence reads in that
	// order and closes on a. Reversed, it would read "a → c → b → a" and be
	// a false statement about the configuration.
	if want := "dependency cycle: a → b → c → a"; d.Summary != want {
		t.Errorf("summary = %q, want %q", d.Summary, want)
	}

	// Related is every member but the first, which carries Origin. Without
	// the closing repeat this slice silently loses c.
	if len(d.Related) != 2 {
		t.Fatalf("Related = %v, want 2 entries (b and c)", d.Related)
	}
	for i, want := range []string{"b", "c"} {
		if got := d.Related[i].String(); got != want {
			t.Errorf("Related[%d] = %q, want %q", i, got, want)
		}
	}
}

// TestCycleForSkipsDanglingReferenceWithoutPanicking is a regression test
// for the skip in cycleFor: "if _, ok := cfg.Get(dep); !ok { continue }".
// Ordinarily stage 6 (bind) already rejects a reference to a resource
// absent from configuration before validateGraph ever runs, but cycleFor
// builds its own graph.Graph directly from DependsOn and calls g.Edge for
// every dependency — including one that names a resource that was never
// g.Add-ed to this graph, which graph.Edge panics on (see
// internal/graph/graph.go's Edge). The skip is what turns that would-be
// crash into "not a node in this graph, nothing to walk into", exactly as
// firstCycle's equivalent skip did before this task's swap. This
// configuration combines a dangling reference with a genuine, unrelated
// cycle so the test also confirms the dangling reference does not swallow
// the real diagnostic.
func TestCycleForSkipsDanglingReferenceWithoutPanicking(t *testing.T) {
	a := res("a", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)})
	b := res("b", "test.network", map[string]value.Value{"cidr": value.String("10.0.1.0/16", value.SourceExplicit)})
	c := res("c", "test.network", map[string]value.Value{"cidr": value.String("10.0.2.0/16", value.SourceExplicit)})
	a.DependsOn = []address.Address{{Name: "b"}}
	b.DependsOn = []address.Address{{Name: "a"}}
	// c depends on a resource absent from this configuration entirely.
	c.DependsOn = []address.Address{{Name: "ghost"}}
	graph := cfg(a, b, c)

	ds := validateGraph(&graph, testRegistry(t))
	if !ds.HasErrors() {
		t.Fatal("the a<->b cycle must still be reported even though c references a resource absent from configuration")
	}
	found := false
	for _, d := range ds {
		if strings.HasPrefix(d.Summary, "dependency cycle: ") {
			found = true
		}
	}
	if !found {
		t.Errorf("no cycle diagnostic among: %+v", ds)
	}
}
