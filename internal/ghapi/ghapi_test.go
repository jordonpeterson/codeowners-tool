// Package ghapi_test defines the GitHub API client's fail-closed contract.
//
// SPEC R-12: a lookup that cannot be answered definitively (bad token,
// missing scope, rate limit, network failure) is INCONCLUSIVE — never a
// negative. The worst failure this tool can produce is an expired token
// quietly stripping half the owners from a monorepo.
// SPEC R-15: lookups are cached in memory per run and on disk across runs
// with a TTL. Inconclusive results are never cached.
package ghapi_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jordonpropm/codeowners-tool/internal/ghapi"
)

func server(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for pat, h := range routes {
		mux.HandleFunc(pat, h)
	}
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func ok(body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }
}
func status(code int) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
}

// A 404 on /users/{login} with a working token is a definitive negative.
func TestUserExists(t *testing.T) {
	s := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"/users/alive": ok(`{"login":"alive"}`),
		"/users/dead":  status(404),
		"/user":        ok(`{"login":"me"}`),
	})
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	if got, err := c.UserExists("alive"); err != nil || !got {
		t.Errorf("alive: got=%v err=%v", got, err)
	}
	if got, err := c.UserExists("dead"); err != nil || got {
		t.Errorf("dead: got=%v err=%v", got, err)
	}
}

// SPEC R-12: 401/403 (bad token, missing scope) is inconclusive, not "user
// doesn't exist".
func TestR12_AuthFailureIsInconclusive(t *testing.T) {
	s := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"/users/x": status(401),
	})
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	_, err := c.UserExists("x")
	var inc *ghapi.Inconclusive
	if !errors.As(err, &inc) {
		t.Fatalf("401 must be Inconclusive, got %v", err)
	}
}

// SPEC R-12: rate limiting is inconclusive — a rate-limited 403 looks like a
// forbidden 403 and neither is a negative.
func TestR12_RateLimitIsInconclusive(t *testing.T) {
	s := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"/users/x": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(403)
		},
	})
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	_, err := c.UserExists("x")
	var inc *ghapi.Inconclusive
	if !errors.As(err, &inc) {
		t.Fatalf("rate limit must be Inconclusive, got %v", err)
	}
}

// ProbeOrg is the precondition that lets a later team/member 404 count as a
// true negative: it proves the token can enumerate the org at all (R-12).
func TestR12_ProbeOrgGatesTeamLookups(t *testing.T) {
	s := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"/orgs/closed/teams":   status(403),
		"/orgs/closed/members": status(403),
	})
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	err := c.ProbeOrg("closed")
	var inc *ghapi.Inconclusive
	if !errors.As(err, &inc) {
		t.Fatalf("unreadable org must be Inconclusive, got %v", err)
	}
}

// Team write-access: 200 with push/maintain/admin permission is access;
// 404 after a clean probe means the team does not manage the repo (A-3).
func TestTeamHasWrite(t *testing.T) {
	s := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"/orgs/o/teams/writers/repos/o/r": ok(`{"permissions":{"admin":false,"push":true,"pull":true}}`),
		"/orgs/o/teams/readers/repos/o/r": ok(`{"permissions":{"admin":false,"push":false,"pull":true}}`),
		"/orgs/o/teams/absent/repos/o/r":  status(404),
	})
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	for _, tc := range []struct {
		team string
		want bool
	}{{"writers", true}, {"readers", false}, {"absent", false}} {
		got, err := c.TeamHasWrite("o", tc.team, "o", "r")
		if err != nil || got != tc.want {
			t.Errorf("%s: got=%v err=%v want=%v", tc.team, got, err, tc.want)
		}
	}
}

// SPEC R-15: definitive results are cached — the second identical lookup
// makes no HTTP call. Assume thousands of lookups per run.
func TestR15_DefinitiveResultsCached(t *testing.T) {
	calls := 0
	s := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"/users/u": func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Write([]byte(`{"login":"u"}`))
		},
	})
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	c.UserExists("u")
	c.UserExists("u")
	if calls != 1 {
		t.Errorf("HTTP calls = %d, want 1 (cached)", calls)
	}
}

