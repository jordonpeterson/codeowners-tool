package plan_test

import (
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// SPEC: a set_owners insert that leaves a NARROWER pre-existing rule unable to
// win any path discloses it. Resolved ownership is right and the invariants
// hold — the defect is the rot the write authors: the repo audits clean before
// and fails A-6 ("fully shadowed", exit 4) after, with nothing in the run's own
// output naming the line.
func TestSetOwnersDisclosesTheNarrowerRuleItStrands(t *testing.T) {
	tree := []string{"docs/b.md", "docs/x/f.md"}
	p, err := build(t, "*         @org/everyone\n/docs/x/  @org/x-team\n", tree, plan.Options{},
		"set_owners(/docs/, [@org/new])")
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(p, `rule "/docs/x/" is now fully shadowed`) {
		t.Errorf("the stranded rule went unreported; warnings = %q", p.Warnings)
	}
	// The disclosure has to carry what a reader needs to act: the line that
	// killed it, and the owners the dead line still names.
	if !hasWarning(p, "@org/x-team") {
		t.Errorf("the disclosure does not name the owners the dead line still states; warnings = %q", p.Warnings)
	}
}

// SPEC: the disclosure is about lines this run kills, not about lines that were
// already dead. A rule shadowed before the run is audit's finding and has
// always been; repeating it here would make the new warning noise.
func TestStrandDisclosureIgnoresAnAlreadyDeadRule(t *testing.T) {
	tree := []string{"docs/b.md", "docs/x/f.md"}
	// "/docs/x/" is already fully shadowed by the "/docs/" line below it.
	p, err := build(t, "/docs/x/  @org/x-team\n/docs/    @org/old\n*  @org/everyone\n", tree, plan.Options{},
		"set_owners(/docs/, [@org/new])")
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(p, `rule "/docs/x/" is now fully shadowed`) {
		t.Errorf("a rule that was dead before the run is reported as one this run stranded; warnings = %q", p.Warnings)
	}
}

// SPEC: a rule the insert only partly covers is not stranded and is not
// reported. "**/*.md" loses docs/a.md to the new line and keeps src/b.md, so it
// still takes effect — calling that dead would send a reader to delete a live
// rule.
func TestStrandDisclosureSparesARuleThatStillWinsSomewhere(t *testing.T) {
	tree := []string{"docs/a.md", "src/b.md"}
	p, err := build(t, "*  @org/everyone\n**/*.md  @org/md-team\n", tree, plan.Options{},
		"set_owners(/docs/, [@org/new])")
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(p, "fully shadowed") {
		t.Errorf("a rule that still wins src/b.md was reported as dead; warnings = %q", p.Warnings)
	}
}

func hasWarning(p *plan.Plan, sub string) bool {
	for _, w := range p.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
