// R-22b at the level the guard lives: which SCOPE SPELLINGS earn the
// exemption from the declare-op order-dependence check.
//
// internal/cli covers the operator-visible contract. These cover the two
// spellings that resolve to one tracked file TODAY while naming a language
// that can grow tomorrow — the cases where "it matched exactly one file" is
// not the same claim as "it can only ever match that file".
package plan_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// dpContent grants @org/bot everything under .github/, so removing it from
// .github/CODEOWNERS alone requires a narrowing rule and cannot be a no-op.
const dpContent = "* @org/everyone\n.github @org/everyone @org/bot\n"

var dpTree = []string{".github/CODEOWNERS", ".github/workflows/ci.yml", "src/main.go"}

// dpDeclaredPairFragment is unique to the declare-op arm of R-8. Asserting
// only "R-8" would let these pins pass on the tree-based arm, or on ops.go's
// static conflict check, neither of which is what they are guarding.
const dpDeclaredPairFragment = "can both govern a path that does not exist yet"

// SPEC R-22b: an anchored, wildcard-free scope naming one tracked file
// commutes with a declared op. The declared op matched zero tracked paths, so
// it cannot match a tracked file, and nothing can appear beneath a file.
func TestR22b_AnchoredExactFileScopeCommutesWithADeclare(t *testing.T) {
	_, err := buildZM(t, dpContent, dpTree, plan.Options{OnEmpty: "error"},
		skipZMOp("remove_owner(/.github/CODEOWNERS, @org/bot)"),
		declareOp("add_owner(**/justfile, @org/bot)"))
	if err != nil {
		t.Fatalf("the two scopes can never meet, so the batch has no order to depend on: %v", err)
	}
}

// SPEC R-22b: a scope containing a wildcard is refused even when it resolves
// to exactly one tracked file.
//
// "/.github/CODEOWNER?" selects only .github/CODEOWNERS in this tree, so a
// tree-count test ("scope matched one path") would admit it. Its LANGUAGE is
// wider: a future .github/CODEOWNERX matches it, and so does a declared
// "**/CODEOWNERX". Admitting it would repeat TestINV6_TrailingStarStarIsNot
// ADirectoryPrefix — a disjointness claim that the tree happens to satisfy
// and the pattern does not, which is a wrong write rather than a missed one.
func TestR22b_WildcardScopeIsNotAnExactFileEvenWhenItMatchesOne(t *testing.T) {
	_, err := buildZM(t, dpContent, dpTree, plan.Options{OnEmpty: "error"},
		skipZMOp("remove_owner(/.github/CODEOWNER?, @org/bot)"),
		declareOp("add_owner(**/CODEOWNERX, @org/bot)"))
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("a wildcard scope can grow to meet the declared scope and must stay refused (R-8), got %v", err)
	}
	if !strings.Contains(err.Error(), dpDeclaredPairFragment) {
		t.Errorf("refusal must come from the declare-op guard (R-8), got %q", err.Error())
	}
}

// SPEC R-22b: an UNANCHORED scope is refused even when it resolves to exactly
// one tracked file.
//
// "justfile" matches that basename at any depth, so it is not a claim about
// one path at all — a justfile added under any new directory joins the scope,
// and a declared "/vendor/**" would then govern it too. The leading slash is
// what makes the scope's language finite, which is the property the exemption
// rests on; without it the tool would be reading a spelling as a guarantee it
// does not carry.
func TestR22b_UnanchoredScopeIsNotAnExactFileEvenWhenItMatchesOne(t *testing.T) {
	tree := []string{".github/CODEOWNERS", "justfile", "src/main.go"}
	_, err := buildZM(t, "* @org/everyone\njustfile @org/everyone @org/bot\n", tree,
		plan.Options{OnEmpty: "error"},
		skipZMOp("remove_owner(justfile, @org/bot)"),
		declareOp("add_owner(/vendor/**, @org/bot)"))
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("an unanchored scope matches at any depth and must stay refused (R-8), got %v", err)
	}
	if !strings.Contains(err.Error(), dpDeclaredPairFragment) {
		t.Errorf("refusal must come from the declare-op guard (R-8), got %q", err.Error())
	}
}
