// Package lint_test defines the whole-file repair pass behind `audit --lint`.
//
// `audit` reports; lint FIXES — which makes it the one Engine B path that can
// destroy ownership, so every guarantee here is about what it is NOT allowed to
// do while fixing. The three stages run in a fixed order, and the order is the
// product: a handle split by whitespace is rejoined BEFORE anybody asks the
// network whether it exists, because `@ org/team` is one owner nobody has
// looked up yet, not two owners that are missing.
//
// SPEC R-0: lint never writes. Build returns a *plan.Plan for apply.Apply,
// which remains the system's single writer.
// SPEC R-6: a removal that would empty a rule's owner set requires an explicit
// --on-empty policy; there is deliberately no default.
// SPEC R-11: deleting a rule that matches zero tracked files is OPT-IN. A dead
// pattern may be deliberate and forward-looking, and deleting it destroys
// intent that no other record holds.
// SPEC R-13: email owners are unverifiable, never dead — reported in
// Result.Unverifiable and left exactly where they are.
// SPEC INV-5: every byte lint did not decide to change survives — comments,
// blank lines, column alignment and CRLF line endings included.
//
// Stage-1-before-stage-2 is the requirement with teeth: repair first, then
// verify the REPAIRED handle. Verifying first would ask about tokens nobody
// wrote, get "does not exist" for both halves, and delete a live team's
// ownership under the banner of cleanup.
package lint_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/lint"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
	"github.com/jordonpeterson/codeowners-tool/internal/resolve"
)

// fakeVerifier answers existence questions from fixed tables and records the
// order in which it was asked. The recording is not decoration: the only way to
// prove stage 1 ran before stage 2 is to look at what the network was asked
// about, and an assertion on the output alone cannot distinguish "repaired then
// verified" from "verified the wrong thing and got lucky".
type fakeVerifier struct {
	users map[string]bool  // login -> exists
	teams map[string]bool  // "org/slug" -> exists
	orgs  map[string]error // org -> ProbeOrg result (absent = enumerable)
	// notAdmin marks orgs the token does NOT own. Only consulted on a team
	// 404, where an owner's "not found" is definitive and everyone else's is
	// indistinguishable from a secret team.
	notAdmin map[string]bool
	calls    []string // call log, in order
}

var _ lint.Verifier = (*fakeVerifier)(nil)

func newVerifier() *fakeVerifier {
	return &fakeVerifier{
		users:    map[string]bool{},
		teams:    map[string]bool{},
		orgs:     map[string]error{},
		notAdmin: map[string]bool{},
	}
}

func (v *fakeVerifier) ProbeOrg(org string) error {
	v.calls = append(v.calls, "ProbeOrg("+org+")")
	return v.orgs[org]
}

func (v *fakeVerifier) TeamExists(org, slug string) (bool, error) {
	v.calls = append(v.calls, "TeamExists("+org+"/"+slug+")")
	return v.teams[org+"/"+slug], nil
}

func (v *fakeVerifier) UserExists(login string) (bool, error) {
	v.calls = append(v.calls, "UserExists("+login+")")
	return v.users[login], nil
}

// ViewerIsOrgAdmin defaults to true here so the cases in this file stay about
// what they are about. It is only consulted when a team already 404s, and
// failclosed_test.go owns the cases where the answer is no — a token that is
// not an org owner cannot tell a deleted team from a secret one, and lint must
// then refuse to delete.
func (v *fakeVerifier) ViewerIsOrgAdmin(org string) (bool, error) {
	v.calls = append(v.calls, "ViewerIsOrgAdmin("+org+")")
	if v.notAdmin[org] {
		return false, nil
	}
	return true, nil
}

