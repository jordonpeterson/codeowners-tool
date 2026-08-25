// Except-clause end-to-end tests (R-26…R-32). Written ahead
// of the implementation per CONTRIBUTING.md; every negative case asserts a
// message fragment as well as the exit code, because today the whole grammar
// dies at checkScope's whitespace rule with exit 3 — the same code several of
// these tests expect — and an exit-code-only assertion would pass vacuously
// against a feature that does not exist.
//
// Mutating tests assert EXACT file bytes or snapshot-level resolution, never
// line existence: under last-match-wins (S-1) a substring check is satisfied
// by a file whose line ORDER hands the excepted path to the grantee
// (adversarial-review finding — an append-at-EOF implementation threaded
// through every Contains-based oracle in the first draft).
//
// Three cases pass against today's binary by design and are labeled as pins
// (TestR27_UppercaseExceptStaysRefused, TestR32_NonExceptRecordsCarryNoExceptFields,
// and the escaped-scope subtest of TestR26_EscapedSpaceIsNotAnExceptDelimiter):
// they freeze current behavior the spec promises to preserve, rather than
// specify new behavior. Everything else fails until the feature lands.
package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// exceptRepo is the motivating fixture: the file at the highest-precedence
// location (S-8), one catch-all owner, real content under .github/ and outside
// it so INV-2 has something to protect on both sides of the scope.
func exceptRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		".github/CODEOWNERS":       "* @org/original_team\n",
		".github/workflows/ci.yml": "name: ci\n",
		".github/dependabot.yml":   "version: 2\n",
		"src/main.go":              "package main\n",
	})
}

// exceptDecodeRaw decodes a sync record WITHOUT the typed struct: the fields
// under test (excepted, except_unmatched, proven) do not exist on
// cli.SyncRecord until the feature lands, and these tests must compile — and
// fail — today.
func exceptDecodeRaw(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("record is not valid JSON: %v\nraw:\n%s", err, out)
	}
	return m
}

// exceptOp0 digs ops[0] out of a raw record.
func exceptOp0(t *testing.T, rec map[string]any) map[string]any {
	t.Helper()
	opsAny, ok := rec["ops"].([]any)
	if !ok || len(opsAny) == 0 {
		t.Fatalf("record has no ops[]: %v", rec)
	}
	op, ok := opsAny[0].(map[string]any)
	if !ok {
		t.Fatalf("ops[0] is not an object: %v", opsAny[0])
	}
	return op
}

// exceptOwnership resolves the whole repo via `snapshot` — the oracle for
// batch tests where commuting ops may legally produce different BYTES in
// different orders, but must produce identical RESOLUTION.
func exceptOwnership(t *testing.T, repo string) map[string][]string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "snap.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", repo, "--out", out); code != cli.ExitOK {
		t.Fatalf("snapshot: exit %d\n%s", code, e)
	}
	var snap struct {
		Ownership map[string][]string `json:"ownership"`
	}
	if err := json.Unmarshal([]byte(syncReadFile(t, out)), &snap); err != nil {
		t.Fatalf("snapshot JSON: %v", err)
	}
	for _, owners := range snap.Ownership {
		sort.Strings(owners) // owners are a set (OwnersEqual); order is presentation
	}
	return snap.Ownership
}

func exceptWantFragment(t *testing.T, where, got, fragment string) {
	t.Helper()
	if !strings.Contains(got, fragment) {
		t.Errorf("%s must contain %q — the operator has to be able to tell this refusal from every other one.\ngot:\n%s",
			where, fragment, got)
	}
}

func exceptWantFile(t *testing.T, path, want string) {
	t.Helper()
	if got := syncReadFile(t, path); got != want {
		t.Errorf("file bytes:\n%s\nwant:\n%s", got, want)
	}
}

