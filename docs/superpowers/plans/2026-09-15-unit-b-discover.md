# Unit B — Discover That Associates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `discover` name resources after their tags, link them to each other, hide what is already managed, flag what the cloud created for itself, and filter.

**Architecture:** Naming moves from a hard-coded lowercase `attrs["tags"]` lookup to the registry's own case-insensitive alias fold, which is what makes it work against a real plugin at all. Cross-references are resolved in `internal/generator` by reading `schema.Attribute.References` — a declaration the plugin publishes, never a relationship the engine infers. Exclusion of managed resources is done in `internal/cli` against every environment's state, which needs a new `state.Local.List()`. The system-owned flag is a plugin-declared field and raises the protocol to 4.

**Tech Stack:** Go 1.27, Cobra, `gopkg.in/yaml.v3`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-15-cli-output-discover-init-design.md` (§3)

## Global Constraints

- Go 1.27.0 module floor; `mise` is not active in non-interactive shells — use `make` targets or `export PATH="$HOME/.local/share/mise/shims:$PATH"`.
- **Third-party budget is two packages**: `github.com/spf13/cobra`, `gopkg.in/yaml.v3`.
- **The core engine must not know about AWS.** §3.5 exists because of this rule: detecting a default VPC in the engine would violate it, so the plugin declares it.
- **A generated file must never hold a secret.** A sensitive attribute is OMITTED and the file says so at the point of omission. Nothing in this unit may weaken that.
- **`internal/generator` is the one argued exception** to "only `internal/config` touches `yaml.Node`". It EMITS YAML and parses none. Keep it that way.
- **Naming happens after the whole set is sorted, never during the walk** — `Unique` is order-dependent by construction.
- **Nothing in `pkg/schema` may gain a function-typed field.** A function cannot cross a pipe.
- `gofmt -l .` and `go vet ./...` clean before each commit.
- Commit messages: plain English, no em-dashes, minimal, never mention Claude or AI.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/discovery/name.go` | Naming. Gains the registry, the tag fold, and the type prefix. |
| `internal/discovery/walk.go` | `Result` gains `SystemOwned`; `Walk` passes the registry to naming. |
| `internal/discovery/filter.go` *(new)* | Tag, type and name filters, applied after naming. |
| `internal/generator/references.go` *(new)* | Resolving a literal to `${name}` from a `References` declaration. |
| `internal/generator/generate.go` | Emit the reference where one resolved; comment where it did not. |
| `internal/state/local.go` | `List()` — the environments that have state. |
| `internal/cli/discover.go` | `STATUS` column under `--all`, filter flags, the managed footer. |
| `internal/cli/import.go` | `selectForImport` excludes managed and system-owned. |
| `pkg/provider/provider.go` | `DiscoveredResource.SystemOwned`, `SystemOwnedReason`. |
| `pkg/pluginproto/proto.go` | `Version = 4`, `Supported = {4,3,2,1}`, the two fields on the wire. |

---

## Task 1: The tag lookup asks the schema

The bug behind every `vpc-129012092` James reported. First, alone, because every naming task after it is measured against a name that now actually comes from a tag.

**Files:**
- Modify: `internal/discovery/name.go`, `internal/discovery/walk.go`
- Test: `internal/discovery/name_test.go`

**Interfaces:**
- Consumes: `registry.Registry.Definition(string) (*schema.ResourceDefinition, bool)`; `schema.ResourceDefinition.Canonical(written string) (canonical string, ok bool)`.
- Produces:
  - `func Name(reg *registry.Registry, r provider.DiscoveredResource) string`
  - `func Unique(reg *registry.Registry, taken map[string]string, r provider.DiscoveredResource) string`

- [ ] **Step 1: Write the failing test**

```go
// THE REGRESSION. internal/discovery/name.go read attrs["tags"] as a literal
// lowercase key, and the AWS plugin's canonical attribute is "Tags". The
// lookup missed on every AWS resource, so section 27.2's documented naming
// rule was dead code against the only real provider and every name fell
// through to the sanitised provider ID.
func TestNameFindsTheTagMapWhateverThePluginCallsIt(t *testing.T) {
	for _, attrName := range []string{"tags", "Tags", "TagSet"} {
		t.Run(attrName, func(t *testing.T) {
			reg := registryWithType(t, "aws.vpc", map[string]schema.Attribute{
				// TagSet exercises the ALIAS half of the fold, not just case.
				attrName: {Kind: value.KindMap, Aliases: []string{"tags"}},
			})
			r := provider.DiscoveredResource{
				Type:       "aws.vpc",
				ProviderID: "vpc-1023902339",
				Attributes: map[string]value.Value{
					attrName: value.Map(map[string]value.Value{
						"Name": value.String("app1", value.SourceProvider),
					}, value.SourceProvider),
				},
			}

			if got := Name(reg, r); got != "vpc-app1" {
				t.Errorf("Name = %q, want vpc-app1", got)
			}
		})
	}
}

// Both spellings inside the map, in the documented order: AWS writes Name,
// most other things write name.
func TestNamePrefersNameOverLowercaseName(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", map[string]schema.Attribute{
		"Tags": {Kind: value.KindMap},
	})
	r := provider.DiscoveredResource{
		Type:       "aws.vpc",
		ProviderID: "vpc-1",
		Attributes: map[string]value.Value{
			"Tags": value.Map(map[string]value.Value{
				"name": value.String("lower", value.SourceProvider),
				"Name": value.String("upper", value.SourceProvider),
			}, value.SourceProvider),
		},
	}

	if got := Name(reg, r); got != "vpc-upper" {
		t.Errorf("Name = %q, want vpc-upper", got)
	}
}

// A type the registry does not know must not panic and must not lose the
// resource: discovery reports what a provider returned, and refusing to name
// something would drop it.
func TestNameFallsBackWhenTheTypeIsUnknown(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", nil)
	r := provider.DiscoveredResource{Type: "aws.mystery", ProviderID: "m-1"}

	if got := Name(reg, r); got == "" {
		t.Error("Name returned empty for an unknown type")
	}
}
```

