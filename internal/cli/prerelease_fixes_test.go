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
// write path's components are Lstat'ed, so an ordinary repo full of links
// syncs as before.
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

// A symlinked PARENT directory is the same dead-on-arrival write one level up:
// git tracks `.github -> real-gh` as a link blob, so `.github/CODEOWNERS` does
// not exist in the tree GitHub reads — yet Lstat'ing only the final component
// (a real file, reached through the link) let sync write through it at exit 0.
// The refusal must fire, name WHICH component is the link, and leave the
// link's target untouched.
func TestFix_SyncRefusesSymlinkedParentDir(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"real-gh/CODEOWNERS": "/src/ @org/team\n",
		"src/a.go":           "",
	})
	if err := os.Symlink("real-gh", filepath.Join(repo, ".github")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "dir link")
	before, _ := os.ReadFile(filepath.Join(repo, "real-gh/CODEOWNERS"))

	code, _, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/src/, @org/extra)")
	if code != cli.ExitRefused {
		t.Fatalf("sync through symlinked .github/: want exit 2, got %d\nstderr: %s", code, stderr)
	}
	want := filepath.ToSlash(filepath.Join(repo, ".github")) + " is a symlink"
	if !strings.Contains(stderr, want) {
		t.Errorf("the refusal must name the symlinked COMPONENT (%q):\n%s", want, stderr)
	}
	after, _ := os.ReadFile(filepath.Join(repo, "real-gh/CODEOWNERS"))
	if string(after) != string(before) {
		t.Errorf("the link's target directory was written anyway:\n%s", after)
	}
}

// The parent-directory refusal reaches `apply` too: the link can appear
// between planning and applying, exactly like the final-component case the
// existing guard pins.
func TestFix_ApplyRefusesSymlinkedParentDir(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"real-gh/CODEOWNERS": "* @org/every\n",
		"a.md":               "",
	})
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, stderr := runCLI(t, "plan", "--repo", repo, "--op", "add_owner(a.md, @org/a)", "--out", planPath); code != cli.ExitOK {
		t.Fatalf("plan: want exit 0, got %d\nstderr: %s", code, stderr)
	}
	if err := os.RemoveAll(filepath.Join(repo, ".github")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real-gh", filepath.Join(repo, ".github")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(repo, "real-gh/CODEOWNERS"))

	code, _, stderr := runCLI(t, "apply", "--plan", planPath)
	if code != cli.ExitRefused {
		t.Fatalf("apply through symlinked .github/: want exit 2, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, filepath.ToSlash(filepath.Join(repo, ".github"))+" is a symlink") {
		t.Errorf("the refusal must name the symlinked component:\n%s", stderr)
	}
	after, _ := os.ReadFile(filepath.Join(repo, "real-gh/CODEOWNERS"))
	if string(after) != string(before) {
		t.Errorf("the link's target directory was written anyway:\n%s", after)
	}
}

// lint shares the helper, so the parent-directory case refuses there too —
// offline, before any API call, like its final-component sibling above.
func TestFix_LintRefusesSymlinkedParentDir(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"real-gh/CODEOWNERS": "* @org/every\n",
		"a.md":               "",
	})
	if err := os.Symlink("real-gh", filepath.Join(repo, ".github")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "dir link")

	// --file, because discovery cannot see .github/CODEOWNERS in a tree where
	// .github is a link blob — which is the point of the refusal.
	code, _, stderr := runCLI(t, "lint", "--repo", repo, "--github-repo", "o/r", "--token", "t", "--file", ".github/CODEOWNERS")
	if code != cli.ExitRefused {
		t.Fatalf("lint through symlinked .github/: want exit 2, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "is a symlink") {
		t.Errorf("the refusal must name the symlinked component:\n%s", stderr)
	}
}

// A symlinked DIRECTORY that is not on the write path stays irrelevant, like a
// symlinked file elsewhere always has: the walk covers only the components
// between the repository root and the CODEOWNERS being written.
func TestFix_SymlinkedDirOffWritePathStaysIrrelevant(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"static/logo.txt":    "x",
		"a.md":               "",
	})
	if err := os.Symlink("static", filepath.Join(repo, "assets")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "dir link elsewhere")

	code, _, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(a.md, @org/a)")
	if code != cli.ExitOK {
		t.Errorf("sync with a symlinked dir off the write path: want exit 0, got %d\nstderr: %s", code, stderr)
	}
}

// R-25's refusal names only the ops that would have APPLIED: an op whose rule
// was already satisfied changed zero paths, so naming it sent the operator
// narrowing an op that was never behind the number.
func TestFix_CeilingRefusalNamesOnlyAppliedOps(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/src/ @org/team\n",
		"src/a.go":           "",
	})
	code, _, stderr := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/src/, @org/team)", // already satisfied → unchanged
		"--op", "add_owner(/src/, @org/extra)", // would apply → behind the count
		"--max-paths-changed", "0")
	if code != cli.ExitRefused {
		t.Fatalf("over the ceiling: want exit 2, got %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "ops[1]") {
		t.Errorf("the refusal must name the op behind the number (ops[1]):\n%s", stderr)
	}
	if strings.Contains(stderr, "ops[0]") {
		t.Errorf("the refusal names ops[0], which changed zero paths:\n%s", stderr)
	}
}

