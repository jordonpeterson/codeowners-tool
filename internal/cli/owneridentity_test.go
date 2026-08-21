// Owner-identity end-to-end tests (docs/policy.md, R-38). Written ahead of
// the implementation per CONTRIBUTING.md.
//
// The vacuity trap in this area is unusually sharp: most of the broken cases
// exit 0 today with status "unchanged", which is also exactly what a CORRECT
// no-op produces. An assertion of the exit code or of `status == unchanged`
// alone would pass against the bug. Every test here therefore asserts the
// thing that actually differs — the EXACT file bytes, the resolved ownership
// read back through `snapshot`, or `plan`'s no-op exit code (1 vs 0), which
// is the one place the two outcomes are already distinguishable.
//
// Mutating tests never use strings.Contains on file content: under
// last-match-wins (S-1) a substring check is satisfied by a file whose line
// ORDER hands the path to the wrong owner.
//
// Negative cases assert a message fragment as well as an exit code, and every
// fragment contains a space so it cannot be satisfied by `policy.Error`'s
// leading filename or by the test name that `t.TempDir()` embeds in its path.
//
// Every subtest whose name begins with "pin:", plus the whole of
// TestR38f_SyncDoesNotCollapsePreExistingDuplicates and
// TestR38a_EmailOwnersStayCaseSensitive, PASSES against today's binary by
// design. They freeze behavior R-38 must PRESERVE — e-mail owners staying
// case-sensitive, a hand-written duplicate surviving an unrelated edit, a
// rename writing its new name verbatim — rather than specify new behavior, so
// a fix that over-folds or over-repairs turns them red. Several are also the
// PREMISE their failing sibling rests on: without them an exit code there
// could have come from anywhere, so they run as separate subtests and report
// even when that sibling fails. Everything else fails until R-38 lands.
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
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
)

// oiRepo is R-38's fixture: a catch-all owner so a removal always has
// something to fall through to, TWO files under /x/ so a paths-changed count
// is not confusable with an owner count, and one file outside the scope so
// INV-2 has something to protect.
func oiRepo(t *testing.T, codeowners string) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"CODEOWNERS": codeowners,
		"x/a.go":     "package x\n",
		"x/b.go":     "package x\n",
		"top.md":     "top\n",
	})
}

