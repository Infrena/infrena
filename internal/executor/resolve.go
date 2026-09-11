package executor

import (
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/expressions"
	"github.com/infrata/infrata/internal/planner"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
)

// runtimeScope builds the apply-time scope: attribute references resolve
// against resources already applied earlier in this run (or present in state
// before it started).
//
// The scope itself is expressions.ResourceScope, shared with the planner,
// rather than a second scope defined here. Both phases must agree exactly on
// which references are resolvable and which stay deferred — a plan that
// resolves a reference the apply does not (or vice versa) is a plan that
// lies, which is how invariant 2 was broken before that type existed. See its
// doc comment for why Variable reports unavailable and why an unknown
// attribute is not a resolvable answer.
//
// resources is a snapshot, not a live *state.State; see Task 8's run.snapshot
// for why a snapshot is what a worker goroutine is handed.
func runtimeScope(resources map[string]*resource.ResourceState) expressions.ResourceScope {
	scope := make(expressions.ResourceScope, len(resources))
	for key, rs := range resources {
		if rs == nil {
			continue
		}
		scope[key] = rs.Attributes
	}
	return scope
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
// A value still unknown after evaluation becomes an error diagnostic, not a
// silently fabricated one: the resource it depends on should already have
// run, by dependency ordering (spec §14), so a value still unknown here
// means either that ordering did not hold, or the dependency's provider
// response never populated the attribute the expression names. Either way
// the operation cannot proceed with a value that was never actually
// determined, so the caller must see this as a failure rather than pass an
// unknown to a provider as though it were real (pkg/resource's
// ResolvedResource.Desired applies the same rule one layer down). That policy
// lives here rather than inside expressions.ResolveDeferred because it is the
// opposite of the planner's: at plan time an unfinished reference is the
// ordinary case of a dependency that does not exist yet.
//
// An unknown carrying no expression at all is dropped from the result rather
// than carried through as Known=false. It is not a deferred reference — it is
// a computed schema attribute the provider assigns itself, which
// afterAttributes (internal/planner/diff.go) stages as unknown purely so
// planner.Render can show "(known after apply)". Evaluating it would produce
// an unknown with no error and then be reported as a dependency-ordering
// failure it never was, turning the plain create of ANY resource with an unset
// computed attribute into an error; ResolveDeferred is what declines to
// evaluate it, and omitting the key here is what lets
// resource.ResolvedResource.Desired() build a DesiredResource without tripping
// its own unknown-attribute refusal on an attribute that was never a provider
// input to begin with.
//
// environment is part of this function's interface contract (Task 7's brief,
// consumed by Task 8) rather than something the scope reads: per the invariant
// documented on expressions.ResourceScope, no well-formed deferred value can
// contain a reference to "environment" (or any other variable) for the scope
// to resolve, so there is currently nothing for this parameter to feed. It is
// kept in the signature rather than dropped because Task 8 calls resolveAfter
// through this exact contract; if that invariant is ever deliberately relaxed,
// this is where an environment value would be threaded back in.
func resolveAfter(op *planner.Operation, resources map[string]*resource.ResourceState, environment string) (map[string]value.Value, diag.Diagnostics) {
	resolved, unresolved, ds := expressions.ResolveDeferred(op.After, runtimeScope(resources))

	out := make(map[string]value.Value, len(resolved))
	for name, v := range resolved {
		if !v.Known && v.Expr == nil {
			continue
		}
		out[name] = v
	}

	// unresolved is already sorted, so two runs of the identical apply cannot
	// report the same failures in a different order.
	for _, name := range unresolved {
		v := op.After[name]
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

	return out, ds
}
