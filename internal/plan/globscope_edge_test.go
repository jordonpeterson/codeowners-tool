package plan_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// Shape 4 must read a rule's ANCHORING from its original spelling. `app2/**`
// carries an interior slash, so — exactly like `/app2/**`, and unlike `app2/`
// — it is root-anchored and never governs `vendor/app2/`. Deriving the prefix
// by trimming the `/**` suffix first destroys the very slash that decides
// this, making the rule look single-segment and any-depth. The narrowing rule
// then governs MORE than the rule it narrows: today's tree may hide that (the
// confirmation loop only sees tracked paths), but the moment anyone adds a
// matching file under a nested same-named directory the extra owner appears.
func TestShape4_TrailingGlobstarRuleIsRootAnchored(t *testing.T) {
	tree := []string{
		"root.gradle",
		"app2/build.gradle", "app2/Main.java",
		"vendor/app2/Main.java", "vendor/app2/pom.xml",
	}
	const content = "* @a\napp2/** @c\n"

	// Precondition: this repo's own resolver agrees `app2/**` is root-anchored.
	if got := plan.ResolveContent(content, tree)["vendor/app2/Main.java"].Owners; len(got) != 1 || got[0] != "@a" {
		t.Fatalf("precondition: vendor/app2/Main.java = %v, want [@a]", got)
	}

	p, err := build(t, content, tree, plan.Options{}, "add_owner(*.gradle, @plat)")
	if err != nil {
		t.Fatalf("expressible intent was refused: %v", err)
	}
	if strings.Contains(p.AfterContent, "**/app2/") {
		t.Errorf("derived an any-depth prefix for the root-anchored rule %q:\n%s", "app2/**", p.AfterContent)
	}
	// The narrowing must govern nothing `app2/**` did not: a .gradle file
	// added under vendor/app2 later must not pull in @c.
	future := append(append([]string{}, tree...), "vendor/app2/build.gradle")
	for _, o := range plan.ResolveContent(p.AfterContent, future)["vendor/app2/build.gradle"].Owners {
		if o == "@c" {
			t.Errorf("the inserted rule granted @c a tree that `app2/**` never matched")
		}
	}
	wantOwners(t, plan.ResolveContent(p.AfterContent, tree), map[string][]string{
		"root.gradle":           {"@a", "@plat"},
		"app2/build.gradle":     {"@c", "@plat"},
		"app2/Main.java":        {"@c"},
		"vendor/app2/Main.java": {"@a"},
		"vendor/app2/pom.xml":   {"@a"},
	})
}

// The mirror of the above: a `**/`-prefixed MULTI-segment rule really is
// any-depth, and stripping the `**/` before classifying re-anchors it at the
// repo root. That under-derives — the narrowing misses the nested copies the
// rule actually governs — so the confirmation loop rejects it and an
// expressible intent is refused.
func TestShape4_LeadingGlobstarRuleStaysAnyDepth(t *testing.T) {
	tree := []string{
		"root.gradle",
		"a/b/build.gradle", "a/b/Main.java",
		"vendor/a/b/build.gradle", "vendor/a/b/Main.java",
	}
	p, err := build(t, "* @x\n**/a/b @ab\n", tree, plan.Options{},
		"add_owner(*.gradle, @plat)")
	if err != nil {
		t.Fatalf("expressible intent was refused: %v", err)
	}
	wantOwners(t, plan.ResolveContent(p.AfterContent, tree), map[string][]string{
		"root.gradle":             {"@x", "@plat"},
		"a/b/build.gradle":        {"@ab", "@plat"},
		"vendor/a/b/build.gradle": {"@ab", "@plat"},
		"a/b/Main.java":           {"@ab"},
		"vendor/a/b/Main.java":    {"@ab"},
	})
}

// The tree-confirmation loop in globScopeIntersect is the whole safety
// argument for shape 4, and nothing pinned it: delete it and the suite stayed
// green. This is the case that needs it. Rule `/app*` matches the root FILE
// `apple.gradle` as well as the directory `apps/`, so `apple.gradle` is inside
// (scope ∩ rule) — but the derived `/app*/**/*.gradle` cannot match a root
// file. The derivation is inexact, so the planner must REFUSE rather than
// write a rule that silently drops an in-scope path.
func TestShape4_TreeConfirmationRefusesInexactDerivation(t *testing.T) {
	tree := []string{
		"apple.gradle",
		"apps/build.gradle", "apps/Main.java",
		// A .gradle file OUTSIDE /app* keeps the scope from being a subset of
		// the rule, so the derivation falls through to shape 4 rather than
		// being short-circuited by shape 1.
		"other/x.gradle", "other/README.md",
	}
	_, err := build(t, "* @a\n/app* @c\n", tree, plan.Options{},
		"add_owner(*.gradle, @plat)")
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("an inexact shape-4 derivation must be refused (exit 2), got %v", err)
	}
}

// A DIRECTORY whose own name matches the scope glob. `*.gradle` matches the
// path segment `gen.gradle`, so every file beneath it is in scope — including
// files that are not themselves .gradle. Surprising, but it is what GitHub
// resolves, so the plan must follow the resolver rather than the intuition
// that "*.gradle means .gradle files".
func TestShape4_DirectoryNameMatchingScopeGlob(t *testing.T) {
	tree := []string{
		"app2/gen.gradle/out.txt", "app2/gen.gradle/notes.md",
		"app2/build.gradle", "app2/Main.java",
	}
	const content = "* @a\n/app2 @c\n"
	before := plan.ResolveContent(content, tree)
	inScope := before["app2/gen.gradle/out.txt"].Matched

	p, err := build(t, content, tree, plan.Options{}, "add_owner(*.gradle, @plat)")
	if err != nil {
		t.Fatalf("expressible intent was refused: %v", err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	// Whatever the resolver says about the directory, the .gradle FILE and the
	// non-.gradle sibling must land on opposite sides of the change.
	wantOwners(t, after, map[string][]string{
		"app2/build.gradle": {"@c", "@plat"},
		"app2/Main.java":    {"@c"},
	})
	t.Logf("directory-matching-glob in scope=%v; app2/gen.gradle/out.txt = %v",
		inScope, after["app2/gen.gradle/out.txt"].Owners)
}
