// R-39: `--on-empty=inherit` where the rule that would be deleted also governs
// out-of-scope paths. Deletion is unavailable (R-1 protects those paths), but
// the narrowing rule the planner already inserts can restate what the in-scope
// paths fall through to, which resolves identically. These are the white-box
// cases the fleet e2e suite cannot reach: the cascade, and the two states no
// single narrowing line can express.
package plan_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// SPEC R-39: the restated owners are the fallthrough MINUS the owner being
// removed. The rule above lists @x too, so a verbatim restatement would hand
// the revoked owner straight back to the path the removal just cleared — the
// cascade `inherit` performs when it deletes (R-6), which narrowing must
// perform too. Paths outside the op's scope keep @x, because nothing asked
// for it to be removed there (INV-2).
func TestR39_NarrowingStripsTheRemovedOwnerFromTheFallthrough(t *testing.T) {
	tree := []string{"a/f.go", "a/g.go", "top.txt"}
	p, err := build(t, "* @x @root\n/a/ @x\n", tree, plan.Options{OnEmpty: "inherit"},
		"remove_owner(/a/f.go, @x)")
	if err != nil {
		t.Fatalf("expressible inherit narrowing must not be refused: %v", err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	for path, want := range map[string][]string{
		"a/f.go":  {"@root"},
		"a/g.go":  {"@x"},
		"top.txt": {"@x", "@root"},
	} {
		if got := after[path].Owners; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
}

// SPEC R-39: nothing to fall through to. Deleting the rule would leave the
// in-scope path matching NO rule, and "unmatched" is not the explicitly
// zero-owned `[]` of S-9 — no line expresses it, so the planner refuses and
// names the alternative rather than inventing an owner.
func TestR39_NoFallthroughRuleRefuses(t *testing.T) {
	tree := []string{"a/f.go", "a/g.go"}
	_, err := build(t, "/a/ @x\n", tree, plan.Options{OnEmpty: "inherit"},
		"remove_owner(/a/f.go, @x)")
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("want refusal (exit 2), got %v", err)
	}
	for _, want := range []string{"would match no rule", "--on-empty=unowned"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q must contain %q", err, want)
		}
	}
}

// SPEC R-39: in-scope paths that would fall through to DIFFERENT owners cannot
// be stated by one narrowing rule, and the planner will not pick a winner. It
// refuses naming both sides, because which paths diverge is the thing the
// operator has to look at.
func TestR39_DivergentFallthroughRefuses(t *testing.T) {
	tree := []string{"a/sub/f.go", "a/sub/g.go", "a/other.go"}
	_, err := build(t, "* @root\n/a/sub/g.go @other\n/a/ @x\n", tree,
		plan.Options{OnEmpty: "inherit"}, "remove_owner(/a/sub/, @x)")
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("want refusal (exit 2), got %v", err)
	}
	for _, want := range []string{"a/sub/f.go", "a/sub/g.go", "@root", "@other"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q must name %q", err, want)
		}
	}
}

// SPEC R-39/R-6 (pin — passes before and after R-39): the branch that CAN
// delete still deletes. Where the rule lies entirely inside the op's scope, nothing out of scope depends on it, and the
// file loses a line rather than gaining one — narrowing must not become the
// answer everywhere and leave every fleet repo one line longer.
func TestR39_DeletableRuleIsStillDeleted(t *testing.T) {
	tree := []string{"a/f.go", "top.txt"}
	p, err := build(t, "* @root\n/a/ @x\n", tree, plan.Options{OnEmpty: "inherit"},
		"remove_owner(/a/, @x)")
	if err != nil {
		t.Fatalf("deletable inherit removal must not be refused: %v", err)
	}
	if strings.Contains(p.AfterContent, "/a/") {
		t.Errorf("rule /a/ must be deleted, not narrowed:\n%s", p.AfterContent)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	if got := after["a/f.go"].Owners; !reflect.DeepEqual(got, []string{"@root"}) {
		t.Errorf("a/f.go = %v, want {@root}", got)
	}
}
