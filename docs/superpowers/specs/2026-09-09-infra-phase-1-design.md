# infra — Phase 1 Core Engine Design

- **Date:** 2026-09-09
- **Status:** Approved design, not yet implemented
- **Scope:** All nineteen items of `PLAN.md` §49 (Phase 1), specified as one document with seven internal milestones
- **Supersedes:** nothing. `PLAN.md` remains the product specification; this document specifies the implementation of its Phase 1.

## 1. Context and scope

`PLAN.md` specifies a declarative infrastructure management tool (working name `infra`)
in the shape of "Ansible's readability + Terraform's state and planning, with better
environments, import, and defaults." It is a product specification: it fixes the
configuration language, the CLI surface, the provider interface, the invariants, and a
five-phase build order. It deliberately does not specify internal design.

This document specifies the internal design of **Phase 1: the core engine**, developed and
tested entirely against a fake provider. No AWS code is in scope.

At the time of writing the repository contains `PLAN.md`, `CLAUDE.md`, and this document.
There is no Go module and no version control.

### In scope

All of `PLAN.md` §49:

repository scaffolding · CLI · YAML parser · typed configuration model · variable system ·
environment system · module system · resource registry · provider interface · fake provider ·
state model · local state backend · state locking · dependency graph · planner ·
plan renderer · executor · lifecycle protection · comprehensive tests.

Plus, by explicit decision during design (§20 below records why):

- Value provenance carried in the value type from the first commit.
- Full CRUD reconciliation including provider-`Read` refresh and drift detection.
- Serializable, applyable, fingerprinted plan artifacts with staleness refusal.
- Production protections (§38 of `PLAN.md`) enforced by the engine.

### Out of scope

Discovery, import, configuration generation, and literal export (Phase 2). Any AWS
provider work (Phase 3). Remote state backends and state encryption (Phase 4). Secrets
integration beyond the `secret:` reference form, policy checks, CI integration,
create-before-destroy replacement, and provider throttling heuristics (Phase 5).

## 2. Goals

1. The reconciliation engine is **correct and deterministic**. `PLAN.md` §59 names this the
   primary engineering goal; every structural decision below is subordinate to it.
2. Five of the six critical invariants (§3) are **provable by automated test**, not merely
   intended.
3. The core contains no provider-specific logic, so Phase 3 adds AWS without modifying the
   compiler, planner, graph, or executor.
4. The full `PLAN.md` §48 MVP script runs end to end against the fake provider.

## 3. Acceptance invariants

From `PLAN.md` §47. Each has a named test in §18 of this document.

| # | Invariant | Phase |
|---|-----------|-------|
| 1 | In state, absent from configuration ⇒ plan proposes destroy, unless `prevent_destroy` or `retain` | 1 |
| 2 | Desired equals actual ⇒ plan contains zero operations | 1 |
| 3 | discover → import → generate → plan ⇒ no unexpected changes | 2 |
| 4 | A resource never executes before its dependencies | 1 |
| 5 | Two applies cannot mutate one environment concurrently; different environments can | 1 |
| 6 | Identical configuration, state and observations ⇒ equivalent plan | 1 |

Invariant 3 is unprovable in Phase 1 because import does not exist yet. It is listed so the
Phase 2 spec inherits it explicitly.

## 4. Architecture

Chosen approach: **staged pure compiler, upfront refresh, pure planner**. Considered and
rejected: a lazy evaluation graph (efficient, but evaluation order becomes implicit and
determinism becomes hard to prove) and provider-driven diffing (smaller core, but diff
semantics scatter across providers and the determinism guarantee stops living in one place).

```
files
  │
  ▼
Compile(files, environment, cliVars) ──► ResolvedConfig + Diagnostics   [pure]
  │
  ▼
Refresh(state, providers) ──► Observations                              [concurrent, read-only]
  │
  ▼
Plan(config, state, observations, opts) ──► Plan + Diagnostics          [pure]
  │
  ├──► Render(plan) ──► string                                          [pure]
  │
  ▼
Execute(plan, graph, providers, backend) ──► Result                     [concurrent, mutating]
```

Three of the five stages are pure functions. `Plan` in particular is a pure function of
exactly three inputs, which is what turns invariant 6 from an aspiration into a property
test, and what makes plan rendering golden-testable.

