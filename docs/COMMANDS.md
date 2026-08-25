# Reference: commands

Every command, its flags, and what each exit code means. Policy file fields are in
[POLICY-FILE.md](POLICY-FILE.md); JSON shapes in [JSON.md](JSON.md); `audit`/`lint` in
[AUDIT.md](AUDIT.md).

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
         [--policy FILE] [--repo DIR] [--branch REF] [--file PATH] [--format text|json]
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
| `--repo` | Local git repository. Default `.` when the flag is absent; an explicitly empty `--repo ""` is refused (exit 3), because that is what a shell produces from an unset variable. |
| `--branch` | Ref whose tracked tree governs resolution (S-7). Default `HEAD`. |
| `--file` | CODEOWNERS path override, repo-relative. |
| `--on-empty` | Policy when `remove_owner` empties an owner set. Allowed only with `--op`; with `--policy`, set `on_empty` in the file instead. An unknown value is exit 3, checked before any repository is opened. |
| `--create` | Permission to write a CODEOWNERS if the repo has none — not an instruction to. Off by default, never overwrites, and a run with nothing to write creates nothing (no file, no `.github/`). With `--file`, the file is created at that path instead of `.github/CODEOWNERS`. Allowed only with `--op`; with `--policy`, set `create` in the file instead (R-34b), or the artifact in git is not the policy that ran. |
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

## `snapshot` and `verify`

`snapshot` resolves the CODEOWNERS **committed at `--branch`** (default `HEAD`) against
that ref's tracked tree — an uncommitted edit is invisible to it, exactly as it is to
GitHub. In the `ownership` map, `[]` means a rule matches the path and deliberately
assigns no owners; `null` means no rule matches it at all.

`verify` compares two snapshots and exits `0` when every ownership change falls inside
a declared `--scope` (repeatable), `2` — printing each offending path — when any change
falls outside them (with no `--scope`, any change at all violates), and `3` for a
malformed snapshot. A path that enters or leaves the tracked tree counts as a change
(R-18), so don't commit the snapshot files themselves between the two snapshots.

## Exit codes

`sync` uses a coarse three-code contract — its question is "did this repo converge?" — and
returns exactly `0`, `2`, or `3`, never anything else. Every other command uses the
precise taxonomy below.

An exit-3 verdict is reached **before the repository is opened**, so that run emits
no JSON record and writes neither `--out` nor `--summary-out`. This is deliberate — a
row for a repo that was never read would be a phantom entry in the aggregation — but
it means a fleet that aggregates `records/*.json` will not see those repos at all.
The exit code is the signal, and `sync` says so on stderr when either sink was asked
for.

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
| 1 | No-op — nothing to change (never returned by `audit --lint` — see [AUDIT.md](AUDIT.md#lint)) |
| 2 | Refused — would violate INV-1/INV-2, or exceed the 3 MB cap |
| 3 | Invalid input — malformed op, zero-match scope, conflicting batch |
| 4 | Audit findings at or above `--fail-on` (default: any finding) — see [AUDIT.md](AUDIT.md) |
| 5 | Inconclusive — API unavailable, token insufficient, rate limited |
| 6 | Validation failed post-write; rolled back |

