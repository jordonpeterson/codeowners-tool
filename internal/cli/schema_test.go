package cli_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// ---------------------------------------------------------------------------
// Schema pins for the sync record (R-24).
//
// A SyncRecord is not a log line, it is the fleet's data plane. `sync
// --format json` appends one object per repo to results.jsonl and the fleet
// script in docs/FLEET.md aggregates the run with
// `jq -s 'group_by(.status)|map({status:.[0].status, n:length})'`, plus the
// documented habit of projecting `.ops_skipped`. Those key names are the whole
// interface between this tool and a user's shell; nothing in Go's type system
// touches them.
//
// Until these tests existed the shape was pinned by NOTHING. Every other test
// in this package json.Unmarshals INTO cli.SyncRecord, so the struct tags sit
// on both sides of the assertion: renaming `ops_applied` to `opsApplied` keeps
// the entire suite green while silently emptying the field for every jq
// consumer on the planet. A schema that only ever round-trips through itself
// is not pinned at all.
//
// These are characterization tests. They record the CURRENT shape as
// intentional so the next change to it is a deliberate, reviewed one, and they
// mirror internal/plan/schema_test.go, which does the same job for the plan
// document. Like those, they assert the key SET, never the key ORDER —
// encoding/json emits fields in declaration order, but that ordering is not
// part of the contract and pinning it would fail on a harmless reshuffle.
// ---------------------------------------------------------------------------

// schemaTopLevelKeys marshals v and returns its top-level JSON object keys,
// sorted so comparisons are order-independent.
func schemaTopLevelKeys(t *testing.T, v any) []string {
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

// schemaKindOf names the JSON type of a decoded value. Arrays report their
// element type too, so an array of objects turning into an array of strings is
// caught, not just a rename.
func schemaKindOf(v any) string {
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
		return "array<" + schemaKindOf(x[0]) + ">"
	}
	return "unknown"
}

// schemaFieldKinds marshals v and reports every top-level key's JSON type.
func schemaFieldKinds(t *testing.T, v any) map[string]string {
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
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		out[k] = schemaKindOf(v)
	}
	return out
}

func schemaWantKeys(t *testing.T, what string, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s top-level JSON keys changed.\n got: %v\nwant: %v\n"+
			"If this change is intentional, update the literal in this test AND the README's\n"+
			"JSON output section — these key names are what users' jq scripts select on.",
			what, got, want)
	}
}

// schemaFullOpResult is an OpResult with every field populated, so no
// omitempty tag can hide a field from the key enumeration.
func schemaFullOpResult() plan.OpResult {
	return plan.OpResult{ID: "tf", Op: "add_owner(**/*.tf, @org/infra)", Status: "skipped", Proven: "structural", Reason: "scope matched zero tracked files"}
}

func schemaFullChange() plan.Change {
	return plan.Change{
		Action:    "amend",
		Line:      7,
		Pattern:   "/services/api/",
		OldOwners: []string{"@org/api-team"},
		NewOwners: []string{"@org/api-team", "@org/infra"},
		OldLine:   "/services/api/ @org/api-team",
		NewLine:   "/services/api/ @org/api-team @org/infra",
		Reason:    "add_owner",
	}
}

// schemaFullRecord is a SyncRecord with every field populated, including the
// omitempty ones, so the key enumeration sees the maximal document.
func schemaFullRecord() cli.SyncRecord {
	return cli.SyncRecord{
		Repo: "work/org/foo",
		// The artifact this record came from (R-20). Populated for the same
		// reason every other omitempty field here is: a field left at its zero
		// value is a field the key enumeration cannot see.
		Policy: "waves/api-ownership.json",
		// Populated like every other omitempty field: a field missing HERE is a
		// field the key enumeration below cannot see, so the pin passes and the
		// addition goes unnoticed — which is the failure that test is for.
		CodeownersPath: ".github/CODEOWNERS",
		Status:         cli.StatusApplied,
		Ops:            []plan.OpResult{schemaFullOpResult()},
		OpsApplied:     2,
		OpsSkipped:     1,
		PathsChanged:   37,
		Created:        true,
		DryRun:         true,
		Warnings:       []string{"file is 2.6 MB"},
		Changes:        []plan.Change{schemaFullChange()},
		Error:          "would violate INV-2 at /docs",
	}
}

