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

Environments are cheap and numerous on purpose. A project may have `dev`,
`staging`, `sandbox`, `preview-471` and three more nobody remembers creating.
They hold the SAME infrastructure and differ only in what their variables say —
that is the whole mechanism for environment variation, and there must not be a
second one.

## 6.1 An environment is reachable if it is declared OR it has state

Removing an environment from configuration must tear it down, and must let you
SEE the teardown first.

An environment with state but no declaration has an empty desired
configuration. Invariant 1 then applies exactly as it does to a single removed
resource: everything in its state is proposed for destruction. `plan` shows
that, and `apply` performs it.

The state FILE is deliberately left in place afterwards, holding nothing. It is
not what makes an environment reachable — "has state" means "state lists
resources", so an emptied environment is already unreachable as an orphan and
planning it reports the typo case. Removing the file would also discard the
serial, which is how a stale plan is detected and which `destroy` is required to
advance on the write that records its own removals.

A project that declares NO environments at all keeps working with any name, as it
has since M2 — there is nothing for a name to be a typo against until at least
one environment is declared. §6.1's rule begins to bite only then.

An environment that is neither declared NOR holds state, in a project that does
declare others, stays an error naming them. That is what keeps `infra plan devv` a typo rather
than a silent no-op, and it is why the rule is a disjunction rather than
"anything goes".

The plan for an undeclared environment must say WHY everything is being
destroyed — "no environment named X is declared; its state lists N resources"
— because a total destruction that looks like an ordinary plan is the single
most alarming output this tool can produce.

## 6.2 Restricting a resource to environments: `skip` and `only`

Any resource may name the environments it belongs to:

```yaml
resources:

  debug_box:
    type: test.application
    skip: [dev, staging]

  replica:
    type: test.database
    only: production

  canary:
    type: test.application
    only: [staging, production]
```

Rules:

- **A scalar or a list.** `only: production` and `only: [staging, production]`
  mean the same shape of thing.
- **Both keys on one resource is an error.** They are two spellings of one idea
  and can contradict each other.
- **An environment named that no environment declares is an error**, naming the
  ones that exist. A filter that quietly matches nothing is worse than no
  filter — `skip: [prod]` against an environment called `production` would
  otherwise do nothing, forever, silently.
- **A skipped resource is exactly as if it were not declared** in that
  environment. It follows that adding `skip:` to something already applied
  PROPOSES DESTROYING IT there. That is the feature, not a side effect, and
  `lifecycle.prevent_destroy` still refuses it.
- **A reference to a skipped resource is an error** — `${debug_box.url}` from a
  live resource reports that `debug_box` is skipped in this environment, NOT
  "no such resource". The same applies to `depends_on`. The consequence is
  real: skipping a resource forces you to skip what depends on it. Silently
  dropping the edge is the alternative, and it produces a plan that applies and
  then fails partway.
- **The value may be an expression.** `only: ${replica_environments}` resolving
  to a string or a list. This is what lets a MODULE be written with parts that
  the caller can switch off:

  ```yaml
  # modules/app-stack/module.yml
  inputs:
    replica_in:
      type: list
      default: []
  resources:
    replica:
      type: test.database
      only: ${replica_in}
  ```
  ```yaml
  # the caller
  resources:
    stack:
      type: module.app_stack
      replica_in: [production]
  ```

- **On a module call**, `skip`/`only` are evaluated in the CALLER's scope,
  before expansion — a skipped module never expands at all. **Inside a module**
  they are evaluated in the module's own scope, so they can read its inputs.
- The names are checked after the expression resolves, so a variable supplying
  a nonexistent environment is caught too.

A skipped resource cannot simply vanish during expansion, or a reference to it
reports "no such resource" and the rule above is unimplementable. It stays in
the expansion marked as skipped, reference binding reports it, and it is
dropped before the planner sees it.

## 6.3 The process variables

Four values come from the invocation rather than from any file, and are
available in every scope including inside modules:

| Variable | Source |
|---|---|
| `environment` | the environment argument |
| `project` | `project:` |
| `region` | `--region`, when supplied |
| `account` | `--account`, when supplied |

`project` is here because a resource name or a tag almost always wants it, and
threading it through as an ordinary variable makes every project declare the
same line.

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

## 10.1 Interpolation inside a composite value

An interpolation may appear in a string leaf of a list or a map, not only in a
bare string:

```yaml
tags:
  environment: ${environment}
  project: ${project}
  team: payments
```

Until M10 this was refused — "interpolation inside a map is not supported" —
because `HasExpressions` was set whenever any leaf held `${` while the leaves
themselves were never parsed, so passing the composite through would have put
raw `${...}` text into a plan as though it were a literal. The refusal was
correct for the code that existed; what M10 adds is the walk that makes it
unnecessary.

Rules:

- Each STRING leaf is parsed and evaluated independently. A leaf with no `${` is
  untouched.
- A key is never interpolated. `${x}: y` is not a thing; keys are literal, so a
  configuration's shape never depends on a value.
- Sensitivity and provenance are per leaf, as everywhere else (§43). A map one
  leaf of which resolves to a secret is a map with one sensitive leaf, not a
  wholly sensitive map — `pkg/value` already models this and
  `value.Format` already redacts at that granularity.
- An unknown leaf makes the composite unknown for dependency purposes, because
  the resource genuinely cannot be created until it resolves. The EDGE is
  recorded from the leaf, exactly as it would be from a bare string.
- Nesting is allowed to whatever depth YAML produced, because refusing depth two
  while allowing depth one would be a rule nobody could predict.

Three places refuse this today — `internal/compiler/bind.go` and two in
`internal/modules/outputs.go`. They must end up calling ONE walk. Three copies
of a rule about where expressions may appear is three chances for a module
output and a resource attribute to disagree about the same YAML.

## 10.2 Functions

The set is FIXED and enumerated. Adding one is a configuration-language change
requiring an amendment to this section — `internal/expressions/funcs.go` says so
and must keep saying so.

```text
lower(s)            upper(s)            trim(s)
replace(s, old, new)                    join(list, sep)
default(v, fallback)                    merge(a, b, ...)
```

`merge` is M10's addition. It takes maps and returns their union, with LATER
arguments winning per key. It exists because §12.1's provider block REPLACES
rather than merges, which makes merging something the user asks for explicitly
rather than something that happens to them.

**Every function is pure, total and side-effect free.** No `now()`, no `uuid()`,
no file or network access, ever. Those three are the ones most often asked for
next, and each one silently breaks invariant 6: the same configuration and state
would produce a different plan on a second run, which is the property the whole
plan/apply split rests on. A function that cannot be evaluated twice with the
same answer does not belong in this language.

Sensitivity unions across ALL arguments. This is not a detail: `join()` and
`replace()` each shipped wrong here, and `replace()`'s omission let a secret
search term reveal its own position through an unclassified result. `merge` is
the third chance to get it wrong and the one whose result is most likely to be
written into a tag.

## 10.3 Literals in argument position

A map or list literal may appear as a function ARGUMENT, and nowhere else:

```yaml
tags: "${merge(tags, {team: payments, project: billing})}"
```

Not as a value on its own, because YAML already does that job. Bounding it to
argument position is what keeps this from being the first step toward a
programming language.

**The quotes are required, and not by us.** YAML itself rejects the unquoted
form: a plain scalar may not contain `: `, so `tags: ${merge(a, {b: c})}` fails
with "mapping values are not allowed in this context" before any of this code
sees it. That message says nothing about quoting, so the loader detects this
shape and says what to do.

