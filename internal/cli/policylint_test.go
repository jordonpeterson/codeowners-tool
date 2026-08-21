// Policy-as-source-of-truth end-to-end tests for the two requirements this
// file owns: the `lint` block (R-36) and `except` as a JSON array (R-37),
// specified in docs/policy.md. Written ahead of the implementation per
// CONTRIBUTING.md.
//
// Vacuity is the whole difficulty here, and it has three sources:
//
//   - Today `"lint"` at the top level and `"except"` on an op are UNKNOWN
//     FIELDS, which is exit 3 — the same code most of the negative cases below
//     expect. An exit-code-only assertion would pass against a binary that has
//     never heard of either feature, so every negative case also asserts a
//     message fragment that today's unknown-field error does not contain.
//   - policy.Error renders the FILE first, so a fixture called lint.json or
//     except.json makes "the message mentions except" true by accident. Every
//     policy here is written by plPolicy, which names it p.json.
//   - t.TempDir() embeds the test's own name in the path, and the path is in
//     the message. No test or subtest below is named with a fragment any
//     assertion looks for — in particular the requirement-id fragment is
//     spelled "R-37" (hyphen) while the test names use "R37".
//
// Where the array spelling has a defect the STRING spelling already diagnoses
// (duplicate patterns, unprovable containment, except on rename_owner), the
// fragment asserted is the string spelling's own message: R-37a says the two
// are equivalent in every respect, so an array that fails differently from the
// string it is equivalent to is a defect, not a wording choice. Where the
// defect is array-only (both spellings at once, an empty array) the fragment is
// the requirement id, which every policy-error message in this codebase already
// carries ("(R-26a)", "(R-27)", "(R-28)", "(R-29)") and today's unknown-field
// message does not.
//
// Mutating tests assert EXACT file bytes. Under last-match-wins (S-1) a
// strings.Contains check over file content is satisfied by a file whose line
// ORDER hands the excepted path to the grantee — the adversarial-review finding
// that shaped except_test.go, and R-37's byte-equality claim is precisely a
// claim about that order.
//
// Two subtests are pins that pass today and are labeled as such, both under
// TestR37c_ArrayPatternMayContainALiteralSpace: the escaped string spelling
// (which R-37 promises not to change) and the refusal of the unescaped one
// (which is what makes the array form strictly more expressive). They freeze
// current behavior rather than specifying new behavior; everything else fails
// until the feature lands.
package cli_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// plPolicy writes a policy to a neutrally named file. p.json and not lint.json
// or except.json on purpose: policy.Error prints the file name first, so a
// fixture named after the feature satisfies a keyword assertion on a binary
// that does not implement it (CONTRIBUTING.md).
func plPolicy(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The two spellings of the motivating carve-out. The op STRING is the one
// difference R-37 licenses between them; plRedact removes it so everything
// else can be compared for equality.
const (
	plStringOp = "add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)"
	plArrayOp  = "add_owner(/.github/, @org/team_a)"
)

func plRedact(s string) string {
	for _, op := range []string{plStringOp, plArrayOp} {
		s = strings.ReplaceAll(s, op, "<op>")
	}
	return s
}

// plSame is the assertion R-37e is made of: equivalent in every respect means
// the recorded facts compare equal, not merely "both look plausible".
func plSame(t *testing.T, what string, array, str any) {
	t.Helper()
	if !reflect.DeepEqual(array, str) {
		t.Errorf("%s differs between spellings; R-37e promises identical plans, bytes and R-29/R-32 reporting\n  array form:  %#v\n  string form: %#v", what, array, str)
	}
}

// plSyncOK runs one policy against a fresh copy of the motivating fixture and
// returns the record and the written bytes.
func plSyncOK(t *testing.T, src string) (map[string]any, string) {
	t.Helper()
	repo := exceptRepo(t)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", plPolicy(t, src), "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	return exceptDecodeRaw(t, out), syncReadFile(t, filepath.Join(repo, ".github/CODEOWNERS"))
}

// plWantPolicyError is the exit-3 shape: `check` dies on the artifact before
// repo 1 is opened, `sync` dies identically and writes nothing at all.
func plWantPolicyError(t *testing.T, src, fragment string) {
	t.Helper()
	pol := plPolicy(t, src)
	code, _, errOut := runCLI(t, "check", "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "check stderr", errOut, fragment)

	repo := exceptRepo(t)
	snap := checkDirSnapshot(t, repo)
	code, _, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("sync: want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "sync stderr", errOut, fragment)
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("a policy error must write NOTHING; repo changed")
	}
}

// ---------- R-37: except as a JSON array ----------

// SPEC R-37a/R-37e: the array spelling and the string spelling of one carve-out
// produce the same file, byte for byte, and the same record. Byte equality
// between the two runs is the strongest form the equivalence claim has: a
// carve line that exists but sits in the wrong place resolves the excepted path
// to the grantee under last-match-wins (S-1), and only order-sensitive bytes
// catch that. The literal is also asserted, so a pair of runs that are equally
// wrong cannot pass.
func TestR37a_ArrayAndStringSpellingsWriteIdenticalBytes(t *testing.T) {
	const want = "* @org/original_team\n" +
		"/.github/ @org/original_team @org/team_a\n" +
		"/.github/CODEOWNERS @org/original_team\n"

	arrayRec, arrayBytes := plSyncOK(t, `{"version":1,"ops":[{"id":"g","op":"`+plArrayOp+`","except":["/.github/CODEOWNERS"]}]}`)
	stringRec, stringBytes := plSyncOK(t, `{"version":1,"ops":[{"id":"g","op":"`+plStringOp+`"}]}`)

	if arrayBytes != want {
		t.Errorf("array form wrote:\n%s\nwant:\n%s", arrayBytes, want)
	}
	if arrayBytes != stringBytes {
		t.Errorf("the two spellings wrote different files\n array form:\n%s\n string form:\n%s", arrayBytes, stringBytes)
	}
	plSame(t, "changes[]", arrayRec["changes"], stringRec["changes"])

	arrayOp, stringOp := exceptOp0(t, arrayRec), exceptOp0(t, stringRec)
	plSame(t, "ops[0].excepted (R-32)", arrayOp["excepted"], stringOp["excepted"])
	plSame(t, "ops[0].proven", arrayOp["proven"], stringOp["proven"])
	plSame(t, "ops[0].status", arrayOp["status"], stringOp["status"])
}

// SPEC R-37a: the array reaches every verb that has a scope, not just
// add_owner — a spec that only carved for add_owner would make `except` an
// add_owner feature in practice (the R-29 reasoning, one level up). The
// set_owners case also carries an owner LIST on the same op, because the two
// JSON-shaped features on one object are where a hand-written decoder drops
// one of them. Exact bytes pin both halves: the base mutation happened AND the
// excepted path kept its owners.
func TestR37a_ArrayWorksOnEveryScopedVerb(t *testing.T) {
	files := map[string]string{
		"CODEOWNERS": "/x/ @a @b\n",
		"x/main.go":  "package x\n",
		"x/keep.md":  "keep\n",
	}
	cases := []struct {
		name  string
		array string
		strin string
		want  string
	}{
		{
			name:  "set_owners with an owner list",
			array: `{"version":1,"ops":[{"id":"s","op":"set_owners(/x/, [@new, @second])","except":["/x/keep.md"]}]}`,
			strin: `{"version":1,"ops":[{"id":"s","op":"set_owners(/x/ except /x/keep.md, [@new, @second])"}]}`,
			want:  "/x/ @new @second\n/x/keep.md @a @b\n",
		},
		{
			name:  "remove_owner",
			array: `{"version":1,"on_empty":"error","ops":[{"id":"rm","op":"remove_owner(/x/, @b)","except":["/x/keep.md"]}]}`,
			strin: `{"version":1,"on_empty":"error","ops":[{"id":"rm","op":"remove_owner(/x/ except /x/keep.md, @b)"}]}`,
			want:  "/x/ @a\n/x/keep.md @a @b\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(src string) string {
				repo := initRepo(t, files)
				code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", plPolicy(t, src), "--format", "json")
				if code != cli.ExitOK {
					t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
				}
				return syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))
			}
			got := run(tc.array)
			if got != tc.want {
				t.Errorf("array form wrote:\n%s\nwant:\n%s", got, tc.want)
			}
			if str := run(tc.strin); got != str {
				t.Errorf("the two spellings wrote different files\n array form:\n%s\n string form:\n%s", got, str)
			}
		})
	}
}