// SPEC R-12 + R-15: inconclusive results are NEVER cached — a transient
// failure must not poison later runs.
func TestR12_InconclusiveNotCached(t *testing.T) {
	calls := 0
	s := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"/users/u": func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.WriteHeader(500)
				return
			}
			w.Write([]byte(`{"login":"u"}`))
		},
	})
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	if _, err := c.UserExists("u"); err == nil {
		t.Fatal("first call must be inconclusive")
	}
	got, err := c.UserExists("u")
	if err != nil || !got {
		t.Errorf("second call must retry and succeed: got=%v err=%v", got, err)
	}
}

// SPEC R-15: the disk cache honors its TTL.
func TestR15_DiskCacheTTL(t *testing.T) {
	dir := t.TempDir()
	fresh := ghapi.NewDiskCache(dir, time.Hour)
	fresh.Set("k", []byte("v"))
	if v, ok := fresh.Get("k"); !ok || string(v) != "v" {
		t.Error("fresh entry must hit")
	}
	stale := ghapi.NewDiskCache(dir, -time.Second)
	if _, ok := stale.Get("k"); ok {
		t.Error("expired entry must miss")
	}
}

// Review finding: GitHub answers 302 on /orgs/{org}/members/{login} when the
// requester cannot see the membership; following the redirect lands on
// /public_members, where a CONCEALED member 404s — a false definitive
// negative that would propose removing a real member. Redirects are never
// followed and always inconclusive.
func TestR12_MembershipRedirectIsInconclusive(t *testing.T) {
	s := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"/orgs/o/members/concealed": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/orgs/o/public_members/concealed")
			w.WriteHeader(302)
		},
		"/orgs/o/public_members/concealed": status(404),
	})
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	_, err := c.UserIsOrgMember("o", "concealed")
	var inc *ghapi.Inconclusive
	if !errors.As(err, &inc) {
		t.Fatalf("302 on membership must be Inconclusive, got %v (a false 'not a member' here proposes removing a real member)", err)
	}
}

// Review finding: cache keys must be scoped to (BaseURL, token) — a cache
// directory shared across hosts or tokens must never serve one context's
// definitive answers (or probe successes) to another.
func TestR15_CacheScopedByHostAndToken(t *testing.T) {
	shared := ghapi.NewMemCache()
	sA := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"/users/alice": ok(`{"login":"alice"}`),
		"/user":        ok(`{"login":"me"}`),
	})
	sB := server(t, map[string]func(http.ResponseWriter, *http.Request){
		"/users/alice": status(404),
		"/user":        ok(`{"login":"me"}`),
	})
	a := ghapi.New(sA.URL, "tokA", shared)
	b := ghapi.New(sB.URL, "tokB", shared)
	if got, err := a.UserExists("alice"); err != nil || !got {
		t.Fatalf("host A: got=%v err=%v", got, err)
	}
	got, err := b.UserExists("alice")
	if err != nil || got {
		t.Fatalf("host B must get ITS OWN answer (false), not host A's cached true: got=%v err=%v", got, err)
	}
}

// Second-review finding: a mistyped API base URL (e.g. GHES without /api/v3)
// 404s on EVERY endpoint. Without a positive-control probe, every user owner
// would be marked dead with removal ops — the mass false-negative R-12
// exists to prevent. A user 404 is definitive only after GET /user succeeds.
func TestR12_WrongBaseURLNeverDefinitive(t *testing.T) {
	s := server(t, map[string]func(http.ResponseWriter, *http.Request){}) // everything 404s
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	_, err := c.UserExists("anyone")
	var inc *ghapi.Inconclusive
	if !errors.As(err, &inc) {
		t.Fatalf("user 404 without a working /user probe must be Inconclusive, got %v", err)
	}
}
