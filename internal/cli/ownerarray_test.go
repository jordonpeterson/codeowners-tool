// End-to-end tests for R-39: `owners` as a JSON array on a policy op.
//
// R-33 gave every owner-naming verb a bracketed list inside the op STRING.
// R-37 gave `except` a JSON array beside the op string. R-39 closes the
// asymmetry those two left: a generator emitting a policy can hand `except`
// to the JSON encoder but has to string-build `[@a, @b]` for owners, quoting
// and comma-joining by hand in the one field where a mistake grants the wrong
// team. The array is the same fact as the list, so it is validated, applied
// and REPORTED as the `(scope, [owners])` spelling it is equivalent to.
//
//	{"op": "add_owner(/x/)", "owners": ["@a", "@b"]}
//	{"op": "add_owner(/x/, [@a, @b])"}
//
// Written ahead of the implementation per CONTRIBUTING.md. Vacuity has four
// sources here, and every assertion below is shaped against them:
//
//   - Today `"owners"` on an op is an UNKNOWN FIELD, which is exit 3 — the
//     same code most negative cases expect. Every negative case therefore
//     also asserts a message fragment today's unknown-field error does not
//     contain: either the requirement id "R-39", or the string spelling's own
//     diagnosis of the same defect.
//   - `policy.Error` renders the FILE first, so a fixture named owners.json
//     would make "the message mentions owners" true by accident. Every policy
//     here is written by oaPolicy, which names it p.json.
//   - `t.TempDir()` embeds the test's own name in the path, and the path is in
//     the message. No test below is named with a fragment any assertion looks
//     for — in particular the requirement id is spelled "R-39" in assertions
//     and "R39" in test names.
//   - The word "owners" appears inside `set_owners` and inside `field
//     "owners"` alike, so a bare "owners" fragment proves nothing. Assertions
//     quote the field (`"owners"`) or cite the requirement.
//
// Where the array has a defect the string spelling already diagnoses — a
// duplicate owner, an invalid token, an empty list on add_owner — the fragment
// asserted is the STRING spelling's own message. R-39a says the two spellings
// are equivalent in every respect, so an array that fails differently from the
// list it is equivalent to is a defect, not a wording choice.
//
// Because the array is re-spelled as the bracketed list before anything else
// sees it, an array and the LIST it is equivalent to produce byte-identical op
// strings — so unlike R-37 these tests can compare the WHOLE per-op record,
// `op` field included, rather than redacting it. That equality is the
// requirement: it is what makes `results.jsonl` from an array-spelled wave
// greppable by the same tooling as a hand-written one. The one exception is
// the BARE single-owner spelling, which R-33a already declares to be the same
// op as `[@a]`; that test compares outcomes rather than spelling, and says so.
//
// Mutating tests assert EXACT file bytes. Under last-match-wins (S-1) a
// strings.Contains check over file content is satisfied by a file whose line
// ORDER hands the scope to the wrong owners — and every claim here about a
// grant, a carve or a displacement is a claim about that order.
//
// Three subtests are pins that pass today and are labeled as such: the
// scope-only op string with no array is still an arity error, `set_owners`
// still requires its brackets in the string spelling, and the string list is
// unchanged. They freeze behavior R-39 promises not to disturb; everything
// else fails until the feature lands.
package cli_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// oaPolicy writes a policy to a neutrally named file. p.json and not
// owners.json on purpose: policy.Error prints the file name first, so a
// fixture named after the feature satisfies a keyword assertion on a binary
// that does not implement it (CONTRIBUTING.md).
func oaPolicy(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// oaFiles is the fixture every equivalence test runs against: a catch-all, one
// narrower rule with an owner no policy below names, content on both sides of
// every scope so INV-2 has something to protect, and a generated file that
// gives `except` something real to carve.
func oaFiles() map[string]string {
	return map[string]string{
		"CODEOWNERS":             "* @org/original\n/services/api/ @org/api-team @org/legacy\n",
		"services/api/main.go":   "package api\n",
		"services/api/gen.pb.go": "package api\n",
		"services/web/app.js":    "//\n",
		"docs/guide.md":          "guide\n",
	}
}

func oaRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, oaFiles())
}

// oaSyncOK runs one policy to completion and returns the raw record and the
// written bytes. The record is decoded into a map, not cli.SyncRecord: these
// tests must compile — and fail — against a binary that has never heard of the
// array.
func oaSyncOK(t *testing.T, repo, src string, extra ...string) (map[string]any, string) {
	t.Helper()
	args := append([]string{"sync", "--repo", repo, "--policy", oaPolicy(t, src), "--format", "json"}, extra...)
	code, out, errOut := runCLI(t, args...)
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	return exceptDecodeRaw(t, out), syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))
}

// oaEquivalent is the assertion R-39a is made of, run against a fresh copy of
// the same fixture for each spelling: identical bytes, identical per-op record
// (including the echoed `op` string), identical changes[].
//
// The literal `want` is asserted too, so two runs that are equally wrong cannot
// pass each other. A differential assertion alone would be satisfied by an
// implementation that ignored the owners entirely in both spellings.
func oaEquivalent(t *testing.T, files map[string]string, arraySrc, stringSrc, want string) {
	t.Helper()
	run := func(src string) (map[string]any, string) {
		return oaSyncOK(t, initRepo(t, files), src)
	}
	arrayRec, arrayBytes := run(arraySrc)
	stringRec, stringBytes := run(stringSrc)

	if arrayBytes != want {
		t.Errorf("array spelling wrote:\n%s\nwant:\n%s", arrayBytes, want)
	}
	if arrayBytes != stringBytes {
		t.Errorf("the two spellings wrote different files\n array:\n%s\n string:\n%s", arrayBytes, stringBytes)
	}
	oaSame(t, "changes[]", arrayRec["changes"], stringRec["changes"])
	oaSame(t, "ops[0]", exceptOp0(t, arrayRec), exceptOp0(t, stringRec))
}