// SPEC R-37b: one intent, one place. An op carrying an except clause in its op
// string AND an `except` array is exit 3, before any repo is read — a policy
// whose two halves might disagree must never reach a decision about which one
// wins.
//
// The fragment is the requirement id: today this policy dies with `unknown
// field "except"`, which contains the words "except" and "op", so any obvious
// semantic fragment passes vacuously against a binary that does not implement
// the array at all. Every policy-error message in this codebase already cites
// its requirement.
func TestR37b_BothSpellingsOnOneOpIsExit3(t *testing.T) {
	plWantPolicyError(t,
		`{"version":1,"ops":[{"id":"g","op":"`+plStringOp+`","except":["/.github/workflows/"]}]}`,
		"R-37")
}

// SPEC R-37c: the array needs no delimiter, so the escaping rules the string
// grammar imposes do not apply to it and a pattern containing a space is a
// plain JSON string. This is the one case where the array is strictly more
// expressive than the string spelling it is otherwise equivalent to, so both
// halves are asserted: the array accepts it, and the string form's unescaped
// spelling is still refused.
//
// The written carve line is the CODEOWNERS spelling — the space escaped —
// because the file has its own grammar; the JSON does not.
func TestR37c_ArrayPatternMayContainALiteralSpace(t *testing.T) {
	files := map[string]string{
		"CODEOWNERS":       "* @o\n",
		"docs/my dir/f.md": "x\n",
		"docs/c.md":        "y\n",
	}
	const want = "* @o\n" +
		"/docs/ @o @x\n" +
		`/docs/my\ dir/ @o` + "\n"

	t.Run("array carries it as one pattern", func(t *testing.T) {
		repo := initRepo(t, files)
		pol := plPolicy(t, `{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/, @x)","except":["/docs/my dir/"]}]}`)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		exceptWantFile(t, filepath.Join(repo, "CODEOWNERS"), want)
	})

	// Pin, passes today: the equivalent escaped string spelling must keep
	// writing the same file. R-37c adds a spelling; it does not change what the
	// string grammar means, and the array's claim to equivalence is only worth
	// something while the thing it is equivalent TO holds still.
	t.Run("escaped string spelling still writes the same file", func(t *testing.T) {
		repo := initRepo(t, files)
		code, _, errOut := runCLI(t, "sync", "--repo", repo,
			"--op", `add_owner(/docs/ except /docs/my\ dir/, @x)`, "--format", "json")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		exceptWantFile(t, filepath.Join(repo, "CODEOWNERS"), want)
	})

	// Pin, passes today: the string grammar delimits on unescaped whitespace,
	// so `/docs/my dir/` is two patterns there and the second one is not
	// contained in the scope. This refusal is what makes the array form worth
	// having; an implementation that "helpfully" started accepting the
	// unescaped string spelling would silently reinterpret every scope with a
	// space in it.
	t.Run("string spelling cannot express it", func(t *testing.T) {
		repo := initRepo(t, files)
		snap := checkDirSnapshot(t, repo)
		code, _, errOut := runCLI(t, "sync", "--repo", repo,
			"--op", "add_owner(/docs/ except /docs/my dir/, @x)")
		if code != cli.ExitInvalid {
			t.Fatalf("want exit 3, got %d\nstderr:\n%s", code, errOut)
		}
		exceptWantFragment(t, "stderr", errOut, "not provably contained")
		if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
			t.Errorf("a policy error must write NOTHING; repo changed")
		}
	})
}

