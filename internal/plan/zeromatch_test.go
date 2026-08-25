package plan_test

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
	"github.com/jordonpeterson/codeowners-tool/internal/resolve"
)

// These tests cover R-21 (on_zero_match: require | skip | declare), R-22
// (what `declare` actually writes) and INV-6 (a declare op is proven
// STRUCTURALLY, because there is no tracked file to prove it against).
//
// The whole safety argument for fleet automation lives here. A declare op is
// the one place the tool writes a rule it cannot check against the repo, and
// it is executed unattended across 100 repositories on a schedule — so every
// defect in it is multiplied by 100 and nobody is watching.

// zmOp is an op string plus the two things a policy file can say that `--op`
// cannot: which zero-match policy applies, and what to call this op in
// results and errors.
type zmOp struct {
	spec string // op string, same syntax as --op
	mode string // on_zero_match: "" | require | skip | declare
	id   string // policy label; "" when the op came from --op
}

// mkOps parses op strings and attaches the policy-file fields. ops.Parse
// never sets them (R-21: the zero value must preserve today's behavior), so
// a test that wants a zero-match policy must say so here.
func mkOps(t *testing.T, in ...zmOp) []ops.Op {
	t.Helper()
	out := make([]ops.Op, 0, len(in))
	for _, o := range in {
		parsed, err := ops.Parse(o.spec)
		if err != nil {
			t.Fatalf("op %q: %v", o.spec, err)
		}
		parsed.OnZeroMatch, parsed.ID = o.mode, o.id
		out = append(out, parsed)
	}
	return out
}

func buildZM(t *testing.T, content string, tree []string, opts plan.Options, in ...zmOp) (*plan.Plan, error) {
	t.Helper()
	return plan.Build([]byte(content), tree, mkOps(t, in...), opts)
}

func declareOp(spec string) zmOp { return zmOp{spec: spec, mode: ops.ZeroMatchDeclare} }
func skipZMOp(spec string) zmOp  { return zmOp{spec: spec, mode: ops.ZeroMatchSkip} }