// oaSame reports a difference between the two spellings' recorded facts.
// "Equivalent in every respect" means the records compare equal, not that both
// look plausible.
func oaSame(t *testing.T, what string, array, str any) {
	t.Helper()
	if !reflect.DeepEqual(array, str) {
		t.Errorf("%s differs between spellings; R-39a promises the array is the list it is equivalent to\n  array:  %#v\n  string: %#v", what, array, str)
	}
}

// oaWantPolicyError is the exit-3 shape: `check` dies on the artifact with no
// repository open at all, `sync` dies identically and writes nothing. Both
// halves matter to a fleet — exit 3 from `check` is what stops the rollout
// before repo 1, and "wrote nothing" is what makes a mid-wave abort safe.
func oaWantPolicyError(t *testing.T, src, fragment string) {
	t.Helper()
	pol := oaPolicy(t, src)
	code, _, errOut := runCLI(t, "check", "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	oaWantFragment(t, "check stderr", errOut, fragment)

	repo := oaRepo(t)
	snap := checkDirSnapshot(t, repo)
	code, _, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("sync: want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	oaWantFragment(t, "sync stderr", errOut, fragment)
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("a policy error must write NOTHING; repo changed")
	}
}

func oaWantFragment(t *testing.T, where, got, fragment string) {
	t.Helper()
	if !strings.Contains(got, fragment) {
		t.Errorf("%s must contain %q — the operator has to be able to tell this refusal from every other one.\ngot:\n%s",
			where, fragment, got)
	}
}

func oaWantFile(t *testing.T, path, want string) {
	t.Helper()
	if got := syncReadFile(t, path); got != want {
		t.Errorf("file bytes:\n%s\nwant:\n%s", got, want)
	}
}

// ---------- R-39a: the array IS the bracketed list ----------

// SPEC R-39a: the array spelling and the list spelling of one grant produce
// the same file, byte for byte, and the same record — the echoed `op` string
// included, because the array is re-spelled as the list before anything else
// sees it. That echo is not cosmetic: `results.jsonl` from a hundred repos is
// grepped and aggregated by op string, and an array-spelled wave whose records
// said `add_owner(/services/api/)` would report a grant naming nobody.
func TestR39a_ArrayAndListSpellingsAreIndistinguishable(t *testing.T) {
	oaEquivalent(t, oaFiles(),
		`{"version":1,"ops":[{"id":"g","op":"add_owner(/services/api/)","owners":["@org/x","@org/y"]}]}`,
		`{"version":1,"ops":[{"id":"g","op":"add_owner(/services/api/, [@org/x, @org/y])"}]}`,
		"* @org/original\n/services/api/ @org/api-team @org/legacy @org/x @org/y\n")
}

// SPEC R-39a: the array reaches every verb that names owners, not just
// add_owner. A feature that carved only for add_owner would push generators
// straight back to string-building for the two verbs where a mistake is
// destructive — `set_owners` displaces, `remove_owner` revokes.
//
// Each case asserts the whole outcome, not just that something changed:
// set_owners must drop @org/api-team (that is the difference from add_owner),
// and remove_owner must leave the line with the survivors and not delete it.
func TestR39a_ArrayReachesEveryOwnerNamingVerb(t *testing.T) {
	cases := []struct {
		name        string
		array, list string
		want        string
	}{
		{
			name:  "add_owner co-owns, keeping the owner the policy never names",
			array: `{"version":1,"ops":[{"id":"o","op":"add_owner(/services/api/)","owners":["@org/x","@org/y"]}]}`,
			list:  `{"version":1,"ops":[{"id":"o","op":"add_owner(/services/api/, [@org/x, @org/y])"}]}`,
			want:  "* @org/original\n/services/api/ @org/api-team @org/legacy @org/x @org/y\n",
		},
		{
			name:  "set_owners displaces every prior owner",
			array: `{"version":1,"ops":[{"id":"o","op":"set_owners(/services/api/)","owners":["@org/x","@org/y"]}]}`,
			list:  `{"version":1,"ops":[{"id":"o","op":"set_owners(/services/api/, [@org/x, @org/y])"}]}`,
			want:  "* @org/original\n/services/api/ @org/x @org/y\n",
		},
		{
			name:  "remove_owner revokes, leaving the survivors on the line",
			array: `{"version":1,"on_empty":"error","ops":[{"id":"o","op":"remove_owner(/services/api/)","owners":["@org/legacy"]}]}`,
			list:  `{"version":1,"on_empty":"error","ops":[{"id":"o","op":"remove_owner(/services/api/, [@org/legacy])"}]}`,
			want:  "* @org/original\n/services/api/ @org/api-team\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oaEquivalent(t, oaFiles(), tc.array, tc.list, tc.want)
		})
	}
}

// SPEC R-39a/R-33a: a one-element array is the bare single-owner form, byte for
// byte in the FILE. R-33a made that promise for `[@a]` inside the op string; a
// generator that emits an array of length one for a one-owner intent — which
// every generator does, because the length is data — must not produce a
// different file from a human typing `@a`.
//
// The echoed op string is the one thing that legitimately differs: the array is
// always re-spelled as a list, so it reports itself as `add_owner(/docs/,
// [@org/x])` where the hand-written op says `add_owner(/docs/, @org/x)`. R-33a
// is precisely the statement that those two are the same op, so the assertion
// here is over the bytes, the changes and the op's outcome — never over the
// spelling. Asserting spelling equality would force a one-element special case
// into the re-speller, which is a rule that buys nothing and one more place for
// the two spellings to diverge.
func TestR39a_SingleElementArrayEqualsTheBareForm(t *testing.T) {
	const want = "* @org/original\n/docs/ @org/original @org/x\n/services/api/ @org/api-team @org/legacy\n"

	arrayRec, arrayBytes := oaSyncOK(t, oaRepo(t), `{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/)","owners":["@org/x"]}]}`)
	bareRec, bareBytes := oaSyncOK(t, oaRepo(t), `{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/, @org/x)"}]}`)

	if arrayBytes != want {
		t.Errorf("array spelling wrote:\n%s\nwant:\n%s", arrayBytes, want)
	}
	if arrayBytes != bareBytes {
		t.Errorf("a one-element array wrote a different file from the bare owner\n array:\n%s\n bare:\n%s", arrayBytes, bareBytes)
	}
	oaSame(t, "changes[]", arrayRec["changes"], bareRec["changes"])

	arrayOp, bareOp := exceptOp0(t, arrayRec), exceptOp0(t, bareRec)
	for _, field := range []string{"id", "status", "proven"} {
		oaSame(t, "ops[0]."+field, arrayOp[field], bareOp[field])
	}
	if got, want := arrayOp["op"], "add_owner(/docs/, [@org/x])"; got != want {
		t.Errorf("ops[0].op = %v, want %q — one element is still spelled as a list", got, want)
	}
}

