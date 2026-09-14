# Provider-declared resource references, and passing a resource whole

**Date:** 2026-09-14
**Status:** draft, awaiting review
**Amends:** `PLAN.md` §14 (a new §14.3), §17, §31.1, §61.3
**Raises:** the plugin protocol to 3
**Depends on:** `docs/superpowers/specs/2026-09-14-reference-grammar-design.md`, shipped as v0.5.0

---

## 1. Why

James, on the problem this exists to solve:

> I've had this exact issue, creating new terraform, providing an id when it wanted an
> arn, and vice versa. Just passing the VPC to it seems way easier.

Today a reference names an attribute and the engine has no idea what that attribute MEANS.
`vpc_id: ${vpc.arn}` is accepted, planned, applied, and rejected by AWS — or worse,
accepted by AWS and wrong. The engine cannot help, because `vpc_id` is a string to it and
`${vpc.arn}` is a string to it.

Two things follow from teaching the schema what an attribute refers to, and the second is
the one that pays for the first:

- **`vpc_id: ${vpc}`** — pass the resource, and the engine picks the attribute the target
  expects. You stop needing to know whether this particular API wants the id or the arn.
- **`vpc_id: ${database.id}`** becomes a COMPILE-TIME ERROR, because the schema knows
  `vpc_id` refers to an `aws.ec2.vpc` and `database` is not one. Terraform cannot do this;
  it finds out when the API says no.

v0.5.0 (spec one) freed the bare-name slot: `${vpc}` currently parses and reports "is not a
reference … a resource reference needs an attribute". This spec replaces that error with
the projection.

## 2. What `${vpc}` means

**The CONSUMING attribute declares what it refers to. The projection reads that
declaration.**

```yaml
subnet:
  type: aws.ec2.subnet
  vpc_id: ${vpc}       # aws.ec2.subnet's vpc_id refers to aws.ec2.vpc's VpcId
  cidr: 10.0.1.0/24
```

**A trap worth naming, because it caught me writing this spec.** The AWS plugin's
`gen/overlay.yaml` already contains `name: vpc` for `AWS::EC2::Subnet` — but under
`requirements:`, not `aliases:`. That is a REQUIREMENT's name (§17, and §2.3 below), not an
attribute alias, and `VpcId` has no alias today. So `vpc: ${vpc}` does not parse now and
this spec does not make it parse.

Adding `VpcId: [vpc]` to the overlay's `aliases:` table is a one-line change and worth doing
alongside — §14.1 shipped the mechanism — but it is a separate thing from the projection,
and conflating the two is exactly the confusion the identical spelling invites.

### 2.1 Consumer-declares, not producer-declares

Considered and REJECTED: each resource type declaring one "identity attribute", with
`${vpc}` always meaning that.

It is cheaper — one entry per type rather than one per relationship, and largely derivable
from `primaryIdentifier`, which every CloudFormation schema carries. It also cannot solve
the stated problem. The whole complaint is that different consumers of the SAME type want
different attributes: a subnet wants the VPC's id, something else wants its arn. A single
identity per type makes one of those callers wrong, which is where we started.

So the declaration lives on the consuming attribute, and the cost is a bigger table. §4 is
about paying for that table without hand-writing it.

### 2.2 `${vpc}` is SUGAR, and that is the load-bearing property

`${vpc}` never replaces `${vpc.arn}`. Both are legal, forever.

This is what makes partial coverage shippable rather than a half-feature:

- An attribute with **no** declared reference: `${vpc}` is an error naming the fix — "name
  the attribute you mean". The user writes `${vpc.arn}` and is exactly where they are today.
- An attribute with a **wrong** declared reference: caught by §5's type check the moment the
  passed resource is of the wrong type, at compile time.
- An attribute with a **right** declared reference: the projection works.

**The engine never guesses.** There is no fallback to "probably the id". A missing
relationship degrades to today's behaviour; it never degrades to a wrong value. That is the
property that lets §4 ship a table covering some relationships rather than waiting for all
of them.


### 2.3 How this relates to `Requirement`, which already exists

`schema.Requirement` (`pkg/schema/definition.go:13`) already declares relationships, and the
AWS overlay already hand-maintains them:

```yaml
requirements:
  AWS::EC2::Subnet:
    - name: vpc
      types: [AWS::EC2::VPC]
      description: A subnet must be created inside a VPC
```

