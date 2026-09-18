# Running Infrena in CI

Everything here works in any CI system. Nothing is specific to GitHub, and Infrena
integrates with no forge — it reads exit codes, environment variables and files, which is
all any pipeline has.

## The short version

```bash
infrena validate production                       # config errors, no providers contacted
infrena plan production --output plan.json        # exit 2 means there are changes
infrena apply production --plan plan.json --auto-approve
```

The middle step is the one that matters. **A saved plan is applied without recompiling and
is refused outright if state moved since it was made**, so what runs is what was reviewed.

## Exit codes

Branch on these. They are stable.

| Code | Meaning |
| --- | --- |
| `0` | Success, nothing to do |
| `1` | Error — configuration, a provider, a backend |
| `2` | Success, and there are changes (`plan`) or changes were applied (`apply`) |
| `77` | Changes need an approval this run cannot obtain |

`infrena state migrate --check` has its own set: `0` no migration needed, `2` migration
needed, `3` already complete, `4` source and destination disagree.

**2 is not an error.** A pipeline that treats a non-zero exit as failure will treat a plan
with changes as a broken build, which is the most common mistake made here:

```bash
set +e
infrena plan production --output plan.json
status=$?
set -e

case $status in
  0) echo "nothing to do" ;;
  2) echo "changes proposed" ;;
  *) exit "$status" ;;
esac
```

The `set +e` matters under `set -e`, which most CI systems enable: without it the shell
exits on the 2 before anything reads it.

## Secrets

Infrena reads secrets from the environment and nowhere else:

```yaml
password: ${secret.DB_PASSWORD}
database_url: postgres://app:${secret.DB_PASSWORD}@db.internal/app
```

Inject `DB_PASSWORD` the way your CI system injects any secret. Infrena never stores it,
never writes it to a file it manages, and redacts it everywhere it prints.

**Or commit them, encrypted.** `infrena vault` encrypts a `secrets.yml` that feeds the same
`${secret.NAME}` references, so a project with no secret manager can keep its secrets beside
its configuration:

```bash
infrena vault create secrets.yml     # opens $EDITOR, writes encrypted
infrena vault edit secrets.yml
```

CI then needs one secret instead of many — `INFRENA_VAULT_PASSWORD`, or
`--vault-password-file`. **The environment still wins**, so a rotated credential can be
injected for a single run without re-encrypting anything.

At a terminal you are prompted instead, without echo. In CI there is no terminal, so a
missing passphrase fails immediately rather than hanging on a prompt nobody can answer.

Two things to know before committing one. Ciphertext in git is permanent: if the repository
is ever published, every historical version goes with it, and a passphrase compromised later
opens all of them — so rotate the secrets, not just the passphrase. And `secrets.yml` must be
encrypted before it is committed; nothing stops you committing it in clear.

**An unset or empty secret fails the plan.** That is deliberate: an empty credential does
not fail at the plan, it fails at the provider, after somebody approved the run. An unset
repository secret expands to an empty string rather than disappearing, so empty is treated
the same as missing.

## Protected environments

An environment can require that a person approves changes:

```yaml
environments:
  production:
    require_approval: true
    prevent_destroy: true
```

With `require_approval`, **`--auto-approve` is refused with exit 77** — a protection a flag
can switch off is not one. Two approvals are accepted, and both involve a person: somebody
confirming at a terminal, or a saved plan.

That makes the CI shape a two-stage one, which is the point:

```bash
# On the pull request: produce the plan and publish it for review.
infrena plan production --output plan.json

# A human reads it. They need nothing installed:
infrena plan --show plan.json

# On merge: apply exactly what was reviewed.
infrena apply production --plan plan.json --auto-approve \
  --approved-by "$PR_URL"
```

`--approved-by` takes any text — a pull request URL, a ticket, a name — and records it in
the run's report. **It is a record, not a permission**: Infrena cannot tell a real URL from
an invented one, so nothing is allowed on the strength of it. It exists so a run can say
under whose authority it happened.

`prevent_destroy` refuses `destroy`, and refuses an apply that would destroy a resource
removed from configuration. It does not refuse a replacement — otherwise no immutable
attribute could ever be changed in a protected environment.

## Machine-readable output

`--output FILE` writes newline-delimited JSON and **silences stdout completely**, so a
frontend tails one file rather than scraping a terminal. Every command writes the same
shape: a `meta` line, then `event`, `observation` and `diagnostic` lines as work happens,
then a final `result` line.

```json
{"type":"meta","version":4,"infrena":"0.11.1","command":"apply","environment":"production","startedAt":"..."}
{"type":"event","event":"started","address":"db","op":"update","at":"..."}
{"type":"result","approved_by":"https://github.com/acme/infra/pull/42","state_serial":18,"applied":["db"]}
```

`infrena` names the build that produced the report and `state_serial` names the state
version the run produced, so an archived report can be placed without being taken on
trust. Sensitive values are redacted before they reach it.

Diagnostics go to stderr regardless, so a failing run still says why on a channel somebody
sees.

## Things worth knowing

**Plans go stale on purpose.** `apply --plan` reads state inside the lock and refuses the
plan if that is not the state it was made against. If two pipelines can apply to one
environment, the second will be refused rather than applying against a world that moved.

**`validate` contacts no providers**, so it is the cheap gate to run first. It does check
that the plugins and the state backend a project names are installed.

**Locking is per environment**, so pipelines for different environments do not block each
other. Two applies to the same environment do.

**Exit 141 cannot happen.** Infrena ignores SIGPIPE, so `infrena plan | head -1` still
reports the exit code the table above describes.
