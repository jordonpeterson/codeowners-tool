// R-41's adversarial half: everything the owner check must refuse to do.
//
// The first file proves R-41 catches the reported bug. This one proves it
// cannot cause a worse one. A check that runs before a hundred writes has two
// failure modes and they point in opposite directions:
//
//   - It reads an unanswerable lookup as "does not exist" and halts a wave
//     whose policy was correct. One expired token, one 5xx, one GHES URL
//     missing /api/v3, and every owner in the file looks deleted — the mass
//     false negative R-12 exists to prevent, arriving now on the verb that
//     WRITES rather than the one that reports.
//   - It reaches the network at all with a credential the operator did not
//     mean to spend, or renders that credential into a CI log (CWE-532).
//
// Every case below therefore asserts what the run did NOT say as well as what
// it did: an inconclusive verdict that contains "does not exist" is the bug,
// not the fix, and an exit code alone cannot tell the two apart.
package cli_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oeGhost is the policy every fail-closed case here runs: one team owner that
// the stub does not know about, so a build that reads an undecidable answer as
// a negative has something to be wrong about.
func oeGhost(t *testing.T) string {
	t.Helper()
	return oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, @org/platform)"]}`)
}

// SPEC R-41/R-12: every shape of "the lookup could not be answered" refuses
// the write, not just the rate limit. A 500, an expired token's 401, a 403
// carrying no rate-limit header and a 429 are four branches of ghapi's
// classifier and one contract: an owner that is neither proven live nor
// proven dead is never written, and is never described as missing. (The
// fifth, a connection that dies mid-request, has its own test below — no
// status code reaches it.) Pinning only the rate-limited 403 — the one branch
// with a message of its own — leaves the others free to return "does not
// exist" and halt a fleet over a policy that was correct.
func TestR41_EveryUndecidableLookupRefusesTheWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func(w http.ResponseWriter)
	}{
		{"500", func(w http.ResponseWriter) { oeReply(w, http.StatusInternalServerError, `{"message":"server error"}`) }},
		{"401 expired token", func(w http.ResponseWriter) { oeReply(w, http.StatusUnauthorized, `{"message":"Bad credentials"}`) }},
		{"403 with no rate-limit header", func(w http.ResponseWriter) { oeReply(w, http.StatusForbidden, `{"message":"Forbidden"}`) }},
		{"429", func(w http.ResponseWriter) { oeReply(w, http.StatusTooManyRequests, `{"message":"slow down"}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := oeRepo(t)
			api, _ := oeAPIWithOverride(t, func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasPrefix(r.URL.Path, "/orgs/org/teams/") {
					tc.reply(w)
					return true
				}
				return false
			}, "org/api-team", "org/everyone")

			code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", oeGhost(t),
				"--verify-owners", "--token", "t", "--api-url", api)

			if code != 3 {
				t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
			}
			// The real assertion: an unanswered question rendered as a
			// negative is the failure, and it exits 3 either way.
			if strings.Contains(errb, "does not exist") {
				t.Errorf("an undecidable lookup was reported as a missing owner:\n%s", errb)
			}
			if !strings.Contains(errb, "R-12") || !strings.Contains(errb, "@org/platform") {
				t.Errorf("stderr does not cite R-12 and name the owner:\n%s", errb)
			}
			if strings.Contains(errb, policyGuidanceFragment) {
				t.Errorf("reported as a policy error; the remedy is to re-run:\n%s", errb)
			}
			if got := oeContent(t, repo); got != oeBefore {
				t.Errorf("CODEOWNERS moved:\n%s", got)
			}
		})
	}
}

// SPEC R-41/R-12: until ProbeOrg succeeds, a team 404 means "invisible to
// these scopes" as readily as "deleted", so a failing probe stops the run
// before TeamExists is ever consulted. Acting on the team lookup first is the
// bug the probe exists to prevent, and under R-41 it would halt a
// hundred-repo wave over a scope the token never had.
func TestR41_ProbeOrgFailureIsNotADeadTeam(t *testing.T) {
	repo := oeRepo(t)
	api, calls := oeAPIWithOverride(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/orgs/org/teams" || r.URL.Path == "/orgs/org/members" {
			oeReply(w, http.StatusForbidden, `{"message":"Forbidden"}`)
			return true
		}
		return false
	})

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", oeGhost(t),
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if strings.Contains(errb, "does not exist") {
		t.Errorf("a failing probe was reported as a missing team:\n%s", errb)
	}
	for _, c := range calls.all() {
		if strings.HasPrefix(c, "/orgs/org/teams/") {
			t.Errorf("the team was looked up before the org probe succeeded: %v", calls.all())
		}
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("CODEOWNERS moved:\n%s", got)
	}
}

// SPEC R-41/R-12: the org-owner question is asked from a point where the team
// already looks gone, so an implementation that swallows its error into "not
// an owner" reports the wrong remedy — "re-run with an org-owner token" for
// what is actually a 5xx. Neither answer may become a write.
func TestR41_ViewerIsOrgAdminFailureIsInconclusive(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPIWithOverride(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/user/memberships/orgs/") {
			oeReply(w, http.StatusInternalServerError, `{"message":"server error"}`)
			return true
		}
		return false
	}, "org/api-team", "org/everyone")

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", oeGhost(t),
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if strings.Contains(errb, "does not exist") {
		t.Errorf("a failed admin lookup decided a team was deleted:\n%s", errb)
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("CODEOWNERS moved:\n%s", got)
	}
}

// SPEC R-41/R-12: a GHES base URL missing /api/v3 returns 404 from EVERY
// endpoint, so every owner in the policy looks deleted at once. Refusing a
// hundred repositories over one typo in a URL is indistinguishable, at the
// HTTP layer, from the wave R-41 was built to stop — so the run proves the
// base URL reaches a GitHub API before it reads any 404 as an answer, and the
// message names the URL rather than the owner.
func TestR41_MistypedApiUrlDoesNotMakeEveryOwnerLookDead(t *testing.T) {
	for _, tc := range []struct{ name, op string }{
		{"team owner", "add_owner(/services/api/, @org/platform)"},
		{"user owner", "add_owner(/services/api/, @some-dev)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := oeRepo(t)
			// Everything 404s, exactly as a GHES host does when /api/v3 is
			// missing from the base URL.
			api, _ := oeAPIWithOverride(t, func(w http.ResponseWriter, r *http.Request) bool {
				oeReply(w, http.StatusNotFound, `{"message":"Not Found"}`)
				return true
			})
			pol := oePolicy(t, `{"version":1,"ops":["`+tc.op+`"]}`)

			code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
				"--verify-owners", "--token", "t", "--api-url", api)

			if code != 3 {
				t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
			}
			if strings.Contains(errb, "does not exist") {
				t.Errorf("a wrong base URL was reported as a deleted owner:\n%s", errb)
			}
			if !strings.Contains(errb, "base URL") {
				t.Errorf("stderr does not send the operator to the base URL:\n%s", errb)
			}
			if strings.Contains(errb, policyGuidanceFragment) {
				t.Errorf("reported as a policy error; the policy is fine:\n%s", errb)
			}
			if got := oeContent(t, repo); got != oeBefore {
				t.Errorf("CODEOWNERS moved:\n%s", got)
			}
		})
	}
}

// SPEC R-41: $GITHUB_TOKEN is the documented fallback and reaches the wire.
// Asserting only exit 0 would pass against a build that sent no credential at
// all, against a stub that does not check one.
func TestR41_EnvTokenIsUsedAndReachesTheWire(t *testing.T) {
	repo := oeRepo(t)
	api, calls := oeAPI(t, "org/api-team", "org/everyone", "org/platform")
	pol := oeGhost(t)

	t.Setenv("GITHUB_TOKEN", "env-sentinel")
	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, errb)
	}
	if len(calls.auths()) == 0 {
		t.Fatal("no request was made, so the fallback proved nothing")
	}
	for _, a := range calls.auths() {
		if !strings.Contains(a, "env-sentinel") {
			t.Errorf("Authorization = %q, want the $GITHUB_TOKEN value", a)
		}
	}
}

// SPEC R-41: an explicit --token wins over the environment. One line of code
// and one precedence rule an operator relies on when a CI runner exports a
// token they did not choose.
func TestR41_ExplicitTokenBeatsTheEnvironment(t *testing.T) {
	repo := oeRepo(t)
	api, calls := oeAPI(t, "org/api-team", "org/everyone", "org/platform")

	t.Setenv("GITHUB_TOKEN", "env-sentinel")
	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", oeGhost(t),
		"--verify-owners", "--token", "flag-sentinel", "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, errb)
	}
	if len(calls.auths()) == 0 {
		t.Fatal("no request was made")
	}
	for _, a := range calls.auths() {
		if strings.Contains(a, "env-sentinel") {
			t.Errorf("the environment beat --token: %q", a)
		}
	}
}

// SPEC R-41: the credential never reaches stderr. `--api-url` and `--token`
// exist on the WRITING verbs for the first time, so redaction has to hold on
// a path nothing has ever driven — and
// `--api-url https://svc:hunter2@ghes.example` is a legal thing to type, in
// the one message a GHES operator is most likely to see (CWE-532). Every
// shape of R-41's output is driven, because the leak only has to happen once.
func TestR41_VerifyOwnersNeverPrintsTheCredential(t *testing.T) {
	const secret = "hunter2"
	const tokenSentinel = "ghp-tokensentinel"
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone")

	for _, tc := range []struct {
		name string
		args []string
		env  string
	}{
		{"dead owner", []string{"--verify-owners", "--token", tokenSentinel, "--api-url", api}, ""},
		{"unreachable api with userinfo", []string{"--verify-owners", "--token", tokenSentinel, "--api-url", "https://svc:" + secret + "@127.0.0.1:9/api/v3"}, ""},
		{"unparseable api url with userinfo", []string{"--verify-owners", "--token", tokenSentinel, "--api-url", "ht tp://svc:" + secret + "@example.invalid"}, ""},
		{"no token", []string{"--verify-owners"}, ""},
		{"token from the environment", []string{"--verify-owners", "--api-url", api}, tokenSentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tc.env)
			args := append([]string{"sync", "--repo", repo, "--policy", oeGhost(t)}, tc.args...)
			_, out, errb := runCLI(t, args...)
			for _, s := range []string{out, errb} {
				if strings.Contains(s, secret) {
					t.Errorf("the --api-url password reached the output:\n%s", s)
				}
				if strings.Contains(s, tokenSentinel) {
					t.Errorf("the token reached the output:\n%s", s)
				}
			}
		})
	}
}

// SPEC R-41/R-24: a refusal decided before the repository is opened writes no
// record, and a fleet that asked for one has to be told. Silence drops the
// repo out of results.jsonl entirely — the aggregation shows 99 rows for a
// 100-repo wave and nothing says which one is missing, so the count of repos
// needing attention goes DOWN. Both R-41 refusals go through their own
// closure, so a test through one leaves the other unproven.
func TestR41_RefusalSaysNoRecordWasWritten(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override oeOverride
	}{
		{"dead owner", nil},
		{"undecidable lookup", func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasPrefix(r.URL.Path, "/orgs/org/teams/") {
				oeReply(w, http.StatusInternalServerError, `{"message":"server error"}`)
				return true
			}
			return false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := oeRepo(t)
			api, _ := oeAPIWithOverride(t, tc.override, "org/api-team", "org/everyone")
			dir := t.TempDir()
			rec := filepath.Join(dir, "rec.json")
			sum := filepath.Join(dir, "sum.md")

			code, out, errb := runCLI(t, "sync", "--repo", repo, "--policy", oeGhost(t),
				"--format", "json", "--out", rec, "--summary-out", sum,
				"--verify-owners", "--token", "t", "--api-url", api)

			if code != 3 {
				t.Fatalf("exit = %d, want 3; stderr:\n%s", code, errb)
			}
			if !strings.Contains(errb, "no record was written") {
				t.Errorf("a fleet asking for a record was not told it got none:\n%s", errb)
			}
			// Half a record is worse than none: `jq -s` over the directory
			// would read it as an outcome.
			if strings.TrimSpace(out) != "" {
				t.Errorf("stdout was not empty under --format json:\n%s", out)
			}
			for _, p := range []string{rec, sum} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("%s was created by a refused run", p)
				}
			}
		})
	}
}