Write `registryWithType(t, typeName, attrs)` as a helper in the test file, building a `*registry.Registry` with one registered plugin carrying one `schema.ResourceDefinition`. Read `internal/registry/registry_test.go` first and reuse its construction pattern.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestName' ./internal/discovery/ -v`
Expected: FAIL — `Name` takes one argument, not two; and once that compiles, the `Tags`/`TagSet` cases return `vpc-1023902339`.

- [ ] **Step 3: Write minimal implementation**

In `name.go`, replace the `nameTags` map lookup inside `nameFrom`:

```go
// tagMap finds the resource's tag map by ASKING THE SCHEMA rather than
// guessing its spelling.
//
// It used to read attrs["tags"], a literal lowercase key. The AWS plugin
// spells it "Tags", so the lookup missed on every AWS resource and section
// 27.2's naming rule never once fired against the only real provider.
//
// The fix is not a third hard-coded spelling. That is the same defect with a
// longer list, and the next plugin spells it a fourth way. Canonical is the
// engine's ONE alias fold (PLAN.md section 14.1), already case-insensitive
// across canonical names and aliases, and already what the compiler uses at
// its own boundary. Asking it means discovery cannot drift from the rest of
// the engine about what an attribute is called.
func tagMap(reg *registry.Registry, resourceType string, attrs map[string]value.Value) (map[string]value.Value, bool)
```

It resolves the definition, calls `def.Canonical("tags")`, reads that attribute, and returns its map when it is a known `value.KindMap`. An unknown type or no match returns false — `Name` then falls through to the provider ID exactly as before.

Thread `reg` through `Name` and `Unique`, and update `Walk` (which already holds one) to pass it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/discovery/ -v 2>&1 | tail -20`
Expected: PASS. Existing tests calling `Name(r)` need their signature updated, which is mechanical.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/discovery/
git add internal/discovery/
git commit -m "Find the tag map by asking the schema, not by guessing its name

Discovery read the tag map as a literal lowercase key and the AWS plugin
spells it with a capital, so the naming rule never fired against the only
real provider and every resource fell back to its cloud identifier. A third
hard coded spelling would be the same bug with a longer list."
```

---

## Task 2: Names carry a type prefix

**Files:**
- Modify: `internal/discovery/name.go`
- Test: `internal/discovery/name_test.go`

**Interfaces:**
- Consumes: Task 1's `Name(reg, r)`.
- Produces: `func typePrefix(resourceType string) string` — the last dotted segment.

- [ ] **Step 1: Write the failing test**

