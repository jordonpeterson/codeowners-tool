# co-own.sh — undo an exclusive fleet-wide CODEOWNERS line

Turns "`@org/platform` owns `.github/workflows` alone" into "`@org/platform`
owns it together with whoever owns the surrounding tree" — in one repo, so a
fleet loop can run it across a hundred.

No single policy file can express this end state: the co-owners differ per
repo. The script derives them per repo and proves the result with
`codeowners-tool` before reporting success.

## Usage

```sh
tools/fleet/co-own.sh --repo work/org/name --scope .github/workflows --owner @org/platform
```

`--branch NAME` overrides the work branch (default `co-own/<scope>`);
`CODEOWNERS_TOOL` points at the binary if it is not on `PATH`.

## What it does, per repo

1. If the scope is already co-owned, matches no files, or the owner does not
   own it at all: touch nothing, exit 0.
2. If a dedicated `SCOPE OWNER` line exists, delete it — when the broader rule
   behind it already lists the owner, that alone is the answer.
3. When the broader rule lacks the owner, deleting would revoke instead of
   share; the script inverts: keeps the line and adds the broader team(s) to it.
4. Either way the edit lands as **one commit on a new branch**, and is kept
   only if the tool proves nothing outside the scope changed owners
   (`verify --scope`) and every in-scope path ends with the owner **plus at
   least one other team**.
5. Anything unprovable — nothing behind the line, exclusivity from a broad
   rule, a dirty clone — is refused with the repo handed back untouched.

## Exit codes and output

Same contract as `sync`: `0` converged (or nothing to do), `2` this repo needs
a human, `3` the invocation is broken and would fail identically everywhere.
Stdout is one JSON object: `{status, detail, branch, codeowners_path}` with
status `shared | unchanged | skipped | needs-human`.

## A fleet loop

Cloning, auth and PRs stay with `gh`, as in [docs/FLEET.md](../../docs/FLEET.md):

```sh
while read -r repo; do
  gh repo clone "$repo" "work/$repo" -- --depth 1 -q || { echo "$repo" >> clone-failed; continue; }
  code=0
  tools/fleet/co-own.sh --repo "work/$repo" \
    --scope .github/workflows --owner @org/platform >> results.jsonl || code=$?
  case $code in
    0) if [ "$(tail -1 results.jsonl | jq -r .status)" = shared ]; then
         git -C "work/$repo" push -u origin HEAD
         gh pr create --repo "$repo" --title 'chore: co-own .github/workflows' --fill
       fi ;;
    2) echo "$repo" >> needs-human ;;
    *) exit "$code" ;;     # broken invocation — stop the run
  esac
done < repos.txt
```

`needs-human` repos are the ones where somebody must decide who co-owns the
scope; `jq -r 'select(.status=="needs-human") | .detail' results.jsonl` says
why for each.

## Tests

`coown_test.go` is the specification — one e2e test per behavior above,
running the real binary against real git repositories:

```sh
go test ./tools/fleet/
```