// SPEC R-26: the motivating carve-out is ONE op. `add_owner(/.github/ except
// /.github/CODEOWNERS, @org/team_a)` grants team_a co-ownership of .github/
// while the CODEOWNERS file itself keeps exactly its current owners — owners
// the policy never names, discovered per repo. Exact bytes: the carve line
// must restate the original owners and sit after the broad grant (R-29
// structural placement), so last-match-wins (S-1) resolves the excepted path
// to them.
func TestR26_ExceptCarveOutEndToEnd(t *testing.T) {
	repo := exceptRepo(t)
	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)",
		"--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"),
		"* @org/original_team\n"+
			"/.github/ @org/original_team @org/team_a\n"+
			"/.github/CODEOWNERS @org/original_team\n")
	rec := exceptDecodeRaw(t, out)
	if rec["status"] != "applied" {
		t.Errorf("status = %v, want applied", rec["status"])
	}
}

// SPEC R-26/R-32: except means DON'T TOUCH, not revoke. When the grantee
// already owns an excepted path via a pre-existing rule, the op succeeds,
// that rule keeps winning, and the record's `excepted` says so — the one
// place the don't-touch semantics could otherwise hide something a
// security-minded operator needs to see. Exact bytes matter here more than
// anywhere: an append-at-EOF implementation leaves the asserted LINE intact
// while the appended grant steals the path by last-match (INV-2 violation
// invisible to a substring check).
func TestR26_ExceptDoesNotRevokeExistingOwnership(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":       "* @org/original_team\n/.github/CODEOWNERS @org/team_a\n",
		".github/workflows/ci.yml": "name: ci\n",
	})
	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)",
		"--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	// The grant narrows `*` and is inserted after it; the pre-existing carve
	// rule FOLLOWS the grant and needs no synthesized replacement.
	exceptWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"),
		"* @org/original_team\n"+
			"/.github/ @org/original_team @org/team_a\n"+
			"/.github/CODEOWNERS @org/team_a\n")
	op := exceptOp0(t, exceptDecodeRaw(t, out))
	excepted, _ := op["excepted"].([]any)
	found := false
	for _, e := range excepted {
		if m, ok := e.(map[string]any); ok && m["path"] == ".github/CODEOWNERS" {
			found = true
			owners, _ := m["owners"].([]any)
			if !reflect.DeepEqual(owners, []any{"@org/team_a"}) {
				t.Errorf("excepted owners = %v, want [@org/team_a] — the record is where the don't-touch≠revoke surprise surfaces", owners)
			}
		}
	}
	if !found {
		t.Errorf("record.ops[0].excepted must list .github/CODEOWNERS; got %v", op["excepted"])
	}
}

// SPEC R-26/R-29: multiple excepts are a flat list; one carve line per
// excepted pattern, in except-list order, all placed after the grant they
// correct. Exact bytes also pin that the grant line itself EXISTS — the
// first draft never asserted it, so a carve-only implementation that never
// granted anything passed (adversarial-review finding).
func TestR26_MultipleExceptsEachKeepOwners(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":       "* @org/original_team\n",
		".github/workflows/ci.yml": "name: ci\n",
		".github/FUNDING.yml":      "github: x\n",
	})
	code, _, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/.github/ except /.github/CODEOWNERS /.github/FUNDING.yml, @org/team_a)",
		"--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"),
		"* @org/original_team\n"+
			"/.github/ @org/original_team @org/team_a\n"+
			"/.github/CODEOWNERS @org/original_team\n"+
			"/.github/FUNDING.yml @org/original_team\n")
}

// SPEC R-26/R-29: except applies to every verb, and R-29 carves for every
// line the op writes OR AMENDS — set_owners and remove_owner amend `/x/` in
// place, which captures the excepted path until the carve restores it. Exact
// bytes pin both halves: the base mutation actually happened (the first
// draft never checked, so a carve-only no-op implementation passed) and the
// excepted path kept its owners.
func TestR26_SetOwnersAndRemoveOwnerHonorExcept(t *testing.T) {
	t.Run("set_owners", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS": "/x/ @a @b\n",
			"x/main.go":  "package x\n",
			"x/keep.md":  "keep\n",
		})
		code, _, errOut := runCLI(t, "sync", "--repo", repo,
			"--op", "set_owners(/x/ except /x/keep.md, [@new])", "--format", "json")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		exceptWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"/x/ @new\n"+
				"/x/keep.md @a @b\n")
	})
	t.Run("remove_owner", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS": "/x/ @a @b\n",
			"x/main.go":  "package x\n",
			"x/keep.md":  "keep\n",
		})
		pol := syncWritePolicy(t, `{"version":1,"on_empty":"error",
			"ops":[{"id":"rm","op":"remove_owner(/x/ except /x/keep.md, @b)"}]}`)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		exceptWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"/x/ @a\n"+
				"/x/keep.md @a @b\n")
	})
}

