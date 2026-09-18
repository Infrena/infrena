# Infrastructure Management Tool

## Product & Implementation Specification

### Name

The product is named **Infrena**, with the GitHub organization `github.com/infrena`. The command is `infrena`, installed with `go install github.com/infrena/infrena/cmd/infrena@latest`.

**RENAMED 2026-09-14, from Infrata**, after a legal collision on that name. The etymology this
line used to give — *infra* + *strata* — belonged to the old name and is not carried over.

What the rename moved: the module path (`github.com/infrata/infrata` →
`github.com/infrena/infrena`), the CLI binary, the plugin binary the host searches for
(`infrata-plugin-<name>` → `infrena-plugin-<name>`), the `INFRATA_*` environment variables, a
project's `infrata:` version floor, and `plugin.yaml`'s `infrata:` field — the last being a
manifest FORMAT change, so `pluginmanifest` goes to 2.

What it did NOT move, which is more than expected: `.infra/`, so every existing state file keeps
working; resource type prefixes, since a prefix is the plugin's name and not the product's; and
every `pluginproto` message shape.

**Releases v0.1.0 to v0.3.0 were published as `infrata`** and declare the OLD module path
permanently. They are not reachable as `github.com/infrena/infrena`, and no amount of GitHub
redirecting changes what a `go.mod` inside a tag says. v0.4.0 is the first release under the new
name, and a plugin moves to it directly rather than bumping.

Because the plugin BINARY NAME changed too, an infrata-era plugin and an infrena-era host can
never meet — the host does not look for the old name. There is no mixed pair to support, which is
what makes this a cutover rather than a migration.

The Go module is `github.com/infrena/infrena` and the binary is `cmd/infrena`; the rename landed 2026-09-11, between M4 and M5.

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

## 4.2 Where a project is found

A command looks in exactly two places, **closest first**: `./infra.yml`, then
`./infrena/infra.yml`. The first that exists is the project root. A directory holding both is
a project at its root that also happens to have a directory called `infrena`, and the root is
the answer. This is what lets infrastructure live beside the application without every command
needing a flag.

**The search NEVER walks up, deliberately.** Walking up means a command run deep in a tree
silently operates on a project the user may not have realised they were in, and state mutation
is the wrong place for that kind of convenience. A directory with no project of its own is an
error naming both places that were looked in and pointing at `infrena init`, per §44.

**An explicit `--chdir` skips the search entirely.** The flag means what it says, and a user
who named a directory has already answered the question.

`init` skips the search outright, since it runs where no project exists yet. `version`,
`explain`, `discover`, `help` and `completion` are searched for like anything else but are not
refused when nothing is found: they are legitimately projectless.

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
- **The value may be an expression.** `only: ${var.replica_environments}` resolving
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
      only: ${var.replica_in}
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

**`region` and `account` were in this table and are REMOVED (2026-09-13).** They were
listed as coming from `--region` and `--account` "when supplied" — flags that were never
built. `compiler.Options` carried both fields, `seedProcessVariables` read both, and
nothing ever assigned either, so `${var.region}` reported `undefined variable "region"` in
every project that ever ran. Being reserved on top of that was worse than inert: a
project declaring its own `region` variable got "undefined variable" at the use site
instead of "variable is not set" at the declaration, and `--var region=...` was refused
outright with a message about the value being "silently discarded in favour of the
engine's own value" — describing a mechanism that did not exist.

They are ordinary variable names now, which is what the agreed AWS model wants anyway:
every regional type declares `region` as Required and ForceNew, and the instance supplies
it through `defaults:`. If a flag is ever wanted, it goes in as a flag with the plumbing
attached, not as a table entry promising one.

`project` is here because a resource name or a tag almost always wants it, and
threading it through as an ordinary variable makes every project declare the
same line.

**Both are read as `${var.environment}` and `${var.project}`, the same as any
other variable** (§10.5). There is no separate spelling for a process
variable; the prefix applies to every variable without exception, because a
prefix that applies to most of them is the kind of exception nobody
remembers to check for.

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
    name: ${var.project_name}
    replicas: ${var.replicas}
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

Interpolation names exactly one of two things, and which is which is never
counted from the number of segments (§10.5): a declared variable, always
written `${var.x}`, or an attribute a provider assigns, written
`${resource.attribute}`.

```yaml
name: ${var.project_name}-${var.environment}
```

Resource references:

```yaml
database_url: ${database.connection_string}
```

A small set of pure helper functions may eventually be supported:

```yaml
name: ${lower(var.project_name)}-${var.environment}
```

Do not initially build a Terraform/HCL-like programming language.

Expressions should remain intentionally constrained.

## 10.1 Interpolation inside a composite value

An interpolation may appear in a string leaf of a list or a map, not only in a
bare string:

```yaml
tags:
  environment: ${var.environment}
  project: ${var.project}
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
- A key is never interpolated. `${var.x}: y` is not a thing; keys are literal, so a
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
tags: "${merge(var.tags, {team: payments, project: billing})}"
```

Not as a value on its own, because YAML already does that job. Bounding it to
argument position is what keeps this from being the first step toward a
programming language.

**The quotes are required, and not by us.** YAML itself rejects the unquoted
form: a plain scalar may not contain `: `, so `tags: ${merge(var.a, {b: c})}` fails
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

**§10.5's `[index]` introduces no new token.** `splitArgs` already nests
brackets (`open: "({["`, `close: ")}]"`) for §10.3's list literals, so a `[` in
a reference path is a shape `matchBrace`/`splitArgs` already track. Reading a
path is a change to `parseReference`, not to the scanner.

## 10.5 The var namespace, paths and indices

Six forms, each with exactly one meaning, and no rule that depends on counting
segments:

```yaml
${var.region}          a variable
${var.tags.team}       a path into a map variable
${var.azs[0]}          an entry of a list variable
${vpc.id}              an attribute of resource `vpc`
${vpc.tags.Name}       a path into a resource attribute
${vpc}                 the resource `vpc` itself, projected to whichever attribute
                       the plugin declared it refers to (§14.3)
```

Before this, a variable and a resource attribute were told apart by COUNTING
SEGMENTS — one is a variable, two or more is a resource attribute
(`internal/expressions/parse.go`). That single rule meant a map variable could
never be indexed (`${tags.team}` was already spoken for as a resource
reference), meant the error for a variable shadowed by a same-named resource
lied about which one existed, and meant `${vpc.tags.Name}` constructed a
target — a resource named `vpc.tags` — that `checkResourceName` forbids anyone
from ever declaring. Counting segments could not be patched into correctness;
it had to be replaced.

**`var` is reserved as a resource name.** A resource so named would make
`${var.x}` mean two things, and the error is reported at the DECLARATION, not
at the reference, because that is where the fix is. A VARIABLE named `var`
stays legal — `${var.var}` is unambiguous, and reserving it would be a rule
with no cause.

**Process variables carry the prefix too:** `${var.environment}`,
`${var.project}` (§6.3). This is what makes the rule total — no bare name
anywhere resolves to a variable — and a prefix applied to some variables and
not others is the kind of exception nobody remembers to check for.

**A bare single segment parses as a WHOLE-RESOURCE reference**, not a variable and
not a parse error — `${vpc}` becomes `value.OpResourceRef` with an empty attribute,
and stage 6 fills the attribute in or reports why it cannot (§14.3). This replaced
an earlier design, kept here because the failure it describes is still worth
knowing: `${vpc}` used to be an unconditional parse-time error —

```
${vpc} is not a reference.

Variables are written ${var.vpc}. A resource reference needs an attribute,
as ${vpc.id}.
```

— which named the fix rather than the mistake, mattering most during the
migration off counted segments: every un-prefixed variable a rewrite missed
announced its own repair instead of resolving to something silently wrong.
§14.3 narrowed that error rather than removing the principle: an un-prefixed
variable is still impossible to write by accident (`var` is reserved, above),
and a resource whose consuming attribute declares no reference still gets an
error naming the fix, just at stage 6 instead of the parser, and phrased as
"declares no reference" rather than "is not a reference" — because now some
single segments ARE references, and the message must say why this one is not
usable rather than deny the whole shape.

**How a reference divides.** The first segment is the resource, the second is
the attribute, and everything after that is a path — `${vpc.tags.Name}` is
resource `vpc`, attribute `tags`, path `Name`. This works because a resource's
name can never contain a dot (`checkResourceName` refuses one: a resource's
name is its address, and a dot is how an address separates module levels), so
a user-written target is always exactly one segment; module paths are added
STRUCTURALLY at stage 6, not through this parser. `module` stays reserved
exactly as it was.

### Path semantics

**A path indexes a map, and only a map**, to whatever depth YAML produced —
refusing depth two while allowing depth one would be a rule nobody could
predict (§10.1 settled the same argument for composite interpolation).

**Lists are indexed with brackets, maps with dotted keys, and the two compose
in either order:**

```yaml
${var.azs[0]}                 an entry of a list
${var.subnets[0].cidr}        index, then key
${var.regions.us_east.azs[0]} key, then index
```

Brackets rather than a dotted `${var.azs.0}` is not cosmetic: a map may have a
numeric-looking key, so `${var.ports.0}` is genuinely ambiguous between key
`"0"` and index `0`. Resolving it by the value's runtime kind would make a
reference's meaning depend on the type of the thing it names, which is the
class of implicit rule this language avoids everywhere else. With brackets,
`[0]` is always an index and `.0` is always a key, decided at parse time with
no value in hand.

**An index is an integer literal, and nothing else.** No `${var.azs[i]}`, no
`${var.azs[var.i]}`, no arithmetic. Reading a known list at a fixed position
generates nothing; an index that can VARY is only useful if something varies
it, and that is the iteration §54 refuses. The restriction is the whole guard
against that, so it is stated as a rule rather than left as an accident of
what the grammar happens to allow.

No negative indices. `[-1]` for "last" makes a reference's meaning depend on a
length the reader cannot see, and is additive later if it earns its place.

**An out-of-range index is a compile-time error**, on the same reasoning as a
missing key below — the list is fully resolved by stage 4, so out of range
then is out of range forever:

```
var.azs has 3 entries; there is no index 5
```

**Indexing a map, or keying a list, names the mistake both ways.**
`${var.tags[0]}` says "`var.tags` is a map; index it by key, as
`${var.tags.team}`", and `${var.azs.first}` says the converse — each points at
the other form rather than reporting a missing member.

**A missing key is a compile-time error, never an unknown.** Unknown means
"does not exist yet" — a resource the executor will create, whose value
arrives later. A variable's map is fully resolved by stage 4, so a key absent
then is absent forever, and treating it as unknown would defer a certain
failure to apply time, where §20's safety story is weakest. The diagnostic
lists the keys that do exist, the same shape resource-name errors already use:

```
variable "tags" has no key "tema"

Known keys:
  team
  project
```

**A step into a scalar names the kind:** "`var.region` is a string; it has no
members" — not "no such key", which would send a reader hunting for a typo in
a name spelled correctly. A step into the wrong kind of CONTAINER is the case
above, and points at the other form instead.

**Extraction must not launder a secret.** `pkg/value` lets a container be
`Sensitive` itself, separately from its leaves, and a variable declared
`sensitive: true` holding a map or list carries the flag on the container —
its leaves may carry nothing. Walking `Raw` and returning the leaf `Value`
would therefore silently DECLASSIFY every secret inside a sensitive map or
list: `${var.creds.password}` would return unflagged, reach `Format`, and
print in clear in a plan, which is §36's exact failure. **The rule: extraction
unions the sensitivity of every container along the path onto the result —
keys and indices alike.** This is the same rule §10.2 states for functions —
sensitivity unions across all arguments, and `join()`/`replace()` each shipped
that rule wrong once — in a new position, and it needs its own test rather
than assuming existing coverage extends to it. Provenance (§43) takes the
container's source; the leaf has no independent origin.

**The path lives inside the variable reference**, not as a general operator:
`value.VarRef` carries a path — a slice of steps, each a key or an index — and
there is no `OpIndex`. A general index operator would make `${lower(x).y}` and
`${merge(a,b).k}` grammatical, which is the first step toward the expression
language this section exists to refuse; bounding the path to one syntactic
position is the same move §10.3 makes for map literals, legal as an argument
and nowhere else.

**Resource attributes take the same paths.** `${vpc.tags.Name}` resolves with
the same steps, the same deferral and the same resolution as `${vpc.id}`:
`ResourceScope.Attribute` returns the map once it is known, the steps apply to
it, and `ResolveDeferred` finishes the job at apply against what the resource
actually turned out to be — deferred exactly as a whole attribute is, for the
same reason: the value does not exist yet.

One check is weaker here, and only one: the attribute NAME (`tags`) is
validated against the schema at compile time, but a step past it (`Name`) is
not, because `pkg/schema` models `tags` as `Kind: KindMap` and nothing deeper.
Two things keep that acceptable rather than a hole — it fails before dispatch,
not during create, since `resolveAfter` runs in `execute` ahead of the
provider call; and the apply-time diagnostic carries what a compile-time one
would have said, the keys that do exist, in this section's shape. Closing the
gap — a compile-time check on a nested resource attribute key — needs nested
shape in `pkg/schema`, which crosses the plugin wire, and is deliberately left
for later rather than made to wait on a wire-format change.

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
it sees are its inputs plus the ambient `environment` and `project`. (It was `environment`,
`region` and `account` until 2026-09-13; the last two were never actually in scope to
cross a module boundary, and a project variable named `region` is now passed to a module
as an ordinary input like any other.)

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
    name: ${var.application_name}
    image: ${var.image}
    count: ${var.replicas}

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
    iam-role: ${var.aws_role}
    region: ${var.aws_region}
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

**`${var.aws_region}` is an ORDINARY DECLARED VARIABLE, and these examples used to write
`${var.region}` as though it were supplied by the process.** It is not. `region` and
`account` are named as process variables in §12/§43 alongside `environment` and
`project`, and `compiler.Options.Region` exists and is read — but nothing anywhere
assigns it, so `${var.region}` reports `undefined variable "region"` today (checked against
the built binary, 2026-09-13). `provider.DiscoverRequest.Region` is inert in the same
way: `internal/discovery/walk.go` builds the request without it and no flag sets it, so
a plugin author implementing against `DiscoverParams.Region` reads `""` forever.

**DECIDED AND DONE, 2026-09-13: the fields came out.** The AWS plugin's agreed model
needs none of them — every regional type declares `region` as Required and ForceNew, an
instance supplies it through `defaults:`, and `discover` takes its scan list from the
instance's own `config:`. So `compiler.Options.Region`/`.Account`,
`provider.DiscoverRequest.Region` and `pluginproto.DiscoverParams.Region` are all
removed, and `region` is an ordinary variable name. No example here may use `${var.region}`
as a process variable, because there is no longer any such thing.

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
      environment: ${var.environment}
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
`region: ${var.aws_region}` — and that creates a cycle. Constructing a provider needs its
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

