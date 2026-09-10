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

// String names a walkStatus for use in panic messages — Done and Skip
// report the status they found instead of the status they needed, so a
// caller sees exactly what went wrong without instrumenting the Walk
// itself.
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

// Walk tracks a graph's execution readiness incrementally: unlike Layers,
// which is a barrier where every node in layer N finishes before layer N+1
// starts, Walk exposes a live ready queue — a node becomes ready the instant
// its own predecessors are done, regardless of what else is still running
// elsewhere in the same graph. Spec §15 asks for exactly this: "a ready
// queue of operations whose predecessors have completed," which is strictly
// more parallel than a layer barrier whenever a graph has a slow node
// sharing a layer with unrelated fast ones.
//
// Not safe for concurrent use. The executor owns one Walk from a single
// goroutine — the one that accepts completions off a worker pool and hands
// out newly-ready work — never mutated from two goroutines at once; the
// concurrency in "worker pool" lives entirely in what runs the operations a
// Walk hands out, not in the Walk itself.
type Walk[T Node] struct {
	nodes     map[string]T
	out       map[string]map[string]bool
	remaining map[string]int
	status    map[string]walkStatus
	left      int
}

// Walk begins an incremental readiness walk over g. It fails on the same
// cycle Layers would, and for the same reason: a Walk over a cyclic graph
// could never resolve every node to done or skipped, so Remaining would
// never reach zero and an executor built on it would hang instead of
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
// Every id a Walk ever sees comes from the executor itself — off Ready,
// Done or Skip's own return values, or an address the planner already
// validated — never from unvalidated configuration text a user typed. That
// is exactly the trust boundary Graph.Edge enforces on the same g.nodes
// map, for the same stated reason: the mistake belongs at the call site
// that made it, not absorbed as a Go zero-value default that leaves the
// Walk quietly wrong (an unknown id previously decremented Remaining for a
// node that was never there to begin with).
func (w *Walk[T]) mustKnow(method, id string) {
	if _, ok := w.nodes[id]; !ok {
		panic("graph: Walk." + method + "(" + id + "): " + id + " was never added")
	}
}

// mustBeDispatched panics if id is not currently dispatched — the status
// Ready, Done or Skip leaves a node in exactly once, the instant it hands
// that node out. Done and Skip each call this before doing anything else,
// which is what makes "each id resolved exactly once" a property the type
// enforces rather than a discipline callers have to maintain by hand: a
// second Done or Skip call for the same id — two independent completion
// signals for one operation, or a completion signal for an operation that
// was never dispatched — now panics before it can touch a
// remaining-predecessor count a second time. See Done's doc comment for
// why that matters concretely, and why this panics instead of no-oping.
func (w *Walk[T]) mustBeDispatched(method, id string) {
	if w.status[id] != statusDispatched {
		panic(fmt.Sprintf("graph: Walk.%s(%s): status is %s, not dispatched — %s must be called exactly once per id, only on an id previously returned by Ready, Done, or Skip", method, id, w.status[id], method))
	}
}

