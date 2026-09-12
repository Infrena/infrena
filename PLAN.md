# Infrastructure Management Tool

## Product & Implementation Specification

### Name

The product is named **Infrata** (*infra* + *strata*: layers of infrastructure), with the GitHub organization `github.com/infrata` and the domain `infrata.dev`. The command is `infrata`, installed with `go install github.com/infrata/infrata/cmd/infrata@latest`.

The Go module is `github.com/infrata/infrata` and the binary is `cmd/infrata`; the rename landed 2026-09-11, between M4 and M5.

---

# 1. Product Vision

Build a modern declarative infrastructure management tool combining:

* The readability and simplicity of Ansible YAML.
* Terraform-style declarative infrastructure.
* Terraform-style state management.
* Terraform-style planning and reconciliation.
* First-class environments.
* First-class modules.
* Sensible provider defaults.
* Infrastructure discovery and import.
* Automatic detection of missing infrastructure dependencies.
* Variable files.
* Safe production workflows.
* Fast concurrent execution.
* A clean provider/plugin architecture.

The product should feel significantly simpler than Terraform while retaining the important characteristics that make Terraform useful.

The central philosophy is:

> **Configuration describes the desired infrastructure. State records what is managed. The planner determines the difference. The executor reconciles reality with the desired state.**

Configuration is authoritative.

If a managed resource is removed from configuration, the plan should normally destroy that resource.

---

# 2. Recommended Implementation Language

Use **Go**.

Reasons:

* Excellent fit for CLI applications.
* Excellent concurrency primitives.
* Simple deployment as a single binary.
* Strong AWS SDK.
* Mature YAML libraries.
* Strong JSON support.
* Excellent testing ecosystem.
* Easy cross-compilation.
* Easy integration with external provider processes if needed later.
* Familiar language for infrastructure engineers.
* Easier for Claude Code to generate, test, refactor, and maintain than a more unusual infrastructure stack.

Recommended baseline:

* Go 1.24+
* Cobra for CLI
* `gopkg.in/yaml.v3` or equivalent maintained YAML library
* AWS SDK for Go v2
* SQLite for local state/cache where appropriate
* JSON Schema or equivalent for structured validation
* Standard Go testing
* Go modules

Do not over-engineer the initial implementation.

---

# 3. Core Design Principles

## 3.1 Declarative

Users describe what should exist.

Example:

```yaml
resources:

  database:
    type: aws.rds
    engine: postgres

  application:
    type: aws.ecs.service
    image: myapp:latest
```

The user does not describe the sequence of API calls required to create those resources.

---

## 3.2 Configuration Is Authoritative

If a resource exists in configuration:

> It should exist.

If a managed resource is removed from configuration:

> It should be proposed for destruction.

Example:

```yaml
resources:
  database:
    type: aws.rds
```

Removing `database` produces:

```text
- aws.rds.database

    destroy
```

This behavior must be tested extensively.

---

## 3.3 Plan Before Apply

Every mutating operation should go through a plan.

Normal workflow:

```text
configuration
      ↓
compile
      ↓
validate
      ↓
discover/read current state
      ↓
calculate differences
      ↓
dependency graph
      ↓
execution plan
      ↓
user approval
      ↓
apply
```

Do not allow the normal `apply` implementation to bypass planning.

---

## 3.4 State

State is required to associate:

```text
logical resource
      ↓
resource type
      ↓
provider
      ↓
provider-specific ID
      ↓
last known attributes
```

Example:

```text
production.database
    type: aws.rds
    provider_id: arn:aws:rds:...
```

State must support:

* Create
* Read/refresh
* Update
* Delete
* Import
* Drift detection
* Dependency tracking
* State locking
* State versioning/migrations

---

# 4. Configuration Structure

A project should look approximately like:

```text
infra/
├── infra.yml
├── variables.yml
├── environments/
│   ├── dev.yml
│   ├── staging.yml
│   └── production.yml
│
├── modules/
│   ├── application/
│   │   ├── module.yml
│   │   └── resources.yml
│   │
│   ├── database/
│   │   └── module.yml
│   │
│   └── networking/
│       └── module.yml
│
└── discovered/        # written by `infra import --generate`; LOADED like environments/
```

Exact structure may evolve during implementation, but environments and modules must be
first-class concepts.

## 4.1 Conventional directories, globbed

Three directories are read automatically, in the spirit of Ansible's layout — a project is
organised by putting files where they belong, not by listing them somewhere:

| Directory | Holds | Notes |
|---|---|---|
| `resources/**` | resource declarations | `infra.yml`'s own `resources:` still works and is equivalent |
| `vars/**` | variable values | the directory form of `variables.yml`; both are base configuration |
| `modules/**` | modules | a directory containing `module.yml`, already discovered in M5 |
| `discovered/**` | generated configuration | written by `infra import --generate` (§27.1) |

A resource directory may hold its own scoped material:

```text
resources/
  database/
    database.yml         the resources
    vars/                variables visible ONLY to resources in this directory
    templates/           reserved; see below
```

`resources/<dir>/vars/**` is scoped: those values are visible to the resources declared in that
directory and nowhere else. A value defined both there and in the project-wide `vars/` resolves
to the directory's, per §7 — the more specific statement about the same thing wins.

**A name defined twice at the SAME level is an error naming both files**, never last-one-wins.
Two files silently becoming one is the failure this language refuses everywhere else, and a
globbed directory makes it easy to do by accident.

### How `vars/` files name their environment

At the TOP LEVEL of `vars/`, the filename does the work:

| File | Applies to |
|---|---|
| `vars/default.yml` | every environment |
| `vars/production.yml` | `production` only |
| `vars/<env>.yml` | that environment only |

