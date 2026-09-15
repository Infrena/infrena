# Unit C — `init` and the Project Root Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `infrena init` scaffolds a real project layout in `./infrena`, and every command finds it without `--chdir`.

**Architecture:** Root discovery is one function called from the one place `--chdir` is already resolved, so a command added later inherits it. `init` gains an optional directory argument and a `--provider` flag, and its scaffold becomes the §4 layout rather than two files.

**Tech Stack:** Go 1.27, Cobra, `gopkg.in/yaml.v3`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-15-cli-output-discover-init-design.md` (§4)

## Global Constraints

- Go 1.27.0 module floor; `mise` is not active in non-interactive shells.
- **Third-party budget is two packages.**
- **What `init` writes must pass `infrena validate` immediately.** An init whose output does not validate is worse than no init at all: it teaches the language wrongly at the one moment a user has no way to tell.
- **A shipped infrena carries no provider.** The scaffold must validate on a machine with no plugin installed.
- **`init` refuses to overwrite**, checked across every file before anything is written, so a refusal leaves the directory exactly as it was.
- `config.ProjectFileName` is `"infra.yml"`. It did not change in the rename, and neither did `.infra/`.
- `gofmt -l .` and `go vet ./...` clean before each commit.
- Commit messages: plain English, no em-dashes, minimal, never mention Claude or AI.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/cli/root.go` | Resolve the project root once, where `--chdir` is already handled. |
| `internal/cli/projectroot.go` *(new)* | `findProjectRoot`. |
| `internal/cli/projectroot_test.go` *(new)* | The three cases and the tie. |
| `internal/cli/init.go` | Directory argument, `--provider`, the §4 layout. |
| `internal/cli/init_test.go` | Scaffold shape, validation, refusal. |

---

## Task 1: Finding the project root

**Files:**
- Create: `internal/cli/projectroot.go`, `internal/cli/projectroot_test.go`
- Modify: `internal/cli/root.go` (`PersistentPreRunE`)

**Interfaces:**
- Consumes: `config.ProjectFileName` (`"infra.yml"`); `GlobalOptions.Dir` (the `--chdir` value, default `"."`).
- Produces: `func findProjectRoot(dir string) (string, error)`.

- [ ] **Step 1: Write the failing test**

```go
func TestFindProjectRootPrefersTheClosestProject(t *testing.T) {
	// A directory holding both is a project at its root that also happens to
	// have a directory called infrena. The root is the answer.
	dir := t.TempDir()
	write(t, filepath.Join(dir, "infra.yml"), "project: outer\n")
	write(t, filepath.Join(dir, "infrena", "infra.yml"), "project: inner\n")

	got, err := findProjectRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("findProjectRoot = %q, want %q", got, dir)
	}
}

func TestFindProjectRootDescendsIntoInfrena(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "infrena", "infra.yml"), "project: p\n")

	got, err := findProjectRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "infrena") {
		t.Errorf("findProjectRoot = %q, want the infrena subdirectory", got)
	}
}

// Not walking up is deliberate: a command run in a subdirectory quietly
// mutating a project the user did not realise they were in is the wrong
// place for that kind of convenience.
func TestFindProjectRootDoesNotWalkUp(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "infra.yml"), "project: p\n")
	sub := filepath.Join(dir, "deep", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := findProjectRoot(sub); err == nil {
		t.Fatal("findProjectRoot walked up to a parent project")
	}
}

// Section 44: an error says what was expected and what to do.
func TestFindProjectRootNamesBothPlacesItLooked(t *testing.T) {
	_, err := findProjectRoot(t.TempDir())
	if err == nil {
		t.Fatal("findProjectRoot accepted a directory with no project")
	}
	for _, want := range []string{"infra.yml", "infrena/infra.yml", "infrena init"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestFindProjectRoot' ./internal/cli/ -v`
