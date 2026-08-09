// Package file is the lossless CODEOWNERS file model.
//
// Parsing preserves every byte: Parse followed by Bytes is always
// byte-identical (INV-5). Mutation is restricted to the four primitives the
// writer rules allow: amend a line's owner list, insert a new line, delete a
// line (solely for R-6's `inherit` policy), and replace an INVALID line with
// its repaired text (solely for lint's owner-spacing fix, which is the one
// edit no rule-level primitive can express). Nothing here reorders lines.
//
// Invalid lines are retained, reported, and skipped during resolution,
// matching GitHub's current per-line-skip semantics.
package file

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
)

// LineKind classifies a parsed line.
type LineKind int

const (
	LineBlank LineKind = iota
	LineComment
	LineRule
	LineInvalid
)

func (k LineKind) String() string {
	switch k {
	case LineBlank:
		return "blank"
	case LineComment:
		return "comment"
	case LineRule:
		return "rule"
	case LineInvalid:
		return "invalid"
	}
	return "unknown"
}

// SyntaxError describes why a line is invalid. Kind vocabulary matches
// GitHub's codeowners/errors API: "Invalid pattern", "Invalid owner".
type SyntaxError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Line    int    `json:"line"` // 1-based
	Source  string `json:"source"`
}

// Rule is a valid pattern line.
type Rule struct {
	PatternText string
	Pattern     *pattern.Pattern
	Owners      []string
	LineIndex   int // index into File.Lines

	prefix string // leading ws + pattern text + separator ws
	suffix string // inline comment (including any ws before '#'), or ""
}

// OwnersCopy returns a defensive copy of the rule's owner list (non-nil).
func (r *Rule) OwnersCopy() []string {
	out := make([]string, len(r.Owners))
	copy(out, r.Owners)
	return out
}

// Line is one physical line of the file.
type Line struct {
	Raw   string // original text, without EOL
	EOL   string // "\n", "\r\n", or "" (final line without newline)
	Kind  LineKind
	Rule  *Rule
	Err   *SyntaxError
	dirty bool
}

// File is a parsed CODEOWNERS file.
type File struct {
	Lines []*Line
}

var (
	handleRe = regexp.MustCompile(`^@[A-Za-z0-9][A-Za-z0-9_.\-]*(/[A-Za-z0-9_.\-]+)?$`)
	emailRe  = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// ValidOwnerToken reports whether tok is a syntactically valid owner:
// @username, @org/team-name, or an email address.
func ValidOwnerToken(tok string) bool {
	if strings.HasPrefix(tok, "@") {
		return handleRe.MatchString(tok)
	}
	return emailRe.MatchString(tok)
}

// IsEmailOwner reports whether a valid owner token is an email owner (R-13).
func IsEmailOwner(tok string) bool { return !strings.HasPrefix(tok, "@") }

// Parse parses CODEOWNERS content. It never fails: unparseable lines become
// LineInvalid with a SyntaxError.
func Parse(content []byte) *File {
	f := &File{}
	rest := string(content)
	lineNo := 0
	for len(rest) > 0 || lineNo == 0 && len(content) > 0 {
		if len(rest) == 0 {
			break
		}
		var raw, eol string
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			raw, eol = rest[:i], "\n"
			if strings.HasSuffix(raw, "\r") {
				raw, eol = raw[:len(raw)-1], "\r\n"
			}
			rest = rest[i+1:]
		} else {
			raw, eol = rest, ""
			rest = ""
		}
		lineNo++
		f.Lines = append(f.Lines, parseLine(raw, eol, lineNo))
	}
	return f
}

