package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// `audit --lint` is the first verb in this tool that EDITS a file on the basis
// of what a remote API said. Everything else either reports (audit) or acts on
// an operator's explicit intent (plan/apply/sync, where the ops are typed out by
// a human). Lint's instructions come from GitHub: "this team 404s, so drop it".
//
// That inverts the risk. A `sync` that misreads its input writes a rule nobody
// asked for and a human sees it in the diff; a `lint` that misreads the network
// DELETES owners across every rule in the file, and the diff looks like exactly
// the tidy-up it claims to be. The three ways that goes wrong are the three
// things this file spends most of its lines on:
//
//   - An expired or under-scoped token turns every lookup into a 404-shaped
//     answer, and a lint pass that treats those as negatives strips the whole
//     file's owners in one commit. R-12 says the run fails closed AS A WHOLE —
//     one undecidable lookup and nothing is written, not even the parts that
//     were decided. TestLint_InconclusiveWritesNothing and its mixed case pin
//     that: a definitively dead owner sitting next to an undecidable one must
//     still leave the file byte-identical.
//   - Stage 1 must run before stage 2. `/x/ @ org/live` is ONE owner with a
//     space in it, not two owners that happen not to exist. An implementation
//     that checks existence first looks up `@` and `org/live`, finds neither,
//     and deletes a live team while reporting a successful repair.
//     TestLint_RepairRunsBeforeExistenceChecks is the guard.
//   - Deleting a rule whose pattern matches nothing is not obviously safe: a
//     forward-looking rule for a directory that lands next week is indis-
//     tinguishable from a dead one. R-11 keeps it opt-in, so
//     TestLint_StalePathsAreOptIn asserts the DEFAULT leaves it alone.
//
// SPEC R-17: `--lint` reuses the exit-code contract — 0 clean or written,
// 4 pending fixes under --dry-run (the CI gate), 5 inconclusive, 3 invalid
// input, 2 refused, 6 rolled back after a failed post-write validation.
//
// SPEC R-12: no token or no --github-repo is exit 5, never a partial offline
// lint. Owner existence is not decidable offline, and a lint that silently
// skipped stage 2 would report "clean" for a file full of dead teams.
//
// SPEC S-7: `--lint` reads the WORKING-TREE bytes (they are what gets written)
// while resolving ownership against `--branch`'s tracked tree. Plain `audit`
// still reads the file committed at `--branch`. The two verbs deliberately
// disagree about which bytes they are looking at, so
// TestLint_ReadsWorkingTreeWhileAuditReadsTheCommit pins the difference rather
// than leaving it to be discovered by whoever's uncommitted edit vanished.

// ---------- stub GitHub ----------

// lintStubAPI serves handler on a local httptest server and returns its base
// URL, suitable for --api-url. No test in this file touches the network.
func lintStubAPI(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

// lintOverride handles a request specially and reports whether it did. It is
// how a test injects one rate-limited or 5xx endpoint into an otherwise
// healthy API — the mixed case R-12 is really about.
type lintOverride func(w http.ResponseWriter, r *http.Request) bool

// lintAPI is the convenience stub: a healthy GitHub where exactly the owners in
// `exists` exist.
//
// It serves the five endpoints ghapi reaches for owner existence:
//
//	GET /user                            ProbeAPI  — 200
//	GET /orgs/{org}/teams?per_page=1     ProbeOrg  — 200
//	GET /orgs/{org}/members?per_page=1   ProbeOrg  — 200
//	GET /orgs/{org}/teams/{slug}         TeamExists
//	GET /users/{login}                   UserExists
//
// The two probes matter as much as the lookups: ghapi refuses to read a team
// 404 as "gone" until ProbeOrg has succeeded, precisely so that an org this
// token cannot enumerate cannot masquerade as an org full of deleted teams.
// A stub that answered the team lookup but not the probe would make every
// removal test inconclusive instead of proving anything.
//
// Entries in `exists` are "org/slug" for a team and "login" for a user; a
// leading "@" is accepted and ignored.
func lintAPI(t *testing.T, override lintOverride, exists ...string) string {
	t.Helper()
	set := make(map[string]bool, len(exists))
	for _, e := range exists {
		set[strings.TrimPrefix(e, "@")] = true
	}
	return lintStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if override != nil && override(w, r) {
			return
		}
		seg := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case len(seg) == 1 && seg[0] == "user":
			lintReply(w, http.StatusOK, `{"login":"ci-bot"}`)
		case len(seg) == 3 && seg[0] == "orgs" && (seg[2] == "teams" || seg[2] == "members"):
			lintReply(w, http.StatusOK, `[]`)
		// ProbeRepo: --lint establishes that the token can see the repository
		// whose CODEOWNERS it is about to rewrite before it removes anything.
		// A stub that omitted this would make every lint run inconclusive.
		case len(seg) == 3 && seg[0] == "repos":
			lintReply(w, http.StatusOK, `{"full_name":"`+seg[1]+`/`+seg[2]+`"}`)
		// ViewerIsOrgAdmin, consulted only when a team 404s: only an org owner
		// can tell a team that was deleted from a secret team they cannot see,
		// and lint DELETES on that answer. The default stub says "owner" so the
		// cases in this file stay about what they are about; the override in
		// TestLint_TeamNotFoundIsInconclusiveForANonOwnerToken is where the
		// other answer is exercised.
		case len(seg) == 4 && seg[0] == "user" && seg[1] == "memberships" && seg[2] == "orgs":
			lintReply(w, http.StatusOK, `{"state":"active","role":"admin"}`)
		case len(seg) == 4 && seg[0] == "orgs" && seg[2] == "teams":
			if set[seg[1]+"/"+seg[3]] {
				lintReply(w, http.StatusOK, `{"slug":"`+seg[3]+`"}`)
				return
			}
			lintReply(w, http.StatusNotFound, `{"message":"Not Found"}`)
		case len(seg) == 2 && seg[0] == "users":
			if set[seg[1]] {
				lintReply(w, http.StatusOK, `{"login":"`+seg[1]+`"}`)
				return
			}
			lintReply(w, http.StatusNotFound, `{"message":"Not Found"}`)
		default:
			lintReply(w, http.StatusNotFound, `{"message":"Not Found"}`)
		}
	})
}