## 10.4 One scanner

`matchBrace` and `splitArgs` both walk a string tracking quote state and nesting
depth, and they already share `skipEscape` with a comment explaining why:

> when two scanners disagree, input is accepted by one and rejected by the other,
> which is the class of bug the quote handling was added to fix in the first place

They get away with the remaining duplication only because one counts `{}` and the
other counts `()`, and those sets do not overlap today. §10.3 makes them overlap:
a comma inside a map literal is not an argument separator. **The two must become
one walk before literals are added**, not after, because the alternative is
editing both in lockstep — which is exactly what that comment predicts will go
wrong.

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

## 12.1 Provider instances

`providers:` is a LIST of provider instances. Each entry names the PLUGIN that
implements it, an optional NAME, and that instance's own configuration.

```yaml
# One instance. Every resource uses it.
providers:
  - plugin: aws
    iam-role: some-role-it-assumes
```

```yaml
# Two plugins. `aws` is the default because it is first.
providers:
  - plugin: aws
    iam-role: some-role-it-assumes
  - plugin: azure
    auth-token: a-token
```

```yaml
# Two instances of ONE plugin, so they need names.
providers:
  - plugin: aws
    name: main
    iam-role: role-for-main
  - plugin: aws
    name: acct2
    iam-role: role-for-acct2
```

Resources choose one by NAME, and omitting the key means the default:

```yaml
resources:
  main-network:
    type: test.network
    cidr: 10.0.0.0/16
    provider: main
  acct2-network:
    type: test.network
    cidr: 10.1.0.0/16
    provider: acct2
  web1:
    type: test.application
    image: nginx:1.27
    # no `provider:` — the default instance
```

### Rules

- **`plugin` is required.** It names an implementation the build offers; an
  unknown one is an error listing what is available.
- **`name` defaults to the plugin name.** One `aws` entry with no name is called
  `aws`.
- **Names are unique, and a collision is an error.** Two entries that resolve to
  the same name — two unnamed `aws` entries, or two both named `aws` — are
  refused. There is no precedence to invent: whichever won, the other instance's
  resources would silently go to the wrong account.
- **The default is the FIRST entry** unless one is marked otherwise.
- **A resource's `provider:` names an INSTANCE, never a plugin.** An unknown name
  is an error listing the declared instances, for the reason §6.2 gives about
  `skip`: a filter or selector that quietly matches nothing is worse than none.
- **A module CALL's `provider:` is inherited by everything it expands into**, unless an inner
  resource names one itself. Deploying one stack into two accounts is then two calls differing by
  one line, rather than a provider threaded through a module input onto every resource inside.
  `depends_on` on a call already fans out the same way, for the same reason.
- **Moving a resource between instances is a DESTROY and a CREATE**, not an
  update. The resource genuinely lives in a different account; the same reasoning
  as §5.2's module paths, and the planner must say so where a user reads it.

### Variables, so an instance differs per environment

An instance's configuration may interpolate, which is how one project reaches a
different account per environment:

```yaml
providers:
  - plugin: aws
    iam-role: ${aws_role}
    region: ${aws_region}
```

```yaml
# vars/production.yml
aws_role: arn:aws:iam::111111111111:role/deploy
aws_region: us-east-1
```

```yaml
# vars/dev.yml
aws_role: arn:aws:iam::222222222222:role/deploy
aws_region: us-west-2
```

**`${aws_region}` is an ORDINARY DECLARED VARIABLE, and these examples used to write
`${region}` as though it were supplied by the process.** It is not. `region` and
`account` are named as process variables in §12/§43 alongside `environment` and
`project`, and `compiler.Options.Region` exists and is read — but nothing anywhere
assigns it, so `${region}` reports `undefined variable "region"` today (checked against
the built binary, 2026-09-13). `provider.DiscoverRequest.Region` is inert in the same
way: `internal/discovery/walk.go` builds the request without it and no flag sets it, so
a plugin author implementing against `DiscoverParams.Region` reads `""` forever.

Two dead fields and a documented variable that does not exist. **Phase 3 decides it**:
either a `--region` flag that fills all three, or region is ordinary configuration and
the fields come out. The AWS plugin's agreed model (2026-09-13) needs none of them —
every regional type declares `region` as Required and ForceNew, an instance supplies it
through `defaults:`, and `discover` takes its scan list from the instance's own
`config:` — which is evidence for removal rather than for the flag. Until it is decided,
no example here may use `${region}`.

**Variables only — never a resource reference.** A provider's configuration is
needed before any resource exists, so `iam-role: ${some_resource.arn}` cannot be
satisfied: the provider would have to create the thing its own credentials depend
on. Resolution therefore happens after stage 4 (variables) and before stage 5,
and a reference to a resource attribute here is an error saying so rather than an
unknown that fails later.

That is the same constraint §6.2 puts on `skip`/`only`, and for the same reason:
both decide something the engine needs before it can plan anything.

The per-leaf walk from §10.1 applies, so a nested value interpolates too:

```yaml
providers:
  - plugin: aws
    tags:
      environment: ${environment}
```

### Internal shape

Resolved to a map keyed by instance name, for the lookup resources do:

```text
aws   -> {plugin: aws, config: {iam-role: aaaa}, default: true}
acct2 -> {plugin: aws, config: {iam-role: bbbb}}
```

The list is the AUTHORING shape because order decides the default and because a
YAML map cannot hold two `aws` keys — which is exactly the collision the rules
above refuse, and a shape that cannot express it would refuse it silently.

### What this costs, recorded before it is built

Three assumptions in the engine are one-provider-per-type and must change (all three are
now discharged, the migration included — see §21.1):

- `registry.Registry` is keyed by resource TYPE and `Register` REFUSES a type a
  second provider already claims. Two instances of one plugin collide there
  immediately, so lookups become (type, instance) rather than (type).
- Five call sites do `reg.Provider(someType)`: `executor/apply.go` twice,
  `refresh/refresh.go` twice, `cli/import.go` once. Each needs the instance the
  resource belongs to.
- `resource.ResourceState.Provider` already exists and already holds a provider
  name, so state needs no new field — but it must come to hold the INSTANCE name,
  and a state file written before this change names a plugin. That is a migration,
  and §21's migration path WAS lossy, which is what made fixing it urgent rather
  than theoretical. It is fixed: `state.Decode` decodes the migration path with
  `json.Decoder.UseNumber()`, so a number keeps its exact text and a version bump
  is an ordinary change again.

### A plugin is not a provider: the factory split

An instance's configuration may interpolate a variable — that is the whole point of
`region: ${aws_region}` — and that creates a cycle. Constructing a provider needs its
configuration; resolving the configuration needs variables; resolving variables needs
a compile; and a compile needs the provider's SCHEMAS. Something has to come first.

What breaks it is that **schemas need no configuration**. `aws.instance` is described
the same way whichever account it would be created in. So the provider interface
splits in two:

- `provider.Plugin` — `Name()`, `Definitions()`, and `New(instance, config)`. The
  first two answer before anything is configured; the third is the factory.
- `provider.Provider` — one configured instance, unchanged from §31.

The registry holds both halves, and registration happens in two steps at two
different times:

1. `RegisterPlugin(p)` records the SCHEMAS. The CLI does this before it reads
   anything, so `Definition(type)` answers throughout the compile.
