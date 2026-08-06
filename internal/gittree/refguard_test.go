package gittree_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/gittree"
)

// --branch reaches git as a POSITIONAL argument, so a dash-leading value is
// parsed as an OPTION. Reachable today: `--branch '--format=%(path)'` answers
// "--format can't be combined with other format-altering options", which is
// ls-tree's option parser, not its revision parser. Not demonstrably exploitable,
// but the boundary is otherwise whatever options the installed git happens to
// have. git-check-ref-format forbids a dash-leading refname anyway.
func repoWithOneFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	return dir
}

// Refused by us, with our words, rather than handed to git's option parser.
func TestRefGuard_ListTrackedRejectsARefThatGitWouldParseAsAnOption(t *testing.T) {
	repo := repoWithOneFile(t)
	for _, ref := range []string{"--format=%(path)", "-o/tmp/x", "--help"} {
		_, err := gittree.ListTracked(repo, ref)
		if err == nil {
			t.Errorf("ListTracked(%q) succeeded: git parsed an operator-supplied ref as one of its own options", ref)
			continue
		}
		if !strings.Contains(err.Error(), ref) {
			t.Errorf("ListTracked(%q): error does not name the rejected value, so the operator cannot see what was wrong: %v", ref, err)
		}
		// git's own diagnostics are the tell that the argument reached its option
		// parser at all.
		for _, leak := range []string{"can't be combined", "usage: git ls-tree", "unknown option"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("ListTracked(%q): the value reached git's option parser (%q appears in the error); reject it before exec instead", ref, leak)
			}
		}
	}
}

// Same guard on the blob read: `cat-file blob <ref>:<path>` concatenates the ref,
// so a dash-leading ref makes the whole argument dash-leading.
func TestRefGuard_ReadFileAtRefRejectsADashLeadingRef(t *testing.T) {
	repo := repoWithOneFile(t)
	_, err := gittree.ReadFileAtRef(repo, "--output=/tmp/pwned", "a.txt")
	if err == nil {
		t.Fatalf("ReadFileAtRef with a dash-leading ref succeeded; the ref is concatenated into a positional argument and reaches git's option parser")
	}
	// Vacuous on the unfixed code: git fails anyway, since that is not an object it
	// can find. A non-nil error proves nothing — what matters is that the value
	// never reached git, and git's "fatal:" prefix is the evidence that it did.
	if strings.Contains(err.Error(), "fatal:") {
		t.Errorf("the ref was handed to git and git rejected it (%v); reject it before exec, so the diagnosis does not depend on which git is installed", err)
	}
}

// The guard must cost nothing legitimate: refs, tags, SHAs and revision
// expressions all still resolve.
func TestRefGuard_OrdinaryRefsStillResolve(t *testing.T) {
	repo := repoWithOneFile(t)
	for _, ref := range []string{"HEAD", "main", "HEAD^{commit}", "@"} {
		files, err := gittree.ListTracked(repo, ref)
		if err != nil {
			t.Errorf("ListTracked(%q) failed, but it is an ordinary ref: %v", ref, err)
			continue
		}
		if len(files) != 1 || files[0] != "a.txt" {
			t.Errorf("ListTracked(%q) = %v, want [a.txt]", ref, files)
		}
	}
}
