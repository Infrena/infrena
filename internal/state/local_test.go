package state

import (
	"context"
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

func TestPutIsAtomicAndPrivate(t *testing.T) {
	root := t.TempDir()
	b := NewLocal(root)
	if err := b.Put(context.Background(), "dev", New("myapp", "dev")); err != nil {
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

	entries, err := os.ReadDir(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("temporary files were left behind: %v", entries)
	}
}

func TestEnvironmentsAreIndependent(t *testing.T) {
	b := NewLocal(t.TempDir())
	ctx := context.Background()

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
