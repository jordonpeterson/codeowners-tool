package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// SPEC R-18: `verify` refuses a before/after pair whose snapshots share no
// tracked path, exit 3, naming both files. Such a pair was never compared —
// every row is a tree delta, which R-18 never counts as a violation — so the
// run could only ever print `ok`. That is the green a fleet loop reports on
// all 100 repositories when one filename in it is wrong.
//
// Exit 3, not 2: no path changed out of scope, so there is no offending path
// to print, and the same wrong invocation fails identically in every repo —
// the "stop the rollout" class, alongside a malformed snapshot.
func TestVerifyRefusesAPairWithNoPathInCommon(t *testing.T) {
	repoA := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "* @org/every\n",
		"services/api/api.go": "",
	})
	repoB := initRepo(t, map[string]string{
		"CODEOWNERS":   "* @org/other\n",
		"lib/thing.rb": "",
	})
	dir := t.TempDir()
	before := filepath.Join(dir, "before.json")
	after := filepath.Join(dir, "after.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", repoA, "--out", before); code != cli.ExitOK {
		t.Fatalf("snapshot A: %d %s", code, e)
	}
	if code, _, e := runCLI(t, "snapshot", "--repo", repoB, "--out", after); code != cli.ExitOK {
		t.Fatalf("snapshot B: %d %s", code, e)
	}

	code, stdout, stderr := runCLI(t, "verify", "--before", before, "--after", after)
	if code != cli.ExitInvalid {
		t.Fatalf("exit %d, want %d — an uncomparable pair is a broken invocation, not a clean gate\nstdout:\n%s\nstderr:\n%s",
			code, cli.ExitInvalid, stdout, stderr)
	}
	// The whole point of the refusal is telling the operator WHICH two files
	// they paired, since the bug is a loop variable naming the wrong one.
	if !strings.Contains(stderr, before) || !strings.Contains(stderr, after) {
		t.Errorf("the refusal must name both snapshot files:\n%s", stderr)
	}
	if strings.Contains(stdout, "ok:") {
		t.Errorf("nothing may be reported ok:\n%s", stdout)
	}
}

