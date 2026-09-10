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
	p := planWith(
		Operation{Address: addr("network"), Type: "test.network", Kind: OpDestroy},
		Operation{Address: addr("database"), Type: "test.database", Kind: OpDestroy},
	)
	g, err := BuildExecution(p, depsFrom(map[string][]string{"network": {"database"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if indexOf(t, order, "destroy:database") > indexOf(t, order, "destroy:network") {
		t.Errorf("order = %v; a dependent must be destroyed BEFORE what it depends on — "+
			"destroying the network first would strand the database", order)
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
