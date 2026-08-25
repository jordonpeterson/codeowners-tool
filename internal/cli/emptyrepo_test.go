package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// SPEC: `--repo ""` is rejected at argument-parsing time (exit 3), on every
// verb that takes the flag. `.` stays the default when the flag is ABSENT.
//
// An empty string is the one wrong value that used to get a default instead of
// an error, and it is the value a shell produces by accident:
//
//	REPO=$(lookup_repo "$name")     # returned nothing
//	codeowners-tool sync --repo "$REPO" --op '...'
//
// A typo'd path fails correctly. The empty string fell through to the flag's
// own default and targeted the working directory, so a fleet script standing
// in an unrelated checkout wrote to it at exit 0 — and the record carried
// "repo":"", so results.jsonl got a success row that did not say which
// repository had been changed.
func TestEmptyRepoFlagRejected(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/all\n",
		"important.txt":      "x\n",
	})
	plan := filepath.Join(t.TempDir(), "p.json")
	if code, _, e := runCLI(t, "plan", "--repo", dir, "--op", "add_owner(important.txt, @org/x)", "--out", plan); code != 0 {
		t.Fatalf("plan setup: %d %s", code, e)
	}

	for _, tc := range []struct {
		verb string
		args []string
	}{
		{"sync", []string{"sync", "--repo", "", "--op", "add_owner(important.txt, @attacker/team)"}},
		{"plan", []string{"plan", "--repo", "", "--op", "add_owner(important.txt, @attacker/team)"}},
		{"snapshot", []string{"snapshot", "--repo", ""}},
		{"audit", []string{"audit", "--repo", ""}},
		{"lint", []string{"lint", "--repo", ""}},
		{"apply", []string{"apply", "--plan", plan, "--repo", ""}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			code, out, errOut := runCLI(t, tc.args...)
			if code != cli.ExitInvalid {
				t.Errorf("%s --repo \"\": exit %d, want %d (repo-independent)\nstdout: %s\nstderr: %s",
					tc.verb, code, cli.ExitInvalid, out, errOut)
			}
			if !strings.Contains(errOut, "--repo") {
				t.Errorf("%s: the error must name the flag, got %q", tc.verb, errOut)
			}
		})
	}
}

// The refusal happens before anything is written. The working directory this
// process is standing in must be untouched — that is the whole point.
func TestEmptyRepoWritesNothingToTheWorkingDirectory(t *testing.T) {
	victim := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/all\n",
		"important.txt":      "x\n",
	})
	co := filepath.Join(victim, ".github", "CODEOWNERS")
	before, err := os.ReadFile(co)
	if err != nil {
		t.Fatal(err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	if err := os.Chdir(victim); err != nil {
		t.Fatal(err)
	}

	code, _, _ := runCLI(t, "sync", "--repo", "", "--op", "add_owner(important.txt, @attacker/team)")
	if code != cli.ExitInvalid {
		t.Fatalf("exit %d, want %d", code, cli.ExitInvalid)
	}

	after, err := os.ReadFile(co)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the working directory was written to:\n%s", after)
	}
}

// Absent is not empty. Omitting --repo still means the working directory,
// which is the documented default and what every one-repo invocation relies on.
func TestAbsentRepoFlagStillDefaultsToCwd(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/all\n",
		"a.txt":              "x\n",
	})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "audit")
	if code == cli.ExitInvalid {
		t.Fatalf("omitting --repo must keep working: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

// apply's --repo defaults to "" meaning "use the plan's repo", so the rejection
// cannot be read off the VALUE — it has to ask whether the flag was passed.
// Both readings are pinned here: passing it empty is an error, omitting it is
// how the plan's own repo field gets used.
func TestApplyEmptyRepoDistinguishesPassedFromAbsent(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/all\n",
		"a.txt":              "x\n",
	})
	plan := filepath.Join(t.TempDir(), "p.json")
	if code, _, e := runCLI(t, "plan", "--repo", dir, "--op", "add_owner(a.txt, @org/x)", "--out", plan); code != 0 {
		t.Fatalf("plan: %d %s", code, e)
	}

	if code, _, errOut := runCLI(t, "apply", "--plan", plan, "--repo", ""); code != cli.ExitInvalid {
		t.Errorf("apply --repo \"\": exit %d, want %d (%s)", code, cli.ExitInvalid, errOut)
	}
	if code, _, errOut := runCLI(t, "apply", "--plan", plan); code != cli.ExitOK {
		t.Errorf("apply with no --repo must use the plan's repo: exit %d (%s)", code, errOut)
	}
}
