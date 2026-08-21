// Package resolve maps tracked paths to owner sets (S-1: last match wins).
package resolve

import (
	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
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
//
// The winning rule's owners are CANONICALISED before they leave: each owner
// once, under ops.FoldOwner, first spelling kept (R-38a/R-38f). A resolution
// is "who owns this path", and a hand-written `/x/ @org/team @Org/Team` names
// one owner twice — reporting it twice would make the two ways this codebase
// compares owner collections disagree, since ops.sameOwnerSet compares as a
// SET and OwnersEqual as a MULTISET. Canonicalising here means every owner
// collection either of them can ever see is already free of folded
// duplicates, so they cannot disagree, and OwnersEqual keeps its multiplicity
// check — which is what lets plan.Build's gate still catch a planner that
// wrote an owner twice into its own model.
//
// The RULE's own slice is never touched: spelling is preserved on the line
// (R-38b), and `sync` does not collapse a pre-existing duplicate (R-38f).
func One(rules []*file.Rule, path string) Resolution {
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].Pattern.Match(path) {
			owners := CanonicalOwners(rules[i].Owners)
			if owners == nil {
				owners = []string{}
			}
			return Resolution{Path: path, Matched: true, Owners: owners, LineIndex: rules[i].LineIndex}
		}
	}
	return Resolution{Path: path, Matched: false, Owners: nil, LineIndex: -1}
}

// CanonicalOwners drops repeated owners under the identity, keeping the first
// spelling. It returns the input slice untouched when there is nothing to drop
// — the common case, and the one where copying every rule's owners for every
// tracked path would be pure allocation.
//
// Exported for owner collections that did NOT come from One: a snapshot on
// disk was written by whatever version of the tool (or whichever hand) made
// it, and comparing one that lists an owner twice against one that lists it
// once must not read as an ownership change.
func CanonicalOwners(owners []string) []string {
	dup := false
	seen := make(map[string]bool, len(owners))
	for _, o := range owners {
		f := ops.FoldOwner(o)
		if seen[f] {
			dup = true
			break
		}
		seen[f] = true
	}
	if !dup {
		return owners
	}
	out := make([]string, 0, len(owners))
	kept := make(map[string]bool, len(owners))
	for _, o := range owners {
		f := ops.FoldOwner(o)
		if kept[f] {
			continue
		}
		kept[f] = true
		out = append(out, o)
	}
	return out
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
//
// Members compare under ops.FoldOwner (R-38a): `@org/team` and `@Org/Team` are
// one owner, so a plan whose model spells a handle one way and whose file
// spells it the other has changed nobody's access and must not be refused as
// an invariant violation. Only the SPELLING is indifferent here — multiplicity
// is not, so this stays a multiset comparison and still catches a model that
// carries an owner twice where the file carries it once.
func OwnersEqual(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, o := range a {
		seen[ops.FoldOwner(o)]++
	}
	for _, o := range b {
		f := ops.FoldOwner(o)
		if seen[f] == 0 {
			return false
		}
		seen[f]--
	}
	return true
}
