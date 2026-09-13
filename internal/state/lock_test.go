package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSecondLockOnSameEnvironmentFails(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, err := b.Lock(ctx, "production"); err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	_, err := b.Lock(ctx, "production")
	if err == nil {
		t.Fatal("a second lock on the same environment must fail — invariant 5")
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("error = %v, want one wrapping ErrLocked", err)
	}
}

func TestLockErrorNamesTheHolder(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()
	if _, err := b.Lock(ctx, "production"); err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	_, err := b.Lock(ctx, "production")
	if err == nil {
		t.Fatal("expected a conflict")
	}
	msg := err.Error()
	for _, want := range []string{"held by", "pid"} {
		if !strings.Contains(strings.ToLower(msg), want) {
			t.Errorf("conflict message %q does not say %q — it must identify who holds the lock (spec §9.2)", msg, want)
		}
	}
}

func TestDifferentEnvironmentsLockIndependently(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, err := b.Lock(ctx, "production"); err != nil {
		t.Fatalf("Lock production: %v", err)
	}
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Errorf("Lock dev while production is locked: %v — concurrent applies to different environments are allowed", err)
	}
}

func TestUnlockThenRelock(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := b.Unlock(ctx, "dev"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Errorf("relock after unlock: %v", err)
	}
}

func TestInspectDescribesTheLock(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, held, err := b.Inspect("dev"); err != nil || held {
		t.Fatalf("Inspect before locking = held %v, err %v", held, err)
	}
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	lock, held, err := b.Inspect("dev")
	if err != nil || !held {
		t.Fatalf("Inspect after locking = held %v, err %v", held, err)
	}
	if lock.PID == 0 || lock.Host == "" || lock.Environment != "dev" {
		t.Errorf("lock descriptor is incomplete: %#v", lock)
	}
}

func TestUnlockRefusesAnotherProcessesLockButForceSucceeds(t *testing.T) {
	root := t.TempDir()
	b := NewLocal(root)
	ctx := context.Background()

	// Write a lock file as if another process holds it.
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	held := `{"environment":"production","pid":999999,"host":"elsewhere","user":"someone","operation":"apply","at":"2026-09-09T10:00:00Z"}`
	if err := os.WriteFile(filepath.Join(stateDir, "production.lock"), []byte(held), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	if err := b.Unlock(ctx, "production"); err == nil {
		t.Error("Unlock must refuse a lock held by another process, or `force` means nothing")
	}
	if err := b.ForceUnlock("production"); err != nil {
		t.Errorf("ForceUnlock should override: %v", err)
	}
	if _, ok, _ := b.Inspect("production"); ok {
		t.Error("the lock should be gone after ForceUnlock")
	}
}

func TestConcurrentLockAttemptsElectExactlyOneWinner(t *testing.T) {
	// The other lock tests are single-goroutine: they verify the observable
	// contract, but a naive stat-then-create implementation would pass every
	// one of them. This is the only test that actually exercises the race
	// O_EXCL exists to prevent, and so the only direct proof of invariant 5.
	//
	// A single election is a coin flip against a genuine stat-then-create
	// TOCTOU: mutating Lock to that shape and running this test at
	// -count=10 landed FAIL/ok/ok/FAIL/ok — a real atomicity bug detected
	// only 2 times in 5. One election is not evidence; the fix, same as the
	// map-iteration flakiness measured elsewhere this week, is to make the
	// property something the test measures across many independent trials
	// rather than assumes from one. 50 rounds, each releasing the lock
	// before the next: missing a bug this test catches 40% of the time,
	// across 50 independent rounds, is 0.6^50 — vanishing. Do not reduce
	// this loop back to a single round; that is the exact defect this
	// comment exists to prevent recurring.
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	const goroutines = 16
	const rounds = 50

	for round := 1; round <= rounds; round++ {
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			granted int
			refused int
		)
		start := make(chan struct{})

		for range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // release together, to maximise contention
				_, err := b.Lock(ctx, "production")

				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					granted++
				case errors.Is(err, ErrLocked):
					refused++
				default:
					t.Errorf("round %d: unexpected lock error: %v", round, err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if granted != 1 {
			t.Fatalf("round %d: %d goroutines acquired the lock, want exactly 1 — invariant 5", round, granted)
		}
		if refused != goroutines-1 {
			t.Errorf("round %d: %d goroutines saw ErrLocked, want %d", round, refused, goroutines-1)
		}

		// Release before the next round contends for the same environment.
		// ForceUnlock, not Unlock: this goroutine's own PID never actually
		// took the lock (one of the 16 spawned goroutines did, and which
		// one is not tracked), so a PID-checked Unlock would refuse it.
		if err := b.ForceUnlock("production"); err != nil {
			t.Fatalf("round %d: ForceUnlock: %v", round, err)
		}
	}
}

func TestUnlockUnheldEnvironmentIsAnError(t *testing.T) {
	b := NewLocal(t.TempDir())
	if err := b.Unlock(context.Background(), "dev"); err == nil {
		t.Error("unlocking an environment that is not locked must report that clearly rather than succeeding silently")
	}
}

// TestConcurrentInspectNeverObservesAPartiallyWrittenLock pins the atomicity
// that O_EXCL alone does not provide: O_EXCL makes ACQUISITION atomic
// (exactly one caller creates the file), but if the file's content is
// written after creation, there is a window where the file exists and is
// empty. A concurrent Inspect landing in that window reads zero bytes and
// reports "lock file is malformed: unexpected end of JSON input" — a real,
// reachable failure, since Lock itself calls Inspect on the fs.ErrExist
// path to name the holder for a conflicting caller.
//
// Inspect has exactly two correct outcomes: no lock (not yet created) or a
// complete lock (fully written). A malformed-JSON error is a third outcome
// that must never happen, no matter how the two calls interleave.
//
// A single round is not evidence either way — the window is a handful of
// instructions wide. This spins many goroutines with no backoff against
// many independent rounds so that, if the window exists, something lands
// in it: see TestConcurrentLockAttemptsElectExactlyOneWinner above for the
// same reasoning applied to acquisition instead of content.
func TestConcurrentInspectNeverObservesAPartiallyWrittenLock(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	const inspectors = 16
	const rounds = 200

	for round := 1; round <= rounds; round++ {
		var wg sync.WaitGroup
		stop := make(chan struct{})

		for range inspectors {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					_, _, err := b.Inspect("production")
					if err == nil {
						continue
					}
					var syntaxErr *json.SyntaxError
					if errors.As(err, &syntaxErr) {
						t.Errorf("round %d: Inspect observed a partially written lock file: %v", round, err)
						return
					}
				}
			}()
		}

		if _, err := b.Lock(ctx, "production"); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("round %d: Lock: %v", round, err)
		}
		close(stop)
		wg.Wait()

		if err := b.ForceUnlock("production"); err != nil {
			t.Fatalf("round %d: ForceUnlock: %v", round, err)
		}
	}
}

