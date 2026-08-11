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

- **`--max-paths-changed N` / `max_paths_changed` (R-25)**, an opt-in ceiling on how much
  ownership one run may move. Over it, the run refuses (exit 2, per repo), writes nothing,
  and keeps `paths_changed` in the record so the operator can see what it would have been.
  Off by default — a default ceiling would break every legitimate `set_owners(*, …)`
  baseline on upgrade and teach people to pass a huge number reflexively. Flag only with
  `--op`, field only in a policy, exactly like `--on-empty`: a ceiling is a claim about
  the intent, so it belongs in the artifact a reviewer approves.
- **`sync` records the file it wrote**, as `codeowners_path` in the JSON record and
  as a bullet in `--summary-out`. Ownership lives in `.github/`, the root, or
  `docs/` depending on the repo, so a rollout that has to commit its change could
  only `git add -A` and hope nothing else had moved in the clone. `plan` and
  `snapshot` already named their file; the record that drives a fleet was the one
  document that did not. The key is present **exactly when the run changed the
  file** — or, under `--dry-run`, would have — so its presence means there is
  something to stage and nothing else. Emitting it whenever a file was merely
  chosen would break the documented commit recipe on the two commonest fleet
  outcomes: an already-correct repo names a file with no diff, `git commit` fails
  with "nothing to commit", and a `set -euo pipefail` rollout dies at a repo that
  was fine. A refusal names its file in the `error` string instead, which is what
  triage needs.
- **`audit --fail-on any|warning|error|never`** decides which findings exit 4.
  Every finding is still printed under every setting — the flag moves the gate,
  not the report. Without it, the documented rollout and the documented CI gate
  contradicted each other: a baseline with `on_zero_match: declare` writes rules
  that match zero files, A-4 reports each one as a report-only `warning`, and the
  next push to every repo the rollout touched failed CI. The default is `any`, so
  the existing exit-4 contract is unchanged. Inconclusive stays exit 5 under every
  setting (R-12).
- **`sync` warns about four things that converge anyway**: a second CODEOWNERS
  file GitHub ignores (A-10), a run writing a file that is not the one GitHub
  reads (a `--file` at the wrong location — reported `applied` while moving no
  ownership at all), lines GitHub cannot parse and silently skips (S-3), and a
  comment still naming an owner a `rename_owner` renamed away. None is a reason
  to refuse a correct edit, and none is visible at fleet scale unless the run
  that touched the file says so. Warnings now also render into `--summary-out`,
  under **Worth a look** — the PR is the one moment somebody is already reading
  that file and can fix it in the same commit.
- `SECURITY.md`, `CONTRIBUTING.md`, `CHANGELOG.md`, `.github/dependabot.yml`
  (GitHub Actions only — see the file), and a `CODEOWNERS` for this repository,
  which previously failed its own A-11 check.
- The differential fuzz against the vendored `hmarr/codeowners` oracle runs on
  every PR — the full 500k cases, in its own parallel job (~35s).
- `difftest` takes an optional seed; with it fixed, raising the case count only
  extends one sequence. Printed with the result so failures replay.

### Changed

- **`rename_owner` substitutes in place.** It removed the old identifier and appended the
  new one, which preserves the owner set — GitHub cares about nothing else — but permutes
  every line that lists the renamed team alongside anyone else. The op is documented as
  pure identifier substitution, the one op safe to read as a text diff, and a hundred-repo
  reorg is only reviewable if that is true. When the new name is already on the line the
  result collapses to one owner at the earliest position, which is a real change of
  review requirements and shows up as such in `changes`.
- **A non-commuting batch provable from the op strings alone is now exit 3 everywhere.**
  `set_owners(*, [@a])` next to `add_owner(/services/api/, @b)` was decided against the
  tree, so `check` passed the policy and every repo refused at exit 2 — a hundred entries
  in `needs-human` for a defect that was in the policy the whole time. Both `check` and
  `sync` now reject the provable subset before opening a repository, with the remedy in
  the message. Overlaps only a real tree reveals are unchanged: exit 2, per repo.
- **The README is a front door, not the manual.** It now carries the mental model
  (`add_owner` co-owns, `set_owners` displaces), one worked example, and three
  task guides — lint a file, write a new one, modify an existing one — plus a
  router table and a `snapshot`/`audit`/`check` section for finding out what a
  repo already has. Install keeps only `brew`; every other route, and the
  provenance verification that goes with the direct download, moved to
  `docs/INSTALL.md`. The command, flag, policy-field, JSON, exit-code, audit and
  GitHub-semantics tables moved to `docs/REFERENCE.md`, and the fleet script to
  `docs/FLEET.md`. The supply-chain test that pinned `gh attestation verify` to
  the README now pins it to `docs/INSTALL.md`, and a second test pins the
  README's link to it, so the path from "I want to install this" to "verify what
  you downloaded" is still enforced rather than merely intended.
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