2. `RegisterInstance(instance, plugin, config)` builds the provider OBJECT. Compiler
   stage 4.5 (`internal/providers.Prepare`) does this — after variables, before module
   expansion — from configuration it has just resolved.

Between the two the registry dispatches nothing, and that is not a hazard to design
around: the only production caller is `internal/cli`, and every command either
compiles (so stage 4.5 runs) or is a state-only command that registers its instances
explicitly.

**The two state-only paths are the honest cost.** `destroy` and `refresh` never
compile: one synthesises an empty desired configuration from state and the other
reads state and calls `Provider.Read`. `discover` and `import` do not compile either.
None of them has a variable scope, so none can resolve an instance's configuration —
they read `providers:` and take LITERAL values only, reporting an instance whose
configuration interpolates anything rather than guessing at it. `plan` and `apply` on
an ORPHANED environment (§6.1) are the same path for the same reason. What saves this
is that the instance NAME is recorded in state, so a resource still reaches the right
account whenever that account's own configuration does not depend on an environment.
An instance configured per environment cannot be destroyed by `infra destroy`, and
the diagnostic says so rather than reaching for a default.

**Fail-closed is the engine's guarantee, not each plugin's.** Stage 4.5 refuses to
construct anything when any instance's configuration did not resolve. A plugin that
read a missing value as "use the default" would otherwise turn a broken interpolation
into a silently different account, and a resource created in a place nobody named is
not a problem anyone gets to read about.

A plugin also refuses configuration keys it does not declare. A misspelled `clowd:`
that is quietly ignored means an instance silently sharing another's account, and the
first sign of it is a plan proposing to destroy resources somebody else owns.

### Resource-attribute defaults live under `defaults:`

An instance may also default attributes on every resource that uses it:

```yaml
providers:
  - plugin: aws
    iam-role: some-role
    region: ${aws_region}
    defaults:
      tags: ${tags}
      prevent_destroy: ${protect}
```

Nested rather than mixed in, because at the top level `tags: ${tags}` and
`iam-role: x` are indistinguishable while meaning entirely different things — one
defaults a RESOURCE, the other configures the PROVIDER. The alternative
considered was letting the plugin declare its own config keys and treating
anything else as a resource default; it was rejected because a typo in a config
key (`iam-rol:`) would then silently become a resource default applied to
everything the provider owns.

Attribute resolution gains one rung, between what the resource says and what the
plugin's schema says:

```
explicit on the resource        tags: {team: payments}
        ↓
the instance's `defaults:`      defaults: {tags: ${tags}}
        ↓
the plugin's schema default     whatever the plugin ships
```

A user-authored default beats a plugin-authored one; anything written on the
resource beats both, WHOLE — a map is replaced, not merged, which is why §10.2
has `merge()`.

**A `defaults:` key must be an attribute some resource type of that plugin
declares.** One that nothing declares is an error naming what exists, because
`tag:` for `tags:` would otherwise apply to nothing, in every environment,
forever, with no output in which its absence is visible.

`prevent_destroy` and `retain` are accepted there too, and every resource accepts
those — so the lifecycle key names are RESERVED, and a plugin declaring an
attribute that collides with one is rejected at registration, the same place and
for the same reason the `module.` type namespace is.

A resource that WRITES a lifecycle option beats the block, in both directions.
`prevent_destroy: false` on a resource under an instance defaulting it to true has
to win, which is why `LifecycleDecl` records whether each key was written at all:
in a bare bool, `false` and absent are the same value, and getting it backwards
refuses a destroy the user explicitly allowed — the one direction a user cannot
work around.

**Where each half is decided.** The schema attributes are stage 7's, because they
need a resource definition to check a key and a kind against. The lifecycle options
are stage 6's, because a lifecycle option is not a schema attribute and has nothing
to resolve against a definition. Two stages for one feature, each where its own
thing lives.

**What a plan says.** An instance default resolves as `SourceDefault` with
`ScopeInstanceDefault`, so a plan reads `size: 200 [default, from provider instance
default]` where a plugin's own would read `[default, from provider default]`. The
distinction is worth a Scope constant of its own: one is something the user wrote
and can edit, the other is something the plugin ships.

**The plan artifact records the instance.** `operationWire` gained a `provider` key,
because the artifact is the format §50 reads a saved plan back FROM — and a destroy
read back without its instance would be dispatched to whichever account happened to
be consulted. The key changes the bytes of essentially every artifact, which does not
touch invariant 6 (determinism is "same inputs, equivalent plan", not byte-stability
across builds) and needs no `version` bump because it is additive. Found by a sabotage
and closed with a frozen-keys test, which the artifact had never had.

**Generation omits it**, the same way §27 omits a schema default — it is not
something the reader has to supply. Keyed by the resource's OWN instance, never by
"any instance that defaults this name": two instances exist precisely because they
differ, and trimming a value against the other account's block writes a file that
plans a change the moment it is read back. `export` keeps it, because §28 is the
opposite job.

### `default: true` overrides order

Order is the fallback, not the only mechanism:

```yaml
providers:
  - plugin: aws
    name: main
  - plugin: aws
    name: acct2
    default: true      # wins over being second
```

Two entries both marked is an error, for the same reason two entries with one
name are: there is no precedence to invent, and either choice sends some
resources to the wrong account.

---

# 13. Environment-Aware Defaults — WITHDRAWN

**This section described a feature that has been removed. It is kept, rather
than deleted, because the reasoning is the useful part and because the code
carried it for seven milestones.**

The original idea was that a provider default could vary by environment class:
a `production` environment would get a larger instance, multi-AZ, stronger
backups, without anyone writing that at each resource. `schema.DefaultContext`
carried an `EnvironmentType`, and `environments: { x: { type: production } }`
looked like the way to set it.

It is withdrawn for three reasons, in increasing order of weight.

**It never worked as documented.** `type:` was never a reserved key. The
environment decoder handles `extends` and `variables`; everything else becomes
a variable override — so `type: production` silently declared a VARIABLE named
`type`, reachable as `${type}`, and classified nothing. Classification was done
by matching the environment's NAME against `"production"` and `"prod"`.

**Name-matching is wrong exactly where it matters.** §6 says environments are
cheap and numerous. `prod-eu`, `production-canary` and `staging` all classify
as non-production and silently receive development-shaped defaults — a quietly
wrong value in the environment least able to absorb one.

**It competes with variables, and loses.** §6 fixes ONE mechanism for
environment variation: what the environment's variables say. A provider default
that changes with environment class is a second mechanism for the same job,
invisible in the configuration, discoverable only by reading a plan annotation
or running `explain`. Two mechanisms for one concept is the defect this project
refuses everywhere else.

So: **a provider default is a single value per attribute.** Anything that
should differ between environments is a variable, which is visible in the
configuration, carries provenance, and is already built.

`DefaultContext` keeps `Environment`, `Region`, `Account`, `Project` and
`Type` — facts about the invocation, not a classification of it. `type:` in an
environment block is an ERROR naming what to use instead, because leaving it to
become a silent variable is the trap rather than the fix.

## 13.1 What this does not remove

Production PROTECTION is a separate concern and survives (§20, §38). When it is
built it is declared on the environment directly — `require_approval: true` —
rather than implied by a class. A protection you can read on the environment
beats one inferred from its name.

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

