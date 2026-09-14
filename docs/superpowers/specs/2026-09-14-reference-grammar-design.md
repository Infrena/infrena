# Reference grammar: `var.` namespacing, map paths, and the bare-name slot

**Date:** 2026-09-14
**Status:** approved, not implemented
**Amends:** `PLAN.md` §9, §10, §10.4, §12.1, §57
**Sequence:** this is spec ONE of two. Spec two (provider-declared resource
references and the `${vpc}` projection) depends on it and is written separately.

---

## 1. Why

Today the parser tells a variable from a resource attribute by COUNTING
SEGMENTS (`internal/expressions/parse.go:478`): one segment is a variable, two
or more is a resource attribute. That single rule is the root of three defects,
which is what makes it worth replacing rather than patching.

**A map variable cannot be read.** `value.KindMap` exists and `PLAN.md` §9
promises map-typed variables, but there is no indexing syntax and none can be
added: `${tags.team}` is already spoken for as a resource reference. A map
variable can be passed whole or merged, and never reached into.

**The error for that case lies.** With a map variable `vpc` and no resource of
that name, `${vpc.arn}` reports "reference to undeclared resource or module
`vpc`" (`internal/compiler/bind.go:385`) and lists the known RESOURCE names. A
thing named `vpc` does exist; it is a variable, and the message never says so.
With a resource named `vpc` also present, the variable is shadowed silently.

**The two forms are indistinguishable by eye.** `${vpc}` and `${vpc.arn}` mean
entirely unrelated things — one a value from `vars/`, the other an attribute a
provider assigns during apply — and nothing marks which is which.

Confirmed empirically before designing (throwaway test against the real parser
and evaluator, scope holding both a variable `vpc` and a resource `vpc.arn`):

```
${vpc}        op=VarRef:vpc           known=true  raw=VARIABLE-VALUE
${vpc.arn}    op=ResourceRef:vpc.arn  known=true  raw=RESOURCE-ARN
${vpc.cidr}   op=ResourceRef:vpc.cidr known=false (deferred)
```

A fourth reason is forward-looking. Spec two wants `${vpc}` to mean "the
resource `vpc`, projected to whichever attribute the target expects" — the fix
for wiring an id where an arn was wanted. That slot is occupied by variables
today, and this change is what frees it.

---

## 2. The grammar

Five forms, each with exactly one meaning, and no rule that depends on counting
segments:

```yaml
${var.region}          a variable
${var.tags.team}       a path into a map variable                     (new)
${var.azs[0]}          an entry of a list variable                    (new)
${vpc.id}              an attribute of resource `vpc`
${vpc}                 the resource `vpc` itself   (reserved here; spec two)
```

**`var` is reserved as a resource name.** A resource so named makes `${var.x}`
mean two things. The error is reported at the DECLARATION, not at the
reference, because that is where the fix is. A VARIABLE named `var` remains
legal: `${var.var}` is unambiguous, and reserving it would be a rule with no
cause.

**Process variables move too.** `${var.environment}`, `${var.project}`. This is
what makes the rule total — no bare name anywhere resolves to a variable — and
a half-applied prefix is the kind of exception nobody remembers. `PLAN.md` §6.3
is amended accordingly.

**A bare single segment is an ERROR in this spec**, not yet a resource
reference:

```
${vpc} is not a reference.

Variables are written ${var.vpc}. A resource reference needs an attribute,
as ${vpc.id}.
```

Two reasons to spend the error here. It keeps this spec shippable alone — the
projection needs spec two's relationship table — and during migration every
un-prefixed variable announces itself with a message naming its own fix rather
than resolving to something wrong. Spec two replaces this error with the
projection; the diagnostic's existence is what makes that a one-line change
instead of a new parse path.

`module` stays reserved exactly as it is (`parse.go:437`, contract Amendment
14a). Nothing about that changes.

**What this deletes:** `parse.go:478`'s "One segment is a variable; two or more
is a resource attribute."

---

## 3. Path semantics

**A path indexes a map, and only a map**, to arbitrary depth. §10.1 settled the
depth argument for composites — "refusing depth two while allowing depth one
would be a rule nobody could predict" — and it holds here.

