package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// syncRecord runs sync --format json and decodes the record.
func syncRecord(t *testing.T, args ...string) map[string]any {
	t.Helper()
	code, out, errOut := runCLI(t, append([]string{"sync", "--format", "json"}, args...)...)
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("sync --format json did not emit a record (exit %d): %v\nstdout: %s\nstderr: %s", code, err, out, errOut)
	}
	return rec
}

// SPEC R-5/R-11: a rule whose pattern matches zero tracked files is invisible
// to remove_owner, because the op's scope is derived from paths. That is
// defensible — the op need not edit dormant rules — but reporting `unchanged`
// with `warnings: null` is not: in a fleet grouped on `.status`, the repo reads
// as ALREADY CORRECT while a dissolved team keeps a live claim that activates
// the moment somebody creates the directory.
//
// The reporter found this only by running `grep -rn 'org/legacy'` over the
// fleet after the tool had declared it clean — a step no documented workflow
// includes.
func TestRemoveOwner_WarnsAboutARuleItCannotReach(t *testing.T) {
	dir := initRepo(t, map[string]string{
		// /vendor/ names @org/legacy but there is no vendor/ directory.
		".github/CODEOWNERS": "/vendor/ @org/legacy\n* @org/all\n",
		"src/main.go":        "x\n",
	})

	rec := syncRecord(t, "--repo", dir, "--op", "remove_owner(*, @org/legacy)", "--dry-run")

	warnings, _ := rec["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatalf("a run that leaves a rule naming the removed owner must not be silent; record: %v", rec)
	}
	joined := ""
	for _, w := range warnings {
		joined += w.(string) + "\n"
	}
	for _, want := range []string{"/vendor/", "@org/legacy", "owns no files"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warning must mention %q, got:\n%s", want, joined)
		}
	}
}

// The warning is scoped by PATTERN containment, so a narrower op does not
// report a dormant rule it never claimed to touch. remove_owner(/src/, …)
// says nothing about /vendor/.
func TestRemoveOwner_NarrowerScopeDoesNotWarnAboutUnrelatedDormantRules(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/vendor/ @org/legacy\n/src/ @org/legacy @org/all\n",
		"src/main.go":        "x\n",
	})

	rec := syncRecord(t, "--repo", dir, "--op", "remove_owner(/src/, @org/legacy)", "--dry-run")

	warnings, _ := rec["warnings"].([]any)
	for _, w := range warnings {
		if strings.Contains(w.(string), "/vendor/") {
			t.Errorf("an op scoped to /src/ must not report /vendor/: %v", w)
		}
	}
}

// No dormant rule, nothing to say. The warning must not fire on the ordinary
// converged run, or a fleet learns to ignore it.
func TestRemoveOwner_NoWarningWhenNothingIsLeftBehind(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/src/ @org/legacy @org/all\n",
		"src/main.go":        "x\n",
	})

	rec := syncRecord(t, "--repo", dir, "--op", "remove_owner(*, @org/legacy)", "--dry-run")

	warnings, _ := rec["warnings"].([]any)
	for _, w := range warnings {
		if strings.Contains(w.(string), "owns no files") {
			t.Errorf("nothing was left behind; no unreachable-rule warning expected: %v", w)
		}
	}
}

// SPEC R-5: the zero-match refusal is worded for the op that hit it. Naming a
// dormant rule's own pattern as the scope of a remove_owner used to be refused
// with "refusing to create a dead rule" — but the operator is not creating
// anything, they are trying to remove an owner from a rule that already exists.
func TestRemoveOwner_ZeroMatchRefusalDoesNotTalkAboutCreating(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/vendor/ @org/legacy\n* @org/all\n",
		"src/main.go":        "x\n",
	})

	code, _, errOut := runCLI(t, "sync", "--repo", dir, "--op", "remove_owner(/vendor/, @org/legacy)", "--dry-run")
	if code != cli.ExitRefused {
		t.Fatalf("exit %d, want %d", code, cli.ExitRefused)
	}
	if strings.Contains(errOut, "create a dead rule") {
		t.Errorf("remove_owner creates nothing; the refusal must not say so: %s", errOut)
	}
	if !strings.Contains(errOut, "zero tracked files") {
		t.Errorf("the refusal must still say why: %s", errOut)
	}
}
