// Package pluginproto is the contract between infrena and a provider plugin.
//
// A plugin is a separate binary that infrena launches and talks to over stdio
// in newline-delimited JSON. This package holds the message types and the
// protocol version and nothing else: no transport, no host logic, no plugin
// logic. Both sides compile against it, so it stays importable by a plugin
// author without dragging in half the engine.
//
// The wire form is the compatibility contract, not the Go types. A plugin
// built against an older SDK keeps working for as long as its protocol version
// is supported, so this package changes additively: a new optional field is
// fine, a removed or repurposed one bumps Version.
package pluginproto

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// Version is the protocol version this build speaks, announced in the
// Handshake.
//
// Raise it whenever a newer plugin talking to an older host would have
// something silently dropped rather than refused. Decoding is lenient in both
// directions, so an unknown key is ignored rather than reported; announcing a
// version the host does not support turns that silence into a refusal that
// names the plugin.
//
//	2 — `optional` and `aliases` on a schema attribute
//	3 — `References` on a schema attribute
//	4 — `system_owned` and `system_owned_reason` on a discovered resource
//	5 — `max_concurrency` in the handshake
//	6 — `elem` on a schema attribute, describing a list's elements
//
// The reverse direction needs no bump: an older plugin omits the key, and
// absent means what it always meant.
const Version = 6

// Supported lists every protocol version this build can talk to, newest first.
//
// It is a set rather than a single number so that raising Version does not
// orphan every plugin already built. Older versions stay listed for as long as
// what an older plugin omits still means what it meant then.
var Supported = []int{6, 5, 4, 3, 2, 1}

// IsSupported reports whether a plugin's protocol version can be spoken here.
func IsSupported(v int) bool {
	return slices.Contains(Supported, v)
}

// CookieEnv is the environment variable the host sets when launching a plugin.
//
// A plugin binary run by hand without it prints a line saying what it is and
// exits non-zero, rather than sitting silently waiting for protocol input on a
// terminal, which looks exactly like a hang.
const CookieEnv = "INFRENA_PLUGIN_COOKIE"

// Method names. Every request carries exactly one, in Request.Method.
const (
	// MethodSchemas asks for the resource types a plugin offers. It needs no
	// configured instance, so it can be answered first.
	MethodSchemas = "schemas"
	// MethodConfigure builds one configured instance and returns its handle.
	// Every resource method carries that handle.
	MethodConfigure = "configure"
	// MethodRead reads one resource's current state.
	MethodRead = "read"
	// MethodCreate creates one resource.
	MethodCreate = "create"
	// MethodUpdate updates one resource.
	MethodUpdate = "update"
	// MethodDelete deletes one resource.
	MethodDelete = "delete"
	// MethodDiscover lists what exists in the remote system.
	MethodDiscover = "discover"
	// MethodImport reads one existing resource by its provider ID.
	MethodImport = "import"
	// MethodShutdown asks the plugin to stop serving. It is answered, then the
	// plugin exits.
	MethodShutdown = "shutdown"
	// MethodCancel is a notification, not a request: it carries the id of a
	// request already in flight and expects no response of its own. The host
	// still waits for that request's real response, so that a plugin part-way
	// through a create can finish reporting what it created.
	MethodCancel = "cancel"
)

// Handshake is a plugin's first message, sent before it serves any request.
type Handshake struct {
	// Protocol is the protocol version the plugin speaks. The host refuses a
	// plugin whose version is not in Supported.
	Protocol int `json:"protocol"`
	// Name is the plugin's own name, which prefixes every resource type it
	// serves.
	Name string `json:"name"`
	// Version is the plugin's own release version, not the protocol's.
	Version string `json:"version"`
	// MaxConcurrency is how many operations this plugin wants in flight
	// against it at once, or 0 for no claim. Protocol 5.
	//
	// Only the plugin knows: a rate limit belongs to the remote API, which the
	// host has never seen. Absent means no claim and the host keeps its own
	// conservative default, so an older host ignoring this runs the plugin
	// wider than it asked for. A plugin that depends on its ceiling being
	// honoured requires it with an `infrena: ">= …"` floor in its manifest.
	MaxConcurrency int `json:"max_concurrency,omitempty"`
}

