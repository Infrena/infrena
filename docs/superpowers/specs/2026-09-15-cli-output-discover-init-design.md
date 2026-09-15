# Progress and machine output, discovery that associates, and a project root

**Date:** 2026-09-15
**Status:** draft, awaiting review
**Amends:** `PLAN.md` §16 (exit codes), §25 (discover), §27 (generation), §37 (CLI surface), §4 (layout)
**Raises:** the plugin protocol to 4; `planner.PlanVersion`; `report.Version`
**Ships as:** three independent units — A (output), B (discover), C (init). Each is releasable alone.

---

## 1. Why

Four complaints from the first real run of infrena against AWS through
`infrena-provider-aws`, in James's words:

> Infrena can sit for a long time without output. We need to present progress.

> If outputting JSON we shouldn't output anything to STDOUT. It should fail without
> `--auto-approve` or a plan that it's running from.

> Discover finds things, but doesn't self associate, so if it finds a VPC and a subnet, the
> file it generates for the subnet just shows: `VpcID: vpc-1023902339`. It should instead
> send the variable `vpcID: ${vpc-resource}`.

> `infrena init` should create a full directory structure.

The first two are one subsystem — what the CLI writes and where — and are in direct tension
if designed apart. The third and fourth are independent.

**The AWS numbers are why silence is a defect rather than a rough edge.** From the live
suite on 2026-09-14: VPC create 15.7 s, security group 10.0 s, IAM role 24.7 s, and one
subnet delete that took 2 m 52 s. A plan against a fifty-resource account refreshes every
one of them before it prints anything. A user watching a blank terminal for three minutes
cannot tell a slow apply from a hung one, and the only recovery they have is Ctrl-C — which
this engine deliberately makes expensive, because the second SIGINT leaves the lock.

---

## 2. Unit A — output discipline

### 2.1 The rule

**stdout carries progress and the product. `--output` silences stdout completely and moves
everything into the file.**

| | `--output` absent | `--output <file>` set |
|---|---|---|
| stdout | progress, then the plan / summary / table | **nothing, ever** |
| stderr | diagnostics | diagnostics |
| the file | — | NDJSON: `meta`, `event`, `diagnostic`, `plan`, `result` |

One rule, every command, no new flag. The reason it is one rule rather than a per-command
judgement: a frontend that has to know which subcommands are chatty is a frontend that
breaks when a new subcommand is added.

Today this is violated in two places that between them cover every mutating command.
`internal/cli/plan.go:148` prints the entire rendered plan to stdout and *then* writes the
artifact. `internal/cli/apply.go` prints the plan render and `executor.Render`'s summary the
same way. Machine mode currently hands a caller both halves and expects it to sort them out.

**stderr is deliberately left alone.** Diagnostics already go there, piping already works,
and `--output` does not silence them: a run that fails must say so on a channel the operator
sees whether or not a frontend is reading the file. Progress does NOT go to stderr — that
was considered and rejected, because it splits one narrative across two channels for a human
who is watching neither in isolation.

### 2.2 Progress events

The seam exists. `executor.Options.OnEvent` already fires `EventStarted`, `EventSucceeded`,
`EventFailed`, `EventRetrying` and `EventSkipped` carrying `Address`, `Op` and `At`
(`internal/executor/types.go:156`), and `apply.go:214` already forwards them to the report
writer. Nothing renders them for a human. That is the whole of the gap.

Rendered form, on stdout, when `--output` is absent:

```
Creating aws.vpc.vpc-app1... done (15.7s)
Creating aws.role.role-api... still working (30s)
Creating aws.role.role-api... done (24.7s)
Deleting aws.subnet.subnet-app1a... still working (2m30s)
```

**Lines are APPEND-ONLY. No cursor control, no in-place rewriting, no spinner.** stdout here
is as often a CI log or a redirected file as it is a terminal, and escape sequences in either
of those are noise a reader has to mentally strip. It also means the renderer needs no TTY
detection, so there is one behaviour to test rather than two. A resource therefore emits one
line when it starts, a `still working` line per heartbeat, and one line when it finishes.

