# Remote State Step 1 — The Backend Boundary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a state backend something a plugin can be, with local still built in and nothing a user can see changing.

**Architecture:** `state.Backend` already exists and already carries `Get`, `Put`, `Lock` and `Unlock`; the executor already depends on it. This widens it by the three methods the CLI calls on the concrete type, moves the public types to `pkg/backend` so a third-party author can import them, defines the wire protocol and the host that speaks it, and re-points `backendFor` so a project naming a backend gets a plugin instead of local. The proof the extraction is faithful is that the entire existing suite still passes untouched.

**Tech Stack:** Go 1.27, Cobra, `gopkg.in/yaml.v3`. Standard library only for the new packages.

**Spec:** `docs/superpowers/specs/2026-09-16-remote-state-design.md`

## Global Constraints

- Go 1.27.0 module floor. `mise` is not active in non-interactive shells — use `make` targets or `export PATH="$HOME/.local/share/mise/shims:$PATH"`.
- **Third-party budget is two packages**: `github.com/spf13/cobra`, `gopkg.in/yaml.v3`. Everything new here is standard library.
- **Local is built in and is NOT a backend plugin.** It is the bootstrap that works before anything is installed. A shipped infrena carries no *remote* backend.
- **Every backend must lock.** A backend that does not implement locking is refused **when it loads**, never at apply time. Invariant 5 — two applies cannot mutate one environment concurrently — is absolute.
- **`state:` may not interpolate variables, at all.** You need state before you can compile, and compiling is what resolves variables. A `${...}` there is a diagnostic explaining the cycle, never a feature to add.
- **A backend stores bytes and does not interpret them.** `state.State` does not move to `pkg/`; a backend that parsed state would be a second reader free to disagree with the first.
- **`Backend.Put` is called once per operation and writes the whole state.** The envelope must carry state as raw bytes, never JSON nested inside JSON.
- **`backendproto.Version` is its own format version** (§61's seventh), independent of `pluginproto.Version`.
- Errors follow §44: what is wrong, where, what was expected, an action the user can take.
- `gofmt -l .` and `go vet ./...` clean; full `go test -count=1 ./...` green with `INFRENA_REQUIRE_PLUGIN=1`.
- Commit messages: plain English, no em-dashes, minimal, **never** mention Claude, AI or the model. Each ends with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

---

## File Structure

| File | Responsibility |
|---|---|
| `pkg/backend/backend.go` *(new)* | `Lock`, `ErrLocked`, `ErrNotLocked` — the public types a backend author imports. |
| `internal/state/backend.go` | `Backend` widened; the moved types re-exported as aliases so nothing else churns. |
| `pkg/backendproto/proto.go` *(new)* | The wire contract and `Version`. |
| `pkg/backendsdk/serve.go` *(new)* | The whole of a backend's `main()`. |
| `internal/backendhost/host.go` *(new)* | The host side: launch, handshake, dispatch. |
| `internal/config/declarations.go` | `StateDecl` — the decoded `state:` block. |
| `internal/config/decode_state.go` *(new)* | Decoding it, and refusing interpolation. |
| `internal/cli/context.go` | `backendFor` returns `state.Backend` and routes. |

---

## Task 1: Widen the interface, move the public types

**Files:**
- Create: `pkg/backend/backend.go`
- Modify: `internal/state/backend.go`, `internal/state/lock.go`, `internal/state/local.go`, callers in `internal/cli`
- Test: `internal/state/backend_test.go`

**Interfaces:**
- Produces:
  - `backend.Lock{Environment, PID, Host, User, Operation string/int/time.Time}`, `backend.ErrLocked`, `backend.ErrNotLocked`
  - `state.Lock = backend.Lock` (alias), `state.ErrLocked`, `state.ErrNotLocked` (re-exported vars)
  - `state.Backend` gaining `List() ([]string, error)`, `Inspect(ctx, environment) (Lock, bool, error)`, `ForceUnlock(ctx, environment) error`

- [ ] **Step 1: Write the failing test**

```go
// The CLI calls List, Inspect and ForceUnlock on the concrete *Local. Until
// they are on the interface, backendFor cannot return a Backend, and a plugin
// can never stand in for local. This is the whole of what step 1 unblocks.
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestLocalSatisfies|TestInspectAndForceUnlock|TestStateLockIsTheSame' ./internal/state/ -v`
Expected: FAIL — `*Local` does not satisfy `Backend` (three methods missing), `Inspect` takes no context, `backend` package does not exist.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/backend/backend.go` holding `Lock`, `ErrLocked` and `ErrNotLocked`, moved verbatim with their doc comments. Package doc:

```go
// Package backend is the public contract a state backend implements.
//
// It holds the types a third-party backend author must be able to import, and
// nothing else. `state.State` is deliberately ABSENT: a backend stores bytes
// and never parses them (spec §6), so a backend needs no state type at all.
// One that parsed state would be a second reader, free to disagree with the
// first about what a state file means.
//
// Lock's fields are public contract rather than an implementation detail,
// because they are what the stale-lock diagnostic already prints: a user who
// sees "held by alice on host-3, pid 4211" is reading these.
package backend
```

In `internal/state/backend.go`, alias rather than duplicate:

```go
// Lock is an alias, not a copy. Two structs would be two things to keep in
// step, and the first drift would be a lock a backend can build and the
// engine cannot read.
type Lock = backend.Lock

var (
	ErrLocked    = backend.ErrLocked
	ErrNotLocked = backend.ErrNotLocked
)
```

Add the three methods to the `Backend` interface, with doc comments saying what each demands of an implementation. Change `Local.Inspect` and `Local.ForceUnlock` to take a `context.Context` (unused by local, and say so in a comment rather than leaving a reader wondering). Update every caller in `internal/cli` — `grep -rn "\.Inspect(\|\.ForceUnlock(" internal/` finds them.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS. **The entire existing suite passing untouched is the proof this extraction was faithful** — if a behavioural test changed, something moved that should not have.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add pkg/backend internal/state internal/cli
git commit -m "Make the backend interface something a plugin can satisfy

It carried four of the seven methods the command line calls, so the
constructor still handed back a concrete local backend and nothing else
could stand in. The lock type moves to a public package because an author
cannot implement locking against a type they cannot import."
```

---

## Task 2: The wire contract

**Files:**
- Create: `pkg/backendproto/proto.go`, `pkg/backendproto/proto_test.go`

**Interfaces:**
- Consumes: Task 1's `backend.Lock`.
- Produces:
  - `const Version = 1`, `var Supported = []int{1}`
  - Request/response types for `get`, `put`, `list`, `lock`, `unlock`, `inspect`, `force_unlock`, and `configure`
  - `type PutRequest struct { Environment string; State []byte }` — **raw bytes**

- [ ] **Step 1: Write the failing test**

```go
// State crosses this wire once per operation during an apply, and the local
// backend already serialises the whole state every time. Carrying it as a
// nested JSON object or a base64 string would add a second full encode to
// every operation. Raw bytes, once.
func TestStateCrossesTheWireAsRawBytesNotNestedJSON(t *testing.T) {
	raw := []byte(`{"version":1,"serial":7,"resources":{}}`)
	data, err := json.Marshal(PutRequest{Environment: "dev", State: raw})
	if err != nil {
		t.Fatal(err)
	}

	var got PutRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.State, raw) {
		t.Errorf("State = %q, want %q", got.State, raw)
	}
	// encoding/json renders []byte as base64, which is ONE encode and is what
	// we accept. What must not appear is the state's own field names, which
	// would mean it had been re-marshalled as a structure.
	if bytes.Contains(data, []byte(`"serial"`)) {
		t.Errorf("state was re-encoded as JSON structure, not carried as bytes:\n%s", data)
	}
}

// Section 61: every boundary carries exactly one version, and this one is
// independent of the provider protocol. A state format change must not force
// a provider release, and vice versa.
func TestVersionIsIndependentOfThePluginProtocol(t *testing.T) {
	if Version != 1 {
		t.Errorf("Version = %d, want 1", Version)
	}
	if !slices.Contains(Supported, Version) {
		t.Error("Supported does not include Version")
	}
}

// A lock crosses the wire whole, so a conflict can name its holder.
func TestALockRoundTripsAcrossTheWire(t *testing.T) {
	want := backend.Lock{
		Environment: "production", PID: 4211, Host: "host-3",
		User: "alice", Operation: "apply", At: time.Unix(1700000000, 0).UTC(),
	}
	data, err := json.Marshal(LockResponse{Held: want})
	if err != nil {
		t.Fatal(err)
	}
	var got LockResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Held.At.Equal(want.At) || got.Held.User != want.User || got.Held.PID != want.PID {
		t.Errorf("Held = %+v, want %+v", got.Held, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/backendproto/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

Read `pkg/pluginproto/proto.go` first and mirror its shape — request/response envelopes, the error shape, how the handshake is expressed. Two protocols that look different for no reason cost a reader twice.

```go
// Package backendproto is the wire contract between infrena and a state
// backend plugin.
//
// SEPARATE FROM pluginproto DELIBERATELY. The two evolve for unrelated
// reasons: a change to how state is stored must not force every provider to
// cut a release, and a change to how resources are described must not force
// every backend to. PLAN.md §61 gives each boundary exactly one version, and
// this is the seventh.
//
// The transport is shared — newline-delimited JSON over stdio, the same
// handshake and the same cookie — because that part has no reason to differ
// and a second transport would be a second thing to get wrong.
package backendproto
```

Define `configure` carrying the project's literal `state.config` map, and the seven operations. Every response carries an error shape distinguishing a lock conflict from a transport failure, since `ErrLocked` is tested for by callers.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/backendproto/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./pkg/...
git add pkg/backendproto
git commit -m "Define the wire contract for a state backend

State crosses it as raw bytes, because it is written once per operation
during an apply and a nested encoding would pay for itself every time.
The version is its own, so a storage change does not force a provider
release."
```

---

## Task 3: The SDK a backend author writes against

**Files:**
- Create: `pkg/backendsdk/serve.go`, `pkg/backendsdk/serve_test.go`

**Interfaces:**
- Consumes: Tasks 1 and 2.
- Produces: `func Serve(b backend.Backend)` — the whole of a backend plugin's `main()`.

**Note:** `backend.Backend` here is the *plugin-side* interface, which is `state.Backend` minus nothing — the same seven methods. Define it in `pkg/backend` in this task so an author has one import.

- [ ] **Step 1: Write the failing test**

```go
// pkg/pluginsdk is "the whole of a plugin's main()". This is the same promise
// for a backend: an author writes seven methods and one call.
func TestServeAnswersEveryOperationOverThePipe(t *testing.T) {
	in, out := pipePair(t)
	go Serve(&memoryBackend{}, in.reader, out.writer)

	handshake(t, in, out)

	if got := call(t, in, out, "lock", `{"environment":"dev"}`); !strings.Contains(got, `"user"`) {
		t.Errorf("lock response carries no holder: %s", got)
	}
	call(t, in, out, "put", `{"environment":"dev","state":"eyJ2ZXJzaW9uIjoxfQ=="}`)
	if got := call(t, in, out, "get", `{"environment":"dev"}`); !strings.Contains(got, "eyJ2ZXJzaW9uIjoxfQ==") {
		t.Errorf("get did not return what put stored: %s", got)
	}
	call(t, in, out, "unlock", `{"environment":"dev"}`)
}

// THE RULE THE PROTOCOL EXISTS TO ENFORCE. A backend that does not lock is
// refused when it loads, not when an apply reaches for it, so the SDK must
// make an unlocking backend impossible to write rather than merely
// discouraged.
func TestABackendThatCannotLockCannotBeServed(t *testing.T) {
	// memoryBackend implements all seven. A type missing Lock must not
	// compile, which is asserted by the interface itself rather than at
	// runtime — this test documents the intent and pins the interface shape.
	var _ backend.Backend = (*memoryBackend)(nil)
}
```

Write `memoryBackend`, `pipePair`, `handshake` and `call` in the test file. `memoryBackend` is a map guarded by a mutex — the reference implementation an author reads.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/backendsdk/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

Read `pkg/pluginsdk/serve.go` and follow it closely. Same loop shape, same error handling, same cookie check. Where they must differ, say why in a comment.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/... -v 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./pkg/...
git add pkg/backendsdk pkg/backend
git commit -m "Give a backend author the whole of their main function

Seven methods and one call, matching what a provider author already gets.
Locking is part of the interface rather than an option, so a backend that
cannot lock does not compile."
```

---

## Task 4: The host

**Files:**
- Create: `internal/backendhost/host.go`, `internal/backendhost/host_test.go`

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: `func Open(ctx, name string, dirs []string, config map[string]any) (state.Backend, func() error, error)`

- [ ] **Step 1: Write the failing test**

```go
// The host presents a plugin as an ordinary Backend, so nothing upstream
// learns that state became remote.
func TestOpenPresentsAPluginAsAnOrdinaryBackend(t *testing.T) {
	dir := buildFakeBackend(t) // compiles a tiny backend plugin from testdata

	b, closeFn, err := Open(context.Background(), "memory", []string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	st := state.New("p", "dev")
	if err := b.Put(ctx, "dev", st); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if got.Environment != "dev" {
		t.Errorf("Environment = %q, want dev", got.Environment)
	}
}

// A lock conflict must survive the wire as ErrLocked, or every caller that
// tests for it silently stops working when state goes remote.
func TestALockConflictArrivesAsErrLocked(t *testing.T) {
	dir := buildFakeBackend(t)
	b, closeFn, _ := Open(context.Background(), "memory", []string{dir}, nil)
	defer closeFn()
	ctx := context.Background()

	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	_, err := b.Lock(ctx, "dev")
	if !errors.Is(err, state.ErrLocked) {
		t.Fatalf("second Lock returned %v, want ErrLocked", err)
	}
}

// A plugin that dies mid-run must produce an error naming the backend, not a
// nil dereference. The executor writes state once per operation, so this is
// the failure mode that matters most.
func TestABackendThatDiesMidRunErrorsClearly(t *testing.T) {
	dir := buildFakeBackend(t)
	b, closeFn, _ := Open(context.Background(), "crash-on-put", []string{dir}, nil)
	defer closeFn()

	err := b.Put(context.Background(), "dev", state.New("p", "dev"))
	if err == nil {
		t.Fatal("a dead backend accepted a write")
	}
	if !strings.Contains(err.Error(), "crash-on-put") {
		t.Errorf("error does not name the backend: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/backendhost/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

Read `internal/pluginhost/` — `loader.go`, `connect.go`, `client.go` — and follow its structure. Reuse the search path: a backend plugin lives in the same directories `pluginhost.DefaultSearch` already returns, because §31.3 says install populates the directories that exist rather than adding a second mechanism.

The adapter converts a protocol error back into `state.ErrLocked` where the response says lock conflict, so `errors.Is` keeps working for every existing caller.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/backendhost
git commit -m "Run a state backend as a separate process

The host presents it as an ordinary backend, so nothing upstream learns
that state became remote. A lock conflict survives the wire as the same
error callers already test for."
```

---

## Task 5: `state:` in configuration

**Files:**
- Modify: `internal/config/declarations.go`
- Create: `internal/config/decode_state.go`
- Test: `internal/config/decode_state_test.go`

**Interfaces:**
- Produces: `config.StateDecl{Backend string; Config map[string]any; Origin value.Origin}`, on `ProjectDecl.State`.

- [ ] **Step 1: Write the failing test**

```go
func TestStateBlockDecodes(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
state:
  backend: s3
  config:
    bucket: acme-infra
    region: us-east-1
`,
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if p.State.Backend != "s3" {
		t.Errorf("Backend = %q, want s3", p.State.Backend)
	}
	if p.State.Config["bucket"] != "acme-infra" {
		t.Errorf("Config = %v", p.State.Config)
	}
}

// No state block means local, which is what every project does today.
func TestNoStateBlockMeansLocal(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{ProjectFileName: projectWithNoResources})
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	if p.State.Backend != "" {
		t.Errorf("Backend = %q, want empty", p.State.Backend)
	}
}

// THE CYCLE. You need state before you can compile, and compiling is what
// resolves variables, so a variable here can never be resolved. The
// diagnostic must explain that rather than saying "unknown variable", which
// would send a reader off to declare one.
func TestAVariableInTheStateBlockIsRefusedWithTheReason(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "state:\n  backend: s3\n  config:\n    bucket: ${var.bucket}\n",
	})
	if !ds.HasErrors() {
		t.Fatal("a variable in the state block was accepted")
	}
	msg := ds.Error()
	if !strings.Contains(msg, "before") {
		t.Errorf("diagnostic does not explain the ordering: %s", msg)
	}
	if !strings.Contains(msg, "bucket") {
		t.Errorf("diagnostic does not name the key: %s", msg)
	}
}

// Fails closed on unknown keys, like every other block.
func TestAnUnknownKeyInTheStateBlockIsAnError(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "state:\n  bakend: s3\n",
	})
	if !ds.HasErrors() {
		t.Fatal("an unknown key was accepted")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestStateBlock|TestNoStateBlock|TestAVariableInTheState|TestAnUnknownKeyInTheState' ./internal/config/ -v`
Expected: FAIL — `ProjectDecl` has no `State`.

- [ ] **Step 3: Write minimal implementation**

Decode with `yaml.Node` like every other block in this package. Scan every string value in `config` for `${` and refuse it, naming the key:

```go
// A variable here can never be resolved, and the diagnostic has to say why or
// the reader goes and declares one.
//
// `providers:` MAY interpolate, because compiler stage 4.5 constructs provider
// instances after variables resolve. State cannot: state is read BEFORE any
// compile — `destroy`, `refresh`, `discover` and `import` never compile at all
// — so there is no point at which a value here could be filled in. It is a
// cycle, not a missing feature, and a future request to "just support
// variables in state:" is a request to break the ordering.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/config
git commit -m "Read the state block, and refuse a variable in it

A value here can never be resolved, because state is read before any
compile and compiling is what resolves variables. The message says that
rather than reporting an undeclared variable."
```

---

## Task 6: Route to the plugin, and keep local the bootstrap

**Files:**
- Modify: `internal/cli/context.go` (`backendFor`, line 294)
- Test: `internal/cli/backend_test.go` *(new)*

**Interfaces:**
- Consumes: Tasks 1, 4, 5.
- Produces: `func backendFor(ctx context.Context, dir string) (state.Backend, func() error, error)`

- [ ] **Step 1: Write the failing test**

```go
// No state block: local, exactly as today, with nothing installed and no
// process started. This is the bootstrap and it must never need a plugin.
func TestAProjectWithNoStateBlockUsesLocalAndStartsNothing(t *testing.T) {
	dir := newProjectFixture(t)
	blocked := blockNetwork(t)

	b, closeFn, err := backendFor(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	if _, ok := b.(*state.Local); !ok {
		t.Errorf("backendFor returned %T, want *state.Local", b)
	}
	if blocked.Attempts() != 0 {
		t.Error("the local backend reached the network")
	}
}

// A backend named but not installed is an error naming the backend and how to
// get it, not a silent fallback to local. Falling back would write state
// somewhere the user did not ask for, which is the worst available outcome.
func TestAMissingBackendPluginIsAnErrorAndNeverFallsBackToLocal(t *testing.T) {
	dir := newProjectWithStateBackend(t, "s3")

	_, _, err := backendFor(context.Background(), dir)
	if err == nil {
		t.Fatal("a missing backend silently fell back")
	}
	for _, want := range []string{"s3", "plugins install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestAProjectWithNoStateBlock|TestAMissingBackendPlugin' ./internal/cli/ -v`
Expected: FAIL — `backendFor` takes one argument and returns one value.

- [ ] **Step 3: Write minimal implementation**

Decode `infra.yml` for the `state:` block — decode only, never compile. Empty backend gives `state.NewLocal` and a no-op closer. Otherwise `backendhost.Open`. Update all eight call sites; each must defer the closer.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS — the whole existing suite included, since no project in it declares a backend.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli
git commit -m "Send a project to the backend it names

A project with no state block still gets the local backend and starts no
process. One that names a backend it has not installed is told so, and
never quietly falls back to writing state somewhere else."
```

---

## Task 7: Documentation

**Files:**
- Modify: `PLAN.md` (§52, §31.2, §61), `CLAUDE.md`
- Create: `docs/state-backends.md`

- [ ] **Step 1: Replace `PLAN.md` §52's six bullets**

with the design's shape: backends are plugins, local is the bootstrap, every backend must lock, `state:` is literal-only. Mark step 1 shipped and steps 2–4 as designs.

- [ ] **Step 2: Update §31.2 and §61**

`plugin.yaml` gains `kind: provider | backend`, defaulting to `provider`. `backendproto.Version` joins §61's table as the seventh format.

- [ ] **Step 3: Write `docs/state-backends.md`**

The trust boundary, stated plainly: **state reaches a backend in cleartext, exactly as values already reach provider plugins.** A backend plugin can read every secret in your infrastructure; so can a provider. The mitigation is the same — you choose which plugins to trust, and `plugins.lock` records the choice. Recommend server-side encryption on the store. Say client-side encryption is deferred because it needs a key story before it needs code.

- [ ] **Step 4: Update `CLAUDE.md`**

Local is the bootstrap and is built in; every remote backend is a plugin; a shipped infrena carries no remote backend; `state:` never interpolates and why; `Backend.Put` is per-operation and carries raw bytes.

- [ ] **Step 5: Verify and commit**

```bash
INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... && go vet ./... && gofmt -l .
git add PLAN.md CLAUDE.md docs/state-backends.md
git commit -m "Document the state backend boundary and what a backend can see"
```
