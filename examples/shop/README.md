# `shop` — a worked example

Everything Phase 1 does, in one small project: environments, a typed variable, a module with
inputs and an output, values flowing in and out across the module boundary, and the
conventional directory layout that decides which files get read.

Run it from this directory. Nothing here touches a network — the only provider is the fake
one, which keeps its "cloud" in a hand-editable JSON file.

This example declares no `providers:` block, so it gets one implicit instance. A project that
needs two accounts of one cloud declares a list of instances and picks one per resource with
`provider:` — see `PLAN.md` §12.1. That is deliberately not shown here: a second account is a
different lesson, needing its own cloud file and its own explanation, and this walkthrough is
about the layout, variables and modules you meet first.

```bash
infrata validate
infrata plan dev
infrata apply dev --auto-approve
infrata plan dev            # nothing to do
```

## The layout

```
infra.yml                     the project, its variable declarations, environments, modules
vars/default.yml              variable values for every environment
vars/production.yml           what production does differently
resources/network/            a resource directory
resources/app/                another one
resources/app/vars/sizes.yml  variables only resources/app/ can see
modules/app-stack/            a module
```

Four directory names are read automatically — `resources/`, `vars/`, `modules/` and
`discovered/` — at any depth. Nothing lists them; putting a file in one is how you load it.

**Splitting a project across directories is organisation, not meaning.** The same content in a
single `infra.yml` plans identically, so move things around freely. Two rules are worth knowing
before you do:

- **A directory scopes variables, never names.** `resources/app/app.yml` writes
  `${network.id}`, naming a resource declared in `resources/network/`. A resource's address is
  its name — the directory it was declared in is not part of it, so moving a file between
  directories renames nothing and destroys nothing.
- **A name declared twice is an error naming both files.** Not last-one-wins. Globbing makes
  accidental duplication easy in a way a single file does not, and one definition silently
  winning is how you deploy something you did not write.

`vars/default.yml` applies to every environment; a file named after an environment overrides it
**value by value**, not file by file. `vars/production.yml` sets only `db_password`, so
production still gets everything else from `default.yml`.

## What to look at in the plan

```
+ test.database.module.stack.db
    password: <sensitive> [variable, from base config]
    size: 50 [variable, from directory vars]
+ test.application.web
    replicas: 1 [default, from provider default]
```

- The address `module.stack.db` embeds the module path. It is part of the resource's identity,
  so moving a resource between modules is a destroy plus a create, not a move.
- `<sensitive>` is redacted because the provider's schema marks `password` sensitive — not
  because the configuration asked. There is exactly one place in the codebase that renders it.
- `[variable, from base config]`, `[variable, from directory vars]` and
  `[default, from provider default]` are provenance: every value remembers where it came from,
  which is what `plan` prints and what `explain` explains. The middle one is how you know to
  look in `resources/app/vars/` rather than `vars/`.
- `size: 50` is `resources/app/vars/sizes.yml` overriding the module's own `default: 10`.
- `tags:` is a map whose values interpolate, crossing the module boundary as one input.
  `${environment}` and `${project}` come from the invocation, so `plan production` shows
  different values with no second copy of the map anywhere. It cannot live in a variables file:
  a variable is resolved before any expression scope exists, so `${...}` there has nothing to
  refer to. To combine it with another map, `${merge(a, {team: storefront})}` — quoted, because
  YAML ends a plain scalar at `: `.

Precedence runs: provider default → `vars/**` and `variables.yml` → `resources/<dir>/vars/**` →
module defaults → environment → `--var`. More specific file scope wins, an environment beats
every file, and `--var` beats everything.

## Try breaking it

**Drift.** Edit `.infra/fake-cloud.json` after an apply — change the database's `size` to 999 —
then:

```bash
infrata refresh dev
infrata plan dev            # size: 999 -> 50
```

**Removal.** Delete the `web:` block from `resources/app/app.yml` and re-plan: a destroy is
proposed. The configuration is the desired state; anything in state and absent from it is
scheduled to go.

**Scope.** Move `resources/app/vars/sizes.yml` to `resources/network/vars/sizes.yml` and
re-plan. `size` is no longer visible where it is used:

```
Error: undefined variable "size"
  at resources/app/app.yml:7:5
```

**A production apply.** `infrata plan production` takes `db_password` from
`vars/production.yml` and leaves everything else exactly as `dev` has it. That is the point:
environments hold the same infrastructure and differ only in what their variables say. If you
want production to differ in some other way, set that value in `vars/production.yml` too —
there is no second, hidden mechanism that changes behaviour because an environment is called
"production".

## What the module boundary refuses

`modules/app-stack/module.yml` declares `password` as an input, and `resources/app/app.yml`
passes it. That is not ceremony: a module cannot see a project variable, or a directory's
variables either. Try routing around the input — delete the `password:` declaration from the
module, delete the `password:` line from the call, and reference `${db_password}` directly
inside the module:

```
Error: undefined variable "db_password"
  at modules/app-stack/module.yml:16:5, in module.stack
```

(Leave the `password:` line on the call and you get the other half of the boundary first:
`module "app_stack" has no input "password"`. A module refuses what it does not declare in
both directions.)

A module depends only on what it declares, so the same module works in another project.

## Other commands

```bash
infrata graph dev               # the dependency tree, module structure included
infrata explain test.database   # the resource type, read out of the schema itself
infrata state list dev
infrata destroy dev --auto-approve
```
