package action

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	manifest        = "../../action.yml"
	releaseWorkflow = "../../.github/workflows/release.yml"
	installDoc      = "../../docs/INSTALL.md"
)

func repoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

var (
	// A line scan, not a YAML parse: go.mod is dependency-free by policy.
	inputDeclRe = regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]*):\s*$`)
	envMapRe    = regexp.MustCompile(`(INPUT_[A-Z0-9_]+):\s*\$\{\{\s*inputs\.([a-z0-9-]+)\s*\}\}`)
	scriptEnvRe = regexp.MustCompile(`INPUT_[A-Z0-9_]+`)
	topKeyRe    = regexp.MustCompile(`(?m)^[a-z]`)
)

// section returns the block under a top-level key, ending at the next one.
func section(body, key string) string {
	i := strings.Index(body, "\n"+key+":\n")
	if i < 0 {
		return ""
	}
	rest := body[i+len(key)+3:]
	if m := topKeyRe.FindStringIndex(rest); m != nil {
		return rest[:m[0]]
	}
	return rest
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A composite action does NOT get its inputs as INPUT_* environment variables the
// way a JavaScript action does — every one has to be passed explicitly. Forgetting
// the mapping is silent: the script reads an unset variable and takes its default,
// so a workflow setting `version:` gets whatever "unset" means instead of an error.
func TestAction_EveryInputTheScriptReadsIsPassedByTheManifest(t *testing.T) {
	body := repoFile(t, manifest)

	read := map[string]bool{}
	for _, m := range scriptEnvRe.FindAllString(repoFile(t, "./setup.sh"), -1) {
		read[m] = true
	}
	if len(read) == 0 {
		t.Fatal("setup.sh reads no INPUT_* variable at all; this gate's premise is stale, re-read it before deleting")
	}

	passed, declaredFor := map[string]bool{}, map[string]string{}
	for _, m := range envMapRe.FindAllStringSubmatch(body, -1) {
		passed[m[1]] = true
		declaredFor[m[1]] = m[2]
	}
	for name := range read {
		if !passed[name] {
			t.Errorf("setup.sh reads %s but action.yml never maps it from an input.\nA composite action passes nothing implicitly: add `%s: ${{ inputs.<name> }}` to the step's env, or the input is silently always empty.", name, name)
		}
	}
	for name := range passed {
		if !read[name] {
			t.Errorf("action.yml passes %s, which setup.sh never reads — an input the workflow can set and nothing acts on", name)
		}
	}

	declared := map[string]bool{}
	for _, m := range inputDeclRe.FindAllStringSubmatch(section(body, "inputs"), -1) {
		declared[m[1]] = true
	}
	for name, input := range declaredFor {
		if !declared[input] {
			t.Errorf("action.yml passes %s from inputs.%s, but declares no `%s:` under inputs: — an undeclared input is empty at runtime, not an error", name, input, input)
		}
	}
	for input := range declared {
		used := false
		for _, in := range declaredFor {
			if in == input {
				used = true
			}
		}
		if !used {
			t.Errorf("action.yml declares input %q and never passes it to the step; declared inputs %v, passed %v", input, sorted(declared), sorted(passed))
		}
	}
}

// The manifest has to sit at the repository root for `uses: owner/repo@ref` to
// find it, and it has to run the script these tests drive — a manifest that
// inlined its own copy of the logic would be green here and untested in fact.
func TestAction_ManifestRunsTheScriptUnderTest(t *testing.T) {
	body := repoFile(t, manifest)
	if !strings.Contains(body, "using: composite") {
		t.Errorf("action.yml is not a composite action; the steps these tests drive would not run")
	}
	if !strings.Contains(body, "tools/action/setup.sh") {
		t.Errorf("action.yml does not run tools/action/setup.sh, so what it does run is untested")
	}
	// $GITHUB_ACTION_PATH is where the action's own repository was checked out.
	// A path relative to the working directory would resolve inside the CONSUMER's
	// repository, where none of these files exist.
	if !strings.Contains(body, "GITHUB_ACTION_PATH") && !strings.Contains(body, "github.action_path") {
		t.Errorf("action.yml runs the script by a path that is not rooted at the action path.\nThe working directory in a consumer's job is THEIR repository; the script and install.sh are only under $GITHUB_ACTION_PATH.")
	}
}

// The step's only command is `sh <script>`, and the script is POSIX by contract —
// shellcheck gates it as sh in CI. Asking the runner for bash would make the action
// require an interpreter it never uses, and fail outright on the minimal Linux
// images self-hosted runners are often built from, which carry /bin/sh and no bash.
func TestAction_TheStepDoesNotRequireAShellItNeverUses(t *testing.T) {
	body := repoFile(t, manifest)
	shellRe := regexp.MustCompile(`(?m)^\s*shell:\s*["']?([a-z0-9-]+)`)
	found := shellRe.FindAllStringSubmatch(body, -1)
	if len(found) == 0 {
		t.Fatal("action.yml declares no `shell:` at all; a composite run step requires one")
	}
	for _, m := range found {
		if m[1] != "sh" {
			t.Errorf("action.yml runs its step with `shell: %s`, but the step only invokes `sh` and the script it runs is POSIX.\nRequiring %s makes the action fail on a runner image that has /bin/sh and not %s.", m[1], m[1], m[1])
		}
	}
}

