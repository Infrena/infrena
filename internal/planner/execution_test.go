package planner

import (
	"strings"
	"testing"

	"infra/internal/graph"
	"infra/pkg/address"
)

// addr is NOT redefined here: plan_test.go (Task 12) already declares it in
// this package, and a second declaration will not compile.

// depsFrom builds the lookup BuildExecution needs from a plain map.
func depsFrom(m map[string][]string) func(address.Address) []address.Address {
	return func(a address.Address) []address.Address {
		var out []address.Address
		for _, name := range m[a.Name] {
			out = append(out, addr(name))
		}
		return out
	}
}

func planWith(ops ...Operation) *Plan {
	return &Plan{Version: PlanVersion, Operations: ops}
}

func orderOf(t *testing.T, g *graph.Graph[OpNode]) []string {
	t.Helper()
	layers, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	var out []string
	for _, layer := range layers {
		for _, n := range layer {
			out = append(out, n.ID())
		}
	}
	return out
}

func indexOf(t *testing.T, order []string, id string) int {
	t.Helper()
	for i, got := range order {
		if got == id {
			return i
		}
	}
	t.Fatalf("%q not found in %v", id, order)
	return -1
}

func TestCreatesRunAfterWhatTheyDependOn(t *testing.T) {
	p := planWith(
		Operation{Address: addr("network"), Type: "test.network", Kind: OpCreate},
		Operation{Address: addr("database"), Type: "test.database", Kind: OpCreate},
	)
	// network has one dependent: database.
	g, err := BuildExecution(p, depsFrom(map[string][]string{"network": {"database"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if indexOf(t, order, "create:network") > indexOf(t, order, "create:database") {
		t.Errorf("order = %v; a resource must be created after what it depends on", order)
	}
}

func TestDestroysRunInReverseDependencyOrder(t *testing.T) {
	// webapp depends on network, so the required order is destroy:webapp
	// before destroy:network. The names are deliberately chosen so
	// alphabetical order ("network" < "webapp") DISAGREES with that
	// required order: graph.Layers breaks ties alphabetically, so if the
	// destroy-side edge were never drawn at all, both nodes would land in
	// one untied layer and sort_strings would put destroy:network first —
	// which happens to be wrong. An earlier version of this test used
	// "network" and "database", where the required order (destroy:database
	// before destroy:network) coincided with alphabetical order, so the
	// assertion passed even with the destroy-side edge logic deleted
	// outright (Task 18 fix round 1 found this).
	p := planWith(
		Operation{Address: addr("network"), Type: "test.network", Kind: OpDestroy},
		Operation{Address: addr("webapp"), Type: "test.application", Kind: OpDestroy},
	)
	g, err := BuildExecution(p, depsFrom(map[string][]string{"network": {"webapp"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if indexOf(t, order, "destroy:webapp") > indexOf(t, order, "destroy:network") {
		t.Errorf("order = %v; a dependent must be destroyed BEFORE what it depends on — "+
			"destroying the network first would strand webapp", order)
	}
}

func TestReplaceBecomesTwoNodesDestroyThenCreate(t *testing.T) {
	p := planWith(
		Operation{Address: addr("database"), Type: "test.database", Kind: OpReplace},
	)
	g, err := BuildExecution(p, depsFrom(nil))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if len(order) != 2 {
		t.Fatalf("order = %v, want two nodes for a replacement", order)
	}
	if indexOf(t, order, "destroy:database") > indexOf(t, order, "create:database") {
		t.Errorf("order = %v; Phase 1 replacement is destroy-then-create", order)
	}
}

func TestReplaceOrdersDependentsAroundBothPhases(t *testing.T) {
	// Replacing a network with an application on top: the app must be
	// destroyed before the network's destroy, and created after its create.
	p := planWith(
		Operation{Address: addr("network"), Type: "test.network", Kind: OpReplace},
		Operation{Address: addr("app"), Type: "test.application", Kind: OpReplace},
	)
	g, err := BuildExecution(p, depsFrom(map[string][]string{"network": {"app"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if indexOf(t, order, "destroy:app") > indexOf(t, order, "destroy:network") {
		t.Errorf("order = %v; the dependent's destroy must precede its dependency's destroy", order)
	}
	if indexOf(t, order, "create:network") > indexOf(t, order, "create:app") {
		t.Errorf("order = %v; the dependency's create must precede its dependent's create", order)
	}
}

func TestNoOpsAreNotScheduled(t *testing.T) {
	p := planWith(
		Operation{Address: addr("network"), Type: "test.network", Kind: OpNoOp},
		Operation{Address: addr("database"), Type: "test.database", Kind: OpCreate},
	)
	g, err := BuildExecution(p, depsFrom(nil))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	order := orderOf(t, g)
	if len(order) != 1 || order[0] != "create:database" {
		t.Errorf("order = %v; a no-op is not work and must not be scheduled", order)
	}
}

func TestForgetIsScheduledWithoutAProviderCall(t *testing.T) {
	// Forget still removes the resource from state, so it is ordered like a
	// destroy even though no provider is called.
	p := planWith(
		Operation{Address: addr("database"), Type: "test.database", Kind: OpForget},
	)
	g, err := BuildExecution(p, depsFrom(nil))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	if order := orderOf(t, g); len(order) != 1 || order[0] != "forget:database" {
		t.Errorf("order = %v, want one forget node", order)
	}
}

func TestOrderingIsDeterministic(t *testing.T) {
	p := planWith(
		Operation{Address: addr("c"), Type: "test.network", Kind: OpCreate},
		Operation{Address: addr("a"), Type: "test.network", Kind: OpCreate},
		Operation{Address: addr("b"), Type: "test.network", Kind: OpCreate},
		Operation{Address: addr("d"), Type: "test.network", Kind: OpCreate},
	)
	deps := depsFrom(nil)

	g, err := BuildExecution(p, deps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	first := strings.Join(orderOf(t, g), ",")

	for i := 0; i < 20; i++ {
		g, err := BuildExecution(p, deps)
		if err != nil {
			t.Fatalf("BuildExecution: %v", err)
		}
		if got := strings.Join(orderOf(t, g), ","); got != first {
			t.Fatalf("ordering varies between runs: %q then %q", first, got)
		}
	}
}

// TestForgetSharingADestroyEdgeDoesNotPanic is a regression test for a real
// panic found during Task 18's fix round: an earlier version of addEdges
// reconstructed each side's node ID from a "destroy:"/"create:" string
// prefix rather than asking the node for its own ID. OpForget's
// destroy-phase node renders as "forget:", not "destroy:" (see OpNode.ID),
// so whenever a forgotten resource shared a destroy-side edge with another
// operation, that reconstruction named a node — "destroy:<the forgotten
// address>" — that was never Add-ed, and graph.Edge panicked. Keying `has`
// to the OpNode itself, so every edge asks the node for ID() instead of
// rebuilding one, is what fixes this; this test is what proves it stays
// fixed.
func TestForgetSharingADestroyEdgeDoesNotPanic(t *testing.T) {
	p := planWith(
		Operation{Address: addr("forgotten"), Type: "test.network", Kind: OpForget},
		Operation{Address: addr("dependent"), Type: "test.database", Kind: OpDestroy},
	)
	// dependent depends on forgotten, so BuildExecution draws a
	// destroy-side edge from dependent's destroy to forgotten's
	// destroy-phase (forget) node.
	g, err := BuildExecution(p, depsFrom(map[string][]string{"forgotten": {"dependent"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if len(order) != 2 {
		t.Fatalf("order = %v, want both the forget and the destroy scheduled", order)
	}
	if indexOf(t, order, "destroy:dependent") > indexOf(t, order, "forget:forgotten") {
		t.Errorf("order = %v; the dependent must be destroyed before the resource it depends on is forgotten", order)
	}
}

func TestCycleAmongOperationsIsAnError(t *testing.T) {
	p := planWith(
		Operation{Address: addr("a"), Type: "test.network", Kind: OpCreate},
		Operation{Address: addr("b"), Type: "test.network", Kind: OpCreate},
	)
	// a depends on b and b depends on a.
	g, err := BuildExecution(p, depsFrom(map[string][]string{"a": {"b"}, "b": {"a"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	if cycle := g.Cycle(); len(cycle) == 0 {
		t.Error("a cycle among operations must be detectable before execution")
	}
}
