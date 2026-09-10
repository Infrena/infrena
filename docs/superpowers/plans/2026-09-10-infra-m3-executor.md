# M3: Executor, apply, destroy, refresh — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the reconcile loop. `infra apply`, `infra destroy` and `infra refresh` work end to end against the fake provider, and acceptance invariants 4 (dependency ordering) and 5 (locking) become demonstrable rather than aspirational.

**Architecture:** A worker pool drains a ready queue of operations whose predecessors have completed, bounded twice — globally by `--parallelism` and per provider by a semaphore. State is persisted after every operation under one lock held for the whole run, so a crash leaves state describing reality. A failure stops its branch, not the world. Deferred expressions resolve at apply time using the compiler's own evaluator.

**Tech Stack:** Go 1.24, Cobra, `gopkg.in/yaml.v3`. No other third-party dependency.

**Spec:** `docs/superpowers/specs/2026-09-09-infra-phase-1-design.md` — §15 is the executor, §9.2 locking, §10 refresh, §12.2 apply, §13 protections, §16 CLI. The spec is the authority; where this plan and the spec disagree, the spec wins and the disagreement is a defect in this plan.

## Global Constraints

Every task's requirements implicitly include this section.

- **Go 1.24.** `mise` is inactive in non-interactive shells: use the `make` targets, or `export PATH="$HOME/.local/share/mise/shims:$PATH"` first. A bare `go` resolves to 1.20 and fails.
- **Exactly two third-party dependencies**: `github.com/spf13/cobra` and `gopkg.in/yaml.v3`. Do not run `go get`.
- **Layering.** `internal/graph` imports no other `infra` package — that leaf property is why it exists. `internal/executor`, `internal/planner`, `internal/compiler` and `pkg/*` must not import `internal/cli`. Nothing in the core engine imports any AWS SDK. `providers/*` import no `internal/*`.
- **Sensitivity is per-leaf.** Anything rendering a value goes through `value.Format`. There is exactly one redaction path in this codebase; a second one is how a secret leaked in M2, twice.
- **Determinism.** Every map-to-slice boundary sorts. Concurrency must not make output order depend on completion order.
- **State is persisted after every operation**, `Serial` incrementing, under one lock held for the whole run — never batched until the end.
- **Every command literal sets `SilenceUsage: true, SilenceErrors: true`.** Cobra consults the root's fields only when a command has a parent, and tests construct commands standalone. `TestEveryCommandSilencesUsageAndErrors` walks the tree and will fail otherwise.
- `make check` green and `go test ./... -race` green before any task is reported done.

### Test rules

M2 shipped **eight** assertions that could not fail as their author intended. The causes differed; the shape did not — a fixture chosen for brevity or convenience, where the convenience destroys the assertion.

- **For an ordering assertion, choose values whose natural order CONTRADICTS the expected order.** `graph.Layers` and the walker tie-break alphabetically, so a fixture sorted the way the answer requires passes with the logic deleted.
- **A single-element fixture asserts nothing** about ordering, sorting or selection. Use three.
- **`contains` cannot detect pollution.** To assert a clean run, compare exactly or assert what must NOT be present.
- **A concurrency test that passes against a serial implementation is not a concurrency test.** Assert observed overlap — an in-flight counter, a barrier both goroutines must reach — not elapsed time alone.
- **Never write a conditional assertion that cannot fail before the fix.** `if x == want { check(y) }` where `x != want` today is vacuous until the change it verifies lands.
- Every fix gets a test that fails before it and passes after. Run the RED and quote it. If a test passes both ways, say so plainly rather than dressing it up.
- Ask "why *can't* this fail?" — sometimes the answer is that the code beneath it does nothing.

---

### Task 1: `Local.Put` refuses to write without a held lock

**Files:**
- Modify: `internal/state/backend.go` (add `ErrNotLocked`)
- Modify: `internal/state/lock.go` (add `requireOwnLock`)
- Modify: `internal/state/local.go` (call it from `Put`)
- Modify: `internal/state/local_test.go` (new tests, and fix five existing tests that call `Put` without a lock)
- Modify: `internal/refresh/refresh_test.go` (one `Put` call site needs a `Lock` first)
- Modify: `internal/cli/plan_test.go` (one `Put` call site needs a `Lock` first)

**Interfaces:**
- Consumes: `Local.Inspect(environment string) (Lock, bool, error)`, `Lock{Environment, PID, Host, User, Operation, At}`, the unexported `hostname() string` (all existing in `internal/state/lock.go`); `ErrLocked` (existing, `internal/state/backend.go`).
- Produces: `var ErrNotLocked error` (`internal/state/backend.go`), wrapped by `Put` whenever it refuses. `Local.Put`'s signature is unchanged — `func (l *Local) Put(ctx context.Context, environment string, s *State) error` — exactly as the M3 authoring contract already pins it for every later task that calls it.

The contract fixes `Put`'s signature before this task runs: it is already the exact shape `internal/refresh` and every later M3 command is written against. That settles the question the task description poses. A token — `Put(ctx, environment, s, lock state.Lock)` — would be the stronger guarantee, caught at compile time rather than discovered from an error return, and it is the shape you would pick starting from nothing. But starting from nothing is not the situation: changing `Put`'s signature here means changing `state.Backend`, and rippling that change through `internal/refresh`, `internal/cli`, and Group B and D's not-yet-written call sites, all to buy a guarantee that a cheaper check gives at the one place it actually needs to hold — the moment bytes are about to be written to disk. So `Put` re-derives lock ownership itself, the same way `Unlock` already does: read the lock file, compare `PID` and `Host` against this process, refuse if they disagree or nothing is held.

What that choice costs, plainly: the check is enforced at runtime, on every call, not at compile time — a caller that never calls `Lock` finds out from an `ErrNotLocked` return, not from a build failure. And every `Put` now costs one extra file read (`Inspect`) before it writes. Both are cheap against what they buy. `refresh` and `apply` already hold one lock across many `Put` calls per run (spec §15), so the added read is one stat-and-read alongside a write already going to disk — not a new write, not a new lock file, not new contention. `plan` needs nothing special here at all: it never calls `Put`, so it never reaches this check, and stays exactly as lock-free as spec §10 requires. The one sharp edge worth naming: this is check-then-act, not a held handle. If a lock is force-removed by a human running `infra state unlock` mid-run — which is the intended remedy for a stale lock, not a bug — the *next* `Put` after that will correctly refuse; it cannot retroactively un-write whatever the *concurrent* write in flight at that exact instant already committed. That is an acceptable gap for M3: locks are refused by convention violation (two applies), not raced against a human deliberately intervening, and Group C's Task 11 (SIGINT) is what makes "the lock is gone, stop touching state" observable to a running process going forward.

- [ ] **Step 1: Write the failing tests**

Replace `internal/state/local_test.go` in full with:

```go
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
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/state/ -run 'TestPutWithoutLockRefuses|TestPutRefusesWhenLockedByAnotherProcess|TestPutAfterUnlockRefuses|TestMultiplePutsSucceedUnderOneHeldLock' -v`
Expected: FAIL to build — `undefined: ErrNotLocked`. The whole package fails to compile because `ErrNotLocked` does not exist yet; every test in the package reports as failed as a consequence, which is expected at this step.

- [ ] **Step 3: Implement**

In `internal/state/backend.go`, add next to `ErrLocked`:

```go
// ErrNotLocked is wrapped by Put when the caller has not acquired the
// environment's lock — or no longer holds it — at the moment of the write.
// The storage layer enforces this itself, at the write, rather than relying
// on callers to have locked earlier: invariant 5 is a property of what the
// backend allows, not of caller discipline (spec §9.2, §15).
var ErrNotLocked = errors.New("state write refused: environment is not locked by this process")
```

In `internal/state/lock.go`, add:

```go
// requireOwnLock refuses unless environment is currently locked by this
// process. Put calls it before writing anything: see the design note on Put
// in local.go for why this re-derives ownership from the lock file's
// contents — the same check Unlock already makes — rather than trusting a
// token handed back by Lock.
func (l *Local) requireOwnLock(environment string) error {
	held, ok, err := l.Inspect(environment)
	if err != nil {
		return fmt.Errorf("checking lock for %q before writing state: %w", environment, err)
	}
	if !ok {
		return fmt.Errorf("refusing to write state for %q: no lock is held; call Lock first: %w", environment, ErrNotLocked)
	}
	if held.PID != os.Getpid() || held.Host != hostname() {
		return fmt.Errorf("refusing to write state for %q: locked by %s on %s (pid %d), not by this process: %w",
			environment, held.User, held.Host, held.PID, ErrNotLocked)
	}
	return nil
}
```

In `internal/state/local.go`, change `Put`'s doc comment and add the check as its first statement:

```go
// Put writes state atomically: a temporary file in the same directory, then
// a rename. A crash mid-write therefore cannot corrupt state.
//
// Put refuses to write unless the caller currently holds environment's
// lock, checked by re-reading the lock file's contents and comparing PID
// and host against this process — the same check Unlock already makes. This
// keeps the Backend.Put(ctx, environment, s) signature exactly as the M3
// authoring contract already pins it, consumed by internal/refresh,
// internal/cli and every later M3 task as written: a token-carrying
// signature such as Put(ctx, environment, s, lock) would ripple through
// every one of those call sites and the Backend interface itself, for a
// guarantee the weaker check already gives at the point that actually
// matters — the write. What that costs: the check is enforced at runtime on
// every call rather than at compile time, and each Put now costs one extra
// file read to re-Inspect the lock. Both are cheap next to a signature
// change touching every caller in the tree — refresh and apply each Put
// many times across one run under a single held lock (spec §15), so the
// added read is one stat-and-read alongside a write already going to disk —
// and plan never calls Put at all, so it stays lock-free with no
// special-casing needed here.
func (l *Local) Put(ctx context.Context, environment string, s *State) error {
	if err := l.requireOwnLock(environment); err != nil {
		return err
	}

	path := l.statePath(environment)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	// Stamp the write, but restore on any failure path: a Serial that has
	// advanced past what is on disk would make a retry skip a value, and any
	// caller inspecting s.Serial after a failed Put would believe a write
	// happened. The serial is what detects stale plans, so it must not drift.
	prevSerial, prevEnv, prevUpdated := s.Serial, s.Environment, s.UpdatedAt
	committed := false
	defer func() {
		if !committed {
			s.Serial, s.Environment, s.UpdatedAt = prevSerial, prevEnv, prevUpdated
		}
	}()

	s.Serial++
	s.Environment = environment
	s.UpdatedAt = time.Now().UTC()

	data, err := s.Encode()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}
```

In `internal/refresh/refresh_test.go`, in `TestRefreshNeverWritesState`, replace:

```go
	backend := state.NewLocal(root)
	st := state.New("myapp", "dev")
	st.Set(created)
	if err := backend.Put(context.Background(), "dev", st); err != nil {
		t.Fatalf("Put: %v", err)
	}
```

with:

```go
	backend := state.NewLocal(root)
	if _, err := backend.Lock(context.Background(), "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(created)
	if err := backend.Put(context.Background(), "dev", st); err != nil {
		t.Fatalf("Put: %v", err)
	}
```

In `internal/cli/plan_test.go`, in `TestPlanFailsOnPreventDestroy`, replace:

```go
	st := state.New("myapp", "dev")
	st.Set(rs)
	if err := backendFor(dir).Put(ctx, "dev", st); err != nil {
		t.Fatalf("seeding state: %v", err)
	}
```

with:

```go
	st := state.New("myapp", "dev")
	st.Set(rs)
	b := backendFor(dir)
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := b.Put(ctx, "dev", st); err != nil {
		t.Fatalf("seeding state: %v", err)
	}
	if err := b.Unlock(ctx, "dev"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/state/ -v`
Expected: PASS — 29 tests (10 in `local_test.go`, 8 in `lock_test.go`, 9 in `state_test.go`, 2 in `golden_test.go`).

Run: `go test ./internal/refresh/ -v`
Expected: PASS — 11 tests, unaffected in count, `TestRefreshNeverWritesState` now locking before it seeds state.

Run: `go test ./internal/cli/ -v`
Expected: PASS — 23 tests, unaffected in count.

Run: `go test ./... && go vet ./... && gofmt -l .`
Expected: everything green, `gofmt -l .` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/state/backend.go internal/state/lock.go internal/state/local.go internal/state/local_test.go internal/refresh/refresh_test.go internal/cli/plan_test.go
git commit -m "$(cat <<'EOF'
fix: Local.Put refuses to write without a held lock

Today any caller can write state with no lock at all, which leaves
invariant 5 ("two applies cannot mutate one environment concurrently")
resting on convention rather than on the code. Put now re-derives lock
ownership from the lock file, the same way Unlock already does, and
refuses with ErrNotLocked when the environment is unlocked or held by
someone else. Backend.Put's signature is unchanged, so every existing
and future caller is unaffected except at the point they were already
supposed to be holding the lock.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

### Task 2: `graph.Walk`, an incremental readiness walker

**Files:**
- Create: `internal/graph/walk.go`
- Test: `internal/graph/walk_test.go`

**Interfaces:**
- Consumes: `graph.Node`, `graph.Graph[T]` with its unexported `nodes`, `out`, `in` fields and `(*Graph[T]).Cycle() []string` (all existing, same package, `internal/graph/graph.go`); the `testNode`, `structNode` and `build` test helpers from `internal/graph/graph_test.go` (same test package).
- Produces exactly the shape the M3 authoring contract names: `type Walk[T Node] struct{ ... }`; `func (g *Graph[T]) Walk() (*Walk[T], error)`; `func (w *Walk[T]) Ready() []T`; `func (w *Walk[T]) Done(id string) []T`; `func (w *Walk[T]) Skip(id string) []string`; `func (w *Walk[T]) Remaining() int`.

`Layers()` is a barrier: nothing in layer N+1 starts until every member of layer N — including whichever one happens to be slowest — has finished. §15 asks for something strictly more parallel: "a ready queue of operations whose predecessors have completed." The gap between the two only shows up when a layer contains both a slow node and a fast node that some other, unrelated node in the next layer depends on only through the fast one — `Layers` forces that unrelated node to wait for the slow one anyway, purely because they landed in the same layer; `Walk` must not, because that unrelated node's own predecessors say nothing about the slow one. `TestReadyDoesNotWaitForAnUnrelatedSlowNodeInTheSameLayer` below is built to fail against an implementation that computes `Walk` as `Layers` underneath and just hands out one layer at a time — the most tempting wrong implementation, since it would pass every other test in this file.

`Walk` tracks each node's status (`unstarted`, `dispatched`, `done`, `skipped`) alongside a per-node count of unfinished predecessors, decremented as predecessors resolve. `Ready` and the newly-ready portion of `Done`'s return both mark a node `dispatched` before handing it back, which is what makes "each returned once" true even if a caller calls `Ready` again before dispatching everything it was already given — a legitimate pattern for a bounded worker pool topping up idle workers. `Skip` marks a node and everything transitively reachable from it as `skipped`, stopping the moment it reaches a node already `skipped` — which makes it safe to call twice for a dependent two independent failures both reach, without double-reporting it or double-decrementing `Remaining`.

- [ ] **Step 1: Write the failing test**

Create `internal/graph/walk_test.go`:

```go
package graph

import (
	"strings"
	"testing"
)

func TestWalkOnACyclicGraphErrors(t *testing.T) {
	g := build(t, []string{"a", "b"}, [][2]string{{"a", "b"}, {"b", "a"}})
	_, err := g.Walk()
	if err == nil {
		t.Fatal("Walk() over a cyclic graph must error, the same way Layers() does")
	}
	for _, name := range []string{"a", "b"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error must name %q: %v", name, err)
		}
	}
}

func TestReadyReturnsEveryRootOnceEach(t *testing.T) {
	g := build(t, []string{"c", "a", "b"}, nil)
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	ready := w.Ready()
	if len(ready) != 3 || ready[0] != "a" || ready[1] != "b" || ready[2] != "c" {
		t.Fatalf("Ready() = %v, want [a b c] sorted", ready)
	}
}

func TestReadyDoesNotReturnAnAlreadyDispatchedNode(t *testing.T) {
	g := build(t, []string{"b", "a"}, nil)
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	first := w.Ready()
	if len(first) != 2 {
		t.Fatalf("first Ready() = %v, want both roots", first)
	}
	second := w.Ready()
	if len(second) != 0 {
		t.Errorf("second Ready() = %v, want none — both nodes were already dispatched by the first call", second)
	}
}

func TestReadyDoesNotWaitForAnUnrelatedSlowNodeInTheSameLayer(t *testing.T) {
	// slow, fastA and fastB are all roots — one layer under Layers(). child
	// depends only on fastA and fastB, never on slow. A barrier scheduler
	// (Layers) would hold child back until the WHOLE layer, including slow,
	// finishes; Walk must not, since child's own predecessors say nothing
	// about slow. This is the exact gap between Layers (§14) and the "ready
	// queue of operations whose predecessors have completed" §15 asks for.
	g := New[testNode]()
	for _, id := range []string{"slow", "fastA", "fastB", "child"} {
		g.Add(testNode(id))
	}
	g.Edge("fastA", "child")
	g.Edge("fastB", "child")

	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	ready := w.Ready()
	if len(ready) != 3 || ready[0] != "fastA" || ready[1] != "fastB" || ready[2] != "slow" {
		t.Fatalf("Ready() = %v, want [fastA fastB slow]", ready)
	}

	if got := w.Done("fastA"); len(got) != 0 {
		t.Errorf("Done(fastA) = %v, want none — fastB has not completed yet", got)
	}
	got := w.Done("fastB")
	if len(got) != 1 || got[0] != "child" {
		t.Fatalf("Done(fastB) = %v, want [child] — both of child's predecessors have completed, and slow is not one of them", got)
	}
	if w.Remaining() != 2 {
		t.Errorf("Remaining() = %d, want 2 — slow has not finished, and child is ready but not yet done", w.Remaining())
	}
}

func TestDoneTriggersReadinessRegardlessOfPredecessorCompletionOrder(t *testing.T) {
	// child depends on zeta and alpha. Finishing zeta — the alphabetically
	// LATER of the two — first must not trigger readiness; only finishing
	// both does, regardless of which order they complete in. An
	// implementation that (wrongly) assumed predecessors resolve in ID
	// order would pass this the other way around, which is why the
	// out-of-alphabetical-order finish comes first.
	g := build(t, []string{"alpha", "zeta", "child"}, [][2]string{
		{"alpha", "child"}, {"zeta", "child"},
	})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready()

	if got := w.Done("zeta"); len(got) != 0 {
		t.Errorf("Done(zeta) = %v, want none — alpha has not finished", got)
	}
	got := w.Done("alpha")
	if len(got) != 1 || got[0] != "child" {
		t.Fatalf("Done(alpha) = %v, want [child]", got)
	}
}

func TestSkipPropagatesTransitivelyAndSortsByID(t *testing.T) {
	// Topological order is zulu -> mike -> alpha; alphabetical order is the
	// reverse. Skip's contract is "sorted", not "in propagation order" — a
	// fixture where those two orders disagree is required to prove which
	// one comes back.
	g := build(t, []string{"zulu", "mike", "alpha", "other"}, [][2]string{
		{"zulu", "mike"}, {"mike", "alpha"},
	})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready() // dispatches zulu and other

	got := w.Skip("zulu")
	if len(got) != 2 || got[0] != "alpha" || got[1] != "mike" {
		t.Fatalf("Skip(zulu) = %v, want [alpha mike] sorted", got)
	}
	for _, id := range got {
		if id == "zulu" {
			t.Error("Skip must not include the failed node itself in its result")
		}
	}
	if w.Remaining() != 1 {
		t.Errorf("Remaining() = %d, want 1 — only \"other\" is left, unrelated to zulu's branch", w.Remaining())
	}
}

func TestSkipOnASharedDependentIsNotDoubleCounted(t *testing.T) {
	// c depends on both a and b. If a fails first, Skip(a) skips c. When b
	// also fails, Skip(b) must not report c again — it was already
	// resolved — and must not decrement Remaining a second time for it.
	g := build(t, []string{"a", "b", "c"}, [][2]string{{"a", "c"}, {"b", "c"}})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	w.Ready() // dispatches a and b

	first := w.Skip("a")
	if len(first) != 1 || first[0] != "c" {
		t.Fatalf("Skip(a) = %v, want [c]", first)
	}
	afterFirst := w.Remaining()

	second := w.Skip("b")
	if len(second) != 0 {
		t.Errorf("Skip(b) = %v, want none — c was already skipped by Skip(a)", second)
	}
	if w.Remaining() != afterFirst-1 {
		t.Errorf("Remaining() = %d, want %d — only b itself should be newly resolved, not c a second time", w.Remaining(), afterFirst-1)
	}
	if w.Remaining() != 0 {
		t.Errorf("Remaining() = %d, want 0 — a, b and c are all resolved", w.Remaining())
	}
}

func TestRemainingCountsDownAsNodesResolve(t *testing.T) {
	g := build(t, []string{"a", "b"}, [][2]string{{"a", "b"}})
	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if w.Remaining() != 2 {
		t.Fatalf("Remaining() = %d, want 2 before anything runs", w.Remaining())
	}
	w.Ready()
	w.Done("a")
	if w.Remaining() != 1 {
		t.Errorf("Remaining() = %d, want 1 after a is done", w.Remaining())
	}
	w.Done("b")
	if w.Remaining() != 0 {
		t.Errorf("Remaining() = %d, want 0 after everything is done", w.Remaining())
	}
}

func TestWalkWorksOverStructNodes(t *testing.T) {
	// The executor instantiates Walk[planner.OpNode], a struct. Proving the
	// generic works with testNode, a named string type, elsewhere in this
	// package proves nothing about a struct constraint — structNode (from
	// graph_test.go) is what closes that gap here, the same way it does for
	// Graph itself in TestGraphAcceptsAStructValueType.
	g := New[structNode]()
	root := structNode{Name: "network", Phase: 0}
	dependent := structNode{Name: "database", Phase: 0}
	g.Add(root)
	g.Add(dependent)
	g.Edge(root.ID(), dependent.ID())

	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	ready := w.Ready()
	if len(ready) != 1 || ready[0] != root {
		t.Fatalf("Ready() = %v, want [%v]", ready, root)
	}
	got := w.Done(root.ID())
	if len(got) != 1 || got[0] != dependent {
		t.Fatalf("Done(%q) = %v, want [%v]", root.ID(), got, dependent)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/graph/ -run TestWalk -v`
Expected: FAIL to build — `g.Walk undefined (type *Graph[T] has no field or method Walk)`.

- [ ] **Step 3: Implement**

Create `internal/graph/walk.go`:

```go
package graph

import (
	"fmt"
	"sort"
	"strings"
)

// walkStatus is one node's progress through a Walk.
type walkStatus uint8

const (
	statusUnstarted walkStatus = iota
	statusDispatched
	statusDone
	statusSkipped
)

// Walk tracks a graph's execution readiness incrementally: unlike Layers,
// which is a barrier where every node in layer N finishes before layer N+1
// starts, Walk exposes a live ready queue — a node becomes ready the instant
// its own predecessors are done, regardless of what else is still running
// elsewhere in the same graph. Spec §15 asks for exactly this: "a ready
// queue of operations whose predecessors have completed," which is strictly
// more parallel than a layer barrier whenever a graph has a slow node
// sharing a layer with unrelated fast ones.
//
// Not safe for concurrent use. The executor owns one Walk from a single
// goroutine — the one that accepts completions off a worker pool and hands
// out newly-ready work — never mutated from two goroutines at once; the
// concurrency in "worker pool" lives entirely in what runs the operations a
// Walk hands out, not in the Walk itself.
type Walk[T Node] struct {
	nodes     map[string]T
	out       map[string]map[string]bool
	remaining map[string]int
	status    map[string]walkStatus
	left      int
}

// Walk begins an incremental readiness walk over g. It fails on the same
// cycle Layers would, and for the same reason: a Walk over a cyclic graph
// could never resolve every node to done or skipped, so Remaining would
// never reach zero and an executor built on it would hang instead of
// reporting an error.
func (g *Graph[T]) Walk() (*Walk[T], error) {
	if cycle := g.Cycle(); cycle != nil {
		full := append(append([]string(nil), cycle...), cycle[0])
		return nil, fmt.Errorf("graph: cycle detected: %s", strings.Join(full, " -> "))
	}

	w := &Walk[T]{
		nodes:     make(map[string]T, len(g.nodes)),
		out:       g.out,
		remaining: make(map[string]int, len(g.nodes)),
		status:    make(map[string]walkStatus, len(g.nodes)),
		left:      len(g.nodes),
	}
	for id, n := range g.nodes {
		w.nodes[id] = n
		w.remaining[id] = len(g.in[id])
	}
	return w, nil
}

// sortedOut returns the IDs id points to, sorted — the same guarantee
// Graph.sortedOut makes, kept separate because Walk reads g.out directly
// rather than holding a *Graph[T]; the graph is finished being built by the
// time a Walk exists (Add and Edge are always complete before Walk is
// called), so nothing here needs the rest of Graph's API.
func (w *Walk[T]) sortedOut(id string) []string {
	next := w.out[id]
	out := make([]string, 0, len(next))
	for n := range next {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Ready returns every node with no unfinished predecessor that has not
// already been handed out. Each returned node is marked dispatched before
// this returns, so calling Ready again before those nodes reach Done or
// Skip returns none of them a second time — which makes it safe for a
// worker pool to call it again after finishing a batch, to pick up anything
// else that became ready in the meantime, without tracking dispatch state
// of its own.
func (w *Walk[T]) Ready() []T {
	var ids []string
	for id, r := range w.remaining {
		if r == 0 && w.status[id] == statusUnstarted {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	out := make([]T, 0, len(ids))
	for _, id := range ids {
		w.status[id] = statusDispatched
		out = append(out, w.nodes[id])
	}
	return out
}

// Done marks id complete and returns the nodes that become ready as a
// direct result: every successor of id whose last unfinished predecessor
// was id itself. This is the incremental half of the readiness contract —
// a successor with a second, still-unfinished predecessor elsewhere in the
// graph is not returned here, no matter how much else has finished,
// because §15's ready queue is defined by a node's OWN predecessors, not by
// how much of the graph as a whole has completed.
//
// The result is already in sorted order: it is built by filtering
// sortedOut(id), and filtering a sorted sequence cannot unsort it.
func (w *Walk[T]) Done(id string) []T {
	if w.status[id] == statusUnstarted || w.status[id] == statusDispatched {
		w.left--
	}
	w.status[id] = statusDone

	var ready []T
	for _, next := range w.sortedOut(id) {
		if w.status[next] != statusUnstarted {
			continue
		}
		w.remaining[next]--
		if w.remaining[next] == 0 {
			w.status[next] = statusDispatched
			ready = append(ready, w.nodes[next])
		}
	}
	return ready
}

// Skip marks id, and every node reachable from it — its dependents,
// transitively — as skipped, and returns their IDs sorted. This is how "a
// failure stops its branch, not the world" (spec §15) is implemented: a
// dependent of a failed operation can never legitimately run, since at
// least one of its own predecessors never completed, so Skip retires it
// without ever dispatching it, rather than leaving it to wait forever with
// Remaining never reaching zero.
//
// id itself is never in the returned slice. The caller already knows id
// failed — that is why Skip was called instead of Done — and reports it
// separately; what this returns is exactly the set the caller should add to
// its own report of skipped work.
//
// Calling Skip a second time for a node already skipped by an earlier call
// — because two independent failures share a dependent — is safe and
// returns none of it again: the moment a node is found already
// statusSkipped, this stops descending through it, so neither Remaining nor
// the returned slice double-counts it.
func (w *Walk[T]) Skip(id string) []string {
	var skipped []string
	seen := map[string]bool{id: true}
	queue := []string{id}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if w.status[cur] == statusSkipped {
			continue
		}
		if w.status[cur] == statusUnstarted || w.status[cur] == statusDispatched {
			w.left--
		}
		w.status[cur] = statusSkipped
		if cur != id {
			skipped = append(skipped, cur)
		}

		for _, next := range w.sortedOut(cur) {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}

	sort.Strings(skipped)
	return skipped
}

// Remaining reports how many nodes have not yet been marked done or
// skipped. It reaches zero exactly when every node the Walk started with
// has been resolved one way or the other — successfully or not — which is
// the executor's own signal that its run over this graph is finished.
func (w *Walk[T]) Remaining() int { return w.left }
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/graph/ -v`
Expected: PASS — 24 tests (9 new in `walk_test.go`, plus the 15 already in `graph_test.go`, unaffected).

