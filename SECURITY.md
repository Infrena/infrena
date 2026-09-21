# Security policy

## Reporting a vulnerability

Report it privately through GitHub, using
[**Report a vulnerability**](https://github.com/infrena/infrena/security/advisories/new)
on this repository's Security tab. That opens a private advisory visible only to you and
the maintainers.

Please do not open a public issue for a security problem, and please do not disclose it
publicly until a fix is released.

Include what you would want if you were fixing it: the version (`infrena version`), what
you did, what happened, and what you expected. A minimal configuration that reproduces the
problem is worth more than a description of one.

You should get a first response within a few days. This is a small project, so that is a
person answering rather than a process.

## Supported versions

Infrena is pre-1.0 and moves quickly. Only the most recent release is supported. If you
are on an older one, upgrade before reporting — the problem may already be fixed.

## What is in scope

Infrena runs with credentials to your infrastructure, so most of its surface is
security-relevant. In particular:

- **Plugin installation and verification.** Plugins are downloaded binaries. `plugins.lock`
  records a checksum per platform and installation is refused when it does not match.
  Anything that lets an unverified binary run, or that lets the lock be bypassed, is a
  vulnerability.
- **Secret handling.** Values marked sensitive must not reach logs, plans, reports or
  diagnostics. A leak through any of those is a vulnerability.
- **The vault.** Encrypted secrets files, and anything that weakens or bypasses them.
- **Credentials.** Anything that causes credentials to be written to disk, logged, or sent
  to a provider they were not meant for.
- **State handling.** Anything that lets one environment's state be read or written while
  operating on another.

## What is known, and not a vulnerability

These are documented design positions rather than oversights. Reporting them is welcome as
a discussion, but they are not treated as vulnerabilities.

- **State reaches a backend in cleartext**, including sensitive attributes. Client-side
  state encryption is not implemented. Protect the bucket. See
  [docs/state-backends.md](docs/state-backends.md).
- **Values reach provider plugins in cleartext.** A provider cannot create a resource from
  a value it cannot read.
- **A plugin you install runs as you do.** Verification proves a binary is the one the lock
  recorded, not that it is trustworthy. Install plugins from sources you trust.

## Scope of this policy

This repository, and the plugin and backend repositories published under the same
organisation. If you are unsure which one a problem belongs to, report it here and it will
be routed.
