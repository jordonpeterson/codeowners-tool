// Findings about WHICH CODEOWNERS file a run governs, in repositories that
// have more than one — or one somewhere other than where discovery looks.
// S-8 gives GitHub's order (.github/ > root > docs/, first found wins, never
// merged), and every test here is a case where the tool and that order come
// apart while the run reports success.
//
// Each test is a FAILING repro of a confirmed defect, written first per
// CONTRIBUTING.md.
package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// FINDING: `--create` with a `--file` that OUTRANKS the repository's real
// CODEOWNERS supersedes it — every rule in the governing file stops applying,
// and the run reports `applied (proven: tree)`, exit 0.
//
// A repo whose ownership lives in `docs/CODEOWNERS`:
//
//	$ codeowners-tool sync --file .github/CODEOWNERS --create --op 'add_owner(/docs/, @org/docs-team)'
//	applied: 1 op(s) applied, 0 skipped; 1 line change(s), 2 path(s) change owners
//	  ops[0]  applied (proven: tree)
//	  created a new CODEOWNERS file
//	$ cat .github/CODEOWNERS
//	/docs/ @org/docs-team
//
// Under S-8 that one line is now the whole repository's ownership. Both
// invariants are broken by the run that called itself proven:
//
//   - INV-2: services/api/main.go silently loses @org/api-team. The tool's own
//     `verify` says INVARIANT VIOLATED on the same change, exit 2.
//   - INV-1: `add_owner` promises "every pre-existing owner of every path in
//     scope is kept" (README) and @org/everyone is gone from /docs/.
//
// BEHAVIOR.md names this precise failure as the thing the spec forbids:
// "'Never overwrites' is also satisfied by writing a SECOND file at
// .github/CODEOWNERS, which leaves the original untouched and, under S-8,
// silently demotes it to a file GitHub never loads — the whole repo's
// ownership replaced by one op's worth of rules." The guard enforcing it sits
// on the DISCOVERY path; an explicit `--file` walks around it, because
// governing() takes rel from --file and proves against empty bytes.
//
// It is reachable from a policy (`"create": true` plus a pinned `--file`), so
// it is a fleet-scale hazard, and POLICY-FILE.md's "Never overwrites, so it is
// safe to leave set for a fleet where only some repos have a file" is false
// for exactly the repos that have one somewhere other than .github/.
func TestCreateWithFileMustNotSupersedeTheGoverningCodeowners(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"docs/CODEOWNERS":      "* @org/everyone\n/services/api/ @org/api-team\n",
		"docs/readme.md":       "",
		"services/api/main.go": "",
	})

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--create",
		"--file", ".github/CODEOWNERS", "--op", "add_owner(/docs/, @org/docs-team)")
	out := stdout + stderr

	if code == cli.ExitOK {
		t.Errorf("creating .github/CODEOWNERS over a repo governed by docs/CODEOWNERS reported success (exit 0)\n"+
			"under S-8 the new file is now the only one GitHub reads, so every rule in docs/CODEOWNERS stopped applying\noutput:\n%s", out)
	}
	if b, err := os.ReadFile(filepath.Join(repo, ".github/CODEOWNERS")); err == nil {
		t.Errorf(".github/CODEOWNERS was written, superseding docs/CODEOWNERS; it now reads:\n%s\n"+
			"services/api/main.go lost @org/api-team (INV-2) and /docs/ lost @org/everyone (INV-1)", b)
	}
	// The one warning it does print asserts the opposite of what happened:
	// the file it just made authoritative is described as governing nothing.
	if strings.Contains(out, "the rules written here govern nothing") {
		t.Errorf("the warning is inverted — .github/CODEOWNERS outranks docs/CODEOWNERS, so the rules written there\n"+
			"govern EVERYTHING and docs/CODEOWNERS is the file that stopped governing\noutput:\n%s", out)
	}
}

