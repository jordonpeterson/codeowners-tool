// Package resolve_test defines ownership resolution: mapping every tracked
// path to its owner set via the LAST matching rule.
//
// SPEC S-1: last matching rule wins; owner sets do not union.
// SPEC S-9: matching a zero-owner rule yields an explicitly-empty owner set,
// distinct from "no rule matched".
// SPEC INV-3: resolution is computed over the actual tracked file tree, never
// over the pattern set.
package resolve_test

import (
	"reflect"
	"testing"

	"github.com/jordonpropm/codeowners-tool/internal/file"
	"github.com/jordonpropm/codeowners-tool/internal/resolve"
)

func mustParse(t *testing.T, s string) *file.File {
	t.Helper()
	return file.Parse([]byte(s))
}

// SPEC S-1: the LAST matching rule's owners apply — exclusively.
func TestS1_LastMatchWins(t *testing.T) {
	f := mustParse(t, "* @global\n*.js @js-owner\n")
	tree := []string{"a.txt", "src/b.js"}
	res := resolve.All(f, tree)

	if got := res["a.txt"].Owners; !reflect.DeepEqual(got, []string{"@global"}) {
		t.Errorf("a.txt owners = %v, want [@global]", got)
	}
	// Only @js-owner — NOT the union {@global, @js-owner}.
	if got := res["src/b.js"].Owners; !reflect.DeepEqual(got, []string{"@js-owner"}) {
		t.Errorf("src/b.js owners = %v, want [@js-owner] (owner sets do not union)", got)
	}
}

// SPEC S-9: GitHub's official example — a zero-owner rule un-owns a subtree.
func TestS9_ZeroOwnerRuleUnowns(t *testing.T) {
	f := mustParse(t, "/apps/ @octocat\n/apps/github\n")
	res := resolve.All(f, []string{"apps/web/a.go", "apps/github/b.go", "README.md"})

	if got := res["apps/web/a.go"].Owners; !reflect.DeepEqual(got, []string{"@octocat"}) {
		t.Errorf("apps/web owners = %v", got)
	}
	r := res["apps/github/b.go"]
	if !r.Matched || len(r.Owners) != 0 {
		t.Errorf("apps/github must be MATCHED with zero owners, got matched=%v owners=%v", r.Matched, r.Owners)
	}
	if res["README.md"].Matched {
		t.Error("README.md matches no rule; must be unmatched, not zero-owned")
	}
}

// Invalid lines are skipped during resolution (current GitHub semantics) —
// they neither own paths nor shadow earlier rules.
func TestA8_InvalidLinesDoNotResolve(t *testing.T) {
	f := mustParse(t, "* @global\n*.go not-an-owner\n")
	res := resolve.All(f, []string{"main.go"})
	if got := res["main.go"].Owners; !reflect.DeepEqual(got, []string{"@global"}) {
		t.Errorf("owners = %v, want [@global] (invalid line must be skipped)", got)
	}
}

// SPEC INV-3: a pattern matching no tracked file resolves nothing; resolution
// answers are only ever about real paths in the tree.
func TestINV3_ResolutionIsOverTree(t *testing.T) {
	f := mustParse(t, "/ghost/ @nobody\n* @global\n")
	res := resolve.All(f, []string{"real.txt"})
	if len(res) != 1 {
		t.Fatalf("resolutions = %d, want 1 (one per tracked path)", len(res))
	}
	if got := res["real.txt"].Owners; !reflect.DeepEqual(got, []string{"@global"}) {
		t.Errorf("owners = %v", got)
	}
}

// Resolution reports WHICH rule won, so plans can explain themselves (R-16).
func TestResolution_ReportsWinningRule(t *testing.T) {
	f := mustParse(t, "* @a\n/x/ @b\n")
	res := resolve.All(f, []string{"x/1.txt", "y/2.txt"})
	if res["x/1.txt"].LineIndex != 1 {
		t.Errorf("x/1.txt won by line %d, want 1", res["x/1.txt"].LineIndex)
	}
	if res["y/2.txt"].LineIndex != 0 {
		t.Errorf("y/2.txt won by line %d, want 0", res["y/2.txt"].LineIndex)
	}
}

// Owners compares as a SET for invariant checks: order on the line is
// presentation, not semantics (branch protection treats owners as OR).
func TestOwnersEqual_IsSetEquality(t *testing.T) {
	if !resolve.OwnersEqual([]string{"@a", "@b"}, []string{"@b", "@a"}) {
		t.Error("owner sets must compare order-insensitively")
	}
	if resolve.OwnersEqual([]string{"@a"}, []string{"@a", "@b"}) {
		t.Error("different sets must not compare equal")
	}
	if resolve.OwnersEqual(nil, []string{}) {
		t.Error("unmatched (nil) and explicit zero-owner must differ")
	}
	if !resolve.OwnersEqual(nil, nil) || !resolve.OwnersEqual([]string{}, []string{}) {
		t.Error("nil==nil and empty==empty must hold")
	}
}