An environment file overrides `default.yml` per VALUE, not per file. If `default.yml` says
`size: 50` and `production.yml` sets no `size`, production gets 50; if `production.yml` sets
`size: 100`, production gets 100 and every other environment still gets 50. A file that names an
environment is a set of differences, not a replacement.

Deeper files cannot lean on a filename, so they carry the environment inside:

```yaml
size: 40              # the default, for every environment
region: eu-west-1

production:
  size: 100           # overrides the default above, for production only

dev:
  size: 10
```

A bare key is a default; a key naming an environment is a block of overrides for it.

**A top-level key is an environment block only if it matches a DECLARED environment.** Anything
else is a variable, whatever shape its value has — a variable whose value is a map stays a
variable. And a variable that collides with an environment name is an ERROR naming both, never a
silent reinterpretation: the alternative is that adding an environment months later changes what
an existing file means, without touching it.

### `templates/` is reserved, not implemented

It will hold text blobs rendered into attributes — IAM policy documents, lambda sources, unit
files, anything a provider takes as a string. That needs a template language, and choosing one
is a decision in its own right: `${}` interpolation is deliberately not a programming language
(§10), and a template engine is.

The directory is named now so the layout does not change when the engine arrives, and so the
choice is made against a stated purpose rather than in the abstract. Nothing reads it yet, and
a `templates/` directory present today is not an error.

---

# 5. Basic YAML Syntax

Example:

```yaml
project: myapp

resources:

  network:
    type: aws.vpc
    cidr: 10.20.0.0/16

  database:
    type: aws.rds
    engine: postgres

  application:
    type: aws.ecs.service
    image: myapp:latest
    replicas: 2
```

Resource names are logical names controlled by the user.

The provider resource type is specified by:

```yaml
type: aws.rds
```

---

# 6. Environments

Environments are first-class.

Example:

```yaml
environments:

  dev:
    variables:
      replicas: 1

  staging:
    variables:
      replicas: 2

  production:
    variables:
      replicas: 10
```

Commands:

```bash
infra plan dev
infra apply dev

infra plan staging
infra apply staging

infra plan production
infra apply production
```

Each environment must have independent state.

Never require users to manually manipulate Terraform-style workspaces.

---

# 7. Environment Inheritance

Support:

```yaml
environments:

  default:
    replicas: 1
    instance_class: db.t4g.small

  dev:
    extends: default

  staging:
    extends: default
    replicas: 2

  production:
    extends: default
    replicas: 10
    instance_class: db.t4g.large
```

Inheritance must be deterministic.

Define and document precedence.

Recommended precedence:

```text
provider defaults
        ↓
base configuration          variables.yml and vars/**
        ↓
directory-scoped variables  resources/<dir>/vars/**
        ↓
module defaults
        ↓
environment inheritance
        ↓
environment variables
        ↓
CLI overrides               --var, --var-file
```

Explicit user configuration always overrides an implicit default.

**More specific file scope wins, and an environment wins over every file.** Those are two
different axes and conflating them is the mistake to avoid. A directory is how the project is
ORGANISED; an environment is where it is DEPLOYED. A `resources/db/vars/` value therefore beats
a project-wide one, because it is the more specific statement about the same thing — but an
environment beats both, or `production` could no longer tune a value the code happened to set
locally, and environments being first-class (§6) would mean nothing.

`--var` is above everything, always. It is the operator saying what they want right now, and it
is the one rung that cannot be outranked by a file someone else wrote.

---

# 8. Variable Files

Support simple variable files.

Example:

```yaml
# variables.yml

project_name: myapp
region: us-east-1
domain: example.com
```

Environment:

```yaml
# environments/production.yml

replicas: 10
```

Configuration:

```yaml
resources:

  application:
    type: aws.ecs.service
    name: ${project_name}
    replicas: ${replicas}
```

Commands:

```bash
infra plan production
```

should automatically load the appropriate project and environment variables.

Also support:

```bash
infra plan production --var replicas=20
```

CLI values override variable files.

---

# 9. Typed Variables

Variables should support optional schemas.

Example:

```yaml
variables:

  replicas:
    type: integer
    default: 2
    min: 1
    max: 100
```

Support at minimum:

* string
* integer
* float
* boolean
* list
* map

Validation must happen during `infra validate` and before planning.

Do not build a general-purpose programming language into variable expressions.

---

# 10. Expressions

Support simple interpolation:

```yaml
name: ${project_name}-${environment}
```

Resource references:

```yaml
database_url: ${database.connection_string}
```

A small set of pure helper functions may eventually be supported:

```yaml
name: ${lower(project_name)}-${environment}
```

Do not initially build a Terraform/HCL-like programming language.

Expressions should remain intentionally constrained.

---

# 11. Modules

Modules are reusable infrastructure components. Using one is two steps, and the
separation is deliberate: **loading** makes a module available under a name, and
**instantiating** calls it like a resource.

## 11.1 Loading

`modules:` is a LIST of sources. It carries no inputs — loading a module says only
where it comes from and what to call it.

```yaml
modules:
  - ./modules/networking
  - https://github.com/acme/infra-app-stack:v1.2.0
  - git@github.com:acme/infra-database:9f3c1ab

  - name: app_stack_v2
    source: https://github.com/other/infra-app-stack:v2.0.0
```

Each entry is either a **scalar** — the source, with the name derived from it — or a
**mapping** of `name` and `source`. The two forms mean the same thing; the mapping
exists so a name can be overridden, which is the only way to load two different
modules that would otherwise derive the same name.

A derived name is the last path segment with any `.git` suffix and version suffix
removed, normalised to a valid identifier: `./modules/networking` → `networking`,
`https://github.com/acme/infra-app-stack:v1.2.0` → `infra_app_stack`.

