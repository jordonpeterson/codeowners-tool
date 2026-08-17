package cli_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// jsonUnmarshalStrict decodes and rejects trailing data, so a test cannot pass
// on the first of two objects accidentally written to stdout.
func jsonUnmarshalStrict(s string, v any) error {
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(v); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err == nil {
		return errTrailing
	}
	return nil
}

var errTrailing = errors.New("trailing JSON after the first value")

// sameOwners compares owner sets without imposing an order — the assertions in
// this file are about who can approve a file, and CODEOWNERS order is not part
// of that claim.
func sameOwners(got, want []string) bool {
	g, w := append([]string{}, got...), append([]string{}, want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) == 0 && len(w) == 0 {
		return true
	}
	return reflect.DeepEqual(g, w)
}

func sameSnapshot(before, after map[string]string) bool { return reflect.DeepEqual(before, after) }

// gitCommitAll commits the working tree, because `snapshot` reads the ref's
// tree rather than the working directory — it answers what GitHub would do,
// and GitHub only ever sees committed files.
func gitCommitAll(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "sync"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// assertAll fails unless every fragment appears, naming the ones that did not
// — an error message is part of the contract here, since these are the strings
// an operator greps out of a fleet log.
func assertAll(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("output must mention %q; got:\n%s", w, got)
		}
	}
}

// These tests cover R-25 at the command line: `--on-unowned own|skip`.
//
// The planner tests prove the narrowing is correct. These prove the OPERATOR
// can reach it in one invocation, which is the entire point of the flag —
// the alternative it replaces is a snapshot, a jq filter, a generated policy
// and a shell script wrapped around the tool, all of which have to agree with
// the tool about what "unowned" means in order to be safe.

// r25Repo is the shape the flag exists for: some *.gradle files sit under a
// team's rule, and some sit where no rule reaches them.
func r25Repo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		".github/CODEOWNERS":        "# owners\n/services/api/    @org/api-team\n/libs/            @org/libs-team\n",
		"services/api/build.gradle": "",
		"services/api/main.go":      "",
		"libs/settings.gradle":      "",
		"build.gradle":              "", // root: no rule reaches it
		"unowned-area/build.gradle": "", // ditto
		"README.md":                 "",
	})
}

// r25Owners resolves ownership from a snapshot of the working tree, so the
// assertions are about who can approve a file — not about which lines the
// synthesizer chose to write.
func r25Owners(t *testing.T, dir string) map[string][]string {
	t.Helper()
	code, out, errb := runCLI(t, "snapshot", "--repo", dir)
	if code != 0 {
		t.Fatalf("snapshot exit %d: %s", code, errb)
	}
	var snap struct {
		Ownership map[string][]string `json:"ownership"`
	}
	if err := jsonUnmarshalStrict(out, &snap); err != nil {
		t.Fatalf("snapshot JSON: %v\n%s", err, out)
	}
	return snap.Ownership
}

// SPEC R-25 (the whole feature, in one command): `--on-unowned skip` adds the
// owner alongside existing owners and leaves paths nobody owns untouched.
//
// The file has to be committed before snapshot sees it — snapshot asks what
// GitHub would do, and GitHub only ever reads committed files.
func TestR25_CLISkipCoOwnsOnlyWhereSomebodyAlreadyOwns(t *testing.T) {
	dir := r25Repo(t)
	code, _, errb := runCLI(t, "sync", "--repo", dir,
		"--op", "add_owner(*.gradle, @github_platform)", "--on-unowned", "skip")
	if code != 0 {
		t.Fatalf("sync exit %d, want 0: %s", code, errb)
	}
	gitCommitAll(t, dir)

	got := r25Owners(t, dir)
	for path, want := range map[string][]string{
		"services/api/build.gradle": {"@org/api-team", "@github_platform"},
		"libs/settings.gradle":      {"@org/libs-team", "@github_platform"},
		"services/api/main.go":      {"@org/api-team"}, // not a .gradle file
	} {
		if !sameOwners(got[path], want) {
			t.Errorf("%s owners = %v, want %v", path, got[path], want)
		}
	}
	for _, path := range []string{"build.gradle", "unowned-area/build.gradle", "README.md"} {
		if len(got[path]) != 0 {
			t.Errorf("%s owners = %v, want NOBODY: these had no owner before, and adding one would newly REQUIRE an approval to merge them", path, got[path])
		}
	}
}