// oiCommit stages and commits the working tree so `snapshot` (which resolves
// against a ref, not the working tree) sees what sync just wrote.
// --allow-empty is load-bearing: the bug under test is a sync that writes
// NOTHING, and a helper that died on "nothing to commit" would report the
// wrong failure for exactly the cases this file exists to catch.
func oiCommit(t *testing.T, repo string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "--allow-empty", "-m", "sync"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// oiOwnership is the oracle for every claim of the form "X no longer owns Y".
// It reads resolution back out of the tool through `snapshot` rather than
// inspecting the file, because the removal contract is about who owns a path,
// not about which bytes happen to be on a line.
func oiOwnership(t *testing.T, repo string) map[string][]string {
	t.Helper()
	oiCommit(t, repo)
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

// oiWantNoSpellingOwns states the claim the way R-38c does: not "the token is
// gone" but "no owner that IS this owner resolves for the path". The identity
// it asks is ops.FoldOwner, which R-38a names as the only one.
func oiWantNoSpellingOwns(t *testing.T, own map[string][]string, path, owner string) {
	t.Helper()
	for _, got := range own[path] {
		if ops.FoldOwner(got) == ops.FoldOwner(owner) {
			t.Errorf("%s resolves to %v — %q is the same owner as %q under ops.FoldOwner (R-38a), so the owner the op named off this path still owns it",
				path, own[path], got, owner)
		}
	}
}

// oiWantFoldedOwners compares resolved owners as SETS UNDER THE IDENTITY. It
// is the right oracle where the spelling of the result is not itself
// specified — two commuting orders may legitimately write a handle
// differently, but R-38a makes them the same owner set.
func oiWantFoldedOwners(t *testing.T, own map[string][]string, path string, want []string) {
	t.Helper()
	fold := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, o := range in {
			out = append(out, ops.FoldOwner(o))
		}
		sort.Strings(out)
		return out
	}
	if got := fold(own[path]); !reflect.DeepEqual(got, fold(want)) {
		t.Errorf("%s resolves to %v (folded %v), want %v", path, own[path], got, fold(want))
	}
}

func oiWantOwners(t *testing.T, own map[string][]string, path string, want []string) {
	t.Helper()
	sort.Strings(want)
	if !reflect.DeepEqual(own[path], want) {
		t.Errorf("%s resolves to %v, want %v", path, own[path], want)
	}
}

func oiWantFile(t *testing.T, repo, want string) {
	t.Helper()
	if got := syncReadFile(t, filepath.Join(repo, "CODEOWNERS")); got != want {
		t.Errorf("CODEOWNERS bytes:\n%q\nwant:\n%q", got, want)
	}
}

func oiWantFragment(t *testing.T, where, got, fragment string) {
	t.Helper()
	if !strings.Contains(got, fragment) {
		t.Errorf("%s must contain %q — the operator has to be able to tell this refusal from every other one.\ngot:\n%s",
			where, fragment, got)
	}
}

// oiSync runs `sync --format json` and returns the exit code, the decoded
// record and stderr. The record is decoded only on the exit codes that write
// one (0 and 2); an exit-3 policy error writes none.
func oiSync(t *testing.T, repo string, args ...string) (int, cli.SyncRecord, string) {
	t.Helper()
	full := append([]string{"sync", "--repo", repo, "--format", "json"}, args...)
	code, out, errOut := runCLI(t, full...)
	if code != cli.ExitOK && code != cli.ExitRefused {
		return code, cli.SyncRecord{}, errOut
	}
	return code, syncDecodeRecord(t, out), errOut
}

// oiPlanExit runs `plan` and returns its exit code alone. `plan` is the only
// verb whose contract already separates "I would change something" (0) from
// "there is nothing to do" (1, R-17), so it is the sharpest available oracle
// for a no-op claim — sync collapses both into exit 0.
func oiPlanExit(t *testing.T, repo, op string) int {
	t.Helper()
	code, _, _ := runCLI(t, "plan", "--repo", repo, "--op", op)
	return code
}

// SPEC R-38c: a removal names an OWNER, not a spelling. `remove_owner(/x/,
// @ORG/TEAM)` against a file that wrote `@org/team` must leave that owner
// owning no in-scope path; today it reports "unchanged" at exit 0 and the
// team keeps the directory, so a fleet run of "revoke the departed team"
// reports converged on every repository that capitalised the handle. The
// oracle is the resolved ownership, not the bytes: "unchanged" and a correct
// no-op are indistinguishable by exit code, and only resolution says whether
// the owner is gone.
func TestR38c_RemoveTakesEverySpelling(t *testing.T) {
	cases := map[string]struct {
		seed, op, want string
	}{
		"op spells the handle differently than the file": {
			seed: "* @org/original\n/x/ @org/team @org/keep\n",
			op:   "remove_owner(/x/, @ORG/TEAM)",
			want: "* @org/original\n/x/ @org/keep\n",
		},
		"file spells the handle differently than the op": {
			seed: "* @org/original\n/x/ @Org/Team @org/keep\n",
			op:   "remove_owner(/x/, @org/team)",
			want: "* @org/original\n/x/ @org/keep\n",
		},
		"one line names both spellings and loses both": {
			seed: "* @org/original\n/x/ @org/team @Org/Team @org/keep\n",
			op:   "remove_owner(/x/, @org/team)",
			want: "* @org/original\n/x/ @org/keep\n",
		},
		// The narrower line names @org/keep as well as the departing owner, so
		// removing the owner leaves it with one. An earlier version of this
		// fixture had the narrower line naming the departing owner ALONE and
		// still expected "/x/a.go @org/keep": unreachable under every legal
		// on-empty policy, because writing an INHERITED owner onto a narrower
		// rule is not something any verb does. The bug under test is the
		// cross-line match, not R-6's empty-set handling.
		"the two spellings sit on different lines": {
			seed: "* @org/original\n/x/ @Org/Team @org/keep\n/x/a.go @org/team @org/keep\n",
			op:   "remove_owner(/x/, @ORG/TEAM)",
			want: "* @org/original\n/x/ @org/keep\n/x/a.go @org/keep\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := oiRepo(t, tc.seed)
			code, rec, errOut := oiSync(t, repo, "--op", tc.op)
			if code != cli.ExitOK {
				t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
			}
			if rec.Status != cli.StatusApplied {
				t.Errorf("status = %q, want %q — the owner owned an in-scope path, so there was work to do",
					rec.Status, cli.StatusApplied)
			}
			oiWantFile(t, repo, tc.want)
			own := oiOwnership(t, repo)
			for _, p := range []string{"x/a.go", "x/b.go"} {
				oiWantNoSpellingOwns(t, own, p, "@org/team")
			}
		})
	}
}