Run: `go test ./... && go vet ./... && gofmt -l .`
Expected: everything green.

- [ ] **Step 5: Commit**

```bash
git add internal/graph/walk.go internal/graph/walk_test.go
git commit -m "$(cat <<'EOF'
feat: add graph.Walk, an incremental readiness walker

Layers() is a barrier: nothing in layer N+1 starts until every member
of layer N has finished, including whichever one happens to be
slowest. Spec §15 asks for a ready queue of operations whose
predecessors have completed, which is strictly more parallel — a node
in the next layer that only depends on the FAST members of this one
should not have to wait for the slow one too. Walk tracks readiness
per node instead of per layer, and Skip propagates a failure to its
transitive dependents without touching anything on an independent
branch.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

### Task 3: fold resolved parts into a deferred expression

**Files:**
- Modify: `internal/expressions/eval.go` (add `residual`, call it from `unknownFrom`)
- Test: `internal/expressions/eval_test.go` (new tests)
- Test: `internal/compiler/resolved_test.go` (one new test — the hash consequence)

**Interfaces:**
- Consumes: `value.Expr`, `value.ExprOp` (`OpLiteral`, `OpVarRef`, `OpResourceRef`, `OpConcat`, `OpCall`), `value.Value`.
- Produces: no new exported names. `Value.Expr` on a deferred value now carries a *residual* expression rather than the source expression.

An expression that cannot be evaluated yet is deferred: `unknownFrom` returns an
unknown value carrying the expression in `Value.Expr`, and the executor
re-evaluates it at apply time once the dependency exists (Task 7). Today it
attaches the **original, unmodified** expression, so every part of it —
including parts that resolved perfectly well at compile time — is left to be
resolved again later.

That is wrong in two ways, and the second is the one that decides this task.

The first is that the executor would need the compiler's whole scope to
re-evaluate. Measured on `network: ${prefix}-${network.id}` compiled with
`--var prefix=acme`:

```
deferred expr renders as: ${prefix}-${network.id}
  arg[0] op=OpVarRef      ref="prefix"      <- resolved at compile time, still a ref
  arg[1] op=OpLiteral     raw="-"
  arg[2] op=OpResourceRef ref="network.id"  <- genuinely unknown until apply
```

The second is that `ConfigHash` cannot see the difference. `hashExpr` folds an
unresolved expression by writing its op name, function, ref *name* and argument
count — never a resolved value, because there isn't one to write. So:

```
prefix=acme              -> 4e8601823af6ab9438edb61b205a264e826bb4585e62d56420eaa0aad3e8f53e
prefix=totally-different -> 4e8601823af6ab9438edb61b205a264e826bb4585e62d56420eaa0aad3e8f53e
```

Byte-identical hashes for two configurations that differ in a value the plan was
computed with. M6's staleness refusal compares `ConfigHash` to decide whether a
saved plan still describes the configuration, so this is a hole in a check that
does not exist yet — which is exactly when it is cheap to close.

The fix is to defer a **residual**: an expression in which everything already
known is a literal, and only what is genuinely unknown remains a reference. A
deferred expression then says what it means — "this is what is left to compute"
— the hash captures the values the plan was actually built from, and the
executor needs no variable scope at all.

The residual is built from the *evaluated* arguments, so `residual` takes them
alongside the source expression. `OpConcat` is the only operator that can be
partially resolved: a call is all-or-nothing (its arguments are evaluated, and
if any is unknown the call cannot run), and a bare ref is either resolved or
not. So the fold applies to concat, and every other deferral keeps the source
expression — which is already correct, because nothing in it resolved.

- [ ] **Step 1: Write the failing tests**

Add to `internal/expressions/eval_test.go`. These use the file's existing
`testScope` and `evalSrc` helpers, so they go through the real parser as well as
the evaluator. **I compiled and ran all three against the current code before
writing them here**: the first fails for the stated reason, the third passes
already (correctly — nothing resolves in it, so there is nothing to fold).

```go
func foldScope() testScope {
	return testScope{vars: map[string]value.Value{
		"prefix": value.String("acme", value.SourceVariable),
		"secret": value.String("hunter2", value.SourceVariable).WithSensitive(true),
	}}
}

// TestDeferredConcatFoldsResolvedParts pins that a deferred expression carries
// only what is still unknown. Before this, the whole source expression was
// deferred, so a variable resolved at compile time was left to be resolved
// again by whoever evaluated it later — and ConfigHash could not see its value.
func TestDeferredConcatFoldsResolvedParts(t *testing.T) {
	got, ds := evalSrc(t, "${prefix}-${network.id}", foldScope())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got.Known {
		t.Fatal("a resource reference is unknown until apply, so the result must be unknown")
	}
	if got.Expr == nil {
		t.Fatal("a deferred value must carry the expression that will produce it")
	}
	if len(got.Expr.Args) != 3 {
		t.Fatalf("residual should keep all three positions, got %d", len(got.Expr.Args))
	}
	// The variable resolved, so it is now a literal carrying its VALUE.
	if op := got.Expr.Args[0].Op; op != value.OpLiteral {
		t.Errorf("arg[0] op = %v, want OpLiteral — the variable resolved at compile time", op)
	}
	if s, _ := got.Expr.Args[0].Literal.AsString(); s != "acme" {
		t.Errorf("arg[0] literal = %q, want %q — folding must carry the value, not just the kind", s, "acme")
	}
	// The resource reference did not, so it survives as a reference.
	if op := got.Expr.Args[2].Op; op != value.OpResourceRef {
		t.Errorf("arg[2] op = %v, want OpResourceRef — it is genuinely unknown until apply", op)
	}
}

// TestDeferredSensitivitySurvivesFolding pins that folding a resolved SENSITIVE
// part into a literal does not lose its mark. A folded literal that dropped its
// Sensitive flag would be a way to launder a secret into a plan artifact.
//
// Both assertions are UNCONDITIONAL on purpose. An earlier draft guarded the
// second with `if args[0].Op == OpLiteral`, which is false before the fix — so
// the check could not fail until after the change it exists to verify. That is
// the vacuous-assertion shape this project has shipped eight times; do not
// reintroduce it by making either line conditional.
func TestDeferredSensitivitySurvivesFolding(t *testing.T) {
	got, _ := evalSrc(t, "${secret}-${network.id}", foldScope())
	if !got.Sensitive {
		t.Error("a deferred value built from a sensitive part must itself be sensitive")
	}
	if got.Expr == nil || len(got.Expr.Args) == 0 {
		t.Fatal("no residual expression")
	}
	if got.Expr.Args[0].Op != value.OpLiteral {
		t.Fatalf("arg[0] op = %v, want OpLiteral", got.Expr.Args[0].Op)
	}
	if !got.Expr.Args[0].Literal.Sensitive {
		t.Error("the folded literal must stay marked sensitive; otherwise the residual launders the secret")
	}
}

// TestFullyUnresolvedConcatKeepsItsShape checks the fold does not disturb the
// case where nothing resolved. This one PASSES before the fix as well as after,
// and that is stated rather than dressed up: it is a guard against the fold
// damaging an expression it should leave alone, not evidence the fold works.
func TestFullyUnresolvedConcatKeepsItsShape(t *testing.T) {
	got, _ := evalSrc(t, "${a.id}-${b.id}", foldScope())
	if got.Expr == nil {
		t.Fatal("expected a deferred expression")
	}
	if s := got.Expr.String(); s != "${a.id}-${b.id}" {
		t.Errorf("residual = %q, want the source shape back", s)
	}
}
```

Add to `internal/compiler/resolved_test.go`:

```go
// TestConfigHashSeesAVariableFeedingADeferredExpression is the reason Task 3
// exists. hashExpr folds an unresolved expression by op name, function, ref
// NAME and argument count — never a resolved value, because before folding
// there was none to write. Two configurations differing only in a --var that
// feeds a deferred expression therefore hashed identically, and M6's staleness
// refusal compares exactly this hash to decide whether a saved plan still
// describes the configuration.
func TestConfigHashSeesAVariableFeedingADeferredExpression(t *testing.T) {
	body := `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${prefix}-${network.id}
`
	hashWith := func(prefix string) string {
		t.Helper()
		cfg, ds := Compile(loadFiles(t, body), testRegistry(t), Options{
			Vars: map[string]string{"prefix": prefix},
		})
		if ds.HasErrors() {
			t.Fatalf("compile with prefix=%q: %+v", prefix, ds)
		}
		h, err := cfg.Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		return h
	}

	if a, b := hashWith("acme"), hashWith("totally-different"); a == b {
		t.Errorf("ConfigHash is identical for two different --var values feeding a deferred "+
			"expression (%s); a saved plan would be accepted against configuration it was not computed from", a)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/expressions/ -run 'TestDeferred|TestFullyUnresolved' -v`
Expected: FAIL, and this is the measured output, not a prediction:

```
--- FAIL: TestDeferredConcatFoldsResolvedParts
    arg[0] op = 1, want OpLiteral — the variable resolved at compile time
    arg[0] literal = "", want "acme"
--- PASS: TestDeferredSensitivitySurvivesFolding   <- see note below
--- PASS: TestFullyUnresolvedConcatKeepsItsShape
```

`op = 1` is `OpVarRef`: the deferred tree still holds the reference rather than
the value. Note that `TestDeferredSensitivitySurvivesFolding` passed in the
draft that guarded its second assertion behind a conditional; with the
unconditional form above it fails too, at the `want OpLiteral` line. If it
passes for you before the fix, the assertion has been weakened — check it
before continuing.

Run: `go test ./internal/compiler/ -run TestConfigHashSeesAVariableFeedingADeferredExpression -v`
Expected: FAIL with the two hashes printed identical.

Quote both failures in your report. The hash test in particular is the one that
matters: it is the only evidence that this task is worth doing at all.

- [ ] **Step 3: Build the residual**

In `internal/expressions/eval.go`, add:

```go
// residual builds the expression that is left to evaluate once everything
// resolvable has been resolved.
//
// A deferred expression is re-evaluated later — by the executor, once the
// resource it references exists. Attaching the SOURCE expression makes that
// later evaluation redo work that already succeeded, which has two costs. It
// forces the later evaluator to carry the earlier one's whole scope, including
// variables that have nothing to do with the resource being waited on. And it
// makes the deferred expression silent about the values it was built from, so
// ConfigHash — which folds an unresolved expression by op, function, ref name
// and arity — cannot distinguish two configurations that differ only in one of
// them.
//
// Only OpConcat can be partially resolved. A call is all-or-nothing: its
// arguments are evaluated and if any is unknown the call cannot run, so there
// is no partial result to keep. A bare reference either resolved or did not.
// Every other deferral therefore keeps its source expression, which is already
// correct because nothing in it resolved.
//
// evaluated[i] is the result of evaluating e.Args[i]; the two slices are
// parallel. The source expression is never modified: it belongs to the caller's
// AST, is shared by every value that references it, and is what a diagnostic
// renders.
func residual(e *value.Expr, evaluated []value.Value) *value.Expr {
	if e.Op != value.OpConcat || len(evaluated) != len(e.Args) {
		return e
	}

	out := &value.Expr{Op: e.Op, Function: e.Function, Ref: e.Ref, Origin: e.Origin}
	out.Args = make([]*value.Expr, len(e.Args))
	for i, arg := range e.Args {
		v := evaluated[i]
		if !v.Known {
			// Still unknown: keep the reference so the executor can resolve it.
			out.Args[i] = arg
			continue
		}
		// Resolved: fold the VALUE in, sensitivity and all. A folded literal
		// that lost its Sensitive flag would be a way to launder a secret into
		// a plan artifact.
		out.Args[i] = &value.Expr{Op: value.OpLiteral, Literal: v, Origin: arg.Origin}
	}
	return out
}
```

- [ ] **Step 4: Defer the residual instead of the source**

`unknownFrom` is called from eight places. Only the concat path has evaluated
arguments to fold, so give it a variant rather than changing all eight:

```go
// unknownResidual defers the residual of a partially-resolved expression.
func unknownResidual(e *value.Expr, evaluated []value.Value, kind value.Kind, sensitive bool) value.Value {
	return unknownFrom(residual(e, evaluated), kind, sensitive)
}
```

Then in `evaluateConcat`, collect the evaluated arguments and use it. The loop
currently discards each `v` after stringifying; keep them:

```go
func evaluateConcat(e *value.Expr, scope Scope, ds *diag.Diagnostics) value.Value {
	var b strings.Builder
	sensitive := false
	known := true
	evaluated := make([]value.Value, len(e.Args))

	for i, arg := range e.Args {
		v := evaluate(arg, scope, ds)
		evaluated[i] = v
		sensitive = sensitive || v.Sensitive
		if !v.Known {
			known = false
			continue
		}
		s, ok := stringify(v)
		if !ok {
			// A composite (list or map) cannot be interpolated into a string.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "cannot interpolate a " + v.Kind.String() + " value into a string",
				Detail:   "Interpolation accepts only string, integer, float and boolean values.",
				Origin:   arg.Origin,
			})
			known = false
			// Mark it unknown so the residual keeps the source arg rather than
			// folding in a value the diagnostic just rejected.
			evaluated[i] = value.Unknown(v.Kind, value.SourceComputed)
			continue
		}
		b.WriteString(s)
	}

	if !known {
		// Unknownness is contagious, and the result is classified before it is
		// known: a secret in any part makes the whole result sensitive.
		return unknownResidual(e, evaluated, value.KindString, sensitive)
	}
	return value.String(b.String(), value.SourceComputed).
		WithSensitive(sensitive).
		WithOrigin(e.Origin)
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/expressions/ ./internal/compiler/ -v`
Expected: PASS.

**Then check the ripple, which is the part most likely to bite.** Folding
changes `ConfigHash` for any configuration mixing a resolved value with a
deferred reference. Run the whole suite:

Run: `go test ./... -race`
Expected: PASS. If a determinism fixture, golden file or hash assertion moved,
that is a real consequence of this change and not a flake — report exactly which
test moved and why, and confirm the new value is stable across runs before
accepting it. Do not update a golden without saying what changed in it.

- [ ] **Step 6: Commit**

```bash
git add internal/expressions/eval.go internal/expressions/eval_test.go internal/compiler/resolved_test.go
git commit -m "$(cat <<'EOF'
fix: defer the residual of an expression, not the whole source

An expression that cannot be evaluated yet is deferred for the executor to
resolve once its dependency exists. unknownFrom attached the original
expression, so parts that resolved perfectly well at compile time were left
to be resolved again later.

Measured on `${prefix}-${network.id}` with prefix supplied by --var: the
deferred tree kept `ref="prefix"` rather than the value "acme".

Two costs. The executor would need the compiler's entire scope, including
variables unrelated to the resource being waited on. And ConfigHash could not
see the difference at all — hashExpr folds an unresolved expression by op,
function, ref NAME and arity, so:

    prefix=acme              -> 4e8601823af6ab94...
    prefix=totally-different -> 4e8601823af6ab94...

Byte-identical hashes for configurations differing in a value the plan was
computed from. M6's staleness refusal compares that hash to decide whether a
saved plan still describes the configuration, so this is a hole in a check
that does not exist yet — which is when it is cheapest to close.

A deferred expression now carries only what is still unknown. Resolved parts
fold to literals carrying their values, sensitivity included, since a folded
literal that lost its flag would launder a secret. Only OpConcat can be
partially resolved; every other deferral keeps its source, which is already
right because nothing in it resolved. The source AST is never mutated.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

### Task 4: `internal/executor` types

**Files:**
- Create: `internal/executor/types.go`
- Test: `internal/executor/types_test.go`

**Interfaces:**
- Consumes: `planner.OpKind`, `planner.OpNode{Address, Kind, Phase}` with `(OpNode).ID() string`, `planner.OpReplace`, `planner.PhaseCreate` (`internal/planner`, existing); `address.Address` (`pkg/address`, existing); `registry.Registry` (`internal/registry`, existing); `state.Local`, `state.State` (`internal/state`, existing); `value.Value` (`pkg/value`, existing, used only in a guard test — never as a field).
- Produces exactly the shapes the M3 authoring contract names: `executor.Result{Applied, Failed, Skipped, State}`; `executor.Options{Parallelism, PerProvider, Registry, Backend, Environment, Retry, Now, OnEvent}`; `executor.RetryPolicy{MaxAttempts, Base, Max, Sleep, Jitter}` (fields only — Task 5 gives it behavior); `executor.EventKind` with constants `EventStarted`, `EventSucceeded`, `EventFailed`, `EventRetrying`, `EventSkipped` and `(EventKind).String() string`; `executor.Event{Kind, Address, Op, Attempt, Err, Message, At}`.

This is the first task to create `internal/executor`, so it carries the package doc. `RetryPolicy` is defined here rather than with Task 5's retry loop because `Options.Retry` is typed as `RetryPolicy` — the struct has to exist for `Options` to compile — but nothing here gives it behavior; `retryable`, `backoff` and `Attempt` are Task 5's, in a separate file, and are the only things that read these fields.

`Event` is the one type here worth deliberating over, because two different downstream authors depend on it doing its job without a third mistake: Group C's Task 12 builds summary rendering by consuming a stream of these, and Group D's commands print progress from them as an apply runs. The three states an operation moves through are started, then either succeeded or (failed, possibly after retrying), or never attempted at all — five kinds cover that: `EventStarted`, `EventSucceeded`, `EventFailed`, `EventRetrying`, `EventSkipped`. What `Event` must NOT carry is any way to print a resource attribute without going through `value.Format` first — the codebase's single redaction path, already burned once by a second one drifting out of sync with it (see `pkg/value/format.go`'s own comment on exactly that history). The two ways an `Event` type usually acquires that hazard are a raw `map[string]value.Value` of attributes, or a `value.Value` field for "the value that changed" — this `Event` has neither. Its only text is `Message`, a string that whatever populates an `Event` (Task 6's dispatch code, Task 5's retry loop) is responsible for having already run through `value.Format` wherever it quotes something from a resource. `TestEventCarriesNoValueType` below pins that structurally, by reflection, so a future field addition that reintroduces a `value.Value` leaf fails a test rather than shipping.

- [ ] **Step 1: Write the failing test**

Create `internal/executor/types_test.go`:

```go
package executor

import (
	"errors"
	"reflect"
	"testing"

	"infra/internal/planner"
	"infra/pkg/address"
	"infra/pkg/value"
)

func TestEventKindStringNamesEveryKindDistinctly(t *testing.T) {
	cases := []struct {
		kind EventKind
		want string
	}{
		{EventStarted, "started"},
		{EventSucceeded, "succeeded"},
		{EventFailed, "failed"},
		{EventRetrying, "retrying"},
		{EventSkipped, "skipped"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		got := tc.kind.String()
		if got != tc.want {
			t.Errorf("EventKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
		if seen[got] {
			t.Errorf("%q is returned by more than one EventKind — a renderer keying off this string would merge two different kinds", got)
		}
		seen[got] = true
	}
}

// TestEventCarriesNoValueType guards the property Event's GoDoc promises:
// nothing on the type can bypass value.Format's redaction because there is
// no value.Value leaf on it to bypass. A hand-maintained checklist would not
// catch a field added later that reintroduces one; reflection does.
func TestEventCarriesNoValueType(t *testing.T) {
	valueType := reflect.TypeOf(value.Value{})
	typ := reflect.TypeOf(Event{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type == valueType {
			t.Errorf("Event.%s is a value.Value — every field on Event must already be pre-formatted text, or a future OnEvent can print an unredacted secret", f.Name)
		}
	}
}

// TestResultFailedIsKeyedByOpNodeID pins Result.Failed's documented key
// shape against the real OpNode.ID(), not a hand-typed string that could
// drift from what planner.OpNode actually produces.
func TestResultFailedIsKeyedByOpNodeID(t *testing.T) {
	n := planner.OpNode{
		Address: address.Address{Name: "db"},
		Kind:    planner.OpReplace,
		Phase:   planner.PhaseCreate,
	}
	r := Result{Failed: map[string]error{n.ID(): errors.New("boom")}}
	if _, ok := r.Failed["create:db"]; !ok {
		t.Errorf("Failed must be keyed by OpNode.ID() (e.g. %q), got keys %v", n.ID(), keysOf(r.Failed))
	}
}

func keysOf(m map[string]error) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/executor/ -v`
Expected: FAIL to build — `undefined: EventKind` (the package does not exist yet beyond this test file).

- [ ] **Step 3: Implement**

Create `internal/executor/types.go`:

```go
// Package executor turns an execution graph into changes against real
// infrastructure: it drains a graph.Walk, dispatches each ready operation to
// its provider, persists state after every one, and reports what happened.
// Spec §15.
package executor

import (
	"context"
	"time"

	"infra/internal/planner"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
)

// Result is what one Apply run produced.
type Result struct {
	// Applied lists every address that was successfully created, updated or
	// destroyed. It is a report of outcome, not a replayable schedule, so it
	// carries no ordering guarantee beyond the order Apply happened to
	// finish them in.
	Applied []address.Address
	// Failed maps the planner.OpNode.ID() of every operation that failed to
	// the error it failed with. It is keyed by node ID rather than address
	// because a Replace is two nodes at one address ("destroy:x" and
	// "create:x"); an address alone cannot say which half failed.
	Failed map[string]error
	// Skipped lists the planner.OpNode.ID() of every operation that was
	// never attempted because one of its dependencies failed — every id
	// graph.Walk.Skip returned, accumulated across every failure in the run.
	Skipped []string
	// State is state as it now stands: every Put made during the run,
	// reflecting exactly what was actually applied rather than what the
	// plan proposed.
	State *state.State
}

// Options configures one Apply run.
type Options struct {
	// Parallelism bounds how many operations run at once across the whole
	// graph. Values below 1 mean 1 — a misconfigured --parallelism 0 must
	// degrade to serial execution, not deadlock on a zero-capacity resource.
	Parallelism int
	// PerProvider bounds how many operations run at once against a single
	// provider, independent of Parallelism, so one provider's rate limits
	// cannot be exhausted by an unrelated wide graph touching many
	// providers at once (PLAN.md §34). Values below 1 mean 1.
	PerProvider int
	// Registry resolves a resource type to the provider that implements it.
	Registry *registry.Registry
	// Backend is where state is read from and persisted to, under a lock
	// held for the whole run.
	Backend *state.Local
	// Environment names which environment's lock and state this run uses.
	Environment string
	// Retry governs how a failed provider call is retried, per the
	// classification table in retry.go.
	Retry RetryPolicy
	// Now is injected the same way the planner's is: production wires
	// time.Now, tests wire a fixed clock so event timestamps and any
	// time-based assertions are deterministic.
	Now func() time.Time
	// OnEvent receives one Event per progress notification. nil is allowed
	// — a caller that wants no progress reporting passes nothing. It may be
	// called concurrently from multiple workers; a receiver that is not
	// itself safe for concurrent use must serialize its own access.
	OnEvent func(Event)
}

// RetryPolicy governs how a failed provider call is retried. It carries no
// behavior of its own; see retry.go for the classification rules and the
// loop that reads these fields.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first —
	// not the number of retries. Values below 1 mean 1: exactly one
	// attempt, no retry.
	MaxAttempts int
	// Base is the delay before the first retry. Each subsequent retry
	// doubles it, capped at Max.
	Base time.Duration
	// Max caps the backoff delay, however many attempts have elapsed.
	Max time.Duration
	// Sleep waits out one backoff delay. It is injectable so tests can
	// supply a version that returns immediately instead of actually
	// sleeping, and so the real implementation can return early — wrapping
	// ctx.Err() — when the context is cancelled mid-wait, which is how a
	// SIGINT during a retry delay is able to interrupt the wait rather than
	// block until it elapses. nil means the real implementation.
	Sleep func(context.Context, time.Duration) error
	// Jitter perturbs a computed backoff delay before it is used, so many
	// operations that failed at the same instant do not all wake up and
	// retry in lockstep. It is injectable so tests can supply the identity
	// function and assert exact delays. nil means the real implementation.
	Jitter func(time.Duration) time.Duration
	// OnRetry is called once per retry, immediately before the backoff wait
	// begins: attempt is the attempt that just failed (1-based), err is why,
	// and delay is how long the wait will be. nil means no notification.
	//
	// It exists because this loop is the only place that knows all three
	// facts at the moment they are true. Task 8 needs them to emit
	// EventRetrying with honest timing; reconstructing them from outside
	// would mean guessing at the backoff schedule, and a progress line that
	// guesses is worse than none. Called synchronously and before the wait,
	// so an event reaches the user while the delay is still ahead rather
	// than being reported after the fact.
	OnRetry func(attempt int, err error, delay time.Duration)
}

// EventKind classifies one Event.
type EventKind uint8

const (
	// EventStarted reports that an operation has begun its first attempt.
	EventStarted EventKind = iota
	// EventSucceeded reports that an operation completed successfully.
	EventSucceeded
	// EventFailed reports that an operation failed with no further retry
	// coming — either the failure was not retryable, or every attempt
	// RetryPolicy allowed has been used.
	EventFailed
	// EventRetrying reports that an attempt failed but another is coming;
	// it fires before the backoff wait for that next attempt begins.
	EventRetrying
	// EventSkipped reports that an operation was never attempted because a
	// dependency of it failed.
	EventSkipped
)

// String names an event kind for logging and rendering.
func (k EventKind) String() string {
	switch k {
	case EventStarted:
		return "started"
	case EventSucceeded:
		return "succeeded"
	case EventFailed:
		return "failed"
	case EventRetrying:
		return "retrying"
	case EventSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

// Event is one progress notification from Apply.
//
// It deliberately carries no value.Value and no resource attribute map. The
// only text on it is Message, a human-readable line that whatever populates
// an Event has ALREADY passed through value.Format wherever it originated
// from a resource attribute — the same way every other place in this
// codebase that renders a value does; pkg/value/format.go is the only
// redaction path, and nothing may grow a second one. Giving Event a raw
// attributes field would hand every future OnEvent implementation — a
// terminal renderer, a JSON log line, a webhook — its own opportunity to
// print a secret straight from an attribute before it is ever redacted,
// which is exactly the shape of leak value.Format exists to foreclose.
// Keeping Event's surface to an address, a kind, an attempt counter and
// pre-formatted text keeps "sensitivity is per-leaf" true by construction
// here: there is no leaf on this type for a caller to reach around Format
// and print.
type Event struct {
	// Kind classifies the notification.
	Kind EventKind
	// Address identifies the resource the operation concerns.
	Address address.Address
	// Op is what the plan proposed for this resource — Create, Update,
	// Replace, Destroy or Forget. It is planner.OpKind rather than a
	// provider verb: a Replace is one plan operation dispatched as two
	// provider calls, and an Event reports at the plan-operation level a
	// person reads a plan at, not the provider-call level retry.go
	// classifies at.
	Op planner.OpKind
	// Attempt is 1 on an operation's first try and increases on each
	// EventRetrying. It is 0 for EventSkipped, which is never attempted.
	Attempt int
	// Err is set on EventFailed and nil otherwise.
	Err error
	// Message is human-readable detail — already redacted by whatever set
	// it, wherever it quotes a resource attribute.
	Message string
	// At is when the event occurred, from Options.Now.
	At time.Time
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/executor/ -v`
Expected: PASS — 3 tests.