// SPEC R-26a/grammar: escaped whitespace is NEVER an except delimiter. A
// strings.Fields-style splitter passes every other test in this file and
// breaks exactly here (adversarial-review finding): a directory literally
// named "a except b" must stay one scope, and an except pattern containing
// an escaped space must stay one pattern.
func TestR26_EscapedSpaceIsNotAnExceptDelimiter(t *testing.T) {
	// Pin, passes today: escaped-space scopes already parse; the future
	// tokenizer must keep treating escaped whitespace as part of the token.
	t.Run("scope containing escaped ' except '", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS":      "* @o\n",
			"a except b/f.md": "x\n",
			"other/g.md":      "y\n",
		})
		code, _, errOut := runCLI(t, "sync", "--repo", repo,
			"--op", `add_owner(/a\ except\ b/, @x)`, "--format", "json")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		exceptWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"* @o\n"+
				`/a\ except\ b/ @o @x`+"\n")
	})
	t.Run("except pattern with escaped space", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS":  "* @o\n",
			"docs/a b.md": "x\n",
			"docs/c.md":   "y\n",
		})
		code, _, errOut := runCLI(t, "sync", "--repo", repo,
			"--op", `add_owner(/docs/ except /docs/a\ b.md, @x)`, "--format", "json")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		exceptWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"* @o\n"+
				"/docs/ @o @x\n"+
				`/docs/a\ b.md @o`+"\n")
	})
}

