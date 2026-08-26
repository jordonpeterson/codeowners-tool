// Package action tests the composite GitHub Action at the repository root, whose
// logic lives in tools/action/setup.sh.
//
// The action exists because the documented use of this tool is a CI gate — `sync
// --dry-run` exiting 4, `lint` in a pipeline, `audit` across a fleet — and until
// now nothing said how the binary gets onto the runner. The two routes that
// existed both misfit: `go install` builds a Go toolchain to compile a
// dependency-free binary that was already built six ways at release time, and a
// hand-rolled `curl | sh` step verifies provenance only on a runner that happens
// to have gh signed in.
//
// setup.sh does NOT download or verify anything itself. install.sh already does
// that, it is already shellcheck'd and gated by tools/supplychain, and a second
// implementation of the same download would be a second supply-chain surface to
// audit. setup.sh resolves which release to ask for and hands off. That handoff
// is the seam these tests drive: GITHUB_ACTION_PATH points at a temporary
// directory holding a stub install.sh, which records the environment it was
// invoked with and plants a binary reporting a version of the test's choosing.
package action

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// setupScript is relative to this package: go test runs with the package
// directory as the working directory.
const setupScript = "./setup.sh"

// run is one invocation of setup.sh. Every field maps to something the composite
// action in action.yml passes down, so a test reads as the workflow that would
// produce it.
type run struct {
	version     string // inputs.version
	provenance  string // inputs.provenance
	installDir  string // inputs.install-dir
	actionRef   string // github.action_ref — the ref the action was pinned to
	runnerOS    string // RUNNER_OS, as the runner sets it
	stubVersion string // what the planted binary answers to `version`
	stubExit    int    // stub install.sh exit status; 0 unless the test wants a failed install
	withGH      bool   // whether a gh that can resolve the latest release is on PATH
	ghTag       string // the tag that stub gh reports as latest
}

// result is what the runner would observe afterwards: the action's outputs, the
// PATH it exported, and the environment install.sh was actually handed.
type result struct {
	exitCode   int
	output     string            // combined stdout+stderr
	outputs    map[string]string // parsed $GITHUB_OUTPUT
	pathAdds   []string          // lines appended to $GITHUB_PATH
	installed  map[string]string // environment the stub install.sh recorded
	ranInstall bool
}

func (r result) String() string { return r.output }

// stubInstall writes the stand-in for install.sh: it records its environment and
// plants a binary that answers `version`, which is all setup.sh depends on.
func stubInstall(t *testing.T, dir, record, stubVersion string, exit int) {
	t.Helper()
	script := `#!/bin/sh
set -eu
{
  echo "VERSION=${VERSION-<unset>}"
  echo "BINDIR=${BINDIR-<unset>}"
  echo "PROVENANCE=${PROVENANCE-<unset>}"
} > "` + record + `"
if [ ` + strconv.Itoa(exit) + ` -ne 0 ]; then
  echo "install.sh: stub failure" >&2
  exit ` + strconv.Itoa(exit) + `
fi
mkdir -p "$BINDIR"
cat > "$BINDIR/codeowners-tool" <<'BIN'
#!/bin/sh
[ "$1" = version ] || exit 1
echo "` + stubVersion + `"
BIN
chmod +x "$BINDIR/codeowners-tool"
`
	writeExec(t, filepath.Join(dir, "install.sh"), script)
}

// stubGH stands in for the GitHub CLI. setup.sh asks it for the latest release
// rather than curling api.github.com anonymously, because the anonymous limit is
// 60/hour per IP and hosted runners share addresses.
func stubGH(t *testing.T, dir, tag string) {
	t.Helper()
	writeExec(t, filepath.Join(dir, "gh"), `#!/bin/sh
case "$1 $2" in
"auth status") exit 0 ;;
"release view") echo "`+tag+`" ;;
*) exit 1 ;;
esac
`)
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (c run) exec(t *testing.T) result {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no POSIX shell on PATH: %v", err)
	}
	root := t.TempDir() // stands in for GITHUB_ACTION_PATH
	work := t.TempDir() // stands in for RUNNER_TEMP
	record := filepath.Join(root, "install-env")
	stubVersion := c.stubVersion
	if stubVersion == "" {
		stubVersion = "v0.0.28"
	}
	stubInstall(t, root, record, stubVersion, c.stubExit)

	binDir := t.TempDir() // holds only the stubs setup.sh is allowed to find
	if c.withGH {
		tag := c.ghTag
		if tag == "" {
			tag = "v0.0.28"
		}
		stubGH(t, binDir, tag)
	}

	ghPath := filepath.Join(work, "github-path")
	ghOutput := filepath.Join(work, "github-output")
	for _, f := range []string{ghPath, ghOutput} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runnerOS := c.runnerOS
	if runnerOS == "" {
		runnerOS = "Linux"
	}
	cmd := exec.Command("sh", setupScript)
	// A pruned PATH: setup.sh must not reach a real gh on the machine running
	// the tests, or "no gh here" cases would pass for the wrong reason.
	cmd.Env = []string{
		"PATH=" + binDir + ":/usr/bin:/bin",
		"HOME=" + work,
		"INPUT_VERSION=" + c.version,
		"INPUT_PROVENANCE=" + c.provenance,
		"INPUT_INSTALL_DIR=" + c.installDir,
		"GITHUB_ACTION_PATH=" + root,
		"GITHUB_ACTION_REF=" + c.actionRef,
		"GITHUB_PATH=" + ghPath,
		"GITHUB_OUTPUT=" + ghOutput,
		"RUNNER_TEMP=" + work,
		"RUNNER_OS=" + runnerOS,
	}
	out, err := cmd.CombinedOutput()
	res := result{output: string(out), outputs: map[string]string{}, installed: map[string]string{}}
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run setup.sh: %v\n%s", err, out)
		}
		res.exitCode = ee.ExitCode()
	}
	res.outputs = parseKV(t, ghOutput)
	res.pathAdds = nonEmptyLines(readFile(t, ghPath))
	if b, err := os.ReadFile(record); err == nil {
		res.ranInstall = true
		for k, v := range parseString(string(b)) {
			res.installed[k] = v
		}
	}
	return res
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func parseKV(t *testing.T, path string) map[string]string {
	t.Helper()
	return parseString(readFile(t, path))
}

func parseString(body string) map[string]string {
	out := map[string]string{}
	for _, l := range nonEmptyLines(body) {
		if k, v, ok := strings.Cut(l, "="); ok {
			out[k] = v
		}
	}
	return out
}

func nonEmptyLines(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
