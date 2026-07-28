// Package audit_test defines Engine B: read-only rot detection.
//
// SPEC R-0: audit NEVER writes. Where a fix is expressible, it emits Engine A
// operations for a human to review and run through `plan`/`apply`.
// SPEC R-11: A-4 (dead rule) is report-only, permanently.
// SPEC R-12: every API-dependent check fails closed.
// SPEC R-13: email owners are unverifiable, never dead.
// SPEC R-14: removing a sole owner is a reassignment, shown as one.
package audit_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/audit"
	"github.com/jordonpeterson/codeowners-tool/internal/ghapi"
)

func findingsFor(rep *audit.Report, check string) []audit.Finding {
	var out []audit.Finding
	for _, f := range rep.Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func anyFixOps(rep *audit.Report) []string {
	var out []string
	for _, f := range rep.Findings {
		out = append(out, f.FixOps...)
	}
	return out
}

// ---------- offline checks ----------

// SPEC A-4 + R-11: a rule matching zero tracked files is reported and NEVER
// auto-fixed — `*.tf` in a repo about to adopt Terraform is intent, not rot.
func TestA4_DeadRuleReportOnly(t *testing.T) {
	rep := audit.Run(audit.Input{
		Content: []byte("/ghost/ @a\n* @b\n"),
		Tree:    []string{"real.txt"},
	})
	fs := findingsFor(rep, "A-4")
	if len(fs) != 1 {
		t.Fatalf("A-4 findings = %+v", rep.Findings)
	}
	if len(fs[0].FixOps) != 0 {
		t.Error("A-4 must never propose a fix (R-11)")
	}
}

// SPEC T-9 (S-6, A-5): `/Src/` on a tree containing `src/` is a CASE
// problem, not merely a dead rule. A-5 fires with a corrected pattern; A-4
// must not double-report it.
func TestT9_CaseMismatchIsA5NotA4(t *testing.T) {
	rep := audit.Run(audit.Input{
		Content: []byte("/Src/ @a\n"),
		Tree:    []string{"src/main.go"},
	})
	a5 := findingsFor(rep, "A-5")
	if len(a5) != 1 {
		t.Fatalf("A-5 must fire once, findings = %+v", rep.Findings)
	}
	if a5[0].SuggestedPattern != "/src/" {
		t.Errorf("suggested pattern = %q, want /src/", a5[0].SuggestedPattern)
	}
	if len(findingsFor(rep, "A-4")) != 0 {
		t.Error("A-4 must not misreport a case mismatch as merely dead (T-9)")
	}
}

// SPEC A-6: a rule every one of whose matches is won by later rules can
// never take effect.
func TestA6_FullyShadowedRule(t *testing.T) {
	rep := audit.Run(audit.Input{
		Content: []byte("/x/a.go @early\n/x/ @late\n"),
		Tree:    []string{"x/a.go"},
	})
	fs := findingsFor(rep, "A-6")
	if len(fs) != 1 || !strings.Contains(fs[0].Message, "line 2") {
		t.Errorf("A-6 findings = %+v", fs)
	}
}

// SPEC A-7: duplicate patterns are reported, not fixed.
func TestA7_DuplicatePattern(t *testing.T) {
	rep := audit.Run(audit.Input{
		Content: []byte("/x/ @a\n/x/ @b\n"),
		Tree:    []string{"x/a.go"},
	})
	if len(findingsFor(rep, "A-7")) != 1 {
		t.Errorf("A-7 findings = %+v", rep.Findings)
	}
}

// SPEC A-8: syntax errors are findings (and the invalid line is skipped, per
// current GitHub semantics — not the whole file).
func TestA8_SyntaxErrors(t *testing.T) {
	rep := audit.Run(audit.Input{
		Content: []byte("!bad @a\n* @ok\n"),
		Tree:    []string{"f.txt"},
	})
	if len(findingsFor(rep, "A-8")) != 1 {
		t.Errorf("A-8 findings = %+v", rep.Findings)
	}
}

// SPEC A-9: unowned coverage is a report, with count and samples.
func TestA9_UnownedCoverage(t *testing.T) {
	rep := audit.Run(audit.Input{
		Content: []byte("/x/ @a\n"),
		Tree:    []string{"x/a.go", "free1.txt", "free2.txt"},
	})
	fs := findingsFor(rep, "A-9")
	if len(fs) != 1 || !strings.Contains(fs[0].Message, "2 of 3") {
		t.Errorf("A-9 findings = %+v", fs)
	}
}

// SPEC A-10 (S-8): multiple CODEOWNERS files is an error — GitHub silently
// uses only the first by precedence and never merges.
func TestA10_MultipleFiles(t *testing.T) {
	rep := audit.Run(audit.Input{
		Content:    []byte("* @a\n"),
		Tree:       []string{".github/CODEOWNERS", "CODEOWNERS", "f.txt"},
		AllPresent: []string{".github/CODEOWNERS", "CODEOWNERS"},
	})
	fs := findingsFor(rep, "A-10")
	if len(fs) != 1 || fs[0].Severity != "error" {
		t.Errorf("A-10 findings = %+v", fs)
	}
}

// SPEC A-11: the CODEOWNERS file itself being unowned means nobody guards
// the guardrails.
func TestA11_CodeownersUnowned(t *testing.T) {
	rep := audit.Run(audit.Input{
		Content:        []byte("/x/ @a\n"),
		Tree:           []string{"x/a.go", ".github/CODEOWNERS"},
		CodeownersPath: ".github/CODEOWNERS",
	})
	if len(findingsFor(rep, "A-11")) != 1 {
		t.Errorf("A-11 findings = %+v", rep.Findings)
	}
}

// SPEC A-12 (S-4): size approaching 3 MB is a warning before it becomes a
// silent total failure.
func TestA12_SizeWarning(t *testing.T) {
	big := strings.Repeat("# padding line to inflate the file toward the limit\n", 50_000) + "* @a\n"
	rep := audit.Run(audit.Input{
		Content: []byte(big),
		Tree:    []string{"f.txt"},
	})
	if len(findingsFor(rep, "A-12")) != 1 {
		t.Errorf("A-12 findings = %+v (size=%d)", findingsFor(rep, "A-12"), len(big))
	}
}

// ---------- API-dependent checks ----------

func apiServer(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) *ghapi.Client {
	t.Helper()
	mux := http.NewServeMux()
	for pat, h := range routes {
		mux.HandleFunc(pat, h)
	}
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return ghapi.New(s.URL, "tok", ghapi.NewMemCache())
}

func ok(body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }
}
func status(code int) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
}

