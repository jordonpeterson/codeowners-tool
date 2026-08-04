// R-19 — convergence and idempotence at fleet scale.
//
// The failure this file exists to prevent: a scheduled job pushes one policy
// at 100 repositories every night, and every night each run appends the same
// line again. Nothing errors, every exit code is 0, and resolved ownership is
// correct on every pass — so a test that compares OWNERSHIP passes forever
// while the file grows without bound. It ends at S-4's 3 MB cliff, where
// GitHub silently stops loading CODEOWNERS at all: total ownership failure
// across the whole fleet, produced by a job that reported success every night
// on the way there.
//
// This is not hypothetical. The planner's no-op detector is
// `bytes.Equal(afterBytes, content)` (internal/plan/plan.go), gated on a
// `desired` map keyed by TRACKED path. A `declare` op writes a rule for files
// that do not exist yet, so it moves no tracked path's resolution and the
// detector structurally cannot see its line. Ownership-level idempotence is
// therefore the wrong assertion. Every claim below compares BYTES.

package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// ---------------------------------------------------------------------------
// Helpers. All prefixed `idem` — this package is written by several agents at
// once and an unprefixed helper is a compile error for everyone.
// initRepo and runCLI come from cli_test.go and are reused, never redefined.
// ---------------------------------------------------------------------------

// idemRepo is one fleet member: a name for failure messages and its directory.
type idemRepo struct {
	name string
	dir  string
}

func idemGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// idemCommit commits whatever the last sync wrote. That is the real shape of
// the scheduled job: last night's PR is merged, and tonight's run reads the
// merged result. Committing also makes a --create'd file tracked, so the
// second pass can find it at all.
func idemCommit(t *testing.T, dir string) {
	t.Helper()
	idemGit(t, dir, "add", "-A")
	idemGit(t, dir, "commit", "-q", "--allow-empty", "-m", "sync")
}

// idemOwnersRel returns the repo-relative CODEOWNERS path in S-8 precedence
// order.
func idemOwnersRel(t *testing.T, repo string) string {
	t.Helper()
	for _, cand := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(cand))); err == nil {
			return cand
		}
	}
	t.Fatalf("no CODEOWNERS file in %s", repo)
	return ""
}

// idemBytes reads the governing CODEOWNERS verbatim. Every idempotence claim
// in this file is made against these bytes, never against resolution.
func idemBytes(t *testing.T, repo string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(idemOwnersRel(t, repo))))
	if err != nil {
		t.Fatalf("read CODEOWNERS in %s: %v", repo, err)
	}
	return b
}

func idemPolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// idemSyncRaw runs `sync --format json` and returns the untouched streams, for
// the cases where no record is expected (a broken policy is exit 3 and emits
// no repo row).
func idemSyncRaw(t *testing.T, repo string, args ...string) (int, string, string) {
	t.Helper()
	argv := append([]string{"sync", "--repo", repo, "--format", "json"}, args...)
	return runCLI(t, argv...)
}

// idemSync runs one sync and decodes the single JSON record (R-24).
func idemSync(t *testing.T, repo string, args ...string) (int, cli.SyncRecord, string) {
	t.Helper()
	code, out, errOut := idemSyncRaw(t, repo, args...)
	var rec cli.SyncRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err != nil {
		t.Fatalf("sync --format json must write exactly one SyncRecord to stdout (exit %d): %v\nstdout: %q\nstderr: %q",
			code, err, out, errOut)
	}
	return code, rec, errOut
}

// idemPass is one nightly pass: sync, then commit what it wrote.
func idemPass(t *testing.T, repo string, args ...string) (int, cli.SyncRecord) {
	t.Helper()
	code, rec, _ := idemSync(t, repo, args...)
	idemCommit(t, repo)
	return code, rec
}

// idemConverged asserts a repeat pass converged: exit 0 and a status that
// claims nothing was written. "applied" on a repeat pass is the growth bug.
func idemConverged(t *testing.T, name string, pass int, code int, rec cli.SyncRecord) {
	t.Helper()
	if code != cli.ExitOK {
		t.Errorf("%s pass %d: exit %d, want 0 — a converged repo must not fail (status %q, error %q)",
			name, pass, code, rec.Status, rec.Error)
	}
	if rec.Status != cli.StatusUnchanged && rec.Status != cli.StatusSkipped {
		t.Errorf("%s pass %d: status %q, want %q or %q — re-running the same policy must not re-apply it",
			name, pass, rec.Status, cli.StatusUnchanged, cli.StatusSkipped)
	}
}

