package cli_test

// The offline tree-only mode of `lint --remove-stale-paths` (UAT finding,
// TestKnownBug_OfflineStaleRuleRemovalReportable).
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
// is removed — the run does the tree work without touching a single owner, and
// reports no owner action of any kind.
func TestLintOffline_OwnerRepairsAndRemovalsStayRefused(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "* @org/everyone\n/x/ @ org/split\n/ghost/ @org/ghost-team\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)

	code, out, errOut := lintOffline(t, repo, "--remove-stale-paths")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	got := lintRead(t, path)
	if !strings.Contains(got, "/x/ @ org/split") {
		t.Errorf("CODEOWNERS = %q: the split handle was repaired offline — an owner repair R-12 reserves for a run that can verify the result", got)
	}
	if strings.Contains(got, "/ghost/") {
		t.Errorf("CODEOWNERS = %q: the dead pattern survived", got)
	}
	for _, kind := range []string{"repair-owner-spacing", "remove-dead-owner", "unrepairable-line"} {
		if strings.Contains(out, kind) {
			t.Errorf("output records %q on an offline run — no owner action of any kind may happen or be claimed:\n%s", kind, out)
		}
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
