# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> Project notes (source of truth): `Obsidian Vault/projects/labs/infra-tool.md`

## Current state

**M1-M4 are merged to `main`** (tags `m1`-`m4`); **M5 (modules) is complete on
`m5-modules`**. The reconcile loop closes end to end against the fake provider,
including through a module: `validate` → `plan` → `apply` → re-plan clean →
externally mutate → `refresh` → drift shown → remove from YAML → destroy proposed →
`apply`. ~52,235 lines of Go across 24 packages.

Present: the value model with per-leaf provenance and sensitivity, addressing, diagnostics,
declarative resource schemas, the provider interface, a hand-editable file-backed fake
provider, versioned state with atomic writes and `O_EXCL` locking, compiler stages 1-2 and
6-8, a generic dependency graph, provider refresh, the planner, the plan renderer, and —
new in M3 — the executor (a worker pool bounded globally and per provider, per-operation
state persistence, failure isolation, SIGINT handling, retries classified three ways) and
the commands `apply`, `destroy` and `refresh`.

New in M4: compiler stages 3 and 4, so PLAN.md §7's precedence chain is real end to end —
provider defaults → base configuration (`variables.yml`) → environment inheritance
(`extends`) → the selected environment → `--var-file` → `--var`, each rung winning in that
order. Typed variable schemas (`type`, `default`, `min`, `max`) validate the value that WINS
its rung — not every value supplied, so a bad entry in `variables.yml` that every environment
overrides is not reported today. A numeric literal is coerced to its declared kind wherever it
appears — exactly, or it is rejected. `Value` gained `Scope` (which rung supplied it) and `SuppliedBy` (which
input, at the CLI rung), so a plan names its own provenance: `size: 7 [variable, from
conf/prod-sizes.yml]`. Both fields are excluded from `Equal` and `ConfigHash` and both
round-trip through the plan artifact; that is proved through the binary, not asserted.
`validate`, `plan` and `apply` resolve variables identically; `destroy` and `refresh`
refuse the flags rather than accept and ignore them.

M5: modules, and compiler stage 5. `PLAN.md` §11 was rewritten on 2026-09-11 and the
module model is NOT what an earlier reading of it would suggest. Loading a module and
instantiating it are two separate steps:

```yaml
modules:                  # a LIST of sources. No inputs. Loading only makes a
  - ./modules/networking  # module available under a name.
  - https://github.com/acme/infra-app-stack:v1.2.0   # remote sources need :tag-or-hash
  - name: app_stack_v2                                # the mapping form exists only
    source: https://github.com/other/infra-app-stack:v2.0.0   # to override a name

resources:
  prod:
    type: module.app_stack   # instantiation is a RESOURCE whose type names a module
    replicas: 3              # its attributes are the module's inputs
```

Three consequences worth knowing before touching module code:

- **A module input is an ordinary `AttributeDecl`**, bound by stage 6's existing
  `bindAttribute`. Do not write scope/provenance handling for module inputs — a literal
  lands on `ScopeBaseConfig` and a `${count}` from `--var` keeps `ScopeCLIOverride` by
  the same path as every other attribute. `ScopeModuleDefault` is filled by the module's
  own declared `default:` and by nothing else.
- **`module.` is a reserved type prefix.** `internal/registry.Register` refuses a
  provider definition that claims it. That guard stops a colliding provider at startup;
  it is NOT reachable from user config, because stage 5 expands every instance away
  before stage 7 calls `Definition`.
- **Never fold module inputs by substituting and re-serializing.** That route calls
  `value.Expr.String()`, which returns `value.Redacted` — so a sensitive input would be
  applied as the literal string `<sensitive>`. Stage 5 records a per-instance scope
  instead (`modules.Expansion`); stage 6 resolves names against it.

Modules nest, bounded at depth 32 (a cycle and excessive depth are reported as distinct
diagnostics), and `ScopeModuleDefault` — the rung `PLAN.md` §7's precedence chain
declares and M4 left unpopulated — is now filled by a module's own declared `default:`,
closing that chain end to end. A module exposes `outputs:`, addressed by the caller as
`${call.output}`; the value may be unknown (it can read a computed provider attribute)
and reading an output the module does not declare is reported at `validate`, naming what
it does declare. After stage 5, addresses are module-qualified at every level
(`module.platform.module.storage.store`) and nothing downstream — the planner, the
executor, the state package — knows a module exists.

