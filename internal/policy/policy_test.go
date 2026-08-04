// Package policy_test encodes the acceptance tests for the policy file.
//
// The policy file is the unit of review for a fleet rollout (R-20): one
// artifact in git that states what ran across N repositories. That makes it
// the one place where a silent default is unaffordable — a typo'd
// `on_zero_mtach` that fell back to the default would apply the WRONG policy
// to every repo at once, and nothing downstream would notice.
//
// So the contract is:
//
//	Unknown fields, bad enum values, and duplicate JSON keys are HARD errors.
//	Syntax errors fail fast; semantic errors accumulate into *MultiError.
//	Every error carries file:line:col and names the op by index and id.
//	Go's own decoder messages never reach the operator.
//
// Per-op zero-match (R-21) is validated here too, because whether an op may
// carry `on_zero_match` at all depends only on the op kind — a repo-independent
// fact, and therefore a policy error, caught on repo 0 rather than repo 47.
package policy_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/policy"
)

const testFile = "policy.json"

func mustParse(t *testing.T, src string) *policy.Policy {
	t.Helper()
	p, err := policy.Parse([]byte(src), testFile)
	if err != nil {
		t.Fatalf("policy must parse, got error:\n%v\nsource:\n%s", err, src)
	}
	if p == nil {
		t.Fatal("Parse returned (nil, nil)")
	}
	return p
}

func mustReject(t *testing.T, src string) error {
	t.Helper()
	p, err := policy.Parse([]byte(src), testFile)
	if err == nil {
		t.Fatalf("policy must be rejected, got %+v for source:\n%s", p, src)
	}
	return err
}

// flatten expands a *MultiError so a test can count and inspect the individual
// problems rather than string-matching one blob.
func flatten(err error) []error {
	var multi *policy.MultiError
	if errors.As(err, &multi) {
		var out []error
		for _, e := range multi.Errs {
			out = append(out, flatten(e)...)
		}
		return out
	}
	return []error{err}
}

// located returns every problem that carries position information.
func located(err error) []*policy.Error {
	var out []*policy.Error
	for _, e := range flatten(err) {
		var pe *policy.Error
		if errors.As(e, &pe) {
			out = append(out, pe)
		}
	}
	return out
}

// assertMentions fails unless every wanted fragment appears in the error text.
func assertMentions(t *testing.T, err error, want ...string) {
	t.Helper()
	got := err.Error()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("error must mention %q; got:\n%s", w, got)
		}
	}
}

// assertMentionsAny fails unless at least one wanted fragment appears. Used
// where the message must SHOW the operator what it found but the natural
// rendering differs by JSON type — a literal for scalars, a type name for a
// composite. Pinning one of those spellings would be pinning wording.
func assertMentionsAny(t *testing.T, err error, want ...string) {
	t.Helper()
	got := err.Error()
	for _, w := range want {
		if strings.Contains(got, w) {
			return
		}
	}
	t.Errorf("error must show the offending value, as one of %q; got:\n%s", want, got)
}

// findLocated returns the first problem satisfying match. Tests search the
// error set instead of indexing it: an accumulating *MultiError makes no
// promise about WHICH problem prints first, and a test that indexes locs[0]
// turns any future reordering of the validation passes into a false failure.
func findLocated(err error, match func(*policy.Error) bool) (*policy.Error, bool) {
	for _, e := range located(err) {
		if match(e) {
			return e, true
		}
	}
	return nil, false
}

// readsAsFutureVersion reports whether an error tells the operator that their
// BINARY is behind the file, rather than that the file is wrong. The vocabulary
// is a set rather than one word so that a rewritten message keeps passing; it
// is checked in the negative for malformed versions, where a broad set makes
// the guard stricter, and in the positive alongside the version numbers
// themselves, which carry the actual meaning.
func readsAsFutureVersion(err error) bool {
	got := strings.ToLower(err.Error())
	for _, phrase := range []string{"newer", "too new", "future", "upgrade"} {
		if strings.Contains(got, phrase) {
			return true
		}
	}
	return false
}

// assertNoGoInternals enforces UX rule 5: Go's default decoder message names Go
// types and points at the wrong concept, so it must never reach the operator.
func assertNoGoInternals(t *testing.T, err error) {
	t.Helper()
	got := err.Error()
	for _, bad := range []string{"cannot unmarshal", "Go value of type", "encoding/json"} {
		if strings.Contains(got, bad) {
			t.Errorf("Go's own decoder message escaped (contains %q):\n%s", bad, got)
		}
	}
}

// SPEC R-20: the smallest policy anyone would write by hand — a version and a
// list of op strings — is accepted exactly as written, and every field the
// author did not mention stays at its default rather than acquiring one.
//
// This is step two of the escalation the README promises: run one operation
// with --op, decide you want to keep it, paste it into a file. If the minimal
// file needs a name, a description, or a per-op object before the tool will
// take it, that promise breaks at the moment an operator first tries to save
// their work — and a policy file nobody can write by hand is a policy file
// nobody reviews.
func TestR20_MinimalPolicyParses(t *testing.T) {
	p := mustParse(t, `{
  "version": 1,
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    "add_owner(/.github/workflows/, @org/ci)"
  ]
}`)
	if p.Version != policy.Version {
		t.Errorf("Version = %d, want %d", p.Version, policy.Version)
	}
	if len(p.Ops) != 2 {
		t.Fatalf("Ops = %+v, want 2 ops", p.Ops)
	}
	if p.Ops[0].Kind != ops.AddOwner || p.Ops[0].Scope != "/services/api/" || p.Ops[0].Owner != "@org/api-team" {
		t.Errorf("Ops[0] = %+v, want the parsed add_owner", p.Ops[0])
	}
	if p.OnEmpty != "" || p.Name != "" || p.Description != "" {
		t.Errorf("optional fields must stay at their zero value, got %+v", p)
	}
}

