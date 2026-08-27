package supplychain

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The release line is chosen by hand in VERSION; the patch number is the
// workflow's. These gates run the workflow's own script rather than reading it,
// because the failure they exist for is arithmetic: a `1.0` in VERSION whose
// first release comes out `v1.0.1`, so the version everyone writes down —
// "1.0.0" — names no build that exists.

const versionFile = "../../VERSION"

var (
	versionLineRe = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	majorPinRe    = regexp.MustCompile(`uses: jordonpeterson/codeowners-tool@v([0-9]+)\b`)
)

func versionLine(t *testing.T) string {
	t.Helper()
	line := strings.TrimSpace(readRepoFile(t, versionFile))
	if !versionLineRe.MatchString(line) {
		t.Fatalf("VERSION holds %q; release.yml refuses anything but MAJOR.MINOR", line)
	}
	return line
}

// stepScript returns the shell body of a `run: |` block, dedented, so a test can
// execute exactly what the runner executes.
func stepScript(t *testing.T, body, step string) string {
	t.Helper()
	i := strings.Index(body, "- name: "+step)
	if i < 0 {
		t.Fatalf("release.yml has no step named %q", step)
	}
	rest := body[i:]
	j := strings.Index(rest, "run: |")
	if j < 0 {
		t.Fatalf("step %q has no `run: |` block", step)
	}
	lines := strings.Split(rest[j+len("run: |"):], "\n")[1:]
	indent := len(lines[0]) - len(strings.TrimLeft(lines[0], " "))
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			out = append(out, "")
			continue
		}
		if len(l)-len(strings.TrimLeft(l, " ")) < indent {
			break
		}
		out = append(out, l[indent:])
	}
	return strings.Join(out, "\n")
}

// computeVersion runs the workflow's version step in a throwaway clone carrying
// the given tags, and returns the tag it decided to publish.
func computeVersion(t *testing.T, version string, tags ...string) string {
	t.Helper()
	dir := t.TempDir()
	// The step runs `git fetch --tags --force`, which needs a remote to reach;
	// without one it dies under `set -e` before it computes anything.
	origin := filepath.Join(dir, "origin.git")
	work := filepath.Join(dir, "work")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "--bare", "-q", origin)
	git("init", "-q", work)
	git("-C", work, "remote", "add", "origin", origin)
	git("-C", work, "commit", "-q", "--allow-empty", "-m", "seed")
	for _, tag := range tags {
		git("-C", work, "tag", tag)
	}
	if err := os.WriteFile(filepath.Join(work, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "output")
	if err := os.WriteFile(out, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := stepScript(t, releaseWorkflow(t), "Compute next version")
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+out)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("version step failed for VERSION=%s tags=%v: %v\n%s", version, tags, err, combined)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if after, ok := strings.CutPrefix(l, "tag="); ok {
			return after
		}
	}
	t.Fatalf("version step wrote no tag= to GITHUB_OUTPUT:\n%s", b)
	return ""
}

// A release line opens at .0. Starting it at .1 means the release everyone
// writes down and pins to — v1.0.0 — is a tag that never exists, and `brew
// install codeowners-tool@1.0.0` and `uses: ...@v1.0.0` both 404.
func TestRelease_FirstReleaseOnALineIsPatchZero(t *testing.T) {
	if got, want := computeVersion(t, "1.0", "v0.0.1", "v0.0.30"), "v1.0.0"; got != want {
		t.Errorf("VERSION=1.0 with no v1.0.* tags publishes %s, want %s", got, want)
	}
	if got, want := computeVersion(t, versionLine(t)), "v"+versionLine(t)+".0"; got != want {
		t.Errorf("the committed VERSION=%s opens its line at %s, want %s", versionLine(t), got, want)
	}
}

// The patch number still comes from the tags already on that line — a line that
// restarted at .0 on every run would try to republish an immutable release.
func TestRelease_LaterReleasesContinueTheLine(t *testing.T) {
	if got, want := computeVersion(t, "0.0", "v0.0.9", "v0.0.29", "v0.0.30"), "v0.0.31"; got != want {
		t.Errorf("next release after v0.0.30 is %s, want %s", got, want)
	}
	if got, want := computeVersion(t, "1.0", "v0.0.30", "v1.0.0"), "v1.0.1"; got != want {
		t.Errorf("next release after v1.0.0 is %s, want %s", got, want)
	}
}

// `uses: ...@vN` resolves through the major tag, and the release only moves the
// major tag of the line it publishes. Docs still pinning the previous major hold
// every CI consumer on the last release of the old line, silently and forever.
func TestRelease_DocsPinTheMajorTagReleasesStillMove(t *testing.T) {
	major := "v" + strings.SplitN(versionLine(t), ".", 2)[0]
	for _, page := range []string{readmePath, installDoc, "../../docs/LINTING.md"} {
		body := readRepoFile(t, page)
		for _, m := range majorPinRe.FindAllStringSubmatch(body, -1) {
			if got := "v" + m[1]; got != major {
				t.Errorf("%s tells consumers to pin @%s while VERSION names the %s line.\nReleases move %s only, so everyone following this page is frozen at the last %s release.",
					page, got, major, major, got)
			}
		}
	}
}