Run: `go build ./... && go vet ./... && gofmt -l .`
Expected: everything green (there is nothing yet in this package for `Apply` to call, so only `build`/`vet`/`gofmt` and this package's own tests apply).

- [ ] **Step 5: Commit**

```bash
git add internal/executor/types.go internal/executor/types_test.go
git commit -m "$(cat <<'EOF'
feat: add internal/executor's Result, Options, RetryPolicy and Event types

Types only, no behavior: Result and Options match the M3 authoring
contract exactly, RetryPolicy's fields exist here because Options.Retry
needs the type even though Task 5 gives it behavior, and Event is
built to carry enough for a progress line — started, succeeded, failed,
retrying, skipped — without a value.Value leaf anywhere on it that
could bypass value.Format's redaction.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

### Task 5: the classification-aware retry loop

**Files:**
- Create: `internal/executor/retry.go`
- Test: `internal/executor/retry_test.go`

**Interfaces:**
- Consumes: `executor.RetryPolicy{MaxAttempts, Base, Max, Sleep, Jitter}` (Task 4, this package); `provider.Retryability` with constants `NotSafeToRetry`, `ConditionallyRetryable`, `SafeToRetry` (`pkg/provider`, existing).
- Produces: `executor.Verb` with constants `VerbRead`, `VerbCreate`, `VerbUpdate`, `VerbDelete` and `(Verb).String() string`; `func executor.Attempt(ctx context.Context, verb Verb, policy RetryPolicy, classify func(error) provider.Retryability, fn func() error) error`. **Task 8 (`Apply`) is the caller**, via its own `verbFor(node)` helper — not Task 6. An earlier version of this line named Task 6, which is wrong and would have had dispatch implement a second retry loop inside the one Task 8 already wraps it in. Dispatch makes the raw provider call; the worker pool decides whether to retry it and wires `classify` to that provider's `ClassifyError`. Keeping the retry decision in one layer is the point.

`Verb` is deliberately not `planner.OpKind`, even though both packages are already coupled through `Event.Op` and `Apply`'s own signature. `OpKind` names what the *plan* proposes for a resource — `OpReplace` most pointedly, which is not one provider call but two: a destroy-phase node and a create-phase node, dispatched separately. `OpForget` makes no provider call at all. Retry eligibility is decided per provider call, not per plan operation, so `Attempt` needs to be told which single verb is in flight for THIS call — a decision that belongs to Task 6's dispatch code, which already has to resolve an `OpNode`'s `(Kind, Phase)` pair down to one concrete provider method before it can call anything.

The rule that matters is `retryable`'s table, transcribed directly from spec §15:

| Verb   | `NotSafeToRetry` | `ConditionallyRetryable` | `SafeToRetry` |
|--------|------------------|--------------------------|---------------|
| Read   | no               | yes                      | yes           |
| Update | no               | yes                      | yes           |
| Create | no               | **no**                   | yes           |
| Delete | no               | **no**                   | yes           |

`Create`/`ConditionallyRetryable` is the cell that matters most: if the first attempt's provider call actually succeeded and only the response was lost — the textbook ambiguous failure — retrying creates a second real resource with nothing in state pointing at the first. That is how duplicate infrastructure appears, and no amount of retry ceiling or backoff fixes it once it has happened, so `Create` only ever retries on `SafeToRetry`, where the provider is asserting the first attempt is known not to have taken effect. `Delete` gets the identical answer for the mirror reason: a conditionally-retryable delete might already have removed the object, and retrying risks acting on whatever now occupies that identity rather than confirming an idempotent no-op. `Read` and `Update` carry no such asymmetry — both are naturally safe to repeat — so they retry on both of the non-zero classifications. `TestRetryableTableMatchesSpec` below pins all twelve cells of this table directly against `retryable`, and `TestAttemptNeverRetriesCreateOnAnAmbiguousFailure` additionally proves it end to end through `Attempt`, with a `Sleep` that fails the test outright if it is ever called — the only cell where a silent regression would be a real production incident rather than a wrong plan.

- [ ] **Step 1: Write the failing test**

Create `internal/executor/retry_test.go`:

```go
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
		func(context.Context) error { calls++; return boom })

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
		func(context.Context) error { return errors.New("boom") })
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/executor/ -run 'TestRetryable|TestBackoff|TestAttempt|TestDefault' -v`
Expected: FAIL to build — `undefined: Verb` (and `undefined: retryable`, `undefined: backoff`, `undefined: Attempt`, `undefined: defaultJitter`, `undefined: defaultSleep`).

- [ ] **Step 3: Implement**

Create `internal/executor/retry.go`:

```go
package executor

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"infra/pkg/provider"
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
type Verb uint8

