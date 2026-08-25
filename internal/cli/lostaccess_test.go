package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decodeSyncRecord runs sync --format json and decodes the record it emits.
func decodeSyncRecord(t *testing.T, args ...string) map[string]any {
	t.Helper()
	code, out, errOut := runCLI(t, append([]string{"sync", "--format", "json"}, args...)...)
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("sync --format json emitted no record (exit %d): %v\nstdout: %s\nstderr: %s", code, err, out, errOut)
	}
	return rec
}

// SPEC R-24: a sync record distinguishes CO-OWNING from DISPLACING.
//
// `sync --format json` / `--summary-out` is what docs/FLEET.md loops over, and
// for a displacing change it carried no before-state at all: the change record
// is line-level, and an `insert` has no previous line, so `old_owners` is
// absent and `warnings` is null. The PR body a reviewer saw said only
// "paths whose owners change: 5" — five paths CHANGED, not "three teams stop
// owning things".
//
// The information already existed: `plan --out` emits ownership_rows with
// owners_before → owners_after for the identical op. But FLEET.md is explicit
// that a rollout loops over `sync`, so at fleet scale the artifact that would
// catch this was the one you did not have. This is surfacing, not new analysis.
func TestSync_ReportsOwnersLosingAccess(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @acme/all\n/scanners/ @acme/appsec @acme/security-leads\n",
		"scanners/scan.py":   "x\n",
		"README.md":          "y\n",
	})

	// The README's own advice for a displacing baseline.
	rec := decodeSyncRecord(t, "--repo", dir, "--op", "set_owners(*, [@acme/everyone])", "--dry-run")

	lost, _ := rec["owners_removed"].([]any)
	if len(lost) == 0 {
		t.Fatalf("a displacing run must report who lost access; record: %v", rec)
	}

	// Every owner that stops owning a path must appear.
	blob, _ := json.Marshal(lost)
	for _, want := range []string{"@acme/appsec", "@acme/security-leads", "scanners/scan.py"} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("owners_removed must name %q, got: %s", want, blob)
		}
	}
}

// A co-owning change takes nothing away, so the section must stay absent — a
// key that is present on every run is one a fleet stops reading.
func TestSync_CoOwningChangeReportsNoLostAccess(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @acme/all\n",
		"scanners/scan.py":   "x\n",
	})

	rec := decodeSyncRecord(t, "--repo", dir, "--op", "add_owner(/scanners/, @acme/appsec)", "--dry-run")

	if lost, ok := rec["owners_removed"]; ok {
		t.Errorf("add_owner co-owns and takes nothing away; owners_removed must be absent, got %v", lost)
	}
}

// The PR body is the one moment somebody is already reading. A displacing run
// gets its own section there, naming the teams rather than a count of paths.
func TestSync_SummaryOutHasAnOwnersLosingAccessSection(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @acme/all\n/scanners/ @acme/appsec @acme/security-leads\n",
		"scanners/scan.py":   "x\n",
		"README.md":          "y\n",
	})
	summary := filepath.Join(t.TempDir(), "body.md")

	if code, _, e := runCLI(t, "sync", "--repo", dir, "--op", "set_owners(*, [@acme/everyone])",
		"--dry-run", "--summary-out", summary); code != 0 {
		t.Fatalf("sync: %d %s", code, e)
	}
	body, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "Owners losing access") {
		t.Fatalf("the PR body must call out lost access:\n%s", got)
	}
	for _, want := range []string{"@acme/appsec", "@acme/security-leads"} {
		if !strings.Contains(got, want) {
			t.Errorf("the section must name %q:\n%s", want, got)
		}
	}
}

// And stays out of the body when nothing was taken away.
func TestSync_SummaryOutOmitsTheSectionWhenNothingIsLost(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @acme/all\n",
		"scanners/scan.py":   "x\n",
	})
	summary := filepath.Join(t.TempDir(), "body.md")

	if code, _, e := runCLI(t, "sync", "--repo", dir, "--op", "add_owner(/scanners/, @acme/appsec)",
		"--dry-run", "--summary-out", summary); code != 0 {
		t.Fatalf("sync: %d %s", code, e)
	}
	body, _ := os.ReadFile(summary)
	if strings.Contains(string(body), "Owners losing access") {
		t.Errorf("nothing was lost; the section must be absent:\n%s", body)
	}
}

// A path going from owned to UNOWNED is the sharpest form of losing access,
// and must be reported as such rather than slipping through as "no owners
// removed because there is no owner to name".
func TestSync_UnowningAPathCountsAsLostAccess(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/scanners/ @acme/appsec\n",
		"scanners/scan.py":   "x\n",
	})

	rec := decodeSyncRecord(t, "--repo", dir, "--op", "remove_owner(/scanners/, @acme/appsec)",
		"--on-empty", "unowned", "--dry-run")

	lost, _ := rec["owners_removed"].([]any)
	if len(lost) == 0 {
		t.Fatalf("un-owning a path is losing access; record: %v", rec)
	}
	blob, _ := json.Marshal(lost)
	if !strings.Contains(string(blob), "@acme/appsec") {
		t.Errorf("owners_removed must name @acme/appsec, got: %s", blob)
	}
}