// FINDING: R-24's "every time" warning is emitted only by `sync`. `plan`,
// `apply` and `lint` — the other three verbs that touch the file — write a
// CODEOWNERS GitHub does not read, in total silence.
//
// BEHAVIOR.md, TestRollout_WritingAFileGitHubWillNotReadIsWarned: "SPEC R-24
// (S-8/A-10): when the file this run writes is not the file GitHub will read,
// the record says so — every time, on stderr and in `warnings`." That test
// covers `sync`. governingWarnings() has exactly one call site, in sync.go, so
// plan and apply skip it:
//
//	$ codeowners-tool plan --file docs/CODEOWNERS --op 'add_owner(/services/api/, @org/p)' --out p.json
//	plan written to p.json
//	$ codeowners-tool apply --plan p.json
//	applied: docs/CODEOWNERS (5 → 30 bytes)
//
// .github/CODEOWNERS governs this repo. The plan is the artifact a human
// reviews before the write, which is exactly where "this file governs
// nothing" needs to appear, and it is the one place it never does. `lint`,
// which writes without a plan at all, is the same.
func TestPlanApplyAndLintWarnWhenTheyWriteAFileGitHubWillNotRead(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/every\n",
		"docs/CODEOWNERS":      "* @org/stale\n/ghost/ @org/gone\n",
		"services/api/main.go": "",
	})
	planPath := filepath.Join(t.TempDir(), "plan.json")

	code, stdout, stderr := runCLI(t, "plan", "--repo", repo, "--file", "docs/CODEOWNERS",
		"--op", "add_owner(/services/api/, @org/platform)", "--out", planPath)
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("plan: exit %d: %s", code, out)
	}
	if !strings.Contains(out, ".github/CODEOWNERS") {
		t.Errorf("plan wrote a plan against docs/CODEOWNERS without saying that GitHub resolves this repo from\n"+
			".github/CODEOWNERS — the review artifact is where R-24's disclosure matters most\noutput:\n%s", out)
	}

	code, stdout, stderr = runCLI(t, "apply", "--plan", planPath, "--repo", repo)
	out = stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("apply: exit %d: %s", code, out)
	}
	if !strings.Contains(out, ".github/CODEOWNERS") {
		t.Errorf("apply wrote docs/CODEOWNERS and reported success without naming the file that actually governs\noutput:\n%s", out)
	}

	// lint writes with no plan to review at all, so its own output is the
	// only place the disclosure could appear. Offline mode (tree-only
	// repairs) engages exactly when neither a token nor --github-repo is
	// given, so an inherited GITHUB_TOKEN would change the verb under test.
	t.Setenv("GITHUB_TOKEN", "")
	code, stdout, stderr = runCLI(t, "lint", "--repo", repo, "--file", "docs/CODEOWNERS", "--remove-stale-paths")
	out = stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("lint: exit %d: %s", code, out)
	}
	if !strings.Contains(out, ".github/CODEOWNERS") {
		t.Errorf("lint repaired docs/CODEOWNERS and reported the file written, without saying GitHub reads\n"+
			".github/CODEOWNERS instead\noutput:\n%s", out)
	}
}

// FINDING: `--file` naming a path that is not in the ref is refused with a
// message that is false three times over, and that false text is what lands in
// the fleet's JSON record.
//
// In a repo carrying all three CODEOWNERS files:
//
//		$ codeowners-tool sync --file OWNERS --op 'add_owner(/services/api/, @org/p)'
//		error: no CODEOWNERS file found in .github/, root, or docs/ at HEAD;
//		       re-run with --create to write one at .github/CODEOWNERS, or --file to name a path (R-23)
//
//	 1. the repo has CODEOWNERS in all three locations;
//	 2. `--create` would write at OWNERS, not at .github/CODEOWNERS;
//	 3. "or --file to name a path" is offered to someone who just passed --file.
//
// noCodeownersError is built from a fixed head with no knowledge that
// filePath was set, and governing() reaches it for any missing --file target.
// A `needs-human` triage pile reading these records concludes those repos have
// no CODEOWNERS.
func TestMissingFileTargetIsNotReportedAsNoCodeownersAnywhere(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/a\n",
		"CODEOWNERS":           "* @org/a\n",
		"docs/CODEOWNERS":      "* @org/a\n",
		"services/api/main.go": "",
	})

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--file", "OWNERS",
		"--op", "add_owner(/services/api/, @org/platform)")
	out := stdout + stderr
	if code == cli.ExitOK {
		t.Fatalf("expected a refusal, got exit 0:\n%s", out)
	}
	if strings.Contains(out, "no CODEOWNERS file found in .github/, root, or docs/") {
		t.Errorf("the refusal claims this repo has no CODEOWNERS, in a repo that has all three (S-8 locations);\n"+
			"the missing path is the one --file named, and this text is what the fleet record carries\noutput:\n%s", out)
	}
	if !strings.Contains(out, "OWNERS") {
		t.Errorf("the refusal never names OWNERS, the path --file actually pointed at\noutput:\n%s", out)
	}
	if strings.Contains(out, "--create to write one at .github/CODEOWNERS") {
		t.Errorf("the remedy is wrong: with --file OWNERS, --create writes OWNERS, not .github/CODEOWNERS\noutput:\n%s", out)
	}
}