// SPEC R-38c: the spec's own example, where removal empties the line. Under
// --on-empty=inherit the rule is deleted and the in-scope paths fall through
// to the catch-all; a surviving spelling would instead keep the whole line
// alive, which is the difference this asserts.
func TestR38c_RemoveEmptiesALineNamingBothSpellings(t *testing.T) {
	repo := oiRepo(t, "* @org/original\n/x/ @org/team @Org/Team\n")
	code, rec, errOut := oiSync(t, repo, "--on-empty", "inherit", "--op", "remove_owner(/x/, @ORG/TEAM)")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	if rec.Status != cli.StatusApplied {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusApplied)
	}
	oiWantFile(t, repo, "* @org/original\n")
	own := oiOwnership(t, repo)
	for _, p := range []string{"x/a.go", "x/b.go"} {
		oiWantNoSpellingOwns(t, own, p, "@org/team")
		oiWantOwners(t, own, p, []string{"@org/original"})
	}
}

// SPEC R-38b: adding an owner who already owns every in-scope path is a
// no-op WHATEVER spelling either side used, and the line is left
// byte-identical — the file's existing spelling wins, because restyling a
// handle nobody asked to change churns a diff on every repository in a fleet.
// Both halves are asserted: exact bytes catch a rewrite-to-my-spelling
// implementation, which would also leave the owner owning and so satisfy any
// resolution-only oracle. `plan`'s exit code carries the no-op claim, since
// sync answers 0 either way.
func TestR38b_AddIsANoOpWhenTheOwnerAlreadyOwns(t *testing.T) {
	cases := map[string]struct{ seed, op string }{
		"file lowercase, op mixed case": {
			seed: "* @org/original\n/x/ @org/team\n",
			op:   "add_owner(/x/, @Org/Team)",
		},
		"file mixed case, op lowercase": {
			seed: "* @org/original\n/x/ @Org/Team\n",
			op:   "add_owner(/x/, @org/team)",
		},
		"a third spelling against a line already naming two": {
			seed: "* @org/original\n/x/ @org/team @Org/Team\n",
			op:   "add_owner(/x/, @ORG/TEAM)",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := oiRepo(t, tc.seed)
			if got := oiPlanExit(t, repo, tc.op); got != cli.ExitNoOp {
				t.Errorf("plan exit = %d, want %d — the owner already owns every in-scope path, so there is no change to plan", got, cli.ExitNoOp)
			}
			code, rec, errOut := oiSync(t, repo, "--op", tc.op)
			if code != cli.ExitOK {
				t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
			}
			if rec.Status != cli.StatusUnchanged {
				t.Errorf("status = %q, want %q", rec.Status, cli.StatusUnchanged)
			}
			oiWantFile(t, repo, tc.seed)
		})
	}
}

