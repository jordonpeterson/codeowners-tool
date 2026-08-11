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

// ---------------------------------------------------------------------------
// The rollout scenarios.
//
// fleet_test.go asks whether one policy survives seven differently-shaped
// CODEOWNERS files. This file asks the questions that come BEFORE and AFTER
// that: the shapes of the CLONES a runner is handed, the file the run actually
// wrote, the commit and the PR that follow it, and the CI gate the fleet is
// left standing on afterwards.
//
// Each test is one scenario from a real org-wide rollout, written as the
// operator meets it. The requirement in each doc comment is what the tool has
// to provide for that scenario to be survivable at 100 repos — not what is
// convenient to assert.
// ---------------------------------------------------------------------------

// rolloutPolicySrc is the org baseline: one op every repo is expected to
// satisfy, and one declared rule that is the whole point of a baseline — every
// repo ends up governing its workflows directory whether or not it has one
// today.
const rolloutPolicySrc = `{
  "version": 1,
  "name": "org baseline ownership",
  "description": "CI owns workflows everywhere.",
  "ops": [
    { "id": "api", "op": "add_owner(/services/api/, @org/api-team)" },
    { "id": "ci",  "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare" }
  ]
}`

// gitIn runs one git command in dir, with a fixed identity so the test does
// not depend on the machine's git config.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// rolloutSync runs one repo's sync under --format json and returns the parsed
// record with the exit code — the two things the fleet loop reads.
func rolloutSync(t *testing.T, args ...string) (cli.SyncRecord, int, string) {
	t.Helper()
	code, stdout, errOut := runCLI(t, append([]string{"sync", "--format", "json"}, args...)...)
	var rec cli.SyncRecord
	if strings.TrimSpace(stdout) != "" {
		if err := json.Unmarshal([]byte(stdout), &rec); err != nil {
			t.Fatalf("record is not JSON: %v\nraw: %s\nstderr: %s", err, stdout, errOut)
		}
	}
	return rec, code, errOut
}

// rolloutPolicy writes the policy outside every clone.
func rolloutPolicy(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// SPEC R-19/R-24: the four clone shapes a fleet runner is handed, and the
// verdict each must produce.
//
// docs/FLEET.md's loop clones with `gh repo clone -- --depth 1`, so the
// ordinary case is a SHALLOW clone; CI runners (actions/checkout among them)
// leave a DETACHED HEAD; an org always has a repo that is freshly created and
// has NO COMMITS AT ALL; and plenty of repos still default to `master`. None
// of these is about CODEOWNERS, and that is the point — a rollout that halts,
// or worse writes into a tree it could not read, because of how a clone was
// made would be unusable. Three of the four must converge normally and the
// empty one must be exit 2 with `status: error`, so the fleet script files it
// under infrastructure rather than under repos that need a CODEOWNERS decision.
func TestRollout_CloneShapesTheRunnerMeets(t *testing.T) {
	t.Parallel()
	policy := rolloutPolicy(t, rolloutPolicySrc)
	origin := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/eng\n",
		"services/api/main.go": "package api\n",
	})

	t.Run("shallow clone", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "clone")
		gitIn(t, t.TempDir(), "clone", "-q", "--depth", "1", "file://"+origin, dir)
		rec, code, errOut := rolloutSync(t, "--repo", dir, "--policy", policy)
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0 — the documented fleet loop clones with --depth 1\nstderr: %s", code, errOut)
		}
		if rec.Status != cli.StatusApplied {
			t.Errorf("status %q, want applied", rec.Status)
		}
	})

	t.Run("detached HEAD", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "clone")
		gitIn(t, t.TempDir(), "clone", "-q", "file://"+origin, dir)
		gitIn(t, dir, "checkout", "-q", "--detach", "HEAD")
		rec, code, errOut := rolloutSync(t, "--repo", dir, "--policy", policy)
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0 — a CI checkout is detached, and the default --branch HEAD is exactly what it has\nstderr: %s", code, errOut)
		}
		if rec.Status != cli.StatusApplied {
			t.Errorf("status %q, want applied", rec.Status)
		}
	})

	t.Run("default branch is not main", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "clone")
		gitIn(t, t.TempDir(), "clone", "-q", "file://"+origin, dir)
		gitIn(t, dir, "branch", "-m", "master")
		rec, code, errOut := rolloutSync(t, "--repo", dir, "--policy", policy)
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0 — nothing in a rollout may depend on what the default branch is called\nstderr: %s", code, errOut)
		}
		if rec.Status != cli.StatusApplied {
			t.Errorf("status %q, want applied", rec.Status)
		}
	})

	t.Run("repository with no commits", func(t *testing.T) {
		dir := t.TempDir()
		gitIn(t, dir, "init", "-q", "-b", "main")
		rec, code, _ := rolloutSync(t, "--repo", dir, "--policy", policy, "--create")
		if code != cli.ExitRefused {
			t.Fatalf("exit %d, want 2 — a repo with no commits is one repo's problem, and exit 3 would halt the rollout", code)
		}
		if rec.Status != cli.StatusError {
			t.Errorf("status %q, want %q: there is no tree to read, so this is a clone that could not be processed rather than a CODEOWNERS the tool declined to touch",
				rec.Status, cli.StatusError)
		}
		if rec.Error == "" {
			t.Error("an error record must carry the reason")
		}
		if _, ok := fleetCodeowners(t, dir); ok {
			t.Error("--create must write nothing into a repository the tool could not read")
		}
	})
}