// SPEC R-37d: an empty array is exit 3 — state no `except`, not an empty one.
// A generator that emits `"except": []` when it has nothing to carve is asking
// for the broad grant with no carve-out, which is exactly the S-8
// precedence-escalation shape R-28's default refuses per repo; as a static fact
// about the policy it belongs at exit 3, once, not at exit 2 a hundred times.
//
// Fragment is the requirement id, for the reason given on R-37b: today's
// unknown-field message already contains every obvious semantic word.
func TestR37d_EmptyArrayIsExit3(t *testing.T) {
	plWantPolicyError(t,
		`{"version":1,"ops":[{"id":"g","op":"`+plArrayOp+`","except":[]}]}`,
		"R-37")
}

// SPEC R-37a: equivalent in every respect includes the refusals. Each case
// below is a defect the STRING spelling already diagnoses, and the fragment is
// the string spelling's own message — an array that fails at a different rule,
// or with a message that sends the operator hunting in the wrong argument, is
// not the same feature spelled differently.
//
// Duplicates are compared over the PATTERN LANGUAGE, not as strings: /x/gen/
// and /x/gen/** are one pattern spelled twice, and a string-equality check
// waves the pair through (R-27.3, and the adversarial-review finding behind
// TestR27_StaticExceptDefectsAreExit3).
func TestR37_ArrayDefectsRefuseLikeTheStringSpelling(t *testing.T) {
	cases := []struct {
		name     string
		policy   string
		fragment string
	}{
		{"duplicate pattern",
			`{"version":1,"ops":[{"op":"` + plArrayOp + `","except":["/.github/CODEOWNERS","/.github/CODEOWNERS"]}]}`,
			"duplicate except"},
		{"language-equal duplicate pattern",
			`{"version":1,"ops":[{"op":"add_owner(/x/, @a)","except":["/x/gen/","/x/gen/**"]}]}`,
			"duplicate except"},
		{"pattern outside the scope",
			`{"version":1,"ops":[{"op":"` + plArrayOp + `","except":["/docs/README.md"]}]}`,
			"not provably contained"},
		{"pattern equal to the scope",
			`{"version":1,"ops":[{"op":"` + plArrayOp + `","except":["/.github/"]}]}`,
			"entire scope"},
		// rename_owner has no scope, so it has nothing to subtract from — the
		// array cannot become the back door into a verb the string grammar
		// keeps out (R-27.4).
		{"except on rename_owner",
			`{"version":1,"ops":[{"op":"rename_owner(@a, @b)","except":["/x/"]}]}`,
			"takes no scope"},
		// A non-string element is a type error, reported the way every other
		// mistyped policy field is ("field \"note\" must be a string, got the
		// number 7") rather than as a pattern that happens to compile to
		// nothing.
		{"non-string element",
			`{"version":1,"ops":[{"op":"` + plArrayOp + `","except":["/.github/CODEOWNERS",7]}]}`,
			"must be a string"},
		// declare writes one literal CODEOWNERS line and CODEOWNERS has no
		// negation (S-2), so the carve would be a comment rather than a
		// constraint — refused whichever way the except is spelled (R-30).
		{"declare with an except array",
			`{"version":1,"ops":[{"op":"` + plArrayOp + `","except":["/.github/CODEOWNERS"],"on_zero_match":"declare"}]}`,
			"cannot encode subtraction"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plWantPolicyError(t, tc.policy, tc.fragment)
		})
	}
}