// SPEC R-19/R-26: an except op converges. Run twice: second run exits 0,
// changes nothing, and the file is byte-identical — the property that lets a
// nightly fleet job re-run the same policy forever.
func TestR19_ExceptIsIdempotent(t *testing.T) {
	repo := exceptRepo(t)
	op := "add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)"
	if code, _, e := runCLI(t, "sync", "--repo", repo, "--op", op); code != cli.ExitOK {
		t.Fatalf("first run: want exit 0, got %d\n%s", code, e)
	}
	first := syncReadFile(t, filepath.Join(repo, ".github/CODEOWNERS"))
	code, _, e := runCLI(t, "sync", "--repo", repo, "--op", op)
	if code != cli.ExitOK {
		t.Fatalf("second run: want exit 0, got %d\n%s", code, e)
	}
	if second := syncReadFile(t, filepath.Join(repo, ".github/CODEOWNERS")); first != second {
		t.Errorf("second run changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// SPEC R-19/R-28: idempotence must also hold on the ALLOW path, where run 2
// sees the grant present but NO carve line for the unmatched except — the
// state a naive "carve missing, so not yet applied" detector re-appends or
// refuses on, every night, until S-4 stops loading the file
// (adversarial-review finding).
func TestR19_ExceptAllowIsIdempotent(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":               "* @org/original_team\n",
		".github/workflows/ci.yml": "name: ci\n",
	})
	pol := syncWritePolicy(t, `{"version":1,"ops":[
		{"id":"g","op":"add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)",
		 "on_except_zero_match":"allow"}]}`)
	if code, _, e := runCLI(t, "sync", "--repo", repo, "--policy", pol); code != cli.ExitOK {
		t.Fatalf("first run: want exit 0, got %d\n%s", code, e)
	}
	first := syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))
	code, out, e := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("second run: want exit 0, got %d\n%s", code, e)
	}
	if second := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); first != second {
		t.Errorf("second run changed bytes:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	op := exceptOp0(t, exceptDecodeRaw(t, out))
	un, _ := op["except_unmatched"].([]any)
	if !reflect.DeepEqual(un, []any{"/.github/CODEOWNERS"}) {
		t.Errorf("run 2 must still disclose the inert except; except_unmatched = %v", un)
	}
}

// SPEC R-27: every static defect in an except clause is a POLICY error — exit
// 3, identical in every repo, caught by `check` before repo 1 — and each
// message names its defect, because "fix the policy" is only actionable when
// the operator can tell WHICH of the R-27 rules fired.
func TestR27_StaticExceptDefectsAreExit3(t *testing.T) {
	cases := []struct {
		name     string
		policy   string
		fragment string
	}{
		// Fragments below are deliberately phrases only the R-27 messages will
		// contain. The obvious fragments pass VACUOUSLY today: the rename case's
		// current error quotes the token "@a except @b", the enum case's quotes
		// the unknown field name — both "contain except" for the wrong reason.
		{"not contained in scope",
			`{"version":1,"ops":["add_owner(/.github/ except /docs/README.md, @a)"]}`,
			"not provably contained"},
		{"equal to scope",
			`{"version":1,"ops":["add_owner(/.github/ except /.github/, @a)"]}`,
			"entire scope"},
		// R-27.2 is over the PATTERN LANGUAGE: /x/** spells /x/ differently, and
		// a string-equality check waves it through — then every repo fails at
		// exit 2 where the contract demands one exit 3 (adversarial-review
		// finding; Contains holds in both directions for this pair).
		{"language-equal to scope",
			`{"version":1,"ops":["add_owner(/x/ except /x/**, @a)"]}`,
			"entire scope"},
		{"duplicate except",
			`{"version":1,"ops":["add_owner(/.github/ except /.github/CODEOWNERS /.github/CODEOWNERS, @a)"]}`,
			"duplicate except"},
		// Same language-not-string rule for duplicates: /x/gen/ and /x/gen/**
		// are one pattern spelled twice.
		{"language-equal duplicate except",
			`{"version":1,"ops":["add_owner(/x/ except /x/gen/ /x/gen/**, @a)"]}`,
			"duplicate except"},
		{"except on rename_owner",
			`{"version":1,"ops":["rename_owner(@a except @b, @c)"]}`,
			"takes no scope"},
		{"declare with except",
			`{"version":1,"ops":[{"op":"add_owner(/.github/ except /.github/CODEOWNERS, @a)","on_zero_match":"declare"}]}`,
			"cannot encode subtraction"},
		{"bad on_except_zero_match enum",
			`{"version":1,"ops":[{"op":"add_owner(/.github/ except /.github/CODEOWNERS, @a)","on_except_zero_match":"maybe"}]}`,
			`"require" or "allow"`},
		{"on_except_zero_match without except",
			`{"version":1,"ops":[{"op":"add_owner(/.github/, @a)","on_except_zero_match":"allow"}]}`,
			"no except clause"},
		{"missing pattern after keyword",
			`{"version":1,"ops":["add_owner(/.github/ except , @a)"]}`,
			"no patterns"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol := syncWritePolicy(t, tc.policy)
			// check first: the whole point of R-27 is dying before any repo.
			code, _, errOut := runCLI(t, "check", "--policy", pol)
			if code != cli.ExitInvalid {
				t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			exceptWantFragment(t, "check stderr", errOut, tc.fragment)

			repo := exceptRepo(t)
			snap := checkDirSnapshot(t, repo)
			code, _, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol)
			if code != cli.ExitInvalid {
				t.Fatalf("sync: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			exceptWantFragment(t, "sync stderr", errOut, tc.fragment)
			if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
				t.Errorf("a policy error must write NOTHING; repo changed")
			}
		})
	}
}

// SPEC R-22/R-27: `check` must also ACCEPT valid except policies — every
// R-27 case above exits 3 today for an unrelated reason (the whitespace
// rule), so without this test an implementation whose validator rejects the
// valid grammar, or whose containment prover is too weak for a legal glob
// except, halts every fleet script at line one and nothing goes red
// (adversarial-review finding).
func TestR22_CheckAcceptsValidExceptPolicies(t *testing.T) {
	for name, src := range map[string]string{
		"motivating": `{"version":1,"ops":["add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)"]}`,
		"layered": `{"version":1,"ops":[
			"add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)",
			"add_owner(/.github/CODEOWNERS, @org/platform)"]}`,
		"glob except": `{"version":1,"ops":["add_owner(/src/ except /src/**/*.pb.go, @org/team_a)"]}`,
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

// Pin, passes today: the keyword is lowercase `except`, exactly. `EXCEPT`
// keeps hitting checkScope's whitespace refusal — the spec promises no new
// acceptance for near-misses, so this test freezes the current refusal
// against an implementation that tokenizes case-insensitively.
func TestR27_UppercaseExceptStaysRefused(t *testing.T) {
	repo := exceptRepo(t)
	code, _, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/.github/ EXCEPT /.github/CODEOWNERS, @a)")
	if code != cli.ExitInvalid {
		t.Fatalf("want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "stderr", errOut, "unescaped whitespace")
}

// SPEC R-28 (default): an except pattern matching zero tracked files refuses
// THIS repo, exit 2, nothing written. This is the guard that keeps the
// two-pass flow's protection: a repo whose CODEOWNERS still sits at the root
// has no /.github/CODEOWNERS to carve, and granting /.github/ there without
// the carve reopens the S-8 precedence-escalation hole — the grantee could
// self-approve CREATING /.github/CODEOWNERS and take the repo. Both
// fragments are required: "matches zero tracked files" alone is verbatim in
// today's R-5/R-21 scope messages, and a diagnosis blaming the SCOPE sends
// the operator hunting a typo in the wrong argument.
func TestR28_UnmatchedExceptRefusesByDefault(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":               "* @org/original_team\n",
		".github/workflows/ci.yml": "name: ci\n",
	})
	snap := checkDirSnapshot(t, repo)
	code, _, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)")
	if code != cli.ExitRefused {
		t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "stderr", errOut, "except pattern")
	exceptWantFragment(t, "stderr", errOut, "matches zero tracked files")
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("refusal must write nothing; repo changed")
	}
}