func lintReply(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// lintRateLimited is the shape ghapi classifies as inconclusive rather than as
// a negative: 403 WITH X-RateLimit-Remaining: 0. A plain 403 lands in the same
// class for a different documented reason (missing scope), so both spellings
// appear below.
func lintRateLimited(w http.ResponseWriter) {
	w.Header().Set("X-RateLimit-Remaining", "0")
	lintReply(w, http.StatusForbidden, `{"message":"API rate limit exceeded"}`)
}

// lintTeamPath is the endpoint TeamExists hits for @org/slug.
func lintTeamPath(org, slug string) string { return "/orgs/" + org + "/teams/" + slug }

// ---------- repo and invocation ----------

const lintOwnersRel = ".github/CODEOWNERS"

func lintOwnersPath(repo string) string {
	return filepath.Join(repo, filepath.FromSlash(lintOwnersRel))
}

func lintRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// lintWrite puts bytes in the WORKING TREE without committing them — the state
// a developer is actually in when they run the linter.
func lintWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// lintUnchanged is the assertion most of this file is built out of: a run that
// did not earn the right to write must leave the file byte-identical. Not
// "equivalent", not "reparsed the same" — identical, because the only evidence
// an operator has that a failed lint did no harm is `git diff` being empty.
func lintUnchanged(t *testing.T, what, path, before string) {
	t.Helper()
	got := lintRead(t, path)
	if got != before {
		t.Errorf("%s: CODEOWNERS was modified by a run that must write nothing\nbefore: %q\nafter:  %q", what, before, got)
	}
}

// lintRun invokes `audit --lint` with credentials that satisfy R-12's
// precondition, plus whatever extra flags the case needs.
func lintRun(t *testing.T, repo, api string, extra ...string) (int, string, string) {
	t.Helper()
	args := []string{
		"audit", "--repo", repo, "--lint",
		"--github-repo", "org/repo",
		"--token", "lint-test-token",
		"--api-url", api,
	}
	args = append(args, extra...)
	return runCLI(t, args...)
}

// lintMentions fails when none of the alternatives appears in text. Output
// wording is not a contract; the fact that the operator is TOLD something is.
func lintMentions(t *testing.T, what, text string, alternatives ...string) {
	t.Helper()
	low := strings.ToLower(text)
	for _, alt := range alternatives {
		if strings.Contains(low, strings.ToLower(alt)) {
			return
		}
	}
	t.Errorf("%s: output mentions none of %v — the operator cannot tell what happened:\n%s", what, alternatives, text)
}

// ---------- JSON helpers ----------

// lintDecode reads the single JSON object `--format json` promises on stdout.
// Extra keys are tolerated by construction; a second object, or prose mixed in,
// is not — a record a script cannot `jq` is not a record.
func lintDecode(t *testing.T, stdout string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("--format json must emit one JSON object on stdout, got %v\nstdout: %s", err, stdout)
	}
	return doc
}

func lintString(t *testing.T, doc map[string]any, key string) string {
	t.Helper()
	v, ok := doc[key]
	if !ok {
		t.Fatalf("JSON record has no %q field: %v", key, lintKeys(doc))
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("JSON %q is %T, want string", key, v)
	}
	return s
}

func lintBool(t *testing.T, doc map[string]any, key string) bool {
	t.Helper()
	v, ok := doc[key]
	if !ok {
		t.Fatalf("JSON record has no %q field: %v", key, lintKeys(doc))
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("JSON %q is %T, want bool", key, v)
	}
	return b
}