// sortedOut returns the IDs id points to, sorted — the same guarantee
// Graph.sortedOut makes, kept separate because Walk reads g.out directly
// rather than holding a *Graph[T]; the graph is finished being built by the
// time a Walk exists (Add and Edge are always complete before Walk is
// called), so nothing here needs the rest of Graph's API.
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
// already been handed out. Each returned node is marked dispatched before
// this returns, so calling Ready again before those nodes reach Done or
// Skip returns none of them a second time — which makes it safe for a
// worker pool to call it again after finishing a batch, to pick up anything
// else that became ready in the meantime, without tracking dispatch state
// of its own.
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
// direct result: every successor of id whose last unfinished predecessor
// was id itself. This is the incremental half of the readiness contract —
// a successor with a second, still-unfinished predecessor elsewhere in the
// graph is not returned here, no matter how much else has finished,
// because §15's ready queue is defined by a node's OWN predecessors, not by
// how much of the graph as a whole has completed.
//
// Done panics if id was never added to the graph, or if id is not
// currently dispatched — meaning it was never handed out by Ready, Done or
// Skip, or it already was and Done (or Skip) has already been called for
// it. This is a deliberate choice over a silent no-op: a second Done call
// for the same id is always a caller bug (the executor reporting one
// operation's completion twice, or completion for an operation it never
// dispatched), never a legitimate pattern, and letting it through quietly
// used to double-decrement a shared successor's remaining-predecessor
// count — handing that successor out as ready before all of its real
// dependencies had finished, a false-ready signal that would let the
// executor run a resource ahead of one of its own dependencies (invariant
// 4, §47). Graph.Edge already panics on the equivalent mistake — an edge
// naming an id that was never added — for the same reason: ids reaching a
// Walk always come from the executor itself, never unvalidated input, so
// the mistake belongs at the call site that made it rather than being
// absorbed silently.
//
// The result is already in sorted order: it is built by filtering
// sortedOut(id), and filtering a sorted sequence cannot unsort it.
func (w *Walk[T]) Done(id string) []T {
	w.mustKnow("Done", id)
	w.mustBeDispatched("Done", id)

	w.status[id] = statusDone
	w.left--

	var ready []T
	for _, next := range w.sortedOut(id) {
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

// Skip marks id, and every node reachable from it — its dependents,
// transitively — as skipped, and returns those nodes sorted by ID. This is
// how "a failure stops its branch, not the world" (spec §15) is
// implemented: a dependent of a failed operation can never legitimately
// run, since at least one of its own predecessors never completed, so Skip
// retires it without ever dispatching it, rather than leaving it to wait
// forever with Remaining never reaching zero.
//
// Returns nodes, not IDs: the caller (executor.tracker.recordFailure) needs
// each skipped node's Address and Kind to report what was skipped and why,
// and Walk already holds the node behind every ID in w.nodes — returning
// just the ID would force the caller to rebuild an id→node map purely to
// look up information the walker had the whole time. Result.Skipped is
// still []string; the executor is where that narrowing happens, once, with
// the node in hand — not here, where every caller would pay for it.
//
// id itself is never in the returned slice. The caller already knows id
// failed — that is why Skip was called instead of Done — and reports it
// separately; what this returns is exactly the set the caller should add to
// its own report of skipped work.
//
// Skip panics if id was never added to the graph, or if id itself is not
// currently dispatched — the same two checks, for the same reason and with
// the same panic-over-no-op choice, as Done (see Done's doc comment). A
// second Skip call naming the same id directly is always a caller bug, not
// a legitimate double-failure report: the legitimate case — two
// independent failures whose branches share a downstream dependent — is
// TestSkipOnASharedDependentIsNotDoubleCounted, and it never calls Skip
// twice with the same id; it calls Skip once each on two different ids
// that are each still dispatched at the time, and the transitive walk
// below is what resolves their shared dependent exactly once no matter how
// many failed ancestors reach it.
//
// Calling Skip a second time for a node already skipped by an earlier call
// — because two independent failures share a dependent — is safe and
// returns none of it again: the moment a node is found already
// statusSkipped, this stops descending through it, so neither Remaining nor
// the returned slice double-counts it. That is about a node reached
// TRANSITIVELY through the walk below, not about id itself — id itself is
// covered by the panic above, before the walk ever starts.
//
// A caller may rely on this as a standing guarantee, not just a detail of
// this call: for the lifetime of one Walk, any given node is returned by at
// most one Skip call, ever, no matter how many separate failures reach it
// transitively. executor.tracker.recordFailure already depends on this —
// it is what makes its own t.skipped[id] dedupe guard provably unreachable
// today (internal/executor/isolation.go) — so if this filtering is ever
// changed, that guard becomes load-bearing and needs new test coverage.
//
// The traversal below filters statusSkipped but not statusDone: a
// statusDone node reached transitively would be re-marked statusSkipped and
// appended to the result. That is unreachable today for the same reason a
// dependent of a failed node can never be in flight (Ready only dispatches
// a node once every predecessor is statusDone, and a failed predecessor
// never reaches statusDone) — noted here only because the guarantee above
// is now something callers are told to rely on, and this is the boundary of
// what it currently costs nothing to keep true.
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
// skipped. It reaches zero exactly when every node the Walk started with
// has been resolved one way or the other — successfully or not — which is
// the executor's own signal that its run over this graph is finished.
func (w *Walk[T]) Remaining() int { return w.left }
