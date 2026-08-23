package lint_test

// The tree-only mode behind offline `lint --remove-stale-paths`
// (Options.SkipOwnerChecks).
//
// R-12 makes owner existence — an API fact — undecidable offline, and the
// whole run fails closed on it. But whether a pattern matches zero tracked
// files is a git-TREE fact the offline audit (A-4/A-5) already proves, so a
// run that asks ONLY for the stale-path repair may run with no Verifier at
// all. The contract for that mode, pinned here:
//
//   - stage 3 runs exactly as it does online — same staleness judgment, same
//     A-5 sparing of case-only misses;
//   - the owner WORK of stages 1 and 2 is skipped, not degraded: no owner is
//     looked up, repaired, or removed, and no invalid line is rewritten;
//   - stage 1's REPORTING still runs: an invalid line GitHub is skipping is a
//     file fact, no API needed, so it is reported (NeedsHuman → exit 4) — a
//     line the online run would repair with a reason that names the
//     credentialed run, any other with the same reason as online;
//   - SkipOwnerChecks without RemoveStalePaths is invalid input, because with
//     owner work forbidden there is nothing left the run may do (R-11/R-12).

import (
	"errors"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/lint"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// offlineOpts is the option set the offline CLI path builds: stage 3 opted in,
// owner checks skipped, an --on-empty that could never matter (stage 2 is off).
func offlineOpts(workTree []string) lint.Options {
	return lint.Options{RemoveStalePaths: true, WorkTree: workTree, SkipOwnerChecks: true}
}

// SPEC A-4/R-11 offline: a rule whose pattern matches nothing tracked and
// nothing on disk is deleted with a NIL Verifier. The nil is the proof that no
// lookup can possibly have been made — an implementation that touched the
// network here would panic, not pass.
func TestOffline_DeadRuleIsRemovedWithANilVerifier(t *testing.T) {
	content := []byte("* @org/everyone\n/ghost/ @org/ghost-team\n")
	tree := []string{"a.md"}

	res, err := lint.Build(content, tree, nil, offlineOpts(tree))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	after := fcAfter(res)
	if after != "* @org/everyone\n" {
		t.Errorf("after = %q, want only the dead rule gone", after)
	}
	stale := fcActionsOfKind(res, lint.ActionRemoveStale)
	if len(stale) != 1 || stale[0].Pattern != "/ghost/" {
		t.Errorf("stale actions = %+v, want exactly one for /ghost/", res.Actions)
	}
	// A stale rule wins no tracked path by construction, so its deletion must
	// change no ownership — the same property the end-of-run gate relies on.
	if len(res.Plan.Rows) != 0 {
		t.Errorf("ownership rows = %+v, want none: deleting a dead rule changes no current ownership", res.Plan.Rows)
	}
}

// SPEC R-12 offline: owners are never touched, even one a lookup WOULD have
// proven dead. Proven from the call log, not the output — an implementation
// that asked and ignored the answer is one refactor from believing it.
func TestOffline_OwnersAreNeverLookedUpOrRemoved(t *testing.T) {
	v := fcNew()
	v.missingUsers["gone"] = true // dead if anyone asked; nobody may ask

	content := []byte("* @keep @gone\n/ghost/ @keep\n")
	tree := []string{"a.md"}
	res, err := lint.Build(content, tree, v, offlineOpts(tree))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if calls := v.fcCalls(); len(calls) != 0 {
		t.Errorf("offline run made API calls: %v (R-12: owner existence was not asked for and must not be answered)", calls)
	}
	fcAssertNotRemoved(t, res, "@gone")
	if after := fcAfter(res); !strings.Contains(after, "@keep @gone") {
		t.Errorf("after = %q: an owner was touched by a run that could not know anything about owners", after)
	}
}

// SPEC R-12 offline: stage 1's REPAIR is an owner repair — it puts a
// previously skipped line, and the unverified owner on it, into force — so
// offline the line is left byte-for-byte. But it IS reported: GitHub skipping
// the line is a file fact, and a run that exits 0 over it goes green over rot.
// The reason is honest about which run can fix it — a credentialed one, which
// repairs it mechanically and verifies the reassembled owner.
func TestOffline_SplitHandleIsReportedNotRepaired(t *testing.T) {
	content := []byte("* @keep\n/x/ @ org/team\n/ghost/ @keep\n")
	tree := []string{"x/a.go"}

	res, err := lint.Build(content, tree, nil, offlineOpts(tree))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	after := fcAfter(res)
	if !strings.Contains(after, "/x/ @ org/team") {
		t.Errorf("after = %q: the invalid line was rewritten or deleted offline", after)
	}
	if n := len(fcActionsOfKind(res, lint.ActionRepairOwner)); n != 0 {
		t.Errorf("%d owner repair(s) in an offline run — repairing an owner offline is what R-12 forbids", n)
	}
	stuck := fcActionsOfKind(res, lint.ActionUnrepairable)
	if len(stuck) != 1 {
		t.Fatalf("unrepairable reports = %+v, want exactly one for the split handle — GitHub is skipping that line, and silence over it is exit 0 over rot", res.Actions)
	}
	if !strings.Contains(stuck[0].Reason, "credentialed run") {
		t.Errorf("reason = %q: a line the online run repairs mechanically must be reported as needing that run, not as flatly unrepairable", stuck[0].Reason)
	}
	if res.NeedsHuman() != 1 {
		t.Errorf("NeedsHuman = %d, want 1 — the broken line keeps the run at exit 4", res.NeedsHuman())
	}
	if strings.Contains(after, "/ghost/") {
		t.Errorf("after = %q: the genuinely dead rule survived — sparing owners is not a blanket refusal to do the tree work", after)
	}
}

// SPEC offline reporting: a line no run can repair — `@keep /docs` is shaped
// exactly like two rules on one line — is reported offline with the SAME
// reason as online. GitHub skipping it is a file fact, and docs/LINTING.md
// promises exit 4 for it; an offline exit 0 would let a CI gate on
// .needs_human go green over a broken line.
func TestOffline_UnrepairableLineIsReportedSameAsOnline(t *testing.T) {
	content := []byte("* @keep\n/x/ @keep /docs\n/ghost/ @keep\n")
	tree := []string{"x/a.go"}

	res, err := lint.Build(content, tree, nil, offlineOpts(tree))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	stuck := fcActionsOfKind(res, lint.ActionUnrepairable)
	if len(stuck) != 1 {
		t.Fatalf("unrepairable reports = %+v, want exactly one", res.Actions)
	}
	if !strings.Contains(stuck[0].Reason, "GitHub skips this line") {
		t.Errorf("reason = %q, want the online wording — nothing about this line's diagnosis needed an API", stuck[0].Reason)
	}
	if strings.Contains(stuck[0].Reason, "credentialed run") {
		t.Errorf("reason = %q: this line is ambiguous for EVERY run, and pointing at a credentialed one promises a repair it will also refuse", stuck[0].Reason)
	}
	if res.NeedsHuman() != 1 {
		t.Errorf("NeedsHuman = %d, want 1", res.NeedsHuman())
	}
	if after := fcAfter(res); !strings.Contains(after, "/x/ @keep /docs") {
		t.Errorf("after = %q: the invalid line was touched", after)
	}
}

// SPEC A-5/S-6 offline: a rule that matches nothing ONLY because of case is a
// typo, not a dead rule, and the offline mode spares it with exactly the
// online logic — deleting it would silently un-own the files it was aimed at.
// Spared means reported (NeedsHuman), so the caller still exits 4.
func TestOffline_CaseOnlyMissIsSparedAndNeedsAHuman(t *testing.T) {
	content := []byte("* @keep\n/Src/ @keep\n")
	tree := []string{"src/a.go"}

	res, err := lint.Build(content, tree, nil, offlineOpts(tree))
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("err = %v (%T), want *plan.NoOpError — the spared rule is the only candidate, so nothing changes", err, err)
	}
	if got := fcActionsOfKind(res, lint.ActionKeptCaseMismatch); len(got) != 1 {
		t.Fatalf("kept-case-mismatch actions = %+v, want exactly one for /Src/", res.Actions)
	}
	if res.NeedsHuman() != 1 {
		t.Errorf("NeedsHuman = %d, want 1 — a spared typo still needs a person (exit 4)", res.NeedsHuman())
	}
	// The no-op message must not claim the owner checks ran.
	if msg := noop.Error(); strings.Contains(msg, "every owner named by a valid rule exists") {
		t.Errorf("no-op message asserts owner checks that never ran: %q", msg)
	}
}

