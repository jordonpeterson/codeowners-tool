// Package pattern_test defines the CODEOWNERS pattern-matching semantics.
//
// SPEC S-2: patterns are gitignore-LIKE, but with no negation (`!`) and no
// character ranges (`[a-z]`). GitHub's docs list three explicit exceptions to
// gitignore syntax; everything else follows gitignore matching rules.
// SPEC S-6: matching is case-sensitive regardless of local filesystem.
//
// The authoritative corpus in testdata/patterns.json is vendored from
// hmarr/codeowners (MIT), the reference implementation this matcher is
// differentially tested against.
package pattern_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
)

type corpusEntry struct {
	Name    string          `json:"name"`
	Pattern string          `json:"pattern"`
	Paths   map[string]bool `json:"paths"`
}

// SPEC S-2 (differential): every case in the vendored hmarr/codeowners corpus
// must match GitHub's observed behavior exactly.
func TestS2_Corpus_MatchesReferenceImplementation(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "patterns.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var corpus []corpusEntry
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(corpus) == 0 {
		t.Fatal("empty corpus")
	}
	for _, entry := range corpus {
		t.Run(entry.Name, func(t *testing.T) {
			p, err := pattern.Compile(entry.Pattern)
			if err != nil {
				t.Fatalf("Compile(%q): %v", entry.Pattern, err)
			}
			for path, want := range entry.Paths {
				if got := p.Match(path); got != want {
					t.Errorf("pattern %q path %q: got %v, want %v", entry.Pattern, path, got, want)
				}
			}
		})
	}
}

// SPEC S-2: examples taken verbatim from GitHub's "About code owners" docs.
func TestS2_GitHubDocExamples(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
		why     string
	}{
		{"*", "anything/at/all.txt", true, "star matches everything"},
		{"*.js", "deeply/nested/file.js", true, "extension glob matches at any depth"},
		{"*.js", "file.jsx", false, "extension must match exactly"},
		{"/build/logs/", "build/logs/x/y.log", true, "anchored dir matches recursively"},
		{"/build/logs/", "other/build/logs/y.log", false, "leading slash anchors to root"},
		{"docs/*", "docs/getting-started.md", true, "single star matches direct children"},
		{"docs/*", "docs/build-app/troubleshooting.md", false, "single star does not cross /"},
		{"apps/", "x/apps/y/z.go", true, "unanchored dir matches anywhere"},
		{"apps/", "apps", false, "trailing slash requires a directory, not a file"},
		{"**/logs", "build/logs/a.txt", true, "leading globstar matches dir at any depth"},
		{"**/logs", "deeply/nested/logs/a.txt", true, "leading globstar, deeper"},
	}
	for _, c := range cases {
		p, err := pattern.Compile(c.pattern)
		if err != nil {
			t.Fatalf("Compile(%q): %v", c.pattern, err)
		}
		if got := p.Match(c.path); got != c.want {
			t.Errorf("pattern %q path %q: got %v want %v (%s)", c.pattern, c.path, got, c.want, c.why)
		}
	}
}

// SPEC S-6: paths are case-sensitive. `/Src/` on a tree containing `src/` is a
// dead rule that looks fine on a case-insensitive filesystem.
func TestS6_CaseSensitive(t *testing.T) {
	p, err := pattern.Compile("/Src/")
	if err != nil {
		t.Fatal(err)
	}
	if p.Match("src/main.go") {
		t.Error("/Src/ must NOT match src/main.go: CODEOWNERS is case-sensitive")
	}
	if !p.Match("Src/main.go") {
		t.Error("/Src/ must match Src/main.go")
	}
}

// SPEC S-2: character ranges are NOT ranges in CODEOWNERS. GitHub treats
// `[` and `]` as literal characters (observed via hmarr issue #42/#44: Next.js
// paths like /apps/[param]/file.ts only match the literal bracket path).
func TestS2_BracketsAreLiterals(t *testing.T) {
	p, err := pattern.Compile("/apps/[param]/file.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Match("apps/[param]/file.ts") {
		t.Error("brackets must match literally")
	}
	if p.Match("apps/p/file.ts") {
		t.Error("brackets must NOT act as a character range")
	}
}

// SPEC S-2: negation is not supported. A pattern starting with `!` is a
// compile error surfaced to the caller, never a silent negation.
func TestS2_NegationRejected(t *testing.T) {
	if _, err := pattern.Compile("!/foo"); err == nil {
		t.Error("negation pattern must be rejected")
	}
}

// SPEC S-2/S-6: GitHub honors no `\#` escape of a leading hash — a line
// starting with '#' is always a comment there, so a `\#…` rule is dead on
// GitHub. Same standard as `!`: a mutation tool must never accept or emit one.
func TestS2_EscapedLeadingHashRejected(t *testing.T) {
	if _, err := pattern.Compile(`\#tag.md`); err == nil {
		t.Error(`pattern \#tag.md must be rejected: GitHub reads the written line as a comment`)
	}
}

// Only the LEADING position is special-cased: a mid-pattern '#' needs no
// escape at all, and a mid-pattern `\#` keeps the reference implementation's
// literal-'#' meaning.
func TestS2_MidPatternHashAccepted(t *testing.T) {
	for _, c := range []struct{ pat, path string }{
		{"a#b.md", "a#b.md"},
		{`a\#b.md`, "a#b.md"},
	} {
		p, err := pattern.Compile(c.pat)
		if err != nil {
			t.Fatalf("Compile(%q): %v", c.pat, err)
		}
		if !p.Match(c.path) {
			t.Errorf("pattern %q must match %q literally", c.pat, c.path)
		}
	}
}

// Empty and comment-like inputs are the parser's problem, not the matcher's;
// compiling them is an error so bugs surface loudly.
func TestCompile_RejectsEmpty(t *testing.T) {
	if _, err := pattern.Compile(""); err == nil {
		t.Error("empty pattern must be rejected")
	}
}

// Literal special characters observed accepted by GitHub (hmarr issues
// #47 caret, #50 tilde, #52 colon): they match themselves.
func TestS2_LiteralSpecials(t *testing.T) {
	cases := []struct{ pat, path string }{
		{"vllm/v1/worker/^cpu", "vllm/v1/worker/^cpu/model.py"},
		{"services/foo:bar/x.go", "services/foo:bar/x.go"},
		{"a~b/c.txt", "a~b/c.txt"},
	}
	for _, c := range cases {
		p, err := pattern.Compile(c.pat)
		if err != nil {
			t.Fatalf("Compile(%q): %v", c.pat, err)
		}
		if !p.Match(c.path) {
			t.Errorf("pattern %q must match %q literally", c.pat, c.path)
		}
	}
}
