package lint_test

// This file is the adversarial half of the lint suite: everything lint must
// REFUSE to do. The sibling file proves lint fixes what it should; this one
// proves it never fixes anything on a guess, on a partial view of the file, or
// on an answer the network could not give.
//
// SPEC R-12: the fail-closed contract, applied to the WHOLE RUN. One
// inconclusive owner lookup — a rate limit, a 401 from an expired token, an
// org this token cannot enumerate — and Build returns *lint.InconclusiveError
// having planned nothing at all. Not the repairs, not the removals it WAS sure
// about, not the stale-path deletions that never touched the network. A lint
// pass that cannot see the whole file does not get to edit part of it: an
// expired token quietly stripping owners from CODEOWNERS is the worst outcome
// this tool has, because the file keeps working, the reviews keep merging, and
// nobody finds out until an unreviewed change ships.
//
// SPEC R-13: email owners are unverifiable, never dead. No API call is made
// for them, they are reported in Result.Unverifiable, and their presence alone
// never makes a run inconclusive.
//
// The rest is the boundary of lint's one guess — RepairHandle rejoining a
// handle that whitespace split in two. A merge rule that reached one token too
// far would invent an owner nobody wrote, and an invented owner in CODEOWNERS
// is a review request routed to a stranger (or to nobody). The cases below are
// the ones where a plausible implementation invents one.

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/ghapi"
	"github.com/jordonpeterson/codeowners-tool/internal/lint"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// fcVerifier is a lint.Verifier that answers from fixed tables and RECORDS
// every call. The recording is half the point: R-13 is a statement about calls
// that must not happen, and only the call log can prove a lookup was skipped
// rather than merely answered harmlessly.
//
// Unknown owners exist by default, so a test names only the owners it wants
// dead (missingUsers/missingTeams) or undecidable (userErr/teamErr/orgErr).
type fcVerifier struct {
	mu    sync.Mutex
	calls []string

	missingUsers map[string]bool
	missingTeams map[string]bool

	orgErr  map[string]error
	teamErr map[string]error
	userErr map[string]error
	// notAdmin marks orgs this token does not own; adminErr makes the
	// ownership lookup itself undecidable.
	notAdmin map[string]bool
	adminErr map[string]error
}

func fcNew() *fcVerifier {
	return &fcVerifier{
		missingUsers: map[string]bool{},
		missingTeams: map[string]bool{},
		orgErr:       map[string]error{},
		teamErr:      map[string]error{},
		userErr:      map[string]error{},
		notAdmin:     map[string]bool{},
		adminErr:     map[string]error{},
	}
}

// fcInconclusive builds the exact error type a real *ghapi.Client returns when
// a lookup could not be decided (R-12).
func fcInconclusive(reason string) error { return &ghapi.Inconclusive{Reason: reason} }

func (v *fcVerifier) record(call string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, call)
}

func (v *fcVerifier) ProbeOrg(org string) error {
	v.record("ProbeOrg(" + org + ")")
	return v.orgErr[org]
}

func (v *fcVerifier) TeamExists(org, slug string) (bool, error) {
	key := org + "/" + slug
	v.record("TeamExists(" + key + ")")
	if err := v.teamErr[key]; err != nil {
		return false, err
	}
	return !v.missingTeams[key], nil
}

func (v *fcVerifier) UserExists(login string) (bool, error) {
	v.record("UserExists(" + login + ")")
	if err := v.userErr[login]; err != nil {
		return false, err
	}
	return !v.missingUsers[login], nil
}

// ViewerIsOrgAdmin is what separates "this team was deleted" from "this token
// cannot see this team". Defaults to owner so existing cases are unaffected.
func (v *fcVerifier) ViewerIsOrgAdmin(org string) (bool, error) {
	v.record("ViewerIsOrgAdmin(" + org + ")")
	if err := v.adminErr[org]; err != nil {
		return false, err
	}
	return !v.notAdmin[org], nil
}

// fcCalls returns a copy of the call log.
func (v *fcVerifier) fcCalls() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.calls...)
}

