// Package environments walks an environment's `extends` chain and produces the
// ordered scope stack the variable resolver consumes.
package environments

import (
	"slices"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// Layer is one environment in a resolved extends chain.
//
// Overrides stay a slice, passed through untouched: they arrive already sorted
// by name with an Origin each, and a map would throw away the line number a
// diagnostic needs.
type Layer struct {
	Name      string
	Overrides []config.OverrideDecl
	Scope     value.Scope
	Origin    value.Origin
}

// Chain is the ordered scope stack: ancestors first, the environment named on
// the command line last. Later layers win, the same direction the rest of the
// precedence ladder runs in, so the variable resolver applies it with one
// forward loop and no reversal anywhere.
//
// A Chain returned alongside errors is empty rather than partial. Half an
// inheritance chain is the dangerous shape: it drops exactly the overrides that
// distinguish production from the base environment, and then resolves and plans
// successfully.
type Chain struct {
	Name string
	// Selected is false when no environment was named, which is legal.
	Selected bool
	Layers   []Layer

	// RequireApproval and PreventDestroy are resolved down the chain: the
	// nearest declaration wins, so an environment inherits its parent's
	// protection by saying nothing and drops it by writing `false`. Inheriting
	// is the safe direction — a child that gives up a guard says so in its own
	// file, where a reviewer reads it.
	//
	// Resolved here rather than by each caller (apply, destroy, the planner),
	// because a protection computed three times can disagree with itself.
	RequireApproval bool
	PreventDestroy  bool
	// RequireApprovalFrom and PreventDestroyFrom name the environment each
	// protection was declared on, which is not always Name: a diagnostic saying
	// "production prevents this" when the user ran `apply prod-eu` would send
	// them to the wrong file.
	RequireApprovalFrom string
	PreventDestroyFrom  string
}

// Resolve walks name's extends chain.
//
// An empty name is not an error: `infrena validate` takes no environment and
// must still compile the project. It yields a Chain with Selected false and no
// layers, and the variable resolver reads Selected to decide whether a variable
// that only an environment sets is an error or an unknown.
func Resolve(decls []config.EnvironmentDecl, name string) (Chain, diag.Diagnostics) {
	var ds diag.Diagnostics

	if name == "" {
		return Chain{}, ds
	}

	byName := make(map[string]config.EnvironmentDecl, len(decls))
	for _, d := range decls {
		byName[d.Name] = d
	}

	// A project that has not adopted environments at all still plans:
	// `infrena plan dev` works against a bare infrena.yml. Once a project does
	// declare environments, a name that is not among them is a typo worth
	// reporting.
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
	for i, d := range slices.Backward(order) {
		scope := value.ScopeEnvironmentInherit
		if i == 0 {
			scope = value.ScopeEnvironmentVar
		}
		layers = append(layers, Layer{Name: d.Name, Overrides: d.Overrides, Scope: scope, Origin: d.Origin})
	}

	chain := Chain{Name: name, Selected: true, Layers: layers}
	// order is leaf-first, so the first declaration met walking it is the
	// nearest one and wins.
	for _, d := range order {
		if d.RequireApprovalSet && chain.RequireApprovalFrom == "" {
			chain.RequireApproval, chain.RequireApprovalFrom = d.RequireApproval, d.Name
		}
		if d.PreventDestroySet && chain.PreventDestroyFrom == "" {
			chain.PreventDestroy, chain.PreventDestroyFrom = d.PreventDestroy, d.Name
		}
	}
	return chain, ds
}

// cycleDiagnostic renders a cycle in participation order with the wrap
// included — `a -> b -> a` — matching how the module and resource graphs render
// theirs, so all three cycle errors read alike. The full cycle, not one
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

// declaredNames lists environment names for a diagnostic, reading the
// declaration slice rather than the lookup map: the slice arrives sorted by
// name, while ranging the map would reorder the message between identical runs.
func declaredNames(decls []config.EnvironmentDecl) []string {
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.Name)
	}
	return out
}
