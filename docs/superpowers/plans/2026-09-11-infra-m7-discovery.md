# M7 — Discovery and Import Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to
> implement this plan task-by-task.

**Goal:** Point the tool at infrastructure that already exists, work out what is there, adopt it
into state, and write the configuration that would have produced it — closing invariant 3, the
last of the six with no test.

**Architecture:** A new `internal/discovery` walks providers and names what it finds; a new
`internal/generator` renders resources to minimal YAML; the fake provider learns to enumerate
and import its own JSON cloud. Three commands — `discover`, `import`, `export` — sit on top.
The compiler learns to load `discovered/*.yml`, which is what makes import safe.

**Tech Stack:** Go 1.24, Cobra, `gopkg.in/yaml.v3`. Still the entire third-party budget; AWS
arrives in Phase 3, not here.

**Spec:** `PLAN.md` §25 (explorer), §26 (import), §27 + §27.1 + §27.2 (generation, naming,
where it lands), §28 (export), §29 (round trip), §50 (the component list).

## Global Constraints

Every task's requirements implicitly include this section.

- **Exactly two third-party dependencies**: `github.com/spf13/cobra` and `gopkg.in/yaml.v3`.
  `git diff --stat <BASE> HEAD -- go.mod go.sum` must print nothing.
- **`internal/config` is the ONLY package permitted to touch `yaml.Node`.** The GENERATOR is the
  one exception and it must be argued for: it EMITS yaml, and emitting is not parsing. Prefer
  `yaml.Marshal` over hand-built nodes, and if a node is unavoidable keep it inside
  `internal/generator`. This check must still print nothing outside those two:
  ```bash
  grep -rn "yaml\." --include=*.go internal/ pkg/ cmd/ | grep -v "^internal/config/" | grep -v "^internal/generator/"
  ```
- **Exactly one redaction path**, `pkg/value.Format`. Generation must never emit a secret it
  read from a provider — see the rule below, which is this milestone's sharpest hazard.
- **`internal/graph` imports no other infra package; `providers/*` import no `internal/*`.**
- **`mise` is inactive in non-interactive shells.** Every command needs
  `export PATH="$HOME/.local/share/mise/shims:$PATH"`.
- **`-count=1` on every test run.** `tests/integration` shells out to `go build`.
- **Commit discipline:** `git add <explicit paths>` then `git commit -m "..." -- <same paths>`.
  Forbidden: `git add -A`, `git add .`, `git commit -am`.
- Diagnostics collect rather than fail fast, and each says what is wrong, where, what was
  expected, and an action that works **today**.
- Anything user-observable is sorted once, deliberately — never Go's map order.
- **Assert the thing did not happen, not that we said it would not happen.**
- **For any check, ask what the cheapest implementation that satisfies it would be.** If that
  would be wrong, the difference is a missing test.
- **A fixture must contradict its expected output.** A one-element fixture cannot prove a sort;
  that hid three separate defects in M5.
- A sabotage should leave the code COMPILING and change behaviour.

## The two hazards specific to this milestone

**1. A generated file must never contain a secret.** Discovery reads real attribute values from
a provider, including ones the schema marks sensitive. Writing those into `discovered/*.yml`
would put a live password in a file destined for version control — the M2 leak with a new
delivery mechanism. A sensitive attribute is OMITTED from generated configuration, and the
generated file says so at the point of omission, naming what the reader must supply. This is
the one place in the milestone where getting it wrong is unrecoverable: a secret committed to
git is a secret rotated, not a secret deleted.

**2. Import must not destroy.** §26 is explicit. A resource adopted into state but absent from
configuration is scheduled for destruction by invariant 1, which is why §27.1 loads
`discovered/`. Any task that can leave state and configuration disagreeing is wrong.

---

## Task 1: the compiler loads `discovered/*.yml`

