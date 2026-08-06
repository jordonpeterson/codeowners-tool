# Security policy

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Report it through GitHub's private vulnerability reporting on this repository:

> **Security** tab → **Report a vulnerability**

That opens a private advisory visible only to the maintainers, and it is the
preferred channel because it needs no shared mailbox and gives us a place to
issue a fix and an advisory from the same thread.

If private reporting is unavailable to you for any reason, open a public issue
containing **only** the words "security report, requesting a private channel" and
no details, and you will get a private channel to send them through.

### What to include

- What an attacker can do that they should not be able to do.
- The version — `codeowners-tool version` — and how it was installed.
- A reproduction: the repository shape, the exact command, what happened, and
  what should have happened.

### What to expect

This is a single-maintainer project, so response times are best-effort rather
than contractual: acknowledgement within 5 working days, an assessment within 10.
Fixes ship in a normal release with a GitHub Security Advisory. Credit is given
in the advisory unless you ask otherwise.

## Supported versions

| Version | Supported |
|---|---|
| latest release | yes |
| anything older | no — upgrade first |

There are no maintained release branches. Fixes go onto `main` and into the next
release; there are no backports. Use `codeowners-tool version` to check what a
binary actually is — it is stamped at link time by the release workflow, and any
build that answers `dev` did not come from a release.

## What is in scope

This tool writes CODEOWNERS files and reads the GitHub API. The failures that
matter most, in the maintainers' view:

- **A write that leaves the repository.** Everything about the write path is
  contained to `--repo`, including through committed symlinks. An escape is a
  vulnerability, not a bug.
- **A "wrong but confident" answer.** `sync` reporting `applied` for a file
  GitHub does not read, or `audit` reporting an owner as valid when it cannot
  be, are treated as severe: the entire product is a claim that the change was
  proven correct.
- **A credential in an output.** Tokens must never reach stdout, stderr, a plan
  artifact, a summary, or a log — including through usage text and flag-parse
  errors.
- **Release integrity.** Anything that lets a build not produced by this
  repository's release workflow pass `install.sh`.

Known and deliberate:

- `--out`, `--summary-out` and `--plan` paths are **trusted operator input** and
  are not contained to `--repo`. They are typed on the same command line as the
  shell redirection they replace. A repository cannot influence them.
- Auditing is O(rules × files) and slow on very large monorepos. That is a
  scalability limit, not a security boundary.

## Verifying what you installed

Every release archive carries a build-provenance attestation signed by this
repository's release workflow. `checksums.txt` proves only that the bytes arrived
intact — it ships on the same release, from the same host:

```sh
gh attestation verify <archive> \
  --repo jordonpeterson/codeowners-tool \
  --signer-workflow jordonpeterson/codeowners-tool/.github/workflows/release.yml
```

`install.sh` runs this automatically when `gh` is available; set
`PROVENANCE=require` to refuse to install when it cannot.
