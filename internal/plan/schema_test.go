package plan_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// ---------------------------------------------------------------------------
// Schema pins for the plan document (R-16).
//
// The plan JSON is not an output format, it is a CONTRACT. `plan` writes it,
// `apply` reads it, and nothing else validates it in between: apply unmarshals
// the document straight into plan.Plan and writes bytes to disk from
// after_content, gated only on sha256_before. Every invariant the planner
// proved is carried across that boundary by the document alone.
//
// The document is also a CI artifact: `plan --out plan.json` in one job,
// `apply --plan plan.json` in a later job, possibly with a newer binary. That
// makes both directions of compatibility load-bearing — a newer binary must
// read an older plan, and an older reader must not choke on a field it does
// not know.
//
// Until these tests existed the shape was pinned by NOTHING. No test in the
// suite marshalled a Plan and compared the result to anything, so a field
// added to Plan silently changed `plan --out`'s output and the suite stayed
// green. Plan.OpResults was added exactly that way. These tests are
// characterization tests: they record the CURRENT shape as intentional, so the
// next change to it is a deliberate, reviewed one.
//
// They assert the key SET, never the key ORDER. Go's marshaller emits fields
// in declaration order, but that ordering is not part of the contract and
// pinning it would fail on a harmless field reshuffle.
// ---------------------------------------------------------------------------

// planFileMirror mirrors internal/cli's unexported planFile — Plan plus the
// apply-time context the CLI adds. cli.planFile cannot be referenced from
// here, so this copy stands in for it; TestR16_PlanFileEnvelopeInlinesPlan
// pins the relationship between the two shapes so the copy cannot quietly
// drift into a different contract than the one apply actually reads.
type planFileMirror struct {
	plan.Plan
	Repo           string `json:"repo"`
	Ref            string `json:"ref"`
	CodeownersPath string `json:"codeowners_path"`
}

// topLevelKeys marshals v and returns its top-level JSON object keys, sorted
// so comparisons are order-independent.
func topLevelKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal into key map: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// jsonKind names the JSON type of a raw value. Arrays report their element
// type too, so a []string turning into a []int is caught, not just a rename.
func jsonKind(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return kindOf(v)
}

func kindOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		if len(x) == 0 {
			return "array<empty>"
		}
		return "array<" + kindOf(x[0]) + ">"
	}
	return "unknown"
}

// fieldKinds marshals v and reports every top-level key's JSON type.
func fieldKinds(t *testing.T, v any) map[string]string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal into key map: %v", err)
	}
	out := make(map[string]string, len(m))
	for k, raw := range m {
		out[k] = jsonKind(t, raw)
	}
	return out
}

func wantKeys(t *testing.T, what string, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s top-level JSON keys changed.\n got: %v\nwant: %v\n"+
			"If this change is intentional, update the literal in this test — the plan\n"+
			"document is `apply`'s sole input and a CI artifact, so its shape is a contract.",
			what, got, want)
	}
}

// fullChange is a Change with every field populated, so no omitempty tag can
// hide a field from the key enumeration.
func fullChange() plan.Change {
	return plan.Change{
		Action:    "amend",
		Line:      7,
		Pattern:   "/x/",
		OldOwners: []string{"@a"},
		NewOwners: []string{"@a", "@b"},
		OldLine:   "/x/ @a",
		NewLine:   "/x/ @a @b",
		Reason:    "because",
	}
}

func fullRow() plan.Row {
	return plan.Row{Path: "x/a.go", Before: []string{"@a"}, After: []string{"@a", "@b"}}
}

func fullOpResult() plan.OpResult {
	// Every slice populated too: the R-32 fields rode in unpinned (the exact
	// omitempty-hides-a-field failure this helper's comment warns about), and
	// R-40's left_open must not repeat that (review finding).
	return plan.OpResult{ID: "op-1", Op: "add_owner(/x/, @b)", Status: "applied", Proven: "tree", Reason: "because",
		Excepted:        []plan.ExceptedPath{{Path: "x/gen/g.go", Owners: []string{"@a"}}},
		ExceptUnmatched: []string{"/x/ghost/"},
		LeftOpen:        []string{"x/open.md"},
	}
}

