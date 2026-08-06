# Changelog

Notable changes to codeowners-tool. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are
[semantic](https://semver.org/), with `MAJOR.MINOR` set by hand in `VERSION` and
`PATCH` assigned automatically by the release workflow.

While the version is `0.x`, the CLI surface — flag names, JSON record fields, and
the exit-code contract — may change in a minor release. Exit codes are the part
scripts depend on; changes to which class a failure lands in are called out here
explicitly.

## [Unreleased]

### Security

- **`install.sh` now verifies build provenance, not just the checksum.** Every
  release archive already carried an attestation signed by this repository's
  release workflow, and nothing read it. `checksums.txt` ships on the same
  release, from the same host, over the same channel as the archive it covers, so
  it proves integrity in transit and nothing about origin. `gh` is deliberately
  **not** an install prerequisite: the default (`PROVENANCE=auto`) verifies when
  `gh` is present and signed in and warns loudly when it is not, `require`
  refuses to install without verification, and `skip` opts out. A verification
  that runs and fails always aborts.
- **The Homebrew job no longer holds `contents: write` on this repository.** It
  inherited the workflow-level permission because it declared none of its own —
  a job-level block replaces the workflow-level one rather than adding to it.
- **The tap token is no longer embedded in a clone URL.** It was written in
  cleartext to `.git/config` on the runner and would appear in any git error that
  echoed the remote. The job now authenticates through `gh`'s credential helper.
- **`plan` is contained to the repository.** It writes no CODEOWNERS, so reading
  through an escaping symlink was not an escape — but it produced a reviewable
  artifact whose `codeowners_path` pointed outside the clone, at a file GitHub
  never reads. Every downstream refusal fired after that review. `plan` now
  refuses (exit 2) and writes no artifact.
- **Refs that git would parse as its own options are rejected.**
  `--branch '--format=…'` reached git's option parser rather than its revision
  parser. A refname cannot begin with a dash, so the guard costs nothing.
- **GitHub API paths and queries are escaped.** Paths were built by
  concatenation, and the owner grammar permits a dot — so `@org/..` produced
  `GET /orgs/org/teams/..`, which any normalizing proxy rewrites to
  `GET /orgs/org`, answering 200 and reporting a nonexistent team as valid. A
  `ref` containing `&` appended query parameters nobody wrote, and one containing
  a space failed to form a URL at all.

### Added

- `SECURITY.md`, `CONTRIBUTING.md`, `CHANGELOG.md`, `.github/dependabot.yml`
  (GitHub Actions only — see the file for why there is no `gomod` entry), and a
  `CODEOWNERS` file for this repository, which previously failed its own A-11
  check.
- CI runs the differential fuzz against the vendored `hmarr/codeowners` oracle on
  every PR — the full 500k cases, not a reduced pass, in its own parallel job
  (about 35 seconds, so it never sits on the critical path).
- `difftest` takes an optional seed argument. With the seed fixed, raising the
  case count only extends one sequence; a different seed explores somewhere new.
  The seed is printed with the result so any failure replays exactly.

### Changed

- `--out`, `--summary-out` and `plan --out` are documented in their flag help as
  trusted operator paths: overwritten, and deliberately not contained to
  `--repo`. Unlike `--file` and the discovered CODEOWNERS path, no repository can
  influence them.

## Earlier releases

`v0.0.1` (2026-07-27) through `v0.0.5` (2026-08-04) predate this file. Their
contents are in the auto-generated notes on each
[GitHub release](https://github.com/jordonpeterson/codeowners-tool/releases).