// The documented recipe — snapshot two refs of ONE repository, verify against
// the declared scope — keeps working across a branch that renames, adds and
// deletes files. The guard above fires only on an empty intersection, so a
// pull request that churns most of the tree still gets a real answer.
func TestVerifyPassesTheDocumentedTwoRefRecipeAcrossAChurnedTree(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "/services/api/ @org/api\n* @org/all\n",
		"services/api/main.ts": "x\n",
		"web/old1.js":          "y\n",
		"web/old2.js":          "y\n",
		"gone.md":              "z\n",
	})
	before := filepath.Join(t.TempDir(), "before.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", before); code != cli.ExitOK {
		t.Fatalf("snapshot before: %d %s", code, e)
	}

	for _, f := range []string{"web/old1.js", "web/old2.js", "gone.md"} {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"web/new1.js", "web/new2.js", "web/new3.js"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(f)), []byte("n\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitAll(t, dir, "churn the tree")

	after := filepath.Join(t.TempDir(), "after.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", after); code != cli.ExitOK {
		t.Fatalf("snapshot after: %d %s", code, e)
	}
	code, stdout, stderr := runCLI(t, "verify", "--before", before, "--after", after, "--scope", "/services/api/")
	if code != cli.ExitOK {
		t.Fatalf("the documented recipe must still pass: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// Two clones of one repository have different `--repo` paths — CI checking
// main and the pull request into separate directories is the ordinary shape —
// so the pair must be judged on the tree it describes, never on that string.
func TestVerifyAcceptsSnapshotsTakenFromTwoClones(t *testing.T) {
	src := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/all\n",
		"a.go":               "x\n",
		"web/app.js":         "y\n",
	})
	dst := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}

	before := filepath.Join(t.TempDir(), "before.json")
	after := filepath.Join(t.TempDir(), "after.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", src, "--out", before); code != cli.ExitOK {
		t.Fatalf("snapshot src: %d %s", code, e)
	}
	if code, _, e := runCLI(t, "snapshot", "--repo", dst, "--out", after); code != cli.ExitOK {
		t.Fatalf("snapshot clone: %d %s", code, e)
	}
	code, stdout, stderr := runCLI(t, "verify", "--before", before, "--after", after)
	if code != cli.ExitOK {
		t.Fatalf("two clones of one repo are the same repository: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// The bootstrap pair: a repository whose base ref has NO CODEOWNERS at all
// (`snapshot` refuses it, exit 3) is verified against a hand-written baseline
// that lists every path as unowned. It carries no `repo`, `ref`,
// `codeowners_path` or `codeowners_sha256` — so a guard built on any of those
// fields would refuse the one flow that most needs the gate.
func TestVerifyAcceptsAHandWrittenBootstrapBaseline(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/all\n",
		"a.go":               "x\n",
		"web/app.js":         "y\n",
	})
	after := filepath.Join(t.TempDir(), "after.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", after); code != cli.ExitOK {
		t.Fatalf("snapshot: %d %s", code, e)
	}
	before := filepath.Join(t.TempDir(), "before.json")
	if err := os.WriteFile(before, []byte(
		`{"ownership":{".github/CODEOWNERS":null,"a.go":null,"web/app.js":null}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, "verify", "--before", before, "--after", after, "--scope", "*")
	if code != cli.ExitOK {
		t.Fatalf("a bootstrap baseline must still verify: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "changed: a.go") {
		t.Errorf("the pair really was compared — the bootstrap owns every path now:\n%s", stdout)
	}
}

// Two snapshots taken with different `--file` describe the same tree governed
// by different CODEOWNERS files, so `codeowners_path` and `codeowners_sha256`
// differ by construction. That is a comparison worth making, not a mismatched
// pair.
func TestVerifyAcceptsSnapshotsTakenWithDifferentFileFlags(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/all\n",
		"docs/CODEOWNERS":    "* @org/all\n/web/ @org/web\n",
		"web/app.js":         "y\n",
	})
	before := filepath.Join(t.TempDir(), "before.json")
	after := filepath.Join(t.TempDir(), "after.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", before); code != cli.ExitOK {
		t.Fatalf("snapshot .github: %d %s", code, e)
	}
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--file", "docs/CODEOWNERS", "--out", after); code != cli.ExitOK {
		t.Fatalf("snapshot docs: %d %s", code, e)
	}

	code, stdout, stderr := runCLI(t, "verify", "--before", before, "--after", after, "--scope", "/web/")
	if code != cli.ExitOK {
		t.Fatalf("differing codeowners_path is not a mismatched pair: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "changed: web/app.js") {
		t.Errorf("the two files really do resolve web/app.js differently:\n%s", stdout)
	}
}

// An ordinary snapshot is byte-for-byte what it has always been. The escape
// for non-UTF-8 paths must be invisible here: a literal `a%E9.md`, a
// multi-byte rune and the encoder's own HTML escaping all keep their exact
// spelling, so no existing consumer of the documented format breaks.
func TestSnapshotOfAnAsciiTreeIsByteIdentical(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n/web/ @org/web\n",
		"web/app.js":         "",
		"a%E9.md":            "",
		"café.md":            "",
		"a&b<c>.md":          "",
		"x/y.md":             "",
	})
	out := filepath.Join(t.TempDir(), "snap.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", out); code != cli.ExitOK {
		t.Fatalf("snapshot: %d %s", code, e)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{
  "repo": "<REPO>",
  "ref": "HEAD",
  "codeowners_path": ".github/CODEOWNERS",
  "codeowners_sha256": "7bcf3b02dc1b5b8a3a988ea12a9839071e2270ec9e74a5321735f22397395841",
  "ownership": {
    ".github/CODEOWNERS": [
      "@org/every"
    ],
    "a%E9.md": [
      "@org/every"
    ],
    "a\u0026b\u003cc\u003e.md": [
      "@org/every"
    ],
    "café.md": [
      "@org/every"
    ],
    "web/app.js": [
      "@org/web"
    ],
    "x/y.md": [
      "@org/every"
    ]
  }
}`
	got := strings.ReplaceAll(string(raw), dir, "<REPO>")
	if got != want {
		t.Errorf("snapshot bytes changed for an ordinary tree\n got:\n%s\nwant:\n%s", got, want)
	}
}

// SPEC R-18: two tracked paths differing only in a byte that is not valid
// UTF-8 are compared SEPARATELY. Both spell `bin/a�.md` once JSON has
// folded them, so before the fix each snapshot held one key for the two files
// and the gate reported half the reassignment it was looking at.
func TestVerifyComparesEachNonUTF8PathSeparately(t *testing.T) {
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"bin/a\xe9.md":       "",
		"bin/a\xff.md":       "",
		"keep.md":            "",
	})
	before := filepath.Join(t.TempDir(), "before.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", before); code != cli.ExitOK {
		t.Fatalf("snapshot before: %d %s", code, e)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github", "CODEOWNERS"),
		[]byte("* @org/every\n/bin/ @org/tools\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, dir, "reassign /bin/")
	after := filepath.Join(t.TempDir(), "after.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", dir, "--out", after); code != cli.ExitOK {
		t.Fatalf("snapshot after: %d %s", code, e)
	}

	// No --scope: the reassignment of both files must be reported.
	code, stdout, stderr := runCLI(t, "verify", "--before", before, "--after", after)
	if code != cli.ExitRefused {
		t.Fatalf("exit %d, want %d — /bin/ was reassigned\nstdout:\n%s\nstderr:\n%s", code, cli.ExitRefused, stdout, stderr)
	}
	for _, p := range []string{"bin/a%E9.md", "bin/a%FF.md"} {
		if !strings.Contains(stdout, "changed: "+p) {
			t.Errorf("%s is a tracked file whose owners changed; verify never saw it\nstdout:\n%s", p, stdout)
		}
		if !strings.Contains(stderr, p) {
			t.Errorf("%s must be listed as a violation\nstderr:\n%s", p, stderr)
		}
	}
	// And nothing is reported under the U+FFFD spelling, which names no file
	// in the repository.
	if strings.Contains(stdout+stderr, "�") {
		t.Errorf("a path was reported under a name that is not in the tree:\n%s\n%s", stdout, stderr)
	}

	// The snapshot on disk keeps both paths, and `verify` reads them back.
	var snap struct {
		Ownership map[string]json.RawMessage `json:"ownership"`
	}
	raw, err := os.ReadFile(after)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("snapshot is not decodable: %v", err)
	}
	if len(snap.Ownership) != 4 {
		t.Errorf("snapshot has %d of 4 tracked paths:\n%s", len(snap.Ownership), raw)
	}
}
