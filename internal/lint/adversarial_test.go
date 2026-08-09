package lint_test

// Regressions from the adversarial review of the first `audit --lint` commit.
//
// Every case below is a write the tool actually performed, at exit 0, with a
// message asserting it was safe. They are grouped here rather than folded into
// the tables above because the shared property is not a feature — it is that
// each one PASSED a gate. A test that only states the intended behavior would
// have gone green against the broken code too; these state the exploit.
//
// SPEC R-5 (adapted): a proof that examined zero paths is not a proof. Lint
// refuses --remove-stale-paths against an empty tree for the same reason
// plan.Build refuses a zero-match scope.
// SPEC R-11: staleness is judged against what exists, which is the committed
// tree AND the checkout — not the committed tree alone.
// SPEC R-12: a team 404 is definitive only for a token that can see every team
// in the org. Only an org owner can.

import (
	"errors"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/lint"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// Two rules written on one line must never be fused into one owner.
//
// `/src @alice /docs @bob` is a line somebody meant as two rules. It is invalid
// — `/docs` is not an owner token — so lint looks at it, and the original merge
// rule accepted the join because `/docs` starts with a slash: it produced
// `/src @alice/docs @bob`, handing `@bob` every file under `/src` and quietly
// leaving `/docs` to the catch-all. Exit 0, "applied".
//
// The shape is indistinguishable from `@org /team`, a real handle with a space
// in it, which is why the fix is to repair NEITHER: a run may only begin at a
// token that is not already a valid owner. Ambiguity gets reported, not guessed.
func TestRepair_NeverFusesTwoRulesWrittenOnOneLine(t *testing.T) {
	const before = "* @org/everyone\n/src @alice /docs @bob\n"
	v := liveOrg("org/everyone")
	v.users["alice"] = true
	v.users["bob"] = true

	res, err := lint.Build([]byte(before), []string{"src/a.go", "docs/b.md"}, v, lint.Options{})
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("err = %v, want NoOpError — there is nothing here lint may safely repair", err)
	}
	if got := res.Plan.AfterContent; got != before {
		t.Errorf("file was rewritten:\n before %q\n after  %q", before, got)
	}
	if n := res.CountUnrepairable(); n != 1 {
		t.Errorf("unrepairable lines = %d, want 1 — the line must be REPORTED, not silently left", n)
	}
	// The owner that the broken merge would have invented.
	if strings.Contains(res.Plan.AfterContent, "@alice/docs") {
		t.Error("invented owner @alice/docs")
	}
}

// The same fusion, with an org the token really can enumerate.
//
// The first exploit was partly masked by luck: `ProbeOrg("alice")` 404s, so the
// run went inconclusive before the damage showed. With an org that exists —
// the repository's own org, the one a CI token can always see — nothing masks
// it, and the merge also manufactured a duplicate owner.
func TestRepair_NeverFusesUsingAnOrgTheTokenCanSee(t *testing.T) {
	const before = "* @acme/everyone\n/src @acme /docs @acme/docs\n"
	v := liveOrg("acme/everyone", "acme/docs")

	res, err := lint.Build([]byte(before), []string{"src/a.go", "docs/b.md"}, v, lint.Options{})
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("err = %v, want NoOpError", err)
	}
	if got := res.Plan.AfterContent; got != before {
		t.Errorf("file was rewritten:\n before %q\n after  %q", before, got)
	}
}

// A merge that reassembles an owner the line already names is refused.
//
// `/x @ org/a @org/a` reassembles into `@org/a @org/a`: a rule naming one team
// twice. It conserves bytes and passes every other check, so only an explicit
// duplicate guard catches it. Fuzzing turned up 539 distinct inputs of this
// shape.
func TestRepair_RefusesAMergeThatDuplicatesAnExistingOwner(t *testing.T) {
	if got, ok := lint.RepairLine("/x @ org/a @org/a"); ok {
		t.Errorf("RepairLine repaired to %q; want refusal — the merge produces a rule listing one team twice", got)
	}
}

// A long line must not take quadratic time.
//
// `joinable` accepts any token starting with "/", so a line of the shape
// `/p @ /aaa /aaa …` drove the scan to the end of the token list, doing an O(n)
// concatenation and an O(n) regex match at every step: 32k tokens took 2.8s and
// 128k took 30s, all of it spent to conclude the line cannot be repaired. One
// garbage file would stall a fleet run. A handle is at most four tokens, so the
// run is capped there.
func TestRepair_LongLineDoesNotBlowUp(t *testing.T) {
	line := "/p @" + strings.Repeat(" /aaaaaaaaaa", 20000)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if got, ok := lint.RepairLine(line); ok {
			t.Errorf("RepairLine claimed a repair: %q", got)
		}
	}()
	// A cap makes this linear; without one it does not finish inside the test
	// timeout at all, so simply completing is the assertion.
	<-done
}

// --remove-stale-paths against an empty tree must refuse, not empty the file.
//
// Staleness asks which rules match nothing. Over an empty tree that is every
// rule, and the gate cannot object because it iterates the tree and therefore
// iterates nothing: the run deleted every rule, reported no ownership rows, and
// exited 0. An orphan branch, an --allow-empty initial commit, or a tag on an
// empty tree all reach it.
func TestStalePaths_RefuseWhenTheTreeIsEmpty(t *testing.T) {
	const before = "# ownership\n* @org/everyone\n/src @org/backend\n"
	v := liveOrg("org/everyone", "org/backend")

	_, err := lint.Build([]byte(before), nil, v, lint.Options{RemoveStalePaths: true})
	var invalid *plan.InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v (%T), want *plan.InvalidError — an empty tree cannot distinguish a dead rule from a tree the tool cannot see", err, err)
	}
}

