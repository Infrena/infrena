# Phase 4 — Remote state

**Date:** 2026-09-16
**Status:** draft, awaiting review
**Amends:** `PLAN.md` §52 (which is six bullets and no design), §31.3 (the naming convention extends to backends), §61 (a seventh format version)
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

**Correction to an earlier draft of this spec, which described the interface as new.**
`state.Backend` ALREADY EXISTS in `internal/state/backend.go`, carrying `Get`, `Put`, `Lock` and
`Unlock` — and its doc comment already anticipates this phase, saying the lock contract binds
"any future implementation — an S3 backend in Phase 4, for instance". `internal/executor` already
depends on the interface rather than the concrete type.

So the work is narrower than "extract an interface". It is:

- **Widen** `Backend` by the three methods the CLI calls on the concrete type and the interface
  does not carry: `List`, `Inspect`, `ForceUnlock`.
- **Re-point** `backendFor`, which returns `*state.Local` today (`internal/cli/context.go:294`)
  and must return `state.Backend`.
- **Move** the public types out to `pkg/backend`, since a third-party author must be able to
  import them.

The resulting interface, every method of which exists on `*state.Local` today:

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

`List` was added 2026-09-15 for discovery and never reached the interface, which is why
`backendFor` still hands back a concrete type. `backendFor(dir)` — called from eight places in `internal/cli` —
becomes the single place that decides local or plugin.

Two signatures normalise on the way: `Inspect` and `ForceUnlock` are synchronous on `*Local` and
gain a `context.Context`, because over a wire they can block and must be cancellable. That is a
change to existing callers, all of them in `internal/cli`, and it is mechanical.

### Where the public surface lives

Backends are third-party code, so they need the same public footing providers have. Mirroring
the existing split exactly, rather than inventing a second shape:

| Providers | Backends | Holds |
| --- | --- | --- |
| `pkg/pluginproto` | `pkg/backendproto` | the wire contract and its version |
| `pkg/pluginsdk` | `pkg/backendsdk` | the whole of a backend's `main()` |
| `pkg/provider` | `pkg/backend` | the interface and its types |
| `internal/pluginhost` | `internal/backendhost` | the host side |

**`state.Lock`, `state.ErrLocked` and `state.ErrNotLocked` must move to `pkg/backend`.** It is currently in `internal/state`, and a backend
author cannot implement locking against a type they cannot import. Its fields — user, host, PID,
operation, timestamp — become part of the public contract, which is the right outcome: they are
what the stale-lock diagnostic prints, so they were already a user-facing shape.

**`state.State` does NOT move, and that is the point of §6.** A backend stores bytes and never
parses them, so `pkg/backend` needs no state type at all. The SDK surface stays genuinely small:
seven methods and a lock struct.

**LOCAL IS BUILT IN AND IS NOT A BACKEND PLUGIN.** It is the bootstrap: it works before anything
is installed, the way `init` works with no provider. Every REMOTE backend is a plugin, and a
shipped infrena carries none — which is what keeps the plugin path the only path and therefore
incapable of rotting.

---

## 3. Configuration, and one constraint that falls out of the architecture

```yaml
backend:
  plugin: s3
  bucket: acme-infra
  profile: platform-infra
  path: /infrena/
  endpoint: https://nyc3.digitaloceanspaces.com
```

Absent, state is local, exactly as today.

**The shape mirrors `providers:`**, which already names a plugin with `plugin:` and carries its
configuration in sibling keys. One shape to learn rather than two. **`plugin:` is the only
reserved key; every other key is passed to the backend untouched**, so the engine never needs to
know what a bucket is.

A `profile:` is a POINTER to a credential, not a credential. Naming which profile to use is
configuration and belongs here; an access key is a secret and does not. The plugin resolves the
pointer however it likes — a credentials file, an instance profile, the environment.

### `backend:` may not interpolate variables. At all.

`providers:` may interpolate because compiler stage 4.5 constructs provider instances after
variables resolve. **State cannot, because you need state before you can compile, and compiling
is what resolves variables.** Supporting `${var.bucket}` would be a cycle, not a feature.

