package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// BLOCKER 1 — the write must not leave the repository.
//
// containedRelPath already closes one spelling of this: `--file ../ESCAPED/CODEOWNERS`
// is rejected before the repo is opened, and its comment names the consequence
// exactly — "a fleet loop pointed at 100 clones writes 100 files into whatever
// happens to sit next to them". checkRepoRoot closes a second, where --repo points
// below the root and the file lands where GitHub never reads it.
//
// A third spelling is open, and unlike the other two it needs no flag: a committed
// symlink. apply.Apply calls filepath.EvalSymlinks and writes the RESOLVED target,
// so a repository whose governing CODEOWNERS is a symlink to ../../outside/f edits
// `f` — outside the clone — and reports "applied", exit 0.
//
// In the fleet model this verb exists for, the symlink travels IN the repository:
// anyone with commit access to any one repo in the fleet chooses a path the central
// runner writes to. The primitive is constrained (the target must already exist, its
// bytes must match the hash the planner pinned, and the appended text is
// CODEOWNERS-shaped) but the PATH is arbitrary.
//
// It is also wrong with no attacker at all. GitHub does not follow a symlinked
// CODEOWNERS, so the file this run edits governs nothing, while the run reports
// success — the "applied, dead on arrival" outcome the whole verb exists to prevent.
//
// Refusing outright and containing to the repo root are both acceptable fixes; these
// tests pin only that the bytes outside the repository are never touched and that no
// record claims otherwise.

// symlinkRepo builds a repository whose governing CODEOWNERS at coPath is a
// committed symlink to linkTarget (interpreted relative to coPath's directory).
func symlinkRepo(t *testing.T, coPath, linkTarget string, files map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevated privileges on Windows")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	for path, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, filepath.FromSlash(coPath))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.FromSlash(linkTarget), link); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	return dir
}

// outsideVictim writes a file as a SIBLING of repoDir and returns its path plus
// its original bytes. A sibling is the realistic shape: fleet clones sit next to
// each other under one parent, which is what "../../" reaches.
func outsideVictim(t *testing.T, repoDir, name, content string) (string, string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(repoDir), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, content
}

// A committed symlink must not turn `sync` into a writer outside the clone.
func TestContainment_SyncNeverWritesThroughASymlinkLeavingTheRepo(t *testing.T) {
	sym := symlinkRepo(t, ".github/CODEOWNERS", "../../VICTIM.txt", map[string]string{
		"svc/a.go": "package svc\n",
	})
	victim, original := outsideVictim(t, sym, "VICTIM.txt", "/svc/ @org/old\n")

	code, out, errOut := runCLI(t, "sync", "--repo", sym,
		"--op", "add_owner(/svc/, @org/new)", "--format", "json")

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("the file OUTSIDE the repository was modified.\n path: %s\n was:  %q\n now:  %q\nA committed symlink is a path chosen by whoever can push to the repo; the writer must stay inside --repo.",
			victim, original, got)
	}
	if code == cli.ExitOK {
		t.Errorf("exit 0: the run reported success while editing a file GitHub never reads (a symlinked CODEOWNERS does not govern) — want a refusal (exit %d)\nstdout: %s\nstderr: %s",
			cli.ExitRefused, out, errOut)
	}
	if strings.TrimSpace(out) != "" {
		rec := syncDecodeRecord(t, out)
		if rec.Status == cli.StatusApplied {
			t.Errorf("record status = %q: a fleet aggregating results.jsonl would count this repo as converged", rec.Status)
		}
	}
}

// The same escape must not be reachable through plan → apply. `plan` is the
// artifact-producing verb and writes no CODEOWNERS, so the containment decision
// has to hold at apply time too — a plan reviewed and merged in one place is
// applied somewhere else entirely.
func TestContainment_ApplyNeverWritesThroughASymlinkLeavingTheRepo(t *testing.T) {
	sym := symlinkRepo(t, ".github/CODEOWNERS", "../../VICTIM.txt", map[string]string{
		"svc/a.go": "package svc\n",
	})
	victim := filepath.Join(filepath.Dir(sym), "VICTIM.txt")
	original := "/svc/ @org/old\n"
	if err := os.WriteFile(victim, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, errOut := runCLI(t, "plan", "--repo", sym,
		"--op", "add_owner(/svc/, @org/new)", "--out", planPath); code != cli.ExitOK {
		// If plan itself refuses, containment already holds here; nothing to apply.
		t.Skipf("plan refused (exit %d), so apply is unreachable: %s", code, errOut)
	}

	code, _, errOut := runCLI(t, "apply", "--plan", planPath)

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("apply modified the file OUTSIDE the repository.\n path: %s\n was:  %q\n now:  %q",
			victim, original, got)
	}
	if code == cli.ExitOK {
		t.Errorf("exit 0: apply reported success writing outside the repository\nstderr: %s", errOut)
	}
}

