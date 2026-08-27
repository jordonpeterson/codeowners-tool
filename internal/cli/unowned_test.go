// R-40 end-to-end tests: `on_unowned` through check and sync, real repo,
// real exit codes. Written ahead of the implementation.
//
// The motivating fleet: repos where only select files have owners (a
// build.gradle, say). A blanket add_owner on /.github/ turned files any
// developer could approve into files only the platform team could —
// on_unowned=skip makes "co-own what is owned, leave open paths open" a
// statable, reviewable intent.
package cli_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// uoRepo is the motivating fixture: one owned file, everything else open.
func uoRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"CODEOWNERS":               "/build.gradle @org/gradle\n",
		"build.gradle":             "plugins {}\n",
		".github/workflows/ci.yml": "on: push\n",
		"src/main.go":              "package main\n",
	})
}

// SPEC R-40: sync with on_unowned=skip grants only where an owner already
// exists. The owned file gains the co-owner; the open paths stay open — the
// record says which, under `left_open` — and a repo that owned nothing in
// scope reports `skipped` at exit 0 with the file untouched.
func TestR40_SyncSkipsOpenPaths(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,"ops":[
		{"id":"gradle","op":"add_owner(*.gradle, @org/platform)","on_unowned":"skip"},
		{"id":"gh","op":"add_owner(/.github/, @org/platform)","on_unowned":"skip"}
	]}`)
	if code, _, errOut := runCLI(t, "check", "--policy", pol); code != cli.ExitOK {
		t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}

	repo := uoRepo(t)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("sync: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	pfWantFile(t, filepath.Join(repo, "CODEOWNERS"), "/build.gradle @org/gradle @org/platform\n")

	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusApplied || rec.OpsApplied != 1 || rec.OpsSkipped != 1 {
		t.Errorf("status=%q applied=%d skipped=%d, want applied/1/1", rec.Status, rec.OpsApplied, rec.OpsSkipped)
	}
	if rec.Ops[0].Status != "applied" {
		t.Errorf("gradle op: status = %q, want applied", rec.Ops[0].Status)
	}
	gh := rec.Ops[1]
	if gh.Status != "skipped" || !strings.Contains(gh.Reason, "on_unowned") {
		t.Errorf("gh op = %+v, want skipped with an on_unowned reason", gh)
	}
	if !reflect.DeepEqual(gh.LeftOpen, []string{".github/workflows/ci.yml"}) {
		t.Errorf("gh left_open = %v, want the workflow file", gh.LeftOpen)
	}
}

// SPEC R-40/R-34: `create: true` composes — a repo with NO CODEOWNERS has
// every path open, so an all-skip policy creates nothing (an empty grant must
// not conjure a file, R-23), reports `skipped`, and exits 0. The same policy
// on a repo whose file owns something both applies and stays created:false.
func TestR40_AllOpenRepoCreatesNothing(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,"create":true,"defaults":{"on_unowned":"skip"},"ops":[
		{"id":"gh","op":"add_owner(/.github/, @org/platform)"}
	]}`)
	repo := initRepo(t, map[string]string{
		".github/workflows/ci.yml": "on: push\n",
		"src/main.go":              "package main\n",
	})
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusSkipped || rec.Created {
		t.Errorf("status=%q created=%v, want skipped/false — an all-open repo gets no file", rec.Status, rec.Created)
	}
	for _, loc := range []string{"CODEOWNERS", ".github/CODEOWNERS", "docs/CODEOWNERS"} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(loc))); !os.IsNotExist(err) {
			t.Errorf("%s was created for a policy that granted nothing", loc)
		}
	}
}

