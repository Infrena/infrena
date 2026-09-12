# M11 — Provider Instances

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to
> implement this plan task-by-task.

**Goal:** One project, several provider instances — two AWS accounts, or AWS beside Azure — with
each resource choosing one, and each instance's configuration varying per environment.

**Architecture:** `providers:` decodes to a list, resolves (after variables, so it can
interpolate) into a table keyed by instance name, and every provider lookup becomes
(type, instance) instead of (type). The registry's one-provider-per-type key is what changes
underneath; everything else follows from it.

**Tech Stack:** Go 1.24, Cobra, `gopkg.in/yaml.v3`. No new dependency. AWS is still Phase 3 —
this is tested with two instances of the FAKE provider, which is exactly what it is for.

**Spec:** `PLAN.md` §12.1 (instances, names, defaults, `defaults:`, variables), §21 (state),
§31 (the Provider interface), §43 (provenance).

## Global Constraints

- **Exactly two third-party dependencies.** `git diff --stat <BASE> HEAD -- go.mod go.sum` must
  print nothing.
- **`internal/config` is the ONLY package permitted to touch `yaml.Node`**, with
  `internal/generator` the one argued exception.
- **Exactly one redaction path**, `pkg/value.Format`.
- **`internal/graph` imports no other infra package; `providers/*` import no `internal/*`.**
- **The core engine must not know about AWS.** This milestone makes multi-account possible; it
  must not make anything AWS-shaped.
- **`mise` is inactive in non-interactive shells.** `export PATH="$HOME/.local/share/mise/shims:$PATH"`.
- **`-count=1` on every test run.**
- **Commit discipline:** `git add <explicit paths>` then `git commit -m "..." -- <same paths>`.
- Diagnostics say what is wrong, where, what was expected, and an action that works **today**.
- Anything user-observable is sorted once — never Go's map order.
- **Assert the thing did not happen.** **A fixture must contradict its expected output.**
- **A sabotage must leave the code COMPILING and change behaviour.**
- **A test of a mechanism must exercise the one path where that mechanism is load-bearing**, and
  a sabotage is the only way to find that path. M10 shipped a comment promising work that nothing
  did, and only the worked example caught it.
- **Restore a sabotage with `cp` from a backup, never `git checkout <file>`.**

## The rule that governs this milestone

**A resource's instance must be derivable from STATE alone, not only from configuration.**

Everything else here is plumbing. This one is different because `destroy` has no configuration —
it reads state and deletes what is there. So does the removal half of every apply: a resource
deleted from the file is destroyed using nothing but its state entry.

If the instance is only ever resolved from configuration, then removing a resource from a project
with two instances leaves the engine unable to say which account to delete it from. The
consequences of guessing are not symmetrical: deleting from the wrong account destroys something
nobody asked about, and deleting from the right one by luck is indistinguishable until the day it
is not.

`resource.ResourceState.Provider` already exists and already holds a provider name. It must come
to hold the INSTANCE name, and every dispatch on the destroy path must read it.

### Which makes the migration question load-bearing, and answerable without a version bump

A v1 state file holds the PLUGIN name — `provider: "test"`. §12.1 names an unnamed instance after
its plugin, so for every project that has not named its instances, the existing value is already
the correct instance name. Nothing to migrate.

For a project that HAS named them, a state entry naming no declared instance is the case to
handle, and the answer is NOT to guess the default silently: report it, name the instance the
resource claims and the ones that exist, and stop. A wrong guess here deletes real infrastructure
in the wrong account.

**So this milestone needs no state version bump**, which matters because §21's migration path is
still lossy (`json.Unmarshal` into `map[string]any` turns a large integer into a float64 — see the
recorded follow-up). Verify that claim before relying on it.

---

## Task 1: decode `providers:`

**Files:**
- Modify: `internal/config/declarations.go`, `internal/config/decode.go`
- Test: `internal/config/providers_test.go`

**Interfaces:**
- Produces: `config.ProviderDecl{Plugin, Name string, Default bool, Config map[string]AttributeDecl,
  Defaults map[string]AttributeDecl, Origin value.Origin}` and `ProjectDecl.Providers []ProviderDecl`,
  in declaration order — order decides the default, so the SLICE is the shape, never a map.

Decoding only. No resolution, no registry, no dispatch.

- [ ] **Step 1.1: Failing tests**

