package graph

import (
	"strings"
	"testing"
)

func TestWalkOnACyclicGraphErrors(t *testing.T) {
	g := build(t, []string{"a", "b"}, [][2]string{{"a", "b"}, {"b", "a"}})
	_, err := g.Walk()
	if err == nil {
		t.Fatal("Walk() over a cyclic graph must error, the same way Layers() does")
	}
	for _, name := range []string{"a", "b"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error must name %q: %v", name, err)
		}
	}
}

func TestReadyReturnsEveryRootOnceEach(t *testing.T) {
	g := build(t, []string{"c", "a", "b"}, nil)
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	ready := w.Ready()
	if len(ready) != 3 || ready[0] != "a" || ready[1] != "b" || ready[2] != "c" {
		t.Fatalf("Ready() = %v, want [a b c] sorted", ready)
	}
}

func TestReadyDoesNotReturnAnAlreadyDispatchedNode(t *testing.T) {
	g := build(t, []string{"b", "a"}, nil)
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	first := w.Ready()
	if len(first) != 2 {
		t.Fatalf("first Ready() = %v, want both roots", first)
	}
	second := w.Ready()
	if len(second) != 0 {
		t.Errorf("second Ready() = %v, want none — both nodes were already dispatched by the first call", second)
	}
}

func TestReadyDoesNotWaitForAnUnrelatedSlowNodeInTheSameLayer(t *testing.T) {
	// slow, fastA and fastB are all roots — one layer under Layers(). child
	// depends only on fastA and fastB, never on slow. A barrier scheduler
	// (Layers) would hold child back until the WHOLE layer, including slow,
	// finishes; Walk must not, since child's own predecessors say nothing
	// about slow. This is the exact gap between Layers (§14) and the "ready
	// queue of operations whose predecessors have completed" §15 asks for.
	g := New[testNode]()
	for _, id := range []string{"slow", "fastA", "fastB", "child"} {
		g.Add(testNode(id))
	}
	g.Edge("fastA", "child")
	g.Edge("fastB", "child")

	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	ready := w.Ready()
	if len(ready) != 3 || ready[0] != "fastA" || ready[1] != "fastB" || ready[2] != "slow" {
		t.Fatalf("Ready() = %v, want [fastA fastB slow]", ready)
	}

	if got := w.Done("fastA"); len(got) != 0 {
		t.Errorf("Done(fastA) = %v, want none — fastB has not completed yet", got)
	}
	got := w.Done("fastB")
	if len(got) != 1 || got[0] != "child" {
		t.Fatalf("Done(fastB) = %v, want [child] — both of child's predecessors have completed, and slow is not one of them", got)
	}
	if w.Remaining() != 2 {
		t.Errorf("Remaining() = %d, want 2 — slow has not finished, and child is ready but not yet done", w.Remaining())
	}
}

func TestDoneTriggersReadinessRegardlessOfPredecessorCompletionOrder(t *testing.T) {
	// child depends on zeta and alpha. Finishing zeta — the alphabetically
	// LATER of the two — first must not trigger readiness; only finishing
	// both does, regardless of which order they complete in. An
	// implementation that (wrongly) assumed predecessors resolve in ID
	// order would pass this the other way around, which is why the
	// out-of-alphabetical-order finish comes first.
	g := build(t, []string{"alpha", "zeta", "child"}, [][2]string{
		{"alpha", "child"}, {"zeta", "child"},
	})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready()

	if got := w.Done("zeta"); len(got) != 0 {
		t.Errorf("Done(zeta) = %v, want none — alpha has not finished", got)
	}
	got := w.Done("alpha")
	if len(got) != 1 || got[0] != "child" {
		t.Fatalf("Done(alpha) = %v, want [child]", got)
	}
}

func TestSkipPropagatesTransitivelyAndSortsByID(t *testing.T) {
	// Topological order is zulu -> mike -> alpha; alphabetical order is the
	// reverse. Skip's contract is "sorted", not "in propagation order" — a
	// fixture where those two orders disagree is required to prove which
	// one comes back.
	g := build(t, []string{"zulu", "mike", "alpha", "other"}, [][2]string{
		{"zulu", "mike"}, {"mike", "alpha"},
	})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready() // dispatches zulu and other

	got := w.Skip("zulu")
	if len(got) != 2 || got[0].ID() != "alpha" || got[1].ID() != "mike" {
		t.Fatalf("Skip(zulu) = %v, want [alpha mike] sorted", got)
	}
	for _, n := range got {
		if n.ID() == "zulu" {
			t.Error("Skip must not include the failed node itself in its result")
		}
	}
	if w.Remaining() != 1 {
		t.Errorf("Remaining() = %d, want 1 — only \"other\" is left, unrelated to zulu's branch", w.Remaining())
	}
}

func TestSkipOnASharedDependentIsNotDoubleCounted(t *testing.T) {
	// c depends on both a and b. If a fails first, Skip(a) skips c. When b
	// also fails, Skip(b) must not report c again — it was already
	// resolved — and must not decrement Remaining a second time for it.
	g := build(t, []string{"a", "b", "c"}, [][2]string{{"a", "c"}, {"b", "c"}})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready() // dispatches a and b

	first := w.Skip("a")
	if len(first) != 1 || first[0].ID() != "c" {
		t.Fatalf("Skip(a) = %v, want [c]", first)
	}
	afterFirst := w.Remaining()

	second := w.Skip("b")
	if len(second) != 0 {
		t.Errorf("Skip(b) = %v, want none — c was already skipped by Skip(a)", second)
	}
	if w.Remaining() != afterFirst-1 {
		t.Errorf("Remaining() = %d, want %d — only b itself should be newly resolved, not c a second time", w.Remaining(), afterFirst-1)
	}
	if w.Remaining() != 0 {
		t.Errorf("Remaining() = %d, want 0 — a, b and c are all resolved", w.Remaining())
	}
}

