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

// The ref and the working tree can disagree, and `create` believed the working
// tree.
//
// `governing` resolves WHICH file to edit from the tracked tree (S-8 discovery
// runs over `git ls-tree`) and then reads its BYTES from disk. When those two
// answers disagree — the path is tracked, the file is not on disk — the read
// failed with os.ErrNotExist and `create` read that as "this repo has no
// CODEOWNERS", planned against an empty file, and wrote the result. The
// original rules were not merged, not backed up and not mentioned: a
// sparse-checkout that excludes `/.github/`, a partial clone, or a file someone
// deleted locally was enough to have a fleet run replace a repository's entire
// ownership file with one rule and report `"status":"applied"`, exit 0.
//
// R-23/R-34d say create "never overwrites an existing file", and the O_EXCL in
// `write` is what makes that a property of the syscall — but O_EXCL only sees
// the working tree, which is exactly the half that was empty. The rules below
// close it at the other end: the tool refuses when the two answers disagree,
// because it cannot tell what it would be destroying.
//
// Exit 2, not 3: which files a clone has checked out is a fact about ONE
// repository, so a fleet loop records it and steps to the next clone. Exit 3
// would halt a hundred-repo rollout on the first sparse clone in the list.

// checkoutRepoMissingFromDisk commits a CODEOWNERS at rel and then removes it
// from the working tree, leaving the file tracked at HEAD and absent on disk —
// what a sparse-checkout, a partial clone or a local `rm` all look like from
// here.
func checkoutRepoMissingFromDisk(t *testing.T, rel, content string) string {
	t.Helper()
	repo := initRepo(t, map[string]string{
		rel:             content,
		"docs/guide.md": "# guide\n",
		"services/s.go": "package s\n",
	})
	if err := os.Remove(filepath.Join(repo, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
	return repo
}

// SPEC R-23/R-34d: `create` REFUSES (exit 2) when the CODEOWNERS path is
// tracked in the ref but missing from the working tree, and writes nothing.
//
// The concrete loss: `.github/CODEOWNERS` holds `* @org/everyone` and
// `/services/ @org/platform`, the clone is sparse and does not check
// `/.github/` out, and a policy with `"create": true` adds one rule for
// `/docs/`. Before this rule the run wrote a file containing that one rule and
// nothing else — both original rules gone, `"created":true`, `paths_changed:1`
// and exit 0, so neither a reviewed `max_paths_changed` ceiling nor the record
// showed that a whole file had been replaced.
//
// The bytes are the assertion: the file must not exist on disk afterwards, and
// the tracked bytes at HEAD must be unchanged. A test that only read the exit
// code would pass against an implementation that refused AFTER truncating.
func TestCheckout_CreateRefusesWhenTrackedFileIsMissingFromWorkingTree(t *testing.T) {
	const tracked = "* @org/everyone\n/services/ @org/platform\n"
	repo := checkoutRepoMissingFromDisk(t, ".github/CODEOWNERS", tracked)
	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":["add_owner(/docs/, @org/docs-team)"]}`)

	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("exit %d, want %d (exit 2): a checkout that lacks a tracked file is a fact about THIS clone, so the fleet loop records it and steps on\nstdout:\n%s\nstderr:\n%s",
			code, cli.ExitRefused, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(repo, ".github", "CODEOWNERS")); !os.IsNotExist(err) {
		t.Fatalf(".github/CODEOWNERS exists after a refusal (stat err %v): the run wrote a file whose tracked version it never read", err)
	}
	if got := checkoutTrackedBytes(t, repo, ".github/CODEOWNERS"); got != tracked {
		t.Errorf("tracked bytes at HEAD changed.\n got: %q\nwant: %q", got, tracked)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusRefused {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusRefused)
	}
	if rec.Created {
		t.Error("created = true on a run that wrote nothing")
	}
	if rec.PathsChanged != 0 {
		t.Errorf("paths_changed = %d, want 0", rec.PathsChanged)
	}
	checkoutWantRefusalText(t, rec.Error)
}

// SPEC R-23/R-34d: `--dry-run` refuses the same checkout rather than previewing
// a creation.
//
// The preview is the review surface: it said "a new CODEOWNERS file WOULD be
// created" and showed one rule, disclosing nothing about the two rules the real
// run would drop. An operator reading that body approves the rollout.
func TestCheckout_DryRunRefusesTheSameCheckoutInsteadOfPreviewingACreate(t *testing.T) {
	repo := checkoutRepoMissingFromDisk(t, ".github/CODEOWNERS", "* @org/everyone\n/services/ @org/platform\n")
	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":["add_owner(/docs/, @org/docs-team)"]}`)
	sumPath := filepath.Join(t.TempDir(), "body.md")

	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--dry-run", "--format", "json", "--summary-out", sumPath)
	if code != cli.ExitRefused {
		t.Fatalf("--dry-run exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, cli.ExitRefused, out, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Created {
		t.Error("created = true: the preview still advertises a creation")
	}
	checkoutWantRefusalText(t, rec.Error)
	for _, line := range strings.Split(syncReadFile(t, sumPath), "\n") {
		if strings.Contains(line, "WOULD be created") {
			t.Errorf("the PR body previews a creation the real run refuses:\n%s", line)
		}
	}
}

// SPEC R-23: an explicit `--file` naming a tracked-but-absent path is refused
// on the same rule.
//
// Discovery is not the only way to reach the path — `--file .github/CODEOWNERS`
// reaches it directly — and the loss is identical, so the guard belongs where
// the two answers are compared and not in the discovery branch alone.
func TestCheckout_ExplicitFileFlagIsRefusedOnTheSameRule(t *testing.T) {
	repo := checkoutRepoMissingFromDisk(t, "docs/CODEOWNERS", "* @org/everyone\n")
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/docs/, @org/docs-team)",
		"--file", "docs/CODEOWNERS", "--create", "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, cli.ExitRefused, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(repo, "docs", "CODEOWNERS")); !os.IsNotExist(err) {
		t.Fatalf("docs/CODEOWNERS exists after a refusal (stat err %v)", err)
	}
	checkoutWantRefusalText(t, syncDecodeRecord(t, out).Error)
}

// SPEC R-23: the real sparse-checkout, end to end.
//
// The rule above is proven with a deleted file because that is portable and
// fast, but the reported failure was a sparse clone, and the two must reach the
// same code. `git sparse-checkout` needs git 2.25; the test skips rather than
// fails where it is unavailable, since an old git is not a defect in this tool.
func TestCheckout_SparseCheckoutIsRefusedNotTreatedAsAnEmptyRepo(t *testing.T) {
	const tracked = "* @org/everyone\n/services/ @org/platform\n"
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": tracked,
		"docs/guide.md":      "# guide\n",
		"services/s.go":      "package s\n",
	})
	for _, args := range [][]string{
		{"sparse-checkout", "init", "--no-cone"},
		{"sparse-checkout", "set", "/docs/", "/services/"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v unavailable here: %v\n%s", args, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".github", "CODEOWNERS")); !os.IsNotExist(err) {
		t.Skipf("sparse-checkout did not exclude .github/ (stat err %v); the fixture proves nothing", err)
	}

	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":["add_owner(/docs/, @org/docs-team)"]}`)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, cli.ExitRefused, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(repo, ".github", "CODEOWNERS")); !os.IsNotExist(err) {
		t.Fatal(".github/CODEOWNERS was written into a sparse clone that deliberately excludes it")
	}
	if got := checkoutTrackedBytes(t, repo, ".github/CODEOWNERS"); got != tracked {
		t.Errorf("tracked bytes at HEAD changed.\n got: %q\nwant: %q", got, tracked)
	}
	checkoutWantRefusalText(t, syncDecodeRecord(t, out).Error)
}

