# Rolling a policy out across an org

The tool works on one repo at a time and doesn't clone anything, so you write the loop.
The short version is four lines:

```bash
while read -r repo; do
  gh repo clone "$repo" "work/$repo" -- --depth 1 -q
  codeowners-tool sync --repo "work/$repo" --policy policy.json --create
done < repos.txt          # one "org/name" per line
```

That is genuinely all you need for a first pass. Don't use it for a real 100-repo
rollout, though: it stops dead the first time a clone fails or a repo needs a human.
The version below survives both, records what happened, and can be resumed.

## The real loop

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

## The two piles at the end

`clone-failed` is infrastructure — re-run the loop against just that list.
`needs-human` is the interesting one: for each repo, run `sync --dry-run` locally to
see the refusal, then either restructure that repo's CODEOWNERS so the intent becomes
expressible (usually: replace the over-broad rule the error names with narrower ones),
or accept that this repo is a legitimate exception and drop it from `repos.txt`.

The tool does not clone, commit, branch, or open PRs — that stays your script's job.
`--format json` prints one line per repo so `jq` can aggregate the fleet;
`--summary-out` writes a markdown summary for a PR body (keep it outside the clone, or
`git add -A` will commit it). Add `--dry-run` for a preview of the whole fleet: it
changes no CODEOWNERS file, but still emits the JSON and the summaries so there's
something to review.

One `jq` habit worth having: project `.ops_skipped` too. A policy with one typo'd path
prefix skips on every repo, and grouping on `.status` alone shows a reassuring wall of
`skipped` rows that you might read as success.
