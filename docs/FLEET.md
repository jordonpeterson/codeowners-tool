# Rolling a policy across many repos

The tool works on one repo at a time and never clones, commits, branches, or opens PRs.
Cloning, auth, hosts, parallelism and retries are already solved by `gh` and `ghorg`, so
the loop stays in your script and the tool stays composable with whichever you use.

## The idea, in five lines

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

Then:

```bash
while read -r repo; do
  gh repo clone "$repo" "work/$repo" -- --depth 1 -q
  codeowners-tool sync --repo "work/$repo" --policy policy.json --create
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

```sh
codeowners-tool check --policy policy.json
```

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
non-error reason. Re-run it whenever you edit the policy; it's the only step that catches
a mistake before it reaches a repo.

## Committing the change and opening the PR

The tool never touches git, so the commit is yours to make — and *what to stage* is the
part that differs per repo. Ownership lives in `.github/CODEOWNERS`, the root
`CODEOWNERS`, or `docs/CODEOWNERS`, whichever GitHub would load, and across a hundred
repos it is all three. Every record names the file that run actually wrote, under
`codeowners_path`:

```bash
file=$(jq -r '.codeowners_path // empty' "records/${repo//\//__}.json")
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
if you wrote it inside the clone — out of a hundred PRs. The field is **absent** when no
file was chosen, which is why the `// empty` guard is there: a repo that refused has
nothing to stage, and `jq -r` would otherwise hand you the string `null` as a path.

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

Three conditions exit 0 and report `applied` — they are not reasons to refuse a correct
edit — but each one leaves something a human should look at, so the run that touched the
file says so in `warnings`:

- **A second CODEOWNERS file.** The right one was edited; the other still sits in the repo
  looking authoritative and usually says something different (A-10).
- **The file being edited is not the one GitHub reads.** Almost always a `--file` pointed
  at the wrong location. The rules land in a file GitHub never loads, so the rollout
  reports success and moves no ownership at all.
- **Lines GitHub cannot parse.** It skips them individually and honors the rest (S-3), so
  the change is correct and those lines are left exactly as they were — but some paths in
  that repo are owned by nobody, and the reason is a line this run just read.

```sh
jq -r 'select(.warnings) | "\(.repo)\t\(.warnings|join("; "))"' results.jsonl
```

## What's left at the end

Two piles.

**`clone-failed`** is infrastructure — re-run the loop against just that list.

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
empty — that applies to `ops`, `warnings` and `changes`. A refused repo has no `.ops` at
all, so the same query without the guard dies with `Cannot iterate over null` on the first
repo that needed a human, which is the one you most wanted to see.

Full JSON shape: [REFERENCE.md](REFERENCE.md#json-output).