func lintKeys(doc map[string]any) []string {
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	return keys
}

// lintActionKinds checks every action carries the three fields the schema
// promises and returns the kinds in file order.
func lintActionKinds(t *testing.T, doc map[string]any) []string {
	t.Helper()
	raw, ok := doc["actions"].([]any)
	if !ok {
		t.Fatalf("JSON %q is %T, want an array: %v", "actions", doc["actions"], lintKeys(doc))
	}
	kinds := make([]string, 0, len(raw))
	for i, a := range raw {
		m, ok := a.(map[string]any)
		if !ok {
			t.Fatalf("actions[%d] is %T, want an object", i, a)
		}
		for _, field := range []string{"kind", "line", "reason"} {
			if _, ok := m[field]; !ok {
				t.Errorf("actions[%d] has no %q: an action nobody can attribute to a line or a reason is not reviewable\n%v", i, field, m)
			}
		}
		k, _ := m["kind"].(string)
		kinds = append(kinds, k)
	}
	return kinds
}

func lintHasKind(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// ---------- stage 1: repair ----------

// SPEC stage 1 (repair-owner-spacing): `/x/ @ org/live` is a line GitHub throws
// away wholesale, so the team owns nothing and is never asked to review — and
// nothing in the UI says so. Lint rejoins the handle and writes the file.
//
// Getting this wrong is invisible by construction: the broken line looks
// plausible in review, and the only symptom is review requests that never
// arrive. That is why a repair is worth writing at all rather than merely
// reporting.
func TestLint_RepairsSplitOwnerHandle(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "# owners\n/x/ @ org/live\n/y/ @org/live\n",
		"x/a.go":      "package x\n",
		"y/b.go":      "package y\n",
	})
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api)
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0 — the repair was computed and written\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if got := lintRead(t, lintOwnersPath(repo)); got != "# owners\n/x/ @org/live\n/y/ @org/live\n" {
		t.Errorf("CODEOWNERS = %q, want the handle rejoined and everything else untouched", got)
	}
	lintMentions(t, "repair run", out, "repair-owner-spacing")
}

// SPEC stage 1 before stage 2 — the ordering the package doc calls out and the
// single most damaging way to implement this feature wrong.
//
// `@ org/live` is ONE owner with whitespace in it. An implementation that runs
// existence checks over the raw tokens asks GitHub about `@` and about
// `org/live`, gets nothing back for either, and removes them — deleting a LIVE
// team from a rule while reporting a clean, successful lint. The file that
// results parses, resolves, and is wrong: the directory is now owned by
// whatever broader rule precedes it, silently.
//
// So this asserts the positive outcome (the owner is still there) AND the
// absence of the removal, because a run that deleted the owner and re-added it
// would pass the first check alone.
func TestLint_RepairRunsBeforeExistenceChecks(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @ org/live\n",
		"x/a.go":      "package x\n",
	})
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	got := lintRead(t, lintOwnersPath(repo))
	if !strings.Contains(got, "@org/live") {
		t.Fatalf("CODEOWNERS = %q: the team exists and must SURVIVE the repair — looking up `@` and `org/live` separately deletes a live owner", got)
	}
	if got != "/x/ @org/live\n" {
		t.Errorf("CODEOWNERS = %q, want %q", got, "/x/ @org/live\n")
	}
	kinds := lintActionKinds(t, lintDecode(t, out))
	if !lintHasKind(kinds, "repair-owner-spacing") {
		t.Errorf("actions = %v, want a repair-owner-spacing entry", kinds)
	}
	if lintHasKind(kinds, "remove-dead-owner") {
		t.Errorf("actions = %v: a removal was recorded for an owner that exists — stage 2 ran against the UNREPAIRED tokens", kinds)
	}
}

// ---------- stage 2: dead owners ----------

// SPEC stage 2 (remove-dead-owner, A-1): a deleted or renamed team on a rule is
// a review request routed to nobody. GitHub does not warn; the PR simply sits
// with one fewer reviewer than the file claims.
//
// The surviving owner is asserted explicitly: "removed the dead one" and
// "removed both" produce the same exit code and very similar output.
func TestLint_RemovesDeadOwner(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @org/live @org/gone\n",
		"x/a.go":      "package x\n",
	})
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api)
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if got := lintRead(t, lintOwnersPath(repo)); got != "/x/ @org/live\n" {
		t.Errorf("CODEOWNERS = %q, want %q — @org/gone dropped, @org/live kept", got, "/x/ @org/live\n")
	}
	lintMentions(t, "dead-owner run", out, "remove-dead-owner")
}

