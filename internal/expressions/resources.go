package expressions

import (
	"sort"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// ResourceScope resolves attribute references against resources whose
// attributes are already known, keyed by canonical address. It is the one
// scope both later phases use: the planner resolves a reference against what
// the plan says the referenced resource will look like after apply, and the
// executor resolves the same reference against what the resource actually
// turned out to be once applied.
//
// One type rather than two exists deliberately. A second, hand-rolled
// resolver in the other phase would be free to disagree with this one about
// what counts as resolvable, and a plan that resolves a reference differently
// from the apply that carries it out is a plan that lies — which is the whole
// failure this closes (invariant 2: desired == actual must produce zero
// operations, forever, not just on the first run).
//
// Variable always reports unavailable, deliberately. A well-formed unknown
// reaching either phase can never depend on an OpVarRef: every variable name
// the compiler can resolve at compile time — everything stage 4
// (internal/variables) resolved, plus "environment" unconditionally and
// --var-supplied region/account when present (internal/compiler/compile.go,
// seedProcessVariables) — evaluates successfully in the very first,
// compile-time pass, and residual()
// folds any argument whose OWN sub-evaluation succeeded into an OpLiteral
// before the expression is ever stored as a deferred value. A variable that
// does NOT resolve at compile time is a hard compile error (bindAttribute adds
// an "undefined variable" diagnostic), which stops the pipeline before a Plan
// — and so before an Operation — is ever produced. Either way, no OpVarRef
// survives into a value this scope is asked to finish.
//
// So an OpVarRef appearing here would mean that invariant broke somewhere
// upstream — a compiler bug, not a normal unresolved dependency. Reporting it
// "unavailable" rather than defensively answering for it makes that bug
// surface loudly: Evaluate reports it as an undefined variable and an error
// diagnostic reaches the caller, rather than the attribute being silently
// resolved. That is preferable to a scope that quietly covers for a broken
// invariant.
type ResourceScope map[string]map[string]value.Value

// Variable always reports unavailable — see the type doc comment for why that
// is correct rather than merely unimplemented.
func (s ResourceScope) Variable(string) (value.Value, bool) { return value.Value{}, false }

// Attribute resolves a resource reference against the snapshot.
//
// A resource missing from the snapshot — not yet applied, never going to be,
// or not in this configuration at all — reports unavailable rather than
// panicking or fabricating a value.
//
// An attribute that is present but NOT known reports unavailable too, which
// is the difference that matters at plan time. The planner's snapshot holds
// each operation's After map, and After deliberately carries a computed
// attribute the provider has yet to assign (fake.network's "id" on a create)
// as an unknown, purely so the plan can print "(known after apply)". Handing
// that unknown back as a resolved answer would substitute it for the
// referring expression's own unknown and, with it, that expression — leaving
// a nil-Expr unknown in the plan's After that apply can no longer finish, so
// the resource would be created with the attribute silently dropped. Refusing
// it instead leaves the reference deferred, carrying the expression that will
// reproduce it, which is exactly what a not-yet-created dependency should
// look like.
func (s ResourceScope) Attribute(ref value.Reference) (value.Value, bool) {
	// Keyed by the reference's own canonical address, so a resource inside a
	// module and a same-named resource at the root are two different keys.
	// Before M5 this constructed a root address from a bare name, which was
	// correct only because nothing could produce a non-empty module path yet;
	// value.Reference now carries one (contract Ruling 1).
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

// ResolveDeferred finishes every deferred expression in an attribute map
// against scope, returning a new map, the names that could not be finished,
// and any diagnostics evaluation produced.
//
// Three kinds of entry are treated differently, and confusing any two of them
// reintroduces a bug this function exists to prevent:
//
//   - A known value passes through untouched, with no call into the evaluator
//     at all. An attribute map holding no unknowns does no work and produces
//     no diagnostics.
//
//   - An unknown with a nil Expr is NOT a deferred reference and is passed
//     through unchanged. Value.Expr's own doc comment (pkg/value) says Expr
//     "is set when Known is false because the value depends on a resource that
//     does not exist yet"; a nil Expr here is a computed schema attribute the
//     provider assigns itself and the caller never supplies, staged as unknown
//     so a plan can display it. Evaluating it would call Evaluate(nil, scope),
//     which returns an unknown with no error, and it would then be reported as
//     unresolved — turning the plain create of ANY resource with an unset
//     computed attribute into a dependency-ordering failure it never was.
//
//   - An unknown carrying an Expr is re-evaluated.
//
// A re-evaluated value that is STILL unknown is left exactly as it was: the
// original unknown, with its original expression, not the evaluator's residual.
// Resolution therefore only ever replaces an unknown with a fully known value
// and otherwise changes nothing at all. That is what makes it safe to run at
// plan time on a configuration that has never been applied — an unapplied plan
// is byte-for-byte what it was before this function existed — and it keeps
// every unresolved value carrying an expression the next phase can retry,
// rather than a residual that may have been folded against a different
// snapshot than the one that will finish it.
//
// unresolved names the attributes that carried an expression, were evaluated,
// and are still unknown without evaluation itself erroring. It is sorted, and
// so is the order attributes are evaluated in, because Go's map iteration is
// randomised and two runs of the identical input must produce identical
// diagnostics (invariant 6). Whether being unresolved is an error is the
// caller's policy, not this function's: at plan time it is the ordinary case
// of a dependency that does not exist yet, and at apply time it is a failure.
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

		// A COMPOSITE carries no expression of its own — its leaves do — so it
		// must be walked rather than evaluated. Without this branch the map falls
		// into the pass-through below and its unresolved leaves reach the state
		// file, which cannot encode them.
		if v.Kind == value.KindList || v.Kind == value.KindMap {
			if !HasUnknownLeaf(v) {
				out[name] = v
				continue
			}
			// leafErrors is THIS attribute's, not the accumulated set. Checking
			// ds.HasErrors() would mean an earlier attribute's failure silently
			// suppressed this one's "still unknown" report — the same
			// accept-and-say-nothing shape the scalar branch below avoids by
			// checking its own evalDS.
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
