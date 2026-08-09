// Package lint is the whole-file repair pass behind `audit --lint`.
//
// `audit` reports; lint FIXES, over the entire CODEOWNERS file, in three
// stages that run in this order and only this order:
//
//  1. REPAIR the whitespace inside an @handle. `/x/ @ org/team` is not a rule
//     with a slightly odd owner — GitHub skips the whole line, so the team
//     owns nothing and nobody is told. The handle is rejoined FIRST, before
//     any existence check, because `@` and `org/team` are not two owners that
//     do not exist; they are one owner nobody has looked up yet.
//  2. REMOVE owners that definitively do not exist (A-1). A deleted or
//     renamed team is a review request that silently goes nowhere.
//  3. REMOVE rules whose pattern matches zero tracked files — OPT-IN only
//     (Options.RemoveStalePaths). R-11 keeps this report-only in `audit`
//     because a dead pattern may be deliberate and forward-looking; deleting
//     it destroys intent, so it takes an explicit human instruction.
//
// Two contracts are inherited wholesale from Engine B and are what keep this
// package from being the tool's most dangerous code path:
//
//	R-12 — it fails closed, and it fails closed for the WHOLE RUN. If a single
//	owner lookup is inconclusive (rate limit, 401/403, network, an org this
//	token cannot enumerate), Build returns *InconclusiveError and no repair,
//	no removal and no deletion is written. A lint pass that cannot see the
//	whole file does not get to edit part of it — an expired token quietly
//	stripping owners is the worst thing this tool could do.
//
//	R-13 — email owners are unverifiable, never dead. They are reported in
//	Result.Unverifiable and left exactly where they are.
//
// Like plan.Build, nothing here writes: Build returns a *plan.Plan and the CLI
// hands it to apply.Apply, which remains the system's single writer (R-0).
// And like plan.Build, the result is PROVEN rather than trusted — see the gate
// in Build.
package lint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/ghapi"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
	"github.com/jordonpeterson/codeowners-tool/internal/resolve"
)

// Verifier answers the only question lint asks the network: does this owner
// exist? *ghapi.Client satisfies it, and its fail-closed contract carries
// over unchanged — an error means "could not be decided", never "no".
//
// Deliberately narrower than the audit client: lint removes owners that do
// not EXIST (A-1), never owners that merely lack write access (A-3) or org
// membership (A-2). Those are findings for a human, because the fix may well
// be to grant the access rather than to drop the owner.
type Verifier interface {
	// ProbeOrg proves the token can enumerate the org. Until it succeeds, an
	// org-scoped 404 does not mean "gone" (R-12).
	ProbeOrg(org string) error
	// TeamExists reports whether @org/slug exists. Call ProbeOrg first.
	TeamExists(org, slug string) (bool, error)
	// UserExists reports whether @login exists.
	UserExists(login string) (bool, error)
}

// Options tunes a lint run.
type Options struct {
	// RemoveStalePaths opts in to stage 3 (R-11). Off by default.
	RemoveStalePaths bool
	// OnEmpty is R-6's policy for the case where removing a dead owner would
	// leave a rule with no owners: "" (invalid — an explicit choice is
	// required), "error", "inherit", or "unowned".
	OnEmpty string
	// MaxSize / WarnSize are S-4's cliff and R-9's threshold; zero means the
	// same defaults plan.Options uses.
	MaxSize  int
	WarnSize int
}

// Action kinds.
const (
	// ActionRepairOwner: whitespace removed from inside an @handle (stage 1).
	ActionRepairOwner = "repair-owner-spacing"
	// ActionRemoveOwner: an owner that does not exist, dropped (stage 2).
	ActionRemoveOwner = "remove-dead-owner"
	// ActionRemoveStale: a rule matching zero tracked files, deleted (stage 3).
	ActionRemoveStale = "remove-stale-rule"
	// ActionUnrepairable: an invalid line lint could not repair without
	// guessing. Reported, never rewritten, never deleted.
	ActionUnrepairable = "unrepairable-line"
)