// idemSameBytes is the assertion this whole file is built around.
func idemSameBytes(t *testing.T, label string, want, got []byte) {
	t.Helper()
	if bytes.Equal(want, got) {
		return
	}
	t.Errorf("%s: CODEOWNERS changed on a repeat run (%d → %d bytes)\nbefore:\n%s\nafter:\n%s",
		label, len(want), len(got), want, got)
}

// idemFleetPolicyJSON is one standardized policy pushed at heterogeneous
// repos: one op that must be declared where the directory does not exist yet,
// two that are opportunistic. This is the shape that makes a fleet policy
// applicable everywhere, and the shape that hides unbounded growth.
const idemFleetPolicyJSON = `{
  "version": 1,
  "name": "org baseline ownership",
  "description": "R-19 fleet convergence fixture",
  "ops": [
    { "id": "ci",   "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare" },
    { "id": "tf",   "op": "add_owner(**/*.tf, @org/infra)",          "on_zero_match": "skip" },
    { "id": "docs", "op": "add_owner(/docs/, @org/docs)",            "on_zero_match": "skip" }
  ]
}`

// idemFleet builds eight deliberately unalike repos: different CODEOWNERS
// locations (S-8), different line endings, a catch-all rule, a repo the policy
// already satisfies, and repos where each op variously applies, skips, or must
// be declared. A fleet is heterogeneous or it is not a fleet.
func idemFleet(t *testing.T) []idemRepo {
	t.Helper()
	specs := []struct {
		name  string
		files map[string]string
	}{
		// Everything applies: workflows, terraform and docs all present.
		{"root-full", map[string]string{
			"CODEOWNERS":               "* @org/base\n",
			".github/workflows/ci.yml": "on: push\n",
			"infra/main.tf":            "resource {}\n",
			"docs/guide.md":            "# guide\n",
		}},
		// Governed by .github/CODEOWNERS, not the root file.
		{"dotgithub", map[string]string{
			".github/CODEOWNERS":       "/src/ @org/app\n",
			".github/workflows/ci.yml": "on: push\n",
			"src/a.go":                 "package a\n",
		}},
		// Governed by docs/CODEOWNERS, which the docs op also matches.
		{"docsdir", map[string]string{
			"docs/CODEOWNERS": "# owners\n",
			"docs/guide.md":   "# guide\n",
			"a.go":            "package a\n",
		}},
		// Nothing matches: one declare, two skips.
		{"bare", map[string]string{
			"CODEOWNERS": "/lib/ @org/lib\n",
			"lib/x.go":   "package lib\n",
		}},
		// CRLF throughout — a line ending re-normalized on every pass is
		// growth's quieter sibling.
		{"crlf", map[string]string{
			"CODEOWNERS":    "# crlf\r\n/api/ @org/api\r\n",
			"api/a.go":      "package api\n",
			"infra/b.tf":    "resource {}\n",
			"api/README.md": "# api\n",
		}},
		// Comments, blank lines, tabs, trailing whitespace (INV-5 surface).
		{"messy", map[string]string{
			"CODEOWNERS":              "# header\n\n/x/\t@a   # inline\n   \n",
			"x/a.go":                  "package x\n",
			".github/workflows/w.yml": "on: push\n",
		}},
		// Already correct on pass 1 — the common fleet outcome.
		{"converged", map[string]string{
			"CODEOWNERS":              "/.github/workflows/ @org/ci\n",
			".github/workflows/w.yml": "on: push\n",
			"README.md":               "# r\n",
		}},
		// A catch-all rule: a declared line must land AFTER it, or it is dead.
		{"catchall", map[string]string{
			"CODEOWNERS": "* @org/everyone\n",
			"main.go":    "package main\n",
		}},
	}
	fleet := make([]idemRepo, 0, len(specs))
	for _, s := range specs {
		fleet = append(fleet, idemRepo{name: s.name, dir: initRepo(t, s.files)})
	}
	return fleet
}

