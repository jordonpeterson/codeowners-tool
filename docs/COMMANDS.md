# Reference: commands

Every command, its flags, and what each exit code means. Policy file fields are in
[POLICY-FILE.md](POLICY-FILE.md); JSON shapes in [JSON.md](JSON.md); `audit`/`lint` in
[AUDIT.md](AUDIT.md).

## Commands

```
sync     (--op 'OP' ... | --policy FILE) [--on-empty error|inherit|unowned]
         [--repo DIR] [--branch REF] [--file PATH] [--create] [--dry-run]
         [--max-paths-changed N] [--format text|json] [--out FILE] [--summary-out FILE]
         [--verify-owners [--token T | $GITHUB_TOKEN] [--api-url URL]]
check    (--op 'OP' ... | --policy FILE) [--format text|json]
         [--verify-owners [--token T | $GITHUB_TOKEN] [--api-url URL]]
plan     --op 'OP' ... [--on-empty error|inherit|unowned]
         [--repo DIR] [--branch REF] [--file PATH] [--out plan.json]
         [--max-size BYTES] [--warn-size BYTES]
         [--verify-owners [--token T | $GITHUB_TOKEN] [--api-url URL]]
apply    --plan plan.json [--repo DIR]
audit    [--checks a1,a3,a6] [--fail-on any|warning|error|never] [--format json|text]
         [--github-repo owner/name] [--token T | $GITHUB_TOKEN] [--api-url URL]
         [--cache-dir D] [--cache-ttl DUR] [--repo DIR] [--branch REF] [--file PATH]
         [--lint [--remove-stale-paths] [--on-empty error|inherit|unowned] [--dry-run]]
lint     --github-repo owner/name [--token T | $GITHUB_TOKEN] [--api-url URL]
         [--remove-stale-paths] [--on-empty error|inherit|unowned] [--dry-run]
         [--policy FILE] [--repo DIR] [--branch REF] [--file PATH] [--format text|json]
snapshot [--repo DIR] [--branch REF] [--file PATH] [--out snap.json]
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
`--file` — and a `--file` the ref does not carry is refused by `audit` and `snapshot`
(exit 3), naming the path rather than echoing git plumbing.

Whenever the file a run writes is not the file S-8 picks, the run says so on stderr and in
`warnings` — `sync`, `plan`, `apply` and `lint` alike (R-24). So does one git has never
recorded: `sync` still edits it, since discovery falls back to the working tree so a job
that created the file yesterday can amend it today. Two shapes are refused (exit 2)
instead: one the repository's ignore rules forbid committing, which no re-run can fix, and
a higher-precedence CODEOWNERS sitting uncommitted beside the tracked one — mid-migration
neither can be edited soundly, so `--file` has to say which.

## `sync` and `check`

`sync` runs the whole pipeline — plan, assert, apply, validate — in one step.

`check` reads no repository and writes nothing. It exits `0` for a valid policy, `3` for a
broken one, and never `1` — so under `set -e` a good policy always lets the script continue
and a bad one always stops it. Syntax errors stop at the first; everything else (bad enums,
ops that can't carry your `on_zero_match`, a `remove_owner` with no `on_empty`) is reported
at once, because fixing a generated 40-op policy one error per run is miserable.

| Flag | Meaning |
|---|---|
| `--op` / `--policy` | Where the ops come from. Mutually exclusive; passing both or neither is exit 3. |
| `--repo` | Local git repository. Default `.` when the flag is absent; an explicitly empty `--repo ""` is refused (exit 3), because that is what a shell produces from an unset variable. |
| `--branch` | Ref whose tracked tree governs resolution (S-7). Default `HEAD`. |
| `--file` | CODEOWNERS path override, repo-relative. A path that is nowhere in the repository is refused naming *that* path, with `--create` offered at it. |
| `--on-empty` | Policy when `remove_owner` empties an owner set. Allowed only with `--op`; with `--policy`, set `on_empty` in the file instead. An unknown value is exit 3, checked before any repository is opened. |
| `--create` | Permission to write a CODEOWNERS if the repo has none — not an instruction to. Off by default, never overwrites, and a run with nothing to write creates nothing (no file, no `.github/`). With `--file`, the file is created at that path instead of `.github/CODEOWNERS` — unless that path outranks the CODEOWNERS this repo is already governed by, which is exit 2, since the new file would supersede it under S-8. Allowed only with `--op`; with `--policy`, set `create` in the file instead (R-34b), or the artifact in git is not the policy that ran. |
| `--max-paths-changed` | R-25 ceiling: refuse (exit 2) if the run would change the owners of more than N paths. Off by default. Allowed only with `--op`; with `--policy`, set `max_paths_changed` in the file. |
| `--verify-owners` | Ask GitHub whether every owner the run would put into force exists, and refuse the whole run (exit 3) if one does not — the check that stops a typo'd team being written as a rule that owns nothing (R-41). Off by default; needs a token. May be passed beside `--policy`, unlike `--create`: it changes nothing that gets written, only whether the run happens. `--verify-owners=false` against a policy that set `verify_owners` to **true** is exit 3, on `check` as well as `sync`. |
| `--token` / `--api-url` | Credential and API base URL for `--verify-owners`. `--token` defaults to `$GITHUB_TOKEN`; GHES needs `/api/v3` on the URL. Environment rather than intent, so both are legal beside `--policy`. |
| `--dry-run` | Makes no change to CODEOWNERS. `--out` and `--summary-out` still emit. |
| `--format` | `text` (default) or `json`. Under `json`, stdout is data and stderr is logs. |
| `--out` | **Also** write the JSON record here — always JSON, whatever `--format` says. Stdout is unaffected, which is what lets a fleet loop append to `results.jsonl` and keep a per-repo record at the same time. (`plan --out` and `snapshot --out` do replace stdout; `sync` does not.) |
| `--summary-out` | Markdown rendering, for a PR body. |

`--out`, `--summary-out` and `plan --out` are trusted operator paths: they are overwritten
without asking, and they are *not* contained to `--repo`. Unlike `--file` and the
discovered CODEOWNERS path, no repository can influence them.

An **unmerged CODEOWNERS is refused** (exit 2) by every verb that reads its bytes to decide
an edit — `sync`, `plan`, `apply`, `lint`. After a conflicted merge, rebase or cherry-pick
the file holds both sides at once (`=======` is a legal zero-owner rule, S-9), so the
"before" ownership is a state no commit ever had. Resolve it and `git add`, then re-run; a
conflict in any *other* file is not refused.

### Proving the owners exist (R-41)

Everything else this tool proves is checked against the repository's own files, which
settles *which paths a rule governs* and says nothing about the other half of the line.
GitHub resolves an owner it does not recognise to nobody and reports no error, so
`add_owner(/services/api/, @org/plaform)` is written, reported `proven: tree`, exits 0 —
and owns nothing. `audit` finds it afterwards (A-1), in a run a clean exit 0 gives nobody
a reason to make.

`--verify-owners`, or `"verify_owners": true` in the policy, asks first:

```console
$ codeowners-tool sync --policy ownership.json --verify-owners
error: @org/plaform cannot be written: team @org/plaform does not exist (deleted or renamed); review requests to it silently do nothing — GitHub resolves an unknown owner to nobody and reports no error, so the rule would be written and own nothing (A-1, R-41)
this is a policy error — it will fail identically on every repo; fix the policy, do not retry
```

Only owners the run puts **into force** are checked: `add_owner` and `set_owners` owners,
and `rename_owner`'s new name. `remove_owner`'s are not — dropping a team that was deleted
is the repair, not the risk. Email owners are **unverifiable, never dead** (R-13): they
resolve through a verified address the API cannot see, so they are written and disclosed
on stderr rather than refused.

The refusal is **exit 3**, decided before the repository is opened: "this owner does not
exist" is a fact about the policy, identical on every clone, so a fleet halts at repo 0.
`check --policy p.json --verify-owners` reaches the same verdict with no repository at all
— one lookup instead of a hundred refusals. `plan --verify-owners` is the same check on
the reviewable half of the pipeline, and the only place it belongs there: a dead owner is
invalid input (exit 3) and an unanswerable lookup is exit 5, both before a plan file
exists. `apply` does not check — it executes a plan a human has already approved.

**Known limitation.** `plan` takes no `--policy`, so there it depends on the call site
passing the flag; only the `sync`/`check` route can put the requirement in a reviewed
artifact. A pipeline that must guarantee the check should run `check --policy p.json
--verify-owners` as its gate.

An owner that *exists* is still not necessarily one GitHub will route a review to. A bare
organization handle (`@acme` rather than `@acme/team`) is refused — CODEOWNERS resolves
only a user, an `@org/team` or an email — as is any other account that is not a user, each
named for what it is. Write access is not checked here; that is `audit`'s A-3, which needs
the repository the token is standing in.

One case is written rather than refused: an account that exists whose *type* the API will
not report. Re-running asks the same server the same question, so refusing would be
permanent rather than fail-closed. It is written and disclosed, like an email owner.

`audit` and `lint` do not yet make the organization-handle judgement: A-1 asks only whether
the account exists, so `audit` will report an `@acme` already in a file as live and `lint`
will not remove it, while `sync --verify-owners` refuses to write one. The asymmetry is
deliberate for now — `lint` DELETES on A-1, and widening what it deletes is a change to
Engine B's contract that belongs in its own review.

A lookup that cannot be answered — rate limit, 5xx, an expired token, or a team 404 seen
by a token that is not an org owner (a secret team returns the same 404 as a deleted one)
— is **not** a licence to write. The run refuses, writes nothing, and says so as something
to re-run rather than as a policy to fix (R-12).

Three things here are called "policy". `--policy` is your ops file, always "the policy
file"; `--on-empty` and `on_zero_match` are per-situation rules the tool follows.

## `plan` and `apply`

```
intent (ops) ──▶ PLAN ──▶ ASSERT ──▶ APPLY ──▶ VALIDATE
                  │         │
                  │         └── gate; refuses on violation
                  └── resolves ownership before/after over the real git tree
