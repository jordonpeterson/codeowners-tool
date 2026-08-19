# Linting and repairing a CODEOWNERS file

Two commands, and the difference between them is whether they write:

| | `audit` | `lint` |
|---|---|---|
| What it does | Reports 12 checks (A-1 … A-12) | Repairs 3 of them |
| Writes? | **Never** | Yes, unless `--dry-run` |
| Reads | the file committed at `--branch` | the file **in your working tree** |
| Needs a token? | Only for the owner checks | **Always** |

`audit --lint` is the older spelling of `lint` and still works — same code path.

- [Quick start](#quick-start)
- [What `lint` repairs](#what-lint-repairs)
- [What it refuses to repair](#what-it-refuses-to-repair)
- [Wiring it into CI](#wiring-it-into-ci)
- [Exit codes](#exit-codes)
- [Errors you will actually hit](#errors-you-will-actually-hit)
- [Running it on a schedule](#running-it-on-a-schedule)

## Quick start

Always look first. `--dry-run` writes nothing:

```console
$ GITHUB_TOKEN=... codeowners-tool lint --github-repo org/repo --dry-run
lint: 2 fix(es) pending in .github/CODEOWNERS (--dry-run; nothing written)
  [repair-owner-spacing] (line 3) "/x/ @ acme/live" → "/x/ @acme/live"
  [remove-dead-owner] (line 5) @acme/gone removed from "/y/": team @acme/gone does not exist (deleted or renamed); review requests to it silently do nothing
  owners change: y/b.go  {@acme/live, @acme/gone} → {@acme/live}
$ echo $?
4
```

Drop `--dry-run` to write it. Every flag is in
[REFERENCE.md](REFERENCE.md#lint); the ones you will reach for:

| Flag | Why |
|---|---|
| `--dry-run` | Report, write nothing. Exit 4 if anything is pending. |
| `--github-repo owner/name` | Required. Proves the token can see this repo. |
| `--remove-stale-paths` | Also delete rules matching no files. Off by default. |
| `--on-empty error\|inherit\|unowned` | Required *only* if a removal would empty a rule. |
| `--file PATH` | For a CODEOWNERS you have not committed yet. |
| `--format json` | Machine-readable record; carries `needs_human`. |

## What `lint` repairs

Three stages, in this order. The order is the point.

**1. Rejoins `@`handles that whitespace has split.** `/x/ @ org/team` looks like a rule
with an owner. It is not — GitHub cannot parse the line, skips it entirely, and that team
owns nothing while everyone assumes it does. Rejoined spellings:

```
/x/ @ org/team      →  /x/ @org/team
/x/ @org/ team      →  /x/ @org/team
/x/ @ org / team    →  /x/ @org/team
```

This runs **before** any lookup, deliberately: `@` and `org/team` are not two owners that
do not exist, they are one owner nobody has asked about yet. Verifying first would find
neither, delete both, and call it a cleanup.

**2. Removes users and teams that do not exist.** A deleted or renamed team is a review
request that silently goes nowhere. Only *existence* (audit's A-1) — never "not in the
org" (A-2) or "no write access" (A-3), where the right fix is often to grant the access
rather than drop the owner.

**3. Removes rules that match no files** — only with `--remove-stale-paths`. Off by
default because a dead pattern is often deliberate: a directory that lands next week.
Deleting it destroys intent no other record holds, so it takes you saying so. A rule is
stale only if it matches nothing in the committed tree **and** nothing on disk.

## What it refuses to repair

`lint` reports and leaves alone anything it would have to guess at. Two cases you will
see:

**Ambiguous handles.** `@org /team` reads exactly like `@alice /docs` — somebody putting
two rules on one line. Nothing in the file distinguishes them, and guessing wrong hands
`/docs`'s owner every file under `/src`. So only a *visibly broken* handle is rejoined —
one starting from a token that is not already a valid owner — and only until it becomes
one: `@ alice /docs` assembles `@alice` and then stops.

**Case-only typos.** `/Src/` where the directory is `src/` matches nothing, but it is a
typo, not a dead rule. `--remove-stale-paths` spares it and reports it, because deleting
it would silently un-own the files it was aimed at. Fix the casing yourself — the tree's
real casing may not be the naive lowercase, so the tool will not guess it either.

Both are reported and both make the run exit 4.

## Wiring it into CI

Gate on the **dry run**, not on the writing run:

```yaml
- run: codeowners-tool lint --dry-run --github-repo ${{ github.repository }}
  env:
    GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

A *successful write* can also exit 4 — when it fixed what it could and something is still
left for a person — so `lint && git commit` will not do what you want. If you are
auto-committing, ignore the status and check the diff, or use the JSON record.

Scripting the JSON? `--format json` carries `needs_human`, set by the same function that
produces the exit code, so the gate is one expression:

```console
$ codeowners-tool lint --dry-run --github-repo org/repo --format json | jq -e .needs_human
```

Do not write your own gate out of `.changes` — a file whose only problem is a line lint
refuses to guess at has an empty `changes` array and still needs somebody.

## Exit codes

One rule: **0 when the file needs nothing further from a person, 4 when it does.**

| Code | Means |
|---|---|
| 0 | Nothing to repair, or the fixes were written and nothing is left over |
| 2 | Refused — `--on-empty=error`, the size cap, wrong `--branch`, or `--repo` below the repo root |
| 3 | Invalid input — a missing `--on-empty`, a bad flag combination, or the file changed under the run |
| 4 | Still needs a person — pending fixes under `--dry-run`, an unrepairable line, or a case-only typo |
| 5 | Inconclusive — a lookup could not be answered, or no token. **Nothing written** |
| 6 | Post-write validation failed; rolled back |

`lint` never returns 1. A file needing no repair is this command's success, and exiting
"no-op" would make every healthy repository in a fleet read as a failure under `set -e`.

## Errors you will actually hit

**`lint needs a token … and --github-repo`** (exit 5). Owner existence is not decidable
offline, and that is the whole point of the command. It names whichever one you left out.

**`inconclusive: … no owner was removed and nothing was written`** (exit 5). One lookup
could not be answered — rate limit, expired token, an org your token cannot enumerate —
so the *entire* run is held back, including the offline whitespace fixes. Partial
knowledge does not earn a partial edit; re-run when the lookup works and you get the
complete fix as one reviewable diff.

**`… this token is not an owner of that org`** (exit 5). A *secret* team you cannot see
returns the same 404 as a deleted one, so only an org owner's 404 is definitive. Re-run
with an org-owner token, or remove that owner by hand.

**`an explicit --on-empty policy … is required`** (exit 3). Removing a dead owner would
leave a rule with no owners at all, and there is deliberately no default. Pick one:

| `--on-empty` | Effect |
|---|---|
| `unowned` | Keep the pattern with zero owners. A legal, deliberate un-owning. |
| `inherit` | **Delete the rule line** so the preceding broader rule takes over. On a file with nothing broader behind it, this can empty the file. |
| `error` | Refuse the run (exit 2). |

Whatever you pick, the resulting reassignment is listed in the ownership rows.

**`--branch X is not what this clone has checked out`** (exit 2). `lint` proves against
`--branch`'s tree and writes the working-tree file; on a clone standing elsewhere those
are different trees, and a rule can be deleted as stale while the directory sits in your
checkout. Check the branch out, or add `--dry-run`.

**`--repo X is inside the repository rooted at Y`** (exit 2). Pointed below the root, git
reports paths relative to the *root*, so the run would read one CODEOWNERS and rewrite a
different file of the same name — leaving the one GitHub actually loads untouched.

**`--cache-dir is not available with --lint`** (exit 3). A cached "this owner does not
exist" is served without revalidation. Under `audit` that is a finding somebody reads;
here it deletes an owner. Lookups are still cached in memory for the run.

## Running it on a schedule

`lint` is safe to re-run: a second run over its own output is a no-op, byte for byte.

**Do not schedule `lint` and `sync` against overlapping owners.** `sync` adds the owners
your policy names and never asks whether they exist; `lint` removes owners that do not and
knows nothing about your policy. Pointed at the same repository on a timer they will undo
each other forever, each exiting 0, each making a commit. Pick one.

## What it will not do to your file

Nothing here bypasses the invariants the rest of the tool holds to. The edits are proven
against every tracked file before anything is written, then go through the same `apply`
path as `sync` — the hash is pinned so a file that changed under the run is refused, the
result is validated before the write, the write is an atomic rename, and a failure rolls
back. Comments, blank lines, column alignment and CRLF endings all survive; `lint`
corrects ownership, not spelling or layout.

The full guarantee list, with the test that enforces each one, is in
[BEHAVIOR.md](BEHAVIOR.md).
