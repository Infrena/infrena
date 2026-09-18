# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> Project notes (source of truth): `Obsidian Vault/projects/labs/infra-tool.md`

## Current state

**PHASES 1 AND 2 ARE COMPLETE, and infrena is RELEASED: v0.1.0 and v0.2.0 are tagged and
published.** M1-M11 are merged to `main` (tags `m1`-`m11`, which stay LOCAL; the `v*` release
tags are pushed), and the provider-plugin cutover has landed on top of them.

**v0.2.0 is a MINOR because it refuses configuration v0.1.0 accepted** — requirement
satisfaction is now per provider instance. §61.1 was amended when that came up: it defined
bumps purely in terms of the format-version table, none of which moved, so it would have
called a release that breaks projects a patch. The test is what an existing project does. Every one of `PLAN.md` §49's nineteen components exists. Phase 3 (AWS)
is the next milestone and has not started.

The MVP workflow §48 describes runs end to end: `init` → `validate` → `plan` → `apply` →
re-plan clean → externally mutate → `refresh` → drift shown → remove from YAML → destroy
proposed → `apply`, plus `graph`, `explain`, and §48's last three — `discover`, `import`,
`export` (M8). It runs against the fake provider, which since the cutover is a separate
BINARY from a separate repository; see "A SHIPPED INFRENA CARRIES NO PROVIDER" below before
assuming an in-tree provider exists.

Present: the value model with per-leaf provenance and sensitivity, addressing, diagnostics,
declarative resource schemas, the provider interface, a hand-editable file-backed fake provider,
versioned state with atomic writes and `O_EXCL` locking, all eight compiler stages, a generic
dependency graph, provider refresh, the planner, the plan renderer, the executor, lifecycle
protection, and the commands `init`, `validate`, `plan`, `apply`, `destroy`, `refresh`, `state`,
`graph` and `explain`.

**New in M5 — modules.** Loading and instantiating are SEPARATE: `modules:` is a list of sources
carrying no inputs, and a resource of type `module.<name>` instantiates one. Modules nest, take
typed inputs with defaults, publish outputs, and may be fetched from a pinned git remote.
Compiler stage 5 expands them away entirely, so the planner, graph, state and executor never
learn modules exist. Addresses embed the module path (`module.prod.db`), which means moving a
resource between modules is a destroy plus a create — there is no `state mv`.

**New in M6 — the Phase-1 CLI surface.** `init` scaffolds a project that validates immediately
and refuses to overwrite — the §4 layout (`infra.yml`, `resources/`, `vars/`, `modules/`) into
`./infrena` by default, `init [dir]` for anywhere else, environments `production` and `staging`
matching the `vars/` filenames, and the example resource COMMENTED OUT unless `--provider` names
one, because a shipped infrena carries no provider and the scaffold has to validate on a machine
with nothing installed. Every command then finds that project with no `--chdir`: `./infra.yml`
or `./infrena/infra.yml`, closest first, and never upwards (§4.2), so a command run in a
subdirectory cannot quietly mutate a project the user did not know they were in. `graph` renders
the dependency tree from the same compile `plan` does;
`explain` renders a resource type from the registry, so documentation cannot drift from the
schemas it describes.

**New in M7 — conventional directories.** Four directory names are read automatically at any
depth, and nothing lists them: `resources/**` (resources), `vars/**` (variable values),
`modules/**` (modules), `discovered/**` (generated, and LOADED, which is what makes import
safe). A resources directory may carry `vars/` of its own, visible only to the resources
declared there — a new precedence rung above base configuration and below an environment. A
`vars/` file may be named for an environment (`production.yml`), with `default.yml` applying to
all of them and an environment file overriding it VALUE BY VALUE, not file by file.
`templates/` is reserved and deliberately unread, at both the project level and inside a
resources directory.

Two rules make the layout organisation rather than semantics, and both are load-bearing:
**a directory scopes variables, never names** (`ResourceDecl.Dir` is not part of a resource's
identity, so moving a file between directories renames nothing), and **a name declared twice is
an error naming both files** — never last-one-wins, because globbing makes accidental
duplication easy in a way a single file does not. `tests/integration/m7_layout_test.go` pins
that the directory form and the single-file form produce byte-identical plans.

**New in M8 — discovery and import.** `discover` lists what exists without writing anything,
showing the name each resource WOULD be given so a collision is visible before it happens.
`import <env> [--generate]` adopts resources into state and writes the configuration declaring
them, under `discovered/`. `export <env>` dumps state in full for auditing.

Four rules carry most of the weight, and each is a hazard rather than a nicety:

- **A generated file must never hold a secret.** A sensitive attribute is OMITTED and the file
  says so at the point of omission. A secret committed to git is a secret rotated, not deleted.
- **Configuration is written BEFORE state.** A resource in state that no configuration declares
  is what invariant 1 schedules for destruction, so failing between the two writes must leave
  the harmless half done: configuration-without-state plans a CREATE, which is visible and
  refusable; state-without-configuration plans a DESTROY.
- **Computed attributes are omitted because they cannot be set**, not for tidiness. Emitting one
  produces "is computed and cannot be set" — a file that does not load, pointing at a file the
  user never wrote.
- **A reference is emitted only when its target is in the same generated set**, and the
  dependency edge it implies is recorded into state by the SAME pass that emitted it
  (§27.3). A `${vpc-app1}` pointing outside the set would be a compile error in a file the
  user never wrote; and a reference with no edge in state is a `depends_on` change proposed
  on the first plan after an import, which is invariant 3 failing. `generator.Generate`
  returns the edges with the files so a caller cannot write one without the other.

Invariant 3 (the round trip) now has its test, the last of the six to get one. Its guarantee is
qualified — see §29.1: a deliberately omitted secret shows as one pending change, and the two
alternatives (writing it to disk, or dropping it from state) are both worse. Minimality is
asserted SEPARATELY from the round trip, because a generator emitting every attribute would
also plan clean. So are references, and for the same reason in reverse: a generator emitting
NONE would plan clean too.

**Discovery narrows what it shows (Unit B).** Names are prefixed with their type
(`network-vpc-0a1b`), and the tag they come from is found through the registry's alias fold
rather than a hard-coded `attrs["tags"]` — that literal key missed on every AWS resource,
which is why every discovered name used to be a cloud identifier. Resources managed in ANY
environment are hidden behind `--all`; resources a plugin flags as cloud-owned are shown with
the plugin's reason and never adopted without being named; `--tag`, `--exclude-type` and
`--name` narrow both `discover` and `import`, and they run AFTER naming because the
uniqueness pass is order-dependent.

**New in M9 — environments do what §6 says.** `skip` and `only` on any resource name the
environments it belongs to, as a scalar, a list, or an expression — which is what lets a module
be written with parts a caller switches off. An environment is reachable if it is DECLARED or it
HAS STATE, so removing one from configuration proposes tearing it down and lets you see the
teardown first. `${var.project}` is a fourth process variable.

Three rules to know before touching any of it:

- **A skipped resource is exactly as if never declared, EXCEPT that something still knows it was
  skipped.** It is marked in stage 5, reported by stage 6, and dropped at ONE place in
  `bindReferences` — after reference binding, before anything downstream. Drop it earlier and a
  reference to it reports "no such resource", sending a reader after a typo that is not there.
  The marking also means its own attributes are still bound, so a mistake inside a
  production-only resource surfaces when you plan dev.