```go
// TestASingleProviderNeedsOnlyItsPlugin — §12.1's first example.
func TestASingleProviderNeedsOnlyItsPlugin(t *testing.T) { /* providers: [- plugin: aws] */ }

// TestNameDefaultsToThePluginName.
func TestNameDefaultsToThePluginName(t *testing.T) { /* one unnamed aws entry is called "aws" */ }

// TestTwoInstancesWithTheSameNameIsAnError, in BOTH spellings §12.1 names: two
// unnamed entries of one plugin, and two both named the same. Assert the
// diagnostic names the collision AND both origins — a user with six instances
// needs to know which two.
func TestTwoInstancesWithTheSameNameIsAnError(t *testing.T) { /* ... */ }

// TestTheFirstEntryIsTheDefault, and TestDefaultTrueOverridesOrder. Both, because
// the first alone passes against an implementation that ignores `default:`.
func TestTheFirstEntryIsTheDefault(t *testing.T) { /* ... */ }
func TestDefaultTrueOverridesOrder(t *testing.T) { /* ... */ }

// TestTwoEntriesMarkedDefaultIsAnError. There is no precedence to invent, and
// either choice sends some resources to the wrong account.
func TestTwoEntriesMarkedDefaultIsAnError(t *testing.T) { /* ... */ }

// TestConfigAndDefaultsAreSeparate is what §12.1's nesting exists for: `iam-role`
// lands in Config, `tags` under `defaults:` lands in Defaults, and neither
// appears in the other. Assert BOTH absences — a decoder that put everything in
// one map would pass a test that only checked presence.
func TestConfigAndDefaultsAreSeparate(t *testing.T) { /* ... */ }

// TestAProvidersBlockThatIsAMapIsRefused. `providers: {aws: {...}}` cannot express
// two aws entries, which is the collision §12.1 refuses — a shape that cannot
// express it would refuse it SILENTLY. The diagnostic must show the list form.
func TestAProvidersBlockThatIsAMapIsRefused(t *testing.T) { /* ... */ }

// TestPluginIsRequired.
func TestPluginIsRequired(t *testing.T) { /* an entry with only a name */ }
```

- [ ] **Steps 1.2-1.6:** fail, implement, pass, discriminate (make names collide silently; ignore
      `default:`; merge Config and Defaults into one map), commit.

---

## Task 2: a resource names its instance

**Files:** Modify `internal/config/declarations.go`, `internal/config/decode.go`; test alongside.

**Interfaces:**
- Produces: `ResourceDecl.Provider AttributeDecl` — a NAME, resolved in Task 4.

`provider:` joins `skip`/`only`/`lifecycle` as a named resource key rather than an attribute, for
the reason Task 4 of M9 records: anything the switch does not recognise becomes an attribute, and
`provider` would then reach stage 7 as "no such attribute" on every resource that uses it.

- [ ] **Step 2.1: Failing tests** — it decodes; it is NOT an attribute; it survives in a module
      file (one decoder serves both, so this pins the sharing).
- [ ] **Steps 2.2-2.6.**

---

## Task 3: resolve instances, after variables

**Files:**
- Create: `internal/providers/resolve.go`, `internal/providers/resolve_test.go`
- Modify: `internal/compiler/compile.go`

§12.1's variables rule. This runs after stage 4 and before stage 5.

**Interfaces:**
- Produces: `providers.Instance{Name, Plugin string, Config map[string]value.Value,
  Defaults map[string]value.Value, Default bool}` and
  `providers.Resolve(decls []config.ProviderDecl, scope variables.Scope) (map[string]Instance, diag.Diagnostics)`.

- [ ] **Step 3.1: Failing tests**

```go
// TestAnInstancesConfigInterpolates — the point of the whole rule. `iam-role:
// ${aws_role}` resolving differently in two scopes, asserted in BOTH, because one
// cannot tell "resolved" from "resolved correctly".
func TestAnInstancesConfigInterpolates(t *testing.T) { /* ... */ }

// TestACompositeConfigValueInterpolatesPerLeaf, through M10's WalkLeaves rather
// than a second walk.
func TestACompositeConfigValueInterpolatesPerLeaf(t *testing.T) { /* ... */ }

// TestAResourceReferenceInProviderConfigIsAnError. §12.1: a provider's
// configuration is needed before any resource exists, so `${db.arn}` would have
// the provider create the thing its own credentials depend on. An ERROR naming
// that, not an unknown that fails later — and the message must say why, because
// "unknown value" would read as a bug.
func TestAResourceReferenceInProviderConfigIsAnError(t *testing.T) { /* ... */ }

