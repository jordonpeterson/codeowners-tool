package lint_test

import (
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/lint"
)

// SPEC R-38a in stage 2's record: a dead owner spelled @Org/Gone is the same
// owner as @org/gone, so the lookup and the `dead` map are both case-folded —
// but the Action.Reason was read back with the UNFOLDED spelling, an empty
// reason on exactly the removals whose spelling differs from the fold. The
// reason is what a reviewer approves the deletion on, so it must survive the
// file's own capitalisation.
func TestRemoveDeadOwner_MixedCaseSpellingKeepsItsReason(t *testing.T) {
	v := fcNew()
	v.missingTeams["org/gone"] = true

	content := []byte("* @keep @Org/Gone\n")
	res, err := lint.Build(content, []string{"a.md"}, v, fcOpts())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	removed := fcActionsOfKind(res, lint.ActionRemoveOwner)
	if len(removed) != 1 || removed[0].Owner != "@Org/Gone" {
		t.Fatalf("remove actions = %+v, want exactly one for @Org/Gone", res.Actions)
	}
	if removed[0].Reason == "" {
		t.Fatal("Reason is empty — the dead map is keyed by the folded spelling and the action looked it up unfolded")
	}
	if !strings.Contains(removed[0].Reason, "does not exist") {
		t.Errorf("Reason = %q, want the lookup's verdict on the record", removed[0].Reason)
	}
}