// SPEC R-39a/R-33e: array order fixes the APPEND order in the written line and
// nothing else. The bytes differ between two orders — that is what "order is
// preserved" means, and a writer that sorted would violate it — while the
// resolution is identical, because owners are a set to GitHub.
//
// The oracle for the second half is `snapshot`, not the file: a test that
// asserted only bytes would pass against an implementation that wrote the
// owners in a line the resolver never reaches.
func TestR39a_ArrayOrderFixesAppendOrderNotResolution(t *testing.T) {
	forward := oaRepo(t)
	reverse := oaRepo(t)
	_, fwdBytes := oaSyncOK(t, forward, `{"version":1,"ops":[{"id":"g","op":"add_owner(/services/api/)","owners":["@org/x","@org/y"]}]}`)
	_, revBytes := oaSyncOK(t, reverse, `{"version":1,"ops":[{"id":"g","op":"add_owner(/services/api/)","owners":["@org/y","@org/x"]}]}`)

	if fwdBytes != "* @org/original\n/services/api/ @org/api-team @org/legacy @org/x @org/y\n" {
		t.Errorf("forward order wrote:\n%s", fwdBytes)
	}
	if revBytes != "* @org/original\n/services/api/ @org/api-team @org/legacy @org/y @org/x\n" {
		t.Errorf("reverse order wrote:\n%s", revBytes)
	}
	if got, want := exceptOwnership(t, reverse), exceptOwnership(t, forward); !reflect.DeepEqual(got, want) {
		t.Errorf("array order changed RESOLUTION; it may only change append order (R-33e)\n got:  %v\n want: %v", got, want)
	}
}

