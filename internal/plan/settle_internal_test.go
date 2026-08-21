// White-box test for the remove_owner settling logic (second-review
// regression): divergence between the pure transform desired∖{owner} and the
// file's actual resolution is accepted ONLY under --on-empty=inherit, where
// rule deletion legitimately resurrects owners from surviving rules. Under
// any other policy no rule is deleted, so divergence means a synthesis bug
// (or a bad earlier batched edit) and must REFUSE — accepting it would
// launder the error past the gate.
package plan

import (
	"errors"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
)

func TestSettle_RefusesDivergenceUnderNonInherit(t *testing.T) {
	f := file.Parse([]byte("/a/ @b @y\n"))
	tree := []string{"a/f.go"}
	op := ops.Op{Kind: ops.RemoveOwner, Scope: "/a/", Owners: []string{"@y"}, Raw: "remove_owner(/a/, @y)"}
	scope := map[string]bool{"a/f.go": true}

	// Corrupted desired state simulating an earlier op's bad edit: claims the
	// path should own {@b, @z}. The file after removal resolves to {@b}.
	desired := map[string][]string{"a/f.go": {"@b", "@z"}}
	err := synthRemove(f, tree, op, scope, desired, "", &Plan{})
	var ref *RefusalError
	if !errors.As(err, &ref) {
		t.Fatalf("divergence under non-inherit must refuse (gate laundering), got %v", err)
	}

	// Control: consistent desired state settles cleanly via the pure transform.
	f2 := file.Parse([]byte("/a/ @b @y\n"))
	desired2 := map[string][]string{"a/f.go": {"@b", "@y"}}
	if err := synthRemove(f2, tree, op, scope, desired2, "", &Plan{}); err != nil {
		t.Fatalf("consistent removal must succeed: %v", err)
	}
}