// SPEC R-20: a bare string is SHORTHAND for {"op": "<string>"} with every other
// field at its default — not a second form with its own code path. If the two
// ever diverge, a reviewer reading a mixed ops array cannot tell what runs.
//
// "Every other field at its default" includes the id, which stays EMPTY. The
// `ops[0]` form that appears in error messages and results is a display label
// the renderer computes from the position; storing it in the field instead
// would make an unnamed op indistinguishable from one a policy author
// deliberately named "ops[0]", and would key that op's fleet-wide results by a
// name that shifts the moment somebody inserts an op above it.
func TestR20_BareStringIsExactlyObjectShorthand(t *testing.T) {
	const spec = "add_owner(/x/, @a)"
	bare := mustParse(t, `{"version":1,"ops":["`+spec+`"]}`)
	object := mustParse(t, `{"version":1,"ops":[{"op":"`+spec+`"}]}`)
	if !reflect.DeepEqual(bare.Ops, object.Ops) {
		t.Errorf("shorthand and object form must be identical:\n bare = %+v\n obj  = %+v", bare.Ops, object.Ops)
	}
	// ...and both must equal what --op would have produced, plus defaults.
	direct, err := ops.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	got := bare.Ops[0]
	if got.Kind != direct.Kind || got.Scope != direct.Scope || got.Owner != direct.Owner || got.Raw != direct.Raw {
		t.Errorf("policy op = %+v, want the same parse as --op: %+v", got, direct)
	}
	if got.OnZeroMatch != "" {
		t.Errorf("OnZeroMatch = %q, want \"\" — the zero value must mean require, so shorthand preserves R-5 exactly", got.OnZeroMatch)
	}
	if got.ID != "" {
		t.Errorf("ID = %q, want \"\" — `ops[0]` is a display label, never a stored value", got.ID)
	}
	if object.Ops[0].ID != "" {
		t.Errorf("object-form ID = %q, want \"\" — omitting `id` must not synthesize one either", object.Ops[0].ID)
	}
}

// SPEC R-20: both forms are legal in the same ops array, in any order, and
// order is preserved (R-8 conflict detection downstream depends on it).
func TestR20_ShorthandAndObjectFormsMix(t *testing.T) {
	p := mustParse(t, `{
  "version": 1,
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    { "id": "tf", "op": "add_owner(**/*.tf, @org/infra)", "on_zero_match": "skip" },
    "add_owner(/docs/, @org/docs)"
  ]
}`)
	if len(p.Ops) != 3 {
		t.Fatalf("Ops = %+v, want 3", p.Ops)
	}
	wantScopes := []string{"/services/api/", "**/*.tf", "/docs/"}
	for i, want := range wantScopes {
		if p.Ops[i].Scope != want {
			t.Errorf("Ops[%d].Scope = %q, want %q — order must be preserved", i, p.Ops[i].Scope, want)
		}
	}
	if p.Ops[1].ID != "tf" || p.Ops[1].OnZeroMatch != ops.ZeroMatchSkip {
		t.Errorf("Ops[1] = %+v, want id=tf on_zero_match=skip", p.Ops[1])
	}
	if p.Ops[0].OnZeroMatch != "" || p.Ops[2].OnZeroMatch != "" {
		t.Error("an object-form neighbour must not leak its settings onto the shorthand ops")
	}
}

// SPEC R-20: every optional field round-trips. name/description/note exist so
// the PR reviewer on repo 63 learns WHY the change is in front of them; if they
// are silently dropped at parse, --summary-out has nothing to render.
func TestR20_AllOptionalFieldsArePopulated(t *testing.T) {
	p := mustParse(t, `{
  "version": 1,
  "name": "org baseline ownership",
  "description": "CI owns workflows everywhere; infra owns Terraform where it exists.",
  "on_empty": "error",
  "ops": [
    { "id": "tf", "op": "add_owner(**/*.tf, @org/infra)", "on_zero_match": "skip",
      "note": "Opportunistic — only repos that actually have Terraform." },
    { "id": "ci", "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare",
      "note": "Baseline; also covers workflows added later." }
  ]
}`)
	if p.Name != "org baseline ownership" {
		t.Errorf("Name = %q", p.Name)
	}
	if !strings.HasPrefix(p.Description, "CI owns workflows") {
		t.Errorf("Description = %q", p.Description)
	}
	if p.OnEmpty != "error" {
		t.Errorf("OnEmpty = %q, want %q", p.OnEmpty, "error")
	}
	wantNotes := map[string]string{
		"tf": "Opportunistic — only repos that actually have Terraform.",
		"ci": "Baseline; also covers workflows added later.",
	}
	if !reflect.DeepEqual(p.Notes, wantNotes) {
		t.Errorf("Notes = %#v, want %#v — keyed by op id", p.Notes, wantNotes)
	}
}

// SPEC R-21: on_zero_match and id ride on ops.Op, because plan.Build's signature
// does not change — the planner learns per-op zero-match behavior only from the
// op it is handed. A policy that parsed but left these zero would silently run
// the default everywhere.
func TestR21_OnZeroMatchAndIDComeFromTheFile(t *testing.T) {
	p := mustParse(t, `{
  "version": 1,
  "ops": [
    { "id": "req", "op": "add_owner(/a/, @a)", "on_zero_match": "require" },
    { "id": "skp", "op": "add_owner(/b/, @b)", "on_zero_match": "skip" },
    { "id": "dcl", "op": "add_owner(/c/, @c)", "on_zero_match": "declare" }
  ]
}`)
	want := []struct{ id, ozm string }{
		{"req", ops.ZeroMatchRequire},
		{"skp", ops.ZeroMatchSkip},
		{"dcl", ops.ZeroMatchDeclare},
	}
	for i, w := range want {
		if p.Ops[i].ID != w.id {
			t.Errorf("Ops[%d].ID = %q, want %q", i, p.Ops[i].ID, w.id)
		}
		if p.Ops[i].OnZeroMatch != w.ozm {
			t.Errorf("Ops[%d].OnZeroMatch = %q, want %q", i, p.Ops[i].OnZeroMatch, w.ozm)
		}
	}
}

// SPEC R-20: an unknown top-level field is a hard error. Silently ignoring
// "opps" means the policy that ran is not the policy that was reviewed.
func TestR20_UnknownTopLevelFieldRejected(t *testing.T) {
	for _, field := range []string{"opps", "onEmpty", "owners", "repos", "except_repos"} {
		t.Run(field, func(t *testing.T) {
			err := mustReject(t, `{"version":1,"`+field+`":"x","ops":["add_owner(/x/, @a)"]}`)
			assertMentions(t, err, field)
			assertNoGoInternals(t, err)
		})
	}
}

