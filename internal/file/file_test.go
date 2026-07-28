// Package file_test defines the CODEOWNERS file model: a byte-preserving
// parse that can always be serialized back to the exact input.
//
// SPEC INV-5: comments, blank lines, inline spacing, and line ordering outside
// of directly modified lines are preserved exactly.
// SPEC S-9: a rule may legally have zero owners (GitHub's sanctioned
// substitute for `!` negation) and must not be treated as malformed.
// SPEC A-8 / current GitHub semantics: an invalid line is SKIPPED — it does
// not disable the file. (GitHub docs changed here; older docs said the whole
// file was disabled. The tool implements current behavior and still reports
// every invalid line.)
package file_test

import (
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
)

// SPEC INV-5: parse→serialize is byte-identical for any input, including
// CRLF line endings, trailing whitespace, missing final newline, tabs,
// comments, and invalid lines.
func TestINV5_RoundTrip_ByteIdentical(t *testing.T) {
	inputs := []string{
		"",
		"\n",
		"# comment only\n",
		"*  @a\n/x/ @b @c\n",
		"*\t@a\t\t@b   \n\n\n# trailing comment",
		"/x/ @a\r\n/y/ @b\r\n",
		"   # indented comment\n\t\n/z/ @a # inline\n",
		"not-an-owner-line bareword\n",     // invalid line still round-trips
		"/spaced\\ path/ @a\n",             // escaped space in pattern
		"/x/ @a #inline no space before\n", // preserved verbatim
		"/last-line-no-newline @a",
	}
	for _, in := range inputs {
		f := file.Parse([]byte(in))
		out := f.Bytes()
		if string(out) != in {
			t.Errorf("round trip changed bytes:\n in: %q\nout: %q", in, out)
		}
	}
}

// Blank lines and comments are recognized but carry no rules.
func TestParse_CommentsAndBlanks(t *testing.T) {
	f := file.Parse([]byte("# a comment\n\n   \n*.go @dev\n"))
	if got := len(f.Rules()); got != 1 {
		t.Fatalf("want 1 rule, got %d", got)
	}
	kinds := []file.LineKind{file.LineComment, file.LineBlank, file.LineBlank, file.LineRule}
	for i, want := range kinds {
		if f.Lines[i].Kind != want {
			t.Errorf("line %d: kind %v, want %v", i, f.Lines[i].Kind, want)
		}
	}
}

// SPEC S-9: a pattern with no owners is a VALID rule with an empty owner set.
func TestS9_ZeroOwnerRuleIsValid(t *testing.T) {
	f := file.Parse([]byte("/apps/ @octocat\n/apps/github\n"))
	rules := f.Rules()
	if len(rules) != 2 {
		t.Fatalf("want 2 rules, got %d (zero-owner rule must not be dropped)", len(rules))
	}
	if len(rules[1].Owners) != 0 {
		t.Errorf("zero-owner rule parsed with owners %v", rules[1].Owners)
	}
	if len(f.InvalidLines()) != 0 {
		t.Errorf("zero-owner rule flagged invalid: %v", f.InvalidLines())
	}
}

// Owner token forms per GitHub docs: @username, @org/team-name, email.
func TestParse_OwnerForms(t *testing.T) {
	f := file.Parse([]byte("/x/ @user @org/team-name docs@example.com\n"))
	r := f.Rules()[0]
	want := []string{"@user", "@org/team-name", "docs@example.com"}
	if strings.Join(r.Owners, " ") != strings.Join(want, " ") {
		t.Errorf("owners = %v, want %v", r.Owners, want)
	}
}

// Inline comments after the owner list are legal (GitHub's own example file:
// `*.js    @js-owner #This is an inline comment.`).
func TestParse_InlineComment(t *testing.T) {
	f := file.Parse([]byte("*.js    @js-owner #This is an inline comment.\n"))
	r := f.Rules()[0]
	if len(r.Owners) != 1 || r.Owners[0] != "@js-owner" {
		t.Errorf("owners = %v, want [@js-owner]", r.Owners)
	}
}

// Escaped spaces belong to the pattern, not the owner separator
// (mszostok/codeowners-validator issue #71 — naive whitespace splits break).
func TestParse_EscapedSpaceInPattern(t *testing.T) {
	f := file.Parse([]byte("/Path\\ With\\ Space.cs @a\n"))
	r := f.Rules()[0]
	if r.PatternText != "/Path\\ With\\ Space.cs" {
		t.Errorf("pattern = %q", r.PatternText)
	}
	if len(r.Owners) != 1 || r.Owners[0] != "@a" {
		t.Errorf("owners = %v", r.Owners)
	}
	if !r.Pattern.Match("Path With Space.cs") {
		t.Error("escaped-space pattern must match the literal path")
	}
}

