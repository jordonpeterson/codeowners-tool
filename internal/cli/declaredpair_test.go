// R-22b end-to-end tests: which pairs the declare-op order-dependence guard
// (R-8 for zero-match ops) is entitled to refuse.
//
// Written ahead of the implementation per CONTRIBUTING.md. The four negative
// cases below pass against today's binary and are labeled as pins — they
// freeze refusals R-22b must preserve, so the change cannot be "fixed" by
// deleting the guard. The positive cases fail today with the R-8 refusal
// quoted in TestR22b_ExactTrackedFileAgainstADeclareApplies.
package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// dpRepo is the motivating fixture, reduced from a real fleet policy: a
// directory rule that hands @org/bot everything under .github/, including the
// CODEOWNERS file itself, and NO justfile anywhere — so an op scoped to
// **/justfile matches zero tracked files and must be declared.
func dpRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		".github/CODEOWNERS":       "* @org/everyone\n.github @org/everyone @org/bot\n",
		".github/workflows/ci.yml": "name: ci\n",
		"src/main.go":              "package main\n",
	})
}

// dpDeclaredPairFragment is unique to the declare-op arm of R-8. Asserting
// only "R-8" would let the pins below pass on the tree-based arm, or on the
// static conflict check in internal/ops, neither of which they are guarding.
const dpDeclaredPairFragment = "can both govern a path that does not exist yet"

// dpWantFile asserts exact bytes. Under last-match-wins (S-1) a substring
// check is satisfied by a file whose line ORDER hands the path back to the
// owner the op just removed.
func dpWantFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("CODEOWNERS =\n%q\nwant\n%q", got, want)
	}
}

// dpSync runs one policy against a fresh fixture and returns the exit code,
// stderr, and the repo path.
func dpSync(t *testing.T, src string) (int, string, string) {
	t.Helper()
	repo := dpRepo(t)
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", syncWritePolicy(t, src))
	return code, errOut, repo
}

// SPEC R-22b: an op whose scope is an anchored, wildcard-free path naming one
// TRACKED FILE commutes with a declared op, whatever owners the two name.
//
// The declared op matched zero tracked paths, so by construction it does not
// match that file; and no path can appear beneath a file. The two scopes can
// never meet, so there is no order to depend on.
//
// The guard refuses the pair today. Its own comment (plan.go:376) justifies
// itself with a hazard that needs BOTH ops declared — two rules stacked at
// EOF where last-match-wins silently picks the winner — but the condition it
// fires on is `declared[i] || declared[j]`, so it also catches a declare
// paired with an op that resolves against the real tree. `remove_owner`
// cannot even carry `declare` (plan.go:232), so the pair below is
// unreachable by the hazard the guard was written for:
//
//	error: ops "remove_owner(/.github/CODEOWNERS, @org/bot)" and
//	"add_owner(**/justfile, @org/bot)" can both govern a path that does not
//	exist yet and do not commute (R-8: refusing order-dependent batch)
//
// This is the shape every real "grant the bot everything except CODEOWNERS"
// fleet policy takes, and refusing it forces the operator to split one
// reviewed artifact into two invocations that must both be rolled out.
func TestR22b_ExactTrackedFileAgainstADeclareApplies(t *testing.T) {
	want := "* @org/everyone\n" +
		".github @org/everyone @org/bot\n" +
		"/.github/CODEOWNERS @org/everyone\n" +
		"**/justfile @org/bot\n"

	// remove_owner is the verb that cannot be declared at all, so it is the
	// clearest case: the guard's stated hazard is structurally unreachable.
	t.Run("remove_owner", func(t *testing.T) {
		code, errOut, repo := dpSync(t, `{"version":1,"on_empty":"error","ops":[
			{"op":"remove_owner(/.github/CODEOWNERS, @org/bot)","on_zero_match":"skip"},
			{"op":"add_owner(**/justfile, @org/bot)","on_zero_match":"declare"}]}`)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		dpWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"), want)
	})

	// set_owners CAN be declared, so this case proves the exemption turns on
	// whether the op resolved against the tree, not on the verb.
	t.Run("set_owners", func(t *testing.T) {
		code, errOut, repo := dpSync(t, `{"version":1,"ops":[
			{"op":"set_owners(/.github/CODEOWNERS, [@org/everyone])","on_zero_match":"skip"},
			{"op":"add_owner(**/justfile, @org/bot)","on_zero_match":"declare"}]}`)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		dpWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"), want)
	})
}

