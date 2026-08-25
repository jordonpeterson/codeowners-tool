// Scope-spelling tests for co-own.sh: the pattern the operator types must
// flow through to the op the script runs and the verify that proves it —
// anchored stays anchored, unanchored stays unanchored.
package fleet

import (
	"strings"
	"testing"
)

// An anchored single-segment scope (/docs/) matches only at the root. Here the
// broader rule already lists the owner, so the fix is deleting the dedicated
// line — and the committed diff must remove exactly that anchored rule, adding
// nothing, so deeper directories that merely share the name (sub/docs/) are
// untouched by construction.
func TestCoOwn_AnchoredScopeDiffTouchesOnlyAnchoredRule(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		"* @org/broad @org/platform\n/sub/docs/ @org/other\n/docs/ @org/platform\n",
		map[string]string{"docs/a.md": "a\n", "sub/docs/b.md": "b\n", "src/main.go": "x\n"})

	res, code := runScriptScoped(t, repo, "/docs/", "@org/platform")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (result: %+v)", code, res)
	}
	if res.Status != "shared" {
		t.Fatalf("status %q, want \"shared\"", res.Status)
	}
	// Branch names cannot carry the slashes; the sanitized spelling is for
	// naming only, never for the op or its proof.
	if res.Branch != "co-own/docs" {
		t.Fatalf("branch %q, want \"co-own/docs\"", res.Branch)
	}
	git(t, repo, "checkout", "-q", res.Branch)
	if got := owners(t, repo, "docs/a.md"); !sameOwners(got, "@org/broad", "@org/platform") {
		t.Fatalf("docs/a.md owners after: %v, want exactly {@org/broad, @org/platform}", got)
	}
	if got := owners(t, repo, "sub/docs/b.md"); !sameOwners(got, "@org/other") {
		t.Fatalf("sub/docs/b.md owners became %v — the anchored scope leaked to a deeper docs/", got)
	}
	diff := git(t, repo, "show", "--pretty=format:", "--unified=0", "HEAD")
	var removed, added []string
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++"):
		case strings.HasPrefix(line, "-"):
			removed = append(removed, strings.TrimSpace(strings.TrimPrefix(line, "-")))
		case strings.HasPrefix(line, "+"):
			added = append(added, line)
		}
	}
	if len(removed) != 1 || removed[0] != "/docs/ @org/platform" || len(added) != 0 {
		t.Fatalf("committed diff must delete exactly the anchored rule and add nothing:\n%s", diff)
	}
}

// An unanchored single-segment scope (docs) matches at any depth — that is
// what the operator typed, and the script must pass it through rather than
// anchor it: every docs/ directory in the tree ends co-owned, and only the
// dedicated unanchored line goes.
func TestCoOwn_UnanchoredScopePassesThroughUnmodified(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		"* @org/broad @org/platform\ndocs @org/platform\n",
		map[string]string{"docs/a.md": "a\n", "sub/docs/b.md": "b\n", "src/main.go": "x\n"})

	res, code := runScriptScoped(t, repo, "docs", "@org/platform")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (result: %+v)", code, res)
	}
	if res.Status != "shared" {
		t.Fatalf("status %q, want \"shared\"", res.Status)
	}
	if res.Branch != "co-own/docs" {
		t.Fatalf("branch %q, want \"co-own/docs\"", res.Branch)
	}
	git(t, repo, "checkout", "-q", res.Branch)
	// Both depths were in the typed scope, so both end co-owned.
	if got := owners(t, repo, "docs/a.md"); !sameOwners(got, "@org/broad", "@org/platform") {
		t.Fatalf("docs/a.md owners after: %v, want exactly {@org/broad, @org/platform}", got)
	}
	if got := owners(t, repo, "sub/docs/b.md"); !sameOwners(got, "@org/broad", "@org/platform") {
		t.Fatalf("sub/docs/b.md owners after: %v — the unanchored scope was narrowed", got)
	}
	if got := owners(t, repo, "src/main.go"); !sameOwners(got, "@org/broad", "@org/platform") {
		t.Fatalf("src/main.go owners moved to %v — the script edited outside its scope", got)
	}
}