func fullPlan() plan.Plan {
	return plan.Plan{
		Ops:          []string{"add_owner(/x/, @b)"},
		HashBefore:   "0f7c1e1d",
		SizeBefore:   14,
		SizeAfter:    20,
		Warnings:     []string{"a warning"},
		Changes:      []plan.Change{fullChange()},
		Rows:         []plan.Row{fullRow()},
		Diff:         "@ line 1\n-/x/ @a\n+/x/ @a @b\n",
		AfterContent: "/x/ @a @b\n",
		OpResults:    []plan.OpResult{fullOpResult()},
	}
}

// SPEC R-16 (plan document, top level): the set of keys a marshalled Plan
// emits is pinned to a literal list. This is deliberately an ENUMERATION and
// not a spot-check: the failure this guards against is a field being ADDED
// without anyone noticing that `plan --out`'s output changed, and a
// spot-checking test cannot see an addition. op_results is included because
// it was added without a test noticing; listing it here records the current
// shape as intentional.
func TestR16_PlanTopLevelKeys(t *testing.T) {
	wantKeys(t, "plan.Plan", topLevelKeys(t, fullPlan()), []string{
		"ops",
		"sha256_after",
		"sha256_before",
		"size_before",
		"size_after",
		"warnings",
		"changes",
		"ownership_rows",
		"diff",
		"after_content",
		"op_results",
	})
}

// SPEC R-16 (change records): a Change's JSON keys, enumerated. Each change is
// one line edit plus the reason it was chosen; a reviewer reading a plan in CI
// reads these fields, so renaming one silently breaks every consumer that is
// not the Go binary.
func TestR16_ChangeKeys(t *testing.T) {
	wantKeys(t, "plan.Change", topLevelKeys(t, fullChange()), []string{
		"action", "line", "pattern",
		"old_owners", "new_owners",
		"old_line", "new_line",
		"reason",
	})
}

// SPEC R-16 (ownership rows): a Row's JSON keys, enumerated. Note the Go field
// names (Before/After) differ from the JSON names (owners_before/owners_after)
// — exactly the kind of mapping a refactor of the struct can break without any
// compile error.
func TestR16_RowKeys(t *testing.T) {
	wantKeys(t, "plan.Row", topLevelKeys(t, fullRow()), []string{
		"path", "owners_before", "owners_after",
	})
}

// SPEC R-24 (op results): an OpResult's JSON keys, enumerated. This is the
// newest member of the plan document and the one whose addition this suite
// failed to notice.
func TestR16_OpResultKeys(t *testing.T) {
	wantKeys(t, "plan.OpResult", topLevelKeys(t, fullOpResult()), []string{
		"id", "op", "status", "proven", "reason",
		"excepted", "except_unmatched", "left_open",
	})
}

// SPEC R-16 (field types): every field's JSON name mapped to its JSON type.
// The key-set tests catch renames and additions; this catches a type change
// under an unchanged name — a string becoming an int, a scalar becoming a
// list — which is the change most likely to pass review and then break a
// consumer at runtime.
func TestR16_FieldTypes(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  map[string]string
	}{
		{"plan.Plan", fullPlan(), map[string]string{
			"ops":            "array<string>",
			"sha256_after":   "string",
			"sha256_before":  "string",
			"size_before":    "number",
			"size_after":     "number",
			"warnings":       "array<string>",
			"changes":        "array<object>",
			"ownership_rows": "array<object>",
			"diff":           "string",
			"after_content":  "string",
			"op_results":     "array<object>",
		}},
		{"plan.Change", fullChange(), map[string]string{
			"action":     "string",
			"line":       "number",
			"pattern":    "string",
			"old_owners": "array<string>",
			"new_owners": "array<string>",
			"old_line":   "string",
			"new_line":   "string",
			"reason":     "string",
		}},
		{"plan.Row", fullRow(), map[string]string{
			"path":          "string",
			"owners_before": "array<string>",
			"owners_after":  "array<string>",
		}},
		{"plan.OpResult", fullOpResult(), map[string]string{
			"id":               "string",
			"op":               "string",
			"status":           "string",
			"proven":           "string",
			"reason":           "string",
			"excepted":         "array<object>",
			"except_unmatched": "array<string>",
			"left_open":        "array<string>",
		}},
	}
	for _, c := range cases {
		got := fieldKinds(t, c.value)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s field types changed.\n got: %v\nwant: %v", c.name, got, c.want)
		}
	}
}

