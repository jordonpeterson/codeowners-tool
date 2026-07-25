// Package plan_test encodes the spec's acceptance tests for Engine A.
//
// The planner takes intent-level ops, computes the resolved ownership
// before/after over the real tree, synthesizes line edits, and GATES the
// result on two invariants:
//
//	INV-1: every path in scope resolves to exactly what the op requires.
//	INV-2: every path outside scope resolves to exactly what it did before.
//
// A plan that cannot be proven is refused (exit 2), never guessed at.
package plan_test

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jordonpropm/codeowners-tool/internal/ops"
	"github.com/jordonpropm/codeowners-tool/internal/plan"
)

func mustOps(t *testing.T, specs ...string) []ops.Op {
	t.Helper()
	parsed, err := ops.ParseAll(specs)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func build(t *testing.T, content string, tree []string, opts plan.Options, specs ...string) (*plan.Plan, error) {
	t.Helper()
	return plan.Build([]byte(content), tree, mustOps(t, specs...), opts)
}

// SPEC T-1 (S-1, the core case): add_owner must RETAIN pre-existing owners.
// A result of {@b} alone is the primary failure mode this tool exists to
// prevent — a naive append would replace @a, not join it.
func TestT1_AddOwnerRetainsExistingOwners(t *testing.T) {
	tree := []string{"x/foo.go", "x/deep/bar.go", "y/other.txt"}
	p, err := build(t, "/x/ @a\n", tree, plan.Options{}, "add_owner(/x/, @b)")
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	for _, path := range []string{"x/foo.go", "x/deep/bar.go"} {
		got := after[path].Owners
		sort.Strings(got)
		if !reflect.DeepEqual(got, []string{"@a", "@b"}) {
			t.Errorf("%s owners = %v, want exactly {@a, @b}", path, got)
		}
	}
	if after["y/other.txt"].Matched {
		t.Error("y/other.txt must remain unmatched (INV-2)")
	}
}

// SPEC T-2 (R-2): add_owner must handle EVERY rule whose match set intersects
// the scope, not just the closest one. One appended line satisfies neither
// invariant when a later narrower rule exists.
func TestT2_AddOwnerHandlesAllIntersectingRules(t *testing.T) {
	tree := []string{"x/foo.go", "x/README.md"}
	p, err := build(t, "/x/ @a\n/x/*.md @docs\n", tree, plan.Options{}, "add_owner(/x/, @b)")
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	got := after["x/foo.go"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@a", "@b"}) {
		t.Errorf("x/foo.go = %v, want {@a, @b}", got)
	}
	got = after["x/README.md"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@b", "@docs"}) {
		t.Errorf("x/README.md = %v, want {@docs, @b}", got)
	}
}

// SPEC T-3 (R-3): set_owners must prove no later rule recaptures any path in
// scope. Inserting `/x/ @b` ABOVE `*.tf @infra` would silently lose every
// .tf file under x/ to @infra.
func TestT3_SetOwnersNoLaterRecapture(t *testing.T) {
	tree := []string{"x/main.tf", "x/app.go", "other.tf"}
	p, err := build(t, "/x/ @a\n*.tf @infra\n", tree, plan.Options{}, "set_owners(/x/, [@b])")
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	if got := after["x/main.tf"].Owners; !reflect.DeepEqual(got, []string{"@b"}) {
		t.Errorf("x/main.tf = %v, want {@b} — recaptured by a later rule (R-3 violation)", got)
	}
	if got := after["x/app.go"].Owners; !reflect.DeepEqual(got, []string{"@b"}) {
		t.Errorf("x/app.go = %v, want {@b}", got)
	}
	// INV-2: other.tf stays with @infra.
	if got := after["other.tf"].Owners; !reflect.DeepEqual(got, []string{"@infra"}) {
		t.Errorf("other.tf = %v, want {@infra} (INV-2)", got)
	}
}

// SPEC T-5 (INV-4): applying the same op twice produces byte-identical
// output — the second plan is a no-op (exit 1), not further growth.
func TestT5_Idempotence(t *testing.T) {
	tree := []string{"x/a.go", "x/b.md"}
	content := "/x/ @a\n/x/*.md @docs\n"
	p1, err := build(t, content, tree, plan.Options{}, "add_owner(/x/, @b)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = build(t, p1.AfterContent, tree, plan.Options{}, "add_owner(/x/, @b)")
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("second identical op must be a no-op (exit 1), got err=%v", err)
	}
}

// SPEC T-6 (INV-5): untouched lines — comments, blanks, spacing, trailing
// whitespace — are byte-identical after an op.
func TestT6_UntouchedLinesByteIdentical(t *testing.T) {
	content := "# Header comment\n\n/x/\t@a   # inline\n   \n/y/ @z  \n"
	tree := []string{"x/a.go", "y/b.go"}
	p, err := build(t, content, tree, plan.Options{}, "add_owner(/y/, @new)")
	if err != nil {
		t.Fatal(err)
	}
	wantUntouched := []string{"# Header comment", "", "/x/\t@a   # inline", "   "}
	gotLines := strings.Split(p.AfterContent, "\n")
	for i, want := range wantUntouched {
		if gotLines[i] != want {
			t.Errorf("line %d changed: %q -> %q", i, want, gotLines[i])
		}
	}
}

// SPEC T-7 (R-5): a scope matching zero tracked files is invalid input
// (exit 3). Dead rules are how CODEOWNERS files rot; the tool must not
// create them.
func TestT7_ZeroMatchScopeRejected(t *testing.T) {
	_, err := build(t, "* @a\n", []string{"real.txt"}, plan.Options{}, "add_owner(/ghost/, @b)")
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("zero-match scope must be InvalidError (exit 3), got %v", err)
	}
}

