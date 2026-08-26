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
	Kind  Kind   `json:"kind"`
	Scope string `json:"scope,omitempty"` // pattern text; empty for rename_owner
	// Owner is rename_owner's OLD name and nothing else. add/remove/set all
	// carry their targets in Owners, so no site can read a stale single value
	// off an op that names several owners (R-33) — the field is simply not
	// set for them.
	Owner    string `json:"owner,omitempty"`
	NewOwner string `json:"new_owner,omitempty"` // rename new name
	// Owners is the target set for add_owner, remove_owner and set_owners:
	// non-nil for all three, length >= 1 for add/remove (R-33d), possibly
	// empty for set_owners, which is how "nobody owns this" is spelled.
	Owners []string `json:"owners,omitempty"`
	Raw    string   `json:"raw"`

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
	// Every verb takes at least two arguments, so a short count is where a
	// swallowed separator shows up — and blaming the arity there sends the
	// operator counting arguments they wrote two of.
	if len(args) < 2 {
		if bad, ok := swallowedSeparator(argStr); ok {
			return Op{}, fmt.Errorf("argument %q ends with a dangling backslash, and that backslash escaped the comma after it: `\\,` is a literal comma INSIDE a path, so the separator and everything past it became part of this one argument", bad)
		}
	}
	op := Op{Kind: kind, Raw: s}

	switch kind {
	case AddOwner, RemoveOwner:
		// A bare owner is one arg; a bracketed list arrives split on its own
		// commas, so anything from args[1] on is part of it.
		if len(args) < 2 {
			return Op{}, fmt.Errorf("%s takes (scope, owner) or (scope, [owners]), got %d args", kind, len(args))
		}
		scope, excepts, err := parseScopeArg(args[0])
		if err != nil {
			return Op{}, err
		}
		op.Scope, op.Except = scope, excepts
		if strings.HasPrefix(args[1], "[") {
			owners, err := parseOwnerList(strings.Join(args[1:], ","), kind)
			if err != nil {
				return Op{}, err
			}
			op.Owners = owners
			break
		}
		if len(args) != 2 {
			// An argument that cannot be an owner is named as such, before the
			// arity is blamed: `add_owner(/a,b/, @org/x)` has three arguments
			// because a comma inside a PATH split one of them, and "got 3 args"
			// sends the operator counting arguments they wrote two of.
			if bad, ok := nonOwnerArg(args[1:]); ok {
				return Op{}, fmt.Errorf("invalid owner token %q%s", bad, commaSplitHint)
			}
			return Op{}, fmt.Errorf("%s takes (scope, owner) or (scope, [owners]), got %d args", kind, len(args))
		}
		if !file.ValidOwnerToken(args[1]) {
			return Op{}, fmt.Errorf("invalid owner token %q%s", args[1], commaSplitHint)
		}
		op.Owners = []string{args[1]}
	case SetOwners:
		if len(args) < 2 || !strings.HasPrefix(args[1], "[") || !strings.HasSuffix(args[len(args)-1], "]") {
			hint := ""
			if len(args) > 1 && !strings.HasPrefix(args[1], "[") && !file.ValidOwnerToken(args[1]) {
				hint = commaSplitHint
			}
			return Op{}, fmt.Errorf("set_owners takes (scope, [owners]) — the list brackets are required%s", hint)
		}
		scope, excepts, err := parseScopeArg(args[0])
		if err != nil {
			return Op{}, err
		}
		op.Scope, op.Except = scope, excepts
		// One grammar, every verb that names owners (R-33): a second copy
		// here is how set_owners came to accept `[@org/a, @org/a]` a year
		// after add_owner stopped, write it, and leave `verify` reporting a
		// rollback-worthy invariant violation over a semantic no-op.
		owners, err := parseOwnerList(strings.Join(args[1:], ","), kind)
		if err != nil {
			return Op{}, err
		}
		op.Owners = owners
	case RenameOwner:
		if len(args) != 2 {
			return Op{}, fmt.Errorf("rename_owner takes (old, new), got %d args", len(args))
		}
		for _, a := range args {
			// R-33f: a list on a rename has to be named as such. Falling
			// through to `invalid owner token "[@b]"` sends the operator
			// hunting a typo instead of learning the verb takes no list.
			if strings.HasPrefix(a, "[") || strings.HasSuffix(a, "]") {
				return Op{}, fmt.Errorf("rename_owner takes no list: it renames one owner to one owner (R-33f)")
			}
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

// parseOwnerList parses a bracketed owner list for every verb that names
// owners (R-33): add_owner, remove_owner and set_owners all reach it, so one
// grammar covers them and the claim is checked by the compiler rather than by
// memory. It tolerates whitespace and a trailing comma, and adds the two
// refusals a list makes possible and a single owner cannot: an empty list and
// a repeated owner. Both are facts about the text alone, so both are exit 3 on
// every repository rather than a per-repo refusal.
//
// The empty list is the one thing the verbs disagree about: set_owners(scope,
// []) is how "nobody owns this" is spelled, while an empty add or remove
// states no intent at all (R-33d).
func parseOwnerList(listStr string, kind Kind) ([]string, error) {
	listStr = strings.TrimSpace(listStr)
	if !strings.HasPrefix(listStr, "[") || !strings.HasSuffix(listStr, "]") {
		return nil, fmt.Errorf("%s owner list is not closed: want [@owner, @owner]", kind)
	}
	inner := listStr[1 : len(listStr)-1]
	if strings.ContainsAny(inner, "[]") {
		// Inherit set_owners' diagnosis: a stray bracket is a bad token, and
		// the operator should read one sentence for that defect, not two.
		return nil, fmt.Errorf("invalid owner token in %s owner list: brackets do not nest, want [@owner, @owner]", kind)
	}
	// Non-nil even when empty: set_owners(scope, []) means "nobody owns this",
	// which is a stated intent, and a nil slice is how an op that names no
	// owners at all would look.
	owners := []string{}
	seen := make(map[string]string)
	for _, tok := range strings.FieldsFunc(inner, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		if !file.ValidOwnerToken(tok) {
			return nil, fmt.Errorf("invalid owner token %q", tok)
		}
		if prev, dup := seen[FoldOwner(tok)]; dup {
			if prev == tok {
				return nil, fmt.Errorf("duplicate owner %q in one %s list", tok, kind)
			}
			// Spelled differently, so both spellings are shown: an operator
			// told only "duplicate owner @org/team" cannot see which of the
			// two entries they wrote is the other one.
			return nil, fmt.Errorf("duplicate owner: %q and %q are one owner in the same %s list — @handles are case-insensitive on GitHub", prev, tok, kind)
		}
		seen[FoldOwner(tok)] = tok
		owners = append(owners, tok)
	}
	if len(owners) == 0 && kind != SetOwners {
		// set_owners(scope, []) is legal and means "nobody owns this"; an
		// empty add or remove states no intent at all, so it is a defect
		// rather than a no-op that silently ships to a hundred repositories.
		return nil, fmt.Errorf("%s has an empty owner list: name at least one owner, or delete the op", kind)
	}
	return owners, nil
}

// FoldOwner is THE identity under which two owner tokens are the same owner
// (R-38a). @handles fold to lowercase (GitHub does); an email is left alone,
// because the local part of an address is not ours to case-fold.
//
// Every comparison of two owners in this repository asks this function — add,
// remove, rename's old name, set_owners, commutation and resolution — with no
// site free to use its own. Byte equality is what let
// `remove_owner(/x/, @ORG/TEAM)` report "unchanged" against a file holding
// @org/team: a fleet run of "revoke the departed team" reported converged on
// every repository that capitalised the handle.
//
// Folding governs MATCHING only, never output: the spelling on the line is
// preserved (R-38b) and only `rename_owner` writes a new one (R-38d).
//
// internal/lint had a byte-identical unexported copy, which is where this
// identity was first written down; it now calls this one. Two copies of an
// identity is how they drift.
func FoldOwner(o string) string {
	if strings.HasPrefix(o, "@") {
		return strings.ToLower(o)
	}
	return o
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

// commaSplitHint names the one thing that can put text where an owner belongs:
// an unescaped comma inside a scope. It is phrased as a condition because the
// grammar cannot know which the comma was — `add_owner(/x/, nonsense)` reaches
// the same refusal with no comma of its own to blame — but naming the escape is
// what makes a path holding a comma reachable at all.
const commaSplitHint = `; if that text belongs to the scope, an unescaped comma separated it — a comma inside a path is written "\," (as a space is written "\ ")`

// nonOwnerArg returns the first argument that cannot be an owner token.
func nonOwnerArg(args []string) (string, bool) {
	for _, a := range args {
		if !file.ValidOwnerToken(a) {
			return a, true
		}
	}
	return "", false
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
	// A pattern whose language is EMPTY is dead in every repository, not just
	// in this one. R-5 refuses "creating a dead rule" from the tree, at exit 2,
	// and on_zero_match=declare is allowed to overrule it — the guarantee it
	// trades down to is "when someone later adds a matching file, this rule
	// takes it" (GUARANTEES.md). For `**/` or `/` no such file can exist in any
	// repo, so that guarantee is not weaker, it is void: the line would be
	// written, reported `proven: structural`, and own nothing forever. Refuse
	// it here, where the verdict is a fact about the op string alone and so
	// belongs to exit 3 — `check`, which opens no repository, catches it before
	// repo #1.
	if pattern.NeverMatches(scope) {
		return fmt.Errorf("scope %q can never match any path, in any repository — no repo-relative path can match it, so the rule would be dead everywhere and on_zero_match=declare would only write it down (R-5)", scope)
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
//
// A BACKSLASHED byte is never structural — `\,` is a literal comma, `\[` a
// literal bracket — which is the same escape the scope grammar already uses for
// a space (checkScope, splitUnescapedWS). Without it a tracked path holding a
// comma was unreachable by every spelling this tool has: `add_owner(/a,b/,
// @org/x)` split into three arguments and came back as an arity error naming an
// owner list nobody wrote, and the escaped spelling died on "dangling
// backslash". A comma is a legal git path byte, and so are brackets (S-2 makes
// them literal), so these are real monorepo paths.
//
// The backslash STAYS in the token: these are raw pattern texts, and stripping
// the escape would hand the planner a pattern for a different path — the same
// reason splitUnescapedWS keeps it.
func splitArgs(s string) []string {
	var args []string
	depth := 0
	start := 0
	esc := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case esc:
			esc = false
		case c == '\\':
			esc = true
		case c == '[':
			depth++
		case c == ']':
			depth--
		case c == ',' && depth == 0:
			args = append(args, trimArg(s[start:i]))
			start = i + 1
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

// swallowedSeparator finds an argument that ends with a dangling backslash and
// so ate the comma after it.
//
// `\,` is a literal comma inside a path (splitArgs), so a backslash at the END
// of an argument escapes the SEPARATOR instead, and the op arrives with fewer
// arguments than it names: `add_owner(/docs/ except /docs/gen\, @org/team_a)`
// comes out as one argument.
//
// The signature is the escaped comma followed by whitespace or the end of the
// argument list. A scope may not hold unescaped whitespace (checkScope), so
// `\, ` cannot be part of one path, while `/a\,b/` — a real path with a comma
// in its name — is left alone. Consulted only when the split already came up
// short, so a well-formed op never reaches it.
func swallowedSeparator(body string) (string, bool) {
	esc := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case esc:
			esc = false
			if c == ',' && (i+1 == len(body) || body[i+1] == ' ' || body[i+1] == '\t') {
				return trimArg(body[:i]), true
			}
		case c == '\\':
			esc = true
		}
	}
	return "", false
}

// WithExcept returns the op string s re-spelled with an except clause carrying
// pats, and reports whether that clause survives the grammar's own splitting.
//
// R-37's `except` array is validated, applied and REPORTED as the `<scope>
// except <pat> …` string it is equivalent to (R-37a), so this is the one place
// that writes that string. A caller that spelled it one way to validate and
// another to report is how an array-spelled op came to echo itself WITHOUT its
// carve in the R-8 remedy: advice to run an op that displaces the owners the
// carve existed to protect (adversarial-review finding).
//
// ok is false when the patterns cannot be carried by an op string at all, which
// the caller must report in the operator's terms rather than by parsing the
// mangled result: a pattern holding a comma re-splits as another ARGUMENT, and
// the arity error that follows names an owner list nobody wrote. The check is
// the round trip itself rather than a list of forbidden characters, because a
// balanced character class is live pattern syntax (R-37c) and survives.
//
// On a false return the ATTEMPTED string is still returned when one could be
// built, so a caller with nothing structural to name can report the grammar's
// own refusal of it rather than inventing a sentence: an element ending in a
// backslash eats the comma that ends the scope argument, and "contains a
// character an op string cannot carry" names no character at all.
//
// s must be an op string Parse accepted, and must not already carry an except
// clause; both spellings on one op are refused before this is reached (R-37b).
func WithExcept(s string, pats []string) (string, bool) {
	arg, start, end, ok := scopeArgSpan(s)
	if !ok || len(pats) == 0 {
		return "", false
	}
	out := s[:start] + arg + " except " + strings.Join(pats, " ") + s[end:]
	gotArg, _, _, ok := scopeArgSpan(out)
	if !ok {
		return out, false
	}
	gotScope, gotPats, isExcept := splitExceptClause(gotArg)
	if !isExcept || gotScope != arg || len(gotPats) != len(pats) {
		return out, false
	}
	for i := range pats {
		if gotPats[i] != pats[i] {
			return out, false
		}
	}
	return out, true
}

// scopeArgSpan returns an op string's first argument — the scope, or
// rename_owner's old owner — and the byte range it occupies, split exactly as
// Parse splits it. Offsets are into s as given: Parse trims before recording
// Op.Raw, so a caller passing Raw gets offsets it can splice.
func scopeArgSpan(s string) (arg string, start, end int, ok bool) {
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return "", 0, 0, false
	}
	body := s[open+1 : len(s)-1]
	depth, cut := 0, -1
	esc := false
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case esc:
			esc = false
		case c == '\\':
			esc = true
		case c == '[':
			depth++
		case c == ']':
			depth--
		case c == ',' && depth == 0 && cut < 0:
			cut = i
		}
	}
	if cut < 0 {
		// Every verb takes at least two arguments, so an op string with no
		// top-level comma has already lost one of them to a bracket.
		return "", 0, 0, false
	}
	raw := body[:cut]
	trimmed := trimArg(raw)
	lead := len(raw) - len(strings.TrimLeft(raw, " \t"))
	start = open + 1 + lead
	return trimmed, start, start + len(trimmed), true
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
//	set(L)    ∘ add(A)     iff A ⊆ L — set-then-add yields L ∪ A, add-then-set yields L
//	set(L)    ∘ remove(A)  iff A ∩ L = ∅ — set-then-remove yields L \ A, remove-then-set yields L
//	add(A)    ∘ remove(B)  iff A ∩ B = ∅
//
// Every case is set-valued because one op may name several owners (R-33).
// Reading a single owner field here silently degraded to comparing "" with "",
// which made every provably-overlapping pair look order-dependent and refused
// correct batches at exit 3.
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
			return ownersSubset(b.Owners, a.Owners)
		case RemoveOwner:
			return ownersDisjoint(a.Owners, b.Owners)
		}
	case AddOwner:
		if b.Kind == RemoveOwner {
			return ownersDisjoint(a.Owners, b.Owners)
		}
		return true // add ∘ add
	case RemoveOwner:
		if b.Kind == AddOwner {
			return ownersDisjoint(a.Owners, b.Owners)
		}
		return true // remove ∘ remove
	}
	// An op kind this function does not model must never be waved through as
	// commuting: the tree-based R-8 is the backstop, and reporting "these
	// commute" here would skip it.
	return false
}