```go
func TestTypePrefixIsTheLastDottedSegment(t *testing.T) {
	// Verified against infrena-provider-aws gen/names.lock.json: the AWS
	// generator already collapses unambiguous type names, so the last segment
	// is the right prefix without the plugin publishing a short name and
	// without a protocol change.
	for _, tc := range []struct{ in, want string }{
		{"aws.vpc", "vpc"},
		{"aws.subnet", "subnet"},
		{"aws.role", "role"},
		{"aws.s3.bucket", "bucket"},
		{"aws.securitygroup", "securitygroup"},
		{"aws.rds.dbinstance", "dbinstance"},
		{"fake", "fake"},
	} {
		if got := typePrefix(tc.in); got != tc.want {
			t.Errorf("typePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// AWS provider IDs are themselves prefixed, so prefixing blindly gives
// vpc-vpc-0a1b2c3d.
func TestNameDoesNotDoublePrefixAProviderID(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", nil)
	r := provider.DiscoveredResource{Type: "aws.vpc", ProviderID: "vpc-0a1b2c3d"}

	if got := Name(reg, r); got != "vpc-0a1b2c3d" {
		t.Errorf("Name = %q, want vpc-0a1b2c3d", got)
	}
}

// The prefix makes cross-type collisions impossible, so Unique's provider-ID
// suffix now only fires for two resources of ONE type sharing a Name tag,
// which is genuinely two resources and must never be silently merged.
func TestPrefixSeparatesTwoTypesSharingATag(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.vpc":    {"Tags": {Kind: value.KindMap}},
		"aws.subnet": {"Tags": {Kind: value.KindMap}},
	})
	taken := map[string]string{}
	tagged := func(typ, id string) provider.DiscoveredResource {
		return provider.DiscoveredResource{
			Type: typ, ProviderID: id,
			Attributes: map[string]value.Value{
				"Tags": value.Map(map[string]value.Value{
					"Name": value.String("app1", value.SourceProvider),
				}, value.SourceProvider),
			},
		}
	}

	a := Unique(reg, taken, tagged("aws.vpc", "vpc-1"))
	b := Unique(reg, taken, tagged("aws.subnet", "subnet-1"))

	if a != "vpc-app1" || b != "subnet-app1" {
		t.Errorf("got %q and %q, want vpc-app1 and subnet-app1", a, b)
	}
}

// Every generated name must still be one config will accept.
func TestEveryGeneratedNameIsValid(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", map[string]schema.Attribute{"Tags": {Kind: value.KindMap}})
	for _, tag := range []string{"app 1", "app/1", "1app", "-app", "", "ÄPP"} {
		r := provider.DiscoveredResource{
			Type: "aws.vpc", ProviderID: "vpc-1",
			Attributes: map[string]value.Value{
				"Tags": value.Map(map[string]value.Value{
					"Name": value.String(tag, value.SourceProvider),
				}, value.SourceProvider),
			},
		}
		if got := Name(reg, r); !config.ValidResourceName(got) {
			t.Errorf("Name for tag %q = %q, which config rejects", tag, got)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestTypePrefix|TestNameDoesNotDouble|TestPrefixSeparates|TestEveryGeneratedName' ./internal/discovery/ -v`
Expected: FAIL — `typePrefix` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// typePrefix is the short name a generated resource is prefixed with: the
// last dotted segment of the resource type.
//
// A plugin-published short name was considered and deliberately NOT taken.
// schema.ResourceDefinition has no such field, adding one raises the protocol,
// and the AWS generator already collapses unambiguous type names — AWS::EC2::VPC
// becomes aws.vpc — so the last segment is already the right answer for the
// types a user meets. An optional ShortName stays available later with this as
// its fallback, the same partial-coverage shape References uses, where absence
// costs exactly what it costs today.
func typePrefix(resourceType string) string {
	if i := strings.LastIndex(resourceType, "."); i >= 0 {
		return resourceType[i+1:]
	}
	return resourceType
}
```

In `Name`, prefix both the tag path and the provider-ID fallback, and skip the prefix when the sanitised text already begins with `prefix + "-"`. Sanitise the joined string, not the parts, so one rule decides what a name may contain.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/discovery/ -v 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/discovery/
git add internal/discovery/
git commit -m "Prefix a discovered name with its type

So a reference reads as vpc-app1 rather than app1, and two types sharing a
Name tag cannot collide. The prefix is the last segment of the type, which
needs no protocol change because the AWS generator already collapses
unambiguous type names."
```

---

## Task 3: `state.Local.List()`

**Files:**
- Modify: `internal/state/local.go`
- Test: `internal/state/local_test.go`

**Interfaces:**
- Produces: `func (l *Local) List() ([]string, error)` — environment names that have state, sorted.

- [ ] **Step 1: Write the failing test**

```go
func TestListReportsEnvironmentsThatHaveState(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	ctx := context.Background()

	for _, env := range []string{"production", "dev"} {
		if err := l.Put(ctx, env, New()); err != nil {
			t.Fatal(err)
		}
	}

	got, err := l.List()
	if err != nil {
		t.Fatal(err)
	}
	// Sorted, because a caller renders this and a run-to-run reorder is a
	// diff nobody can review.
	if !reflect.DeepEqual(got, []string{"dev", "production"}) {
		t.Errorf("List = %v, want [dev production]", got)
	}
}

// A project that has never applied anything is the common case for discover,
// so this must be an empty list and not an error.
func TestListOnAProjectWithNoStateIsEmpty(t *testing.T) {
	got, err := NewLocal(t.TempDir()).List()
	if err != nil {
		t.Fatalf("List errored on a project with no state: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
}

// Locks live beside state files. Listing must not report a lock as an
// environment, or discover would exclude resources against a file that holds
// no resources at all.
func TestListIgnoresLockFiles(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	if err := l.Put(context.Background(), "dev", New()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Lock(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}

	got, err := l.List()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"dev"}) {
		t.Errorf("List = %v, want [dev]", got)
	}
}
```