**Lists are indexed with BRACKETS**, maps with dotted keys, and the two compose
in either order:

```yaml
${var.azs[0]}                 an entry of a list
${var.subnets[0].cidr}        index, then key
${var.regions.us_east.azs[0]} key, then index
```

A path is therefore a SEQUENCE OF STEPS, each one a key or an index, applied
left to right.

**Brackets rather than a dotted `${var.azs.0}`**, for a reason that is not
cosmetic: a map may have a numeric-looking key, so `${var.ports.0}` is
genuinely ambiguous between key `"0"` and index `0`. Resolving it by the
value's runtime kind would make the meaning of a reference depend on the type
of the thing it names — the class of implicit rule this language avoids. With
brackets, `[0]` is always an index and `.0` is always a key, decided at parse
time with no value in hand.

The scanner already supports this. §10.4's unification SHIPPED — `parse.go`'s
`scanner` is "the ONE place" quote state and nesting are tracked — and
`splitArgs` already nests brackets (`open: "({["`, `close: ")}]"`) for §10.3's
list literals. Adding an index is a change to `parseReference`, not to
scanning, and introduces no new token.

**An index is an INTEGER LITERAL, and nothing else.** No `${var.azs[i]}`, no
`${var.azs[var.i]}`, no arithmetic. This is the line that keeps indexing a read
rather than a loop: reading a known list at a fixed position generates nothing,
while an index that can VARY is only useful if something varies it, and that is
the iteration §54 refuses. The restriction is the whole guard, so it is stated
as a rule rather than left as an omission.

No negative indices. `[-1]` for "last" makes a reference's meaning depend on a
length the reader cannot see, and it is additive later if it earns its place.

**An out-of-range index is a COMPILE-TIME ERROR**, on the same reasoning as a
missing key below — the list is fully resolved at stage 4, so out of range then
is out of range forever. The message gives the length, because that is the fact
the reader lacks:

```
var.azs has 3 entries; there is no index 5
```

**Indexing a map, or keying a list, names the mistake both ways.** `${var.tags[0]}`
says "`var.tags` is a map; index it by key, as `${var.tags.team}`", and
`${var.azs.first}` says the converse. Each points at the other form rather than
reporting a missing member.

**A missing key is a COMPILE-TIME ERROR, never an unknown.** The distinction is
load-bearing. Unknown means "does not exist yet" — a resource the executor will
create, whose value arrives later. A variable's map is fully resolved at stage
4, so a key absent then is absent forever, and staging it as unknown defers a
certain failure to apply time, where §20's safety story is weakest. The
diagnostic lists the keys that do exist, as `bind.go:388` already does for
resource names:

```
variable "tags" has no key "tema"

Known keys:
  team
  project
```

**A step into a SCALAR names the kind**: "`var.region` is a string; it has no
members." Not "no such key", which would send a reader hunting for a typo in a
name spelled correctly. (A step into the *wrong kind of container* — a map
indexed, a list keyed — is covered above, and points at the other form.)

### 3.1 Extraction must not launder a secret

`pkg/value` lets a CONTAINER be `Sensitive` itself, separately from its leaves —
`CarrySensitivity` sets `dst.Sensitive = src.Sensitive` on the container as
well as recursing, for maps and lists alike (`pkg/value/sensitivity.go`). A
variable declared `sensitive: true` holding either therefore carries the flag
on the container, and its leaves may carry nothing.

So the naive implementation — walk `Raw`, return the leaf `Value` — SILENTLY
DECLASSIFIES every secret in a sensitive map or list variable. `${var.creds.password}`
returns unflagged, reaches `Format`, and prints in clear in a plan. That is
§36's exact failure, and per `sensitivity.go`'s own note, "no amount of
correctness in Format can recover it."

**The rule: extraction unions the sensitivity of every container along the
path onto the result — keys and indices alike.** Sensitive at any depth is
sensitive at the end. An index is no different from a key here, and saying so
explicitly is what stops the list case being added later without the union.

