# Remote State Step 2 — The S3-Compatible Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A state backend that works against any S3-compatible object store, locks safely, and refuses rather than pretending when a store cannot lock.

**Store compatibility, established by James on 2026-09-17** — this is data, not something to rediscover:

| Store | Conditional writes |
| --- | --- |
| AWS S3 | yes |
| Cloudflare R2 | yes |
| DigitalOcean Spaces | yes |
| MinIO | yes |
| **Backblaze B2** | **NO** |
| Wasabi | unverified |

**HOW B2 fails, measured against the real service on 2026-09-17** (endpoint
`s3.us-west-000.backblazeb2.com`), because this corrects an assumption the first draft of this
plan was built on:

```
PUT (If-None-Match: *) -> A header you provided implies functionality that is not implemented
                          error code: "NotImplemented"
```

**B2 refuses the FIRST conditional write, loudly and immediately.** It does NOT silently
overwrite. The first draft assumed the dangerous case — a store that ignores the header and
reports success — and designed the probe around detecting that. Measuring found the friendlier
behaviour instead.

**Keep the two-write probe anyway.** It now covers two distinct failure modes rather than one:

| Store behaviour | What the probe sees | Verdict |
| --- | --- | --- |
| Refuses write #1 with `NotImplemented` | error on the first write | refuse — this is B2 |
| Accepts both writes | no error on either | refuse — the header was ignored, a lock here never locks |
| Accepts #1, refuses #2 with `PreconditionFailed` | the expected refusal | accept |

No store is currently known to do the middle one, but the probe costs one extra round trip at
configure time and it is the case that fails silently, so it stays.

**The refusal message should quote the store's own error.** B2's is genuinely informative —
better than any generic "your store does not support locking" this plugin could invent.

**Architecture:** A new repository, `infrena-backend-s3`, built the way `infrena-provider-aws` is built: its own module, its own dependency budget, compiled against a local infrena through a gitignored `go.work`. It implements `backend.Backend` from `pkg/backend` and is served by `pkg/backendsdk`. Locking is a conditional write (`If-None-Match: *`), and support for that is **proven at configure time, never assumed** — measured behaviour above, not documentation.

**Tech Stack:** Go 1.27, `github.com/minio/minio-go/v7`, `github.com/infrena/infrena` (for `pkg/backend`, `pkg/backendsdk`, `pkg/pluginmanifest`). Tests use MinIO in a container.

**Spec:** `docs/superpowers/specs/2026-09-16-remote-state-design.md` (§9 step 2)

## Global Constraints

- **This is a SEPARATE REPOSITORY**, `~/projects/infrena-backend-s3`. Do not add anything to `~/projects/infrena`. Follow `~/projects/infrena-provider-aws`'s layout, `go.work` arrangement and CI shape — read it before starting rather than inventing a second pattern.
- Go 1.27.0 floor, matching infrena's. The plugin repo has its own dependency budget; infrena's two-package limit does not apply here and does not license carelessness either.
- **Every backend must lock** (spec §5). A store that cannot do a compare-and-swap is refused, by name, saying what it lacks. There is no unsafe fallback and no opt-out.
- **Conditional-write support is PROVEN, not assumed.** See Task 3 and the measured B2 result above. A store may refuse the header loudly (B2) or ignore it silently; the probe catches both, and a store's documentation catches neither.
- **A backend stores bytes and does not interpret them.** State arrives as `[]byte` and is written unchanged. Never parse it.
- **`plugin:` is the only key infrena reserves.** Every other key in the `backend:` block arrives in `configure` and is this plugin's to define.
- **A profile name is configuration; an access key is a secret.** Accept `profile:`, read credentials from the environment, a credentials file or an instance role. Never accept an access key or secret in the `backend:` block, and say so if one is passed.
- Binary is `infrena-backend-s3`; repository is `infrena-backend-s3`; `plugin.yaml` names it `s3`. §31.3's convention.
- `gofmt -l .`, `go vet ./...` clean; `go test ./...` green.
- Commit messages: plain English, no em-dashes, minimal, **never** mention Claude, AI or the model. Each ends with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

