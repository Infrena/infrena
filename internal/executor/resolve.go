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
// present in state before it started), and the one variable the compiler
// always injects regardless of --var. It implements expressions.Scope so
// resolution goes through internal/expressions.Evaluate — the exact
// evaluator the compiler used to produce the unknown value in the first
// place (spec §6, §15) — rather than a second, hand-rolled evaluator that
// could disagree with it about what a function or a concatenation means.
//
// Variable only ever resolves "environment" because Task 3 folds every
// resolvable part into the deferred expression before it is stored. A
// residual therefore holds resource references and literals — never a
// variable — so the apply-time scope needs no variable table. environment
// is kept anyway because a residual that concatenates an already-resolved
// variable with a still-unresolved resource reference (see
// TestResolveAfterMixesVariableAndResourceReference) still contains the
// original OpVarRef node for that variable: Task 3 only folds an argument
// once ITS OWN sub-evaluation succeeds, and evaluation of the whole
// expression only ran once, at compile time, before the resource reference
// was known — so the compile-time result for "environment" was never
// substituted into the stored residual either. Re-evaluating the residual
// at apply time therefore visits that OpVarRef node again and needs an
// answer for it.
//
// resources is a snapshot, not a live *state.State; see Task 8's
// run.snapshot for why a snapshot is what a worker goroutine is handed.
type runtimeScope struct {
	resources   map[string]*resource.ResourceState
	environment string
}

// Variable resolves the one variable a residual can still contain.
func (s runtimeScope) Variable(name string) (value.Value, bool) {
	if name == "environment" {
		return value.String(s.environment, value.SourceEnvironment), true
	}
	return value.Value{}, false
}

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
func resolveAfter(op *planner.Operation, resources map[string]*resource.ResourceState, environment string) (map[string]value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	scope := runtimeScope{resources: resources, environment: environment}

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
