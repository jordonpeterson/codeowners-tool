package plan_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// R-8 must refuse a batch whose result depends on op order. A rename's scope
// was derived from the BEFORE state alone, so an identifier that owns nothing
// yet produced an EMPTY scope set and the commutativity check ran vacuously —
// even though a sibling op in the same batch hands that identifier to paths
// the rename would then rewrite. The two orderings write different files, and
// both are accepted at exit 0: exactly what R-8's own error string promises to
// refuse. (Ownership of /Apps/ ends up @c under one order and @t2 under the
// other; the user asked for @t2.)
func TestR8_RenameScopeMustCoverOwnersGrantedLaterInTheBatch(t *testing.T) {
	tree := []string{"Apps/b.txt", "x/README.md"}
	const content = "*.md @c\n/Apps/ @a\n"
	orders := [][]string{
		{"rename_owner(@t2, @c)", "set_owners(/Apps/, [@t2])"},
		{"set_owners(/Apps/, [@t2])", "rename_owner(@t2, @c)"},
	}
	for _, specs := range orders {
		_, err := build(t, content, tree, plan.Options{}, specs...)
		var inv *plan.InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("batch %v: order-dependent batch must be refused (R-8, exit 3), got %v", specs, err)
		}
	}
}

// A rename batched with an op that does NOT hand it its old identifier stays
// legal — the widening must not turn every batch containing a rename into a
// refusal.
func TestR8_RenameBatchedWithUnrelatedOpStillPlans(t *testing.T) {
	tree := []string{"Apps/b.txt", "x/README.md"}
	_, err := build(t, "*.md @c\n/Apps/ @a\n", tree, plan.Options{},
		"rename_owner(@a, @b)", "add_owner(*.md, @docs)")
	if err != nil {
		t.Fatalf("independent ops must still batch: %v", err)
	}
}

// Shape 1 returns the scope pattern VERBATIM when every in-scope path matches
// the rule. For an unanchored glob scope that emits a repo-global line
// (`*.gradle @c @plat`) sitting under a directory rule: exact over today's
// tracked tree, so the gate passes, but strictly broader than the intersection
// it represents. Every .gradle file added anywhere later silently inherits
// /app2's owner. When an anchored derivation exists it is the tighter of two
// equally-exact patterns and must be preferred.
func TestR2_GlobScopeNarrowingMustNotEmitRepoGlobalRule(t *testing.T) {
	tree := []string{"app2/build.gradle", "app2/src/Main.java", "README.md"}
	p, err := build(t, "* @a\n/app2 @c\n", tree, plan.Options{},
		"add_owner(*.gradle, @plat)")
	if err != nil {
		t.Fatalf("expressible intent was refused: %v", err)
	}
	for _, line := range strings.Split(p.AfterContent, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "*.gradle ") {
			t.Errorf("emitted repo-global rule %q; the intersection of an unanchored glob"+
				" scope with a directory rule must be anchored to that directory:\n%s",
				strings.TrimSpace(line), p.AfterContent)
		}
	}
	// A .gradle file added outside /app2 later must not inherit @c.
	future := append(append([]string{}, tree...), "tools/build.gradle")
	for _, o := range plan.ResolveContent(p.AfterContent, future)["tools/build.gradle"].Owners {
		if o == "@c" {
			t.Errorf("tools/build.gradle inherited /app2's owner @c repo-wide")
		}
	}
}