// SPEC R-40: a policy error is caught by `check` with no repository open —
// exit 3, naming the field, the op, and the legal set — and `sync` refuses
// identically before writing anything.
func TestR40_CheckCatchesBadOnUnowned(t *testing.T) {
	cases := map[string]struct {
		src       string
		fragments []string
	}{
		"bad value": {
			`{"version":1,"ops":[{"id":"gh","op":"add_owner(/x/, @a)","on_unowned":"skpi"}]}`,
			[]string{"on_unowned", `"assign"`, `"skip"`},
		},
		"wrong verb": {
			`{"version":1,"ops":[{"id":"s","op":"set_owners(/x/, [@a])","on_unowned":"skip"}]}`,
			[]string{"on_unowned", "add_owner"},
		},
		"declare with except (R-30 stands)": {
			`{"version":1,"ops":[{"id":"d","op":"add_owner(/x/ except /x/gen/, @a)","on_zero_match":"declare","on_unowned":"skip"}]}`,
			[]string{"declare", "except"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pol := syncWritePolicy(t, tc.src)
			code, _, errOut := runCLI(t, "check", "--policy", pol)
			if code != cli.ExitInvalid {
				t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			for _, f := range tc.fragments {
				pfWantFragment(t, "check stderr", errOut, f)
			}
		})
	}
}

// SPEC R-40 (by R-32's rule): the left-open facts render in EVERY format,
// not just JSON. The text output names each declined path under its op, and
// the PR summary — the artifact the reviewer actually reads — carries a
// "Left open" section, exactly as the carve-out facts do (review finding:
// the disclosure reached only --format json).
func TestR40_LeftOpenRendersInTextAndSummary(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,"ops":[{"id":"all","op":"add_owner(*, @org/platform)","on_unowned":"skip"}]}`)
	repo := uoRepo(t)
	sumPath := filepath.Join(t.TempDir(), "summary.md")
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--summary-out", sumPath)
	if code != cli.ExitOK {
		t.Fatalf("sync: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	if !strings.Contains(out, "left open: .github/workflows/ci.yml") ||
		!strings.Contains(out, "left open: src/main.go") {
		t.Errorf("text output must name each left-open path, got:\n%s", out)
	}
	sum := syncReadFile(t, sumPath)
	if !strings.Contains(sum, "## Left open (`on_unowned: skip`)") ||
		!strings.Contains(sum, "- `all`: `src/main.go`") {
		t.Errorf("summary must carry the Left open section, got:\n%s", sum)
	}
}

// SPEC R-40/R-35b: `check` echoes the RESOLVED on_unowned beside the other
// per-op settings, so the reviewer sees the value in force at each op without
// folding the defaults block in their head — and a policy that never mentions
// the field keeps its pre-R-40 output byte for byte (the echo states only
// settings somebody asked about).
func TestR40_CheckEchoesResolvedOnUnowned(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,"defaults":{"on_unowned":"skip"},"ops":[
		"add_owner(/a/, @a)",
		{"id":"b","op":"add_owner(/b/, @b)","on_unowned":"assign"},
		{"id":"d","op":"add_owner(/d/, @d)","on_zero_match":"declare"}
	]}`)
	code, out, errOut := runCLI(t, "check", "--policy", pol)
	if code != cli.ExitOK {
		t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	for _, want := range []string{
		"ops[0]  on_zero_match: require (built-in); on_unowned: skip",
		"b       on_zero_match: require (built-in); on_unowned: assign",
		"d       on_zero_match: declare; on_unowned: skip",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check output missing %q:\n%s", want, out)
		}
	}

	// A policy that never mentions on_unowned echoes nothing about it.
	quiet := syncWritePolicy(t, `{"version":1,"ops":[{"id":"z","op":"add_owner(/a/, @a)","on_zero_match":"skip"}]}`)
	code, out, errOut = runCLI(t, "check", "--policy", quiet)
	if code != cli.ExitOK {
		t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	if strings.Contains(out, "on_unowned") {
		t.Errorf("pre-R-40 policies must keep their echo unchanged, got:\n%s", out)
	}
}

