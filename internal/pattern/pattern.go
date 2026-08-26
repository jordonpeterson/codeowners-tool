// Package pattern implements GitHub CODEOWNERS pattern matching.
//
// CODEOWNERS patterns follow "most of the same rules used in gitignore files"
// (GitHub docs), with three documented exceptions: no `!` negation, no `[ ]`
// character ranges, and no `\#` escaping of a leading hash. Square brackets
// are literal characters. Matching is case-sensitive (S-6).
//
// The matching logic is ported from hmarr/codeowners (match.go), MIT License,
// Copyright (c) 2020 Harry Marr — the actively maintained reference
// implementation whose semantics are differentially tested against GitHub's
// observed behavior. The vendored corpus in testdata/patterns.json comes from
// the same project. Local divergences from the port, all deliberate:
//   - Compile rejects patterns starting with `!` (GitHub: negation "doesn't
//     work"; a mutation tool must never accept or emit one).
//   - Compile rejects patterns starting with `\#` (S-2/S-6: GitHub honors no
//     `\#` escape — a line starting with `#` is always a comment, so the rule
//     would be dead there; same standard as `!`). Only the LEADING position is
//     special-cased: a mid-pattern `#` needs no escape at all, so `\#` after
//     the first character keeps the oracle's literal-`#` meaning.
//   - Compile rejects empty patterns.
package pattern

import (
	"fmt"
	"regexp"
	"strings"
)

// Pattern is a compiled CODEOWNERS pattern.
type Pattern struct {
	raw                 string
	regex               *regexp.Regexp
	leftAnchoredLiteral bool
}

// String returns the original pattern text.
func (p *Pattern) String() string { return p.raw }

// Compile parses a CODEOWNERS pattern.
func Compile(patternStr string) (*Pattern, error) {
	if patternStr == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	if patternStr[0] == '!' {
		return nil, fmt.Errorf("negation (%q) is not supported in CODEOWNERS", patternStr)
	}
	if strings.HasPrefix(patternStr, `\#`) {
		return nil, fmt.Errorf(`pattern %q needs a \# escape GitHub does not honor (S-2/S-6): a line starting with '#' is always a comment there`, patternStr)
	}
	if patternStr[0] == '#' {
		return nil, fmt.Errorf(`pattern %q starts with '#': the line written for it reads back as a comment, not as a rule for that pattern (S-2/S-6) — the same standard as `+"`!`"+` and `+"`\\#`"+`, the rule would be dead there and a mutation tool must never accept or emit one`, patternStr)
	}
	p := &Pattern{raw: patternStr}
	if !strings.ContainsAny(patternStr, "*?\\") && patternStr[0] == '/' {
		p.leftAnchoredLiteral = true
		return p, nil
	}
	re, err := buildPatternRegex(patternStr)
	if err != nil {
		return nil, err
	}
	p.regex = re
	return p, nil
}

// Match reports whether the repo-relative path (forward slashes, no leading
// slash) matches the pattern. Paths name tracked FILES; a pattern matching a
// directory matches the files beneath it per the rules encoded here.
func (p *Pattern) Match(testPath string) bool {
	if p.leftAnchoredLiteral {
		prefix := p.raw
		if prefix[0] == '/' {
			prefix = prefix[1:]
		}
		// Pattern ending in slash: simple prefix match (directory contents).
		if prefix != "" && prefix[len(prefix)-1] == '/' {
			return strings.HasPrefix(testPath, prefix)
		}
		if len(testPath) == len(prefix) {
			return testPath == prefix
		}
		if len(testPath) > len(prefix) && testPath[len(prefix)] == '/' {
			return testPath[:len(prefix)] == prefix
		}
		return false
	}
	return p.regex.MatchString(testPath)
}

// normalizeSegs splits a pattern into the segment list buildPatternRegex
// compiles, applying gitignore's anchoring rules. Contains depends on this
// being the SAME normalization the matcher uses — a divergence here would make
// containment reason about a different language than Match implements.
func normalizeSegs(pattern string) []string {
	segs := strings.Split(pattern, "/")

	if segs[0] == "" {
		// Leading slash: match is relative to root.
		segs = segs[1:]
	} else if len(segs) == 1 || (len(segs) == 2 && segs[1] == "") {
		// Single-segment pattern with no leading slash matches at any depth
		// (equivalent to a leading **/). A trailing slash does NOT anchor it:
		// "docs/" is one segment and matches "src/docs/notes.md".
		if segs[0] != "**" {
			segs = append([]string{"**"}, segs...)
		}
	}

	if len(segs) > 1 && segs[len(segs)-1] == "" {
		// Trailing slash is equivalent to "/**".
		segs[len(segs)-1] = "**"
	}
	return segs
}

// buildPatternRegex compiles a gitignore-style CODEOWNERS pattern to a regex.
// Ported from hmarr/codeowners.
func buildPatternRegex(pattern string) (*regexp.Regexp, error) {
	switch {
	case strings.Contains(pattern, "***"):
		return nil, fmt.Errorf("pattern cannot contain three consecutive asterisks")
	case pattern == "/":
		// "/" doesn't match anything
		return regexp.Compile(`\A\z`)
	}

	segs := normalizeSegs(pattern)

	const sep = "/"
	lastSegIndex := len(segs) - 1
	needSlash := false
	var re strings.Builder
	re.WriteString(`\A`)
	for i, seg := range segs {
		switch seg {
		case "**":
			switch {
			case i == 0 && i == lastSegIndex:
				re.WriteString(`.+`)
			case i == 0:
				re.WriteString(`(?:.+` + sep + `)?`)
				needSlash = false
			case i == lastSegIndex:
				re.WriteString(sep + `.*`)
			default:
				re.WriteString(`(?:` + sep + `.+)?`)
				needSlash = true
			}
		case "*":
			if needSlash {
				re.WriteString(sep)
			}
			re.WriteString(`[^` + sep + `]+`)
			needSlash = true
		default:
			if needSlash {
				re.WriteString(sep)
			}
			escape := false
			for _, ch := range seg {
				if escape {
					escape = false
					re.WriteString(regexp.QuoteMeta(string(ch)))
					continue
				}
				// CODEOWNERS has no character classes: [ and ] are literals.
				switch ch {
				case '\\':
					escape = true
				case '*':
					re.WriteString(`[^` + sep + `]*`)
				case '?':
					re.WriteString(`[^` + sep + `]`)
				case '[', ']':
					re.WriteString(`\` + string(ch))
				default:
					re.WriteString(regexp.QuoteMeta(string(ch)))
				}
			}
			if i == lastSegIndex {
				// No trailing slash (that'd hit the '**' case): also match
				// descendant paths.
				re.WriteString(`(?:` + sep + `.*)?`)
			}
			needSlash = true
		}
	}
	re.WriteString(`\z`)
	return regexp.Compile(re.String())
}
