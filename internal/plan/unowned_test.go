package plan_test

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// These tests cover R-25 (on_unowned: own | skip).
//
// The distinction they exist to protect is the one CODEOWNERS itself hides.
// `add_owner(*.gradle, @platform)` reads like one intent, but it does two
// different things depending on the path it lands on: on a path some team
// already owns it adds a CO-owner, and on a path nobody owns it writes a rule
// that makes @platform the SOLE owner. The second is not a smaller version of
// the first — it converts a file anyone could merge into a file that now
// requires one specific team's approval, which is a permissions change nobody
// asked for, applied to whichever paths happened to be uncovered that day.
//
// on_unowned=skip is how a caller says "co-owner only, never sole owner". It
// is implemented as a SCOPE NARROWING rather than a case inside synthesis:
// paths without an owner are removed from the op's scope set before anything
// is planned, which makes INV-2 — already proven over the real tree by the
// existing gate — the thing that guarantees they do not move. These tests
// check the guarantee at the resolved-ownership level for that reason, not by
// asserting on which lines got written.

// unownedOp is an op string plus the on_unowned policy to attach to it, which
// ops.Parse never sets (R-25: the zero value must preserve the behavior that
// predates the field).
func unownedOp(t *testing.T, spec, mode string) []ops.Op {
	t.Helper()
	parsed, err := ops.Parse(spec)
	if err != nil {
		t.Fatalf("op %q: %v", spec, err)
	}
	parsed.OnUnowned = mode
	return []ops.Op{parsed}
}

// ownersOf returns a path's resolved owners in the planned content, sorted so
// comparisons do not depend on the order the synthesizer happened to emit.
func ownersOf(p *plan.Plan, tree []string, path string) []string {
	got := plan.ResolveContent(p.AfterContent, tree)[path].Owners
	out := append([]string{}, got...)
	sort.Strings(out)
	return out
}

// SPEC R-25: the DEFAULT is unchanged. With on_unowned absent, an in-scope
// path that no rule matches is given the op's owner and that owner becomes its
// sole owner — exactly what the tool did before this field existed.
//
// This is the compatibility test for the whole feature. Every op parsed from
// --op carries the zero value, so if this ever changes, the field has silently
// altered the behavior of every policy already in production.
func TestR25_DefaultStillOwnsUnownedPaths(t *testing.T) {
	tree := []string{"owned/build.gradle", "loose/build.gradle"}
	p, err := plan.Build([]byte("/owned/ @org/eng\n"), tree,
		unownedOp(t, "add_owner(*.gradle, @platform)", ""), plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ownersOf(p, tree, "loose/build.gradle"); !reflect.DeepEqual(got, []string{"@platform"}) {
		t.Errorf("loose/build.gradle owners = %v, want {@platform}: the default must keep taking ownership of unowned in-scope paths", got)
	}
	if got := ownersOf(p, tree, "owned/build.gradle"); !reflect.DeepEqual(got, []string{"@org/eng", "@platform"}) {
		t.Errorf("owned/build.gradle owners = %v, want {@org/eng, @platform}", got)
	}
}

// SPEC R-25: on_unowned=skip adds the owner where somebody already owns the
// path, and leaves paths nobody owns exactly as they were.
//
// Both halves matter. Skipping the unowned path is the point of the field, but
// an implementation that skipped the whole op whenever ANY in-scope path was
// unowned would also pass a test that only checked the first half — and would
// silently do nothing on the repos that need the change most.
func TestR25_SkipCoOwnsWithoutManufacturingOwnership(t *testing.T) {
	tree := []string{"a/build.gradle", "b/build.gradle", "loose/build.gradle", "a/main.go"}
	p, err := plan.Build([]byte("/a/ @org/a-team\n/b/ @org/b-team @org/eng\n"), tree,
		unownedOp(t, "add_owner(*.gradle, @platform)", ops.UnownedSkip), plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]string{
		"a/build.gradle":     {"@org/a-team", "@platform"},
		"b/build.gradle":     {"@org/b-team", "@org/eng", "@platform"},
		"a/main.go":          {"@org/a-team"}, // out of scope entirely (INV-2)
		"loose/build.gradle": nil,             // in scope by pattern, dropped by R-25
	} {
		got := ownersOf(p, tree, path)
		if len(want) == 0 {
			if len(got) != 0 {
				t.Errorf("%s owners = %v, want NOBODY: on_unowned=skip must not hand an unowned path a sole owner", path, got)
			}
			continue
		}
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s owners = %v, want %v", path, got, want)
		}
	}
}

