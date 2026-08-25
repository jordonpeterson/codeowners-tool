package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// commitAll stages and commits the current worktree, so a test can take a
// second snapshot from a ref that differs from the first by more than
// CODEOWNERS. snapshot reads the COMMITTED tree, so an uncommitted edit is
// invisible to it — the same hygiene rule docs/GUIDE.md states.
func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// SPEC R-18: the documented CI recipe — snapshot two refs, verify against the
// declared scope — survives a pull request that adds a file, with CODEOWNERS
// byte-identical. This is the whole reason the gate exists, run end to end
// through the real command surface rather than through Compare alone.
//
// It used to exit 2 with "INVARIANT VIOLATED: 1 path(s) changed outside the
// declared scope", because two snapshots taken from different refs differ in
// their trees on every real pull request.
func TestVerify_AddedFilePassesTheDocumentedRecipe(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "/services/api/ @org/api\n* @org/all\n",
		"services/api/main.ts": "x\n",
		"web/app.js":           "y\n",
	})
	before := filepath.Join(t.TempDir(), "before.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", before); code != 0 {
		t.Fatalf("snapshot before: %d %s", code, e)
	}

	// A pull request that adds a file and touches nothing else.
	if err := os.WriteFile(filepath.Join(dir, "web", "newfile.js"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, dir, "add a file")

	after := filepath.Join(t.TempDir(), "after.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", after); code != 0 {
		t.Fatalf("snapshot after: %d %s", code, e)
	}

	code, out, errOut := runCLI(t, "verify", "--before", before, "--after", after, "--scope", "/services/api/")
	if code != cli.ExitOK {
		t.Fatalf("adding a file must not fail the gate: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "added:   web/newfile.js") {
		t.Errorf("the added path must still be REPORTED, got:\n%s", out)
	}
	if strings.Contains(errOut, "INVARIANT VIOLATED") {
		t.Errorf("no invariant was violated: %s", errOut)
	}
}

// The gate is not weakened by the above. A CODEOWNERS edit that reassigns a
// subtree still fails, and adding a file to that same subtree in the same
// commit does not launder it — the reassignment shows on the subtree's
// PRE-EXISTING files, which have a before for INV-2 to be about.
func TestVerify_AddedFileDoesNotLaunderAReassignment(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "/services/api/ @org/api\n* @org/all\n",
		"services/api/main.ts": "x\n",
		"web/app.js":           "y\n",
	})
	before := filepath.Join(t.TempDir(), "before.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", before); code != 0 {
		t.Fatalf("snapshot before: %d %s", code, e)
	}

	// Reassign /web/ out of scope AND add a file under it, in one commit.
	co := filepath.Join(dir, ".github", "CODEOWNERS")
	if err := os.WriteFile(co, []byte("/services/api/ @org/api\n* @org/all\n/web/ @attacker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "new.js"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, dir, "reassign web and add a file")

	after := filepath.Join(t.TempDir(), "after.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", after); code != 0 {
		t.Fatalf("snapshot after: %d %s", code, e)
	}

	code, out, errOut := runCLI(t, "verify", "--before", before, "--after", after, "--scope", "/services/api/")
	if code != cli.ExitRefused {
		t.Fatalf("an out-of-scope reassignment must still fail: exit %d\nstdout: %s", code, out)
	}
	if !strings.Contains(errOut, "web/app.js") {
		t.Errorf("the violation must name the pre-existing path, got: %s", errOut)
	}
	if strings.Contains(errOut, "web/new.js") {
		t.Errorf("the ADDED path is not the violation and must not be listed as one: %s", errOut)
	}
}

// A path leaving the tree is the mirror case: no after, so nothing INV-2
// preserves, so not a violation — but still reported.
func TestVerify_RemovedFileIsReportedNotFatal(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/all\n",
		"keep.go":            "x\n",
		"gone.go":            "y\n",
	})
	before := filepath.Join(t.TempDir(), "before.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", before); code != 0 {
		t.Fatalf("snapshot before: %d %s", code, e)
	}

	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	commitAll(t, dir, "delete a file")

	after := filepath.Join(t.TempDir(), "after.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", after); code != 0 {
		t.Fatalf("snapshot after: %d %s", code, e)
	}

	// No --scope at all: the strictest mode, "assert nothing changed".
	code, out, errOut := runCLI(t, "verify", "--before", before, "--after", after)
	if code != cli.ExitOK {
		t.Fatalf("a deleted path must not fail even the no-scope gate: exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "removed: gone.go") {
		t.Errorf("the removed path must be reported, got:\n%s", out)
	}
}
