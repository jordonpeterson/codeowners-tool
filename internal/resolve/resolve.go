// Package resolve maps tracked paths to owner sets (S-1: last match wins).
package resolve

import (
	"github.com/jordonpeterson/codeowners-tool/internal/file"
)

// Resolution is the resolved ownership of one tracked path.
type Resolution struct {
	Path    string   `json:"path"`
	Matched bool     `json:"matched"`
	Owners  []string `json:"owners"` // nil if unmatched; non-nil (possibly empty, S-9) if matched
	// LineIndex is the index into File.Lines of the winning rule (-1 if none).
	LineIndex int `json:"line_index"`
}

// One resolves a single path against the file's rules: the LAST matching
// valid rule wins (S-1); invalid lines are skipped.
func One(rules []*file.Rule, path string) Resolution {
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].Pattern.Match(path) {
			owners := rules[i].Owners
			if owners == nil {
				owners = []string{}
			}
			return Resolution{Path: path, Matched: true, Owners: owners, LineIndex: rules[i].LineIndex}
		}
	}
	return Resolution{Path: path, Matched: false, Owners: nil, LineIndex: -1}
}

// All resolves every tracked path (INV-3: the tree, not the pattern set, is
// the domain of resolution).
func All(f *file.File, tree []string) map[string]Resolution {
	rules := f.Rules()
	out := make(map[string]Resolution, len(tree))
	for _, p := range tree {
		out[p] = One(rules, p)
	}
	return out
}

// OwnersEqual compares two owner sets. Order is presentation, not semantics
// (owners on a line are alternatives, not a sequence), so this is set
// equality — but nil (unmatched) and empty (explicitly zero-owned) are
// distinct states and never equal.
func OwnersEqual(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, o := range a {
		seen[o]++
	}
	for _, o := range b {
		if seen[o] == 0 {
			return false
		}
		seen[o]--
	}
	return true
}
