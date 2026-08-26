package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// auditReport is the subset of `audit --format json` these tests assert on.
// Going through the JSON record means the assertions name the CHECK ID, which
// is the contract, rather than prose a reword would break.
type auditReport struct {
	Findings []struct {
		Check            string `json:"check"`
		Severity         string `json:"severity"`
		Message          string `json:"message"`
		SuggestedPattern string `json:"suggested_pattern"`
	} `json:"findings"`
	Inconclusive []string `json:"inconclusive"`
}

// forCheck returns "severity: message" for every finding under check, so a
// failure prints what was actually reported.
func (r auditReport) forCheck(check string) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Check == check {
			out = append(out, f.Severity+": "+f.Message)
		}
	}
	return out
}

func auditJSON(t *testing.T, repo string, extra ...string) (int, auditReport) {
	t.Helper()
	args := append([]string{"audit", "--repo", repo, "--format", "json"}, extra...)
	code, stdout, stderr := runCLI(t, args...)
	var rep auditReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("audit --format json did not emit one JSON document: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	return code, rep
}

// A nested CODEOWNERS is a `warning`, and a second ROOT-LEVEL one stays an
// `error` — the two are different defects and `--fail-on` must tell them apart.
//
// A second root-level file is ambiguity in the document that governs: two files
// GitHub itself searches, one of them silently losing. A `packages/foo/`
// CODEOWNERS is never searched at all, so nothing about what governs is in
// doubt, and it is routinely a deliberate artifact of a Bazel/Gerrit OWNERS
// migration that some other tool still consumes. Ranking it `error` would turn
// `--fail-on error` — documented as the tier for "GitHub is doing something
// other than what the file says" — red on every such monorepo, over a condition
// no edit to the governing file resolves. It is still rot (review is routed by
// it in people's heads and nothing enforces that), so it must clear `info`.
func TestAuditNestedCodeownersIsAWarningAndRootLevelStaysAnError(t *testing.T) {
	nested := initRepo(t, map[string]string{
		".github/CODEOWNERS":              "* @org/every\n",
		"packages/foo/.github/CODEOWNERS": "/src/ @org/foo-team\n",
		"packages/foo/src/a.go":           "",
	})
	code, rep := auditJSON(t, nested)
	got := rep.forCheck("A-10")
	if len(got) != 1 || !strings.HasPrefix(got[0], "warning: ") {
		t.Fatalf("nested CODEOWNERS: want exactly one A-10 at severity warning, got %v", got)
	}
	if !strings.Contains(got[0], "packages/foo/.github/CODEOWNERS") {
		t.Errorf("the A-10 finding does not name the file that governs nothing: %q", got[0])
	}
	if code != cli.ExitFindings {
		t.Errorf("default --fail-on any: exit %d, want %d", code, cli.ExitFindings)
	}
	if code, _, _ := runCLI(t, "audit", "--repo", nested, "--fail-on", "error"); code != cli.ExitOK {
		t.Errorf("--fail-on error exits %d on a nested-only repo; a file GitHub never searches is not the "+
			"same defect as two files it does, and the error gate is what fleet CI blocks on", code)
	}
	if code, _, _ := runCLI(t, "audit", "--repo", nested, "--fail-on", "warning"); code != cli.ExitFindings {
		t.Errorf("--fail-on warning exits %d, want %d: the nested file is still rot", code, cli.ExitFindings)
	}

	// The neighbour this must not break: docs/CODEOWNERS beside
	// .github/CODEOWNERS is the original A-10 and stays an error.
	rootLevel := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"docs/CODEOWNERS":    "* @org/docs\n",
		"a.go":               "",
	})
	code, rep = auditJSON(t, rootLevel)
	got = rep.forCheck("A-10")
	if len(got) != 1 || !strings.HasPrefix(got[0], "error: ") {
		t.Fatalf("docs/CODEOWNERS beside .github/CODEOWNERS: want one A-10 at severity error, got %v", got)
	}
	if !strings.Contains(got[0], "docs/CODEOWNERS") {
		t.Errorf("the root-level A-10 stopped naming docs/CODEOWNERS: %q", got[0])
	}
	if code, _, _ := runCLI(t, "audit", "--repo", rootLevel, "--fail-on", "error"); code != cli.ExitFindings {
		t.Errorf("--fail-on error exits %d on two root-level CODEOWNERS files; that gate is what A-10 was "+
			"severity error for", code)
	}
}