// SPEC R-23: a read that fails for a reason other than absence is its own
// refusal, never a create.
//
// `governing` reads the file and interprets the error. os.ErrNotExist means
// "absent"; EISDIR and a permission denial do not, and treating them as absence
// would put the whole finding above back with a different errno in front of it.
// The message has to say the file could not be READ, because "no CODEOWNERS
// found" sends the operator to add `create` to a policy that is already
// correct.
func TestCheckout_UnreadableFileIsRefusedAndNotReportedAsAbsent(t *testing.T) {
	repo := initRepo(t, map[string]string{"docs/guide.md": "# guide\n"})
	// A directory where the file should be: os.ReadFile fails with EISDIR,
	// which is not os.ErrNotExist.
	if err := os.MkdirAll(filepath.Join(repo, "docs", "CODEOWNERS"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/docs/, @org/docs-team)",
		"--file", "docs/CODEOWNERS", "--create", "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, cli.ExitRefused, out, errOut)
	}
	msg := syncDecodeRecord(t, out).Error
	if !strings.Contains(msg, "could not be read") {
		t.Errorf("refusal does not say the file could not be READ, so it reads as absence:\n%s", msg)
	}
	if !strings.Contains(msg, "docs/CODEOWNERS") {
		t.Errorf("refusal does not name the path it failed on:\n%s", msg)
	}
}

