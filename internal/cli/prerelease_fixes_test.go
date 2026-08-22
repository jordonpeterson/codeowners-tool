// Regression guards around the pre-release fixes: each test here pins an edge
// the KnownBug test that motivated the fix does not — the same behavior on the
// verbs the bug report only implied, and the neighboring cases the fix must
// NOT have broken.
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

// fixGitOut runs git and returns its trimmed stdout, for tests that need a
// value (a SHA) rather than a side effect (which is gitRun's job).
func fixGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// Positional args are rejected on EVERY verb, not only the audit invocation
// that surfaced the bug: the flag package stops at the first non-flag token on
// all of them alike, so any verb left unguarded still swallows whole
// arguments. Exit 3 — decidable from the arguments alone, like every other
// member of that class.
func TestFix_PositionalArgsRejectedOnEveryVerb(t *testing.T) {
	for _, verb := range []string{"sync", "check", "plan", "apply", "audit", "lint", "verify", "snapshot"} {
		code, _, stderr := runCLI(t, verb, "stray-arg", "--repo", ".")
		if code != cli.ExitInvalid {
			t.Errorf("%s with positional arg: want exit 3, got %d\nstderr: %s", verb, code, stderr)
		}
		if !strings.Contains(stderr, "stray-arg") {
			t.Errorf("%s: the error must name the stray argument\nstderr: %s", verb, stderr)
		}
	}
}

// The pure-JSON fix must not have taken the human verdict with it: under the
// default text format a clean audit still says so on stdout.
func TestFix_AuditCleanLineStaysInTextMode(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/t\n",
		"f.md":               "",
	})
	code, stdout, _ := runCLI(t, "audit", "--repo", repo, "--checks", "a4")
	if code != cli.ExitOK {
		t.Fatalf("clean audit: want exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "audit clean") {
		t.Errorf("text mode lost its verdict line:\n%s", stdout)
	}
}

// `audit --format json` stdout is one JSON document in the findings case too,
// not only on the clean repo the KnownBug test pins.
func TestFix_AuditJSONWithFindingsIsPureJSON(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/e\n/ghost/ @org/gone\n",
		"a.md":               "",
	})
	code, stdout, _ := runCLI(t, "audit", "--repo", repo, "--checks", "a4", "--format", "json")
	if code != cli.ExitFindings {
		t.Fatalf("audit with dead rule: want exit 4, got %d", code)
	}
	var v any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Errorf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
}

// headLabel's contract: on a detached HEAD the label is the bare abbreviated
// SHA — the honest answer, since there is no branch name to offer. Before the
// fix the echoed `--end-of-options` line meant the name never equalled "HEAD"
// and the detached path could not fire.
func TestFix_DetachedHeadLabelIsBareSHA(t *testing.T) {
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
	gitRun(t, repo, "checkout", "-q", "--detach")
	short := fixGitOut(t, repo, "rev-parse", "HEAD")[:7]

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--branch", "feature", "--op", "add_owner(a.md, @org/x)")
	if code != cli.ExitRefused {
		t.Fatalf("branch mismatch on detached HEAD: want exit 2, got %d", code)
	}
	out := stdout + stderr
	if !strings.Contains(out, "HEAD is "+short) {
		t.Errorf("detached HEAD should be named by its bare SHA %q:\n%s", short, out)
	}
	if strings.Contains(out, "HEAD is HEAD") || strings.Contains(out, "--end-of-options") {
		t.Errorf("refusal still leaks a placeholder or git plumbing:\n%s", out)
	}
}

// The other uncleaned spellings of a governing location: `.github//CODEOWNERS`
// and `docs/../CODEOWNERS` name the S-8 files they clean to, so neither draws
// the "governs nothing" warning — while a path that genuinely is not an S-8
// location still does.
func TestFix_FileFlagSpellingsClassifyByCleanPath(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/e\n",
		"a.md":               "",
	})
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--file", ".github//CODEOWNERS", "--op", "add_owner(a.md, @org/y)")
	if code != cli.ExitOK {
		t.Fatalf("--file .github//CODEOWNERS: want exit 0, got %d\nstderr: %s", code, stderr)
	}
	if out := stdout + stderr; strings.Contains(out, "governs nothing") || strings.Contains(out, "govern nothing") {
		t.Errorf("false warning for a doubled-slash spelling of the governing file:\n%s", out)
	}

	rootRepo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @org/e\n",
		"a.md":       "",
	})
	code, stdout, stderr = runCLI(t, "sync", "--repo", rootRepo, "--file", "docs/../CODEOWNERS", "--op", "add_owner(a.md, @org/y)")
	if code != cli.ExitOK {
		t.Fatalf("--file docs/../CODEOWNERS: want exit 0, got %d\nstderr: %s", code, stderr)
	}
	if out := stdout + stderr; strings.Contains(out, "governs nothing") || strings.Contains(out, "govern nothing") {
		t.Errorf("false warning for a dot-dot spelling of the governing root file:\n%s", out)
	}

	// The warning itself must survive the fix: a path that cleans to something
	// GitHub never loads still governs nothing however it is spelled.
	offRepo := initRepo(t, map[string]string{
		"build/OWNERS": "* @org/e\n",
		"a.md":         "",
	})
	code, stdout, stderr = runCLI(t, "sync", "--repo", offRepo, "--file", "./build/OWNERS", "--op", "add_owner(a.md, @org/y)")
	if code != cli.ExitOK {
		t.Fatalf("--file ./build/OWNERS: want exit 0, got %d\nstderr: %s", code, stderr)
	}
	if out := stdout + stderr; !strings.Contains(out, "governs nothing") {
		t.Errorf("a genuinely non-governing --file lost its S-8 warning:\n%s", out)
	}
}

