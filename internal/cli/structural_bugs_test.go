// Bug-hunt findings from a review aimed at complex repository structures:
// monorepos, repos whose CODEOWNERS is not where the tool assumes, trees
// carrying bytes Go's JSON encoder cannot represent, and fleet scripts that
// pair the wrong two files.
//
// Each test below is a FAILING repro of a confirmed defect, written first per
// CONTRIBUTING.md. The doc comment on each states the finding, what the tool
// does today, and which existing guarantee or sibling guard it contradicts.
package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// FINDING: `verify` accepts a before/after pair taken from two DIFFERENT
// repositories and reports `ok`, exit 0.
//
// verify is the documented CI gate — COMMANDS.md prescribes `snapshot` twice
// then `verify --before … --after …` to "prove in CI that a merged change
// moved nothing outside its declared scope". A snapshot carries `repo`, `ref`,
// `codeowners_path` and `codeowners_sha256`, and verify.Compare reads none of
// them: it diffs the two ownership maps and classifies every path present in
// only one snapshot as a tree delta, which R-18 says is never a violation.
// Two unrelated repositories share no paths at all, so EVERY row is a tree
// delta and the gate reports "0 change(s), all within scope".
//
// The failure mode is a fleet loop — the setting FLEET.md is written for —
// where before.json and after.json are per-repo filenames and one loop
// variable is wrong: every repo comes back green, and the invariant the gate
// exists to prove was never checked on any of them. `apply` already refuses
// exactly this mistake ("--repo X is a different repository from the one this
// plan was computed against"); verify, which is the LAST line of defence
// rather than the first, has no such guard.
func TestVerifyRefusesSnapshotsFromDifferentRepositories(t *testing.T) {
	repoA := initRepo(t, map[string]string{
		".github/CODEOWNERS":  "* @org/every\n/services/api/ @org/api-team\n",
		"services/api/api.go": "",
		"docs/guide.md":       "",
	})
	repoB := initRepo(t, map[string]string{
		// Root CODEOWNERS, so the two snapshots do not even share that path.
		"CODEOWNERS":   "* @org/other\n",
		"lib/thing.rb": "",
	})

	dir := t.TempDir()
	beforeA := filepath.Join(dir, "before.json")
	afterB := filepath.Join(dir, "after.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", repoA, "--out", beforeA); code != 0 {
		t.Fatalf("snapshot repoA: exit %d: %s", code, e)
	}
	if code, _, e := runCLI(t, "snapshot", "--repo", repoB, "--out", afterB); code != 0 {
		t.Fatalf("snapshot repoB: exit %d: %s", code, e)
	}

	// Sanity: the two snapshots genuinely share no path, so nothing verify
	// could compare is common to them.
	load := func(path string) map[string][]string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var s struct {
			Ownership map[string][]string `json:"ownership"`
		}
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		return s.Ownership
	}
	ownA, ownB := load(beforeA), load(afterB)
	for p := range ownA {
		if _, both := ownB[p]; both {
			t.Fatalf("fixture is wrong: %q is in both snapshots", p)
		}
	}

	code, stdout, stderr := runCLI(t, "verify", "--before", beforeA, "--after", afterB)
	if code == cli.ExitOK {
		t.Errorf("verify green-lit a before/after pair from two different repositories (exit 0)\n"+
			"the pair shares ZERO paths, so nothing was actually compared; `apply` refuses the same mistake\n"+
			"stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
}

// FINDING: `sync` silently adopts an UNTRACKED CODEOWNERS as the governing
// file, while `plan`, `snapshot` and `audit` all refuse the same repository at
// the same moment with "no CODEOWNERS file found … at HEAD" (exit 3).
//
// governing()'s D5 fallback searches the working tree when `git ls-tree` finds
// no CODEOWNERS, so a file that exists on disk but was never committed becomes
// the baseline INV-1 and INV-2 are proven against. On GitHub that repository
// has NO CODEOWNERS: every path is unowned, and the "before" state the proof
// rests on is one GitHub has never seen. The run reports
// `applied (proven: tree)`, exit 0, with no warning — and R-23's `--create`
// gate, the tool's stated permission to write a CODEOWNERS into a repo that
// has none, is never consulted.
//
// D5's stated purpose is narrower than its effect: it exists so a nightly job
// that created the file in pass 1 can converge in pass 2. It also silently
// adopts a template a provisioning script dropped in, a half-finished manual
// edit, and — the subtest below — a CODEOWNERS the repo's own .gitignore says
// can never be committed at all, which no amount of "commit it and re-run"
// will fix. governingWarnings() is the mechanism for exactly this class
// ("Every condition below exits 0 and reports 'applied' … invisible at fleet
// scale unless the run that touched the file says so") and has no case for it.
func TestSyncWarnsWhenGoverningCodeownersIsUntracked(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"services/api/main.go": "",
		"tools/build.sh":       "",
	})
	// Never committed: GitHub reads no CODEOWNERS from this repository.
	if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".github/CODEOWNERS"),
		[]byte("* @org/every\n/services/api/ @org/api-team\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The three read-only commands agree the repository has no CODEOWNERS.
	for _, args := range [][]string{
		{"snapshot", "--repo", repo},
		{"audit", "--repo", repo},
		{"plan", "--repo", repo, "--op", "add_owner(/services/api/, @org/platform)", "--out", filepath.Join(t.TempDir(), "p.json")},
	} {
		if code, _, _ := runCLI(t, args...); code != cli.ExitInvalid {
			t.Fatalf("fixture drifted: %v returned %d, expected exit 3 (no CODEOWNERS at HEAD)", args[0], code)
		}
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/platform)")
	out := stdout + stderr
	if code == cli.ExitOK && !strings.Contains(out, "untracked") && !strings.Contains(out, "not tracked") && !strings.Contains(out, "committed") {
		t.Errorf("sync adopted an untracked .github/CODEOWNERS as the governing file and reported success with no warning (exit %d)\n"+
			"plan, snapshot and audit all exit 3 on this same repository; GitHub reads no CODEOWNERS from it, so\n"+
			"the INV-2 baseline this run proved against is one that has never existed\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}

	// The same fallback, on a file git has been told to never track.
	t.Run("gitignored", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			".gitignore":           ".github/CODEOWNERS\n",
			"services/api/main.go": "",
		})
		if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".github/CODEOWNERS"),
			[]byte("* @org/every\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/services/api/, @org/platform)")
		out := stdout + stderr
		if code == cli.ExitOK && !strings.Contains(out, "ignor") && !strings.Contains(out, "untracked") {
			t.Errorf("sync proved a change against a CODEOWNERS the repo's .gitignore forbids committing, exit 0\n"+
				"no re-run can ever make this file the one GitHub reads\noutput:\n%s", out)
		}
	})
}

