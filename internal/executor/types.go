// Package executor turns an execution graph into changes against real
// infrastructure: it drains a graph.Walk, dispatches each ready operation to
// its provider, persists state after every one, and reports what happened.
// Spec §15.
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
	// destroyed, sorted by canonical address (pkg/address.Sort) — never in
	// the order operations happened to finish in. Determinism (invariant 6,
	// PLAN.md §47) requires this: two runs over the same plan, state and
	// provider observations must produce an equivalent plan and, downstream
	// of that, an equivalent Result, regardless of which worker's provider
	// call happened to return first. Apply assembles this into a set keyed
	// by address during the run — so a replace's two completions at one
	// address collapse into a single entry — and sorts it exactly once, at
	// the very end, never appending in completion order.
	Applied []address.Address
	// Forgotten is the subset of Applied that was dropped from management
	// without being deleted — OpForget, spec §11's retain. It is a subset,
	// not a separate category: a forget IS applied work, and the run did
	// exactly what was planned.
	//
	// It exists because the summary renderer cannot recover the distinction
	// any other way. It marks a removal by finding the address absent from
	// Result.State, and a forgotten resource is absent for exactly the same
	// reason a destroyed one is — so every forget rendered with a destroy's
	// "-", telling the user their retained resource had been deleted. The
	// plan renderer distinguishes them ("=" versus "-") because it has the
	// operation kind; this is how the summary gets it too.
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
	// cannot be exhausted by an unrelated wide graph touching many
	// providers at once (PLAN.md §34). Values below 1 mean 1.
	PerProvider int
	// ProviderLimits is what individual plugins declared they tolerate,
	// keyed by provider name, and it overrides PerProvider for those.
	//
	// A ceiling belongs to a cloud API, and the host has never seen one:
	// PerProvider is a constant chosen as "the order cloud APIs throttle at",
	// which is a guess made by the only participant with no information. A
	// plugin that knows a real number says so in its handshake (protocol 5),
	// and this is where that answer arrives. A provider absent from the map
	// made no claim and keeps PerProvider, which is why an older plugin is
	// unaffected.
	ProviderLimits map[string]int
	// Registry resolves a resource type to the provider that implements it.
	Registry *registry.Registry
	// Backend is where state is read from and persisted to, under a lock
	// held for the whole run. state.Backend rather than the concrete
	// *state.Local so a test can substitute a double that behaves
	// differently from Local in one specific way (e.g. actually honouring
	// ctx cancellation, which Local.Put does not) without editing Local
	// itself; every production caller still passes a *state.Local, which
	// satisfies this interface directly.
	Backend state.Backend
	// Environment names which environment's lock and state this run uses.
	Environment string
	// Observed is what the refresh that immediately preceded planning saw on
	// the real infrastructure, keyed by address.Address.String(). It is what
	// the executor builds the `current` argument of Provider.Update and
	// Provider.Delete out of — see currentFor (observed.go) for the merge, and
	// for why the last persisted state alone is the wrong answer.
	//
	// An address may be missing from it, and an entry may be nil: nil means
	// the refresh found nothing there (the read failed, or the resource is
	// gone), and `infrena apply --plan`, which applies a saved plan without
	// refreshing at all, passes no observations whatever. Every one of those
	// falls back to the last persisted state, which is what the executor used
	// for every operation before observations existed.
	//
	// Read-only for the whole run: Apply never writes to it, and worker
	// goroutines read it concurrently.
	Observed map[string]*resource.ResourceState
	// Retry governs how a failed provider call is retried, per the
	// classification table in retry.go.
	Retry RetryPolicy
	// Now is injected the same way the planner's is: production wires
	// time.Now, tests wire a fixed clock so event timestamps and any
	// time-based assertions are deterministic. Like OnEvent below, it is
	// called concurrently from multiple worker goroutines — once per Event
	// emitted, and Apply may have opts.Parallelism workers each emitting
	// events at once — so an implementation that is not itself safe for
	// concurrent use (time.Now is; a hand-rolled fake clock with mutable
	// internal state may not be) must serialize its own access.
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
// It deliberately carries no value.Value and no resource attribute map. The
// only text on it is Message, a human-readable line that whatever populates
// an Event has ALREADY passed through value.Format wherever it originated
// from a resource attribute — the same way every other place in this
// codebase that renders a value does; pkg/value/format.go is the only
// redaction path, and nothing may grow a second one. Giving Event a raw
// attributes field would hand every future OnEvent implementation — a
// terminal renderer, a JSON log line, a webhook — its own opportunity to
// print a secret straight from an attribute before it is ever redacted,
// which is exactly the shape of leak value.Format exists to foreclose.
// Keeping Event's surface to an address, a kind, an attempt counter and
// pre-formatted text keeps "sensitivity is per-leaf" true by construction
// here: there is no leaf on this type for a caller to reach around Format
// and print.
type Event struct {
	// Kind classifies the notification.
	Kind EventKind
	// Address identifies the resource the operation concerns.
	Address address.Address
	// Op is what the plan proposed for this resource — Create, Update,
	// Replace, Destroy or Forget. It is planner.OpKind rather than a
	// provider verb: a Replace is one plan operation dispatched as two
	// provider calls, and an Event reports at the plan-operation level a
	// person reads a plan at, not the provider-call level retry.go
	// classifies at.
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
