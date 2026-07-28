// Property tests for the planner (T-4, T-5): thousands of generated
// (file, tree, op) cases, all checked against independently computed
// expectations. Deterministic seeds — failures reproduce exactly.
package plan_test

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

var (
	genDirs   = []string{"src", "docs", "x", "y", "Apps", "pkg", "services/api"}
	genFiles  = []string{"main.go", "README.md", "a.tf", "b.txt", "App.js", "mod.rs"}
	genPats   = []string{"/src/", "docs/*", "*.md", "/x/", "y/", "*.tf", "/pkg/", "*", "/services/api/", "/Apps/"}
	genOwners = []string{"@a", "@b", "@c", "@org/t1", "@org/t2", "dev@example.com"}
	policies  = []string{"error", "inherit", "unowned"}
)

func genTree(r *rand.Rand) []string {
	n := 3 + r.Intn(25)
	seen := map[string]bool{}
	var tree []string
	for i := 0; i < n; i++ {
		p := genDirs[r.Intn(len(genDirs))]
		if r.Intn(3) == 0 {
			p += "/" + genDirs[r.Intn(len(genDirs))]
		}
		p += "/" + genFiles[r.Intn(len(genFiles))]
		if !seen[p] {
			seen[p] = true
			tree = append(tree, p)
		}
	}
	return tree
}

