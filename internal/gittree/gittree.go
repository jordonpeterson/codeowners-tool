// Package gittree reads the tracked file tree of a local git repository at a
// named ref (S-7: CODEOWNERS is per-branch; resolution runs against a
// specific branch's tree).
package gittree

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// ListTracked returns the repo-relative paths (forward slashes) of every
// tracked file at ref. A bad ref or non-repo is a hard error.
func ListTracked(repoDir, ref string) ([]string, error) {
	out, err := gitOutput(repoDir, "ls-tree", "-r", "--name-only", "-z", ref, "--")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	return paths, nil
}

// ReadFileAtRef reads a file's content from a ref without touching the
// working tree.
func ReadFileAtRef(repoDir, ref, path string) ([]byte, error) {
	return gitOutput(repoDir, "cat-file", "blob", ref+":"+path)
}

// CodeownersLocations is GitHub's search order: first found wins (S-8).
//
// Exported because the same order governs two different lookups — "what governs
// at this ref", over the tracked tree below, and "what is on disk right now",
// which `sync` needs for its working-tree fallback. The questions differ; the
// list is one fact about GitHub, and a second copy of it is a place for the two
// to disagree the day GitHub adds a location.
var CodeownersLocations = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

// FindCodeownersPaths returns every CODEOWNERS file present in the tree, in
// precedence order. More than one entry is an error condition for callers
// (A-10) — GitHub uses only the first and never merges.
func FindCodeownersPaths(tree []string) []string {
	present := make(map[string]bool, len(tree))
	for _, p := range tree {
		present[p] = true
	}
	var found []string
	for _, loc := range CodeownersLocations {
		if present[loc] {
			found = append(found, loc)
		}
	}
	return found
}

func gitOutput(repoDir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, msg)
	}
	return out, nil
}