// emittedLines splits planned content into physical lines, dropping the empty
// element a trailing newline produces.
func emittedLines(s string) []string {
	out := strings.Split(s, "\n")
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func lastEmittedLine(t *testing.T, s string) string {
	t.Helper()
	ls := emittedLines(s)
	if len(ls) == 0 {
		t.Fatalf("planned content is empty")
	}
	return ls[len(ls)-1]
}

// futureResolution resolves a path that does NOT exist in the repo against
// the planned content. It is the only way to observe what a declare op
// actually declared: the tracked tree, by definition, says nothing about it.
func futureResolution(content string, tree []string, path string) resolve.Resolution {
	full := append(append([]string{}, tree...), path)
	return plan.ResolveContent(content, full)[path]
}

func opResultByID(t *testing.T, p *plan.Plan, id string) plan.OpResult {
	t.Helper()
	for _, r := range p.OpResults {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no op result with id %q; results = %+v", id, p.OpResults)
	return plan.OpResult{}
}

// SPEC R-21 (compatibility): `require` and its zero value "" preserve R-5
// exactly — a scope matching zero tracked files is invalid input (exit 3).
//
// This is the guarantee that lets `on_zero_match` be added at all: ops parsed
// from `--op` never set the field, so every pre-existing test and every
// existing caller must keep the behavior TestT7_ZeroMatchScopeRejected pins.
// If this regresses, adding the field silently changed what one-repo `plan`
// does — the tool's oldest documented refusal.
func TestR21_RequireAndZeroValuePreserveR5(t *testing.T) {
	tree := []string{"real.txt"}
	for _, mode := range []string{"", ops.ZeroMatchRequire} {
		for _, spec := range []string{
			"add_owner(/ghost/, @b)",
			"set_owners(/ghost/, [@b])",
			"set_owners(/ghost/, [])",
			"remove_owner(/ghost/, @b)",
		} {
			t.Run(mode+"/"+spec, func(t *testing.T) {
				_, err := buildZM(t, "* @a\n", tree, plan.Options{}, zmOp{spec: spec, mode: mode})
				var inv *plan.InvalidError
				if !errors.As(err, &inv) {
					t.Fatalf("zero-match scope under on_zero_match=%q must stay InvalidError (exit 3), got %v", mode, err)
				}
				if !strings.Contains(err.Error(), "R-5") {
					t.Errorf("refusal must still cite R-5, got %q", err.Error())
				}
			})
		}
	}
}

// SPEC R-21 (`skip`): a skipped op changes nothing, is reported as skipped
// with a reason, and does not stop the rest of the batch from applying.
//
// This is the opportunistic-op case — "if this repo has Terraform, @org/infra
// owns it". If a skip silently aborted the batch, the baseline ops that DO
// apply here would never land; if it silently applied something, `skip` would
// mean nothing.
func TestR21_SkipIsANoOpForThatOpOnly(t *testing.T) {
	content := "# baseline\n* @org/everyone\n"
	tree := []string{"services/api/main.go", "README.md"}

	api := zmOp{spec: "add_owner(/services/api/, @org/api)", id: "api"}
	tf := zmOp{spec: "add_owner(/terraform/, @org/infra)", mode: ops.ZeroMatchSkip, id: "tf"}

	alone, err := buildZM(t, content, tree, plan.Options{}, api)
	if err != nil {
		t.Fatalf("reference plan (api op alone) must succeed: %v", err)
	}
	p, err := buildZM(t, content, tree, plan.Options{}, tf, api)
	if err != nil {
		t.Fatalf("a skipped op must not fail the batch: %v", err)
	}
	if p.AfterContent != alone.AfterContent {
		t.Errorf("skipped op changed bytes:\n got %q\nwant %q (identical to the batch without it)", p.AfterContent, alone.AfterContent)
	}
	for _, c := range p.Changes {
		if strings.Contains(c.Pattern, "terraform") {
			t.Errorf("skipped op emitted a change: %+v", c)
		}
	}
	if len(p.OpResults) != 2 {
		t.Fatalf("op_results = %+v, want one per op", p.OpResults)
	}
	got := opResultByID(t, p, "tf")
	if got.Status != "skipped" {
		t.Errorf("tf status = %q, want %q", got.Status, "skipped")
	}
	if got.Reason == "" {
		t.Error("a skipped op must carry a reason — without it the fleet operator cannot tell a skipped repo from a converged one")
	}
	if got.Proven != "" {
		t.Errorf("tf proven = %q, want empty: a skipped op proves nothing", got.Proven)
	}
	if got := opResultByID(t, p, "api"); got.Status != "applied" || got.Proven != "tree" {
		t.Errorf("api result = %+v, want applied/tree", got)
	}
}

// SPEC R-21 (`skip`): when EVERY op skips, the plan as a whole is a no-op
// (exit 1) — there is nothing to write.
//
// The planner must not emit a plan whose AfterContent equals its input:
// `apply` would then rewrite the file with identical bytes and the fleet
// would open a PR per repo containing no diff.
func TestR21_AllOpsSkippedIsAWholePlanNoOp(t *testing.T) {
	_, err := buildZM(t, "* @a\n", []string{"README.md"}, plan.Options{},
		skipZMOp("add_owner(/terraform/, @org/infra)"),
		skipZMOp("set_owners(/vendor/, [@org/vendor])"))
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("a batch in which every op skips must be a no-op (exit 1), got %v", err)
	}
}

// TRAP 1 — SPEC R-22 (INV-4 for declare): a declare op must be IDEMPOTENT.
//
// This is the highest-cost defect in the feature. The planner's only no-op
// detector is `bytes.Equal(afterBytes, content)`, and the desired-state map
// is keyed by TRACKED path — a declare op has none. So a declare resolves
// identically before and after, and nothing in the current design can notice
// that the line it is about to append is already the line at the bottom of
// the file.
//
// Unfixed, the failure is not subtle and not local: a scheduled fleet run
// appends the same rule to all 100 CODEOWNERS files, every run, forever. That
// is R-4's unbounded growth, once per repo per night, ending at S-4's 3 MB
// cap where GitHub stops loading the file and the repo silently has NO
// ownership at all. The second run must be a no-op (exit 1).
func TestR22_DeclareIsIdempotent(t *testing.T) {
	content := "# baseline\n* @org/everyone\n"
	tree := []string{"README.md", "src/main.go"}
	op := declareOp("add_owner(/.github/workflows/, @org/ci)")

	p, err := buildZM(t, content, tree, plan.Options{}, op)
	if err != nil {
		t.Fatalf("declare on a zero-match scope must plan: %v", err)
	}
	want := "# baseline\n* @org/everyone\n/.github/workflows/ @org/ci\n"
	if p.AfterContent != want {
		t.Fatalf("after = %q, want %q", p.AfterContent, want)
	}

	// Re-run the SAME op against the OUTPUT, three times: this is literally
	// what tomorrow's, and the next day's, scheduled run does.
	for run := 2; run <= 4; run++ {
		_, err := buildZM(t, p.AfterContent, tree, plan.Options{}, op)
		var noop *plan.NoOpError
		if !errors.As(err, &noop) {
			t.Fatalf("run %d of the same declare must be a no-op (exit 1), got %v — this is the unbounded-growth failure (R-4)", run, err)
		}
	}
	if n := strings.Count(p.AfterContent, "/.github/workflows/"); n != 1 {
		t.Errorf("declared pattern appears %d times, want exactly 1", n)
	}
}

// TRAP 1, second face — SPEC R-22: when the declared rule is already present
// but with DIFFERENT owners, the op must amend that rule in place, not append
// a second copy of the pattern.
//
// Appending instead would leave two rules for the same pattern; last-match
// wins, so `add_owner` would silently DROP the owners on the shadowed line
// (the exact R-4/R-7 failure the amend-preferred rule exists to prevent), and
// the file would gain a line on every ownership change forever.
func TestR22_DeclareAmendsAnExistingRuleWithDifferentOwners(t *testing.T) {
	tree := []string{"README.md"}
	cases := []struct {
		name, content, spec, want string
	}{
		{"add_owner joins the existing owners",
			"# baseline\n* @org/everyone\n/.github/workflows/ @org/old-ci\n",
			"add_owner(/.github/workflows/, @org/ci)",
			"# baseline\n* @org/everyone\n/.github/workflows/ @org/old-ci @org/ci\n"},
		{"set_owners replaces them",
			"# baseline\n* @org/everyone\n/.github/workflows/ @org/old-ci\n",
			"set_owners(/.github/workflows/, [@org/ci])",
			"# baseline\n* @org/everyone\n/.github/workflows/ @org/ci\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := buildZM(t, c.content, tree, plan.Options{}, declareOp(c.spec))
			if err != nil {
				t.Fatalf("declare over an existing rule must plan: %v", err)
			}
			if p.AfterContent != c.want {
				t.Errorf("after = %q, want %q", p.AfterContent, c.want)
			}
			if n := strings.Count(p.AfterContent, "/.github/workflows/"); n != 1 {
				t.Errorf("declared pattern appears %d times, want exactly 1 — a duplicate pattern shadows the other (R-7)", n)
			}
		})
	}
}