// SPEC R-39a: the array is idempotent and adds only what is missing, exactly as
// the list is. The second run of a converged policy must be byte-identical —
// the property a fleet depends on to tell "already done" from "changed", and
// the one an implementation that appends the whole array unconditionally
// breaks on run two.
func TestR39a_ArrayIsIdempotentAndAddsOnlyWhatIsMissing(t *testing.T) {
	const src = `{"version":1,"ops":[{"id":"g","op":"add_owner(/services/api/)","owners":["@org/api-team","@org/x"]}]}`

	t.Run("adds only the missing owner", func(t *testing.T) {
		repo := oaRepo(t)
		oaSyncOK(t, repo, src)
		oaWantFile(t, filepath.Join(repo, "CODEOWNERS"), "* @org/original\n/services/api/ @org/api-team @org/legacy @org/x\n")
	})
	t.Run("second run is byte-identical and reports unchanged", func(t *testing.T) {
		repo := oaRepo(t)
		_, first := oaSyncOK(t, repo, src)
		rec, second := oaSyncOK(t, repo, src)
		if second != first {
			t.Errorf("second run changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
		}
		if status, _ := rec["status"].(string); status != string(cli.StatusUnchanged) {
			t.Errorf("status = %q, want %q — a converged repo must be distinguishable from one this run wrote to", status, cli.StatusUnchanged)
		}
	})
}

// ---------- R-39a: the array is validated as the list it becomes ----------

// SPEC R-39a: every refusal the bracketed list makes is the array's refusal
// too, in the STRING spelling's own words. A second copy of these checks
// beside the array is how the two would drift — the array would still accept a
// duplicate owner a year after the list stopped, and `verify` would report a
// rollback-worthy invariant violation over a policy `check` called clean.
//
// The duplicate cases are the ones that need both spellings asserted: an
// owner named twice is a fact about the TEXT, so it is exit 3 on every
// repository rather than a per-repo refusal, and case-variant handles are one
// owner under R-38a because GitHub folds them.
func TestR39a_TheArrayInheritsEveryListRefusal(t *testing.T) {
	cases := []struct {
		name, src, fragment string
	}{
		{
			name:     "owner named twice",
			src:      `{"version":1,"ops":[{"op":"add_owner(/docs/)","owners":["@org/x","@org/x"]}]}`,
			fragment: `duplicate owner "@org/x" in one add_owner list`,
		},
		{
			name:     "one owner spelled two ways is still one owner",
			src:      `{"version":1,"ops":[{"op":"add_owner(/docs/)","owners":["@Org/X","@org/x"]}]}`,
			fragment: `are one owner in the same add_owner list`,
		},
		{
			name:     "duplicate reaches set_owners too",
			src:      `{"version":1,"ops":[{"op":"set_owners(/docs/)","owners":["@org/x","@org/x"]}]}`,
			fragment: `duplicate owner "@org/x" in one set_owners list`,
		},
		{
			name:     "not an owner token",
			src:      `{"version":1,"ops":[{"op":"add_owner(/docs/)","owners":["@org/x","not-an-owner"]}]}`,
			fragment: `invalid owner token "not-an-owner"`,
		},
		{
			name:     "a bare @ names no identity",
			src:      `{"version":1,"ops":[{"op":"add_owner(/docs/)","owners":["@"]}]}`,
			fragment: `invalid owner token "@"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oaWantPolicyError(t, tc.src, tc.fragment)
		})
	}
}

// SPEC R-39a/R-33d: an empty array follows the VERB, exactly as an empty list
// does. `set_owners(scope, [])` is how "nobody owns this" is spelled and stays
// legal; an empty add or remove states no intent at all and is exit 3.
//
// This is the one place the owners array must NOT copy R-37d's blanket refusal
// of an empty `except` array: emptiness means something here. A generator that
// computes an owner set and finds it empty is making a statement when the verb
// is set_owners and has a bug when it is not.
func TestR39a_EmptyArrayFollowsTheVerb(t *testing.T) {
	t.Run("set_owners un-owns the scope", func(t *testing.T) {
		oaEquivalent(t, oaFiles(),
			`{"version":1,"ops":[{"id":"z","op":"set_owners(/services/api/)","owners":[]}]}`,
			`{"version":1,"ops":[{"id":"z","op":"set_owners(/services/api/, [])"}]}`,
			"* @org/original\n/services/api/\n")
	})
	t.Run("add_owner states no intent", func(t *testing.T) {
		oaWantPolicyError(t,
			`{"version":1,"ops":[{"op":"add_owner(/docs/)","owners":[]}]}`,
			"add_owner has an empty owner list")
	})
	t.Run("remove_owner states no intent", func(t *testing.T) {
		oaWantPolicyError(t,
			`{"version":1,"on_empty":"error","ops":[{"op":"remove_owner(/docs/)","owners":[]}]}`,
			"remove_owner has an empty owner list")
	})
}

// SPEC R-39b: one intent, one place. An op naming owners in its op string AND
// in an `owners` array is exit 3 before any repo is read — a policy whose two
// halves might disagree must never reach a decision about which one wins.
// Silently preferring either one grants a team somebody reviewed out, or drops
// one they reviewed in, in a diff where both spellings are visible and neither
// is marked dead.
//
// Both string spellings are covered: the bare owner and the bracketed list.
// An implementation that detected the conflict by "does the op string parse"
// catches the first and misses neither — but one that looked for a `[` catches
// only the second.
//
// The fragment is the requirement id: today this policy dies with `unknown
// field "owners"`, which contains the word owners and the word field, so any
// obvious semantic fragment passes vacuously against a binary that does not
// implement the array at all.
func TestR39b_OwnersInBothPlacesIsExit3(t *testing.T) {
	t.Run("bare owner in the op string", func(t *testing.T) {
		oaWantPolicyError(t,
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/, @org/x)","owners":["@org/y"]}]}`,
			"R-39")
	})
	t.Run("bracketed list in the op string", func(t *testing.T) {
		oaWantPolicyError(t,
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/, [@org/x])","owners":["@org/y"]}]}`,
			"R-39")
	})
	t.Run("even when the two halves agree", func(t *testing.T) {
		// Agreement is not a defence: the next generator run changes one half
		// and not the other, and a policy that was legal while they matched
		// silently becomes a policy nobody wrote.
		oaWantPolicyError(t,
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/, [@org/x])","owners":["@org/x"]}]}`,
			"R-39")
	})
}

// SPEC R-39c: `rename_owner` takes no `owners` array, in the same way and for
// the same reason it takes no list (R-33f) and no `except` (R-27.4): it names
// one owner and one replacement, both required, neither a set. Falling through
// to a message about arity or about a list nobody wrote sends the operator
// hunting a typo instead of learning the verb takes no array.
func TestR39c_RenameOwnerTakesNoOwnersArray(t *testing.T) {
	t.Run("alongside both arguments", func(t *testing.T) {
		oaWantPolicyError(t,
			`{"version":1,"ops":[{"op":"rename_owner(@org/old, @org/new)","owners":["@org/x"]}]}`,
			"R-39")
	})
	t.Run("in place of the second argument", func(t *testing.T) {
		oaWantPolicyError(t,
			`{"version":1,"ops":[{"op":"rename_owner(@org/old)","owners":["@org/new"]}]}`,
			"R-39")
	})
}

// SPEC R-39d: an element is one owner token, and the array is not a delimited
// string — so an element carrying a character the op string uses as structure
// is refused in the operator's terms rather than silently read as two owners.
//
// Only an email owner can reach this: `handleRe` admits none of these
// characters, while `emailRe` is `[^@\s]+@[^@\s]+\.[^@\s]+` and admits a comma
// and a bracket. `a,b@x.com` spliced into an op string re-splits into the two
// owners `a` and `b@x.com`, and `a]b@x.com` closes the list early — one input
// landing on two identities, in the field where that is a grant to a stranger.
//
// The control matters as much as the refusals: an ordinary email owner is
// legal in the array (R-13), and an implementation that refused every email to
// be safe would break the one owner form GitHub allows for individuals.
func TestR39d_ElementTheOpStringCannotCarryIsExit3(t *testing.T) {
	for _, tc := range []struct{ name, owner string }{
		{"comma", "a,b@x.com"},
		{"closing bracket", "a]b@x.com"},
		{"opening bracket", "a[b@x.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oaWantPolicyError(t,
				`{"version":1,"ops":[{"op":"add_owner(/docs/)","owners":["`+tc.owner+`"]}]}`,
				"R-39")
		})
	}
	t.Run("control: an ordinary email owner is legal", func(t *testing.T) {
		oaEquivalent(t, oaFiles(),
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/)","owners":["dev@example.com","@org/x"]}]}`,
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/, [dev@example.com, @org/x])"}]}`,
			"* @org/original\n/docs/ @org/original dev@example.com @org/x\n/services/api/ @org/api-team @org/legacy\n")
	})
}

