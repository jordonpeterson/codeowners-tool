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

// A path added or removed from the tree is reported, not silently ignored.
// It is NOT a violation — see TestR18_AddedPathIsNotAViolation for why. This
// test pinned the older reading, where any tree delta failed verification;
// it now pins that the delta still SURFACES, which is the half worth keeping.
func TestR18_TreeChangesSurfaceButDoNotViolate(t *testing.T) {
	before := snap(map[string][]string{"a.go": {"@a"}})
	after := snap(map[string][]string{"a.go": {"@a"}, "new.go": {"@a"}})
	res, _ := verify.Compare(before, after, nil)
	if len(res.Added) != 1 || res.Added[0].Path != "new.go" {
		t.Errorf("new path must surface as an addition, got %+v", res.Added)
	}
	if !res.OK() {
		t.Errorf("...but must not fail verification: %+v", res.Violations)
	}
}

// SPEC R-18 + INV-2: a path present in only ONE snapshot is a tree delta, not
// an ownership change, and cannot violate the invariant. INV-2 preserves what
// a path resolved to BEFORE; an added path has no before and a removed one has
// no after, so neither has anything the invariant can be about. Reported, so
// the run is not silent about a tree that moved — but never fatal.
//
// Before this, the documented CI recipe (snapshot two branches, verify against
// the declared scope) failed on any PR that added a file outside that scope,
// with CODEOWNERS byte-identical. Snapshots come from different refs, so their
// trees differ on every real pull request.
func TestR18_AddedPathIsNotAViolation(t *testing.T) {
	before := snap(map[string][]string{"a.go": {"@a"}})
	after := snap(map[string][]string{"a.go": {"@a"}, "web/new.js": {"@frontend"}})

	res, err := verify.Compare(before, after, []string{"/services/api/"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Errorf("an added path must not violate INV-2, got violations %+v", res.Violations)
	}
	if len(res.Added) != 1 || res.Added[0].Path != "web/new.js" {
		t.Errorf("added = %+v, want the one new path", res.Added)
	}
	if len(res.Changed) != 0 {
		t.Errorf("a tree delta is not an ownership change: changed = %+v", res.Changed)
	}
}

// The mirror: a deleted path has no after, so it cannot violate INV-2 either.
func TestR18_RemovedPathIsNotAViolation(t *testing.T) {
	before := snap(map[string][]string{"a.go": {"@a"}, "old.go": {"@legacy"}})
	after := snap(map[string][]string{"a.go": {"@a"}})

	res, _ := verify.Compare(before, after, nil)
	if !res.OK() {
		t.Errorf("a removed path must not violate INV-2, got %+v", res.Violations)
	}
	if len(res.Removed) != 1 || res.Removed[0].Path != "old.go" {
		t.Errorf("removed = %+v, want the one deleted path", res.Removed)
	}
}

// The gate is not weakened. A CODEOWNERS edit that reassigns a subtree still
// shows up on that subtree's PRE-EXISTING files, so adding a file to the same
// subtree cannot launder the reassignment. Only a scope whose every file is
// new to the branch goes unchecked, and there the invariant has nothing to say.
func TestR18_AddedFileDoesNotMaskReassignment(t *testing.T) {
	before := snap(map[string][]string{"web/app.ts": {"@a"}})
	after := snap(map[string][]string{
		"web/app.ts": {"@attacker"}, // reassigned — the real violation
		"web/new.js": {"@attacker"}, // added in the same subtree
	})

	res, _ := verify.Compare(before, after, []string{"/services/api/"})
	if res.OK() {
		t.Fatal("a reassignment of a pre-existing path must still violate")
	}
	if len(res.Violations) != 1 || res.Violations[0].Path != "web/app.ts" {
		t.Errorf("violations = %+v, want only the reassigned pre-existing path", res.Violations)
	}
}

// Tree deltas stay out of the violation set under EVERY scope setting,
// including the no-scope "assert nothing changed" mode — the strictest one.
func TestR18_TreeDeltasAreNotViolationsWithoutScopes(t *testing.T) {
	before := snap(map[string][]string{"a.go": {"@a"}})
	after := snap(map[string][]string{"a.go": {"@a"}, "new.go": {"@a"}})
	if res, _ := verify.Compare(before, after, nil); !res.OK() {
		t.Errorf("no-scope mode asserts no OWNERSHIP change; a new path is not one: %+v", res.Violations)
	}
}

// A path that is added AND lands in an unowned state is still only a tree
// delta. `null` here is the absence of a matching rule, not a lost owner.
func TestR18_AddedUnownedPathIsStillOnlyATreeDelta(t *testing.T) {
	before := snap(map[string][]string{"a.go": {"@a"}})
	after := snap(map[string][]string{"a.go": {"@a"}, "stray.txt": nil})
	res, _ := verify.Compare(before, after, nil)
	if !res.OK() {
		t.Errorf("an added unowned path must not violate: %+v", res.Violations)
	}
	if len(res.Added) != 1 || res.Added[0].Path != "stray.txt" {
		t.Errorf("added = %+v", res.Added)
	}
}
