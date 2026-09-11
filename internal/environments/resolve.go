// Package environments implements compiler stage 3: it walks an environment's
// `extends` chain and produces the ordered scope stack the variable resolver
// consumes (spec §7 stage 3; PLAN.md §7).
package environments

import (
	"strconv"
	"strings"

	"infra/internal/config"
	"infra/internal/diag"
	"infra/pkg/value"
)

// Layer is one environment in a resolved extends chain.
//
// Overrides are stage 2's slice, passed through untouched. Stage 2 already
// sorted them by name and gave each one its own Origin; a map here would throw
// away the line number a diagnostic needs and force every consumer to re-sort
// what was already ordered.
type Layer struct {
	Name      string
	Overrides []config.OverrideDecl
	Scope     value.Scope
	Origin    value.Origin
}

// Chain is the ordered scope stack stage 3 produces: ancestors first, the
// environment named on the command line last. Later layers win, which is the
// same direction the rest of the precedence ladder runs in, so stage 4 applies
// it with one forward loop and no reversal anywhere.
//
// CONTRACT: the returned Chain is meaningless if the returned diagnostics
// contain errors, and on failure it is deliberately EMPTY rather than partial.
// Half an inheritance chain is the dangerous shape: it drops exactly the
// overrides that distinguish production from the base environment, and then
// resolves and plans successfully.
type Chain struct {
	Name     string
	Selected bool
	Layers   []Layer
}

// Resolve walks name's extends chain.
//
// An empty name is not an error: `infra validate` has no environment argument
// (internal/cli/validate.go) and must still compile the project. It yields a
// Chain with Selected false and no layers, and stage 4 reads Selected to
// decide whether a variable that only an environment sets is an error or an
// unknown.
func Resolve(decls []config.EnvironmentDecl, name string) (Chain, diag.Diagnostics) {
	var ds diag.Diagnostics

	if name == "" {
		return Chain{}, ds
	}

	byName := make(map[string]config.EnvironmentDecl, len(decls))
	for _, d := range decls {
		byName[d.Name] = d
	}

	// A project that has not adopted environments at all still plans: M2 ships
	// `infra plan dev` against a bare infra.yml, and the integration suite
	// depends on it. Once a project DOES declare environments, a name that is
	// not among them is a typo worth reporting.
	if len(byName) == 0 {
		return Chain{
			Name:     name,
			Selected: true,
			Layers:   []Layer{{Name: name, Scope: value.ScopeEnvironmentVar}},
		}, ds
	}

	start, ok := byName[name]
	if !ok {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "unknown environment " + strconv.Quote(name),
			Detail:   "Declared environments:\n  " + strings.Join(declaredNames(decls), "\n  "),
			Action:   "Use one of those, or declare " + strconv.Quote(name) + " under `environments:` or in environments/" + name + ".yml.",
		})
		return Chain{}, ds
	}

	// Walk parent-ward, recording where each name was seen so a repeat yields
	// the exact cycle rather than just its existence.
	var order []config.EnvironmentDecl
	seen := map[string]int{}
	current := start
	for {
		if at, dup := seen[current.Name]; dup {
			ds.Add(cycleDiagnostic(order[at:], current))
			return Chain{}, ds
		}
		seen[current.Name] = len(order)
		order = append(order, current)

		if current.Extends == "" {
			break
		}
		parent, ok := byName[current.Extends]
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "environment " + strconv.Quote(current.Name) + " extends unknown environment " + strconv.Quote(current.Extends),
				Detail:   "Declared environments:\n  " + strings.Join(declaredNames(decls), "\n  "),
				Action:   "Correct `extends`, or declare " + strconv.Quote(current.Extends) + ".",
				Origin:   current.ExtendsOrigin,
			})
			return Chain{}, ds
		}
		current = parent
	}

	// order is leaf-first because the walk goes parent-ward; reverse it once,
	// here, so no consumer has to know that.
	layers := make([]Layer, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		d := order[i]
		scope := value.ScopeEnvironmentInherit
		if i == 0 {
			scope = value.ScopeEnvironmentVar
		}
		layers = append(layers, Layer{Name: d.Name, Overrides: d.Overrides, Scope: scope, Origin: d.Origin})
	}

	return Chain{Name: name, Selected: true, Layers: layers}, ds
}

// cycleDiagnostic renders a cycle in participation order with the wrap
// included — `a -> b -> a` — matching graph.Layers' rendering, so the three
// cycle errors in this system (environment extends, the module graph, the
// resource graph) read alike. Spec §7.4 requires the full cycle, not one
// participant: naming only one leaves the reader to find the rest by hand.
func cycleDiagnostic(cycle []config.EnvironmentDecl, repeat config.EnvironmentDecl) diag.Diagnostic {
	names := make([]string, 0, len(cycle)+1)
	for _, d := range cycle {
		names = append(names, d.Name)
	}
	names = append(names, repeat.Name)

	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "environment inheritance forms a cycle",
		Detail:   strings.Join(names, " -> ") + "\n\nEach environment `extends` the next, so none of them has a base to inherit from.",
		Action:   "Remove one `extends` so the chain ends at an environment that has none.",
		Origin:   cycle[0].ExtendsOrigin,
	}
}

// declaredNames lists environment names for a diagnostic, reading them from
// the declaration SLICE rather than the lookup map. ProjectDecl.Environments
// is already sorted by name (stage 2), so there is nothing to sort; ranging
// the map instead would reorder the message between identical runs and need a
// sort to undo it.
func declaredNames(decls []config.EnvironmentDecl) []string {
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.Name)
	}
	return out
}