// The control for the check above: a repository whose only CODEOWNERS is the
// governing one reports no A-10 at all. A scan that counted the governing file
// itself, or any nearby ownership file, would make every healthy monorepo in a
// fleet report rot it does not have.
func TestAuditReportsNoA10WhenTheOnlyCodeownersIsTheGoverningOne(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":           "* @org/every\n",
		"packages/foo/OWNERS":          "@org/legacy\n", // a Bazel OWNERS file is not a CODEOWNERS
		"packages/foo/CODEOWNERS.bak":  "* @org/old\n",  // nor is a backup of one
		"packages/foo/docs/codeowners": "",              // nor is a lowercase namesake
		"packages/foo/a.go":            "",
	})
	code, rep := auditJSON(t, repo)
	if got := rep.forCheck("A-10"); len(got) != 0 {
		t.Errorf("A-10 fired on a repo with exactly one CODEOWNERS: %v", got)
	}
	if code != cli.ExitOK {
		t.Errorf("audit exits %d on a clean single-CODEOWNERS repo, want 0", code)
	}
}

// A symlinked governing CODEOWNERS is severity `error`: GitHub does not follow
// it, so no rule in the repository takes effect and every path is unowned.
// That is A-12-over-the-cliff's situation — "ownership is silently off" — and
// it has to fail `--fail-on error`, the gate a fleet blocks rollouts on.
//
// The content-derived checks go with it, because `cat-file` hands back the link
// TARGET: A-4 was reporting "../OWNERS.real" as a dead pattern, and A-9/A-11
// were describing a document that does not exist.
func TestAuditSymlinkedCodeownersIsAnErrorAndSuppressesTheBogusParse(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"OWNERS.real":          "* @org/everyone\n",
		"services/api/main.go": "",
	})
	if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../OWNERS.real", filepath.Join(repo, ".github/CODEOWNERS")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "symlink the codeowners")

	_, rep := auditJSON(t, repo)
	got := rep.forCheck("A-10")
	if len(got) != 1 || !strings.HasPrefix(got[0], "error: ") {
		t.Fatalf("symlinked CODEOWNERS: want one A-10 at severity error, got %v (all findings: %+v)", got, rep.Findings)
	}
	if !strings.Contains(got[0], "symlink") || !strings.Contains(got[0], "OWNERS.real") {
		t.Errorf("the finding does not name the link or its target the way `sync` does: %q", got[0])
	}
	for _, f := range rep.Findings {
		if f.Check != "A-10" {
			t.Errorf("[%s/%s] %s — a check computed from the link target's bytes, which are not a CODEOWNERS document",
				f.Check, f.Severity, f.Message)
		}
	}
	if code, _, _ := runCLI(t, "audit", "--repo", repo, "--fail-on", "error"); code != cli.ExitFindings {
		t.Errorf("--fail-on error exits %d while the whole repository is unowned", code)
	}
	// A subset that excludes A-10 must not come back "clean" over a file whose
	// content is a link target: the requested check could not be run (R-12).
	if code, _, _ := runCLI(t, "audit", "--repo", repo, "--checks", "a4"); code != cli.ExitInconclusive {
		t.Errorf("`audit --checks a4` on a symlinked CODEOWNERS exits %d; a check that could not run is "+
			"exit 5, never a vacuous pass", code)
	}
}

// The neighbour: an ordinary CODEOWNERS is never called a symlink, and its
// content checks still run. A mode test that fired on a regular blob would
// silence every finding in every repository.
func TestAuditLeavesAPlainCodeownersAlone(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n/future/ @org/later\n",
		"a.go":               "",
	})
	_, rep := auditJSON(t, repo)
	for _, f := range rep.Findings {
		if strings.Contains(f.Message, "symlink") {
			t.Errorf("a regular CODEOWNERS blob was reported as a symlink: %q", f.Message)
		}
	}
	if got := rep.forCheck("A-4"); len(got) != 1 {
		t.Fatalf("the content checks stopped running over a regular file: A-4 findings = %v", got)
	}
}

