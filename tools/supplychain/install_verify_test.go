package supplychain

import (
	"os"
	"strings"
	"testing"
)

// Signing that nobody verifies buys nothing.
//
// release.yml now attests every archive with a build-provenance statement bound
// to this workflow in this repository. install.sh still checks only the SHA-256
// against checksums.txt — and checksums.txt is published on the same release,
// fetched over the same channel, from the same host as the archive it covers.
// That is integrity in transit and nothing about ORIGIN: whoever can write the
// release rewrites the archive and the checksum in one step, and the install
// still reports "Checksum OK." Homebrew inherits it, because gen-formula.sh
// derives its sha256 values from that same unsigned file.
//
// The attestation is the only artifact on the release that a rewrite cannot
// forge, because it is signed with a short-lived OIDC identity the attacker does
// not hold. Until install.sh consults it, the release pipeline signs into a void.

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

checksums.txt ships on the same release as the archive and is fetched from the
same host over the same channel, so it detects corruption in transit and proves
nothing about origin: anyone who can write the release rewrites both, and
"Checksum OK." still prints. release.yml already produces a build-provenance
attestation signed with a short-lived OIDC identity for this repository — until
install.sh runs 'gh attestation verify' against it, that signature is never read
by anything.`)
	}
	// A verification that does not name the repository would accept a provenance
	// statement from any repository at all, which is not a narrower claim than the
	// checksum it is meant to strengthen.
	if strings.Contains(body, "gh attestation verify") && !strings.Contains(body, "--repo") {
		t.Errorf("install.sh runs `gh attestation verify` without --repo: an attestation from ANY repository would satisfy it, which proves no more about origin than the checksum already did")
	}
}

// gh is not present on a minimal container, and `curl | sh` is the documented
// install path. Verification therefore has to have a defined behavior when gh is
// missing — chosen deliberately and visible in the script — rather than the
// install simply exploding on `gh: not found`.
func TestSupplyChain_InstallScriptHandlesAMachineWithoutGH(t *testing.T) {
	body := readRepoFile(t, installScript)
	if !strings.Contains(body, "gh attestation verify") {
		t.Skip("no attestation verification yet; the test above is the failure that matters")
	}
	if !strings.Contains(body, "have gh") {
		t.Errorf("install.sh calls `gh attestation verify` but never tests for gh with the script's own `have` helper.\nOn a machine without gh — the common case for `curl | sh` inside a container — the install would die on `gh: not found` at the last step, after the download and the checksum both succeeded.")
	}
}

// The direct-download path bypasses install.sh entirely, so the verification
// command has to be written down where that path is documented. A reader told to
// "verify it against checksums.txt" and nothing more has been handed the weaker
// of the two checks as though it were the whole story.
func TestSupplyChain_ReadmeDocumentsVerifyingTheDirectDownload(t *testing.T) {
	body := readRepoFile(t, readmePath)
	if !strings.Contains(body, "gh attestation verify") {
		t.Errorf("README.md documents the direct-download path as \"verify it against checksums.txt\" and never mentions `gh attestation verify`.\nThat sends the reader who skips install.sh to the check that proves integrity in transit, while the one that proves origin goes unmentioned.")
	}
}
