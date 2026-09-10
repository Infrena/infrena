package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"infra/pkg/provider"
)

func alwaysSafe(error) provider.Retryability        { return provider.SafeToRetry }
func alwaysConditional(error) provider.Retryability { return provider.ConditionallyRetryable }

func TestRetryableTableMatchesSpec(t *testing.T) {
	cases := []struct {
		verb Verb
		r    provider.Retryability
		want bool
	}{
		{VerbRead, provider.NotSafeToRetry, false},
		{VerbRead, provider.ConditionallyRetryable, true},
		{VerbRead, provider.SafeToRetry, true},

		{VerbUpdate, provider.NotSafeToRetry, false},
		{VerbUpdate, provider.ConditionallyRetryable, true},
		{VerbUpdate, provider.SafeToRetry, true},

		{VerbCreate, provider.NotSafeToRetry, false},
		{VerbCreate, provider.ConditionallyRetryable, false},
		{VerbCreate, provider.SafeToRetry, true},

		{VerbDelete, provider.NotSafeToRetry, false},
		{VerbDelete, provider.ConditionallyRetryable, false},
		{VerbDelete, provider.SafeToRetry, true},
	}
	for _, tc := range cases {
		if got := retryable(tc.verb, tc.r); got != tc.want {
			t.Errorf("retryable(%s, %d) = %v, want %v", tc.verb, tc.r, got, tc.want)
		}
	}
}

// TestRetryableFailsClosedOnUnrecognizedClassification pins the fix for
// review finding #1 (task-5 fix round 1): provider.Retryability is a plain
// uint8, not a validated closed type, so a provider built against a future
// core — or one with a bug — could return a value outside
// {NotSafeToRetry, ConditionallyRetryable, SafeToRetry}. Before this fix,
// retryable's catch-all default treated any such value identically to
// ConditionallyRetryable, which meant Read and Update would silently retry
// on a classification nobody defined. The core owns the policy (spec §15),
// and the conservative policy for an unrecognized classification is never
// retry, for every verb — including Read and Update, which have no
// Create/Delete-style asymmetry to fall back on for protection.
func TestRetryableFailsClosedOnUnrecognizedClassification(t *testing.T) {
	const unrecognized provider.Retryability = 99
	for _, verb := range []Verb{VerbRead, VerbCreate, VerbUpdate, VerbDelete} {
		if got := retryable(verb, unrecognized); got != false {
			t.Errorf("retryable(%s, unrecognized=99) = %v, want false — an unrecognized classification must fail closed, not be treated as ConditionallyRetryable", verb, got)
		}
	}
}

// TestRetryableRefusesVerbInvalid pins the fix for review finding #2 (task-5
// fix round 1): VerbInvalid — the zero value of Verb — must never be
// eligible for retry, under any classification including SafeToRetry. An
// uninitialized or unrecognized Verb reaching retryable is itself a bug,
// and the safe answer to that bug is "do not retry," not "retry because the
// classification looked fine." This matters because Verb's zero value
// would otherwise number the same as VerbRead under plain iota, which is
// the most permissive verb, not the least.
func TestRetryableRefusesVerbInvalid(t *testing.T) {
	for _, r := range []provider.Retryability{provider.NotSafeToRetry, provider.ConditionallyRetryable, provider.SafeToRetry, 99} {
		if got := retryable(VerbInvalid, r); got != false {
			t.Errorf("retryable(VerbInvalid, %d) = %v, want false", r, got)
		}
	}
}