// SPEC R-40/R-19: nightly convergence. Run 1 applies; runs 2 and 3 report
// `unchanged` with byte-identical content — the skipped-open paths do not
// make a converged repo look like pending work, and they stay disclosed in
// `left_open` on every run (R-19's "run 2 keeps disclosing what run 1 did").
func TestR40_NightlyRerunConverges(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,"defaults":{"on_unowned":"skip"},"ops":["add_owner(*.gradle, @org/platform)"]}`)
	repo := uoRepo(t)

	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("run 1: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	first := syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))

	for run := 2; run <= 3; run++ {
		code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
		if code != cli.ExitOK {
			t.Fatalf("run %d: want exit 0, got %d\nstderr:\n%s", run, code, errOut)
		}
		rec := syncDecodeRecord(t, out)
		if rec.Status != cli.StatusUnchanged {
			t.Errorf("run %d: status = %q, want unchanged", run, rec.Status)
		}
		if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != first {
			t.Errorf("run %d changed bytes:\n%s\nvs\n%s", run, first, got)
		}
	}
}

// SPEC R-40b: the real fleet policy, run unchanged across the three repo
// shapes it must survive. One reviewed op, `declare` + `skip`, states the
// whole rule — pre-own what does not exist, never close what is open today —
// and each shape gets the outcome that rule implies, all at exit 0.
//
// This policy was exit 3 before the refusal lift: `check` halted the wave at
// repo 0, so none of these outcomes was reachable.
func TestR40b_FleetPolicyAcrossThreeRepoShapes(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":[
		{"id":"gh","op":"add_owner(/.github/, @org/platform)","on_zero_match":"declare","on_unowned":"skip"}
	]}`)
	if code, _, errOut := runCLI(t, "check", "--policy", pol); code != cli.ExitOK {
		t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}

	// Shape 1 — no .github/ at all: the rule is declared for the future.
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":  "/src/ @org/core\n",
		"src/main.go": "package main\n",
	})
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("shape 1: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	pfWantFile(t, filepath.Join(repo, "CODEOWNERS"), "/src/ @org/core\n/.github/ @org/platform\n")
	if rec := syncDecodeRecord(t, out); rec.Ops[0].Proven != "structural" {
		t.Errorf("shape 1: proven = %q, want structural", rec.Ops[0].Proven)
	}

	// Shape 2 — .github/ with one owned file and one open: co-own the owned
	// one, leave the open one open.
	// No .github/CODEOWNERS in this fixture: it would become the GOVERNING
	// file (S-8) and, being empty, would leave every path open — making this
	// shape 3 by accident.
	repo = initRepo(t, map[string]string{
		"CODEOWNERS":               "/.github/dependabot.yml @org/admins\n",
		".github/dependabot.yml":   "version: 2\n",
		".github/workflows/ci.yml": "on: push\n",
	})
	code, out, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("shape 2: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Ops[0].Status != "applied" || rec.Ops[0].Proven != "tree" {
		t.Errorf("shape 2: op = %+v, want applied/tree", rec.Ops[0])
	}
	if !reflect.DeepEqual(rec.Ops[0].LeftOpen, []string{".github/workflows/ci.yml"}) {
		t.Errorf("shape 2: left_open = %v, want the workflow file", rec.Ops[0].LeftOpen)
	}

	// Shape 3 — .github/ exists and is entirely open: nothing is written, and
	// the record says which paths stayed open. Declare must NOT step in here:
	// a rule for the scope would close every one of them.
	repo = initRepo(t, map[string]string{
		"CODEOWNERS":               "/src/ @org/core\n",
		"src/main.go":              "package main\n",
		".github/workflows/ci.yml": "on: push\n",
	})
	code, out, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("shape 3: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	pfWantFile(t, filepath.Join(repo, "CODEOWNERS"), "/src/ @org/core\n")
	rec = syncDecodeRecord(t, out)
	if rec.Status != cli.StatusSkipped {
		t.Errorf("shape 3: status = %q, want skipped", rec.Status)
	}
	if !strings.Contains(rec.Ops[0].Reason, "on_unowned") {
		t.Errorf("shape 3: reason = %q, want the on_unowned skip reason", rec.Ops[0].Reason)
	}
}
