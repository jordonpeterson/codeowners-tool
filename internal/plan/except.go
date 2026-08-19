package plan

import (
	"fmt"
	"sort"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
	"github.com/jordonpeterson/codeowners-tool/internal/resolve"
)

// Carve synthesis (R-29).
//
// An except-carrying op's verb synthesis runs over the EFFECTIVE scope, but
// the lines it writes or amends are patterns, and a pattern does not know
// about the subtraction: under last-match-wins the written line can capture
// the excepted paths. The carve restores them — one synthesized line per
// (written/amended line × owner-homogeneous excepted region), restating the
// owners the paths had on the evolving file just before this op, placed
// IMMEDIATELY after the line it corrects.
//
// The placement is structural, not tree-observed, and that is a soundness
// requirement, not a style choice: "anywhere it wins for the excepted paths
// and nothing else" admits an end-of-file placement the proof gate cannot
// reject — the gate ranges tracked paths, and a pre-existing rule matching
// zero tracked files (a dead security rule like `/x/gen/secret/ @Sec`) would
// be silently shadowed for every FUTURE file it exists to guard. The project
// already treats tree-exact-but-future-wrong output as a wrong write
// (anchoredDirPrefix); carve placement inherits that standard, so a carve
// never moves past a pre-existing rule.

// deriveDomain is the path set narrowing candidates are proven tree-exact
// over: the op's effective scope plus its tracked except matches. For a plain
// op that IS the scope set, so nothing that derived before excepts existed
// derives differently now. For an except-carrying op the union is required
// for soundness's mirror image — availability: a narrowing line legitimately
// matches the excepted paths, because the carve synthesized immediately after
// it restores them (R-29); proven over the effective scope alone, the
// motivating candidate "/.github/" fails tree-exactness at exactly the
// excepted CODEOWNERS path and the op refuses on every repo.
func deriveDomain(op ops.Op, scope map[string]bool, tree []string) map[string]bool {
	if len(op.Except) == 0 {
		return scope
	}
	out := make(map[string]bool, len(scope))
	for p := range scope {
		out[p] = true
	}
	for _, e := range op.Except {
		ep, err := pattern.Compile(e)
		if err != nil {
			continue // Build already refused uncompilable excepts
		}
		for _, p := range tree {
			if ep.Match(p) {
				out[p] = true
			}
		}
	}
	return out
}

// exceptBaseline is one excepted path's resolution on the evolving file
// before its op ran: the owners a carve must restate (R-29 — the EVOLVING
// file, not the before-batch snapshot: a sibling rename ordered earlier must
// see its rename respected, and restating stale before-batch owners would
// force a spurious gate refusal), and the rule that granted them, which is
// what owner-homogeneous regions are keyed by.
type exceptBaseline struct {
	owners  []string // non-nil; may be empty (S-9)
	rulePat string   // pattern text of the winning rule at baseline time
	ruleIdx int      // line index of that rule at baseline time — the region
	// key. Two rules spelling one pattern (R-7 duplicates) are
	// still two regions, because only one of them was winning.
}

// exceptedBaseline captures every excepted path's pre-op resolution, and
// enforces R-29's one unfixable edge: an excepted path that currently matches
// no rule. Unmatched (nil) and explicitly zero-owned ([], S-9) are distinct
// resolved states and never equal (OwnersEqual); no writable line can restore
// "unmatched" once a broad line captures the path, so the op refuses rather
// than quietly converting "nobody owns this" into "a rule says nobody owns
// this". The refusal is unconditional — not deferred until a written line
// happens to capture the path — because whether the verb amends in place or
// inserts a broad line is a synthesis detail, and a don't-touch promise that
// holds or fails with it would flicker repo to repo.
func exceptedBaseline(f *file.File, tree []string, op ops.Op, excepted map[string]bool) (map[string]exceptBaseline, error) {
	res := resolve.All(f, tree)
	base := make(map[string]exceptBaseline, len(excepted))
	for _, p := range tree {
		if !excepted[p] {
			continue
		}
		r := res[p]
		if !r.Matched {
			return nil, &RefusalError{Msg: fmt.Sprintf(
				"refusing: excepted path %q matches no rule, so no carve can restore it — unmatched and explicitly zero-owned are different states (S-9), and writing %s would quietly convert \"nobody owns this\" into \"a rule says nobody owns this\" (R-29)",
				p, op.Raw)}
		}
		base[p] = exceptBaseline{
			owners:  r.Owners,
			rulePat: f.Lines[r.LineIndex].Rule.PatternText,
			ruleIdx: r.LineIndex,
		}
	}
	return base, nil
}