// idemTotalSize is the fleet-wide byte total — the number that must not creep.
func idemTotalSize(t *testing.T, fleet []idemRepo) int {
	t.Helper()
	total := 0
	for _, r := range fleet {
		total += len(idemBytes(t, r.dir))
	}
	return total
}

// idemLineIndex returns the index of the first line containing sub, or -1.
func idemLineIndex(content, sub string) int {
	for i, line := range strings.Split(content, "\n") {
		if strings.Contains(line, sub) {
			return i
		}
	}
	return -1
}

// idemCountLines counts lines containing sub — one is convergence, two is the
// bug this file is named after.
func idemCountLines(content, sub string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The headline claims.
// ---------------------------------------------------------------------------

// SPEC R-19: one policy, eight unalike repos, run twice. After the second pass
// every repo must report a status that claims nothing was written, exit 0, and
// hold a BYTE-IDENTICAL CODEOWNERS. Comparing resolved ownership instead would
// pass while each file grew by a line a night: a `declare` line matches no
// tracked path, so it moves no path's resolution and the planner's
// `bytes.Equal(afterBytes, content)` no-op detector never sees it.
func TestR19_FleetTwoPassesByteIdentical(t *testing.T) {
	policy := idemPolicy(t, idemFleetPolicyJSON)
	fleet := idemFleet(t)

	after1 := map[string][]byte{}
	for _, r := range fleet {
		code, rec := idemPass(t, r.dir, "--policy", policy)
		if code != cli.ExitOK {
			t.Fatalf("%s pass 1: exit %d, want 0 (status %q, error %q) — every op is skip or declare, so no repo may refuse",
				r.name, code, rec.Status, rec.Error)
		}
		after1[r.name] = idemBytes(t, r.dir)
	}

	for _, r := range fleet {
		code, rec := idemPass(t, r.dir, "--policy", policy)
		idemConverged(t, r.name, 2, code, rec)
		idemSameBytes(t, r.name+" pass 2", after1[r.name], idemBytes(t, r.dir))
		if rec.PathsChanged != 0 {
			t.Errorf("%s pass 2: paths_changed = %d, want 0", r.name, rec.PathsChanged)
		}
	}
}

// SPEC R-19: the same fleet, five passes. Two passes is not enough evidence —
// a bug that appends only on odd passes, or one that flips a file between two
// spellings (line endings, owner order, a re-sorted rule), is byte-identical
// every other run and survives a two-run test forever. Five passes catch both:
// every pass from the second on must be byte-identical to the second, and no
// file may ever be larger than it was after pass 1.
func TestR19_FleetFivePassesNeverGrow(t *testing.T) {
	policy := idemPolicy(t, idemFleetPolicyJSON)
	fleet := idemFleet(t)

	snaps := map[string][][]byte{}
	for pass := 1; pass <= 5; pass++ {
		for _, r := range fleet {
			code, rec := idemPass(t, r.dir, "--policy", policy)
			if pass == 1 {
				if code != cli.ExitOK {
					t.Fatalf("%s pass 1: exit %d (status %q, error %q)", r.name, code, rec.Status, rec.Error)
				}
			} else {
				idemConverged(t, r.name, pass, code, rec)
			}
			snaps[r.name] = append(snaps[r.name], idemBytes(t, r.dir))
		}
	}

	for _, r := range fleet {
		s := snaps[r.name]
		for i := 2; i < len(s); i++ {
			idemSameBytes(t, fmt.Sprintf("%s pass %d vs pass 2", r.name, i+1), s[1], s[i])
		}
		for i := 1; i < len(s); i++ {
			if len(s[i]) > len(s[0]) {
				t.Errorf("%s: pass %d is %d bytes, pass 1 was %d — the file grows every run; at a line a night this reaches S-4's 3 MB cliff and GitHub stops loading it",
					r.name, i+1, len(s[i]), len(s[0]))
			}
		}
	}
}

// SPEC R-19 (S-4): the fleet's TOTAL CODEOWNERS size after five passes must
// equal its total after one. Size is the number the failure is measured in:
// past 3 MB GitHub silently stops loading a CODEOWNERS file entirely, so an
// unbounded appender does not degrade ownership, it deletes it — with no
// error, on every repo the job ever touched.
func TestR19_FleetSizeStableAfterFivePasses(t *testing.T) {
	policy := idemPolicy(t, idemFleetPolicyJSON)
	fleet := idemFleet(t)

	for _, r := range fleet {
		if code, rec := idemPass(t, r.dir, "--policy", policy); code != cli.ExitOK {
			t.Fatalf("%s pass 1: exit %d (status %q, error %q)", r.name, code, rec.Status, rec.Error)
		}
	}
	size1 := idemTotalSize(t, fleet)

	for pass := 2; pass <= 5; pass++ {
		for _, r := range fleet {
			code, rec := idemPass(t, r.dir, "--policy", policy)
			idemConverged(t, r.name, pass, code, rec)
		}
	}
	if size5 := idemTotalSize(t, fleet); size5 != size1 {
		t.Errorf("fleet total CODEOWNERS size: %d bytes after pass 1, %d after pass 5 (+%d) — unbounded growth ends at S-4's 3 MB cliff, where GitHub stops loading the file at all",
			size1, size5, size5-size1)
	}
}

// ---------------------------------------------------------------------------
// Per-op-kind idempotence. Each kind synthesizes edits differently, so each
// gets its own two-run byte comparison.
// ---------------------------------------------------------------------------

// SPEC R-19 (INV-4): add_owner run twice is byte-identical. A second append of
// the same owner would be invisible in resolution — `@a @b @b` resolves to the
// same set as `@a @b` — and would grow the line on every nightly run.
func TestR19_AddOwnerTwiceByteIdentical(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "/x/ @a\n",
		"x/a.go":     "package x\n",
	})
	code, rec := idemPass(t, repo, "--op", "add_owner(/x/, @b)")
	if code != cli.ExitOK {
		t.Fatalf("pass 1: exit %d (status %q, error %q)", code, rec.Status, rec.Error)
	}
	first := idemBytes(t, repo)

	code, rec = idemPass(t, repo, "--op", "add_owner(/x/, @b)")
	idemConverged(t, "add_owner", 2, code, rec)
	idemSameBytes(t, "add_owner", first, idemBytes(t, repo))
	if n := idemCountLines(string(first), "@b"); n != 1 {
		t.Errorf("@b appears on %d lines, want 1", n)
	}
}