// SPEC R-25: the same command without the flag takes ownership of those paths
// — the difference the flag exists to express, shown side by side on one repo.
func TestR25_CLIDefaultTakesOwnershipOfUnownedPaths(t *testing.T) {
	dir := r25Repo(t)
	code, _, errb := runCLI(t, "sync", "--repo", dir, "--op", "add_owner(*.gradle, @github_platform)")
	if code != 0 {
		t.Fatalf("sync exit %d, want 0: %s", code, errb)
	}
	gitCommitAll(t, dir)

	got := r25Owners(t, dir)
	if !sameOwners(got["build.gradle"], []string{"@github_platform"}) {
		t.Errorf("build.gradle owners = %v, want {@github_platform}: without the flag the default still takes ownership", got["build.gradle"])
	}
}

// SPEC R-25: `--on-unowned` is rejected with `--policy`, for R-20's reason —
// the policy file in git must be the complete statement of what ran.
//
// It has a second effect worth keeping: on_zero_match=declare is settable only
// in a policy file and this flag only without one, so the contradictory pair
// can never meet anywhere except the policy validator that rejects it.
func TestR25_CLIFlagRejectedWithPolicy(t *testing.T) {
	dir := r25Repo(t)
	pol := syncWritePolicy(t, `{"version":1,"ops":["add_owner(*.gradle, @github_platform)"]}`)
	code, _, errb := runCLI(t, "sync", "--repo", dir, "--policy", pol, "--on-unowned", "skip")
	if code != 3 {
		t.Fatalf("exit %d, want 3 (repo-independent, halts a fleet run at repo 0): %s", code, errb)
	}
	assertAll(t, errb, "--on-unowned", "on_unowned", "R-20")
}

// SPEC R-25: the flag enforces the same legality table as a policy file, so a
// batch containing an op that cannot carry it is rejected outright rather than
// having the flag applied to some ops and silently passed over on others.
func TestR25_CLIFlagRejectedOnBatchesItCannotApplyTo(t *testing.T) {
	dir := r25Repo(t)
	for _, spec := range []string{
		"set_owners(*.gradle, [@github_platform])",
		"rename_owner(@org/api-team, @org/platform-api)",
	} {
		t.Run(spec, func(t *testing.T) {
			code, _, errb := runCLI(t, "sync", "--repo", dir,
				"--op", "add_owner(*.gradle, @github_platform)", "--op", spec, "--on-unowned", "skip")
			if code != 3 {
				t.Fatalf("exit %d, want 3: %s", code, errb)
			}
			assertAll(t, errb, "--on-unowned", "add_owner")
		})
	}
}

// SPEC R-25: an unknown value is exit 3 and never a silent fall back to the
// default, which would write the opposite intent across a whole fleet.
func TestR25_CLIUnknownValueIsExit3(t *testing.T) {
	dir := r25Repo(t)
	code, _, errb := runCLI(t, "sync", "--repo", dir,
		"--op", "add_owner(*.gradle, @github_platform)", "--on-unowned", "maybe")
	if code != 3 {
		t.Fatalf("exit %d, want 3: %s", code, errb)
	}
	assertAll(t, errb, "maybe", "own", "skip")
}

// SPEC R-25: a repo where NO in-scope path is owned is a clean skip carrying
// R-25's own reason — not a refusal, and not confusable with R-21's.
//
// This is the fleet case. "This repo owns none of its *.gradle files" is the
// policy working as intended, and a rollout must step over it rather than
// stopping, while still recording enough for an operator to tell the two kinds
// of skip apart afterwards.
func TestR25_CLINothingOwnedIsARecordedSkipNotAFailure(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/docs/ @org/docs\n",
		"build.gradle":       "",
		"docs/a.md":          "",
	})
	before := checkDirSnapshot(t, dir)
	code, out, errb := runCLI(t, "sync", "--repo", dir, "--format", "json",
		"--op", "add_owner(*.gradle, @github_platform)", "--on-unowned", "skip")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — a fleet run must step over this repo: %s", code, errb)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != "skipped" {
		t.Errorf("status = %q, want %q", rec.Status, "skipped")
	}
	if len(rec.Ops) != 1 {
		t.Fatalf("ops = %d, want 1", len(rec.Ops))
	}
	if r := rec.Ops[0]; r.Status != "skipped" || !strings.Contains(r.Reason, "R-25") {
		t.Errorf("op result = %+v, want status skipped with a reason naming R-25 so it is distinguishable from R-21's zero-match skip", r)
	}
	if after := checkDirSnapshot(t, dir); !sameSnapshot(before, after) {
		t.Error("a skipped run must write nothing at all")
	}
}
