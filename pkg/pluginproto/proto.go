// Package pluginproto is the contract between infrena and a provider plugin.
//
// A plugin is a separate binary that infrena launches and talks to over stdio in
// newline-delimited JSON (PLAN.md §31.1). This package holds the message types
// and the protocol version, and NOTHING ELSE: no transport, no host logic, no
// plugin logic. It is what both sides compile against, so it must stay importable
// by a plugin author without dragging in half the engine.
//
// THE WIRE FORM IS THE COMPATIBILITY CONTRACT, not the Go types. A plugin built
// against an older SDK keeps working for as long as its protocol version is
// supported, so this package changes ADDITIVELY — a new optional field is fine, a
// removed or repurposed one bumps Version.
package pluginproto

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// Version is the protocol version this build speaks.
//
// The host accepts a SET of versions (see Supported), so raising this does not
// immediately orphan every plugin in the world.
//
// RAISED TO 2 on 2026-09-14, for `optional` and `aliases` on an attribute (PLAN.md
// §14.1). The messages did not change shape; the SCHEMA PAYLOAD gained two keys, and
// that is enough to need a version for one reason: Attribute.UnmarshalJSON decodes
// leniently, so a plugin built with this SDK talking to an OLDER host would have both
// keys silently DROPPED. Its aliases would simply not work, and the user would meet
// "aws.ec2.vpc has no attribute cidr" with nothing anywhere explaining why, or would
// have configuration refused for setting an attribute the plugin marked settable.
//
// Announcing 2 makes that a refusal instead: an older host lists what it supports and
// names the plugin. A plugin that does NOT use either field keeps announcing 1 by being
// built with an older SDK, and this build still talks to it — which is the whole point
// of Supported being a set rather than a number.
//
// RAISED TO 3 on 2026-09-14, for `References` on an attribute (PLAN.md §14.3). The
// messages did not change shape; the SCHEMA PAYLOAD gained one key, and the reason is
// exactly the reason 2 was raised: Attribute.UnmarshalJSON decodes leniently, so a
// plugin built with this SDK talking to an OLDER host would have References silently
// DROPPED. `${vpc}` would then report "declares no reference" about an attribute whose
// plugin plainly declares one, with nothing anywhere explaining why it was not heard.
//
// Announcing 3 makes that a refusal that names the plugin and the versions instead.
//
// RAISED TO 4 on 2026-09-15, for `system_owned` and `system_owned_reason` on a discovered
// resource (PLAN.md §31.1). This time the MESSAGE gained two keys rather than the schema
// payload, and the reason is the same one in a new place: a plugin built with this SDK
// talking to an older host would have both keys dropped by a lenient decode, so a default
// VPC the plugin plainly flagged would be offered for adoption with nothing anywhere
// saying it had been flagged — and importing the default VPC, then destroying it when it
// leaves configuration, is the worst foot gun discovery has.
//
// The other direction costs nothing, which is what keeps 3 supported: a protocol 3 plugin
// sends neither key, absent decodes as "the plugin made no claim", and that is exactly
// what every plugin in existence means today.
// Version 5 added Handshake.MaxConcurrency, so a plugin can declare the
// concurrency its API tolerates rather than the host guessing on its behalf.
const Version = 5

// Supported lists every protocol version this build can talk to, newest first.
//
// 1 stays: a plugin built before §14.1 sends no `optional` and no `aliases`, and absent
// means exactly what it meant then — not optional, no aliases. Nothing about an older
// plugin becomes wrong, so nothing about it should stop working. §61.1 calls this a
// MINOR: a version added while the previous one keeps working.
var Supported = []int{5, 4, 3, 2, 1}

// IsSupported reports whether a plugin's protocol version can be spoken here.
func IsSupported(v int) bool {
	return slices.Contains(Supported, v)
}

// CookieEnv is the environment variable the host sets when launching a plugin.
//
// A plugin binary run by hand without it prints a line saying what it is and
// exits non-zero. Without that check it would sit silently waiting for protocol
// input on a terminal, which looks exactly like a hang.
const CookieEnv = "INFRENA_PLUGIN_COOKIE"

// Method names. Every request carries one.
const (
	MethodSchemas   = "schemas"
	MethodConfigure = "configure"
	MethodRead      = "read"
	MethodCreate    = "create"
	MethodUpdate    = "update"
	MethodDelete    = "delete"
	MethodDiscover  = "discover"
	MethodImport    = "import"
	MethodShutdown  = "shutdown"
	// MethodCancel is a NOTIFICATION, not a request: it carries the id of a
	// request already in flight and expects no response of its own. The host
	// still waits for that request's real response.
	MethodCancel = "cancel"
)

