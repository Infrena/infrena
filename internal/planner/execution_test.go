package planner

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/graph"
	"github.com/infrena/infrena/pkg/address"
)

// addr is NOT redefined here: plan_test.go already declares it in this package.

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
		Operation{Address: addr("network"), Type: "fake.network", Kind: OpCreate},
		Operation{Address: addr("database"), Type: "fake.database", Kind: OpCreate},
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
	// webapp depends on network, so the required order is destroy:webapp before
	// destroy:network. The names are deliberately chosen so alphabetical order
	// ("network" < "webapp") DISAGREES with that: graph.Layers breaks ties
	// alphabetically, so with no destroy-side edge both nodes land in one untied
	// layer and destroy:network comes first. Names whose alphabetical order
	// happens to match the required order would pass with the edge logic deleted.
	p := planWith(
		Operation{Address: addr("network"), Type: "fake.network", Kind: OpDestroy},
		Operation{Address: addr("webapp"), Type: "fake.application", Kind: OpDestroy},
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
		Operation{Address: addr("database"), Type: "fake.database", Kind: OpReplace},
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
	//
	// The dependent is "webapp", not "app", and the choice is load-bearing.
	// graph.Layers breaks ties among ready nodes alphabetically, so a name like
	// "app" would AGREE with the required destroy order and the assertion would
	// pass with the teardown edge block deleted. "webapp" > "network", so
	// alphabetical order DISAGREES and only a real edge can produce it.
	//
	// The create assertion below is sound for the same reason in reverse:
	// "create:webapp" > "create:network", the opposite of what a missing
	// build-side edge would produce.
	p := planWith(
		Operation{Address: addr("network"), Type: "fake.network", Kind: OpReplace},
		Operation{Address: addr("webapp"), Type: "fake.application", Kind: OpReplace},
	)
	g, err := BuildExecution(p, depsFrom(map[string][]string{"network": {"webapp"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	order := orderOf(t, g)
	if indexOf(t, order, "destroy:webapp") > indexOf(t, order, "destroy:network") {
		t.Errorf("order = %v; the dependent's destroy must precede its dependency's destroy", order)
	}
	if indexOf(t, order, "create:network") > indexOf(t, order, "create:webapp") {
		t.Errorf("order = %v; the dependency's create must precede its dependent's create", order)
	}
}

func TestNoOpsAreNotScheduled(t *testing.T) {
	p := planWith(
		Operation{Address: addr("network"), Type: "fake.network", Kind: OpNoOp},
		Operation{Address: addr("database"), Type: "fake.database", Kind: OpCreate},
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
		Operation{Address: addr("database"), Type: "fake.database", Kind: OpForget},
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
		Operation{Address: addr("c"), Type: "fake.network", Kind: OpCreate},
		Operation{Address: addr("a"), Type: "fake.network", Kind: OpCreate},
		Operation{Address: addr("b"), Type: "fake.network", Kind: OpCreate},
		Operation{Address: addr("d"), Type: "fake.network", Kind: OpCreate},
	)
	deps := depsFrom(nil)

	g, err := BuildExecution(p, deps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	first := strings.Join(orderOf(t, g), ",")

	for range 20 {
		g, err := BuildExecution(p, deps)
		if err != nil {
			t.Fatalf("BuildExecution: %v", err)
		}
		if got := strings.Join(orderOf(t, g), ","); got != first {
			t.Fatalf("ordering varies between runs: %q then %q", first, got)
		}
	}
}

// TestForgetSharingADestroyEdgeDoesNotPanic. addEdges must ask each node for
// its own ID rather than reconstructing one from a "destroy:"/"create:" prefix.
// OpForget's destroy-phase node renders as "forget:", not "destroy:" (see
// OpNode.ID), so whenever a forgotten resource shares a destroy-side edge with
// another operation a reconstructed ID names a node that was never Add-ed, and
// graph.Edge panics.
func TestForgetSharingADestroyEdgeDoesNotPanic(t *testing.T) {
	p := planWith(
		Operation{Address: addr("forgotten"), Type: "fake.network", Kind: OpForget},
		Operation{Address: addr("dependent"), Type: "fake.database", Kind: OpDestroy},
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
		Operation{Address: addr("a"), Type: "fake.network", Kind: OpCreate},
		Operation{Address: addr("b"), Type: "fake.network", Kind: OpCreate},
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

// TestBuildExecutionRejectsADuplicateAddress pins the rule
// executor.tracker.recordFailure depends on to safely collapse a replace's two
// phases into one Applied entry: a plan may name each address at most once (a
// replace's destroy and create phases come from ONE Operation, not two). A
// normally-built plan can never violate it — planAddresses already dedups by
// address — but planWith bypasses that, the same way a hand-edited or stale
// saved plan read back from disk would. Two different-kind operations sharing an
// address, as built here, is exactly the shape that would otherwise let
// recordFailure's delete wipe a legitimately-applied, unrelated entry.
func TestBuildExecutionRejectsADuplicateAddress(t *testing.T) {
	p := planWith(
		Operation{Address: addr("dup"), Type: "fake.network", Kind: OpCreate},
		Operation{Address: addr("dup"), Type: "fake.network", Kind: OpDestroy},
	)
	noDeps := func(address.Address) []address.Address { return nil }

	_, err := BuildExecution(p, noDeps)
	if err == nil {
		t.Fatal("BuildExecution: want an error for a plan with two operations at the same address, got nil")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("error %q does not name the offending address", err.Error())
	}
}
