// Package ghapi is the GitHub REST client for Engine B (audit). It never
// writes anywhere.
//
// Fail-closed contract (R-12): every lookup returns either a definitive
// answer or *Inconclusive. HTTP 401/403, rate limiting, 5xx, and transport
// errors are inconclusive — never negatives. A team/member 404 counts as a
// true negative only after ProbeOrg proved the token can enumerate that org,
// which removes "invisible to this token's scopes" from the 404's possible
// meanings; rate limiting is detected separately via headers.
//
// Works against github.com and GHES via BaseURL (--api-url).
package ghapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrUnreadableAccountType reports that an account exists but the API did not
// say whether it is a user or an organization. Permanent rather than
// transient: unlike every other inconclusive answer, re-running asks the same
// server the same question and gets the same silence.
var ErrUnreadableAccountType = errors.New("the API did not report an account type")

// Inconclusive is R-12's error: the lookup could not be answered
// definitively; the caller must report `unknown`, exit 5, and propose
// nothing.
type Inconclusive struct{ Reason string }

func (e *Inconclusive) Error() string { return "inconclusive: " + e.Reason }

// Cache stores definitive lookup results (R-15).
type Cache interface {
	Get(key string) ([]byte, bool)
	Set(key string, val []byte)
}

// MemCache is the per-run in-memory cache.
type MemCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

func NewMemCache() *MemCache { return &MemCache{m: map[string][]byte{}} }

func (c *MemCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *MemCache) Set(key string, val []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = val
}

// DiskCache persists across runs with a TTL (R-15).
type DiskCache struct {
	Dir string
	TTL time.Duration
	mem *MemCache
}

func NewDiskCache(dir string, ttl time.Duration) *DiskCache {
	os.MkdirAll(dir, 0o700)
	return &DiskCache{Dir: dir, TTL: ttl, mem: NewMemCache()}
}

type diskEntry struct {
	At  time.Time `json:"at"`
	Val []byte    `json:"val"`
}

func (c *DiskCache) file(key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(c.Dir, hex.EncodeToString(h[:])+".json")
}

func (c *DiskCache) Get(key string) ([]byte, bool) {
	if v, ok := c.mem.Get(key); ok {
		return v, true
	}
	b, err := os.ReadFile(c.file(key))
	if err != nil {
		return nil, false
	}
	var e diskEntry
	if json.Unmarshal(b, &e) != nil || time.Since(e.At) > c.TTL {
		return nil, false
	}
	c.mem.Set(key, e.Val)
	return e.Val, true
}

func (c *DiskCache) Set(key string, val []byte) {
	c.mem.Set(key, val)
	b, _ := json.Marshal(diskEntry{At: time.Now(), Val: val})
	_ = os.WriteFile(c.file(key), b, 0o600)
}

// Client is the API client.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	cache   Cache
	// scope prefixes every cache key with a fingerprint of (BaseURL, token)
	// so a cache directory shared across hosts or tokens can never serve one
	// context's "definitive" answers to another — a probe cached from a
	// privileged token would otherwise let an unprivileged token treat 404s
	// as true negatives (found in review).
	scope string
}

// New builds a client. baseURL "" means https://api.github.com.
func New(baseURL, token string, cache Cache) *Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if cache == nil {
		cache = NewMemCache()
	}
	fp := sha256.Sum256([]byte(baseURL + "\x00" + token))
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			// Never follow redirects: GitHub uses 302 on the org-membership
			// endpoint to mean "requester cannot see this membership", and
			// following it to /public_members turns a concealed member into a
			// definitive-looking 404 (found in review). All 3xx are
			// classified inconclusive in get().
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		cache: cache,
		scope: hex.EncodeToString(fp[:8]),
	}
}

func (c *Client) key(k string) string { return c.scope + ":" + k }

// rawUserinfo returns the userinfo of a URL-shaped string — everything
// between "://" and the first "@" — or "" if there is none.
//
// Deliberately its own scanner rather than url.Parse: the strings that MUST be
// redacted are exactly the ones url.Parse rejects, and a space inside the
// userinfo is the commonest way to produce one. It is also why the first
// attempt at this leaked: a regex that excluded whitespace from userinfo
// missed every input it was written for.
//
// The "/" guard is what keeps it from claiming an "@" that belongs to a path
// (`https://host/a@b`): userinfo cannot span a slash.
func rawUserinfo(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return ""
	}
	rest := raw[i+3:]
	at := strings.IndexByte(rest, '@')
	if at < 0 {
		return ""
	}
	if slash := strings.IndexByte(rest, '/'); slash >= 0 && slash < at {
		return ""
	}
	return rest[:at]
}

