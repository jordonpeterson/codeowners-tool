package cli_test

// The `lint` verb, and the reporting contract a UX review found the flag
// spelling was getting wrong.
//
// The mode outgrew being a flag on `audit`: six of audit's fourteen flags
// changed meaning or validity on one boolean, and the exit contract had already
// forked. `audit --lint` is kept as an alias and routed to the same code, so
// both spellings are pinned here — a divergence between them would be invisible
// to everyone still scripting the old one.
//
// SPEC R-17: exit 4 means the file still needs a PERSON. That is one rule with
// three causes — fixes computed but not written, a line lint would not guess at,
// and a case-only miss it deliberately spared — and the output has to make the
// nonzero status legible next to a headline that says "applied", or a CI log
// reads green above a red result.
// SPEC S-6: CODEOWNERS is case-sensitive, so a rule that misses only by case is
// a TYPO, not a dead rule. Deleting it would silently un-own the files it was
// aimed at, which is the opposite of what --remove-stale-paths was asked to do.

import (
	"strings"
	"testing"
)

// lintVerbRun invokes the `lint` verb (not `audit --lint`) with credentials
// that satisfy R-12.
func lintVerbRun(t *testing.T, repo, api string, extra ...string) (int, string, string) {
	t.Helper()
	args := []string{
		"lint", "--repo", repo,
		"--github-repo", "org/repo",
		"--token", "lint-test-token",
		"--api-url", api,
	}
	return runCLI(t, append(args, extra...)...)
}

// The `lint` verb does the same work as `audit --lint`, byte for byte.
//
// The alias exists so nothing that already scripts the flag has to change. If
// the two ever diverge, the people who did not migrate are the ones who find
// out, in production, from a diff.
func TestLintVerb_MatchesTheAuditFlagSpelling(t *testing.T) {
	content := "* @org/live\n/x/ @ org/live\n"
	mk := func() string {
		repo := initRepo(t, map[string]string{
			lintOwnersRel: "* @org/live\n",
			"x/a.go":      "package x\n",
		})
		lintWrite(t, lintOwnersPath(repo), content)
		return repo
	}
	api := lintAPI(t, nil, "org/live")

	repoA := mk()
	codeA, outA, _ := lintVerbRun(t, repoA, api)
	repoB := mk()
	codeB, outB, _ := lintRun(t, repoB, api)

	if codeA != codeB {
		t.Errorf("exit differs: `lint` = %d, `audit --lint` = %d", codeA, codeB)
	}
	if outA != outB {
		t.Errorf("stdout differs:\n lint:        %q\n audit --lint: %q", outA, outB)
	}
	if a, b := lintRead(t, lintOwnersPath(repoA)), lintRead(t, lintOwnersPath(repoB)); a != b {
		t.Errorf("written file differs:\n lint:        %q\n audit --lint: %q", a, b)
	}
	if a := lintRead(t, lintOwnersPath(repoA)); a != "* @org/live\n/x/ @org/live\n" {
		t.Errorf("file = %q, want the handle rejoined", a)
	}
}

// The verb's flagset carries only the flags that apply to it.
//
// This is the whole reason it exists. `--checks` and `--cache-dir` are audit's;
// under the flag spelling they had to be rejected at runtime with a message
// each, and under the verb they cannot be typed at all. An unknown flag is a
// parse error, which is exit 3.
func TestLintVerb_RejectsAuditOnlyFlags(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/live\n",
		"x/a.go":      "package x\n",
	})
	api := lintAPI(t, nil, "org/live")
	for _, flag := range []string{"--checks", "--cache-dir", "--cache-ttl"} {
		t.Run(flag, func(t *testing.T) {
			code, _, errb := lintVerbRun(t, repo, api, flag, "x")
			if code != 3 {
				t.Errorf("exit = %d, want 3 for %s on the lint verb; stderr=%q", code, flag, errb)
			}
		})
	}
}

// A missing precondition names the thing that is missing.
//
// The message used to list both requirements whatever you had passed, so an
// operator holding a perfectly good token had to diff the sentence against
// their own command line to find the one word they needed.
func TestLintVerb_PreconditionNamesOnlyWhatIsMissing(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/live\n",
		"x/a.go":      "package x\n",
	})
	t.Setenv("GITHUB_TOKEN", "")
	api := lintAPI(t, nil, "org/live")

	// Token present, repo missing: the message must not ask for a token.
	code, _, errb := runCLI(t, "lint", "--repo", repo, "--token", "t", "--api-url", api)
	if code != 5 {
		t.Fatalf("exit = %d, want 5; stderr=%q", code, errb)
	}
	if !strings.Contains(errb, "--github-repo") {
		t.Errorf("stderr does not name the missing flag: %q", errb)
	}
	if strings.Contains(errb, "$GITHUB_TOKEN") {
		t.Errorf("stderr asks for a token that was supplied: %q", errb)
	}
}

