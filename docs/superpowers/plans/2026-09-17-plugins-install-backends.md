# Installing Backends Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `infrena plugins install` can install a state backend, and knows which kind you meant without asking when the project already says.

**Architecture:** `plugins.RepoPrefix` is a single constant, `"infrena-provider-"`, and `ParseSource` *refuses* anything else — so `infrena-backend-s3` is not merely invisible to search, it is rejected when written. This teaches the package two prefixes and gives every source and candidate a `Role`. Install then resolves the role from `infra.yml`, because a project already says which kind it needs; `--kind` exists only for when there is no project to ask.

**Tech Stack:** Go 1.27, Cobra. Standard library only.

**Spec:** `PLAN.md` §31.3, and `docs/superpowers/specs/2026-09-16-remote-state-design.md` §4 (the naming convention extended to backends)

## Global Constraints

- Go 1.27.0 floor. `mise` is not active in non-interactive shells — use `make` targets or `export PATH="$HOME/.local/share/mise/shims:$PATH"`.
- **Third-party budget is two packages**: `github.com/spf13/cobra`, `gopkg.in/yaml.v3`.
- **THE NETWORK IS NEVER ON THE HOT PATH.** `internal/plugins` carries `TestPluginsPackageCannotReachTheNetwork`, asserting `net/http` is absent from its dependency tree. All HTTP stays in `internal/plugins/remote`. That test must keep passing.
- **Never auto-pick between two candidates answering one name** (§31.3). Present both and let the user choose; the choice is recorded in `plugins.lock`.
- **Never claim a plugin does not exist when it merely could not be seen.** A rate limit, an unauthenticated listing of private repositories, and a cached negative from another credential are three doors into that same mistake, and five bugs came through them.
- **A project may NAME a source; only a user may TRUST one.** Nothing here may widen that.
- Errors follow §44: what is wrong, where, what was expected, an action the user can take.
- `gofmt -l .` and `go vet ./...` clean; full `go test -count=1 ./...` green with `INFRENA_REQUIRE_PLUGIN=1`.
- Commit messages: plain English, no em-dashes, minimal, **never** mention Claude, AI or the model. Each ends with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.

**A NAMING TRAP, read this before writing any code.** `plugins.Kind` **already exists** and means *owner versus repository* — the two forms a SOURCE takes (`KindOwner`, `KindRepository`). It has nothing to do with provider versus backend. This plan therefore calls the new concept **`Role`** (`RoleProvider`, `RoleBackend`) and does **not** rename the existing type. The user-facing flag is still `--kind`, because that is the word a person types; the mismatch is deliberate and gets one comment saying so. Renaming `Kind` to `SourceKind` was considered and rejected as churn that makes the diff harder to review than the feature deserves.

---

## Why this is one missing qualifier and not a design flaw

Worth stating, because the obvious reaction is that the convention is broken. It is not. Every place the two kinds actually coexist, they already coexist correctly:

| | Provider | Backend |
| --- | --- | --- |
| Configuration | `providers: - plugin: s3` | `backend: plugin: s3` |
| Binary on disk | `infrena-plugin-s3` | `infrena-backend-s3` |
| `plugins.lock` key | `s3` | `infrena-backend-s3` (`backendhost.LockKey`) |
| Not-found error | `pluginhost.NotFoundError` | `backendhost.NotFoundError` |

