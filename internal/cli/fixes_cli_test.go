// Neighbours of the seven CLI fixes in this change: the cases each fix must
// NOT sweep up. Every test here passes before the fix as well as after — that
// is the point. What each one holds is written above it, together with the
// mutation of the product code that makes it fail, because a guard nobody has
// broken on purpose is a guard nobody knows works.
package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// A CODEOWNERS git tracks must not be called untracked.
//
// The D5 disclosure is conditioned on `!trackedAt(tree, rel)`; without that
// condition every ordinary repository in a fleet would carry a warning saying
// GitHub reads nothing from its CODEOWNERS, which is both false and the
// fastest way to teach an operator to stop reading warnings.
//
// MUTATION: drop the `!trackedAt(tree, rel)` guard in governingWarnings and
// this fails — the warning fires on the committed file.
func TestFix_TrackedCodeownersIsNotCalledUntracked(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/every\n",
		"services/api/main.go": "",
	})
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/platform)")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("sync on an ordinary repo: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "git has never recorded it") {
		t.Errorf("sync called a committed .github/CODEOWNERS untracked\noutput:\n%s", out)
	}
}

// `--create` writes a file that is untracked by definition, and saying so is
// noise: the record already reports `created`, and there is no earlier state
// to have committed.
//
// MUTATION: drop the `!creating` guard in governingWarnings and this fails.
func TestFix_CreateDoesNotWarnAboutTheFileItIsCreating(t *testing.T) {
	repo := initRepo(t, map[string]string{"services/api/main.go": ""})
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--create",
		"--op", "add_owner(/services/api/, @org/platform)")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("sync --create: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "git has never recorded it") {
		t.Errorf("--create warned that the file it just created is not tracked\noutput:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".github/CODEOWNERS")); err != nil {
		t.Errorf("--create wrote nothing: %v", err)
	}
}

// D5 still converges. An untracked CODEOWNERS the operator can commit is
// DISCLOSED, not refused: the fallback exists so a nightly job that created
// the file in pass 1 can amend it in pass 2, and a refusal would make that
// sequence impossible for every repo the job bootstraps.
//
// MUTATION: turn the governingWarnings case into a refusal (return an error
// from execute instead of appending a warning) and this fails at exit 2.
func TestFix_UntrackedButCommittableCodeownersStillConverges(t *testing.T) {
	repo := initRepo(t, map[string]string{"services/api/main.go": ""})
	if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".github/CODEOWNERS"), []byte("* @org/every\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/platform)")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Errorf("an untracked CODEOWNERS the operator can still commit was refused (exit %d) — D5 exists so pass 2\n"+
			"of a nightly job can amend what pass 1 created\noutput:\n%s", code, out)
	}
	// "does not record it at THAT REF", not "has never recorded it": the check
	// is trackedAt over one tree, and a file committed then `git rm --cached`
	// in a later commit has a history the second phrasing denies. A message a
	// reader can falsify with one `git log` is the failure mode this branch
	// has already turned back three times.
	if !strings.Contains(out, "not tracked at HEAD") || !strings.Contains(out, "does not record it at that ref") {
		t.Errorf("the run converged without disclosing that GitHub reads nothing from this file yet\noutput:\n%s", out)
	}
	if strings.Contains(out, "never recorded") {
		t.Errorf("the disclosure claims the file has no history, which trackedAt cannot know:\n%s", out)
	}
	b, err := os.ReadFile(filepath.Join(repo, ".github/CODEOWNERS"))
	if err != nil || !strings.Contains(string(b), "@org/platform") {
		t.Errorf("the amendment was not written: %v\n%s", err, b)
	}
}

// The ignore refusal is about the CODEOWNERS, not about the repository having
// a .gitignore. Refusing on the presence of ignore rules would stop nearly
// every real repository in a fleet.
//
// MUTATION: make refuseIgnoredCodeowners fire whenever `.gitignore` exists
// (rather than when check-ignore matches rel) and this fails at exit 2.
func TestFix_IgnoreRulesAboutOtherPathsDoNotRefuseTheRun(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".gitignore":           "node_modules/\n*.log\n",
		"services/api/main.go": "",
	})
	if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".github/CODEOWNERS"), []byte("* @org/every\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/platform)")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Errorf("a .gitignore that says nothing about CODEOWNERS stopped the run (exit %d)\noutput:\n%s", code, out)
	}
	if strings.Contains(out, "ignore rules match it") {
		t.Errorf("the ignore refusal fired for a file git is perfectly willing to track\noutput:\n%s", out)
	}
}