Check `NewLocal`'s real constructor name in `internal/state/local.go` and match it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestList' ./internal/state/ -v`
Expected: FAIL — `List` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// List reports the environments that have state, sorted.
//
// PLAN.md section 6.1 makes an environment reachable if it is DECLARED or it
// HAS STATE, and until now the second half was only ever answered for one
// named environment at a time. Discover needs the whole set: a resource
// managed in production is managed whichever environment you are importing
// into, and adopting it twice would put one real resource under two addresses
// for invariant 1 to then schedule for destruction under whichever loses.
//
// A missing directory is an empty list, not an error: a project that has
// never applied anything is the ordinary case for the command that needs
// this.
func (l *Local) List() ([]string, error)
```

Read the directory `statePath` writes into, take entries matching the state-file naming `statePath` produces, strip the suffix, sort, return. `os.ReadDir` returning `fs.ErrNotExist` gives `nil, nil`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/state/ -v 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/state/
git add internal/state/
git commit -m "List the environments that have state

Discover needs the whole set, not one named environment, because a resource
managed anywhere is managed. Remote state in phase 4 will want it too."
```

---

## Task 4: Discover and import exclude what is managed

**Files:**
- Modify: `internal/cli/discover.go`, `internal/cli/import.go`
- Create: `internal/cli/managed.go`
- Test: `internal/cli/discover_test.go`

**Interfaces:**
- Consumes: Task 3's `Local.List()`; `state.State.Addresses()`; `resource.ResourceState` (read its `ProviderID` field name from `pkg/resource/resource.go` and match it).
- Produces:
  - `func managedProviderIDs(ctx context.Context, backend *state.Local) (map[string]string, error)` — provider ID to the environment managing it.

- [ ] **Step 1: Write the failing test**

```go
func TestDiscoverHidesWhatIsAlreadyManaged(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1", "vpc-2")
	importOne(t, dir, "dev", "vpc-1")

	stdout, _, _ := runCommand(t, dir, "discover")

	if strings.Contains(stdout, "vpc-1") {
		t.Errorf("discover listed a managed resource:\n%s", stdout)
	}
	if !strings.Contains(stdout, "vpc-2") {
		t.Errorf("discover hid an unmanaged resource:\n%s", stdout)
	}
	// A count a user cannot see is a count they will assume is zero.
	if !strings.Contains(stdout, "already managed") {
		t.Errorf("footer does not state the exclusion:\n%s", stdout)
	}
}

// Scope is EVERY environment, not the one you happen to be importing into.
func TestManagedMeansManagedInAnyEnvironment(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1")
	importOne(t, dir, "production", "vpc-1")

	stdout, _, _ := runCommand(t, dir, "discover")

	if strings.Contains(stdout, "vpc-1") {
		t.Errorf("discover listed a resource managed in another environment:\n%s", stdout)
	}
}

// The STATUS column only earns its place under --all: in the default view
// every row would read unmanaged, and a column with one value is noise.
func TestAllShowsEverythingWithAStatusColumn(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1", "vpc-2")
	importOne(t, dir, "dev", "vpc-1")

	all, _, _ := runCommand(t, dir, "discover", "--all")
	def, _, _ := runCommand(t, dir, "discover")

	if !strings.Contains(all, "STATUS") || !strings.Contains(all, "managed (dev)") {
		t.Errorf("--all lacks the status column:\n%s", all)
	}
	if strings.Contains(def, "STATUS") {
		t.Errorf("default view grew a status column:\n%s", def)
	}
}

func TestImportWillNotReadoptAManagedResource(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1")
	importOne(t, dir, "dev", "vpc-1")

	_, stderr, _ := runCommand(t, dir, "import", "dev", "vpc-1", "--generate")

	if !strings.Contains(stderr, "already managed") {
		t.Errorf("import re-adopted a managed resource, or did not say why not:\n%s", stderr)
	}
}
```

Build `newProjectWithDiscoverableResources` and `importOne` on the existing fixtures in `internal/cli/import_test.go` and `discovery_plugins_test.go`; the fake provider double is already injected by `internal/cli`'s TestMain.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestDiscoverHides|TestManagedMeans|TestAllShows|TestImportWillNot' ./internal/cli/ -v`
Expected: FAIL — managed resources are listed today.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/managed.go` with `managedProviderIDs`: for each environment from `backend.List()`, load its state and index every resource's provider ID to that environment name. **Key on provider ID, not address**: an address is what infrena chose to call it, and the same real resource adopted twice would have two.

In `discover.go`: build the map, partition the walk's results, and add `--all`. The footer always states both counts — `12 unmanaged, 50 already managed (--all to show them)` — because a count a user cannot see is one they will assume is zero.

In `import.go`'s `selectForImport`: drop managed results **unless the selector named one explicitly**, in which case error saying which environment manages it and that the resource is already under management. Silently ignoring an explicit selector is worse than refusing it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/cli/
git commit -m "Discover and import skip what is already managed

A real account returns hundreds of rows and the question is always what has
not been adopted yet. Adopting one resource twice puts it under two
addresses, and removal from configuration then destroys it under whichever
one loses."
```

