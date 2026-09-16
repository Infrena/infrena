# Contributing to Infrena

Thanks for considering it. This file covers the two things worth knowing before you spend
time: how the project works, and why there is a CLA.

## Before you start

**Open an issue first for anything non-trivial.** Infrena is designed rather than accreted —
`PLAN.md` is a long specification, and most decisions in the codebase were argued before they
were written. A pull request that cuts across one of those decisions is not a bad pull
request, but it is a conversation, and it is cheaper to have before the code exists.

Small fixes — a typo, a wrong comment, a genuinely broken thing — just send them.

## What the project expects of code

These are not style preferences. They are the rules the codebase is built on, and a change
that breaks one will be sent back:

- **The core knows nothing about any specific cloud.** No AWS types, no cloud-specific
  branching outside a provider plugin.
- **Tests come first, and a test must be able to fail.** Write it, watch it fail for the right
  reason, then make it pass. A test that passes when you delete the code it covers is worse
  than no test, because it reports safety that is not there. This project keeps a running
  count of tests that turned out to be incapable of failing; do not add to it.
- **The third-party dependency budget is two packages** — Cobra and a YAML parser. Adding a
  third is a decision to be argued in an issue, not in a pull request.
- **Errors say what is wrong, where, what was expected, and what to do about it.** A
  diagnostic whose suggested action the reader cannot take is a bug.
- **Values keep their provenance.** Do not merge defaults into user configuration and lose
  where a value came from; plans, generated configuration and `explain` all depend on it.
- **Document what you changed.** Work is not finished until the docs reflect it.

Run this before you push:

```bash
go test ./...
go vet ./...
gofmt -l .
```

The integration suite needs the fake provider plugin; set `INFRENA_REQUIRE_PLUGIN=1` so that a
suite which silently skips counts as a failure rather than reporting green.

## Commit messages

Plain English, present tense, and explain **why** rather than what — the diff already says
what. No em-dashes. If a commit fixes something subtle, the message is the right place to
record how it was found.

## Why there is a CLA

Most projects this size use a lightweight sign-off instead. Infrena asks for a
[Contributor License Agreement](CLA.md), and it is worth being straight about the reason
rather than leaving you to guess.

**What it is not:** it is not a copyright assignment. You keep ownership of everything you
write. Nobody is taking your code.

**What it does, for you:** it makes your grant irrevocable. Once your contribution is in, it
cannot be pulled back out from under people who have built on it — including you changing your
mind, and including anyone claiming rights through you later.

**What it does, for the project:** there is a commercial platform planned around Infrena —
hosted, paid, aimed at organisations. The CLA means the project has the rights it needs to
build that without chasing every past contributor for permission.

**The limit that makes this fair.** Clause 4 permits relicensing **only to other
OSI-approved open source licences**. The project cannot take your contribution — or the CLI —
proprietary or source-available. That restriction is deliberate, and it is there because this
is precisely the mechanism by which Terraform was moved to a non-open licence in 2023. You do
not have to take anyone's word that it will not happen here; the agreement you sign does not
permit it.

What is free and what is paid is written down in [docs/open-core.md](docs/open-core.md),
including the rule that nothing already free may ever move to the paid side.

## Signing

Comment on your first pull request:

```
I have read the CLA document and I hereby sign the CLA.
```

Once, ever. Tell us in the pull request if you are contributing on behalf of an employer.

## Provider plugins

You do not need any of this to write a provider. Plugins are separate programs in separate
repositories, talking to Infrena over a documented protocol, and they are yours — your
repository, your licence, your release schedule. `pkg/` is Apache 2.0 and you may compile
against it freely.

See `PLAN.md` §31.1 and §31.2, and the `pkg/pluginsdk` package, which is the whole of a
plugin's `main()`.