// The migration guard is directional. A LOWER-precedence CODEOWNERS in the
// working tree changes nothing when it lands — `.github/CODEOWNERS` still
// wins under S-8 — so neither `sync` nor `audit` has anything to say about it.
//
// MUTATION: drop the outranksCodeowners test in refuseWorkTreeSupersede (and
// in stagedHigherCodeowners), leaving "present on disk and not tracked", and
// both halves of this fail.
func TestFix_LowerPrecedenceWorkTreeCodeownersIsNotAMigration(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/everyone\n",
		"services/api/main.go": "",
	})
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs/CODEOWNERS"), []byte("* @org/newer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "docs/CODEOWNERS")

	code, stdout, stderr := runCLI(t, "audit", "--repo", repo)
	if out := stdout + stderr; strings.Contains(out, "is in the working tree but not committed at this ref") {
		t.Errorf("audit reported a staged docs/CODEOWNERS as superseding .github/CODEOWNERS; it outranks nothing\noutput:\n%s", out)
	} else if code != cli.ExitOK {
		t.Errorf("audit: exit %d on a repo with nothing wrong with it\noutput:\n%s", code, out)
	}

	code, stdout, stderr = runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/api)")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Errorf("sync refused over a staged docs/CODEOWNERS that will never govern (exit %d)\noutput:\n%s", code, out)
	}
	b, err := os.ReadFile(filepath.Join(repo, ".github/CODEOWNERS"))
	if err != nil || !strings.Contains(string(b), "@org/api") {
		t.Errorf("the governing .github/CODEOWNERS was not amended: %v\n%s", err, b)
	}
}

// --file is the migration guard's escape hatch, and it has to actually open.
// The refusal's own advice is "pass --file to say which of the two this run
// should edit", so a --file run that still refused would be advice into a
// wall.
//
// MUTATION: drop the `r.filePath != ""` early return in
// refuseWorkTreeSupersede and this fails at exit 2.
func TestFix_FileNamesWhichHalfOfAMigrationToEdit(t *testing.T) {
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

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--file", ".github/CODEOWNERS",
		"--op", "add_owner(/services/api/, @org/api)")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("--file naming the incoming file was refused anyway (exit %d) — the refusal advises exactly this\noutput:\n%s", code, out)
	}
	b, err := os.ReadFile(filepath.Join(repo, ".github/CODEOWNERS"))
	if err != nil || !strings.Contains(string(b), "@org/api") {
		t.Errorf("the named file was not the one edited: %v\n%s", err, b)
	}
	// The outgoing file must be untouched: naming one file is not permission
	// to rewrite the other.
	if b, err := os.ReadFile(filepath.Join(repo, "CODEOWNERS")); err != nil || strings.Contains(string(b), "@org/api") {
		t.Errorf("the outgoing root CODEOWNERS was edited too: %v\n%s", err, b)
	}
}

// audit reads the ref, and the working tree belongs to HEAD. Auditing a ref
// this clone is not standing on must not report files on disk against it —
// those files are a different commit's, and the finding would be an assertion
// about a tree nobody looked at.
//
// MUTATION: make refIsCheckedOut return true unconditionally and this fails —
// .github/CODEOWNERS, committed on main and absent from `release`, is reported
// as staged against release.
func TestFix_AuditDoesNotReadTheWorkTreeAgainstAnotherRef(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":           "* @org/everyone\n",
		"services/api/main.go": "",
	})
	gitRun(t, repo, "branch", "release")
	if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".github/CODEOWNERS"), []byte("* @org/newer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".github/CODEOWNERS")
	gitRun(t, repo, "commit", "-qm", "migrate")

	// Sanity: the two refs really do differ on that path.
	if _, out, _ := runGit(t, repo, "ls-tree", "--name-only", "release", ".github/"); strings.Contains(out, "CODEOWNERS") {
		t.Fatalf("fixture drifted: release already carries .github/CODEOWNERS\n%s", out)
	}
	_, stdout, stderr := runCLI(t, "audit", "--repo", repo, "--branch", "release")
	if out := stdout + stderr; strings.Contains(out, "is in the working tree but not committed at this ref") {
		t.Errorf("audit --branch release reported the working tree, which belongs to main\noutput:\n%s", out)
	}
}

