package executor

import (
	"sort"

	"infra/internal/diag"
	"infra/internal/expressions"
	"infra/internal/planner"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

// runtimeScope resolves expression references at apply time: attribute
// references against resources already applied earlier in this run (or
// present in state before it started). It implements expressions.Scope so
// resolution goes through internal/expressions.Evaluate — the exact
// evaluator the compiler used to produce the unknown value in the first
// place (spec §6, §15) — rather than a second, hand-rolled evaluator that
// could disagree with it about what a function or a concatenation means.
//
// Variable always reports unavailable, deliberately. A well-formed residual
// (internal/expressions' residual, Task 3) can never contain an OpVarRef at
// all: every variable name the compiler's compileScope can resolve —
// "environment" unconditionally, plus --var/region/account
// (internal/compiler/bind.go, variableScope) — evaluates successfully in
// the very first, compile-time pass, and residual() folds any argument
// whose OWN sub-evaluation succeeded into an OpLiteral before the
// expression is ever stored as a deferred value (eval.go's residual, on
// the rule stated in its own comment: fold what resolved, keep the
// sub-expression's own residual for what did not). A variable that does
// NOT resolve at compile time is a hard compile error (bindAttribute adds
// an "undefined variable" diagnostic), which stops the pipeline before a
// Plan — and so before an Operation — is ever produced. Either way, no
// OpVarRef survives into an Operation.After that reaches an apply run.
//
// So an OpVarRef appearing in a residual here would mean that invariant
// broke somewhere upstream — a compiler bug, not a normal unresolved
// dependency. Reporting it "unavailable" rather than defensively answering
// for it (as an earlier version of this scope did, incorrectly assuming a
// residual could still carry a resolved-at-compile-time variable
// reference — see TestResolveAfterTreatsResidualVarRefAsCompilerBug) makes
// that bug surface loudly: expressions.Evaluate reports it as an undefined
// variable, an error diagnostic ends up in the returned Diagnostics, and
// the attribute stays unknown rather than being silently resolved. That is
// preferable to a scope that quietly covers for a broken invariant.
//
// resources is a snapshot, not a live *state.State; see Task 8's
// run.snapshot for why a snapshot is what a worker goroutine is handed.
type runtimeScope struct {
	resources map[string]*resource.ResourceState
}

// Variable always reports unavailable — see the type doc comment for why
// that is correct rather than merely unimplemented.
func (s runtimeScope) Variable(string) (value.Value, bool) { return value.Value{}, false }

// Attribute resolves a resource reference against the apply-time snapshot.
// A resource missing from the snapshot — not yet applied, or never going to
// be — reports unavailable rather than panicking or fabricating a value;
// expressions.Evaluate turns that into an unknown, and resolveAfter turns a
// still-unknown result into an error diagnostic rather than passing it on
// silently.
func (s runtimeScope) Attribute(ref value.Reference) (value.Value, bool) {
	rs, ok := s.resources[(address.Address{Name: ref.Resource}).String()]
	if !ok {
		return value.Value{}, false
	}
	v, ok := rs.Attributes[ref.Attribute]
	return v, ok
}

// resolveAfter finishes every deferred expression in an operation's After
// attributes against resources already applied earlier in this run.
//
// It reads only op.After, never op.Before: Before is the plan-time snapshot
// the diff was computed against (see
// TestResolveAfterUsesLiveSnapshotNotPlanTimeBefore), and the resolved value
// must come from what the dependency actually turned out to be — resources,
// carrying each dependency's real, post-apply attributes (Source:
// SourceProvider) — not from anything the plan recorded.
//
// It builds and returns a fresh map rather than writing into op.After in
// place: op is the Operation from the Plan the user approved, and mutating
// it would mean the approved plan artifact silently changed shape while the
// apply that is supposed to be reconciling reality with IT is still running.
//
// Already-Known attributes pass through untouched with no call into the
// evaluator at all — an operation whose After holds no unknowns (a plain
// destroy, whose After is unset entirely; a create or update with no
// cross-resource reference) does no evaluation work and produces no
// diagnostics.
//
// A value still unknown after evaluation becomes an error diagnostic, not a
// silently fabricated one: the resource it depends on should already have
// run, by dependency ordering (spec §14), so a value still unknown here
// means either that ordering did not hold, or the dependency's provider
// response never populated the attribute the expression names. Either way
// the operation cannot proceed with a value that was never actually
// determined, so the caller must see this as a failure rather than pass an
// unknown to a provider as though it were real (pkg/resource's
// ResolvedResource.Desired applies the same rule one layer down).
//
// environment is part of this function's interface contract (Task 7's
// brief, consumed by Task 8) rather than something runtimeScope reads: per
// the invariant documented on runtimeScope's Variable, no well-formed
// residual can contain a reference to "environment" (or any other
// variable) for runtimeScope to resolve, so there is currently nothing for
// this parameter to feed. It is kept in the signature rather than dropped
// because Task 8 calls resolveAfter through this exact contract; if that
// invariant is ever deliberately relaxed, this is where an environment
// value would be threaded back in.
func resolveAfter(op *planner.Operation, resources map[string]*resource.ResourceState, environment string) (map[string]value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	scope := runtimeScope{resources: resources}

	// Sorted rather than ranged directly: diagnostic order must not depend
	// on Go's randomised map iteration, or two runs of the identical apply
	// could report the same failures in a different order.
	names := make([]string, 0, len(op.After))
	for name := range op.After {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]value.Value, len(op.After))
	for _, name := range names {
		v := op.After[name]
		if v.Known {
			out[name] = v
			continue
		}

		resolved, evalDS := expressions.Evaluate(v.Expr, scope)
		ds.Extend(evalDS)
		if !resolved.Known && !evalDS.HasErrors() {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  op.Address.String() + ": " + name + " is still unknown after its dependencies were applied",
				Detail: "Expected " + name + " (" + v.Expr.String() + ") to resolve once every resource it " +
					"references had been applied, but it did not. Either the resource it references was not " +
					"applied before this operation ran — a bug in dependency ordering (spec §14) — or the " +
					"provider that created it did not report the attribute the expression names.",
				Action:  "Check that every resource " + name + " refers to is declared as a dependency of " + op.Address.String() + " and that the provider populates the referenced attribute.",
				Origin:  v.Origin,
				Related: []address.Address{op.Address},
			})
		}
		out[name] = resolved
	}
	return out, ds
}