Two entries deriving the same name is an ERROR naming both origins and suggesting
`name:` on one of them. It is never resolved by order.

**Sources** are a filesystem path (absolute, or relative to the file that declares
it), or a git remote — `https://`, `git@`, or `ssh://` — with a **required**
`:tag-or-hash` suffix. An unpinned remote is an error: an unpinned module means the
same configuration plans differently on different days, which breaks invariant 6.

**Discovery.** Any directory beneath the project root that contains a `module.yml`
is loaded automatically under its directory name, with no `modules:` entry. An
explicit entry naming the same module wins over the discovered one, per §7's rule
that explicit configuration beats an implicit default.

Discovery populates the ROOT level only. Inside a module, only that module's own
`modules:` entries are in scope — a module never resolves a name against directories
that happen to lie around the project consuming it, because a module that did would
work in one project and fail in the next.

## 11.2 Instantiating

A loaded module is called by a resource whose type is `module.<name>`:

```yaml
resources:

  prod:
    type: module.app_stack
    application_name: storefront
    image: acme/web:1.4
    replicas: 3

  staging:
    type: module.app_stack_v2
    application_name: storefront
    image: acme/web:edge

  cdn:
    type: fake_cdn
    origin: ${prod.endpoint}
```

The `module.` prefix is what distinguishes a module call from a provider resource
type, so the two namespaces can never collide and a reader never has to consult
`modules:` to know which one a type names.

**A module exposes its outputs, not its resources.** `${prod.endpoint}` reads an
output. There is no spelling that reaches inside: `${module.prod.database.id}` is
refused, and the `module.` segment belongs to addresses — what `state show` and the
plan print — never to a reference a user writes.

Everything a resource can do, a module call can do: its attributes are the module's
inputs and carry provenance and sensitivity like any other attribute, `depends_on`
and `lifecycle` apply, and its outputs are read as `${prod.endpoint}` — the same
spelling as a provider resource's attributes. A module may be instantiated any number
of times; each instantiation is independent.

Resources inside an instance are addressed with a `module.` segment per level, which
is what `pkg/address` has produced since M1: `module.prod.database`,
`module.prod.module.network.vpc`. Modules nest.

The `module.` marker is redundant with the type's prefix and is kept anyway, because
addresses and references would otherwise share a spelling. An instance `prod` both
CONTAINS resources and EXPOSES outputs, so `prod.endpoint` (a reference to an output)
and `prod.database` (an address of a contained resource) would be the same shape under
two different grammars. `state show module.prod.database` cannot be misread as
`${prod.endpoint}`.

### A resource's address includes its module path

A resource declared inside a module is addressed `module.<instantiation>.<name>`, and
state is keyed by that address. Moving a resource from one module to another — or renaming
the resource that instantiates the module — therefore RENAMES it, and a rename is a
destroy followed by a create, not a move.

There is no `infra state mv` yet. Before restructuring modules that manage a resource
holding data, run `infra plan <env>` and read it: a destroy you did not intend appears
there.

## 11.3 The module file

A module is a directory containing `module.yml`. It declares `inputs:`, `resources:`,
`outputs:` and may itself declare `modules:`. It may NOT declare `project:`,
`environments:` or `variables:` — a module does not own environments, and the values
it sees are its inputs plus the ambient `environment`, `region` and `account`.

```yaml
inputs:

  application_name:
    type: string

  image:
    type: string

  replicas:
    type: integer
    default: 1

resources:

  service:
    type: fake_service
    name: ${application_name}
    image: ${image}
    count: ${replicas}

outputs:

  endpoint:
    value: ${service.endpoint}
```

`inputs:` is spelled exactly as §9's `variables:` — the same `type`, `default` and
bounds. An input with no `default` that the caller does not supply is an error. A
name inside a module resolves to that module's own input, never to a project variable
of the same name.

Modules support inputs, defaults, resources, outputs, nested modules and dependencies.

---

# 12. Sensible Defaults

This is a core product feature.

Resources should not require users to specify every possible provider attribute.

Example:

```yaml
database:
  type: aws.rds
  engine: postgres
```

The provider may choose sensible defaults for:

* Instance class
* Storage
* Backup retention
* Minor settings
* Health checks
* Retry behavior
* Appropriate AWS defaults

Defaults must be visible in plans.

Example:

```text
+ aws.rds.database

    engine              postgres
    version             17
    instance_class      db.t4g.medium     [default]
    storage             100 GB            [default]
    backup_retention    7 days            [default]
    multi_az            false             [default]
```

The CLI should visually distinguish defaults from explicit configuration.

The implementation must preserve the source of values internally:

```text
explicit
default
inherited
computed
provider
```

Do not simply merge defaults into user configuration and lose that information.

---

# 13. Environment-Aware Defaults

Defaults may depend on environment.

Example:

```text
dev:
    smaller instance
    single AZ
    minimal backups

staging:
    moderate instance
    single AZ
    normal backups

production:
    larger instance
    multi-AZ
    stronger backup defaults
```

Provider default calculation may consider:

* Environment
* Region
* Account
* Resource type
* Project

Defaults should remain conservative and predictable.

Do not silently choose expensive production infrastructure without clearly showing it in the plan.

---

# 14. Resource Definitions

Every provider resource should have a schema.

Conceptually:

```go
type ResourceDefinition struct {
    Type          string
    Schema        Schema
    Defaults      DefaultResolver
    Requirements  []Requirement
    Capabilities  []Capability
    Lifecycle     LifecycleDefinition
}
```

Resources must define:

* Attributes
* Required attributes
* Optional attributes
* Defaults
* Computed attributes
* Sensitive attributes
* Immutable attributes
* Replacement behavior
* Dependencies
* Capabilities
* Import behavior

