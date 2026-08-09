package supplychain

import (
	"regexp"
	"strings"
	"testing"
)

// release.yml's workflow-level `permissions:` is contents: write, and a job-level
// block REPLACES it rather than adding to it — so a job declaring none silently
// inherits the write token.

var (
	// Job headers sit at two spaces under `jobs:`; keys inside a job at four.
	jobHeaderRe  = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)
	jobPermsRe   = regexp.MustCompile(`(?m)^    permissions:\s*$`)
	contentsRe   = regexp.MustCompile(`(?m)^\s*contents:\s*write\b`)
	topLevelKeyR = regexp.MustCompile(`^[A-Za-z0-9_-]+:`)

	attestUsesRe  = regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*["']?actions/attest[^\s"'@]*@`)
	attestWriteRe = regexp.MustCompile(`(?m)^\s*attestations:\s*write\b`)
)

// splitJobs maps each job name to its block. A line scan, not a YAML parse:
// go.mod is dependency-free by policy.
func splitJobs(t *testing.T, body string) map[string]string {
	t.Helper()
	lines := strings.Split(body, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimRight(l, " \t") == "jobs:" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("workflow has no `jobs:` key; the scan below cannot be trusted")
	}
	jobs := map[string]string{}
	name, buf := "", []string{}
	flush := func() {
		if name != "" {
			jobs[name] = strings.Join(buf, "\n")
		}
		name, buf = "", nil
	}
	for _, l := range lines[start:] {
		if m := jobHeaderRe.FindStringSubmatch(l); m != nil {
			flush()
			name = m[1]
			continue
		}
		// A key back at column 0 ends the jobs block entirely.
		if strings.TrimSpace(l) != "" && topLevelKeyR.MatchString(l) {
			flush()
			break
		}
		if name != "" {
			buf = append(buf, l)
		}
	}
	flush()
	if len(jobs) == 0 {
		t.Fatalf("no jobs parsed out of the workflow; the scan is broken, not the workflow")
	}
	return jobs
}

// Least privilege must be stated per job: inheritance defaults to the most
// privileged thing in the file.
func TestSupplyChain_EveryReleaseJobDeclaresItsOwnPermissions(t *testing.T) {
	body := releaseWorkflow(t)
	if !contentsRe.MatchString(body) {
		t.Fatalf("release.yml no longer declares contents: write anywhere; this test's premise is stale, re-read it before deleting")
	}
	for name, block := range splitJobs(t, body) {
		if !jobPermsRe.MatchString(block) {
			t.Errorf("release.yml job %q declares no job-level `permissions:` block, so it inherits the workflow-level contents: write.\nA job-level block REPLACES the workflow-level one, so declaring one is the only way to drop a permission.", name)
		}
	}
}

// The release used to push the Homebrew formula to jordonpeterson/homebrew-tap
// with a HOMEBREW_TAP_TOKEN secret — a personal access token holding write on a
// second repository, for the lifetime of the secret. It was never set, and the
// job it gated logged a notice and exited 0, so four releases reported a green
// "bump homebrew tap" while the formula stayed at v0.0.2. The tap now pulls its
// own bump with the token Actions mints for it, and this asserts the credential
// does not come back: `secrets.GITHUB_TOKEN` is scoped to this repository and
// minted per run, and anything else in a workflow holding contents: write is a
// standing grant on something else.
func TestSupplyChain_NoWorkflowHoldsACrossRepositoryCredential(t *testing.T) {
	secretRefRe := regexp.MustCompile(`\$\{\{\s*secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	for file, body := range workflowFiles(t) {
		for _, m := range secretRefRe.FindAllStringSubmatch(stripComments(body), -1) {
			if m[1] == "GITHUB_TOKEN" {
				continue
			}
			t.Errorf("%s reads `secrets.%s`. A secret that is not GITHUB_TOKEN is a standing credential for something outside this repository, and its absence is invisible — the tap token was never set, and the job that needed it stayed green for four releases while doing nothing.\nIf a second repository has to be written, have THAT repository pull the change with its own token.", file, m[1])
		}
	}
}

// Least privilege has a floor: a job must still hold what its own steps need.
// Minting the OIDC token that signs the attestation (id-token: write) and writing
// the signed statement to the repository's attestation store (attestations: write)
// are separate permissions, and the job held only the first — so every release
// after the attest step was added died on "Resource not accessible by
// integration". The step runs before `gh release create` on purpose, which turns
// the missing permission into a dropped release rather than an unsigned one.
func TestSupplyChain_JobsThatAttestCanWriteAnAttestation(t *testing.T) {
	for file, body := range workflowFiles(t) {
		if !attestUsesRe.MatchString(stripComments(body)) {
			continue
		}
		for name, block := range splitJobs(t, body) {
			block = stripComments(block)
			if !attestUsesRe.MatchString(block) {
				continue
			}
			if !attestWriteRe.MatchString(block) {
				t.Errorf("%s: job %q runs an actions/attest* step but declares no `attestations: write`.\nid-token: write only mints the signing token; persisting the attestation needs attestations: write, and without it the step fails with \"Resource not accessible by integration\".", file, name)
			}
		}
	}
}

// A token pasted into a clone URL lands in .git/config in cleartext and in any
// git error echoing the remote. Actions masks its own log lines, not those.
func TestSupplyChain_NoWorkflowEmbedsACredentialInAURL(t *testing.T) {
	// A URL whose authority contains a shell or Actions expansion: the only
	// reason to interpolate anything before the `@` is a credential.
	credURL := regexp.MustCompile(`https://[^\s"']*\$[^\s"']*@`)
	for name, body := range workflowFiles(t) {
		for _, m := range credURL.FindAllString(stripComments(body), -1) {
			t.Errorf("%s embeds a credential in a URL: %s\nUse a credential helper (`gh auth setup-git`), GIT_ASKPASS, or gh itself.", name, m)
		}
	}
}

// stripComments drops whole-line comments: release.yml documents this very fix in
// a comment quoting the URL it replaced, and a detector that fails on its own
// remediation is one somebody deletes.
func stripComments(body string) string {
	lines := strings.Split(body, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimLeft(l, " \t"), "#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