This is stated here explicitly because it will look like an inconsistency to the next reader, and
because the fix for "why can't I use a variable here" must be a diagnostic that says *why*,
not a feature request that gets accepted.

Note the existing precedent this mirrors: `destroy`, `refresh`, `discover` and `import` already
read `providers:` for LITERAL values only, and refuse an instance whose configuration
interpolates, because those commands never compile. `backend:` is that rule taken to its
conclusion — it is read by *every* command, including the ones that compile, so it is literal
always.

**Credentials never appear in this block.** They are the plugin's business: environment
variables, an instance profile, a credentials file, whatever it chooses. A bucket name is
configuration; a secret key is not.

---

## 4. A backend is found exactly the way a provider is

**A backend lives in `infrena-backend-<name>` and ships `infrena-backend-<name>`**, the same way
a provider lives in `infrena-provider-<name>` and ships `infrena-plugin-<name>`. The naming
convention already IS the registry (§31.3); this extends it rather than adding anything.

**No `kind:` field in `plugin.yaml`.** An earlier draft proposed one, arguing a naming convention
would mean "two search paths, two install paths and two lock files". **That was wrong**, checked
against what §31.3 actually shipped: `pluginhost.DefaultSearch` returns the same directories for
both, `plugins install` writes to the same place, and `plugins.lock` is one file. The only thing
that differs is a name pattern.

The convention is also STRICTLY BETTER than a manifest field for search. An owner listing can
filter `infrena-backend-*` locally, from the one call that lists repositories, without fetching a
manifest per candidate to discover what each one is. Unauthenticated GitHub allows sixty requests
an hour and §31.3 spends real design effort on not wasting them; a `kind` field would spend one
per candidate to answer a question the repository name already answers for free.

It also resolves a collision the field could not: a provider named `s3` and a backend named `s3`
are different repositories under this convention, and indistinguishable search results under a
manifest field.

**There is no bootstrap cycle**, and the reason is worth stating: decoding `infra.yml` requires
no state. So the sequence is read the file, learn the backend name, load the plugin, get state.
`plugins install` writes `plugins.lock` at the project root and reads no state, so installing a
backend never requires the backend.

`.infra/` remains the local working directory for plugins, cache and locks regardless of where
state lives. Remote state does not make `.infra/` go away.

---

## 5. The protocol, and the demand it makes

A second wire protocol in `pkg/backendproto`, sharing `pluginproto`'s transport — newline-delimited
JSON over stdio, the handshake, the cookie — with its own message set and **its own version
number**, `backendproto.Version`, per §61's rule that each boundary carries exactly one. It joins
that section's table as the seventh format. It is not a new version of the provider protocol: the two
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

**Redesigned 2026-09-17.** The original `infrena state migrate --to s3` named a PLUGIN, not a
configuration — and you cannot migrate to an S3 backend without knowing its bucket, region and
path. A migration needs both ends fully configured; `backend:` holds one.

A second attempt made the other end always local, migrating remote-to-remote by pivoting through
local. **James rejected it, correctly**: moving from an S3 bucket to the hosted platform's own
backend is a path that should be one command, and that design made it a three-step detour. It also
assumes a viable local disk, which CI may not have and large state may not fit.

### A temporary second block

```yaml
backend:
  plugin: s3
  bucket: new-bucket
  region: us-east-1

migrate_from:
  plugin: s3
  bucket: old-bucket
  region: eu-west-1
```

`migrate_from:` takes the same shape as `backend:` and the same rules: `plugin:` is the only
reserved key, every other key crosses to the backend untouched, and **no interpolation**, for the
identical ordering reason (§3).

**`plugin: local` is valid in both blocks**, so every migration names both ends and nothing is
implicit. Uniform across every pair — local to remote, remote to local, S3 to S3, S3 to whatever
backend exists in two years. No pivot and no special case.

Rejected: a pointer recording where state last lived. It solves remote-to-remote, but it lives in
a gitignored directory, so a colleague's checkout disagrees about where state was and a stale
pointer produces a confident migration from somewhere holding nothing. Also rejected: `state pull`
and `state push`, which need less machinery but write **state to a file in cleartext** — and state
is the one place secrets are not redacted, so that is a file someone forgets to delete.