Different config keys, different filenames, already-distinct lock entries, and separate typed errors that each know what they were looking for. **The ambiguity exists in exactly one place: the bare `<name>` argument to `plugins search` and `plugins install`.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/plugins/source.go` | Both prefixes; `Role`; parsing a backend repository. |
| `internal/plugins/search.go` | Carry `Role` onto every candidate; search both prefixes. |
| `internal/cli/plugins.go` | `list` and `search` show a KIND column. |
| `internal/cli/plugins_install.go` | Resolve the role from the project; `--kind`; bare install. |
| `internal/cli/plugins_project.go` *(new)* | Reading what a project declares, provider and backend. |

---

## Task 1: Two prefixes, and a role

**Files:**
- Modify: `internal/plugins/source.go`, `internal/plugins/search.go`
- Test: `internal/plugins/source_test.go`, `internal/plugins/search_test.go`

**Interfaces:**
- Produces:
  - `type Role int` with `RoleProvider`, `RoleBackend`; `(Role) String() string` returning `"provider"` / `"backend"`
  - `const ProviderRepoPrefix = "infrena-provider-"`, `const BackendRepoPrefix = "infrena-backend-"`
  - `RepoPrefix` stays as a deprecated alias of `ProviderRepoPrefix` **only if** something outside this package uses it; check with `grep -rn "plugins.RepoPrefix"` and delete it if nothing does.
  - `Source.Role Role` — meaningful for `KindRepository`; for `KindOwner` it is not set, because an owner publishes both.
  - `Candidate.Role Role`
  - `func (s Source) PluginName() (string, bool)` — unchanged signature, now stripping either prefix

- [ ] **Step 1: Write the failing test**

```go
// A backend repository is currently REFUSED at parse time, not merely
// filtered out of search: ParseSource requires the provider prefix. So
// nobody can even name one as a source.
func TestABackendRepositoryParses(t *testing.T) {
	got, err := ParseSource("github.com/infrena/infrena-backend-s3")
	if err != nil {
		t.Fatalf("ParseSource refused a backend repository: %v", err)
	}
	if got.Kind != KindRepository {
		t.Errorf("Kind = %v, want KindRepository", got.Kind)
	}
	if got.Role != RoleBackend {
		t.Errorf("Role = %v, want RoleBackend", got.Role)
	}
	name, ok := got.PluginName()
	if !ok || name != "s3" {
		t.Errorf("PluginName = %q, %v; want s3", name, ok)
	}
	if got.String() != "github.com/infrena/infrena-backend-s3" {
		t.Errorf("String = %q", got.String())
	}
}

func TestAProviderRepositoryStillParsesAndIsAProvider(t *testing.T) {
	got, err := ParseSource("github.com/infrena/infrena-provider-aws")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != RoleProvider {
		t.Errorf("Role = %v, want RoleProvider", got.Role)
	}
	name, _ := got.PluginName()
	if name != "aws" {
		t.Errorf("PluginName = %q, want aws", name)
	}
}

// The convention is still load-bearing: a repository matching NEITHER prefix
// can never be found by an owner search, so it is refused when written. The
// message must now offer both shapes rather than only the provider one.
func TestARepositoryMatchingNeitherPrefixIsStillRefused(t *testing.T) {
	_, err := ParseSource("github.com/someone/hetzner")
	if err == nil {
		t.Fatal("a repository matching neither prefix was accepted")
	}
	for _, want := range []string{"infrena-provider-", "infrena-backend-"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not offer %q: %v", want, err)
		}
	}
}

// An owner publishes both kinds, so an owner source cannot carry a role and
// must not pretend to.
func TestAnOwnerSourceCarriesNoRole(t *testing.T) {
	got, err := ParseSource("github.com/mycorp")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindOwner {
		t.Fatalf("Kind = %v", got.Kind)
	}
	// Documented as meaningless for an owner; the test pins that reading it
	// is not mistaken for a claim.
	if _, ok := got.PluginName(); ok {
		t.Error("an owner source named a plugin")
	}
}
```

```go
// An owner search must list BOTH prefixes, or a backend published by a
// trusted owner is invisible.
func TestSearchFindsABackendPublishedByAnOwner(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{"mycorp": {"infrena-backend-s3", "infrena-provider-aws", "website"}},
		tags: map[string]string{
			"mycorp/infrena-backend-s3": "v1.0.0",
		},
		files: map[string][]byte{
			"mycorp/infrena-backend-s3@v1.0.0": manifestBytes(t, "s3", "1.0.0"),
		},
	}

	got, errs := Search(context.Background(), f, []Source{mustSource(t, "github.com/mycorp")}, "s3", hereEnv())
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Role != RoleBackend {
		t.Errorf("Role = %v, want RoleBackend", got[0].Role)
	}
}