// This action runs inside a consumer's job, with the consumer's token. A mutable
// tag here is a supply-chain hazard in someone else's repository, not just ours.
func TestAction_ThirdPartyActionsInTheManifestArePinnedToACommitSHA(t *testing.T) {
	usesRe := regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*["']?([^"'\s#]+)`)
	shaRe := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, m := range usesRe.FindAllStringSubmatch(repoFile(t, manifest), -1) {
		ref := m[1]
		if strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "docker://") {
			continue
		}
		at := strings.LastIndex(ref, "@")
		if at < 0 || !shaRe.MatchString(ref[at+1:]) {
			t.Errorf("action.yml: `uses: %s` is not pinned to a 40-character commit SHA.\nThis manifest executes in the consumer's job with the consumer's token; a moved tag is new code running there.", ref)
		}
	}
}

// `uses: jordonpeterson/codeowners-tool@v0` is the pin the docs recommend, and it
// resolves to whatever commit the v0 tag points at. Nothing moves that tag unless
// the release does, so without this step every consumer on @v0 is frozen at the
// commit it first pointed to.
func TestAction_ReleaseMovesTheMajorTagConsumersPin(t *testing.T) {
	body := repoFile(t, releaseWorkflow)
	if !strings.Contains(body, "git tag -f") && !strings.Contains(body, "git tag --force") {
		t.Errorf("release.yml never force-moves a tag, so the major tag `uses: ...@v0` resolves through is never updated.\nConsumers pinned to it stay on the commit it first pointed at, and every action fix after that reaches nobody.")
	}
	if !strings.Contains(body, "--force") {
		t.Errorf("release.yml moves a tag locally but never force-pushes it; the remote tag is what `uses:` resolves")
	}
}

// The release only runs on paths that change the binary. The action is shipped by
// the same tag and is not one of them, so a fix to the action would sit on main
// with no release to move the major tag onto it.
func TestAction_ReleaseRunsWhenTheActionItselfChanges(t *testing.T) {
	paths := section(repoFile(t, releaseWorkflow), "on")
	if paths == "" {
		t.Fatal("release.yml has no `on:` block; this gate cannot read its triggers")
	}
	for _, want := range []string{"action.yml", "tools/action/setup.sh"} {
		if !strings.Contains(paths, want) {
			t.Errorf("release.yml's paths filter does not include %q, so changing the action cuts no release — and the major tag consumers pin to never moves onto the fix", want)
		}
	}
	// Only the two files that SHIP. A glob over the package would cut a release
	// for a change to its tests, which ship nothing: a new tag, and the major tag
	// consumers pin to advanced onto a commit whose action is byte-identical.
	if strings.Contains(paths, "tools/action/**") {
		t.Errorf("release.yml's paths filter globs tools/action/**, which includes the tests.\nEditing a _test.go file would cut a release and move the tag consumers resolve through, for a commit that ships nothing different.")
	}
}

// A route nobody is told about is a route nobody takes; the CI gate is the
// documented reason this tool exists.
func TestAction_InstallDocShowsTheWorkflowStep(t *testing.T) {
	body := repoFile(t, installDoc)
	if !strings.Contains(body, "uses: jordonpeterson/codeowners-tool@") {
		t.Errorf("docs/INSTALL.md never shows `uses: jordonpeterson/codeowners-tool@...`, so the CI route exists and is undiscoverable")
	}
}
