package plan_test

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// These tests cover R-40 (on_unowned: assign | skip): an add_owner that leaves
// open paths open. The motivating fleet: repos where only build.gradle has an
// owner, where a blanket grant on /.github/ would turn files any developer
// could approve into files only the new owner can — R-40 makes "co-own what is
// already owned, own nothing new" a statable intent.
//
// "Open" is any path with no owner today: unmatched, or matched by a rule
// listing zero owners (S-9). Both leave GitHub's code-owner review requirement
// inert, which is the property the policy exists to preserve.

// uoOp is an op string plus the policy-file fields these tests exercise.
type uoOp struct {
	spec    string // op string, same syntax as --op
	unowned string // on_unowned: "" | assign | skip
	zero    string // on_zero_match: "" | require | skip | declare
	id      string
}

func mkUO(t *testing.T, in ...uoOp) []ops.Op {
	t.Helper()
	out := make([]ops.Op, 0, len(in))
	for _, o := range in {
		parsed, err := ops.Parse(o.spec)
		if err != nil {
			t.Fatalf("op %q: %v", o.spec, err)
		}
		parsed.OnUnowned, parsed.OnZeroMatch, parsed.ID = o.unowned, o.zero, o.id
		out = append(out, parsed)
	}
	return out
}

func buildUO(t *testing.T, content string, tree []string, opts plan.Options, in ...uoOp) (*plan.Plan, error) {
	t.Helper()
	return plan.Build([]byte(content), tree, mkUO(t, in...), opts)
}

func skipUO(spec string) uoOp { return uoOp{spec: spec, unowned: ops.UnownedSkip} }

// SPEC R-40 (the core case): with on_unowned=skip, owned paths in scope gain
// the co-owner and open paths stay open — no rule is inserted for them, so a
// file every developer could approve yesterday is a file every developer can
// approve tomorrow.
func TestR40_MixedScopeGrantsOwnedAndLeavesOpenPathsOpen(t *testing.T) {
	tree := []string{"x/build.gradle", "x/app.py", "x/README.md", "y/other.txt"}
	p, err := buildUO(t, "/x/build.gradle @org/gradle\n", tree, plan.Options{},
		skipUO("add_owner(/x/, @org/platform)"))
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	got := after["x/build.gradle"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@org/gradle", "@org/platform"}) {
		t.Errorf("x/build.gradle owners = %v, want {@org/gradle, @org/platform}", got)
	}
	for _, open := range []string{"x/app.py", "x/README.md", "y/other.txt"} {
		if after[open].Matched {
			t.Errorf("%s must stay unmatched (R-40: open paths stay open)", open)
		}
	}
	// No scope-wide rule may appear: the whole point is that "/x/" is never
	// written down as a line that would capture the open paths.
	if strings.Contains(p.AfterContent, "/x/ ") {
		t.Errorf("a rule for the raw scope was inserted:\n%s", p.AfterContent)
	}
	if len(p.OpResults) != 1 || p.OpResults[0].Status != "applied" {
		t.Fatalf("OpResults = %+v, want one applied op", p.OpResults)
	}
	wantOpen := []string{"x/README.md", "x/app.py"}
	if !reflect.DeepEqual(p.OpResults[0].LeftOpen, wantOpen) {
		t.Errorf("LeftOpen = %v, want %v (sorted, in-scope only)", p.OpResults[0].LeftOpen, wantOpen)
	}
	// Ownership rows list exactly the granted path — the open paths did not
	// change owners and must not appear.
	if len(p.Rows) != 1 || p.Rows[0].Path != "x/build.gradle" {
		t.Errorf("Rows = %+v, want exactly x/build.gradle", p.Rows)
	}
}

// SPEC R-40: a scope whose every tracked file is open skips — status
// "skipped" with a reason naming on_unowned, nothing written. This is the
// fleet outcome: a repo that owns nothing under /.github/ is left exactly as
// it was, at exit 1's nothing-to-change.
func TestR40_AllOpenScopeSkips(t *testing.T) {
	tree := []string{"x/a.py", "x/b.py", "docs/d.md"}
	p, err := buildUO(t, "/docs/ @org/docs\n", tree, plan.Options{},
		uoOp{spec: "add_owner(/x/, @org/platform)", unowned: ops.UnownedSkip, id: "gh"})
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("want NoOpError (nothing to change), got %v", err)
	}
	if p == nil {
		t.Fatal("a skipped run must still return a populated plan (R-24)")
	}
	if p.AfterContent != "/docs/ @org/docs\n" {
		t.Errorf("file must be byte-identical, got:\n%s", p.AfterContent)
	}
	r := p.OpResults[0]
	if r.Status != "skipped" {
		t.Errorf("status = %q, want skipped", r.Status)
	}
	if !strings.Contains(r.Reason, "on_unowned") {
		t.Errorf("a skipped op must carry a reason naming on_unowned, got %q", r.Reason)
	}
	if r.Proven != "" {
		t.Errorf("a skipped op proves nothing, got proven=%q", r.Proven)
	}
	wantOpen := []string{"x/a.py", "x/b.py"}
	if !reflect.DeepEqual(r.LeftOpen, wantOpen) {
		t.Errorf("LeftOpen = %v, want %v", r.LeftOpen, wantOpen)
	}
}