// A directory that exists on disk but is not committed is not stale.
//
// Staleness was judged against the tree at --branch while the edit lands on the
// WORKING-TREE file. So a rule for a directory the developer just created —
// present in the checkout, not yet committed — read as "matches zero tracked
// files" and its owners were deleted, at exit 0, with the message "deleting it
// changes no ownership". The directory was sitting right there.
func TestStalePaths_AnUncommittedButPresentPathIsNotStale(t *testing.T) {
	const before = "* @org/everyone\n/src/ @org/backend\n"
	v := liveOrg("org/everyone", "org/backend")

	// Committed tree has no src/; the checkout does.
	res, err := lint.Build([]byte(before), []string{".github/CODEOWNERS", "README.md"}, v, lint.Options{
		RemoveStalePaths: true,
		WorkTree:         []string{".github/CODEOWNERS", "README.md", "src/a.go"},
	})
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("err = %v, want NoOpError — nothing here is stale", err)
	}
	if got := res.Plan.AfterContent; got != before {
		t.Errorf("deleted a rule for a directory that exists on disk:\n before %q\n after  %q", before, got)
	}
	// And the same rule IS stale once it is gone from the checkout too.
	res2, err2 := lint.Build([]byte(before), []string{".github/CODEOWNERS", "README.md"}, v, lint.Options{
		RemoveStalePaths: true,
		WorkTree:         []string{".github/CODEOWNERS", "README.md"},
	})
	if err2 != nil {
		t.Fatalf("err = %v, want the stale rule deleted", err2)
	}
	if got := res2.Plan.AfterContent; got != "* @org/everyone\n" {
		t.Errorf("after = %q, want the stale rule gone", got)
	}
}

// A team 404 does not authorize a deletion unless the token could have seen the
// team.
//
// GET /orgs/{org}/teams/{slug} returns 404 for a team that was deleted AND for
// a SECRET team the caller is not a member of. ProbeOrg does not separate them:
// it proves the token can call the endpoint, not that it can see what is behind
// it. So a scheduled lint with an ordinary org-member token would strip every
// secret team from CODEOWNERS, and the diff would look exactly like the tidy-up
// it claimed to be. Only an org owner sees secret teams.
func TestTeamNotFound_IsInconclusiveUnlessTheTokenOwnsTheOrg(t *testing.T) {
	const before = "* @org/everyone\n/x @org/secret\n"
	v := liveOrg("org/everyone") // org/secret is absent → 404
	v.notAdmin["org"] = true

	res, err := lint.Build([]byte(before), []string{"x/a.go"}, v, lint.Options{OnEmpty: "unowned"})
	var inc *lint.InconclusiveError
	if !errors.As(err, &inc) {
		t.Fatalf("err = %v (%T), want *lint.InconclusiveError — a non-owner cannot tell a deleted team from a secret one", err, err)
	}
	if res != nil {
		t.Error("a result was returned alongside the refusal; nothing may be written")
	}
	if !strings.Contains(strings.Join(inc.Reasons, " "), "org-owner") {
		t.Errorf("reasons = %v, want the operator told how to get this decided", inc.Reasons)
	}
}

// ...and an org-owner token still gets the removal, so the guard costs nothing
// on the path it is meant to protect.
func TestTeamNotFound_AnOrgOwnerTokenStillRemoves(t *testing.T) {
	const before = "* @org/everyone\n/x @org/gone\n"
	v := liveOrg("org/everyone") // org/gone is absent → 404; token IS an owner

	res, err := lint.Build([]byte(before), []string{"x/a.go"}, v, lint.Options{OnEmpty: "unowned"})
	if err != nil {
		t.Fatalf("err = %v, want the dead team removed", err)
	}
	if got := res.Plan.AfterContent; got != "* @org/everyone\n/x\n" {
		t.Errorf("after = %q, want /x left with no owners", got)
	}
	if !v.asked("ViewerIsOrgAdmin(org)") {
		t.Error("the ownership question was never asked; the 404 was treated as definitive on its own")
	}
}

// The inherit escape hatch is scoped to the paths the deleted rule was winning.
//
// The gate accepts a divergence from the pure owner-set transform under
// --on-empty=inherit, because deleting a rule resurrects whatever sat behind
// it. That acceptance was keyed on a whole-run counter: one inherit-delete
// anywhere disarmed the ownership comparison for EVERY path in the file, for
// any cause. plan.synthRemove narrows its equivalent to one op's scope, and its
// comment records that the broader form "weakened the gate".
func TestInheritEscapeHatch_IsScopedToTheDeletedRulesPaths(t *testing.T) {
	// /a is inherit-deleted (its only owner is dead) and falls through to *;
	// /b is untouched and must still be compared strictly.
	const before = "* @org/everyone\n/a/ @org/gone\n/b/ @org/everyone\n"
	v := liveOrg("org/everyone")

	res, err := lint.Build([]byte(before), []string{"a/x.go", "b/y.go"}, v, lint.Options{OnEmpty: "inherit"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := res.Plan.AfterContent; got != "* @org/everyone\n/b/ @org/everyone\n" {
		t.Errorf("after = %q", got)
	}
	// a/x.go moved from the dead team to the catch-all; b/y.go did not move.
	row := rowFor(t, res.Plan, "a/x.go")
	if !resolveEqual(row.After, []string{"@org/everyone"}) {
		t.Errorf("a/x.go after = %v, want the inherited catch-all", row.After)
	}
	for _, r := range res.Plan.Rows {
		if r.Path == "b/y.go" {
			t.Errorf("b/y.go changed owners (%v → %v); the inherit hatch must not cover it", r.Before, r.After)
		}
	}
}

func resolveEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