// SPEC R-12, the whole point of this feature having a fail-closed contract at
// all: an owner lookup that could not be DECIDED must never be read as "does
// not exist".
//
// A 403 with X-RateLimit-Remaining: 0 is the everyday version — a scheduled
// fleet job burns the hourly budget partway through and every remaining lookup
// comes back looking like a negative. If lint acted on those it would open a PR
// stripping owners from every rule it had not yet reached, with a diff that
// looks exactly like the tidy-up it was asked for.
//
// The mixed case below is the one that separates "fails closed" from "fails
// closed for the whole run": one owner is DEFINITIVELY dead and one lookup is
// undecidable. The tempting behaviour — apply what was decided, report the rest
// — is wrong, because the operator's `git diff` then shows a real edit produced
// by a run that could not see the whole file, and there is no second run that
// tells them which parts were skipped.
func TestLint_InconclusiveWritesNothing(t *testing.T) {
	t.Run("rate limited", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			lintOwnersRel: "/x/ @org/live @org/gone\n",
			"x/a.go":      "package x\n",
		})
		before := lintRead(t, lintOwnersPath(repo))
		api := lintAPI(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == lintTeamPath("org", "gone") {
				lintRateLimited(w)
				return true
			}
			return false
		}, "org/live")

		code, out, errOut := lintRun(t, repo, api)
		if code != cli.ExitInconclusive {
			t.Errorf("exit %d, want 5 — a rate-limited lookup is not a negative\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		lintUnchanged(t, "rate-limited lint", lintOwnersPath(repo), before)
		lintMentions(t, "rate-limited lint", out+errOut, "inconclusive", "rate limit")
	})

	t.Run("server error", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			lintOwnersRel: "/x/ @org/live @org/gone\n",
			"x/a.go":      "package x\n",
		})
		before := lintRead(t, lintOwnersPath(repo))
		api := lintAPI(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == lintTeamPath("org", "gone") {
				lintReply(w, http.StatusInternalServerError, `{"message":"boom"}`)
				return true
			}
			return false
		}, "org/live")

		code, out, errOut := lintRun(t, repo, api)
		if code != cli.ExitInconclusive {
			t.Errorf("exit %d, want 5 — a 5xx is not a negative\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		lintUnchanged(t, "5xx lint", lintOwnersPath(repo), before)
		lintMentions(t, "5xx lint", out+errOut, "inconclusive", "500")
	})

	t.Run("one dead owner and one undecidable lookup", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			lintOwnersRel: "/x/ @org/live @org/gone\n/y/ @org/live @org/flaky\n",
			"x/a.go":      "package x\n",
			"y/b.go":      "package y\n",
		})
		before := lintRead(t, lintOwnersPath(repo))
		// @org/gone is a real, provable 404. @org/flaky cannot be decided.
		api := lintAPI(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == lintTeamPath("org", "flaky") {
				lintRateLimited(w)
				return true
			}
			return false
		}, "org/live")

		code, out, errOut := lintRun(t, repo, api)
		if code != cli.ExitInconclusive {
			t.Errorf("exit %d, want 5\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		lintUnchanged(t, "mixed decided/undecided lint", lintOwnersPath(repo), before)
		if got := lintRead(t, lintOwnersPath(repo)); !strings.Contains(got, "@org/gone") {
			t.Errorf("CODEOWNERS = %q: the provably dead owner was removed anyway — R-12 fails the WHOLE run closed, not the undecidable part of it", got)
		}
	})
}

// SPEC R-12: owner existence is not decidable offline, so `--lint` without
// credentials is inconclusive (exit 5) and writes nothing.
//
// The alternative — quietly running stage 1 and skipping stage 2 — would report
// "lint clean" for a file full of deleted teams, which is worse than not running
// at all: it is a green check that means nothing. Exit 5 keeps a credential-less
// CI job red until someone gives it a token.
func TestLint_RequiresTokenAndGitHubRepo(t *testing.T) {
	// A stale $GITHUB_TOKEN in the ambient environment would silently satisfy
	// half of what this test is asserting is missing.
	t.Setenv("GITHUB_TOKEN", "")
	api := lintAPI(t, nil, "org/live")

	cases := []struct {
		name  string
		extra []string
	}{
		{"no token", []string{"--github-repo", "org/repo", "--api-url", api}},
		{"no --github-repo", []string{"--token", "lint-test-token", "--api-url", api}},
		{"neither", []string{"--api-url", api}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t, map[string]string{
				lintOwnersRel: "/x/ @ org/live @org/gone\n",
				"x/a.go":      "package x\n",
			})
			before := lintRead(t, lintOwnersPath(repo))
			args := append([]string{"audit", "--repo", repo, "--lint"}, tc.extra...)
			code, out, errOut := runCLI(t, args...)
			if code != cli.ExitInconclusive {
				t.Errorf("exit %d, want 5 — owner existence cannot be decided offline\nstdout: %s\nstderr: %s", code, out, errOut)
			}
			lintUnchanged(t, tc.name, lintOwnersPath(repo), before)
		})
	}
}

