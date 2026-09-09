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

	"infra/pkg/address"
	"infra/pkg/value"
)

// Lifecycle defines immutability constraints on a resource.
type Lifecycle struct {
	PreventDestroy bool
	Retain         bool
}

// ResolvedResource is a resource after the planner has resolved it.
type ResolvedResource struct {
	Address   address.Address
	Type      string
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
type ResourceState struct {
	Address      address.Address
	Type         string
	Provider     string
	ProviderID   string
	Attributes   map[string]value.Value
	Dependencies []address.Address
	Lifecycle    Lifecycle
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Clone deep-copies a resource state. Refresh and planning must never mutate
// the state that was loaded from disk.
func (s *ResourceState) Clone() *ResourceState {
	if s == nil {
		return nil
	}
	out := *s
	out.Attributes = make(map[string]value.Value, len(s.Attributes))
	for k, v := range s.Attributes {
		out.Attributes[k] = v
	}
	out.Dependencies = append([]address.Address(nil), s.Dependencies...)
	return &out
}