// SPEC R-38d: rename matches the old name under the same identity as every
// other verb, and writes the new name EXACTLY as the op spells it — a rename
// is the one verb whose purpose is to change the text, so it is the one place
// a spelling change is intended. Today the match is byte equality, so a
// differently cased old name renames nothing and reports "unchanged".
func TestR38d_RenameMatchesTheOldNameUnderTheIdentity(t *testing.T) {
	t.Run("differently cased old name still renames", func(t *testing.T) {
		repo := oiRepo(t, "* @org/original\n/x/ @org/team\n")
		if got := oiPlanExit(t, repo, "rename_owner(@ORG/TEAM, @org/new-team)"); got != cli.ExitOK {
			t.Errorf("plan exit = %d, want %d — @org/team is on the file, so the rename has work to do", got, cli.ExitOK)
		}
		code, rec, errOut := oiSync(t, repo, "--op", "rename_owner(@ORG/TEAM, @org/new-team)")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if rec.Status != cli.StatusApplied {
			t.Errorf("status = %q, want %q", rec.Status, cli.StatusApplied)
		}
		oiWantFile(t, repo, "* @org/original\n/x/ @org/new-team\n")
		own := oiOwnership(t, repo)
		oiWantOwners(t, own, "x/a.go", []string{"@org/new-team"})
		oiWantNoSpellingOwns(t, own, "x/a.go", "@org/team")
	})

	// A rename is global (§4.1), so every spelling on every line is the same
	// owner and every one of them moves. Two lines, two spellings, one op.
	t.Run("every spelling on every line moves", func(t *testing.T) {
		repo := oiRepo(t, "* @org/original\n/top.md @ORG/TEAM\n/x/ @org/team\n")
		code, _, errOut := oiSync(t, repo, "--op", "rename_owner(@Org/Team, @org/new-team)")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		oiWantFile(t, repo, "* @org/original\n/top.md @org/new-team\n/x/ @org/new-team\n")
		own := oiOwnership(t, repo)
		for _, p := range []string{"top.md", "x/a.go"} {
			oiWantNoSpellingOwns(t, own, p, "@org/team")
		}
	})

	// pin: the new name is written verbatim, never folded. A fix that
	// lowercased handles on the way out would silently rewrite a whole
	// fleet's CODEOWNERS while this suite's other tests stayed green.
	t.Run("pin: the new name is written exactly as spelled", func(t *testing.T) {
		repo := oiRepo(t, "* @org/original\n/x/ @org/team\n")
		code, _, errOut := oiSync(t, repo, "--op", "rename_owner(@org/team, @Org/New-Team)")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		oiWantFile(t, repo, "* @org/original\n/x/ @Org/New-Team\n")
		oiWantOwners(t, oiOwnership(t, repo), "x/a.go", []string{"@Org/New-Team"})
	})
}

// SPEC R-38a: `set_owners` states an EXACT set, and R-38a makes ops.FoldOwner
// the identity for every comparison of two owners, set_owners named among
// them. So `set_owners(/x/, [@ORG/TEAM])` against `/x/ @org/team` states a set
// that is ALREADY satisfied, and the right answer is to do nothing: report
// `unchanged` at exit 0, leave the bytes alone, and let `plan` exit 1.
//
// The alternative — treat the set as satisfied but rewrite the line to the
// op's spelling — is rejected for three reasons, none of them aesthetic.
// (1) R-38b settles the identical question for `add` and answers "the file's
// existing spelling wins", giving the reason: restyling churns a diff on
// every repository in a fleet. Nothing distinguishes set_owners here; the
// owner set before and after is the same set. (2) R-38d says a rename is
// "the ONE verb whose purpose is to change the text, so it is also the one
// place a spelling change is intended" — which excludes set_owners by name.
// (3) `synthSet` already short-circuits when every in-scope path resolves to
// exactly the requested set, on the stated ground that an intent-level tool
// must not edit just because no byte-identical rule exists; R-38a only makes
// that comparison ask the right question. A restyling set_owners would also
// make `unchanged` a lie: nothing about ownership changed.
func TestR38a_SetOwnersIsSatisfiedUnderTheIdentity(t *testing.T) {
	t.Run("an exact set already satisfied is a no-op", func(t *testing.T) {
		seed := "* @org/original\n/x/ @org/team\n"
		repo := oiRepo(t, seed)
		if got := oiPlanExit(t, repo, "set_owners(/x/, [@ORG/TEAM])"); got != cli.ExitNoOp {
			t.Errorf("plan exit = %d, want %d — the stated set is the set the file already has", got, cli.ExitNoOp)
		}
		code, rec, errOut := oiSync(t, repo, "--op", "set_owners(/x/, [@ORG/TEAM])")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if rec.Status != cli.StatusUnchanged {
			t.Errorf("status = %q, want %q", rec.Status, cli.StatusUnchanged)
		}
		oiWantFile(t, repo, seed)
		oiWantOwners(t, oiOwnership(t, repo), "x/a.go", []string{"@org/team"})
	})

	// Derived, not quoted: R-38a does not spell out the partially satisfied
	// case, but the same two reasons decide it. The op genuinely changes the
	// owner set, so it applies; the owner that was already there was not the
	// part being changed, so its spelling is not restyled either.
	t.Run("a set that adds an owner keeps the existing spelling", func(t *testing.T) {
		repo := oiRepo(t, "* @org/original\n/x/ @org/team\n")
		code, rec, errOut := oiSync(t, repo, "--op", "set_owners(/x/, [@Org/Team, @org/new])")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if rec.Status != cli.StatusApplied {
			t.Errorf("status = %q, want %q", rec.Status, cli.StatusApplied)
		}
		oiWantFile(t, repo, "* @org/original\n/x/ @org/team @org/new\n")
		oiWantOwners(t, oiOwnership(t, repo), "x/a.go", []string{"@org/team", "@org/new"})
	})

	// pin: the dropping half of set_owners already answers to the identity,
	// because set_owners replaces the whole list rather than matching owners
	// one by one. It is the premise the two subtests above rest on — R-38a is
	// only about the COMPARISON — and a fix that reworks set_owners around
	// per-owner matching must not lose it.
	t.Run("pin: an owner the set omits loses the path in every spelling", func(t *testing.T) {
		repo := oiRepo(t, "* @org/original\n/x/ @Org/Team @org/keep\n")
		code, _, errOut := oiSync(t, repo, "--op", "set_owners(/x/, [@org/keep])")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		oiWantFile(t, repo, "* @org/original\n/x/ @org/keep\n")
		oiWantNoSpellingOwns(t, oiOwnership(t, repo), "x/a.go", "@org/team")
	})
}

