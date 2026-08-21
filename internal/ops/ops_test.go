// Package ops_test defines the intent language: operations are expressed
// over resolved ownership, never over lines (§1 of the spec).
package ops_test

import (
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
)

func TestParse_AddOwner(t *testing.T) {
	op, err := ops.Parse("add_owner(/services/api, @org/team-1)")
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != ops.AddOwner || op.Scope != "/services/api" || len(op.Owners) != 1 || op.Owners[0] != "@org/team-1" {
		t.Errorf("got %+v", op)
	}
	// R-33: Owner is rename_owner's old name only. A bare owner must land in
	// Owners like any list, so no downstream site can read a single value off
	// an op that might name several.
	if op.Owner != "" {
		t.Errorf("add_owner must not set Owner, got %q", op.Owner)
	}
}

// SPEC R-33: add_owner and remove_owner accept a bracketed owner list, folding
// to the same targets as the single-owner ops it replaces.
func TestParse_OwnerList(t *testing.T) {
	for _, spec := range []string{"add_owner(/x/, [@a, @b])", "remove_owner(/x/, [@a, @b])"} {
		op, err := ops.Parse(spec)
		if err != nil {
			t.Fatalf("Parse(%q): %v", spec, err)
		}
		if len(op.Owners) != 2 || op.Owners[0] != "@a" || op.Owners[1] != "@b" {
			t.Errorf("Parse(%q): got Owners %v, want [@a @b]", spec, op.Owners)
		}
	}
}

func TestParse_SetOwners(t *testing.T) {
	op, err := ops.Parse("set_owners(/x/, [@a, @b])")
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != ops.SetOwners || op.Scope != "/x/" || len(op.Owners) != 2 || op.Owners[0] != "@a" || op.Owners[1] != "@b" {
		t.Errorf("got %+v", op)
	}
}

// set_owners with an empty list is legal: it is the explicit S-9 "un-own this
// subtree" intent.
func TestParse_SetOwnersEmpty(t *testing.T) {
	op, err := ops.Parse("set_owners(/x/, [])")
	if err != nil {
		t.Fatal(err)
	}
	if op.Owners == nil || len(op.Owners) != 0 {
		t.Errorf("want explicit empty owner list, got %+v", op)
	}
}

func TestParse_RemoveOwner(t *testing.T) {
	op, err := ops.Parse("remove_owner(/x/, @a)")
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != ops.RemoveOwner || op.Scope != "/x/" || len(op.Owners) != 1 || op.Owners[0] != "@a" {
		t.Errorf("got %+v", op)
	}
}

// rename_owner is deliberately unscoped (§4.1): it is the only operation safe
// as pure identifier substitution, because it cannot change any rule's match
// set.
func TestParse_RenameOwner(t *testing.T) {
	op, err := ops.Parse("rename_owner(@old-team, @org/new-team)")
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != ops.RenameOwner || op.Owner != "@old-team" || op.NewOwner != "@org/new-team" {
		t.Errorf("got %+v", op)
	}
}

// SPEC R-17 exit 3: malformed ops are invalid input, rejected at parse time.
func TestParse_Malformed(t *testing.T) {
	bad := []string{
		"",
		"frobnicate(/x/, @a)",          // unknown op
		"add_owner(/x/)",               // missing owner
		"add_owner(/x/, not-an-owner)", // invalid owner token
		"add_owner(, @a)",              // empty scope
		"set_owners(/x/, @a)",          // owners must be a [list]
		"remove_owner(/x/, [@a, @a])",  // R-33c: duplicate inside one list
		"add_owner(/x/, [])",           // R-33d: empty list states no intent
		"add_owner(/x/, [@a, [@b]])",   // R-33: brackets do not nest
		"rename_owner([@a], @b)",       // R-33f: rename takes no list
		"rename_owner(@a)",             // missing new name
		"rename_owner(@a, @a)",         // self-rename is meaningless
		"add_owner(!/x/, @a)",          // negation scope
	}
	for _, s := range bad {
		if _, err := ops.Parse(s); err == nil {
			t.Errorf("Parse(%q) must fail", s)
		}
	}
}

