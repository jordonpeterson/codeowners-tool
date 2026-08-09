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
	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
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
	// ViewerIsOrgAdmin reports whether the token is an OWNER of org. Only an
	// owner sees secret teams, so only an owner's team-404 means "deleted"
	// rather than "invisible to me" — and lint deletes on that answer.
	ViewerIsOrgAdmin(org string) (bool, error)
	// UserExists reports whether @login exists.
	UserExists(login string) (bool, error)
}

// Options tunes a lint run.
type Options struct {
	// RemoveStalePaths opts in to stage 3 (R-11). Off by default.
	RemoveStalePaths bool
	// WorkTree is what is on disk right now — tracked files plus untracked,
	// non-ignored ones. Stage 3 requires a pattern to match nothing in BOTH
	// this and the committed tree before it will delete the rule.
	//
	// Without it, staleness is judged against the tree at --branch while the
	// edit lands on the working-tree file, so a directory that exists on disk
	// but has not been committed yet reads as "matches zero tracked files" and
	// its owners are deleted out from under it, at exit 0 (found by adversarial
	// review). Required whenever RemoveStalePaths is set.
	WorkTree []string
	// OnEmpty is R-6's policy for the case where removing a dead owner would
	// leave a rule with no owners: "error", "inherit", or "unowned". "" is the
	// ordinary default and is perfectly valid — it becomes a *plan.InvalidError
	// only if a removal actually empties a set, which is R-6's point: the
	// choice is demanded where it matters, not up front.
	//
	// "inherit" DELETES the rule line so the preceding broader rule takes over.
	// On a rule with nothing broader behind it, that leaves the path unowned —
	// and on a single-rule file it empties the file. The reassignment is always
	// disclosed in Plan.Rows (R-14), which is what makes that reviewable rather
	// than silent.
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
	// ActionKeptCaseMismatch: a rule that matches nothing ONLY because of
	// case, spared from stage 3 and handed to a human instead.
	ActionKeptCaseMismatch = "kept-case-mismatch"
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

// NeedsHuman is how many lines lint deliberately left for a person: ones it
// refused to guess at, and ones it spared because deleting them would have
// destroyed the evidence of a typo.
//
// It is the difference between "this file is clean" and "this file has nothing
// left that a machine should touch", and callers must not conflate the two: the
// second still needs somebody, and "lint clean" over it is the one sentence
// that would send nobody.
func (r *Result) NeedsHuman() int {
	n := 0
	for _, a := range r.Actions {
		if a.Kind == ActionUnrepairable || a.Kind == ActionKeptCaseMismatch {
			n++
		}
	}
	return n
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
//   - the run's FIRST token begins with "@" and is NOT already a valid owner —
//     it is VISIBLY broken, a bare "@" or a dangling "@org/". This is the load-
//     bearing condition, and it is why `@org /team` is deliberately NOT
//     repaired. On a CODEOWNERS line everything after the pattern is an owner,
//     so `@org /team` — one handle a space got into — and `@alice /docs` — two
//     rules somebody put on one line — are the same three bytes in the same
//     three positions. Nothing distinguishes them, and guessing picks up
//     `/docs`'s owner and hands them all of `/src`. A token that already parses
//     as an owner is therefore never merged into anything; the line is reported
//     unrepairable instead. An email owner is excluded by the same clause, and
//     needs to be: `a@b.com` + `/x` concatenates into a syntactically VALID
//     address nobody wrote.
//   - every join sits inside a handle — the left side ends with "@" or "/", or
//     the right side STARTS with "/", so the only bytes removed are whitespace
//     that had a handle's own punctuation on one side of it. A right side
//     starting with "@" does not qualify: that is where the next owner begins.
//   - the concatenation is a valid @handle.
//
// So `/x/ @a b` never merges into `/x/ @ab`, `@a @b` never into `@a@b`, and
// `/x @a /b @a /b` never into `/x @a/b @a/b` — which the first condition
// refuses outright, and which the other two, on their own, did not.
//
// The LONGEST valid run wins, which is what carries `@ org / team` — where the
// intermediate `@org` happens to parse — all the way to `@org/team` instead of
// stopping at the first thing that does. That is safe only because the run
// STARTED broken: `@` is not something anyone wrote as an owner.
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

// maxHandleTokens caps a merge run. The worst legitimate shattering of a
// handle is four tokens — "@", "org", "/", "team" — so nothing beyond that is
// a handle being reassembled.
//
// The cap is also what keeps this linear. Without it a line of the shape
// `/p @ /aaa /aaa /aaa …` drives the scan to the end of the token list, doing
// an O(n) concatenation and an O(n) regex match at every step: 32k tokens took
// 2.8s and 128k took 30s, all of it spent to return "cannot repair" (found by
// adversarial review). One hostile or merely garbage file would stall a fleet
// run.
const maxHandleTokens = 4

// longestHandleJoin returns the index of the last token in the longest run
// starting at i that concatenates into a valid @handle across legal join
// points — or i itself when no merge is available.
func longestHandleJoin(tokens []string, i int) int {
	// The run must start VISIBLY BROKEN. A token that already parses as an
	// owner is never merged into anything, because `@alice /docs` (two rules on
	// one line) and `@org /team` (one handle with a space in it) are
	// byte-identical in shape, and guessing wrong hands `/docs`'s owner every
	// file under `/src`. Reported unrepairable is the only honest answer to an
	// ambiguity, and this clause is what makes that the outcome. It also
	// excludes email owners for free: `a@b.com` is valid, so it never starts a
	// run, and `a@b.com` + `/x` cannot fuse into an address nobody wrote.
	if !strings.HasPrefix(tokens[i], "@") || file.ValidOwnerToken(tokens[i]) {
		return i
	}
	best, acc := i, tokens[i]
	for j := i + 1; j < len(tokens) && j-i < maxHandleTokens; j++ {
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
	// A merge that produces an owner the line ALREADY names is not a repair of
	// that line, it is a coincidence: `/x @ org/a @org/a` reassembles into
	// `@org/a @org/a`, a rule listing one team twice. Refuse rather than emit
	// it — the line goes to the operator as unrepairable, which is what an
	// ambiguous reassembly deserves.
	if introducesDuplicate(tokens, merged) {
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
	// be a guess at all. Build's tree-level gate CANNOT check stage 1 — it
	// compares against a desired state derived from the already-repaired file,
	// so for this stage it would be comparing the repair to itself. These four
	// checks are therefore the whole proof, and each is independent of the
	// merge rule that produced the candidate:
	//
	//  1. BYTE CONSERVATION over the owner region. The repair may remove
	//     whitespace and nothing else. This is what "no owner was invented"
	//     actually means here: a merge that dropped, added or altered a single
	//     non-whitespace byte fails, whatever RepairHandle believed it was
	//     doing.
	//  2. The result is ONE line — a repair may not introduce a newline.
	//  3. It parses as a RULE, so the line GitHub was skipping is now in force.
	//  4. Its pattern is byte-identical to the pattern that went in, compared
	//     against the ORIGINAL line. Comparing against `out`'s own prefix
	//     instead would only ask whether concatenation works; what matters is
	//     that re-parsing did not shift the pattern/owner boundary, which is
	//     the one way a repair could change which files the line governs.
	if strings.Join(merged, "") != strings.Join(tokens, "") {
		return "", false
	}
	after := file.Parse([]byte(out))
	if len(after.Lines) != 1 || after.Lines[0].Kind != file.LineRule {
		return "", false
	}
	r := after.Lines[0].Rule
	if r.PatternText != file.PatternToken(raw) || len(r.Owners) != len(merged) {
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
		// Trailing whitespace is kept for the same reason the inline comment
		// is: INV-5 makes byte preservation the file model's promise, and a
		// repair that also silently reflows the end of the line makes the diff
		// say more than the repair did.
		if ws == len(rest) {
			return tokens, rest
		}
		rest = rest[ws:]
	}
	return tokens, ""
}

// introducesDuplicate reports whether merging created a repeated owner token
// that the original token list did not already have.
func introducesDuplicate(before, after []string) bool {
	count := func(list []string) map[string]int {
		m := map[string]int{}
		for _, s := range list {
			m[s]++
		}
		return m
	}
	was, now := count(before), count(after)
	for tok, n := range now {
		if n > 1 && n > was[tok] {
			return true
		}
	}
	return false
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
// WHAT THIS GATE DOES NOT COVER, stated plainly because an overstated proof is
// worse than an honest one: the desired state is derived from the file AFTER
// stage 1, so for stage 1 the comparison is against its own output. Stage 1 is
// proven structurally instead, inside RepairLine — byte conservation over the
// owner region, a byte-identical pattern, and a result that re-parses as one
// rule — and those three, not this loop, are why a repair cannot invent an
// owner or move a line's match set.
//
// Errors: *InconclusiveError (R-12, nothing written), *plan.NoOpError (already
// clean — the Result is still populated), *plan.InvalidError (a removal empties
// an owner set with no --on-empty policy, R-6; or RemoveStalePaths with no tree
// to judge staleness against), *plan.RefusalError (the gate rejected the
// synthesized edits, or --on-empty=error, or S-4's size cap).
func Build(content []byte, tree []string, v Verifier, opts Options) (*Result, error) {
	if opts.MaxSize == 0 {
		opts.MaxSize = 3_000_000
	}
	if opts.WarnSize == 0 {
		opts.WarnSize = 2_500_000
	}
	// Stage 3 decides staleness by asking which rules match nothing. Over an
	// EMPTY tree that is every rule, and the gate cannot object because it
	// iterates the tree and therefore iterates nothing — a vacuous proof for a
	// run that empties the file, at exit 0, with no ownership rows to show for
	// it (found by adversarial review; an orphan branch, an --allow-empty
	// initial commit or a tag on an empty tree all reach it). plan.Build
	// refuses a zero-match scope under R-5 for the same reason; this is lint's
	// analogue.
	if opts.RemoveStalePaths && len(tree) == 0 {
		return nil, &plan.InvalidError{Msg: "refusing --remove-stale-paths: the tree at this ref has no files, so EVERY rule matches nothing and staleness cannot distinguish a dead rule from a tree the tool cannot see — the whole file would be deleted on a proof that examined zero paths (R-5's reasoning)"}
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
	// `known` is the owner set stages 2 and 3 START from, and the gate's
	// "nothing was invented" check is scoped to exactly that: those two stages
	// only ever REMOVE, so any owner they leave behind must be one of these.
	// It is deliberately NOT a check on stage 1 — stage 1 is the only stage
	// that synthesizes a token, and it is proven in RepairLine instead, where
	// byte conservation over the owner region makes "invented" impossible
	// rather than merely detectable. Building this set before stage 1 and
	// whitelisting the repairs would have been the same circle drawn wider.
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
	// Paths whose winning rule was DELETED for emptiness under
	// --on-empty=inherit, and only those. plan.synthRemove narrows its
	// equivalent acceptance to one op's scope for a documented reason — a
	// broader one "weakened the gate" — and a whole-run counter here would
	// reproduce that mistake at file scope: one inherit-delete anywhere would
	// disarm the ownership comparison for every path in the tree, for any
	// cause (found by adversarial review).
	//
	// `repaired` is the right source even across a cascade: a second delete can
	// only displace paths whose winner was the first deleted line, and those
	// are already in the set.
	inheritPaths := map[string]bool{}
	for i := len(f.Lines) - 1; i >= 0; i-- {
		ln := f.Lines[i]
		if ln.Kind != file.LineRule {
			continue
		}
		r := ln.Rule

		if opts.RemoveStalePaths && !matchesTree(r, tree) && !matchesTree(r, opts.WorkTree) {
			// A rule that misses ONLY because of case is a typo, not a dead
			// rule, and deleting it destroys the single piece of evidence that
			// the typo happened — `/Src/ @team` becomes no line at all, and the
			// files under `src/` are quietly unowned by a run that reported
			// success. This is audit's A-5, and A-5's answer is to correct the
			// casing, which lint has no way to do safely: the tree's real
			// casing may not be the naive lowercase. Spared and handed over.
			if sugg, caseOnly := caseOnlyMiss(r, tree, opts.WorkTree); caseOnly {
				msg := fmt.Sprintf("pattern %q matches nothing ONLY because of case — CODEOWNERS is case-sensitive (S-6), so this is a typo, not a dead rule; kept, because deleting it would silently un-own the files it was meant to cover", r.PatternText)
				if sugg != "" {
					msg += fmt.Sprintf("; %q would match", sugg)
				}
				res.Actions = append(res.Actions, Action{
					Kind: ActionKeptCaseMismatch, Line: i + 1, Pattern: r.PatternText,
					Before: f.LineText(i), Reason: msg,
				})
				continue
			}
			res.Actions = append(res.Actions, Action{
				Kind: ActionRemoveStale, Line: i + 1, Pattern: r.PatternText, Before: f.LineText(i),
				Reason: fmt.Sprintf("pattern %q matches nothing tracked at this ref and nothing on disk, so it cannot win any path today and deleting it changes no CURRENT ownership; a path added later that it would have matched now falls to whatever rule remains (--remove-stale-paths; R-11 keeps this off by default because a dead pattern may be deliberate)", r.PatternText),
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
				// Record the paths this line was winning BEFORE deleting it —
				// they are the only ones whose divergence from the pure
				// owner-set transform the gate may later accept.
				for p, rr := range repaired {
					if rr.LineIndex == i {
						inheritPaths[p] = true
					}
				}
				f.DeleteLine(i)
				deletedLine = true
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
		// transform can predict. Accepted only for the paths THAT rule was
		// actually winning, and only when no dead owner came back with them;
		// any other divergence is a synthesis bug, and accepting it here would
		// launder that bug straight past the gate.
		if inheritPaths[p] && !anyDead(got, dead) {
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
	// Stage 1 appends ascending and stages 2/3 descending, so both lists are
	// sorted before anyone sees them — an out-of-order diff makes a reviewer
	// hunt for a reordering that never happened.
	sort.SliceStable(res.Actions, func(i, j int) bool { return res.Actions[i].Line < res.Actions[j].Line })
	sort.SliceStable(res.Plan.Changes, func(i, j int) bool { return res.Plan.Changes[i].Line < res.Plan.Changes[j].Line })

	if bytes.Equal(afterBytes, content) {
		// A populated Result, not nil: the caller still has to report the lines
		// it could not repair, and "clean" is the modal outcome of a scheduled
		// run.
		//
		// The message is careful about what was actually established. Owners on
		// an UNREPAIRABLE line were never looked up — they are not owners, the
		// line is invalid — so a flat "every owner exists" would assert a check
		// that never ran on precisely the lines a human still has to fix.
		msg := "nothing to lint: no repairable owner spacing, and every owner named by a valid rule exists"
		if n := res.NeedsHuman(); n > 0 {
			msg = fmt.Sprintf("nothing lint can repair: %d line(s) are invalid in ways it will not guess at, and their owners were never checked — GitHub is skipping those lines", n)
		}
		return res, &plan.NoOpError{Msg: msg}
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
		// A 404, which is where reporting and DELETING part company. GitHub
		// returns exactly this for a team that was removed and for a SECRET
		// team the caller cannot see, and ProbeOrg does not separate them: it
		// proves the token can call the endpoint, not that it can see every
		// team behind it. Only an org owner sees secret teams, so only an org
		// owner's 404 is definitive. Anything else is inconclusive — which is
		// R-12 doing its job, and costs nothing on the common path, because
		// this is only ever reached when a team already looks gone.
		admin, err := v.ViewerIsOrgAdmin(org)
		if err != nil {
			return false, "", err
		}
		if !admin {
			return false, "", &ghapi.Inconclusive{Reason: fmt.Sprintf(
				"%s was not found in %s, but this token is not an owner of that org, and a secret team returns the same 404 as a deleted one — re-run with an org-owner token to have this decided, or remove the owner by hand",
				owner, org)}
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

// caseOnlyMiss reports whether a rule matches nothing ONLY because of case,
// and — separately — a corrected pattern, which is non-empty only when it
// VERIFIABLY matches real paths case-sensitively. A miss whose real casing is
// not simply lowercase yields ("", true): the rule is still spared, but no
// unverified suggestion is offered. Mirrors audit's A-5 (caseCorrected), over
// both the committed tree and the checkout, because either is enough to prove
// the pattern was aimed at something real.
func caseOnlyMiss(r *file.Rule, trees ...[]string) (suggestion string, caseOnly bool) {
	lowered := strings.ToLower(r.PatternText)
	p, err := pattern.Compile(lowered)
	if err != nil {
		return "", false
	}
	for _, tree := range trees {
		for _, path := range tree {
			if lowered != r.PatternText && p.Match(path) {
				return lowered, true
			}
		}
	}
	for _, tree := range trees {
		for _, path := range tree {
			if p.Match(strings.ToLower(path)) {
				return "", true
			}
		}
	}
	return "", false
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
