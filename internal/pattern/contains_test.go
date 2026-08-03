package pattern_test

import (
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
)

// containsCorpus is the pattern universe the brute-force checks range over. It
// deliberately mixes anchoring styles, because the shapes that look equivalent
// but are not ("/docs/" vs "docs/") are exactly where containment goes wrong.
var containsCorpus = []string{
	"*", "**", "/", "*.gradle", "**/*.gradle", "build.gradle", "/build.gradle",
	"docs", "docs/", "/docs", "/docs/", "/docs/**", "docs/*", "docs/**",
	"docs/*.md", "/docs/*.md", "/docs/readme.md", "**/docs", "**/docs/*",
	"x", "x/", "/x", "/x/", "/x/*", "/x/**", "x/y", "/x/y/", "/x/*.gradle",
	"x/**/y", "src/", "src/*", "/src/api/", "src/api", "services/**",
	"path/app/*", "path/app/", "/path/app/*.gradle", "v?.md", "/a b/*",
}

// containsPaths is the witness universe. Every path is a plausible repo-relative
// FILE path; several exist only to catch specific traps (a directory whose name
// matches a file glob, the same basename at several depths).
var containsPaths = func() []string {
	var out []string
	dirs := []string{
		"", "docs/", "x/", "x/y/", "x/y/z/", "src/", "src/api/", "path/", "path/app/",
		"a/docs/", "a/b/docs/", "web/docs/", "services/api/", "packages/docs/",
		"x/foo.gradle/", "docs/deep/", "a b/", "sub/x/",
	}
	names := []string{
		"build.gradle", "a.gradle", "readme.md", "v1.md", "main.go", "docs",
		"x", "y", "Dockerfile", "a.b.gradle",
	}
	for _, d := range dirs {
		for _, n := range names {
			out = append(out, d+n)
		}
	}
	return out
}()

// Contains must be SOUND: it may answer false when containment holds, but it
// must never answer true when a concrete path matches `inner` and not `outer`.
// An unsound answer here lets the planner amend a rule in place and silently
// hand an owner every future file that rule will ever match — the defect this
// whole mechanism exists to prevent.
func TestContainsIsSound(t *testing.T) {
	compiled := map[string]*pattern.Pattern{}
	for _, p := range containsCorpus {
		c, err := pattern.Compile(p)
		if err != nil {
			t.Fatalf("Compile(%q): %v", p, err)
		}
		compiled[p] = c
	}
	unsound := 0
	for _, outer := range containsCorpus {
		for _, inner := range containsCorpus {
			if !pattern.Contains(outer, inner) {
				continue
			}
			for _, p := range containsPaths {
				if compiled[inner].Match(p) && !compiled[outer].Match(p) {
					unsound++
					if unsound <= 20 {
						t.Errorf("Contains(%q, %q) = true, but %q matches %q and not %q",
							outer, inner, p, inner, outer)
					}
					break
				}
			}
		}
	}
	if unsound > 0 {
		t.Errorf("%d unsound pair(s)", unsound)
	}
}

// Reflexivity: a pattern always contains itself, whatever its shape.
func TestContainsIsReflexive(t *testing.T) {
	for _, p := range containsCorpus {
		if !pattern.Contains(p, p) {
			t.Errorf("Contains(%q, %q) = false, want true", p, p)
		}
	}
}

// The containments the planner relies on to amend a rule in place, and the
// near-miss shapes it must NOT accept.
func TestContainsKnownPairs(t *testing.T) {
	cases := []struct {
		outer, inner string
		want         bool
		why          string
	}{
		// A trailing slash does NOT anchor a single-segment pattern: "docs/"
		// matches "web/docs/spec.md", which the anchored scope does not.
		{"/docs/", "docs/", false, "unanchored rule escapes an anchored scope"},
		{"/docs", "docs/", false, "same, without the scope's trailing slash"},
		{"/docs/**", "docs/", false, "same, with an explicit globstar"},
		{"docs/", "/docs/", true, "the anchored subtree is inside the any-depth one"},

		// A file glob contains the literals that are instances of it.
		{"*.gradle", "build.gradle", true, "literal is an instance of the glob"},
		{"*.gradle", "/build.gradle", true, "anchored literal too"},
		{"*.gradle", "/x/*.gradle", true, "same final segment, deeper"},
		{"*.gradle", "**/*.gradle", true, "**/foo is equivalent to foo"},
		{"*.gradle", "*.md", false, "disjoint globs"},
		{"*.md", "docs/*.md", true, "any-depth glob covers the anchored one"},
		{"/docs/*.md", "/docs/readme.md", true, "glob segment covers the literal"},

		// A single-segment pattern denotes a whole subtree at any depth.
		{"x", "/x/", true, "anchored subtree is inside the any-depth entry"},
		{"x", "/x/**", true, "explicit globstar form"},
		{"x", "/x/*", true, "one-level form"},
		{"/x/", "x", false, "the any-depth entry escapes the anchored subtree"},

		// Anchoring without a leading slash: a mid-string slash already anchors.
		{"src/api", "/src/api/", true, "mid-string slash anchors at the root"},
		{"path/app/", "/path/app/*", true, "same, one level down"},
		{"docs/", "docs/*", true, "one-level rule inside the subtree"},
		{"docs/", "docs/**", true, "globstar rule inside the subtree"},

		// "*" and "**" are universal; "/" matches nothing.
		{"*", "build.gradle", true, "* matches at any depth"},
		{"**", "/x/y/", true, "** is universal"},
		{"/", "*", false, "/ matches nothing"},
		{"*", "/", true, "the empty language is contained in everything"},

		// One level vs. subtree are genuinely incomparable.
		{"/x/*", "/x/", false, "one level does not cover the subtree"},
		{"/x/", "/x/*", true, "subtree covers one level"},
	}
	for _, c := range cases {
		if got := pattern.Contains(c.outer, c.inner); got != c.want {
			t.Errorf("Contains(%q, %q) = %v, want %v — %s", c.outer, c.inner, got, c.want, c.why)
		}
	}
}

// Contains must not blow up or report true on patterns Compile rejects.
func TestContainsRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"", "!x", "a***b"} {
		if pattern.Contains(bad, "x") || pattern.Contains("x", bad) {
			if strings.Contains(bad, "!") || bad == "" || strings.Contains(bad, "***") {
				t.Errorf("Contains involving invalid pattern %q returned true", bad)
			}
		}
	}
}
