package ops_test

import (
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
)

// SPEC: a backslash escapes a comma in an op string exactly as it escapes a
// space, so a path holding a comma is reachable. The backslash stays in the
// scope text: it is the pattern language's own escape, and stripping it would
// hand the planner a pattern for a different path.
func TestEscapedCommaKeepsTheScopeWhole(t *testing.T) {
	op, err := ops.Parse(`add_owner(/a\,b/, @org/x)`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if op.Scope != `/a\,b/` {
		t.Errorf("Scope = %q, want %q", op.Scope, `/a\,b/`)
	}
	if len(op.Owners) != 1 || op.Owners[0] != "@org/x" {
		t.Errorf("Owners = %q", op.Owners)
	}
	// A bracket is a literal in CODEOWNERS (S-2), and an escaped one must not
	// move the argument splitter's bracket depth either.
	if op, err := ops.Parse(`add_owner(/a\[b/, @org/x)`); err != nil {
		t.Errorf(`Parse(add_owner(/a\[b/, @org/x)): %v`, err)
	} else if op.Scope != `/a\[b/` {
		t.Errorf("Scope = %q", op.Scope)
	}
}

// SPEC: an UNESCAPED comma still separates arguments, and the refusal names the
// text that landed where an owner belongs plus the escape that would have kept
// it in the scope — rather than an argument count the operator did not write.
func TestUnescapedCommaInAScopeIsNamedForWhatItIs(t *testing.T) {
	_, err := ops.Parse(`add_owner(/a,b/, @org/x)`)
	if err == nil {
		t.Fatal("an unescaped comma splits the arguments; this must be refused")
	}
	for _, want := range []string{`"b/"`, `\,`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// SPEC: a scope's dangling backslash eats the comma that ends the argument, and
// the refusal says so. Under the escape this is no longer an arity accident —
// it is one argument swallowing the rest of the op.
func TestDanglingBackslashSwallowingTheSeparatorIsNamed(t *testing.T) {
	_, err := ops.Parse(`add_owner(/docs/ except /docs/gen\, @org/team_a)`)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "dangling backslash") {
		t.Errorf("error %q does not name the dangling backslash", err)
	}
}

// SPEC: NamesOwners answers R-39b's question — does this op string STATE
// owners — and text that cannot be an owner states none. Reading `b/` as an
// owner is how an op naming one scope came to be refused for naming owners
// twice.
func TestNamesOwnersAsksWhetherTheTextCouldBeAnOwner(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want bool
	}{
		{"add_owner(/x/, @org/a)", true},
		{"add_owner(/x/, [@org/a, @org/b])", true},
		{"set_owners(/x/, [])", true},
		{"add_owner(/x/)", false},
		{"add_owner(/a,b/)", false},
		{`add_owner(/a\,b/)`, false},
		{"rename_owner(@a, @b)", true},
	} {
		if got := ops.NamesOwners(tc.spec); got != tc.want {
			t.Errorf("NamesOwners(%q) = %v, want %v", tc.spec, got, tc.want)
		}
	}
}

// SPEC: a scope no path can ever match is refused where the verdict belongs —
// in the op string, with no repository open, so `check` catches it once instead
// of every repo refusing it separately at exit 2 (or `declare` writing it).
func TestScopeThatCanNeverMatchIsRefused(t *testing.T) {
	for _, scope := range []string{"**/", "**/**", "**/**/x", "/"} {
		if _, err := ops.Parse("add_owner(" + scope + ", @org/x)"); err == nil {
			t.Errorf("scope %q was accepted; no path can ever match it", scope)
		} else if !strings.Contains(err.Error(), "any repository") {
			t.Errorf("scope %q: error %q does not say the pattern is dead everywhere", scope, err)
		}
	}
	// Alive neighbours, including the two adjacent-`**` spellings that are not
	// leading and the one a trailing slash normalizes into them.
	for _, scope := range []string{"x/**/**", "**/x/**", "foo/**/", "**/*.tf", "/docs/", "*"} {
		if _, err := ops.Parse("add_owner(" + scope + ", @org/x)"); err != nil {
			t.Errorf("scope %q was refused: %v", scope, err)
		}
	}
}

// SPEC: a scope whose written line would read back as a comment is refused at
// parse time, in the words of the defect — the same standard `!` and `\#`
// already meet.
func TestLeadingHashScopeIsRefusedByParse(t *testing.T) {
	_, err := ops.Parse("add_owner(#hash/, @org/x)")
	if err == nil {
		t.Fatal("a scope starting with '#' writes a line that is a comment")
	}
	if !strings.Contains(err.Error(), "comment") {
		t.Errorf("error %q does not say the line reads back as a comment", err)
	}
}