// Every sync exit-3 verdict asked for a sink discloses that no record was
// written — not only the two paths the first fix covered. A fleet aggregating
// --out records otherwise loses these repos silently, the exact hazard the
// note exists to disclose.
func TestFix_NoRecordNoteCoversEverySyncExit3(t *testing.T) {
	polPath := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(polPath, []byte(`{"version":1,"ops":["add_owner(/x/, @a)"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	op := "add_owner(/x/, @a)"
	cases := []struct {
		name string
		args []string // --out / --summary-out / --format json appended per case
	}{
		{"bad --format", []string{"sync", "--op", op, "--format", "jsn", "--out", ""}},
		{"bad --on-empty", []string{"sync", "--op", op, "--on-empty", "typo", "--out", ""}},
		{"--on-empty with --policy", []string{"sync", "--policy", polPath, "--on-empty", "error", "--format", "json"}},
		{"--file escape", []string{"sync", "--op", op, "--file", "../esc/CODEOWNERS", "--out", ""}},
		{"--file in .git", []string{"sync", "--op", op, "--file", ".git/CODEOWNERS", "--summary-out", ""}},
		{"bad --branch", []string{"sync", "--op", op, "--branch", "-bad", "--out", ""}},
		{"stray positional", []string{"sync", "--op", op, "--out", "", "stray-arg"}},
		{"no ops", []string{"sync", "--out", ""}},
		{"negative ceiling", []string{"sync", "--op", op, "--max-paths-changed", "-5", "--out", ""}},
		{"--create with --policy", []string{"sync", "--policy", polPath, "--create", "--out", ""}},
		{"ceiling with --policy", []string{"sync", "--policy", polPath, "--max-paths-changed", "5", "--out", ""}},
		{"invalid scope", []string{"sync", "--op", "add_owner(!x, @a)", "--out", ""}},
		{"static conflict", []string{"sync", "--op", "set_owners(/x/, @a)", "--op", "set_owners(/x/, @b)", "--out", ""}},
	}
	for _, tc := range cases {
		outPath := filepath.Join(t.TempDir(), "rec.json")
		args := make([]string, len(tc.args))
		copy(args, tc.args)
		for i := range args {
			if args[i] == "" && i > 0 && (args[i-1] == "--out" || args[i-1] == "--summary-out") {
				args[i] = outPath
			}
		}
		code, _, stderr := runCLI(t, args...)
		if code != cli.ExitInvalid {
			t.Errorf("%s: want exit 3, got %d\nstderr: %s", tc.name, code, stderr)
			continue
		}
		if !strings.Contains(stderr, "no record was written") {
			t.Errorf("%s: exit 3 with a sink asked for must carry the no-record note\nstderr: %s", tc.name, stderr)
		}
		if _, err := os.Stat(outPath); err == nil {
			t.Errorf("%s: a record file was written for an exit-3 verdict", tc.name)
		}
	}

	// The gate is unchanged: with no sink asked for there is no aggregation to
	// protect, so the note stays quiet.
	code, _, stderr := runCLI(t, "sync", "--op", op, "--format", "jsn")
	if code != cli.ExitInvalid {
		t.Fatalf("bad --format without sinks: want exit 3, got %d", code)
	}
	if strings.Contains(stderr, "no record was written") {
		t.Errorf("the note fired with no sink asked for:\n%s", stderr)
	}
}

// The stale-comment warning needs a LEADING token boundary too: a rename of
// @old-team must stay quiet about `someone@old-team`, where the match is the
// tail of an email, while a real mention — space-separated or glued to the
// comment glyph — still warns.
func TestFix_StaleCommentWarningLeadingBoundary(t *testing.T) {
	renameOp := "rename_owner(@old-team, @new-team)"
	cases := []struct {
		name     string
		content  string
		wantWarn bool
	}{
		{"email tail", "# email someone@old-team about escalations\n/src/ @old-team\n", false},
		{"real mention", "# ping @old-team about escalations\n/src/ @old-team\n", true},
		{"glued to comment glyph", "#@old-team\n/src/ @old-team\n", true},
	}
	for _, tc := range cases {
		repo := initRepo(t, map[string]string{
			".github/CODEOWNERS": tc.content,
			"src/a.go":           "",
		})
		code, _, stderr := runCLI(t, "sync", "--repo", repo, "--op", renameOp)
		if code != cli.ExitOK {
			t.Errorf("%s: rename should apply at exit 0, got %d\nstderr: %s", tc.name, code, stderr)
			continue
		}
		gotWarn := strings.Contains(stderr, `still names "@old-team"`)
		if gotWarn != tc.wantWarn {
			t.Errorf("%s: warning fired=%v, want %v\nstderr: %s", tc.name, gotWarn, tc.wantWarn, stderr)
		}
	}
}

// SPEC R-23/R-24 (S-8): `--create` at a location BELOW the one this repo is
// governed from still writes, and still says the rules govern nothing.
//
// The direction that is refused (superseding the governing file) and the one
// that is allowed differ only in which way the precedence runs, so a guard
// written as "--file plus an existing CODEOWNERS" would take this case with
// it. Nothing is lost by writing here — .github/CODEOWNERS keeps governing —
// and the R-24 warning is what tells the operator that.
func TestFix_CreateBelowTheGoverningFileStillWrites(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/every\n",
		"services/api/main.go": "",
	})
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--create",
		"--file", "docs/CODEOWNERS", "--op", "add_owner(/services/api/, @org/platform)")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d — docs/ ranks below .github/, so this write supersedes nothing\nstderr:\n%s", code, errOut)
	}
	if got := syncReadFile(t, filepath.Join(repo, "docs", "CODEOWNERS")); !strings.Contains(got, "@org/platform") {
		t.Errorf("docs/CODEOWNERS = %q", got)
	}
	if !strings.Contains(errOut, "govern nothing") || !strings.Contains(errOut, ".github/CODEOWNERS") {
		t.Errorf("R-24's warning is missing: the file just written is the one GitHub never loads\nstderr:\n%s", errOut)
	}
}

// SPEC R-23 (S-8): the superseding-create refusal is about PRECEDENCE, not
// about .github/ — `--file CODEOWNERS --create` over a docs/-governed repo is
// refused on the same rule.
//
// Root outranks docs/ exactly as .github/ does, so a guard narrowed to the
// one location the finding happened to use would leave this spelling writing
// a file that silently takes the whole repository's ownership.
func TestFix_CreateAtRootOverDocsIsRefused(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"docs/CODEOWNERS":      "* @org/everyone\n/services/api/ @org/api-team\n",
		"services/api/main.go": "",
	})
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--create",
		"--file", "CODEOWNERS", "--op", "add_owner(/services/api/, @org/platform)")
	if code != cli.ExitRefused {
		t.Fatalf("want exit 2, got %d — a root CODEOWNERS would demote docs/CODEOWNERS to a file GitHub never reads\nstderr:\n%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(repo, "CODEOWNERS")); err == nil {
		t.Error("CODEOWNERS was written: docs/CODEOWNERS now governs nothing")
	}
	fixWantNamesBothFiles(t, errOut, "CODEOWNERS", "docs/CODEOWNERS")
}

// SPEC R-23/R-34d (S-8): the policy spelling is refused identically —
// `"create": true` with a pinned `--file` is the fleet-scale shape of this
// hazard, set once and run against a hundred clones.
//
// The record is the whole assertion. A refused repo carries no
// codeowners_path, so `.error` is all a `needs-human` pile has to triage on,
// and `created: false` is what keeps a commit step from staging a file that
// was never written.
func TestFix_PolicyCreateWithAPinnedFileIsRefused(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":["add_owner(/services/api/, @org/platform)"]}`)
	repo := initRepo(t, map[string]string{
		"docs/CODEOWNERS":      "* @org/everyone\n/services/api/ @org/api-team\n",
		"services/api/main.go": "",
	})
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--file", ".github/CODEOWNERS", "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("want exit 2, got %d — exit 3 would halt a fleet over one repo's layout\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(repo, ".github")); err == nil {
		t.Error("a .github/ directory was created for a file that was never written")
	}
	if got := syncReadFile(t, filepath.Join(repo, "docs", "CODEOWNERS")); got != "* @org/everyone\n/services/api/ @org/api-team\n" {
		t.Errorf("the governing file changed:\n%s", got)
	}

	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusRefused {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusRefused)
	}
	if rec.Created {
		t.Error("created = true on a run that wrote nothing")
	}
	if v, present := pfRawRecord(t, out)["codeowners_path"]; present {
		t.Errorf("codeowners_path is present on a refused run: %v", v)
	}
	fixWantNamesBothFiles(t, rec.Error, ".github/CODEOWNERS", "docs/CODEOWNERS")
}

