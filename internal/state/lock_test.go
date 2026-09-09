package state

import (
	"context"
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
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	const goroutines = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
		refused int
	)
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
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
				t.Errorf("unexpected lock error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted != 1 {
		t.Fatalf("%d goroutines acquired the lock, want exactly 1 — invariant 5", granted)
	}
	if refused != goroutines-1 {
		t.Errorf("%d goroutines saw ErrLocked, want %d", refused, goroutines-1)
	}
}

func TestUnlockUnheldEnvironmentIsAnError(t *testing.T) {
	b := NewLocal(t.TempDir())
	if err := b.Unlock(context.Background(), "dev"); err == nil {
		t.Error("unlocking an environment that is not locked must report that clearly rather than succeeding silently")
	}
}