// synthCarves restores every excepted path the op's edits captured. Runs
// per except pattern in clause order, re-resolving before each so a carve
// already written for a broader pattern is seen by a narrower one (the
// narrower pattern's paths then need no second carve).
func synthCarves(f *file.File, tree []string, op ops.Op, excepted map[string]bool, base map[string]exceptBaseline, pl *Plan) error {
	// The carves THIS op inserted, by rule identity (Line pointers are stable
	// across inserts). Placement may step over these — a second carve for the
	// same corrected line goes after the first, keeping clause order — but
	// never over a rule that existed before this op.
	myCarves := map[*file.Rule]bool{}
	for _, e := range op.Except {
		ep, err := pattern.Compile(e)
		if err != nil {
			return &InvalidError{Msg: fmt.Sprintf("invalid except pattern %q: %v", e, err)}
		}
		post := resolve.All(f, tree)

		// Captured paths: excepted, matching this pattern, and resolving
		// differently than at baseline. Only this op's edits sit between the
		// two resolutions, so a change means an op-written or op-amended line
		// now wins for the path.
		matched := map[string]bool{}
		byLine := map[int][]string{}
		var lines []int
		homogeneous := true
		var shared []string
		first := true
		for _, p := range tree {
			if !excepted[p] || !ep.Match(p) {
				continue
			}
			matched[p] = true
			if first {
				shared, first = base[p].owners, false
			} else if homogeneous && !resolve.OwnersEqual(shared, base[p].owners) {
				homogeneous = false
			}
			if !post[p].Matched {
				// A rule deletion (R-6 inherit) dropped the excepted path to
				// "no rule matches"; same unfixable edge as the baseline one.
				return &RefusalError{Msg: fmt.Sprintf(
					"refusing: %s leaves excepted path %q matching no rule, and no carve can restore an unmatched state (R-29)", op.Raw, p)}
			}
			if resolve.OwnersEqual(post[p].Owners, base[p].owners) {
				continue // untouched (e.g. a pre-existing later rule still wins) — except means don't touch, and nothing needs correcting
			}
			l := post[p].LineIndex
			if _, seen := byLine[l]; !seen {
				lines = append(lines, l)
			}
			byLine[l] = append(byLine[l], p)
		}
		if len(lines) == 0 {
			continue
		}
		// Descending, so an insert after a later line never shifts an earlier
		// capturing line's index out from under the loop.
		sort.Sort(sort.Reverse(sort.IntSlice(lines)))

		if len(lines) == 1 && homogeneous {
			// One capturing line, one owner set across everything the pattern
			// matches: the carve is the except pattern verbatim. Re-capturing
			// an E-match the line did NOT change is harmless here — the carve
			// restates the owner set that path already has.
			insertCarve(f, pl, myCarves, lines[0], e, e, shared)
			continue
		}
		for _, l := range lines {
			if err := carveRegions(f, tree, ep, e, l, byLine[l], base, myCarves, pl); err != nil {
				return err
			}
		}
	}
	return nil
}

