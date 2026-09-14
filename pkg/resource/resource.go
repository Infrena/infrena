// Package resource defines the resource representations shared by the engine
// and providers.
//
// The distinction between ResolvedResource and DesiredResource is load-bearing:
// the planner works with ResolvedResource, where unknown values are permitted;
// providers only ever receive DesiredResource, where they are not. Spec §5.3.
package resource

import (
	"fmt"
	"maps"
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

// Clone copies a resource state deeply enough that a caller cannot mutate the
// original through it: the struct, the Attributes map and the Dependencies
// slice are all fresh. Refresh and planning must never mutate the state that
// was loaded from disk, and this is what they use to avoid it.
//
// The one thing it does NOT deep-copy is the interior of a composite value. A
// value.Value whose Raw holds a map[string]value.Value or a []value.Value
// shares that container with the original, so a caller that reaches into
// Attributes["x"].Raw and mutates it in place still writes through. No caller
// does today, and value.Value has no Clone of its own to make it cheap; if one
// ever needs to, that is the gap to close first.
//
// This comment used to say "deep-copies" flatly. It was read during M2 by an
// implementer who concluded from it that no aliasing precaution was needed,
// which was the opposite of what it was trying to say — hence the precision.
func (s *ResourceState) Clone() *ResourceState {
	if s == nil {
		return nil
	}
	out := *s
	out.Attributes = make(map[string]value.Value, len(s.Attributes))
	maps.Copy(out.Attributes, s.Attributes)
	out.Dependencies = append([]address.Address(nil), s.Dependencies...)
	return &out
}