// TRAP 1, third face — SPEC R-22: "already declared" is a question about
// RESOLUTION, not about bytes. A rule written with a tab, extra spaces, or an
// inline comment already grants the declared owners, so re-declaring it is a
// no-op — and must not rewrite the line, which would churn a diff into 100
// pull requests that change only whitespace.
func TestR22_DeclareSeesAnExistingRuleThroughSpacing(t *testing.T) {
	tree := []string{"README.md"}
	content := "# baseline\n* @org/everyone\n/.github/workflows/\t  @org/ci   # owned by CI\n"
	for _, spec := range []string{
		"add_owner(/.github/workflows/, @org/ci)",
		"set_owners(/.github/workflows/, [@org/ci])",
	} {
		t.Run(spec, func(t *testing.T) {
			_, err := buildZM(t, content, tree, plan.Options{}, declareOp(spec))
			var noop *plan.NoOpError
			if !errors.As(err, &noop) {
				t.Fatalf("an already-satisfied declare must be a no-op (exit 1) regardless of spacing, got %v", err)
			}
		})
	}
}

// TRAP 2 — SPEC R-22: a declare op appends at EOF. It must NOT reuse
// synthAdd's unowned-path insert point, which inserts at firstRuleIndex,
// i.e. BEFORE every existing rule.
//
// CODEOWNERS is last-match-wins, so a rule written above a trailing
// `* @org/everyone` catch-all — the single most common line in real
// CODEOWNERS files — is shadowed for every path it was meant to govern. The
// tool would report "applied", the diff would look right in review, and the
// declared ownership would never take effect: 100 pull requests that do
// nothing, discovered months later when the first workflow file lands and
// pings the wrong team.
//
// The tracked tree cannot show any of this (the scope matches nothing), so
// the assertion resolves a path that does not exist yet against the emitted
// bytes.
func TestR22_DeclareAppendsAtEOFBelowACatchAll(t *testing.T) {
	content := "# CODEOWNERS\n/services/api/ @org/api\n* @org/everyone\n"
	tree := []string{"services/api/main.go", "README.md"}

	p, err := buildZM(t, content, tree, plan.Options{},
		declareOp("add_owner(/.github/workflows/, @org/ci)"))
	if err != nil {
		t.Fatalf("declare must plan: %v", err)
	}
	if got, want := lastEmittedLine(t, p.AfterContent), "/.github/workflows/ @org/ci"; got != want {
		t.Errorf("last line = %q, want %q — a declared rule written above the catch-all is dead on arrival", got, want)
	}
	// The point of appending at EOF: a matching file added LATER resolves to
	// the declared owners, not to the catch-all's.
	future := futureResolution(p.AfterContent, tree, ".github/workflows/ci.yml")
	if !reflect.DeepEqual(future.Owners, []string{"@org/ci"}) {
		t.Errorf("future .github/workflows/ci.yml = %v, want {@org/ci} — the declared rule is shadowed", future.Owners)
	}
	// INV-2 is unaffected: nothing tracked moves. Note both tracked paths
	// resolve to the catch-all BEFORE any op runs — the trailing `*` shadows
	// `/services/api/` under last-match-wins (S-1), which is the whole point of
	// the fixture. Asserting {@org/api} here would demand the declare op MOVE a
	// tracked path, i.e. the exact INV-2 violation this block exists to rule
	// out. (An earlier revision did assert that and was unsatisfiable by any
	// correct implementation.)
	before := plan.ResolveContent(content, tree)
	wantOwners(t, before, map[string][]string{
		"services/api/main.go": {"@org/everyone"},
		"README.md":            {"@org/everyone"},
	})
	after := plan.ResolveContent(p.AfterContent, tree)
	wantOwners(t, after, map[string][]string{
		"services/api/main.go": {"@org/everyone"},
		"README.md":            {"@org/everyone"},
	})
}

// TRAP 2, EOF edge: the file's final line has NO trailing newline. Appending
// must put the declared rule on its own line — concatenating onto the last
// line would rewrite an existing rule into something else entirely (pattern
// plus a stray owner list), which is a silent ownership change, not a
// declaration.
func TestR22_DeclareAppendsAtEOFWithoutATrailingNewline(t *testing.T) {
	content := "# CODEOWNERS\n* @org/everyone"
	tree := []string{"README.md"}

	p, err := buildZM(t, content, tree, plan.Options{},
		declareOp("add_owner(/.github/workflows/, @org/ci)"))
	if err != nil {
		t.Fatalf("declare must plan: %v", err)
	}
	want := "# CODEOWNERS\n* @org/everyone\n/.github/workflows/ @org/ci\n"
	if p.AfterContent != want {
		t.Errorf("after = %q, want %q", p.AfterContent, want)
	}
	if got := plan.ResolveContent(p.AfterContent, tree)["README.md"].Owners; !reflect.DeepEqual(got, []string{"@org/everyone"}) {
		t.Errorf("README.md = %v, want {@org/everyone} (INV-2)", got)
	}
}

// TRAP 2, EOF edge: a file containing only comments has no rules at all, so
// "after the last rule" and "before the first rule" are the same index. The
// declared rule still goes at the end, and every comment line stays
// byte-identical (INV-5) — a header explaining the file must not be pushed
// below the rules it introduces.
func TestR22_DeclareAppendsAtEOFInACommentOnlyFile(t *testing.T) {
	content := "# CODEOWNERS\n# nothing is owned yet — see the ownership policy\n"
	tree := []string{"README.md"}

	p, err := buildZM(t, content, tree, plan.Options{},
		declareOp("add_owner(/.github/workflows/, @org/ci)"))
	if err != nil {
		t.Fatalf("declare must plan: %v", err)
	}
	want := content + "/.github/workflows/ @org/ci\n"
	if p.AfterContent != want {
		t.Errorf("after = %q, want %q", p.AfterContent, want)
	}
	future := futureResolution(p.AfterContent, tree, ".github/workflows/ci.yml")
	if !reflect.DeepEqual(future.Owners, []string{"@org/ci"}) {
		t.Errorf("future .github/workflows/ci.yml = %v, want {@org/ci}", future.Owners)
	}
}

