package action

import (
	"path/filepath"
	"strings"
	"testing"
)

// The ordinary case, and the two facts a workflow author depends on afterwards:
// the binary is on PATH for every later step, and the action says which build it
// installed.
func TestAction_InstallsTheRequestedReleaseAndPutsItOnPATH(t *testing.T) {
	r := run{version: "v0.0.9", stubVersion: "v0.0.9"}.exec(t)
	if r.exitCode != 0 {
		t.Fatalf("setup.sh exited %d, want 0\n%s", r.exitCode, r)
	}
	if got := r.installed["VERSION"]; got != "v0.0.9" {
		t.Errorf("install.sh got VERSION=%q, want v0.0.9\n%s", got, r)
	}
	if got := r.outputs["version"]; got != "v0.0.9" {
		t.Errorf("output version=%q, want v0.0.9\n%s", got, r)
	}
	dir := r.installed["BINDIR"]
	if want := filepath.Join(dir, "codeowners-tool"); r.outputs["path"] != want {
		t.Errorf("output path=%q, want %q\n%s", r.outputs["path"], want, r)
	}
	if len(r.pathAdds) != 1 || r.pathAdds[0] != dir {
		t.Errorf("$GITHUB_PATH got %v, want exactly [%s] — without it every later step in the job has to spell out the full path", r.pathAdds, dir)
	}
}

// The trap this exists to close: the action and the tool ship from the SAME
// repository, so `uses: jordonpeterson/codeowners-tool@v0.0.9` reads as a pin.
// Resolving the download to "latest" regardless would hand that workflow a
// different build on any day a release lands — a pin that silently isn't one is
// worse than no pin, because it is the version people write down in an incident.
func TestAction_ADefaultedVersionFollowsThePinnedActionTag(t *testing.T) {
	r := run{actionRef: "v0.0.9", stubVersion: "v0.0.9"}.exec(t)
	if r.exitCode != 0 {
		t.Fatalf("setup.sh exited %d, want 0\n%s", r.exitCode, r)
	}
	if got := r.installed["VERSION"]; got != "v0.0.9" {
		t.Errorf("action pinned at v0.0.9 installed VERSION=%q; a full-version action tag must name the release it downloads", got)
	}
}

// The action ref is a default, not an override: a workflow that pins the action
// at one tag and asks for another version means the version it asked for.
func TestAction_AnExplicitVersionOverridesTheActionTag(t *testing.T) {
	r := run{version: "v0.0.5", actionRef: "v0.0.9", stubVersion: "v0.0.5"}.exec(t)
	if r.exitCode != 0 {
		t.Fatalf("setup.sh exited %d, want 0\n%s", r.exitCode, r)
	}
	if got := r.installed["VERSION"]; got != "v0.0.5" {
		t.Errorf("install.sh got VERSION=%q, want the explicitly requested v0.0.5", got)
	}
}

// `uses: ...@v0` is the recommended pin and names no release, so the tag has to
// be resolved. Doing it through gh rather than an anonymous api.github.com call
// matters on hosted runners: the anonymous limit is 60/hour per IP and runners
// share addresses, so the resolution that only ever runs in CI is exactly the
// one that must be authenticated.
func TestAction_AMajorActionTagResolvesTheLatestReleaseThroughGH(t *testing.T) {
	r := run{actionRef: "v0", withGH: true, ghTag: "v0.0.28", stubVersion: "v0.0.28"}.exec(t)
	if r.exitCode != 0 {
		t.Fatalf("setup.sh exited %d, want 0\n%s", r.exitCode, r)
	}
	if got := r.installed["VERSION"]; got != "v0.0.28" {
		t.Errorf("install.sh got VERSION=%q, want the v0.0.28 gh reported as latest", got)
	}
}

// gh is on every hosted runner but not on every self-hosted one, and its absence
// must not be fatal: install.sh resolves "latest" on its own. Passing no VERSION
// is how setup.sh says "you resolve it".
func TestAction_WithoutGHTheLatestResolutionIsLeftToTheInstallScript(t *testing.T) {
	r := run{version: "latest", stubVersion: "v0.0.28"}.exec(t)
	if r.exitCode != 0 {
		t.Fatalf("setup.sh exited %d, want 0\n%s", r.exitCode, r)
	}
	if got := r.installed["VERSION"]; got != "<unset>" {
		t.Errorf("install.sh got VERSION=%q; with no gh to resolve the tag, setup.sh must pass none and let install.sh do it", got)
	}
	// The build is still reported, because the binary itself is asked.
	if got := r.outputs["version"]; got != "v0.0.28" {
		t.Errorf("output version=%q, want v0.0.28 read back from the installed binary", got)
	}
}

// A version input reaches install.sh as a release tag, and a release tag here is
// vMAJOR.MINOR.PATCH. Catching a malformed one before the download makes the
// error say what is wrong with the workflow, instead of a 404 on an asset URL.
func TestAction_RefusesAVersionThatIsNotAReleaseTag(t *testing.T) {
	for _, bad := range []string{"0.0", "v0.0", "main", "v0.0.9-rc1", "latest; rm -rf /"} {
		r := run{version: bad}.exec(t)
		if r.exitCode == 0 {
			t.Errorf("version %q was accepted; it is not a release tag", bad)
		}
		if r.ranInstall {
			t.Errorf("version %q reached install.sh; a malformed input must fail before anything is downloaded", bad)
		}
	}
}