---

# 15. Resource Lifecycle

Support:

```yaml
lifecycle:
  prevent_destroy: true
```

and:

```yaml
lifecycle:
  retain: true
```

`prevent_destroy` means a normal apply must refuse destruction.

`retain` means removing the resource from configuration removes it from management without deleting the external resource.

This is particularly useful for:

* Databases
* S3 buckets
* Persistent volumes
* DNS
* Critical production infrastructure

---

# 16. Dependency Graph

The compiler must construct a dependency DAG.

Example:

```text
VPC
 ├── Subnet A
 ├── Subnet B
 ├── Security Group
 └── IAM Role
          │
          ▼
        RDS
          │
          ▼
      Application
```

Dependencies may come from:

1. Explicit references.
2. Resource requirements.
3. Module relationships.
4. Provider-defined dependencies.

Resources that have no dependency relationship should be eligible for concurrent execution.

---

# 17. Missing Resource Detection

The system should understand resource requirements.

Example:

```yaml
application:
  type: aws.ecs.service
  image: myapp:latest
```

The provider may declare that an ECS service requires:

```text
cluster
task definition
network
subnets
security group
execution role
```

If these aren't available, planning should report them before apply.

Example:

```text
✗ application

Missing required infrastructure:

  cluster
    └─ not found

  network
    └─ not found

  execution_role
    └─ not found
```

Do not wait until an AWS API call fails.

---

# 18. Dependency Resolution

If possible, the system should suggest how to resolve missing dependencies.

Example:

```text
Required resources are missing:

  aws.ecs.cluster
  aws.ecs.execution_role
  aws.vpc

These can be generated automatically.

Run:

    infra generate dependencies

or use:

    infra plan --fix
```

Automatic generation must be conservative.

Do not automatically generate overly broad IAM policies.

---

# 19. Plans

Plans are first-class objects.

Example:

```text
Environment: production

+ aws.vpc.main

+ aws.rds.database
    engine              postgres
    version             17
    instance_class      db.t4g.medium     [default]

~ aws.ecs.service.web
    replicas:
      2 → 10

- aws.s3.temporary

Plan:
  2 to create
  1 to update
  1 to destroy
```

Plans must contain machine-readable operations.

Support eventually:

```bash
infra plan production --output plan.json
```

and:

```bash
infra apply production plan.json
```

The plan should record enough information to verify that the configuration/state it was generated from has not unexpectedly changed.

---

# 20. Plan Safety

Before applying a plan:

* Verify state lock.
* Verify configuration hash/version.
* Verify plan validity.
* Re-check resources where necessary.
* Refuse to apply stale plans unless explicitly overridden.

Production plans should clearly identify destructive operations.

Example:

```text
⚠ DESTRUCTIVE CHANGE

aws.rds.database

The following resource will be destroyed.

This resource has 3 dependent resources.

Continue? [y/N]
```

---

# 21. State Backend

Implement a state abstraction:

```go
type StateBackend interface {
    Get(ctx context.Context, environment string) (*State, error)
    Put(ctx context.Context, environment string, state *State) error
    Lock(ctx context.Context, environment string) (Lock, error)
    Unlock(ctx context.Context, environment string) error
}
```

Initial backends:

1. Local filesystem.
2. S3.

Later:

* PostgreSQL
* HTTP
* Other object stores

State locking is mandatory for remote operation.

---

# 22. State Locking

Locks are environment-specific.

Example:

```text
production = LOCKED
staging    = AVAILABLE
dev        = AVAILABLE
```

Concurrent applies to the same environment must be prevented.

Concurrent applies to different environments should be allowed.

---

# 23. Drift Detection

The system must support refreshing provider state.

Example:

Configured:

```text
instance_class = db.t4g.medium
```

Actual AWS infrastructure:

```text
instance_class = db.t4g.large
```

Plan:

```text
~ aws.rds.database

    instance_class:
      configured: db.t4g.medium
      actual:     db.t4g.large
```

Applying the plan should restore the declared configuration where the provider supports it.

---

# 24. Infrastructure Discovery

This is a major product feature.

Command:

```bash
infra discover aws
```

should discover infrastructure in the configured account/regions.

Example:

```text
AWS Infrastructure

VPC
├── vpc-012345
│   ├── subnet-a
│   ├── subnet-b
│   └── security groups

RDS
├── production-db
└── staging-db

ECS
├── production
│   ├── web
│   └── worker

S3
├── production-assets
└── backups
```

Discovery should be optimized to avoid unnecessary API calls.

Cache discovery results where practical.

---

# 25. Interactive Infrastructure Explorer

Provide commands such as:

```bash
infra discover
infra discover list
infra discover show
infra discover aws.rds
infra discover aws.ecs
```

Eventually provide an interactive terminal explorer.

Example:

```text
AWS
 └── us-east-1
      ├── VPC
      ├── ECS
      ├── RDS
      ├── S3
      └── IAM
```

The goal is to allow an engineer to rapidly understand an unfamiliar AWS account.

---

# 26. Import

Support:

```bash
infra import
```

and targeted import:

```bash
infra import aws.rds.production-db
```

Import should:

1. Locate the external resource.
2. Determine its provider type.
3. Read its current attributes.
4. Generate a logical resource identity.
5. Add it to state.
6. Optionally generate configuration.

Import must not blindly destroy or modify infrastructure.

---

# 27. Configuration Generation

Support:

```bash
infra import aws.rds.production-db --generate
```

Generated configuration should be **minimal**.

If AWS returns:

```text
engine = postgres
version = 17
instance_class = db.t4g.medium
storage = 100
backup_retention = 7
multi_az = false
```

and all except `engine` are defaults, generate:

