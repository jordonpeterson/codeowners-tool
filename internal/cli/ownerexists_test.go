// Owner-existence end-to-end tests (R-41). Written ahead of the implementation
// per CONTRIBUTING.md.
//
// The failure this file is about was reported from a real rollout. A policy
// named a live team and a team that had been renamed months earlier:
//
//	add_owner(/services/api/, [@org/api-team, @org/plaform])
//
// `sync` applied it, reported `proven: tree`, exit 0, and wrote both owners.
// The proof was sound and the line was dead: GitHub silently ignores an owner
// it cannot resolve, so `/services/api/` ended up owned by @org/api-team alone
// and the co-ownership the wave existed to establish was never in force. This
// is the "applied, dead on arrival" outcome the fleet verbs are supposed to
// make impossible, and it landed in 100 repositories at once because nothing
// in the write path ever asks GitHub whether an owner exists — `audit` asks,
// after the fact, in a run nobody had a reason to make.
//
// The vacuity trap here is the usual one for a feature that does not exist
// yet: today's binary rejects `--verify-owners` as an unknown flag and exits
// 3, which is the same code a correct refusal returns. Every negative case
// therefore asserts a message fragment as well, and every fragment names
// something only the implemented feature can say. The positive cases —
// verification passing, and the offline default — assert the FILE BYTES, so a
// build that refuses everything cannot pass them.
//
// Ownership assertions never use strings.Contains on file content: under
// last-match-wins (S-1) a substring check is satisfied by a file whose line
// ORDER hands the path to the wrong owner.
package cli_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ---------- stub GitHub ----------

// oeAPI is a healthy GitHub where exactly the owners in `exists` exist, and
// the token is an org owner (so a team 404 is definitive rather than "secret
// team, invisible to you" — see ghapi.ViewerIsOrgAdmin).
//
// It returns the base URL and a pointer to the request log, so a test can
// prove that a run made NO calls at all — the offline default's real claim,
// which an exit code cannot express.
//
// Entries in `exists` are "org/slug" for a team and "login" for a user; a
// leading "@" is accepted and ignored.
func oeAPI(t *testing.T, exists ...string) (string, *oeCalls) {
	t.Helper()
	return oeAPIWithOverride(t, nil, exists...)
}

// oeOverride handles a request specially and reports whether it did — how a
// test injects one rate-limited or non-owner-token endpoint into an otherwise
// healthy API.
type oeOverride func(w http.ResponseWriter, r *http.Request) bool

type oeCalls struct {
	mu   sync.Mutex
	list []string
	// auth is every Authorization header the stub saw, in order. Which
	// credential actually reached the wire is not observable from an exit
	// code, and --token's precedence over $GITHUB_TOKEN is one line of code
	// and one rule an operator relies on.
	auth []string
}

func (c *oeCalls) add(p, authHeader string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, p)
	c.auth = append(c.auth, authHeader)
}

func (c *oeCalls) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.list...)
}

func (c *oeCalls) auths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.auth...)
}