// schemaStatusConstsFromSource parses this package's non-test sources and
// returns every exported `Status*` string constant it declares, name to value.
// Reading the source rather than the compiled identifiers is what lets the
// test notice a SIXTH status being added: a Go test can reference constants it
// knows about, but it cannot enumerate ones it does not.
func schemaStatusConstsFromSource(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if !strings.HasPrefix(id.Name, "Status") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", lit.Value, err)
					}
					out[id.Name] = v
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no Status* constants in the package source — the scan is broken, not the schema")
	}
	return out
}

// SPEC R-24 (sync record, top level): the set of keys a marshalled SyncRecord
// emits is pinned to a literal list. This is deliberately an ENUMERATION and
// not a spot-check: the failures it guards against are a field being RENAMED
// (every jq selector on it starts returning null) and a field being ADDED
// without anyone noticing that `sync --format json` now emits something new,
// and a spot-checking test can see neither. Every other cli test unmarshals
// into SyncRecord, using the same tags on both sides of its assertions, so
// this is the only place the wire names are compared to anything external.
func TestR24_SyncRecordTopLevelKeys(t *testing.T) {
	schemaWantKeys(t, "cli.SyncRecord", schemaTopLevelKeys(t, schemaFullRecord()), []string{
		"repo",
		"policy",
		"codeowners_path",
		"status",
		"ops",
		"ops_applied",
		"ops_skipped",
		"paths_changed",
		"created",
		"dry_run",
		"warnings",
		"changes",
		"error",
	})
}

// SPEC R-24 (field types): every field's JSON name mapped to its JSON type.
// The key-set test catches renames and additions; this catches a type change
// under an unchanged name. `ops_applied` going from number to string is the
// concrete case — `jq 'map(.ops_applied)|add'` on strings is a runtime error
// in the middle of a 100-repo rollout, and nothing in Go would have complained
// at the moment the field's type changed. `created` being a bool matters for
// the same reason: `select(.created)` is true for the string "false".
func TestR24_SyncRecordFieldTypes(t *testing.T) {
	got := schemaFieldKinds(t, schemaFullRecord())
	want := map[string]string{
		"repo":            "string",
		"policy":          "string",
		"codeowners_path": "string",
		"status":          "string",
		"ops":             "array<object>",
		"ops_applied":     "number",
		"ops_skipped":     "number",
		"paths_changed":   "number",
		"created":         "bool",
		"dry_run":         "bool",
		"warnings":        "array<string>",
		"changes":         "array<object>",
		"error":           "string",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cli.SyncRecord field types changed.\n got: %v\nwant: %v", got, want)
	}
}

// SPEC R-24 (omitempty): which keys DISAPPEAR from a minimal record, pinned
// explicitly, and which are unconditional. The split is not cosmetic. The
// optional six (ops, warnings, changes, error, codeowners_path, policy) are
// absent when there is nothing to say, so a consumer reads their absence as
// "empty", and a clean run's line stays short enough to eyeball.
// `codeowners_path` earns its place in that set: absent means no file was ever
// chosen, so `git add "$(jq -r .codeowners_path)"` on such a record stages
// nothing rather than staging a path the run never wrote. `policy` earns it the
// same way: absent means no policy file was involved at all, which is what
// separates an ad-hoc `--op` run from a wave, and an empty string would bucket
// every such run together under `group_by(.policy)`. The unconditional six are
// the aggregation keys: absence there is a malformed record, not an empty
// result.
func TestR24_OmitEmptyKeysDisappear(t *testing.T) {
	got := schemaTopLevelKeys(t, cli.SyncRecord{})
	schemaWantKeys(t, "minimal cli.SyncRecord", got, []string{
		"repo", "status", "ops_applied", "ops_skipped", "paths_changed", "created",
	})
	present := map[string]bool{}
	for _, k := range got {
		present[k] = true
	}
	for _, k := range []string{"ops", "warnings", "changes", "error", "codeowners_path", "policy"} {
		if present[k] {
			t.Errorf("minimal record: key %q should be omitted when empty but was emitted", k)
		}
	}
}

