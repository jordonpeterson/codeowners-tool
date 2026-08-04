package plan

import (
	"fmt"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/resolve"
)

// Zero-match semantics (R-21/R-22/INV-6).
//
// Every other op in this package is checked against files that exist. A
// `declare` op is the one place the tool writes a rule with nothing in the repo
// to check it against, and it runs unattended across a fleet on a schedule — so
// each defect here is multiplied by the fleet and nobody is watching. Two
// properties carry the whole safety argument, and both are established without
// consulting the tree, which by definition says nothing about the declared
// scope:
//
//   - IDEMPOTENCE. The planner's only no-op detector is a byte comparison, and
//     its desired-state map is keyed by tracked path. A declare op has neither,
//     so nothing already here can notice that the line it is about to append is
//     already the last line of the file. Unfixed, a nightly fleet run appends
//     the same rule to every repo forever, until the file crosses the 3 MB cliff
//     where GitHub stops loading it and every path loses its owners at once
//     (R-4/S-4). lastRuleForScope is the fix: "already declared" is a question
//     about resolution, not about bytes.
//
//   - THE STRUCTURAL PROOF (INV-6). The gate iterates the tree; with an empty
//     scope set it iterates nothing, so INV-1 is vacuously true for a declare —
//     "proven" without one statement having been made about the line written.
//     proveDeclares replaces it: over the RE-PARSED bytes, the rule is there, it
//     says what was asked, and no rule after it can capture the scope.

// declareCheck is one declared scope's structural obligation, carried from
// synthesis to the gate. Keyed by pattern LANGUAGE, not by op: two declares on
// the same scope collapse into one rule, and checking the first op's owner set
// against the rule the second one amended would refuse a batch that is correct.
type declareCheck struct {
	scope string
	want  []string
}

// synthDeclare writes a rule for a scope that matches nothing tracked (R-22).
//
// It deliberately does NOT reuse synthAdd's unowned-path branch, which inserts
// at firstRuleIndex — i.e. BEFORE every existing rule. CODEOWNERS is
// last-match-wins, so a rule written above a trailing `* @org/everyone`
// catch-all is shadowed for every path it was meant to govern: the tool would
// report "applied", the diff would look right in review, and the declaration
// would never take effect. A declaration appends at EOF, where nothing can
// recapture it.
func synthDeclare(f *file.File, op ops.Op, checks *[]*declareCheck, pl *Plan) error {
	warnDuplicateDeclaredPatterns(f, op.Scope, pl)
	rule, effective := lastRuleForScope(f, op.Scope)

	// A rule for this pattern is already here, but a later rule can capture the
	// scope, so it does not grant what it appears to grant. Reporting a no-op
	// would read as a converged repo in the fleet summary while the rollout did
	// nothing; the only way to make it effective is a second copy of the
	// pattern, and R-7 treats a duplicate pattern as a defect to report rather
	// than to author. Refuse and say so.
	if rule != nil && !effective {
		return &RefusalError{Msg: fmt.Sprintf(
			"refusing: rule %q on line %d already declares scope %q but a later rule can recapture it; "+
				"declaring it again would need a duplicate pattern, which is the defect R-7 exists to report — fix the ordering first",
			rule.PatternText, rule.LineIndex+1, op.Scope)}
	}

	var base []string
	if rule != nil {
		base = rule.OwnersCopy()
	}
	want := declaredOwners(op, base)

	switch {
	case rule == nil:
		at := len(f.Lines)
		f.InsertRule(at, op.Scope, want)
		pl.Changes = append(pl.Changes, Change{
			Action: "insert", Line: at + 1, Pattern: op.Scope,
			NewOwners: want, NewLine: f.LineText(at),
			Reason: fmt.Sprintf(
				"scope %q matches no tracked file; on_zero_match=declare appends the rule at EOF so no existing rule can shadow it (R-22, last-match-wins S-1)", op.Scope),
		})
	case !sameRuleOwners(rule.Owners, want):
		// Amend, never append a second line for the same pattern: two rules for
		// one pattern means last-match-wins silently DROPS the owners on the
		// shadowed line (R-4/R-7), and the file gains a line on every ownership
		// change forever.
		l := rule.LineIndex
		old := f.LineText(l)
		oldOwners := rule.OwnersCopy()
		f.SetOwners(l, want)
		pl.Changes = append(pl.Changes, Change{
			Action: "amend", Line: l + 1, Pattern: rule.PatternText,
			OldOwners: oldOwners, NewOwners: want,
			OldLine: old, NewLine: f.LineText(l),
			Reason: fmt.Sprintf(
				"rule %q already declares scope %q and nothing later recaptures it; amended in place rather than appending a duplicate pattern (R-22/R-7)", rule.PatternText, op.Scope),
		})
	default:
		// Already declared and already effective: edit nothing. This is the
		// only thing standing between a scheduled fleet and R-4's unbounded
		// growth, and it must survive spacing — the rule may be written with a
		// tab and an inline comment and still grant exactly these owners.
	}

	recordDeclareCheck(checks, op.Scope, want)
	return nil
}