// Unescaped whitespace in a scope cannot survive serialization: the written
// line "a b @x" re-parses as pattern "a" owned by "b" — a different, valid
// rule that silently breaks both invariants (review finding). CODEOWNERS
// spells such patterns with escaped spaces, and so must ops.
func TestParse_UnescapedWhitespaceScopeRejected(t *testing.T) {
	if _, err := ops.Parse(`add_owner(/a b/, @x)`); err == nil {
		t.Error("unescaped-space scope must be rejected")
	}
	if _, err := ops.Parse(`add_owner(/a\ b/, @x)`); err != nil {
		t.Errorf("escaped-space scope must be accepted: %v", err)
	}
}

// Second-review finding: a trailing ESCAPED space (`a\ ` — a path ending in
// a space) was mangled by argument trimming into a dangling `a\`, which
// compiles to a pattern for a DIFFERENT path. It must survive intact, and a
// genuinely dangling backslash must be rejected outright.
func TestParse_TrailingEscapedSpacePreserved(t *testing.T) {
	op, err := ops.Parse(`add_owner(/a\ , @x)`)
	if err != nil {
		t.Fatal(err)
	}
	if op.Scope != `/a\ ` {
		t.Errorf("scope = %q, want %q (escaped trailing space must survive)", op.Scope, `/a\ `)
	}
	if _, err := ops.Parse(`add_owner(a\, @x)`); err == nil {
		t.Error("dangling backslash scope must be rejected")
	}
}

