package pattern

import "strings"

// NeverMatches reports whether a pattern's language is EMPTY: no repo-relative
// path, in any repository, present or future, can match it. It is a statement
// about the pattern alone, which is what puts a refusal built on it at exit 3
// (the policy is broken everywhere) rather than exit 2 (this repo needs a
// human).
//
// It is deliberately NOT part of Compile. GitHub loads such a line and skips no
// line for it — `**/ @org/x` is a well-formed rule that simply owns nothing —
// so calling it a syntax error would misreport an existing file: audit would
// report it as a line GitHub skips (S-3/A-1) instead of as a dead pattern
// (A-4). Reading is unaffected; only WRITING one is refused, by ops.checkScope,
// on the same standard as `!` and `\#` — "a mutation tool must never emit a
// rule that is dead by construction" (R-5).
//
// Two families reach it. Both are already documented in this package as dead
// and neither was refused anywhere:
//
//   - `/`, which buildPatternRegex compiles to `\A\z`;
//   - a LEADING `**` immediately followed by another `**` — `**/`, `**/**`,
//     `**/**/x`. buildPatternRegex tracks whether the separator was already
//     consumed (needSlash), and the `**` arms do not honour it: the leading
//     `**` emits `(?:.+/)?` and clears the flag, then the next `**` emits its
//     own separator regardless, so every match would need a leading `/` or a
//     `//`. See contains.go, which refuses to model these for the same reason.
//   - an empty segment (`a//b`, `//x`), which forces the same `//`.
//
// Non-leading adjacency is NOT dead and must not be refused: `x/**/**` compiles
// to `\Ax(?:/.+)?/.*\z` and matches `x/a`, and `foo/**/` normalizes to the same
// shape. The analysis below decides each pattern rather than pattern-matching
// on spellings, so those stay alive.
//
// Method: the regex buildPatternRegex emits is a concatenation of pieces, each
// of which either consumes a mandatory separator, contributes segment text, or
// contributes an optional run. Track, as a SET, what a valid path prefix can
// look like at each boundary — nothing yet, ending in `/`, or ending inside a
// segment — and drop any transition that would require a leading `/` or a `//`,
// neither of which git produces. The pattern is dead exactly when no state
// survives, or when no surviving state can end a path (a path never ends in
// `/`). Since every piece is modeled from the arm that emits it, the answer is
// exact for this compiler, and TestNeverMatchesAgreesWithTheMatcher checks that
// against the matcher over a generated corpus.
func NeverMatches(patternStr string) bool {
	if patternStr == "" || strings.Contains(patternStr, "***") {
		// Compile refuses both; claiming anything about their language would
		// be a claim about a pattern that cannot exist.
		return false
	}
	if patternStr == "/" {
		return true // buildPatternRegex: `\A\z`
	}
	return !liveAtEnd(normalizeSegs(patternStr))
}

// Prefix shapes a partial match can have at a segment boundary.
const (
	stStart = 1 << iota // nothing emitted yet
	stSlash             // the prefix ends with "/"
	stSeg               // the prefix ends inside or at the end of a segment
)

// liveAtEnd walks the normalized segments exactly as buildPatternRegex does,
// including its needSlash bookkeeping, and reports whether some valid path can
// match.
func liveAtEnd(segs []string) bool {
	last := len(segs) - 1
	st := stStart
	needSlash := false

	// A mandatory separator: only a prefix that ended inside a segment can take
	// one. From the start it would be a leading slash, after a slash a `//`.
	sep := func() {
		if st&stSeg != 0 {
			st = stSlash
		} else {
			st = 0
		}
	}

	for i, seg := range segs {
		switch seg {
		case "**":
			switch {
			case i == 0 && i == last:
				st = stSeg | stSlash // `.+`
			case i == 0:
				st = stStart | stSlash // `(?:.+/)?`
				needSlash = false
			case i == last:
				sep() // `/` then `.*`
				if st != 0 {
					st = stSeg | stSlash
				}
			default:
				// `(?:/.+)?`: skip it, or consume a separator and at least one
				// character.
				if st&stSeg != 0 {
					st |= stSeg | stSlash
				}
				needSlash = true
			}
		case "*":
			if needSlash {
				sep()
			}
			if st != 0 {
				st = stSeg // `[^/]+`, at least one non-separator character
			}
			needSlash = true
		default:
			if needSlash {
				sep()
			}
			canEmpty, canText := segShape(seg)
			next := 0
			if canEmpty {
				next |= st
			}
			if canText && st != 0 {
				next |= stSeg
			}
			st = next
			if i == last && st&stSeg != 0 {
				st |= stSlash | stSeg // trailing `(?:/.*)?` for descendants
			}
			needSlash = true
		}
		if st == 0 {
			return false
		}
	}
	// A path ends with a segment character, never with a separator and never
	// empty.
	return st&stSeg != 0
}

// segShape reports what a literal-or-glob segment's regex can match, using the
// same escape and wildcard rules as buildPatternRegex's default arm. An empty
// segment (a `//` in the pattern, or a segment that is nothing but a dangling
// escape) matches only the empty string, which is what makes the neighbouring
// separators collide.
func segShape(seg string) (canEmpty, canText bool) {
	mandatory, optional := 0, 0
	escape := false
	for _, ch := range seg {
		switch {
		case escape:
			escape = false
			mandatory++
		case ch == '\\':
			escape = true
		case ch == '*':
			optional++ // `[^/]*`
		default:
			mandatory++ // a literal, or `?` as `[^/]`
		}
	}
	return mandatory == 0, mandatory+optional > 0
}
