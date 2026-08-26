# Reference: audit checks and `lint`

The lookup tables. [LINTING.md](LINTING.md) is the guide — what each stage does, what it
refuses to guess at, and every error with what to do about it.

## Audit checks

Read-only **except `audit --lint`** ([below](#lint)). Plain `audit` never writes —
where a fix is expressible it emits op strings for a human to review and run through
`plan`/`apply`. Even under `--lint` the bytes reach disk only through `apply`, which
remains the system's single writer path.

| ID | Check | API | Auto-fix |
|---|---|---|---|
| A-1 | Owner doesn't exist (deleted/renamed user or team) | yes | proposes `remove_owner`; **applied** by `--lint` |
| A-2 | Owner exists but isn't in the org | yes | proposes `remove_owner` |
| A-3 | Owner lacks **explicit write access** (org membership isn't enough) | yes | proposes `remove_owner` |
| A-4 | Rule matches zero tracked files | no | report only by default — a dead pattern may be deliberate intent; deleted only by `--lint --remove-stale-paths` |
| A-5 | Rule dead **only by an invisible spelling difference** — case (`/Src/` vs `src/`, S-6) or Unicode normalization (NFC `é` vs NFD `é`) | no | suggests corrected pattern |
| A-6 | Rule fully shadowed by later rules | no | report only |
| A-7 | Duplicate pattern | no | report only |
| A-8 | Syntax errors | optional | no |
| A-9 | Unowned path coverage | no | n/a |
| A-10 | A CODEOWNERS file GitHub does not load, or is about to stop loading the one it does | no | error for a second **root-level** file, a **symlinked** governing one, or an uncommitted **higher-precedence** one; warning for a **nested** one |
| A-11 | CODEOWNERS file itself unowned | no | report only |
| A-12 | File size approaching 3 MB | no | n/a |

**A-10's four shapes.** A second *root-level* file and a *symlinked* governing file are
`error`: what governs is wrong or absent, and with a symlink GitHub loads no rules at all,
so the rest of the report is suppressed — `cat-file` returns the link target, not a
document. A *nested* `packages/foo/CODEOWNERS` is `warning`: GitHub never searches it, so
what governs is not in doubt, and it is often a deliberate leftover another tool consumes.
Under `--checks` without `a10`, a symlinked file is exit 5, never a clean run.

The fourth is *mid-migration*: a higher-precedence CODEOWNERS sitting in the working tree,
uncommitted — `.github/CODEOWNERS` staged in a repo still governed by root `CODEOWNERS`.
`error`, because every rule in the file this report describes stops applying the moment
that commit lands. It is the one thing `audit` reads off disk, and it changes nothing else:
ownership still resolves against `--branch`, because that is what GitHub sees. Reported
only when `--branch` is the commit this clone is standing on — on any other ref the files
on disk belong to a different tree — and ignored files never count.

**A-5 and normalization.** NFC and NFD spellings of an accented name render identically and
CODEOWNERS matches bytes, so the pattern is dead with nothing on screen to show why. The
comparison is a partial NFD over Latin-1 accented letters — not a full Unicode
normalization, and the finding says so — and it prints the codepoints of both spellings.
A different accent (`é` vs `è`) is a different name and stays A-4.

**A-6 is disclosed by the run that causes it.** Where an inserted line leaves a
pre-existing narrower rule unable to win any path, `plan`/`sync` warn and name it, so the
run that authors the finding is not the one run silent about it.

Run a subset with `--checks a1,a3,a6` (`a4`, `A4`, `a-4` and `A-4` are all accepted; an
unrecognized name is a hard error, because silently matching nothing would make audits
pass vacuously). Requesting A-1/A-2/A-3 without both `--token` and `--github-repo` is exit
5, not a silent skip.

**`--fail-on` sets the gate, not the report.** Every finding is printed under every
setting; the flag decides which of them make the run exit 4.

| `--fail-on` | Exits 4 when a finding is |
|---|---|
| `any` *(default)* | any severity — the behavior this flag was added under |
| `warning` | `warning` or `error`; `info` reports only — A-9 unowned-path coverage, and A-1's `unverifiable` email owners (R-13) |
| `error` | `error` only — A-1, A-3, A-8, A-10 (root-level or symlinked), and A-12 over the cliff |
| `never` | never; findings are reported and the run exits 0 |

The case it exists for: a fleet baseline uses `on_zero_match: declare`, a declared rule
matches zero files by construction, and A-4 reports every one of them at severity
`warning` — so gating CI on the default turns every repo the rollout touched red. Naming
the other eleven checks with `--checks` would do it too, and would silently opt out of any
check added later. Inconclusive runs are a different axis: exit 5 under every setting, as
R-12 requires — a check that could not run is not a finding whose severity you can weigh.

**Fail closed (R-12):** a 404 can mean deleted, renamed, invisible to the token, or
rate-limited. The client probes org/repo visibility first; anything inconclusive is
reported `unknown`, exits 5, and **never proposes a removal**. An expired token quietly
stripping owners is the worst failure this tool can produce, so it can't. Email owners are
`unverifiable`, never dead (R-13). Removing a sole owner is presented as a
**reassignment** with before → after owners per path, never a bare line deletion (R-14).
Lookups are cached in memory per run and optionally on disk (`--cache-dir`, `--cache-ttl`).

`--token` exists but `$GITHUB_TOKEN` is the safer habit: the flag package prints non-empty
string defaults in its usage text, so the environment variable is read only after parsing,
where nothing can render it into a log (CWE-532).

## `lint`

Repairs three of the checks above, over the whole file. **[LINTING.md](LINTING.md)** is
the guide — what each stage does, what it refuses to guess at, the errors and what to do
about them. This is the lookup table.

`audit --lint` is the older spelling and is the same code path; under it `--checks` is
rejected, because a subset of a whole-file repair is ambiguous rather than smaller.

| Flag | Meaning |
|---|---|
| `--github-repo owner/name` | **Required**, bar the offline mode below. Probed, so a token that cannot see the repo stops the run. |
| `--token` / `$GITHUB_TOKEN` | **Required**, bar the offline mode below. Owner existence is not decidable offline. |
| `--dry-run` | Compute and report; write nothing. Exit 4 if anything is pending. |
| `--remove-stale-paths` | Stage 3. Deletes rules matching nothing tracked **and** nothing on disk. |
| `--on-empty error\|inherit\|unowned` | R-6, required only when a removal would empty a rule. `inherit` deletes the line. |
| `--file PATH` | The path is discovered from `--branch`'s tree, so an uncommitted CODEOWNERS needs this. |
| `--repo`, `--branch`, `--api-url`, `--format` | As elsewhere. |

`--cache-dir` and `--cache-ttl` are rejected (exit 3): a cached negative is served without
revalidation, and here that answer deletes an owner rather than printing a finding.
Lookups are still cached in memory per run.

**Offline tree-only mode:** with `--remove-stale-paths` and *neither* a token nor
`--github-repo`, the run does stage 3 alone — dead rules judged against the tree, invalid
lines still reported at exit 4, no owner verified, repaired, or removed — and the skip is
disclosed. One credential without the other still refuses at exit 5.

| # | Stage | Opt-in | What it does |
|---|---|---|---|
| 1 | Repair owner spacing | no | Rejoins an `@`handle split by whitespace: `@ org/team`, `@org/ team`, `@ org / team` → `@org/team`. Runs **before** stage 2 — those are one owner nobody has looked up, not two that are missing. |
| 2 | Remove dead owners | no | Drops users and teams that definitively do not exist (A-1 only; never A-2/A-3). |
| 3 | Remove stale rules | `--remove-stale-paths` | Deletes rules matching nothing in the committed tree and nothing on disk. Off by default per R-11. A rule missing only by **case** is spared and reported. |

**Refusals are deliberate.** A merge run may only start at a token that is not already a
valid owner, and once the accumulator is a valid owner it may absorb only a bare `/`. So
`@org /team` is not repaired: it is shaped exactly like `@alice /docs`, two rules on one
line, and guessing wrong hands one rule's owner the other rule's files. Byte conservation
over the owner region and a byte-identical pattern are checked on every repair.

**Fail-closed applies to the whole run.** One inconclusive lookup and nothing is written,
including the offline stages. Removing a **team** additionally requires an org-owner
token: a secret team returns the same 404 as a deleted one, and only an owner sees secret
teams. Email owners are `unverifiable`, never dead (R-13), and never make a run
inconclusive.

**Repository guards**, all exit 2: `--branch` must be the ref the clone has checked out
(lifted by `--dry-run`), `--repo` must be the repository root (not lifted), and the
governing CODEOWNERS must not be left unmerged by a conflict — a rule judged against both
sides of a merge at once is judged against text no commit has ever had. They exist because
lint proves against a tree and writes a file, and those are only the same document when
the two agree.

| Exit | When |
|---|---|
| 0 | Nothing to repair, or written with nothing left over |
| 2 | Refused — `--on-empty=error`, size cap, or either repository guard |
| 3 | Invalid input — missing `--on-empty`, a rejected flag, an empty tree under `--remove-stale-paths`, or hash drift between read and write |
| 4 | Still needs a person — pending fixes under `--dry-run`, an unrepairable line, or a case-only typo |
| 5 | Inconclusive, or missing credentials (bar the offline mode above). Nothing written |
| 6 | Post-write validation failed; rolled back |

`lint` never returns 1: a file needing no repair is its success, and "no-op" would make
every healthy repository in a fleet read as a failure under `set -e`.

`--format json` emits one object: `codeowners_path`, `applied`, `dry_run`, `needs_human`,
`exit_code`, `actions[]` (`kind`, `line`, `owner`, `pattern`, `reason`), `unverifiable[]`,
`changes[]`, `ownership_rows[]`, `diff`, `warnings[]`. `needs_human` and `exit_code` come
from the same function that sets the process status, so `jq -e .needs_human` is the gate.
An offline run additionally carries `owner_checks_skipped: true` — the record says
nothing about whether the owners exist.
Unlike the `sync` record, `actions`, `changes` and `ownership_rows` are always present
(possibly empty).

