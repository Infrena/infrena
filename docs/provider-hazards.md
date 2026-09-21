# Two hazards a provider plugin only meets against a real cloud

Both of these were found by running the AWS provider against a real account, and neither can be
reproduced against a fake provider. A plugin with a green test suite can have both. They are
written down here so a plugin author can recognise them before a resource plans a change forever
in someone's production account.

Names and wording come from the AWS provider's author, who found all of them.

---

## 1. Rewritten values

> **If the cloud answers with a different value than you sent, your plugin must decide the two
> are the same, or plans never converge.**

A value the provider rewrites converges only if the plugin echoes back what the user wrote,
whenever the two mean the same thing.

"Rewritten" rather than "normalised" deliberately. Normalisation sounds like tidying up.
What actually happens is that the cloud answers with a *different value* than the one you sent,
and both are correct.

### What it looks like

The engine diffs configured against observed. If the cloud returns something that is not
byte-equal to what configuration says, the planner proposes a change — correctly, from its point
of view. Apply sends the original value again, the cloud rewrites it again, and the next plan
proposes the same change. **The failure is not an error. It is a plan that is never clean**,
which is far harder to notice in review than a crash.

### The four found so far, in the order they were found

| | What was written | What came back |
| --- | --- | --- |
| 1 | Nested values: keys in the user's spelling, list order where the schema says order is not significant | Keys respelled, keys AWS added unasked, lists reordered |
| 2 | IAM `AssumeRolePolicyDocument` as JSON text | An object — the schema types it object-or-string |
| 3 | ECR `LifecyclePolicyText`, spaced JSON | The same JSON, minified |
| 4 | RDS `EngineVersion: "17"` | `17.9` |

**Three of those four are JSON or text reformatting. The fourth is semantic** — a major version
resolved to a specific minor one. That distinction is worth knowing, because a plugin author who
has read this will go looking for a JSON-shaped problem and walk straight past the version one.

Number 1 is also why the AWS provider has a reconciliation layer at all, rather than comparing
with `value.Equal` and hoping.

### Why the engine does not fix this for you

Terraform's answer is `DiffSuppressFunc`: a function in the schema that decides whether two
values are equivalent. **Infrena cannot have that.** A provider is a separate process and a
schema has to survive a pipe, so nothing in `pkg/schema` may hold a function.

That constraint turns out to be the better design rather than a limitation to work around.
Whether `17` and `17.9` mean the same thing is knowledge about an API, and it belongs to whoever
owns that API. Doing it in the plugin, on read, makes it explicit, testable, and visible in the
plugin's own code — instead of hidden in a comparison callback the engine invokes.

### What to do

Reconcile on read: when the cloud's answer and the user's written value mean the same thing,
return the value the user wrote. Do it at any depth — hazard 3 was a plain string nested inside
an object, so a top-level check would have missed it.

---

## 2. Silent cross-resource coupling

> **When two resources must agree on a value no schema mentions, set it explicitly on both, and
> say in a comment that they move together.**

Two attributes, on two different resources, must match. Nothing in either schema says so. The
cloud enforces it at apply time and nothing before that can see it.

### The example

An RDS parameter group has a family (`postgres17`). A DB instance has an `EngineVersion`. They
must agree, and neither schema mentions the other.

**The trap is that the safe-looking choice is what breaks it.** `EngineVersion` is
optional-and-computed, so the advice everyone would give — leave it unset and let the cloud
choose — is normally right. Do that here and AWS picks its current default major version, which
was 18, and then refuses the `postgres17` parameter group outright:

```
can't be used for this instance. Use a parameter group with DBParameterGroupFamily postgres18
```

The two resources took their defaults from different clocks.

### Why the engine cannot express it

Infrena has two ways to declare a relationship, and this is neither:

- `schema.Reference` connects **an attribute to a resource** — "`VpcId` holds an `aws.vpc`'s id".
- `schema.Requirement` connects **a type to a type** — "a subnet needs a VPC to exist at all".

This coupling is **attribute to attribute, across two types**, which neither can say. Adding a
constraint language for it would put the engine back in the business of guessing about an API,
which is exactly what the schema is meant to avoid.

So it stays a known limitation, and the mitigation is the author's job: set the value explicitly
on both resources, and leave a comment saying they move together — including what to do when the
cloud retires the version you pinned.

---

## Why neither of these has a test in this repository

The fake provider applies the desired state wholesale. It never rewrites a value and it has no
second resource to disagree with. **Both hazards are invisible to infrena's own suite by
construction**, which is precisely why they were found by a live run against a real account and
not before.

If you are writing a provider, the lesson generalises past these two: a green suite against a
double is evidence that your code does what you think it does, not that the cloud agrees.
