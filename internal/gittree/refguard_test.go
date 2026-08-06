package gittree_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/gittree"
)

// --branch REF reaches git as a POSITIONAL argument, so a value beginning with a
// dash is parsed by git as an OPTION rather than as a ref.
//
// It is reachable today: `--branch '--format=%(path)'` produces
//
//	fatal: --format can't be combined with other format-altering options
//
// which is git's ls-tree option parser talking, not its revision parser. Nothing
// here is demonstrably exploitable — ls-tree's options change output, they do not
// write files or open connections the way `--upload-pack` does on the commands
// that take it — but "the option surface of whatever git version is installed"
// is not a boundary worth leaving open when git itself forbids the input.
//
// git-check-ref-format is explicit that a refname cannot begin with a dash, and
// no revision expression (HEAD, main, @~2, a SHA) starts with one either, so
// rejecting the leading dash rejects nothing a user could legitimately mean.
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

// A dash-leading ref must be refused by us, with our words, rather than handed to
// git's option parser to fail (or succeed) in whatever way that version happens to.
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

// The same guard on the blob read: `cat-file blob <ref>:<path>` concatenates the
// ref, so a dash-leading ref produces a dash-leading argument there too.
func TestRefGuard_ReadFileAtRefRejectsADashLeadingRef(t *testing.T) {
	repo := repoWithOneFile(t)
	_, err := gittree.ReadFileAtRef(repo, "--output=/tmp/pwned", "a.txt")
	if err == nil {
		t.Fatalf("ReadFileAtRef with a dash-leading ref succeeded; the ref is concatenated into a positional argument and reaches git's option parser")
	}
	// This one would pass VACUOUSLY on the unfixed code: git fails anyway, because
	// `--output=/tmp/pwned:a.txt` is not an object it can find. A non-nil error
	// proves nothing on its own — what has to be true is that the value never
	// reached git at all, and git's "fatal:" prefix is the evidence that it did.
	if strings.Contains(err.Error(), "fatal:") {
		t.Errorf("the ref was handed to git and git rejected it (%v); reject it before exec, so the diagnosis does not depend on which git is installed", err)
	}
}

// The guard must not cost anything a user could legitimately want. Refs, tags,
// SHAs and revision expressions all still resolve.
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
