# `shop` — a worked example

Everything Phase 1 does, in one small project: environments, a typed variable, a module with
inputs and an output, and values flowing in and out across the module boundary.

Run it from this directory. Nothing here touches a network — the only provider is the fake
one, which keeps its "cloud" in a hand-editable JSON file.

```bash
infrata validate
infrata plan dev
infrata apply dev --auto-approve
infrata plan dev            # nothing to do
```

## What to look at in the plan

```
+ test.database.module.stack.db
    password: <sensitive> [variable, from base config]
    size: 50
+ test.application.web
    replicas: 1 [default, from provider default]
```

- The address `module.stack.db` embeds the module path. It is part of the resource's identity,
  so moving a resource between modules is a destroy plus a create, not a move.
- `<sensitive>` is redacted because the provider's schema marks `password` sensitive — not
  because the configuration asked. There is exactly one place in the codebase that renders it.
- `[variable, from base config]` and `[default, from provider default]` are provenance: every
  value remembers where it came from, which is what `plan` prints and what `explain` explains.
- `size: 50` is the caller overriding the module's own `default: 10`.

## Try breaking it

**Drift.** Edit `.infra/fake-cloud.json` after an apply — change the database's `size` to 999 —
then:

```bash
infrata refresh dev
infrata plan dev            # size: 999 -> 50
```

**Removal.** Delete the `web:` block from `infra.yml` and re-plan: a destroy is proposed. The
configuration is the desired state; anything in state and absent from it is scheduled to go.

**A production apply.** `infrata plan production` uses the environment's own `db_password`, and
`test.database`'s size default is 100 there rather than 10 — `infrata explain test.database`
says so.

## What the module boundary refuses

`modules/app-stack/module.yml` declares `password` as an input, and `infra.yml` passes it.
That is not ceremony: a module cannot see a project variable. Try deleting the input
declaration and referencing `${db_password}` inside the module instead —

```
Error: undefined variable "db_password"
  at modules/app-stack/module.yml:15:5, in module.stack
```

A module depends only on what it declares, so the same module works in another project.

## Other commands

```bash
infrata graph dev               # the dependency tree, module structure included
infrata explain test.database   # the resource type, read out of the schema itself
infrata state list dev
infrata destroy dev --auto-approve
```
