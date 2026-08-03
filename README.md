# codeowners-tool

Make safe, provable changes to GitHub CODEOWNERS files — in one repo, or across a
hundred.

**The problem.** CODEOWNERS is written in *lines*, but what anyone cares about is *who
owns which file*. The two are connected by rules that surprise people: the **last**
matching line wins, and owner sets don't combine — appending `/x/ @team-2` **replaces**
the owners of `/x/`, it doesn't add to them.

So you say what you want — "`@team-2` should co-own `/services/api/`" — and this tool
works out the lines. Then it checks its own work against every file in your repo, and
**refuses to write anything it can't prove correct.**

It also reads: `codeowners-tool audit` finds owners who've left the company, rules that
match no files, and owners who don't actually have permission to approve. Auditing
never writes anything.

Works with github.com and GitHub Enterprise Server.

## Install

**Homebrew** (macOS/Linux):

```sh
brew install jordonpeterson/tap/codeowners-tool
```

**Install script** (macOS/Linux) — downloads the right prebuilt binary, verifies its
checksum, and installs it:

```sh
curl -fsSL https://raw.githubusercontent.com/jordonpeterson/codeowners-tool/main/install.sh | sh
```

Set `VERSION=vX.Y.Z` to pin a release or `BINDIR=~/.local/bin` to change the install
location.