---

## Task 5: Generated configuration references what it found

**Files:**
- Create: `internal/generator/references.go`, `internal/generator/references_test.go`
- Modify: `internal/generator/generate.go`

**Interfaces:**
- Consumes: `schema.Attribute.References *schema.Reference` with fields `Type` and `Attribute`; `registry.Registry.Definition`.
- Produces:
  - `type refIndex map[string]map[string]string` — resource type, then canonical-attribute value, to the configuration name holding it.
  - `func buildRefIndex(resources []Resource, reg *registry.Registry) refIndex`
  - `func (ix refIndex) resolve(ref *schema.Reference, v value.Value) (name string, ok bool)`

- [ ] **Step 1: Write the failing test**

```go
func TestGeneratedSubnetReferencesTheDiscoveredVPC(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.vpc":    {"VpcId": {Kind: value.KindString, Computed: true}},
		"aws.subnet": {"VpcId": {Kind: value.KindString, References: &schema.Reference{Type: "aws.vpc", Attribute: "VpcId"}}},
	})
	resources := []Resource{
		{Name: "vpc-app1", Type: "aws.vpc", ProviderID: "vpc-1023902339", Attributes: map[string]value.Value{
			"VpcId": value.String("vpc-1023902339", value.SourceProvider)}},
		{Name: "subnet-app1a", Type: "aws.subnet", ProviderID: "subnet-1", Attributes: map[string]value.Value{
			"VpcId": value.String("vpc-1023902339", value.SourceProvider)}},
	}

	files, err := Generate(resources, reg, MinimalOptions())
	if err != nil {
		t.Fatal(err)
	}

	got := fileNamed(t, files, FileName("aws.subnet"))
	if !strings.Contains(got, "${vpc-app1}") {
		t.Errorf("subnet does not reference the VPC:\n%s", got)
	}
	if strings.Contains(got, "vpc-1023902339") {
		t.Errorf("subnet still carries the literal:\n%s", got)
	}
}

// A reference whose target was not discovered would be a compile error in a
// file the user never wrote. The literal survives, and the omission is stated
// where a reader meets it.
func TestAnUnmatchedReferenceKeepsItsLiteralAndSaysSo(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.vpc":    {"VpcId": {Kind: value.KindString, Computed: true}},
		"aws.subnet": {"VpcId": {Kind: value.KindString, References: &schema.Reference{Type: "aws.vpc", Attribute: "VpcId"}}},
	})
	resources := []Resource{
		{Name: "subnet-app1a", Type: "aws.subnet", ProviderID: "subnet-1", Attributes: map[string]value.Value{
			"VpcId": value.String("vpc-not-discovered", value.SourceProvider)}},
	}

	files, err := Generate(resources, reg, MinimalOptions())
	if err != nil {
		t.Fatal(err)
	}

	got := fileNamed(t, files, FileName("aws.subnet"))
	if !strings.Contains(got, "vpc-not-discovered") {
		t.Errorf("literal was dropped:\n%s", got)
	}
	if !strings.Contains(got, "not discovered") {
		t.Errorf("omission is not stated in the file:\n%s", got)
	}
}

// The AWS plugin emits 97 list edges, so a list of ids must project too.
func TestAListOfReferencesProjectsEachEntry(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.subnet": {"SubnetId": {Kind: value.KindString, Computed: true}},
		"aws.lb": {"SubnetIds": {Kind: value.KindList,
			References: &schema.Reference{Type: "aws.subnet", Attribute: "SubnetId"}}},
	})
	resources := []Resource{
		{Name: "subnet-a", Type: "aws.subnet", ProviderID: "subnet-a", Attributes: map[string]value.Value{
			"SubnetId": value.String("subnet-a", value.SourceProvider)}},
		{Name: "lb-app1", Type: "aws.lb", ProviderID: "lb-1", Attributes: map[string]value.Value{
			"SubnetIds": value.List([]value.Value{value.String("subnet-a", value.SourceProvider)}, value.SourceProvider)}},
	}

	files, err := Generate(resources, reg, MinimalOptions())
	if err != nil {
		t.Fatal(err)
	}

	if got := fileNamed(t, files, FileName("aws.lb")); !strings.Contains(got, "${subnet-a}") {
		t.Errorf("list entry did not project:\n%s", got)
	}
}

// Invariant 3. The reference resolves at compile time to the same literal the
// attribute held, so the round trip still plans clean.
func TestReferencesDoNotBreakTheRoundTrip(t *testing.T) {
	// Build the project from the generated files and assert planner.Compute
	// returns zero operations, following the existing round-trip test in
	// tests/integration. Read it and extend it rather than writing a second.
	t.Skip("implemented as an extension of the existing round-trip integration test in Task 8")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestGeneratedSubnet|TestAnUnmatchedReference|TestAListOfReferences' ./internal/generator/ -v`
