# Documentation

Start with the [README](../README.md) for what Infrena is and how to install it. These pages
are the detail.

## Writing configuration

| Page | Covers |
| --- | --- |
| [configuration.md](configuration.md) | The language: `infrena.yml`, resources, references, functions, `for_each`, `lifecycle`, `depends_on`, `skip`/`only` |
| [environments.md](environments.md) | Variables, environments, where values come from, and the precedence between them |
| [modules.md](modules.md) | Grouping resources, inputs and outputs, remote sources |
| [templates.md](templates.md) | Keeping documents in their own files, and the two-pass render |
| [secrets.md](secrets.md) | `${secret.}`, the vault, and what redaction does and does not cover |

## Running it

| Page | Covers |
| --- | --- |
| [ci.md](ci.md) | Exit codes, the reviewed-plan workflow, protected environments, machine-readable output |
| [state-backends.md](state-backends.md) | Where state lives, locking, and migrating between backends |

## Around the project

| Page | Covers |
| --- | --- |
| [open-core.md](open-core.md) | What stays free, and why that line cannot move |
| [provider-hazards.md](provider-hazards.md) | What to watch for when writing a provider plugin |
| [using-ai.md](using-ai.md) | Using AI tools on this codebase, and who remains responsible |

There is a worked project in [`examples/shop`](../examples/shop), covering directories,
scoped variables, a module, and per-environment values.
