# Installation

Every release ships Linux, macOS, and Windows builds for both amd64 and arm64.

## Homebrew (macOS/Linux)

```sh
brew install jordonpeterson/tap/codeowners-tool
```

## Install script (macOS/Linux)

Downloads the right prebuilt binary, verifies its checksum *and its build
provenance*, and installs it:

```sh
curl -fsSL https://raw.githubusercontent.com/jordonpeterson/codeowners-tool/main/install.sh | sh
```

Set `VERSION=vX.Y.Z` to pin a release or `BINDIR=~/.local/bin` to change the install
location.

## From source

With Go 1.24+:

```sh
go install github.com/jordonpeterson/codeowners-tool/cmd/codeowners-tool@latest
```

Or from a checkout: `make build`. `make all` runs vet, tests, build, and docs.

## Direct download

Grab the archive for your platform from the
[latest release](https://github.com/jordonpeterson/codeowners-tool/releases/latest),
verify it (below), extract, and put `codeowners-tool` on your `PATH`.

## Verifying a download

`checksums.txt` answers *"did these bytes arrive intact?"* — it ships on the same
release from the same host, so it detects corruption, not tampering.

The build-provenance attestation answers *"was this built by this repository's release
workflow?"* It is signed with a short-lived OIDC identity only that workflow can
obtain, so it survives a compromise of the release itself:

```sh
gh attestation verify codeowners-tool_vX.Y.Z_darwin_arm64.tar.gz \
  --repo jordonpeterson/codeowners-tool \
  --signer-workflow jordonpeterson/codeowners-tool/.github/workflows/release.yml
```

`install.sh` runs that check when the [GitHub CLI](https://cli.github.com) is
installed and signed in. **`gh` is not a prerequisite** — the point of `curl | sh` is
a machine with nothing on it, and requiring it would just push people to
hand-downloaded tarballs that get verified less. Set `PROVENANCE=`:

| | |
|---|---|
| `auto` (default) | verify when `gh` can; warn loudly when it cannot |
| `require` | no verification, no install — for CI and managed fleets |
| `skip` | do not attempt it |

A check that runs and *fails* aborts the install in every mode; only the inability to
run one degrades to a warning.

> Homebrew derives its `sha256` from `checksums.txt`, so `brew install` inherits the
> weaker guarantee. Verify the attestation directly if that matters to you.

> **macOS note:** the binaries are not notarized, so a build downloaded through a
> browser is quarantined by Gatekeeper. Clear it with
> `xattr -d com.apple.quarantine ./codeowners-tool`. Homebrew and the install script
> above are unaffected — neither quarantines its downloads.