// SPEC R-24: every record names the CODEOWNERS file the run wrote, under
// `codeowners_path`.
//
// A rollout does not end at sync: the loop has to commit the change and open a
// PR. Which file to `git add` differs per repo — .github/, the root, or docs/,
// whichever GitHub would load (S-8) — and the operator cannot know which
// without opening every clone. Without this field the only workable recipe is
// `git add -A`, which is how a stray build artifact or a summary written into
// the clone ends up in 100 PRs. `plan` and `snapshot` have named their file
// since v1; the record that drives the fleet is the one document that did not.
//
// Under --create the path is where the file WOULD be written, so `--dry-run
// --create` previews it too.
func TestRollout_RecordNamesTheFileItWrote(t *testing.T) {
	t.Parallel()
	policy := rolloutPolicy(t, rolloutPolicySrc)
	sources := map[string]string{
		"services/api/main.go": "package api\n",
	}
	cases := []struct {
		name  string
		files map[string]string
		extra []string
		want  string
	}{
		{name: ".github", files: map[string]string{".github/CODEOWNERS": "* @org/eng\n"}, want: ".github/CODEOWNERS"},
		{name: "root", files: map[string]string{"CODEOWNERS": "* @org/eng\n"}, want: "CODEOWNERS"},
		{name: "docs", files: map[string]string{"docs/CODEOWNERS": "* @org/eng\n"}, want: "docs/CODEOWNERS"},
		{name: "created", files: nil, extra: []string{"--create"}, want: ".github/CODEOWNERS"},
		{name: "created under dry-run", files: nil, extra: []string{"--create", "--dry-run"}, want: ".github/CODEOWNERS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range sources {
				files[k] = v
			}
			for k, v := range tc.files {
				files[k] = v
			}
			dir := initRepo(t, files)
			rec, code, errOut := rolloutSync(t, append([]string{"--repo", dir, "--policy", policy}, tc.extra...)...)
			if code != cli.ExitOK {
				t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
			}
			if rec.CodeownersPath != tc.want {
				t.Fatalf("codeowners_path = %q, want %q — the fleet loop stages exactly this path", rec.CodeownersPath, tc.want)
			}
			// The claim is only worth anything if the bytes really moved there.
			if !strings.Contains(strings.Join(tc.extra, " "), "--dry-run") {
				b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rec.CodeownersPath)))
				if err != nil {
					t.Fatalf("record names %q but nothing is there: %v", rec.CodeownersPath, err)
				}
				if !strings.Contains(string(b), "@org/api-team") {
					t.Errorf("record names %q, but the change is not in it:\n%s", rec.CodeownersPath, b)
				}
			}
		})
	}

	// A run that never chose a file must not claim one: absent, not "".
	t.Run("no file chosen", func(t *testing.T) {
		dir := initRepo(t, sources)
		rec, code, _ := rolloutSync(t, "--repo", dir, "--policy", policy)
		if code != cli.ExitRefused {
			t.Fatalf("exit %d, want 2 (no CODEOWNERS, no --create)", code)
		}
		if rec.CodeownersPath != "" {
			t.Errorf("codeowners_path = %q, want empty — nothing was written and nothing may be staged", rec.CodeownersPath)
		}
	})
}