Duration is computed from the `At` of the paired `EventStarted` and terminal event. **No new
field on `Event`**: a duration field would be a second source of truth for something two
timestamps already answer, and the two could disagree.

**A heartbeat is required, not optional.** An operation with no terminal event within 15 s
re-renders as `still working (Ns)`, repeating every 15 s. Without it a single 2 m 52 s delete
is indistinguishable from a hang, which is the exact failure this unit exists to close.

**Concurrent writes.** `OnEvent`'s own doc comment states it is called from worker
goroutines. The renderer therefore owns a mutex and is the only writer to stdout during
execution. This is why the renderer is a type and not a closure appended to `apply.go`.

**Interleaving with the final summary.** Progress lines are written as work happens;
`executor.Render`'s summary is written once at the end. They share stdout, so the renderer
must be quiesced before the summary is printed. Ordering is therefore: progress lines,
blank line, summary.

### 2.3 `refresh` gets an event hook, because it is the slow phase

`refresh.Refresh` has no hook at all, and it is the phase that dominates wall-clock on a
real account: it runs once inside `plan` and **twice** inside `apply` (once unlocked for the
preview, once inside the lock). Adding progress to the executor and not to refresh would fix
the visible half of the wait and leave the longer half silent.

`refresh.Refresh` gains an `OnObservation` hook of the same nil-means-no-hook shape
`executor.Options.OnEvent` uses. It renders as `Reading aws.vpc.vpc-app1...` and emits an
`observation` line into the report, which `pkg/report` already defines.

`discover` gets the same treatment for the same reason — a `Discover` across a real account
is API calls, and Unit B adds filters that make it slower still.

### 2.4 `plan`'s artifact moves into the stream

`plan --output` is the one command whose file is not a report: it is a single JSON document
that `apply --plan` reads back. Under §2.1's rule that file must now also carry progress,
and a single document cannot.

**The artifact becomes the last line of the stream**, on a `plan` line beside the existing
`meta`, `event`, `diagnostic` and `result` lines. A frontend then tails one format for every
command, which is the entire point of the file.

**`apply --plan` accepts either shape**, and that is a compatibility requirement rather than
a courtesy: saved plans already exist on disk. The reader sniffs — a file whose first
non-space byte begins a bare JSON object *and which parses whole* is the old artifact; a file
of newline-delimited objects is the new stream, and the plan is its `plan` line. Refusing
the old shape would break `apply --plan` for every plan saved before this release, and §37's
saved-plan path is explicitly refused-not-degraded everywhere else, so a silent
misinterpretation here would be the worst possible failure.

`planner.PlanVersion` bumps, and `report.Version` bumps for the new line kind. §61 keeps
them independent; both move here because both formats genuinely changed.

**What does NOT change:** the artifact's contents, its 0600 mode, its cleartext sensitive
values, and every refusal in `apply --plan` (project, environment or state fingerprint
moved). This is a change of envelope, not of meaning.

### 2.5 Exit 77 when approval is required and unobtainable

**A run that cannot ask for approval must refuse before it mutates anything**, report why in
a form a machine can read, and exit with a code that says *this specific thing* went wrong.

Condition, checked after the plan is computed and before the lock is taken:

> the plan has changes, `--auto-approve` is absent, `--plan` is absent, **and** approval
> cannot be obtained — because `--output` is set (nobody is reading stdout) or stdin is
> already at EOF (nobody is there to type).

Result: a `result` line naming the fix, and **exit 77**, sysexits.h's `EX_NOPERM`.

`PLAN.md` §16's exit table gains a fourth row, so this is a documented product-API change:

| code | meaning |
|---|---|
| 0 | success, no changes |
| 1 | error |
| 2 | success, changes present |
| **77** | **changes require approval that this run cannot obtain** |

**This changes behaviour on a path that works today, and the change is the point.** A piped
`apply` currently reaches `confirm()`, gets EOF from `bufio.Scanner`, and exits 1 saying
*"apply cancelled: you must type \"yes\" to approve"* — advice nobody in that pipeline could
have taken. It becomes exit 77 and *"this run cannot ask for approval: pass --auto-approve,
or apply a saved plan with --plan"*, which is §44's requirement that a suggested action be
one the user can actually take.