```yaml
database:
  type: aws.rds
  engine: postgres
```

Do not generate pages of unnecessary configuration.

## 27.1 Where generation writes, and why it is loaded

Generated configuration goes to `discovered/`, in files named for what they hold —
`databases.yml`, `networks.yml` — rather than one file per import run. A person looking for
the database they imported last month looks in `databases.yml`.

**`discovered/*.yml` is LOADED by the compiler, exactly as `environments/*.yml` is.** That is
not a convenience; it is what makes import safe. Import adds a resource to state (§26 step 5),
and a resource in state that no configuration declares is scheduled for DESTRUCTION by
invariant 1. If the generated file were a staging area, `infra import` followed by
`infra apply` would destroy the very infrastructure just adopted — which §26's own rule that
"import must not blindly destroy or modify infrastructure" forbids.

So the resource is in state and in configuration at the same moment, and the next plan is
clean. Moving a block out of `discovered/` into a file of your own is then an ordinary edit,
made when you want to, not a step you must complete before it is safe to run anything.

## 27.2 Naming a discovered resource

§26 step 4 requires a logical identity. It comes from, in order:

1. A `name` attribute or tag, if the resource has one. This is how people actually label cloud
   resources, and it is the name they will look for.
2. Otherwise the provider ID, sanitised to an identifier — `net-1` becomes `net_1`.

A collision between two resources claiming the same name is resolved by suffixing the provider
ID, never by dropping one: two resources silently becoming one is the shape this project
guards against everywhere else.

Names matter more here than they look. A resource's name is part of its address, and an address
is what state is keyed by — so renaming an imported resource later is a destroy plus a create.
Generation should therefore produce the name a person would have chosen, not one they will
immediately want to change.

---

# 28. Literal Export

Also support complete export:

```bash
infra export production
```

which includes every known configurable attribute.

This is useful for:

* Auditing
* Migration
* Debugging
* Documentation
* Reproducing infrastructure

---

# 29. Import/Generate Round Trip

A core invariant should be:

```text
existing infrastructure
        ↓
discover
        ↓
import
        ↓
generate minimal YAML
        ↓
plan
        ↓
no unexpected changes
```

This must be an integration test.
`tests/integration/m8_roundtrip_test.go` is it (M8).

## 29.1 What "unexpected" excludes

There is exactly one expected change, and it is not a defect to be engineered
away: **an attribute the generator omitted because the provider marks it
sensitive.**

§27 forbids writing a discovered secret into `discovered/*.yml`, because that
file is destined for version control and a secret committed to git is a secret
ROTATED, not a secret deleted. State must still hold the value, or drift
detection on that attribute silently stops working. So configuration and state
genuinely disagree about that one attribute, and the plan says so.

Both alternatives are worse. Writing the secret to disk is the failure the whole
milestone exists to prevent. Dropping it from state would make the plan clean by
discarding the engine's only record of the real value, which is the same trade
as suppressing a drift report because it is inconvenient.

So the round trip's guarantee is: **zero operations, except that each attribute
the generator omitted for sensitivity shows as one pending change on its own
resource, and the generated file names it at the point of omission.** Supplying
the value — which is what the generated file's `TODO` tells the reader to do —
makes the plan clean.

A COMPUTED attribute is not an exception to this: it is omitted too, and the
planner already ignores computed attributes when diffing, so it produces no
change at all. Nor is an attribute a provider reports that its own schema does
not declare.

## 29.2 Minimality is not tested by the round trip

A generator that emitted EVERY attribute would also produce a clean plan. The
round trip therefore proves nothing about §27, and minimality has to be asserted
separately against the generated bytes. Verified by sabotage: disabling the
default-omission rule leaves `TestTheImportRoundTripPlansClean` passing and
fails only `TestTheRoundTripGeneratesMinimalConfiguration`.

---

# 30. `infra explain`

Provide:

```bash
infra explain aws.rds
infra explain aws.ecs.service
```

Example:

```text
aws.ecs.service

Required:
  cluster
  task_definition
  network

Optional:
  replicas          default: 1
  health_check      default: enabled

Computed:
  endpoint

Capabilities:
  create
  read
  update
  delete
  import
```

This makes the system discoverable without requiring users to constantly consult documentation.

---

# 31. Providers

Providers should be isolated behind an interface.

Conceptually:

```go
type Provider interface {
    Name() string

    Discover(ctx context.Context, request DiscoverRequest) ([]DiscoveredResource, error)

    Read(ctx context.Context, resource *ResourceState) (*ResourceState, error)

    Create(ctx context.Context, resource *DesiredResource) (*ResourceState, error)

    Update(ctx context.Context, current *ResourceState, desired *DesiredResource) (*ResourceState, error)

    Delete(ctx context.Context, resource *ResourceState) error

    Import(ctx context.Context, resourceType string, id string) (*ResourceState, error)
}
```

Do not let AWS-specific code leak into the core planner.

---

# 32. Provider Resources

Initial AWS MVP resources:

### Networking

* VPC
* Subnet
* Security Group

### Storage

* S3 bucket

### Database

* RDS PostgreSQL

### Compute

* ECS cluster
* ECS task definition
* ECS service

### IAM

* IAM role
* IAM policy attachment

### Networking/application

* ALB
* Route53 record

Do not attempt to implement all AWS resources initially.

---

# 33. Fake Provider

Before implementing AWS, implement a fake provider.

Example:

```yaml
resources:

  database:
    type: test.database

  application:
    type: test.application
```

The fake provider should support:

* Create
* Read
* Update
* Delete
* Drift
* Import
* Dependencies
* Failures

This lets the core engine be developed and tested without AWS.

---

# 34. Executor

The executor consumes a plan and dependency graph.

Example:

```text
              VPC
               │
       ┌───────┼───────┐
       ▼       ▼       ▼
    subnet1 subnet2 security
       │       │       │
       └───────┼───────┘
               ▼
              RDS
```

Independent resources execute concurrently.

Use Go concurrency primitives carefully.

Limit concurrency per provider/account to avoid API throttling.

---

# 35. Retries

Provider operations should have retry policies.

Handle:

* AWS throttling
* Temporary network failures
* Eventual consistency
* Retryable API failures

Do not blindly retry destructive operations.

Every operation should be classified as:

```text
safe-to-retry
conditionally-retryable
not-safe-to-retry
```

---

# 36. Sensitive Values

Secrets must never appear in normal plans.

Example:

```text
password:
    <sensitive>
```

State must protect sensitive values.

Support references such as:

```yaml
password:
  secret: DATABASE_PASSWORD
```

Eventually support external secret stores such as AWS Secrets Manager.

---

# 37. CLI

Initial commands:

```bash
infra init
infra validate

infra plan <environment>
infra apply <environment>

infra destroy <environment>

infra state
infra state show <address>

infra refresh <environment>

infra import
infra export

infra discover

infra graph

infra explain <resource-type>
```

Useful options:

```bash
--var
--var-file
--output
--auto-approve
--parallelism
--verbose
```

---

# 38. Production Protection

Environment configuration should support:

```yaml
environment:
  type: production

  protections:
    require_approval: true
    prevent_destroy: true
```

Desired behavior:

```text
dev
 └── automatic apply allowed

staging
 └── normal approval

production
 ├── plan required
 ├── approval required
 ├── destructive changes highlighted
 └── destroy protection
```

These protections should be enforceable by the engine, not merely conventions.

---

# 39. Configuration Validation

`infra validate` should detect:

* Invalid YAML.
* Unknown resource types.
* Invalid attributes.
* Invalid variable types.
* Missing variables.
* Invalid references.
* Circular dependencies.
* Missing required dependencies.
* Invalid module inputs.
* Invalid environment inheritance.
* Invalid lifecycle configuration.

Validation must not make provider mutations.

---

# 40. Graph Visualization

Support:

```bash
infra graph production
```

Output at minimum:

```text
network
 ├── database
 └── application
       └── load_balancer
```

Eventually support Graphviz/DOT output:

```bash
infra graph production --format dot
```

---

# 41. Repository Structure

Start approximately as:

```text
cmd/
  infra/

internal/
  config/
  compiler/
  expressions/
  environments/
  modules/
  variables/
  state/
  planner/
  graph/
  executor/
  discovery/
  importer/
  generator/
  lifecycle/
  secrets/
  cli/

pkg/
  provider/
  schema/
  plan/
  resource/

providers/
  test/
  aws/

tests/
  integration/
```

Keep the core engine independent of AWS.

---

# 42. Core Data Model

Define clear internal representations for:

```text
Project
Environment
Module
Resource
ResourceDefinition
DesiredResource
ResourceState
Plan
PlanOperation
Dependency
Variable
Expression
Provider
```

Do not allow raw YAML maps to flow throughout the application.

Parse YAML into typed intermediate representations early.

---

# 43. Value Provenance

Every resolved value should retain its origin.

Possible sources:

```text
explicit
default
environment
variable
module
computed
provider
```

Example:

```go
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
```

This enables:

* Better plans.
* Better generated YAML.
* Better debugging.
* Better explain output.

---

# 44. Error Messages

Errors are a major part of the product experience.

Prefer:

```text
application requires a network.

No network resource was found in environment "production".

Expected one of:
  aws.vpc
  module.network

Suggested action:
  Add a networking module or resource.
```

Avoid:

```text
InvalidParameterValue: subnet ID cannot be empty
```

Provider errors should be translated into useful contextual errors whenever practical.

---

# 45. Logging

Normal output should be clean.

Example:

```text
Planning production...

✓ Configuration valid
✓ State loaded
✓ Dependencies resolved
✓ Current infrastructure refreshed

Plan:
  + 3 create
  ~ 2 update
  - 1 destroy
```

Verbose mode:

```bash
infra plan production --verbose
```

should expose API/provider details useful for debugging.

---

# 46. Testing Strategy

Tests are mandatory at every layer.

## Unit tests

Test:

* YAML parsing
* Variable resolution
* Environment inheritance
* Module resolution
* Default resolution
* Value provenance
* Expression evaluation
* Dependency graph
* Plan generation
* Lifecycle behavior

## Integration tests

Use the fake provider to test:

* Create
* Update
* Delete
* Import
* Drift
* Missing dependencies
* Concurrent execution
* State locking
* Failed operations
* Recovery

## AWS integration tests

Use real AWS only for explicit integration tests.

Do not require AWS credentials for normal CI.

---

# 47. Critical Invariants

The following must always hold.

### Configuration removal

If:

```text
resource exists in state
resource absent from configuration
```

then:

```text
plan => destroy
```

unless lifecycle protection says otherwise.

### No-op plan

If desired state equals actual state:

```text
plan => zero operations
```

### Import

Imported infrastructure followed by minimal generation and planning should produce no unexpected changes.
"Unexpected" excludes exactly one thing — an attribute omitted because it is
sensitive. See §29.1, which also explains why making that case clean would be
worse than leaving it visible.

### Dependency ordering

A resource must never execute before its dependencies.

### Locking

Two applies cannot mutate the same environment simultaneously.

### Plan determinism

Identical:

```text
configuration
+
state
+
provider observations
```

must produce equivalent plans.

---

# 48. MVP Definition

The MVP is complete when the following workflow works end-to-end with the fake provider:

```bash
infra init

infra validate

infra plan dev

infra apply dev

infra plan dev
# no changes

# manually mutate fake infrastructure

infra plan dev
# drift detected

# remove a resource from YAML

infra plan dev
# destroy proposed

infra apply dev

infra discover

infra import ...

infra export ...
```

Then implement the AWS provider.

---

# 49. Phase 1 — Core Engine

Implement:

1. Repository scaffolding.
2. CLI.
3. YAML parser.
4. Typed configuration model.
5. Variable system.
6. Environment system.
7. Module system.
8. Resource registry.
9. Provider interface.
10. Fake provider.
11. State model.
12. Local state backend.
13. State locking.
14. Dependency graph.
15. Planner.
16. Plan renderer.
17. Executor.
18. Lifecycle protection.
19. Comprehensive tests.

Do not begin with AWS.

---

# 50. Phase 2 — Discovery and Import

Implement:

1. Provider discovery interface.
2. Fake discovery.
3. Resource importer.
4. State import.
5. Configuration generator.
6. Minimal/default-aware generation.
7. Full export.
8. Interactive discovery.
9. Round-trip integration tests.

---

# 51. Phase 3 — AWS

Implement:

1. AWS provider.
2. Credential detection.
3. Region handling.
4. VPC.
5. Subnet.
6. Security Group.
7. S3.
8. RDS.
9. IAM roles.
10. ECS cluster.
11. ECS task definition.
12. ECS service.
13. ALB.
14. Route53.

Prioritize complete lifecycle support over a huge number of partially implemented resources.

---

# 52. Phase 4 — Remote State

Implement:

1. S3 backend.
2. Locking.
3. State versioning.
4. State migration.
5. State encryption/documentation.
6. Concurrent environment testing.

---

# 53. Phase 5 — Production Features

Implement:

* Drift detection improvements.
* Secrets.
* Policy checks.
* Plan artifacts.
* CI integration.
* Plan/apply approvals.
* Better import.
* Better generated configuration.
* Provider throttling.
* Audit information.
* Resource replacement strategies.

---

# 54. Do Not Build Yet

Do not initially build:

* Every AWS resource.
* A general-purpose programming language.
* A web UI.
* A SaaS control plane.
* Distributed execution.
* Kubernetes provider.
* GCP/Azure providers.
* Complex policy language.
* Remote collaboration platform.
* Automatic IAM policy generation beyond conservative basics.

The architecture should allow these later, but they should not delay the core product.

The web UI, SaaS control plane and remote collaboration platform are the commercial product (§60). They are built later, as a separate codebase, and never inside the open core.

---

# 55. Future Architecture

The long-term architecture should permit:

```text
                 CLI
                  │
                  ▼
              Core Engine
                  │
        ┌─────────┼─────────┐
        ▼         ▼         ▼
      State     Planner   Providers
        │                   │
        │             ┌─────┼─────┐
        │             ▼     ▼     ▼
        │            AWS   GCP   Azure
        │
        ▼
 Remote State
```

A future distributed executor could be added without changing the configuration language.

---

# 56. Product Differentiators

The implementation should preserve these as explicit product goals.

## Simpler than Terraform

A new infrastructure engineer should be able to understand:

```yaml
database:
  type: aws.rds
  engine: postgres
```

without understanding a large configuration language.

## Better environments

Environments are first-class, not workspaces.

## Better defaults

Users should specify what matters rather than boilerplate.

## Better onboarding

Existing infrastructure should be discoverable and importable quickly.

## Better generated configuration

Import should produce clean, minimal YAML rather than enormous dumps.

## Better errors

Missing dependencies should be identified before provider APIs fail.

## Better plans

Plans should clearly show:

* Creates
* Updates
* Deletes
* Replacements
* Defaults
* Sensitive values
* Dependencies
* Potentially dangerous changes

---

# 57. Initial Example

A realistic project should eventually look roughly like:

```yaml
project: myapp

resources:

  network:
    type: aws.vpc
    cidr: 10.20.0.0/16

  database:
    type: aws.rds
    engine: postgres

  application:
    type: aws.ecs.service
    image: ${application_image}
    replicas: ${replicas}
    database_url: ${database.connection_string}
```

`variables.yml`:

```yaml
application_image: myapp:latest
```

`environments/dev.yml`:

```yaml
replicas: 1
```

`environments/staging.yml`:

```yaml
replicas: 2
```

`environments/production.yml`:

```yaml
replicas: 10
```

Then:

```bash
infra plan dev
infra apply dev

infra plan staging
infra apply staging

infra plan production
```

The same infrastructure definition is used for all three environments.

Only the environment-specific values differ.

---

# 58. Claude Code Implementation Instructions

When implementing this project:

1. Start by creating the repository structure.
2. Write architecture/design documentation.
3. Implement the typed configuration model.
4. Implement validation.
5. Implement variables and environments.
6. Implement the provider/resource abstraction.
7. Implement the fake provider.
8. Implement state.
9. Implement dependency graphs.
10. Implement planning.
11. Implement plan rendering.
12. Implement execution.
13. Add tests for every feature.
14. Implement discovery/import/generation.
15. Only then implement AWS.

Do not skip tests in order to move faster.

When making architectural decisions not explicitly covered here:

* Prefer simple designs.
* Prefer standard Go patterns.
* Prefer explicit behavior over magic.
* Keep provider-specific logic out of the core.
* Preserve value provenance.
* Preserve deterministic plans.
* Avoid introducing unnecessary abstractions.
* Document decisions that affect the public configuration format.

Before changing the configuration language, consider backwards compatibility even during pre-1.0 development.

The configuration language is a product API and should not be changed casually.

---

# 59. Definition of Success

The project succeeds if an engineer can take an existing AWS account and do:

```bash
infra discover
```