// SPEC R-22b: the accepted batch is order-independent in the strong sense —
// the two orderings produce the SAME BYTES, not merely the same resolution.
//
// That is what makes the exemption safe for a path that does not exist yet: a
// declared rule always appends at EOF and a narrowing rule is always inserted
// after the rule it corrects, so even if .github/CODEOWNERS were replaced by a
// directory containing a justfile, both orderings agree on which line wins.
// An assertion on resolution alone would not have caught a file that differs.
func TestR22b_ExactTrackedFilePairIsByteIdenticalInBothOrders(t *testing.T) {
	remove := `{"op":"remove_owner(/.github/CODEOWNERS, @org/bot)","on_zero_match":"skip"}`
	declare := `{"op":"add_owner(**/justfile, @org/bot)","on_zero_match":"declare"}`

	read := func(order string) string {
		t.Helper()
		code, errOut, repo := dpSync(t, `{"version":1,"on_empty":"error","ops":[`+order+`]}`)
		if code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		got, err := os.ReadFile(filepath.Join(repo, ".github/CODEOWNERS"))
		if err != nil {
			t.Fatal(err)
		}
		return string(got)
	}
	if a, b := read(remove+","+declare), read(declare+","+remove); a != b {
		t.Errorf("op order changed the file:\nremove first:\n%q\ndeclare first:\n%q", a, b)
	}
}

// SPEC R-22b (the boundary that must keep refusing): a DIRECTORY scope is not
// an exact tracked file, however anchored and wildcard-free it is spelled.
//
// `/.github/` names a directory that can gain files, so a future
// .github/main.tf matches both that scope and the declared `**/*.tf`. Which
// one governs it depends on which line was written where, so the batch really
// is order-dependent and R-8 must still refuse it. This is the case that
// broke when the exemption was first written as "the other op resolved
// against a non-empty tree" — a condition a directory satisfies too.
//
// Pin: passes today, and must keep passing.
func TestR22b_DirectoryScopeAgainstADeclareStaysRefused(t *testing.T) {
	cases := map[string]string{
		"remove_owner over a directory": `{"version":1,"on_empty":"error","ops":[
			{"op":"remove_owner(/.github/, @org/bot)","on_zero_match":"skip"},
			{"op":"add_owner(**/*.tf, @org/bot)","on_zero_match":"declare"}]}`,
		"set_owners over a directory": `{"version":1,"ops":[
			{"op":"set_owners(/src/, [@org/team])","on_zero_match":"skip"},
			{"op":"set_owners(**/*.tf, [@org/infra])","on_zero_match":"declare"}]}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			code, errOut, _ := dpSync(t, src)
			if code != cli.ExitRefused {
				t.Fatalf("an order-dependent batch must be refused (sync exit 2), got %d\nstderr:\n%s", code, errOut)
			}
			if !strings.Contains(errOut, dpDeclaredPairFragment) {
				t.Errorf("refusal must come from the declare-op guard (R-8), got %q", errOut)
			}
		})
	}
}

// SPEC R-22b (the boundary that must keep refusing): two DECLARED ops are the
// hazard the guard was written for, and the exemption must not reach them.
//
// Neither op resolves against the tree, both rules land at EOF, and
// last-match-wins hands a future terraform/main.tf to whichever was listed
// second — so the two orderings produce different files. Refusing here is the
// whole point of the guard.
//
// Pin: passes today, and must keep passing.
func TestR22b_TwoDeclaresStayRefused(t *testing.T) {
	code, errOut, _ := dpSync(t, `{"version":1,"ops":[
		{"op":"add_owner(/terraform/, @org/infra)","on_zero_match":"declare"},
		{"op":"set_owners(/terraform/*.tf, [@org/tf])","on_zero_match":"declare"}]}`)
	if code != cli.ExitRefused {
		t.Fatalf("two contradictory declares must be refused (sync exit 2), got %d\nstderr:\n%s", code, errOut)
	}
	if !strings.Contains(errOut, dpDeclaredPairFragment) {
		t.Errorf("refusal must come from the declare-op guard (R-8), got %q", errOut)
	}
}

// SPEC R-22b: `check` reads no repository, so it cannot reach any of this.
//
// Whether an op is declared is a fact about one repo's tree, so both the
// accepted and the refused policies above are valid policies. This is why the
// refusal surfaces at sync, per repo, as exit 2 rather than halting a rollout
// at exit 3 — and it is the reason an operator sees "check passed, sync
// failed" and reasonably concludes the policy file is fine.
func TestR22b_CheckCannotDecideDeclaredPairs(t *testing.T) {
	for name, src := range map[string]string{
		"accepted at sync": `{"version":1,"on_empty":"error","ops":[
			{"op":"remove_owner(/.github/CODEOWNERS, @org/bot)","on_zero_match":"skip"},
			{"op":"add_owner(**/justfile, @org/bot)","on_zero_match":"declare"}]}`,
		"refused at sync": `{"version":1,"on_empty":"error","ops":[
			{"op":"remove_owner(/.github/, @org/bot)","on_zero_match":"skip"},
			{"op":"add_owner(**/*.tf, @org/bot)","on_zero_match":"declare"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			code, _, errOut := runCLI(t, "check", "--policy", syncWritePolicy(t, src))
			if code != cli.ExitOK {
				t.Fatalf("check reads no repository and must accept both policies, got %d\nstderr:\n%s", code, errOut)
			}
		})
	}
}
