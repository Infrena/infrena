package graph

import (
	"strconv"
	"strings"
	"testing"
)

type testNode string

func (n testNode) ID() string { return string(n) }

func build(t *testing.T, nodes []string, edges [][2]string) *Graph[testNode] {
	t.Helper()
	g := New[testNode]()
	for _, id := range nodes {
		g.Add(testNode(id))
	}
	for _, e := range edges {
		g.Edge(e[0], e[1])
	}
	return g
}

func TestRootsOnAGraphWithNoEdgesReturnsEveryNodeSorted(t *testing.T) {
	g := build(t, []string{"c", "a", "b"}, nil)
	roots := g.Roots()
	if len(roots) != 3 || roots[0] != "a" || roots[1] != "b" || roots[2] != "c" {
		t.Errorf("Roots() = %v, want [a b c] sorted", roots)
	}
}

func TestAddReplacesAnExistingID(t *testing.T) {
	g := New[testNode]()
	g.Add(testNode("a"))
	g.Add(testNode("a")) // same ID: replaces, does not duplicate
	if len(g.Roots()) != 1 {
		t.Errorf("Roots() = %v, want exactly one node", g.Roots())
	}
}

func TestRootsExcludesNodesWithAnIncomingEdge(t *testing.T) {
	g := build(t, []string{"a", "b", "c"}, [][2]string{{"a", "b"}})
	roots := g.Roots()
	if len(roots) != 2 || roots[0] != "a" || roots[1] != "c" {
		t.Errorf("Roots() = %v, want [a c] — b has a predecessor", roots)
	}
}

func TestCycleIsNilOnAnAcyclicGraph(t *testing.T) {
	g := build(t, []string{"a", "b", "c"}, [][2]string{{"a", "b"}, {"b", "c"}})
	if cycle := g.Cycle(); cycle != nil {
		t.Errorf("Cycle() = %v, want nil", cycle)
	}
}

func TestCycleFindsAThreeNodeCycle(t *testing.T) {
	g := build(t, []string{"a", "b", "c"}, [][2]string{{"a", "b"}, {"b", "c"}, {"c", "a"}})
	cycle := g.Cycle()
	if len(cycle) != 3 {
		t.Fatalf("Cycle() = %v, want a 3-node cycle", cycle)
	}
	// The cycle is a-b-c in some rotation; every consecutive pair, including
	// the wrap, must be an edge this graph actually has.
	edges := map[[2]string]bool{{"a", "b"}: true, {"b", "c"}: true, {"c", "a"}: true}
	for i := range cycle {
		pair := [2]string{cycle[i], cycle[(i+1)%len(cycle)]}
		if !edges[pair] {
			t.Errorf("Cycle() = %v names pair %v which is not an edge", cycle, pair)
		}
	}
}

func TestCycleOfOneIsASelfEdge(t *testing.T) {
	g := build(t, []string{"a"}, [][2]string{{"a", "a"}})
	cycle := g.Cycle()
	if len(cycle) != 1 || cycle[0] != "a" {
		t.Errorf("Cycle() = %v, want [a]", cycle)
	}
}

func TestCycleIsDeterministicAcrossRuns(t *testing.T) {
	g := build(t, []string{"a", "b", "c", "d"}, [][2]string{
		{"d", "a"}, {"a", "b"}, {"b", "c"}, {"c", "a"},
	})
	first := g.Cycle()
	for i := 0; i < 20; i++ {
		if got := g.Cycle(); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("Cycle() = %v, want %v on every run — map iteration is randomised", got, first)
		}
	}
}

func TestLayersOrdersADiamond(t *testing.T) {
	//   a
	//  / \
	// b   c
	//  \ /
	//   d
	g := build(t, []string{"a", "b", "c", "d"}, [][2]string{
		{"a", "b"}, {"a", "c"}, {"b", "d"}, {"c", "d"},
	})
	layers, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if len(layers) != 3 {
		t.Fatalf("Layers() has %d layers, want 3", len(layers))
	}
	if len(layers[0]) != 1 || layers[0][0] != "a" {
		t.Errorf("layer 0 = %v, want [a]", layers[0])
	}
	if len(layers[1]) != 2 || layers[1][0] != "b" || layers[1][1] != "c" {
		t.Errorf("layer 1 = %v, want [b c] sorted", layers[1])
	}
	if len(layers[2]) != 1 || layers[2][0] != "d" {
		t.Errorf("layer 2 = %v, want [d]", layers[2])
	}
}