// SPEC R-24 (always-present keys): `status` and the three counts are emitted
// even at their zero values, on every record, unconditionally. This is the
// single most load-bearing property of the whole document. The documented
// fleet aggregation is `jq -s 'group_by(.status)'`; on a record missing
// `.status` that expression does not error, it groups the repo under null — a
// silent fleet-wide misreport, which is exactly the failure this schema exists
// to prevent. Same for the counts: `ops_skipped,omitempty` would hide 0 and make
// the documented "project .ops_skipped too" habit read null on precisely the
// repos that were fine.
//
// The zero-valued record below is not hypothetical: a refused repo emits
// status "refused" with all three counts at 0, and that line still has to
// aggregate.
func TestR24_StatusAndCountsAreAlwaysPresent(t *testing.T) {
	records := []cli.SyncRecord{
		{},
		{Status: cli.StatusRefused, Repo: "work/org/foo"},
		{Status: cli.StatusUnchanged, Repo: "work/org/bar", OpsApplied: 0, OpsSkipped: 0, PathsChanged: 0},
		schemaFullRecord(),
	}
	required := []string{"status", "ops_applied", "ops_skipped", "paths_changed"}
	for i, rec := range records {
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range required {
			if _, ok := m[k]; !ok {
				t.Errorf("record %d (%s): key %q is missing; `jq -s 'group_by(.status)'` buckets such a record under null",
					i, b, k)
			}
		}
	}
}

// SPEC R-24 (nested per-op results): the per-op array renders under the key
// `ops`, and each element carries plan.OpResult's own names.
//
// Note the DELIBERATE difference between the two documents: plan.Plan renders
// this exact same []plan.OpResult under `op_results`, because Plan.Ops already
// owns `ops` there as the list of raw op strings (R-16) and must keep it. The
// sync record has no such collision, so it uses the shorter, documented name —
// `ops` is what docs/JSON.md shows and what
// fleet scripts select. This is not an inconsistency to be tidied up: renaming
// either one to match the other breaks a published contract. The test pins
// both names at once so a well-meant "fix" fails here instead of in a user's
// pipeline.
func TestR24_PerOpResultsRenderUnderOps(t *testing.T) {
	b, err := json.Marshal(schemaFullRecord())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["ops"]; !ok {
		t.Fatalf("sync record must render per-op results under %q; got keys %v", "ops", schemaTopLevelKeys(t, schemaFullRecord()))
	}
	if _, ok := m["op_results"]; ok {
		t.Error("sync record must NOT use `op_results` — that is the plan document's name for the same type")
	}

	// The two documents deliberately differ. Pin the other side too.
	planKeys := schemaTopLevelKeys(t, plan.Plan{OpResults: []plan.OpResult{schemaFullOpResult()}})
	hasOpResults := false
	for _, k := range planKeys {
		if k == "op_results" {
			hasOpResults = true
		}
	}
	if !hasOpResults {
		t.Errorf("plan.Plan must keep rendering op results under `op_results` (its `ops` is the raw op strings); keys: %v", planKeys)
	}

	// Each element's own key set, so a rename inside OpResult is caught here
	// too — the README documents id/status/proven/reason by name.
	var elems []json.RawMessage
	if err := json.Unmarshal(m["ops"], &elems); err != nil {
		t.Fatalf("ops is not an array: %v", err)
	}
	if len(elems) != 1 {
		t.Fatalf("got %d op results, want 1", len(elems))
	}
	schemaWantKeys(t, "sync record ops[0]", schemaTopLevelKeys(t, schemaFullOpResult()), []string{
		"id", "op", "status", "proven", "reason",
	})
	got := schemaFieldKinds(t, schemaFullOpResult())
	want := map[string]string{"id": "string", "op": "string", "status": "string", "proven": "string", "reason": "string"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("op result field types changed.\n got: %v\nwant: %v", got, want)
	}
}

