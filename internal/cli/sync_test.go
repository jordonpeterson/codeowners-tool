package cli_test

import (
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// syncWritePolicy writes a policy file to its own temp dir — deliberately
// OUTSIDE any repo, because a policy that lives in the clone is one
// `git add -A` from being committed by the fleet script.
func syncWritePolicy(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// syncDecodeRecord parses one sync record and asserts it is exactly ONE JSON
// object with nothing trailing. `jq -s` over results.jsonl aggregates a whole
// fleet; a second object or a stray log line on stdout breaks every consumer.
func syncDecodeRecord(t *testing.T, s string) cli.SyncRecord {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var rec cli.SyncRecord
	if err := dec.Decode(&rec); err != nil {
		t.Fatalf("record is not valid JSON: %v\nraw:\n%s", err, s)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("want exactly one JSON object, got trailing %s (err %v)\nraw:\n%s", trailing, err, s)
	}
	return rec
}

func syncReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// syncOpResult finds a per-op result by policy id.
func syncOpResult(t *testing.T, rec cli.SyncRecord, id string) (int, bool) {
	t.Helper()
	for i, r := range rec.Ops {
		if r.ID == id {
			return i, true
		}
	}
	return 0, false
}

// checkDirSnapshot records every file under dir by content, so a test can
// prove a command wrote nothing at all.
func checkDirSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		snap[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// syncFleetPolicy is the worked example from the UX spec: one op that applies
// against the tree, one opportunistic op that skips, one baseline op that is
// declared for files this repo does not have yet.
const syncFleetPolicy = `{
  "version": 1,
  "name": "org baseline ownership",
  "description": "CI owns workflows everywhere; infra owns Terraform where it exists.",
  "ops": [
    { "id": "api", "op": "add_owner(/x/, @org/api-team)" },
    { "id": "tf",  "op": "add_owner(**/*.tf, @org/infra)", "on_zero_match": "skip",
      "note": "Opportunistic — only repos that actually have Terraform." },
    { "id": "ci",  "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare",
      "note": "Baseline; also covers workflows added later." }
  ]
}`

// syncBrokenPolicies is the exit-3 class: every one of these is a property of
// the POLICY, not of any repo, so it fails identically on all 100 and must
// halt the fleet run rather than being recorded and skipped past.
var syncBrokenPolicies = []struct {
	name string
	src  string
}{
	{"unknown top-level field", `{"version":1,"ops":["add_owner(/x/, @b)"],"on_zero_match":"skip"}`},
	{"unknown per-op field", `{"version":1,"ops":[{"op":"add_owner(/x/, @b)","on_zero_mtach":"skip"}]}`},
	{"missing version", `{"ops":["add_owner(/x/, @b)"]}`},
	{"unsupported version", `{"version":2,"ops":["add_owner(/x/, @b)"]}`},
	{"empty ops", `{"version":1,"ops":[]}`},
	{"missing ops", `{"version":1,"name":"n"}`},
	{"bad on_empty enum", `{"version":1,"on_empty":"inhrit","ops":["add_owner(/x/, @b)"]}`},
	{"bad on_zero_match enum", `{"version":1,"ops":[{"op":"add_owner(/x/, @b)","on_zero_match":"maybe"}]}`},
	{"duplicate key", `{"version":1,"on_empty":"error","on_empty":"inherit","ops":["remove_owner(/x/, @a)"]}`},
	{"malformed op string", `{"version":1,"ops":["frob(/x/, @b)"]}`},
	{"on_zero_match on rename_owner", `{"version":1,"ops":[{"op":"rename_owner(@a, @b)","on_zero_match":"skip"}]}`},
	{"declare on remove_owner", `{"version":1,"on_empty":"error","ops":[{"op":"remove_owner(/x/, @a)","on_zero_match":"declare"}]}`},
	{"remove_owner without on_empty", `{"version":1,"ops":["remove_owner(/x/, @a)"]}`},
	{"op is neither string nor object", `{"version":1,"ops":[7]}`},
}

// syncSmallRepo is the fixture most exit-code tests stand in: one owned
// directory, one CODEOWNERS at the root.
func syncSmallRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"CODEOWNERS": "# owners\n/x/ @a\n",
		"x/a.go":     "package x\n",
	})
}

// SPEC R-19: sync is convergent and idempotent. The first run applies and
// exits 0; the identical second run changes nothing and STILL exits 0. The
// collapse of "applied" and "already correct" onto 0 is the entire point —
// under `set -e` at 100 repos, the common outcome must not read as failure.
func TestSync_ConvergesThenIsIdempotent(t *testing.T) {
	repo := syncSmallRepo(t)
	coPath := filepath.Join(repo, "CODEOWNERS")

	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/x/, @b)", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("first sync: want exit 0, got %d\n%s", code, errOut)
	}
	first := syncDecodeRecord(t, out)
	if first.Status != cli.StatusApplied {
		t.Errorf("first sync status = %q, want %q", first.Status, cli.StatusApplied)
	}
	applied := syncReadFile(t, coPath)
	if applied != "# owners\n/x/ @a @b\n" {
		t.Fatalf("CODEOWNERS after first sync = %q", applied)
	}

	code2, out2, errOut2 := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/x/, @b)", "--format", "json")
	if code2 != cli.ExitOK {
		t.Fatalf("second sync: want exit 0 (converged), got %d\n%s", code2, errOut2)
	}
	second := syncDecodeRecord(t, out2)
	if second.Status != cli.StatusUnchanged {
		t.Errorf("second sync status = %q, want %q", second.Status, cli.StatusUnchanged)
	}
	if second.OpsApplied != 0 || second.PathsChanged != 0 {
		t.Errorf("second sync must change nothing: %+v", second)
	}
	if got := syncReadFile(t, coPath); got != applied {
		t.Errorf("second sync rewrote the file: %q → %q", applied, got)
	}
}

