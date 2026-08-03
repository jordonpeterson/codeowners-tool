# codeowners-tool

A single CLI for making **safe, intent-level, verifiable** changes to large
GitHub CODEOWNERS files, plus an auditor that finds rot (dead owners, dead
rules, inert owners). GitHub only (github.com and GHES); GitLab/Bitbucket are
explicit non-goals.

**The problem it solves:** CODEOWNERS edits are expressed in *lines*, but what
anyone cares about is *resolved ownership per file path*. The two are related
by non-obvious semantics (last match wins; owner sets don't union; appending
`/x/ @team-2` **replaces** the owners of `/x/`, it doesn't add to them). This
tool makes you express intent over resolved ownership and refuses to apply
anything it can't prove.

```
intent (ops) ──▶ PLAN ──▶ ASSERT ──▶ APPLY ──▶ VALIDATE
                  │         │
                  │         └── gate; refuses on violation
                  └── resolves ownership before/after over the real git tree
```

## Getting started

### Install

**Homebrew** (macOS/Linux):

```sh
brew install jordonpeterson/tap/codeowners-tool
```

**Install script** (macOS/Linux) — downloads the right prebuilt binary,
verifies its checksum, and installs it:

```sh
curl -fsSL https://raw.githubusercontent.com/jordonpeterson/codeowners-tool/main/install.sh | sh
```

Set `VERSION=vX.Y.Z` to pin a release or `BINDIR=~/.local/bin` to change the
install location.

**Direct download**: grab the archive for your platform from the
[latest release](https://github.com/jordonpeterson/codeowners-tool/releases/latest),
verify it against `checksums.txt`, extract, and put `codeowners-tool` on your
`PATH`. Every release ships Linux, macOS, and Windows builds for both amd64 and
arm64.

> **macOS note:** the binaries are not notarized, so a build downloaded through
> a browser is quarantined by Gatekeeper. Clear it with
> `xattr -d com.apple.quarantine ./codeowners-tool`. Homebrew and the install
> script above are unaffected — neither quarantines its downloads.

**From source** with Go 1.24.7+ (the version pinned in `go.mod`):

```sh
go install github.com/jordonpeterson/codeowners-tool/cmd/codeowners-tool@latest
```

### Make your first change

Run inside a git repository that contains a CODEOWNERS file (searched in
`.github/`, then the repo root, then `docs/`):

```sh
# Add @org/team-1 as a co-owner of everything under /services/api/, keeping any
# existing owners. This only writes a reviewable plan — nothing is changed yet.
codeowners-tool plan --op 'add_owner(/services/api/, @org/team-1)' --out plan.json

# plan.json shows the resolved ownership rows and the exact line diff.
# Apply only what the planner proved safe:
codeowners-tool apply --plan plan.json
```

`plan` re-resolves ownership across your real git tree and refuses anything it
can't prove; `apply` writes only the proven edit. Both are idempotent:
re-planning the same op against an already-satisfied file changes nothing and
**exits 1** (`nothing to change`) rather than 0, so a CI step that runs `plan`
unconditionally should treat 1 as success. To audit a repo for rot or to
verify in CI that a change stayed inside its declared scope, read on.

## Quick start

```sh
go build -o codeowners-tool ./cmd/codeowners-tool

# Add a co-owner to everything under /services/api — existing owners retained:
./codeowners-tool plan --op 'add_owner(/services/api/, @org/team-1)' --out plan.json
# Review plan.json (ownership rows + literal line diff), then:
./codeowners-tool apply --plan plan.json

# Audit for rot:
GITHUB_TOKEN=... ./codeowners-tool audit --github-repo org/repo --format json

# Prove in CI that a change touched nothing outside its declared scope:
./codeowners-tool snapshot --branch main --out before.json
./codeowners-tool snapshot --branch feature --out after.json
./codeowners-tool verify --before before.json --after after.json --scope /services/api/
```

`verify` compares *resolved ownership per path*, independently of the planner —
CI can check the invariant without trusting the tool that made the change.
Two things to know about the recipe above:

- **Omitting `--scope` asserts that nothing changed at all.** Scopes are the
  allowlist; with none, every difference is a violation.
- **Files the branch adds or deletes are reported, never violations.** The two
  snapshots come from different refs, so their trees differ. An added path had
  no prior ownership for INV-2 to preserve, so it prints as `added:` and does
  not fail the check. A real reassignment still fails it — the subtree's
  pre-existing files change.

## Operations (Engine A — mutation)

Scope is a directory, file path, or glob — same syntax as CODEOWNERS patterns.

| Op | Meaning |
|---|---|
| `add_owner(scope, owner)` | Owner becomes a **co-owner**; every pre-existing owner of every path in scope is retained. |
| `set_owners(scope, [owners])` | Exact owner set for every path in scope, displacing prior owners. `[]` is legal: it deliberately un-owns the scope. |
| `remove_owner(scope, owner)` | Owner stops owning every path in scope. If a rule's owner set would empty, `--on-empty` is **required** (see below). |
| `rename_owner(old, new)` | Global identifier substitution — the only op safe as pure text replacement (it can't change any rule's match set). |

### The two invariants

- **INV-1 (in scope):** after apply, every in-scope path resolves to exactly
  what the op requires.
- **INV-2 (out of scope):** after apply, every out-of-scope path resolves to
  exactly what it did before. **This is the product.**

The planner synthesizes line edits, then *proves* the result by re-resolving
the entire tracked tree (at `--branch`, default HEAD) and comparing against an
independently computed desired state. Anything unprovable → exit 2, nothing
written. Plans are idempotent (re-running is a no-op) and preserve every
untouched byte — comments, blank lines, spacing, ordering.

### How the planner edits lines

You express intent over paths; the planner decides which lines move. It never
reorders or reformats, but it does **add lines you did not write**. When an
existing rule governs your scope *and* paths outside it, amending that rule in
place would violate INV-2 — so the planner inserts a **narrowing rule**
immediately after it. That is why the getting-started example above produces a
line nobody typed:

```
/services/ @org/platform
/services/api/ @org/platform @org/team-1   ← synthesized: narrows /services/
/web/ @org/frontend
```

Because the last matching rule wins (S-1), this leaves everything outside
`/services/api/` resolving exactly as before. Two consequences worth knowing:

- **Refusal when no narrowing is expressible.** For some scope/rule
  combinations no CODEOWNERS pattern describes exactly the intersection.
  Amending would break INV-2 and appending would break INV-1, so the plan is
  refused (exit 2) rather than guessed at. Narrow the scope, or restructure the
  offending rule by hand.
- **The inexact-narrowing warning.** A synthesized glob can be exact for every
  file tracked today yet not provably confined for files added later. The plan
  still applies, and prints a warning naming the pattern — read it, because it
  is telling you a future file could land on the wrong side.

Every synthesized line carries a `reason` in the plan JSON explaining why it
exists.

### `--on-empty` (R-6)

Removing the sole owner of a rule needs an explicit policy — **there is no
default**, and the documented recommendation is `error`:

- `error` — refuse (recommended: consistent with the tool's fail-closed posture)
- `inherit` — delete the rule; the preceding broader rule takes over
  (removal **cascades** if the fallthrough rule also lists the owner)
- `unowned` — keep the pattern with zero owners (GitHub's sanctioned
  substitute for `!` negation)

Under `inherit`/`unowned` the resulting reassignment is shown in the plan's
ownership rows.

## Audit (Engine B)

Read-only. **Never writes** — where a fix is expressible it emits Engine A op
strings for a human to review and run through `plan`/`apply`, the system's
single writer path.

| ID | Check | API | Auto-fix |
|---|---|---|---|
| A-1 | Owner doesn't exist (deleted/renamed user or team) | yes | proposes `remove_owner` |
| A-2 | Owner exists but isn't in the org | yes | proposes `remove_owner` |
| A-3 | Owner lacks **explicit write access** (org membership isn't enough) | yes | proposes `remove_owner` |
| A-4 | Rule matches zero tracked files | no | report only, permanently — a dead pattern may be deliberate intent |
| A-5 | Rule dead **only because of case** (`/Src/` vs `src/`) | no | suggests corrected pattern |
| A-6 | Rule fully shadowed by later rules | no | report only |
| A-7 | Duplicate pattern | no | report only |
| A-8 | Syntax errors | optional | no |
| A-9 | Unowned path coverage | no | n/a |
| A-10 | Multiple CODEOWNERS files present | no | error — GitHub uses only the first |
| A-11 | CODEOWNERS file itself unowned | no | report only |
| A-12 | File size approaching 3 MB | no | n/a |

**Fail closed (R-12):** a 404 can mean deleted, renamed, invisible to the
token, or rate-limited. The client probes org/repo visibility first; anything
inconclusive is reported `unknown`, exits 5, and **never proposes a removal**.
An expired token quietly stripping owners is the worst failure this tool can
produce, so it can't. Email owners are `unverifiable`, never dead (R-13).
Removing a sole owner is presented as a **reassignment** with before → after
owners per path, never a bare line deletion (R-14). Lookups are cached in
memory per run and optionally on disk (`--cache-dir`, `--cache-ttl`).

## CLI reference

Five commands. `plan` and `apply` are the only writer path; `audit`,
`snapshot`, and `verify` are read-only.

**Common to `plan`, `audit`, `snapshot`:**

| Flag | Default | Meaning |
|---|---|---|
| `--repo DIR` | `.` | Path to the local git repository |
| `--branch REF` | `HEAD` | Ref whose tracked tree governs resolution (S-7) |
| `--file PATH` | discovery | Repo-relative CODEOWNERS override, bypassing `.github/` > root > `docs/` precedence. The escape hatch when A-10 reports more than one file |

**`plan`** — resolve, synthesize, prove. Writes a plan, never the file.

| Flag | Default | Meaning |
|---|---|---|
| `--op 'kind(args)'` | required | Operation; repeatable for a batch |
| `--on-empty POLICY` | none | `error`\|`inherit`\|`unowned`; required only when a removal would empty an owner set |
| `--out FILE` | stdout | Where to write the plan JSON |
| `--max-size N` | `3000000` | Hard byte cap; over it, refuse (S-4) |
| `--warn-size N` | `2500000` | Byte threshold that emits a warning (R-9) |

**`apply`** — write the proven edit, then validate.

| Flag | Default | Meaning |
|---|---|---|
| `--plan FILE` | required | Plan JSON from `plan` |
| `--repo DIR` | plan's repo | Override the repository recorded in the plan |

The plan pins the input file's SHA-256; if the file changed since planning,
apply refuses rather than clobber the other edit. A write that introduces new
syntax errors is rolled back (exit 6).

**`audit`** — find rot. Never writes.

| Flag | Default | Meaning |
|---|---|---|
| `--checks LIST` | all | Comma-separated subset, e.g. `a1,a3,a6`. `a1`, `A1`, `a-1`, `A-1` all parse; an unknown name is a hard error, so a typo can't make an audit pass vacuously |
| `--format FMT` | `text` | `text` or `json` |
| `--github-repo owner/name` | none | Required, with a token, for the API checks A-1…A-3 |
| `--token T` | `$GITHUB_TOKEN` | GitHub PAT |
| `--api-url URL` | `https://api.github.com` | API base, for GHES |
| `--cache-dir D` | memory only | Persist API lookups to disk (R-15) |
| `--cache-ttl DUR` | `24h` | Disk cache lifetime |

Without a token and `--github-repo`, audit runs the offline checks (A-4…A-12)
and says so. If you *explicitly* request A-1/A-2/A-3 via `--checks` and they
can't run, that is exit 5 — inconclusive, not a silent skip.

**`snapshot`** — write resolved ownership for every tracked path at a ref.

| Flag | Default | Meaning |
|---|---|---|
| `--out FILE` | stdout | Where to write the snapshot JSON |

**`verify`** — compare two snapshots.

| Flag | Default | Meaning |
|---|---|---|
| `--before FILE` | required | Baseline snapshot |
| `--after FILE` | required | Snapshot to check |
| `--scope PATTERN` | none | Where change is allowed; repeatable. **With none, any change is a violation** |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success — applied, or audit found nothing |
| 1 | No-op — nothing to change |
| 2 | Refused — would violate INV-1/INV-2, or exceed the 3 MB cap |
| 3 | Invalid input — malformed op, zero-match scope, conflicting batch |
| 4 | Audit findings present |
| 5 | Inconclusive — API unavailable, token insufficient, rate limited |
| 6 | Validation failed post-write; rolled back |

## GitHub semantics this tool encodes

| # | Property |
|---|---|
| S-1 | Last matching rule wins; owner sets do not union |
| S-2 | Patterns are gitignore-*like*: no `!` negation, no `[ ]` ranges, no `\#` escape; brackets are literals |
| S-3 | Invalid lines are **skipped individually** (current GitHub behavior — older docs claimed the whole file was disabled; this tool implements and tests the current per-line semantics, and still refuses to *write* new invalid lines) |
| S-4 | Files over 3 MB are silently not loaded at all |
| S-5 | Owners need **explicit write access**; org membership is not enough |
| S-6 | Paths are case-sensitive regardless of local filesystem |
| S-7 | The PR **base branch's** CODEOWNERS governs — resolution runs against a named ref's tree |
| S-8 | Only one file is used: `.github/` > root > `docs/`, never merged |
| S-9 | Zero-owner rules are legal and deliberately un-own a subtree |

## Design decisions (resolved from the spec's open list)

1. **`--on-empty` recommendation:** `error`.
2. **Performance:** naive full-tree resolve (twice per plan). Fine to ~100k
   files; a pattern-level pre-filter is a straightforward later optimization.
3. **GHES:** in, via `--api-url`; endpoint gaps degrade to exit 5, not wrong answers.
4. **Auth:** PAT only for v1 (`--token` / `$GITHUB_TOKEN`). GitHub App is the
   right v2 answer for org-wide automation.
5. **Branch handling:** local repo, any ref via `--branch` (git `ls-tree`);
   plan/apply read and write the working-tree file, resolve against the ref's tree.

## Tests as documentation

The test suite is the specification. Every test carries a doc comment naming
the spec requirement it enforces; `make docs` regenerates
[docs/BEHAVIOR.md](docs/BEHAVIOR.md) from them via `go/ast`, so the docs
cannot drift from what is verified. Verification layers:

- **Vendored corpus** (`testdata/patterns.json`) from
  [hmarr/codeowners](https://github.com/hmarr/codeowners) — the actively
  maintained reference implementation whose matcher encodes GitHub's observed
  divergences from gitignore.
- **Differential fuzz** (`make diff-test`): 500k random pattern/path cases
  against hmarr's *unmodified* matcher, vendored verbatim as an oracle.
- **Property tests**: thousands of generated (file, tree, op) cases proving
  INV-1/INV-2 and idempotence by independent re-resolution — these caught two
  real bugs during development (removal cascade under `inherit`; self-rename
  churn).
- **Acceptance tests T-1…T-11** from the spec, end-to-end CLI tests in real
  git repos, and mocked-API fail-closed tests.

## Prior art this design learned from

[hmarr/codeowners](https://github.com/hmarr/codeowners) (reference matcher),
[mszostok/codeowners-validator](https://github.com/mszostok/codeowners-validator)
(check taxonomy; also a cautionary tale — it mixes three glob engines),
[snyk/github-codeowners](https://github.com/snyk/github-codeowners) and
[beaugunderson/codeowners](https://github.com/beaugunderson/codeowners)
(both built on the npm `ignore` gitignore engine, whose `!`/`[ ]` support
diverges from GitHub — exactly the trap S-2 warns about),
[toptal/codeowners-checker](https://github.com/toptal/codeowners-checker)
(archived; the only prior mutation tool — it reorders lines, which R-1 forbids).

## Non-goals

GitLab/Bitbucket semantics · reordering or reformatting existing files ·
auto-deleting rules that match zero files · inventing owners · resolving
conflicting batches by precedence · opening PRs or any git write.

## License

MIT. The pattern matcher is ported from, and differentially tested against,
[hmarr/codeowners](https://github.com/hmarr/codeowners) (MIT, © Harry Marr) —
see NOTICE.
