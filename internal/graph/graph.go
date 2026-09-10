// Package graph implements a generic, deterministic directed graph used to
// order work: M2 orders plan operations with it, and M3's executor reuses
// it for tasks. Determinism is the point — Cycle, Layers and Roots all sort
// before returning, because Go map iteration is randomised and two runs
// over identical nodes and edges must produce identical output (spec §14,
// invariant 6).
package graph

import (
	"fmt"
	"sort"
	"strings"
)

// Node is anything a Graph can hold: a stable, caller-assigned identifier
// that Add and Edge key on.
type Node interface {
	// ID returns the node's identifier, unique within one Graph.
	ID() string
}

// Graph is a generic directed graph over nodes with unique IDs.
type Graph[T Node] struct {
	nodes map[string]T
	out   map[string]map[string]bool // fromID -> set of toID
	in    map[string]map[string]bool // toID -> set of fromID
}

// New returns an empty graph.
func New[T Node]() *Graph[T] {
	return &Graph[T]{
		nodes: map[string]T{},
		out:   map[string]map[string]bool{},
		in:    map[string]map[string]bool{},
	}
}

// Add registers a node, keyed by its ID. Adding a node whose ID is already
// present replaces it, the same as an ordinary map assignment.
func (g *Graph[T]) Add(node T) {
	g.nodes[node.ID()] = node
}

// Edge records that fromID must run before toID.
//
// Naming a node that was never added is a programming error, not user
// input: a graph in this system is always built by another infra package —
// the planner assembling operations, the executor assembling tasks — from
// addresses it already knows are valid, never from unvalidated
// configuration text a user typed. Edge therefore panics rather than
// returning an error, so the mistake surfaces at the call site that made it
// instead of silently leaving a graph with one fewer edge than its builder
// intended. This matches the one other place the codebase already panics on
// an invariant violation rather than a user-facing one (internal/cli's
// provider registration).
//
// Because Edge panics on an unknown ID, every node an edge names must be
// Add-ed before that edge is recorded — a caller cannot add edges and nodes
// interleaved in a single pass if an edge can be seen before both of its
// endpoints. Callers that build a graph from data where edges may be
// discovered before the nodes they name (for example, a single pass over
// unordered resource definitions) must Add every node first, then record
// edges in a second pass.
func (g *Graph[T]) Edge(fromID, toID string) {
	if _, ok := g.nodes[fromID]; !ok {
		panic("graph: Edge(" + fromID + ", " + toID + "): " + fromID + " was never added")
	}
	if _, ok := g.nodes[toID]; !ok {
		panic("graph: Edge(" + fromID + ", " + toID + "): " + toID + " was never added")
	}
	if g.out[fromID] == nil {
		g.out[fromID] = map[string]bool{}
	}
	g.out[fromID][toID] = true
	if g.in[toID] == nil {
		g.in[toID] = map[string]bool{}
	}
	g.in[toID][fromID] = true
}

// sortedIDs returns every node ID, sorted.
func (g *Graph[T]) sortedIDs() []string {
	ids := make([]string, 0, len(g.nodes))
	for id := range g.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// sortedOut returns the IDs fromID points to, sorted.
func (g *Graph[T]) sortedOut(fromID string) []string {
	next := g.out[fromID]
	out := make([]string, 0, len(next))
	for id := range next {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Cycle returns the first cycle found, as the node IDs in participation
// order — each consecutive pair, including the wrap from the last ID back
// to the first, is a real edge — or nil when the graph is acyclic. Nodes
// and their outgoing edges are both visited in sorted order, so the result
// is identical on every run.
func (g *Graph[T]) Cycle() []string {
	const (
		unvisited = iota
		visiting
		done
	)
	state := make(map[string]int, len(g.nodes))
	var stack []string

	var visit func(id string) []string
	visit = func(id string) []string {
		state[id] = visiting
		stack = append(stack, id)

		for _, next := range g.sortedOut(id) {
			switch state[next] {
			case visiting:
				// next is still on the stack: the cycle is the suffix of
				// stack starting at next.
				for i, s := range stack {
					if s == next {
						return append([]string(nil), stack[i:]...)
					}
				}
			case unvisited:
				if cycle := visit(next); cycle != nil {
					return cycle
				}
			}
		}

		stack = stack[:len(stack)-1]
		state[id] = done
		return nil
	}

	for _, id := range g.sortedIDs() {
		if state[id] == unvisited {
			if cycle := visit(id); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// Layers returns the graph's nodes grouped into topological layers: layer 0
// holds every node with no incoming edge, layer 1 every node whose
// predecessors are all in layer 0, and so on. Each layer is sorted by ID,
// so two runs over the same graph produce identical output.
//
// It returns an error naming the cycle when the graph is not a DAG.
func (g *Graph[T]) Layers() ([][]T, error) {
	if cycle := g.Cycle(); cycle != nil {
		full := append(append([]string(nil), cycle...), cycle[0])
		return nil, fmt.Errorf("graph: cycle detected: %s", strings.Join(full, " -> "))
	}

	remaining := make(map[string]int, len(g.nodes))
	for id := range g.nodes {
		remaining[id] = len(g.in[id])
	}

	var layers [][]T
	for len(remaining) > 0 {
		var ready []string
		for id, degree := range remaining {
			if degree == 0 {
				ready = append(ready, id)
			}
		}
		sort.Strings(ready)

		layer := make([]T, 0, len(ready))
		for _, id := range ready {
			layer = append(layer, g.nodes[id])
			delete(remaining, id)
		}
		for _, id := range ready {
			for _, next := range g.sortedOut(id) {
				if _, ok := remaining[next]; ok {
					remaining[next]--
				}
			}
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

// Roots returns every node with no incoming edge — the nodes with no
// prerequisite, which can run first — sorted by ID.
func (g *Graph[T]) Roots() []T {
	var ids []string
	for id := range g.nodes {
		if len(g.in[id]) == 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	out := make([]T, 0, len(ids))
	for _, id := range ids {
		out = append(out, g.nodes[id])
	}
	return out
}
