// Package plan is Engine A's planner: intent → line edits, gated on the two
// invariants that make the tool trustworthy.
//
//	INV-1 (in scope): after apply, owners(p) equals what the op requires.
//	INV-2 (out of scope): after apply, owners(p) is identical to before.
//
// The planner synthesizes edits heuristically, then PROVES the result by
// re-resolving the entire tree and comparing against the independently
// computed desired state. Anything unprovable is refused (exit 2). INV-2 is
// the product; everything else is implementation.
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jordonpropm/codeowners-tool/internal/file"
	"github.com/jordonpropm/codeowners-tool/internal/ops"
	"github.com/jordonpropm/codeowners-tool/internal/pattern"
	"github.com/jordonpropm/codeowners-tool/internal/resolve"
)

// NoOpError: nothing to change (exit 1).
type NoOpError struct{ Msg string }

func (e *NoOpError) Error() string { return e.Msg }

// RefusalError: applying would violate INV-1 or INV-2, or the intent is not
// expressible without breaking an invariant (exit 2).
type RefusalError struct {
	Msg     string
	Details []string
}

func (e *RefusalError) Error() string {
	if len(e.Details) == 0 {
		return e.Msg
	}
	return e.Msg + "\n  " + strings.Join(e.Details, "\n  ")
}

// InvalidError: malformed op, unresolvable scope, conflicting batch (exit 3).
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Options tunes planning. OnEmpty has NO default: emptying an owner set
// without an explicit policy is invalid input (R-6).
type Options struct {
	OnEmpty  string // "", "error", "inherit", "unowned"
	MaxSize  int    // refuse above this (default 3,000,000 — S-4's 3 MB, read conservatively)
	WarnSize int    // warn at or above this (default 2,500,000 per R-9)
}

func (o *Options) setDefaults() {
	if o.MaxSize == 0 {
		o.MaxSize = 3_000_000
	}
	if o.WarnSize == 0 {
		o.WarnSize = 2_500_000
	}
}

// Change is one line edit, with the reason it was chosen (R-16).
type Change struct {
	Action    string   `json:"action"` // amend | insert | delete
	Line      int      `json:"line"`   // 1-based line number at synthesis time
	Pattern   string   `json:"pattern"`
	OldOwners []string `json:"old_owners,omitempty"`
	NewOwners []string `json:"new_owners,omitempty"`
	OldLine   string   `json:"old_line,omitempty"`
	NewLine   string   `json:"new_line,omitempty"`
	Reason    string   `json:"reason"`
}

// Row is one path whose resolved owners change (R-16). Before/After nil
// means "no rule matches" — distinct from an explicit empty owner set.
type Row struct {
	Path   string   `json:"path"`
	Before []string `json:"owners_before"`
	After  []string `json:"owners_after"`
}

// Plan is the machine-readable output of `plan` and the sole input of
// `apply` (R-16).
type Plan struct {
	Ops          []string `json:"ops"`
	HashBefore   string   `json:"sha256_before"`
	SizeBefore   int      `json:"size_before"`
	SizeAfter    int      `json:"size_after"`
	Warnings     []string `json:"warnings,omitempty"`
	Changes      []Change `json:"changes"`
	Rows         []Row    `json:"ownership_rows"`
	Diff         string   `json:"diff"`
	AfterContent string   `json:"after_content"`
}

// ResolveContent parses content and resolves the whole tree — the primitive
// `verify` and the tests use to check plans without trusting the planner.
func ResolveContent(content string, tree []string) map[string]resolve.Resolution {
	return resolve.All(file.Parse([]byte(content)), tree)
}