func TestRemainingCountsDownAsNodesResolve(t *testing.T) {
	g := build(t, []string{"a", "b"}, [][2]string{{"a", "b"}})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if w.Remaining() != 2 {
		t.Fatalf("Remaining() = %d, want 2 before anything runs", w.Remaining())
	}
	w.Ready()
	w.Done("a")
	if w.Remaining() != 1 {
		t.Errorf("Remaining() = %d, want 1 after a is done", w.Remaining())
	}
	w.Done("b")
	if w.Remaining() != 0 {
		t.Errorf("Remaining() = %d, want 0 after everything is done", w.Remaining())
	}
}

func TestWalkWorksOverStructNodes(t *testing.T) {
	// The executor instantiates Walk[planner.OpNode], a struct. Proving the
	// generic works with testNode, a named string type, elsewhere in this
	// package proves nothing about a struct constraint — structNode (from
	// graph_test.go) is what closes that gap here, the same way it does for
	// Graph itself in TestGraphAcceptsAStructValueType.
	g := New[structNode]()
	root := structNode{Name: "network", Phase: 0}
	dependent := structNode{Name: "database", Phase: 0}
	g.Add(root)
	g.Add(dependent)
	g.Edge(root.ID(), dependent.ID())

	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	ready := w.Ready()
	if len(ready) != 1 || ready[0] != root {
		t.Fatalf("Ready() = %v, want [%v]", ready, root)
	}
	got := w.Done(root.ID())
	if len(got) != 1 || got[0] != dependent {
		t.Fatalf("Done(%q) = %v, want [%v]", root.ID(), got, dependent)
	}
}

func TestDoneOnAnUnknownIDPanics(t *testing.T) {
	// "ghost" was never added to the graph at all. Done previously accepted
	// it silently, via Go's zero-value map defaults, and decremented
	// Remaining for a node that was never there to begin with. Graph.Edge
	// already panics on the equivalent mistake — naming an id that was
	// never added — for the same reason: ids reaching a Walk always come
	// from the executor itself, never unvalidated input.
	g := build(t, []string{"a"}, nil)
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	before := w.Remaining()

	defer func() {
		if recover() == nil {
			t.Error("Done on an id that was never added must panic")
		}
	}()
	w.Done("ghost")
	if w.Remaining() != before {
		t.Errorf("Remaining() changed from %d without a real node ever resolving", before)
	}
}

func TestSkipOnAnUnknownIDPanics(t *testing.T) {
	g := build(t, []string{"a"}, nil)
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("Skip on an id that was never added must panic")
		}
	}()
	w.Skip("ghost")
}

func TestDoneOnANotYetDispatchedIDPanics(t *testing.T) {
	// b has a real predecessor, a, which has not finished — b was never
	// returned by Ready and is still unstarted. Reporting it done anyway is
	// a caller bug (the executor cannot have run an operation it was never
	// handed), and must not be absorbed silently.
	g := build(t, []string{"a", "b"}, [][2]string{{"a", "b"}})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("Done on an id that was never dispatched must panic")
		}
	}()
	w.Done("b")
}

func TestDoneCalledTwiceDoesNotDoubleDispatchASharedSuccessor(t *testing.T) {
	// child depends on both a and b. Two independent completion reports for
	// a — a caller bug, since a genuine operation only finishes once — must
	// not let child's remaining-predecessor count get decremented twice.
	// That would dispatch child as ready with b never having completed: a
	// false-ready signal, which is exactly what invariant 4 ("a resource
	// never executes before its dependencies", §47) rules out. The
	// assertion that matters is not that the second Done(a) panics — it's
	// that Done(b), the ONLY legitimate second predecessor to finish, is
	// still the one and only call that reports child ready. If the
	// panicking second Done(a) call had reprocessed a's successors before
	// panicking, child would already be dispatched here and this final
	// Done(b) would report nothing (or something already wrong) instead of
	// [child].
	g := build(t, []string{"a", "b", "child"}, [][2]string{
		{"a", "child"}, {"b", "child"},
	})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready() // dispatches a and b

	if got := w.Done("a"); len(got) != 0 {
		t.Fatalf("first Done(a) = %v, want none — b has not finished", got)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("second Done(a) must panic — a is already done, not dispatched")
			}
		}()
		w.Done("a")
	}()

	got := w.Done("b")
	if len(got) != 1 || got[0] != "child" {
		t.Fatalf("Done(b) = %v, want [child] exactly once — the double Done(a) call must not have already dispatched it", got)
	}
}

func TestSkipCalledTwiceOnTheSameIDPanics(t *testing.T) {
	// Distinct from TestSkipOnASharedDependentIsNotDoubleCounted, which
	// calls Skip once each on two DIFFERENT dispatched ids that happen to
	// share a downstream dependent — that is a legitimate pattern and must
	// not panic. This test calls Skip twice on the SAME id, which is
	// always a caller bug: a is already skipped, not dispatched, by the
	// time the second call happens.
	g := build(t, []string{"a", "b"}, [][2]string{{"a", "b"}})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready() // dispatches a

	w.Skip("a")

	defer func() {
		if recover() == nil {
			t.Error("second Skip(a) must panic — a is already skipped, not dispatched")
		}
	}()
	w.Skip("a")
}
