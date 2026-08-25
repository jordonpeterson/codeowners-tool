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

func readPlanDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// SPEC R-16: a plan is bound to the repository it was computed in. `--repo`
// used to be taken and the plan's own `repo` field ignored, so a plan computed
// against one clone was applied to another:
//
//	plan  --repo /tmp/repA --op 'add_owner(/services/web/, @org/web-team)' --out planA.json
//	apply --plan planA.json --repo /tmp/repB     # same CODEOWNERS bytes, different tree
//
// Observed: applied, exit 0, and repB/services/web/SECRET.ts — a path the plan
// never mentions — gained @org/web-team. The bytes-differ case WAS caught, but
// identical bootstrap CODEOWNERS across many repos is precisely the situation
// this tool is built for, so the hash collides legitimately.
func TestApply_RefusesADifferentRepository(t *testing.T) {
	same := map[string]string{".github/CODEOWNERS": "* @org/all\n"}
	repoA := initRepo(t, mergeFiles(same, map[string]string{"services/web/app.ts": "x\n"}))
	repoB := initRepo(t, mergeFiles(same, map[string]string{
		"services/web/app.ts":    "x\n",
		"services/web/SECRET.ts": "s\n", // a path the plan never mentions
	}))

	planPath := filepath.Join(t.TempDir(), "planA.json")
	if code, _, e := runCLI(t, "plan", "--repo", repoA, "--op", "add_owner(/services/web/, @org/web-team)", "--out", planPath); code != 0 {
		t.Fatalf("plan: %d %s", code, e)
	}

	code, out, errOut := runCLI(t, "apply", "--plan", planPath, "--repo", repoB)
	if code == cli.ExitOK {
		t.Fatalf("a plan must not be applied to a different repository: exit 0\n%s", out)
	}
	// The one signal an operator gets must point at the real cause. The old
	// bytes-differ refusal said "changed since the plan was computed", which
	// sends them looking for an edit that never happened.
	if !strings.Contains(errOut, "different repository") && !strings.Contains(errOut, "was computed against") {
		t.Errorf("the refusal must name the cause, got: %s", errOut)
	}
	co, _ := os.ReadFile(filepath.Join(repoB, ".github", "CODEOWNERS"))
	if strings.Contains(string(co), "@org/web-team") {
		t.Errorf("repoB was written to:\n%s", co)
	}
}

// SPEC R-16: sha256_before catches a CODEOWNERS that moved between plan and
// apply. Nothing caught a TREE that moved, so the ownership_rows a human
// reviewed — computed at plan time, never revalidated — could understate the
// blast radius. This is the documented plan-in-CI / apply-after-merge
// workflow, not an exotic sequence.
//
// Before: `applied: ... (16 → 59 bytes)`, exit 0, no warning; 3 paths changed
// owners where the reviewed plan declared 1.
func TestApply_RefusesWhenTheTreeMovedUnderThePlan(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "* @org/all\n",
		"services/web/app.ts": "x\n",
	})
	planPath := filepath.Join(t.TempDir(), "planS.json")
	if code, _, e := runCLI(t, "plan", "--repo", dir, "--op", "add_owner(/services/web/, @org/web-team)", "--out", planPath); code != 0 {
		t.Fatalf("plan: %d %s", code, e)
	}
	doc := readPlanDoc(t, planPath)
	if rows, ok := doc["ownership_rows"].([]any); !ok || len(rows) != 1 {
		t.Fatalf("precondition: the reviewed plan should declare exactly 1 path, got %v", doc["ownership_rows"])
	}

	// A colleague merges two more files under the same scope. CODEOWNERS is
	// untouched, so sha256_before still matches.
	for _, name := range []string{"auth.ts", "billing.ts"} {
		if err := os.WriteFile(filepath.Join(dir, "services", "web", name), []byte("y\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitTree(t, dir, "colleague adds two files")

	code, out, errOut := runCLI(t, "apply", "--plan", planPath)
	if code == cli.ExitOK {
		t.Fatalf("a stale plan must not silently exceed its declared blast radius: exit 0\n%s", out)
	}
	if !strings.Contains(errOut, "tree") {
		t.Errorf("the refusal must say the tree moved, got: %s", errOut)
	}
}

// The guard is transparent on the ordinary path: plan, then apply, tree
// unchanged.
func TestApply_UnmovedTreeStillApplies(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "* @org/all\n",
		"services/web/app.ts": "x\n",
	})
	planPath := filepath.Join(t.TempDir(), "p.json")
	if code, _, e := runCLI(t, "plan", "--repo", dir, "--op", "add_owner(/services/web/, @org/web-team)", "--out", planPath); code != 0 {
		t.Fatalf("plan: %d %s", code, e)
	}
	if code, _, errOut := runCLI(t, "apply", "--plan", planPath); code != cli.ExitOK {
		t.Fatalf("an unmoved tree must still apply: exit %d (%s)", code, errOut)
	}
}

// Passing --repo that AGREES with the plan is the ordinary re-statement an
// operator makes, and must not be punished.
func TestApply_MatchingRepoFlagIsAccepted(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "* @org/all\n",
		"services/web/app.ts": "x\n",
	})
	planPath := filepath.Join(t.TempDir(), "p.json")
	if code, _, e := runCLI(t, "plan", "--repo", dir, "--op", "add_owner(/services/web/, @org/web-team)", "--out", planPath); code != 0 {
		t.Fatalf("plan: %d %s", code, e)
	}
	if code, _, errOut := runCLI(t, "apply", "--plan", planPath, "--repo", dir); code != cli.ExitOK {
		t.Errorf("a matching --repo must be accepted: exit %d (%s)", code, errOut)
	}
}

// A plan records the repository as an ABSOLUTE path, so it can be applied from
// any working directory. `plan --repo .` used to record "." verbatim, and
// applying that plan from anywhere else died on raw git plumbing:
//
//	error: git rev-parse --show-toplevel: exit status 128:
//	fatal: not a git repository (or any of the parent directories): .git
func TestApply_PlanFromRelativeRepoAppliesFromAnywhere(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "* @org/all\n",
		"services/web/app.ts": "x\n",
	})
	planPath := filepath.Join(t.TempDir(), "p.json")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI(t, "plan", "--repo", ".", "--op", "add_owner(/services/web/, @org/web-team)", "--out", planPath); code != 0 {
		t.Fatalf("plan: %d %s", code, e)
	}
	if got, _ := readPlanDoc(t, planPath)["repo"].(string); !filepath.IsAbs(got) {
		t.Errorf("plan recorded repo = %q, want an absolute path", got)
	}

	// Apply from somewhere else entirely.
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, "apply", "--plan", planPath); code != cli.ExitOK {
		t.Errorf("a plan made with --repo . must apply from another cwd: exit %d (%s)", code, errOut)
	}
}

// commitTree stages and commits the worktree so a second snapshot/plan sees a
// moved tree. snapshot and plan read the COMMITTED tree, so an uncommitted
// edit is invisible to them.
func commitTree(t *testing.T, dir, msg string) {
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

func mergeFiles(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