// redactURL strips userinfo before a base URL reaches a message.
//
// `--api-url https://svc:hunter2@ghes.example/api/v3` is a legal thing to type,
// and the "base URL looks wrong" path is the single most likely error a GHES
// operator hits — so the credential landed in stderr, and from there into a CI
// log (CWE-532).
//
// UNPARSEABLE input is redacted too. The first version returned it as-is on
// the reasoning that a string url.Parse rejects "is not a URL, so it has no
// userinfo to expose", which is false in the one case that matters: an
// --api-url with a space in it is rejected FOR the space and still carries the
// password into the parse error that renders it. Everything but the credential
// is preserved, because the operator needs the rest of the string to see their
// typo.
func redactURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		if u.User == nil {
			return raw
		}
		u.User = url.User("REDACTED")
		return u.String()
	}
	info := rawUserinfo(raw)
	if info == "" {
		return raw
	}
	return strings.Replace(raw, info+"@", "REDACTED@", 1)
}

// redact removes this client's secrets from a message on its way to stderr.
//
// Every earlier attempt redacted at ONE site and was defeated somewhere else:
// url.Error renders its URL with %q, so a base URL holding a control character
// no longer matches the raw string a caller was substituting; and the
// transport branch was never redacted at all, so the canonical
// `https://<PAT>@host` spelling — which parses perfectly, and whose PAT is a
// USERNAME, which net/http does not mask — reached the log on any connection
// failure. So redaction happens here, on the way out, keyed on the secret
// rather than on the shape of the message.
//
// The token is only substituted when it is long enough to be a real
// credential: `--token t` in a test would otherwise redact every "t" in the
// sentence, which is both useless and a way to hide the message's meaning.
func (c *Client) redact(s string) string {
	if info := rawUserinfo(c.BaseURL); info != "" {
		s = strings.ReplaceAll(s, info, "REDACTED")
	}
	if len(c.Token) >= 8 {
		s = strings.ReplaceAll(s, c.Token, "REDACTED")
	}
	return s
}

// pathSeg encodes one value as exactly one URL path segment.
//
// `@org/..` is a legal owner, so TeamExists gets the slug ".." and
// GET /orgs/org/teams/.. normalizes to GET /orgs/org — 200, and a nonexistent
// team reads as valid. url.PathEscape leaves "." and ".." alone, hence the
// dot-only case below.
func pathSeg(s string) string {
	e := url.PathEscape(s)
	if e != "" && strings.Trim(e, ".") == "" {
		return strings.ReplaceAll(e, ".", "%2E")
	}
	return e
}