// SPEC R-24 (S-8/A-10): when the file this run writes is not the file GitHub
// will read, the record says so — every time, on stderr and in `warnings`.
//
// Two spellings of one failure, and both report "applied", exit 0 today. A
// repo carrying both `.github/CODEOWNERS` and a root `CODEOWNERS` gets the
// .github one edited, correctly, while the root file — which GitHub never
// loads and which usually says something different — sits there looking
// authoritative to every human who opens the repo. And `--file docs/CODEOWNERS`
// on a repo whose .github/ file governs writes a rule into a file GitHub does
// not read at all: the rollout reports 100 successes and moves no ownership,
// which is the "applied, dead on arrival" outcome these verbs exist to
// prevent. A rollout cannot audit every clone by hand, so the run that touches
// the file is the one that has to say it.
func TestRollout_WritingAFileGitHubWillNotReadIsWarned(t *testing.T) {
	t.Parallel()
	policy := rolloutPolicy(t, rolloutPolicySrc)

	t.Run("second CODEOWNERS file present", func(t *testing.T) {
		dir := initRepo(t, map[string]string{
			".github/CODEOWNERS":   "* @org/eng\n",
			"CODEOWNERS":           "* @org/somebody-else\n",
			"services/api/main.go": "package api\n",
		})
		rec, code, errOut := rolloutSync(t, "--repo", dir, "--policy", policy)
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0 — a second file is a cleanup task, not a reason to refuse\nstderr: %s", code, errOut)
		}
		if rec.CodeownersPath != ".github/CODEOWNERS" {
			t.Errorf("wrote %q, want .github/CODEOWNERS (S-8 precedence)", rec.CodeownersPath)
		}
		got := strings.Join(rec.Warnings, "\n")
		if !strings.Contains(got, "CODEOWNERS") || !strings.Contains(got, "A-10") {
			t.Errorf("no warning naming the ignored file and A-10; warnings = %#v", rec.Warnings)
		}
		if !strings.Contains(errOut, "warning:") {
			t.Errorf("the warning must also reach stderr, where a `--format text` operator sees it:\n%s", errOut)
		}
		// The ignored file is not touched: it is a human's to delete.
		b, _ := os.ReadFile(filepath.Join(dir, "CODEOWNERS"))
		if string(b) != "* @org/somebody-else\n" {
			t.Errorf("the non-governing file was modified: %q", b)
		}
	})

	t.Run("--file names a file that does not govern", func(t *testing.T) {
		dir := initRepo(t, map[string]string{
			".github/CODEOWNERS":   "* @org/eng\n",
			"docs/CODEOWNERS":      "* @org/eng\n",
			"services/api/main.go": "package api\n",
		})
		rec, code, errOut := rolloutSync(t, "--repo", dir, "--policy", policy, "--file", "docs/CODEOWNERS")
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0 — an explicit --file is the operator's call\nstderr: %s", code, errOut)
		}
		got := strings.Join(rec.Warnings, "\n")
		if !strings.Contains(got, ".github/CODEOWNERS") {
			t.Errorf("the warning must name the file that DOES govern, or the operator cannot act on it; warnings = %#v", rec.Warnings)
		}
	})

	t.Run("the ordinary repo warns about nothing", func(t *testing.T) {
		dir := initRepo(t, map[string]string{
			".github/CODEOWNERS":   "* @org/eng\n",
			"services/api/main.go": "package api\n",
		})
		rec, code, _ := rolloutSync(t, "--repo", dir, "--policy", policy)
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0", code)
		}
		if len(rec.Warnings) != 0 {
			t.Errorf("a plain repo emitted warnings %#v — a warning nobody can act on trains the fleet to ignore all of them", rec.Warnings)
		}
	})
}

