# Infrena

**Infrastructure as code you can read.**

Infrena reconciles infrastructure described in YAML with infrastructure that actually exists. It treats environments as part of the project rather than something you build a wrapper around, catches wiring mistakes at compile time instead of halfway through an apply, and keeps its core small by putting every cloud behind a plugin.

> **The core is free and stays free.** Nothing that works today gets moved behind a paid tier later.

---

## What it looks like

```yaml
# infra.yml
infrena: ">= 0.7"
project: ec2-example

environments:
  dev: {}

variables:
  aws_region:
    type: string
    default: us-east-1

providers:
  - plugin: aws
    defaults:
      region: ${var.aws_region}

resources:
  vpc:
    type: aws.vpc
    cidr: 10.0.0.0/16
    enable_dns_hostnames: true
    tags:
      Name: ec2-example-vpc

  public_subnet:
    type: aws.subnet
    vpc: ${vpc}
    cidr: 10.0.1.0/24
    az: us-east-1a
    map_public_ip_on_launch: true

  server_security_group:
    type: aws.securitygroup
    description: Security group for EC2 instances
    vpc_id: ${vpc}
    ingress:
      - ip_protocol: tcp
        from_port: 443
        to_port: 443
        cidr_ip: 0.0.0.0/0

  web_instance:
    type: aws.ec2.instance
    image_id: ami-0c55b159cbfafe1f0
    instance_type: t3.micro
    subnet_id: ${public_subnet}
    security_group_ids:
      - ${server_security_group}
```

Note `vpc: ${vpc}` — you pass the resource, and Infrena fills in the attribute the provider says that field refers to. You stop having to remember whether a given API wants an id or an ARN.

A plan reads the way you would describe the change out loud:

```
Plan for project "demo", environment "dev":

  + fake.database.db
      endpoint: (known after apply)
      engine: "postgres"
      network: (known after apply)
      size: 10 [default, from provider default]

  + fake.network.net
      cidr: "10.0.0.0/16"
      id: (known after apply)

Plan: 2 to create, 0 to update, 0 to replace, 0 to destroy, 0 to forget.
```

Values carry their origin all the way through, which is why the plan can tell you `size` came from a provider default rather than from something you wrote.

---

## Mistakes fail before anything runs

Because the provider's schema declares what each attribute means, Infrena can check your wiring at compile time rather than discovering it when an API says no:

```
$ infrena validate
Error: fake.network has no attribute "nme"
  at infra.yml:14:5

  ${net.nme} reads an attribute that does not exist.
  Attributes of fake.network:
    cidr
    id

  Suggested action:
    Correct the attribute name.
```

The same check catches pointing a field at the wrong *kind* of resource. If a provider declares that `vpc_id` holds a VPC's id, then `vpc_id: ${database.id}` is a compile error naming the fix — not a create that fails after four other resources already exist.

Every diagnostic is required to say what is wrong, where, what was expected, and an action you can actually take. That is a rule the project enforces on itself, not an aspiration.

---

## Environments are part of the project

```yaml
environments:
  dev: {}
  staging: {}
  production: {}
```

Each environment has its own state and its own lock, so two applies against different environments do not contend. Values differ per environment through variables, not through copied directories:

```
vars/
├── default.yml       # applies everywhere
├── staging.yml       # overrides default, value by value
└── production.yml
```

Resolution order is fixed and documented: provider defaults → base configuration → module defaults → environment inheritance → environment variables → `--var` on the command line. Explicit configuration always beats an implicit default.

Removing an environment from `infra.yml` does not silently orphan it. Infrena proposes the teardown and shows you exactly what it would destroy before you agree.

---

## Adopting infrastructure you already have

Discovery is read-only. It tells you what exists and what each resource *would* be called, so a name collision is visible before it happens rather than after:

```bash
infrena discover
```

```
TYPE        ID                     NAME
aws.subnet  subnet-0a1b2c3d4e5f    subnet-app1a
aws.vpc     vpc-1023902339         vpc-app1

2 resources found. Nothing has been imported.
```

Names come from a resource's `Name` tag where it has one, so you get `vpc-app1` rather than `vpc-1023902339`. When you are ready, import adopts them into state and — with `--generate` — writes the configuration that declares them:

```bash
infrena import dev --generate
```

Generated configuration is written under `discovered/`, which is loaded like any other directory but kept separate so you can read it before deciding where things belong. It is **minimal**: anything equal to a provider default is left out, so you get a file worth reviewing rather than a dump. Resources reference each other by name instead of pasting ids:

```yaml
subnet-app1a:
  type: aws.subnet
  VpcId: ${vpc-app1}        # not vpc-1023902339
  CidrBlock: 10.0.1.0/24
```

The round trip is a tested guarantee: discover → import → generate → plan produces no unexpected changes. And a generated file never contains a secret — a sensitive attribute is omitted, with a comment at that line saying so.

Filters keep it manageable on a real account:

```bash
infrena discover --tag Name=app1 --exclude-type aws.logs.loggroup
```

Resources this project already manages are left out by default, as are resources the provider says the cloud created for itself — an account's default VPC is not something you want to adopt by accident. `--all` shows everything.

---

## Project layout

Start with one file:

```
infra.yml
```

Split it up when it earns it:

```
infra.yml
├── resources/          # your infrastructure, organised however suits you
│   └── networking/
│       ├── vpc.yml
│       └── vars/       # variables scoped to just this directory
├── vars/               # project-wide values, one file per environment
├── modules/            # reusable components
└── discovered/         # generated by import, reviewed by you
```

Four directory names are read automatically at any depth and nothing has to list them. A directory scopes variables but never names, so moving a file between directories renames nothing. Declaring the same name twice is an error that points at both files rather than silently keeping one.

