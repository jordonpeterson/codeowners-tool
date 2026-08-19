package plan_test

import (
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// R-2, shape 4: narrowing must depend on what a scope MATCHES, not on how it is
// spelled.
//
// `build.gradle` and `**/build.gradle` are the same pattern — CODEOWNERS gives a
// slashless pattern basename semantics, so both select every build.gradle at
// every depth. The planner disagrees. It derives the narrowing
// `/.github/**/build.gradle` for the bare spelling and refuses the `**/` one with
// "no sound narrowing pattern is derivable", so the operator's choice of two
// interchangeable spellings decides whether an expressible intent is expressed
// or exits 2.
//
// The repo shape is the common one: a catch-all `*`, with `/.github/` locked to
// a release-engineering team. Adding a build owner to every build.gradle crosses
// that lockdown at `.github/build.gradle`, which is what forces a narrowing
// rather than a plain amend.
//
// basenameGlob rejects any pattern containing "/", so `**/build.gradle` never
// reaches the candidate logic — even though deriveIntersection already strips a
// `**/` prefix when it normalizes RULE patterns.
func TestR2_NarrowingIsIndependentOfScopeSpelling(t *testing.T) {
	tree := []string{
		"build.gradle", "app/build.gradle", "README.md",
		".github/build.gradle", ".github/workflows/ci.yml", ".github/CODEOWNERS",
	}
	const content = "*         @org/everyone\n/.github/ @org/platform\n"

	// Observed output of the bare spelling, which the planner already handles.
	// Both spellings must land here: every build.gradle gains @org/team_a as a
	// CO-owner (INV-1) and nothing else moves (INV-2) — note .github/build.gradle
	// KEEPS @org/platform, so the lockdown is joined, not replaced.
	want := map[string][]string{
		"build.gradle":             {"@org/everyone", "@org/team_a"},
		"app/build.gradle":         {"@org/everyone", "@org/team_a"},
		".github/build.gradle":     {"@org/platform", "@org/team_a"},
		"README.md":                {"@org/everyone"},
		".github/workflows/ci.yml": {"@org/platform"},
		".github/CODEOWNERS":       {"@org/platform"},
	}

	for _, scope := range []string{"build.gradle", "**/build.gradle"} {
		t.Run(scope, func(t *testing.T) {
			p, err := build(t, content, tree, plan.Options{},
				"add_owner("+scope+", @org/team_a)")
			if err != nil {
				t.Fatalf("scope %q selects exactly the files %q does, and that "+
					"spelling is satisfiable, so this intent is expressible: %v",
					scope, "build.gradle", err)
			}
			wantOwners(t, plan.ResolveContent(p.AfterContent, tree), want)
		})
	}
}

// The equivalence the test above rests on: nothing in the planner is allowed to
// treat these two spellings as selecting different files. If this ever fails,
// TestR2_NarrowingIsIndependentOfScopeSpelling is asserting the wrong thing and
// the spelling-sensitivity finding needs rereading, not the matcher fixing.
func TestBasenameSpellingsSelectTheSameFiles(t *testing.T) {
	tree := []string{
		"build.gradle", "app/build.gradle", "app/sub/build.gradle",
		".github/build.gradle", ".github/workflows/build.gradle",
		"README.md", "settings.gradle",
	}
	matched := func(pat string) map[string]bool {
		got := map[string]bool{}
		for path, r := range plan.ResolveContent(pat+" @org/marker\n", tree) {
			if r.Matched {
				got[path] = true
			}
		}
		return got
	}
	bare, star := matched("build.gradle"), matched("**/build.gradle")
	if len(bare) == 0 {
		t.Fatal("bare spelling matched nothing; fixture is wrong")
	}
	for path := range bare {
		if !star[path] {
			t.Errorf("%s matched by \"build.gradle\" but not \"**/build.gradle\"", path)
		}
	}
	for path := range star {
		if !bare[path] {
			t.Errorf("%s matched by \"**/build.gradle\" but not \"build.gradle\"", path)
		}
	}
}
