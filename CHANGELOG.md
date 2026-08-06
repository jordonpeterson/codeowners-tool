# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
are [semantic](https://semver.org/), with `MAJOR.MINOR` set by hand in `VERSION`
and `PATCH` assigned by the release workflow.

While the version is `0.x`, flag names, JSON record fields, and the exit-code
contract may change in a minor release. Exit codes are what scripts depend on, so
changes to which class a failure lands in are called out explicitly.

## [Unreleased]

### Security

- **`install.sh` verifies build provenance, not just the checksum.** Releases
  already carried an attestation and nothing read it. `checksums.txt` ships on the
  same release from the same host, so it proves integrity in transit and nothing
  about origin. `gh` is deliberately not a prerequisite: `PROVENANCE=auto`
  (default) verifies when it can and warns loudly when it cannot, `require`
  refuses to install without verification, `skip` opts out. A verification that
  runs and fails always aborts.
- **The Homebrew job no longer holds `contents: write`** on this repository. It
  inherited the permission by declaring none of its own — a job-level block
  replaces the workflow-level one rather than adding to it.
- **The tap token is no longer embedded in a clone URL**, where it was written in
  cleartext to `.git/config` on the runner and would appear in any git error
  echoing the remote. It now goes through `gh`'s credential helper.
- **`plan` is contained to the repository.** It writes nothing, so reading through
  an escaping symlink was not an escape — but it produced a reviewable artifact
  whose `codeowners_path` pointed outside the clone, at a file GitHub never reads,
  and every downstream refusal fired after that review. Now exits 2 with no
  artifact.
- **Refs git would parse as its own options are rejected.** `--branch '--format=…'`
  reached git's option parser. A refname cannot begin with a dash, so the guard
  costs nothing legitimate.
- **GitHub API paths and queries are escaped.** The owner grammar permits a dot,
  so `@org/..` produced `GET /orgs/org/teams/..`, which normalizes to
  `GET /orgs/org` — 200, and a nonexistent team reported as valid. A `ref`
  containing `&` appended parameters nobody wrote; one containing a space failed
  to form a URL at all.

### Added

- `SECURITY.md`, `CONTRIBUTING.md`, `CHANGELOG.md`, `.github/dependabot.yml`
  (GitHub Actions only — see the file), and a `CODEOWNERS` for this repository,
  which previously failed its own A-11 check.
- The differential fuzz against the vendored `hmarr/codeowners` oracle runs on
  every PR — the full 500k cases, in its own parallel job (~35s).
- `difftest` takes an optional seed; with it fixed, raising the case count only
  extends one sequence. Printed with the result so failures replay.

### Changed

- `--out`, `--summary-out` and `plan --out` are documented in their flag help as
  trusted operator paths: overwritten, and not contained to `--repo`. Unlike
  `--file` and the discovered CODEOWNERS path, no repository can influence them.

## Earlier releases

`v0.0.1` (2026-07-27) through `v0.0.5` (2026-08-04) predate this file; see the
auto-generated notes on each
[GitHub release](https://github.com/jordonpeterson/codeowners-tool/releases).