### `migrate_from:` IS INERT FOR EVERY COMMAND EXCEPT `state migrate`

This is the rule the CI case forces, and it is not an optimisation.

Where a user cannot reach the cloud directly, a migration is necessarily TWO commits: push the
config carrying `migrate_from:`, let the pipeline migrate, then push again to remove it. Between
those commits — possibly hours — the block sits in committed configuration. If it affected
ordinary commands, every `plan` and `apply` in that window would find state in the new backend and
fail. **A successful migration would break the pipeline until somebody tidied up.**

So `plan`, `apply`, `refresh`, `destroy` and the rest do not ACT on it. Leaving it behind is
harmless, which also makes the second commit hygiene rather than a fix.

### But ignoring it entirely recreates your infrastructure

**Found by James, 2026-09-17, and it is the dangerous half of this design.**

An earlier draft said ordinary commands ignore `migrate_from:` completely. Consider what that
means in the window this design creates:

Configuration lands carrying `backend:` (the new backend, **empty**) and `migrate_from:` (the old
one, holding all your state). Nobody has triggered the migration yet. CI runs `plan` on the pull
request, or `apply` on merge.

Reading `backend:` alone, the new backend is empty. **Every resource looks unmanaged, and apply
creates all of it again** — duplicate infrastructure, and two backends each claiming to record the
same resources. In the CI flow this design exists to support, that ordering is the LIKELY one:
configuration lands first and the pipeline runs before a human triggers anything.

So ordinary commands consult `migrate_from:` for exactly one thing, and it is a GUARD rather than
an action:

| `migrate_from:` | Destination | Source | Ordinary commands |
| --- | --- | --- | --- |
| absent | — | — | proceed |
| present | has state | — | proceed — migration done, the block is inert |
| present | empty | empty | proceed — nothing to migrate |
| present | **empty** | **has state** | **REFUSE**: migrate first, or remove the block |

**A command never performs the migration.** Migration is always deliberate, always
`state migrate`. This only stops a run that would otherwise recreate everything.

The check is cheap where it matters: look at the DESTINATION first, which the command opens
anyway. State there means the migration is done and the source is never touched. Only an empty
destination costs a second open, and an empty destination is already the unusual case.

### What `infrena state migrate` does

Lock both ends, then decide on THREE cases rather than two:

| Source | Destination | Result |
| --- | --- | --- |
| has state | empty | migrate |
| has state | **identical state** | **succeed as a no-op** — "already migrated" |
| has state | **different state** | **refuse**, `--force` to overwrite |

**The middle row exists because CI re-runs happen** — a flaky job, a retry, a pipeline that runs
twice. Failing there would be safe but noisy, and the natural response to a red pipeline is to add
`--force` to the workflow, where it then sits on every future run. That is the same hazard as a
`lock: false` escape hatch, arriving by the same route, so the refusal message must NOT lead with
`--force`.

The last row genuinely deserves refusal: differing state at both ends means somebody has been
applying to one of them, and silently choosing a winner destroys real work.

Then copy every environment, verify each by reading it back, and release both. **A failure at any
point leaves the source authoritative** and the destination incomplete rather than the reverse —
the same "leave the harmless half done" rule that orders configuration before state in `import`.

**MIGRATION COPIES. It never empties the source** (James, 2026-09-17: "If user encounters a bug in
the migration process they can revert back to the old state"). A failed migration therefore cannot
lose anything, and a successful one leaves a fallback while the new backend is verified in anger.
Emptying the old store is a deliberate act the user performs later, not this command's to make —
and the documentation must say that the old state lingers, secrets included, until they do.

Report the outcome and suggest removing `migrate_from:` — adding that leaving it is harmless, since
in CI that message goes to a log nobody reads.

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
- **Variable interpolation in `backend:`.** §3 — a cycle, not a feature.
- **Azure.** It is not S3-compatible and needs its own plugin, which is someone's project and
  not this design's.
- **State history or time travel.** A store with object versioning gives it for free; the engine
  does not need to know.
- **Locking that the engine implements on a backend's behalf.** If the engine could lock generic
  storage safely it would not need the backend to.
