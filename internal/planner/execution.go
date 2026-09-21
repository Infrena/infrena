package planner

import (
	"fmt"

	"github.com/infrena/infrena/internal/graph"
	"github.com/infrena/infrena/pkg/address"
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
	// Address identifies the resource the work concerns.
	Address address.Address
	// Kind is the operation the plan proposed.
	Kind OpKind
	// Phase distinguishes a replacement's two halves.
	Phase Phase
	// CreateBeforeDestroy is carried on the node rather than looked up from
	// the plan, because four decisions downstream turn on it and each has
	// only the node in hand: how the two phases are ordered, whether the
	// destroy phase deletes the current object or the deposed one, whether
	// finishing that phase means the address is gone, and what the run
	// reports. Four lookups would be four chances to disagree.
	//
	// Meaningful only for a replace; every other kind has one phase.
	CreateBeforeDestroy bool
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
	case OpDestroyDeposed:
		// Its own id, so it can coexist with an operation at the same
		// address: the resource may be updated in the same run that clears
		// up after an earlier replacement of it.
		return "deposed:" + n.Address.String()
	default:
		return "noop:" + n.Address.String()
	}
}

// BuildExecution turns a plan into an execution graph.
//
// Ordering differs by operation kind, and conflating the two directions
// causes apply-time failures:
//
//   - a create runs after the creates of what it depends on
//   - a destroy runs before the destroys of what it depends on — reverse
//     order, because destroying a dependency first strands its dependents
//   - a replacement is a destroy then a create, with dependents ordered
//     around both halves
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

	// A well-formed plan names each address at most once: a replace's two
	// phases share one Operation, and the split into two nodes happens
	// below. Plans this package builds cannot violate that, but a plan built
	// by hand or read back from disk can. The executor depends on "at most
	// one node per address, except a replace's two phases" when it removes a
	// same-address applied entry on failure, so this check is what makes
	// that an enforced invariant rather than an assumption about callers.
	seen := make(map[string]bool, len(p.Operations))
	for _, op := range p.Operations {
		key := op.Address.String()
		if op.Kind == OpDestroyDeposed {
			// A deposed cleanup legitimately shares an address with another
			// operation, since the resource itself may be unchanged, updated
			// or replaced in the same run. Keyed apart rather than exempted,
			// so two deposed cleanups at one address are still caught.
			key += " (deposed)"
		}
		if seen[key] {
			return nil, fmt.Errorf("planner: BuildExecution: %s: a plan may contain at most one operation per address (a replace's destroy and create phases come from a single Operation, not two) — this plan has more than one", op.Address)
		}
		seen[key] = true
	}

	// Which phases exist for each address, holding the node itself rather
	// than a bool: an edge must name the ID actually added, and a forget's
	// destroy-phase node reads "forget:", not "destroy:". Rebuilding that ID
	// from the phase and address would name a node that does not exist, and
	// Edge panics on those. Storing the node lets addEdges ask it.
	has := map[string]map[Phase]OpNode{}

	add := func(n OpNode) {
		g.Add(n)
		// A deposed cleanup is not recorded in has: it shares an address and
		// a phase with the real destroy, so it would overwrite the
		// replacement's own destroy node and silently redirect every edge
		// meant for it. It takes part in no edges by design, so it belongs
		// in the graph but not in this index.
		if n.Kind == OpDestroyDeposed {
			return
		}
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
			cbd := op.Lifecycle.CreateBeforeDestroy
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseDestroy, CreateBeforeDestroy: cbd})
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseCreate, CreateBeforeDestroy: cbd})
		case OpDestroy, OpForget:
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseDestroy})
		case OpDestroyDeposed:
			// Deliberately unordered against everything else. The object it
			// removes is referred to by nothing: it left configuration when
			// its replacement was created, and no dependency edge names it.
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseDestroy})
		case OpCreate, OpUpdate:
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseCreate})
			// No default arm: a future kind landing here unhandled becomes no
			// node and is never scheduled, which is safer than being absorbed
			// as a create that could be actively wrong for it.
		}
	}

	for _, op := range p.Operations {
		if op.Kind == OpNoOp {
			continue
		}
		if op.Kind == OpReplace {
			// The two phases of one replacement are ordered against each
			// other even when nothing depends on it. Every other edge here
			// relates two different addresses, so without this a
			// dependent-free replacement could be scheduled either way
			// round.
			//
			// Both lookups are checked explicitly so that a disagreement
			// between the node-adding pass and this one panics with a named
			// cause, rather than Edge panicking on a zero-value node whose
			// ID reads as an unrelated mystery.
			d, dok := has[op.Address.String()][PhaseDestroy]
			c, cok := has[op.Address.String()][PhaseCreate]
			if !dok || !cok {
				panic("planner: BuildExecution: replace at " + op.Address.String() +
					" is missing a phase node — the node-adding and edge-adding passes disagree")
			}
			// The one edge create_before_destroy reverses. Everything else
			// about the replacement is unchanged; what differs is which half
			// may run first, and so whether there is a moment when the
			// resource does not exist.
			if op.Lifecycle.CreateBeforeDestroy {
				g.Edge(c.ID(), d.ID())
			} else {
				g.Edge(d.ID(), c.ID())
			}
		}
		for _, dependent := range deps(op.Address) {
			addEdges(g, has, op.Address, dependent, op.Lifecycle.CreateBeforeDestroy)
		}
	}

	return g, nil
}

// addEdges wires one dependency relationship — dependent depends on target —
// in both directions of work that exist.
//
// Each side is looked up as the node itself rather than reassembled from a
// "destroy:"/"create:" prefix, because a forget's destroy-phase node reads
// "forget:" and a prefix-built ID would name a node that was never added.
func addEdges(
	g *graph.Graph[OpNode], has map[string]map[Phase]OpNode,
	target, dependent address.Address, targetCreatesFirst bool,
) {
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

	if targetCreatesFirst {
		// This is the edge that makes create_before_destroy mean something
		// rather than merely reorder two calls. The dependent's own work —
		// an update pointing it at the new object, or its own creation —
		// must finish before the old object is destroyed. Without it the old
		// object is torn down the instant the new one exists, while
		// everything still refers to the old: the outage the flag was set to
		// avoid, one step later.
		if dc, ok := has[d][PhaseCreate]; ok {
			if td, ok := has[t][PhaseDestroy]; ok {
				g.Edge(dc.ID(), td.ID())
			}
		}
		// The "tear the dependent down before rebuilding the target" edge is
		// deliberately omitted: it is the opposite of what this flag asks
		// for, and combined with the edge above it is a cycle.
		return
	}

	// A dependent being torn down must go before the target is rebuilt.
	if dd, ok := has[d][PhaseDestroy]; ok {
		if tc, ok := has[t][PhaseCreate]; ok {
			g.Edge(dd.ID(), tc.ID())
		}
	}
}
