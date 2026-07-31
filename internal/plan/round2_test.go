package plan_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// A rule ending in a `*`-only segment governs a directory's DIRECT CHILDREN,
// so it has no subtree prefix: deriving `**/x/*/` from `**/x/*` yields a
// narrowing matching arbitrarily deep descendants the rule never governed.
// The anchored spellings happen to fail shape 4's tree confirmation, but the
// any-depth ones pass it whenever the tracked tree does not distinguish the
// two — here a nested `x/x/` makes every tracked path agree, the gate proves
// the plan, and exit 0 writes the over-broad line. INV-1/INV-2 hold today and
// break for files added later.
func TestShape4_DirectChildrenRuleHasNoSubtreePrefix(t *testing.T) {
	tree := []string{"README.md", "build.gradle", "x/x/f.gradle", "x/x/Main.java"}
	p, err := build(t, "* @a\n**/x/* @b\n", tree, plan.Options{},
		"add_owner(*.gradle, @plat)")
	var ref *plan.RefusalError
	if errors.As(err, &ref) {
		return // fail-closed is an acceptable outcome
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// `**/x/* @b` matches neither of these, so @b must not reach them.
	future := append(append([]string{}, tree...),
		"x/y/f.gradle",      // sibling dir: not a child of an `x` dir
		"x/x/deep/g.gradle", // grandchild: too deep for `**/x/*`
	)
	after := plan.ResolveContent(p.AfterContent, future)
	for _, path := range []string{"x/y/f.gradle", "x/x/deep/g.gradle"} {
		for _, o := range after[path].Owners {
			if o == "@b" {
				t.Errorf("%s = %v: narrowing granted @b a path `**/x/*` never matched:\n%s",
					path, after[path].Owners, p.AfterContent)
			}
		}
	}
}

// Shape 1 returns the scope pattern verbatim when every in-scope path matches
// the rule — but `subset` is established over the CURRENT tree while the
// pattern is written to a file that outlives it. An UNANCHORED scope matches
// at any depth, so under a rule that does not, verbatim is repo-global. This
// is spelling-independent: it must hold for a globstar-prefixed scope and for
// a wildcard-free basename alike, neither of which the first attempt's
// `ContainsAny(scope, "*?")` guard caught.
func TestShape1_UnanchoredScopeNeverEmittedVerbatimUnderNonUniversalRule(t *testing.T) {
	for _, tc := range []struct{ scope, file string }{
		{"**/*.gradle", "build.gradle"},
		{"build.gradle", "build.gradle"},
	} {
		tree := []string{"app2/" + tc.file, "app2/src/Main.java", "README.md"}
		p, err := build(t, "* @a\n/app2 @c\n", tree, plan.Options{},
			"add_owner("+tc.scope+", @plat)")
		var ref *plan.RefusalError
		if errors.As(err, &ref) {
			continue // fail-closed is acceptable
		}
		if err != nil {
			t.Errorf("scope %q: unexpected error: %v", tc.scope, err)
			continue
		}
		for _, line := range strings.Split(p.AfterContent, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), tc.scope+" ") {
				t.Errorf("scope %q: emitted repo-global rule %q:\n%s",
					tc.scope, strings.TrimSpace(line), p.AfterContent)
			}
		}
		future := append(append([]string{}, tree...), "tools/"+tc.file)
		for _, o := range plan.ResolveContent(p.AfterContent, future)["tools/"+tc.file].Owners {
			if o == "@c" {
				t.Errorf("scope %q: tools/%s inherited /app2's owner @c repo-wide", tc.scope, tc.file)
			}
		}
	}
}

// A rename's R-8 scope must also widen for a removal under --on-empty=inherit.
// Inheritance DELETES a rule, resurrecting whatever shadowed line sat behind
// it, which can hand arbitrary identifiers — including the rename's old one —
// to that rule's paths. `/app/sub @a` is dead while `/app @b` shadows it, so
// @a owns nothing, the rename's scope set is empty, and the R-8 inherit guard
// (which only fires on a non-empty intersection) never runs: the batch plans
// at exit 0 and leaves @b owning paths that remove_owner(/app, @b) just
// removed it from.
func TestR8_InheritRemovalWidensRenameScope(t *testing.T) {
	tree := []string{"app/f.txt", "app/sub/g.txt", "app/sub/deep/h.txt"}
	_, err := build(t, "/app/sub @a\n/app @b\n", tree,
		plan.Options{OnEmpty: "inherit"},
		"remove_owner(/app, @b)", "rename_owner(@a, @b)")
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("inherit-removal batched with a rename is order-dependent and must be refused (R-8), got %v", err)
	}
}

// `ruleIsUniversal` must not claim universality for a rule this repo's matcher
// does not treat as universal. A lone `**` is the one spelling that gets no
// implicit `**/` prefix, so it compiles to `\A.+\z`; Go's `.` excludes "\n"
// while `[^/]` does not, so `**` misses a path with a newline in its last
// segment that `*` and `**/*` both match. The vendored hmarr oracle agrees, so
// the matcher is faithful and must not change — but shape 1 relied on the
// claim to emit an unanchored scope VERBATIM, a line that outlives the tree it
// was proven against. A gradle file added later with a newline in its name
// would be captured by that line although `**` never governed it.
func TestShape1_DoubleStarIsNotUniversalForThisMatcher(t *testing.T) {
	tree := []string{"build.gradle", "README.md", "app1/build.gradle"}
	p, err := build(t, "* @root\n** @org/all\n", tree, plan.Options{},
		"add_owner(*.gradle, @plat)")
	var ref *plan.RefusalError
	if errors.As(err, &ref) {
		return // fail-closed is the acceptable outcome
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, line := range strings.Split(p.AfterContent, "\n") {
		if strings.TrimSpace(line) == "*.gradle @org/all @plat" {
			t.Errorf("emitted the scope verbatim under a rule that is not universal"+
				" for this matcher:\n%s", p.AfterContent)
		}
	}
	future := append(append([]string{}, tree...), "we\nird.gradle")
	got := plan.ResolveContent(p.AfterContent, future)["we\nird.gradle"].Owners
	for _, o := range got {
		if o == "@org/all" {
			t.Errorf("we\\nird.gradle = %v: captured by a line whose rule `**` does not match it", got)
		}
	}
}
