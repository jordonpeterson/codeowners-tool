// Package gittree_test defines how the tool obtains the tracked file tree.
//
// SPEC S-7 / INV-3: resolution runs against the tracked tree of a specific
// ref (default HEAD) of a local repository — never the working directory
// listing, never the pattern set.
package gittree_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/gittree"
)

func initRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for path, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

// SPEC S-7: the tree comes from a named ref via git, repo-relative paths with
// forward slashes.
func TestS7_ListTracked(t *testing.T) {
	dir := initRepo(t, map[string]string{
		"a.txt":         "x",
		"src/b.go":      "y",
		"src/deep/c.md": "z",
	})
	got, err := gittree.ListTracked(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"a.txt", "src/b.go", "src/deep/c.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// SPEC S-7: untracked files are invisible — resolution must not see them.
func TestS7_UntrackedFilesExcluded(t *testing.T) {
	dir := initRepo(t, map[string]string{"tracked.txt": "x"})
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := gittree.ListTracked(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "tracked.txt" {
		t.Errorf("got %v, want [tracked.txt]", got)
	}
}

// A bad ref is a hard error (exit-code 3 territory), not an empty tree — an
// empty tree would make every scope "matches nothing" and mask user error.
func TestS7_BadRefErrors(t *testing.T) {
	dir := initRepo(t, map[string]string{"a.txt": "x"})
	if _, err := gittree.ListTracked(dir, "no-such-ref"); err == nil {
		t.Error("bad ref must error, not return an empty tree")
	}
}

// FindCodeownersPaths reports every CODEOWNERS file present in the tree, in
// GitHub's precedence order (.github/, root, docs/) — S-8: more than one is
// an error condition for the caller, never a merge.
func TestS8_FindCodeownersPaths(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @a\n",
		"CODEOWNERS":         "* @b\n",
		"docs/CODEOWNERS":    "* @c\n",
		"x.txt":              "",
	})
	tree, err := gittree.ListTracked(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	got := gittree.FindCodeownersPaths(tree)
	want := []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}
	if len(got) != 3 {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("precedence order wrong: got %v, want %v", got, want)
		}
	}
}

// ReadFileAtRef reads a blob from a ref without touching the working tree.
func TestS7_ReadFileAtRef(t *testing.T) {
	dir := initRepo(t, map[string]string{"CODEOWNERS": "* @a\n"})
	b, err := gittree.ReadFileAtRef(dir, "HEAD", "CODEOWNERS")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "* @a\n" {
		t.Errorf("got %q", b)
	}
}