// Action is one thing lint did — or, for ActionUnrepairable, one thing it
// deliberately declined to guess at.
type Action struct {
	Kind    string `json:"kind"`
	Line    int    `json:"line"` // 1-based, at the time the action was taken
	Owner   string `json:"owner,omitempty"`
	Pattern string `json:"pattern,omitempty"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
	Reason  string `json:"reason"`
}

// Result is one lint run.
type Result struct {
	// Plan carries the after-content, the line changes, the ownership rows and
	// the pinned before-hash. It is the same artifact `plan` emits and the same
	// one apply.Apply consumes, so lint gets the hash pin, the size cap, the
	// pre-write syntax validation and the atomic rename for free.
	Plan *plan.Plan `json:"plan"`
	// Actions is what lint did, in file order.
	Actions []Action `json:"actions,omitempty"`
	// Unverifiable lists email owners left untouched (R-13).
	Unverifiable []string `json:"unverifiable,omitempty"`
}

// InconclusiveError is R-12 applied to the whole run: at least one owner could
// not be verified, so NOTHING is written. Exit 5.
type InconclusiveError struct{ Reasons []string }

func (e *InconclusiveError) Error() string {
	return "inconclusive: " + strings.Join(e.Reasons, "; ") +
		" — no owner was removed and nothing was written (R-12); re-run when the lookup can be answered"
}

// RepairHandle rejoins whitespace that splits a single @handle into several
// tokens, returning the repaired token list and whether anything changed.
//
// Exported for the same reason it is written as a pure function over tokens:
// it is a guess, and a guess is only acceptable when its boundaries can be
// stated exactly and tested directly. Tokens are merged only when
//
//   - the run starts with a token beginning "@" (an email owner is never
//     repaired: `a@b.com` + `/x` concatenates into a syntactically VALID email
//     owner, so a merge rule that allowed it would invent an address nobody
//     wrote), and
//   - every join sits inside a handle — the left side ends with "@" or "/", or
//     the right side STARTS with "/", so the only bytes removed are whitespace
//     that had a handle's own punctuation on one side of it. A right side
//     starting with "@" does not qualify: that is where the next owner begins,
//     and
//   - the concatenation is a valid @handle.
//
// `/x/ @a b` therefore never merges into `/x/ @ab` — the join touches neither
// an "@" nor a "/" — and `@a @b` never merges into `@a@b`, which the join rule
// permits (the left side ends with... "a", so it does not, in fact) and which
// the validity rule refuses anyway, since "@a@b" is not a handle. Both guards
// are kept: either alone leaves a case the other catches.
//
// The LONGEST valid run wins, which is what carries `@ org / team` — where the
// intermediate `@org` is itself a perfectly valid handle — all the way to
// `@org/team` instead of stopping at the first thing that happens to parse.
func RepairHandle(tokens []string) (repaired []string, changed bool) {
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); {
		j := longestHandleJoin(tokens, i)
		if j > i {
			out = append(out, strings.Join(tokens[i:j+1], ""))
			changed = true
			i = j + 1
			continue
		}
		out = append(out, tokens[i])
		i++
	}
	if !changed {
		// Hand back the caller's own slice untouched: "nothing to repair" must
		// not look like an edit to anything comparing identity or nil-ness.
		return tokens, false
	}
	return out, true
}

// longestHandleJoin returns the index of the last token in the longest run
// starting at i that concatenates into a valid @handle across legal join
// points — or i itself when no merge is available.
func longestHandleJoin(tokens []string, i int) int {
	if !strings.HasPrefix(tokens[i], "@") {
		return i
	}
	best, acc := i, tokens[i]
	for j := i + 1; j < len(tokens); j++ {
		if !joinable(acc, tokens[j]) {
			break
		}
		acc += tokens[j]
		// An intermediate step may be invalid on the way to a valid whole:
		// `@` + `org` + `/` + `team` passes through `@org/`, which is not a
		// handle. Keep extending and remember the last spelling that was.
		if strings.HasPrefix(acc, "@") && file.ValidOwnerToken(acc) {
			best = j
		}
	}
	return best
}

// joinable reports whether the whitespace between left and right sits INSIDE a
// handle — the whole safety property of the repair. Removing whitespace
// anywhere else would fuse two tokens the author wrote apart.
func joinable(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return strings.HasSuffix(left, "@") || strings.HasSuffix(left, "/") || strings.HasPrefix(right, "/")
}

// RepairLine applies RepairHandle to one raw line, preserving the pattern
// token, the original spacing around it and any inline comment. ok is false
// when nothing could be repaired, or when the repaired text does not re-parse
// as a rule with a byte-identical pattern — a repair that changes which files
// a line governs is not a repair.
func RepairLine(raw string) (fixed string, ok bool) {
	// Only a line GitHub is currently skipping over its OWNERS is a candidate.
	// A line with a bad pattern is not repairable here (no rejoining of owners
	// makes the pattern compile), and a line that already parses needs nothing
	// — asking the token merger about it would be asking it to invent work.
	orig := file.Parse([]byte(raw))
	if len(orig.Lines) != 1 || orig.Lines[0].Kind != file.LineInvalid {
		return "", false
	}
	if orig.Lines[0].Err == nil || orig.Lines[0].Err.Kind != "Invalid owner" {
		return "", false
	}

	head, region, split := file.SplitOwnerRegion(raw)
	if !split {
		return "", false
	}
	tokens, comment := splitOwnerTokens(region)
	if len(tokens) == 0 {
		return "", false
	}
	merged, changed := RepairHandle(tokens)
	if !changed {
		return "", false
	}

	out := head + strings.Join(merged, " ")
	if comment != "" {
		if !strings.HasPrefix(comment, " ") && !strings.HasPrefix(comment, "\t") {
			out += " "
		}
		out += comment
	}

	// The structural gate for stage 1, and the reason the repair is allowed to
	// be a guess at all: the result must be ONE line, it must parse as a rule,
	// its pattern must be the byte-identical pattern that went in (so the set
	// of files this line governs is untouched), and it must carry exactly the
	// tokens the merge produced (so no owner was dropped or invented).
	after := file.Parse([]byte(out))
	if len(after.Lines) != 1 || after.Lines[0].Kind != file.LineRule {
		return "", false
	}
	r := after.Lines[0].Rule
	if !strings.HasPrefix(out, head) || len(r.Owners) != len(merged) {
		return "", false
	}
	for i, o := range r.Owners {
		if o != merged[i] {
			return "", false
		}
	}
	return out, true
}

// splitOwnerTokens splits a line's owner region into whitespace-separated
// tokens plus any trailing inline comment, kept VERBATIM (leading whitespace
// included) so a repair does not reflow a file's column alignment. It mirrors
// parseLine's own loop, which is what makes "comment" mean the same thing in
// both places.
func splitOwnerTokens(region string) (tokens []string, comment string) {
	rest := region
	for rest != "" {
		if rest[0] == '#' {
			return tokens, rest
		}
		var tok string
		if end := strings.IndexAny(rest, " \t"); end < 0 {
			tok, rest = rest, ""
		} else {
			tok, rest = rest[:end], rest[end:]
		}
		tokens = append(tokens, tok)
		ws := 0
		for ws < len(rest) && (rest[ws] == ' ' || rest[ws] == '\t') {
			ws++
		}
		if ws < len(rest) && rest[ws] == '#' {
			return tokens, rest
		}
		rest = rest[ws:]
	}
	return tokens, ""
}

// Build computes the lint plan. It never writes anything.
//
// The gate is plan.Build's, restated for the edits lint makes: the after-bytes
// are RE-PARSED and re-resolved over the whole tracked tree, and every path's
// owner set must equal an independently computed desired state — the ownership
// that the REPAIRED file would have, minus the owners proven not to exist.
// Stage 3 contributes nothing to that state by construction (a rule matching
// zero tracked files can never win a tracked path), which is exactly why
// deleting one is safe and why the gate is what proves it rather than the
// author's say-so.
//
// Errors: *InconclusiveError (R-12, nothing written), *plan.NoOpError (already
// clean — the Result is still populated), *plan.InvalidError (a removal empties
// an owner set with no --on-empty policy, R-6), *plan.RefusalError (the gate
// rejected the synthesized edits, or --on-empty=error, or S-4's size cap).
func Build(content []byte, tree []string, v Verifier, opts Options) (*Result, error) {
	if opts.MaxSize == 0 {
		opts.MaxSize = 3_000_000
	}
	if opts.WarnSize == 0 {
		opts.WarnSize = 2_500_000
	}

	f := file.Parse(content)
	beforeOwners := make(map[string][]string, len(tree))
	for p, r := range resolve.All(f, tree) {
		beforeOwners[p] = r.Owners // nil if unmatched
	}

	res := &Result{Plan: &plan.Plan{HashBefore: hashHex(content), SizeBefore: len(content)}}

	// ---- Stage 1: repair owner spacing. -------------------------------------
	// Before any lookup: `@ org/team` is one owner nobody has asked about yet,
	// not two owners that do not exist.
	for i, ln := range f.Lines {
		if ln.Kind != file.LineInvalid {
			continue
		}
		old := ln.Raw
		fixed, ok := RepairLine(old)
		if !ok {
			// Reported, never touched. A line the tool does not understand is
			// a rule somebody wrote and believes is in force; guessing at it or
			// deleting it are both worse than saying so.
			res.Actions = append(res.Actions, Action{
				Kind: ActionUnrepairable, Line: i + 1, Before: old,
				Reason: fmt.Sprintf("%s: %s — left exactly as written; GitHub skips this line, so nothing here owns anything",
					ln.Err.Kind, ln.Err.Message),
			})
			continue
		}
		f.ReplaceLine(i, fixed)
		r := f.Lines[i].Rule
		res.Actions = append(res.Actions, Action{
			Kind: ActionRepairOwner, Line: i + 1, Pattern: r.PatternText,
			Before: old, After: fixed,
			Reason: fmt.Sprintf("whitespace inside an @handle made GitHub skip the whole line; rejoined to %s — the pattern is unchanged, so this line now governs the files it always claimed to",
				strings.Join(r.OwnersCopy(), " ")),
		})
		res.Plan.Changes = append(res.Plan.Changes, plan.Change{
			Action: "amend", Line: i + 1, Pattern: r.PatternText,
			NewOwners: r.OwnersCopy(), OldLine: old, NewLine: fixed,
			Reason: "repaired owner spacing (stage 1): the line was invalid and therefore skipped; it is now in force",
		})
	}

	// The independently computed baseline for the gate below: what GitHub
	// would resolve if the typo'd lines had been written correctly. Stage 2's
	// desired state is a pure set subtraction on top of it.
	repaired := resolve.All(f, tree)

	// ---- Stage 2: which owners definitively do not exist? --------------------
	owners := distinctOwners(f)
	known := make(map[string]bool, len(owners))
	for _, o := range owners {
		known[o] = true
	}
	dead := map[string]string{} // owner -> why it is dead
	var reasons []string
	for _, o := range owners {
		// R-13: an email owner resolves via a verified address the API cannot
		// see. Unverifiable is not inconclusive — it is permanent, so treating
		// it as R-12 would wedge lint forever on any file that has one.
		if file.IsEmailOwner(o) {
			res.Unverifiable = append(res.Unverifiable, o)
			continue
		}
		gone, reason, err := ownerIsGone(v, o)
		if err != nil {
			reasons = appendUnique(reasons, fmt.Sprintf("%s: %s", o, errReason(err)))
			continue
		}
		if gone {
			dead[o] = reason
		}
	}
	// R-12, applied to the whole run rather than to one owner. Partial
	// knowledge does not earn a partial edit: the offline stages are held back
	// with the removals, so a rate-limited run leaves a file that is exactly
	// what it was, and re-running once the lookup works produces the complete
	// fix in one reviewable diff.
	if len(reasons) > 0 {
		return nil, &InconclusiveError{Reasons: reasons}
	}

	desired := make(map[string][]string, len(tree))
	for p, r := range repaired {
		desired[p] = withoutDead(r.Owners, dead)
	}

	// ---- Stages 2 and 3: synthesize the edits. ------------------------------
	// Descending line order so a delete never invalidates an index still to be
	// visited.
	inheritDeletes := 0
	for i := len(f.Lines) - 1; i >= 0; i-- {
		ln := f.Lines[i]
		if ln.Kind != file.LineRule {
			continue
		}
		r := ln.Rule

		if opts.RemoveStalePaths && !matchesTree(r, tree) {
			res.Actions = append(res.Actions, Action{
				Kind: ActionRemoveStale, Line: i + 1, Pattern: r.PatternText, Before: f.LineText(i),
				Reason: fmt.Sprintf("pattern %q matches zero tracked files, so it can never win a path and deleting it changes no ownership (--remove-stale-paths; R-11 keeps this off by default because a dead pattern may be deliberate)", r.PatternText),
			})
			res.Plan.Changes = append(res.Plan.Changes, plan.Change{
				Action: "delete", Line: i + 1, Pattern: r.PatternText,
				OldOwners: r.OwnersCopy(), OldLine: f.LineText(i),
				Reason: "stale rule deleted (stage 3): its pattern matches zero tracked files",
			})
			f.DeleteLine(i)
			continue
		}

		var removed, keep []string
		for _, o := range r.Owners {
			if _, isDead := dead[o]; isDead {
				removed = append(removed, o)
			} else {
				keep = append(keep, o)
			}
		}
		if len(removed) == 0 {
			continue
		}

		old, oldOwners := f.LineText(i), r.OwnersCopy()
		deletedLine := false
		if len(keep) == 0 {
			// R-6: emptying an owner set is a decision, and it has no default.
			switch opts.OnEmpty {
			case "":
				return nil, &plan.InvalidError{Msg: fmt.Sprintf(
					"removing %s empties the owner set of %q; an explicit --on-empty policy (error|inherit|unowned) is required — there is deliberately no default (R-6)",
					strings.Join(removed, ", "), r.PatternText)}
			case "error":
				return nil, &plan.RefusalError{Msg: fmt.Sprintf(
					"refusing: removing %s would leave %q with no owners (--on-empty=error, R-6)",
					strings.Join(removed, ", "), r.PatternText)}
			case "unowned":
				f.SetOwners(i, nil)
			case "inherit":
				f.DeleteLine(i)
				deletedLine = true
				inheritDeletes++
			default:
				return nil, &plan.InvalidError{Msg: fmt.Sprintf("unknown --on-empty policy %q", opts.OnEmpty)}
			}
		} else {
			f.SetOwners(i, keep)
		}

		for _, o := range removed {
			res.Actions = append(res.Actions, Action{
				Kind: ActionRemoveOwner, Line: i + 1, Owner: o, Pattern: r.PatternText,
				Before: old, Reason: dead[o],
			})
		}
		if deletedLine {
			res.Plan.Changes = append(res.Plan.Changes, plan.Change{
				Action: "delete", Line: i + 1, Pattern: r.PatternText,
				OldOwners: oldOwners, OldLine: old,
				Reason: "owner set emptied by stage 2; rule deleted per --on-empty=inherit so the preceding broader rule takes over (R-6) — the resulting reassignment appears in the ownership rows",
			})
			continue
		}
		res.Plan.Changes = append(res.Plan.Changes, plan.Change{
			Action: "amend", Line: i + 1, Pattern: r.PatternText,
			OldOwners: oldOwners, NewOwners: f.Lines[i].Rule.OwnersCopy(),
			OldLine: old, NewLine: f.LineText(i),
			Reason: fmt.Sprintf("removed %s: no such user or team on GitHub", strings.Join(removed, ", ")),
		})
	}

	// ---- The gate. ----------------------------------------------------------
	// Serialize, RE-PARSE, and re-resolve over the real tree. Gating on the
	// re-parsed bytes rather than the in-memory model is what proves the edits
	// survive serialization, exactly as plan.Build does.
	afterBytes := f.Bytes()
	afterFile := file.Parse(afterBytes)
	after := resolve.All(afterFile, tree)
	var violations []string
	for _, p := range tree {
		want, got := desired[p], after[p].Owners
		if resolve.OwnersEqual(got, want) {
			continue
		}
		// Under --on-empty=inherit a deleted rule legitimately resurrects the
		// owners of whatever line sat behind it — an outcome no pure owner-set
		// transform can predict. It is accepted only when a rule was in fact
		// deleted for emptiness, and only when no dead owner came back with it;
		// otherwise the divergence is a synthesis bug and accepting it here
		// would launder that bug straight past the gate.
		if inheritDeletes > 0 && !anyDead(got, dead) {
			desired[p] = got
			continue
		}
		violations = append(violations, fmt.Sprintf("%s — want %s, would get %s", p, fmtOwners(want), fmtOwners(got)))
	}
	// Two claims the ownership comparison above cannot make, because both are
	// about the FILE rather than about the tree: an owner that does not exist
	// must appear nowhere (a rule matching zero tracked files still hands a
	// reviewer a name that goes nowhere), and no owner may be invented.
	for _, r := range afterFile.Rules() {
		for _, o := range r.Owners {
			if _, isDead := dead[o]; isDead {
				violations = append(violations, fmt.Sprintf("line %d still lists %s, which does not exist", r.LineIndex+1, o))
			}
			if !known[o] && !file.IsEmailOwner(o) {
				violations = append(violations, fmt.Sprintf("line %d lists %s, which was not an owner before the lint run", r.LineIndex+1, o))
			}
		}
	}
	if violations != nil {
		return nil, &plan.RefusalError{Msg: "refusing: the lint edits do not satisfy the invariants", Details: violations}
	}

	res.Plan.AfterContent = string(afterBytes)
	res.Plan.SizeAfter = len(afterBytes)
	sort.SliceStable(res.Actions, func(i, j int) bool { return res.Actions[i].Line < res.Actions[j].Line })

	if bytes.Equal(afterBytes, content) {
		// A populated Result, not nil: the caller still has to report the lines
		// it could not repair, and "clean" is the modal outcome of a scheduled
		// run.
		return res, &plan.NoOpError{Msg: "nothing to lint: no repairable owner spacing, and every owner exists"}
	}

	if res.Plan.SizeAfter > opts.MaxSize {
		return nil, &plan.RefusalError{Msg: fmt.Sprintf(
			"refusing: result would be %d bytes, over the %d-byte limit — GitHub silently ignores CODEOWNERS files over 3 MB (S-4)",
			res.Plan.SizeAfter, opts.MaxSize)}
	}
	if res.Plan.SizeAfter >= opts.WarnSize {
		res.Plan.Warnings = append(res.Plan.Warnings, fmt.Sprintf(
			"file size %d bytes is at or above the warning threshold %d (R-9); GitHub stops loading at 3 MB", res.Plan.SizeAfter, opts.WarnSize))
	}

	var changed []string
	for _, p := range tree {
		if !resolve.OwnersEqual(beforeOwners[p], after[p].Owners) {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	for _, p := range changed {
		res.Plan.Rows = append(res.Plan.Rows, plan.Row{Path: p, Before: beforeOwners[p], After: after[p].Owners})
	}

	var diff strings.Builder
	for _, c := range res.Plan.Changes {
		switch c.Action {
		case "amend":
			fmt.Fprintf(&diff, "@ line %d\n-%s\n+%s\n", c.Line, c.OldLine, c.NewLine)
		case "delete":
			fmt.Fprintf(&diff, "@ line %d\n-%s\n", c.Line, c.OldLine)
		}
	}
	res.Plan.Diff = diff.String()
	return res, nil
}

// ownerIsGone answers stage 2's question for one owner: does it definitively
// not exist? An error means the question could not be answered (R-12).
func ownerIsGone(v Verifier, owner string) (gone bool, reason string, err error) {
	if org, slug, isTeam := splitTeam(owner); isTeam {
		// A team 404 is only meaningful once the token has proven it can
		// enumerate the org — otherwise "invisible to these scopes" and
		// "deleted" are the same response.
		if err := v.ProbeOrg(org); err != nil {
			return false, "", err
		}
		exists, err := v.TeamExists(org, slug)
		if err != nil {
			return false, "", err
		}
		if exists {
			return false, "", nil
		}
		return true, fmt.Sprintf("team %s does not exist (deleted or renamed); review requests to it silently do nothing", owner), nil
	}
	exists, err := v.UserExists(strings.TrimPrefix(owner, "@"))
	if err != nil {
		return false, "", err
	}
	if exists {
		return false, "", nil
	}
	return true, fmt.Sprintf("user %s does not exist (deleted or renamed); review requests to it silently do nothing", owner), nil
}

// distinctOwners lists every owner named by a valid rule, in file order.
func distinctOwners(f *file.File) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range f.Rules() {
		for _, o := range r.Owners {
			if !seen[o] {
				seen[o] = true
				out = append(out, o)
			}
		}
	}
	return out
}

// matchesTree reports whether a rule governs at least one tracked file.
func matchesTree(r *file.Rule, tree []string) bool {
	for _, p := range tree {
		if r.Pattern.Match(p) {
			return true
		}
	}
	return false
}

// withoutDead is the pure owner-set transform stage 2 promises. nil
// (unmatched) stays nil; an explicitly empty set stays empty (S-9).
func withoutDead(owners []string, dead map[string]string) []string {
	if owners == nil {
		return nil
	}
	out := make([]string, 0, len(owners))
	for _, o := range owners {
		if _, isDead := dead[o]; !isDead {
			out = append(out, o)
		}
	}
	return out
}

func anyDead(owners []string, dead map[string]string) bool {
	for _, o := range owners {
		if _, isDead := dead[o]; isDead {
			return true
		}
	}
	return false
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func splitTeam(owner string) (org, slug string, ok bool) {
	if !strings.HasPrefix(owner, "@") || !strings.Contains(owner, "/") {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(owner, "@"), "/", 2)
	return parts[0], parts[1], true
}

func errReason(err error) string {
	var inc *ghapi.Inconclusive
	if errors.As(err, &inc) {
		return inc.Reason
	}
	return err.Error()
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