// ---------- --dry-run ----------

// SPEC exit 4 under --dry-run: this is the CI gate. A repository whose
// CODEOWNERS needs repair must FAIL the check, and a clean one must pass, so
// that `codeowners-tool audit --lint --dry-run` can sit in a required workflow
// exactly the way `gofmt -l` does.
//
// Exit 0 with the fixes printed would be the natural-looking mistake and would
// make the gate decorative: every repository would pass forever while the
// output nobody reads listed the problems.
//
// The byte-identity assertion is the other half. A preview that writes is not a
// preview, and the CI runner's checkout is a place where a stray write is
// especially easy not to notice.
func TestLint_DryRunReportsPendingFixesAndWritesNothing(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "# owners\n/x/ @ org/live\n/y/ @org/live @org/gone\n",
		"x/a.go":      "package x\n",
		"y/b.go":      "package y\n",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api, "--dry-run")
	if code != cli.ExitFindings {
		t.Fatalf("exit %d, want 4 — pending fixes under --dry-run are the CI gate\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "--dry-run", path, before)
	lintMentions(t, "--dry-run", out, "dry-run")
	lintMentions(t, "--dry-run", out, "repair-owner-spacing")
	lintMentions(t, "--dry-run", out, "remove-dead-owner")
}

// A clean file exits 0 under --dry-run as well, or the gate above is a gate
// that never opens.
func TestLint_DryRunOnACleanFileIsSuccess(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @org/live\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api, "--dry-run")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0 — nothing is pending\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "--dry-run on a clean file", path, before)
	lintMentions(t, "--dry-run on a clean file", out, "nothing to repair")
}

// SPEC exit 0: a file with nothing to fix is a SUCCESS, not a
// no-op (exit 1). Under the fleet contract exit 1 reads as "nothing to do here"
// and, in a `set -e` script, as failure — so a repository that is already
// correct would abort the run that was only checking on it.
func TestLint_CleanFileIsUntouched(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "# owners\n\n/x/ @org/live\n* @alice\n",
		"x/a.go":      "package x\n",
		"z.txt":       "z\n",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)
	api := lintAPI(t, nil, "org/live", "alice")

	code, out, errOut := lintRun(t, repo, api)
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintMentions(t, "clean lint", out, "nothing to repair")
	lintUnchanged(t, "clean lint", path, before)
}

// ---------- argument-class rejections ----------

