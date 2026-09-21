# State backends, and what a backend can see

This document exists because remote state moves your state file onto somebody else's
machine and into somebody else's code, and both of those are worth saying out loud rather
than leaving a reader to infer from a protocol definition.

---

## The shape, in four rules

**A backend is a plugin.** Not a branch in the engine. It lives in a repository called
`infrena-backend-<name>`, ships a binary of that name, is found in the directories
`infrena plugins install` already writes to, and is verified against the same committed
`plugins.lock`. An author imports `pkg/backend` for the interface and the lock type,
speaks `pkg/backendproto` over stdio, and calls `backendsdk.Serve` as the whole of their
`main()` — seven methods and one call, which is what a provider author already gets.

**Local is the bootstrap and is built in.** It writes to `.infrena/` and works before
anything has been installed, which is precisely why it cannot itself be a plugin:
something has to hold state for the project that has not installed one yet. A project with
no `backend:` block gets local, starts no process and touches no network.

**A shipped infrena carries no remote backend**, exactly as it carries no provider. If
your state is in S3, you installed the thing that puts it there, and `plugins.lock`
records what you installed.

**Every backend must lock.** A backend that cannot lock is refused when it loads, not when
an apply reaches for it — and in practice it does not compile, because locking is part of
the interface rather than an option. Two applies must never mutate one environment
concurrently.

---

## Configuring one

```yaml
backend:
  plugin: s3            # the only key infrena reads
  bucket: acme-tfstate  # everything below here is the backend's own
  region: eu-west-1
  path: /infrena/
```

`plugin:` names the implementation. **Every other key belongs to the backend and crosses
to it untouched**, which makes this the one block in the language where an unrecognised
key is not an error: infrena cannot know which keys an s3 backend accepts, and refusing
what it does not recognise would make every backend option a change to the core. A missing
`plugin:` *is* an error, because that is the one key infrena owns.

**Credentials do not go here.** They are the plugin's business — environment variables, an
instance profile, a credentials file, whatever it chooses. A bucket name is configuration;
a secret key is not.

`infrena validate` enforces that, which matters because `infrena.yml` is committed and
`validate` is the cheap gate CI runs. Writing an `access_key_id` into the block fails there,
before anything is contacted and before the key is ever pushed:

```
Error: `backend` is not a block the s3 backend can read
  at infrena.yml:2:1

  backend s3: `backend.access_key_id` is a secret and this backend will not read
  one: infrena.yml is committed to git. Set `profile:` to name a profile in your AWS
  credentials file instead, or leave credentials to the environment or an
  instance role
```

The backend answers that question itself, over an offline-only protocol method, because only
the backend knows which of its keys are credentials. A backend that does not implement the
method is not asked, and its block is checked when it runs — as every backend's was before
the method existed.

### `backend:` may not use variables, and this will not change

A `${...}` anywhere in the block — including three maps deep, or behind a YAML anchor — is
a diagnostic naming the key. The reason is an ordering cycle rather than a missing
feature:

> **State is read before anything is compiled, and compiling is what resolves variables.**

`destroy`, `refresh`, `discover` and `import` never compile at all, and every other command
has to open the backend to find out what already exists. So there is no point in any run
at which a value here could be filled in. Reading state would have to wait for a compile
that is waiting for state.

`providers:` may interpolate because provider instances are constructed *after* variables
resolve. `backend:` is read before all of it. A request to "just support variables in
`backend:`" is a request to break that ordering, and the answer is to give the key a
literal value, keep environments that need different state in separate projects, or let
the backend read the value from its surroundings the way a provider reads credentials.

---

## Moving state between backends

`infrena state migrate` copies state from one backend into another. Both ends are named in
configuration, because a migration with one end implicit is a migration that can go somewhere
nobody asked for:

```yaml
backend:                  # where state is going
  plugin: s3
  bucket: acme-state
  region: eu-west-1

migrate_from:             # where state is today
  plugin: local
```

`migrate_from:` takes **the same shape and the same rules as `backend:`** — the same decoder,
the same literal-only rule, the same "`plugin:` is the only key infrena reads". Two blocks that
mean the same thing must not be able to disagree about what a key means.

**`plugin: local` names the built-in backend.** It has to be namable, because otherwise
"migrate back to local" could only be said by leaving the block out, and leaving it out already
means "there is no migration".

### It COPIES. It never empties the source

`state migrate` writes to the destination and removes **nothing** from the source. That is
deliberate: if a bug in the migration puts something wrong in the new backend, the old one is
still the record and you can point `backend:` back at it.

**The consequence is yours to clean up.** The old state stays where it is — every resource,
every attribute, and every secret recorded in it — until you delete it. Nothing about a
successful migration tidies the old store up, and you should not assume it did. When you are
satisfied the new backend is right, empty the old one yourself.