**THE LESSON THIS PLAN IS BUILT AROUND.** Three times in the last two days, a fully green suite hid a bug that one run against a real service found immediately: five plugin-search bugs against a fake GitHub, an extractor that rejected every real archive, and an RDS engine version that no fake could have produced. **A test against a double proves your code does what you think, not that the store agrees.** Task 6 runs the whole backend against a real MinIO, and Task 8 against at least one non-MinIO store. Neither is optional and no task may be reported complete on unit tests alone.

---

## File Structure

| File | Responsibility |
|---|---|
| `go.mod`, `go.work` *(gitignored)* | Module, and the local infrena link. |
| `cmd/infrena-backend-s3/main.go` | Nine lines: build the backend, call `backendsdk.Serve`. |
| `internal/s3backend/config.go` | The `configure` payload: bucket, path, profile, region, endpoint, path-style. |
| `internal/s3backend/client.go` | minio-go wiring, credentials resolution, key layout. |
| `internal/s3backend/lock.go` | Conditional-write locking, and the support proof. |
| `internal/s3backend/backend.go` | `Get`, `Put`, `List` over the client. |
| `internal/s3backend/*_test.go` | Unit tests, plus the MinIO-backed suite behind a build tag. |
| `plugin.yaml` | `name: s3`, `protocol: [1]`, `infrena: ">= 0.8.0"`. |

---

## Task 1: Scaffold the repository

**Files:** the whole skeleton.

**Interfaces:**
- Produces: a module that builds and serves, answering the handshake and nothing else.

- [ ] **Step 1: Read the precedent**

Read `~/projects/infrena-provider-aws`: its `go.mod`, its gitignored `go.work`, `.gitignore`, `Makefile`, `.github/workflows/`, and `cmd/`. Mirror the arrangement. The `go.work` points at `../infrena` for local work while `go.mod` requires a real version, and CI builds with `GOWORK=off` against the required version — copy that exactly, including the comment explaining it.

- [ ] **Step 2: Write the failing test**

```go
// The plugin must answer a handshake before it can do anything else, and
// pkg/plugintest is the harness an out-of-module plugin is meant to test
// through. If this passes, the SDK wiring is right and every later task is
// about behaviour rather than plumbing.
func TestThePluginServesAndAnswersAHandshake(t *testing.T) {
	bin := buildBinary(t) // go build ./cmd/infrena-backend-s3

	h, err := plugintest.Open(bin)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()
}
```

Check what `pkg/plugintest` actually offers for a BACKEND before writing this — it was built for providers, and if it has no backend equivalent, say so in your reply and test the handshake directly over a pipe instead of inventing a half-harness.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./... -run TestThePluginServes -v`
Expected: FAIL — nothing exists yet.

- [ ] **Step 4: Write minimal implementation**

`cmd/infrena-backend-s3/main.go` constructs an `s3backend.New()` and calls `backendsdk.Serve`. The backend's seven methods can return "not configured" at this point; Task 2 fills them in.

- [ ] **Step 5: Run and commit**

```bash
go build ./... && go test ./... && gofmt -l . && go vet ./...
git init && git add -A
git commit -m "Scaffold the S3 backend plugin