// SPEC T-10 (R-9, S-4): refuse to produce a file over 3 MB — GitHub would
// silently not load it, which is total ownership failure.
func TestT10_SizeCapRefusal(t *testing.T) {
	var sb strings.Builder
	line := "/x/ @aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"
	for sb.Len() < 3*1024*1024-len(line) {
		sb.WriteString("# padding comment to approach the size limit xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n")
	}
	sb.WriteString(line)
	tree := []string{"x/a.go"}
	_, err := build(t, sb.String(), tree, plan.Options{}, "add_owner(/x/, @bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb)")
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("pushing past 3MB must be refused (exit 2), got %v", err)
	}
}

// SPEC R-4: prefer amending an existing exact-match line over inserting.
// Without this, repeated automated runs grow the file without bound.
func TestR4_AmendPreferredOverInsert(t *testing.T) {
	tree := []string{"x/a.go"}
	p, err := build(t, "/x/ @a\n", tree, plan.Options{}, "add_owner(/x/, @b)")
	if err != nil {
		t.Fatal(err)
	}
	if p.AfterContent != "/x/ @a @b\n" {
		t.Errorf("want amended single line, got %q", p.AfterContent)
	}
	for _, c := range p.Changes {
		if c.Action == "insert" {
			t.Errorf("expected amend, got insert: %+v", c)
		}
	}
}

// SPEC R-6: remove_owner emptying an owner set requires an explicit policy.
func TestR6_EmptyPolicies(t *testing.T) {
	tree := []string{"x/a.go", "y/b.go"}
	content := "* @default\n/x/ @solo\n"

	// Unspecified policy: invalid input (no default exists, by design).
	_, err := build(t, content, tree, plan.Options{}, "remove_owner(/x/, @solo)")
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("unspecified on-empty policy must be invalid input, got %v", err)
	}

	// error: refuse.
	_, err = build(t, content, tree, plan.Options{OnEmpty: "error"}, "remove_owner(/x/, @solo)")
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("on-empty=error must refuse, got %v", err)
	}

	// unowned: rule keeps its pattern with zero owners (S-9).
	p, err := build(t, content, tree, plan.Options{OnEmpty: "unowned"}, "remove_owner(/x/, @solo)")
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	r := after["x/a.go"]
	if !r.Matched || len(r.Owners) != 0 {
		t.Errorf("unowned policy: want matched+empty, got %+v", r)
	}

	// inherit: rule deleted; the broader rule takes over. The resulting
	// ownership change is IN scope and must appear in the plan rows.
	p, err = build(t, content, tree, plan.Options{OnEmpty: "inherit"}, "remove_owner(/x/, @solo)")
	if err != nil {
		t.Fatal(err)
	}
	after = plan.ResolveContent(p.AfterContent, tree)
	if got := after["x/a.go"].Owners; !reflect.DeepEqual(got, []string{"@default"}) {
		t.Errorf("inherit policy: x/a.go = %v, want {@default}", got)
	}
	found := false
	for _, row := range p.Rows {
		if row.Path == "x/a.go" && reflect.DeepEqual(row.After, []string{"@default"}) {
			found = true
		}
	}
	if !found {
		t.Errorf("inherit fallthrough must appear as an ownership row; rows=%+v", p.Rows)
	}
}