// SPEC R-20: unknown fields inside an OP object are the dangerous case — the
// natural string-or-object implementation loses DisallowUnknownFields the
// moment a custom unmarshaler takes over, so this is the test that catches the
// regression the UX doc calls out by name.
func TestR20_UnknownFieldInsideOpRejected(t *testing.T) {
	// "description" and "version" are legal at TOP level; inside an op they are
	// not. A per-level field set, not one merged bag.
	for _, field := range []string{"notes", "descriptoin", "description", "version", "ops", "when_absent"} {
		t.Run(field, func(t *testing.T) {
			err := mustReject(t, `{"version":1,"ops":[{"op":"add_owner(/x/, @a)","`+field+`":"x"}]}`)
			assertMentions(t, err, field)
			assertNoGoInternals(t, err)
		})
	}
}

// SPEC R-20: the motivating case. `on_zero_mtach` differs from the real field by
// two transposed letters; under a permissive decoder it applies the DEFAULT
// zero-match policy to 100 repos while the file in git says "skip". The error
// must quote the offending key and point at the one that was meant.
func TestR20_NearMissTypoNamesTheOffendingField(t *testing.T) {
	err := mustReject(t, `{"version":1,"ops":[{"id":"tf","op":"add_owner(**/*.tf, @org/infra)","on_zero_mtach":"skip"}]}`)
	assertMentions(t, err, "on_zero_mtach", "on_zero_match")
	assertNoGoInternals(t, err)
}

// SPEC R-20: version is required, not optional-defaulting-to-1. A strict format
// read by pinned binaries across a fleet, with no version marker, is a corner
// with no way out.
func TestR20_MissingVersionRejected(t *testing.T) {
	err := mustReject(t, `{"ops":["add_owner(/x/, @a)"]}`)
	assertMentions(t, err, "version")
	assertNoGoInternals(t, err)
}

// SPEC R-20: version 0 is what an absent field decodes to. It must be rejected
// for the same reason absence is — otherwise "version": 0 and "no version at
// all" become indistinguishable and the marker stops carrying information.
func TestR20_VersionZeroRejected(t *testing.T) {
	err := mustReject(t, `{"version":0,"ops":["add_owner(/x/, @a)"]}`)
	assertMentions(t, err, "version")
	assertNoGoInternals(t, err)
}

// SPEC R-20: "your binary is too old" and "this file is nonsense" are two
// different jobs for the operator — upgrade the tool, or go fix whatever
// generated the file. A pinned fleet binary that meets a version it does not
// implement has to say which one it is, or the operator picks wrong and
// spends the outage on the other.
//
// The message carries that verdict in the two NUMBERS it names: the version
// the file asks for and the version this binary implements. A malformed
// version has no such pair — there is nothing to compare — so it names the
// field and shows what it actually found, and must never send anyone off to
// upgrade over a stray quote mark.
func TestR20_NewerVersionDistinguishedFromGarbage(t *testing.T) {
	newer := mustReject(t, `{"version":2,"ops":["add_owner(/x/, @a)"]}`)
	assertNoGoInternals(t, newer)
	assertMentions(t, newer, "version", "2", strconv.Itoa(policy.Version))
	if !readsAsFutureVersion(newer) {
		t.Errorf("an unsupported FUTURE version must tell the operator the file came from a later release than this binary; got:\n%s", newer.Error())
	}

	// Each malformed version must name the field and show the value that was
	// found. `shown` lists the renderings that count: a scalar can be echoed
	// literally, a composite is more honestly named by its JSON type.
	garbage := []struct {
		json  string
		shown []string
	}{
		{`"1"`, []string{`"1"`, "string"}},
		{`-1`, []string{"-1"}},
		{`1.5`, []string{"1.5"}},
		{`true`, []string{"true", "bool"}},
		{`null`, []string{"null"}},
		{`[1]`, []string{"[1]", "array", "list"}},
	}
	for _, g := range garbage {
		t.Run("garbage_"+g.json, func(t *testing.T) {
			err := mustReject(t, `{"version":`+g.json+`,"ops":["add_owner(/x/, @a)"]}`)
			assertNoGoInternals(t, err)
			assertMentions(t, err, "version")
			assertMentionsAny(t, err, g.shown...)
			if readsAsFutureVersion(err) {
				t.Errorf("version %s is malformed, not from the future; the message must not send the operator to upgrade:\n%s", g.json, err.Error())
			}
		})
	}
}

// SPEC R-20: a policy with no ops does nothing on 100 repos and exits 0 on all
// of them — the silent-success failure this design exists to make impossible.
func TestR20_MissingOpsRejected(t *testing.T) {
	err := mustReject(t, `{"version":1}`)
	assertMentions(t, err, "ops")
	assertNoGoInternals(t, err)
}

// SPEC R-20: an EMPTY ops array is the same silent success, and is what a
// generator emits when its input query returned nothing.
func TestR20_EmptyOpsArrayRejected(t *testing.T) {
	err := mustReject(t, `{"version":1,"ops":[]}`)
	assertMentions(t, err, "ops")
	assertNoGoInternals(t, err)
}

// SPEC R-20: dispatch is on the first non-space byte — `"` is the string form,
// `{` the object form, and ANYTHING ELSE is a typed error. A number or null
// slipping through as a zero-value op would run an empty op against the fleet.
func TestR20_OpsEntryOfWrongJSONTypeRejected(t *testing.T) {
	for name, entry := range map[string]string{
		"number":  `7`,
		"float":   `1.5`,
		"array":   `["add_owner(/x/, @a)"]`,
		"null":    `null`,
		"bool":    `true`,
		"nested":  `[{"op":"add_owner(/x/, @a)"}]`,
		"emptyob": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := mustReject(t, `{"version":1,"ops":[`+entry+`]}`)
			assertNoGoInternals(t, err)
			if len(located(err)) == 0 {
				t.Errorf("a wrong-typed ops entry must still report a position; got %T: %v", err, err)
			}
		})
	}
}

