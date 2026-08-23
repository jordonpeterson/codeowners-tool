package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitCommit stages and commits, for the snapshot-pair tests where the whole
// point is what a snapshot can and cannot see.
func gitCommit(t *testing.T, dir, msg string) {
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

// A path present in only one snapshot, unowned in both, is not an ownership
// change. Compare gated equality on `bok && aok` — presence in the tree —
// so adding an ordinary unowned source file between two snapshots failed the
// rollout gate at exit 2, printing the self-contradictory
// "(unowned) → (unowned)" as its evidence.
func TestVerify_NewUnownedPathIsNotAChange(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/src/ @org/api\n",
		"src/a.go":           "package src\n",
	})
	dir := t.TempDir()
	before := filepath.Join(dir, "before.json")
	after := filepath.Join(dir, "after.json")

	if code, _, e := runCLI(t, "snapshot", "--repo", repo, "--out", before); code != 0 {
		t.Fatalf("snapshot before: %d %s", code, e)
	}
	// A new file nobody owns. CODEOWNERS is untouched.
	os.MkdirAll(filepath.Join(repo, "vendor"), 0o755)
	if err := os.WriteFile(filepath.Join(repo, "vendor", "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommit(t, repo, "add an unowned file")
	if code, _, e := runCLI(t, "snapshot", "--repo", repo, "--out", after); code != 0 {
		t.Fatalf("snapshot after: %d %s", code, e)
	}

	code, out, errb := runCLI(t, "verify", "--before", before, "--after", after)
	if code != 0 {
		t.Errorf("verify = %d, want 0 — no rule changed and the new path is unowned on both sides\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if strings.Contains(out+errb, "vendor/new.txt") {
		t.Errorf("vendor/new.txt reported as an ownership change; it is unowned before and after\nstdout: %s\nstderr: %s", out, errb)
	}
}

// The other direction has to keep working: a new file that lands under an
// existing rule really did gain owners, and the gate must still see it.
func TestVerify_NewOwnedPathIsStillAChange(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/src/ @org/api\n",
		"src/a.go":           "package src\n",
	})
	dir := t.TempDir()
	before := filepath.Join(dir, "before.json")
	after := filepath.Join(dir, "after.json")

	if code, _, e := runCLI(t, "snapshot", "--repo", repo, "--out", before); code != 0 {
		t.Fatalf("snapshot before: %d %s", code, e)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "b.go"), []byte("package src\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommit(t, repo, "add a file under an owned rule")
	if code, _, e := runCLI(t, "snapshot", "--repo", repo, "--out", after); code != 0 {
		t.Fatalf("snapshot after: %d %s", code, e)
	}

	code, out, errb := runCLI(t, "verify", "--before", before, "--after", after)
	if code == 0 {
		t.Errorf("verify = 0; src/b.go went from no owner to @org/api and must be reported\nstdout: %s", out)
	}
	if !strings.Contains(out+errb, "src/b.go") {
		t.Errorf("src/b.go missing from the report\nstdout: %s\nstderr: %s", out, errb)
	}
}

// snapshot reads the tree committed at --branch; sync writes the WORKING
// TREE. The obvious CI shape — snapshot, sync, snapshot, verify — therefore
// hashed two identical snapshots and passed green over a real edit. snapshot
// now says so rather than answering a question it cannot see.
func TestSnapshot_DisclosesUncommittedCodeownersEdit(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/src/ @org/api\n",
		"src/a.go":           "package src\n",
		"web/b.js":           "//\n",
	})
	if code, _, e := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/web/, @org/web)"); code != 0 {
		t.Fatalf("sync: %d %s", code, e)
	}
	code, _, errb := runCLI(t, "snapshot", "--repo", repo, "--out", filepath.Join(t.TempDir(), "s.json"))
	if code != 0 {
		t.Fatalf("snapshot = %d, want 0 — the disclosure is a warning, not a refusal: %s", code, errb)
	}
	if !strings.Contains(errb, "working tree") {
		t.Errorf("snapshot said nothing about the uncommitted edit it cannot see; stderr=%q", errb)
	}
}

// The trap end to end: the loop a CI author writes from the synopsis alone
// must not report success over an uncommitted sync.
func TestSnapshotVerifyLoop_DoesNotPassSilentlyOverAnUncommittedSync(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/src/ @org/api\n",
		"src/a.go":           "package src\n",
		"web/b.js":           "//\n",
	})
	dir := t.TempDir()
	before := filepath.Join(dir, "before.json")
	after := filepath.Join(dir, "after.json")

	runCLI(t, "snapshot", "--repo", repo, "--out", before)
	if code, _, e := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/web/, @org/web)"); code != 0 {
		t.Fatalf("sync: %d %s", code, e)
	}
	_, _, snapErr := runCLI(t, "snapshot", "--repo", repo, "--out", after)
	code, out, errb := runCLI(t, "verify", "--before", before, "--after", after)

	// verify compares what it was given, and the two files really are equal;
	// the disclosure has to come from snapshot, at the moment it is asked for
	// a picture of a file the caller has already edited.
	if code == 0 && !strings.Contains(snapErr, "working tree") {
		t.Errorf("the loop reported success over an uncommitted sync and nothing warned\nverify: %s%s\nsnapshot stderr: %q", out, errb, snapErr)
	}
}
