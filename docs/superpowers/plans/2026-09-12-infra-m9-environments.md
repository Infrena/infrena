# M9 — Environments: `skip`/`only`, Reachability, and Withdrawing Class Defaults

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to
> implement this plan task-by-task.

**Goal:** Make environments do what §6 says they do — cheap, numerous, identical infrastructure
differing only by variables — by adding per-resource environment filtering, making a removed
environment tear down visibly, and deleting the second (invisible) mechanism for environment
variation.

**Architecture:** One new compiler concern, expressed as a marking rather than a filter:
`skip`/`only` resolve during stage 5 and mark instances, stage 6 reports references to marked
instances, and the marked set is dropped before stage 7. Environment reachability is a change to
one guard in stage 3 plus the planner's empty-desired-state path, which already exists.
Withdrawing §13 deletes code rather than adding it.

**Tech Stack:** Go 1.24, Cobra, `gopkg.in/yaml.v3`. No new dependency.

**Spec:** `PLAN.md` §6.1 (reachability), §6.2 (`skip`/`only`), §6.3 (process variables),
§13 (withdrawn, with the reasoning), §38 (protections declared, not implied).

## Global Constraints

- **Exactly two third-party dependencies.** `git diff --stat <BASE> HEAD -- go.mod go.sum` must
  print nothing.
- **`internal/config` is the ONLY package permitted to touch `yaml.Node`**, with
  `internal/generator` the one argued exception (it emits, never parses).
- **Exactly one redaction path**, `pkg/value.Format`.
- **`internal/graph` imports no other infra package; `providers/*` import no `internal/*`.**
- **`mise` is inactive in non-interactive shells.** Every command needs
  `export PATH="$HOME/.local/share/mise/shims:$PATH"`.
- **`-count=1` on every test run.** `tests/integration` shells out to `go build`.
- **Commit discipline:** `git add <explicit paths>` then `git commit -m "..." -- <same paths>`.
  Forbidden: `git add -A`, `git add .`, `git commit -am`.
- Diagnostics collect rather than fail fast, and each says what is wrong, where, what was
  expected, and an action that works **today**.
- Anything user-observable is sorted once — never Go's map order.
- **Assert the thing did not happen, not that we said it would not happen.**
- **A fixture must contradict its expected output.** A one-element fixture cannot prove a sort.
- **A sabotage must leave the code COMPILING and change behaviour.** One that breaks the build
  fails every test and discriminates nothing.
- **A test of a mechanism must exercise the one path where that mechanism is load-bearing.**
  "It passes either way" is the only evidence that tells you which path that is — so every task
  below ends by sabotaging, not by observing green.
- **Restore a sabotage with `cp` from a backup, never `git checkout <file>`.** M7 lost an entire
  file's uncommitted work that way.

## The rule that governs this milestone

**A skipped resource is exactly as if it were never declared — except that something must still
know it was skipped.**

Those two halves pull against each other and every defect in this milestone will live between
them. "As if never declared" is what makes invariant 1 destroy it, which is the feature. But a
resource that simply vanishes makes `${debug_box.url}` report "no such resource", which sends a
user hunting for a typo in a name that is right there in the file.

So skipped instances are MARKED, not removed, and are dropped at exactly one place: after
reference binding, before the planner. Any task that deletes them earlier is wrong — see Task 5's
correction for the real reason, which is not the one written here first.

---

## Task 1: `${project}` as a process variable

**Status: done** (`d75ce2d`).

**Files:**
- Modify: `internal/variables/resolve.go` (`ProcessVariables`), `internal/compiler/compile.go`
  (`seedProcessVariables`)
- Test: `internal/compiler/compile_test.go`, `tests/integration/m9_test.go`

**Interfaces:**
- Produces: `variables.ProcessVariables` gains `"project"`, so `internal/modules` carries it
  across a module boundary with the other three at no extra cost.

First and alone, because §6.2's own examples interpolate it and every later task's fixtures will.

- [ ] **Step 1.1: Write the failing tests**

