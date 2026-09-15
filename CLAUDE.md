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

Three rules carry most of the weight, and each is a hazard rather than a nicety:

- **A generated file must never hold a secret.** A sensitive attribute is OMITTED and the file
  says so at the point of omission. A secret committed to git is a secret rotated, not deleted.
- **Configuration is written BEFORE state.** A resource in state that no configuration declares
  is what invariant 1 schedules for destruction, so failing between the two writes must leave
  the harmless half done: configuration-without-state plans a CREATE, which is visible and
  refusable; state-without-configuration plans a DESTROY.
- **Computed attributes are omitted because they cannot be set**, not for tidiness. Emitting one
  produces "is computed and cannot be set" — a file that does not load, pointing at a file the
  user never wrote.

Invariant 3 (the round trip) now has its test, the last of the six to get one. Its guarantee is
qualified — see §29.1: a deliberately omitted secret shows as one pending change, and the two
alternatives (writing it to disk, or dropping it from state) are both worse. Minimality is
asserted SEPARATELY from the round trip, because a generator emitting every attribute would
also plan clean.

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

**Absent until Phase 3+:** remote state and AWS. Nothing half-implements either.

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
                    lifecycle, secrets, cli
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
`infrena plugins ...` and in one interactive prompt, never in `plan` or `apply`.

After that, Phase 4 remote state (§52) and Phase 5 production features (§53).

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

## Decision defaults

When `PLAN.md` does not cover something (§58): prefer simple designs and standard Go
patterns, explicit behaviour over magic, keep provider specifics out of the core,
preserve provenance and plan determinism, avoid unnecessary abstractions, and do not
skip tests to move faster.
