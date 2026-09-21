package state

import (
	"context"
	"testing"

	"github.com/infrena/infrena/pkg/backend"
)

// The CLI calls List, Inspect and ForceUnlock on the concrete *Local. Until
// they are on the interface, backendFor cannot return a Backend, and a plugin
// can never stand in for local.
func TestLocalSatisfiesTheWidenedBackendInterface(t *testing.T) {
	var _ Backend = (*Local)(nil)
}

// Inspect and ForceUnlock are synchronous on *Local. Over a wire they can
// block, so they take a context. Asserted here because the signature change
// reaches every caller and must not be quietly skipped for local.
func TestInspectAndForceUnlockTakeAContext(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	ctx := context.Background()

	if _, err := l.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	held, ok, err := l.Inspect(ctx, "dev")
	if err != nil || !ok {
		t.Fatalf("Inspect = %v, %v, %v", held, ok, err)
	}
	if held.Environment != "dev" {
		t.Errorf("Environment = %q, want dev", held.Environment)
	}
	if err := l.ForceUnlock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l.Inspect(ctx, "dev"); ok {
		t.Error("lock survived ForceUnlock")
	}
}

// A backend author cannot implement locking against a type they cannot
// import, so the lock type is public. The alias keeps every existing
// reference to state.Lock compiling.
func TestStateLockIsTheSameTypeAsBackendLock(t *testing.T) {
	var a Lock
	var b backend.Lock
	a = b
	b = a
	_ = a
	_ = b
}
