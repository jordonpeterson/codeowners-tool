package cli_test

// The offline tree-only mode of `lint --remove-stale-paths` (UAT finding,
// TestOfflineStaleRuleRemovalReportable).
//
// R-12 makes owner existence — an API fact — undecidable offline, so lint
// without credentials refuses at exit 5. Whether a pattern matches zero
// tracked files is a git-TREE fact (audit's A-4/A-5), so when
// --remove-stale-paths names that repair as what the run is for, the run
// proceeds offline: stage 3 alone, owner checks skipped and DISCLOSED, no
// owner verified, repaired, or removed. Everything else about lint's contract
// holds unchanged — exit 4 for pending or spared work, exit 0 for clean,
// never 1; the A-5 sparing of case-only typos; apply as the single writer.

import (
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// lintOffline invokes the `lint` verb with NO credentials at all: the ambient
// $GITHUB_TOKEN is cleared so the run is genuinely offline, not accidentally
// authenticated by the test environment.
func lintOffline(t *testing.T, repo string, extra ...string) (int, string, string) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	return runCLI(t, append([]string{"lint", "--repo", repo}, extra...)...)
}

// SPEC A-4/R-11 offline: the dry run is the CI-shaped half of the fix — the
// pending dead-rule removal is reported at exit 4, the dead pattern is named,
// nothing is written, and the output says the owner checks were skipped so
// nobody reads the report as "the owners are fine too".
func TestLintOffline_DryRunReportsThePendingRemovalAndWritesNothing(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/ghost/ @org/ghost-team\n",
		"a.md":        "",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)

	code, out, errOut := lintOffline(t, repo, "--dry-run", "--remove-stale-paths")
	if code != cli.ExitFindings {
		t.Fatalf("exit %d, want 4 — a pending removal under --dry-run\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "offline --dry-run", path, before)
	lintMentions(t, "pending removal", out, "/ghost/")
	lintMentions(t, "pending removal", out, "remove-stale-rule")
	lintMentions(t, "the disclosure", out, "owner checks were skipped")
}

// SPEC R-0 offline: the write path works too, through the same apply machinery
// as every other write — and re-running over its own output is a no-op at exit
// 0 (clean is lint's success, never exit 1), so the mode is schedulable.
func TestLintOffline_WriteRemovesTheDeadRuleAndIsIdempotent(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/ghost/ @org/ghost-team\n",
		"a.md":        "",
	})
	path := lintOwnersPath(repo)

	code, out, errOut := lintOffline(t, repo, "--remove-stale-paths")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0 — the removal was computed and written\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if got := lintRead(t, path); got != "* @org/everyone\n" {
		t.Fatalf("CODEOWNERS = %q, want only the dead rule gone", got)
	}
	lintMentions(t, "offline write", out, "owner checks were skipped")

	// Second run: byte-identical file, exit 0, and a headline scoped to what
	// this run actually established (dead patterns — not owners).
	code, out, errOut = lintOffline(t, repo, "--remove-stale-paths")
	if code != cli.ExitOK {
		t.Fatalf("second run: exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "offline idempotence", path, "* @org/everyone\n")
	lintMentions(t, "scoped clean headline", out, "nothing to remove")
}

// SPEC A-5/S-6 offline: a rule that misses ONLY because of case is a typo, not
// a dead rule, and the offline mode spares it exactly as the online mode does
// — deleting it would silently un-own the files it was aimed at. Spared means
// reported, and the run exits 4: a typo still needs a person.
func TestLintOffline_CaseOnlyMissIsSparedNotDeleted(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/Src/ @org/everyone\n",
		"src/a.go":    "package src\n",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)

	code, out, errOut := lintOffline(t, repo, "--remove-stale-paths")
	if code != cli.ExitFindings {
		t.Errorf("exit %d, want 4 (a typo needs a person)\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "offline case-only miss", path, before)
	lintMentions(t, "the sparing", out, "kept-case-mismatch")
}

// SPEC R-12: offline WITHOUT --remove-stale-paths keeps the exit-5 refusal —
// there is no tree-only repair to run, and quietly doing nothing would report
// success over a file full of owners nobody checked. The refusal now names the
// one offline escape, so the operator who only wanted the dead rules gone is
// told the flag instead of being told to find a token.
func TestLintOffline_WithoutRemoveStalePathsStillRefuses(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/ghost/ @org/ghost-team\n",
		"a.md":        "",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)

	code, out, errOut := lintOffline(t, repo, "--dry-run")
	if code != cli.ExitInconclusive {
		t.Errorf("exit %d, want 5 — owner existence is still not decidable offline\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "offline without --remove-stale-paths", path, before)
	lintMentions(t, "the escape hatch", out+errOut, "--remove-stale-paths")
}

// SPEC R-12 offline: the fail-closed contract for OWNERS is untouched. A split
// handle (stage 1's repair) and a dead-looking owner (stage 2's removal) both
// survive the offline run byte-for-byte while the dead PATTERN on another line
// is removed — the run does the tree work without touching a single owner. The
// broken line is REPORTED, not repaired: GitHub is skipping it, which is a
// file fact, so the run exits 4 and names the credentialed run that can fix it.
func TestLintOffline_OwnerRepairsAndRemovalsStayRefused(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/x/ @ org/split\n/ghost/ @org/ghost-team\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)

	code, out, errOut := lintOffline(t, repo, "--remove-stale-paths")
	if code != cli.ExitFindings {
		t.Fatalf("exit %d, want 4 — the broken line GitHub skips still needs a person\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	got := lintRead(t, path)
	if !strings.Contains(got, "/x/ @ org/split") {
		t.Errorf("CODEOWNERS = %q: the split handle was repaired offline — an owner repair R-12 reserves for a run that can verify the result", got)
	}
	if strings.Contains(got, "/ghost/") {
		t.Errorf("CODEOWNERS = %q: the dead pattern survived", got)
	}
	for _, kind := range []string{"repair-owner-spacing", "remove-dead-owner"} {
		if strings.Contains(out, kind) {
			t.Errorf("output records %q on an offline run — no owner may be repaired or removed, or claimed to be:\n%s", kind, out)
		}
	}
	lintMentions(t, "the broken-line report", out, "unrepairable-line")
	lintMentions(t, "the honest remedy", out, "credentialed run")
}

// SPEC offline reporting (docs/LINTING.md's exit table): a line GitHub is
// silently skipping is reported at exit 4 / needs_human even offline —
// "syntactically broken" is a file fact, no API needed — so a CI gate on
// `jq -e .needs_human` cannot go green over broken lines.
func TestLintOffline_InvalidLineFailsTheJSONGate(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/x/ @org/everyone /docs\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)

	code, out, errOut := lintOffline(t, repo, "--dry-run", "--remove-stale-paths", "--format", "json")
	if code != cli.ExitFindings {
		t.Fatalf("exit %d, want 4 — a broken line needs a person, offline or not\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "offline invalid line", path, before)
	doc := lintDecode(t, out)
	if !lintBool(t, doc, "needs_human") {
		t.Error("needs_human is false over a line GitHub is silently skipping — the CI gate goes green over rot")
	}
	if !lintHasKind(lintActionKinds(t, doc), "unrepairable-line") {
		t.Errorf("actions carry no unrepairable-line entry: %s", out)
	}
}

// SPEC --format json offline: the record carries the disclosure as a field, so
// a script consuming a mixed fleet of online and offline records can tell
// which ones say nothing about owners — prose in a note line cannot be jq'd.
func TestLintOffline_JSONRecordCarriesTheDisclosure(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/ghost/ @org/ghost-team\n",
		"a.md":        "",
	})

	code, out, errOut := lintOffline(t, repo, "--dry-run", "--remove-stale-paths", "--format", "json")
	if code != cli.ExitFindings {
		t.Fatalf("exit %d, want 4\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	doc := lintDecode(t, out)
	if !lintBool(t, doc, "owner_checks_skipped") {
		t.Error("owner_checks_skipped is absent or false on an offline record")
	}
	if ec, _ := doc["exit_code"].(float64); int(ec) != code {
		t.Errorf("exit_code = %v, want %d", doc["exit_code"], code)
	}
	kinds := lintActionKinds(t, doc)
	if !lintHasKind(kinds, "remove-stale-rule") {
		t.Errorf("actions = %v, want a remove-stale-rule entry naming the pending removal", kinds)
	}
}

// SPEC R-12: the offline mode engages only when NEITHER credential was
// offered. A run that named a repo or held a token asked for the credentialed
// lint; silently narrowing it to dead patterns would report success over owner
// checks the operator believes ran. Refused at exit 5, naming exactly what is
// absent — never the credential that was supplied.
func TestLintOffline_PartialCredentialsRefuseNamingWhatIsMissing(t *testing.T) {
	newRepo := func(t *testing.T) (string, string, string) {
		repo := initRepo(t, map[string]string{
			lintOwnersRel: "* @org/everyone\n/ghost/ @org/ghost-team\n",
			"a.md":        "",
		})
		path := lintOwnersPath(repo)
		return repo, path, lintRead(t, path)
	}

	t.Run("token but no --github-repo", func(t *testing.T) {
		repo, path, before := newRepo(t)
		code, out, errOut := lintOffline(t, repo, "--token", "t", "--dry-run", "--remove-stale-paths")
		if code != cli.ExitInconclusive {
			t.Fatalf("exit %d, want 5 — a run holding a token wanted the credentialed lint\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		lintUnchanged(t, "token without --github-repo", path, before)
		lintMentions(t, "the missing flag", errOut, "--github-repo")
		if strings.Contains(errOut, "$GITHUB_TOKEN") {
			t.Errorf("stderr asks for a token that was supplied: %q", errOut)
		}
		if strings.Contains(out, "owner checks were skipped") {
			t.Errorf("the run degraded to the tree-only mode with a token in hand:\n%s", out)
		}
	})

	t.Run("--github-repo but no token", func(t *testing.T) {
		repo, path, before := newRepo(t)
		code, out, errOut := lintOffline(t, repo, "--github-repo", "org/repo", "--dry-run", "--remove-stale-paths")
		if code != cli.ExitInconclusive {
			t.Fatalf("exit %d, want 5 — a run that named a repo wanted the credentialed lint\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		lintUnchanged(t, "--github-repo without token", path, before)
		lintMentions(t, "the missing credential", errOut, "token")
		if strings.Contains(out, "owner checks were skipped") {
			t.Errorf("the run degraded to the tree-only mode for a NAMED repo:\n%s", out)
		}
	})
}

// SPEC exit 3: a malformed --github-repo is a misspelled argument the operator
// plainly meant to use, and it is diagnosed with or without a token — before
// the fix, a garbage value with no token slid into the offline mode with the
// flag silently ignored.
func TestLintOffline_MalformedGitHubRepoIsInvalidEvenWithoutAToken(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/ghost/ @org/ghost-team\n",
		"a.md":        "",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)

	code, out, errOut := lintOffline(t, repo, "--github-repo", "not-owner-name", "--dry-run", "--remove-stale-paths")
	if code != cli.ExitInvalid {
		t.Fatalf("exit %d, want 3 — the value the operator typed must not be ignored\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "malformed --github-repo", path, before)
	lintMentions(t, "the diagnosis", errOut, "must be owner/name")
}

// SPEC R-36a offline: `"remove_stale_paths": true` in the policy file's "lint"
// block opts in to the tree-only mode exactly as the flag does — the reviewed
// artifact IS the configuration, so the offline escape must not require a flag
// the same run bans (R-36b).
func TestLintOffline_PolicyRemoveStalePathsEnablesTreeOnlyMode(t *testing.T) {
	pol := plPolicy(t, `{"version":1,"lint":{"remove_stale_paths":true},"ops":["add_owner(/x/, @org/other)"]}`)
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/ghost/ @org/ghost-team\n",
		"a.md":        "",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)

	code, out, errOut := lintOffline(t, repo, "--policy", pol, "--dry-run")
	if code != cli.ExitFindings {
		t.Fatalf("exit %d, want 4 — the policy opted in to the one offline repair\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "offline --policy --dry-run", path, before)
	lintMentions(t, "the pending removal", out, "remove-stale-rule")
	lintMentions(t, "the disclosure", out, "owner checks were skipped")
}

// SPEC R-36b: the offline refusal's escape hatch is worded for how THIS run
// was configured. --remove-stale-paths is exit-3-banned next to --policy, so
// under --policy the remedy is the policy field, not the flag — the old advice
// sent a policy-mode operator straight into a second refusal.
func TestLintOffline_PolicyRefusalNamesThePolicyFieldNotTheBannedFlag(t *testing.T) {
	pol := plPolicy(t, `{"version":1,"lint":{"on_empty":"unowned"},"ops":["add_owner(/x/, @org/other)"]}`)
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/ghost/ @org/ghost-team\n",
		"a.md":        "",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)

	code, out, errOut := lintOffline(t, repo, "--policy", pol, "--dry-run")
	if code != cli.ExitInconclusive {
		t.Fatalf("exit %d, want 5 — the policy did not opt in to the offline repair\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "offline --policy without remove_stale_paths", path, before)
	lintMentions(t, "the policy-mode remedy", errOut, `"remove_stale_paths"`)
	lintMentions(t, "the block it lives in", errOut, `"lint" block`)
	if strings.Contains(errOut, "--remove-stale-paths") {
		t.Errorf("the remedy names a flag this command line refuses at exit 3 (R-36b):\n%s", errOut)
	}
}