// `plan` writes no CODEOWNERS, so reading through an escaping symlink is not an
// escape. It is still a lie in an artifact: the plan's `codeowners_path` names a
// file outside the repository — one GitHub never reads, because it does not
// follow a symlinked CODEOWNERS — and the plan is the thing a human reviews and
// approves. Every downstream refusal (sync's, apply's) fires AFTER that review.
//
// Containment belongs at plan time for the same reason the tool refuses rather
// than warns everywhere else: the artifact should never exist to be approved.
func TestContainment_PlanRefusesToPlanThroughASymlinkLeavingTheRepo(t *testing.T) {
	sym := symlinkRepo(t, ".github/CODEOWNERS", "../../VICTIM.txt", map[string]string{
		"svc/a.go": "package svc\n",
	})
	outsideVictim(t, sym, "VICTIM.txt", "/svc/ @org/old\n")

	planPath := filepath.Join(t.TempDir(), "plan.json")
	code, out, errOut := runCLI(t, "plan", "--repo", sym,
		"--op", "add_owner(/svc/, @org/new)", "--out", planPath)

	if code == cli.ExitOK {
		t.Errorf("exit 0: plan produced a reviewable artifact whose codeowners_path is outside the repository, at a file GitHub does not read\nstdout: %s\nstderr: %s", out, errOut)
	}
	if code != cli.ExitRefused {
		t.Errorf("exit %d, want %d (refused): the repo was read fine and the tool is declining to act on it, which is a refusal, not malformed input",
			code, cli.ExitRefused)
	}
	if _, err := os.Stat(planPath); err == nil {
		t.Errorf("plan wrote %s anyway: an artifact that exists is an artifact that gets reviewed and applied", planPath)
	}
}

// With plan refusing, the plan→apply chain above stops early — so apply's own
// containment needs a test that does not depend on plan producing the escape.
//
// This is the realistic shape anyway. A plan is reviewed in one place and applied
// somewhere else entirely, possibly against a different clone via --repo, so the
// bytes that reach `apply` are not necessarily the bytes `plan` emitted.
func TestContainment_ApplyRefusesAPlanWhosePathEscapesTheRepo(t *testing.T) {
	repo := symlinkRepoless(t, map[string]string{
		".github/CODEOWNERS": "/svc/ @org/old\n",
		"svc/a.go":           "package svc\n",
	})
	victim, original := outsideVictim(t, repo, "VICTIM.txt", "/svc/ @org/old\n")

	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, errOut := runCLI(t, "plan", "--repo", repo,
		"--op", "add_owner(/svc/, @org/new)", "--out", planPath); code != cli.ExitOK {
		t.Fatalf("building the baseline plan failed: exit %d: %s", code, errOut)
	}

	// Repoint the artifact at a file outside the clone, exactly as a tampered or
	// hand-edited plan would.
	b, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var pf map[string]any
	if err := json.Unmarshal(b, &pf); err != nil {
		t.Fatal(err)
	}
	pf["codeowners_path"] = "../VICTIM.txt"
	b, err = json.Marshal(pf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runCLI(t, "apply", "--plan", planPath)

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("apply followed the plan's path out of the repository.\n path: %s\n was:  %q\n now:  %q", victim, original, got)
	}
	if code == cli.ExitOK {
		t.Errorf("exit 0: apply reported success for a path outside --repo\nstderr: %s", errOut)
	}
}

// symlinkRepoless is symlinkRepo without the symlink: an ordinary committed repo.
func symlinkRepoless(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	for path, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	return dir
}

// --create is already safe (O_EXCL), but only because the file does not exist.
// A DANGLING symlink is the create path's version of the same escape: the link
// resolves to nothing, so O_EXCL succeeds, and the new file lands outside.
func TestContainment_CreateNeverFollowsADanglingSymlinkOutOfTheRepo(t *testing.T) {
	sym := symlinkRepo(t, ".github/CODEOWNERS", "../../NOT-YET.txt", map[string]string{
		"svc/a.go": "package svc\n",
	})
	outside := filepath.Join(filepath.Dir(sym), "NOT-YET.txt")

	code, _, errOut := runCLI(t, "sync", "--repo", sym, "--create",
		"--op", "add_owner(/svc/, @org/new)", "--format", "json")

	if _, err := os.Stat(outside); err == nil {
		t.Errorf("--create wrote a new file OUTSIDE the repository at %s", outside)
	}
	if code == cli.ExitOK {
		t.Errorf("exit 0 with a dangling symlink as the governing CODEOWNERS; want a refusal\nstderr: %s", errOut)
	}
}