```

`sync` runs that whole pipeline in one step. Run the halves separately for the reviewable
artifact in the middle — a JSON plan with resolved ownership per path and the line diff:

```sh
codeowners-tool plan --op 'add_owner(/services/api/, @org/team-1)' --out plan.json
codeowners-tool apply --plan plan.json
```

The size flags are `plan`'s alone. `--max-size` (default 3,000,000) is the S-4 hard cap: a
result over it is refused at exit 2, since GitHub silently ignores a CODEOWNERS past 3 MB.
`--warn-size` (default 2,500,000) only warns (R-9) and still exits 0.

A plan records `sha256_before`, `sha256_after`, `size_before`/`size_after`, `changes`,
`ownership_rows`, `diff`, `after_content` and `op_results`. Two hashes, two different
jobs: `sha256_before` is the drift gate (R-16) — `apply` hashes the file it is about to
write and refuses if it no longer matches, so a plan reviewed against one state cannot be
applied to another. `sha256_after` pins `after_content` itself, so a plan corrupted,
truncated or hand-edited between review and apply is refused rather than written. A plan
carrying no `sha256_after` is refused too: a missing integrity field is not a waived
check. The success line reports the bytes the write actually moved, measured on disk
rather than read back out of the plan, and names the CODEOWNERS by its repo-relative path —
`repo` is recorded absolute so a plan travels, and two runners must log the same line.

A plan is also bound to the **repository and tree** it was computed in. `repo` is recorded
absolute, so a plan applies from any working directory; `apply --repo` naming a different
repository is refused. `tree_sha256` fingerprints the tracked tree and `apply` refuses when
it has moved — `ownership_rows` are facts about one tree, so a colleague's merge under a
reviewed scope would otherwise widen the blast radius after approval. It is bound to the
plan's **ref** too: `apply` refuses (exit 2) off `ref`, the same S-7 rule `sync` and `lint`
enforce, at the verb that writes. To roll one intent across many repos use `sync --policy`;
a plan is per-repository by construction.

`snapshot` and `verify` ([below](#snapshot-and-verify)) are the after-the-fact version of
the same question — prove in CI that a merged change moved nothing outside its declared
scope. The worked run is in [GUIDE.md](GUIDE.md#reviewing-the-change-before-it-lands).

## `snapshot` and `verify`

`snapshot` resolves the CODEOWNERS **committed at `--branch`** (default `HEAD`) against
that ref's tracked tree — an uncommitted edit is invisible to it, exactly as it is to
GitHub. Its `ownership` map keeps the **two ways a path can have no owner** apart: `null`
means no rule matched it — a gap nobody has addressed — while `[]` means a rule matched
and deliberately un-owns it (S-9), a decision someone defended in review. Bytes that are
not valid UTF-8 get an escaped key ([JSON.md](JSON.md#paths-that-are-not-valid-utf-8)).

`--file` decides which CODEOWNERS the map comes from — the one flag that can make
`snapshot` answer about a file GitHub does not read — in place of the S-8 path.

`verify` compares two snapshots and exits `0` when every ownership change falls inside
a declared `--scope` (repeatable), `2` — printing each offending path — when any change
falls outside them (with no `--scope`, any change at all violates), and `3` for a
malformed snapshot, or for a pair with no tracked path in common: nothing in such a pair
is compared, so it could only ever report `ok`. Usually that pair is two repositories —
a fleet loop having named one file wrong. A path present in only one snapshot is a **tree
delta**, reported as `added:`/`removed:` and never a violation: INV-2 preserves what a
path resolved to before, and an added path has no before (R-18).

## Exit codes

`sync` uses a coarse three-code contract — its question is "did this repo converge?" — and
returns exactly `0`, `2` or `3`. Every other command uses the precise taxonomy below.

An exit-3 verdict is reached **before the repository is opened**, so that run writes no
JSON record and neither `--out` nor `--summary-out` — a row for a repo never read would be
a phantom entry. A fleet aggregating `records/*.json` therefore will not see those repos:
the exit code is the signal, and `sync` says so on stderr when either sink was asked for.

**The two tables do not use the same numbers for the same things**, so don't read across.
`sync` maps the precise codes onto its own by one question — *policy, or this repo?*

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
| 5 owner lookup inconclusive (R-41) | **3** | Repo-independent, and a fleet halting once beats 99 more refusals — but the advice line says re-run, not "fix the policy" |
| 3 owner does not exist (R-41) | **3** | Will fail identically on all 100 |

Without `--verify-owners` (and without a policy setting `verify_owners`), `sync` makes no
network calls at all — the default, and what keeps it usable offline. With it, the two
verdicts above are the only ones the network adds, and both land in `sync`'s existing exit
`3`: it has no code for "inconclusive", and inventing one would move a failure between
classes for scripts that already read `3` as "stop".

`check` keeps its own two-code contract for the same reason — `0` for a valid policy, `3`
otherwise, including an owner lookup that could not be answered. `plan` is on the precise
taxonomy and does distinguish them: `3` for an owner that does not exist, `5` for a lookup
that could not be answered — and `3` when a run has both, since the dead owner is settled
whatever the lookup would have said. `apply` runs no check at all; it executes a plan a human has
already approved, and `plan` is where that approval is earned.

| Code | Meaning |
|---|---|
| 0 | Success — applied, or audit found nothing |
| 1 | No-op — nothing to change (never returned by `audit --lint` — see [AUDIT.md](AUDIT.md#lint)) |
| 2 | Refused — would violate INV-1/INV-2, or exceed the 3 MB cap |
| 3 | Invalid input — malformed op, zero-match scope, conflicting batch |
| 4 | Audit findings at or above `--fail-on` (default: any finding) — see [AUDIT.md](AUDIT.md) |
| 5 | Inconclusive — API unavailable, token insufficient, rate limited |
| 6 | Validation failed post-write; rolled back |