// Build computes a plan. It never writes anything.
func Build(content []byte, tree []string, opList []ops.Op, opts Options) (*Plan, error) {
	opts.setDefaults()
	if len(opList) == 0 {
		return nil, &InvalidError{Msg: "no operations supplied"}
	}

	f := file.Parse(content)
	before := resolve.All(f, tree)
	beforeOwners := make(map[string][]string, len(tree))
	for p, r := range before {
		beforeOwners[p] = r.Owners // nil if unmatched
	}

	// Per-op scope path sets (R-5: empty scope is invalid input).
	scopeSets := make([]map[string]bool, len(opList))
	for i, op := range opList {
		set := map[string]bool{}
		if op.Kind == ops.RenameOwner {
			for p, own := range beforeOwners {
				if contains(own, op.Owner) {
					set[p] = true
				}
			}
		} else {
			pat, err := pattern.Compile(op.Scope)
			if err != nil {
				return nil, &InvalidError{Msg: fmt.Sprintf("invalid scope %q: %v", op.Scope, err)}
			}
			for _, p := range tree {
				if pat.Match(p) {
					set[p] = true
				}
			}
			if len(set) == 0 {
				return nil, &InvalidError{Msg: fmt.Sprintf("scope %q matches zero tracked files (R-5: refusing to create a dead rule)", op.Scope)}
			}
		}
		scopeSets[i] = set
	}

	// R-8: reject order-dependent overlapping batches.
	for i := 0; i < len(opList); i++ {
		for j := i + 1; j < len(opList); j++ {
			for p := range scopeSets[i] {
				if !scopeSets[j][p] {
					continue
				}
				ij := simulate(opList[j], simulate(opList[i], beforeOwners[p]))
				ji := simulate(opList[i], simulate(opList[j], beforeOwners[p]))
				if !resolve.OwnersEqual(ij, ji) {
					return nil, &InvalidError{Msg: fmt.Sprintf(
						"ops %q and %q overlap on %q and do not commute (R-8: refusing order-dependent batch)",
						opList[i].Raw, opList[j].Raw, p)}
				}
			}
		}
	}

	// Desired final ownership, computed independently of edit synthesis.
	desired := make(map[string][]string, len(tree))
	for p, own := range beforeOwners {
		desired[p] = own
	}

	pl := &Plan{HashBefore: hashHex(content), SizeBefore: len(content)}
	for _, op := range opList {
		pl.Ops = append(pl.Ops, op.Raw)
	}

	// Synthesize edits op by op on the evolving file.
	for i, op := range opList {
		var err error
		switch op.Kind {
		case ops.AddOwner:
			err = synthAdd(f, tree, op, scopeSets[i], desired, pl)
		case ops.SetOwners:
			err = synthSet(f, tree, op, scopeSets[i], desired, pl)
		case ops.RemoveOwner:
			err = synthRemove(f, tree, op, scopeSets[i], desired, opts.OnEmpty, pl)
		case ops.RenameOwner:
			err = synthRename(f, op, desired, pl)
		}
		if err != nil {
			return nil, err
		}
	}

	// ASSERT: the gate. Re-resolve the mutated file over the real tree and
	// prove INV-1/INV-2 against the independently computed desired state.
	after := resolve.All(f, tree)
	var violations []string
	for _, p := range tree {
		want := desired[p]
		got := after[p].Owners
		if !resolve.OwnersEqual(got, want) {
			inScope := false
			for i := range opList {
				if scopeSets[i][p] {
					inScope = true
					break
				}
			}
			inv := "INV-2 (out of scope changed)"
			if inScope {
				inv = "INV-1 (in-scope result wrong)"
			}
			violations = append(violations, fmt.Sprintf("%s: %s — want %s, would get %s",
				inv, p, fmtOwners(want), fmtOwners(got)))
		}
	}
	if violations != nil {
		return nil, &RefusalError{Msg: "refusing: synthesized edits do not satisfy the invariants", Details: violations}
	}

	afterBytes := f.Bytes()
	if bytes.Equal(afterBytes, content) {
		return nil, &NoOpError{Msg: "nothing to change: file already satisfies the requested ops"}
	}

	pl.AfterContent = string(afterBytes)
	pl.SizeAfter = len(afterBytes)
	if pl.SizeAfter > opts.MaxSize {
		return nil, &RefusalError{Msg: fmt.Sprintf(
			"refusing: result would be %d bytes, over the %d-byte limit — GitHub silently ignores CODEOWNERS files over 3 MB (S-4)",
			pl.SizeAfter, opts.MaxSize)}
	}
	if pl.SizeAfter >= opts.WarnSize {
		pl.Warnings = append(pl.Warnings, fmt.Sprintf(
			"file size %d bytes is at or above the warning threshold %d (R-9); GitHub stops loading at 3 MB", pl.SizeAfter, opts.WarnSize))
	}

	// Ownership rows: every path whose resolved owners change.
	var changed []string
	for _, p := range tree {
		if !resolve.OwnersEqual(beforeOwners[p], after[p].Owners) {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	for _, p := range changed {
		pl.Rows = append(pl.Rows, Row{Path: p, Before: beforeOwners[p], After: after[p].Owners})
	}

	// Literal line diff from the change records (R-16).
	var diff strings.Builder
	for _, c := range pl.Changes {
		switch c.Action {
		case "amend":
			fmt.Fprintf(&diff, "@ line %d\n-%s\n+%s\n", c.Line, c.OldLine, c.NewLine)
		case "insert":
			fmt.Fprintf(&diff, "@ line %d\n+%s\n", c.Line, c.NewLine)
		case "delete":
			fmt.Fprintf(&diff, "@ line %d\n-%s\n", c.Line, c.OldLine)
		}
	}
	pl.Diff = diff.String()
	return pl, nil
}

// simulate applies an op's owner-set transform to one path's owners — used
// only for the R-8 commutativity check.
func simulate(op ops.Op, owners []string) []string {
	switch op.Kind {
	case ops.AddOwner:
		if contains(owners, op.Owner) {
			return owners
		}
		return append(append([]string{}, owners...), op.Owner)
	case ops.SetOwners:
		return append([]string{}, op.Owners...)
	case ops.RemoveOwner:
		return minus(owners, op.Owner)
	case ops.RenameOwner:
		if !contains(owners, op.Owner) {
			return owners
		}
		out := minus(owners, op.Owner)
		if !contains(out, op.NewOwner) {
			out = append(out, op.NewOwner)
		}
		return out
	}
	return owners
}

func synthAdd(f *file.File, tree []string, op ops.Op, scope map[string]bool, desired map[string][]string, pl *Plan) error {
	cur := resolve.All(f, tree)
	winners := winnersByLine(cur)
	warnShadowedDuplicates(f, tree, scope, pl)

	groups := map[int]bool{}
	var unowned []string
	for p := range scope {
		if r := cur[p]; r.Matched {
			groups[r.LineIndex] = true
		} else {
			unowned = append(unowned, p)
		}
	}

	var lines []int
	for l := range groups {
		lines = append(lines, l)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(lines)))

	for _, l := range lines {
		r := f.Lines[l].Rule
		if contains(r.Owners, op.Owner) {
			continue
		}
		if subset(winners[l], scope) {
			old := f.LineText(l)
			newOwners := append(append([]string{}, r.Owners...), op.Owner)
			f.SetOwners(l, newOwners)
			pl.Changes = append(pl.Changes, Change{
				Action: "amend", Line: l + 1, Pattern: r.PatternText,
				OldOwners: r.OwnersCopy(), NewOwners: newOwners,
				OldLine: old, NewLine: f.LineText(l),
				Reason: fmt.Sprintf("every path governed by %q is inside scope %q; amended in place (R-2/R-4)", r.PatternText, op.Scope),
			})
		} else {
			inter, ok := intersectPattern(op.Scope, r, scope, tree)
			if !ok {
				return &RefusalError{Msg: fmt.Sprintf(
					"refusing: rule %q also governs paths outside scope %q, and no sound narrowing pattern is derivable — amending would violate INV-2, appending would violate INV-1",
					r.PatternText, op.Scope)}
			}
			newOwners := append(append([]string{}, r.Owners...), op.Owner)
			f.InsertRule(l+1, inter, newOwners)
			pl.Changes = append(pl.Changes, Change{
				Action: "insert", Line: l + 2, Pattern: inter,
				OldOwners: r.OwnersCopy(), NewOwners: newOwners,
				NewLine: f.LineText(l + 1),
				Reason:  fmt.Sprintf("rule %q also governs out-of-scope paths; inserted narrowing rule %q immediately after it so out-of-scope resolution is untouched (R-2)", r.PatternText, inter),
			})
		}
	}

	if len(unowned) > 0 {
		at := firstRuleIndex(f)
		f.InsertRule(at, op.Scope, []string{op.Owner})
		pl.Changes = append(pl.Changes, Change{
			Action: "insert", Line: at + 1, Pattern: op.Scope,
			NewOwners: []string{op.Owner}, NewLine: f.LineText(at),
			Reason: fmt.Sprintf("%d in-scope path(s) matched no rule; inserted before all existing rules so every existing rule keeps precedence (last-match-wins, S-1)", len(unowned)),
		})
	}

	for p := range scope {
		if !contains(desired[p], op.Owner) {
			if desired[p] == nil {
				desired[p] = []string{op.Owner}
			} else {
				desired[p] = append(append([]string{}, desired[p]...), op.Owner)
			}
		}
	}
	return nil
}

func synthSet(f *file.File, tree []string, op ops.Op, scope map[string]bool, desired map[string][]string, pl *Plan) error {
	warnShadowedDuplicates(f, tree, scope, pl)
	rules := f.Rules()

	// Rules whose MATCH SET (not winner set) intersects scope — R-3 is about
	// recapture, and any matching later rule recaptures.
	lastIntersecting := -1
	var lastRule *file.Rule
	for _, r := range rules {
		for _, p := range tree {
			if scope[p] && r.Pattern.Match(p) {
				lastIntersecting = r.LineIndex
				lastRule = r
				break
			}
		}
	}

	if lastRule != nil && matchSetEquals(lastRule, tree, scope) {
		if resolve.OwnersEqual(lastRule.OwnersCopy(), op.Owners) {
			// Already exact; nothing to do for this op.
		} else {
			old := f.LineText(lastIntersecting)
			oldOwners := lastRule.OwnersCopy()
			f.SetOwners(lastIntersecting, op.Owners)
			pl.Changes = append(pl.Changes, Change{
				Action: "amend", Line: lastIntersecting + 1, Pattern: lastRule.PatternText,
				OldOwners: oldOwners, NewOwners: op.Owners,
				OldLine: old, NewLine: f.LineText(lastIntersecting),
				Reason: fmt.Sprintf("existing rule %q matches exactly the scope and is the last intersecting rule — amended in place (R-4); no later rule can recapture (R-3)", lastRule.PatternText),
			})
		}
	} else {
		at := lastIntersecting + 1
		if lastIntersecting < 0 {
			at = len(f.Lines)
		}
		f.InsertRule(at, op.Scope, op.Owners)
		pl.Changes = append(pl.Changes, Change{
			Action: "insert", Line: at + 1, Pattern: op.Scope,
			NewOwners: op.Owners, NewLine: f.LineText(at),
			Reason: "inserted after the last rule whose match set intersects the scope, so no later rule recaptures any in-scope path (R-3)",
		})
	}

	for p := range scope {
		desired[p] = append([]string{}, op.Owners...)
	}
	return nil
}

func synthRemove(f *file.File, tree []string, op ops.Op, scope map[string]bool, desired map[string][]string, onEmpty string, pl *Plan) error {
	warnShadowedDuplicates(f, tree, scope, pl)

	// Removal must run to a FIXPOINT: deleting a rule under --on-empty=inherit
	// can expose an EARLIER rule that also lists the owner (found by property
	// testing, T-4/T-5). Each pass strictly reduces the lines listing the
	// owner that can win for in-scope paths, so this terminates.
	maxPasses := len(f.Lines) + 2
	for pass := 0; ; pass++ {
		if pass > maxPasses {
			return &RefusalError{Msg: fmt.Sprintf(
				"refusing: remove_owner(%s, %s) did not converge — file structure defeats the writer", op.Scope, op.Owner)}
		}
		changed, err := removePass(f, tree, op, scope, desired, onEmpty, pl)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
	}
}

// removePass performs one round of removal edits; reports whether any edit
// was made.
func removePass(f *file.File, tree []string, op ops.Op, scope map[string]bool, desired map[string][]string, onEmpty string, pl *Plan) (bool, error) {
	cur := resolve.All(f, tree)
	winners := winnersByLine(cur)

	groups := map[int][]string{}
	for p := range scope {
		if r := cur[p]; r.Matched && contains(f.Lines[r.LineIndex].Rule.Owners, op.Owner) {
			groups[r.LineIndex] = append(groups[r.LineIndex], p)
		}
	}
	if len(groups) == 0 {
		return false, nil
	}
	var lines []int
	for l := range groups {
		lines = append(lines, l)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(lines)))

	for _, l := range lines {
		r := f.Lines[l].Rule
		newOwners := minus(r.Owners, op.Owner)
		if subset(winners[l], scope) {
			if len(newOwners) > 0 {
				old := f.LineText(l)
				oldOwners := r.OwnersCopy()
				f.SetOwners(l, newOwners)
				pl.Changes = append(pl.Changes, Change{
					Action: "amend", Line: l + 1, Pattern: r.PatternText,
					OldOwners: oldOwners, NewOwners: newOwners,
					OldLine: old, NewLine: f.LineText(l),
					Reason: fmt.Sprintf("every path governed by %q is inside scope; removed %s in place", r.PatternText, op.Owner),
				})
				for _, p := range winners[l] {
					desired[p] = minus(desired[p], op.Owner)
				}
				continue
			}
			// Owner set would become empty: explicit policy required (R-6).
			switch onEmpty {
			case "":
				return false, &InvalidError{Msg: fmt.Sprintf(
					"removing %s empties the owner set of %q; an explicit --on-empty policy (error|inherit|unowned) is required — there is deliberately no default (R-6)",
					op.Owner, r.PatternText)}
			case "error":
				return false, &RefusalError{Msg: fmt.Sprintf(
					"refusing: removing %s would leave %q with no owners (--on-empty=error, R-6)", op.Owner, r.PatternText)}
			case "unowned":
				old := f.LineText(l)
				oldOwners := r.OwnersCopy()
				f.SetOwners(l, nil)
				pl.Changes = append(pl.Changes, Change{
					Action: "amend", Line: l + 1, Pattern: r.PatternText,
					OldOwners: oldOwners, NewOwners: []string{},
					OldLine: old, NewLine: f.LineText(l),
					Reason: fmt.Sprintf("owner set emptied; pattern kept with zero owners per --on-empty=unowned — a legal, deliberate un-owning (S-9)"),
				})
				for _, p := range winners[l] {
					desired[p] = []string{}
				}
			case "inherit":
				old := f.LineText(l)
				affected := winners[l]
				pl.Changes = append(pl.Changes, Change{
					Action: "delete", Line: l + 1, Pattern: r.PatternText,
					OldOwners: r.OwnersCopy(), OldLine: old,
					Reason: "owner set emptied; rule deleted per --on-empty=inherit so the preceding broader rule takes over (R-6) — the resulting reassignment appears in the ownership rows",
				})
				f.DeleteLine(l)
				rules := f.Rules()
				for _, p := range affected {
					res := resolve.One(rules, p)
					desired[p] = res.Owners // nil if now unmatched
				}
			default:
				return false, &InvalidError{Msg: fmt.Sprintf("unknown --on-empty policy %q", onEmpty)}
			}
		} else {
			// Rule also governs out-of-scope paths: split via narrowing insert.
			inter, ok := intersectPattern(op.Scope, r, scope, tree)
			if !ok {
				return false, &RefusalError{Msg: fmt.Sprintf(
					"refusing: rule %q also governs paths outside scope %q and no sound narrowing pattern is derivable", r.PatternText, op.Scope)}
			}
			if len(newOwners) == 0 {
				switch onEmpty {
				case "":
					return false, &InvalidError{Msg: fmt.Sprintf(
						"removing %s empties the owner set for in-scope paths of %q; an explicit --on-empty policy is required (R-6)", op.Owner, r.PatternText)}
				case "error":
					return false, &RefusalError{Msg: fmt.Sprintf(
						"refusing: removing %s would leave in-scope paths of %q with no owners (--on-empty=error)", op.Owner, r.PatternText)}
				case "inherit":
					return false, &RefusalError{Msg: fmt.Sprintf(
						"refusing: --on-empty=inherit cannot be expressed for %q, which also governs out-of-scope paths — inheritance would require deleting a rule other paths depend on (R-1)", r.PatternText)}
				case "unowned":
					// A zero-owner narrowing rule expresses it exactly (S-9).
				default:
					return false, &InvalidError{Msg: fmt.Sprintf("unknown --on-empty policy %q", onEmpty)}
				}
			}
			f.InsertRule(l+1, inter, newOwners)
			pl.Changes = append(pl.Changes, Change{
				Action: "insert", Line: l + 2, Pattern: inter,
				OldOwners: r.OwnersCopy(), NewOwners: newOwners,
				NewLine: f.LineText(l + 1),
				Reason:  fmt.Sprintf("rule %q also governs out-of-scope paths; inserted narrowing rule %q after it (R-2); out-of-scope resolution untouched", r.PatternText, inter),
			})
			for _, p := range groups[l] {
				if len(newOwners) == 0 {
					desired[p] = []string{}
				} else {
					desired[p] = minus(desired[p], op.Owner)
				}
			}
		}
	}
	return true, nil
}

