package planner

import (
	"infra/internal/graph"
	"infra/pkg/address"
)

// Phase distinguishes the two halves of a replacement, which is the one
// operation that becomes two nodes.
type Phase uint8

const (
	// PhaseDestroy removes an object.
	PhaseDestroy Phase = iota
	// PhaseCreate builds one.
	PhaseCreate
)

// OpNode is one unit of executable work.
type OpNode struct {
	Address address.Address
	Kind    OpKind
	Phase   Phase
}

// ID identifies the node uniquely, and readably enough to name in a test
// failure or a diagnostic.
func (n OpNode) ID() string {
	switch n.Kind {
	case OpReplace:
		if n.Phase == PhaseDestroy {
			return "destroy:" + n.Address.String()
		}
		return "create:" + n.Address.String()
	case OpCreate:
		return "create:" + n.Address.String()
	case OpUpdate:
		return "update:" + n.Address.String()
	case OpDestroy:
		return "destroy:" + n.Address.String()
	case OpForget:
		return "forget:" + n.Address.String()
	default:
		return "noop:" + n.Address.String()
	}
}

// BuildExecution turns a plan into an execution graph.
//
// Ordering differs by operation kind, and conflating the two directions causes
// apply-time failures (spec §14):
//
//   - a create runs after the creates of what it depends on
//   - a destroy runs BEFORE the destroys of what it depends on — reverse order,
//     because destroying a dependency first would strand its dependents
//   - a replacement is a destroy then a create, with dependents ordered around
//     both halves
//
// deps reports the resources that depend on a given address. The caller
// supplies it because the two sides draw from different places: create-side
// edges come from configuration, destroy-side edges from the Dependencies
// recorded in state, since a resource being destroyed may no longer appear in
// configuration at all. Operation.Dependents already holds the resolved
// answer.
func BuildExecution(p *Plan, deps func(address.Address) []address.Address) (*graph.Graph[OpNode], error) {
	g := graph.New[OpNode]()
	if p == nil {
		return g, nil
	}

	// Which phases exist for each address, keyed to the node itself rather
	// than a bare bool: an edge must name the ID actually Add-ed, and for
	// OpForget the destroy-phase node's ID is "forget:", not "destroy:".
	// Reconstructing that ID from Phase and address alone (as a bool map
	// would force) mismatches, and Edge panics on the nonexistent
	// "destroy:" id — turning a valid plan (a forgotten resource with a
	// dependent being destroyed) into a crash. Storing the node lets
	// addEdges always ask it for its own ID.
	has := map[string]map[Phase]OpNode{}

	add := func(n OpNode) {
		g.Add(n)
		key := n.Address.String()
		if has[key] == nil {
			has[key] = map[Phase]OpNode{}
		}
		has[key][n.Phase] = n
	}

	for _, op := range p.Operations {
		switch op.Kind {
		case OpNoOp:
			// Not work. Scheduling it would make every plan look busy and
			// would put unchanged resources in the executor's path.
			continue
		case OpReplace:
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseDestroy})
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseCreate})
		case OpDestroy, OpForget:
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseDestroy})
		default: // OpCreate, OpUpdate
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseCreate})
		}
	}

	for _, op := range p.Operations {
		if op.Kind == OpNoOp {
			continue
		}
		if op.Kind == OpReplace {
			// The two phases of one replacement are ordered relative to each
			// other even when nothing else depends on it: a replacement with
			// no dependents still has to destroy before it creates. Every
			// other edge in this function relates two DIFFERENT addresses,
			// so without this, a dependent-free replacement's own pair is
			// left unconstrained and can be scheduled in either order.
			d, c := has[op.Address.String()][PhaseDestroy], has[op.Address.String()][PhaseCreate]
			g.Edge(d.ID(), c.ID())
		}
		for _, dependent := range deps(op.Address) {
			addEdges(g, has, op.Address, dependent)
		}
	}

	return g, nil
}

// addEdges wires one dependency relationship — dependent depends on target —
// in both directions of work that exist.
//
// Each side is looked up as the OpNode itself, not reassembled from a
// "destroy:"/"create:" prefix: OpForget's destroy-phase node renders as
// "forget:", so a prefix-based ID would name a node that was never Add-ed
// and Edge would panic. Asking the node for its own ID keeps this correct
// for every kind, including forget.
func addEdges(g *graph.Graph[OpNode], has map[string]map[Phase]OpNode, target, dependent address.Address) {
	t, d := target.String(), dependent.String()

	// Build side: the target is created before its dependent is.
	if tc, ok := has[t][PhaseCreate]; ok {
		if dc, ok := has[d][PhaseCreate]; ok {
			g.Edge(tc.ID(), dc.ID())
		}
	}
	// Teardown side: the dependent is destroyed before the target is.
	if dd, ok := has[d][PhaseDestroy]; ok {
		if td, ok := has[t][PhaseDestroy]; ok {
			g.Edge(dd.ID(), td.ID())
		}
	}
	// A dependent being torn down must go before the target is rebuilt.
	if dd, ok := has[d][PhaseDestroy]; ok {
		if tc, ok := has[t][PhaseCreate]; ok {
			g.Edge(dd.ID(), tc.ID())
		}
	}
}
