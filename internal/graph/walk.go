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
// The result is already in sorted order: it is built by filtering
// sortedOut(id), and filtering a sorted sequence cannot unsort it.
func (w *Walk[T]) Done(id string) []T {
	if w.status[id] == statusUnstarted || w.status[id] == statusDispatched {
		w.left--
	}
	w.status[id] = statusDone

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
// transitively — as skipped, and returns their IDs sorted. This is how "a
// failure stops its branch, not the world" (spec §15) is implemented: a
// dependent of a failed operation can never legitimately run, since at
// least one of its own predecessors never completed, so Skip retires it
// without ever dispatching it, rather than leaving it to wait forever with
// Remaining never reaching zero.
//
// id itself is never in the returned slice. The caller already knows id
// failed — that is why Skip was called instead of Done — and reports it
// separately; what this returns is exactly the set the caller should add to
// its own report of skipped work.
//
// Calling Skip a second time for a node already skipped by an earlier call
// — because two independent failures share a dependent — is safe and
// returns none of it again: the moment a node is found already
// statusSkipped, this stops descending through it, so neither Remaining nor
// the returned slice double-counts it.
func (w *Walk[T]) Skip(id string) []string {
	var skipped []string
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
			skipped = append(skipped, cur)
		}

		for _, next := range w.sortedOut(cur) {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}

	sort.Strings(skipped)
	return skipped
}

// Remaining reports how many nodes have not yet been marked done or
// skipped. It reaches zero exactly when every node the Walk started with
// has been resolved one way or the other — successfully or not — which is
// the executor's own signal that its run over this graph is finished.
func (w *Walk[T]) Remaining() int { return w.left }