// SPEC exit 3: `--checks` selects a SUBSET of the audit's checks, and lint
// covers the whole file by construction — there is no subset of a repair pass.
//
// Accepting the combination and ignoring the flag is the dangerous version: an
// operator who wrote `--checks a1 --lint` believing they had scoped the run to
// one check would get a whole-file rewrite. Refusing at the argument, before the
// repository is opened, keeps that from ever reaching a write.
func TestLint_ChecksIsRejected(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @org/live @org/gone\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api, "--checks", "a1")
	if code != cli.ExitInvalid {
		t.Errorf("exit %d, want 3 — there is no subset of a whole-file repair\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "--checks with --lint", path, before)
	if strings.TrimSpace(errOut) == "" {
		t.Error("an argument-class rejection must say which argument it rejected")
	}
}

// SPEC exit 3: the lint-only flags are meaningless without --lint, and
// accepting them silently is how an operator ends up believing a plain `audit`
// deleted their stale rules (it cannot write at all) or previewed something
// (`--dry-run` on a verb that never writes describes nothing).
//
// Each is asserted separately so a regression names the flag that regressed.
func TestLint_LintOnlyFlagsRequireLint(t *testing.T) {
	api := lintAPI(t, nil, "org/live")
	for _, extra := range [][]string{
		{"--remove-stale-paths"},
		{"--on-empty", "unowned"},
		{"--on-empty", "error"},
		{"--dry-run"},
	} {
		name := strings.Join(extra, " ")
		t.Run(name, func(t *testing.T) {
			repo := initRepo(t, map[string]string{
				lintOwnersRel: "/x/ @org/live\n",
				"x/a.go":      "package x\n",
			})
			path := lintOwnersPath(repo)
			before := lintRead(t, path)
			args := append([]string{"audit", "--repo", repo,
				"--github-repo", "org/repo", "--token", "lint-test-token", "--api-url", api}, extra...)
			code, out, errOut := runCLI(t, args...)
			if code != cli.ExitInvalid {
				t.Errorf("`audit %s` without --lint: exit %d, want 3\nstdout: %s\nstderr: %s", name, code, out, errOut)
			}
			lintUnchanged(t, name, path, before)
		})
	}
}

// SPEC exit 3: a malformed --github-repo is decidable from the argument alone.
// Under --lint it is worse than usual to get this wrong, because the fallback
// for "no usable repo" is exit 5 — and a run that exits 5 on a TYPO looks like
// a rate limit, which operators retry rather than fix.
func TestLint_MalformedGitHubRepoIsInvalidInput(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @org/live @org/gone\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := runCLI(t, "audit", "--repo", repo, "--lint",
		"--github-repo", "org", "--token", "lint-test-token", "--api-url", api)
	if code != cli.ExitInvalid {
		t.Errorf("exit %d, want 3 for --github-repo without a slash\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintUnchanged(t, "malformed --github-repo", path, before)
}

// ---------- stage 3: stale paths ----------

// SPEC R-11: a rule whose pattern matches zero tracked files is REPORTED by
// default and deleted only when a human asks with --remove-stale-paths.
//
// The asymmetry is deliberate. A pattern matching nothing today is often a
// pattern for a directory that lands next week, or for one that exists on
// another branch; deleting it destroys an intent nothing else in the repository
// records. The other two stages fix things that are unambiguously broken, so
// they are on by default and this one is not — and the default must be pinned
// here, because a lint pass that quietly pruned "dead" rules across a fleet
// would be discovered only when the directory finally arrives unowned.
func TestLint_StalePathsAreOptIn(t *testing.T) {
	content := "/x/ @org/live\n/gone/ @org/live\n"
	api := lintAPI(t, nil, "org/live")

	t.Run("default keeps the rule", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			lintOwnersRel: content,
			"x/a.go":      "package x\n",
		})
		path := lintOwnersPath(repo)
		code, out, errOut := lintRun(t, repo, api)
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		lintUnchanged(t, "no --remove-stale-paths", path, content)
		if strings.Contains(out, "remove-stale-rule") {
			t.Errorf("a stale rule was deleted without --remove-stale-paths:\n%s", out)
		}
	})

	t.Run("opted in deletes exactly that line", func(t *testing.T) {
		repo := initRepo(t, map[string]string{
			lintOwnersRel: content,
			"x/a.go":      "package x\n",
		})
		path := lintOwnersPath(repo)
		code, out, errOut := lintRun(t, repo, api, "--remove-stale-paths")
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		if got := lintRead(t, path); got != "/x/ @org/live\n" {
			t.Errorf("CODEOWNERS = %q, want only the stale rule gone", got)
		}
		lintMentions(t, "--remove-stale-paths", out, "remove-stale-rule")
	})
}

// ---------- R-6: emptying an owner set ----------

