package graph

import (
	"fmt"
	"sort"
	"strings"
)

// walkStatus is one node's progress through a Walk.
type walkStatus uint8

const (
	statusUnstarted walkStatus = iota
	statusDispatched
	statusDone
	statusSkipped
)

// String names a walkStatus for panic messages.
func (s walkStatus) String() string {
	switch s {
	case statusUnstarted:
		return "unstarted"
	case statusDispatched:
		return "dispatched"
	case statusDone:
		return "done"
	case statusSkipped:
		return "skipped"
	default:
		return "invalid"
	}
}

// Walk tracks a graph's execution readiness incrementally. Unlike Layers,
// which is a barrier where every node in layer N finishes before layer N+1
// starts, Walk exposes a live ready queue: a node becomes ready the instant
// its own predecessors are done, regardless of what else is still running.
//
// Not safe for concurrent use. The executor owns one Walk from a single
// goroutine; the concurrency lives in what runs the operations a Walk hands
// out, not in the Walk itself.
//
// The state machine the methods below all depend on:
//
//   - Every node is, at any moment, exactly one of unstarted, dispatched,
//     done, or skipped.
//   - A node becomes dispatched only when every one of its own predecessors
//     has reached done — never because of anything skipped elsewhere. A
//     dependent of a failed (therefore skipped) node can consequently never
//     be dispatched, so it is always still unstarted when Skip retires it.
//   - Skip decrements the Walk's total remaining count, but never an
//     individual node's per-predecessor count: that field exists only for
//     Done's bookkeeping, and Skip resolves a node through status instead.
//   - Skip returns any given node at most once for the lifetime of a Walk,
//     however many separate failures reach it transitively.
type Walk[T Node] struct {
	nodes     map[string]T
	out       map[string]map[string]bool
	remaining map[string]int
	status    map[string]walkStatus
	left      int
}

// Walk begins an incremental readiness walk over g. It rejects a cyclic
// graph: nodes in a cycle could never resolve to done or skipped, so
// Remaining would never reach zero and the executor would hang instead of
// reporting an error.
func (g *Graph[T]) Walk() (*Walk[T], error) {
	if cycle := g.Cycle(); cycle != nil {
		full := append(append([]string(nil), cycle...), cycle[0])
		return nil, fmt.Errorf("graph: cycle detected: %s", strings.Join(full, " -> "))
	}

	w := &Walk[T]{
		nodes:     make(map[string]T, len(g.nodes)),
		out:       g.out,
		remaining: make(map[string]int, len(g.nodes)),
		status:    make(map[string]walkStatus, len(g.nodes)),
		left:      len(g.nodes),
	}
	for id, n := range g.nodes {
		w.nodes[id] = n
		w.remaining[id] = len(g.in[id])
	}
	return w, nil
}

// mustKnow panics if id was never added to the graph this Walk began from.
// Every id reaching a Walk comes from the executor — off Ready, Done or
// Skip's own return values — never from user input, so an unknown id is a
// caller bug. Absorbed as a zero value it would instead decrement Remaining
// for a node that does not exist, leaving the Walk quietly wrong.
func (w *Walk[T]) mustKnow(method, id string) {
	if _, ok := w.nodes[id]; !ok {
		panic("graph: Walk." + method + "(" + id + "): " + id + " was never added")
	}
}

// mustBeDispatched panics if id is not currently dispatched. Done and Skip
// both call it first, which is what makes "each id is resolved exactly once"
// a property of the type rather than a discipline callers must keep: a
// second Done or Skip for one id panics before it can touch a
// remaining-predecessor count twice.
func (w *Walk[T]) mustBeDispatched(method, id string) {
	if w.status[id] != statusDispatched {
		panic(fmt.Sprintf("graph: Walk.%s(%s): status is %s, not dispatched — %s must be called exactly once per id, only on an id previously returned by Ready, Done, or Skip", method, id, w.status[id], method))
	}
}

