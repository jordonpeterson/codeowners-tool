package cli_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// BLOCKER 2 — the token must never be rendered.
//
// cmdAudit declares the flag as
//
//	token := fs.String("token", os.Getenv("GITHUB_TOKEN"), "...")
//
// which makes the LIVE CREDENTIAL the flag's default value, and Go's flag package
// prints non-empty string defaults in its usage text. So the secret is written to
// stderr by `audit --help` and by ANY flag error:
//
//	$ GITHUB_TOKEN=ghp_real codeowners-tool audit --bogusflag
//	flag provided but not defined: -bogusflag
//	  -token string
//	        GitHub token (default $GITHUB_TOKEN) (default "ghp_real")
//
// That is CWE-532. One mistyped flag in a scheduled fleet job writes the PAT into
// the build log and from there into whatever aggregates logs. GitHub Actions masks
// secrets it registered; Jenkins, GitLab, CircleCI and Buildkite are much less
// reliable, and a terminal on a screen share is masked by nothing.
//
// The fix is to default the flag to "" and read the environment AFTER Parse, so the
// token is never a value the flag package can render. These tests pin both halves:
// the secret never appears in output, and $GITHUB_TOKEN is still honored.

const tokenSentinel = "ghp_SENTINEL_must_never_be_printed_0123456789"

// assertNoSentinel fails with the offending stream named, so a regression points
// straight at the sink that leaked.
func assertNoSentinel(t *testing.T, what, stdout, stderr string) {
	t.Helper()
	if strings.Contains(stdout, tokenSentinel) {
		t.Errorf("%s: the token appears in STDOUT\n%s", what, stdout)
	}
	if strings.Contains(stderr, tokenSentinel) {
		t.Errorf("%s: the token appears in STDERR — this is what CI captures into the build log\n%s", what, stderr)
	}
}

// `--help` is a request, not a broken invocation (flagParseCode says so), and it is
// the single most likely command to be run by someone reading over a shoulder.
func TestTokenRedaction_HelpDoesNotPrintTheToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", tokenSentinel)
	_, out, errOut := runCLI(t, "audit", "--help")
	assertNoSentinel(t, "audit --help", out, errOut)
}

// The flag-error path is the dangerous one: it is reached by a typo, in CI, where
// nobody is watching the output at the time it is produced.
func TestTokenRedaction_FlagErrorDoesNotPrintTheToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", tokenSentinel)
	code, out, errOut := runCLI(t, "audit", "--bogusflag")
	if code != cli.ExitInvalid {
		t.Errorf("exit %d, want %d for an unknown flag", code, cli.ExitInvalid)
	}
	assertNoSentinel(t, "audit --bogusflag", out, errOut)
}

// Every verb, not just audit: a token in the environment must survive any usage
// render anywhere in the CLI. This is the regression guard for the day a second
// command grows a credential flag.
func TestTokenRedaction_NoVerbPrintsTheToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", tokenSentinel)
	for _, verb := range []string{"sync", "check", "plan", "apply", "audit", "verify", "snapshot"} {
		for _, args := range [][]string{{verb, "--help"}, {verb, "--bogusflag"}} {
			_, out, errOut := runCLI(t, args...)
			assertNoSentinel(t, strings.Join(args, " "), out, errOut)
		}
	}
	_, out, errOut := runCLI(t, "--help")
	assertNoSentinel(t, "--help", out, errOut)
}

// The other half of the contract: redaction must not be achieved by dropping
// support for $GITHUB_TOKEN. A local API stands in for GitHub so this asserts the
// credential actually reaches the wire, with no network access.
func TestTokenRedaction_EnvTokenIsStillSentToTheAPI(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Enough for ProbeOrg/ProbeRepo/ProbeAPI to succeed; the audit's verdict
		// is irrelevant here, only that the request carried the credential.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"permission":"write","permissions":{"push":true},"errors":[]}`))
	}))
	defer srv.Close()

	t.Setenv("GITHUB_TOKEN", tokenSentinel)
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "/svc/ @org/team-a\n",
		"svc/a.go":   "package svc\n",
	})

	_, out, errOut := runCLI(t, "audit", "--repo", repo,
		"--github-repo", "org/repo", "--api-url", srv.URL, "--checks", "a1")

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatalf("the audit made no API request, so $GITHUB_TOKEN was not picked up from the environment\nstdout: %s\nstderr: %s", out, errOut)
	}
	for _, h := range seen {
		if h != "Bearer "+tokenSentinel {
			t.Errorf("Authorization = %q, want %q: the env var must still authenticate", h, "Bearer "+tokenSentinel)
		}
	}
	// And it still must not be echoed back at the operator.
	assertNoSentinel(t, "audit (successful run)", out, errOut)
}
