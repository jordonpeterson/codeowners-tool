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

// ValidateRef rejects a ref that git would parse as one of its own options.
//
// Every ref this package receives is an operator-supplied --branch that reaches
// git as a POSITIONAL argument, and git's parsers do not distinguish "the third
// word on the command line" from "an option". It is reachable:
// `--branch '--format=%(path)'` answers
//
//	fatal: --format can't be combined with other format-altering options
//
// which is ls-tree's OPTION parser talking, not its revision parser. Nothing here
// is demonstrably exploitable — ls-tree's options reshape output, they do not
// write files or open connections the way --upload-pack does on the commands that
// take it — but the boundary as it stands is "whatever options the installed git
// happens to have", which is not a boundary anyone can reason about, and it moves
// with a version bump nobody reviewed.
//
// Rejecting a leading dash costs nothing a user could legitimately mean:
// git-check-ref-format forbids a refname beginning with a dash, and no revision
// expression (HEAD, main, @~2, v1.2.3, a SHA) starts with one either.
//
// The alternative — `--end-of-options` — does not fit every call site here. On
// ls-tree it is actively worse: `ls-tree ... --end-of-options <ref> --` makes the
// trailing `--` a PATHSPEC rather than a separator, matching nothing, so the tree
// comes back EMPTY and every scope silently resolves against a repository that
// appears to have no files in it. A silent wrong answer is the one failure this
// tool must not have, so ls-tree keeps its `--` and relies on this guard, and
// --end-of-options is used only where the grammar leaves the separator alone.
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

// ReadFileAtRef reads a file's content from a ref without touching the
// working tree.
// cat-file's grammar has no rev/path separator to lose, so --end-of-options can
// be used here as well as the guard — the ref is concatenated with the path, so
// a dash-leading ref makes the whole argument dash-leading.
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
