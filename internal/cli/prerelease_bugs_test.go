// Pre-release findings: each test in this file states behavior the tool SHOULD
// have and currently does not. They are expected to FAIL until the bug they
// document is fixed; fix the bug, and the test becomes its regression guard.
package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// KNOWN BUG: plan and apply skip the repo-root guard that sync enforces.
// sync refuses `--repo <subdir>` because the CODEOWNERS it would write lands
// at a path GitHub never reads (checkRepoRoot). plan happily plans against the
// subtree and apply writes the dead file, reporting success — the "applied,
// dead on arrival" outcome the guard exists to prevent. plan must refuse
// exactly as sync does.
func TestKnownBug_PlanBelowRepoRootRefused(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":     "* @org/root\n",
		"a.md":                   "",
		"sub/.github/CODEOWNERS": "* @org/fixture\n",
		"sub/inner.md":           "",
	})
	sub := filepath.Join(repo, "sub")

	// Baseline: sync refuses this.
	if code, _, _ := runCLI(t, "sync", "--repo", sub, "--op", "add_owner(inner.md, @org/a)"); code != cli.ExitRefused {
		t.Fatalf("sync below root: want exit 2, got %d", code)
	}

	planPath := filepath.Join(t.TempDir(), "plan.json")
	code, _, stderr := runCLI(t, "plan", "--repo", sub, "--op", "add_owner(inner.md, @org/a)", "--out", planPath)
	if code != cli.ExitRefused {
		t.Errorf("plan below root: want exit 2 (same refusal as sync), got %d\nstderr: %s", code, stderr)
	}
	if _, err := os.Stat(planPath); err == nil {
		t.Errorf("plan below root wrote %s; a refused run must write nothing", planPath)
	}
}

// KNOWN BUG: a `\#`-escaped pattern is accepted and written, but S-6/S-2 says
// GitHub honors no `\#` escape — on GitHub the written line is dead, so the
// tool reports `proven: tree` for a rule that provably does not hold there.
// The unescaped spelling `add_owner(#tag.md, …)` is already refused; the
// escaped spelling must be refused too, and nothing written.
func TestKnownBug_EscapedHashPatternRefused(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"#tag.md":            "",
	})
	before, _ := os.ReadFile(filepath.Join(repo, ".github/CODEOWNERS"))

	code, _, stderr := runCLI(t, "sync", "--repo", repo, "--op", `add_owner(\#tag.md, @org/a)`)
	if code != cli.ExitRefused && code != cli.ExitInvalid {
		t.Errorf(`add_owner(\#tag.md, …): want refusal (exit 2 or 3), got %d`, code)
	}
	after, _ := os.ReadFile(filepath.Join(repo, ".github/CODEOWNERS"))
	if string(after) != string(before) {
		t.Errorf("file was rewritten with a \\#-escaped pattern GitHub will not honor:\n%s\nstderr: %s", after, stderr)
	}
}

// KNOWN BUG: a symlinked .github/CODEOWNERS inside the clone is written
// through and reported applied with no warning. The tool's own docs state
// GitHub does not follow a symlinked CODEOWNERS, so the run edited a file
// that governs nothing while reporting success. An out-of-repo symlink target
// is already refused (containedWritePath); the in-repo case must at minimum
// not be a silent success.
func TestKnownBug_SymlinkedCodeownersNotSilentSuccess(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"docs/OWNERS_REAL": "* @org/every\n",
		"a.md":             "",
	})
	if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../docs/OWNERS_REAL", filepath.Join(repo, ".github/CODEOWNERS")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "symlink")

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(a.md, @org/a)")
	if code == cli.ExitOK && !strings.Contains(stdout+stderr, "symlink") {
		t.Errorf("write through symlinked CODEOWNERS succeeded silently (exit 0, no symlink warning)\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}

// KNOWN BUG: the S-7 branch-mismatch refusal interpolates raw `git rev-parse
// --abbrev-ref --end-of-options HEAD` output, and rev-parse echoes the
// `--end-of-options` operator as an output line — so the one-line error (and
// the JSON record's error field) reads "HEAD is --end-of-options\nmain (…)".
func TestKnownBug_BranchMismatchErrorNamesHeadCleanly(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/e\n",
		"a.md":               "",
	})
	gitRun(t, repo, "switch", "-qc", "feature")
	if err := os.WriteFile(filepath.Join(repo, "b.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "f")
	gitRun(t, repo, "switch", "-q", "main")

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--branch", "feature", "--op", "add_owner(a.md, @org/x)")
	if code != cli.ExitRefused {
		t.Fatalf("branch mismatch: want exit 2, got %d", code)
	}
	if out := stdout + stderr; strings.Contains(out, "--end-of-options") {
		t.Errorf("refusal leaks git plumbing into the error text (want \"HEAD is main (…)\"):\n%s", out)
	}
}

// KNOWN BUG: `--file ./.github/CODEOWNERS` (or any uncleaned spelling of a
// governing location) triggers a false "governs nothing" warning. trackedAt
// cleans the spelling before matching the tracked file; the S-8 location
// check compares the raw string, so a live change is reported as dead in the
// warning, the --out record, and the --summary-out PR body.
func TestKnownBug_FileFlagSpellingNoFalseGovernsNothing(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/e\n",
		"a.md":               "",
	})
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--file", "./.github/CODEOWNERS", "--op", "add_owner(a.md, @org/y)")
	if code != cli.ExitOK {
		t.Fatalf("sync --file ./.github/CODEOWNERS: want exit 0, got %d\nstderr: %s", code, stderr)
	}
	if out := stdout + stderr; strings.Contains(out, "governs nothing") {
		t.Errorf("false warning for an alternate spelling of the governing file:\n%s", out)
	}
}