## 21.1 Migrating `test.*` state to `fake.*`

**WRITTEN 2026-09-13** (`internal/state/migrations.go`), version 1 → 2, and the first use
the migration mechanism has ever had. The reasoning below is kept because it is why.

The step rewrites every resource's `type` prefix unconditionally — a type belongs to the
plugin — and the recorded `provider` ONLY where it is `test`, the name the implicit
instance takes from its plugin. A user who wrote `providers: [{plugin: test, name: main}]`
has state recording `main`, and rewriting every provider name would point their resources
at an instance that does not exist. That asymmetry is the whole care in the function.

`testdata/state-v1.json` — the frozen golden a version-1 build actually wrote — is KEPT as
the migration's fixture rather than regenerated, and the golden is now named for the
version it freezes so the next bump is additive. Real historical bytes beat a hand-written
approximation, and there is exactly one chance to keep them. That file also carries a
2^53+1 integer, so the migration test proves the non-lossy decode on a real file.

**Originally recorded as open, with a recommendation:** Renaming the fake provider from `test` to `fake`
renames every resource type it serves, and a state file recording `test.network` names
a type no loaded plugin offers. Today that is refused with "no provider registers that
type" — correct, and a dead end for anyone holding such a file.

**Recommendation: write the migration.** It was the wrong trade twice before and is the
right one now, for a reason that changed underneath it.

`state.Decode` used to route every non-current file through `map[string]any`, rounding
any integer past 2^53 — so M7 and M11 each added a `Scope` without bumping
`CurrentVersion`, because bumping cost silent precision loss on every existing file.
That defect is fixed (`json.Decoder.UseNumber()`), and a version bump is an ordinary
change again. This is the first real reason to make one.

The migration is small: version 1 → 2, rewriting each resource's `type` prefix from
`test.` to `fake.`, and its `provider` from `test` to `fake` where it says `test`. It
is exactly the shape `Migration.Apply` exists for, and it is the case that proves the
mechanism — which has never run in production, since no migration has ever been
registered.

**What argues against it**, recorded because it is not nothing: the fake provider
manages nothing real, so a user who cannot load their state loses a file describing
imaginary infrastructure. Deleting `.infra/state/` costs them nothing. Against that:
the fake provider is what every test suite and every tutorial uses, so the file exists
on many machines, and "delete your state" is a bad first experience of an upgrade —
and the mechanism needs its first real exercise somewhere less frightening than AWS.

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

## 31.1 Provider plugins are separate processes

**Decided 2026-09-12, to be built before Phase 3.** A provider plugin is a separately
distributed binary that `infrata` launches as a child process and talks to over
stdin/stdout. That includes the official plugins: AWS ships the same way a third party's
plugin would, and the fake provider becomes a binary too.

Plugins are written in Go against an SDK this repository publishes. The protocol does not
depend on Go, but nothing is built for any other language, and the protocol uses only the
standard library, so **it adds no third-party dependency**.

### Alternatives rejected

- **Go's `plugin` package (`.so` files).** The host and every plugin must be built with
  the exact same toolchain and the exact same version of every shared dependency. There
  is no Windows support, and a plugin can never be unloaded. For binaries built by other
  people this fails on every release. A panicking plugin also takes the engine down
  mid-apply, while it holds a state lock and possibly after a real resource was created.
- **JavaScript through goja.** A provider's job is authenticated network calls to a cloud
  API. goja has no Node APIs, no `fetch` and no event loop, so it cannot run any cloud's
  SDK. The host would end up re-implementing those SDKs as bindings. It would also make
  JavaScript the only language plugins can be written in.
- **WebAssembly (wazero).** It is pure Go, sandboxed and portable, but WASI networking is
  immature, so the host would have to supply HTTP and TLS: goja's problem again. The
  sandbox also buys little here, because a provider holds cloud credentials by design and
  users choose which plugins they run. Worth revisiting if either of those changes.
- **`hashicorp/go-plugin` with gRPC.** Proven, but it brings in grpc, protobuf and hclog,
  far past a dependency budget that so far holds two libraries. Calls are counted in
  dozens to thousands per run, so gRPC's performance buys nothing measurable.

### Transport

- A plugin named `aws` is the executable `infrata-plugin-aws`.
- The host starts it with `INFRATA_PLUGIN_COOKIE` set to a random value. A binary run by
  hand without that variable prints "this is an infrata plugin; it is run by infrata" and
  exits non-zero. Without the cookie it would sit silently waiting for protocol input.
- **stdout carries protocol messages and nothing else.** Messages are newline-delimited
  JSON: `{"id", "method", "params"}` requests, `{"id", "result"}` or `{"id", "error"}`
  responses, and `{"method": "cancel", "id"}` notifications.
- **stderr is the plugin's log.** The host prefixes each line with the plugin name and
  shows it under `--verbose`. The SDK points `os.Stdout` at stderr before plugin code runs,
  because a stray `fmt.Println` in a plugin would otherwise corrupt the protocol stream.
  That is the classic failure of stdio protocols.
- Requests carry IDs and are multiplexed on one pipe. The SDK serves each request in its
  own goroutine, so `--parallelism` works against one process. Per-provider concurrency
  bounds (§34) stay on the host side.
- **Cancellation is a message, not a kill.** A cancelled context sends `cancel`, the SDK
  cancels that request's `ctx`, and the host still waits for the response. Killing a
  plugin during `Create` can orphan a resource that was really created: the case §31's
  non-nil-on-success rule exists to prevent.

### Handshake and version

The plugin's first message is `{"protocol": 1, "name": "aws", "version": "0.3.1"}`, sent
before any request.

- The host supports a SET of protocol versions and refuses anything else with a §44 error.
  The error names the plugin and the path it was loaded from, gives both sides' versions,
  and suggests which one to upgrade.
- `name` must equal the `plugin:` that selected the binary. A renamed or mis-copied binary
  otherwise serves the wrong schemas, and the first sign of that is a plan.
- **The protocol version is the compatibility contract, not the Go types.** A plugin built
  against an older SDK keeps working for as long as its protocol version is supported. So
  `pkg/pluginproto` changes additively, and any removal bumps `protocol`.

### Methods

| Method | Carries | Replaces |
| --- | --- | --- |
| `schemas` | nothing → `[]ResourceDefinition` | `Plugin.Definitions` |
| `configure` | instance, config, project dir → handle | `Plugin.New` |
| `read` / `create` / `update` / `delete` | handle + resource | same `Provider` methods |
| `discover` / `import` | handle + request | same `Provider` methods |
| `shutdown` | nothing | — |

- **The two-step registration survives unchanged.** `schemas` feeds `RegisterPlugin`
  before anything is read. `configure` is what stage 4.5 calls for `RegisterInstance`.
  The cycle §12.1 breaks stays broken, and state-only commands still pass literal
  configuration only.
- `configure` passes the project directory because a plugin can no longer be handed one
  when it is constructed. The fake provider needs it: its `cloud:` defaults to a file
  under the project.
- **An error carries its own classification:** `{"message", "retryability"}`.
  `ClassifyError(err error)` cannot cross a process boundary, because an `error` value does
  not serialise. The SDK calls the plugin's `ClassifyError` on the plugin's side and sends
  the answer. The host rebuilds a typed error, and §35's classification reads that
  instead of calling into the provider.

### Process model

- **One process per plugin per command, not one per instance.** Two AWS accounts means one
  `infrata-plugin-aws` holding two configured clients, addressed by handle.