// A provider and a backend of one name both survive. This is the collision
// the whole plan is about, and neither may be dropped.
func TestAProviderAndABackendOfOneNameBothSurvive(t *testing.T) {
	f := &fakeFetcher{
		repos: map[string][]string{"mycorp": {"infrena-provider-s3", "infrena-backend-s3"}},
		tags: map[string]string{
			"mycorp/infrena-provider-s3": "v2.0.0",
			"mycorp/infrena-backend-s3":  "v1.0.0",
		},
		files: map[string][]byte{
			"mycorp/infrena-provider-s3@v2.0.0": manifestBytes(t, "s3", "2.0.0"),
			"mycorp/infrena-backend-s3@v1.0.0":  manifestBytes(t, "s3", "1.0.0"),
		},
	}

	got, _ := Search(context.Background(), f, []Source{mustSource(t, "github.com/mycorp")}, "s3", hereEnv())

	if len(got) != 2 {
		t.Fatalf("got %d candidates, want both: %+v", len(got), got)
	}
	roles := map[Role]bool{}
	for _, c := range got {
		roles[c.Role] = true
	}
	if !roles[RoleProvider] || !roles[RoleBackend] {
		t.Errorf("roles found = %v, want both", roles)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/plugins/ -run 'TestABackendRepository|TestAProviderRepositoryStill|TestARepositoryMatchingNeither|TestAnOwnerSource|TestSearchFindsABackend|TestAProviderAndABackend' -v`
Expected: FAIL — `Role` undefined, and `ParseSource` refuses the backend repository.

- [ ] **Step 3: Write minimal implementation**

```go
// Role is what a plugin DOES: serve resources, or store state.
//
// NOT called Kind, because Kind is taken in this package and means something
// else entirely — whether a source names an owner or one repository. The
// user-facing flag is still `--kind`, since that is the word a person types.
type Role int
```

`ParseSource` tries both prefixes and records which matched. An owner source leaves `Role` at its zero value and the field's doc says it is meaningless there — an owner publishes both. The refusal message offers both shapes.

In `search.go`, the owner listing keeps repositories matching **either** prefix, and the candidate carries the role the repository name implies.

**The manifest still decides the match.** A repository called `infrena-backend-s3` whose manifest says `name: something-else` is not a match for `s3`, exactly as before. The prefix decides the ROLE; the manifest decides the NAME.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/plugins/... -v 2>&1 | tail -5`
Expected: PASS, including `TestPluginsPackageCannotReachTheNetwork`.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/plugins/...
git add internal/plugins
git commit -m "Let a source name a backend repository

A backend was refused when it was written, not merely missed by a search,
because the parser required the provider prefix. An owner search now lists
both, and a candidate carries what the repository name says it is."
```

---

## Task 2: `list` and `search` say which kind

**Files:**
- Modify: `internal/cli/plugins.go`
- Test: `internal/cli/plugins_search_test.go`, `internal/cli/plugins_list_test.go`

**Interfaces:**
- Consumes: Task 1's `Candidate.Role`, `Role.String()`.
- Produces: no new symbols.

- [ ] **Step 1: Write the failing test**

```go
// Search is exploratory. If you ask what is called s3 and there is a
// provider and a backend, both is the honest answer, and the column is what
// makes it readable rather than confusing.
func TestSearchShowsTheKindOfEachCandidate(t *testing.T) {
	dir := newProjectFixture(t)
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, _, code := runCommand(t, dir, "plugins", "search", "s3")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "KIND") {
		t.Errorf("no kind column:\n%s", stdout)
	}
	for _, want := range []string{"provider", "backend"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not show %q:\n%s", want, stdout)
		}
	}
}

