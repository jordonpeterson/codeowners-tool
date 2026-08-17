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

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
	"github.com/jordonpeterson/codeowners-tool/internal/resolve"
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

// OpResult is one op's outcome (R-24). Proven distinguishes an op checked
// against real tracked files ("tree") from a declare op that matched none
// and could only be proven structurally ("structural", INV-6).
type OpResult struct {
	ID     string `json:"id,omitempty"`
	Op     string `json:"op"`
	Status string `json:"status"`           // applied | skipped | unchanged
	Proven string `json:"proven,omitempty"` // tree | structural
	Reason string `json:"reason,omitempty"`
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

	// Named op_results, not ops: Ops above already owns the "ops" tag as the
	// raw op strings (R-16) and must keep it. The sync record renders these
	// as "ops" in ITS document, where there is no collision.
	OpResults []OpResult `json:"op_results,omitempty"`
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

	// Per-op scope path sets (R-5: empty scope is invalid input, unless a
	// policy op opts out per R-21).
	scopeSets := make([]map[string]bool, len(opList))
	skipped := make([]bool, len(opList))
	declared := make([]bool, len(opList))
	for i, op := range opList {
		set := map[string]bool{}
		if op.Kind == ops.RenameOwner {
			// R-21 never reaches a rename: its scope comes from current
			// ownership, not from a pattern, so there is no zero-match branch
			// to take here. Rejecting on_zero_match on a rename is a static
			// property of the policy and belongs to internal/policy.
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
				switch op.OnZeroMatch {
				case ops.ZeroMatchSkip:
					// The op stays in opList with an empty scope set rather
					// than being filtered out. Filtering would drop its raw
					// string from Plan.Ops, which is R-16's record of what was
					// REQUESTED, and an all-skip batch would fall into "no
					// operations supplied" (exit 3) where R-21 requires a
					// per-repo no-op (exit 1).
					skipped[i] = true
				case ops.ZeroMatchDeclare:
					if op.Kind == ops.RemoveOwner {
						return nil, &InvalidError{Msg: fmt.Sprintf(
							"on_zero_match=declare is meaningless on %s: there is no rule to remove an owner from (R-21)", op.Raw)}
					}
					declared[i] = true
				default:
					// "" and "require" are the same state, and this arm — not a
					// comparison against "require" — is what makes that true:
					// every op parsed from --op carries "", and R-21's
					// compatibility guarantee is that those keep hitting R-5
					// exactly as before the field existed.
					return nil, &InvalidError{Msg: fmt.Sprintf("scope %q matches zero tracked files (R-5: refusing to create a dead rule)", op.Scope)}
				}
			}
		}
		scopeSets[i] = set
	}

	// A rename's scope comes from the BEFORE state, but a sibling op can hand
	// its old identifier to paths that did not carry it — and the rename would
	// then rewrite those paths too. Left stale, an identifier that owns nothing
	// yet yields an EMPTY set, R-8 below finds no overlap, and an
	// order-dependent batch sails through at exit 0. Widen each rename to every
	// path any other op could assign its old owner to, to a fixpoint since
	// renames chain (found by multi-agent review).
	for changed := true; changed; {
		changed = false
		for i, op := range opList {
			if op.Kind != ops.RenameOwner {
				continue
			}
			for j, other := range opList {
				// Under --on-empty=inherit a removal DELETES a rule, which
				// resurrects whatever shadowed line sat behind it and can hand
				// arbitrary identifiers — including this rename's old one — to
				// its paths. That is unmodellable as an owner-set transform, so
				// treat any such removal as owner-assigning: widening makes the
				// existing inherit guard below fire (round-2 review, critical).
				assigns := assignsOwner(other, op.Owner) ||
					(opts.OnEmpty == "inherit" && other.Kind == ops.RemoveOwner)
				if i == j || !assigns {
					continue
				}
				for p := range scopeSets[j] {
					if !scopeSets[i][p] {
						scopeSets[i][p] = true
						changed = true
					}
				}
			}
		}
	}

	// R-8: reject order-dependent overlapping batches.
	for i := 0; i < len(opList); i++ {
		for j := i + 1; j < len(opList); j++ {
			// Range the tree, not the scope-set map: map order is randomised,
			// so naming the offending path from it made one fixed batch emit a
			// different message run to run (round-4 review).
			for _, p := range tree {
				if !scopeSets[i][p] || !scopeSets[j][p] {
					continue
				}
				// simulate() cannot model --on-empty=inherit (inheritance
				// resurrects owners from OTHER rules, which a pure owner-set
				// transform cannot see), so a remove overlapping anything
				// under inherit is unprovably order-independent — reject it
				// rather than resolve by input order (found in review).
				if opts.OnEmpty == "inherit" &&
					(opList[i].Kind == ops.RemoveOwner || opList[j].Kind == ops.RemoveOwner) {
					return nil, &InvalidError{Msg: fmt.Sprintf(
						"ops %q and %q overlap on %q and --on-empty=inherit makes their order-independence unprovable (R-8) — run them as separate invocations",
						opList[i].Raw, opList[j].Raw, p)}
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

	// R-8 for zero-match ops. The loop above intersects TREE path sets, and a
	// declared scope owns none — so two contradictory declares meet on the
	// empty set, are waved through as "commuting", and get written in input
	// order with last-match-wins silently picking the winner. There is no path
	// to test, so decide these pairs over the patterns and the transforms
	// instead. Skipped ops write nothing and commute with everything; pairs of
	// ordinary ops are left entirely to the check above, so nothing that
	// planned before this existed can start refusing now.
	for i := 0; i < len(opList); i++ {
		for j := i + 1; j < len(opList); j++ {
			if skipped[i] || skipped[j] || (!declared[i] && !declared[j]) {
				continue
			}
			if patternsProvablyDisjoint(opList[i].Scope, opList[j].Scope) {
				continue
			}
			if !commuteOnEveryOwnerSet(opList[i], opList[j]) {
				return nil, &InvalidError{Msg: fmt.Sprintf(
					"ops %q and %q can both govern a path that does not exist yet and do not commute (R-8: refusing order-dependent batch)",
					opList[i].Raw, opList[j].Raw)}
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

	// Synthesize edits op by op on the evolving file. A declare op runs against
	// the SAME evolving file, which is what lets two declares on one scope
	// merge into a single rule instead of stacking two lines where the second
	// shadows the first.
	//
	// The scopes this batch declares are collected UP FRONT: INV-6's third
	// obligation is relaxed for overlaps between them (see
	// classifyDeclareShadow), and an op must be able to see a scope declared
	// later in the policy than itself, on the pass that writes the lines and on
	// every pass after it.
	var batchDeclares []string
	for i, op := range opList {
		if declared[i] {
			batchDeclares = append(batchDeclares, op.Scope)
		}
	}
	batch := newDeclareBatch(batchDeclares)
	var declares []*declareCheck
	for i, op := range opList {
		mark := len(pl.Changes)
		var err error
		switch {
		case skipped[i]:
			// R-21: a skipped op changes nothing and does not stop the rest of
			// the batch from applying.
		case declared[i]:
			err = synthDeclare(f, op, batch, &declares, pl)
		default:
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
		}
		if err != nil {
			return nil, err
		}
		pl.OpResults = append(pl.OpResults, opResultFor(op, skipped[i], declared[i], len(pl.Changes) > mark))
	}

	// ASSERT: the gate. Serialize, RE-PARSE, and re-resolve over the real
	// tree, proving INV-1/INV-2 against the independently computed desired
	// state. Gating on the re-parsed bytes — not the in-memory model — is
	// essential: it also proves the plan survives serialization (a rule that
	// re-parses differently than it was modeled is caught here, not on disk;
	// found in review).
	afterBytes := f.Bytes()
	after := resolve.All(file.Parse(afterBytes), tree)
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

	// INV-6. The loop above ranged the tree; for a declared scope it ranged
	// nothing, so INV-1 came out true without a single statement having been
	// made about the line just written — precisely the case a reviewer most
	// needs told. Prove it structurally instead, or refuse.
	if err := proveDeclares(file.Parse(afterBytes), declares, batch, pl); err != nil {
		return nil, err
	}

	if bytes.Equal(afterBytes, content) {
		// A populated plan, not nil: "already correct" is the modal outcome of
		// a scheduled fleet run and must still report one result per op, or the
		// sync record cannot say which repos are converged. Callers that only
		// check err are unaffected. Nothing moved, so nothing was applied —
		// an op whose synthesized edit rendered byte-identical text is reported
		// as unchanged rather than left claiming a change nobody can see.
		for k := range pl.OpResults {
			if pl.OpResults[k].Status == "applied" {
				pl.OpResults[k].Status = "unchanged"
			}
		}
		pl.AfterContent = string(afterBytes)
		pl.SizeAfter = len(afterBytes)
		return pl, &NoOpError{Msg: "nothing to change: file already satisfies the requested ops"}
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

// ruleIsUniversal reports whether a rule matches every path in any tree, which
// is what makes returning an unanchored scope verbatim sound for future trees
// as well as the current one.
//
// Deliberately NOT "**": a lone `**` is the one spelling that does not get an
// implicit `**/` prefix, so it compiles to `\A.+\z`, and Go's `.` excludes
// "\n" while `[^/]` does not. `**` therefore fails to match a path with a
// newline in its last segment where `*` and `**/*` both match it. The
// vendored hmarr oracle agrees, so this is matcher fidelity, not a local bug —
// but claiming universality here would emit an unanchored scope verbatim under
// a rule that does not in fact govern every path (round-3 review).
func ruleIsUniversal(pat string) bool {
	return pat == "*" || pat == "**/*"
}

// scopeContainedInRule reports whether every path matching `scope` also matches
// `rule` in ANY tree — the property shape 1 actually needs before it may return
// the scope verbatim. Rule universality is only the degenerate case of it;
// testing universality alone refused contained-but-not-universal pairs like
// `README.md` under `*.md` (round-4 review).
//
// Decided structurally, never over the tree, and deliberately incomplete: it
// establishes containment for the shapes that occur in practice and answers
// false whenever it cannot prove it, so an unproven pair still refuses.
func scopeContainedInRule(scope, rulePat string) bool {
	if ruleIsUniversal(rulePat) {
		return true
	}
	// Both must match at any depth, i.e. be single-segment (an explicit "**/"
	// prefix is the same thing spelled out). A "/" anywhere else anchors or
	// deepens the pattern and this cheap test no longer applies.
	r := strings.TrimPrefix(rulePat, "**/")
	s := strings.TrimPrefix(scope, "**/")
	if strings.Contains(r, "/") || strings.Contains(s, "/") {
		return false
	}
	// Rule of the form "*<literal>": containment reduces to a suffix test,
	// since neither "*" nor "?" ever matches "/".
	if !strings.HasPrefix(r, "*") {
		return false
	}
	suffix := r[1:]
	if suffix == "" || strings.ContainsAny(suffix, "*?") {
		return false
	}
	return strings.HasSuffix(s, suffix)
}

// assignsOwner reports whether op can give `owner` to a path in its scope —
// i.e. whether a rename of `owner` batched alongside it could be widened by it.
func assignsOwner(op ops.Op, owner string) bool {
	switch op.Kind {
	case ops.AddOwner:
		return op.Owner == owner
	case ops.SetOwners:
		return contains(op.Owners, owner)
	case ops.RenameOwner:
		return op.NewOwner == owner
	}
	return false
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
		if pattern.Contains(op.Scope, r.PatternText) {
			old := f.LineText(l)
			// Capture BEFORE SetOwners mutates the rule — r aliases the live
			// rule, so a post-mutation OwnersCopy reports the new set as the
			// old one (E2E-testing finding).
			oldOwners := r.OwnersCopy()
			newOwners := append(append([]string{}, r.Owners...), op.Owner)
			f.SetOwners(l, newOwners)
			pl.Changes = append(pl.Changes, Change{
				Action: "amend", Line: l + 1, Pattern: r.PatternText,
				OldOwners: oldOwners, NewOwners: newOwners,
				OldLine: old, NewLine: f.LineText(l),
				Reason: fmt.Sprintf("pattern %q can only ever match paths inside scope %q; amended in place (R-2/R-4)", r.PatternText, op.Scope),
			})
		} else {
			inter, exact, ok := intersectPattern(op.Scope, r, scope, tree)
			if !ok {
				return &RefusalError{Msg: fmt.Sprintf(
					"refusing: rule %q also governs paths outside scope %q, and no sound narrowing pattern is derivable — amending would violate INV-2, appending would violate INV-1",
					r.PatternText, op.Scope)}
			}
			if !exact {
				pl.addWarning(inexactNarrowingWarning(inter, op.Scope, r.PatternText))
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

	// Semantic no-op: if every in-scope path ALREADY resolves to exactly the
	// requested owner set, edit nothing — an intent-level tool must not
	// insert redundant rules just because no byte-identical rule exists
	// (second-review finding: this previously exited 0 with a pointless
	// insert instead of 1).
	cur := resolve.All(f, tree)
	satisfied := true
	for p := range scope {
		if !resolve.OwnersEqual(cur[p].Owners, op.Owners) {
			satisfied = false
			break
		}
	}
	if satisfied {
		for p := range scope {
			desired[p] = append([]string{}, op.Owners...)
		}
		return nil
	}

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

	// Amend only when the rule governs EXACTLY the scope as a pattern. Equal
	// match sets over the tracked tree are not enough: set_owners replaces the
	// owner set outright, so amending a rule that merely looks scope-sized
	// today hands every future file it matches to the new owners and strips
	// whoever owned them (review finding — the reported bug, in set_owners).
	if lastRule != nil && samePatternLanguage(op.Scope, lastRule.PatternText) {
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
		changed, err := removePass(f, tree, op, scope, onEmpty, pl)
		if err != nil {
			return err
		}
		if !changed {
			break
		}
	}

	// Desired state for removal is settled AFTER the fixpoint, not
	// incrementally per pass: a pass may delete a rule (recording its
	// fallthrough) and then amend that very fallthrough rule in the same
	// pass, so per-pass snapshots go stale and produce spurious refusals
	// (found in review). Where the outcome is the pure transform
	// desired∖{owner}, keep it — that preserves the gate's independence.
	// Where inheritance legitimately resurrected other owners, assert the
	// removal contract (op.Owner owns nothing in scope — INV-1 for remove)
	// and accept the fallthrough set. INV-2 is untouched: desired outside
	// scope never changes here.
	final := resolve.All(f, tree)
	for p := range scope {
		want := minus(desired[p], op.Owner)
		got := final[p].Owners
		if resolve.OwnersEqual(got, want) {
			desired[p] = want
			continue
		}
		if contains(got, op.Owner) {
			return &RefusalError{Msg: fmt.Sprintf(
				"refusing: %s would still own %q after remove_owner(%s, %s)", op.Owner, p, op.Scope, op.Owner)}
		}
		// Divergence from the pure transform is legitimate ONLY under
		// inherit, where deleting a rule resurrects owners from surviving
		// rules. Under every other policy no rule is deleted, so divergence
		// means a synthesis bug (or a bad earlier batched edit) — accepting
		// it here would launder the error past the gate (second-review
		// finding: this exact acceptance weakened the gate). Refuse instead.
		if onEmpty != "inherit" {
			return &RefusalError{Msg: fmt.Sprintf(
				"refusing: remove_owner(%s, %s) produced %s for %q where the operation's semantics require %s",
				op.Scope, op.Owner, fmtOwners(got), p, fmtOwners(want))}
		}
		desired[p] = got // inherit fallthrough: owners come from surviving rules
	}
	return nil
}

// removePass performs one round of removal edits; reports whether any edit
// was made.
func removePass(f *file.File, tree []string, op ops.Op, scope map[string]bool, onEmpty string, pl *Plan) (bool, error) {
	cur := resolve.All(f, tree)

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
		if pattern.Contains(op.Scope, r.PatternText) {
			if len(newOwners) > 0 {
				old := f.LineText(l)
				oldOwners := r.OwnersCopy()
				f.SetOwners(l, newOwners)
				pl.Changes = append(pl.Changes, Change{
					Action: "amend", Line: l + 1, Pattern: r.PatternText,
					OldOwners: oldOwners, NewOwners: newOwners,
					OldLine: old, NewLine: f.LineText(l),
					Reason: fmt.Sprintf("pattern %q can only ever match paths inside scope; removed %s in place", r.PatternText, op.Owner),
				})
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
			case "inherit":
				old := f.LineText(l)
				pl.Changes = append(pl.Changes, Change{
					Action: "delete", Line: l + 1, Pattern: r.PatternText,
					OldOwners: r.OwnersCopy(), OldLine: old,
					Reason: "owner set emptied; rule deleted per --on-empty=inherit so the preceding broader rule takes over (R-6) — the resulting reassignment appears in the ownership rows",
				})
				f.DeleteLine(l)
			default:
				return false, &InvalidError{Msg: fmt.Sprintf("unknown --on-empty policy %q", onEmpty)}
			}
		} else {
			// Rule also governs out-of-scope paths: split via narrowing insert.
			inter, exact, ok := intersectPattern(op.Scope, r, scope, tree)
			if !ok {
				return false, &RefusalError{Msg: fmt.Sprintf(
					"refusing: rule %q also governs paths outside scope %q and no sound narrowing pattern is derivable", r.PatternText, op.Scope)}
			}
			if !exact {
				pl.addWarning(inexactNarrowingWarning(inter, op.Scope, r.PatternText))
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
		newOwners := substituteOwner(ln.Rule.Owners, op.Owner, op.NewOwner)
		f.SetOwners(i, newOwners)
		pl.Changes = append(pl.Changes, Change{
			Action: "amend", Line: i + 1, Pattern: ln.Rule.PatternText,
			OldOwners: oldOwners, NewOwners: newOwners,
			OldLine: old, NewLine: f.LineText(i),
			Reason: fmt.Sprintf("global rename %s → %s: pure identifier substitution cannot change any rule's match set (§4.1)", op.Owner, op.NewOwner),
		})
	}
	// The desired state substitutes the same way, or the gate would compare the
	// file's in-place order against an appended-at-the-end expectation and
	// refuse every rename.
	for p, own := range desired {
		if contains(own, op.Owner) {
			desired[p] = substituteOwner(own, op.Owner, op.NewOwner)
		}
	}
	return nil
}

// intersectPattern derives a pattern matching exactly (scope ∩ rule) and
// PROVES it before returning. deriveIntersection supplies the candidate
// shapes; this layer is what makes them trustworthy.
//
// The load-bearing check is pattern containment: the candidate must not reach
// outside the scope (which would grant the new owner beyond what was asked)
// nor outside the rule (which would grant the RULE's owners paths it never
// governed). Both are claims about files that do not exist yet, so no tree can
// establish them.
//
// exact=false means the candidate is right for every tracked file but not
// provably confined for future ones — the caller discloses it. CODEOWNERS
// genuinely cannot express some intersections: a `dir/*` rule governs one
// level, and no pattern says "one level AND matching *.gradle", since
// `dir/*.gradle` also matches under a DIRECTORY named `.gradle`. Refusing
// outright would make add_owner(*.gradle, …) impossible on any repo with a
// `dir/*` rule.
func intersectPattern(scope string, rule *file.Rule, scopeSet map[string]bool, tree []string) (inter string, exact bool, ok bool) {
	var cands []string
	if c, got := deriveIntersection(scope, rule, scopeSet, tree); got {
		cands = append(cands, c)
	}
	// deriveIntersection rejects one-level (`dir/*`) rules outright, since no
	// subtree prefix describes them. The closest expressible narrowing is the
	// glob at that same level; it is inexact only for descendants of a
	// directory whose own name matches the glob.
	if seg, got := basenameGlob(scope); got {
		if d, got := oneLevelRuleDir(rule.PatternText); got {
			cands = append(cands, d+seg)
		}
	}
	for _, c := range cands {
		if treeExact(c, rule, scopeSet, tree) &&
			pattern.Contains(scope, c) && pattern.Contains(rule.PatternText, c) {
			return c, true, true
		}
	}
	for _, c := range cands {
		if treeExact(c, rule, scopeSet, tree) {
			return c, false, true
		}
	}
	return "", false, false
}

// oneLevelRuleDir returns the directory prefix of a rule that governs a
// directory's DIRECT CHILDREN (`path/app/*`), preserving the rule's own
// anchoring so the derived pattern matches at the same depth.
func oneLevelRuleDir(pat string) (string, bool) {
	if pat == "*" || !strings.HasSuffix(pat, "/*") || strings.HasSuffix(pat, "/**/*") {
		return "", false
	}
	return strings.TrimSuffix(pat, "*"), true
}

// treeExact reports whether a candidate matches exactly (scope ∩ rule) over the
// tracked tree. It pins down an under-narrow guess with a clear refusal instead
// of letting it loop in removePass's fixpoint or surface as a gate violation,
// but says nothing about files that do not exist yet.
func treeExact(cand string, rule *file.Rule, scopeSet map[string]bool, tree []string) bool {
	cp, err := pattern.Compile(cand)
	if err != nil {
		return false
	}
	for _, p := range tree {
		if cp.Match(p) != (scopeSet[p] && rule.Pattern.Match(p)) {
			return false
		}
	}
	return true
}

// basenameGlob recognizes a single-segment pattern — one that matches a
// basename at any depth.
func basenameGlob(pat string) (string, bool) {
	if pat == "" || pat == "**" || strings.Contains(pat, "/") {
		return "", false
	}
	return pat, true
}

// samePatternLanguage reports whether two patterns match exactly the same set
// of paths.
func samePatternLanguage(a, b string) bool {
	return pattern.Contains(a, b) && pattern.Contains(b, a)
}

// inexactNarrowingWarning explains a narrowing rule that is exact for every
// tracked file but not provably exact for files that do not exist yet.
func inexactNarrowingWarning(inter, scope, rulePat string) string {
	return fmt.Sprintf(
		"narrowing rule %q is exact for every tracked file, but is not provably confined to %q ∩ %q for files added later; "+
			"a future path matching %q that %q does not govern would also pick up that rule's owners",
		inter, scope, rulePat, inter, rulePat)
}

// addWarning appends a warning, skipping exact duplicates.
func (p *Plan) addWarning(w string) {
	if !containsStr(p.Warnings, w) {
		p.Warnings = append(p.Warnings, w)
	}
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
//     could recapture these paths. An UNANCHORED scope is returned verbatim
//     only when scopeContainedInRule proves containment structurally — the
//     tree-scoped `subset` above says nothing about the trees this line will
//     outlive, and verbatim under a rule that does not match every path is
//     repo-global.
//  2. anchored directory scope × unanchored single-segment pattern
//     (e.g. /x/ × *.tf → /x/**/*.tf).
//  3. anchored pattern already inside the scope prefix: the pattern itself.
//  4. the mirror of 2 — unanchored single-segment SCOPE glob × directory rule
//     (e.g. *.gradle × /app2 → /app2/**/*.gradle). The monorepo shape: a
//     file-type scope cutting across per-directory rules. Unlike 1–3 the
//     derivation can under-match (a rule whose own last segment matches the
//     scope glob, e.g. /app* × *.gradle over a root file apple.gradle), so it
//     is confirmed against the tree before being returned.
func deriveIntersection(scope string, rule *file.Rule, scopeSet map[string]bool, tree []string) (string, bool) {
	subset := true
	for p := range scopeSet {
		if !rule.Pattern.Match(p) {
			subset = false
			break
		}
	}
	if subset {
		// `subset` is established over the CURRENT tree, but the pattern
		// returned here is written to a file that outlives it. An UNANCHORED
		// scope matches at any depth, so under a rule that does not, the
		// verbatim scope is repo-global: it hands that rule's owners every
		// matching file added anywhere later. Verbatim is sound only where
		// containment is proven STRUCTURALLY; otherwise derive the anchored
		// form, and refuse if none exists rather than emit the over-broad one
		// (round-2 review — the tree-scoped reasoning here was wrong for every
		// unanchored spelling, not just globbed ones).
		if !strings.HasPrefix(scope, "/") && !scopeContainedInRule(scope, rule.PatternText) {
			return globScopeIntersect(scope, rule, scopeSet, tree)
		}
		return scope, true
	}
	prefix, ok := anchoredDirPrefix(scope)
	if !ok {
		return globScopeIntersect(scope, rule, scopeSet, tree)
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

// globScopeIntersect derives shape 4: an unanchored single-segment scope glob
// (*.gradle) intersected with a rule that governs a directory subtree, giving
// <ruleDir>/**/<scope>. Returns ok=false unless the derived pattern is legal
// and matches exactly (scope ∩ rule) over the tree — the caller refuses on
// false, so an inexact derivation degrades to a refusal, never a wrong write.
func globScopeIntersect(scope string, rule *file.Rule, scopeSet map[string]bool, tree []string) (string, bool) {
	// The scope must be a bare basename glob: anchored or multi-segment scopes
	// cannot be concatenated onto a directory prefix without the two path
	// fragments overlapping.
	if strings.Contains(scope, "/") {
		return "", false
	}
	dir, ok := ruleDirPrefix(rule.PatternText)
	if !ok {
		return "", false
	}
	cand := dir + "**/" + scope
	pat, err := pattern.Compile(cand)
	if err != nil {
		return "", false
	}
	for _, p := range tree {
		want := scopeSet[p] && rule.Pattern.Match(p)
		if pat.Match(p) != want {
			return "", false
		}
	}
	return cand, true
}

// ruleDirPrefix turns a rule pattern that governs a directory subtree into the
// prefix its contents match under: "/x", "/x/", "/x/**" → "/x/"; a
// single-segment unanchored "x" or "x/" matches at any depth → "**/x/"; an
// unanchored pattern with an interior slash is root-anchored, like gitignore.
//
// Anchoring is read from the ORIGINAL spelling, before any affix is trimmed.
// Trimming first destroys the very slash that decides it: "app2/**" is a
// two-segment, root-anchored pattern that never governs vendor/app2/, but
// dropping its "/**" leaves the single segment "app2", which would be derived
// as the any-depth "**/app2/" — a narrowing rule broader than the rule it
// narrows. "**/a/b" is the mirror: explicitly any-depth, yet dropping the
// "**/" leaves "a/b", which would be re-anchored to the repo root.
func ruleDirPrefix(pat string) (string, bool) {
	// A rule ending in a `*`-only segment governs a directory's DIRECT
	// CHILDREN, not its subtree, so it has no subtree prefix at all: deriving
	// "**/x/*/" from "**/x/*" would match arbitrarily deep descendants the rule
	// never governs. The anchored spellings ("/x/*") happen to fail the tree
	// confirmation, but the any-depth ones can pass it whenever the tree does
	// not distinguish the two — so reject the shape outright (round-2 review).
	//
	// "<dir>/**/*" is NOT that shape: it compiles to `\Adir(?:/.+)?/[^/]+\z`,
	// the whole subtree, exactly like "<dir>/**". Rejecting it too was an
	// over-refusal (round-4 review).
	if pat == "*" || (strings.HasSuffix(pat, "/*") && !strings.HasSuffix(pat, "/**/*")) {
		return "", false
	}
	anyDepth := strings.HasPrefix(pat, "**/") ||
		(!strings.HasPrefix(pat, "/") && !strings.Contains(strings.TrimSuffix(pat, "/"), "/"))

	// Trim to a fixpoint so "x/**/" collapses to "x" rather than leaving a
	// "**" that compounds into "/x/**/**/<scope>".
	body := pat
	for {
		t := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(body, "/"), "/**/*"), "/**")
		if t == body {
			break
		}
		body = t
	}
	body = strings.TrimPrefix(body, "**/")
	switch body {
	case "", "/", "*", "**", ".", "..":
		return "", false
	}
	if strings.HasPrefix(body, "/") {
		return body + "/", true
	}
	if anyDepth {
		return "**/" + body + "/", true
	}
	return "/" + body + "/", true
}

// anchoredDirPrefix normalizes an anchored, WILDCARD-FREE directory scope
// ("/x/", "/x/**", "/x") to the prefix "/x/", and rejects everything else. The
// rejection is what keeps patternsProvablyDisjoint honest: the prefix is used
// as a stand-in for the pattern's whole language, which is only true when the
// pattern has no wildcard to spill outside that prefix.
//
// The wildcard check therefore runs on the ORIGINAL spelling, BEFORE any suffix
// is trimmed, and "**" is stripped only where a "/" precedes it. Trimming first
// normalized "/src**" to "/src/", so patternsProvablyDisjoint("/src**",
// "/srcx/") answered true — but "/src**" compiles to
// `\Asrc[^/]*[^/]*(?:/.*)?\z`, which matches srcx/a.go, and so does "/srcx/".
// That is a WRONG WRITE, not a missed proof: it let a declare be amended under
// a later rule capturing its entire scope (reported applied, dead on arrival)
// and let R-8 wave through an order-dependent declare batch whose result
// depended on op order (adversarial audit of Wave 1).
func anchoredDirPrefix(scope string) (string, bool) {
	if !strings.HasPrefix(scope, "/") {
		return "", false
	}
	if strings.ContainsAny(scope, "*?[]\\") {
		// "/x/**" is the one wildcard spelling that is still exactly a directory
		// subtree, and only with the slash intact: in "/src**" the "**" binds to
		// the "src" segment and reaches siblings like srcx/.
		body, ok := strings.CutSuffix(scope, "/**")
		if !ok || strings.ContainsAny(body, "*?[]\\") {
			return "", false
		}
		scope = body
	}
	if !strings.HasSuffix(scope, "/") {
		scope += "/"
	}
	return scope, true
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

func firstRuleIndex(f *file.File) int {
	for i, ln := range f.Lines {
		if ln.Kind == file.LineRule {
			return i
		}
	}
	return len(f.Lines)
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

// substituteOwner replaces old with new IN PLACE, keeping every other owner
// where it was.
//
// Dropping the old identifier and appending the new one gives the same owner
// SET, which is all GitHub reads, but it permutes every line listing the
// renamed team alongside anyone else — and `rename_owner` is documented as pure
// identifier substitution, the one op safe to read as a text diff. A reorg
// across a fleet is only reviewable if that holds.
//
// When new is ALREADY on the line the earliest position is kept and the other
// dropped: `@a @b` under rename(@a → @b) is `@b`, not `@b @b`.
func substituteOwner(list []string, old, new string) []string {
	if list == nil {
		return nil
	}
	// Hoisted: whether old is present cannot change while we walk the list, and
	// testing it per element made a linear substitution quadratic.
	hasOld := containsStr(list, old)
	out := make([]string, 0, len(list))
	done := false
	for _, x := range list {
		switch {
		case x == old:
			if done {
				continue // old listed twice: the second is a duplicate either way
			}
			done = true
			out = append(out, new)
		case x == new && done:
			// Already emitted at the renamed owner's position; this later copy
			// would be a duplicate.
		case x == new && hasOld:
			// new appears BEFORE old on this line: keep it here, and the old
			// identifier's slot collapses when we reach it.
			done = true
			out = append(out, new)
		default:
			out = append(out, x)
		}
	}
	return out
}

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
