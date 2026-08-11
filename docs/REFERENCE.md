# Reference

> IDs like `R-6`, `S-4`, `INV-2` and `A-9` are numbered requirements from the
> specification. Each is enforced by a named test — see [BEHAVIOR.md](BEHAVIOR.md), which
> is generated from the test suite. You never need them to use the tool; they're there so
> every claim below is traceable to something that's actually checked.

- [Commands](#commands)
- [`sync` and `check`](#sync-and-check)
- [Policy file fields](#policy-file-fields)
- [JSON output](#json-output)
- [`plan` and `apply`](#plan-and-apply)
- [Operations](#operations)
- [The two invariants](#the-two-invariants)
- [What `declare` costs](#what-declare-costs)
- [`--on-empty` / `on_empty` (R-6)](#--on-empty--on_empty-r-6)
- [Audit checks](#audit-checks)
- [Exit codes](#exit-codes)
- [GitHub semantics this tool encodes](#github-semantics-this-tool-encodes)
- [Design decisions](#design-decisions)
- [Prior art](#prior-art)

## Commands

```
sync     (--op 'OP' ... | --policy FILE) [--on-empty error|inherit|unowned]
         [--repo DIR] [--branch REF] [--file PATH] [--create] [--dry-run]
         [--format text|json] [--out FILE] [--summary-out FILE]
check    (--op 'OP' ... | --policy FILE) [--format text|json]
plan     --op 'OP' ... [--on-empty error|inherit|unowned]
         [--repo DIR] [--branch REF] [--file PATH] [--out plan.json]
apply    --plan plan.json [--repo DIR]
audit    [--checks a1,a3,a6] [--fail-on any|warning|error|never] [--format json|text]
         [--github-repo owner/name] [--token T | $GITHUB_TOKEN] [--api-url URL]
         [--cache-dir D] [--cache-ttl DUR] [--repo DIR] [--branch REF]
snapshot [--repo DIR] [--branch REF] [--out snap.json]
verify   --before before.json --after after.json [--scope PATTERN ...]
version  print the build this binary was stamped with
```

**Which file each command reads** is not uniform, and the difference is deliberate:

| Command | CODEOWNERS content from | Ownership resolved against |
|---|---|---|
| `sync`, `plan`, `apply` | the working tree (and written back there) | the tree at `--branch` |
| `audit`, `snapshot` | the tree at `--branch` | the tree at `--branch` |
| `check` | nothing — it reads no repository | n/a |

`audit` and `snapshot` ask what GitHub would do, and GitHub only ever sees committed
files, so an uncommitted edit will not show up in either.

## `sync` and `check`

`sync` runs the whole pipeline — plan, assert, apply, validate — in one step.

`check` reads no repository and writes nothing. It exits `0` for a valid policy, `3` for a
broken one, and never `1` — so under `set -e` a good policy always lets the script
continue and a bad one always stops it. Syntax errors stop at the first one; everything
else (bad enum values, ops that can't carry the `on_zero_match` you gave them, a
`remove_owner` with no `on_empty`) is reported all at once, because fixing a generated
40-op policy one error per run is miserable.

| Flag | Meaning |
|---|---|
| `--op` / `--policy` | Where the ops come from. Mutually exclusive; passing both or neither is exit 3. |
| `--repo` | Local git repository. Default `.`. |
| `--branch` | Ref whose tracked tree governs resolution (S-7). Default `HEAD`. |
| `--file` | CODEOWNERS path override, repo-relative. |
| `--on-empty` | Policy when `remove_owner` empties an owner set. Allowed only with `--op`; with `--policy`, set `on_empty` in the file instead. |
| `--create` | Write CODEOWNERS if the repo has none. Off by default; never overwrites an existing file. |
| `--dry-run` | Makes no change to CODEOWNERS. `--out` and `--summary-out` still emit. |
| `--format` | `text` (default) or `json`. Under `json`, stdout is data and stderr is logs. |
| `--out` | Write the JSON record here instead of stdout. |
| `--summary-out` | Markdown rendering, for a PR body. |

`--out`, `--summary-out` and `plan --out` are trusted operator paths: they are overwritten
without asking, and they are *not* contained to `--repo`. Unlike `--file` and the
discovered CODEOWNERS path, no repository can influence them.

Three different things in this tool are called "policy". `--policy` is your ops file — and
that file is always "the policy file". `--on-empty` and `on_zero_match` are per-situation
rules the tool follows.

## Policy file fields

| Field | Where | Required | Meaning |
|---|---|---|---|
| `version` | top | yes | Format version. `1`. |
| `ops` | top | yes | Op strings, or objects. A bare string is shorthand for `{"op": "..."}` with everything else defaulted. |
| `name`, `description` | top | no | Surfaced in `--summary-out`, so PR reviewers know why. |
| `on_empty` | top | if any `remove_owner` | `error` \| `inherit` \| `unowned` |
| `op` | per op | yes | Op string, same syntax as `--op`. |
| `id` | per op | no | Short label used in JSON results and error messages. |
| `on_zero_match` | per op | no | `require` (default) \| `skip` \| `declare` |
| `note` | per op | no | Reaches the PR reviewer via `--summary-out`. |

Unknown fields are a hard error — a typo'd `on_zero_mtach` that silently fell back to the
default would apply the wrong policy to every repo at once. JSON has no comments, so keys
beginning with `_` (and the key `//`) are always ignored and can hold one.

`on_zero_match` is rejected on `rename_owner` (its scope comes from current ownership, not
a pattern) and `declare` is rejected on `remove_owner` (there is no rule to write).

Ops in one batch must **commute**. Two ops whose scopes overlap on a path and whose order
would change the outcome are refused rather than resolved by position (R-8):

```
error: ops "set_owners(*, [@org/everyone])" and "add_owner(/services/api/, @org/api-team)"
overlap on "services/api/main.go" and do not commute (R-8: refusing order-dependent batch)
```

`add_owner` ops commute with each other, so any number of them can share a run. A
`set_owners` on a broad scope generally cannot share a run with anything narrower — split
it into two invocations.

## JSON output

Real `sync --format json` output, abridged only in `changes`:

```json
{
  "repo": "work/org/foo",
  "codeowners_path": ".github/CODEOWNERS",
  "status": "applied",
  "ops": [
    {"op": "add_owner(/services/api/, @org/api-team)", "status": "applied", "proven": "tree"},
    {"id": "tf", "op": "add_owner(**/*.tf, @org/infra)", "status": "skipped",
     "reason": "scope \"**/*.tf\" matches zero tracked files and on_zero_match=skip (R-21)"},
    {"id": "ci", "op": "add_owner(/.github/workflows/, @org/ci)", "status": "applied",
     "proven": "structural"}
  ],
  "ops_applied": 2, "ops_skipped": 1, "paths_changed": 37,
  "created": false, "changes": [ ]
}
```

`status` is `applied`, `unchanged`, `skipped`, `refused`, or `error`. `proven` is `tree`
when the result was checked against real files, `structural` when it wasn't — see
[below](#what-declare-costs).

`codeowners_path` is the file this run wrote — one of the three locations in S-8, and
which one differs per repo — so a fleet loop can stage exactly it instead of `git add -A`.
It is **absent** when no file was chosen at all (an unreadable repository, or a missing
CODEOWNERS without `--create`), so its presence means there is something to commit. Under
`--create` and `--dry-run` it is the path that *would* be written. See
[FLEET.md](FLEET.md#committing-the-change-and-opening-the-pr).

`warnings` carries what a human should look at in a repo that nonetheless converged: a
second CODEOWNERS file GitHub ignores (A-10), a run writing a file that is not the one
GitHub reads, and lines GitHub cannot parse and silently skips (S-3). None of these is a
reason to refuse a correct edit, and none of them is visible at fleet scale unless the run
that touched the file reports it.

Each entry in `changes` carries the reason the edit took the shape it did, which is the
part a reviewer wants:

```json
{
  "action": "insert", "line": 2, "pattern": "/services/web/",
  "old_owners": ["@org/everyone"],
  "new_owners": ["@org/everyone", "@org/web-team"],
  "new_line": "/services/web/ @org/everyone @org/web-team",
  "reason": "rule \"*\" also governs out-of-scope paths; inserted narrowing rule \"/services/web/\" immediately after it so out-of-scope resolution is untouched (R-2)"
}
```

Two things to know before you write `jq` against this. `id` appears only on ops your
policy named, so key on it only where you set it. And **`ops_applied` + `ops_skipped`
doesn't have to equal your op count** — an op that was already satisfied is `unchanged`
and counted by neither. Keys with nothing in them are **omitted entirely** rather than
emitted empty, which applies to `ops`, `warnings` and `changes`; guard with `// []`. See
[FLEET.md](FLEET.md#the-jq-habit-worth-having) for the aggregation recipes.

## `plan` and `apply`

```
intent (ops) ──▶ PLAN ──▶ ASSERT ──▶ APPLY ──▶ VALIDATE
                  │         │
                  │         └── gate; refuses on violation
                  └── resolves ownership before/after over the real git tree
```

`sync` runs that whole pipeline in one step. Run the halves separately when you want the
reviewable artifact in the middle — a JSON plan with resolved ownership per path plus the
literal line diff:

```sh
codeowners-tool plan --op 'add_owner(/services/api/, @org/team-1)' --out plan.json
codeowners-tool apply --plan plan.json
```

A plan records `sha256_before`, `size_before`/`size_after`, `changes`, `ownership_rows`,
`diff`, `after_content` and `op_results`. `sha256_before` is the drift gate (R-16):
`apply` hashes the file it is about to write and refuses if it no longer matches, so a
plan reviewed against one state cannot be applied to another.

`snapshot` and `verify` are the after-the-fact version of the same question — prove in CI
that a merged change moved nothing outside its declared scope:

```sh
codeowners-tool snapshot --branch main    --out before.json
codeowners-tool snapshot --branch feature --out after.json
codeowners-tool verify --before before.json --after after.json --scope /services/api/
```

## Operations

Scope is a directory, file path, or glob — same syntax as CODEOWNERS patterns.

| Op | Meaning |
|---|---|
| `add_owner(scope, owner)` | Owner becomes a **co-owner**; every pre-existing owner of every path in scope is retained. |
| `set_owners(scope, [owners])` | Exact owner set for every path in scope, displacing prior owners. `[]` is legal: it deliberately un-owns the scope. |
| `remove_owner(scope, owner)` | Owner stops owning every path in scope. If a rule's owner set would empty, an `--on-empty` policy is **required**. |
| `rename_owner(old, new)` | Global identifier substitution — the only op safe as pure text replacement (it can't change any rule's match set). |

## The two invariants

- **INV-1 (in scope):** after apply, every in-scope path resolves to exactly what the op
  requires.
- **INV-2 (out of scope):** after apply, every out-of-scope path resolves to exactly what
  it did before. **This is the product.**

The planner synthesizes line edits, then *proves* the result by re-resolving every file
git knows about at `--branch` and comparing against an independently computed desired
state. Anything unprovable → refusal, nothing written. Plans are idempotent (re-running is
a no-op) and preserve every untouched byte — comments, blank lines, spacing, ordering.

When there is no line that satisfies both invariants, the refusal names the rule in the
way:

```
error: refusing: rule "infra/" also governs paths outside scope "**/*.tf", and no sound
narrowing pattern is derivable — amending would violate INV-2, appending would violate INV-1
```

## What `declare` costs

`declare` is the one place a guarantee weakens:

- **INV-2 is unaffected.** A pattern that matches nothing in the repo cannot move any
  existing file's ownership. Proven exactly as usual.
- **INV-1 weakens.** Your repo has no files matching the pattern, so there is nothing to
  check the rule against — the tool cannot prove the rule does what you meant. It proves
  the next best thing: that no rule after it can override it, which it guarantees by
  putting the rule at the end of the file. When someone later adds a matching file, this
  rule takes it. If that wasn't what you wanted, nothing will have caught it.

Ops that took this path report `"proven": "structural"` in the JSON and are called out in
`--summary-out`, so a reviewer can find them without reading the diff.

## `--on-empty` / `on_empty` (R-6)

Removing the sole owner of a rule needs an explicit policy — **there is no default**, and
the documented recommendation is `error`:

- `error` — refuse (recommended: consistent with the tool's fail-closed posture)
- `inherit` — delete the rule; the preceding broader rule takes over (removal **cascades**
  if the fallthrough rule also lists the owner)
- `unowned` — keep the pattern with zero owners (GitHub's sanctioned substitute for `!`
  negation)

Under `inherit`/`unowned` the resulting reassignment is shown in the plan's ownership rows.

## Audit checks

Read-only. **Never writes** — where a fix is expressible it emits op strings for a human
to review and run through `plan`/`apply`, the system's single writer path.

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

Run a subset with `--checks a1,a3,a6` (`a4`, `A4`, `a-4` and `A-4` are all accepted; an
unrecognized name is a hard error, because silently matching nothing would make audits
pass vacuously). Requesting A-1/A-2/A-3 without both `--token` and `--github-repo` is exit
5, not a silent skip.

**`--fail-on` sets the gate, not the report.** Every finding is printed under every
setting; the flag decides which of them make the run exit 4.

| `--fail-on` | Exits 4 when a finding is |
|---|---|
| `any` *(default)* | any severity — the behavior this flag was added under |
| `warning` | `warning` or `error`; `info` (A-9 coverage) reports only |
| `error` | `error` only — A-1, A-3, A-8, A-10, and A-12 over the cliff |
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

## Exit codes

`sync` uses a coarse three-code contract — its question is "did this repo converge?" — and
returns exactly `0`, `2`, or `3`, never anything else. Every other command uses the
precise taxonomy below.

**The two tables do not use the same numbers for the same things**, so don't read across.
`sync` maps the precise codes onto its own by asking a single question — *is this about
the policy, or about this repo?*

| Precise code | Under `sync` | Why |
|---|---|---|
| 1 no-op | **0** | "Already correct" is the common fleet outcome; special-casing it defeats the point |
| 2 refused | **2** | This repo's file has an awkward shape |
| 3 zero-match scope | **2** | Whether a path exists is the most repo-specific fact there is |
| 3 malformed op, bad policy | **3** | Will fail identically on all 100 |
| 6 rolled back | **2** | A rolled-back write is about that one repo, not your policy |

`sync` makes no network calls, so it never returns 4 or 5.

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

## Design decisions

Resolved from the specification's open list.

1. **`--on-empty` recommendation:** `error`.
2. **Performance:** naive full-tree resolve (twice per plan). Fine to ~100k files; a
   pattern-level pre-filter is a straightforward later optimization.
3. **GitHub Enterprise Server:** in, via `--api-url`; endpoint gaps degrade to exit 5, not
   wrong answers.
4. **Auth:** PAT only for v1 (`--token` / `$GITHUB_TOKEN`). GitHub App is the right v2
   answer for org-wide automation.
5. **Branch handling:** local repo, any ref via `--branch` (git `ls-tree`); plan/apply read
   and write the working-tree file, resolve against the ref's tree.
6. **Fleet automation:** the loop stays in your script. Cloning, auth, hosts, parallelism
   and retries are solved by `gh`/`ghorg`; the tool stays single-repo and composes with
   them.

## Prior art

[hmarr/codeowners](https://github.com/hmarr/codeowners) (reference matcher),
[mszostok/codeowners-validator](https://github.com/mszostok/codeowners-validator) (check
taxonomy; also a cautionary tale — it mixes three glob engines),
[snyk/github-codeowners](https://github.com/snyk/github-codeowners) and
[beaugunderson/codeowners](https://github.com/beaugunderson/codeowners) (both built on the
npm `ignore` gitignore engine, whose `!`/`[ ]` support diverges from GitHub — exactly the
trap S-2 warns about),
[toptal/codeowners-checker](https://github.com/toptal/codeowners-checker) (archived; the
only prior mutation tool — it reorders lines, which R-1 forbids).