// The discovery-path refusal keeps its original sentence. A repo that really
// has no CODEOWNERS anywhere is the case that head was written for, and every
// fleet script and triage note that greps for it predates this change.
//
// MUTATION: route every noCodeownersError through fileError and this fails.
func TestFix_NoCodeownersAnywhereKeepsItsRefusal(t *testing.T) {
	repo := initRepo(t, map[string]string{"services/api/main.go": ""})
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/platform)")
	out := stdout + stderr
	if code != cli.ExitRefused {
		t.Fatalf("want exit 2, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "no CODEOWNERS file found in .github/, root, or docs/ at HEAD") {
		t.Errorf("the discovery refusal lost its wording; it is true here and scripts grep for it\noutput:\n%s", out)
	}
	if !strings.Contains(out, "--create to write one at .github/CODEOWNERS") {
		t.Errorf("the discovery remedy lost the path --create would really write\noutput:\n%s", out)
	}
}

// A missing --file target in a repo that has no CODEOWNERS either must not
// claim a governing file it does not have — the "drop --file to amend that
// file" advice would point at nothing.
//
// The path deliberately does not end in `OWNERS`: every message in this area
// contains the word CODEOWNERS, so asserting on `OWNERS` alone passes without
// the fix and proves nothing.
//
// MUTATION: hardcode the existing-file clause (drop the `e.existing != ""`
// test in fileError) and this fails.
func TestFix_MissingFileTargetWithNoGoverningFileClaimsNone(t *testing.T) {
	repo := initRepo(t, map[string]string{"services/api/main.go": ""})
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--file", "ownership/OWNERS.txt",
		"--op", "add_owner(/services/api/, @org/platform)")
	out := stdout + stderr
	if code != cli.ExitRefused {
		t.Fatalf("want exit 2, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "ownership/OWNERS.txt") {
		t.Errorf("the refusal never names the path --file pointed at\noutput:\n%s", out)
	}
	if strings.Contains(out, "IS governed by") {
		t.Errorf("the refusal claims a governing file in a repo that has none\noutput:\n%s", out)
	}
	if !strings.Contains(out, "--create to write one at ownership/OWNERS.txt") {
		t.Errorf("the remedy must name the path --create would really write\noutput:\n%s", out)
	}
}

// And the remedy that refusal prints has to be true: --create with the same
// --file writes exactly there, and nowhere else.
//
// MUTATION: make governing()'s create branch fall back to
// gittree.CodeownersLocations[0] when --create is set, and this fails — the
// remedy would then be advertising a path the run does not write.
func TestFix_TheCreateRemedyWritesWhereItSays(t *testing.T) {
	repo := initRepo(t, map[string]string{"services/api/main.go": ""})
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--file", "ownership/OWNERS.txt", "--create",
		"--op", "add_owner(/services/api/, @org/platform)")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("sync --file … --create: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(repo, "ownership/OWNERS.txt")); err != nil {
		t.Errorf("--create did not write the path the refusal advertises: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".github/CODEOWNERS")); err == nil {
		t.Errorf(".github/CODEOWNERS was written instead of the --file path")
	}
}

