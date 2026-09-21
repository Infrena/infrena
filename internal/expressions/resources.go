package expressions

import (
	"sort"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// ResourceScope resolves attribute references against resources whose attributes
// are already known, keyed by canonical address.
//
// One type serves both later phases: the planner resolves a reference against
// what the plan says the resource will look like after apply, and the executor
// resolves the same reference against what it actually turned out to be. A second
// resolver would be free to disagree about what counts as resolvable, and a plan
// that resolves a reference differently from the apply carrying it out is a plan
// that lies.
type ResourceScope map[string]map[string]value.Value

// Variable always reports unavailable, and that is correct rather than merely
// unimplemented.
//
// No variable reference can survive into a value this scope is asked to finish:
// one that resolves does so in the compile-time pass and is folded into a
// literal, and one that does not is a hard compile error that stops the pipeline
// before a plan exists. So a variable reference arriving here means a compiler
// bug, and reporting it unavailable makes that surface as a diagnostic rather
// than be quietly answered for.
func (s ResourceScope) Variable(string) (value.Value, bool) { return value.Value{}, false }

// Attribute resolves a resource reference against the snapshot.
//
// A resource missing from the snapshot — not yet applied, never going to be, or
// not in this configuration at all — reports unavailable rather than fabricating
// a value.
//
// An attribute that is present but not known reports unavailable too. The
// planner's snapshot holds each operation's After map, which deliberately carries
// a computed attribute the provider has yet to assign as an unknown so the plan
// can print "(known after apply)". Handing that unknown back as a resolved answer
// would substitute it for the referring expression's own unknown, and with it
// that expression — leaving a nil-Expr unknown that apply can no longer finish,
// so the resource would be created with the attribute silently dropped.
func (s ResourceScope) Attribute(ref value.Reference) (value.Value, bool) {
	// Keyed by the reference's own canonical address, so a resource inside a
	// module and a same-named resource at the root are two different keys.
	attrs, ok := s[ref.Target.String()]
	if !ok {
		return value.Value{}, false
	}
	v, ok := attrs[ref.Attribute]
	if !ok || !v.Known {
		return value.Value{}, false
	}
	return v, ok
}

// ResolveDeferred finishes every deferred expression in an attribute map against
// scope, returning a new map, the names that could not be finished, and any
// diagnostics evaluation produced.
//
// Three kinds of entry are treated differently, and confusing any two of them
// reintroduces a bug this function exists to prevent:
//
//   - A known value passes through untouched, with no call into the evaluator at
//     all, so a map holding no unknowns produces no diagnostics.
//
//   - An unknown with a nil Expr is not a deferred reference and passes through
//     unchanged. It is a computed schema attribute the provider assigns itself,
//     staged as unknown so a plan can display it. Evaluating it would yield
//     another unknown and report it unresolved, turning the plain create of any
//     resource with an unset computed attribute into a dependency-ordering
//     failure it never was.
//
//   - An unknown carrying an Expr is re-evaluated.
//
// A re-evaluated value that is still unknown is left exactly as it was — the
// original unknown with its original expression, not the evaluator's residual. So
// resolution only ever replaces an unknown with a fully known value, which is
// what makes it safe to run at plan time on a configuration that has never been
// applied, and keeps every unresolved value carrying an expression the next phase
// can retry rather than one folded against a different snapshot.
//
// unresolved names the attributes that carried an expression, were evaluated, and
// are still unknown without evaluation itself erroring. It is sorted, and so is
// the order attributes are evaluated in, because Go randomises map iteration and
// two runs of identical input must produce identical diagnostics. Whether being
// unresolved is an error is the caller's policy: at plan time it is the ordinary
// case of a dependency that does not exist yet, and at apply time it is a
// failure.
func ResolveDeferred(attrs map[string]value.Value, scope Scope) (map[string]value.Value, []string, diag.Diagnostics) {
	var ds diag.Diagnostics
	var unresolved []string

	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]value.Value, len(attrs))
	for _, name := range names {
		v := attrs[name]

		// A composite carries no expression of its own — its leaves do — so it
		// must be walked rather than evaluated. Without this branch the map falls
		// into the pass-through below and its unresolved leaves reach the state
		// file, which cannot encode them.
		if v.Kind == value.KindList || v.Kind == value.KindMap {
			if !HasUnknownLeaf(v) {
				out[name] = v
				continue
			}
			// This attribute's errors, not the accumulated set: checking
			// ds.HasErrors() would let an earlier attribute's failure suppress
			// this one's "still unknown" report.
			var leafErrors diag.Diagnostics
			walked := WalkDeferred(v, func(leaf value.Value) value.Value {
				resolved, evalDS := Evaluate(leaf.Expr, scope)
				leafErrors.Extend(evalDS)
				if !resolved.Known {
					return leaf
				}
				return resolved
			})
			ds.Extend(leafErrors)
			if HasUnknownLeaf(walked) {
				out[name] = v
				if !leafErrors.HasErrors() {
					unresolved = append(unresolved, name)
				}
				continue
			}
			// Every leaf resolved, so the composite is whole again.
			walked.Known = true
			out[name] = walked
			continue
		}

		if v.Known || v.Expr == nil {
			out[name] = v
			continue
		}

		resolved, evalDS := Evaluate(v.Expr, scope)
		ds.Extend(evalDS)
		if !resolved.Known {
			out[name] = v
			if !evalDS.HasErrors() {
				unresolved = append(unresolved, name)
			}
			continue
		}
		out[name] = resolved
	}
	return out, unresolved, ds
}