// fixWantNamesBothFiles pins the two paths a superseding-create refusal is
// about: the one --file asked for and the one that governs today. Either name
// alone leaves the operator unable to tell which file to keep.
//
// The write target is matched as `creating <path> `, not as the bare path: at
// the root the bare spelling "CODEOWNERS" is a substring of the governing
// "docs/CODEOWNERS", so a refusal that named only the governing file would
// satisfy both halves and the assertion would prove nothing (the vacuous pass
// CONTRIBUTING warns about — confirmed by mutating Error() to drop the write
// target, which this test then still accepted).
func fixWantNamesBothFiles(t *testing.T, msg, wrote, governing string) {
	t.Helper()
	for _, want := range []string{"creating " + wrote + " ", governing, "S-8"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must name %q; got:\n%s", want, msg)
		}
	}
}

// fixMountSubmodule mounts sub inside parent at `at` and commits the gitlink.
// A sandbox that forbids the file:// transport cannot build the fixture, which
// is an environment limit rather than a result, so it skips.
func fixMountSubmodule(t *testing.T, parent, sub, at string) {
	t.Helper()
	cmd := exec.Command("git", "-C", parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, at)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot mount a local submodule here: %v\n%s", err, out)
	}
	gitRun(t, parent, "commit", "-qm", "mount submodule at "+at)
}