// SPEC R-24: the artifacts a rollout commits and reviews agree with each other
// — record, working tree, and PR body all name one file — and a second wave
// over the committed result converges.
//
// This is the whole loop docs/FLEET.md stops one step short of: sync, stage
// exactly what the record names, commit, open the PR with --summary-out as its
// body. If the summary named a different file from the record, or the second
// wave found something left to do, the fleet would open a PR per repo per run
// forever.
func TestRollout_CommitAndPullRequestRoundTrip(t *testing.T) {
	t.Parallel()
	policy := rolloutPolicy(t, rolloutPolicySrc)
	dir := initRepo(t, map[string]string{
		"CODEOWNERS":           "* @org/eng\n",
		"services/api/main.go": "package api\n",
	})
	body := filepath.Join(t.TempDir(), "body.md")

	rec, code, errOut := rolloutSync(t, "--repo", dir, "--policy", policy, "--summary-out", body)
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
	}
	if rec.CodeownersPath == "" {
		t.Fatal("the record must name the file, or there is nothing to stage")
	}

	// `git add "$(jq -r .codeowners_path)"` — the point of the field.
	gitIn(t, dir, "add", rec.CodeownersPath)
	staged := gitIn(t, dir, "diff", "--cached", "--name-only")
	if staged != rec.CodeownersPath {
		t.Fatalf("staged %q, want exactly %q — staging by the record is what keeps a stray file out of 100 PRs", staged, rec.CodeownersPath)
	}
	gitIn(t, dir, "commit", "-qm", "chore: org baseline ownership")

	b, err := os.ReadFile(body)
	if err != nil {
		t.Fatalf("--summary-out: %v", err)
	}
	if !strings.Contains(string(b), rec.CodeownersPath) {
		t.Errorf("the PR body does not name the file the PR changes (%q):\n%s", rec.CodeownersPath, b)
	}
	if !strings.Contains(string(b), "org baseline ownership") {
		t.Errorf("the PR body must name the policy:\n%s", b)
	}

	// Wave two, against the committed result.
	rec2, code2, errOut2 := rolloutSync(t, "--repo", dir, "--policy", policy)
	if code2 != cli.ExitOK {
		t.Fatalf("second wave: exit %d, want 0\nstderr: %s", code2, errOut2)
	}
	if rec2.Status != cli.StatusUnchanged {
		t.Fatalf("second wave: status %q, want unchanged — otherwise every run opens another PR", rec2.Status)
	}
	if out := gitIn(t, dir, "status", "--porcelain"); out != "" {
		t.Errorf("second wave left the clone dirty:\n%s", out)
	}
}