// SPEC R-19 (R-17 remap): "already correct" is exit 0 under sync even though
// `plan` calls the same situation a no-op and exits 1. If sync inherited the
// 1, every fleet runner would abort on the most common outcome there is.
func TestSync_AlreadyCorrectIsZeroNotNoOp(t *testing.T) {
	repo := syncSmallRepo(t)
	if code, _, _ := runCLI(t, "plan", "--repo", repo, "--op", "add_owner(/x/, @a)"); code != cli.ExitNoOp {
		t.Fatalf("precondition: plan must still report exit 1 for a no-op, got %d", code)
	}
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/x/, @a)", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("no-op sync: want exit 0, got %d\n%s", code, errOut)
	}
	if rec := syncDecodeRecord(t, out); rec.Status != cli.StatusUnchanged {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusUnchanged)
	}
}

// SPEC R-19 (exit contract): a refusal is exit 2 — this repo's file has an
// awkward shape and needs a human. The fleet script records it and keeps
// going; it must never be confused with a broken policy.
func TestSync_RefusalExitsTwo(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @default\n/x/ @solo\n",
		"x/a.go":     "package x\n",
		"y/b.go":     "package y\n",
	})
	// R-6: removing the only owner under --on-empty=error is a refusal.
	code, _, _ := runCLI(t, "sync", "--repo", repo, "--op", "remove_owner(/x/, @solo)", "--on-empty", "error")
	if code != cli.ExitRefused {
		t.Errorf("on-empty=error refusal: want exit 2, got %d", code)
	}

	// INV-1/INV-2 unprovable: no sound narrowing pattern is derivable.
	repo2 := initRepo(t, map[string]string{
		"CODEOWNERS":    "*.md @docs\n",
		"a/x/doc.md":    "x\n",
		"a/x/code.go":   "package x\n",
		"other/doc.md":  "x\n",
		"other/keep.go": "package other\n",
	})
	if code, _, _ := runCLI(t, "sync", "--repo", repo2, "--op", "add_owner(x/, @b)"); code != cli.ExitRefused {
		t.Errorf("inexpressible intent: want exit 2, got %d", code)
	}
}

// SPEC R-19/R-21: a scope matching zero tracked files under the default
// `require` is exit 2, NOT 3. `plan` calls it invalid input; sync remaps it
// because whether a path exists is the most repo-specific fact there is.
// Getting this backwards halts a 100-repo run on repo 3.
func TestSync_ZeroMatchUnderRequireExitsTwo(t *testing.T) {
	repo := syncSmallRepo(t)
	if code, _, _ := runCLI(t, "plan", "--repo", repo, "--op", "add_owner(/ghost/, @b)"); code != cli.ExitInvalid {
		t.Fatalf("precondition: plan must still report exit 3 for zero-match, got %d", code)
	}
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/ghost/, @b)")
	if code != cli.ExitRefused {
		t.Errorf("zero-match under require: want exit 2 (this repo), got %d\n%s", code, errOut)
	}
}

// SPEC R-23: no CODEOWNERS and no --create is exit 2, not 3. --create is off
// by default, so treating "this repo has no file" as a policy error halted
// revision 1's fleet run at roughly repo 3.
func TestSync_NoCodeownersWithoutCreateExitsTwo(t *testing.T) {
	repo := initRepo(t, map[string]string{"svc/api/main.go": "package main\n"})
	if code, _, _ := runCLI(t, "plan", "--repo", repo, "--op", "add_owner(/svc/api/, @org/api)"); code != cli.ExitInvalid {
		t.Fatalf("precondition: plan must still report exit 3 for a missing file, got %d", code)
	}
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/svc/api/, @org/api)", "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("missing CODEOWNERS without --create: want exit 2, got %d\n%s", code, errOut)
	}
	if rec := syncDecodeRecord(t, out); rec.Created {
		t.Error("created must be false when nothing was created")
	}
	if _, err := os.Stat(filepath.Join(repo, ".github", "CODEOWNERS")); err == nil {
		t.Error("--create is off by default: no file may be written")
	}
}

// SPEC R-19/R-20: a broken policy is exit 3 — it fails identically on every
// repo, so the fleet script halts instead of grinding through 100 clones.
func TestSync_BrokenPolicyExitsThree(t *testing.T) {
	repo := syncSmallRepo(t)
	for _, tc := range syncBrokenPolicies {
		t.Run(tc.name, func(t *testing.T) {
			p := syncWritePolicy(t, tc.src)
			code, _, _ := runCLI(t, "sync", "--repo", repo, "--policy", p)
			if code != cli.ExitInvalid {
				t.Errorf("%s: want exit 3 (halt the fleet), got %d", tc.name, code)
			}
		})
	}
	// A malformed --op string is the same class.
	if code, _, _ := runCLI(t, "sync", "--repo", repo, "--op", "frob(/x/, @b)"); code != cli.ExitInvalid {
		t.Error("malformed --op: want exit 3")
	}
	// So is a policy file that is not there at all.
	if code, _, _ := runCLI(t, "sync", "--repo", repo, "--policy", filepath.Join(t.TempDir(), "nope.json")); code != cli.ExitInvalid {
		t.Error("missing policy file: want exit 3")
	}
}

// SPEC R-20: --op and --policy are mutually exclusive, and --policy twice is
// an error, never a silent last-wins. Silent last-wins means the artifact in
// git is not the policy that ran.
func TestSync_OpAndPolicyMisuseExitsThree(t *testing.T) {
	repo := syncSmallRepo(t)
	p1 := syncWritePolicy(t, `{"version":1,"ops":["add_owner(/x/, @b)"]}`)
	p2 := syncWritePolicy(t, `{"version":1,"ops":["add_owner(/x/, @c)"]}`)

	// A slice, not a map, and one t.Run per case: a map ranges in random order
	// and, without subtests, all three cases report under this test's single
	// name. A failure then names a case that may not be the one that failed on
	// the next run, and `go test -run` cannot re-run just the broken one.
	cases := []struct {
		name string
		args []string
	}{
		{"both", []string{"sync", "--repo", repo, "--op", "add_owner(/x/, @b)", "--policy", p1}},
		{"neither", []string{"sync", "--repo", repo}},
		{"policy twice", []string{"sync", "--repo", repo, "--policy", p1, "--policy", p2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, _ := runCLI(t, c.args...)
			if code != cli.ExitInvalid {
				t.Errorf("%s: want exit 3, got %d", c.name, code)
			}
		})
	}
	// Last-wins would have silently applied p2 and left the file changed.
	if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != "# owners\n/x/ @a\n" {
		t.Errorf("a rejected invocation must write nothing, file = %q", got)
	}
}