// sortedOut returns the IDs id points to, sorted. It duplicates
// Graph.sortedOut because a Walk holds g.out directly rather than a
// *Graph[T]; the graph is fully built before a Walk exists.
//
// Both current callers re-sort their own results, so the sort here is not
// load-bearing today. It keeps the traversal itself deterministic for a
// future caller that reads the order directly.
func (w *Walk[T]) sortedOut(id string) []string {
	next := w.out[id]
	out := make([]string, 0, len(next))
	for n := range next {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Ready returns every node with no unfinished predecessor that has not
// already been handed out. Each returned node is marked dispatched, so a
// worker pool can call Ready as often as it likes without tracking dispatch
// state of its own.
func (w *Walk[T]) Ready() []T {
	var ids []string
	for id, r := range w.remaining {
		if r == 0 && w.status[id] == statusUnstarted {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	out := make([]T, 0, len(ids))
	for _, id := range ids {
		w.status[id] = statusDispatched
		out = append(out, w.nodes[id])
	}
	return out
}

// Done marks id complete and returns the nodes that become ready as a
// direct result: every successor of id whose last unfinished predecessor was
// id itself. A successor still waiting on another predecessor is not
// returned, however much of the rest of the graph has finished.
//
// Done panics if id was never added, or if id is not currently dispatched —
// so a second Done for one id, or a completion for something never
// dispatched, fails loudly. Absorbed silently it would double-decrement a
// shared successor's remaining count and hand that successor out before its
// real dependencies had finished.
//
// The result is already sorted: it filters sortedOut(id), and filtering
// cannot unsort.
func (w *Walk[T]) Done(id string) []T {
	w.mustKnow("Done", id)
	w.mustBeDispatched("Done", id)

	w.status[id] = statusDone
	w.left--

	var ready []T
	for _, next := range w.sortedOut(id) {
		// Defensive: nothing today depends on this guard, because Skip
		// never decrements a node's own remaining count, so an already
		// skipped successor can never reach zero here anyway. It is cheap
		// insurance should Skip's contract ever change.
		if w.status[next] != statusUnstarted {
			continue
		}
		w.remaining[next]--
		if w.remaining[next] == 0 {
			w.status[next] = statusDispatched
			ready = append(ready, w.nodes[next])
		}
	}
	return ready
}

// Skip marks id, and every node transitively reachable from it, as skipped,
// and returns those nodes sorted by ID. This is how a failure stops its own
// branch rather than the whole run: a dependent of a failed operation can
// never legitimately run, so Skip retires it instead of leaving it waiting
// with Remaining never reaching zero.
//
// It returns nodes rather than IDs because the caller needs each skipped
// node's address and kind to report what was skipped, and the Walk already
// holds them.
//
// id itself is never in the result. The caller knows id failed — that is
// why it called Skip — and reports it separately.
//
// Skip panics on an unknown or not-currently-dispatched id, like Done. Two
// independent failures sharing a downstream dependent is the legitimate
// case, and it calls Skip once per failed id, not twice for one id.
//
// A node reached transitively that is already skipped stops the descent, so
// neither Remaining nor the result double-counts it however many failed
// ancestors reach it. The executor relies on this directly.
func (w *Walk[T]) Skip(id string) []T {
	w.mustKnow("Skip", id)
	w.mustBeDispatched("Skip", id)

	var skippedIDs []string
	seen := map[string]bool{id: true}
	queue := []string{id}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if w.status[cur] == statusSkipped {
			continue
		}
		if w.status[cur] == statusUnstarted || w.status[cur] == statusDispatched {
			w.left--
		}
		w.status[cur] = statusSkipped
		if cur != id {
			skippedIDs = append(skippedIDs, cur)
		}

		for _, next := range w.sortedOut(cur) {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}

	sort.Strings(skippedIDs)
	skipped := make([]T, len(skippedIDs))
	for i, sid := range skippedIDs {
		skipped[i] = w.nodes[sid]
	}
	return skipped
}

// Remaining reports how many nodes have not yet been marked done or
// skipped. It reaches zero when every node has been resolved one way or the
// other, which is the executor's signal that the run is finished.
func (w *Walk[T]) Remaining() int { return w.left }