// TRAP 3 — SPEC INV-6: `proven` distinguishes an op checked against real
// files from one that could only be argued structurally.
//
// The gate iterates the TREE. For an op whose scope set is empty it iterates
// nothing, so INV-1 is vacuously true — the plan is "proven" without a single
// statement having been made about the line just written. That is precisely
// the case where a reviewer most needs to be told. `proven: "structural"` is
// the disclosure that a rule went in unverified against the repo; without it,
// a fleet operator reading 100 JSON records cannot tell a checked rollout
// from an unchecked one.
func TestINV6_ProvenIsStructuralOnlyForZeroMatchDeclares(t *testing.T) {
	content := "* @org/everyone\n"
	tree := []string{"services/api/main.go", "README.md"}

	p, err := buildZM(t, content, tree, plan.Options{},
		zmOp{spec: "add_owner(/services/api/, @org/api)", mode: ops.ZeroMatchDeclare, id: "api"},
		zmOp{spec: "add_owner(/.github/workflows/, @org/ci)", mode: ops.ZeroMatchDeclare, id: "ci"})
	if err != nil {
		t.Fatalf("declare batch must plan: %v", err)
	}
	if got := opResultByID(t, p, "api"); got.Status != "applied" || got.Proven != "tree" {
		t.Errorf("api result = %+v, want applied/tree — this op matched real files and IS provable over the tree", got)
	}
	if got := opResultByID(t, p, "ci"); got.Status != "applied" || got.Proven != "structural" {
		t.Errorf("ci result = %+v, want applied/structural — nothing tracked matched it (INV-6)", got)
	}
}

// TRAP 3 — SPEC INV-6: the structural proof must be a real proof. Since the
// gate's tree loop is vacuous for a zero-match op, the ONLY thing standing
// between the user and a wrong line is the structural check, and it must
// re-read the emitted bytes exactly as the gate does for tracked paths.
//
// Here the scope contains unescaped whitespace, so the line written for it —
// `docs x@y.zz @a` — re-parses as a DIFFERENT rule (pattern `docs`). With a
// tracked match this is caught today (TestGate_ProvesSerializedBytesNotModel).
// With zero matches nothing looks, and the tool would hand @a every `docs`
// directory in the repo while reporting that it declared ownership of a path
// with a space in it. Must refuse (exit 2).
func TestINV6_RefusesADeclareWhoseEmittedLineDoesNotRoundTrip(t *testing.T) {
	tree := []string{"README", "docs/guide.md"}
	op := ops.Op{Kind: ops.AddOwner, Scope: "docs x@y.zz", Owners: []string{"@a"},
		Raw: "add_owner(docs x@y.zz, @a)", OnZeroMatch: ops.ZeroMatchDeclare, ID: "bad"}

	_, err := plan.Build([]byte("README @keep\n"), tree, []ops.Op{op}, plan.Options{})
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("a declared line that re-parses as a different rule must be refused (exit 2), got %v", err)
	}
}

