# Modules

A module is a group of resources that always go together, with a declared set of inputs and
outputs. A database plus its subnet group plus its parameter group; a service plus its task
definition plus its log group. You write it once and use it in three environments, or five
times in one.

## Writing one

A module is a directory containing `module.yml`:

```
modules/
└── app-stack/
    └── module.yml
```

```yaml
inputs:
  network:
    type: string
  size:
    type: integer
    default: 10
  password:
    type: string
  tags:
    type: map
    default: {}

resources:
  db:
    type: fake.database
    engine: postgres
    network: ${var.network}
    size: ${var.size}
    password: ${var.password}
    tags: ${var.tags}

outputs:
  endpoint:
    value: ${db.endpoint}
```

Inside the module, an input is read as `${var.NAME}`, exactly like a variable. An input with
no `default` is required, and omitting it is an error at the call site.

The file is called `module.yml` and not `infrena.yml` on purpose. The two are different
documents — a module has `inputs` and `outputs` and no `project` — and separate names make
them distinguishable by construction rather than by four rejection rules. It also stops a
module directory from looking like a project: under a shared name, running `infrena plan dev`
inside `modules/app-stack/` would find a valid file and *try*, producing a pile of errors
describing a situation that is not a mistake.

## Using one

Modules in `modules/` are found automatically. A module from elsewhere is listed:

```yaml
modules:
  - ./modules/app-stack
  - https://github.com/myorg/infra-networking:v1.2.0
```

Then it is used like any other resource type, with `module.` as the prefix:

```yaml
resources:
  stack:
    type: module.app_stack
    network: ${network.id}
    size: ${var.size}
    password: ${var.db_password}
    tags:
      environment: ${var.environment}
      project: ${var.project}
      team: storefront

  web:
    type: fake.application
    image: nginx:1.27
    database_url: ${stack.endpoint}
```

A directory name becomes an identifier by turning `-` into `_`, so `modules/app-stack/`
is `module.app_stack`.

Outputs are read off the call the same way attributes are read off a resource:
`${stack.endpoint}`. The dependency that creates is real — `web` waits for the module's
database.

### Passing a map across the boundary

A map input crosses as one value, and its entries may interpolate:

```yaml
    tags:
      environment: ${var.environment}
      project: ${var.project}
      team: storefront
```

This cannot live in a variables file. A variable is resolved before any expression scope
exists, so `${...}` there would have nothing to refer to. Building it at the call site is
also what lets the module stay ignorant of environments: the caller knows which environment
it is in, and the module just receives tags.

## Addresses

A resource inside a module is addressed through its call:

```
module.stack.db
```

Output that lists resources prefixes the type, so `plan` and `state list` show
`fake.database.module.stack.db`. Commands take the address:

```bash
infrena state show dev module.stack.db
```

`module` is reserved as a resource name for exactly this reason.

### Moving a resource between modules destroys it

An address embeds the module path, so moving a resource into a module, out of one, or
between two changes its address. Infrena matches state to configuration by address, so
it sees the old address gone and a new one arrived: **the plan is a destroy and a
create, not a move.** For a database or a bucket that is data loss, and `apply` will do
it without complaining, because as far as it can tell that is what you asked for.

There is no `state mv` yet. Until there is, the safe sequence is:

```bash
infrena plan <environment>          # read it: a destroy paired with a create is the signal
infrena state rm <environment> <old address>
# move the resource in configuration, then adopt it back at its new address
infrena import <environment> <type>.<provider id> --as <new name>
```

`state rm` stops infrena managing a resource without destroying it, and `import` adopts
it again, so the resource itself is never touched. `plan` names this case when it can:
a destroy whose type and name match something the same plan creates is flagged as a
possible move.

## for_each and modules

`for_each` works on a module **call**, and on a resource **inside** a module. They key
different things.

### On a call

```yaml
resources:
  store:
    type: module.app_stack
    for_each: {orders: postgres, billing: mysql}
    network: ${network.id}
    engine: ${each.value}
```

The whole module is instantiated once per entry, and every resource inside lands under the
keyed call:

```
module.store["orders"].db
module.store["orders"].subnet_group
module.store["billing"].db
module.store["billing"].subnet_group
```

The call's inputs are evaluated **once per entry**, which is what makes `${each.key}` and
`${each.value}` useful here: without that it would be a loop producing N copies of one thing.

Read one instance's output by naming it:

```yaml
    database_url: ${store["orders"].endpoint}
```

Referring to the whole call is an error that names the instances, because a keyed call has no
single set of outputs. The dependency lands on **that instance**, not on everything the call
produced, so instances that have nothing to do with each other are not serialised behind one
another.

Removing one entry destroys that instance's resources and leaves the others alone.

### Inside a module

```yaml
# modules/app-stack/module.yml
resources:
  store:
    type: fake.database
    for_each: ${var.engines}
    engine: ${each.value}
```

```
module.primary.store["postgres"]
module.primary.store["mysql"]
```

The two compose: a keyed resource inside a keyed call is
`module.store["orders"].db["postgres"]`.

See [configuration.md](configuration.md#for_each) for the rules that apply to both.

## Remote modules

A source may be a git repository, written as the repository followed by `:` and a ref:

```yaml
modules:
  - https://github.com/acme/infra-app-stack:v1.2.0     # a tag
  - git@github.com:acme/infra-database:9f3c1ab         # a commit
  - ssh://git@git.example.com:2222/acme/repo:v1.0.0    # ssh, with a port
```

The ref may contain slashes (`:release/1.0`), and a `.git` suffix is fine. A ref is
**required**: an unpinned repository means the module you reviewed and the module you applied
are not necessarily the same.

Resolved sources are recorded in `modules.lock`, which belongs in version control. A module
that resolves to different bytes on somebody else's machine is a module you cannot review.
A commit pin does not need the network on a later run; a tag pin does, because a tag can
move and the lockfile is what notices.

**Never put a password or token in a source.** It would be recorded in `modules.lock` and
printed by every diagnostic that names the module. Use your ssh agent or a credential helper.

## What a module is not

A module is a grouping, not an abstraction layer. It has no way to reach outside itself, no
access to the caller's other resources, and no opinion about environments. Everything it
knows, it was passed.

That is deliberate. The failure mode of module systems is a module that quietly depends on
something at the call site, so it works in the project it was written in and nowhere else.
Inputs and outputs being the entire interface is what makes a module movable.

## A project built only of modules

Nothing special is required. Which plugins to load is worked out from the resource types
configuration declares, and `module.<name>` is not one of them — so for a while a provider
used *only* inside a module was never loaded, and the plan failed naming a type inside the
module, which read as the module being wrong.

Module types are now collected after expansion, so this works:

```yaml
project: myapp

resources:
  primary:
    type: module.db
    cidr: 10.0.0.0/16
```

A `providers:` block is still the way to say anything *about* a provider — a region, an
account, two instances of one plugin — and naming a plugin there has always been enough. It
is simply no longer required to make a module-only project load.
