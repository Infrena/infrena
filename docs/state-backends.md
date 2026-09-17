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

**Local is the bootstrap and is built in.** It writes to `.infra/` and works before
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
  PID you recorded would stop existing the moment the run ends — and `infra state unlock`
  prints that PID for a human to go and check. The operation (`apply`, `destroy`,
  `refresh`) is not knowable inside your process at all.
- **Write your log to stderr.** stdout is the protocol; anything you print there corrupts
  the stream. The SDK points `os.Stdout` at stderr to stop the common accident.
