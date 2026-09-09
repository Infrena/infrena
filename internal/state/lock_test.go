package state

import (
	"context"
	"errors"
	"strings"
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

func TestUnlockUnheldEnvironmentIsAnError(t *testing.T) {
	b := NewLocal(t.TempDir())
	if err := b.Unlock(context.Background(), "dev"); err == nil {
		t.Error("unlocking an environment that is not locked must report that clearly rather than succeeding silently")
	}
}