const (
	// VerbRead is a provider Read call.
	VerbRead Verb = iota
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
func retryable(verb Verb, r provider.Retryability) bool {
	switch r {
	case provider.SafeToRetry:
		return true
	case provider.NotSafeToRetry:
		return false
	default: // provider.ConditionallyRetryable
		return verb == VerbRead || verb == VerbUpdate
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
	maxAttempts := policy.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

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
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/executor/ -v`
Expected: PASS — 17 tests (14 new in `retry_test.go`, plus the 3 from `types_test.go`).

Run: `go test ./... -race && go vet ./... && gofmt -l .`
Expected: everything green.

- [ ] **Step 5: Commit**

```bash
git add internal/executor/retry.go internal/executor/retry_test.go
git commit -m "$(cat <<'EOF'
feat: add the classification-aware retry loop

Attempt retries a provider call according to RetryPolicy, deciding
eligibility from a table keyed on which verb is being retried and how
the provider classified the failure (spec §15). Create only retries
on SafeToRetry — a retried create on an ambiguous failure is how
duplicate infrastructure appears — and Delete gets the same answer for
the mirror reason. Sleep and Jitter are both injectable so the tests
that pin this table run with no real waiting and no flakiness.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

### Task 6: dispatch one operation to its provider

**Files:**
- Create: `internal/executor/dispatch.go`
- Test: `internal/executor/dispatch_test.go`

**Interfaces:**
- Consumes: `planner.OpNode{Address, Kind, Phase}` and `(OpNode).ID()`
  (`internal/planner/execution.go`); `provider.Provider` (`pkg/provider`);
  `resource.ResourceState`, `resource.DesiredResource` (`pkg/resource`).
- Produces: `func dispatch(ctx context.Context, prov provider.Provider, node planner.OpNode, current *resource.ResourceState, desired *resource.DesiredResource) (*resource.ResourceState, error)`,
  consumed by Task 8's `run.execute`.

`OpNode.ID()` is not injective over `OpKind`: a replace is `destroy:<addr>`
then `create:<addr>`, two different ids at one address, and `OpKind` alone
(`OpReplace` for both) cannot tell them apart either. What *can* is the pair
already sitting on the node — `Kind` and `Phase` — so `dispatch` switches on
both together, not on `Kind` alone. Getting this wrong in one specific
direction is the dangerous one: an implementation keyed on `Kind` alone would
see `OpReplace` at both the destroy and the create phase and could easily
route the create phase into `Update` (reusing the just-destroyed object's
identity) instead of `Create` (allocating a new one) — which silently patches
whatever the delete missed rather than replacing it, defeating the entire
point of a replace. `TestDispatchReplaceCreatePhaseAllocatesANewObject` below
is built to catch exactly that.

`Before`/`After` on `planner.Operation` are `map[string]value.Value` —
attribute maps only, with no `ProviderID`. A provider cannot locate the
object it manages from attributes alone, so `dispatch` never reads
`op.Before` or `op.After` at all; `current *resource.ResourceState` (the live
state record, supplied by the caller) is what `Update` and `Delete` receive,
because it — not the plan's snapshot — carries the `ProviderID` a provider
call needs, and `Provider.Update`/`Provider.Delete`'s own signatures already
require exactly that type. `desired *resource.DesiredResource` (fully
resolved by Task 7, built by Task 8) is what `Create` and `Update` receive.
This is why `dispatch` takes `current` and `desired` as separate typed
arguments instead of an `Operation`: the type of what each provider method
needs settles which source supplies it, rather than leaving that decision to
whichever caller happens to reach in and read the wrong field.

`OpForget` makes no provider call at all — dropping a resource from
management without touching the real infrastructure is the whole point of
`lifecycle.retain` (spec §11, invariant 1). `dispatch` returns `(nil, nil)`
immediately for it, never touching `prov`, `current` or `desired` — safe to
call with `prov == nil` for exactly this reason, which
`TestDispatchForgetMakesNoProviderCall` proves directly with a provider
double that fails the test if any of its methods are ever reached.

- [ ] **Step 1: Write the failing test**

Create `internal/executor/dispatch_test.go`:

```go
package executor

import (
	"context"
	"path/filepath"
	"testing"

	"infra/internal/planner"
	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

// poisonProvider is a Provider double whose every method fails the test
// immediately. It exists to prove a negative: that dispatch never reaches
// the provider at all for an operation kind that must not call it.
type poisonProvider struct {
	t            *testing.T
	resourceType string
}

func (p poisonProvider) Name() string { return "poison" }
func (p poisonProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p poisonProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	p.t.Fatal("dispatch must not call the provider for this operation")
	return nil, nil
}
func (p poisonProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	p.t.Fatal("dispatch must not call the provider for this operation")
	return nil, nil
}
func (p poisonProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	p.t.Fatal("dispatch must not call the provider for this operation")
	return nil, nil
}
func (p poisonProvider) Delete(context.Context, *resource.ResourceState) error {
	p.t.Fatal("dispatch must not call the provider for this operation")
	return nil
}
func (p poisonProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p poisonProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p poisonProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = poisonProvider{}

func mustCreate(t *testing.T, prov *testprovider.Provider, name, resourceType string, attrs map[string]value.Value) *resource.ResourceState {
	t.Helper()
	st, err := prov.Create(context.Background(), &resource.DesiredResource{
		Address: address.Address{Name: name},
		Type:    resourceType,
		Attrs:   attrs,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return st
}

func TestDispatchForgetMakesNoProviderCall(t *testing.T) {
	node := planner.OpNode{Address: address.Address{Name: "old"}, Kind: planner.OpForget, Phase: planner.PhaseDestroy}
	current := &resource.ResourceState{Address: node.Address, Type: "test.network", ProviderID: "net-1"}

	result, err := dispatch(context.Background(), poisonProvider{t: t}, node, current, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result != nil {
		t.Errorf("result = %+v, want nil — forget records nothing new, it only drops the existing entry", result)
	}
}

func TestDispatchCreateCallsProviderCreate(t *testing.T) {
	dir := t.TempDir()
	prov := testprovider.New(filepath.Join(dir, "cloud.json"))

	node := planner.OpNode{Address: address.Address{Name: "net"}, Kind: planner.OpCreate, Phase: planner.PhaseCreate}
	desired := &resource.DesiredResource{
		Address: node.Address,
		Type:    "test.network",
		Attrs:   map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
	}

	got, err := dispatch(context.Background(), prov, node, nil, desired)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got == nil || got.ProviderID == "" {
		t.Fatalf("got = %+v, want a resource state with a provider ID", got)
	}
	if cidr, _ := got.Attributes["cidr"].AsString(); cidr != "10.0.0.0/16" {
		t.Errorf("cidr = %q", cidr)
	}
}

func TestDispatchUpdateReusesProviderID(t *testing.T) {
	dir := t.TempDir()
	prov := testprovider.New(filepath.Join(dir, "cloud.json"))
	current := mustCreate(t, prov, "net", "test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	})

	node := planner.OpNode{Address: current.Address, Kind: planner.OpUpdate, Phase: planner.PhaseCreate}
	desired := &resource.DesiredResource{
		Address: current.Address,
		Type:    "test.network",
		Attrs:   map[string]value.Value{"cidr": value.String("10.1.0.0/16", value.SourceExplicit)},
	}

	got, err := dispatch(context.Background(), prov, node, current, desired)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got.ProviderID != current.ProviderID {
		t.Errorf("ProviderID = %q, want unchanged %q — update must not allocate a new object", got.ProviderID, current.ProviderID)
	}
	if cidr, _ := got.Attributes["cidr"].AsString(); cidr != "10.1.0.0/16" {
		t.Errorf("cidr = %q, want the new value", cidr)
	}
}

func TestDispatchDestroyRemovesTheResource(t *testing.T) {
	dir := t.TempDir()
	prov := testprovider.New(filepath.Join(dir, "cloud.json"))
	current := mustCreate(t, prov, "net", "test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	})

	node := planner.OpNode{Address: current.Address, Kind: planner.OpDestroy, Phase: planner.PhaseDestroy}
	got, err := dispatch(context.Background(), prov, node, current, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil — Delete returns no state", got)
	}

	after, err := prov.Read(context.Background(), current)
	if err != nil {
		t.Fatalf("Read after destroy: %v", err)
	}
	if after != nil {
		t.Error("resource still exists at the provider after dispatch destroyed it")
	}
}

func TestDispatchReplaceCreatePhaseAllocatesANewObject(t *testing.T) {
	dir := t.TempDir()
	prov := testprovider.New(filepath.Join(dir, "cloud.json"))
	original := mustCreate(t, prov, "net", "test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	})

	// A replace is two nodes at the SAME address, distinguished only by
	// Phase — OpNode.ID() cannot tell them apart (contract). Drive both
	// through dispatch the way Task 8's worker pool will: destroy first,
	// then create, both carrying Kind == OpReplace.
	destroyNode := planner.OpNode{Address: original.Address, Kind: planner.OpReplace, Phase: planner.PhaseDestroy}
	if _, err := dispatch(context.Background(), prov, destroyNode, original, nil); err != nil {
		t.Fatalf("destroy phase: %v", err)
	}

	createNode := planner.OpNode{Address: original.Address, Kind: planner.OpReplace, Phase: planner.PhaseCreate}
	desired := &resource.DesiredResource{
		Address: original.Address,
		Type:    "test.network",
		Attrs:   map[string]value.Value{"cidr": value.String("10.2.0.0/16", value.SourceExplicit)},
	}
	got, err := dispatch(context.Background(), prov, createNode, nil, desired)
	if err != nil {
		t.Fatalf("create phase: %v", err)
	}

	// The defect this catches: an implementation keying its behaviour off
	// node.Kind alone would see OpReplace both times and could dispatch the
	// create phase as an Update reusing original's ProviderID instead of a
	// fresh Create — silently patching the OLD object rather than replacing
	// it, defeating the entire point of a replace. A reused ID here must
	// fail this assertion.
	if got.ProviderID == original.ProviderID {
		t.Fatalf("ProviderID = %q, same as the destroyed original %q — the create phase must allocate a new object, not update the old one", got.ProviderID, original.ProviderID)
	}
	if got.ProviderID == "" {
		t.Fatal("create phase produced no provider ID")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/executor/ -run TestDispatch -v`
Expected: FAIL to build — `undefined: dispatch` (`internal/executor/dispatch.go` does not exist yet).

- [ ] **Step 3: Implement**

Create `internal/executor/dispatch.go`:

```go
package executor

import (
	"context"
	"fmt"

	"infra/internal/planner"
	"infra/pkg/provider"
	"infra/pkg/resource"
)

// dispatch performs the one provider call an OpNode implies and returns the
// resulting resource state.
//
// It switches on (node.Kind, node.Phase) together, never node.Kind alone:
// OpNode.ID() is not injective over OpKind (a replace is two nodes,
// destroy:<addr> then create:<addr>, both carrying Kind == OpReplace), and
// neither is Kind by itself — Phase is what tells the two apart.
//
// current is the live resource.ResourceState the caller already holds for
// this address (nil when there is none, e.g. a plain create); it, not
// op.Before, is what Update and Delete receive, because Before is a bare
// map[string]value.Value with no ProviderID, and a provider cannot find the
// object it manages without one. desired is the fully-resolved
// DesiredResource Task 7 built from op.After; dispatch does not evaluate
// expressions itself.
//
// OpForget makes no provider call at all: dropping a resource from
// management without touching the real infrastructure is retain's entire
// point (spec §11, invariant 1). It returns (nil, nil) without touching
// prov, current or desired — safe to call with prov == nil for exactly this
// reason.
func dispatch(
	ctx context.Context,
	prov provider.Provider,
	node planner.OpNode,
	current *resource.ResourceState,
	desired *resource.DesiredResource,
) (*resource.ResourceState, error) {
	switch {
	case node.Kind == planner.OpForget:
		return nil, nil

	case node.Kind == planner.OpCreate,
		node.Kind == planner.OpReplace && node.Phase == planner.PhaseCreate:
		if desired == nil {
			return nil, fmt.Errorf("%s: dispatch: create requires a resolved desired resource", node.Address)
		}
		return prov.Create(ctx, desired)

	case node.Kind == planner.OpUpdate:
		if current == nil {
			return nil, fmt.Errorf("%s: dispatch: update requires the resource's current state", node.Address)
		}
		if desired == nil {
			return nil, fmt.Errorf("%s: dispatch: update requires a resolved desired resource", node.Address)
		}
		return prov.Update(ctx, current, desired)

	case node.Kind == planner.OpDestroy,
		node.Kind == planner.OpReplace && node.Phase == planner.PhaseDestroy:
		if current == nil {
			return nil, fmt.Errorf("%s: dispatch: destroy requires the resource's current state", node.Address)
		}
		return nil, prov.Delete(ctx, current)

	default:
		return nil, fmt.Errorf("%s: dispatch: unhandled operation kind %s phase %d", node.Address, node.Kind, int(node.Phase))
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/executor/ -run TestDispatch -v`
Expected: PASS — 5 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/executor/dispatch.go internal/executor/dispatch_test.go
git commit -m "$(cat <<'EOF'
feat: dispatch one plan operation to its provider call

Maps an OpNode's (Kind, Phase) pair to the Create/Update/Delete call it
implies, or no call at all for OpForget. Keyed on Phase as well as Kind
because a replace is two nodes at one address and Kind alone cannot tell
them apart — TestDispatchReplaceCreatePhaseAllocatesANewObject pins the
failure mode that distinction exists to prevent.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

### Task 7: resolve deferred expressions against apply-time state

**Files:**
- Create: `internal/executor/resolve.go`
- Test: `internal/executor/resolve_test.go`

**Interfaces:**
- Consumes: `expressions.Scope`, `expressions.Evaluate(e *value.Expr, scope Scope) (value.Value, diag.Diagnostics)` (`internal/expressions`); `planner.Operation{After}` (`internal/planner`); `resource.ResourceState{Attributes}` (`pkg/resource`); `value.Reference{Resource, Attribute}` (`pkg/value`).
- Produces: `func resolveAfter(op *planner.Operation, resources map[string]*resource.ResourceState, environment string) (map[string]value.Value, diag.Diagnostics)`,
  consumed by Task 8's `run.execute`.

§6 requires apply time to finish a deferred expression with **the same
evaluator** the compiler used, not a second one — `internal/expressions.Evaluate`
is that evaluator, and this task's only real job is supplying it the right
`Scope`. `internal/compiler/bind.go`'s `compileScope` is the model: it
resolves variables but reports every attribute reference unavailable, which
is what turns a reference into a deferred unknown carrying its expression in
the first place. `runtimeScope` below is its mirror — it resolves
`Attribute` against resources already applied earlier in this run, using the
exact `Scope` interface `compileScope` implements, so resolution at apply
time is provably the same evaluation `bindAttribute` did at compile time,
just with more of the world visible.

`resources` is a `map[string]*resource.ResourceState` snapshot, not a live
`*state.State` — see Task 8's `run.snapshot` for why a snapshot is what a
worker goroutine gets handed, and note that this makes `resolveAfter`
trivially unit-testable with a hand-built map, no `*state.State` required.

The interesting design question: the plan was computed with the referenced
attribute unknown, so `op.After[name].Expr` is what's available — never
`op.Before[name]`, which `resolveAfter` never reads. The **value** the
resolution produces comes from `resources`, i.e. the dependency's *real*
post-apply attributes (`Source: SourceProvider`), not from anything the plan
recorded. If that real value turns out to be something that would have
changed the operation's *kind* had it been known at plan time — an
attribute the planner would have called `ForceNew` turns out identical to
what was already there, say, making a proposed `OpReplace` pointless in
hindsight — **M3 does not re-derive `Kind`.** `resolveAfter` (and everything
downstream of it in Task 8) fills in the concrete attribute value for
whatever kind the plan already fixed; it never revisits whether that kind
was the right call. This is acceptable now for three reasons. First,
`value.Equal`'s own contract already treats unknown conservatively — "an
attribute that cannot be proven unchanged must be reported as a change" —
so the planner never *under*-proposes work from an unknown; the failure
mode this trade accepts is a redundant update or an unnecessary replace,
never a missed necessary one. Second, re-deriving `Kind` mid-apply would
mean re-running diff and dependency-graph logic inside the executor,
duplicating the planner and breaking the layering rule that keeps diff logic
out of `internal/executor`. Third, the cost of being wrong is bounded and
self-correcting: the next `infra plan` sees the true state and, per
invariant 2, proposes nothing for that resource — one wasted cycle, not a
persistent inconsistency. Re-planning mid-apply, with proper staleness
handling, is explicitly M6's job (contract: "Deliberately out of scope for
M3").

- [ ] **Step 1: Write the failing test**

Create `internal/executor/resolve_test.go`:

```go
package executor

import (
	"testing"

	"infra/internal/expressions"
	"infra/internal/planner"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

// planTimeScope reproduces the compiler's compile-time scope: it resolves
// "environment" the way every real compile does (internal/compiler/bind.go,
// variableScope), and reports every resource attribute as unavailable —
// which is what turns a reference into a deferred unknown carrying its
// expression (spec §6). Building fixtures through the real parser and
// evaluator, rather than a hand-built *value.Expr, is what proves
// resolveAfter is handed the exact shape of Value the compiler actually
// produces.
type planTimeScope struct{ environment string }

func (s planTimeScope) Variable(name string) (value.Value, bool) {
	if name == "environment" {
		return value.String(s.environment, value.SourceEnvironment), true
	}
	return value.Value{}, false
}

func (s planTimeScope) Attribute(value.Reference) (value.Value, bool) { return value.Value{}, false }

// deferredValue parses and evaluates src the way the compiler would, and
// fails the test if the result is anything other than a genuine deferred
// unknown — the only shape resolveAfter is meant to handle.
func deferredValue(t *testing.T, src string) value.Value {
	t.Helper()
	e, ds := expressions.Parse(src, value.Origin{File: "infra.yml", Line: 1})
	if ds.HasErrors() {
		t.Fatalf("Parse(%q): %+v", src, ds)
	}
	v, ds := expressions.Evaluate(e, planTimeScope{environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("Evaluate(%q): %+v", src, ds)
	}
	if v.Known {
		t.Fatalf("Evaluate(%q) is Known; the fixture must be unresolvable at compile time", src)
	}
	return v
}

func TestResolveAfterFillsUnknownFromDependency(t *testing.T) {
	op := &planner.Operation{
		Address: address.Address{Name: "app"},
		Type:    "test.application",
		After: map[string]value.Value{
			"network_id": deferredValue(t, "${net.id}"),
		},
	}
	snapshot := map[string]*resource.ResourceState{
		"net": {
			Address:    address.Address{Name: "net"},
			Attributes: map[string]value.Value{"id": value.String("net-1", value.SourceProvider)},
		},
	}

	got, ds := resolveAfter(op, snapshot, "dev")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	v := got["network_id"]
	if !v.Known {
		t.Fatal("network_id must be Known once its dependency has been applied")
	}
	if s, _ := v.AsString(); s != "net-1" {
		t.Errorf("network_id = %q, want \"net-1\"", s)
	}
}

func TestResolveAfterLeavesKnownValuesUntouched(t *testing.T) {
	op := &planner.Operation{
		Address: address.Address{Name: "app"},
		After: map[string]value.Value{
			"name": value.String("fixed", value.SourceExplicit),
		},
	}

	got, ds := resolveAfter(op, map[string]*resource.ResourceState{}, "dev")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s, _ := got["name"].AsString(); s != "fixed" {
		t.Errorf("name = %q, want \"fixed\" — an already-Known value must pass through untouched", s)
	}
}

func TestResolveAfterReportsUnresolvedDependencyAsDiagnostic(t *testing.T) {
	op := &planner.Operation{
		Address: address.Address{Name: "app"},
		After: map[string]value.Value{
			"network_id": deferredValue(t, "${net.id}"),
		},
	}

	// The dependency is missing from the snapshot entirely — the case a bug
	// in dependency ordering (spec §14) would produce.
	got, ds := resolveAfter(op, map[string]*resource.ResourceState{}, "dev")
	if !ds.HasErrors() {
		t.Fatal("expected a diagnostic: network_id cannot resolve without its dependency")
	}
	if got["network_id"].Known {
		t.Error("network_id must remain unknown, not silently fabricated")
	}
}

func TestResolveAfterMixesVariableAndResourceReference(t *testing.T) {
	// Unknownness is contagious (spec §6): the whole concat goes unknown
	// because net.id is unresolvable at compile time, even though
	// "environment" resolves immediately. The stored Expr is the ORIGINAL
	// tree, so re-evaluating it at apply time must resolve BOTH the
	// variable and the resource reference again — this is the test that
	// proves runtimeScope.Variable actually does that (Task 3 folds resolved parts into the deferred expression, so a residual
	// contains no variable reference and this scope needs no variables.)
	op := &planner.Operation{
		Address: address.Address{Name: "app"},
		After: map[string]value.Value{
			"name": deferredValue(t, "${environment}-${net.id}"),
		},
	}
	snapshot := map[string]*resource.ResourceState{
		"net": {
			Address:    address.Address{Name: "net"},
			Attributes: map[string]value.Value{"id": value.String("net-1", value.SourceProvider)},
		},
	}

	got, ds := resolveAfter(op, snapshot, "dev")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s, _ := got["name"].AsString(); s != "dev-net-1" {
		t.Errorf("name = %q, want \"dev-net-1\"", s)
	}
}

func TestResolveAfterUsesLiveSnapshotNotPlanTimeBefore(t *testing.T) {
	// Before carries what the plan diffed against — a stale, plan-time
	// snapshot. resolveAfter must never read it: only op.After (the
	// expression) and the live snapshot (the dependency's real, post-apply
	// attributes) may determine the resolved value.
	op := &planner.Operation{
		Address: address.Address{Name: "app"},
		Before: map[string]value.Value{
			"network_id": value.String("stale-would-be-wrong", value.SourceComputed),
		},
		After: map[string]value.Value{
			"network_id": deferredValue(t, "${net.id}"),
		},
	}
	snapshot := map[string]*resource.ResourceState{
		"net": {
			Address:    address.Address{Name: "net"},
			Attributes: map[string]value.Value{"id": value.String("net-1", value.SourceProvider)},
		},
	}

	got, ds := resolveAfter(op, snapshot, "dev")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s, _ := got["network_id"].AsString(); s != "net-1" {
		t.Errorf("network_id = %q, want the live value \"net-1\", not Before's stale one", s)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/executor/ -run TestResolveAfter -v`
Expected: FAIL to build — `undefined: resolveAfter`.

- [ ] **Step 3: Implement**

Create `internal/executor/resolve.go`:

```go
package executor

import (
	"sort"

	"infra/internal/diag"
	"infra/internal/expressions"
	"infra/internal/planner"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

// runtimeScope resolves expression references at apply time: attribute
// references against resources already applied earlier in this run (or
// present in state before it started), and the one variable the compiler
// always injects regardless of --var. It implements expressions.Scope so
// resolution goes through internal/expressions.Evaluate — the exact
// evaluator the compiler used to produce the unknown value in the first
// place (spec §6, §15) — rather than a second, hand-rolled evaluator that
// could disagree with it about what a function or a concatenation means.
//
// Variable only ever resolves "environment" because Task 3 folds every
// resolvable part into the deferred expression before it is stored. A
// residual therefore holds resource references and literals — never a
// variable — so the apply-time scope needs no variable table.
//
// resources is a snapshot, not a live *state.State; see Task 8's
// run.snapshot for why a snapshot is what a worker goroutine is handed.
type runtimeScope struct {
	resources   map[string]*resource.ResourceState
	environment string
}

func (s runtimeScope) Variable(name string) (value.Value, bool) {
	if name == "environment" {
		return value.String(s.environment, value.SourceEnvironment), true
	}
	return value.Value{}, false
}

func (s runtimeScope) Attribute(ref value.Reference) (value.Value, bool) {
	rs, ok := s.resources[(address.Address{Name: ref.Resource}).String()]
	if !ok {
		return value.Value{}, false
	}
	v, ok := rs.Attributes[ref.Attribute]
	return v, ok
}

// resolveAfter finishes every deferred expression in an operation's After
// attributes against resources already applied in this run. Already-Known
// attributes pass through untouched; a still-unknown value after evaluation
// becomes an error diagnostic, not a silently fabricated one — the resource
// it depends on should already have run, by dependency ordering (spec §14),
// so a value still unknown here means that ordering did not hold.
func resolveAfter(op *planner.Operation, resources map[string]*resource.ResourceState, environment string) (map[string]value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	scope := runtimeScope{resources: resources, environment: environment}

	names := make([]string, 0, len(op.After))
	for name := range op.After {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]value.Value, len(op.After))
	for _, name := range names {
		v := op.After[name]
		if v.Known {
			out[name] = v
			continue
		}
		resolved, evalDS := expressions.Evaluate(v.Expr, scope)
		ds.Extend(evalDS)
		if !resolved.Known && !evalDS.HasErrors() {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  op.Address.String() + ": " + name + " is still unknown after every dependency has run",
				Detail: "Expected " + name + " to resolve once its dependencies completed, but it did not. " +
					"This means the operation depends on a resource outside its own dependency graph, which " +
					"dependency ordering (spec §14) does not guarantee has run yet.",
				Origin:  v.Origin,
				Related: []address.Address{op.Address},
			})
		}
		out[name] = resolved
	}
	return out, ds
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/executor/ -v`
Expected: PASS — 10 tests (5 from Task 6, 5 from this task).

- [ ] **Step 5: Commit**

```bash
git add internal/executor/resolve.go internal/executor/resolve_test.go
git commit -m "$(cat <<'EOF'
feat: resolve deferred expressions at apply time

resolveAfter finishes an operation's still-unknown After attributes by
re-running internal/expressions.Evaluate — the compiler's own evaluator —
against a runtimeScope backed by resources already applied earlier in this
run. Reads only op.After, never op.Before: Before is the plan-time
snapshot the diff was computed against, and the resolved value must come
from what the dependency actually turned out to be, not from what the plan
recorded.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

### Task 8: `Apply` — the bounded worker pool

**Files:**
- Create: `internal/executor/apply.go`
- Test: `internal/executor/apply_test.go`

**Interfaces:**
- Consumes: `dispatch` (Task 6), `resolveAfter` (Task 7); `graph.Graph[T].Walk() (*Walk[T], error)`, `Walk.Ready() []T`, `Walk.Done(id string) []T`, `Walk.Skip(id string) []string`, `Walk.Remaining() int` (Group A Task 2); `executor.Result`, `executor.Options`, `executor.RetryPolicy`, `executor.EventKind` + constants, `executor.Event` (Group A Task 4); `executor.Verb`, `VerbCreate`, `VerbUpdate`, `VerbDelete`, `executor.Attempt(ctx, verb, policy, classify, fn) error` (Group A Task 5); `planner.Plan{Operations}`, `planner.OpNode`, `planner.BuildExecution` (`internal/planner`); `registry.Registry.Provider(resourceType string) (provider.Provider, bool)` (`internal/registry`); `resource.ResolvedResource.Desired() (DesiredResource, error)` (`pkg/resource`).
- Produces: `func Apply(ctx context.Context, p *planner.Plan, g *graph.Graph[planner.OpNode], st *state.State, opts Options) (Result, diag.Diagnostics)`,
  matching the contract exactly; `type run struct{...}` and its methods,
  modified in place by Task 9.

`graph.Walk` is documented "not safe for concurrent use; the executor owns
it from one goroutine and hands work out." `Apply`'s own goroutine — call it
the **owner** — is the only one that ever touches `w`, `st`, or the
scheduling bookkeeping (`queue`, `providerInFlight`, `r.inFlight`). Worker
goroutines, one per in-flight operation, receive exactly two things: the
`OpNode` to run and a **snapshot** — a shallow copy of `st.Resources` taken
by the owner at the moment it launches that worker — and return exactly one
thing, a `nodeResult`, over an unbuffered channel the owner alone reads.
This is what makes concurrent completions safe with no mutex around `st`:
only the owner ever calls `st.Set` / `st.Remove`, and it only does so
between receiving one `nodeResult` and dispatching the next batch — never
while a snapshot handed to a still-running worker is in use, because
`st.Set` and `st.Remove` only ever *replace* a map entry, never mutate a
`*resource.ResourceState` in place once it is stored
(`pkg/resource.ResourceState.Clone`'s own doc comment states this as the
invariant callers rely on). A shallow copy of the map is therefore a cheap,
race-free, point-in-time read for a worker to resolve deferred expressions
and locate a current `ResourceState` against, with no need to clone every
value inside it.

Bounding happens twice, and independently, at the moment the owner decides
what to launch next: a global counter (`r.inFlight` against
`opts.Parallelism`) and a per-provider counter (`providerInFlight[name]`
against `opts.PerProvider`), keyed by `prov.Name()` — never by resource
type, since two types can share one provider and one rate limit.
`OpForget` contributes to neither provider counter (empty string is never
checked against the bound), because it makes no provider call and has no
rate limit to protect. A node blocked only by its own provider's bound must
not block a different node behind it in the queue that happens to use a
different provider, so each dispatch pass scans the whole queue rather than
stopping at the first thing it cannot launch yet — this is what makes
per-provider bounding genuinely independent of the global one rather than a
second layer of the same serialization,
`TestApplyBoundsPerProviderIndependentlyOfGlobalParallelism` is built to
catch a shared-semaphore implementation that would pass every other test in
this file.

Determinism: nothing in `Result` may depend on which goroutine happened to
finish first. `Result.Applied` is assembled into a `map[string]address.Address`
during the run (so a replace's two completions at one address collapse into
one entry) and only turned into a sorted slice once, at the very end —
never appended in completion order.
`TestApplyResultOrderDoesNotDependOnCompletionOrder` picks delays so the
address that must sort *second* finishes *first*, the same
values-contradict-the-expected-order discipline the contract's test rules
require for an ordering assertion.

Retries go through `executor.Attempt` (Group A), not a second loop here:
`run.execute` maps `(node.Kind, node.Phase)` to a `Verb` — the same mapping
`dispatch` uses to pick a provider method, because retry eligibility is
decided per provider call, exactly as `Attempt`'s own doc says. `OpForget`
has no `Verb` (nothing to retry — no call is ever made), so `execute` calls
`dispatch` directly for it instead of through `Attempt`.

Progress events: `execute` emits `EventStarted` right before the dispatch
attempt and, via a single `defer` reading the function's named return
values, exactly one of `EventSucceeded` or `EventFailed` when it returns —
see the notes on `RetryPolicy.OnRetry` (Task 4) and on `Walk.Skip` returning
ids (Task 2) for `EventRetrying` and `EventSkipped`, which
this task does not emit and says why.

`ctx` cancellation stops the owner from launching **new** work — checked
fresh at the top of every dispatch pass — but does not touch anything
already dispatched; those goroutines run to completion on the same `ctx`
they were given, and whether they notice cancellation mid-call is between
them and whatever they called. This is deliberately the only hook Apply
itself needs: "finish the in-flight operation, then stop" (spec §15) is
just "cancel `ctx` after the current wave" from the caller's side. Apply
knows nothing about signals — that is Group C's Task 11 to wire, against
this same `ctx`.

- [ ] **Step 1: Write the failing test**

Create `internal/executor/apply_test.go`:

```go
package executor

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"infra/internal/planner"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
)

func addr(name string) address.Address { return address.Address{Name: name} }

func op(a address.Address, resourceType string, kind planner.OpKind) planner.Operation {
	return planner.Operation{Address: a, Type: resourceType, Kind: kind}
}

func planWith(ops ...planner.Operation) *planner.Plan {
	return &planner.Plan{Version: planner.PlanVersion, Operations: ops}
}

func noDeps(address.Address) []address.Address { return nil }

func depsFrom(m map[string][]string) func(address.Address) []address.Address {
	return func(a address.Address) []address.Address {
		var out []address.Address
		for _, name := range m[a.Name] {
			out = append(out, addr(name))
		}
		return out
	}
}

func newLockedBackend(t *testing.T, environment string) *state.Local {
	t.Helper()
	backend := state.NewLocal(t.TempDir())
	if _, err := backend.Lock(context.Background(), environment); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	return backend
}

// sequencedProvider is a Provider double whose Create can be given a
// per-address delay and records the order calls actually completed in, so
// tests can assert real ordering directly instead of inferring it from
// wall-clock timing.
type sequencedProvider struct {
	resourceType string
	delays       map[string]time.Duration

	mu    sync.Mutex
	order []string
}

func (p *sequencedProvider) Name() string { return "sequenced" }
func (p *sequencedProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *sequencedProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	time.Sleep(p.delays[d.Address.String()])
	p.mu.Lock()
	p.order = append(p.order, d.Address.String())
	p.mu.Unlock()
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *sequencedProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *sequencedProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *sequencedProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *sequencedProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *sequencedProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *sequencedProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = (*sequencedProvider)(nil)

func TestApplyRunsDependenciesBeforeDependents(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &sequencedProvider{resourceType: "test.thing", delays: map[string]time.Duration{}}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// alpha depends on zeta, so zeta must run first — but "alpha" sorts
	// BEFORE "zeta" alphabetically, so an implementation dispatching in
	// address order instead of dependency order would still (wrongly) pass
	// a fixture that happened to agree with alphabetical order.
	plan := planWith(
		op(addr("zeta"), "test.thing", planner.OpCreate),
		op(addr("alpha"), "test.thing", planner.OpCreate),
	)
	g, err := planner.BuildExecution(plan, depsFrom(map[string][]string{"zeta": {"alpha"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v", result.Failed)
	}

	prov.mu.Lock()
	order := append([]string(nil), prov.order...)
	prov.mu.Unlock()
	if len(order) != 2 || order[0] != "zeta" || order[1] != "alpha" {
		t.Fatalf("order = %v, want [zeta alpha] — a resource must be created after what it depends on", order)
	}
}

func TestApplyResultOrderDoesNotDependOnCompletionOrder(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &sequencedProvider{resourceType: "test.thing", delays: map[string]time.Duration{
		"a": 40 * time.Millisecond,
		"b": 0,
	}}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("a"), "test.thing", planner.OpCreate), op(addr("b"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	prov.mu.Lock()
	order := append([]string(nil), prov.order...)
	prov.mu.Unlock()
	if len(order) != 2 || order[0] != "b" || order[1] != "a" {
		t.Fatalf("test setup broken: completion order = %v, want [b a] — b must finish first for this test to prove anything", order)
	}

	want := []address.Address{addr("a"), addr("b")}
	if !reflect.DeepEqual(result.Applied, want) {
		t.Errorf("Applied = %v, want %v — sorted by address regardless of which finished first", result.Applied, want)
	}
}

// barrierProvider blocks inside Create on an unbuffered send until the test
// receives it, giving a hard, race-free proof that two Creates were both
// mid-flight at once — not an inference from elapsed time.
type barrierProvider struct {
	resourceType string
	started      chan struct{}

	mu            sync.Mutex
	concurrent    int
	maxConcurrent int
}

func (p *barrierProvider) Name() string { return "barrier" }
func (p *barrierProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *barrierProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.concurrent++
	if p.concurrent > p.maxConcurrent {
		p.maxConcurrent = p.concurrent
	}
	p.mu.Unlock()

	p.started <- struct{}{}

	p.mu.Lock()
	p.concurrent--
	p.mu.Unlock()
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *barrierProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *barrierProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *barrierProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *barrierProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *barrierProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *barrierProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = (*barrierProvider)(nil)

func TestApplyRunsIndependentOperationsConcurrently(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &barrierProvider{resourceType: "test.thing", started: make(chan struct{})}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("a"), "test.thing", planner.OpCreate), op(addr("b"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	done := make(chan struct{})
	go func() {
		defer close(done)
		Apply(context.Background(), plan, g, st, Options{
			Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
		})
	}()

	// Each Create increments concurrent (and maxConcurrent) BEFORE
	// blocking on the send, so by the time both of these receives have
	// completed, both increments already happened — a hard, race-free
	// guarantee, not an inference from timing.
	<-prov.started
	<-prov.started
	<-done

	prov.mu.Lock()
	max := prov.maxConcurrent
	prov.mu.Unlock()
	if max < 2 {
		t.Fatalf("maxConcurrent = %d, want 2 — independent operations must overlap, not run one at a time", max)
	}
}

// countingProvider tracks concurrent Create calls with an artificial delay,
// for bounding tests where a hard barrier isn't needed.
type countingProvider struct {
	resourceType string
	delay        time.Duration

	mu            sync.Mutex
	concurrent    int
	maxConcurrent int
}

func (p *countingProvider) Name() string { return "counting" }
func (p *countingProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *countingProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.concurrent++
	if p.concurrent > p.maxConcurrent {
		p.maxConcurrent = p.concurrent
	}
	p.mu.Unlock()

	time.Sleep(p.delay)

	p.mu.Lock()
	p.concurrent--
	p.mu.Unlock()
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *countingProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *countingProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *countingProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *countingProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *countingProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *countingProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = (*countingProvider)(nil)

func TestApplyBoundsGlobalParallelism(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &countingProvider{resourceType: "test.thing", delay: 50 * time.Millisecond}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var ops []planner.Operation
	for i := 0; i < 6; i++ {
		ops = append(ops, op(addr(fmt.Sprintf("r%d", i)), "test.thing", planner.OpCreate))
	}
	plan := planWith(ops...)
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 3, PerProvider: 3, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Applied) != 6 {
		t.Fatalf("Applied = %v, want 6 addresses", result.Applied)
	}

	prov.mu.Lock()
	max := prov.maxConcurrent
	prov.mu.Unlock()
	if max > 3 {
		t.Errorf("max concurrent = %d, want at most 3", max)
	}
	if max < 2 {
		t.Errorf("max concurrent = %d, want at least 2 — operations should overlap, not run one at a time", max)
	}
}

// sharedCounters is shared by two boundedProviders standing in for two
// different providers, so a single test can observe both a per-provider
// cap and cross-provider overlap at once.
type sharedCounters struct {
	mu             sync.Mutex
	perProvider    map[string]int
	maxPerProvider map[string]int
	global         int
	maxGlobal      int
}

type boundedProvider struct {
	name         string
	resourceType string
	delay        time.Duration
	shared       *sharedCounters
}

func (p *boundedProvider) Name() string { return p.name }
func (p *boundedProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *boundedProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	s := p.shared
	s.mu.Lock()
	s.perProvider[p.name]++
	if s.perProvider[p.name] > s.maxPerProvider[p.name] {
		s.maxPerProvider[p.name] = s.perProvider[p.name]
	}
	s.global++
	if s.global > s.maxGlobal {
		s.maxGlobal = s.global
	}
	s.mu.Unlock()

	time.Sleep(p.delay)

	s.mu.Lock()
	s.perProvider[p.name]--
	s.global--
	s.mu.Unlock()
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *boundedProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *boundedProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *boundedProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *boundedProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *boundedProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *boundedProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = (*boundedProvider)(nil)

func TestApplyBoundsPerProviderIndependentlyOfGlobalParallelism(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	shared := &sharedCounters{perProvider: map[string]int{}, maxPerProvider: map[string]int{}}
	provX := &boundedProvider{name: "x", resourceType: "test.x", delay: 50 * time.Millisecond, shared: shared}
	provY := &boundedProvider{name: "y", resourceType: "test.y", delay: 50 * time.Millisecond, shared: shared}
	reg := registry.New()
	if err := reg.Register(provX); err != nil {
		t.Fatalf("Register x: %v", err)
	}
	if err := reg.Register(provY); err != nil {
		t.Fatalf("Register y: %v", err)
	}

	plan := planWith(
		op(addr("x0"), "test.x", planner.OpCreate),
		op(addr("x1"), "test.x", planner.OpCreate),
		op(addr("y0"), "test.y", planner.OpCreate),
		op(addr("y1"), "test.y", planner.OpCreate),
	)
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 4, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Applied) != 4 {
		t.Fatalf("Applied = %v, want 4 addresses", result.Applied)
	}

	shared.mu.Lock()
	maxX, maxY, maxGlobal := shared.maxPerProvider["x"], shared.maxPerProvider["y"], shared.maxGlobal
	shared.mu.Unlock()

	if maxX > 1 {
		t.Errorf("provider x max concurrent = %d, want at most 1 — PerProvider: 1", maxX)
	}
	if maxY > 1 {
		t.Errorf("provider y max concurrent = %d, want at most 1 — PerProvider: 1", maxY)
	}
	if maxGlobal < 2 {
		t.Errorf("global max concurrent = %d, want at least 2 — two different providers, each capped at 1, must still run at the same time as each other", maxGlobal)
	}
}

// failingProvider fails Create for a fixed set of addresses.
type failingProvider struct {
	resourceType string
	failAddrs    map[string]bool
}

func (p *failingProvider) Name() string { return "failing" }
func (p *failingProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *failingProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	if p.failAddrs[d.Address.String()] {
		return nil, fmt.Errorf("simulated failure for %s", d.Address)
	}
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *failingProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *failingProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *failingProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *failingProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *failingProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *failingProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = (*failingProvider)(nil)

func TestApplyFailureSkipsDependentsButNotIndependentBranches(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &failingProvider{resourceType: "test.thing", failAddrs: map[string]bool{"root": true}}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(
		op(addr("root"), "test.thing", planner.OpCreate),
		op(addr("child"), "test.thing", planner.OpCreate),
		op(addr("cousin"), "test.thing", planner.OpCreate),
	)
	g, err := planner.BuildExecution(plan, depsFrom(map[string][]string{"root": {"child"}}))
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
	})
	_ = ds

	if len(result.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly one entry", result.Failed)
	}
	if _, ok := result.Failed["create:root"]; !ok {
		t.Errorf("Failed = %v, want the key \"create:root\"", result.Failed)
	}

	wantSkipped := []string{"create:child"}
	if !reflect.DeepEqual(result.Skipped, wantSkipped) {
		t.Errorf("Skipped = %v, want exactly %v — not merely containing it", result.Skipped, wantSkipped)
	}

	wantApplied := []address.Address{addr("cousin")}
	if !reflect.DeepEqual(result.Applied, wantApplied) {
		t.Errorf("Applied = %v, want exactly %v — root failed and child was skipped, neither belongs here", result.Applied, wantApplied)
	}
}

func TestApplyDoesNotDispatchWhenContextIsAlreadyCancelled(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	reg := registry.New()
	if err := reg.Register(poisonProvider{t: t, resourceType: "test.thing"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("a"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, _ := Apply(ctx, plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
	})
	if len(result.Applied) != 0 {
		t.Errorf("Applied = %v, want none — the provider must never be called once ctx is already cancelled", result.Applied)
	}
	if _, ok := st.Get(addr("a")); ok {
		t.Error("state must not contain a — nothing was ever dispatched")
	}
}

// flakyProvider fails Create a fixed number of times, then succeeds.
type flakyProvider struct {
	resourceType string
	failures     int

	mu    sync.Mutex
	calls int
}

func (p *flakyProvider) Name() string { return "flaky" }
func (p *flakyProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p *flakyProvider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n <= p.failures {
		return nil, fmt.Errorf("transient failure #%d", n)
	}
	return &resource.ResourceState{Address: d.Address, Type: d.Type, Provider: p.Name(), ProviderID: d.Address.String(), Attributes: d.Attrs}, nil
}
func (p *flakyProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *flakyProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *flakyProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *flakyProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p *flakyProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *flakyProvider) ClassifyError(error) provider.Retryability { return provider.SafeToRetry }

var _ provider.Provider = (*flakyProvider)(nil)

func TestApplyRetriesAccordingToPolicy(t *testing.T) {
	backend := newLockedBackend(t, "dev")
	prov := &flakyProvider{resourceType: "test.thing", failures: 2}
	reg := registry.New()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	plan := planWith(op(addr("a"), "test.thing", planner.OpCreate))
	g, err := planner.BuildExecution(plan, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	st := state.New("proj", "dev")

	result, ds := Apply(context.Background(), plan, g, st, Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend, Environment: "dev",
		Retry: RetryPolicy{MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("Failed = %v, want none — the third attempt should have succeeded", result.Failed)
	}
	if len(result.Applied) != 1 {
		t.Fatalf("Applied = %v, want one address", result.Applied)
	}

	prov.mu.Lock()
	calls := prov.calls
	prov.mu.Unlock()
	if calls != 3 {
		t.Errorf("Create called %d times, want 3 — two failures then a success, wired through opts.Retry", calls)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/executor/ -run TestApply -v`
Expected: FAIL to build — `undefined: Apply` (`internal/executor/apply.go`
does not exist yet).

- [ ] **Step 3: Implement**

Create `internal/executor/apply.go`:

```go
package executor

import (
	"context"
	"fmt"
	"sort"
	"time"

	"infra/internal/diag"
	"infra/internal/graph"
	"infra/internal/planner"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/provider"
	"infra/pkg/resource"
)

// Apply drains the execution graph, dispatching each operation's provider
// call through a worker pool bounded twice — globally by opts.Parallelism
// and per provider by opts.PerProvider.
//
// Ownership: this function's own goroutine (the owner) is the only one that
// ever touches w, st, or the scheduling bookkeeping below. Worker
// goroutines receive a read-only snapshot of st and the single OpNode they
// were handed (run.launch), do the provider call (run.execute), and send a
// nodeResult back on a channel; they touch nothing shared. The owner is the
// only reader of that channel, and it is the only place st.Set / st.Remove
// are called (run.record) — always between receiving one nodeResult and
// dispatching the next batch, never while a snapshot in use by a
// still-running worker could be affected, because st.Set/st.Remove only
// ever replace a map entry rather than mutate a *resource.ResourceState in
// place (see run.snapshot).
func Apply(ctx context.Context, p *planner.Plan, g *graph.Graph[planner.OpNode], st *state.State, opts Options) (Result, diag.Diagnostics) {
	var ds diag.Diagnostics
	result := Result{Failed: map[string]error{}, State: st}

	if opts.Parallelism < 1 {
		opts.Parallelism = 1
	}
	if opts.PerProvider < 1 {
		opts.PerProvider = 1
	}

	w, err := g.Walk()
	if err != nil {
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: "cannot execute plan: " + err.Error()})
		return result, ds
	}

	r := &run{
		ctx:     ctx,
		st:      st,
		ops:     indexOperations(p),
		opts:    opts,
		results: make(chan nodeResult),
	}

	queue := w.Ready()
	providerInFlight := map[string]int{}
	appliedSet := map[string]address.Address{}
	var skipped []string

	for w.Remaining() > 0 {
		stopping := ctx.Err() != nil

		var deferred []planner.OpNode
		for _, node := range queue {
			if stopping || r.inFlight >= opts.Parallelism {
				deferred = append(deferred, node)
				continue
			}
			providerName := r.providerNameFor(node)
			if providerName != "" && providerInFlight[providerName] >= opts.PerProvider {
				deferred = append(deferred, node)
				continue
			}
			r.launch(node, providerName)
			providerInFlight[providerName]++
		}
		queue = deferred

		if r.inFlight == 0 {
			if stopping {
				// Nothing running and nothing will be launched: whatever
				// is left in queue, or still blocked deeper in the graph,
				// is simply never attempted. It is deliberately absent
				// from every Result field — Applied and Failed both
				// require an attempt, and Skipped is specifically the set
				// graph.Walk.Skip returns for a failed dependency, not
				// "everything an early stop left behind."
				break
			}
			if len(queue) == 0 {
				// Every remaining node is blocked on something that will
				// never complete. g.Walk() already rejected a cyclic
				// graph above, so this guards a future bug in this
				// loop's own bookkeeping, not a state reachable today.
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  fmt.Sprintf("executor: %d operation(s) remain but none are ready or running", w.Remaining()),
				})
				break
			}
		}

		res := <-r.results
		r.inFlight--
		if res.providerName != "" {
			providerInFlight[res.providerName]--
		}

		if res.err != nil {
			result.Failed[res.node.ID()] = res.err
			skipped = append(skipped, w.Skip(res.node.ID())...)
			continue
		}

		r.record(res)
		appliedSet[res.node.Address.String()] = res.node.Address
		queue = append(queue, w.Done(res.node.ID())...)
	}

	applied := make([]address.Address, 0, len(appliedSet))
	for _, a := range appliedSet {
		applied = append(applied, a)
	}
	address.Sort(applied)
	result.Applied = applied

	sort.Strings(skipped)
	result.Skipped = skipped
	return result, ds
}

// run holds the mutable bookkeeping one Apply call owns.
type run struct {
	ctx  context.Context
	st   *state.State
	ops  map[string]*planner.Operation
	opts Options

	results  chan nodeResult
	inFlight int
}

// nodeResult is what a worker goroutine sends back to the owner when one
// operation finishes.
type nodeResult struct {
	node         planner.OpNode
	state        *resource.ResourceState
	removed      bool
	err          error
	providerName string
}

// providerNameFor reports the provider name node's operation will call
// through, or "" for OpForget, which calls no provider and so is bounded
// only by --parallelism, never by a per-provider semaphore protecting rate
// limits it never touches.
func (r *run) providerNameFor(node planner.OpNode) string {
	if node.Kind == planner.OpForget {
		return ""
	}
	op, ok := r.ops[node.Address.String()]
	if !ok {
		return ""
	}
	prov, ok := r.opts.Registry.Provider(op.Type)
	if !ok {
		return ""
	}
	return prov.Name()
}

// launch takes a snapshot of state and starts one worker goroutine for
// node. Called only from the owner goroutine.
func (r *run) launch(node planner.OpNode, providerName string) {
	r.inFlight++
	snapshot := r.snapshot()
	go func() {
		st, removed, err := r.execute(node, snapshot)
		r.results <- nodeResult{node: node, state: st, removed: removed, err: err, providerName: providerName}
	}()
}

// snapshot copies the resource map's entries, not their contents, into a
// fresh map a worker goroutine can read without racing a later st.Set or
// st.Remove on the owner goroutine. Set and Remove only ever replace a map
// entry — nothing in this codebase mutates a *resource.ResourceState in
// place once it is stored — so a shallow copy is a safe, cheap, read-only
// view of state as of the moment it was taken.
func (r *run) snapshot() map[string]*resource.ResourceState {
	out := make(map[string]*resource.ResourceState, len(r.st.Resources))
	for k, v := range r.st.Resources {
		out[k] = v
	}
	return out
}

// record applies a successful completion to state. Called only from the
// owner goroutine, which is what makes an unsynchronised st.Set / st.Remove
// here safe. Modified by Task 9 to also persist.
func (r *run) record(res nodeResult) {
	if res.removed {
		r.st.Remove(res.node.Address)
		return
	}
	if res.state != nil {
		r.st.Set(res.state)
	}
}

func (r *run) now() time.Time {
	if r.opts.Now != nil {
		return r.opts.Now()
	}
	return time.Now()
}

func (r *run) emit(e Event) {
	if r.opts.OnEvent != nil {
		r.opts.OnEvent(e)
	}
}

// execute resolves an operation's deferred expressions (Task 7) and
// dispatches its provider call (Task 6) through executor.Attempt, which
// retries according to opts.Retry and the classification table in
// retry.go. It runs on a worker goroutine and reads only its own
// arguments — never r.st — which is exactly what the snapshot exists to
// make possible.
func (r *run) execute(node planner.OpNode, snapshot map[string]*resource.ResourceState) (result *resource.ResourceState, removed bool, err error) {
	op, ok := r.ops[node.Address.String()]
	if !ok {
		return nil, false, fmt.Errorf("%s: no operation in the plan for this node", node.Address)
	}

	defer func() {
		if err != nil {
			r.emit(Event{Kind: EventFailed, Address: node.Address, Op: op.Kind, Err: err, At: r.now()})
			return
		}
		r.emit(Event{Kind: EventSucceeded, Address: node.Address, Op: op.Kind, At: r.now()})
	}()

	current := snapshot[node.Address.String()]

	var prov provider.Provider
	if node.Kind != planner.OpForget {
		p, ok := r.opts.Registry.Provider(op.Type)
		if !ok {
			return nil, false, fmt.Errorf("%s: no provider registered for type %q", node.Address, op.Type)
		}
		prov = p
	}

	var desired *resource.DesiredResource
	if needsDesired(node) {
		after, resolveDS := resolveAfter(op, snapshot, r.opts.Environment)
		if resolveDS.HasErrors() {
			return nil, false, fmt.Errorf("%s: could not resolve deferred values: %s", node.Address, firstErrorSummary(resolveDS))
		}
		lifecycle := resource.Lifecycle{}
		if node.Kind == planner.OpUpdate && current != nil {
			// Operation deliberately carries no Lifecycle: the planner already
			// encodes every lifecycle decision into the operation kind — retain
			// becomes OpForget, prevent_destroy produces no operation at all — so
			// enforcing it again here would be a second enforcement point that can
			// disagree with the first. An
			// update carries forward whatever is already on record.
			lifecycle = current.Lifecycle
		}
		d, desiredErr := (resource.ResolvedResource{Address: node.Address, Type: op.Type, Attrs: after, Lifecycle: lifecycle}).Desired()
		if desiredErr != nil {
			return nil, false, desiredErr
		}
		desired = &d
	}

	r.emit(Event{Kind: EventStarted, Address: node.Address, Op: op.Kind, Attempt: 1, At: r.now()})

	verb, hasVerb := verbFor(node)
	if !hasVerb {
		// OpForget: no provider call, so nothing to retry through Attempt.
		result, err = dispatch(r.ctx, prov, node, current, desired)
	} else {
		err = Attempt(r.ctx, verb, r.opts.Retry, prov.ClassifyError, func() error {
			var derr error
			result, derr = dispatch(r.ctx, prov, node, current, desired)
			return derr
		})
	}
	if err != nil {
		return nil, false, err
	}

	removed = node.Kind == planner.OpForget || node.Kind == planner.OpDestroy ||
		(node.Kind == planner.OpReplace && node.Phase == planner.PhaseDestroy)
	return result, removed, nil
}

// verbFor maps an OpNode to the provider verb its dispatch call will use —
// the same (Kind, Phase) mapping dispatch itself switches on — so Attempt's
// retry classification is decided per provider call, as its own doc
// requires. OpForget has no verb: no call is made, nothing to retry.
func verbFor(node planner.OpNode) (Verb, bool) {
	switch {
	case node.Kind == planner.OpCreate,
		node.Kind == planner.OpReplace && node.Phase == planner.PhaseCreate:
		return VerbCreate, true
	case node.Kind == planner.OpUpdate:
		return VerbUpdate, true
	case node.Kind == planner.OpDestroy,
		node.Kind == planner.OpReplace && node.Phase == planner.PhaseDestroy:
		return VerbDelete, true
	default:
		return 0, false
	}
}

func needsDesired(node planner.OpNode) bool {
	return node.Kind == planner.OpCreate || node.Kind == planner.OpUpdate ||
		(node.Kind == planner.OpReplace && node.Phase == planner.PhaseCreate)
}

func indexOperations(p *planner.Plan) map[string]*planner.Operation {
	out := make(map[string]*planner.Operation, len(p.Operations))
	for i := range p.Operations {
		out[p.Operations[i].Address.String()] = &p.Operations[i]
	}
	return out
}

func firstErrorSummary(ds diag.Diagnostics) string {
	for _, d := range ds {
		if d.Severity == diag.SeverityError {
			return d.Summary
		}
	}
	return "unknown error"
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/executor/ -race -v`
Expected: PASS — 18 tests (5 from Task 6, 5 from Task 7, 8 from this task).
The `-race` flag matters here more than anywhere else in this group: it is
what proves `run.snapshot`'s no-mutex argument actually holds.

- [ ] **Step 5: Commit**

```bash
git add internal/executor/apply.go internal/executor/apply_test.go
git commit -m "$(cat <<'EOF'
feat: Apply — bounded concurrent worker pool draining the execution graph

One owner goroutine holds graph.Walk and *state.State exclusively; worker
goroutines receive a snapshot and a single OpNode, dispatch it through
Task 6/6 and executor.Attempt, and report back over a channel only the
owner reads. Bounded twice — globally by Parallelism, per provider by
PerProvider, keyed by provider name so one provider's rate limit cannot
starve or be starved by an unrelated one. Result.Applied is sorted once at
the end, never appended in completion order.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

### Task 9: persist state after every operation

**Files:**
- Modify: `internal/executor/apply.go` (`run.record` and the success branch
  of `Apply`'s owner loop that calls it)
- Test: `internal/executor/persist_test.go`

**Interfaces:**
- Consumes: `state.Local.Put(ctx, environment string, s *State) error`
  (atomic; stamps and restores `Serial`; refuses to write without a held
  lock — Group A Task 1); `state.Local.Lock(ctx, environment) (Lock, error)`.
- Produces: `func (r *run) record(res nodeResult) error` (signature change
  from Task 8's `func (r *run) record(res nodeResult)`), consumed only by
  `Apply`'s own success branch.

§15, verbatim: "A crash then leaves state that accurately describes
reality. Batching writes until completion guarantees the opposite precisely
when accuracy matters most." Task 8's `record` already does the in-memory
half of that (`st.Set` / `st.Remove`) on the owner goroutine, once per
completed operation, never batched — this task adds the missing half:
calling `opts.Backend.Put` in the same place, so a write lands on disk
before the owner goes back to dispatching more work. Because `record` runs
only on the owner goroutine, and the owner never dispatches new work
without first draining `r.results` for whatever it is currently waiting on,
successive `Put` calls happen one at a time in the order operations
complete — which is what "`Serial` incrementing each time, under the one
lock held for the whole run" actually requires in practice: `Put` itself
increments `Serial` on every call (`internal/state/local.go`), so this task
does not need to touch `Serial` directly, only make sure `Put` is called at
all, and exactly once per completed operation.

Ownership of the lock itself is **not** this task's job, and not
`Apply`'s: `Options` carries a `Backend *state.Local` and an `Environment
string`, never a `state.Lock` value or an acquire/release call. `Put`'s own
held-lock check (Group A Task 1, `requireOwnLock`) re-reads the lock file
and compares PID and host against the current process — so as long as
*something* in this process called `Backend.Lock(ctx, environment)` before
`Apply` runs, every `Put` inside it succeeds; `Apply` neither knows nor
cares who did that. The natural owner is whichever CLI command wraps
`Apply` (`apply`/`destroy`/`refresh`, spec §9.2), which needs the lock held
across the *whole* run regardless of how it ends — including the SIGINT
path Group C's Task 11 adds, which persists state and releases the lock
*after* `Apply` has already stopped. A `Put` failure is not this run's only
concern once it happens: what happens to `st` is answered below, but the
lock is unaffected by it either way — this task never calls `Unlock`.

What happens to `*state.State` when a `Put` fails mid-run: `st.Set` /
`st.Remove` already ran by the time `record` calls `Put`, and what they
recorded is true — the resource really was created, updated or destroyed.
Rolling that back to match a stale disk file would make `Result.State` lie
about reality, which is strictly worse than a state file that is one write
behind it: the in-memory value is exactly what a caller needs to retry the
write, or to understand what actually happened. So the mutation stands.
What a `Put` failure changes is whether the **run continues**: `Apply`
stops handing out *new* work — the same `stopping` gate `ctx` cancellation
already uses in Task 8's loop, extended with `|| persistFailed` — while
letting anything already dispatched finish and be recorded (and have its
own `Put` attempted; a transient failure might not repeat). Continuing to
schedule new operations after a write has already failed once would only
widen the gap between disk and reality for a cause — disk full, permission
revoked, backend unreachable — that a different resource's write is not
going to escape either. The diagnostic `Apply` returns says exactly this:
the operation succeeded, `Result.State` reflects it, and the run stopped
rather than compounding an already-unpersisted state.

- [ ] **Step 1: Write the failing test**

Create `internal/executor/persist_test.go`:

```go
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
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/executor/ -run TestApplyPersists -v`
Expected: FAIL — `TestApplyPersistsStateAfterEveryOperationNotJustAtTheEnd`
times out waiting for `sawIntermediate` (Task 8's `record` never calls
`Put`, so `backend.Get` returns an empty, never-written state for the
entire run).

- [ ] **Step 3: Implement**

In `internal/executor/apply.go`, replace the `record` method and the
success branch of `Apply`'s owner loop that calls it with the following.
Nothing else in the file changes.

`record`'s new signature:

```go
// record applies a successful completion to state and persists
// immediately — not batched until the run ends (spec §15: "A crash then
// leaves state that accurately describes reality. Batching writes until
// completion guarantees the opposite precisely when accuracy matters
// most."). Called only from the owner goroutine, which both makes the
// unsynchronised st.Set/st.Remove safe (Apply's doc comment) and is what
// serializes successive Put calls without an extra lock: the owner never
// dispatches new work before finishing this one.
//
// A Put failure does not undo the mutation: st.Set/st.Remove already ran,
// and what they recorded is true. Rolling it back to match a stale disk
// file would make Result.State lie about reality, which is worse than a
// state file that is one write behind it. The caller (Apply's loop) is
// what decides the run stops after this — see Apply's stopping gate.
func (r *run) record(res nodeResult) error {
	if res.removed {
		r.st.Remove(res.node.Address)
	} else if res.state != nil {
		r.st.Set(res.state)
	}
	return r.opts.Backend.Put(r.ctx, r.opts.Environment, r.st)
}
```

`Apply`'s owner loop, with the stopping condition and the success branch
both updated:

```go
	queue := w.Ready()
	providerInFlight := map[string]int{}
	appliedSet := map[string]address.Address{}
	var skipped []string
	persistFailed := false

	for w.Remaining() > 0 {
		stopping := ctx.Err() != nil || persistFailed

		var deferred []planner.OpNode
		for _, node := range queue {
			if stopping || r.inFlight >= opts.Parallelism {
				deferred = append(deferred, node)
				continue
			}
			providerName := r.providerNameFor(node)
			if providerName != "" && providerInFlight[providerName] >= opts.PerProvider {
				deferred = append(deferred, node)
				continue
			}
			r.launch(node, providerName)
			providerInFlight[providerName]++
		}
		queue = deferred

		if r.inFlight == 0 {
			if stopping {
				break
			}
			if len(queue) == 0 {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  fmt.Sprintf("executor: %d operation(s) remain but none are ready or running", w.Remaining()),
				})
				break
			}
		}

		res := <-r.results
		r.inFlight--
		if res.providerName != "" {
			providerInFlight[res.providerName]--
		}

		if res.err != nil {
			result.Failed[res.node.ID()] = res.err
			skipped = append(skipped, w.Skip(res.node.ID())...)
			continue
		}

		if err := r.record(res); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "failed to persist state after " + res.node.ID() + ": " + err.Error(),
				Detail: res.node.Address.String() + " was applied successfully and Result.State reflects it, " +
					"but the write to disk failed. The run is stopping rather than scheduling further " +
					"operations against a state file that can no longer be kept in sync with reality.",
				Related: []address.Address{res.node.Address},
			})
			persistFailed = true
		}
		appliedSet[res.node.Address.String()] = res.node.Address
		queue = append(queue, w.Done(res.node.ID())...)
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/executor/ -race -v`
Expected: PASS — 20 tests (18 from Tasks 5–7, 2 from this task).

Run: `make check`
Expected: green.

- [ ] **Step 5: Commit**

```bash
git add internal/executor/apply.go internal/executor/persist_test.go
git commit -m "$(cat <<'EOF'
feat: persist state after every operation, not batched at the end

run.record now calls Backend.Put immediately after mutating st, still on
the single owner goroutine — so successive writes are naturally
serialized without an extra lock, and Serial (which Put itself
increments) advances by exactly one per completed operation.
TestApplyPersistsStateAfterEveryOperationNotJustAtTheEnd polls the on-disk
file mid-run and requires observing a partial write, which a
batched-at-the-end implementation cannot produce. A Put failure stops the
run from scheduling further work but never rolls back the in-memory
mutation, which is already true.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

### Task 10: Failure isolation

**Files:**
- Create: `internal/executor/isolation.go`
- Modify: `internal/executor/apply.go` (wire the tracker into `run.record`; created by Task 8, also modified by Task 9 — do those first)
- Test: `internal/executor/isolation_test.go`

**Interfaces:**
- Consumes: `graph.Walk[planner.OpNode]` — `Ready() []planner.OpNode`,
  `Done(id string) []planner.OpNode`, `Skip(id string) []string`,
  `Remaining() int` (Group A, task 2); `planner.OpNode.ID() string` and its
  documented `"<verb>:<address>"` shape (existing, HEAD `a374a89`);
  `executor.Result{Applied, Failed, Skipped, State}` (Group A, task 3);
  `diag.Diagnostics` / `diag.Diagnostic{Severity, Summary, Detail, Related}`
  (existing); `address.Sort` (existing). Integrates into the body of
  `executor.Apply` (Group B, tasks 7–8), whose exact internal shape this
  contract does not name. Task 8 defines `run.record`; wire into it there.
- Produces: unexported `tracker` — `newTracker() *tracker`,
  `(*tracker).recordSuccess(w, node) []planner.OpNode`,
  `(*tracker).recordFailure(w, node, err, *diag.Diagnostics)`,
  `(*tracker).result(*state.State) Result`. Consumed only from inside
  `executor.Apply`'s own coordinator loop, in the same package — no other
  task calls these directly.

`Walk.Skip` already computes the transitive closure ("returns ids
transitively skipped, sorted" — contract), so the question of whether a
skipped operation's own dependent is *also* skipped is answered upstream, by
Group A's task 2, not here: this task's job is narrower — call `Skip` exactly
once per failure, on the node that actually failed, and trust its return
value completely rather than re-deriving reachability by hand. That matters
because a re-derivation is the second implementation of a rule this codebase
has already been burned by duplicating once (`value.Format`'s own history).
The reason transitivity is *correct*, not just contractual, is that a
dependent-of-a-dependent's inputs reference a resource that will now never
exist — not a stale value, an absent one — so running it is not "using
slightly old data," it is calling a provider with a reference to nothing.

A failure in one branch must not stop new work starting in an unrelated
branch: `recordFailure` only ever touches the failed node and whatever `Skip`
names as downstream of it. It does not cancel a context, does not set a
run-wide flag, and does not prevent the coordinator from calling
`Walk.Ready()` again — that call still returns other roots or newly-completed
branches exactly as it would with no failures at all. If Group B's loop
currently `return`s or `break`s out of its dispatch loop on the first
non-nil error (a very natural first draft), that behavior must be removed as
part of wiring this task in; the loop's only correct termination condition is
`w.Remaining() == 0`.

The process exit code deserves stating precisely even though this task does
not compute it: exit code 1 is "error", 2 is "success with changes" (spec
§16). A `Result` with any non-empty `Failed` must make the CLI (task 12,
Group D — not built yet) return `ExitError`, never `ExitChanges`, even though
other operations in the same run genuinely applied and state was correctly,
durably updated for them. A partial failure is still a failure: the user
asked to reconcile the whole plan, and part of it did not happen. Reporting
`ExitChanges` would tell a CI pipeline "this succeeded, changes were made,"
which is the same lie a caller would tell if it silently dropped `Failed`
into `Applied`. This is precisely why `Result` keeps `Applied`, `Failed` and
`Skipped` as three distinct fields instead of collapsing them into one
"things that happened" list — separation is what lets task 12 make this call
correctly later.

- [ ] **Step 1: Write the failing test**

```go
package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"infra/internal/diag"
	"infra/internal/graph"
	"infra/internal/planner"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

func opnode(name string) planner.OpNode {
	return planner.OpNode{Address: address.Address{Name: name}, Kind: planner.OpCreate, Phase: planner.PhaseCreate}
}

func idsOf(nodes []planner.OpNode) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID()
	}
	return ids
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestTrackerSkipsTransitivelyAndLeavesUnrelatedBranchesAlone is a pure, fast
// unit test of the tracker alone — no provider, no coordinator loop. The
// chain a -> b -> c is three nodes, not two: a two-node fixture cannot tell
// "b was skipped because Skip is transitive" apart from "b was skipped
// because it is a's one direct dependent" — c is what forces the
// implementation to actually use Skip's transitive return rather than only
// handling the immediate dependent. d shares no edge with anything, and
// proves a failure in one branch does not stop an unrelated one.
//
// A naive non-transitive implementation fails this test concretely: b would
// be Skipped but c would be neither Applied, Failed nor Skipped, so
// Remaining() would never reach zero — caught below even if the Skipped
// slice assertion were somehow satisfied by accident.
func TestTrackerSkipsTransitivelyAndLeavesUnrelatedBranchesAlone(t *testing.T) {
	g := graph.New[planner.OpNode]()
	a, b, c, d := opnode("a"), opnode("b"), opnode("c"), opnode("d")
	for _, n := range []planner.OpNode{a, b, c, d} {
		g.Add(n)
	}
	g.Edge(a.ID(), b.ID())
	g.Edge(b.ID(), c.ID())
	// d has no edge to anything: an unrelated branch.

	w, err := g.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	ready := w.Ready()
	if len(ready) != 2 || ready[0].ID() != a.ID() || ready[1].ID() != d.ID() {
		t.Fatalf("Ready() = %v, want [%s %s]", idsOf(ready), a.ID(), d.ID())
	}

	tr := newTracker()
	var ds diag.Diagnostics

	tr.recordFailure(w, a, errors.New("boom"), &ds)
	tr.recordSuccess(w, d)

	res := tr.result(nil)

	if got := res.Applied; len(got) != 1 || got[0].String() != d.Address.String() {
		t.Fatalf("Applied = %v, want [d]", got)
	}
	if _, ok := res.Failed[a.ID()]; !ok || len(res.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly {%s: <err>}", res.Failed, a.ID())
	}
	if want := []string{b.ID(), c.ID()}; !equalStrings(res.Skipped, want) {
		t.Fatalf("Skipped = %v, want %v", res.Skipped, want)
	}
	if w.Remaining() != 0 {
		t.Fatalf("Remaining() = %d, want 0 — something is stuck neither applied, failed nor skipped", w.Remaining())
	}
}

// TestApplyIsolatesAFailureEndToEnd proves the isolation above is actually
// wired into Apply's real coordinator loop (Group B, tasks 7–8), not merely
// that tracker is correct sitting on its own. a's create is configured to
// fail; b depends on a; c is unrelated.
func TestApplyIsolatesAFailureEndToEnd(t *testing.T) {
	cloudPath := t.TempDir() + "/fake-cloud.json"
	cloud := &testprovider.Cloud{
		Resources: map[string]*testprovider.CloudResource{},
		Failures: []testprovider.FailureRule{
			{Op: "create", Address: "a", Nth: 1},
		},
	}
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("seeding fake cloud: %v", err)
	}

	reg := registry.New()
	if err := reg.Register(testprovider.New(cloudPath)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	p := &planner.Plan{
		Version: planner.PlanVersion,
		Operations: []planner.Operation{
			{Address: address.Address{Name: "a"}, Type: "test.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/24", value.SourceExplicit)}},
			{Address: address.Address{Name: "b"}, Type: "test.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.1.0/24", value.SourceExplicit)}},
			{Address: address.Address{Name: "c"}, Type: "test.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.2.0/24", value.SourceExplicit)}},
		},
	}
	deps := func(a address.Address) []address.Address {
		if a.Name == "a" {
			return []address.Address{{Name: "b"}}
		}
		return nil
	}
	g, err := planner.BuildExecution(p, deps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	backend := state.NewLocal(t.TempDir())
	st, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	opts := Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend,
		Environment: "dev", Retry: RetryPolicy{MaxAttempts: 1}, Now: time.Now,
	}

	type outcome struct {
		res Result
		ds  diag.Diagnostics
	}
	done := make(chan outcome, 1)
	go func() {
		res, ds := Apply(context.Background(), p, g, st, opts)
		done <- outcome{res, ds}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Apply did not return within 10s — a failed but b was never marked done, failed or skipped, so the coordinator loop is stuck waiting on it")
	}

	if len(got.res.Applied) != 1 || got.res.Applied[0].String() != "c" {
		t.Fatalf("Applied = %v, want [c]", got.res.Applied)
	}
	if _, ok := got.res.Failed["create:a"]; !ok || len(got.res.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly {create:a: <err>}", got.res.Failed)
	}
	if want := []string{"create:b"}; !equalStrings(got.res.Skipped, want) {
		t.Fatalf("Skipped = %v, want %v", got.res.Skipped, want)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/executor/ -run TestTrackerSkipsTransitivelyAndLeavesUnrelatedBranchesAlone -v`
Expected: FAIL to build — `undefined: newTracker` (the type does not exist yet).

Run: `go test ./internal/executor/ -run TestApplyIsolatesAFailureEndToEnd -v`
Expected: FAIL — the exact shape depends on how task 7/8 left the coordinator
loop, so treat either of these as the expected red, not just one: **(a)** the
10-second timeout fires with "Apply did not return within 10s" because the
loop aborted on the first error without ever calling anything to advance `b`
past "waiting on a", so `Remaining()` never reaches zero and the loop
deadlocks; or **(b)** `Apply` returns quickly but `got.res.Applied` is empty
and `got.res.Failed` has one entry — a first draft that stops the whole run
on any error, rather than isolating the failed branch.

- [ ] **Step 3: Implement**

```go
package executor

import (
	"fmt"
	"sort"

	"infra/internal/diag"
	"infra/internal/graph"
	"infra/internal/planner"
	"infra/internal/state"
	"infra/pkg/address"
)

// tracker accumulates what happened to every operation the coordinating
// goroutine has processed, in a form Apply's loop can update one event at a
// time. It is not safe for concurrent use, which is fine: Walk itself is
// "not safe for concurrent use; the executor owns it from one goroutine and
// hands work out" (contract), and tracker is only ever touched from that
// same goroutine, never from a worker.
type tracker struct {
	applied []address.Address
	failed  map[string]error
	skipped map[string]bool
}

func newTracker() *tracker {
	return &tracker{failed: map[string]error{}, skipped: map[string]bool{}}
}

// recordSuccess records that node completed and advances the walk exactly
// as a direct call to Walk.Done would — call this INSTEAD of Done, never
// alongside it, or a completed node gets marked done twice.
func (t *tracker) recordSuccess(w *graph.Walk[planner.OpNode], node planner.OpNode) []planner.OpNode {
	t.applied = append(t.applied, node.Address)
	return w.Done(node.ID())
}

// recordFailure is the failure path spec §15 describes: "a failure stops
// its branch, not the world." It marks node itself failed, asks Walk which
// dependents that transitively strands, and marks each of those skipped —
// emitting one diagnostic per failure and one per skip, which is how the
// CLI (§16: diagnostics on stderr) reports "what failed and why" and "what
// was skipped and because of what" without this package needing a second,
// duplicate channel for that text: diag.Diagnostics is already the one
// place explanations like this live.
//
// The `id == node.ID()` guard exists because the contract's own wording —
// "marks failed/skipped; returns ids transitively skipped" — does not say
// outright whether Skip's returned slice excludes the id passed in. node
// belongs in Failed, never in Skipped (Result.Skipped is documented as
// "OpNode.IDs never attempted", and a failed node WAS attempted), so this
// filters defensively rather than assuming either reading of the contract.
func (t *tracker) recordFailure(w *graph.Walk[planner.OpNode], node planner.OpNode, err error, ds *diag.Diagnostics) {
	t.failed[node.ID()] = err
	ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  fmt.Sprintf("%s %s failed", node.Kind, node.Address),
		Detail:   err.Error(),
		Related:  []address.Address{node.Address},
	})

	for _, id := range w.Skip(node.ID()) {
		if id == node.ID() || t.skipped[id] {
			continue
		}
		t.skipped[id] = true
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityWarning,
			Summary:  fmt.Sprintf("%s skipped", id),
			Detail:   fmt.Sprintf("skipped because %s %s failed", node.Kind, node.Address),
		})
	}
}

// result assembles the Result the package contract promises. Sorting here,
// not as operations complete, is what keeps output independent of
// completion order under concurrency (spec §15: "determinism").
func (t *tracker) result(st *state.State) Result {
	applied := append([]address.Address(nil), t.applied...)
	address.Sort(applied)

	skipped := make([]string, 0, len(t.skipped))
	for id := range t.skipped {
		skipped = append(skipped, id)
	}
	sort.Strings(skipped)

	return Result{
		Applied: applied,
		Failed:  t.failed,
		Skipped: skipped,
		State:   st,
	}
}
```

**Wiring into Apply's coordinator loop.** This is the part the contract
cannot name for you — locate it by reading, not by
grepping for a fixed name. `executor.Apply` (task 7/8) owns a single
goroutine that calls `g.Walk()` once and then loops calling `w.Ready()`,
dispatching each returned node (bounded by `opts.Parallelism` and a
per-provider semaphore), and reacting to each dispatched operation's outcome.
In that reaction point:

- On success, replace whatever currently happens with `newly :=
  tracker.recordSuccess(w, node)` and feed `newly` into however the loop
  decides what to dispatch next (the same thing a direct `w.Done(node.ID())`
  call would have fed it — `recordSuccess` returns the identical value).
- On failure, replace whatever currently happens — most importantly, remove
  any `return`/`break` that stops the loop — with `tracker.recordFailure(w,
  node, err, &ds)`, where `ds` is the `diag.Diagnostics` the loop is already
  accumulating to return as `Apply`'s second value.
- The loop's only termination condition is `w.Remaining() == 0`.
- Where `Apply` currently builds its return value, replace it with
  `tracker.result(finalState)`, where `finalState` is the same `*state.State`
  task 8's persistence step has been writing to `opts.Backend` throughout the
  run.

Create `newTracker()` once, at the top of `Apply`, before the loop starts.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/executor/ -v`
Expected: PASS — 2 tests (plus whatever task 5–8 already added).

- [ ] **Step 5: Commit**

```bash
git add internal/executor/isolation.go internal/executor/isolation_test.go
git commit -m "$(cat <<'EOF'
executor: isolate failures to their own branch

A failed operation's dependents are skipped via Walk.Skip's transitive
closure; independent branches keep running; Result separates applied,
failed and skipped so a caller can tell a partial failure from either
extreme. Wires into Apply's coordinator loop from tasks 7-8.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

### Task 11: Signals

**Files:**
- Create: `internal/cli/interrupt.go`
- Create: `internal/executor/context.go`
- Modify: `internal/executor/dispatch.go` (provider calls must run under `operationContext(ctx)`, not `ctx`; created by Task 6)
- Test: `internal/cli/interrupt_test.go`
- Test: `internal/executor/context_test.go`

**Interfaces:**
- Consumes: the `context.Context` argument of `executor.Apply` (contract);
  `cli.ExitError` (existing, `root.go`); `state.Local.Lock`/`Unlock`'s
  existing conflict-reporting (existing, `lock.go` — read only, not
  modified); whatever function task 5 uses to call
  `pkg/provider.Provider.Create/Read/Update/Delete` (unnamed by the contract
  — CONTRACT GAP, located by grep as described below).
- Produces: `internal/cli.runInterruptible(environment string, fn
  func(context.Context) error) error` and the sentinel `cli.errInterrupted`
  — Group D's tasks 12–14 must wrap their call to `executor.Apply` /
  `executor.Destroy` / `executor.Refresh` in this, or the SIGINT behavior
  silently does not exist for that command.
  `internal/executor.operationContext(ctx context.Context) context.Context`
  — task 5's dispatch call sites must use this instead of `ctx` directly.

**Where the handler lives, and why.** `os/signal.Notify` is process-global
registration: correct for the one binary entrypoint, wrong for a package
other tools might import as a library — and `internal/executor` must stay
importable that way, since nothing in its layering rules ("Layering,"
contract) says otherwise. So the actual `signal.Notify` call belongs in
`internal/cli`, not `internal/executor`. It is a standalone function,
`runInterruptible`, rather than inlined into `newApplyCommand`'s `RunE`
(task 12, not yet written) because `destroy` and `refresh` (tasks 13–14) take
the lock for their whole run exactly as `apply` does (spec §9.2) and need
identical behavior; three copies of this would be three chances to diverge.

**"Cancels the context" is not "aborts the operation."** Canceling `ctx`
must only stop the coordinator from *starting* new work — it must not, by
itself, reach into a provider call already in flight. If it did, a Create
that has physically already reached the provider could be told to stop
believing its own result, which is exactly the ambiguous-outcome problem
`RetryPolicy`'s "create is never retried on an ambiguous failure" rule (task
4) exists to keep out of this system, arriving by a different door. The fix
is `context.WithoutCancel`: every dispatched operation's own provider call
runs under a context derived from `ctx` that keeps its values but cannot be
canceled by it, so the *only* place the coordinator's cancellation is
observable is the top of its own loop, in the choice not to call
`Walk.Ready()` again. This is genuinely different from ordinary context
propagation, and it is a deliberate departure from it, not an oversight: a
SIGINT during a retry backoff sleep is *not* protected the same way,
because nothing is in flight during a sleep — the previous provider call
already returned — so `RetryPolicy.Sleep` (task 4) should keep receiving
`ctx` directly, letting a SIGINT cut a backoff wait short and surface that
attempt's classified error into this task's own failure-isolation path
(task 9) like any other failure.

**Why a second SIGINT deliberately abandons the lock.** The lock is held by
`Apply`'s own goroutine for the whole run (`Options.Backend`,
`Options.Environment` — spec §9.2), and after the first SIGINT that goroutine
is still running: persisting final state and calling `Unlock`. The only
thing that could safely release the lock at that point is that goroutine
finishing on its own — anything else risks unlocking while a write to the
state file is still in progress, which would either corrupt the very state
the first SIGINT was trying to preserve or release a lock while a write is
still pending under it, reopening the concurrent-mutation hole invariant 5
exists to close. A second SIGINT means the user does not want to wait for
that any longer, so `runInterruptible` stops waiting — `os.Exit` bypasses
every deferred cleanup, including `Apply`'s own `Unlock` — and the lock is
left exactly where it is. That is not a shortcut around tidying up; it is
the only honest description of what happened: the lock's true state is
unknown, and spec §9.2 already decided that locks must never guess ("a
timeout that guesses wrong is exactly how two applies end up running at
once"). Calling `ForceUnlock` here on the CLI's behalf would be exactly that
guess.

**How the user learns the lock is stale.** Two channels, one already built
and one new. Reading `internal/state/lock.go` (`Local.Lock`) confirms the
existing one: the very next command that touches this environment gets `Lock`'s conflict error, which already
names the holder, pid, host, operation and timestamp and says `release it
with 'infra state unlock %s'` — no new code needed there. The new channel is
for the person still sitting at the terminal that just got abandoned, who
has no "next command" yet: `runInterruptible` prints the same instruction to
stderr immediately, before `os.Exit`.

- [ ] **Step 1: Write the failing test**

```go
// internal/cli/interrupt_test.go
package cli

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestRunInterruptibleFirstSignal delivers a real SIGINT to this test's own
// process (a standard, working pattern on Linux: signal.Notify installs the
// handler before the signal is sent, so the runtime delivers it in-process
// rather than terminating). It would catch two different real bugs: if
// runInterruptible never calls cancel() on the first signal, fn blocks on
// ctx.Done() forever and this test times out; if runInterruptible returns
// as soon as it receives the signal instead of waiting for fn to actually
// finish its post-cancellation cleanup, cleanedUp is not yet closed when
// the assertion below runs.
func TestRunInterruptibleFirstSignal(t *testing.T) {
	started := make(chan struct{})
	cleanedUp := make(chan struct{})
	resultErr := make(chan error, 1)

	go func() {
		resultErr <- runInterruptible("dev", func(ctx context.Context) error {
			close(started)
			<-ctx.Done() // unblocks only once the signal cancels ctx
			time.Sleep(20 * time.Millisecond) // stand-in for persist+unlock
			close(cleanedUp)
			return nil
		})
	}()

	<-started
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("finding own process: %v", err)
	}
	if err := self.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signaling self: %v", err)
	}

	select {
	case err := <-resultErr:
		select {
		case <-cleanedUp:
		default:
			t.Fatal("runInterruptible returned before fn's post-cancellation cleanup finished")
		}
		if !errors.Is(err, errInterrupted) {
			t.Fatalf("err = %v, want errInterrupted", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runInterruptible did not return after a single SIGINT")
	}
}

func TestRunInterruptibleNoSignalReturnsFnsError(t *testing.T) {
	want := errors.New("boom")
	err := runInterruptible("dev", func(ctx context.Context) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want to wrap %v", err, want)
	}
}
```

```go
// internal/executor/context_test.go
package executor

import (
	"context"
	"testing"
	"time"

	"infra/internal/diag"
	"infra/internal/planner"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

// TestApplySurvivesCancellationDuringDispatch is the proof that dispatch's
// provider calls run under operationContext, not Apply's own ctx. The fake
// provider's own delay is ctx-aware (providers/test/provider.go selects on
// ctx.Done()), so this exercises the exact failure mode operationContext
// exists to prevent: cancel while a create is genuinely in flight
// (LatencyMS holds it there for 400ms) and confirm it still finishes,
// rather than returning early with a context.Canceled-derived error. The
// cancellation fires at 100ms, well inside the 400ms window — canceling
// only after the create would already have finished could not tell "it
// survived cancellation" apart from "it was never affected".
func TestApplySurvivesCancellationDuringDispatch(t *testing.T) {
	cloudPath := t.TempDir() + "/fake-cloud.json"
	cloud := &testprovider.Cloud{
		Resources: map[string]*testprovider.CloudResource{},
		LatencyMS: 400,
	}
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("seeding fake cloud: %v", err)
	}

	reg := registry.New()
	if err := reg.Register(testprovider.New(cloudPath)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	p := &planner.Plan{
		Version: planner.PlanVersion,
		Operations: []planner.Operation{
			{Address: address.Address{Name: "network"}, Type: "test.network", Kind: planner.OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/24", value.SourceExplicit)}},
		},
	}
	g, err := planner.BuildExecution(p, func(address.Address) []address.Address { return nil })
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}

	backend := state.NewLocal(t.TempDir())
	st, err := backend.Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	opts := Options{
		Parallelism: 1, PerProvider: 1, Registry: reg, Backend: backend,
		Environment: "dev", Retry: RetryPolicy{MaxAttempts: 1}, Now: time.Now,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond) // well inside the 400ms delay
		cancel()
	}()

	type outcome struct {
		res Result
		ds  diag.Diagnostics
	}
	done := make(chan outcome, 1)
	go func() {
		res, ds := Apply(ctx, p, g, st, opts)
		done <- outcome{res, ds}
	}()

	select {
	case got := <-done:
		if len(got.res.Applied) != 1 || got.res.Applied[0].String() != "network" {
			t.Fatalf("Applied = %v, Failed = %v — the create did not survive cancellation; "+
				"dispatch is passing ctx straight to the provider instead of operationContext(ctx)",
				got.res.Applied, got.res.Failed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Apply did not return within 3s of a single operation with 400ms latency")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/cli/ -run TestRunInterruptible -v`
Expected: FAIL to build — `undefined: runInterruptible` (new symbol, not
written yet).

Run: `go test ./internal/executor/ -run TestApplySurvivesCancellationDuringDispatch -v`
Expected: FAIL — `Applied = [], Failed = map[create:network:context canceled]`
(or the process's exact wrapping of it), because dispatch (task 5) currently
hands the provider call `ctx` directly, and the fake provider's `delay`
returns `ctx.Err()` as soon as `cancel()` fires at 100ms — well before the
400ms latency would otherwise have let the create finish.

- [ ] **Step 3: Implement**

```go
// internal/cli/interrupt.go
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
)

// errInterrupted signals that a run stopped because of a SIGINT rather than
// because it failed or completed on its own. Execute's existing generic
// error path (root.go) already prints any non-nil error and returns
// ExitError for anything that is not errChanges, so errInterrupted needs no
// special case there — only errors.Is-based tests, and a human reading the
// message, need to tell an interruption apart from an ordinary failure.
var errInterrupted = errors.New("interrupted by SIGINT: the in-flight operation finished, state was saved, and the lock was released")

// runInterruptible runs fn under a context canceled by the first SIGINT,
// and does not return until fn itself returns. A second SIGINT — received
// while fn is still winding down from the first — exits the process
// immediately instead of waiting any longer.
//
// It lives here, not in internal/executor, because os/signal registration
// is process-global and belongs to the one binary entrypoint, not to a
// package other tools may import as a library (see this task's own
// write-up for the fuller argument). apply, destroy and refresh (spec
// §9.2: all three take the lock for their whole run) each call this rather
// than reimplementing it.
func runInterruptible(environment string, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	select {
	case err := <-done:
		// fn finished on its own; no interruption happened.
		return err

	case <-sig:
		// First SIGINT: ask fn to wind down, then WAIT for it. Returning
		// early here would let this function's caller — and eventually the
		// process — move on while fn is still mid-write to a state file it
		// holds locked.
		cancel()
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("%w (%v)", errInterrupted, err)
			}
			return errInterrupted

		case <-sig:
			// Second SIGINT, received before fn finished winding down from
			// the first. The user has now asked twice not to wait, so this
			// stops waiting: fn's goroutine, and the lock it still holds
			// (spec §9.2), are abandoned here deliberately — Local.Lock
			// already reports exactly this the next time anyone touches
			// this environment; this message is for the person still at
			// this terminal, who has no "next command" yet to tell them.
			fmt.Fprintf(os.Stderr,
				"infra: second interrupt received, exiting without waiting; "+
					"the lock on environment %q was not released and must be cleared with `infra state unlock %s`\n",
				environment, environment)
			os.Exit(ExitError)
			return nil // unreachable; os.Exit does not return
		}
	}
}
```

```go
// internal/executor/context.go
package executor

import "context"

// operationContext returns the context a single dispatched operation's
// provider calls must run with — never Apply's own ctx directly.
//
// Spec §15: a SIGINT "finishes the in-flight operation" rather than
// aborting it. If dispatch handed a provider call Apply's own ctx, a
// well-behaved provider — including the fake provider, whose delay honors
// ctx exactly as a real SDK client would — could see that ctx canceled
// mid-call and abort a Create that is physically already underway. That is
// the same ambiguous-outcome risk RetryPolicy's "create is never retried on
// an ambiguous failure" rule (task 4) already exists to keep out of this
// system, arriving through cancellation instead of a retry.
//
// context.WithoutCancel keeps any request-scoped values ctx carries while
// detaching cancellation, so a dispatched operation's own provider call
// cannot observe the coordinator's decision to stop. That decision stays
// visible only where it belongs: at the top of Apply's loop, in whether it
// calls Walk.Ready() again — never inside a call already handed to a
// provider.
//
// This does NOT apply to RetryPolicy.Sleep (task 4): nothing is in flight
// during a backoff sleep between attempts (the previous provider call
// already returned), so Sleep should keep receiving ctx directly — a
// SIGINT cutting a backoff short and surfacing that attempt's classified
// error is exactly the ordinary failure path task 9 already handles.
func operationContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
```

**Wiring `operationContext` into dispatch.** Locate task 5's call sites from
the repo root:

```bash
grep -rn '\.Create(ctx\|\.Read(ctx\|\.Update(ctx\|\.Delete(ctx' internal/executor
```

Every match passes `ctx` — Apply's own context — straight to a
`pkg/provider.Provider` method. Replace the `ctx` argument at each with
`operationContext(ctx)`, and add this comment immediately above the block:

```go
// operationContext detaches this call from Apply's own cancellation: a
// SIGINT must finish an in-flight operation, not abort it (spec §15; see
// executor/context.go).
```

Do not touch any call to `opts.Retry.Sleep` — see `operationContext`'s own
comment for why that one keeps `ctx` directly.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/ ./internal/executor/ -v`
Expected: PASS — both new tests, plus everything task 1–9 already added.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/interrupt.go internal/cli/interrupt_test.go \
        internal/executor/context.go internal/executor/context_test.go
git commit -m "$(cat <<'EOF'
cli, executor: finish the in-flight operation on SIGINT

runInterruptible (internal/cli) cancels a context on the first SIGINT and
waits for the caller to wind down before returning non-zero; a second
SIGINT exits immediately, deliberately leaving the environment lock as
stale. operationContext (internal/executor) detaches a dispatched
operation's own provider call from that cancellation, so "finishes the
in-flight operation" means what it says rather than aborting one already
under way.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

**Testing signals, plainly.** `runInterruptible`'s cancellation-on-first-
signal and wait-before-returning behavior is a genuine unit test — the self-
signal pattern above is real (Linux, this environment), not a smoke test: it
was run against a deliberately broken version with the `cancel()` call
removed and failed with exactly "runInterruptible did not return after a
single SIGINT" after the 2-second timeout, and passed once restored.
`operationContext`'s effect is proven the same way, one layer down, by
canceling a context programmatically and calling the real `executor.Apply`
directly — no OS signal needed for that half, because the behavior under
test is "cancellation must not reach an in-flight provider call," which is
independent of how the cancellation was triggered. What **cannot** be proven
here is the second-SIGINT path itself: it calls `os.Exit`, which would kill
the test binary rather than report a result, so it can only be exercised by
sending real signals to a real subprocess — and the only real subprocess
that holds a real environment lock while genuinely blocked in a create is
`infra apply`, which does not exist until task 12. Task 16's integration
suite (Group D, MVP round trip) should add: start `infra apply <env>
--auto-approve` against a project whose fake cloud has enough `latency_ms`
to stay in flight, send one SIGINT, confirm the process exits non-zero and
the resource still ends up in both state and the fake cloud (proof the
create was not aborted) and the lock file is gone; then repeat and send a
second SIGINT before the first would have finished winding down, confirming
the process exits promptly and the lock file is still present on disk
afterward. This document states that gap rather than writing a unit test
that would only prove `os.Exit` exits.

---

### Task 12: Summary rendering

**Files:**
- Create: `internal/executor/summary.go`
- Test: `internal/executor/summary_test.go`

**Interfaces:**
- Consumes: `executor.Result{Applied, Failed, Skipped, State}` (Group A,
  task 3); `state.State.Get(address.Address) (*resource.ResourceState,
  bool)` (existing); `value.Format`/`value.FormatOptions` (existing — THE
  redaction path); `address.Sort` (existing).
- Produces: `executor.Render(r Result, opts RenderOptions) string` and
  `executor.RenderOptions{Verbose, Color bool}` — for task 12 (Group D) to
  call after `executor.Apply` returns, the same way `internal/cli/plan.go`
  already calls `planner.Render(p, planner.RenderOptions{...})` after
  `planner.Compute`. This name is my own choice, not contract-named (see
  the authoring contract, which named no result renderer): Task 13 and Task 14
  both call it, and neither defines its own. An earlier draft of Task 13 did,
  and that duplicate was removed — two renderers for one Result is the defect
  that leaked a secret in M2.

`Render` takes only `Result`, not `*planner.Plan`. The plan the user
approved was already rendered in full, with every `+`/`~`/`-` and attribute
diff, before `apply` executed it (task 12's job); reprinting that here would
either duplicate it or drift from it, and `Result` alone is what the
contract actually hands this function. What it renders instead — the thing
a plan cannot show — is what the provider gave back for something that
actually ran: IDs, endpoints, anything that was `(known after apply)` in the
plan is a real value in `Result.State` now, and that is worth a person
seeing. Nor does `Render` explain *why* something failed or was skipped
beyond the one-line error or bare ID it has directly from `Result`: the
fuller explanation — which specific dependency's failure caused a given
skip — is a `diag.Diagnostic` that task 9's failure path already produces,
and `diag.Diagnostics.Render` already knows how to print it, on stderr,
separately from this function's stdout output (spec §16). Reprinting that
detail here would be the second copy of an explanation that task 9 already
owns.

Every value this function prints goes through `value.Format` — the same
function `internal/planner/render.go`'s `renderLeaf` calls, not a
reimplementation of what it does. That is the whole rule stated in the
contract ("there is exactly one redaction path... do not write a second
one") and in `value.Format`'s own comment (two prior leaks, both from a
second copy silently diverging from the first): this file gets to call the
one function, never to decide on its own when a value is safe to print bare.

- [ ] **Step 1: Write the failing test**

```go
package executor

import (
	"errors"
	"strings"
	"testing"

	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

func summaryRS(name string, attrs map[string]value.Value) *resource.ResourceState {
	return &resource.ResourceState{Address: address.Address{Name: name}, Attributes: attrs}
}

// TestRenderRedactsSensitiveAttributes compares the ENTIRE output exactly,
// not with strings.Contains: a contains-only check on "<sensitive>" cannot
// tell a correctly redacted output apart from one that ALSO leaked
// "hunter2" beside it. The negative check below is belt-and-suspenders on
// top of the exact comparison, not a substitute for it.
func TestRenderRedactsSensitiveAttributes(t *testing.T) {
	st := &state.State{}
	st.Set(summaryRS("db", map[string]value.Value{
		"engine":   value.String("postgres", value.SourceExplicit),
		"password": value.String("hunter2", value.SourceProvider).WithSensitive(true),
	}))
	r := Result{Applied: []address.Address{{Name: "db"}}, Failed: map[string]error{}, State: st}

	got := Render(r, RenderOptions{})
	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + db\n" +
		"      engine: \"postgres\"\n" +
		"      password: <sensitive>\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "hunter2") {
		t.Fatal("Render() leaked the raw secret")
	}
}

// TestRenderSortsFailedAndSkippedRegardlessOfInputOrder inserts both
// Failed and Skipped in an order that CONTRADICTS the expected sorted
// order (zulu before alpha, zebra/alpha-dep/mango not alphabetical), so a
// missing sort.Strings/address.Sort call fails this test rather than
// passing by coincidence.
func TestRenderSortsFailedAndSkippedRegardlessOfInputOrder(t *testing.T) {
	st := &state.State{}
	st.Set(summaryRS("network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	}))
	r := Result{
		Applied: []address.Address{{Name: "network"}},
		Failed: map[string]error{
			"update:zulu":  errors.New("connection refused"),
			"create:alpha": errors.New("quota exceeded"),
		},
		Skipped: []string{"update:zebra", "create:alpha-dep", "destroy:mango"},
		State:   st,
	}

	got := Render(r, RenderOptions{})
	want := "Apply complete: 1 applied, 2 failed, 3 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + network\n" +
		"      cidr: \"10.0.0.0/16\"\n" +
		"\n" +
		"Failed:\n" +
		"  x create alpha: quota exceeded\n" +
		"  x update zulu: connection refused\n" +
		"\n" +
		"Skipped (see the diagnostics for which dependency failed):\n" +
		"  - create alpha-dep\n" +
		"  - destroy mango\n" +
		"  - update zebra\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderEmptyResultShowsZeroCounts(t *testing.T) {
	got := Render(Result{Failed: map[string]error{}}, RenderOptions{})
	want := "Apply complete: 0 applied, 0 failed, 0 skipped.\n"
	if got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/executor/ -run TestRender -v`
Expected: FAIL to build — `undefined: Render` / `undefined: RenderOptions`
(neither exists yet).

- [ ] **Step 3: Implement**

```go
package executor

import (
	"fmt"
	"sort"
	"strings"

	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/value"
)

const (
	summaryAnsiReset   = "\x1b[0m"
	summaryAnsiGreen   = "\x1b[32m"
	summaryAnsiBoldRed = "\x1b[1;31m"
	summaryAnsiCyan    = "\x1b[36m"
)

// RenderOptions controls how a Result is rendered after an apply. It
// mirrors planner.RenderOptions in shape and meaning — Verbose reserved for
// future detail, Color wraps markers in ANSI — so the two renderers read as
// one family of output, not two unrelated ones.
type RenderOptions struct {
	Verbose bool
	Color   bool
}

// Render turns a Result into the text a person reads after an apply: what
// was applied — including the attributes the provider returned, the values
// a plan could only ever show as "(known after apply)" — what failed and
// why, and what was skipped. It never re-derives WHY something was
// skipped beyond the bare operation: that explanation is a diag.Diagnostic
// task 9 already produced, rendered separately on stderr (spec §16).
func Render(r Result, opts RenderOptions) string {
	var lines []string

	applied := append([]address.Address(nil), r.Applied...)
	address.Sort(applied)

	lines = append(lines, fmt.Sprintf("Apply complete: %d applied, %d failed, %d skipped.",
		len(applied), len(r.Failed), len(r.Skipped)))

	if len(applied) > 0 {
		lines = append(lines, "", "Applied:")
		for _, addr := range applied {
			lines = append(lines, renderAppliedLines(addr, r.State, opts)...)
		}
	}

	if len(r.Failed) > 0 {
		lines = append(lines, "", "Failed:")
		for _, id := range sortedFailedIDs(r.Failed) {
			verb, addr := splitOpID(id)
			lines = append(lines, fmt.Sprintf("  %s %s %s: %s", failedMarker(opts.Color), verb, addr, r.Failed[id]))
		}
	}

	if len(r.Skipped) > 0 {
		lines = append(lines, "", "Skipped (see the diagnostics for which dependency failed):")
		skipped := append([]string(nil), r.Skipped...)
		sort.Strings(skipped)
		for _, id := range skipped {
			verb, addr := splitOpID(id)
			lines = append(lines, fmt.Sprintf("  %s %s %s", skippedMarker(opts.Color), verb, addr))
		}
	}

	return strings.Join(lines, "\n") + "\n"
}

// renderAppliedLines renders one applied resource and the attributes the
// provider returned for it, redacting through value.Format exactly as
// planner.Render's renderLeaf does — the same, and only, redaction path
// (see value.Format's own comment for the two leaks that made that rule).
// A resource can be Applied with nothing in st when it was destroyed or
// forgotten: State.Get's comma-ok reports that plainly rather than this
// treating a missing entry as a bug.
func renderAppliedLines(addr address.Address, st *state.State, opts RenderOptions) []string {
	header := "  " + appliedMarker(opts.Color) + " " + addr.String()
	if st == nil {
		return []string{header}
	}
	rs, ok := st.Get(addr)
	if !ok || len(rs.Attributes) == 0 {
		return []string{header}
	}

	lines := []string{header}
	names := make([]string, 0, len(rs.Attributes))
	for name := range rs.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lines = append(lines, "      "+name+": "+renderValue(rs.Attributes[name]))
	}
	return lines
}

// renderValue is this package's one call site into value.Format, sibling to
// internal/planner/render.go's renderLeaf. Unknown differs deliberately
// from the plan renderer's "(known after apply)": after an apply every
// applied attribute should genuinely be known, so an unknown one here is an
// anomaly, not a promise about the future.
func renderValue(v value.Value) string {
	return value.Format(v, value.FormatOptions{Unknown: "(unknown)", QuoteStrings: true})
}

func sortedFailedIDs(failed map[string]error) []string {
	ids := make([]string, 0, len(failed))
	for id := range failed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// splitOpID recovers the verb and address from an OpNode.ID() string,
// documented as exactly "<verb>:<address>". SplitN on the first ":" is
// safe because an address never itself contains ":" (pkg/address's
// canonical form is dot-separated).
func splitOpID(id string) (verb, addr string) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return "operation", id
	}
	return parts[0], parts[1]
}

func appliedMarker(color bool) string {
	if !color {
		return "+"
	}
	return summaryAnsiGreen + "+" + summaryAnsiReset
}

func failedMarker(color bool) string {
	if !color {
		return "x"
	}
	return summaryAnsiBoldRed + "x" + summaryAnsiReset
}

func skippedMarker(color bool) string {
	if !color {
		return "-"
	}
	return summaryAnsiCyan + "-" + summaryAnsiReset
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/executor/ -v`
Expected: PASS — 3 new tests, plus everything task 1–10 already added.

- [ ] **Step 5: Commit**

```bash
git add internal/executor/summary.go internal/executor/summary_test.go
git commit -m "$(cat <<'EOF'
executor: render a post-apply summary

Applied resources show the attributes the provider actually returned,
redacted through the one value.Format path; failed and skipped operations
are listed with deterministic ordering. The "why" for a skip stays on
stderr as a diagnostic, which task 9 already produces, rather than a
second copy of that explanation here.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

### Task 13: `infra apply <environment>`

**Files:**
- Create: `internal/cli/apply.go`
- Create: `internal/cli/apply_test.go`
- Modify: `internal/cli/root.go`

**Interfaces:**
- Consumes: `compiler.Compile`, `config.Load`, `refresh.Refresh`, `planner.Compute`,
  `planner.BuildExecution`, `planner.Render`, `executor.Apply`, `executor.Options`,
  `executor.RetryPolicy`, `executor.Result`, `state.Local.Get/Put/Lock/Unlock`,
  `state.WithOperation`, `backendFor`, `buildRegistry`, `parseVars`, package-level
  `errChanges` (already defined in `plan.go`, same package).
- Produces (for task 13 to reuse, same package): `computePlan`, `confirm`,
  `dependentsOf`, `defaultRetryPolicy`, `releaseLock`. Signal handling comes
  from `runInterruptible` (Task 11) and result rendering from
  `executor.Render` (Task 12); this task defines neither.

Three design questions drive this task, and each has a trade-off worth stating rather
than a default falling out of the code.

**No changes ⇒ no lock, no prompt.** The lock exists to serialize *writers* (spec §9.2);
a run that has decided there is nothing to write has nothing to serialize against, and
taking the lock anyway would make a routine, already-reconciled `apply` collide with an
unrelated `refresh` or another no-op `apply` for no reason. So `apply` computes and shows
its plan completely unlocked — the same `backend.Get` → `refresh.Refresh` →
`planner.Compute` sequence `infra plan` uses, spec §10's promise that this sequence is
always safe to run repeatedly and against a locked environment. Only once the plan is
known to have changes, and only once a human (or `--auto-approve`) has approved them, does
`apply` call `backend.Lock`.

That ordering creates a second question this task has to answer: if planning and approval
both happen before the lock, what does `apply` actually execute — the plan a human just
approved, possibly several seconds stale, or something recomputed fresh? M3 has no
staleness-refusal machinery (that is explicitly M6: "reading a saved plan back... staleness
refusal... are M6", spec §12.2) — there is no `ConfigHash`/`StateHash` comparison to refuse
on a mismatch. Rather than execute the pre-lock plan blind, `apply` **recomputes the plan a
second time immediately after acquiring the lock**, and executes that one. This costs a
second refresh round-trip to the provider on every apply with changes, but it closes the
real hazard: without it, `apply` could execute against state or provider reality that
moved in the window between the unlocked preview and the lock being granted — precisely
the race invariant 5's lock exists to prevent, reopened one layer up by approving against a
stale read. The pre-lock plan and the post-lock plan will be identical the overwhelming
majority of the time (nothing else can be applying to this environment, by definition,
once the lock is held); when they are not, M3's answer is "execute what was actually just
observed", not "refuse" — full staleness refusal comparing the approved plan's fingerprint
against a fresh one is M6's job, not invented here. One accepted cosmetic cost of computing
the plan twice: if either pass produces warnings, they are rendered on stderr each time
they are computed, so a warning-producing apply may show that warning twice. Not a
correctness issue, and not worth a suppression flag for M3.

**Where the approval prompt reads from.** `cmd.InOrStdin()` defaults to `os.Stdin`, which
in a genuinely non-interactive context — a CI runner with no pty attached, or a test that
never calls `cmd.SetIn` — returns EOF immediately rather than blocking forever on a human
who is not there. `confirm` (below) treats `bufio.Scanner.Scan` returning `false` — EOF or
any other read error — as "not approved". This is exactly why `--auto-approve` exists: a
CI pipeline that forgets it gets an immediate, legible refusal ("apply cancelled: you must
type \"yes\" to approve") instead of a hang. The alternative — detecting whether stdin is a
real terminal and only then prompting — would need an `isatty` check, which needs a third
dependency the contract does not grant (`github.com/spf13/cobra` and `gopkg.in/yaml.v3`
only); failing closed on EOF gets the same practical outcome without one.

**What a lock conflict shows.** Nothing extra is needed: `state.Local.Lock`'s own error
(verified by reading `internal/state/lock.go`) already reports who holds it — user, host,
pid, the operation they are running (via `state.WithOperation`), and the exact `infra
state unlock <environment>` command to clear a stale one — wrapped around the sentinel
`state.ErrLocked`. `apply` returns that error unchanged; `cli.Execute` prints
`"Error: %v\n"` to stderr, so the holder information reaches the user with zero additional
code.

Exit codes (spec §16, reusing the package's existing `errChanges` sentinel and
`cli.Execute`'s existing `errors.Is(err, errChanges)` handling): **0** when the plan had no
changes; **2** (`errChanges`) when there were changes and every operation the executor
attempted succeeded; **1** for every other outcome — invalid configuration, a refresh or
plan-time diagnostic error, a lock conflict, a declined approval, or one or more failed
operations reported in `executor.Result.Failed`.

```go
// internal/cli/apply.go
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"infra/internal/compiler"
	"infra/internal/config"
	"infra/internal/executor"
	"infra/internal/planner"
	"infra/internal/refresh"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
)

// applyPrompt is what a human sees before infra mutates anything. "yes",
// typed in full — not a bare y — is the bar for an ordinary apply. destroy
// (task 13) sets a higher bar, typing the environment name, because destroy
// is unconditionally destructive by definition; most applies are pure
// creates with nothing to destroy at all, and attaching the heavier bar to
// every apply would desensitize users to it well before it mattered. Spec
// §13's stronger "typed confirmation... when the environment is
// type: production" is environment-level and explicitly M6 (no environment
// types exist yet in M3) — this is the ordinary-apply default that holds
// until that lands.
const applyPrompt = "\nDo you want to perform these actions?\n" +
	"  infra will perform the actions described above.\n" +
	"  Only 'yes' will be accepted to approve.\n\n" +
	"  Enter a value: "

// newApplyCommand builds `infra apply <environment>`: compile, refresh,
// plan, show it, take approval, execute. Spec §16, §9.2, §10.
func newApplyCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "apply <environment>",
		Short:         "Reconcile real infrastructure with the configured desired state",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			vars, err := parseVars(opts.Vars)
			if err != nil {
				return err
			}

			files, err := config.Load(opts.Dir)
			if err != nil {
				return err
			}

			reg := buildRegistry(opts.Dir)
			cfg, cds := compiler.Compile(files, reg, compiler.Options{
				Environment: environment,
				Vars:        vars,
			})
			if cds.HasErrors() {
				cds.Render(cmd.ErrOrStderr())
				return errors.New("configuration is not valid")
			}

			backend := backendFor(opts.Dir)

			// Unlocked preview — identical in spirit to `infra plan`: safe
			// to run against a locked environment, in CI, or repeatedly.
			p, _, err := computePlan(cmd.Context(), cmd, backend, reg, cfg, environment, opts)
			if err != nil {
				return err
			}

			fmt.Fprint(cmd.OutOrStdout(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose}))

			if !p.HasChanges() {
				return nil
			}

			if !opts.AutoApprove {
				if !confirm(cmd, applyPrompt, "yes") {
					return errors.New("apply cancelled: you must type \"yes\" to approve")
				}
			}

			// Everything from here runs under runInterruptible (Task 11),
			// which owns SIGINT: the first one cancels this context so no
			// NEW work is scheduled, while the operation already in flight
			// finishes against a context Task 8 derives with
			// context.WithoutCancel; the second exits immediately and
			// deliberately leaves the lock, which the next run reports as
			// stale. Signal handling lives in internal/cli and never in the
			// executor, so a library import cannot install a handler behind
			// a caller's back.
			return runInterruptible(environment, func(ctx context.Context) error {
				ctx = state.WithOperation(ctx, "apply")
				if _, err := backend.Lock(ctx, environment); err != nil {
					// Lock's own error already names the holder (spec §9.2) —
					// nothing to add.
					return err
				}
				defer releaseLock(backend, environment, cmd.ErrOrStderr())

				// Re-plan inside the lock — see this task's doc comment above
				// for why: apply must never execute against state or provider
				// reality gathered before the lock was held.
				p2, st, err := computePlan(ctx, cmd, backend, reg, cfg, environment, opts)
				if err != nil {
					return err
				}
				if !p2.HasChanges() {
					fmt.Fprintln(cmd.OutOrStdout(), "\nNo changes remained once the environment lock was acquired; nothing to apply.")
					return nil
				}

				g, err := planner.BuildExecution(p2, dependentsOf(p2))
				if err != nil {
					return fmt.Errorf("building execution graph: %w", err)
				}

				res, execDiags := executor.Apply(ctx, p2, g, st, executor.Options{
					Parallelism: opts.Parallelism,
					PerProvider: opts.Parallelism, // no dedicated flag yet; see the note below
					Registry:    reg,
					Backend:     backend,
					Environment: environment,
					Retry:       defaultRetryPolicy(),
					Now:         time.Now,
				})
				execDiags.Render(cmd.ErrOrStderr())

				// executor.Render (Task 12) is THE result renderer. An earlier
				// draft of this task wrote a private renderResult here; two
				// renderers for one Result is the duplicate-implementation
				// defect that put a secret in M2's output, so this calls the
				// shared one.
				fmt.Fprint(cmd.OutOrStdout(), executor.Render(res, executor.RenderOptions{Verbose: opts.Verbose}))

				if execDiags.HasErrors() || len(res.Failed) > 0 {
					return errors.New("apply completed with failures")
				}
				return errChanges
			})
		},
	}
}

// computePlan runs the same unlocked, side-effect-free sequence `infra
// plan` uses — backend.Get, refresh.Refresh, planner.Compute — and returns
// the resulting plan together with the state it was computed against.
// apply calls it twice (before and after taking the lock); destroy (task
// 13) reuses it unchanged with an empty desired configuration, which is
// what turns "everything currently in state" into a full teardown plan
// through the planner's own decision table rather than a second, bespoke
// "destroy everything" code path.
func computePlan(ctx context.Context, cmd *cobra.Command, backend *state.Local, reg *registry.Registry, cfg compiler.ResolvedConfig, environment string, opts *GlobalOptions) (*planner.Plan, *state.State, error) {
	st, err := backend.Get(ctx, environment)
	if err != nil {
		return nil, nil, err
	}

	obs, refreshDiags := refresh.Refresh(ctx, st, reg, opts.Parallelism)
	refreshDiags.Render(cmd.ErrOrStderr())
	if refreshDiags.HasErrors() {
		return nil, nil, errors.New("refreshing provider state failed")
	}

	p, planDiags := planner.Compute(cfg, st, obs, planner.Options{
		Environment: environment,
		Now:         time.Now,
		Registry:    reg,
	})
	planDiags.Render(cmd.ErrOrStderr())
	if planDiags.HasErrors() {
		return nil, nil, errors.New("planning failed")
	}
	return p, st, nil
}

// confirm prints prompt to stdout and reads exactly one line from stdin,
// reporting whether it equals want exactly (bufio.Scanner's default
// ScanLines split already strips the line terminator; nothing else is
// trimmed). A near miss — wrong case, "y" for "yes", trailing spaces — is
// refused, not generously accepted: a command about to mutate or destroy
// infrastructure should fail closed on ambiguous input.
//
// Scan returning false — EOF or any other read error — is treated as
// declined. See this task's doc comment for why that must never block.
func confirm(cmd *cobra.Command, prompt, want string) bool {
	fmt.Fprint(cmd.OutOrStdout(), prompt)
	scanner := bufio.NewScanner(cmd.InOrStdin())
	if !scanner.Scan() {
		return false
	}
	return scanner.Text() == want
}

// dependentsOf builds the `deps` function planner.BuildExecution requires.
// Reading internal/planner/execution.go: deps(addr) must report the
// addresses that depend ON addr, and BuildExecution's own doc comment says
// "Operation.Dependents already holds the resolved answer" — so this is a
// lookup into what the planner already decided, never a second resolution
// of dependency edges computed independently of it. apply and destroy
// share this helper for exactly that reason: two answers to "what depends
// on what" could disagree with each other.
func dependentsOf(p *planner.Plan) func(address.Address) []address.Address {
	byAddr := make(map[string]planner.Operation, len(p.Operations))
	for _, op := range p.Operations {
		byAddr[op.Address.String()] = op
	}
	return func(addr address.Address) []address.Address {
		return byAddr[addr.String()].Dependents
	}
}

// defaultRetryPolicy is the backoff apply and destroy hand the executor.
// Three attempts total, half a second base doubling toward a ten second
// ceiling, with full jitter (a uniform random duration between zero and the
// computed delay) so many operations retrying together do not all wake on
// the same tick and hammer the provider at once — the thundering-herd
// failure mode plain exponential backoff invites. Sleep is a real timer
// that still respects context cancellation, so a SIGINT during a retry
// wait does not have to run out the clock before the executor notices.
// Which operation kinds are ever retried at all, and under which
// classification, is the executor's own decision (spec §15, §35: Create
// never retried on an ambiguous failure, Delete only on SafeToRetry) — this
// policy only supplies the timing, identical for apply and destroy.
func defaultRetryPolicy() executor.RetryPolicy {
	return executor.RetryPolicy{
		MaxAttempts: 3,
		Base:        500 * time.Millisecond,
		Max:         10 * time.Second,
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		Jitter: func(d time.Duration) time.Duration {
			if d <= 0 {
				return 0
			}
			return time.Duration(rand.Int63n(int64(d)))
		},
	}
}

// NOTE: an earlier draft of this task defined installDoubleInterruptHandler
// here, because no seam for signal handling was contract-named at the time.
// Task 11 owns that seam and exposes runInterruptible; two signal handlers in
// one binary is not a style question, it is a correctness one, so this
// definition was removed and the RunE above calls Task 11's instead.


// releaseLock unlocks best-effort on the way out of apply/destroy/refresh.
// It uses a fresh background context, not the run's own: the run's context
// may already be cancelled (SIGINT) or its deadline passed, and releasing
// the lock is exactly the cleanup that must still happen when that is
// true. A failure here is reported, not fatal: the run's actual result was
// already decided, and demoting a working apply to "error" because the
// lock file could not be removed would hide a fine outcome behind a worse
// one.
func releaseLock(backend *state.Local, environment string, stderr io.Writer) {
	if err := backend.Unlock(context.Background(), environment); err != nil {
		fmt.Fprintf(stderr, "warning: failed to release the lock on %q: %v\n", environment, err)
	}
}

// NOTE: an earlier draft of this task defined renderResult here, because no
// shared summary renderer was contract-named at the time. Task 12 owns that
// and exposes executor.Render(Result, RenderOptions). Two renderers for one
// Result is the duplicate-implementation defect that leaked a secret in M2,
// so this definition was removed and the RunE above calls Task 12's instead.
//
// One thing the deleted version did that Task 12's does NOT: it counted a
// replacement once rather than twice, because a replace is two execution
// nodes at one address. If Task 12's Render double-counts a replace in its
// totals, that is a real defect — check it when Task 12 lands and report it
// rather than reintroducing a second renderer here.

```

- [ ] **Step 1: Write the failing tests**

```go
// internal/cli/apply_test.go
package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

// seedState writes st directly to the backend, taking and releasing the
// environment lock around the write — state.Local.Put refuses to write
// without a held lock (Group A, task 1), and this helper must keep working
// whether or not that change has landed yet in a given build: taking the
// lock first is a no-op cost either way.
func seedState(t *testing.T, dir, environment string, st *state.State) {
	t.Helper()
	backend := backendFor(dir)
	ctx := state.WithOperation(context.Background(), "test-seed")
	if _, err := backend.Lock(ctx, environment); err != nil {
		t.Fatalf("locking to seed state: %v", err)
	}
	defer func() {
		if err := backend.Unlock(ctx, environment); err != nil {
			t.Fatalf("unlocking after seeding state: %v", err)
		}
	}()
	if err := backend.Put(ctx, environment, st); err != nil {
		t.Fatalf("seeding state: %v", err)
	}
}

func TestApplyWithNoChangesTakesNoLockAndDoesNotPrompt(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	ctx := context.Background()
	prov := testprovider.New(filepath.Join(dir, testprovider.DefaultCloudPath))
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("")) // must never be read: no prompt should occur
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil (no changes is success)", err)
	}
	if !strings.Contains(stdout.String(), "No changes") {
		t.Errorf("stdout does not report a clean plan:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("apply with no changes must not take the environment lock")
	}
}

func TestApplyWithAutoApproveCreatesResourcesAndReportsChanges(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges — a fresh apply always has changes", err)
	}
	if !strings.Contains(stdout.String(), "Apply complete!") {
		t.Errorf("stdout does not report completion:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "1 created") {
		t.Errorf("stdout does not report the create:\n%s", stdout.String())
	}

	st, err := backendFor(dir).Get(context.Background(), "dev")
	if err != nil {
		t.Fatalf("reading state after apply: %v", err)
	}
	if _, ok := st.Get(address.Address{Name: "network"}); !ok {
		t.Error("network was not recorded in state after a successful apply")
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("apply must release the lock when it finishes")
	}
}

func TestApplyDeclinedApprovalAppliesNothing(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("no\n"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil || errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error — declining approval must not apply anything", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.json")); !os.IsNotExist(err) {
		t.Error("declining approval must not write state")
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("declining approval must not leave the environment locked — the lock is taken after approval, not before")
	}
}

func TestApplyLockConflictNamesTheHolder(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	backend := backendFor(dir)
	lockCtx := state.WithOperation(context.Background(), "some-other-run")
	if _, err := backend.Lock(lockCtx, "dev"); err != nil {
		t.Fatalf("pre-locking dev: %v", err)
	}
	defer backend.ForceUnlock("dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newApplyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error — the environment is already locked")
	}
	if !strings.Contains(err.Error(), "some-other-run") {
		t.Errorf("error does not name the holder's operation: %v", err)
	}
	if !strings.Contains(err.Error(), "pid") {
		t.Errorf("error does not name the holder's pid: %v", err)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/cli/ -run TestApply -v`
Expected: FAIL to build — `newApplyCommand` is undefined (`apply.go` does not exist yet).

- [ ] **Step 3: Implement**

Write `internal/cli/apply.go` exactly as above. Then modify `internal/cli/root.go`,
adding the command to the tree (the existing `TestEveryCommandSilencesUsageAndErrors`
walks `NewRootCommand()` and enforces `SilenceUsage`/`SilenceErrors` on every command with
a `RunE`, which `newApplyCommand`'s literal already sets — no separate test needed for
that):

```go
	root.AddCommand(newValidateCommand(opts))
	root.AddCommand(newStateCommand(opts))
	root.AddCommand(newPlanCommand(opts))
	root.AddCommand(newApplyCommand(opts))
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/ -v`
Expected: PASS — all `internal/cli` tests, including the four new ones above and the
existing `TestEveryCommandSilencesUsageAndErrors`.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/apply.go internal/cli/apply_test.go internal/cli/root.go
git commit -m "$(cat <<'EOF'
Add `infra apply`: unlocked plan/approve, then locked re-plan/execute

Planning and approval happen before the environment lock is taken, exactly
like `infra plan` — a no-op apply never locks or prompts. Once there are
approved changes, apply re-plans a second time inside the lock and executes
that plan, so execution never runs against state or provider reality read
before the lock was held. Approval reads "yes" from stdin and fails closed
on EOF, so a CI run that forgets --auto-approve gets an immediate refusal
instead of a hang.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

### Task 14: `infra destroy <environment>`

**Files:**
- Create: `internal/cli/destroy.go`
- Create: `internal/cli/destroy_test.go`
- Modify: `internal/cli/root.go`

**Interfaces:**
- Consumes: `computePlan`, `confirm`, `dependentsOf`, `defaultRetryPolicy`,
  `releaseLock`, `errChanges`, plus `runInterruptible` (Task 11) and
  `executor.Render` (Task 12) (all from
  `apply.go`, task 12, same package); `compiler.ResolvedConfig`, `planner.BuildExecution`,
  `planner.Render`, `executor.Apply`, `state.Local.Get/Lock/Unlock`,
  `state.WithOperation`.
- Produces: nothing further tasks depend on.

`destroy` needs a plan that proposes removing everything infra manages in an environment.
Rather than write a second "delete everything" code path, it hands `planner.Compute` an
**empty** `compiler.ResolvedConfig{Project: <from state>, Environment: environment}` —
one with no resources at all. Every resource recorded in state then falls into the
planner's own "in state, not in config" row (spec §11), which is already exactly Destroy,
or Forget under `retain`, or a plan-time refusal under `prevent_destroy`. `computePlan`
(task 12) doesn't care whether its `cfg` argument came from compiling YAML or was built by
hand — it is reused unchanged. This means `destroy` never calls `config.Load` or
`compiler.Compile` at all: it does not matter what the project's `infra.yml` currently
says, only what is recorded in state, so the project name comes from `state.State.Project`
itself. It also means `--var`/`--var-file` are simply inapplicable to `destroy` — there is
no configuration for a variable to interpolate into — which is not a broken promise the
way `--var-file` being registered-but-unwired was; the flag is globally registered, and
`destroy` just never reaches the code path that would read it.

`prevent_destroy` and `retain` need no special casing here beyond what `computePlan`
already does: a resource with `prevent_destroy` makes `planner.Compute` return a
plan-time diagnostic error, so `computePlan` renders it and returns an error before there
is anything to confirm or execute — `destroy` refuses exactly like `plan`/`apply` would,
never reaching the confirmation prompt. A resource with `retain` renders as Forget (symbol
`=`, not `-`) and the executor removes it from state without ever calling the provider's
`Delete` — spec §11's "removes the resource from state without calling the provider" —
which falls out of routing every operation through the same `planner.BuildExecution` /
`executor.Apply` pair `apply` uses; `destroy` does not distinguish Forget from Destroy
anywhere in its own code.

**Typed confirmation.** Spec §13 requires destructive operations to demand more than a
bare `y`. `destroy`, unlike `apply`, is unconditionally and entirely destructive by
definition — there is no partial-destroy version of this command — so it always requires
the confirmation, not only when some future environment-level `type: production` flag is
set (that flag does not exist in M3; environment-level protections are explicitly out of
scope, per this milestone's brief). The word chosen is **the exact environment name**, not
a fixed word like "yes". The costliest failure mode `destroy` invites is not "the user
didn't mean to destroy anything" (they typed the command) but "the user meant to destroy a
*different* environment than the one this shell happens to be pointed at" — a stale
`--chdir`, the wrong terminal tab, a copy-pasted command with the wrong argument. Typing a
fixed word defends only against the first; typing the environment name defends against
both, and is the same reasoning GitHub's "type the repository name to delete it" and
Terraform Cloud's destroy confirmation both use. `--auto-approve` skips this exactly as it
skips `apply`'s prompt — one flag, one meaning ("skip confirmation"), not two different
opt-out mechanisms a script author has to remember separately.

```go
// internal/cli/destroy.go
package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"infra/internal/compiler"
	"infra/internal/executor"
	"infra/internal/planner"
	"infra/internal/state"
)

// newDestroyCommand builds `infra destroy <environment>`: plan the removal
// of everything infra manages in the environment, confirm, execute. Spec
// §16, §9.2, §11, §13.
func newDestroyCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "destroy <environment>",
		Short:         "Destroy every resource infra manages in an environment",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			reg := buildRegistry(opts.Dir)
			backend := backendFor(opts.Dir)

			st0, err := backend.Get(cmd.Context(), environment)
			if err != nil {
				return err
			}
			// An empty desired configuration: every resource recorded in
			// state falls into planner.Compute's "in state, not in
			// config" row, which is already Destroy/Forget/prevent_destroy
			// (spec §11) — see this task's doc comment above.
			emptyCfg := compiler.ResolvedConfig{Project: st0.Project, Environment: environment}

			p, _, err := computePlan(cmd.Context(), cmd, backend, reg, emptyCfg, environment, opts)
			if err != nil {
				return err
			}

			fmt.Fprint(cmd.OutOrStdout(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose}))

			if !p.HasChanges() {
				return nil
			}

			if !opts.AutoApprove {
				prompt := fmt.Sprintf("\nDestroying environment %q will delete every resource infra "+
					"manages there. This cannot be undone.\nType the environment name to confirm: ", environment)
				if !confirm(cmd, prompt, environment) {
					return fmt.Errorf("destroy cancelled: you must type %q to confirm", environment)
				}
			}

			// Same seam as apply (Task 13): Task 11's runInterruptible owns
			// SIGINT, and Task 12's executor.Render owns the result summary.
			return runInterruptible(environment, func(ctx context.Context) error {
				ctx = state.WithOperation(ctx, "destroy")
				if _, err := backend.Lock(ctx, environment); err != nil {
					return err
				}
				defer releaseLock(backend, environment, cmd.ErrOrStderr())

				p2, st, err := computePlan(ctx, cmd, backend, reg, emptyCfg, environment, opts)
				if err != nil {
					return err
				}
				if !p2.HasChanges() {
					fmt.Fprintln(cmd.OutOrStdout(), "\nNothing remained to destroy once the environment lock was acquired.")
					return nil
				}

				g, err := planner.BuildExecution(p2, dependentsOf(p2))
				if err != nil {
					return fmt.Errorf("building execution graph: %w", err)
				}

				res, execDiags := executor.Apply(ctx, p2, g, st, executor.Options{
					Parallelism: opts.Parallelism,
					PerProvider: opts.Parallelism,
					Registry:    reg,
					Backend:     backend,
					Environment: environment,
					Retry:       defaultRetryPolicy(),
					Now:         time.Now,
				})
				execDiags.Render(cmd.ErrOrStderr())

				fmt.Fprint(cmd.OutOrStdout(), executor.Render(res, executor.RenderOptions{Verbose: opts.Verbose}))

				if execDiags.HasErrors() || len(res.Failed) > 0 {
					return errors.New("destroy completed with failures")
				}
				return errChanges
			})
		},
	}
}
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/cli/destroy_test.go
package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

func TestDestroyWithEmptyStateTakesNoLockAndDoesNotPrompt(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil — nothing to destroy is success", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("destroy with nothing to destroy must not take the environment lock")
	}
}

func TestDestroyRequiresTheEnvironmentNameNotABareYes(t *testing.T) {
	dir := seedOneNetwork(t, "dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("yes\n")) // a bare "yes" is not the environment name
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil || errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want a plain error — \"yes\" must not satisfy destroy's confirmation", err)
	}
	st, gerr := backendFor(dir).Get(context.Background(), "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := st.Get(address.Address{Name: "network"}); !ok {
		t.Error("network was destroyed despite confirmation being refused")
	}
}

func TestDestroyWithCorrectConfirmationRemovesEverything(t *testing.T) {
	dir := seedOneNetwork(t, "dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader("dev\n"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges — a destroy with something to remove has changes", err)
	}
	if !strings.Contains(stdout.String(), "Destroy complete!") {
		t.Errorf("stdout does not report completion:\n%s", stdout.String())
	}

	st, gerr := backendFor(dir).Get(context.Background(), "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := st.Get(address.Address{Name: "network"}); ok {
		t.Error("network is still recorded in state after a confirmed destroy")
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.lock")); !os.IsNotExist(err) {
		t.Error("destroy must release the lock when it finishes")
	}
}

func TestDestroyForgetsARetainedResourceWithoutDeletingIt(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
		Lifecycle: resource.Lifecycle{Retain: true},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, AutoApprove: true}
	cmd := newDestroyCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v, want errChanges", err)
	}
	// Forget's symbol is "=" (planner.OpKind.Symbol), not destroy's "-": a
	// plan that rendered a retained resource with "-" would be telling the
	// user it is about to call Delete on infrastructure retain exists to
	// protect.
	if !strings.Contains(stdout.String(), "= test.network.network") {
		t.Errorf("plan did not render the retained resource as forgotten:\n%s", stdout.String())
	}

	afterSt, gerr := backendFor(dir).Get(ctx, "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := afterSt.Get(address.Address{Name: "network"}); ok {
		t.Error("retained resource is still recorded in state after destroy")
	}

	// The whole point of retain: the real infrastructure must still exist.
	current, rerr := prov.Read(ctx, rs)
	if rerr != nil {
		t.Fatalf("reading back the fake cloud: %v", rerr)
	}
	if current == nil {
		t.Error("destroy called Delete on a retained resource — retain must remove it from state without touching the provider")
	}
}

// seedOneNetwork creates one test.network resource through the fake
// provider and records it in state for environment, returning the project
// directory. Shared by the confirmation tests above, which only care that
// something exists to destroy, not about its specific attributes.
func seedOneNetwork(t *testing.T, environment string) string {
	t.Helper()
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	prov := testprovider.New(filepath.Join(dir, testprovider.DefaultCloudPath))
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", environment)
	st.Set(rs)
	seedState(t, dir, environment, st)
	return dir
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/cli/ -run TestDestroy -v`
Expected: FAIL to build — `newDestroyCommand` is undefined (`destroy.go` does not exist
yet).

- [ ] **Step 3: Implement**

Write `internal/cli/destroy.go` exactly as above. Modify `internal/cli/root.go`:

```go
	root.AddCommand(newApplyCommand(opts))
	root.AddCommand(newDestroyCommand(opts))
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/ -v`
Expected: PASS — all `internal/cli` tests, including the four new ones above.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/destroy.go internal/cli/destroy_test.go internal/cli/root.go
git commit -m "$(cat <<'EOF'
Add `infra destroy`: plan a full teardown from state, confirm by name, execute

destroy plans against an empty desired configuration rather than a second
"delete everything" code path, so every recorded resource falls through the
planner's existing prevent_destroy/retain/destroy decision table unchanged.
Confirmation requires typing the exact environment name rather than a bare
yes — destroy is unconditionally destructive, and the costliest mistake is
targeting the wrong environment, not failing to mean it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

### Task 15: `infra refresh <environment>`

**Files:**
- Create: `internal/cli/refresh.go`
- Create: `internal/cli/refresh_test.go`
- Modify: `internal/cli/root.go`

**Interfaces:**
- Consumes: `refresh.Refresh`, `refresh.Observation`, `refresh.Observations`,
  `state.Local.Get/Put/Lock/Unlock`, `state.WithOperation`, `releaseLock` (from
  `apply.go`, task 12, same package), `backendFor`, `buildRegistry`.
- Produces: nothing further tasks depend on.

`refresh` is the odd one out among M3's three new commands: `apply` and `destroy` both
compute a throwaway preview before ever touching the lock, because both have a real
"nothing to do" outcome worth skipping the lock for. `refresh` does not — its entire job
*is* the write. There is nothing to preview and discard; every run reads state, reads
providers, and (when there is anything in state at all) writes state. So `refresh` takes
the lock first, unlike `apply`/`destroy`'s "plan then lock" split, and unlike them it never
returns `errChanges` — spec §16's exit code 2 is defined in terms of a *plan's* proposed
operations, and `refresh` produces no plan and proposes nothing. Overloading exit 2 to
mean "drift was observed" would need a definition the spec does not give; that is a
candidate for a future spec change, not something to invent silently here. `refresh` exits
0 on success (whether or not anything changed) and 1 if any resource could not be read.

The one decision this task exists to get right is spec §10's distinction, restated in the
brief as "the worst bug available in this milestone": `refresh.Observation.State == nil`
with `Err == nil` means the provider *affirmatively reports the resource gone* — that is
the only case that removes it from state. `Err != nil` means reality is **unknown** — a
transient failure, a deregistered resource type, a cancelled read — and state for that
resource must be left exactly as it was. Writing "gone" for "unknown" would make the next
`plan` propose recreating infrastructure that, for all `refresh` actually learned, is still
there. The two are handled in a three-way switch on the observation, not a boolean check,
specifically so a future edit cannot collapse `Err != nil` into the "absent" branch by
merging two `if`s.

```go
// internal/cli/refresh.go
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"infra/internal/refresh"
	"infra/internal/state"
)

// newRefreshCommand builds `infra refresh <environment>`: read every
// resource's current provider state and persist what was learned. The only
// M3 verb that writes what a read observed (spec §10) — `plan` and the
// preview half of `apply`/`destroy` use the exact same refresh.Refresh call
// and discard its result.
func newRefreshCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "refresh <environment>",
		Short:         "Reconcile recorded state with what providers actually report",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			reg := buildRegistry(opts.Dir)
			backend := backendFor(opts.Dir)

			// refresh's whole job is the write, unlike apply/destroy's
			// plan-then-lock split (see this task's doc comment above):
			// the lock is taken up front.
			ctx := state.WithOperation(cmd.Context(), "refresh")
			if _, err := backend.Lock(ctx, environment); err != nil {
				return err
			}
			defer releaseLock(backend, environment, cmd.ErrOrStderr())

			st, err := backend.Get(ctx, environment)
			if err != nil {
				return err
			}

			obs, ds := refresh.Refresh(ctx, st, reg, opts.Parallelism)
			ds.Render(cmd.ErrOrStderr())

			// st.Addresses() is sorted, so this — and therefore stdout — is
			// deterministic regardless of which read finished first.
			out := cmd.OutOrStdout()
			wrote := false
			for _, addr := range st.Addresses() {
				o := obs[addr.String()]
				switch {
				case o.Err != nil:
					// Reality is UNKNOWN. State is left exactly as it was —
					// never removed, never overwritten — because writing
					// "gone" here would make the next plan propose
					// recreating infrastructure that may well still exist.
					fmt.Fprintf(out, "  %s: could not refresh (see diagnostics)\n", addr)
				case o.State == nil:
					// The provider affirmatively reports this resource
					// gone. This is the ONLY case that removes it from
					// state.
					fmt.Fprintf(out, "  %s: no longer exists; removed from state\n", addr)
					st.Remove(addr)
					wrote = true
				default:
					fmt.Fprintf(out, "  %s: refreshed\n", addr)
					st.Set(o.State)
					wrote = true
				}
			}

			if wrote {
				if err := backend.Put(ctx, environment, st); err != nil {
					return fmt.Errorf("writing refreshed state: %w", err)
				}
			}

			if ds.HasErrors() {
				return errors.New("refresh completed with errors; some resources could not be read")
			}
			return nil
		},
	}
}
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/cli/refresh_test.go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

func TestRefreshRemovesAResourceTheProviderReportsGone(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	// Delete it from the fake cloud directly, standing in for someone
	// deleting it outside infra entirely.
	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	delete(cloud.Resources, rs.ProviderID)
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if !strings.Contains(stdout.String(), "no longer exists") {
		t.Errorf("stdout does not report the removal:\n%s", stdout.String())
	}

	after, gerr := backendFor(dir).Get(ctx, "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	if _, ok := after.Get(address.Address{Name: "network"}); ok {
		t.Error("network is still in state after refresh observed it gone")
	}
}

func TestRefreshPersistsDriftedAttributes(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	// Someone resizes it by hand, outside infra.
	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	cloud.Resources[rs.ProviderID].Attributes["cidr"] = "10.99.0.0/16"
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	data, rerr := os.ReadFile(filepath.Join(dir, ".infra", "state", "dev.json"))
	if rerr != nil {
		t.Fatalf("reading state file: %v", rerr)
	}
	var doc struct {
		Resources map[string]struct {
			Attributes map[string]struct {
				Raw any `json:"raw"`
			} `json:"attributes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	if got := doc.Resources["network"].Attributes["cidr"].Raw; got != "10.99.0.0/16" {
		t.Errorf("state cidr = %v, want the drifted value %q — refresh must persist what it observed", got, "10.99.0.0/16")
	}
}

// TestRefreshNeverTreatsAReadErrorAsDeletion is the test the brief names as
// guarding "the worst bug available in this milestone". It injects a read
// failure rather than an absence, and asserts the resource survives in
// state UNCHANGED. A refresh implementation that collapsed
// Observation.Err != nil into the same branch as State == nil would delete
// the resource from state here and this test would catch it: the address
// would be gone from state instead of present with its original
// attributes.
func TestRefreshNeverTreatsAReadErrorAsDeletion(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	ctx := context.Background()
	cloudPath := filepath.Join(dir, testprovider.DefaultCloudPath)
	prov := testprovider.New(cloudPath)
	rs, err := prov.Create(ctx, &resource.DesiredResource{
		Address: address.Address{Name: "network"},
		Type:    "test.network",
		Attrs: map[string]value.Value{
			"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
		},
	})
	if err != nil {
		t.Fatalf("seeding the fake cloud: %v", err)
	}
	st := state.New("myapp", "dev")
	st.Set(rs)
	seedState(t, dir, "dev", st)

	cloud, err := testprovider.LoadCloud(cloudPath)
	if err != nil {
		t.Fatalf("loading fake cloud: %v", err)
	}
	cloud.Failures = append(cloud.Failures, testprovider.FailureRule{
		Op:           "read",
		Address:      "network",
		Nth:          1,
		Retryability: testprovider.RetryNotSafe,
		Message:      "simulated provider outage",
	})
	if err := cloud.Save(cloudPath); err != nil {
		t.Fatalf("saving fake cloud: %v", err)
	}

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err = cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error — a read failure must fail the command")
	}
	if !strings.Contains(stderr.String(), "network") {
		t.Errorf("stderr does not name the resource that failed to read: %s", stderr.String())
	}

	after, gerr := backendFor(dir).Get(ctx, "dev")
	if gerr != nil {
		t.Fatalf("reading state: %v", gerr)
	}
	got, ok := after.Get(address.Address{Name: "network"})
	if !ok {
		t.Fatal("network was removed from state after a read ERROR — a read error must never be treated as deletion")
	}
	if got.Attributes["cidr"].Raw() != "10.20.0.0/16" {
		t.Errorf("network's recorded cidr changed after a failed read: got %v, want unchanged %q", got.Attributes["cidr"].Raw(), "10.20.0.0/16")
	}
}

func TestRefreshLockConflictNamesTheHolder(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources: {}
`)
	backend := backendFor(dir)
	lockCtx := state.WithOperation(context.Background(), "some-other-run")
	if _, err := backend.Lock(lockCtx, "dev"); err != nil {
		t.Fatalf("pre-locking dev: %v", err)
	}
	defer backend.ForceUnlock("dev")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4}
	cmd := newRefreshCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error — the environment is already locked")
	}
	if !strings.Contains(err.Error(), "some-other-run") {
		t.Errorf("error does not name the holder's operation: %v", err)
	}
}
```

Note on `got.Attributes["cidr"].Raw()`: confirm the exact accessor against
`pkg/value.Value` before using it (§ "Verify before you assert" — this contract does not
enumerate `value.Value`'s full method set beyond `Format`/`Equal`). If `Raw` is a field
rather than a method, or returns `any` requiring a type assertion, adjust the assertion
accordingly; the point being tested — the attribute is byte-for-byte what it was before
the failed read — does not change.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/cli/ -run TestRefresh -v`
Expected: FAIL to build — `newRefreshCommand` is undefined (`refresh.go` does not exist
yet).