// SPEC R-24 (status vocabulary): there are exactly five legal `status` values
// — applied, unchanged, skipped, refused, error — and they are pinned both to
// the exported constants and to the complete set of Status* constants declared
// in the package source. The source scan is the point: referencing the five
// constants catches a value being CHANGED, but only enumerating the
// declarations catches a sixth being ADDED. A new status shipped without a
// docs update means fleet operators have a bucket in their `group_by`
// output that no documentation explains, and every `case` statement written
// against the documented five silently falls through.
func TestR24_StatusValuesAreExactlyFive(t *testing.T) {
	want := map[string]string{
		"StatusApplied":   "applied",
		"StatusUnchanged": "unchanged",
		"StatusSkipped":   "skipped",
		"StatusRefused":   "refused",
		"StatusError":     "error",
	}
	// The constants as the compiler sees them: catches a changed value.
	live := map[string]string{
		"StatusApplied":   cli.StatusApplied,
		"StatusUnchanged": cli.StatusUnchanged,
		"StatusSkipped":   cli.StatusSkipped,
		"StatusRefused":   cli.StatusRefused,
		"StatusError":     cli.StatusError,
	}
	if !reflect.DeepEqual(live, want) {
		t.Errorf("status constant values changed.\n got: %v\nwant: %v", live, want)
	}
	// The constants as declared: catches a sixth being added.
	declared := schemaStatusConstsFromSource(t)
	if !reflect.DeepEqual(declared, want) {
		t.Errorf("the set of Status* constants changed.\n got: %v\nwant: %v\n"+
			"A new sync status must be added to the README's JSON output section and to this\n"+
			"test together — fleet scripts switch on these five by name.", declared, want)
	}
	// And they survive the wire as themselves.
	for name, v := range want {
		b, err := json.Marshal(cli.SyncRecord{Status: v})
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Status != v {
			t.Errorf("%s: status round-tripped as %q, want %q", name, doc.Status, v)
		}
	}
}

// SPEC R-24 (round trip): a fully-populated record marshalled and unmarshalled
// is semantically identical. `--out FILE` writes a record that a later step
// reads back, and the fleet script's results.jsonl is read by whatever the
// operator points at it, possibly a newer build of this same tool. A field
// that does not survive its own round trip is a field the document claims to
// carry and does not.
func TestR24_RoundTripPreservesEveryField(t *testing.T) {
	orig := schemaFullRecord()
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back cli.SyncRecord
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orig, back) {
		t.Errorf("record did not survive the round trip.\n got: %#v\nwant: %#v", back, orig)
	}
}

// SPEC R-24 (JSONL shape): several records marshalled one per line each parse
// individually, and no single record contains a raw newline. This is what
// makes the documented `>> results.jsonl` plus `jq -s` work at all: the shell
// appends whole lines and jq slurps them line by line, so one embedded newline
// inside one record splits it into two unparseable fragments and takes the
// whole fleet report down with it — after the run, when the repos are already
// cloned and mutated.
//
// The hazard is real, not theoretical: `error` and `warnings` carry free text
// composed from refusal messages, and `changes` carries CODEOWNERS line
// content. encoding/json escapes newlines to \n, which is precisely the
// property being pinned; a future switch to an encoder that does not (or one
// left in indent mode) breaks line-oriented parsing without breaking a single
// unmarshal-based test.
func TestR24_JSONLLinesParseIndividually(t *testing.T) {
	records := []cli.SyncRecord{
		{Repo: "work/org/a", Status: cli.StatusApplied, OpsApplied: 3, PathsChanged: 12},
		{Repo: "work/org/b", Status: cli.StatusUnchanged},
		{Repo: "work/org/c", Status: cli.StatusSkipped, OpsSkipped: 2},
		{
			Repo:     "work/org/d",
			Status:   cli.StatusRefused,
			Error:    "refused: INV-2 violated\n  /docs/x.md  {@a} → {@b}",
			Warnings: []string{"multi\nline\nwarning"},
			Changes:  []plan.Change{{Action: "amend", Line: 1, Pattern: "/x/", NewLine: "/x/ @a\n"}},
		},
		{Repo: "work/org/e", Status: cli.StatusError, Error: "clone missing"},
	}

	var lines []string
	for _, rec := range records {
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if i := strings.IndexAny(string(b), "\n\r"); i >= 0 {
			t.Fatalf("record for %s contains a raw newline at byte %d; a JSONL line must be one line: %s", rec.Repo, i, b)
		}
		lines = append(lines, string(b))
	}

	// Reassemble the file exactly as `>> results.jsonl` would, then parse it
	// back the way `jq -s` does: line by line, each independently.
	file := strings.Join(lines, "\n") + "\n"
	var slurped []cli.SyncRecord
	for i, line := range strings.Split(strings.TrimRight(file, "\n"), "\n") {
		var rec cli.SyncRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d failed to parse on its own: %v\nline: %s", i+1, err, line)
		}
		slurped = append(slurped, rec)
	}
	if !reflect.DeepEqual(slurped, records) {
		t.Errorf("records did not survive the JSONL round trip.\n got: %#v\nwant: %#v", slurped, records)
	}

	// And the aggregation the README actually prints: group_by(.status).
	counts := map[string]int{}
	for _, rec := range slurped {
		counts[rec.Status]++
	}
	want := map[string]int{
		cli.StatusApplied:   1,
		cli.StatusUnchanged: 1,
		cli.StatusSkipped:   1,
		cli.StatusRefused:   1,
		cli.StatusError:     1,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("group_by(.status) over the JSONL gave %v, want %v", counts, want)
	}
	if _, grouped := counts[""]; grouped {
		t.Error("a record aggregated under the empty status — that is the null bucket jq would report")
	}
}

