# Installing codeowners-tool

Every release ships Linux, macOS, and Windows builds for both amd64 and arm64.

- [Homebrew](#homebrew) — the short one
- [GitHub Actions](#github-actions) — for CI
- [Install script](#install-script) — for a machine with nothing on it
- [Direct download](#direct-download)
- [From source](#from-source)
- [Verifying a download](#verifying-a-download)
- [Upgrading and uninstalling](#upgrading-and-uninstalling)
- [GitHub Enterprise Server](#github-enterprise-server)

## Homebrew

macOS and Linux:

```sh
brew install jordonpeterson/tap/codeowners-tool
```

> Homebrew derives its `sha256` from `checksums.txt`, so `brew install` inherits the
> weaker of the two guarantees below. Verify the attestation directly if that matters to
> you.

## GitHub Actions

Add one step and the binary is on the `PATH` for every step after it:

```yaml
- uses: jordonpeterson/codeowners-tool@v0
- run: codeowners-tool lint --dry-run --github-repo ${{ github.repository }}
  env:
    GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

| Input | Effect |
|---|---|
| `version` | Release to install. Defaults to the tag the action is pinned at: a full-version pin installs exactly that release, `@v0` takes the newest. Set it to `latest` to float regardless of the pin. |
| `provenance` | `auto` (default), `require`, or `skip` — see [below](#verifying-a-download). |
| `install-dir` | Where to put the binary. Defaults to a directory under `RUNNER_TEMP`. |

It outputs `version` (the build actually installed) and `path`. Unlike `curl | sh` on a
laptop, provenance here is verified by default: hosted runners ship `gh`, and the action
hands it a token.

Linux and macOS runners. On Windows, download the `.zip`
[from the release](#direct-download) in a `run:` step.

## Install script

macOS and Linux. Downloads the right prebuilt binary, verifies its checksum *and its build
provenance*, and installs it:

```sh
curl -fsSL https://raw.githubusercontent.com/jordonpeterson/codeowners-tool/main/install.sh | sh
```

| Variable | Effect |
|---|---|
| `VERSION=vX.Y.Z` | Pin a release instead of taking the latest. |
| `BINDIR=~/.local/bin` | Change the install location. |
| `PROVENANCE=` | Control provenance verification — see [below](#verifying-a-download). |

## Direct download

Grab the archive for your platform from the
[latest release](https://github.com/jordonpeterson/codeowners-tool/releases/latest),
[verify it](#verifying-a-download), extract, and put `codeowners-tool` on your `PATH`.

> **macOS note:** the binaries are not notarized, so a build downloaded through a browser
> is quarantined by Gatekeeper. Clear it with
> `xattr -d com.apple.quarantine ./codeowners-tool`. Homebrew and the install script are
> unaffected — neither quarantines its downloads.

## From source

Go 1.24+, and no dependencies to fetch — `go.mod` lists none:

```sh
go install github.com/jordonpeterson/codeowners-tool/cmd/codeowners-tool@latest
```

Or from a checkout:

```sh
make build     # ./bin/codeowners-tool
make all       # vet, test, build, docs
```

## Verifying a download

There are two checks and they answer different questions.

**`checksums.txt` answers *"did these bytes arrive intact?"*** It ships on the same
release from the same host, so it detects corruption, not tampering — whoever can write
the release can rewrite both the archive and its checksum.

**The build-provenance attestation answers *"was this built by this repository's release
workflow?"*** It is signed with a short-lived OIDC identity only that workflow can obtain,
so it survives a compromise of the release itself:

```sh
gh attestation verify codeowners-tool_vX.Y.Z_darwin_arm64.tar.gz \
  --repo jordonpeterson/codeowners-tool \
  --signer-workflow jordonpeterson/codeowners-tool/.github/workflows/release.yml
```

`install.sh` runs that check when the [GitHub CLI](https://cli.github.com) is installed
and signed in. **`gh` is not a prerequisite** — the point of `curl | sh` is a machine with
nothing on it, and requiring `gh` would just push people toward hand-downloaded tarballs
that get verified less often. Set `PROVENANCE=`:

| | |
|---|---|
| `auto` (default) | Verify when `gh` can; warn loudly when it cannot. |
| `require` | No verification, no install — for CI and managed fleets. |
| `skip` | Do not attempt it. |

A check that runs and *fails* aborts the install in every mode; only the inability to run
one degrades to a warning.

## Upgrading and uninstalling

```sh
brew upgrade jordonpeterson/tap/codeowners-tool     # Homebrew
brew uninstall codeowners-tool
```

The install script has no uninstaller because it has no state: re-running it replaces the
binary, and removing `codeowners-tool` from your `BINDIR` (default `/usr/local/bin`) is
the uninstall. The tool writes nothing outside the repository you point it at, except the
optional `audit --cache-dir` you name yourself.

Confirm which build you're on — the binary is stamped at release time:

```sh
codeowners-tool version
```

## GitHub Enterprise Server

No separate build. The only commands that reach the network are `audit`'s A-1/A-2/A-3
owner checks; point them at your instance with `--api-url`:

```sh
GITHUB_TOKEN=... codeowners-tool audit \
  --github-repo org/repo \
  --api-url https://github.example.com/api/v3
```

Endpoint gaps degrade to exit 5 (inconclusive), never to a wrong answer. Auth is a PAT for
v1, via `--token` or `$GITHUB_TOKEN`.