// SPEC R-7: with duplicate patterns, edit the EFFECTIVE (last) one and
// report the shadowed duplicate — never silently fix it.
func TestR7_DuplicatePatternsEditEffective(t *testing.T) {
	tree := []string{"x/a.go"}
	p, err := build(t, "/x/ @old\n/x/ @a\n", tree, plan.Options{}, "add_owner(/x/, @b)")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(p.AfterContent, "\n")
	if lines[0] != "/x/ @old" {
		t.Errorf("shadowed duplicate must be untouched, got %q", lines[0])
	}
	if lines[1] != "/x/ @a @b" {
		t.Errorf("effective rule must be amended, got %q", lines[1])
	}
	if len(p.Warnings) == 0 || !strings.Contains(strings.Join(p.Warnings, " "), "shadow") {
		t.Errorf("shadowed duplicate must be reported, warnings=%v", p.Warnings)
	}
}

// SPEC R-8: order-dependent overlapping batches are rejected, not resolved
// by input order. Commuting batches are fine.
func TestR8_ConflictingBatchRejected(t *testing.T) {
	tree := []string{"x/a.go"}
	_, err := build(t, "/x/ @a\n", tree, plan.Options{},
		"set_owners(/x/, [@b])", "set_owners(/x/, [@c])")
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("order-dependent batch must be invalid input, got %v", err)
	}

	// Two add_owners commute: allowed.
	p, err := build(t, "/x/ @a\n", tree, plan.Options{},
		"add_owner(/x/, @b)", "add_owner(/x/, @c)")
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	got := after["x/a.go"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@a", "@b", "@c"}) {
		t.Errorf("commuting adds: got %v", got)
	}
}

// rename_owner replaces the identifier everywhere; it cannot change any
// rule's match set (§4.1), so it is exempt from scope machinery.
func TestRenameOwner_Global(t *testing.T) {
	tree := []string{"x/a.go", "y/b.go"}
	p, err := build(t, "/x/ @old-team\n/y/ @old-team @keep\n", tree, plan.Options{},
		"rename_owner(@old-team, @org/new-team)")
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	if got := after["x/a.go"].Owners; !reflect.DeepEqual(got, []string{"@org/new-team"}) {
		t.Errorf("x = %v", got)
	}
	got := after["y/b.go"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@keep", "@org/new-team"}) {
		t.Errorf("y = %v", got)
	}
	// Renaming an absent owner is a no-op (exit 1).
	_, err = build(t, "/x/ @a\n", []string{"x/a.go"}, plan.Options{}, "rename_owner(@ghost, @new)")
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("rename of absent owner must be no-op, got %v", err)
	}
}

// add_owner over a scope that intersects an UNANCHORED broad rule whose match
// set extends outside the scope: the planner must synthesize an intersection
// pattern, and the out-of-scope paths governed by that rule stay untouched.
func TestR2_SplitBroadRuleAcrossScopeBoundary(t *testing.T) {
	tree := []string{"x/app.go", "x/infra/main.tf", "elsewhere/other.tf"}
	p, err := build(t, "/x/ @a\n*.tf @infra\n", tree, plan.Options{}, "add_owner(/x/, @b)")
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	got := after["x/infra/main.tf"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@b", "@infra"}) {
		t.Errorf("x/infra/main.tf = %v, want {@infra, @b}", got)
	}
	if got := after["elsewhere/other.tf"].Owners; !reflect.DeepEqual(got, []string{"@infra"}) {
		t.Errorf("elsewhere/other.tf = %v, want {@infra} — INV-2", got)
	}
}