// SPEC R-19: set_owners run twice is byte-identical, including the S-9
// zero-owner form `set_owners(scope, [])`. The empty list is the dangerous
// one: it writes a rule with no owners, so "did this already apply?" cannot be
// answered by asking who owns the path — both states answer "nobody".
func TestR19_SetOwnersTwiceByteIdentical(t *testing.T) {
	t.Run("named_owners", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS": "/x/ @a\n*.tf @infra\n",
			"x/main.tf":  "resource {}\n",
			"x/app.go":   "package x\n",
			"other.tf":   "resource {}\n",
		})
		code, rec := idemPass(t, repo, "--op", "set_owners(/x/, [@b, @c])")
		if code != cli.ExitOK {
			t.Fatalf("pass 1: exit %d (status %q, error %q)", code, rec.Status, rec.Error)
		}
		first := idemBytes(t, repo)

		code, rec = idemPass(t, repo, "--op", "set_owners(/x/, [@b, @c])")
		idemConverged(t, "set_owners", 2, code, rec)
		idemSameBytes(t, "set_owners", first, idemBytes(t, repo))
	})

	t.Run("empty_list_S9", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			"CODEOWNERS": "/x/ @a\n",
			"x/a.go":     "package x\n",
		})
		code, rec := idemPass(t, repo, "--op", "set_owners(/x/, [])")
		if code != cli.ExitOK {
			t.Fatalf("pass 1: exit %d (status %q, error %q)", code, rec.Status, rec.Error)
		}
		first := idemBytes(t, repo)

		code, rec = idemPass(t, repo, "--op", "set_owners(/x/, [])")
		idemConverged(t, "set_owners([])", 2, code, rec)
		idemSameBytes(t, "set_owners([])", first, idemBytes(t, repo))
	})
}

