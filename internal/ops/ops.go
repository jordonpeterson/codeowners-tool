// Package ops parses intent-level operations. The user expresses WHAT
// resolved ownership should look like; the planner decides which lines
// change. Scope is a directory, file path, or glob (§4.1).
package ops

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
)

// Kind identifies an operation.
type Kind string

const (
	AddOwner    Kind = "add_owner"
	SetOwners   Kind = "set_owners"
	RemoveOwner Kind = "remove_owner"
	RenameOwner Kind = "rename_owner"
)

// Op is one parsed operation.
type Op struct {
	Kind     Kind     `json:"kind"`
	Scope    string   `json:"scope,omitempty"`     // pattern text; empty for rename_owner
	Owner    string   `json:"owner,omitempty"`     // add/remove target; rename old name
	NewOwner string   `json:"new_owner,omitempty"` // rename new name
	Owners   []string `json:"owners,omitempty"`    // set_owners exact set (non-nil, may be empty)
	Raw      string   `json:"raw"`

	// OnZeroMatch selects behavior when Scope matches zero tracked files:
	// "" (== "require") | "require" | "skip" | "declare". The zero value
	// preserves R-5 exactly, which is why adding this changes nothing for
	// ops built by Parse.
	OnZeroMatch string `json:"on_zero_match,omitempty"`
	// ID is a policy-file label used in results and errors; "" from --op.
	ID string `json:"id,omitempty"`
}

// Zero-match policies (R-21).
const (
	ZeroMatchRequire = "require"
	ZeroMatchSkip    = "skip"
	ZeroMatchDeclare = "declare"
)

func (o Op) String() string { return o.Raw }

// Parse parses one op of the form kind(arg1, arg2).
func Parse(s string) (Op, error) {
	s = strings.TrimSpace(s)
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return Op{}, fmt.Errorf("malformed op %q: want kind(args)", s)
	}
	kind := Kind(strings.TrimSpace(s[:open]))
	argStr := s[open+1 : len(s)-1]
	args := splitArgs(argStr)
	op := Op{Kind: kind, Raw: s}

	switch kind {
	case AddOwner, RemoveOwner:
		if len(args) != 2 {
			return Op{}, fmt.Errorf("%s takes (scope, owner), got %d args", kind, len(args))
		}
		if err := checkScope(args[0]); err != nil {
			return Op{}, err
		}
		if strings.HasPrefix(args[1], "[") {
			return Op{}, fmt.Errorf("%s takes a single owner, not a list", kind)
		}
		if !file.ValidOwnerToken(args[1]) {
			return Op{}, fmt.Errorf("invalid owner token %q", args[1])
		}
		op.Scope, op.Owner = args[0], args[1]
	case SetOwners:
		if len(args) < 2 || !strings.HasPrefix(args[1], "[") || !strings.HasSuffix(args[len(args)-1], "]") {
			return Op{}, fmt.Errorf("set_owners takes (scope, [owners]) — the list brackets are required")
		}
		if err := checkScope(args[0]); err != nil {
			return Op{}, err
		}
		op.Scope = args[0]
		listStr := strings.TrimSpace(strings.Join(args[1:], ","))
		listStr = strings.TrimSuffix(strings.TrimPrefix(listStr, "["), "]")
		op.Owners = []string{}
		for _, tok := range strings.FieldsFunc(listStr, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if !file.ValidOwnerToken(tok) {
				return Op{}, fmt.Errorf("invalid owner token %q", tok)
			}
			op.Owners = append(op.Owners, tok)
		}
	case RenameOwner:
		if len(args) != 2 {
			return Op{}, fmt.Errorf("rename_owner takes (old, new), got %d args", len(args))
		}
		for _, a := range args {
			if !file.ValidOwnerToken(a) {
				return Op{}, fmt.Errorf("invalid owner token %q", a)
			}
		}
		if args[0] == args[1] {
			return Op{}, fmt.Errorf("rename_owner old and new are identical (%s)", args[0])
		}
		op.Owner, op.NewOwner = args[0], args[1]
	default:
		return Op{}, fmt.Errorf("unknown op %q (want add_owner, set_owners, remove_owner, rename_owner)", kind)
	}
	return op, nil
}

// ParseAll parses a batch, preserving order (R-8 conflict detection is the
// planner's job — parse only rejects individually malformed ops).
func ParseAll(specs []string) ([]Op, error) {
	out := make([]Op, 0, len(specs))
	for _, s := range specs {
		op, err := Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

func checkScope(scope string) error {
	if scope == "" {
		return fmt.Errorf("empty scope")
	}
	// Unescaped whitespace cannot survive serialization: a written rule line
	// "a b @x" re-parses as pattern "a" with owner "b" — a DIFFERENT valid
	// rule, silently violating both invariants (found in review). CODEOWNERS
	// spells such patterns with escaped spaces; require the same here.
	esc := false
	for i := 0; i < len(scope); i++ {
		switch {
		case esc:
			esc = false
		case scope[i] == '\\':
			esc = true
		case scope[i] == ' ' || scope[i] == '\t':
			return fmt.Errorf("scope %q contains unescaped whitespace; write spaces as '\\ ' (as CODEOWNERS itself requires)", scope)
		}
	}
	if esc {
		return fmt.Errorf("scope %q ends with a dangling backslash — it would silently match a different path than intended", scope)
	}
	if _, err := pattern.Compile(scope); err != nil {
		return fmt.Errorf("invalid scope %q: %v", scope, err)
	}
	return nil
}

// trimArg trims surrounding whitespace WITHOUT eating escaped trailing
// whitespace: a scope like `a\ ` (pattern for a path ending in a space) must
// survive argument splitting intact — a plain TrimSpace mangled it to a
// dangling `a\` that compiles to a pattern for a DIFFERENT path
// (second-review finding).
func trimArg(s string) string {
	s = strings.TrimLeft(s, " \t")
	for len(s) > 0 {
		last := s[len(s)-1]
		if last != ' ' && last != '\t' {
			break
		}
		bs := 0
		for i := len(s) - 2; i >= 0 && s[i] == '\\'; i-- {
			bs++
		}
		if bs%2 == 1 {
			break // escaped whitespace belongs to the token
		}
		s = s[:len(s)-1]
	}
	return s
}

// splitArgs splits on top-level commas, keeping [...] groups intact enough
// for set_owners; whitespace is trimmed from each piece.
func splitArgs(s string) []string {
	var args []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				args = append(args, trimArg(s[start:i]))
				start = i + 1
			}
		}
	}
	args = append(args, trimArg(s[start:]))
	// set_owners' bracketed list arrives with commas inside brackets intact
	// because depth>0 suppressed the split; but "[@a, @b]" was suppressed, so
	// re-append pieces if the bracket group was split by TrimSpace… it wasn't.
	// Drop trailing empty args from "kind(a, )" forms.
	for len(args) > 0 && args[len(args)-1] == "" {
		args = args[:len(args)-1]
	}
	return args
}

