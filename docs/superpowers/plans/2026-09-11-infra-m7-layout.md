# M7 — Conventional Directories and Scoped Variables

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development.

**Goal:** A project is organised by putting files where they belong. `resources/**`, `vars/**`,
`modules/**` and `discovered/**` are read automatically, and a resource directory may carry its
own `vars/` visible only to the resources declared there.

**Architecture:** `internal/config.Load` grows a conventional-directory walk. Variable
resolution gains one rung between base configuration and module defaults. Nothing downstream of
stage 4 changes — a scoped variable is a variable by the time the compiler binds anything.

**Tech Stack:** Go 1.24, Cobra, `gopkg.in/yaml.v3`. No new dependency.

**Spec:** `PLAN.md` §4.1 (the layout), §7 (precedence, amended), §10 (why `templates/` is
reserved rather than built).

## Global Constraints

- **Exactly two third-party dependencies.** `git diff --stat <BASE> HEAD -- go.mod go.sum` must
  print nothing.
- **`internal/config` is the ONLY package permitted to touch `yaml.Node`.**
- **Exactly one redaction path**, `pkg/value.Format`.
- `mise` is inactive in non-interactive shells; `-count=1` on every test run.
- `git add <explicit paths>` then `git commit -m "..." -- <same paths>`. No `-A`, no `.`, no `-am`.
- Diagnostics say what is wrong, where, what was expected, and an action that works today.
- Anything user-observable is sorted once — never Go's map order.
- **Assert the thing did not happen.** **A fixture must contradict its expected output.**
- A sabotage must leave the code compiling and change behaviour.

## The rule that governs this whole milestone

**A name defined twice at the same level is an ERROR naming both files.** Not last-one-wins, not
first-one-wins. Globbing makes accidental duplication easy in a way a single file does not, and
two declarations silently becoming one is the failure this language refuses everywhere else —
it is why module load names collide loudly, why a duplicate resource key is refused, and why
`decodeResources` tracks `seenKeys`.

This applies to resources across `resources/**`, to variables across `vars/**`, and to a
variable defined in both `variables.yml` and `vars/**`. It does NOT apply across LEVELS: a
directory-scoped variable shadowing a project-wide one is the feature, not a collision.

---

## Task 1: glob `resources/**` and `vars/**`

**Files:**
- Modify: `internal/config/load.go`
- Test: `internal/config/load_test.go`

**Interfaces:**
- Produces: `ResourcesDirName = "resources"`, `VarsDirName = "vars"`,
  `DiscoveredDirName = "discovered"`, and a `walkConventionalDir` helper the later tasks reuse.

- [ ] **Step 1.1: Failing tests**

```go
// TestResourcesDirectoryIsLoaded — a resource declared under resources/ is as real as one in
// infra.yml, and the two forms coexist.
func TestResourcesDirectoryIsLoaded(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "infra.yml", "project: p\nresources:\n  inline:\n    type: test.network\n    cidr: 10.0.0.0/16\n")
	write(t, dir, "resources/database/database.yml",
		"resources:\n  store:\n    type: test.database\n    engine: postgres\n")

	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Assert BOTH: a loader that replaced infra.yml's resources rather than adding to them
	// would pass a test that only looked for `store`.
	if !loaded(files, "database.yml") {
		t.Error("resources/** was not loaded")
	}
	if !loaded(files, "infra.yml") {
		t.Error("infra.yml's own resources: block stopped being read")
	}
}

// TestNestedResourceDirectoriesAreLoaded — `resources/**`, not `resources/*`.
func TestNestedResourceDirectoriesAreLoaded(t *testing.T) { /* resources/eu/west/net.yml */ }

// TestVarsDirectoryIsLoadedAsBaseConfiguration — a value in vars/** behaves exactly as one in
// variables.yml, because §7 puts them on the same rung.
func TestVarsDirectoryIsLoadedAsBaseConfiguration(t *testing.T) { /* ... */ }

// TestConventionalDirectoriesAreSortedOnce — two files per directory whose declaration order
// differs from sorted order, run 20 times. A project whose file order changes between runs
// breaks invariant 6 before the planner sees it.
func TestConventionalDirectoriesAreSortedOnce(t *testing.T) { /* zeta.yml, alpha.yml */ }

// TestAMissingConventionalDirectoryIsFine — most projects have none of them.
func TestAMissingConventionalDirectoryIsFine(t *testing.T) { /* ... */ }