// SPEC R-39: the array's SHAPE is checked before its contents. A generator
// that emits a bare string where an array belongs — the most common encoder
// mistake, and one JSON itself will not catch — must not have it read as a
// one-owner array by a decoder being helpful.
//
// The fragment is the requirement id and not `field "owners"`: today's
// unknown-field message is literally `unknown field "owners"`, so an assertion
// on the field name passes against a binary that never implemented the array.
func TestR39_ArrayShapeErrorsAreExit3(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"a bare string", `"@org/x"`},
		{"an object", `{"0":"@org/x"}`},
		{"a number", `7`},
		{"null", `null`},
		{"an element that is not a string", `["@org/x", 7]`},
		{"an element that is empty", `["@org/x", ""]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oaWantPolicyError(t,
				`{"version":1,"ops":[{"op":"add_owner(/docs/)","owners":`+tc.value+`}]}`,
				"R-39")
		})
	}
}

// ---------- R-39: the array under every other setting ----------

// SPEC R-39/R-21: the array changes WHO an op names, never WHETHER it runs. A
// zero-match scope is still disposed of by on_zero_match: require refuses this
// repo naming the SCOPE (the owners are not why it refused), skip is a clean
// no-op that writes nothing at all, and declare writes ONE line carrying every
// owner in the array — the R-33b shape on the path where no tracked file
// exists to fold against.
//
// The declare case is where an implementation that appended owners one at a
// time shows itself: two lines, or one line rewritten twice, in a repository
// where nothing tracked matches and structure is the whole proof (INV-6).
func TestR39_ZeroMatchDisposesTheOpNotTheOwners(t *testing.T) {
	const arr = `"op":"add_owner(/ghost/)","owners":["@org/x","@org/y"]`

	t.Run("require refuses this repo at exit 2", func(t *testing.T) {
		repo := oaRepo(t)
		snap := checkDirSnapshot(t, repo)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy",
			oaPolicy(t, `{"version":1,"ops":[{"id":"g",`+arr+`}]}`))
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2 (this repo needs a human), got %d\nstderr:\n%s", code, errOut)
		}
		oaWantFragment(t, "stderr", errOut, `scope "/ghost/" matches zero tracked files`)
		if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
			t.Errorf("a refusal must write nothing; repo changed")
		}
	})
	t.Run("skip writes nothing and exits 0", func(t *testing.T) {
		repo := oaRepo(t)
		snap := checkDirSnapshot(t, repo)
		oaSyncOK(t, repo, `{"version":1,"ops":[{"id":"g",`+arr+`,"on_zero_match":"skip"}]}`)
		if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
			t.Errorf("a skipped op must write nothing; repo changed")
		}
	})
	t.Run("declare writes one line naming every owner", func(t *testing.T) {
		oaEquivalent(t, oaFiles(),
			`{"version":1,"ops":[{"id":"g",`+arr+`,"on_zero_match":"declare"}]}`,
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/ghost/, [@org/x, @org/y])","on_zero_match":"declare"}]}`,
			"* @org/original\n/services/api/ @org/api-team @org/legacy\n/ghost/ @org/x @org/y\n")
	})
}

// SPEC R-39/R-35: a `defaults` block reaches an array-spelled op exactly as it
// reaches a string-spelled one. The block supplies what an op does not state,
// and an implementation that resolved defaults against the op string before
// the array was folded in would leave array-spelled ops running under R-5's
// require while `check` echoed the default — the resolved echo and the run
// disagreeing, which is the one thing R-35b exists to prevent.
func TestR39_DefaultsBlockReachesAnArraySpelledOp(t *testing.T) {
	repo := oaRepo(t)
	snap := checkDirSnapshot(t, repo)
	src := `{"version":1,"defaults":{"on_zero_match":"skip"},"ops":[
		{"id":"g","op":"add_owner(/ghost/)","owners":["@org/x","@org/y"]}]}`

	code, out, errOut := runCLI(t, "check", "--policy", oaPolicy(t, src), "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	oaWantFragment(t, "check --format json", out, `"on_zero_match":"skip"`)

	oaSyncOK(t, repo, src)
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("the default said skip, so the op must write nothing; repo changed")
	}
}

// SPEC R-39/R-37: the two JSON-shaped features on ONE op. `owners` and
// `except` together are where a hand-written decoder drops one of them, and
// where the order they are folded into the op string matters: owners becomes
// the second argument, except a clause on the first, and an implementation
// that spliced them in the wrong order produces an op string that parses as a
// DIFFERENT op.
//
// Exact bytes assert both halves — the broad grant carries both owners, and
// the carve line restates the excepted path's current owners and sits AFTER
// the line it corrects, so last-match-wins (S-1) resolves the excepted path to
// the owners it had rather than to the grantees.
func TestR39_OwnersAndExceptArraysOnOneOp(t *testing.T) {
	t.Run("both arrays against the all-string spelling", func(t *testing.T) {
		oaEquivalent(t, oaFiles(),
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/services/)","owners":["@org/x","@org/y"],"except":["/services/api/gen.pb.go"]}]}`,
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/services/ except /services/api/gen.pb.go, [@org/x, @org/y])"}]}`,
			"* @org/original\n"+
				"/services/ @org/original @org/x @org/y\n"+
				"/services/api/ @org/api-team @org/legacy @org/x @org/y\n"+
				"/services/api/gen.pb.go @org/api-team @org/legacy\n")
	})
	t.Run("mixed: array owners, string except clause", func(t *testing.T) {
		oaEquivalent(t, oaFiles(),
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/services/ except /services/api/gen.pb.go)","owners":["@org/x","@org/y"]}]}`,
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/services/ except /services/api/gen.pb.go, [@org/x, @org/y])"}]}`,
			"* @org/original\n"+
				"/services/ @org/original @org/x @org/y\n"+
				"/services/api/ @org/api-team @org/legacy @org/x @org/y\n"+
				"/services/api/gen.pb.go @org/api-team @org/legacy\n")
	})
	t.Run("on_except_zero_match reaches an array-spelled op", func(t *testing.T) {
		rec, got := oaSyncOK(t, oaRepo(t),
			`{"version":1,"ops":[{"id":"g","op":"add_owner(/services/)","owners":["@org/x","@org/y"],
			  "except":["/services/ghost/"],"on_except_zero_match":"allow"}]}`)
		if want := "* @org/original\n" +
			"/services/ @org/original @org/x @org/y\n" +
			"/services/api/ @org/api-team @org/legacy @org/x @org/y\n"; got != want {
			t.Errorf("wrote:\n%s\nwant:\n%s", got, want)
		}
		// R-32: the carve that bit nothing has to be REPORTED, or the grant
		// silently covers paths the reviewer believed were carved out.
		op0 := exceptOp0(t, rec)
		if !reflect.DeepEqual(op0["except_unmatched"], []any{"/services/ghost/"}) {
			t.Errorf("ops[0].except_unmatched = %#v, want [/services/ghost/] (R-32)", op0["except_unmatched"])
		}
	})
}