// SPEC R-23: a tracked CODEOWNERS that is a symlink to a missing target is
// refused, and the refusal says so rather than claiming the file is absent.
//
// The read fails with os.ErrNotExist exactly as a sparse checkout's does, and
// the same rule applies — nothing was read, so nothing may be written over it.
// The wording is the part worth pinning: something IS at that path, and telling
// the operator to restore a file they can see with `ls` sends them looking for
// the wrong thing. (Containment still refuses a link that leaves the clone; this
// refusal comes first because it is true whether the target is inside or out.)
func TestCheckout_TrackedDanglingSymlinkSaysWhatIsActuallyWrong(t *testing.T) {
	repo := symlinkRepo(t, ".github/CODEOWNERS", "MISSING.txt", map[string]string{
		"docs/guide.md": "# guide\n",
	})
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/docs/, @org/docs)",
		"--create", "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, cli.ExitRefused, out, errOut)
	}
	msg := syncDecodeRecord(t, out).Error
	if !strings.Contains(msg, "symlink") || !strings.Contains(msg, "target") {
		t.Errorf("the refusal describes a missing file, but the path holds a symlink whose target is gone:\n%s", msg)
	}
	checkoutWantRefusalText(t, msg)
}

// checkoutWantRefusalText pins what the refusal has to tell the operator. The
// four facts are the operator's whole diagnosis: WHICH file, that the REF has
// it, that the WORKING TREE does not, and that the fix is to the checkout. The
// last assertion is the load-bearing one — advising `create` here is advising
// the operator to destroy the file.
func checkoutWantRefusalText(t *testing.T, msg string) {
	t.Helper()
	for _, want := range []string{"tracked", "working tree", "checkout"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must say %q; got:\n%s", want, msg)
		}
	}
	for _, forbidden := range []string{"--create", `"create": true`, "re-run with --create"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("refusal advises %q — following that advice replaces the tracked file with the ops alone:\n%s", forbidden, msg)
		}
	}
}

// checkoutTrackedBytes reads rel out of HEAD, so the assertion is about what
// the repository still holds and not about what happens to be on disk.
func checkoutTrackedBytes(t *testing.T, repo, rel string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "show", "HEAD:"+rel).Output()
	if err != nil {
		t.Fatalf("git show HEAD:%s: %v", rel, err)
	}
	return string(out)
}

// FINDING 5 — `--file` may not name a path inside the git directory.
//
// `containedRelPath` rejects `--file ../ESCAPED/CODEOWNERS` and
// `--file /abs/path`, both because the write lands somewhere the repository
// does not own. `.git/CODEOWNERS` is the third spelling of that and was
// accepted: with `--create` the tool wrote a real file INSIDE `.git`, warned
// that GitHub does not read it, and exited 0. Nothing GitHub loads lives there,
// git's own directory is not a place a tool may deposit files, and a fleet loop
// pointed at 100 clones does it 100 times.