// `--create` reaches the submodule by a different route than discovery does:
// the mount holds no CODEOWNERS at all, so D5 finds nothing on disk and the
// default location is chosen from scratch. Creating there puts a brand new
// file inside the submodule's checkout — a path the parent cannot stage —
// and reports created:true at exit 0. The refusal must fire before the file
// exists, since --create never overwrites and so would never fix it up.
func TestFix_CreateIntoASubmoduleIsRefused(t *testing.T) {
	sub := initRepo(t, map[string]string{"README.md": "shared org files\n"})
	parent := initRepo(t, map[string]string{"services/api/main.go": "m\n"})
	fixMountSubmodule(t, parent, sub, ".github")

	code, stdout, stderr := runCLI(t, "sync", "--repo", parent, "--create",
		"--op", "add_owner(/services/api/, @org/api)", "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("--create into a submodule: want exit 2, got %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(parent, ".github", "CODEOWNERS")); err == nil {
		t.Error("a CODEOWNERS was created inside the submodule's checkout")
	}
	fixWantSubmoduleRefusal(t, stdout+stderr, ".github")
}

// `--file` names the path directly, so discovery is bypassed entirely — the
// same dead-on-arrival write with the operator's spelling on it.
func TestFix_FileInsideASubmoduleIsRefused(t *testing.T) {
	sub := initRepo(t, map[string]string{"CODEOWNERS": "* @sub/owners\n"})
	parent := initRepo(t, map[string]string{"services/api/main.go": "m\n"})
	fixMountSubmodule(t, parent, sub, "docs")
	before := syncReadFile(t, filepath.Join(parent, "docs", "CODEOWNERS"))

	// `./docs/CODEOWNERS`, not `docs/CODEOWNERS`: the guard compares the write
	// target against paths git reported, and git reports clean ones, so an
	// unnormalized comparison matches nothing and this exact run writes into
	// the submodule again at exit 0 (confirmed by mutating relClean out of the
	// guard, which the whole suite otherwise cannot see).
	code, stdout, stderr := runCLI(t, "sync", "--repo", parent, "--file", "./docs/CODEOWNERS",
		"--op", "add_owner(/services/api/, @org/api)", "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("--file inside a submodule: want exit 2, got %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if got := syncReadFile(t, filepath.Join(parent, "docs", "CODEOWNERS")); got != before {
		t.Errorf("the submodule's own CODEOWNERS was rewritten:\n%s", got)
	}
	fixWantSubmoduleRefusal(t, stdout+stderr, "docs")
}

// A submodule that is nowhere near a CODEOWNERS location is an ordinary part
// of the tree: the gitlink guard looks only at the components of the write
// path, exactly like its symlink sibling, so a vendored dependency must not
// cost this repo its run.
func TestFix_SubmoduleOffTheWritePathStaysIrrelevant(t *testing.T) {
	sub := initRepo(t, map[string]string{"lib.go": "package lib\n"})
	parent := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "m\n",
	})
	fixMountSubmodule(t, parent, sub, "vendor/lib")

	code, stdout, stderr := runCLI(t, "sync", "--repo", parent,
		"--op", "add_owner(/services/api/, @org/api)")
	if code != cli.ExitOK {
		t.Fatalf("sync with a submodule at vendor/lib: want exit 0, got %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if got := syncReadFile(t, filepath.Join(parent, ".github", "CODEOWNERS")); !strings.Contains(got, "@org/api") {
		t.Errorf("the parent's own CODEOWNERS was not amended:\n%s", got)
	}
}

// The ordinary shape the guard must leave alone: a real `.github` DIRECTORY
// holding a tracked CODEOWNERS. `ls-tree -r` lists no directories, so nothing
// in this tree is an ancestor path of the write target and there is nothing to
// refuse.
//
// The stray `.github/CODEOWNER` (no S) is the near miss that pins the
// component boundary: it is a tracked path and a string prefix of the file
// being written, so a guard comparing prefixes without requiring a `/` after
// them refuses this repo forever with "…: .github/CODEOWNER is a submodule",
// and no other test in the suite notices.
func TestFix_PlainCodeownersDirectoryStillSyncs(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		".github/CODEOWNER":    "# a typo somebody committed\n",
		"services/api/main.go": "m\n",
	})
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/services/api/, @org/api)")
	if code != cli.ExitOK {
		t.Fatalf("ordinary .github/CODEOWNERS: want exit 0, got %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if got := syncReadFile(t, filepath.Join(repo, ".github", "CODEOWNERS")); !strings.Contains(got, "/services/api/ @org/everyone @org/api") {
		t.Errorf("the amendment did not land:\n%s", got)
	}
}

// fixWantSubmoduleRefusal pins what a gitlink refusal has to say: WHICH path
// is the mount, that it is a submodule rather than some other awkward object,
// and the consequence that makes it a refusal instead of a warning.
//
// The mount is matched as `records <mount> at `, not bare: ".github" is a
// substring of the write path the same message names, so the bare spelling is
// satisfied by a message that never identifies the mount at all — and the
// sibling assertion below proves the boundary, since "records .github at "
// does not match a message about ".github/CODEOWNER".
func fixWantSubmoduleRefusal(t *testing.T, out, mount string) {
	t.Helper()
	for _, want := range []string{"records " + mount + " at ", "as a submodule", "cannot stage", "nothing was written"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal must contain %q; got:\n%s", want, out)
		}
	}
}