```go
// TestProjectIsAProcessVariable — §6.3. A resource name or a tag almost always wants it.
func TestProjectIsAProcessVariable(t *testing.T) {
	// project: MainApp, a resource with cidr: ${project}-${environment}
	// Assert the resolved value is "MainApp-dev", and that its Scope is
	// ScopeCLIOverride with SuppliedBy naming where it came from — the other
	// three are authoritative and this one must be too, or a --var could
	// change what the project is called halfway through a plan.
}

// TestProjectCrossesAModuleBoundary. A module sees its own inputs and the process
// variables and nothing else (§11.3); `project` must be in the second group.
// Assert from INSIDE a module, because internal/modules carries the list
// separately from where it is seeded.
func TestProjectCrossesAModuleBoundary(t *testing.T) { /* ... */ }

// TestAProjectVariableDeclaredByTheUserIsStillRefused. `project` joins the
// reserved names: --var project=other must be refused the way environment is,
// or a resource name could claim one project while state records another.
func TestAProjectVariableDeclaredByTheUserIsStillRefused(t *testing.T) { /* ... */ }
```

- [ ] **Step 1.2: Run them, see them fail** — `undefined variable "project"`.
- [ ] **Step 1.3: Minimal code.** Add `"project"` to `ProcessVariables` (KEEP IT SORTED — the
      list is walked in order and a module's scope is built from it). Seed it in
      `seedProcessVariables` from `cfg.Project`; note that `seedProcessVariables` currently takes
      only `Options`, so the project name has to reach it — pass it as a parameter rather than
      widening `Options`, because `Options` is the invocation and the project name is the
      configuration.
- [ ] **Step 1.4: Run, see them pass.**
- [ ] **Step 1.5: Discrimination.** Remove `"project"` from `ProcessVariables` but keep the seed:
      the module test must fail and the root test must still pass. That is the whole point of the
      list being separate from the seeding, and if both fail together the module test is not
      testing what it claims.
- [ ] **Step 1.6: Commit.**

---

## Task 2: withdraw environment-class defaults

**Status: done** (`cf4b76b`).

**Files:**
- Modify: `internal/compiler/schema.go` (delete `EnvironmentType`), `internal/cli/import.go`,
  `internal/cli/export.go`, `internal/cli/explain.go`, `internal/cli/init.go`,
  `providers/test/definitions.go`, `pkg/schema/attribute.go`
- Test: the existing tests that assert production defaults, which must be rewritten rather than
  deleted.

§13. This task DELETES a feature. Do it before `skip`/`only` so the later fixtures are not
written against behaviour that is about to go.

- [ ] **Step 2.1: Find every test that depends on it first.**

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
grep -rn "EnvironmentType\|production" --include=*_test.go . | grep -v "^./tests/integration/m8"
```
Expected: `internal/cli/explain_test.go` (the "or 100 in production" rendering),
`internal/compiler/schema_test.go`, `tests/integration/*`, `examples/shop`. Each is a decision,
not a deletion — write down which of the two it is BEFORE editing:
  - it tested that defaults vary by environment → delete, and say so in the commit
  - it happened to use production as a fixture → change the fixture, keep the test

- [ ] **Step 2.2: Delete the mechanism.**
  - `internal/compiler/schema.go`: delete `EnvironmentType` and its call in `defaultContextFor`.
  - `pkg/schema/attribute.go`: delete `DefaultContext.EnvironmentType`. The field going away is
    what makes this irreversible-by-accident: a provider cannot quietly keep using it.
  - `providers/test/definitions.go`: `test.database.size` defaults to `10` unconditionally.
  - `internal/cli/explain.go`: `describeDefault` no longer probes two contexts. It prints one
    default. The two-context probe is the only reason that function is complicated.
  - `internal/cli/import.go`, `export.go`: drop the `EnvironmentType` field from the contexts
    they build.
- [ ] **Step 2.3: `type:` in an environment becomes an error.**

In `internal/config/decode.go`'s `decodeEnvironmentBody`, add a case BEFORE the `default:` arm:

```go
case "type":
	ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "environment " + strconv.Quote(name) + " sets `type`, which no longer means anything",
		Detail: "Environment classification was withdrawn (PLAN.md §13): a provider default is " +
			"one value per attribute, and anything that differs between environments is a variable. " +
			"Left alone, `type:` would silently declare a VARIABLE named `type`, which is what it " +
			"did before this was an error.",
		Action: "Remove it. To vary a value by environment, set it in that environment's " +
			"variables. For production protections, see PLAN.md §38.",
		Origin: keyOrigin,
	})
```

An error rather than a silent variable is the whole point: someone writing `type: production`
expects it to do something.

- [ ] **Step 2.4: Update the scaffold and the example.**
  - `internal/cli/init.go`: remove `type:` from both environments AND the comment claiming
    "`type` drives provider defaults". That comment is currently false and is the first thing a
    new user reads.
  - `examples/shop/`: remove `type:` from `infra.yml`. Its README does not mention `type:`; check
    that it still does not mention production defaults, and fix `TestTheShopExampleStillWorks`
    if it asserted one.
- [ ] **Step 2.5: Run everything.** `go test ./... -count=1`. Expect breakage in exactly the
      places Step 2.1 listed. A failure anywhere else is a consumer nobody knew about — stop and
      write it down.
- [ ] **Step 2.6: Discrimination.** Re-add `EnvironmentType` to `DefaultContext` and have
      `test.database.size` read it again: `TestTheTypeKeyIsRefused` must still pass (it is about
      the decoder) while the defaults tests fail. The two halves of this task are independent and
      the tests must show that.
- [ ] **Step 2.7: Commit.**

---

## Task 3: an environment is reachable if declared OR stateful

**Status: done** (`1f86788`). The rule has THREE arms, not two — a project declaring no
environments at all still plans with any name. Verifying the plan's own claim found that.

**Files:**
- Modify: `internal/environments/resolve.go`, `internal/compiler/compile.go`, `internal/cli/`
  (wherever "unknown environment" is produced)
- Test: `internal/environments/resolve_test.go`, `tests/integration/m9_test.go`

§6.1.

**Interfaces:**
- Consumes: `state.Local.Get` — the reachability question needs to know whether state exists, and
  that is a backend concern, so the check belongs in the CLI where the backend already lives, NOT
  in `internal/environments`, which must stay a pure function of declarations.

- [ ] **Step 3.1: Write the failing tests**

```go
// TestPlanOnARemovedEnvironmentProposesDestroyingEverything — §6.1. Today this
// is "unknown environment", so the only reachable command is `destroy`, run blind.
func TestPlanOnARemovedEnvironmentProposesDestroyingEverything(t *testing.T) {
	// apply sandbox, remove sandbox from infra.yml, plan sandbox.
	// Assert: exit 2, every resource shown as a destroy, and the output SAYS WHY —
	// "no environment named \"sandbox\" is declared". A total destruction that
	// reads like an ordinary plan is the most alarming output this tool has.
}

// TestAnUnknownStatelessEnvironmentIsStillAnError is the half that keeps the rule
// a disjunction. Without it, `plan devv` silently plans nothing and exits 0, and
// a typo becomes a no-op instead of a diagnostic.
func TestAnUnknownStatelessEnvironmentIsStillAnError(t *testing.T) { /* ... */ }

