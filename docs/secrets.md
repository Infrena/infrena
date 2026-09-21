# Secrets

A database password has to reach the provider, and it must not reach the terminal, the
report, or a pull request diff. Infrena handles that with one reference namespace and two
places it can be answered from: the process environment, or an encrypted file you commit.

## The reference

Write `${secret.NAME}` anywhere a value goes:

```yaml
resources:
  db:
    type: aws.rds
    engine: postgres
    password: ${secret.DATABASE_PASSWORD}
```

It composes, because most credentials arrive as part of a longer string:

```yaml
    database_url: postgres://app:${secret.DATABASE_PASSWORD}@db.internal/app
```

`${secret.}` sits alongside `${var.}` deliberately. It is the same grammar in the same
position; there is nothing extra to learn beyond the word.

## Where the value comes from

Highest first:

1. **The process environment** — `DATABASE_PASSWORD` in the environment infrena was started with.
2. **`secrets/<environment>.yml`** — that environment's vault, if there is one.
3. **`secrets.yml`** — the vault every environment shares.

The environment wins, which is the same direction the rest of the ladder runs: `--var` beats
a variables file, and an explicit thing beats a committed one. It means CI can inject a
rotated credential without anybody editing and re-encrypting a file, and it means a pipeline
can run with no vault passphrase at all.

Be aware of the cost, because it is real and nothing warns you about it: a leftover variable
in somebody's shell silently shadows the committed one. That is the same trade `--var`
already makes.

## Unset is an error, and so is empty

```
Error: secret "DATABASE_PASSWORD" is not set
  at infrena.yml:6:5
```

The run stops at plan time rather than at the provider. Empty is refused alongside missing
because empty is the shape a CI failure actually takes: a repository secret that was never
created expands to an empty string rather than disappearing, and an empty password does not
fail until after somebody has approved the run.

## What redaction actually covers

A secret is sensitive **because of where it came from**, not because a provider marked the
attribute. Schema-declared sensitivity describes an attribute; this describes a value. Put a
credential somewhere nobody expected and it is still redacted:

```
  ~ password: <sensitive>
```

That holds for the plan on your terminal, for `--output` reports, and for anything derived
from a secret — a connection string containing one is sensitive in full, and a
[template](templates.md) given one renders entirely as `<sensitive>`.

**Two places hold the real value in clear, and you should know about both.**

- **State.** `.infrena/state/<environment>.json` records what was actually applied, which
  includes the password that was set. Written mode `0600`. If state lives in a bucket, that
  bucket holds credentials — see [state backends](state-backends.md).
- **A saved plan.** `plan --output` writes the values `apply --plan` needs to apply them.
  Also `0600`. It is a file to treat like a credential, not an artifact to attach to a
  ticket.

Neither is an oversight. An applier needs the real value; redaction is about what is
*displayed*, and the boundary is drawn at display rather than at storage because storage is
where the reconciler does its job.

## The vault

Reading secrets from the environment is the whole answer for CI. On a laptop it only moves
the problem, because "where do I keep this file" usually resolves to a password in a repo in
clear. `infrena vault` is the answer to that: an encrypted file you can commit beside the
configuration that references it.

```bash
infrena vault create secrets.yml     # new vault, opens in $EDITOR
infrena vault edit secrets.yml       # decrypt into $EDITOR, re-encrypt on save
infrena vault view secrets.yml       # print, write nothing
infrena vault encrypt secrets.yml    # seal a plain file in place
infrena vault decrypt secrets.yml    # unseal it in place
infrena vault rekey secrets.yml      # change the passphrase
```

Every one of those takes a file path, resolved against the **project**, not against the
directory your shell happens to be in. With `--chdir` that distinction matters: without it,
`vault edit secrets.yml` would happily edit a file nothing loads and tell you it worked.

The contents are plain YAML, one secret per key:

```yaml
DATABASE_PASSWORD: hunter2
API_KEY: abc123
```

Those names are the same names `${secret.NAME}` asks for. A vault is not a separate
namespace; it is a second place the same question gets answered.

### Per-environment vaults

`secrets/` takes the same shape `vars/` does. `secrets/production.yml` belongs to production
and beats the shared `secrets.yml` for any name both define, so the common arrangement is
shared defaults in one file and the real production credentials in another, encrypted under a
passphrase fewer people hold.

```
myapp/
├── infrena.yml
├── secrets.yml              # shared
└── secrets/
    ├── staging.yml
    └── production.yml
```

### The passphrase

Three sources, in order:

1. `--vault-password-file <path>`
2. `INFRENA_VAULT_PASSWORD` in the environment
3. A prompt, with no echo

Creating a vault asks twice, because a typo while sealing produces a file nobody can open.

A project with **no** vault never asks for a passphrase, and a project with one opens it
lazily — a command that references no secret does not need it either. If you supply
everything through the environment, the feature is invisible.

### If the passphrase is wrong

```
Error: the vault would not open: wrong passphrase, or the file has been changed
```

The two are deliberately not distinguished. Telling them apart would tell someone which of
the two they had achieved.

### What the file looks like

```
$INFRENA_VAULT;1;AES256-GCM;PBKDF2-SHA256;600000
yJPn499OKP3LyL1pVmBwdw==
isOzrfWKKoq3vJC9
M1P6VIaX6Uu2rWQPux2ad9LjT9Un2hYaoDeoqfoEhZ/NjF2EEE7Do8U=
```

Four lines: the header, then the salt, the nonce and the ciphertext, each base64.

AES-256-GCM, with the key derived by PBKDF2-SHA256 at 600,000 iterations. The header line is
authenticated along with the contents, so editing the iteration count down in the file does
not produce a file that opens faster — it produces one that does not open. Greppable on its
first line, so a pre-commit hook can tell a sealed file from an unsealed one.

## What the vault is not

It is not a secret manager. It rotates nothing, controls nobody's access, and records no
reads. It is what you use when you do not have such a store.

**Ciphertext in git is permanent.** If the repository is ever published, every historical
version of the file goes with it, and a passphrase compromised later opens all of them. If
that happens, rotate the secrets themselves, not just the passphrase.

Integration with a real secret store — AWS Secrets Manager, Vault, 1Password — is a platform
feature rather than a CLI one; [docs/open-core.md](open-core.md) records where that line sits.
The CLI deliberately stores and transports nothing: it reads what the process was already
given, or a file you chose to encrypt yourself.

## Using secrets in CI

[docs/ci.md](ci.md) has the full picture. The short version: set them as environment
variables in the pipeline, don't put a vault passphrase in the same place as the vault, and
remember that `--output` reports are safe to publish while saved plans are not.