// fcMentions reports whether any recorded call contains sub.
func (v *fcVerifier) fcMentions(sub string) bool {
	for _, c := range v.fcCalls() {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// fcOpts is the options every Build test starts from: an explicit --on-empty
// policy, so that a test about fail-closed behavior can never be answered by
// R-6's "no default" refusal instead.
func fcOpts() lint.Options { return lint.Options{OnEmpty: "unowned"} }

// fcAfter returns the after-content a Build result carries, or "" when there is
// no plan to speak of.
func fcAfter(res *lint.Result) string {
	if res == nil || res.Plan == nil {
		return ""
	}
	return res.Plan.AfterContent
}

// fcAssertNothingWritten is R-12 stated as one assertion: the run failed
// closed, and nothing came back that would rewrite a single byte of the file.
func fcAssertNothingWritten(t *testing.T, res *lint.Result, err error, content []byte) *lint.InconclusiveError {
	t.Helper()
	var ie *lint.InconclusiveError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v (%T), want *lint.InconclusiveError (R-12)", err, err)
	}
	if after := fcAfter(res); after != "" && after != string(content) {
		t.Fatalf("R-12 violated: an inconclusive run produced a rewritten file:\n%q", after)
	}
	if res != nil && res.Plan != nil && len(res.Plan.Changes) != 0 {
		t.Fatalf("R-12 violated: plan carries %d line change(s) after an inconclusive lookup", len(res.Plan.Changes))
	}
	if len(ie.Reasons) == 0 {
		t.Error("InconclusiveError carries no reasons — an operator cannot tell what to fix or re-run")
	}
	return ie
}

// fcAssertNotRemoved fails if any recorded action removed the named owner.
func fcAssertNotRemoved(t *testing.T, res *lint.Result, owner string) {
	t.Helper()
	if res == nil {
		return
	}
	for _, a := range res.Actions {
		if a.Kind == lint.ActionRemoveOwner && a.Owner == owner {
			t.Fatalf("owner %s was removed: %+v", owner, a)
		}
	}
}

// fcActionsOfKind returns the recorded actions of one kind.
func fcActionsOfKind(res *lint.Result, kind string) []lint.Action {
	var out []lint.Action
	if res == nil {
		return out
	}
	for _, a := range res.Actions {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// ---------- R-12: the whole run fails closed ----------

// SPEC R-12: a single user lookup that could not be decided — a rate limit, a
// 5xx, a dropped connection — stops the ENTIRE run. Nothing is repaired,
// nothing is removed, and the error names the owner so an operator knows what
// to re-run rather than being told only that "something" failed.
//
// Without this, the tool's behavior degrades exactly when GitHub is degraded:
// a rate-limited lookup reads as "this owner does not exist" and the owner is
// dropped from a file that is otherwise fine.
func TestR12_InconclusiveUserLookupStopsTheRun(t *testing.T) {
	v := fcNew()
	v.userErr["flaky"] = fcInconclusive("rate limited on /users/flaky")

	content := []byte("* @keep\n/x/ @flaky\n")
	res, err := lint.Build(content, []string{"x/a.go", "y.go"}, v, fcOpts())

	ie := fcAssertNothingWritten(t, res, err, content)
	if joined := strings.Join(ie.Reasons, "\n"); !strings.Contains(joined, "flaky") {
		t.Errorf("reasons = %v; must name the owner or the reason that could not be decided", ie.Reasons)
	}
}

// SPEC R-12: a team lookup that returns an error is inconclusive, not a
// negative. The Verifier contract is explicit that an error means "could not
// be decided" — so ANY error from TeamExists, not merely a *ghapi.Inconclusive,
// must fail the whole run closed.
//
// A team is the owner shape most likely to be looked up through a token with
// partial org scopes; treating its errors as "team is gone" would strip the
// owners of whole directories at the moment the API is least reliable.
func TestR12_TeamLookupErrorStopsTheRun(t *testing.T) {
	v := fcNew()
	v.teamErr["org/team"] = errors.New("dial tcp 140.82.121.6:443: connection reset by peer")

	content := []byte("* @keep\n/x/ @org/team\n")
	res, err := lint.Build(content, []string{"x/a.go", "y.go"}, v, fcOpts())

	fcAssertNothingWritten(t, res, err, content)
}

// SPEC R-12: until ProbeOrg proves the token can enumerate an org, that org's
// 404s are meaningless — "not visible to you" and "does not exist" are the same
// HTTP status. So a failed probe both fails the run closed AND disqualifies
// every team answer for that org, even a definitive-looking `false`.
//
// This is the mass-false-negative case: one unprivileged token against an org
// with fifty teams would otherwise delete fifty owners in a single commit, each
// removal individually "justified" by a 404.
func TestR12_ProbeOrgFailureDisqualifiesTeamNegatives(t *testing.T) {
	v := fcNew()
	v.orgErr["ghost-org"] = fcInconclusive(`org "ghost-org" not visible to this token (404 on /orgs/ghost-org/teams)`)
	// The trap: the team lookup would answer "does not exist" if anyone asked
	// it without regard for the failed probe.
	v.missingTeams["ghost-org/team"] = true

	content := []byte("* @keep\n/x/ @ghost-org/team\n")
	res, err := lint.Build(content, []string{"x/a.go", "y.go"}, v, fcOpts())

	fcAssertNothingWritten(t, res, err, content)
	fcAssertNotRemoved(t, res, "@ghost-org/team")
	if !v.fcMentions("ProbeOrg(ghost-org)") {
		t.Errorf("the org was never probed at all; calls = %v — an org-scoped answer that skips the probe is not definitive", v.fcCalls())
	}
	if after := fcAfter(res); after != "" && !strings.Contains(after, "@ghost-org/team") {
		t.Fatalf("team of an unprobed org was dropped from the after-content:\n%q", after)
	}
}

// SPEC R-12: the case the rule exists for — one owner PROVEN dead sitting on
// the same line as one owner nobody could decide. The proven removal is
// correct, tempting, and must not be written: a partial edit publishes a file
// that looks reviewed and complete while silently omitting whatever the failed
// lookups would have said.
//
// The whole run is one atomic decision. Either lint saw the entire file, or it
// writes nothing and exits 5 so a human re-runs it with a working token.
func TestR12_DeadOwnerBesideAnInconclusiveOneWritesNothing(t *testing.T) {
	v := fcNew()
	v.missingUsers["dead"] = true // definitively gone
	v.userErr["flaky"] = fcInconclusive("HTTP 403 on /users/flaky — token invalid or missing a required scope")

	content := []byte("* @keep\n/docs/ @keep @dead @flaky\n")
	res, err := lint.Build(content, []string{"docs/a.md", "y.go"}, v, fcOpts())

	fcAssertNothingWritten(t, res, err, content)
	fcAssertNotRemoved(t, res, "@dead")
	if after := fcAfter(res); after != "" && !strings.Contains(after, "@dead") {
		t.Fatalf("the proven-dead owner was removed anyway — R-12 is not per-owner:\n%q", after)
	}
}

// SPEC R-12: an inconclusive lookup also blocks the stages that never touched
// the network — the whitespace repair of stage 1 and the opt-in stale-path
// deletion of stage 3.
//
// It is tempting to let the offline work through, since no API answer was
// involved. But the three stages compose into one file rewrite, and shipping
// half of it means a commit that says "lint" and contains an arbitrary subset
// of the intended change — impossible to review, and impossible to tell apart
// from the complete run by looking at the result.
func TestR12_InconclusiveAlsoBlocksTheOfflineStages(t *testing.T) {
	v := fcNew()
	v.userErr["flaky"] = fcInconclusive("rate limited on /users/flaky")

	opts := fcOpts()
	opts.RemoveStalePaths = true // stage 3 explicitly opted in

	content := []byte("* @flaky\n/x/ @ org/team\n/ghost/ @keep\n")
	res, err := lint.Build(content, []string{"x/a.go"}, v, opts)

	fcAssertNothingWritten(t, res, err, content)
	if n := len(fcActionsOfKind(res, lint.ActionRepairOwner)); n != 0 {
		t.Errorf("%d owner repair(s) survived an inconclusive run", n)
	}
	if n := len(fcActionsOfKind(res, lint.ActionRemoveStale)); n != 0 {
		t.Errorf("%d stale-rule deletion(s) survived an inconclusive run", n)
	}
}

// ---------- R-13: email owners ----------

// SPEC R-13: an email owner is never looked up. GitHub resolves it against
// commit-author addresses, not against any API this tool can query, so there
// is no endpoint that could answer and no answer that could justify removing
// it. It is reported in Result.Unverifiable and left exactly where it is.
//
// Proven from the call log, not from the output: an implementation that asks
// about `docs@example.com` and merely ignores the reply is one refactor away
// from believing it.
func TestR13_EmailOwnersAreNeverLookedUp(t *testing.T) {
	v := fcNew()

	content := []byte("* @keep\n/x/ @keep docs@example.com\n")
	res, err := lint.Build(content, []string{"x/a.go", "y.go"}, v, fcOpts())

	var ie *lint.InconclusiveError
	if errors.As(err, &ie) {
		t.Fatalf("an email owner must not make a run inconclusive: %v", ie.Reasons)
	}
	var noop *plan.NoOpError
	if err != nil && !errors.As(err, &noop) {
		t.Fatalf("err = %v (%T), want nil or *plan.NoOpError for an already-clean file", err, err)
	}
	if v.fcMentions("docs") || v.fcMentions("example.com") {
		t.Errorf("email owner was looked up (R-13): calls = %v", v.fcCalls())
	}
	if res == nil {
		t.Fatal("Result must still be populated for an already-clean file")
	}
	found := false
	for _, u := range res.Unverifiable {
		if u == "docs@example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("Unverifiable = %v, want it to list docs@example.com (R-13)", res.Unverifiable)
	}
	if after := fcAfter(res); after != "" && !strings.Contains(after, "docs@example.com") {
		t.Errorf("email owner was removed from the file:\n%q", after)
	}
}

// SPEC R-13: "unverifiable" is not "unknown". A file whose only unusual owner
// is an email address is a CLEAN file — it must not exit 5, and it must not
// make lint refuse the removals it was otherwise sure about.
//
// Conflating the two would put every repo that routes ownership through a
// mailing list into permanent exit-5 limbo, and the operator response to that
// is to stop running the tool.
func TestR13_EmailOwnerAloneIsNotInconclusive(t *testing.T) {
	v := fcNew()

	content := []byte("/x/ docs@example.com\n")
	res, err := lint.Build(content, []string{"x/a.go"}, v, fcOpts())

	var ie *lint.InconclusiveError
	if errors.As(err, &ie) {
		t.Fatalf("email-only file marked inconclusive (R-13): %v", ie.Reasons)
	}
	if calls := v.fcCalls(); len(calls) != 0 {
		t.Errorf("no lookup at all should have been made, got %v", calls)
	}
	if after := fcAfter(res); after != "" && after != string(content) {
		t.Errorf("clean email-only file was rewritten:\n%q", after)
	}
}

// ---------- RepairHandle: the boundary of lint's one guess ----------

// fcJoined concatenates a token slice, which is the invariant every repair must
// preserve: merging removes only the whitespace BETWEEN tokens, so the bytes of
// the tokens themselves are the same before and after.
func fcJoined(tokens []string) string { return strings.Join(tokens, "") }

// SPEC R-12/§4: RepairHandle merges tokens only where the join sits inside what
// can only be one handle. Each case below is a join a looser rule would take,
// and each one would MINT AN OWNER that appears nowhere in the file: `@a b`
// becoming `@ab`, `a@b.com /x` becoming the perfectly valid address
// `a@b.com/x`, `@a @b` becoming `@a@b`.
//
// An invented owner is worse than a broken line. A broken line is skipped by
// GitHub and shows up in `audit`; an invented owner silently routes review
// requests to a stranger, or to a handle that will exist the day somebody
// registers it.
func TestRepairHandle_NeverInventsAnOwner(t *testing.T) {
	cases := []struct {
		name   string
		tokens []string
		why    string
	}{
		{
			name:   "handle then bare word",
			tokens: []string{"@a", "b"},
			why:    "the join touches neither an @ nor a /, so `@a b` must never become `@ab`",
		},
		{
			name:   "email then path fragment",
			tokens: []string{"a@b.com", "/x"},
			why:    "the run does not start with @; merging would invent the VALID address a@b.com/x",
		},
		{
			name:   "two complete handles",
			tokens: []string{"@a", "@b"},
			why:    "`@a@b` is not a valid handle, and these are two owners a human deliberately wrote",
		},
		{
			name:   "bare at sign then handle",
			tokens: []string{"@", "@x"},
			why:    "the join sits against an @, but the concatenation `@@x` is not a valid handle",
		},
		{
			name:   "lone at sign",
			tokens: []string{"@"},
			why:    "there is nothing to merge with; a one-token run is never a repair",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]string(nil), tc.tokens...)
			got, changed := lint.RepairHandle(tc.tokens)
			if changed {
				t.Errorf("changed = true, want false: %s (got %q)", tc.why, got)
			}
			if len(got) != len(in) {
				t.Fatalf("tokens = %q, want them left alone as %q: %s", got, in, tc.why)
			}
			for i := range in {
				if got[i] != in[i] {
					t.Fatalf("tokens = %q, want %q: %s", got, in, tc.why)
				}
			}
		})
	}
}