// SPEC R-11/A-4: `audit --fail-on` lets the CI gate ride on the findings that
// mean something, so a fleet can gate on audit at all.
//
// The two halves of this tool's own advice contradict each other today. A
// baseline rollout uses `on_zero_match: declare` — that is what makes 100 repos
// converge on one policy — and a declared rule matches zero files by
// construction. The README then puts `codeowners-tool audit` in CI, where exit
// 4 is the gate. So the recommended rollout turns every repo it touched red on
// the next push, over an A-4 finding the tool itself labels "report-only: may
// be deliberate". The operator's only escape is `--checks` with the other
// eleven checks spelled out, which silently stops running any check added
// later.
//
// The findings are still all printed under every setting: the flag moves the
// GATE, never the report. Inconclusive (exit 5, R-12) is a different axis and
// deliberately untouched — an unreachable API is not a finding you can grade.
func TestRollout_AuditGateAfterADeclareBaseline(t *testing.T) {
	t.Parallel()
	policy := rolloutPolicy(t, rolloutPolicySrc)
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/eng\n",
		"services/api/main.go": "package api\n",
	})
	if _, code, errOut := rolloutSync(t, "--repo", dir, "--policy", policy); code != cli.ExitOK {
		t.Fatalf("sync: exit %d\nstderr: %s", code, errOut)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "rollout")

	// The default is unchanged: any finding trips the gate.
	code, out, _ := runCLI(t, "audit", "--repo", dir)
	if code != cli.ExitFindings {
		t.Fatalf("default audit: exit %d, want 4 — the declared baseline rule is an A-4 finding\n%s", code, out)
	}
	if !strings.Contains(out, "A-4") {
		t.Fatalf("expected the A-4 finding:\n%s", out)
	}

	// Gating on errors: the report is identical, the verdict is not.
	code, gated, _ := runCLI(t, "audit", "--repo", dir, "--fail-on", "error")
	if code != cli.ExitOK {
		t.Fatalf("--fail-on error: exit %d, want 0 — the only finding is a report-only warning\n%s", code, gated)
	}
	if !strings.Contains(gated, "A-4") {
		t.Errorf("--fail-on must move the gate, not suppress the report:\n%s", gated)
	}

	// An error-severity finding still fails under the same setting. A second
	// CODEOWNERS file (A-10) is the one this rollout can actually create.
	if err := os.WriteFile(filepath.Join(dir, "CODEOWNERS"), []byte("* @org/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "second file")
	code, out, _ = runCLI(t, "audit", "--repo", dir, "--fail-on", "error")
	if code != cli.ExitFindings {
		t.Fatalf("--fail-on error with an A-10 error present: exit %d, want 4\n%s", code, out)
	}

	// never is the reporting mode: everything printed, nothing gated.
	code, out, _ = runCLI(t, "audit", "--repo", dir, "--fail-on", "never")
	if code != cli.ExitOK {
		t.Fatalf("--fail-on never: exit %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "A-10") {
		t.Errorf("--fail-on never must still report:\n%s", out)
	}

	// The middle rung: an `info` finding (A-9's unowned-path coverage, which
	// every large repo has) is a fleet-wide statistic, not a CI failure.
	unowned := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/x/ @org/eng\n",
		"x/a.go":             "package x\n",
		"y/b.go":             "package y\n",
	})
	if code, out, _ := runCLI(t, "audit", "--repo", unowned, "--checks", "a9"); code != cli.ExitFindings {
		t.Fatalf("default audit with an info finding: exit %d, want 4\n%s", code, out)
	}
	code, out, _ = runCLI(t, "audit", "--repo", unowned, "--checks", "a9", "--fail-on", "warning")
	if code != cli.ExitOK {
		t.Fatalf("--fail-on warning with only an info finding: exit %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "A-9") {
		t.Errorf("the info finding must still be reported:\n%s", out)
	}

	// A misspelled threshold is invalid input, never a silently wide-open
	// gate: `--fail-on eror` passing everything would be a green CI that
	// checks nothing.
	if code, _, errOut := runCLI(t, "audit", "--repo", dir, "--fail-on", "eror"); code != cli.ExitInvalid {
		t.Errorf("--fail-on eror: exit %d, want 3\nstderr: %s", code, errOut)
	}
}

// SPEC R-24 (S-3): a repo whose CODEOWNERS already has an invalid line
// converges, keeps that line byte-for-byte, and reports it as a warning.
//
// Every org has them: a line with a typo'd owner, committed years ago. GitHub
// skips such a line and honors the rest of the file (S-3), and so does this
// tool — the ownership it proves against is the ownership GitHub computes, so
// the rollout is correct. What it must not do is stay quiet. Some paths in that
// repo are owned by nobody and the reason is on a line the tool just read,
// while the operator is looking at a green `applied` row. The rewrite is also
// exactly when someone will assume the file was validated.
func TestRollout_InheritedInvalidLineIsWarnedNotSwallowed(t *testing.T) {
	t.Parallel()
	before := "* @org/eng\nnot an owner!!\n"
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":   before,
		"services/api/main.go": "package api\n",
	})
	rec, code, errOut := rolloutSync(t, "--repo", dir,
		"--op", "add_owner(/services/api/, @org/api-team)")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0 — GitHub skips the bad line and honors the rest (S-3), and so must a rollout\nstderr: %s", code, errOut)
	}
	got := strings.Join(rec.Warnings, "\n")
	if !strings.Contains(got, "line 2") {
		t.Errorf("no warning naming the invalid line; warnings = %#v", rec.Warnings)
	}
	if !strings.Contains(got, "audit") {
		t.Errorf("the warning must point at the verb that lists them all; warnings = %#v", rec.Warnings)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "not an owner!!") {
		t.Errorf("the invalid line was rewritten or dropped; this tool never edits a line it was not asked about (INV-5):\n%s", b)
	}
}