// SPEC R-37e/R-28: an except pattern matching zero tracked files refuses this
// repo at exit 2 under the default, and the refusal an operator reads must not
// depend on which spelling the policy used — a fleet that mixes the two would
// otherwise produce two different diagnoses for one condition. The stderrs are
// compared with the op string redacted, since that is the one difference R-37
// licenses.
func TestR37e_ZeroMatchRefusalIsIdenticalBetweenSpellings(t *testing.T) {
	files := map[string]string{
		"CODEOWNERS":               "* @org/original_team\n",
		".github/workflows/ci.yml": "name: ci\n",
	}
	run := func(src string) string {
		t.Helper()
		repo := initRepo(t, files)
		snap := checkDirSnapshot(t, repo)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", plPolicy(t, src))
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
		}
		exceptWantFragment(t, "stderr", errOut, "except pattern")
		exceptWantFragment(t, "stderr", errOut, "matches zero tracked files")
		if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
			t.Errorf("a refusal must write nothing; repo changed")
		}
		return plRedact(errOut)
	}
	array := run(`{"version":1,"ops":[{"id":"g","op":"` + plArrayOp + `","except":["/.github/CODEOWNERS"]}]}`)
	str := run(`{"version":1,"ops":[{"id":"g","op":"` + plStringOp + `"}]}`)
	if array != str {
		t.Errorf("the two spellings refuse differently\n array form:\n%s\n string form:\n%s", array, str)
	}
}