- The host launches the plugins named in `providers:`, and only those. A resource in state
  whose instance names no entry is already an error before any plugin is needed.
- **Every command launches plugins**, including `validate` and `explain`, because each
  needs schemas. That is the main cost of this design; see below.
- At the end of a command the host sends `shutdown`, closes stdin, waits a grace period,
  then kills.
- **A plugin that exits unexpectedly** fails every pending call with an error classified
  `NotSafeToRetry`. The error message includes the last lines of the plugin's stderr. The
  executor treats it like any other failed operation. State is written incrementally
  (§34), so everything that completed before the crash is recorded.

### What the engine stops trusting a plugin with

Today several guarantees rest on each provider following a comment. A binary somebody
else built cannot be held to a comment. Each of these moves to the host adapter
(`internal/pluginhost`), so it holds for every plugin, the fake one included:

1. **Bookkeeping is never sent, so it cannot be dropped.** The host sends `type`,
   `provider_id` and `attributes`. It re-attaches `Address`, `Provider`, `Dependencies`,
   `Lifecycle`, `CreatedAt` and `UpdatedAt` to whatever comes back. §31's carry-forward
   contract on `Read` stops being a plugin obligation. Losing `Lifecycle` makes a
   `prevent_destroy` guard vanish silently, and that is too important to delegate.
2. **Sensitivity is forced from the schema.** A value the host receives is sensitive if
   the plugin says so OR the schema declares the attribute sensitive. A plugin that
   forgets the flag must not be able to put a password into a report or a generated file
   (§36).
3. **Provenance is the host's.** A plugin sends kind, known, raw value and sensitivity.
   The host sets `Source` and `Scope` on returned values, so a plugin cannot claim a value
   was written by the user.
4. **`(nil, nil)` from `create` or `update` becomes an error.** The error says the
   resource may exist untracked, instead of being read as "nothing happened".
5. **A returned attribute the schema does not declare is an error**, not something
   silently persisted into state.
6. **Schemas are validated on load**, through the same `Definition.Validate`, reserved
   lifecycle key names and `module.` namespace check `RegisterPlugin` already applies.
   One new rule: **a plugin's types must be prefixed with its own name**. Plugin `aws`
   serves `aws.*`, so two plugins cannot both claim a type, and a type name says where it
   came from.

The host adapter wraps the in-process fake provider too (see testing below), so each rule
gets its sabotage test once, against a deliberately misbehaving plugin.

### Schemas become data

`schema.Attribute` holds three functions, and none of them can cross a pipe:

| Field | Today | Becomes |
| --- | --- | --- |
| `Default DefaultFunc(DefaultContext)` | the fake provider returns constants | a static datum |
| `Validate func(value.Value) error` | unused by any provider | removed |
| `ImportSpec.Parse` | unused, "may be nil until Phase 2" | removed |

- **A default is a value, not a function of context.** `DefaultContext` carries
  environment, region, account and project. It existed for environment-class defaults, and
  §13 withdrew those. A value that really varies by region or account is either a variable
  (the user decides) or a computed attribute (the provider reports it).
- **Validation, if a plugin needs it, is declarative:** an enum, a range, a pattern,
  checked by the host in stage 7. It is added when the first real attribute needs it. A
  per-attribute call into the plugin would put a round trip inside the compiler for every
  value.
- **Parsing an import ID is `import`'s job.** The plugin reports a malformed ID as an
  error from `import`.

**This change comes first and is in-process.** It makes the existing interface
serialisable before any transport exists. The protocol is then only a transport for an
interface that already needs nothing else, and a failing test at that step is about
schemas, not pipes.

### Finding a plugin: path first, installing later

**Phase A (this milestone).** No downloading.

- The binary is looked up, first match wins, in:
  1. `--plugin-dir` and `INFRATA_PLUGIN_PATH`
  2. `<project>/.infra/plugins/`
  3. `~/.local/share/infrata/plugins/`
  4. `$PATH`
- `--verbose` prints the path each plugin was loaded from.
- A missing plugin is a §44 error naming the `plugin:` entry, every place searched, and
  where to put the binary.
- A project may constrain versions:

  ```yaml
  plugins:
    aws: ">= 0.3.0, < 0.4.0"
  ```

  The constraint is checked against the version the handshake reports. It is optional:
  an unconstrained plugin runs whatever version is found, and `--verbose` says which. It
  is a map keyed by plugin, not a key on each `providers:` entry, because two instances of
  one plugin share one process and so necessarily share one version.

  **BUILT 2026-09-13.** Three rules, each with a reason it is not the obvious thing:

  - **The LOADER enforces it**, not the callers. Four paths load plugins — a compile,
    the state-only commands, `explain`, and discovery's load-everything — and a
    constraint checked in three of them is a constraint nobody can rely on.
  - **A plugin reporting no version satisfies nothing**, and gets its own message. The
    SDK answers `0.0.0` when a plugin does not implement `Version()`, so it fails any
    constraint above that — correct, and useless said as "0.0.0 does not satisfy
    >= 0.3.0", which sends an author looking for a version they never set. Note the
    ASYMMETRY with §61.2's `infrata:` floor, which EXEMPTS a 0.0.0 build: there the
    unversioned binary is the user's own development build and the complaint is not
    actionable; here it is a third-party plugin they installed, and it is.
  - **A constraint on a plugin the project does not use is an ERROR**, listing the ones
    it does. Same reasoning as a `defaults:` key nothing declares (§12.1): it pins
    nothing, in every environment, forever, and the user believes they have pinned a
    version. Checked in the compiler rather than the loader, because only a compile
    knows the whole set a project uses.

  A refused plugin is shut down rather than left running: the command is going to fail,
  and leaving a child to be reaped at exit is how a refusal becomes a hang on a plugin
  that ignores stdin closing. The refusal is cached, so a project naming one bad plugin
  on twenty resources hears about it once.
- **`plugins:` is a new top-level key**, so it is a configuration-language change (§58).
  It is additive, and a project without it behaves exactly as today. Constraint syntax is
  deliberately small: comparison operators on `MAJOR.MINOR.PATCH`, comma meaning AND,
  parsed by hand rather than by a semver library.

### The repository stays PRIVATE until feature complete

**Ruled 2026-09-13.** `github.com/infrata/infrata` is not a fetchable module and will not
be until the product is feature complete. Requested by the fake-provider port, decided
against for now; do not re-raise it as a blocker.

What that costs, so nobody re-derives it:

- **There are no third-party plugin authors yet**, and cannot be. A plugin needs either a
  checkout of a private repository or `GOPRIVATE=github.com/infrata/*` plus credentials to
  the org. `AGENT.md`, §31.2 and the reference plugin all exist to invite outside plugins,
  and that invitation is on hold rather than withdrawn.
- **The one consumer uses `replace`.** `infrata-provider-fake` carries
  `replace github.com/infrata/infrata => ../infrata`, so every contributor needs a sibling
  checkout named `infrata` — the directory a clone produces — and its release workflow needs
  a token to fetch this repository beside it. It said `../ilan` until 2026-09-13, a local
  folder name no clone creates, which broke CI the first time it needed the plugin.