// SPEC INV-5/R-19: a fleet is not all-Unix and not all-tidy. A CRLF file stays
// CRLF, and a file with no trailing newline does not have its last rule joined
// to the first one written.
//
// Both are invisible in a diff view and catastrophic in the file: a lone `\n`
// appended to a CRLF file gives GitHub a rule line ending in a stray `\r`, and
// appending to a file whose last byte is not a newline concatenates two
// patterns into one that matches nothing. At fleet scale nobody opens the
// files; the run's own claim to have converged is all there is.
func TestRollout_ByteStyleSurvivesTheRollout(t *testing.T) {
	t.Parallel()
	policy := rolloutPolicy(t, rolloutPolicySrc)
	cases := []struct {
		name    string
		before  string
		wantEOL string
	}{
		{name: "CRLF", before: "* @org/eng\r\n# a comment\r\n", wantEOL: "\r\n"},
		{name: "no trailing newline", before: "* @org/eng", wantEOL: "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := initRepo(t, map[string]string{
				".github/CODEOWNERS":   tc.before,
				"services/api/main.go": "package api\n",
			})
			if _, code, errOut := rolloutSync(t, "--repo", dir, "--policy", policy); code != cli.ExitOK {
				t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
			}
			b, err := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
			if err != nil {
				t.Fatal(err)
			}
			got := string(b)
			if !strings.HasSuffix(got, tc.wantEOL) {
				t.Errorf("file does not end with the file's own line ending %q:\n%q", tc.wantEOL, got)
			}
			for i, line := range strings.Split(strings.TrimSuffix(got, tc.wantEOL), tc.wantEOL) {
				if strings.Contains(line, "\r") || strings.Contains(line, "\n") {
					t.Errorf("line %d mixes line endings: %q", i+1, line)
				}
				if strings.Count(line, "@org/eng") > 1 {
					t.Errorf("line %d looks like two rules joined together: %q", i+1, line)
				}
			}
			// And the run is still convergent on the file it just wrote.
			rec, code, _ := rolloutSync(t, "--repo", dir, "--policy", policy)
			if code != cli.ExitOK || rec.Status != cli.StatusUnchanged {
				t.Errorf("second run: exit %d status %q, want 0/unchanged", code, rec.Status)
			}
		})
	}
}

// SPEC R-24/S-6: tracked paths with spaces and non-ASCII characters resolve
// like any other, and are proven against, not skipped.
//
// `git ls-tree` quotes such paths unless it is asked for NUL-terminated output,
// and a tool that mis-splits them does not fail — it silently believes the
// repository has fewer files than it does, and then "proves" INV-2 over the
// subset it can see. Every org has a repo with a `docs/Über guide.md` in it.
func TestRollout_UnusualTrackedPathsAreStillProven(t *testing.T) {
	t.Parallel()
	odd := "services/api/héllo wörld.go"
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/eng\n",
		odd:                    "package api\n",
		"services/api/main.go": "package api\n",
	})
	rec, code, errOut := rolloutSync(t, "--repo", dir,
		"--op", "add_owner(/services/api/, @org/api-team)")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
	}
	if rec.PathsChanged != 2 {
		t.Errorf("paths_changed = %d, want 2 — the odd path must be counted, not dropped from the tree", rec.PathsChanged)
	}

	snapPath := filepath.Join(t.TempDir(), "snap.json")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "rollout")
	if code, _, errOut := runCLI(t, "snapshot", "--repo", dir, "--out", snapPath); code != cli.ExitOK {
		t.Fatalf("snapshot: exit %d\nstderr: %s", code, errOut)
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
	owners, ok := snap.Ownership[odd]
	if !ok {
		t.Fatalf("snapshot has no entry for %q; it lists %d paths", odd, len(snap.Ownership))
	}
	if len(owners) != 2 || owners[1] != "@org/api-team" {
		t.Errorf("%q resolves to %v, want the api team added", odd, owners)
	}
}

