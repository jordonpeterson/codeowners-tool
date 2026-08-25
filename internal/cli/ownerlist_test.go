// Owner-list end-to-end tests (R-33). Written ahead of the
// implementation per CONTRIBUTING.md: today every one of these op strings
// dies in ops.Parse with "add_owner takes a single owner, not a list" — exit
// 3, the same code several of these tests expect — so every negative case
// asserts a MESSAGE FRAGMENT as well as the exit code. An exit-code-only
// assertion here passes vacuously against a feature that does not exist.
//
// The fragments are chosen to be unreachable from today's refusal, and each
// is normative: the implementation must contain it.
//
//	R-33c  "duplicate owner"     today's message says "not a list"
//	R-33d  "empty owner list"    ditto — note the current text contains
//	                             "owner, not a list", never "owner list"
//	R-33f  "takes no list"       today: `invalid owner token "[@b]"`
//	malformed list  "owner list" / "invalid owner token" (set_owners' wording,
//	                             which the list grammar inherits)
//
// Mutating tests assert EXACT file bytes, a snapshot-level resolution, or the
// `plan --out` artifact — never strings.Contains on file content: under
// last-match-wins (S-1) a substring check is satisfied by a file whose line
// ORDER produces the wrong ownership, which is the bug adversarial review
// found in the except suite. Where a byte assertion could itself be wrong,
// the expected bytes are additionally derived from the single-owner op
// sequence the list replaces (R-33a), so the oracle cannot drift from the
// behavior it folds.
//
// Two subtests pass against today's binary by design, are named "pin:" and
// are labeled where they stand: the `set_owners(scope, [])` case of
// TestR33d_EmptyListIsExit3 and the set_owners case of
// TestR33_TrailingCommaFollowsTheSetOwnersGrammar. They freeze behavior R-33
// promises to preserve rather than specify new behavior; both are the premise
// their sibling case relies on, so they run as separate subtests and report
// even when that sibling fails. Everything else fails until the feature lands.
package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// listRepo is R-33's motivating fixture: one catch-all owner, a scope with
// TWO tracked files (so a paths-changed count can be told apart from an
// owners count, R-25) and one file outside the scope so INV-2 has something
// to protect.
func listRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"CODEOWNERS":        "* @org/original\n",
		"services/api/a.go": "package api\n",
		"services/api/b.go": "package api\n",
		"top.md":            "top\n",
	})
}

// listDocsRepo is the remove_owner fixture from the spec's second example:
// two owners to strip, one to keep, so removal can never empty the set and
// the test is about the LIST, not about on_empty.
func listDocsRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"CODEOWNERS": "* @org/original\n/docs/ @org/legacy @org/former @org/keep\n",
		"docs/a.md":  "a\n",
		"docs/b.md":  "b\n",
		"top.md":     "top\n",
	})
}

// listBothRepo carries both scopes the static-error tables use, so a refusal
// under test is the ONLY defect the run could report. Against a repo missing
// the scope, an implementation that validated the list lazily would refuse
// with R-5's zero-match message instead, and the fragment assertion would go
// red for a reason that has nothing to do with R-33.
func listBothRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"CODEOWNERS":        "* @org/original\n/docs/ @org/legacy @org/former @org/keep\n",
		"services/api/a.go": "package api\n",
		"services/api/b.go": "package api\n",
		"docs/a.md":         "a\n",
		"top.md":            "top\n",
	})
}

func listWantFile(t *testing.T, path, want string) {
	t.Helper()
	if got := syncReadFile(t, path); got != want {
		t.Errorf("file bytes:\n%s\nwant:\n%s", got, want)
	}
}

func listWantFragment(t *testing.T, where, got, fragment string) {
	t.Helper()
	if !strings.Contains(got, fragment) {
		t.Errorf("%s must contain %q — the operator has to be able to tell this refusal from every other one.\ngot:\n%s",
			where, fragment, got)
	}
}

