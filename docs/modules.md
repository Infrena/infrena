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

The file is called `module.yml` and not `infra.yml` on purpose. The two are different
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

## for_each and modules

`for_each` works on a resource **inside** a module, and the key lands on that resource:

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

**It does not work on the module call itself.** Writing it there is refused:

```
Error: `for_each` is not supported on a module call

  It works on a resource, but a module call cannot yet expand into several
  instances — and written here it would have made exactly one, silently.

  Suggested action:
    Declare the call once per entry, or move the `for_each` onto a resource
    inside "app_stack".
```

Until it exists, declare the call once per entry. Expanding a call means every resource
inside needs a keyed address and every output needs keying to match, which is more than a
loop.

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

## A known limitation: declare `providers:` if everything is in modules

Which plugins to load is worked out from the resource types the **root** configuration
declares, and `module.<name>` is not one of them. Module files are not read until later, so a
provider used *only* inside a module is never discovered, and the plan fails naming a type
inside the module:

```
Error: unknown resource type "fake.network"
  at modules/db/module.yml:5:3, in module.primary

  Known types:

  Suggested action:
    Correct the type, or check that the provider offering it is available.
```

The empty "Known types" list is the tell: no provider was loaded at all.

**The fix is to name the provider explicitly**, which is what `providers:` is for:

```yaml
project: myapp

providers:
  - plugin: fake

resources:
  primary:
    type: module.db
    cidr: 10.0.0.0/16
```

`providers:` is optional precisely *because* a resource type usually implies its plugin. When
every resource is inside a module there is nothing at the root to imply it, so say it. Most
projects reach for `providers:` anyway to set a region or an account.

This is recorded as a bug: the diagnostic should say the plugin was never loaded rather than
blame the type.