**They are different axes and both are wanted.**

- `Requirement` is TYPE-level: *this resource type needs something of type X to exist at
  all.* It powers §17's missing-resource detection, which runs at `validate` before any
  attribute is examined.
- `References` is ATTRIBUTE-level: *this particular attribute holds an X's Y.* It powers the
  projection and the type check.

A `Requirement` cannot do the projection, because it never says which attribute carries the
reference — nor which attribute of the target is used, which is the id-versus-arn question
this whole spec exists for.

**But `References` can DERIVE `Requirement`.** A required attribute referring to type X
means the type requires an X. So the hand-maintained `requirements:` table becomes
redundant for every relationship `References` covers, and the two should be reconciled
rather than left to drift — two hand-maintained tables saying overlapping things about the
same relationship is how they come to disagree.

**Not settled here, and it should be settled on review.** Deriving `Requirement` from
`References` is the tidy end state, but §4's coverage is partial, so a derivation would
silently DROP the requirements the overlay states today for relationships the heuristic did
not find. The conservative order is: populate `References`, leave `requirements:` alone,
and add a generation check that every `requirements:` entry has a matching `References`
somewhere — turning the existing table into a CROSS-CHECK on the new one rather than a
casualty of it. That also makes the existing entries do useful work immediately: they are
already human-approved relationship facts, which is precisely what §4.2's allowlist is
short of.

## 3. The schema field

```go
// References names what this attribute refers to, when it holds another
// resource's identifier rather than a value of its own.
//
// It is DATA, not behaviour. §31.1's rule that nothing in pkg/schema may gain a
// function-typed field is why this is a type name and an attribute name rather
// than a resolver a plugin supplies: a function cannot cross a pipe, and a
// plugin that resolved its own references would be a second resolver free to
// disagree with the engine's.
type Reference struct {
	// Type is the resource type referred to, in the plugin's own naming —
	// "aws.ec2.vpc", not "AWS::EC2::VPC".
	Type string
	// Attribute is which of that type's attributes this one holds. It is the
	// CANONICAL name, never an alias (§14.1: the canonical name is the identity).
	Attribute string
}
```

added to `schema.Attribute` as `References *Reference`.

**Nil means "not a reference", and that is the honest default.** Most attributes are not
references, and an empty `Reference{}` would be indistinguishable from a plugin author who
meant to fill it in.

`schema.Validate` refuses a definition whose `References.Type` names a type the plugin does
not declare, or whose `References.Attribute` names an attribute that type does not have.
A plugin with a broken relationship is a plugin that will not load, on the §14.1 precedent
that a collision should be a load failure rather than a silent runtime surprise.

## 4. Where the table comes from

**AWS does not publish this.** Measured against the real CloudFormation bundle
(`schemas/CloudformationSchema.zip`, 1730 types):

```
types carrying `relationshipRef` at all:     26   (1.5%)
  ... and often null: AWS::EC2::EIPAssociation's is literally `EIP: null`
AWS::EC2::Subnet.VpcId                       prose description only
AWS::EC2::SecurityGroup, RouteTable, Route, Instance    no relationship data at all
```