// A version may be written the way people say it out loud.
func TestAction_AcceptsAVersionWithoutItsLeadingV(t *testing.T) {
	r := run{version: "0.0.9", stubVersion: "v0.0.9"}.exec(t)
	if r.exitCode != 0 {
		t.Fatalf("setup.sh exited %d, want 0\n%s", r.exitCode, r)
	}
	if got := r.installed["VERSION"]; got != "v0.0.9" {
		t.Errorf("install.sh got VERSION=%q, want v0.0.9", got)
	}
}

// install.sh treats an unknown PROVENANCE as fatal, but only after the download
// and the checksum have already run. Rejecting it here keeps a typo'd mode from
// being reported as a failure of the install.
func TestAction_RefusesAnUnknownProvenanceMode(t *testing.T) {
	r := run{version: "v0.0.9", provenance: "yes"}.exec(t)
	if r.exitCode == 0 {
		t.Fatalf("provenance mode \"yes\" was accepted\n%s", r)
	}
	if r.ranInstall {
		t.Error("an unknown provenance mode reached install.sh instead of failing first")
	}
	if !strings.Contains(r.output, "auto") || !strings.Contains(r.output, "require") || !strings.Contains(r.output, "skip") {
		t.Errorf("the error does not list the modes that would work:\n%s", r)
	}
}

// The default has to match install.sh's own, and `require` has to reach it:
// a fleet that mandates verified provenance sets it once in the workflow.
func TestAction_PassesTheProvenanceModeToTheInstallScript(t *testing.T) {
	if got := (run{version: "v0.0.9"}).exec(t).installed["PROVENANCE"]; got != "auto" {
		t.Errorf("default PROVENANCE=%q, want auto — the mode install.sh documents", got)
	}
	if got := (run{version: "v0.0.9", provenance: "require"}).exec(t).installed["PROVENANCE"]; got != "require" {
		t.Errorf("PROVENANCE=%q, want require passed through", got)
	}
}

// The whole point of naming a version is that the job runs that build. A release
// whose binary was stamped wrong, or an install that quietly found an older
// binary already in the target directory, would otherwise be invisible: the job
// goes green having tested something other than what it pinned.
func TestAction_FailsWhenTheInstalledBuildIsNotTheRequestedOne(t *testing.T) {
	r := run{version: "v0.0.9", stubVersion: "v0.0.1"}.exec(t)
	if r.exitCode == 0 {
		t.Fatalf("setup.sh accepted a binary reporting v0.0.1 for a requested v0.0.9\n%s", r)
	}
	if len(r.pathAdds) != 0 {
		t.Errorf("$GITHUB_PATH got %v; a build that is not the requested one must not be exported to later steps", r.pathAdds)
	}
}

// A failed install must not leave the job with a PATH entry pointing at nothing:
// the next step would fail on "codeowners-tool: not found", which reads as a
// broken workflow rather than a failed download.
func TestAction_AFailedInstallExportsNoPATHEntry(t *testing.T) {
	r := run{version: "v0.0.9", stubExit: 1}.exec(t)
	if r.exitCode == 0 {
		t.Fatalf("setup.sh reported success though install.sh failed\n%s", r)
	}
	if len(r.pathAdds) != 0 {
		t.Errorf("$GITHUB_PATH got %v after a failed install, want nothing", r.pathAdds)
	}
	if len(r.outputs) != 0 {
		t.Errorf("outputs %v were set after a failed install, want none", r.outputs)
	}
}

// Releases ship Windows builds, but as .zip archives that install.sh explicitly
// declines to handle. Saying so on the runner where it is true beats a Git Bash
// uname failing somewhere inside the download.
func TestAction_RefusesWindowsRunnersWithTheRouteThatWorks(t *testing.T) {
	r := run{version: "v0.0.9", runnerOS: "Windows"}.exec(t)
	if r.exitCode == 0 {
		t.Fatalf("the action reported success on a Windows runner\n%s", r)
	}
	if r.ranInstall {
		t.Error("a Windows runner reached install.sh, which refuses the platform after downloading nothing it can use")
	}
	if !strings.Contains(r.output, ".zip") {
		t.Errorf("the refusal does not point at the .zip on the release, which is the route that does work:\n%s", r)
	}
}

// A workflow that wants the binary somewhere it controls — a cached directory, a
// path baked into a container — must be able to say so.
func TestAction_HonorsAnExplicitInstallDir(t *testing.T) {
	dir := t.TempDir()
	r := run{version: "v0.0.9", installDir: dir, stubVersion: "v0.0.9"}.exec(t)
	if r.exitCode != 0 {
		t.Fatalf("setup.sh exited %d, want 0\n%s", r.exitCode, r)
	}
	if got := r.installed["BINDIR"]; got != dir {
		t.Errorf("install.sh got BINDIR=%q, want the requested %q", got, dir)
	}
}
