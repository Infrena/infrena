// Package executor turns an execution graph into changes against real
// infrastructure: it drains a graph.Walk, dispatches each ready operation to
// its provider, persists state after every one, and reports what happened.
// Spec §15.
package executor

import (
	"context"
	"time"

	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
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

// RetryPolicy governs how a failed provider call is retried. It carries no
// behavior of its own; see retry.go for the classification rules and the
// loop that reads these fields.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first —
	// not the number of retries. Values below 1 mean 1: exactly one
	// attempt, no retry.
	MaxAttempts int
	// Base is the delay before the first retry. Each subsequent retry
	// doubles it, capped at Max.
	Base time.Duration
	// Max caps the backoff delay, however many attempts have elapsed.
	Max time.Duration
	// Sleep waits out one backoff delay. It is injectable so tests can
	// supply a version that returns immediately instead of actually
	// sleeping, and so the real implementation can return early — wrapping
	// ctx.Err() — when the context is cancelled mid-wait, which is how a
	// SIGINT during a retry delay is able to interrupt the wait rather than
	// block until it elapses. nil means the real implementation.
	Sleep func(context.Context, time.Duration) error
	// Jitter perturbs a computed backoff delay before it is used, so many
	// operations that failed at the same instant do not all wake up and
	// retry in lockstep. It is injectable so tests can supply the identity
	// function and assert exact delays. nil means the real implementation.
	Jitter func(time.Duration) time.Duration
	// OnRetry is called once per retry, immediately before the backoff wait
	// begins: attempt is the attempt that just failed (1-based), err is why,
	// and delay is how long the wait will be. nil means no notification.
	//
	// It exists because this loop is the only place that knows all three
	// facts at the moment they are true. Task 8 needs them to emit
	// EventRetrying with honest timing; reconstructing them from outside
	// would mean guessing at the backoff schedule, and a progress line that
	// guesses is worse than none. Called synchronously and before the wait,
	// so an event reaches the user while the delay is still ahead rather
	// than being reported after the fact.
	//
	// Called concurrently from multiple worker goroutines, exactly like
	// OnEvent above: every retrying operation in a run calls Attempt with
	// this same RetryPolicy value, and Apply may have several retrying at
	// once. An implementation that is not itself safe for concurrent use
	// must serialize its own access — the same requirement OnEvent already
	// states, and for the same reason.
	//
	// One subtlety worth being explicit about: Apply gives each operation
	// its own RetryPolicy value (a plain copy, so its own wrapping of
	// OnRetry to also emit EventRetrying never races a sibling operation's
	// copy), but a copy of the struct does not copy what a func value
	// points to. If a caller's OnRetry closes over shared state — a
	// counter, a slice it appends to, anything mutable — that state is
	// still the ONE thing every retrying worker's copy of the policy calls
	// into concurrently. The per-operation struct copy isolates the field
	// itself; it does nothing for what the field, once called, touches.
	OnRetry func(attempt int, err error, delay time.Duration)
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
