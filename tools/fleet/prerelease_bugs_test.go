// Regression guard from the pre-release review of co-own.sh: the test began
// life as a failing repro of a confirmed bug and now pins the fixed behavior.
package fleet

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runScriptScoped is runScript with a caller-chosen scope, for tests about
// scope spelling rather than the fleet rollout shape.
func runScriptScoped(t *testing.T, repo, scope, owner string) (result, int) {
	t.Helper()
	script, err := filepath.Abs("co-own.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, "--repo", repo, "--scope", scope, "--owner", owner)
	cmd.Env = append(os.Environ(), "CODEOWNERS_TOOL="+toolPath,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	stdout, runErr := cmd.Output()
	code := 0
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("run co-own.sh: %v", runErr)
		}
		code = ee.ExitCode()
		t.Logf("stderr:\n%s", ee.Stderr)
	}
	var res result
	if line := strings.TrimSpace(string(stdout)); line != "" {
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			t.Fatalf("stdout is not one JSON object: %q (%v)", line, err)
		}
	}
	return res, code
}

// Pre-release finding, fixed: co-own.sh strips the anchoring slash from a single-segment scope
// (`/docs/` becomes the unanchored `docs`, which matches at any depth), so
// both the op it runs and the `verify --scope` that is supposed to prove the
// edit use a broader scope than the operator typed. Ownership changes outside
// the declared scope, and verify blesses it as "all within scope".
func TestCoOwnAnchoredScopeStaysAnchored(t *testing.T) {
	needBinaries(t)
	repo := mkRepo(t,
		"* @org/broad\n/sub/docs/ @org/other\n/docs/ @org/platform\n",
		map[string]string{"docs/a.md": "a\n", "sub/docs/b.md": "b\n"},
	)
	before := owners(t, repo, "sub/docs/b.md")

	res, code := runScriptScoped(t, repo, "/docs/", "@org/platform")
	if code != 0 {
		t.Fatalf("co-own.sh: exit %d, status %q", code, res.Status)
	}

	git(t, repo, "checkout", "-q", res.Branch)
	after := owners(t, repo, "sub/docs/b.md")
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Errorf("scope /docs/ is anchored to the root, but sub/docs/b.md changed owners: %v -> %v", before, after)
	}
}