// SPEC R-20: "ops" itself must be an array. A single string is the plausible
// generator mistake, and reading it as one op would be a helpful guess — the
// exact behavior a strict format forbids.
func TestR20_OpsItselfMustBeAnArray(t *testing.T) {
	for name, val := range map[string]string{
		"string": `"add_owner(/x/, @a)"`,
		"object": `{"op":"add_owner(/x/, @a)"}`,
		"null":   `null`,
		"number": `3`,
	} {
		t.Run(name, func(t *testing.T) {
			err := mustReject(t, `{"version":1,"ops":`+val+`}`)
			assertMentions(t, err, "ops")
			assertNoGoInternals(t, err)
		})
	}
}

// SPEC R-20: `op` is the one required field of the object form. An object
// carrying only id and note describes nothing; accepting it would put a
// phantom entry in the per-op results of every repo.
func TestR20_OpObjectWithoutOpKeyRejected(t *testing.T) {
	err := mustReject(t, `{"version":1,"ops":[{"id":"tf","note":"terraform","on_zero_match":"skip"}]}`)
	assertMentions(t, err, "op")
	assertNoGoInternals(t, err)
}

// SPEC R-21: the enum is exactly require|skip|declare, case-sensitively. "write"
// is the OLD name from revision 1 — accepting it would silently give a fleet
// the default behavior under a spelling whose author meant something else, and
// the rename would never actually have happened.
func TestR21_BadOnZeroMatchValueRejected(t *testing.T) {
	for _, bad := range []string{"SKIP", "Skip", "write", "wrote", "error", "declair", "true", ""} {
		t.Run("value_"+bad, func(t *testing.T) {
			err := mustReject(t, `{"version":1,"ops":[{"id":"tf","op":"add_owner(/x/, @a)","on_zero_match":"`+bad+`"}]}`)
			assertNoGoInternals(t, err)
			assertMentions(t, err, "on_zero_match")
			// The legal set must be enumerated — "invalid value" alone sends the
			// operator to the source.
			for _, legal := range []string{"require", "skip", "declare"} {
				if !strings.Contains(err.Error(), legal) {
					t.Errorf("error must enumerate the legal set (missing %q):\n%s", legal, err.Error())
				}
			}
		})
	}
}

// SPEC R-20: on_empty is validated lazily today — plan.go only reaches "unknown
// --on-empty policy" when a removal actually empties an owner set. A policy
// saying "inhrit" would pass, work on 46 repos, and blow up on repo 47.
// Validate the enum at load; that is precisely what `check` exists to prevent.
func TestR20_BadOnEmptyValueRejected(t *testing.T) {
	for _, bad := range []string{"inhrit", "ERROR", "unowend", "keep", "true", ""} {
		t.Run("value_"+bad, func(t *testing.T) {
			err := mustReject(t, `{"version":1,"on_empty":"`+bad+`","ops":["remove_owner(/x/, @a)"]}`)
			assertMentions(t, err, "on_empty")
			assertNoGoInternals(t, err)
			for _, legal := range []string{"error", "inherit", "unowned"} {
				if !strings.Contains(err.Error(), legal) {
					t.Errorf("error must enumerate the legal set (missing %q):\n%s", legal, err.Error())
				}
			}
		})
	}
	// The three legal values must survive.
	for _, good := range []string{"error", "inherit", "unowned"} {
		t.Run("legal_"+good, func(t *testing.T) {
			p := mustParse(t, `{"version":1,"on_empty":"`+good+`","ops":["remove_owner(/x/, @a)"]}`)
			if p.OnEmpty != good {
				t.Errorf("OnEmpty = %q, want %q", p.OnEmpty, good)
			}
		})
	}
}

// SPEC R-20: encoding/json silently takes the LAST duplicate key. A generator
// concatenating fragments produces on_empty twice and `inherit` wins without a
// word — the same failure class as the typo, and invisible in review because
// both lines are individually correct.
// The message must show BOTH values, because the whole failure is that one of
// them vanished without trace. Naming the key alone leaves the reviewer to
// work out which of their two lines the tool would have obeyed.
func TestR20_DuplicateTopLevelKeyRejected(t *testing.T) {
	cases := []struct {
		key string
		src string
		// The two values in conflict, which the message must both show. Empty
		// for a key whose values are whole arrays: quoting those back is not a
		// readable message, so `ops` is held only to naming the key.
		conflicting []string
	}{
		{"on_empty", `{"version":1,"on_empty":"error","ops":["remove_owner(/x/, @a)"],"on_empty":"inherit"}`,
			[]string{"error", "inherit"}},
		{"ops", `{"version":1,"ops":["add_owner(/x/, @a)"],"ops":["add_owner(/y/, @b)"]}`, nil},
		{"version", `{"version":1,"ops":["add_owner(/x/, @a)"],"version":2}`,
			[]string{"1", "2"}},
		{"name", `{"version":1,"name":"baseline-fragment-one","name":"baseline-fragment-two","ops":["add_owner(/x/, @a)"]}`,
			[]string{"baseline-fragment-one", "baseline-fragment-two"}},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			err := mustReject(t, c.src)
			assertMentions(t, err, c.key)
			assertMentions(t, err, c.conflicting...)
			assertNoGoInternals(t, err)
		})
	}
}

// SPEC R-20: the same, one level down. Per-element decoders are built fresh for
// each op, so it is easy to implement duplicate detection at the top level and
// forget it inside the ops array — where the values that steer behavior live.
// Every value here is a scalar, so the message has no excuse: it must name the
// key and show both of the values that were in conflict.
func TestR20_DuplicateKeyInsideOpRejected(t *testing.T) {
	cases := []struct {
		key         string
		src         string
		conflicting []string
	}{
		{"on_zero_match", `{"version":1,"ops":[{"id":"tf","op":"add_owner(/x/, @a)","on_zero_match":"skip","on_zero_match":"declare"}]}`,
			[]string{"skip", "declare"}},
		{"op", `{"version":1,"ops":[{"op":"add_owner(/x/, @a)","op":"add_owner(/y/, @b)"}]}`,
			[]string{"add_owner(/x/, @a)", "add_owner(/y/, @b)"}},
		{"id", `{"version":1,"ops":[{"id":"terraform-first","id":"terraform-second","op":"add_owner(/x/, @a)"}]}`,
			[]string{"terraform-first", "terraform-second"}},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			err := mustReject(t, c.src)
			assertMentions(t, err, c.key)
			assertMentions(t, err, c.conflicting...)
			assertNoGoInternals(t, err)
		})
	}
}