**AMENDED 2026-09-13. The state-only commands resolve variables after all.** This
section used to call that impossible and bill it as "the honest cost": `destroy`,
`refresh`, `discover` and `import` never compile, so — the argument went — none of them
has a variable scope, and each must read `providers:` for LITERAL values only and refuse
an instance whose configuration interpolates anything.

The premise was wrong. **Resolving variables is stages 1 to 4 and needs nothing else** —
no registry, no plugins, no resources, no modules. `compiler.VariableScope` is those
four stages with the rest of the compile left off, shared with `Compile` rather than
copied, because the precedence ladder IS the product's rule (§7) and a second
implementation of it would drift in exactly the way a user could not predict. Three of
the four commands are handed an environment on the command line, so their scope is the
same one `plan` would build.

What the old rule actually cost was not elegance. An AWS instance supplying its region
through `defaults: {region: ${var.aws_region}}` — the agreed model — could be planned and
applied and then never refreshed or destroyed. **A project the tool cannot tear down is
worse than one it cannot build**, and it would have shipped that way.

`--var` and `--var-file` are therefore accepted by `destroy` and `refresh`, which used to
refuse them outright. That refusal was right about its own reasoning and wrong about the
facts: a flag that cannot affect the outcome must be refused rather than accepted and
ignored, and this flag can affect the outcome, because an instance's configuration
interpolates it. The concern the refusal protected still stands and is now a test: a
`--var` naming a RESOURCE variable must not change what these commands do, since what
they read and what they tear down comes from state.

**`discover` is the one that keeps a real limit**, because it takes no environment at
all. It resolves every rung that does not depend on one — a declared `default:`,
`variables.yml`, `vars/default.yml`, `--var` — and an unselected chain leaves a
per-environment variable UNKNOWN rather than erroring. An unknown must not reach a
plugin's `Configure`: it would be read as absent and the survey would run against
whichever account the plugin defaults to, which is the one outcome invisible in the
output. So an instance still holding an unknown is refused by name, and the suggested
action is `--var` — a promise the command now keeps, where before it named a flag it
then rejected. `plan` and `apply` on an ORPHANED environment (§6.1) take the same path
for the same reason: the environment is no longer declared, so its per-environment
values went away with the declaration.

What still saves the rest is unchanged: the instance NAME is recorded in state, so a
resource reaches the right account even when nothing can be resolved about it.

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
    region: ${var.aws_region}
    defaults:
      tags: ${var.tags}
      prevent_destroy: ${var.protect}
```

Nested rather than mixed in, because at the top level `tags: ${var.tags}` and
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
the instance's `defaults:`      defaults: {tags: ${var.tags}}
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
`type`, reachable as `${var.type}`, and classified nothing. Classification was done
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

## 14.1 Provider-chosen attributes and attribute aliases

**Agreed 2026-09-14**, from the AWS plugin's requirements (`infrena-provider-aws`,
`docs/investigations/2026-09-13-generic-aws-provider.md` §5 and §8). Two changes to
`pkg/schema`, designed and landed TOGETHER so plugin authors meet one contract change
rather than two.

Both are additive. No existing plugin sets either field, so the fake provider is
unaffected and `pluginproto.Version` does not move.

### The attribute model was binary, and a real cloud is not

An attribute was either configuration's (`Required`, or optional with a `Default`) or the
provider's (`Computed`, and `schema.Validate` refuses `Required && Computed`). AWS needs a
third state that neither expresses: **configuration MAY set it, and the provider picks
when configuration does not.**

It does not fail cleanly. It fails to converge, which was reproduced against the fake
provider before any of this was designed:

```
  ~ fake.database.db
      tags: {team: "picked-by-the-cloud"} -> (absent)
```

The planner treats a returned value that configuration does not set as "removed from
configuration" and proposes to unset it. So: omit `availability_zone`, AWS picks
`eu-west-1a`, the next plan proposes unsetting it, apply, AWS picks again — forever.
Invariant 2 broken for a whole class of attributes. Live evidence from the AWS spike: a
VPC created through Cloud Control with only `CidrBlock` and `Tags` came back from
`GetResource` with ten properties, including `EnableDnsSupport`, `EnableDnsHostnames` and
`InstanceTenancy`.

**IT IS NOT A HANDFUL OF CASES, and that constrains the design.** CloudFormation schemas
mark `required`, `readOnlyProperties` and `createOnlyProperties`. They do not mark "AWS
picks this if unset", and `default` is almost never populated — of fifteen properties the
AWS session checked against a real account, only IAM `Role.Path` had one. A generic
provider therefore cannot tell "AWS chooses this" from "this stays absent", and must set
the flag on **every non-required, non-read-only property**. So the semantics must be inert
for an attribute that is simply absent everywhere, and nothing may depend on the flag
being used sparingly.

### `Optional` beside `Computed`

```go
type Attribute struct {
    Required  bool
    Computed  bool
    Optional  bool // with Computed: configuration MAY set it; the provider picks if it does not
    ...
}
```

- **Configuration sets it** — an ordinary attribute. Diffed normally, `ForceNew` applies
  normally.
- **Configuration does not set it** — the provider's value is recorded and NEVER diffed.
  No "removed from configuration", no replacement.

`ForceNew` therefore needs no special rule: an unset optional+computed attribute produces
no diff at all, so it can never produce a replacement, however the provider's value
changes. Write `us-east-1b` where state holds `us-east-1a` and the plan replaces, which is
correct; omit it and nothing happens whatever AWS reports.

`schema.Validate` keeps refusing `Required && Computed` and `Computed && Default != nil`.
`Optional` without `Computed` is meaningless — every non-required attribute is already
optional — and is refused, so the field cannot be set in the belief it does something.

### What the plan says, and what it deliberately does not

A value the provider chose is shown as

```
availability_zone: "eu-west-1a"   [provider-chosen, not in configuration]
```

under `--verbose`, and ALWAYS when the attribute is also `ForceNew` — that being the case
where a later explicit value replaces the resource, so a reader needs to know the value is
currently the provider's. If the `--verbose` output proves noisy on a large schema, that
threshold is the dial to revisit.

**It does not say "no longer set in configuration", and that is a correction to the
original proposal rather than a wording preference.** infrena cannot know it. Every
attribute in a state file records `source=provider` — including ones configuration set
explicitly, because state records what the provider RETURNED. Checked against a real state
file rather than assumed. Answering "was this ever configured?" would mean recording
per-attribute "configuration manages this" in state: a format version bump plus a
migration, bought for an informational line. Refused. The line above is true regardless of
history and still does the job — a user who has just deleted that line from their file and
re-planned sees the attribute now reads as provider-chosen, so their edit changed nothing.

**Accepted cost: drift on an unset optional+computed attribute is invisible to `plan`.**
Someone flips `EnableDnsHostnames` in the console on a VPC that never configured it and no
plan reports it. That is the right trade — an attribute infrena does not manage is not
infrena's to report as drift — and it remains visible through `refresh` and `state show`,
which is where someone investigating a console change looks.

### `import --generate` emits the ForceNew ones and omits the rest

A generated file gets the optional+computed attributes that are also `ForceNew`, and not
the updatable ones.

The ForceNew ones are the resource's IDENTITY — a bucket name, a role name, a subnet's
availability zone. Omitting them writes a file that says "AWS, pick a name", which is a
different request from the one that was just imported, and a later explicit value would
replace the resource. The updatable ones (`EnableDnsSupport`, `MaxSessionDuration`) are
settings with cloud defaults, and emitting them pins every AWS default into the file as
noise — against §27's minimal-generation rule. The rule is mechanical for a plugin, since
`ForceNew` comes from `createOnlyProperties`.

### Aliases: several spellings, one identity

A plugin may declare alternative spellings for an attribute:

```go
Attribute{ Kind: value.KindString, Aliases: []string{"cidr", "cidr_block"} }
```

Matching is CASE-INSENSITIVE across the canonical name and every alias, so `CidrBlock`,
`cidrblock`, `cidr_block` and `cidr` all reach the same attribute. `schema.Validate`
REFUSES a definition whose names fold together — two attributes, or an attribute and an
alias, or two aliases — which turns the collision hazard into a plugin that will not load
rather than a silent runtime surprise for whichever spelling wins.

**THE ALIASES ARE THE PLUGIN'S, AND CROSS THE WIRE.** infrena contains no mapping, no
table and no file: it learns every type, attribute and alias from the schema a plugin
hands over at load, exactly as it already learns everything else. A curated overlay
belongs in the plugin's own repository at its codegen time, where infrena never sees it.

Considered and REJECTED: a configuration file infrena reads. It would need a version and a
compatibility story (§61), a defined location, and rules for reconciling a file that
disagrees with the schema — and a stale entry would silently map to an attribute the
plugin no longer declares. The schema already crosses the wire and is already validated
against the attribute set on arrival, so it is one artifact where a file would be two. The
cost of this choice, accepted deliberately: **changing an alias requires a plugin
release.** For a generated plugin that releases whenever upstream schemas change, that is
no extra cost.

### Canonicalise ONCE, at the compiler boundary

This is the load-bearing rule, and it comes from tracing every place an attribute is
looked up by name rather than from preference. There are seven:

| Site | |
| --- | --- |
| `compiler/schema.go` resource attribute keys | canonicalise |
| `compiler/schema.go` `defaults:` keys | canonicalise |
| `compiler/bind.go` reference attributes (`${vpc.vpc_id}`) | canonicalise |
| `compiler/schema.go` `markSensitive` | exact — downstream |
| `planner/diff.go` | exact — downstream |
| `pluginhost/adapter.go` undeclared-attribute refusal | exact — downstream |
| `generator/generate.go` | exact — downstream |

**The first three are the boundary; the last four are downstream of it and stay exact
lookups.** Spreading alias-awareness across all seven is how this goes wrong, and two of
the downstream four show exactly how:

- `markSensitive` applies schema-declared sensitivity by exact name. An alias reaching it
  unresolved means **a sensitive attribute written under an alias is never marked
  sensitive**, and appears in plans and reports in clear — §36's redaction guarantee,
  broken.
- The planner's diff would see `cidr` from configuration and `CidrBlock` from the provider
  as two attributes, and propose a change forever — the same non-convergence class this
  section exists to fix.

References are canonicalised in stage 6 rather than at evaluation, because
`expressions.ResourceScope` is `map[address]map[name]Value` with no schema access, while
stage 6 already validates a reference's attribute against the definition. Two spellings of
one attribute in one resource is an ERROR naming both, the same shape as §4.1's
name-declared-twice rule.

### Identity versus display, which settles itself

The stable key is the plugin's declared name. Aliases are input and display only.

This needs no enforcement and is not a convention anyone can break: state, the plan
artifact and the plugin wire are all written DOWNSTREAM of canonicalisation, so they
cannot carry an alias. Adding a curated `cidr` in a later plugin release therefore changes
only what a user may type and what a plan renders — never a stored key, and never a
migration.

`explain` lists every accepted spelling, and plans and generated configuration show the
friendly alias where one exists, by mapping canonical to display at render time. A pure
function of the schema, with no storage consequence.

### Out of scope, recorded so each is a decision

- **Keys inside map- and list-valued attributes** (`Tags[].Key`,
  `PrivateDnsNameOptionsOnLaunch.HostnameType`). infrena does not validate nested keys at
  all, so a plugin translates those itself.
- **Nested create-only pointers** (37 on VPC alone) and **`conditionalCreateOnly`**. Real,
  and neither is blocked by this.
- **Project-level aliases**, letting a USER rename an attribute for their own project
  whatever the plugin says. A genuinely different feature, decidable on its own merits.

---

## 14.2 `ignore_changes`: attributes something else owns

**Agreed 2026-09-14.** A resource may name attributes whose drift infrena does not propose
to revert:

```yaml
resources:
  app:
    type: aws.ecs.service
    image: myapp:latest
    lifecycle:
      ignore_changes: [task_revision]
```

**The case it exists for.** A CI pipeline deploys by setting an ECS service's task
revision. infrena computes its plan from configuration, so the next `plan` proposes
reverting the revision to whatever the file says, and the next `apply` undoes the
deployment. The two systems fight, and the one that ran most recently wins. `ignore_changes`
is the user saying: that attribute is not mine.

Under `lifecycle:`, beside `prevent_destroy` and `retain`, because it is the same kind of
statement — a rule about how this resource is managed rather than a value it holds.

### What it does, exactly

- **The planner does not diff it.** No operation is proposed for it, in either direction,
  whatever the provider reports.
- **State keeps what is really there.** The operation's `After` carries the OBSERVED value,
  not configuration's. Without that, the executor would record configuration's value and
  state would claim a revision the service is not running — the plan saying nothing changed
  while quietly rewriting the record of what did. Not diffing is only half of ignoring.
- **A create uses configuration.** There is no existing resource, so configuration's value
  is the only one there is; ignoring it would build the resource with the attribute unset.
- **A REPLACEMENT RESETS IT**, and the plan says so. Replacement rebuilds from
  configuration, so an ignored attribute cannot survive one. The first version of this
  printed `[change ignored]` beside `size: 500 -> 10` — both half true and impossible for a
  reader to resolve. An update says `[change ignored]`; a replacement says
  `[ignored, but a replacement resets it]`, because losing a deployed revision to a change
  made somewhere else entirely is exactly the surprise worth naming.
- **It is not settable from an instance's `defaults:`.** Which attributes a resource lets
  drift is a property of that resource and its pipeline, not of the account it lives in.

### Spellings, and why a wrong name is an ERROR

Entries go through §14.1's canonicalisation boundary like everything else a user writes, so
`[task_revision]`, `[taskRevision]` and any declared alias all name the same attribute.

**A name matching no attribute is refused.** The failure mode here is silence: the planner
compares these names against canonical attribute keys, so an unresolved spelling ignores
NOTHING — the user writes `ignore_changes: [taskRevision]`, sees it accepted, and watches
the next apply revert the attribute they thought they had protected. A warning would be
read as "it worked". Two spellings of one attribute are deduplicated rather than refused,
since ignoring twice is ignoring.

The list is sorted, because it reaches the plan artifact and invariant 6 wants that
byte-stable however it was written.

### Versions this moved, checked rather than assumed

- **State** gained `ignore_changes` inside `lifecycle`, additively and `omitempty`. **No
  version bump**: an older infrena ignores the key and has no ignore feature to get wrong,
  and a newer infrena reading older state finds nothing, which is correct. The planner
  reads the list from compiled CONFIGURATION, never from state, so state's copy is
  bookkeeping.
- **The plan artifact** gained the same key, additively. **No version bump.** The
  frozen-keys test did not descend into `lifecycle` and so did not notice, which was its
  own defect: a nested object in a versioned format is still the format. It descends now.
- **The plugin wire**, the report and the manifest are untouched: `Lifecycle` does not
  cross to a plugin at all.
- **The product version** is a MINOR, by §61.1's amended rule: this adds configuration
  syntax, and a project using it cannot be read by an older build.

---

## 14.3 Provider-declared resource references

**Agreed 2026-09-14**, from a concrete complaint James had building Terraform:

> I've had this exact issue, creating new terraform, providing an id when it wanted an
> arn, and vice versa. Just passing the VPC to it seems way easier.

A reference used to name an attribute and stop there — the engine had no idea what the
attribute MEANT. `vpc_id: ${vpc.arn}` compiled, planned, applied, and was rejected by the
cloud, or worse, accepted and wrong: `vpc_id` was a string to infrena and `${vpc.arn}` was a
string to infrena, and nothing in between knew a VPC's id and its arn are different things.

```yaml
subnet:
  type: aws.ec2.subnet
  vpc_id: ${vpc}       # aws.ec2.subnet's vpc_id refers to aws.ec2.vpc's id
  cidr: 10.0.1.0/24