### Three cases, decided before anything is written

Both ends are locked for the whole operation, and both are compared before the first write,
because the interesting question is which of three situations this is and finding out halfway
through a copy is too late:

| Source | Destination | Outcome |
| --- | --- | --- |
| holds state | empty | **copy**, then read each environment back to verify it |
| holds state | holds the same state | **no-op, and SUCCEED**: "already migrated" |
| holds state | holds different state | **refuse**, naming the environments that differ |

**The middle case succeeding is not a nicety.** A migration performed through CI gets re-run —
a retried job, a re-pushed branch, a workflow that runs on every commit. If a second run failed,
the natural response to the red pipeline is to add `--force` to the workflow file, where it then
sits on every future run and silently overwrites the next real conflict. That is the same hazard
as a `lock: false` escape hatch, arriving by the same route. So a re-run of a completed
migration exits 0.

`--force` exists for the third case, and the refusal mentions it **last**, after telling you how
to find out which end is actually right. The first thing offered is the thing that ends up in a
workflow file.

Comparison is over the encoded state with the two fields a *write* stamps — `serial` and
`updated_at` — set aside, because a faithful copy necessarily advances both. Nothing else is set
aside, so a genuine difference in what is managed is still a conflict.

**Any failure leaves the source authoritative.** Nothing is removed from it, so a migration that
dies halfway has cost you nothing but a partially filled destination.

### `migrate_from:` is inert everywhere else, so leaving it is harmless

No command but `state migrate` acts on the block. That matters because a migration performed
through CI is necessarily **two commits** — one adding `migrate_from:`, one removing it — and
between them the block sits in committed configuration:

1. Commit the `backend:` block for the new backend and the `migrate_from:` block for the old
   one. Every ordinary command still refuses to run (see below), because the migration has not
   happened yet.
2. Run `infrena state migrate`, from CI or from a laptop.
3. Commit the removal of `migrate_from:` whenever it suits you. Everything works in the
   meantime; the block is inert once the destination holds state.

If step 3 lagged and an ordinary command acted on the block, a *successful* migration would
break every `plan` until somebody pushed the removal. That is why it is inert.

### The one thing ordinary commands do consult it for

There is exactly one exception, and it is a **guard, not an action**: while the destination is
empty and the source holds state, `plan`, `apply`, `refresh` and the rest **refuse to run**.

| `migrate_from:` | Destination | Source | What ordinary commands do |
| --- | --- | --- | --- |
| absent | — | — | **proceed** — this is every project |
| present | holds state | not opened | **proceed**: the migration is done, the block is inert |
| present | empty | holds state | **REFUSE**, naming the environments and telling you to run `infrena state migrate` |
| present | empty | empty | **proceed**: a new project carrying both blocks is not a pending migration, and refusing would block a legitimate first apply |

Without that guard, the window between the configuration landing and the migration being run is
a window in which a command reading only `backend:` finds an empty backend, sees **every
resource as unmanaged**, and an apply **creates all of it a second time** alongside what already
exists — duplicate infrastructure, and two backends each claiming to record the same resources.
In a pipeline that ordering is the likely one: the configuration lands first and the job runs
before a human triggers anything.

**A command never performs the migration.** Moving state as a side effect of a `plan` would be a
worse surprise than the one being prevented. The only two outcomes are "carry on" and "stop, and
here is what to run".

**The destination is checked first**, and it is the backend the command has opened anyway. State
there means the migration is done and the source is **never opened at all** — which matters,
because opening it may start a plugin process and reach a network. Only an empty destination
pays for a second open, and an empty destination is already the unusual case.

### `--check`, for pipelines

`infrena state migrate --check` runs the same three-way comparison — the same function, not a
read-only re-implementation of it — and **writes nothing and takes no lock**. A check that
migrated would be the worst surprise a pipeline can hold: the thing you ran to find out whether
to act would have acted. A check that took a lock would block the migration it just recommended.

**The exit code is the answer**, so a script branches without parsing anything:

| Code | Meaning |
| --- | --- |
| `0` | no migration needed — there is no `migrate_from:` block |
| `2` | **pending** — the source holds state the destination does not |
| `3` | already complete — both ends agree, and the block is stale |
| `4` | the two ends **differ** — a person has to decide |
| `1` | error — a backend unreachable, configuration unreadable |

**2 is deliberately `plan`'s code**, not a collision: both mean the same thing to a pipeline —
something is pending, run the matching command. 4 is distinct from 1 for the same reason 77 is
distinct from 1: a pipeline that cannot tell "this failed" from "this needs a person" treats
both the same, and they want opposite responses.

```bash
infrena state migrate --check
case $? in
  0|3) ;;                        # nothing to do
  2)   infrena state migrate ;;  # pending, and safe to run
  4)   exit 1 ;;                 # stop: both ends hold different state
esac
```