// SPEC R-6: removing a dead owner that happens to be a rule's ONLY owner is a
// different decision from removing one of several, and it has no safe default.
//
// The three outcomes are genuinely different repositories. `unowned` keeps the
// pattern with no owners, which is a real and deliberate statement ("this path
// is explicitly nobody's"). `inherit` deletes the rule so the preceding broader
// rule takes over, silently transferring the path to another team. `error`
// refuses. Guessing between them is guessing who reviews a directory, so the
// absence of the flag is invalid input (exit 3) rather than any of them.
//
// Note the exit codes are deliberately distinct: 3 for "you did not tell me"
// and 2 for "you told me to refuse". A script can retry the first with an added
// flag; the second is a decision that has already been made.
func TestLint_SoleOwnerRemovalNeedsAnExplicitOnEmptyPolicy(t *testing.T) {
	content := "* @org/live\n/x/ @org/gone\n"
	api := lintAPI(t, nil, "org/live")
	files := map[string]string{
		lintOwnersRel: content,
		"x/a.go":      "package x\n",
		"z.txt":       "z\n",
	}

	t.Run("no policy is invalid input", func(t *testing.T) {
		repo := initRepo(t, files)
		path := lintOwnersPath(repo)
		code, out, errOut := lintRun(t, repo, api)
		if code != cli.ExitInvalid {
			t.Errorf("exit %d, want 3 — R-6 has deliberately no default\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		lintUnchanged(t, "no --on-empty", path, content)
	})

	t.Run("unowned keeps the pattern with no owners", func(t *testing.T) {
		repo := initRepo(t, files)
		path := lintOwnersPath(repo)
		code, out, errOut := lintRun(t, repo, api, "--on-empty", "unowned")
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		got := lintRead(t, path)
		if strings.Contains(got, "@org/gone") {
			t.Errorf("CODEOWNERS = %q: the dead owner is still there", got)
		}
		if !strings.Contains(got, "/x/") {
			t.Errorf("CODEOWNERS = %q: --on-empty=unowned keeps the PATTERN — deleting it would hand /x/ to the `*` rule instead of un-owning it", got)
		}
	})

	t.Run("error refuses and writes nothing", func(t *testing.T) {
		repo := initRepo(t, files)
		path := lintOwnersPath(repo)
		code, out, errOut := lintRun(t, repo, api, "--on-empty", "error")
		if code != cli.ExitRefused {
			t.Errorf("exit %d, want 2 — a refusal the operator asked for is not invalid input\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		lintUnchanged(t, "--on-empty=error", path, content)
	})
}

// ---------- JSON record ----------

// SPEC --format json: one object on stdout carrying enough to review the run
// without re-running it — which file was touched, whether anything was written,
// whether this was a preview, what was done and why, and the plan artifacts
// (changes / ownership_rows / diff) that make the edit auditable.
//
// `dry_run` is omitted when false rather than emitted as false for the same
// reason SyncRecord.DryRun is: a preview record and a real record must not be
// byte-identical, and the field only ever needs to be present on the one that
// requires the warning. `applied` carries the other half — a script gating on a
// lint run needs a single boolean for "did this repository change".
func TestLint_JSONRecordShape(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "# owners\n/x/ @ org/live\n/y/ @org/live @org/gone\n",
		"x/a.go":      "package x\n",
		"y/b.go":      "package y\n",
	})
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	doc := lintDecode(t, out)

	if got := lintString(t, doc, "codeowners_path"); got != lintOwnersRel {
		t.Errorf("codeowners_path = %q, want %q — the record must name the file it edited", got, lintOwnersRel)
	}
	if !lintBool(t, doc, "applied") {
		t.Error("applied = false on a run that rewrote the file")
	}
	if v, ok := doc["dry_run"]; ok && v != false {
		t.Errorf("dry_run = %v on a real run: a preview record and a rollout record must not be confusable", v)
	}
	for _, key := range []string{"changes", "ownership_rows", "diff"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("JSON record has no %q: without it the edit cannot be reviewed from the record alone (keys: %v)", key, lintKeys(doc))
		}
	}
	kinds := lintActionKinds(t, doc)
	for _, want := range []string{"repair-owner-spacing", "remove-dead-owner"} {
		if !lintHasKind(kinds, want) {
			t.Errorf("actions = %v, want a %q entry", kinds, want)
		}
	}
	if v, ok := doc["unverifiable"]; ok {
		if arr, isArr := v.([]any); !isArr {
			t.Errorf("unverifiable is %T, want an array", v)
		} else if len(arr) == 0 {
			t.Error("unverifiable is present but empty: it is omitempty, so an empty array is noise in every clean record")
		}
	}
}

// The preview's record must say so, for the same reason sync's does: a file of
// records collected by one `>>` cannot be told apart afterwards otherwise.
func TestLint_JSONDryRunRecordSaysSo(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @org/live @org/gone\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)
	before := lintRead(t, path)
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api, "--dry-run", "--format", "json")
	if code != cli.ExitFindings {
		t.Fatalf("exit %d, want 4\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	doc := lintDecode(t, out)
	if !lintBool(t, doc, "dry_run") {
		t.Error("dry_run is absent or false on a --dry-run record")
	}
	if lintBool(t, doc, "applied") {
		t.Error("applied = true on a run that wrote nothing")
	}
	lintUnchanged(t, "--dry-run --format json", path, before)
}

// SPEC R-13: an email owner cannot be looked up, so it is never dead. It stays
// exactly where it is and is reported in `unverifiable`.
//
// Without the carve-out the only consistent alternatives are both bad: treat an
// unlookupable owner as dead (and strip every mailing-list owner in the file) or
// treat it as inconclusive (and make R-12 abort every run on a file that
// contains one).
func TestLint_EmailOwnersAreLeftAlone(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ docs@example.com @org/gone\n",
		"x/a.go":      "package x\n",
	})
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0 — an email owner is unverifiable, not undecidable\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	got := lintRead(t, lintOwnersPath(repo))
	if !strings.Contains(got, "docs@example.com") {
		t.Errorf("CODEOWNERS = %q: the email owner was removed", got)
	}
	if strings.Contains(got, "@org/gone") {
		t.Errorf("CODEOWNERS = %q: the dead team survived", got)
	}
	doc := lintDecode(t, out)
	arr, ok := doc["unverifiable"].([]any)
	if !ok || len(arr) == 0 {
		t.Errorf("unverifiable = %v, want the email owner reported (R-13)", doc["unverifiable"])
	}
}

// ---------- working tree vs committed ----------

