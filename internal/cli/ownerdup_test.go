// R-33c end to end: an owner named twice in one list is a fact about the op
// string, so every command that reads that string refuses it — before any
// repository is opened.
//
// Adversarial-review finding: only add_owner and remove_owner routed through
// the owner-list grammar. set_owners had a hand-rolled loop with no duplicate
// check, so the defect passed `check` at exit 0 and reached disk, and the
// duplicate line then made `verify` report an invariant violation over a
// semantic no-op.
//
// Vacuity, per CONTRIBUTING.md: no test name here contains a fragment any
// assertion looks for.
package cli_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

func TestR33c_OneOwnerNamedTwiceNeverReachesDisk(t *testing.T) {
	cases := map[string]string{
		"set_owners":              "set_owners(/x/, [@org/a, @org/a])",
		"set_owners case variant": "set_owners(/x/, [@org/a, @ORG/A])",
		"add_owner case variant":  "add_owner(/x/, [@Org/Team, @org/team])",
	}
	for name, op := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, errOut := runCLI(t, "check", "--op", op)
			if code != cli.ExitInvalid {
				t.Errorf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			repo := initRepo(t, map[string]string{
				"CODEOWNERS": "/x/ @org/a\n",
				"x/main.go":  "package x\n",
			})
			snap := checkDirSnapshot(t, repo)
			code, _, errOut = runCLI(t, "sync", "--repo", repo, "--op", op)
			if code != cli.ExitInvalid {
				t.Errorf("sync: want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
				t.Errorf("an invalid op must write NOTHING; %s now reads:\n%s",
					filepath.Join(repo, "CODEOWNERS"), after["CODEOWNERS"])
			}
		})
	}
}
