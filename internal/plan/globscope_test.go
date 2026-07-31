package plan_test

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/plan"
	"github.com/jordonpeterson/codeowners-tool/internal/resolve"
)

// wantOwners asserts the resolved owner set of every tree path, so INV-1 (in
// scope) and INV-2 (out of scope) are checked by the same assertion.
func wantOwners(t *testing.T, after map[string]resolve.Resolution, want map[string][]string) {
	t.Helper()
	for path, exp := range want {
		got := append([]string{}, after[path].Owners...)
		sort.Strings(got)
		w := append([]string{}, exp...)
		sort.Strings(w)
		if !reflect.DeepEqual(got, w) {
			t.Errorf("%s = %v, want %v", path, got, w)
		}
	}
}

// R-2, shape 4: a FILE-GLOB scope crossing anchored directory rules.
//
// The monorepo shape — catch-all `*`, per-app anchored rules, intent "add
// @platform to every *.gradle file". Every intersecting rule has a sound
// narrowing pattern (`*.gradle` for `*`, `/appN/**/*.gradle` for the anchored
// ones), so the intent is expressible without disturbing one out-of-scope
// path. Before the shape-4 derivation the planner refused it outright.
func TestR2_FileGlobScopeAcrossAnchoredRules(t *testing.T) {
	tree := []string{
		"build.gradle", "settings.gradle", "README.md",
		"app1/build.gradle", "app1/src/Main.java",
		"app2/build.gradle", "app2/src/Main.java", "app2/README.md",
	}
	p, err := build(t, "* @org/team-a\n/app1 @org/team-b\n/app2 @org/team-c\n",
		tree, plan.Options{}, "add_owner(*.gradle, @org/team-platform)")
	if err != nil {
		t.Fatalf("expressible intent was refused: %v", err)
	}
	wantOwners(t, plan.ResolveContent(p.AfterContent, tree), map[string][]string{
		// INV-1: every .gradle file gains @org/team-platform as a CO-owner,
		// keeping the owner it already had.
		"build.gradle":      {"@org/team-a", "@org/team-platform"},
		"settings.gradle":   {"@org/team-a", "@org/team-platform"},
		"app1/build.gradle": {"@org/team-b", "@org/team-platform"},
		"app2/build.gradle": {"@org/team-c", "@org/team-platform"},
		// INV-2: @org/team-platform must NOT swallow /app1 or /app2 wholesale
		// — the failure mode a naive "append /app2 @c @platform" produces.
		"README.md":          {"@org/team-a"},
		"app1/src/Main.java": {"@org/team-b"},
		"app2/src/Main.java": {"@org/team-c"},
		"app2/README.md":     {"@org/team-c"},
	})
}

// Shape 4 with the rule written as an UNANCHORED directory (`app2/`, no
// leading slash). Such a rule matches app2/ at any depth, so the narrowing
// must too: `**/app2/**/*.gradle`, not `/app2/**/*.gradle`.
func TestR2_FileGlobScopeAcrossUnanchoredDirRule(t *testing.T) {
	tree := []string{
		"build.gradle",
		"app2/build.gradle", "app2/src/Main.java",
		"vendor/app2/build.gradle", "vendor/app2/src/Main.java",
	}
	p, err := build(t, "* @a\napp2/ @c\n", tree, plan.Options{},
		"add_owner(*.gradle, @plat)")
	if err != nil {
		t.Fatalf("expressible intent was refused: %v", err)
	}
	wantOwners(t, plan.ResolveContent(p.AfterContent, tree), map[string][]string{
		"build.gradle":              {"@a", "@plat"},
		"app2/build.gradle":         {"@c", "@plat"},
		"vendor/app2/build.gradle":  {"@c", "@plat"},
		"app2/src/Main.java":        {"@c"},
		"vendor/app2/src/Main.java": {"@c"},
	})
}

// Shape 4 must narrow to the rule's FULL prefix, not just its first segment:
// a nested anchored rule /app2/legacy needs /app2/legacy/**/*.gradle, so the
// rest of /app2 keeps resolving through the broader rule.
func TestR2_FileGlobScopeAcrossNestedAnchoredRule(t *testing.T) {
	tree := []string{
		"app2/build.gradle", "app2/src/Main.java",
		"app2/legacy/build.gradle", "app2/legacy/Old.java",
	}
	p, err := build(t, "* @a\n/app2 @c\n/app2/legacy @legacy\n", tree,
		plan.Options{}, "add_owner(*.gradle, @plat)")
	if err != nil {
		t.Fatalf("expressible intent was refused: %v", err)
	}
	wantOwners(t, plan.ResolveContent(p.AfterContent, tree), map[string][]string{
		"app2/build.gradle":        {"@c", "@plat"},
		"app2/legacy/build.gradle": {"@legacy", "@plat"},
		"app2/src/Main.java":       {"@c"},
		"app2/legacy/Old.java":     {"@legacy"},
	})
}

// The same derivation serves remove_owner, which shares intersectPattern:
// stripping an owner from just the .gradle files of a directory it otherwise
// still owns. The root build.gradle matters — without a .gradle file outside
// /app2 the scope would be a subset of the rule and shape 1 would carry the
// case, never exercising shape 4.
func TestR2_FileGlobScopeRemoveOwnerAcrossAnchoredRule(t *testing.T) {
	tree := []string{
		"build.gradle", "README.md",
		"app2/build.gradle", "app2/src/Main.java",
	}
	p, err := build(t, "* @a\n/app2 @c @plat\n", tree, plan.Options{},
		"remove_owner(*.gradle, @plat)")
	if err != nil {
		t.Fatalf("expressible intent was refused: %v", err)
	}
	wantOwners(t, plan.ResolveContent(p.AfterContent, tree), map[string][]string{
		"app2/build.gradle":  {"@c"},
		"app2/src/Main.java": {"@c", "@plat"},
		"build.gradle":       {"@a"},
		"README.md":          {"@a"},
	})
}

// Shape 4 is deliberately narrow: it derives `<ruleDir>/**/<glob>` only for a
// single-segment basename glob, so a MULTI-SEGMENT unanchored scope still has
// no derivation and must be refused. The boundary that shape 4 itself owns —
// a derivation that is inexact over the tree — is pinned by
// TestShape4_TreeConfirmationRefusesInexactDerivation.
func TestGate_StillRefusesMultiSegmentGlobScope(t *testing.T) {
	tree := []string{"a/x/doc.md", "a/x/code.go", "b/x/doc.md", "other/doc.md"}
	_, err := build(t, "* @a\n/a @docs\n", tree, plan.Options{},
		"add_owner(*/x/doc.md, @b)")
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("inexpressible intent must be refused (exit 2), got %v", err)
	}
}