// SPEC R-20: --on-empty is allowed only with --op. With --policy it is a hard
// error, because a flag that silently beats a policy field means the file in
// git is not the complete statement of what ran — and `check` would then
// validate something other than what executes.
func TestSync_OnEmptyWithPolicyExitsThree(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @default\n/x/ @solo\n",
		"x/a.go":     "package x\n",
		"y/b.go":     "package y\n",
	})
	p := syncWritePolicy(t, `{"version":1,"on_empty":"inherit","ops":["remove_owner(/x/, @solo)"]}`)
	if code, _, _ := runCLI(t, "sync", "--repo", repo, "--policy", p, "--on-empty", "unowned"); code != cli.ExitInvalid {
		t.Error("--on-empty with --policy: want exit 3")
	}
	// The same flag with --op is exactly how R-6 is expressed there.
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", "remove_owner(/x/, @solo)", "--on-empty", "inherit")
	if code != cli.ExitOK {
		t.Errorf("--on-empty with --op: want exit 0, got %d\n%s", code, errOut)
	}
}

// SPEC R-19: sync returns ONLY 0, 2, 3. Revision 1's "3+ means halt" was
// unsafe three separate ways; reserving nothing is the fix. 1 (no-op), 4
// (findings), 5 (inconclusive) and 6 (rolled back) must all be unreachable.
func TestSync_NeverReturnsOtherExitCodes(t *testing.T) {
	ok := syncSmallRepo(t)
	empty := initRepo(t, map[string]string{"svc/api/main.go": "package main\n"})
	removal := initRepo(t, map[string]string{
		"CODEOWNERS": "* @default\n/x/ @solo\n",
		"x/a.go":     "package x\n",
		"y/b.go":     "package y\n",
	})
	broken := syncWritePolicy(t, `{"version":1,"ops":[{"op":"add_owner(/x/, @b)","on_zero_mtach":"skip"}]}`)
	fleet := syncWritePolicy(t, syncFleetPolicy)

	invocations := [][]string{
		{"sync", "--repo", ok, "--op", "add_owner(/x/, @b)"},
		{"sync", "--repo", ok, "--op", "add_owner(/x/, @a)"},
		{"sync", "--repo", ok, "--op", "add_owner(/ghost/, @b)"},
		{"sync", "--repo", ok, "--policy", fleet},
		{"sync", "--repo", ok, "--policy", broken},
		{"sync", "--repo", empty, "--op", "add_owner(/svc/api/, @org/api)"},
		{"sync", "--repo", empty, "--op", "add_owner(/svc/api/, @org/api)", "--create"},
		{"sync", "--repo", removal, "--op", "remove_owner(/x/, @solo)", "--on-empty", "error"},
		{"sync", "--repo", removal, "--op", "remove_owner(/x/, @solo)"},
		{"sync", "--repo", filepath.Join(t.TempDir(), "not-a-repo"), "--op", "add_owner(/x/, @b)"},
		{"sync", "--repo", ok, "--op", "add_owner(/x/, @b)", "--format", "json", "--dry-run"},
	}
	for _, args := range invocations {
		code, _, _ := runCLI(t, args...)
		switch code {
		case cli.ExitOK, cli.ExitRefused, cli.ExitInvalid:
		default:
			t.Errorf("sync %v: exit %d — sync may return only 0, 2, 3", args[1:], code)
		}
	}
}

// SPEC R-19: `sync -h` and `sync --help` exit 0. Under a fleet contract a
// help request reading as "the policy is broken, halt" is wrong.
func TestSync_HelpExitsZero(t *testing.T) {
	for _, flagSpelling := range []string{"-h", "--help"} {
		if code, _, _ := runCLI(t, "sync", flagSpelling); code != cli.ExitOK {
			t.Errorf("sync %s: want exit 0, got %d", flagSpelling, code)
		}
		if code, _, _ := runCLI(t, "check", flagSpelling); code != cli.ExitOK {
			t.Errorf("check %s: want exit 0, got %d", flagSpelling, code)
		}
	}
}

// SPEC R-23: --create writes .github/CODEOWNERS when the repo has none, and
// reports created:true so the fleet aggregation can tell a new file from an
// edit.
func TestSync_CreateWritesWhenAbsent(t *testing.T) {
	repo := initRepo(t, map[string]string{"svc/api/main.go": "package main\n"})
	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/svc/api/, @org/api)", "--create", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("sync --create: want exit 0, got %d\n%s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if !rec.Created {
		t.Error("created must be true when the file was written from nothing")
	}
	got := syncReadFile(t, filepath.Join(repo, ".github", "CODEOWNERS"))
	if !strings.Contains(got, "@org/api") {
		t.Errorf(".github/CODEOWNERS = %q, want the new owner", got)
	}
}

// SPEC R-23: --create NEVER overwrites. Creating a file is the one action
// with no prior artifact to prove INV-2 against; clobbering an existing one
// would destroy ownership the tool exists to protect.
func TestSync_CreateNeverOverwrites(t *testing.T) {
	repo := syncSmallRepo(t)
	coPath := filepath.Join(repo, "CODEOWNERS")
	before := syncReadFile(t, coPath)

	// An op that is already satisfied: the run converges without touching
	// a byte, and --create must not manufacture a second file.
	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/x/, @a)", "--create", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\n%s", code, errOut)
	}
	if got := syncReadFile(t, coPath); got != before {
		t.Errorf("existing CODEOWNERS was rewritten: %q → %q", before, got)
	}
	if _, err := os.Stat(filepath.Join(repo, ".github", "CODEOWNERS")); err == nil {
		t.Error("--create must not create a second file when one already governs")
	}
	if rec := syncDecodeRecord(t, out); rec.Created {
		t.Error("created must be false when a file already existed")
	}
}