**Files:**
- Modify: `internal/config/load.go` (`Load`, beside `loadEnvironmentDir`)
- Test: `internal/config/load_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `config.DiscoveredDirName = "discovered"`. Every later task depends on this
  existing, which is why it is first and alone.

- [ ] **Step 1.1: Write the failing test**

```go
// TestLoadReadsTheDiscoveredDirectory. §27.1: generated configuration is part of the project
// from the moment it is written, because import adds to state and a resource in state that no
// configuration declares is scheduled for destruction.
func TestLoadReadsTheDiscoveredDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "infra.yml", "project: p\nresources:\n  a:\n    type: test.network\n    cidr: 10.0.0.0/16\n")
	write(t, dir, "discovered/databases.yml", "resources:\n  imported:\n    type: test.database\n    engine: postgres\n")

	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var found bool
	for _, f := range files {
		if strings.Contains(f.Path, "databases.yml") {
			found = true
		}
	}
	if !found {
		t.Error("discovered/databases.yml was not loaded; an imported resource would be " +
			"in state and absent from configuration, which invariant 1 schedules for destruction")
	}
}

// TestLoadReadsEveryDiscoveredFileInOrder. Two files, declared so that map order or
// readdir order would show: a project whose resource set changes between identical runs
// breaks invariant 6 before the planner ever sees it.
func TestLoadReadsEveryDiscoveredFileInOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "infra.yml", "project: p\n")
	write(t, dir, "discovered/zeta.yml", "resources:\n  z:\n    type: test.network\n    cidr: 10.1.0.0/16\n")
	write(t, dir, "discovered/alpha.yml", "resources:\n  a:\n    type: test.network\n    cidr: 10.2.0.0/16\n")

	var first string
	for i := 0; i < 20; i++ {
		files, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range files {
			names = append(names, filepath.Base(f.Path))
		}
		got := strings.Join(names, ",")
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d loaded %s, want %s", i, got, first)
		}
	}
	if !strings.Contains(first, "alpha.yml,zeta.yml") {
		t.Errorf("discovered files are not sorted: %s", first)
	}
}

// TestLoadWithNoDiscoveredDirectoryIsFine. Most projects never import anything.
func TestLoadWithNoDiscoveredDirectoryIsFine(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "infra.yml", "project: p\n")
	if _, err := Load(dir); err != nil {
		t.Errorf("a project with no discovered/ must load: %v", err)
	}
}
```

- [ ] **Step 1.2: Run it, see it fail**

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 -run TestLoadReadsTheDiscovered ./internal/config/
```
Expected: FAIL — `discovered/databases.yml was not loaded`.

- [ ] **Step 1.3: Minimal code**

In `internal/config/load.go`, beside `EnvironmentsDirName`:

```go
// DiscoveredDirName holds configuration written by `infra import --generate`.
//
// It is LOADED, not staged. Import adds a resource to state (§26), and a resource in state that
// no configuration declares is scheduled for destruction by invariant 1 — so a staging area
// would mean import followed by apply destroys what was just adopted. §27.1.
const DiscoveredDirName = "discovered"
```

In `Load`, after the environments block and before the return, read the directory the same way
`loadEnvironmentDir` does — sorted, `.yml` only, a missing directory is not an error. Reuse that
function if its shape allows; duplicating a directory walk is how two orderings appear.

- [ ] **Step 1.4: Run it, see it pass**

```bash
go test -count=1 ./internal/config/
```

- [ ] **Step 1.5: Discrimination**

Delete the sort from the discovered walk and run `TestLoadReadsEveryDiscoveredFileInOrder` 20
times: it must fail. Restore with `cp`, never `git checkout --`.

- [ ] **Step 1.6: Commit**

```bash
git add internal/config/load.go internal/config/load_test.go
git commit -m "config: load discovered/*.yml as part of the project" -- internal/config/load.go internal/config/load_test.go
```

---

## Task 2: the fake provider discovers and imports

**Files:**
- Modify: `providers/test/provider.go` (`Discover`, `Import` — both currently `ErrNotImplemented`)
- Test: `providers/test/provider_test.go`

**Interfaces:**
- Consumes: `provider.DiscoverRequest{Types, Region}`, `provider.DiscoveredResource{Type, ProviderID, Attributes}` — declared in M1 so they would not churn here.
- Produces: a provider that enumerates its own JSON cloud, for every later task to walk.

- [ ] **Step 2.1: Write the failing test**

