package pattern

import (
	"regexp"
	"strings"
)

// Contains reports whether every path matching `inner` also matches `outer` —
// containment of the pattern LANGUAGES, not of their match sets over some
// snapshot of a file tree. Callers use it to decide whether editing a rule in
// place can affect anything outside an operation's scope, and a CODEOWNERS rule
// governs files that do not exist yet, so a tree-based answer is an accident of
// what happens to be committed today.
//
// Contains is SOUND but deliberately incomplete: it never reports true for a
// pair where containment fails, and answers false whenever it cannot prove the
// containment. Callers must treat false as "unknown", not as "disjoint".
//
// Paths are repo-relative FILE paths (see Match), so a pattern ending in `/**`
// is taken to require at least one further segment.
func Contains(outer, inner string) bool {
	if outer == inner {
		return true
	}
	// "/" matches nothing: the empty language is contained in everything, and
	// contains only itself.
	if inner == "/" {
		return true
	}
	if outer == "/" {
		return false
	}
	ot, ok := tokenize(outer)
	if !ok {
		return false
	}
	it, ok := tokenize(inner)
	if !ok {
		return false
	}
	if universal(ot) {
		return true
	}
	memo := make(map[[2]int]bool, len(ot)*len(it))
	return covers(ot, it, 0, 0, memo)
}

// universal reports whether a token sequence matches every non-empty path.
// "*" and "**" both normalize to a run of tokMany around a single tokAny1,
// which the segment-by-segment walk cannot recognize on its own: the walk
// pairs tokens positionally, so it cannot see that one tokAny1 plus a tokMany
// already covers "any path with at least one segment".
func universal(toks []token) bool {
	ones, manys := 0, 0
	for _, t := range toks {
		switch t.kind {
		case tokSeg:
			return false
		case tokAny1:
			ones++
		default:
			manys++
		}
	}
	return ones == 1 && manys > 0
}

// allMany reports whether every token is a tokMany, so the run matches any
// number of segments including none.
func allMany(toks []token) bool {
	for _, t := range toks {
		if t.kind != tokMany {
			return false
		}
	}
	return true
}

// producesSegment reports whether a token run must yield at least one segment.
func producesSegment(toks []token) bool {
	for _, t := range toks {
		if t.kind != tokMany {
			return true
		}
	}
	return false
}

// Token kinds. A pattern is modeled as a sequence of tokens over path segments.
const (
	tokSeg  = iota // exactly one segment, matching a single-segment glob
	tokAny1        // exactly one segment, unconstrained ("*")
	tokMany        // zero or more segments ("**", or an implicit descendant tail)
)

type token struct {
	kind int
	glob string // tokSeg only
}

// tokenize converts a pattern into its token sequence, mirroring the regex
// buildPatternRegex emits for each normalized segment. It reports false for any
// pattern it cannot model faithfully; callers then answer "unproven", which is
// always safe.
func tokenize(pat string) ([]token, bool) {
	if pat == "" || pat[0] == '!' || strings.Contains(pat, "***") {
		return nil, false
	}
	segs := normalizeSegs(pat)
	last := len(segs) - 1
	// Refuse patterns with two adjacent "**" segments. buildPatternRegex tracks
	// whether the separator has already been consumed (its needSlash flag), and
	// the "**" arms do not honour it: a leading "**" compiles to "(?:.+/)?" and
	// clears needSlash, then the next "**" emits the separator again anyway. So
	// "**/" — segments ["**", "**"] — compiles to `\A(?:.+/)?/.*\z`, which no
	// repo-relative path can match. Modeling those segments as tokens loses that
	// bookkeeping entirely and reads the pattern as universal, which is UNSOUND:
	// Contains("**/", "*") would be true while "a.go" matches "*" and not "**/".
	// These spellings match nothing (or, like "*/**/**", are simply degenerate),
	// so declining to reason about them costs a refusal and nothing real.
	for i := 1; i <= last; i++ {
		if segs[i] == "**" && segs[i-1] == "**" {
			return nil, false
		}
	}
	var out []token
	for i, seg := range segs {
		switch seg {
		case "**":
			// A leading or middle "**" compiles to an optional run of
			// segments. A TRAILING "**" compiles to "/.*", which requires the
			// separator — for file paths that means at least one more segment.
			if i == last {
				out = append(out, token{kind: tokAny1})
			}
			out = append(out, token{kind: tokMany})
		case "*":
			out = append(out, token{kind: tokAny1})
		default:
			out = append(out, token{kind: tokSeg, glob: seg})
		}
	}
	// A final literal-or-glob segment also matches descendants ("(?:/.*)?").
	// A final bare "*" does NOT: buildPatternRegex emits it from the `case "*"`
	// arm, which never appends that tail — so "/x/*" matches exactly one level
	// while "/x/foo" also matches everything beneath "x/foo".
	if segs[last] != "**" && segs[last] != "*" {
		out = append(out, token{kind: tokMany})
	}
	return out, true
}

