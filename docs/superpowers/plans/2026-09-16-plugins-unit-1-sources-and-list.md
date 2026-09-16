# Plugin Sources and `plugins list` — Unit 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Teach infrena where plugins may come from, and let a user ask what they are actually running — both entirely offline.

**Architecture:** A source is a parsed value with two forms (an owner to search, or one exact repository), living in a new `internal/plugins` package that knows nothing about HTTP — Unit 2 adds the network on top of it. The user's own `~/.config/infrena/plugins.yml` is the only place a source is TRUSTED; a project may only NAME one, via an additive mapping form on the existing `plugins:` key. `infrena plugins list` reports what the existing loader already knows and makes no network request at all.

**Tech Stack:** Go 1.27, Cobra, `gopkg.in/yaml.v3`. No new dependencies.

**Spec:** `PLAN.md` §31.3 (and §31.2 for `plugin.yaml`, already implemented in `pkg/pluginmanifest`)

## Global Constraints

- Go 1.27.0 module floor. `mise` is not active in non-interactive shells — use `make` targets or `export PATH="$HOME/.local/share/mise/shims:$PATH"`.
- **Third-party budget is two packages**: `github.com/spf13/cobra`, `gopkg.in/yaml.v3`. Unit 2's GitHub client will be stdlib `net/http`. Do not add anything.
- **THE NETWORK IS NEVER ON THE HOT PATH.** Nothing in this unit may make a network request at all. `validate`, `plan`, `apply`, `destroy`, `refresh`, `discover`, `import`, `graph`, `explain` and `state` must never make one for plugin discovery, ever. This is a hard rule, not a performance preference: a `plan` that consults the network behaves differently on a train, in a locked-down CI runner and during a GitHub outage, and invariant 6 says the same inputs produce the same plan.
- **A project may NAME a source; only a user may TRUST one.** Project configuration travels with a `git clone`, so it must never be able to grant a place infrena fetches executables from.
- **`internal/config` is the only package permitted to touch `yaml.Node`** (the one argued exception is `internal/generator`, which emits and never parses). The user's `plugins.yml` is NOT project configuration, so parse it with plain `yaml.Unmarshal` into a typed struct in `internal/plugins` — no `yaml.Node`, and do not route it through `internal/config`.
- **Parse into typed models immediately.** No `map[string]any` flowing through the application.
- Errors follow §44: what is wrong, where, what was expected, and a suggested action the user can actually take.
- `gofmt -l .` and `go vet ./...` clean before each commit; full `go test ./...` green with `INFRENA_REQUIRE_PLUGIN=1`.
- Commit messages: plain English, no em-dashes, minimal, **never** mention Claude, AI or the model. Each ends with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/plugins/source.go` *(new)* | `Source`: parsing and the owner-vs-repository distinction. No I/O. |
| `internal/plugins/source_test.go` *(new)* | Parsing table, including every malformed form. |
| `internal/plugins/trusted.go` *(new)* | Reading `~/.config/infrena/plugins.yml`; the always-trusted official owner. |
| `internal/plugins/trusted_test.go` *(new)* | Missing file, malformed file, the unremovable owner. |
| `internal/config/declarations.go` | `PluginConstraint` gains `Source`. |
| `internal/config/decode_*.go` | Decode the additive mapping form of `plugins:`. |
| `internal/cli/plugins.go` *(new)* | `infrena plugins` and `plugins list`. |
| `internal/cli/plugins_list_test.go` *(new)* | Output shape, and that no network is touched. |
| `internal/cli/root.go` | Register the command. |

---

## Task 1: `Source` parsing

**Files:**
- Create: `internal/plugins/source.go`, `internal/plugins/source_test.go`

**Interfaces:**
- Produces:
  - `type Kind int` with `KindOwner`, `KindRepository`
  - `type Source struct { Host, Owner, Repo string; Kind Kind }`
  - `func ParseSource(s string) (Source, error)`
  - `(Source) String() string` — round-trips what the user wrote
  - `(Source) PluginName() (string, bool)` — for a repository source, the `<name>` in `infrena-provider-<name>`

- [ ] **Step 1: Write the failing test**

```go
func TestParseSourceTellsAnOwnerFromARepository(t *testing.T) {
	for _, tc := range []struct {
		in    string
		kind  Kind
		owner string
		repo  string
		name  string
	}{
		{"github.com/mycorp", KindOwner, "mycorp", "", ""},
		{"github.com/infrena", KindOwner, "infrena", "", ""},
		{"github.com/someone/infrena-provider-hetzner", KindRepository, "someone", "infrena-provider-hetzner", "hetzner"},
		{"github.com/infrena/infrena-provider-aws", KindRepository, "infrena", "infrena-provider-aws", "aws"},
	} {
		got, err := ParseSource(tc.in)
		if err != nil {
			t.Fatalf("ParseSource(%q): %v", tc.in, err)
		}
		if got.Kind != tc.kind || got.Owner != tc.owner || got.Repo != tc.repo {
			t.Errorf("ParseSource(%q) = %+v, want kind %v owner %q repo %q", tc.in, got, tc.kind, tc.owner, tc.repo)
		}
		if got.String() != tc.in {
			t.Errorf("String() = %q, want %q", got.String(), tc.in)
		}
		name, ok := got.PluginName()
		if tc.name == "" {
			if ok {
				t.Errorf("PluginName() = %q for an owner source, want none", name)
			}
		} else if !ok || name != tc.name {
			t.Errorf("PluginName() = %q, %v; want %q", name, ok, tc.name)
		}
	}
}