// Handshake is a plugin's first message, sent before it serves any request.
type Handshake struct {
	Protocol int    `json:"protocol"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	// MaxConcurrency is how many operations this plugin wants in flight
	// against it at once, or 0 for "no claim". Protocol 5.
	//
	// THE PLUGIN IS THE ONLY PARTY THAT KNOWS. A rate limit belongs to a cloud
	// API, and the host has never seen one: before this the ceiling was a
	// constant of 8 chosen as "the order cloud APIs throttle at", which is a
	// guess made by the one participant with no information. A user flag would
	// only move the guess to somebody else.
	//
	// Absent means no claim, and the host keeps its conservative default — so
	// a protocol 4 plugin is unaffected and correct, which is why 4 stays
	// supported. The direction that would have been dangerous is the other
	// one: a plugin whose declared ceiling an older host ignored would be run
	// wider than it asked for and throttled. A plugin that actually depends on
	// its ceiling being honoured says so with an `infrena: ">= …"` floor in its
	// manifest, the same way any other protocol-gated behaviour is required.
	MaxConcurrency int `json:"max_concurrency,omitempty"`
}

// Request is one call from host to plugin.
//
// ID is unique within a session and is what multiplexes calls on one pipe: the
// SDK serves each request in its own goroutine, so --parallelism works against a
// single process.
type Request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is one reply. Exactly one of Result and Error is set.
type Response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is a failure, carrying its own retryability.
//
// The classification travels WITH the error because provider.ClassifyError takes
// an `error` value, and an error does not serialise. The SDK calls the plugin's
// ClassifyError on the plugin's side and puts the answer here; the host rebuilds a
// typed error, and §35's retry logic reads this instead of calling back into the
// provider with a value it would no longer recognise.
type Error struct {
	Message string `json:"message"`
	// Retryability is provider.Retryability's numeric value. Absent means
	// NotSafeToRetry, which is the safe default for a plugin too old to send it.
	Retryability uint8 `json:"retryability,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// ConfigureParams asks the plugin to build one configured instance of itself.
//
// Dir is the project directory. A plugin can no longer be handed one at
// construction — it is a separate process started before any configuration is
// read — and a relative path in its configuration has to resolve against the
// project rather than against whatever directory the plugin happened to inherit.
type ConfigureParams struct {
	Instance string                 `json:"instance"`
	Config   map[string]value.Value `json:"config,omitempty"`
	Dir      string                 `json:"dir"`
}

// ConfigureResult returns the handle naming this configured instance.
//
// One process per plugin, not per instance: two AWS accounts means one
// infrena-plugin-aws holding two configured clients, told apart by this handle.
type ConfigureResult struct {
	Handle string `json:"handle"`
}

// SchemasResult is what a plugin offers, before anything is configured.
//
// Schemas need no configuration, which is what breaks the cycle §12.1 describes
// and what lets `schemas` be answered by a process that has not been told
// anything yet.
type SchemasResult struct {
	Definitions []*schema.ResourceDefinition `json:"definitions"`
}

// ResourceParams carries a resource to act on.
//
// DELIBERATELY NOT a resource.ResourceState. The host sends only what the plugin
// owns — the type, the provider's own ID, and the attributes — and re-attaches
// every bookkeeping field itself (PLAN.md §31.1). A field never sent cannot be
// dropped by a plugin that forgot to carry it forward, and losing Lifecycle makes
// a prevent_destroy guard vanish silently.
type ResourceParams struct {
	Handle string `json:"handle"`
	Type   string `json:"type"`
	// Address is the name infrena knows this resource by.
	//
	// SENT, unlike the rest of the bookkeeping, because it is an INPUT rather than
	// something the plugin reports: a provider legitimately needs a name — to tag
	// the resource, to name it in the remote system, to put it in an error message.
	// The distinction the trust rules draw is between what the host sends and what
	// it believes coming back: the address on a RESULT is ignored and re-attached
	// from what the host already knows.
	Address    string                 `json:"address,omitempty"`
	ProviderID string                 `json:"provider_id,omitempty"`
	Attributes map[string]value.Value `json:"attributes,omitempty"`
}

// UpdateParams carries both sides of an update.
type UpdateParams struct {
	Handle  string         `json:"handle"`
	Current ResourceParams `json:"current"`
	Desired ResourceParams `json:"desired"`
}

// ResourceResult is what a plugin reports about one resource.
//
// Absent means the resource no longer exists, which is `read`'s (nil, nil). For
// `create` and `update` the host turns Absent into an error instead: a nil result
// there is indistinguishable from "nothing happened", and if the call did take
// effect the resource is now orphaned — created for real and tracked nowhere.
type ResourceResult struct {
	Absent     bool                   `json:"absent,omitempty"`
	ProviderID string                 `json:"provider_id,omitempty"`
	Attributes map[string]value.Value `json:"attributes,omitempty"`
}

// DiscoverParams asks what exists.
//
// A `region` field was here until 2026-09-13 and the host NEVER populated it —
// internal/discovery built every request without one and no flag could set it — so a
// plugin implementing against it read "" forever. A field in a wire contract that can
// never carry a value is a trap with a doc note taped over it, and the fake provider's
// authoring guide was about to document it as permanently empty.
//
// Removal is safe across the version boundary in both directions: an older plugin sending
// the key has it ignored, and a newer plugin reading an absent key gets the same zero
// value it always got. Nothing needed a protocol bump. A plugin that scans regions takes
// them from its OWN instance configuration, which is the model AWS agreed.
type DiscoverParams struct {
	Handle string   `json:"handle"`
	Types  []string `json:"types,omitempty"`
}

// DiscoverResult lists what was found.
type DiscoverResult struct {
	Found []Discovered `json:"found"`
}

// Discovered is one resource that exists, whether or not infrena manages it.
type Discovered struct {
	Type       string                 `json:"type"`
	ProviderID string                 `json:"provider_id"`
	Attributes map[string]value.Value `json:"attributes,omitempty"`

	// SystemOwned and SystemOwnedReason are the plugin's claim that the cloud
	// created and manages this resource. Protocol 4; omitempty, so a plugin
	// making no claim sends what it always sent, and an older host reading a
	// newer plugin is refused at the handshake rather than quietly dropping it.
	SystemOwned       bool   `json:"system_owned,omitempty"`
	SystemOwnedReason string `json:"system_owned_reason,omitempty"`
}

// ImportParams adopts one existing resource by its provider ID.
type ImportParams struct {
	Handle string `json:"handle"`
	Type   string `json:"type"`
	ID     string `json:"id"`
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

// StateOf rebuilds a ResourceState from what a plugin reported.
//
// The bookkeeping fields are NOT filled in here; internal/pluginhost re-attaches
// them from what it already knows. This returns only what the plugin owns, so
// that a caller cannot accidentally treat a plugin's answer as a complete state.
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