// listPlanArtifact runs `plan --out` and returns the artifact as parsed by
// the same struct `apply` consumes. R-33b is a claim about the PLAN, not only
// about the file it produces: the intermediate line is invisible in the final
// bytes and visible here.
func listPlanArtifact(t *testing.T, repo string, ops ...string) plan.Plan {
	t.Helper()
	out := filepath.Join(t.TempDir(), "plan.json")
	args := []string{"plan", "--repo", repo}
	for _, op := range ops {
		args = append(args, "--op", op)
	}
	args = append(args, "--out", out)
	code, _, errOut := runCLI(t, args...)
	if code != cli.ExitOK {
		t.Fatalf("plan: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	var p plan.Plan
	if err := json.Unmarshal([]byte(syncReadFile(t, out)), &p); err != nil {
		t.Fatalf("plan artifact is not valid JSON: %v", err)
	}
	return p
}

// listChangesFor selects the plan's changes for one pattern. R-33b counts
// hunks per written line, and a carve line (R-29) is a different line, so the
// count has to be taken per pattern rather than over the whole plan.
func listChangesFor(changes []plan.Change, pattern string) []plan.Change {
	var out []plan.Change
	for _, c := range changes {
		if c.Pattern == pattern {
			out = append(out, c)
		}
	}
	return out
}

// listWantNoIntermediate fails if any hunk in the plan reads or writes a line
// that never exists on disk. This is the exact shape of the defect R-33b
// names: two adds on one scope emit hunk 2 whose old_line is hunk 1's
// new_line, so a reviewer of the artifact sees a state the repo never held.
func listWantNoIntermediate(t *testing.T, p plan.Plan, intermediate string) {
	t.Helper()
	for i, c := range p.Changes {
		if c.OldLine == intermediate || c.NewLine == intermediate {
			t.Errorf("changes[%d] names %q, a line that is never on disk at any point — one intent must be one hunk (R-33b)\nchanges: %+v",
				i, intermediate, p.Changes)
		}
	}
}

// SPEC R-33a: an owner list produces exactly the file the single-owner ops it
// replaces produce, including byte order. The oracle is BOTH: the literal
// expected bytes, and the same repo built by running the two single-owner ops
// in sequence — a hand-written expectation can be wrong, and a differential
// one alone cannot catch two implementations wrong the same way.
func TestR33a_ListFoldsToTheSameFileAsTheOpsItReplaces(t *testing.T) {
	t.Run("add_owner", func(t *testing.T) {
		folded := listRepo(t)
		code, _, errOut := runCLI(t, "sync", "--repo", folded,
			"--op", "add_owner(/services/api/, [@org/api-team, @org/platform])")
		if code != cli.ExitOK {
			t.Fatalf("list form: want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		want := "* @org/original\n" +
			"/services/api/ @org/original @org/api-team @org/platform\n"
		listWantFile(t, filepath.Join(folded, "CODEOWNERS"), want)

		sequence := listRepo(t)
		for _, owner := range []string{"@org/api-team", "@org/platform"} {
			if code, _, e := runCLI(t, "sync", "--repo", sequence,
				"--op", "add_owner(/services/api/, "+owner+")"); code != cli.ExitOK {
				t.Fatalf("sequence %s: want exit 0, got %d\n%s", owner, code, e)
			}
		}
		if got := syncReadFile(t, filepath.Join(sequence, "CODEOWNERS")); got != want {
			t.Fatalf("the op sequence itself no longer produces the expected bytes — this test's oracle is stale:\n%s", got)
		}
	})
	t.Run("remove_owner", func(t *testing.T) {
		folded := listDocsRepo(t)
		code, _, errOut := runCLI(t, "sync", "--repo", folded, "--on-empty", "error",
			"--op", "remove_owner(/docs/, [@org/legacy, @org/former])")
		if code != cli.ExitOK {
			t.Fatalf("list form: want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		want := "* @org/original\n/docs/ @org/keep\n"
		listWantFile(t, filepath.Join(folded, "CODEOWNERS"), want)

		sequence := listDocsRepo(t)
		for _, owner := range []string{"@org/legacy", "@org/former"} {
			if code, _, e := runCLI(t, "sync", "--repo", sequence, "--on-empty", "error",
				"--op", "remove_owner(/docs/, "+owner+")"); code != cli.ExitOK {
				t.Fatalf("sequence %s: want exit 0, got %d\n%s", owner, code, e)
			}
		}
		if got := syncReadFile(t, filepath.Join(sequence, "CODEOWNERS")); got != want {
			t.Fatalf("the op sequence itself no longer produces the expected bytes — this test's oracle is stale:\n%s", got)
		}
	})
}

// SPEC R-33a: a one-element list is the bare form, byte for byte. "The
// single-owner form stays legal and unchanged" is only half the promise; the
// other half is that a generator emitting `[@a]` for a one-owner intent does
// not produce a different file from a human typing `@a`.
func TestR33a_SingleElementListEqualsTheBareForm(t *testing.T) {
	bare := listRepo(t)
	if code, _, e := runCLI(t, "sync", "--repo", bare,
		"--op", "add_owner(/services/api/, @org/api-team)"); code != cli.ExitOK {
		t.Fatalf("bare form: want exit 0, got %d\n%s", code, e)
	}
	want := syncReadFile(t, filepath.Join(bare, "CODEOWNERS"))
	if want != "* @org/original\n/services/api/ @org/original @org/api-team\n" {
		t.Fatalf("bare-form oracle is stale:\n%s", want)
	}
	list := listRepo(t)
	code, _, errOut := runCLI(t, "sync", "--repo", list,
		"--op", "add_owner(/services/api/, [@org/api-team])")
	if code != cli.ExitOK {
		t.Fatalf("single-element list: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	listWantFile(t, filepath.Join(list, "CODEOWNERS"), want)
}

// SPEC R-33a/grammar: whitespace inside the brackets is insignificant, as it
// already is for set_owners. `[@a,@b]`, `[@a, @b]`, `[ @a , @b ]` and a
// tab-separated spelling are one op, and a policy reformatted by a generator
// must not produce a different file. Each variant is checked against exact
// bytes, not against the others alone: four identically broken runs (all
// refusing, all writing nothing) would agree with each other.
func TestR33a_ListWhitespaceVariantsAreOneGrammar(t *testing.T) {
	want := "* @org/original\n" +
		"/services/api/ @org/original @org/api-team @org/platform\n"
	// Named rather than keyed by the spelling itself: Go renders a tab in a
	// subtest name as the same underscore a space becomes, so two of these
	// four would report under one name and a failure in either would be
	// unattributable.
	for name, spelling := range map[string]string{
		"comma only":    "[@org/api-team,@org/platform]",
		"comma space":   "[@org/api-team, @org/platform]",
		"padded":        "[ @org/api-team , @org/platform ]",
		"comma tab":     "[@org/api-team,\t@org/platform]",
		"leading space": "[@org/api-team,  @org/platform]",
	} {
		t.Run(name, func(t *testing.T) {
			repo := listRepo(t)
			code, _, errOut := runCLI(t, "sync", "--repo", repo,
				"--op", "add_owner(/services/api/, "+spelling+")")
			if code != cli.ExitOK {
				t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
			}
			listWantFile(t, filepath.Join(repo, "CODEOWNERS"), want)
		})
	}
}

// SPEC R-33b: N owners on one scope are ONE line change, not N. The defect
// this requirement exists to fix is visible only in the artifact: today
// `--op add_owner(/x/, @a) --op add_owner(/x/, @b)` emits two hunks against
// the same physical line, the second's old_line equal to the first's
// new_line, so `plan --out` shows a state that is on disk at no point in the
// run. Both line-shapes are covered — the inserted narrowing rule and the
// amended pre-existing one — because they are produced by different arms of
// the planner and only one of them was reproduced when this was found.
func TestR33b_OneIntentIsOneHunk(t *testing.T) {
	t.Run("inserted narrowing line", func(t *testing.T) {
		p := listPlanArtifact(t, listRepo(t),
			"add_owner(/services/api/, [@org/api-team, @org/platform])")
		got := listChangesFor(p.Changes, "/services/api/")
		if len(got) != 1 {
			t.Fatalf("want exactly 1 change for /services/api/, got %d: %+v", len(got), p.Changes)
		}
		if got[0].NewLine != "/services/api/ @org/original @org/api-team @org/platform" {
			t.Errorf("new_line = %q", got[0].NewLine)
		}
		listWantNoIntermediate(t, p, "/services/api/ @org/original @org/api-team")
		if p.AfterContent != "* @org/original\n/services/api/ @org/original @org/api-team @org/platform\n" {
			t.Errorf("after_content = %q", p.AfterContent)
		}
	})
	t.Run("amended pre-existing line", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS":        "* @org/original\n/services/api/ @org/original\n",
			"services/api/a.go": "package api\n",
			"services/api/b.go": "package api\n",
		})
		p := listPlanArtifact(t, repo, "add_owner(/services/api/, [@x, @y])")
		got := listChangesFor(p.Changes, "/services/api/")
		if len(got) != 1 {
			t.Fatalf("want exactly 1 change for /services/api/, got %d: %+v", len(got), p.Changes)
		}
		if got[0].OldLine != "/services/api/ @org/original" ||
			got[0].NewLine != "/services/api/ @org/original @x @y" {
			t.Errorf("hunk = %q -> %q", got[0].OldLine, got[0].NewLine)
		}
		listWantNoIntermediate(t, p, "/services/api/ @org/original @x")
	})
	// A wide list is where a per-owner implementation is cheapest to write and
	// most expensive to review: twelve owners must still be one hunk and one
	// line, not twelve hunks the reviewer has to fold in their head.
	t.Run("twelve owners", func(t *testing.T) {
		owners := []string{"@t01", "@t02", "@t03", "@t04", "@t05", "@t06",
			"@t07", "@t08", "@t09", "@t10", "@t11", "@t12"}
		p := listPlanArtifact(t, listRepo(t),
			"add_owner(/services/api/, ["+strings.Join(owners, ", ")+"])")
		if got := listChangesFor(p.Changes, "/services/api/"); len(got) != 1 {
			t.Fatalf("want exactly 1 change for /services/api/, got %d: %+v", len(got), p.Changes)
		}
		want := "* @org/original\n/services/api/ @org/original " + strings.Join(owners, " ") + "\n"
		if p.AfterContent != want {
			t.Errorf("after_content = %q\nwant %q", p.AfterContent, want)
		}
	})
}

// SPEC R-33b/R-26: a list and an except clause on ONE op. The grant line is
// one hunk however many owners it names, and the carve line (R-29) is a
// second, separate hunk — so the count is taken per pattern. Today the
// two-op spelling of this intent emits three hunks for two lines, with the
// grant amended from a state the file never holds. Exact bytes are asserted
// too: a plan that counted right while placing the carve at EOF would hand
// the excepted path to the grantee under last-match-wins (S-1).
func TestR33b_ListWithExceptGrantsOnceAndCarvesOnce(t *testing.T) {
	op := "add_owner(/.github/ except /.github/CODEOWNERS, [@org/team_a, @org/team_b])"
	p := listPlanArtifact(t, exceptRepo(t), op)
	if got := listChangesFor(p.Changes, "/.github/"); len(got) != 1 {
		t.Errorf("want exactly 1 grant hunk for /.github/, got %d: %+v", len(got), p.Changes)
	}
	if got := listChangesFor(p.Changes, "/.github/CODEOWNERS"); len(got) != 1 {
		t.Errorf("want exactly 1 carve hunk for /.github/CODEOWNERS, got %d: %+v", len(got), p.Changes)
	}
	listWantNoIntermediate(t, p, "/.github/ @org/original_team @org/team_a")

	repo := exceptRepo(t)
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", op)
	if code != cli.ExitOK {
		t.Fatalf("sync: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	listWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"),
		"* @org/original_team\n"+
			"/.github/ @org/original_team @org/team_a @org/team_b\n"+
			"/.github/CODEOWNERS @org/original_team\n")
}

// SPEC R-33c: a duplicate owner inside one list is exit 3 — a fact about the
// policy, identical in every repo, so `check` catches it before repo 1 and
// `sync` writes nothing. The non-adjacent case is included because the
// cheapest duplicate check compares neighbours after a sort that the list
// grammar must not perform (R-33e: order is preserved).
//
// Case-variant handles (`@A` vs `@a`) are one owner under R-33c too, and are
// asserted in internal/ops (TestParse_DuplicateOwnersFoldCase) rather than
// here. The justification this comment used to give — that case is "audit's
// A-5 question" — was simply wrong: A-5 is `rule dead only because of case`
// over pattern text, and nothing in audit looks at owner-handle case. The
// identity that decides it is ops.FoldOwner, which lint has always used
// (adversarial-review finding).
func TestR33c_DuplicateInsideOneListIsExit3(t *testing.T) {
	cases := map[string]string{
		"adjacent":     `{"version":1,"ops":["add_owner(/services/api/, [@org/api-team, @org/api-team])"]}`,
		"non-adjacent": `{"version":1,"ops":["add_owner(/services/api/, [@org/api-team, @org/platform, @org/api-team])"]}`,
		"remove_owner": `{"version":1,"on_empty":"error","ops":["remove_owner(/docs/, [@org/legacy, @org/legacy])"]}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			pol := syncWritePolicy(t, src)
			code, _, errOut := runCLI(t, "check", "--policy", pol)
			if code != cli.ExitInvalid {
				t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			listWantFragment(t, "check stderr", errOut, "duplicate owner")

			repo := listBothRepo(t)
			snap := checkDirSnapshot(t, repo)
			code, _, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol)
			if code != cli.ExitInvalid {
				t.Fatalf("sync: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			listWantFragment(t, "sync stderr", errOut, "duplicate owner")
			if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
				t.Errorf("a policy error must write NOTHING; repo changed")
			}
		})
	}
}

// SPEC R-33d: an empty list on add_owner/remove_owner is exit 3 — "add
// nobody" and "remove nobody" are not intents, they are generator bugs, and a
// tool that accepted them would report a wall of green no-ops across a fleet.
// The bracketed-whitespace and lone-comma spellings are included because they
// reach the tokenizer as an empty list rather than as a syntax error.
func TestR33d_EmptyListIsExit3(t *testing.T) {
	for _, op := range []string{
		"add_owner(/services/api/, [])",
		"add_owner(/services/api/, [ ])",
		"add_owner(/services/api/, [,])",
		"remove_owner(/docs/, [])",
	} {
		t.Run(op, func(t *testing.T) {
			pol := syncWritePolicy(t, `{"version":1,"on_empty":"error","ops":[`+listQuotedOp(op)+`]}`)
			code, _, errOut := runCLI(t, "check", "--policy", pol)
			if code != cli.ExitInvalid {
				t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			listWantFragment(t, "check stderr", errOut, "empty owner list")

			repo := listBothRepo(t)
			snap := checkDirSnapshot(t, repo)
			code, _, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol)
			if code != cli.ExitInvalid {
				t.Fatalf("sync: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			listWantFragment(t, "sync stderr", errOut, "empty owner list")
			if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
				t.Errorf("a policy error must write NOTHING; repo changed")
			}
		})
	}
	// Pin, passes today: R-33d's second sentence. `set_owners(scope, [])`
	// stays legal and still means "nobody owns this" — the S-9 zero-owner
	// rule. Freezes it against an implementation that rejects every empty
	// list in one place and takes set_owners down with it.
	t.Run("pin: set_owners empty list stays legal", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS": "* @org/original\n/docs/ @org/legacy\n",
			"docs/a.md":  "a\n",
		})
		code, _, errOut := runCLI(t, "sync", "--repo", repo,
			"--op", "set_owners(/docs/, [])", "--on-empty", "unowned")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFile(t, filepath.Join(repo, "CODEOWNERS"), "* @org/original\n/docs/\n")
	})
}

// listQuotedOp JSON-quotes an op string for embedding in a policy literal.
// The spellings under test carry brackets and commas, so pasting one into a
// policy by concatenation would risk a policy that is invalid for a reason
// unrelated to R-33 — the vacuous-failure mirror of a vacuous pass.
func listQuotedOp(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// SPEC R-33e: list order fixes the APPEND order in the written line and
// nothing else. Both orders are asserted against exact bytes — the claim is
// about bytes, and an implementation that sorted owners would satisfy a
// resolution-only oracle while quietly rewriting every line it touches — and
// against resolution via `snapshot`, which is the property that actually
// matters to GitHub, where owners are a set.
func TestR33e_ListOrderFixesAppendOrderNotResolution(t *testing.T) {
	orders := map[string]string{
		"[@org/api-team, @org/platform]": "/services/api/ @org/original @org/api-team @org/platform\n",
		"[@org/platform, @org/api-team]": "/services/api/ @org/original @org/platform @org/api-team\n",
	}
	for list, wantLine := range orders {
		t.Run(list, func(t *testing.T) {
			repo := listRepo(t)
			code, _, errOut := runCLI(t, "sync", "--repo", repo,
				"--op", "add_owner(/services/api/, "+list+")")
			if code != cli.ExitOK {
				t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
			}
			listWantFile(t, filepath.Join(repo, "CODEOWNERS"), "* @org/original\n"+wantLine)
			gitCommitAll(t, repo)
			own := exceptOwnership(t, repo) // owners come back sorted: a set, not a sequence
			for path, want := range map[string][]string{
				"services/api/a.go": {"@org/api-team", "@org/original", "@org/platform"},
				"services/api/b.go": {"@org/api-team", "@org/original", "@org/platform"},
				"top.md":            {"@org/original"},
			} {
				if !reflect.DeepEqual(own[path], want) {
					t.Errorf("%s resolves to %v, want %v — list order must not change ownership", path, own[path], want)
				}
			}
		})
	}
}

// SPEC R-33f: `rename_owner` takes no list, in either argument position —
// exit 3. It renames one owner to one owner; a list there has no meaning, and
// falling through to `invalid owner token "[@b]"` (today's message) leaves the
// operator hunting a typo in an owner name instead of learning that the
// construct does not exist, exactly as R-27.4 argued for except.
func TestR33f_RenameOwnerTakesNoList(t *testing.T) {
	for _, op := range []string{
		"rename_owner(@org/old, [@org/new])",
		"rename_owner([@org/old], @org/new)",
		"rename_owner([@org/old], [@org/new])",
	} {
		t.Run(op, func(t *testing.T) {
			pol := syncWritePolicy(t, `{"version":1,"ops":[`+listQuotedOp(op)+`]}`)
			code, _, errOut := runCLI(t, "check", "--policy", pol)
			if code != cli.ExitInvalid {
				t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			listWantFragment(t, "check stderr", errOut, "takes no list")

			repo := listBothRepo(t)
			snap := checkDirSnapshot(t, repo)
			code, _, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol)
			if code != cli.ExitInvalid {
				t.Fatalf("sync: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			listWantFragment(t, "sync stderr", errOut, "takes no list")
			if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
				t.Errorf("a policy error must write NOTHING; repo changed")
			}
		})
	}
}

// SPEC R-33/grammar: a malformed list is exit 3 with a message naming the
// defect. An unclosed bracket must not be read as an owner token called
// "[@a, @b" and a nested bracket must not be read as a group: R-33 adds a
// flat list, and the day it grows structure it stops being the spelling of
// one intent. The bad-token case inherits set_owners' existing wording so one
// grammar keeps one diagnosis.
func TestR33_MalformedListIsExit3(t *testing.T) {
	cases := []struct {
		op       string
		fragment string
	}{
		{"add_owner(/services/api/, [@a, @b)", "owner list"},
		{"remove_owner(/docs/, [@a, @b)", "owner list"},
		{"add_owner(/services/api/, [@a, [@b]])", "invalid owner token"},
		{"add_owner(/services/api/, [@a, not-an-owner])", "invalid owner token"},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			repo := listBothRepo(t)
			snap := checkDirSnapshot(t, repo)
			code, _, errOut := runCLI(t, "sync", "--repo", repo, "--on-empty", "error", "--op", tc.op)
			if code != cli.ExitInvalid {
				t.Fatalf("want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			listWantFragment(t, "stderr", errOut, tc.fragment)
			if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
				t.Errorf("a policy error must write NOTHING; repo changed")
			}
		})
	}
}

// SPEC R-33: a trailing comma is the same list. `set_owners` accepts one
// today (asserted below, so the premise is visible rather than assumed), and
// R-33 exists to remove an asymmetry between the verbs — refusing it on
// add_owner alone would create a second one, where a generator's output is
// legal in one op and a policy error in the other.
func TestR33_TrailingCommaFollowsTheSetOwnersGrammar(t *testing.T) {
	// Pin, passes today: set_owners' tolerance, which the clause below
	// inherits. It runs as its own subtest so it still reports when the
	// add_owner half fails; if this one ever goes red, tighten both verbs
	// together rather than letting the grammars diverge.
	t.Run("pin: set_owners accepts a trailing comma", func(t *testing.T) {
		repo := listRepo(t)
		code, _, errOut := runCLI(t, "sync", "--repo", repo,
			"--op", "set_owners(/services/api/, [@org/api-team, @org/platform,])",
			"--on-empty", "error")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"* @org/original\n/services/api/ @org/api-team @org/platform\n")
	})
	t.Run("add_owner", func(t *testing.T) {
		repo := listRepo(t)
		code, _, errOut := runCLI(t, "sync", "--repo", repo,
			"--op", "add_owner(/services/api/, [@org/api-team, @org/platform,])")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"* @org/original\n/services/api/ @org/original @org/api-team @org/platform\n")
	})
}

// SPEC R-33a/R-19: a list converges. All-present is a byte-identical no-op
// reported as `unchanged`, a partly-present list adds only what is missing
// (and never a second copy of an owner already on the line), and a second
// identical run changes nothing — the property that lets a nightly fleet job
// re-run the same policy forever.
func TestR33_ListIsIdempotentAndAddsOnlyWhatIsMissing(t *testing.T) {
	op := "add_owner(/services/api/, [@org/api-team, @org/platform])"
	t.Run("every owner already present", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS":        "* @org/original\n/services/api/ @org/original @org/api-team @org/platform\n",
			"services/api/a.go": "package api\n",
		})
		before := syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))
		code, out, errOut := runCLI(t, "sync", "--repo", repo, "--op", op, "--format", "json")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if rec := syncDecodeRecord(t, out); rec.Status != cli.StatusUnchanged {
			t.Errorf("status = %q, want unchanged", rec.Status)
		}
		listWantFile(t, filepath.Join(repo, "CODEOWNERS"), before)
	})
	t.Run("some owners already present", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS":        "* @org/original\n/services/api/ @org/original @org/api-team\n",
			"services/api/a.go": "package api\n",
		})
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", op)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"* @org/original\n/services/api/ @org/original @org/api-team @org/platform\n")
	})
	t.Run("second run is byte-identical", func(t *testing.T) {
		repo := listRepo(t)
		if code, _, e := runCLI(t, "sync", "--repo", repo, "--op", op); code != cli.ExitOK {
			t.Fatalf("first run: want exit 0, got %d\n%s", code, e)
		}
		first := syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", op)
		if code != cli.ExitOK {
			t.Fatalf("second run: want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if second := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); second != first {
			t.Errorf("second run changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
		}
	})
}

// SPEC R-33/R-21: an owner list changes WHO an op names, never WHETHER the op
// runs. A zero-match scope is still disposed of by on_zero_match — require
// refuses this repo at exit 2 naming the scope (not the owners), skip is a
// clean no-op, and declare writes ONE line carrying the whole list, which is
// R-33b on the path where no tracked file exists to fold against.
func TestR33_ZeroMatchScopeIsUnaffectedByTheList(t *testing.T) {
	list := "add_owner(/ghost/, [@a, @b])"
	t.Run("require refuses this repo", func(t *testing.T) {
		repo := listRepo(t)
		snap := checkDirSnapshot(t, repo)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", list)
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFragment(t, "stderr", errOut, `scope "/ghost/" matches zero tracked files`)
		if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
			t.Errorf("refusal must write nothing; repo changed")
		}
	})
	t.Run("skip writes nothing", func(t *testing.T) {
		repo := listRepo(t)
		snap := checkDirSnapshot(t, repo)
		pol := syncWritePolicy(t, `{"version":1,"ops":[
			{"id":"g","op":"add_owner(/ghost/, [@a, @b])","on_zero_match":"skip"}]}`)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
			t.Errorf("a skipped op must write nothing; repo changed")
		}
	})
	t.Run("declare writes one line naming every owner", func(t *testing.T) {
		repo := listRepo(t)
		pol := syncWritePolicy(t, `{"version":1,"ops":[
			{"id":"g","op":"add_owner(/ghost/, [@a, @b])","on_zero_match":"declare"}]}`)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFile(t, filepath.Join(repo, "CODEOWNERS"), "* @org/original\n/ghost/ @a @b\n")
	})
}

// SPEC R-33/R-8: commutation is decided over every owner the op names. An
// implementation that stores the list beside the existing single-owner field
// and leaves ops.commutes reading that field admits
// `add_owner(/x/, [@a, @b])` batched with `remove_owner(/x/, @b)` — an
// order-dependent pair on every repository, which is precisely the batch R-8
// exists to refuse. The commuting pair is asserted alongside it: a
// conservative implementation that refuses every list-carrying batch would
// otherwise satisfy the first half and break every real policy.
func TestR33_R8CommutationSeesEveryOwnerInTheList(t *testing.T) {
	t.Run("non-commuting pair still refuses", func(t *testing.T) {
		pol := syncWritePolicy(t, `{"version":1,"on_empty":"error","ops":[
			"add_owner(/services/api/, [@a, @b])",
			"remove_owner(/services/api/, @b)"]}`)
		code, _, errOut := runCLI(t, "check", "--policy", pol)
		if code != cli.ExitInvalid {
			t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFragment(t, "check stderr", errOut, "R-8")

		repo := listRepo(t)
		snap := checkDirSnapshot(t, repo)
		code, _, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol)
		if code != cli.ExitInvalid {
			t.Fatalf("sync: want exit 3, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFragment(t, "sync stderr", errOut, "R-8")
		if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
			t.Errorf("a policy error must write NOTHING; repo changed")
		}
	})
	t.Run("commuting pair is accepted", func(t *testing.T) {
		pol := syncWritePolicy(t, `{"version":1,"on_empty":"error","ops":[
			"add_owner(/services/api/, [@a, @b])",
			"remove_owner(/services/api/, @c)"]}`)
		if code, _, e := runCLI(t, "check", "--policy", pol); code != cli.ExitOK {
			t.Fatalf("check: want exit 0, got %d\n%s", code, e)
		}
		repo := listRepo(t)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("sync: want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"* @org/original\n/services/api/ @org/original @a @b\n")
	})
}

// SPEC R-33/R-25: the blast-radius ceiling counts PATHS whose owners change,
// and a list changes no more paths than the bare form does. Six owners over a
// two-file scope is two paths, not six and not twelve; a ceiling of 2 must
// pass and a ceiling of 1 must refuse with the honest number, or every fleet
// operator who set a ceiling has to raise it whenever a policy names one more
// team.
func TestR33_BlastRadiusCountsPathsNotOwners(t *testing.T) {
	owners := "[@t1, @t2, @t3, @t4, @t5, @t6]"
	op := "add_owner(/services/api/, " + owners + ")"
	t.Run("under the ceiling", func(t *testing.T) {
		repo := listRepo(t)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", op, "--max-paths-changed", "2")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"* @org/original\n/services/api/ @org/original @t1 @t2 @t3 @t4 @t5 @t6\n")
	})
	t.Run("over the ceiling", func(t *testing.T) {
		repo := listRepo(t)
		snap := checkDirSnapshot(t, repo)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", op, "--max-paths-changed", "1")
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
		}
		listWantFragment(t, "stderr", errOut, "change the owners of 2 path(s)")
		if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
			t.Errorf("refusal must write nothing; repo changed")
		}
	})
}

// SPEC R-33: an owner list holds every owner spelling the single-owner form
// holds — an @org/team, a bare @user, and an email owner (R-13) — in one
// list, on one line. A list grammar that only recognized the `@org/team`
// shape would pass every other test in this file.
func TestR33_ListHoldsEveryOwnerSpelling(t *testing.T) {
	repo := listRepo(t)
	code, _, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/services/api/, [dev@example.com, @octocat, @org/api-team])")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	listWantFile(t, filepath.Join(repo, "CODEOWNERS"),
		"* @org/original\n/services/api/ @org/original dev@example.com @octocat @org/api-team\n")
}

// SPEC R-22/R-33: `check` must ACCEPT the valid list grammar. Every negative
// case in this file exits 3 today for an unrelated reason — the whole
// grammar is refused — so without this test an implementation whose
// validator rejects legal lists halts every fleet script at line one and
// nothing in the suite goes red.
func TestR33_CheckAcceptsValidListPolicies(t *testing.T) {
	for name, src := range map[string]string{
		"motivating add":    `{"version":1,"ops":["add_owner(/services/api/, [@org/api-team, @org/platform])"]}`,
		"motivating remove": `{"version":1,"on_empty":"error","ops":["remove_owner(/docs/, [@org/legacy, @org/former])"]}`,
		"single element":    `{"version":1,"ops":["add_owner(/services/api/, [@org/api-team])"]}`,
		"with except":       `{"version":1,"ops":["add_owner(/.github/ except /.github/CODEOWNERS, [@org/team_a, @org/team_b])"]}`,
		"object form with on_zero_match": `{"version":1,"ops":[
			{"id":"g","op":"add_owner(/services/api/, [@org/api-team, @org/platform])","on_zero_match":"skip"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			pol := syncWritePolicy(t, src)
			code, _, errOut := runCLI(t, "check", "--policy", pol)
			if code != cli.ExitOK {
				t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
			}
		})
	}
}

// SPEC R-33: --dry-run previews a list op and writes nothing. The fleet
// preview of "which repos would gain these two teams" must not require
// mutating one repository, and must not be an empty preview either.
func TestR33_DryRunPreviewsTheListWithoutWriting(t *testing.T) {
	repo := listRepo(t)
	snap := checkDirSnapshot(t, repo)
	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/services/api/, [@org/api-team, @org/platform])",
		"--dry-run", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("--dry-run must write nothing; repo changed")
	}
	rec := syncDecodeRecord(t, out)
	if !rec.DryRun {
		t.Errorf("dry_run = false, want true")
	}
	if len(rec.Changes) != 1 {
		t.Fatalf("want exactly 1 previewed change (R-33b), got %d: %+v", len(rec.Changes), rec.Changes)
	}
	if rec.Changes[0].NewLine != "/services/api/ @org/original @org/api-team @org/platform" {
		t.Errorf("previewed new_line = %q", rec.Changes[0].NewLine)
	}
	if _, err := os.Stat(filepath.Join(repo, "CODEOWNERS")); err != nil {
		t.Fatalf("fixture lost its CODEOWNERS: %v", err)
	}
}
