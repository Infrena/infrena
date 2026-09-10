package compiler

import (
	"sort"
	"strconv"
	"strings"

	"infra/internal/diag"
	"infra/internal/registry"
	"infra/pkg/address"
	"infra/pkg/value"
)

// validateGraph is compiler stage 8. It runs once schema binding has
// confirmed every resource's type, kinds and required attributes, and checks
// what only the whole graph can reveal: dependency cycles, infrastructure a
// resource needs but configuration never supplies, and lifecycle settings
// that contradict each other. None of these checks contact a provider.
func validateGraph(cfg *ResolvedConfig, reg *registry.Registry) diag.Diagnostics {
	var ds diag.Diagnostics

	for _, cycle := range findCycles(cfg) {
		ds.Add(cycleDiagnostic(cfg, cycle))
	}

	checkRequirements(cfg, reg, &ds)
	checkLifecycle(cfg, &ds)

	return ds
}

// --- dependency cycles ------------------------------------------------

// color tracks a node's state during the depth-first walk: not yet visited,
// on the current path, or fully explored. A plain visited set would say a
// cycle exists but not which nodes are actually on it versus merely
// reachable from it; the third state is what lets the walk tell the two
// apart.
type color int

const (
	white color = iota
	gray
	black
)

// findCycles walks the dependency graph and returns every distinct cycle,
// each in order and closed (a → b → c → a), deduplicated and sorted for a
// deterministic result. A cycle of one — a resource referring to itself — is
// already rejected when stage 6 binds the reference; nothing here special-
// cases that, though the same walk would still catch one if it ever reached
// this stage some other way.
func findCycles(cfg *ResolvedConfig) [][]address.Address {
	colors := map[string]color{}
	onStack := map[string]int{}
	var stack []address.Address
	var found [][]address.Address

	var visit func(addr address.Address)
	visit = func(addr address.Address) {
		key := addr.String()
		colors[key] = gray
		onStack[key] = len(stack)
		stack = append(stack, addr)

		if r, ok := cfg.Get(addr); ok {
			for _, dep := range r.DependsOn {
				dk := dep.String()
				switch colors[dk] {
				case white:
					visit(dep)
				case gray:
					// A back edge to a node still on the path: the cycle is
					// everything from that node to here, closed by repeating it.
					start := onStack[dk]
					cycle := append([]address.Address{}, stack[start:]...)
					cycle = append(cycle, dep)
					found = append(found, cycle)
				}
			}
		}

		stack = stack[:len(stack)-1]
		delete(onStack, key)
		colors[key] = black
	}

	for _, addr := range cfg.Addresses() {
		if colors[addr.String()] == white {
			visit(addr)
		}
	}

	return dedupeCycles(found)
}

// dedupeCycles collapses cycles discovered more than once — the same loop is
// found again starting from any of its members — by rotating each to start
// at its lexicographically smallest address, then removing repeats. The
// result is sorted so the diagnostic list is stable across runs.
func dedupeCycles(cycles [][]address.Address) [][]address.Address {
	seen := map[string]bool{}
	var out [][]address.Address

	for _, c := range cycles {
		body := c[:len(c)-1] // drop the closing repeat of the first element
		minIdx := 0
		for i, a := range body {
			if a.String() < body[minIdx].String() {
				minIdx = i
			}
		}
		rotated := append(append([]address.Address{}, body[minIdx:]...), body[:minIdx]...)
		rotated = append(rotated, rotated[0])

		key := cycleKey(rotated)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, rotated)
	}

	sort.Slice(out, func(i, j int) bool { return cycleKey(out[i]) < cycleKey(out[j]) })
	return out
}

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