// SPEC R-38e: idempotence holds ACROSS spellings. Each row runs the verb once
// with one casing and again with another; the second run must be `unchanged`
// at exit 0 with a byte-identical file. This is the property that makes a
// fleet re-run safe, and the one the old behaviour broke most quietly — today
// the add row appends a second spelling on every re-run, so a weekly job
// grows the line without bound.
func TestR38e_IdempotenceHoldsAcrossSpellings(t *testing.T) {
	cases := map[string]struct {
		seed, first, want, second string
		onEmpty                   string
	}{
		"add_owner": {
			seed:   "* @org/original\n/x/ @org/keep\n",
			first:  "add_owner(/x/, @Org/Team)",
			want:   "* @org/original\n/x/ @org/keep @Org/Team\n",
			second: "add_owner(/x/, @ORG/TEAM)",
		},
		"remove_owner": {
			seed:   "* @org/original\n/x/ @org/team @org/keep\n",
			first:  "remove_owner(/x/, @ORG/TEAM)",
			want:   "* @org/original\n/x/ @org/keep\n",
			second: "remove_owner(/x/, @Org/Team)",
		},
		"set_owners": {
			seed:   "* @org/original\n/x/ @org/team\n",
			first:  "set_owners(/x/, [@ORG/TEAM])",
			want:   "* @org/original\n/x/ @org/team\n",
			second: "set_owners(/x/, [@Org/Team])",
		},
		"rename_owner": {
			seed:   "* @org/original\n/x/ @org/team\n",
			first:  "rename_owner(@ORG/TEAM, @org/new-team)",
			want:   "* @org/original\n/x/ @org/new-team\n",
			second: "rename_owner(@Org/Team, @org/new-team)",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := oiRepo(t, tc.seed)
			args := []string{"--op", tc.first}
			if tc.onEmpty != "" {
				args = append([]string{"--on-empty", tc.onEmpty}, args...)
			}
			code, _, errOut := oiSync(t, repo, args...)
			if code != cli.ExitOK {
				t.Fatalf("first run: want exit 0, got %d\nstderr:\n%s", code, errOut)
			}
			oiWantFile(t, repo, tc.want)

			args = []string{"--op", tc.second}
			if tc.onEmpty != "" {
				args = append([]string{"--on-empty", tc.onEmpty}, args...)
			}
			code, rec, errOut := oiSync(t, repo, args...)
			if code != cli.ExitOK {
				t.Fatalf("second run: want exit 0, got %d\nstderr:\n%s", code, errOut)
			}
			if rec.Status != cli.StatusUnchanged {
				t.Errorf("second run status = %q, want %q", rec.Status, cli.StatusUnchanged)
			}
			oiWantFile(t, repo, tc.want)
		})
	}
}

