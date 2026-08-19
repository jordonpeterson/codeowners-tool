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
	if n := res.NeedsHuman(); n != 1 {
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

// The fusion defect, one space to the right of where it was first fixed.
//
// The first fix guarded only the FIRST token of a merge run, so
// `/src @alice /docs @bob` was refused while `/src @ alice /docs @bob` — the
// same line with one more space — still fused into `@alice/docs`, handed
// `@bob`'s directory to an owner nobody typed, and exited 0. A second review
// reproduced it in 178 of 200,000 generated files.
//
// Brokenness is a property of the first join, not of the whole run: after
// `@`+`alice` the accumulator is `@alice`, an ordinary owner, and `@alice` +
// `/docs` is byte-for-byte the ambiguity the package already refuses. The pair
// below must therefore have the SAME outcome — that is the assertion, more than
// either case individually.
func TestRepair_ABrokenStartDoesNotLicenseTheNextJoin(t *testing.T) {
	pairs := [][2]string{
		{"/src @alice /docs @bob", "/src @ alice /docs @bob"},
		{"/src @org /team", "/src @ org /team"},
	}
	for _, p := range pairs {
		_, okGuarded := lint.RepairLine(p[0])
		got, okBroken := lint.RepairLine(p[1])
		if okGuarded != okBroken {
			t.Errorf("the same ambiguity decided two ways:\n  %q -> ok=%v\n  %q -> ok=%v (%q)",
				p[0], okGuarded, p[1], okBroken, got)
		}
		if okBroken {
			t.Errorf("fused %q into %q", p[1], got)
		}
	}
	// Every shape a second review found in the wild.
	for _, in := range []string{
		"* @ alice /docs @bob",
		"/a/b a@b.com @ team /x",
		"/src/ @alice @ org /x",
		"/a/b @ team /docs @bob @alice",
	} {
		if got, ok := lint.RepairLine(in); ok {
			t.Errorf("fused %q into %q", in, got)
		}
	}
}

// ...and the four-token shattering still repairs, because a BARE "/" is the one
// token that cannot be anything else — not a pattern anybody writes, not an
// owner. That single exception is the whole difference between the rule that
// works and the rule that fused.
func TestRepair_TheFourTokenShatteringStillWorks(t *testing.T) {
	for in, want := range map[string]string{
		"/x/ @ org/team":   "/x/ @org/team",
		"/x/ @org/ team":   "/x/ @org/team",
		"/x/ @ org / team": "/x/ @org/team",
		"/x/ @ alice":      "/x/ @alice",
	} {
		got, ok := lint.RepairLine(in)
		if !ok || got != want {
			t.Errorf("RepairLine(%q) = %q, ok=%v; want %q", in, got, ok, want)
		}
	}
}

// Property: a merge may only ever remove whitespace, and only from inside one
// handle. Stated over random token lists, because the two spellings we now know
// about are not the interesting ones — the next variant is.
//
// The invariant that catches every variant at once: concatenating the output
// must equal concatenating the input (nothing added, nothing lost), and every
// token the merge PRODUCED must be a valid @handle. A fusion like `@alice/docs`
// satisfies the first and is caught by the run-start rule; a dropped token
// fails the first outright.
func TestRepairHandle_ConservesBytesAndProducesOnlyHandles(t *testing.T) {
	alphabet := []string{"@", "/", "@a", "@org", "@org/", "org", "team", "/team", "/docs", "@b", "a@b.com", "x", "@org/team"}
	// Deterministic pseudo-random walk: a fixed sequence beats a seed nobody
	// records, and this runs on every CI push.
	next := uint64(1)
	rnd := func(n int) int {
		next = next*6364136223846793005 + 1442695040888963407
		return int((next >> 33) % uint64(n))
	}
	for i := 0; i < 200000; i++ {
		toks := make([]string, 1+rnd(5))
		for j := range toks {
			toks[j] = alphabet[rnd(len(alphabet))]
		}
		in := append([]string(nil), toks...)
		out, changed := lint.RepairHandle(toks)
		if strings.Join(out, "") != strings.Join(in, "") {
			t.Fatalf("bytes not conserved: %v -> %v", in, out)
		}
		if !changed {
			continue
		}
		// Any token that is not one of the inputs was produced by a merge, and
		// a merge may only ever produce a valid handle.
		was := map[string]bool{}
		for _, tk := range in {
			was[tk] = true
		}
		for _, tk := range out {
			if !was[tk] && !strings.HasPrefix(tk, "@") {
				t.Fatalf("merge produced a non-handle %q from %v", tk, in)
			}
		}
	}
}

// Adding a conservative cleanup flag must never make the tool do LESS.
//
// Sparing a case-typo rule skipped the dead-owner removal for that line, so the
// end-of-run gate found the dead owner still present and refused the WHOLE run
// — exit 2, nothing written, including an unrelated and perfectly safe repair
// elsewhere in the file. Every file with both a case typo and a dead owner was
// permanently un-lintable under the flag, and the message read like a tool
// crash.
func TestStalePaths_SparingACaseTypoStillCleansThatLine(t *testing.T) {
	const before = "* @org/all\n/Src/ @org/dead\n/other/ @ org/all\n"
	tree := []string{"src/a.go", "other/b.go"}
	v := liveOrg("org/all")

	res, err := lint.Build([]byte(before), tree, v, lint.Options{
		RemoveStalePaths: true, WorkTree: tree, OnEmpty: "unowned",
	})
	if err != nil {
		t.Fatalf("Build refused a file it can fix: %v", err)
	}
	if want := "* @org/all\n/Src/\n/other/ @org/all\n"; res.Plan.AfterContent != want {
		t.Errorf("after = %q, want %q", res.Plan.AfterContent, want)
	}
	// The flag must not subtract the unrelated repair either.
	if n := len(actionsOfKind(res, lint.ActionRepairOwner)); n != 1 {
		t.Errorf("repair actions = %d, want 1 — the unrelated fix was lost", n)
	}
}

// --remove-stale-paths without a working-tree list is refused.
//
// Options.WorkTree is documented as required for stage 3, and nothing enforced
// it: omitting the field silently reinstated the exact write that judging
// staleness against the committed tree alone produces. One caller away from the
// whole protection being off.
func TestStalePaths_RefuseWithoutAWorkingTreeList(t *testing.T) {
	v := liveOrg("org/everyone", "org/backend")
	_, err := lint.Build([]byte("* @org/everyone\n/src/ @org/backend\n"),
		[]string{".github/CODEOWNERS", "README.md"}, v, lint.Options{RemoveStalePaths: true})
	var invalid *plan.InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v (%T), want *plan.InvalidError", err, err)
	}
}

