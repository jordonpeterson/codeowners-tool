#!/bin/sh
# Generate the Homebrew formula for codeowners-tool from a release tag and its
# checksums.txt (as published on the GitHub release).
#
#   tools/gen-formula.sh v0.0.1 checksums.txt > Formula/codeowners-tool.rb
#
# Kept as a standalone script (not inline YAML) so it can be tested locally and
# reused by the release workflow's Homebrew-tap bump.
set -eu

tag="${1:?usage: gen-formula.sh <tag> <checksums.txt>}"
checksums="${2:?usage: gen-formula.sh <tag> <checksums.txt>}"
ver="${tag#v}"
base="https://github.com/jordonpeterson/codeowners-tool/releases/download/${tag}"

sha() {
  s=$(grep "$1" "$checksums" | awk '{print $1}' | head -1)
  [ -n "$s" ] || { echo "gen-formula: no checksum for $1 in $checksums" >&2; exit 1; }
  echo "$s"
}
da=$(sha "darwin_amd64")
dr=$(sha "darwin_arm64")
la=$(sha "linux_amd64")
lr=$(sha "linux_arm64")

cat <<EOF
class CodeownersTool < Formula
  desc "Safe, intent-level, verifiable CODEOWNERS changes"
  homepage "https://github.com/jordonpeterson/codeowners-tool"
  version "${ver}"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "${base}/codeowners-tool_${tag}_darwin_arm64.tar.gz"
      sha256 "${dr}"
    else
      url "${base}/codeowners-tool_${tag}_darwin_amd64.tar.gz"
      sha256 "${da}"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "${base}/codeowners-tool_${tag}_linux_arm64.tar.gz"
      sha256 "${lr}"
    else
      url "${base}/codeowners-tool_${tag}_linux_amd64.tar.gz"
      sha256 "${la}"
    end
  end

  def install
    bin.install "codeowners-tool"
  end

  test do
    assert_match "codeowners-tool", shell_output("#{bin}/codeowners-tool --help")
  end
end
EOF
