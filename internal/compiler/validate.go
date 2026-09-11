package compiler

import (
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/graph"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/value"
)

// validateGraph is compiler stage 8. It runs once schema binding has
// confirmed every resource's type, kinds and required attributes, and checks
// what only the whole graph can reveal: dependency cycles, infrastructure a
// resource needs but configuration never supplies, and lifecycle settings
// that contradict each other. None of these checks contact a provider.
func validateGraph(cfg *ResolvedConfig, reg *registry.Registry) diag.Diagnostics {
	var ds diag.Diagnostics

	if cycle := cycleFor(cfg); cycle != nil {
		ds.Add(cycleDiagnostic(cfg, cycle))
	}

	checkRequirements(cfg, reg, &ds)
	checkLifecycle(cfg, &ds)

	return ds
}

// --- dependency cycles ------------------------------------------------

// cycleFor reports a dependency cycle in the resolved configuration, or nil.
// Detection lives in internal/graph so the compiler and the executor cannot
// disagree about what a cycle is.
//
// Edges run resource → dependency, the direction the diagnostic reads in
// ("a → b" means a depends on b), which is the opposite of the planner's
// graph, where an edge means "must execute first". Both orientations detect
// the same cycles; only this one prints in the order the message claims.
//
// graph.Cycle returns members without repeating the first at the end. The
// closing repeat is re-added here because cycleDiagnostic slices
// cycle[1:len(cycle)-1] for its Related list and would otherwise drop a real
// member of the cycle.
func cycleFor(cfg *ResolvedConfig) []address.Address {
	g := graph.New[addrNode]()
	for _, addr := range cfg.Addresses() {
		g.Add(addrNode{addr})
	}
	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]
		for _, dep := range r.DependsOn {
			if _, ok := cfg.Get(dep); !ok {
				continue // not a node in this graph; stage 6 already reported it
			}
			g.Edge(addr.String(), dep.String())
		}
	}

	ids := g.Cycle()
	if len(ids) == 0 {
		return nil
	}

	out := make([]address.Address, 0, len(ids)+1)
	for _, id := range ids {
		addr, err := address.Parse(id)
		if err != nil {
			// Impossible: every id came from an Address we put in.
			panic("compiler: graph returned an unparseable address " + strconv.Quote(id) + ": " + err.Error())
		}
		out = append(out, addr)
	}
	return append(out, out[0])
}

// addrNode adapts an address to the graph's Node interface.
type addrNode struct{ addr address.Address }

func (n addrNode) ID() string { return n.addr.String() }

// cycleKey renders a closed cycle as "a → b → c → a", used both as the
// diagnostic text and as the deduplication key.
func cycleKey(cycle []address.Address) string {
	parts := make([]string, len(cycle))
	for i, a := range cycle {
		parts[i] = a.String()
	}
	return strings.Join(parts, " → ")
}

// cycleDiagnostic renders one closed cycle as an error diagnostic, pointing
// at the origin of the first resource on it and naming the rest as related.
func cycleDiagnostic(cfg *ResolvedConfig, cycle []address.Address) diag.Diagnostic {
	origin := value.Origin{}
	if r, ok := cfg.Get(cycle[0]); ok {
		origin = r.Origin
	}
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "dependency cycle: " + cycleKey(cycle),
		Detail:   "Each resource in the cycle depends, directly or indirectly, on itself through the others, so none of them could ever be created first.",
		Action:   "Break the cycle by removing one dependency, or restructuring the resources so it is not required.",
		Origin:   origin,
		Related:  cycle[1 : len(cycle)-1],
	}
}

// --- requirements -------------------------------------------------------

// checkRequirements enforces the Requirement mechanism (spec §8.1): a
// non-optional requirement unsatisfied by anything in configuration is an
// error, reported before any provider is ever called — "an application
// needs a database," not an opaque failure once a provider API is finally
// asked to create it (PLAN.md §17).
//
// Satisfaction here is existence across the whole resolved configuration, not
// a traced reference from the specific resource that declares the
// requirement: Requirement carries no attribute name to trace against, so
// there is no principled way to demand a specific edge. The spec also allows
// a requirement to be satisfied by an existing resource already in state, but
// validateGraph has no state to consult — that half of satisfaction belongs
// to whichever stage has state, not to compilation.
func checkRequirements(cfg *ResolvedConfig, reg *registry.Registry, ds *diag.Diagnostics) {
	present := map[string]bool{}
	for _, r := range cfg.Resources {
		present[r.Type] = true
	}

	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]

		def, ok := reg.Definition(r.Type)
		if !ok {
			// An unresolved type is stage 7's problem. Compile does not reach
			// this stage until stage 7 is clean; a direct caller of
			// validateGraph passing an unresolved type gets no further noise.
			continue
		}

		for _, req := range def.Requirements {
			if req.Optional || satisfiesAny(present, req.Types) {
				continue
			}
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(addr.String()) + " is missing required " + req.Name,
				Detail:   req.Description + "\nSatisfied by a resource of type: " + strings.Join(req.Types, ", "),
				Action:   "Add a resource of type " + strings.Join(req.Types, " or ") + " to this configuration.",
				Origin:   r.Origin,
			})
		}
	}
}

// satisfiesAny reports whether any of types is present in the configuration.
func satisfiesAny(present map[string]bool, types []string) bool {
	for _, t := range types {
		if present[t] {
			return true
		}
	}
	return false
}

// --- lifecycle ------------------------------------------------------------

// checkLifecycle rejects lifecycle settings that contradict each other.
// prevent_destroy refuses to destroy the resource; retain abandons it from
// state without destroying it — a resource cannot mean both at once.
func checkLifecycle(cfg *ResolvedConfig, ds *diag.Diagnostics) {
	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]
		if r.Lifecycle.PreventDestroy && r.Lifecycle.Retain {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(addr.String()) + " sets both prevent_destroy and retain",
				Detail:   "prevent_destroy refuses to destroy the resource; retain abandons it from state without destroying it. Setting both is a contradiction.",
				Action:   "Choose one: prevent_destroy to keep infra managing it and refuse destruction, or retain to let infra forget it without deleting it.",
				Origin:   r.Origin,
			})
		}
	}
}