So it is derived. The heuristic: a writable property named `<Base><Suffix>`, where Suffix
is `Id`/`Ids`/`Arn`/`Arns`, refers to the type whose name's last segment is `<Base>`, at
that type's `<Suffix>`-bearing attribute. With three refinements — a **same-service
tiebreak** (`AWS::EC2::Route.InstanceId` picks `AWS::EC2::Instance` over Connect's and
Lightsail's), the target's `primaryIdentifier` accepted as the attribute
(`IAM::ManagedPolicy`'s identifier is `PolicyArn`, it has no `Arn`), and a suffix-match
fallback (`MonitoringRoleArn` → `…::Role`):

```
candidate properties                      1614
  MATCHED                                  993   61.5%   (naive, without the refinements: 33.6%)
    tier 1 — exact name match               832
    tier 2 — suffix-match fallback          161
  ambiguous                                399   24.7%
  no such type                             159    9.9%   ← mostly CORRECT negatives
  target lacks the attribute                63    3.9%
```

61.5% understates it: `AvailabilityZoneId`, `KernelId`, `RamdiskId` and `ImageId` refer to
things CloudFormation does not manage, so "no such type" is usually the right answer.

### 4.1 The heuristic FABRICATES, which decides the design

```
AWS::RDS::DBInstance.DBSystemId              -> AWS::ResilienceHubV2::System.SystemId   WRONG
AWS::ApplicationInsights::*.SNSNotificationArn -> AWS::Connect::Notification.Arn         WRONG
AWS::EC2::LocalGatewayVirtualInterface.OutpostLagId -> AWS::DirectConnect::Lag.LagId     WRONG
AWS::MWAA::Environment.ExecutionRoleArn      -> AWS::IAM::Role.Arn                       right
```

`DBSystemId` is an Oracle CDB name, not a reference to anything. A fabricated relationship
is worse than a missing one: §5's type check would reject a correct configuration.

So matches are not equal, and the design follows the evidence rather than a uniform rule:

- **Tier 1 (832) is auto-accepted.** An exact type-name match with a same-service tiebreak
  is mechanical.
- **Tier 2 (161) requires human approval, entry by entry.**

### 4.2 The tier-2 curve is dominated by one entry

Measured, ranked by how many attributes point at each target:

```
 1. AWS::IAM::Role.Arn                        84    52.2% cumulative
 2. AWS::EC2::Subnet.SubnetId                  8    57.1%
 3. AWS::EC2::IPAMPool.IpamPoolId              7    61.5%
 4. AWS::EC2::VPC.VpcId                        4    64.0%
 5. AWS::DMS::Endpoint.EndpointArn             4    66.5%
 ...
10. AWS::Lambda::Version.FunctionArn           2    75.2%
```

**Approving `AWS::IAM::Role.Arn` alone captures 52% of tier 2**, and it is the single most
valuable relationship in AWS — `*RoleArn` is the id/arn confusion James actually hit. The
first four entries reach 64%; ten reach 75%.

And the tail is where the fabrications live: `AWS::Connect::Notification.Arn` and
`AWS::ResilienceHubV2::System.SystemId` are both in the top 14 and both wrong. So the review
is not ceremony — it is the thing that separates the 52% that matters from the entries that
would break correct configuration.

**Decision: ship tier 1 plus a reviewed tier-2 allowlist, approved by TARGET type.** Start
with `IAM::Role`, `EC2::Subnet`, `EC2::VPC`, `EC2::RouteTable`, `EC2::SecurityGroup`,
`KMS::Key`, `S3::Bucket`, `Logs::LogGroup`. Approving a target type approves every tier-2
edge pointing at it, because the judgement being made — "is a property named `<X>RoleArn`
really a reference to an IAM role" — is a judgement about the target, not about each of the
84 consumers.

### 4.3 The lock file, so a schema refresh cannot add a relationship silently

`gen/names.lock.json` already exists in the AWS plugin — 99KB of generated-then-pinned data
whose whole job is that a new AWS type cannot silently rename an existing resource. A
relationships lock is the same mechanism for the same reason.

- Generation writes every tier-1 match and every APPROVED tier-2 match.
- A tier-2 match whose target is not in the allowlist is written to the lock as
  **pending**, and is NOT emitted into the catalog.
- A relationship that appears on a schema refresh and is not in the lock **fails
  generation**. Someone looks at it.

This is what keeps the fabrication risk bounded over time rather than only at first write.

## 5. The type check, which is half the value

With `References` populated, the compiler checks the referred type at stage 6:

```
aws.ec2.subnet.vpc_id refers to aws.ec2.vpc, and `database` is aws.rds.dbinstance

  vpc: ${database}
       ^^^^^^^^^^^

Pass an aws.ec2.vpc, or name the attribute you mean explicitly.
```

This fires for `${database}` AND for `${database.id}` — naming an attribute explicitly
escapes the projection, not the type check. A reference into the wrong resource is a
mistake whichever spelling it wears.

**It fires only when `References` is populated**, so an undeclared relationship checks
exactly as much as it does today: nothing. Coverage buys checking; absence costs nothing.

This also bears on §17's missing-resource detection, which today reads the hand-maintained
`requirements:` table — see §2.3 for why the two should be reconciled rather than one
quietly replacing the other.

## 6. Nested attribute shape, and the check spec one deferred

Spec one §3.3 shipped paths into resource attributes (`${vpc.tags.Name}`) with the final key
UNCHECKED, because `pkg/schema` models `tags` as `Kind: KindMap` and nothing deeper. It
recorded that closing the gap "belongs with spec two's protocol bump if it happens at all".

It happens here, for one reason: **the protocol bump is the cost, and it is already being
paid.** Two schema additions across two releases means two bumps and two windows where a
plugin and a host disagree.

```go
// Fields describes a KindMap attribute's known keys, where the provider knows
// them. Nil means the map is open — any key, checked at apply as today.
Fields map[string]Attribute
```

With it, `${vpc.tags.Nmae}` is a compile-time error listing the keys that exist, which is
what `bind.go:400` already does for a top-level attribute name and what its comment says is
worth doing:

> a typo here passed `validate`, produced a clean plan, and failed halfway through `apply`
> after real infrastructure existed

**Nil is a first-class answer, not a gap.** AWS tags are an open map and always will be;
declaring `Fields` for them would be a lie. The check applies where a provider genuinely
knows the shape.

**This section is separable.** If review wants a smaller spec two, it can be cut and the
protocol bump saved for it — but then it must be raised again later, and §31.1's rule that
the protocol is the compatibility contract means that is a real cost, not a bookkeeping one.

## 7. Protocol 3

`pluginproto`'s own rule is that the package changes additively and "a new optional field is
fine". But the v2 bump was taken for a schema-payload addition anyway, and its reasoning
applies here word for word:

> Attribute.UnmarshalJSON decodes leniently, so a plugin built with this SDK talking to an
> OLDER host would have both keys silently DROPPED.

A plugin declaring `References` against a v0.5 host would have them dropped, and `${vpc}`
would report "no reference target declared" with nothing anywhere explaining why the plugin
that clearly declares one is not being heard. Announcing 3 makes that a refusal that names
the plugin and the versions instead.

`Supported` becomes `{3, 2, 1}`. A plugin using neither new field keeps announcing 2 and
keeps working, which is the point of `Supported` being a set.

## 8. Diagnostics

| Trigger | Says |
|---|---|
| `${vpc}` where the attribute declares no reference | name the attribute you mean, as `${vpc.id}` |
| `${vpc}` where the target type does not match | what the attribute refers to, what was passed, both type names |
| `${vpc}` naming a module instance | a module exposes outputs; name one |
| `${vpc.tags.Nmae}` with `Fields` declared | no key `Nmae`; lists the keys that exist |
| a plugin whose `References.Type` is undeclared | the plugin does not load, naming the attribute |

Every one names the fix, per §44, and every list of names is sorted — invariant 6.

## 9. Testing

- The projection resolves through a real plan and apply, not only at the parser.
- An attribute with no declared reference still accepts `${vpc.arn}` and still refuses
  `${vpc}` — the degradation path in §2.2, which is what makes partial coverage safe.
- The type check fires for both `${database}` and `${database.id}`.
- A saved plan carrying a projected reference round-trips (§37). Spec one shipped a wire bug
  of exactly this shape, found only at final review, because the in-memory test never
  crossed the wire. **Assert across `MarshalJSON`, not on `Expr.String()`.**
- Generation: a tier-2 match outside the allowlist is written pending and NOT emitted; an
  unknown relationship on a schema refresh fails generation.
- A host at protocol 2 refuses a plugin announcing 3, naming both.

## 10. Out of scope, recorded so each is a decision

- **Producer-declared identity** (§2.1) — cannot express two consumers wanting different
  attributes of one type, which is the whole problem.
- **Any fallback when no relationship is declared** (§2.2) — guessing "probably the id" is
  how a wrong value ships silently. The error is the feature.
- **Emitting `${vpc}` from `import --generate`** — generated configuration writes literal
  discovered values (§27); teaching it to infer references is a separate question.
- **Reference targets for module outputs** — a module publishes outputs, not schema'd
  attributes. `${prod}` bare stays an error.
- **Deriving `Requirement` from `References`** (§2.3) — the tidy end state, but partial
  coverage means a derivation would silently drop requirements the overlay states today.
  Cross-check first; derive when coverage justifies it.
- **Adding `VpcId: [vpc]` and its siblings to the overlay's alias table** (§2) — worth
  doing, a separate change, and not a prerequisite for anything here.
- **Auto-approving tier 2** (§4.1) — the fabrications are in the top 14, not the long tail.