`destroy`'s stronger confirmation (typing the environment name) and a teardown apply's are
covered by the same rule and the same exit code. The bar being higher does not make it
obtainable.

---

## 3. Unit B — discover that associates

### 3.1 The bug behind `vpc-129012092`

`internal/discovery/name.go:86` reads the tag map as a literal lowercase key:

```go
tags, ok := attrs["tags"]
```

The AWS plugin's canonical attribute is **`Tags`**, capital T (`catalog.Type.TagsAsMap`,
`internal/gen/build.go:71`). The lookup misses on every AWS resource, `nameFrom` returns
false, and `Name` falls through to the sanitised provider ID. **This is not a naming-policy
gap. It is a missed lookup, and it makes the documented §27.2 naming rule dead code against
the only real provider.**

The fix is not to add `"Tags"` to `nameTags`. A third hard-coded spelling is the same defect
with a longer list, and the next plugin spells it a fourth way. **The tag attribute is
resolved through the registry's definition**, folding case-insensitively across the canonical
name and its aliases — which is exactly what the compiler already does at its own boundary
(§14.1), so this asks the existing rule rather than restating it.

`discovery.Name` and `Unique` therefore take the `*registry.Registry`. `Walk` already holds
one. Inside the resolved map, the `Name` then `name` keys are consulted as today.

### 3.2 Names are type-prefixed

`<short>-<tag>`: `vpc-app1`, `subnet-app1a`, `role-api`.

**`<short>` is the last dotted segment of the resource type.** A plugin-published short name
was considered and rejected for now: `schema.ResourceDefinition` has no such field, adding
one is a protocol change, and the AWS generator already collapses unambiguous type names to
exactly the right thing.

| type | prefix |
|---|---|
| `aws.vpc` | `vpc` |
| `aws.subnet` | `subnet` |
| `aws.role` | `role` |
| `aws.s3.bucket` | `bucket` |
| `aws.securitygroup` | `securitygroup` |
| `aws.rds.dbinstance` | `dbinstance` |

An optional `ShortName` on the definition stays available later, with the last segment as its
fallback — the same partial-coverage shape `References` uses, where absence costs exactly
what it costs today.

**Do not prefix when the sanitised fallback already carries the prefix.** AWS provider IDs
are themselves prefixed, so an untagged VPC would otherwise be named `vpc-vpc-0a1b2c3d`. The
rule: if the sanitised ID already begins with `<short>-`, it is used unchanged.

Hyphens are safe in this position. `config.identifierSegment` admits `-` after the first
character, and the expression language has no operators at all — no arithmetic, by §10's
design — so `${vpc-app1}` parses as a reference to the resource named `vpc-app1`.
`internal/expressions/parse.go`'s `parseExpr` dispatches on `(` or falls through to
`parseReference`, which splits on `.` and never inspects `-`.

**Collisions keep their existing treatment.** `Unique` still suffixes the provider ID rather
than a counter, for the reason its doc comment already gives. The prefix makes cross-type
collisions impossible, so the suffix now only fires for two resources of one type sharing a
`Name` tag — which is genuinely two resources, and must not be silently merged.

### 3.3 Cross-references, read from a declaration and never inferred

For each generated attribute whose schema declares `References{Type, Attribute}`, the
literal is matched against the discovered set: the resource of that `Type` whose canonical
`Attribute` equals this value. On a match, emit `${<that resource's name>}` in place of the
literal.

```yaml
subnet-app1a:
  type: aws.subnet
  VpcId: ${vpc-app1}        # was: VpcId: vpc-1023902339
  CidrBlock: 10.0.1.0/24
```

Lists are included — the AWS plugin emits 97 list edges — so `SubnetIds: [${subnet-app1a},
${subnet-app1b}]`.

