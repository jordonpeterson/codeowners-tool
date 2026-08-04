package policy

import "strings"

// Did-you-mean over the known field and enum sets.
//
// The motivating error is `on_zero_mtach` — two transposed letters from the real
// field, and under a permissive decoder it applies the DEFAULT zero-match policy
// to 100 repos while the file in git says "skip". Naming the offending key is
// half the fix; pointing at the one that was meant is the other half, and it is
// twenty lines and zero dependencies.

// suggest returns the closest known name to bad, or "" when nothing is close
// enough to be worth guessing. A wrong guess is worse than none: it sends the
// operator to edit a field they never wrote.
func suggest(bad string, known []string) string {
	// Case is checked first and exactly. `SKIP` is not a near miss of `skip`, it
	// is the same word in the wrong case, and saying "did you mean skip?" reads
	// as nonsense unless the case difference is what we actually spotted.
	for _, k := range known {
		if k != bad && strings.EqualFold(k, bad) {
			return k
		}
	}
	best, bestDist := "", 0
	for _, k := range known {
		d := levenshtein(bad, k)
		if d == 0 {
			continue
		}
		// Scale the tolerance with the candidate's length — one edit in `ops` is
		// a different kind of evidence than two edits in `on_zero_match` — with a
		// floor of 2 so short names still get their obvious typo caught.
		limit := max((len(k)+2)/3, 2)
		if d <= limit && (best == "" || d < bestDist) {
			best, bestDist = k, d
		}
	}
	return best
}

// levenshtein is the standard two-row edit distance.
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

// hint renders a did-you-mean clause, or "" when there is nothing to suggest.
func hint(bad string, known []string) string {
	if s := suggest(bad, known); s != "" {
		return " (did you mean " + quote(s) + "?)"
	}
	return ""
}

func quote(s string) string { return `"` + s + `"` }

// list renders a legal set the way the message should read it back: quoted, in
// the order the docs use, never alphabetized into a different meaning.
func list(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = quote(it)
	}
	return strings.Join(parts, ", ")
}
