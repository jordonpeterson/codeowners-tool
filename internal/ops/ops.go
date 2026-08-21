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

// StaticConflict reports a pair of ops whose order changes the outcome, decided
// from the op strings alone with no tree.
//
// plan.Build already refuses order-dependent batches (R-8), but it learns the
// overlap from a concrete path in the repository, so the verdict is repo-shaped
// and `sync` reports it at exit 2. A fleet then files all 100 repos under
// `needs-human` one at a time while `check`, which opens no repo, calls the
// policy valid — for a defect that was in the policy the whole time. The
// canonical case is the one the README already warns about:
//
//	set_owners(*, [@org/everyone])        # displaces
//	add_owner(/services/api/, @org/api)   # co-owns
//
// Sound rather than complete, deliberately: only pairs where pattern.Contains
// PROVES one scope covers the other, because exit 3 halts a rollout and a false
// positive there is the expensive direction. Everything else stays with
// plan.Build's tree-based R-8, at exit 2 per repo.
func StaticConflict(list []Op) error {
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			a, b := list[i], list[j]
			if conditionalScope(a) || conditionalScope(b) {
				// This pair's order-dependence is a fact about the TREE, so it
				// belongs to exit 2 and to plan.Build, not here. See
				// conditionalScope.
				continue
			}
			aCoversB, bCoversA := scopeCoverage(a, b)
			if !aCoversB && !bCoversA {
				continue
			}
			if commutes(a, b) {
				continue
			}
			// Which op to run first comes from the CONTAINMENT, never from the
			// kinds: `add_owner(/services/api/, @x)` batched with
			// `remove_owner(*, @x)` has no set_owners in it at all, and naming
			// the op that happens to be listed first told the operator to run
			// the narrow one before the broad one — the order that silently
			// strips @x again, in an exit-3 message whose whole purpose is to
			// be acted on.
			outer, inner := a, b
			if bCoversA && !aCoversB {
				outer, inner = b, a
			}
			if aCoversB && bCoversA {
				return fmt.Errorf("ops %q and %q do not commute, and their scopes %q and %q govern exactly the same paths — so the batch is order-dependent on every repository (R-8); state one intent per scope, or run them as separate invocations",
					a.Raw, b.Raw, a.Scope, b.Scope)
			}
			// The remedy names the risk it carries. Telling an operator to run
			// the broader op alone is correct, and for a `set_owners` that run
			// REPLACES the owners of everything in scope — one reported doing
			// exactly that on a security repo and stripping two teams, having
			// read the old closing clause ("which is two exit-0 invocations")
			// as reassurance. An exit code is not a safety property.
			return fmt.Errorf("ops %q and %q do not commute, and %q provably governs every path %q does — so the batch is order-dependent on every repository that has one (R-8); run %q on its own first and the narrower op(s) in a second run%s",
				a.Raw, b.Raw, outer.Scope, inner.Scope, outer.Raw, displacementWarning(outer))
		}
	}
	return nil
}

// conditionalScope reports whether an op's on_zero_match makes its effect
// depend on the repository, which is what decides the exit code above.
//
// A pair of `require` ops earns exit 3: a repo WITH the narrower scope refuses
// on the overlap and a repo without it refuses on the zero match (R-5), so the
// policy converges nowhere and saying so at repo 0 beats saying it a hundred
// times. `skip` breaks that argument — "if this repo has Terraform" is a clean
// no-op in the rest, and refusing statically turned 100 × exit 0 into a halted
// rollout — and a `declare`d rule lands at EOF where last-match-wins settles
// the outcome, so there is no order ambiguity at all. Both go to plan.Build.
func conditionalScope(op Op) bool {
	return op.OnZeroMatch == ZeroMatchSkip || op.OnZeroMatch == ZeroMatchDeclare
}

// displacementWarning is appended to the R-8 remedy when running the broader op
// alone would displace owners rather than add to them.
func displacementWarning(outer Op) string {
	if outer.Kind != SetOwners {
		return ""
	}
	return fmt.Sprintf(" — but preview that first run with --dry-run or `plan --out`: %q REPLACES the owners of every path in scope, so anyone owning those paths today and not listed in it loses them", outer.Raw)
}