- **A `replace` means that repository builds against a WORKING TREE, not a version.** Its
  tests run against whatever is uncommitted here, which is how it saw a stale
  `internal/semver` that had been moved. That is a fast loop while both repositories change
  together daily, and a correctness hazard once they do not.

  **v0.1.0 (2026-09-13) is the exit.** There is now a tag to require, so a plugin repository
  can drop the `replace` in CI — `GOPRIVATE=github.com/infrata/*` plus a token, requiring the
  released version — and keep it only for local work. Until a release existed this was not
  available at any price: every build reported `0.0.0-dev`.

**A semver tag is worth cutting anyway**, and is independent of visibility: with
`GOPRIVATE` set, a tagged version lets the plugin `require github.com/infrata/infrata
vX.Y.Z` and drop the `replace`, which removes the sibling-checkout requirement and pins
the build to something reproducible. It also makes `infrata version` report a real version
instead of `0.0.0-dev` (§61.1).

**Revisit when:** the product is feature complete. That is the stated gate, and going
public is the only thing that makes an outside plugin author possible.

**Phase B (later, §53).**

- `infrata plugins install` fetches release binaries.
- A committed `plugins.lock` records the resolved version and a SHA-256 per platform.
- The host verifies the checksum on every launch once a lock file exists.
- Phase A's search path is where install writes, so nothing moves.

## 31.2 The plugin manifest: `plugin.yaml`

**Agreed 2026-09-13**, from a proposal by the `infrata-provider-fake` port
(`docs/proposals/2026-09-13-plugin-manifest.md` in that repository), amended as below.

A plugin repository ships one file saying what the plugin is and what it works with.
Its PURPOSE decides its whole shape: it is fetched over HTTP and read **before any
binary is downloaded**, so that a search can answer "is this compatible with what I am
running, and is there a build for my machine" without fetching anything else.

```yaml
# plugin.yaml
manifest: 1
name: fake
version: 0.1.0
protocol: [1]
platforms: [linux/amd64, linux/arm64, darwin/arm64, windows/amd64]
description: A fake provider for testing infrata without a cloud account.
infrata: ">= 0.2.0"
source: https://github.com/infrata/infrata-provider-fake
```

| Key | Required | Meaning |
| --- | --- | --- |
| `manifest` | yes | the format version of THIS FILE. Checked first, before any other key. |
| `name` | yes | the plugin's name: the binary is `infrata-plugin-<name>`, `Plugin.Name()` returns it, and every resource type is prefixed with it. The host already refuses a mismatch between the last two. |
| `version` | yes | `MAJOR.MINOR.PATCH`, and it must equal the tag this file is read at. |
| `protocol` | yes | every plugin protocol version the plugin can speak, as a list, because the host accepts a SET (`pluginproto.Supported`). |
| `platforms` | yes | `GOOS/GOARCH` for every published build. |
| `description` | yes | one line, for a search result to show. |
| `infrata` | no | the infrata releases this plugin is known to work with, in `pkg/semver`'s syntax. ABSENT means unconstrained. |
| `source` | no | where the plugin lives, for a search result to link. |

### READ IT AT THE TAG, never at the default branch

The file at the root of the default branch describes UNRELEASED code, so reading it to
judge a released version answers the wrong question — `v0.3.1` gets judged by a manifest
that may already describe `v0.4.0`. And that is the mistake an implementer makes by
default, because `raw.githubusercontent.com/<owner>/<repo>/HEAD/plugin.yaml` is the
obvious URL.

```text
raw.githubusercontent.com/<owner>/<repo>/refs/tags/v0.3.1/plugin.yaml
```

The manifest answers two questions with different lifetimes — IDENTITY (`name`,
`description`, `source`), which is the same on every ref, and THE COMPATIBILITY OF ONE
VERSION (`version`, `protocol`, `platforms`, `infrata`), which differs per release. One
file serves both only because it is always read at a tag.

### Compatible means three things

1. **Format:** `manifest` is a version this build understands. Checked FIRST, so a newer
   manifest reports "this plugin needs a newer infrata to describe itself" rather than a
   parse error about a key nobody recognises.
2. **Protocol:** `protocol` shares at least one version with the build's own
   `pluginproto.Supported`. Already enforced at runtime by the handshake.
3. **Release:** `infrata`, if stated, allows the running build. A development build is
   EXEMPT, the same exemption §61.2 gives a project's own floor and for the same reason.

### Why the format is versioned, when configuration is not

§61.2 argues AGAINST versioning the configuration language, and this is the other case.
The distinction is who reads the file and when:

- Configuration is written and read by the same person at the same time, on one machine,
  and fails closed on an unknown key — which catches their typo.
- A manifest is written by a plugin author and read by every infrata build for years
  afterwards, over the network, with no way to upgrade the reader in step with the
  writer. Refusing unknown keys there means a 2026 infrata cannot install a 2027 plugin.

So: fail closed on unknown keys for a `manifest` version this build knows, and
tolerate-with-a-warning for one it does not.

### What the manifest deliberately does not carry

