// Package ops parses intent-level operations. The user expresses WHAT
// resolved ownership should look like; the planner decides which lines
// change. Scope is a directory, file path, or glob (§4.1).
package ops

import (
	"fmt"
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
	// OnUnowned selects what happens to in-scope paths that nobody owns yet:
	// "" (== "own") | "own" | "skip". The zero value preserves the original
	// behavior exactly, which is why adding this changes nothing for ops
	// built by Parse (R-25).
	OnUnowned string `json:"on_unowned,omitempty"`
	// ID is a policy-file label used in results and errors; "" from --op.
	ID string `json:"id,omitempty"`
}

// Zero-match policies (R-21).
const (
	ZeroMatchRequire = "require"
	ZeroMatchSkip    = "skip"
	ZeroMatchDeclare = "declare"
)

// Unowned-path policies (R-25). "own" is what the tool has always done: an
// in-scope path that no rule matches is given the op's owner, which makes that
// owner its SOLE owner. "skip" narrows the op to paths that already have an
// owner, so the op can only ever add a co-owner and never manufacture
// ownership where there was none.
const (
	UnownedOwn  = "own"
	UnownedSkip = "skip"
)

// CanCarryOnUnowned reports whether on_unowned is meaningful on this kind of
// op. It is add_owner and nothing else, and the reason is one table so the
// policy file and the --on-unowned flag can never drift apart on it (R-25).
//
//   - add_owner narrows cleanly, because its synthesis already works rule by
//     rule: it derives a narrowing pattern from the intersection of the scope
//     with each rule that currently owns something in it. A path nobody owns
//     is under none of those rules, so dropping it from the scope drops it
//     from the synthesis too, and the pattern written is exactly as narrow as
//     the paths it is allowed to touch.
//   - set_owners cannot. It writes ONE rule spelled with the op's scope
//     pattern verbatim, and "the .gradle files that somebody already owns" is
//     a predicate about ownership, not a path shape — there is generally no
//     pattern that expresses it. The rule would recapture the very paths the
//     narrowing removed, INV-2 would catch it, and the op would refuse. That
//     refusal is per-repo: it would pass on repos where every in-scope path is
//     already owned and fail on the ones where some are not, i.e. fail exactly
//     where the field is doing something. A flag that works until it matters
//     is worse than one that says no up front.
//   - remove_owner can never give a path an owner, and rename_owner draws its
//     scope from current ownership, so for both the field asks for something
//     that already holds.
func CanCarryOnUnowned(k Kind) bool { return k == AddOwner }

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