// `plan` is where the submodule write has to die, because `plan` is what makes
// the artifact: `--file` bypasses discovery, so the verb that refuses nothing
// hands a human a plan whose codeowners_path is inside another repository's
// checkout, and `apply` then writes it. The plan file must not exist
// afterwards — a refused run produces no artifact to approve.
func TestFix_PlanRefusesACodeownersInsideASubmodule(t *testing.T) {
	sub := initRepo(t, map[string]string{"CODEOWNERS": "* @sub/owners\n"})
	parent := initRepo(t, map[string]string{"services/api/main.go": "m\n"})
	fixMountSubmodule(t, parent, sub, ".github")
	planPath := filepath.Join(t.TempDir(), "plan.json")

	code, stdout, stderr := runCLI(t, "plan", "--repo", parent, "--file", ".github/CODEOWNERS",
		"--op", "add_owner(/services/api/, @org/api)", "--out", planPath)
	if code != cli.ExitRefused {
		t.Fatalf("plan into a submodule: want exit 2, got %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if _, err := os.Stat(planPath); err == nil {
		t.Error("a plan was written for a write that can never be applied here")
	}
	if strings.Contains(stdout+stderr, "@sub/owners") {
		t.Errorf("the plan quoted the submodule's owners — a fact about a different repository\noutput:\n%s%s", stdout, stderr)
	}
	fixWantSubmoduleRefusal(t, stdout+stderr, ".github")
}

// lint writes the same file the other verbs do, so it refuses the same shape —
// and offline, before the token is used, like its symlink sibling above.
func TestFix_LintRefusesACodeownersInsideASubmodule(t *testing.T) {
	sub := initRepo(t, map[string]string{"CODEOWNERS": "* @sub/owners\n"})
	parent := initRepo(t, map[string]string{"services/api/main.go": "m\n"})
	fixMountSubmodule(t, parent, sub, ".github")
	before := syncReadFile(t, filepath.Join(parent, ".github", "CODEOWNERS"))

	// --file, because discovery finds nothing in a tree whose .github is a
	// gitlink — which is what makes the working-tree file reachable at all.
	code, stdout, stderr := runCLI(t, "lint", "--repo", parent, "--github-repo", "o/r", "--token", "t",
		"--file", ".github/CODEOWNERS")
	if code != cli.ExitRefused {
		t.Fatalf("lint into a submodule: want exit 2, got %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if got := syncReadFile(t, filepath.Join(parent, ".github", "CODEOWNERS")); got != before {
		t.Errorf("the submodule's own CODEOWNERS was repaired in place:\n%s", got)
	}
	fixWantSubmoduleRefusal(t, stdout+stderr, ".github")
}

// The refusal has to diagnose what git actually records, not what the common
// case makes likely. A tracked symlink at `.github` that has been DELETED from
// the working tree reaches the same guard — the symlink refusal is an Lstat, so
// it finds nothing to refuse — and the tree evidence alone (".github is a path,
// and paths are not directories") cannot tell a gitlink from a link blob.
//
// Refusing is right either way: `.github/CODEOWNERS` does not exist at the ref,
// so the run would prove its invariants against a tree that has no CODEOWNERS.
// Calling it a submodule is not: in THIS repo `git add .github/CODEOWNERS`
// succeeds (exit 0, staged as `D .github` + `A .github/CODEOWNERS`), so a
// message claiming it "fails with is in submodule" sends the operator looking
// for a submodule that does not exist. The message is the only evidence a
// refused fleet row carries.
func TestFix_TrackedLinkAtACodeownersLocationIsDiagnosedAsALink(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"real-gh/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "m\n",
	})
	if err := os.Symlink("real-gh", filepath.Join(repo, ".github")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "link .github at a real directory")
	if err := os.Remove(filepath.Join(repo, ".github")); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--create",
		"--op", "add_owner(/services/api/, @org/api)")
	out := stdout + stderr
	if code != cli.ExitRefused {
		t.Fatalf("--create over a tracked link at .github: want exit 2, got %d\noutput:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".github", "CODEOWNERS")); err == nil {
		t.Error("a CODEOWNERS was created at a path the tracked tree records as a link")
	}
	if strings.Contains(out, "submodule") {
		t.Errorf("the refusal calls a link blob a submodule; `git add .github/CODEOWNERS` succeeds in this repo,\n"+
			"so the operator is sent looking for a submodule that does not exist\noutput:\n%s", out)
	}
	for _, want := range []string{"records .github at ", "as a symlink", "nothing was written"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal must contain %q; got:\n%s", want, out)
		}
	}
}