// SPEC R-23: --create honors --file when given, and hard-errors with a
// non-HEAD --branch — there is nothing to create a file "at" on a ref you are
// not standing on, and silently writing to the working tree instead would be
// the wrong file in the wrong place.
func TestSync_CreateHonorsFileAndRejectsNonHeadBranch(t *testing.T) {
	repo := initRepo(t, map[string]string{"svc/api/main.go": "package main\n"})
	code, _, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/svc/api/, @org/api)", "--create", "--file", "docs/CODEOWNERS")
	if code != cli.ExitOK {
		t.Fatalf("--create --file: want exit 0, got %d\n%s", code, errOut)
	}
	if got := syncReadFile(t, filepath.Join(repo, "docs", "CODEOWNERS")); !strings.Contains(got, "@org/api") {
		t.Errorf("docs/CODEOWNERS = %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, ".github", "CODEOWNERS")); err == nil {
		t.Error("--file was given: the default location must not also be written")
	}

	// A GENUINELY non-HEAD branch. initRepo runs `git init -b main`, so the
	// earlier revision of this test passed "main" — the branch the repo is
	// already standing on — and therefore asserted that naming your own
	// checked-out branch is an error. That is an ordinary fleet invocation
	// (`--branch main` in a script), and rejecting it halted the whole rollout
	// at repo 0. The intent below was always right; the fixture did not
	// exercise it. Refusal is exit 2, not 3: whether a ref is the one checked
	// out is a fact about this repo, not about the arguments.
	repo2 := initRepo(t, map[string]string{"svc/api/main.go": "package main\n"})
	syncGit(t, repo2, "branch", "other")
	syncGit(t, repo2, "commit", "-q", "--allow-empty", "-m", "move main ahead of other")
	if code, _, _ := runCLI(t, "sync", "--repo", repo2,
		"--op", "add_owner(/svc/api/, @org/api)", "--create", "--branch", "other"); code != cli.ExitRefused {
		t.Errorf("--create with a genuinely non-HEAD --branch: want exit 2, got %d", code)
	}
	// Naming the checked-out branch explicitly must still work.
	repo3 := initRepo(t, map[string]string{"svc/api/main.go": "package main\n"})
	if code, _, errOut := runCLI(t, "sync", "--repo", repo3,
		"--op", "add_owner(/svc/api/, @org/api)", "--create", "--branch", "main"); code != cli.ExitOK {
		t.Errorf("--branch main on a repo standing on main: want exit 0, got %d\n%s", code, errOut)
	}
}

// syncGit runs a git command in dir, for tests that need a second branch.
func syncGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// SPEC R-19/R-24: --dry-run makes NO change to CODEOWNERS but STILL emits
// --out and --summary-out. That combination is what makes a fleet preview
// useful — at 100 repos, the aggregated preview is the only review possible.
func TestSync_DryRunWritesNoCodeownersButStillEmits(t *testing.T) {
	repo := syncSmallRepo(t)
	coPath := filepath.Join(repo, "CODEOWNERS")
	before := syncReadFile(t, coPath)
	outDir := t.TempDir()
	recPath := filepath.Join(outDir, "record.json")
	sumPath := filepath.Join(outDir, "body.md")

	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/x/, @b)",
		"--dry-run", "--out", recPath, "--summary-out", sumPath)
	if code != cli.ExitOK {
		t.Fatalf("dry run: want exit 0, got %d\n%s", code, errOut)
	}
	if got := syncReadFile(t, coPath); got != before {
		t.Fatalf("--dry-run wrote to CODEOWNERS: %q → %q", before, got)
	}
	rec := syncDecodeRecord(t, syncReadFile(t, recPath))
	if rec.Status != cli.StatusApplied {
		t.Errorf("dry-run status = %q, want %q — the preview must show what WOULD happen", rec.Status, cli.StatusApplied)
	}
	if len(rec.Changes) == 0 {
		t.Error("a preview with no changes tells the operator nothing")
	}
	if s := syncReadFile(t, sumPath); strings.TrimSpace(s) == "" {
		t.Error("--summary-out must still be written under --dry-run")
	}
}

// SPEC R-24: under --format json, stdout is data and stderr is logs. A log
// line on stdout breaks `jq -s` over results.jsonl for the whole fleet.
func TestSync_FormatJSONSeparatesDataFromLogs(t *testing.T) {
	repo := syncSmallRepo(t)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/x/, @b)", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\n%s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Repo == "" {
		t.Error("record must name the repo — a fleet row without it is unattributable")
	}
	if strings.Contains(errOut, `"status"`) {
		t.Errorf("the record must not also be written to stderr:\n%s", errOut)
	}

	// text is the default, and it is not JSON.
	repo2 := syncSmallRepo(t)
	code2, out2, _ := runCLI(t, "sync", "--repo", repo2, "--op", "add_owner(/x/, @b)")
	if code2 != cli.ExitOK {
		t.Fatalf("default format: want exit 0, got %d", code2)
	}
	if strings.HasPrefix(strings.TrimSpace(out2), "{") {
		t.Errorf("--format text is the default, got JSON:\n%s", out2)
	}

	// An unrecognized format is a policy-class error, not a silent fallback.
	if code, _, _ := runCLI(t, "sync", "--repo", syncSmallRepo(t), "--op", "add_owner(/x/, @b)", "--format", "yaml"); code != cli.ExitInvalid {
		t.Error("--format yaml: want exit 3, never a silent fallback to text")
	}
}

