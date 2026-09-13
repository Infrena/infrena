# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> Project notes (source of truth): `Obsidian Vault/projects/labs/infra-tool.md`

## Current state

**PHASE 1 IS COMPLETE; PHASE 2 IS UNDER WAY.** M1-M7 are merged to `main` (tags `m1`-`m7`),
and M8 (discovery and import) is on `m8-discovery`. Every one of `PLAN.md`
§49's nineteen components exists, the last being the module system (M5) and the Phase-1 CLI
surface (M6).

The MVP workflow §48 describes runs end to end against the fake provider, as far as Phase 1
reaches: `init` → `validate` → `plan` → `apply` → re-plan clean → externally mutate → `refresh`
→ drift shown → remove from YAML → destroy proposed → `apply`, plus `graph` and `explain`. The
three commands §48 ends on — `discover`, `import`, `export` — are **Phase 2** (§50), and
`providers/test` returns `ErrNotImplemented` for `Discover` and `Import` saying so.

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
and refuses to overwrite; `graph` renders the dependency tree from the same compile `plan` does;
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
teardown first. `${project}` is a fourth process variable.

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

**Absent until Phase 3+:** reading a saved plan back, remote state, AWS. Nothing half-implements
one of those.

**Planned before Phase 3: provider plugins as separate processes** (`PLAN.md` §31.1, §50.1).
Plugins are separately distributed Go binaries speaking newline-delimited JSON over stdio, with
the standard library only. Official plugins, including AWS and the fake provider, ship that way
too. Two consequences constrain work done before it lands:

- **Nothing in `pkg/schema` may gain another function-typed field.** `DefaultFunc`, `Validate`
  and `ImportSpec.Parse` are being removed because a function cannot cross a pipe.
- **Do not add a guarantee that rests on a provider obeying a comment.** Read's carry-forward,
  non-nil-on-success and sensitivity flags are moving into the engine's host adapter, because a
  third-party binary cannot be held to a doc comment.

## Name

The product is **Infrata** (GitHub org `infrata`, command `infrata`). The Go module path
and binary are still `infra` until a dedicated rename, so don't rename piecemeal inside
feature work.

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
third-party budget so far. AWS SDK v2 arrives with Phase 3, inside `providers/aws`'s OWN
`go.mod`, so it never enters the core module (§31.1).

**`mise` is not active in non-interactive shells.** Either use the `make` targets, which set
the shim path structurally, or `export PATH="$HOME/.local/share/mise/shims:$PATH"` first.
A bare `go` resolves to 1.20 and fails. Check with `go version` if anything looks odd.

```bash
go build ./cmd/infrata        # build the binary
go test ./...                 # full unit + fake-provider integration suite
go test ./internal/planner/   # one package
go test -run TestPlanDestroy ./internal/planner/   # one test
go vet ./...
gofmt -l .
```

AWS integration tests must be opt-in (build tag or env guard) — **normal CI must not
require AWS credentials** (§46).

**CI** (`.github/workflows/ci.yml`) runs gofmt, vet, the suite and `-race`.
`GOTOOLCHAIN=local` is set for the whole job, because Go otherwise downloads a newer
toolchain to satisfy `go.mod` and a floor break would pass by fetching the very version
it should have failed on. `go.mod`'s floor currently MATCHES `mise.toml`, so the matrix
has one entry; the moment the floor drops below the pin, add the older version as a
second entry — two toolchains is the only way the older promise is ever tested. The floor
is a promise to plugin authors, and a plugin repo must declare at least it. It also runs on
`merge_group`, and is called by the release workflow so a release cannot skip it.

**Releases** (`.github/workflows/release.yml`) fire on a `v*` tag and cross-compile eight
platforms from one runner — this is pure Go with no cgo, so a matrix of operating systems
would buy nothing. `CGO_ENABLED=0` for static binaries; `-trimpath`; deliberately NOT
`-s -w`, because the engine lets an unrecovered panic crash the process rather than
recover mid-apply, which makes the symbol table the whole diagnostic.

**The version is stamped only there** (§61.1), so a release build is the only one that
reports a real version. A step asserts the built binary says the tag — a wrong `-ldflags`
path would otherwise ship binaries silently reporting `0.0.0-dev`.

**Versioning (§61).** Six format versions already exist and stay INDEPENDENT —
`state.CurrentVersion`, `pluginproto.Version`/`Supported`, `planner.PlanVersion`,
`report.Version`, and the module lockfile and cache. One shared number would mean a state
migration every time a report gained a field. `infrata version` prints all of them, reading
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
`infrata: ">= 0.4"` floor, sharing its constraint syntax with `plugins:`, checked
immediately after decoding so a binary that cannot understand a project says so once.

