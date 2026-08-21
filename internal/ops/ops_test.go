// Package ops_test defines the intent language: operations are expressed
// over resolved ownership, never over lines (§1 of the spec).
package ops_test

import (
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
