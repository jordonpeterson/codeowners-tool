package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// hardeningGit runs a git command inside dir, for the cases that need a
// repository shaped differently from initRepo's single-commit default.
func hardeningGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Review finding (F1): a CODEOWNERS write that LANDED was reported as a failed
// repo, with no record emitted at all.
//
// --out was written before stdout and the first sink error became the exit
// code, so `--out` pointing into a directory that does not exist turned a
// converged repo into exit 2 with an empty stdout. The fleet script's
// `>> results.jsonl` collected nothing for that repo — so the row an operator
// would use to see the change is missing precisely where a change happened —
// and the `2)` arm filed an already-correct repo under `needs-human`. At 100
// repos with a mistyped `--out` directory that is 100 real edits reported as
// 100 failures, with no record of any of them.
//
// The record on stdout is the durable trace and goes first, unconditionally; a
// sink that cannot be written is a warning on stderr and nothing more.
func TestHardening_SinkFailureNeverReportsAConvergedRepoAsFailed(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":           "# owners\n/services/api/ @org/a\n",
		"services/api/main.go": "package main\n",
	})
	bad := filepath.Join(t.TempDir(), "no-such-dir", "rec.json")

	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/services/api/, @org/c)", "--out", bad, "--format", "json")

	if code != cli.ExitOK {
		t.Errorf("exit %d, want 0: the CODEOWNERS write succeeded — an unwritable --out path is not a fact about this repository\nstderr: %s", code, errOut)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("stdout is empty: the record is the fleet's only durable trace of this repo, and it must survive a failed file sink\nstderr: %s", errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusApplied {
		t.Errorf("status = %q, want %q — the file on disk was changed", rec.Status, cli.StatusApplied)
	}
	if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); !strings.Contains(got, "@org/c") {
		t.Errorf("CODEOWNERS = %q, want the new owner: this test is only meaningful when the write landed", got)
	}
	if !strings.Contains(errOut, "--out") {
		t.Errorf("the failed sink must still be reported on stderr, or it is silently lost:\n%s", errOut)
	}
}

// Review finding (F2, security): `--file` was not contained by `--repo`, so
// `sync --create` wrote a CODEOWNERS OUTSIDE the repository.
//
// `--file ../ESCAPED/CODEOWNERS --create` exited 0 having created the
// directories and the file next to the clone; an absolute `--file /tmp/x/ABS`
// was not rejected but reinterpreted as repo-relative, building a garbage tree
// inside the clone and reporting success. Both hurt the operator running the
// fleet loop over 100 checkouts: one typo writes 100 files into whatever sits
// beside them (or 100 junk trees inside them), each one reported applied. The
// verdict depends on nothing but the argument, so it is exit 3 — the class that
// halts at repo 0 rather than working through the fleet.
func TestHardening_FileArgumentIsContainedByTheRepo(t *testing.T) {
	repo := initRepo(t, map[string]string{"services/api/main.go": "package main\n"})
	outside := filepath.Join(repo, "..", "ESCAPED")

	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/services/api/, @org/b)", "--file", "../ESCAPED/CODEOWNERS", "--create")
	if code != cli.ExitInvalid {
		t.Errorf("--file ../ESCAPED/CODEOWNERS: exit %d, want 3\nstderr: %s", code, errOut)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Errorf("%s exists: --create wrote outside the repository", outside)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("an argument-class rejection emits no record (D8), got: %q", out)
	}

	// An absolute path must be REFUSED, never quietly re-rooted inside the repo.
	abs := filepath.Join(t.TempDir(), "x", "ABS.txt")
	code2, _, errOut2 := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/services/api/, @org/b)", "--file", abs, "--create")
	if code2 != cli.ExitInvalid {
		t.Errorf("--file %s: exit %d, want 3\nstderr: %s", abs, code2, errOut2)
	}
	if _, err := os.Stat(abs); err == nil {
		t.Errorf("%s exists: --create wrote outside the repository", abs)
	}
	if _, err := os.Stat(filepath.Join(repo, abs)); err == nil {
		t.Errorf("%s exists: an absolute --file was reinterpreted as repo-relative and built a lookalike tree inside the clone", filepath.Join(repo, abs))
	}

	// The guard must not cost the legitimate spellings anything.
	if code, _, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/services/api/, @org/b)", "--file", "docs/CODEOWNERS", "--create"); code != cli.ExitOK {
		t.Fatalf("--file docs/CODEOWNERS: exit %d, want 0 — the containment guard must not reject repo-relative paths\nstderr: %s", code, errOut)
	}
	if got := syncReadFile(t, filepath.Join(repo, "docs", "CODEOWNERS")); !strings.Contains(got, "@org/b") {
		t.Errorf("docs/CODEOWNERS = %q", got)
	}
}