// TestApplyingARemovedEnvironmentEmptiesAndRemovesItsState. The state file must
// GO, or the environment stays reachable forever and `infra state` lists a ghost.
func TestApplyingARemovedEnvironmentEmptiesAndRemovesItsState(t *testing.T) { /* ... */ }

// TestRefreshReachesARemovedEnvironment — refresh refuses today for the same
// reason plan does, and drift on something you are about to destroy is exactly
// what you want to see first.
func TestRefreshReachesARemovedEnvironment(t *testing.T) { /* ... */ }
```

- [ ] **Steps 3.2-3.4:** fail, implement, pass. The implementation is a guard, not a new code
      path: the planner already produces a destroy for everything in state when configuration
      declares nothing, because that IS invariant 1. What changes is that stage 3 stops refusing
      first, and the plan renderer gains the explanatory line.
- [ ] **Step 3.5: Discrimination.** Three sabotages, each failing ONE test:
      make reachability unconditional (the typo test fails); leave the state file behind (the
      state test fails); drop the explanatory line (the first test fails).
- [ ] **Step 3.6: Commit.**

---

## Task 4: decode `skip` and `only`

**Status: done** (`0954839`).

**Files:**
- Modify: `internal/config/declarations.go`, `internal/config/decode.go`
- Test: `internal/config/decode_test.go`

Decoding only. No behaviour yet — a resource carrying `skip:` still appears everywhere until
Task 5.

**Interfaces:**
- Produces: `ResourceDecl.Skip` and `ResourceDecl.Only`, each an `AttributeDecl` so the value
  travels with its Origin and its `HasExpressions` flag, and so stage 5 can evaluate it with the
  machinery that already evaluates attributes. NOT `[]string` — a bare slice loses the origin and
  cannot hold an expression.

- [ ] **Step 4.1: Write the failing tests**

```go
// TestSkipAcceptsAScalarOrAList — §6.2. `only: production` and
// `only: [staging, production]` are the same shape of thing.
func TestSkipAcceptsAScalarOrAList(t *testing.T) { /* both forms, both decode */ }

