// Neighbours of the four op/pattern fixes: the cases each fix must NOT take
// with it, end to end through the CLI.
package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// SPEC: a scope that is dead HERE and alive elsewhere still declares. R-5's
// refusal is a fact about this repository, and `on_zero_match: declare` is the
// documented way to overrule it; only a scope no repository can ever satisfy is
// refused as a policy error.
func TestDeclareStillWritesADeliberatelyDeadScope(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "*  @org/everyone\n",
		"src/a.go":   "",
	})
	policy := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(policy, []byte(
		`{"version":1,"ops":[{"op":"add_owner(/.github/workflows/, @org/ci)","on_zero_match":"declare"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--policy", policy)
	if code != cli.ExitOK {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "structural") {
		t.Errorf("a declare still reports proven: structural\n%s", stdout)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "CODEOWNERS"))
	if !strings.Contains(string(b), "/.github/workflows/ @org/ci") {
		t.Errorf("the declared rule was not written:\n%s", b)
	}
}

// SPEC: the `**` spellings that are alive still work as scopes. `x/**/**` and
// `foo/**/` hold two adjacent `**` segments and match real paths; refusing them
// alongside the leading `**/**` family would take a live pattern with it.
func TestAdjacentDoubleStarScopesStillApply(t *testing.T) {
	for _, scope := range []string{"x/**/**", "**/x/**", "foo/**/", "**/*.tf"} {
		t.Run(scope, func(t *testing.T) {
			repo := initRepo(t, map[string]string{
				"CODEOWNERS":     "*  @org/everyone\n",
				"x/a/b.go":       "",
				"foo/deep/c.txt": "",
				"infra/main.tf":  "",
			})
			code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner("+scope+", @org/x)")
			if code != cli.ExitOK {
				t.Fatalf("scope %q: exit %d\n%s%s", scope, code, stdout, stderr)
			}
			b, _ := os.ReadFile(filepath.Join(repo, "CODEOWNERS"))
			if !strings.Contains(string(b), "@org/x") {
				t.Errorf("scope %q wrote nothing:\n%s", scope, b)
			}
		})
	}
}

// SPEC: the byte-equal duplicate an insert strands keeps R-7's disclosure and
// only that one. The narrower-rule disclosure is a second finding, not a
// replacement, and two warnings for one line would double-report it.
func TestSetOwnersByteEqualDuplicateKeepsTheR7Disclosure(t *testing.T) {
	// The pre-existing "/docs/" wins docs/b.md before the run and nothing
	// after it, so both disclosures are live candidates for the same line —
	// which is the only arrangement that can tell them apart.
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "*  @org/everyone\n/docs/  @org/old\n/docs/a.md  @org/other\n",
		"docs/a.md":  "",
		"docs/b.md":  "",
	})
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "set_owners(/docs/, [@org/new])")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "duplicate pattern \"/docs/\"") {
		t.Errorf("the R-7 duplicate disclosure is gone:\n%s", out)
	}
	if strings.Contains(out, "rule \"/docs/\" is now fully shadowed") {
		t.Errorf("the byte-equal line is reported twice, once per disclosure:\n%s", out)
	}
	// The rule that is NOT byte-equal is the one the new disclosure exists for.
	if !strings.Contains(out, "rule \"/docs/a.md\" is now fully shadowed") {
		t.Errorf("the narrower rule this run stranded went unreported:\n%s", out)
	}
}

// SPEC: an op that strands nothing says nothing. The stranded-rule disclosure
// must fire on the run that authors the dead line and on no other, or a fleet
// learns to ignore it.
func TestSetOwnersIsQuietWhenItStrandsNothing(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":  "*  @org/everyone\n/docs/x/  @org/x-team\n",
		"docs/x/f.md": "",
		"src/a.go":    "",
	})
	// The scope is disjoint from the pre-existing narrower rule, so nothing is
	// shadowed by the line this run writes.
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "set_owners(/src/, [@org/new])")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.Contains(out, "fully shadowed") {
		t.Errorf("nothing was stranded, and the run reported one:\n%s", out)
	}
}

// SPEC: the op-string grammar is unchanged where no escape is involved — a
// bare owner, a bracketed list, and the arity refusal for two bare owners.
func TestOrdinaryOpStringsAreUnchangedByTheCommaEscape(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "*  @org/everyone\n",
		"docs/a.md":  "",
	})
	if code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--dry-run", "--op", "add_owner(/docs/, [@org/a, @org/b])"); code != cli.ExitOK {
		t.Errorf("bracketed list: exit %d\n%s%s", code, stdout, stderr)
	}
	if code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--dry-run", "--op", "add_owner(/docs/, @org/a)"); code != cli.ExitOK {
		t.Errorf("bare owner: exit %d\n%s%s", code, stdout, stderr)
	}
	// Two bare owners is still an arity refusal: every argument is a valid
	// owner, so nothing suggests a comma landed inside a path.
	_, stdout, stderr := runCLI(t, "check", "--op", "add_owner(/docs/, @org/a, @org/b)")
	if out := stdout + stderr; !strings.Contains(out, "got 3 args") {
		t.Errorf("the arity refusal for two bare owners changed:\n%s", out)
	}
}

// SPEC: a path holding a comma is reachable from the policy file too, in both
// owner spellings, and the line it writes reads back as the rule that was
// planned.
func TestCommaScopeWorksFromAPolicyFile(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @org/everyone\n",
		"a,b/f.md":   "",
	})
	policy := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(policy, []byte(
		`{"version":1,"ops":[{"op":"add_owner(/a\\,b/)","owners":["@org/x"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runCLI(t, "check", "--policy", policy); code != cli.ExitOK {
		t.Fatalf("check: exit %d\n%s%s", code, stdout, stderr)
	}
	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--policy", policy)
	if code != cli.ExitOK {
		t.Fatalf("sync: exit %d\n%s%s", code, stdout, stderr)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "CODEOWNERS"))
	if !strings.Contains(string(b), `/a\,b/`) {
		t.Errorf("the written line does not carry the escaped comma:\n%s", b)
	}
	// Re-running is a no-op, which is the proof the line reads back as the
	// rule that was planned (R-19).
	if code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--policy", policy); code != cli.ExitOK {
		t.Errorf("second run: exit %d\n%s%s", code, stdout, stderr)
	}
}