// SPEC R-39/R-8: commutation is decided over every owner the ARRAY names. An
// implementation that folded the array in after ops.StaticConflict ran would
// compare two ops that name no owners at all, find every pair commuting, and
// admit an order-dependent batch — at exit 0, into a hundred repositories,
// which is precisely what R-8 exists to prevent.
//
// The commuting pair is asserted alongside the refusal: an implementation that
// refused every array-carrying batch to be safe would satisfy the first half
// and break every real policy.
func TestR39_R8CommutationSeesEveryOwnerInTheArray(t *testing.T) {
	t.Run("non-commuting pair is exit 3", func(t *testing.T) {
		oaWantPolicyError(t, `{"version":1,"on_empty":"error","ops":[
			{"id":"a","op":"add_owner(/services/api/)","owners":["@org/x","@org/y"]},
			{"id":"r","op":"remove_owner(/services/api/)","owners":["@org/y"]}]}`,
			"R-8")
	})
	t.Run("commuting pair still runs", func(t *testing.T) {
		oaEquivalent(t, oaFiles(),
			`{"version":1,"ops":[
				{"id":"a","op":"add_owner(/services/api/)","owners":["@org/x"]},
				{"id":"b","op":"add_owner(/services/api/)","owners":["@org/y"]}]}`,
			`{"version":1,"ops":[
				{"id":"a","op":"add_owner(/services/api/, [@org/x])"},
				{"id":"b","op":"add_owner(/services/api/, [@org/y])"}]}`,
			"* @org/original\n/services/api/ @org/api-team @org/legacy @org/x @org/y\n")
	})
	t.Run("the conflict is seen across spellings", func(t *testing.T) {
		// One op spelled as an array and the other as a list is the mixed
		// policy a migration produces, and the pair is order-dependent
		// whichever way each half is written.
		oaWantPolicyError(t, `{"version":1,"on_empty":"error","ops":[
			{"id":"a","op":"add_owner(/services/api/)","owners":["@org/x","@org/y"]},
			{"id":"r","op":"remove_owner(/services/api/, @org/y)"}]}`,
			"R-8")
	})
}

// SPEC R-39/R-25: the blast-radius ceiling counts PATHS whose owners change,
// not owners named, so an array of five owners over one path is one path. The
// ceiling is the reviewed artifact's statement about how big this wave is; an
// implementation that counted owners would refuse every multi-owner policy
// under a sane ceiling.
func TestR39_BlastRadiusCountsPathsNotOwners(t *testing.T) {
	t.Run("under the ceiling, three paths change", func(t *testing.T) {
		_, got := oaSyncOK(t, oaRepo(t),
			`{"version":1,"max_paths_changed":3,"ops":[{"id":"g","op":"add_owner(/services/)","owners":["@org/x","@org/y","@org/z"]}]}`)
		if want := "* @org/original\n" +
			"/services/ @org/original @org/x @org/y @org/z\n" +
			"/services/api/ @org/api-team @org/legacy @org/x @org/y @org/z\n"; got != want {
			t.Errorf("wrote:\n%s\nwant:\n%s", got, want)
		}
	})
	t.Run("over the ceiling refuses and writes nothing", func(t *testing.T) {
		repo := oaRepo(t)
		snap := checkDirSnapshot(t, repo)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", oaPolicy(t,
			`{"version":1,"max_paths_changed":1,"ops":[{"id":"g","op":"add_owner(/services/)","owners":["@org/x","@org/y"]}]}`))
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
		}
		oaWantFragment(t, "stderr", errOut, "R-25")
		if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
			t.Errorf("a ceiling refusal must write nothing; repo changed")
		}
	})
}

// SPEC R-39/R-6: an array-spelled removal reaches `on_empty` like any other.
// The three dispositions are asserted together because they are one decision
// with three answers, and because `inherit` and `unowned` produce files that
// differ by one line whose absence changes what GitHub does with the path.
func TestR39_OnEmptyDisposesAnArrayRemoval(t *testing.T) {
	const arr = `"op":"remove_owner(/services/api/)","owners":["@org/api-team","@org/legacy"]`

	t.Run("error refuses this repo", func(t *testing.T) {
		repo := oaRepo(t)
		snap := checkDirSnapshot(t, repo)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy",
			oaPolicy(t, `{"version":1,"on_empty":"error","ops":[{"id":"r",`+arr+`}]}`))
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
		}
		oaWantFragment(t, "stderr", errOut, "R-6")
		if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
			t.Errorf("a refusal must write nothing; repo changed")
		}
	})
	t.Run("inherit deletes the emptied rule", func(t *testing.T) {
		oaEquivalent(t, oaFiles(),
			`{"version":1,"on_empty":"inherit","ops":[{"id":"r",`+arr+`}]}`,
			`{"version":1,"on_empty":"inherit","ops":[{"id":"r","op":"remove_owner(/services/api/, [@org/api-team, @org/legacy])"}]}`,
			"* @org/original\n")
	})
	t.Run("unowned keeps the rule with no owners", func(t *testing.T) {
		oaEquivalent(t, oaFiles(),
			`{"version":1,"on_empty":"unowned","ops":[{"id":"r",`+arr+`}]}`,
			`{"version":1,"on_empty":"unowned","ops":[{"id":"r","op":"remove_owner(/services/api/, [@org/api-team, @org/legacy])"}]}`,
			"* @org/original\n/services/api/\n")
	})
}