// SPEC R-37a/R-37e: WithExcept writes the one string the array spelling is
// validated, applied and reported as — so what it returns has to be an op that
// re-parses to the same intent, and it has to REFUSE what an op string cannot
// carry rather than return something that parses as a different op.
//
// The refusals are the adversarial-review findings: a comma re-splits the
// clause as another argument (the arity error that followed named an owner
// list nobody wrote), and a trailing backslash escapes the space in front of
// the next pattern, silently merging two carve-outs into one pattern that
// matches nothing.
func TestWithExcept_RoundTripsOrRefuses(t *testing.T) {
	ok := []struct {
		op   string
		pats []string
		want string
	}{
		{"add_owner(/x/, @a)", []string{"/x/gen/"}, "add_owner(/x/ except /x/gen/, @a)"},
		{"add_owner( /x/ , @a)", []string{"/x/gen/"}, "add_owner( /x/ except /x/gen/ , @a)"},
		// The owner list is left exactly as written: the result is echoed back
		// to an operator as an op to re-run, so it must be their own text.
		{"set_owners(/x/, [@a, @b])", []string{"/x/gen/", "/x/vendor/"},
			"set_owners(/x/ except /x/gen/ /x/vendor/, [@a, @b])"},
		// A balanced character class is live pattern syntax (R-37c), commas
		// inside it included, and survives the split.
		{"add_owner(/x/, @a)", []string{"/x/[ab].md"}, "add_owner(/x/ except /x/[ab].md, @a)"},
		// rename_owner has no scope: the clause lands on the argument that
		// exists, so Parse can say so (R-27.4) instead of reporting a bad
		// owner token.
		{"rename_owner(@a, @b)", []string{"/x/"}, "rename_owner(@a except /x/, @b)"},
	}
	for _, tc := range ok {
		got, valid := ops.WithExcept(tc.op, tc.pats)
		if !valid || got != tc.want {
			t.Errorf("WithExcept(%q, %q) = %q, %v; want %q, true", tc.op, tc.pats, got, valid, tc.want)
			continue
		}
		if tc.op == "rename_owner(@a, @b)" {
			continue // refused by Parse on purpose (R-27.4)
		}
		back, err := ops.Parse(got)
		if err != nil {
			t.Errorf("WithExcept(%q, %q) = %q, which does not parse: %v", tc.op, tc.pats, got, err)
			continue
		}
		if len(back.Except) != len(tc.pats) {
			t.Errorf("%q re-parses with %d except patterns, want %d", got, len(back.Except), len(tc.pats))
		}
	}

	bad := []struct {
		op   string
		pats []string
	}{
		{"add_owner(/x/, @a)", []string{"/x/a,b.md"}},     // splits as another argument
		{"add_owner(/x/, @a)", []string{"/x/a[b.md"}},     // swallows the separator
		{"add_owner(/x/, @a)", []string{"/x/a]b.md"}},     // same, from the other side
		{"add_owner(/x/, @a)", []string{`/x/a\`, "/x/b"}}, // escapes the space before /x/b
		{"add_owner(/x/, @a)", nil},                       // no clause to write
	}
	for _, tc := range bad {
		if got, valid := ops.WithExcept(tc.op, tc.pats); valid {
			t.Errorf("WithExcept(%q, %q) = %q, true; want false — the clause does not survive the split", tc.op, tc.pats, got)
		}
	}
}

// SPEC R-33c/R-33d: one grammar covers every verb that names owners.
//
// set_owners had its own hand-rolled list loop with no duplicate check, so
// `set_owners(/x/, [@org/a, @org/a])` passed `check` at exit 0 and `sync` wrote
// `/services/api/ @org/a @org/a` — after which `verify` reported {@org/a} →
// {@org/a, @org/a} as an invariant violation, the fleet's rollback signal
// firing on a semantic no-op. The pair of set_owners ops differing only by a
// duplicate also split check (exit 0) from sync (exit 2 per repo, with an R-8
// message), which is precisely the repo-independent split ops.StaticConflict
// exists to prevent.
func TestParse_SetOwnersSharesTheOwnerListGrammar(t *testing.T) {
	if _, err := ops.Parse("set_owners(/x/, [@org/a, @org/a])"); err == nil {
		t.Error("set_owners with a duplicate owner must be refused, as add_owner already is (R-33c)")
	} else if !strings.Contains(err.Error(), "duplicate owner") {
		t.Errorf("refusal must name the duplicate; got %v", err)
	}
	// The list grammar's other refusal, which set_owners also lacked: a nested
	// bracket names the real problem instead of blaming the token `[@a`.
	if _, err := ops.Parse("set_owners(/x/, [[@a, @b]])"); err == nil {
		t.Error("nested brackets must be refused")
	} else if !strings.Contains(err.Error(), "brackets do not nest") {
		t.Errorf("refusal must name the nesting; got %v", err)
	}
	// R-33d, unchanged and load-bearing: the empty list is how set_owners
	// spells "nobody owns this", while an empty add or remove states no intent
	// at all. Owners stays non-nil so no site can confuse it with an op that
	// names no owners at all.
	op, err := ops.Parse("set_owners(/x/, [])")
	if err != nil {
		t.Fatalf("set_owners(/x/, []) is legal: %v", err)
	}
	if op.Owners == nil || len(op.Owners) != 0 {
		t.Errorf("Owners = %#v, want an empty non-nil slice", op.Owners)
	}
	for _, spec := range []string{"add_owner(/x/, [])", "remove_owner(/x/, [])"} {
		if _, err := ops.Parse(spec); err == nil {
			t.Errorf("%s must be refused (R-33d)", spec)
		}
	}
}

// SPEC R-33c: two owner tokens are duplicates under the identity the rest of
// the codebase uses for owners — @handles fold to lowercase, because GitHub
// does.
//
// Exact-string identity let `add_owner(/x/, [@Org/Team, @org/team])` through,
// and syncing it onto a file already holding @org/team wrote
// `/x/ @org/team @Org/Team`: one owner, twice. The dangerous half is removal —
// `remove_owner(/x/, [@Org/Team])` against a file holding @org/team reports
// "unchanged" at exit 0 while the owner still owns the path, so a fleet run of
// "revoke the departed team" reports converged on every repo that capitalised
// the handle. This test fixes the duplicate half; the matching half is a
// broader change (see the report accompanying this commit).
func TestParse_DuplicateOwnersFoldCase(t *testing.T) {
	for _, spec := range []string{
		"add_owner(/x/, [@Org/Team, @org/team])",
		"remove_owner(/x/, [@org/team, @ORG/TEAM])",
		"set_owners(/x/, [@A, @a])",
	} {
		if _, err := ops.Parse(spec); err == nil {
			t.Errorf("%s names one owner twice (R-33c)", spec)
		} else if !strings.Contains(err.Error(), "duplicate owner") {
			t.Errorf("%s: refusal must name the duplicate; got %v", spec, err)
		}
	}
	// An email is left alone: the local part of an address is not ours to
	// case-fold, and two spellings may be two different mailboxes.
	if _, err := ops.Parse("add_owner(/x/, [Dev@example.com, dev@example.com])"); err != nil {
		t.Errorf("email owners must not be case-folded: %v", err)
	}
}