// SPEC R-19 (R-6): remove_owner run twice is byte-identical under every
// --on-empty policy. `inherit` deletes a rule and `unowned` writes a
// zero-owner one — two different files that resolve identically, which is
// exactly the pair a resolution-level idempotence check cannot tell apart. The
// second run, where the owner is already gone, must write nothing at all.
func TestR19_RemoveOwnerTwiceEachOnEmptyPolicy(t *testing.T) {
	cases := []struct {
		policy string
		files  map[string]string
		op     string
	}{
		// Removal does not empty the set, so the policy never fires.
		{"error", map[string]string{
			"CODEOWNERS": "/x/ @a @b\n",
			"x/a.go":     "package x\n",
		}, "remove_owner(/x/, @b)"},
		// Removal empties the set: the rule goes away and @base is inherited.
		{"inherit", map[string]string{
			"CODEOWNERS": "* @base\n/x/ @a\n",
			"x/a.go":     "package x\n",
			"y.txt":      "y\n",
		}, "remove_owner(/x/, @a)"},
		// Removal empties the set: the path becomes explicitly unowned.
		{"unowned", map[string]string{
			"CODEOWNERS": "* @base\n/x/ @a\n",
			"x/a.go":     "package x\n",
			"y.txt":      "y\n",
		}, "remove_owner(/x/, @a)"},
	}
	for _, c := range cases {
		t.Run(c.policy, func(t *testing.T) {
			repo := initRepo(t, c.files)
			code, rec := idemPass(t, repo, "--op", c.op, "--on-empty", c.policy)
			if code != cli.ExitOK {
				t.Fatalf("pass 1: exit %d (status %q, error %q)", code, rec.Status, rec.Error)
			}
			first := idemBytes(t, repo)

			code, rec = idemPass(t, repo, "--op", c.op, "--on-empty", c.policy)
			idemConverged(t, "remove_owner --on-empty="+c.policy, 2, code, rec)
			idemSameBytes(t, "remove_owner --on-empty="+c.policy, first, idemBytes(t, repo))
		})
	}
}

// SPEC R-19: rename_owner run twice is byte-identical. rename_owner derives
// its scope from CURRENT ownership, so the second run sees an old name that
// owns nothing — historically the churn case, where an empty scope produced an
// edit anyway and the file was rewritten on every nightly pass.
func TestR19_RenameOwnerTwiceByteIdentical(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "/x/ @old\n/y/ @old @other\n",
		"x/a.go":     "package x\n",
		"y/b.go":     "package y\n",
	})
	code, rec := idemPass(t, repo, "--op", "rename_owner(@old, @new)")
	if code != cli.ExitOK {
		t.Fatalf("pass 1: exit %d (status %q, error %q)", code, rec.Status, rec.Error)
	}
	first := idemBytes(t, repo)
	if strings.Contains(string(first), "@old") {
		t.Fatalf("pass 1 left @old behind:\n%s", first)
	}

	code, rec = idemPass(t, repo, "--op", "rename_owner(@old, @new)")
	idemConverged(t, "rename_owner", 2, code, rec)
	idemSameBytes(t, "rename_owner re-run", first, idemBytes(t, repo))
	if n := idemCountLines(string(first), "@new"); n != 2 {
		t.Errorf("@new appears on %d lines, want 2 (one per renamed rule)", n)
	}
}

// SPEC R-19: a rename whose old and new names are identical must never touch
// the file. `rename_owner(@a, @a)` is the self-rename churn case: a
// no-difference rewrite that a nightly job would apply, commit, and PR forever.
// ops.Parse rejects it outright today, which under sync's contract is exit 3
// (a broken policy fails identically on all 100 repos) — so either exit is
// defensible, but the two runs must agree with each other and the bytes must
// not move.
func TestR19_RenameOwnerSelfRenameNeverWrites(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "/x/ @a\n",
		"x/a.go":     "package x\n",
	})
	original := idemBytes(t, repo)

	code1, _, _ := idemSyncRaw(t, repo, "--op", "rename_owner(@a, @a)")
	idemSameBytes(t, "self-rename run 1", original, idemBytes(t, repo))
	code2, _, _ := idemSyncRaw(t, repo, "--op", "rename_owner(@a, @a)")
	idemSameBytes(t, "self-rename run 2", original, idemBytes(t, repo))

	if code1 != code2 {
		t.Errorf("self-rename exit codes differ between identical runs: %d then %d", code1, code2)
	}
	if code1 != cli.ExitOK && code1 != cli.ExitInvalid {
		t.Errorf("self-rename: exit %d, want 0 (nothing to do) or 3 (rejected as a broken op); sync returns only 0, 2 and 3", code1)
	}
}

