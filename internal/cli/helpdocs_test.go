// `--help` against the pages it summarizes.
//
// The help text is the only reference an operator reads without leaving the
// terminal, so a flag it omits or a rule it overstates is a doc that has
// drifted — checked here the same way docexamples_test.go checks the console
// blocks. Every test below was written as a failing repro of a confirmed
// mismatch between `--help` and docs/COMMANDS.md, docs/AUDIT.md or
// docs/LINTING.md.
package cli_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// helpText returns the root `--help` output, which cli.Run writes to stdout.
func helpText(t *testing.T) string {
	t.Helper()
	code, stdout, stderr := runCLI(t, "--help")
	if code != 0 {
		t.Fatalf("--help: exit %d: %s", code, stderr)
	}
	return stdout
}

// usageText returns a verb's flag usage, which every verb writes to stderr.
func usageText(t *testing.T, verb string) string {
	t.Helper()
	code, stdout, stderr := runCLI(t, verb, "--help")
	if code != 0 {
		t.Fatalf("%s --help: exit %d", verb, code)
	}
	return stdout + stderr
}

// synopses splits a synopsis block — the indented command list in `--help` or
// the fenced one in COMMANDS.md — into command name → the flags it names.
func synopses(block string) map[string][]string {
	flagRe := regexp.MustCompile(`--[a-z-]+`)
	verbRe := regexp.MustCompile(`^\s{0,2}(sync|check|plan|apply|audit|lint|snapshot|verify|version)\s`)
	out := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(block, "\n") {
		if m := verbRe.FindStringSubmatch(line); m != nil {
			cur = m[1]
		} else if strings.TrimSpace(line) == "" || !strings.HasPrefix(line, "  ") {
			cur = ""
		}
		if cur == "" {
			continue
		}
		for _, f := range flagRe.FindAllString(line, -1) {
			out[cur] = append(out[cur], f)
		}
	}
	return out
}