// SPEC §4: the control for the cases above — the ONE shape a repair is allowed
// to take. `@ org/team` is not two owners that do not exist; it is one owner
// nobody has looked up yet, and the join sits directly against the `@`.
//
// Without a passing control, every refusal test above would also pass against
// an implementation that simply never merges — and the split handle, which
// GitHub skips silently and which is the entire reason stage 1 exists, would go
// unfixed forever.
func TestRepairHandle_MergesTheSplitHandleItIsFor(t *testing.T) {
	got, changed := lint.RepairHandle([]string{"@", "org/team"})
	if !changed {
		t.Fatalf("changed = false; `@ org/team` is the split handle stage 1 exists to rejoin")
	}
	if len(got) != 1 || got[0] != "@org/team" {
		t.Fatalf("tokens = %q, want [@org/team]", got)
	}
	if fcJoined(got) != fcJoined([]string{"@", "org/team"}) {
		t.Fatalf("a repair may only remove whitespace between tokens: %q", got)
	}
}

// SPEC R-12: an empty or nil token list is what a zero-owner rule (S-9) and a
// comment-only line hand to RepairHandle. Both must come back unchanged and
// neither may panic — lint runs over whole files nobody vetted first, and a
// panic in a repair pass takes down a run that had real fixes queued behind it.
func TestRepairHandle_EmptyAndNilAreLeftAlone(t *testing.T) {
	for _, tokens := range [][]string{nil, {}} {
		got, changed := lint.RepairHandle(tokens)
		if changed {
			t.Errorf("changed = true for %#v; there is nothing to repair", tokens)
		}
		if len(got) != 0 {
			t.Errorf("tokens = %q for input %#v, want empty", got, tokens)
		}
	}
}

