# M10 — Composite Interpolation, Literals, and `merge()`

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to
> implement this plan task-by-task.

**Goal:** Make the expression language able to express a map whose values interpolate, and to
combine two of them — which is what everything asking for provider-wide tags actually needs
underneath.

**RESCOPED mid-milestone.** This plan originally ended with a `provider:` block (§12.1). The
owner replaced that design with provider INSTANCES — a `providers:` list, named instances of one
plugin, per-resource selection, and `defaults:` nested inside each. That is a milestone of its
own (M11) and not a task: it changes the registry's one-provider-per-type key, five provider
lookups, and what `ResourceState.Provider` means, which is a state migration. Tasks 1-4 stand on
their own and are what M10 now is.

**Architecture:** Four capabilities, strictly ordered because each is load-bearing for the next.
One scanner replaces two. Composite values learn to carry interpolations, in ONE walk that three
existing refusal sites call. `merge()` joins the fixed function set, with literals permitted in
argument position.

**Tech Stack:** Go 1.24, Cobra, `gopkg.in/yaml.v3`. No new dependency.

**Spec:** `PLAN.md` §10.1 (composite interpolation), §10.2 (functions, purity, sensitivity),
§10.3 (argument-position literals), §10.4 (one scanner), §43 (provenance), §36 (redaction).
§12.1 moved to M11.

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
- **A fixture must contradict its expected output.**
- **A sabotage must leave the code COMPILING and change behaviour.**
- **A test of a mechanism must exercise the one path where that mechanism is load-bearing**, and
  a sabotage is the only way to know which path that is. M9 produced three tests that asserted
  true things about code they never reached.
- **Restore a sabotage with `cp` from a backup, never `git checkout <file>`.**

## The rule that governs this milestone

**Sensitivity is per leaf, and every new path that touches a composite is a new chance to lose
it.**

M10 adds two of them: the walk that evaluates leaves inside a map, and `merge()` combining two
maps. (The third, a provider supplying a map to every resource, moved to M11 with §12.1.) A
secret is most likely to reach a TAG, and a tag is the value most likely to be printed, exported,
and committed.

`pkg/value` already models per-leaf sensitivity and `value.Format` already redacts at that
granularity. Every task below must use them rather than deciding again — and `join()` and
`replace()` have each already shipped a hand-written union that was wrong, one of which let a
secret search term reveal its position through an unclassified result.

**Ordering is not negotiable.** Task 1 before Task 3, because adding literals to two scanners
means editing both in lockstep — verified retrospectively: Task 3's first sabotage narrows the
shared scanner back to parens only and fails every map-literal test.

---

## Task 1: one scanner

**Status: done** (`88f81c6`). The depth counter was deliberately NOT unified — see the commit.

**Files:**
- Modify: `internal/expressions/parse.go`
- Test: `internal/expressions/parse_test.go`

**Interfaces:**
- Produces: an unexported scanner that `matchBrace` and `splitArgs` both drive. No exported
  surface changes.

§10.4. Pure refactor, NO new behaviour — which is exactly why it goes first and alone: a
behaviour-preserving change is only provably behaviour-preserving while nothing else moves.

- [ ] **Step 1.1: Pin today's behaviour BEFORE touching anything.**

Write a table test over the existing `Parse`, covering every shape the two scanners disagree
about or might: nested `${}`, quotes containing `}`, quotes containing `,`, escaped quotes,
unbalanced `(`, unbalanced `{`, an empty argument, a trailing comma. Assert the DIAGNOSTIC
SUMMARY, not just success or failure, because the whole risk of this task is a message changing.

```go
// TestParseShapesAreUnchangedByTheScannerRewrite is a characterisation test: it
// records what the two scanners do TODAY so the unification can be shown not to
// alter it. Each case names the shape rather than the expectation, so a wrong
// expectation is visible as a wrong name.
func TestParseShapesAreUnchangedByTheScannerRewrite(t *testing.T) { /* ... */ }
```

- [ ] **Step 1.2: Run it green against the OLD code.** A characterisation test that fails before
      the refactor is recording a bug, not the behaviour.
- [ ] **Step 1.3: Unify.** One walk maintaining quote state and a single nesting depth across
      every delimiter pair; `matchBrace` stops at depth zero on `}`, `splitArgs` cuts at depth
      zero on `,`. Delete the duplicated loops.
- [ ] **Step 1.4: Run it green again.** Any difference is either a bug being fixed (say so
      explicitly, in the commit) or the refactor being wrong.
