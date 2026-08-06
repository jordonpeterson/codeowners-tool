package supplychain

import (
	"regexp"
	"strings"
	"testing"
)

// The blast radius of the release workflow is what these gates are about.
//
// `permissions:` at the top of release.yml is contents: write, because the
// release job creates tags and releases. A job-level block REPLACES the
// workflow-level one rather than adding to it, so a job with no block of its own
// silently inherits the write token — including the Homebrew job, which pushes to
// a different repository entirely with a separate PAT and has no business
// holding a token that can rewrite releases in this one.

var (
	// Job headers sit at two spaces under `jobs:`; keys inside a job at four.
	jobHeaderRe  = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)
	jobPermsRe   = regexp.MustCompile(`(?m)^    permissions:\s*$`)
	contentsRe   = regexp.MustCompile(`(?m)^\s*contents:\s*write\b`)
	topLevelKeyR = regexp.MustCompile(`^[A-Za-z0-9_-]+:`)
)

// splitJobs returns each job's name mapped to its block of the workflow file.
// This is a deliberate line scan rather than a YAML parse: go.mod is
// dependency-free by policy, and a YAML library is not worth importing to read
// two files whose indentation this repository controls.
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

// Least privilege has to be stated per job, because inheritance here defaults to
// the most privileged thing in the file.
func TestSupplyChain_EveryReleaseJobDeclaresItsOwnPermissions(t *testing.T) {
	body := workflowFiles(t)["release.yml"]
	if !contentsRe.MatchString(body) {
		t.Fatalf("release.yml no longer declares contents: write anywhere; this test's premise is stale, re-read it before deleting")
	}
	for name, block := range splitJobs(t, body) {
		if !jobPermsRe.MatchString(block) {
			t.Errorf("release.yml job %q declares no job-level `permissions:` block, so it inherits the workflow-level contents: write.\nA job-level block REPLACES the workflow-level one, which is why declaring one is the only way to drop a permission a job does not need.", name)
		}
	}
}

// The Homebrew job clones and pushes to jordonpeterson/homebrew-tap using a
// separate PAT. Nothing it does touches this repository's contents, so a token
// that can rewrite this repository's releases is pure standing blast radius: a
// compromised gen-formula.sh, or any step it runs, would inherit it.
func TestSupplyChain_HomebrewJobCannotWriteThisRepository(t *testing.T) {
	jobs := splitJobs(t, workflowFiles(t)["release.yml"])
	block, ok := jobs["homebrew"]
	if !ok {
		t.Skipf("no `homebrew` job in release.yml; jobs present: %v", jobNames(jobs))
	}
	// Checking only for the absence of `contents: write` inside the block would
	// pass VACUOUSLY today: the job holds that permission precisely because it
	// declares nothing, so the literal is not there to find. The absence of a
	// block is the finding.
	if !jobPermsRe.MatchString(block) {
		t.Fatalf("the homebrew job declares no `permissions:` block at all, so it inherits the workflow-level contents: write on THIS repository — while everything it actually does is read this repo for tools/gen-formula.sh and push to the tap with its own PAT.")
	}
	if contentsRe.MatchString(block) {
		t.Errorf("the homebrew job declares contents: write on THIS repository, but it only reads this repo (for tools/gen-formula.sh) and writes to the tap with its own PAT.\nDeclare `permissions: contents: read` on the job.")
	}
}

// A token pasted into a clone URL is written to .git/config on the runner in
// cleartext, and every git error that echoes the remote — "could not read from
// remote repository: https://x-access-token:ghp_...@github.com/..." — puts it in
// the build log. Actions masks registered secrets in its own log lines, but the
// value here has already been through a shell, and neither a runner's git config
// nor a third-party log shipper is masked at all.
func TestSupplyChain_NoWorkflowEmbedsACredentialInAURL(t *testing.T) {
	// A URL whose authority contains a shell or Actions expansion: the only
	// reason to interpolate anything before the `@` is a credential.
	credURL := regexp.MustCompile(`https://[^\s"']*\$[^\s"']*@`)
	for name, body := range workflowFiles(t) {
		for _, m := range credURL.FindAllString(stripComments(body), -1) {
			t.Errorf("%s embeds a credential in a URL: %s\nIt lands in .git/config on the runner and leaks into any error that echoes the remote. Use a credential helper (`gh auth setup-git`), GIT_ASKPASS, or gh itself.", name, m)
		}
	}
}

// stripComments drops whole-line comments before the scan above.
//
// Not cosmetic: the fix for this very finding is documented in a comment in
// release.yml that quotes the URL it replaced, and a detector that cannot tell an
// executed line from prose describing why that line is gone would fail forever on
// its own remediation — the standard way a gate ends up deleted. A commented-out
// URL is inert to both the shell and YAML, so nothing real is lost.
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

func jobNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