// SPEC R-28 (`allow`): the opt-out APPLIES the grant (exact bytes — a
// wrong implementation that skips the op entirely while reporting the
// unmatched pattern is the "wall of silent green" this knob must not
// become), records the inert pattern, warns, and marks the op
// proven:"structural" — the declare-class weakening made visible.
func TestR28_UnmatchedExceptAllowProceedsAndSurfaces(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":               "* @org/original_team\n",
		".github/workflows/ci.yml": "name: ci\n",
	})
	pol := syncWritePolicy(t, `{"version":1,"ops":[
		{"id":"g","op":"add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)",
		 "on_except_zero_match":"allow"}]}`)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFile(t, filepath.Join(repo, "CODEOWNERS"),
		"* @org/original_team\n"+
			"/.github/ @org/original_team @org/team_a\n")
	rec := exceptDecodeRaw(t, out)
	op := exceptOp0(t, rec)
	un, _ := op["except_unmatched"].([]any)
	if !reflect.DeepEqual(un, []any{"/.github/CODEOWNERS"}) {
		t.Errorf("except_unmatched = %v, want [/.github/CODEOWNERS]", un)
	}
	if op["proven"] != "structural" {
		t.Errorf("proven = %v, want structural — the allow path is a declare-class weakening and must be marked like one", op["proven"])
	}
	if warns, _ := rec["warnings"].([]any); len(warns) == 0 {
		t.Errorf("an inert except must warn; record has no warnings:\n%s", out)
	}
}