// StaticConflict reports a pair of ops whose order changes the outcome for
// every repository that has a file in the narrower one's scope — decided from
// the op strings alone, with no tree.
//
// plan.Build already refuses order-dependent batches (R-8), but it learns the
// overlap from the repository: it needs a concrete path that both scopes
// match. That makes the verdict repo-shaped, so `sync` reports it as exit 2 —
// this repo needs a human — and a fleet run files all 100 repos under
// `needs-human` one at a time while `check`, which never opens a repo, passes
// the policy as valid. The defect is in the policy and the operator finds out
// a hundred times.
//
// The subset decidable here is the one the README already warns about:
//
//	set_owners(*, [@org/everyone])        # displaces
//	add_owner(/services/api/, @org/api)   # co-owns
//
// Run in either order these give different owners to services/api, so the
// batch has no defined meaning. Whether a particular repo HAS a
// services/api/ is not the question — a policy whose meaning depends on which
// files a repo happens to contain is exactly what exit 3 is for, and the fix
// (run the displacing op alone, then the narrower ones) is the same edit for
// every repo in the fleet.
//
// Soundness over completeness, deliberately. It reports only pairs where
// pattern.Contains PROVES one scope covers the other, because exit 3 halts a
// rollout and a false positive there is the expensive direction. Everything it
// cannot prove is left to plan.Build's tree-based R-8, which still refuses per
// repo at exit 2.
func StaticConflict(list []Op) error {
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			a, b := list[i], list[j]
			if !staticallyOverlapping(a, b) || commuteOnEveryOwnerSet(a, b) {
				continue
			}
			displacing, other := a, b
			if b.Kind == SetOwners && a.Kind != SetOwners {
				displacing, other = b, a
			}
			return fmt.Errorf("ops %q and %q do not commute, and %q provably governs every path %q does — so the batch is order-dependent on every repository that has one (R-8); run %q on its own first and the narrower op(s) in a second run, which is two exit-0 invocations",
				a.Raw, b.Raw, displacing.Scope, other.Scope, displacing.Raw)
		}
	}
	return nil
}

// staticallyOverlapping reports whether one op's scope provably covers the
// other's. A rename has no pattern scope at all — it derives its scope from
// current ownership — so it is never decidable here and is left to the tree.
func staticallyOverlapping(a, b Op) bool {
	if a.Kind == RenameOwner || b.Kind == RenameOwner {
		return false
	}
	return pattern.Contains(a.Scope, b.Scope) || pattern.Contains(b.Scope, a.Scope)
}

// commuteOnEveryOwnerSet reports whether two ops produce the same owners in
// either order, for every owner set they could meet.
//
// The owner sets that matter are generated from the identifiers the two ops
// name: an op's transform only ever tests membership of its own owners, so a
// set built from those names plus one unrelated bystander covers every
// distinction either op can make. The bystander is what catches
// set_owners displacing an owner the other op never mentions.
func commuteOnEveryOwnerSet(a, b Op) bool {
	names := map[string]bool{"@bystander": true}
	for _, o := range []Op{a, b} {
		for _, n := range append([]string{o.Owner, o.NewOwner}, o.Owners...) {
			if n != "" {
				names[n] = true
			}
		}
	}
	var universe []string
	for n := range names {
		universe = append(universe, n)
	}
	sort.Strings(universe) // subset enumeration below must not depend on map order
	for mask := 0; mask < 1<<len(universe); mask++ {
		var owners []string
		for k, n := range universe {
			if mask&(1<<k) != 0 {
				owners = append(owners, n)
			}
		}
		if !sameOwnerSet(applyTo(b, applyTo(a, owners)), applyTo(a, applyTo(b, owners))) {
			return false
		}
	}
	return true
}

// applyTo is the owner-set transform of one op, used only for the commutation
// question above.
func applyTo(op Op, owners []string) []string {
	switch op.Kind {
	case AddOwner:
		if containsOwner(owners, op.Owner) {
			return owners
		}
		return append(append([]string{}, owners...), op.Owner)
	case SetOwners:
		return append([]string{}, op.Owners...)
	case RemoveOwner:
		out := make([]string, 0, len(owners))
		for _, o := range owners {
			if o != op.Owner {
				out = append(out, o)
			}
		}
		return out
	}
	return owners
}

func sameOwnerSet(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	seen := map[string]int{}
	for _, o := range x {
		seen[o]++
	}
	for _, o := range y {
		seen[o]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func containsOwner(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