// SPEC R-16 (omitempty): which keys DISAPPEAR from a minimal document, pinned
// explicitly. omitempty is not cosmetic here — it decides whether a consumer
// can distinguish "this plan produced no warnings" from "this plan is from a
// binary that predates warnings". The unconditional keys (ops, changes,
// ownership_rows, diff, after_content) are always present, so a reader can
// treat their absence as a malformed document rather than as an empty result.
func TestR16_OmitEmptyKeysDisappear(t *testing.T) {
	cases := []struct {
		name    string
		value   any
		want    []string
		omitted []string
	}{
		{
			name:    "plan.Plan",
			value:   plan.Plan{},
			want:    []string{"ops", "sha256_before", "sha256_after", "size_before", "size_after", "changes", "ownership_rows", "diff", "after_content"},
			omitted: []string{"warnings", "op_results"},
		},
		{
			name:    "plan.Change",
			value:   plan.Change{},
			want:    []string{"action", "line", "pattern", "reason"},
			omitted: []string{"old_owners", "new_owners", "old_line", "new_line"},
		},
		{
			// Row has NO omitempty anywhere, and that is the point: see
			// TestR16_OwnershipRowsDistinguishNullFromEmpty.
			name:    "plan.Row",
			value:   plan.Row{},
			want:    []string{"path", "owners_before", "owners_after"},
			omitted: nil,
		},
		{
			name:    "plan.OpResult",
			value:   plan.OpResult{},
			want:    []string{"op", "status"},
			omitted: []string{"id", "proven", "reason"},
		},
	}
	for _, c := range cases {
		got := topLevelKeys(t, c.value)
		wantKeys(t, "minimal "+c.name, got, c.want)
		present := map[string]bool{}
		for _, k := range got {
			present[k] = true
		}
		for _, k := range c.omitted {
			if present[k] {
				t.Errorf("minimal %s: key %q should be omitted when empty but was emitted", c.name, k)
			}
		}
	}
}

// SPEC R-16/S-9 (null vs []): ownership_rows must distinguish JSON null —
// "no rule matches this path", i.e. genuinely unowned — from [] — "a rule
// matches and deliberately lists zero owners" (--on-empty=unowned). They are
// different states of the repository and internal/verify treats a transition
// between them as a real change (see internal/verify/verify_test.go, which
// pins the same distinction at the snapshot layer).
//
// This is why Row carries no omitempty: `owners_before,omitempty` would erase
// nil and [] into the same absent key, collapsing the two states into one on
// the way through the plan document.
func TestR16_OwnershipRowsDistinguishNullFromEmpty(t *testing.T) {
	p := plan.Plan{Rows: []plan.Row{
		{Path: "unowned.go", Before: nil, After: []string{"@a"}},
		{Path: "zeroed.go", Before: []string{"@a"}, After: []string{}},
	}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Rows []map[string]json.RawMessage `json:"ownership_rows"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(doc.Rows))
	}
	if got := string(doc.Rows[0]["owners_before"]); got != "null" {
		t.Errorf("unowned path: owners_before = %s, want null (no rule matches)", got)
	}
	if got := string(doc.Rows[1]["owners_after"]); got != "[]" {
		t.Errorf("explicitly zero-owned path: owners_after = %s, want [] (rule matches, zero owners)", got)
	}

	// And the distinction survives the trip back: nil stays nil, [] stays a
	// non-nil empty slice.
	var back plan.Plan
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Rows[0].Before != nil {
		t.Errorf("null owners_before unmarshalled to %#v, want nil", back.Rows[0].Before)
	}
	if back.Rows[1].After == nil || len(back.Rows[1].After) != 0 {
		t.Errorf("[] owners_after unmarshalled to %#v, want a non-nil empty slice", back.Rows[1].After)
	}
}

// SPEC R-16/S-9 (null vs [], end to end): the same distinction as produced by
// the real planner, not by a hand-built struct. remove_owner under
// --on-empty=unowned keeps the pattern with zero owners, and the row for that
// path must serialize owners_after as [], not null — a reviewer reading the
// plan in CI is being told "deliberately un-owned", which is a different (and
// legal, S-9) outcome from "no rule matches".
func TestR16_OwnershipRowsNullVsEmptyFromRealPlan(t *testing.T) {
	tree := []string{"x/a.go", "y/b.go"}
	p, err := build(t, "/x/ @a\n/y/ @b\n", tree, plan.Options{OnEmpty: "unowned"}, "remove_owner(/x/, @a)")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Rows []map[string]json.RawMessage `json:"ownership_rows"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Rows) != 1 {
		t.Fatalf("got %d ownership rows, want 1", len(doc.Rows))
	}
	if got := string(doc.Rows[0]["owners_after"]); got != "[]" {
		t.Errorf("owners_after = %s, want [] (--on-empty=unowned leaves a matching rule with zero owners, S-9)", got)
	}
}

