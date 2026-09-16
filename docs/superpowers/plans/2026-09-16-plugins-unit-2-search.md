# Plugin Search — Unit 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `infrena plugins search <name>` finds every plugin of that name across every source the user trusts or the project names, says which can actually run here, and says why each of the others cannot.

**Architecture:** All network code lives in a new `internal/plugins/remote`, BELOW nothing and imported only by the CLI — `internal/plugins` itself has a test asserting `net/http` is not in its dependency tree, and that must keep passing. Compatibility filtering composes the predicates `pkg/pluginmanifest` already ships (`Supports`, `SpeaksProtocol`, `AllowsInfrena`) and adds the one thing they do not: a human reason for every rejection. A disk cache sits between the client and GitHub so a repeated search costs nothing.

**Tech Stack:** Go 1.27, Cobra, `gopkg.in/yaml.v3`, and `net/http` from the standard library. **No HTTP or GitHub library may be added** — the third-party budget stays at two packages.

**Spec:** `PLAN.md` §31.3 (build order amended 2026-09-16; this is step 2)

## Global Constraints

- Go 1.27.0 module floor. `mise` is not active in non-interactive shells.
- **Third-party budget is two packages**: `github.com/spf13/cobra`, `gopkg.in/yaml.v3`. The GitHub client is `net/http` and `encoding/json` from the standard library. Adding `go-github` or any HTTP helper is out of scope for this plan and would need its own decision.
- **THE NETWORK IS NEVER ON THE HOT PATH.** `validate`, `plan`, `apply`, `destroy`, `refresh`, `discover`, `import`, `graph`, `explain` and `state` must never make a network request for plugin discovery. Searching happens only in `infrena plugins search` / `install`, and (Unit 3) in one interactive prompt. **`internal/plugins/network_test.go`'s `TestPluginsPackageCannotReachTheNetwork` must keep passing** — put every HTTP import in `internal/plugins/remote`, never in `internal/plugins`.
- **A manifest is read at the git TAG, never the default branch** (§31.2). The default branch describes unreleased code, and judging compatibility from it would report a plugin as compatible that nobody can install.
- **A rate limit must NEVER be reported as "not found".** See Task 1; this is the single most important message in the unit.
- **Never auto-pick between two sources answering one name.** Not the first alphabetically, not the higher version, not the official one.
- **Nothing is executed.** This unit downloads no binaries and runs nothing; it reads text files.
- Errors follow §44: what is wrong, where, what was expected, a suggested action the user can take.
- `gofmt -l .` and `go vet ./...` clean; full `go test -count=1 ./...` green with `INFRENA_REQUIRE_PLUGIN=1`.
- Commit messages: plain English, no em-dashes, minimal, **never** mention Claude, AI or the model. Each ends with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/plugins/remote/github.go` *(new)* | The GitHub calls: list an owner's repositories, resolve the latest release tag, fetch a file at a tag. |
| `internal/plugins/remote/errors.go` *(new)* | `RateLimitError` and friends — the distinction that must not collapse. |
| `internal/plugins/remote/cache.go` *(new)* | Disk cache with a TTL under the user's cache dir. |
| `internal/plugins/remote/*_test.go` *(new)* | Against `httptest`, never the real API. |
| `internal/plugins/compat.go` *(new)* | Filtering with a reason per rejection. No I/O, so it stays in the network-free package. |
| `internal/plugins/search.go` *(new)* | Orchestration over sources; takes a fetcher interface so it needs no network itself. |
| `internal/cli/plugins.go` | The `search` subcommand. |

---

## Task 1: The GitHub client, and the error that must not be confused

**Files:**
- Create: `internal/plugins/remote/github.go`, `internal/plugins/remote/errors.go`, `internal/plugins/remote/github_test.go`

**Interfaces:**
- Produces:
  - `type Client struct{ HTTP *http.Client; BaseURL string; Token string }`
  - `func NewClient() *Client` — reads `INFRENA_GITHUB_TOKEN`, then `GITHUB_TOKEN`
  - `(*Client) Repositories(ctx, owner string) ([]string, error)`
  - `(*Client) LatestTag(ctx, owner, repo string) (string, error)`
  - `(*Client) FileAtTag(ctx, owner, repo, tag, path string) ([]byte, error)`
  - `type RateLimitError struct{ Resets time.Time; Authenticated bool }` with `Error() string`
  - `type NotFoundError struct{ What string }`

- [ ] **Step 1: Write the failing test**

```go
// THE MOST IMPORTANT TEST IN THIS UNIT. A rate limit and a missing repository
// come back from the SAME API call, and conflating them tells a user their
// plugin does not exist when what actually happened is that they searched four
// times in an hour. Those two messages send a reader to completely different
// places: one to check the spelling of a name, the other to wait or set a token.
func TestARateLimitIsNeverReportedAsNotFound(t *testing.T) {
	reset := time.Now().Add(37 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		// GitHub answers an exhausted limit with 403, and 404 for a repository
		// that is missing OR private. Both must be told apart from each other.
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	_, err := c.Repositories(context.Background(), "mycorp")

	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error is %T (%v), want *RateLimitError", err, err)
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		t.Fatal("a rate limit also reported as not found")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("message does not say rate limit: %v", err)
	}
	// Section 44: the message says when it resets and that a token raises it,
	// because both are actions the user can actually take.
	if !strings.Contains(err.Error(), "INFRENA_GITHUB_TOKEN") {
		t.Errorf("message does not name the token variable: %v", err)
	}
	if rl.Resets.Unix() != reset.Unix() {
		t.Errorf("Resets = %v, want %v", rl.Resets, reset)
	}
}

// The other side of the same coin: a genuine 404 must be a NotFoundError and
// must not be dressed up as a rate limit.
func TestAMissingRepositoryIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	_, err := c.LatestTag(context.Background(), "someone", "infrena-provider-nope")

	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error is %T (%v), want *NotFoundError", err, err)
	}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		t.Fatal("a missing repository also reported as a rate limit")
	}
}

// A 403 that is NOT a rate limit (a private repository, a bad token) must be
// neither of the two above, or the message sends the reader somewhere useless.
func TestAForbiddenThatIsNotARateLimitIsItsOwnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	_, err := c.Repositories(context.Background(), "mycorp")

	var rl *RateLimitError
	if errors.As(err, &rl) {
		t.Error("a plain forbidden reported as a rate limit")
	}
	if err == nil {
		t.Fatal("a forbidden response was accepted")
	}
}

func TestRepositoriesListsAnOwnersRepositories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"name":"infrena-provider-aws"},{"name":"website"},{"name":"infrena-provider-hetzner"}]`)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	got, err := c.Repositories(context.Background(), "mycorp")
	if err != nil {
		t.Fatal(err)
	}
	// Unfiltered: the CALLER decides what the naming convention means, so this
	// stays a thin transport and the convention lives in one place.
	if len(got) != 3 {
		t.Errorf("Repositories = %v, want all three", got)
	}
}

// The manifest is read at the TAG, never the default branch (section 31.2):
// the default branch describes unreleased code, so judging compatibility from
// it would report a plugin as compatible that nobody can install.
func TestFileAtTagRequestsTheTagAndNotTheDefaultBranch(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		fmt.Fprint(w, "manifest: 2\n")
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	if _, err := c.FileAtTag(context.Background(), "infrena", "infrena-provider-aws", "v0.4.0", "plugin.yaml"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "v0.4.0") {
		t.Errorf("request path %q does not name the tag", path)
	}
	for _, branch := range []string{"main", "master", "HEAD"} {
		if strings.Contains(path, branch) {
			t.Errorf("request path %q names a branch", path)
		}
	}
}

func TestATokenIsSentWhenOneIsSet(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, Token: "secret"}

	if _, err := c.Repositories(context.Background(), "mycorp"); err != nil {
		t.Fatal(err)
	}
	if auth == "" {
		t.Error("no Authorization header was sent")
	}
	if strings.Contains(auth, "secret") == false {
		t.Errorf("Authorization header does not carry the token")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/plugins/remote/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

Package doc:

```go
// Package remote is the only place in infrena that talks to a plugin's forge.
//
// IT IS SEPARATE FROM internal/plugins DELIBERATELY. PLAN.md §31.3 makes "the
// network is never on the hot path" a hard rule, and internal/plugins carries a
// test asserting net/http is absent from its dependency tree. A package that
// CAN reach the network is one that a future caller on the hot path can reach it
// from; keeping the reaching one layer up is what makes the rule checkable
// rather than merely intended.
//
// It executes nothing and downloads no binaries. It reads text.
package remote
```

`BaseURL` exists so tests point at `httptest` — never at api.github.com. Classify responses in one place:

```go
// classify turns a response into the error the caller must be able to tell
// apart, and the rate-limit case is why this function exists at all.
//
// GitHub answers an exhausted limit with 403 and a missing-or-private
// repository with 404, and unauthenticated callers get 60 requests an hour —
// which one owner search with a few candidates can exhaust in a single
// invocation. Reporting that as "not found" tells a user their plugin does not
// exist, sends them to check a spelling that was right, and hides a condition
// that fixes itself in under an hour. The two messages must never collapse into
// one.
func classify(resp *http.Response) error
```

A 403 is a `RateLimitError` only when `X-RateLimit-Remaining` is `0`; otherwise it is a plain forbidden error mentioning a private repository or a bad token. `Resets` comes from `X-RateLimit-Reset`. The message names the reset time and both token variables.

`NewClient` reads `INFRENA_GITHUB_TOKEN` then `GITHUB_TOKEN`, and the token is used **for search only**. Set a timeout on the `http.Client` — a hung search with no deadline is a hung command.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/remote/ -v && go test ./internal/plugins/ -run TestPluginsPackageCannotReachTheNetwork -v`
Expected: both PASS — the second proves the new HTTP code did not leak into the network-free package.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/...
git add internal/plugins/remote/
git commit -m "Talk to GitHub, and tell a rate limit from a missing repository

Both come back from the same call, and an exhausted limit reported as not
found tells a user their plugin does not exist when they have really just
searched too often in an hour."
```

---

## Task 2: The cache

**Files:**
- Create: `internal/plugins/remote/cache.go`, `internal/plugins/remote/cache_test.go`

**Interfaces:**
- Produces:
  - `type Cache struct{ Dir string; TTL time.Duration; Now func() time.Time }`
  - `(*Cache) Get(key string) ([]byte, bool)`
  - `(*Cache) Put(key string, data []byte) error`
  - `func DefaultCacheDir() (string, error)` — `<user cache dir>/infrena/plugins`

- [ ] **Step 1: Write the failing test**

```go
func TestAFreshEntryIsReturnedAndAStaleOneIsNot(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &Cache{Dir: t.TempDir(), TTL: time.Hour, Now: func() time.Time { return now }}
	if err := c.Put("mycorp/repos", []byte("payload")); err != nil {
		t.Fatal(err)
	}

	if got, ok := c.Get("mycorp/repos"); !ok || string(got) != "payload" {
		t.Errorf("Get = %q, %v; want the payload", got, ok)
	}

	now = now.Add(2 * time.Hour)
	if _, ok := c.Get("mycorp/repos"); ok {
		t.Error("a stale entry was returned")
	}
}

// A key contains a slash and whatever an owner is called. It must not be able
// to escape the cache directory or collide with another key.
func TestAKeyCannotEscapeTheCacheDirectory(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir, TTL: time.Hour, Now: time.Now}

	if err := c.Put("../../etc/passwd", []byte("x")); err != nil {
		t.Fatal(err)
	}

	var outside []string
	filepath.WalkDir(filepath.Dir(dir), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(p, dir) && strings.Contains(p, "passwd") {
			outside = append(outside, p)
		}
		return nil
	})
	if len(outside) > 0 {
		t.Errorf("cache wrote outside its directory: %v", outside)
	}
}

// A corrupt or unreadable cache entry is a MISS, never an error: a cache is an
// optimisation, and one that can fail a command is worse than no cache.
func TestACorruptEntryIsAMissRatherThanAnError(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir, TTL: time.Hour, Now: time.Now}
	if err := c.Put("k", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) == 0 {
		t.Fatal("nothing was written")
	}
	if err := os.WriteFile(filepath.Join(dir, entries[0].Name()), []byte("\x00 not valid"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := c.Get("k"); ok {
		t.Error("a corrupt entry was returned as a hit")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestAFreshEntry|TestAKeyCannot|TestACorruptEntry' ./internal/plugins/remote/ -v`
Expected: FAIL — `Cache` undefined.

- [ ] **Step 3: Write minimal implementation**

Hash the key (`sha256`, hex) for the filename, which removes the traversal question entirely rather than sanitising it. Store the stored-at time alongside the payload. Every failure path on read returns a miss. `Put` failing is reported to the caller but must never be fatal at the call site.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/remote/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/...
git add internal/plugins/remote/
git commit -m "Cache search results on disk

Unauthenticated GitHub allows sixty requests an hour and one owner search
can spend several, so a repeated search should cost nothing. A corrupt
entry is a miss, because a cache that can fail a command is worse than no
cache."
```

---

## Task 3: Compatibility filtering, with a reason for every rejection

**Files:**
- Create: `internal/plugins/compat.go`, `internal/plugins/compat_test.go`

This lives in `internal/plugins`, NOT `remote`: it is pure logic over a parsed manifest, so it belongs in the package that cannot reach the network.

**Interfaces:**
- Consumes: `pluginmanifest.Manifest` with `Supports(Platform)`, `SpeaksProtocol([]int)`, `AllowsInfrena(string)`; `semver.Constraint`.
- Produces:
  - `type Candidate struct { Source Source; Manifest *pluginmanifest.Manifest; Tag string; Usable bool; Reason string }`
  - `func Check(m *pluginmanifest.Manifest, env Environment) (usable bool, reason string)`
  - `type Environment struct { Platform pluginmanifest.Platform; Protocols []int; InfrenaVersion string; Constraint semver.Constraint }`

- [ ] **Step 1: Write the failing test**

```go
// Section 31.3: "Every filtered-out candidate is still worth mentioning, with
// the reason." A user whose plugin exists but has no darwin/arm64 build must be
// TOLD that, not told nothing was found — those send a reader to completely
// different places.
func TestEveryRejectionCarriesAReasonThatNamesTheEvidence(t *testing.T) {
	here := pluginmanifest.Platform{OS: "darwin", Arch: "arm64"}
	for _, tc := range []struct {
		name   string
		m      *pluginmanifest.Manifest
		env    Environment
		want   string
	}{
		{
			name: "no build for this platform",
			m:    manifest(t, "hetzner", "2.0.0", []int{4}, []string{"linux/amd64"}),
			env:  Environment{Platform: here, Protocols: []int{4, 3}},
			want: "darwin/arm64",
		},
		{
			name: "protocol does not intersect",
			m:    manifest(t, "hetzner", "2.0.0", []int{9}, []string{"darwin/arm64"}),
			env:  Environment{Platform: here, Protocols: []int{4, 3}},
			want: "protocol",
		},
		{
			name: "version constraint not satisfied",
			m:    manifest(t, "hetzner", "1.0.0", []int{4}, []string{"darwin/arm64"}),
			env:  Environment{Platform: here, Protocols: []int{4}, Constraint: constraint(t, ">= 2.0.0")},
			want: "2.0.0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usable, reason := Check(tc.m, tc.env)
			if usable {
				t.Fatal("candidate was accepted")
			}
			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason %q does not name %q", reason, tc.want)
			}
		})
	}
}

func TestAUsableCandidateHasNoReason(t *testing.T) {
	m := manifest(t, "hetzner", "2.0.0", []int{4}, []string{"darwin/arm64"})
	usable, reason := Check(m, Environment{
		Platform:  pluginmanifest.Platform{OS: "darwin", Arch: "arm64"},
		Protocols: []int{4, 3},
	})
	if !usable {
		t.Fatalf("a compatible plugin was rejected: %s", reason)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}
```

Write `manifest(t, name, version, protocols, platforms)` and `constraint(t, s)` as helpers building real `pluginmanifest.Manifest` values — read `pkg/pluginmanifest/manifest.go` for the actual field names rather than guessing them.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestEveryRejection|TestAUsableCandidate' ./internal/plugins/ -v`
Expected: FAIL — `Check` undefined.

- [ ] **Step 3: Write minimal implementation**

Check in a fixed order and return the FIRST failure, so a manifest failing two tests reports the one a user acts on first. Compose the manifest's own predicates; do not reimplement them. Each reason names the concrete evidence — this platform, the protocols on each side, the version and the constraint.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/ -v 2>&1 | tail -5`
Expected: PASS, including the network test.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/...
git add internal/plugins/
git commit -m "Say why a plugin cannot be used here

A plugin that exists but publishes no build for this machine is a
different answer from no plugin at all, and sends a reader somewhere
different."
```

---

## Task 4: Searching across sources

**Files:**
- Create: `internal/plugins/search.go`, `internal/plugins/search_test.go`

**Interfaces:**
- Consumes: Task 3's `Candidate`, `Check`, `Environment`; Unit 1's `Source`, `LoadTrusted`.
- Produces:
  - `type Fetcher interface { Repositories(ctx, owner string) ([]string, error); LatestTag(ctx, owner, repo string) (string, error); FileAtTag(ctx, owner, repo, tag, path string) ([]byte, error) }`
  - `func Search(ctx context.Context, f Fetcher, sources []Source, name string, env Environment) ([]Candidate, []error)`

`Fetcher` is why this file stays in the network-free package: it depends on the shape of the calls, not on making them, and `remote.Client` satisfies it.

- [ ] **Step 1: Write the failing test**

```go
// Section 31.3: two owners can both answer one name, and infrena must present
// every match and NEVER auto-pick - not the first alphabetically, not the
// higher version, not the official one. Choosing silently when two candidates
// both answer what the user asked gives them a thing they did not name.
func TestTwoOwnersPublishingOneNameBothSurvive(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{
			"infrena": {"infrena-provider-hetzner"},
			"mycorp":  {"infrena-provider-hetzner", "website"},
		},
		tags:  map[string]string{"infrena/infrena-provider-hetzner": "v1.0.0", "mycorp/infrena-provider-hetzner": "v2.0.0"},
		files: map[string][]byte{
			"infrena/infrena-provider-hetzner@v1.0.0": manifestBytes(t, "hetzner", "1.0.0"),
			"mycorp/infrena-provider-hetzner@v2.0.0":  manifestBytes(t, "hetzner", "2.0.0"),
		},
	}
	sources := []Source{mustSource(t, "github.com/infrena"), mustSource(t, "github.com/mycorp")}

	got, errs := Search(context.Background(), f, sources, "hetzner", hereEnv())
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}

	if len(got) != 2 {
		t.Fatalf("Search returned %d candidates, want both: %+v", len(got), got)
	}
}

// A repository source names one repository exactly and must not trigger an
// owner listing.
func TestARepositorySourceIsNotSearchedAsAnOwner(t *testing.T) {
	f := &fakeFetcher{
		tags:  map[string]string{"someone/infrena-provider-hetzner": "v1.0.0"},
		files: map[string][]byte{"someone/infrena-provider-hetzner@v1.0.0": manifestBytes(t, "hetzner", "1.0.0")},
	}

	got, errs := Search(context.Background(), f,
		[]Source{mustSource(t, "github.com/someone/infrena-provider-hetzner")}, "hetzner", hereEnv())

	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if f.repoCalls != 0 {
		t.Errorf("Repositories was called %d times for a repository source", f.repoCalls)
	}
}

// A rate limit from one source must not be silently swallowed, and must not be
// turned into "nothing found" for the whole search.
func TestARateLimitFromOneSourceIsReportedNotSwallowed(t *testing.T) {
	f := &fakeFetcher{
		repos:    map[string][]string{"mycorp": {"infrena-provider-hetzner"}},
		tags:     map[string]string{"mycorp/infrena-provider-hetzner": "v2.0.0"},
		files:    map[string][]byte{"mycorp/infrena-provider-hetzner@v2.0.0": manifestBytes(t, "hetzner", "2.0.0")},
		failOwner: map[string]error{"infrena": &remote.RateLimitError{Resets: time.Now().Add(time.Hour)}},
	}
	sources := []Source{mustSource(t, "github.com/infrena"), mustSource(t, "github.com/mycorp")}

	got, errs := Search(context.Background(), f, sources, "hetzner", hereEnv())

	if len(errs) == 0 {
		t.Fatal("a rate limit was swallowed")
	}
	// The other source still answered, so a partial result is still returned -
	// but the caller is told the answer is partial.
	if len(got) != 1 {
		t.Errorf("got %d candidates, want the one that succeeded", len(got))
	}
}

// A manifest whose name does not match what was asked for is not a match, even
// though the repository name suggested it would be. The manifest is the truth.
func TestTheManifestNameDecidesTheMatch(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{"mycorp": {"infrena-provider-hetzner"}},
		tags:  map[string]string{"mycorp/infrena-provider-hetzner": "v1.0.0"},
		files: map[string][]byte{"mycorp/infrena-provider-hetzner@v1.0.0": manifestBytes(t, "somethingelse", "1.0.0")},
	}

	got, _ := Search(context.Background(), f, []Source{mustSource(t, "github.com/mycorp")}, "hetzner", hereEnv())

	if len(got) != 0 {
		t.Errorf("got %+v, want no match when the manifest names a different plugin", got)
	}
}
```

Write `fakeFetcher` in the test file. **The fake is the point**: `Search` must be testable with no network at all, and the interface exists so that it is.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestTwoOwners|TestARepositorySource|TestARateLimitFromOne|TestTheManifestName' ./internal/plugins/ -v`
Expected: FAIL — `Search` undefined.

- [ ] **Step 3: Write minimal implementation**

For an owner source, list repositories and keep those matching `RepoPrefix`. For a repository source, go straight to it. For each, resolve the latest tag, fetch `plugin.yaml` **at that tag**, parse with `pluginmanifest.Parse`, and keep it only when the manifest's `name` equals the requested name. Run `Check` and record every candidate, usable or not.

Errors are collected and returned alongside results, never thrown away and never allowed to turn a partial answer into an empty one — the same "a partial answer a reader mistakes for a complete one is the failure worth avoiding" rule `discovery.Walk` already follows.

Results are **sorted deterministically** (source, then version) so repeated searches render identically — but sorting is presentation, and the caller still must not auto-pick.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/... -v 2>&1 | tail -5`
Expected: PASS, network test included.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/...
git add internal/plugins/
git commit -m "Search every source for a plugin by name

Two owners can both publish one name, so every match is kept and none is
chosen. A source that fails is reported rather than quietly turning a
partial answer into an empty one."
```

---

## Task 5: `infrena plugins search`

**Files:**
- Modify: `internal/cli/plugins.go`
- Test: `internal/cli/plugins_search_test.go` *(new)*

**Interfaces:**
- Consumes: Task 4's `Search`; Task 1's `remote.NewClient`; Task 2's `Cache`; Unit 1's `LoadTrusted`; the project's `plugins:` source, when one is named.
- Produces: a `search` subcommand with `--refresh`.

- [ ] **Step 1: Write the failing test**

```go
func TestSearchPrintsUsableAndRejectedWithReasons(t *testing.T) {
	dir := newProjectFixture(t)
	srv := fakeGitHub(t) // serves one usable and one wrong-platform plugin
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, _, code := runCommand(t, dir, "plugins", "search", "hetzner")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "2.0.0") {
		t.Errorf("usable candidate missing:\n%s", stdout)
	}
	// The rejected one must still appear, with why.
	if !strings.Contains(stdout, "no build for") {
		t.Errorf("rejected candidate or its reason missing:\n%s", stdout)
	}
}

// Nothing found is an answer and exits 0. It is also the moment a clear
// message matters most, because the user is deciding whether they typed the
// name wrong.
func TestSearchFindingNothingSaysSoAndExitsZero(t *testing.T) {
	dir := newProjectFixture(t)
	srv := fakeGitHubEmpty(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, _, code := runCommand(t, dir, "plugins", "search", "nope")

	if code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "No plugin named") {
		t.Errorf("output does not say nothing was found:\n%s", stdout)
	}
}

// Unit A's rule.
func TestSearchOutputModeLeavesStdoutByteEmpty(t *testing.T) {
	dir := newProjectFixture(t)
	srv := fakeGitHub(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)
	out := filepath.Join(t.TempDir(), "run.ndjson")

	stdout, _, _ := runCommand(t, dir, "plugins", "search", "hetzner", "--output", out)

	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
}

// THE HARD RULE, asserted at the command boundary: no command on the hot path
// may reach the network. This is the guard that catches a future change wiring
// search into one of them.
func TestHotPathCommandsMakeNoNetworkRequest(t *testing.T) {
	for _, command := range []string{"validate", "plan", "graph", "explain", "state"} {
		t.Run(command, func(t *testing.T) {
			dir := newProjectFixture(t)
			blocked := blockNetwork(t)

			runCommand(t, dir, command, "dev")

			if n := blocked.Attempts(); n != 0 {
				t.Errorf("%s made %d network attempts, want 0", command, n)
			}
		})
	}
}
```

`INFRENA_GITHUB_API` is a test seam for `BaseURL`. Document it as such; it is not a user-facing feature.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestSearch|TestHotPathCommands' ./internal/cli/ -v`
Expected: FAIL — unknown subcommand.

- [ ] **Step 3: Write minimal implementation**

Build the `Environment` from the running binary: `runtime.GOOS`/`GOARCH`, `pluginproto.Supported`, `version.Current()`, and the project's `plugins:` constraint for that name when there is one. Sources are `LoadTrusted` plus any source the project names for that plugin — **and a project-named source that is not trusted is rendered as such**, since Unit 3 is where installing from it requires confirmation.

Print a table through `ro.Out()`: SOURCE, VERSION, PROTOCOL, and either usable or the reason. Nothing found prints a sentence and exits 0.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli/
git commit -m "Add plugins search

Shows every match across every source, and for each one that cannot run
here, why. A plugin with no build for this machine is a different answer
from no plugin at all."
```

---

## Task 6: Documentation

**Files:**
- Modify: `PLAN.md` §31.3, `CLAUDE.md`

- [ ] **Step 1: Mark what shipped**

§31.3's build order was amended on 2026-09-16; mark step 2 shipped and leave steps 3 and 4 as designs.

- [ ] **Step 2: Update `CLAUDE.md`**

Cover: `internal/plugins/remote` is the only package that talks to a forge, and `internal/plugins` has a test asserting `net/http` is absent from its dependency tree; a manifest is read at the TAG and never the default branch; a rate limit is never reported as not-found; two owners answering one name are both shown and never auto-picked; the cache and the token variables.

- [ ] **Step 3: Verify**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... && go vet ./... && gofmt -l .`
Expected: all clean, integration suite RUNS.

- [ ] **Step 4: Commit**

```bash
git add PLAN.md CLAUDE.md
git commit -m "Document plugin search and where the network code lives"
```