// SPEC R-28: emptiness questions are ORDERED — on_zero_match disposes of the
// op first, and on_except_zero_match is consulted only if the op will write.
// When the excepts swallow every in-scope path, the require message must say
// the emptiness came from the excepts, not the scope.
func TestR28_FullySweptScopeFollowsOnZeroMatch(t *testing.T) {
	files := map[string]string{
		"CODEOWNERS": "* @org/original_team\n",
		"x/only.md":  "x\n",
	}
	t.Run("require refuses and names the excepts", func(t *testing.T) {
		repo := initRepo(t, files)
		code, _, errOut := runCLI(t, "sync", "--repo", repo,
			"--op", "add_owner(/x/ except /x/only.md, @a)")
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
		}
		exceptWantFragment(t, "stderr", errOut, "excepted")
	})
	t.Run("skip proceeds", func(t *testing.T) {
		repo := initRepo(t, files)
		before := syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))
		pol := syncWritePolicy(t, `{"version":1,"ops":[
			{"id":"g","op":"add_owner(/x/ except /x/only.md, @a)","on_zero_match":"skip"}]}`)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != before {
			t.Errorf("skip must write nothing; file changed:\n%s", got)
		}
	})
	// The ordering case proper: scope matches NOTHING, so on_zero_match=skip
	// must win outright and the defaulted on_except_zero_match=require must
	// never fire — otherwise "if this repo has /services/, ..." policies
	// refuse on every repo that lacks the directory (adversarial-review
	// finding: the first spec draft left this precedence undefined).
	t.Run("zero-match scope with skip never consults the except", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS": "* @org/original_team\n",
			"top.md":     "x\n",
		})
		before := syncReadFile(t, filepath.Join(repo, "CODEOWNERS"))
		pol := syncWritePolicy(t, `{"version":1,"ops":[
			{"id":"g","op":"add_owner(/services/ except /services/legacy/, @a)","on_zero_match":"skip"}]}`)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0 (skip wins), got %d\nstderr:\n%s", code, errOut)
		}
		if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != before {
			t.Errorf("skipped op must write nothing; file changed:\n%s", got)
		}
	})
}

// SPEC R-28/R-23: --create meets except fail-closed. Under the default, the
// motivating op on a repo with NO CODEOWNERS refuses (the excepted path is
// untracked → zero-match require) and creates nothing — because a created
// file would have no original owners to preserve and the grant would land
// carve-free, which is the S-8 hole again (adversarial-review finding).
func TestR28_CreateWithExceptRefusesByDefault(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/workflows/ci.yml": "name: ci\n",
		"src/main.go":              "package main\n",
	})
	snap := checkDirSnapshot(t, repo)
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--create",
		"--op", "add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)")
	if code != cli.ExitRefused {
		t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(repo, ".github/CODEOWNERS")); !os.IsNotExist(err) {
		t.Errorf("a refusal must not leave a created file behind (created: false)")
	}
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("refusal must write nothing; repo changed")
	}
}

// SPEC R-29: exact bytes for the one fixture with an unrelated LATER rule —
// where placement bugs live. The grant narrows `*` and is inserted after it;
// the carve goes immediately after the grant (structural placement), NOT at
// end of file; the workflows rule is amended in place and keeps its
// precedence; every comment and blank survives verbatim. The changes[]
// entry for the carve must name the except — reviewers must never meet an
// unexplained owner in a diff.
func TestR29_CarveLineExplainsItselfAndPreservesBytes(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":       "# fallback\n* @org/original_team\n\n# infra's pipeline\n/.github/workflows/ @org/infra\n",
		".github/workflows/ci.yml": "name: ci\n",
		".github/dependabot.yml":   "version: 2\n",
	})
	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)",
		"--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"),
		"# fallback\n"+
			"* @org/original_team\n"+
			"/.github/ @org/original_team @org/team_a\n"+
			"/.github/CODEOWNERS @org/original_team\n"+
			"\n"+
			"# infra's pipeline\n"+
			"/.github/workflows/ @org/infra @org/team_a\n")
	rec := exceptDecodeRaw(t, out)
	changes, _ := rec["changes"].([]any)
	carveExplained := false
	for _, c := range changes {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if m["pattern"] == "/.github/CODEOWNERS" {
			reason, _ := m["reason"].(string)
			if strings.Contains(reason, "except") {
				carveExplained = true
			}
		}
	}
	if !carveExplained {
		t.Errorf("changes[] must contain a carve insert for /.github/CODEOWNERS whose reason names the except; got %v", changes)
	}
}