Module, local work file pointing at a checkout of infrena, and a main
that serves the backend protocol and answers a handshake."
```

---

## Task 2: Configuration and the client

**Files:**
- Create: `internal/s3backend/config.go`, `internal/s3backend/client.go`, `internal/s3backend/config_test.go`

**Interfaces:**
- Produces:
  - `type Config struct { Bucket, Path, Profile, Region, Endpoint string; PathStyle *bool }`
  - `func ParseConfig(raw map[string]any) (Config, error)`
  - `func (c Config) keyFor(environment string) string`, `func (c Config) lockKeyFor(environment string) string`

- [ ] **Step 1: Write the failing test**

```go
func TestParseConfigTakesWhatTheBackendBlockCarries(t *testing.T) {
	got, err := ParseConfig(map[string]any{
		"bucket":  "some-bucket-name",
		"profile": "my-bucket-profile",
		"path":    "/infrena/",
		"region":  "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Bucket != "some-bucket-name" || got.Profile != "my-bucket-profile" {
		t.Errorf("Config = %+v", got)
	}
}

// A bucket is the one thing with no sensible default.
func TestAMissingBucketIsAnErrorNamingIt(t *testing.T) {
	_, err := ParseConfig(map[string]any{"path": "/infrena/"})
	if err == nil {
		t.Fatal("a config with no bucket was accepted")
	}
	if !strings.Contains(err.Error(), "bucket") {
		t.Errorf("error does not name the missing key: %v", err)
	}
}

// A profile NAMES a credential. A key IS one, and must never sit in a file
// that is committed to git. Refusing loudly is better than accepting it and
// having it end up in a repository.
func TestAnAccessKeyInConfigurationIsRefused(t *testing.T) {
	for _, key := range []string{"access_key", "secret_key", "access_key_id", "secret_access_key"} {
		_, err := ParseConfig(map[string]any{"bucket": "b", key: "AKIAEXAMPLE"})
		if err == nil {
			t.Errorf("%q was accepted as configuration", key)
			continue
		}
		if !strings.Contains(err.Error(), "profile") {
			t.Errorf("%q: refusal does not point at the alternative: %v", key, err)
		}
	}
}

// Keys are built by the backend, so the key space has no surprises in it.
func TestKeysAreBuiltUnderThePath(t *testing.T) {
	c := Config{Bucket: "b", Path: "/infrena/"}
	if got := c.keyFor("production"); got != "infrena/production.json" {
		t.Errorf("keyFor = %q", got)
	}
	if got := c.lockKeyFor("production"); got != "infrena/production.lock" {
		t.Errorf("lockKeyFor = %q", got)
	}
}

// An unset path puts state at the bucket root rather than erroring, and a
// path with or without slashes means the same thing.
func TestPathIsNormalised(t *testing.T) {
	for _, p := range []string{"", "/", "infrena", "/infrena", "infrena/", "/infrena/"} {
		c := Config{Bucket: "b", Path: p}
		got := c.keyFor("dev")
		if strings.HasPrefix(got, "/") || strings.Contains(got, "//") {
			t.Errorf("path %q produced key %q", p, got)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/s3backend/ -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Write minimal implementation**

`ParseConfig` reads known keys and **refuses an unknown one**, naming it — unlike infrena's `backend:` block, which cannot know this plugin's keys, this plugin does know them, so a typo is caught here rather than silently ignored. That asymmetry is deliberate and worth a comment.

`client.go` builds a `minio.Client` from the config. Credentials resolve through minio-go's chain: the named profile from a credentials file, then the environment, then an instance role. Region defaults sensibly; `PathStyle` defaults to on for a custom endpoint and off for AWS, since most self-hosted stores need path-style addressing.

- [ ] **Step 4: Run and commit**

```bash
go test ./... && gofmt -l . && go vet ./...
git add internal/s3backend
git commit -m "Read the backend configuration and build a client

A profile names a credential and belongs in configuration. An access key
is a secret and is refused, because this block is committed to git."
```

---

## Task 3: Locking, and proving the store can do it

**This is the task the whole design rests on. Read it twice.**

**Files:**
- Create: `internal/s3backend/lock.go`, `internal/s3backend/lock_test.go`

**Interfaces:**
- Produces:
  - `func (b *Backend) Lock(ctx context.Context, environment string) (backend.Lock, error)`
  - `func (b *Backend) Unlock(ctx context.Context, environment string) error`
  - `func (b *Backend) Inspect(ctx context.Context, environment string) (backend.Lock, bool, error)`
  - `func (b *Backend) ForceUnlock(ctx context.Context, environment string) error`
  - `func (b *Backend) proveConditionalWrites(ctx context.Context) error`

- [ ] **Step 1: Write the failing test**

```go
// THE POINT OF THIS TASK. Two different stores fail two different ways, and
// the probe has to catch both.
//
// Backblaze B2 refuses the FIRST conditional write with NotImplemented -
// measured against the real service, not assumed. That is the loud, easy
// case.
//
// The dangerous case is a store that IGNORES the header and overwrites,
// reporting success, because that is a lock that never locks with no error
// anywhere. No store is currently known to do it, but it costs one extra
// round trip to rule out and it is the one that fails silently.
//
// If the second write succeeds, two applies can both take the same lock, and
// invariant 5 is gone with no error anywhere.
// B2's ACTUAL behaviour, measured against the real service: NotImplemented on
// the first write. The loud case.
func TestProveConditionalWritesRejectsAStoreThatRefusesTheHeader(t *testing.T) {
	store := &fakeStore{conditionalWritesNotImplemented: true}
	b := newTestBackend(t, store)

	err := b.proveConditionalWrites(context.Background())
	if err == nil {
		t.Fatal("a store that cannot do conditional writes was accepted")
	}
	// The store's own message is more use than anything invented here.
	if !strings.Contains(err.Error(), "NotImplemented") {
		t.Errorf("refusal does not quote the store: %v", err)
	}
}

// The SILENT case: no store is known to do this, but it is the one that
// produces a lock which never locks with no error anywhere.
func TestProveConditionalWritesRejectsAStoreThatIgnoresTheHeader(t *testing.T) {
	store := &fakeStore{ignoresIfNoneMatch: true}
	b := newTestBackend(t, store)

	err := b.proveConditionalWrites(context.Background())
	if err == nil {
		t.Fatal("a store that silently overwrites was accepted")
	}
	for _, want := range []string{"conditional", "lock"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("refusal does not explain what is missing: %v", err)
		}
	}
}

func TestProveConditionalWritesAcceptsAStoreThatHonoursIt(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	if err := b.proveConditionalWrites(context.Background()); err != nil {
		t.Fatalf("a conforming store was refused: %v", err)
	}
}

// The probe must not leave rubbish in the user's bucket.
func TestTheProbeCleansUpAfterItself(t *testing.T) {
	store := &fakeStore{}
	b := newTestBackend(t, store)

	if err := b.proveConditionalWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := store.objectCount(); n != 0 {
		t.Errorf("probe left %d objects behind", n)
	}
}

// A second lock fails, and names who holds the first — the whole reason a
// lock records a holder.
func TestASecondLockFailsAndNamesTheHolder(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()

	first, err := b.Lock(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Lock(ctx, "dev")
	if err == nil {
		t.Fatal("a second lock was granted")
	}
	if !strings.Contains(err.Error(), first.User) || !strings.Contains(err.Error(), first.Host) {
		t.Errorf("conflict does not name the holder: %v", err)
	}
}

// A PreconditionFailed from the store is a lock conflict and must be
// reported as one, not as a transport error — the host maps it back to
// ErrLocked and every caller tests for that.
func TestAPreconditionFailureIsALockConflict(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}

	_, err := b.Lock(ctx, "dev")
	if !errors.Is(err, backend.ErrLocked) {
		t.Fatalf("second Lock returned %v, want ErrLocked", err)
	}
}

// Unlock removes the lock; Inspect then reports nothing held.
func TestUnlockReleasesAndInspectAgrees(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := b.Unlock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := b.Inspect(ctx, "dev"); held {
		t.Error("a lock survived Unlock")
	}
}
```

Write `fakeStore` with an `ignoresIfNoneMatch` switch — it is the only way to test the store that does the wrong thing, since a real MinIO always behaves.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/s3backend/ -run 'TestProve|TestASecondLock|TestAPrecondition|TestUnlock|TestTheProbe' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Write minimal implementation**

`proveConditionalWrites` writes `<path>/.infrena-probe-<random>` with `SetMatchETagExcept("*")`, writes it again the same way, and requires **the first to succeed and the second to fail** with `PreconditionFailed`. Then deletes it. Run once, at configure time.

An error on the first write means the store cannot do conditional writes at all — **quote its message**, because B2's `NotImplemented` says more than anything this plugin would write. Both writes succeeding means the header was ignored, which is the silent case, and the message has to explain that itself since the store said nothing.

```go
// A store that does not implement conditional writes does not say so. It
// ignores the header and overwrites, and reports success — which is exactly
// the shape of a lock that never locks. So support is proven by observing a
// refusal, never by asking.
//
// The probe runs at configure time rather than at the first Lock, because a
// backend that cannot lock must be refused when it loads (spec §5), not when
// an apply is already under way.
```

`Lock` writes the lock object with the same conditional header, carrying the holder the host supplied. `PreconditionFailed` becomes `backend.ErrLocked`, wrapping a message naming the current holder — which means reading the existing object to report it.

- [ ] **Step 4: Run and commit**

```bash
go test ./... && gofmt -l . && go vet ./...
git add internal/s3backend
git commit -m "Lock with a conditional write, and prove the store can do one

A store that does not implement conditional writes does not return an
error, it overwrites and reports success, which is a lock that never
locks. Support is proven by writing a probe twice and requiring the
second to be refused."
```

---

## Task 4: Get, Put and List

**Files:**
- Create: `internal/s3backend/backend.go`, `internal/s3backend/backend_test.go`

- [ ] **Step 1: Write the failing test**

```go
// State is bytes. The backend never parses it, so a round trip must be
// byte-identical rather than merely equivalent.
func TestStateRoundTripsByteForByte(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"version":1,"serial":7,"resources":{"a":{"x":1}}}`)

	if err := b.Put(ctx, "dev", raw); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("Get = %s, want %s", got, raw)
	}
}

// An environment never written is empty, not an error — a project that has
// never applied is the ordinary case.
func TestGettingAnEnvironmentThatWasNeverWrittenIsEmpty(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	got, err := b.Get(context.Background(), "never")
	if err != nil {
		t.Fatalf("Get on a missing environment errored: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Get = %s, want empty", got)
	}
}

// Put without the lock is refused BY THE BACKEND, not by caller discipline.
// Invariant 5 is a property of what the backend allows.
func TestPutWithoutTheLockIsRefused(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	err := b.Put(context.Background(), "dev", []byte(`{}`))
	if !errors.Is(err, backend.ErrNotLocked) {
		t.Fatalf("Put without a lock returned %v, want ErrNotLocked", err)
	}
}

// List reports environments, not objects: it must not report the lock files
// sitting beside them, and must be sorted.
func TestListReportsEnvironmentsAndNotLockObjects(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	for _, env := range []string{"production", "dev"} {
		if _, err := b.Lock(ctx, env); err != nil {
			t.Fatal(err)
		}
		if err := b.Put(ctx, env, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"dev", "production"}) {
		t.Errorf("List = %v, want [dev production]", got)
	}
}
```

- [ ] **Step 2–4:** as the pattern above. Commit message:

```
Read and write state as bytes

A backend stores what it is given and never parses it, so a round trip is
byte for byte. Listing reports environments rather than objects, so the
lock files beside them do not appear as environments of their own.
```

---

## Task 5: A conformance suite, in the infrena repository

**THIS ONE TASK IS IN `~/projects/infrena`, NOT the backend repo.** It ships in infrena so that every backend author gets it, and the S3 backend is simply its first consumer.

### Why a conformance suite rather than a test harness

A backend author has three things they could test, and only one is worth anyone's time:

1. **Their seven methods behave** — ordinary Go unit tests. No harness needed.
2. **Their binary speaks the protocol** — this is what a `pkg/plugintest` equivalent would give them. But the transport is entirely `pkg/backendsdk`'s code, so testing it per-backend tests INFRENA's code, once per backend, forever. It catches nothing infrena's own SDK tests do not.
3. **Their backend satisfies the contract's SEMANTICS** — a second lock is refused, `Put` without a lock is refused, `Get` of a never-written environment is empty rather than an error, `List` excludes lock objects, a conflict names its holder.

**Only 3 is missing, and it is where a wrong backend does real damage**: every item on that list, implemented wrongly, corrupts state silently rather than erroring.

It is also the same idea as the host adapter for providers — which exists because a third-party binary cannot be held to a doc comment (§31.1). "Every backend must lock" is currently enforced only at the type level: you cannot compile without the methods, and nothing checks that they actually EXCLUDE. This checks it.

**Files:**
- Create: `pkg/backendtest/conformance.go`, `pkg/backendtest/conformance_test.go`

**Interfaces:**
- Produces: `func Conformance(t *testing.T, newBackend func(t *testing.T) backend.Backend)`

- [ ] **Step 1: Write the failing test — the suite's own proof**

A conformance suite that passes everything is worthless, so the suite is tested against backends that are deliberately wrong. **Every check must be shown to fail against an implementation that violates exactly that check, and nothing else.**

```go
// A correct backend passes.
func TestConformancePassesACorrectBackend(t *testing.T) {
	Conformance(t, func(t *testing.T) backend.Backend { return newMemoryBackend() })
}

// And every check catches the one thing it is for. Each broken backend
// violates exactly one rule, so a check that fires on the wrong one is as
// visible as a check that does not fire at all.
func TestEveryConformanceCheckCatchesItsOwnViolation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		broken func() backend.Backend
		expect string
	}{
		{"a lock that does not exclude", func() backend.Backend { return &brokenBackend{lockAlwaysSucceeds: true} }, "second lock"},
		{"a write with no lock held", func() backend.Backend { return &brokenBackend{putWithoutLock: true} }, "not locked"},
		{"a missing environment erroring", func() backend.Backend { return &brokenBackend{missingIsError: true} }, "never written"},
		{"listing that includes lock objects", func() backend.Backend { return &brokenBackend{listIncludesLocks: true} }, "lock"},
		{"state altered in transit", func() backend.Backend { return &brokenBackend{mangleState: true} }, "byte"},
		{"a conflict that does not name its holder", func() backend.Backend { return &brokenBackend{anonymousConflict: true} }, "holder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &recordingT{}
			Conformance(fake, func(*testing.T) backend.Backend { return tc.broken() })

			if !fake.failed {
				t.Fatalf("conformance passed a backend that %s", tc.name)
			}
			if !strings.Contains(strings.ToLower(fake.log()), tc.expect) {
				t.Errorf("failure does not name the violation %q:\n%s", tc.expect, fake.log())
			}
		})
	}
}
```

`recordingT` captures failures instead of failing the outer test — check whether the standard library offers a way to do this before writing one; `testing.T` cannot be constructed, so `Conformance` should take a small interface (`Errorf`, `Fatalf`, `Helper`, `Run`) that `*testing.T` satisfies. Say in your reply which shape you used.

`brokenBackend` is a correct in-memory backend with one switch each. Keep them independent — a broken backend that violates two rules cannot prove which check fired.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/backendtest/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

```go
// Package backendtest checks that a state backend obeys the contract, and is
// the thing a backend author runs against their own implementation.
//
// IT TESTS SEMANTICS, NOT TRANSPORT. Whether a plugin speaks the protocol
// correctly is pkg/backendsdk's business, and testing it once per backend
// would test infrena's code rather than the author's. What differs between
// backends, and what silently corrupts state when it is wrong, is whether a
// second lock is actually refused and whether a write without one is.
//
// "Every backend must lock" (spec §5) is enforced at the type level today:
// a backend without the methods does not compile. Nothing checks the methods
// EXCLUDE. This does, which is the same reason internal/pluginhost's adapter
// enforces what the engine will not trust a provider to honour — a
// third-party binary cannot be held to a doc comment.
```

Each check is its own function with a name that says what it proves, so a failure names the rule rather than a line number.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS. Also wire it into `pkg/backendsdk`'s own tests so the reference in-memory backend runs it — **the suite must have a consumer inside infrena, or it is a code path the default does not exercise**, which is how the last four of these rotted.

- [ ] **Step 5: Commit in the INFRENA repo**

```bash
cd ~/projects/infrena
gofmt -l . && go vet ./...
git add pkg/backendtest pkg/backendsdk
git commit -m "Let a backend check that it obeys the contract

