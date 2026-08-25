// Owner-identity tests for co-own.sh: owner comparisons follow the tool's
// R-38 rule — @handles are the same owner regardless of case, emails are
// byte-exact — and snapshot preserves file spellings, so the script must fold
// its own comparisons rather than trust byte equality.
package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A dedicated line spelled @Org/Platform is the same owner as
// --owner @org/platform. Byte comparison called this "no-owner" (skipped) or
// missed the dedicated line entirely; the fold must land it in the ordinary
// shared lane, deleting the line.
func TestCoOwn_CaseVariantHandleOnDedicatedLineIsRecognized(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		"* @org/team_a\n.github @org/team_a @Org/Platform\n.github/workflows @Org/Platform\n",
		map[string]string{".github/workflows/ci.yml": "x", ".github/dependabot.yml": "x", "src/main.go": "x"})

	res, code := runScriptScoped(t, repo, ".github/workflows", "@org/platform")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (result: %+v)", code, res)
	}
	if res.Status != "shared" {
		t.Fatalf("status %q, want \"shared\" — a case-variant handle is the same owner (R-38)", res.Status)
	}
	git(t, repo, "checkout", "-q", res.Branch)
	// Snapshot preserves the file's spelling, so the surviving broader rule's
	// @Org/Platform is what resolution reports.
	if got := owners(t, repo, ".github/workflows/ci.yml"); !sameOwners(got, "@org/team_a", "@Org/Platform") {
		t.Fatalf("workflows owners after: %v, want exactly {@org/team_a, @Org/Platform}", got)
	}
	body, err := os.ReadFile(filepath.Join(repo, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), ".github/workflows") {
		t.Fatalf("the dedicated line survived — the awk match did not fold the handle:\n%s", body)
	}
}

// An email owner is NOT folded (R-38: the local part's case is not ours to
// change). A case-variant email is a different owner — skipped, untouched —
// while a byte-identical one still flows through the shared lane.
func TestCoOwn_EmailOwnerStaysByteExact(t *testing.T) {
	needBinaries(t)

	t.Run("case-variant email is a different owner", func(t *testing.T) {
		repo := mkRepo(t,
			"* @org/team_a\n.github/workflows Ops@Example.com\n",
			map[string]string{".github/workflows/ci.yml": "x", "src/main.go": "x"})
		base := git(t, repo, "rev-parse", "HEAD")

		res, code := runScriptScoped(t, repo, ".github/workflows", "ops@example.com")
		if code != 0 {
			t.Fatalf("exit %d, want 0 (result: %+v)", code, res)
		}
		if res.Status != "skipped" {
			t.Fatalf("status %q, want \"skipped\" — ops@example.com does not own what Ops@Example.com owns", res.Status)
		}
		if head := git(t, repo, "rev-parse", "HEAD"); head != base {
			t.Fatal("HEAD moved on a repo the script had no mandate in")
		}
		if got := owners(t, repo, ".github/workflows/ci.yml"); !sameOwners(got, "Ops@Example.com") {
			t.Fatalf("ownership moved to %v — emails must compare byte-exactly", got)
		}
	})

	t.Run("byte-identical email takes the shared lane", func(t *testing.T) {
		repo := mkRepo(t,
			"* @org/team_a\n.github ops@example.com @org/team_a\n.github/workflows ops@example.com\n",
			map[string]string{".github/workflows/ci.yml": "x", "src/main.go": "x"})

		res, code := runScriptScoped(t, repo, ".github/workflows", "ops@example.com")
		if code != 0 {
			t.Fatalf("exit %d, want 0 (result: %+v)", code, res)
		}
		if res.Status != "shared" {
			t.Fatalf("status %q, want \"shared\"", res.Status)
		}
		git(t, repo, "checkout", "-q", res.Branch)
		if got := owners(t, repo, ".github/workflows/ci.yml"); !sameOwners(got, "ops@example.com", "@org/team_a") {
			t.Fatalf("workflows owners after: %v, want exactly {ops@example.com, @org/team_a}", got)
		}
	})
}