// TestANonYamlFileInAConventionalDirectoryIsIgnored — a README, a .gitkeep, an editor
// backup. Refusing to load a project because someone left notes in it is hostile.
func TestANonYamlFileInAConventionalDirectoryIsIgnored(t *testing.T) { /* README.md, .gitkeep */ }
```

- [ ] **Step 1.2: Run, see them fail.**
- [ ] **Step 1.3: Minimal code.** One `walkConventionalDir(root, kind)` used by all of them;
  `.yml` only, sorted, missing directory not an error, nested included. Do NOT write a second
  walk for each directory — two walks is two orderings.
- [ ] **Step 1.4: Run, see them pass.**
- [ ] **Step 1.5: Discrimination.** Remove the sort, run the ordering test 20×. Then make the
  walk non-recursive and watch the nested test fail.
- [ ] **Step 1.6: Commit.**

---

## Task 2: a name declared twice is an error

**Files:** Modify `internal/config/decode.go`; test in `internal/config/decode_test.go`.

- [ ] **Step 2.1: Failing tests**

```go
// TestTheSameResourceInTwoFilesIsAnError. Globbing makes this easy to do by accident, and
// silently taking one is how a user deploys something they did not write.
func TestTheSameResourceInTwoFilesIsAnError(t *testing.T) {
	// resources/a/x.yml and resources/b/x.yml both declare `store`.
	// Assert the diagnostic names BOTH paths, and assert NEITHER survives — a diagnostic
	// alone leaves one in the set for anything that does not check errors first.
}

// TestTheSameVariableInVarsAndVariablesYmlIsAnError — same rung, so it is a collision, not
// an override.
func TestTheSameVariableInVarsAndVariablesYmlIsAnError(t *testing.T) { /* ... */ }

// TestADirectoryScopedVariableShadowingAProjectOneIsNOTAnError is the boundary, and the half
// that keeps the rule honest: shadowing across LEVELS is the feature. A check that refused
// it would break the thing this milestone exists to add.
func TestADirectoryScopedVariableShadowingAProjectOneIsNOTAnError(t *testing.T) { /* ... */ }
```

- [ ] **Steps 2.2-2.6.** The accept case in the third test is load-bearing: the cheapest
  implementation of "duplicates are an error" refuses shadowing too, and would pass the first
  two tests.

---

## Task 3: directory-scoped variables

**Files:** Modify `internal/variables/`, `internal/compiler/`; tests in both.

**Interfaces:**
- Consumes: Task 1's walk, Task 2's collision rule.
- Produces: a new `value.Scope` constant between `ScopeBaseConfig` and `ScopeModuleDefault`,
  and its label in `Scope.String()` — the single label table.

- [ ] **Step 3.1: Failing tests**

```go
// TestADirectoryScopedVariableBeatsTheProjectWideOne — §7's new rung.
func TestADirectoryScopedVariableBeatsTheProjectWideOne(t *testing.T) { /* ... */ }

// TestADirectoryScopedVariableIsNotVisibleElsewhere is the SCOPING half, and the one that
// makes it worth having. Assert a resource in ANOTHER directory reports the variable as
// undefined — asserting only that the right value appears in the right place passes against
// an implementation that made every scoped var global.
func TestADirectoryScopedVariableIsNotVisibleElsewhere(t *testing.T) { /* ... */ }

// TestAnEnvironmentBeatsADirectoryScopedVariable — the two axes. A directory is how the
// project is organised; an environment is where it is deployed. If a directory outranked an
// environment, production could no longer tune a value the code set locally.
func TestAnEnvironmentBeatsADirectoryScopedVariable(t *testing.T) { /* ... */ }

// TestDashDashVarBeatsEverything — including a scoped variable. The operator saying what they
// want right now is the one rung no file outranks.
func TestDashDashVarBeatsEverything(t *testing.T) { /* ... */ }

// TestThePlanNamesTheScopeAValueCameFrom — provenance. `[variable, from resources/db/vars]`
// or the label the scope table gives it; a new rung that renders as an existing one is a
// rung a user cannot tell apart.
func TestThePlanNamesTheScopeAValueCameFrom(t *testing.T) { /* ... */ }
```

- [ ] **Steps 3.2-3.6.** Sabotage each rung boundary separately: make the scoped rung lose to
  base config, then make it beat the environment. Each must fail its own test and no other.

---

## Task 4: the whole layout, through the binary

**Files:** Create `tests/integration/m7_layout_test.go`; update `examples/shop` to use the
directory form; update `CLAUDE.md`.

- [ ] A project using `resources/**`, `vars/**`, a scoped `vars/`, and `modules/**` together,
      planned and applied through the built binary.
- [ ] The same project expressed entirely in `infra.yml` produces an EQUIVALENT plan — the
      directory form is organisation, not semantics, and a difference between them is a bug.
- [ ] A duplicate across two files fails `validate` naming both paths.
- [ ] `templates/` present and unread: a project with one must validate, because §4.1 reserves
      the name without implementing it.
- [ ] Update `examples/shop` to the directory layout, and check
      `TestTheShopExampleStillWorks` still asserts what its README claims.