**Direct download**: grab the archive for your platform from the
[latest release](https://github.com/jordonpeterson/codeowners-tool/releases/latest),
verify it against `checksums.txt`, extract, and put `codeowners-tool` on your `PATH`.
Every release ships Linux, macOS, and Windows builds for both amd64 and arm64.

> **macOS note:** the binaries are not notarized, so a build downloaded through a
> browser is quarantined by Gatekeeper. Clear it with
> `xattr -d com.apple.quarantine ./codeowners-tool`. Homebrew and the install script
> above are unaffected — neither quarantines its downloads.

**From source** with Go 1.24+:

```sh
go install github.com/jordonpeterson/codeowners-tool/cmd/codeowners-tool@latest
```

## Change one repo

Say your `.github/CODEOWNERS` looks like this:

```
*                 @org/everyone
/services/api/    @org/api-team
```

You want `@org/team-1` to co-own the API directory. Run this inside the repo:

```console
$ codeowners-tool sync --op 'add_owner(/services/api/, @org/team-1)'
1 line changed, 12 paths change owners, 64 → 78 bytes
```

and the file is now:

```
*                 @org/everyone
/services/api/    @org/api-team @org/team-1
```

That's the whole thing. Note what *didn't* happen: `@org/api-team` is still there.
Adding that line by hand as `/services/api/ @org/team-1` would have silently replaced
them — which is the mistake this tool exists to make impossible.

Run it again and nothing happens; it's already true. Add `--dry-run` to see the change
without making it. If the repo has no CODEOWNERS at all, add `--create` and one is
written at `.github/CODEOWNERS`.

## Roll a policy out across your org

Put the ops in a file once:

```json
{
  "version": 1,
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    "add_owner(/.github/workflows/, @org/ci)"
  ]
}
```

Each of those lines is an **op** — one intent, same syntax as `--op` above.

The tool works on one repo at a time and doesn't clone anything, so you write the loop:

```bash
while read -r repo; do
  gh repo clone "$repo" "work/$repo" -- --depth 1 -q
  codeowners-tool sync --repo "work/$repo" --policy policy.json --create
done < repos.txt          # one "org/name" per line
```

That's the whole idea, and it is genuinely all you need for a first pass. Don't use it
for a real 100-repo rollout, though: it stops dead the first time a clone fails or a
repo needs a human. [Fleet scripting](#fleet-scripting) below is the version that
survives both, records what happened, and can be resumed.

Your 100 repos aren't identical, though, so an op can say what to do when nothing in
that repo matches it. Write it as a plain string until it needs to say something extra,
then swap in an object — both forms can sit in the same list:

```json
{
  "version": 1,
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    { "op": "add_owner(**/*.tf, @org/infra)",          "on_zero_match": "skip"    },
    { "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare" }
  ]
}
```

| `on_zero_match` | What happens when nothing in the repo matches |
|---|---|
| `require` *(default)* | Treat it as a problem. This repo gets no changes and exits 2; your script records it and carries on to the next one. Use it for paths every repo really does have. |
| `skip` | Move on. "*If* this repo has Terraform, `@org/infra` owns it." |
| `declare` | Write the rule anyway, at the end of the file, ready for files added later. |

`declare` is how you get an identical baseline into every repo without editing it again
each time someone adds a file — at the cost of a weaker guarantee, explained
[below](#what-declare-costs).

Check a policy before you run it anywhere:

```sh
codeowners-tool check --policy policy.json
```

`check` reads no repo and writes nothing. It catches the problems that would fail
identically on all 100 repos, so you find them once instead of a hundred times.

## When it can't do what you asked

It refuses, tells you which rule was in the way, and **writes nothing**:

```console
$ codeowners-tool sync --op 'add_owner(/services/api/, @org/team-1)'
error: refusing: rule "*" also governs paths outside scope "/services/api/", and no
sound narrowing pattern is derivable — amending would violate INV-2, appending would
violate INV-1
$ echo $?
2
```

`INV-1` is "the files you named end up owned the way you asked"; `INV-2` is "every
other file in the repo ends up exactly as it was." In English, then: a later `*` rule
already covers everything, so any line added for `/services/api/` would just get
overridden (breaking INV-1) — and reordering the file to fix that would move files you
never mentioned (breaking INV-2). There is no line the tool can write that does what
you asked and nothing else, so it writes none. That's a normal, expected outcome for
some repos; the tool *fails closed* and would rather stop than guess.

Across a fleet that means your script records the handful that stopped and carries on.
`sync` returns exactly three codes, never anything else:

| Exit | Meaning | In a fleet script |
|---|---|---|
| 0 | Done — changed it, or it was already correct | continue |
| 2 | **This repo** needs a human | record it, continue |
| 3 | **The policy** is broken — it'll fail the same way everywhere | stop the run |

That split is the whole contract: exit 3 is only ever for problems that have nothing to
do with which repo you're standing in. `check` catches exactly that class, which is why
running it first is worth the two seconds.

---

# Reference

> Throughout this section, IDs like `R-6`, `S-4`, `INV-2` and `A-9` are numbered
> requirements from the specification. Each is enforced by a named test — see
> [docs/BEHAVIOR.md](docs/BEHAVIOR.md), which is generated from the test suite. You
> never need them to use the tool; they're there so every claim below is traceable to
> something that's actually checked.

## Fleet scripting

```bash
#!/usr/bin/env bash
set -euo pipefail

codeowners-tool check --policy policy.json     # fail on repo 0, not 100 times
mkdir -p work bodies
touch done.txt

while read -r repo; do                         # repos.txt: one "org/name" per line
  grep -qxF "$repo" done.txt && continue       # resume: skip what already finished

  # Clone failures are infrastructure, not policy — record and keep going, or one
  # rate-limited clone at repo 40 ends the run.
  rm -rf "work/$repo"                          # so a re-run doesn't clone onto itself
  if ! gh repo clone "$repo" "work/$repo" -- --depth 1 -q 2>>clone-errors; then
    echo "$repo" >> clone-failed
    continue
  fi

  code=0
  codeowners-tool sync --repo "work/$repo" --policy policy.json --create \
    --format json --summary-out "bodies/${repo//\//__}.md" >> results.jsonl || code=$?
  case $code in
    0) ;;                                      # converged
    2) echo "$repo" >> needs-human ;;          # this repo, not the policy
    *) exit "$code" ;;                         # policy broken — stop
  esac
  echo "$repo" >> done.txt
done < repos.txt

jq -s 'group_by(.status)|map({status:.[0].status, n:length})' results.jsonl
wc -l done.txt needs-human clone-failed 2>/dev/null || true
```

`check` exits `0` for a valid policy and `3` for a broken one — and never `1`. That
matters under `set -e`: a valid policy lets the script proceed, a broken one stops it
before the first clone, and there's no third case where a fine policy halts you for a
non-error reason. Re-run it whenever you edit the policy; it's the only step that
catches a mistake before it reaches a repo.

Two piles are left at the end. `clone-failed` is infrastructure — re-run the loop
against just that list. `needs-human` is the interesting one: for each repo, run
`sync --dry-run` locally to see the refusal, then either restructure that repo's
CODEOWNERS so the intent becomes expressible (usually: replace the over-broad rule the
error names with narrower ones), or accept that this repo is a legitimate exception and
drop it from `repos.txt`.

The tool does not clone, commit, branch, or open PRs — that stays your script's job.
`--format json` prints one line per repo so `jq` can aggregate the fleet;
`--summary-out` writes a markdown summary for a PR body (keep it outside the clone, or
`git add -A` will commit it). Add `--dry-run` for a preview of the whole fleet: it
changes no CODEOWNERS file, but still emits the JSON and the summaries so there's
something to review.

One `jq` habit worth having: project `.ops_skipped` too. A policy with one typo'd path
prefix skips on every repo, and grouping on `.status` alone shows a reassuring wall of
`skipped` rows that you might read as success.

## `sync` and `check`

```
sync   (--op 'OP' ... | --policy FILE) [--on-empty error|inherit|unowned]
       [--repo DIR] [--branch REF] [--file PATH]
       [--create] [--dry-run]
       [--format text|json] [--out FILE] [--summary-out FILE]

check  (--op 'OP' ... | --policy FILE) [--format text|json]
```

`check` reads no repository and writes nothing. It exits `0` for a valid policy, `3`
for a broken one, and never `1` — so under `set -e` a good policy always lets the
script continue and a bad one always stops it. Syntax errors stop at the first one;
everything else (bad enum values, ops that can't carry the `on_zero_match` you gave
them, a `remove_owner` with no `on_empty`) is reported all at once, because fixing a
generated 40-op policy one error per run is miserable.

Note that `--on-empty`, `on_zero_match`, and `--policy` all use the word "policy" for
different things: `--policy` is your ops file, while the other two are per-situation
rules the tool follows. The file is always "the policy file".

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

### Policy file fields

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

Unknown fields are a hard error — a typo'd `on_zero_mtach` that silently fell back to
the default would apply the wrong policy to every repo at once. JSON has no comments, so
keys beginning with `_` (and the key `//`) are always ignored and can hold one.

`on_zero_match` is rejected on `rename_owner` (its scope comes from current ownership,
not a pattern) and `declare` is rejected on `remove_owner` (there is no rule to write).

### JSON output

```json
{
  "repo": "work/org/foo",
  "status": "applied",
  "ops": [
    {"id": "api", "status": "applied", "proven": "tree"},
    {"id": "tf",  "status": "skipped", "reason": "scope matched zero tracked files"},
    {"id": "ci",  "status": "applied", "proven": "structural"}
  ],
  "ops_applied": 2, "ops_skipped": 1, "paths_changed": 37,
  "created": false, "warnings": [], "changes": [ /* ... */ ]
}
```

`status` is `applied`, `unchanged`, `skipped`, `refused`, or `error`. `proven` is
`tree` when the result was checked against real files, `structural` when it wasn't —
see [below](#what-declare-costs).

## `plan` and `apply`

```
intent (ops) ──▶ PLAN ──▶ ASSERT ──▶ APPLY ──▶ VALIDATE
                  │         │
                  │         └── gate; refuses on violation
                  └── resolves ownership before/after over the real git tree
```

`sync` runs that whole pipeline in one step. If you want the reviewable artifact in the
middle — a JSON plan showing resolved ownership per path plus the literal line diff —
run the two halves separately:

```sh
codeowners-tool plan --op 'add_owner(/services/api/, @org/team-1)' --out plan.json
codeowners-tool apply --plan plan.json
```

Other commands:

```sh
# Audit a repo for rot:
GITHUB_TOKEN=... codeowners-tool audit --github-repo org/repo --format json

# Prove in CI that a change touched nothing outside its declared scope:
codeowners-tool snapshot --branch main --out before.json
codeowners-tool snapshot --branch feature --out after.json
codeowners-tool verify --before before.json --after after.json --scope /services/api/
```

## Operations (mutation)

Scope is a directory, file path, or glob — same syntax as CODEOWNERS patterns.

| Op | Meaning |
|---|---|
| `add_owner(scope, owner)` | Owner becomes a **co-owner**; every pre-existing owner of every path in scope is retained. |
| `set_owners(scope, [owners])` | Exact owner set for every path in scope, displacing prior owners. `[]` is legal: it deliberately un-owns the scope. |
| `remove_owner(scope, owner)` | Owner stops owning every path in scope. If a rule's owner set would empty, an `--on-empty` policy is **required** (see below). |
| `rename_owner(old, new)` | Global identifier substitution — the only op safe as pure text replacement (it can't change any rule's match set). |

### The two invariants

- **INV-1 (in scope):** after apply, every in-scope path resolves to exactly what the
  op requires.
- **INV-2 (out of scope):** after apply, every out-of-scope path resolves to exactly
  what it did before. **This is the product.**

The planner synthesizes line edits, then *proves* the result by re-resolving every file
git knows about at `--branch` and comparing against an independently computed desired
state. Anything unprovable → refusal, nothing written. Plans are idempotent (re-running
is a no-op) and preserve every untouched byte — comments, blank lines, spacing,
ordering.

### What `declare` costs

`declare` is the one place a guarantee weakens:

- **INV-2 is unaffected.** A pattern that matches nothing in the repo cannot move any
  existing file's ownership. Proven exactly as usual.
- **INV-1 weakens.** Your repo has no files matching the pattern, so there is nothing
  to check the rule against — the tool cannot prove the rule does what you meant. It
  proves the next best thing: that no rule after it can override it, which it
  guarantees by putting the rule at the end of the file. When someone later adds a
  matching file, this rule takes it. If that wasn't what you wanted, nothing will have
  caught it.

Ops that took this path report `"proven": "structural"` in the JSON and are called out
in `--summary-out`, so a reviewer can find them without reading the diff.

### `--on-empty` / `on_empty` (R-6)

Removing the sole owner of a rule needs an explicit policy — **there is no default**,
and the documented recommendation is `error`:

- `error` — refuse (recommended: consistent with the tool's fail-closed posture)
- `inherit` — delete the rule; the preceding broader rule takes over (removal
  **cascades** if the fallthrough rule also lists the owner)
- `unowned` — keep the pattern with zero owners (GitHub's sanctioned substitute for `!`
  negation)

Under `inherit`/`unowned` the resulting reassignment is shown in the plan's ownership
rows.

## Audit — find rot without writing anything

Read-only. **Never writes** — where a fix is expressible it emits op strings for a
human to review and run through `plan`/`apply`, the system's single writer path.

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

**Fail closed (R-12):** a 404 can mean deleted, renamed, invisible to the token, or
rate-limited. The client probes org/repo visibility first; anything inconclusive is
reported `unknown`, exits 5, and **never proposes a removal**. An expired token quietly
stripping owners is the worst failure this tool can produce, so it can't. Email owners
are `unverifiable`, never dead (R-13). Removing a sole owner is presented as a
**reassignment** with before → after owners per path, never a bare line deletion
(R-14). Lookups are cached in memory per run and optionally on disk (`--cache-dir`,
`--cache-ttl`).

## Exit codes

`sync` uses the coarse three-code contract described
[above](#when-it-cant-do-what-you-asked) — its question is "did this repo converge?"
and it returns exactly `0`, `2`, or `3`, never anything else. Every other command uses
the precise taxonomy below.

**The two tables do not use the same numbers for the same things**, so don't read
across. `sync` maps the precise codes onto its own by asking a single question — *is
this about the policy, or about this repo?*

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

`sync` deliberately collapses 0 and 1: across a fleet, "already correct" is the common
outcome, and making every caller special-case it defeats the point. It also folds 6
into 2 — a rolled-back write is a problem with that one repo, not with your policy.

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
2. **Performance:** naive full-tree resolve (twice per plan). Fine to ~100k files; a
   pattern-level pre-filter is a straightforward later optimization.
3. **GitHub Enterprise Server:** in, via `--api-url`; endpoint gaps degrade to exit 5,
   not wrong answers.
4. **Auth:** PAT only for v1 (`--token` / `$GITHUB_TOKEN`). GitHub App is the right v2
   answer for org-wide automation.
5. **Branch handling:** local repo, any ref via `--branch` (git `ls-tree`); plan/apply
   read and write the working-tree file, resolve against the ref's tree.
6. **Fleet automation:** the loop stays in your script. Cloning, auth, hosts,
   parallelism and retries are solved by `gh`/`ghorg`; the tool stays single-repo and
   composes with them.

## Tests as documentation

The test suite is the specification. Every test carries a doc comment naming the spec
requirement it enforces; `make docs` regenerates [docs/BEHAVIOR.md](docs/BEHAVIOR.md)
from them via `go/ast`, so the docs cannot drift from what is verified. Verification
layers:

- **Vendored corpus** (`testdata/patterns.json`) from
  [hmarr/codeowners](https://github.com/hmarr/codeowners) — the actively maintained
  reference implementation whose matcher encodes GitHub's observed divergences from
  gitignore.
- **Differential fuzz** (`make diff-test`): 500k random pattern/path cases against
  hmarr's *unmodified* matcher, vendored verbatim as an oracle.
- **Property tests**: thousands of generated (file, tree, op) cases proving INV-1/INV-2
  and idempotence by independent re-resolution — these caught two real bugs during
  development (removal cascade under `inherit`; self-rename churn).
- **Acceptance tests** from the spec, end-to-end CLI tests in real git repos, and
  mocked-API fail-closed tests.

Build from a checkout with `make build`; `make all` runs vet, tests, build, and docs.

## Prior art this design learned from

[hmarr/codeowners](https://github.com/hmarr/codeowners) (reference matcher),
[mszostok/codeowners-validator](https://github.com/mszostok/codeowners-validator)
(check taxonomy; also a cautionary tale — it mixes three glob engines),
[snyk/github-codeowners](https://github.com/snyk/github-codeowners) and
[beaugunderson/codeowners](https://github.com/beaugunderson/codeowners) (both built on
the npm `ignore` gitignore engine, whose `!`/`[ ]` support diverges from GitHub —
exactly the trap S-2 warns about),
[toptal/codeowners-checker](https://github.com/toptal/codeowners-checker) (archived;
the only prior mutation tool — it reorders lines, which R-1 forbids).

## Non-goals

GitLab/Bitbucket semantics · reordering or reformatting existing files · auto-deleting
rules that match zero files · inventing owners · resolving conflicting batches by
precedence · opening PRs or any git write · iterating over repos (that's your script's
job).

## License

MIT. The pattern matcher is ported from, and differentially tested against,
[hmarr/codeowners](https://github.com/hmarr/codeowners) (MIT, © Harry Marr) — see
NOTICE.