// A rule that misses ONLY because of case is spared by --remove-stale-paths.
//
// `/Src/` matches nothing when the directory is `src/`, so stage 3 saw a dead
// rule and deleted it — destroying the one piece of evidence that a typo ever
// happened, and quietly un-owning `src/` under a message reassuring the reader
// that nothing changed. It is audit's A-5, and A-5's answer is to fix the
// casing, which lint cannot do safely (the tree's real casing may not be the
// naive lowercase). So it is kept, reported, and the run exits 4.
func TestLintVerb_CaseOnlyMissIsSparedNotDeleted(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/live\n",
		"src/a.go":    "package src\n",
	})
	path := lintOwnersPath(repo)
	before := "* @org/live\n/Src/ @org/live\n"
	lintWrite(t, path, before)

	api := lintAPI(t, nil, "org/live")
	code, out, errb := lintVerbRun(t, repo, api, "--remove-stale-paths")
	if code != 4 {
		t.Errorf("exit = %d, want 4 (a typo needs a person); stdout=%q stderr=%q", code, out, errb)
	}
	lintUnchanged(t, "case-only miss", path, before)
	lintMentions(t, "case-only report", out, "kept-case-mismatch")
	lintMentions(t, "the correction", out, "/src/")

	// A genuinely dead rule in the same file is still deleted, so sparing the
	// typo is not a blanket refusal to do the job.
	lintWrite(t, path, "* @org/live\n/Src/ @org/live\n/gone/ @org/live\n")
	if code, out, errb := lintVerbRun(t, repo, api, "--remove-stale-paths"); code != 4 {
		t.Errorf("exit = %d, want 4; stdout=%q stderr=%q", code, out, errb)
	}
	got := lintRead(t, path)
	if strings.Contains(got, "/gone/") {
		t.Errorf("the genuinely dead rule survived: %q", got)
	}
	if !strings.Contains(got, "/Src/") {
		t.Errorf("the case typo was deleted: %q", got)
	}
}

// "lint clean" must not be printed over a file lint would not repair.
//
// The headline came from the change count alone, so a file whose only problems
// were ones lint refuses to guess at printed "lint clean" — and then exited 4.
// A CI log that reads green above a red status is worse than either alone.
// It must also not claim to be clean in the general sense: lint covers three
// things and `audit` runs twelve checks, so a bare "clean" sends somebody away
// from a file `audit` would still flag.
func TestLintVerb_HeadlineNeverContradictsTheExitCode(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/live\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)
	api := lintAPI(t, nil, "org/live")

	t.Run("nothing to repair, but a line needs a person", func(t *testing.T) {
		lintWrite(t, path, "* @org/live\n/x @@@bad\n")
		code, out, _ := lintVerbRun(t, repo, api)
		if code != 4 {
			t.Fatalf("exit = %d, want 4", code)
		}
		if strings.Contains(out, "lint clean") {
			t.Errorf("headline claims the file is clean above exit 4:\n%s", out)
		}
		lintMentions(t, "headline", out, "needs a person")
	})

	t.Run("truly nothing to do scopes its claim", func(t *testing.T) {
		lintWrite(t, path, "* @org/live\n")
		code, out, _ := lintVerbRun(t, repo, api)
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		// It must say what it checked, so nobody reads it as "audit is clean".
		lintMentions(t, "scoped success", out, "run `audit`", "audit for the other")
	})
}

// A successful write that leaves something over says so, and says why the exit
// code is 4.
//
// `lint … && git commit` is the obvious script, and exit 4 after a real write
// breaks it while the headline says "applied". The code is deliberate, so the
// output has to reconcile the two: the fixes ARE written, and the nonzero
// status is about what is left, not about the write.
func TestLintVerb_WriteThatLeavesSomethingOverExplainsTheExitCode(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/live\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)
	lintWrite(t, path, "* @org/live\n/x/ @ org/live\n/y @@@bad\n")

	api := lintAPI(t, nil, "org/live")
	code, out, errb := lintVerbRun(t, repo, api)
	if code != 4 {
		t.Fatalf("exit = %d, want 4; stderr=%q", code, errb)
	}
	if got := lintRead(t, path); !strings.Contains(got, "/x/ @org/live") {
		t.Errorf("the repair was not written: %q", got)
	}
	lintMentions(t, "write confirmation", out, "the file was written")
	lintMentions(t, "exit-code explanation", out, "not a failed write")
}

// The JSON record answers "did this need a person?" in one field.
//
// A reviewer writing the obvious gate — `jq '.changes|length > 0'` — went green
// over a file with an unrepairable line, because the correct expression also
// has to know the literal action-kind strings. needs_human and exit_code come
// from the same function that produces the process status, so they cannot drift
// from it.
func TestLintVerb_JSONCarriesTheGateDirectly(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/live\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)
	api := lintAPI(t, nil, "org/live")

	// Nothing to change, but a line nobody can repair: changes is empty and the
	// gate must still fire.
	lintWrite(t, path, "* @org/live\n/x @@@bad\n")
	code, out, _ := lintVerbRun(t, repo, api, "--format", "json", "--dry-run")
	doc := lintDecode(t, out)
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	if needs, _ := doc["needs_human"].(bool); !needs {
		t.Errorf("needs_human = %v, want true — `jq -e .needs_human` is the documented gate; record=%v", doc["needs_human"], doc)
	}
	if ec, _ := doc["exit_code"].(float64); int(ec) != code {
		t.Errorf("exit_code = %v, want %d — it must not drift from the process status", doc["exit_code"], code)
	}
	if ch, ok := doc["changes"].([]any); ok && len(ch) != 0 {
		t.Errorf("changes = %v, want empty — this case is exactly the one a changes-only gate misses", ch)
	}

	// And a genuinely clean file must not trip the gate.
	lintWrite(t, path, "* @org/live\n")
	code, out, _ = lintVerbRun(t, repo, api, "--format", "json", "--dry-run")
	doc = lintDecode(t, out)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if needs, _ := doc["needs_human"].(bool); needs {
		t.Errorf("needs_human = true on a clean file; record=%v", doc)
	}
}