// SPEC R-20: two ops in one policy may not share an id. Results are keyed by
// id — the summary a reviewer reads, and whatever a fleet script pipes into
// jq — so a repeated id silently overwrites one op's outcome with another's.
// The reviewer then sees a policy that ran N ops reporting N-1 results, with
// no indication which repo the missing one applied to.
//
// An op with NO id is not a duplicate of another op with no id: the empty id
// is the absence of a name, and unnamed ops are referred to by position.
func TestR20_DuplicateOpIDRejected(t *testing.T) {
	err := mustReject(t, `{"version":1,"ops":[
  {"id":"tf","op":"add_owner(/a/, @a)"},
  {"id":"ci","op":"add_owner(/b/, @b)"},
  {"id":"tf","op":"add_owner(/c/, @c)"}
]}`)
	assertMentions(t, err, "tf")
	assertNoGoInternals(t, err)
	e, ok := findLocated(err, func(e *policy.Error) bool { return e.OpIndex == 0 || e.OpIndex == 2 })
	if !ok {
		t.Fatalf("the repeated id must be attributed to one of the ops that carries it (ops[0] or ops[2]); got %T:\n%v", err, err)
	}
	if e.OpID != "tf" {
		t.Errorf("OpID = %q, want %q — the error names the id that collided", e.OpID, "tf")
	}

	// Distinct ids are fine, and so is a policy where no op is named at all.
	mustParse(t, `{"version":1,"ops":[{"id":"a","op":"add_owner(/a/, @a)"},{"id":"b","op":"add_owner(/b/, @b)"}]}`)
	mustParse(t, `{"version":1,"ops":[{"op":"add_owner(/a/, @a)"},{"op":"add_owner(/b/, @b)"},"add_owner(/c/, @c)"]}`)
}

// SPEC R-21: an ABSENT on_zero_match means require. An on_zero_match that is
// PRESENT and empty is an error. These are two different states of the file and
// the parser must be able to tell them apart.
//
// The distinction is easy to lose: a plain Go struct field decodes both to the
// empty string, so an implementation that reads the field and checks its value
// cannot see the difference and will accept `"on_zero_match": ""` as a default.
// Detecting which keys the file actually contained is therefore part of the
// contract, not an implementation preference.
//
// What it buys: a generator that emits an empty string where it meant to emit
// a decision has produced a file that reads, to a human reviewer, as if a
// choice was made. Accepting it applies the default across the fleet under a
// spelling that says otherwise — the same silent-default failure as the typo,
// arriving through the correctly-spelled field.
func TestR21_ExplicitEmptyOnZeroMatchIsNotTheSameAsAbsent(t *testing.T) {
	// Absent: legal, and means require.
	absent := mustParse(t, `{"version":1,"ops":[{"id":"a","op":"add_owner(/x/, @a)"}]}`)
	if absent.Ops[0].OnZeroMatch != "" {
		t.Errorf("OnZeroMatch = %q, want \"\" — an absent key leaves the zero value, which means require", absent.Ops[0].OnZeroMatch)
	}
	// The bare-string shorthand has no key either, and lands in the same state.
	shorthand := mustParse(t, `{"version":1,"ops":["add_owner(/x/, @a)"]}`)
	if shorthand.Ops[0].OnZeroMatch != "" {
		t.Errorf("OnZeroMatch = %q, want \"\"", shorthand.Ops[0].OnZeroMatch)
	}

	// Present and empty: rejected, and the message names the field so the
	// operator can find the line their generator wrote.
	err := mustReject(t, `{"version":1,"ops":[{"id":"a","op":"add_owner(/x/, @a)","on_zero_match":""}]}`)
	assertMentions(t, err, "on_zero_match")
	assertNoGoInternals(t, err)
	for _, legal := range []string{"require", "skip", "declare"} {
		if !strings.Contains(err.Error(), legal) {
			t.Errorf("error must enumerate the legal set (missing %q):\n%s", legal, err.Error())
		}
	}
	if _, ok := findLocated(err, func(e *policy.Error) bool { return e.OpIndex == 0 }); !ok {
		t.Errorf("the empty on_zero_match must be attributed to ops[0]; got %T:\n%v", err, err)
	}

	// The same distinction one level up: on_empty is optional when no op
	// removes an owner, but writing it as an empty string is not "unset".
	mustParse(t, `{"version":1,"ops":["add_owner(/x/, @a)"]}`)
	err = mustReject(t, `{"version":1,"on_empty":"","ops":["add_owner(/x/, @a)"]}`)
	assertMentions(t, err, "on_empty")
	assertNoGoInternals(t, err)
}

// SPEC R-20: JSON has no comments and unknown fields are fatal, so without an
// escape hatch the universal "_comment" convention would be ILLEGAL. Keys
// starting with _ and the key // are ignored at EVERY level. This does not
// weaken typo detection — on_zero_mtach does not start with an underscore.
func TestR20_UnderscoreAndSlashKeysAreIgnored(t *testing.T) {
	p := mustParse(t, `{
  "version": 1,
  "_note": "keys starting with _ are ignored — JSON has no comments",
  "//": "so is this one",
  "_": "and a bare underscore",
  "on_empty": "error",
  "ops": [
    "add_owner(/x/, @a)",
    { "id": "tf", "op": "add_owner(**/*.tf, @org/infra)", "on_zero_match": "skip",
      "_why": "generated from the terraform inventory",
      "//": "do not hand-edit" }
  ]
}`)
	if len(p.Ops) != 2 {
		t.Fatalf("Ops = %+v, want 2", p.Ops)
	}
	if p.Ops[1].ID != "tf" || p.Ops[1].OnZeroMatch != ops.ZeroMatchSkip {
		t.Errorf("Ops[1] = %+v — ignored keys must not disturb the fields around them", p.Ops[1])
	}
	if _, ok := p.Notes["tf"]; ok {
		t.Errorf("Notes = %#v — `_why` is ignored, not a note", p.Notes)
	}
}