// The naming convention is load-bearing: an owner search works by repository
// name, so a repository that does not follow it cannot be found by name and
// must be refused HERE rather than silently never matching.
func TestParseSourceRefusesARepositoryThatBreaksTheConvention(t *testing.T) {
	for _, in := range []string{
		"github.com/someone/hetzner",                 // no infrena-provider- prefix
		"github.com/someone/infrena-provider-",       // empty name
		"github.com/someone/infrena-plugin-hetzner",  // that is the BINARY name, not the repo name
	} {
		if _, err := ParseSource(in); err == nil {
			t.Errorf("ParseSource(%q) was accepted", in)
		}
	}
}

func TestParseSourceRefusesMalformedInput(t *testing.T) {
	for _, in := range []string{
		"",
		"mycorp",                                          // no host
		"github.com",                                      // host only
		"github.com/someone/infrena-provider-x/extra",     // too deep
		"https://github.com/mycorp",                       // a scheme is not the syntax
		"gitlab.com/mycorp",                               // only GitHub is built; see below
	} {
		if _, err := ParseSource(in); err == nil {
			t.Errorf("ParseSource(%q) was accepted", in)
		}
	}
}

// Section 31.3 says the syntax is host-prefixed PRECISELY so another forge can
// be added later without changing what a user wrote. So an unknown host is
// refused with a message that says it is not supported YET, not that the input
// is malformed - those send a reader to different places.
func TestAnUnsupportedHostSaysSoRatherThanCallingItMalformed(t *testing.T) {
	_, err := ParseSource("gitlab.com/mycorp")
	if err == nil {
		t.Fatal("gitlab.com was accepted")
	}
	if !strings.Contains(err.Error(), "gitlab.com") || !strings.Contains(err.Error(), "github.com") {
		t.Errorf("error should name the host given and the one supported: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/plugins/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

Create the package with this doc comment, which carries the rules the rest of §31.3 depends on:

```go
// Package plugins knows where a provider plugin may come from, and which of
// those places the USER has trusted.
//
// IT MAKES NO NETWORK REQUESTS, and Unit 2 adds a client on top of it rather
// than inside it. That split is not tidiness: PLAN.md §31.3 makes "the network
// is never on the hot path" a hard rule, and a package that can reach the
// network is one a future caller on the hot path can reach it FROM. Keeping the
// reaching in one place above this one is what makes the rule checkable.
//
// THE NAMING CONVENTION IS LOAD-BEARING. An owner search works by repository
// name, so a plugin lives in `infrena-provider-<name>`, its manifest says
// `name: <name>`, and its binary is `infrena-plugin-<name>`. That convention IS
// the registry: no index, no server, no publishing step, no account. The cost is
// that a plugin in a differently-named repository is findable only by naming the
// repository exactly, which is why the repository form of a source exists.
package plugins
```

`ParseSource` splits on `/` into at most three parts. One part is an error naming the two forms. Two parts is an owner. Three parts is a repository, and the third must match `infrena-provider-<name>` with a non-empty name — refuse it otherwise, naming the convention, because a repository that breaks it can never be found by name. Only `github.com` is accepted as a host, and an unknown host says so in those terms rather than "malformed".

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/
git add internal/plugins/
git commit -m "Parse where a plugin may come from

An owner to search, or one exact repository. A repository that breaks the
infrena-provider naming convention is refused when it is written, because
an owner search works by repository name and it could never be found."
```

---

## Task 2: The user's trusted sources

**Files:**
- Create: `internal/plugins/trusted.go`, `internal/plugins/trusted_test.go`

**Interfaces:**
- Consumes: Task 1's `Source`, `ParseSource`.
- Produces:
  - `func OfficialOwner() Source` — `github.com/infrena`
  - `func LoadTrusted(configHome string) ([]Source, error)` — the official owner first, then the user's, deduplicated
  - `func TrustedPath(configHome string) string`

- [ ] **Step 1: Write the failing test**

```go
// A user with no config file is the ordinary case and must not be an error.
// They still get the official owner, or `plugin: aws` would report that
// nothing matches, on a fresh machine, with no way to tell why.
func TestNoConfigFileStillTrustsTheOfficialOwner(t *testing.T) {
	got, err := LoadTrusted(t.TempDir())
	if err != nil {
		t.Fatalf("LoadTrusted with no file: %v", err)
	}
	if len(got) != 1 || got[0].String() != "github.com/infrena" {
		t.Errorf("LoadTrusted = %v, want just the official owner", got)
	}
}

func TestUserSourcesAreAddedAfterTheOfficialOwner(t *testing.T) {
	home := t.TempDir()
	writeTrusted(t, home, "sources:\n  - github.com/mycorp\n  - github.com/someone/infrena-provider-hetzner\n")

	got, err := LoadTrusted(home)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"github.com/infrena", "github.com/mycorp", "github.com/someone/infrena-provider-hetzner"}
	if len(got) != len(want) {
		t.Fatalf("LoadTrusted = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i].String(), want[i])
		}
	}
}

// Section 31.3: the official owner is always searched and CANNOT be removed.
// It is not a configurable default, because a configurable default is a thing
// that gets misconfigured into an empty list, after which `plugin: aws` reports
// that nothing matches and the cause is invisible.
func TestTheOfficialOwnerCannotBeRemoved(t *testing.T) {
	home := t.TempDir()
	// A user who lists other sources and not the official one, and a user who
	// writes an explicitly empty list, must both still get it.
	for _, body := range []string{"sources:\n  - github.com/mycorp\n", "sources: []\n", "sources:\n"} {
		writeTrusted(t, home, body)
		got, err := LoadTrusted(home)
		if err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if len(got) == 0 || got[0].String() != "github.com/infrena" {
			t.Errorf("body %q: LoadTrusted = %v, official owner missing", body, got)
		}
	}
}

// Listing it explicitly must not list it twice.
func TestListingTheOfficialOwnerDoesNotDuplicateIt(t *testing.T) {
	home := t.TempDir()
	writeTrusted(t, home, "sources:\n  - github.com/infrena\n  - github.com/mycorp\n")

	got, err := LoadTrusted(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("LoadTrusted = %v, want 2 entries", got)
	}
}

// A malformed trust file is an ERROR, never a silent fallback to the official
// owner alone. This file is a security decision the user wrote down; quietly
// ignoring it would mean a plugin install that should have been possible
// reports that nothing was found, or worse, that a source they revoked is
// silently still absent from a list they think they edited.
func TestAMalformedTrustFileIsAnErrorNamingTheFile(t *testing.T) {
	home := t.TempDir()
	writeTrusted(t, home, "sources:\n  - github.com/someone/not-a-plugin-repo\n")

	_, err := LoadTrusted(home)
	if err == nil {
		t.Fatal("a malformed source was accepted")
	}
	if !strings.Contains(err.Error(), TrustedPath(home)) {
		t.Errorf("error does not name the file: %v", err)
	}
}
```

Write `writeTrusted(t, home, body)` in the test file; it creates `TrustedPath(home)`'s parent and writes the body.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestNoConfigFile|TestUserSources|TestTheOfficialOwner|TestListingTheOfficial|TestAMalformed' ./internal/plugins/ -v`
Expected: FAIL — `LoadTrusted` undefined.

- [ ] **Step 3: Write minimal implementation**

`TrustedPath(configHome)` is `<configHome>/infrena/plugins.yml`; the caller passes `os.UserConfigDir()`'s result, so tests can supply a temp dir. Parse with plain `yaml.Unmarshal` into:

```go
type trustedFile struct {
	Sources []string `yaml:"sources"`
}
```

Set `yaml.Decoder.KnownFields(true)` so an unknown key is refused rather than ignored — this project fails closed on unknown keys everywhere else, and a typo in a security file is the worst place to start being lenient.

A missing file is `nil, nil` before the official owner is prepended. Any other read or parse error, and any unparseable source, is an error naming `TrustedPath`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/
git add internal/plugins/
git commit -m "Read the sources a user trusts

The official owner is always present and cannot be removed, because a
default that can be configured away is one that gets configured away, and
then a missing plugin reports nothing found with no visible cause."
```

---

## Task 3: A project may name a source

**Files:**
- Modify: `internal/config/declarations.go` (`PluginConstraint`), and whichever `internal/config/decode_*.go` decodes `plugins:` — find it with `grep -rn "Plugins\[" internal/config/`
- Test: the existing `internal/config` decode tests

**Interfaces:**
- Produces: `config.PluginConstraint` gains `Source string` and `SourceOrigin value.Origin`.

- [ ] **Step 1: Write the failing test**

```go
// The scalar form is what every existing project writes and must behave
// EXACTLY as it does today. The mapping form is additive (section 58).
func TestPluginsAcceptsBothTheScalarAndMappingForms(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
plugins:
  aws: ">= 0.3.0, < 0.4.0"
  hetzner:
    version: ">= 1.2"
    source: github.com/someone/infrena-provider-hetzner
`,
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}

	if got := p.Plugins["aws"]; got.Source != "" {
		t.Errorf("scalar form gained a source: %q", got.Source)
	}
	hetzner := p.Plugins["hetzner"]
	if hetzner.Source != "github.com/someone/infrena-provider-hetzner" {
		t.Errorf("source = %q", hetzner.Source)
	}
	if hetzner.SourceOrigin.Line == 0 {
		t.Error("source carries no origin, so a diagnostic cannot point at it")
	}
}

// Fails closed on unknown keys, like every other block in this language.
func TestAnUnknownKeyInThePluginMappingIsAnError(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "plugins:\n  aws:\n    versoin: \">= 1\"\n",
	})
	if !ds.HasErrors() {
		t.Fatal("an unknown key was accepted")
	}
}

// A source the project names must be WELL FORMED at decode time, so the error
// points at the line in infra.yml rather than surfacing later from an install.
func TestAMalformedProjectSourceIsRefusedAtItsLine(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "plugins:\n  aws:\n    source: not-a-source\n",
	})
	if !ds.HasErrors() {
		t.Fatal("a malformed source was accepted")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestPluginsAccepts|TestAnUnknownKeyInThePlugin|TestAMalformedProjectSource' ./internal/config/ -v`
Expected: FAIL — only the scalar form decodes.

- [ ] **Step 3: Write minimal implementation**

In the decoder, branch on the node kind: a scalar decodes exactly as today; a mapping takes `version` and `source`, both optional, refusing any other key. Validate `source` through `plugins.ParseSource`.

**Check the import direction before writing this.** If `internal/config` importing `internal/plugins` creates a cycle, do the validation in the compiler instead and say so in a comment — do not invert the dependency to make it fit.

Document on the field why a project may name but not trust:

```go
	// Source is where this plugin comes from, when the project says so
	// (PLAN.md §31.3). Empty means the trusted sources decide.
	//
	// A SOURCE NAMED HERE IS A CANDIDATE, NOT A PERMISSION. This file is
	// checked into git and travels to whoever clones it, so if it could grant
	// a download source then `git clone && infrena plan` would be enough for a
	// repository to introduce a place infrena fetches executables from. Only
	// the user's own configuration trusts an owner, and installing from an
	// untrusted one takes an explicit confirmation that names it.
	Source       string
	SourceOrigin value.Origin
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ ./internal/compiler/ 2>&1 | tail -5`
Expected: PASS, with every existing scalar-form test untouched.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/config/
git commit -m "Let a project name where a plugin comes from

The mapping form is additive and the scalar form still means what it did.
Naming a source is not trusting it, because this file travels with a clone
and must not be able to grant a place binaries are fetched from."
```

---

## Task 4: `infrena plugins list`

**Files:**
- Create: `internal/cli/plugins.go`, `internal/cli/plugins_list_test.go`
- Modify: `internal/cli/root.go`

**Interfaces:**
- Consumes: `pluginhost.Loader.Available()`, `pluginhost.DefaultSearch(dir, explicit)`, Unit A's `openRun`/`runOutput`, Task 2's `LoadTrusted`.
- Produces: `newPluginsCommand(opts *GlobalOptions) *cobra.Command` with a `list` subcommand.

Read `internal/pluginhost/loader.go` and `connect.go` first and use what is actually there to learn a plugin's resolved path and version. If the loader does not currently expose the path it loaded a binary from, adding a narrow accessor is in scope; adding a second search implementation is not.

- [ ] **Step 1: Write the failing test**

```go
func TestPluginsListReportsWhatIsInstalled(t *testing.T) {
	dir := newProjectFixture(t)

	stdout, _, code := runCommand(t, dir, "plugins", "list")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"NAME", "VERSION", "PATH", "fake"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

// "Nothing installed" is a real answer and must not look like a failure. It is
// also the state a fresh machine is in, which is exactly when a clear message
// matters most.
func TestPluginsListWithNothingInstalledSaysSoAndExitsZero(t *testing.T) {
	dir := newProjectFixture(t)

	stdout, _, code := runCommandWithoutPlugins(t, dir, "plugins", "list")

	if code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "No plugins installed") {
		t.Errorf("output does not say nothing is installed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "plugins install") {
		t.Errorf("output does not name what to do next:\n%s", stdout)
	}
}

// Unit A's rule, which every command obeys.
func TestPluginsListOutputModeLeavesStdoutByteEmpty(t *testing.T) {
	dir := newProjectFixture(t)
	out := filepath.Join(t.TempDir(), "run.ndjson")

	stdout, _, _ := runCommand(t, dir, "plugins", "list", "--output", out)

	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
}

// THE HARD RULE. This command must never reach the network, and neither may
// anything it calls.
func TestPluginsListMakesNoNetworkRequest(t *testing.T) {
	dir := newProjectFixture(t)
	// Point every proxy variable at a listener that fails the test if dialled,
	// and unset anything that could bypass it.
	blocked := blockNetwork(t)

	runCommand(t, dir, "plugins", "list")

	if n := blocked.Attempts(); n != 0 {
		t.Errorf("plugins list made %d network attempts, want 0", n)
	}
}
```

Write `blockNetwork(t)` as a helper: start a `net.Listener` on localhost that records any accepted connection, set `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY` to it with `t.Setenv`, and report the count. It is a smoke alarm, not a proof — but a direct `http.Get` in a future change trips it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestPluginsList' ./internal/cli/ -v`
Expected: FAIL — unknown command "plugins".

- [ ] **Step 3: Write minimal implementation**

Build `infrena plugins` as a parent with `list` under it, so `search`, `install` and `verify` land beside it in Units 2 and 3. Render a tabwriter table of NAME, VERSION, PATH through `ro.Out()`. Nothing installed prints a sentence naming `infrena plugins install`, and exits 0 — an empty list is an answer, not a failure.

Register it in `root.go` beside the other commands.

```go
// newPluginsCommand builds `infrena plugins` (PLAN.md §31.3).
//
// `list` ANSWERS A QUESTION WITH NO NETWORK: what am I actually running. That
// is deliberate and is the rule the whole subsection is built on — searching
// happens in `plugins search` and `plugins install`, and in one interactive
// prompt, never anywhere a plan or an apply can reach.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli/
git commit -m "Add plugins list

Answers what is actually installed, its version and where it was loaded
from, with no network request. Nothing installed is an answer rather than
a failure, and says what to run next."
```

---

## Task 5: Documentation

**Files:**
- Modify: `PLAN.md` §31.3, `CLAUDE.md`

- [ ] **Step 1: Mark what shipped in `PLAN.md` §31.3**

That section is a design written ahead of the code. Add a short note at its top saying which parts now exist — source parsing, trusted sources, the project mapping form, and `plugins list` — and that search, install, the lock file and the interactive offer are still designs. A design document that does not say which half is real sends a reader looking for code that is not there.

- [ ] **Step 2: Update `CLAUDE.md`**

Add a paragraph covering: the two source forms; that a project may NAME a source but only a user may TRUST one, and why (project configuration travels with a clone); that `github.com/infrena` cannot be removed; and the hard rule that the network is never on the hot path, naming the commands that must never touch it.

- [ ] **Step 3: Verify**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... && go vet ./... && gofmt -l .`
Expected: all clean, integration suite RUNS rather than skipping.

- [ ] **Step 4: Commit**

```bash
git add PLAN.md CLAUDE.md
git commit -m "Document plugin sources and what of the install design exists"
```