// A dead pattern that differs from a tracked path ONLY by Unicode
// normalization is diagnosed under A-5 — the check that already exists for the
// other invisible spelling mismatch — and names both the tracked path it
// collides with and the codepoints, since the two strings render identically.
func TestAuditNormalizationMismatchIsDiagnosedUnderA5(t *testing.T) {
	const nfd = "docs/réunion/notes.md" // e + combining acute, as macOS stores it
	const nfc = "/docs/réunion/"         // precomposed é, as the pattern is typed
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @org/everyone\n" + nfc + " @org/docs-team\n",
		nfd:          "",
	})
	_, rep := auditJSON(t, repo)
	got := rep.forCheck("A-5")
	if len(got) != 1 {
		t.Fatalf("want one A-5 for the normalization mismatch, got %v (all findings: %+v)", got, rep.Findings)
	}
	if !strings.HasPrefix(got[0], "warning: ") {
		t.Errorf("A-5 severity changed: %q", got[0])
	}
	if !strings.Contains(strings.ToLower(got[0]), "normaliz") {
		t.Errorf("the finding does not name normalization: %q", got[0])
	}
	if !strings.Contains(got[0], nfd) {
		t.Errorf("the finding does not name the tracked path the pattern collides with: %q", got[0])
	}
	// The two strings render identically, so a message that only quotes them
	// tells the reader nothing they can act on.
	if !strings.Contains(got[0], "U+00E9") || !strings.Contains(got[0], "U+0301") {
		t.Errorf("the finding quotes two identical-looking strings and no codepoints: %q", got[0])
	}
	if got := rep.forCheck("A-4"); len(got) != 0 {
		t.Errorf("the generic A-4 fired as well as A-5: %v", got)
	}
}

// The neighbours the normalization branch must not swallow: a genuine
// case-only miss is still reported AS a case miss, an ordinary forward-looking
// dead pattern is still report-only A-4, and two names that differ by a real
// accent are still two different names.
func TestAuditKeepsCaseAndDeliberateDeadPatternsDistinctFromNormalization(t *testing.T) {
	t.Run("case-only stays A-5 case", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS": "* @org/every\n/Src/ @org/src\n",
			"src/a.go":   "",
		})
		_, rep := auditJSON(t, repo)
		got := rep.forCheck("A-5")
		if len(got) != 1 {
			t.Fatalf("want one A-5, got %v", got)
		}
		if !strings.Contains(got[0], "case") || !strings.Contains(got[0], "S-6") {
			t.Errorf("the case finding lost its wording: %q", got[0])
		}
		if strings.Contains(strings.ToLower(got[0]), "normaliz") {
			t.Errorf("a case-only miss was reported as a normalization mismatch: %q", got[0])
		}
	})

	t.Run("deliberate dead pattern stays A-4", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS": "* @org/every\n/lands-next-week/ @org/team\n",
			"a.go":       "",
		})
		_, rep := auditJSON(t, repo)
		got := rep.forCheck("A-4")
		if len(got) != 1 || !strings.Contains(got[0], "may be deliberate") {
			t.Fatalf("want the report-only A-4 (R-11), got %v", got)
		}
		if n := len(rep.forCheck("A-5")); n != 0 {
			t.Errorf("A-5 fired on a plain dead pattern: %v", rep.forCheck("A-5"))
		}
	})

	// Precision: claiming "these differ only by normalization" over é vs è
	// would be a false diagnosis, and the operator would retype a pattern that
	// still matches nothing.
	t.Run("a different accent is not a normalization mismatch", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS":             "* @org/every\n/docs/réunion/ @org/docs\n", // acute, composed
			"docs/rèunion/notes.md": "",                                         // grave, decomposed
		})
		_, rep := auditJSON(t, repo)
		if n := len(rep.forCheck("A-5")); n != 0 {
			t.Errorf("é vs è reported as a normalization mismatch: %v", rep.forCheck("A-5"))
		}
		if got := rep.forCheck("A-4"); len(got) != 1 {
			t.Errorf("want the generic A-4 for two genuinely different names, got %v", got)
		}
	})
}