## 5. Core types

### 5.1 Value

The atom of the system. Resolved attribute values are never bare Go values and never
`map[string]any`.

```go
type Kind int // String, Int, Float, Bool, List, Map

type ValueSource string

const (
    SourceExplicit    ValueSource = "explicit"
    SourceDefault     ValueSource = "default"
    SourceEnvironment ValueSource = "environment"
    SourceVariable    ValueSource = "variable"
    SourceModule      ValueSource = "module"
    SourceComputed    ValueSource = "computed"
    SourceProvider    ValueSource = "provider"
)

type Value struct {
    Kind      Kind
    Known     bool        // false ⇒ Raw is nil and Expr is set
    Raw       any         // List holds []Value; Map holds map[string]Value
    Source    ValueSource
    Sensitive bool
    Expr      *Expr       // deferred expression, evaluated during apply
    Origin    Origin
}

type Origin struct {
    File   string
    Line   int
    Column int
    Module []string // module instantiation chain, outermost first
}
```

Four properties are load-bearing:

**Unknown values carry a `Kind`.** A reference to a not-yet-created resource is unknown but
its type is known from the target attribute's schema, so type checking still happens before
apply rather than during it.

**Sensitivity propagates.** Interpolating a sensitive value into a larger string yields a
sensitive result. This lives in the value rather than the renderer; otherwise secrets leak
through concatenation.

**Composites hold `Value`s recursively,** so provenance is per-leaf. A map with three keys
from a module default and one explicit override renders each correctly.

**Deferred expressions live inside the value.** The alternative — a `Deferred map[string]Expr`
beside the attributes — permits the two to disagree about whether an attribute is resolved.

### 5.2 Addressing

```go
type Address struct {
    Module []string // empty for root
    Name   string
}
```

The canonical address is the module path plus the logical name: `module.network.main`.
Logical names are unique within a module, and this form survives a resource changing type.

Type is stored alongside the address, never embedded in it: `aws.rds.database` cannot be
unambiguously parsed into type and name, because types themselves contain dots.

Plans and other human output render the readable `type.name` form. `infra state show`
accepts the canonical address, and also accepts `type.name` when it resolves unambiguously;
an ambiguous input is an error that lists the candidates.

Compile-time module flattening (§7) means addresses embed the module path permanently.
Restructuring modules therefore appears in state as resource moves. A `state mv` command is
deferred to a later phase; the workaround in Phase 1 is destroy and recreate, which is
acceptable while the only provider is fake.

### 5.3 Resolved configuration

```go
type ResolvedConfig struct {
    Project     string
    Environment string
    Resources   map[string]*ResolvedResource // keyed by Address.String()
}

type ResolvedResource struct {
    Address   Address
    Type      string
    Attrs     map[string]Value
    DependsOn []Address // explicit depends_on plus edges derived from references
    Lifecycle Lifecycle
    Origin    Origin
}

type Lifecycle struct {
    PreventDestroy bool
    Retain         bool
}
```

`DesiredResource` is what the executor hands a provider: a `ResolvedResource` whose
deferred expressions have all been evaluated, so every attribute is `Known`. The planner
works with `ResolvedResource` (unknowns permitted); providers only ever see
`DesiredResource` (unknowns impossible). The type separation makes "a provider was called
with an unresolved value" unrepresentable rather than merely a bug to test for.

```go
type DesiredResource struct {
    Address   Address
    Type      string
    Attrs     map[string]Value // every Value has Known == true
    Lifecycle Lifecycle
}
```

`ResolvedConfig` is the seam of the system. Everything above it (variables, environments,
modules, precedence) produces it; everything below it (registry, providers, state, graph,
planner, executor) consumes it and knows nothing about how it was produced.

## 6. Expressions and unknown values

The expression language is deliberately minimal, per `PLAN.md` §10: interpolation of
variables (`${project_name}`), references to resource attributes
(`${database.connection_string}`), and a small set of pure functions. It is not a
programming language and must not grow into one.

```go
type Expr struct {
    Op       ExprOp   // Literal, VarRef, ResourceRef, Concat, Call
    Literal  Value
    Ref      Reference
    Args     []*Expr
    Function string
    Origin   Origin
}
```

