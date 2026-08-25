// Findings about the STATE of the checkout a run is pointed at — a CODEOWNERS
// location occupied by a submodule, a file left unmerged by a conflict, a
// migration staged but not committed, a symlink the reporting verbs do not
// recognize. In each case the tool's model of the repository and git's differ,
// and the run reports success or "clean" anyway.
//
// Each test is a FAILING repro of a confirmed defect, written first per
// CONTRIBUTING.md.
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

// FINDING: when a SUBMODULE is mounted at one of the three CODEOWNERS
// locations, `sync` writes into the submodule's checkout, proves the change
// against the submodule's rules, and reports `applied (proven: tree)` — and
// the parent repository can never track the file it wrote.
//
// The shared-org-`.github` submodule is a real layout. `git ls-tree` reports
// `.github` as a gitlink (`160000 commit`), so FindCodeownersPaths correctly
// finds no CODEOWNERS, and governing()'s working-tree fallback then adopts the
// file inside the submodule's own checkout:
//
//	$ codeowners-tool sync --op 'add_owner(/services/api/, @org/api)' --format json
//	{"codeowners_path":".github/CODEOWNERS","status":"applied",
//	 "changes":[{"old_owners":["@sub/owners"],"new_owners":["@sub/owners","@org/api"]}]}
//	$ git add .github/CODEOWNERS
//	fatal: Pathspec '.github/CODEOWNERS' is in submodule '.github'
//
// `@sub/owners` is a fact about a DIFFERENT repository; the parent has no
// CODEOWNERS at all, so its true INV-2 baseline is "everything unowned".
// `snapshot` and `plan` both exit 3 on this repo, agreeing with git.
//
// refuseSymlinkedTarget refuses the exactly-analogous case and says why: "git
// commits a symlinked directory as a link BLOB, not a tree, so with
// `.github -> real-gh` there is no `.github/CODEOWNERS` in the tree GitHub
// reads … the write would edit a file that governs nothing while reporting
// applied." A gitlink is not a tree either, and there is no guard for it —
// "submodule" does not appear in the write path at all. The evidence needed to
// refuse is already in hand: `.github` is itself a path in the tracked tree.
func TestSubmoduleAtCodeownersLocationIsRefused(t *testing.T) {
	sub := initRepo(t, map[string]string{"CODEOWNERS": "* @sub/owners\n"})
	parent := initRepo(t, map[string]string{"services/api/main.go": "m\n"})

	gitRun(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, ".github")
	gitRun(t, parent, "commit", "-qm", "mount shared .github")

	// git's view: .github is a gitlink, so the parent tracks no CODEOWNERS.
	if out, err := os.ReadFile(filepath.Join(parent, ".gitmodules")); err != nil || !strings.Contains(string(out), ".github") {
		t.Fatalf("fixture: submodule not mounted at .github (%v)", err)
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", parent,
		"--op", "add_owner(/services/api/, @org/api)", "--format", "json")
	out := stdout + stderr
	if code == cli.ExitOK {
		t.Errorf("sync wrote CODEOWNERS inside a submodule and reported success (exit 0)\n"+
			"the parent repository cannot stage that path, so the change can never reach GitHub;\n"+
			"the proof was computed against the SUBMODULE's owners\noutput:\n%s", out)
	}
	if strings.Contains(out, "@sub/owners") {
		t.Errorf("the record's old_owners are the submodule's rules — a fact about a different repository\noutput:\n%s", out)
	}
}

// FINDING: `sync` rewrites a CODEOWNERS left UNMERGED by a conflict and
// reports `applied (proven: tree)`, exit 0.
//
//	$ git status --short
//	UU .github/CODEOWNERS
//	$ codeowners-tool sync --op 'add_owner(/services/api/, @org/api)'
//	warning: … has 2 line(s) GitHub cannot parse and silently skips (S-3) …
//	applied: 1 op(s) applied, 0 skipped; 1 line change(s), 1 path(s) change owners
//	  ops[0]  applied (proven: tree)
//
// The conflict markers are the only thing it notices, and it reads them as
// syntax errors rather than as an unmerged file. `=======` parses as a valid
// zero-owner rule (S-9), and BOTH sides' `/docs/` lines stay live, so the
// "before" ownership the invariants were proven against is a conflict-mangled
// state that no commit has ever had and GitHub will never see. The file is
// still `UU` afterwards — the run reports a proven change to something that
// cannot be committed as it stands.
//
// The tool refuses the adjacent checkout hazards in detail (the sparse-checkout
// refusal names sparse-checkout, partial clones and local deletion by name),
// and S-7's checkBranchIsWritable exists to stop "a rule justified by one tree
// and landing in another". An unmerged index is the same class, and one
// `git status --porcelain` settles it.
func TestUnmergedCodeownersIsNotSilentlyRewritten(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "",
		"docs/x.md":            "",
	})
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, ".github/CODEOWNERS"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "switch", "-qc", "sidebranch")
	write("* @org/everyone\n/docs/ @org/docs-a\n")
	gitRun(t, repo, "commit", "-aqm", "a")
	gitRun(t, repo, "switch", "-q", "main")
	write("* @org/everyone\n/docs/ @org/docs-b\n")
	gitRun(t, repo, "commit", "-aqm", "b")

	// merge is EXPECTED to fail here, so it does not go through gitRun.
	if code, _, _ := runGit(t, repo, "merge", "sidebranch"); code == 0 {
		t.Fatal("fixture: the merge was supposed to conflict")
	}
	if _, status, _ := runGit(t, repo, "status", "--short"); !strings.Contains(status, "UU .github/CODEOWNERS") {
		t.Fatalf("fixture: expected an unmerged CODEOWNERS, git says:\n%s", status)
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/api)")
	out := strings.ToLower(stdout + stderr)
	if code == cli.ExitOK && !strings.Contains(out, "unmerged") && !strings.Contains(out, "conflict") {
		t.Errorf("sync rewrote a file git reports as UU (unmerged) and called the result proven (exit 0)\n"+
			"the ownership it proved against is the conflict-mangled text, which no commit has ever had\n"+
			"output:\n%s%s", stdout, stderr)
	}
}