// SPEC R-41: the note is for the fleet that asked for a record, not for every
// run. A plain text run with no --out has nothing to disclose, and printing
// the note anyway would train operators to skip it.
func TestR41_NoRecordNoteIsAbsentWhenNoRecordWasAskedFor(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone")

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", oeGhost(t),
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Fatalf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if strings.Contains(errb, "no record was written") {
		t.Errorf("the note fired for a run that asked for no record:\n%s", errb)
	}
}

// SPEC R-41: a definitive "does not exist" beside an unanswerable lookup is
// reported as the dead owner, and the advice line says BOTH things. The dead
// owner is true whatever the rate limiter does next, so calling the whole run
// something to re-run would send the operator back for the same refusal; but
// the plain "fix the policy, do not retry" is false of the lookup nobody
// could answer, and an operator who believes it ships the fix and meets a
// second refusal they were told would not happen.
func TestR41_DeadOwnerBesideAnInconclusiveOneIsRefusedAsDead(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPIWithOverride(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/orgs/org/teams/flaky" {
			oeReply(w, http.StatusInternalServerError, `{"message":"server error"}`)
			return true
		}
		return false
	}, "org/api-team", "org/everyone")
	pol := oePolicy(t, `{"version":1,
	  "ops":["add_owner(/services/api/, [@org/ghost, @org/flaky])"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Fatalf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if !strings.Contains(errb, "@org/ghost") || !strings.Contains(errb, "does not exist") {
		t.Errorf("the dead owner is not reported as dead:\n%s", errb)
	}
	if !strings.Contains(errb, "@org/flaky") {
		t.Errorf("the unanswered lookup vanished; it is what the operator meets next:\n%s", errb)
	}
	if !strings.Contains(errb, "fix the policy") {
		t.Errorf("a run with a definitively dead owner must be reported as a policy error:\n%s", errb)
	}
	if !strings.Contains(errb, "re-run") {
		t.Errorf("the advice line does not say the unanswered lookup needs a re-run:\n%s", errb)
	}
	if strings.Contains(errb, policyGuidanceFragment) {
		t.Errorf("%q is false of the lookup that could not be answered:\n%s", policyGuidanceFragment, errb)
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("CODEOWNERS moved:\n%s", got)
	}
}

// SPEC R-41: the --op route has no policy file, so the flag is the only thing
// that can arm the check — a different branch from every policy case, and the
// one an operator explores with before writing the artifact.
func TestR41_OpRouteVerifiesWithoutAPolicyFile(t *testing.T) {
	t.Run("refuses a dead owner", func(t *testing.T) {
		repo := oeRepo(t)
		api, _ := oeAPI(t, "org/api-team", "org/everyone")

		code, _, errb := runCLI(t, "sync", "--repo", repo,
			"--op", "add_owner(/services/api/, @org/ghost)",
			"--verify-owners", "--token", "t", "--api-url", api)

		if code != 3 {
			t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
		}
		if !strings.Contains(errb, "@org/ghost") {
			t.Errorf("stderr does not name the owner:\n%s", errb)
		}
		if got := oeContent(t, repo); got != oeBefore {
			t.Errorf("CODEOWNERS moved:\n%s", got)
		}
	})
	t.Run("writes a live one", func(t *testing.T) {
		repo := oeRepo(t)
		api, _ := oeAPI(t, "org/api-team", "org/everyone", "org/platform")

		code, _, errb := runCLI(t, "sync", "--repo", repo,
			"--op", "add_owner(/services/api/, @org/platform)",
			"--verify-owners", "--token", "t", "--api-url", api)

		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr:\n%s", code, errb)
		}
		want := "* @org/everyone\n/services/api/ @org/api-team @org/platform\n"
		if got := oeContent(t, repo); got != want {
			t.Errorf("CODEOWNERS:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})
}

// SPEC R-41/R-13: a run that introduces NOBODY needs no credential. A wave
// whose ops only remove owners is the deleted-team cleanup this whole area
// recommends, and refusing it for want of a token would refuse the repair on
// the strength of a check with nothing to check. Same for a policy whose only
// new owners are email owners, which are unverifiable by construction.
func TestR41_APolicyThatIntroducesNobodyNeedsNoToken(t *testing.T) {
	for _, tc := range []struct{ name, before, ops, want string }{
		{"removal only",
			"* @org/everyone\n/services/api/ @org/api-team @org/ghost\n",
			`"on_empty":"error","ops":["remove_owner(/services/api/, @org/ghost)"]`,
			"* @org/everyone\n/services/api/ @org/api-team\n"},
		{"email owner only",
			"* @org/everyone\n/services/api/ @org/api-team\n",
			`"ops":["add_owner(/services/api/, dev@example.com)"]`,
			"* @org/everyone\n/services/api/ @org/api-team dev@example.com\n"},
		{"set_owners with an empty list",
			"* @org/everyone\n/services/api/ @org/api-team\n",
			`"ops":["set_owners(/services/api/, [])"]`,
			"* @org/everyone\n/services/api/\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t, map[string]string{
				"CODEOWNERS":        tc.before,
				"services/api/a.go": "package api\n",
			})
			api, calls := oeAPI(t)
			pol := oePolicy(t, `{"version":1,"verify_owners":true,`+tc.ops+`}`)

			t.Setenv("GITHUB_TOKEN", "")
			code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--api-url", api)

			if code != 0 {
				t.Fatalf("exit = %d, want 0 — there was nothing to verify; stderr:\n%s", code, errb)
			}
			if got := calls.all(); len(got) != 0 {
				t.Errorf("requests were made with nothing to ask about: %v", got)
			}
			if got := oeContent(t, repo); got != tc.want {
				t.Errorf("CODEOWNERS:\ngot:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// SPEC R-41/R-36: `verify_owners` is validated like every other policy field
// — a wrong type is a hard error naming the TYPE, and a near-miss spelling is
// an unknown field with a suggestion. The typo is the reported incident one
// level up: a policy that silently does NOT verify is the same failure class
// as an owner that silently owns nothing.
func TestR41_VerifyOwnersFieldIsValidated(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"string", `{"version":1,"verify_owners":"true","ops":["add_owner(/x/, @a)"]}`, "must be a boolean"},
		{"number", `{"version":1,"verify_owners":1,"ops":["add_owner(/x/, @a)"]}`, "must be a boolean"},
		{"null", `{"version":1,"verify_owners":null,"ops":["add_owner(/x/, @a)"]}`, "must be a boolean"},
		{"typo", `{"version":1,"verify_owner":true,"ops":["add_owner(/x/, @a)"]}`, "verify_owners"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pol := oePolicy(t, tc.body)
			code, _, errb := runCLI(t, "check", "--policy", pol)
			if code != 3 {
				t.Fatalf("exit = %d, want 3; stderr:\n%s", code, errb)
			}
			if !strings.Contains(errb, tc.want) {
				t.Errorf("stderr does not contain %q:\n%s", tc.want, errb)
			}
		})
	}
}

// SPEC R-41: `check --format json` stays a clean machine gate. R-41's own
// disclosures go to stderr, so stdout is still exactly one JSON object for
// `jq` — a note on stdout would break the fleet gate it is meant to serve.
func TestR41_CheckJSONStaysOneObjectOnStdout(t *testing.T) {
	api, _ := oeAPI(t, "org/platform")
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, [@org/platform, dev@example.com])"]}`)

	code, out, errb := runCLI(t, "check", "--policy", pol, "--format", "json",
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, errb)
	}
	if n := strings.Count(strings.TrimSpace(out), "\n"); n != 0 {
		t.Errorf("stdout is not a single line of JSON:\n%s", out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("stdout is not a JSON object:\n%s", out)
	}
	// The unverifiable email owner is disclosed, and on stderr.
	if !strings.Contains(errb, "dev@example.com") {
		t.Errorf("the unverifiable owner was not disclosed:\n%s", errb)
	}
}

