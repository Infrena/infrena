// Package retry owns the backoff loop around a provider call. The provider
// classifies a failure; the core decides what to do about it.
package retry

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"github.com/infrena/infrena/pkg/provider"
)

// Verb identifies which provider call a retry attempt is making. Eligibility
// depends on which one it is, so the loop is told explicitly rather than
// inferring it from the error.
//
// It is deliberately not the planner's operation kind: a Replace is a destroy
// node and a create node dispatched separately, and a Forget makes no provider
// call at all. The dispatch layer resolves an operation down to the single verb
// it actually invokes and passes that here.
type Verb uint8

const (
	// VerbInvalid is the zero value: an unset or unrecognized Verb. It is never
	// eligible for retry under any classification, including SafeToRetry, so a
	// Verb that was constructed but never set cannot retry by accident.
	VerbInvalid Verb = iota
	// VerbRead is a provider Read call.
	VerbRead
	// VerbCreate is a provider Create call.
	VerbCreate
	// VerbUpdate is a provider Update call.
	VerbUpdate
	// VerbDelete is a provider Delete call.
	VerbDelete
)

// String names a verb for logging and test failure messages.
func (v Verb) String() string {
	switch v {
	case VerbInvalid:
		return "invalid"
	case VerbRead:
		return "read"
	case VerbCreate:
		return "create"
	case VerbUpdate:
		return "update"
	case VerbDelete:
		return "delete"
	default:
		return "verb(" + strconv.Itoa(int(v)) + ")"
	}
}

// Policy governs how a failed provider call is retried. It carries no behavior
// of its own; Attempt holds the loop that reads these fields.
//
// executor.RetryPolicy is an alias for this type.
type Policy struct {
	// MaxAttempts is the total number of attempts, including the first — not
	// the number of retries. Values below 1 mean 1: one attempt, no retry.
	MaxAttempts int
	// Base is the delay before the first retry. Each subsequent retry doubles
	// it, capped at Max.
	Base time.Duration
	// Max caps the backoff delay, however many attempts have elapsed.
	Max time.Duration
	// Sleep waits out one backoff delay; nil means the real implementation.
	// It is injectable so tests need not sleep, and because the real one
	// returns early on context cancellation, which is how a SIGINT during a
	// retry delay interrupts the wait instead of blocking until it elapses.
	Sleep func(context.Context, time.Duration) error
	// Jitter perturbs a computed backoff delay before it is used, so many
	// operations that failed at the same instant do not all retry in lockstep.
	// nil means the real implementation; tests pass the identity function to
	// assert exact delays.
	Jitter func(time.Duration) time.Duration
	// OnRetry is called once per retry, synchronously and immediately before
	// the backoff wait begins, so a progress line reaches the user while the
	// delay is still ahead. attempt is the 1-based attempt that just failed,
	// err is why, and delay is how long the wait will be. nil means no
	// notification.
	//
	// It exists because this loop is the only place that knows all three facts
	// at the moment they are true; reconstructing them from outside would mean
	// guessing at the backoff schedule.
	//
	// Called concurrently from several worker goroutines, so an implementation
	// that is not safe for concurrent use must serialize its own access. Each
	// operation gets its own copy of the Policy struct, but a copy of a func
	// value does not copy the state it closes over: shared state inside
	// OnRetry is still shared.
	OnRetry func(attempt int, err error, delay time.Duration)
}

const (
	defaultBase = 500 * time.Millisecond
	defaultMax  = 30 * time.Second
)

// retryable reports whether an attempt is eligible for another try, given
// which verb failed and how its provider classified the error:
//
//	| Verb   | NotSafeToRetry | ConditionallyRetryable | SafeToRetry |
//	|--------|----------------|------------------------|-------------|
//	| Read   | no             | yes                    | yes         |
//	| Update | no             | yes                    | yes         |
//	| Create | no             | no                     | yes         |
//	| Delete | no             | no                     | yes         |
//
// Create is never retried on an ambiguous failure: if the call succeeded and
// only the response was lost, retrying creates a second real resource with
// nothing in state pointing at the first. A conditionally-retryable Delete may
// already have removed the object, so retrying risks acting on whatever now
// occupies that identity. Read and Update are naturally safe to repeat.
//
// provider.Retryability is a plain uint8, so nothing stops a provider built
// against a future core from returning a value outside the three. Every known
// value is cased explicitly and anything else joins NotSafeToRetry, because
// falling through a catch-all default would silently grant it
// ConditionallyRetryable's answer. VerbInvalid fails closed the same way, under
// every classification.
func retryable(verb Verb, r provider.Retryability) bool {
	if verb == VerbInvalid {
		return false
	}
	switch r {
	case provider.SafeToRetry:
		return true
	case provider.NotSafeToRetry:
		return false
	case provider.ConditionallyRetryable:
		return verb == VerbRead || verb == VerbUpdate
	default:
		// Unrecognized classification: fail closed for every verb.
		return false
	}
}

