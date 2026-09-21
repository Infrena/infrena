// Package provider defines the boundary between the infrena core and the
// systems it manages. No provider-specific type may appear above this
// interface.
package provider

import (
	"context"
	"errors"

	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// Config is everything a plugin is told when it configures one instance of
// itself.
//
// A struct rather than a list of arguments, so that a plugin built against an
// older SDK keeps compiling when this grows: a new field is additive, a new
// parameter is not, and plugins are compiled by other people.
//
// ProjectDir is a field of its own rather than a reserved key in Values,
// because a plugin that validates its own configuration keys would otherwise
// have to know to skip it.
type Config struct {
	// Instance is the name the configuration gave this instance.
	//
	// A plugin whose configuration is entirely optional still needs it to keep
	// two instances apart, so that two undeclared instances are two accounts
	// rather than two names for one.
	Instance string

	// Values is the instance's own configuration, resolved. Nothing reserved
	// appears here; every key came from the user.
	Values map[string]value.Value

	// ProjectDir is the project directory, for resolving a relative path a user
	// wrote. It is not the plugin's working directory, which is inherited from
	// infrena and is not where the project is.
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
// owns the backoff and retry policy built on that classification.
type Retryability uint8

const (
	// NotSafeToRetry means the operation must not be attempted again. It is
	// the zero value, so an error nobody classified is treated as the least
	// safe kind.
	NotSafeToRetry Retryability = iota
	// ConditionallyRetryable means the failure was ambiguous: the operation
	// may or may not have taken effect. The core retries reads and updates on
	// it, but never creates or deletes.
	ConditionallyRetryable
	// SafeToRetry means the operation definitely had no effect and may simply
	// be attempted again.
	SafeToRetry
)

// DiscoverRequest asks a provider what exists.
//
// There is deliberately no region or account field. The host cannot know what
// a region is without learning about a particular cloud, so a provider that
// scans several takes them from its own instance configuration.
type DiscoverRequest struct {
	// Types filters the scan to these resource types. Empty asks for
	// everything the provider offers. The provider applies the filter itself,
	// because answering only for the types asked for is usually fewer API
	// calls than fetching everything and discarding most of it.
	Types []string
}

// DiscoveredResource is one resource found during discovery.
type DiscoveredResource struct {
	// Type is the resource type, as the provider names it.
	Type string
	// ProviderID is the remote system's own identifier for the resource.
	ProviderID string
	// Attributes are the resource's current attributes, as Read would report
	// them.
	Attributes map[string]value.Value

	// SystemOwned marks a resource the remote system created and manages,
	// which a user did not ask for and generally must not adopt: a cloud's
	// default network, its default security group, its service-linked roles.
	//
	// Only the plugin can set it. Detecting such a resource means recognising
	// one cloud's conventions, and the core must not know about any particular
	// cloud.
	//
	// Nothing refuses to import a flagged resource — adopting one is
	// occasionally right — but it is never adopted by default and never
	// silently.
	SystemOwned bool

	// SystemOwnedReason says why, in the plugin's own words, for the line a
	// user reads before deciding. A flag with no reason is one a user
	// overrides without understanding it.
	SystemOwnedReason string
}

// Plugin is a provider implementation: the resource types it offers, and how
// to construct one configured instance of itself.
//
// Splitting it from Provider breaks a cycle. Constructing a provider needs its
// configuration; resolved configuration needs variables; variables need a
// compile; and a compile needs the schemas. Schemas need no configuration — a
// resource type is described the same way whichever account it would be
// created in — so the compiler can have them before any provider object
// exists.
//
// One plugin serves many instances. New is called once per instance, with that
// instance's own resolved configuration.
type Plugin interface {
	// Name is the plugin's own name, which is what `plugin:` names in a
	// `providers:` entry. Never an instance name.
	Name() string
	// Definitions are the resource types the plugin offers. The same for every
	// instance, and available before any instance is configured.
	Definitions() []*schema.ResourceDefinition
	// New constructs one instance from its resolved configuration.
	//
	// An error here is a configuration error the user can act on — a missing
	// credential, an unreadable path, a key the plugin does not accept — not a
	// programming error. Fail closed on a key you do not understand: a
	// misspelled key silently ignored means an instance quietly sharing
	// another's account.
	New(cfg Config) (Provider, error)
}

// Provider is one configured instance of a plugin: the live connection between
// the core and the external system it manages.
type Provider interface {
	// Name returns the provider name.
	Name() string
	// Definitions returns the resource definitions this provider supports.
	Definitions() []*schema.ResourceDefinition

	// Read returns the current state of a managed resource. A nil state with a
	// nil error means the resource no longer exists.
	//
	// Return only what the provider owns: the provider ID and the attributes.
	// Bookkeeping the core attaches to a resource — address, dependencies,
	// lifecycle, timestamps — is re-attached by the host from what it already
	// holds, and anything set for it on the returned value is ignored.
	//
	// current is given so it can be used — the ID to look the resource up, the
	// attributes to diff against — not so it can be copied back.
	Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error)
	// Create creates a resource and returns its state.
	//
	// It must not return (nil, nil) on success. The executor persists exactly
	// what is returned as the record of what now exists, and a nil result with
	// a nil error is indistinguishable from "nothing happened" to every caller
	// above this interface — so if the call did take effect, that resource is
	// now orphaned: created for real, tracked nowhere, unfindable by a later
	// plan or destroy. Report an error instead when the created state cannot
	// be determined.
	//
	// Return only the provider ID and the attributes, as with Read.
	Create(ctx context.Context, desired *resource.DesiredResource) (*resource.ResourceState, error)
	// Update updates a resource and returns its new state. Same
	// non-nil-on-success requirement as Create, for the same reason.
	Update(ctx context.Context, current *resource.ResourceState, desired *resource.DesiredResource) (*resource.ResourceState, error)
	// Delete deletes a resource.
	Delete(ctx context.Context, current *resource.ResourceState) error

	// Discover lists resources that exist in the remote system, whether or not
	// this project manages them. Providers that do not offer it return
	// ErrNotImplemented.
	Discover(ctx context.Context, req DiscoverRequest) ([]DiscoveredResource, error)
	// Import reads one existing resource by its provider ID so it can be
	// adopted. Providers that do not offer it return ErrNotImplemented.
	Import(ctx context.Context, resourceType, id string) (*resource.ResourceState, error)

	// ClassifyError says whether the operation that produced err may be
	// retried. It is answered by the provider because only the provider can
	// recognise its own API's errors; the core decides what to do with the
	// answer.
	ClassifyError(err error) Retryability
}
