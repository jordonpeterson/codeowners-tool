#!/bin/sh
# Install codeowners-tool from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/jordonpeterson/codeowners-tool/main/install.sh | sh
#
# Environment overrides:
#   VERSION=v0.0.1   install a specific release (default: latest)
#   BINDIR=~/.local/bin   install location (default: /usr/local/bin)
#   PROVENANCE=auto|require|skip   build-provenance check (default: auto)
#
# Downloads the prebuilt binary for your OS/arch, verifies its SHA-256 against
# the release checksums.txt AND its build provenance against the attestation
# signed by this repository's release workflow, and installs it. Downloads via
# curl are not quarantined by macOS Gatekeeper, so no notarization prompt.
set -eu

REPO="jordonpeterson/codeowners-tool"
BIN="codeowners-tool"
WORKFLOW=".github/workflows/release.yml"

err() { echo "install.sh: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# verify_provenance answers "was this built by THIS workflow in THIS repository",
# which no hash on the release can answer.
#
# A verification that RUNS and FAILS is always fatal, kept apart from the
# inability to run one (handled by the caller). gh exits nonzero for a forged
# attestation, a missing one, and an unknown flag alike, so the only branch on
# its error text is for a gh too old to know --signer-workflow — a missing client
# feature, not a statement about the artifact.
verify_provenance() {
  echo "Verifying build provenance..."
  if out=$(gh attestation verify "$1" --repo "$REPO" \
      --signer-workflow "$REPO/$WORKFLOW" 2>&1); then
    echo "Provenance OK: built by $REPO's release workflow."
    return 0
  fi
  case "$out" in
  *"unknown flag"* | *"unknown shorthand"*)
    if out=$(gh attestation verify "$1" --repo "$REPO" 2>&1); then
      echo "Provenance OK: built in $REPO."
      echo "Note: this gh is too old for --signer-workflow, so the archive is" >&2
      echo "      pinned to the repository but not to the release workflow." >&2
      echo "      Upgrade gh to check that too." >&2
      return 0
    fi
    ;;
  esac
  printf '%s\n' "$out" >&2
  err "BUILD PROVENANCE VERIFICATION FAILED for $2 — this archive does not carry a valid attestation from $REPO. Do not install it."
}

# --- detect platform ---
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) err "unsupported architecture: $arch" ;;
esac
case "$os" in
  linux | darwin) ;;
  *) err "unsupported OS: $os (Windows: download the .zip from the releases page)" ;;
esac

have tar || err "tar is required"
have curl || err "curl is required"

# --- resolve version ---
tag="${VERSION:-}"
if [ -z "$tag" ]; then
  tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name" *: *"([^"]+)".*/\1/')
  [ -n "$tag" ] || err "could not determine the latest release tag"
fi

asset="${BIN}_${tag}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

# --- download ---
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "Downloading $asset ($tag)..."
curl -fSL --proto '=https' "$base/$asset" -o "$tmp/$asset" \
  || err "download failed: $base/$asset"
curl -fsSL --proto '=https' "$base/checksums.txt" -o "$tmp/checksums.txt" \
  || err "could not download checksums.txt"

# --- verify checksum ---
want=$(grep "$asset" "$tmp/checksums.txt" | awk '{print $1}' | head -1)
[ -n "$want" ] || err "no checksum listed for $asset"
if have sha256sum; then
  got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
elif have shasum; then
  got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
else
  err "need sha256sum or shasum to verify the download"
fi
[ "$want" = "$got" ] || err "checksum mismatch for $asset (expected $want, got $got)"
echo "Checksum OK."

# --- verify build provenance ---
# The checksum proves the archive arrived intact, not where it came from:
# checksums.txt ships on the same release from the same host, so whoever can
# write the release rewrites both and "Checksum OK." still prints. The
# attestation is the one thing a rewrite cannot forge, being signed with a
# short-lived OIDC identity belonging to this repository's release workflow.
#
# gh is deliberately NOT a prerequisite. This script exists for `curl | sh` on a
# machine with nothing on it, and gh is absent from most of them; requiring it
# would not make those installs verified, it would make them fail, and the
# realistic answer to a failing install is a hand-downloaded tarball verified
# less than this. So the default verifies when it can and is loud when it cannot,
# and environments that can insist say so:
#
#   PROVENANCE=auto     verify when gh can; warn loudly when it cannot (default)
#   PROVENANCE=require  no verification, no install
#   PROVENANCE=skip     do not attempt it
provenance="${PROVENANCE:-auto}"
case "$provenance" in
auto | require | skip) ;;
*) err "PROVENANCE must be auto, require or skip (got '$provenance')" ;;
esac

if [ "$provenance" = skip ]; then
  echo "Provenance check skipped (PROVENANCE=skip): the origin of this build is unverified." >&2
else
  # gh's availability is established BEFORE running it: "no attestation exists for
  # these bytes" and "this machine cannot check" must never collapse into one
  # outcome. The first aborts the install; the second must not.
  cannot=""
  if ! have gh; then
    cannot="the GitHub CLI (gh) is not installed"
  elif ! gh auth status >/dev/null 2>&1; then
    cannot="gh is installed but not signed in (run: gh auth login)"
  fi
  if [ -n "$cannot" ]; then
    if [ "$provenance" = require ]; then
      err "PROVENANCE=require, but provenance cannot be checked: $cannot"
    fi
    cat >&2 <<EOF

WARNING: build provenance was NOT verified — $cannot.
  The checksum proves this archive arrived intact, not where it came from.
  To check origin, install the GitHub CLI and re-run, or verify by hand:
      gh attestation verify <archive> --repo $REPO \\
        --signer-workflow $REPO/$WORKFLOW
  Set PROVENANCE=require to make this a hard failure instead.

EOF
  else
    verify_provenance "$tmp/$asset" "$asset"
  fi
fi

# --- extract & install ---
tar -xzf "$tmp/$asset" -C "$tmp"
src="$tmp/${BIN}_${tag}_${os}_${arch}/$BIN"
[ -f "$src" ] || err "binary not found in archive"

BINDIR="${BINDIR:-/usr/local/bin}"
mkdir -p "$BINDIR" 2>/dev/null || true
if [ -w "$BINDIR" ]; then
  install -m 0755 "$src" "$BINDIR/$BIN"
elif have sudo; then
  echo "Installing to $BINDIR (requires sudo)..."
  sudo install -m 0755 "$src" "$BINDIR/$BIN"
else
  err "cannot write to $BINDIR; re-run with BINDIR=\$HOME/.local/bin"
fi

echo "Installed $BIN $tag to $BINDIR/$BIN"
case ":$PATH:" in
  *":$BINDIR:"*) ;;
  *) echo "Note: $BINDIR is not on your PATH; add it to use '$BIN' directly." ;;
esac
echo "Run '$BIN --help' to get started."