// SPEC R-12: a long run of tokens terminates, and terminates without inventing
// bytes. A merge rule implemented as "restart the scan after every merge" is
// easy to write and quadratic-to-nonterminating on a pathological line; a
// CODEOWNERS file is attacker-influenced input in any repo that takes pull
// requests, so a line that hangs the linter is a denial of service on every
// downstream check that waits for it.
//
// The byte-level invariant is asserted alongside: whatever the merge decides,
// concatenating the result must equal concatenating the input, because the only
// bytes a repair may delete are the whitespace BETWEEN tokens.
func TestRepairHandle_LongTokenRunTerminates(t *testing.T) {
	var tokens []string
	for i := 0; i < 25; i++ {
		tokens = append(tokens, "@", "a")
	}
	in := append([]string(nil), tokens...)

	got, _ := lint.RepairHandle(tokens)

	if len(got) > len(in) {
		t.Fatalf("repair produced MORE tokens (%d) than it was given (%d)", len(got), len(in))
	}
	if fcJoined(got) != fcJoined(in) {
		t.Fatalf("repair changed the bytes: %q, want a regrouping of %q", fcJoined(got), fcJoined(in))
	}
	for _, tok := range got {
		if strings.ContainsAny(tok, " \t") {
			t.Fatalf("repaired token %q contains whitespace", tok)
		}
	}
}

