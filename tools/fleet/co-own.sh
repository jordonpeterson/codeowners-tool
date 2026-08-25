#!/usr/bin/env bash
# co-own.sh — make OWNER a shared, never exclusive, owner of SCOPE in one repo.
#
# For the rollout shape where a fleet-wide line like
#   .github/workflows @org/platform
# should become "platform still owns this, together with whoever owns the
# surrounding tree". The co-owners differ per repo, so no single policy can
# state the end state; this script derives it per repo and proves it with
# codeowners-tool before reporting success. See README.md for the fleet loop.
#
# Usage: co-own.sh --repo DIR --scope PATTERN --owner @org/team [--branch NAME]
# Env:   CODEOWNERS_TOOL — path to the binary (default: codeowners-tool on PATH)
#
# Exit codes follow the tool's fleet contract:
#   0  converged (edited, already correct, or nothing in scope)
#   2  this repo needs a human; handed back untouched
#   3  bad invocation or broken environment — will fail identically everywhere
#
# Stdout is exactly one JSON object: {status, detail, branch, codeowners_path}.
# status: shared | unchanged | skipped | needs-human. Everything else -> stderr.
set -euo pipefail

REPO="" SCOPE="" OWNER="" BRANCH=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo)   REPO="${2:?}"; shift 2 ;;
    --scope)  SCOPE="${2:?}"; shift 2 ;;
    --owner)  OWNER="${2:?}"; shift 2 ;;
    --branch) BRANCH="${2:?}"; shift 2 ;;
    *) echo "co-own.sh: unknown argument: $1" >&2; exit 3 ;;
  esac
done
if [ -z "$REPO" ] || [ -z "$SCOPE" ] || [ -z "$OWNER" ]; then
  echo "usage: co-own.sh --repo DIR --scope PATTERN --owner @org/team [--branch NAME]" >&2
  exit 3
fi

TOOL="${CODEOWNERS_TOOL:-codeowners-tool}"
for bin in git jq awk; do
  command -v "$bin" > /dev/null || { echo "co-own.sh: $bin not found" >&2; exit 3; }
done
command -v "$TOOL" > /dev/null || { echo "co-own.sh: codeowners-tool not found (set CODEOWNERS_TOOL)" >&2; exit 3; }