```go
// TestDiscoverFindsEveryResourceInTheCloud, including ones this project never created —
// which is the entire point: discovery is for infrastructure that predates the tool.
func TestDiscoverFindsEveryResourceInTheCloud(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cloud.json")
	writeCloud(t, path, map[string]any{
		"resources": map[string]any{
			"net-1": map[string]any{"type": "test.network", "address": "", "attributes": map[string]any{"cidr": "10.0.0.0/16", "id": "net-1"}},
			"db-9":  map[string]any{"type": "test.database", "address": "", "attributes": map[string]any{"engine": "postgres", "id": "db-9"}},
		},
	})

	got, err := New(path).Discover(context.Background(), provider.DiscoverRequest{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("found %d resources, want 2: %+v", len(got), got)
	}
	// Sorted by provider ID: the list is printed to a user and diffed by scripts.
	if got[0].ProviderID != "db-9" || got[1].ProviderID != "net-1" {
		t.Errorf("discovery is not sorted: %s, %s", got[0].ProviderID, got[1].ProviderID)
	}
	if got[0].Type != "test.database" {
		t.Errorf("type = %q", got[0].Type)
	}
}

// TestDiscoverFiltersByType. §25's `infra discover aws.rds` asks one question of a large
// account, and asking it by fetching everything and discarding most is how discovery becomes
// too slow to use.
func TestDiscoverFiltersByType(t *testing.T) {
	// ... two types in the cloud, request one, assert only that one returns AND that the
	// other is absent — asserting the count alone passes against a filter that returns nothing.
}

// TestImportReadsARealResourceByID.
func TestImportReadsARealResourceByID(t *testing.T) {
	// ... assert the returned ResourceState carries the provider's values with
	// SourceProvider, and that a missing ID is an error naming the ID.
}
```

- [ ] **Step 2.2: Run it, see it fail** — `ErrNotImplemented`.

- [ ] **Step 2.3: Minimal code**

Replace both stubs. `Discover` reads the cloud file, filters by `req.Types` when non-empty, and
returns results **sorted by ProviderID**. `Import` looks one up by ID and returns
`p.toState(...)`, which already stamps `SourceProvider` and applies the schema's sensitivity —
reuse it rather than building state by hand, or sensitivity is decided in two places.

Delete the two "arrives in Phase 2" comments.

- [ ] **Step 2.4: Run it, see it pass. Step 2.5: Discrimination** — remove the sort, run 20×.

- [ ] **Step 2.6: Commit**

---

## Task 3: naming a discovered resource

**Files:**
- Create: `internal/discovery/name.go`, `internal/discovery/name_test.go`

**Interfaces:**
- Produces: `func Name(r provider.DiscoveredResource) string` and
  `func Unique(names map[string]string, r provider.DiscoveredResource) string`.

§27.2. A name comes from a `name`/`Name` attribute or tag if present, else the sanitised
provider ID. A collision suffixes the provider ID; it never drops a resource.

- [ ] **Step 3.1: Write the failing test**

```go
func TestNameComesFromTheNameTagWhenThereIsOne(t *testing.T) { /* name="orders" -> orders */ }

func TestNameFallsBackToASanitisedProviderID(t *testing.T) {
	// net-1 -> net_1; i-0abc123 -> i_0abc123. A hyphen is not an identifier character and
	// a resource's name is its address.
}

// TestCollidingNamesBothSurvive is the one that matters. Two resources tagged `orders` must
// become two resources, not one — a silently merged pair is the shape this project guards
// against everywhere. Assert BOTH are present and distinct, not merely that no error occurred.
func TestCollidingNamesBothSurvive(t *testing.T) { /* orders, orders_db_9 */ }

// TestAnEmptyOrUnusableNameTagFallsBack. A tag of "" or "  " or "123" is not a name.
func TestAnEmptyOrUnusableNameTagFallsBack(t *testing.T) { /* ... */ }
```

- [ ] **Steps 3.2-3.6:** fail, implement, pass, discriminate (make `Unique` return the bare name
  and watch the collision test fail), commit.

---

## Task 4: minimal, default-aware generation

**Files:**
- Create: `internal/generator/generate.go`, `internal/generator/generate_test.go`

**Interfaces:**
- Consumes: `registry.Registry` (for defaults and sensitivity), `discovery.Name`.
- Produces: `func Generate(resources []Resource, reg *registry.Registry, env schema.DefaultContext) ([]byte, error)`, and the grouping that decides which file a resource lands in.