// list answers "what am I actually running", and with backends installable
// that answer now has two kinds in it. Inferred from the binary name, which
// is already distinct: infrena-plugin-<name> against infrena-backend-<name>.
func TestListShowsTheKindOfEachInstalledPlugin(t *testing.T) {
	dir := newProjectWithInstalledPluginAndBackend(t)

	stdout, _, code := runCommand(t, dir, "plugins", "list")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "KIND") {
		t.Errorf("no kind column:\n%s", stdout)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestSearchShowsTheKind|TestListShowsTheKind' ./internal/cli/ -v`
Expected: FAIL — no KIND column.

- [ ] **Step 3: Write minimal implementation**

Add the column to both tabwriter tables. `list` infers the role from the binary's prefix — `infrena-backend-` means backend, `infrena-plugin-` means provider — which is already unambiguous on disk and needs no new state.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./internal/cli/ 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli
git commit -m "Show whether a plugin is a provider or a backend

Both can carry one name, so a list of two things called s3 is unreadable
without saying which is which."
```

---

## Task 3: Install asks the project, not the user

**FOUR THINGS ESTABLISHED AFTER THIS PLAN WAS WRITTEN — read before starting.**

**1. A backend release is NOT named like a provider release, and the fixture in the tree guesses that it is.** Verified against `infrena-backend-s3`'s own `scripts/build-release`:

```
infrena-backend-s3_<version>_<goos>_<goarch>.tar.gz     (.zip on windows)
   └── infrena-backend-s3_<version>_<goos>_<goarch>/
         └── infrena-backend-s3
```

`remote.AssetName` hardcodes an `infrena-plugin-` prefix, so for a backend it constructs `infrena-plugin-s3_<version>_...` — **an asset no release publishes.** `AssetName` must become role-aware, and so must the binary name (`pluginhost.BinaryName` against `backendhost.BinaryName`) and the install destination. `plugins_install.go:118` is the call site.

**Fix `fakeGitHubServingBothKinds` in `plugins_install_test.go` to serve the real backend archive name**, not the guessed one. A fake that answers whatever the code asks for validates any implementation, right or wrong — that is exactly how five bugs shipped through a fake GitHub two days ago.

**2. A helper name already collides.** `newProjectWithBackend(t, block string)` exists in `backend_test.go` and takes a YAML block, not a name. This task's tests call it as `newProjectWithBackend(t, "s3")`. Rename one of them; say which and why.

**3. Install must keep writing the lock entry `list` reads.** `plugins list` reads a backend's version from `plugins.lock` under `backendhost.LockKey(name)` — that is `"infrena-backend-" + name`, not the bare name, so a provider `s3` and a backend `s3` cannot collide on one entry.

**4. `RepoPrefix` survives** as a deprecated alias of `ProviderRepoPrefix`, because `plugins_install_test.go` still used it. New code uses `ProviderRepoPrefix` / `BackendRepoPrefix`.



**Files:**
- Create: `internal/cli/plugins_project.go`
- Modify: `internal/cli/plugins_install.go`
- Test: `internal/cli/plugins_install_test.go`

**Interfaces:**
- Consumes: Task 1's `Role`; `config.ProjectDecl.NeededPlugins() []string`; `config.ProjectDecl.Backend.Plugin string`.
- Produces:
  - `func declaredRoles(dir, name string) ([]plugins.Role, error)` — which roles the project declares for this name; empty when the project declares neither, or when there is no project
  - `--kind provider|backend` on `plugins install`

- [ ] **Step 1: Write the failing test**

```go
// The project already knows. A name declared only as a backend installs the
// backend, without asking and without a flag.
func TestInstallResolvesTheKindFromTheProject(t *testing.T) {
	dir := newProjectWithBackend(t, "s3")
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, _, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	// It must say which it chose, because it chose without being told.
	if !strings.Contains(stdout, "backend") {
		t.Errorf("output does not say which kind was installed:\n%s", stdout)
	}
	if !installedBinaryExists(t, dir, "infrena-backend-s3") {
		t.Error("the backend binary was not installed")
	}
	if installedBinaryExists(t, dir, "infrena-plugin-s3") {
		t.Error("a provider was installed and nothing asked for one")
	}
}

// A project naming both needs both. That is not choosing between them, it is
// doing what was asked.
func TestInstallInstallsBothWhenTheProjectNeedsBoth(t *testing.T) {
	dir := newProjectWithProviderAndBackend(t, "s3")
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, _, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !installedBinaryExists(t, dir, "infrena-backend-s3") || !installedBinaryExists(t, dir, "infrena-plugin-s3") {
		t.Error("a project naming both did not get both")
	}
}

// With no project to ask and both kinds published, infrena must NOT pick. It
// is the same rule as two owners answering one name: choosing silently gives
// the user a thing they did not name.
func TestInstallRefusesToGuessWhenThereIsNoProjectToAsk(t *testing.T) {
	dir := t.TempDir() // deliberately not a project
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3", "--global")

	if code == ExitOK {
		t.Fatal("install guessed a kind with nothing to go on")
	}
	for _, want := range []string{"provider", "backend", "--kind"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal does not offer %q:\n%s", want, stderr)
		}
	}
}

// --kind is the fallback for exactly that case.
func TestKindFlagResolvesTheAmbiguity(t *testing.T) {
	dir := t.TempDir()
	srv := fakeGitHubServingBothKinds(t, "s3")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, _, code := runCommandWithStdin(t, dir, "", "plugins", "install", "s3", "--global", "--kind", "backend")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
}

