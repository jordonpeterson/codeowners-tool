# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
are [semantic](https://semver.org/), with `MAJOR.MINOR` set by hand in `VERSION`
and `PATCH` assigned by the release workflow.

While the version is `0.x`, flag names, JSON record fields, and the exit-code
contract may change in a minor release. Exit codes are what scripts depend on, so
changes to which class a failure lands in are called out explicitly.

## [Unreleased]

### Fixed (from five user-test personas driving the shipped binary)

- **A `declare` batched with an op on one tracked file is no longer refused
  (R-22b).** R-8's zero-match guard exists because two `declare`d rules both land
  at EOF, where last-match-wins picks between them silently — but it fired
  whenever *either* op was declared, so it also caught a declare paired with an
  op resolved against the real tree. `remove_owner(/.github/CODEOWNERS, @org/bot)`
  batched with `add_owner(**/justfile, @org/bot)` was refused as
  order-dependent in exactly the repositories with no justfile, even though the
  two scopes can never meet — forcing "grant the bot .github/ but not
  .github/CODEOWNERS" to be split into two rollouts, and doing it repo by repo
  so `check` passed and `sync` failed. The guard now skips a pair whose other op
  scopes an anchored, wildcard-free path naming one tracked file: a declared
  scope matches nothing tracked, and no path can appear beneath a file. Every
  other pair is refused exactly as before — a directory scope (`/src/`) still
  gains files, a wildcard scope (`/.github/CODEOWNER?`) still grows, an
  unanchored scope (`justfile`) still matches at any depth, and two declares are
  still the hazard the guard was written for. No exit-code class moves: what was
  a per-repo exit 2 is now exit 0.

- **One owner identity, everywhere (R-38).** `@handles` are case-insensitive on
  GitHub, and only `lint` and the R-33c duplicate check knew it — matching
  compared bytes. So `remove_owner(/x/, @ORG/TEAM)` reported `unchanged` at exit
  0 against a file holding `@org/team`, and a fleet run of "revoke the departed
  team" reported **converged** on every repository that capitalised the handle;
  `add_owner` appended a second spelling of an owner who already owned the path,
  growing the line on every scheduled run. Every comparison of two owners now
  asks `ops.FoldOwner` — add, remove, `rename_owner`'s old name, `set_owners`,
  commutation, resolution and `verify` — so removal takes every spelling with it
  and re-running any op with a differently cased handle is a byte-identical
  no-op. Spelling is preserved, never normalised: the file's handle wins and
  only `rename_owner` writes new text. E-mail owners still compare exactly —
  the local part of an address is not ours to case-fold. Exit-code change: an
  add and a remove of one owner spelled two ways is now the exit-3 policy error
  it always was, not a per-repo exit 2, and `set_owners(L) ∘ add_owner(A)` over
  one owner in two spellings is no longer refused as order-dependent.

- **A policy error writes no record, and now says so.** The exit-3 verdict is
  reached before the repository is opened, so no `--out` file and no
  `--summary-out` is created — deliberate, since a row there would put a phantom
  repo in the aggregation, but previously silent. A fleet aggregating
  `records/*.json` saw affected repos DISAPPEAR rather than appear as refused,
  so the count of repos needing attention went down. The exit code is the
  signal, and stderr now says the record is missing. (#30)
- **The stale-comment warning follows the old handle, not the edit.** It was
  gated on the rename reporting `applied`, which silenced it in exactly the
  repos where a retired team's handle survives only in a comment — the repos
  where docs/FLEET.md promises it is the only thing that will find them. Both
  cases now warn, with wording that does not claim a rename that did not
  happen. (#33)
- **The R-8 remedy warns what it costs.** Telling an operator to run the broader
  op alone is right, and for a `set_owners` that run replaces the owners of
  everything in scope; one followed it verbatim and stripped two teams from a
  security repo. The message now says so and points at `--dry-run`. The old
  closing clause ("which is two exit-0 invocations") read as reassurance and is
  gone. (#35)
- **An unknown `--on-empty` value is exit 3**, from the argument alone, like the
  `on_empty` policy field it mirrors. It used to be validated only where a
  removal actually emptied an owner set, so a typo ran a whole fleet at exit 0
  and then reported exit 2 on the one repo that tripped it. (#36)
- **docs/FLEET.md's resumable script no longer retires the repos it files for
  triage.** An exit-2 repo went into `needs-human` *and* `done.txt`, so the
  resume guard skipped it forever — including after a human fixed it. (#37)
- **`sync` reports who LOSES access.** The record and `--summary-out` are what
  docs/FLEET.md loops over, and for a displacing change they carried no
  before-state at all: `changes` is line-level, an inserted rule has no previous
  line, so `set_owners(*, [@acme/everyone])` over a repo with hand-curated
  ownership reported `applied`, `5 path(s) change owners`, `warnings: null` —
  and three teams had just lost a security directory. Nothing distinguished
  co-owning five files from displacing five files' owners. The planner already
  computed it; `owners_removed` now carries it per path, and `--summary-out`
  gains an **Owners losing access** section grouped by owner, since the
  reviewer's question is which teams stop owning things rather than how many
  paths moved. Present only when access is actually lost, and a run that merely
  re-spells a handle reports none: owner identity is R-38a's. Named the single
  highest-value improvement by two independent user tests. (#32)
- **`--summary-out`'s `proven` column stops doubling as the skip reason**, which
  put a full sentence where a reviewer scans for `tree` or `structural`. Reasons
  have their own column. (#41)

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
  - The merge rule refuses a join once the accumulator is already a valid
    owner, with a bare `/` as the sole exception. Guarding only the run's first
    token left the fusion defect reachable one space away — `/src @ alice /docs
    @bob` still fused into `@alice/docs` where `/src @alice /docs @bob` was
    correctly refused — because brokenness is a property of the FIRST join, not
    of the whole run. Reproduced in 178 of 200,000 generated files by a second
    review; now covered by a byte-conservation property test rather than by
    examples of the two spellings we happen to know about.
  - Owner handles are case-folded for the LOOKUP (never in the file). GitHub
    treats `@Org/Team` and `@org/team` as one owner and team slugs are lowercase
    by construction, so asking about a capitalised spelling risked a 404 that
    means only "you typed it differently" — and under lint a 404 deletes.
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

- **`rename_owner` substitutes in place.** It removed the old identifier and appended the
  new one, which preserves the owner set — GitHub cares about nothing else — but permutes
  every line that lists the renamed team alongside anyone else. The op is documented as
  pure identifier substitution, the one op safe to read as a text diff, and a hundred-repo
  reorg is only reviewable if that is true. When the new name is already on the line the
  result collapses to one owner at the earliest position, which is a real change of
  review requirements and shows up as such in `changes`.
- **A non-commuting batch provable from the op strings alone is now exit 3 everywhere**,
  when both ops carry the default `on_zero_match: require`. `set_owners(*, [@a])` next to
  `add_owner(/services/api/, @b)` was decided against the tree, so `check` passed the
  policy and every repo refused at exit 2 — a hundred entries in `needs-human` for a
  defect that was in the policy the whole time. Such a policy converges nowhere (a repo
  with the scope refuses on the overlap, one without it refuses on the zero match), which
  is what makes the verdict repo-independent. Ops carrying `skip` or `declare` are
  deliberately excluded — their order-dependence *is* a fact about the tree — and so are
  overlaps only a real tree reveals: both stay exit 2, per repo.
- **`docs/LINTING.md`** carries the repair guide — the three stages, what lint
  refuses to guess at and why, the exit-code table, every error you will
  actually hit and what to do about each, and the warning about scheduling
  `lint` alongside `sync`. The README keeps a 45-line entry point: what the
  three stages are, one worked `--dry-run`, the CI gate, and a link.
  `docs/REFERENCE.md` keeps the lookup tables, so the same material is not
  written out three times. The README's own lint section is five lines and a
  link: the reader who types `lint` wants the command, not an essay on what it
  does before they have run it once.
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
