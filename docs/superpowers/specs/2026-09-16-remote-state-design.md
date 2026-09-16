# Phase 4 — Remote state

**Date:** 2026-09-16
**Status:** draft, awaiting review
**Amends:** `PLAN.md` §52 (which is six bullets and no design), §31.2 (`plugin.yaml` gains `kind`), §61 (a seventh format version)
**Raises:** a new backend protocol, versioned independently
**Depends on:** §31.3, shipped 2026-09-16 — `plugins install` is how a backend gets onto a machine

---

## 1. Why, and what changed in the asking

`PLAN.md` §52 lists six items — S3 backend, locking, state versioning, state migration,
encryption, concurrent testing — and no design at all. This is that design.

Two decisions from James reshaped it, and both are recorded here because the reasoning is not
recoverable from the result:

**"S3 should mean anything with an S3-compatible API."** DigitalOcean Spaces, Cloudflare R2,
Backblaze B2, Wasabi, Linode, MinIO and Ceph all speak it, and Google Cloud Storage has an
S3-compatible XML API usable with HMAC keys. **Azure Blob is the real exception** — it has its
own REST API and is not S3-compatible. So the realistic landscape is *one S3-compatible backend
covering nearly the whole market, plus an Azure one*, not one per cloud.

**"Backends are plugins, including S3."** This overturned the author's first recommendation, and
the argument that settled it is worth keeping: **a code path the default does not exercise
rots.** This project has hit that repeatedly — a constraint checked in three of four loaders, a
per-provider concurrency bound unreachable by construction, a refresh hook two of three callers
passed `nil` to. If S3 were built in and the backend protocol were used only by third parties,
the plugin path would be second-class and subtly broken by the time anyone needed it. Providers
avoid this because **a shipped infrena carries no provider**; the same rule now applies here.

The counter-argument was that a process boundary adds a failure mode to the per-operation state
write, which `internal/executor/apply.go` already wraps in `context.WithoutCancel` because
durability there outranks cancellation. That objection was overstated: the engine already
handles a *provider* plugin dying mid-apply, which is the same severity. It is a second instance
of a designed-for failure, not a new class.

---

## 2. The boundary

`state.Local` becomes one implementation of an interface carrying exactly what the code already
calls:

```go
type Backend interface {
	Get(ctx context.Context, environment string) (*State, error)
	Put(ctx context.Context, environment string, s *State) error
	List() ([]string, error)
	Lock(ctx context.Context, environment string) (Lock, error)
	Unlock(ctx context.Context, environment string) error
	Inspect(environment string) (Lock, bool, error)
	ForceUnlock(environment string) error
}
```

Nothing is invented: every method exists on `*state.Local` today, and `List` was added
2026-09-15 for discovery. `backendFor(dir)` — called from eight places in `internal/cli` —
becomes the single place that decides local or plugin.

**LOCAL IS BUILT IN AND IS NOT A BACKEND PLUGIN.** It is the bootstrap: it works before anything
is installed, the way `init` works with no provider. Every REMOTE backend is a plugin, and a
shipped infrena carries none — which is what keeps the plugin path the only path and therefore
incapable of rotting.

---

## 3. Configuration, and one constraint that falls out of the architecture

```yaml
state:
  backend: s3
  config:
    bucket: acme-infra
    prefix: projects/platform
    endpoint: https://nyc3.digitaloceanspaces.com
    region: us-east-1
```

Absent, state is local, exactly as today.

### `state:` may not interpolate variables. At all.

`providers:` may interpolate because compiler stage 4.5 constructs provider instances after
variables resolve. **State cannot, because you need state before you can compile, and compiling
is what resolves variables.** Supporting `${var.bucket}` would be a cycle, not a feature.

This is stated here explicitly because it will look like an inconsistency to the next reader, and
because the fix for "why can't I use a variable here" must be a diagnostic that says *why*,
not a feature request that gets accepted.

Note the existing precedent this mirrors: `destroy`, `refresh`, `discover` and `import` already
read `providers:` for LITERAL values only, and refuse an instance whose configuration
interpolates, because those commands never compile. `state:` is that rule taken to its
conclusion — it is read by *every* command, including the ones that compile, so it is literal
always.

**Credentials never appear in this block.** They are the plugin's business: environment
variables, an instance profile, a credentials file, whatever it chooses. A bucket name is
configuration; a secret key is not.

---

## 4. A backend is found exactly the way a provider is

`plugin.yaml` gains **`kind: provider | backend`**, defaulting to `provider` so every existing
manifest stays valid.

Considered and rejected: a second naming convention (`infrena-backend-<name>` repositories and
binaries). It would mean two search paths, two install paths and two lock files for one concept.
The manifest already exists to describe what a thing is, so it describes this too. `plugins
list` and `plugins search` gain a kind column rather than a parallel mechanism.