// ---------- RepairLine: a repair that changes the pattern is not a repair ----------

// SPEC R-12/INV-5: the repaired line must re-parse with a BYTE-IDENTICAL
// pattern token. The case that catches a naive implementation is a pattern
// containing — or ending in — an ESCAPED space, which is a legal way to write a
// path with a space in it: `/trailing\ ` is a pattern, and the space after it is
// the separator.
//
// Anything that splits on whitespace, or trims it, turns `/trailing\ ` into
// `/trailing` — a pattern governing a DIFFERENT set of files. The line would
// still look repaired, the owners would still look right, and ownership of two
// directories would have silently swapped. file.SplitOwnerRegion exists so this
// scan is written once; this test is what proves lint uses it.
func TestRepairLine_PreservesAnEscapedSpaceInThePattern(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantFixed   string
		wantPattern string
	}{
		{
			name:        "escaped space inside the pattern",
			raw:         `/a\ /b @ org/team`,
			wantFixed:   `/a\ /b @org/team`,
			wantPattern: `/a\ /b`,
		},
		{
			name:        "pattern ending in an escaped space",
			raw:         `/trailing\  @ org/team`,
			wantFixed:   `/trailing\  @org/team`,
			wantPattern: `/trailing\ `,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, owners, ok := file.SplitOwnerRegion(tc.raw)
			if !ok || owners != "@ org/team" {
				t.Fatalf("premise broken: SplitOwnerRegion(%q) = (%q, %q, %v)", tc.raw, head, owners, ok)
			}

			fixed, ok := lint.RepairLine(tc.raw)
			if !ok {
				t.Fatalf("RepairLine(%q) declined a split handle it can rejoin", tc.raw)
			}
			if fixed != tc.wantFixed {
				t.Fatalf("fixed = %q, want %q — the head must survive byte-for-byte", fixed, tc.wantFixed)
			}
			if !strings.HasPrefix(fixed, head) {
				t.Fatalf("fixed = %q does not keep the original head %q", fixed, head)
			}

			f := file.Parse([]byte(fixed))
			rules := f.Rules()
			if len(rules) != 1 {
				t.Fatalf("repaired line does not re-parse as one rule: %q", fixed)
			}
			if rules[0].PatternText != tc.wantPattern {
				t.Fatalf("pattern = %q, want %q — a repair that changes which files a line governs is not a repair",
					rules[0].PatternText, tc.wantPattern)
			}
			if got := rules[0].Owners; len(got) != 1 || got[0] != "@org/team" {
				t.Fatalf("owners = %q, want [@org/team]", got)
			}
		})
	}
}