Expected: FAIL — the generated file carries the literal.

- [ ] **Step 3: Write minimal implementation**

Create `internal/generator/references.go`:

```go
// buildRefIndex maps each discovered resource's referenceable attribute values
// to the configuration name that will hold them.
//
// THE PLUGIN DECIDES, and this file only reads. schema.Attribute.References
// says that, say, aws.subnet's VpcId holds an aws.vpc's VpcId; the engine
// never infers that a property ending in Id points anywhere (PLAN.md section
// 14.3). An attribute with no declaration keeps its literal, which is exactly
// what it does today, so partial plugin coverage costs nothing.
func buildRefIndex(resources []Resource, reg *registry.Registry) refIndex
```

In `generate.go`, at the point each attribute is rendered: if the attribute declares `References` and the index resolves the value, emit `${name}` instead of the literal; for a `KindList`, resolve entry by entry and emit a list of the mix. Where it does not resolve, emit the literal and attach a comment naming the target type and that it was not discovered — the same comment mechanism the omitted-secret path already uses, which is why this package emits `yaml.Node` rather than marshalling.

**Only resources in `resources` are indexed.** That is the rule that keeps a generated `${vpc-app1}` from being a compile error in a file the user never wrote.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/generator/ -v 2>&1 | tail -30`
Expected: PASS. The existing minimality tests must still pass — a reference is not a reason to emit an attribute that equals its default.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./internal/generator/
git add internal/generator/
git commit -m "Generated configuration references what discovery found

A subnet now says its VPC by name instead of pasting a cloud identifier,
read from the relationship the plugin declares rather than guessed from the
property name. Only targets in the same import set are referenced, or the
generated file would name a resource that does not exist."
```

---

## Task 6: The system-owned flag, protocol 4

**Files:**
- Modify: `pkg/provider/provider.go`, `pkg/pluginproto/proto.go`, `internal/pluginhost/adapter.go`, `internal/discovery/walk.go`, `internal/cli/discover.go`, `internal/cli/import.go`
- Test: `pkg/pluginproto/version_test.go`, `internal/cli/discover_test.go`

**Interfaces:**
- Produces:
  - `provider.DiscoveredResource.SystemOwned bool`, `.SystemOwnedReason string`
  - `discovery.Result.SystemOwned bool`, `.SystemOwnedReason string`
  - `pluginproto.Version = 4`, `Supported = []int{4, 3, 2, 1}`

- [ ] **Step 1: Write the failing test**

```go
func TestProtocolFourIsSupportedAlongsideItsPredecessors(t *testing.T) {
	if Version != 4 {
		t.Errorf("Version = %d, want 4", Version)
	}
	// A protocol 3 plugin reports nothing and behaves exactly as today.
	// Absence costs what it costs now, which is what makes it safe to add a
	// field before any plugin sets it.
	for _, v := range []int{4, 3, 2, 1} {
		if !slices.Contains(Supported, v) {
			t.Errorf("Supported does not include %d", v)
		}
	}
}
```

```go
func TestDiscoverFlagsWhatTheCloudOwns(t *testing.T) {
	dir := newProjectWithSystemOwnedResource(t, "vpc-default", "the account's default VPC")

	stdout, _, _ := runCommand(t, dir, "discover")

	if !strings.Contains(stdout, "the account's default VPC") {
		t.Errorf("discover did not say why the resource is flagged:\n%s", stdout)
	}
}

// Never adopted by default and never silently. Naming it explicitly is the
// escape hatch, because adopting a default VPC is occasionally right and the
// engine should not be the one forbidding it.
func TestImportSkipsSystemOwnedUnlessNamed(t *testing.T) {
	dir := newProjectWithSystemOwnedResource(t, "vpc-default", "the account's default VPC")

	_, stderr, _ := runCommand(t, dir, "import", "dev", "--generate")
	if !strings.Contains(stderr, "skipped") {
		t.Errorf("import did not report the skip:\n%s", stderr)
	}

	_, _, code := runCommand(t, dir, "import", "dev", "vpc-default", "--generate")
	if code != ExitOK {
		t.Errorf("naming it explicitly did not import it: exit %d", code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestProtocolFour' ./pkg/pluginproto/ -v && go test -run 'TestDiscoverFlags|TestImportSkipsSystem' ./internal/cli/ -v`
Expected: FAIL — `Version = 3`; the field does not exist.

- [ ] **Step 3: Write minimal implementation**

In `pkg/provider/provider.go`:

