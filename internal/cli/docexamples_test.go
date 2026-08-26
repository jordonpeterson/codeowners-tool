// The documented console blocks, executed. Each test below rebuilds the repo a
// doc page describes, runs the command it prints, and compares against the
// output the page claims — so a doc that has drifted from the tool fails here
// rather than in a reader's terminal.
//
// Every test is a FAILING repro of a confirmed drift, written first per
// CONTRIBUTING.md. Where the doc is the wrong half, the doc comment says so:
// these pin the mismatch, not a preferred side.
package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FINDING: docs/OPERATIONS.md's only worked example of naming several owners in
// one op prints the output of a DIFFERENT, one-op policy.
//
// The policy shown has two ops — the bracketed-list spelling and the `owners`
// array spelling, which the page is there to introduce — and the block under
// it reads:
//
//	$ codeowners-tool sync --policy ownership.json
//	applied: 1 op(s) applied, 0 skipped; 1 line change(s), 1 path(s) change owners
//	$ cat CODEOWNERS
//	/services/api/   @org/api-team @org/platform @org/sre
//
// The real run reports 2 ops / 2 line changes / 2 paths, prints a per-op line
// for each, and writes a `/docs/` rule the block did not show. A reader
// comparing the two concludes the `owners`-array op wrote nothing — the exact
// misreading the page exists to prevent.
//
// The assertion runs the documented policy against the documented tree and
// requires the PAGE to show what the tool really prints. Asserting the reverse
// — that the tool prints what the page claims — was this test's own first
// mistake: that form can only ever be satisfied by changing the tool, and here
// the tool is right.
func TestOperationsDocOwnerListExample(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":        "/services/api/   @org/api-team\n",
		"services/api/m.go": "",
		"docs/d.md":         "",
	})
	policy := filepath.Join(t.TempDir(), "ownership.json")
	if err := os.WriteFile(policy, []byte(`{ "version": 1,
  "ops": [ "add_owner(/services/api/, [@org/platform, @org/sre])",
           { "op": "add_owner(/docs/)", "owners": ["@org/platform", "@org/sre"] } ] }`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--policy", policy)
	if code != 0 {
		t.Fatalf("sync: exit %d: %s", code, stderr)
	}
	got, err := os.ReadFile(filepath.Join(repo, "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}

	doc := readDocPage(t, "OPERATIONS.md")
	wantDocumented(t, doc, "OPERATIONS.md", stdout, "the `sync` output")
	wantDocumented(t, doc, "OPERATIONS.md", string(got), "the resulting CODEOWNERS")
}

// FINDING: docs/GUIDE.md's bootstrap example drops the per-op lines from both
// blocks it prints — including the one line the prose then tells the reader to
// go and look for.
//
//	$ codeowners-tool check --policy bootstrap.json
//	ok: bootstrap.json — 4 op(s), no policy errors
//	$ codeowners-tool sync --policy bootstrap.json
//	applied: 4 op(s) applied, 0 skipped; 4 line change(s), 4 path(s) change owners
//	  created a new CODEOWNERS file
//
// `check` also echoes each op's resolved `on_zero_match` (this policy has a
// `declare`, so the echo fires), and `sync` also prints `ops[0..3]`, the last
// of them `applied (proven: structural)` — which GUIDE.md's own next paragraph
// says to look for. FLEET.md shows both echoes and reproduces exactly, so
// GUIDE was the outlier.
//
// The fixture is the four files the section's prose describes, so every number
// in the block — `4 path(s) change owners` included — is the real one for the
// repo a reader following along would have.
func TestGuideBootstrapExample(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"README.md":            "",
		"docs/guide.md":        "",
		"services/api/main.go": "",
		"services/web/app.ts":  "",
	})
	policy := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(policy, []byte(`{
  "version": 1,
  "name": "bootstrap ownership",
  "create": true,
  "ops": [
    "add_owner(*, @org/everyone)",
    "add_owner(/services/api/, @org/api-team)",
    "add_owner(/docs/, @org/docs-team)",
    { "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare" }
  ]
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	doc := readDocPage(t, "GUIDE.md")

	code, checkOut, stderr := runCLI(t, "check", "--policy", policy)
	if code != 0 {
		t.Fatalf("check: exit %d: %s", code, stderr)
	}
	// `check` echoes the policy path it was given, and the page shows the
	// bare name a reader standing beside the file would type. The t.TempDir()
	// prefix is this harness's, not the tool's.
	checkOut = strings.ReplaceAll(checkOut, policy, filepath.Base(policy))
	wantDocumented(t, doc, "GUIDE.md", checkOut, "the `check` output")

	code, syncOut, stderr := runCLI(t, "sync", "--repo", repo, "--policy", policy)
	if code != 0 {
		t.Fatalf("sync: exit %d: %s", code, stderr)
	}
	wantDocumented(t, doc, "GUIDE.md", syncOut, "the `sync` output")
}

// readDocPage reads a page from docs/, two levels above this package.
func readDocPage(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wantDocumented asserts the page shows what the tool really printed.
//
// Line-wise rather than as one block, because a console block interleaves
// `$ command` lines with output and the two cannot be compared whole. It is
// also the more useful failure: it names the one line that drifted instead of
// printing two blocks to diff by eye.
func wantDocumented(t *testing.T, doc, page, got, what string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.Contains(doc, line) {
			t.Errorf("%s does not show %s as the tool prints it.\nmissing line:\n\t%s\nthe real output was:\n%s",
				page, what, line, got)
		}
	}
}

// FINDING: docs/GUIDE.md documents `apply` as naming the CODEOWNERS
// repo-relative; it prints an absolute path.
//
//	documented: applied: .github/CODEOWNERS (58 → 101 bytes)
//	actual:     applied: /home/me/clones/api-service/.github/CODEOWNERS (58 → 101 bytes)
//
// The documented form is unreachable: apply joins the plan's `repo`, and
// COMMANDS.md states that field is recorded absolute. Every other number in
// the block still matches, so this is one path, and it is a real difference to
// anyone diffing rollout logs — the repo-relative name is stable across
// machines and the absolute one is not.
func TestApplyPrintsTheDocumentedCodeownersPath(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "# owners\n*  @org/everyone\n/services/api/   @org/api-team\n",
		"services/api/main.go": "",
		"services/web/app.ts":  "",
	})
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if code, _, e := runCLI(t, "plan", "--repo", repo,
		"--op", "add_owner(/services/web/, @org/web-team)", "--out", planPath); code != 0 {
		t.Fatalf("plan: exit %d: %s", code, e)
	}

	code, stdout, stderr := runCLI(t, "apply", "--plan", planPath)
	if code != 0 {
		t.Fatalf("apply: exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "applied: .github/CODEOWNERS (") {
		t.Errorf("GUIDE.md documents `applied: .github/CODEOWNERS (…)`; the run prints an absolute path:\n%s", stdout)
	}
}

// FINDING: docs/LINTING.md documents an error the `lint` verb cannot produce.
//
// Under "every error lint can print", the page lists:
//
//	**`--cache-dir is not available with --lint`** (exit 3). A cached "this
//	owner does not exist" is served without revalidation … here it deletes an
//	owner.
//
// `lint`'s flagset never defines `--cache-dir`, so the run dies in the flag
// package with `flag provided but not defined: -cache-dir` and a usage dump —
// no mention of caching, revalidation, or why the combination is refused. The
// documented message is reachable only through the older `audit --lint`
// spelling. The reasoning is worth keeping, so the fix is to make `lint`
// recognise the flag and refuse it with that sentence.
func TestLintRefusesCacheDirWithTheDocumentedReason(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/every\n",
		"a.md":               "",
	})
	t.Setenv("GITHUB_TOKEN", "")
	_, stdout, stderr := runCLI(t, "lint", "--repo", repo, "--cache-dir", t.TempDir(), "--remove-stale-paths")
	out := stdout + stderr
	if !strings.Contains(out, "--cache-dir is not available") {
		t.Errorf("LINTING.md documents `--cache-dir is not available with --lint` as one of lint's errors;\n"+
			"lint does not define the flag, so the reader gets a bare flag-package error instead:\n%s", out)
	}
}

// FINDING: docs/COMMANDS.md's synopses omit flags that change what the command
// does, and REFERENCE.md routes every flag lookup there — so the flags are
// documented nowhere.
//
//	plan     … [--repo DIR] [--branch REF] [--file PATH] [--out plan.json]
//	snapshot [--repo DIR] [--branch REF] [--out snap.json]
//
// `plan` also accepts `--max-size` (default 3,000,000 — the S-4 cliff) and
// `--warn-size` (2,500,000), both of which can turn a run into a refusal or a
// warning. `snapshot` also accepts `--file`, which decides which CODEOWNERS
// the ownership map is derived from — the one flag that can make `snapshot`
// answer about a file GitHub does not read.
func TestCommandsDocListsEveryFlagThatChangesBehavior(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "COMMANDS.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, flag := range []string{"--max-size", "--warn-size"} {
		if !strings.Contains(doc, flag) {
			t.Errorf("COMMANDS.md never mentions plan's %s, and REFERENCE.md sends every flag question here", flag)
		}
	}
	synopsis := "snapshot [--repo DIR] [--branch REF] [--out snap.json]"
	if strings.Contains(doc, synopsis) {
		t.Errorf("COMMANDS.md gives snapshot's synopsis as %q, omitting --file, which decides which\n"+
			"CODEOWNERS the ownership map comes from", synopsis)
	}
}
