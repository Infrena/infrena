// Package provider defines the boundary between the infra core and the systems
// it manages. No provider-specific type may appear above this interface.
// PLAN.md §31.
package provider

import (
	"context"
	"errors"

	"infra/pkg/resource"
	"infra/pkg/schema"
	"infra/pkg/value"
)

// ErrNotImplemented is returned by capabilities a provider does not offer.
var ErrNotImplemented = errors.New("not implemented")

// Retryability classifies a provider error. The provider classifies; the core
// owns backoff. PLAN.md §35.
type Retryability uint8

const (
	NotSafeToRetry Retryability = iota
	ConditionallyRetryable
	SafeToRetry
)

// DiscoverRequest and DiscoveredResource are declared in M1 so the interface
// does not churn in Phase 2, where discovery is implemented.
type DiscoverRequest struct {
	Types  []string
	Region string
}

// DiscoveredResource is a resource found during discovery.
type DiscoveredResource struct {
	Type       string
	ProviderID string
	Attributes map[string]value.Value
}

// Provider is the boundary between the infra core and external systems it manages.
type Provider interface {
	// Name returns the provider name.
	Name() string
	// Definitions returns the resource definitions this provider supports.
	Definitions() []*schema.ResourceDefinition

	// Read returns the current state of a managed resource. A nil state with a
	// nil error means the resource no longer exists.
	Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error)
	// Create creates a resource.
	Create(ctx context.Context, desired *resource.DesiredResource) (*resource.ResourceState, error)
	// Update updates a resource.
	Update(ctx context.Context, current *resource.ResourceState, desired *resource.DesiredResource) (*resource.ResourceState, error)
	// Delete deletes a resource.
	Delete(ctx context.Context, current *resource.ResourceState) error

	// Discover discovers resources of the given types.
	Discover(ctx context.Context, req DiscoverRequest) ([]DiscoveredResource, error)
	// Import imports a resource by ID.
	Import(ctx context.Context, resourceType, id string) (*resource.ResourceState, error)

	// ClassifyError classifies a provider error for retryability.
	ClassifyError(err error) Retryability
}