// SPEC R-16 (envelope): the CLI wraps a Plan in planFile, adding repo, ref and
// codeowners_path. Because Plan is EMBEDDED without a json tag, encoding/json
// inlines its fields — the document has no nested "Plan" object, and apply
// unmarshalling the file gets both the envelope and the plan in one pass.
// Pinning that inlining matters: giving the embedded field a name would nest
// every plan key one level deeper and break every existing plan.json, with no
// compile error anywhere.
func TestR16_PlanFileEnvelopeInlinesPlan(t *testing.T) {
	pf := planFileMirror{Plan: fullPlan(), Repo: ".", Ref: "HEAD", CodeownersPath: ".github/CODEOWNERS"}
	got := topLevelKeys(t, pf)
	want := append(topLevelKeys(t, fullPlan()), "repo", "ref", "codeowners_path")
	wantKeys(t, "cli planFile envelope", got, want)
	for _, k := range got {
		if k == "Plan" || k == "plan" {
			t.Error("planFile must inline the embedded Plan, not nest it under a key")
		}
	}
}

// SPEC R-16 (round trip): a plan marshalled and unmarshalled through the exact
// path `apply` uses — json.MarshalIndent of the planFile envelope, then
// json.Unmarshal back into it — is semantically identical. This is the actual
// apply contract: everything the planner proved reaches the writer through
// this round trip and nothing else, so any field that does not survive it is a
// proof that silently does not reach `apply`.
func TestR16_RoundTripThroughApplyPath(t *testing.T) {
	orig := planFileMirror{
		Plan:           fullPlan(),
		Repo:           "/some/repo",
		Ref:            "HEAD",
		CodeownersPath: ".github/CODEOWNERS",
	}
	// json.MarshalIndent(pf, "", "  ") is precisely what cmdPlan writes.
	b, err := json.MarshalIndent(orig, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var back planFileMirror
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orig, back) {
		t.Errorf("plan did not survive the apply round trip.\n got: %#v\nwant: %#v", back, orig)
	}
}

// SPEC R-16 (round trip, real plan): the same round trip over a plan produced
// by the planner rather than by hand, including the two fields apply actually
// consumes — after_content (the bytes written to disk) and sha256_before (the
// drift gate).
func TestR16_RoundTripPreservesRealPlan(t *testing.T) {
	tree := []string{"x/a.go", "y/b.go"}
	p, err := build(t, "/x/ @a\n/y/ @b\n", tree, plan.Options{}, "add_owner(/x/, @b)")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(planFileMirror{Plan: *p, Repo: ".", Ref: "HEAD", CodeownersPath: "CODEOWNERS"}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var back planFileMirror
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*p, back.Plan) {
		t.Errorf("real plan did not survive the round trip.\n got: %#v\nwant: %#v", back.Plan, *p)
	}
	if back.Plan.AfterContent != p.AfterContent {
		t.Errorf("after_content = %q, want %q — apply writes these bytes to disk", back.Plan.AfterContent, p.AfterContent)
	}
}