// SPEC R-40/S-9: a path matched by a zero-owner rule is as open as an
// unmatched one — the rule states "nobody owns this", and on_unowned=skip must
// not overwrite that statement. The zero-owner line survives byte-for-byte.
func TestR40_ExplicitZeroOwnerRuleCountsAsOpen(t *testing.T) {
	tree := []string{"x/vendor/v.go", "x/src/m.go"}
	p, err := buildUO(t, "/x/vendor/\n/x/src/ @a\n", tree, plan.Options{},
		skipUO("add_owner(/x/, @p)"))
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	if own := after["x/vendor/v.go"].Owners; len(own) != 0 {
		t.Errorf("x/vendor/v.go owners = %v, want the deliberate zero (S-9)", own)
	}
	got := after["x/src/m.go"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@a", "@p"}) {
		t.Errorf("x/src/m.go owners = %v, want {@a, @p}", got)
	}
	if !strings.Contains(p.AfterContent, "/x/vendor/\n") {
		t.Errorf("the zero-owner line must survive unchanged:\n%s", p.AfterContent)
	}
	if !reflect.DeepEqual(p.OpResults[0].LeftOpen, []string{"x/vendor/v.go"}) {
		t.Errorf("LeftOpen = %v, want the zero-owned path", p.OpResults[0].LeftOpen)
	}
}

// SPEC R-40 (compatibility): the zero value changes nothing. An op with no
// on_unowned grants open paths exactly as it always has — the scope rule is
// inserted before all existing rules and the open path gains the owner.
func TestR40_ZeroValuePreservesGrantToOpenPaths(t *testing.T) {
	tree := []string{"x/build.gradle", "x/app.py"}
	p, err := build(t, "/x/build.gradle @g\n", tree, plan.Options{}, "add_owner(/x/, @p)")
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	if !reflect.DeepEqual(after["x/app.py"].Owners, []string{"@p"}) {
		t.Errorf("x/app.py owners = %v, want {@p} (pre-R-40 behavior)", after["x/app.py"].Owners)
	}
	if p.OpResults[0].LeftOpen != nil {
		t.Errorf("LeftOpen = %v, want absent on a default-behavior op", p.OpResults[0].LeftOpen)
	}
}