// SPEC R-20: an op string that ops.Parse rejects is a POLICY error. Letting the
// raw ops.Parse text out loses the file, line, and op index — the operator gets
// `unknown op "add_ownr"` with no idea which of 40 ops in which of 100 repos.
func TestR20_MalformedOpStringIsAPolicyError(t *testing.T) {
	cases := map[string]string{
		"unknown kind":   "add_ownr(/x/, @a)",
		"no parens":      "add_owner /x/ @a",
		"missing arg":    "add_owner(/x/)",
		"bad owner":      "add_owner(/x/, notanowner!)",
		"unescaped ws":   "add_owner(a b, @x)",
		"empty":          "",
		"set without []": "set_owners(/x/, @a)",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			err := mustReject(t, `{"version":1,"ops":["`+spec+`"]}`)
			assertNoGoInternals(t, err)
			locs := located(err)
			if len(locs) == 0 {
				t.Fatalf("a bad op string must surface as *policy.Error, got %T: %v", err, err)
			}
			e := locs[0]
			if e.File != testFile {
				t.Errorf("File = %q, want %q", e.File, testFile)
			}
			if e.OpIndex != 0 {
				t.Errorf("OpIndex = %d, want 0 — the operator must be told WHICH op", e.OpIndex)
			}
			if e.Line <= 0 {
				t.Errorf("Line = %d, want a real line number", e.Line)
			}
		})
	}
}

// SPEC R-21: rename_owner's scope is derived from current ownership, not a
// pattern — plan.go exempts it from R-5 entirely, so on_zero_match can never
// fire on it. Accepting the field and ignoring it is the same class of failure
// as the typo the strictness rule exists to catch.
func TestR21_OnZeroMatchRejectedOnRenameOwner(t *testing.T) {
	for _, val := range []string{"require", "skip", "declare"} {
		t.Run(val, func(t *testing.T) {
			err := mustReject(t, `{"version":1,"ops":[{"id":"ren","op":"rename_owner(@old, @new)","on_zero_match":"`+val+`"}]}`)
			assertMentions(t, err, "on_zero_match", "rename_owner")
			assertNoGoInternals(t, err)
		})
	}
	// Without the field it is a perfectly good op.
	p := mustParse(t, `{"version":1,"ops":["rename_owner(@old, @new)"]}`)
	if p.Ops[0].Kind != ops.RenameOwner {
		t.Errorf("Ops[0] = %+v, want a rename_owner", p.Ops[0])
	}
}

// SPEC R-21: `declare` means "write the rule anyway, for files that do not
// exist yet". A remove_owner has nothing to write — there is no rule to declare
// the absence of an owner on paths that are not there. Reject at parse.
func TestR21_DeclareRejectedOnRemoveOwner(t *testing.T) {
	err := mustReject(t, `{"version":1,"on_empty":"error","ops":[{"id":"rm","op":"remove_owner(/x/, @a)","on_zero_match":"declare"}]}`)
	assertMentions(t, err, "declare", "remove_owner")
	assertNoGoInternals(t, err)
}

// SPEC R-21: require and skip ARE meaningful on remove_owner — "this repo must
// have had @a here" versus "clean it up where it exists". Rejecting them would
// make the only fleet-safe removal impossible.
func TestR21_RequireAndSkipAreLegalOnRemoveOwner(t *testing.T) {
	for _, val := range []string{ops.ZeroMatchRequire, ops.ZeroMatchSkip} {
		t.Run(val, func(t *testing.T) {
			p := mustParse(t, `{"version":1,"on_empty":"error","ops":[{"id":"rm","op":"remove_owner(/x/, @a)","on_zero_match":"`+val+`"}]}`)
			if p.Ops[0].OnZeroMatch != val {
				t.Errorf("OnZeroMatch = %q, want %q", p.Ops[0].OnZeroMatch, val)
			}
		})
	}
}

// SPEC R-21: all three values are legal on add_owner and set_owners, including
// set_owners with an empty list (the zero-owner rule, S-9) under declare.
// The legality table is the contract; a value legal in the docs and rejected by
// the parser halts a fleet on repo 0 for no reason.
func TestR21_AllZeroMatchValuesLegalOnAddOwnerAndSetOwners(t *testing.T) {
	specs := []string{
		"add_owner(/x/, @a)",
		"set_owners(/x/, [@a, @b])",
		"set_owners(/x/, [])",
	}
	for _, spec := range specs {
		for _, val := range []string{ops.ZeroMatchRequire, ops.ZeroMatchSkip, ops.ZeroMatchDeclare} {
			t.Run(spec+"/"+val, func(t *testing.T) {
				p := mustParse(t, `{"version":1,"ops":[{"id":"o","op":"`+spec+`","on_zero_match":"`+val+`"}]}`)
				if p.Ops[0].OnZeroMatch != val {
					t.Errorf("OnZeroMatch = %q, want %q", p.Ops[0].OnZeroMatch, val)
				}
			})
		}
	}
}

// SPEC R-20: on_empty is REQUIRED when any op is a remove_owner, validated
// statically. Otherwise the R-6 question ("what happens when a removal empties
// an owner set?") is answered lazily on whichever repo first hits it — a repo-47
// surprise, turned here into a repo-0 one.
func TestR20_RemoveOwnerRequiresTopLevelOnEmpty(t *testing.T) {
	err := mustReject(t, `{"version":1,"ops":["add_owner(/x/, @a)","remove_owner(/y/, @b)"]}`)
	assertMentions(t, err, "on_empty", "remove_owner")
	assertNoGoInternals(t, err)

	// With on_empty present the same policy is fine...
	mustParse(t, `{"version":1,"on_empty":"inherit","ops":["add_owner(/x/, @a)","remove_owner(/y/, @b)"]}`)
	// ...and a policy with no removals must NOT be forced to declare one.
	if p := mustParse(t, `{"version":1,"ops":["add_owner(/x/, @a)"]}`); p.OnEmpty != "" {
		t.Errorf("OnEmpty = %q, want \"\" — a policy without removals must not need the field", p.OnEmpty)
	}
}