// SPEC R-25: a rule with an EMPTY owner set counts as unowned.
//
// This is the case a "did any rule match?" test gets wrong, and it is not
// hypothetical: `--on-empty unowned` (R-6) deliberately writes a pattern with
// no owners as GitHub's sanctioned substitute for `!` negation. Such a path IS
// matched, so a Matched check calls it owned, and the op then amends that rule
// and hands the owner sole approval rights over exactly the paths somebody
// went out of their way to leave unowned. The test is on the owner COUNT.
func TestR25_SkipTreatsEmptyOwnerSetAsUnowned(t *testing.T) {
	tree := []string{"x/build.gradle", "y/build.gradle"}
	content := "/x/\n/y/ @org/y-team\n" // "/x/" matches, and lists nobody
	p, err := plan.Build([]byte(content), tree,
		unownedOp(t, "add_owner(*.gradle, @platform)", ops.UnownedSkip), plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ownersOf(p, tree, "x/build.gradle"); len(got) != 0 {
		t.Errorf("x/build.gradle owners = %v, want NOBODY: a rule with an empty owner set leaves the path with no one who can approve it, which is the state on_unowned=skip exists not to fill in", got)
	}
	if got := ownersOf(p, tree, "y/build.gradle"); !reflect.DeepEqual(got, []string{"@org/y-team", "@platform"}) {
		t.Errorf("y/build.gradle owners = %v, want {@org/y-team, @platform}", got)
	}
}

// SPEC R-25: when every in-scope path is unowned, the op is SKIPPED with a
// reason of its own — not an error, and not confusable with R-21's skip.
//
// Across a fleet this is an ordinary answer: "this repo owns none of its
// *.gradle files" is the policy working, not the repo being broken. Failing
// here would halt a 100-repo rollout on a repo that is behaving exactly as
// intended. The reason string is load-bearing too — with two ways to be
// skipped, an operator grepping records has to be able to tell which happened.
func TestR25_SkipWithNothingOwnedIsANoOpNotAnError(t *testing.T) {
	tree := []string{"build.gradle", "docs/a.md"}
	p, err := plan.Build([]byte("/docs/ @org/docs\n"), tree,
		unownedOp(t, "add_owner(*.gradle, @platform)", ops.UnownedSkip), plan.Options{})
	// NoOpError is the "nothing to change" signal, which is what an all-skipped
	// batch is — the same result R-21's zero-match skip produces, and the CLI
	// renders both as `skipped` at exit 0. What must NOT happen is a refusal or
	// an invalid-input error, either of which halts a fleet rollout.
	var noop *plan.NoOpError
	if err != nil && !errors.As(err, &noop) {
		t.Fatalf("all-unowned scope must be a no-op, not a failure: %v", err)
	}
	if p == nil {
		t.Fatal("a skipped op must still return a populated plan, or a fleet record cannot say which repos were skipped")
	}
	if len(p.OpResults) != 1 {
		t.Fatalf("OpResults = %d, want 1: a skipped op must still report itself", len(p.OpResults))
	}
	r := p.OpResults[0]
	if r.Status != "skipped" {
		t.Errorf("status = %q, want %q", r.Status, "skipped")
	}
	if !strings.Contains(r.Reason, "R-25") || !strings.Contains(r.Reason, "no owner") {
		t.Errorf("reason = %q, want it to name R-25 and say the paths have no owner, so it cannot be mistaken for R-21's zero-match skip", r.Reason)
	}
	if p.Diff != "" {
		t.Errorf("diff = %q, want empty: a skipped op writes nothing", p.Diff)
	}
}

// SPEC R-25: skip narrows the scope, it does not suppress R-5. A scope that
// matches zero tracked files is still a dead rule and still refuses.
//
// The two conditions look alike from the outside — both end with an empty
// scope set — but they mean opposite things. "The pattern is wrong" is a typo
// worth failing on; "the files exist and nobody owns them" is the policy
// declining to act. Collapsing them would make on_unowned=skip a way to
// silence R-5 by accident.
func TestR25_SkipDoesNotSuppressTheZeroMatchRefusal(t *testing.T) {
	tree := []string{"a/build.gradle"}
	_, err := plan.Build([]byte("/a/ @org/a-team\n"), tree,
		unownedOp(t, "add_owner(**/*.kts, @platform)", ops.UnownedSkip), plan.Options{})
	if err == nil {
		t.Fatal("a scope matching zero tracked files must still refuse under R-5, even with on_unowned=skip")
	}
	if !strings.Contains(err.Error(), "R-5") {
		t.Errorf("error = %q, want the R-5 dead-rule refusal", err)
	}
}

// SPEC R-25: running the same skip op twice is a no-op the second time.
//
// Idempotence is what makes this safe on a schedule. An implementation that
// re-derived a narrowing rule each run would grow the file without bound on
// exactly the repos the policy is scheduled against.
func TestR25_SkipIsIdempotent(t *testing.T) {
	tree := []string{"a/build.gradle", "loose/build.gradle"}
	first, err := plan.Build([]byte("/a/ @org/a-team\n"), tree,
		unownedOp(t, "add_owner(*.gradle, @platform)", ops.UnownedSkip), plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = plan.Build([]byte(first.AfterContent), tree,
		unownedOp(t, "add_owner(*.gradle, @platform)", ops.UnownedSkip), plan.Options{})
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("second run err = %v, want NoOpError: the op is already satisfied", err)
	}
}

// SPEC R-25: this is WHY on_unowned is confined to add_owner
// (ops.CanCarryOnUnowned), and the reason is worth a test rather than only a
// comment, because "we rejected it for safety" and "it would actually break"
// are very different claims.
//
// set_owners writes one rule spelled with the scope pattern VERBATIM. Narrow
// its scope set and the pattern no longer describes it: the rule recaptures
// the paths the narrowing just excluded and hands them the new owner set. The
// gate catches that — nothing unsafe can ship — but the verdict depends on the
// repository, so the flag would work until it mattered and then refuse mid
// rollout. Both boundaries reject it up front instead; if either rejection is
// ever relaxed, this test says what the planner will do about it.
func TestR25_SetOwnersUnderSkipIsRefusedByTheGate(t *testing.T) {
	tree := []string{"a/build.gradle", "loose/build.gradle"}
	_, err := plan.Build([]byte("/a/ @org/a-team\n"), tree,
		unownedOp(t, "set_owners(*.gradle, [@platform])", ops.UnownedSkip), plan.Options{})
	if err == nil {
		t.Fatal("set_owners under on_unowned=skip must not silently succeed: its rule would recapture the excluded paths")
	}
	if !strings.Contains(err.Error(), "INV-2") || !strings.Contains(err.Error(), "loose/build.gradle") {
		t.Errorf("error = %q, want an INV-2 refusal naming the path the narrowing excluded", err)
	}
}