// ownersSubset reports whether every owner in sub appears in super.
func ownersSubset(sub, super []string) bool {
	for _, o := range sub {
		if !containsOwner(super, o) {
			return false
		}
	}
	return true
}

// ownersDisjoint reports whether two owner lists share no member.
func ownersDisjoint(x, y []string) bool {
	for _, o := range x {
		if containsOwner(y, o) {
			return false
		}
	}
	return true
}

// sameOwnerSet compares two owner lists as SETS: order carries no meaning to
// GitHub, and two set_owners naming the same teams in different orders are the
// same intent. Membership is FoldOwner's (R-38a), so `[@Org/Team]` and
// `[@org/team]` are the same set — otherwise set ∘ set would be called
// order-dependent over one owner spelled two ways.
func sameOwnerSet(x, y []string) bool {
	seen := map[string]bool{}
	for _, o := range x {
		seen[FoldOwner(o)] = true
	}
	other := map[string]bool{}
	for _, o := range y {
		other[FoldOwner(o)] = true
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

// containsOwner asks FoldOwner, not byte equality: commutation is a comparison
// of two owners, so an add and a remove of one owner spelled two ways must not
// look disjoint — one order leaves the owner on the line and the other does
// not, which is exactly the batch R-8 exists to refuse (R-38a).
func containsOwner(list []string, s string) bool {
	for _, x := range list {
		if FoldOwner(x) == FoldOwner(s) {
			return true
		}
	}
	return false
}

// SpelledKind returns the verb an op string names, without validating anything
// else about it. R-39 needs the verb BEFORE the op parses — `rename_owner`
// takes no `owners` array (R-39c), and a scope-only op string does not parse at
// all until the array has been folded into it — and reading the verb off the
// text is the only thing available at that point.
//
// It reports Kind("") for text with no argument list, which no case below
// treats as a verb.
func SpelledKind(s string) Kind {
	s = strings.TrimSpace(s)
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return ""
	}
	return Kind(strings.TrimSpace(s[:open]))
}

// NamesOwners reports whether an op string already carries an owner argument.
//
// This is the question R-39b turns on: owners stated in the op string AND in an
// `owners` array is one intent in two places, and a policy whose two halves
// might disagree must never reach a decision about which one wins. The test is
// arity rather than a search for `[`, because the bare single-owner spelling
// `add_owner(/x/, @a)` is exactly as much a statement of owners as the list is,
// and an implementation that looked for a bracket would silently prefer one
// half of that pair.
func NamesOwners(s string) bool {
	body, ok := opBody(s)
	if !ok {
		return false
	}
	args := splitArgs(body)
	if len(args) < 2 {
		return false
	}
	if strings.HasPrefix(args[1], "[") {
		// A bracketed list states owners whatever it holds; what is inside it
		// is the list grammar's diagnosis to make, not this one's.
		return true
	}
	// Arity alone WAS the whole test, and the reason above still holds: the
	// bare `add_owner(/x/, @a)` spelling states owners exactly as much as the
	// list does. What arity could not tell apart is text that is not an owner
	// at all. A comma is a legal byte in a git path, so `add_owner(/a,b/)` — an
	// op naming one scope and no owners — arrived here as two arguments and was
	// refused for naming owners "in its op string AND in an owners array"
	// (R-39b), sending the reader to look for an array they did not write.
	// Text that cannot be an owner names none, and the op string's own grammar
	// then reports what is actually wrong with it.
	for _, a := range args[1:] {
		if !file.ValidOwnerToken(a) {
			return false
		}
	}
	return true
}

// opBody returns the text between an op string's outer parentheses, split
// exactly as Parse splits it — same trim, same open paren, same closing byte —
// so a string these helpers accept is one Parse reads the same way.
func opBody(s string) (string, bool) {
	s = strings.TrimSpace(s)
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return "", false
	}
	return s[open+1 : len(s)-1], true
}

