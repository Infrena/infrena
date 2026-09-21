# Configuration reference

Everything infrena reads is YAML. This is the shape of it.

- [The file](#the-file)
- [Resources](#resources)
- [References](#references)
- [Functions](#functions)
- [`for_each`](#for_each)
- [`lifecycle`](#lifecycle)
- [`depends_on`](#depends_on)
- [`skip` and `only`](#skip-and-only)

Variables and environments have their own page: [environments.md](environments.md).
So do [modules](modules.md), [secrets](secrets.md) and [templates](templates.md).

## The file

A project is a directory containing `infrena.yml`. Commands run from that directory, or from
its parent when the project lives in `./infrena/` beside an application.

```yaml
project: myapp                 # required, the project's name

infrena: ">= 0.11"             # the oldest engine that understands this project

plugins:                       # version constraints, one per plugin
  aws: ">= 0.3.0, < 0.4.0"

providers:                     # a LIST: each entry names a plugin, plus its config
  - plugin: aws
    region: eu-west-1

backend:                       # where state lives; omit for local state
  plugin: s3
  bucket: myapp-state

variables:                     # see environments.md
  instance_size:
    type: integer
    default: 2

environments:                  # see environments.md
  dev: {}
  production: {}

modules:                       # a LIST of sources; see modules.md
  - ./modules/app-stack

resources:
  net:
    type: aws.vpc
    cidr_block: 10.0.0.0/16
```

`providers:` is a list rather than a map because a map cannot hold two instances of one
plugin and would drop one silently. Each entry takes an optional `name:`, defaulting to the
plugin's own name, which is how a project talks to two AWS regions at once.

`infrena:` is worth keeping even though it is optional: without it, an engine that is too old
reports unknown keys one at a time instead of saying it is too old.

`migrate_from:` is the tenth key, used once when moving state between backends; see
[state-backends.md](state-backends.md).

An unrecognised top-level key is a warning that lists the ten, rather than an error — a
newer project read by an older binary should still be usable.

### Splitting the file up

Nothing has to be listed. Conventional directory names are found on their own:

```
myapp/
├── infrena.yml
├── resources/         # your infrastructure, organised however suits you
│   └── networking/
│       ├── vpc.yml
│       ├── vars/      # variables scoped to this directory
│       └── templates/ # templates scoped to this directory
├── vars/              # values, one file per environment
├── environments/      # one file per environment, its overrides
├── modules/           # reusable components
├── templates/         # project-wide templates
├── secrets/           # per-environment vaults
└── discovered/        # written by import, reviewed by you
```

`resources/` and `vars/` are globbed **recursively**, so organise them however suits you.
`secrets/` is read at the project root only.

A directory scopes **variables and templates**, never names. Moving a file between
directories renames nothing, so reorganising is safe. Declaring the same name twice is an
error pointing at both files rather than a silent winner.

`discovered/` is loaded like any other configuration rather than staged. A resource in state
that no configuration declares is scheduled for destruction, so a staging area would mean
`import` followed by `apply` destroys what was just adopted.

The single-file and directory forms produce byte-identical plans. That is pinned by a test,
so growing a project is a layout change and never a behaviour change.

## Resources

```yaml
resources:
  db:
    type: aws.rds            # required: <plugin>.<type>
    engine: postgres         # everything else is the provider's schema
    size: ${var.db_size}
    vpc_id: ${net.id}
```

The name (`db`) is yours and becomes the address. The `type` prefix names the plugin that
serves it, so `aws.rds` needs the `aws` plugin; that is how plugin discovery knows what to
load.

Attribute names, types and requirements come from the provider's schema, so a typo, a missing
required attribute, or a value of the wrong type is an error before anything is contacted:

```
Error: fake.network has no attribute "nme"
  at infrena.yml:14:5

  ${net.nme} reads an attribute that does not exist.
  Attributes of fake.network:
    cidr
    id

  Suggested action:
    Correct the attribute name.
```

`infrena explain aws.rds` prints the schema for a type without you having to find its docs.

Two names are reserved and cannot be used for a resource: **`var`**, because it would make
`${var.x}` ambiguous, and **`module`**, because it is how an address names a module level.
Both are refused at the declaration, where the fix is, rather than at the reference.

## References

`${ }` marks an expression. Six forms, each with exactly one meaning:

```yaml
${var.region}          a variable
${var.tags.team}       a path into a map variable
${var.azs[0]}          an entry of a list variable
${vpc.id}              an attribute of resource `vpc`
${vpc.tags.Name}       a path into a resource attribute
${vpc}                 the resource `vpc` itself
```

The first segment is the resource, the second is the attribute, and anything after that is a
path into it. There is no rule that depends on counting segments, and a resource name can
never contain a dot, so this stays unambiguous however deep the path goes.

`${vpc}` — a whole resource with no attribute — resolves to whichever attribute the consuming
field declared it refers to. If a provider says `vpc_id` holds a VPC's id, then
`vpc_id: ${vpc}` means `${vpc.id}` and you did not have to look it up. If the consuming
attribute declares no such reference, the error says so rather than pretending the resource
does not exist.

Interpolation composes inside a larger value:

```yaml
    name: ${var.project}-${var.environment}-db
    url: postgres://app:${secret.DB_PASSWORD}@${db.endpoint}/app
```

Four further namespaces exist:

| Namespace | Means | Page |
| --- | --- | --- |
| `${secret.NAME}` | a credential, from the environment or a vault | [secrets.md](secrets.md) |
| `${template.NAME}` | a file, interpolated | [templates.md](templates.md) |
| `${file.NAME}` | a file, verbatim | [templates.md](templates.md) |
| `${each.key}`, `${each.value}` | the current `for_each` entry | [below](#for_each) |

### Referencing creates ordering

A reference is a dependency, not a string that happens to mention something. `vpc_id:
${net.id}` means the network is created first and the real id is substituted; on teardown the
order reverses. Nothing needs declaring for this to happen — see `depends_on` below for the
case where it does.

Pointing a field at the wrong *kind* of resource is a compile error too. If `vpc_id` holds a
VPC's id, `vpc_id: ${database.id}` fails at `validate`, not after four other resources already
exist.

## Functions

Seven, and no more:

| Function | Does |
| --- | --- |
| `lower(s)` / `upper(s)` | case |
| `trim(s)` | strip surrounding whitespace |
| `replace(s, old, new)` | substitute |
| `join(sep, list)` | join with a separator |
| `default(value, fallback)` | `fallback` when `value` is unset |
| `merge(a, b, …)` | combine maps, later keys winning |

Every one is pure and total. `now()`, `uuid()` and reading a file are the three most often
asked for next, and each would break plan determinism: the same configuration and state would
plan differently on a second run, which is the property the whole plan/apply split rests on.
Adding an eighth is a language change, not a patch.

## for_each

To make several of something, give it a list or a map:

```yaml
resources:
  subnet:
    type: aws.subnet
    for_each: ${var.availability_zones}     # [eu-west-1a, eu-west-1b, eu-west-1c]
    vpc_id: ${net.id}
    availability_zone: ${each.value}
```

Each instance gets an address keyed by its entry:

```
subnet["eu-west-1a"]
subnet["eu-west-1b"]
subnet["eu-west-1c"]
```

With a **list**, `each.key` and `each.value` are both the entry. With a **map**, they differ:

```yaml
  db:
    type: aws.rds
    for_each: {orders: postgres, billing: mysql}
    engine: ${each.value}
    tags:
      service: ${each.key}
```

### Identity is the key, never the position

This is the reason `for_each` exists here and `count` does not. Remove `eu-west-1b` from the
middle of that list and exactly one subnet is destroyed. Nothing else in the plan moves.

With ordinal addressing, removing the middle of three shifts every later one — the third
becomes the second — and the plan proposes destroying and recreating resources that did not
change. For anything holding data, that is not a renumbering, it is a data loss. Keying by
entry makes it structurally impossible rather than merely discouraged.

If you write `count:` and the provider has no such attribute, the error suggests `for_each`.

### Referring to one instance

```yaml
    subnet_id: ${subnet["eu-west-1a"]}
```

Referring to the **whole set** is an error, because one resource cannot consume three ids.
The message names the instances rather than calling the resource undeclared, which would send
you looking for a typo in a name that is right there in the file.

### Other rules

- **Keys must be known at plan time**, values need not be. A key that depends on something not
  yet created would mean not knowing how many resources a plan contains.
- **A duplicate key is refused**, not silently collapsed. Two declarations quietly becoming
  one instance is how a resource goes missing.
- **An empty list or map makes nothing**, with no error. This is the optional-resource case,
  and it is why there is no `count: enabled ? 1 : 0` idiom to learn.
- **Map keys are ordered** before expansion, so plans are deterministic.

`for_each` works on a module call, and on a resource inside a module; see
[modules.md](modules.md#for_each-and-modules).

## lifecycle

Five options, all per resource:

```yaml
resources:
  db:
    type: aws.rds
    lifecycle:
      prevent_destroy: true
      prevent_replace: true
      create_before_destroy: true
      retain: true
      ignore_changes: [tags.LastModified]
```

**`prevent_destroy`** refuses a plan that would destroy this resource — the case where
somebody deleted the block, or pointed at the wrong environment.

**`prevent_replace`** refuses a plan that would *replace* it: destroy and recreate, because an
attribute the provider marks as forcing a new resource changed underneath it. This is the more
insidious of the two. The configuration still names the resource, the diff reads as an edit,
and the data is gone all the same. Neither option implies the other, because they guard
different mistakes. The diagnostic names the attributes that forced the replacement, since
without them a reader is left working out why an ordinary-looking edit became a destroy.

**`retain`** removes the resource from state without deleting it, when the configuration stops
managing it. For the thing you want to keep after you stop managing it.

**`ignore_changes`** lists attributes whose drift should not produce a diff — the tag another
system writes, the field a console edit is allowed to own.

Refusals happen at plan time, so they arrive before any approval rather than part-way through
an apply.

**`create_before_destroy`** reverses the two halves of a replacement: the new object is
built, everything pointing at it is moved across, and only then is the old one destroyed.
Use it for anything that must not be *absent* in between — a load balancer, an instance
serving traffic.

It is opt-in per resource and cannot be the default, because a great many resources cannot
exist twice: a unique name, a fixed port, a key that is the identity. For those, reversing
the order turns a clean replacement into a create that collides. You are the one who knows
which kind you have.

Two things worth knowing:

- **Dependents follow automatically.** A resource referring to the replaced one is updated in
  place to point at the new object, and that update finishes *before* the old one is
  destroyed. Setting the flag on one resource does not silently change how any other resource
  is replaced.
- **If the old object cannot be deleted**, the run reports it and state keeps a record of it.
  Every later plan proposes the cleanup until it succeeds:

  ```
  Plan: 0 to create, 0 to update, 0 to replace, 0 to destroy, 0 to forget. 1 left over from an interrupted replacement to clean up.
  ```

  That is deliberate. A real object nothing can name is a leak that bills monthly; one state
  still names is a line in the next plan.

## depends_on

References create ordering on their own, so this is only for the dependency the
configuration cannot see — an IAM policy that must exist before something using the role
works, with no attribute connecting them:

```yaml
  app:
    type: aws.instance
    depends_on: [policy_attachment]
```

If you find yourself reaching for it often, check whether a reference would express the same
thing. A reference is checked; `depends_on` is taken on trust.

## skip and only

Per resource, to keep something out of some environments:

```yaml
  bastion:
    type: aws.instance
    only: [dev, staging]        # exists nowhere else

  replica:
    type: aws.rds
    skip: [dev]                 # exists everywhere but dev
```

A resource excluded from an environment is not planned there and not destroyed there — it
simply is not part of that environment. A reference from a resource that *is* present to one
that is not is an error naming both, rather than a null substituted at apply.