// SPEC R-40/R-19: a second run of the same policy over the produced file is a
// no-op, byte-identical. The owned path now carries the owner (nothing to
// add), the open paths are still open (still skipped) — idempotence is what
// makes the op safe on a nightly schedule.
func TestR40_RepeatRunIsByteIdentical(t *testing.T) {
	tree := []string{"x/build.gradle", "x/app.py"}
	p1, err := buildUO(t, "/x/build.gradle @g\n", tree, plan.Options{},
		skipUO("add_owner(/x/, @p)"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := buildUO(t, p1.AfterContent, tree, plan.Options{},
		skipUO("add_owner(/x/, @p)"))
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("re-run must be a no-op, got %v", err)
	}
	if p2.AfterContent != p1.AfterContent {
		t.Errorf("re-run is not byte-identical:\n%q\nvs\n%q", p2.AfterContent, p1.AfterContent)
	}
	if s := p2.OpResults[0].Status; s != "unchanged" {
		t.Errorf("re-run status = %q, want unchanged", s)
	}
}

// SPEC R-40: batch semantics are decided against the BEFORE state, so a
// sibling grant in the same batch does not feed paths into a skip-unowned
// op's scope. The batch is deterministic in either order (add ∘ add commutes,
// R-8), and the record shows which op declined the path.
func TestR40_ScopeIsDecidedAgainstBeforeBatchState(t *testing.T) {
	tree := []string{"x/f.go"}
	orders := [][]uoOp{
		{{spec: "add_owner(/x/, @a)"}, skipUO("add_owner(/x/, @b)")},
		{skipUO("add_owner(/x/, @b)"), {spec: "add_owner(/x/, @a)"}},
	}
	for _, batch := range orders {
		p, err := buildUO(t, "", tree, plan.Options{}, batch...)
		if err != nil {
			t.Fatalf("batch %v: %v", batch, err)
		}
		after := plan.ResolveContent(p.AfterContent, tree)
		if !reflect.DeepEqual(after["x/f.go"].Owners, []string{"@a"}) {
			t.Errorf("x/f.go owners = %v, want {@a} only: x/f.go was open before the batch, so the skip-unowned op never grants there", after["x/f.go"].Owners)
		}
	}
}

// SPEC R-40/R-2: the narrowing machinery still works over the restricted
// scope. A rule that also governs out-of-scope paths gets a narrowing insert
// for the owned in-scope paths; out-of-scope resolution is untouched.
func TestR40_NarrowingInsertStillWorksOverRestrictedScope(t *testing.T) {
	tree := []string{"x/a.go", "x/b.md", "y/c.go"}
	p, err := buildUO(t, "*.go @go\n", tree, plan.Options{},
		skipUO("add_owner(/x/, @p)"))
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	got := after["x/a.go"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@go", "@p"}) {
		t.Errorf("x/a.go owners = %v, want {@go, @p}", got)
	}
	if !reflect.DeepEqual(after["y/c.go"].Owners, []string{"@go"}) {
		t.Errorf("y/c.go owners = %v, want {@go} untouched (INV-2)", after["y/c.go"].Owners)
	}
	if after["x/b.md"].Matched {
		t.Error("x/b.md must stay unmatched (R-40)")
	}
}

// SPEC R-40/R-26: except and on_unowned compose — both subtract from the
// effective scope, for different reasons, and the record reports each under
// its own field. An excepted path is reported as excepted, never as left
// open: except is the stronger statement (deliberately out of scope whatever
// its state), and one path must not appear in two lists.
func TestR40_ExceptAndOnUnownedCompose(t *testing.T) {
	tree := []string{"x/owned.go", "x/open.md", "x/gen/g.go"}
	p, err := buildUO(t, "/x/owned.go @a\n/x/gen/g.go @gen\n", tree, plan.Options{},
		skipUO("add_owner(/x/ except /x/gen/, @p)"))
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	got := after["x/owned.go"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@a", "@p"}) {
		t.Errorf("x/owned.go owners = %v, want {@a, @p}", got)
	}
	if !reflect.DeepEqual(after["x/gen/g.go"].Owners, []string{"@gen"}) {
		t.Errorf("x/gen/g.go owners = %v, want {@gen} untouched (R-26)", after["x/gen/g.go"].Owners)
	}
	if after["x/open.md"].Matched {
		t.Error("x/open.md must stay unmatched")
	}
	r := p.OpResults[0]
	if !reflect.DeepEqual(r.LeftOpen, []string{"x/open.md"}) {
		t.Errorf("LeftOpen = %v, want exactly x/open.md (the excepted path is not left open, it is excepted)", r.LeftOpen)
	}
	if len(r.Excepted) != 1 || r.Excepted[0].Path != "x/gen/g.go" {
		t.Errorf("Excepted = %+v, want exactly x/gen/g.go", r.Excepted)
	}
}

// SPEC R-29 (pinned against R-40): the unconditional refusal for an excepted
// path that matches no rule survives on_unowned=skip — WHEN the op still
// writes. The refusal is deliberately not deferred to synthesis details, and
// skip-unowned is a synthesis-independent scope restriction, not a license to
// capture a path no carve can restore. Only an op the restriction empties
// outright escapes it, by writing nothing at all (see
// TestR40_EmptiedOpDoesNotConsultExceptZeroMatch).
func TestR40_UnmatchedExceptedPathStillRefusesWhenOpWrites(t *testing.T) {
	tree := []string{"x/owned.go", "x/gen/g.go"}
	_, err := buildUO(t, "/x/owned.go @a\n", tree, plan.Options{},
		skipUO("add_owner(/x/ except /x/gen/, @p)"))
	var ref *plan.RefusalError
	if !errors.As(err, &ref) || !strings.Contains(err.Error(), "R-29") {
		t.Fatalf("want the R-29 refusal (exit 2), got %v", err)
	}
}

// SPEC R-40/R-21: the two skips answer different questions and compose. A
// repo without the scope at all skips on zero-match; a repo with the scope
// but nothing owned in it skips on on_unowned; a repo with owned paths
// applies. One policy, three repos, three correct records.
func TestR40_ComposesWithOnZeroMatchSkip(t *testing.T) {
	op := uoOp{spec: "add_owner(/x/, @p)", unowned: ops.UnownedSkip, zero: ops.ZeroMatchSkip}

	// No /x/ in the tree: zero-match skip, and the reason says so.
	p, err := buildUO(t, "/docs/ @d\n", []string{"docs/d.md"}, plan.Options{}, op)
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("zero-match repo: want NoOpError, got %v", err)
	}
	if r := p.OpResults[0]; r.Status != "skipped" || !strings.Contains(r.Reason, "on_zero_match") {
		t.Errorf("zero-match repo: result = %+v, want skipped with a zero-match reason", r)
	}

	// /x/ exists, nothing owned: on_unowned skip, and the reason says THAT.
	p, err = buildUO(t, "/docs/ @d\n", []string{"docs/d.md", "x/a.go"}, plan.Options{}, op)
	if !errors.As(err, &noop) {
		t.Fatalf("all-open repo: want NoOpError, got %v", err)
	}
	if r := p.OpResults[0]; r.Status != "skipped" || !strings.Contains(r.Reason, "on_unowned") {
		t.Errorf("all-open repo: result = %+v, want skipped with an on_unowned reason", r)
	}

	// /x/ exists and is owned: applies.
	p, err = buildUO(t, "/x/ @a\n", []string{"x/a.go"}, plan.Options{}, op)
	if err != nil {
		t.Fatalf("owned repo: %v", err)
	}
	if r := p.OpResults[0]; r.Status != "applied" {
		t.Errorf("owned repo: status = %q, want applied", r.Status)
	}
}