// SPEC R-24: the two flags are independent. `--out FILE` ALWAYS writes the JSON
// record to FILE, whatever `--format` says; `--format` governs stdout and only
// stdout.
//
// The alternative — `--out` emitting whatever `--format` names — quietly
// destroys the artifact the flag exists for. `sync --out records/$repo.json`
// with the default text format would leave a directory of human prose, and the
// `jq -s` aggregation the README builds over it fails on the first file with a
// parse error, after the whole rollout has already run and the CODEOWNERS
// writes are done. Making it depend on a flag nobody passed is worse than
// making it wrong: it works for the operator who happened to type `--format
// json` and fails for the one who did not.
//
// The converse guarantee matters as much: `--out` must not go on to SUPPRESS
// stdout. Someone piping `sync --format json ... | tee` while also archiving
// with `--out` gets both, byte-identical, and the fleet script's
// `>> results.jsonl` keeps working when a per-repo `--out` is added to it.
func TestSync_OutWritesRecordToFile(t *testing.T) {
	// Default format is text: the FILE is still the JSON record, stdout is
	// still the human render.
	repo := syncSmallRepo(t)
	recPath := filepath.Join(t.TempDir(), "record.json")
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/x/, @b)", "--out", recPath)
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\n%s", code, errOut)
	}
	rec := syncDecodeRecord(t, syncReadFile(t, recPath))
	if rec.Status != cli.StatusApplied {
		t.Errorf("record status = %q, want %q", rec.Status, cli.StatusApplied)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("--format defaults to text: stdout must carry the human render, not the record:\n%s", out)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("--out must not silence stdout: --format governs stdout, --out governs the file, and neither implies the other")
	}

	// --format json --out FILE: both sinks, same record, no interaction.
	repo2 := syncSmallRepo(t)
	recPath2 := filepath.Join(t.TempDir(), "record2.json")
	code2, out2, errOut2 := runCLI(t, "sync", "--repo", repo2, "--op", "add_owner(/x/, @b)",
		"--format", "json", "--out", recPath2)
	if code2 != cli.ExitOK {
		t.Fatalf("--format json --out: want exit 0, got %d\n%s", code2, errOut2)
	}
	fromFile := syncDecodeRecord(t, syncReadFile(t, recPath2))
	fromStdout := syncDecodeRecord(t, out2)
	if fromFile.Status != cli.StatusApplied {
		t.Errorf("--format json --out: file record status = %q, want %q", fromFile.Status, cli.StatusApplied)
	}
	if fromStdout.Status != fromFile.Status || fromStdout.Repo != fromFile.Repo {
		t.Errorf("--format json --out: stdout and file disagree:\nstdout %+v\nfile   %+v", fromStdout, fromFile)
	}
	if strings.TrimSpace(out2) != strings.TrimSpace(syncReadFile(t, recPath2)) {
		t.Errorf("--format json --out: the two sinks must be byte-identical\nstdout: %q\nfile:   %q",
			out2, syncReadFile(t, recPath2))
	}
}

// SPEC R-24/INV-6: --summary-out renders markdown for a PR body. It must name
// the policy (that is why `name` and `description` exist) and call out every
// op proven only structurally, so a reviewer finds the weakened INV-1 cases
// without reading the diff.
func TestSync_SummaryOutNamesPolicyAndStructuralOps(t *testing.T) {
	repo := syncSmallRepo(t)
	p := syncWritePolicy(t, syncFleetPolicy)
	sumPath := filepath.Join(t.TempDir(), "body.md")

	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", p, "--summary-out", sumPath)
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\n%s", code, errOut)
	}
	summary := syncReadFile(t, sumPath)
	for _, want := range []string{"org baseline ownership", "CI owns workflows everywhere"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary must carry the policy %q:\n%s", want, summary)
		}
	}
	// The `ci` op matched nothing tracked and was proven structurally
	// (INV-6). A reviewer who cannot see that cannot review it.
	if !strings.Contains(summary, "ci") {
		t.Errorf("summary must name the structurally-proven op by id:\n%s", summary)
	}
	if !strings.Contains(strings.ToLower(summary), "structural") {
		t.Errorf("summary must call out structural proof (INV-6):\n%s", summary)
	}
}

// SPEC R-24: the record's per-op results carry id/op/status/proven, and the
// counts agree with the array. "ops_skipped: 1" alone cannot answer the
// question that motivates `skip` — WHICH repos lack Terraform.
func TestSync_JSONRecordPerOpResults(t *testing.T) {
	repo := syncSmallRepo(t)
	p := syncWritePolicy(t, syncFleetPolicy)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", p, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\n%s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusApplied {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusApplied)
	}
	if len(rec.Ops) != 3 {
		t.Fatalf("want one result per policy op, got %d: %+v", len(rec.Ops), rec.Ops)
	}
	var applied, skipped int
	for _, r := range rec.Ops {
		if r.ID == "" || r.Op == "" || r.Status == "" {
			t.Errorf("per-op result missing id/op/status: %+v", r)
		}
		switch r.Status {
		case "applied":
			applied++
			if r.Proven != "tree" && r.Proven != "structural" {
				t.Errorf("applied op %q proven = %q, want tree|structural", r.ID, r.Proven)
			}
		case "skipped":
			skipped++
			if r.Reason == "" {
				t.Errorf("skipped op %q must say why", r.ID)
			}
		case "unchanged":
		default:
			t.Errorf("op %q status = %q, want applied|skipped|unchanged", r.ID, r.Status)
		}
	}
	if rec.OpsApplied != applied || rec.OpsSkipped != skipped {
		t.Errorf("counts disagree with the array: ops_applied=%d ops_skipped=%d vs %d/%d",
			rec.OpsApplied, rec.OpsSkipped, applied, skipped)
	}
	i, ok := syncOpResult(t, rec, "tf")
	if !ok {
		t.Fatalf("no result for the `tf` op: %+v", rec.Ops)
	}
	if rec.Ops[i].Status != "skipped" {
		t.Errorf("tf op (no Terraform in this repo, on_zero_match=skip) = %q, want skipped", rec.Ops[i].Status)
	}
	j, ok := syncOpResult(t, rec, "ci")
	if !ok {
		t.Fatalf("no result for the `ci` op: %+v", rec.Ops)
	}
	if rec.Ops[j].Proven != "structural" {
		t.Errorf("declare op with zero matches must be proven %q (INV-6), got %q", "structural", rec.Ops[j].Proven)
	}
	if rec.PathsChanged < 1 {
		t.Errorf("paths_changed = %d, want the /x/ paths the api op took", rec.PathsChanged)
	}
	if rec.Created {
		t.Error("created must be false — the repo already had a CODEOWNERS")
	}
	if len(rec.Warnings) != 0 {
		t.Errorf("clean run must carry no warnings, got %v", rec.Warnings)
	}
}

