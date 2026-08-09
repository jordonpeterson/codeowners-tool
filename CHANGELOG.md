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

- **`lint` repairs the whole file instead of only describing it.** Three
  stages, in this order: rejoin `@`handles that whitespace has split
  (`/x/ @ org/team` — GitHub skips such a line entirely, so that team owns
  nothing and nobody is told); remove users and teams that definitively do not
  exist; and, only with `--remove-stale-paths`, delete rules matching zero
  tracked files. The rejoin runs before any lookup on purpose: `@` and
  `org/team` are not two owners that do not exist, they are one owner nobody has
  asked about yet. `--dry-run` reports without writing and exits 4, which is the
  CI gate. `audit` is unchanged and still never writes; the bytes still reach
  disk only through `apply`, with the hash pin, the size cap, the pre-write
  validation and the atomic rename.
  - It is a verb, not a flag. It began as `audit --lint`, and that spelling
    still works and routes to the same code — but six of `audit`'s fourteen
    flags changed meaning or validity on that one boolean, three error messages
    existed only to police the coupling, `--help` introduced `-dry-run` ("with
    --lint, …") three entries before the flag explaining what `--lint` was, and
    the exit contract had already forked. `lint` has ten flags and none of them
    are mode-dependent.
  - A rule that misses only because of CASE (`/Src/` where the directory is
    `src/`) is spared by `--remove-stale-paths` and reported. It is audit's A-5
    — a typo, not a dead rule — and deleting it destroys the only evidence the
    typo happened while quietly un-owning the files it was aimed at. Lint
    cannot correct the casing safely, because the tree's real casing may not be
    the naive lowercase, so it hands it over.
  - The JSON record carries `needs_human` and `exit_code`, both from the same
    function that produces the process status. The obvious hand-written gate
    (`jq '.changes|length > 0'`) went green over a file whose only problems were
    ones lint refuses to guess at.
  - R-12 is applied to the WHOLE RUN rather than per owner: one inconclusive
    lookup and nothing is written at all, including the offline whitespace
    fixes. A rate-limited run therefore leaves the file byte-identical instead
    of dribbling out partial repairs, and `--lint` requires a token and
    `--github-repo` rather than silently degrading to a whitespace tidy.
  - The repair is a guess, so its boundaries are exact: the run must start at a
    token that is VISIBLY BROKEN — beginning `@` and not already a valid owner —
    every join must sit against an `@` or a `/`, and the concatenation must be a
    valid handle. `@org /team` is therefore NOT repaired: on a CODEOWNERS line
    everything after the pattern is an owner, so it is shaped exactly like
    `@alice /docs`, somebody putting two rules on one line, and adversarial
    review produced precisely that write — `@alice/docs` fused into one owner,
    handing `/docs`'s owner every file under `/src`, at exit 0. Ambiguity is
    reported, not guessed. An email owner is never repaired for the same reason:
    `a@b.com` is already valid, and `a@b.com` + `/x` would concatenate into a
    syntactically valid address nobody wrote. A repair is accepted only if it
    conserves every non-whitespace byte and re-parses as a rule with a
    byte-identical pattern.
  - Stale-path removal stays opt-in per R-11: a pattern matching nothing may be
    deliberate and forward-looking, and deleting it destroys that intent.
  - It carries both of `sync`'s repository guards, which adversarial review
    found it had been shipped without. `--branch` must be what the clone has
    checked out, or a directory present on HEAD and absent on that ref makes its
    rule look stale and `--remove-stale-paths` un-owns a directory sitting right
    there in the checkout, at exit 0 (`--dry-run` lifts this one — nothing is
    written, so nothing lands in the wrong tree). And `--repo` must be the
    repository root, or discovery finds the root's `.github/CODEOWNERS` in a tree
    git reports relative to that root while the join addresses a different file
    of the same name one level down — reading one file, rewriting another,
    printing the first one's path, and leaving the file GitHub loads untouched.
    That guard holds under `--dry-run` too.
  - Exit codes under `--lint` follow one rule — 0 when the file needs nothing
    further from a human, 4 when it does (pending fixes under `--dry-run`, or a
    line lint would not guess at). `--remove-stale-paths`, `--on-empty` and
    `--dry-run` are rejected with exit 3 when `--lint` is absent rather than
    ignored; `--cache-dir` is rejected *with* it, because a cached "does not
    exist" is served without revalidation and here that answer deletes an owner
    instead of printing a finding.
  - **`--lint` never returns 1.** The precise taxonomy maps 1 to "no-op —
    nothing to change", and a CODEOWNERS that needs no repair is this mode's
    SUCCESS: under `set -e` a scheduled fleet run would otherwise read every
    healthy repository as a failure. This is the only place the precise table's
    1 does not apply.
  - **Removing a team requires an org-owner token.** A secret team the caller
    cannot see returns the same 404 as a deleted one, and enumerating the org
    does not separate them, so anyone else's 404 is inconclusive. The check only
    runs once a team already looks gone, so it costs nothing on the common path.
- `SECURITY.md`, `CONTRIBUTING.md`, `CHANGELOG.md`, `.github/dependabot.yml`
  (GitHub Actions only — see the file), and a `CODEOWNERS` for this repository,
  which previously failed its own A-11 check.
- The differential fuzz against the vendored `hmarr/codeowners` oracle runs on
  every PR — the full 500k cases, in its own parallel job (~35s).
- `difftest` takes an optional seed; with it fixed, raising the case count only
  extends one sequence. Printed with the result so failures replay.

### Changed

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