func synthRename(f *file.File, op ops.Op, desired map[string][]string, pl *Plan) error {
	for i, ln := range f.Lines {
		if ln.Kind != file.LineRule || !contains(ln.Rule.Owners, op.Owner) {
			continue
		}
		old := f.LineText(i)
		oldOwners := ln.Rule.OwnersCopy()
		newOwners := minus(ln.Rule.Owners, op.Owner)
		if !contains(newOwners, op.NewOwner) {
			newOwners = append(newOwners, op.NewOwner)
		}
		f.SetOwners(i, newOwners)
		pl.Changes = append(pl.Changes, Change{
			Action: "amend", Line: i + 1, Pattern: ln.Rule.PatternText,
			OldOwners: oldOwners, NewOwners: newOwners,
			OldLine: old, NewLine: f.LineText(i),
			Reason: fmt.Sprintf("global rename %s → %s: pure identifier substitution cannot change any rule's match set (§4.1)", op.Owner, op.NewOwner),
		})
	}
	for p, own := range desired {
		if contains(own, op.Owner) {
			out := minus(own, op.Owner)
			if !contains(out, op.NewOwner) {
				out = append(out, op.NewOwner)
			}
			desired[p] = out
		}
	}
	return nil
}

// intersectPattern derives a pattern matching exactly (scope ∩ rule) for the
// shapes that arise in practice. Returns ok=false when no sound derivation
// exists — the caller refuses rather than guesses; the gate re-proves
// whatever is produced.
//
// Shapes handled:
//  1. scope ⊆ match(rule) over the tree (e.g. /services/api/ inside
//     /services/): the scope itself IS the intersection. Sound as a lone
//     insert: since every in-scope path matches the rule at line L, every
//     in-scope path's winner is ≥ L, so no other group needs an insert that
//     could recapture these paths.
//  2. anchored directory scope × unanchored single-segment pattern
//     (e.g. /x/ × *.tf → /x/**/*.tf).
//  3. anchored pattern already inside the scope prefix: the pattern itself.
func intersectPattern(scope string, rule *file.Rule, scopeSet map[string]bool, tree []string) (string, bool) {
	subset := true
	for p := range scopeSet {
		if !rule.Pattern.Match(p) {
			subset = false
			break
		}
	}
	if subset {
		return scope, true
	}
	prefix, ok := anchoredDirPrefix(scope)
	if !ok {
		return "", false
	}
	pat := rule.PatternText
	if strings.HasPrefix(pat, "/") {
		if strings.HasPrefix(pat, prefix) {
			return pat, true
		}
		return "", false
	}
	seg := strings.TrimPrefix(pat, "**/")
	body := strings.TrimSuffix(seg, "/")
	if body == "" || strings.Contains(body, "/") {
		return "", false
	}
	return prefix + "**/" + seg, true
}