// SPEC R-39/R-34: `create` writes a repository's first CODEOWNERS from an
// array-spelled policy, at the highest-precedence location (S-8), naming every
// owner on one line. A fleet wave that creates files is exactly where a
// per-owner append bug would produce N one-owner lines and let last-match-wins
// hand the scope to whichever owner sorted last.
func TestR39_CreateWritesTheFirstFileNamingEveryOwner(t *testing.T) {
	repo := initRepo(t, map[string]string{"src/a.go": "package a\n"})
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", oaPolicy(t,
		`{"version":1,"create":true,"ops":[{"id":"g","op":"add_owner(/src/)","owners":["@org/x","@org/y"]}]}`))
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	oaWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"), "/src/ @org/x @org/y\n")
}

// SPEC R-39/R-19: `--dry-run` previews an array-spelled policy and writes
// nothing. The record must still name every owner: the preview is what an
// operator reads before letting the wave run, and one that under-reported the
// grant would be worse than no preview at all.
func TestR39_DryRunPreviewsTheArrayWithoutWriting(t *testing.T) {
	repo := oaRepo(t)
	snap := checkDirSnapshot(t, repo)
	rec, _ := oaSyncOK(t, repo,
		`{"version":1,"ops":[{"id":"g","op":"add_owner(/services/api/)","owners":["@org/x","@org/y"]}]}`,
		"--dry-run")
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("--dry-run must write nothing; repo changed")
	}
	if got, want := exceptOp0(t, rec)["op"], "add_owner(/services/api/, [@org/x, @org/y])"; got != want {
		t.Errorf("ops[0].op = %v, want %q — the preview has to name who would be granted", got, want)
	}
}

// SPEC R-39/R-24: the PR body a reviewer reads names every owner the array
// grants. `--summary-out` is the one artifact a human sees before merging a
// hundred near-identical PRs; an array-spelled op rendered as
// `add_owner(/docs/)` would put a grant naming nobody in front of the only
// person positioned to catch it.
func TestR39_SummaryNamesEveryOwnerFromTheArray(t *testing.T) {
	repo := oaRepo(t)
	summary := filepath.Join(t.TempDir(), "s.md")
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", oaPolicy(t,
		`{"version":1,"name":"wave","ops":[{"id":"g","op":"add_owner(/docs/)","owners":["@org/x","@org/y"],"note":"why"}]}`),
		"--summary-out", summary)
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	body := syncReadFile(t, summary)
	oaWantFragment(t, "summary", body, "`add_owner(/docs/, [@org/x, @org/y])`")
	oaWantFragment(t, "summary", body, "why")
}

// SPEC R-39: a real wave mixes spellings and verbs in one policy, and the
// result must not depend on which op was written which way. Four ops, two
// spellings, three verbs, one run — the shape of an actual migration, where a
// generator emits arrays for the ops it computes and a human hand-writes the
// one-off.
//
// Exact bytes over the whole file, because the interesting failure here is not
// a missing owner but a line ORDER that resolves a path to the wrong one.
func TestR39_MixedSpellingsAndVerbsInOneWave(t *testing.T) {
	oaEquivalent(t, oaFiles(),
		`{"version":1,"ops":[
			{"id":"d","op":"add_owner(/docs/)","owners":["@org/docs","@org/x"]},
			{"id":"w","op":"add_owner(/services/web/, @org/web)"},
			{"id":"a","op":"set_owners(/services/api/)","owners":["@org/api2"]}]}`,
		`{"version":1,"ops":[
			{"id":"d","op":"add_owner(/docs/, [@org/docs, @org/x])"},
			{"id":"w","op":"add_owner(/services/web/, @org/web)"},
			{"id":"a","op":"set_owners(/services/api/, [@org/api2])"}]}`,
		"* @org/original\n"+
			"/services/web/ @org/original @org/web\n"+
			"/docs/ @org/original @org/docs @org/x\n"+
			"/services/api/ @org/api2\n")
}

// ---------- Pins: what R-39 promises NOT to change ----------

// SPEC R-39: the string spellings R-33 shipped are untouched. These three
// subtests PASS TODAY and are pins, not specifications of new behavior: a
// feature that made a scope-only op string legal on its own, or relaxed
// `set_owners`' brackets as a side effect, would change what an existing
// policy means without anyone editing it.
//
// The first two are the errors an operator sees when they forget the array
// entirely — they must stay the arity message, which names the two legal
// spellings, rather than becoming a message about a field the policy does not
// mention.
func TestR39_PinsBehaviorTheArrayDoesNotDisturb(t *testing.T) {
	t.Run("pin: a scope-only op string with no array is still an arity error", func(t *testing.T) {
		oaWantPolicyError(t,
			`{"version":1,"ops":[{"op":"add_owner(/docs/)"}]}`,
			"add_owner takes (scope, owner) or (scope, [owners])")
	})
	t.Run("pin: set_owners still requires its brackets in the string spelling", func(t *testing.T) {
		oaWantPolicyError(t,
			`{"version":1,"ops":[{"op":"set_owners(/docs/, @org/x)"}]}`,
			"set_owners takes (scope, [owners]) — the list brackets are required")
	})
	t.Run("pin: the bracketed list still means what it meant", func(t *testing.T) {
		repo := oaRepo(t)
		oaSyncOK(t, repo, `{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/, [@org/x, @org/y])"}]}`)
		oaWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"* @org/original\n/docs/ @org/original @org/x @org/y\n/services/api/ @org/api-team @org/legacy\n")
	})
}

// SPEC R-39/R-35c: `owners` is a per-OP field and belongs nowhere else. A
// `defaults` block supplying owners would be a fleet-wide grant stated once,
// far from the ops it silently joins — the ambiguity R-35c exists to remove,
// one field worse. The top level is refused for the same reason.
//
// Both refusals are the unknown-field message, which names the set the level
// does accept, so the fragment asserted is that set rather than R-39: this is
// a claim that the field did NOT spread, and the evidence is the accept-list
// staying as it was.
func TestR39_OwnersIsAPerOpFieldAndNowhereElse(t *testing.T) {
	t.Run("not in defaults", func(t *testing.T) {
		oaWantPolicyError(t,
			`{"version":1,"defaults":{"owners":["@org/x"]},"ops":[{"op":"add_owner(/docs/, @org/x)"}]}`,
			`"on_zero_match", "on_except_zero_match"`)
	})
	t.Run("not at the top level", func(t *testing.T) {
		oaWantPolicyError(t,
			`{"version":1,"owners":["@org/x"],"ops":[{"op":"add_owner(/docs/, @org/x)"}]}`,
			"unknown field")
	})
}