// TestSkipAndOnlyTogetherIsAnError. Two spellings of one idea that can contradict.
// Assert the diagnostic names BOTH keys and the resource.
func TestSkipAndOnlyTogetherIsAnError(t *testing.T) { /* ... */ }

// TestSkipIsNotAResourceAttribute. The one that keeps the namespace honest: after
// decoding, `skip` must NOT appear in ResourceDecl.Attributes, or stage 7 reports
// "no such attribute skip" on every resource that uses the feature.
func TestSkipIsNotAResourceAttribute(t *testing.T) { /* ... */ }

// TestSkipSurvivesInAModuleFile — §6.2 allows it inside a module, and module files
// are decoded by a different function (decodeModuleFile) than project files.
// Two decoders, one rule: the classic place for this to be implemented once.
func TestSkipSurvivesInAModuleFile(t *testing.T) { /* ... */ }
```

- [ ] **Steps 4.2-4.6:** fail, implement, pass, discriminate (delete the `skip`/`only` cases from
      the RESOURCE key switch and watch `TestSkipIsNotAResourceAttribute` fail — it will fail as
      "no such attribute", which is the diagnostic a user would have seen), commit.

---

## Task 5: resolve and mark

**Status: done** (`b20ce39`).

**Files:**
- Modify: `internal/modules/expand.go`, `internal/modules/inputs.go`
- Test: `internal/modules/skip_test.go`

The heart of the milestone.

**Interfaces:**
- Consumes: Task 4's `ResourceDecl.Skip`/`Only`, the level's `*Scope`.
- Produces: `Instance.Skipped bool` and `Instance.SkipOrigin value.Origin`. Stage 6 reads both.

- [ ] **Step 5.1: Write the failing tests**

```go
// TestOnlyKeepsAResourceInItsNamedEnvironments, and TestSkipRemovesIt. Both
// directions, and BOTH environments asserted in each — a filter tested in one
// environment cannot tell "kept everywhere" from "kept correctly".
func TestOnlyKeepsAResourceInItsNamedEnvironments(t *testing.T) { /* ... */ }

// TestAnUnknownEnvironmentNameIsAnError. A filter that quietly matches nothing is
// worse than no filter. Assert the diagnostic LISTS the declared environments —
// `skip: [prod]` against `production` is the case this exists for.
func TestAnUnknownEnvironmentNameIsAnError(t *testing.T) { /* ... */ }

// TestSkipMayBeAnExpression, resolving to a string and to a list. This is what
// lets a module be written with switchable parts, so it is the feature, not a
// generalisation.
func TestSkipMayBeAnExpression(t *testing.T) { /* only: ${replica_in} */ }

