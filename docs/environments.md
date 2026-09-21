# Variables and environments

Dev, staging and production hold the same infrastructure and differ in what their values say.
Infrena makes that the structure rather than a convention: environments are declared in the
project, each has its own state and its own lock, and the difference between them lives in
variables rather than in copied directories.

## Declaring environments

```yaml
environments:
  dev: {}
  staging: {}
  production: {}
```

Every command that touches infrastructure names one:

```bash
infrena plan production
infrena apply production
```

Two applies against different environments do not contend, because the lock is per
environment.

Removing an environment from `infrena.yml` does not silently orphan what it held. Infrena
proposes the teardown and shows exactly what it would destroy before you agree, so plan that
environment before you delete the line.

## Declaring variables

Declarations are typed, and live in `infrena.yml`:

```yaml
variables:
  db_size:
    type: integer
    default: 10
    min: 5
    max: 100
  db_password:
    type: string
  tags:
    type: map
    default: {}
```

Six types: `string`, `integer`, `float`, `boolean`, `list`, `map`. `min` and `max` apply to
the numeric ones.

A variable with no `default` must be supplied somewhere, and a run that does not supply it
fails at plan time naming the variable, rather than passing an empty value to a provider.

Read them as `${var.NAME}`:

```yaml
resources:
  db:
    type: aws.rds
    size: ${var.db_size}
```

The `var.` prefix is not optional and there is no bare form. That is what makes the rule
total: no bare name anywhere resolves to a variable, so nothing depends on whether a resource
happens to share a name.

### Two variables you did not declare

`${var.environment}` and `${var.project}` are always available — the environment this run
names, and the project's own name. They carry the prefix like everything else, because a
prefix that applies to some variables and not others is an exception nobody remembers.

```yaml
    name: ${var.project}-${var.environment}-db
```

## Giving them values

Values live in `vars/`, one file per environment:

```
vars/
├── default.yml       # applies everywhere
├── staging.yml       # overrides default, value by value
└── production.yml
```

```yaml
# vars/default.yml
db_size: 10
db_password: dev-secret
```

```yaml
# vars/production.yml
db_size: 100
```

An environment-named file overrides `default.yml` **value by value**, not file by file.
Production above gets `db_size: 100` and still gets `db_password` from `default.yml`. You
never restate what has not changed, which is what keeps the files honest: everything written
in `production.yml` is something that genuinely differs.

A variables file is a plain mapping of name to value. Typed declarations belong in
`infrena.yml`; the two are different documents and are decoded differently.

### Directory-scoped variables

A directory under `resources/` may have its own `vars/`, visible only to the resources
declared there:

```
resources/
└── app/
    ├── app.yml
    └── vars/
        └── sizes.yml      # `size` means something only inside resources/app/
```

Nothing outside that directory can see those names. This is how a large project keeps a
generic name like `size` from becoming `app_instance_size_for_the_web_tier`.

A directory scopes **variables and templates, never names**. Moving a file between
directories renames no resource.

## Precedence

Lowest to highest:

1. Provider defaults
2. Base configuration
3. Module defaults
4. Environment inheritance
5. Environment variables (`vars/<environment>.yml`)
6. `--var-file` on the command line
7. `--var` on the command line

Explicit configuration always beats an implicit default, and the command line always wins.

```bash
infrena plan production --var db_size=200
infrena plan production --var-file /tmp/incident-overrides.yml
```

`--var-file` is repeatable and later files win. Both are for the exceptional run — an
incident, a one-off test — and neither is a substitute for a committed file, because nothing
records that you passed them.

Values carry their origin all the way through, so the plan can tell you where a value came
from:

```
      size: 10 [default, from provider default]
```

That line is the answer to "why is this 10 when I set it to 50 somewhere" — it says which
layer won.

## Inheritance between environments

An environment may extend another:

```yaml
environments:
  production: {}
  production_eu:
    extends: production
```

The extending environment starts from the other's values and overrides what it names. It is
for the case where two environments are genuinely the same thing in two places, not for
saving a few lines.

## Keeping a resource out of an environment

```yaml
  bastion:
    type: aws.instance
    only: [dev, staging]

  replica:
    type: aws.rds
    skip: [dev]
```

A resource excluded from an environment is not planned there and not destroyed there. It is
simply not part of it.

Referring to an excluded resource from one that is present is an error, named at the line
that excluded it:

```
Error: ${net.id} reads "net", which is skipped in environment "dev"
  at infrena.yml:9:5

  "net" is excluded from this environment by the `skip`/`only` at infrena.yml:9:5,
  so the value this needs will never exist here.

  Suggested action:
    Skip this resource in the same environments, or widen the filter on "net".
```

## Secrets are not variables

A password does not belong in `vars/production.yml`, because that file is committed. Use
`${secret.NAME}`, which reads the environment or an encrypted vault — see
[secrets.md](secrets.md).