// TestTheResolvedTableIsKeyedByInstanceName, with exactly one Default.
func TestTheResolvedTableIsKeyedByInstanceName(t *testing.T) { /* ... */ }
```

- [ ] **Steps 3.2-3.6.** Discrimination must include resolving BEFORE variables (the config comes
      out holding raw `${...}`) and allowing a resource reference (it becomes an unknown that
      surfaces as a credential failure at apply, which is the outcome the error exists to avoid).

---

## Task 4: the registry becomes instance-aware

**Files:**
- Modify: `internal/registry/registry.go`, every caller of `Registry.Provider`
- Test: `internal/registry/registry_test.go`

The change everything else rests on. `Registry` is keyed by resource TYPE and `Register` REFUSES a
type a second provider already claims — two instances of one plugin collide there immediately.

**Interfaces:**
- Produces: `Registry.ProviderFor(resourceType, instance string) (provider.Provider, bool)` and
  registration that takes an INSTANCE NAME alongside the provider.
- The five existing call sites: `executor/apply.go` ×2, `refresh/refresh.go` ×2, `cli/import.go`.

- [ ] **Step 4.1: Failing tests**

```go
// TestTwoInstancesOfOnePluginBothRegister — today this is an error, and it is the
// whole point of the milestone.
func TestTwoInstancesOfOnePluginBothRegister(t *testing.T) { /* ... */ }

// TestLookupIsByTypeAndInstance. Two instances of one plugin, and a lookup for
// each must return a DIFFERENT provider object — assert they differ, because
// returning the same one for both is the bug that would send every resource to
// one account.
func TestLookupIsByTypeAndInstance(t *testing.T) { /* ... */ }

// TestAnUnknownInstanceIsRefused, naming the declared ones.
func TestAnUnknownInstanceIsRefused(t *testing.T) { /* ... */ }

// TestDefinitionsStillCollideWithinOneInstance. The old check was right about one
// thing: two PLUGINS claiming one type inside a single instance is still an error,
// because nothing could then say which owns it.
func TestDefinitionsStillCollideWithinOneInstance(t *testing.T) { /* ... */ }
```

- [ ] **Steps 4.2-4.6.** Discrimination: return the same provider for both instances (the
      difference assertion fails and nothing else does), and drop the within-instance collision
      check.

---

## Task 5: dispatch — and the destroy path especially

**Files:** Modify `internal/executor/apply.go`, `internal/refresh/refresh.go`,
`internal/cli/import.go`, `internal/compiler/` (carry the instance onto the resolved resource),
`providers/test/provider.go` (write the instance name into state).

This is where the milestone's governing rule is honoured or lost.

- [ ] **Step 5.1: Failing tests**

```go
// TestAResourceIsCreatedInTheInstanceItNames — two fake clouds, and the resource
// appears in ONE. Assert the other is untouched; asserting only presence passes
// against a build that created it in both.
func TestAResourceIsCreatedInTheInstanceItNames(t *testing.T) { /* ... */ }

// TestAResourceWithNoProviderKeyUsesTheDefault.
func TestAResourceWithNoProviderKeyUsesTheDefault(t *testing.T) { /* ... */ }

// TestADESTROYEDResourceIsDeletedFromTheInstanceStateNames is THE test of this
// milestone. Remove the resource from configuration entirely, so nothing but
// state says which instance owns it, and assert it is deleted from that cloud and
// the OTHER cloud is untouched.
func TestADestroyedResourceIsDeletedFromTheInstanceStateNames(t *testing.T) { /* ... */ }

// TestStateRecordsTheInstanceNameNotThePluginName. Two instances of one plugin
// both have plugin "test"; if state recorded the plugin, the test above could not
// tell them apart and would pass while deleting from the wrong account.
func TestStateRecordsTheInstanceNameNotThePluginName(t *testing.T) { /* ... */ }

// TestAStateEntryNamingNoDeclaredInstanceIsReported — the migration case. NOT a
// silent fall back to the default: a wrong guess deletes real infrastructure in
// the wrong account. Assert the message names the claimed instance and the
// declared ones.
func TestAStateEntryNamingNoDeclaredInstanceIsReported(t *testing.T) { /* ... */ }

