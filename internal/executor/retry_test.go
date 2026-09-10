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

func TestDefaultJitterStaysWithinZeroToD(t *testing.T) {
	d := 100 * time.Millisecond
	for i := 0; i < 50; i++ {
		got := defaultJitter(d)
		if got < 0 || got > d {
			t.Fatalf("defaultJitter(%v) = %v, want within [0, %v]", d, got, d)
		}
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