**A plugin repository ships `plugin.yaml`** (§31.2, agreed 2026-09-13): `manifest`, `name`,
`version`, `protocol`, `platforms`, `description`, and optionally `infrata` and `source`.
It is fetched over HTTP **before any binary is downloaded**, so a search can judge
compatibility without one — and therefore **read at the TAG, never the default branch**,
which describes unreleased code. Unlike the configuration language (§61.2) the format IS
versioned, because its reader cannot be upgraded in step with its writer. It deliberately
carries no checksums (they postdate the build; `SHA256SUMS` is a release asset) and no
asset names (a convention mirrors infrata's own releases).

**`plugins:` constrains provider plugin versions** (§31.1), keyed by plugin because two
instances of one plugin share one process and therefore one version. The LOADER enforces
it — four paths load plugins, and a constraint checked in three is one nobody can rely on
— while the compiler refuses a constraint naming a plugin the project does not use. A
plugin reporting no version (`0.0.0`, the SDK's answer when `Version()` is absent)
satisfies nothing and says so in its own words; that is deliberately the opposite of
§61.2's `infrata:` floor, which exempts a 0.0.0 build because it is the user's own.

The CLI surface to implement (§37): `init`, `validate`, `plan <env>`, `apply <env>`,
`destroy <env>`, `state` / `state show <address>`, `refresh <env>`, `import`, `export`,
`discover`, `graph`, `explain <resource-type>`. Global options: `--var`, `--var-file`,
`--output`, `--auto-approve`, `--parallelism`, `--verbose`.

`--output` means two different things, deliberately. For `plan` it writes the plan
ARTIFACT: one JSON document, sensitive values in cleartext, because M6 reads it back to
apply it and needs the real values. **`operations` lists EVERY resource, including
`kind: "noop"`** — it describes the whole plan, not just its changes, so a consumer
counting entries is not counting changes. `Plan.HasChanges()` is what decides the exit
code. For `validate`, `apply`, `refresh` and `destroy` it
writes a REPORT: newline-delimited JSON — a `meta` line carrying a format version, then
`event` or `observation` lines as work happens, `diagnostic` lines keeping §44's four
parts separate, and a final `result` line, so a consumer tails the file and reads the
outcome from the last line. Reports REDACT through `pkg/value.Format`, the single
redaction path: a report is read by things that do not need the secret. Both are 0600.
The `version` field exists so the wire format can change without breaking consumers;
never omit it.

## Architecture

Planned layout (§41):

```
cmd/infrata/        CLI entrypoint
internal/           config, compiler, expressions, environments, modules, variables,
                    state, planner, graph, executor, discovery, importer, generator,
                    lifecycle, secrets, cli
pkg/                provider, schema, plan, resource, pluginproto, pluginsdk, plugintest,
                    semver   (the stable-ish interfaces; plugin authors compile against
                    these, so §61.1's rules govern changing them)
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
  is deferred past Phase 1 (§5.2). `infra plan` notes the destroy/create pair
  when it sees one (`moveCandidates` in `internal/planner/render.go`), which is
  the only warning a user gets; do not remove it without replacing it with
  something a user reads before typing `apply`.
- **Environments are first-class, not workspaces.** Each environment has independent
  state and its own lock. Inheritance (`extends`) resolution order is
  provider defaults → base config → module defaults → environment inheritance →
  environment variables → CLI overrides (§7). Explicit config always wins over an
  implicit default.
- **Expressions stay constrained.** `${var}` interpolation and `${resource.attr}`
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

Do not start with AWS. Phase 1 (§49) is the core engine against the **fake provider**
(`providers/test/`), which must support create/read/update/delete/drift/import/
dependencies/failures so the whole engine is testable without cloud credentials.

MVP is done when this works end to end against the fake provider (§48): `init`,
`validate`, `plan dev`, `apply dev`, re-plan showing no changes, externally mutate the
fake infra and see drift, remove a resource from YAML and see a destroy proposed,
`apply`, then `discover` / `import` / `export`.

Then Phase 2 discovery+import (§50), Phase 3 AWS (§51 — VPC, subnet, security group,
S3, RDS Postgres, IAM role/policy attachment, ECS cluster/task definition/service, ALB,
Route53; complete lifecycle support beats resource breadth), Phase 4 remote state
(§52), Phase 5 production features (§53).

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
  (§27). Full dumps are `infra export` only (§28).
- **Retries** classify every operation as safe-to-retry / conditionally-retryable /
  not-safe-to-retry; never blindly retry destructive operations (§35).
- **Concurrency** is bounded per provider/account to avoid API throttling (§34).

## Decision defaults

When `PLAN.md` does not cover something (§58): prefer simple designs and standard Go
patterns, explicit behaviour over magic, keep provider specifics out of the core,
preserve provenance and plan determinism, avoid unnecessary abstractions, and do not
skip tests to move faster.