// SPEC R-24 (D1): an already-correct repo STILL reports one per-op result per
// policy op, each `unchanged`.
//
// This pins the no-op path specifically, and it is a real hole today:
// plan.Build returns `nil, &NoOpError{}` when the file already says what the
// policy wants, so a caller that renders the record from the returned *Plan
// has nothing to render and emits `"ops": []`. `unchanged` is the MODAL
// outcome of a mature fleet — most repos are already correct most nights — so
// the empty case is the one an operator sees on 90 of 100 rows. Without it the
// record cannot answer "is op `tf` actually in place here, or did the policy
// never mention this repo?", and the whole per-op array degrades to detail that
// appears only when something changed, which is exactly when you least need it.
//
// Three fleet tests (fleet_test.go's "per-op results" and the idempotence
// file's repeat passes) read per-op detail out of records produced by this
// path; none of them pins the path itself.
func TestSync_AlreadyCorrectStillCarriesPerOpResults(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "# owners\n/x/ @a @b\n/y/ @c\n",
		"x/a.go":     "package x\n",
		"y/b.go":     "package y\n",
	})
	p := syncWritePolicy(t, `{
	  "version": 1,
	  "ops": [
	    {"id":"x","op":"add_owner(/x/, @b)"},
	    {"id":"y","op":"add_owner(/y/, @c)"}
	  ]
	}`)
	before := syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))

	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", p, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("already-correct repo: want exit 0, got %d\n%s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusUnchanged {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusUnchanged)
	}
	if len(rec.Ops) != 2 {
		t.Fatalf("no-op run carried %d per-op results, want one per policy op (2): %+v — the no-op path must report as fully as the applying one", len(rec.Ops), rec.Ops)
	}
	for _, r := range rec.Ops {
		if r.Status != "unchanged" {
			t.Errorf("op %q: status %q, want unchanged", r.ID, r.Status)
		}
		if r.ID == "" {
			t.Errorf("op result lost its policy id: %+v — results are keyed by id", r)
		}
		if r.Op == "" {
			t.Errorf("op result lost its op text: %+v (D7: Op is the raw op string)", r)
		}
	}
	if _, ok := syncOpResult(t, rec, "x"); !ok {
		t.Errorf("no result for op `x`: %+v", rec.Ops)
	}
	if _, ok := syncOpResult(t, rec, "y"); !ok {
		t.Errorf("no result for op `y`: %+v", rec.Ops)
	}
	if rec.OpsApplied != 0 {
		t.Errorf("ops_applied = %d, want 0 — nothing was applied", rec.OpsApplied)
	}
	if rec.OpsSkipped != 0 {
		t.Errorf("ops_skipped = %d, want 0 — `unchanged` is not `skipped`", rec.OpsSkipped)
	}
	if rec.PathsChanged != 0 {
		t.Errorf("paths_changed = %d, want 0", rec.PathsChanged)
	}
	if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != before {
		t.Errorf("an already-correct repo must not be rewritten: %q → %q", before, got)
	}
}

// SPEC R-20 (D3): two ops sharing one `id` is exit 3.
//
// Results are KEYED by id — syncOpResult here, fleetOpResult in fleet_test.go,
// opResultByID in the planner's tests, and every `jq` recipe an operator writes
// over results.jsonl. Every one of them returns the FIRST match. So a policy
// with a duplicated id does not fail: it silently reports the first op's
// outcome as if it were the second's, across the whole fleet, and the operator
// reading "tf: applied" cannot tell that the other `tf` refused. Nothing about
// this depends on any repo, so it belongs in the class that halts at repo 0 —
// and `check` must catch it before the first clone.
func TestSync_DuplicateOpIDsExitThree(t *testing.T) {
	repo := syncSmallRepo(t)
	dup := syncWritePolicy(t, `{
	  "version": 1,
	  "ops": [
	    {"id":"tf","op":"add_owner(/x/, @b)"},
	    {"id":"tf","op":"add_owner(/x/, @c)"}
	  ]
	}`)
	before := syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))

	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", dup, "--format", "json")
	if code != cli.ExitInvalid {
		t.Errorf("duplicate op id: want exit 3, got %d — a shadowed id makes every per-op result unreliable\nstderr: %s", code, errOut)
	}
	if !strings.Contains(errOut, "tf") {
		t.Errorf("the error must quote the duplicated id, or nobody can find it in a 40-op policy:\n%s", errOut)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("exit 3 emits no record (D8), got: %q", out)
	}
	if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != before {
		t.Errorf("a rejected policy writes nothing, file = %q", got)
	}
	// `check` is the first line of the fleet script; this must never get past it.
	if code, _, _ := runCLI(t, "check", "--policy", dup); code != cli.ExitInvalid {
		t.Errorf("check on a duplicate-id policy: want exit 3, got %d", code)
	}
}

