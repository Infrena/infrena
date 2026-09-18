// Package resource defines the resource representations shared by the engine
// and providers.
//
// The distinction between ResolvedResource and DesiredResource is load-bearing:
// the planner works with ResolvedResource, where unknown values are permitted;
// providers only ever receive DesiredResource, where they are not. Spec §5.3.
package resource

import (
	"fmt"
	"sort"
	"time"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

// Lifecycle defines immutability constraints on a resource.
//
// The JSON tags are part of the state file's versioned on-disk contract: they
// are named explicitly so that renaming a Go field stays an ordinary refactor
// rather than a silent format change with no version bump.
type Lifecycle struct {
	PreventDestroy bool `json:"prevent_destroy"`
	// PreventReplace refuses a plan that would REPLACE this resource: destroy
	// it and create a new one in its place (PLAN.md §38.1).
	//
	// Separate from PreventDestroy because they guard different mistakes, and
	// neither implies the other. PreventDestroy is about a resource LEAVING
	// configuration — somebody deleted the block. PreventReplace is about a
	// resource STAYING in configuration while an attribute the provider marks
	// ForceNew changes underneath it, which is the more insidious of the two:
	// the configuration still names the resource, the diff looks like an edit,
	// and the data is gone all the same.
	//
	// It is deliberately opt-in per resource rather than an environment-wide
	// setting. An environment that refused every replacement could never
	// change an immutable attribute on anything, which is a far larger
	// restriction than the one people actually want — and the resources this
	// matters for are specific and few: the database, the volume, the thing
	// whose contents do not survive being recreated.
	PreventReplace bool `json:"prevent_replace,omitempty"`
	Retain         bool `json:"retain"`

	// IgnoreChanges names attributes whose drift the planner does not propose to
	// revert (PLAN.md §14.2): the real resource keeps whatever it has, and the plan
	// says so rather than staying silent.
	//
	// The case it exists for: something outside infrena owns one attribute. A CI
	// pipeline sets an ECS service's task revision on every deploy, so a plan computed
	// from configuration would revert it on the next apply and undo the deployment.
	//
	// CANONICAL names by the time it is here — stage 7 resolves whatever spelling the
	// user wrote, so `ignore_changes: [taskRevision]` and `[task_revision]` both arrive
	// as the attribute the plugin declared.
	//
	// Sorted, so a plan artifact is byte-stable however the list was written
	// (invariant 6).
	IgnoreChanges []string `json:"ignore_changes,omitempty"`
}

// ResolvedResource is a resource after the planner has resolved it.
type ResolvedResource struct {
	Address address.Address
	Type    string
	// Provider is the provider INSTANCE this resource belongs to (PLAN.md §12.1)
	// — a name, never a plugin. Resolved from the resource's own `provider:`, or
	// the module call it came from, or the default instance.
	//
	// It reaches ResourceState so that a DESTROY, which has no configuration at
	// all, still knows which account to delete from.
	Provider  string
	Attrs     map[string]value.Value
	DependsOn []address.Address
	Lifecycle Lifecycle
	Origin    value.Origin
}

// DesiredResource is what the executor hands a provider. Every attribute is
// known.
type DesiredResource struct {
	Address   address.Address
	Type      string
	Attrs     map[string]value.Value
	Lifecycle Lifecycle
}

// Desired converts a resolved resource for provider consumption, refusing any
// attribute that is still unknown. Callers reach this point only after every
// dependency has been created and its deferred expressions evaluated.
func (r ResolvedResource) Desired() (DesiredResource, error) {
	var unresolved []string
	attrs := make(map[string]value.Value, len(r.Attrs))
	for name, v := range r.Attrs {
		if !v.Known {
			unresolved = append(unresolved, name)
			continue
		}
		attrs[name] = v
	}
	if len(unresolved) > 0 {
		// Redundancy note (measured): removing this sort fails nothing —
		// the names it orders appear inside one error message, and no
		// fixture has two unresolved attributes on one resource. Kept
		// because this error reaches a user verbatim, and an error string
		// that reorders itself between identical runs is the kind of
		// nondeterminism that makes a tool look untrustworthy.
		sort.Strings(unresolved)
		return DesiredResource{}, fmt.Errorf("%s: attributes still unknown: %v", r.Address, unresolved)
	}
	return DesiredResource{
		Address:   r.Address,
		Type:      r.Type,
		Attrs:     attrs,
		Lifecycle: r.Lifecycle,
	}, nil
}

// ResourceState is the recorded association between a logical resource and the
// external object it manages. PLAN.md §3.4.
//
// The JSON tags are the state file's versioned on-disk contract; see Lifecycle.
// omitzero rather than omitempty on the timestamps because encoding/json cannot
// omit a zero struct any other way.
type ResourceState struct {
	Address      address.Address        `json:"address"`
	Type         string                 `json:"type"`
	Provider     string                 `json:"provider"`
	ProviderID   string                 `json:"provider_id"`
	Attributes   map[string]value.Value `json:"attributes"`
	Dependencies []address.Address      `json:"dependencies,omitempty"`
	Lifecycle    Lifecycle              `json:"lifecycle"`
	CreatedAt    time.Time              `json:"created_at,omitzero"`
	UpdatedAt    time.Time              `json:"updated_at,omitzero"`
}

// Clone copies a resource state so that a caller cannot mutate the original
// through it: the struct, the Attributes map, the Dependencies slice and the
// INTERIOR of every composite attribute are all fresh. Refresh and planning
// must never mutate the state that was loaded from disk, and this is what they
// use to avoid it.
//
// The interior used to be shared. A value.Value whose Raw holds a
// map[string]value.Value or a []value.Value carries a reference type, so
// copying the Value copied the header and left both states addressing one
// backing map: a caller reaching into Attributes["tags"].Raw wrote through to
// the state on disk. Nothing did, which is the only reason it never showed up,
// and "no caller does today" is not a property a shared mutable container can
// be relied on to keep. value.Clone closes it.
//
// This comment used to say "deep-copies" flatly. It was read during M2 by an
// implementer who concluded from it that no aliasing precaution was needed,
// which was the opposite of what it was trying to say — hence the precision,
// which is now simply true.
func (s *ResourceState) Clone() *ResourceState {
	if s == nil {
		return nil
	}
	out := *s
	out.Attributes = value.CloneMap(s.Attributes)
	out.Dependencies = append([]address.Address(nil), s.Dependencies...)
	return &out
}