// SPEC R-20 (error quality): the format is file:line:col, then the op by index
// and id, then the message. A byte offset is what encoding/json gives and what
// nobody can use; this string is what a person greps out of a 100-repo log at
// 2am.
func TestR20_ErrorFormatsFileLineColAndOp(t *testing.T) {
	e := &policy.Error{File: "policy.json", Line: 14, Col: 22, OpIndex: 2, OpID: "tf", Msg: "unknown field \"on_zero_mtach\""}
	if got, want := e.Error(), `policy.json:14:22: ops[2] (id "tf"): unknown field "on_zero_mtach"`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// Not op-scoped: OpIndex -1 drops the op clause entirely.
	e = &policy.Error{File: "policy.json", Line: 2, Col: 3, OpIndex: -1, Msg: "missing required field \"version\""}
	if got, want := e.Error(), `policy.json:2:3: missing required field "version"`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// An op with no id still names its index.
	e = &policy.Error{File: "policy.json", Line: 5, Col: 7, OpIndex: 0, Msg: "boom"}
	if got, want := e.Error(), `policy.json:5:7: ops[0]: boom`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// A MultiError prints every problem, one per line.
	m := &policy.MultiError{Errs: []error{
		&policy.Error{File: "policy.json", Line: 1, OpIndex: -1, Msg: "a"},
		&policy.Error{File: "policy.json", Line: 2, OpIndex: -1, Msg: "b"},
	}}
	if got, want := m.Error(), "policy.json:1: a\npolicy.json:2: b"; got != want {
		t.Errorf("MultiError.Error() = %q, want %q", got, want)
	}
}

// SPEC R-20 (error quality): a real multi-line policy must report the line the
// mistake is actually on. Line 0 — the value you get by not converting the byte
// offset — is the whole feature failing quietly.
func TestR20_ErrorCarriesAPlausibleLineNumber(t *testing.T) {
	src := `{
  "version": 1,
  "on_empty": "error",
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    { "id": "tf",
      "op": "add_owner(**/*.tf, @org/infra)",
      "on_zero_mtach": "skip" }
  ]
}`
	err := mustReject(t, src)
	e, ok := findLocated(err, func(e *policy.Error) bool { return e.Line == 8 })
	if !ok {
		t.Fatalf("no problem was reported on line 8, the on_zero_mtach line; a byte offset that was never converted reports 0. Got %T:\n%v", err, err)
	}
	if e.Col <= 0 {
		t.Errorf("Col = %d, want a 1-based column", e.Col)
	}
	if e.File != testFile {
		t.Errorf("File = %q, want %q — Parse's filename argument exists for exactly this", e.File, testFile)
	}
	if !strings.Contains(err.Error(), testFile+":8:") {
		t.Errorf("rendered error = %q, want it to locate the problem at %q", err.Error(), testFile+":8:")
	}
}

// SPEC R-20 (error quality): the op is identified by index AND id. The index
// alone makes a reviewer count array elements; the id alone is optional and may
// be absent. Both, always.
//
// When the op has no id the index still stands alone, and no id is invented to
// fill the gap: `ops[1]` is how the renderer refers to an unnamed op, and an
// error claiming an op is named "ops[1]" sends a reviewer searching the policy
// file for a string that is not in it.
func TestR20_ErrorsIdentifyTheOpByIndexAndID(t *testing.T) {
	err := mustReject(t, `{"version":1,"ops":[
  "add_owner(/a/, @a)",
  "add_owner(/b/, @b)",
  {"id":"tf","op":"add_owner(**/*.tf, @org/infra)","on_zero_match":"nope"}
]}`)
	e, ok := findLocated(err, func(e *policy.Error) bool { return e.OpIndex == 2 })
	if !ok {
		t.Fatalf("no problem was attributed to ops[2], the only op with a mistake in it; got %T:\n%v", err, err)
	}
	if e.OpID != "tf" {
		t.Errorf("OpID = %q, want %q", e.OpID, "tf")
	}
	assertMentions(t, err, "ops[2]", `id "tf"`)

	// An op with no id still reports its index, and reports no phantom id.
	err = mustReject(t, `{"version":1,"ops":["add_owner(/a/, @a)",{"op":"add_owner(/b/, @b)","on_zero_match":"nope"}]}`)
	e, ok = findLocated(err, func(e *policy.Error) bool { return e.OpIndex == 1 })
	if !ok {
		t.Fatalf("no problem was attributed to ops[1], the only op with a mistake in it; got %T:\n%v", err, err)
	}
	if e.OpID != "" {
		t.Errorf("OpID = %q, want \"\" — the op has no id, and `ops[1]` is the display label, never a stored value", e.OpID)
	}
	assertMentions(t, err, "ops[1]")
}

// SPEC R-20: SEMANTIC errors accumulate. Fixing a generated 40-op policy one
// error per run is miserable and is how an operator gives up and stops running
// `check` at all.
func TestR20_SemanticErrorsAccumulate(t *testing.T) {
	err := mustReject(t, `{"version":1,"ops":[
  {"id":"a","op":"add_owner(/x/, @a)","on_zero_match":"SKIP"},
  {"id":"b","op":"add_owner(/y/, @b)","on_zero_match":"wrote"},
  {"id":"c","op":"add_owner(/z/, @c)","on_zero_match":"write"}
]}`)
	var multi *policy.MultiError
	if !errors.As(err, &multi) {
		t.Fatalf("three independent semantic errors must arrive as one *MultiError, got %T: %v", err, err)
	}
	if len(multi.Errs) != 3 {
		t.Errorf("MultiError carries %d errors, want 3:\n%s", len(multi.Errs), err.Error())
	}
	assertMentions(t, err, `id "a"`, `id "b"`, `id "c"`, "SKIP", "wrote", "write")
	assertNoGoInternals(t, err)

	// Errors of different kinds accumulate together, not just repeats of one.
	err = mustReject(t, `{"version":1,"ops":[
  {"id":"a","op":"add_owner(/x/, @a)","on_zero_match":"SKIP"},
  {"id":"b","op":"rename_owner(@o, @n)","on_zero_match":"skip"},
  {"id":"c","op":"remove_owner(/z/, @c)","on_zero_match":"declare"},
  {"id":"d","op":"add_ownr(/w/, @d)"}
]}`)
	if !errors.As(err, &multi) || len(multi.Errs) < 4 {
		t.Errorf("want at least 4 accumulated problems (bad enum, rename legality, declare-on-remove, bad op string), got %T with %d:\n%v",
			err, len(flatten(err)), err)
	}
}