// SPEC R-29: an excepted path that currently matches NO rule is uncarvable.
// nil (unmatched) and [] (zero-owner rule, S-9) are distinct resolved states
// and never equal (OwnersEqual), so once a broad grant line captures the
// path there is no writable line that restores "unmatched" — INV-2 is
// unsatisfiable and the tool must refuse rather than quietly convert "nobody
// owns this" into "a rule says nobody owns this".
func TestR29_UnmatchedExceptedPathRefuses(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "/x/owned.go @a\n",
		"x/owned.go": "package x\n",
		"x/free.md":  "unowned\n",
	})
	snap := checkDirSnapshot(t, repo)
	code, _, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/x/ except /x/free.md, @b)")
	if code != cli.ExitRefused {
		t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "stderr", errOut, "matches no rule")
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("refusal must write nothing; repo changed")
	}
}

// SPEC R-31: excepts make the layered-delegation batch disjoint, so ONE
// policy expresses "team_a gets .github/, platform gets the CODEOWNERS file"
// — the pair R-8 refuses today. The oracle is RESOLUTION via snapshot, not
// bytes: commuting ops may legally produce different line layouts in
// different orders, but must resolve identically — so the same assertions
// run for both op orders, which is the literal meaning of "these commute"
// (first-draft gap: one order, substring oracles, and a mixed-machinery
// placement bug passed).
func TestR31_ExceptMakesLayeredBatchDisjoint(t *testing.T) {
	policies := map[string]string{
		"grant first": `{"version":1,"ops":[
			{"id":"grant","op":"add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)"},
			{"id":"carve","op":"set_owners(/.github/CODEOWNERS, [@org/platform])"}]}`,
		"carve first": `{"version":1,"ops":[
			{"id":"carve","op":"set_owners(/.github/CODEOWNERS, [@org/platform])"},
			{"id":"grant","op":"add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)"}]}`,
	}
	for name, src := range policies {
		t.Run(name, func(t *testing.T) {
			repo := exceptRepo(t)
			pol := syncWritePolicy(t, src)
			code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
			if code != cli.ExitOK {
				t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
			}
			if rec := syncDecodeRecord(t, out); rec.OpsApplied != 2 {
				t.Errorf("ops_applied = %d, want 2 (out:\n%s)", rec.OpsApplied, out)
			}
			// Commit so snapshot resolves the written state.
			gitCommitAll(t, repo)
			own := exceptOwnership(t, repo)
			for path, want := range map[string][]string{
				".github/CODEOWNERS":       {"@org/platform"},
				".github/workflows/ci.yml": {"@org/original_team", "@org/team_a"},
				".github/dependabot.yml":   {"@org/original_team", "@org/team_a"},
				"src/main.go":              {"@org/original_team"},
			} {
				if !reflect.DeepEqual(own[path], want) {
					t.Errorf("%s resolves to %v, want %v", path, own[path], want)
				}
			}
		})
	}
}