Expected: FAIL — `findProjectRoot` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// findProjectRoot resolves where a command actually runs: dir itself when it
// holds infra.yml, else dir/infrena when that does.
//
// CLOSEST WINS. A directory holding both is a project at its root that also
// has a directory called infrena, and the root is the answer.
//
// IT DOES NOT WALK UP, deliberately. Walking up means a command run in a
// subdirectory silently operates on a project the user may not have realised
// they were in, and state mutation is the wrong place for that convenience.
//
// An explicit --chdir never reaches here: it means what it says, and a user
// who named a directory has already answered this question.
func findProjectRoot(dir string) (string, error)
```

In `root.go`'s `PersistentPreRunE`, resolve `opts.Dir` through it **only when `--chdir` was not set explicitly** — use `cmd.Flags().Changed("chdir")`. `init` must be exempt, since it runs where no project exists yet; skip the resolution for that command by name.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/cli/
git add internal/cli/
git commit -m "Find the project in the current directory or in ./infrena

So infrastructure can live beside the application without every command
needing a chdir flag. It does not search parent directories, because a
command run deep in a tree should not quietly mutate a project the user
did not know they were in."
```

---

## Task 2: The scaffold

**Files:**
- Modify: `internal/cli/init.go`
- Test: `internal/cli/init_test.go`

**Interfaces:**
- Consumes: Task 1's `findProjectRoot` (for the closing message only).
- Produces:
  - `func scaffoldFiles(provider string) map[string]string`
  - `infrena init [dir]` with `--provider`.

- [ ] **Step 1: Write the failing test**

```go
func TestInitScaffoldsIntoInfrenaByDefault(t *testing.T) {
	dir := t.TempDir()

	if _, _, code := runCommandIn(t, dir, "init"); code != ExitOK {
		t.Fatalf("init exit = %d", code)
	}

	for _, want := range []string{
		"infrena/infra.yml",
		"infrena/resources/network.yml",
		"infrena/vars/default.yml",
		"infrena/vars/production.yml",
		"infrena/vars/staging.yml",
		"infrena/modules/.gitkeep",
		"infrena/.gitignore",
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("init did not create %s", want)
		}
	}
}

// The promise init has always made, and the one that matters most: a shipped
// infrena carries no provider, so the scaffold must validate on a machine
// with nothing installed. That is why the example resource is commented out.
func TestScaffoldValidatesWithNoPluginInstalled(t *testing.T) {
	dir := t.TempDir()
	runCommandIn(t, dir, "init")

	_, stderr, code := runCommandInWithoutPlugins(t, dir, "validate")

	if code != ExitOK {
		t.Errorf("scaffold does not validate bare: exit %d\n%s", code, stderr)
	}
}

// Environments must match the vars filenames, or vars/staging.yml applies to
// nothing and the one file a user reads to learn the language is dead
// configuration.
func TestScaffoldedEnvironmentsMatchTheVarsFiles(t *testing.T) {
	dir := t.TempDir()
	runCommandIn(t, dir, "init")

	body := read(t, filepath.Join(dir, "infrena", "infra.yml"))
	for _, env := range []string{"production", "staging"} {
		if !strings.Contains(body, env+":") {
			t.Errorf("infra.yml does not declare %s", env)
		}
	}
}

func TestInitTakesADirectoryArgument(t *testing.T) {
	dir := t.TempDir()

	runCommandIn(t, dir, "init", "myinfra")

	if _, err := os.Stat(filepath.Join(dir, "myinfra", "infra.yml")); err != nil {
		t.Errorf("init ignored its directory argument: %v", err)
	}
}

// A directory findProjectRoot does not discover must be told the flag, or the
// closing message is advice that does not work from where the user stands.
func TestClosingMessageCarriesChdirForAnUndiscoverableDirectory(t *testing.T) {
	dir := t.TempDir()

	stdout, _, _ := runCommandIn(t, dir, "init", "myinfra")

	if !strings.Contains(stdout, "--chdir myinfra") {
		t.Errorf("closing message omits the flag:\n%s", stdout)
	}
}

func TestInitWithProviderAWSScaffoldsARealProvidersBlock(t *testing.T) {
	dir := t.TempDir()

	runCommandIn(t, dir, "init", "--provider", "aws")

	body := read(t, filepath.Join(dir, "infrena", "infra.yml"))
	if !strings.Contains(body, "plugin: aws") {
		t.Errorf("no aws provider instance:\n%s", body)
	}
}

// Refusing matters more here than anywhere else in the CLI, and it is checked
// before anything is written so a refusal leaves the directory as it was.
func TestInitRefusesToOverwriteAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	runCommandIn(t, dir, "init")
	before := read(t, filepath.Join(dir, "infrena", "infra.yml"))
	if err := os.Remove(filepath.Join(dir, "infrena", "vars", "staging.yml")); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCommandIn(t, dir, "init")

	if code == ExitOK {
		t.Error("init overwrote an existing project")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("refusal does not say why:\n%s", stderr)
	}
	if read(t, filepath.Join(dir, "infrena", "infra.yml")) != before {
		t.Error("init modified infra.yml before refusing")
	}
	// The half-scaffolded case: the file removed above must NOT come back.
	if _, err := os.Stat(filepath.Join(dir, "infrena", "vars", "staging.yml")); err == nil {
		t.Error("init wrote a file despite refusing")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestInit|TestScaffold|TestClosingMessage' ./internal/cli/ -v`
