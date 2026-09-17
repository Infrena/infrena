# State Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `infrena state migrate` moves state between any two backends, safely enough to run from CI by someone who cannot reach the cloud directly.

**Architecture:** A temporary `migrate_from:` block sits beside `backend:`, taking the same shape and the same literal-only rule, so both ends of a migration are fully configured and nothing is implicit. It is **inert for every command except `state migrate`** — a migration performed through CI is necessarily two commits, and an active block between them would break every `plan` in the window. The command locks both ends, decides on three cases rather than two, copies, verifies by reading back, and leaves the source authoritative on any failure.

**Tech Stack:** Go 1.27, Cobra, `gopkg.in/yaml.v3`. Standard library only.

**Spec:** `docs/superpowers/specs/2026-09-16-remote-state-design.md` §7 (redesigned 2026-09-17)

## Global Constraints

- Go 1.27.0 floor. `mise` is not active in non-interactive shells — use `make` targets or `export PATH="$HOME/.local/share/mise/shims:$PATH"`.
- **Third-party budget is two packages**: `github.com/spf13/cobra`, `gopkg.in/yaml.v3`.
- **`migrate_from:` never interpolates**, for the identical ordering reason `backend:` does not (§3): state is read before anything compiles, and compiling is what resolves variables.
- **`plugin:` is the only reserved key** in either block; every other key crosses to the backend untouched, so an unrecognised key is not an error while a missing `plugin:` is.
- **A backend stores bytes and does not interpret them.** Migration moves `*state.State` values through `Get`/`Put` and never parses or rewrites them.
- **Every backend must lock.** Migration takes both locks and releases both.
- **A failure leaves the SOURCE authoritative.** Same rule that orders configuration before state in `import`: the half left done must be the harmless one.
- **The network is never on the hot path.** Nothing here changes that; `state migrate` is a command a user runs deliberately.
- Errors follow §44: what is wrong, where, what was expected, an action the user can take.
- `gofmt -l .` and `go vet ./...` clean; full `go test -count=1 ./...` green with `INFRENA_REQUIRE_PLUGIN=1`.
- Commit messages: plain English, no em-dashes, minimal, **never** mention Claude, AI or the model. Each ends with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/declarations.go` | `ProjectDecl.MigrateFrom` — a second `BackendDecl`. |
| `internal/config/decode_backend.go` | Decoding both blocks through one path. |
| `internal/cli/context.go` | `backendFor` gains a sibling that opens the `migrate_from:` backend. |
| `internal/cli/state_migrate.go` *(new)* | The command: the three-case decision, the copy, the verify. |
| `internal/cli/state_migrate_test.go` *(new)* | Including the CI-shaped tests. |
| `internal/cli/state.go` | Register the subcommand. |

---

## Task 1: `migrate_from:` decodes, and changes nothing else

**Files:**
- Modify: `internal/config/declarations.go`, `internal/config/decode_backend.go`
- Test: `internal/config/decode_backend_test.go`

**Interfaces:**
- Produces: `ProjectDecl.MigrateFrom config.BackendDecl` — the same type `Backend` uses. Zero value (empty `Plugin`) means absent.

- [ ] **Step 1: Write the failing test**

```go
func TestMigrateFromDecodesLikeABackendBlock(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
backend:
  plugin: s3
  bucket: new-bucket
migrate_from:
  plugin: s3
  bucket: old-bucket
  region: eu-west-1
`,
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if p.Backend.Plugin != "s3" || p.Backend.Config["bucket"] != "new-bucket" {
		t.Errorf("Backend = %+v", p.Backend)
	}
	if p.MigrateFrom.Plugin != "s3" || p.MigrateFrom.Config["bucket"] != "old-bucket" {
		t.Errorf("MigrateFrom = %+v", p.MigrateFrom)
	}
}

// Both ends are namable, so nothing is implicit. `plugin: local` is how a
// migration says "the built-in local backend" rather than relying on the
// block being absent.
func TestLocalIsNamableInBothBlocks(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "backend:\n  plugin: s3\n  bucket: b\nmigrate_from:\n  plugin: local\n",
	})
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	if p.MigrateFrom.Plugin != "local" {
		t.Errorf("MigrateFrom.Plugin = %q, want local", p.MigrateFrom.Plugin)
	}
}

// The same ordering cycle as `backend:`, so the same refusal with the same
// explanation. A reader who meets it in one block must not have to discover
// it separately in the other.
func TestAVariableInMigrateFromIsRefusedWithTheSameReason(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "backend:\n  plugin: s3\n  bucket: b\nmigrate_from:\n  plugin: s3\n  bucket: ${var.old}\n",
	})
	if !ds.HasErrors() {
		t.Fatal("a variable in migrate_from was accepted")
	}
	if !strings.Contains(ds.Error(), "before") {
		t.Errorf("diagnostic does not explain the ordering: %s", ds.Error())
	}
}

func TestAMigrateFromWithNoPluginIsAnError(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "backend:\n  plugin: s3\n  bucket: b\nmigrate_from:\n  bucket: old\n",
	})
	if !ds.HasErrors() {
		t.Fatal("a migrate_from with no plugin was accepted")
	}
}

// A project with no migrate_from is every project, and must be untouched.
func TestNoMigrateFromBlockIsTheOrdinaryCase(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{ProjectFileName: projectWithNoResources})
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	if p.MigrateFrom.Plugin != "" {
		t.Errorf("MigrateFrom.Plugin = %q, want empty", p.MigrateFrom.Plugin)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestMigrateFrom|TestLocalIsNamable|TestAVariableInMigrateFrom|TestAMigrateFromWithNoPlugin|TestNoMigrateFromBlock' ./internal/config/ -v`
Expected: FAIL — `ProjectDecl` has no `MigrateFrom`.

- [ ] **Step 3: Write minimal implementation**

Decode both blocks through **one function**, so the two cannot drift. Read `decode_backend.go` first; `migrate_from:` should be the same call with a different key and a different destination field.

```go
	// MigrateFrom is where `state migrate` reads from. Same shape and same
	// rules as Backend, decoded by the same code, because two blocks that
	// mean the same thing must not be able to disagree about what a key means.
	//
	// NO COMMAND BUT `state migrate` ACTS ON IT (spec §7). A migration run
	// through CI is necessarily two commits — one adding this block, one
	// removing it — and between them it sits in committed configuration. If
	// `plan` or `apply` acted on it, a SUCCESSFUL migration would break the
	// pipeline until somebody tidied up.
	//
	// Ordinary commands DO consult it for one GUARD, added after the first
	// draft: while the destination is empty and the source holds state, the
	// migration has not happened, and a command reading only `backend:` would
	// see every resource as unmanaged and RECREATE ALL OF IT. See Task 4.
	MigrateFrom BackendDecl
```

`plugin: local` must be accepted rather than rejected as an unknown plugin — that is Task 2's business to resolve, but the decoder must not refuse it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS. **Every existing test must pass untouched** — this block changes no behaviour yet.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/config
git commit -m "Read a migrate from block beside the backend block

Both ends of a migration have to be configured, and one block holds one.
It decodes through the same code as the backend block so the two cannot
disagree about what a key means, and nothing reads it yet."
```

---

## Task 2: Opening both ends, and naming local

**Files:**
- Modify: `internal/cli/context.go`
- Test: `internal/cli/backend_test.go`

**Interfaces:**
- Consumes: Task 1's `ProjectDecl.MigrateFrom`; existing `backendFor(ctx, opts) (state.Backend, func() error, error)`.
- Produces: `func openBackend(ctx context.Context, opts *GlobalOptions, decl config.BackendDecl) (state.Backend, func() error, error)` — opens any declared backend, with `plugin: local` returning `state.NewLocal`.

- [ ] **Step 1: Write the failing test**

```go
// `plugin: local` is how a migration names the built-in backend. Without it,
// "migrate to local" could only be expressed by ABSENCE, and absence already
// means "no migration".
func TestOpenBackendResolvesLocalByName(t *testing.T) {
	dir := newProjectFixture(t)

	b, closeFn, err := openBackend(context.Background(), &GlobalOptions{Dir: dir},
		config.BackendDecl{Plugin: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	if _, ok := b.(*state.Local); !ok {
		t.Errorf("openBackend returned %T, want *state.Local", b)
	}
}

// Two ends open independently, so a migration holds both at once.
func TestBothEndsCanBeOpenAtTheSameTime(t *testing.T) {
	dir := newProjectFixture(t)
	opts := &GlobalOptions{Dir: dir}

	a, closeA, err := openBackend(context.Background(), opts, config.BackendDecl{Plugin: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeA()
	b, closeB, err := openBackend(context.Background(), opts, config.BackendDecl{Plugin: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeB()

	if a == nil || b == nil {
		t.Fatal("one end failed to open")
	}
}

// A backend named but not installed is an error naming it and how to get it,
// exactly as backendFor already refuses. Never a silent fall back to local:
// migrating INTO local when the user asked for a bucket would put state
// somewhere they did not ask for, which is the worst available outcome.
func TestAMissingBackendPluginInAMigrationIsAnError(t *testing.T) {
	dir := newProjectFixture(t)

	_, _, err := openBackend(context.Background(), &GlobalOptions{Dir: dir},
		config.BackendDecl{Plugin: "nosuch"})
	if err == nil {
		t.Fatal("a missing backend opened silently")
	}
	if !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("error does not name the backend: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestOpenBackendResolvesLocal|TestBothEndsCanBeOpen|TestAMissingBackendPluginInAMigration' ./internal/cli/ -v`
Expected: FAIL — `openBackend` undefined.

- [ ] **Step 3: Write minimal implementation**

Factor the body of `backendFor` into `openBackend(ctx, opts, decl)`, then make `backendFor` read `infra.yml`'s `backend:` and call it. `Plugin == "local"` returns `state.NewLocal` with a no-op closer; empty `Plugin` keeps meaning local for `backendFor`'s own callers, which is existing behaviour and must not change.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS, whole suite untouched.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli
git commit -m "Open any declared backend, local included

A migration holds both ends at once, and local has to be namable or
migrating back to it could only be said by leaving a block out, which
already means something else."
```

---

## Task 3: `infrena state migrate`

**Files:**
- Create: `internal/cli/state_migrate.go`, `internal/cli/state_migrate_test.go`
- Modify: `internal/cli/state.go` (register it)

**Interfaces:**
- Consumes: Task 2's `openBackend`; `state.Backend`'s `Get`, `Put`, `List`, `Lock`, `Unlock`.
- Produces: `infrena state migrate [--force]`.

- [ ] **Step 1: Write the failing test**

```go
func TestMigrateMovesEveryEnvironment(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t) // migrate_from: local, backend: a fake remote
	seedLocalState(t, dir, "dev", "production")

	stdout, _, code := runCommand(t, dir, "state", "migrate")

	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}
	for _, env := range []string{"dev", "production"} {
		if !destinationHasState(t, dir, env) {
			t.Errorf("%s did not arrive at the destination", env)
		}
	}
}

// THE CI CASE. A migration through a pipeline is two commits, and between
// them this block sits in committed configuration. If an ordinary command
// consulted it, a SUCCESSFUL migration would break every plan until somebody
// pushed the removal.
func TestMigrateFromDoesNotAffectOrdinaryCommands(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	if _, _, code := runCommand(t, dir, "state", "migrate"); code != ExitOK {
		t.Fatal("setup migration failed")
	}

	// migrate_from: is STILL in the config, and the destination now has state.
	for _, args := range [][]string{
		{"plan", "dev"}, {"validate"}, {"state", "list", "dev"},
	} {
		_, stderr, code := runCommand(t, dir, args...)
		if code == ExitError {
			t.Errorf("%v failed with migrate_from still present:\n%s", args, stderr)
		}
	}
}

// A re-run must be safe. CI re-runs happen, and the natural response to a red
// pipeline is to add --force to the workflow file, where it then sits on every
// future run. Succeeding as a no-op is what stops that.
func TestMigratingTwiceIsANoOpWhenBothEndsAgree(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	runCommand(t, dir, "state", "migrate")

	stdout, _, code := runCommand(t, dir, "state", "migrate")

	if code != ExitOK {
		t.Fatalf("a second migration failed: exit %d\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "already migrated") {
		t.Errorf("output does not say it was already done:\n%s", stdout)
	}
}

// Differing state at both ends means somebody has been applying to one of
// them. Choosing a winner silently destroys real work.
func TestMigrateRefusesWhenBothEndsHoldDifferentState(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	seedDestinationState(t, dir, "dev", "something else entirely")

	_, stderr, code := runCommand(t, dir, "state", "migrate")

	if code == ExitOK {
		t.Fatal("migrate overwrote differing state without being asked")
	}
	if !strings.Contains(stderr, "dev") {
		t.Errorf("refusal does not name the environment:\n%s", stderr)
	}
	// --force must not be the first thing offered. It is the same hazard as a
	// lock: false escape hatch, and it arrives the same way: somebody adding a
	// flag to get past a red build.
	first := strings.SplitN(stderr, "\n", 2)[0]
	if strings.Contains(first, "--force") {
		t.Errorf("the refusal leads with --force:\n%s", stderr)
	}
}

func TestForceOverwritesDifferingState(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	seedDestinationState(t, dir, "dev", "something else entirely")

	_, _, code := runCommand(t, dir, "state", "migrate", "--force")

	if code != ExitOK {
		t.Fatalf("--force did not overwrite: exit %d", code)
	}
}

// A failure must leave the source authoritative. The half left done has to be
// the harmless one.
func TestAFailedMigrationLeavesTheSourceIntact(t *testing.T) {
	dir := newProjectMigratingLocalToFailingDestination(t)
	seedLocalState(t, dir, "dev")

	_, _, code := runCommand(t, dir, "state", "migrate")

	if code == ExitOK {
		t.Fatal("a failing destination reported success")
	}
	if !sourceStillHasState(t, dir, "dev") {
		t.Fatal("the source lost state when the destination failed")
	}
}

// No migrate_from: is not a migration, and the error should say what to add
// rather than reporting an internal condition.
func TestMigrateWithNoMigrateFromSaysWhatToAdd(t *testing.T) {
	dir := newProjectFixture(t)

	_, stderr, code := runCommand(t, dir, "state", "migrate")

	if code == ExitOK {
		t.Fatal("migrate succeeded with nothing to migrate from")
	}
	if !strings.Contains(stderr, "migrate_from") {
		t.Errorf("error does not name the block to add:\n%s", stderr)
	}
}
```

Build the fixtures on `internal/cli`'s existing helpers — `newProjectFixture`, `runCommand` — and on `backendhost`'s test backend for the fake remote. Read `internal/backendhost/host_test.go` for how it builds one rather than inventing a second approach.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestMigrate|TestForceOverwrites|TestAFailedMigration' ./internal/cli/ -v`
Expected: FAIL — unknown subcommand "migrate".

- [ ] **Step 3: Write minimal implementation**

Order, and each step is deliberate:

```go
// migrate moves state from the backend `migrate_from:` names to the one
// `backend:` names.
//
// Both ends are LOCKED for the whole operation. Both are compared BEFORE
// anything is written, because the interesting decision is which of three
// situations this is, and discovering it halfway through a copy is too late:
//
//	source has state, destination empty      -> migrate
//	source has state, destination identical  -> no-op, succeed
//	source has state, destination differs    -> refuse
//
// The middle case is what makes a re-run safe, and it is not a nicety: a
// migration performed through CI gets re-run, and a failure there invites
// somebody to add --force to the workflow, where it stays forever.
//
// Every environment is verified by reading it back from the destination. A
// backend that accepted a write and cannot return it is a backend that has
// lost state, and the whole point of this command is to not do that.
//
// ANY FAILURE LEAVES THE SOURCE AUTHORITATIVE. Nothing is removed from the
// source at all -- migration COPIES. Emptying the source is the user's
// decision, made by pointing the configuration somewhere else, not this
// command's to make.
```

Compare with `bytes.Equal` over the encoded state, not field by field — a backend stores bytes and this command must not start interpreting them.

Report per environment, then the outcome, then the suggestion to remove `migrate_from:` **and that leaving it is harmless**.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli
git commit -m "Move state between two backends

Both ends are locked and compared before anything is written. An
identical destination is a no-op rather than a failure, because a
migration run from a pipeline gets re-run and failing there invites
somebody to put a force flag in the workflow forever. Nothing is removed
from the source."
```

---

## Task 4: Refuse to run into a backend awaiting migration

**This is the task that stops the design destroying someone's infrastructure.** Read spec §7's "But ignoring it entirely recreates your infrastructure" before starting.

**Files:**
- Modify: `internal/cli/context.go` (`backendFor`)
- Test: `internal/cli/backend_test.go`

**Interfaces:**
- Consumes: Task 1's `MigrateFrom`, Task 2's `openBackend`.
- Produces: no new exported symbols; `backendFor` gains the guard.

- [ ] **Step 1: Write the failing test**

```go
// THE HAZARD. Configuration lands with a new empty backend and a
// migrate_from: naming the old one that holds everything. Nobody has run the
// migration yet. Reading only `backend:`, every resource looks unmanaged and
// apply would CREATE ALL OF IT AGAIN.
//
// In the CI flow this design exists for, that ordering is the LIKELY one:
// config lands first, the pipeline runs before a human triggers anything.
func TestCommandsRefuseWhileAMigrationIsPending(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev") // the SOURCE holds state; the destination is empty

	for _, args := range [][]string{{"plan", "dev"}, {"apply", "dev", "--auto-approve"}, {"refresh", "dev"}} {
		_, stderr, code := runCommand(t, dir, args...)

		if code == ExitOK || code == ExitChanges {
			t.Errorf("%v proceeded into a backend awaiting migration", args)
		}
		if !strings.Contains(stderr, "state migrate") {
			t.Errorf("%v: refusal does not name the fix:\n%s", args, stderr)
		}
	}
}

// Once migrated, the block is harmless and everything proceeds. This is the
// property the whole inertness rule exists to protect: a SUCCESSFUL migration
// must not break the pipeline while the removal commit is pending.
func TestCommandsProceedOnceTheMigrationIsDone(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	if _, _, code := runCommand(t, dir, "state", "migrate"); code != ExitOK {
		t.Fatal("setup migration failed")
	}

	// migrate_from: is STILL present and must now be inert.
	if _, stderr, code := runCommand(t, dir, "plan", "dev"); code == ExitError {
		t.Errorf("plan refused after a completed migration:\n%s", stderr)
	}
}

// A new project that has both blocks but nothing anywhere is not a pending
// migration. Refusing here would block a legitimate first apply.
func TestAnEmptySourceIsNotAPendingMigration(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t) // nothing seeded anywhere

	if _, stderr, code := runCommand(t, dir, "plan", "dev"); code == ExitError {
		t.Errorf("plan refused with nothing to migrate:\n%s", stderr)
	}
}

// The guard must not cost anything in the ordinary case. With state at the
// destination the source is never opened, which matters when opening it means
// starting a plugin process and talking to a network.
func TestTheSourceIsNotOpenedWhenTheDestinationHasState(t *testing.T) {
	dir := newProjectMigratingLocalToCountingFake(t) // counts opens of the SOURCE
	seedLocalState(t, dir, "dev")
	runCommand(t, dir, "state", "migrate")

	resetSourceOpenCount(t, dir)
	runCommand(t, dir, "plan", "dev")

	if n := sourceOpenCount(t, dir); n != 0 {
		t.Errorf("the source backend was opened %d times when the destination already had state", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestCommandsRefuseWhile|TestCommandsProceedOnce|TestAnEmptySource|TestTheSourceIsNotOpened' ./internal/cli/ -v`
Expected: FAIL — commands proceed into the empty backend.

- [ ] **Step 3: Write minimal implementation**

In `backendFor`, after opening the configured backend and only when `MigrateFrom.Plugin` is set:

```go
// Check the DESTINATION first. State there means the migration is done and
// the source need never be opened -- which matters, because opening it may
// start a plugin process and reach a network. Only an empty destination pays
// for a second open, and an empty destination is already unusual.
```

Destination non-empty → proceed. Destination empty → open the source; if it holds state, refuse, naming the environments at risk and telling the user to run `state migrate` or remove the block. Source also empty → proceed.

The refusal is a §44 diagnostic: what is wrong (this backend is waiting for a migration), what was expected, and the action.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli
git commit -m "Refuse to run into a backend that is waiting for a migration

Between the config landing and the migration being run, the new backend
is empty, so every resource looks unmanaged and an apply would create all
of it again. The destination is checked first, so the ordinary case costs
nothing."
```

---

## Task 5: The real round trip

**MANDATORY, and not satisfiable with a fake.** The spec asks for local → S3 → local specifically because it proves the two backends agree about what state IS, rather than proving one can read its own writing.

**Files:**
- Create: an `e2e`-style test, or extend `tests/integration` if it can reach a real backend

- [ ] **Step 1: Stand up MinIO**

`quay.io/minio/minio:latest` run as a container with `server /data` — **not** as a CI service container, which cannot pass that argument. `REQUIRE_LIVE_STORE=1` turns a skip into a failure.

- [ ] **Step 2: The round trip**

Build infrena and `infrena-backend-s3`. Then:

1. A project with local state and two environments, each holding real resources after an `apply`.
2. Add `backend:` naming the S3 backend and `migrate_from: {plugin: local}`. Run `state migrate`.
3. Assert the state objects exist **in the bucket, read directly**, not through infrena.
4. `infrena plan` against both environments is CLEAN — the migrated state is understood, not merely stored.
5. Swap the blocks: `backend: {plugin: local}`, `migrate_from:` naming S3. Run `state migrate` again.
6. Assert the local state files are **byte-identical to what step 1 produced**.

**Step 6 is the point of the whole test.** Byte-identical after a round trip through a different backend proves the two agree about what state is. Anything less proves only that each can read its own writing.

- [ ] **Step 3: Report what you saw**, verbatim, including anything that differed. A difference is a finding.

- [ ] **Step 4: Commit**

```bash
git commit -m "Prove state survives a round trip through another backend

Byte identical local state after going out to a bucket and back is what
proves two backends agree about what state is. Either one alone only
proves it can read its own writing."
```

---

## Task 6: `--check`, for pipelines

**Files:**
- Modify: `internal/cli/state_migrate.go`, `internal/cli/root.go`, `pkg/report/report.go`
- Test: `internal/cli/state_migrate_test.go`

**Interfaces:**
- Consumes: Task 3's three-way comparison.
- Produces: `--check` on `state migrate`; `ExitMigrationPending = 2`, `ExitMigrationComplete = 3`, `ExitMigrationConflict = 4`; `report.MigrateResult{Type, Status string, Environments []string, Error string}` and `(*report.Writer).WriteMigrateResult`.

- [ ] **Step 1: Write the failing test**

```go
// The exit code IS the report. A pipeline branches on it without parsing
// anything, which is the whole point of the flag.
func TestCheckReportsEachSituationByExitCode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) string
		want  int
	}{
		{"no migrate_from block", func(t *testing.T) string { return newProjectFixture(t) }, ExitOK},
		{"migration pending", func(t *testing.T) string {
			dir := newProjectMigratingLocalToFake(t)
			seedLocalState(t, dir, "dev")
			return dir
		}, ExitMigrationPending},
		{"already complete", func(t *testing.T) string {
			dir := newProjectMigratingLocalToFake(t)
			seedLocalState(t, dir, "dev")
			runCommand(t, dir, "state", "migrate")
			return dir
		}, ExitMigrationComplete},
		{"ends differ", func(t *testing.T) string {
			dir := newProjectMigratingLocalToFake(t)
			seedLocalState(t, dir, "dev")
			seedDestinationState(t, dir, "dev", "something else entirely")
			return dir
		}, ExitMigrationConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			_, _, code := runCommand(t, dir, "state", "migrate", "--check")
			if code != tc.want {
				t.Errorf("exit = %d, want %d", code, tc.want)
			}
		})
	}
}

// --check must be genuinely read-only. A check that migrated would be the
// worst possible surprise in a pipeline: the thing you ran to find out
// whether to act would have acted.
func TestCheckWritesNothingAndTakesNoLock(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")

	if _, _, code := runCommand(t, dir, "state", "migrate", "--check"); code != ExitMigrationPending {
		t.Fatalf("exit = %d", code)
	}

	if destinationHasState(t, dir, "dev") {
		t.Error("--check wrote to the destination")
	}
	// A lock left behind would block the migration the check just recommended.
	if lockHeld(t, dir, "dev") {
		t.Error("--check left a lock behind")
	}
}

// The status crosses as a STRING, so a consumer never maps an exit code back
// to a meaning.
func TestCheckWritesTheStatusAsAStringInTheReport(t *testing.T) {
	dir := newProjectMigratingLocalToFake(t)
	seedLocalState(t, dir, "dev")
	out := filepath.Join(t.TempDir(), "check.ndjson")

	stdout, _, _ := runCommand(t, dir, "state", "migrate", "--check", "--output", out)

	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status":"pending"`) {
		t.Errorf("report does not carry the status as a string:\n%s", body)
	}
	if !strings.Contains(string(body), "dev") {
		t.Errorf("report does not name the environments:\n%s", body)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestCheck' ./internal/cli/ -v`
Expected: FAIL — no `--check` flag, exit codes undefined.

- [ ] **Step 3: Write minimal implementation**

Add the three codes to `root.go` beside the existing ones, each with a comment saying what a pipeline does with it. `report.Version` bumps for the new result line.

```go
	// ExitMigrationPending is 2 DELIBERATELY, the same code `plan` uses for
	// "changes present". Not a collision: both mean the same thing to a
	// pipeline -- something is pending, run the corresponding command.
	ExitMigrationPending = 2
	// ExitMigrationComplete: both ends agree, so there is nothing to do and
	// the `migrate_from:` block is stale. Distinct from 0 so a pipeline can
	// remind somebody to remove it.
	ExitMigrationComplete = 3
	// ExitMigrationConflict: both ends hold DIFFERENT state, so somebody has
	// been applying to one of them. Distinct from 1 for the reason 77 is: a
	// pipeline that cannot tell "this failed" from "this needs a person" will
	// treat both the same, and they want opposite responses.
	ExitMigrationConflict = 4
```

`--check` shares Task 3's comparison — **the same function, not a second copy.** Two implementations of "are these the same" is how `--check` comes to disagree with what `migrate` then does.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli pkg/report
git commit -m "Let a pipeline ask whether a migration is needed

The exit code is the answer, so a script branches without parsing
anything, and the machine readable output carries the status as a word
rather than a number. It writes nothing and takes no lock, because a
check that acted would be the worst surprise in a pipeline."
```

---

## Task 7: Documentation

**Files:**
- Modify: `PLAN.md` (§52 build order, §37 command list), `CLAUDE.md`, `docs/state-backends.md`

- [ ] **Step 1: Mark step 3 shipped in `PLAN.md` §52** and add `state migrate` to §37's command list, with `--check`.

- [ ] **Step 1b: Add exit codes 3 and 4 to §16's table**, alongside 0, 1, 2 and 77, noting that 2 is shared with `plan` deliberately.

- [ ] **Step 2: `docs/state-backends.md`** gains how to migrate: both blocks, the two-commit CI shape, that `migrate_from:` is inert elsewhere so leaving it is harmless, and the three cases.

- [ ] **Step 3: `CLAUDE.md`** — `migrate_from:` is inert except to one command and why (a successful CI migration would otherwise break the pipeline), the three-case decision and why the middle case exists, and that migration copies rather than moves.

- [ ] **Step 4: Verify**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... && go vet ./... && gofmt -l .`

- [ ] **Step 5: Commit**

```bash
git add PLAN.md CLAUDE.md docs/state-backends.md
git commit -m "Document moving state between backends"
```