```go
	// SystemOwned marks a resource the CLOUD created and manages, which a user
	// did not ask for and generally must not adopt: AWS's default VPC and
	// default security group, service-linked roles.
	//
	// THE PLUGIN DECLARES IT. The engine cannot detect one without learning
	// about AWS, and "the core engine must not know about AWS" is this
	// project's first architectural rule. Spotting GroupName: default, or an
	// aws:cloudformation:* tag, or a service-linked role path, is AWS
	// knowledge by any reading, and it belongs to whoever owns the API.
	//
	// Nothing REFUSES to import one. It is occasionally right, and the engine
	// is not the party to forbid it. It is never adopted by default and never
	// silently, which is a different and weaker claim on purpose.
	SystemOwned bool

	// SystemOwnedReason says why, in the plugin's own words, for the line a
	// user reads before deciding. A flag with no reason is a flag a user
	// overrides without understanding it.
	SystemOwnedReason string
```

Carry both across the wire in `pkg/pluginproto`, raise `Version` to 4 and `Supported` to `{4, 3, 2, 1}`. Copy them through `internal/pluginhost/adapter.go` and `discovery.Walk` into `discovery.Result`.

`discover` renders the reason in a `NOTE` column. `import` excludes them unless a selector names one, and reports the count and reason of what it skipped.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... 2>&1 | tail -20`
Expected: PASS. The protocol handshake tests in `internal/pluginhost` must still accept a protocol 3 plugin — a version bump that drops an older plugin is not what this is.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add pkg/ internal/
git commit -m "Protocol 4 lets a plugin flag what the cloud owns

Importing the default VPC and later destroying it is the worst foot gun in
discovery, and it has already bitten. The plugin declares it because the
engine cannot detect one without learning about AWS. A protocol 3 plugin
reports nothing and behaves exactly as before."
```

---

## Task 7: Filters

**Files:**
- Create: `internal/discovery/filter.go`, `internal/discovery/filter_test.go`
- Modify: `internal/cli/discover.go`, `internal/cli/import.go`

**Interfaces:**
- Produces:
  - `type Filter struct { Tags map[string]string; ExcludeTypes []string; NameGlob string }`
  - `func (f Filter) Apply(reg *registry.Registry, in []Result) ([]Result, error)`

- [ ] **Step 1: Write the failing test**

```go
// Filtering happens AFTER naming, because Unique is order-dependent by
// construction: a filter applied first would change the names of the
// resources that survive it, so the same resource would import under
// different names depending on an unrelated flag.
func TestFilteringAfterNamingLeavesNamesUnchanged(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", map[string]schema.Attribute{"Tags": {Kind: value.KindMap}})
	all := namedResults(t, reg, "app1", "app2", "app3")

	got, err := Filter{NameGlob: "vpc-app2"}.Apply(reg, all)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Name != "vpc-app2" {
		t.Errorf("Apply = %v, want one result named vpc-app2", got)
	}
}

func TestTagFiltersAreAndedTogether(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", map[string]schema.Attribute{"Tags": {Kind: value.KindMap}})
	in := taggedResults(t, reg, map[string]map[string]string{
		"vpc-1": {"Name": "app1", "env": "prod"},
		"vpc-2": {"Name": "app1", "env": "dev"},
	})

	got, err := Filter{Tags: map[string]string{"Name": "app1", "env": "prod"}}.Apply(reg, in)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Errorf("Apply returned %d results, want 1", len(got))
	}
}

func TestExcludeTypeRemovesEveryResourceOfThatType(t *testing.T) {
	reg := registryWithTypes(t, map[string]map[string]schema.Attribute{
		"aws.vpc": nil, "aws.subnet": nil,
	})
	in := []Result{{Name: "vpc-a", Type: "aws.vpc"}, {Name: "subnet-a", Type: "aws.subnet"}}

	got, err := Filter{ExcludeTypes: []string{"aws.subnet"}}.Apply(reg, in)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Type != "aws.vpc" {
		t.Errorf("Apply = %v, want only the vpc", got)
	}
}

// path.Match reports a syntax error, and a silently-empty result would look
// like an empty account.
func TestABadGlobIsAnErrorNotAnEmptyResult(t *testing.T) {
	reg := registryWithType(t, "aws.vpc", nil)

	if _, err := (Filter{NameGlob: "["}).Apply(reg, []Result{{Name: "vpc-a", Type: "aws.vpc"}}); err == nil {
		t.Fatal("a malformed glob was accepted")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestFiltering|TestTagFilters|TestExcludeType|TestABadGlob' ./internal/discovery/ -v`
Expected: FAIL — `Filter` undefined.

- [ ] **Step 3: Write minimal implementation**

`Filter.Apply` uses the same `tagMap` fold Task 1 added, so `--tag Name=app1` works whatever the plugin spells the map. `NameGlob` uses `path.Match` — standard library, which the two-package budget requires. Wire `--tag` (repeatable), `--exclude-type` (repeatable) and `--name` on both `discover` and `import`, and call `Apply` after `Walk` returns.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/discovery/ ./internal/cli/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/
git commit -m "Filter discovery by tag, type and name