// ---------------------------------------------------------------------------
// on_zero_match: one test per value. `declare` is the critical one.
// ---------------------------------------------------------------------------

// SPEC R-19/R-21: `on_zero_match: require` is stable across runs. A scope that
// matches nothing fails THIS repo (exit 2 — whether a path exists is the most
// repo-specific fact there is) and must leave the file untouched, identically,
// every night. A refusal that half-wrote something would be worse than the
// growth bug.
func TestR19_ZeroMatchRequireStableAcrossRuns(t *testing.T) {
	policy := idemPolicy(t, `{
  "version": 1,
  "ops": [ { "id": "ghost", "op": "add_owner(/ghost/, @org/ghost)", "on_zero_match": "require" } ]
}`)
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "/x/ @a\n",
		"x/a.go":     "package x\n",
	})
	original := idemBytes(t, repo)

	for pass := 1; pass <= 2; pass++ {
		code, rec, _ := idemSync(t, repo, "--policy", policy)
		if code != cli.ExitRefused {
			t.Errorf("pass %d: exit %d, want 2 — a zero-match under `require` is this repo's problem, not the policy's (status %q)",
				pass, code, rec.Status)
		}
		if rec.Status != cli.StatusRefused {
			t.Errorf("pass %d: status %q, want %q", pass, rec.Status, cli.StatusRefused)
		}
		idemSameBytes(t, fmt.Sprintf("require pass %d", pass), original, idemBytes(t, repo))
	}
}

// SPEC R-19/R-21: `on_zero_match: skip` is a no-op on every run — exit 0,
// status "skipped" (never "unchanged": a policy that matches nothing anywhere
// must not read as "100 repos already correct"), and the file untouched. Twice,
// because a skip that quietly wrote a rule anyway would be growth with a
// reassuring status attached.
func TestR19_ZeroMatchSkipIsNoOpBothRuns(t *testing.T) {
	policy := idemPolicy(t, `{
  "version": 1,
  "ops": [ { "id": "tf", "op": "add_owner(**/*.tf, @org/infra)", "on_zero_match": "skip" } ]
}`)
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "/x/ @a\n",
		"x/a.go":     "package x\n",
	})
	original := idemBytes(t, repo)

	for pass := 1; pass <= 2; pass++ {
		code, rec := idemPass(t, repo, "--policy", policy)
		if code != cli.ExitOK {
			t.Errorf("pass %d: exit %d, want 0 (status %q, error %q)", pass, code, rec.Status, rec.Error)
		}
		if rec.Status != cli.StatusSkipped {
			t.Errorf("pass %d: status %q, want %q", pass, rec.Status, cli.StatusSkipped)
		}
		if rec.OpsSkipped != 1 || rec.OpsApplied != 0 {
			t.Errorf("pass %d: ops_applied=%d ops_skipped=%d, want 0 and 1", pass, rec.OpsApplied, rec.OpsSkipped)
		}
		idemSameBytes(t, fmt.Sprintf("skip pass %d", pass), original, idemBytes(t, repo))
	}
}

// SPEC R-19/R-21/INV-6: `on_zero_match: declare` writes its rule exactly ONCE
// and never again. This is the case the existing no-op detector cannot see: a
// declared rule matches no tracked file, so it changes no tracked path's
// resolution, and `bytes.Equal(afterBytes, content)` in plan.Build compares a
// file the declare line was already appended to. Four passes, one line: the
// difference between a policy that converges and a nightly job that adds a
// line to 100 CODEOWNERS files until GitHub stops loading them (S-4).
func TestR19_ZeroMatchDeclareWritesOnceThenNever(t *testing.T) {
	policy := idemPolicy(t, `{
  "version": 1,
  "ops": [ { "id": "ci", "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare" } ]
}`)
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "/x/ @a\n",
		"x/a.go":     "package x\n",
	})
	original := idemBytes(t, repo)

	code, rec := idemPass(t, repo, "--policy", policy)
	if code != cli.ExitOK {
		t.Fatalf("pass 1: exit %d (status %q, error %q)", code, rec.Status, rec.Error)
	}
	if rec.Status != cli.StatusApplied {
		t.Errorf("pass 1: status %q, want %q — declare writes the rule", rec.Status, cli.StatusApplied)
	}
	if len(rec.Ops) != 1 || rec.Ops[0].Proven != "structural" {
		t.Errorf("pass 1: ops = %+v, want one op proven %q (INV-6: nothing in the tree to prove it against)",
			rec.Ops, "structural")
	}
	first := idemBytes(t, repo)
	if bytes.Equal(first, original) {
		t.Fatalf("pass 1 wrote nothing; declare must write the rule for files that do not exist yet")
	}

	for pass := 2; pass <= 4; pass++ {
		code, rec := idemPass(t, repo, "--policy", policy)
		idemConverged(t, "declare", pass, code, rec)
		idemSameBytes(t, fmt.Sprintf("declare pass %d", pass), first, idemBytes(t, repo))
	}
	if n := idemCountLines(string(idemBytes(t, repo)), "@org/ci"); n != 1 {
		t.Errorf("@org/ci appears on %d lines after four passes, want exactly 1 — this is the unbounded-growth defect", n)
	}
}

