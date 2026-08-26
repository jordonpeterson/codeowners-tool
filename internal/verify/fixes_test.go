package verify_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/verify"
)

// SPEC R-18: a before/after pair with no tracked path in common is refused,
// because nothing in it was compared. Every row of such a pair is a tree
// delta, and R-18 says a tree delta is never a violation, so the run can only
// ever report `ok` — the vacuous green a fleet loop with one wrong filename
// gets on every repository it visits.
func TestCompareRefusesAPairWithNoPathInCommon(t *testing.T) {
	before := snap(map[string][]string{".github/CODEOWNERS": {"@org/every"}, "services/api/api.go": {"@org/api"}})
	after := snap(map[string][]string{"CODEOWNERS": {"@org/other"}, "lib/thing.rb": {"@org/other"}})

	res, err := verify.Compare(before, after, nil)
	if err == nil {
		t.Fatalf("two snapshots sharing zero paths must be refused, got result %+v", res)
	}
	if res != nil {
		t.Errorf("a refused comparison must return no result, got %+v", res)
	}
	// The counts are the evidence; without them the operator cannot tell a
	// swapped filename from an empty snapshot.
	if !strings.Contains(err.Error(), "2 path") {
		t.Errorf("the refusal must report how many paths each side has: %v", err)
	}
}

// The conservative boundary: ONE shared path is enough to compare, and the
// pair is accepted. A false refusal here breaks the documented CI gate on
// every repository whose branch renamed most of the tree, so the guard fires
// only when the intersection is genuinely empty.
func TestCompareAcceptsAPairSharingASinglePath(t *testing.T) {
	before := snap(map[string][]string{"keep.go": {"@a"}, "gone1.go": {"@a"}, "gone2.go": {"@a"}})
	after := snap(map[string][]string{"keep.go": {"@a"}, "new1.go": {"@a"}, "new2.go": {"@a"}})

	res, err := verify.Compare(before, after, nil)
	if err != nil {
		t.Fatalf("one path in common is enough to compare: %v", err)
	}
	if !res.OK() {
		t.Errorf("nothing changed on the shared path: %+v", res.Violations)
	}
	if len(res.Added) != 2 || len(res.Removed) != 2 {
		t.Errorf("the tree deltas must still surface: %+v / %+v", res.Added, res.Removed)
	}
}

// The `repo` field is whatever path the operator passed to `snapshot`, so two
// snapshots of one repository taken from two clones (CI checks main and the
// feature branch into separate directories) carry different `repo` strings.
// Identity is the tree they describe, never that string.
func TestCompareIgnoresTheRepoFieldWhenTheTreesMatch(t *testing.T) {
	before := &verify.Snapshot{Repo: "/build/main-checkout", Ref: "main",
		Ownership: map[string][]string{"a.go": {"@a"}}}
	after := &verify.Snapshot{Repo: "/build/pr-checkout", Ref: "feature",
		Ownership: map[string][]string{"a.go": {"@a"}}}

	res, err := verify.Compare(before, after, nil)
	if err != nil {
		t.Fatalf("differing --repo paths are not a mismatched pair: %v", err)
	}
	if !res.OK() {
		t.Errorf("identical ownership must verify clean: %+v", res.Violations)
	}
}

// SPEC R-18: a path git stores as bytes that are not valid UTF-8 round-trips
// through the snapshot exactly. encoding/json folds every such byte to
// U+FFFD, so `a\xe9.md` and `a\xff.md` used to be written as the same key
// twice and one tracked file vanished from the gate.
func TestSnapshotJSONRoundTripsNonUTF8Paths(t *testing.T) {
	orig := &verify.Snapshot{Ownership: map[string][]string{
		".github/CODEOWNERS": {"@org/every"},
		"a\xe9.md":           {"@org/one"},
		"a\xff.md":           nil,
		"a%E9.md":            {"@org/literal"}, // the escaped spelling, as a real ASCII file
		// A literal % INSIDE a path that is already being escaped: without
		// the %25 rule these two tracked files both encode to
		// `bin/a%FF.md%E9` and one of them is lost again.
		"bin/a\xff.md\xe9": {"@org/pct"},
		"bin/a%FF.md\xe9":  {"@org/pct-literal"},
	}}
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// encoding/json writes the replacement rune escaped, so check both spellings.
	if strings.Contains(string(b), "�") || strings.Contains(string(b), `\ufffd`) {
		t.Errorf("no path byte may be folded to U+FFFD:\n%s", b)
	}

	var got verify.Snapshot
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	if !reflect.DeepEqual(map[string][]string(got.Ownership), map[string][]string(orig.Ownership)) {
		t.Errorf("round trip lost or changed a path\n got: %#v\nwant: %#v\njson: %s",
			got.Ownership, orig.Ownership, b)
	}
}

// The encoding is applied ONLY to paths that are not valid UTF-8: every other
// key, including a literal percent escape and a multi-byte rune, keeps the
// exact bytes the plain encoder produced, so an existing consumer of an
// ordinary snapshot sees no difference.
func TestSnapshotJSONLeavesValidUTF8KeysByteIdentical(t *testing.T) {
	own := map[string][]string{
		"a%E9.md":   {"@a"},
		"café.md":   {"@a"},
		"a&b<c>.md": nil,
		"x/y.md":    {},
	}
	want, err := json.Marshal(own)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(verify.Snapshot{Ownership: own})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), string(want)) {
		t.Errorf("the ownership object must be byte-identical to the plain encoder's\n got: %s\nwant: %s", got, want)
	}
}

// A snapshot whose escaped keys decode to the same path is malformed: keeping
// either one silently would lose a tracked path, which is the defect this
// encoding exists to prevent.
func TestLoadRejectsEscapedKeysThatCollide(t *testing.T) {
	var s verify.Snapshot
	err := json.Unmarshal([]byte(`{"ownership":{"\u0000a%E9.md":["@a"],"\u0000a%e9.md":["@b"]}}`), &s)
	if err == nil {
		t.Fatalf("two keys decoding to one path must be an error, got %#v", s.Ownership)
	}
}