// SPEC R-39/R-38: the array is compared under the one owner identity (R-38a),
// not by bytes. @handles fold on GitHub, so an array naming `@org/api-team`
// against a file spelling it `@org/API-Team` names an owner that is already
// there — and an implementation that decoded the array into its own comparison
// would report "applied" over a semantic no-op on every repository that
// capitalised a handle, which is the fleet-wide false diff R-38 exists to
// prevent.
//
// All three verbs are asserted, because the identity has to hold on the way in
// (add sees the owner as present), on the way through (set is satisfied) and on
// the way out (remove takes every spelling). The file's own spelling is
// preserved throughout: folding governs MATCHING, never output (R-38b), and a
// run that rewrote `@org/API-Team` to the array's spelling would put a diff in
// front of a reviewer that nobody asked for.
func TestR39_ArrayIsComparedUnderTheOwnerIdentity(t *testing.T) {
	files := map[string]string{
		"CODEOWNERS":           "* @org/original\n/services/api/ @org/API-Team @org/legacy\n",
		"services/api/main.go": "package api\n",
		"docs/guide.md":        "guide\n",
	}
	const unchanged = "* @org/original\n/services/api/ @org/API-Team @org/legacy\n"

	t.Run("add of an owner already present under the identity is a no-op", func(t *testing.T) {
		repo := initRepo(t, files)
		rec, got := oaSyncOK(t, repo, `{"version":1,"ops":[{"id":"g","op":"add_owner(/services/api/)","owners":["@org/api-team"]}]}`)
		if got != unchanged {
			t.Errorf("wrote:\n%s\nwant the file untouched:\n%s", got, unchanged)
		}
		if status, _ := rec["status"].(string); status != string(cli.StatusUnchanged) {
			t.Errorf("status = %q, want %q — the owner is already there under R-38a", status, cli.StatusUnchanged)
		}
	})
	t.Run("set is satisfied under the identity", func(t *testing.T) {
		repo := initRepo(t, files)
		rec, got := oaSyncOK(t, repo,
			`{"version":1,"ops":[{"id":"g","op":"set_owners(/services/api/)","owners":["@org/api-team","@org/legacy"]}]}`)
		if got != unchanged {
			t.Errorf("wrote:\n%s\nwant the file untouched (spelling is preserved, R-38b):\n%s", got, unchanged)
		}
		if status, _ := rec["status"].(string); status != string(cli.StatusUnchanged) {
			t.Errorf("status = %q, want %q", status, cli.StatusUnchanged)
		}
	})
	t.Run("remove takes the owner whatever the array spells it", func(t *testing.T) {
		oaEquivalent(t, files,
			`{"version":1,"on_empty":"error","ops":[{"id":"r","op":"remove_owner(/services/api/)","owners":["@ORG/LEGACY"]}]}`,
			`{"version":1,"on_empty":"error","ops":[{"id":"r","op":"remove_owner(/services/api/, [@ORG/LEGACY])"}]}`,
			"* @org/original\n/services/api/ @org/API-Team\n")
	})
}

// SPEC R-39/R-33c: the duplicate rule is per LIST, not per policy. Two ops may
// name the same owner — that is an ordinary wave granting one team several
// scopes — and only a repeat inside one array is the generator bug R-33c
// refuses. An implementation that hoisted the check to the policy would refuse
// the most common multi-op policy there is.
func TestR39_OneOwnerMayAppearInSeveralOps(t *testing.T) {
	oaEquivalent(t, oaFiles(),
		`{"version":1,"ops":[
			{"id":"a","op":"add_owner(/docs/)","owners":["@org/x","@org/y"]},
			{"id":"b","op":"add_owner(/services/web/)","owners":["@org/x"]}]}`,
		`{"version":1,"ops":[
			{"id":"a","op":"add_owner(/docs/, [@org/x, @org/y])"},
			{"id":"b","op":"add_owner(/services/web/, [@org/x])"}]}`,
		"* @org/original\n"+
			"/services/web/ @org/original @org/x\n"+
			"/docs/ @org/original @org/x @org/y\n"+
			"/services/api/ @org/api-team @org/legacy\n")
}

// SPEC R-39/R-36e: an array-spelled policy is validated whole, including the
// sections the running command does not act on. `sync` ignores the `lint`
// block and `lint` ignores `ops`, but a malformed half of either would
// otherwise ride through a whole fleet unseen — and a policy carrying both an
// `owners` array and a `lint` block is the shape a repository-wide baseline
// actually has.
func TestR39_ArrayPolicyIsValidatedAlongsideTheLintBlock(t *testing.T) {
	t.Run("well-formed: both halves accepted", func(t *testing.T) {
		oaSyncOK(t, oaRepo(t), `{"version":1,"lint":{"remove_stale_paths":true,"on_empty":"unowned"},
			"ops":[{"id":"g","op":"add_owner(/docs/)","owners":["@org/x","@org/y"]}]}`)
	})
	t.Run("a defect in either half is exit 3", func(t *testing.T) {
		oaWantPolicyError(t, `{"version":1,"lint":{"remove_stale_paths":"yes"},
			"ops":[{"id":"g","op":"add_owner(/docs/)","owners":["@org/x"]}]}`,
			`"remove_stale_paths"`)
		oaWantPolicyError(t, `{"version":1,"lint":{"remove_stale_paths":true},
			"ops":[{"id":"g","op":"add_owner(/docs/)","owners":["@org/x","@org/x"]}]}`,
			`duplicate owner "@org/x" in one add_owner list`)
	})
}