// SPEC R-19: a realistic policy — three declared rules interleaved with two
// ordinary ops — is byte-identical on a second run AND keeps its declared
// lines in the same relative order. Re-ordering is a silent correctness change:
// the last matching rule wins in CODEOWNERS, so two runs that shuffle appended
// rules produce different ownership from the same policy, and every nightly run
// churns a diff for reviewers.
func TestR19_MixedPolicyDeclareLinesDoNotReorder(t *testing.T) {
	policy := idemPolicy(t, `{
  "version": 1,
  "name": "mixed",
  "ops": [
    { "id": "ci",     "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare" },
    { "id": "tf",     "op": "add_owner(**/*.tf, @org/infra)",          "on_zero_match": "declare" },
    { "id": "charts", "op": "add_owner(/charts/, @org/k8s)",           "on_zero_match": "declare" },
    "add_owner(/src/, @org/app)",
    "add_owner(/lib/, @org/lib)"
  ]
}`)
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @org/base\n",
		"src/a.go":   "package a\n",
		"lib/b.go":   "package lib\n",
	})

	code, rec := idemPass(t, repo, "--policy", policy)
	if code != cli.ExitOK {
		t.Fatalf("pass 1: exit %d (status %q, error %q)", code, rec.Status, rec.Error)
	}
	first := idemBytes(t, repo)

	declared := []string{"@org/ci", "@org/infra", "@org/k8s"}
	order1 := make([]int, len(declared))
	for i, owner := range declared {
		order1[i] = idemLineIndex(string(first), owner)
		if order1[i] < 0 {
			t.Fatalf("pass 1 did not declare %s:\n%s", owner, first)
		}
		if n := idemCountLines(string(first), owner); n != 1 {
			t.Errorf("pass 1: %s on %d lines, want 1", owner, n)
		}
	}
	for i := 1; i < len(order1); i++ {
		if order1[i] <= order1[i-1] {
			t.Errorf("declared rules are out of policy order after pass 1: %s at line %d, %s at line %d\n%s",
				declared[i-1], order1[i-1], declared[i], order1[i], first)
		}
	}

	code, rec = idemPass(t, repo, "--policy", policy)
	idemConverged(t, "mixed policy", 2, code, rec)
	second := idemBytes(t, repo)
	idemSameBytes(t, "mixed policy", first, second)
	for i, owner := range declared {
		if got := idemLineIndex(string(second), owner); got != order1[i] {
			t.Errorf("%s moved from line %d to line %d between identical passes — declared rules must not reorder, the last matching rule wins",
				owner, order1[i], got)
		}
	}
}