// SPEC R-24: `--repo` pointing at a directory that is not a git repository is a
// RECORDED per-repo failure — exit 2, one record, status "error".
//
// This is the only producer of cli.StatusError, and it is a different animal
// from "refused": refused means the tool understood this repo and declined to
// touch it, error means it never got that far. Both need a human, so both are
// exit 2 and both must appear in results.jsonl; grouping on .status is how an
// operator separates "12 repos have awkward CODEOWNERS files" from "12 clones
// failed and were never actually synced". It must NOT be exit 3: a failed or
// half-finished clone is the most repo-specific fact there is, and halting the
// whole rollout on it strands the other 99 — the same mistake revision 1 made
// with zero-match scopes.
func TestSync_NonRepoDirectoryIsAnErrorRecord(t *testing.T) {
	dir := t.TempDir() // exists, is readable, has no .git
	code, out, errOut := runCLI(t, "sync", "--repo", dir, "--op", "add_owner(/x/, @b)", "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("--repo is not a git repo: exit %d, want 2 — exit 3 would halt the fleet for one bad clone\nstderr: %s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusError {
		t.Errorf("status = %q, want %q — %q would claim the tool read this repo and declined, which it did not",
			rec.Status, cli.StatusError, cli.StatusRefused)
	}
	if strings.TrimSpace(rec.Error) == "" {
		t.Error("an error record must carry the reason text, or the row says only that something went wrong")
	}
	if rec.Repo != dir {
		t.Errorf("record .repo = %q, want the --repo argument %q verbatim (D6)", rec.Repo, dir)
	}
	if rec.Created {
		t.Error("created must be false: nothing was written")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("a repo that could not be read must not be written to: %v (err %v)", entries, err)
	}
}

// SPEC R-24: `skipped` is a DISTINCT status, used when at least one op
// skipped and none applied. Without it a policy with one typo'd path prefix
// skips everywhere and reports 100 × `unchanged` — the operator groups on
// .status, reads "already correct", and ships a no-op rollout.
func TestSync_StatusSkippedWhenNothingApplied(t *testing.T) {
	repo := syncSmallRepo(t)
	p := syncWritePolicy(t, `{
	  "version": 1,
	  "ops": [
	    {"id":"tf","op":"add_owner(**/*.tf, @org/infra)","on_zero_match":"skip"},
	    {"id":"typo","op":"add_owner(/servcies/api/, @org/api)","on_zero_match":"skip"}
	  ]
	}`)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", p, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("an all-skipped run is graceful: want exit 0, got %d\n%s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusSkipped {
		t.Errorf("status = %q, want %q — %q here is how a no-op rollout ships unnoticed",
			rec.Status, cli.StatusSkipped, cli.StatusUnchanged)
	}
	if rec.OpsApplied != 0 || rec.OpsSkipped != 2 {
		t.Errorf("ops_applied=%d ops_skipped=%d, want 0/2", rec.OpsApplied, rec.OpsSkipped)
	}
	if rec.PathsChanged != 0 {
		t.Errorf("paths_changed = %d, want 0", rec.PathsChanged)
	}
	if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != "# owners\n/x/ @a\n" {
		t.Errorf("an all-skipped run must not touch the file, got %q", got)
	}
}

// SPEC R-24: a refusal still produces a parseable record carrying the reason.
// The fleet script appends every run to results.jsonl; a refused repo that
// emits nothing (or emits prose) is a hole in the aggregation exactly where
// the operator needs to look.
func TestSync_RefusalRecordCarriesError(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @default\n/x/ @solo\n",
		"x/a.go":     "package x\n",
		"y/b.go":     "package y\n",
	})
	code, out, _ := runCLI(t, "sync", "--repo", repo,
		"--op", "remove_owner(/x/, @solo)", "--on-empty", "error", "--format", "json")
	if code != cli.ExitRefused {
		t.Fatalf("want exit 2, got %d", code)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusRefused {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusRefused)
	}
	if strings.TrimSpace(rec.Error) == "" {
		t.Error("a refusal record must carry the reason text")
	}
	if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != "* @default\n/x/ @solo\n" {
		t.Errorf("a refusal writes nothing, file = %q", got)
	}
}

// SPEC R-22: `check` exits 0 on a valid policy — never 1. It is the first
// line of every fleet script under `set -e`; a clean policy returning the
// no-op code would abort the run before the loop starts.
func TestCheck_ValidPolicyExitsZeroNeverOne(t *testing.T) {
	p := syncWritePolicy(t, syncFleetPolicy)
	code, out, errOut := runCLI(t, "check", "--policy", p)
	if code != cli.ExitOK {
		t.Fatalf("valid policy: want exit 0, got %d\n%s%s", code, out, errOut)
	}
	if code == cli.ExitNoOp {
		t.Fatal("check must never return exit 1")
	}
	code, out, errOut = runCLI(t, "check", "--policy", p, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("valid policy --format json: want exit 0, got %d\n%s", code, errOut)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Errorf("check --format json stdout is not one JSON object: %v\n%s", err, out)
	}
}

// SPEC R-22: `check` exits 3 on every member of the exit-3 class, and 3 only.
// Its definition is exactly that class: the failures that will repeat
// identically on all 100 repos.
func TestCheck_BrokenPolicyExitsThree(t *testing.T) {
	for _, tc := range syncBrokenPolicies {
		t.Run(tc.name, func(t *testing.T) {
			p := syncWritePolicy(t, tc.src)
			code, _, _ := runCLI(t, "check", "--policy", p)
			if code != cli.ExitInvalid {
				t.Errorf("%s: want exit 3, got %d", tc.name, code)
			}
		})
	}
}

// SPEC R-22: `check --op` syntax-checks an op string too, so the escalation
// path has no gap — the user who has not yet written a policy file can still
// validate before touching 100 repos.
func TestCheck_OpStringIsSyntaxChecked(t *testing.T) {
	if code, _, _ := runCLI(t, "check", "--op", "add_owner(/x/, @a)"); code != cli.ExitOK {
		t.Error("valid --op: want exit 0")
	}
	if code, _, _ := runCLI(t, "check", "--op", "frob(/x/, @b)"); code != cli.ExitInvalid {
		t.Error("malformed --op: want exit 3")
	}
	if code, _, _ := runCLI(t, "check", "--op", "rename_owner(@a, @a)"); code != cli.ExitInvalid {
		t.Error("rename_owner to itself: want exit 3")
	}
	// Same mutual exclusion as sync, and for the same reason.
	p := syncWritePolicy(t, `{"version":1,"ops":["add_owner(/x/, @b)"]}`)
	if code, _, _ := runCLI(t, "check", "--op", "add_owner(/x/, @b)", "--policy", p); code != cli.ExitInvalid {
		t.Error("check with both --op and --policy: want exit 3")
	}
	if code, _, _ := runCLI(t, "check"); code != cli.ExitInvalid {
		t.Error("check with neither --op nor --policy: want exit 3")
	}
}