func oeAPIWithOverride(t *testing.T, override oeOverride, exists ...string) (string, *oeCalls) {
	t.Helper()
	set := make(map[string]bool, len(exists))
	for _, e := range exists {
		set[strings.TrimPrefix(e, "@")] = true
	}
	calls := &oeCalls{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.add(r.URL.Path, r.Header.Get("Authorization"))
		if override != nil && override(w, r) {
			return
		}
		seg := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case len(seg) == 1 && seg[0] == "user": // ProbeAPI
			oeReply(w, http.StatusOK, `{"login":"ci-bot"}`)
		case len(seg) == 3 && seg[0] == "orgs" && (seg[2] == "teams" || seg[2] == "members"): // ProbeOrg
			oeReply(w, http.StatusOK, `[]`)
		case len(seg) == 4 && seg[0] == "user" && seg[1] == "memberships" && seg[2] == "orgs": // ViewerIsOrgAdmin
			oeReply(w, http.StatusOK, `{"state":"active","role":"admin"}`)
		case len(seg) == 4 && seg[0] == "orgs" && seg[2] == "teams": // TeamExists
			oeExistsReply(w, set[seg[1]+"/"+seg[3]])
		case len(seg) == 2 && seg[0] == "users": // UserExists
			oeExistsReply(w, set[seg[1]])
		default:
			oeReply(w, http.StatusNotFound, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, calls
}

func oeReply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func oeExistsReply(w http.ResponseWriter, ok bool) {
	if ok {
		oeReply(w, http.StatusOK, `{"id":1}`)
		return
	}
	oeReply(w, http.StatusNotFound, `{"message":"Not Found"}`)
}

// ---------- fixtures ----------

// oeRepo is the reported scenario's shape: a catch-all, a narrower rule with
// one live owner, and two files under the scope so a paths-changed count is
// not confusable with an owner count.
func oeRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"CODEOWNERS":        "* @org/everyone\n/services/api/ @org/api-team\n",
		"services/api/a.go": "package api\n",
		"services/api/b.go": "package api\n",
		"top.md":            "top\n",
	})
}