// anchoredDirPrefix normalizes "/x/", "/x/**", "/x" to the prefix "/x/".
func anchoredDirPrefix(scope string) (string, bool) {
	if !strings.HasPrefix(scope, "/") {
		return "", false
	}
	s := strings.TrimSuffix(scope, "**")
	if !strings.HasSuffix(s, "/") {
		s += "/"
	}
	if strings.ContainsAny(s, "*?[]\\") {
		return "", false
	}
	return s, true
}

// warnShadowedDuplicates reports duplicate patterns intersecting the scope
// (R-7): the effective one gets edited; fixing duplicates is Engine B's job.
func warnShadowedDuplicates(f *file.File, tree []string, scope map[string]bool, pl *Plan) {
	rules := f.Rules()
	byPattern := map[string][]*file.Rule{}
	for _, r := range rules {
		byPattern[r.PatternText] = append(byPattern[r.PatternText], r)
	}
	for pat, rs := range byPattern {
		if len(rs) < 2 {
			continue
		}
		intersects := false
		for _, p := range tree {
			if scope[p] && rs[0].Pattern.Match(p) {
				intersects = true
				break
			}
		}
		if !intersects {
			continue
		}
		for _, shadowed := range rs[:len(rs)-1] {
			w := fmt.Sprintf("line %d: duplicate pattern %q is shadowed by line %d; edited only the effective rule (R-7) — run `audit` to clean up duplicates",
				shadowed.LineIndex+1, pat, rs[len(rs)-1].LineIndex+1)
			if !containsStr(pl.Warnings, w) {
				pl.Warnings = append(pl.Warnings, w)
			}
		}
	}
}

func winnersByLine(res map[string]resolve.Resolution) map[int][]string {
	out := map[int][]string{}
	for p, r := range res {
		if r.Matched {
			out[r.LineIndex] = append(out[r.LineIndex], p)
		}
	}
	return out
}

func firstRuleIndex(f *file.File) int {
	for i, ln := range f.Lines {
		if ln.Kind == file.LineRule {
			return i
		}
	}
	return len(f.Lines)
}

func matchSetEquals(r *file.Rule, tree []string, scope map[string]bool) bool {
	n := 0
	for _, p := range tree {
		if r.Pattern.Match(p) {
			if !scope[p] {
				return false
			}
			n++
		}
	}
	return n == len(scope)
}

func subset(paths []string, set map[string]bool) bool {
	for _, p := range paths {
		if !set[p] {
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func containsStr(list []string, s string) bool { return contains(list, s) }

func minus(list []string, s string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func fmtOwners(o []string) string {
	if o == nil {
		return "(unowned: no rule matches)"
	}
	if len(o) == 0 {
		return "{} (explicitly zero owners)"
	}
	return "{" + strings.Join(o, ", ") + "}"
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
