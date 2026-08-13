package cli_test

// The two repository guards `audit --lint` shares with `sync`, found by
// adversarial review of the feature after it was written and green.
//
// Both are the same failure in two spellings, and it is the failure the whole
// tool exists to prevent: a run that resolves ownership against ONE tree and
// writes the file governed by ANOTHER, then reports "applied", exit 0. Nothing
// about it looks wrong from inside — the plan is proven, the invariants hold,
// the write succeeds — because every one of those statements is true about the
// wrong tree.
//
// SPEC S-7: CODEOWNERS is per-branch; resolution runs against a specific ref's
// tree. What is on disk is whatever is checked out, and the two are only the
// same tree when the clone is standing on that ref.
// SPEC S-8: GitHub loads the CODEOWNERS at the repository ROOT. A file of the
// same name one directory down governs nothing.
//
// `plan` is deliberately exempt from both: it emits an artifact and writes no
// CODEOWNERS, so proving against another ref is exactly its job.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// lintGit runs a git command in dir, failing the test on error. Committing
// mid-test is how these cases build a second ref to point --branch at.
func lintGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A --branch that is not what the clone has checked out must refuse to write.
//
// This is not a hypothetical ordering nicety. `--branch` selects the tree lint
// proves against, but the bytes come from — and go back to — the working tree.
// Point it at a ref where a directory does not exist and every rule governing
// that directory looks stale; with --remove-stale-paths those lines are
// deleted, and the file that lands on disk has just un-owned a directory
// sitting right there in the checkout, at exit 0 with "applied" on stdout.
// Before this guard existed the run below deleted `/y/ @org/live` while
// `y/b.go` was present on HEAD.
//
// Refusing is chosen over quietly implying --dry-run for the reason sync
// documents: a run that exits 0 having written nothing reads, to the script
// running it, as "this repo was already clean".
func TestLint_RefusesWritingWhileProvingAgainstAnotherRef(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @org/live\n/y/ @org/live\n",
		"x/a.go":      "package x\n",
		"y/b.go":      "package y\n",
	})
	// A second ref whose tree has no y/ at all.
	lintGit(t, repo, "checkout", "-q", "-b", "other")
	lintGit(t, repo, "rm", "-q", "-r", "y")
	lintGit(t, repo, "commit", "-qm", "drop y")
	lintGit(t, repo, "checkout", "-q", "main")

	path := lintOwnersPath(repo)
	before := "/x/ @ org/live\n/y/ @org/live\n"
	lintWrite(t, path, before)

	api := lintAPI(t, nil, "org/live")
	code, out, errb := lintRun(t, repo, api, "--branch", "other",
		"--remove-stale-paths", "--on-empty", "unowned")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (refused); stdout=%q stderr=%q", code, out, errb)
	}
	lintUnchanged(t, "non-HEAD --branch", path, before)
	lintMentions(t, "refusal", errb+out, "not what this clone has checked out")
}

// --dry-run against another ref stays allowed: it writes nothing, so there is
// no file to land in the wrong tree, and previewing what a rollout would do on
// another branch is a legitimate thing to want.
func TestLint_DryRunAgainstAnotherRefIsAllowed(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @org/live\n",
		"x/a.go":      "package x\n",
	})
	lintGit(t, repo, "checkout", "-q", "-b", "other")
	lintGit(t, repo, "commit", "-q", "--allow-empty", "-m", "second ref")
	lintGit(t, repo, "checkout", "-q", "main")

	path := lintOwnersPath(repo)
	before := "/x/ @ org/live\n"
	lintWrite(t, path, before)

	api := lintAPI(t, nil, "org/live")
	code, out, errb := lintRun(t, repo, api, "--branch", "other", "--dry-run")
	if code != 4 {
		t.Errorf("exit = %d, want 4 (fixes pending); stdout=%q stderr=%q", code, out, errb)
	}
	lintUnchanged(t, "--dry-run", path, before)
}

// A --branch naming a DIFFERENT ref that points at the same commit still
// writes. The guard compares resolved commits rather than ref names for the
// reason sync documents: `--branch main` on a clone standing on main is an
// entirely ordinary invocation, and rejecting it by string comparison would
// break every caller that spells HEAD out.
func TestLint_NamedBranchAtTheSameCommitStillWrites(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @org/live\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)
	lintWrite(t, path, "/x/ @ org/live\n")

	api := lintAPI(t, nil, "org/live")
	code, out, errb := lintRun(t, repo, api, "--branch", "main")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, out, errb)
	}
	if got := lintRead(t, path); got != "/x/ @org/live\n" {
		t.Errorf("file = %q, want the repaired handle", got)
	}
}

// A --repo one level below the repository root must refuse.
//
// git walks UP to the enclosing repository rather than refusing, and reports
// tracked paths relative to that ROOT. So discovery pointed at repo/sub finds
// the ROOT's `.github/CODEOWNERS` in the tree, and joining that repo-relative
// path back onto `--repo sub` addresses `sub/.github/CODEOWNERS` — a DIFFERENT
// file that happens to share the name. The run then reads one file, rewrites
// another, prints the first one's path, and leaves the file GitHub actually
// loads untouched (S-8). A fleet whose clone layout carries one extra
// directory level does that to every repository and reports every one a
// success.
func TestLint_RefusesRepoBelowTheRepositoryRoot(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel:            "/sub/x/ @org/live\n",
		"sub/.github/CODEOWNERS": "/x/ @org/live\n/nope/ @org/live\n",
		"sub/x/a.go":             "package x\n",
	})
	sub := filepath.Join(repo, "sub")
	subOwners := filepath.Join(sub, ".github", "CODEOWNERS")
	rootOwners := lintOwnersPath(repo)

	subBefore := "/x/ @ org/live\n/nope/ @org/live\n"
	lintWrite(t, subOwners, subBefore)
	rootBefore := lintRead(t, rootOwners)

	api := lintAPI(t, nil, "org/live")
	code, out, errb := lintRun(t, sub, api, "--remove-stale-paths", "--on-empty", "unowned")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (refused); stdout=%q stderr=%q", code, out, errb)
	}
	lintUnchanged(t, "--repo below the root (subdirectory file)", subOwners, subBefore)
	lintUnchanged(t, "--repo below the root (governing file)", rootOwners, rootBefore)
	lintMentions(t, "refusal", errb+out, "not that root")
}

// The root guard holds under --dry-run too, unlike the branch guard.
//
// The two are asymmetric on purpose. --dry-run makes the branch mismatch
// harmless — nothing is written, so nothing lands in the wrong tree — but it
// does NOT make a wrong --repo harmless: the preview would be computed from
// the bytes of a file that governs nothing, so every line of it is a claim
// about the wrong document. A preview nobody can act on is worse than a
// refusal, because it looks like an answer.
func TestLint_RootGuardAppliesUnderDryRun(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel:            "/sub/x/ @org/live\n",
		"sub/.github/CODEOWNERS": "/x/ @ org/live\n",
		"sub/x/a.go":             "package x\n",
	})
	sub := filepath.Join(repo, "sub")
	api := lintAPI(t, nil, "org/live")
	code, out, errb := lintRun(t, sub, api, "--dry-run")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (refused even under --dry-run); stdout=%q stderr=%q", code, out, errb)
	}
}