// TRAP 3 — SPEC INV-6 / INV-2: declare weakens INV-1 and NOTHING else. INV-2
// is still proven over the whole tree exactly as today.
//
// This is the promise the feature is sold on ("a pattern matching nothing
// tracked cannot move any existing path's resolution"). If a declare op could
// move even one tracked path, the fleet rollout would be silently
// reassigning ownership in 100 repositories under cover of "declaring" paths
// that do not exist.
func TestINV6_DeclareDoesNotWeakenINV2(t *testing.T) {
	content := "# ownership\n* @org/everyone\n/services/ @org/svc\n/services/api/ @org/api\n*.md @org/docs\n"
	tree := []string{
		"README.md", "src/util.go",
		"services/api/main.go", "services/api/README.md", "services/web/app.js",
	}
	before := plan.ResolveContent(content, tree)

	p, err := buildZM(t, content, tree, plan.Options{},
		declareOp("add_owner(/terraform/, @org/infra)"))
	if err != nil {
		t.Fatalf("declare must plan: %v", err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	for _, path := range tree {
		b, a := before[path], after[path]
		if b.Matched != a.Matched || !resolve.OwnersEqual(b.Owners, a.Owners) {
			t.Errorf("INV-2: %s changed from %v (matched=%v) to %v (matched=%v)",
				path, b.Owners, b.Matched, a.Owners, a.Matched)
		}
	}
	if len(p.Rows) != 0 {
		t.Errorf("ownership rows = %+v, want none: a declare op moves no tracked path", p.Rows)
	}
	if len(p.Changes) != 1 || p.Changes[0].Action != "insert" || p.Changes[0].Pattern != "/terraform/" {
		t.Errorf("changes = %+v, want exactly one insert of /terraform/", p.Changes)
	}
}

// TRAP 4 — SPEC R-22 (R-8 for declare ops): the batch commutativity check
// intersects TREE path sets. Two zero-match scopes intersect on nothing, so
// an order-dependent batch of declare ops sails through the R-8 check as
// "commuting" and is written in input order.
//
// The pair below is genuinely contradictory: for a future `terraform/main.tf`
// the first op asks for @org/infra among the owners and the second asks for
// exactly {@org/tf}. Last-match-wins means whichever line is written second
// decides — the two orderings produce different CODEOWNERS files and both are
// accepted today. The refusal must cite R-8; a refusal citing R-5 means
// `declare` was never implemented and this test is passing for the wrong
// reason.
func TestR22_ZeroMatchBatchIsNotVacuouslyCommuting(t *testing.T) {
	cases := []struct {
		name string
		tree []string
		in   []zmOp
	}{
		{"two declare ops on overlapping future paths",
			[]string{"README.md"},
			[]zmOp{
				declareOp("add_owner(/terraform/, @org/infra)"),
				declareOp("set_owners(/terraform/*.tf, [@org/tf])"),
			}},
		{"declare op vs. an op that applies here",
			[]string{"README.md", "src/util.go"},
			[]zmOp{
				declareOp("set_owners(**/*.tf, [@org/infra])"),
				{spec: "set_owners(/src/, [@org/team])"},
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildZM(t, "# CODEOWNERS\n", c.tree, plan.Options{}, c.in...)
			var inv *plan.InvalidError
			if !errors.As(err, &inv) {
				t.Fatalf("order-dependent batch must be invalid input (R-8), got %v", err)
			}
			if !strings.Contains(err.Error(), "R-8") {
				t.Fatalf("refusal must cite R-8 (order dependence), got %q", err.Error())
			}
		})
	}
}

// TRAP 4, the other half — SPEC R-22: the order-dependence guard must not
// become "reject any batch containing a declare op". A fleet policy is mostly
// declare ops; a guard that refused all of them would make the feature
// unusable and push operators back to hand-editing 100 files.
func TestR22_CommutingDeclareBatchIsAccepted(t *testing.T) {
	tree := []string{"README.md"}

	t.Run("disjoint scopes", func(t *testing.T) {
		p, err := buildZM(t, "* @org/everyone\n", tree, plan.Options{},
			declareOp("add_owner(/terraform/, @org/infra)"),
			declareOp("add_owner(/.github/workflows/, @org/ci)"))
		if err != nil {
			t.Fatalf("two declare ops on disjoint scopes must be accepted: %v", err)
		}
		if got := futureResolution(p.AfterContent, tree, "terraform/main.tf").Owners; !reflect.DeepEqual(got, []string{"@org/infra"}) {
			t.Errorf("future terraform/main.tf = %v, want {@org/infra}", got)
		}
		if got := futureResolution(p.AfterContent, tree, ".github/workflows/ci.yml").Owners; !reflect.DeepEqual(got, []string{"@org/ci"}) {
			t.Errorf("future .github/workflows/ci.yml = %v, want {@org/ci}", got)
		}
	})

	// Two adds on the SAME scope commute as owner-set operations, so they must
	// be accepted — but they cannot be written as two stacked lines: the second
	// would shadow the first and the future path would resolve to one owner,
	// not both.
	t.Run("same scope, both owners survive", func(t *testing.T) {
		p, err := buildZM(t, "* @org/everyone\n", tree, plan.Options{},
			declareOp("add_owner(/terraform/, @org/infra)"),
			declareOp("add_owner(/terraform/, @org/sre)"))
		if err != nil {
			t.Fatalf("two commuting add_owner declares must be accepted: %v", err)
		}
		got := append([]string{}, futureResolution(p.AfterContent, tree, "terraform/main.tf").Owners...)
		sort.Strings(got)
		if !reflect.DeepEqual(got, []string{"@org/infra", "@org/sre"}) {
			t.Errorf("future terraform/main.tf = %v, want {@org/infra, @org/sre} — a second stacked rule shadows the first", got)
		}
	})
}

// SPEC R-22: `declare` on set_owners, including the empty owner list. An
// empty list is a legal, deliberate un-owning (S-9): the rule keeps its
// pattern with zero owners, so a future matching file is explicitly NOT
// governed by whatever broader rule sits above it — the only way to say "this
// path is deliberately unowned" for paths that do not exist yet.
func TestR22_DeclareSetOwners(t *testing.T) {
	content := "# CODEOWNERS\n* @org/everyone\n"
	tree := []string{"README.md"}

	t.Run("explicit owner set", func(t *testing.T) {
		p, err := buildZM(t, content, tree, plan.Options{},
			declareOp("set_owners(/terraform/, [@org/infra, @org/sre])"))
		if err != nil {
			t.Fatalf("declare set_owners must plan: %v", err)
		}
		if got, want := lastEmittedLine(t, p.AfterContent), "/terraform/ @org/infra @org/sre"; got != want {
			t.Errorf("last line = %q, want %q", got, want)
		}
		got := append([]string{}, futureResolution(p.AfterContent, tree, "terraform/main.tf").Owners...)
		sort.Strings(got)
		if !reflect.DeepEqual(got, []string{"@org/infra", "@org/sre"}) {
			t.Errorf("future terraform/main.tf = %v, want exactly {@org/infra, @org/sre}", got)
		}
	})

	t.Run("empty owner set is a zero-owner rule (S-9)", func(t *testing.T) {
		p, err := buildZM(t, content, tree, plan.Options{},
			declareOp("set_owners(/generated/, [])"))
		if err != nil {
			t.Fatalf("declare set_owners with an empty list must plan: %v", err)
		}
		if got, want := lastEmittedLine(t, p.AfterContent), "/generated/"; got != want {
			t.Errorf("last line = %q, want %q — a pattern with no owners", got, want)
		}
		r := futureResolution(p.AfterContent, tree, "generated/api.pb.go")
		if !r.Matched || len(r.Owners) != 0 {
			t.Errorf("future generated/api.pb.go = %+v, want matched with zero owners — not a fallthrough to the catch-all", r)
		}
	})
}

// SPEC R-22: a declare op whose scope matches SOME tracked files is not a
// zero-match at all. It takes the ordinary path — same edits, same bytes,
// proven over the tree.
//
// `on_zero_match` describes what to do when the scope matches NOTHING. If
// setting it to `declare` changed how an op that does match is written, then
// adding the field to a policy would quietly alter the behavior of every op
// in it, and reviewers of the policy file would be reading the wrong thing.
func TestR22_DeclareWithMatchesIsTheOrdinaryPath(t *testing.T) {
	content := "* @org/everyone\n/services/ @org/svc\n"
	tree := []string{"services/api/main.go", "services/web/app.js", "README.md"}
	spec := "add_owner(/services/api/, @org/api)"

	def, err := buildZM(t, content, tree, plan.Options{}, zmOp{spec: spec, id: "api"})
	if err != nil {
		t.Fatalf("default-mode plan must succeed: %v", err)
	}
	dec, err := buildZM(t, content, tree, plan.Options{}, zmOp{spec: spec, mode: ops.ZeroMatchDeclare, id: "api"})
	if err != nil {
		t.Fatalf("declare-mode plan must succeed: %v", err)
	}
	if dec.AfterContent != def.AfterContent {
		t.Errorf("declare changed the bytes of an op that matches files:\n got %q\nwant %q", dec.AfterContent, def.AfterContent)
	}
	if !reflect.DeepEqual(dec.Changes, def.Changes) {
		t.Errorf("declare changed the edits of an op that matches files:\n got %+v\nwant %+v", dec.Changes, def.Changes)
	}
	if got := opResultByID(t, dec, "api"); got.Status != "applied" || got.Proven != "tree" {
		t.Errorf("api result = %+v, want applied/tree (INV-6 applies only to zero-match ops)", got)
	}
}

// SPEC R-22 (R-24): OpResults is one entry per op, in op order, carrying the
// op's id and text.
//
// The fleet record is the only artifact of an unattended run. "Which repos
// lack Terraform" is answerable only if each op reports itself, by id, in the
// order the policy lists them — a bare count cannot answer it, and a reordered
// list attributes the wrong outcome to the wrong op.
func TestZeroMatch_OpResultsAreInOpOrder(t *testing.T) {
	content := "* @org/everyone\n"
	tree := []string{"services/api/main.go", "docs/guide.md", "README.md"}
	in := []zmOp{
		{spec: "add_owner(/services/api/, @org/api)", id: "api"},
		{spec: "add_owner(/terraform/, @org/infra)", mode: ops.ZeroMatchSkip, id: "tf"},
		{spec: "add_owner(/.github/workflows/, @org/ci)", mode: ops.ZeroMatchDeclare, id: "ci"},
		{spec: "add_owner(/docs/, @org/everyone)", id: "docs"},
	}
	parsed := mkOps(t, in...)
	p, err := plan.Build([]byte(content), tree, parsed, plan.Options{})
	if err != nil {
		t.Fatalf("mixed batch must plan: %v", err)
	}
	want := []struct{ id, status string }{
		{"api", "applied"},
		{"tf", "skipped"},
		{"ci", "applied"},
		{"docs", "unchanged"}, // @org/everyone already owns /docs/ via the catch-all
	}
	if len(p.OpResults) != len(want) {
		t.Fatalf("op_results = %+v, want %d entries in op order", p.OpResults, len(want))
	}
	for i, w := range want {
		got := p.OpResults[i]
		if got.ID != w.id || got.Status != w.status {
			t.Errorf("op_results[%d] = %+v, want id %q status %q", i, got, w.id, w.status)
		}
		if got.Op != parsed[i].Raw {
			t.Errorf("op_results[%d].Op = %q, want the op text %q", i, got.Op, parsed[i].Raw)
		}
	}
}

// SPEC R-22: several declare ops stack at EOF in POLICY order.
//
// Order is not cosmetic in a last-match-wins file: the order the lines land
// in is the order that decides overlaps for every file added later. It must
// be the order the policy author wrote and a reviewer read — not map order,
// which would differ run to run and make two repos with identical inputs
// produce different files.
func TestR22_MultipleDeclaresStackAtEOFInPolicyOrder(t *testing.T) {
	content := "# CODEOWNERS\n* @org/everyone\n"
	tree := []string{"README.md"}

	p, err := buildZM(t, content, tree, plan.Options{},
		declareOp("add_owner(/.github/workflows/, @org/ci)"),
		declareOp("add_owner(/terraform/, @org/infra)"),
		declareOp("add_owner(/docs/, @org/docs)"))
	if err != nil {
		t.Fatalf("declare batch must plan: %v", err)
	}
	want := "# CODEOWNERS\n* @org/everyone\n" +
		"/.github/workflows/ @org/ci\n/terraform/ @org/infra\n/docs/ @org/docs\n"
	if p.AfterContent != want {
		t.Errorf("after = %q, want %q", p.AfterContent, want)
	}
}

// SPEC R-22: a declare op whose rule would be byte-identical to a rule
// already in the file.
//
// Two cases with opposite answers, and the difference is precedence, not
// bytes. When the identical rule is already the last word, there is nothing
// to do (exit 1). When a later catch-all shadows it, the declaration is NOT
// satisfied — reporting "already correct" there is the silent no-op rollout
// that `skip` vs `declare` exists to distinguish, and it would read as 100
// converged repos in the fleet summary.
func TestZeroMatch_DeclareByteIdenticalToAnExistingRule(t *testing.T) {
	tree := []string{"README.md"}
	spec := "add_owner(/terraform/, @org/infra)"

	t.Run("identical rule is already last", func(t *testing.T) {
		_, err := buildZM(t, "* @org/everyone\n/terraform/ @org/infra\n", tree,
			plan.Options{}, declareOp(spec))
		var noop *plan.NoOpError
		if !errors.As(err, &noop) {
			t.Fatalf("an already-declared rule must be a no-op (exit 1), got %v", err)
		}
	})

	t.Run("identical rule is shadowed by a later catch-all", func(t *testing.T) {
		p, err := buildZM(t, "/terraform/ @org/infra\n* @org/everyone\n", tree,
			plan.Options{}, declareOp(spec))
		var noop *plan.NoOpError
		var inv *plan.InvalidError
		switch {
		case errors.As(err, &noop):
			t.Fatalf("a shadowed declaration is not satisfied; reporting a no-op hides a rollout that did nothing")
		case errors.As(err, &inv):
			t.Fatalf("a shadowed declaration is not invalid input, got %v", err)
		case err != nil:
			// A refusal (exit 2) is an acceptable answer: the only fix is a
			// duplicate pattern, which R-7 treats as a defect to report.
		default:
			got := futureResolution(p.AfterContent, tree, "terraform/main.tf").Owners
			if !reflect.DeepEqual(got, []string{"@org/infra"}) {
				t.Errorf("future terraform/main.tf = %v, want {@org/infra} — the declared rule is still shadowed", got)
			}
		}
	})
}

// SPEC R-22 (S-4): the size cap applies to declared lines too. A rule for
// files that do not exist yet is still bytes GitHub has to load, and past
// 3 MB GitHub silently ignores the whole file — every path in the repo loses
// its owners at once. A declare op is exactly the kind of op a fleet script
// runs on a schedule, so it is the likeliest way to cross the line.
func TestR22_DeclareRespectsTheSizeCap(t *testing.T) {
	_, err := buildZM(t, "* @a\n", []string{"README.md"},
		plan.Options{MaxSize: 20},
		declareOp("add_owner(/.github/workflows/, @org/ci)"))
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("a declare pushing the file over the size cap must be refused (exit 2), got %v", err)
	}
}

// SPEC INV-6 / R-8 (regression, adversarial audit of Wave 1): the disjointness
// proof must not treat "/src**" as the directory "/src/".
//
// anchoredDirPrefix stripped the trailing "**" BEFORE testing for wildcards, so
// "/src**" normalized to "/src/" and patternsProvablyDisjoint("/src**",
// "/srcx/") answered TRUE. It is false: "/src**" compiles to
// `\Asrc[^/]*[^/]*(?:/.*)?\z` and matches srcx/a.go, which "/srcx/" matches too.
// A false disjointness proof is the one failure mode this package is built to
// exclude — it is a WRONG WRITE, not a missed one, and both reproducers below
// were accepted at exit 0 before the fix.
func TestINV6_TrailingStarStarIsNotADirectoryPrefix(t *testing.T) {
	// Reproducer (a): the declaration is amended into a rule that a later
	// "/src**" recaptures entirely, so a future srcx/a.go resolves to
	// @org/legacy — while the tool reports the op applied and proven=structural.
	t.Run("amend under a later /src** is refused", func(t *testing.T) {
		content := "/srcx/  @org/old\n/src**  @org/legacy\n"
		tree := []string{"src/main.go"}
		p, err := buildZM(t, content, tree, plan.Options{},
			declareOp("add_owner(/srcx/, @org/new)"))
		var ref *plan.RefusalError
		if !errors.As(err, &ref) {
			after := "<no plan>"
			if p != nil {
				after = p.AfterContent
			}
			t.Fatalf("declaring /srcx/ under a later /src** must be refused (exit 2): "+
				"/src** captures the whole declared scope, so future srcx/a.go resolves to @org/legacy. got err=%v, after=%q",
				err, after)
		}
	})

	// Reproducer (b): R-8's zero-match guard skipped the pair as "provably
	// disjoint", so BOTH orderings were accepted and a future srcx/a.go resolved
	// to whichever declare was listed second.
	t.Run("order-dependent declare batch is refused in both orders", func(t *testing.T) {
		srcx := declareOp("set_owners(/srcx/, [@org/a])")
		starstar := declareOp("set_owners(/src**, [@org/b])")
		for _, c := range []struct {
			name string
			in   []zmOp
		}{
			{"/srcx/ first", []zmOp{srcx, starstar}},
			{"/src** first", []zmOp{starstar, srcx}},
		} {
			t.Run(c.name, func(t *testing.T) {
				p, err := buildZM(t, "# empty\n", []string{"README.md"}, plan.Options{}, c.in...)
				var inv *plan.InvalidError
				if !errors.As(err, &inv) {
					after := "<no plan>"
					if p != nil {
						after = p.AfterContent
					}
					t.Fatalf("both declares can govern srcx/a.go and set_owners does not commute — "+
						"the batch is order-dependent and must be refused (R-8). got err=%v, after=%q", err, after)
				}
				if !strings.Contains(err.Error(), "R-8") {
					t.Errorf("refusal must cite R-8 (order dependence), got %q", err.Error())
				}
			})
		}
	})
}

// SPEC INV-6 (third obligation): a scope this same policy declares LATER may
// overlap an earlier declared scope PARTIALLY — allowed, and disclosed.
//
// The canonical fleet baseline is "CI owns workflows everywhere, infra owns
// Terraform where it exists". Those scopes meet on .github/workflows/deploy.tf,
// and CODEOWNERS gives a path exactly one owner set, so one of them must lose
// there. Refusing the pair made that baseline inexpressible in EVERY repo,
// forever, and reported a policy-level conflict as an identical per-repo exit 2
// on all 100 repos — the misclassification the exit-2/exit-3 split exists to
// prevent. The author wrote both in one document; its order is the precedence,
// exactly as in a hand-written file. R-7 sets the precedent: disclose the
// shadowing, do not refuse it.
//
// The plan suite never covered this before — TestR22_MultipleDeclaresStackAtEOF
// InPolicyOrder and TestR22_CommutingDeclareBatchIsAccepted both stack
// anchored, wildcard-free scopes, which are provably disjoint — so the
// overlapping-but-satisfiable case stayed invisible until the CLI wired it up.
func TestINV6_PartialOverlapBetweenSameBatchDeclaresIsAllowedAndDisclosed(t *testing.T) {
	tree := []string{"README.md"}
	p, err := buildZM(t, "* @org/base\n", tree, plan.Options{},
		declareOp("add_owner(/.github/workflows/, @org/ci)"),
		declareOp("add_owner(**/*.tf, @org/infra)"),
		declareOp("add_owner(/charts/, @org/k8s)"))
	if err != nil {
		t.Fatalf("partially overlapping declares from one policy must be accepted: %v", err)
	}

	want := "* @org/base\n/.github/workflows/ @org/ci\n**/*.tf @org/infra\n/charts/ @org/k8s\n"
	if p.AfterContent != want {
		t.Fatalf("after = %q, want %q — declares stack at EOF in policy order", p.AfterContent, want)
	}

	// Every scope still governs something: none of these declarations is dead.
	for _, c := range []struct {
		path string
		want []string
	}{
		{".github/workflows/ci.yml", []string{"@org/ci"}},
		{"src/main.tf", []string{"@org/infra"}},
		{"charts/values.yaml", []string{"@org/k8s"}},
		// The intersections, decided by policy order: the later rule wins.
		{".github/workflows/deploy.tf", []string{"@org/infra"}},
		{"charts/main.tf", []string{"@org/k8s"}},
	} {
		if got := futureResolution(p.AfterContent, tree, c.path).Owners; !reflect.DeepEqual(got, c.want) {
			t.Errorf("future %s = %v, want %v", c.path, got, c.want)
		}
	}

	// The disclosure is the whole price of the relaxation: silently writing an
	// overlap a reviewer cannot see in the diff is what R-7 refuses to do.
	found := ""
	for _, w := range p.Warnings {
		if strings.Contains(w, `"/.github/workflows/"`) && strings.Contains(w, `"**/*.tf"`) {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("overlapping declares must warn, naming both scopes; warnings = %#v", p.Warnings)
	}
	if !strings.Contains(found, "wins on every path both match") {
		t.Errorf("the warning must say which scope wins on the intersection, got %q", found)
	}
	if !strings.Contains(found, "INV-6") {
		t.Errorf("the warning must cite the rule it relaxes (INV-6), got %q", found)
	}
}

// SPEC INV-6 (third obligation): TOTAL capture by a same-batch declare is still
// a refusal. The relaxation above is only for overlaps that leave the declared
// rule the last word SOMEWHERE. A rule no path can ever reach is dead on
// arrival: the tool would report it applied with proven=structural, the diff
// would look right in review, and the declaration would never take effect —
// which is the entire failure mode this file exists to prevent.
func TestINV6_TotalShadowingBetweenSameBatchDeclaresIsRefused(t *testing.T) {
	p, err := buildZM(t, "* @org/base\n", []string{"README.md"}, plan.Options{},
		declareOp("add_owner(/src/api/, @org/api)"),
		declareOp("add_owner(/src/, @org/app)"))
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		after := "<no plan>"
		if p != nil {
			after = p.AfterContent
		}
		t.Fatalf("/src/ captures /src/api/ entirely, so the /src/api/ declaration can never win for any path "+
			"and must be refused (INV-6). got err=%v, after=%q", err, after)
	}
	if !strings.Contains(err.Error(), "INV-6") {
		t.Errorf("refusal must cite INV-6, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "/src/api/") || !strings.Contains(err.Error(), "/src/") {
		t.Errorf("refusal must name both scopes so the author can reorder the policy, got %q", err.Error())
	}
}

// SPEC INV-6 / R-1: a partial overlap with a PRE-EXISTING later rule is still a
// refusal. Same geometry as the allowed case above — "/.github/workflows/"
// against a later "**/*.tf" — and the opposite answer, because the difference is
// authority, not shape: R-1 forbids reordering lines this run did not write, so
// unlike a same-policy overlap there is no order the planner is entitled to
// choose. Accepting it would let the tool silently ratify whatever precedence
// the existing file happens to encode.
func TestINV6_PartialOverlapWithAPreexistingLaterRuleIsStillRefused(t *testing.T) {
	content := "/.github/workflows/ @org/ci\n**/*.tf @org/legacy\n"
	p, err := buildZM(t, content, []string{"README.md"}, plan.Options{},
		declareOp("add_owner(/.github/workflows/, @org/ci2)"))
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		after := "<no plan>"
		if p != nil {
			after = p.AfterContent
		}
		t.Fatalf("the overlapping **/*.tf is not this policy's line to reorder (R-1), so the declare must be refused: "+
			"got err=%v, after=%q", err, after)
	}
	if !strings.Contains(err.Error(), "/.github/workflows/") {
		t.Errorf("refusal must name the declared scope, got %q", err.Error())
	}
}

// Regression guard from the pre-release review: an UNRECOGNIZED on_zero_match
// value on a zero-match scope must refuse (exit 3), naming the value. Policy
// parsing validates the enum, but the field is exported with a json tag, so a
// library caller — or a value the policy layer learns before the planner does
// — can carry anything. Before the fix no switch arm matched, the op
// synthesized nothing, and the run reported the repo converged with a proven
// tree: the silent no-op rollout this file exists to prevent.
func TestZeroMatch_UnrecognizedOnZeroMatchIsInvalid(t *testing.T) {
	tree := []string{"README.md"}
	op := ops.Op{Kind: ops.AddOwner, Scope: "/ghost/", Owners: []string{"@b"},
		Raw: "add_owner(/ghost/, @b)", OnZeroMatch: "frob", ID: "bad"}

	_, err := plan.Build([]byte("* @a\n"), tree, []ops.Op{op}, plan.Options{})
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("unrecognized on_zero_match on a zero-match scope must be invalid input (exit 3), got %v", err)
	}
	if !strings.Contains(err.Error(), `"frob"`) {
		t.Errorf("refusal must name the unrecognized value, got %q", err.Error())
	}
}

// Same defense for the sibling switch: an unrecognized on_except_zero_match on
// an op whose except bites nothing must be invalid input (exit 3) naming the
// value — not silently run as `require`, whose exit-2 refusal reads as a
// per-repo problem when the defect is in the policy and identical everywhere.
func TestZeroMatch_UnrecognizedOnExceptZeroMatchIsInvalid(t *testing.T) {
	tree := []string{"src/main.go", "README.md"}
	op := ops.Op{Kind: ops.AddOwner, Scope: "/src/", Owners: []string{"@b"},
		Raw: "add_owner(/src/, @b)", Except: []string{"/src/ghost/"},
		OnExceptZeroMatch: "frob", ID: "bad"}

	_, err := plan.Build([]byte("* @a\n"), tree, []ops.Op{op}, plan.Options{})
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("unrecognized on_except_zero_match must be invalid input (exit 3), got %v", err)
	}
	if !strings.Contains(err.Error(), `"frob"`) {
		t.Errorf("refusal must name the unrecognized value, got %q", err.Error())
	}
}