// TestAV1StateFileNamingThePluginStillWorks — the common migration case, where an
// unnamed instance is named after its plugin so the existing value is already
// correct. No version bump.
func TestAV1StateFileNamingThePluginStillWorks(t *testing.T) { /* ... */ }

// TestMovingAResourceBetweenInstancesIsADestroyAndACreate — §12.1. The resource
// genuinely lives in a different account, and the plan must say so where a user
// reads it before typing apply.
func TestMovingAResourceBetweenInstancesIsADestroyAndACreate(t *testing.T) { /* ... */ }
```

- [ ] **Steps 5.2-5.6.** Discrimination, and every one of these must fail alone: dispatch from
      configuration instead of state on the destroy path; record the plugin name in state; fall
      back to the default for an unknown instance.

---

## Task 6: `defaults:`

**Files:** Modify `internal/compiler/schema.go`, `pkg/value/scope.go`, `internal/registry/registry.go`.

The rung §12.1 describes: explicit on the resource → the instance's `defaults:` → the plugin's
schema default.

- [ ] A new `value.Scope` for provenance, so a plan says where the value came from rather than
      crediting the resource. **Check the state-version guard first**: M7 recorded that adding a
      Scope trips a test demanding a version bump, and that bumping routes every existing file
      through the lossy migration. Read that test's comment before adding the constant.
- [ ] A resource's own value REPLACES the block's, whole. The fixture's two maps must share a key
      and differ in another, so a merge would visibly produce a third thing.
- [ ] A `defaults:` key no resource type of that plugin declares is an ERROR listing what exists.
      `tag:` for `tags:` would otherwise apply to nothing, forever, with no output in which its
      absence is visible.
- [ ] `prevent_destroy` and `retain` are accepted and apply to every resource; the lifecycle names
      are reserved, and a plugin declaring an attribute that collides is rejected at REGISTRATION,
      in the first loop, because `Register` validates before mutating.
- [ ] Generation omits a value that came from `defaults:` — it is not something the reader must
      supply.

---

## Task 7: the whole thing through the binary

**Files:** Create `tests/integration/m11_test.go`.

- [ ] Two instances of the fake provider, two clouds, resources split between them by `provider:`,
      applied and converging.
- [ ] Each cloud holds only its own resources. Assert both, and assert the counts DIFFER.
- [ ] Instance configuration differing per environment through variables.
- [ ] `defaults:` reaching resources of one instance and not the other's.
- [ ] Remove a resource and watch it destroyed from the right cloud with the other untouched.
- [ ] Two same-named instances fail `validate` naming both lines.
- [ ] Update `examples/shop` only if this makes its README wrong. A second provider instance may be
      more than that example should carry — decide deliberately and say which.

---

## Self-review notes

Five claims, ALL VERIFIED against the code before this plan was committed. Recorded with their
findings rather than as instructions, because two of them came back slightly different from what
was assumed:

1. **VERIFIED: exactly five callers** — `cli/import.go:119`, `executor/apply.go:242` and `:439`,
   `refresh/refresh.go:97` and `:155`. A missed one dispatches to the wrong account.
2. **VERIFIED, with a correction.** `ResourceState.Provider` is written only by the provider
   itself (`providers/test/provider.go:369`, `Provider: p.Name()`) and read in THREE places, not
   two: `cli/state.go:76` (display) and `executor/summary.go:171` and `:175` (both emptiness
   checks). Nothing dispatches on it, which is what Task 5 changes. `discovery/walk.go:66` writes
   a `Provider` field too, but on a `discovery.Result` rather than on state.
3. **VERIFIED: the migration registry exists and refuses a non-advancing migration.** The LOSSY
   part is upstream of it and was found in M7: a file not at `CurrentVersion` goes through
   `json.Unmarshal` into `map[string]any`, where a JSON number becomes a float64, so an integer
   beyond 2^53 comes back wrong — `9007199254740993` reads back as `9007199254740992`, which the
   state package's own golden test demonstrates. That is why this milestone avoids a version bump.
4. **VERIFIED: `pkg/value/scope_test.go:176` demands a state version bump for a new Scope.** M7
   hit it, attempted the bump, and reverted when the state package's golden test showed the
   migration was lossy. Read that comment before Task 6 adds a constant — the answer may be to
   render provenance without a new Scope at all.
5. **VERIFIED: `test.New(cloudPath)` takes its cloud path as a parameter**, so two instances with
   two clouds are constructible. That is how this milestone is testable without AWS at all, and it
   is what the fake provider exists for.