// KNOWN BUG: set_owners on a scope whose pattern already exists earlier in
// the file authors a shadowed duplicate — the old line stays, dead under
// last-match-wins but still naming its owners to human readers — and the run
// that creates it says nothing. The R-7 duplicate warning fires only on the
// NEXT run that touches the file. The run creating the duplicate must
// disclose it.
func TestKnownBug_SetOwnersDisclosesAuthoredDuplicate(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/x/ @org/a\n* @org/e\n",
		"x/a.go":             "",
		"README.md":          "",
	})
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "set_owners(/x/, [@org/n])")
	if code != cli.ExitOK {
		t.Fatalf("set_owners: want exit 0, got %d\nstderr: %s", code, stderr)
	}
	after, _ := os.ReadFile(filepath.Join(repo, ".github/CODEOWNERS"))
	if strings.Count(string(after), "/x/ ") != 2 {
		t.Skipf("file no longer contains a duplicate /x/ rule; bug shape changed:\n%s", after)
	}
	if out := stdout + stderr; !strings.Contains(out, "duplicate") && !strings.Contains(out, "shadow") {
		t.Errorf("run authored a shadowed duplicate of /x/ without disclosing it\nfile:\n%s\noutput:\n%s", after, out)
	}
}

// KNOWN BUG: `audit --format json` on a clean repo prints the literal line
// "audit clean" after the JSON object, so the one case CI most wants to pipe
// to jq — the healthy repo — is the one case the output isn't parseable.
// Under `--format json`, stdout is data.
func TestKnownBug_AuditJSONCleanIsPureJSON(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/t\n",
		"f.md":               "",
	})
	code, stdout, _ := runCLI(t, "audit", "--repo", repo, "--checks", "a4", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("clean audit: want exit 0, got %d", code)
	}
	var v any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Errorf("audit --format json stdout is not one JSON document: %v\nstdout:\n%s", err, stdout)
	}
}

// KNOWN BUG: positional arguments are silently discarded, and every flag
// after them with them. `audit ../other-repo --checks a999` (note the missing
// --repo) audits the CWD with all defaults and exits 0 — the invalid
// `--checks a999`, which the parser would reject loudly, is never seen. A
// tool this strict about flag values must not swallow whole arguments.
func TestKnownBug_PositionalArgsRejected(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/t\n",
		"f.md":               "",
	})
	// t.Chdir is process-wide: this test must never gain t.Parallel(). From
	// inside a healthy repo, the dropped args make audit run on "." with all
	// defaults and exit 0 — hiding both the bogus path and the bad --checks.
	t.Chdir(repo)
	code, stdout, stderr := runCLI(t, "audit", filepath.Join(repo, "does-not-exist"), "--checks", "a999")
	if code != cli.ExitInvalid {
		t.Errorf("audit with positional arg and invalid --checks: want exit 3, got %d\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
}

// KNOWN BUG: `audit` silently accepts an unknown `--format` and falls back to
// text. sync, check, and lint all reject unknown formats at exit 3 — "never a
// silent fallback to text" — and audit is documented with the same
// `--format json|text` contract.
func TestKnownBug_AuditRejectsUnknownFormat(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/t\n",
		"f.md":               "",
	})
	code, _, _ := runCLI(t, "audit", "--repo", repo, "--format", "xml")
	if code != cli.ExitInvalid {
		t.Errorf("audit --format xml: want exit 3, got %d", code)
	}
}

// KNOWN BUG (UAT finding): a tree-provably-dead rule cannot be repaired
// offline. `lint --dry-run --remove-stale-paths` refuses everything at exit 5
// citing R-12 ("owner existence is not decidable offline") — but whether a
// pattern matches zero tracked files is a git-tree fact the offline audit
// (A-4/A-5) itself proves, no API needed. With --remove-stale-paths as the
// requested repair, the dry run should report the pending dead-rule removal
// (exit 4) instead of demanding a token; today the only offline remedy is the
// hand edit the tool exists to prevent.
func TestKnownBug_OfflineStaleRuleRemovalReportable(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/everyone\n/ghost/ @org/ghost-team\n",
		"a.md":               "",
	})
	code, stdout, stderr := runCLI(t, "lint", "--repo", repo, "--dry-run", "--remove-stale-paths")
	if code != cli.ExitFindings {
		t.Errorf("offline lint --dry-run --remove-stale-paths on a tree-provably-dead rule: want exit 4 (pending fix), got %d\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if out := stdout + stderr; !strings.Contains(out, "/ghost/") {
		t.Errorf("the pending removal should name the dead pattern /ghost/\noutput:\n%s", out)
	}
}

// gitRun is a helper for tests that need extra git steps after initRepo.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