// carveRegions carves one capturing line whose captured paths span more than
// one owner set (or whose pattern's paths were captured by several lines):
// one carve per owner-homogeneous region, keyed by the baseline rule that was
// winning — paths that shared a winning rule share its owners by construction.
// Regions are written in baseline-line order, recreating the original file's
// last-match-wins layering among them.
func carveRegions(f *file.File, tree []string, ep *pattern.Pattern, exceptPat string, l int, paths []string, base map[string]exceptBaseline, myCarves map[*file.Rule]bool, pl *Plan) error {
	byRule := map[int][]string{}
	var order []int
	for _, p := range paths {
		idx := base[p].ruleIdx
		if _, seen := byRule[idx]; !seen {
			order = append(order, idx)
		}
		byRule[idx] = append(byRule[idx], p)
	}
	sort.Ints(order)
	for _, idx := range order {
		region := byRule[idx]
		rulePat := base[region[0]].rulePat
		cand, exact, ok := deriveCarvePattern(exceptPat, ep, rulePat, region, tree)
		if !ok {
			return &RefusalError{Msg: fmt.Sprintf(
				"refusing: except pattern %q spans paths whose current owners differ, and no sound carve pattern is derivable for the region governed by %q — writing the line would capture the excepted paths (R-29)",
				exceptPat, rulePat)}
		}
		if !exact {
			pl.addWarning(inexactNarrowingWarning(cand, exceptPat, rulePat))
		}
		insertCarve(f, pl, myCarves, l, cand, exceptPat, base[region[0]].owners)
	}
	return nil
}

// deriveCarvePattern finds a pattern for one owner-homogeneous region of an
// except clause, reusing the narrowing machinery's shapes and its proof
// discipline: a candidate is returned only if, over the tracked tree, it
// matches every region path and nothing outside (except ∩ baseline rule) —
// so a carve can never steal a tracked path the clause does not name. exact
// reports whether it is also provably confined for FUTURE files (containment
// in both parents over the pattern language); the caller discloses inexact
// ones with the same warning the narrowing inserts use.
func deriveCarvePattern(exceptPat string, ep *pattern.Pattern, rulePat string, region []string, tree []string) (cand string, exact, ok bool) {
	var cands []string
	// The baseline rule sits entirely inside the except clause: restoring the
	// rule's own pattern restores exactly its coverage, future files included.
	if pattern.Contains(exceptPat, rulePat) {
		cands = append(cands, rulePat)
	}
	if rp, err := pattern.Compile(rulePat); err == nil {
		regionSet := map[string]bool{}
		for _, p := range region {
			regionSet[p] = true
		}
		synth := &file.Rule{PatternText: rulePat, Pattern: rp}
		if c, got := deriveIntersection(exceptPat, synth, regionSet, tree); got {
			cands = append(cands, c)
		}
		for _, c := range cands {
			cp, err := pattern.Compile(c)
			if err != nil {
				continue
			}
			sound := true
			for _, p := range tree {
				m := cp.Match(p)
				if m && !(ep.Match(p) && rp.Match(p)) {
					sound = false // reaches a tracked path outside except ∩ rule
					break
				}
			}
			if !sound {
				continue
			}
			covers := true
			for _, p := range region {
				if !cp.Match(p) {
					covers = false
					break
				}
			}
			if !covers {
				continue
			}
			return c, pattern.Contains(exceptPat, c) && pattern.Contains(rulePat, c), true
		}
	}
	return "", false, false
}

// insertCarve places one carve line immediately after the line it corrects,
// stepping over carves this op already wrote for that line (so several
// excepts carving one grant appear in clause order) and never over a
// pre-existing rule.
func insertCarve(f *file.File, pl *Plan, myCarves map[*file.Rule]bool, l int, carvePat, exceptPat string, owners []string) {
	at := l + 1
	for at < len(f.Lines) {
		ln := f.Lines[at]
		if ln.Kind == file.LineRule && myCarves[ln.Rule] {
			at++
			continue
		}
		break
	}
	restated := append([]string{}, owners...)
	r := f.InsertRule(at, carvePat, restated)
	myCarves[r] = true
	pl.Changes = append(pl.Changes, Change{
		Action: "insert", Line: at + 1, Pattern: carvePat,
		NewOwners: restated, NewLine: f.LineText(at),
		Reason: fmt.Sprintf(
			"carve for `except %s`: the line above would otherwise govern the excepted path(s) by last-match-wins (S-1); this restates their current owners immediately after it, before any pre-existing rule (R-29)",
			exceptPat),
	})
}