// SPEC R-37e/R-28/R-32: the opt-out reports identically too. `allow` is a
// declare-class weakening of INV-1 — the grant is written with no carve, so a
// matching file created later falls under it — and the three things that make
// it visible (proven: "structural", except_unmatched, a warning) must not go
// missing merely because the carve-out was spelled as an array. Exact bytes
// pin that `allow` APPLIES the grant rather than quietly skipping the op.
func TestR37e_AllowPathReportsIdenticallyBetweenSpellings(t *testing.T) {
	files := map[string]string{
		"CODEOWNERS":               "* @org/original_team\n",
		".github/workflows/ci.yml": "name: ci\n",
	}
	const want = "* @org/original_team\n" +
		"/.github/ @org/original_team @org/team_a\n"

	run := func(src string) (map[string]any, string) {
		t.Helper()
		repo := initRepo(t, files)
		code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", plPolicy(t, src), "--format", "json")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		return exceptDecodeRaw(t, out), syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))
	}
	arrayRec, arrayBytes := run(`{"version":1,"ops":[{"id":"g","op":"` + plArrayOp +
		`","except":["/.github/CODEOWNERS"],"on_except_zero_match":"allow"}]}`)
	stringRec, stringBytes := run(`{"version":1,"ops":[{"id":"g","op":"` + plStringOp +
		`","on_except_zero_match":"allow"}]}`)

	if arrayBytes != want {
		t.Errorf("array form wrote:\n%s\nwant:\n%s", arrayBytes, want)
	}
	if arrayBytes != stringBytes {
		t.Errorf("the two spellings wrote different files\n array form:\n%s\n string form:\n%s", arrayBytes, stringBytes)
	}
	arrayOp, stringOp := exceptOp0(t, arrayRec), exceptOp0(t, stringRec)
	if arrayOp["proven"] != "structural" {
		t.Errorf("proven = %v, want structural — the allow path is a declare-class weakening and is marked like one whichever spelling asked for it", arrayOp["proven"])
	}
	plSame(t, "ops[0].except_unmatched (R-32)", arrayOp["except_unmatched"], stringOp["except_unmatched"])
	plSame(t, "ops[0].proven", arrayOp["proven"], stringOp["proven"])
	plSame(t, "warnings", arrayRec["warnings"], stringRec["warnings"])
	plSame(t, "changes[]", arrayRec["changes"], stringRec["changes"])
}

// ---------- R-36: the lint block ----------

// plLintRepo is the repair fixture: one live rule and one rule whose pattern
// matches nothing, so `remove_stale_paths` has something to decide about.
func plLintRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @org/live\n/gone/ @org/live\n",
		"x/a.go":      "package x\n",
	})
}

// plEmptyOwnerRepo is the R-6 fixture: removing the dead owner would leave
// "/x/" with no owners, which has deliberately no default.
func plEmptyOwnerRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		lintOwnersRel: "* @org/live\n/x/ @org/gone\n",
		"x/a.go":      "package x\n",
		"z.txt":       "z\n",
	})
}