// SPEC R-24 (forward compatibility, unknown field): a record containing a
// field this binary does not know still unmarshals, with the known fields
// untouched. results.jsonl outlives the binary that wrote it — a fleet run is
// resumed days later, often after an upgrade — so an unknown key must be
// ignored rather than rejected. This is Go's default; the reader must never
// opt into DisallowUnknownFields, and this test is what notices if it ever
// does.
func TestR24_ForwardCompatUnknownFieldIsIgnored(t *testing.T) {
	b, err := json.Marshal(schemaFullRecord())
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

	var back cli.SyncRecord
	if err := json.Unmarshal(withExtra, &back); err != nil {
		t.Fatalf("record with an unknown field failed to unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, schemaFullRecord()) {
		t.Errorf("unknown field disturbed the known ones.\n got: %#v\nwant: %#v", back, schemaFullRecord())
	}
}

// SPEC R-24 (forward compatibility, missing field): a record written before a
// field existed unmarshals with that field at its zero value and everything
// else intact. The counts are the ones that matter — a pre-`paths_changed`
// line must read as 0 rather than failing the parse, because the alternative
// is a resumed fleet run aborting on its own earlier output.
//
// Note what this test does NOT claim: that 0 and "absent" are
// distinguishable. They are not, which is exactly why the always-present
// guarantee in TestR24_StatusAndCountsAreAlwaysPresent is load-bearing — it is
// the only thing keeping "this run changed nothing" apart from "this field did
// not exist yet".
func TestR24_ForwardCompatMissingFieldIsZero(t *testing.T) {
	b, err := json.Marshal(schemaFullRecord())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	// Simulate a line written by a binary that predates these fields.
	for _, k := range []string{"paths_changed", "created", "ops", "warnings"} {
		delete(doc, k)
	}
	older, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	var back cli.SyncRecord
	if err := json.Unmarshal(older, &back); err != nil {
		t.Fatalf("older record failed to unmarshal: %v", err)
	}
	if back.PathsChanged != 0 {
		t.Errorf("paths_changed = %d, want 0 for a record that predates the field", back.PathsChanged)
	}
	if back.Created {
		t.Error("created = true, want false for a record that predates the field")
	}
	if back.Ops != nil {
		t.Errorf("ops = %#v, want nil for a record that omits it", back.Ops)
	}
	if back.Warnings != nil {
		t.Errorf("warnings = %#v, want nil for a record that omits it", back.Warnings)
	}
	// The aggregation keys that DID exist must be untouched by the omission.
	full := schemaFullRecord()
	if back.Status != full.Status || back.Repo != full.Repo {
		t.Errorf("repo/status = %q/%q, want %q/%q", back.Repo, back.Status, full.Repo, full.Status)
	}
	if back.OpsApplied != full.OpsApplied || back.OpsSkipped != full.OpsSkipped {
		t.Errorf("counts = %d/%d, want %d/%d", back.OpsApplied, back.OpsSkipped, full.OpsApplied, full.OpsSkipped)
	}
}