Phase 1 ships these functions and no others: `lower`, `upper`, `trim`, `join`, `replace`,
`default`. Each is pure, total, and side-effect free. Adding a function is a configuration
language change and follows the compatibility rule in `PLAN.md` §58.

**Evaluation is two-phase and shares one evaluator.** During compilation, everything that
can be evaluated is: literals, variable references, functions over known arguments. A
reference to another resource's attribute cannot be, so it produces
`Value{Known: false, Source: SourceComputed, Expr: <the expression>}` and simultaneously
records a dependency edge. During execution, once a dependency has been created and its real
attributes are known, the same evaluator finishes the expression.

Unknownness is contagious: any function or concatenation with an unknown argument yields an
unknown result, carrying the union of its arguments' sensitivity.

`secret: NAME` is a distinct declaration form rather than a function. It resolves from the
process environment in Phase 1, marks the resulting value `Sensitive`, and is the
integration point for external secret stores in Phase 5.

## 7. Compiler pipeline

Eight stages. Each is a pure function `func(in) (out, Diagnostics)` and is tested in
isolation. Composed as `Compile(files, environment, cliVars) (ResolvedConfig, Diagnostics)`.

| # | Stage | Responsibility |
|---|-------|----------------|
| 1 | Load | Read `infra.yml`, `variables.yml`, `environments/*.yml`, module files into `yaml.Node` trees preserving line and column |
| 2 | Decode | `yaml.Node` → typed unresolved declarations |
| 3 | Environment resolution | Walk the `extends` chain, detect cycles, produce an ordered scope stack |
| 4 | Variable resolution | Evaluate declarations, files, environment overrides and `--var`; validate against typed schemas |
| 5 | Module expansion | Recursively load sources, evaluate inputs in the caller's scope, instantiate resources under module-qualified addresses, collect outputs |
| 6 | Reference binding | Walk every `Expr`; fold known references; turn resource references into deferred unknowns and emit dependency edges; reject references to nonexistent resources or attributes |
| 7 | Schema binding and defaults | Resolve `Type` to a `ResourceDefinition`; check names, kinds and requiredness; run default resolvers for absent optionals; mark sensitive and computed attributes |
| 8 | Whole-graph validation | Dependency cycles, missing required infrastructure, invalid lifecycle configuration |

Stage 2 is the only stage permitted to touch `yaml.Node`. This is how `PLAN.md` §42's "do
not allow raw YAML maps to flow throughout the application" is enforced structurally rather
than by discipline.

### 7.1 Precedence and provenance are one mechanism

`PLAN.md` §7 fixes the precedence chain:

```
provider defaults → base configuration → module defaults → environment inheritance
→ environment variables → CLI overrides
```

Stages 3 and 4 build an ordered scope stack in exactly that order. A resolved value's
`Source` is simply a record of **which scope won**. There is one implementation, so what a
plan claims about a value's origin cannot drift away from the precedence rule that produced
it.

### 7.2 Module expansion flattens

After stage 5, addresses are module-qualified and nothing downstream knows modules exist.
This is a substantial simplification of the planner, state, graph and executor, paid for in
two places: error messages depend entirely on `Origin.Module` to say which module
instantiation a problem came from, and addresses are coupled to module structure (§5.2).

Module sources in Phase 1 are local relative paths only. Versioned and remote sources are
deferred.

Recursion is bounded at a depth of 32 instantiations, and a module may not instantiate
itself transitively; both produce diagnostics rather than a stack overflow.

### 7.3 Default resolvers are pure and narrow

```go
type DefaultContext struct {
    Environment string
    EnvironmentType string // "production" and similar, from environment config
    Region      string
    Account     string
    Project     string
    Type        string
}

type DefaultFunc func(DefaultContext) (any, bool)
```

A default resolver receives that context and nothing else. It may not read other resources'
attributes, which guarantees defaults never depend on unknown values and are therefore
always computable at plan time. This is what makes `PLAN.md` §13's environment-aware defaults
safe: they vary by environment, never by the state of the graph.

### 7.4 Diagnostics collect rather than fail fast

```go
type Severity int // Error, Warning

type Diagnostic struct {
    Severity Severity
    Summary  string // one line: what is wrong
    Detail   string // what was expected, and what was found
    Action   string // suggested fix, if one is known
    Origin   Origin
    Related  []Address
}
```

