# Rolling a policy across many repos

The tool works on one repo at a time and never clones, commits, or opens PRs — cloning,
auth, parallelism and retries are `gh` and `ghorg`'s job, so the loop stays in your script.

## One policy for the whole fleet

Put the ops in a file once. Your 100 repos aren't identical, so an op can say what to do
when nothing in a repo matches it — a plain string until it needs an object:

```json
{
  "version": 1,
  "create": true,
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    { "op": "add_owner(**/*.tf, @org/infra)",          "on_zero_match": "skip"    },
    { "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare" }
  ]
}
```

| `on_zero_match` | What happens when nothing in the repo matches |
|---|---|
| `require` *(default)* | A problem: this repo gets no changes and exits 2; your script records it and continues. For paths every repo really does have. |
| `skip` | Move on. "*If* this repo has Terraform, `@org/infra` owns it." |
| `declare` | Write the rule anyway, at the end of the file, ready for files added later. |

`create` is permission, not instruction: a repo with no CODEOWNERS gets its first one, an
existing file is never overwritten, and a repo where every op skips still gets no file
([POLICY-FILE.md](POLICY-FILE.md#creating-a-file-r-23-and-not-creating-one)).

`declare` buys an identical baseline at the cost of a weaker guarantee, plus one trap:
an op declared in some repos and amended in others produces different owner sets. Both,
with the `set_owners` remedy: [what `declare` costs](GUARANTEES.md#what-declare-costs).

`max_paths_changed` (`--max-paths-changed N` with `--op`) ceilings the blast radius: a
repo where the wave would move more ownership than anyone meant is refused — exit 2,
nothing written, and the record keeps `paths_changed` so you can see what it would have
been. Set it from the intent — a narrow wave changes dozens of files whether the repo
has 500 files or 50,000 — and give a `*` baseline no ceiling, deliberately. Details in
[POLICY-FILE.md](POLICY-FILE.md#the-blast-radius-ceiling-r-25).

Check the policy before it reaches a single repo:

```console
$ codeowners-tool check --policy policy.json
ok: policy.json — 3 op(s), no policy errors
  ops[0]  on_zero_match: require (built-in)
  ops[1]  on_zero_match: skip
  ops[2]  on_zero_match: declare
```

The echo is each op's *resolved* `on_zero_match`, so a `defaults` block that misses an op
is visible before the first clone; `check` reads no repo and exits `0` or `3`, never `1`.

## The exit-code contract this depends on

| Exit | Meaning | In a fleet script |
|---|---|---|
| 0 | Done — changed it, or it was already correct | continue |
| 2 | **This repo** needs a human | record it, continue |
| 3 | **The policy** is broken — it'll fail the same way everywhere | stop the run |

`sync` returns exactly these three codes, never anything else. Exit 3 is only ever for
problems that have nothing to do with which repo you're standing in — exactly the class
`check` catches before the first clone.

Any ordinary clone state works — shallow, detached HEAD, any default-branch name; an
empty repo or a mid-merge CODEOWNERS conflict is per-repo exit 2. `--branch` names the
ref whose tree governs resolution while the bytes written are the working tree's, so a
write proving against a ref not checked out here is refused; use `--dry-run` or `plan`.

## The script that survives a real rollout

```bash
#!/usr/bin/env bash
set -euo pipefail

codeowners-tool check --policy policy.json     # fail on repo 0, not 100 times
mkdir -p work bodies records                   # outputs outside the clones, or a later `git add -A` commits them
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
    --format json --summary-out "bodies/${repo//\//__}.md" \
    --out "records/${repo//\//__}.json" >> results.jsonl || code=$?
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

Adding `--dry-run` to the `sync` line rehearses the whole fleet: no CODEOWNERS file
changes, but the same records and summaries to review before the first real wave.

## Committing the change and opening the PR

The tool never touches git, so the commit is yours — and *what to stage* differs per
repo: ownership lives in `.github/CODEOWNERS`, root `CODEOWNERS`, or `docs/CODEOWNERS`,
and across a hundred repos it is all three. Each record names the file it wrote:

```bash
# Present only when the run changed the file — and a --dry-run record reports what it
# WOULD write — so guard both, or `set -e` ends the rollout on an already-correct repo.
file=$(jq -r 'select(.dry_run|not) | .codeowners_path // empty' "records/${repo//\//__}.json")
[ -n "$file" ] || continue                     # nothing was written; nothing to commit

git -C "work/$repo" checkout -b codeowners-baseline
git -C "work/$repo" add "$file"                # exactly the one file, never -A
git -C "work/$repo" commit -m 'chore: org baseline ownership'
git -C "work/$repo" push -u origin HEAD
gh pr create --repo "$repo" --title 'chore: org baseline ownership' \
  --body-file "bodies/${repo//\//__}.md"
```

Presence rules for `codeowners_path`: [JSON.md](JSON.md). The PR body from `--summary-out`
names the same file, so a reviewer sees where that repo keeps its ownership before the diff.

## The CI gate afterwards

Once the baseline is merged, `audit` in each repo's CI keeps it honest — but gate it on
severity, or the rollout you just finished turns the fleet red:

```yaml
- run: codeowners-tool audit --github-repo ${{ github.repository }} --fail-on error
  env:
    GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

A `declare`d rule matches zero files — that is what a baseline *is* — and `audit` reports
each as A-4, a `warning`: the default `--fail-on any` fails the next push to every repo
the rollout touched. `--fail-on error` moves the gate, not the report
([AUDIT.md](AUDIT.md#audit-checks)).

Unclaimed paths stay unclaimed — no `*` catch-all is synthesized — and
`audit --checks a9 --fail-on never` reports what nobody owns without failing anything.

## What's left at the end

**`clone-failed`** is infrastructure — re-run the loop against just that list.

**`needs-human`** holds two different jobs; split it on `.status` before triaging:

```sh
jq -r 'select(.status=="refused") | .repo' results.jsonl   # a CODEOWNERS decision
jq -r 'select(.status=="error")   | .repo' results.jsonl   # a clone that could not be read
```

`refused` means the tool read the repository and declined: run `sync --dry-run` locally
to see why, then restructure that repo's CODEOWNERS (usually: replace the over-broad rule
the error names with narrower ones) or drop it from `repos.txt` as a legitimate
exception. `error` never got that far — an empty repository, a bad `--branch` — and
belongs with `clone-failed`. After fixes, re-run the whole loop; the resume guard skips
what converged.

**`warnings`** is what a human should look at in a repo the tool did not refuse over — a
second CODEOWNERS file, an edit landing in a file GitHub doesn't read or git never
committed, lines GitHub silently skips, a stale comment after a `rename_owner`.
Catalogued in [JSON.md](JSON.md), rendered into the PR body under **Worth a look**, and
surfaced fleet-wide with:

```sh
jq -r 'select(.warnings) | "\(.repo)\t\(.warnings|join("; "))"' results.jsonl
```

## The `jq` habit worth having

Project `.ops_skipped` too: a policy with one typo'd path prefix skips on every repo, and
`.status` alone then shows a reassuring wall of `skipped` rows. To ask "is this op
reaching any repo at all", count it out of `.ops[]`:

```sh
jq -s '[.[] | (.ops // [])[]] | group_by(.op) | map({op: .[0].op, n: length,
        applied: (map(select(.status=="applied")) | length)})' results.jsonl
```

The `// []` guard is required — empty keys are omitted entirely, and a refused repo has
no `.ops` at all — and `ops_applied + ops_skipped` need not equal your op count: an
already-satisfied op is `unchanged`, counted by neither ([JSON.md](JSON.md)).