// TestSecondLockStillRefusedAndNamesHolderAfterLinkFix guards the failure
// mode a naive fix for the above would introduce: replacing the O_EXCL
// create with write-temp-then-os.Rename, mirroring Put's pattern in
// local.go. Rename OVERWRITES its target, so a second Lock would silently
// replace the first holder's lock file instead of being refused — two
// concurrent applies would both believe they hold the lock, which is
// exactly what invariant 5 (spec §47.5) forbids. The correct primitive,
// os.Link, preserves O_EXCL-like exclusivity (Link fails with EEXIST if the
// target exists) while still writing content atomically.
//
// This is not a new behavioural contract — TestSecondLockOnSameEnvironmentFails
// and TestLockErrorNamesTheHolder already pin it — but it is written again
// here, deliberately, pinned to the fixed (Link-based) implementation, so
// that a future rewrite of Lock's atomicity trips over it immediately
// rather than relying on someone remembering this file's history.
func TestSecondLockStillRefusedAndNamesHolderAfterLinkFix(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, err := b.Lock(ctx, "production"); err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	_, err := b.Lock(ctx, "production")
	if err == nil {
		t.Fatal("a second Lock on a held environment must fail — invariant 5")
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("error = %v, want one wrapping ErrLocked", err)
	}
	msg := strings.ToLower(err.Error())
	for _, want := range []string{"held by", "pid"} {
		if !strings.Contains(msg, want) {
			t.Errorf("conflict message %q does not say %q — it must identify who holds the lock (spec §9.2)", err.Error(), want)
		}
	}

	// The first holder's lock must still be intact, not overwritten.
	held, ok, err := b.Inspect("production")
	if err != nil || !ok {
		t.Fatalf("Inspect after refused second Lock: held %v, err %v", ok, err)
	}
	if held.PID != os.Getpid() {
		t.Errorf("lock holder PID = %d, want this process's own %d — the original lock must not have been replaced", held.PID, os.Getpid())
	}
}