// SPEC R-19: convergence from a partially-applied state. A file that already
// satisfies SOME of the policy — the normal state of a fleet mid-rollout, or
// of a repo a human edited by hand — must converge in ONE pass, leave the
// already-satisfied lines byte-untouched (INV-5), and be a no-op on the next.
// A tool that only converges after N passes has no fixed point a scheduled job
// can ever reach.
func TestR19_ConvergesFromPartiallyAppliedState(t *testing.T) {
	policy := idemPolicy(t, `{
  "version": 1,
  "ops": [
    "add_owner(/x/, @b)",
    "add_owner(/y/, @d)"
  ]
}`)
	repo := initRepo(t, map[string]string{
		// /x/ already satisfies op 1; /y/ does not satisfy op 2.
		"CODEOWNERS": "# owners\n/x/ @a @b\n/y/ @c\n",
		"x/a.go":     "package x\n",
		"y/b.go":     "package y\n",
	})

	code, rec := idemPass(t, repo, "--policy", policy)
	if code != cli.ExitOK {
		t.Fatalf("pass 1: exit %d (status %q, error %q)", code, rec.Status, rec.Error)
	}
	first := string(idemBytes(t, repo))
	lines := strings.Split(first, "\n")
	if len(lines) < 3 || lines[0] != "# owners" || lines[1] != "/x/ @a @b" {
		t.Errorf("pass 1 rewrote lines it did not need to touch (INV-5):\n%s", first)
	}
	if idemLineIndex(first, "@d") < 0 {
		t.Errorf("pass 1 did not converge the unsatisfied op — @d is absent:\n%s", first)
	}
	if n := idemCountLines(first, "@b"); n != 1 {
		t.Errorf("@b on %d lines after pass 1, want 1 (it was already there)", n)
	}

	code, rec = idemPass(t, repo, "--policy", policy)
	idemConverged(t, "partially-applied", 2, code, rec)
	idemSameBytes(t, "partially-applied", []byte(first), idemBytes(t, repo))
}

// SPEC R-19: repeated --dry-run runs change nothing and say the same thing
// every time. --dry-run is the fleet preview — the only review step that
// exists at 100 repos — so a preview that differs run to run, or that writes
// while previewing, makes the review meaningless.
func TestR19_DryRunRepeatedIsInert(t *testing.T) {
	policy := idemPolicy(t, idemFleetPolicyJSON)
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":               "* @org/base\n",
		".github/workflows/ci.yml": "on: push\n",
		"infra/main.tf":            "resource {}\n",
	})
	original := idemBytes(t, repo)

	var firstRecord string
	for run := 1; run <= 3; run++ {
		code, rec, _ := idemSync(t, repo, "--policy", policy, "--dry-run")
		if code != cli.ExitOK {
			t.Errorf("dry run %d: exit %d, want 0 (status %q, error %q)", run, code, rec.Status, rec.Error)
		}
		idemSameBytes(t, fmt.Sprintf("dry run %d", run), original, idemBytes(t, repo))
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if run == 1 {
			firstRecord = string(b)
			continue
		}
		if string(b) != firstRecord {
			t.Errorf("dry run %d reported differently from dry run 1:\n%s\n%s", run, firstRecord, b)
		}
	}
}

// SPEC R-19: the actual scheduled-job shape — sync, merge the result, and let
// the NEXT run read what the last one wrote. This is the loop that turns a
// one-line-per-run bug into a 3 MB file, and it is the only test here where
// the second run's input is genuinely the first run's committed output rather
// than a working-tree edit.
func TestR19_SequentialSyncsReadPreviousOutput(t *testing.T) {
	policy := idemPolicy(t, idemFleetPolicyJSON)
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":               "# baseline\n* @org/base\n",
		".github/workflows/ci.yml": "on: push\n",
		"docs/guide.md":            "# guide\n",
	})

	code, rec := idemPass(t, repo, "--policy", policy)
	if code != cli.ExitOK {
		t.Fatalf("run 1: exit %d (status %q, error %q)", code, rec.Status, rec.Error)
	}
	rel := idemOwnersRel(t, repo)
	committed := idemGit(t, repo, "show", "HEAD:"+rel)
	if committed != string(idemBytes(t, repo)) {
		t.Fatalf("run 1's output was not committed intact; the second run would not read it")
	}

	code, rec = idemPass(t, repo, "--policy", policy)
	idemConverged(t, "scheduled job", 2, code, rec)
	if after := idemGit(t, repo, "show", "HEAD:"+rel); after != committed {
		t.Errorf("run 2 rewrote the committed file (%d → %d bytes) — every night this opens another no-op PR and grows the file\nbefore:\n%s\nafter:\n%s",
			len(committed), len(after), committed, after)
	}
	if len(rec.Changes) != 0 {
		t.Errorf("run 2 reported %d line change(s), want 0", len(rec.Changes))
	}
}
