# Security policy

Signal Lab is a local-first demo and reference project. It has **no authentication and no TLS** by
design, binds to `127.0.0.1`, and must not be exposed to a network (see the
[security notes in the README](README.md#security-notes-local-demo)).

## Reporting a vulnerability

If you find a security problem in the code itself (for example input handling that could be abused
even in a local setup, or a dependency issue), please report it privately using GitHub's
**Security -> Report a vulnerability** button on this repository rather than opening a public issue.
Include what you found, how to reproduce it, and the version or commit.

This is a hobby/educational project maintained on a best-effort basis: there is no service-level
agreement, but reports are appreciated and will be looked at.

## Supported versions

Only the latest release and the `main` branch.