// A scope nested INSIDE a broader anchored rule: the broad rule governs
// paths outside the scope, so it cannot be amended — the scope itself is the
// sound narrowing pattern, inserted after the broad rule. (Found by smoke
// testing; the classic monorepo shape: /services/ owned broadly, one service
// gains a co-owner.)
func TestR2_ScopeNestedInsideBroaderAnchoredRule(t *testing.T) {
	tree := []string{"services/api/main.go", "services/web/app.js", "README.md"}
	p, err := build(t, "* @platform\n/services/ @services\n", tree, plan.Options{},
		"add_owner(/services/api/, @api-team)")
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	got := after["services/api/main.go"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@api-team", "@services"}) {
		t.Errorf("services/api/main.go = %v, want {@services, @api-team}", got)
	}
	if got := after["services/web/app.js"].Owners; !reflect.DeepEqual(got, []string{"@services"}) {
		t.Errorf("services/web/app.js = %v, want {@services} (INV-2)", got)
	}
	if got := after["README.md"].Owners; !reflect.DeepEqual(got, []string{"@platform"}) {
		t.Errorf("README.md = %v (INV-2)", got)
	}
}

// When intent cannot be expressed without breaking an invariant, the planner
// REFUSES (exit 2). It never emits a best-effort plan.
func TestGate_RefusesInexpressibleIntent(t *testing.T) {
	// Scope is an UNANCHORED directory glob whose paths only partially match
	// the intersecting broad rule: the scope is not a subset of the rule, the
	// scope is not anchored, and the rule extends outside the scope — no
	// sound narrowing pattern is derivable.
	tree := []string{"a/x/doc.md", "a/x/code.go", "other/doc.md"}
	_, err := build(t, "*.md @docs\n", tree, plan.Options{}, "add_owner(x/, @b)")
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("inexpressible intent must be refused (exit 2), got %v", err)
	}
}

// add_owner covering previously-UNOWNED paths: a scope rule is inserted
// where later rules keep precedence, so owned paths in scope are not
// double-captured.
func TestAddOwner_CoversUnownedPaths(t *testing.T) {
	tree := []string{"x/free.go", "x/owned.md"}
	p, err := build(t, "/x/*.md @docs\n", tree, plan.Options{}, "add_owner(/x/, @b)")
	if err != nil {
		t.Fatal(err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	if got := after["x/free.go"].Owners; !reflect.DeepEqual(got, []string{"@b"}) {
		t.Errorf("x/free.go = %v, want {@b}", got)
	}
	got := after["x/owned.md"].Owners
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"@b", "@docs"}) {
		t.Errorf("x/owned.md = %v, want {@docs, @b}", got)
	}
}

// SPEC R-16: the plan carries both views — ownership rows AND the literal
// line diff — plus byte sizes and per-change reasons.
func TestR16_PlanIsMachineReadable(t *testing.T) {
	tree := []string{"x/a.go"}
	p, err := build(t, "/x/ @a\n", tree, plan.Options{}, "add_owner(/x/, @b)")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rows) != 1 || p.Rows[0].Path != "x/a.go" {
		t.Errorf("rows = %+v", p.Rows)
	}
	if !reflect.DeepEqual(p.Rows[0].Before, []string{"@a"}) {
		t.Errorf("before = %v", p.Rows[0].Before)
	}
	if p.SizeBefore == 0 || p.SizeAfter <= p.SizeBefore {
		t.Errorf("sizes: before=%d after=%d", p.SizeBefore, p.SizeAfter)
	}
	if !strings.Contains(p.Diff, "-/x/ @a") || !strings.Contains(p.Diff, "+/x/ @a @b") {
		t.Errorf("diff missing literal lines:\n%s", p.Diff)
	}
	for _, c := range p.Changes {
		if c.Reason == "" {
			t.Errorf("change without reason: %+v", c)
		}
	}
	if p.HashBefore == "" {
		t.Error("plan must pin the input file hash for apply-time drift detection")
	}
}