// SPEC R-22: `check` reads no repository. As a verb it carries no repo flags
// at all, so the shape enforces the contract — it must succeed from a
// directory that is not a git repo, which is where the fleet script runs it
// before the first clone exists.
func TestCheck_ReadsNoRepository(t *testing.T) {
	p := syncWritePolicy(t, syncFleetPolicy)
	notARepo := t.TempDir()
	// t.Chdir mutates PROCESS-WIDE state. This test must never gain
	// t.Parallel(), and neither may any test that reaches the filesystem
	// through a relative path — fleet_test.go already runs t.Parallel()
	// throughout, and it is only safe because every path it touches is
	// absolute. Adding t.Parallel() here would move this chdir into that
	// window and make those tests fail nondeterministically, in whichever
	// order the scheduler happened to interleave them.
	t.Chdir(notARepo)

	code, _, errOut := runCLI(t, "check", "--policy", p)
	if code != cli.ExitOK {
		t.Fatalf("check outside any git repo: want exit 0, got %d\n%s", code, errOut)
	}
	// The repo-scoped flags do not exist on this verb.
	for _, args := range [][]string{
		{"check", "--policy", p, "--repo", notARepo},
		{"check", "--policy", p, "--create"},
		{"check", "--policy", p, "--dry-run"},
	} {
		if code, _, _ := runCLI(t, args...); code != cli.ExitInvalid {
			t.Errorf("check %v: repo-scoped flags must not be accepted, got %d", args[1:], code)
		}
	}
}

// SPEC R-22: `check` writes nothing. It sits one token away from --dry-run,
// where the failure mode is exit 0 across all 100 repos having read and
// written nothing — silent success, the worst outcome this design produces.
func TestCheck_WritesNothing(t *testing.T) {
	policyDir := t.TempDir()
	policyPath := filepath.Join(policyDir, "policy.json")
	if err := os.WriteFile(policyPath, []byte(syncFleetPolicy), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	// Same rule as TestCheck_ReadsNoRepository: t.Chdir is process-wide, so
	// this test must NOT become parallel. Here the chdir is load-bearing —
	// the assertion below is "check wrote nothing into the current working
	// directory" — so under t.Parallel() a concurrent test's write would be
	// attributed to `check`, and worse, the parallel tests in fleet_test.go
	// would be running against a cwd this test moved out from under them.
	t.Chdir(work)

	before := checkDirSnapshot(t, policyDir)
	if code, _, _ := runCLI(t, "check", "--policy", policyPath); code != cli.ExitOK {
		t.Fatalf("valid policy: want exit 0")
	}
	if got := checkDirSnapshot(t, policyDir); len(got) != len(before) || got["policy.json"] != before["policy.json"] {
		t.Errorf("check modified the policy directory: %v → %v", before, got)
	}
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 0 {
		t.Errorf("check wrote into the working directory: %v (err %v)", entries, err)
	}
}

// SPEC R-22: syntax errors fail fast at the FIRST problem; semantic errors
// accumulate and all print. Fixing a generated 40-op policy one error per run
// is miserable — but a file that is not JSON has no second error to report,
// only guesses.
func TestCheck_SyntaxFailsFastSemanticsAccumulate(t *testing.T) {
	// Trailing comma: not JSON. The semantic problems downstream of it
	// (`inhrit`, `frob`) are unreachable and must not be claimed.
	syntax := syncWritePolicy(t, `{"version":1,"on_empty":"inhrit","ops":["frob(/x/, @b)"],}`)
	code, out, errOut := runCLI(t, "check", "--policy", syntax)
	if code != cli.ExitInvalid {
		t.Fatalf("syntax error: want exit 3, got %d", code)
	}
	all := out + errOut
	if strings.Contains(all, "inhrit") || strings.Contains(all, "frob") {
		t.Errorf("syntax errors stop at the first; downstream semantics must not be reported:\n%s", all)
	}

	// Three independent semantic problems: all three must print in one run.
	semantic := syncWritePolicy(t, `{
	  "version": 1,
	  "on_empty": "inhrit",
	  "ops": [
	    {"id":"one","op":"add_owner(/x/, @b)","on_zero_match":"maybe"},
	    {"id":"two","op":"frob(/y/, @c)"}
	  ]
	}`)
	code, out, errOut = runCLI(t, "check", "--policy", semantic)
	if code != cli.ExitInvalid {
		t.Fatalf("semantic errors: want exit 3, got %d", code)
	}
	all = out + errOut
	for _, want := range []string{"inhrit", "maybe", "frob"} {
		if !strings.Contains(all, want) {
			t.Errorf("every semantic error must print in one run; %q missing:\n%s", want, all)
		}
	}
}

// SPEC R-22: check never returns anything but 0 and 3 — in particular not 1,
// 2, 4, 5 or 6. The fleet script's `*)` catch-all exits on anything else.
func TestCheck_NeverReturnsOtherExitCodes(t *testing.T) {
	valid := syncWritePolicy(t, syncFleetPolicy)
	invocations := [][]string{
		{"check", "--policy", valid},
		{"check", "--op", "add_owner(/x/, @a)"},
		{"check", "--op", "frob(/x/, @b)"},
		{"check", "--policy", filepath.Join(t.TempDir(), "absent.json")},
		{"check"},
	}
	for _, tc := range syncBrokenPolicies {
		invocations = append(invocations, []string{"check", "--policy", syncWritePolicy(t, tc.src)})
	}
	for _, args := range invocations {
		code, _, _ := runCLI(t, args...)
		if code != cli.ExitOK && code != cli.ExitInvalid {
			t.Errorf("check %v: exit %d — check may return only 0 or 3", args[1:], code)
		}
	}
}