// SPEC R-23: `--file` naming anything inside the git directory is exit 3, and
// nothing is written there.
//
// Exit 3 rather than 2, matching `--file ../escape/…`: it is decidable from the
// argument alone with no repository open, so it is the same verdict on every
// repo in the fleet and the run should halt at repo 0 instead of recording the
// identical refusal a hundred times.
func TestCheckout_FileInsideTheGitDirectoryIsRefused(t *testing.T) {
	for _, rel := range []string{".git/CODEOWNERS", ".git/hooks/CODEOWNERS", "vendor/lib/.git/CODEOWNERS"} {
		t.Run(rel, func(t *testing.T) {
			repo := initRepo(t, map[string]string{"docs/guide.md": "# guide\n"})
			code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/docs/, @org/docs)",
				"--file", rel, "--create")
			if code != cli.ExitInvalid {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, cli.ExitInvalid, out, errOut)
			}
			if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(rel))); !os.IsNotExist(err) {
				t.Errorf("%s exists (stat err %v): the tool wrote into the git directory", rel, err)
			}
			if !strings.Contains(errOut, ".git") {
				t.Errorf("the refusal does not name the git directory, so the operator cannot see which part of --file was wrong:\n%s", errOut)
			}
		})
	}
}

// FINDING 3 — the PR body credited a flag that could not have been passed.
//
// `--policy p.json --create` is exit 3 (R-34b), so on a policy run the flag is
// unpassable and `"create": true` in the file is what ran. The body said
// ``a new CODEOWNERS file was written (`--create`)`` anyway. R-24's body is the
// review surface R-20 exists to produce: a reviewer who reads it and goes
// looking for `--create` in the fleet script finds nothing, and the reviewed
// artifact gets no credit for the one setting that mattered.

// SPEC R-24/R-34a: the PR body names the spelling that actually applied — the
// policy field on a `--policy` run, the flag on an `--op` run.
//
// Asserted as a whole LINE rather than as a substring of the body: the body
// carries the policy path in several places, so "the text mentions the policy
// somewhere" is satisfied by a body that still credits the flag.
func TestSummary_CreateCreditsThePolicyFieldNotTheFlag(t *testing.T) {
	repo := initRepo(t, map[string]string{"docs/guide.md": "# guide\n"})
	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":["add_owner(/docs/, @org/docs-team)"]}`)
	sumPath := filepath.Join(t.TempDir(), "body.md")

	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--summary-out", sumPath)
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr:\n%s", code, errOut)
	}
	want := "- a new CODEOWNERS file was written (`\"create\": true` in `" + pol + "`)"
	if !checkoutHasLine(syncReadFile(t, sumPath), want) {
		t.Errorf("PR body has no line\n%s\ngot:\n%s", want, syncReadFile(t, sumPath))
	}
	for _, line := range strings.Split(syncReadFile(t, sumPath), "\n") {
		if strings.Contains(line, "`--create`") {
			t.Errorf("the body credits `--create`, which R-34b makes exit 3 on a policy run:\n%s", line)
		}
	}
}

// SPEC R-24/R-34a: the `--op` spelling still credits the flag, and under
// --dry-run both spellings stay in the future tense.
//
// The flag half is the control: a fix that simply renamed the string would
// mis-credit the other half of the same sentence, and an operator reading a
// body from an `--op` run would go looking for a policy file that does not
// exist.
func TestSummary_CreateCreditsTheFlagOnAnOpRun(t *testing.T) {
	repo := initRepo(t, map[string]string{"docs/guide.md": "# guide\n"})
	sumPath := filepath.Join(t.TempDir(), "body.md")
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/docs/, @org/docs-team)",
		"--create", "--summary-out", sumPath)
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr:\n%s", code, errOut)
	}
	if want := "- a new CODEOWNERS file was written (`--create`)"; !checkoutHasLine(syncReadFile(t, sumPath), want) {
		t.Errorf("PR body has no line\n%s\ngot:\n%s", want, syncReadFile(t, sumPath))
	}

	repo2 := initRepo(t, map[string]string{"docs/guide.md": "# guide\n"})
	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":["add_owner(/docs/, @org/docs-team)"]}`)
	sumPath2 := filepath.Join(t.TempDir(), "body2.md")
	code2, _, errOut2 := runCLI(t, "sync", "--repo", repo2, "--policy", pol, "--dry-run", "--summary-out", sumPath2)
	if code2 != cli.ExitOK {
		t.Fatalf("dry run: exit %d, want 0\nstderr:\n%s", code2, errOut2)
	}
	want := "- a new CODEOWNERS file WOULD be created (`\"create\": true` in `" + pol + "`)"
	if !checkoutHasLine(syncReadFile(t, sumPath2), want) {
		t.Errorf("preview body has no line\n%s\ngot:\n%s", want, syncReadFile(t, sumPath2))
	}
}