// covers reports whether outer[oi:] matches every path outer[ii:] can produce.
func covers(outer, inner []token, oi, ii int, memo map[[2]int]bool) bool {
	key := [2]int{oi, ii}
	if v, seen := memo[key]; seen {
		return v
	}
	memo[key] = false // conservative on cycles

	r := func() bool {
		if oi == len(outer) {
			// Outer is spent, so it can absorb nothing further. Any leftover
			// inner token — including a tokMany, which is nullable but need not
			// be null — can produce a segment outer cannot cover.
			return ii == len(inner)
		}
		// A universal remainder covers whatever is left without pairing tokens
		// off one by one. "/x/" ends in [tokAny1, tokMany] — "one or more
		// segments, anything" — which the positional walk alone cannot match
		// against an inner "**" that stands for a variable number of segments.
		if allMany(outer[oi:]) {
			return true
		}
		if universal(outer[oi:]) && producesSegment(inner[ii:]) {
			return true
		}
		ot := outer[oi]
		if ot.kind == tokMany {
			// Absorb zero inner segments, or swallow one more inner token.
			if covers(outer, inner, oi+1, ii, memo) {
				return true
			}
			return ii < len(inner) && covers(outer, inner, oi, ii+1, memo)
		}
		// Outer needs exactly one segment here, so inner must supply exactly
		// one. A tokMany could supply zero or many; refuse to guess.
		if ii >= len(inner) {
			return false
		}
		switch it := inner[ii]; it.kind {
		case tokMany:
			return false
		case tokAny1:
			// Only an unconstrained outer segment covers an arbitrary one.
			return ot.kind == tokAny1 && covers(outer, inner, oi+1, ii+1, memo)
		default: // tokSeg
			if ot.kind != tokAny1 && !segContains(ot.glob, it.glob) {
				return false
			}
			return covers(outer, inner, oi+1, ii+1, memo)
		}
	}()
	memo[key] = r
	return r
}

// segContains reports whether every segment matching glob `inner` also matches
// glob `outer`. Proven only when `inner` is a literal (the case that matters:
// does "build.gradle" fall under "*.gradle"); otherwise only identical globs
// are accepted.
func segContains(outer, inner string) bool {
	if outer == inner {
		return true
	}
	lit, ok := literalSeg(inner)
	if !ok {
		return false
	}
	re, err := segRegex(outer)
	if err != nil {
		return false
	}
	return re.MatchString(lit)
}

// literalSeg unescapes a segment glob that contains no wildcard, reporting the
// exact string it matches.
func literalSeg(seg string) (string, bool) {
	var b strings.Builder
	escape := false
	for _, ch := range seg {
		if escape {
			escape = false
			b.WriteRune(ch)
			continue
		}
		switch ch {
		case '\\':
			escape = true
		case '*', '?':
			return "", false
		default:
			b.WriteRune(ch)
		}
	}
	if escape {
		return "", false
	}
	return b.String(), true
}

// segRegex compiles a single segment's glob, using the same escape and
// wildcard rules as buildPatternRegex's default arm (no character classes:
// "[" and "]" are literals).
func segRegex(seg string) (*regexp.Regexp, error) {
	var re strings.Builder
	re.WriteString(`\A`)
	escape := false
	for _, ch := range seg {
		if escape {
			escape = false
			re.WriteString(regexp.QuoteMeta(string(ch)))
			continue
		}
		switch ch {
		case '\\':
			escape = true
		case '*':
			re.WriteString(`[^/]*`)
		case '?':
			re.WriteString(`[^/]`)
		case '[', ']':
			re.WriteString(`\` + string(ch))
		default:
			re.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	re.WriteString(`\z`)
	return regexp.Compile(re.String())
}