// SPEC R-19/R-18: the second wave of a reorg — rename one team, retire
// another — converges, and `verify` proves from raw snapshots that nothing
// outside the declared scope moved.
//
// Wave 1 is the baseline everyone runs. Wave 2 is the one that scares people:
// a team was renamed in an acquisition and another was dissolved, so the
// rollout both substitutes an identifier everywhere and removes an owner from
// one scope. The removal empties a rule, which is where R-6 stops and demands
// a decision — and the fleet has to make that decision once, in the policy
// file, not per repo. Afterwards the proof is done the hard way: snapshot
// before, snapshot after, compare, with no trust in the planner that produced
// the change.
func TestRollout_SecondWaveReorgVerifiesOutOfScope(t *testing.T) {
	t.Parallel()
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/eng\n" +
			"/services/api/ @org/api-team @org/legacy\n" +
			"/docs/ @org/docs-team\n",
		"services/api/main.go": "package api\n",
		"docs/guide.md":        "# guide\n",
	})
	before := filepath.Join(t.TempDir(), "before.json")
	after := filepath.Join(t.TempDir(), "after.json")
	if code, _, errOut := runCLI(t, "snapshot", "--repo", dir, "--out", before); code != cli.ExitOK {
		t.Fatalf("snapshot before: exit %d\nstderr: %s", code, errOut)
	}

	wave2 := rolloutPolicy(t, `{
  "version": 1,
  "name": "reorg wave 2",
  "on_empty": "error",
  "ops": [
    { "id": "rename", "op": "rename_owner(@org/api-team, @org/platform-api)" },
    { "id": "retire", "op": "remove_owner(/services/api/, @org/legacy)" }
  ]
}`)
	if code, _, errOut := runCLI(t, "check", "--policy", wave2); code != cli.ExitOK {
		t.Fatalf("check: exit %d — wave 2's policy must survive review before it reaches a repo\nstderr: %s", code, errOut)
	}
	rec, code, errOut := rolloutSync(t, "--repo", dir, "--policy", wave2)
	if code != cli.ExitOK {
		t.Fatalf("wave 2: exit %d, want 0\nstderr: %s", code, errOut)
	}
	if rec.Status != cli.StatusApplied {
		t.Fatalf("wave 2: status %q, want applied", rec.Status)
	}

	got, err := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	want := "* @org/eng\n/services/api/ @org/platform-api\n/docs/ @org/docs-team\n"
	if string(got) != want {
		t.Errorf("file after wave 2:\n got: %q\nwant: %q", got, want)
	}

	// Re-running wave 2 is a no-op, so a partially-failed rollout can be
	// restarted over the whole list rather than a reconstructed subset.
	rec2, code2, _ := rolloutSync(t, "--repo", dir, "--policy", wave2)
	if code2 != cli.ExitOK || rec2.Status != cli.StatusUnchanged {
		t.Errorf("wave 2 re-run: exit %d status %q, want 0/unchanged", code2, rec2.Status)
	}

	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "reorg")
	if code, _, errOut := runCLI(t, "snapshot", "--repo", dir, "--out", after); code != cli.ExitOK {
		t.Fatalf("snapshot after: exit %d\nstderr: %s", code, errOut)
	}
	// /docs/ was never in scope: the reorg must be provable as confined to
	// the api scope, from the snapshots alone.
	if code, _, errOut := runCLI(t, "verify", "--before", before, "--after", after, "--scope", "/services/api/"); code != cli.ExitOK {
		t.Fatalf("verify: exit %d, want 0 — wave 2 moved ownership outside the scope it declared\nstderr: %s", code, errOut)
	}
}