// Request is one call from host to plugin.
type Request struct {
	// ID is unique within a session and is what multiplexes calls on one pipe.
	// The SDK serves each request in its own goroutine, so --parallelism works
	// against a single plugin process. The response carries the same ID.
	ID uint64 `json:"id"`
	// Method is one of the Method constants.
	Method string `json:"method"`
	// Params is the method's own parameter message.
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is one reply. Exactly one of Result and Error is set.
type Response struct {
	// ID is the ID of the request being answered. Responses may arrive in any
	// order.
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is a failed request, carrying its own retryability.
//
// The classification travels with the error because provider.ClassifyError
// takes an error value, and an error does not cross a pipe. The SDK asks the
// plugin to classify on the plugin's side and puts the answer here; the host
// rebuilds a typed error, and its retry logic reads this rather than calling
// back into a provider that would no longer recognise the value.
type Error struct {
	// Message is the failure as the plugin described it.
	Message string `json:"message"`
	// Retryability is provider.Retryability's numeric value. Absent means
	// NotSafeToRetry, the safe default for a plugin too old to send it.
	Retryability uint8 `json:"retryability,omitempty"`
}

// Error returns the message, so a decoded protocol error is usable as an
// ordinary Go error.
func (e *Error) Error() string { return e.Message }

// ConfigureParams asks the plugin to build one configured instance of itself.
type ConfigureParams struct {
	// Instance is the name the configuration gave this instance.
	Instance string `json:"instance"`
	// Config is the instance's own resolved configuration.
	Config map[string]value.Value `json:"config,omitempty"`
	// Dir is the project directory, which a relative path in the
	// configuration resolves against. A plugin is a separate process, so the
	// directory it inherited is not where the project is.
	Dir string `json:"dir"`
}

// ConfigureResult returns the handle naming this configured instance.
//
// There is one process per plugin, not per instance: two accounts means one
// plugin process holding two configured clients, told apart by this handle,
// which every later request carries.
type ConfigureResult struct {
	Handle string `json:"handle"`
}

// SchemasResult lists the resource types a plugin offers.
//
// Schemas need no configuration, which is what lets a process that has not been
// told anything yet answer the very first request.
type SchemasResult struct {
	Definitions []*schema.ResourceDefinition `json:"definitions"`
}

// ResourceParams carries one resource for read, create or delete to act on.
//
// Deliberately not a resource.ResourceState. The host sends only what the
// plugin owns and re-attaches every bookkeeping field itself, so that a field
// never sent cannot be dropped by a plugin that forgot to carry it forward —
// losing a resource's lifecycle rules would make a prevent-destroy guard
// vanish silently.
type ResourceParams struct {
	// Handle names the configured instance, from ConfigureResult.
	Handle string `json:"handle"`
	// Type is the resource type being acted on.
	Type string `json:"type"`
	// Address is the name infrena knows this resource by.
	//
	// Sent, unlike the rest of the bookkeeping, because it is an input rather
	// than something the plugin reports: a provider legitimately needs a name
	// to tag the resource with, or to put in an error message. An address on a
	// result is still ignored and re-attached from what the host knows.
	Address string `json:"address,omitempty"`
	// ProviderID is the remote system's own identifier, empty for a resource
	// that does not exist yet.
	ProviderID string `json:"provider_id,omitempty"`
	// Attributes are the resource's attributes: current state for read and
	// delete, desired state for create.
	Attributes map[string]value.Value `json:"attributes,omitempty"`
}

// UpdateParams carries both sides of an update.
type UpdateParams struct {
	// Handle names the configured instance, from ConfigureResult.
	Handle string `json:"handle"`
	// Current is the resource as it exists now.
	Current ResourceParams `json:"current"`
	// Desired is the resource as the configuration asks for it.
	Desired ResourceParams `json:"desired"`
}

// ResourceResult is what a plugin reports about one resource.
type ResourceResult struct {
	// Absent says the resource does not exist. That is a valid answer to
	// `read`. For `create` and `update` the host turns it into an error
	// instead: a result reporting nothing there is indistinguishable from
	// "nothing happened", and if the call did take effect the resource is now
	// orphaned — created for real and tracked nowhere.
	Absent bool `json:"absent,omitempty"`
	// ProviderID is the remote system's own identifier for the resource.
	ProviderID string `json:"provider_id,omitempty"`
	// Attributes are the resource's attributes as the plugin now sees them.
	Attributes map[string]value.Value `json:"attributes,omitempty"`
}

// DiscoverParams asks what exists.
//
// There is deliberately no region or account field. The host cannot supply one
// without knowing what a region is for a particular cloud, so a plugin that
// scans several takes them from its own instance configuration.
type DiscoverParams struct {
	// Handle names the configured instance, from ConfigureResult.
	Handle string `json:"handle"`
	// Types limits the scan to these resource types. Empty asks for
	// everything the plugin offers.
	Types []string `json:"types,omitempty"`
}

// DiscoverResult lists what was found.
type DiscoverResult struct {
	Found []Discovered `json:"found"`
}

// Discovered is one resource that exists, whether or not infrena manages it.
type Discovered struct {
	// Type is the resource type, as the plugin names it.
	Type string `json:"type"`
	// ProviderID is the remote system's own identifier for the resource.
	ProviderID string `json:"provider_id"`
	// Attributes are the resource's current attributes.
	Attributes map[string]value.Value `json:"attributes,omitempty"`

	// SystemOwned and SystemOwnedReason are the plugin's claim that the remote
	// system created and manages this resource, and its reason. Protocol 4.
	//
	// Both are omitempty, so a plugin making no claim sends exactly what it
	// always sent; an older host reading a newer plugin is refused at the
	// handshake rather than quietly dropping the flag.
	SystemOwned       bool   `json:"system_owned,omitempty"`
	SystemOwnedReason string `json:"system_owned_reason,omitempty"`
}

// ImportParams adopts one existing resource by its provider ID.
type ImportParams struct {
	// Handle names the configured instance, from ConfigureResult.
	Handle string `json:"handle"`
	// Type is the resource type to import as.
	Type string `json:"type"`
	// ID is the remote system's own identifier for the resource.
	ID string `json:"id"`
}

// Encode marshals params for a request, wrapping the failure with the method so
// a malformed call says which one it was.
func Encode(method string, params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encoding %s params: %w", method, err)
	}
	return b, nil
}

// Decode unmarshals into out, naming the method on failure.
func Decode(method string, raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding %s: %w", method, err)
	}
	return nil
}

// StateOf rebuilds a ResourceState from what a plugin reported, or nil when
// the plugin reported the resource absent.
//
// The bookkeeping fields are deliberately left zero; the host re-attaches them
// from what it already knows. Returning only what the plugin owns keeps a
// caller from mistaking a plugin's answer for a complete state.
func (r ResourceResult) StateOf(resourceType string) *resource.ResourceState {
	if r.Absent {
		return nil
	}
	return &resource.ResourceState{
		Type:       resourceType,
		ProviderID: r.ProviderID,
		Attributes: r.Attributes,
	}
}
