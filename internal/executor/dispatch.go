package executor

import (
	"context"
	"fmt"

	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
)

// dispatch performs the one provider call an OpNode implies and returns the
// resulting resource state.
//
// It switches on (node.Kind, node.Phase) together, never node.Kind alone:
// OpNode.ID() is not injective over OpKind (a replace is two nodes,
// destroy:<addr> then create:<addr>, both carrying Kind == OpReplace), and
// neither is Kind by itself — Phase is what tells the two apart.
//
// current is the resource as it now stands: the attributes the refresh
// immediately before planning OBSERVED, carrying the host's own bookkeeping
// — the provider instance, Dependencies, Lifecycle, the timestamps — from
// the last state persisted. executor.currentFor (observed.go) builds it, and
// its doc comment is the authority on the merge. It falls back to the last
// persisted state alone when nothing was observed for the address, and is
// nil when there is no record at all (a plain create).
//
// This comment used to say current was "the live resource.ResourceState",
// which was false: the executor handed over the state the previous apply had
// written, refreshed observations reaching the planner and no further. A
// plugin that read this and diffed current against desired to build a patch
// — which is what the sentence invites, and what pkg/provider.Provider.Read
// invites in as many words — therefore skipped precisely the drift the plan
// had proposed to correct. The comment was half the bug: the code misled,
// and the documentation confirmed the misreading.
//
// It is current, not op.Before, that Update and Delete receive, because
// Before is a bare map[string]value.Value with no ProviderID, and a provider
// cannot find the object it manages without one. desired is the
// fully-resolved DesiredResource Task 7 built from op.After; dispatch does
// not evaluate expressions itself.
//
// OpForget makes no provider call at all: dropping a resource from
// management without touching the real infrastructure is retain's entire
// point (spec §11, invariant 1). It returns (nil, nil) without touching
// prov, current or desired — safe to call with prov == nil for exactly this
// reason.
//
// dispatch does not retry: Attempt (retry.go) owns the retry loop, and
// Task 8's worker pool is what calls dispatch through it. Wrapping this call
// in a second retry loop here would double the backoff and double-count
// attempts against Attempt's own policy.
//
// The switch has no default arm that reaches a provider call. Every case
// that calls Create, Update or Delete is spelled out explicitly by
// (Kind, Phase); the fallback below only ever returns an error naming the
// unhandled pair, so a future OpKind landing here unhandled fails loudly
// instead of silently reaching a provider call it was never vetted for —
// the same failure shape this project has repeatedly shipped as a
// permissive default arm.
func dispatch(
	ctx context.Context,
	prov provider.Provider,
	node planner.OpNode,
	current *resource.ResourceState,
	desired *resource.DesiredResource,
) (*resource.ResourceState, error) {
	switch {
	case node.Kind == planner.OpForget:
		return nil, nil

	case node.Kind == planner.OpCreate,
		node.Kind == planner.OpReplace && node.Phase == planner.PhaseCreate:
		if prov == nil {
			return nil, fmt.Errorf("%s: dispatch: no provider available for create", node.Address)
		}
		if desired == nil {
			return nil, fmt.Errorf("%s: dispatch: create requires a resolved desired resource", node.Address)
		}
		// operationContext detaches this call from Apply's own cancellation: a
		// SIGINT must finish an in-flight operation, not abort it (spec §15; see
		// executor/context.go).
		return prov.Create(operationContext(ctx), desired)

	case node.Kind == planner.OpUpdate:
		if prov == nil {
			return nil, fmt.Errorf("%s: dispatch: no provider available for update", node.Address)
		}
		if current == nil {
			return nil, fmt.Errorf("%s: dispatch: update requires the resource's current state", node.Address)
		}
		if desired == nil {
			return nil, fmt.Errorf("%s: dispatch: update requires a resolved desired resource", node.Address)
		}
		// operationContext detaches this call from Apply's own cancellation: a
		// SIGINT must finish an in-flight operation, not abort it (spec §15; see
		// executor/context.go).
		return prov.Update(operationContext(ctx), current, desired)

	case node.Kind == planner.OpDestroy,
		node.Kind == planner.OpDestroyDeposed,
		node.Kind == planner.OpReplace && node.Phase == planner.PhaseDestroy:
		if prov == nil {
			return nil, fmt.Errorf("%s: dispatch: no provider available for destroy", node.Address)
		}
		if current == nil {
			return nil, fmt.Errorf("%s: dispatch: destroy requires the resource's current state", node.Address)
		}
		// operationContext detaches this call from Apply's own cancellation: a
		// SIGINT must finish an in-flight operation, not abort it (spec §15; see
		// executor/context.go).
		return nil, prov.Delete(operationContext(ctx), current)

	default:
		// Reachable only by an OpKind this switch was never taught about —
		// OpNoOp never reaches dispatch (BuildExecution skips it entirely),
		// and every other kind is matched above. No arm here may fall
		// through to prov.Create/Update/Delete: an unrecognised kind must
		// fail, not guess which provider call it implies.
		return nil, fmt.Errorf("%s: dispatch: unhandled operation kind %s phase %d", node.Address, node.Kind, int(node.Phase))
	}
}
