// Package cli_test exercises the end-to-end contract: real git repo, real
// files, real exit codes (R-17).
package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

func initRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for path, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// The full loop: plan → apply → snapshot/verify. The verify step checks
// INV-2 from raw snapshots, without trusting plan/apply (R-18).
func TestEndToEnd_PlanApplyVerify(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "# owners\n/x/ @a\n",
		"x/a.go":     "package x\n",
		"y/b.go":     "package y\n",
	})
	planPath := filepath.Join(t.TempDir(), "plan.json")
	beforeSnap := filepath.Join(t.TempDir(), "before.json")
	afterSnap := filepath.Join(t.TempDir(), "after.json")

	if code, _, e := runCLI(t, "snapshot", "--repo", repo, "--out", beforeSnap); code != 0 {
		t.Fatalf("snapshot: %d %s", code, e)
	}
	if code, _, e := runCLI(t, "plan", "--repo", repo, "--op", "add_owner(/x/, @b)", "--out", planPath); code != 0 {
		t.Fatalf("plan: %d %s", code, e)
	}
	if code, _, e := runCLI(t, "apply", "--plan", planPath); code != 0 {
		t.Fatalf("apply: %d %s", code, e)
	}

	got, _ := os.ReadFile(filepath.Join(repo, "CODEOWNERS"))
	if string(got) != "# owners\n/x/ @a @b\n" {
		t.Errorf("file = %q", got)
	}

	// Snapshot the working-tree state by committing it first.
	cmd := exec.Command("git", "-C", repo, "commit", "-aqm", "apply")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	if code, _, e := runCLI(t, "snapshot", "--repo", repo, "--out", afterSnap); code != 0 {
		t.Fatalf("snapshot after: %d %s", code, e)
	}

	// In scope: passes.
	if code, _, e := runCLI(t, "verify", "--before", beforeSnap, "--after", afterSnap, "--scope", "/x/"); code != 0 {
		t.Errorf("verify with scope: %d %s", code, e)
	}
	// No declared scope: the change is a violation → exit 2.
	if code, _, _ := runCLI(t, "verify", "--before", beforeSnap, "--after", afterSnap); code != cli.ExitRefused {
		t.Errorf("verify without scope: want exit 2")
	}
}

// SPEC R-17: exit 1 no-op, exit 3 invalid input.
func TestExitCodes_NoOpAndInvalid(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "/x/ @a\n",
		"x/a.go":     "",
	})
	// Owner already present → no-op → 1.
	if code, _, _ := runCLI(t, "plan", "--repo", repo, "--op", "add_owner(/x/, @a)"); code != cli.ExitNoOp {
		t.Errorf("no-op plan: want exit 1, got %d", code)
	}
	// T-7: zero-match scope → 3.
	if code, _, _ := runCLI(t, "plan", "--repo", repo, "--op", "add_owner(/ghost/, @b)"); code != cli.ExitInvalid {
		t.Errorf("zero-match scope: want exit 3, got %d", code)
	}
	// Malformed op → 3.
	if code, _, _ := runCLI(t, "plan", "--repo", repo, "--op", "frob(/x/, @b)"); code != cli.ExitInvalid {
		t.Errorf("malformed op: want exit 3, got %d", code)
	}
}

// SPEC R-17 exit 4: offline audit findings.
func TestExitCodes_AuditFindings(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/Ghost/ @a\n* @b\n",
		"ghost/x.go":         "",
	})
	code, out, _ := runCLI(t, "audit", "--repo", repo, "--checks", "a4,a5,a9")
	if code != cli.ExitFindings {
		t.Fatalf("want exit 4, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "A-5") {
		t.Errorf("expected A-5 case finding:\n%s", out)
	}
}

// SPEC R-12/T-8: requesting API checks without credentials is inconclusive
// (exit 5), never silently skipped.
func TestExitCodes_ApiChecksWithoutToken(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @org/team\n",
		"a.txt":      "",
	})
	if code, _, _ := runCLI(t, "audit", "--repo", repo, "--checks", "a1", "--token", ""); code != cli.ExitInconclusive {
		t.Errorf("api checks without token: want exit 5, got %d", code)
	}
}

// A clean audit exits 0.
func TestExitCodes_AuditClean(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @a\n",
		"a.txt":              "",
	})
	if code, out, _ := runCLI(t, "audit", "--repo", repo, "--checks", "a4,a5,a6,a7,a8,a10,a12"); code != 0 {
		t.Errorf("clean audit: want 0, got %d\n%s", code, out)
	}
}

// Review finding: `--checks A-4` — the canonical dashed spelling the tool
// itself prints — used to normalize to "A--4", silently disabling every
// check and exiting 0 "audit clean". All spellings must work, and an
// unknown check name must be a hard error, never a vacuous pass.
func TestExitCodes_ChecksSpellings(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/ghost/ @a\n* @b\n",
		"real.txt":           "",
	})
	for _, spelling := range []string{"a4", "A4", "a-4", "A-4"} {
		if code, out, _ := runCLI(t, "audit", "--repo", repo, "--checks", spelling); code != cli.ExitFindings {
			t.Errorf("--checks %s: want exit 4 (A-4 finding), got %d\n%s", spelling, code, out)
		}
	}
	if code, _, _ := runCLI(t, "audit", "--repo", repo, "--checks", "a99"); code != cli.ExitInvalid {
		t.Error("unknown check must be invalid input (exit 3), not a vacuous clean audit")
	}
	if code, _, _ := runCLI(t, "audit", "--repo", repo, "--checks", "bogus"); code != cli.ExitInvalid {
		t.Error("garbage check name must be invalid input (exit 3)")
	}
}

// Review finding: a typo'd --scope must be a loud exit-3 error, not silently
// dropped (which turned every change into an inexplicable violation).
func TestExitCodes_VerifyBadScope(t *testing.T) {
	dir := t.TempDir()
	snap := filepath.Join(dir, "s.json")
	if err := os.WriteFile(snap, []byte(`{"ownership":{"a.go":["@x"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI(t, "verify", "--before", snap, "--after", snap, "--scope", "!bad")
	if code != cli.ExitInvalid {
		t.Errorf("bad scope: want exit 3, got %d", code)
	}
	if !strings.Contains(errOut, "invalid --scope") {
		t.Errorf("stderr must name the bad scope: %s", errOut)
	}
}