- [ ] **Step 3: Implement**

Write `internal/cli/refresh.go` exactly as above. Modify `internal/cli/root.go`:

```go
	root.AddCommand(newDestroyCommand(opts))
	root.AddCommand(newRefreshCommand(opts))
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/ -v`
Expected: PASS — all `internal/cli` tests, including the five new ones above.
`TestRefreshNeverTreatsAReadErrorAsDeletion` is the one that matters most: confirm it
actually fails (resource missing from state) against a deliberately broken version of the
switch that merges the `o.Err != nil` and `o.State == nil` branches, before trusting it
passes for the right reason against the real implementation.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/refresh.go internal/cli/refresh_test.go internal/cli/root.go
git commit -m "$(cat <<'EOF'
Add `infra refresh`: the only M3 verb that persists provider observations

refresh takes the environment lock up front — unlike apply/destroy, its
entire job is the write, so there is no unlocked preview to compute and
discard. A resource the provider reports gone (State == nil, Err == nil) is
removed from state; a resource that failed to read (Err != nil) is left
exactly as it was, because reality is unknown, not confirmed absent, and
treating the two the same would make the next plan propose recreating
infrastructure that may still exist.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

### Task 16: M3 integration tests

**Files:**
- Create: `tests/integration/m3_test.go`

**Interfaces:**
- Consumes: `binary`, `run`, `project`, `requireContains` (from `helpers_test.go`, unchanged
  — no modification needed, see below); the built `infra` binary's `apply`, `destroy`,
  `refresh`, `plan`, `validate` subcommands.