With `--output`, stdout stays empty and the `result` line carries the status **as a string** —
`"none"`, `"pending"`, `"complete"`, `"conflict"` or `"error"` — so a consumer never maps a
number back to a meaning.

### What a round trip proves

The migration is tested local → S3 → local, in `tests/integration`, because either direction
alone only proves a backend can read its own writing. The state that comes home is compared
against the state that left.

**A literally byte-identical round trip is impossible**, and the test says so rather than
pretending otherwise: `internal/state/local.go` increments `serial` and restamps `updated_at` on
every `Put`, so state that has been written three times cannot carry the numbers it started
with. The test therefore asserts two things, and the second is what makes the first mean
something:

1. the encoded state matches with `serial` and `updated_at` cleared, and
2. those two are the **only** top-level keys that changed.

That is stronger than a normalised comparison on its own, because normalisation can hide a
change in a field it also touches. Every resource, every attribute and every key ordering has to
cross unchanged.

---

## The trust boundary, stated plainly

**State reaches a backend in cleartext.** Every attribute recorded in it, sensitive ones
included, is encoded and handed to the backend process as bytes. A backend plugin can read
every secret in your infrastructure.

**So can a provider plugin.** Values reach provider plugins in cleartext already — that is
what dispatching a create to a plugin *is* — so a backend is the same trust level as a
provider rather than a new kind of exposure. Naming it as new would be dishonest in the
other direction.

**The mitigation is the same one, and it is a choice rather than a mechanism:** you decide
which plugins to trust, and `plugins.lock` records what you decided, in a file that is
committed and shows up in a diff. Concretely:

- A source has to be trusted before anything is installed from it, and only a person at a
  terminal can grant that. Project configuration cannot.
- `plugins.lock` records the resolved version, the source, and a SHA-256 per platform. A
  backend is keyed there as `infrena-backend-<name>`, so it never shares an entry with a
  provider of the same name.
- **The host hashes a locked backend binary before launching it**, because after the
  process has started is after its code has run. A binary replaced on disk after it was
  installed is caught at the next command. A lock that cannot be read refuses every
  launch rather than being treated as absent.
- A backend the lock does not mention still loads, exactly as a hand-placed provider does.
  The lock governs what install put there; it does not claim authority over everything.

What that does **not** protect against is a malicious publisher, who would simply publish
matching checksums. What guards against that is the trust decision and the reviewable
lockfile, and nothing else. Signing is out of scope and is recorded as out of scope rather
than implied.

### What to do about it

**Turn on server-side encryption on the store.** It is the store's job, it is one setting,
and it covers the thing most people mean when they ask whether state is encrypted: state
at rest in a bucket somebody can list. Use a bucket policy that refuses unencrypted
writes, restrict who can read the bucket, and turn on object versioning — a store with
versioning gives you state history for free, and the engine does not need to know.

**Client-side encryption is deferred, with the reason named.** Encrypting state before it
leaves infrena needs a key story — where the key lives, how it rotates, who can read it,
and what happens the day it is lost — before it needs code. Shipping the code first would
be shipping the easy half of the problem and calling it a feature; a state file nobody can
decrypt is worse than a state file in a private bucket. This is the same judgement applied
to plugin signing.

---

## A backend stores bytes and does not interpret them

`state.State` is deliberately absent from `pkg/backend`. A backend is handed raw bytes and
hands them back; it never parses state. A backend that parsed state would be a second
reader of it, free to disagree with the first about what a state file means — and the two
disagreeing is a class of bug with no good outcome.

This is also why `Put` carries state as raw bytes rather than as JSON nested inside JSON.
It is called once per operation during an apply, and the state is already serialised by
then; a second full encode would be paid on every write for nothing.

---

## For a backend author

- `pkg/backend` — the interface, `Lock`, `ErrLocked`, `ErrNotLocked`. Everything you must
  be able to import.
- `pkg/backendproto` — the wire contract and its version. `backendproto.Version` is its
  own format version, independent of the provider protocol: a change to how state is
  stored must not force every provider to cut a release, and the reverse.
- `pkg/backendsdk` — `Serve`, the whole of your `main()`.
- **How you lock is entirely your business.** A conditional PUT where the store supports
  one, a DynamoDB table, a Redis key, a lock service. The engine cannot lock generic
  storage safely, which is exactly why it asks you to.
- **The host stamps the lock holder, not you.** Your process is a child of infrena's, so a
  PID you recorded would stop existing the moment the run ends — and `infrena state unlock`
  prints that PID for a human to go and check. The operation (`apply`, `destroy`,
  `refresh`) is not knowable inside your process at all.
- **Write your log to stderr.** stdout is the protocol; anything you print there corrupts
  the stream. The SDK points `os.Stdout` at stderr to stop the common accident.