// SPEC R-12: a line that is invalid because of its PATTERN is never "repaired".
// Rejoining its owners would produce a line that still does not parse, and
// guessing at the pattern would change which files it governs — the one thing
// lint may never do.
//
// Both inputs below are rejected by pattern.Compile, not by the owner scan:
// CODEOWNERS has no negation, and three consecutive asterisks are not a
// pattern. The right outcome is to hand the line back untouched so it surfaces
// as ActionUnrepairable and a human decides.
func TestRepairLine_InvalidPatternIsNeverRepaired(t *testing.T) {
	for _, raw := range []string{
		`!nope @ org/team`,
		`a***b @ org/team`,
	} {
		if f := file.Parse([]byte(raw)); len(f.InvalidLines()) != 1 {
			t.Fatalf("premise broken: %q must be an invalid line", raw)
		}
		fixed, ok := lint.RepairLine(raw)
		if ok {
			t.Errorf("RepairLine(%q) = (%q, true); an unparseable PATTERN is not repairable", raw, fixed)
		}
		if fixed != "" && fixed != raw {
			t.Errorf("RepairLine(%q) returned rewritten text %q while declining the repair", raw, fixed)
		}
	}
}

// SPEC S-9: a rule with NO owners is legal and deliberate — it is how a repo
// says "this path is explicitly unowned", and it is what --on-empty=unowned
// writes. There is nothing to rejoin, so RepairLine declines.
//
// Reported, because an implementation that treats "no owner tokens" as "a
// handle got split" would start manufacturing owners out of the pattern itself,
// and would do it on lines the file's authors were most deliberate about.
func TestRepairLine_ZeroOwnerRuleIsLeftAlone(t *testing.T) {
	for _, raw := range []string{
		`/x/`,
		`/x/ # deliberately unowned`,
	} {
		if f := file.Parse([]byte(raw)); len(f.InvalidLines()) != 0 {
			t.Fatalf("premise broken: %q is a legal zero-owner rule (S-9)", raw)
		}
		fixed, ok := lint.RepairLine(raw)
		if ok {
			t.Errorf("RepairLine(%q) = (%q, true); a zero-owner rule is not a broken one", raw, fixed)
		}
	}
}

