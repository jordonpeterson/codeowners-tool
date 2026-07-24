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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

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
}

// New builds a client. baseURL "" means https://api.github.com.
func New(baseURL, token string, cache Cache) *Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if cache == nil {
		cache = NewMemCache()
	}
	return &Client{BaseURL: baseURL, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}, cache: cache}
}

// get performs a GET, classifying failures per R-12. Definitive statuses are
// 2xx and 404; everything else is inconclusive.
func (c *Client) get(path, accept string) (int, []byte, error) {
	req, err := http.NewRequest("GET", c.BaseURL+path, nil)
	if err != nil {
		return 0, nil, &Inconclusive{Reason: err.Error()}
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, &Inconclusive{Reason: "network: " + err.Error()}
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 1024)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, body, nil
	case resp.StatusCode == 404:
		return 404, body, nil
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
	if v, ok := c.cache.Get(key); ok {
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
	c.cache.Set(key, b)
	return val, nil
}

// ProbeOrg proves the token can enumerate the org's teams and members. Until
// this succeeds, org-scoped 404s must not be treated as negatives (R-12).
func (c *Client) ProbeOrg(org string) error {
	if _, ok := c.cache.Get("probe:" + org); ok {
		return nil
	}
	for _, p := range []string{"/orgs/" + org + "/teams?per_page=1", "/orgs/" + org + "/members?per_page=1"} {
		status, _, err := c.get(p, "")
		if err != nil {
			return err
		}
		if status == 404 {
			return &Inconclusive{Reason: fmt.Sprintf("org %q not visible to this token (404 on %s)", org, p)}
		}
	}
	c.cache.Set("probe:"+org, []byte("1"))
	return nil
}

// ProbeRepo proves the token can read the repo; precondition for treating
// collaborator-permission 404s as negatives.
func (c *Client) ProbeRepo(owner, repo string) error {
	if _, ok := c.cache.Get("probe-repo:" + owner + "/" + repo); ok {
		return nil
	}
	status, _, err := c.get("/repos/"+owner+"/"+repo, "")
	if err != nil {
		return err
	}
	if status == 404 {
		return &Inconclusive{Reason: fmt.Sprintf("repo %s/%s not visible to this token", owner, repo)}
	}
	c.cache.Set("probe-repo:"+owner+"/"+repo, []byte("1"))
	return nil
}

// UserExists: A-1 for @username owners. User visibility is not org-scoped, so
// a 404 with a working token is definitive.
func (c *Client) UserExists(login string) (bool, error) {
	return c.cachedBool("user:"+login, func() (bool, error) {
		status, _, err := c.get("/users/"+login, "")
		if err != nil {
			return false, err
		}
		return status != 404, nil
	})
}

// TeamExists: A-1 for @org/team owners. Callers must ProbeOrg(org) first.
func (c *Client) TeamExists(org, slug string) (bool, error) {
	return c.cachedBool("team:"+org+"/"+slug, func() (bool, error) {
		status, _, err := c.get("/orgs/"+org+"/teams/"+slug, "")
		if err != nil {
			return false, err
		}
		return status != 404, nil
	})
}

// UserIsOrgMember: A-2. Callers must ProbeOrg(org) first.
func (c *Client) UserIsOrgMember(org, login string) (bool, error) {
	return c.cachedBool("member:"+org+"/"+login, func() (bool, error) {
		status, _, err := c.get("/orgs/"+org+"/members/"+login, "")
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
		status, body, err := c.get("/repos/"+owner+"/"+repo+"/collaborators/"+login+"/permission", "")
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
			"/orgs/"+org+"/teams/"+slug+"/repos/"+owner+"/"+repo,
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
	p := "/repos/" + owner + "/" + repo + "/codeowners/errors"
	if ref != "" {
		p += "?ref=" + ref
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
