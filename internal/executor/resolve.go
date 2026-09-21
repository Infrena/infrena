package executor

import (
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/expressions"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// runtimeScope builds the apply-time scope: attribute references resolve
// against resources already applied earlier in this run (or present in state
// before it started).
//
// The scope type is shared with the planner rather than redefined here:
// both phases must agree exactly on which references resolve and which stay
// deferred, or the plan lies about what the apply will do.
//
// resources is a snapshot, not a live *state.State, because a worker
// goroutine must not read state another worker is writing.
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
// It reads only op.After, never op.Before. Before is the plan-time snapshot
// the diff was computed against; the resolved value must come from what the
// dependency actually turned out to be.
//
// It returns a fresh map rather than writing into op.After, so the plan the
// user approved does not change shape underneath the apply reconciling
// against it.
//
// A value still unknown after evaluation is an error, never a fabricated
// one. Dependency ordering should already have applied the resource it
// refers to, so an unknown here means either that ordering did not hold or
// the provider never populated the attribute named. Neither may be passed to
// a provider as though it were real. The rule is the opposite of the
// planner's, where an unfinished reference is the ordinary case, which is
// why it lives here rather than in the expression evaluator.
//
// An unknown carrying no expression is dropped from the result instead. It
// is not a deferred reference but a computed attribute the provider assigns
// itself, staged as unknown only so the plan can render "known after apply".
// Reporting it would turn the plain create of any resource with an unset
// computed attribute into a dependency-ordering failure it never was.
//
// environment is part of the contract callers use rather than something the
// scope reads: no well-formed deferred value can reference a variable, so
// there is nothing here for it to feed today. It is the place an environment
// value would be threaded back in if that ever changes.
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
				"applied before this operation ran — a bug in dependency ordering — or the " +
				"provider that created it did not report the attribute the expression names.",
			Action:  "Check that every resource " + name + " refers to is declared as a dependency of " + op.Address.String() + " and that the provider populates the referenced attribute.",
			Origin:  v.Origin,
			Related: []address.Address{op.Address},
		})
	}

	return out, ds
}