Locking is enforced at the type level today, so a backend without the
methods does not compile and one whose methods never exclude does. Every
check is proven against a backend that breaks exactly that rule."
```

**Versioning note for whoever tags next:** this adds a public package and no format version. By §61's letter that is a patch, since nothing an existing project does changes. Adding public API in a patch is unusual enough to be worth a deliberate decision rather than a default — raise it rather than assuming.

- [ ] **Step 6: Run it from the S3 backend**

Back in `~/projects/infrena-backend-s3`, add a test calling `backendtest.Conformance` against the S3 backend pointed at MinIO. This is Task 6's territory for the live plumbing, but the call belongs here so the suite gains its real consumer immediately.

---

## Task 6: Against a real MinIO

**MANDATORY. A green unit suite is not evidence the store agrees.**

**Files:**
- Create: `internal/s3backend/live_test.go` (build tag `live`), `docker-compose.yml` or an equivalent runner script

- [ ] **Step 1: Stand up MinIO**

A container on a local port with a known access key, and a test helper that creates a bucket per test run and removes it afterwards. Skip the whole file, with a message saying how to start MinIO, when it is not reachable — **but never skip silently**: mirror infrena's `INFRENA_REQUIRE_PLUGIN` idea with a `REQUIRE_LIVE_STORE=1` that turns the skip into a failure, so CI cannot report green on tests that never ran.

- [ ] **Step 2: Run the whole unit suite against it**

Every behaviour from Tasks 3 and 4, against the real store: the conditional-write proof, a second lock failing, the holder being named, a byte-for-byte round trip, an empty Get, a refused unlocked Put, and List excluding lock objects.

- [ ] **Step 3: Test concurrency for real**

```go
// Invariant 5, against a real store. Ten goroutines race for one lock;
// exactly one may win. A fake cannot prove this — its map is guarded by a
// mutex the real store does not have.
func TestOnlyOneOfManyConcurrentLocksSucceeds(t *testing.T) {
	b := liveBackend(t)
	var won int64
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Lock(context.Background(), "race"); err == nil {
				atomic.AddInt64(&won, 1)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d of 10 concurrent locks succeeded, want exactly 1", won)
	}
}
```

- [ ] **Step 4: Report what you saw**, verbatim, including anything that behaved differently from the fake. A difference is a finding.

- [ ] **Step 5: Commit**

```bash
git add internal/s3backend/live_test.go docker-compose.yml
git commit -m "Run the backend against a real store