- Produces: nothing further tasks depend on.

Three things need proving end to end against the real binary, and each needs a test that
would fail against a plausible wrong implementation, not one that only exercises the happy
path.

**The MVP round trip**, scoped to what M3 actually ships. §48's full script starts with
`infra init`; `internal/cli/root.go` has no such command, no task in this milestone's
fifteen builds one, and the design spec's own milestone table (§19) places "§48 MVP script
green" at M7, not M3 — the spec's milestone table was amended to say so, since no
row had claimed `init`. This test scaffolds configuration with the
existing `project()` helper instead, exactly as M2's own suite does, and drives
`validate → plan → apply → plan (clean) → hand-edit the fake cloud → plan (drift) →
refresh (persists the drift) → remove the resource from YAML → plan (destroy proposed) →
apply (destroy applied)`. This is the reconcile loop invariants 1 and 2 exist to describe,
run as one continuous script instead of asserted piecemeal, and it is the first test in the
whole suite that proves `refresh` actually changes what a *subsequent* command sees — no
unit test spans two command invocations the way this does.

**A concurrency test for invariant 5** that could not pass against a serial
implementation. Two real OS processes are started — `first` via `cmd.Start()` (never
`cmd.Wait()`'d before the second process begins), so they are genuinely running at the
same time, not merely invoked one after another. The fake cloud's configurable latency
(`latency_ms` in `.infra/fake-cloud.json`, already used by the provider's own tests) is set
high enough that the first apply is still inside its slow `Create` call — which happens
*after* it has taken the lock — when the second apply attempts to lock the same
environment. What this catches: an `apply` that never calls `Lock` at all (both processes
would proceed, the second would not fail, and the fake cloud would end up with two
resources at address `network` instead of one); an `apply` that takes the lock only around
the final state write rather than the whole execution (the second process could still slip
in between plan and execute); or a `Local.Lock` that is not actually safe under real
concurrent processes (flaky, not deterministic, failures specifically under this test).

**A dependency-ordering test for invariant 4** that cannot pass by accident. Per the
contract's test rules, a fixture whose alphabetical order already matches the required
execution order would still pass against a graph walker that silently fell back to
declaration or alphabetical order instead of real dependency edges. The fixture below
names the dependency `zzz_network` and the dependent `database` — `"database" <
"zzz_network"` alphabetically, the opposite of the required creation order — so an
implementation bug of exactly that shape is caught rather than hidden.

```go
// tests/integration/m3_test.go
package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startAsync starts the CLI without waiting for it, so the caller can start
// a second invocation while the first is still running — the genuine
// overlap a concurrency test needs. Unlike run, it does not block on
// cmd.Wait(); the caller does that explicitly once it needs the result.
func startAsync(t *testing.T, dir string, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(binary(t), append([]string{"--chdir", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting infra %v: %v", args, err)
	}
	return cmd, &stdout, &stderr
}

// runStdin is run with stdin controlled, for the typed-confirmation tests.
// run (helpers_test.go) has no way to set stdin, and is left untouched —
// this is additive, not a duplicate of it.
func runStdin(t *testing.T, dir, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary(t), append([]string{"--chdir", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if err2, ok := err.(*exec.ExitError); ok {
			exitErr = err2
		}
		if exitErr == nil {
			t.Fatalf("running infra %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// seedCloudLatency writes .infra/fake-cloud.json with no resources and a
// simulated per-operation delay, so a subsequent apply's Create call is
// slow enough to overlap a second process's attempt to lock the
// environment.
func seedCloudLatency(t *testing.T, dir string, ms int) {
	t.Helper()
	cloudDir := filepath.Join(dir, ".infra")
	if err := os.MkdirAll(cloudDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	doc := map[string]any{"resources": map[string]any{}, "latency_ms": ms}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal fake cloud: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cloudDir, "fake-cloud.json"), data, 0o600); err != nil {
		t.Fatalf("write fake cloud: %v", err)
	}
}

func readFakeCloudResources(t *testing.T, dir string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".infra", "fake-cloud.json"))
	if err != nil {
		t.Fatalf("reading fake cloud: %v", err)
	}
	var doc struct {
		Resources map[string]map[string]any `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding fake cloud: %v", err)
	}
	return doc.Resources
}

