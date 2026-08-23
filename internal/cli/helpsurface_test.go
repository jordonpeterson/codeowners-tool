package cli_test

import (
	"strings"
	"testing"
)

// A blind operator, given only --help, could not learn the two things they
// must type to do anything at all: the op grammar and the policy schema.
// Both were derivable only by submitting something wrong and reading the
// refusal — learnable through failure, unreadable before the first attempt.

func helpOf(t *testing.T, args ...string) string {
	t.Helper()
	_, out, errb := runCLI(t, append(args, "--help")...)
	return out + errb
}

// set_owners and rename_owner appeared ZERO times across every help surface;
// add_owner appeared once, as an example. The verb list was reachable only
// from `unknown op "x" (want ...)`.
func TestHelp_OpGrammarIsDiscoverable(t *testing.T) {
	for _, verb := range []string{"sync", "check", "plan"} {
		h := helpOf(t, verb)
		for _, want := range []string{"add_owner", "set_owners", "remove_owner", "rename_owner"} {
			if !strings.Contains(h, want) {
				t.Errorf("%s --help does not mention %s; --op is this command's primary interface", verb, want)
			}
		}
		// The bracket rule cost a round trip to discover every time.
		if !strings.Contains(h, "[@") {
			t.Errorf("%s --help does not show the bracketed owner-list form", verb)
		}
	}
}

// `--policy FILE (R-20)` was the whole description. Nothing said the file
// needs "version", needs a non-empty "ops", or that an op is a string.
func TestHelp_PolicySchemaIsDiscoverable(t *testing.T) {
	for _, verb := range []string{"sync", "check"} {
		h := helpOf(t, verb)
		for _, want := range []string{`"version"`, `"ops"`} {
			if !strings.Contains(h, want) {
				t.Errorf("%s --help never names the required policy field %s", verb, want)
			}
		}
	}
}

// Every verb opened straight into an alphabetically sorted flag dump. Not one
// said what it did; the top-level prose described only audit and lint.
func TestHelp_EveryVerbStatesItsPurpose(t *testing.T) {
	for _, verb := range []string{"sync", "check", "plan", "apply", "audit", "lint", "snapshot", "verify"} {
		h := helpOf(t, verb)
		first := strings.TrimSpace(strings.SplitN(h, "\n", 2)[0])
		if first == "" || strings.HasPrefix(first, "-") || strings.HasPrefix(first, "Usage of") {
			t.Errorf("%s --help opens with %q — no statement of what the verb does", verb, first)
		}
	}
}

// Rule IDs (R-6, S-7, R-20 …) are cited throughout the flag descriptions and
// defined nowhere, and help named no document to resolve them in. The only
// URLs in the entire help output were the GitHub API base URL.
func TestHelp_PointsSomewhereForTheRuleIDsItCites(t *testing.T) {
	_, out, _ := runCLI(t, "--help")
	if !strings.Contains(out, "docs/") {
		t.Errorf("top-level help cites rule IDs but names no document that defines them:\n%s", out)
	}
}

// Not one end-to-end invocation appeared anywhere in help. An examples block
// is what closes the op-grammar and snapshot-ordering gaps for a reader who
// is scanning rather than experimenting.
func TestHelp_TopLevelCarriesExamples(t *testing.T) {
	_, out, _ := runCLI(t, "--help")
	if !strings.Contains(out, "EXAMPLES") {
		t.Error("top-level help has no EXAMPLES section")
	}
	// Search inside the examples block: every verb name also appears in the
	// synopsis above it, which would satisfy a naive ordering check.
	ex := out[strings.Index(out, "EXAMPLES"):]
	i, j, k := strings.Index(ex, "snapshot"), strings.Index(ex, "git "), strings.Index(ex, "verify")
	if i < 0 || j < 0 || k < 0 || !(i < j && j < k) {
		t.Errorf("no snapshot → commit → verify example; that ordering is what makes the gate real\n%s", ex)
	}
}

// Go calls fs.Usage on every flag error, not only on --help. A mistyped flag
// needs the error and the flag list, not the whole op grammar and policy
// schema underneath it.
func TestHelp_ReferenceBlocksOnlyOnAnExplicitHelpRequest(t *testing.T) {
	_, out, errb := runCLI(t, "sync", "--badflag")
	got := out + errb
	if !strings.Contains(got, "not defined") {
		t.Fatalf("expected a flag error:\n%s", got)
	}
	if strings.Contains(got, "Operations (--op") || strings.Contains(got, "Policy file (--policy)") {
		t.Errorf("a typo'd flag printed the reference blocks (%d lines)", strings.Count(got, "\n"))
	}
	// The purpose line is cheap and worth keeping on the error path.
	if !strings.Contains(got, "sync — converge") {
		t.Errorf("flag error dropped the purpose line:\n%s", got)
	}
}

// `help sync` silently printed top-level usage — the wrong answer rather than
// an error.
func TestHelp_HelpVerbRoutesToTheVerb(t *testing.T) {
	code, out, errb := runCLI(t, "help", "sync")
	if code != 0 {
		t.Fatalf("help sync = %d, want 0: %s", code, errb)
	}
	// The synopsis lists sync's flags too, so assert on something only the
	// per-verb dump carries — and that the top-level banner is absent.
	got := out + errb
	if !strings.Contains(got, "R-25 ceiling") {
		t.Errorf("`help sync` did not show sync's own flag descriptions:\n%s", got)
	}
	if strings.Contains(got, "audit REPORTS") {
		t.Errorf("`help sync` printed the top-level usage instead of sync's:\n%s", got)
	}
}