The single-file and directory forms produce byte-identical plans. That is pinned by a test, so growing a project is a layout change and never a behaviour change.

---

## Providers are plugins

Providers run as separate processes and are distributed separately, so the core never grows an SDK:

```bash
infrena plugins list        # what is installed, and where it was loaded from
infrena plugins search aws  # find one across every source you trust
```

A project may *name* where a plugin comes from. Only you may *trust* a source — project configuration travels with a `git clone`, so it must not be able to introduce a place Infrena downloads executables from.

The engine has **two third-party dependencies**: Cobra and a YAML parser. The AWS provider's SDK lives in the AWS provider, not in your Infrena binary.

---

## State, and where it lives

State records what Infrena manages. By default it is a file per environment under `.infra/`, locked while a run holds it, and that is the whole setup — a project that never says otherwise never has to think about this.

To share it, name a backend:

```yaml
backend:
  plugin: s3
  bucket: my-infrena-state
```

**Backends are plugins, exactly as providers are** — including S3. Nothing about object storage is built into the engine, so a store Infrena has never heard of needs a backend, not a patch.

Two rules that are not configurable, because the failures they prevent are silent ones:

**Every backend must lock, and there is no unsafe fallback.** The S3 backend locks with a conditional write, and proves the store honours it before serving anything: it writes a throwaway object twice and requires the second write to be refused. A store that ignores the header reports success for both, which looks exactly like a lock that never locks — so the bucket is refused when the project loads, rather than during an apply that is already under way. AWS S3 and MinIO have been run against the full live suite and pass. Backblaze B2 fails and is refused, which is the one result you would rather learn before writing a configuration than after. Cloudflare R2 and DigitalOcean Spaces are expected to work but have not been tested here — and because the check runs at load time, an untested store costs you a clear message rather than a lock that silently does nothing.

**Credentials are never configuration.** `infra.yml` is committed, so there is deliberately nowhere in the `backend:` block to put a secret — writing one there is an error that points at `profile:` instead. Accepting it would have worked perfectly and put a long-lived key in a repository.

Moving between backends is a command, not a manual copy:

```yaml
backend:                    # where state should live
  plugin: s3
  bucket: new-bucket
migrate_from:               # where it lives today
  plugin: local
```

`infrena state migrate` **copies, and never empties the source**, so a migration that goes wrong leaves the old backend still holding the record. Until it runs, ordinary commands refuse rather than treating an empty destination as an empty world. `state migrate --check` answers the same question for CI without touching anything.

---

## Machine-readable output

`--output` writes newline-delimited JSON for a frontend or a pipeline to consume, and **stdout stays empty** so nothing has to be parsed out of human text:

```bash
infrena apply production --output run.ndjson --auto-approve
```

Exit codes are stable: `0` no changes, `1` error, `2` changes applied, `77` changes need an approval this run cannot obtain. That last one exists so a pipeline gets a distinguishable answer instead of a prompt nobody can see. [docs/ci.md](docs/ci.md) is the whole picture: the two-stage reviewed-plan workflow, protected environments, secrets from the environment, and what the machine-readable output contains.

---

## Getting started

Download a binary from [Releases](https://github.com/Infrena/infrena/releases) — there is one for
Linux, macOS and Windows, on both Intel and ARM. Verify it, unpack it, and put it on your `PATH`:

```bash
VERSION=0.11.1
BASE=https://github.com/Infrena/infrena/releases/download/v$VERSION

curl -fsSLO $BASE/infrena_${VERSION}_linux_amd64.tar.gz
curl -fsSLO $BASE/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing     # shasum -a 256 -c on macOS

tar -xzf infrena_${VERSION}_linux_amd64.tar.gz
sudo mv infrena_${VERSION}_linux_amd64/infrena /usr/local/bin/
infrena version
```

**Check the checksum.** Infrena verifies every plugin it installs against `plugins.lock`, and a
tool that does that while shrugging at its own download would not be worth believing. `SHA256SUMS`
is published with every release for exactly this.

Create a project — by default in `./infrena/`, so infrastructure sits beside the application it belongs to:

```bash
infrena init
infrena init .          # or scaffold in place
infrena init --provider aws
```

Then the loop:

```bash
infrena validate        # no providers contacted
infrena plan dev
infrena apply dev
```

Every release ships binaries for eight platforms:

| OS | Architectures |
| --- | --- |
| macOS | amd64, arm64 |
| Linux | 386, amd64, arm64, armv7 |
| Windows | amd64, arm64 |

Each release includes `SHA256SUMS`.

---

## Status

Infrena is pre-1.0 and under active development. The engine is complete and tested — the full lifecycle of create, plan, apply, drift detection, refresh, import and destroy runs end to end, with six correctness invariants asserted explicitly rather than assumed.

Being straight about the rest: there is one production provider (AWS, via Cloud Control, covering 1,584 resource types), and nothing has yet run at scale against a large production account. Remote state works — an S3-compatible bucket, with locking, and migration between backends — but it is new, and the backends are plugins so a store nobody has tried is a store nobody has tried. Configuration syntax may still change, and releases say plainly when they break something.

Design principles the project holds itself to, in rough order of how easy they are to violate:

1. Configuration describes desired infrastructure; state records what is managed; the planner finds the difference.
2. The core knows nothing about any specific cloud.
3. Values keep their origin all the way through.
4. Errors say what to do about them.
5. Simple projects stay simple; large projects have room to grow.
6. The core stays small, and stays free.

---

## Licence

Apache 2.0 — see [LICENSE](LICENSE).

The CLI is free and open source, and stays that way. A commercial platform is planned for
organisations — collaboration, access control, audit, policy, drift history — but nothing
that works in the CLI today will move behind it. [docs/open-core.md](docs/open-core.md)
records where that line sits and why it cannot move.

Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