// cloudResourceID returns the single provider id whose recorded address
// matches name, failing loudly on anything other than exactly one match —
// silently picking "the first" on ambiguity would make a future
// multi-resource fixture pass or fail for the wrong reason.
func cloudResourceID(t *testing.T, dir, name string) string {
	t.Helper()
	var found []string
	for id, r := range readFakeCloudResources(t, dir) {
		if addr, _ := r["address"].(string); addr == name {
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one cloud resource with address %q, found %d", name, len(found))
	}
	return found[0]
}

func mutateCloudAttribute(t *testing.T, dir, id, key string, value any) {
	t.Helper()
	path := filepath.Join(dir, ".infra", "fake-cloud.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fake cloud: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding fake cloud: %v", err)
	}
	resources := doc["resources"].(map[string]any)
	resource := resources[id].(map[string]any)
	attrs := resource["attributes"].(map[string]any)
	attrs[key] = value
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal fake cloud: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("writing fake cloud: %v", err)
	}
}

func readStateFile(t *testing.T, dir, environment string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".infra", "state", environment+".json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	return doc
}

func readStateAttribute(t *testing.T, dir, environment, name, attr string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".infra", "state", environment+".json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var doc struct {
		Resources map[string]struct {
			Attributes map[string]struct {
				Raw any `json:"raw"`
			} `json:"attributes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	r, ok := doc.Resources[name]
	if !ok {
		t.Fatalf("resource %q not found in state", name)
	}
	a, ok := r.Attributes[attr]
	if !ok {
		t.Fatalf("attribute %q not found on %q", attr, name)
	}
	s, ok := a.Raw.(string)
	if !ok {
		t.Fatalf("attribute %q on %q is not a string: %#v", attr, name, a.Raw)
	}
	return s
}

// TestM3MVPRoundTrip drives §48's script as far as M3's own scope goes: no
// `infra init` (see this task's doc comment — it is M7, not M3), starting
// instead from a hand-written infra.yml exactly like M2's suite already
// does.
func TestM3MVPRoundTrip(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)

	if v := run(t, dir, "validate"); v.ExitCode != 0 {
		t.Fatalf("validate exit code %d, want 0\n%s", v.ExitCode, v.combined())
	}

	p1 := run(t, dir, "plan", "dev")
	if p1.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2\n%s", p1.ExitCode, p1.combined())
	}
	requireContains(t, p1.Stdout, "+ test.network.network")
	if _, err := os.Stat(filepath.Join(dir, ".infra", "state", "dev.json")); !os.IsNotExist(err) {
		t.Fatal("plan must not have written state")
	}

	a1 := run(t, dir, "apply", "dev", "--auto-approve")
	if a1.ExitCode != 2 {
		t.Fatalf("apply exit code %d, want 2\n%s", a1.ExitCode, a1.combined())
	}
	requireContains(t, a1.Stdout, "Apply complete!")

	p2 := run(t, dir, "plan", "dev")
	if p2.ExitCode != 0 {
		t.Fatalf("re-plan exit code %d, want 0 (no changes)\n%s", p2.ExitCode, p2.combined())
	}
	requireContains(t, p2.Stdout, "No changes")
	if strings.Contains(p2.Stdout, "Usage:") || strings.Contains(p2.Stdout, "Error:") {
		t.Errorf("re-plan output is polluted:\n%s", p2.Stdout)
	}

	// Externally mutate .infra/fake-cloud.json by hand, standing in for a
	// person changing real infrastructure outside infra entirely.
	id := cloudResourceID(t, dir, "network")
	mutateCloudAttribute(t, dir, id, "cidr", "10.99.0.0/16")

	p3 := run(t, dir, "plan", "dev")
	if p3.ExitCode != 2 {
		t.Fatalf("drift plan exit code %d, want 2\n%s", p3.ExitCode, p3.combined())
	}
	requireContains(t, p3.Stdout, "~ test.network.network")
	requireContains(t, p3.Stdout, "10.99.0.0/16 -> 10.20.0.0/16")

	// plan never wrote the drift down (spec §10) — refresh does.
	r1 := run(t, dir, "refresh", "dev")
	if r1.ExitCode != 0 {
		t.Fatalf("refresh exit code %d, want 0\n%s", r1.ExitCode, r1.combined())
	}
	if got := readStateAttribute(t, dir, "dev", "network", "cidr"); got != "10.99.0.0/16" {
		t.Errorf("refresh did not persist the observed drift: state cidr = %q, want %q", got, "10.99.0.0/16")
	}

	// Remove the resource from configuration.
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte("project: myapp\nresources: {}\n"), 0o644); err != nil {
		t.Fatalf("rewriting infra.yml: %v", err)
	}

	p4 := run(t, dir, "plan", "dev")
	if p4.ExitCode != 2 {
		t.Fatalf("removal plan exit code %d, want 2\n%s", p4.ExitCode, p4.combined())
	}
	requireContains(t, p4.Stdout, "- test.network.network")

	a2 := run(t, dir, "apply", "dev", "--auto-approve")
	if a2.ExitCode != 2 {
		t.Fatalf("teardown apply exit code %d, want 2\n%s", a2.ExitCode, a2.combined())
	}
	requireContains(t, a2.Stdout, "Apply complete!")

	st := readStateFile(t, dir, "dev")
	if _, ok := st["resources"].(map[string]any)["network"]; ok {
		t.Error("network is still recorded in state after its destroy was applied")
	}
}

// TestConcurrentApplyToOneEnvironmentSerializes proves invariant 5 with two
// genuinely overlapping OS processes, not two sequential invocations. See
// this task's doc comment for exactly what a broken implementation looks
// like under this test.
func TestConcurrentApplyToOneEnvironmentSerializes(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	seedCloudLatency(t, dir, 500)

	first, firstOut, firstErr := startAsync(t, dir, "apply", "dev", "--auto-approve")
	time.Sleep(150 * time.Millisecond) // give the first apply time to lock and enter its slow Create

	second := run(t, dir, "apply", "dev", "--auto-approve")

	if err := first.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("waiting for the first apply: %v", err)
		}
	}
	firstCode := first.ProcessState.ExitCode()
	if firstCode != 2 {
		t.Fatalf("first apply exit code %d, want 2 (it should have won the lock and applied)\n%s%s",
			firstCode, firstOut.String(), firstErr.String())
	}
	requireContains(t, firstOut.String(), "Apply complete!")

	if second.ExitCode != 1 {
		t.Fatalf("second apply exit code %d, want 1 (the environment was already locked)\n%s", second.ExitCode, second.combined())
	}
	requireContains(t, second.Stderr, "is locked")
	requireContains(t, second.Stderr, "apply")

	count := 0
	for _, r := range readFakeCloudResources(t, dir) {
		if addr, _ := r["address"].(string); addr == "network" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("fake cloud has %d resources at address \"network\", want exactly 1 — "+
			"a lock that failed to serialize the two applies let both create it", count)
	}
}

// TestApplyCreatesDependencyBeforeDependent proves invariant 4. zzz_network
// sorts AFTER database alphabetically — the opposite of the order
// dependency resolution requires — so a walker that silently fell back to
// declaration or alphabetical order instead of real dependency edges would
// create database first, and this test would catch it via the cloud's
// globally increasing id counter.
func TestApplyCreatesDependencyBeforeDependent(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    network: ${zzz_network.id}
  zzz_network:
    type: test.network
    cidr: 10.20.0.0/16
`)

	res := run(t, dir, "apply", "dev", "--auto-approve")
	if res.ExitCode != 2 {
		t.Fatalf("apply exit code %d, want 2\n%s", res.ExitCode, res.combined())
	}

	netID := cloudResourceID(t, dir, "zzz_network")
	dbID := cloudResourceID(t, dir, "database")
	netN := trailingNumber(t, netID)
	dbN := trailingNumber(t, dbID)
	if !(netN < dbN) {
		t.Errorf("zzz_network (id %s) was not created before database (id %s) — invariant 4 violated", netID, dbID)
	}

	// The dependent's reference resolved to the real id, not an unresolved
	// placeholder — proof the executor deferred the expression until
	// zzz_network actually completed (spec §15), not just that the two
	// creates happened to land in some order for an unrelated reason.
	if got := readStateAttribute(t, dir, "dev", "database", "network"); got != netID {
		t.Errorf("database.network = %q, want %q (zzz_network's real id)", got, netID)
	}
}

