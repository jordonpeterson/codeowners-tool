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

	code, stdout, stderr := runCLI(t, "sync", "--repo", parent, "--file", "docs/CODEOWNERS",
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
// holding a tracked CODEOWNERS. `ls-tree -r` lists no directories, so `.github`
// is not itself a tracked path here and there is no gitlink to find — the
// distinction the whole guard rests on.
func TestFix_PlainCodeownersDirectoryStillSyncs(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
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

// fixWantSubmoduleRefusal pins what a gitlink refusal has to say: the mount
// point, the word an operator would search for, and the reason it is not a
// syntax or ownership problem. The mount point is matched with its ` is a
// submodule` suffix rather than bare, because ".github" is a substring of the
// write path the same message names — a refusal that mentioned only the file
// would otherwise satisfy the assertion and prove nothing.
func fixWantSubmoduleRefusal(t *testing.T, out, mount string) {
	t.Helper()
	for _, want := range []string{mount + " is a submodule", "cannot stage", "nothing was written"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal must contain %q; got:\n%s", want, out)
		}
	}
}
