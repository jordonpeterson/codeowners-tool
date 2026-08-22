// Package fleet tests tools/fleet/co-own.sh, the per-repo fleet script that
// makes an owner a shared — never exclusive — owner of a scope.
//
// The script exists for one rollout shape: a fleet-wide line such as
// ".github/workflows @org/platform" was added everywhere, and the intent has
// changed to "platform still owns it, but together with whoever owns the
// surrounding tree". The surrounding owners differ per repo, so no single
// policy file can express the end state; the script derives it per repo and
// proves it with the tool before reporting success.
package fleet

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	scope = ".github/workflows"
	owner = "@org/platform"
)

var toolPath string // set in TestMain; the script runs the real binary

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "coown-tool")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	toolPath = filepath.Join(dir, "codeowners-tool")
	build := exec.Command("go", "build", "-o", toolPath, "../../cmd/codeowners-tool")
	if out, err := build.CombinedOutput(); err != nil {
		panic("build codeowners-tool: " + err.Error() + "\n" + string(out))
	}
	os.Exit(m.Run())
}

func needBinaries(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"git", "jq", "awk", "bash"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; the script cannot run here", bin)
		}
	}
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// mkRepo builds a committed single-branch repository whose tracked files are
// exactly the given map, plus .github/CODEOWNERS with the given content.
func mkRepo(t *testing.T, codeowners string, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	for path, body := range files {
		full := filepath.Join(repo, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if codeowners != "" {
		full := filepath.Join(repo, ".github", "CODEOWNERS")
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(codeowners), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "init")
	return repo
}

type result struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
	Branch string `json:"branch"`
}

// runScript executes co-own.sh against repo and returns its parsed JSON
// result line and exit code.
func runScript(t *testing.T, repo string) (result, int) {
	t.Helper()
	script, err := filepath.Abs("co-own.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, "--repo", repo, "--scope", scope, "--owner", owner)
	cmd.Env = append(os.Environ(), "CODEOWNERS_TOOL="+toolPath,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	stdout, runErr := cmd.Output()
	code := 0
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("run co-own.sh: %v", runErr)
		}
		code = ee.ExitCode()
		t.Logf("stderr:\n%s", ee.Stderr)
	}
	var res result
	line := strings.TrimSpace(string(stdout))
	if line != "" {
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			t.Fatalf("stdout is not one JSON object: %q (%v)", line, err)
		}
	}
	return res, code
}

// owners resolves the repo with the real tool and returns the owner list for
// one tracked path, so assertions are about resolved ownership — what GitHub
// would do — never about the file's bytes.
func owners(t *testing.T, repo, path string) []string {
	t.Helper()
	cmd := exec.Command(toolPath, "snapshot", "--repo", repo)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var snap struct {
		Ownership map[string][]string `json:"ownership"`
	}
	if err := json.Unmarshal(out, &snap); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	return snap.Ownership[path]
}

func sameOwners(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	set := map[string]bool{}
	for _, o := range got {
		set[o] = true
	}
	for _, o := range want {
		if !set[o] {
			return false
		}
	}
	return true
}

// The canonical fleet shape: the exclusive line has a broader rule behind it
// that already lists the owner, so deleting the line alone reaches the end
// state. Keeping the line instead would leave the owner exclusive; amending
// the broader rule instead would move ownership of paths outside the scope.
func TestCoOwn_ExclusiveLineRemovedWhenFallbackKeepsOwner(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		"* @org/team_a\n.github @org/team_a @org/platform\n.github/workflows @org/platform\n",
		map[string]string{".github/workflows/ci.yml": "x", ".github/dependabot.yml": "x", "src/main.go": "x"})
	base := git(t, repo, "rev-parse", "HEAD")

	res, code := runScript(t, repo)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (result: %+v)", code, res)
	}
	if res.Status != "shared" {
		t.Fatalf("status %q, want \"shared\"", res.Status)
	}
	if got := owners(t, repo, ".github/workflows/ci.yml"); !sameOwners(got, "@org/team_a", "@org/platform") {
		t.Fatalf("workflows owners after: %v, want exactly {@org/team_a, @org/platform}", got)
	}
	// Out-of-scope ownership must not move: that is the whole safety claim.
	if got := owners(t, repo, "src/main.go"); !sameOwners(got, "@org/team_a") {
		t.Fatalf("src/main.go owners moved to %v — the script edited outside its scope", got)
	}
	if got := owners(t, repo, ".github/dependabot.yml"); !sameOwners(got, "@org/team_a", "@org/platform") {
		t.Fatalf(".github/dependabot.yml owners moved to %v — the script edited outside its scope", got)
	}
	body, err := os.ReadFile(filepath.Join(repo, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), ".github/workflows") {
		t.Fatalf("the exclusive line survived:\n%s", body)
	}
	// One commit on a new branch; the original branch still points at base, so
	// the fleet loop can push the branch and open a PR without touching main.
	if res.Branch == "" {
		t.Fatal("result names no branch to push")
	}
	if cur := git(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); cur != res.Branch {
		t.Fatalf("HEAD is on %q, want the reported branch %q", cur, res.Branch)
	}
	if git(t, repo, "rev-parse", "main") != base {
		t.Fatal("main moved; the change must live only on the work branch")
	}
	if parent := git(t, repo, "rev-parse", "HEAD~1"); parent != base {
		t.Fatal("more than one commit on the work branch; reviewers get one hunk per repo")
	}
	if dirty := git(t, repo, "status", "--porcelain"); dirty != "" {
		t.Fatalf("working tree left dirty:\n%s", dirty)
	}
}