func trailingNumber(t *testing.T, id string) int {
	t.Helper()
	i := strings.LastIndex(id, "-")
	if i < 0 {
		t.Fatalf("cloud id %q has no numeric suffix", id)
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil {
		t.Fatalf("cloud id %q has a non-numeric suffix: %v", id, err)
	}
	return n
}

// TestDestroyRequiresTypedEnvironmentName exercises task 13's confirmation
// design end to end against the real binary: a bare "y" (or any word other
// than the environment's own name) must be refused, and the exact name must
// be accepted.
func TestDestroyRequiresTypedEnvironmentName(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	if res := run(t, dir, "apply", "dev", "--auto-approve"); res.ExitCode != 2 {
		t.Fatalf("setup apply failed: exit %d\n%s", res.ExitCode, res.combined())
	}

	bare := runStdin(t, dir, "y\n", "destroy", "dev")
	if bare.ExitCode != 1 {
		t.Fatalf("destroy with a bare y exit code %d, want 1\n%s", bare.ExitCode, bare.combined())
	}
	stillThere := readStateFile(t, dir, "dev")
	if _, ok := stillThere["resources"].(map[string]any)["network"]; !ok {
		t.Error("network was removed from state despite typed confirmation being refused")
	}

	confirmed := runStdin(t, dir, "dev\n", "destroy", "dev")
	if confirmed.ExitCode != 2 {
		t.Fatalf("destroy with the correct typed confirmation exit code %d, want 2\n%s", confirmed.ExitCode, confirmed.combined())
	}
	st := readStateFile(t, dir, "dev")
	if _, ok := st["resources"].(map[string]any)["network"]; ok {
		t.Error("network is still recorded in state after a confirmed destroy")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./tests/integration/ -run 'TestM3MVPRoundTrip|TestConcurrentApplyToOneEnvironmentSerializes|TestApplyCreatesDependencyBeforeDependent|TestDestroyRequiresTypedEnvironmentName' -v`
Expected: FAIL to build — the binary target `infra/cmd/infra` does not yet have `apply`,
`destroy` or `refresh` subcommands (tasks 12–14 not yet applied to this checkout), so
`run(t, dir, "apply", ...)` returns exit code 1 with cobra's "unknown command" error, and
every assertion on exit code 2 fails. Once tasks 12–14 land, re-run this step before
Step 3 exists to confirm — there is nothing further for this task to implement.

- [ ] **Step 3: Implement**

Nothing to implement — this task is the test file itself, written against tasks 12–14's
already-landed commands. If Step 2 was run before tasks 12–14 landed, re-run it now.

- [ ] **Step 4: Run the tests**

Run: `go test ./tests/integration/ -v`
Expected: PASS — the full integration suite, M2's tests unchanged plus the five new M3
tests (`TestM3MVPRoundTrip`, `TestConcurrentApplyToOneEnvironmentSerializes`,
`TestApplyCreatesDependencyBeforeDependent`, `TestDestroyRequiresTypedEnvironmentName`, and
`TestDestroyForgetsARetainedResourceWithoutDeletingIt` if not already covered at the unit
level in task 13). Run `go test ./tests/integration/ -race -run TestConcurrentApply -v` at
least once, since the concurrency test is the one most likely to be flaky under `-race`
if the lock is not actually exclusive.

- [ ] **Step 5: Commit**

```bash
git add tests/integration/m3_test.go
git commit -m "$(cat <<'EOF'
Add M3 integration tests: MVP round trip, concurrent-apply locking, dependency ordering

Drives the real binary through validate/plan/apply/refresh/destroy end to
end, including a hand-edit of .infra/fake-cloud.json to induce drift the
same way a person would. Two genuinely overlapping OS processes prove
invariant 5 (one apply wins the lock, the other reports who holds it, and
the fake cloud ends up with exactly one resource, not two); a fixture whose
alphabetical order contradicts its dependency order proves invariant 4.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_015M5QdCFKa6booSVrhnAzTV
EOF
)"
```

---

## M3 Definition of Done

Every box must be true before M4 begins.

- [ ] `make check` green from a clean checkout; `go test ./... -race` clean.
- [ ] `go list -deps ./providers/... | grep '^infra/internal'` prints nothing.
- [ ] `go list -deps ./internal/graph | grep '^infra/'` prints only `infra/internal/graph` — the leaf property holds.
- [ ] `infra apply dev` on a fresh project creates every resource and exits 0; a second `apply` reports no changes and exits 0.
- [ ] `infra plan dev` still takes no lock; `apply`, `destroy` and `refresh` each take one for their whole run.
- [ ] A second `apply` against a locked environment is refused and **names who holds the lock** — user, host and pid — not merely that one exists.
- [ ] **Invariant 4:** a resource never executes before its dependencies, proven on a fixture whose alphabetical order contradicts its required order.
- [ ] **Invariant 5:** two concurrent `apply` processes cannot both mutate one environment. Proven with two real processes, not two sequential runs.
- [ ] State is written after every operation, not batched: a run killed midway leaves state describing what actually happened.
- [ ] A failed operation skips its dependents transitively, independent branches still complete, and the summary reports applied / failed / skipped separately.
- [ ] A partial failure exits 1 even though real work succeeded.
- [ ] `Create` is never retried on an ambiguous failure. `Delete` retries only on `SafeToRetry`.
- [ ] `SIGINT` finishes the in-flight operation, persists state, releases the lock, exits non-zero. A second `SIGINT` exits immediately, leaves the lock, and the next run reports it as stale with the command to clear it.
- [ ] `infra refresh dev` persists observations; a provider reporting a resource **gone** removes it from state, while a read **error** does not.
- [ ] `infra destroy dev` requires typed confirmation, not a bare `y`, and honours `prevent_destroy` and `retain` exactly as the planner encodes them.
- [ ] A sensitive value never appears in apply output, summary output, or a progress event, at any nesting depth.
- [ ] `ConfigHash` differs for two configurations that differ only in a `--var` feeding a deferred expression.
- [ ] The §48 MVP round trip runs green end to end against the fake provider.

## What M3 deliberately leaves undone

A partial feature is worse than an absent one, because it looks present.

- Variables beyond `--var`, `--var-file`, environments and `extends` — M4. `--var-file` errors rather than being silently ignored.
- Modules — M5.
- Reading a saved plan back (`infra apply <env> plan.json`), staleness refusal, environment-level `protections` — M6.
- `init`, `explain`, `graph`, `state show`, `discover`, `import`, `export` — M7 and Phase 2.
- Encryption of state at rest — Phase 4.

## Carry-forwards into M4

- `ResourceState.Clone` is not deep for the interior of a composite value; `value.Value` has no `Clone`. Recorded in M2, still true.
- `expressions/eval.go` classifies sensitivity top-level-only while `funcs.go` does it recursively. Harmless while unknowns carry no data — but M3's executor resolves them, so re-check it once Task 7 lands.
- `internal/planner`'s `p.Diagnostics == ds` holds by construction across three duplicated assignments and is enforced by nothing. A `reflect.DeepEqual` test, or one assignment instead of three, would catch drift at the source.