// WithOwners returns the op string s — which must name a scope and no owners —
// re-spelled with owners as its owner argument, and reports whether that list
// survives the grammar's own splitting.
//
// R-39's `owners` array is validated, applied and REPORTED as the `(scope,
// [owners])` string it is equivalent to (R-39a), so this is the one place that
// writes that string. Re-spelling rather than decoding straight into Op.Owners
// is the point: every refusal the list grammar makes — a duplicate owner, an
// invalid token, an empty list on the wrong verb — then applies to the array
// for free, and a second copy of those checks beside the array is exactly how
// the two spellings would drift apart.
//
// The list is always spelled with brackets, including for one owner: R-33a
// makes `[@a]` and `@a` the same op, so there is no case to special-case, and
// one spelling means `results.jsonl` from an array-spelled wave is greppable
// by the same tooling as a list-spelled one.
//
// ok is false when the op string cannot carry the list — it names no scope, or
// it already names owners — and also when a token would not survive the split.
// The round trip is the backstop for that second case, not the diagnosis: only
// an email owner can hold a comma, a bracket or a space (a @handle cannot), and
// "your address has a comma in it" is a sentence the caller has to say for
// itself. A caller that reported a false return verbatim would tell an operator
// their op string is broken when their owner is.
func WithOwners(s string, owners []string) (string, bool) {
	t := strings.TrimSpace(s)
	open := strings.IndexByte(t, '(')
	body, ok := opBody(t)
	if !ok {
		return "", false
	}
	args := splitArgs(body)
	if len(args) != 1 || args[0] == "" {
		return "", false
	}
	out := strings.TrimSpace(t[:open]) + "(" + args[0] + ", [" + strings.Join(owners, ", ") + "])"

	// The round trip is the check, not a list of forbidden characters: the
	// owner grammar is two regexps and one of them (email) admits far more than
	// it looks like it does, so what matters is whether each element comes back
	// as its own token, unchanged, beside the scope it started with.
	gotBody, ok := opBody(out)
	if !ok {
		return "", false
	}
	gotArgs := splitArgs(gotBody)
	if len(gotArgs) != 2 || gotArgs[0] != args[0] {
		return "", false
	}
	list := gotArgs[1]
	if !strings.HasPrefix(list, "[") || !strings.HasSuffix(list, "]") {
		return "", false
	}
	inner := list[1 : len(list)-1]
	// The same rule parseOwnerList applies: brackets do not nest. A token
	// holding one survives the ARGUMENT split — `[a]b@x.com]` splits off the
	// scope cleanly — and would then be read as a list that closed early, so
	// the round trip has to ask this question itself rather than infer it from
	// the token count.
	if strings.ContainsAny(inner, "[]") {
		return "", false
	}
	toks := strings.FieldsFunc(inner, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	if len(toks) != len(owners) {
		return "", false
	}
	for i := range owners {
		if toks[i] != owners[i] {
			return "", false
		}
	}
	return out, true
}
