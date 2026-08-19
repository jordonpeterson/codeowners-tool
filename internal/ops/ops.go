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
	// Except is the op's scope-subtraction patterns (R-26a): tracked paths
	// matching any of them are out of the op's scope. Parsed from the one
	// spelling `<scope> except <pat> [<pat> ...]`; there is no JSON field
	// carrying excepts, so an op built any other way has none.
	Except []string `json:"except,omitempty"`
	// OnExceptZeroMatch selects behavior when an except pattern matches zero
	// tracked files AND the op will write (R-28): "" (== "require") |
	// "require" | "allow". Policy object form only, like OnZeroMatch.
	OnExceptZeroMatch string `json:"on_except_zero_match,omitempty"`
	// ID is a policy-file label used in results and errors; "" from --op.
	ID string `json:"id,omitempty"`
}

// Zero-match policies (R-21).
const (
	ZeroMatchRequire = "require"
	ZeroMatchSkip    = "skip"
	ZeroMatchDeclare = "declare"
)

// Except zero-match policies (R-28). Require shares R-21's spelling — the
// default posture has one name across both knobs.
const (
	ExceptZeroMatchRequire = "require"
	ExceptZeroMatchAllow   = "allow"
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
		scope, excepts, err := parseScopeArg(args[0])
		if err != nil {
			return Op{}, err
		}
		if strings.HasPrefix(args[1], "[") {
			return Op{}, fmt.Errorf("%s takes a single owner, not a list", kind)
		}
		if !file.ValidOwnerToken(args[1]) {
			return Op{}, fmt.Errorf("invalid owner token %q", args[1])
		}
		op.Scope, op.Except, op.Owner = scope, excepts, args[1]
	case SetOwners:
		if len(args) < 2 || !strings.HasPrefix(args[1], "[") || !strings.HasSuffix(args[len(args)-1], "]") {
			return Op{}, fmt.Errorf("set_owners takes (scope, [owners]) — the list brackets are required")
		}
		scope, excepts, err := parseScopeArg(args[0])
		if err != nil {
			return Op{}, err
		}
		op.Scope, op.Except = scope, excepts
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
			// R-27.4: an except clause on a rename has to be named as such.
			// Falling through to "invalid owner token \"@a except @b\"" would
			// leave the operator hunting a typo in an owner name instead of
			// learning that the clause has no meaning here.
			if _, _, isExcept := splitExceptClause(a); isExcept {
				return Op{}, fmt.Errorf("rename_owner takes no scope, so it cannot carry an except clause (R-27)")
			}
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

// splitUnescapedWS splits s on runs of UNESCAPED spaces and tabs, keeping
// escape sequences inside their token. This is the except delimiter (R-26a):
// unescaped whitespace is otherwise illegal in a scope (checkScope), so every
// op string legal today yields one token here and parses identically. A
// strings.Fields splitter would split `/a\ except\ b/` — a directory literally
// named "a except b" — into three tokens and read a clause its author never
// wrote (adversarial-review finding).
func splitUnescapedWS(s string) []string {
	var toks []string
	var cur strings.Builder
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			cur.WriteByte(c)
			esc = false
		case c == '\\':
			// The backslash stays in the token: these are raw pattern texts,
			// and stripping the escape would turn `/docs/a\ b.md` into a
			// pattern for a different path.
			cur.WriteByte(c)
			esc = true
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	return toks
}

// splitExceptClause recognizes the one except spelling: a second
// unescaped-whitespace token that is lowercase `except`, exactly. Any other
// unescaped-whitespace form (including `EXCEPT`) reports isExcept=false and is
// left for checkScope's existing whitespace refusal — the spec promises no new
// acceptance for near-misses (R-26a).
func splitExceptClause(arg string) (scope string, excepts []string, isExcept bool) {
	toks := splitUnescapedWS(arg)
	if len(toks) < 2 || toks[1] != "except" {
		return "", nil, false
	}
	return toks[0], toks[2:], true
}

// parseScopeArg parses a scope argument, with or without an except clause,
// and runs every R-27 check that is a property of the op string alone. These
// live here rather than in the policy validator so `--op` and a policy file
// refuse identically — two validation paths for one grammar is how they
// drift apart.
func parseScopeArg(arg string) (scope string, excepts []string, err error) {
	scope, excepts, isExcept := splitExceptClause(arg)
	if !isExcept {
		if err := checkScope(arg); err != nil {
			return "", nil, err
		}
		return arg, nil, nil
	}
	if len(excepts) == 0 {
		return "", nil, fmt.Errorf("except clause has no patterns in %q: write `<scope> except <pat> [<pat> ...]` (R-26a)", arg)
	}
	if err := checkScope(scope); err != nil {
		return "", nil, err
	}
	for _, e := range excepts {
		if err := checkScope(e); err != nil {
			return "", nil, fmt.Errorf("invalid except pattern: %v", err)
		}
	}
	// R-27 comparisons are over the pattern LANGUAGE (containment both ways),
	// never string equality: `/x/**` and `/x/` are one language spelled two
	// ways, and a string check would wave through a policy that empties its
	// own scope in every repo, then fail per-repo at exit 2 a hundred times
	// where the contract demands one exit 3.
	for _, e := range excepts {
		if pattern.Contains(e, scope) {
			return "", nil, fmt.Errorf("except pattern %q covers the entire scope %q — the op is emptied by construction (R-27)", e, scope)
		}
		if !pattern.Contains(scope, e) {
			return "", nil, fmt.Errorf("except pattern %q is not provably contained in scope %q (R-27: unprovable containment is refused, so a carve can never bite a foreign subtree)", e, scope)
		}
	}
	for i := 0; i < len(excepts); i++ {
		for j := i + 1; j < len(excepts); j++ {
			if pattern.Contains(excepts[i], excepts[j]) && pattern.Contains(excepts[j], excepts[i]) {
				return "", nil, fmt.Errorf("duplicate except pattern: %q and %q match the same paths — a generator bug, not a choice (R-27)", excepts[i], excepts[j])
			}
		}
	}
	return scope, excepts, nil
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
