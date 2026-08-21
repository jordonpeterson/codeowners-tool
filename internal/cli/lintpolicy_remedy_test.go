// Adversarial-review findings against `lint --policy` (R-36), each one a
// mismatch between what the run refuses and what it tells the operator to do
// about it.
//
// Both tests here assert EXACT stderr rather than a fragment, because both
// claims are about wording that another package produces. A `strings.Contains`
// on "on_empty" would go green on the very message the finding is about, since
// the flag-mode sentence names the field too.
//
// The control in TestR36a_TheR6RemedyNamesTheBlockUnderPolicy is load-bearing:
// the policy-mode sentence is built by rewriting the flag-mode one, so a
// reworded refusal in internal/lint would silently stop matching and hand the
// operator back the illegal advice with nothing failing. Pinning the flag-mode
// bytes is what turns that drift into a red test.
package cli_test

import (
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// The two spellings of the R-6 refusal. The flag-mode one is today's, byte for
// byte: `lint` without --policy still has --on-empty, so its advice is correct
// there and must not move.
const (
	plR6FlagMode = "error: removing @org/gone empties the owner set of \"/x/\"; " +
		"an explicit --on-empty policy (error|inherit|unowned) is required — there is deliberately no default (R-6)\n"
	plR6PolicyMode = "error: removing @org/gone empties the owner set of \"/x/\"; " +
		"an explicit R-6 policy is required: set \"on_empty\" (error|inherit|unowned) in the policy file's \"lint\" block " +
		"— there is deliberately no default (R-6)\n"
)

// SPEC R-36a/R-36b/R-6: under `--policy`, the refusal for an unset R-6 policy
// must name the policy file's "lint" block, because the flag it named instead
// is exit-3 illegal in that mode (R-36b). An operator who did as they were told
// got a second refusal — "--on-empty is not allowed with --policy" — at the
// moment they could least tell a broken policy from an awkward repository.
//
// `sync`'s noCodeownersError is the precedent this mirrors: one refusal, two
// remedies, chosen by whether a policy drove the run.
func TestR36a_TheR6RemedyNamesTheBlockUnderPolicy(t *testing.T) {
	api := lintAPI(t, nil, "org/live")

	t.Run("policy run is told to set the field", func(t *testing.T) {
		repo := plEmptyOwnerRepo(t)
		before := lintRead(t, lintOwnersPath(repo))
		pol := plPolicy(t, `{"version":1,"lint":{},"ops":["add_owner(/x/, @org/live)"]}`)
		code, out, errb := lintVerbRun(t, repo, api, "--policy", pol)
		if code != cli.ExitInvalid {
			t.Fatalf("want exit 3, got %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		if errb != plR6PolicyMode {
			t.Errorf("stderr:\n%q\nwant:\n%q", errb, plR6PolicyMode)
		}
		if strings.Contains(errb, "--on-empty") {
			t.Errorf("the remedy names a flag this mode refuses (R-36b):\n%s", errb)
		}
		lintUnchanged(t, "R-6 refusal under --policy", lintOwnersPath(repo), before)
	})

	// Not a restatement of the case above: it is the anchor that keeps the
	// rewrite matching. It also holds `audit --lint` and flag-mode `lint`
	// unchanged, which is the whole reason the fork exists rather than one new
	// sentence for both modes.
	t.Run("control: without a policy the flag advice is unchanged", func(t *testing.T) {
		repo := plEmptyOwnerRepo(t)
		code, out, errb := lintVerbRun(t, repo, api)
		if code != cli.ExitInvalid {
			t.Fatalf("want exit 3, got %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		if errb != plR6FlagMode {
			t.Errorf("stderr:\n%q\nwant today's wording, unchanged:\n%q", errb, plR6FlagMode)
		}
	})

	// The finding itself, stated as a test: what the refusal advises has to be
	// something this mode accepts. The old advice is exit 3; the new advice
	// completes the repair.
	t.Run("the advice given is advice that works", func(t *testing.T) {
		repo := plEmptyOwnerRepo(t)
		pol := plPolicy(t, `{"version":1,"lint":{"on_empty":"inherit"},"ops":["add_owner(/x/, @org/live)"]}`)
		code, out, errb := lintVerbRun(t, repo, api, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("the remedy the refusal names: want exit 0, got %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		if got := lintRead(t, lintOwnersPath(repo)); got != "* @org/live\n" {
			t.Errorf("CODEOWNERS = %q, want the emptied rule deleted", got)
		}

		flagRepo := plEmptyOwnerRepo(t)
		empty := plPolicy(t, `{"version":1,"lint":{},"ops":["add_owner(/x/, @org/live)"]}`)
		code, _, errb = lintVerbRun(t, flagRepo, api, "--policy", empty, "--on-empty", "inherit")
		if code != cli.ExitInvalid || !strings.Contains(errb, "not allowed with --policy") {
			t.Fatalf("the OLD advice must still be refused, or this finding is about nothing: exit %d\nstderr: %s", code, errb)
		}
	})
}

// SPEC R-36e/R-36b: a policy defect outranks the ban on a flag that sits next
// to --policy, exactly as it does under `sync` (commit db7fc71, and the
// precedence pin in TestFleet_BrokenPolicyHaltsOnTheFirstRepo).
//
// The order is the whole point. With the ban first, "the broken policy stopped
// the run" can be satisfied by the flag, with the policy never read at all —
// a vacuous pass that hides a defect riding through the fleet, and an operator
// who drops the flag and re-runs meets a completely different failure.
//
// The control is the other half: the ban must still fire on a policy that
// loads, or "the defect is reported first" would be reachable by removing the
// ban altogether.
func TestR36e_ABrokenPolicyOutranksTheFlagBanUnderLint(t *testing.T) {
	api := lintAPI(t, nil, "org/live")
	// Well-formed `lint` block, so the block is never the reason for exit 3;
	// one misspelled op verb, which is repo-independent and therefore the
	// exit-3 fact this run must report.
	const brokenSrc = `{"version":1,"lint":{"remove_stale_paths":true},"ops":["add_ownr(/x/, @org/live)"]}`

	for _, tc := range []struct {
		name  string
		flags []string
	}{
		{"the boolean repair flag", []string{"--remove-stale-paths"}},
		{"the R-6 flag", []string{"--on-empty", "inherit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := plLintRepo(t)
			before := lintRead(t, lintOwnersPath(repo))
			args := append([]string{"--policy", plPolicy(t, brokenSrc)}, tc.flags...)
			code, out, errb := lintVerbRun(t, repo, api, args...)
			if code != cli.ExitInvalid {
				t.Fatalf("want exit 3, got %d\nstdout: %s\nstderr: %s", code, out, errb)
			}
			// The misspelled verb, not merely "some exit 3": any other
			// exit-3 reason would satisfy a looser assertion while leaving
			// the policy unread.
			if !strings.Contains(errb, "add_ownr") {
				t.Errorf("the flag ban was reported instead of the policy defect, so the policy was never read:\n%s", errb)
			}
			if strings.Contains(errb, "not allowed with --policy") {
				t.Errorf("the flag ban came first:\n%s", errb)
			}
			lintUnchanged(t, "broken policy with a banned flag", lintOwnersPath(repo), before)
		})
	}

	t.Run("control: a policy that loads still bans the flag", func(t *testing.T) {
		repo := plLintRepo(t)
		before := lintRead(t, lintOwnersPath(repo))
		pol := plPolicy(t, `{"version":1,"lint":{"remove_stale_paths":true},"ops":["add_owner(/x/, @org/live)"]}`)
		code, out, errb := lintVerbRun(t, repo, api, "--policy", pol, "--remove-stale-paths")
		if code != cli.ExitInvalid {
			t.Fatalf("want exit 3, got %d\nstdout: %s\nstderr: %s", code, out, errb)
		}
		if !strings.Contains(errb, "not allowed with --policy") {
			t.Errorf("R-36b's ban went missing when the policy loads cleanly:\n%s", errb)
		}
		lintUnchanged(t, "banned flag beside a valid policy", lintOwnersPath(repo), before)
	})
}