// The submodule finding, still live through the one verb that had no guard:
// `apply` writes the WORKING TREE but validates only the tree at the plan's
// ref, and never checks the clone is standing on it. With `.github` a real
// directory on `main` and a submodule mount on `subm`, a plan made
// `--branch main` from a clone on `subm` is internally consistent — main's
// tree holds `.github/CODEOWNERS`, so nothing is a non-tree ancestor there,
// and the tree digest still matches at apply time because main never changed
// — while the bytes it planned against, and the file it writes, are the
// SUBMODULE's.
//
// sync and lint both refuse this shape (S-7, checkBranchIsWritable) and sync's
// refusal points the operator at `plan`, so the tool routed people into the
// unguarded path. The defect is wider than submodules: any ref whose tree
// differs from the checkout lands a change justified by a tree nobody wrote to.
func TestFix_ApplyRefusesAPlanFromARefThisCloneIsNotOn(t *testing.T) {
	sub := initRepo(t, map[string]string{"CODEOWNERS": "* @sub/owners\n"})
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "m\n",
	})
	gitRun(t, repo, "switch", "-qc", "subm")
	gitRun(t, repo, "rm", "-r", "-q", ".github")
	gitRun(t, repo, "commit", "-qm", "drop the directory")
	fixMountSubmodule(t, repo, sub, ".github")
	before := syncReadFile(t, filepath.Join(repo, ".github", "CODEOWNERS"))

	planPath := filepath.Join(t.TempDir(), "plan.json")
	// `plan` against another ref is its documented job (checkBranchIsWritable
	// sends sync here), so this half must keep working.
	if code, out, errOut := runCLI(t, "plan", "--repo", repo, "--branch", "main", "--file", ".github/CODEOWNERS",
		"--op", "add_owner(/services/api/, @org/api)", "--out", planPath); code != cli.ExitOK {
		t.Fatalf("plan for another ref: want exit 0, got %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	code, stdout, stderr := runCLI(t, "apply", "--repo", repo, "--plan", planPath)
	out := stdout + stderr
	if code != cli.ExitRefused {
		t.Fatalf("apply of a plan for a ref this clone is not on: want exit 2, got %d\noutput:\n%s", code, out)
	}
	if got := syncReadFile(t, filepath.Join(repo, ".github", "CODEOWNERS")); got != before {
		t.Errorf("the submodule's own CODEOWNERS was written by the parent repository:\n%s", got)
	}
	// "computed against main," and not a bare "main": the fixture's own
	// services/api/main.go puts that substring in unrelated output, so the
	// bare form would pass on a refusal that never named the ref at all.
	if !strings.Contains(out, "not what this clone has checked out") || !strings.Contains(out, "computed against main,") {
		t.Errorf("the refusal must name the ref and say the clone is not on it:\n%s", out)
	}
	// apply has neither flag, and sending an operator to a flag that does not
	// exist is the failure the mode-specific wording was fixed for.
	for _, absent := range []string{"--branch", "--dry-run"} {
		if strings.Contains(out, absent) {
			t.Errorf("the refusal advises %s, which `apply` does not have:\n%s", absent, out)
		}
	}
}

// The fallback noun, which no other test reaches: a tracked REGULAR FILE at
// `.github` is neither a gitlink nor a link, so the refusal must not guess at
// either. Naming it "a submodule" here would be the same falsifiable diagnosis
// the link case was fixed for, and the whole suite passes with that mutation.
func TestFix_TrackedFileAtACodeownersLocationIsNamedNeutrally(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github":              "not a directory at all\n",
		"services/api/main.go": "m\n",
	})
	planPath := filepath.Join(t.TempDir(), "plan.json")

	code, stdout, stderr := runCLI(t, "plan", "--repo", repo, "--file", ".github/CODEOWNERS",
		"--op", "add_owner(/services/api/, @org/api)", "--out", planPath)
	out := stdout + stderr
	if code != cli.ExitRefused {
		t.Fatalf("plan through a tracked regular file: want exit 2, got %d\noutput:\n%s", code, out)
	}
	if !strings.Contains(out, "records .github at ") || !strings.Contains(out, "as a file object") {
		t.Errorf("the refusal must name the path and the object git records, without guessing:\n%s", out)
	}
	for _, absent := range []string{"submodule", "symlink"} {
		if strings.Contains(out, absent) {
			t.Errorf("the refusal claims %q for a plain tracked file:\n%s", absent, out)
		}
	}
}

// SPEC S-7: the branch guard compares RESOLVED COMMITS, not ref names, so a
// second name for the commit the clone is on still applies.
//
// checkBranchIsWritable's contract has always been the commit, and `apply`
// is now a third verb relying on it — but nothing pinned it: adding a
// name-equality requirement passed the entire suite while breaking every
// alias and tag. An `apply` that refused a plan naming `release` while the
// clone sat on `main` at the same commit would fail a rollout over a
// difference no tree reflects.
func TestFix_ApplyAcceptsAnotherNameForTheSameCommit(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "m\n",
	})
	gitRun(t, repo, "branch", "release")

	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, errOut := runCLI(t, "plan", "--repo", repo, "--branch", "release",
		"--op", "add_owner(/services/api/, @org/api)", "--out", planPath); code != cli.ExitOK {
		t.Fatalf("plan --branch release: want exit 0, got %d\n%s", code, errOut)
	}
	// The clone is on main; release names the same commit.
	if code, stdout, stderr := runCLI(t, "apply", "--repo", repo, "--plan", planPath); code != cli.ExitOK {
		t.Fatalf("apply of a plan naming another ref at the SAME commit: want exit 0, got %d\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}
	if got := syncReadFile(t, filepath.Join(repo, ".github", "CODEOWNERS")); !strings.Contains(got, "@org/api") {
		t.Errorf("apply exited 0 without writing the change:\n%s", got)
	}
}

// SPEC S-7: a plan whose ref no longer resolves is refused in words, not in
// git plumbing.
//
// The branch was deleted or renamed since the plan was written — a fact about
// THIS clone, so exit 2 like the mismatch it sits beside. Before this, the
// error was `git rev-parse --verify --end-of-options release^{commit}: exit
// status 128: fatal: Needed a single revision`, which names a command nobody
// ran and a flag this tool passes on the operator's behalf.
func TestFix_ApplyRefusesAPlanWhoseRefIsGoneWithoutGitPlumbing(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "m\n",
	})
	gitRun(t, repo, "switch", "-qc", "release")
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, errOut := runCLI(t, "plan", "--repo", repo, "--branch", "release",
		"--op", "add_owner(/services/api/, @org/api)", "--out", planPath); code != cli.ExitOK {
		t.Fatalf("plan --branch release: want exit 0, got %d\n%s", code, errOut)
	}
	gitRun(t, repo, "switch", "-q", "main")
	gitRun(t, repo, "branch", "-qD", "release")

	code, stdout, stderr := runCLI(t, "apply", "--repo", repo, "--plan", planPath)
	out := stdout + stderr
	if code != cli.ExitRefused {
		t.Fatalf("apply of a plan whose ref is gone: want exit 2, got %d\noutput:\n%s", code, out)
	}
	if !strings.Contains(out, "release") || !strings.Contains(out, "does not resolve in this clone") {
		t.Errorf("the refusal must name the ref and say it does not resolve here:\n%s", out)
	}
	// --branch and --dry-run are flags `apply` does not have: its ref came from
	// the plan file. Naming one sends the reader hunting for a flag that does
	// not exist, which is what refPhrase exists to prevent — and collapsing it
	// to the sync wording otherwise survives the whole suite.
	for _, plumbing := range []string{"rev-parse", "--end-of-options", "^{commit}", "exit status", "--branch", "--dry-run"} {
		if strings.Contains(out, plumbing) {
			t.Errorf("the refusal leaks %q, which `apply` never invoked and does not have:\n%s", plumbing, out)
		}
	}
}

