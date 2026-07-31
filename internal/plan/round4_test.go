package plan_test

import (
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// The direct-children guard must not swallow the SUBTREE spelling. `/app2/*`
// governs direct children only, but `/app2/**/*` compiles to
// `\Aapp2(?:/.+)?/[^/]+\z` — the whole subtree, semantically identical to
// `/app2/**`. Rejecting every pattern ending in `/*` therefore refuses an
// intent that both `/app2` and `/app2/**` express fine.
func TestShape4_SubtreeRuleSpelledWithTrailingStar(t *testing.T) {
	tree := []string{
		"root.gradle",
		"app2/build.gradle", "app2/a/b/c/x.gradle", "app2/src/Main.java",
	}
	p, err := build(t, "* @a\n/app2/**/* @c\n", tree, plan.Options{},
		"add_owner(*.gradle, @plat)")
	if err != nil {
		t.Fatalf("subtree rule spelling was refused: %v", err)
	}
	wantOwners(t, plan.ResolveContent(p.AfterContent, tree), map[string][]string{
		"root.gradle":         {"@a", "@plat"},
		"app2/build.gradle":   {"@c", "@plat"},
		"app2/a/b/c/x.gradle": {"@c", "@plat"},
		"app2/src/Main.java":  {"@c"},
	})
}

// Shape 1 needs match(scope) ⊆ match(rule) for ALL trees; universality of the
// rule is merely one sufficient case. A basename scope under a file-type rule
// (`README.md` under `*.md`) satisfies containment structurally — every path
// matching `README.md` ends in `.md` — so the scope verbatim IS the exact
// intersection on every future tree, and refusing it is an avoidable
// capability loss.
func TestShape1_ContainedBasenameScopeUnderFileGlobRule(t *testing.T) {
	tree := []string{"README.md", "docs/guide.md", "src/main.go"}
	p, err := build(t, "* @a\n*.md @docs\n", tree, plan.Options{},
		"add_owner(README.md, @plat)")
	if err != nil {
		t.Fatalf("contained scope was refused: %v", err)
	}
	wantOwners(t, plan.ResolveContent(p.AfterContent, tree), map[string][]string{
		"README.md":     {"@docs", "@plat"},
		"docs/guide.md": {"@docs"},
		"src/main.go":   {"@a"},
	})
	// Containment must hold on a grown tree too.
	future := append(append([]string{}, tree...), "sub/README.md", "sub/other.md")
	after := plan.ResolveContent(p.AfterContent, future)
	wantOwners(t, after, map[string][]string{
		"sub/README.md": {"@docs", "@plat"},
		"sub/other.md":  {"@docs"},
	})
}
