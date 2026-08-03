// Package verify compares two ownership snapshots (R-18). It is deliberately
// independent of the planner: CI can prove "nothing outside the declared
// scope changed" from raw data, without trusting the tool that made the
// change.
package verify

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
	"github.com/jordonpeterson/codeowners-tool/internal/resolve"
)

// Snapshot is the on-disk format produced by `codeowners-tool snapshot`.
// Ownership maps path → owners; JSON null means no rule matched (distinct
// from [] — an explicit zero-owner match, S-9).
type Snapshot struct {
	Repo      string              `json:"repo,omitempty"`
	Ref       string              `json:"ref,omitempty"`
	Path      string              `json:"codeowners_path,omitempty"`
	SHA256    string              `json:"codeowners_sha256,omitempty"`
	Ownership map[string][]string `json:"ownership"`
}

// Change is one path whose resolved owners differ between snapshots.
type Change struct {
	Path   string   `json:"path"`
	Before []string `json:"owners_before"`
	After  []string `json:"owners_after"`
}

// TreePath is a path present in only one of the two snapshots — a file the
// branch added or deleted. Reported for visibility, never a violation: see
// Compare.
type TreePath struct {
	Path   string   `json:"path"`
	Owners []string `json:"owners"`
}

// Result of a comparison. Changed lists every ownership difference among
// paths common to both snapshots; Violations lists the subset outside the
// declared scopes (all of them, when no scope given). Added and Removed
// record tree differences and never affect OK.
type Result struct {
	Changed    []Change   `json:"changed"`
	Violations []Change   `json:"violations"`
	Added      []TreePath `json:"added,omitempty"`
	Removed    []TreePath `json:"removed,omitempty"`
}

// OK reports whether the invariant holds: no out-of-scope change.
func (r *Result) OK() bool { return len(r.Violations) == 0 }

// Load reads a snapshot file.
func Load(path string) (*Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse snapshot %s: %v", path, err)
	}
	if s.Ownership == nil {
		return nil, fmt.Errorf("snapshot %s has no ownership map", path)
	}
	return &s, nil
}

// Compare diffs two snapshots. scopes, when non-empty, are CODEOWNERS
// patterns declaring where change is allowed; every change outside them is a
// violation (INV-2 from raw data). With no scopes, ANY change is a violation.
// A scope that fails to compile is a hard error — silently dropping it would
// misreport which changes are in scope (found in review).
//
// Only paths present in BOTH snapshots can violate the invariant. INV-2 says
// every out-of-scope path resolves to what it did before; a path the branch
// added has no "before" to preserve, and one it deleted has no "after". The
// two snapshots normally come from different refs, so treating a tree delta
// as a violation failed every real PR that touched a file outside the
// declared scope, with CODEOWNERS byte-identical. Such paths are reported in
// Added/Removed instead — surfaced, never fatal.
//
// This does not weaken the gate: a CODEOWNERS edit that reassigns a subtree
// still shows up on that subtree's pre-existing files. Only a scope whose
// every file is new to the branch goes unchecked, and there the invariant has
// nothing to say.
func Compare(before, after *Snapshot, scopes []string) (*Result, error) {
	var pats []*pattern.Pattern
	for _, s := range scopes {
		p, err := pattern.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("invalid --scope %q: %v", s, err)
		}
		pats = append(pats, p)
	}
	inScope := func(path string) bool {
		for _, p := range pats {
			if p.Match(path) {
				return true
			}
		}
		return false
	}

	paths := map[string]bool{}
	for p := range before.Ownership {
		paths[p] = true
	}
	for p := range after.Ownership {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	res := &Result{}
	for _, p := range sorted {
		b, bok := before.Ownership[p]
		a, aok := after.Ownership[p]
		switch {
		case !bok:
			res.Added = append(res.Added, TreePath{Path: p, Owners: a})
		case !aok:
			res.Removed = append(res.Removed, TreePath{Path: p, Owners: b})
		case resolve.OwnersEqual(b, a):
			// unchanged
		default:
			c := Change{Path: p, Before: b, After: a}
			res.Changed = append(res.Changed, c)
			if len(pats) == 0 || !inScope(p) {
				res.Violations = append(res.Violations, c)
			}
		}
	}
	return res, nil
}
