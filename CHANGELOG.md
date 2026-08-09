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
- **The Homebrew tap pulls its own bump instead of the release pushing it.** The
  push needed a personal access token holding write on a second repository,
  stored here, with no way for this repository to see whether it was still valid.
  A workflow in `jordonpeterson/homebrew-tap` now polls for the newest release and
  writes its own formula with the token Actions mints for it, so there is no
  cross-repo secret to rotate or to silently expire. It verifies the release's
  build provenance before writing, so the formula's `sha256` values mean "built by
  this workflow" rather than "whatever that release currently holds". The cost is
  latency — a release cannot notify the tap without reintroducing the token — so
  the tap polls hourly and can be run on demand.
- A supply-chain gate now rejects any `secrets.*` reference other than
  `GITHUB_TOKEN` in a workflow. The failed tap token was invisible precisely
  because an unset secret is indistinguishable from a set one until something
  tries to use it.

### Fixed

- **Merges to `main` publish a release again.** The build-provenance step added in
  the last change ran with `id-token: write` and no `attestations: write` — the
  token that signs the statement, but not the permission to store it. Every
  release since died there on `Resource not accessible by integration`, and
  because the step deliberately runs before `gh release create`, the failure
  dropped the whole release rather than leaving an unattested one. `v0.0.6` never
  shipped. A supply-chain gate now fails any job that runs an `actions/attest*`
  step without the permission to persist its output.
- **A change to `release.yml` cuts a release.** The path filter listed only the
  binary's sources, so a merge that repairs the release pipeline could not itself
  publish the releases the broken pipeline had dropped.
- **The Homebrew formula tracks releases again.** `brew install
  jordonpeterson/tap/codeowners-tool` — the first install method the README lists
  — served `v0.0.2` from July 28, missing every fix through `v0.0.6`. The bump was
  never automated in practice: the job that pushed it was gated on a
  `HOMEBREW_TAP_TOKEN` secret that was never set, and with the secret absent it
  logged a notice and exited 0, so four releases showed a green `bump homebrew
  tap` while the formula went nowhere.

## Earlier releases

`v0.0.1` (2026-07-27) through `v0.0.5` (2026-08-04) predate this file; see the
auto-generated notes on each
[GitHub release](https://github.com/jordonpeterson/codeowners-tool/releases).