// checkoutHasLine reports whether the document has a line equal to want, byte
// for byte. Whole-line equality is the point: the bullets under test differ
// from each other by a parenthetical, which a substring check would not see.
func checkoutHasLine(doc, want string) bool {
	for _, line := range strings.Split(doc, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// FINDING 10 — the sync record did not name the policy that produced it.
//
// `check --format json` emits `"policy":"<path>"`; the record a fleet
// aggregates did not, so a directory of `--out records/$repo.json` files could
// not be attributed to the artifact that produced them. R-20's whole claim is
// that the policy file is the complete statement of what ran; a record that
// cannot name it makes that claim uncheckable a week later, when two waves have
// landed and results.jsonl is all anyone kept.

// SPEC R-20/R-24: a record from a `--policy` run carries `policy`, spelled as
// the path the operator passed and keyed exactly as `check` keys it.
//
// Asserted through a raw JSON map rather than through cli.SyncRecord: every
// other test in this package unmarshals into the struct, so the struct tag sits
// on both sides of the assertion and a rename would keep them all green.
func TestRecord_PolicyRunNamesThePolicy(t *testing.T) {
	repo := initRepo(t, map[string]string{"CODEOWNERS": "* @org/all\n", "docs/guide.md": "# guide\n"})
	pol := syncWritePolicy(t, `{"version":1,"ops":["add_owner(/docs/, @org/docs-team)"]}`)
	outPath := filepath.Join(t.TempDir(), "rec.json")

	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json", "--out", outPath)
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr:\n%s", code, errOut)
	}
	for _, doc := range []string{out, syncReadFile(t, outPath)} {
		var m map[string]any
		if err := json.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("record is not valid JSON: %v\n%s", err, doc)
		}
		if got, _ := m["policy"].(string); got != pol {
			t.Errorf("record policy = %v, want %q — a fleet aggregating records cannot otherwise tell which artifact produced them", m["policy"], pol)
		}
	}
}

// SPEC R-20/R-24: an `--op` run emits no `policy` key at all.
//
// Absence has to mean "no policy file was involved", the same way `check`'s
// omitempty spelling does. An empty string would make
// `jq 'group_by(.policy)'` bucket every ad-hoc run under "" alongside a policy
// that genuinely has no name.
func TestRecord_OpRunOmitsThePolicyKey(t *testing.T) {
	repo := initRepo(t, map[string]string{"CODEOWNERS": "* @org/all\n", "docs/guide.md": "# guide\n"})
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/docs/, @org/docs-team)", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr:\n%s", code, errOut)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("record is not valid JSON: %v\n%s", err, out)
	}
	if raw, ok := m["policy"]; ok {
		t.Errorf("an --op run emitted policy=%s; absence is what tells a fleet no artifact was involved", raw)
	}
}

// SPEC R-20/R-24: a REFUSED record names the policy too.
//
// The refused rows are the ones an operator re-reads afterwards, and "which
// wave was this?" is the first question asked of a `needs-human` pile. A field
// present only on success would be missing from exactly the records anyone
// goes back to.
func TestRecord_RefusedRecordStillNamesThePolicy(t *testing.T) {
	repo := initRepo(t, map[string]string{"CODEOWNERS": "* @org/all\n", "docs/guide.md": "# guide\n"})
	// Zero paths allowed, so the run refuses on R-25 with the file untouched.
	pol := syncWritePolicy(t, `{"version":1,"max_paths_changed":0,"ops":["add_owner(/docs/, @org/docs-team)"]}`)
	code, out, _ := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("exit %d, want %d\n%s", code, cli.ExitRefused, out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("record is not valid JSON: %v\n%s", err, out)
	}
	if got, _ := m["policy"].(string); got != pol {
		t.Errorf("refused record policy = %v, want %q", m["policy"], pol)
	}
}