// Review finding (F3): `--repo` pointing BELOW a repository root wrote a
// CODEOWNERS that git will never read, and called it applied.
//
// gittree.ListTracked runs `git -C <repo>`, and git walks UP to the enclosing
// repository rather than refusing: pointed at rK/sub it answers with rK's tree
// minus the `sub/` prefix. Nothing then looks wrong from inside — the scopes
// match, the plan is proven, the write succeeds — and the run reports
// created:true, status "applied", exit 0. What it produced is
// rK/sub/.github/CODEOWNERS, and GitHub loads only the CODEOWNERS at the
// repository ROOT (or its .github/ or docs/), so the file governs nothing;
// worse, the rules inside it are anchored at that root, where `/services/api/`
// names a directory that does not exist. The repository's real CODEOWNERS, if
// it has one, is never even read, because discovery looked below it.
//
// A fleet whose clone layout carries one extra directory level
// (…/clones/<repo>/checkout is a common one) writes 100 dead files and reports
// 100 successes: the exact "reported applied, dead on arrival" outcome this
// verb exists to prevent. Repo-specific, so exit 2 — recorded, and the loop
// moves on to the next clone.
func TestHardening_RepoMustBeTheRepositoryRoot(t *testing.T) {
	root := initRepo(t, map[string]string{
		"sub/services/api/main.go": "package main\n",
		"sub/README.md":            "x\n",
	})
	sub := filepath.Join(root, "sub")

	// The scope a fleet policy would carry for a clone rooted here: it resolves
	// against the prefix-stripped tree git hands back, which is what makes the
	// run look successful.
	code, out, errOut := runCLI(t, "sync", "--repo", sub,
		"--op", "add_owner(/services/api/, @org/api)", "--create", "--format", "json")
	if code != cli.ExitRefused {
		t.Errorf("--repo below the root: exit %d, want 2\nstderr: %s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusRefused {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusRefused)
	}
	if strings.TrimSpace(rec.Error) == "" {
		t.Error("the refusal must carry a reason, or the operator cannot tell a layout mistake from a broken clone")
	}
	if rec.Repo != sub {
		t.Errorf("record .repo = %q, want the --repo argument %q verbatim (D6)", rec.Repo, sub)
	}
	if _, err := os.Stat(filepath.Join(sub, ".github", "CODEOWNERS")); err == nil {
		t.Errorf("%s was written: git never reads it, and its existence makes every later run think this tree has a CODEOWNERS", filepath.Join(sub, ".github", "CODEOWNERS"))
	}
	if _, err := os.Stat(filepath.Join(root, ".github", "CODEOWNERS")); err == nil {
		t.Error("a refused run must write nothing at all, including at the real root")
	}

	// The root itself still works, so the guard is about the layout and not
	// about the repository.
	if code, _, errOut := runCLI(t, "sync", "--repo", root,
		"--op", "add_owner(/sub/services/api/, @org/api)", "--create"); code != cli.ExitOK {
		t.Fatalf("--repo at the root: exit %d, want 0\nstderr: %s", code, errOut)
	}
	if got := syncReadFile(t, filepath.Join(root, ".github", "CODEOWNERS")); !strings.Contains(got, "@org/api") {
		t.Errorf("root .github/CODEOWNERS = %q", got)
	}
}

// Review finding (F4): `--branch REF` proved the change against REF's tree and
// then wrote the working tree of whatever was checked out.
//
// On a clone standing on main, `sync --branch old` resolved every scope against
// old's tree, proved INV-2 there, and wrote main's file — exit 0, "applied",
// carrying a rule that is dead where it landed and justified by a tree nobody
// wrote to. sync.go already refused --create for exactly this reason; the
// argument never depended on the file being new. Writing is refused (exit 2)
// rather than silently downgraded to a dry run: an implied dry run would exit 0
// having written nothing, which under this contract reads as "converged", and a
// fleet of 100 silently unchanged repos with 100 green rows is worse than the
// bug. --dry-run still previews, and `plan` still targets another ref.
func TestHardening_NonHeadBranchMayNotWrite(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":  "# owners\n/legacy/ @org/a\n",
		"legacy/x.go": "package legacy\n",
	})
	hardeningGit(t, repo, "branch", "old")
	// Move main on, so `old` is genuinely a different commit.
	if err := os.WriteFile(filepath.Join(repo, "legacy", "y.go"), []byte("package legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hardeningGit(t, repo, "add", ".")
	hardeningGit(t, repo, "commit", "-qm", "second")
	before := syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))

	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--branch", "old",
		"--op", "add_owner(/legacy/, @org/legacy)", "--format", "json")
	if code != cli.ExitRefused {
		t.Errorf("--branch old (writing): exit %d, want 2\nstderr: %s", code, errOut)
	}
	if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != before {
		t.Errorf("the working tree was written while proving against another ref: %q → %q", before, got)
	}
	if rec := syncDecodeRecord(t, out); rec.Status != cli.StatusRefused {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusRefused)
	}

	// --dry-run is the supported way to ask about another ref: it writes no
	// CODEOWNERS, so there is no tree to disagree with.
	code2, _, errOut2 := runCLI(t, "sync", "--repo", repo, "--branch", "old",
		"--op", "add_owner(/legacy/, @org/legacy)", "--dry-run")
	if code2 != cli.ExitOK {
		t.Errorf("--branch old --dry-run: exit %d, want 0 — previewing another ref writes nothing and is always safe\nstderr: %s", code2, errOut2)
	}
	if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != before {
		t.Errorf("--dry-run wrote to CODEOWNERS: %q → %q", before, got)
	}

	// Naming the checked-out branch explicitly is the ordinary fleet
	// invocation and still writes: the refs are compared by resolved commit,
	// not by the literal string "HEAD".
	code3, _, errOut3 := runCLI(t, "sync", "--repo", repo, "--branch", "main",
		"--op", "add_owner(/legacy/, @org/legacy)")
	if code3 != cli.ExitOK {
		t.Fatalf("--branch main on a clone checked out at main: exit %d, want 0\nstderr: %s", code3, errOut3)
	}
	if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); !strings.Contains(got, "@org/legacy") {
		t.Errorf("CODEOWNERS = %q, want the new owner", got)
	}
}

