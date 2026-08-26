# Security policy

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Use GitHub's private vulnerability reporting on this repository: **Security** tab
→ **Report a vulnerability**. That opens a private advisory and gives us one place
to ship a fix and publish from.

If that is unavailable to you, open a public issue containing only the words
"security report, requesting a private channel" — no details — and you will get
one.

Include what an attacker can do that they should not be able to do, the version
(`codeowners-tool version`) and how it was installed, and a reproduction: repo
shape, exact command, what happened, what should have.

Single-maintainer project, so response is best-effort rather than contractual:
acknowledgement within 5 working days, assessment within 10. Fixes ship in a
normal release with an advisory, crediting you unless you ask otherwise.

## Supported versions

Only the latest release. There are no maintained release branches and no
backports — fixes go onto `main` and into the next release.

`codeowners-tool version` reports the build, stamped at link time by the release
workflow. Anything answering `dev` did not come from a release.

## Scope

The failures that matter most here:

- **A write that leaves the repository.** The write path is contained to
  `--repo`, including through committed symlinks. An escape is a vulnerability.
- **A "wrong but confident" answer.** `sync` reporting `applied` for a file
  GitHub does not read, or `audit` calling an owner valid when it cannot be. The
  whole product is a claim that the change was proven correct.
- **A credential in an output** — stdout, stderr, a plan artifact, a summary, a
  log, or usage text.
- **Release integrity** — anything letting a build not produced by this
  repository's release workflow pass `install.sh`.

Known and deliberate: `--out`, `--summary-out` and `--plan` are trusted operator
paths, not contained to `--repo` — no repository can influence them. Auditing is
O(rules × files) and slow on very large monorepos, which is a scalability limit
rather than a security boundary.

## Verifying what you installed

`checksums.txt` proves only that the bytes arrived intact; the build-provenance
attestation proves origin. The commands, and how `install.sh` applies them
(`PROVENANCE=require` refuses to install when it cannot verify), are in
[docs/INSTALL.md](docs/INSTALL.md#verifying-a-download).
