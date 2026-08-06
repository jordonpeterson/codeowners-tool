package gittree_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/gittree"
)

// --branch arrives positionally, so a dash-leading value is parsed as an OPTION:
// `--branch '--format=%(path)'` answers "--format can't be combined with other
// format-altering options" — ls-tree's option parser, not its revision parser.
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

// Refused by us, rather than handed to git's option parser.
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
		// git's diagnostics are the tell that it reached the option parser.
		for _, leak := range []string{"can't be combined", "usage: git ls-tree", "unknown option"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("ListTracked(%q): the value reached git's option parser (%q appears in the error); reject it before exec instead", ref, leak)
			}
		}
	}
}

// Same guard on the blob read, where the ref is concatenated with the path.
func TestRefGuard_ReadFileAtRefRejectsADashLeadingRef(t *testing.T) {
	repo := repoWithOneFile(t)
	_, err := gittree.ReadFileAtRef(repo, "--output=/tmp/pwned", "a.txt")
	if err == nil {
		t.Fatalf("ReadFileAtRef with a dash-leading ref succeeded; the ref is concatenated into a positional argument and reaches git's option parser")
	}
	// Vacuous otherwise: git fails on the bogus object anyway, so a non-nil error
	// proves nothing. The "fatal:" prefix is the evidence it reached git.
	if strings.Contains(err.Error(), "fatal:") {
		t.Errorf("the ref was handed to git and git rejected it (%v); reject it before exec, so the diagnosis does not depend on which git is installed", err)
	}
}

// The guard must cost nothing legitimate.
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
