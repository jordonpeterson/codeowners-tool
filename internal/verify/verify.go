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
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
	"github.com/jordonpeterson/codeowners-tool/internal/resolve"
)

// Snapshot is the on-disk format produced by `codeowners-tool snapshot`.
// Ownership maps path → owners; JSON null means no rule matched (distinct
// from [] — an explicit zero-owner match, S-9).
type Snapshot struct {
	Repo      string    `json:"repo,omitempty"`
	Ref       string    `json:"ref,omitempty"`
	Path      string    `json:"codeowners_path,omitempty"`
	SHA256    string    `json:"codeowners_sha256,omitempty"`
	Ownership Ownership `json:"ownership"`
}

// Ownership maps a tracked path to its resolved owners. In memory the key is
// the path exactly as git stores it — raw bytes, which need not be valid
// UTF-8; the codec below is the only place the JSON spelling exists.
type Ownership map[string][]string

// escapeMarker prefixes the JSON key of a path whose bytes are not valid
// UTF-8. encoding/json replaces every such byte with U+FFFD, so the two
// tracked files `a\xe9.md` and `a\xff.md` were written as the SAME key
// twice: any decoder keeps one, so a path vanished from the snapshot before
// Compare ever ran and an ownership change on it was invisible to the gate.
//
// NUL is the one byte a git path can never contain, so a marker no real path
// can collide with is available. Marking the key rather than escaping in
// place is what keeps every OTHER key byte-identical to what the plain
// encoder wrote — an ordinary repository's snapshot is unchanged, including
// a path that literally spells an escape, like `a%E9.md`.
const escapeMarker = "\x00"

// EscapePath renders a path for a human — verify's own output, and the part
// of a JSON key after the marker. It is the identity on valid UTF-8, which is
// every path in an ordinary repository; elsewhere each invalid byte becomes
// %XX, and a literal % becomes %25 so the spelling stays reversible.
func EscapePath(p string) string {
	if utf8.ValidString(p) {
		return p
	}
	var b strings.Builder
	b.Grow(len(p) + 8)
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRuneInString(p[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, "%%%02X", p[i])
			i++
		case p[i] == '%':
			b.WriteString("%25")
			i++
		default:
			b.WriteString(p[i : i+size])
			i += size
		}
	}
	return b.String()
}

func unescapePath(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("truncated %%-escape in path %q", s)
		}
		v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			return "", fmt.Errorf("invalid %%-escape %q in path %q", s[i:i+3], s)
		}
		b.WriteByte(byte(v))
		i += 2
	}
	return b.String(), nil
}

func encodeKey(p string) string {
	if utf8.ValidString(p) {
		return p
	}
	return escapeMarker + EscapePath(p)
}

func decodeKey(k string) (string, error) {
	if !strings.HasPrefix(k, escapeMarker) {
		return k, nil
	}
	return unescapePath(strings.TrimPrefix(k, escapeMarker))
}

// MarshalJSON writes the escaped keys through the plain encoder, so key
// ordering and string escaping are byte for byte what they always were.
func (o Ownership) MarshalJSON() ([]byte, error) {
	if o == nil {
		return []byte("null"), nil
	}
	m := make(map[string][]string, len(o))
	for p, owners := range o {
		m[encodeKey(p)] = owners
	}
	return json.Marshal(m)
}

func (o *Ownership) UnmarshalJSON(b []byte) error {
	var m map[string][]string
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if m == nil { // explicit null; Load reports the missing map
		return nil
	}
	out := make(Ownership, len(m))
	for k, owners := range m {
		p, err := decodeKey(k)
		if err != nil {
			return err
		}
		// Two keys naming one path is the defect this encoding exists to
		// prevent; keeping either silently would lose a tracked path again.
		if _, dup := out[p]; dup {
			return fmt.Errorf("path %q appears twice in the ownership map", EscapePath(p))
		}
		out[p] = owners
	}
	*o = out
	return nil
}

// Change is one path whose resolved owners differ between snapshots.
type Change struct {
	Path   string   `json:"path"`
	Before []string `json:"owners_before"`
	After  []string `json:"owners_after"`
}