`infra validate` reports every problem in one pass. Stages continue past an error using a
poison value where they can, so a single typo does not mask the rest of the file. Rendering
follows `PLAN.md` §44: what is wrong, where, what was expected, what to do.

Cycle detection appears three times — environment `extends`, the module graph, the resource
graph — and is one shared algorithm parameterized by an edge function. Its diagnostic names
the full cycle, not just one participant.

## 8. Schema and provider layer

### 8.1 Resource definitions are plain data

```go
type ResourceDefinition struct {
    Type         string
    Description  string
    Attributes   map[string]Attribute
    Requirements []Requirement
    Capabilities Capabilities
    ImportID     ImportSpec
}

// ImportSpec describes the provider ID form for a resource type. Phase 1 uses
// Description only, to render `infra explain`. Parse is declared now so the field does
// not change shape in Phase 2, and may be nil until the importer exists.
type ImportSpec struct {
    Description string // e.g. "the DB instance identifier, or its ARN"
    Parse       func(id string) (map[string]Value, error)
}

type Attribute struct {
    Kind        Kind
    Required    bool
    Computed    bool   // provider sets it; configuration may not
    Sensitive   bool
    ForceNew    bool   // a change replaces rather than updates
    Default     DefaultFunc
    Description string
    Validate    func(Value) error
}

type Requirement struct {
    Name        string   // "cluster", "network", "execution_role"
    Types       []string // resource types that satisfy it
    Optional    bool
    Description string
}

type Capabilities struct {
    Create, Read, Update, Delete, Import bool
}
```

Rejected alternatives: struct tags plus reflection (environment-aware default resolvers do
not fit in tags, and reflection bugs surface at runtime rather than in review) and
schema-as-data with code generation (a generator and build step to maintain before there is
any evidence the boilerplate hurts).

`ForceNew` living in the schema is what lets the planner decide replace-versus-update without
consulting the provider, which is required by the chosen architecture.

`Requirement` powers `PLAN.md` §17 missing-dependency detection. A requirement is satisfied
either by a reference in configuration to a resource of a listed type, or by an existing
resource of a listed type in state. An unsatisfied non-optional requirement is a plan-time
diagnostic naming what is missing — never an apply-time provider error.

### 8.2 Registry

```go
type Registry interface {
    Definition(resourceType string) (*ResourceDefinition, bool)
    Provider(resourceType string) (Provider, bool)
    Types() []string
}
```

Providers register their definitions at construction. `infra explain` is a rendering function
over the registry, so it cannot go stale relative to the schemas it documents.

### 8.3 Provider interface

`PLAN.md` §31 verbatim, plus schema exposure:

```go
type Provider interface {
    Name() string
    Definitions() []*ResourceDefinition

    Read(ctx context.Context, resource *ResourceState) (*ResourceState, error)
    Create(ctx context.Context, resource *DesiredResource) (*ResourceState, error)
    Update(ctx context.Context, current *ResourceState, desired *DesiredResource) (*ResourceState, error)
    Delete(ctx context.Context, resource *ResourceState) error

    Discover(ctx context.Context, req DiscoverRequest) ([]DiscoveredResource, error) // Phase 2
    Import(ctx context.Context, resourceType, id string) (*ResourceState, error)     // Phase 2
    ClassifyError(err error) Retryability
}
```

`Discover` and `Import` are declared in Phase 1 so the interface does not churn in Phase 2;
the fake provider may return `ErrNotImplemented` for them until then.

`Read` returning a nil state with no error means the resource no longer exists.

Providers are in-process Go implementations. `PLAN.md` §2 anticipates external provider
processes eventually; nothing in this interface prevents that later, and nothing in Phase 1
should build for it.

### 8.4 Fake provider

`providers/test/`. Not merely a test double — `PLAN.md` §48's MVP script requires that a
human can "manually mutate fake infrastructure," so the fake cloud is a JSON file at
`.infra/fake-cloud.json`, hand-editable and equally editable by tests.

Resource types: `test.network`, `test.database`, `test.application` — enough to exercise a
three-level dependency chain, a `ForceNew` attribute, a computed attribute, a sensitive
attribute, and a `Requirement`.

