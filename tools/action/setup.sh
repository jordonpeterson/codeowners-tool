#!/bin/sh
# Install a codeowners-tool release onto a GitHub Actions runner.
#
# Run by action.yml at the repository root, not by hand. It resolves WHICH
# release to ask for and hands the download to install.sh, which already
# verifies the checksum and the build provenance and is already gated by
# tools/supplychain. A second download here would be a second supply-chain
# surface for the same bytes.
set -eu

REPO="jordonpeterson/codeowners-tool"
BIN="codeowners-tool"

err() { echo "$BIN setup: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
is_tag() { printf '%s' "${1:-}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; }

# Releases ship Windows builds, but as .zip archives install.sh does not unpack.
# Saying so here beats a Git Bash `uname` failing partway through a download.
case "${RUNNER_OS:-Linux}" in
Linux | macOS) ;;
Windows) err "Windows runners are not supported. Releases ship Windows builds as .zip archives, which the install script does not unpack; download the .zip from https://github.com/$REPO/releases and add it to the PATH in a run: step." ;;
*) err "unsupported runner OS: $RUNNER_OS" ;;
esac

# Checked before the download rather than after: install.sh validates the mode
# too, but only once the archive and its checksum have already been fetched, so
# a typo would surface as a failed install.
provenance="${INPUT_PROVENANCE:-}"
[ -n "$provenance" ] || provenance=auto
case "$provenance" in
auto | require | skip) ;;
*) err "provenance must be auto, require or skip (got '$provenance')" ;;
esac

# Resolve the release tag. An empty version means install.sh resolves it.
#
# The action ships from the same repository as the tool, so
# `uses: $REPO@v0.0.9` reads as a pin. Taking the newest release there would
# hand that workflow a different build on any day a release lands — a pin that
# silently isn't one is worse than none, because it is the version people write
# down during an incident.
requested="${INPUT_VERSION:-}"
# `latest` is an explicit request to float, so it beats the ref default rather
# than falling through to it: pinning the action at @v0.0.9 for stable behavior
# while taking the newest tool is a coherent thing to ask for, and answering it
# with v0.0.9 because the ref happened to name a release answers a different
# question than the one the workflow asked.
version=""
if [ "$requested" = latest ]; then
  version=""
elif [ -n "$requested" ]; then
  case "$requested" in
  v*) version="$requested" ;;
  *) version="v$requested" ;;
  esac
  is_tag "$version" || err "version must be a release tag like v0.0.28 or 'latest' (got '$requested'); see https://github.com/$REPO/releases"
elif is_tag "${GITHUB_ACTION_REF:-}"; then
  version="$GITHUB_ACTION_REF"
fi

# `uses: ...@v0` names no release, so the tag has to be looked up. Through gh
# rather than an anonymous api.github.com call: that limit is 60/hour per IP and
# hosted runners share addresses, so the lookup that only ever runs in CI is the
# one that must be authenticated. Where gh cannot answer, install.sh resolves
# "latest" itself and the run continues.
if [ -z "$version" ] && have gh && gh auth status >/dev/null 2>&1; then
  latest=$(gh release view --repo "$REPO" --json tagName --jq .tagName 2>/dev/null || true)
  if is_tag "$latest"; then
    version="$latest"
  fi
fi

bindir="${INPUT_INSTALL_DIR:-}"
[ -n "$bindir" ] || bindir="${RUNNER_TEMP:-/tmp}/$BIN"
mkdir -p "$bindir" || err "cannot create the install directory $bindir"

# The working directory in a consumer's job is THEIR repository; install.sh only
# exists under the action path.
action_path="${GITHUB_ACTION_PATH:-}"
[ -n "$action_path" ] || err "GITHUB_ACTION_PATH is unset — this script is run by action.yml, not by hand"
installer="$action_path/install.sh"
[ -f "$installer" ] || err "install script not found at $installer"

if [ -n "$version" ]; then
  VERSION="$version" BINDIR="$bindir" PROVENANCE="$provenance" sh "$installer"
else
  BINDIR="$bindir" PROVENANCE="$provenance" sh "$installer"
fi

bin="$bindir/$BIN"
[ -x "$bin" ] || err "the install reported success but there is no binary at $bin"
installed=$("$bin" version) || err "the installed binary at $bin does not run"

# A release stamped with the wrong tag, or an older binary already sitting in the
# install directory, would otherwise be invisible: the job goes green having run
# something other than the build it pinned.
if [ -n "$version" ] && [ "$installed" != "$version" ]; then
  err "requested $version but the installed binary reports $installed — refusing to put a build on the PATH that is not the one this job asked for"
fi

# Nothing was pinned, so there is no tag to compare against — but the build still
# has to name one. A release built without the -X stamp reports "dev", and a job
# that exports it reports a version nobody can map back to a build, which is the
# guesswork the release workflow stamps the tag to prevent.
is_tag "$installed" || err "the installed binary reports '$installed', which is not a release tag — this build carries no version, so nothing downstream could say which one ran"

# Exported only now: a PATH entry pointing at a failed install turns the next
# step's "codeowners-tool: not found" into what looks like a broken workflow.
if [ -n "${GITHUB_PATH:-}" ]; then
  printf '%s\n' "$bindir" >> "$GITHUB_PATH"
fi
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  printf 'version=%s\npath=%s\n' "$installed" "$bin" >> "$GITHUB_OUTPUT"
fi
echo "$BIN $installed is on the PATH ($bin)"