// Review finding (F5): a refused WRITE left an applied-looking record.
//
// When the write itself failed, sync.go set status/error and cleared `created`
// but left ops, ops_applied, paths_changed and the changes array exactly as the
// planner produced them. The row then said a repo whose CODEOWNERS is
// byte-for-byte unchanged had applied N ops and changed M paths, so the
// fleet report's `jq '[.[].ops_applied] | add'` overcounted the rollout by
// precisely the repos where nothing was written — the operator reads a total
// that includes the failures and believes more of the fleet moved than did.
func TestHardening_RefusedWriteRecordCountsNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not stop the write")
	}
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "# owners\n/x/ @a\n",
		"x/a.go":             "package x\n",
	})
	dir := filepath.Join(repo, ".github")
	before := syncReadFile(t, filepath.Join(dir, "CODEOWNERS"))
	// apply writes atomically via a temp file in the target's directory, so a
	// read-only directory fails the write while leaving the file readable.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/x/, @b)", "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("unwritable CODEOWNERS directory: exit %d, want 2\nstderr: %s", code, errOut)
	}
	if got := syncReadFile(t, filepath.Join(dir, "CODEOWNERS")); got != before {
		t.Fatalf("the file changed after a failed write: %q → %q", before, got)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusRefused {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusRefused)
	}
	if rec.OpsApplied != 0 {
		t.Errorf("ops_applied = %d, want 0 — nothing reached disk, and `jq '[.[].ops_applied]|add'` sums this field across the fleet", rec.OpsApplied)
	}
	if rec.PathsChanged != 0 {
		t.Errorf("paths_changed = %d, want 0 — no path changed owners", rec.PathsChanged)
	}
	if len(rec.Changes) != 0 {
		t.Errorf("changes carries %d entry(s) for a file that was never written: %+v", len(rec.Changes), rec.Changes)
	}
	if rec.Created {
		t.Error("created must be false when the write failed")
	}
	for _, o := range rec.Ops {
		if o.Status == "applied" {
			t.Errorf("op %q reports status %q on a run that wrote nothing", o.ID, o.Status)
		}
	}
}

// Review finding: a --dry-run record was byte-identical to a record from a real
// rollout, and the markdown summary contradicted itself.
//
// Nothing in results.jsonl said "this was a preview", so an operator (or a
// script) holding a file of records could not tell a modelled fleet from a
// changed one — and the two are collected by the same `>> results.jsonl` line.
// The PR body was worse than ambiguous: it claimed "a new CODEOWNERS file was
// written" and then, three lines down, "nothing was written".
func TestHardening_DryRunRecordSaysSoAndSummaryAgrees(t *testing.T) {
	repo := initRepo(t, map[string]string{"services/api/main.go": "package main\n"})
	sumPath := filepath.Join(t.TempDir(), "body.md")

	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/api)",
		"--create", "--dry-run", "--format", "json", "--summary-out", sumPath)
	if code != cli.ExitOK {
		t.Fatalf("dry run: exit %d, want 0\nstderr: %s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if !rec.DryRun {
		t.Error("dry_run is absent or false on a --dry-run record: the row is then indistinguishable from a real rollout's")
	}
	if _, err := os.Stat(filepath.Join(repo, ".github", "CODEOWNERS")); err == nil {
		t.Error("--dry-run created a file")
	}
	summary := syncReadFile(t, sumPath)
	if strings.Contains(summary, "file was written") {
		t.Errorf("the preview's PR body claims a file was written, then says nothing was:\n%s", summary)
	}
	if !strings.Contains(summary, "nothing was written") {
		t.Errorf("a preview body must say it is a preview:\n%s", summary)
	}

	// The real run is the control: no dry_run key, and the summary says what
	// actually happened.
	sumPath2 := filepath.Join(t.TempDir(), "body2.md")
	code2, out2, errOut2 := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/api)",
		"--create", "--format", "json", "--summary-out", sumPath2)
	if code2 != cli.ExitOK {
		t.Fatalf("real run: exit %d, want 0\nstderr: %s", code2, errOut2)
	}
	if rec2 := syncDecodeRecord(t, out2); rec2.DryRun {
		t.Error("dry_run is true on a run that actually wrote")
	}
	if summary2 := syncReadFile(t, sumPath2); !strings.Contains(summary2, "file was written") {
		t.Errorf("a real run's body must report the file it wrote:\n%s", summary2)
	}
}