```

### THE PLUGIN DECIDES. The engine never guesses.

Infrena cannot know that Cloud Control's `AWS::EC2::Subnet.VpcId` wants a VPC's id rather
than its arn — that is knowledge about an API, and it belongs to whoever owns the API. So
`schema.Attribute` gained `References *Reference{Type, Attribute}`, and the engine's whole
part in it is to READ that declaration and project `${vpc}` into `${vpc.<attribute>}` at
stage 6, beside canonicalisation. This is DATA, per §31.1's rule that nothing in
`pkg/schema` may gain a function-typed field: a plugin that resolved its own references
would be a second resolver, free to disagree with the engine's about what a reference means
— exactly the class of bug two independent implementations of one fact always produce.

### `${vpc}` is SUGAR over `${vpc.id}`, and that is the property that ships it

Both spellings are legal, forever. `${vpc}` never replaces `${vpc.id}`; it is shorthand for
it, filled in from the declaration.

That is what makes partial coverage shippable rather than a half-feature. An attribute
whose plugin has not declared a reference is not a smaller version of this feature — it gets
the SAME behaviour as before this section existed: `${vpc}` is an error naming the fix
("name the attribute you mean, as `${vpc.<attribute>}`"), and `${vpc.id}` keeps working
exactly as it always has. **The engine never falls back to "probably the id".** A missing
declaration degrades to today's behaviour; it never degrades to a wrong value shipped
silently. That is what lets a plugin publish references for the relationships it has
reviewed and say nothing about the rest, rather than blocking on covering every relationship
before any of them ship.

### The type check fires for both spellings

Naming the attribute explicitly does not escape the check — only the projection.
`vpc_id: ${database}` and `vpc_id: ${database.id}` are both compile errors when `vpc_id`
declares `aws.ec2.vpc` and `database` is an `aws.rds.dbinstance`, because reaching into the
wrong resource is the same mistake whichever spelling it wears. This is half of this
feature's value: Terraform finds this kind of mistake when the API says no; infrena finds it
before anything runs.

### `References` and `Requirement`: different axes, not yet reconciled

`schema.Requirement` already declares relationships, and the AWS overlay already
hand-maintains them — "a subnet needs a VPC to exist at all", which powers §17's
missing-resource detection before any attribute is examined. `References` is a different
axis: "this particular attribute holds a VPC's id." A `Requirement` cannot do the
projection, because it never says which attribute carries the reference, nor which
attribute of the target is used — the very id-versus-arn question this section exists to
answer. But `References` CAN derive a `Requirement`: an attribute required to refer to type
X means the type requires an X.

**Left unreconciled, deliberately, for now.** Two hand-maintained tables saying
overlapping things about the same relationship is how they come to disagree, so leaving them
alone is not neutral — but deriving `Requirement` from `References` today would silently
DROP requirements the overlay already states for relationships the AWS plugin's heuristic
has not yet found or had approved, since its coverage is partial by design. The correct
order is populate `References` first, leave `requirements:` alone, and add a
generation-time check that every `requirements:` entry has a matching `References`
somewhere — a cross-check rather than a replacement, until coverage earns the derivation.

### `Fields`: a declared map's known keys

`schema.Attribute` also gained `Fields map[string]Attribute`, for the same reason
`References` did: `${vpc.tags.Nmae}` used to pass `validate`, produce a clean plan, and fail
halfway through `apply` after real infrastructure existed, because nothing checked a path
PAST the attribute axis. Where a provider knows a map attribute's keys, it declares them in
`Fields`, and a typo in a path becomes a compile error listing the keys that exist — the
same promise §14.1 already makes for a top-level attribute name, extended one level deeper.

**NIL MEANS OPEN, and that is deliberate.** AWS tags take any key and always will;
declaring `Fields` for them would be a LIE — a schema claiming to know a shape it does not.
An attribute with no `Fields` is checked exactly as it always was: not at all, until apply.
`Fields` is therefore never something a plugin is obliged to declare — it is something a
plugin declares only where it genuinely knows the shape, the same partial-coverage promise
`References` makes: a plugin that has reviewed some relationships and not others ships what
it has reviewed and says nothing about the rest, rather than blocking on completeness.

`schema.Validate` refuses `Fields` on an attribute whose `Kind` is not `KindMap`, at every
nesting level — a declared set of keys is meaningless on anything that is not a map, and a
plugin that declared one anyway is describing a shape that cannot exist rather than one it
has not gotten around to.

A NESTED `References` — one attribute inside a `Fields` map declaring a relationship of its
own — is refused by `schema.Validate` too, rather than silently ignored. `projectRefs`
(`internal/compiler/bind.go`) only ever reads a *top-level* attribute's `References`;
nothing walks into `Fields` to consult a nested one. A declaration nothing consults is
exactly the defect class this section exists to close — a plugin author reads the field,
believes it does something, and discovers otherwise only when a reference silently fails to
project. Refusing it at validation is simpler than teaching every consumer of `References`
to recurse, and it keeps the failure at the moment the plugin loads rather than at the
moment a user's `${vpc}` quietly does not become `${vpc.id}`.

### Adding a `References` is a BREAKING change for that plugin's users

A declaration does not only enable `${vpc}`. It also type-checks the spelling that was
always legal: once `vpc_id` declares that it refers to an `aws.ec2.vpc`, the configuration
`vpc_id: ${database.id}` STOPS COMPILING, and it may be configuration a user wrote months
ago and has already applied.

So a plugin release that adds declarations to attributes that had none is a breaking
release for that plugin, whatever it does to its own version number, and its notes must say
so. This is the §61.1 test — what an existing project does — applied to a plugin rather than
to infrena.

It cuts the other way too, and that is the reassuring half: a plugin that declares NOTHING
changes nothing for anybody. Coverage buys checking; absence costs exactly what it cost
before. Which is what makes it safe to add declarations a few at a time rather than all at
once — provided each batch is released as the breaking change it is.

### A module boundary carries no `References` in either direction

`${net}` is sugar the ENGINE fills in from a consuming attribute's own `References`
declaration (above). A MODULE BOUNDARY has no such declaration to read, on either side of
it, and that is ONE rule, not two: an input declares a TYPE — string, integer, map — not a
relationship to a resource (§9), and an output PUBLISHES A VALUE with no consuming attribute
in sight to have declared one. Neither carries `References`, so projecting a whole-resource
reference at a module boundary would be the engine guessing, which this section forbids
outright — the same reason, stated once, because it is the same hole seen from both ends.

`internal/modules/inputs.go`'s `refuseWholeResourceInput` and `outputs.go`'s
`refuseWholeResourceOutput` are the two enforcement points, and BOTH must exist for the rule
to hold: a boundary closed from only one direction is not a smaller version of this rule, it
is a hole with a different shape. That is exactly what shipped first — the input side was
closed, the output side was not, and the output side is the more dangerous half to leave
open. Passing a whole resource as a module INPUT fails loudly, at compile time, the moment
it is written. Publishing one as a module OUTPUT compiles clean: the failure is invisible
until whatever eventually consumes the output reaches apply — after the module's own
resources already exist.

**This is a LANGUAGE RULE this branch invented, and it exists to close one specific hazard:
an empty-attribute reference escaping compiler stage 6.** `expressions.ResourceScope.Attribute`
looks an attribute up by name in a plain map; `""` misses, so a reference that keeps an
empty attribute all the way to the executor does not fail at compile time — it stays
deferred forever, and the run either dies mid-apply after real infrastructure already
exists, or creates the resource with the attribute silently unset. `${net}` bare, at a
module boundary with no consuming declaration to project against, is exactly such a
reference, so it is refused at the one place each side can still say why, naming the fix:
write `${net.<attribute>}` instead. Nothing downstream of stage 6, and nothing that crosses
a module boundary in either direction, may ever see a reference whose `Attribute` is empty.

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

## 25.1 What `discover` leaves out, and how to narrow it

**Added 2026-09-15 (Unit B).** A real account answers with hundreds of rows, and the
question a user is actually asking is "what have I not adopted yet". So the default view is
not everything.

- **Resources this project already manages are hidden**, and the footer says how many:
  `62 resources found: 12 unmanaged, 50 already managed (--all to show them).` A count a
  user cannot see is one they will assume is zero. `--all` shows them with a `STATUS` column naming the environment
  that manages each. The column earns its place only under `--all`: in the default view
  every row would read `unmanaged`, and a column with one value is noise.
- **Managed means managed in ANY environment**, not in the one you happen to be importing
  into. A resource adopted in `production` is adopted; offering it again in `dev` would put
  one real resource under two addresses, and invariant 1 then schedules the one that
  declares nothing for destruction.
- **Resources the provider flags as cloud-owned** carry a `NOTE` giving the plugin's own
  reason — an account's default VPC, a default security group, a service-linked role. They
  are listed, never hidden: §31.1's `system_owned` is a warning, not a veto. The column
  holds the REASON and not the flag, because a flag a user overrides without understanding
  it may as well not be there, and it appears only when something is noted.
- **`--tag k=v`, `--exclude-type` and `--name` narrow the list**, all repeatable except
  `--name`, which is a `path.Match` glob. A malformed glob is an ERROR rather than an empty
  result, because an empty result reads as an empty account. `import` takes the same three
  flags, so the list a user reads and the list they adopt come from one question.
- **Filtering happens AFTER naming**, never before. §27.2's uniqueness pass is
  order-dependent by construction, so filtering first would change the names of the
  resources that survive it — the same resource would import under a different name
  depending on a flag that has nothing to do with naming.

`discover` also reports each provider instance as it answers and honours `--output`, the
same progress-and-report hook the other long commands have (§2.3 of the CLI spec).

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

## 26.1 What import leaves out

**Added 2026-09-15 (Unit B).** Import adopts what discovery found, so it inherits §25.1's
exclusions, with one asymmetry that is deliberate:

- **With no selector**, a resource already managed is simply not part of the question and is
  dropped silently. That is what makes `infrena import dev` re-runnable: it adopts whatever
  is new and says `Nothing to import.` when there is nothing. It used to REFUSE the whole
  run, which turned a 300-resource import into an error because 3 of them were already
  adopted.
- **Naming one explicitly is refused**, with the environment that manages it. Silently
  ignoring an explicit selector is worse than refusing it: the user asked for one specific
  resource and would be told the run succeeded.
- **A cloud-owned resource is never adopted by default and never silently.** The skip is
  reported with the plugin's reason. Naming it as `<type>.<provider id>` adopts it anyway —
  a team that genuinely manages its default VPC exists, and the engine is not the party to
  forbid it.

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
2. Otherwise the provider ID, sanitised to an identifier.

**The name is PREFIXED WITH ITS TYPE**, added 2026-09-15 (Unit B): the last dotted segment of
the resource type, so `aws.vpc` tagged `app1` is `vpc-app1` and `fake.network` with id
`vpc-0a1b` is `network-vpc-0a1b`. It reads correctly at the point it matters, which is inside
a reference (§27.0) — `network: ${network-vpc-0a1b}` says what it points at. The prefix is
skipped where the sanitised text already starts with it, because AWS identifiers are
themselves prefixed and `vpc-vpc-0a1b2c3d` helps nobody. The last segment is the right prefix
without a protocol change, because the AWS generator already collapses unambiguous type names;
an optional plugin-published short name stays available later with this as its fallback.

**The tag map is found by ASKING THE SCHEMA**, through the registry's alias fold
(`Definition.Canonical`), not by reading `attrs["tags"]`. That literal lowercase key was the
bug behind every `vpc-129012092`: the AWS plugin's canonical attribute is `Tags`, so the
lookup missed on every AWS resource and this section's naming rule was dead code against the
only real provider. A third hard-coded spelling would be the same defect with a longer list.
Inside the map, `Name` is preferred over `name` — AWS writes the first, most other things
write the second.

A collision between two resources claiming the same name is resolved by suffixing the provider
ID, never by dropping one: two resources silently becoming one is the shape this project
guards against everywhere else. The type prefix makes a cross-type collision impossible, so
that suffix now fires only for two resources of ONE type sharing a `Name` tag — which is
genuinely two resources.

Names matter more here than they look. A resource's name is part of its address, and an address
is what state is keyed by — so renaming an imported resource later is a destroy plus a create.
Generation should therefore produce the name a person would have chosen, not one they will
immediately want to change.

## 27.3 Generated configuration references what discovery found

**Added 2026-09-15 (Unit B).** A subnet says its VPC BY NAME rather than pasting a cloud
identifier:

```yaml
database-db-9:
  type: fake.database
  engine: postgres
  network: ${network-vpc-0a1b}
