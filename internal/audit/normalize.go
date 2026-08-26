package audit

import (
	"fmt"
	"strings"
)

// Partial Unicode decomposition, for A-5's normalization branch.
//
// The failure it diagnoses: a monorepo touched by both macOS (which hands
// filenames to the filesystem decomposed, NFD) and Linux (which stores whatever
// was typed, normally composed, NFC) ends up with `docs/re<U+0301>union/` in the
// tree and `/docs/r<U+00E9>union/` in CODEOWNERS. CODEOWNERS matches bytes, so
// the rule is dead — correctly — but the two strings render identically, and
// A-4's "may be deliberate" leaves the reader concluding the tool is broken.
//
// go.mod lists no dependencies, so golang.org/x/text/unicode/norm is not
// available and the standard library ships no normalizer. Full NFD would need
// the whole Unicode decomposition table; the finding needs something narrower —
// "these two differ by combining-mark composition, not by name" — so only the
// precomposed letters of Latin-1 Supplement are decomposed here. That is the
// range the macOS/Linux filename trap actually lives in, and the finding states
// what was compared so nobody reads more into it than that.
//
// Letters with NO canonical decomposition are deliberately absent from the
// table: Æ, Ð, Ø, Þ and ß are separate letters, not accented forms, and folding
// them to A/D/O/T/s would report "differs only by normalization" between two
// genuinely different names.

// latinBases[i] is the base letter of U+00C0+i (upper) and U+00E0+i (lower),
// or NUL where the rune has no canonical decomposition. All bases are ASCII, so
// byte indexing is exact.
const (
	latinUpperBases = "AAAAAA\x00CEEEEIIII\x00NOOOOO\x00\x00UUUUY\x00\x00"
	latinLowerBases = "aaaaaa\x00ceeeeiiii\x00nooooo\x00\x00uuuuy\x00y"
)

// latinMarks[i] is the combining mark U+00C0+i decomposes to. The lowercase
// block matches slot for slot except the last (ß has no decomposition where ÿ
// takes a diaeresis), which latinDecompose special-cases.
var latinMarks = [32]rune{
	0x0300, 0x0301, 0x0302, 0x0303, 0x0308, 0x030A, 0, 0x0327,
	0x0300, 0x0301, 0x0302, 0x0308, 0x0300, 0x0301, 0x0302, 0x0308,
	0, 0x0303, 0x0300, 0x0301, 0x0302, 0x0303, 0x0308, 0,
	0, 0x0300, 0x0301, 0x0302, 0x0308, 0x0301, 0, 0,
}

// latinDecompose returns the canonical base letter and combining mark of a
// Latin-1 precomposed letter, or (0, 0) for every other rune.
func latinDecompose(r rune) (base, mark rune) {
	var bases string
	switch {
	case r >= 0x00C0 && r <= 0x00DF:
		bases = latinUpperBases
	case r >= 0x00E0 && r <= 0x00FF:
		bases = latinLowerBases
	default:
		return 0, 0
	}
	i := int(r) & 0x1F
	b := bases[i]
	mark = latinMarks[i]
	if r == 0x00FF { // ÿ = y + diaeresis; its slot holds ß's empty entry
		mark = 0x0308
	}
	if b == 0 || mark == 0 {
		return 0, 0
	}
	return rune(b), mark
}

// decompose returns s with every Latin-1 precomposed letter replaced by base +
// combining mark, and everything else copied through unchanged. Two strings
// that agree after this differ only in how those letters were composed.
//
// It is NOT a normalizer: nothing outside Latin-1 is touched, and the canonical
// ordering of adjacent marks is not applied (the letters here decompose to one
// mark each, so there is no pair to order). Both sides of every comparison go
// through it, so a string it cannot decompose simply fails to match rather than
// matching something it should not.
func decompose(s string) string {
	if !hasLatinPrecomposed(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if base, mark := latinDecompose(r); base != 0 {
			b.WriteRune(base)
			b.WriteRune(mark)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func hasLatinPrecomposed(s string) bool {
	for _, r := range s {
		if base, _ := latinDecompose(r); base != 0 {
			return true
		}
	}
	return false
}

// composedCodepoints names the first precomposed letter in s and how it and the
// tree spell it. The whole difficulty of this finding is that the two strings
// render identically, so a message that only quotes them tells the reader
// nothing they can act on. Empty when s holds no precomposed letter — the
// pattern is then the decomposed side, and there is nothing to point at.
func composedCodepoints(s string) string {
	for _, r := range s {
		base, mark := latinDecompose(r)
		if base == 0 {
			continue
		}
		return fmt.Sprintf("%q is U+%04X in the pattern and U+%04X U+%04X in the tree", string(r), r, base, mark)
	}
	return ""
}