// R-24's disclosure must stay a disclosure. plan, apply and lint pointed at
// the file that DOES govern have nothing to report, and a warning on every
// ordinary run is a warning nobody reads.
//
// MUTATION: drop the `relClean(rel) != present[0]` test in governingWarnings
// and all three clauses fail.
func TestFix_PlanApplyAndLintAreSilentOnTheGoverningFile(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/every\n",
		"services/api/main.go": "",
	})
	planPath := filepath.Join(t.TempDir(), "plan.json")
	code, stdout, stderr := runCLI(t, "plan", "--repo", repo,
		"--op", "add_owner(/services/api/, @org/platform)", "--out", planPath)
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("plan: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "govern nothing") {
		t.Errorf("plan warned about the file GitHub actually reads\noutput:\n%s", out)
	}
	code, stdout, stderr = runCLI(t, "apply", "--plan", planPath, "--repo", repo)
	out = stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "govern nothing") {
		t.Errorf("apply warned about the file GitHub actually reads\noutput:\n%s", out)
	}
	t.Setenv("GITHUB_TOKEN", "")
	code, stdout, stderr = runCLI(t, "lint", "--repo", repo, "--remove-stale-paths")
	out = stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("lint: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "govern nothing") {
		t.Errorf("lint warned about the file GitHub actually reads\noutput:\n%s", out)
	}
}

// R-24 says the record says so "on stderr and in `warnings`". For `plan` the
// record is the plan file, which is the artifact a human reviews — stderr
// scrolls past in CI and the JSON is what gets attached to the PR.
//
// MUTATION: print the warnings to stderr only, without appending them to
// p.Warnings, and this fails while the target test still passes.
func TestFix_PlanCarriesTheDisclosureInTheArtifact(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/every\n",
		"docs/CODEOWNERS":      "* @org/stale\n",
		"services/api/main.go": "",
	})
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, o, e := runCLI(t, "plan", "--repo", repo, "--file", "docs/CODEOWNERS",
		"--op", "add_owner(/services/api/, @org/platform)", "--out", planPath); code != cli.ExitOK {
		t.Fatalf("plan: exit %d\n%s%s", code, o, e)
	}
	b, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var pf struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(b, &pf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(pf.Warnings, "\n"), ".github/CODEOWNERS") {
		t.Errorf("the plan's own warnings never name the file GitHub resolves from; stderr is not the artifact\nwarnings: %v", pf.Warnings)
	}
}