Expected: FAIL — init scaffolds two files into the current directory and takes no arguments.

- [ ] **Step 3: Write minimal implementation**

`Args: cobra.MaximumNArgs(1)`, defaulting to `"infrena"`. Add `--provider` (default `""`).

`scaffoldFiles(provider)` returns the §4 layout. `infra.yml` declares `project`, the `infrena: ">= 0.7"` floor, `environments: production, staging`, and — when `provider == "aws"` — a `providers:` block naming `plugin: aws`. `resources/network.yml` carries the example, **commented out** unless `--provider` named one:

```go
// The example is COMMENTED OUT by default, and that is a change from the
// scaffold that shipped before.
//
// A shipped infrena carries no provider. The old scaffold declared a live
// `fake.network`, so a fresh init only validated on a machine that happened
// to have the fake plugin installed — and what init writes must pass
// `infrena validate` immediately, or it teaches the language wrongly at the
// one moment a user has no way to tell.
//
// Commented out it validates bare and still shows the shape. With --provider
// the resource is live, because naming a provider is the user saying they
// have one.
```

The closing message names the next commands, adding `--chdir <dir>` when the chosen directory is not one `findProjectRoot` discovers (anything but `infrena` or `.`).

Keep the existing pre-write refusal loop and extend it to every new path.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ 2>&1 | tail -20`
Expected: PASS. The existing `init_test.go` assertions about the two-file scaffold are **correct to update**.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli/
git commit -m "Scaffold a real project layout

Init wrote two files into the current directory. It now writes the layout
the docs describe, into ./infrena by default, so infrastructure sits beside
the application. The example resource is commented out because a shipped
infrena carries no provider and what init writes has to validate straight
away."
```

---

## Task 3: Documentation

**Files:**
- Modify: `PLAN.md` (§4, §37), `CLAUDE.md`

- [ ] **Step 1: Update `PLAN.md` §4**

Record that a project is found at `./infra.yml` or `./infrena/infra.yml`, closest first, with no upward search, and that `--chdir` skips the search entirely.

- [ ] **Step 2: Update `PLAN.md` §37**

`init` takes an optional directory (default `infrena`) and `--provider`.

- [ ] **Step 3: Update `CLAUDE.md`**

The M6 line says "`init` scaffolds a project that validates immediately and refuses to overwrite". Extend it with the layout, the default directory, and the reason the example is commented out.

- [ ] **Step 4: Verify**

Run: `go test ./... && go vet ./... && gofmt -l .`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add PLAN.md CLAUDE.md
git commit -m "Document the project root rule and the init layout"
```