// TestAModuleCallsSkipResolvesInTheCallersScope, and the resources INSIDE resolve
// in the module's own. Assert a skipped CALL expands to nothing at all — not to
// marked instances, because the module was never entered and its resources have
// no addresses to mark.
func TestAModuleCallsSkipResolvesInTheCallersScope(t *testing.T) { /* ... */ }

// TestASkippedResourceIsMarkedNotDropped is the rule that governs this milestone.
// Assert the instance is PRESENT in the expansion with Skipped true. A task that
// drops it here makes Task 6 impossible, and the failure lands on a user as
// "no such resource".
func TestASkippedResourceIsMarkedNotDropped(t *testing.T) { /* ... */ }
```

- [ ] **Steps 5.2-5.6:** fail, implement, pass, discriminate, commit.

Discrimination must include: make the marking a drop instead, and confirm
`TestASkippedResourceIsMarkedNotDropped` is the only failure. If Task 6's tests also fail, they
are coupled to the representation rather than to the behaviour.

**CORRECTION, found by running that sabotage.** Task 6's tests do NOT fail, and the claim in
"The rule that governs this milestone" that dropping early "makes Task 6 impossible" is WRONG.
`Scope.skipped` carries the NAMES, and the names are what the reference rule reads — so dropping
in stage 5 leaves every Task 6 test passing and fails only the test asserting that the marking
exists, which is circular.

What the marking actually buys is that a skipped resource's own attributes are STILL BOUND, so a
mistake inside a `production`-only resource is reported when planning `dev` rather than surviving
until the production run. `TestASkippedResourceIsStillChecked` is the non-circular reason, and
the sabotage now fails it.

---

## Task 6: reference and `depends_on` to a skipped resource

**Status: done** (`713f00c`, with Task 7).

**Files:**
- Modify: `internal/compiler/bind.go`
- Test: `internal/compiler/skip_test.go`

§6.2's sharpest rule, and the reason Task 5 marks rather than drops.

- [ ] **Step 6.1: Write the failing tests**

```go
// TestAReferenceToASkippedResourceSaysSo. NOT "no such resource" — the name is
// right there in the file and a user would hunt for a typo that does not exist.
// Assert the diagnostic contains "skipped" AND the environment name AND the
// origin of the skip, so the reader can find the line that caused it.
func TestAReferenceToASkippedResourceSaysSo(t *testing.T) { /* ... */ }

// TestDependsOnASkippedResourceSaysSo — the same rule through the other door.
// depends_on is resolved separately from attribute references (fanOut), so this
// is a second code path, not a second spelling of one test.
func TestDependsOnASkippedResourceSaysSo(t *testing.T) { /* ... */ }

// TestASkippedResourceMayReferToALiveOne is the boundary. The edge points the
// harmless way and must not be reported: the skipped resource is leaving.
func TestASkippedResourceMayReferToALiveOne(t *testing.T) { /* ... */ }

// TestAReferenceToAGenuinelyMissingResourceStillSaysNoSuchResource. Without this,
// an implementation that reported EVERY unresolved name as "skipped" passes the
// first two tests and makes every typo in the language misleading.
func TestAReferenceToAGenuinelyMissingResourceStillSaysNoSuchResource(t *testing.T) { /* ... */ }
```

- [ ] **Steps 6.2-6.6.** The fourth test is the one to write first; it is the cheapest
      implementation's failure mode.

---

## Task 7: drop before the planner, and destroy what was skipped

**Status: done** (`713f00c`).

**Files:**
- Modify: `internal/compiler/compile.go`
- Test: `internal/compiler/skip_test.go`, `tests/integration/m9_test.go`

- [ ] **Step 7.1: Write the failing tests**

```go
// TestASkippedResourceIsNotInTheResolvedConfig — after binding, before the planner.
func TestASkippedResourceIsNotInTheResolvedConfig(t *testing.T) { /* ... */ }

// TestSkippingAnAppliedResourceProposesDestroyingIt is the ruling, through the
// binary: apply in dev, add `skip: [dev]`, plan, see a destroy.
func TestSkippingAnAppliedResourceProposesDestroyingIt(t *testing.T) { /* ... */ }