func parseLine(raw, eol string, lineNo int) *Line {
	ln := &Line{Raw: raw, EOL: eol}
	trimmed := strings.TrimLeft(raw, " \t")
	switch {
	case trimmed == "":
		ln.Kind = LineBlank
		return ln
	case strings.HasPrefix(trimmed, "#"):
		ln.Kind = LineComment
		return ln
	}

	leadingWS := raw[:len(raw)-len(trimmed)]

	patEnd := patternTokenEnd(trimmed)
	patText := trimmed[:patEnd]
	rest := trimmed[patEnd:]

	pat, err := pattern.Compile(patText)
	if err != nil {
		ln.Kind = LineInvalid
		ln.Err = &SyntaxError{Kind: "Invalid pattern", Message: err.Error(), Line: lineNo, Source: raw}
		return ln
	}

	// Separator whitespace after the pattern.
	sepEnd := leadingWSLen(rest)
	sep, rest := rest[:sepEnd], rest[sepEnd:]

	// Owner tokens until end of line or an inline comment token.
	var owners []string
	suffix := ""
	for rest != "" {
		if rest[0] == '#' {
			suffix = rest
			break
		}
		end := strings.IndexAny(rest, " \t")
		var tok string
		if end < 0 {
			tok, rest = rest, ""
		} else {
			tok, rest = rest[:end], rest[end:]
		}
		if !ValidOwnerToken(tok) {
			ln.Kind = LineInvalid
			ln.Err = &SyntaxError{Kind: "Invalid owner", Message: "invalid owner token " + tok, Line: lineNo, Source: raw}
			return ln
		}
		owners = append(owners, tok)
		// Consume whitespace; if a comment follows, keep that whitespace in
		// the suffix so amended lines preserve it.
		wsEnd := leadingWSLen(rest)
		if wsEnd < len(rest) && rest[wsEnd] == '#' {
			suffix = rest
			break
		}
		rest = rest[wsEnd:]
	}

	ln.Kind = LineRule
	ln.Rule = &Rule{
		PatternText: patText,
		Pattern:     pat,
		Owners:      owners,
		prefix:      leadingWS + patText + sep,
		suffix:      suffix,
	}
	return ln
}

// patternTokenEnd returns the length of the pattern token at the head of s.
//
// The scan is escape-aware because a pattern may legally contain an ESCAPED
// space (`a\ b` — a path with a space in it), so the token does not simply end
// at the first byte of whitespace.
func patternTokenEnd(s string) int {
	end, escaped := 0, false
	for end < len(s) {
		c := s[end]
		if escaped {
			escaped = false
		} else if c == '\\' {
			escaped = true
		} else if c == ' ' || c == '\t' {
			break
		}
		end++
	}
	return end
}

// leadingWSLen returns the length of the run of spaces and tabs at the head of s.
func leadingWSLen(s string) int {
	n := 0
	for n < len(s) && (s[n] == ' ' || s[n] == '\t') {
		n++
	}
	return n
}