// fixConflictedMerge leaves repo standing in a conflicted merge, with path
// unmerged: the side branch and main write different content there, and the
// merge of the two fails. Returns the conflicted bytes on disk.
func fixConflictedMerge(t *testing.T, repo, path, theirs, ours string) string {
	t.Helper()
	full := filepath.Join(repo, filepath.FromSlash(path))
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "switch", "-qc", "sidebranch")
	write(theirs)
	gitRun(t, repo, "commit", "-aqm", "theirs")
	gitRun(t, repo, "switch", "-q", "main")
	write(ours)
	gitRun(t, repo, "commit", "-aqm", "ours")
	// The merge is EXPECTED to fail, so it does not go through gitRun.
	if code, _, _ := runGit(t, repo, "merge", "sidebranch"); code == 0 {
		t.Fatal("fixture: the merge was supposed to conflict")
	}
	if _, status, _ := runGit(t, repo, "status", "--short"); !strings.Contains(status, "U "+path) {
		t.Fatalf("fixture: expected %s unmerged, git says:\n%s", path, status)
	}
	return syncReadFile(t, full)
}

// An unmerged CODEOWNERS is refused by every verb that reads its bytes to
// decide an edit, not only by the `sync` the finding was reported against.
//
// The bytes on disk are both sides of a conflict, so the "before" ownership is
// a state no commit has ever had: `=======` parses as a zero-owner rule (S-9)
// and both sides' rules stay live. `plan` is in the list because the artifact
// it emits is what a human approves — a plan computed from conflict-mangled
// bytes should not exist to be approved. Exit 2: an unmerged index is a fact
// about THIS clone, so a fleet loop records it and steps to the next repo.
func TestFix_UnmergedCodeownersIsRefusedByEveryVerbThatReadsIt(t *testing.T) {
	for _, tc := range []struct {
		verb string
		args []string
	}{
		{"sync", []string{"sync", "--op", "add_owner(/services/api/, @org/api)"}},
		{"plan", []string{"plan", "--op", "add_owner(/services/api/, @org/api)"}},
		{"lint", []string{"lint", "--github-repo", "o/r", "--token", "t"}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			repo := initRepo(t, map[string]string{
				".github/CODEOWNERS":   "* @org/everyone\n",
				"services/api/main.go": "m\n",
				"docs/x.md":            "d\n",
			})
			conflicted := fixConflictedMerge(t, repo, ".github/CODEOWNERS",
				"* @org/everyone\n/docs/ @org/docs-a\n", "* @org/everyone\n/docs/ @org/docs-b\n")

			code, stdout, stderr := runCLI(t, append(tc.args, "--repo", repo)...)
			out := stdout + stderr
			if code != cli.ExitRefused {
				t.Fatalf("%s on an unmerged CODEOWNERS: want exit 2, got %d\noutput:\n%s", tc.verb, code, out)
			}
			if !strings.Contains(out, "unmerged") || !strings.Contains(out, ".github/CODEOWNERS") {
				t.Errorf("the refusal must name the file and say it is unmerged\noutput:\n%s", out)
			}
			if got := syncReadFile(t, filepath.Join(repo, ".github", "CODEOWNERS")); got != conflicted {
				t.Errorf("the conflicted file was rewritten anyway:\n%s", got)
			}
		})
	}
}