// SPEC R-38a: commutation is a comparison of two owners, so it answers to the
// same identity. `ops.commutes` reaches it through `containsOwner`, which
// compares bytes, so today an add and a remove of ONE owner spelled two ways
// look disjoint and the batch is waved through as order-independent — the
// exact case R-8 exists to refuse, since one order leaves the owner on the
// line and the other does not. The refusal belongs at exit 3 on `check`:
// "these two tokens are one owner" is a fact about the policy, independent of
// which repository it runs against.
func TestR38a_CommutationSeesOneOwner(t *testing.T) {
	t.Run("add and remove of one owner in two spellings do not commute", func(t *testing.T) {
		src := `{"version":1,"on_empty":"inherit","ops":[
			{"id":"grant","op":"add_owner(/x/, @Org/Team)"},
			{"id":"revoke","op":"remove_owner(/x/, @org/team)"}]}`
		pol := syncWritePolicy(t, src)
		code, _, errOut := runCLI(t, "check", "--policy", pol)
		if code != cli.ExitInvalid {
			t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
		}
		oiWantFragment(t, "check stderr", errOut, "do not commute")

		// sync must reach the same verdict at the same exit code. Today it
		// refuses this batch at exit 2 from the tree-based backstop instead:
		// the same batch, classed as "this repo needs a human" rather than
		// "the policy is broken", which is the distinction fleet scripts
		// branch on (CONTRIBUTING.md, "Exit codes are a contract").
		repo := oiRepo(t, "* @org/original\n/x/ @org/team\n")
		code, _, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
		if code != cli.ExitInvalid {
			t.Fatalf("sync: want exit 3, got %d\nstderr:\n%s", code, errOut)
		}
		oiWantFragment(t, "sync stderr", errOut, "do not commute")
	})

	// pin: the same pair spelled identically already refuses. It is the
	// premise the case above rests on — without it, an exit 3 there could
	// come from anywhere — so it runs as its own subtest and reports even
	// when its sibling fails.
	t.Run("pin: the same pair in one spelling already refuses", func(t *testing.T) {
		pol := syncWritePolicy(t, `{"version":1,"on_empty":"inherit","ops":[
			{"id":"grant","op":"add_owner(/x/, @org/team)"},
			{"id":"revoke","op":"remove_owner(/x/, @org/team)"}]}`)
		code, _, errOut := runCLI(t, "check", "--policy", pol)
		if code != cli.ExitInvalid {
			t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
		}
		oiWantFragment(t, "check stderr", errOut, "do not commute")
	})

	// The identity cuts the other way too, and this half is a false REFUSAL
	// rather than a false acceptance: set(L) ∘ add(A) commutes iff A ⊆ L, and
	// {@org/team} is a subset of {@Org/Team} because they are one owner. A
	// batch that is provably order-independent is refused at exit 3 today,
	// which is the class CONTRIBUTING.md reserves for a broken policy.
	//
	// Both orders are then run for real, which is the literal meaning of
	// "these commute" and reaches R-38a's *resolution* clause: plan.Build
	// compares the two orders' resolved owner sets, so an implementation that
	// folds `ops.commutes` but leaves resolution comparing bytes would pass
	// `check` at exit 0 and then refuse the same batch per repository at exit
	// 2 — the repo-0-defect-becomes-N-repository-defect failure R-36e argues
	// against. Which spelling ends up on the line is deliberately NOT
	// asserted: it differs by order and R-38 does not specify it.
	t.Run("set_owners then add of the same owner in two spellings commutes", func(t *testing.T) {
		orders := map[string]string{
			"set first": `{"version":1,"ops":[
				{"id":"set","op":"set_owners(/x/, [@Org/Team])"},
				{"id":"grant","op":"add_owner(/x/, @org/team)"}]}`,
			"add first": `{"version":1,"ops":[
				{"id":"grant","op":"add_owner(/x/, @org/team)"},
				{"id":"set","op":"set_owners(/x/, [@Org/Team])"}]}`,
		}
		for name, src := range orders {
			t.Run(name, func(t *testing.T) {
				pol := syncWritePolicy(t, src)
				code, _, errOut := runCLI(t, "check", "--policy", pol)
				if code != cli.ExitOK {
					t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
				}
				repo := oiRepo(t, "* @org/original\n/x/ @org/keep\n")
				code, _, errOut = oiSync(t, repo, "--policy", pol)
				if code != cli.ExitOK {
					t.Fatalf("sync: want exit 0, got %d\nstderr:\n%s", code, errOut)
				}
				own := oiOwnership(t, repo)
				for _, p := range []string{"x/a.go", "x/b.go"} {
					oiWantFoldedOwners(t, own, p, []string{"@org/team"})
				}
			})
		}
	})

	// pin: the same pair spelled identically is already accepted and already
	// runs to the same ownership in either order — the premise for the
	// subtest above, asserted the same way so the two are comparable.
	t.Run("pin: the same pair in one spelling is already accepted", func(t *testing.T) {
		orders := map[string]string{
			"set first": `{"version":1,"ops":[
				{"id":"set","op":"set_owners(/x/, [@org/team])"},
				{"id":"grant","op":"add_owner(/x/, @org/team)"}]}`,
			"add first": `{"version":1,"ops":[
				{"id":"grant","op":"add_owner(/x/, @org/team)"},
				{"id":"set","op":"set_owners(/x/, [@org/team])"}]}`,
		}
		for name, src := range orders {
			t.Run(name, func(t *testing.T) {
				pol := syncWritePolicy(t, src)
				code, _, errOut := runCLI(t, "check", "--policy", pol)
				if code != cli.ExitOK {
					t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
				}
				repo := oiRepo(t, "* @org/original\n/x/ @org/keep\n")
				code, _, errOut = oiSync(t, repo, "--policy", pol)
				if code != cli.ExitOK {
					t.Fatalf("sync: want exit 0, got %d\nstderr:\n%s", code, errOut)
				}
				oiWantFile(t, repo, "* @org/original\n/x/ @org/team\n")
			})
		}
	})
}

