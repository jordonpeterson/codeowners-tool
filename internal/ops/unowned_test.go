package ops_test

import (
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
)

// SPEC R-40/R-8: on_unowned=skip makes an op's effective scope depend on the
// repository's current ownership, so a provably-overlapping pair is
// order-dependent only in repos that actually own something in the overlap —
// a fact about the tree. Like on_zero_match=skip, the pair therefore belongs
// to plan.Build's per-repo exit 2, and StaticConflict must stand down rather
// than halt the whole rollout at exit 3.
func TestR40_StaticConflictDefersSkipUnownedOps(t *testing.T) {
	set, err := ops.Parse("set_owners(*, [@org/everyone])")
	if err != nil {
		t.Fatal(err)
	}
	add, err := ops.Parse("add_owner(/services/api/, @org/platform)")
	if err != nil {
		t.Fatal(err)
	}

	// Without the field the pair is the canonical static conflict.
	if err := ops.StaticConflict([]ops.Op{set, add}); err == nil {
		t.Fatal("the displacing pair must be a static conflict without on_unowned (R-8)")
	}

	add.OnUnowned = ops.UnownedSkip
	if err := ops.StaticConflict([]ops.Op{set, add}); err != nil {
		t.Errorf("with on_unowned=skip the conflict is repo-conditional and belongs to exit 2, got static refusal: %v", err)
	}
}