// get performs a GET, classifying failures per R-12. Definitive statuses are
// 2xx and 404; everything else is inconclusive.
func (c *Client) get(path, accept string) (int, []byte, error) {
	req, err := http.NewRequest("GET", c.BaseURL+path, nil)
	if err != nil {
		// url.Parse's error quotes the WHOLE input, userinfo included, and
		// this is the one branch that renders it raw: an --api-url the parser
		// rejects never reaches the transport, whose own error strings mask
		// the password for us. `--api-url "ht tp://svc:hunter2@ghes/api/v3"`
		// put the credential in stderr and from there into a CI log
		// (CWE-532), which is exactly what redactURL exists to stop
		// everywhere else.
		return 0, nil, &Inconclusive{Reason: c.redact(err.Error())}
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, &Inconclusive{Reason: c.redact("network: " + err.Error())}
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if rerr != nil {
		return 0, nil, &Inconclusive{Reason: "truncated response body on " + path + ": " + rerr.Error()}
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, body, nil
	case resp.StatusCode == 404:
		return 404, body, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Redirects are semantic on some endpoints (302 on org membership =
		// "requester cannot see this") and never a definitive negative.
		reason := fmt.Sprintf("HTTP %d redirect on %s — the token cannot answer this definitively (e.g. concealed org membership)", resp.StatusCode, path)
		if resp.StatusCode == 301 {
			reason = fmt.Sprintf("HTTP 301 on %s — the resource may have been renamed (Location: %s); update the reference and re-run", path, resp.Header.Get("Location"))
		}
		return resp.StatusCode, nil, &Inconclusive{Reason: reason}
	case resp.StatusCode == 403 && resp.Header.Get("X-RateLimit-Remaining") == "0",
		resp.StatusCode == 429:
		return resp.StatusCode, nil, &Inconclusive{Reason: fmt.Sprintf("rate limited on %s", path)}
	case resp.StatusCode == 401, resp.StatusCode == 403:
		return resp.StatusCode, nil, &Inconclusive{Reason: fmt.Sprintf("HTTP %d on %s — token invalid or missing a required scope", resp.StatusCode, path)}
	default:
		return resp.StatusCode, nil, &Inconclusive{Reason: fmt.Sprintf("HTTP %d on %s", resp.StatusCode, path)}
	}
}

// cachedBool wraps a definitive boolean lookup with caching. Inconclusive
// results are never cached (R-12).
func (c *Client) cachedBool(key string, fetch func() (bool, error)) (bool, error) {
	if v, ok := c.cache.Get(c.key(key)); ok {
		return string(v) == "1", nil
	}
	val, err := fetch()
	if err != nil {
		return false, err
	}
	b := []byte("0")
	if val {
		b = []byte("1")
	}
	c.cache.Set(c.key(key), b)
	return val, nil
}

// ProbeOrg proves the token can enumerate the org's teams and members. Until
// this succeeds, org-scoped 404s must not be treated as negatives (R-12).
func (c *Client) ProbeOrg(org string) error {
	if _, ok := c.cache.Get(c.key("probe:" + org)); ok {
		return nil
	}
	for _, p := range []string{"/orgs/" + pathSeg(org) + "/teams?per_page=1", "/orgs/" + pathSeg(org) + "/members?per_page=1"} {
		status, _, err := c.get(p, "")
		if err != nil {
			return err
		}
		if status == 404 {
			return &Inconclusive{Reason: fmt.Sprintf("org %q not visible to this token (404 on %s)", org, p)}
		}
	}
	c.cache.Set(c.key("probe:"+org), []byte("1"))
	return nil
}

// ProbeRepo proves the token can read the repo; precondition for treating
// collaborator-permission 404s as negatives.
func (c *Client) ProbeRepo(owner, repo string) error {
	if _, ok := c.cache.Get(c.key("probe-repo:" + owner + "/" + repo)); ok {
		return nil
	}
	status, _, err := c.get("/repos/"+pathSeg(owner)+"/"+pathSeg(repo), "")
	if err != nil {
		return err
	}
	if status == 404 {
		return &Inconclusive{Reason: fmt.Sprintf("repo %s/%s not visible to this token", owner, repo)}
	}
	c.cache.Set(c.key("probe-repo:"+owner+"/"+repo), []byte("1"))
	return nil
}

// ProbeAPI proves the base URL and token actually reach a GitHub API at all,
// via GET /user (200 for any valid token). Without this, a mistyped GHES
// --api-url (e.g. missing /api/v3) 404s on EVERY endpoint and would mark
// every user owner dead — the mass false-negative R-12 exists to prevent
// (second-review finding).
func (c *Client) ProbeAPI() error {
	if _, ok := c.cache.Get(c.key("probe-api")); ok {
		return nil
	}
	status, _, err := c.get("/user", "")
	if err != nil {
		return err
	}
	if status == 404 {
		return &Inconclusive{Reason: fmt.Sprintf(
			"GET %s/user returned 404 — the API base URL looks wrong (GHES needs /api/v3)", redactURL(c.BaseURL))}
	}
	c.cache.Set(c.key("probe-api"), []byte("1"))
	return nil
}

// UserExists: A-1 for @username owners. User visibility is not org-scoped, so
// a 404 with a working token is definitive — but only after ProbeAPI has
// proven that 404s from this base URL mean "not found" rather than "not a
// GitHub API".
func (c *Client) UserExists(login string) (bool, error) {
	return c.cachedBool("user:"+login, func() (bool, error) {
		status, body, err := c.get("/users/"+pathSeg(login), "")
		if err != nil {
			return false, err
		}
		if status == 404 {
			if err := c.ProbeAPI(); err != nil {
				return false, err
			}
			return false, nil
		}
		// The same response answers AccountIsOrganization, and R-41 asks both
		// questions about every bare handle it writes. Filing the answer here
		// makes that one request instead of two identical ones; the lookup
		// below still stands on its own for any caller that skips this.
		var out struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(body, &out) == nil && out.Type != "" {
			b := []byte("0")
			if out.Type == "Organization" {
				b = []byte("1")
			}
			c.cache.Set(c.key("account-is-org:"+login), b)
		}
		return true, nil
	})
}

// AccountIsOrganization reports whether a bare @handle names an ORGANIZATION
// rather than a user. Callers must establish that the account exists first.
//
// It exists for a gap only a caller that PROVES something has to close.
// `@acme` is a syntactically valid owner token and `GET /users/acme` answers
// 200 for an organization, so "this account exists" is true and useless:
// GitHub's CODEOWNERS resolver accepts a user, an `@org/team` or an email
// address and nothing else, so an org handle on a rule is silently nobody —
// the same dead line R-41 exists to refuse, reached through the check itself.
//
// A response this cannot classify is inconclusive, never a negative (R-12).
func (c *Client) AccountIsOrganization(login string) (bool, error) {
	return c.cachedBool("account-is-org:"+login, func() (bool, error) {
		status, body, err := c.get("/users/"+pathSeg(login), "")
		if err != nil {
			return false, err
		}
		if status == 404 {
			// NOT rendered as "not an organization". Callers reach this only
			// after proving the account exists, so a 404 now means the
			// precondition was skipped or the account vanished between two
			// requests — and "false" here is the write-it direction, which is
			// the one answer a bare 404 must never buy in this package (R-12).
			return false, &Inconclusive{Reason: fmt.Sprintf(
				"@%s was not found when its account type was read, though it existed a moment earlier", login)}
		}
		var out struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(body, &out) != nil || out.Type == "" {
			// Distinguishable from every other inconclusive answer, because
			// it is the only one re-running cannot settle: the server will
			// answer identically forever. A caller that fails closed on it
			// would refuse a correct policy permanently, so it is given its
			// own sentinel to recognise.
			return false, fmt.Errorf("%w for @%s", ErrUnreadableAccountType, login)
		}
		return out.Type == "Organization", nil
	})
}

// TeamExists: A-1 for @org/team owners. Callers must ProbeOrg(org) first.
func (c *Client) TeamExists(org, slug string) (bool, error) {
	return c.cachedBool("team:"+org+"/"+slug, func() (bool, error) {
		status, _, err := c.get("/orgs/"+pathSeg(org)+"/teams/"+pathSeg(slug), "")
		if err != nil {
			return false, err
		}
		return status != 404, nil
	})
}

// ViewerIsOrgAdmin reports whether the authenticated token is an OWNER of org.
//
// It exists for one question that only a destructive caller has to ask: does a
// team 404 mean "deleted", or "invisible to me"? GET /orgs/{org}/teams lists
// only the teams the caller can see, and GET /orgs/{org}/teams/{slug} 404s for
// a SECRET team the caller is not a member of — so ProbeOrg succeeding proves
// the token can enumerate the endpoint, not that it can see every team. An org
// member who is not an owner, a fine-grained token, or a GitHub App with a
// narrow grant all get a 404 that is indistinguishable at the HTTP layer from
// a team that no longer exists.
//
// Org owners see every team including secret ones, so this is the one cheap
// answer that makes a 404 definitive. `audit` does not need it — it reports,
// and a false "does not exist" finding costs a human one lookup. `lint` deletes,
// so it does.
//
// A 404 here means "no membership visible", which is itself a definitive no.
func (c *Client) ViewerIsOrgAdmin(org string) (bool, error) {
	return c.cachedBool("viewer-admin:"+org, func() (bool, error) {
		status, body, err := c.get("/user/memberships/orgs/"+pathSeg(org), "")
		if err != nil {
			return false, err
		}
		if status == 404 {
			return false, nil
		}
		var out struct {
			State string `json:"state"`
			Role  string `json:"role"`
		}
		if json.Unmarshal(body, &out) != nil {
			return false, &Inconclusive{Reason: "unparseable org membership response for " + org}
		}
		return out.State == "active" && out.Role == "admin", nil
	})
}

// UserIsOrgMember: A-2. Callers must ProbeOrg(org) first.
func (c *Client) UserIsOrgMember(org, login string) (bool, error) {
	return c.cachedBool("member:"+org+"/"+login, func() (bool, error) {
		status, _, err := c.get("/orgs/"+pathSeg(org)+"/members/"+pathSeg(login), "")
		if err != nil {
			return false, err
		}
		return status != 404, nil
	})
}

// UserHasWrite: A-3 for users — S-5: EXPLICIT write access; org membership
// is not enough. Callers must ProbeRepo first.
func (c *Client) UserHasWrite(owner, repo, login string) (bool, error) {
	return c.cachedBool("perm:"+owner+"/"+repo+":"+login, func() (bool, error) {
		status, body, err := c.get("/repos/"+pathSeg(owner)+"/"+pathSeg(repo)+"/collaborators/"+pathSeg(login)+"/permission", "")
		if err != nil {
			return false, err
		}
		if status == 404 {
			return false, nil // not a collaborator at all
		}
		var out struct {
			Permission string `json:"permission"`
			RoleName   string `json:"role_name"`
		}
		if json.Unmarshal(body, &out) != nil {
			return false, &Inconclusive{Reason: "unparseable permission response for " + login}
		}
		switch out.Permission {
		case "admin", "write":
			return true, nil
		}
		return out.RoleName == "maintain" || out.RoleName == "write" || out.RoleName == "admin", nil
	})
}

// TeamHasWrite: A-3 for teams — the team must have explicit push (or
// stronger) permission on the repo. Callers must ProbeOrg(org) first.
func (c *Client) TeamHasWrite(org, slug, owner, repo string) (bool, error) {
	return c.cachedBool("teamperm:"+org+"/"+slug+":"+owner+"/"+repo, func() (bool, error) {
		status, body, err := c.get(
			"/orgs/"+pathSeg(org)+"/teams/"+pathSeg(slug)+"/repos/"+pathSeg(owner)+"/"+pathSeg(repo),
			"application/vnd.github.v3.repository+json")
		if err != nil {
			return false, err
		}
		if status == 404 {
			return false, nil // team does not manage this repo
		}
		if status == 204 {
			// Manages the repo but the server didn't honor the media type;
			// permission level unknown.
			return false, &Inconclusive{Reason: fmt.Sprintf("team %s/%s manages %s/%s but permission level unavailable", org, slug, owner, repo)}
		}
		var out struct {
			Permissions struct {
				Admin    bool `json:"admin"`
				Maintain bool `json:"maintain"`
				Push     bool `json:"push"`
			} `json:"permissions"`
		}
		if json.Unmarshal(body, &out) != nil {
			return false, &Inconclusive{Reason: "unparseable team-repo response"}
		}
		return out.Permissions.Admin || out.Permissions.Maintain || out.Permissions.Push, nil
	})
}

// CodeownersErrors calls GET /repos/{owner}/{repo}/codeowners/errors (A-8
// remote validation). ref "" means the default branch.
func (c *Client) CodeownersErrors(owner, repo, ref string) ([]RemoteError, error) {
	p := "/repos/" + pathSeg(owner) + "/" + pathSeg(repo) + "/codeowners/errors"
	if ref != "" {
		// Raw interpolation let a ref containing `&` append parameters nobody wrote,
		// and one containing a space fail to parse as a URL at all.
		p += "?" + url.Values{"ref": {ref}}.Encode()
	}
	status, body, err := c.get(p, "")
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, &Inconclusive{Reason: "codeowners/errors endpoint returned 404 (repo invisible, or endpoint unavailable on this GHES version)"}
	}
	var out struct {
		Errors []RemoteError `json:"errors"`
	}
	if json.Unmarshal(body, &out) != nil {
		return nil, &Inconclusive{Reason: "unparseable codeowners/errors response"}
	}
	return out.Errors, nil
}

// RemoteError is one entry from the codeowners/errors API.
type RemoteError struct {
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Kind       string `json:"kind"`
	Message    string `json:"message"`
	Path       string `json:"path"`
	Source     string `json:"source,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}