Controls, configured in the fake cloud file rather than in Go, so integration tests drive
them the same way a human would:

- failure injection: fail the nth operation of a given kind on a given address
- latency: per-operation delay, for concurrency and parallelism tests
- error classification: mark an injected failure retryable or not

## 9. State

```go
type State struct {
    Version     int    // schema version; migrations run on load
    Serial      uint64 // increments on every write
    Project     string
    Environment string
    Resources   map[string]*ResourceState // keyed by Address.String()
    UpdatedAt   time.Time
}

type ResourceState struct {
    Address      Address
    Type         string
    Provider     string
    ProviderID   string
    Attributes   map[string]Value // last known; Source is SourceProvider
    Dependencies []Address
    Lifecycle    Lifecycle
    CreatedAt    time.Time
    UpdatedAt    time.Time
}
```

`Version` plus a registry of migration functions applied at load satisfies `PLAN.md` §21's
versioning requirement from the first release. Retrofitting migrations after state files
exist in the wild is far more expensive than carrying the field from the start.

### 9.1 Backend

`PLAN.md` §21 verbatim:

```go
type StateBackend interface {
    Get(ctx context.Context, environment string) (*State, error)
    Put(ctx context.Context, environment string, state *State) error
    Lock(ctx context.Context, environment string) (Lock, error)
    Unlock(ctx context.Context, environment string) error
}
```

Whole-state get and put is a file-shaped interface, which is why the local backend is a JSON
file rather than SQLite: the S3 backend in Phase 4 has the same shape, and designing around
database semantics that do not generalize would be wasted work.

Local backend: `.infra/state/<environment>.json`, written to a temporary file in the same
directory and renamed into place, so a crash mid-write cannot corrupt state. Mode `0600`.

### 9.2 Locking

A lock is a `<environment>.lock` file created with `O_CREAT|O_EXCL`, containing the holder's
pid, hostname, user, operation and timestamp. A conflict therefore reports *who* holds the
lock, not merely that one exists.

Locks do not expire. A stale lock requires explicit human action via `infra state unlock
<environment>`, because a timeout that guesses wrong is precisely how two applies end up
running simultaneously — the failure mode invariant 5 exists to prevent.

Scope: `apply`, `destroy`, and `refresh` take the lock for their whole run. `plan` and
`validate` do not lock, because they do not write.

### 9.3 Sensitive values at rest

Stored plaintext, file mode `0600`, documented as such in the CLI's own output on first
`init`. Encryption is Phase 4 (`PLAN.md` §52). Encrypting state now while values still flow
through plan artifacts, logs and terminal output would buy the appearance of protection
rather than protection.

The same decision applies to plan artifacts (§12.2).

## 10. Refresh

Before any diffing, every resource in state is read from its provider concurrently, bounded
by the same per-provider semaphore the executor uses (§15).

```go
type Observation struct {
    Address Address
    State   *ResourceState // nil when the resource no longer exists
    Err     error
}

type Observations map[string]Observation
```

**`plan` never writes state.** It uses observations in memory and discards them. Only
`refresh` persists. This keeps `plan` safe to run against a locked environment, in CI, or
repeatedly, and keeps the planner a pure function of its inputs. The cost is that a drifted
resource stays drifted in the state file until someone runs `refresh`, and two consecutive
`plan` runs each re-read the provider. Surprising writes from a read-only verb are the worse
trade.

A read error is a diagnostic that fails planning for that resource rather than a silent
absence; treating a transient failure as deletion would propose destroying live
infrastructure.

## 11. Planner

```go
func Plan(cfg ResolvedConfig, st *State, obs Observations, opts Options) (*Plan, Diagnostics)
```

Pure. Per address, the operation is decided by the triple of (in config, in state, observed):

| In config | In state | Observed | Operation |
|-----------|----------|----------|-----------|
| yes | no | — | Create |
| yes | yes | present, no differences | NoOp |
| yes | yes | present, differences in updatable attributes | Update |
| yes | yes | present, differences in `ForceNew` attributes | Replace |
| yes | yes | absent | Create (recreate; something deleted it outside) |
| no | yes | present | Destroy, or Forget under `retain`, or diagnostic under `prevent_destroy` |
| no | yes | absent | Forget (already gone; drop from state) |