// commandsDocSynopses reads the synopsis block COMMANDS.md opens with — the
// canonical list REFERENCE.md routes every flag question to.
func commandsDocSynopses(t *testing.T) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "COMMANDS.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, "```\nsync")
	if start < 0 {
		t.Fatal("COMMANDS.md no longer opens with a fenced synopsis block")
	}
	rest := doc[start+4:]
	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatal("unterminated synopsis block in COMMANDS.md")
	}
	return synopses(rest[:end])
}

// FINDING: `--help`'s synopses omit flags COMMANDS.md documents, so a flag that
// can refuse a run or redirect it at a different file is invisible to anyone
// who never opens the docs.
//
//	plan     … [--repo DIR] [--branch REF] [--file PATH] [--out plan.json]
//	audit    … [--cache-dir D] [--cache-ttl DUR] [--repo DIR] [--branch REF] [--file PATH]
//	snapshot [--repo DIR] [--branch REF] [--out snap.json]
//
// `plan` also takes `--max-size` (the S-4 cliff — over it the run is refused)
// and `--warn-size`; `snapshot` also takes `--file`, the one flag that makes it
// answer about a CODEOWNERS GitHub does not read; `audit` also takes the
// `--lint` cluster the help's own prose goes on to describe. COMMANDS.md names
// all four, and the same omission in that page is already pinned by
// TestCommandsDocListsEveryFlagThatChangesBehavior — the help kept it.
func TestHelpSynopsesCarryEveryFlagCommandsDocDocuments(t *testing.T) {
	help := synopses(helpText(t))
	doc := commandsDocSynopses(t)
	for _, verb := range []string{"sync", "check", "plan", "apply", "audit", "lint", "snapshot", "verify"} {
		have := map[string]bool{}
		for _, f := range help[verb] {
			have[f] = true
		}
		var missing []string
		for _, f := range doc[verb] {
			if !have[f] {
				missing = append(missing, f)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("`--help`'s %s synopsis omits %s, which COMMANDS.md documents",
				verb, strings.Join(missing, ", "))
		}
	}
}

// FINDING: `--help` and `lint --help` both call the credentials unconditional —
// "it needs a token and --github-repo", "required: owner existence is not
// decidable offline" — but the offline tree-only mode is real and documented:
// with --remove-stale-paths and NEITHER credential, lint runs stage 3 alone
// (LINTING.md, "Errors you will actually hit"; AUDIT.md, "Offline tree-only
// mode"). An operator with no token reads the help and concludes the run below
// is impossible.
func TestHelpDisclosesTheOfflineLintMode(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/everyone\n/ghost/ @org/ghost-team\n",
		"a.md":               "",
	})
	t.Setenv("GITHUB_TOKEN", "")
	code, out, errOut := runCLI(t, "lint", "--repo", repo, "--remove-stale-paths", "--dry-run")
	if code != 4 {
		t.Fatalf("offline lint --remove-stale-paths --dry-run: exit %d, want 4 — the mode the help denies\nstdout: %s\nstderr: %s", code, out, errOut)
	}

	help := helpText(t)
	if !strings.Contains(help, "--remove-stale-paths alone") {
		t.Errorf("`--help` says lint \"needs a token and --github-repo\" with no exception, but the run above needed neither:\n%s", help)
	}
	usage := usageText(t, "lint")
	if strings.Contains(usage, "required: owner existence is not decidable offline") {
		t.Errorf("lint --help calls --github-repo flatly \"required\"; AUDIT.md says \"Required, bar the offline mode below\":\n%s", usage)
	}
}

// FINDING: `--help` gives `check` a code it cannot return.
//
//	sync/check use a coarser contract and return only:
//	            0 converged · 2 this repo needs a human · 3 the policy is broken
//
// `check` opens no repository at all (R-22), so "this repo needs a human" is
// not a verdict it can reach: it returns 0 or 3. COMMANDS.md says so —
// "It exits 0 for a valid policy, 3 for a broken one, and never 1" — and a CI
// gate written off the help's table has a branch that never runs.
func TestHelpDoesNotGiveCheckAnExitCodeItCannotReturn(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/everyone\n",
		"a.md":               "",
	})
	// The op whose scope matches nothing is `sync`'s exit 2 — the repo-specific
	// verdict. `check` reads no repository, so the same op is exit 0 there.
	if code, _, _ := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/nope/, @org/t)"); code != 2 {
		t.Fatalf("sync on a zero-match scope: exit %d, want 2", code)
	}
	if code, _, _ := runCLI(t, "check", "--op", "add_owner(/nope/, @org/t)"); code != 0 {
		t.Fatalf("check on the same op: exit %d, want 0 — check opens no repository", code)
	}

	help := helpText(t)
	line := ""
	for _, l := range strings.Split(help, "\n") {
		if strings.Contains(l, "coarser contract") {
			line = l
		}
	}
	if strings.Contains(line, "check") {
		t.Errorf("`--help` puts `check` under sync's three-code contract, whose 2 means \"this repo needs a human\" —\na verdict a verb that opens no repository cannot reach: %q", line)
	}
}

// FINDING: `sync --help` marks two of the three op-only flags "(only with
// --op)" and leaves `--create` unmarked, though passing it with `--policy` is
// exit 3 — the R-34 rule COMMANDS.md states for all three.
func TestSyncCreateUsageSaysItIsOpOnly(t *testing.T) {
	repo := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/everyone\n",
		"a.md":               "",
	})
	policy := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(policy, []byte(`{"version":1,"ops":["add_owner(/a.md, @org/t)"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", policy, "--create")
	if code != 3 {
		t.Fatalf("sync --policy --create: exit %d, want 3\n%s", code, errOut)
	}
	usage := usageText(t, "sync")
	create := ""
	lines := strings.Split(usage, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "-create") && i+1 < len(lines) {
			create = lines[i+1]
		}
	}
	if !strings.Contains(create, "only with --op") {
		t.Errorf("--on-empty and --max-paths-changed say \"(only with --op)\"; --create refuses the same way and does not: %q", create)
	}
}

// FINDING: `apply --help` renders --plan's value placeholder as "plan".
//
//	-plan plan
//	    	plan JSON produced by plan
//
// The flag package takes the back-quoted word in a usage string as the name of
// the value, and the usage back-quoted the verb. COMMANDS.md spells the flag
// `--plan plan.json` — a path — so the help names a value the reader has no way
// to supply.
func TestApplyUsageNamesAFileNotTheVerb(t *testing.T) {
	usage := usageText(t, "apply")
	if strings.Contains(usage, "-plan plan\n") {
		t.Errorf("apply --help calls --plan's value \"plan\"; COMMANDS.md documents it as a path, `--plan plan.json`:\n%s", usage)
	}
}

// FINDING: AUDIT.md's `lint` flag table — the page REFERENCE.md routes every
// "a lint flag" lookup to — never names `--policy`, so the flag that decides
// where the repair preferences come from (R-36's `lint` block) is documented
// only in COMMANDS.md's synopsis and POLICY-FILE.md's field table, neither of
// which a reader following that route opens.
func TestAuditDocNamesEveryLintFlag(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "AUDIT.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := string(b)
	if i := strings.Index(section, "\n## `lint`"); i >= 0 {
		section = section[i:]
	} else {
		t.Fatal("AUDIT.md no longer has a `lint` section")
	}
	flagRe := regexp.MustCompile(`(?m)^\s+-([a-z-]+)`)
	for _, m := range flagRe.FindAllStringSubmatch(usageText(t, "lint"), -1) {
		if !strings.Contains(section, "--"+m[1]) {
			t.Errorf("lint defines --%s; AUDIT.md's lint section never names it", m[1])
		}
	}
}