**There is no bootstrap cycle**, and the reason is worth stating: decoding `infra.yml` requires
no state. So the sequence is read the file, learn the backend name, load the plugin, get state.
`plugins install` writes `plugins.lock` at the project root and reads no state, so installing a
backend never requires the backend.

`.infra/` remains the local working directory for plugins, cache and locks regardless of where
state lives. Remote state does not make `.infra/` go away.

---

## 5. The protocol, and the demand it makes

A second wire protocol sharing `pluginproto`'s transport — newline-delimited JSON over stdio, the
handshake, the cookie — with its own message set and **its own version number**, per §61's rule
that each boundary carries exactly one. It is not a new version of the provider protocol: the two
evolve for unrelated reasons and one shared number would mean a state format change forcing a
provider release.

### Every backend must lock

**A backend that does not implement locking is refused when it loads**, not when an apply
reaches for it. Invariant 5 — two applies cannot mutate one environment concurrently — is
absolute, and a backend that cannot enforce it is not a backend.

**How it locks is entirely the plugin's business.** Conditional PUT (`If-None-Match: *`) where
the store supports it, a DynamoDB table, a Redis key, a lock service. This is the real payoff of
making backends plugins: "not every S3-compatible store supports conditional writes" stops being
the engine's problem and becomes one plugin's problem, where the knowledge belongs — the same
argument that puts API knowledge in a provider.

The lock carries what the local lock already carries — user, host, PID, operation, timestamp —
so `ForceUnlock` and the stale-lock diagnostic keep working with no change to their wording.

### The per-operation write

`internal/executor/apply.go` calls `Backend.Put` **once per operation**, and each call writes the
whole state. A fifty-resource apply is fifty whole-state writes. This is not new — the local
backend already serialises the whole state every time — but it now crosses a pipe.

**The envelope must not double-encode.** State is already JSON; carrying it as a nested JSON
object or a base64 string would cost a second full encode per operation. Carry it as raw bytes.

This is the one place to measure rather than assume, once the S3 plugin exists. It is called out
here so that measurement happens deliberately instead of being discovered as a performance
complaint.

---

## 6. State versioning

`state.CurrentVersion` travels unchanged. **A backend stores bytes and does not interpret them**
— it is storage, not a participant in the format. A backend that parsed state would be a second
reader free to disagree with the first.

`Serial` becomes the optimistic-concurrency token a backend MAY check to detect a lost update.
Behind a mandatory lock it is belt-and-braces, and that is exactly its value: it is what catches
a lock implementation that is subtly wrong, in a third-party plugin the engine cannot audit.

---

## 7. Migration

```bash
infrena state migrate --to s3
```

Lock both sides, copy every environment, verify each by reading it back, then release both. A
failure at any point leaves the source authoritative and the destination incomplete rather than
the reverse — the same "leave the harmless half done" rule that orders configuration before state
in `import`.

**This lands last in the build order, necessarily**: there is nothing to migrate to until the S3
plugin exists. The test is a local → S3 → local round trip, which is a better test than either
direction alone because it proves the two backends agree about what state is, rather than proving
one of them can read its own writing.

---

## 8. Trust is documented, not encrypted

**State reaches the backend in cleartext.** A new `docs/state-backends.md` says so plainly, and
says the thing that makes it acceptable: **values already reach provider plugins in cleartext**,
so a backend is the same trust level as a provider, not a new kind of exposure. Server-side
encryption on the bucket is recommended and documented; it is the store's job.

Client-side encryption is DEFERRED, with the reason named rather than left implicit: it needs a
key story — where the key lives, how it rotates, what happens when it is lost — before it needs
code. The same judgement §31.3 applied to plugin signing.

This is a real property to state carefully. A backend plugin can read every secret in your
infrastructure. So can a provider plugin. The mitigation is the same one: you choose which
plugins to trust, and `plugins.lock` records what you chose.

---

## 9. Build order

1. **Interface + protocol + local.** `Backend` extracted, `backendFor` routing, the protocol
   defined, local behind the interface. Nothing user-visible changes; every existing test must
   still pass, which is the proof the extraction was faithful.
2. **The S3-compatible backend plugin.** Its own repository, `kind: backend`, locking via
   conditional PUT with a clear refusal where the store cannot.
3. **Migration.** `state migrate`, and the local → S3 → local round trip.
4. **Concurrency tests.** Two environments applying concurrently succeed; two applies to one
   environment cannot.

Each step is useful alone, and the protocol gets a real consumer at step 2 rather than shipping
with none.

---

## 10. Deliberately not in this design

- **Client-side encryption.** §8, deferred with a reason.
- **A backend built into the engine other than local.** The whole argument of §1.
- **Variable interpolation in `state:`.** §3 — a cycle, not a feature.
- **Azure.** It is not S3-compatible and needs its own plugin, which is someone's project and
  not this design's.
- **State history or time travel.** A store with object versioning gives it for free; the engine
  does not need to know.
- **Locking that the engine implements on a backend's behalf.** If the engine could lock generic
  storage safely it would not need the backend to.
