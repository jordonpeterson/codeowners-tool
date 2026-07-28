#!/bin/sh
# Install codeowners-tool from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/jordonpeterson/codeowners-tool/main/install.sh | sh
#
# Environment overrides:
#   VERSION=v0.0.1   install a specific release (default: latest)
#   BINDIR=~/.local/bin   install location (default: /usr/local/bin)
#
# Downloads the prebuilt binary for your OS/arch, verifies its SHA-256 against
# the release checksums.txt, and installs it. Downloads via curl are not
# quarantined by macOS Gatekeeper, so no notarization prompt.
set -eu

REPO="jordonpeterson/codeowners-tool"
BIN="codeowners-tool"

err() { echo "install.sh: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

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
