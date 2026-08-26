package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// planFileOnDisk builds a plan against dir and returns its path plus the
// decoded JSON, so a test can tamper with one field and write it back — which
// is exactly the window a plan lives in: generated in one place, reviewed,
// then applied somewhere else entirely.
func planFileOnDisk(t *testing.T, dir, op string) (string, map[string]any) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	if code, _, e := runCLI(t, "plan", "--repo", dir, "--op", op, "--out", path); code != 0 {
		t.Fatalf("plan: %d %s", code, e)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return path, doc
}

func writePlan(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// SPEC R-16: apply treats after_content as data to be CHECKED, not as an
// instruction to be obeyed. sha256_before covers the CODEOWNERS the plan was
// computed against — not the plan itself — so a plan corrupted, truncated, or
// hand-edited between review and apply used to be written without complaint.
//
// Observed before: `applied: .../CODEOWNERS (58 → 101 bytes)`, exit 0, and the
// file on disk was 18 bytes of `* @attacker/pwned`.
func TestApply_RefusesTamperedAfterContent(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "* @org/all\n",
		"services/web/app.ts": "x\n",
	})
	path, doc := planFileOnDisk(t, dir, "add_owner(/services/web/, @org/web-team)")

	doc["after_content"] = "* @attacker/pwned\n" // sha256_before left alone
	writePlan(t, path, doc)

	code, out, errOut := runCLI(t, "apply", "--plan", path)
	if code == cli.ExitOK {
		t.Fatalf("a tampered after_content must not be written: exit 0\n%s", out)
	}
	if !strings.Contains(errOut, "after_content") && !strings.Contains(errOut, "sha256_after") {
		t.Errorf("the refusal must name what failed, got: %s", errOut)
	}
	co, err := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(co), "@attacker") {
		t.Fatalf("the tampered content reached disk:\n%s", co)
	}
}

// Truncation is the accidental spelling of the same failure: a plan cut short
// in transit still parses as JSON and still carries a valid sha256_before.
func TestApply_RefusesTruncatedAfterContent(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "/a/ @org/a\n/b/ @org/b\n* @org/all\n",
		"services/web/app.ts": "x\n",
	})
	path, doc := planFileOnDisk(t, dir, "add_owner(/services/web/, @org/web-team)")

	full, _ := doc["after_content"].(string)
	doc["after_content"] = full[:len(full)/2]
	writePlan(t, path, doc)

	if code, _, _ := runCLI(t, "apply", "--plan", path); code == cli.ExitOK {
		t.Error("a truncated after_content must not be written")
	}
}

// An untampered plan still applies. The guard is worthless if it is not
// transparent on the path everybody actually uses.
func TestApply_UntamperedPlanStillApplies(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "* @org/all\n",
		"services/web/app.ts": "x\n",
	})
	path, _ := planFileOnDisk(t, dir, "add_owner(/services/web/, @org/web-team)")

	code, out, errOut := runCLI(t, "apply", "--plan", path)
	if code != cli.ExitOK {
		t.Fatalf("a good plan must apply: exit %d\n%s\n%s", code, out, errOut)
	}
	co, _ := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
	if !strings.Contains(string(co), "@org/web-team") {
		t.Errorf("the planned change did not land:\n%s", co)
	}
}

// A plan with no sha256_after at all is refused rather than silently skipping
// the check. A missing integrity field must never be the easy way past it.
func TestApply_RefusesPlanWithoutAfterHash(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "* @org/all\n",
		"services/web/app.ts": "x\n",
	})
	path, doc := planFileOnDisk(t, dir, "add_owner(/services/web/, @org/web-team)")

	delete(doc, "sha256_after")
	writePlan(t, path, doc)

	code, _, errOut := runCLI(t, "apply", "--plan", path)
	if code == cli.ExitOK {
		t.Fatal("a plan with no sha256_after must not be applied")
	}
	if !strings.Contains(errOut, "re-run") {
		t.Errorf("the refusal should tell the operator to re-run plan, got: %s", errOut)
	}
}

// SPEC: the success line reports the bytes the write actually moved, measured
// on disk. They used to be echoed from the plan's own size_before/size_after,
// so a tampered plan produced a confirmation message that was measurably false
// — 101 bytes reported for an 18-byte write.
func TestApply_ReportsMeasuredBytesNotThePlansClaim(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "* @org/all\n",
		"services/web/app.ts": "x\n",
	})
	path, doc := planFileOnDisk(t, dir, "add_owner(/services/web/, @org/web-team)")

	// Leave after_content honest; lie only about the sizes.
	doc["size_before"] = 9999
	doc["size_after"] = 8888
	writePlan(t, path, doc)

	code, out, errOut := runCLI(t, "apply", "--plan", path)
	if code != cli.ExitOK {
		t.Fatalf("size fields are not integrity fields; the plan must still apply: %d %s", code, errOut)
	}

	// Asserted on the byte report alone, not on the whole line. The line also
	// carries the target path, which under t.TempDir() is
	// .../TestApply_ReportsMeasuredBytesNotThePlansClaim1296888869/001/... —
	// a substring search for the tampered "8888" hits the tempdir's random
	// digits and fails a correct build, and the same search for the honest
	// sizes ("11", "39") passes on a build that reports nothing at all. Exact
	// equality on the extracted span settles both directions: a run echoing
	// the plan's claim reports (9999 → 8888 bytes) and fails here.
	co, _ := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
	before := len("* @org/all\n")
	want := "(" + itoa(before) + " → " + itoa(len(co)) + " bytes)"
	if got := byteReport(t, out); got != want {
		t.Errorf("want the measured %s, got %s\nfull line: %s", want, got, out)
	}
}

// byteReport is the parenthesized `(N → M bytes)` span of an apply success
// line, isolated from the target path that precedes it.
func byteReport(t *testing.T, out string) string {
	t.Helper()
	open, close := strings.LastIndex(out, "("), strings.LastIndex(out, ")")
	if open < 0 || close < open {
		t.Fatalf("no byte report in the success line: %s", out)
	}
	return out[open : close+1]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