// SPEC S-7 and the write target: `--lint` reads the WORKING-TREE bytes because
// those are the bytes it will overwrite, while resolving ownership against
// `--branch`'s tracked tree. Plain `audit` reads the file COMMITTED at
// `--branch`.
//
// The difference is not an accident to be smoothed over. Linting the committed
// copy and writing the working copy would overwrite uncommitted edits with a
// repair computed from bytes the author had already changed — silent data loss
// in the one directory a developer is most likely to have unsaved work in.
// Auditing the working copy, conversely, would make `audit` report on a state
// no reviewer can see.
//
// This is exercised with a rule that matches nothing (A-4's territory), so the
// two verbs' disagreement shows up in the EXIT CODE and does not depend on how
// either one happens to parse a broken owner token.
func TestLint_ReadsWorkingTreeWhileAuditReadsTheCommit(t *testing.T) {
	repo := initRepo(t, map[string]string{
		lintOwnersRel: "/x/ @org/live\n",
		"x/a.go":      "package x\n",
	})
	path := lintOwnersPath(repo)
	// Uncommitted: a split handle (lint's stage 1) and a rule matching nothing
	// (audit's A-4). Neither is in the commit.
	working := "/x/ @ org/live\n/ghost/ @org/live\n"
	lintWrite(t, path, working)
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api, "--dry-run")
	if code != cli.ExitFindings {
		t.Errorf("audit --lint --dry-run: exit %d, want 4 — the working-tree file is broken and is what lint would write\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	lintMentions(t, "lint on the working tree", out, "repair-owner-spacing")
	lintUnchanged(t, "lint --dry-run", path, working)

	// Plain audit looks at the commit, where /ghost/ does not exist, so A-4 has
	// nothing to report.
	code2, out2, errOut2 := runCLI(t, "audit", "--repo", repo, "--checks", "a4")
	if code2 != cli.ExitOK {
		t.Errorf("plain audit --checks a4: exit %d, want 0 — plain audit reads the COMMITTED file, which has no stale rule\nstdout: %s\nstderr: %s", code2, out2, errOut2)
	}
	if strings.Contains(out2, "/ghost/") {
		t.Errorf("plain audit reported an uncommitted rule:\n%s", out2)
	}
	lintUnchanged(t, "plain audit", path, working)
}

// ---------- byte preservation ----------

// SPEC: lint edits the lines it names and nothing else.
//
// A repair pass that reformats is a repair pass nobody will run twice. The
// review cost of a diff is proportional to its size, so a linter that also
// normalizes blank lines, re-wraps comments or re-indents unrelated rules turns
// a two-line safety fix into a file-wide diff that gets rubber-stamped — and
// the one line that mattered is invisible inside it.
//
// The comment lines, the blank lines, the odd spacing on the untouched rule and
// the trailing newline are all asserted by comparing the whole file.
func TestLint_PreservesEverythingItDidNotName(t *testing.T) {
	const before = "# CODEOWNERS — reviewed by the platform team\n" +
		"# second header line\n" +
		"\n" +
		"/x/ @org/live @org/gone\n" +
		"\n" +
		"# unrelated, and deliberately indented oddly\n" +
		"/y/    @org/live\n"
	const want = "# CODEOWNERS — reviewed by the platform team\n" +
		"# second header line\n" +
		"\n" +
		"/x/ @org/live\n" +
		"\n" +
		"# unrelated, and deliberately indented oddly\n" +
		"/y/    @org/live\n"

	repo := initRepo(t, map[string]string{
		lintOwnersRel: before,
		"x/a.go":      "package x\n",
		"y/b.go":      "package y\n",
	})
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api)
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if got := lintRead(t, lintOwnersPath(repo)); got != want {
		t.Errorf("CODEOWNERS =\n%q\nwant\n%q", got, want)
	}
}

// SPEC: a CRLF file stays CRLF.
//
// Re-normalizing line endings rewrites every line in the file. On a repository
// with `* text=auto` or a Windows contributor that is a whole-file diff attached
// to a one-owner fix, and on the next checkout it comes back — so the linter
// produces a diff on every run forever, which is exactly how a required check
// gets disabled.
func TestLint_PreservesCRLF(t *testing.T) {
	const before = "# crlf\r\n/x/ @org/live @org/gone\r\n"
	const want = "# crlf\r\n/x/ @org/live\r\n"

	repo := initRepo(t, map[string]string{
		lintOwnersRel: before,
		"x/a.go":      "package x\n",
	})
	api := lintAPI(t, nil, "org/live")

	code, out, errOut := lintRun(t, repo, api)
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	got := lintRead(t, lintOwnersPath(repo))
	if got != want {
		t.Errorf("CODEOWNERS = %q, want %q", got, want)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Errorf("CODEOWNERS = %q: a bare LF appeared in a CRLF file", got)
	}
}

// ---------- discoverability ----------

// SPEC: `audit --help` documents `--lint`, and exits 0 doing it (flagParseCode:
// --help is a request, not a broken invocation).
//
// An undocumented repair flag is one nobody uses, and a lint pass nobody runs is
// a lint pass that never prevents anything.
func TestLint_HelpDocumentsTheFlag(t *testing.T) {
	code, out, errOut := runCLI(t, "audit", "--help")
	if code != cli.ExitOK {
		t.Errorf("audit --help: exit %d, want 0", code)
	}
	lintMentions(t, "audit --help", out+errOut, "--lint")
}