// scopeCoverage reports, for each direction, whether one op's scope provably
// covers the other's. A rename has no pattern scope at all — it derives its
// scope from current ownership — so it is never decidable here and is left to
// the tree.
//
// An except clause can dissolve coverage entirely (R-31): if some except of the
// broader op contains the narrower op's whole scope, every path the narrower op
// governs is excluded from the broader op's effective scope, so the pair shares
// no path in any repository and cannot be order-dependent. Containment of the
// FULL scope is required — an except that bites only part of it leaves the rest
// contested, and the conflict stands.
func scopeCoverage(a, b Op) (aCoversB, bCoversA bool) {
	if a.Kind == RenameOwner || b.Kind == RenameOwner {
		return false, false
	}
	aCoversB = pattern.Contains(a.Scope, b.Scope) && !exceptsContain(a.Except, b.Scope)
	bCoversA = pattern.Contains(b.Scope, a.Scope) && !exceptsContain(b.Except, a.Scope)
	return aCoversB, bCoversA
}

// exceptsContain reports whether any except pattern provably contains the whole
// of scope — the condition under which the excepting op provably governs none
// of scope's paths.
func exceptsContain(excepts []string, scope string) bool {
	for _, e := range excepts {
		if pattern.Contains(e, scope) {
			return true
		}
	}
	return false
}

// commutes reports whether two ops produce the same owners in either order, on
// every owner set they could ever meet.
//
// Closed form, because the first cut enumerated every subset of the owner names
// the two ops mention: exact, and 2^n. A `set_owners` listing 20 teams — an
// ordinary baseline — took 7 seconds per pair and a 400-op policy took five
// minutes, paid again on every repo before any was opened. Same answer in
// O(owners), exhaustive over the three transforms that reach here (rename is
// excluded above):
//
//	add(x)    ∘ add(y)     always — both orders give S ∪ {x, y}
//	add(x)    ∘ remove(y)  iff x ≠ y; otherwise one order keeps x and the other drops it
//	remove(x) ∘ remove(y)  always
//	set(L)    ∘ set(M)     iff L and M are the same set; otherwise the last one wins
//	set(L)    ∘ add(x)     iff x ∈ L — set-then-add yields L ∪ {x}, add-then-set yields L
//	set(L)    ∘ remove(x)  iff x ∉ L — set-then-remove yields L \ {x}, remove-then-set yields L
func commutes(a, b Op) bool {
	// Order the pair so each case is written once.
	if b.Kind == SetOwners && a.Kind != SetOwners {
		a, b = b, a
	}
	switch a.Kind {
	case SetOwners:
		switch b.Kind {
		case SetOwners:
			return sameOwnerSet(a.Owners, b.Owners)
		case AddOwner:
			return containsOwner(a.Owners, b.Owner)
		case RemoveOwner:
			return !containsOwner(a.Owners, b.Owner)
		}
	case AddOwner:
		if b.Kind == RemoveOwner {
			return a.Owner != b.Owner
		}
		return true // add ∘ add
	case RemoveOwner:
		if b.Kind == AddOwner {
			return a.Owner != b.Owner
		}
		return true // remove ∘ remove
	}
	// An op kind this function does not model must never be waved through as
	// commuting: the tree-based R-8 is the backstop, and reporting "these
	// commute" here would skip it.
	return false
}

// sameOwnerSet compares two owner lists as SETS: order carries no meaning to
// GitHub, and two set_owners naming the same teams in different orders are the
// same intent.
func sameOwnerSet(x, y []string) bool {
	seen := map[string]bool{}
	for _, o := range x {
		seen[o] = true
	}
	other := map[string]bool{}
	for _, o := range y {
		other[o] = true
	}
	if len(seen) != len(other) {
		return false
	}
	for o := range seen {
		if !other[o] {
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
