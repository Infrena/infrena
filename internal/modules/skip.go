package modules

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/expressions"
	"github.com/infrata/infrata/pkg/value"
)

// Env is the environment stage 5 is expanding for, and every environment the
// configuration declares.
//
// Declared is carried so `skip`/`only` can refuse a name nothing declares
// (PLAN.md §6.2). It is empty for a project that declares no environments at
// all, in which case no name can be checked and none is refused — the same
// concession environments.Resolve has made since M2.
type Env struct {
	Name     string
	Declared []string
}

// excluded reports whether r is left out of this environment, and the origin of
// the key that says so.
//
// Evaluated in the scope it is HANDED, which is what makes §6.2's two positions
// work: a module call's is evaluated in the caller's scope before the module is
// entered, and a resource inside a module in the module's own scope, where its
// inputs are visible.
func (w *walker) excluded(r *config.ResourceDecl, scope *Scope, env Env) (bool, value.Origin) {
	decl, isOnly := r.Only, true
	if decl.Name == "" {
		decl, isOnly = r.Skip, false
	}
	if decl.Name == "" {
		return false, value.Origin{}
	}

	names, ok := w.environmentNames(decl, scope, r)
	if !ok {
		return false, value.Origin{}
	}
	w.checkDeclared(names, decl, env)

	listed := slices.Contains(names, env.Name)
	// `only` keeps what is listed; `skip` drops it. Written as one comparison
	// rather than two branches so the two keys cannot drift apart.
	if isOnly {
		return !listed, decl.Origin
	}
	return listed, decl.Origin
}

// environmentNames resolves a `skip`/`only` value to the names it lists.
//
// A scalar and a list mean the same shape of thing (§6.2), and either may be an
// expression. An expression that cannot be resolved to known text is an error
// rather than an empty list: an unresolvable filter that quietly matched
// nothing would silently include every resource in every environment, which is
// the direction that deploys things rather than the direction that omits them.
func (w *walker) environmentNames(decl config.AttributeDecl, scope *Scope, r *config.ResourceDecl) ([]string, bool) {
	v := decl.Value
	if decl.HasExpressions {
		text, ok := v.AsString()
		if !ok {
			w.ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`" + decl.Name + "` on resource " + strconv.Quote(r.Name) + " is not resolvable",
				Detail:   "An expression here must appear on its own, as the whole value.",
				Action:   "Write `" + decl.Name + ": ${names}` or a literal list of environment names.",
				Origin:   decl.Origin,
			})
			return nil, false
		}
		e, parseDiags := expressions.Parse(text, decl.Origin)
		w.ds.Extend(parseDiags)
		if e == nil {
			return nil, false
		}
		evaluated, evalDiags := expressions.Evaluate(e, scope)
		w.ds.Extend(evalDiags)
		v = evaluated
	}

	if !v.Known {
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary: "`" + decl.Name + "` on resource " + strconv.Quote(r.Name) +
				" did not resolve to an environment name",
			Detail: "It decides whether this resource exists at all, so it must be known before " +
				"anything is planned — it cannot depend on another resource's attribute.",
			Action: "Use a variable or a module input, or write the environment names literally.",
			Origin: decl.Origin,
		})
		return nil, false
	}

	switch v.Kind {
	case value.KindString:
		s, _ := v.AsString()
		if strings.TrimSpace(s) == "" {
			return nil, true // an empty name lists nothing
		}
		return []string{s}, true
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return nil, true
		}
		out := make([]string, 0, len(items))
		for i, item := range items {
			s, ok := item.AsString()
			if !ok || !item.Known {
				w.ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary: "`" + decl.Name + "`[" + strconv.Itoa(i) + "] on resource " +
						strconv.Quote(r.Name) + " is not an environment name",
					Action: "Every entry must be the name of a declared environment.",
					Origin: decl.Origin,
				})
				continue
			}
			out = append(out, s)
		}
		return out, true
	default:
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary: "`" + decl.Name + "` on resource " + strconv.Quote(r.Name) +
				" must be an environment name or a list of them",
			Detail: "It is " + article(v.Kind) + " " + v.Kind.String() + ".",
			Action: "Write `" + decl.Name + ": production` or `" + decl.Name + ": [staging, production]`.",
			Origin: decl.Origin,
		})
		return nil, false
	}
}

// checkDeclared refuses an environment nothing declares.
//
// A filter that quietly matches nothing is worse than no filter: `skip: [prod]`
// against an environment called `production` would otherwise do nothing, in
// every environment, forever, with no output of any kind to notice.
func (w *walker) checkDeclared(names []string, decl config.AttributeDecl, env Env) {
	if len(env.Declared) == 0 {
		// A project that declares no environments has nothing to check against.
		return
	}
	declared := make(map[string]bool, len(env.Declared))
	for _, d := range env.Declared {
		declared[d] = true
	}
	var unknown []string
	for _, n := range names {
		if !declared[n] {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) == 0 {
		return
	}
	sort.Strings(unknown)
	w.ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary: "`" + decl.Name + "` names " + plural(len(unknown), "an environment", "environments") +
			" that does not exist: " + strings.Join(quoteAll(unknown), ", "),
		Detail: "Declared environments: " + strings.Join(env.Declared, ", ") + ".\n" +
			"A filter naming an environment that does not exist matches nothing, in every " +
			"environment, with no other sign that it is wrong.",
		Action: "Correct the spelling, or declare it under `environments:`.",
		Origin: decl.Origin,
	})
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strconv.Quote(s)
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func article(k value.Kind) string {
	switch k.String() {
	case "integer":
		return "an"
	default:
		return "a"
	}
}
