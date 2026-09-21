// Package executor turns an execution graph into changes against real
// infrastructure: it drains a graph.Walk, dispatches each ready operation to
// its provider, persists state after every one, and reports what happened.
package executor

import (
	"time"

	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
)

// Result is what one Apply run produced.
type Result struct {
	// Applied lists every address that was successfully created, updated or
	// destroyed, sorted by canonical address rather than by completion
	// order, so two runs over the same plan and state report the same thing
	// whichever worker returns first. It holds one entry per address, so a
	// replace's two completions collapse into one.
	Applied []address.Address
	// Forgotten is the subset of Applied dropped from management without
	// being deleted. It is a subset rather than a separate category: a
	// forget is applied work, and the run did what was planned.
	//
	// It exists because the summary renderer marks a removal by finding the
	// address absent from Result.State, and a forgotten resource is absent
	// for the same reason a destroyed one is. Without this every forget
	// would render as a deletion, which is the opposite of what happened.
	Forgotten []address.Address
	// Failed maps the planner.OpNode.ID() of every operation that failed to
	// the error it failed with. It is keyed by node ID rather than address
	// because a Replace is two nodes at one address ("destroy:x" and
	// "create:x"); an address alone cannot say which half failed.
	Failed map[string]error
	// Skipped lists the planner.OpNode.ID() of every operation that was
	// never attempted because one of its dependencies failed — every id
	// graph.Walk.Skip returned, accumulated across every failure in the run.
	Skipped []string
	// State is state as it now stands: every Put made during the run,
	// reflecting exactly what was actually applied rather than what the
	// plan proposed.
	State *state.State
}

// Options configures one Apply run.
type Options struct {
	// Parallelism bounds how many operations run at once across the whole
	// graph. Values below 1 mean 1 — a misconfigured --parallelism 0 must
	// degrade to serial execution, not deadlock on a zero-capacity resource.
	Parallelism int
	// PerProvider bounds how many operations run at once against a single
	// provider, independent of Parallelism, so one provider's rate limits
	// cannot be exhausted by a wide graph touching many providers at once.
	// Values below 1 mean 1.
	PerProvider int
	// ProviderLimits is what individual plugins declared they tolerate,
	// keyed by provider name, and it overrides PerProvider for those.
	//
	// The ceiling belongs to a cloud API, and the host has never seen one:
	// PerProvider is a guess made by the only participant with no
	// information. A plugin that knows a real number says so in its
	// handshake, and this is where that answer arrives. A provider absent
	// from the map made no claim and keeps PerProvider.
	ProviderLimits map[string]int
	// Registry resolves a resource type to the provider that implements it.
	Registry *registry.Registry
	// Backend is where state is read from and persisted to, under a lock
	// held for the whole run. It is the interface rather than the concrete
	// local backend so a test can substitute a double that differs in one
	// specific way, such as honouring ctx cancellation on a write.
	Backend state.Backend
	// Environment names which environment's lock and state this run uses.
	Environment string
	// Observed is what the refresh preceding planning saw on the real
	// infrastructure, keyed by address. The executor builds the current
	// argument of Provider.Update and Provider.Delete out of it; see
	// currentFor for the merge.
	//
	// An address may be missing, and an entry may be nil: nil means the
	// refresh found nothing there, and applying a saved plan passes no
	// observations at all. Both fall back to the last persisted state.
	//
	// Read-only for the whole run: Apply never writes to it, and worker
	// goroutines read it concurrently.
	Observed map[string]*resource.ResourceState
	// Retry governs how a failed provider call is retried.
	Retry RetryPolicy
	// Now supplies event timestamps: production wires time.Now, tests wire a
	// fixed clock. Like OnEvent, it is called concurrently from several
	// worker goroutines, so a fake clock with mutable internal state must
	// serialize its own access.
	Now func() time.Time
	// OnEvent receives one Event per progress notification. nil is allowed
	// — a caller that wants no progress reporting passes nothing. It may be
	// called concurrently from multiple workers; a receiver that is not
	// itself safe for concurrent use must serialize its own access.
	OnEvent func(Event)
}

// EventKind classifies one Event.
type EventKind uint8

const (
	// EventStarted reports that an operation has begun its first attempt.
	EventStarted EventKind = iota
	// EventSucceeded reports that an operation completed successfully.
	EventSucceeded
	// EventFailed reports that an operation failed with no further retry
	// coming — either the failure was not retryable, or every attempt
	// RetryPolicy allowed has been used.
	EventFailed
	// EventRetrying reports that an attempt failed but another is coming;
	// it fires before the backoff wait for that next attempt begins.
	EventRetrying
	// EventSkipped reports that an operation was never attempted because a
	// dependency of it failed.
	EventSkipped
)

// String names an event kind for logging and rendering.
func (k EventKind) String() string {
	switch k {
	case EventStarted:
		return "started"
	case EventSucceeded:
		return "succeeded"
	case EventFailed:
		return "failed"
	case EventRetrying:
		return "retrying"
	case EventSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

// Event is one progress notification from Apply.
//
// It deliberately carries no resource attributes and no value.Value. Its
// only text is Message, already passed through the one redaction path
// wherever it quotes an attribute. A raw attributes field here would give
// every OnEvent implementation — a terminal renderer, a log line, a webhook
// — its own chance to print a secret before it was ever redacted.
type Event struct {
	// Kind classifies the notification.
	Kind EventKind
	// Address identifies the resource the operation concerns.
	Address address.Address
	// Op is what the plan proposed for this resource. It is a plan operation
	// rather than a provider verb: a replace is one operation dispatched as
	// two provider calls, and an event reports at the level a person reads a
	// plan at.
	Op planner.OpKind
	// Attempt is 1 on an operation's first try and increases on each
	// EventRetrying. It is 0 for EventSkipped, which is never attempted.
	Attempt int
	// Err is set on EventFailed and nil otherwise.
	Err error
	// Message is human-readable detail — already redacted by whatever set
	// it, wherever it quotes a resource attribute.
	Message string
	// At is when the event occurred, from Options.Now.
	At time.Time
}