// SPEC R-40 (defense in depth): the struct is exported, so Build must refuse
// what the policy validator refuses — an on_unowned on a verb that cannot
// carry it, an unknown value, and skip alongside declare — rather than
// silently running the default under a spelling that says otherwise.
func TestR40_BuildRefusesIllegalOnUnowned(t *testing.T) {
	tree := []string{"x/a.go"}
	cases := []struct {
		name string
		op   uoOp
		want []string
	}{
		{"on set_owners", uoOp{spec: "set_owners(/x/, [@a])", unowned: ops.UnownedSkip}, []string{"on_unowned", "add_owner"}},
		{"on remove_owner", uoOp{spec: "remove_owner(/x/, @a)", unowned: ops.UnownedSkip}, []string{"on_unowned", "add_owner"}},
		{"on rename_owner", uoOp{spec: "rename_owner(@a, @b)", unowned: ops.UnownedSkip}, []string{"on_unowned", "add_owner"}},
		{"unknown value", uoOp{spec: "add_owner(/x/, @p)", unowned: "SKIP"}, []string{"on_unowned", "assign", "skip"}},
		{"skip with declare", uoOp{spec: "add_owner(/z/, @p)", unowned: ops.UnownedSkip, zero: ops.ZeroMatchDeclare}, []string{"on_unowned", "declare"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildUO(t, "/x/ @a\n", tree, plan.Options{OnEmpty: "error"}, tc.op)
			var inv *plan.InvalidError
			if !errors.As(err, &inv) {
				t.Fatalf("want InvalidError (exit 3), got %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error must mention %q:\n%v", w, err)
				}
			}
		})
	}
}

// SPEC R-40/R-8: a skip-unowned add batched with a displacing set_owners is
// order-dependent exactly where they share an OWNED path — a per-repo fact
// (the same repo with /x/ fully open has no overlap and the batch is fine),
// so the refusal is the tree-based exit 2, not the static exit 3.
func TestR40_R8OverlapIsDecidedOverTheRestrictedScope(t *testing.T) {
	batch := []uoOp{
		{spec: "set_owners(*, [@org/everyone])"},
		skipUO("add_owner(/x/, @p)"),
	}

	// Owned overlap: refused.
	_, err := buildUO(t, "/x/ @a\n", []string{"x/a.go", "y/b.go"}, plan.Options{}, batch...)
	var inv *plan.InvalidError
	if !errors.As(err, &inv) || !strings.Contains(err.Error(), "R-8") {
		t.Fatalf("owned overlap: want R-8 refusal, got %v", err)
	}

	// Same batch, /x/ fully open: the add skips, no overlap, the batch plans.
	p, err := buildUO(t, "/docs/ @d\n", []string{"x/a.go", "docs/d.md"}, plan.Options{}, batch...)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	if s := p.OpResults[1].Status; s != "skipped" {
		t.Errorf("open repo: add status = %q, want skipped", s)
	}
}

// SPEC R-40: on_except_zero_match stays ordered AFTER the unowned question.
// An op emptied by the unowned restriction writes nothing, so an except
// pattern matching zero tracked files must not refuse the repo — an op that
// writes nothing can reopen nothing (R-28's own ordering, applied to R-40).
func TestR40_EmptiedOpDoesNotConsultExceptZeroMatch(t *testing.T) {
	// /x/gen/ matches nothing; every /x/ path is open. Under require the
	// except would refuse — but the op skips first.
	tree := []string{"x/a.go"}
	p, err := buildUO(t, "/docs/ @d\n", append(tree, "docs/d.md"), plan.Options{},
		skipUO("add_owner(/x/ except /x/gen/, @p)"))
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("want NoOpError (skipped, not refused on the except), got %v", err)
	}
	if s := p.OpResults[0].Status; s != "skipped" {
		t.Errorf("status = %q, want skipped", s)
	}
}
