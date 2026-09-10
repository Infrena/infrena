package state

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"infra/pkg/address"
)

func TestGetMissingEnvironmentReturnsEmptyState(t *testing.T) {
	b := NewLocal(t.TempDir())
	s, err := b.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get on a fresh project: %v", err)
	}
	if len(s.Resources) != 0 {
		t.Error("a project that has never applied has no resources; this must not be an error")
	}
}

func TestPutThenGetRoundTrips(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	s := New("myapp", "dev")
	s.Set(sampleResource("db"))
	if err := b.Put(ctx, "dev", s); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := b.Get(ctx, "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got.Get(address.Address{Name: "db"}); !ok {
		t.Error("resource missing after Put/Get")
	}
}

func TestPutIncrementsSerial(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()
	s := New("myapp", "dev")

	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := b.Put(ctx, "dev", s); err != nil {
		t.Fatalf("Put: %v", err)
	}
	first := s.Serial
	if err := b.Put(ctx, "dev", s); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if s.Serial != first+1 {
		t.Errorf("Serial = %d, want %d — every write must advance the serial so plan staleness can be detected", s.Serial, first+1)
	}
}

func TestFailedPutDoesNotAdvanceSerial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permissions would not block the write")
	}

	root := t.TempDir()
	b := NewLocal(root)
	ctx := context.Background()
	s := New("myapp", "dev")

	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := b.Put(ctx, "dev", s); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	before := s.Serial

	// Make the state directory unwritable so the temp file cannot be created.
	stateDir := filepath.Join(root, "state")
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })

	if err := b.Put(ctx, "dev", s); err == nil {
		t.Fatal("Put into an unwritable directory should fail")
	}
	if s.Serial != before {
		t.Errorf("Serial = %d after a failed Put, want %d unchanged — a serial ahead of disk makes a retry skip a value and misreports staleness", s.Serial, before)
	}
}

func TestPutIsAtomicAndPrivate(t *testing.T) {
	root := t.TempDir()
	b := NewLocal(root)
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := b.Put(ctx, "dev", New("myapp", "dev")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	path := filepath.Join(root, "state", "dev.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %o, want 600 — state holds sensitive values (spec §9.3)", perm)
	}

	// Exactly two entries are expected in the state directory: the state
	// file Put wrote and the lock file held across it. os.ReadDir sorts by
	// filename, so this pins both the count and the identity of what
	// remains — a stray "dev.json.tmp" left behind alongside a missing
	// "dev.lock" would still pass a bare length check, which is why the
	// original version of this test (a bare len(entries) != 1) would not
	// have caught it.
	entries, err := os.ReadDir(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 || entries[0].Name() != "dev.json" || entries[1].Name() != "dev.lock" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("state directory contents = %v, want exactly [dev.json dev.lock]", names)
	}
}

func TestEnvironmentsAreIndependent(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	dev := New("myapp", "dev")
	dev.Set(sampleResource("db"))
	if err := b.Put(ctx, "dev", dev); err != nil {
		t.Fatalf("Put dev: %v", err)
	}

	prod, err := b.Get(ctx, "production")
	if err != nil {
		t.Fatalf("Get production: %v", err)
	}
	if len(prod.Resources) != 0 {
		t.Error("environments must have completely independent state (PLAN.md §6)")
	}
}

func TestPutWithoutLockRefuses(t *testing.T) {
	root := t.TempDir()
	b := NewLocal(root)
	ctx := context.Background()

	err := b.Put(ctx, "dev", New("myapp", "dev"))
	if err == nil {
		t.Fatal("Put without a held lock must refuse — invariant 5 must hold at the point state is written, not only where a caller happened to request a lock upstream")
	}
	if !errors.Is(err, ErrNotLocked) {
		t.Errorf("error = %v, want one wrapping ErrNotLocked", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "state", "dev.json")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Error("a refused Put must not have written a state file")
	}
}

func TestPutRefusesWhenLockedByAnotherProcess(t *testing.T) {
	root := t.TempDir()
	b := NewLocal(root)
	ctx := context.Background()

	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	held := `{"environment":"production","pid":999999,"host":"elsewhere","user":"someone","operation":"apply","at":"2026-09-09T10:00:00Z"}`
	if err := os.WriteFile(filepath.Join(stateDir, "production.lock"), []byte(held), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	err := b.Put(ctx, "production", New("myapp", "production"))
	if err == nil {
		t.Fatal("Put must refuse to write to an environment locked by a different process")
	}
	if !errors.Is(err, ErrNotLocked) {
		t.Errorf("error = %v, want one wrapping ErrNotLocked", err)
	}
}

func TestPutAfterUnlockRefuses(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := b.Unlock(ctx, "dev"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	err := b.Put(ctx, "dev", New("myapp", "dev"))
	if err == nil {
		t.Fatal("Put after the lock has been released must refuse — a caller must not keep writing once its lock is gone")
	}
	if !errors.Is(err, ErrNotLocked) {
		t.Errorf("error = %v, want one wrapping ErrNotLocked", err)
	}
}

func TestMultiplePutsSucceedUnderOneHeldLock(t *testing.T) {
	// refresh and apply both take the lock once and Put repeatedly across a
	// whole run (spec §15) — this is the shape that matters, not a single
	// Put immediately after a single Lock.
	b := NewLocal(t.TempDir())
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	s := New("myapp", "dev")
	for i := 0; i < 3; i++ {
		if err := b.Put(ctx, "dev", s); err != nil {
			t.Fatalf("Put #%d while holding the lock: %v", i+1, err)
		}
	}
	if s.Serial != 3 {
		t.Errorf("Serial = %d after 3 Puts under one lock, want 3", s.Serial)
	}
}