// SplitOwnerRegion splits a raw line into its head — leading whitespace, the
// pattern token, and the whitespace separating pattern from owners — and the
// owner region that follows. ok is false for blank and comment lines, which
// have no pattern token at all.
//
// Exported because an INVALID line has no Rule to interrogate: it never got
// past parseLine's owner loop, so PatternText, prefix and Owners do not exist
// for it. Repairing such a line (lint's owner-spacing fix) needs exactly the
// split parseLine performs, and reimplementing the escape-aware pattern scan
// elsewhere would let the two drift on the one input that matters — a pattern
// ending in an escaped space.
func SplitOwnerRegion(raw string) (head, owners string, ok bool) {
	trimmed := strings.TrimLeft(raw, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	lead := raw[:len(raw)-len(trimmed)]
	patEnd := patternTokenEnd(trimmed)
	rest := trimmed[patEnd:]
	sepEnd := leadingWSLen(rest)
	return lead + trimmed[:patEnd] + rest[:sepEnd], rest[sepEnd:], true
}

// Bytes serializes the file. Untouched lines are emitted byte-identically.
func (f *File) Bytes() []byte {
	var buf bytes.Buffer
	for _, ln := range f.Lines {
		if !ln.dirty {
			buf.WriteString(ln.Raw)
			buf.WriteString(ln.EOL)
			continue
		}
		buf.WriteString(renderRule(ln.Rule))
		buf.WriteString(ln.EOL)
	}
	return buf.Bytes()
}

func renderRule(r *Rule) string {
	prefix := r.prefix
	if len(r.Owners) == 0 {
		s := trimTrailingUnescapedWS(prefix)
		if r.suffix != "" {
			s += " " + strings.TrimLeft(r.suffix, " \t")
		}
		return s
	}
	if !endsWithUnescapedWS(prefix) {
		prefix += " "
	}
	s := prefix + strings.Join(r.Owners, " ")
	if r.suffix != "" {
		if !strings.HasPrefix(r.suffix, " ") && !strings.HasPrefix(r.suffix, "\t") {
			s += " "
		}
		s += r.suffix
	}
	return s
}

// Escape-aware whitespace handling: a pattern may legally end in an ESCAPED
// space (`a\ ` — a path ending in a space). Naive TrimRight/HasSuffix mangled
// such patterns into ones matching a different path (second-review finding).
func endsWithUnescapedWS(s string) bool {
	if s == "" {
		return false
	}
	last := s[len(s)-1]
	if last != ' ' && last != '\t' {
		return false
	}
	bs := 0
	for i := len(s) - 2; i >= 0 && s[i] == '\\'; i-- {
		bs++
	}
	return bs%2 == 0
}

func trimTrailingUnescapedWS(s string) string {
	for endsWithUnescapedWS(s) {
		s = s[:len(s)-1]
	}
	return s
}

// Rules returns the valid rules in file order, with LineIndex set.
func (f *File) Rules() []*Rule {
	var rules []*Rule
	for i, ln := range f.Lines {
		if ln.Kind == LineRule {
			ln.Rule.LineIndex = i
			rules = append(rules, ln.Rule)
		}
	}
	return rules
}

// InvalidLines returns the lines that failed to parse.
func (f *File) InvalidLines() []*Line {
	var out []*Line
	for _, ln := range f.Lines {
		if ln.Kind == LineInvalid {
			out = append(out, ln)
		}
	}
	return out
}

// SetOwners amends the owner list of the rule at line index i (R-1: amend in
// place, never reorder).
func (f *File) SetOwners(i int, owners []string) {
	ln := f.Lines[i]
	if ln.Kind != LineRule {
		panic("SetOwners on non-rule line")
	}
	ln.Rule.Owners = append([]string(nil), owners...)
	ln.dirty = true
}

// InsertRule inserts a new rule line at index i (existing line i shifts
// down). The new line adopts the file's newline style. If the current final
// line lacks a trailing newline and the insertion lands after it, that line
// gains one — the only byte change ever made to an untouched line.
func (f *File) InsertRule(i int, patternText string, owners []string) *Rule {
	pat, err := pattern.Compile(patternText)
	if err != nil {
		panic("InsertRule with invalid pattern: " + err.Error())
	}
	eol := "\n"
	if len(f.Lines) > 0 {
		if e := f.Lines[0].EOL; e != "" {
			eol = e
		}
	}
	if i == len(f.Lines) && len(f.Lines) > 0 && f.Lines[len(f.Lines)-1].EOL == "" {
		// Append ONLY a newline to the final line; its Raw bytes are emitted
		// untouched (marking it dirty would re-render it and collapse its
		// inline spacing — found in review).
		f.Lines[len(f.Lines)-1].EOL = eol
	}
	r := &Rule{
		PatternText: patternText,
		Pattern:     pat,
		Owners:      append([]string(nil), owners...),
		prefix:      patternText + " ",
	}
	ln := &Line{EOL: eol, Kind: LineRule, Rule: r, dirty: true}
	f.Lines = append(f.Lines, nil)
	copy(f.Lines[i+1:], f.Lines[i:])
	f.Lines[i] = ln
	return r
}

// ReplaceLine replaces line i with raw, re-parsed, and returns the new line.
//
// It is the only primitive that can reach an INVALID line. Such a line has no
// Rule, so SetOwners cannot touch it, and DeleteLine would throw away a rule
// somebody wrote and believes is in force — neither is a repair. Its sole
// caller is lint's owner-spacing fix, which hands back the same line with
// whitespace removed from inside an @handle.
//
// The replacement text is emitted VERBATIM (the line is left clean, not
// dirty): raw is already the exact bytes intended, and re-rendering it from
// the parsed rule would collapse whatever column alignment the file uses.
// The original EOL is preserved, so a repair never changes the file's line
// endings.
func (f *File) ReplaceLine(i int, raw string) *Line {
	ln := parseLine(raw, f.Lines[i].EOL, i+1)
	f.Lines[i] = ln
	return ln
}

// DeleteLine removes line i. Exists solely for R-6's `inherit` policy.
func (f *File) DeleteLine(i int) {
	f.Lines = append(f.Lines[:i], f.Lines[i+1:]...)
}

// Size returns the serialized byte size (R-9, S-4).
func (f *File) Size() int { return len(f.Bytes()) }

// LineText returns the current text of line i without its EOL — original
// bytes if untouched, rendered text if amended.
func (f *File) LineText(i int) string {
	ln := f.Lines[i]
	if !ln.dirty {
		return ln.Raw
	}
	return renderRule(ln.Rule)
}