// oePolicy writes a policy file and returns its path.
func oePolicy(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// oeContent reads the repo's CODEOWNERS as bytes. Every mutating assertion in
// this file compares the WHOLE file: R-41 refuses, and "refused" means the
// bytes did not move at all.
func oeContent(t *testing.T, repo string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const oeBefore = "* @org/everyone\n/services/api/ @org/api-team\n"

// ---------- the reported bug ----------

// SPEC R-41: the reported failure. A policy naming one live team and one team
// that does not exist is refused under --verify-owners, and NOTHING is
// written — not even the live half of the list, which is the whole point: a
// partial application would put a rule in force that states an ownership
// nobody asked for.
//
// Exit 3, not 2: "@org/not-real-team does not exist" is a fact about the
// policy and about GitHub, not about which clone the run is standing in, so
// it is identical on every repository in the wave and halting at repo 0 beats
// recording the same refusal 100 times (CONTRIBUTING, "exit codes are a
// contract").
func TestR41_SyncRefusesAnOwnerThatDoesNotExist(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone")
	pol := oePolicy(t, `{"version":1,"name":"api co-ownership",
	  "ops":["add_owner(/services/api/, [@org/api-team, @org/not-real-team])"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3 (the policy is broken on every repo); stderr:\n%s", code, errb)
	}
	if !strings.Contains(errb, "@org/not-real-team") {
		t.Errorf("stderr does not name the owner that does not exist:\n%s", errb)
	}
	if !strings.Contains(errb, "does not exist") {
		t.Errorf("stderr does not say the owner does not exist:\n%s", errb)
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("CODEOWNERS was written despite the refusal:\ngot:\n%s\nwant:\n%s", got, oeBefore)
	}
}

// SPEC R-41: the live owner in the same list is not a reason to write. The
// bug report's list was [live, dead]; a fix that dropped only the dead owner
// and applied the rest would silently apply an ownership the reviewed policy
// does not state, which is worse than refusing.
func TestR41_NoPartialApplicationOfAMixedList(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone", "org/live")
	pol := oePolicy(t, `{"version":1,
	  "ops":["add_owner(/services/api/, [@org/live, @org/ghost])",
	         "add_owner(/top.md, @org/live)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	// The unrelated second op is proof the refusal is whole-run: it names only
	// live owners and would have applied cleanly on its own.
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("CODEOWNERS moved; the refusal must be whole-run:\ngot:\n%s", got)
	}
	if !strings.Contains(errb, "@org/ghost") {
		t.Errorf("stderr does not name @org/ghost:\n%s", errb)
	}
}

// SPEC R-41: --dry-run verifies too. The preview is where a fleet operator
// expects to find this, and a --dry-run that reported "applied" for a policy
// the real run would refuse is a preview of something that never happens.
func TestR41_DryRunVerifiesToo(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team")
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, @org/ghost)"]}`)

	code, out, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--dry-run",
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if !strings.Contains(errb, "@org/ghost") || !strings.Contains(errb, "does not exist") {
		t.Errorf("stderr does not name the owner and say it does not exist:\n%s", errb)
	}
	if strings.Contains(out, "applied") {
		t.Errorf("--dry-run reported an application it would refuse:\n%s", out)
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("--dry-run wrote:\n%s", got)
	}
}

// SPEC R-41: an owner that exists is written exactly as before. This is the
// case that makes every refusal above mean something — a build that refused
// unconditionally would pass all of them — so it asserts the resulting BYTES
// rather than the exit code.
func TestR41_LiveOwnersAreWritten(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone", "org/platform")
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, @org/platform)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, errb)
	}
	want := "* @org/everyone\n/services/api/ @org/api-team @org/platform\n"
	if got := oeContent(t, repo); got != want {
		t.Errorf("CODEOWNERS:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// SPEC R-41: verification is opt-in and the default run touches no network at
// all. The claim is not "it exits 0" — an implementation that called the API
// and happened to succeed would too — it is that ZERO requests were made, so
// a sync in an air-gapped runner or with no token keeps working exactly as it
// did before R-41.
func TestR41_OfflineByDefaultMakesNoAPICalls(t *testing.T) {
	repo := oeRepo(t)
	api, calls := oeAPI(t) // nothing exists here; a run that asked would refuse
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, @org/ghost)"]}`)

	// --api-url is passed WITHOUT --verify-owners: if a future change ever
	// makes the default run reach the network, it hits this stub and the call
	// log catches it, rather than the real api.github.com.
	t.Setenv("GITHUB_TOKEN", "t")
	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0 (R-41 is opt-in); stderr:\n%s", code, errb)
	}
	if got := calls.all(); len(got) != 0 {
		t.Errorf("default sync made API calls: %v — R-41 must be opt-in", got)
	}
	// Opt-in must not mean silent: an operator who passed a credential and
	// forgot the flag would otherwise read exit 0 as "verified", which is the
	// same silent no-op shape as the bug R-41 exists to stop.
	if !strings.Contains(errb, "no owner was verified") {
		t.Errorf("a credential was passed and did nothing, silently:\n%s", errb)
	}
	want := "* @org/everyone\n/services/api/ @org/api-team @org/ghost\n"
	if got := oeContent(t, repo); got != want {
		t.Errorf("CODEOWNERS:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// SPEC R-41/R-12: an undecidable lookup is not a licence to write. A rate
// limit, a 5xx or an expired token makes "does this owner exist" unanswerable,
// and the run refuses exactly as it does for a dead owner — the fail-closed
// posture the audit engine already takes, applied to the verb that WRITES.
//
// The message must not read as a policy error: the operator's next action is
// to re-run, not to edit the policy, and "fix the policy, do not retry" would
// send them to change a file that is correct.
func TestR41_InconclusiveLookupWritesNothing(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPIWithOverride(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/teams/platform") {
			w.Header().Set("X-RateLimit-Remaining", "0")
			oeReply(w, http.StatusForbidden, `{"message":"API rate limit exceeded"}`)
			return true
		}
		return false
	}, "org/api-team", "org/everyone")
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, @org/platform)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	// Exactly 3, not merely non-zero: exit 2 would tell a fleet "this repo
	// needs a human" for a condition no repository had anything to do with.
	if code != 3 {
		t.Fatalf("exit = %d, want 3 on an undecidable lookup; stderr:\n%s", code, errb)
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("wrote on an undecidable lookup:\ngot:\n%s", got)
	}
	if !strings.Contains(errb, "R-12") {
		t.Errorf("stderr does not cite the fail-closed rule:\n%s", errb)
	}
	if strings.Contains(errb, policyGuidanceFragment) {
		t.Errorf("an undecidable lookup was reported as a policy error — the remedy is to re-run, not to edit the policy:\n%s", errb)
	}
}

// SPEC R-41/R-12: a team 404 seen by a token that is not an org owner means
// "deleted OR secret and invisible to you", and those are the same HTTP
// response. Refusing is right either way; calling it "does not exist" is not,
// because the operator's fix is a different one — re-run with an org-owner
// token, not delete the team from the policy.
func TestR41_TeamNotFoundIsInconclusiveForANonOwnerToken(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPIWithOverride(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/user/memberships/orgs/") {
			oeReply(w, http.StatusOK, `{"state":"active","role":"member"}`)
			return true
		}
		return false
	}, "org/api-team", "org/everyone")
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, @org/secret-team)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Fatalf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if strings.Contains(errb, "does not exist") {
		t.Errorf("a 404 this token cannot interpret was reported as a deletion:\n%s", errb)
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("wrote an owner it could not verify:\ngot:\n%s", got)
	}
	if !strings.Contains(errb, "secret") {
		t.Errorf("stderr does not explain that a secret team returns the same 404:\n%s", errb)
	}
}

