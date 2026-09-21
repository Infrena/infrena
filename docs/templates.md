# Templates and files

Some values are documents rather than strings. An IAM policy, a user-data script, a
container definition: too long to read inline in `infrena.yml`, and often something you want
syntax highlighting and a linter for. Templates let those live in their own files.

```yaml
resources:
  app:
    type: aws.iam_role
    assume_role_policy: ${template.trust.json}
    user_data: ${file.bootstrap.sh}
```

Two namespaces, one difference:

- **`${template.NAME}`** — the file is interpolated. `${...}` inside it is evaluated exactly
  as it would be in `infrena.yml`.
- **`${file.NAME}`** — the file is read verbatim. Nothing in it is interpolated.

`${file.}` exists because shell scripts contain `${...}` of their own. Interpolating a
bootstrap script would silently eat `${HOME}` and substitute nothing, producing a script that
runs and does the wrong thing. If a file is not meant to be a template, say so by how you
reference it.

## Where templates live

```
myapp/
├── infrena.yml
├── templates/                 # shared by everything
│   └── trust.json
└── resources/
    └── network/
        ├── vpc.yml
        └── templates/         # belongs to resources/network/, and wins
            └── trust.json
```

Nearest wins. A directory's own `templates/` beats the project's for a name both define,
which is the same direction every other scoped thing in the project runs — a directory's
`vars/` beats the project's variables in exactly the same way.

The name after the namespace is taken whole, because filenames contain dots:
`${template.policy.v2.json}` names one file called `policy.v2.json`. A name that would escape
the templates directory is refused rather than followed.

A project that never uses templates has neither directory and never notices.

When a template is missing, the error says where it looked:

```
Error: no template named "trust.json"

  Templates are read from templates/ beside the project, and from
  resources/<directory>/templates/ for a resource in that directory, which wins.

  Suggested action:
    Create templates/trust.json, or correct the name.
```

## References inside a template

A template is interpolated in the same grammar as configuration, so everything works the way
it does in `infrena.yml` — including resource references and their dependency edges:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": "s3:GetObject",
    "Resource": "${bucket.arn}/*"
  }]
}
```

The role that uses this template now depends on the bucket. It is not a string that happens
to mention one: the planner knows the ordering and waits for the real ARN.

A template that references itself, directly or through another template, is refused rather
than expanded until something runs out.

## Arguments

A template can take one argument, a map, and its contents are rendered by Go's
`text/template` **before** the `${ }` pass:

```yaml
variables:
  policy_args:
    type: map
    default:
      team: platform
      buckets: [uploads, thumbnails]

resources:
  app:
    type: aws.iam_role
    policy: ${template.access.json(var.policy_args)}
```

```json
{
  "Team": "{{ .team }}",
  "Statement": [
    {{ range $i, $b := .buckets }}{{ if $i }},{{ end }}
    {"Resource": "arn:aws:s3:::{{ $b }}", "Vpc": "${net.id}"}
    {{ end }}
  ]
}
```

**Two passes, in this order, and the order is the whole design:**

> `{{ }}` sees what you passed it. `${ }` sees the project.

That split is what makes the combination safe rather than merely powerful. A template engine
handed the project's whole scope could write a secret into the rendered text as a *literal* —
and the second pass would then see an ordinary string with no sensitivity, which reaches the
plan, the state and the report in clear. Pass one can reach nothing it was not handed, and
whatever it was handed has already been evaluated, so we still know what was sensitive.

It is also why `${net.id}` above survives. Pass one leaves `${ }` alone, so the reference
reaches pass two intact, with its deferral and its dependency edge.

Three consequences worth knowing:

- **A sensitive argument taints the whole rendered document.** The template decides where the
  value lands, so there is no single leaf to mark. Coarse, and wrong only in the safe
  direction.
- **An unknown argument defers the whole render** until apply, because a template has no way
  to represent "not yet".
- **`${file.NAME}` takes no argument.** It reads verbatim, so there would be nothing for one
  to affect, and accepting one would advertise behaviour that does not happen.

### Passing a literal

Map and list literals hold literal values, so you cannot mix references into one inline. Use
a map variable, as above, or compose one:

```yaml
    policy: ${template.access.json(merge(var.base_args, {team: platform}))}
```

## Functions

The function set is small and every function is pure — same inputs, same output, no I/O, no
clock, no randomness:

| Function | Does |
| --- | --- |
| `until n` | `[0 1 … n-1]`, for `range` |
| `seq a b` | `[a … b]` inclusive |
| `indent n s` | indent every line of `s` by `n` spaces |
| `quote s` | wrap in double quotes, escaping what needs it |
| `upper s` / `lower s` | case |
| `trim s` | strip surrounding whitespace |
| `join sep list` | join with a separator |
| `sortAlpha list` | sorted copy |

That is the entire set, and it is pinned by a test so it cannot grow by accident.

These are the `{{ }}` pass's functions, and they are **not** the same list as the seven
built-ins the `${ }` grammar has (`lower`, `upper`, `trim`, `replace`, `join`, `default`,
`merge`). Four names appear in both and mean the same thing; the two sets are separate
because the two passes are.

**Why not sprig**, since that is the usual answer in Go: it is 211 functions across 26
modules, sixteen of which contradict guarantees this project makes. `env` and `expandenv`
read the environment, so a secret would reach rendered text as a literal with its sensitivity
stripped — precisely the leak the two-pass order exists to prevent, handed back as a builtin.
`uuidv4`, `now` and the `rand*` family would mean two plans of one unchanged configuration
differ, breaking plan determinism. `getHostByName` performs a DNS lookup while rendering.
Excluding them means maintaining a denylist against an API that grows on somebody else's
schedule, where a miss is silent. Building the set up is safer than cutting one down.

If a template fails to render, the error names the file and lists the functions available, so
a typo in a function name does not read as a syntax error.

## When not to use a template

Templates are for documents. They are deliberately not a way to generate configuration:
there is no templating pass over `infrena.yml` itself, and there is not going to be one.
Generated configuration means every diagnostic points at a line nobody wrote, and being able
to read an error and go straight to the file it names is worth more than the loops would be.

For "I need N of these", use [`for_each`](configuration.md#for_each). For "these three
resources always go together", use a [module](modules.md).
