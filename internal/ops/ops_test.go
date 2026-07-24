// Package ops_test defines the intent language: operations are expressed
// over resolved ownership, never over lines (§1 of the spec).
package ops_test

import (
	"testing"

	"github.com/jordonpropm/codeowners-tool/internal/ops"
)

func TestParse_AddOwner(t *testing.T) {
	op, err := ops.Parse("add_owner(/services/api, @org/team-1)")
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != ops.AddOwner || op.Scope != "/services/api" || op.Owner != "@org/team-1" {
		t.Errorf("got %+v", op)
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
	if op.Kind != ops.RemoveOwner || op.Scope != "/x/" || op.Owner != "@a" {
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
		"remove_owner(/x/, [@a, @b])",  // remove takes one owner
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