// An unambiguous name needs no flag and no project: only one thing is called
// hetzner, so there is nothing to disambiguate.
func TestAnUnambiguousNameNeedsNoProjectAndNoFlag(t *testing.T) {
	dir := t.TempDir()
	srv := fakeGitHubServingOneBackend(t, "hetzner")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, _, code := runCommandWithStdin(t, dir, "", "plugins", "install", "hetzner", "--global")

	if code != ExitOK {
		t.Fatalf("a name with only one candidate was refused: exit %d", code)
	}
}

// --kind naming something that does not exist is an error saying so, not a
// silent fall back to the other kind.
func TestKindFlagForAKindThatDoesNotExistIsAnError(t *testing.T) {
	dir := t.TempDir()
	srv := fakeGitHubServingOneBackend(t, "hetzner")
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "hetzner", "--global", "--kind", "provider")

	if code == ExitOK {
		t.Fatal("asking for a provider installed a backend")
	}
	if !strings.Contains(stderr, "backend") {
		t.Errorf("error does not say what DOES exist:\n%s", stderr)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestInstallResolves|TestInstallInstallsBoth|TestInstallRefusesToGuess|TestKindFlag|TestAnUnambiguousName' ./internal/cli/ -v`
Expected: FAIL — no `--kind` flag, no project resolution.

- [ ] **Step 3: Write minimal implementation**

`declaredRoles` decodes `infra.yml` — **decode, never compile**, the same rule `backendFor` follows, because a project with a broken resource must still be able to install the plugin that would fix it. It returns `RoleProvider` when `NeededPlugins()` contains the name, `RoleBackend` when `Backend.Plugin` equals it, both when both, and nothing when neither or when there is no project.

Resolution order in `runInstall`, and each rung exists for a reason worth keeping:

```go
// Which kind to install, in the order the answer is cheapest and most
// certain:
//
//  1. --kind, if given. The user said so; nothing outranks that.
//  2. What the project declares. It already knows, and asking a user to
//     repeat something written in infra.yml is asking them to keep two
//     places in step.
//  3. The search result, if only ONE kind answers the name. Nothing to
//     disambiguate.
//  4. Otherwise refuse, showing both, naming --kind. §31.3: never auto-pick
//     between two candidates answering one name.
```

When the project resolved it, **say which kind was installed** — a choice made on the user's behalf has to be visible.

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli
git commit -m "Install the kind the project asked for

A project already says whether it needs a provider or a backend of a given
name, so asking the user to repeat it would be asking them to keep two
places in step. With no project to ask and both kinds published, it
refuses rather than choosing."
```

---

## Task 4: `plugins install` with no name

**Files:**
- Modify: `internal/cli/plugins_install.go`
- Test: `internal/cli/plugins_install_test.go`

**Interfaces:**
- Consumes: Task 3's `declaredRoles`; `config.ProjectDecl.NeededPlugins()`; `ProjectDecl.Backend.Plugin`.
- Produces: `Args: cobra.MaximumNArgs(1)` on `plugins install`.

- [ ] **Step 1: Write the failing test**

```go
// A fresh clone needs every plugin the project names. One command, and it is
// unambiguous by construction, because the project says both the names and
// the kinds.
func TestBareInstallInstallsEverythingTheProjectDeclares(t *testing.T) {
	dir := newProjectNeeding(t, []string{"aws", "fake"}, "s3")
	srv := fakeGitHubServingAll(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, _, code := runCommandWithStdin(t, dir, "", "plugins", "install")

	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, bin := range []string{"infrena-plugin-aws", "infrena-plugin-fake", "infrena-backend-s3"} {
		if !installedBinaryExists(t, dir, bin) {
			t.Errorf("%s was not installed:\n%s", bin, stdout)
		}
	}
}

// Already installed is not an error and is not a reinstall: the point is to
// reach a working state, and a second run must be safe.
func TestBareInstallSkipsWhatIsAlreadyInstalled(t *testing.T) {
	dir := newProjectNeeding(t, []string{"aws"}, "")
	srv := fakeGitHubServingAll(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	runCommandWithStdin(t, dir, "", "plugins", "install")
	stdout, _, code := runCommandWithStdin(t, dir, "", "plugins", "install")

	if code != ExitOK {
		t.Fatalf("second run exit = %d", code)
	}
	if !strings.Contains(stdout, "already installed") {
		t.Errorf("second run does not say it skipped:\n%s", stdout)
	}
}

// A project declaring nothing is not a failure. It is a project that needs
// no plugins, which is a real state — `init` produces one.
func TestBareInstallOnAProjectNeedingNothingSaysSo(t *testing.T) {
	dir := newProjectFixture(t)

	stdout, _, code := runCommandWithStdin(t, dir, "", "plugins", "install")

	if code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "nothing") && !strings.Contains(stdout, "No plugins") {
		t.Errorf("output does not say there was nothing to do:\n%s", stdout)
	}
}

// With no project there is nothing to enumerate, so the error says that
// rather than reporting an empty list as success.
func TestBareInstallWithNoProjectIsAnError(t *testing.T) {
	dir := t.TempDir()

	_, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install", "--global")

	if code == ExitOK {
		t.Fatal("bare install succeeded with no project to read")
	}
	if !strings.Contains(stderr, "infra.yml") {
		t.Errorf("error does not name what is missing:\n%s", stderr)
	}
}

// One failure must not silently lose the others. Report what happened to
// each and fail overall — a partial result a reader mistakes for a complete
// one is the failure this project avoids everywhere.
func TestBareInstallReportsEveryPluginEvenWhenOneFails(t *testing.T) {
	dir := newProjectNeeding(t, []string{"aws", "nonesuch"}, "")
	srv := fakeGitHubServingAll(t)
	t.Setenv("INFRENA_GITHUB_API", srv.URL)

	stdout, stderr, code := runCommandWithStdin(t, dir, "", "plugins", "install")

	if code == ExitOK {
		t.Fatal("a failed install reported success")
	}
	if !installedBinaryExists(t, dir, "infrena-plugin-aws") {
		t.Error("the plugin that could be installed was not")
	}
	if !strings.Contains(stdout+stderr, "nonesuch") {
		t.Errorf("the failure is not named:\n%s\n%s", stdout, stderr)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestBareInstall' ./internal/cli/ -v`
Expected: FAIL — `install` takes exactly one argument.

- [ ] **Step 3: Write minimal implementation**

`Args: cobra.MaximumNArgs(1)`. With no argument, enumerate `NeededPlugins()` as providers plus `Backend.Plugin` as a backend, install each through the same path a named install uses, skip what is present, and report per plugin. **Keep going after a failure and fail at the end**, following `discovery.Walk`'s rule about partial answers.

```go
// Installing what the project declares needs no kind resolution at all: the
// project says both the name AND the kind for each one, so the ambiguity
// Task 3 exists to resolve cannot arise here.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli
git commit -m "Install everything a project declares

A fresh clone needs every plugin the project names, and the project says
both the names and the kinds, so there is nothing to disambiguate. One
failure does not hide the others."
```

---

## Task 5: Documentation

**Files:**
- Modify: `PLAN.md` (§31.3), `CLAUDE.md`

- [ ] **Step 1: Update `PLAN.md` §31.3**

The naming convention covers both kinds: `infrena-provider-<name>` shipping `infrena-plugin-<name>`, and `infrena-backend-<name>` shipping `infrena-backend-<name>`. An owner search lists both. Record that the ambiguity is confined to the CLI's bare name argument — configuration, binaries on disk, lock keys and not-found errors already distinguish the two — and that install resolves it from the project first, `--kind` second, a single candidate third, and refuses fourth.

Add `infrena plugins install` with no argument to the command list.

- [ ] **Step 2: Update `CLAUDE.md`**

The four-rung resolution order and why the project outranks a flag prompt; that `plugins.Kind` means owner-versus-repository while `plugins.Role` means provider-versus-backend, so the next reader does not conflate them.

- [ ] **Step 3: Verify**

Run: `INFRENA_REQUIRE_PLUGIN=1 go test -count=1 ./... && go vet ./... && gofmt -l .`
Expected: all clean, integration suite RUNS rather than skipping.

- [ ] **Step 4: Commit**

```bash
git add PLAN.md CLAUDE.md
git commit -m "Document installing backends and the bare install"
```