Task 10's integration suite found two gaps, both since fixed and both worth knowing the
shape of:

1. **A module call's own attribute that references an outer-scope resource recorded no
   dependency edge.** `network: ${net.id}` passed into a module call — the pattern
   nearly every module fixture in `PLAN.md` uses — evaluated to a value inside
   `internal/modules/inputs.go`'s `evaluateCall`, but nothing recorded a graph edge from
   the module's expanded resources to `net`; the same class hit the reverse direction
   too, where `Qualify` folds a resolved module OUTPUT reference into a literal before
   `bindAttribute`'s reference walk runs, so an edge to that output was lost the same
   way. Both fixed by recording edges before the fold. The failure mode is worth
   remembering because it is the worst available shape: `plan` showed a clean, correct
   plan and `apply` failed on it (`"network is still unknown after its dependencies
   were applied"`) — package-level tests could not see it because nothing at that level
   ran a real apply across a module boundary.
2. **`modules.lock` reading, comparing and writing was unwired.**
   `internal/modules/source`'s `Lockfile.Check`, `LoadLockfile` and `WriteLockfile`
   existed and were unit-tested, but nothing in `internal/cli` or `internal/compiler`
   called any of them. Now: the comparison is a pure read, wired into `Compile` so
   `validate` can report a moved tag without writing anything; the write is gated on a
   new `compiler.Options.RecordLocks`, set only by `apply` (the command already
   permitted to change the project directory), and happens once, with the complete
   resolved set, after a walk succeeds — never per-resolution, which is the shape
   `ae1e309` already fixed once for the file's atomicity.

Acceptance invariants 1, 2, 4 and 5 each have a test that fails against the unfixed code.
That phrasing is deliberate: invariant 4's test once passed 20/20 with its dependency edge
deleted, invariant 5's atomicity test caught a real TOCTOU only 2 times in 5, and gap 1
above is invariant 2 failing for exactly this reason — caught only once the integration
suite exercised a real apply across a module boundary, not a plan.

Absent until M6-M7: reading a saved plan back, `init`, `explain`, `graph`,
`discover`, `import`. Nothing half-implements one of those.

The standard that kept `--var-file` erroring rather than being silently ignored still
holds, and M4 showed why it is worth stating as a rule about TASK BOUNDARIES and not only
about flags: one task removed the flag from the unsupported list for every command while
wiring only `plan`, and for four tasks `apply --var-file` accepted the file and applied the
default instead. Remove a guard and add the capability it guards in the same change, per
command. Neither task's tests could see that window, because each covered its own command.

`PLAN.md` remains the product spec. The Phase 1 design spec and the M1-M4 implementation
plans are under `docs/superpowers/`.

`PLAN.md` is the authoritative spec. Read the relevant section before implementing a
feature; the sections below summarise the architecture but do not replace it. Section
numbers referenced here match `PLAN.md` headings.

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

Go 1.24 (pinned via `mise.toml`), Cobra, `gopkg.in/yaml.v3`. Those two are the **entire**
third-party budget so far; AWS SDK v2 arrives with Phase 3.

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

The CLI surface to implement (§37): `init`, `validate`, `plan <env>`, `apply <env>`,
`destroy <env>`, `state` / `state show <address>`, `refresh <env>`, `import`, `export`,
`discover`, `graph`, `explain <resource-type>`. Global options: `--var`, `--var-file`,
`--output`, `--auto-approve`, `--parallelism`, `--verbose`.

`--output` means two different things, deliberately. For `plan` it writes the plan
ARTIFACT: one JSON document, sensitive values in cleartext, because M6 reads it back to
apply it and needs the real values. For `validate`, `apply`, `refresh` and `destroy` it
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
pkg/                provider, schema, plan, resource   (the stable-ish interfaces)
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