// FINDING: `snapshot` emits an ownership object with DUPLICATE keys when the
// tree holds paths that are not valid UTF-8, so the snapshot loses tracked
// paths on the round-trip and `verify` stops checking them.
//
// git stores path bytes verbatim and `ls-tree -z` returns them unquoted, so a
// legacy latin-1 filename reaches the snapshot as an invalid-UTF-8 Go string.
// encoding/json replaces every such byte with U+FFFD, so `a\xe9.md` and
// `a\xff.md` — two distinct tracked files — are written as the same JSON key
// twice. Any decoder keeps one. verify.Load decodes into a map, so one path
// vanishes before Compare ever runs: an ownership change on it is neither a
// change, an addition, nor a removal — it is invisible, and the gate that
// exists to prove INV-2 over "raw data" reports ok.
//
// The snapshot also stops being true on its own terms: it names a path that
// is not in the repository, so anyone grepping it for their file finds
// nothing.
func TestSnapshotIsLosslessForNonUTF8Paths(t *testing.T) {
	// Two names differing only in a byte that is not valid UTF-8; both
	// encode to the same U+FFFD spelling.
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"a\xe9.md":           "",
		"a\xff.md":           "",
	})

	snapPath := filepath.Join(t.TempDir(), "snap.json")
	if code, _, e := runCLI(t, "snapshot", "--repo", repo, "--out", snapPath); code != 0 {
		t.Fatalf("snapshot: exit %d: %s", code, e)
	}
	raw, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Ownership map[string]json.RawMessage `json:"ownership"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("snapshot is not decodable JSON: %v\n%s", err, raw)
	}

	const wantPaths = 3 // .github/CODEOWNERS, a\xe9.md, a\xff.md
	if len(snap.Ownership) != wantPaths {
		t.Errorf("snapshot round-trips to %d of %d tracked paths — a path was lost to a duplicate JSON key\n"+
			"verify decodes the same way, so it can no longer see an ownership change on the lost path\nsnapshot:\n%s",
			len(snap.Ownership), wantPaths, raw)
	}
	if n := strings.Count(string(raw), `"a�.md"`); n > 1 {
		t.Errorf("snapshot names the same key %d times: non-UTF-8 path bytes were folded to U+FFFD\nsnapshot:\n%s", n, raw)
	}
}

// FINDING: `--file` naming a path that is not in the ref hands the operator
// raw git plumbing, `--end-of-options` and all.
//
//	$ codeowners-tool snapshot --repo r --file docs/CODEOWNERS
//	error: git cat-file --end-of-options blob HEAD:docs/CODEOWNERS: exit status 128: fatal: path 'docs/CODEOWNERS' does not exist in 'HEAD'
//
// This is the failure mode TestBranchMismatchErrorNamesHeadCleanly already
// pinned for the S-7 refusal: `--end-of-options` is an operator this tool
// passes to git, never something the reader typed, and echoing it sends them
// hunting for a flag that does not exist. `plan` reports the same condition
// cleanly, so the three commands disagree about the same mistake.
func TestFileNotInRefErrorNamesThePathCleanly(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"a.md":               "",
	})
	for _, cmd := range []string{"snapshot", "audit"} {
		code, stdout, stderr := runCLI(t, cmd, "--repo", repo, "--file", "docs/CODEOWNERS")
		if code == cli.ExitOK {
			t.Fatalf("%s --file docs/CODEOWNERS: expected a refusal, got exit 0", cmd)
		}
		out := stdout + stderr
		if strings.Contains(out, "--end-of-options") || strings.Contains(out, "cat-file") {
			t.Errorf("%s leaks git plumbing into the error a person reads:\n%s", cmd, out)
		}
	}
}

// FINDING: `audit` reports "clean" on a monorepo carrying package-level
// CODEOWNERS files that GitHub never loads.
//
// A-10 is the check for "a CODEOWNERS file that governs nothing", and it fires
// for `docs/CODEOWNERS` sitting beside `.github/CODEOWNERS`. It is built on
// gittree.FindCodeownersPaths, which tests only the three root-level locations
// S-8 names, so `packages/foo/.github/CODEOWNERS` and `packages/bar/CODEOWNERS`
// are invisible to it. Those are the files most likely to mislead: a monorepo
// migrated from Bazel/Gerrit OWNERS or from split repos keeps per-package
// ownership files that look authoritative, review is routed by them in
// people's heads, and GitHub honors not one line of them.
//
// `audit clean`, exit 0, is the CI gate asserting this repository has no
// ownership rot. Here it asserts it over two files' worth of owner
// assignments that do nothing.
func TestAuditReportsNestedCodeownersFilesThatGovernNothing(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":              "* @org/every\n",
		"packages/foo/.github/CODEOWNERS": "/src/ @org/foo-team\n",
		"packages/foo/src/a.go":           "",
		"packages/bar/CODEOWNERS":         "* @org/bar-team\n",
		"packages/bar/b.go":               "",
	})

	code, stdout, stderr := runCLI(t, "audit", "--repo", repo)
	out := stdout + stderr
	if code == cli.ExitOK {
		t.Errorf("audit reports clean on a monorepo with two package-level CODEOWNERS files GitHub never loads (exit 0)\n"+
			"A-10 catches docs/CODEOWNERS beside .github/CODEOWNERS but not packages/foo/.github/CODEOWNERS\noutput:\n%s", out)
	}
	if !strings.Contains(out, "packages/foo/.github/CODEOWNERS") {
		t.Errorf("nothing in the audit names packages/foo/.github/CODEOWNERS, whose rules govern nothing\noutput:\n%s", out)
	}
}