// ---------- whole-file integrity ----------

// SPEC R-12: a line lint cannot repair without guessing is REPORTED as
// ActionUnrepairable and left byte-for-byte in the output — never deleted,
// never rewritten — even on a run that rewrites other lines around it.
//
// Deleting it would destroy a rule somebody wrote and believes is in force, and
// destroy it in the one commit an operator is least likely to read closely: the
// automated one. The correct outcome is a file that still contains the broken
// line and a report that says so.
func TestLint_UnrepairableLineIsReportedNotRewritten(t *testing.T) {
	v := fcNew()
	v.missingUsers["dead"] = true

	content := []byte("* @keep\n!nope @keep\n/x/ @keep @dead\n")
	res, err := lint.Build(content, []string{"x/a.go", "y.go"}, v, fcOpts())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	after := fcAfter(res)
	if !strings.Contains(after, "!nope @keep") {
		t.Fatalf("the unrepairable line was rewritten or deleted:\n%q", after)
	}
	if strings.Contains(after, "@dead") {
		t.Fatalf("the dead owner survived the run that was supposed to remove it:\n%q", after)
	}
	acts := fcActionsOfKind(res, lint.ActionUnrepairable)
	if len(acts) != 1 {
		t.Fatalf("unrepairable actions = %+v, want exactly one for line 2", res.Actions)
	}
	if acts[0].Line != 2 {
		t.Errorf("unrepairable action reported line %d, want 2", acts[0].Line)
	}
	if acts[0].Reason == "" {
		t.Error("an unrepairable line must come with the reason lint declined it")
	}
}

// SPEC INV-5: lint never reorders lines, never touches comments or blank lines,
// and preserves a final line that has no trailing newline.
//
// Every one of those is a diff an operator has to read past to find the change
// that matters. A lint commit whose diff is thirty reflowed lines and one real
// removal will be approved without being read, which defeats the point of
// routing the fix through review at all; adding a trailing newline nobody asked
// for is the same failure in miniature.
func TestLint_PreservesOrderCommentsAndAMissingFinalNewline(t *testing.T) {
	v := fcNew()
	v.missingUsers["dead"] = true

	content := []byte("# top comment\n\n* @keep\n# middle\n/x/ @keep @dead")
	res, err := lint.Build(content, []string{"x/a.go", "y.go"}, v, fcOpts())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	after := fcAfter(res)
	if after == "" {
		t.Fatal("no after-content: the dead owner should have produced a plan")
	}
	if strings.HasSuffix(after, "\n") {
		t.Errorf("a final line without a newline gained one:\n%q", after)
	}
	before := strings.Split(string(content), "\n")
	got := strings.Split(after, "\n")
	if len(got) != len(before) {
		t.Fatalf("line count changed: %d -> %d\n%q", len(before), len(got), after)
	}
	for i := 0; i < 4; i++ {
		if got[i] != before[i] {
			t.Errorf("line %d changed: %q -> %q; only the line with the dead owner may move", i+1, before[i], got[i])
		}
	}
	if got[4] != "/x/ @keep" {
		t.Errorf("line 5 = %q, want %q", got[4], "/x/ @keep")
	}
}