// SPEC R-38f (pin): a hand-written line naming one owner twice is that owner
// for every comparison, but `sync` does not rewrite the line to collapse it.
// Repair is `lint`'s verb; a sync that quietly edits what the policy did not
// name is the surprise this tool exists to avoid. Both subtests pass today
// and are here so a fix that folds owners on the way OUT — deduplicating the
// line while it is open — turns them red.
func TestR38f_SyncDoesNotCollapsePreExistingDuplicates(t *testing.T) {
	t.Run("pin: adding a different owner leaves both spellings", func(t *testing.T) {
		repo := oiRepo(t, "* @org/original\n/x/ @org/team @Org/Team\n")
		code, _, errOut := oiSync(t, repo, "--op", "add_owner(/x/, @org/other)")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		oiWantFile(t, repo, "* @org/original\n/x/ @org/team @Org/Team @org/other\n")
	})

	t.Run("pin: an op on another scope leaves the duplicated line alone", func(t *testing.T) {
		repo := oiRepo(t, "* @org/original\n/top.md @org/keep\n/x/ @org/team @Org/Team\n")
		code, _, errOut := oiSync(t, repo, "--op", "add_owner(/top.md, @org/other)")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		oiWantFile(t, repo, "* @org/original\n/top.md @org/keep @org/other\n/x/ @org/team @Org/Team\n")
	})
}