- **Checksums.** They cannot exist until after the build, so a hand-written,
  checked-in manifest cannot carry them honestly. `SHA256SUMS` is published as a release
  asset (infrata's own release workflow already does this), and §31.1 Phase B's
  `plugins.lock` is what records them per platform.
- **Asset names or download URLs.** A CONVENTION instead, mirroring infrata's own
  releases: `infrata-plugin-<name>_<version>_<goos>_<goarch>.tar.gz`, `.zip` on Windows.
  Install constructs the URL. One convention beats a field every author can get wrong.
- **Resource types.** `name` already implies them — a plugin serves `<name>.*` and the
  host refuses anything else — so "which plugin provides `aws.instance`?" is answerable
  from `name` alone.

### Where infrata reads it

**At install (Phase B).** `infrata plugins install` reads the manifest from the tag,
refuses a plugin failing any of the three rules, and only then fetches and checksums the
binary. A plugin with NO manifest installs with a warning rather than being refused:
Phase A is hand-placed binaries, which is every plugin today.

**Not in the handshake, for now.** That would catch a hand-placed binary too, and the
cheap form needs no manifest embedding — the handshake already sends
`{protocol, name, version}`, so `infrata` is one more optional string supplied the way
`Version()` is. Deferred because with the protocol at 1 and one plugin in existence,
rule 3 has nothing to catch yet. Recorded so it is not re-derived.

### What a plugin repository owes its own manifest

Its release workflow must assert that THREE things agree: the git tag, the manifest's
`version`, and the binary's `Version()`. That is the shape of the check already in
infrata's own release workflow, which builds for the host and refuses to publish a binary
that does not report the tag. A drift test between the manifest and the code is the
weaker substitute — it is a test someone can delete, where the release assertion blocks
the release.

`pkg/semver` is public so a plugin can validate its own `infrata:` field with the same
parser infrata will check it with, rather than a second implementation that drifts.

**`pkg/pluginmanifest` is that parser for the whole file** (built 2026-09-13, requested by
the port). `Parse` returns a `Manifest` and any warnings; `Validate` checks one built in
Go; `SpeaksProtocol`, `Supports` and `AllowsInfrata` are §31.2's three compatibility
rules, so the installer and a plugin's own test ask the same question of the same code.

It is public for the reason `pkg/semver` is, plus one practical one: reading YAML needs a
YAML parser, and a plugin repository whose rule is "standard library plus infrata" cannot
add `gopkg.in/yaml.v3` itself — it accepts it transitively by importing this instead.

That does not breach "internal/config is the only place in the engine permitted to touch
`yaml.Node`": this package touches no `yaml.Node`, decoding into typed structs so no
untyped document flows anywhere. Project configuration and a plugin manifest are different
documents with different readers, and each has exactly one door.

**The format version is read in its own lenient pass, before anything else.** A manifest
from the future carries keys this build has never heard of, and none of them may stop it
answering "which format is this?" — the same probe-then-decode shape `state.Decode` uses
for its own version.

---

### Where the code lives

```text
pkg/pluginproto/        message types, protocol version: the contract
pkg/pluginsdk/          Main(p): what a plugin's main() calls
pkg/plugintest/         the in-process harness a plugin's OWN tests use
pkg/semver/             the constraint syntax, public so a plugin can check its manifest
internal/pluginhost/    launch, handshake, client, the trust rules above
providers/test/         the engine's TEST DOUBLE (see below) — not a shipped provider
```

**No provider ships inside this module, and no plugin belongs under this module's import
path.** `providers/aws/` was listed here as "its own Go module (Phase 3)"; that is
withdrawn — see the amendment below.

**Amended 2026-09-13.** This section previously said `cmd/infrata-plugin-test/` — the
fake provider as a binary inside this repository — and `providers/test/` unchanged.
Neither is what happened, and the difference is deliberate.

**The fake provider gets its OWN REPOSITORY**, `infrata-provider-fake`, building
`infrata-plugin-fake`. It has two jobs, and the second is why it moved out: it is the
only plugin whose source anyone can read, so it is also the reference implementation
every plugin author copies. A plugin living inside the engine's module can quietly
depend on something an external author cannot have — an internal package, a shared
test fixture — and nobody would find out until the first third-party plugin failed.
Outside, if it compiles, the dependency is one an outside author has too.

That is not a hypothetical: the first thing the port found was that
`internal/pluginhost.InProcess` is unreachable from another module, while the
authoring guide recommended testing against it. `pkg/plugintest` exists because of it.

**Amended 2026-09-13: AWS gets its own repository too**, `infrata-provider-aws`, building
`infrata-plugin-aws`. This section previously put it at `providers/aws/` inside this
repository with its own `go.mod`, on the reasoning that a separate module is enough to keep
the AWS SDK out of the core module's dependency budget. It is enough for that, and not
enough for the thing that actually matters, because **Go's internal rule is by import path,
not by module boundary.**

Measured, not assumed, on a scratch copy at `cd51fb7`: a nested module
`github.com/infrata/infrata/providers/awsprobe` with `replace => ../..` COMPILES while
importing `github.com/infrata/infrata/internal/pluginhost`, because the importing path sits
under the parent of `internal/`. The identical file in a module named
`example.com/outsideprobe` fails with `use of internal package
github.com/infrata/infrata/internal/pluginhost not allowed`. So the official provider —
the one whose code every AWS user reads and every third-party author imitates — would have
been the single plugin able to reach engine internals, and the compiler would never have
said so.

A separate module under this module's path also keeps the `replace` problem: it compiles
whatever is in the engine's working tree, committed or not, so its green suite proves
nothing about committed infrata.

**The rule this generalises to: a plugin's module path must not be under
`github.com/infrata/infrata/`.** That is what puts an official plugin on exactly the footing
a third-party plugin has, which is the only way the plugin API is tested by the plugins we
write ourselves.

**`providers/test` was a BUILTIN until that binary shipped; it is now the engine's TEST
DOUBLE.** While it was a builtin, the loader preferred a binary on the search path and
fell back to it, served over `pluginhost.InProcess` — the same handshake, protocol and
trust rules a subprocess gets, so a fallback rather than a second code path. The binary
shipped, and the fallback went with it: a shipped infrata now carries NO provider, `init`
scaffolds `fake.*`, and the double is injected only by `internal/cli`'s `TestMain`.
`TestAShippedBuildCarriesNoProvider` runs the binary with no plugin installed and is the
only test that can make that claim.

**Nothing names a plugin outside configuration.** A `providers:` entry's `plugin:`, a
resource type's prefix, or a type recorded in state: those are the three things that
cause a plugin to load, because they are the three ways a project says it uses one.
`explain <type>` therefore works with no project at all, and `discover` — the one
command whose scope configuration does not set — asks every plugin available.

### Testing

**One code path, two ways to connect it.** `pluginhost.InProcess(p)` runs
`pluginsdk.Serve(p)` on one end of an in-memory pipe and the host client on the other.
`pkg/plugintest` is the same thing with a public door, for a plugin's own tests in its
own module.

- Unit tests and the fast integration suite register the fake provider that way. Every
  call is encoded, decoded and passed through the trust rules without starting a process.
- Launching a subprocess is the only thing a separate, smaller suite adds. That suite
  builds `infrata-plugin-fake` from its own repository in `TestMain` and runs §48's
  workflow against the binary. Still outstanding: it is the only place the SDK's
  `os.Stdout` redirect is observable, because in process `Serve` writes to the pipe it
  is given and a plugin's `fmt.Println` goes somewhere else entirely.
- There is deliberately no path that skips the host adapter. A second path is where the
  trust rules would silently stop applying.

Tests this section requires, each with a sabotage proving it can fail:

- A plugin that drops `Lifecycle` from `read` still has its `prevent_destroy` enforced.
- A plugin that returns a schema-sensitive value unflagged still shows `<sensitive>`.
- A plugin that writes to stdout does not corrupt the session.
- A plugin that exits during `apply` leaves state recording exactly the operations that
  completed, and the error shows the plugin's last stderr lines.
- A cancelled apply waits for the in-flight `create` and records its result.
- A handshake with the wrong protocol version, the wrong name, or a version outside
  `plugins:` is refused with a message naming what to change.
- A plugin returning an undeclared attribute, or a type outside its own prefix, is refused.

### What this costs, recorded before it is built

- **Every command starts processes**, including `validate`, `explain` and `graph`. A plugin
  with an expensive startup, like AWS SDK credential resolution, pays it on each run. The
  fix is caching schemas keyed by the binary's SHA-256, and it is deferred until someone
  measures it being slow.
- **`init` scaffolds a project that uses the fake provider**, so once the builtin is
  deleted a fresh install needs `infrata-plugin-fake` next to `infrata`. Releases ship
  both. Until then the builtin means a fresh install needs nothing.
- **`pkg/*` becomes something other people compile against.** Changing those packages now
  has users outside this repository, even though the wire protocol is the real contract.
- **Debugging crosses a process boundary.** `--verbose` plugin logs and the stderr tail on
  a crash are the minimum that keeps this bearable.

### Build order

1. Schemas become data: remove the three function fields, in-process, all tests green.
2. Trust rules move into a host adapter wrapping in-process providers, each with its
   sabotage test.
3. `pkg/pluginproto`, `pkg/pluginsdk`, `internal/pluginhost` over the in-memory pipe. The
   registry registers every plugin through the host.
4. Subprocess launch, cookie, handshake, search path, `plugins:` constraints, stderr
   forwarding, crash and cancel handling.
5. **DONE 2026-09-13.** `infrata-provider-fake` builds `infrata-plugin-fake` (v0.1.1, 8
   platforms); the state migration landed (§21.1); the `init` scaffold and `examples/shop`
   use `fake.*`; and **a shipped infrata carries no provider at all.**

   How the test suites divide, which is the part worth knowing:

   - **tests/integration builds and runs the REAL plugin** from the sibling repository,
     with `--plugin-dir`. So the path a user takes — a subprocess, a cookie, a handshake,
     stderr forwarding, the host's trust rules over a real pipe — is proved somewhere, and
     `TestAShippedBuildCarriesNoProvider` runs the binary with NO plugin installed to prove
     the builtin is really gone. That test exists because every other suite injects the
     double, so a build that regained a compiled-in provider would pass all of them.
   - **Every in-process suite injects the fake double** through `internal/cli`'s TestMain,
     which is what §31.1's Testing section always said: the unit and fast suites register
     the fake provider over `pluginhost.InProcess`.
   - `providers/test` SURVIVES as the engine's test double, not as a shipped provider. It
     shares an origin with infrata-plugin-fake only because that binary was ported from
     it, and what keeps the two honest is that tests/integration runs the real one.

   CI checks out both repositories and sets `INFRATA_REQUIRE_PLUGIN`, which turns the
   integration suite's skip into a failure: a run that silently skips its integration suite
   reports green for tests that never executed. It needs a `PLUGIN_REPO_TOKEN` secret,
   because the plugin repository is private.
6. Documentation: `explain` and `validate` errors for a missing or incompatible plugin, and
   an authoring guide for the SDK.

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
environments:
  production:
    require_approval: true
    prevent_destroy: true
```

**Declared on the environment, not implied by a class.** An earlier draft of
this section wrote `type: production` and let the protections follow from it.
§13 withdraws that: environment classification by name or by a `type:` key is
gone, and a protection you can read on the environment beats one inferred from
what it is called. `prod-eu` protects itself by saying so.

Neither key is implemented yet. `lifecycle.prevent_destroy` on a RESOURCE is
built and works; this is its environment-wide sibling, and `require_approval`
has no implementation at all.

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
  aws/          # its own go.mod (§31.1)

tests/
  integration/
```

Keep the core engine independent of AWS.

Provider plugins run as separate processes (§31.1), which adds `pkg/pluginproto`
(the wire contract), `pkg/pluginsdk` (what plugin authors import),
`internal/pluginhost` (launch, handshake, and the rules the engine no longer trusts a
plugin with), `pkg/plugintest` (the harness a plugin's own tests use), and
`infrata-provider-fake` — a separate repository — for the fake provider as a binary.

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

## 50.1 Before Phase 3: provider plugins as processes

AWS is the first provider nobody would put in the core binary, so the plugin protocol
comes BEFORE it. Otherwise AWS gets written in-process and ported afterwards. §31.1
specifies it, including its own build order:

1. Schemas become data.
2. The trust rules move into a host adapter.
3. Protocol and SDK, over an in-memory pipe.
4. Subprocess, handshake and search path.
5. The fake provider as a binary.
6. Documentation.

Phase 3 then starts with AWS as a plugin from its first commit, in its OWN REPOSITORY —
`infrata-provider-aws`, building `infrata-plugin-aws`. See §31.1's amendment of
2026-09-13 for why it is not `providers/aws/` inside this repository.

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

# 61. Versioning

**Decided 2026-09-13.** Six format versions already exist, independently, each at 1:

| Version | Package | Guards | How it moves |
| --- | --- | --- | --- |
| `state.CurrentVersion` | `internal/state` | the state file | a migration chain, one step per version |
| `pluginproto.Version`, `Supported` | `pkg/pluginproto` | the plugin wire | negotiated per plugin; `Supported` is a SET |
| `planner.PlanVersion` | `internal/planner` | the plan artifact | additive, with a frozen-keys test |
| `report.Version` | `pkg/report` | `--output` reports | additive |
| lockfile `Version` | `internal/modules/source` | `modules.lock` | internal |
| cache `Version` | `internal/modules/source` | module cache metadata | internal |

**They stay independent, and that is the decision.** Each guards a different boundary
with a different lifetime, and one shared number would mean a state migration every
time a report gained a field — a migration being the most expensive thing in the list.
Nothing may collapse them.

## 61.1 The product version

`infrata` itself is SEMVER, `v0.x.y` until the configuration language stops moving.
It is not a seventh format version; it is the release, and its bumps are DEFINED in
terms of the table above, because otherwise "minor" means nothing:

- **patch** — no format version changes.
- **minor** — may ADD a format version, and must still read every older one. May add
  configuration syntax. May add a `pluginproto` version while leaving the previous one
  in `Supported`, so existing plugins keep working.
- **major** — may drop support for an old format version, or remove configuration
  syntax. A plugin may stop working, and the handshake says so by name.

**It is not a constant in the source.** A hand-maintained version is wrong by the
second commit after a release. `runtime/debug.ReadBuildInfo()` reports the module
version for `go install`, and `-ldflags -X` overrides it for a tagged build; a
development build says so rather than claiming a release it is not.

## 61.2 The configuration language is not versioned

Considered and REJECTED: a `config_version: 2` key, or Terraform's
`required_version`-as-format-number.

The language is already FAIL CLOSED — an unknown lifecycle option, an unknown provider
configuration key, a `defaults:` key nothing declares are each an error naming the key.
So an older binary meeting newer syntax already stops safely and says what it choked
on. A version integer would buy exactly one thing on top of that: a better message.
And it would cost a key in every project file which is wrong by default, because the
person adding a feature is not the person who remembers to raise it.

**Instead, an OPTIONAL floor**, reusing the constraint syntax `plugins:` already needs:

```yaml
infrata: ">= 0.4"

plugins:
  aws: ">= 0.3.0, < 0.4.0"
```

Comparison operators on `MAJOR.MINOR.PATCH`, comma meaning AND, parsed by hand rather
than by a semver library (§31.1). Absent means no constraint, so every project written
before this key behaves exactly as it did. Present, it turns "unknown key `foo`" into
"this project needs infrata >= 0.4; this is 0.3.1", which is the message a team
sharing a repository between CI and laptops actually needs.

**Checked immediately after decoding**, before anything else runs: a binary that cannot
understand a project must say so once, rather than reporting twenty unknown-key errors
that are all the same problem.

**What it does not cover, recorded rather than hidden:** the check lives in the
compiler, so the four commands that never compile — `destroy`, `refresh`, `discover`,
`import` — do not apply it. They barely read configuration, which is why they are also
the commands least likely to meet syntax they cannot parse. Revisit if that stops being
true.

## 61.3 There is no separate plugin SDK version

`pkg/pluginsdk` is Go code a plugin author compiles against, and `pkg/pluginproto` is
the wire. **The protocol version is the compatibility contract, not the Go types**
(§31.1), so:

- a plugin built against an older SDK keeps working for as long as its protocol version
  is in `Supported`, and never needs rebuilding for an infrata release;
- the SDK's Go API rides the module's own semver, which is what a plugin's `go.mod`
  pins, and which therefore follows §61.1's rules like any other package.

Two numbers, already present, doing different jobs. A third — an "SDK version" — would
have to agree with one of them, and would eventually not.

## 61.4 `infrata version`

Nothing currently tells a user, or a bug report, which formats a binary speaks:

```text
$ infrata version
infrata 0.4.1 (a1b2c3d, go1.24.13, linux/amd64)

formats
  state             1
  plugin protocol   1
  plan artifact     1
  report            1
```

`--output json` emits the same thing as one object, so a CI job can assert on it. This
is the artifact that makes "which version do I need" answerable without reading source,
and it is why the format versions are exported rather than package-private.

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