func TestLayersOnAnEmptyGraphIsEmpty(t *testing.T) {
	g := New[testNode]()
	layers, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if len(layers) != 0 {
		t.Errorf("Layers() = %v, want none", layers)
	}
}

func TestLayersIsDeterministicAcrossRuns(t *testing.T) {
	g := build(t, []string{"a", "b", "c", "d", "e"}, [][2]string{
		{"a", "c"}, {"b", "c"}, {"c", "d"}, {"c", "e"},
	})
	render := func(layers [][]testNode) string {
		var b strings.Builder
		for _, l := range layers {
			for _, n := range l {
				b.WriteString(string(n))
				b.WriteByte(',')
			}
			b.WriteByte('|')
		}
		return b.String()
	}
	first, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	want := render(first)
	for i := 0; i < 20; i++ {
		got, err := g.Layers()
		if err != nil {
			t.Fatalf("Layers: %v", err)
		}
		if render(got) != want {
			t.Fatalf("Layers() = %q, want %q on every run", render(got), want)
		}
	}
}

func TestLayersReportsACycleByName(t *testing.T) {
	// Node names are deliberately not single letters. Asserting that an error
	// message contains "a" is satisfied by the words "graph" and "detected",
	// so the original form of this test passed whether or not the members
	// were ever named. A name that cannot appear by accident is what makes
	// the assertion mean anything.
	g := build(t, []string{"alpha", "bravo"}, [][2]string{{"alpha", "bravo"}, {"bravo", "alpha"}})
	_, err := g.Layers()
	if err == nil {
		t.Fatal("Layers() on a cyclic graph must return an error, not a partial result")
	}
	for _, name := range []string{"alpha", "bravo"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error must name every member of the cycle, missing %q: %v", name, err)
		}
	}
}

// structNode is a struct used BY VALUE, which is how Task 18 instantiates
// this package (graph.Graph[planner.OpNode], where OpNode is a struct). The
// rest of this file uses `type testNode string`, and a named string type
// passing proves nothing about a struct: they are different kinds, and the
// gap is exactly where a generic constraint mistake would hide.
type structNode struct {
	Name  string
	Phase int
}

func (n structNode) ID() string { return n.Name + ":" + strconv.Itoa(n.Phase) }

func TestGraphAcceptsAStructValueType(t *testing.T) {
	g := New[structNode]()
	first := structNode{Name: "database", Phase: 0}
	second := structNode{Name: "database", Phase: 1}
	other := structNode{Name: "network", Phase: 0}
	for _, n := range []structNode{first, second, other} {
		g.Add(n)
	}
	g.Edge(other.ID(), first.ID())
	g.Edge(first.ID(), second.ID())

	roots := g.Roots()
	if len(roots) != 1 || roots[0] != other {
		t.Fatalf("Roots() = %v, want exactly %v", roots, other)
	}

	layers, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	want := [][]structNode{{other}, {first}, {second}}
	if len(layers) != len(want) {
		t.Fatalf("Layers() = %v, want %v", layers, want)
	}
	for i := range want {
		if len(layers[i]) != len(want[i]) || layers[i][0] != want[i][0] {
			t.Errorf("layer %d = %v, want %v", i, layers[i], want[i])
		}
	}

	// Two nodes sharing a Name but differing in Phase are distinct nodes.
	// This is the shape Task 18 relies on for a replace, which is a destroy
	// and a create at the same address.
	if g.Cycle() != nil {
		t.Errorf("acyclic graph reported a cycle: %v", g.Cycle())
	}
}

func TestDuplicateEdgeIsOneEdge(t *testing.T) {
	g := build(t, []string{"a", "b"}, [][2]string{{"a", "b"}, {"a", "b"}})
	layers, err := g.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if len(layers) != 2 || len(layers[0]) != 1 || len(layers[1]) != 1 {
		t.Errorf("Layers() = %v, want two singleton layers regardless of the duplicate edge", layers)
	}
}

func TestEdgeToAnUnaddedNodePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Edge naming a node that was never added must panic — it is a programming error in the caller, never user input")
		}
	}()
	g := New[testNode]()
	g.Add(testNode("a"))
	g.Edge("a", "ghost")
}

func TestEdgeFromAnUnaddedNodePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Edge naming an unadded source node must panic")
		}
	}()
	g := New[testNode]()
	g.Add(testNode("a"))
	g.Edge("ghost", "a")
}
