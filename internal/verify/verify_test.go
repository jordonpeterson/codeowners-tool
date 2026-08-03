// Package verify_test defines snapshot comparison (R-18): the invariant can
// be checked in CI from two ownership snapshots WITHOUT trusting the tool
// that produced the change.
package verify_test

import (
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/verify"
)

func snap(ownership map[string][]string) *verify.Snapshot {
	return &verify.Snapshot{Ownership: ownership}
}

// SPEC R-18 + INV-2: with no scope given, verify asserts NOTHING changed.
func TestR18_NoScope_AssertNoChange(t *testing.T) {
	before := snap(map[string][]string{"a.go": {"@x"}, "b.go": {"@y"}})
	same := snap(map[string][]string{"a.go": {"@x"}, "b.go": {"@y"}})
	res, err := verify.Compare(before, same, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || len(res.Changed) != 0 {
		t.Errorf("identical snapshots must verify clean, got %+v", res)
	}

	drifted := snap(map[string][]string{"a.go": {"@x", "@z"}, "b.go": {"@y"}})
	res, _ = verify.Compare(before, drifted, nil)
	if res.OK() {
		t.Error("changed ownership with no declared scope must fail verification")
	}
	if len(res.Changed) != 1 || res.Changed[0].Path != "a.go" {
		t.Errorf("changed = %+v", res.Changed)
	}
}

// With scopes, changes inside scope pass; any out-of-scope change fails —
// this is INV-2 checked from raw data.
func TestR18_ScopedChangesConfined(t *testing.T) {
	before := snap(map[string][]string{"x/a.go": {"@a"}, "y/b.go": {"@y"}})
	after := snap(map[string][]string{"x/a.go": {"@a", "@b"}, "y/b.go": {"@y"}})
	res, err := verify.Compare(before, after, []string{"/x/"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Errorf("in-scope change must pass: %+v", res.Violations)
	}

	badAfter := snap(map[string][]string{"x/a.go": {"@a", "@b"}, "y/b.go": {"@CHANGED"}})
	res, _ = verify.Compare(before, badAfter, []string{"/x/"})
	if res.OK() {
		t.Error("out-of-scope change must fail")
	}
	if len(res.Violations) != 1 || res.Violations[0].Path != "y/b.go" {
		t.Errorf("violations = %+v", res.Violations)
	}
}

// Order of owners on a line is presentation: {"@a","@b"} == {"@b","@a"}.
func TestR18_OwnerOrderIrrelevant(t *testing.T) {
	before := snap(map[string][]string{"a.go": {"@a", "@b"}})
	after := snap(map[string][]string{"a.go": {"@b", "@a"}})
	if res, _ := verify.Compare(before, after, nil); !res.OK() {
		t.Errorf("owner order must not count as change: %+v", res.Changed)
	}
}

// Unowned (no rule matched, null in JSON) vs explicitly-zero-owners ([]) are
// DIFFERENT states; transitioning between them is a real ownership change.
func TestR18_UnownedVsZeroOwnersDistinct(t *testing.T) {
	before := snap(map[string][]string{"a.go": nil})
	after := snap(map[string][]string{"a.go": {}})
	if res, _ := verify.Compare(before, after, nil); res.OK() {
		t.Error("nil→[] must register as a change (unowned vs explicitly zero-owned)")
	}
}

// A path added or removed from the tree is reported, not silently ignored —
// but it is NOT an invariant violation. INV-2 preserves what a path resolved
// to BEFORE; an added path has no before, a removed path has no after. The
// two snapshots come from different refs, so counting a tree delta as a
// violation made the documented CI recipe fail on any PR that added a file
// outside the declared scope, with CODEOWNERS byte-identical.
func TestR18_TreeChangesSurfaceButDoNotViolate(t *testing.T) {
	before := snap(map[string][]string{"a.go": {"@a"}, "gone.go": {"@a"}})
	after := snap(map[string][]string{"a.go": {"@a"}, "new.go": {"@a"}})
	res, _ := verify.Compare(before, after, nil)
	if !res.OK() {
		t.Errorf("tree-only delta must not violate the invariant: %+v", res.Violations)
	}
	if len(res.Changed) != 0 {
		t.Errorf("tree delta is not an ownership change: %+v", res.Changed)
	}
	if len(res.Added) != 1 || res.Added[0].Path != "new.go" {
		t.Errorf("added = %+v, want new.go", res.Added)
	}
	if len(res.Removed) != 1 || res.Removed[0].Path != "gone.go" {
		t.Errorf("removed = %+v, want gone.go", res.Removed)
	}
}

// The gate still bites: an added file does not launder a real reassignment of
// the subtree it lands in, because that subtree's pre-existing files change.
func TestR18_AddedFileDoesNotMaskReassignment(t *testing.T) {
	before := snap(map[string][]string{"web/app.js": {"@fe"}})
	after := snap(map[string][]string{"web/app.js": {"@other"}, "web/new.js": {"@other"}})
	res, _ := verify.Compare(before, after, []string{"/services/"})
	if res.OK() {
		t.Error("out-of-scope reassignment must still fail when the PR also adds files")
	}
	if len(res.Violations) != 1 || res.Violations[0].Path != "web/app.js" {
		t.Errorf("violations = %+v", res.Violations)
	}
}