A fake cannot prove that ten racing locks leave exactly one winner,
because its map is guarded by a mutex the real store does not have."
```

---

## Task 7: End to end, driven by infrena

**Files:**
- Create: `e2e/` following `infrena-provider-aws`'s e2e arrangement

- [ ] **Step 1: The test**

Build the backend binary, place it where `infrena` will find it, write a project whose `infra.yml` carries:

```yaml
backend:
  plugin: s3
  bucket: <test bucket>
  endpoint: http://127.0.0.1:9000
  path: /infrena/
```

Then drive real commands: `infrena apply dev --auto-approve`, a clean re-plan, `infrena state list`, a second concurrent apply that must be refused for the lock, and `infrena destroy`. Assert the state object actually exists in the bucket afterwards, read directly rather than through infrena.

This is the join step 1 could not test: `backendhost` covered `Open`, `internal/cli` covered routing, and nothing exercised both through a command until now.

- [ ] **Step 2–3:** run it, commit.

```
Drive the backend through real infrena commands

Step one covered opening a backend and routing to one, but nothing
exercised both through a command until there was a backend to run.
```

---

## Task 8: A second store, then ship

**Files:** `README.md`, `plugin.yaml`, `.github/workflows/`, `docs/`

- [ ] **Step 1: Prove "S3-compatible" against something that is not MinIO**

MinIO alone demonstrates very little — it is the most standards-conformant implementation there
is, which makes it the easiest possible case. **This step needs credentials James may or may not
have; ask before assuming, and never sign up for anything.** If no third-party account is
available, say so plainly and stop at MinIO rather than claiming coverage that was not tested.

Where an account exists, run the live suite against one of Cloudflare R2, DigitalOcean Spaces or
Wasabi and report exactly what differed. Path-style addressing and region naming are the likely
candidates.

**And if B2 credentials exist, run it against B2 and assert the probe REFUSES.** That is the
single most valuable live test available: a real store that genuinely cannot lock, proving the
refusal path works against reality rather than against a fake with a boolean switch. A passing
B2 run would mean the probe is broken, not that B2 improved.

- [ ] **Step 2: `plugin.yaml`**

`name: s3`, `kind` is not a field (§31.3's naming convention carries it), `protocol: [1]`, `infrena: ">= 0.8.0"`, platforms matching infrena's eight.

- [ ] **Step 3: CI and release workflows**

Copy `infrena-provider-aws`'s, adjusted. CI must run the live suite against MinIO with `REQUIRE_LIVE_STORE=1` so a skip is a failure.

- [ ] **Step 4: README**

What it is, the `backend:` block with every key, how credentials resolve, and the conditional-write requirement stated plainly — including that a store failing the proof is refused rather than run unsafely. Carry the compatibility table verbatim, **naming Backblaze B2 as not supported and why**, so a B2 user learns it from the README rather than from a refusal after they have written their configuration.

- [ ] **Step 5: Commit**

```
Document the backend and wire up CI

The live suite runs against a real store in CI, and a skipped live suite
is a failed one, because a backend that has only been tested against a
fake has not been tested.
```