```

Four rules govern it, and each exists because its absence produced a specific failure:

1. **The PLUGIN declares the relationship.** `schema.Attribute.References` says that
   `aws.subnet`'s `VpcId` holds an `aws.vpc`'s `VpcId` (§14.3). The engine never infers that
   a property whose name ends in `Id` points anywhere; inferring it is how a tool starts
   knowing about AWS, which is this project's first architectural rule broken.
2. **A reference is emitted ONLY where its target is in the same generated set.** A
   `${vpc-app1}` naming a resource no generated file declares is a compile error in a file
   the user never wrote — strictly worse than the identifier they would otherwise have had.
   The literal survives, and a comment at that line says the target was not discovered, so a
   silent literal never reads as a value somebody chose.
3. **A reference is not a reason to emit an attribute.** The omission rules above run first:
   an attribute equal to its default is still omitted, and a computed or sensitive one is
   still gone. §27 stays minimal.
4. **THE EDGES GO INTO STATE FROM THE SAME PASS.** A reference IS a dependency edge — the
   compiler turns `${network-vpc-0a1b}` into exactly one — so `import --generate` records
   the edges the generator emitted, and `generator.Generate` returns them alongside the
   files for that reason. Without this the first plan after an import proposed
   `~ depends_on: [] -> [network-vpc-0a1b]` on every resource that referenced another, and
   §29's invariant did not hold. They come from the rendering pass rather than a second
   opinion computed by the importer, because rule 3 means whether a reference exists at all
   depends on every omission rule; two answers computed separately would eventually differ,
   and the disagreement is invisible until someone runs `plan`.

Rule 4 obeys §26's ordering unchanged: configuration is written FIRST, then state. Failing
between the two leaves a declared resource that is not yet managed, which the next plan
proposes CREATING — visible and refusable. The other order leaves one it proposes DESTROYING.

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

**A REFERENCE is not an exception either, and that took a fix rather than an
argument.** §27.3's `network: ${network-vpc-0a1b}` compiles to the same literal
the attribute held, so attributes matched — but it also compiles to a dependency
edge, and the import path recorded none, so the first plan after an import
proposed `~ depends_on: [] -> [network-vpc-0a1b]` on a resource nobody had
touched. Configuration and state disagreed from the moment both were written.
The fix was to record the edges generation emitted (§27.3 rule 4), NOT to teach
the planner to overlook dependencies and not to stop emitting the reference:
either would have traded a visible bug for a silent one — the first hides a real
edge change, the second throws away the relationship the reference exists to
keep. `TestTheImportRoundTripPlansClean` now pins all three halves, because each
passes alone against a broken generator: the file references by name, state
records the same edge, and the plan is clean.

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
distributed binary that `infrena` launches as a child process and talks to over
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

- A plugin named `aws` is the executable `infrena-plugin-aws`.
- The host starts it with `INFRENA_PLUGIN_COOKIE` set to a random value. A binary run by
  hand without that variable prints "this is an infrena plugin; it is run by infrena" and
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

The plugin's first message is `{"protocol": 4, "name": "aws", "version": "0.3.1"}`, sent
before any request. **`pluginproto.Version` is 4 since 2026-09-15**, and `Supported` is
`{4, 3, 2, 1}`.

| Version | Added | Why a bump rather than a silent addition |
| --- | --- | --- |
| 2 | `optional` and `aliases` on an attribute (§14.1) | |
| 3 | `References` on an attribute (§14.3) | |
| 4 | `system_owned` and `system_owned_reason` on a discovered resource (§25.1) | |

Each one is a KEY ADDED to a payload whose decode is lenient, which is exactly why it is
announced. A plugin built against a newer SDK talking to an older host has the new key
silently DROPPED, and the symptom is always a claim the plugin plainly made going unheard
with nothing anywhere saying so: `${vpc}` reporting "declares no reference" about an
attribute that declares one, or a default VPC the plugin flagged being offered for adoption
unflagged. Announcing the version turns each into a refusal that names the plugin and both
versions.

The other direction costs nothing, which is what keeps every older version supported: a
protocol 3 plugin sends neither system-owned key, absent decodes as "the plugin made no
claim", and that is exactly what every plugin in existence means today.

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
  The cycle §12.1 breaks stays broken. State-only commands pass RESOLVED configuration
  too, since 2026-09-13 — `compiler.VariableScope` gives them stages 1-4 without a
  compile (§12.1, amended).
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
  `infrena-plugin-aws` holding two configured clients, addressed by handle.
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
  1. `--plugin-dir` and `INFRENA_PLUGIN_PATH`
  2. `<project>/.infra/plugins/`
  3. `~/.local/share/infrena/plugins/`
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
    ASYMMETRY with §61.2's `infrena:` floor, which EXEMPTS a 0.0.0 build: there the
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

**Ruled 2026-09-13. Re-examined 2026-09-17 and UPHELD**, with the costs re-measured — see
below, two of the three were out of date. `github.com/infrena/infrena` is not a fetchable
module and will not be until the product is feature complete. Requested by the
fake-provider port, decided against for now; do not re-raise it as a blocker.

This section is written so that the day the switch is flipped is a checklist rather than a
rediscovery. The point of re-examining a standing decision is that several others rest on
it, and a premise that quietly becomes false takes its dependants with it.

What it costs, so nobody re-derives it:

- **There are no third-party plugin authors yet**, and cannot be. A plugin needs either a
  checkout of a private repository or `GOPRIVATE=github.com/infrena/*` plus credentials to
  the org. `AGENT.md`, §31.2 and the reference plugin all exist to invite outside plugins,
  and that invitation is on hold rather than withdrawn. **Still true.**

- **~~The one consumer uses `replace`.~~ There are THREE consumers now, and two of them
  have already taken the exit.** Corrected 2026-09-17:

  | Repository | Requires | Local work | CI |
  | --- | --- | --- | --- |
  | `infrena-provider-fake` | v0.7.0 | committed `replace => ../infrena` | `scripts/ci-use-infrena-tag` strips the replace |
  | `infrena-provider-aws` | v0.7.1 | gitignored `go.work` | `GOWORK=off` against the required version |
  | `infrena-backend-s3` | v0.9.0 | gitignored `go.work` | `GOWORK=off` against the required version |

  **The `go.work` arrangement is the better one and fake should move to it.** A committed
  `replace` is a statement in the module graph that has to be removed to be correct, so
  correctness depends on a script running; a gitignored `go.work` is invisible to the module
  graph and absent by default, so the *default* is the correct build and local work is the
  deliberate exception. That is the right way round.

  The sibling-checkout requirement it records is real but belongs to fake alone now. It has
  been renamed twice — `../ilan` until 2026-09-13, a local folder name no clone creates,
  which broke CI the first time it needed the plugin; and `../infrata` until the 2026-09-14
  rename.

- **A `replace` means that repository builds against a WORKING TREE, not a version.** Its
  tests run against whatever is uncommitted here, which is how it saw a stale
  `internal/semver` that had been moved. A fast loop while both repositories change together
  daily, and a correctness hazard once they do not. **v0.1.0 (2026-09-13) was the exit**, and
  aws and s3 took it. Until a release existed it was not available at any price: every build
  reported `0.0.0-dev`.

- **A separate problem that visibility does not cause and will not fix: every consumer is
  behind.** infrena is at **v0.11.1**; the three require v0.7.0, v0.7.1 and v0.9.0. Nothing
  is broken by that — the protocol is versioned and old plugins are supported deliberately
  (§61) — but a consumer pinned four minors back is not exercising what ships, and the
  reference plugin in particular is meant to demonstrate the current SDK. Track it as its own
  concern, not as a cost of privacy.

**A semver tag is worth cutting anyway**, and is independent of visibility: with
`GOPRIVATE` set, a tagged version lets the plugin `require github.com/infrena/infrena
vX.Y.Z` and drop the `replace`, which removes the sibling-checkout requirement and pins
the build to something reproducible. It also makes `infrena version` report a real version
instead of `0.0.0-dev` (§61.1).

#### What flips on the day

- **§31.2 and `AGENT.md`'s invitation stops being on hold.** It is the one thing here that
  is a decision rather than a mechanic: the moment outside plugins can be built, the
  protocol and the SDK's surface become things other people depend on, and §61's version
  discipline starts being load-bearing rather than tidy.
- **`GOPRIVATE` and the `go.work`/`replace` arrangements become unnecessary**, in all three
  repos. They should be removed rather than left working, because each is a piece of
  scaffolding whose reason will no longer be true and which the next reader would otherwise
  have to reconstruct.
- **CI's `PLUGIN_REPO_TOKEN` secret stops being needed** (§31.2's testing section names it
  explicitly, "because the plugin repository is private").
- **The README's Releases link resolves for a reader for the first time.** It has never been
  followed by anybody who is not signed in to the org, so it has never actually been tested.

#### The repository is rebuilt, not just opened

**Ruled by the owner, 2026-09-17.** Going public is not flipping a visibility switch on this
repository as it stands. Four things happen first, and they are listed here because each is
cheap to do deliberately and expensive to discover halfway through:

1. **Git history is destroyed.** The public repository starts from a single commit. Nothing
   about how this was built is part of what ships.
2. **Every `CLAUDE.md` is removed**, here and in the plugin repositories.
3. **`docs/superpowers/` is removed** — the specs and implementation plans under it are working
   documents, not product documentation.
4. **The remaining documentation is rewritten to read as a human wrote it for another human.**
   Clean, plain, easy to follow. Professional without being stiff.

**What destroying history costs, so it is decided rather than discovered:**

- **Every tag stops pointing at anything**, and a GitHub release is a tag. The uploaded
  archives survive as assets, but the tag-to-commit mapping does not, so releases need
  re-tagging against the new single commit or the release history starts over. Decide which
  before the rewrite, not after.
- **`vcs.revision` baked into every shipped binary names a commit that no longer exists.** That
  is cosmetic — `infrena version` still reports its release number — but a bug report quoting a
  revision becomes unresolvable.
- **The three consumer repositories pin infrena by VERSION, not by commit**, so they survive
  intact as long as the tags are recreated. That is worth keeping true: a commit pin anywhere
  would turn this into a coordinated rewrite across four repositories.

**Point 4 is much larger than it sounds, and the surface was measured on 2026-09-17:**

| What | In Go | In Markdown |
| --- | --- | --- |
| `Amendment <n>` — internal process vocabulary | 116 | 194 |
| `contract.md` — **a file that does not exist in this repository** | 16 | 2 |
| First-person comments (`I`, `my own`) | 18 | 3 |
| `Found by the …` | 2 | — |

The `contract.md` citations are the sharpest of these and are a defect TODAY rather than only
on the day: sixteen comments name it as the authority for "Amendment 6 (owner ruling)" and a
reader who goes looking finds nothing. `AGENT.md` has the same problem in the other direction —
it lives in the reference plugin's repository, and §31.1 above refers to it as though it were
here. A pointer to a document nobody can open is worse than no pointer.

**PLAN.md does not ship.** Ruled by the owner, 2026-09-17: it moves into the notes vault and is
kept there. It is the design record rather than product documentation — enormous, and written in
the vocabulary of the people building it. Every decision's reasoning stays available to whoever
needs it; none of it is a thing a user of the tool should have to read past. What the public
repository documents is how to USE infrena and how to EXTEND it, which is README, `docs/` and the
provider guide.

**Tags, assets and releases are all destroyed, and one fresh release is cut at the current
version — here and in every plugin repository.** Ruled by the owner, 2026-09-17, which settles
the question the previous section left open: the release history does not survive and is not
re-tagged commit by commit. v0.11.1 is republished against the new single commit, and each
plugin republishes at whatever version it is on. Consequences that follow from that, rather than
needing separate decisions:

- The three consumer repositories require infrena by VERSION, so they keep working untouched as
  long as the fresh tag carries the same number. It must.
- `plugins.lock` in any existing project records a CHECKSUM of a release asset. Rebuilt assets
  are not byte-identical, so a lockfile written before the rebuild will not verify against one
  written after. Nobody has such a project — infrena is pre-release and nobody but the owner has
  used it — which is precisely why doing this now costs nothing and doing it later would not.
- Anyone who downloaded a binary keeps it; only the source history behind it is gone.

**`AGENT.md` is not moved, it is replaced.** Ruled by the owner, 2026-09-17: AI use is documented
as a policy rather than as a guide. Measured before acting, because the instruction assumed one
file and there are two: `infrena-provider-fake` carries BOTH a 699-line `AGENT.md` and a
2,346-line `docs/writing-a-provider.md`, covering the same subject at different depths. They have
already drifted — the same wrong sentence had to be corrected in both on 2026-09-17. So:

- `docs/writing-a-provider.md` is the provider guide and stays. It already covers everything
  `AGENT.md` does, plus requirements, cancellation and more depth.
- `AGENT.md`'s one unique section, its closing "checklist before calling a plugin done", folds
  into that guide. The rest is duplication and goes. **Done 2026-09-17, ahead of the day**, since
  nothing about it needed the repository to be public: the checklist is section 15 of the guide,
  `AGENT.md` is deleted, and the eight references to it in that repository's `CLAUDE.md` now name
  the guide.
- `docs/using-ai.md` is the new, separate document, and it is SHORT. It says AI may be used with
  no disclosure requirement, and that what does not change is authorship, understanding and the
  CLA's representation about your own original work. Written 2026-09-17 and already in this
  repository; it did not need to wait for the day.

#### What does NOT flip, checked 2026-09-17

- **`plugins search`'s unauthenticated wording is already correct and needs no change.**
  It was written about private repositories IN GENERAL, not about infrena's own, so a third
  party with a private plugin repo still gets the right answer and the reasoning in §31.3
  stands unaltered. What changes is only how often the unauthenticated path returns
  something — it stops being the edge case and becomes the common one, which the message
  already handles ("If it should be public, check the spelling...").


**Revisit when:** the product is feature complete. That is the stated gate, and going
public is the only thing that makes an outside plugin author possible.

**Phase B — now designed in full in §31.3, which supersedes these four bullets and moves
them out of §53.** In outline:

- `infrena plugins install` fetches release binaries.
- A committed `plugins.lock` records the resolved version, the source, and a SHA-256 per
  platform.
- The host verifies the checksum on every launch once a lock file exists.
- Phase A's search path is where install writes, so nothing moves.

§31.3 adds what this omitted: where infrena LOOKS (the `infrena-provider-*` naming
convention, plus sources a user trusts), the rule that a project may NAME a source but only
a user may TRUST one, and the offer to install a plugin a project references and the machine
does not have. It ships behind Phase 3 rather than in Phase 5, because AWS is the point at
which hand-placing a binary stops being a reasonable ask.

## 31.2 The plugin manifest: `plugin.yaml`

**Agreed 2026-09-13**, from a proposal by the `infrena-provider-fake` port
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
description: A fake provider for testing infrena without a cloud account.
infrena: ">= 0.2.0"
source: https://github.com/infrena/infrena-provider-fake
```

| Key | Required | Meaning |
| --- | --- | --- |
| `manifest` | yes | the format version of THIS FILE. Checked first, before any other key. |
| `name` | yes | the plugin's name: the binary is `infrena-plugin-<name>`, `Plugin.Name()` returns it, and every resource type is prefixed with it. The host already refuses a mismatch between the last two. |
| `version` | yes | `MAJOR.MINOR.PATCH`, and it must equal the tag this file is read at. |
| `protocol` | yes | the plugin protocol versions THIS RELEASE'S BINARY can speak. For a plugin built with `pkg/pluginsdk` that is exactly one — the `pluginproto.Version` of the infrena it was built against. A list only for a plugin that hand-rolls the protocol and genuinely negotiates several. |
| `platforms` | yes | `GOOS/GOARCH` for every published build. |
| `description` | yes | one line, for a search result to show. |
| `infrena` | no | the infrena releases this plugin is known to work with, in `pkg/semver`'s syntax. ABSENT means unconstrained. |
| `source` | no | where the plugin lives, for a search result to link. |

**Amended 2026-09-14: defined by the RELEASE, not by capability.** It said "every plugin
protocol version the plugin can speak", which invites exactly the wrong answer. An author
reads the host's `Supported` — `{4, 3, 2, 1}` since §31.1's protocol 4 — or remembers an
older release, and
writes `[2, 1]`. The binary still announces one number, so the manifest then claims a
protocol that binary cannot speak. Raised by the `infrena-provider-fake` session, which
met it the day the protocol moved.

Three consequences follow, and they are the reassuring ones:

1. **The field is fixed per release and never goes stale.** v0.1.1's `protocol: [1]` was
   true when written and stays true forever: that binary announces 1 and always will.
2. **A host protocol bump forces no re-release.** The old version stays in `Supported`, so
   an existing plugin release keeps working and keeps describing itself correctly.
3. **The NEXT release changes `protocol:` in the same commit as its infrena `require`
   bump**, because the rebuilt binary announces the new number. A plugin's own manifest
   test and its release gate are what enforce that, which is how this was caught.

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
VERSION (`version`, `protocol`, `platforms`, `infrena`), which differs per release. One
file serves both only because it is always read at a tag.

### Compatible means three things

1. **Format:** `manifest` is a version this build understands. Checked FIRST, so a newer
   manifest reports "this plugin needs a newer infrena to describe itself" rather than a
   parse error about a key nobody recognises.
2. **Protocol:** `protocol` shares at least one version with the build's own
   `pluginproto.Supported`. Already enforced at runtime by the handshake.
3. **Release:** `infrena`, if stated, allows the running build. A development build is
   EXEMPT, the same exemption §61.2 gives a project's own floor and for the same reason.

### Why the format is versioned, when configuration is not

§61.2 argues AGAINST versioning the configuration language, and this is the other case.
The distinction is who reads the file and when:

- Configuration is written and read by the same person at the same time, on one machine,
  and fails closed on an unknown key — which catches their typo.
- A manifest is written by a plugin author and read by every infrena build for years
  afterwards, over the network, with no way to upgrade the reader in step with the
  writer. Refusing unknown keys there means a 2026 infrena cannot install a 2027 plugin.

So: fail closed on unknown keys for a `manifest` version this build knows, and
tolerate-with-a-warning for one it does not.

### What the manifest deliberately does not carry

- **Checksums.** They cannot exist until after the build, so a hand-written,
  checked-in manifest cannot carry them honestly. `SHA256SUMS` is published as a release
  asset (infrena's own release workflow already does this), and §31.1 Phase B's
  `plugins.lock` is what records them per platform.
- **Asset names or download URLs.** A CONVENTION instead, mirroring infrena's own
  releases: `infrena-plugin-<name>_<version>_<goos>_<goarch>.tar.gz`, `.zip` on Windows.
  Install constructs the URL. One convention beats a field every author can get wrong.
- **Resource types.** `name` already implies them — a plugin serves `<name>.*` and the
  host refuses anything else — so "which plugin provides `aws.instance`?" is answerable
  from `name` alone.

### Where infrena reads it

**At install (Phase B).** `infrena plugins install` reads the manifest from the tag,
refuses a plugin failing any of the three rules, and only then fetches and checksums the
binary. A plugin with NO manifest installs with a warning rather than being refused:
Phase A is hand-placed binaries, which is every plugin today.

**Not in the handshake, for now.** That would catch a hand-placed binary too, and the
cheap form needs no manifest embedding — the handshake already sends
`{protocol, name, version}`, so `infrena` is one more optional string supplied the way
`Version()` is. Deferred because with the protocol at 1 and one plugin in existence,
rule 3 has nothing to catch yet. Recorded so it is not re-derived.

### What a plugin repository owes its own manifest

Its release workflow must assert that THREE things agree: the git tag, the manifest's
`version`, and the binary's `Version()`. That is the shape of the check already in
infrena's own release workflow, which builds for the host and refuses to publish a binary
that does not report the tag. A drift test between the manifest and the code is the
weaker substitute — it is a test someone can delete, where the release assertion blocks
the release.

`pkg/semver` is public so a plugin can validate its own `infrena:` field with the same
parser infrena will check it with, rather than a second implementation that drifts.

**`pkg/pluginmanifest` is that parser for the whole file** (built 2026-09-13, requested by
the port). `Parse` returns a `Manifest` and any warnings; `Validate` checks one built in
Go; `SpeaksProtocol`, `Supports` and `AllowsInfrena` are §31.2's three compatibility
rules, so the installer and a plugin's own test ask the same question of the same code.

It is public for the reason `pkg/semver` is, plus one practical one: reading YAML needs a
YAML parser, and a plugin repository whose rule is "standard library plus infrena" cannot
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

**Amended 2026-09-13.** This section previously said `cmd/infrena-plugin-test/` — the
fake provider as a binary inside this repository — and `providers/test/` unchanged.
Neither is what happened, and the difference is deliberate.

**The fake provider gets its OWN REPOSITORY**, `infrena-provider-fake`, building
`infrena-plugin-fake`. It has two jobs, and the second is why it moved out: it is the
only plugin whose source anyone can read, so it is also the reference implementation
every plugin author copies. A plugin living inside the engine's module can quietly
depend on something an external author cannot have — an internal package, a shared
test fixture — and nobody would find out until the first third-party plugin failed.
Outside, if it compiles, the dependency is one an outside author has too.

That is not a hypothetical: the first thing the port found was that
`internal/pluginhost.InProcess` is unreachable from another module, while the
authoring guide recommended testing against it. `pkg/plugintest` exists because of it.

**Amended 2026-09-13: AWS gets its own repository too**, `infrena-provider-aws`, building
`infrena-plugin-aws`. This section previously put it at `providers/aws/` inside this
repository with its own `go.mod`, on the reasoning that a separate module is enough to keep
the AWS SDK out of the core module's dependency budget. It is enough for that, and not
enough for the thing that actually matters, because **Go's internal rule is by import path,
not by module boundary.**

Measured, not assumed, on a scratch copy at `cd51fb7`: a nested module
`github.com/infrena/infrena/providers/awsprobe` with `replace => ../..` COMPILES while
importing `github.com/infrena/infrena/internal/pluginhost`, because the importing path sits
under the parent of `internal/`. The identical file in a module named
`example.com/outsideprobe` fails with `use of internal package
github.com/infrena/infrena/internal/pluginhost not allowed`. So the official provider —
the one whose code every AWS user reads and every third-party author imitates — would have
been the single plugin able to reach engine internals, and the compiler would never have
said so.

A separate module under this module's path also keeps the `replace` problem: it compiles
whatever is in the engine's working tree, committed or not, so its green suite proves
nothing about committed infrena.

**The rule this generalises to: a plugin's module path must not be under
`github.com/infrena/infrena/`.** That is what puts an official plugin on exactly the footing
a third-party plugin has, which is the only way the plugin API is tested by the plugins we
write ourselves.

**`providers/test` was a BUILTIN until that binary shipped; it is now the engine's TEST
DOUBLE.** While it was a builtin, the loader preferred a binary on the search path and
fell back to it, served over `pluginhost.InProcess` — the same handshake, protocol and
trust rules a subprocess gets, so a fallback rather than a second code path. The binary
shipped, and the fallback went with it: a shipped infrena now carries NO provider, `init`
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
  builds `infrena-plugin-fake` from its own repository in `TestMain` and runs §48's
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
  deleted a fresh install needs `infrena-plugin-fake` next to `infrena`. Releases ship
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
5. **DONE 2026-09-13.** `infrena-provider-fake` builds `infrena-plugin-fake` (v0.1.1, 8
   platforms); the state migration landed (§21.1); the `init` scaffold and `examples/shop`
   use `fake.*`; and **a shipped infrena carries no provider at all.**

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
     shares an origin with infrena-plugin-fake only because that binary was ported from
     it, and what keeps the two honest is that tests/integration runs the real one.

   CI checks out both repositories and sets `INFRENA_REQUIRE_PLUGIN`, which turns the
   integration suite's skip into a failure: a run that silently skips its integration suite
   reports green for tests that never executed. It needs a `PLUGIN_REPO_TOKEN` secret,
   because the plugin repository is private.
6. Documentation: `explain` and `validate` errors for a missing or incompatible plugin, and
   an authoring guide for the SDK.

---

## 31.3 Finding and installing plugins

**Designed 2026-09-13**, from the owner's requirements. This SUPERSEDES §31.1's four-bullet
"Phase B", and it moves: Phase B was filed under §53 (Phase 5), which was right while the
only plugin was the fake one and hand-placing a binary was a reasonable ask. **AWS makes it
wrong.** The moment a real provider exists, every user hand-places a binary, and the first
thing they hand-place is the thing that touches their production account. This ships behind
Phase 3, not two phases later.

The goal in one sentence: **a project says which plugins it uses, and infrena can find,
check, and install them without the user hunting for a URL.**

### What of this exists today

**Implemented 2026-09-16 (Units 1 and 2).** This section is a design written ahead of the
code, so it says what is built and what is still only designed, rather than leaving a reader
to go looking for code that is not there.

Built:

- **Source parsing** — both forms, in `internal/plugins`. A repository matching neither
  `infrena-provider-<name>` nor `infrena-backend-<name>` is refused when it is written, and
  one that matches carries the role its prefix implies.
- **Trusted sources** — `~/.config/infrena/plugins.yml`, read by `plugins.LoadTrusted`.
  `github.com/infrena` is always first and cannot be removed; a malformed file is an error
  naming the file, never a silent fallback.
- **A project naming a source** — the additive mapping form of `plugins:`
  (`version` and `source`), decoded and validated at its line in `infra.yml`.
- **`infrena plugins list`** — what is installed, its version and where it was loaded from,
  with no network request.
- **The forge client** — `internal/plugins/remote`, on `net/http` and `encoding/json`
  alone. It lists an owner's repositories under BOTH of GitHub's owner shapes
  (`/orgs/{owner}/repos` and `/users/{owner}/repos`, because no endpoint covers a user and
  an organisation at once), resolves a latest release tag and reads a file at that tag. A rate limit and a missing repository arrive from the same call and are
  DIFFERENT ERRORS, never collapsed.
- **A disk cache** — `remote.Cache`, an hour's TTL under the user's cache directory. Every
  failure on the read path is a miss, so a cache can never fail a command.
- **Compatibility filtering with a reason** — `plugins.Check`, composing the manifest's own
  `Supports`, `SpeaksProtocol` and `AllowsInfrena` and adding the sentence they do not have.
- **`infrena plugins search <name>`** — every match across every trusted or project-named
  source, each with its version, protocol and either "usable" or why not. It never picks
  between two sources answering one name, and `--refresh` asks the forge again.

**Implemented 2026-09-16 (Unit 3), which completes this section:** `plugins install`,
`plugins verify`, `plugins.lock`, checksum verification on every launch, and the interactive
offer to install a missing plugin. Nothing in this section is now unbuilt.

**Amended 2026-09-17:** backends are installable through the same commands. Sources and
searches know both prefixes, `list` and `search` show which kind each thing is, and
`plugins install` resolves the kind from the project before it asks — including the bare
form that installs everything a project declares. `plugins verify` was silently broken by
the backend lock key and is fixed: it reads the kind off the key, so an entry spelled
`infrena-backend-s3` is looked for as that binary rather than as a provider of that whole
string.

The only commands that make a network request are `plugins search`, `plugins install`, and
the one interactive prompt — never `validate`, `plan`, `apply`, `destroy`, `refresh`,
`discover`, `import`, `graph`, `explain` or `state`.

`internal/plugins` imports no HTTP client, and a test in that package fails if one is ever
added: the client in `internal/plugins/remote` sits ABOVE this package, never inside it.
`internal/cli` carries the other half — a test running each hot-path command with every
proxy variable pointed at a counting listener, asserting zero attempts.

### What the manifest already bought

§31.2 designed `plugin.yaml` for exactly this and the reasoning holds, so it is not
re-argued here — only the consequence. The manifest is fetched over HTTP at the git **tag**,
before any binary is downloaded, and it carries `name`, `version`, `protocol`, `platforms`,
`description` and an optional `infrena` constraint. So **"is this plugin compatible with
what I am running, and is there a build for my machine" is answerable from one small text
file**, which is what makes search possible without a registry, a server or an index.

Everything below is plumbing around that one fact.

### Sources: what infrena will look at

```yaml
# ~/.config/infrena/plugins.yml — the user's own, global
sources:
  - github.com/mycorp                       # an owner: search it
  - github.com/someone/infrena-provider-hetzner   # one exact repository
```

Two forms, and the distinction is whether a repository is named:

- **An owner** (`github.com/<owner>`, user or organisation) means *search this owner for
  repositories named `infrena-provider-*` or `infrena-backend-*`*. Both, from the one
  listing: an owner publishes what it publishes, and filtering to a single prefix would make
  a backend from a trusted owner invisible.
- **A repository** (`github.com/<owner>/infrena-provider-<name>`, or
  `github.com/<owner>/infrena-backend-<name>`) means *this one, exactly*.

**`github.com/infrena` is always searched and cannot be removed**, and that includes a
machine with no config directory at all: `os.UserConfigDir` fails with neither HOME nor
XDG_CONFIG_HOME, and a missing config DIRECTORY degrades exactly like a missing config
FILE - the official owner, and nothing else. A malformed file is still an error. It is where the official
plugins live, and a user who wants to avoid it can simply not name a plugin that lives
there. It is not a configurable default because a configurable default is a thing that gets
misconfigured into an empty list, after which `plugin: aws` reports that nothing matches —
a failure whose cause is invisible.

**THE NAMING CONVENTION IS LOAD-BEARING.** An owner search works by repository name, so a
plugin must live in a repository called `infrena-provider-<name>`, and `<name>` must equal
the manifest's `name` and the binary's `infrena-plugin-<name>`. This is the whole of the
"registry": no index, no server, no publishing step, no account. The cost is that a plugin
in a differently-named repository is only findable by naming the repository exactly, which
is the second form above and is why that form exists.

**It extends to state backends unchanged** (§52, 2026-09-16): a backend lives in
`infrena-backend-<name>` and ships a binary of that name. Nothing else about this section
changes — the same search directories, the same install path, the same one `plugins.lock`
— because the only thing that differs between the two kinds of plugin is a name pattern.
That is also why there is no `kind:` field in `plugin.yaml`: an owner listing can filter
`infrena-backend-*` locally from the one call that lists repositories, where a manifest
field would cost a request per candidate to answer a question the repository name already
answers for free, against a budget of sixty an hour. And it resolves a collision the field
could not — a provider `s3` and a backend `s3` are different repositories under the
convention and indistinguishable under a field.

In `plugins.lock` a backend is therefore keyed as `infrena-backend-<name>`, not as the
bare name a provider uses (`backendhost.LockKey`). One file, one key each: the two
binaries differ, so one key holding both could only either refuse a correctly installed
pair on every command or verify neither. The key deliberately omits the `.exe` a Windows
binary carries, because the lock is committed and read on other machines.

**The convention therefore covers two shapes, and an owner search lists both** (built
2026-09-17):

| | Provider | Backend |
| --- | --- | --- |
| Repository | `infrena-provider-<name>` | `infrena-backend-<name>` |
| Binary it ships | `infrena-plugin-<name>` | `infrena-backend-<name>` |

A provider's repository and binary differ by one word; a backend's are the same string.
That is not tidy, and it is also not negotiable: both names are already published.

**A release asset is named from the BINARY, not from the plugin name.** Every release
verified by hand is `<binary>_<version>_<goos>_<goarch>`, a `.tar.gz` or a `.zip` on
Windows, so a backend publishes `infrena-backend-s3_1.0.0_linux_amd64.tar.gz` where a
provider of the same name publishes `infrena-plugin-s3_1.0.0_linux_amd64.tar.gz`.
`remote.AssetName` takes the BINARY for exactly this reason: given a plugin name it would
have to guess a prefix, and the prefix it used to guess was the provider's, which
constructed an asset no backend release publishes.

### The ambiguity is one missing qualifier, not a flaw in the convention

Worth saying plainly, because the obvious reaction to "a provider and a backend can both be
called `s3`" is that the convention is broken. It is not. Everywhere the two kinds actually
coexist, they already coexist correctly:

| | Provider | Backend |
| --- | --- | --- |
| Configuration | `providers: - plugin: s3` | `backend: plugin: s3` |
| Binary on disk | `infrena-plugin-s3` | `infrena-backend-s3` |
| `plugins.lock` key | `s3` | `infrena-backend-s3` (`backendhost.LockKey`) |
| Not-found error | `pluginhost.NotFoundError` | `backendhost.NotFoundError` |

Different configuration keys, different filenames, already-distinct lock entries, and two
typed errors that each know what they were looking for. **The ambiguity exists in exactly
one place: the bare `<name>` argument to `plugins search` and `plugins install`**, the one
context carrying no other qualifier. The fix is to supply that missing qualifier there, not
to change a naming scheme that is doing its job everywhere else.

`plugins search` supplies it by SHOWING it: a `KIND` column, and both rows when both exist,
because a search is exploratory and both is the honest answer. `plugins list` shows the same
column, inferred from the binary's prefix, which is already unambiguous on disk and needs no
new state.

**`plugins install` resolves it in four rungs, cheapest and most certain first:**

1. **`--kind provider|backend`, if given.** The user said so; nothing outranks that. A
   `--kind` naming a kind nobody publishes is an error saying what DOES exist, never a
   silent fall back to the other one: they are different binaries doing different jobs.
2. **What the project declares.** `providers:` and the resource types that imply them say
   provider, `backend: plugin:` says backend, and a project naming both gets both, which is
   doing what was asked rather than choosing between them. **This outranks asking**, because
   making someone repeat on the command line what `infra.yml` already says is asking them to
   keep two places in step. The configuration is DECODED, never compiled, the same rule
   `backendFor` follows: a project with a broken resource must still be able to install the
   plugin that would fix it.
3. **The search result, if only ONE kind answers the name.** Nothing to disambiguate, so
   neither a project nor a flag is needed.
4. **Otherwise refuse**, showing both and naming `--kind`. The rule about two owners
   answering one name applies unchanged to two kinds answering it: choosing silently hands
   the user a thing they did not name.

When rung 2 decided, install SAYS which kind it installed. A choice made on the user's
behalf has to be visible.

### A project may NAME a source; only the user may TRUST one

This is the security decision, and it is the one place the design refuses the obvious
thing.

A project's configuration is checked into git and travels to whoever clones it. If project
configuration could grant a download source, then `git clone && infrena plan` would be
enough for a repository to introduce a place infrena fetches executables from. The prompt
would show the URL — and a prompt that appears routinely is a prompt people stop reading.

So:

```yaml
# infra.yml — a project may say where its plugin comes from
plugins:
  aws: ">= 0.3.0, < 0.4.0"              # today's form, unchanged
  hetzner:                               # the new mapping form
    version: ">= 1.2"
    source: github.com/someone/infrena-provider-hetzner
```

- **`plugins:` keeps accepting a bare constraint string.** The mapping form is additive and
  a project using the scalar form behaves exactly as it does today (§58). Considered and
  rejected: a separate top-level `plugin_sources:` key, which would split one plugin's
  facts across two places for no gain.
- **A source named by a project is a CANDIDATE, not a permission.** Installing from an owner
  the user has not already trusted requires an explicit confirmation that names the owner,
  and confirming records that owner in the user's own config. One mechanism — "approve an
  owner once" — rather than a per-install prompt nobody reads.
- **Non-interactive never approves.** With no TTY, an unapproved owner is an error telling
  the user which owner to approve and how. CI must be explicit; a pipeline that silently
  starts trusting a new binary publisher is the failure this whole subsection exists to
  prevent.

`github.com/infrena` is trusted from the start, because the binary making the decision came
from there. That is not a claim that we are trustworthy; it is the observation that a user
who does not trust us has already lost by running `infrena`.

### Detecting what is missing, and offering it

Today a missing plugin is a §44 error (`pluginhost.NotFoundError`) naming the `plugin:`
entry, every directory searched, and where to put the binary. That error is the hook.

When a plugin is missing **and** stdin is a terminal **and** searching is not disabled:

1. Search every trusted and project-named source for a plugin whose manifest `name`
   matches.
2. Filter to what can actually run here: `manifest` format supported, `protocol`
   intersecting `pluginproto.Supported`, `platforms` containing this `GOOS/GOARCH`,
   `infrena` allowing this build, and any `plugins:` version constraint satisfied.
3. Print every survivor — owner, version, description — and ask.
4. On confirmation, install, then **stop and say so**.

**Step 4 does not continue the command**, and that is deliberate. Installing a plugin
mid-`plan` means the first half of the run happened under different conditions from the
second, and the registry is already built by then. "Installed `infrena-plugin-aws` 0.4.1;
re-run your command" is one extra keystroke and leaves nothing to reason about.

**Every filtered-out candidate is still worth mentioning, with the reason.** A user whose
plugin exists but has no `darwin/arm64` build must be told that, not told nothing was found.
"No plugin named `hetzner` was found" and "hetzner 2.0.0 exists but publishes no build for
darwin/arm64" send a reader to completely different places.

**With no TTY, nothing changes**: the existing error is printed, plus the one-line
`infrena plugins install` command that would fix it. Never block on input that cannot come.

### The network is never on the hot path

`validate`, `plan`, `apply`, `destroy`, `refresh`, `discover`, `import`, `graph`, `explain`
and `state` **must not make a network request**, ever, for plugin discovery. Searching
happens in `infrena plugins search` / `install`, and in the interactive prompt above, which
is a user answering a question rather than a command reaching out on its own.

This is a hard rule and not a performance preference. A `plan` that consults the network is
a `plan` that behaves differently on a train, in a locked-down CI runner, and during a
GitHub outage — and invariant 6 says the same inputs produce the same plan.

### `plugins.lock`

Committed, one per project, and the thing that makes a downloaded binary trustworthy after
the fact:

```yaml
version: 1
plugins:
  aws:
    version: 0.4.1
    source: github.com/infrena/infrena-provider-aws
    checksums:
      linux/amd64: sha256:...
      darwin/arm64: sha256:...
```

- It records the **resolved** version, the source it came from, and a SHA-256 per platform.
  The source is recorded because "which owner did this come from" is the question a reviewer
  needs answered, and a name alone cannot answer it.
- **Checksums come from the release's `SHA256SUMS`, not from the manifest.** §31.2 recorded
  why the manifest has none: they postdate the build. Install fetches `SHA256SUMS` beside
  the archive, verifies the archive, and records the hash here.
- **The host verifies on every launch once a lock exists.** A binary replaced on disk after
  install is caught at the next command rather than never.
- `version:` is its own format version, per §61's rule that each boundary carries one.
  It joins that section's table.

**What this does and does not protect against, stated plainly.** `SHA256SUMS` is published
by the same party as the binary, so verifying against it catches corruption in transit and
tampering afterwards — not a malicious publisher, who would simply publish matching
checksums. What guards against that is the trust decision above (the user approved that
owner) and the lock file being reviewable in a diff. Signing is out of scope and recorded
below rather than implied.

### Two owners publishing the same plugin name

`plugin: hetzner` in configuration is a NAME, not a source, so two owners can both answer
it. Present every match and **never auto-pick** — not the first alphabetically, not the
higher version, not the official one. The choice is recorded in `plugins.lock`, so it is
asked once and then visible in review.

The same rule as the ambiguous import selector (`internal/cli/import.go`), and for the same
reason: when two candidates both answer what the user asked, choosing one silently gives
them a thing they did not name.

### Rate limits, caching, and the error that must not be confused

Unauthenticated GitHub allows 60 requests an hour. An owner search costs one request to list
repositories, plus one per candidate for its latest release, plus one per manifest. Two
owners with several repositories each can exhaust that in a single invocation.

- **Search results are cached on disk** under `~/.cache/infrena/plugins/` with a TTL, so a
  repeated search is free. `--refresh` bypasses it.
- **A token is accepted**, from `INFRENA_GITHUB_TOKEN` or `GITHUB_TOKEN`, and raises the
  limit. It is optional and only read for search.
- **INFRENA MUST NEVER CLAIM A PLUGIN DOES NOT EXIST WHEN IT MERELY COULD NOT SEE IT.**
  This is the general rule, and it has at least two doors:
  - **A rate limit reported as "not found".** Both come back from the same API call, and
    conflating them tells a user their plugin does not exist when what actually happened is
    that they searched four times in an hour. The message says the limit was reached, when
    it resets, and that a token raises it.
  - **An empty unauthenticated listing reported as "not found".** A private repository
    answers an unauthenticated listing with 200 and an empty array, which is
    indistinguishable from an owner who publishes nothing, so a search with no token has
    learned nothing about whether the plugin exists. The quiet door is the more dangerous
    of the two: a refusal is at least visible, while this one looks exactly like a complete
    answer. `plugins search` says it ran unauthenticated, that private repositories are
    invisible that way, and names `INFRENA_GITHUB_TOKEN` and `GITHUB_TOKEN` as the action
    (§44). The spelling hint stays, but never as the only explanation. With a token, the
    definitive wording is correct and is used.

  The test for any future search path is the same question: could this answer have come
  back identically from a thing that exists? If yes, the wording says what infrena saw,
  not what is.

### The commands

Deliberately four, one of which takes two shapes:

- `infrena plugins list` — what is installed, its version, and where it was loaded from.
  Answers "what am I actually running" with no network.
- `infrena plugins search <name>` — every match across every source, with why any was
  rejected.
- `infrena plugins install <name>[@version]` — resolve, check, download, verify, write the
  lock. Writes to `<project>/.infra/plugins/`, or `~/.local/share/infrena/plugins/` with
  `--global`. `--kind provider|backend` settles which kind was meant when there is no
  project to say, and is what the fourth rung above tells the user to reach for.
- `infrena plugins install` with **no name** — install every plugin this project declares,
  the providers and the backend alike, which is what a fresh clone needs. It cannot be
  ambiguous by construction: the project states both the name AND the kind of each one, so
  none of the resolution above runs. What is already installed is skipped rather than
  reinstalled or reported as an error, and one failure does not hide the others — every
  plugin is reported and the command fails at the end. `--kind` with no name is refused for
  the same reason resolution is unnecessary: there is nothing for it to apply to.
- `infrena plugins verify` — re-check installed binaries against `plugins.lock`. What CI
  runs.

**Phase A's search path is where install writes, so nothing moves** (§31.1). Install is a
way to populate the directories that already exist, not a second mechanism beside them —
which is also why a hand-placed binary keeps working and keeps winning when `--plugin-dir`
names it.

### Deliberately NOT in this design

Recorded so that each is a decision rather than an oversight:

- **A central registry or index.** The naming convention plus one file per repository is
  enough, and a registry is a service to run, secure and keep available.
- **Publishing.** A plugin author tags a release and pushes archives with `SHA256SUMS`,
  which is what the existing release workflows already do. There is nothing to publish
  *to*.
- **Signing.** `SHA256SUMS` is not a signature and this section says so rather than implying
  otherwise. Sigstore or minisign would be an addition, and a real one, but it needs a key
  story before it needs code.
- **Forges other than GitHub.** The source syntax is host-prefixed (`github.com/...`)
  precisely so another host can be added without changing the shape of what a user wrote.
  Not built until someone wants it.
- **Auto-update.** A plugin version changing without a user asking is a plan changing
  without a user asking.
- **Install scripts.** A plugin is one static binary. Nothing in an archive is ever
  executed except the binary the manifest names, and only when a command needs it.

### Extraction is a security boundary

**ADDED 2026-09-16.** This section described downloading and unpacking archives without
saying what unpacking may do, and the omission is the dangerous kind: an archive is
attacker-controlled data, and a naive extractor is how it becomes attacker-controlled
FILESYSTEM.

The rules, and each is a refusal rather than a sanitisation:

- **An entry whose path escapes the destination is refused, and the archive with it.** Not
  sanitised, not skipped — refused, naming the entry. `../../../.ssh/authorized_keys` inside
  a tarball is not a mistake to tidy up, and an extractor that quietly drops it will happily
  extract whatever came next.
- **Exactly one file is extracted: the binary the manifest names.** Everything else in the
  archive is ignored. A plugin is one static binary (above), so an archive carrying more is
  either careless or hostile and there is no case where infrena needs the rest.
- **No symlinks, no hard links, no devices, no directories with surprising modes.** Only a
  regular file. A symlink is the same escape as a `..` path wearing different clothes.
- **A decompression bound.** A few hundred kilobytes of gzip can expand to gigabytes, and an
  install that fills the disk is a denial of service that survives a reboot. Cap the bytes
  written and refuse past the cap.
- **The binary is written to a temporary path, verified, then moved into place**, so a failed
  or refused install never leaves something runnable where a later command would find it.

`SHA256SUMS` is verified BEFORE extraction, so a tampered archive is refused without being
opened at all. That ordering is the cheapest of these defences and the one that must never be
reversed for convenience.

### What Unit 3 shipped, and the three limits worth knowing

**ADDED 2026-09-16, after installing the real `infrena-plugin-aws` 0.5.0 from the real
GitHub and then corrupting it on disk.** Everything above is now built; these are the edges
a reader will meet that the design above does not imply.

- **`install <name>@<version>` can only satisfy the repository's LATEST release.** A search
  reads each repository's latest tag and its manifest at that tag, which is what makes a
  search cost one request per repository instead of one per release. So a version that is
  not the latest is refused, naming the versions that ARE published, rather than silently
  installing something else. Installing an older release needs a search that walks a
  repository's releases, which is not built and is not pretended to be.

- **The launch check hashes the binary and compares it under `GOOS/GOARCH`**, the same key
  install wrote (`plugins.PlatformKey`, in the lock's own package so there is one spelling
  of it). It happens BEFORE the process is started, in `pluginhost.Loader.open`, because
  after the process has started is after its code has run. A plugin the lock does not
  mention is not hashed at all, so a project that installed one plugin does not pay for the
  other nine. A lock that cannot be READ refuses every launch rather than being treated as
  absent: the file is what says which executables are trustworthy.

- **A binary that fails its checksum gets its OWN error, not the missing-plugin one.**
  `pluginhost.LockError`, reported by `internal/providers` as "does not match the checksum
  recorded for it in plugins.lock" with the advice to install it again or restore it. The
  first version of this reported a corrupted binary as "the aws plugin is not available"
  and advised installing a plugin already sitting on disk — §44's worst case, a suggested
  action the reader cannot act on. Found by hand, not by the suite, which is why the
  hand-verification step exists.

### The offer, in practice

The three conditions are `stdin` being a terminal, `--output` being unset, and
`INFRENA_NO_PLUGIN_SEARCH` being unset. The offer hangs off the path a command takes when
its configuration did not compile — which, for a missing plugin, it never can — so "install,
then stop" is structural rather than remembered: by the time anything is installed the
command has already failed and its only remaining act is to return.

**Detecting a terminal without an ioctl is the one compromise.** The third-party budget has
no room for a terminal library, so the test is "stdin is a character device, and is not
`/dev/null`". The second half is not a nicety: `/dev/null` is a character device, and is what
`go test`, cron, systemd and most CI runners hand a process. Without that exclusion this
package's own suite made a real request to api.github.com from a failing `plan`, which is
the hot-path rule broken by the very feature meant to respect it.

### Build order within this section

**AMENDED 2026-09-16, and the amendment swaps steps 1 and 2.** As written, step 1 built
`plugins.lock` and `verify` before anything could WRITE a lock — a reader with no writer,
which is the "declaration nothing consults" defect this document refuses elsewhere. Nobody
hand-writes SHA-256 checksums, so `verify` shipped first would have been untestable against
anything a user could actually produce.

The lock now arrives WITH install, in step 4, which is the first thing that writes one. The
original rationale — settle the file formats before the network code depends on them —
still holds for the formats the network code actually reads, which is `plugin.yaml`
(§31.2, already shipped). Nothing in search reads the lock.

The implemented order is therefore:

1. Sources and trust, plus `infrena plugins list` — no network at all. **SHIPPED
   2026-09-16.**
2. Manifest fetch plus `infrena plugins search` — the first network code, read-only, and
   the place where compatibility filtering and its messages get written. **SHIPPED
   2026-09-16.**
3. `infrena plugins install`, download, checksum verification, `plugins.lock` and
   `infrena plugins verify` — the lock format and its only writer together. **SHIPPED
   2026-09-16.**
4. The interactive offer on a missing plugin, which is everything above plus a prompt.
   **SHIPPED 2026-09-16.**

Superseded, retained so the change is visible:

1. `plugins.lock` and `infrena plugins verify` — the parts with no network at all.
2. `infrena plugins list` — no network, immediate value, exercises the loader's reporting.
3. Manifest fetch plus `infrena plugins search` — the first network code, read-only, and
   the place where compatibility filtering and its messages get written.
4. `infrena plugins install`, download and checksum verification.
5. The interactive offer on a missing plugin, which is everything above plus a prompt.

The order is not arbitrary: each step is useful alone, the network arrives after the file
formats are settled, and the prompt — the only part that can surprise a user — is last,
built on machinery already exercised by explicit commands.

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
infra state migrate [--check] [--force]

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

## 37.1 Exit codes

| code | meaning |
| --- | --- |
| `0` | success, no changes |
| `1` | error |
| `2` | success, changes present — and `state migrate --check`: a migration is pending |
| `3` | `state migrate --check`: the migration is already complete, so `migrate_from:` is stale |
| `4` | `state migrate`: the two backends hold different state, and a person has to decide |
| `77` | changes require approval that this run cannot obtain |

The first three are what the source calls spec §16. `2` is what makes CI gating possible at
no cost: `plan` exits 2 when it has something to propose, so a job fails on drift without
parsing anything. `validate` never exits 2, because it computes no changes.

**77 is sysexits.h's `EX_NOPERM`, added 2026-09-15**, and being a fourth row it is a
documented change to a product API. It fires when the plan has changes, `--auto-approve` is
absent, `--plan` is absent, **and** approval cannot be obtained. Two causes, detected in two
different places:

- **`--output` is set**, so nobody is reading stdout and nobody can see a prompt. Checked
  explicitly, straight after the plan is computed.
- **stdin is already at EOF**, so nobody is there to type. Detected at the prompt itself,
  inside `confirm`.

Both sit **before `withLockedEnvironment`**, which is the property worth keeping: a run that
exits 77 has taken no lock and mutated nothing, whichever cause fired. It covers `destroy`
as well as `apply` — destroy's higher bar, typing the environment name, does not make
approval obtainable.

It replaces a genuinely misleading outcome. A piped `apply` used to reach `confirm()`, read
end of input, and exit 1 saying you must type `yes` to approve: advice nobody in that
pipeline could have taken, which is precisely what §44 says a suggested action must never
be. It now names an escape that works — `--auto-approve`, or a saved plan with `--plan`.

**3 and 4 arrived with `state migrate --check` on 2026-09-17**, and being new rows they are a
documented change to a product API in the same way 77 was. They are the answers a pipeline
needs that 0 and 1 cannot carry:

- **`2` is shared with `plan` deliberately, and is not a collision.** Both codes mean one thing
  to a script — something is pending, run the corresponding command — so a `plan` that exits 2
  and a `--check` that exits 2 want the same shape of response. A migration is pending when the
  source holds state the destination does not.
- **`3` is "already complete", distinct from 0** so a pipeline can tell "no migration was ever
  configured" from "the migration is done and the `migrate_from:` block is now stale", and
  remind somebody to remove it. A script treats both the same and carries on.
- **`4` is a conflict, distinct from 1** for exactly the reason 77 is: a pipeline that cannot
  tell "this failed" from "this needs a person" will treat both the same, and the two want
  opposite responses. Retrying a conflict achieves nothing; it needs a human to decide which
  end is the real record. Both `--check` and the migration itself report a conflict with 4, so
  a pipeline that ran the migration directly does not have to learn a second spelling of the
  answer the check would have given it.
- **`1` stays "error"**: a backend that would not open, configuration that would not read. A
  question nobody could answer is never reported as "nothing to do".

**141 is not one of these, and SIGPIPE is ignored so that it cannot become one.** Go lets
SIGPIPE kill a process that writes to a broken fd 1 or 2, which is right for a cat-like
filter and wrong for a command whose exit code is the product: `infrena plan dev | head -1`
has to still say 2. `cmd/infrena` therefore ignores the signal, and the failed write returns
an error that every stdout print site already discards — the reader left on purpose, which is
its decision and not a failure infrena reports. A failed write to an `--output` FILE stays a
real error and is still reported; the two cannot be confused, because `--output` routes
stdout to `io.Discard`.

It matters most for `apply`, which is why the proof is an integration test that runs the
built binary with fd 1 on a closed pipe rather than a unit test over a fake writer: a signal
kill walks past the state write the executor makes under `context.WithoutCancel` and past
`withLockedEnvironment`'s release, so the run that died mid-flight left `.infra/state/<env>.lock`
behind and blocked the next one. Progress output made it far likelier, because an apply now
writes continuously instead of twice.

## 37.2 `--output`

**`--output` silences stdout, for every command.** One rule, no exceptions: with the flag
set, everything the run produces goes into the file and stdout is byte-empty, so a frontend
tailing the file never has to strip a human-readable half out of the terminal as well.
Diagnostics are unaffected and stay on stderr, because a run that fails must say so on a
channel the operator sees whether or not anything is reading the file.

**Progress goes to stdout, and only when `--output` is absent.** It is human output, not a
diagnostic, so it never goes to stderr — splitting one narrative across two channels helps
nobody, and the machine-readable file already exists for a consumer that wants the
structured form. Lines are **append-only**: no cursor control, no in-place rewriting, no
spinner. stdout here is as often a CI log or a redirected file as a terminal, so escape
sequences are noise a reader has to strip, and append-only means no TTY detection and one
behaviour to test rather than two. Lines are emitted in **completion order** and are
therefore deliberately NOT byte-stable between runs; invariant 6's determinism test compares
from the `Plan for project` line onward for exactly that reason.

**`--output` writes ONE format for every command**, a newline-delimited JSON report: a `meta`
line carrying the format version, then `event`, `observation` and `diagnostic` lines as work
happens, and a final `result` line, so a consumer tails the file and reads the outcome from
the last line. Reports redact through `pkg/value.Format`, the single redaction path.

**`plan --output` writes that same stream**, not the bare JSON document it used to write: a
`meta` line and a `plan` line carrying the artifact verbatim, and **no `result` line** — the
plan is the product. The artifact still holds sensitive values in cleartext, because
`apply --plan` reads them back and needs the real ones, which is why the file is 0600 like
every other `--output` file. `operations` still lists EVERY resource, `kind: "noop"`
included: it describes the whole plan, not only its changes, and `Plan.HasChanges()` is what
decides the exit code.

**`apply --plan` reads BOTH envelopes** — the bare artifact and the stream. That is a
compatibility requirement, not a courtesy: plans saved by earlier releases are on disk, and
refusing one would break applying a plan that was already reviewed. The envelope is sniffed
on the first line's `type` field, and `planner.DecodePlan` carries an independent guard
refusing any document that has one, so a caller that skips the sniffer fails loudly instead
of decoding a meta line into a plan with no operations and applying nothing.

## 37.3 `init`

`infrena init [dir]` takes an optional directory and defaults to **`./infrena`**, so a fresh
project sits beside the application and §4.2 finds it with no `--chdir`. It writes the §4
layout — `infra.yml`, `resources/network.yml`, `vars/default.yml`, `vars/production.yml`,
`vars/staging.yml`, `modules/.gitkeep` and `.gitignore` — and **refuses to overwrite**,
checked across every path before anything is written, so a refusal leaves the directory
exactly as it was.

The scaffolded environments are `production` and `staging`, **matching the `vars/`
filenames**. An environment no file names, or a file naming an environment nothing declares,
is dead configuration in the one file a user reads to learn the language. `infra.yml` also
pins the floor `infrena: ">= 0.7"` (§61.2).

**`--provider` decides whether the example resource is live.** `--provider aws` scaffolds a
real `providers:` block and a live `aws.vpc`; any other value scaffolds `<provider>.network`.
**With no flag the example is COMMENTED OUT.** A shipped infrena carries no provider, and what
`init` writes must pass `infrena validate` immediately on a machine with nothing installed —
an init whose output does not validate teaches the language wrongly at the one moment a user
has no way to tell. Commented out it validates bare and still shows the shape; naming a
provider is the user saying they have one.

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

**Both keys SHIPPED 2026-09-18.** What each means, and the decisions that are not
obvious from the YAML:

**`require_approval: true` makes `--auto-approve` refuse, with exit 77.** That is the
whole mechanism: a protection a flag can switch off is not one, because the flag is a
line in the pipeline that was going to run anyway. Two approvals are accepted, and both
involve a person — somebody confirming at a terminal, or a SAVED PLAN applied with
`--plan`. The second is stronger than typing "yes", not weaker: it is applied without
recompiling and refused outright if state moved since it was made, so what runs is what
was read. `apply --plan` therefore bypasses the gate by construction rather than by a
special case, and that is the route CI takes.

**`--approved-by` is a record, not a permission.** It takes free text — a pull request
URL, a name, a ticket — and writes it to the run's report line. It permits nothing:
infrena cannot tell a real pull-request URL from an invented one, and a protection
resting on an unverifiable string is a protection in name. Verified approvals need to
read the forge, which is one forge at a time and is the platform's job (docs/open-core.md).
What core can honestly do is require that an approval was obtained and record what was
claimed.

**`prevent_destroy: true` refuses a destroy at PLAN time**, the same timing its
resource-level namesake uses and for the reason that one gives: the refusal must arrive
before the approval, not after. It covers `infrena destroy` and a resource removed from
configuration. **It deliberately does NOT refuse a REPLACE.** A replace destroys and
recreates, so refusing one is defensible — and it would make any ForceNew attribute
unchangeable in a protected environment, which is a far larger restriction than anyone
asking for destroy protection has in mind, discovered the first time production needed a
real change.

**Both inherit down `extends`, nearest declaration winning**, so a child inherits by
saying nothing and drops a guard by writing `false`. Inheriting is the safe direction:
the mistake nobody can see is a child that silently lost its parent's protection. The
chain records WHICH environment declared each, because a refusal that says "production
prevents this" to somebody running `apply prod-eu` sends them to the wrong file.

**The keys are recognised before the variable-override fallback**, and that ordering is
the whole risk: every unrecognised key in an `environments:` block becomes a variable, so
`require_approval: true` falling through would silently declare a VARIABLE of that name
and protect nothing — precisely what `type:` did for seven milestones. Setting either
twice is refused rather than last-wins, because an environment can be declared in both
`infra.yml` and `environments/<name>.yml`, and a silent last-wins resolving to `false`
would disable a guard its author believes is on.

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
`infrena-provider-fake` — a separate repository — for the fake provider as a binary.

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
`infrena-provider-aws`, building `infrena-plugin-aws`. See §31.1's amendment of
2026-09-13 for why it is not `providers/aws/` inside this repository.

**Then §31.3, finding and installing plugins, before anything in Phase 4 or 5.** The
ordering is AWS first and install second rather than the other way round, because AWS is
what makes install necessary and is also what says whether the design is right: until a
plugin exists that someone outside this project wants, every requirement for installing one
is a guess. It must not wait longer than that, though — a released AWS provider with no
install path means every user hand-places the binary that touches their production account.

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

**Designed 2026-09-16** (`docs/superpowers/specs/2026-09-16-remote-state-design.md`). The
six bullets this section used to carry named the deliverables and not the shape, and the
shape is the part that constrains everything else. Four rules:

**A backend is a plugin.** Not an `if` in the engine. It lives in
`infrena-backend-<name>`, ships a binary of that name, is found by the same search
`plugins install` populates, and is verified against the same `plugins.lock` — §31.3's
convention extended, not a second mechanism. `pkg/backend` is the contract an author
imports, `pkg/backendproto` the wire, `pkg/backendsdk` the whole of their `main()`.

**Local is the bootstrap and is BUILT IN.** It is the backend that works before anything
is installed, which is why it cannot be a plugin: something has to hold state for the
project that has not installed one yet. **A shipped infrena carries no REMOTE backend**,
exactly as it carries no provider. A project with no `backend:` block gets local and
starts no process.

**Every backend must lock.** A backend that does not implement locking is refused **when
it loads**, never at apply time — locking is part of the interface rather than an option,
so one that cannot lock does not compile. Invariant 5 is absolute: two applies cannot
mutate one environment concurrently.

**`validate` resolves a named backend's BINARY, and stops there.** Added 2026-09-17, after
`infrena validate` was found reporting "✓ Configuration valid" for a project whose
`backend:` named a plugin that was not installed, while `plan` on the same project failed
with "no binary for state backend". validate already refuses a missing PROVIDER plugin, and
it is the cheap gate a CI pipeline runs first, so that combination is a pipeline approving a
project that cannot run. Resolving a binary is a filesystem lookup, which is why it sits
inside validate's promise not to contact providers; `backendhost.Verify` is deliberately
written as a PREFIX of `Open` — find, then check the lockfile, then stop — so the two cannot
drift about where a backend lives. `migrate_from:` is checked the same way, since a
migration whose source cannot be opened fails as completely as one whose destination cannot.

**What that leaves unchecked, on purpose:** the block's own CONTENTS. A backend validates
its configuration in `Configure`, and for the S3 backend `Configure` also proves the store
honours conditional writes — a network round trip, which validate may not make. So a
credential wrongly written into `backend:` is still only refused once the plugin runs, which
is at `plan`. Closing that would need a protocol method that parses without connecting;
`backendproto` v1 has none, and adding one is a version bump rather than a quiet addition
(§61).

**`backend:` never interpolates.** `plugin:` is the only key infrena reads; every other
key crosses to the backend untouched, so an unrecognised key here is not an error (the
engine cannot know what an s3 backend accepts) while a missing `plugin:` is. A `${...}`
anywhere in the block is a diagnostic explaining the ORDERING CYCLE: state is read before
anything is compiled, and compiling is what resolves variables — `destroy`, `refresh`,
`discover` and `import` never compile at all — so there is no point in any run at which a
value there could be filled in. It is not a feature that has not been got round to, and a
later request to "just support variables in `backend:`" is a request to break the
ordering.

Build order, each step useful alone:

1. **The backend boundary. SHIPPED 2026-09-16.** `state.Backend` widened to the seven
   methods the CLI calls, the public types moved to `pkg/backend`, the protocol and host
   built, `backend:` decoded, and `backendFor` routing to a plugin or to local. Nothing
   user-visible changed, and the whole existing suite passing untouched is the proof the
   extraction was faithful.
2. **The S3-compatible backend plugin. SHIPPED 2026-09-17**, `infrena-backend-s3` v0.1.0.
   Locking is a conditional PUT, and the store is PROVEN able to do one at configure time
   rather than trusted: a probe writes twice and requires the second to be refused, which
   catches both a store that refuses the header (Backblaze B2, `NotImplemented`) and one
   that ignores it and overwrites, which is the silent case. Verified against real AWS S3
   and MinIO — full live suite, infrena's conformance suite, and ten goroutines racing one
   lock leaving exactly one winner — and against B2, where the refusal is the pass.
3. **State migration. SHIPPED 2026-09-17.** `infrena state migrate` copies state from the
   backend a temporary `migrate_from:` block names into the one `backend:` names, with
   `--check` for pipelines and the local → S3 → local round trip proving the two backends
   agree about what state is. Four rules decided it:
   - **It COPIES and never empties the source.** A bug in the migration has to be
     survivable, so the old backend stays a fallback you can point `backend:` back at.
     The cost is that the old state lingers, every secret in it included, until the user
     deletes it, and every message the command prints says so.
   - **Three cases, not two**, compared under both locks before anything is written:
     destination empty → copy; destination identical → no-op and SUCCEED; destination
     different → refuse. The middle case is what makes a CI re-run safe, and failing there
     is what would put `--force` in a workflow file forever — the same hazard as a
     `lock: false` escape hatch arriving by the same route, which is why the refusal
     mentions the flag last rather than first.
   - **`migrate_from:` is inert for every command except `state migrate`**, with one
     exception that is a GUARD and not an action: while the destination is empty and the
     source holds state, ordinary commands refuse. Without it, the window between the
     configuration landing and the migration being run is a window in which a command
     reading only `backend:` sees every resource as unmanaged and an apply recreates all
     of it. The destination is checked first, so a completed migration never opens the
     source at all.
   - **Comparison is over encoded bytes with `Serial` and `UpdatedAt` zeroed**, because
     every `Put` stamps both; without that, every completed migration would read as a
     conflict and the no-op case could never fire. Nothing else is zeroed, so a genuine
     difference in what is managed still refuses.
4. **Concurrency tests. SHIPPED 2026-09-17.** Both halves now exist in
   `tests/integration/m3_test.go`, each driving two genuinely overlapping OS processes.
   The one-environment half (`TestConcurrentApplyToOneEnvironmentSerializes`) was already
   there: the second apply is refused with "is locked" and the fake cloud ends up holding
   exactly one resource. The new half
   (`TestConcurrentApplyToTwoEnvironmentsOverlaps`) covers the other direction, and its
   point is that "both succeeded" is a test that cannot fail — a global lock would
   serialise the two applies and both would still exit 2. So it asserts ELAPSED TIME:
   with `latency_ms` at 500, overlapping finishes in ~540-600ms and serialised takes
   ~1.1s, and the bound is 800ms. Watched fail: running the same two applies one after
   the other reports 1.087s against the 800ms bound. Ten consecutive runs stayed inside
   537-602ms.

State encryption is documented rather than implemented: `docs/state-backends.md` states
the trust boundary — **state reaches a backend in cleartext, exactly as values already
reach a provider** — and client-side encryption is deferred because it needs a key story
before it needs code.

---

# 53. Phase 5 — Production Features

**`infrena plugins install` has MOVED OUT of this phase** — it is designed in §31.3 and
ships behind Phase 3. It was filed here while the fake provider was the only plugin, when
hand-placing a binary was a reasonable ask; AWS is the point at which it stops being one.

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
    image: ${var.application_image}
    replicas: ${var.replicas}
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

**Decided 2026-09-13.** Six format versions already exist, independently, each at 1 when this
was written:

| Version | Package | Guards | How it moves |
| --- | --- | --- | --- |
| `state.CurrentVersion` | `internal/state` | the state file | a migration chain, one step per version |
| `pluginproto.Version`, `Supported` | `pkg/pluginproto` | the plugin wire | negotiated per plugin; `Supported` is a SET. **At 4 since 2026-09-15** (§31.1's table: 2 for `optional` and `aliases`, 3 for `References`, 4 for `system_owned`), with 3, 2 and 1 still supported |
| `planner.PlanVersion` | `internal/planner` | the plan artifact | additive, with a frozen-keys test. **Deliberately still 1 after 2026-09-15**: the plan document's own schema did not change, only the envelope it travels in (§37.2), and `DecodePlan`'s version check is an outright refusal — so a bump would reject every plan already saved to disk in exchange for nothing |
| `report.Version` | `pkg/report` | `--output` reports | additive. **At 2 since 2026-09-15**: the format gained the `plan` line (§37.2), so a consumer that only understands 1 can tell |
| lockfile `Version` | `internal/modules/source` | `modules.lock` | internal |
| `plugins.lock` `version` | Phase B (§31.3) | the resolved plugins and their checksums | internal |
| `backendproto.Version`, `Supported` | `pkg/backendproto` | the state-backend wire | negotiated per backend; `Supported` is a SET. **At 1 since 2026-09-16** (§52) |
| `pluginmanifest.Version`, `Supported` | `pkg/pluginmanifest` | `plugin.yaml` | **At 2 since the 2026-09-14 rename** — `infrata:` became `infrena:`, a renamed key rather than an added one. 1 stays readable, so a pre-rename release keeps meaning what it meant |
| cache `Version` | `internal/modules/source` | module cache metadata | internal |

**The backend wire is the seventh of these and is INDEPENDENT of `pluginproto`** (added
2026-09-16, §52). The two evolve for unrelated reasons: a change to how state is stored
must not force every provider to cut a release, and a change to how resources are
described must not force every backend to. They share a transport — newline-delimited
JSON over stdio, the same handshake and the same cookie — because that part has no reason
to differ and a second transport would be a second thing to get wrong.

**They stay independent, and that is the decision.** Each guards a different boundary
with a different lifetime, and one shared number would mean a state migration every
time a report gained a field — a migration being the most expensive thing in the list.
Nothing may collapse them.

## 61.1 The product version

`infrena` itself is SEMVER, `v0.x.y` until the configuration language stops moving.
It is not a seventh format version; it is the release, and its bumps are DEFINED in
terms of the table above, because otherwise "minor" means nothing:

- **patch** — no format version changes.
- **minor** — may ADD a format version, and must still read every older one. May add
  configuration syntax. May add a `pluginproto` version while leaving the previous one
  in `Supported`, so existing plugins keep working.
- **major** — may drop support for an old format version, or remove configuration
  syntax. A plugin may stop working, and the handshake says so by name.

**AMENDED 2026-09-13, because the rule above told me the wrong answer.** Defined purely in
terms of the format table, it is silent on the change users actually notice: **the engine
getting STRICTER.** v0.2.0 tightened requirement satisfaction to per-account, so a project
that validated under v0.1.0 — a database in one provider instance, its network in another —
stops validating. No format version moved, no configuration syntax was added or removed, so
the letter of the rule said "patch". A user upgrading by a patch release and finding their
project refused would be right to call that a broken promise.

So the test is **what an existing project does**, not which numbers in the table moved:

- **A release that refuses configuration a previous release accepted is at least MINOR**,
  even when nothing in the format table changes. The same goes for a command that starts
  refusing an operation it used to perform — v0.2.0's `import` refuses adopting a provider
  ID another address already manages, which was a data-loss fix and still a behaviour
  change a user can be surprised by.
- **Adding a key to a format without moving its version is also at least MINOR.** The plan
  artifact gained `depends_on` and the value wire gained `expr`, both additive, both leaving
  `PlanVersion` at 1 — correct, because an older reader ignores an unknown key. But an older
  reader ignoring a key it needed is a behaviour difference, and the release it appears in
  should say so.
- **Patch stays what it says**: no format changes, and nothing a working project notices.

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
infrena: ">= 0.4"

plugins:
  aws: ">= 0.3.0, < 0.4.0"
```

Comparison operators on `MAJOR.MINOR.PATCH`, comma meaning AND, parsed by hand rather
than by a semver library (§31.1). Absent means no constraint, so every project written
before this key behaves exactly as it did. Present, it turns "unknown key `foo`" into
"this project needs infrena >= 0.4; this is 0.3.1", which is the message a team
sharing a repository between CI and laptops actually needs.

**Checked immediately after decoding**, before anything else runs: a binary that cannot
understand a project must say so once, rather than reporting twenty unknown-key errors
that are all the same problem.

**This gap CLOSED on 2026-09-13, as a side effect rather than a goal.** It used to read:
the check lives in the compiler, so the four commands that never compile — `destroy`,
`refresh`, `discover`, `import` — do not apply it. They do now: those commands build
their provider instances through `compiler.VariableScope`, which runs the floor check as
stage 1.5 (§12.1, amended). A binary too old to understand a project no longer refuses
to `plan` it and then happily `refresh` it.

Two boundaries on that, both tested. The floor is only enforced for a RELEASE build, as
everywhere else — a development build is exempt. And configuration that cannot be LOADED
still leaves the implicit instance and proceeds, because tearing down a project whose
files are gone is half of what `destroy` is for (§6.1); only configuration that loads and
then states a floor this build cannot meet is refused.

## 61.3 There is no separate plugin SDK version

`pkg/pluginsdk` is Go code a plugin author compiles against, and `pkg/pluginproto` is
the wire. **The protocol version is the compatibility contract, not the Go types**
(§31.1), so:

- a plugin built against an older SDK keeps working for as long as its protocol version
  is in `Supported`, and never needs rebuilding for an infrena release;
- the SDK's Go API rides the module's own semver, which is what a plugin's `go.mod`
  pins, and which therefore follows §61.1's rules like any other package.

Two numbers, already present, doing different jobs. A third — an "SDK version" — would
have to agree with one of them, and would eventually not.

## 61.4 `infrena version`

Nothing currently tells a user, or a bug report, which formats a binary speaks:

```text
$ infrena version
infrena 0.4.1 (a1b2c3d, go1.24.13, linux/amd64)

formats
  state             1
  plugin protocol   1
  plan artifact     1
  report            2
```

`--output json` emits the same thing as one object, so a CI job can assert on it. This
is the artifact that makes "which version do I need" answerable without reading source,
and it is why the format versions are exported rather than package-private.

---

# 60. Open Source and Commercial Model

Infrena will be sold as a commercial product built around an open-source core.

**The core stays open source permanently.** This is a commitment to users, not a phase: the core is never relicensed, and no feature is ever moved out of the core into the paid product.

## The open core

The open core permanently contains:

* The `infrena` CLI and every command in §37.
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

A hosted run interface alone is a crowded market. These features use data only Infrena's engine has, and are where the commercial product competes:

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
* Policy checks at advisory, soft-mandatory (overridable with a reason) and hard levels: simple declarative rules plus OPA integration. Infrena still does not grow a complex policy language of its own (§54).
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

1. **The CLI's machine-readable output is a product API.** The commercial runner invokes the `infrena` binary, as Terraform Cloud agents invoke `terraform`, rather than importing the engine; the engine's packages live under `internal/` and cannot be imported from another module anyway. The JSON plan artifact, a JSON event stream and exit codes are held to the same compatibility standard as the configuration language (§58).
2. **Redaction stays in the engine.** Executor events carry only pre-redacted text, so no integration ever receives a raw attribute (§36).
3. **The actor is recorded.** Plans and state record who made a change, so an audit trail never has to be retrofitted.
4. **Plan and apply keep a seam between them** where policy checks and approvals attach.
5. **No commercial code paths in the core.** No license checks and no feature gates.
6. **The rename completes before the first public release**, so no user's import path ever breaks.

## Making "open source forever" credible

HashiCorp relicensed Terraform in 2023 after years as open source, and the community forked it as OpenTofu. A promise alone will not be believed, so it is made structural:

* **Publish an open-core policy** stating the dividing line and that features never move from free to paid, before the commercial product exists.
* **Accept contributions under a DCO sign-off, not a CLA.** A permissive license already lets contributed code ship inside the commercial product. A CLA would only add the right to relicense, which is exactly what is promised never to happen.
* **Trademark "Infrena".** The code is open; the name is controlled. A trademark policy lets forks use the code but not the name.

## Accepted risk

A permissive core means competitors such as Spacelift, env0 and Scalr can build the same commercial features on the same engine. The advantage has to come from being the maintainers: velocity, trust, the brand, and a better hosted product.

## Open decisions

* **License.** Apache 2.0 is recommended: it carries a patent grant and enterprise legal teams approve it without review. MPL 2.0 is acceptable. AGPL is ruled out, since enterprises commonly ban it and the commercial product depends on adoption.
* **Copyright holder**, an individual or a company. Settle before anything is sold.
* **Contribution sign-off.** DCO is recommended, above.
* **Module path.** `github.com/infrena/infrena`, or a vanity path `infrena.dev/infrena`, which keeps import paths stable if hosting ever moves off GitHub.
* **Trademark registration.**
* **Pricing model.** Avoid pricing per resource under management, which was widely unpopular for HCP Terraform.