// SPEC R-36a: `lint --policy p.json` takes `remove_stale_paths` from the block,
// so the repair policy is reviewed in the same committed artifact as the
// ownership policy instead of living in a shell line nobody diffs. Exact bytes:
// a stale rule deleted is a rule whose intent no other record holds, so
// "roughly the right file" is not an outcome worth asserting.
func TestR36a_BlockSuppliesRemoveStalePaths(t *testing.T) {
	repo := plLintRepo(t)
	pol := plPolicy(t, `{"version":1,"lint":{"remove_stale_paths":true},"ops":["add_owner(/x/, @org/other)"]}`)
	code, out, errb := lintVerbRun(t, repo, lintAPI(t, nil, "org/live", "org/other"), "--policy", pol)
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if got := lintRead(t, lintOwnersPath(repo)); got != "/x/ @org/live\n" {
		t.Errorf("CODEOWNERS = %q, want the stale rule gone and nothing else touched", got)
	}
}

// SPEC R-36a: `on_empty` comes from the block too, and it is genuinely
// consulted — the three values are three different repositories, so all three
// are pinned. Without the block this run is exit 3 ("an explicit --on-empty
// policy is required"), which is what makes each case here evidence that the
// block was read rather than defaulted past.
func TestR36a_BlockSuppliesOnEmpty(t *testing.T) {
	api := lintAPI(t, nil, "org/live")

	t.Run("unowned keeps the pattern with no owners", func(t *testing.T) {
		repo := plEmptyOwnerRepo(t)
		pol := plPolicy(t, `{"version":1,"lint":{"on_empty":"unowned"},"ops":["add_owner(/x/, @org/live)"]}`)
		code, out, errb := lintVerbRun(t, repo, api, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		if got := lintRead(t, lintOwnersPath(repo)); got != "* @org/live\n/x/\n" {
			t.Errorf("CODEOWNERS = %q: unowned keeps the PATTERN — deleting it would hand /x/ to the `*` rule instead of un-owning it", got)
		}
	})

	t.Run("inherit deletes the rule line", func(t *testing.T) {
		repo := plEmptyOwnerRepo(t)
		pol := plPolicy(t, `{"version":1,"lint":{"on_empty":"inherit"},"ops":["add_owner(/x/, @org/live)"]}`)
		code, out, errb := lintVerbRun(t, repo, api, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		if got := lintRead(t, lintOwnersPath(repo)); got != "* @org/live\n" {
			t.Errorf("CODEOWNERS = %q, want the emptied rule deleted so `*` takes over", got)
		}
	})

	// A refusal the operator asked for is exit 2, not exit 3: exit 3 is "you
	// did not tell me", and a script can retry that one with an added setting.
	t.Run("error refuses and writes nothing", func(t *testing.T) {
		repo := plEmptyOwnerRepo(t)
		before := lintRead(t, lintOwnersPath(repo))
		pol := plPolicy(t, `{"version":1,"lint":{"on_empty":"error"},"ops":["add_owner(/x/, @org/live)"]}`)
		code, out, errb := lintVerbRun(t, repo, api, "--policy", pol)
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2, got %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		exceptWantFragment(t, "stderr", errb, "no owners")
		lintUnchanged(t, "on_empty=error", lintOwnersPath(repo), before)
	})
}

// SPEC R-36a: an empty block is legal and states no preference, so every
// default still holds — `remove_stale_paths` off in particular (R-11). A block
// that switched a repair on merely by existing would prune "dead" rules across
// a fleet the first time anyone committed one.
func TestR36a_EmptyBlockMeansDefaults(t *testing.T) {
	repo := plLintRepo(t)
	before := lintRead(t, lintOwnersPath(repo))
	pol := plPolicy(t, `{"version":1,"lint":{},"ops":["add_owner(/x/, @org/live)"]}`)
	code, out, errb := lintVerbRun(t, repo, lintAPI(t, nil, "org/live"), "--policy", pol)
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	lintUnchanged(t, "empty lint block", lintOwnersPath(repo), before)
}

// SPEC R-36b: a flag that duplicates a block field alongside --policy is exit 3
// — the rule `sync` already applies to --on-empty and --max-paths-changed
// ("--on-empty is not allowed with --policy: set \"on_empty\" in the policy
// file instead"). A flag that silently overrode the reviewed artifact would
// mean the file in git is not the policy that ran.
//
// The rejection is unconditional, exactly as `sync`'s is: it does not depend on
// whether the block happens to set that field, because "the flag agreed with
// the file this time" is not a property anyone reviewing the artifact can see.
// Both flags are rejected before the run, so the file is untouched.
func TestR36b_FlagDuplicatingABlockFieldIsExit3(t *testing.T) {
	cases := []struct {
		name  string
		block string
		flags []string
	}{
		{"remove-stale-paths", `{"remove_stale_paths":true}`, []string{"--remove-stale-paths"}},
		{"on-empty", `{"on_empty":"inherit"}`, []string{"--on-empty", "inherit"}},
		{"flag whose field the block omits", `{}`, []string{"--remove-stale-paths"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := plLintRepo(t)
			before := lintRead(t, lintOwnersPath(repo))
			pol := plPolicy(t, `{"version":1,"lint":`+tc.block+`,"ops":["add_owner(/x/, @org/live)"]}`)
			args := append([]string{"--policy", pol}, tc.flags...)
			code, out, errb := lintVerbRun(t, repo, lintAPI(t, nil, "org/live"), args...)
			if code != cli.ExitInvalid {
				t.Fatalf("want exit 3, got %d\nstdout: %s\nstderr: %s", code, out, errb)
			}
			exceptWantFragment(t, "stderr", errb, "not allowed with --policy")
			lintUnchanged(t, "rejected flag combination", lintOwnersPath(repo), before)
		})
	}
}

// SPEC R-36c: one committed file serves both commands, so `sync` must ignore
// the lint block rather than fail on it — and `check` must validate the
// artifact as a whole. A fleet cannot be asked to keep two policy files in
// step; that is the drift this requirement exists to prevent.
func TestR36c_SyncIgnoresTheBlock(t *testing.T) {
	repo := syncSmallRepo(t)
	pol := plPolicy(t, `{"version":1,"lint":{"remove_stale_paths":true,"on_empty":"inherit"},"ops":["add_owner(/x/, @b)"]}`)

	if code, _, errOut := runCLI(t, "check", "--policy", pol); code != cli.ExitOK {
		t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("sync: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFile(t, filepath.Join(repo, "CODEOWNERS"), "# owners\n/x/ @a @b\n")
}

// SPEC R-36c: the other direction, and the dangerous one — `lint` ignores
// `ops`. The op here names an owner the stub API says EXISTS, so an
// implementation that applied it would leave "@org/other" in the file instead
// of having it removed again as a dead owner: the wrong outcome has to be
// distinguishable from the right one, or the test proves nothing. Exact bytes,
// because `lint` writes.
func TestR36c_LintIgnoresOps(t *testing.T) {
	repo := plLintRepo(t)
	lintWrite(t, lintOwnersPath(repo), "/x/ @ org/live\n/gone/ @org/live\n")
	pol := plPolicy(t, `{"version":1,"lint":{"remove_stale_paths":true},"ops":["add_owner(/x/, @org/other)"]}`)
	code, out, errb := lintVerbRun(t, repo, lintAPI(t, nil, "org/live", "org/other"), "--policy", pol)
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	if got := lintRead(t, lintOwnersPath(repo)); got != "/x/ @org/live\n" {
		t.Errorf("CODEOWNERS = %q, want the handle rejoined and the stale rule gone — and no owner from ops[]", got)
	}
}

// SPEC R-36d: an unknown key inside the block is exit 3, matching R-20's
// treatment of unknown fields everywhere else. A typo'd repair preference that
// was ignored would run the fleet with the DEFAULT while the reviewed artifact
// says otherwise — the failure mode R-20 exists to make impossible.
//
// The fragment is the misspelled key: today the whole block is one unknown
// field, so the current message names "lint" and never the key inside it.
func TestR36d_UnknownKeyInTheBlockIsExit3(t *testing.T) {
	src := `{"version":1,"lint":{"remove_stale_pathz":true},"ops":["add_owner(/x/, @org/live)"]}`
	pol := plPolicy(t, src)

	code, _, errOut := runCLI(t, "check", "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "check stderr", errOut, "remove_stale_pathz")

	repo := plLintRepo(t)
	before := lintRead(t, lintOwnersPath(repo))
	code, out, errb := lintVerbRun(t, repo, lintAPI(t, nil, "org/live"), "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("lint: want exit 3, got %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	exceptWantFragment(t, "lint stderr", errb, "remove_stale_pathz")
	lintUnchanged(t, "unknown key in the block", lintOwnersPath(repo), before)
}

// SPEC R-36d/R-20: a known key with an unknown VALUE is exit 3 as well, and the
// message names the value — a policy error is only actionable when the operator
// can see which of the three R-6 outcomes they meant to type. The fragment is
// the misspelled value, because the key name and the word "unknown" both appear
// in today's unrelated message.
func TestR36d_BadEnumValueInTheBlockIsExit3(t *testing.T) {
	src := `{"version":1,"lint":{"on_empty":"inhrit"},"ops":["add_owner(/x/, @org/live)"]}`
	pol := plPolicy(t, src)

	code, _, errOut := runCLI(t, "check", "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "check stderr", errOut, "inhrit")

	repo := plEmptyOwnerRepo(t)
	before := lintRead(t, lintOwnersPath(repo))
	code, out, errb := lintVerbRun(t, repo, lintAPI(t, nil, "org/live"), "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("lint: want exit 3, got %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	exceptWantFragment(t, "lint stderr", errb, "inhrit")
	lintUnchanged(t, "bad enum in the block", lintOwnersPath(repo), before)
}

// SPEC R-36/R-17: the block changes where lint's preferences are written, and
// nothing else. Exit 4 still means "the file needs a person", and a SUCCESSFUL
// WRITE can still exit 4 — the documented reason `lint && git commit` does not
// do what it looks like (docs/LINTING.md, and the CLI help text says it too).
// A --policy run that started collapsing that to 0 would hand every fleet job a
// green light over a file with an unrepairable line in it.
func TestR36_ExitFourAfterASuccessfulWriteIsUnchanged(t *testing.T) {
	const broken = "* @org/live\n/x/ @ org/live\n/y @@@bad\n"
	const repaired = "* @org/live\n/x/ @org/live\n/y @@@bad\n"
	mk := func() string {
		repo := initRepo(t, map[string]string{
			lintOwnersRel: "* @org/live\n",
			"x/a.go":      "package x\n",
		})
		lintWrite(t, lintOwnersPath(repo), broken)
		return repo
	}
	pol := plPolicy(t, `{"version":1,"lint":{},"ops":["add_owner(/x/, @org/live)"]}`)
	api := lintAPI(t, nil, "org/live")

	t.Run("the write happens and the status is still 4", func(t *testing.T) {
		repo := mk()
		code, out, errb := lintVerbRun(t, repo, api, "--policy", pol)
		if code != cli.ExitFindings {
			t.Fatalf("want exit 4, got %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		if got := lintRead(t, lintOwnersPath(repo)); got != repaired {
			t.Errorf("CODEOWNERS = %q, want the repair written; exit 4 is about what is LEFT, not about the write", got)
		}
		lintMentions(t, "write confirmation", out, "the file was written")
		lintMentions(t, "exit-code explanation", out, "not a failed write")
	})

	// --dry-run is per-invocation, not a block field: it says which run this is,
	// not what gets written, so R-36b must not swallow it along with the
	// settings the block owns.
	t.Run("dry-run stays legal alongside the block", func(t *testing.T) {
		repo := mk()
		code, out, errb := lintVerbRun(t, repo, api, "--policy", pol, "--dry-run")
		if code != cli.ExitFindings {
			t.Fatalf("want exit 4, got %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		lintUnchanged(t, "--dry-run with --policy", lintOwnersPath(repo), broken)
	})
}
