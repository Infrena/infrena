// Package resource defines the resource representations shared by the engine
// and providers.
//
// The distinction between ResolvedResource and DesiredResource is load-bearing:
// the planner works with ResolvedResource, where unknown values are permitted;
// providers only ever receive DesiredResource, where they are not.
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
	// PreventDestroy refuses a plan that would destroy this resource.
	PreventDestroy bool `json:"prevent_destroy"`
	// PreventReplace refuses a plan that would replace this resource: destroy
	// it and create a new one in its place.
	//
	// Separate from PreventDestroy because they guard different mistakes, and
	// neither implies the other. PreventDestroy is about a resource leaving
	// configuration — somebody deleted the block. PreventReplace is about a
	// resource staying in configuration while an attribute the provider marks
	// ForceNew changes underneath it, which is the more insidious of the two:
	// the configuration still names the resource, the diff looks like an edit,
	// and the data is gone all the same.
	//
	// Deliberately opt-in per resource rather than an environment-wide setting.
	// An environment that refused every replacement could never change an
	// immutable attribute on anything, and the resources this matters for are
	// specific and few: the database, the volume, the thing whose contents do
	// not survive being recreated.
	PreventReplace bool `json:"prevent_replace,omitempty"`
	// CreateBeforeDestroy reverses the two halves of a replacement: the new
	// object is built, everything pointing at it is moved across, and only then
	// is the old one destroyed.
	//
	// It is for a resource that must not be absent in between. A replacement is
	// destroy-then-create by default, which leaves a window with nothing there —
	// an outage for anything serving traffic — and means a failed create leaves
	// nothing at all, the old object already gone.
	//
	// Opt-in per resource, and it cannot be the default, because a great many
	// resources cannot exist twice: a unique name, a fixed port, a key that is
	// the identity. For those, reversing the order turns a clean replacement
	// into a create that collides.
	//
	// omitempty, for the same reason PreventReplace has it: state written before
	// this field existed decodes unchanged, and absent means false, which is
	// what every existing resource is.
	CreateBeforeDestroy bool `json:"create_before_destroy,omitempty"`
	// Retain drops the resource from management without deleting the external
	// object, when the configuration stops naming it.
	Retain bool `json:"retain"`

	// IgnoreChanges names attributes whose drift the planner does not propose to
	// revert: the real resource keeps whatever it has, and the plan says so
	// rather than staying silent.
	//
	// The case it exists for is something outside infrena owning one attribute.
	// A CI pipeline sets an ECS service's task revision on every deploy, so a
	// plan computed from configuration would revert it on the next apply and
	// undo the deployment.
	//
	// The names are canonical by the time they are here: whatever spelling the
	// user wrote is resolved earlier, so `ignore_changes: [taskRevision]` and
	// `[task_revision]` both arrive as the attribute the plugin declared.
	//
	// Sorted, so a plan artifact is byte-stable however the list was written.
	IgnoreChanges []string `json:"ignore_changes,omitempty"`
}

// ResolvedResource is a resource after the planner has resolved it. Its
// attributes may still be unknown.
type ResolvedResource struct {
	Address address.Address
	Type    string
	// Provider is the provider instance this resource belongs to — a name, never
	// a plugin. Resolved from the resource's own `provider:`, or the module call
	// it came from, or the default instance.
	//
	// It reaches ResourceState so that a destroy, which has no configuration at
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
		// Sorted because this error reaches a user verbatim, and an error string
		// that reorders itself between identical runs makes a tool look
		// untrustworthy.
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
// external object it manages.
//
// The JSON tags are the state file's versioned on-disk contract; see Lifecycle.
// omitzero rather than omitempty on the timestamps because encoding/json cannot
// omit a zero struct any other way.
type ResourceState struct {
	Address  address.Address `json:"address"`
	Type     string          `json:"type"`
	Provider string          `json:"provider"`
	// ProviderID is the external object's identity as the provider names it.
	ProviderID   string                 `json:"provider_id"`
	Attributes   map[string]value.Value `json:"attributes"`
	Dependencies []address.Address      `json:"dependencies,omitempty"`
	Lifecycle    Lifecycle              `json:"lifecycle"`
	CreatedAt    time.Time              `json:"created_at,omitzero"`
	UpdatedAt    time.Time              `json:"updated_at,omitzero"`
	// Deposed are objects this address used to be, still real and no longer
	// current. It is how create_before_destroy survives a crash.
	//
	// The window between the two halves of a reversed replacement is the whole
	// problem: both objects exist at once, and state is persisted after every
	// node, so "both are real" has to be representable on disk rather than
	// merely in memory. Without this, the create phase would overwrite the only
	// record of the old object's ProviderID, and a destroy that then failed
	// would leave something real that nothing can name.
	//
	// Normally empty, and an entry that outlives a run means the destroy failed,
	// so the next plan schedules it.
	//
	// A full ResourceState rather than a bare id, because deleting it is an
	// ordinary Delete call and the provider is handed the object's prior state.
	//
	// omitempty: state written before this existed decodes unchanged, and every
	// resource that has never been replaced this way serialises exactly as it
	// did.
	Deposed []*ResourceState `json:"deposed,omitempty"`
}

// Clone copies a resource state so that a caller cannot mutate the original
// through it: the struct, the Attributes map, the Dependencies slice and the
// interior of every composite attribute are all fresh. Refresh and planning must
// never mutate the state that was loaded from disk, and this is what they use to
// avoid it.
//
// The interior matters as much as the map. A value.Value whose Raw holds a
// map[string]value.Value or a []value.Value carries a reference type, so copying
// the Value alone would leave both states addressing one backing map and a
// caller reaching into Attributes["tags"].Raw would write through to the state
// on disk. value.Clone closes that.
func (s *ResourceState) Clone() *ResourceState {
	if s == nil {
		return nil
	}
	out := *s
	out.Attributes = value.CloneMap(s.Attributes)
	out.Dependencies = append([]address.Address(nil), s.Dependencies...)
	// Deposed entries are cloned too, not shared. They are ResourceStates and
	// carry the same interior aliasing this method exists to prevent — and one
	// of them is the only record of a live object, which is the worst thing in
	// state to let a caller write through to.
	if s.Deposed != nil {
		out.Deposed = make([]*ResourceState, len(s.Deposed))
		for i, d := range s.Deposed {
			out.Deposed[i] = d.Clone()
		}
	}
	return &out
}