// SPEC R-20: SYNTAX errors fail fast. Once the token stream is broken every
// subsequent "error" is invented, and a wall of phantom problems is worse than
// the one real one.
func TestR20_SyntaxErrorsFailFast(t *testing.T) {
	cases := map[string]string{
		"truncated":      `{"version":1,"ops":[`,
		"trailing comma": `{"version":1,"ops":["add_owner(/x/, @a)",]}`,
		"unquoted key":   `{version:1,"ops":["add_owner(/x/, @a)"]}`,
		"not an object":  `["add_owner(/x/, @a)"]`,
		"empty input":    ``,
		"garbage":        `not json at all`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			err := mustReject(t, src)
			assertNoGoInternals(t, err)
			if n := len(flatten(err)); n != 1 {
				t.Errorf("syntax errors must fail fast with exactly 1 problem, got %d:\n%s", n, err.Error())
			}
			if len(located(err)) != 1 {
				t.Fatalf("a syntax error is still a located *policy.Error, got %T: %v", err, err)
			}
			if !strings.HasPrefix(err.Error(), testFile+":") {
				t.Errorf("a syntax error must still name the file it came from; got:\n%s", err.Error())
			}
		})
	}
}

// SPEC R-20 (error quality, rule 5): `json: cannot unmarshal object into Go
// value of type string` names Go types and points at the wrong concept. It must
// never reach an operator, for ANY malformed input.
func TestR20_GoUnmarshalMessagesNeverEscape(t *testing.T) {
	cases := map[string]string{
		"op is a number":            `{"version":1,"ops":[{"op":5}]}`,
		"op is an object":           `{"version":1,"ops":[{"op":{"kind":"add_owner"}}]}`,
		"id is a number":            `{"version":1,"ops":[{"id":7,"op":"add_owner(/x/, @a)"}]}`,
		"on_zero_match is a number": `{"version":1,"ops":[{"op":"add_owner(/x/, @a)","on_zero_match":3}]}`,
		"note is an array":          `{"version":1,"ops":[{"op":"add_owner(/x/, @a)","note":["a"]}]}`,
		"name is an object":         `{"version":1,"name":{},"ops":["add_owner(/x/, @a)"]}`,
		"on_empty is a bool":        `{"version":1,"on_empty":true,"ops":["add_owner(/x/, @a)"]}`,
		"version is a string":       `{"version":"1","ops":["add_owner(/x/, @a)"]}`,
		"ops is an object":          `{"version":1,"ops":{"0":"add_owner(/x/, @a)"}}`,
		"entry is a number":         `{"version":1,"ops":[3]}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			err := mustReject(t, src)
			assertNoGoInternals(t, err)
			if !strings.HasPrefix(err.Error(), testFile+":") {
				t.Errorf("every policy error must lead with the file it came from; got:\n%s", err.Error())
			}
		})
	}
}

// SPEC R-20: Load is Parse over a file on disk — same policy, same result. The
// filename in the errors is the path the operator passed, so they can open it.
func TestR20_LoadReadsAPolicyFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	src := `{
  "version": 1,
  "name": "org baseline ownership",
  "on_empty": "error",
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    { "id": "tf", "op": "add_owner(**/*.tf, @org/infra)", "on_zero_match": "skip", "note": "opportunistic" }
  ]
}`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := policy.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	parsed, err := policy.Parse([]byte(src), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(loaded, parsed) {
		t.Errorf("Load = %+v, want the same policy as Parse = %+v", loaded, parsed)
	}
	if loaded.Ops[1].ID != "tf" || loaded.Notes["tf"] != "opportunistic" {
		t.Errorf("loaded policy lost per-op detail: %+v / %#v", loaded.Ops, loaded.Notes)
	}
}

// SPEC R-20: a missing policy file is a policy error, not a crash and not a
// silent empty policy — the fleet script's first line is `check --policy`, and
// a typo'd path must halt there rather than run zero ops against 100 repos.
func TestR20_LoadOfMissingPathFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	p, err := policy.Load(path)
	if err == nil {
		t.Fatalf("Load of a nonexistent path must fail, got %+v", p)
	}
	if p != nil {
		t.Errorf("Load must return a nil policy on error, got %+v", p)
	}
	assertMentions(t, err, "nope.json")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the OS error must stay wrapped so callers can errors.Is it; got: %v", err)
	}
}

// SPEC R-20: the policy is the unit of review, and reviewing it touches NO
// repository. Every error above is reachable with nothing on disk but the file
// itself — no git, no tree, no CODEOWNERS — which is what lets `check` run once
// before the loop instead of failing on repo 100.
func TestR20_MalformedPolicyIsCaughtWithoutARepo(t *testing.T) {
	dir := t.TempDir() // not a git repo; no CODEOWNERS anywhere
	path := filepath.Join(dir, "policy.json")
	src := `{
  "version": 1,
  "ops": [
    { "id": "tf", "op": "add_owner(**/*.tf, @org/infra)", "on_zero_mtach": "skip" }
  ]
}`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	err := func() error {
		_, err := policy.Load(path)
		return err
	}()
	if err == nil {
		t.Fatal("the typo must be caught from the file alone")
	}
	assertMentions(t, err, "on_zero_mtach", path)
	assertNoGoInternals(t, err)

	// And a good policy validates just as well with no repo in sight.
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"version":1,"ops":["add_owner(/ghost/does/not/exist/, @a)"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.Load(good); err != nil {
		t.Errorf("a scope no repo contains is a per-repo fact (exit 2), not a policy error: %v", err)
	}
}