// The gate proves invariants on the RE-PARSED serialized bytes, not the
// in-memory model (review finding: a scope with unescaped whitespace modeled
// as one rule serializes to a line that re-parses as a DIFFERENT valid rule,
// which the in-memory gate could not see). Ops built via Parse are already
// rejected; a hand-constructed Op must be caught by the gate itself.
func TestGate_ProvesSerializedBytesNotModel(t *testing.T) {
	tree := []string{"docs x@y.zz/f.go", "docs/free.txt", "README"}
	op := ops.Op{Kind: ops.AddOwner, Scope: "docs\\ x@y.zz", Owner: "@a",
		Raw: `add_owner(docs\ x@y.zz, @a)`}
	// Escaped form: works end-to-end and survives re-parse.
	p, err := plan.Build([]byte("README @keep\n"), tree, []ops.Op{op}, plan.Options{})
	if err != nil {
		t.Fatalf("escaped-space scope must plan cleanly: %v", err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	if got := after["docs x@y.zz/f.go"].Owners; len(got) != 1 || got[0] != "@a" {
		t.Errorf("in-scope path = %v, want [@a]", got)
	}
	if after["docs/free.txt"].Matched {
		t.Error("out-of-scope path must stay unmatched (INV-2)")
	}
	// Raw unescaped form (bypassing ops.Parse): the serialization-aware gate
	// must refuse — never emit a plan whose bytes mean something else.
	bad := ops.Op{Kind: ops.AddOwner, Scope: "docs x@y.zz", Owner: "@a",
		Raw: "add_owner(docs x@y.zz, @a)"}
	_, err = plan.Build([]byte("README @keep\n"), tree, []ops.Op{bad}, plan.Options{})
	var ref *plan.RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("non-round-tripping scope must be refused by the gate, got %v", err)
	}
}

// Review finding: under --on-empty=inherit, a single remove_owner may delete
// a narrow rule AND amend its broader fallthrough rule in the same pass. The
// per-pass desired snapshot went stale and refused a perfectly expressible
// op. Must now succeed with both edits.
func TestR6_InheritDeleteThenAmendFallthroughSamePass(t *testing.T) {
	tree := []string{"a/f.go", "a/b/g.go"}
	p, err := build(t, "/a/ @x @y\n/a/b/ @x\n", tree, plan.Options{OnEmpty: "inherit"},
		"remove_owner(/a/, @x)")
	if err != nil {
		t.Fatalf("expressible inherit removal must not be refused: %v", err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	for _, path := range tree {
		if got := after[path].Owners; !reflect.DeepEqual(got, []string{"@y"}) {
			t.Errorf("%s = %v, want {@y}", path, got)
		}
	}
}

// Review finding: simulate() cannot model inherit (inheritance resurrects
// owners from rules the owner-set transform cannot see), so an overlapping
// batch containing remove_owner under --on-empty=inherit was accepted while
// being order-dependent. R-8 now rejects it outright.
func TestR8_InheritOverlapRejected(t *testing.T) {
	tree := []string{"a/b/f.go", "a/g.go", "top.txt"}
	_, err := build(t, "* @root\n/a/ @x\n", tree, plan.Options{OnEmpty: "inherit"},
		"add_owner(/a/b/, @z)", "remove_owner(/a/, @x)")
	var inv *plan.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("overlapping batch under inherit must be invalid input (R-8), got %v", err)
	}
}

// Second-review finding: a set_owners whose scope ALREADY resolves to
// exactly the requested set must be a no-op (exit 1) — not an exit-0 plan
// inserting a redundant rule.
func TestSetOwners_SemanticNoOp(t *testing.T) {
	tree := []string{"dir/f.go", "top.txt"}
	_, err := build(t, "* @a\n", tree, plan.Options{}, "set_owners(/dir/, [@a])")
	var noop *plan.NoOpError
	if !errors.As(err, &noop) {
		t.Fatalf("semantically satisfied set_owners must be a no-op, got %v", err)
	}
}

// Second-review finding (regression in the first fix round): remove_owner's
// post-fixpoint settling accepted ANY divergence from the pure transform as
// long as the removed owner was absent — under non-inherit policies no rule
// is deleted, so divergence means a bug, and accepting it would launder an
// earlier bad edit past the gate. This end-to-end test pins the safe path;
// the white-box divergence case lives in settle_internal_test.go.
func TestR6_UnownedPolicyPureTransform(t *testing.T) {
	tree := []string{"a /f.go"}
	p, err := build(t, "/a\\  @x\n", tree, plan.Options{OnEmpty: "unowned"},
		`remove_owner(/a\ , @x)`)
	if err != nil {
		t.Fatalf("unowned removal on escaped-space pattern must succeed: %v", err)
	}
	after := plan.ResolveContent(p.AfterContent, tree)
	r := after["a /f.go"]
	if !r.Matched || len(r.Owners) != 0 {
		t.Errorf("want matched with zero owners, got %+v", r)
	}
}