// SPEC R-38a (pin): ops.FoldOwner folds @handles ONLY. An e-mail owner is an
// address whose local part is not ours to case-fold, so dev@example.com and
// DEV@EXAMPLE.COM are two owners and must stay two. These pin the boundary a
// fix could most easily overrun — strings.ToLower on every token — in the
// direction where the damage is silent: an over-folding remove would revoke
// an address the policy never named.
func TestR38a_EmailOwnersStayCaseSensitive(t *testing.T) {
	t.Run("pin: adding the upper-cased address adds a second owner", func(t *testing.T) {
		repo := oiRepo(t, "* @org/original\n/x/ dev@example.com\n")
		code, rec, errOut := oiSync(t, repo, "--op", "add_owner(/x/, DEV@EXAMPLE.COM)")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if rec.Status != cli.StatusApplied {
			t.Errorf("status = %q, want %q", rec.Status, cli.StatusApplied)
		}
		oiWantFile(t, repo, "* @org/original\n/x/ dev@example.com DEV@EXAMPLE.COM\n")
		oiWantOwners(t, oiOwnership(t, repo), "x/a.go", []string{"dev@example.com", "DEV@EXAMPLE.COM"})
	})

	// The resolution assertion is the whole point: exit 0 and "unchanged"
	// are what an over-folding removal that DID revoke the address would
	// never produce, but they are also what any unrelated no-op produces, so
	// only "this address still owns the path" states the claim.
	t.Run("pin: removing the upper-cased address leaves the lower-cased one owning", func(t *testing.T) {
		seed := "* @org/original\n/x/ dev@example.com @org/keep\n"
		repo := oiRepo(t, seed)
		code, rec, errOut := oiSync(t, repo, "--op", "remove_owner(/x/, DEV@EXAMPLE.COM)")
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if rec.Status != cli.StatusUnchanged {
			t.Errorf("status = %q, want %q", rec.Status, cli.StatusUnchanged)
		}
		oiWantFile(t, repo, seed)
		oiWantOwners(t, oiOwnership(t, repo), "x/a.go", []string{"dev@example.com", "@org/keep"})
	})

	// R-33c's duplicate check asks the same identity, so it must NOT fire on
	// two addresses that differ only in case.
	t.Run("pin: two spellings of one address are not a duplicate list", func(t *testing.T) {
		pol := syncWritePolicy(t, `{"version":1,"ops":[
			{"id":"set","op":"set_owners(/x/, [dev@example.com, DEV@EXAMPLE.COM])"}]}`)
		code, _, errOut := runCLI(t, "check", "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
	})

	// pin: the handle case still IS a duplicate — the other side of the same
	// boundary, so a fix cannot satisfy the pin above by folding nothing.
	t.Run("pin: two spellings of one handle are a duplicate list", func(t *testing.T) {
		pol := syncWritePolicy(t, `{"version":1,"ops":[
			{"id":"set","op":"set_owners(/x/, [@Org/Team, @org/team])"}]}`)
		code, _, errOut := runCLI(t, "check", "--policy", pol)
		if code != cli.ExitInvalid {
			t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
		}
		oiWantFragment(t, "check stderr", errOut, "are one owner in the same")
	})
}

// SPEC R-38a: `on_zero_match: declare` writes a rule for a scope no tracked
// file matches, and it too must ask whether the owner is ALREADY declared
// rather than whether the token is already spelled that way. Today a
// differently cased handle appends itself beside the one already there, so a
// declare-based baseline rewrites the same line on every repository that
// capitalised it, once per run.
func TestR38a_DeclareDeclaresOneOwner(t *testing.T) {
	t.Run("an already declared owner is not declared again", func(t *testing.T) {
		seed := "* @org/original\n/ghost/ @org/team\n"
		repo := oiRepo(t, seed)
		pol := syncWritePolicy(t, `{"version":1,"ops":[
			{"id":"declare","op":"add_owner(/ghost/, @Org/Team)","on_zero_match":"declare"}]}`)
		code, rec, errOut := oiSync(t, repo, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if rec.Status != cli.StatusUnchanged {
			t.Errorf("status = %q, want %q", rec.Status, cli.StatusUnchanged)
		}
		oiWantFile(t, repo, seed)
	})

	// pin: the same op spelled as the file spells it is already a no-op —
	// the premise the subtest above rests on.
	t.Run("pin: the same op in one spelling is already a no-op", func(t *testing.T) {
		seed := "* @org/original\n/ghost/ @org/team\n"
		repo := oiRepo(t, seed)
		pol := syncWritePolicy(t, `{"version":1,"ops":[
			{"id":"declare","op":"add_owner(/ghost/, @org/team)","on_zero_match":"declare"}]}`)
		code, rec, errOut := oiSync(t, repo, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		if rec.Status != cli.StatusUnchanged {
			t.Errorf("status = %q, want %q", rec.Status, cli.StatusUnchanged)
		}
		oiWantFile(t, repo, seed)
	})
}

// SPEC R-38b with R-26: an except-carrying grant whose grantee already owns
// every NON-excepted in-scope path is a no-op too, and the carve line is part
// of what must not be written. Today the op applies, appends a second
// spelling to the broad rule AND inserts a carve restating the excepted
// path's owners — two lines of diff, in every repository in the fleet, for a
// grant that changes nobody's access.
func TestR38b_ExceptGrantIsANoOpWhenTheGranteeAlreadyOwns(t *testing.T) {
	seed := "* @org/original\n/.github/ @org/original @org/team-a\n"
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":       seed,
		".github/workflows/ci.yml": "name: ci\n",
		".github/dependabot.yml":   "version: 2\n",
		"src/main.go":              "package main\n",
	})
	code, rec, errOut := oiSync(t, repo,
		"--op", "add_owner(/.github/ except /.github/CODEOWNERS, @Org/Team-A)")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	if rec.Status != cli.StatusUnchanged {
		t.Errorf("status = %q, want %q", rec.Status, cli.StatusUnchanged)
	}
	if got := syncReadFile(t, filepath.Join(repo, ".github/CODEOWNERS")); got != seed {
		t.Errorf(".github/CODEOWNERS bytes:\n%q\nwant:\n%q", got, seed)
	}
	own := oiOwnership(t, repo)
	oiWantOwners(t, own, ".github/workflows/ci.yml", []string{"@org/original", "@org/team-a"})
	oiWantOwners(t, own, ".github/CODEOWNERS", []string{"@org/original", "@org/team-a"})
}