func genFile(r *rand.Rand) string {
	var sb strings.Builder
	if r.Intn(2) == 0 {
		sb.WriteString("# generated fixture\n")
	}
	n := r.Intn(7)
	for i := 0; i < n; i++ {
		if r.Intn(5) == 0 {
			sb.WriteString("\n")
		}
		pat := genPats[r.Intn(len(genPats))]
		sb.WriteString(pat)
		k := r.Intn(3)
		for j := 0; j <= k; j++ {
			sb.WriteString(" " + genOwners[r.Intn(len(genOwners))])
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func genOp(r *rand.Rand) string {
	scope := genPats[r.Intn(len(genPats))]
	owner := genOwners[r.Intn(len(genOwners))]
	switch r.Intn(4) {
	case 0:
		return fmt.Sprintf("add_owner(%s, %s)", scope, owner)
	case 1:
		return fmt.Sprintf("set_owners(%s, [%s])", scope, owner)
	case 2:
		return fmt.Sprintf("remove_owner(%s, %s)", scope, owner)
	default:
		other := genOwners[(indexOf(genOwners, owner)+1+r.Intn(len(genOwners)-1))%len(genOwners)]
		return fmt.Sprintf("rename_owner(%s, %s)", owner, other)
	}
}

// SPEC T-4 (INV-1 + INV-2, property form): for ANY generated file, tree, and
// op, every successful plan (a) leaves every out-of-scope path's owners
// byte-identical, and (b) satisfies the op's semantics for every in-scope
// path — both checked by independent re-resolution of the emitted content,
// not by trusting the planner's own gate.
func TestT4_Property_InvariantsHold(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	successes := 0
	for i := 0; i < 3000; i++ {
		content, tree, opSpec := genFile(r), genTree(r), genOp(r)
		policy := policies[r.Intn(len(policies))]
		parsed, err := ops.ParseAll([]string{opSpec})
		if err != nil {
			t.Fatalf("case %d: generator produced unparseable op %q: %v", i, opSpec, err)
		}
		op := parsed[0]
		p, err := plan.Build([]byte(content), tree, parsed, plan.Options{OnEmpty: policy})
		if err != nil {
			continue // no-op / invalid / refused: refusing is always sound
		}
		successes++

		before := plan.ResolveContent(content, tree)
		after := plan.ResolveContent(p.AfterContent, tree)
		scope := scopeSet(op, content, tree)

		for _, path := range tree {
			b, a := before[path].Owners, after[path].Owners
			if !scope[path] {
				if !ownersEq(b, a) {
					t.Fatalf("case %d (seed 42): INV-2 violated\nfile:\n%s\nop: %s (on-empty=%s)\npath %s: %v -> %v\nresult:\n%s",
						i, content, opSpec, policy, path, b, a, p.AfterContent)
				}
				continue
			}
			switch op.Kind {
			case ops.AddOwner:
				want := union(b, op.Owner)
				if !ownersEq(a, want) {
					t.Fatalf("case %d: INV-1 add: path %s: %v -> %v, want %v\nfile:\n%s\nop: %s\nresult:\n%s",
						i, path, b, a, want, content, opSpec, p.AfterContent)
				}
			case ops.SetOwners:
				if !ownersEq(a, op.Owners) {
					t.Fatalf("case %d: INV-1 set: path %s: got %v, want %v\nfile:\n%s\nop: %s\nresult:\n%s",
						i, path, a, op.Owners, content, opSpec, p.AfterContent)
				}
			case ops.RemoveOwner:
				if has(a, op.Owner) {
					t.Fatalf("case %d: INV-1 remove: %s still owns %s\nfile:\n%s\nop: %s\nresult:\n%s",
						i, op.Owner, path, content, opSpec, p.AfterContent)
				}
			case ops.RenameOwner:
				if has(a, op.Owner) {
					t.Fatalf("case %d: INV-1 rename: old owner %s still present on %s", i, op.Owner, path)
				}
				if has(b, op.Owner) && !has(a, op.NewOwner) {
					t.Fatalf("case %d: INV-1 rename: new owner %s missing on %s", i, op.NewOwner, path)
				}
			}
		}
	}
	if successes < 200 {
		t.Fatalf("only %d/3000 generated cases produced plans — generator or planner too restrictive for the property to mean anything", successes)
	}
	t.Logf("verified invariants on %d successful plans", successes)
}

// SPEC T-5 (INV-4, property form): re-running any successful op against its
// own output is a no-op — byte growth on repeat runs is how files blow past
// S-4's limit.
func TestT5_Property_Idempotent(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	checked := 0
	for i := 0; i < 2000; i++ {
		content, tree, opSpec := genFile(r), genTree(r), genOp(r)
		policy := policies[r.Intn(len(policies))]
		parsed, _ := ops.ParseAll([]string{opSpec})
		p, err := plan.Build([]byte(content), tree, parsed, plan.Options{OnEmpty: policy})
		if err != nil {
			continue
		}
		checked++
		_, err = plan.Build([]byte(p.AfterContent), tree, parsed, plan.Options{OnEmpty: policy})
		var noop *plan.NoOpError
		if !errors.As(err, &noop) {
			t.Fatalf("case %d: second run not a no-op (err=%v)\nfile:\n%s\nop: %s (on-empty=%s)\nfirst result:\n%s",
				i, err, content, opSpec, policy, p.AfterContent)
		}
	}
	if checked < 150 {
		t.Fatalf("only %d/2000 cases checked", checked)
	}
	t.Logf("idempotence verified on %d plans", checked)
}

// INV-5 property: parse→serialize round-trips any generated file (plus junk
// mutations) byte-identically.
func TestINV5_Property_RoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	for i := 0; i < 2000; i++ {
		content := genFile(r)
		// Inject line-ending and whitespace chaos.
		if r.Intn(3) == 0 {
			content = strings.ReplaceAll(content, "\n", "\r\n")
		}
		if r.Intn(3) == 0 {
			content = strings.TrimSuffix(content, "\n")
		}
		if r.Intn(4) == 0 {
			content += "   \t\n# trailing comment"
		}
		f := file.Parse([]byte(content))
		if got := string(f.Bytes()); got != content {
			t.Fatalf("case %d: round trip changed bytes:\n in: %q\nout: %q", i, content, got)
		}
	}
}

func scopeSet(op ops.Op, content string, tree []string) map[string]bool {
	set := map[string]bool{}
	if op.Kind == ops.RenameOwner {
		before := plan.ResolveContent(content, tree)
		for _, p := range tree {
			if has(before[p].Owners, op.Owner) {
				set[p] = true
			}
		}
		return set
	}
	pat, err := pattern.Compile(op.Scope)
	if err != nil {
		return set
	}
	for _, p := range tree {
		if pat.Match(p) {
			set[p] = true
		}
	}
	return set
}

func ownersEq(a, b []string) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	as, bs := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func union(owners []string, o string) []string {
	if has(owners, o) {
		return owners
	}
	return append(append([]string{}, owners...), o)
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return 0
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
