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
// It switches on Kind and Phase together, never Kind alone: a replace is two
// nodes, a destroy and a create, both carrying OpReplace, and only Phase
// tells them apart.
//
// current is the resource as it now stands — what the refresh before
// planning observed, with the host's bookkeeping restamped from the last
// persisted state. currentFor builds it and is the authority on that merge.
// Update and Delete receive current rather than op.Before because Before
// carries no ProviderID, and a provider cannot find the object it manages
// without one. desired is the fully resolved DesiredResource built from
// op.After; dispatch evaluates no expressions itself.
//
// OpForget makes no provider call: dropping a resource from management
// without touching the infrastructure is the entire point of retaining it.
// It is therefore safe to call with a nil provider.
//
// dispatch does not retry. The retry loop wraps calls to dispatch, and a
// second loop here would double the backoff and the attempt count.
//
// No arm of the switch may fall through to a provider call. An OpKind this
// function was never taught about must fail loudly rather than guess which
// call it implies.
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
		return nil, prov.Delete(operationContext(ctx), current)

	default:
		// OpNoOp never reaches dispatch, and every other kind is matched
		// above, so this is a kind the switch was never taught about.
		return nil, fmt.Errorf("%s: dispatch: unhandled operation kind %s phase %d", node.Address, node.Kind, int(node.Phase))
	}
}