quickly understand the infrastructure, then:

```bash
infra import
```

bring selected infrastructure under management, generate clean YAML, and subsequently use:

```bash
infra plan dev
infra apply dev

infra plan staging
infra apply staging

infra plan production
infra apply production
```

with confidence.

The final experience should feel like:

> **Ansible's readability + Terraform's state and planning + a much better environment/import/default experience.**

The most important engineering goal is to make the **core reconciliation engine correct and deterministic**. Provider breadth can come later.

---

# 60. Open Source and Commercial Model

Infrata will be sold as a commercial product built around an open-source core.

**The core stays open source permanently.** This is a commitment to users, not a phase: the core is never relicensed, and no feature is ever moved out of the core into the paid product.

## The open core

The open core permanently contains:

* The `infrata` CLI and every command in §37.
* The engine: compiler, planner, executor, state, graph, discovery, import and generation.
* All providers, including AWS.
* Local and S3 state backends with locking (§21, §22, §52).
* Engine-enforced production protections: `require_approval`, `prevent_destroy` and stale-plan refusal (§20, §38).

The dividing line: **what one engineer needs is free; what a team needs to coordinate, or an organization needs to prove, is paid.**

## The commercial product

### Hosted control plane

* Managed state with version history and per-address change history.
* Remote plan and apply on hosted runners, or on self-hosted agents inside the customer's network so cloud credentials never leave it.
* VCS integration: a plan on every pull request, apply on merge, with the guarantee that the approved plan is the plan applied (§20).
* A run queue per environment, and a live run view streamed from executor events.
* Notifications, webhooks, and integration points between plan and apply.

### Differentiated features

A hosted run interface alone is a crowded market. These features use data only Infrata's engine has, and are where the commercial product competes:

* **Environment matrix and promotion.** Status per environment, diffs between environments, and promoting a change applied in one environment to the next (relies on `extends`, §7).
* **Provenance in the plan UI.** Every value shows where it came from and what it overrode (§43).
* **Unmanaged-resource inventory.** Resources no environment manages, with one-click import to a pull request of minimal YAML (§24–27).
* **Drift triage.** Scheduled detection, then either accept into configuration (a pull request) or revert (an apply) (§23).
* **Ephemeral per-pull-request environments** that extend an existing environment and expire.
* **Blast radius** of a change, from the dependency graph (§16).
* **Throttling per cloud account across all runs**, not only within one process (§34).

### Governance

* SSO (SAML/OIDC), SCIM, and per-environment RBAC separating plan, apply and approve.
* Server-enforced approvals: N approvers for production, named approvers for destructive changes. The CLI's protections (§38) stay free; the commercial product makes them impossible to bypass, because production credentials exist only on the runner.
* Policy checks at advisory, soft-mandatory (overridable with a reason) and hard levels: simple declarative rules plus OPA integration. Infrata still does not grow a complex policy language of its own (§54).
* Change windows, freeze periods, and break-glass access with a recorded justification.
* An immutable audit log exportable to a SIEM, and signed plan attestations.
* Cost estimates in plans, and cost-based policies.

### Enterprise deployment

* Self-hosted and air-gapped control plane.
* Private module and provider registries.
* Short-lived, OIDC-federated cloud credentials per environment.
* Secrets backend integrations such as Vault, AWS Secrets Manager and 1Password.
* Support agreements and long-term-support releases.

## Constraints on the open core

The commercial product is a separate, proprietary codebase. It is built on the open core's public contracts, never on a fork. That imposes constraints on the core now:

1. **The CLI's machine-readable output is a product API.** The commercial runner invokes the `infrata` binary, as Terraform Cloud agents invoke `terraform`, rather than importing the engine; the engine's packages live under `internal/` and cannot be imported from another module anyway. The JSON plan artifact, a JSON event stream and exit codes are held to the same compatibility standard as the configuration language (§58).
2. **Redaction stays in the engine.** Executor events carry only pre-redacted text, so no integration ever receives a raw attribute (§36).
3. **The actor is recorded.** Plans and state record who made a change, so an audit trail never has to be retrofitted.
4. **Plan and apply keep a seam between them** where policy checks and approvals attach.
5. **No commercial code paths in the core.** No license checks and no feature gates.
6. **The rename completes before the first public release**, so no user's import path ever breaks.

## Making "open source forever" credible

HashiCorp relicensed Terraform in 2023 after years as open source, and the community forked it as OpenTofu. A promise alone will not be believed, so it is made structural:

* **Publish an open-core policy** stating the dividing line and that features never move from free to paid, before the commercial product exists.
* **Accept contributions under a DCO sign-off, not a CLA.** A permissive license already lets contributed code ship inside the commercial product. A CLA would only add the right to relicense, which is exactly what is promised never to happen.
* **Trademark "Infrata".** The code is open; the name is controlled. A trademark policy lets forks use the code but not the name.

## Accepted risk

A permissive core means competitors such as Spacelift, env0 and Scalr can build the same commercial features on the same engine. The advantage has to come from being the maintainers: velocity, trust, the brand, and a better hosted product.

## Open decisions

* **License.** Apache 2.0 is recommended: it carries a patent grant and enterprise legal teams approve it without review. MPL 2.0 is acceptable. AGPL is ruled out, since enterprises commonly ban it and the commercial product depends on adoption.
* **Copyright holder**, an individual or a company. Settle before anything is sold.
* **Contribution sign-off.** DCO is recommended, above.
* **Module path.** `github.com/infrata/infrata`, or a vanity path `infrata.dev/infrata`, which keeps import paths stable if hosting ever moves off GitHub.
* **Trademark registration.**
* **Pricing model.** Avoid pricing per resource under management, which was widely unpopular for HCP Terraform.