func TestBackoffDoublesThenCapsAtMax(t *testing.T) {
	policy := RetryPolicy{
		Base:   time.Second,
		Max:    3 * time.Second,
		Jitter: func(d time.Duration) time.Duration { return d }, // identity, so exact values are checkable
	}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 3 * time.Second}, // would be 4s uncapped
		{4, 3 * time.Second}, // stays capped
	}
	for _, tc := range cases {
		if got := backoff(policy, tc.attempt); got != tc.want {
			t.Errorf("backoff(attempt=%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestAttemptSucceedsWithoutRetryingOnFirstSuccess(t *testing.T) {
	calls := 0
	slept := 0
	policy := RetryPolicy{
		MaxAttempts: 5,
		Sleep:       func(context.Context, time.Duration) error { slept++; return nil },
	}
	err := Attempt(context.Background(), VerbRead, policy, alwaysSafe, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want 1", calls)
	}
	if slept != 0 {
		t.Errorf("Sleep called %d times, want 0 — nothing failed", slept)
	}
}

func TestAttemptWithZeroMaxAttemptsMeansOne(t *testing.T) {
	calls := 0
	err := Attempt(context.Background(), VerbRead, RetryPolicy{}, alwaysSafe, func() error {
		calls++
		return errors.New("fails")
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want exactly 1 — MaxAttempts below 1 means exactly one attempt, no retry", calls)
	}
}

// TestAttemptWithNegativeMaxAttemptsMeansOne covers the negative half of
// "MaxAttempts below 1 means 1" (doubt #4 in the task-5 brief asked for
// both 0 and a negative value explicitly; the zero case alone was the only
// one pinned before this fix round). The `< 1` guard in Attempt makes 0 and
// a negative value provably the same branch, so this is not expected to
// catch a different bug than the zero case does — it is here so the stated
// coverage matches what was asked for, not left as an unverified inference.
func TestAttemptWithNegativeMaxAttemptsMeansOne(t *testing.T) {
	calls := 0
	err := Attempt(context.Background(), VerbRead, RetryPolicy{MaxAttempts: -5}, alwaysSafe, func() error {
		calls++
		return errors.New("fails")
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want exactly 1 — a negative MaxAttempts means exactly one attempt, no retry, same as 0", calls)
	}
}

func TestAttemptNeverRetriesCreateOnAnAmbiguousFailure(t *testing.T) {
	// The rule that matters most: a retried create is how duplicate
	// infrastructure appears. calls must stop at 1 even though MaxAttempts
	// allows far more, and Sleep must never be reached at all.
	calls := 0
	boom := errors.New("ambiguous: timeout waiting for response")
	policy := RetryPolicy{
		MaxAttempts: 5,
		Sleep: func(context.Context, time.Duration) error {
			t.Fatal("Sleep must not be called — Create must not retry on ConditionallyRetryable")
			return nil
		},
	}
	err := Attempt(context.Background(), VerbCreate, policy, alwaysConditional, func() error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want exactly 1", calls)
	}
}

func TestAttemptRetriesCreateOnlyWhenSafeToRetry(t *testing.T) {
	calls := 0
	policy := RetryPolicy{
		MaxAttempts: 5,
		Sleep:       func(context.Context, time.Duration) error { return nil },
	}
	err := Attempt(context.Background(), VerbCreate, policy, alwaysSafe, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if calls != 3 {
		t.Errorf("fn called %d times, want 3 — SafeToRetry must retry Create", calls)
	}
}

func TestAttemptRetriesDeleteOnlyWhenSafeToRetry(t *testing.T) {
	calls := 0
	boom := errors.New("dependency still attached")
	err := Attempt(context.Background(), VerbDelete, RetryPolicy{MaxAttempts: 5}, alwaysConditional, func() error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want exactly 1 — Delete must not retry on ConditionallyRetryable", calls)
	}

	calls = 0
	policy := RetryPolicy{MaxAttempts: 5, Sleep: func(context.Context, time.Duration) error { return nil }}
	err = Attempt(context.Background(), VerbDelete, policy, alwaysSafe, func() error {
		calls++
		if calls < 2 {
			return errors.New("not found yet, propagating")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if calls != 2 {
		t.Errorf("fn called %d times, want 2 — SafeToRetry must retry Delete", calls)
	}
}

func TestAttemptRetriesUpdateOnConditionallyRetryable(t *testing.T) {
	calls := 0
	policy := RetryPolicy{MaxAttempts: 5, Sleep: func(context.Context, time.Duration) error { return nil }}
	err := Attempt(context.Background(), VerbUpdate, policy, alwaysConditional, func() error {
		calls++
		if calls < 3 {
			return errors.New("throttled")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if calls != 3 {
		t.Errorf("fn called %d times, want 3 — ConditionallyRetryable must retry Update", calls)
	}
}

func TestAttemptStopsAtMaxAttemptsWithoutASleepAfterTheLastFailure(t *testing.T) {
	calls := 0
	slept := 0
	boom := errors.New("still failing")
	policy := RetryPolicy{
		MaxAttempts: 3,
		Sleep:       func(context.Context, time.Duration) error { slept++; return nil },
	}
	err := Attempt(context.Background(), VerbRead, policy, alwaysSafe, func() error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if calls != 3 {
		t.Errorf("fn called %d times, want exactly 3 (MaxAttempts)", calls)
	}
	if slept != 2 {
		t.Errorf("Sleep called %d times, want 2 — between attempts 1-2 and 2-3, never after the final failed attempt", slept)
	}
}

func TestAttemptStopsWhenSleepIsInterrupted(t *testing.T) {
	calls := 0
	boom := errors.New("throttled")
	policy := RetryPolicy{
		MaxAttempts: 5,
		Sleep:       func(context.Context, time.Duration) error { return context.Canceled },
	}
	err := Attempt(context.Background(), VerbRead, policy, alwaysSafe, func() error {
		calls++
		return boom
	})
	if err == nil {
		t.Fatal("Attempt must return an error when Sleep is interrupted")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want one wrapping context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want exactly 1 — Sleep never returned, so no second attempt should have been made", calls)
	}
}

// TestDefaultJitterStaysWithinZeroToD bounds-checks defaultJitter's output
// and additionally requires more than one distinct value across 50 calls
// (fix round 1, Minor #2). A bounds-only check would also pass a fixed,
// non-random implementation — e.g. one that always returns d, or always
// returns 0 — since both are within [0, d]; that implementation would
// silently defeat the point of jitter (spreading retries out so operations
// that failed at the same instant do not all wake up and hammer the
// provider in lockstep) while this test stayed green. The distinctness
// check is not flaky in practice: defaultJitter draws uniformly from a
// nanosecond-resolution range of 100ms, so the odds of 50 independent draws
// all landing on the same value are negligible.
func TestDefaultJitterStaysWithinZeroToD(t *testing.T) {
	d := 100 * time.Millisecond
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		got := defaultJitter(d)
		if got < 0 || got > d {
			t.Fatalf("defaultJitter(%v) = %v, want within [0, %v]", d, got, d)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Errorf("defaultJitter(%v) returned %d distinct value(s) across 50 calls, want more than 1 — a fixed implementation (e.g. always returning d, or always 0) would pass a bounds-only check", d, len(seen))
	}
}

func TestDefaultSleepReturnsContextErrOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := defaultSleep(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("defaultSleep on an already-cancelled context = %v, want context.Canceled", err)
	}
}

// TestAttemptNotifiesEachRetryBeforeWaiting pins OnRetry, which Task 8 uses to
// emit EventRetrying. Three properties, and the third is the one a careless
// implementation gets wrong.
//
// It fires once per RETRY, not once per attempt — three attempts means two
// retries. It reports the attempt that just failed and the error that failed
// it, not the one about to be tried. And it is called BEFORE the wait, so a
// user learns a retry is coming while the delay is still ahead of them; called
// after, the same line is an apology for a pause already endured.
func TestAttemptNotifiesEachRetryBeforeWaiting(t *testing.T) {
	type note struct {
		attempt int
		err     error
		delay   time.Duration
		slept   int // how many sleeps had completed when this fired
	}
	var notes []note
	slept := 0

	boom := errors.New("throttled")
	policy := RetryPolicy{
		MaxAttempts: 3,
		Base:        10 * time.Millisecond,
		Max:         time.Second,
		Jitter:      func(d time.Duration) time.Duration { return d },
		Sleep: func(context.Context, time.Duration) error {
			slept++
			return nil
		},
	}
	policy.OnRetry = func(attempt int, err error, delay time.Duration) {
		notes = append(notes, note{attempt, err, delay, slept})
	}

	calls := 0
	err := Attempt(context.Background(), VerbRead, policy,
		func(error) provider.Retryability { return provider.SafeToRetry },
		func() error { calls++; return boom })

	if !errors.Is(err, boom) {
		t.Fatalf("Attempt = %v, want the underlying error", err)
	}
	if calls != 3 {
		t.Fatalf("fn called %d times, want 3 (MaxAttempts)", calls)
	}
	if len(notes) != 2 {
		t.Fatalf("OnRetry fired %d times, want 2 — once per retry, not once per attempt", len(notes))
	}
	for i, n := range notes {
		wantAttempt := i + 1
		if n.attempt != wantAttempt {
			t.Errorf("notes[%d].attempt = %d, want %d (the attempt that just failed)", i, n.attempt, wantAttempt)
		}
		if !errors.Is(n.err, boom) {
			t.Errorf("notes[%d].err = %v, want the failure that triggered the retry", i, n.err)
		}
		if n.delay <= 0 {
			t.Errorf("notes[%d].delay = %v, want the backoff about to be waited", i, n.delay)
		}
		// Called before the wait: at the moment notes[i] fired, exactly i
		// sleeps had completed. If OnRetry were called after sleeping, this
		// would be i+1.
		if n.slept != i {
			t.Errorf("notes[%d] fired after %d sleeps, want %d — OnRetry must be called BEFORE the wait", i, n.slept, i)
		}
	}
}

// TestAttemptWithNoOnRetryDoesNotPanic guards the nil case, since Options may
// legitimately omit it.
func TestAttemptWithNoOnRetryDoesNotPanic(t *testing.T) {
	policy := RetryPolicy{
		MaxAttempts: 2,
		Base:        time.Millisecond,
		Sleep:       func(context.Context, time.Duration) error { return nil },
	}
	_ = Attempt(context.Background(), VerbRead, policy,
		func(error) provider.Retryability { return provider.SafeToRetry },
		func() error { return errors.New("boom") })
}