// declaredOwners is the owner set the declared rule must end up with. base is
// the owner set of the rule the declaration lands on, if any.
func declaredOwners(op ops.Op, base []string) []string {
	if op.Kind == ops.SetOwners {
		// An empty list is a legal, deliberate un-owning (S-9): the rule keeps
		// its pattern with zero owners, so a future matching path is explicitly
		// NOT governed by whatever broader rule sits above it.
		return append([]string{}, op.Owners...)
	}
	out := append([]string{}, base...)
	if !contains(out, op.Owner) {
		out = append(out, op.Owner)
	}
	return out
}

// lastRuleForScope finds the rule a declaration lands on: the last rule whose
// pattern is the SAME LANGUAGE as the scope. Language, not bytes — a rule
// written with a tab, extra spaces or an inline comment already grants the
// declared owners, and rewriting it would churn a whitespace-only diff into one
// pull request per repo.
//
// It also reports whether that rule is still EFFECTIVE: under last-match-wins a
// rule any later rule can recapture does not grant what it appears to grant, so
// a declaration sitting above a catch-all is not satisfied by it.
func lastRuleForScope(f *file.File, scope string) (*file.Rule, bool) {
	rules := f.Rules()
	idx := -1
	for i, r := range rules {
		if samePatternLanguage(scope, r.PatternText) {
			idx = i
		}
	}
	if idx < 0 {
		return nil, false
	}
	for _, later := range rules[idx+1:] {
		if !patternsProvablyDisjoint(scope, later.PatternText) {
			return rules[idx], false
		}
	}
	return rules[idx], true
}

// proveDeclares is INV-6's structural proof, run over the RE-PARSED bytes for
// exactly the reason the tree gate re-parses: a rule that serializes into
// something that reads back as a DIFFERENT rule is a silent ownership change,
// and for a declared scope there is no tracked path whose resolution would
// betray it. Three obligations, all structural:
//
//  1. the emitted line round-trips as a rule for the declared scope;
//  2. it carries exactly the owner set the op asked for;
//  3. no rule after it can capture the scope, so it is the last word for every
//     path added later — the guarantee that replaces INV-1 here.
//
// Anything unproven is a refusal, never a warning: this is the only check the
// line gets.
func proveDeclares(after *file.File, checks []*declareCheck) error {
	if len(checks) == 0 {
		return nil
	}
	rules := after.Rules()
	for _, c := range checks {
		idx := -1
		for i, r := range rules {
			if samePatternLanguage(c.scope, r.PatternText) {
				idx = i
			}
		}
		if idx < 0 {
			return &RefusalError{Msg: fmt.Sprintf(
				"refusing: the line written for scope %q does not read back as a rule for that pattern — "+
					"nothing tracked matches the scope, so this round-trip is the only proof the rule says what was asked (INV-6)", c.scope)}
		}
		if !sameRuleOwners(rules[idx].Owners, c.want) {
			return &RefusalError{Msg: fmt.Sprintf(
				"refusing: the declared rule for %q reads back as %s, not %s (INV-6)",
				c.scope, fmtOwners(rules[idx].Owners), fmtOwners(c.want))}
		}
		for _, later := range rules[idx+1:] {
			if !patternsProvablyDisjoint(c.scope, later.PatternText) {
				return &RefusalError{Msg: fmt.Sprintf(
					"refusing: rule %q on line %d comes after the rule declared for %q and cannot be shown disjoint from it; "+
						"a later rule that can recapture the scope makes the declaration dead on arrival (INV-6)",
					later.PatternText, later.LineIndex+1, c.scope)}
			}
		}
	}
	return nil
}

func recordDeclareCheck(checks *[]*declareCheck, scope string, want []string) {
	for _, c := range *checks {
		if samePatternLanguage(c.scope, scope) {
			c.want = want
			return
		}
	}
	*checks = append(*checks, &declareCheck{scope: scope, want: want})
}