This mirrors §10.2's "sensitivity unions across ALL arguments" — the rule
`join()` and `replace()` each shipped wrong, and whose omission in `replace()`
let a secret leak its own position. It is the same rule in a new position and
needs its OWN test; assuming the existing coverage extends to a new extraction
site is how it ships wrong a third time.

Provenance (§43) takes the container's source, the leaf having no independent
origin.

### 3.2 Where the path lives

The path goes INSIDE the variable reference. `value.VarRef` gains a path — a
slice of steps, each a key or an index — and there is no `OpIndex`.

A general index operator would make `${lower(x).y}` and `${merge(a,b).k}`
grammatical, which is the first step toward the expression language §10 exists
to refuse. Bounding the path to one syntactic position is the move §10.3 makes
for map literals — legal as an argument, nowhere else — and it keeps the
evaluator's `OpVarRef` case the only place that knows paths exist.

`parse.go:409` already splits on `.` and validates every segment, so this
changes what segments MEAN, not how they are scanned. §10.4's warning about two
scanners disagreeing does not apply: no new token is introduced.

### 3.3 Resource attributes get no paths

`${vpc.tags.Name}` stays an error, though the `var.` prefix now makes it
unambiguous.

The reason is asymmetry, not oversight. A variable's map is a known value at
compile time, so a path into it is checkable then — wrong key, wrong kind, both
caught before a plan exists. A resource attribute's map is `Kind: KindMap` in
`pkg/schema` and nothing more; there is no nested schema describing what is
inside. A path into one could not be checked at compile time, could not be
checked at plan time either (the value is unknown until apply), and would fail
DURING apply — the one place this product promises you will not end up.

Closing it properly means teaching `pkg/schema` nested shape, which crosses the
plugin wire and belongs with spec two's protocol bump if it happens at all.
Until then the diagnostic says that, rather than implying the form is
malformed.

---

## 4. Migration

**The rewrite needs no judgment.** Under the old grammar a bare single segment
could ONLY be a variable — that was the whole of `parse.go:478`. Every bare
`${x}` in infrena configuration becomes `${var.x}`, mechanically. There is no
file where the author might have meant a resource, because the grammar gave
them no way to.

That relocates the entire risk: it is in FILE SELECTION, not in the edits. A
global `sed` is wrong not because it would mangle expressions but because it
would reach files that are not infrena configuration.

Measured across the repository:

```
live code, fixtures, specs and docs:   243 occurrences / 60 files  (.go 188, .md 38, .yml 17)
historical plan and SDD records:       357 occurrences             ← must NOT be rewritten
```

### 4.1 Three things must not be touched

**Historical records** — `docs/superpowers/plans/` and `.superpowers/sdd/`.
These document what was true when written. Rewriting a September plan to use
syntax invented after it falsifies the record. The resulting inconsistency is
CORRECT, and this paragraph exists so a future reader does not tidy it away.

**GitHub Actions and shell** — `.github/workflows/release.yml`'s nine `${{ }}`
are not ours, are a third of the `.yml` count, and rewriting them breaks the
release pipeline.

**Prose placeholders** — `${...}` and `${}` in documentation are illustrative.

### 4.2 Two surfaces need no migration

Checked rather than assumed: `internal/cli/init.go` scaffolds `infra.yml` and
`variables.yml` containing NO interpolations, and `internal/generator` emits
none either, consistent with §27's rule that generated configuration carries
literal discovered values. The two places that write configuration FOR users
are untouched. Migration is confined to test fixtures, specs and docs.

### 4.3 Expand, migrate, contract

Landing the parser change and ~200 fixture edits as one commit gives one
unreviewable diff; landing them separately gives a red build in between. Three
commits, each green:

1. **Expand** — accept `${var.x}` alongside bare `${x}`. Additive; every
   existing test passes untouched.
2. **Migrate** — rewrite the 243, by file allowlist. `go test ./...` is the
   verification: a fixture rewritten wrongly fails a test, so the commit checks
   itself.
3. **Contract** — bare single segment becomes §2's error; `var` becomes
   reserved as a resource name. The new grammar's tests land here.

**The dual-acceptance window exists only inside the branch, never in a
release.** While both forms work, `${vpc}` is ambiguous again — precisely the
property being removed. Commits 2 and 3 are not separately releasable, and the
expand step is NOT a compatibility promise.