**The engine reads the plugin's declaration and never infers a relationship**, which is
§14.3's standing rule. The data is already there: `infrena-provider-aws` v0.2.0 ships 786
accepted and 136 approved edges.

**A reference is emitted only when its target is in the same generated set.** A `${vpc-app1}`
whose VPC was not imported is a compile error in a file the user never wrote — the precise
failure §27's generation rules exist to prevent. An unmatched target keeps its literal and
gains a comment at the point of omission saying what it points at and that the target was not
discovered. That also closes the standing follow-up that `import` can only adopt what
`Discover` returns: the limit becomes visible in the generated file instead of silent.

**Invariant 3 (the round trip) holds, and gains something.** `${vpc-app1}` resolves at compile
time to the same literal the attribute held, so the plan is still clean. What changes is that
a real dependency edge now exists where generated configuration previously had none — so
invariant 4 (dependency ordering) starts applying to imported infrastructure, which today it
cannot.

### 3.4 What is already managed is excluded

**`discover` and `import` report and generate only what is not already in state.**

Scope is *every* environment's state, not one. `discover` deliberately takes no environment
argument — §25 makes it a question about an account rather than a deployment — and a resource
managed in `production` is managed whichever environment you happen to be importing into.
Adopting it twice would put one real resource under two addresses, which invariant 1 then
schedules for destruction under whichever one loses.

Two additions this needs:

- **`state.Local.List()`**, enumerating the environments that have state. No such method
  exists; §6.1's "reachable if declared OR stateful" rule is currently answered per-named-
  environment. Phase 4's remote state will need it regardless.
- **`selectForImport` becomes state-aware.** It currently calls `discovery.Walk` and narrows
  by selector only (`internal/cli/import.go`).

`--all` shows everything, managed included, and **only then** gains a `STATUS` column
(`managed (production)` / `unmanaged`). The default view has no such column, because under it
every row would read `unmanaged` — a column with one value is noise. The default is the
filtered view because the default is the useful one: a real account returns hundreds of rows,
and the question being asked is always "what have I not adopted yet".

The footer states the exclusion whether or not anything was excluded — `12 unmanaged, 50
already managed (--all to show them)`. A count a user cannot see is a count they will assume
is zero.

### 3.5 System-owned resources are flagged — protocol 4

Importing the default VPC and later destroying it is the worst foot-gun in this feature, and
it has already bitten in practice.

**The plugin declares it. The engine must not.** "The core engine must not know about AWS" is
this project's first architectural rule, and spotting `GroupName: default`, `aws:cloudformation:*`
tags or service-linked role paths is AWS knowledge by any reading. So:

```go
type DiscoveredResource struct {
	Type       string
	ProviderID string
	Attributes map[string]value.Value

	// SystemOwned marks a resource the CLOUD created and manages, which a user
	// did not ask for and generally must not adopt: AWS's default VPC and
	// default security group, service-linked roles. Nothing refuses to import
	// one — it is occasionally right — but it is never adopted by default and
	// never silently.
	SystemOwned bool
	// SystemOwnedReason says why, in the plugin's own words, for the line a
	// user reads before deciding.
	SystemOwnedReason string
}
```

`pluginproto.Version` goes to 4, `Supported` becomes `{4, 3, 2, 1}`. A protocol-3 plugin
reports nothing and behaves exactly as today — absence costs what it costs now.

`discover` marks such rows. `import` **excludes them unless named explicitly** as a selector,
and says how many it skipped and why. Explicitly naming one is the escape hatch, because
adopting a default VPC is occasionally the right thing and the engine should not be the one
forbidding it.

This is the only part of Unit B that needs a coordinated `infrena-provider-aws` release. The
rest ships against the plugin as it stands today.

### 3.6 Filters

`discover` and `import` share them:

- `--tag <key>=<value>` — repeatable, AND-ed. Resolved through the same registry fold §3.1
  uses, so `--tag Name=app1` works whatever the plugin spells the map.
- `--exclude-type <type>` — repeatable. Complements the existing positional type selectors.
- `--name <glob>` — matches the name the resource *would be given*, which is the column a
  user is reading. `path.Match` syntax (`*`, `?`, `[...]`), because it is in the standard
  library and this project's third-party budget is two packages.