// gitCommitAll commits the working tree so `snapshot` (which resolves
// against a ref) sees what sync just wrote.
func gitCommitAll(t *testing.T, repo string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "sync"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// SPEC R-31: excepts relax R-8 only where they actually create disjointness.
// Two ops still overlapping on a non-excepted path without commuting refuse —
// the relaxation must not become a hole. The refusal is exit 3, not 2: the
// grant's scope provably governs the workflows scope over the pattern language
// and no except removes it, so ops.StaticConflict decides the pair at the
// policy level, before any repo is opened — identically on check and sync.
func TestR31_ResidualOverlapStillRefuses(t *testing.T) {
	repo := exceptRepo(t)
	snap := checkDirSnapshot(t, repo)
	pol := syncWritePolicy(t, `{"version":1,"on_empty":"error","ops":[
		{"id":"grant","op":"add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)"},
		{"id":"rm","op":"remove_owner(/.github/workflows/, @org/team_a)"}]}`)
	code, _, errOut := runCLI(t, "check", "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("check: want exit 3 (static R-8), got %d\nstderr:\n%s", code, errOut)
	}
	code, _, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("sync: want exit 3 (static R-8, residual overlap on workflows), got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "stderr", errOut, "R-8")
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("refusal must write nothing; repo changed")
	}
}

// SPEC R-32: --dry-run emits the full record — excepted paths, the planned
// changes — and writes nothing. The fleet preview ("who ends up holding the
// carve-outs, in all 100 repos") must not require mutating a single repo,
// and must not be an except-special-cased EMPTY preview either.
func TestR32_DryRunSurfacesExceptedWithoutWriting(t *testing.T) {
	repo := exceptRepo(t)
	snap := checkDirSnapshot(t, repo)
	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)",
		"--dry-run", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("--dry-run must write nothing; repo changed")
	}
	rec := exceptDecodeRaw(t, out)
	if rec["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", rec["dry_run"])
	}
	if changes, _ := rec["changes"].([]any); len(changes) == 0 {
		t.Errorf("dry-run must still preview the plan; changes[] is empty:\n%s", out)
	}
	op := exceptOp0(t, rec)
	excepted, _ := op["excepted"].([]any)
	if len(excepted) != 1 {
		t.Fatalf("excepted = %v, want exactly the CODEOWNERS entry", op["excepted"])
	}
	m, _ := excepted[0].(map[string]any)
	if m["path"] != ".github/CODEOWNERS" ||
		!reflect.DeepEqual(m["owners"], []any{"@org/original_team"}) {
		t.Errorf("excepted[0] = %v, want {path: .github/CODEOWNERS, owners: [@org/original_team]}", m)
	}
}

// SPEC R-32: human output and --summary-out render the carve-out facts too.
// An implementation surfacing excepted paths only in JSON leaves the fleet
// PR reviewer — who reads the summary, not results.jsonl — blind to them.
func TestR32_HumanAndSummaryRenderExcepts(t *testing.T) {
	repo := exceptRepo(t)
	summary := filepath.Join(t.TempDir(), "summary.md")
	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)",
		"--summary-out", summary)
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "human stdout", out, ".github/CODEOWNERS")
	exceptWantFragment(t, "summary markdown", syncReadFile(t, summary), ".github/CODEOWNERS")
}

// Pin, passes today: R-32's schema promise is ADDITIVE — records for ops
// without an except clause carry no excepted/except_unmatched keys, so
// existing jq pipelines parse byte-identical records. Freezes the field
// names against an implementation that emits them empty on every op.
func TestR32_NonExceptRecordsCarryNoExceptFields(t *testing.T) {
	repo := syncSmallRepo(t)
	code, out, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/x/, @b)", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	op := exceptOp0(t, exceptDecodeRaw(t, out))
	for _, key := range []string{"excepted", "except_unmatched"} {
		if _, present := op[key]; present {
			t.Errorf("non-except op record must not carry %q (additive schema, omitempty)", key)
		}
	}
}

// SPEC R-26/glob: an except may be a glob, provided containment is provable.
// Exact bytes pin grant + carve + order. The zero-directory `**` case
// (src/a_gen.pb.go, no intermediate dir) rides on the carve line matching it
// — if the containment prover and the matcher disagree on `**`, the proof
// gate refuses and the exit-0 assertion goes red; a snapshot oracle would
// re-use the same matcher and prove nothing more (that cross-engine question
// belongs to the differential fuzz, not e2e).
func TestR26_GlobExcept(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":        "* @org/original_team\n",
		"src/a.go":          "package a\n",
		"src/a_gen.pb.go":   "package a\n",
		"src/b/b_gen.pb.go": "package b\n",
	})
	code, _, errOut := runCLI(t, "sync", "--repo", repo,
		"--op", "add_owner(/src/ except /src/**/*.pb.go, @org/team_a)", "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFile(t, filepath.Join(repo, "CODEOWNERS"),
		"* @org/original_team\n"+
			"/src/ @org/original_team @org/team_a\n"+
			"/src/**/*.pb.go @org/original_team\n")
}
