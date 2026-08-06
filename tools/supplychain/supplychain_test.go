// Package supplychain holds the release-integrity gates.
//
// These are assertions about the WORKFLOW FILES rather than about the tool's
// behavior, which is why they live under tools/ — gendocs walks internal/* to
// build docs/BEHAVIOR.md, and the provenance of a build is not a statement about
// what the binary does.
//
// BLOCKER 4 — a release must be verifiable, and the pipeline that cuts it must be
// immutable.
//
// Today `checksums.txt` is published on the same release as the artifacts it
// attests, and install.sh fetches both over the same channel from the same host.
// That detects corruption in transit. It proves nothing about origin: whoever can
// write a release rewrites the archives and the checksum file in one step, and
// every downstream verification still passes. Homebrew inherits the same trust,
// because gen-formula.sh reads its sha256 values out of that same file.
//
// Compounding it, both workflows pin actions by MUTABLE TAG (actions/checkout@v4,
// actions/setup-go@v5) while release.yml holds `permissions: contents: write` and
// a tap token. A tag moved upstream — by a compromise or by a maintainer's
// mistake — runs new code inside the job that publishes artifacts and can push to
// the Homebrew tap.
//
// Enterprise procurement asks for both. Neither is more than an afternoon.
package supplychain

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workflowsDir is relative to this package: go test runs with the package
// directory as the working directory.
const workflowsDir = "../../.github/workflows"

var (
	usesRe = regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*["']?([^"'\s#]+)`)
	shaRe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// releaseWorkflow returns release.yml, failing clearly if it is not there.
// Otherwise a rename fails every gate below with a message about the missing
// property rather than the missing file.
func releaseWorkflow(t *testing.T) string {
	t.Helper()
	files := workflowFiles(t)
	body, ok := files["release.yml"]
	if !ok {
		t.Fatalf("release.yml not found; workflows present: %v", keys(files))
	}
	return body
}

func workflowFiles(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext != ".yml" && ext != ".yaml" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(workflowsDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(b)
	}
	if len(out) == 0 {
		t.Fatalf("no workflow files found under %s", workflowsDir)
	}
	return out
}

// Every third-party action must be pinned to a full commit SHA. A tag is a
// mutable pointer, and these jobs hold contents:write and a tap token.
func TestSupplyChain_ActionsArePinnedToACommitSHA(t *testing.T) {
	for name, body := range workflowFiles(t) {
		for _, m := range usesRe.FindAllStringSubmatch(body, -1) {
			ref := m[1]
			// Local composite actions and container actions are not tag-resolved.
			if strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "docker://") {
				continue
			}
			at := strings.LastIndex(ref, "@")
			if at < 0 {
				t.Errorf("%s: `uses: %s` has no ref at all — pin it to a 40-character commit SHA", name, ref)
				continue
			}
			action, version := ref[:at], ref[at+1:]
			if !shaRe.MatchString(version) {
				t.Errorf("%s: `uses: %s` is pinned to the mutable ref %q.\nPin it as %s@<40-char-sha>  # %s — a moved tag runs new code in a job that can publish releases.",
					name, ref, version, action, version)
			}
		}
	}
}

// The release job must produce a verifiable statement of origin, not only a hash
// of what it happened to upload. Any of build-provenance attestation, cosign, or a
// detached signature over the checksums satisfies this.
func TestSupplyChain_ReleaseIsSignedOrAttested(t *testing.T) {
	body := releaseWorkflow(t)
	markers := []string{
		"attest-build-provenance",
		"attest-sbom",
		"cosign",
		"sigstore",
		"gh attestation",
		"--detach-sign",
		"gpg --detach-sig",
	}
	for _, m := range markers {
		if strings.Contains(body, m) {
			return
		}
	}
	t.Errorf(`release.yml publishes archives and a checksums.txt, but nothing signs or attests them.

checksums.txt lives on the same release as the artifacts and is fetched by
install.sh over the same channel, so it proves integrity in transit and nothing
about origin — anyone who can write the release rewrites both, and every
downstream check still passes. Homebrew inherits this, since gen-formula.sh reads
its sha256 values from that same file.

Add one of: %s

The provenance action also needs id-token: write on the job.`, strings.Join(markers, ", "))
}

// Attestation is only reachable if the job is allowed to mint an OIDC token, and
// the failure mode without it is a red release rather than a silent downgrade —
// so it is worth asserting separately from the step itself.
func TestSupplyChain_ReleaseJobCanMintAnOIDCToken(t *testing.T) {
	body := releaseWorkflow(t)
	if !strings.Contains(body, "id-token") {
		t.Errorf("release.yml declares no `id-token: write` permission, so keyless signing and build-provenance attestation cannot run")
	}
}

// The binary must be able to say what it is. Releases build with `-ldflags \"-s -w\"`
// and no -X, and the CLI has no `version` verb, so a fleet cannot be asked which
// build it is running — which makes both rollback and CVE response guesswork.
func TestSupplyChain_ReleaseStampsAVersionIntoTheBinary(t *testing.T) {
	body := releaseWorkflow(t)
	if !strings.Contains(body, "-X ") && !strings.Contains(body, "-X\t") {
		t.Errorf("release.yml builds with -ldflags but never stamps a version via -X.\nWithout it `codeowners-tool version` cannot report the build, and an operator holding a binary cannot tell whether it is the one with a given fix.")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
