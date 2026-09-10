package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"infra/internal/planner"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
)

func TestApplyPersistsStateAfterEveryOperationNotJustAtTheEnd(t *testing.T) {
	dir := t.TempDir()
	backend := state.NewLocal(dir)
	if _, err := backend.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	prov := &countingProvider{resourceType: "test.thing", delay: 40 * time.Millisecond}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var ops []planner.Operation
	for i := 0; i < 3; i++ {
		ops = append(ops, op(addr(fmt.Sprintf("r%d", i)), "test.thing", planner.OpCreate))
	}
	plan := planWith(ops...)
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	done := make(chan struct{})
	go func() {
		defer close(done)
		Apply(context.Background(), plan, g, st, Options{
			// Parallelism: 1 forces the three creates to run strictly one
			// after another, ~40ms apart, so polling below has a real
			// window to catch a partially-written file.
			Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		})
	}()

	var sawIntermediate bool
	deadline := time.After(3 * time.Second)
poll:
	for {
		select {
		case <-done:
			break poll
		case <-deadline:
			t.Fatal("Apply did not finish in time")
		default:
		}
		onDisk, err := backend.Get(context.Background(), "dev")
		if err == nil && onDisk.Serial > 0 && len(onDisk.Resources) > 0 && len(onDisk.Resources) < 3 {
			sawIntermediate = true
			break poll
		}
		time.Sleep(2 * time.Millisecond)
	}
	<-done

	if !sawIntermediate {
		t.Fatal("never observed a partially-written state file while Apply was running — state must be persisted after every operation, not batched until the end")
	}

	final, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Serial != 3 {
		t.Errorf("final Serial = %d, want 3 — one Put per operation", final.Serial)
	}
	if len(final.Resources) != 3 {
		t.Errorf("final resource count = %d, want 3", len(final.Resources))
	}
}

func TestApplyStopsSchedulingAfterAPersistFailureButKeepsAccurateState(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions; this test needs a real write failure")
	}

	dir := t.TempDir()
	backend := state.NewLocal(dir)
	if _, err := backend.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Local.Put creates its temp file inside <root>/state (internal/state/local.go),
	// so removing write permission there makes every subsequent Put fail —
	// Options.Backend is a concrete *state.Local, not an interface, so
	// there is no double to substitute for a controlled failure.
	stateDir := filepath.Join(dir, "state")
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(stateDir, 0o700) })

	prov := &countingProvider{resourceType: "test.thing"}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// root has one dependent, child: if the run kept scheduling after
	// root's Put failed, child — which becomes ready only once root's
	// completion unlocks it, exactly the sequence a real dependency graph
	// produces — would still get created. Its absence is what proves
	// scheduling actually stopped, not merely that a diagnostic came back.
	plan := planWith(
		op(addr("root"), "test.thing", planner.OpCreate),
		op(addr("child"), "test.thing", planner.OpCreate),
	)
	g, err := planner.BuildExecution(plan, depsFrom(map[string][]string{"root": {"child"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
	})

	if !ds.HasErrors() {
		t.Fatal("expected a diagnostic reporting the persistence failure")
	}
	if _, ok := st.Get(addr("root")); !ok {
		t.Error("root's in-memory state must still reflect the successful create — a Put failure must not roll back an accurate mutation")
	}
	if _, ok := st.Get(addr("child")); ok {
		t.Error("child must not have been created — the run must stop scheduling new work once persistence has failed")
	}
	if result.State != st {
		t.Error("Result.State must be the same *state.State the run mutated, Put failure or not")
	}
	if !reflect.DeepEqual(result.Applied, []address.Address{addr("root")}) {
		t.Errorf("Applied = %v, want exactly [root]", result.Applied)
	}
}