Rules that are decisions rather than mechanics:

**An unknown desired value cannot be proven unchanged.** It therefore contributes an Update
and renders as `(known after apply)`. Treating unknown as unchanged produces plans that
under-report, and the discrepancy is invisible until apply.

**Computed attributes absent from configuration never drive a diff.** They are outputs.

**Any changed `ForceNew` attribute promotes Update to Replace,** and the driving attribute is
recorded in `Operation.Reasons` so the plan can say which one forced it.

**`prevent_destroy` is a plan-time error, not an apply-time refusal.** The user learns before
approving, not after.

**`retain` removes the resource from state without calling the provider.** The plan renders
it distinctly from a destroy.

## 12. Plan artifact

### 12.1 Structure

```go
type Plan struct {
    Version     int
    CreatedAt   time.Time
    Project     string
    Environment string
    ConfigHash  string // canonical hash of ResolvedConfig
    StateSerial uint64
    StateHash   string
    Operations  []Operation // sorted by canonical address
    Diagnostics []Diagnostic
}

type Operation struct {
    Address       Address
    Type          string
    Kind          OpKind // NoOp, Create, Update, Replace, Destroy, Forget
    Before        map[string]Value
    After         map[string]Value
    Reasons       []ChangeReason
}
```

`Before` is nil for Create; `After` is nil for Destroy and Forget; both are populated for
NoOp, Update and Replace. `After` may contain unknown values.

```go
type ChangeReason struct {
    Attribute string
    ForceNew  bool
    Note      string // "known after apply", "default", and similar
}
```

Operations are sorted by canonical address, not execution order. Execution order belongs to
the graph (§14). A stable sort is what makes invariant 6 testable by comparing serialized
plans byte for byte.

**Determinism is defined over the canonical form, not the saved artifact.** `CreatedAt`
records when a plan was produced, which is not a property of its inputs, so two identical
plans made a second apart differ in it. `Plan.Canonical()` therefore emits the plan with
`CreatedAt` omitted, and that is what invariant 6 compares and what any plan fingerprint is
taken over. `MarshalJSON` keeps `CreatedAt` for the artifact a user saves and reads.
Without this split the invariant as stated in §18 is unsatisfiable.

`ConfigHash` is computed over the canonicalized `ResolvedConfig` — attribute values,
provenance and lifecycle included, `Origin` excluded, since moving a resource between lines
of a file is not a change in desired state.

### 12.2 Save and apply

`infra plan <env> --output plan.json` writes the artifact, mode `0600` (it contains sensitive
values, which apply needs).

`infra apply <env> plan.json` recompiles the configuration, recomputes `ConfigHash`, reloads
state and compares `StateSerial` and `StateHash`. Any mismatch is refused with a diagnostic
naming which of the three changed, unless `--allow-stale` is passed. It re-refreshes
providers before executing, because provider reality can move even when configuration and
state have not.

A plan whose `Diagnostics` contain an error is never applyable.

### 12.3 Rendering

`Render(plan, opts) string` — pure, golden-tested. Per `PLAN.md` §19 and §12:

- `+` create, `~` update, `-` destroy, `-/+` replace, and a distinct marker for forget
- values sourced from defaults annotated `[default]`
- sensitive values rendered `<sensitive>` regardless of operation
- unknown values rendered `(known after apply)`
- replacements state which attribute forced them
- destructive operations highlighted, with dependent counts, per §20
- summary counts: created, updated, replaced, destroyed, forgotten

## 13. Production protections

`PLAN.md` §38, enforced by the engine:

```yaml
environment:
  type: production
  protections:
    require_approval: true
    prevent_destroy: true
```

- `require_approval: true` causes `--auto-approve` to be rejected for that environment.
- `prevent_destroy: true` at environment level makes any Destroy or Replace operation in a
  plan an error; it is a stronger form of the per-resource lifecycle flag.
- Destructive operations in any environment require typed confirmation, not a bare `y`, when
  the environment is `type: production`.

These are checks in the planner and executor, not conventions in documentation.

## 14. Dependency graph and ordering

Nodes are plan operations, not resources; ordering differs by operation kind and conflating
them causes apply-time failures:

- `create(A)` before `create(B)` when B depends on A
- `destroy(B)` before `destroy(A)` when B depends on A — reverse order
- `replace(A)` is `destroy(A)` then `create(A)`, with dependents ordered around both

Dependency edges come from: references bound in compiler stage 6; explicit `depends_on`;
requirements satisfied by a reference; and, for destroys, the `Dependencies` recorded in
state — which matters because a resource being destroyed may no longer be in configuration
at all, so its edges cannot come from the compiler.

Phase 1 implements **destroy-then-create replacement only**. Create-before-destroy is
deferred to Phase 5, where `PLAN.md` §53 already files replacement strategies.

`infra graph <env>` renders the tree form from `PLAN.md` §40. DOT output is deferred.

## 15. Executor

A worker pool draining a ready queue of operations whose predecessors have completed.

**Concurrency** is bounded twice: by `--parallelism` globally, and by a per-provider
semaphore, so one provider's rate limits cannot be exhausted by an unrelated wide graph
(`PLAN.md` §34).

**State is persisted after every operation,** not at the end, with `Serial` incrementing each
time, under the lock held for the whole apply. A crash then leaves state that accurately
describes reality. Batching writes until completion guarantees the opposite precisely when
accuracy matters most.

**A failure stops its branch, not the world.** Dependents of a failed operation are skipped;
independent branches run to completion; the summary reports applied, failed and skipped
separately. Aborting the whole graph strands work that would have succeeded and leaves state
that is harder to reason about.

**Deferred expressions resolve here.** When an operation completes, its dependents' unknown
values are evaluated against the new resource's real attributes using the same evaluator the
compiler used (§6).

**Retries** (`PLAN.md` §35): the provider classifies each error via `ClassifyError` as
`SafeToRetry`, `ConditionallyRetryable` or `NotSafeToRetry`; the core owns exponential
backoff with jitter and a retry ceiling. `Create` is never retried on an ambiguous failure —
a retried create is how duplicate infrastructure appears. `Delete` is retried only when the
provider classifies the error as safe.

**Signals.** `SIGINT` finishes the in-flight operation, persists state, releases the lock and
exits non-zero. A second `SIGINT` exits immediately, leaving the lock — which is then a
stale lock requiring `state unlock`, and is reported as such.

## 16. CLI

`cmd/infra`, Cobra.

Phase 1 ships: `init`, `validate`, `plan`, `apply`, `destroy`, `refresh`, `state list`,
`state show <address>`, `state unlock <env>`, `graph`, `explain <resource-type>`.

`discover`, `import` and `export` arrive with Phase 2 and are **not** present as stubs, since
a stub promises a capability that does not exist.

Global flags per `PLAN.md` §37: `--var`, `--var-file`, `--output`, `--auto-approve`,
`--parallelism`, `--verbose`.

Output contract: human-readable output on stdout, structured logs on stderr, so piping
works. `--verbose` adds provider-level detail per `PLAN.md` §45. `--output json` emits
machine-readable results for `plan`, `state` and `explain`.

Exit codes: `0` success with no changes, `1` error, `2` success with changes present. The
third makes CI gating possible later at essentially no cost now.

## 17. Repository layout

Per `PLAN.md` §41:

```
cmd/infra/
internal/  config compiler expressions environments modules variables
           state planner graph executor lifecycle secrets cli
pkg/       provider schema plan resource
providers/ test/
tests/integration/
docs/superpowers/specs/
```

`pkg/` holds the types that providers and, later, external consumers depend on. `internal/`
holds the engine. The dependency rule is one-directional: `providers/` may import `pkg/`,
never `internal/`.

## 18. Testing

Test-driven throughout; no feature lands without tests.

**Unit tests** cover each compiler stage in isolation, value and provenance semantics,
expression evaluation including unknown propagation and sensitivity propagation, schema
validation, default resolution, graph construction, and planner decisions across the full
table in §11.

**Property tests** for the invariants:

| Invariant | Test |
|-----------|------|
| 1 | Removing a resource from configuration always yields Destroy, or Forget under `retain`, or an error under `prevent_destroy` |
| 2 | For any generated configuration, apply then plan yields zero operations |
| 4 | The fake provider records call order; no resource is ever created before a dependency, nor destroyed after one |
| 6 | Identical inputs produce byte-identical output from `Plan.Canonical()` across repeated runs and across map iteration orders (see §12.1: `CreatedAt` is excluded, being a record of when the plan was made rather than a property of its inputs) |

