package cli_test

import (
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// SPEC R-36c/R-36e: `ops` stays required — one committed file serves both
// commands — but the refusal must be TRUE of the command that is running.
// "A policy with no ops does nothing on every repo and exits 0 on all of them"
// is a claim about `sync`; the file below fully specifies a `lint` run, and
// telling its author their policy does nothing sends them to add a dummy op
// that `sync` will apply the next time anyone runs it.
//
// Vacuity: the fragments must appear in neither the test name nor the subtest
// names, because t.TempDir puts the test name in the policy path and
// policy.Error renders that path first. Hence "EveryCaller" rather than naming
// either command.
func TestR36c_TheMissingOpsRefusalIsTrueForEveryCaller(t *testing.T) {
	pol := plPolicy(t, `{"version":1,"lint":{"remove_stale_paths":true}}`)

	repo := plLintRepo(t)
	before := lintRead(t, lintOwnersPath(repo))
	code, out, errb := lintVerbRun(t, repo, lintAPI(t, nil, "org/live"), "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("want exit 3, got %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	// The false half of today's sentence.
	if strings.Contains(errb, "does nothing on every repo and exits 0 on all of them") {
		t.Errorf("the refusal is written from the other command's point of view and is false here:\n%s", errb)
	}
	// The true reason, which the operator cannot act on unless it is stated:
	// the pairing is what the requirement protects.
	if !strings.Contains(errb, "sync") {
		t.Errorf("the refusal never names the command the requirement is about:\n%s", errb)
	}
	lintUnchanged(t, "missing ops under a policy-driven repair", lintOwnersPath(repo), before)

	// R-36e's closing promise, and the reason `ops` is not made optional for
	// one caller: one artifact, one verdict.
	if c, _, e := runCLI(t, "check", "--policy", pol); c != cli.ExitInvalid {
		t.Errorf("check: want exit 3, got %d — a file one command calls broken must not be a file another calls fine\nstderr: %s", c, e)
	}
}
