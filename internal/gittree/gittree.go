// Package gittree reads the tracked file tree of a local git repository at a
// named ref (S-7: CODEOWNERS is per-branch; resolution runs against a
// specific branch's tree).
package gittree

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// ValidateRef rejects a ref git would parse as one of its own options: --branch
// arrives positionally, so `--branch '--format=…'` lands in ls-tree's option
// parser. A refname cannot begin with a dash anyway.
//
// --end-of-options is NOT usable on ls-tree — it turns the trailing `--` into a
// pathspec, so the tree comes back EMPTY and every scope resolves against a repo
// that appears to have no files.
func ValidateRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("empty ref: pass a branch, tag, or commit (default HEAD)")
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid ref %q: a ref may not begin with '-' (git-check-ref-format forbids it), and git would parse this one as one of its own options rather than as a ref", ref)
	}
	return nil
}

// ListTracked returns the repo-relative paths (forward slashes) of every
// tracked file at ref. A bad ref or non-repo is a hard error.
func ListTracked(repoDir, ref string) ([]string, error) {
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}
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

// ListWorkTree returns the repo-relative paths of every file that is on disk
// and not ignored — tracked files plus untracked, non-ignored ones.
//
// It answers a different question from ListTracked, and lint needs both. A rule
// is only stale if its pattern matches nothing at the ref AND nothing in the
// checkout: a directory that has been created but not committed is invisible to
// `ls-tree`, and deleting its owners because git has not seen it yet un-owns a
// directory the developer is looking at.
//
// Ignored files are excluded deliberately — build output is not evidence that a
// rule is live.
func ListWorkTree(repoDir string) ([]string, error) {
	out, err := gitOutput(repoDir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var paths []string
	seen := map[string]bool{}
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) == 0 || seen[string(p)] {
			continue
		}
		seen[string(p)] = true
		paths = append(paths, string(p))
	}
	return paths, nil
}

// ReadFileAtRef reads a file's content from a ref without touching the working
// tree. cat-file has no separator to lose, so --end-of-options is safe here.
func ReadFileAtRef(repoDir, ref, path string) ([]byte, error) {
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}
	return gitOutput(repoDir, "cat-file", "--end-of-options", "blob", ref+":"+path)
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
// Digest fingerprints a tracked tree. A plan's ownership_rows are computed
// against the tree at plan time and are the artifact a human reviews, so the
// tree is part of what was approved: two more files under the reviewed scope
// mean the blast radius is no longer the one on the page.
//
// Sorted and NUL-joined, so the digest is a property of the path SET and not
// of whatever order git happened to list it in. NUL is the separator because
// it is the one byte a git path cannot contain, so no two distinct trees can
// join to the same string.
func Digest(tree []string) string {
	sorted := append([]string(nil), tree...)
	sort.Strings(sorted)
	h := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return hex.EncodeToString(h[:])
}

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
