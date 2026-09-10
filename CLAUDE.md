# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> Project notes (source of truth): `Obsidian Vault/projects/labs/infra-tool.md`

## Current state

**M1 and M2 are merged to `main`** (tags `m1`, `m2`). `infra validate` and
`infra plan <environment>` both work end to end against the fake provider — compile,
refresh, diff, render. ~14,000 lines of Go across 18 packages.

Present: the value model with per-leaf provenance and sensitivity, addressing, diagnostics,
declarative resource schemas, the provider interface, a hand-editable file-backed fake
provider, versioned state with atomic writes and `O_EXCL` locking, compiler stages 1-2 and
6-8, a generic dependency graph, provider refresh, the planner, the plan renderer, and the
execution graph M3's executor will consume.

Absent until M3-M7: `apply`, `destroy`, the `refresh` *command* (the engine exists),
variables, environments, modules, reading a saved plan back, `explain`, `graph`, `discover`,
`import`. Nothing half-implements one of those; `--var-file` errors rather than being
silently ignored, which is the standard to hold.

`PLAN.md` remains the product spec. The Phase 1 design spec and the M1/M2 implementation
plans are under `docs/superpowers/`.

`PLAN.md` is the authoritative spec. Read the relevant section before implementing a
feature; the sections below summarise the architecture but do not replace it. Section
numbers referenced here match `PLAN.md` headings.

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
go build ./cmd/infra          # build the binary
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

## Architecture

Planned layout (§41):

```
cmd/infra/          CLI entrypoint
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
