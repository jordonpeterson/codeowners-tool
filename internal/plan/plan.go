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
// against real tracked files ("tree") from one that could only be proven
// structurally ("structural", INV-6) — a declare that matched nothing, or an
// except-carrying op that wrote under on_except_zero_match=allow.
type OpResult struct {
	ID     string `json:"id,omitempty"`
	Op     string `json:"op"`
	Status string `json:"status"`           // applied | skipped | unchanged
	Proven string `json:"proven,omitempty"` // tree | structural
	Reason string `json:"reason,omitempty"`

	// R-32, additive and omitempty so records for ops without an except
	// clause stay byte-identical to before the feature existed. Excepted is
	// every tracked excepted path with its resolved owners in the final
	// after-batch state; ExceptUnmatched lists except patterns that matched
	// zero tracked files (reachable with a written file only under
	// on_except_zero_match=allow, R-28).
	Excepted        []ExceptedPath `json:"excepted,omitempty"`
	ExceptUnmatched []string       `json:"except_unmatched,omitempty"`
}

// ExceptedPath is one carved-out path and who holds it after the batch — the
// per-repo surface for the don't-touch-≠-revoke misread: a grantee already
// owning an excepted path is visible here, not silent (R-32).
type ExceptedPath struct {
	Path   string   `json:"path"`
	Owners []string `json:"owners"`
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
	// policy op opts out per R-21). For an except-carrying op the set is the
	// EFFECTIVE scope — {tracked scope matches} \ {tracked except matches}
	// (R-26) — and every mechanism downstream of here (R-8, the rename
	// fixpoint, synthesis, the INV-1/INV-2 gate) ranges over it unchanged:
	// excepted paths are simply out of the op's scope, so the existing
	// invariants, not new machinery, prove them untouched.
	scopeSets := make([]map[string]bool, len(opList))
	exceptSets := make([]map[string]bool, len(opList))
	exceptUnmatched := make([][]string, len(opList))
	structural := make([]bool, len(opList))
	skipped := make([]bool, len(opList))
	declared := make([]bool, len(opList))
	var allowWarnings []string
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
			raw := 0
			for _, p := range tree {
				if pat.Match(p) {
					set[p] = true
					raw++
				}
			}
			excepted := map[string]bool{}
			var zeroPats []string
			for _, e := range op.Except {
				ep, err := pattern.Compile(e)
				if err != nil {
					return nil, &InvalidError{Msg: fmt.Sprintf("invalid except pattern %q: %v", e, err)}
				}
				n := 0
				for _, p := range tree {
					if ep.Match(p) {
						excepted[p] = true
						delete(set, p)
						n++
					}
				}
				if n == 0 {
					zeroPats = append(zeroPats, e)
				}
			}
			exceptSets[i] = excepted
			if len(set) == 0 {
				// R-28's two emptiness questions are ORDERED, and this arm is
				// the first: an empty EFFECTIVE scope is disposed of by
				// on_zero_match, and on_except_zero_match (the else-if below)
				// is never consulted — an op that writes nothing can reopen
				// nothing, which is what keeps "if this repo has /services/"
				// skip-policies from refusing on every repo that lacks the
				// directory.
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
				case ops.ZeroMatchRequire, "":
					// "" and "require" are the same state, and this arm — not a
					// comparison against "require" — is what makes that true:
					// every op parsed from --op carries "", and R-21's
					// compatibility guarantee is that those keep hitting R-5
					// exactly as before the field existed.
					//
					// The two refusals are distinguished deliberately: "scope
					// matches nothing" sends the operator to the scope, while
					// "everything in scope is excepted" is how a too-broad
					// except announces itself — blaming the scope there sends
					// them hunting a typo in the wrong argument (R-28).
					if raw > 0 {
						return nil, &InvalidError{Msg: fmt.Sprintf(
							"scope %q matches %d tracked file(s), but every one of them is excepted — the except clause empties the op in this repo (R-28)", op.Scope, raw)}
					}
					return nil, &InvalidError{Msg: fmt.Sprintf("scope %q matches zero tracked files (R-5: refusing to create a dead rule)", op.Scope)}
				default:
					// Policy parsing validates the field, but the struct is
					// exported: a library caller (or a future value) can carry
					// anything here, and falling through would synthesize
					// nothing while reporting the repo converged — the silent
					// no-op rollout this switch exists to prevent.
					return nil, &InvalidError{Msg: fmt.Sprintf(
						"unknown on_zero_match value %q on %s; legal values are %q, %q, or %q",
						op.OnZeroMatch, op.Raw, ops.ZeroMatchRequire, ops.ZeroMatchSkip, ops.ZeroMatchDeclare)}
				}
			} else if len(zeroPats) > 0 {
				// R-28's second question, reached only when the op WILL write.
				switch op.OnExceptZeroMatch {
				case ops.ExceptZeroMatchAllow:
					// Declare-class weakening of INV-1, marked like one: the
					// grant goes in with no carve for the unmatched pattern,
					// so nothing in this repo can verify the carve-out — the
					// op is proven "structural", listed in except_unmatched
					// (R-32), and warned about. No dead rule is written for
					// the pattern (R-5).
					exceptUnmatched[i] = zeroPats
					structural[i] = true
					for _, e := range zeroPats {
						allowWarnings = append(allowWarnings, fmt.Sprintf(
							"except pattern %q matches zero tracked files; on_except_zero_match=allow writes the grant with NO carve for it, so a matching file created later falls under the grant — a declare-class weakening of INV-1, marked proven=structural (R-28)", e))
					}
				case ops.ExceptZeroMatchRequire, "":
					// "" and "require" are one state, exactly as above. An
					// except that bites nothing means the carve-out this
					// policy promises does not exist here — in the motivating
					// case, granting .github/ to a repo whose CODEOWNERS
					// still sits at the root would reopen the S-8
					// precedence-escalation hole for a later-created
					// /.github/CODEOWNERS.
					return nil, &RefusalError{Msg: fmt.Sprintf(
						"refusing: except pattern %q matches zero tracked files — the carve-out this policy promises does not exist in this repo, and writing the grant without it would reopen the hole the except exists to close (R-28); normalize this repo first, or set on_except_zero_match=allow to accept the weakening", zeroPats[0])}
				default:
					// Same defense as on_zero_match: an unrecognized value is
					// bad input that fails identically everywhere (exit 3),
					// not a per-repo refusal masquerading as require.
					return nil, &InvalidError{Msg: fmt.Sprintf(
						"unknown on_except_zero_match value %q on %s; legal values are %q or %q",
						op.OnExceptZeroMatch, op.Raw, ops.ExceptZeroMatchRequire, ops.ExceptZeroMatchAllow)}
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
	for _, w := range allowWarnings {
		pl.addWarning(w)
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
			// R-29's baseline: the excepted paths' owners on the EVOLVING file,
			// captured before this op edits it — not the before-batch snapshot.
			// A sibling rename ordered earlier must see its rename respected;
			// restating stale before-batch owners in a carve would force a
			// spurious gate refusal.
			var base map[string]exceptBaseline
			if len(exceptSets[i]) > 0 {
				base, err = exceptedBaseline(f, tree, op, exceptSets[i])
			}
			if err == nil {
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
			if err == nil && len(exceptSets[i]) > 0 {
				err = synthCarves(f, tree, op, exceptSets[i], base, pl)
			}
		}
		if err != nil {
			return nil, err
		}
		pl.OpResults = append(pl.OpResults, opResultFor(op, skipped[i], declared[i], structural[i], len(pl.Changes) > mark))
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

	// R-32: the record explains the carve-outs. Owners come from the FINAL
	// after-batch state, not the before snapshot: when a sibling op reassigns
	// a carved path (R-31's layered policy), the after-batch owners are the
	// ones an operator auditing "who ended up holding the carve-outs" needs.
	// Populated before the converged early-return below, because run 2 of a
	// nightly job must keep disclosing the same facts run 1 did (R-19).
	for i, op := range opList {
		if len(op.Except) == 0 {
			continue
		}
		var ex []ExceptedPath
		for _, p := range tree {
			if exceptSets[i][p] {
				ex = append(ex, ExceptedPath{Path: p, Owners: after[p].Owners})
			}
		}
		pl.OpResults[i].Excepted = ex
		pl.OpResults[i].ExceptUnmatched = exceptUnmatched[i]
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
	case ops.AddOwner, ops.SetOwners:
		// Every owner the op names, not just the first: a rename batched with
		// `add_owner(/x/, [@a, @b])` must be widened by it for either name, or
		// the rename's scope goes stale and R-8 waves an order-dependent batch
		// through (R-33).
		return contains(op.Owners, owner)
	case ops.RenameOwner:
		return sameOwner(op.NewOwner, owner)
	}
	return false
}

// simulate applies an op's owner-set transform to one path's owners — used
// only for the R-8 commutativity check.
func simulate(op ops.Op, owners []string) []string {
	switch op.Kind {
	case ops.AddOwner:
		missing := ownersMissing(owners, op.Owners)
		if len(missing) == 0 {
			return owners
		}
		return append(append([]string{}, owners...), missing...)
	case ops.SetOwners:
		return append([]string{}, op.Owners...)
	case ops.RemoveOwner:
		return minusAll(owners, op.Owners)
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
	derive := deriveDomain(op, scope, tree)
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
		// The WHOLE list is folded into this line's owner set before any edit
		// is recorded, so N owners are one hunk (R-33b) and the appended order
		// is the list's own (R-33e). Owners already on the line are dropped
		// here rather than re-appended, which is what keeps a re-run of the
		// same policy byte-identical (R-19).
		missing := ownersMissing(r.Owners, op.Owners)
		if len(missing) == 0 {
			continue
		}
		if pattern.Contains(op.Scope, r.PatternText) {
			old := f.LineText(l)
			// Capture BEFORE SetOwners mutates the rule — r aliases the live
			// rule, so a post-mutation OwnersCopy reports the new set as the
			// old one (E2E-testing finding).
			oldOwners := r.OwnersCopy()
			newOwners := append(append([]string{}, r.Owners...), missing...)
			f.SetOwners(l, newOwners)
			pl.Changes = append(pl.Changes, Change{
				Action: "amend", Line: l + 1, Pattern: r.PatternText,
				OldOwners: oldOwners, NewOwners: newOwners,
				OldLine: old, NewLine: f.LineText(l),
				Reason: fmt.Sprintf("pattern %q can only ever match paths inside scope %q; amended in place (R-2/R-4)", r.PatternText, op.Scope),
			})
		} else {
			inter, exact, ok := intersectPattern(op.Scope, r, derive, tree)
			if !ok {
				return &RefusalError{Msg: fmt.Sprintf(
					"refusing: rule %q also governs paths outside scope %q, and no sound narrowing pattern is derivable — amending would violate INV-2, appending would violate INV-1",
					r.PatternText, op.Scope)}
			}
			if !exact {
				pl.addWarning(inexactNarrowingWarning(inter, op.Scope, r.PatternText))
			}
			newOwners := append(append([]string{}, r.Owners...), missing...)
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
		// One InsertRule carrying the whole list, not one per owner: a second
		// insert for the same scope would append a duplicate pattern whose
		// predecessor is dead on arrival under last-match-wins (R-7/S-1), and
		// the plan would show a line the file never holds (R-33b).
		newOwners := append([]string{}, op.Owners...)
		f.InsertRule(at, op.Scope, newOwners)
		pl.Changes = append(pl.Changes, Change{
			Action: "insert", Line: at + 1, Pattern: op.Scope,
			NewOwners: newOwners, NewLine: f.LineText(at),
			Reason: fmt.Sprintf("%d in-scope path(s) matched no rule; inserted before all existing rules so every existing rule keeps precedence (last-match-wins, S-1)", len(unowned)),
		})
	}

	for p := range scope {
		for _, o := range ownersMissing(desired[p], op.Owners) {
			if desired[p] == nil {
				desired[p] = []string{o}
				continue
			}
			desired[p] = append(append([]string{}, desired[p]...), o)
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
			// The owners this set KEEPS keep the spelling the file gave them
			// (R-38b's reason, applied to the half of set_owners that is not
			// changing anything: restyling a handle nobody asked to change
			// churns a diff on every repository in a fleet). Only owners the
			// line does not already have arrive in the op's spelling.
			newOwners := keepFileSpelling(lastRule.Owners, op.Owners)
			f.SetOwners(lastIntersecting, newOwners)
			pl.Changes = append(pl.Changes, Change{
				Action: "amend", Line: lastIntersecting + 1, Pattern: lastRule.PatternText,
				OldOwners: oldOwners, NewOwners: newOwners,
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
		// R-7 disclosure for a duplicate THIS run authors: the insert lands
		// after the last intersecting rule, so an earlier byte-equal pattern is
		// left permanently shadowed while still naming its old owners to human
		// readers. warnShadowedDuplicates above ran on the pre-op file and
		// cannot see it — without this, the run creating the dead line is the
		// one run that says nothing about it (pre-release finding).
		for _, r := range f.Rules() {
			if r.LineIndex != at && r.PatternText == op.Scope {
				pl.addWarning(fmt.Sprintf(
					"line %d: duplicate pattern %q is shadowed by line %d, which this run inserted — the earlier line is dead under last-match-wins (R-7); run `audit` to clean up duplicates",
					r.LineIndex+1, op.Scope, at+1))
			}
		}
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
				"refusing: remove_owner(%s, %s) did not converge — file structure defeats the writer", op.Scope, ownerArg(op.Owners))}
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
	// removal contract (EVERY owner the op names owns nothing in scope —
	// INV-1 for remove, which R-33 makes a claim about the whole list rather
	// than about one name) and accept the fallthrough set. INV-2 is untouched:
	// desired outside scope never changes here.
	final := resolve.All(f, tree)
	for p := range scope {
		want := minusAll(desired[p], op.Owners)
		got := final[p].Owners
		if resolve.OwnersEqual(got, want) {
			desired[p] = want
			continue
		}
		// Name the survivors, not the whole list: an operator told "@a, @b
		// would still own x" when only @b did would go looking in the wrong
		// half of their policy.
		if survivors := ownersPresent(got, op.Owners); len(survivors) > 0 {
			return &RefusalError{Msg: fmt.Sprintf(
				"refusing: %s would still own %q after remove_owner(%s, %s)", ownerNames(survivors), p, op.Scope, ownerArg(op.Owners))}
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
				op.Scope, ownerArg(op.Owners), fmtOwners(got), p, fmtOwners(want))}
		}
		desired[p] = got // inherit fallthrough: owners come from surviving rules
	}
	return nil
}

// removePass performs one round of removal edits; reports whether any edit
// was made.
func removePass(f *file.File, tree []string, op ops.Op, scope map[string]bool, onEmpty string, pl *Plan) (bool, error) {
	cur := resolve.All(f, tree)
	derive := deriveDomain(op, scope, tree)

	groups := map[int][]string{}
	for p := range scope {
		if r := cur[p]; r.Matched && containsAny(f.Lines[r.LineIndex].Rule.Owners, op.Owners) {
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
		// One minusAll, not one minus per owner: a line losing three owners is
		// one rewritten line, and the intermediate two-owner spellings never
		// reach the plan (R-33b). `removed` is what this LINE actually gives
		// up, which is what the messages below may truthfully name — a list
		// may name owners this particular rule never carried.
		newOwners := minusAll(r.Owners, op.Owners)
		removed := ownersPresent(r.Owners, op.Owners)
		if pattern.Contains(op.Scope, r.PatternText) {
			if len(newOwners) > 0 {
				old := f.LineText(l)
				oldOwners := r.OwnersCopy()
				f.SetOwners(l, newOwners)
				pl.Changes = append(pl.Changes, Change{
					Action: "amend", Line: l + 1, Pattern: r.PatternText,
					OldOwners: oldOwners, NewOwners: newOwners,
					OldLine: old, NewLine: f.LineText(l),
					Reason: fmt.Sprintf("pattern %q can only ever match paths inside scope; removed %s in place", r.PatternText, ownerNames(removed)),
				})
				continue
			}
			// Owner set would become empty: explicit policy required (R-6).
			switch onEmpty {
			case "":
				return false, &InvalidError{Msg: fmt.Sprintf(
					"removing %s empties the owner set of %q; an explicit --on-empty policy (error|inherit|unowned) is required — there is deliberately no default (R-6)",
					ownerNames(removed), r.PatternText)}
			case "error":
				return false, &RefusalError{Msg: fmt.Sprintf(
					"refusing: removing %s would leave %q with no owners (--on-empty=error, R-6)", ownerNames(removed), r.PatternText)}
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
			inter, exact, ok := intersectPattern(op.Scope, r, derive, tree)
			if !ok {
				return false, &RefusalError{Msg: fmt.Sprintf(
					"refusing: rule %q also governs paths outside scope %q and no sound narrowing pattern is derivable", r.PatternText, op.Scope)}
			}
			if !exact {
				pl.addWarning(inexactNarrowingWarning(inter, op.Scope, r.PatternText))
			}
			reason := fmt.Sprintf("rule %q also governs out-of-scope paths; inserted narrowing rule %q after it (R-2); out-of-scope resolution untouched", r.PatternText, inter)
			if len(newOwners) == 0 {
				switch onEmpty {
				case "":
					return false, &InvalidError{Msg: fmt.Sprintf(
						"removing %s empties the owner set for in-scope paths of %q; an explicit --on-empty policy is required (R-6)", ownerNames(removed), r.PatternText)}
				case "error":
					return false, &RefusalError{Msg: fmt.Sprintf(
						"refusing: removing %s would leave in-scope paths of %q with no owners (--on-empty=error)", ownerNames(removed), r.PatternText)}
				case "inherit":
					// R-39: deletion is not the only spelling of inheritance.
					// The rule cannot be deleted — out-of-scope paths depend on
					// it — but the narrowing rule this branch already inserts
					// can state what the in-scope paths would fall through to,
					// which resolves identically and touches nothing else.
					// Refusing here instead left the intent inexpressible in
					// exactly the fleet repos where the owner being revoked was
					// the sole owner of the rule.
					inherited, from, err := fallthroughOwners(f, l, op, groups[l], removed)
					if err != nil {
						return false, err
					}
					newOwners = inherited
					reason = fmt.Sprintf(
						"removing %s empties the in-scope paths of %q, which also governs out-of-scope paths and cannot be deleted; inserted narrowing rule %q restating the owners those paths fall through to (%s, from rule %q) per --on-empty=inherit (R-39)",
						ownerNames(removed), r.PatternText, inter, ownerNames(inherited), from)
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
				Reason:  reason,
			})

		}
	}
	return true, nil
}

// fallthroughOwners answers the question --on-empty=inherit asks when the rule
// it would delete cannot be deleted (R-39): what would these in-scope paths
// resolve to if this rule did not govern them?
//
// The walk goes UP the file from the rule at line l, and steps over any earlier
// matching rule the same removal would empty for these paths — under deletion-
// inheritance that rule would cascade away too (R-6), so stopping at it would
// restate an owner the operator asked to revoke.
//
// It refuses in the two states no single line can express: paths that would
// fall through to nothing (unmatched is not the same state as the explicitly
// zero-owned `[]` of S-9, and no line means the former), and paths that would
// fall through to DIFFERENT owners, which one narrowing rule cannot say.
func fallthroughOwners(f *file.File, l int, op ops.Op, paths, removed []string) ([]string, string, error) {
	rules := f.Rules()
	// groups[l] comes from a map range, so sort before letting any path reach a
	// message or decide which one is reported first — a fixed file must not
	// produce a different refusal run to run (the R-8 lesson).
	sorted := append([]string{}, paths...)
	sort.Strings(sorted)

	var want []string
	var from, wantPath string
	for _, p := range sorted {
		owners, src := inheritedFor(rules, l, p, op.Owners)
		if owners == nil {
			return nil, "", &RefusalError{Msg: fmt.Sprintf(
				"refusing: removing %s leaves %q with no owners, and nothing to inherit — the path would match no rule, which no line can express (an explicit zero-owner rule is a different state, S-9); set --on-empty=unowned to state that deliberately, or give the path an owner first",
				ownerNames(removed), p)}
		}
		if want == nil {
			want, from, wantPath = owners, src, p
			continue
		}
		if !resolve.OwnersEqual(owners, want) {
			return nil, "", &RefusalError{Msg: fmt.Sprintf(
				"refusing: --on-empty=inherit would give %q %s and %q %s — one narrowing rule cannot state both; split the removal into per-path scopes",
				wantPath, fmtOwners(want), p, fmtOwners(owners))}
		}
	}
	if want == nil {
		// Unreachable while every group carries at least one path, and stated
		// anyway: returning nil here would insert a zero-owner rule, which is a
		// deliberate un-owning nobody asked for (S-9).
		return nil, "", &RefusalError{Msg: fmt.Sprintf(
			"refusing: --on-empty=inherit found no in-scope path to inherit for (removing %s)", ownerNames(removed))}
	}
	return want, from, nil
}

// inheritedFor returns the owners path p would resolve to from the rules ABOVE
// line l, minus the owners being removed, plus the pattern they came from. A
// nil owner slice means no earlier rule matches p at all.
func inheritedFor(rules []*file.Rule, l int, p string, removing []string) ([]string, string) {
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		if r.LineIndex >= l || !r.Pattern.Match(p) {
			continue
		}
		if owners := minusAll(resolve.CanonicalOwners(r.Owners), removing); len(owners) > 0 {
			return owners, r.PatternText
		}
	}
	return nil, ""
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
// basename at any depth. It deliberately does NOT strip an explicit "**/"
// prefix, even though "**/G" and "G" select the same files: this helper also
// seeds the one-level (`dir/*`) candidate in intersectPattern, and widening it
// widens every derivation site at once. The one spelling-equivalence the suite
// demands (TestR2_NarrowingIsIndependentOfScopeSpelling) is handled where its
// boundary can be enforced, in globScopeIntersect.
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
	dir, ok := ruleDirPrefix(rule.PatternText)
	if !ok {
		return "", false
	}
	// The scope must be a single-segment basename glob: anchored or
	// multi-segment scopes cannot be concatenated onto a directory prefix
	// without the two path fragments overlapping.
	//
	// "**/G" spells the same language as the bare "G", and refusing it while
	// deriving for "G" made the operator's spelling, not the scope's match
	// set, decide the outcome — add_owner(**/build.gradle, …) exited 2 on the
	// very repo where add_owner(build.gradle, …) derived
	// /.github/**/build.gradle (TestR2_NarrowingIsIndependentOfScopeSpelling).
	// The trimmed spelling is admitted only against an ANCHORED rule prefix,
	// though: against an unanchored one the suite pins BOTH outcomes — the
	// bare glob derives (TestR2_FileGlobScopeAcrossUnanchoredDirRule,
	// *.gradle × app2/ → **/app2/**/*.gradle) and the "**/" spelling refuses
	// (the fleet's "refuse" shape, add_owner(**/*.tf, …) × infra/, whose
	// exit-2 is the needs-human lane every TestFleet_* contract is built on).
	// Trimming unconditionally flipped that repo from "refused" to "applied":
	// tree-exact here, but a silent widening of what the tool writes on its
	// own — expressibility grows via a reviewed R-2 shape, not as a side
	// effect. So the equivalence holds exactly as far as a test demands it
	// and not one shape further.
	seg := scope
	if s, spelled := strings.CutPrefix(scope, "**/"); spelled {
		if strings.HasPrefix(dir, "**/") {
			return "", false
		}
		seg = s
	}
	if _, ok := basenameGlob(seg); !ok {
		return "", false
	}
	cand := dir + "**/" + seg
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

// contains asks the one owner identity (R-38a): every list this package tests
// membership in is a list of owners, and byte equality here is what let
// `remove_owner(/x/, @ORG/TEAM)` see nothing to do on a file holding
// @org/team. Folding MATCHING never folds output — the spelling that goes
// back on the line comes from the file (R-38b) or, for a rename, from the op
// (R-38d).
func contains(list []string, s string) bool {
	for _, x := range list {
		if sameOwner(x, s) {
			return true
		}
	}
	return false
}

func containsStr(list []string, s string) bool { return contains(list, s) }

// sameOwner is the identity applied to a pair (R-38a). One function decides it
// for the whole repository: ops.FoldOwner.
func sameOwner(a, b string) bool { return ops.FoldOwner(a) == ops.FoldOwner(b) }

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
//
// Both comparisons are the identity's, not bytes' (R-38a/R-38d): the old name
// is matched however either side spelled it, and a differently cased copy of
// the NEW name is the same owner, so it collapses into the one written slot
// rather than surviving beside it. The text written is `new` exactly as the op
// spelled it — rename is the one verb whose purpose is to change the text.
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
		case sameOwner(x, old):
			if done {
				continue // old listed twice: the second is a duplicate either way
			}
			done = true
			out = append(out, new)
		case sameOwner(x, new) && done:
			// Already emitted at the renamed owner's position; this later copy
			// would be a duplicate.
		case sameOwner(x, new) && hasOld:
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

// The R-33 owner-set helpers. An op names a SET of owners, and every site that
// used to read one name has to fold the whole set in ONE edit: looping the
// single-owner path per owner reproduces the very defect the list exists to
// fix, a second hunk whose old_line is the first hunk's new_line, describing a
// file state that is on disk at no point in the run (R-33b).

// keepFileSpelling renders `want` in want's order, substituting the spelling
// `have` already uses for any owner the two share (R-38a/R-38b). It is how a
// verb that rewrites a whole owner list avoids restyling the owners that were
// not the point of the change.
func keepFileSpelling(have, want []string) []string {
	out := make([]string, 0, len(want))
	for _, o := range want {
		spelled := o
		for _, h := range have {
			if sameOwner(h, o) {
				spelled = h
				break
			}
		}
		out = append(out, spelled)
	}
	return out
}

// ownersMissing returns the members of want, in want's order, that are absent
// from have. Order is R-33e: the list fixes append order in the written line.
func ownersMissing(have, want []string) []string {
	var out []string
	for _, o := range want {
		if !contains(have, o) && !contains(out, o) {
			out = append(out, o)
		}
	}
	return out
}

// ownersPresent returns the members of want, in want's order, that ARE in have
// — the owners a removal actually takes off this line, and so the ones its
// message may truthfully name.
func ownersPresent(have, want []string) []string {
	var out []string
	for _, o := range want {
		if contains(have, o) && !contains(out, o) {
			out = append(out, o)
		}
	}
	return out
}

// minusAll is minus over a set: one pass, so a rule losing three owners is one
// rewritten line rather than three (R-33b).
func minusAll(list []string, drop []string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		if !contains(drop, x) {
			out = append(out, x)
		}
	}
	return out
}

// containsAny reports whether list holds any member of want.
func containsAny(list, want []string) bool {
	for _, o := range want {
		if contains(list, o) {
			return true
		}
	}
	return false
}

// ownerArg renders an op's owner set the way the op string spells it, so a
// message quoting the op back — `remove_owner(/docs/, @a)` — stays byte
// identical for the single-owner form and names every owner for a list,
// instead of printing the empty string Op.Owner now holds for these kinds.
func ownerArg(owners []string) string {
	if len(owners) == 1 {
		return owners[0]
	}
	return "[" + strings.Join(owners, ", ") + "]"
}

// ownerNames renders owners as prose ("@a, @b"), for messages whose sentence
// is about the owners themselves rather than about the op's syntax.
func ownerNames(owners []string) string { return strings.Join(owners, ", ") }

// minus drops every spelling of one owner (R-38c): a surviving spelling would
// leave the owner a removal named still owning the path.
func minus(list []string, s string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		if !sameOwner(x, s) {
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