A real account returns more than anyone reads. Filtering runs after naming
because the unique-name pass is order dependent, so filtering first would
change what the surviving resources are called."
```

---

## Task 8: `discover` reports progress and honours `--output`

The spec's §2.3 gives `discover` the hook `refresh` already has. It lands here rather than in Unit A because Unit A owns no discovery code, and it depends on Unit A's `runOutput` being in place.

**Files:**
- Modify: `internal/discovery/walk.go`, `internal/cli/discover.go`
- Test: `internal/discovery/walk_test.go`, `internal/cli/discover_test.go`

**Interfaces:**
- Consumes: Unit A's `openRun`, `runOutput.Out()`, `runOutput.Progress()`.
- Produces: `func Walk(ctx context.Context, reg *registry.Registry, types []string, onProgress func(instance string, found int)) ([]Result, []error)`.

- [ ] **Step 1: Write the failing test**

```go
// A Discover against a real account is API calls, and the filters this unit
// adds make it slower still. Asking an instance and saying nothing until
// every instance has answered is the same silence the spec exists to close.
func TestWalkReportsEachInstanceAsItAnswers(t *testing.T) {
	reg := registryWithTwoInstances(t)
	var seen []string

	if _, problems := Walk(context.Background(), reg, nil, func(instance string, found int) {
		seen = append(seen, instance)
	}); len(problems) != 0 {
		t.Fatalf("Walk reported problems: %v", problems)
	}

	if len(seen) != 2 {
		t.Errorf("progress fired %d times, want once per instance", len(seen))
	}
}

// Nil-means-no-hook, the convention executor.Options.OnEvent and
// refresh.Refresh's onObservation already use.
func TestWalkWithNoProgressHookDoesNotPanic(t *testing.T) {
	reg := registryWithTwoInstances(t)
	if _, problems := Walk(context.Background(), reg, nil, nil); len(problems) != 0 {
		t.Fatalf("Walk reported problems: %v", problems)
	}
}
```

```go
func TestDiscoverOutputModeLeavesStdoutByteEmpty(t *testing.T) {
	dir := newProjectWithDiscoverableResources(t, "vpc-1")
	out := filepath.Join(t.TempDir(), "run.ndjson")

	stdout, _, _ := runCommand(t, dir, "discover", "--output", out)

	if stdout != "" {
		t.Errorf("discover --output wrote %q to stdout, want nothing", stdout)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Errorf("discover --output wrote no file: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestWalkReports|TestWalkWithNoProgress' ./internal/discovery/ -v && go test -run 'TestDiscoverOutputMode' ./internal/cli/ -v`
Expected: FAIL — `Walk` takes three arguments; `discover` ignores `--output` entirely.

- [ ] **Step 3: Write minimal implementation**

Add the parameter to `Walk` and call it after each instance answers, **before** the sort-and-name pass — naming still happens once over the whole set, because `Unique` is order-dependent by construction and per-instance naming would make a name depend on which provider replied first.

Update the three existing `Walk` call sites (`discover.go`, `import.go`, and any test helper) to pass a hook or `nil`.

In `discover.go`, replace the direct `cmd.OutOrStdout()` writes with `openRun` and `ro.Out()`, exactly as Unit A Task 5 did for the other commands, and render the table into the report as `observation` lines when `--output` is set. The provider-problem warnings stay on stderr, where they already are and where Unit A leaves diagnostics.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/discovery/ ./internal/cli/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./...
git add internal/
git commit -m "Discover reports progress and writes a machine readable file

Asking a real account is API calls and the new filters make it slower. It
said nothing until every provider instance had answered."
```

---

## Task 9: Round trip, and documentation

**Files:**
- Modify: `tests/integration/` (the existing invariant 3 round-trip test), `PLAN.md`, `CLAUDE.md`
- Test: as above

- [ ] **Step 1: Extend the round-trip test**

Find the existing invariant 3 test (`discover` → `import` → generate → `plan` ⇒ no unexpected changes) and extend its fixture with two resources where one references the other. Assert the generated file contains `${…}` **and** that the plan is still clean. Both halves: a test asserting only the clean plan would pass against a generator that emitted no reference at all.

- [ ] **Step 2: Run it**

Run: `go test ./tests/integration/ -run 'RoundTrip' -v`
Expected: PASS.

- [ ] **Step 3: Update `PLAN.md`**

§25: discover excludes managed resources, gains `--all` and the three filters, and shows the system-owned note. §27.2: names are type-prefixed and the tag map is resolved through the alias fold. §27: generated configuration carries references, only to targets in the same set. §31.1: protocol 4.

- [ ] **Step 4: Update `CLAUDE.md`**

The M8 section states the three rules generation carries. Add the fourth: a reference is emitted only when its target is in the same generated set, because one that is not would be a compile error in a file the user never wrote.

- [ ] **Step 5: Verify and commit**

```bash
go test ./... && go vet ./... && gofmt -l .
git add tests/ PLAN.md CLAUDE.md
git commit -m "Prove the round trip still holds with references, and document discovery"
```