// `apply` carries the guard too, because its integrity checks cannot reach
// this: `git checkout --ours .github/CODEOWNERS` leaves the file UNMERGED with
// exactly the bytes the plan was computed from, so sha256_before matches and
// the tracked tree at HEAD never moved. Pre-guard, apply wrote at exit 0 and
// the operator's `git add` would then have resolved somebody's merge with
// content the tool synthesized against one side of it.
func TestFix_ApplyRefusesAnUnmergedCodeowners(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "m\n",
		"docs/x.md":            "d\n",
	})
	co := filepath.Join(repo, ".github", "CODEOWNERS")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(co, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "switch", "-qc", "sidebranch")
	write("* @org/everyone\n/docs/ @org/docs-a\n")
	gitRun(t, repo, "commit", "-aqm", "theirs")
	gitRun(t, repo, "switch", "-q", "main")
	write("* @org/everyone\n/docs/ @org/docs-b\n")
	gitRun(t, repo, "commit", "-aqm", "ours")

	// Planned while the checkout is clean — the reviewed artifact.
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, errOut := runCLI(t, "plan", "--repo", repo,
		"--op", "add_owner(/services/api/, @org/api)", "--out", planPath); code != cli.ExitOK {
		t.Fatalf("plan on the clean checkout: want exit 0, got %d\n%s", code, errOut)
	}

	// The merge is EXPECTED to fail, so it does not go through gitRun.
	if code, _, _ := runGit(t, repo, "merge", "sidebranch"); code == 0 {
		t.Fatal("fixture: the merge was supposed to conflict")
	}
	// Our side's bytes, restored: still unmerged, and byte-identical to what
	// the plan was computed from, so sha256_before cannot notice.
	gitRun(t, repo, "checkout", "--ours", "--", ".github/CODEOWNERS")
	before := syncReadFile(t, co)
	if strings.Contains(before, "<<<<<<<") {
		t.Fatalf("fixture: --ours should have removed the markers:\n%s", before)
	}
	if _, status, _ := runGit(t, repo, "status", "--short"); !strings.Contains(status, "U .github/CODEOWNERS") {
		t.Fatalf("fixture: the file must still be unmerged, git says:\n%s", status)
	}

	code, stdout, stderr := runCLI(t, "apply", "--repo", repo, "--plan", planPath)
	out := stdout + stderr
	if code != cli.ExitRefused {
		t.Fatalf("apply onto an unmerged CODEOWNERS: want exit 2, got %d\noutput:\n%s", code, out)
	}
	if !strings.Contains(out, "unmerged") {
		t.Errorf("the refusal must say the file is unmerged\noutput:\n%s", out)
	}
	if got := syncReadFile(t, co); got != before {
		t.Errorf("apply wrote into a file git still reports as unmerged:\n%s", got)
	}
}

// The neighbour the guard must not take with it: an ordinary DIRTY working
// tree. Uncommitted edits to CODEOWNERS are the normal state of a repo the
// tool has just written to — a second `sync` in the same fleet pass sees them
// — and nothing about them is ambiguous, so the run proceeds.
func TestFix_DirtyButMergedWorkingTreeStillSyncs(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "m\n",
	})
	co := filepath.Join(repo, ".github", "CODEOWNERS")
	if err := os.WriteFile(co, []byte("* @org/everyone\n/docs/ @org/docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/api)")
	if code != cli.ExitOK {
		t.Fatalf("sync on a dirty but merged working tree: want exit 0, got %d\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}
	got := syncReadFile(t, co)
	if !strings.Contains(got, "@org/api") || !strings.Contains(got, "/docs/ @org/docs") {
		t.Errorf("the uncommitted edit or the new rule is missing:\n%s", got)
	}
}

// A conflict in some OTHER file must not block a CODEOWNERS edit. The bytes
// the invariants are proven against are this file's, and the tree they are
// resolved against is the ref's — an unmerged main.go changes neither. Blocking
// it would refuse exactly when someone is reconciling a merge, which is when
// ownership most often needs a line.
func TestFix_ConflictInAnotherFileDoesNotBlockACodeownersEdit(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "m\n",
	})
	fixConflictedMerge(t, repo, "services/api/main.go", "side\n", "main\n")

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/api)")
	if code != cli.ExitOK {
		t.Fatalf("sync with an unrelated file unmerged: want exit 0, got %d\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}
	if got := syncReadFile(t, filepath.Join(repo, ".github", "CODEOWNERS")); !strings.Contains(got, "@org/api") {
		t.Errorf("sync exited 0 without writing the change:\n%s", got)
	}
}

// Mid-rebase is not by itself a refusal: an interrupted rebase with nothing
// unmerged leaves CODEOWNERS exactly as some commit wrote it, which is a state
// the invariants can be proven against. The guard asks git what the FILE is,
// not what the repository is in the middle of.
func TestFix_MidRebaseWithoutAConflictStillSyncs(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "m\n",
	})
	gitRun(t, repo, "commit", "-qm", "empty", "--allow-empty")
	// `--exec false` stops the rebase after the first pick with a clean tree.
	if code, _, _ := runGit(t, repo, "rebase", "--exec", "false", "HEAD~1"); code == 0 {
		t.Skip("rebase --exec false did not stop the rebase here")
	}
	if _, status, _ := runGit(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("fixture: the working tree should be clean mid-rebase, git says:\n%s", status)
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/api)")
	if code != cli.ExitOK {
		t.Fatalf("sync mid-rebase with nothing unmerged: want exit 0, got %d\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}
	if got := syncReadFile(t, filepath.Join(repo, ".github", "CODEOWNERS")); !strings.Contains(got, "@org/api") {
		t.Errorf("sync exited 0 without writing the change:\n%s", got)
	}
}