- [ ] **Step 1.5: Discrimination.** A shared depth counter changes one thing: a stray unmatched
      `(` inside an interpolation now consumes the closing `}`. Add the case, assert the message,
      and say in the test which behaviour is new — do not let it hide inside the characterisation
      table.
- [ ] **Step 1.6: Commit.**

---

## Task 2: interpolation inside a composite value

**Status: done** (`5d6f04f`).

**Files:**
- Create: `internal/expressions/composite.go`, `internal/expressions/composite_test.go`
- Modify: `internal/compiler/bind.go`, `internal/modules/outputs.go` (both refusal sites)

§10.1.

**Interfaces:**
- Produces: `func EvaluateDeep(v value.Value, scope Scope, origin value.Origin) (value.Value, diag.Diagnostics)`
  — walks a value, parsing and evaluating every string leaf that contains an interpolation, and
  returning a value of the same shape.
- Consumes: `Parse`, `Evaluate`, and whatever the caller uses to qualify references. The caller
  qualifies; this walks.

- [ ] **Step 2.1: Failing tests**

```go
// TestALeafInsideAMapIsEvaluated — §10.1's worked example.
func TestALeafInsideAMapIsEvaluated(t *testing.T) { /* tags: {environment: ${environment}} */ }

// TestALeafWithNoInterpolationIsUntouched. Not an optimisation: parsing a literal
// that happens to contain a brace would change it.
func TestALeafWithNoInterpolationIsUntouched(t *testing.T) { /* ... */ }

// TestNestingWorksToAnyDepth. Refusing depth two while allowing depth one is a
// rule nobody could predict, so a fixture that is only one level deep cannot
// pin this.
func TestNestingWorksToAnyDepth(t *testing.T) { /* a map of lists of maps */ }

// TestAKeyIsNeverInterpolated. Keys are literal, so a configuration's SHAPE
// never depends on a value — assert that `${x}` used as a key stays the literal
// text and is not resolved.
func TestAKeyIsNeverInterpolated(t *testing.T) { /* ... */ }

// TestSensitivityIsPerLeaf is THE test of this milestone's governing rule. A map
// with one secret leaf must render with ONE leaf redacted and the others
// visible. Assert both halves: a wholly-redacted map hides information the user
// needs, and a wholly-visible one leaks.
func TestSensitivityIsPerLeaf(t *testing.T) { /* ... */ }

// TestAnUnknownLeafMakesTheCompositeUnknownAndRecordsTheEdge. The resource
// genuinely cannot be created until it resolves, and the edge must come from the
// leaf — a missing edge here is a clean plan and a failed apply, which is the
// defect M5's integration suite found twice.
func TestAnUnknownLeafMakesTheCompositeUnknownAndRecordsTheEdge(t *testing.T) { /* ... */ }
```

- [ ] **Steps 2.2-2.4:** fail, implement, pass.
- [ ] **Step 2.5: Delete all three refusals, and prove they were one rule.** After wiring, grep:
      ```bash
      grep -rn "interpolation inside" --include=*.go . | grep -v _test.go
      ```
      Expected: nothing. If any site still has its own logic rather than calling `EvaluateDeep`,
      that is the duplication §10.1 names, and a module output will eventually disagree with a
      resource attribute about the same YAML.
- [ ] **Step 2.6: Discrimination.** Return the composite unwalked (the old behaviour minus the
      diagnostic): every test above must fail. Then redact the whole map rather than the leaf:
      only `TestSensitivityIsPerLeaf` fails. Then drop the edge: only the edge test fails.
- [ ] **Step 2.7: Commit.**

---

## Task 3: literals in argument position

**Status: done** (`e29954c`).

**Files:** Modify `internal/expressions/parse.go`; test in `parse_test.go`.

§10.3. Depends on Task 1 — do not start it first.

- [ ] **Step 3.1: Failing tests**

```go
// TestAMapLiteralIsAnArgument — `merge(tags, {team: payments, project: billing})`.
// The comma INSIDE the braces is the point: before Task 1 this split into three
// arguments.
func TestAMapLiteralIsAnArgument(t *testing.T) { /* ... */ }

// TestAListLiteralIsAnArgument, and a nested one, because the depth counter is
// the mechanism and one level cannot prove a counter.
func TestAListLiteralIsAnArgument(t *testing.T) { /* ... */ }

// TestALiteralOutsideAnArgumentIsRefused is the bound. `cidr: ${{a: b}}` must be
// an error naming what is allowed — the language is not gaining literals, only
// function arguments are.
func TestALiteralOutsideAnArgumentIsRefused(t *testing.T) { /* ... */ }

// TestAnUnquotedMapLiteralIsCaughtByTheLoader. YAML rejects it first, with
// "mapping values are not allowed in this context", which says nothing about
// quoting. The loader must recognise that shape and say what to do.
func TestAnUnquotedMapLiteralIsCaughtByTheLoader(t *testing.T) { /* in internal/config */ }
```