This is §27's core: given six attributes of which five equal their default, emit one line.

- [ ] **Step 4.1: Write the failing tests**

```go
// TestGenerationOmitsWhatADefaultAlreadyProvides is §27's worked example.
func TestGenerationOmitsWhatADefaultAlreadyProvides(t *testing.T) {
	// A test.database whose size equals the provider default for this environment must NOT
	// appear in the output; engine, which has no default, must.
	// Assert the ABSENCE explicitly: "size" must not appear. Asserting only that engine is
	// present passes against a generator that emits everything.
}

// TestGenerationOmitsASensitiveAttributeAndSaysSo. THE hazard of this milestone: a
// generated file is destined for version control, and a secret committed to git is a secret
// ROTATED, not a secret deleted. The omission must be visible, or a reader thinks the
// resource has no password rather than that they must supply one.
func TestGenerationOmitsASensitiveAttributeAndSaysSo(t *testing.T) {
	// Assert the secret's VALUE appears nowhere in the bytes, and that the attribute name
	// does appear in a comment naming what must be supplied.
}

// TestGenerationIsDeterministic — 20 runs, attributes and resources both sorted, with a
// fixture whose declaration order differs from its sorted order.

// TestGeneratedConfigurationParsesBackAndPlansClean is the round trip in miniature, and the
// only test here that proves generation is CORRECT rather than merely plausible.
```

- [ ] **Steps 4.2-4.6.** The default comparison must use the same `DefaultContext` the compiler
  uses, or a production import generates a development-shaped file. Grouping: the last segment
  of the type, pluralised — `test.database` → `databases.yml`, `test.network` → `networks.yml`.

---

## Task 5: `infra discover`

**Files:** Create `internal/cli/discover.go`, `internal/cli/discover_test.go`; modify `root.go`.

§25. `infra discover` lists everything; `infra discover <type>` narrows. Output is a table a
person reads, sorted, showing type, provider ID and the name it WOULD be given — so a user can
see a collision before importing rather than after.

Nothing is written and nothing is adopted: discover is read-only, and says so in its `Short`.

---

## Task 6: `infra import`

**Files:** Create `internal/cli/import.go`, `internal/cli/import_test.go`; modify `root.go`.

§26. `infra import <environment>` imports everything; `infra import <environment> <type>.<id>`
imports one. `--generate` additionally writes `discovered/<group>.yml`.

- **Never overwrite.** A `discovered/databases.yml` that exists is appended to, and a resource
  already present is skipped rather than duplicated — re-running import must be safe, because
  it is the command people run when they are unsure what happened the first time.
- **State and configuration move together.** With `--generate`, both or neither: an import that
  wrote state and failed to write configuration leaves a resource scheduled for destruction.
  Write configuration FIRST, then state.
- Importing a resource already in state is an error naming the address, not a silent overwrite.

---

## Task 7: `infra export`

**Files:** Create `internal/cli/export.go`, `internal/cli/export_test.go`; modify `root.go`.

§28. `infra export <environment>` dumps every known configurable attribute of everything in
state — the opposite of generation, for auditing and migration. Same sensitivity rule: a secret
is omitted, not printed. Reuses the generator with minimal-mode off, rather than a second
renderer.

---

## Task 8: the round trip — invariant 3

**Files:** Create `tests/integration/m7_roundtrip_test.go`.

§29, and the last of the six invariants with no test.

```
existing infrastructure → discover → import → generate minimal YAML → plan → no changes
```

- [ ] Build a fake cloud holding resources this project never created, including one whose
  attributes are all defaults except one, and one with a sensitive attribute.
- [ ] `discover`, then `import --generate`, then `plan` — and assert **zero operations**.
- [ ] Assert the generated file is MINIMAL: the defaulted attributes are absent.
- [ ] Assert the sensitive value appears nowhere in it.
- [ ] Re-run `import --generate` and assert it is idempotent: no duplicate blocks, plan still
      clean.
- [ ] Sabotage: make generation emit every attribute rather than only the non-default ones.
      The plan must STILL be clean — that is the point — so this test must assert minimality
      separately. A round trip that passes because generation emitted everything has proved
      nothing about §27.