// backoff computes the delay before retrying after the given attempt
// (1-based: attempt 1 is the first failure), doubling from Base each time
// and capped at Max, then passed through Jitter. Base and Max fall back to
// package defaults when the policy leaves them at the zero value, and
// Jitter falls back to full jitter — a uniform random duration in [0, d].
func backoff(policy Policy, attempt int) time.Duration {
	base := policy.Base
	if base <= 0 {
		base = defaultBase
	}
	max := policy.Max
	if max <= 0 {
		max = defaultMax
	}

	delay := base
	for i := 1; i < attempt && delay < max; i++ {
		delay *= 2
	}
	if delay > max {
		delay = max
	}

	jitter := policy.Jitter
	if jitter == nil {
		jitter = defaultJitter
	}
	return jitter(delay)
}

// defaultSleep is Policy.Sleep's behavior when left nil: an ordinary wait that
// still honors context cancellation, so a SIGINT arriving during a retry delay
// does not have to wait out the full backoff.
func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// defaultJitter is Policy.Jitter's behavior when left nil: full jitter, a
// uniformly random duration in [0, d], so operations that failed at the same
// instant do not hammer the provider again in lockstep.
func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(d) + 1))
}

// Attempt runs fn, retrying it according to policy for as long as verb and
// classify(err) say the failure is eligible (see retryable) and attempts
// remain. It returns nil the moment fn succeeds, and otherwise the error
// from the last attempt once attempts are exhausted or a non-retryable
// failure is hit.
//
// classify is how a caller wires in the owning provider's ClassifyError:
// Attempt has no opinion about what any particular error means, only about what
// to do with each classification for the given verb.
//
// If the wait between attempts is interrupted — policy.Sleep returning a
// non-nil error, which the default implementation does on context cancellation
// — Attempt stops immediately and returns that error wrapped around the attempt
// that was about to be retried, without calling fn again.
func Attempt(ctx context.Context, verb Verb, policy Policy, classify func(error) provider.Retryability, fn func() error) error {
	maxAttempts := max(policy.MaxAttempts, 1)

	var lastErr error
	// waited is accumulated rather than recomputed from the schedule, because
	// jitter means the schedule is not what happened.
	var waited time.Duration
	spent := 0
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		spent = attempt
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if attempt == maxAttempts {
			break
		}
		if !retryable(verb, classify(lastErr)) {
			// Returned unwrapped: nothing was retried, and "after 1 attempt"
			// on a failure that was refused outright reads as though waiting
			// might have helped.
			return lastErr
		}

		sleep := policy.Sleep
		if sleep == nil {
			sleep = defaultSleep
		}
		delay := backoff(policy, attempt)
		if policy.OnRetry != nil {
			// Before the wait, not after: "retrying in 2s" is information,
			// while the same line after the 2s has elapsed is an apology.
			policy.OnRetry(attempt, lastErr, delay)
		}
		if err := sleep(ctx, delay); err != nil {
			return fmt.Errorf("%s: retry aborted while waiting to retry attempt %d: %w (attempt %d failed: %v)", verb, attempt+1, err, attempt, lastErr)
		}
		waited += delay
	}
	if spent > 1 {
		// Without this an exhausted retry and a single failure print the same
		// words, and the reader cannot tell whether the engine waited at all.
		// The cause is wrapped, not replaced, so errors.Is and errors.As on
		// the way out still work.
		return fmt.Errorf("after %d attempts over %s: %w", spent, waited.Round(time.Millisecond), lastErr)
	}
	return lastErr
}
