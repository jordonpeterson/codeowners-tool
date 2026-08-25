# Rolling a policy across many repos

The tool works on one repo at a time and never clones, commits, branches, or opens PRs.
Cloning, auth, hosts, parallelism and retries are already solved by `gh` and `ghorg`, so
the loop stays in your script and the tool stays composable with whichever you use.

## The idea, in five lines

Put the ops in a file once:

```json
{
  "version": 1,
  "create": true,
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    "add_owner(/.github/workflows/, @org/ci)"
  ]
}
```

`create` lets a repo with no CODEOWNERS get its first one, and never overwrites an
existing file. It belongs in the policy: `--create` is for `--op` runs, and passing it
next to `--policy` is exit 3, so the artifact in git is always the policy that ran.

Then:

```bash
while read -r repo; do
  gh repo clone "$repo" "work/$repo" -- --depth 1 -q
  codeowners-tool sync --repo "work/$repo" --policy policy.json
done < repos.txt          # one "org/name" per line
```

That is genuinely all you need for a first pass. Don't use it for a real 100-repo
rollout, though: it stops dead the first time a clone fails or a repo needs a human.

## Your 100 repos aren't identical

An op can say what to do when nothing in a repo matches it. Write it as a plain string
until it needs to say something extra, then swap in an object — both forms can sit in the
same list:

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
each time someone adds a file — at the cost of a weaker guarantee, explained in
[what `declare` costs](REFERENCE.md#what-declare-costs).

Check the policy before it reaches a single repo:

```console
$ codeowners-tool check --policy policy.json
ok: policy.json — 3 op(s), no policy errors
  ops[0]  on_zero_match: require (built-in)
  ops[1]  on_zero_match: skip
  ops[2]  on_zero_match: declare
```

The echo is each op's *resolved* `on_zero_match`, so a `defaults` block that misses an op
is visible before the first clone rather than after the hundredth.

`check` reads no repo and writes nothing. It catches the problems that would fail
identically on all 100, so you find them once instead of a hundred times.

## The exit-code contract this depends on

`sync` returns exactly three codes, never anything else:

| Exit | Meaning | In a fleet script |
|---|---|---|
| 0 | Done — changed it, or it was already correct | continue |
| 2 | **This repo** needs a human | record it, continue |
| 3 | **The policy** is broken — it'll fail the same way everywhere | stop the run |

That split is the whole contract: exit 3 is only ever for problems that have nothing to do
with which repo you're standing in. `check` catches exactly that class, which is why
running it first is worth the two seconds.

## Nor are the clones

The repos differ; so does the state each clone is handed to the tool in. All of these are
ordinary, and none of them changes a verdict:

| Clone | What the tool does |
|---|---|
| Shallow (`--depth 1`, as above) | Works. Resolution reads the tree at `--branch`, which a shallow clone has. |
| Detached HEAD (any CI checkout) | Works, on the default `--branch HEAD`. |
| Default branch is `master`, or anything else | Works. Nothing here knows the name of your default branch. |
| Freshly created, **no commits at all** | Exit 2, `"status": "error"` — there is no tree to read. Recorded, stepped over. |

`--branch` is the one to be careful with. It names the ref whose tree governs resolution,
but the bytes are the working tree's, so writing while proving against a ref this clone
does *not* have checked out is refused (exit 2, per repo). `--branch main` on a clone
standing on `main` is fine — refs are compared by resolved commit, not by name. If you
need the proof against another ref, add `--dry-run`, or use `plan`.

## The script that survives a real rollout

```bash
#!/usr/bin/env bash
set -euo pipefail

codeowners-tool check --policy policy.json     # fail on repo 0, not 100 times
mkdir -p work bodies records
touch done.txt results.jsonl                   # jq at the end reads it even if every clone failed

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
  codeowners-tool sync --repo "work/$repo" --policy policy.json \
    --format json --summary-out "bodies/${repo//\//__}.md" >> results.jsonl || code=$?
  case $code in
    # done.txt is the resume guard, so ONLY a converged repo goes in it. Marking
    # an exit-2 repo done as well retires it permanently: you fix the repo, re-run,
    # and the loop skips it forever — a rollout you believe is complete and isn't.
    0) echo "$repo" >> done.txt ;;             # converged
    2) echo "$repo" >> needs-human ;;          # this repo, not the policy
    *) exit "$code" ;;                         # policy broken — stop
  esac
done < repos.txt

jq -s 'group_by(.status)|map({status:.[0].status, n:length})' results.jsonl
wc -l done.txt needs-human clone-failed 2>/dev/null || true
```

`check` exits `0` for a valid policy and `3` for a broken one — and never `1`. That
matters under `set -e`: a valid policy lets the script proceed, a broken one stops it
before the first clone, and there's no third case where a fine policy halts you for a
non-error reason. Re-run it whenever you edit the policy; it's the only step that catches
a mistake before it reaches a repo.

## Committing the change and opening the PR

The tool never touches git, so the commit is yours to make — and *what to stage* is the
part that differs per repo. Ownership lives in `.github/CODEOWNERS`, the root
`CODEOWNERS`, or `docs/CODEOWNERS`, whichever GitHub would load, and across a hundred
repos it is all three. Every record names the file that run actually wrote, under
`codeowners_path`:

```bash
# `select(.dry_run|not)`: a preview wave reports `applied` for what it WOULD write, so a
# rehearsal's records must never reach the commit step.
file=$(jq -r 'select(.dry_run|not) | .codeowners_path // empty' "records/${repo//\//__}.json")
[ -n "$file" ] || continue                     # nothing was written; nothing to commit

git -C "work/$repo" checkout -b codeowners-baseline
git -C "work/$repo" add "$file"                # exactly the one file, never -A
git -C "work/$repo" commit -m 'chore: org baseline ownership'
git -C "work/$repo" push -u origin HEAD
gh pr create --repo "$repo" --title 'chore: org baseline ownership' \
  --body-file "bodies/${repo//\//__}.md"
```

That needs `--out "records/${repo//\//__}.json"` on the `sync` line (and a `mkdir -p
records` beside the others), or pipe the same field out of `results.jsonl`. Staging the
named file rather than `git add -A` is what keeps a stray build artifact — or the summary,
if you wrote it inside the clone — out of a hundred PRs.

The `// empty` guard is the load-bearing line. `codeowners_path` is emitted **only when
the run changed the file** (or, under `--dry-run`, would have), so it is absent for the
two outcomes most of your fleet will produce on a second wave — the repo that was already
correct, and the repo the policy had nothing to say about. Without the guard those repos
reach `git add` with nothing staged, `git commit` exits nonzero with "nothing to commit",
and `set -euo pipefail` ends the rollout at a repo that was perfectly fine. `jq -r` would
also hand you the string `null` as a path.

The PR body from `--summary-out` names the same file, so a reviewer looking at one of a
hundred near-identical PRs can see where that repo keeps its ownership before the diff
means anything.

## The CI gate afterwards

Once the baseline is merged, `audit` in each repo's CI keeps it honest — but gate it on
severity, or the rollout you just finished turns the fleet red:

```yaml
- run: codeowners-tool audit --github-repo ${{ github.repository }} --fail-on error
  env:
    GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

Every op with `on_zero_match: declare` writes a rule that matches zero files — that is what
a baseline *is* — and `audit` reports each one as A-4, a `warning` it labels report-only.
With the default `--fail-on any`, the next push to every repo the rollout touched fails
CI. `--fail-on error` still fails on the findings that mean something (a dead owner, an
owner without write access, a second CODEOWNERS file, the 3 MB cliff) and still **prints
every** finding, including the A-4s. It moves the gate, not the report. `--fail-on never`
is the reporting-only mode for a dashboard job.

Inconclusive runs are a separate axis and `--fail-on` does not touch them: an unreachable
API is exit 5 under every setting, because a check that could not run is not a finding
whose severity you can weigh.

## Repos the rollout should tell you about

Four conditions do not stop a run — none is a reason to refuse a correct edit — but each
leaves something a human should look at, so the run that read the file says so in
`warnings`. They are independent, so one repo can carry several, and they ride on any
record whose file was read, including a `refused` one:

- **A second CODEOWNERS file.** The right one was edited; the other still sits in the repo
  looking authoritative and usually says something different (A-10).
- **The file being edited is not the one GitHub reads.** Almost always a `--file` pointed
  at the wrong location. The rules land in a file GitHub never loads, so the rollout
  reports success and moves no ownership at all.
- **Lines GitHub cannot parse.** It skips them individually and honors the rest (S-3), so
  the change is correct and those lines are left exactly as they were — but some paths in
  that repo are owned by nobody, and the reason is a line this run just read.
- **A comment naming an owner you just renamed.** `rename_owner` substitutes owner tokens
  and never edits prose, so `# @org/acq owns the pipeline` survives a rename of
  `@org/acq` and now points at a team that no longer exists. Nothing else finds these —
  `audit` does not read comments, and the handle is gone, so no lookup will trip on it.

```sh
jq -r 'select(.warnings) | "\(.repo)\t\(.warnings|join("; "))"' results.jsonl
```

They also land in the PR body under **Worth a look**, which is where they get fixed: the
PR is the one moment somebody is already reading that file.

## Owning less than you could

A rollout's failure mode is not only "it didn't apply" — it is "it applied to more than
anyone meant". Three things keep a wave narrow:

**`"create": true` in the policy (or `--create` with `--op`) is permission, not
instruction.** A repo where every op skips gets no file,
no `.github/` directory, `"status": "skipped"`, and no `codeowners_path`. Nothing to
commit, nothing to review, and the repo still answers "no CODEOWNERS yet" to whoever asks
next. An empty file would answer *done* forever.

**Unclaimed paths stay unclaimed.** Ownership covers exactly the scopes your ops name;
nothing synthesizes a `*` catch-all to make coverage look complete. Afterwards
`audit --checks a9 --fail-on never` lists what nobody owns, which is a report, not a
failure. In a snapshot, `null` means no rule matched and `[]` means a rule matched and
deliberately owns nobody (S-9) — the difference between a gap and a decision.

**A ceiling on the blast radius.** `max_paths_changed` in the policy (or
`--max-paths-changed N` with `--op`) refuses any repo where the wave would move more
ownership than you expected. It gates `sync`, which is the verb a rollout loops over;
`plan`/`apply` is the deliberate two-step path where a human reads the artifact before
anything is written, and it is not ceilinged:

```json
{ "version": 1, "max_paths_changed": 500, "ops": ["add_owner(/services/api/, @org/api-team)"] }
```

Exit 2, nothing written, and the record keeps `paths_changed` so you can see what it
would have been. Use an absolute number and set it from the intent: a narrow wave should
change dozens of files whether the repo has 500 files or 50,000, so a repo where it wants
4,000 is telling you something. A `*` baseline is the wave that genuinely scales with repo
size — give that one no ceiling, deliberately.

## Revoking one owner from repos whose teams you can't name

"@automated-approvers co-owns `.github/`, but must not review `.github/CODEOWNERS`" is two
disjoint ops, so R-8 takes both in one policy (R-31) — and neither names the teams that
own those repos today:

```json
{ "version": 1,
  "on_empty": "inherit",
  "ops": [
    { "id": "grant",  "op": "add_owner(/.github/ except /.github/CODEOWNERS, @automated-approvers)" },
    { "id": "revoke", "op": "remove_owner(/.github/CODEOWNERS, @automated-approvers)" }
  ] }
```

The `except` alone would not do it: it means *don't touch*, so where the grantee already
owns the carved path through a broader rule that ownership persists — the record's
`excepted` line says so, and revoking is `remove_owner`'s job. The removal writes one
narrower line restating whatever that repo's survivors are, discovered per repo. Where the
automation was the rule's only owner, `inherit` narrows rather than deleting (R-39); the
line still comes from the repo, not the policy.

```sh
jq -r 'select(.status=="applied") | "\(.repo)\t\(.changes[]|select(.action=="insert")|.new_owners|join(","))"' results.jsonl
```

That is the review you actually want before merging 100 PRs: the owners the tool
discovered, per repo, on the line it added. Two refusals are worth expecting in the
`needs-human` pile — a repo whose CODEOWNERS is not at `.github/CODEOWNERS` (the carve-out
does not exist there, R-28: move the file first) and a repo where the carved path would
fall through to no rule at all (nothing to inherit: give it an owner, or state `unowned`
deliberately).

## What's left at the end

Two piles.

**`clone-failed`** is infrastructure — re-run the loop against just that list.

Re-run the whole loop after fixing anything in `needs-human`: converged repos are
skipped by the resume guard, and the repos you fixed are picked up.

**`needs-human`** is the interesting one, and it holds two different jobs. Split it on
`.status` before you start triaging:

```sh
jq -r 'select(.status=="refused") | .repo' results.jsonl   # a CODEOWNERS decision
jq -r 'select(.status=="error")   | .repo' results.jsonl   # a clone that could not be read
```

`refused` means the tool read the repository and declined: run `sync --dry-run` locally to
see the refusal, then either restructure that repo's CODEOWNERS so the intent becomes
expressible (usually: replace the over-broad rule the error names with narrower ones), or
accept that this repo is a legitimate exception and drop it from `repos.txt`. `error`
means it never got that far — an empty repository, a bad `--branch`, a clone that is not a
repository — and belongs with `clone-failed`.

`--format json` prints one line per repo so `jq` can aggregate the fleet. `--summary-out`
writes a markdown summary suitable for a PR body — keep it outside the clone, or
`git add -A` will commit it. Add `--dry-run` for a preview of the whole fleet: it changes
no CODEOWNERS file, but still emits the JSON and the summaries so there's something to
review.

## The `jq` habit worth having

Project `.ops_skipped` too. A policy with one typo'd path prefix skips on every repo, and
grouping on `.status` alone shows a reassuring wall of `skipped` rows that reads like
success.

More generally, `ops_applied + ops_skipped` does **not** have to equal your op count — an
op that was already satisfied is `unchanged` and counted by neither. To ask "is this op
reaching any repo at all", count it out of `.ops[]`:

```sh
jq -s '[.[] | (.ops // [])[]] | group_by(.op) | map({op: .[0].op, n: length,
        applied: (map(select(.status=="applied")) | length)})' results.jsonl
```

Note the `// []`. Keys with nothing in them are **omitted entirely** rather than emitted
empty — that applies to `ops`, `warnings` and `changes`. A repo refused before it was
planned has no `.ops` at all, so the same query without the guard dies with `Cannot
iterate over null` on the first repo that needed a human, which is the one you most wanted
to see. (A repo refused by the R-25 ceiling is the exception: it was planned, so it keeps
`.ops`, every entry reported `unchanged`.)

Full JSON shape: [REFERENCE.md](REFERENCE.md#json-output).