- **The teardown configuration is SUPPLIED, never compiled.** `resources:` is global, so
  compiling an undeclared environment yields every resource and plans a full CREATE against one
  that already holds them. `internal/cli/environment.go` owns the rule.
- **Environment-class defaults are WITHDRAWN** (§13). A provider default is one value per
  attribute; anything that differs between environments is a variable. `type:` in an environment
  is an error, because leaving it alone was never inert — it silently declared a variable named
  `type`.

**New in M11 — provider instances.** `providers:` is a LIST of instances, each naming a
`plugin:` and optionally a `name:` (defaulting to the plugin name), so one project can reach
two accounts of one cloud. A resource picks one with `provider:`; a module call's `provider:`
is inherited by everything it expands into; the first entry is the default unless one is marked
`default: true`. An instance's configuration may interpolate variables, so it differs per
environment.

Three things to know:

- **A plugin is not a provider.** `provider.Plugin` holds the SCHEMAS and a factory;
  `provider.Provider` is one configured instance. The split breaks a real cycle — constructing a
  provider needs configuration, which needs variables, which needs a compile, which needs the
  schemas. `RegisterPlugin` runs before anything is read; compiler stage 4.5
  (`internal/providers.Prepare`) constructs each instance from resolved values.
- **State records the INSTANCE, and the engine stamps it.** A plugin cannot know which instance
  of itself it is, so `executor.stampInstance` writes it after every dispatch. Without it two
  accounts are indistinguishable and removing a resource proposes nothing at all.
- **`destroy`, `refresh`, `discover` and `import` never compile**, so they read `providers:` for
  LITERAL values only and refuse an instance whose configuration interpolates anything. Same for
  `plan`/`apply` on an orphaned environment. The instance name in state is what still gets each
  resource to the right account.

An instance may also carry `defaults:` — attribute defaults for every resource it serves, plus
the lifecycle options, sitting between what the resource writes and what the plugin ships. A
key no resource type of that plugin declares is an ERROR: it would otherwise apply to nothing,
in every environment, forever, with no output in which its absence is visible.

**Absent until Phase 3+:** AWS. Nothing half-implements it.

**NEW 2026-09-16 — the state backend boundary** (`PLAN.md` §52, step 1 of four;
`docs/state-backends.md`). Remote state is no longer absent, but nothing remote ships yet
and that is the point of the split:

- **A backend is a plugin, and local is the BOOTSTRAP.** Local writes to `.infra/` and
  works before anything is installed, which is exactly why it cannot itself be a plugin —
  something has to hold state for a project that has installed none. **A shipped infrena
  carries no REMOTE backend**, the same way it carries no provider. A project with no
  `backend:` block gets local, starts no process and touches no network; `backendFor` is
  where that routing lives, and every one of its call sites defers the closer it returns.
- **`backend:` never interpolates, and the reason is an ordering CYCLE.** State is read
  before anything is compiled, and compiling is what resolves variables — `destroy`,
  `refresh`, `discover` and `import` never compile at all. So a `${...}` there could never
  be filled in at any point in any run. It is not a feature nobody got round to; do not
  add it. `plugin:` is the only key infrena reads and an unrecognised key is NOT an error,
  alone among the blocks in this language, because the engine cannot know which keys an s3
  backend accepts.
- **A backend stores BYTES and does not interpret them.** `state.State` stays in
  `internal/state` and out of `pkg/backend`; one that parsed state would be a second
  reader free to disagree with the first. `Backend.Put` is called once per operation and
  writes the whole state, carried as raw bytes rather than JSON inside JSON — a second
  full encode on every write buys nothing.
- **Every backend must lock**, refused when it LOADS rather than at apply time, so
  invariant 5 holds for a backend the engine cannot audit.