// Owner identity is case-folded for the lookup, because GitHub's is.
//
// `@Org/Team` and `@org/team` are one owner to GitHub and were two to lint: two
// lookups, two cache keys, two entries in the dead set. If the API is
// case-sensitive on the slug, every capitalised team handle 404s and gets
// DELETED — the same "a finding under audit is a write under lint" asymmetry
// that justified the org-owner gate. Folding the lookup is safe either way.
func TestOwners_CaseFoldedForLookupButNotRewritten(t *testing.T) {
	v := liveOrg("org/team")
	res, err := lint.Build([]byte("/x @Org/Team\n"), []string{"x/a.go"}, v, lint.Options{})
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("err = %v, want NoOpError — @Org/Team is @org/team, which exists", err)
	}
	if res.Plan.AfterContent != "/x @Org/Team\n" {
		t.Errorf("after = %q — lint corrects ownership, not spelling", res.Plan.AfterContent)
	}
	if !v.asked("TeamExists(org/team)") {
		t.Errorf("looked up the unfolded spelling; calls = %v", v.calls)
	}
}

// A line repaired and then deleted is ONE change, not two.
//
// len(Plan.Changes) drives "N fix(es) applied", so a single removed line
// reported as two, and the diff rendered a "+" line that never reached disk and
// was immediately deleted again.
func TestStages_ARepairedThenDeletedLineCountsOnce(t *testing.T) {
	tree := []string{"a.txt"}
	v := liveOrg("org/team", "org/everyone")
	res, err := lint.Build([]byte("* @org/everyone\n/ghost/ @ org/team\n"), tree, v,
		lint.Options{RemoveStalePaths: true, WorkTree: tree})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := len(res.Plan.Changes); n != 1 {
		t.Errorf("changes = %d, want 1 for one removed line: %+v", n, res.Plan.Changes)
	}
	if strings.Count(res.Plan.Diff, "@ line ") != 1 {
		t.Errorf("diff has %d hunks for one removed line:\n%s", strings.Count(res.Plan.Diff, "@ line "), res.Plan.Diff)
	}
	if strings.Contains(res.Plan.Diff, "+/ghost/") {
		t.Errorf("diff adds a line that never reached disk:\n%s", res.Plan.Diff)
	}
}

// Idempotency, as a property rather than an example.
//
// Three stages where stage 1 changes what stages 2 and 3 see is exactly the
// shape that fails to converge, and the README promises it does. A second run
// over the first run's output must be a no-op, and so must a third.
func TestBuild_IsIdempotent(t *testing.T) {
	lines := []string{
		"# comment\n", "\n", "* @org/live\n", "/x/ @ org/live\n", "/y/ @org/gone\n",
		"/Src/ @org/live\n", "/ghost/ @org/live\n", "/z @@@bad\n", "/w/ @org/live @org/live\n",
		"/e/ a@b.com\n", "/t/\t@org/live\t# note\n", "/cr/ @org/live\r\n",
	}
	tree := []string{"x/a.go", "y/b.go", "src/c.go", "w/d.go", "t/e.go", "e/f.go", "cr/g.go", "z/h.go"}
	next := uint64(7)
	rnd := func(n int) int {
		next = next*6364136223846793005 + 1442695040888963407
		return int((next >> 33) % uint64(n))
	}
	policies := []string{"", "error", "inherit", "unowned"}
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for j := 0; j < 1+rnd(6); j++ {
			b.WriteString(lines[rnd(len(lines))])
		}
		opts := lint.Options{OnEmpty: policies[rnd(len(policies))]}
		if rnd(2) == 0 {
			opts.RemoveStalePaths, opts.WorkTree = true, tree
		}
		v := liveOrg("org/live")

		first, err := lint.Build([]byte(b.String()), tree, v, opts)
		if err != nil || first == nil {
			continue // refusals and no-ops are settled elsewhere
		}
		second, err2 := lint.Build([]byte(first.Plan.AfterContent), tree, v, opts)
		var noop *plan.NoOpError
		if !errors.As(err2, &noop) {
			t.Fatalf("second run was not a no-op\n input:  %q\n after:  %q\n err:    %v",
				b.String(), first.Plan.AfterContent, err2)
		}
		if second.Plan.AfterContent != first.Plan.AfterContent {
			t.Fatalf("second run changed the file again\n first:  %q\n second: %q",
				first.Plan.AfterContent, second.Plan.AfterContent)
		}
	}
}