Filtering happens after naming, because naming is what `Unique` makes order-dependent: a
filter applied first would change the names of the resources that survive it.

---

## 4. Unit C — `init`, and finding the project root

### 4.1 A project root is discovered

**Every command looks for `./infra.yml`, then `./infrena/infra.yml`, and runs from whichever
it finds.** An explicit `--chdir` wins outright and skips the search — it means what it says
today.

Closest wins: a directory holding both is a project at its root that also has a directory
called `infrena`, and the root is the answer. Finding neither is an error naming both places
it looked, rather than the current "no project here".

The search does **not** walk up the tree. Walking up means a command run in a subdirectory
silently operates on a project the user may not have realised they were in, and state
mutation is the wrong place for that kind of convenience.

### 4.2 `infrena init [dir]`

Defaults to **`./infrena`**, so infrastructure lives beside the application in the same repo
— the layout Terraform users already expect. An explicit directory argument overrides it;
`infrena init .` scaffolds in place.

```
infrena/
  infra.yml            project, infrena floor, providers, environments
  resources/
    network.yml        one commented example
  vars/
    default.yml        applies to every environment
    production.yml
    staging.yml
  modules/.gitkeep
  .gitignore           .infra/
```

**Environments are `production` and `staging`**, matching the vars filenames. Today's
scaffold declares `dev` and `production` while §4's layout names `vars/<environment>.yml`, so
a scaffolded `staging.yml` would apply to nothing — dead configuration in the one file a user
reads to learn the language.

**`--provider aws`** scaffolds a real `providers:` block and an `aws.vpc` example. With no
flag, the example is a **commented-out** `fake.network`. That is a change from today, and the
reason is that a shipped infrena carries no provider: the current scaffold declares a live
`fake.network` resource, so a fresh `init` only validates if the fake plugin happens to be
installed. Commented out, it validates bare and still teaches the shape.

**Refusing to overwrite is unchanged**, and is checked across every file before anything is
written, so a refusal leaves the directory as it was.

The closing message names the next command with the directory in it, since the user is now
one level up from the project:

```
Next: infrena validate, then infrena plan staging
```

That form is correct **only** when the scaffolded directory is one §4.1 discovers — `./infrena`
(the default) or `.`. For any other directory the message must carry the flag, or it is advice
that does not work from where the user is standing:

```
Next: infrena validate --chdir myinfra, then infrena plan staging --chdir myinfra
```

---

## 5. Testing

- **A:** golden tests on the renderer with a fake clock (`executor.Options.Now` already
  injects one), including the heartbeat path. A `--output` test asserting stdout is
  byte-empty for every command — that is the property, so it is asserted directly rather
  than inferred. An `apply --plan` test reading both envelope shapes. An exit-77 test for
  `--output` set and for stdin at EOF, asserting nothing was mutated.
- **B:** a discovery test with a `Tags` (capital) map, which is the regression that would
  have caught §3.1. Name-prefix tests including the `vpc-vpc-` collapse. A reference test
  where the target is outside the set, asserting the literal survives with its comment. A
  round-trip test (invariant 3) through the new generator. Managed-exclusion across two
  environments.
- **C:** root discovery for each of the three cases and the both-present tie. A scaffold test
  asserting the output passes `validate` with no plugin installed.
- **Integration:** `tests/integration` runs the real fake plugin. The protocol-4 field needs
  a fake-plugin release to be exercised end to end; until then its host-side handling is
  covered by `pkg/plugintest`.

---

## 6. What this does not do

- **No project-root walk up the tree** (§4.1).
- **No plugin-published short type name.** Deferred behind the last-segment fallback (§3.2).
- **No `Requirement`/`References` reconciliation.** Still deliberately unreconciled.
- **No `discover --generate`.** Generation stays in `import --generate`; discover writes
  nothing, which is the promise §25 makes in its own help text.
- **No progress for `validate`, `graph`, `explain`, `state` or `export`.** None of them wait
  on a provider.