// SPEC R-41/R-13: an email owner resolves through a verified address the API
// cannot see, so it is UNVERIFIABLE rather than dead. Refusing it would make
// R-41 unusable for every policy that has one, and treating "cannot check" as
// "does not exist" is the mass false negative R-12 exists to prevent.
func TestR41_EmailOwnersAreUnverifiableNotDead(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone")
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, dev@example.com)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0 (R-13); stderr:\n%s", code, errb)
	}
	want := "* @org/everyone\n/services/api/ @org/api-team dev@example.com\n"
	if got := oeContent(t, repo); got != want {
		t.Errorf("CODEOWNERS:\ngot:\n%s\nwant:\n%s", got, want)
	}
	// Silence here would be the defect: the run wrote an owner it did not
	// verify, and the operator asked for verification.
	if !strings.Contains(errb, "dev@example.com") {
		t.Errorf("stderr does not disclose the unverifiable owner:\n%s", errb)
	}
}

// SPEC R-41: remove_owner's owners are never looked up. Removing a team that
// was deleted is the whole reason the op exists, and verifying it would refuse
// exactly the cleanup R-41's sibling checks recommend.
func TestR41_RemoveOwnerDoesNotRequireTheOwnerToExist(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":        "* @org/everyone\n/services/api/ @org/api-team @org/ghost\n",
		"services/api/a.go": "package api\n",
	})
	api, _ := oeAPI(t, "org/api-team", "org/everyone")
	pol := oePolicy(t, `{"version":1,"on_empty":"error",
	  "ops":["remove_owner(/services/api/, @org/ghost)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, errb)
	}
	want := "* @org/everyone\n/services/api/ @org/api-team\n"
	if got := oeContent(t, repo); got != want {
		t.Errorf("CODEOWNERS:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// SPEC R-41: rename_owner checks the NEW name and not the old one. The old
// name is on its way out — a rename away from a deleted team is the common
// case — while the new name is what the run puts into force.
func TestR41_RenameChecksTheNewNameOnly(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":        "* @org/everyone\n/services/api/ @org/gone-team\n",
		"services/api/a.go": "package api\n",
	})
	api, _ := oeAPI(t, "org/everyone", "org/new-team")
	pol := oePolicy(t, `{"version":1,"ops":["rename_owner(@org/gone-team, @org/new-team)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0 — the OLD name must not be looked up; stderr:\n%s", code, errb)
	}
	want := "* @org/everyone\n/services/api/ @org/new-team\n"
	if got := oeContent(t, repo); got != want {
		t.Errorf("CODEOWNERS:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// SPEC R-41: a rename to a name that does not exist is refused. The op is the
// one that rewrites owners as plain text, so nothing downstream would ever
// notice that the file now names a team GitHub cannot resolve.
func TestR41_RenameToANameThatDoesNotExistIsRefused(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":        "* @org/everyone\n/services/api/ @org/old-team\n",
		"services/api/a.go": "package api\n",
	})
	before := "* @org/everyone\n/services/api/ @org/old-team\n"
	api, _ := oeAPI(t, "org/everyone", "org/old-team")
	pol := oePolicy(t, `{"version":1,"ops":["rename_owner(@org/old-team, @org/typo-team)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if !strings.Contains(errb, "@org/typo-team") {
		t.Errorf("stderr does not name the new owner:\n%s", errb)
	}
	if got := oeContent(t, repo); got != before {
		t.Errorf("CODEOWNERS moved:\ngot:\n%s", got)
	}
}

// SPEC R-41: a user owner is checked the same way a team is. `@jdoe` who left
// the company two years ago is the same dead line as a deleted team, and the
// user endpoint is a different code path from the team one.
func TestR41_UserOwnersAreCheckedToo(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone")
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, @departed-dev)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if !strings.Contains(errb, "@departed-dev") {
		t.Errorf("stderr does not name the user:\n%s", errb)
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("CODEOWNERS moved:\ngot:\n%s", got)
	}
}

// SPEC R-41/R-38a: the lookup is case-folded, the file's bytes are not.
// GitHub treats `@Org/API-Team` and `@org/api-team` as one owner; asking the
// API about the mixed-case spelling risks a 404 that means nothing more than
// "you typed it differently", and under R-41 a 404 REFUSES a whole wave.
func TestR41_LookupsAreCaseFolded(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone", "org/platform")
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, @Org/Platform)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0 — the lookup must fold case; stderr:\n%s", code, errb)
	}
	// R-38b: folding governs MATCHING, never output. The operator's spelling
	// reaches the file verbatim.
	want := "* @org/everyone\n/services/api/ @org/api-team @Org/Platform\n"
	if got := oeContent(t, repo); got != want {
		t.Errorf("CODEOWNERS:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// SPEC R-41: --verify-owners with no token is exit 3, never a silent skip.
// "Verify these owners" and "I have no way to" cannot both be honoured, and
// quietly doing the offline thing would report success for the very run the
// operator asked to be checked — the vacuous pass that made this bug possible
// in the first place.
func TestR41_VerifyOwnersWithoutATokenIsRefused(t *testing.T) {
	repo := oeRepo(t)
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, @org/platform)"]}`)

	t.Setenv("GITHUB_TOKEN", "")
	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--verify-owners")

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	// Not "token": every --token/--api-url usage line contains that word, so a
	// flag-parse dump would satisfy it. This fragment appears nowhere but the
	// refusal.
	if !strings.Contains(errb, "not decidable offline") {
		t.Errorf("stderr does not say owner existence cannot be decided offline:\n%s", errb)
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("CODEOWNERS moved:\ngot:\n%s", got)
	}
}

// SPEC R-41: the requirement belongs in the reviewed artifact. A fleet's
// guarantee cannot depend on every call site remembering a flag, so
// `"verify_owners": true` turns it on for every run of that policy — the same
// argument `create` and `max_paths_changed` are policy fields for (R-34,
// R-25).
func TestR41_PolicyFieldTurnsVerificationOn(t *testing.T) {
	repo := oeRepo(t)
	api, calls := oeAPI(t, "org/api-team", "org/everyone")
	pol := oePolicy(t, `{"version":1,"verify_owners":true,
	  "ops":["add_owner(/services/api/, @org/ghost)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3 — the policy asked for verification; stderr:\n%s", code, errb)
	}
	// Without these two the test passes against a binary that has never heard
	// of the field: an unknown top-level key is itself exit 3 and leaves the
	// file untouched, for the wrong reason. The call log is what proves a
	// lookup actually happened.
	if !strings.Contains(errb, "@org/ghost") || !strings.Contains(errb, "does not exist") {
		t.Errorf("stderr does not name the owner and say it does not exist:\n%s", errb)
	}
	if len(calls.all()) == 0 {
		t.Error("the policy field did not arm the check: no API request was made, so exit 3 came from somewhere other than R-41")
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("CODEOWNERS moved:\ngot:\n%s", got)
	}
}

// SPEC R-41/R-20: the command line may add the owner check but never remove
// it. `--verify-owners` beside `--policy` is legal precisely because it
// changes nothing that gets written — it can only refuse — so the artifact in
// git still means what it says; `--verify-owners=false` against a policy that
// asked for verification is the direction that would make a reviewed
// guarantee depend on one call site remembering not to drop it.
func TestR41_FlagCannotSwitchOffAReviewedPolicy(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone", "org/platform")
	pol := oePolicy(t, `{"version":1,"verify_owners":true,
	  "ops":["add_owner(/services/api/, @org/platform)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners=false", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	// Not "verify_owners": --verify-owners' own usage text names the field, so
	// that fragment is satisfied by any flag-parse dump from sync.
	if !strings.Contains(errb, "cannot switch off") {
		t.Errorf("stderr does not say the flag cannot switch the reviewed check off:\n%s", errb)
	}
	if got := oeContent(t, repo); got != oeBefore {
		t.Errorf("CODEOWNERS moved:\n%s", got)
	}
}

// SPEC R-41/R-22: every bad owner in the policy is reported in ONE run.
// Fixing a generated 40-op baseline one refusal per invocation is miserable,
// and each invocation costs a fleet another round trip before repo 0.
func TestR41_EveryDeadOwnerIsReportedInOneRun(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone")
	pol := oePolicy(t, `{"version":1,
	  "ops":["add_owner(/services/api/, @org/ghost-one)",
	         "add_owner(/top.md, [@org/ghost-two, @ghost-user])"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Fatalf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	for _, want := range []string{"@org/ghost-one", "@org/ghost-two", "@ghost-user"} {
		if !strings.Contains(errb, want) {
			t.Errorf("stderr does not name %s — a second invocation would be needed to find it:\n%s", want, errb)
		}
	}
}

// SPEC R-41/R-12: one dead resolver makes every lookup in the run fail the
// same way, and forty lines of the same sentence bury the one fact that
// matters — which is the reason, not the roll call. Owners sharing a reason
// are named together, once.
func TestR41_OneReasonIsReportedOnceForAllTheOwnersItCovers(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPIWithOverride(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/orgs/") {
			oeReply(w, http.StatusInternalServerError, `{"message":"server error"}`)
			return true
		}
		return false
	})
	pol := oePolicy(t, `{"version":1,
	  "ops":["add_owner(/services/api/, [@org/one, @org/two])"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Fatalf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if n := strings.Count(errb, "refusing to write an owner whose existence is unproven"); n != 1 {
		t.Errorf("the same reason printed %d times, want 1:\n%s", n, errb)
	}
	for _, want := range []string{"@org/one", "@org/two"} {
		if !strings.Contains(errb, want) {
			t.Errorf("stderr does not name %s:\n%s", want, errb)
		}
	}
}

// SPEC R-41/R-38a: one owner named by many ops costs ONE lookup. A 40-op
// baseline naming the same platform team throughout would otherwise spend 40
// requests of a rate limit to answer one question, and two capitalisations of
// it could return two contradictory verdicts.
func TestR41_ARepeatedOwnerIsLookedUpOnce(t *testing.T) {
	repo := oeRepo(t)
	api, calls := oeAPI(t, "org/api-team", "org/everyone", "org/platform")
	pol := oePolicy(t, `{"version":1,
	  "ops":["add_owner(/services/api/, @org/platform)",
	         "add_owner(/top.md, @Org/Platform)"]}`)

	code, _, errb := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, errb)
	}
	n := 0
	for _, c := range calls.all() {
		if strings.HasPrefix(c, "/orgs/org/teams/") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("team lookups = %d, want 1 (%v)", n, calls.all())
	}
}

// SPEC R-41: `plan` is the other route into a write, and it is checked where
// intent is stated rather than where it is executed. A plan file naming a team
// that does not exist is an artifact a human APPROVES, after which every
// downstream refusal is too late to matter — so no plan file is produced at
// all. Exit 3: a dead owner is invalid input, not a property of this clone.
func TestR41_PlanRefusesAnOwnerThatDoesNotExist(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPI(t, "org/api-team", "org/everyone")
	planPath := filepath.Join(t.TempDir(), "plan.json")

	code, _, errb := runCLI(t, "plan", "--repo", repo, "--out", planPath,
		"--op", "add_owner(/services/api/, @org/ghost)",
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if !strings.Contains(errb, "@org/ghost") {
		t.Errorf("stderr does not name the owner:\n%s", errb)
	}
	if _, err := os.Stat(planPath); err == nil {
		t.Error("a plan file was written for a policy the run refused")
	}
}

// SPEC R-41/R-12: `plan` has an exit code for "the check could not be made"
// that `sync` does not — 5, inconclusive, fail-closed — and an unanswerable
// owner lookup is exactly what it is for. Reporting it as exit 3 would tell
// the operator their ops are wrong when the ops are fine.
func TestR41_PlanReportsAnUnanswerableLookupAsInconclusive(t *testing.T) {
	repo := oeRepo(t)
	api, _ := oeAPIWithOverride(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/teams/platform") {
			oeReply(w, http.StatusInternalServerError, `{"message":"server error"}`)
			return true
		}
		return false
	}, "org/api-team", "org/everyone")
	planPath := filepath.Join(t.TempDir(), "plan.json")

	code, _, errb := runCLI(t, "plan", "--repo", repo, "--out", planPath,
		"--op", "add_owner(/services/api/, @org/platform)",
		"--verify-owners", "--token", "t", "--api-url", api)

	if code != 5 {
		t.Errorf("exit = %d, want 5 (inconclusive, fail-closed); stderr:\n%s", code, errb)
	}
	if _, err := os.Stat(planPath); err == nil {
		t.Error("a plan file was written on an unanswerable lookup")
	}
}

// SPEC R-41: `check` answers the same question with no repository open, which
// is the point — one lookup at repo 0 instead of a hundred refusals. It is
// the cheapest place to catch the reported typo.
func TestR41_CheckVerifiesWithNoRepository(t *testing.T) {
	api, _ := oeAPI(t, "org/api-team")
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, [@org/api-team, @org/not-real-team])"]}`)

	code, _, errb := runCLI(t, "check", "--policy", pol, "--verify-owners", "--token", "t", "--api-url", api)

	if code != 3 {
		t.Errorf("exit = %d, want 3; stderr:\n%s", code, errb)
	}
	if !strings.Contains(errb, "@org/not-real-team") {
		t.Errorf("stderr does not name the owner:\n%s", errb)
	}
}

// SPEC R-41: `check` without the flag stays what it has always been — a
// policy read that opens nothing and reaches nothing. A fleet gate that
// started making network calls because a validator was added would fail in
// exactly the environments `check` is cheapest in.
func TestR41_CheckIsOfflineByDefault(t *testing.T) {
	api, calls := oeAPI(t)
	pol := oePolicy(t, `{"version":1,"ops":["add_owner(/services/api/, @org/ghost)"]}`)

	t.Setenv("GITHUB_TOKEN", "t")
	code, _, errb := runCLI(t, "check", "--policy", pol, "--api-url", api)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, errb)
	}
	if got := calls.all(); len(got) != 0 {
		t.Errorf("check made API calls: %v", got)
	}
}

// policyGuidanceFragment is the advice sync prints for a genuine policy error.
// An inconclusive lookup must NOT carry it: the policy is fine and the remedy
// is to re-run. Kept as a literal so a reworded guidance line makes the
// assertion above fail loudly rather than pass vacuously.
const policyGuidanceFragment = "fix the policy, do not retry"