**Docs get a fourth commit and eye review.** Nothing fails when a Markdown
example is wrong; that is the one class the suite cannot catch, and separating
it is what makes the miss visible.

### 4.4 Spec of record

`PLAN.md` §10 is AMENDED, not annotated — it is the definition. The bare
variables inside §9, §12.1 and §57's initial example move with it. §10.4 gains
a line confirming no new token was introduced. §6.3's process variables take
the prefix.

`CLAUDE.md` gains the grammar in its current-state section, being what a new
session reads first. `README.md`'s examples move.

The vault note `projects/labs/infra-tool.md` takes the decision and its
reasoning, with the day's entry in `projects/labs/daily/`.

### 4.5 The floor key

§61.2's optional floor (`infrena: ">= 0.5"`) protects nobody today — the
project is unreleased and unused. It is the mechanism that makes the NEXT break
graceful, and `init` emitting it costs one line. Added in commit 4.

---

## 5. Diagnostics

Ten, all §44 shape (Summary, Detail, Action, Origin), each naming the fix
rather than the symptom:

| Trigger | Says |
|---|---|
| `${vpc}` | not a reference; variables are `${var.vpc}`, a resource reference needs an attribute |
| a resource named `var` | reserved; reported at the DECLARATION |
| `${var}` | `var` is a namespace, not a variable |
| `${var.tags.tema}` | no key `tema`; lists the keys that exist |
| `${var.region.x}` | `region` is a string; it has no members |
| `${var.azs[5]}` | `var.azs` has 3 entries; there is no index 5 |
| `${var.tags[0]}` | `var.tags` is a map; index it by key, as `${var.tags.team}` |
| `${var.azs.first}` | `var.azs` is a list; index it, as `${var.azs[0]}` |
| `${var.azs[i]}` | an index is a literal integer; a varying index needs iteration, which this language does not have |
| `${vpc.tags.Name}` | resource attributes have no nested schema to check a path against |

The last two matter most: both are forms a Terraform user types on their first
day, and both are DELIBERATE refusals. "Malformed reference" would send someone
hunting a syntax slip in something spelled correctly.

**One diagnostic is deleted, and that is the point.** `bind.go:385`'s
"reference to undeclared resource or module", the one that lies when a variable
of that name exists, becomes structurally impossible: `${vpc.arn}` can only be
a resource and `${var.vpc}` can only be a variable. Fixed by construction
rather than by a better message.

---

## 6. Testing

Per §46:

- `internal/expressions/characterisation_test.go` pins today's grammar. It is
  UPDATED, not deleted — it is the file that makes this change deliberate
  rather than incidental, and its diff is the clearest statement of what moved.
- §3.1's sensitivity rule gets its OWN test.
- Diagnostics are order-stable (invariant 6): identical input, identical
  output, every run.
- One integration test plans a project using map paths AND list indices end to
  end, proving the rule through the compiler rather than only at the parser.
- A map with a numeric-looking key (`${var.ports.0}` vs `${var.ports[0]}`) is
  pinned, since that pair is the whole reason the syntaxes differ.
- Commit 3 lands a test asserting a bare single segment errors — the proof the
  expand window closed.

---

## 7. Out of scope, recorded so each is a decision

- **Index EXPRESSIONS** (§3) — `${var.azs[i]}`. A varying index is only useful
  with iteration, which §54 refuses; the literal-only rule is what keeps
  indexing a read rather than a loop.
- **Negative indices** (§3) — `[-1]` makes meaning depend on a length the reader
  cannot see. Additive later.
- **Resource attribute paths** (§3.3) — needs nested schema across the plugin
  wire; belongs with spec two's protocol bump if at all.
- **A `resource.` prefix** — considered and rejected. Once `var.` is mandatory,
  a bare name is unambiguously a resource, so the prefix would be a second
  spelling for one identity with nothing to disambiguate. §14.1 already records
  what two spellings cost.
- **Rewriting historical plan records** (§4.1) — the inconsistency is correct.
- **A released dual-acceptance period** (§4.3) — reintroduces the ambiguity
  being removed, for users who do not exist.