// When the broader rule does NOT list the owner, deleting the line would
// remove the owner from the scope entirely — the opposite of "stays an
// owner". The correct move inverts: keep the line and add the broader
// team(s) to it, so the scope ends co-owned without touching the broader rule.
func TestCoOwn_LineAmendedWhenFallbackLacksOwner(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		"* @org/team_a\n.github @org/team_a\n.github/workflows @org/platform\n",
		map[string]string{".github/workflows/ci.yml": "x", ".github/dependabot.yml": "x", "src/main.go": "x"})

	res, code := runScript(t, repo)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (result: %+v)", code, res)
	}
	if res.Status != "shared" {
		t.Fatalf("status %q, want \"shared\"", res.Status)
	}
	if got := owners(t, repo, ".github/workflows/ci.yml"); !sameOwners(got, "@org/team_a", "@org/platform") {
		t.Fatalf("workflows owners after: %v, want exactly {@org/team_a, @org/platform}", got)
	}
	// The broader rule must be untouched: granting @org/platform the rest of
	// .github would be a silent widening nobody asked for.
	if got := owners(t, repo, ".github/dependabot.yml"); !sameOwners(got, "@org/team_a") {
		t.Fatalf(".github/dependabot.yml owners became %v — the broader rule was widened", got)
	}
}

// With nothing behind the line, both moves fail the intent: deleting it
// leaves the scope unowned, and re-adding the owner restores exclusivity.
// Someone has to decide who co-owns, so the script must refuse (exit 2) and
// hand the repo back exactly as it found it.
func TestCoOwn_NothingBehindTheLineNeedsAHuman(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		".github/workflows @org/platform\n",
		map[string]string{".github/workflows/ci.yml": "x", "src/main.go": "x"})
	base := git(t, repo, "rev-parse", "HEAD")

	res, code := runScript(t, repo)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (result: %+v)", code, res)
	}
	if res.Status != "needs-human" {
		t.Fatalf("status %q, want \"needs-human\"", res.Status)
	}
	if head := git(t, repo, "rev-parse", "HEAD"); head != base {
		t.Fatal("HEAD moved; a refused repo must be handed back untouched")
	}
	if cur := git(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); cur != "main" {
		t.Fatalf("left on branch %q, want main", cur)
	}
	if dirty := git(t, repo, "status", "--porcelain"); dirty != "" {
		t.Fatalf("working tree left dirty:\n%s", dirty)
	}
}

// A repo where the invariant already holds gets no branch and no commit —
// "already correct" must stay indistinguishable from "never touched", or a
// second wave opens a hundred empty PRs.
func TestCoOwn_AlreadySharedIsUnchanged(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		"* @org/team_a\n.github/workflows @org/platform @org/team_a\n",
		map[string]string{".github/workflows/ci.yml": "x", "src/main.go": "x"})
	base := git(t, repo, "rev-parse", "HEAD")

	res, code := runScript(t, repo)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (result: %+v)", code, res)
	}
	if res.Status != "unchanged" {
		t.Fatalf("status %q, want \"unchanged\"", res.Status)
	}
	if head := git(t, repo, "rev-parse", "HEAD"); head != base {
		t.Fatal("HEAD moved on an already-correct repo")
	}
	if out := git(t, repo, "branch", "--list"); strings.TrimSpace(out) != "* main" {
		t.Fatalf("a branch was created on an already-correct repo:\n%s", out)
	}
}

// A repo the rollout never reached has no matching files; there is nothing to
// share and nothing to prove. Skipped, exit 0, untouched.
func TestCoOwn_ScopeWithNoFilesIsSkipped(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		"* @org/team_a\n",
		map[string]string{"src/main.go": "x"})
	base := git(t, repo, "rev-parse", "HEAD")

	res, code := runScript(t, repo)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (result: %+v)", code, res)
	}
	if res.Status != "skipped" {
		t.Fatalf("status %q, want \"skipped\"", res.Status)
	}
	if head := git(t, repo, "rev-parse", "HEAD"); head != base {
		t.Fatal("HEAD moved on a repo with nothing in scope")
	}
}

// The owner not owning the scope at all is the same decision as case 3 worn
// differently: the script's mandate is to remove exclusivity, not to grant
// ownership, and guessing that a grant was wanted would silently widen a
// hundred repos. Skipped, exit 0, with the reason in the record.
func TestCoOwn_OwnerAbsentFromScopeIsSkipped(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		"* @org/team_a\n.github/workflows @org/team_b\n",
		map[string]string{".github/workflows/ci.yml": "x", "src/main.go": "x"})

	res, code := runScript(t, repo)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (result: %+v)", code, res)
	}
	if res.Status != "skipped" {
		t.Fatalf("status %q, want \"skipped\"", res.Status)
	}
	if got := owners(t, repo, ".github/workflows/ci.yml"); !sameOwners(got, "@org/team_b") {
		t.Fatalf("ownership moved to %v on a repo the script had no mandate in", got)
	}
}

// Uncommitted changes mean the clone is not in the state the proof would be
// about; committing them for the user or working around them both write
// history nobody reviewed. Refuse per-repo (exit 2), touch nothing.
func TestCoOwn_DirtyWorkingTreeRefused(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		"* @org/team_a\n.github @org/team_a @org/platform\n.github/workflows @org/platform\n",
		map[string]string{".github/workflows/ci.yml": "x"})
	if err := os.WriteFile(filepath.Join(repo, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, code := runScript(t, repo)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (result: %+v)", code, res)
	}
	if res.Status != "needs-human" {
		t.Fatalf("status %q, want \"needs-human\"", res.Status)
	}
	if _, err := os.Stat(filepath.Join(repo, "stray.txt")); err != nil {
		t.Fatal("the user's uncommitted file is gone")
	}
}