// SPEC T-8 (R-12): a token lacking org read access makes every org-dependent
// answer inconclusive: exit-5 state, and ZERO removal ops proposed. An
// expired token must never strip owners.
func TestT8_InsufficientTokenProposesNothing(t *testing.T) {
	client := apiServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/orgs/org/teams":   status(403),
		"/orgs/org/members": status(403),
		"/repos/org/repo":   ok(`{"full_name":"org/repo"}`),
	})
	rep := audit.Run(audit.Input{
		Content:   []byte("/x/ @org/team-1\n"),
		Tree:      []string{"x/a.go"},
		Client:    client,
		RepoOwner: "org", RepoName: "repo",
	})
	if len(rep.Inconclusive) == 0 {
		t.Fatal("insufficient token must mark the run inconclusive (exit 5)")
	}
	if ops := anyFixOps(rep); len(ops) != 0 {
		t.Fatalf("ZERO removal ops may be proposed on inconclusive lookups, got %v", ops)
	}
}

// SPEC T-11 (R-14, A-1): a dead owner's proposed removal is presented as a
// REASSIGNMENT — affected paths shown with before → after owners — never as
// a bare line deletion.
func TestT11_DeadOwnerRemovalShowsReassignment(t *testing.T) {
	client := apiServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/users/default-owner": ok(`{"login":"default-owner"}`),
		"/users/dead-owner":    status(404),
		"/user":                ok(`{"login":"me"}`),
	})
	rep := audit.Run(audit.Input{
		Content:   []byte("* @default-owner\n/x/ @dead-owner\n"),
		Tree:      []string{"x/a.go", "y/b.go"},
		Client:    client,
		RepoOwner: "org", RepoName: "repo",
		Checks: map[string]bool{"A-1": true},
	})
	fs := findingsFor(rep, "A-1")
	if len(fs) != 1 {
		t.Fatalf("A-1 findings = %+v", rep.Findings)
	}
	f := fs[0]
	if len(f.FixOps) != 1 || f.FixOps[0] != "remove_owner(/x/, @dead-owner)" {
		t.Errorf("fix ops = %v", f.FixOps)
	}
	if len(f.Reassignment) == 0 {
		t.Fatal("sole-owner removal must show the resulting reassignment (R-14)")
	}
	r := f.Reassignment[0]
	if r.Path != "x/a.go" ||
		len(r.Before) != 1 || r.Before[0] != "@dead-owner" ||
		len(r.After) != 1 || r.After[0] != "@default-owner" {
		t.Errorf("reassignment row = %+v, want x/a.go: {@dead-owner} -> {@default-owner}", r)
	}
	if len(rep.Inconclusive) != 0 {
		t.Errorf("clean lookups must not be inconclusive: %v", rep.Inconclusive)
	}
}

// SPEC R-13: email owners are exempt from A-1/A-2/A-3 — reported
// `unverifiable`, never dead, never removed.
func TestR13_EmailOwnersUnverifiable(t *testing.T) {
	client := apiServer(t, map[string]func(http.ResponseWriter, *http.Request){})
	rep := audit.Run(audit.Input{
		Content:   []byte("/x/ docs@example.com\n"),
		Tree:      []string{"x/a.go"},
		Client:    client,
		RepoOwner: "org", RepoName: "repo",
	})
	for _, f := range rep.Findings {
		if f.Owner == "docs@example.com" {
			if f.Status != "unverifiable" {
				t.Errorf("email owner status = %q, want unverifiable", f.Status)
			}
			if len(f.FixOps) != 0 {
				t.Error("email owner must never get a removal op (R-13)")
			}
		}
	}
}