// patternsProvablyDisjoint reports whether NO path can ever match both
// patterns. It is SOUND and deliberately incomplete, in the same shape as
// pattern.Contains: false means "could not prove disjoint", never "they
// overlap". Every caller treats an unproven pair as overlapping, so
// incompleteness costs a refusal or a stricter R-8 — never a wrong write.
//
// The shape it proves: two anchored, wildcard-free directory scopes. Such a
// pattern matches exactly its own path plus everything beneath it, so the two
// languages can meet only if one prefix contains the other. anchoredDirPrefix
// supplies the normalization and rejects anything with a wildcard or an escape
// in it, which is what keeps the claim honest.
func patternsProvablyDisjoint(a, b string) bool {
	pa, ok := anchoredDirPrefix(a)
	if !ok {
		return false
	}
	pb, ok := anchoredDirPrefix(b)
	if !ok {
		return false
	}
	return !strings.HasPrefix(pa, pb) && !strings.HasPrefix(pb, pa)
}

// commuteOnEveryOwnerSet decides R-8 for a pair of ops that meet on paths which
// do not exist yet.
//
// The tree-based R-8 check intersects path SETS, and a declared scope owns
// none — so two contradictory declares meet on the empty set and sail through
// as "commuting", and the batch is written in input order with last-match-wins
// silently picking the winner. There is no path to test, so test the transforms
// instead: each op's owner-set transform depends on its input only through
// membership of the identifiers it names (set_owners ignores the input
// entirely), so probing every subset of those identifiers, plus one identifier
// neither op names, decides commutation exactly.
func commuteOnEveryOwnerSet(a, b ops.Op) bool {
	var probe []string
	for _, o := range []string{a.Owner, a.NewOwner, b.Owner, b.NewOwner} {
		if o != "" && !contains(probe, o) {
			probe = append(probe, o)
		}
	}
	// Stands in for whatever an existing broader rule already granted the
	// future path; without it, transforms that differ only on foreign owners
	// (remove vs. set, say) would look equal on the empty set alone.
	probe = append(probe, "@zz/owner-not-named-by-either-op")

	states := [][]string{nil}
	for mask := 0; mask < 1<<len(probe); mask++ {
		s := []string{}
		for k, o := range probe {
			if mask&(1<<k) != 0 {
				s = append(s, o)
			}
		}
		states = append(states, s)
	}
	for _, s := range states {
		if !sameRuleOwners(simulate(b, simulate(a, s)), simulate(a, simulate(b, s))) {
			return false
		}
	}
	return true
}

// sameRuleOwners compares two RULE owner lists. resolve.OwnersEqual
// distinguishes nil ("no rule matches") from empty ("explicitly zero owners"),
// which is load-bearing for path resolution and meaningless here: a rule with
// no owner tokens parses to a nil slice and is built from an empty one.
func sameRuleOwners(a, b []string) bool {
	return resolve.OwnersEqual(append([]string{}, a...), append([]string{}, b...))
}

// warnDuplicateDeclaredPatterns is R-7 for the declare path: only the last of
// several rules for one pattern is effective, and that is the one edited.
func warnDuplicateDeclaredPatterns(f *file.File, scope string, pl *Plan) {
	var hits []*file.Rule
	for _, r := range f.Rules() {
		if samePatternLanguage(scope, r.PatternText) {
			hits = append(hits, r)
		}
	}
	for _, shadowed := range hits[:max0(len(hits)-1)] {
		pl.addWarning(fmt.Sprintf(
			"line %d: rule %q is shadowed by line %d, which governs the same paths; declared scope %q was written to the effective rule only (R-7) — run `audit` to clean up duplicates",
			shadowed.LineIndex+1, shadowed.PatternText, hits[len(hits)-1].LineIndex+1, scope))
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// opResultFor is R-24's per-op record. The fleet record is the only artifact of
// an unattended run: "which repos lack Terraform" is answerable only if every
// op reports itself, by id, in the order the policy lists them.
func opResultFor(op ops.Op, skipped, declared, changed bool) OpResult {
	r := OpResult{ID: op.ID, Op: op.Raw}
	if skipped {
		// A skipped op must carry a reason — without it a fleet operator cannot
		// tell a skipped repo from a converged one. Proven stays empty: a
		// skipped op proves nothing.
		r.Status = "skipped"
		r.Reason = fmt.Sprintf("scope %q matches zero tracked files and on_zero_match=skip (R-21)", op.Scope)
		return r
	}
	r.Status = "unchanged"
	if changed {
		r.Status = "applied"
	}
	// INV-6's disclosure: "structural" says a rule went in without a single
	// tracked file to check it against. A fleet operator reading 100 JSON
	// records cannot otherwise tell a checked rollout from an unchecked one.
	r.Proven = "tree"
	if declared {
		r.Proven = "structural"
	}
	return r
}
