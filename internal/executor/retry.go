package executor

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"github.com/infrena/infrena/pkg/provider"
)

// Verb identifies which provider call a retry attempt is making. Retry
// eligibility depends on which one it is (spec §15), so the loop needs to
// know explicitly rather than inferring it from the error.
//
// Verb is deliberately not planner.OpKind. A Replace does not name a single
// provider call — it is a destroy-phase node and a create-phase node,
// dispatched separately — and a Forget makes no provider call at all. The
// dispatch layer resolves one OpNode's (Kind, Phase) pair down to the
// single provider verb it actually invokes, and passes that verb here.
//
// VerbInvalid is deliberately the zero value. An uninitialized Verb must
// never come out permissive: VerbRead and VerbUpdate are the two verbs
// retryable() allows on ConditionallyRetryable, so a zero value landing on
// Read (as plain iota numbering would give it) would silently retry
// wherever a Verb was constructed but never set. VerbInvalid instead fails
// closed — see retryable().
type Verb uint8

const (
	// VerbInvalid is the zero value: an unset or unrecognized Verb. It is
	// never eligible for retry under any classification, including
	// SafeToRetry — see retryable().
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

const (
	defaultBase = 500 * time.Millisecond
	defaultMax  = 30 * time.Second
)

// retryable reports whether an attempt is eligible for another try, given
// which verb failed and how its provider classified the error. Spec §15:
//
//	| Verb   | NotSafeToRetry | ConditionallyRetryable | SafeToRetry |
//	|--------|----------------|------------------------|-------------|
//	| Read   | no             | yes                    | yes         |
//	| Update | no             | yes                    | yes         |
//	| Create | no             | no                     | yes         |
//	| Delete | no             | no                     | yes         |
//
// Create is never retried on an ambiguous failure: if the first attempt's
// provider call actually succeeded and only the response was lost, retrying
// creates a second real resource with nothing in state pointing at the
// first — a retried create is how duplicate infrastructure appears. Delete
// carries the same asymmetry in the other direction and gets the same
// answer: a conditionally-retryable delete might already have removed the
// object, and retrying risks acting on whatever now occupies that identity
// rather than confirming an idempotent no-op. Read and Update have no such
// asymmetry — both are naturally safe to repeat — so they retry on
// SafeToRetry and ConditionallyRetryable alike.
//
// provider.Retryability is a plain uint8, not a validated closed type —
// nothing stops a provider built against a future core, or one with a bug,
// from returning a value outside {NotSafeToRetry, ConditionallyRetryable,
// SafeToRetry}. Spec §15 puts the core in charge of the policy ("the
// provider classifies; the core owns backoff"), and the conservative policy
// for a classification the core does not recognize is never retry, for any
// verb — not silently reusing ConditionallyRetryable's answer, which is
// what falling through a catch-all default would do. So every known
// Retryability value is cased explicitly below, and anything else — an
// unrecognized value — takes the same branch as NotSafeToRetry.
//
// VerbInvalid gets the same fail-closed treatment independent of
// classification, including SafeToRetry: an uninitialized or unrecognized
// Verb reaching this function is itself a bug, and the safe answer to a bug
// is "do not retry," not "retry because the classification looked fine."
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
func backoff(policy RetryPolicy, attempt int) time.Duration {
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

// defaultSleep is RetryPolicy.Sleep's behavior when left nil: an ordinary
// wait that still honors context cancellation, so a SIGINT arriving during
// a retry delay does not have to wait out the full backoff before the
// executor can react to it.
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

// defaultJitter is RetryPolicy.Jitter's behavior when left nil: full
// jitter, a uniformly random duration in [0, d]. Spreading retries out this
// way is what stops every operation that failed at the same instant from
// waking up and hammering the provider a second time in lockstep.
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
// Attempt has no opinion of its own about what any particular error means,
// only about what to do with each of the three classifications for the
// given verb.
//
// If the wait between attempts is interrupted — policy.Sleep returning a
// non-nil error, which the default implementation does on context
// cancellation — Attempt stops immediately and returns that error wrapped
// around the attempt that was about to be retried, without making a
// further call to fn.
func Attempt(ctx context.Context, verb Verb, policy RetryPolicy, classify func(error) provider.Retryability, fn func() error) error {
	maxAttempts := max(policy.MaxAttempts, 1)

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if attempt == maxAttempts {
			break
		}
		if !retryable(verb, classify(lastErr)) {
			break
		}

		sleep := policy.Sleep
		if sleep == nil {
			sleep = defaultSleep
		}
		delay := backoff(policy, attempt)
		if policy.OnRetry != nil {
			// Before the wait, not after: a progress line saying "retrying in
			// 2s" is information; the same line printed once the 2s has
			// already elapsed is an apology.
			policy.OnRetry(attempt, lastErr, delay)
		}
		if err := sleep(ctx, delay); err != nil {
			return fmt.Errorf("%s: retry aborted while waiting to retry attempt %d: %w (attempt %d failed: %v)", verb, attempt+1, err, attempt, lastErr)
		}
	}
	return lastErr
}