// FINDING: discovery can only ADD a CODEOWNERS from the working tree, never
// let one OUTRANK the tracked file — so a repo mid-migration from root
// `CODEOWNERS` to `.github/CODEOWNERS` gets its outgoing file edited, and
// `audit` calls the repo clean.
//
//	$ git status --short
//	A  .github/CODEOWNERS          # staged, not yet committed
//	$ codeowners-tool audit
//	audit clean
//	$ codeowners-tool sync --op 'add_owner(/services/api/, @org/api)' --format json
//	{"codeowners_path":"CODEOWNERS","status":"applied", …}
//
// The edit is dead the moment the migration commit lands: under S-8 the staged
// `.github/CODEOWNERS` wins outright. `codeowners_path` then tells the fleet
// loop to stage the file that is about to stop governing.
//
// governing()'s working-tree fallback fires only when the TREE has zero
// CODEOWNERS, and A-10's AllPresent is tree-only as well — so the check whose
// severity is `error` ("GitHub uses only the first") is blind to the working
// tree that sync is about to write into. D5's own comment argues the working
// tree must inform discovery; it informs it in one direction only.
func TestStagedHigherPrecedenceCodeownersIsNotIgnored(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":           "* @org/everyone\n",
		"services/api/main.go": "",
	})
	if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".github/CODEOWNERS"), []byte("* @org/newer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".github/CODEOWNERS")

	if code, stdout, stderr := runCLI(t, "audit", "--repo", repo); code == cli.ExitOK {
		t.Errorf("audit reports clean while a higher-precedence .github/CODEOWNERS sits staged in the working tree;\n"+
			"A-10 is severity error for exactly this and only ever looks at the ref\noutput:\n%s%s", stdout, stderr)
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/api)")
	out := stdout + stderr
	if code == cli.ExitOK && !strings.Contains(out, ".github/CODEOWNERS") {
		t.Errorf("sync edited the outgoing root CODEOWNERS without mentioning the staged .github/CODEOWNERS\n"+
			"that outranks it — the edit dies the moment the migration commit lands\noutput:\n%s", out)
	}
}

// FINDING: for a committed SYMLINKED CODEOWNERS, `sync` refuses with a precise
// explanation while `audit` — the documented CI gate — invents a finding about
// a pattern that is really a filesystem path, and never says "symlink".
//
//	$ codeowners-tool audit
//	[A-4/warning] (line 1) pattern "../OWNERS.real" matches zero tracked files …
//	[A-9/info] 3 of 3 tracked paths (100%) have no owner …
//	[A-11/warning] .github/CODEOWNERS itself has no owner …
//	$ codeowners-tool sync --op 'add_owner(/services/api/, @org/x)'
//	error: refusing to write .github/CODEOWNERS: it is a symlink to ../OWNERS.real,
//	       and GitHub does not follow a symlinked CODEOWNERS …
//
// The read-only verbs fetch the blob with `cat-file`, which hands back the link
// TARGET as content, and nothing checks the tree mode — so the target string is
// parsed as a rule. The operator is left to infer the real defect from "100% of
// tracked paths have no owner". The condition is already known to the tool;
// audit is where it belongs, since audit's job is to report rot and this repo's
// entire ownership is inert.
func TestAuditNamesASymlinkedCodeowners(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"OWNERS.real":          "* @org/everyone\n",
		"services/api/main.go": "",
	})
	if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../OWNERS.real", filepath.Join(repo, ".github/CODEOWNERS")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "symlink the codeowners")

	_, stdout, stderr := runCLI(t, "audit", "--repo", repo)
	out := stdout + stderr
	if !strings.Contains(out, "symlink") {
		t.Errorf("audit never says the governing CODEOWNERS is a symlink, which is why the whole repo is unowned;\n"+
			"sync refuses this same repo and names the symlink exactly\noutput:\n%s", out)
	}
	if strings.Contains(out, `pattern "../OWNERS.real"`) {
		t.Errorf("audit parsed the symlink TARGET as a CODEOWNERS pattern and reported a finding about it\noutput:\n%s", out)
	}
}

// runGit is gitRun for commands whose failure is part of the fixture — a merge
// that must conflict, a `status` read afterwards — so a non-zero exit is data
// rather than a t.Fatal.
func runGit(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return code, out.String(), errb.String()
}