- [ ] **Steps 3.2-3.6:** fail, implement, pass, discriminate (remove brace tracking from the
      shared scanner: the map-literal tests must fail and nothing else), commit.

---

## Task 4: `merge()`

**Status: done** (`59bec7e`).

**Files:** Modify `internal/expressions/funcs.go`; test in `funcs_test.go`.

§10.2.

- [ ] **Step 4.1: Failing tests**

```go
// TestMergeUnionsMapsWithLaterArgumentsWinning. The fixture must have a KEY IN
// BOTH maps with different values, or it cannot tell a union from a
// concatenation, nor which side wins.
func TestMergeUnionsMapsWithLaterArgumentsWinning(t *testing.T) { /* ... */ }

// TestMergeIsVariadic — three maps, because two cannot distinguish "folds left"
// from "takes exactly two".
func TestMergeIsVariadic(t *testing.T) { /* ... */ }

// TestMergeRefusesANonMap. `merge(tags, "x")` is a mistake, not an empty result.
func TestMergeRefusesANonMap(t *testing.T) { /* ... */ }

// TestMergeUnionsSensitivityPerLeaf is the milestone rule again, and the THIRD
// time this codebase has had to get it right: join() and replace() each shipped
// a hand-written union that was wrong. Merge a public map with one holding a
// secret and assert exactly one leaf is redacted — not none, and not all.
func TestMergeUnionsSensitivityPerLeaf(t *testing.T) { /* ... */ }

// TestMergeDoesNotMutateItsArguments. The scope's map is shared; mutating it
// would make a later reference to the same variable see the merged result, which
// would depend on evaluation order.
func TestMergeDoesNotMutateItsArguments(t *testing.T) { /* ... */ }
```

- [ ] **Step 4.2-4.5:** fail, implement via the EXISTING `anySensitive` helper rather than a new
      union, pass, discriminate.
- [ ] **Step 4.6: Update the function-set guard.** `funcs.go` says the set is fixed at six and
      that changing it requires a spec amendment. It is now seven, the amendment is §10.2, and
      any test pinning the count must be updated deliberately — not silently.
- [ ] **Step 4.7: Commit.**

---

## Task 5: MOVED to M11

The `provider:` block specified here is superseded by §12.1's provider instances. See
`2026-09-12-infra-m11-providers.md`. The parts of it that were right and carry over: the
fail-closed rule for a key nothing declares, the reserved lifecycle names checked at
registration, the provenance rung, and generation omitting a provider-supplied value.

---

## Task 6: the expression work through the binary

**Files:** Create `tests/integration/m10_test.go`.

What Tasks 1-4 actually deliver, end to end, with the provider-block half removed:

- [ ] A map whose values interpolate, planned in two environments, with the values DIFFERING —
      the case §10.1 exists for. Assert both environments, because one cannot tell "resolved" from
      "resolved correctly".
- [ ] `merge()` combining a variable map with an inline literal, in the owner's own spelling:
      `tags: "${merge(base_tags, {team: payments, project: billing})}"`. Assert the union AND that
      the literal wins the shared key.
- [ ] **A SECRET inside a map is redacted in the plan and absent from `export`** — the assertion
      deferred from Task 2, which could not reach it at the compiler level because sensitivity
      arrives at apply. This is the milestone's governing rule at the only place a user meets it,
      so it goes through a real `apply` and then a real `plan` and `export`.
- [ ] A dependency edge recorded from INSIDE a map: apply must order the two resources, not just
      plan them. A missing edge here is a clean plan and a failed apply.
- [ ] The unquoted-literal YAML error carries the quoting hint.
- [ ] Update `examples/shop` to use an interpolated tag map, and check its README still describes
      what the example does.

---

## Self-review notes

Four things this plan asserts that the implementer should verify rather than trust:

1. **`HasExpressions` is set whenever any leaf holds `${`, and leaves are never parsed.** Task 2
   rests on this. Read `decodeValue` before writing the walk.
2. **`value.Format` already redacts per leaf.** If it does not, Task 2 is bigger than it looks
   and the governing rule needs its own task.
3. **`anySensitive` exists and unions across arguments.** Task 4 says to reuse it rather than
   write a third union.
4. **`registry.Register` validates before mutating.** Carried to M11, where the reserved-name
   check must go in the FIRST loop, or a failed registration leaves the registry half-populated.

All four were verified against the code before Task 1 began, and all four held.