// --file naming a path the ref DOES carry is the ordinary use of the flag and
// must keep working: the new refusal is about absence from the ref, not about
// the flag.
//
// MUTATION: invert the trackedAt test in codeownersAtRef and this fails at
// exit 3 for both verbs.
func TestFix_FileNamingATrackedCodeownersStillReads(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/every\n",
		"docs/CODEOWNERS":      "/services/ @org/docs-team\n",
		"services/api/main.go": "",
	})
	snapPath := filepath.Join(t.TempDir(), "snap.json")
	if code, o, e := runCLI(t, "snapshot", "--repo", repo, "--file", "docs/CODEOWNERS", "--out", snapPath); code != cli.ExitOK {
		t.Fatalf("snapshot --file docs/CODEOWNERS: exit %d\n%s%s", code, o, e)
	}
	b, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Ownership map[string][]string `json:"ownership"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	// The named file, not the governing one: docs/CODEOWNERS owns
	// services/api/main.go and .github/CODEOWNERS's `*` would own everything.
	if got := snap.Ownership["services/api/main.go"]; len(got) != 1 || got[0] != "@org/docs-team" {
		t.Errorf("snapshot did not resolve from the file --file named; owners of services/api/main.go = %v", got)
	}
	if got, ok := snap.Ownership["docs/CODEOWNERS"]; ok && len(got) != 0 {
		t.Errorf("snapshot resolved from .github/CODEOWNERS after all; docs/CODEOWNERS owners = %v", got)
	}
	if code, o, e := runCLI(t, "audit", "--repo", repo, "--file", "docs/CODEOWNERS", "--checks", "a12"); code != cli.ExitOK {
		t.Fatalf("audit --file docs/CODEOWNERS: exit %d\n%s%s", code, o, e)
	}
}

// The missing-target refusal stays in the exit class its sibling is in.
// locate's "no CODEOWNERS file found … use --file to specify one" is exit 3 on
// these two verbs, and splitting one "which file does this ref carry?"
// question across two exit classes costs a script more than either class does.
//
// MUTATION: return a plan.RefusalError from codeownersAtRef and this fails at
// exit 2.
func TestFix_FileNotInRefKeepsItsExitClass(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"a.md":               "",
	})
	for _, verb := range []string{"snapshot", "audit"} {
		code, stdout, stderr := runCLI(t, verb, "--repo", repo, "--file", "docs/CODEOWNERS")
		out := stdout + stderr
		if code != cli.ExitInvalid {
			t.Errorf("%s --file docs/CODEOWNERS: want exit 3 (the class locate's sibling verdict lands in), got %d\n%s", verb, code, out)
		}
		if !strings.Contains(out, "docs/CODEOWNERS") || !strings.Contains(out, "HEAD") {
			t.Errorf("%s: the refusal must name the path and the ref\noutput:\n%s", verb, out)
		}
		if !strings.Contains(out, ".github/CODEOWNERS") {
			t.Errorf("%s: the refusal must name the CODEOWNERS the ref DOES carry\noutput:\n%s", verb, out)
		}
	}
}

// Defining --cache-dir on lint's flagset must not make lint act as though it
// were passed. The refusal is conditioned on a non-empty value, and --cache-ttl
// on the flag having been TYPED — its default is 24h, so its value cannot
// answer "did you ask for this?".
//
// MUTATION: refuse whenever the flags are defined (drop the `r.cacheDir != ""`
// and `r.cacheTTLSet` tests) and this fails at exit 3.
func TestFix_LintWithoutCacheFlagsStillRuns(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"a.md":               "",
	})
	t.Setenv("GITHUB_TOKEN", "")
	code, stdout, stderr := runCLI(t, "lint", "--repo", repo, "--remove-stale-paths")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("lint with no cache flags: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "--cache-dir is not available") || strings.Contains(out, "--cache-ttl has no effect") {
		t.Errorf("lint refused cache flags nobody passed\noutput:\n%s", out)
	}
}

// The other half of what AUDIT.md documents as rejected: `--cache-ttl` is a
// TTL over a disk cache lint does not use, so it governs nothing. It was
// reachable only through `audit --lint` for the same reason --cache-dir was.
//
// MUTATION: remove the cache-ttl flag from lint's flagset and this fails with
// the flag package's `flag provided but not defined: -cache-ttl`.
func TestFix_LintRefusesCacheTTLWithTheDocumentedReason(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"a.md":               "",
	})
	t.Setenv("GITHUB_TOKEN", "")
	code, stdout, stderr := runCLI(t, "lint", "--repo", repo, "--cache-ttl", "1h", "--remove-stale-paths")
	out := stdout + stderr
	if code != cli.ExitInvalid {
		t.Errorf("want exit 3 for --cache-ttl, got %d\noutput:\n%s", code, out)
	}
	if !strings.Contains(out, "--cache-ttl has no effect") {
		t.Errorf("lint did not print the documented --cache-ttl refusal\noutput:\n%s", out)
	}
	if strings.Contains(out, "flag provided but not defined") {
		t.Errorf("the flag package answered instead of the tool\noutput:\n%s", out)
	}
}

// apply's success line names the plan's own repo-relative path, whatever that
// path is and wherever the command is run from. `.github/CODEOWNERS` is the
// common case and would also match a hardcoded string, so this uses a repo
// governed by docs/CODEOWNERS and additionally requires the absolute repo path
// to be absent.
//
// MUTATION: print `target` (the absolute join) again and this fails on the
// absolute-path clause; hardcode ".github/CODEOWNERS" and it fails on the
// first.
func TestFix_ApplyNamesThePlansOwnPathNotAFixedOne(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"docs/CODEOWNERS":      "* @org/every\n",
		"services/api/main.go": "",
	})
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, o, e := runCLI(t, "plan", "--repo", repo,
		"--op", "add_owner(/services/api/, @org/platform)", "--out", planPath); code != cli.ExitOK {
		t.Fatalf("plan: exit %d\n%s%s", code, o, e)
	}
	code, stdout, stderr := runCLI(t, "apply", "--plan", planPath)
	if code != cli.ExitOK {
		t.Fatalf("apply: exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "applied: docs/CODEOWNERS (") {
		t.Errorf("apply did not name the plan's own path\nstdout:\n%s", stdout)
	}
	if strings.Contains(stdout, repo) {
		t.Errorf("apply's success line still carries the absolute repository path, which differs on every machine\n"+
			"repo: %s\nstdout:\n%s", repo, stdout)
	}
}

// SPEC R-23 (S-7): the unmerged guard reaches a CODEOWNERS whose path begins
// with `:`, which git reads as pathspec MAGIC rather than as a path.
//
// The review of the original fix disproved its own justification: dropping
// `:(literal)` broke no test, because the loop already compares the whole path
// exactly, so a glob metacharacter could never mis-select a record. The case
// it does prevent is this one — without the prefix `git status` matches
// nothing, the guard sees a clean tree, and the run rewrites the conflicted
// file reporting `applied (proven: tree)`.
func TestFix_UnmergedGuardReachesAPathThatLooksLikePathspecMagic(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/every\n",
		":weird/CODEOWNERS":    "* @org/every\n",
		"services/api/main.go": "",
	})
	before := fixConflictedMerge(t, repo, ":weird/CODEOWNERS", "/docs/ @org/a\n", "/docs/ @org/b\n")

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--file", ":weird/CODEOWNERS",
		"--op", "add_owner(/services/api/, @org/api)")
	out := stdout + stderr
	if code != cli.ExitRefused {
		t.Fatalf("unmerged CODEOWNERS at a pathspec-magic path: want exit 2, got %d\noutput:\n%s", code, out)
	}
	if got := syncReadFile(t, filepath.Join(repo, ":weird/CODEOWNERS")); got != before {
		t.Errorf("the conflicted file was rewritten:\n%s", got)
	}
}

// SPEC R-23: --dry-run does NOT lift the unmerged guard, unlike the S-7 branch
// guard it sits beside.
//
// There the bytes are real and only the ref is wrong, so a preview is honest.
// Here the preview would report ownership derived from text that is no version
// of the file, which is the defect itself rather than a safe rehearsal of it.
func TestFix_DryRunDoesNotLiftTheUnmergedGuard(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/every\n",
		"docs/x.md":            "",
		"services/api/main.go": "",
	})
	fixConflictedMerge(t, repo, ".github/CODEOWNERS", "/docs/ @org/a\n", "/docs/ @org/b\n")

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--dry-run",
		"--op", "add_owner(/services/api/, @org/api)")
	out := stdout + stderr
	if code != cli.ExitRefused {
		t.Errorf("--dry-run previewed a conflict-mangled file: want exit 2, got %d\noutput:\n%s", code, out)
	}
}

// SPEC R-24 (S-8): a superseding CODEOWNERS that git is told never to track is
// not a migration, so it does not refuse the run — and `sync` and `audit` agree
// about that.
//
// Review finding. `stagedHigherCodeowners` reads the work tree through
// `ls-files --exclude-standard` and its comment states the rule outright: a
// file git will never track can never supersede anything. `findOnDisk`, which
// the sync half used, is a bare os.Stat walk that honours no ignore rules. So
// a repo governed by a tracked docs/CODEOWNERS, with an IGNORED root
// CODEOWNERS on disk, was refused at exit 2 — banked by a fleet loop as
// needs-human — while `audit` on the same repo said clean and `plan` exited 0.
// The refusal even advised committing the migration, which `git add` declines
// for an ignored path.
func TestFix_AnIgnoredSupersedingCodeownersIsNotAMigration(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"docs/CODEOWNERS":      "* @org/every\n",
		".gitignore":           "/CODEOWNERS\n",
		"services/api/main.go": "",
	})
	// Ignored, so `git add CODEOWNERS` refuses it and it can never govern.
	if err := os.WriteFile(filepath.Join(repo, "CODEOWNERS"), []byte("* @org/stray\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/api)")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("an ignored root CODEOWNERS refused the run: want exit 0, got %d\n"+
			"git will never track it, so it supersedes nothing and the advice to commit it is one git declines\noutput:\n%s", code, out)
	}
	if got := syncReadFile(t, filepath.Join(repo, "docs/CODEOWNERS")); !strings.Contains(got, "@org/api") {
		t.Errorf("the governing file was not amended:\n%s", got)
	}
	// audit's half of the same check must agree.
	if code, ao, ae := runCLI(t, "audit", "--repo", repo, "--checks", "a10"); code != cli.ExitOK {
		t.Errorf("audit disagrees with sync about the same repository: want exit 0, got %d\n%s%s", code, ao, ae)
	}
}
