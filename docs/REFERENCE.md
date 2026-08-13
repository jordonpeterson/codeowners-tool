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
- [`lint`](#lint)
- [Exit codes](#exit-codes)
- [GitHub semantics this tool encodes](#github-semantics-this-tool-encodes)
- [Design decisions](#design-decisions)
- [Prior art](#prior-art)

## Commands

```
sync     (--op 'OP' ... | --policy FILE) [--on-empty error|inherit|unowned]
         [--repo DIR] [--branch REF] [--file PATH] [--create] [--dry-run]
         [--max-paths-changed N] [--format text|json] [--out FILE] [--summary-out FILE]
check    (--op 'OP' ... | --policy FILE) [--format text|json]
plan     --op 'OP' ... [--on-empty error|inherit|unowned]
         [--repo DIR] [--branch REF] [--file PATH] [--out plan.json]
apply    --plan plan.json [--repo DIR]
audit    [--checks a1,a3,a6] [--fail-on any|warning|error|never] [--format json|text]
         [--github-repo owner/name] [--token T | $GITHUB_TOKEN] [--api-url URL]
         [--cache-dir D] [--cache-ttl DUR] [--repo DIR] [--branch REF] [--file PATH]
lint     --github-repo owner/name [--token T | $GITHUB_TOKEN] [--api-url URL]
         [--remove-stale-paths] [--on-empty error|inherit|unowned] [--dry-run]
         [--repo DIR] [--branch REF] [--file PATH] [--format text|json]
snapshot [--repo DIR] [--branch REF] [--out snap.json]
verify   --before before.json --after after.json [--scope PATTERN ...]
version  print the build this binary was stamped with
```

**Which file each command reads** is not uniform, and the difference is deliberate:

| Command | CODEOWNERS content from | Ownership resolved against |
|---|---|---|
| `sync`, `plan`, `apply` | the working tree (and written back there) | the tree at `--branch` |
| `audit`, `snapshot` | the tree at `--branch` | the tree at `--branch` |
| `lint` (`audit --lint`) | the working tree (and written back there) | the tree at `--branch` |
| `check` | nothing — it reads no repository | n/a |

`audit` and `snapshot` ask what GitHub would do, and GitHub only ever sees committed
files, so an uncommitted edit will not show up in either. `lint` is the exception, for the
same reason `sync` is: the file it is about to rewrite is the one on disk. Its *path* is
still discovered from `--branch`'s tree, so a CODEOWNERS that is not committed yet needs
`--file`.

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
| `--create` | Permission to write a CODEOWNERS if the repo has none — not an instruction to. Off by default, never overwrites, and a run with nothing to write creates nothing (no file, no `.github/`). With `--file`, the file is created at that path instead of `.github/CODEOWNERS`. |
| `--max-paths-changed` | R-25 ceiling: refuse (exit 2) if the run would change the owners of more than N paths. Off by default. Allowed only with `--op`; with `--policy`, set `max_paths_changed` in the file. |
| `--dry-run` | Makes no change to CODEOWNERS. `--out` and `--summary-out` still emit. |
| `--format` | `text` (default) or `json`. Under `json`, stdout is data and stderr is logs. |
| `--out` | **Also** write the JSON record here — always JSON, whatever `--format` says. Stdout is unaffected, which is what lets a fleet loop append to `results.jsonl` and keep a per-repo record at the same time. (`plan --out` and `snapshot --out` do replace stdout; `sync` does not.) |
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
| `max_paths_changed` | top | no | R-25 ceiling, as a whole number of paths. Zero is legal and asserts the wave changes no ownership at all. |
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

The conflict is caught in **two places, with two different exit codes**, and the split
matters to a fleet:

- **Provable from the op strings alone**, and only when **both ops must apply** (the
  default `on_zero_match: require`) — is exit **3**, from `check` (with no repository at
  all) and from `sync` (before it opens one), identically on every repo whatever its tree
  contains. Such a policy cannot converge anywhere: a repo that has the narrower scope
  refuses on the overlap, and a repo that does not refuses on the zero match, so saying so
  once at repo 0 is strictly better than a hundred times. The remedy is in the error: run
  the displacing op alone first.
- **An op carrying `skip` or `declare`** is never decided here. `skip` means "if this repo
  has it", so the batch is order-dependent only in the repos that do — a fact about the
  tree, and therefore exit 2, per repo. A `declare`d rule lands at EOF where
  last-match-wins settles the outcome, so there is no order ambiguity to refuse.
- **Only visible against a real tree** — two scopes that neither provably contains, which
  happen to meet on a path this repo has — stays exit **2**, per repo, so the fleet loop
  records it and steps to the next clone.

The static half is sound rather than complete: it reports only what `pattern.Contains`
proves, because exit 3 halts a rollout and a false positive there is the expensive
direction.

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

It is present **exactly when `status` is `applied`**: when this run changed the file, or
under `--dry-run` would have. It is absent on `unchanged`, `skipped`, `refused` and
`error`, because none of those wrote a byte and staging a path they named would either
commit nothing (failing the `git commit` that follows, and with it a `set -e` rollout) or
name a file that does not exist.

**Absent means this run wrote nothing — not that no file was chosen.** An `unchanged`
repo has a perfectly good CODEOWNERS; it simply has nothing to stage. And **a `--dry-run`
record reports `applied` for a run that would have written**, so a preview wave emits the
field while the file on disk is untouched: check `dry_run` before staging anything, or
run the commit step only over records from a real wave.

A refusal that got as far as reading the file names it in the `error` string, as
`(governing file: …)` — a refusal in a repo whose ownership lives in `docs/` is a
different conversation from one in `.github/`. Refusals reached before a file was ever
read (a bad `--branch`, no CODEOWNERS and no `--create`) have no file to name, and a
`--create` run does not name a file it was about to invent. See
[FLEET.md](FLEET.md#committing-the-change-and-opening-the-pr).

`warnings` carries what a human should look at in a repo the tool did not refuse over: a
second CODEOWNERS file GitHub ignores (A-10), a run writing a file that is not the one
GitHub reads, lines GitHub cannot parse and silently skips (S-3), and a comment still
naming an owner a `rename_owner` renamed away. None of these is a reason to refuse a
correct edit, and none of them is visible at fleet scale unless the run that touched the
file reports it. They are independent, so a run can carry several at once, and they ride
on any record whose file was read — including a `refused` one, where the warning may be
the more useful half of the row. They are also rendered into `--summary-out`, under **Worth a look** —
the PR is the one moment somebody is already looking at that file and can fix it in the
same commit.

`created` reports what the run did, or under `--dry-run` what it *would* do, so a preview
of a greenfield fleet reports `"created": true` while writing nothing — not even the
parent directory.

A refusal reports `ops_applied` as 0 and carries no `changes` and no `codeowners_path`,
because no byte moved. (`ops_applied` is one of the unconditional keys, so it is emitted
as `0` rather than omitted.) **R-25's ceiling is the one deliberate exception**: it keeps
`paths_changed`, carrying the count it refused over, because a record that refuses on a
number and then omits the number is useless — and unlike other refusals it keeps `ops`,
with every op reported `unchanged`, so `jq` over `.ops[]` still sees one entry per op.

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

A snapshot distinguishes the **two ways a path can have no owner**, and the difference is
the point: `null` means no rule matched it — a gap nobody has addressed — while `[]` means
a rule matched and deliberately un-owns it (S-9), which is a decision someone made and
defended in review. Collapsing them would hide "we chose to leave vendored code unowned"
inside "nobody has looked at this yet".

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

## Creating a file (R-23), and not creating one

`--create` is permission, not instruction. A run whose ops all skip, or that has nothing
to write, creates **no file and no `.github/` directory**, reports `"status": "skipped"`
with `"created": false`, emits no `codeowners_path`, and exits 0. An empty CODEOWNERS
would be worse than none: "which repos still need ownership?" is answered by "which repos
have no CODEOWNERS", and an empty file answers *done* forever.

What a created file contains is exactly one rule line per op that applied — no header, no
provenance comment, no timestamp. The provenance belongs in the PR body (`--summary-out`
names the policy) and in the commit message, both of which are bound to the change that
made them and cannot go stale. A header naming the policy would be a confident lie the
moment wave 2 ran, and a tool that rewrites comments in a file it otherwise never
reformats has given up the guarantee everything else here rests on. A header you write by
hand is preserved byte-for-byte forever, including across re-runs.

Identical inputs produce an identical file: three fresh repos given one policy produce one
byte sequence. A hundred near-identical PRs are only reviewable if that holds.

## The blast-radius ceiling (R-25)

`--max-paths-changed N`, or `max_paths_changed` in a policy, refuses a run that would
change the owners of more than N paths:

```console
$ codeowners-tool sync --op 'add_owner(*, @org/platform)' --max-paths-changed 200
error: refusing: this run would change the owners of 4127 path(s), over the 200-path
ceiling set by --max-paths-changed (R-25) — nothing was written; re-run with `plan --out`
to see which paths, raise the ceiling if the number is right, or narrow the ops if it is not
$ echo $?
2
```

Off by default, because a default ceiling would break every legitimate `set_owners(*, …)`
baseline on upgrade and teach operators to pass an enormous number reflexively. Exit 2,
not 3: how many paths a repo has is the most repo-specific fact there is, so a fleet
records it and carries on. `--dry-run` gives the same verdict, and `--out`/`--summary-out`
still emit — you decide whether to raise the ceiling by reading what it would have done.
The refusal also names the ops behind the number, because the per-op array reports them
all as `unchanged` (nothing applied) and a blocked op would otherwise be indistinguishable
from one that was already satisfied.

The ceiling gates `sync`. `plan`/`apply` is the two-step path where a human reads the
artifact before anything is written, and carries no ceiling of its own — the review *is*
the gate there. A negative value is rejected rather than read as "no ceiling": omit the
flag for that.

The flag is allowed only with `--op`, and the field only in a policy, exactly like
`--on-empty`/`on_empty`. The ceiling is a claim about the intent ("this wave touches
dozens of files per repo, not thousands"), so for a policy run it belongs in the artifact
a reviewer approves; a ceiling in one shell line survives exactly as long as that shell
line. There is no precedence rule to learn because there is no overlap: passing the flag
with `--policy` is exit 3.

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

Read-only **except `audit --lint`** ([below](#audit---lint)). Plain `audit` never writes —
where a fix is expressible it emits op strings for a human to review and run through
`plan`/`apply`. Even under `--lint` the bytes reach disk only through `apply`, which
remains the system's single writer path.

| ID | Check | API | Auto-fix |
|---|---|---|---|
| A-1 | Owner doesn't exist (deleted/renamed user or team) | yes | proposes `remove_owner`; **applied** by `--lint` |
| A-2 | Owner exists but isn't in the org | yes | proposes `remove_owner` |
| A-3 | Owner lacks **explicit write access** (org membership isn't enough) | yes | proposes `remove_owner` |
| A-4 | Rule matches zero tracked files | no | report only by default — a dead pattern may be deliberate intent; deleted only by `--lint --remove-stale-paths` |
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
| `warning` | `warning` or `error`; `info` reports only — A-9 unowned-path coverage, and A-1's `unverifiable` email owners (R-13) |
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

### `lint`

Repairs three of the checks above, over the whole file. **[LINTING.md](LINTING.md)** is
the guide — what each stage does, what it refuses to guess at, the errors and what to do
about them. This is the lookup table.

`audit --lint` is the older spelling and is the same code path; under it `--checks` is
rejected, because a subset of a whole-file repair is ambiguous rather than smaller.

| Flag | Meaning |
|---|---|
| `--github-repo owner/name` | **Required.** Probed, so a token that cannot see the repo stops the run. |
| `--token` / `$GITHUB_TOKEN` | **Required.** Owner existence is not decidable offline. |
| `--dry-run` | Compute and report; write nothing. Exit 4 if anything is pending. |
| `--remove-stale-paths` | Stage 3. Deletes rules matching nothing tracked **and** nothing on disk. |
| `--on-empty error\|inherit\|unowned` | R-6, required only when a removal would empty a rule. `inherit` deletes the line. |
| `--file PATH` | The path is discovered from `--branch`'s tree, so an uncommitted CODEOWNERS needs this. |
| `--repo`, `--branch`, `--api-url`, `--format` | As elsewhere. |

`--cache-dir` and `--cache-ttl` are rejected (exit 3): a cached negative is served without
revalidation, and here that answer deletes an owner rather than printing a finding.
Lookups are still cached in memory per run.

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

**Repository guards**, both exit 2: `--branch` must be the ref the clone has checked out
(lifted by `--dry-run`), and `--repo` must be the repository root (not lifted). Both exist
because lint proves against a tree and writes a file, and those are only the same document
when the two agree.

| Exit | When |
|---|---|
| 0 | Nothing to repair, or written with nothing left over |
| 2 | Refused — `--on-empty=error`, size cap, or either repository guard |
| 3 | Invalid input — missing `--on-empty`, a rejected flag, an empty tree under `--remove-stale-paths`, or hash drift between read and write |
| 4 | Still needs a person — pending fixes under `--dry-run`, an unrepairable line, or a case-only typo |
| 5 | Inconclusive, or no token/`--github-repo`. Nothing written |
| 6 | Post-write validation failed; rolled back |

`lint` never returns 1: a file needing no repair is its success, and "no-op" would make
every healthy repository in a fleet read as a failure under `set -e`.

`--format json` emits one object: `codeowners_path`, `applied`, `dry_run`, `needs_human`,
`exit_code`, `actions[]` (`kind`, `line`, `owner`, `pattern`, `reason`), `unverifiable[]`,
`changes[]`, `ownership_rows[]`, `diff`, `warnings[]`. `needs_human` and `exit_code` come
from the same function that sets the process status, so `jq -e .needs_human` is the gate.
Unlike the `sync` record, `actions`, `changes` and `ownership_rows` are always present
(possibly empty).

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
| 3 non-commuting batch, provable from the patterns | **3** | Same verdict on every repo, reached before one is opened |
| 3 non-commuting batch, only visible in this tree | **2** | Which paths exist is this repo's fact |
| 2 over the R-25 ceiling | **2** | How big this repo is, is this repo's fact |
| 3 malformed op, bad policy | **3** | Will fail identically on all 100 |
| 6 rolled back | **2** | A rolled-back write is about that one repo, not your policy |

`sync` makes no network calls, so it never returns 4 or 5.

| Code | Meaning |
|---|---|
| 0 | Success — applied, or audit found nothing |
| 1 | No-op — nothing to change (never returned by `audit --lint`; see below) |
| 2 | Refused — would violate INV-1/INV-2, or exceed the 3 MB cap |
| 3 | Invalid input — malformed op, zero-match scope, conflicting batch |
| 4 | Audit findings present at or above `--fail-on` (default: any finding) |
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