// TestPreventDestroyStillRefusesASkippedResource. The two features meet here, and
// the answer is that nothing special happens: a skipped resource is an ordinary
// removal, so an ordinary guard blocks it.
func TestPreventDestroyStillRefusesASkippedResource(t *testing.T) { /* ... */ }

// TestTheSameConfigurationPlansDifferentlyPerEnvironment — the milestone in one
// assertion. One file, `only: production` on one resource, planned in dev and in
// production, asserting the resource is ABSENT from one and PRESENT in the other.
func TestTheSameConfigurationPlansDifferentlyPerEnvironment(t *testing.T) { /* ... */ }
```

- [ ] **Steps 7.2-7.6.** Sabotage: drop skipped instances in stage 5 instead of here, and confirm
      Task 6's tests fail while these still pass. That pair of results is what proves the drop
      point is load-bearing rather than incidental.

---

## Task 8: the whole thing through the binary

**Status: done.** `tests/integration/m9_test.go`.

**Files:** Create `tests/integration/m9_test.go` (or extend it, if earlier tasks started it).

- [ ] A project with `dev`, `staging`, `sandbox` and `production`, one `skip:`, one `only:`, and
      a module whose internal resource is switched on by an input. Applied to all four.
- [ ] Assert each environment's state holds a DIFFERENT resource set, and that the fake cloud
      holds the union — same configuration, four outcomes, no variables involved beyond the
      filters.
- [ ] Remove `sandbox` from the configuration; `plan sandbox` proposes destroying everything and
      says why; `apply sandbox` does it; the state file is gone afterwards.
- [ ] Add `skip: [dev]` to an applied resource, and watch `plan dev` propose a destroy while
      `plan staging` proposes nothing.
- [ ] `type:` in an environment fails `validate` with a diagnostic naming what to use instead.
- [ ] Update `examples/shop` if any of this makes its README wrong, and re-run
      `TestTheShopExampleTeachesTheLayout`.

---

## Self-review notes

Three things this plan asserts that the implementer should verify rather than trust:

1. **The planner already destroys everything when configuration declares nothing.** Task 3 says
   this is invariant 1 and needs no new code. Check it before writing the guard; if it is not
   true, Task 3 is bigger than it looks.
2. **`depends_on` is resolved separately from attribute references.** Task 6 depends on this
   being two code paths. `fanOut` in `internal/modules` is the one to read.
3. **`seedProcessVariables` does not currently see the project name.** Task 1 says pass it as a
   parameter. If `Options` already carries something equivalent, prefer that and say so.

---

## Found during Task 1 — a blocker for M10, not for M9

M10's `provider:` block was designed around this shape:

```yaml
# the sketch that was approved
tags:
  environment: ${environment}
  project: ${project}
```

**It cannot work as written, for two independent reasons, both verified against
the binary:**

1. **A variable value may not contain an interpolation at all.** Not in
   `variables.yml`, not in an environment block, not even as a scalar:
   `Error: variable "tags" contains an interpolation — variables.yml is resolved
   before any expression scope exists, so ${...} here has nothing to refer to.`
   That is a layering fact, not an oversight.
2. **Interpolation inside a map is not supported anywhere**, including in a
   resource attribute: `Error: interpolation inside a map is not supported —
   expressions may appear in string values only.`

So M10 needs one new capability before the provider block is writable:
**interpolation inside a composite value.** With that, and variables staying
literal, the design works with the interpolation moved into the block itself:

```yaml
provider:
  test:
    tags:
      environment: ${environment}   # a process variable
      project: ${project}          # ditto, added by Task 1
      tier: ${tier}                # an ordinary LITERAL variable, per environment
```

Per-environment variation then comes from literal variables referenced inside
the map, which keeps variables literal and needs no change to when they resolve.
The alternative — allowing interpolation in variable values — means resolving
variables in dependency order, which is a much larger language change and one
the existing diagnostic argues against.

None of this blocks M9. Recorded here because it was found here.