# SCOPE reaches the tool (sync --op, verify --scope) exactly as typed: the op
# and its proof must carry the operator's spelling — stripping the anchoring
# slash from /docs/ would widen both to any-depth docs. S is the slash-stripped
# spelling for mechanics only: the branch name, the dedicated-line match (awk
# strips the line's slashes the same way), and jq path-prefix checks.
# ANCHORED mirrors CODEOWNERS matching: any slash before a trailing one anchors
# the pattern to the root; a bare single segment matches at any depth.
S="${SCOPE#/}"; S="${S%/}"
case "${SCOPE%/}" in
  */*) ANCHORED=1 ;;
  *)   ANCHORED=0 ;;
esac
CFILE=""

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

emit() { # status detail
  jq -cn --arg s "$1" --arg d "$2" --arg b "${BRANCH}" --arg f "${CFILE}" \
    '{status: $s, detail: $d, branch: $b, codeowners_path: $f}'
}
skip()  { emit "${2:-skipped}" "$1"; exit 0; }
human() { emit needs-human "$1"; exit 2; }

git -C "$REPO" rev-parse --git-dir > /dev/null 2>&1 || human "not a git repository"
[ -z "$(git -C "$REPO" status --porcelain)" ] || human "working tree not clean"

"$TOOL" snapshot --repo "$REPO" --out "$TMP/before.json" >&2 \
  || human "snapshot failed (no commits?)"
CFILE="$(jq -r '.codeowners_path // empty' "$TMP/before.json")"
[ -n "$CFILE" ] || skip "no CODEOWNERS file"

# state SNAPSHOT -> none|no-owner|shared|exclusive, over the in-scope paths.
# In-scope follows the typed spelling: anchored scopes prefix-match from the
# root, unanchored ones also match under any parent. Wildcards are not modeled
# here — such a scope selects no paths and is skipped as "nothing in scope";
# the tool's snapshot/verify stay the authority on any edit regardless.
# Owners compare under the tool's identity (R-38): @handles fold case, an
# email stays byte-exact. Snapshot preserves file spellings, so byte equality
# here would call @Org/Platform a different owner than --owner @org/platform.
state() {
  jq -r --arg s "$S" --arg o "$OWNER" --arg anchored "$ANCHORED" '
    def fold: if startswith("@") then ascii_downcase else . end;
    [.ownership | to_entries[]
     | select(.key as $k | ($k == $s) or ($k | startswith($s + "/"))
         or (($anchored == "0")
             and (($k | endswith("/" + $s)) or ($k | contains("/" + $s + "/")))))
     | .value // []] as $sets
    | if ($sets | length) == 0 then "none"
      elif [$sets[] | map(fold) | index($o | fold)] | any(. == null) then "no-owner"
      elif [$sets[] | length >= 2] | all then "shared"
      else "exclusive" end' "$1"
}
case "$(state "$TMP/before.json")" in
  none)     skip "no tracked files in scope" ;;
  no-owner) skip "$OWNER does not own every path in scope; granting is a different decision" ;;
  shared)   skip "already co-owned" unchanged ;;
esac

# Exclusivity must come from a dedicated "SCOPE OWNER" line; anything else
# (a broad sole-owner rule, extra owners, an inline comment) has no move that
# provably stays inside the scope, so it goes to a human instead. The owner
# match folds like state() does: @handles case-insensitively, emails byte-exact.
if ! awk -v s="$S" -v o="$OWNER" '
  function fold(x) { return x ~ /^@/ ? tolower(x) : x }
  { line = $0; sub(/\r$/, "", line)
    n = split(line, f, /[ \t]+/); i = 1
    if (n > 0 && f[1] == "") i = 2
    p = f[i]; sub(/^\/+/, "", p); sub(/\/+$/, "", p)
    if (n - i + 1 == 2 && p == s && fold(f[i+1]) == fold(o)) { deleted = 1; next }
    print }
  END { exit deleted ? 0 : 1 }
' "$REPO/$CFILE" > "$TMP/without-line"; then
  human "no dedicated \"$SCOPE $OWNER\" line; exclusivity comes from elsewhere in $CFILE"
fi

BASE="$(git -C "$REPO" rev-parse HEAD)"
ORIG_REF="$(git -C "$REPO" symbolic-ref -q --short HEAD || echo "$BASE")"
if [ -z "$BRANCH" ]; then
  BRANCH="co-own/$(printf '%s' "$S" | sed -e 's#[^A-Za-z0-9_-]#-#g' -e 's#^[.-]*##')"
fi
git -C "$REPO" rev-parse -q --verify "refs/heads/$BRANCH" > /dev/null \
  && human "branch $BRANCH already exists"

restore_and_human() { # detail
  git -C "$REPO" reset -q --hard "$BASE"
  git -C "$REPO" checkout -q "$ORIG_REF"
  git -C "$REPO" branch -qD "$BRANCH" 2> /dev/null || true
  human "$1"
}

git -C "$REPO" checkout -qb "$BRANCH"

# Try the clean answer first: delete the line and let the broader rule take
# over. Snapshot resolves the committed tree, so the probe needs a commit;
# everything squashes into one reviewable commit at the end.
cat "$TMP/without-line" > "$REPO/$CFILE"
git -C "$REPO" commit -qam "probe: line removed"
"$TOOL" snapshot --repo "$REPO" --out "$TMP/fallback.json" >&2 \
  || restore_and_human "snapshot failed after line removal"

if [ "$(state "$TMP/fallback.json")" != shared ]; then
  # The broader rule drops or loses $OWNER, so deleting alone breaks "stays an
  # owner". Invert: keep the line, add the broader team(s) to it — one
  # add_owner with an owner list is one line change (R-33b).
  FALLBACK="$(jq -r --arg s "$S" --arg o "$OWNER" --arg anchored "$ANCHORED" '
    def fold: if startswith("@") then ascii_downcase else . end;
    [.ownership | to_entries[]
     | select(.key as $k | ($k == $s) or ($k | startswith($s + "/"))
         or (($anchored == "0")
             and (($k | endswith("/" + $s)) or ($k | contains("/" + $s + "/")))))
     | .value // []] | unique as $sets
    | if ($sets | length) != 1 then ""
      else [$sets[0][] | select(fold != ($o | fold))] | join(", ") end' "$TMP/fallback.json")"
  [ -n "$FALLBACK" ] || restore_and_human "nothing co-ownable behind the line; a human decides who shares $SCOPE"
  git -C "$REPO" reset -q --hard "$BASE"
  RC=0
  "$TOOL" sync --repo "$REPO" --op "add_owner($SCOPE, [$FALLBACK])" >&2 || RC=$?
  case "$RC" in
    0) git -C "$REPO" commit -qam "probe: line amended" ;;
    2) restore_and_human "tool refused to amend: add_owner($SCOPE, [$FALLBACK])" ;;
    *) restore_and_human "tool exited $RC on add_owner($SCOPE, [$FALLBACK])" ;;
  esac
fi

git -C "$REPO" reset -q --soft "$BASE"
git -C "$REPO" commit -qm "chore: co-own $SCOPE ($OWNER no longer exclusive)"

# Prove the result before reporting it: nothing outside the scope moved, and
# every in-scope path is owned by $OWNER plus at least one other team.
"$TOOL" snapshot --repo "$REPO" --out "$TMP/after.json" >&2 \
  || restore_and_human "snapshot failed after edit"
"$TOOL" verify --before "$TMP/before.json" --after "$TMP/after.json" --scope "$SCOPE" >&2 \
  || restore_and_human "ownership moved outside $SCOPE; edit discarded"
[ "$(state "$TMP/after.json")" = shared ] \
  || restore_and_human "end state is not co-owned; edit discarded"

emit shared "one commit on $BRANCH, ready to push"