// Result of a comparison. Changed lists every path whose OWNERS differ
// between the snapshots; Violations lists the subset outside the declared
// scopes (all of them, when no scope given). Added and Removed carry the tree
// deltas — paths present in only one snapshot — which are reported but never
// violations, because INV-2 has nothing to say about them (see Compare).
type Result struct {
	Changed    []Change `json:"changed"`
	Violations []Change `json:"violations"`
	Added      []Change `json:"added"`
	Removed    []Change `json:"removed"`
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

// MismatchError reports a before/after pair that has no tracked path in
// common, so nothing in it was compared.
//
// verify is the documented CI gate, and a fleet loop that gets one filename
// wrong pairs two unrelated repositories. Every row of such a pair is a tree
// delta, which R-18 never counts as a violation, so the run reports
// "0 change(s), all within scope" on every repository it visits and the
// invariant is checked nowhere. `apply` refuses the same class of mistake at
// the other end of the pipeline.
//
// The evidence is the path sets, NOT the `repo` field: that is whatever path
// the operator passed to `snapshot`, so it differs between machines, between
// two clones of one repository, and between a CI job's main and pull-request
// checkouts. `ref`, `codeowners_path`, `codeowners_sha256` and the path set
// itself all differ legitimately in the documented two-refs-of-one-repo flow,
// and a bootstrap baseline may carry none of them. An empty intersection is
// the one signal that survives all of those: a real pair shares the tree the
// invariant is about, and one that shares nothing could only ever report ok.
type MismatchError struct {
	Before, After *Snapshot
	// The file names, when the caller knows them — the bug is a loop
	// variable naming the wrong file, so naming both is most of the fix.
	BeforeName, AfterName string
}

func (e *MismatchError) Error() string {
	b, a := e.BeforeName, e.AfterName
	if b == "" {
		b = "the before snapshot"
	}
	if a == "" {
		a = "the after snapshot"
	}
	return fmt.Sprintf("refusing: %s and %s have no tracked path in common (%d and %d path(s)), so nothing in this pair was compared: %s vs %s. "+
		"A verify pair is two snapshots of the SAME repository; when they share no path every row is a tree delta, which is never a violation (R-18), so this run could only ever report ok. "+
		"Check which two files the loop paired, and re-take the pair against one repository — nothing was verified",
		b, a, len(e.Before.Ownership), len(e.After.Ownership), describe(e.Before), describe(e.After))
}

// describe renders a snapshot's own account of where it came from. It is
// diagnostic only — see MismatchError on why it is not the evidence.
func describe(s *Snapshot) string {
	repo, ref := s.Repo, s.Ref
	if repo == "" {
		repo = "(no repo recorded)"
	}
	if ref == "" {
		ref = "(no ref recorded)"
	}
	return fmt.Sprintf("%s at %s", repo, ref)
}

// sharesAPath reports whether the two ownership maps name any path in common.
func sharesAPath(a, b Ownership) bool {
	if len(b) < len(a) {
		a, b = b, a
	}
	for p := range a {
		if _, ok := b[p]; ok {
			return true
		}
	}
	return false
}

// Compare diffs two snapshots. scopes, when non-empty, are CODEOWNERS
// patterns declaring where change is allowed; every ownership change outside
// them is a violation (INV-2 from raw data). With no scopes, ANY ownership
// change is a violation. Paths present in only one snapshot are tree deltas,
// reported separately and never violations.
// A scope that fails to compile is a hard error — silently dropping it would
// misreport which changes are in scope (found in review).
//
// "Same owners" is R-38a's identity throughout: resolve.OwnersEqual compares
// members under ops.FoldOwner, and each side is canonicalised first because a
// snapshot is a FILE, written by any version of this tool or by hand — one
// that lists `@org/team` beside `@Org/Team` names one owner, and reporting the
// path as changed against a snapshot that lists it once would fail a rollout
// over a difference nobody's access reflects.
//
// A pair sharing no path at all is refused rather than compared (see
// MismatchError).
func Compare(before, after *Snapshot, scopes []string) (*Result, error) {
	if !sharesAPath(before.Ownership, after.Ownership) {
		return nil, &MismatchError{Before: before, After: after}
	}
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
		b, a = resolve.CanonicalOwners(b), resolve.CanonicalOwners(a)
		c := Change{Path: p, Before: b, After: a}
		// A path in only one snapshot is a TREE delta, not an ownership
		// change. INV-2 preserves what a path resolved to before: an added
		// path has no before and a removed one has no after, so neither can
		// violate it. Both still surface — a tree that moved under a rollout
		// is worth seeing — but neither fails the check. Snapshots come from
		// different refs, so without this the documented CI recipe failed on
		// every pull request that added a file, CODEOWNERS untouched.
		//
		// Presence of the KEY decides this, not the value: `null` (no rule
		// matched) and `[]` (an explicit zero-owner match, S-9) are both
		// present, and moving between them stays the real change R-18 says
		// it is.
		switch {
		case !bok:
			res.Added = append(res.Added, c)
			continue
		case !aok:
			res.Removed = append(res.Removed, c)
			continue
		}
		if resolve.OwnersEqual(b, a) {
			continue
		}
		res.Changed = append(res.Changed, c)
		if len(pats) == 0 || !inScope(p) {
			res.Violations = append(res.Violations, c)
		}
	}
	return res, nil
}
