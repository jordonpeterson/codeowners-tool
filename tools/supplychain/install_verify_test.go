package supplychain

import (
	"os"
	"strings"
	"testing"
)

// Signing that nobody verifies buys nothing. checksums.txt ships on the same
// release from the same host, so whoever can write the release rewrites both and
// "Checksum OK." still prints; only the attestation is unforgeable.

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

// The install must ask where the archive came from, not only what it hashes to.
func TestSupplyChain_InstallScriptVerifiesProvenanceNotOnlyTheChecksum(t *testing.T) {
	body := readRepoFile(t, installScript)
	if !strings.Contains(body, "gh attestation verify") {
		t.Errorf(`install.sh verifies the SHA-256 against checksums.txt and stops there.

checksums.txt ships on the same release from the same host, so it proves
integrity in transit and nothing about origin. release.yml already produces a
build-provenance attestation — until install.sh runs 'gh attestation verify'
against it, that signature is never read by anything.`)
	}
	// Without --repo it accepts a provenance statement from anywhere.
	if strings.Contains(body, "gh attestation verify") && !strings.Contains(body, "--repo") {
		t.Errorf("install.sh runs `gh attestation verify` without --repo: an attestation from ANY repository would satisfy it, which proves no more about origin than the checksum already did")
	}
}

// gh is absent from most machines running `curl | sh`, so its absence needs a
// deliberate behavior rather than a dead install.
func TestSupplyChain_InstallScriptHandlesAMachineWithoutGH(t *testing.T) {
	body := readRepoFile(t, installScript)
	if !strings.Contains(body, "gh attestation verify") {
		t.Skip("no attestation verification yet; the test above is the failure that matters")
	}
	if !strings.Contains(body, "have gh") {
		t.Errorf("install.sh calls `gh attestation verify` but never tests for gh with the script's own `have` helper.\nWithout gh the install would die on `gh: not found` after the download and checksum both succeeded.")
	}
}

// The direct-download path bypasses install.sh, so a reader told only about
// checksums.txt gets the weaker check as though it were the whole story.
func TestSupplyChain_ReadmeDocumentsVerifyingTheDirectDownload(t *testing.T) {
	body := readRepoFile(t, readmePath)
	if !strings.Contains(body, "gh attestation verify") {
		t.Errorf("README.md documents the direct-download path but never mentions `gh attestation verify`, so the reader who skips install.sh is sent to the check that proves integrity in transit while the one that proves origin goes unmentioned.")
	}
}
