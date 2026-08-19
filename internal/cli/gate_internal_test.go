package cli

import (
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/audit"
)

// SPEC R-11: a finding whose severity this build does not recognize trips the
// gate at every `--fail-on` setting except `never`.
//
// This is the one branch in the gate whose failure is silent. Adding a check
// with a new severity — or reading a report produced by a newer build — must
// not hand a CI job a green build for findings it could not grade; the whole
// point of `--fail-on` is that it moves the gate deliberately, and an
// unrecognized severity is not a deliberate decision by anyone. It is tested
// from inside the package because no CLI invocation can produce a severity the
// binary does not define, which is exactly why the branch is easy to break
// without noticing: an end-to-end test cannot reach it at all.
func TestR11_UnknownSeverityFailsTheGate(t *testing.T) {
	unknown := []audit.Finding{{Check: "A-99", Severity: "catastrophe", Message: "from a newer build"}}
	for _, level := range []string{auditFailOnAny, "warning", "error"} {
		rank, ok := auditFailOnLevel(level)
		if !ok {
			t.Fatalf("--fail-on %s is not a legal level", level)
		}
		if !auditGateTripped(unknown, rank) {
			t.Errorf("--fail-on %s let an unrecognized severity through; a severity nobody can grade must fail closed, not slip under the gate", level)
		}
	}
	// `never` is the one setting that says "report only", and it means it.
	never, _ := auditFailOnLevel("never")
	if auditGateTripped(unknown, never) {
		t.Error("--fail-on never tripped the gate; it is the documented reporting-only mode")
	}
}

// SPEC R-11: the severity ladder is total and ordered — every severity the
// audit package emits has a rank, and each `--fail-on` level admits exactly the
// severities at or above it.
//
// The table is pinned rather than spot-checked because the failure mode of a
// missing rank is invisible: `auditSeverityRank` returning the zero value would
// make that severity rank BELOW `info`, so a whole class of findings would
// quietly stop gating any CI job that used the flag.
func TestR11_SeverityLadderIsTotal(t *testing.T) {
	for _, sev := range []string{"info", "warning", "error"} {
		if _, ok := auditSeverityRank[sev]; !ok {
			t.Fatalf("severity %q has no rank; it would sort below info and stop gating anything", sev)
		}
	}
	want := map[string]map[string]bool{
		// level -> severity -> trips the gate
		auditFailOnAny: {"info": true, "warning": true, "error": true},
		"warning":      {"info": false, "warning": true, "error": true},
		"error":        {"info": false, "warning": false, "error": true},
		"never":        {"info": false, "warning": false, "error": false},
	}
	for level, bySeverity := range want {
		rank, ok := auditFailOnLevel(level)
		if !ok {
			t.Fatalf("--fail-on %s is not a legal level", level)
		}
		for sev, trips := range bySeverity {
			got := auditGateTripped([]audit.Finding{{Severity: sev}}, rank)
			if got != trips {
				t.Errorf("--fail-on %s with a %s finding: gate tripped = %v, want %v", level, sev, got, trips)
			}
		}
	}
}