// The symlink refusal reaches `apply` too: a plan is reviewed in one place and
// applied in another, so the link can appear between the two — the write must
// refuse, and the link's in-repo target must keep its bytes.
func TestFix_ApplyRefusesSymlinkedCodeowners(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"docs/OWNERS_REAL":   "* @org/every\n",
		"a.md":               "",
	})
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, stderr := runCLI(t, "plan", "--repo", repo, "--op", "add_owner(a.md, @org/a)", "--out", planPath); code != cli.ExitOK {
		t.Fatalf("plan: want exit 0, got %d\nstderr: %s", code, stderr)
	}
	if err := os.Remove(filepath.Join(repo, ".github/CODEOWNERS")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../docs/OWNERS_REAL", filepath.Join(repo, ".github/CODEOWNERS")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(repo, "docs/OWNERS_REAL"))

	code, _, stderr := runCLI(t, "apply", "--plan", planPath)
	if code != cli.ExitRefused {
		t.Errorf("apply through in-repo symlinked CODEOWNERS: want exit 2, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "symlink") {
		t.Errorf("the refusal must name the symlink:\n%s", stderr)
	}
	after, _ := os.ReadFile(filepath.Join(repo, "docs/OWNERS_REAL"))
	if string(after) != string(before) {
		t.Errorf("the link's target was written anyway:\n%s", after)
	}
}

// lint shares the same write path, and its symlink refusal fires before any
// API call — so it is decidable, and tested, offline.
func TestFix_LintRefusesSymlinkedCodeowners(t *testing.T) {
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

	code, _, stderr := runCLI(t, "lint", "--repo", repo, "--github-repo", "o/r", "--token", "t")
	if code != cli.ExitRefused {
		t.Errorf("lint through in-repo symlinked CODEOWNERS: want exit 2, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "symlink") {
		t.Errorf("the refusal must name the symlink:\n%s", stderr)
	}
}

// A symlink that is NOT the governing CODEOWNERS stays irrelevant: only the
// write target is Lstat'ed, so an ordinary repo full of links syncs as before.
func TestFix_SymlinkElsewhereStaysIrrelevant(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"a.md":               "",
	})
	if err := os.Symlink("../a.md", filepath.Join(repo, ".github/NOTES")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "link elsewhere")

	code, _, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(a.md, @org/a)")
	if code != cli.ExitOK {
		t.Errorf("sync with an unrelated symlink in the repo: want exit 0, got %d\nstderr: %s", code, stderr)
	}
}

// The repo-root guard reaches `apply` too: --repo can point the apply at a
// different clone than the plan's, and pointed below the root the joined
// codeowners_path names a file GitHub never reads (checkRepoRoot).
func TestFix_ApplyBelowRepoRootRefused(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":     "* @org/root\n",
		"a.md":                   "",
		"sub/.github/CODEOWNERS": "* @org/fixture\n",
		"sub/a.md":               "",
	})
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, stderr := runCLI(t, "plan", "--repo", repo, "--op", "add_owner(a.md, @org/a)", "--out", planPath); code != cli.ExitOK {
		t.Fatalf("plan: want exit 0, got %d\nstderr: %s", code, stderr)
	}
	sub := filepath.Join(repo, "sub")
	before, _ := os.ReadFile(filepath.Join(sub, ".github/CODEOWNERS"))

	code, _, stderr := runCLI(t, "apply", "--plan", planPath, "--repo", sub)
	if code != cli.ExitRefused {
		t.Errorf("apply below root: want exit 2, got %d\nstderr: %s", code, stderr)
	}
	after, _ := os.ReadFile(filepath.Join(sub, ".github/CODEOWNERS"))
	if string(after) != string(before) {
		t.Errorf("the dead subdirectory file was written anyway:\n%s", after)
	}
}
