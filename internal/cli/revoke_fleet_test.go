// Revoking one owner from the CODEOWNERS file itself, across a fleet whose
// original teams the policy cannot name (R-6, R-31, R-39).
//
// The intent is one sentence — "@automated-approvers co-owns .github/, but
// must not be a required reviewer of .github/CODEOWNERS" — and it is two
// DISJOINT ops: a grant carrying an except, and a removal scoped to exactly
// the carved path. Disjoint ops commute, so R-8 admits both in one policy
// (R-31) and the fleet runs one reviewed artifact everywhere. The `except`
// alone cannot express the intent: it means don't-touch, so where the grantee
// already owns the carved path through a broader rule it stays there (R-26,
// and the record says so). Revocation is the removal's job.
//
// What the policy CANNOT do is name the surviving owners. Across a hundred
// repositories the team holding .github/ differs in each and is unknown to the
// policy's author, which rules out `set_owners` — the survivors have to be
// discovered per repo. Every expectation below therefore pins bytes the policy
// never mentions, and the shapes differ only in who owns .github/ beforehand,
// because that is the axis a real fleet varies on.
//
// One shape refuses on purpose. Where the removal would leave the carved path
// matching NO rule, no line can express the outcome (S-2: CODEOWNERS has no
// negation, and "unmatched" is not "owned by nobody" — S-9), so the repo is
// named for a human instead of being given an invented owner.
package cli_test

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// revokePolicySrc is the artifact the fleet runs unchanged. It names exactly
// one owner: the one being revoked.
const revokePolicySrc = `{
  "version": 1,
  "on_empty": "inherit",
  "ops": [
    { "id": "grant",  "op": "add_owner(/.github/ except /.github/CODEOWNERS, @automated-approvers)" },
    { "id": "revoke", "op": "remove_owner(/.github/CODEOWNERS, @automated-approvers)" }
  ]
}`

// revokeShape is one repository in the fleet: what its CODEOWNERS says before
// the run, and what the one policy must do to it.
type revokeShape struct {
	name string
	// before is the CODEOWNERS content; at is where the file lives, since a
	// repo that keeps it at the root is a shape the fleet meets (S-8).
	before, at string
	wantExit   int
	wantStatus string
	// wantFile is the exact content after the run. Empty means the run must
	// write nothing, and the fixture's `before` is asserted intact instead.
	wantFile string
	// wantFragment is the phrase a refusal must carry so an operator reading
	// 100 records can tell this repo's problem from every other repo's.
	wantFragment string
	// wantOwners is the resolved ownership after the run, for the paths the
	// intent is about. Bytes prove placement; resolution proves meaning.
	wantOwners map[string][]string
}