// SPEC R-11/R-12: SkipOwnerChecks without RemoveStalePaths is invalid input,
// not a clean run. Stages 1 and 2 are owner work the mode forbids, and stage 3
// was not opted into — a "clean" from a run that checked nothing would be a
// green check that means nothing.
func TestOffline_SkipWithoutRemoveStaleIsInvalid(t *testing.T) {
	content := []byte("* @keep\n")
	_, err := lint.Build(content, []string{"a.md"}, nil, lint.Options{SkipOwnerChecks: true})
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("err = %v (%T), want *plan.InvalidError", err, err)
	}
}

// SPEC R-13 offline: email owners are not looked up online either, but online
// they are REPORTED as unverifiable — a statement about a check that ran
// around them. Offline no owner check ran at all, so the report would imply
// the rest of the file's owners were verified. Nothing is reported.
func TestOffline_EmailOwnersAreNotReportedUnverifiable(t *testing.T) {
	content := []byte("* @keep docs@example.com\n/ghost/ @keep\n")
	tree := []string{"a.md"}

	res, err := lint.Build(content, tree, nil, offlineOpts(tree))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Unverifiable) != 0 {
		t.Errorf("Unverifiable = %v, want empty — offline, EVERY owner is unverified, and singling out the email owner implies the others were checked", res.Unverifiable)
	}
	if after := fcAfter(res); !strings.Contains(after, "docs@example.com") {
		t.Errorf("after = %q: the email owner was touched", after)
	}
}