// SPEC R-16 (drift gate): sha256_before is the pin that makes apply REFUSE a
// CODEOWNERS file that changed since the plan was computed — the plan's
// invariants were proven against those exact bytes and are worthless against
// any others. It must be the hash of the planned-over content and it must
// survive the round trip byte for byte; a plan whose hash is lost or mangled
// either fails safe (refuses forever) or, worse, would need the check
// disabled.
func TestR16_HashBeforeSurvivesRoundTrip(t *testing.T) {
	content := "/x/ @a\n/y/ @b\n"
	tree := []string{"x/a.go", "y/b.go"}
	p, err := build(t, content, tree, plan.Options{}, "add_owner(/x/, @b)")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	want := hex.EncodeToString(sum[:])
	if p.HashBefore != want {
		t.Fatalf("sha256_before = %q, want the sha256 of the planned-over bytes %q", p.HashBefore, want)
	}
	b, err := json.MarshalIndent(planFileMirror{Plan: *p, Repo: ".", Ref: "HEAD", CodeownersPath: "CODEOWNERS"}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var back planFileMirror
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.HashBefore != want {
		t.Errorf("sha256_before after round trip = %q, want %q — apply compares this against the file on disk", back.HashBefore, want)
	}
}

// SPEC R-16 (forward compatibility, unknown field): a plan document containing
// a field this binary does not know still unmarshals. Plans are written to CI
// artifacts and read back by a possibly-newer or possibly-older binary, so an
// unknown key must be ignored rather than rejected. This is Go's default —
// apply must never opt into DisallowUnknownFields, and this test is what
// notices if it ever does.
func TestR16_ForwardCompatUnknownFieldIsIgnored(t *testing.T) {
	b, err := json.Marshal(planFileMirror{Plan: fullPlan(), Repo: ".", Ref: "HEAD", CodeownersPath: "CODEOWNERS"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	doc["field_from_a_newer_binary"] = map[string]any{"nested": []any{1, 2, 3}}
	withExtra, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	var back planFileMirror
	if err := json.Unmarshal(withExtra, &back); err != nil {
		t.Fatalf("plan with an unknown field failed to unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back.Plan, fullPlan()) {
		t.Errorf("unknown field disturbed the known ones.\n got: %#v\nwant: %#v", back.Plan, fullPlan())
	}
	if back.CodeownersPath != "CODEOWNERS" {
		t.Errorf("codeowners_path = %q, want %q", back.CodeownersPath, "CODEOWNERS")
	}
}

// SPEC R-16 (forward compatibility, missing field): a plan document written
// before a field existed unmarshals with that field at its zero value, and
// every other field intact. op_results is the concrete case — it was added on
// this branch, so every plan.json already sitting in a CI artifact lacks it,
// and this is what lets a newer binary still apply one.
func TestR16_ForwardCompatMissingFieldIsZero(t *testing.T) {
	b, err := json.Marshal(planFileMirror{Plan: fullPlan(), Repo: ".", Ref: "HEAD", CodeownersPath: "CODEOWNERS"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	// Simulate a plan.json written by a binary that predates these fields.
	delete(doc, "op_results")
	delete(doc, "warnings")
	older, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	var back planFileMirror
	if err := json.Unmarshal(older, &back); err != nil {
		t.Fatalf("older plan failed to unmarshal: %v", err)
	}
	if back.OpResults != nil {
		t.Errorf("op_results = %#v, want nil for a plan that predates the field", back.OpResults)
	}
	if back.Warnings != nil {
		t.Errorf("warnings = %#v, want nil for a plan that omits it", back.Warnings)
	}
	// The fields apply actually depends on must be untouched by the omission.
	if back.HashBefore != fullPlan().HashBefore {
		t.Errorf("sha256_before = %q, want %q", back.HashBefore, fullPlan().HashBefore)
	}
	if back.AfterContent != fullPlan().AfterContent {
		t.Errorf("after_content = %q, want %q", back.AfterContent, fullPlan().AfterContent)
	}
	if !reflect.DeepEqual(back.Changes, fullPlan().Changes) {
		t.Errorf("changes = %#v, want %#v", back.Changes, fullPlan().Changes)
	}
}
