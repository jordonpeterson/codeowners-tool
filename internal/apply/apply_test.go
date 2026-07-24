// Package apply_test defines the writer: the ONLY code path in the system
// that modifies bytes on disk (R-0).
//
// SPEC R-10: after apply, the file is validated; a write that produces new
// syntax errors is rolled back (exit 6).
// The plan pins the input's SHA-256; a drifted file is never written over.
package apply_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jordonpropm/codeowners-tool/internal/apply"
	"github.com/jordonpropm/codeowners-tool/internal/ops"
	"github.com/jordonpropm/codeowners-tool/internal/plan"
)

func makePlan(t *testing.T, content string, tree []string, spec string) *plan.Plan {
	t.Helper()
	parsed, err := ops.ParseAll([]string{spec})
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build([]byte(content), tree, parsed, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApply_WritesPlannedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CODEOWNERS")
	content := "/x/ @a\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p := makePlan(t, content, []string{"x/a.go"}, "add_owner(/x/, @b)")
	if err := apply.Apply(p, path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != p.AfterContent {
		t.Errorf("file = %q, want %q", got, p.AfterContent)
	}
}

// A plan is only valid against the exact bytes it was computed from. If the
// file changed since planning, applying would clobber someone else's edit
// and the invariants would no longer be proven.
func TestApply_RefusesDriftedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CODEOWNERS")
	content := "/x/ @a\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p := makePlan(t, content, []string{"x/a.go"}, "add_owner(/x/, @b)")
	// File drifts after planning.
	if err := os.WriteFile(path, []byte("/x/ @a @someone-else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := apply.Apply(p, path)
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("drifted file must be refused as invalid input, got %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "/x/ @a @someone-else\n" {
		t.Error("drifted file must be left untouched")
	}
}

// SPEC R-10: a write whose result contains NEW invalid lines is rolled back
// and reported (exit 6). Pre-existing invalid lines don't block — the tool
// didn't create them and refuses only what it caused.
func TestR10_RollbackOnNewInvalidLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CODEOWNERS")
	content := "/x/ @a\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p := makePlan(t, content, []string{"x/a.go"}, "add_owner(/x/, @b)")
	// Sabotage the plan's after-content to simulate a writer defect.
	p.AfterContent = "/x/ @a @b\n!broken @c\n"
	err := apply.Apply(p, path)
	var ve *apply.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("invalid write must be a ValidationError (exit 6), got %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != content {
		t.Errorf("file must be rolled back to %q, got %q", content, got)
	}
}

// Pre-existing invalid lines pass through: validation compares against the
// plan's input, not against perfection.
func TestR10_PreexistingInvalidLinesDoNotBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CODEOWNERS")
	content := "!oldbroken @z\n/x/ @a\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p := makePlan(t, content, []string{"x/a.go"}, "add_owner(/x/, @b)")
	if err := apply.Apply(p, path); err != nil {
		t.Fatalf("pre-existing invalid line must not block apply: %v", err)
	}
}

// SPEC S-4: apply re-checks size independently of plan — belt and braces.
func TestApply_RefusesOversizedPlan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CODEOWNERS")
	content := "/x/ @a\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p := makePlan(t, content, []string{"x/a.go"}, "add_owner(/x/, @b)")
	p.AfterContent = p.AfterContent + string(make([]byte, 3_000_001))
	err := apply.Apply(p, path)
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("oversized apply must be refused, got %v", err)
	}
}

// Review finding: apply must write atomically and, when the CODEOWNERS path
// is a symlink, replace the TARGET's content while preserving the link.
func TestApply_SymlinkPreserved(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "REAL_CODEOWNERS")
	link := filepath.Join(dir, "CODEOWNERS")
	content := "/x/ @a\n"
	if err := os.WriteFile(real, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := makePlan(t, content, []string{"x/a.go"}, "add_owner(/x/, @b)")
	if err := apply.Apply(p, link); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink must be preserved, not replaced by a regular file")
	}
	got, _ := os.ReadFile(real)
	if string(got) != p.AfterContent {
		t.Errorf("target content = %q, want %q", got, p.AfterContent)
	}
}