- `pkg/backend` is the contract an author imports, `pkg/backendproto` the wire (its own
  format version, §61's seventh — a storage change must not force a provider release),
  `pkg/backendsdk` the whole of their `main()`, `internal/backendhost` the host.
- **State reaches a backend in CLEARTEXT**, exactly as values already reach a provider. It
  is the same trust level, not a new exposure; the mitigation is choosing which plugins to
  trust, and `plugins.lock` records the choice. Client-side encryption is deferred because
  it needs a key story before it needs code. `docs/state-backends.md` says all of this to
  users.

**NEW 2026-09-17 — `infrena state migrate`** (`PLAN.md` §52 step 3, §37.1;
`internal/cli/state_migrate.go`, `docs/state-backends.md`). A temporary `migrate_from:`
block sits beside `backend:`, taking the same shape and decoded by the same code. Five
things about it are easy to undo by accident:

- **MIGRATION COPIES. It never empties the source.** James's reason: a bug in the migration
  has to be survivable, so the old backend stays a fallback you can point `backend:` back
  at. The consequence has to be said out loud wherever this is documented — **the old state
  lingers, every secret in it included, until the user deletes it.** Nothing tidies it up,
  and nobody should assume it did; the command says so on the run that copies.
- **`migrate_from:` is INERT for every command except `state migrate`.** A migration through
  CI is necessarily two commits — one adding the block, one removing it — and between them
  it sits in committed configuration. If `plan` or `apply` acted on it, a SUCCESSFUL
  migration would break the pipeline until somebody pushed the removal.
- **The one exception is a GUARD, and it is what stops the design destroying somebody's
  infrastructure.** While the destination is empty and the source holds state, ordinary
  commands REFUSE:

  | `migrate_from:` | Destination | Source | Ordinary commands |
  | --- | --- | --- | --- |
  | absent | — | — | proceed — every project |
  | present | holds state | not opened | proceed: migrated, block inert |
  | present | empty | holds state | **REFUSE**, naming the fix |
  | present | empty | empty | proceed: a new project with both blocks is not a pending migration |

  Without it, the window between the configuration landing and the migration being run is a
  window in which a command reading only `backend:` sees every resource as unmanaged and an
  apply RECREATES ALL OF IT. In the CI flow this exists for, that ordering is the likely
  one. **A command never performs the migration** — moving state as a side effect of a
  `plan` would be a worse surprise than the one being prevented; the only outcomes are
  "carry on" and "stop, and here is what to run". **The destination is checked first**,
  which it has been opened for anyway: state there means the migration is done and the
  source is never opened, and that matters because opening it may start a plugin process
  and reach a network.
- **Three cases, not two**, compared under both locks before the first write: destination
  empty → copy and read back to verify; destination identical → **no-op and SUCCEED**;
  destination different → refuse. The middle case is the one the design turns on. A
  migration run through CI gets re-run, and failing there invites somebody to put `--force`
  in a workflow file where it then sits forever — the same hazard as a `lock: false` escape
  hatch arriving by the same route, which is why the refusal mentions the flag last rather
  than first. `--check` and the migration share ONE comparison function; two would be how a
  check comes to report one thing and the migration then does another.
- **`comparableBytes` zeroes `Serial` and `UpdatedAt` before comparing**, and only those.
  Every `Put` stamps both, so without it every COMPLETED migration would read as a conflict
  and the no-op case could never fire. Nothing else is zeroed, so a genuine difference in
  what is managed still refuses.

`--check` writes nothing and takes no lock, and **its exit code is the report**: 0 none
needed, 2 pending (deliberately `plan`'s code — both mean "something is pending, run the
matching command"), 3 already complete, 4 the ends differ, 1 error. 3 and 4 are new rows on
§37.1's table and therefore a documented product-API change. `report.Version` was 3 at that
point, because the `result` line carries a `status` WORD a version-2 consumer has never seen.

**The retry loop lives in `internal/retry`, not `internal/executor`.** It moved on 2026-09-18
because `refresh` and `discovery` both needed it and neither can import `executor` —
`executor` → `planner` → `refresh` is a real import cycle, verified, not guessed.
`executor.RetryPolicy` stays as an exact alias of `retry.Policy` (it is the type of
`Options.Retry`), and `retry.Attempt`/`retry.Verb*` are used directly everywhere else.

**An empty collection the provider reports is not a change, and generation keeps it.**
Two halves of one defect, both found on 2026-09-18 from a plan that proposed replacing four
untouched EC2 instances: `internal/generator`'s `plainValue` dropped an empty list or map
(that branch exists for a collection emptied by SECRETS, and now fires only then), and
`planner.diffAttributes` counted the resulting absence as a change. AWS reports
`Ipv6Addresses: []` inside NetworkInterfaces, which is force-new, so a single synthesised
empty list was a replacement of running infrastructure.

The diff rule is `equalBesidesProviderEmpties`: `value.Equal` plus exactly one forgiveness —
a key the provider reports, configuration does not mention, and whose value is an EMPTY
collection. A shortened list, a differing value, and a key configuration sets that the
provider does not report all remain changes, each pinned by its own test. Top-level
attributes already had this rule via `attr.Computed`; inside a composite there is no
per-leaf schema to ask, so emptiness is the evidence.

**`ensureStateProviders` reads "the project declares no `providers:`" FROM CONFIGURATION**,
never from `len(reg.InstanceNames()) == 0`. Those are the same question only once the
declared instances are registered, and on the plan and apply paths they are not — stage 4.5
builds them in the compile that runs after. The old proxy registered an implicit,
config-less instance first, `providers.Register` then skipped the declared one as a name
already taken, and **every provider call in the run went to the account the plugin defaults
to.** Real consequence, 2026-09-18: `profile:` silently dropped, a different AWS account
read, 63 live resources reported deleted, a plan to create them again. `validate`, `refresh`
and `destroy` were unaffected — they never take that path — and the disagreement between
`validate` and `plan` about the SAME file is what identified it.

**The trigger was state**, which is why it survived: with nothing in state no plugin is
loaded before the compile, so `Implicit` derives nothing. Any test for this must apply
first. `providers.Register` now refuses, loudly, to drop an entry that carries
configuration when the name is taken — a config-less entry (implicit instance, or a test
stub) still keeps what is there.

**An exhausted retry SAYS SO**: `retry.Attempt` wraps the last failure as `after N attempts
over D: ...` once it has actually retried, and returns a never-retried failure untouched.
Without it an exhausted retry and a single failure printed identically, and neither the user
nor the engineer could tell whether anything had waited — which cost a round trip on a
throttled AWS read on 2026-09-18.

**Reads retry; `cli.readRetryPolicy` is their policy and is deliberately not
`defaultRetryPolicy`.** Five attempts / 500ms base / 30s cap for `refresh`'s provider Read
and `discovery`'s Discover, against three attempts for the executor's mutations. The
asymmetry is the point: §15 spends few attempts on a Create because another attempt may make
a second resource, and a read carries no such risk — its only cost is time, weighed against
failing a whole plan over a throttle that would have cleared. Both `Refresh` and `Walk` take
a `retry.Policy`; the ZERO VALUE MEANS ONE ATTEMPT, so a caller that passes none gets the
old behaviour rather than a surprise. This closed the gap where the provider classified a
throttle `SafeToRetry` and the widest fan-out in the product never asked.

**`report.Version` is now 4**, for the `applying` line `apply` and `destroy` write before
they execute (`report.PlanChanges`, `cli.reportPlan`). Two per run: `stage: proposed`
beside the human `planner.Render` call, `stage: executing` immediately before
`executor.Apply` — they are not the same plan, since both commands re-plan inside the
environment lock, and `executing` is the claim that the executor received one. It is
REDACTED, unlike the `plan` line, and typed `applying` rather than `plan` for a
load-bearing reason: `readSavedPlan` applies what it finds on a line typed `plan`, so a
redacted plan wearing that name would write `<sensitive>` into infrastructure. That path
now also refuses a `plan` line whose meta line names another command. **This format is
deliberately not documented publicly beyond what `docs/ci.md` already says** — a UI is not
being blocked, but it is not being helped either.

**The round trip test is honest about what it can prove.** A literally byte-identical
local → S3 → local round trip is IMPOSSIBLE: `internal/state/local.go` increments `Serial`
and restamps `UpdatedAt` on every `Put`. `tests/integration/state_migrate_roundtrip_test.go`
asserts the encoded state matches with those two cleared AND that they are the only
top-level keys that changed — which is stronger than a normalised comparison alone, because
normalisation can hide a change in a field it also touches.

**Reading a saved plan back SHIPPED** (`apply --plan <file>`, 2026-09-13), which closed §37's
last unimplemented surface. It compiles nothing, refreshes nothing and re-plans nothing, and is
refused outright — no override — when the project, environment or state it was made against has
moved. Two rules in it are easy to undo by accident: the state fingerprint is of state AS
LOADED, so a command must hash before mutating anything (apply stamps the project name in, plan
does not), and an unknown value carries its expression through the artifact, which is what makes
a plan containing `${net.id}` apply correctly rather than silently leaving the attribute unset.

**BUILT, not planned: provider plugins as separate processes** (`PLAN.md` §31.1, §31.2, §50.1).
Plugins are separately distributed Go binaries speaking newline-delimited JSON over stdio, with
the standard library only. `pkg/pluginproto` is the wire contract, `pkg/pluginsdk` is the whole
of a plugin's `main()`, `pkg/plugintest` is the harness an out-of-module plugin tests through,
`pkg/pluginmanifest` parses the `plugin.yaml` a plugin repository publishes, and
`internal/pluginhost` is the host — including the adapter enforcing the seven things the engine
refuses to trust a plugin with. AWS will be a plugin from its first line, not a port.

Two rules the design leaves behind, and both still bind:

- **Nothing in `pkg/schema` may gain a function-typed field.** `DefaultFunc`, `Validate` and
  `ImportSpec.Parse` are GONE: a function cannot cross a pipe. `Default` is a datum.
- **Do not add a guarantee that rests on a provider obeying a comment.** Read's carry-forward,
  non-nil-on-success and the sensitivity flags are enforced in the host adapter, because a
  third-party binary cannot be held to a doc comment. A plugin that re-implements them is a
  plugin whose tests pass when the host is broken.

**The reference grammar no longer tells a variable from a resource attribute by counting
segments** (2026-09-14, `PLAN.md` §10.5). Six forms, each with exactly one meaning:
`${var.region}` (a variable), `${var.tags.team}` (a path into a map variable),
`${var.azs[0]}` (a list entry), `${vpc.id}` (a resource attribute), `${vpc.tags.Name}`
(a path into one), and `${vpc}` — pass the resource itself, and the engine projects to
whichever attribute the PLUGIN declared that consuming attribute refers to (`PLAN.md`
§14.3). `var` is reserved as a resource name, checked at the declaration, and the process
variables `${var.environment}`/`${var.project}` take the prefix like any other.

**`${vpc}` projects; it does not guess.** The plugin's schema says that, say, `vpc_id`
refers to `aws.ec2.vpc`'s `id`, and the engine reads that declaration rather than ever
inferring one — that knowledge belongs to whoever owns the API being called, not to infrena.
`${vpc}` is sugar over `${vpc.id}`; both spellings are legal forever. An attribute with no
declared reference makes `${vpc}` a compile error naming the fix, never a fallback to "it's
probably the id" — so a relationship the plugin hasn't declared costs nothing beyond what it
costs today. The type check that catches `vpc_id: ${database}` (wrong resource type) fires
identically whether the attribute is named explicitly or left for the engine to project.

- A path indexes a map with dotted keys or a list with `[n]` — an integer literal only, no
  arithmetic, no negative indices — and the two compose in either order. A missing key or
  an out-of-range index is a compile-time error naming what is actually there, never an
  unknown deferred to apply.
- Extraction unions sensitivity across every container on the path, key or index alike, so
  a leaf pulled out of a sensitive map or list is never returned declassified (§10.5,
  §36).
- A path into a resource attribute resolves exactly like the whole attribute — deferred
  until apply — with one known gap: only the outer attribute name is schema-checked at
  compile time; a key past it fails before dispatch instead, closed properly by a later
  change to the plugin wire.

## Name

The product is **Infrena** (GitHub org `infrena`, command `infrena`), **renamed from Infrata
on 2026-09-14** after a legal collision on that name. The module is
`github.com/infrena/infrena` and the binary is `cmd/infrena`.

**Releases v0.1.0 to v0.3.0 were published as `infrata`** and declare the old module path
permanently — a tag's `go.mod` says what it says, whatever GitHub redirects. v0.4.0 is the
first release under the new name. `.infra/` kept its name, so existing state files work;
resource type prefixes are unaffected, being the plugin's name rather than the product's. The repository is
`github.com/infrena/infrena` and is **private until the product is feature complete** (§31.1),
which is why a plugin in another repository reaches it through a `replace` directive pointing at
a sibling checkout named `infrena` — the directory a `git clone` produces. Local working copies
must use that name, or the plugin repository cannot build.

## What is being built

A CLI that reconciles YAML-described desired infrastructure with real infrastructure,
in the shape of "Ansible's readability + Terraform's state and planning, with better
environments, imports, and defaults."

The governing philosophy:

> Configuration describes the desired infrastructure. State records what is managed.
> The planner determines the difference. The executor reconciles reality with the
> desired state.

## Stack and commands

Go 1.27 (pinned via `mise.toml`, and declared as the module floor in `go.mod`), Cobra, `gopkg.in/yaml.v3`. Those two are the **entire**
third-party budget so far, and it stays that way: the AWS SDK arrives with Phase 3 inside
`infrena-provider-aws`, a SEPARATE REPOSITORY, so it never enters this module at all (§31.1).

**`mise` is not active in non-interactive shells.** Either use the `make` targets, which set
the shim path structurally, or `export PATH="$HOME/.local/share/mise/shims:$PATH"` first.
A bare `go` resolves to 1.20 and fails. Check with `go version` if anything looks odd.

```bash
go build ./cmd/infrena        # build the binary
go test ./...                 # full unit + fake-provider integration suite
go test ./internal/planner/   # one package
go test -run TestPlanDestroy ./internal/planner/   # one test
go vet ./...
gofmt -l .
```

AWS integration tests must be opt-in (build tag or env guard) — **normal CI must not
require AWS credentials** (§46).

**A SHIPPED INFRENA CARRIES NO PROVIDER** (since 2026-09-13). `init` therefore scaffolds its
example resource commented out unless `--provider` names one, and a project installs
`infrena-plugin-fake` like any other plugin. The suites divide:
`tests/integration` builds and runs the REAL plugin from the sibling repository, so the
path a user takes is proved somewhere; every in-process suite injects the fake double via
`internal/cli`'s TestMain, which is what §31.1's Testing section always specified.
`providers/test` survives as the engine's TEST DOUBLE, not a shipped provider.
`TestAShippedBuildCarriesNoProvider` runs the binary with no plugin installed, and is the
only test that can make that claim — every other suite injects the double.

**CI** (`.github/workflows/ci.yml`) runs gofmt, vet, the suite and `-race`. It checks out
`infrena-provider-fake` too (needs a `PLUGIN_REPO_TOKEN` secret, since that repo is
private) and sets `INFRENA_REQUIRE_PLUGIN`, which turns the integration suite's skip into a
failure — a run that silently skips its integration suite reports green for tests that
never ran.
`GOTOOLCHAIN=local` is set for the whole job, because Go otherwise downloads a newer
toolchain to satisfy `go.mod` and a floor break would pass by fetching the very version
it should have failed on. `go.mod`'s floor currently MATCHES `mise.toml`, so the matrix
has one entry; the moment the floor drops below the pin, add the older version as a
second entry — two toolchains is the only way the older promise is ever tested. The floor
is a promise to plugin authors, and a plugin repo must declare at least it. It also runs on
`merge_group`, and is called by the release workflow so a release cannot skip it.

**The repository stays PRIVATE until the product is feature complete** (§31.1, ruled
2026-09-13). So `github.com/infrena/infrena` is not fetchable, there are no third-party
plugin authors yet, and the one plugin repository depends on this one through
`replace => ../infrena` — meaning it builds against a working tree rather than a version.
**Do not raise making it public as a blocker.** A semver tag is still worth cutting: it is
independent of visibility, lets a plugin pin a version with `GOPRIVATE` set, and makes
`infrena version` report something real.

**Milestone tags stay LOCAL.** `m1`–`m11` mark development milestones, not releases, and
are deliberately never pushed — ruled 2026-09-13. A public tag list is where a consumer
looks for releases, and `m11` sitting beside `v0.1.0` blurs which is which. Nothing is at
risk: every tagged commit is an ancestor of `origin/main`, so the tags are labels on work
that is already pushed. **Do not offer to push them.**

**Releases** (`.github/workflows/release.yml`) fire on a `v*` tag and cross-compile eight
platforms from one runner — this is pure Go with no cgo, so a matrix of operating systems
would buy nothing. `CGO_ENABLED=0` for static binaries; `-trimpath`; deliberately NOT
`-s -w`, because the engine lets an unrecovered panic crash the process rather than
recover mid-apply, which makes the symbol table the whole diagnostic.

**The version is stamped only there** (§61.1), so a release build is the only one that
reports a real version. A step asserts the built binary says the tag — a wrong `-ldflags`
path would otherwise ship binaries silently reporting `0.0.0-dev`.

**This work must ship as `v0.7.0` or later**, because `init`'s scaffold pins
`infrena: ">= 0.7"` and a binary tagged below that would be refused by the very project it
just wrote. A development build reports `0.0.0-dev` and is exempt from the floor (§61.2), so
the mismatch would only show up on a released binary.

**Versioning (§61).** Six format versions already exist and stay INDEPENDENT —
`state.CurrentVersion`, `pluginproto.Version`/`Supported`, `planner.PlanVersion`,
`report.Version`, and the module lockfile and cache. One shared number would mean a state
migration every time a report gained a field. `infrena version` prints all of them, reading
each from the package that owns it.

The product is semver, and each bump is defined in terms of those formats: a patch changes
none, a minor may ADD one and must still read every older one, a major may drop support.
The version is never a constant in the source — `-ldflags -X` for a release,
`debug.ReadBuildInfo` otherwise, and a development build says `0.0.0-dev` rather than
claiming a release. Anything that parses as 0.0.0 is treated as "not a release", because a
`go build` in a checkout with a remote produces a pseudo-version that parses that way.

**The configuration language is not versioned**, deliberately (§61.2): it is additive and
already fails closed on unknown keys, so a version integer would buy only a better message
while costing a key in every file that is wrong by default. Instead an optional
`infrena: ">= 0.4"` floor, sharing its constraint syntax with `plugins:`, checked
immediately after decoding so a binary that cannot understand a project says so once.

**A plugin repository ships `plugin.yaml`** (§31.2, agreed 2026-09-13): `manifest`, `name`,
`version`, `protocol`, `platforms`, `description`, and optionally `infrena` and `source`.
It is fetched over HTTP **before any binary is downloaded**, so a search can judge
compatibility without one — and therefore **read at the TAG, never the default branch**,
which describes unreleased code. Unlike the configuration language (§61.2) the format IS
versioned, because its reader cannot be upgraded in step with its writer. It deliberately
carries no checksums (they postdate the build; `SHA256SUMS` is a release asset) and no
asset names (a convention mirrors infrena's own releases).

`pkg/pluginmanifest` parses and validates that file — public so a plugin validates its own
manifest with the same code `plugins install` will, and because a plugin repo whose rule is
"stdlib plus infrena" cannot add a YAML parser itself. It touches no `yaml.Node`, so
internal/config remains the only place in the engine that does.

**`plugins:` constrains provider plugin versions** (§31.1), keyed by plugin because two
instances of one plugin share one process and therefore one version. The LOADER enforces
it — four paths load plugins, and a constraint checked in three is one nobody can rely on
— while the compiler refuses a constraint naming a plugin the project does not use. A
plugin reporting no version (`0.0.0`, the SDK's answer when `Version()` is absent)
satisfies nothing and says so in its own words; that is deliberately the opposite of
§61.2's `infrena:` floor, which exempts a 0.0.0 build because it is the user's own.

**Where a plugin may come from** (§31.3, built 2026-09-16). A source has two forms:
`github.com/<owner>` means search that owner for repositories named `infrena-provider-*` or
`infrena-backend-*`, and `github.com/<owner>/infrena-provider-<name>` (or
`infrena-backend-<name>`) means that one repository exactly. The naming convention is
load-bearing — an owner search works by repository name — so `internal/plugins.ParseSource`
refuses a repository matching neither prefix at the moment it is written, rather than
letting it silently never match. An owner search lists BOTH prefixes from the one call,
because an owner publishes both and filtering to one would make a trusted owner's backend
invisible.

**`plugins.Kind` AND `plugins.Role` ARE DIFFERENT QUESTIONS, and the names invite
conflation.** `Kind` (`KindOwner`, `KindRepository`) is owner-versus-repository: which of
the two shapes above a SOURCE is. `Role` (`RoleProvider`, `RoleBackend`) is
provider-versus-backend: what the plugin DOES. `Kind` was there first and means the former;
renaming it to `SourceKind` was considered and rejected as churn. **The user-facing flag is
`--kind` while the type it parses into is `Role`**, deliberately, because kind is the word a
person types — `parseRole` in `internal/cli/plugins_project.go` is the one place the two
spellings meet. A repository source carries a `Role` from its prefix; an OWNER source
carries none and must not pretend to, since an owner publishes both.

**A project may NAME a source; only a user may TRUST one.** `infra.yml`'s `plugins:` key
takes an additive mapping form (`version:` and `source:`) beside the scalar form, and a
source written there is a CANDIDATE, not a permission: that file is checked into git and
travels with a clone, so if it could grant a download source then `git clone && infrena
plan` would be enough for a repository to introduce a place infrena fetches executables
from. Trust lives only in the user's own `~/.config/infrena/plugins.yml`, read by
`plugins.LoadTrusted`. `github.com/infrena` is always trusted and cannot be removed — a
default that can be configured away is one that gets configured away, after which
`plugin: aws` reports that nothing matches with no visible cause. A malformed trust file is
an error naming the file, never a quiet fallback. A MISSING CONFIG DIRECTORY degrades
exactly like a missing config file: `os.UserConfigDir` fails outright with neither HOME nor
XDG_CONFIG_HOME, which is an ordinary minimal container, and `LoadTrusted("")` then returns
the official owner alone rather than erroring - a search that cannot name a config
directory must still be able to search the owner that needs no configuration. The empty
config home is handled inside `LoadTrusted`, because `TrustedPath("")` is a RELATIVE path
and reading it would let a file in the current working directory decide what infrena
trusts.

**THE NETWORK IS NEVER ON THE HOT PATH.** `validate`, `plan`, `apply`, `destroy`,
`refresh`, `discover`, `import`, `graph`, `explain` and `state` must never make a network
request for plugin discovery, ever — a `plan` that consults the network behaves
differently on a train, in a locked-down CI runner and during a GitHub outage, which
invariant 6 forbids. `infrena plugins list` answers "what am I actually running" entirely
offline. Searching and installing are the only places a request belongs, and the HTTP
client sits ABOVE `internal/plugins`, never inside it: `TestPluginsPackageCannotReachTheNetwork`
fails if `net/http` ever appears in that package's dependency graph, and `internal/cli`'s
`TestHotPathCommandsMakeNoNetworkRequest` runs `validate`, `plan`, `graph`, `explain` and
`state` with every proxy variable pointed at a counting listener and asserts zero attempts.
The two catch different mistakes — one the import, the other the call — and neither
subsumes the other. **Do not delete either when wiring anything new into a hot-path
command.**

**`internal/plugins/remote` is the only package in infrena that talks to a forge**
(§31.3, built 2026-09-16), on `net/http` and `encoding/json` alone — no GitHub or HTTP
library, because the third-party budget is Cobra and yaml.v3 and nothing else. Four rules
in it are easy to break:

- **A manifest is read at the git TAG, never the default branch** (§31.2). The default
  branch describes unreleased code, so judging compatibility from it reports a plugin as
  compatible that nobody can install. `FileAtTag` puts the tag in the PATH rather than a
  query parameter, so a request that lost it cannot fall back to a branch.
- **An owner is a user OR an organisation, and both listings are tried.** GitHub serves
  `/orgs/{owner}/repos` for one and `/users/{owner}/repos` for the other, with no endpoint
  covering both: an organisation answers the user listing with 200 and an empty array, so
  asking one shape reported `github.com/infrena`, which IS an organisation, as publishing
  nothing. `Repositories` asks both and unions them; a 404 from one shape is ordinary when
  the other answered, only both failing is a failure, and any OTHER refusal (a rate limit,
  a forbidden) stops the search rather than being unioned away into an empty list.
- **A rate limit is NEVER reported as not-found.** GitHub answers an exhausted allowance
  with 403 and a missing-or-private repository with 404, and an unauthenticated caller gets
  sixty requests an hour — which one owner search can spend. `RateLimitError` and
  `NotFoundError` are separate types and must stay so: the two messages send a reader to
  completely different places, one to wait or set `INFRENA_GITHUB_TOKEN` (falling back to
  `GITHUB_TOKEN`), the other to check a spelling. `ForbiddenError` is the third case — a
  403 that is not a rate limit — for the same reason.
- **"Nothing found" is only ever said by a search that could SEE.** An unauthenticated
  listing of an owner with private repositories returns 200 and an empty array, so with no
  token `plugins search` says what it saw - that it ran unauthenticated, that a private
  repository is invisible that way, and which variable to set - rather than "no plugin
  named X in any source. Check the spelling". Same rule as the rate limit, different door;
  the definitive wording is used only when a token was set.
- **Two owners publishing one name are both shown, and infrena never picks.** Not the first
  alphabetically, not the higher version, not the official one. `plugins.Search` sorts by
  source then newest version so repeated searches render identically, and that ordering is
  PRESENTATION ONLY: nothing may take element zero. A source that fails is returned
  alongside the results and rendered as a warning, never allowed to turn a partial answer
  into "nothing found".
- **The cache is an optimisation and behaves like one.** `remote.Cache` stores answers for
  `DefaultTTL` (an hour, matching the rate-limit window) under `remote.DefaultCacheDir()`;
  every failure on the read path is a MISS rather than an error, and `--refresh` is spelled
  as a TTL of zero, which makes everything stale. A cache that can fail a command is worse
  than no cache at all.

**Install, the lock, and verification on launch** (§31.3, built 2026-09-16). The order of
`plugins install` IS the design: search, filter to what can run here, refuse if that leaves
anything other than exactly one, check trust, prompt ONLY if a person is there, fetch
`SHA256SUMS`, download, VERIFY, extract, move into place, write the lock. Every step is a
refusal rather than a repair, and a refusal at any step leaves NOTHING on disk. Verification
comes before extraction because a tampered archive must be refused without being opened;
the lock is written last because it records what is already there. **A run with nobody at
the terminal never approves a source**, and infrena never chooses between two sources
answering one name.

`plugins.lock` is committed, keyed by plugin name, with a checksum per `GOOS/GOARCH`
(`plugins.PlatformKey` — one spelling, in the lock's own package, because install writing
one key and the host reading another is a lock that verifies nothing while looking exactly
like one that verifies everything). **TWO ABSENCES, TWO ANSWERS:** a plugin the lock does
not mention passes, because a hand-placed binary keeps working and the lock governs what
INSTALL put there; a plugin the lock DOES mention on a platform it does not record FAILS,
because "no entry for your machine" is a gap to see rather than permission to run whatever
is there.

`pluginhost.Loader` hashes a locked binary BEFORE launching it — after the process starts is
after its code has run — and a lock that cannot be read refuses every launch rather than
being treated as absent. **`backendhost.Open` does the identical thing for a state backend**
(2026-09-16), which is why it takes the project directory: a backend reads and writes the
whole of your state, every secret in it included, so an unverified backend binary is if
anything worse than an unverified provider. It is keyed in the lock as
`infrena-backend-<name>` (`backendhost.LockKey`) rather than the bare name, because one key
cannot hold two different binaries — a provider `s3` and a backend `s3` are separate
repositories under §31.3's convention. A binary that fails its checksum is a `pluginhost.LockError` with
its OWN diagnostic: reporting it as "the plugin is not available" and advising an install
tells a user to install something already on their disk, which is exactly the §44 failure
the version-constraint branch beside it already exists to avoid.

**Installing a backend, and which kind `install` thinks you meant** (§31.3, built
2026-09-17). A provider and a backend can both be called `s3`, and the honest reading is
that this is ONE MISSING QUALIFIER rather than a broken convention: configuration
(`providers: - plugin: s3` against `backend: plugin: s3`), the binaries on disk
(`infrena-plugin-s3` against `infrena-backend-s3`), the lock keys (`s3` against
`backendhost.LockKey`'s `infrena-backend-s3`) and the two typed not-found errors
(`pluginhost.NotFoundError`, `backendhost.NotFoundError`) already tell the two apart. The
ONLY ambiguous context is the CLI's bare `<name>` argument. Do not "fix" the convention;
supply the qualifier where it is missing. `list` and `search` do it by showing a `KIND`
column — `list` infers it from the binary's prefix, which needs no new state —
and `runInstall` resolves it in four rungs:

1. `--kind`, if given. The user said so. A `--kind` for a kind nobody publishes is an error
   naming what DOES exist, never a silent swap to the other one.
2. What the project declares — `NeededPlugins()` for providers, `Backend.Plugin` for the
   backend, both when both. **This outranks asking**, and that ordering is the point of the
   feature: making someone repeat on the command line what `infra.yml` already says is
   asking them to keep two places in step. `declaredRoles` DECODES and never compiles, the
   same rule `backendFor` follows, because a project with a broken resource must still be
   able to install the plugin that would fix it. When this rung decides, install SAYS which
   kind it chose — a choice made on the user's behalf has to be visible.
3. The search result, when only one kind answers the name. Nothing to disambiguate.
4. Otherwise refuse, showing both and naming `--kind`. Same rule as two owners answering one
   name: never auto-pick.

**`infrena plugins install` with no name installs everything the project declares**, which
is what a fresh clone needs and which cannot be ambiguous by construction — the project
states both the names and the kinds, so no rung above runs. Already-installed is a skip, and
one failure does not hide the others: every plugin is reported and the command fails at the
end, as `discovery.Walk` does with partial answers. `--kind` with NO name is refused for the
same reason.

**A release asset is named from the BINARY, not from the plugin name.** `remote.AssetName`
takes the binary because a backend publishes `infrena-backend-s3_<version>_<os>_<arch>.tar.gz`
while a provider publishes `infrena-plugin-<name>_...`; it used to paste the provider prefix
in front of whatever name it was given, so a backend install asked for an asset no release
publishes. **The test fixture had guessed provider naming too**, and fixing it to serve the
real names immediately failed the old code with "publishes no checksum for
infrena-plugin-hetzner_...". That is the lesson, not the bug: a fake that answers whatever
the code asks for validates any implementation, right or wrong, which is how five bugs
shipped through a fake GitHub. Serve the names a real release serves.

**`plugins verify` was silently broken by the backend lock key, and is fixed.** A lock entry
spelled `infrena-backend-s3` was looked up as a provider of that whole string, so verify went
hunting for `infrena-plugin-infrena-backend-s3` and reported a backend install had just
written as missing. `verifyOne` now reads the kind off the key. Worth knowing because a green
suite hid it until a test went looking: the key had changed for one command and nobody asked
what else read the file.

**The interactive offer** (`internal/cli/plugins_offer.go`) fires only when stdin is a
terminal, `--output` is unset and `INFRENA_NO_PLUGIN_SEARCH` is unset; it searches, shows
every candidate with its reason, asks, installs, and then STOPS. Stopping is structural
rather than remembered: it hangs off the path a command takes when its configuration did not
compile, so there is no second half of the run to get wrong. **Its terminal test excludes
`/dev/null`**, which is a character device like any tty and is what `go test` and most CI
runners hand a process — without that, a failing `plan` reaches the network, which is the
hot-path rule broken by the feature meant to respect it. `stdinIsTerminal` is a package
variable because there is no terminal inside `go test` to stand one up against.

**Known limit:** `install <name>@<version>` can only satisfy a version that is the
repository's latest release, because a search reads only the latest tag. It refuses, naming
what IS published, rather than installing something else.

`plugins.Fetcher` is why `plugins.Search` stays in the network-free package: it names the
SHAPE of the three calls, `remote.Client` satisfies it, and `internal/cli` is the one place
that knows about both. `INFRENA_GITHUB_API` redirects the client at another host and is a
TEST SEAM, not a user-facing feature — supporting another forge is a source-syntax
question, not an environment-variable one.

The CLI surface to implement (§37): `init`, `validate`, `plan <env>`, `apply <env>`,
`destroy <env>`, `state` / `state show <address>`, `refresh <env>`, `import`, `export`,
`discover`, `graph`, `explain <resource-type>`. Global options: `--var`, `--var-file`,
`--output`, `--auto-approve`, `--parallelism`, `--verbose`.

**`--output` means ONE thing now** (2026-09-15, `PLAN.md` §37.2): every command writes the
same newline-delimited JSON report, and **stdout goes byte-empty**. It used to mean two
different things and that was called deliberate; it is not any more. A `meta` line carries
the format version, then `event`, `observation` and `diagnostic` lines as work happens
(keeping §44's four parts separate), and a final `result` line, so a consumer tails the file
and reads the outcome from the last line. Reports REDACT through `pkg/value.Format`, the
single redaction path: a report is read by things that do not need the secret. The
`version` field exists so the wire format can change without breaking consumers; never
omit it.

`plan --output` writes that stream too: a `meta` line and a `plan` line carrying the
artifact verbatim, and **no `result` line**, because the plan is the product. The artifact
itself is unchanged — sensitive values in cleartext, because `apply --plan` reads it back
and needs the real ones, which is why every `--output` file is 0600 — and **`operations`
still lists EVERY resource, including `kind: "noop"`**, so a consumer counting entries is
not counting changes. `Plan.HasChanges()` decides the exit code. **`apply --plan` reads
BOTH envelopes**, the bare document and the stream, because plans saved by earlier
releases are on disk and refusing one would break applying a plan already reviewed.

**`report.Version` is 2; `planner.PlanVersion` deliberately stayed 1.** The report format
gained a line kind, so a consumer that only understands 1 must be able to tell. The plan
document's own schema did not move — only the envelope around it — and `DecodePlan`'s
version check is an outright refusal, so bumping `PlanVersion` would reject every plan
already on disk in exchange for nothing. §61 keeps the two numbers independent for exactly
this reason. `DecodePlan` separately refuses ANY document carrying a `type` field: without
that guard a report stream decodes as its own `meta` line and yields a plan with no
operations, which applies nothing, silently.

**Progress reaches stdout only when `--output` is absent.** It is human output, never a
diagnostic, so it does not go to stderr. Lines are append-only with no cursor control, so
a CI log or a redirected file stays readable and there is no TTY detection to test. They
are emitted in COMPLETION ORDER and are deliberately not byte-stable, which is why
invariant 6's determinism test compares from the `Plan for project` line onward.

**Exit 77** (sysexits.h `EX_NOPERM`) is a fourth exit code: changes present, no
`--auto-approve`, no `--plan`, and approval unobtainable — either `--output` is set so
nobody can see a prompt, or stdin is at EOF so nobody can type. The two causes are detected
in two places (the flag check straight after planning, the EOF at the prompt inside
`confirm`), and both are **before `withLockedEnvironment`**, so a run that exits 77 has
taken no lock and mutated nothing. It applies to `apply` and `destroy` alike: destroy's
higher bar does not make approval obtainable.

## Architecture

Planned layout (§41):

```
cmd/infrena/        CLI entrypoint
internal/           config, compiler, expressions, environments, modules, variables,
                    state, planner, graph, executor, discovery, importer, generator,
                    lifecycle, secrets, cli, plugins (sources, trust, search — no
                    network), plugins/remote (the only package that talks to a forge)
pkg/                provider, schema, plan, resource, pluginproto, pluginsdk, plugintest,
                    semver, pluginmanifest   (the stable-ish interfaces; plugin authors
                    compile against these, so §61.1's rules govern changing them)
providers/          test/ (fake provider), aws/
tests/integration/
```

The pipeline every mutating command flows through (§3.3):

```
configuration → compile → validate → refresh current state → diff
→ dependency graph → execution plan → approval → apply
```

Key architectural rules, in rough order of how easy they are to violate:

- **The core engine must not know about AWS.** Providers sit behind the `Provider`
  interface (§31: `Discover`/`Read`/`Create`/`Update`/`Delete`/`Import`). No AWS types
  or AWS-specific branching in `internal/planner`, `internal/executor`, or the compiler.
- **Parse YAML into typed models immediately.** Raw `map[string]any` must not flow
  through the application. Define `Project`, `Environment`, `Module`, `Resource`,
  `ResourceDefinition`, `DesiredResource`, `ResourceState`, `Plan`, `PlanOperation`,
  `Dependency`, `Variable`, `Expression`, `Provider` (§42).
- **Preserve value provenance.** Every resolved value carries a `ValueSource`
  (`explicit`, `default`, `environment`, `variable`, `module`, `computed`, `provider`)
  — §43. Do not merge defaults into user config and lose the origin: plans mark
  defaults `[default]`, generation emits minimal YAML, and `explain` all depend on this.
- **Addresses embed the module path, so moving a resource between modules
  destroys it.** Stage 5 flattens: after it, nothing downstream knows modules
  exist (spec §7.2). That simplification is paid for twice — `Origin.Module` is
  the only thing letting a diagnostic say which instantiation a problem came
  from, and an address is coupled to module structure, so renaming an
  instantiation or moving a resource between modules renames the resource, and
  a renamed resource is destroyed and recreated rather than moved. `state mv`
  is deferred past Phase 1 (§5.2). `infrena plan` notes the destroy/create pair
  when it sees one (`moveCandidates` in `internal/planner/render.go`), which is
  the only warning a user gets; do not remove it without replacing it with
  something a user reads before typing `apply`.
- **State is the last thing persisted; the observation is what is out there now, and
  they must not be merged before the planner has run.** `planner.Compute` takes both
  because its diff IS one against the other; write observations into state first and
  the diff collapses, the plan proposes nothing, and drift silently stops being
  corrected. State stays "what the last apply recorded" everywhere, and the merge
  happens strictly between planning and execution, in `executor.currentFor`
  (`internal/executor/observed.go`), which is what builds the `current` a provider's
  `Update` and `Delete` receive: the OBSERVED attributes carrying the host's own
  bookkeeping — provider instance, `Dependencies`, `Lifecycle`, the timestamps — from
  state. Not a straight swap: `internal/pluginhost.rebuild` builds the state that gets
  PERSISTED out of `current`, so a `current` missing its `Lifecycle` writes a vanished
  `prevent_destroy` guard to disk. The executor used to hand providers state alone, and
  a plugin that diffed `current` against `desired` therefore skipped exactly the drift
  the plan had proposed to correct.
- **Environments are first-class, not workspaces.** Each environment has independent
  state and its own lock. Inheritance (`extends`) resolution order is
  provider defaults → base config → module defaults → environment inheritance →
  environment variables → CLI overrides (§7). Explicit config always wins over an
  implicit default.
- **Expressions stay constrained.** `${var.name}` interpolation and `${resource.attr}`
  references, with a small set of pure functions eventually. This is deliberately not
  a programming language (§10).
- **The configuration language is a product API.** Even pre-1.0, weigh backwards
  compatibility before changing YAML syntax, and document decisions that affect it.

## Invariants that must always hold (§47)

Test these explicitly; they are the correctness definition of the product.

1. **Removal ⇒ destroy.** In state but absent from configuration ⇒ plan proposes
   destroy, unless `lifecycle.prevent_destroy` or `lifecycle.retain` says otherwise
   (`retain` drops it from management without deleting the real resource).
2. **No-op plan.** Desired == actual ⇒ zero operations.
3. **Import round trip.** discover → import → generate minimal YAML → plan ⇒ no
   unexpected changes. This is an integration test (§29).
4. **Dependency ordering.** A resource never executes before its dependencies.

> **Addresses embed the module path** (spec §5.2, §7.2). Moving a resource between modules
> renames it, which the planner reads as a destroy plus a create. This is the price of
> compile-time flattening and is documented in `PLAN.md` §11 and, where a user actually
> meets it, on the destroy operation itself — the plan notes it was "also created in this
> plan" at a different module path (`moveCandidates` in `internal/planner/render.go`).
> `state mv` is deferred past Phase 1.

5. **Locking.** Two applies cannot mutate the same environment concurrently; different
   environments concurrently is fine.
6. **Plan determinism.** Same configuration + state + provider observations ⇒
   equivalent plan.

## Build order

**Phase 1 (§49) and Phase 2 (§50) are DONE**, and the plugin protocol (§31.1) that §50.1
placed before AWS is done with them. Phase 1 built the core engine against the fake provider,
which had to support create/read/update/delete/drift/import/dependencies/failures so the whole
engine is testable without cloud credentials — it now does that from its own repository, as a
plugin. The MVP §48 describes runs end to end, `discover` / `import` / `export` included.

**Phase 3 is AWS, and it is next** (§51 — VPC, subnet, security group, S3, RDS Postgres, IAM
role/policy attachment, ECS cluster/task definition/service, ALB, Route53; complete lifecycle
support beats resource breadth). It is also the first thing built as a plugin from its first line
rather than ported into one, which is why the protocol was finished first.

**It belongs in its own repository**, `infrena-provider-aws`, NOT in this one (decided
2026-09-13, §31.1). Not merely for the dependency budget — a nested module would handle that —
but because **Go's internal rule is by import path, not by module boundary**: a module named
`github.com/infrena/infrena/providers/aws` can import `internal/pluginhost` and compile, while
any outside module cannot. That was measured, not assumed. So the rule is that **a plugin's
module path must never be under `github.com/infrena/infrena/`**, which is what keeps an official
plugin on exactly the footing a third-party plugin has.

**Then §31.3 — finding and installing plugins — before Phase 4 or 5.** `infrena plugins
install`, a committed `plugins.lock` with per-platform checksums, sources a user trusts, and
the offer to install a plugin a project names and the machine lacks. It used to be filed
under Phase 5, which was right while the fake provider was the only plugin; a released AWS
provider with no install path means every user hand-places the binary that touches their
production account. Two rules in that design are easy to violate and worth knowing before
touching it: **a project may NAME a plugin source but only a user may TRUST one** (project
configuration travels with a `git clone`, so it must not be able to grant a download
source), and **no command on the hot path may touch the network** — searching happens in
`infrena plugins ...` and in one interactive prompt, never in `plan` or `apply`. All three
units shipped 2026-09-16: sources, trust and `plugins list`/`plugins search`, then
`plugins install`, the lock file and verification on launch, then the interactive offer.

After that, Phase 4 remote state (§52) — all four steps shipped: the backend boundary
2026-09-16, then `infrena-backend-s3`, the concurrency tests and `state migrate`
2026-09-17 — and Phase 5 production features (§53).

§54 lists what **not** to build yet: the full AWS surface, a general-purpose language,
web UI, SaaS control plane, distributed execution, Kubernetes/GCP/Azure providers, a
complex policy language.

## Product-quality expectations

These are features, not polish, and are easy to under-deliver on:

- **Missing-dependency detection before apply** (§17–18). Providers declare resource
  requirements; planning reports what is missing with suggested fixes rather than
  letting a provider API call fail.
- **Error messages** (§44) state what is wrong, in which environment, what was expected,
  and a suggested action.
- **Plans** distinguish creates/updates/deletes/replacements, mark `[default]` values,
  redact sensitive values (`<sensitive>`, §36), and loudly flag destructive changes in
  production (§20, §38 — `require_approval` / `prevent_destroy` are engine-enforced,
  not conventions).
- **Generated configuration is minimal** — omit anything equal to a provider default
  (§27). Full dumps are `infrena export` only (§28).
- **Retries** classify every operation as safe-to-retry / conditionally-retryable /
  not-safe-to-retry; never blindly retry destructive operations (§35).
- **Concurrency** is bounded per provider/account to avoid API throttling (§34).

## A green suite is not evidence

Over 2026-09-15 to 17, **every serious bug was found by running something against a real service,
and none by the suite going green.** The suite was passing in every one of these cases:

- Five bugs in plugin search, against a fake GitHub. The wrong API endpoint entirely, private
  repositories reported as "no such plugin", a refusal to run with no `HOME`, and a cached
  anonymous negative served to an authenticated search for an hour — which defeated the very
  remedy the previous fix suggested.
- An archive extractor that rejected **every real release on every platform**, because "refuse
  anything that is not a regular file" also refused the directory entry `tar -czf` puts in front
  of it.
- An install asking for `infrena-plugin-s3_…`, an asset no release publishes, because the test
  fixture had guessed provider naming.
- `plugins verify` silently broken for hours by a lock-key change made the same day.
- The reference in-memory backend shipped with the SDK violating its own contract: a run whose
  lock had been force-unlocked and taken over could still write over the new holder.
- `scripts/build-release` broken by `CDPATH` being set, so `cd` echoed its target into a path.

The pattern is one thing. **A test proves the code does what you think. It does not prove the
world agrees.** A fixture answers whatever it is asked, so it validates any implementation,
right or wrong — including one asking a service for something that does not exist.

### What actually catches these

- **Run the binary against the real service before calling anything done.** Once. Every item
  above took one real run to find and none were found by more testing.
- **Key a fixture on strings captured from the real service**, never on what you assume it says.
  The S3 backend's fake carries MinIO's actual `PreconditionFailed` and `NoSuchKey` text; the AWS
  provider's reconciliation is keyed on what Cloud Control really returns.
- **Make a skipped suite a failure.** `INFRENA_REQUIRE_PLUGIN=1` for the integration suite,
  `REQUIRE_LIVE_STORE=1` for the S3 backend's. A suite that silently skips has already reported
  green on tests that never ran, here, more than once.
- **Sabotage every guard.** Break it deliberately and confirm a test fails, and that the RIGHT
  test fails. `pkg/backendtest` goes further and keeps a broken backend per rule, asserting each
  check catches its own violation and no other — a check that fires on the wrong rule is as
  useless as one that never fires.
- **Prove a floor rather than reasoning about it.** `infrena-backend-s3` builds a host from the
  oldest release its `plugin.yaml` admits and runs the suite against it, in CI.
- **Verify a new public API from outside its module.** A generic signature that only compiles in
  its own package is a package nobody can use.

### Two related rules already stated elsewhere, for the same reason

**Never claim something does not exist when you merely could not see it** (§31.3). A rate limit,
an unauthenticated listing of private repositories, and a cached negative from another credential
are three doors into one mistake.

**A value the provider rewrites converges only if the plugin reconciles it** — see
`docs/provider-hazards.md`. Neither that hazard nor silent cross-resource coupling can be
reproduced against the fake provider at all, which is why both were found against real AWS.

## Decision defaults

When `PLAN.md` does not cover something (§58): prefer simple designs and standard Go
patterns, explicit behaviour over magic, keep provider specifics out of the core,
preserve provenance and plan determinism, avoid unnecessary abstractions, and do not
skip tests to move faster.