**Golden files** for rendered plans and serialized plans.

**Integration tests** (`tests/integration/`) drive the real CLI binary in temporary
directories against the fake provider. The headline case is `PLAN.md` §48's MVP script as a
single executable test: init, validate, plan, apply, re-plan clean, mutate
`.infra/fake-cloud.json` by hand, observe drift, remove a resource from YAML, observe the
proposed destroy, apply.

**Concurrency and failure tests:** two applies racing for one environment lock (invariant 5);
concurrent applies to different environments succeeding; a mid-graph injected failure
asserting that state afterwards accurately reflects what was applied; retry classification
honored for each of the three categories.

CI runs `go build ./...`, `go vet ./...`, `gofmt -l .`, and `go test ./...` with no cloud
credentials of any kind.

## 19. Milestones

| M | Content | Demonstrates |
|---|---------|--------------|
| M1 | Scaffolding, core types, registry, fake provider, local state backend, locking, compiler stages 1 and 2 | `infra validate` catches malformed YAML and unregistered resource types; state round-trips; lock contention detected |
| M2 | Compiler stages 6, 7, 8 for flat configuration; graph; planner; renderer | `infra plan` runs; plan serializable; invariant 6 testable |
| M3 | Executor, `apply`, `destroy`, `refresh`, incremental state writes, lifecycle | Reconcile loop closes; invariants 1, 2, 4, 5 |
| M4 | Variables, typed variable schemas, environments, `extends`, stages 3 and 4 | `PLAN.md` §7 precedence chain; provenance gains four sources |
| M5 | Modules, stage 5 | Full configuration surface |
| M6 | Plan artifacts, fingerprinting, staleness refusal, production protections | `PLAN.md` §19, §20, §38 |
| M7 | `explain`, `graph`, `state show`, full invariant suite, §48 MVP script green | Phase 1 complete |

The plan type is serializable from M2 so determinism is golden-tested early; only the
apply-from-file flow waits for M6.

## 20. Decisions and rationale

| Decision | Rationale | Alternative rejected |
|----------|-----------|----------------------|
| All of Phase 1 in one spec | Chosen deliberately after the alternative was presented; the seam at `ResolvedConfig` still exists and could be used to split the implementation plan if it proves unwieldy | Two specs split at the resolved-config seam |
| Provenance in the value type | Retrofitting touches compiler, planner, renderer and every default resolver simultaneously; a side-table can silently fail to be populated by new code paths | Parallel side-table; deferring entirely |
| Declarative schema values | Runtime-inspectable, so `explain`, validation and defaults read one source; explicit over magic per §58 | Struct tags and reflection; schema-as-data with codegen |
| Full plan artifact flow | Chosen deliberately; completes §19–20 now | Serializable but not applyable, deferring staleness to Phase 5 |
| Staged pure compiler, pure planner | Makes invariants structurally testable; matches the interfaces `PLAN.md` already specifies | Lazy evaluation graph; provider-driven diff |
| Modules flatten at compile time | Planner, state, graph and executor never learn about modules | Runtime module scopes |
| `plan` never writes state | A read-only verb that writes is surprising and unsafe to run concurrently | Refresh-and-persist during plan |
| File-backed fake cloud | §48 requires a human to mutate fake infrastructure by hand | In-memory fake |
| Plaintext state and plan artifacts at `0600` | Encryption is Phase 4; partial encryption while values flow through logs and artifacts buys appearance, not safety | Encrypt in Phase 1 |
| Destroy-then-create replacement only | Create-before-destroy is fiddly and §53 files it under Phase 5 | Both strategies now |
| Locks never expire | A timeout that guesses wrong violates invariant 5 | Lease with TTL |

## 21. Open questions

None blocking implementation. Two to settle before Phase 2:

1. Whether `state mv` is needed before modules are restructured in anger, given compile-time
   flattening couples addresses to module structure (§5.2).
2. Whether `ImportSpec` as declared in §8.1 is sufficient for the Phase 2 importer, which
   will be the first real consumer of it.
