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
	Address address.Address
	Kind    OpKind
	Phase   Phase
	// CreateBeforeDestroy is carried on the NODE, not looked up from the plan,
	// because four separate decisions downstream turn on it and every one of
	// them has only the node in hand: which way the two phases are ordered,
	// whether the destroy phase deletes the current object or the deposed one,
	// whether completing the destroy phase means the address is GONE, and what
	// the run reports. A lookup in each place is four chances to disagree.
	//
	// Meaningful only for OpReplace. Every other kind has one phase and nothing
	// to reorder.
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
		// Its OWN id, so it can coexist with an operation at the same address:
		// the resource may be being updated in the same run that clears up
		// after an earlier replacement of it.
		return "deposed:" + n.Address.String()
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

	// A well-formed plan names each address at most once: planAddresses
	// (planner.go) already dedups by address when a plan is built normally,
	// so a replace's destroy and create phases share ONE Operation here,
	// not two — the two-node split happens below, inside this function, not
	// in the plan itself. That makes this codebase's own callers incapable
	// of producing a duplicate today, which is exactly why it was easy to
	// miss: a *Plan built by hand (as a test, or a future M6 caller reading
	// one back from disk — a *Plan is no longer something this codebase
	// constructed at that point, it is untrusted file content) can still
	// violate it. executor.tracker.recordFailure (internal/executor/
	// isolation.go) depends on "at most one node per address, except a
	// replace's two phases" to safely delete a same-address Applied entry
	// on failure — this check is what makes that dependency an enforced
	// invariant instead of an assumption resting on a caller this package
	// does not control.
	seen := make(map[string]bool, len(p.Operations))
	for _, op := range p.Operations {
		key := op.Address.String()
		if op.Kind == OpDestroyDeposed {
			// A deposed cleanup is a SECOND operation at an address that
			// legitimately has another: the resource itself may be unchanged,
			// updated or replaced in the same run. It is keyed apart rather
			// than exempted, so two deposed cleanups at one address are still
			// caught — that would mean the planner emitted one twice.
			key += " (deposed)"
		}
		if seen[key] {
			return nil, fmt.Errorf("planner: BuildExecution: %s: a plan may contain at most one operation per address (a replace's destroy and create phases come from a single Operation, not two) — this plan has more than one", op.Address)
		}
		seen[key] = true
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
		// A deposed cleanup is NOT recorded in `has`. That map answers "which
		// phase node exists for this address" for the edge passes, and a
		// deposed node shares an address and a phase with the real destroy —
		// so recording it would overwrite the replacement's own destroy node,
		// silently redirecting every edge meant for it and tripping the
		// phases-disagree panic below. It takes part in no edges by design
		// (see the node-adding switch), so it belongs in the graph and not in
		// this index.
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
			// UNORDERED with respect to everything else, deliberately. The
			// object it removes is not referred to by anything: it left
			// configuration when its replacement was created, and no dependency
			// edge names it. Ordering it against the live resource at the same
			// address would be inventing a constraint to look tidy.
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseDestroy})
		case OpCreate, OpUpdate:
			add(OpNode{Address: op.Address, Kind: op.Kind, Phase: PhaseCreate})
			// No default: OpKind is a closed enum (see plan.go). A future kind
			// landing here unhandled should be visible — nothing added, so it
			// never becomes a node and never gets scheduled — rather than
			// silently absorbed as a create, which could be actively wrong for
			// whatever that kind turns out to mean.
		}
	}

	for _, op := range p.Operations {
		if op.Kind == OpNoOp {
			continue
		}
		if op.Kind == OpReplace {
			// The two phases of one replacement are ordered relative to each
			// other even when nothing else depends on it: a replacement with
			// no dependents still has to destroy before it creates — or, with
			// create_before_destroy, the other way about. Every
			// other edge in this function relates two DIFFERENT addresses,
			// so without this, a dependent-free replacement's own pair is
			// left unconstrained and can be scheduled in either order.
			//
			// Both lookups are checked explicitly, unlike every other Edge
			// call in this file which already goes through an `ok` check
			// via addEdges: the node-adding switch above and this check both
			// key on op.Kind == OpReplace over the same p.Operations slice,
			// so today they can never disagree. If that invariant is ever
			// broken by an edit to one side and not the other, panicking
			// here with a named cause is more legible than the alternative
			// — Edge panicking on a zero-value OpNode whose ID() is
			// "noop:<addr>", which reads as an unrelated mystery.
			d, dok := has[op.Address.String()][PhaseDestroy]
			c, cok := has[op.Address.String()][PhaseCreate]
			if !dok || !cok {
				panic("planner: BuildExecution: replace at " + op.Address.String() +
					" is missing a phase node — the node-adding and edge-adding passes disagree")
			}
			// THE ONE EDGE create_before_destroy REVERSES. Everything else
			// about the replacement is unchanged: the same two nodes, the same
			// provider calls, the same state writes. What differs is which of
			// them may run first, and therefore whether there is a moment when
			// the resource does not exist.
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
// Each side is looked up as the OpNode itself, not reassembled from a
// "destroy:"/"create:" prefix: OpForget's destroy-phase node renders as
// "forget:", so a prefix-based ID would name a node that was never Add-ed
// and Edge would panic. Asking the node for its own ID keeps this correct
// for every kind, including forget.
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
		// create_before_destroy, and this is the edge that makes it MEAN
		// something rather than merely reorder two calls.
		//
		// The dependent's own work — an update pointing it at the new object,
		// or its own creation — must finish BEFORE the old object is destroyed.
		// Without it the old object could be torn down the instant the new one
		// exists, while everything still refers to the old: the outage the flag
		// was set to avoid, arriving one step later.
		if dc, ok := has[d][PhaseCreate]; ok {
			if td, ok := has[t][PhaseDestroy]; ok {
				g.Edge(dc.ID(), td.ID())
			}
		}
		// The "tear the dependent down before rebuilding the target" edge is
		// deliberately NOT added here. It says the target's create waits for
		// the dependent's destroy, which is the exact opposite of what this
		// flag asks for — and combined with the edge above it is a cycle.
		return
	}

	// A dependent being torn down must go before the target is rebuilt.
	if dd, ok := has[d][PhaseDestroy]; ok {
		if tc, ok := has[t][PhaseCreate]; ok {
			g.Edge(dd.ID(), tc.ID())
		}
	}
}