// SPEC A-8: malformed owner tokens invalidate the LINE (kind "Invalid owner"),
// matching GitHub's codeowners/errors API vocabulary; the rest of the file
// still parses.
func TestA8_InvalidOwnerSkipsLineOnly(t *testing.T) {
	f := file.Parse([]byte("* user-without-at\n/x/ @valid\n"))
	if len(f.Rules()) != 1 {
		t.Fatalf("valid rule count = %d, want 1", len(f.Rules()))
	}
	inv := f.InvalidLines()
	if len(inv) != 1 {
		t.Fatalf("invalid lines = %d, want 1", len(inv))
	}
	if inv[0].Err.Kind != "Invalid owner" {
		t.Errorf("kind = %q, want \"Invalid owner\"", inv[0].Err.Kind)
	}
}

// SPEC A-8: an unsupported pattern (negation, triple star) invalidates the
// line with kind "Invalid pattern".
func TestA8_InvalidPatternSkipsLineOnly(t *testing.T) {
	f := file.Parse([]byte("!negated @a\n***/x @b\n/fine/ @c\n"))
	if len(f.Rules()) != 1 {
		t.Fatalf("valid rule count = %d, want 1", len(f.Rules()))
	}
	for _, inv := range f.InvalidLines() {
		if inv.Err.Kind != "Invalid pattern" {
			t.Errorf("kind = %q, want \"Invalid pattern\"", inv.Err.Kind)
		}
	}
}

// Amending a line keeps its leading whitespace, pattern spelling, and inline
// comment; only the owner list changes (INV-5 for the modified line itself:
// untouched portions stay put).
func TestINV5_AmendOwnersPreservesLineFurniture(t *testing.T) {
	f := file.Parse([]byte("/x/\t@a   @b # keep me\n"))
	f.SetOwners(0, []string{"@a", "@b", "@c"})
	got := string(f.Bytes())
	want := "/x/\t@a @b @c # keep me\n"
	if got != want {
		t.Errorf("amended line = %q, want %q", got, want)
	}
}

// Amending a zero-owner rule to have owners inserts a separator.
func TestAmend_ZeroOwnerRuleGainsOwners(t *testing.T) {
	f := file.Parse([]byte("/apps/github\n"))
	f.SetOwners(0, []string{"@sec"})
	if got := string(f.Bytes()); got != "/apps/github @sec\n" {
		t.Errorf("got %q", got)
	}
}

// InsertRule adds a new line without disturbing any existing line (R-1).
func TestInsertRule(t *testing.T) {
	f := file.Parse([]byte("# header\n/x/ @a\n"))
	f.InsertRule(2, "/y/", []string{"@b"})
	if got := string(f.Bytes()); got != "# header\n/x/ @a\n/y/ @b\n" {
		t.Errorf("got %q", got)
	}
	// Inserting keeps EOL style of the file (CRLF file gets CRLF line).
	f2 := file.Parse([]byte("/x/ @a\r\n"))
	f2.InsertRule(1, "/y/", []string{"@b"})
	if got := string(f2.Bytes()); got != "/x/ @a\r\n/y/ @b\r\n" {
		t.Errorf("got %q", got)
	}
}

// DeleteLine exists solely for R-6's `inherit` policy — the one sanctioned
// deletion in the writer.
func TestDeleteLine(t *testing.T) {
	f := file.Parse([]byte("/x/ @a\n/x/sub/ @b\n"))
	f.DeleteLine(1)
	if got := string(f.Bytes()); got != "/x/ @a\n" {
		t.Errorf("got %q", got)
	}
}

// Inserting at EOF when the final line lacks a newline appends ONLY a
// newline to that line — its inline spacing survives byte-for-byte. (Review
// finding: the old code re-rendered the final rule line, collapsing
// "@b  @c" to "@b @c" invisibly to the plan's diff.)
func TestINV5_InsertAtEOFPreservesFinalLineBytes(t *testing.T) {
	f := file.Parse([]byte("/x/ @a\n/y/ @b  @c"))
	f.InsertRule(2, "/z/", []string{"@d"})
	if got := string(f.Bytes()); got != "/x/ @a\n/y/ @b  @c\n/z/ @d\n" {
		t.Errorf("got %q — final line's double space must survive", got)
	}
}

// Second-review finding: rendering a rule whose pattern ends in an ESCAPED
// space must not eat that space — only unescaped separator whitespace may be
// trimmed or checked when re-rendering.
func TestRender_TrailingEscapedSpacePattern(t *testing.T) {
	// Pattern `a\ ` (matches a path ending in a space), one owner.
	f := file.Parse([]byte("a\\  @x\n"))
	r := f.Rules()[0]
	if r.PatternText != "a\\ " {
		t.Fatalf("pattern = %q", r.PatternText)
	}
	// Amend to zero owners: the escaped space must survive the trim.
	f.SetOwners(0, nil)
	if got := string(f.Bytes()); got != "a\\ \n" {
		t.Errorf("zero-owner render = %q, want %q", got, "a\\ \n")
	}
	// Amend back to owners: separator must be added AFTER the escaped space.
	f2 := file.Parse([]byte("a\\ \n"))
	f2.SetOwners(0, []string{"@y"})
	got := string(f2.Bytes())
	rf := file.Parse([]byte(got))
	if len(rf.Rules()) != 1 || rf.Rules()[0].PatternText != "a\\ " {
		t.Errorf("re-parse of %q lost the pattern: %+v", got, rf.Rules())
	}
}