// SPEC INV-5: a CRLF file stays CRLF, including on the lines lint edits.
//
// Windows-authored repositories are the common case for this, and a single
// LF-terminated line in a CRLF file is the kind of change that makes a
// whole-file diff on the next unrelated edit — or trips a repo's
// end-of-line lint and blocks CI for a reason that has nothing to do with
// ownership.
func TestLint_CRLFFileStaysCRLF(t *testing.T) {
	v := fcNew()
	v.missingUsers["dead"] = true

	content := []byte("# owners\r\n* @keep\r\n/x/ @keep @dead\r\n")
	res, err := lint.Build(content, []string{"x/a.go", "y.go"}, v, fcOpts())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	after := fcAfter(res)
	if after == "" {
		t.Fatal("no after-content: the dead owner should have produced a plan")
	}
	parts := strings.Split(after, "\n")
	for i, ln := range parts[:len(parts)-1] { // the tail after the last "\n" is not a line
		if !strings.HasSuffix(ln, "\r") {
			t.Errorf("line %d lost its CR: %q", i+1, ln)
		}
	}
	if !strings.Contains(after, "/x/ @keep\r\n") {
		t.Errorf("edited line did not keep its CRLF:\n%q", after)
	}
}

// SPEC R-12: a file with nothing in it, and a file with nothing but comments,
// are no-ops — *plan.NoOpError, exit 1, and above all no panic.
//
// A brand-new repo and a repo whose CODEOWNERS is still a template are exactly
// the inputs a fleet-wide scheduled run hits first and most often. A panic
// there takes down the run for every other repo behind it in the batch.
func TestLint_EmptyAndCommentOnlyFilesAreNoOps(t *testing.T) {
	for _, content := range [][]byte{
		[]byte(""),
		[]byte("# CODEOWNERS\n# nothing here yet\n"),
	} {
		v := fcNew()
		res, err := lint.Build(content, []string{"a.go"}, v, fcOpts())
		var noop *plan.NoOpError
		if !errors.As(err, &noop) {
			t.Fatalf("Build(%q) err = %v (%T), want *plan.NoOpError", content, err, err)
		}
		if after := fcAfter(res); after != "" && after != string(content) {
			t.Errorf("Build(%q) rewrote a file it had nothing to do to:\n%q", content, after)
		}
		if calls := v.fcCalls(); len(calls) != 0 {
			t.Errorf("Build(%q) made API calls for a file with no owners: %v", content, calls)
		}
	}
}

// SPEC S-4: a result over the size cap is refused with *plan.RefusalError, not
// written and truncated, not written and warned about.
//
// GitHub silently ignores a CODEOWNERS file over 3 MB — the whole file, with no
// error anywhere in the UI. A lint pass that pushes a file over that line
// converts "some owners are stale" into "this repository has no code owners at
// all", which is the failure this tool exists to prevent.
func TestS4_ResultOverMaxSizeIsRefused(t *testing.T) {
	v := fcNew()
	v.missingUsers["dead"] = true

	opts := fcOpts()
	opts.MaxSize = 8 // far under anything lint could produce

	content := []byte("* @keep\n/x/ @keep @dead\n")
	res, err := lint.Build(content, []string{"x/a.go", "y.go"}, v, opts)

	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("err = %v (%T), want *plan.RefusalError (S-4)", err, err)
	}
	if after := fcAfter(res); after != "" && after != string(content) {
		t.Errorf("a refused plan still carried a rewritten file:\n%q", after)
	}
}