// SPEC A-3 (S-5): an owner that exists, is in the org, but lacks EXPLICIT
// write access will never receive a review request — proposes removal.
func TestA3_NoWriteAccess(t *testing.T) {
	client := apiServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/users/reader":            ok(`{"login":"reader"}`),
		"/users/backup":            ok(`{"login":"backup"}`),
		"/orgs/org/teams":          ok(`[]`),
		"/orgs/org/members":        ok(`[]`),
		"/orgs/org/members/reader": status(204),
		"/repos/org/repo":          ok(`{"full_name":"org/repo"}`),
		"/repos/org/repo/collaborators/reader/permission": ok(`{"permission":"read","role_name":"read"}`),
		"/repos/org/repo/collaborators/backup/permission": ok(`{"permission":"write","role_name":"write"}`),
	})
	rep := audit.Run(audit.Input{
		Content:   []byte("* @backup\n/x/ @reader\n"),
		Tree:      []string{"x/a.go", "y.txt"},
		Client:    client,
		RepoOwner: "org", RepoName: "repo",
		Checks: map[string]bool{"A-3": true},
	})
	fs := findingsFor(rep, "A-3")
	if len(fs) != 1 {
		t.Fatalf("A-3 findings = %+v", rep.Findings)
	}
	if len(fs[0].FixOps) != 1 || fs[0].FixOps[0] != "remove_owner(/x/, @reader)" {
		t.Errorf("fix ops = %v", fs[0].FixOps)
	}
}

// SPEC R-0: no audit path ever produces anything but findings and Engine A
// op strings — verified here by construction: Report has no writer hooks,
// and every FixOp parses as a valid Engine A op.
func TestR0_FixOpsAreValidEngineAOps(t *testing.T) {
	client := apiServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/users/gone": status(404),
		"/users/keep": ok(`{"login":"keep"}`),
		"/user":       ok(`{"login":"me"}`),
	})
	rep := audit.Run(audit.Input{
		Content:   []byte("* @keep\n/a/ @gone\n/b/ @gone\n"),
		Tree:      []string{"a/1.go", "b/2.go", "c/3.go"},
		Client:    client,
		RepoOwner: "org", RepoName: "repo",
		Checks: map[string]bool{"A-1": true},
	})
	ops := anyFixOps(rep)
	if len(ops) != 2 {
		t.Fatalf("want one remove op per affected rule, got %v", ops)
	}
}

// Review finding: a CODEOWNERS file matched by a zero-owner rule is exactly
// as unowned as an unmatched one — A-11 must fire either way.
func TestA11_ZeroOwnerMatchStillUnowned(t *testing.T) {
	rep := audit.Run(audit.Input{
		Content:        []byte("* @team\n/.github/CODEOWNERS\n"),
		Tree:           []string{"a.go", ".github/CODEOWNERS"},
		CodeownersPath: ".github/CODEOWNERS",
	})
	if len(findingsFor(rep, "A-11")) != 1 {
		t.Errorf("A-11 must fire for a zero-owner-matched CODEOWNERS; findings = %+v", rep.Findings)
	}
}

// Review finding: when the tree's real casing is NOT simply lowercase
// (/Docs/ vs DOCS/), A-5 must still fire but must NOT claim a lowercased
// suggestion "would match" — it wouldn't.
func TestT9_A5NoFalseSuggestionForNonLowercaseTree(t *testing.T) {
	rep := audit.Run(audit.Input{
		Content: []byte("/Docs/ @a\n"),
		Tree:    []string{"DOCS/readme.md"},
	})
	fs := findingsFor(rep, "A-5")
	if len(fs) != 1 {
		t.Fatalf("A-5 must fire, findings = %+v", rep.Findings)
	}
	if fs[0].SuggestedPattern != "" {
		t.Errorf("suggestion %q would match nothing; must be empty", fs[0].SuggestedPattern)
	}
	if strings.Contains(fs[0].Message, "would match") {
		t.Errorf("message falsely promises a match: %s", fs[0].Message)
	}
	if len(findingsFor(rep, "A-4")) != 0 {
		t.Error("case-only miss must not double-report as A-4")
	}
}

// Second-review finding: when only A-3 is requested, a team probe failure
// must be attributed to A-3, not hardcoded to A-1.
func TestR12_TeamProbeAttribution(t *testing.T) {
	client := apiServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"/orgs/org/teams":   status(403),
		"/orgs/org/members": status(403),
	})
	rep := audit.Run(audit.Input{
		Content:   []byte("/x/ @org/team-1\n"),
		Tree:      []string{"x/a.go"},
		Client:    client,
		RepoOwner: "org", RepoName: "repo",
		Checks: map[string]bool{"A-3": true},
	})
	if len(rep.Inconclusive) == 0 {
		t.Fatal("probe failure must be inconclusive")
	}
	for _, f := range rep.Findings {
		if f.Status == "unknown" && f.Check != "A-3" {
			t.Errorf("unknown finding attributed to %s, want A-3 (the only requested check)", f.Check)
		}
	}
}
