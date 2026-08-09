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

## What's left at the end

Two piles.

**`clone-failed`** is infrastructure — re-run the loop against just that list.

**`needs-human`** is the interesting one. For each repo, run `sync --dry-run` locally to
see the refusal, then either restructure that repo's CODEOWNERS so the intent becomes
expressible (usually: replace the over-broad rule the error names with narrower ones), or
accept that this repo is a legitimate exception and drop it from `repos.txt`.

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