func revokeShapes() []revokeShape {
	return []revokeShape{
		{
			// The common shape: automation was granted .github/ alongside the
			// owning team and another bot. The removal narrows, restating the
			// survivors — @org/original and @codeowners-pipeline — which the
			// policy never names.
			name: "co-owned",
			at:   ".github/CODEOWNERS",
			before: "* @org/original\n" +
				"/.github/ @org/original @automated-approvers @codeowners-pipeline\n",
			wantExit:   cli.ExitOK,
			wantStatus: cli.StatusApplied,
			wantFile: "* @org/original\n" +
				"/.github/ @org/original @automated-approvers @codeowners-pipeline\n" +
				"/.github/CODEOWNERS @org/original @codeowners-pipeline\n",
			wantOwners: map[string][]string{
				".github/CODEOWNERS":       {"@codeowners-pipeline", "@org/original"},
				".github/workflows/ci.yml": {"@automated-approvers", "@codeowners-pipeline", "@org/original"},
				"README.md":                {"@org/original"},
			},
		},
		{
			// No .github/ rule at all: automation rides the catch-all. The
			// grant is already satisfied, and the removal has to narrow out of
			// a rule that governs the whole repository without disturbing one
			// path outside its scope (INV-2).
			name:       "catch-all only",
			at:         ".github/CODEOWNERS",
			before:     "* @org/original @automated-approvers\n",
			wantExit:   cli.ExitOK,
			wantStatus: cli.StatusApplied,
			wantFile: "* @org/original @automated-approvers\n" +
				"/.github/CODEOWNERS @org/original\n",
			wantOwners: map[string][]string{
				".github/CODEOWNERS":       {"@org/original"},
				".github/workflows/ci.yml": {"@automated-approvers", "@org/original"},
				"README.md":                {"@automated-approvers", "@org/original"},
			},
		},
		{
			// R-39, the shape this file exists for: automation is the SOLE
			// owner of /.github/. Removing it from the carved path empties the
			// narrowing line, and `inherit` cannot delete /.github/ — other
			// paths depend on it. Deletion is not the only spelling of
			// inheritance: one narrower line restating what the path would
			// fall through to (@org/original, from the catch-all) expresses
			// exactly the same resolution and touches nothing else.
			name: "automation is sole owner",
			at:   ".github/CODEOWNERS",
			before: "* @org/original\n" +
				"/.github/ @automated-approvers\n",
			wantExit:   cli.ExitOK,
			wantStatus: cli.StatusApplied,
			wantFile: "* @org/original\n" +
				"/.github/ @automated-approvers\n" +
				"/.github/CODEOWNERS @org/original\n",
			wantOwners: map[string][]string{
				".github/CODEOWNERS":       {"@org/original"},
				".github/workflows/ci.yml": {"@automated-approvers"},
				"README.md":                {"@org/original"},
			},
		},
		{
			// The boundary of R-39: nothing to fall through to. Deleting the
			// rule would leave .github/CODEOWNERS matching no rule, and a
			// written line cannot mean "unmatched" — `[]` is a deliberate
			// un-owning (S-9), a different state. Refuse and name the repo.
			name:         "nothing to inherit from",
			at:           ".github/CODEOWNERS",
			before:       "/.github/ @automated-approvers\n",
			wantExit:     cli.ExitRefused,
			wantStatus:   cli.StatusRefused,
			wantFragment: "would match no rule",
		},
		{
			// Already converged: the carve line is on disk. The fleet's second
			// wave must be exit 0, `unchanged`, and byte-identical — a rewrite
			// here is a hundred no-op pull requests.
			name: "already carved",
			at:   ".github/CODEOWNERS",
			before: "* @org/original\n" +
				"/.github/ @org/original @automated-approvers\n" +
				"/.github/CODEOWNERS @org/original\n",
			wantExit:   cli.ExitOK,
			wantStatus: cli.StatusUnchanged,
			wantOwners: map[string][]string{
				".github/CODEOWNERS":       {"@org/original"},
				".github/workflows/ci.yml": {"@automated-approvers", "@org/original"},
				"README.md":                {"@org/original"},
			},
		},
		{
			// Not normalized: the file is at the root, so /.github/CODEOWNERS
			// is not a tracked path and the carve-out this policy promises
			// does not exist here. R-28 refuses rather than writing a grant
			// with no carve — this repo needs moving first, and the fleet run
			// has to say which repo it is.
			name: "file at repo root",
			at:   "CODEOWNERS",
			before: "* @org/original\n" +
				"/.github/ @org/original @automated-approvers\n",
			wantExit:     cli.ExitRefused,
			wantStatus:   cli.StatusRefused,
			wantFragment: "matches zero tracked files",
		},
	}
}

// revokeRepo materializes one shape: the file where the shape puts it, plus
// content inside .github/ and outside it so INV-2 has something to protect on
// both sides of every scope in the policy.
func revokeRepo(t *testing.T, s revokeShape) string {
	t.Helper()
	return initRepo(t, map[string]string{
		s.at:                       s.before,
		".github/workflows/ci.yml": "name: ci\n",
		".github/dependabot.yml":   "version: 2\n",
		"README.md":                "# repo\n",
		"src/main.go":              "package main\n",
	})
}

// revokeCommitIfDirty commits what sync wrote so `snapshot` (which resolves
// against a ref) sees it. An unchanged repo has nothing to commit, and `git
// commit` exits nonzero there — the same trap docs/FLEET.md's script has to
// avoid, so the test harness may not pretend otherwise.
func revokeCommitIfDirty(t *testing.T, repo string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(out) == 0 {
		return
	}
	gitCommitAll(t, repo)
}

