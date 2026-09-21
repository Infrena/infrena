# What is free, and what is paid

This document exists so that the line between Infrena the tool and Infrena's commercial
platform is written down **before** there is any revenue pressure on it. A boundary decided
under pressure is a boundary that moves.

## The rule

> **Infrena the CLI is free and open source, forever. Everything a single operator or a CI
> runner needs to manage infrastructure is core, and stays core.**
>
> **The commercial platform sells what an organisation needs: coordination between people,
> and history over time.**

Nothing that works in the CLI today will ever move behind a paid tier. That is not a
marketing line; it is the reason this file exists, and it is enforceable — the CLI is
Apache 2.0, so every release that has ever shipped stays usable under that licence no matter
what the project does later.

## Free, in the CLI, forever

- Plan, apply, destroy, refresh, and the whole reconcile loop
- Local and **S3 remote state, including locking**
- Provider plugins, and the ability to write your own
- Discovery and import of existing infrastructure
- Modules
- Environments, variables and the precedence rules
- Drift detection on demand
- The engine-enforced safety rules: `prevent_destroy`, `retain`, `require_approval`
- Machine-readable NDJSON output
- Running anywhere you like, including in your own GitHub Actions

**State locking is explicitly free, including on S3.** Invariant 5 says two applies cannot
mutate one environment concurrently, and a free backend that could not enforce that would ship
a corrupted-state hazard under this project's name. The paid product sells coordination
*between many operators* — queuing, run ordering, seeing who holds what — never the mutex
itself.

**The state backend interface is public.** Anyone may write a backend, the same way anyone may
write a provider. That includes a backend that competes with the hosted one. The platform's
value is everything around state, not custody of it.

## Paid, in the platform

For organisations: collaboration, role-based access control, audit trails, run history,
policy enforcement across many projects, drift monitoring and drift-over-time, managed secrets,
VCS integration that watches a repository, and hosted state with coordination at scale.

## Six pairs that share a name

Several platform features have a counterpart that already ships in the CLI. They are additions,
not replacements, and the distinction matters enough to write down — a name collision with no
explanation reads like something was taken away.

| In the CLI, free | What the platform adds |
| --- | --- |
| `refresh` detects drift on demand | Continuous monitoring, and drift history over time |
| `prevent_destroy`, `retain`, `require_approval` | Policy-as-code applied across many projects at once |
| Sensitive values are redacted and never written to generated files | A managed secret store |
| `--output` writes an NDJSON report of a run | Retained, queryable audit across an organisation |
| `plan` and `apply` in your own GitHub Actions | The platform watching a repository and triggering runs itself |
| State locking, per environment, including on S3 | Coordination among many concurrent operators |

In every row the left column is free forever. The right column is something that does not exist
in the CLI today and never did.

## Why this is not the Terraform story

Terraform was open source for a decade while Terraform Cloud was proprietary, and that
arrangement was fine. What broke trust was changing the licence of the open tool itself in
2023, after people had built on the promise that it would not.

Two things make that harder to do here, deliberately:

1. **Apache 2.0 is irrevocable for anything already published.** A future licence change could
   only affect future releases; everything shipped stays free under the licence it shipped with.
2. **This file exists.** The boundary was written down while the project had no revenue and
   nothing to gain from blurring it.

Infrena also gives away more than Terraform did at the equivalent point: remote state was the
hook into Terraform Cloud, whereas here S3 state, locking included, is free and always will be.

## The platform is a separate project

**James, 2026-09-17: "The paid feature stuff is another project entirely, and does not apply to
infrena core except where we draw the line on functionality."**

So this document is the ONLY place the commercial product touches this repository. There is no
platform code here, no hooks for one, and no feature in the CLI shaped to leave room for one. The
line above is the whole of the relationship.

That is worth stating because the usual way open core goes wrong is not a dramatic relicensing —
it is a slow accumulation of seams, stubs and "we'll need this for the hosted version" decisions
inside the free tool, until the free tool is shaped around a product its users cannot see. A
reviewer who finds something in this repository that only makes sense if you know about the
platform should treat it as a defect.

## Changing this document

Adding to the paid list is allowed when the addition is genuinely new. **Moving something from
the free list to the paid list is not**, and no commercial argument makes it so. If that ever
seems necessary, the honest move is to say plainly that the promise is being broken, not to
redefine the words.
