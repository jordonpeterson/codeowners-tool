package supplychain

import (
	"os"
	"strings"
	"testing"
)

// Signing that nobody verifies buys nothing.
//
// release.yml attests every archive, and install.sh checked only the SHA-256
// against checksums.txt — published on the same release, from the same host, over
// the same channel. That is integrity in transit and nothing about ORIGIN:
// whoever can write the release rewrites both in one step and "Checksum OK."
// still prints. The attestation is the one thing a rewrite cannot forge, being
// signed with a short-lived OIDC identity the attacker does not hold.

const (
	installScript = "../../install.sh"
	readmePath    = "../../README.md"
)

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// The install path must ask "was this built by THIS workflow in THIS repo",
// not only "does it hash to what the release says it hashes to".
func TestSupplyChain_InstallScriptVerifiesProvenanceNotOnlyTheChecksum(t *testing.T) {
	body := readRepoFile(t, installScript)
	if !strings.Contains(body, "gh attestation verify") {
		t.Errorf(`install.sh verifies the SHA-256 against checksums.txt and stops there.

checksums.txt ships on the same release from the same host, so it proves
integrity in transit and nothing about origin. release.yml already produces a
build-provenance attestation — until install.sh runs 'gh attestation verify'
against it, that signature is never read by anything.`)
	}
	// Without --repo it would accept a provenance statement from anywhere, which is
	// no narrower a claim than the checksum it is meant to strengthen.
	if strings.Contains(body, "gh attestation verify") && !strings.Contains(body, "--repo") {
		t.Errorf("install.sh runs `gh attestation verify` without --repo: an attestation from ANY repository would satisfy it, which proves no more about origin than the checksum already did")
	}
}

// `curl | sh` is the documented install path and gh is absent from most minimal
// containers, so a missing gh needs a deliberate, visible behavior rather than
// the install exploding on `gh: not found`.
func TestSupplyChain_InstallScriptHandlesAMachineWithoutGH(t *testing.T) {
	body := readRepoFile(t, installScript)
	if !strings.Contains(body, "gh attestation verify") {
		t.Skip("no attestation verification yet; the test above is the failure that matters")
	}
	if !strings.Contains(body, "have gh") {
		t.Errorf("install.sh calls `gh attestation verify` but never tests for gh with the script's own `have` helper.\nWithout gh the install would die on `gh: not found` after the download and checksum both succeeded.")
	}
}

// The direct-download path bypasses install.sh, so a reader told only to "verify
// it against checksums.txt" has been handed the weaker of the two checks as
// though it were the whole story.
func TestSupplyChain_ReadmeDocumentsVerifyingTheDirectDownload(t *testing.T) {
	body := readRepoFile(t, readmePath)
	if !strings.Contains(body, "gh attestation verify") {
		t.Errorf("README.md documents the direct-download path but never mentions `gh attestation verify`, so the reader who skips install.sh is sent to the check that proves integrity in transit while the one that proves origin goes unmentioned.")
	}
}