// asked reports whether any recorded call contains sub.
func (v *fakeVerifier) asked(sub string) bool {
	for _, c := range v.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// liveOrg returns a verifier for an org whose teams are `live` (and whatever
// else the caller adds); `dead` is deliberately absent, i.e. proven gone.
func liveOrg(teams ...string) *fakeVerifier {
	v := newVerifier()
	for _, t := range teams {
		v.teams[t] = true
	}
	return v
}

func actionsOfKind(res *lint.Result, kind string) []lint.Action {
	var out []lint.Action
	for _, a := range res.Actions {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

func rowFor(t *testing.T, p *plan.Plan, path string) plan.Row {
	t.Helper()
	for _, r := range p.Rows {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("no ownership row for %q; rows = %+v", path, p.Rows)
	return plan.Row{}
}

// ---------- stage 1: RepairHandle ----------

// SPEC (stage 1): RepairHandle is exported and pure over tokens because it is
// a GUESS, and a guess is only tolerable when its boundaries can be stated and
// tested directly. The boundary is: the run starts at an "@" token, every join
// sits against an "@" or a "/", and the concatenation is a valid handle. This
// table is that boundary, written out — both the merges it must make and, just
// as importantly, the ones it must refuse. `@a @b` are two owners; inventing
// `@a@b` from them would silently delete two review requests and create one
// for nobody.
func TestRepairHandle_MergeBoundary(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []string
		changed bool
	}{
		{
			name:    "space after the at sign",
			in:      []string{"@", "org/team"},
			want:    []string{"@org/team"},
			changed: true,
		},
		{
			// `@org /team` is NOT repaired, and this is the single most
			// important entry in the table. On a CODEOWNERS line everything
			// after the pattern is an owner, so these two tokens are shaped
			// exactly like `@alice /docs` — somebody putting two rules on one
			// line. Repairing the first spelling means repairing the second,
			// and repairing the second hands `/docs`'s owner every file under
			// `/src` while `/docs` silently keeps the catch-all. Adversarial
			// review produced that exact write, at exit 0.
			//
			// The rule that prevents it: a run may only START from a token that
			// is not already a valid owner. `@org` is one, so it is never
			// merged into anything, and the line is reported unrepairable —
			// which is the only honest answer to an ambiguity.
			name:    "valid handle followed by a path-shaped token never fuses",
			in:      []string{"@org", "/team"},
			want:    []string{"@org", "/team"},
			changed: false,
		},
		{
			// `@ org /team` is refused too, and a second review is why. The
			// first fix guarded only the run's FIRST token, so this row used to
			// assert `[@org/team]` — and that made `/src @ alice /docs @bob`
			// fuse into `@alice/docs`, handing `@bob`'s directory away at exit
			// 0. Brokenness is a property of the first join: after `@`+`org`
			// the accumulator is `@org`, an ordinary owner, and `@org` +
			// `/team` is byte-for-byte the ambiguity above.
			//
			// `@org` is still assembled — that part is unambiguous — but
			// `/team` is left alone, so the LINE stays invalid and RepairLine
			// reports it rather than writing a guess.
			name:    "a valid accumulator will not swallow a path-shaped token",
			in:      []string{"@", "org", "/team"},
			want:    []string{"@org", "/team"},
			changed: true,
		},
		{
			name:    "space after the slash",
			in:      []string{"@org/", "team"},
			want:    []string{"@org/team"},
			changed: true,
		},
		{
			name:    "handle shattered into four tokens",
			in:      []string{"@", "org", "/", "team"},
			want:    []string{"@org/team"},
			changed: true,
		},
		{
			name:    "two whole handles never fuse",
			in:      []string{"@a", "@b"},
			want:    []string{"@a", "@b"},
			changed: false,
		},
		{
			name:    "handle beside a bare word: @ab is not a repair",
			in:      []string{"@a", "b"},
			want:    []string{"@a", "b"},
			changed: false,
		},
		{
			name:    "two team handles on one line",
			in:      []string{"@org/team-a", "@org/team-b"},
			want:    []string{"@org/team-a", "@org/team-b"},
			changed: false,
		},
		{
			name:    "email owner is never repaired",
			in:      []string{"a@b.com", "/x"},
			want:    []string{"a@b.com", "/x"},
			changed: false,
		},
		{
			name:    "two broken handles on one line",
			in:      []string{"@", "org/a", "@", "org/b"},
			want:    []string{"@org/a", "@org/b"},
			changed: true,
		},
		{
			name:    "broken handle beside an intact one",
			in:      []string{"@org/keep", "@", "org/broken"},
			want:    []string{"@org/keep", "@org/broken"},
			changed: true,
		},
		{
			name:    "already valid",
			in:      []string{"@org/team"},
			want:    []string{"@org/team"},
			changed: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := lint.RepairHandle(append([]string(nil), tc.in...))
			if changed != tc.changed {
				t.Errorf("changed = %v, want %v (tokens %v -> %v)", changed, tc.changed, tc.in, got)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("RepairHandle(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The email exclusion deserves its own statement, because it is the one case
// where the merge rule would produce something SYNTACTICALLY VALID and still be
// wrong: `a@b.com` + `/x` concatenates into `a@b.com/x`, a well-formed email
// owner and an address no human ever typed. A repair that can invent a
// deliverable email address is not a repair, it is a forgery.
func TestRepairHandle_EmailConcatenationIsValidButForbidden(t *testing.T) {
	got, changed := lint.RepairHandle([]string{"a@b.com", "/x"})
	if changed {
		t.Fatalf("email owner was repaired into %v; %q is a valid address nobody wrote", got, "a@b.com/x")
	}
}

// ---------- stage 1: RepairLine ----------

// SPEC (stage 1, line level): RepairLine rewrites only the owner region. The
// pattern token, the whitespace that separates it from the owners and any
// inline comment come through untouched — a repair that shifts which files a
// line governs, or that eats the note explaining why the line exists, is a
// different edit wearing a repair's name.
func TestRepairLine_RepairsOwnersAndPreservesEverythingElse(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{
			name: "space inside the handle",
			raw:  "/x/ @ org/team",
			want: "/x/ @org/team",
			ok:   true,
		},
		{
			name: "leading spaces and a tab separator survive",
			raw:  "  /x/\t@ org/team",
			want: "  /x/\t@org/team",
			ok:   true,
		},
		{
			name: "inline comment survives",
			raw:  "/x/ @ org/team # note",
			want: "/x/ @org/team # note",
			ok:   true,
		},
		{
			name: "escaped space in the pattern is preserved byte for byte",
			raw:  "/a\\ b/ @ org/team",
			want: "/a\\ b/ @org/team",
			ok:   true,
		},
		{
			name: "blank line",
			raw:  "",
			ok:   false,
		},
		{
			name: "whitespace-only line",
			raw:  "   \t",
			ok:   false,
		},
		{
			name: "comment line",
			raw:  "# just a comment @ org/team",
			ok:   false,
		},
		{
			name: "already valid rule line",
			raw:  "/x/ @org/team",
			ok:   false,
		},
		{
			name: "owners that no merge rule can rescue",
			raw:  "/x/ bogus",
			ok:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := lint.RepairLine(tc.raw)
			if ok != tc.ok {
				t.Fatalf("RepairLine(%q) ok = %v, want %v (fixed = %q)", tc.raw, ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("RepairLine(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// The escaped space is the case that makes the pattern token non-trivial to
// find: `/a\ b/` is ONE token containing a space, so any repair that re-splits
// the line on whitespace mangles it into `/a\` and hands the rule a different
// set of files. Proven separately from the table because it is the input that
// silently changes what a line governs rather than failing loudly.
func TestRepairLine_EscapedSpacePatternIsNotResplit(t *testing.T) {
	got, ok := lint.RepairLine("/a\\ b/ @ org/team")
	if !ok {
		t.Fatal("a repairable line with an escaped space in its pattern must still be repaired")
	}
	if !strings.HasPrefix(got, "/a\\ b/ ") {
		t.Errorf("pattern token mangled: %q — the rule now governs different files", got)
	}
}

// ---------- stage 2: dead owners ----------

// SPEC A-1 (stage 2): an owner proven not to exist is removed from EVERY rule
// that lists it, and only from those rules. A deleted or renamed team is a
// review request that goes nowhere and tells nobody, which is why removal is
// worth the risk at all; a live team sharing the line is the thing that risk
// must never touch.
func TestBuild_DeadTeamRemovedFromEveryRuleLiveTeamUntouched(t *testing.T) {
	v := liveOrg("org/live")
	content := "/x/ @org/live @org/dead\n/y/ @org/dead @org/live\n/z/    @org/live\n"
	tree := []string{"x/a.go", "y/b.go", "z/c.go"}

	res, err := lint.Build([]byte(content), tree, v, lint.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "/x/ @org/live\n/y/ @org/live\n/z/    @org/live\n"
	if res.Plan.AfterContent != want {
		t.Errorf("after content =\n%q\nwant\n%q", res.Plan.AfterContent, want)
	}
	if got := len(actionsOfKind(res, lint.ActionRemoveOwner)); got != 2 {
		t.Errorf("remove-owner actions = %d, want one per rule listing the dead team", got)
	}
}

// SPEC (ordering, the headline requirement): stage 1 runs before stage 2, over
// the whole file, so the owner stage only ever sees repaired handles. Given
// `/x/ @ org/live` where org/live EXISTS, the run must rejoin the handle and
// keep the owner. Asserting on the after-content alone is not enough — the
// verifier call log is what proves the question asked was "does org/live
// exist?" and never "does @ exist?", which is the question a stage-2-first
// implementation would ask and answer "no" to, deleting a live team.
func TestBuild_RepairBeforeVerify(t *testing.T) {
	v := liveOrg("org/live")
	res, err := lint.Build([]byte("/x/ @ org/live\n"), []string{"x/a.go"}, v, lint.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "/x/ @org/live\n"; res.Plan.AfterContent != want {
		t.Fatalf("after content = %q, want %q — the repaired owner must survive stage 2", res.Plan.AfterContent, want)
	}
	if !v.asked("TeamExists(org/live)") {
		t.Errorf("verifier was never asked about the repaired handle; calls = %v", v.calls)
	}
	if v.asked("@") {
		t.Errorf("verifier was asked about a raw token, not a handle; calls = %v", v.calls)
	}
	if got := len(actionsOfKind(res, lint.ActionRemoveOwner)); got != 0 {
		t.Errorf("a live team was removed after repair: %+v", res.Actions)
	}
}

// SPEC (why the repair matters at all): GitHub skips a line with a broken
// handle entirely, so before the repair the rule owns nothing. Fixing it is
// therefore a real ownership change, not cosmetics, and it must be REPORTED as
// one — the ownership rows are what a reviewer reads before approving, and a
// repair that silently moves a path from one team to another is exactly the
// kind of change that must not arrive unannounced.
func TestBuild_RepairedLineChangesResolvedOwnership(t *testing.T) {
	v := liveOrg("org/all", "org/live")
	content := "* @org/all\n/x/ @ org/live\n"
	tree := []string{"x/a.go", "other.txt"}

	res, err := lint.Build([]byte(content), tree, v, lint.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	row := rowFor(t, res.Plan, "x/a.go")
	if !resolve.OwnersEqual(row.Before, []string{"@org/all"}) {
		t.Errorf("x/a.go before = %v, want {@org/all} — the broken line was skipped by GitHub", row.Before)
	}
	if !resolve.OwnersEqual(row.After, []string{"@org/live"}) {
		t.Errorf("x/a.go after = %v, want {@org/live}", row.After)
	}
	for _, r := range res.Plan.Rows {
		if r.Path == "other.txt" {
			t.Errorf("out-of-scope path reported as changed: %+v", r)
		}
	}
	if got := len(actionsOfKind(res, lint.ActionRepairOwner)); got != 1 {
		t.Errorf("repair actions = %+v, want exactly one", res.Actions)
	}
}

// ---------- stage 3: stale rules ----------

// SPEC R-11: a rule matching zero tracked files is REPORT-ONLY unless the
// operator asks for it. `*.tf` in a repo about to adopt Terraform is intent,
// and intent lives nowhere else in the system — so with RemoveStalePaths off,
// the file comes back untouched and the run is a no-op rather than a cleanup.
func TestBuild_StaleRuleKeptUnlessOptedIn(t *testing.T) {
	v := liveOrg("org/all", "org/live")
	content := "/ghost/ @org/live\n* @org/all\n"
	tree := []string{"a.txt"}

	res, err := lint.Build([]byte(content), tree, v, lint.Options{})
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("err = %v, want *plan.NoOpError: nothing but the stale rule is wrong, and R-11 forbids touching it", err)
	}
	if res == nil || res.Plan == nil {
		t.Fatal("a no-op run must still return a populated Result")
	}
	if res.Plan.AfterContent != content {
		t.Errorf("after content = %q, want the input verbatim", res.Plan.AfterContent)
	}
	if got := len(actionsOfKind(res, lint.ActionRemoveStale)); got != 0 {
		t.Errorf("stale removal happened without --remove-stale-paths: %+v", res.Actions)
	}
}

// SPEC R-11 (opt-in half): with the explicit instruction given, the dead rule
// is deleted — and deleting it must move NOBODY. That is the whole argument for
// why stage 3 is safe: a rule matching zero tracked files can never win a
// tracked path, so an empty ownership-row set is not an accident of this
// fixture but the property that makes the deletion provable.
func TestBuild_StaleRuleRemovedOnOptInWithoutMovingOwnership(t *testing.T) {
	v := liveOrg("org/all", "org/live")
	content := "/ghost/ @org/live\n* @org/all\n"
	tree := []string{"a.txt"}

	// WorkTree is a precondition of stage 3, not a nicety: without it staleness
	// would be judged against the committed tree alone while the edit lands on
	// the working-tree file. Here the checkout and the tree agree.
	res, err := lint.Build([]byte(content), tree, v, lint.Options{RemoveStalePaths: true, WorkTree: tree})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "* @org/all\n"; res.Plan.AfterContent != want {
		t.Errorf("after content = %q, want %q", res.Plan.AfterContent, want)
	}
	stale := actionsOfKind(res, lint.ActionRemoveStale)
	if len(stale) != 1 {
		t.Fatalf("stale actions = %+v, want exactly one", res.Actions)
	}
	if stale[0].Line != 1 {
		t.Errorf("stale action line = %d, want 1 (1-based, at the time of the action)", stale[0].Line)
	}
	if stale[0].Pattern != "/ghost/" {
		t.Errorf("stale action pattern = %q, want /ghost/", stale[0].Pattern)
	}
	if len(res.Plan.Rows) != 0 {
		t.Errorf("deleting a rule that matches nothing changed ownership: %+v", res.Plan.Rows)
	}
}

// ---------- R-6: emptying an owner set ----------

// SPEC R-6: removing the last owner of a rule is a reassignment of everything
// that rule governs, so the tool refuses to pick a policy on the operator's
// behalf. The four outcomes are genuinely different products — an error, an
// explicitly unowned path, or a fallthrough to the broader rule above — and
// picking a default would mean guessing which one a reviewer meant.
func TestBuild_OnEmptyPolicies(t *testing.T) {
	content := "* @org/all\n/x/ @org/dead\n"
	tree := []string{"x/a.go", "other.txt"}

	t.Run("no policy is invalid input", func(t *testing.T) {
		v := liveOrg("org/all")
		_, err := lint.Build([]byte(content), tree, v, lint.Options{})
		var invalid *plan.InvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("err = %v (%T), want *plan.InvalidError — R-6 has deliberately no default", err, err)
		}
	})

	t.Run("error refuses the run", func(t *testing.T) {
		v := liveOrg("org/all")
		_, err := lint.Build([]byte(content), tree, v, lint.Options{OnEmpty: "error"})
		var refusal *plan.RefusalError
		if !errors.As(err, &refusal) {
			t.Fatalf("err = %v (%T), want *plan.RefusalError", err, err)
		}
	})

	t.Run("unowned keeps the pattern with zero owners", func(t *testing.T) {
		v := liveOrg("org/all")
		res, err := lint.Build([]byte(content), tree, v, lint.Options{OnEmpty: "unowned"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if want := "* @org/all\n/x/\n"; res.Plan.AfterContent != want {
			t.Fatalf("after content = %q, want %q", res.Plan.AfterContent, want)
		}
		row := rowFor(t, res.Plan, "x/a.go")
		if !resolve.OwnersEqual(row.After, []string{}) {
			t.Errorf("x/a.go after = %v, want an explicitly empty owner set — not a fallthrough", row.After)
		}
	})

	t.Run("inherit deletes the line and the path falls through", func(t *testing.T) {
		v := liveOrg("org/all")
		res, err := lint.Build([]byte(content), tree, v, lint.Options{OnEmpty: "inherit"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if want := "* @org/all\n"; res.Plan.AfterContent != want {
			t.Fatalf("after content = %q, want %q", res.Plan.AfterContent, want)
		}
		row := rowFor(t, res.Plan, "x/a.go")
		if !resolve.OwnersEqual(row.Before, []string{"@org/dead"}) {
			t.Errorf("x/a.go before = %v, want {@org/dead}", row.Before)
		}
		if !resolve.OwnersEqual(row.After, []string{"@org/all"}) {
			t.Errorf("x/a.go after = %v, want {@org/all} — the broader rule above must take over", row.After)
		}
	})
}

// ---------- no-op, byte preservation, reporting ----------

// SPEC (exit 1, and the modal outcome): a file with nothing wrong with it must
// come back as a no-op WITH a populated Result. A scheduled fleet run over
// hundreds of repos is mostly this case, and a nil result would leave the sync
// record unable to say a repo was checked and found clean — indistinguishable
// from never having run.
func TestBuild_AlreadyCleanIsNoOpWithPopulatedResult(t *testing.T) {
	v := liveOrg("org/all")
	content := "# owners of everything\n\n*    @org/all\n"

	res, err := lint.Build([]byte(content), []string{"a.go"}, v, lint.Options{})
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("err = %v (%T), want *plan.NoOpError", err, err)
	}
	if res == nil || res.Plan == nil {
		t.Fatal("no-op must still populate the Result — a fleet run needs to record 'checked, clean'")
	}
	if res.Plan.AfterContent != content {
		t.Errorf("after content = %q, want the input verbatim", res.Plan.AfterContent)
	}
	if len(res.Actions) != 0 {
		t.Errorf("actions on a clean file = %+v, want none", res.Actions)
	}
}

// SPEC INV-5: lint edits owner tokens, not files. Comments, blank lines, the
// column alignment somebody hand-maintained and the trailing spacing of rules
// it did not touch all come through byte-identical. A tool that reformats the
// file while fixing one owner produces a diff no reviewer can read, and an
// unreadable diff is how a bad edit gets approved.
func TestBuild_PreservesUntouchedBytes(t *testing.T) {
	v := liveOrg("org/live")
	content := "# header comment\n\n/x/     @org/live @org/dead\n/y/   @org/live    # keep this note\n\n# trailing comment\n"
	tree := []string{"x/a.go", "y/b.go"}

	res, err := lint.Build([]byte(content), tree, v, lint.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "# header comment\n\n/x/     @org/live\n/y/   @org/live    # keep this note\n\n# trailing comment\n"
	if res.Plan.AfterContent != want {
		t.Errorf("after content =\n%q\nwant\n%q", res.Plan.AfterContent, want)
	}
}

// SPEC INV-5 (line endings): a CRLF file stays a CRLF file. Rewriting line
// endings touches every line at once, which turns a one-owner fix into a
// whole-file diff and, on a repo with mixed tooling, into a merge conflict for
// everybody — a cost paid by people who never asked lint to run.
func TestBuild_PreservesCRLFLineEndings(t *testing.T) {
	v := liveOrg("org/live")
	content := "# header\r\n\r\n/x/ @org/live @org/dead\r\n/keep/    @org/live\r\n"
	tree := []string{"x/a.go", "keep/b.go"}

	res, err := lint.Build([]byte(content), tree, v, lint.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "# header\r\n\r\n/x/ @org/live\r\n/keep/    @org/live\r\n"
	if res.Plan.AfterContent != want {
		t.Errorf("after content =\n%q\nwant\n%q", res.Plan.AfterContent, want)
	}
	if strings.Contains(strings.ReplaceAll(res.Plan.AfterContent, "\r\n", ""), "\n") {
		t.Error("a bare LF appeared in a CRLF file")
	}
}

// SPEC R-13: an email owner cannot be verified against the GitHub API at all,
// so it is UNVERIFIABLE, not dead. Treating "cannot check" as "does not exist"
// is the single worst failure mode available to this package, and the rule is
// absolute: report the address, ask nobody about it, leave it on the line.
func TestBuild_EmailOwnersAreUnverifiableAndKept(t *testing.T) {
	v := liveOrg("org/live")
	content := "/x/ docs@example.com @org/dead\n/y/ @org/live\n"
	tree := []string{"x/a.go", "y/b.go"}

	res, err := lint.Build([]byte(content), tree, v, lint.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "/x/ docs@example.com\n/y/ @org/live\n"; res.Plan.AfterContent != want {
		t.Errorf("after content = %q, want %q — the email owner must survive", res.Plan.AfterContent, want)
	}
	found := false
	for _, u := range res.Unverifiable {
		if strings.Contains(u, "docs@example.com") {
			found = true
		}
	}
	if !found {
		t.Errorf("Unverifiable = %v, want it to list docs@example.com (R-13)", res.Unverifiable)
	}
	if v.asked("docs@example.com") {
		t.Errorf("the API was asked about an email owner; calls = %v", v.calls)
	}
}

// SPEC R-16 (reporting): every action carries the 1-based line it happened on
// and a non-empty reason. The reason is not decoration — it is the only thing
// standing between a reviewer and approving a diff on trust, and the line
// number is what lets them find the edit in a file with a thousand rules.
func TestBuild_ActionsCarryLineAndReason(t *testing.T) {
	v := liveOrg("org/live")
	content := "/x/ @ org/live\n/y/ @org/live @org/dead\n"
	tree := []string{"x/a.go", "y/b.go"}

	res, err := lint.Build([]byte(content), tree, v, lint.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Actions) == 0 {
		t.Fatal("two edits were made and neither was reported")
	}
	for _, a := range res.Actions {
		if a.Line < 1 {
			t.Errorf("action %+v has line %d; line numbers are 1-based", a, a.Line)
		}
		if strings.TrimSpace(a.Reason) == "" {
			t.Errorf("action %+v has no reason — a reviewer cannot approve what nothing explains", a)
		}
	}

	repairs := actionsOfKind(res, lint.ActionRepairOwner)
	if len(repairs) != 1 || repairs[0].Line != 1 {
		t.Errorf("repair actions = %+v, want one on line 1", repairs)
	}
	removals := actionsOfKind(res, lint.ActionRemoveOwner)
	if len(removals) != 1 || removals[0].Line != 2 {
		t.Fatalf("remove actions = %+v, want one on line 2", removals)
	}
	if !strings.Contains(removals[0].Owner, "org/dead") {
		t.Errorf("remove action owner = %q, want it to name org/dead", removals[0].Owner)
	}
}

// SPEC (the gate, restated for lint): Build proves its own output. Whatever it
// synthesized, the after-bytes are re-parsed and re-resolved over the tree, so
// a plan that survives is one whose ownership claims hold against a fresh read
// of the file — never against the author's model of it. This test re-runs that
// resolution independently, which is the same check a `verify` run makes.
func TestBuild_AfterContentResolvesToTheReportedOwnership(t *testing.T) {
	v := liveOrg("org/all", "org/live")
	content := "* @org/all\n/x/ @ org/live\n/y/ @org/live @org/dead\n"
	tree := []string{"x/a.go", "y/b.go", "free.txt"}

	res, err := lint.Build([]byte(content), tree, v, lint.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	after := plan.ResolveContent(res.Plan.AfterContent, tree)
	for _, tc := range []struct {
		path string
		want []string
		note string
	}{
		{path: "x/a.go", want: []string{"@org/live"}, note: "repaired handle now governs it"},
		{path: "y/b.go", want: []string{"@org/live"}, note: "dead team dropped, live team kept"},
		{path: "free.txt", want: []string{"@org/all"}, note: "untouched by every stage"},
	} {
		if got := after[tc.path].Owners; !resolve.OwnersEqual(got, tc.want) {
			t.Errorf("%s owners = %v, want %v (%s)", tc.path, got, tc.want, tc.note)
		}
	}
	// Both edited lines move a path's owner set — the repair by making a skipped
	// line effective, the removal by dropping a name that resolved before — and
	// both are reported. free.txt is governed by a line nothing touched, so it
	// must not appear at all: an ownership row for it would mean an edit reached
	// outside the lines lint decided to change.
	rowFor(t, res.Plan, "x/a.go")
	rowFor(t, res.Plan, "y/b.go")
	for _, r := range res.Plan.Rows {
		if r.Path == "free.txt" {
			t.Errorf("untouched path reported as changed: %+v", r)
		}
	}
}

// SPEC R-0: lint computes, apply writes. The evidence is structural — Build
// takes CONTENT, not a path, and hands back a *plan.Plan carrying the pinned
// before-hash and the sizes that apply.Apply re-checks before it renames
// anything into place. Nothing in this package can reach a filesystem.
func TestBuild_ProducesAPlanNotAWrite(t *testing.T) {
	v := liveOrg("org/live")
	content := "/x/ @org/live @org/dead\n"

	res, err := lint.Build([]byte(content), []string{"x/a.go"}, v, lint.Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Plan.HashBefore == "" {
		t.Error("plan carries no before-hash; apply cannot detect a concurrent edit without it")
	}
	if res.Plan.SizeBefore != len(content) {
		t.Errorf("size before = %d, want %d", res.Plan.SizeBefore, len(content))
	}
	if res.Plan.SizeAfter != len(res.Plan.AfterContent) {
		t.Errorf("size after = %d, want %d", res.Plan.SizeAfter, len(res.Plan.AfterContent))
	}
}