// SPEC R-41: an owner written without being verified reaches the record's
// WARNINGS, not merely the file's bytes. A results.jsonl from a wave that
// wrote owners nobody could check must not read the same as one whose every
// owner was verified — and asserting the owner appears somewhere in the
// record proves nothing, because the record echoes the op that named it. Run
// without the flag as the control: the same record, no warning.
func TestR41_UnverifiableOwnersReachTheRecordsWarnings(t *testing.T) {
	warningsFor := func(t *testing.T, extra ...string) []string {
		t.Helper()
		repo := oeRepo(t)
		api, _ := oeAPI(t, "org/api-team", "org/everyone")
		rec := filepath.Join(t.TempDir(), "rec.json")
		pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, dev@example.com)"]}`)

		args := append([]string{"sync", "--repo", repo, "--policy", pol, "--out", rec,
			"--token", "t", "--api-url", api}, extra...)
		code, _, errb := runCLI(t, args...)
		if code != 0 {
			t.Fatalf("exit = %d, want 0; stderr:\n%s", code, errb)
		}
		b, err := os.ReadFile(rec)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Warnings []string `json:"warnings"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("record is not JSON: %v\n%s", err, b)
		}
		return doc.Warnings
	}

	verified := warningsFor(t, "--verify-owners")
	var found bool
	for _, w := range verified {
		if strings.Contains(w, "dev@example.com") && strings.Contains(w, "without verification") {
			found = true
		}
	}
	if !found {
		t.Errorf("the record does not warn about the owner written unverified: %q", verified)
	}
	// The control. Without it, the assertion above is satisfied by any record
	// that happens to mention the address — which every record does.
	for _, w := range warningsFor(t) {
		if strings.Contains(w, "without verification") {
			t.Errorf("a run that verified nothing still warned about verification: %q", w)
		}
	}
}

// SPEC R-41: a credential in --api-url never reaches the output, in ANY shape
// url.Parse can reject and any the transport can fail on. The first fix here
// redacted one shape and left the rest: a space inside the userinfo is what
// makes url.Parse fail in the first place, so a redaction that could not
// handle whitespace missed exactly the inputs it was written for; and
// `https://<PAT>@host` parses perfectly, putting the PAT where net/http masks
// only PASSWORDS, so it survived into every transport error (CWE-532).
//
// Each row is a real thing an operator types or a CI file holds. `127.0.0.1:9`
// refuses connections, so the rows that parse reach the transport branch.
func TestR41_NoCredentialShapeSurvivesIntoTheOutput(t *testing.T) {
	const secret = "hunter2"
	for _, apiURL := range []string{
		"https://svc:" + secret + "@127.0.0.1:9/api/v3",
		"https://svc:hun ter2@ghes.example/api/v3",
		"https://us er:" + secret + "@ghes.example/api/v3",
		"https://svc:" + secret + "@ghes.example/api/v3\r",
		"https://svc:" + secret + "@ghes.exa\tmple/api/v3",
		"http://svc:hun/ter2@ghes.example/api/v3",
		"ht tp://svc:" + secret + "@ghes.example/api/v3",
		// The PAT-as-username form: parses, and net/http masks passwords only.
		"https://ghp-" + secret + "pat@127.0.0.1:9/api/v3",
	} {
		t.Run(apiURL, func(t *testing.T) {
			repo := oeRepo(t)
			code, out, errb := runCLI(t, "sync", "--repo", repo, "--policy", oeGhost(t),
				"--verify-owners", "--token", "t", "--api-url", apiURL)
			if code == 0 {
				t.Fatalf("exit 0 against an unusable API URL:\n%s", errb)
			}
			for _, s := range []string{out, errb} {
				if strings.Contains(s, secret) {
					t.Errorf("the credential reached the output:\n%s", s)
				}
			}
			if got := oeContent(t, repo); got != oeBefore {
				t.Errorf("CODEOWNERS moved:\n%s", got)
			}
		})
	}
}

// SPEC R-41/R-12: a connection that dies mid-request refuses the write. It is
// the fifth branch of ghapi's classifier and the one no status-code table can
// reach — and the branch that leaked a token-as-username until the redaction
// moved to the client's own way out.
func TestR41_ATransportFailureRefusesTheWrite(t *testing.T) {
	repo := oeRepo(t)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", oeGhost(t),
		"--verify-owners", "--token", "t", "--api-url", "http://127.0.0.1:9")

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if strings.Contains(errb, "does not exist") {
		t.Errorf("a dead connection was reported as a missing owner:\n%s", errb)
	}
	if strings.Contains(errb, policyGuidanceFragment) {
		t.Errorf("reported as a policy error; the policy is fine:\n%s", errb)
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("CODEOWNERS moved:\n%s", got)
	}
}
