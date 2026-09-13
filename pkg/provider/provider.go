// Package provider defines the boundary between the infra core and the systems
// it manages. No provider-specific type may appear above this interface.
// PLAN.md §31.
package provider

import (
	"context"
	"errors"

	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
)

// Config is everything a plugin is told when it configures one instance of itself.
//
// A STRUCT rather than a bag of arguments, because a plugin built against an older
// SDK must keep compiling when this grows: a new field is additive, a new parameter
// is not, and plugins are compiled by other people.
//
// It also keeps ProjectDir out of Values. The directory is not part of anybody's
// configuration — it is context the host supplies — and carrying it as a reserved
// key meant every plugin that validates its own keys had to know to skip it. The
// fake provider did not, and refused its own configuration.
type Config struct {
	// Instance is the name the configuration gave this instance.
	//
	// A plugin whose configuration is entirely optional still needs it to keep two
	// instances apart: the fake provider's cloud file is named after the instance,
	// so two undeclared instances are two accounts rather than two names for one.
	Instance string

	// Values is the instance's own configuration, resolved. Nothing reserved
	// appears here; every key came from the user.
	Values map[string]value.Value

	// ProjectDir is the project directory, for resolving a relative path a user
	// wrote. It is not the plugin's working directory, which is inherited from
	// infrata and is not where the project is.
	ProjectDir string
}

// Value returns one configuration value, if the user supplied it.
func (c Config) Value(key string) (value.Value, bool) {
	v, ok := c.Values[key]
	return v, ok
}

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

// Plugin is a provider IMPLEMENTATION: the resource types it offers, and how to
// construct one configured instance of itself.
//
// It exists to break a cycle. Constructing a provider needs its configuration;
// resolved configuration needs variables; variables need a compile; and a compile
// needs the schemas. Splitting the two halves resolves it, because SCHEMAS NEED NO
// CONFIGURATION — `aws.instance` is described the same way whichever account it
// would be created in — so the compiler can have them before any provider object
// exists (PLAN.md §12.1).
//
// One plugin serves many instances. `New` is called once per instance, with that
// instance's own resolved configuration.
type Plugin interface {
	// Name is the plugin's own name, which is what `plugin:` names in a
	// `providers:` entry. Never an instance name.
	Name() string
	// Definitions are the resource types it offers. Static: the same for every
	// instance, and available before any is configured.
	Definitions() []*schema.ResourceDefinition
	// New constructs one instance from its resolved configuration.
	//
	// An error here is a configuration error the user can act on — a missing
	// credential, an unreadable path, a key the plugin does not accept — not a
	// programming error. FAIL CLOSED on a key you do not understand: a misspelled
	// key silently ignored means an instance quietly sharing another's account.
	New(cfg Config) (Provider, error)
}

// Provider is the boundary between the infra core and external systems it manages.
type Provider interface {
	// Name returns the provider name.
	Name() string
	// Definitions returns the resource definitions this provider supports.
	Definitions() []*schema.ResourceDefinition

	// Read returns the current state of a managed resource. A nil state with a
	// nil error means the resource no longer exists.
	//
	// The returned ResourceState MUST carry forward every field the
	// provider itself does not own — at minimum Dependencies, Lifecycle and
	// CreatedAt — from current. Read reports what the remote system says
	// about the attributes it manages; it is not the source of truth for
	// bookkeeping infra attaches to a resource, and current is what already
	// holds that bookkeeping correctly. `infra refresh` (spec §10) persists
	// whatever Read returns verbatim via state.Set, so any field silently
	// dropped here is not a stale read, it is a destructive write: losing
	// Dependencies corrupts the next plan's destroy ordering (spec §14 —
	// Dependencies is the only source of destroy-ordering edges once a
	// resource leaves configuration), and losing Lifecycle makes a
	// configured prevent_destroy or retain guard vanish with no error at
	// all, which is the worst failure mode this product has. A provider
	// that reads current's bookkeeping fields back unchanged onto its
	// result satisfies this; providers/test's carryForward is the pattern
	// to follow.
	Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error)
	// Create creates a resource. On success it MUST return the created
	// resource's state, never (nil, nil): the executor persists exactly
	// what is returned here as the record of what now exists, and a nil
	// result with a nil error is indistinguishable from "nothing happened"
	// to every caller above this interface. If the underlying call actually
	// took effect, that resource is now orphaned — created for real but
	// tracked nowhere, unfindable by a later plan or destroy. Report a
	// failure through the error return instead if the created state cannot
	// be determined.
	//
	// It need not set Lifecycle. The executor stamps that onto the returned
	// state from the plan, because lifecycle is bookkeeping infra attaches to
	// a resource rather than anything the remote system knows about, and a
	// provider that forgot it would silently lose a prevent_destroy or retain
	// guard. Read is the exception, above: no executor is involved in a
	// refresh, so Read must carry it forward itself.
	Create(ctx context.Context, desired *resource.DesiredResource) (*resource.ResourceState, error)
	// Update updates a resource. Same non-nil-on-success requirement as
	// Create, for the same reason.
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