// SPEC R-6/R-31/R-39: one policy revokes an owner from the carved CODEOWNERS
// path across every shape a fleet contains, naming only the owner it revokes.
// The surviving owners are discovered per repository — including where the
// revoked owner was the SOLE owner of the rule, which is R-39: `inherit`
// narrows when it cannot delete, rather than refusing an intent the file can
// express.
func TestR39_RevokeAcrossFleetWithUnknownOriginalTeams(t *testing.T) {
	pol := syncWritePolicy(t, revokePolicySrc)
	for _, s := range revokeShapes() {
		t.Run(s.name, func(t *testing.T) {
			repo := revokeRepo(t, s)
			path := filepath.Join(repo, filepath.FromSlash(s.at))

			code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
			if code != s.wantExit {
				t.Fatalf("exit %d, want %d\nstderr:\n%s", code, s.wantExit, errOut)
			}
			rec := syncDecodeRecord(t, out)
			if rec.Status != s.wantStatus {
				t.Errorf("status %q, want %q", rec.Status, s.wantStatus)
			}
			if s.wantFragment != "" {
				exceptWantFragment(t, "record error", rec.Error, s.wantFragment)
			}
			want := s.wantFile
			if want == "" {
				want = s.before // a refusal or a no-op writes nothing
			}
			exceptWantFile(t, path, want)
			if s.wantOwners == nil {
				return
			}
			revokeCommitIfDirty(t, repo)
			own := exceptOwnership(t, repo)
			for p, w := range s.wantOwners {
				if !reflect.DeepEqual(own[p], w) {
					t.Errorf("%s resolves to %v, want %v", p, own[p], w)
				}
			}
			// The whole point of the wave: whatever the repo looked like
			// before, the automation account no longer reviews CODEOWNERS.
			for _, o := range own[".github/CODEOWNERS"] {
				if o == "@automated-approvers" {
					t.Errorf(".github/CODEOWNERS still resolves to @automated-approvers")
				}
			}
		})
	}
}

// SPEC R-19/R-39: the second wave over the same fleet changes nothing. A
// narrowing insert that restates inherited owners must be recognized as
// already-satisfied on the re-run, or every repo in the fleet grows one line
// per wave.
func TestR39_RevokeWaveIsIdempotent(t *testing.T) {
	pol := syncWritePolicy(t, revokePolicySrc)
	for _, s := range revokeShapes() {
		if s.wantExit != cli.ExitOK {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			repo := revokeRepo(t, s)
			path := filepath.Join(repo, filepath.FromSlash(s.at))
			if code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol); code != cli.ExitOK {
				t.Fatalf("first wave: exit %d\n%s", code, errOut)
			}
			revokeCommitIfDirty(t, repo)
			first := syncReadFile(t, path)

			code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
			if code != cli.ExitOK {
				t.Fatalf("second wave: exit %d\n%s", code, errOut)
			}
			if rec := syncDecodeRecord(t, out); rec.Status != cli.StatusUnchanged {
				t.Errorf("second wave status %q, want unchanged (record: %s)", rec.Status, out)
			}
			if got := syncReadFile(t, path); got != first {
				t.Errorf("second wave rewrote the file:\n%s\nwant:\n%s", got, first)
			}
		})
	}
}

// SPEC R-39: the inserted line explains itself. A reviewer of the pull request
// meets an owner the policy never named, on a line that did not exist before,
// so the change must say where those owners came from — otherwise the only
// honest review of a 100-repo wave is to re-derive each file by hand.
func TestR39_InheritedCarveLineExplainsItself(t *testing.T) {
	var sole revokeShape
	for _, s := range revokeShapes() {
		if s.name == "automation is sole owner" {
			sole = s
		}
	}
	repo := revokeRepo(t, sole)
	pol := syncWritePolicy(t, revokePolicySrc)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr:\n%s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	var found bool
	for _, c := range rec.Changes {
		if c.Action != "insert" || c.Pattern != "/.github/CODEOWNERS" {
			continue
		}
		found = true
		if !reflect.DeepEqual(c.NewOwners, []string{"@org/original"}) {
			t.Errorf("inserted owners %v, want [@org/original]", c.NewOwners)
		}
		for _, want := range []string{"inherit", "@org/original"} {
			if !strings.Contains(c.Reason, want) {
				t.Errorf("change reason %q must name %q — the owners are restated from another rule, not chosen", c.Reason, want)
			}
		}
	}
	if !found {
		t.Errorf("no insert change for /.github/CODEOWNERS; changes: %+v", rec.Changes)
	}
}

// SPEC R-39/INV-2: narrowing under `inherit` restates the owners of the rule
// the path falls through to — it must never edit or delete that rule, whose
// other paths are out of scope. The catch-all here owns the rest of the
// repository and must come through the run byte-identical.
func TestR39_FallthroughRuleIsNotTouched(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":       "* @org/original @org/second\n/.github/ @automated-approvers\n",
		".github/workflows/ci.yml": "name: ci\n",
		"src/main.go":              "package main\n",
	})
	pol := syncWritePolicy(t, revokePolicySrc)
	if code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol); code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, errOut)
	}
	exceptWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"),
		"* @org/original @org/second\n"+
			"/.github/ @automated-approvers\n"+
			"/.github/CODEOWNERS @org/original @org/second\n")
	gitCommitAll(t, repo)
	own := exceptOwnership(t, repo)
	if want := []string{"@org/original", "@org/second"}; !reflect.DeepEqual(own["src/main.go"], want) {
		t.Errorf("out-of-scope src/main.go = %v, want %v (INV-2)", own["src/main.go"], want)
	}
}
