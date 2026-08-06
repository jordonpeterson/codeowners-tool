package ghapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/ghapi"
)

// Every request path in this package is built by string concatenation from
// values that came out of a CODEOWNERS file or off the command line.
//
// The owner token grammar permits a dot, so `@org/..` is a LEGAL owner as far as
// the parser is concerned, and it reaches TeamExists as the slug "..". The
// request then reads GET /orgs/org/teams/.. — and a path with a dot-dot segment
// is normalized, by Go's own ServeMux, by nginx, by any proxy in front of GHES,
// to GET /orgs/org. That endpoint answers 200 for any org the token can see, so
// the client concludes the team EXISTS and A-1 raises nothing: a CODEOWNERS line
// naming an owner that cannot possibly be granted review gets a clean audit.
//
// This is a wrong answer, not a compromise — the client never writes, and the
// token is the caller's own. But "the audit said it was fine" is the entire
// product, so a silently wrong definitive answer is the expensive kind of bug.
//
// recordingServer captures the exact request line the client emits, without a
// ServeMux in the way: a mux would normalize the very thing under test.
func recordingServer(t *testing.T, code int, body string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.RequestURI)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s, &seen
}

func TestURLEscaping_PathSegmentsCannotTraverse(t *testing.T) {
	cases := []struct {
		name string
		call func(c *ghapi.Client)
	}{
		{"team slug", func(c *ghapi.Client) { _, _ = c.TeamExists("org", "..") }},
		{"org name", func(c *ghapi.Client) { _, _ = c.TeamExists("..", "team") }},
		{"user login", func(c *ghapi.Client) { _, _ = c.UserExists("..") }},
		{"member login", func(c *ghapi.Client) { _, _ = c.UserIsOrgMember("org", "..") }},
		{"collaborator", func(c *ghapi.Client) { _, _ = c.UserHasWrite("o", "r", "..") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, seen := recordingServer(t, 200, `{}`)
			tc.call(ghapi.New(s.URL, "tok", ghapi.NewMemCache()))
			if len(*seen) == 0 {
				t.Fatalf("no request reached the server")
			}
			for _, uri := range *seen {
				for _, seg := range strings.Split(strings.SplitN(uri, "?", 2)[0], "/") {
					if seg == ".." || seg == "." {
						t.Errorf("request %q contains the traversable segment %q.\nAny normalizing proxy — or Go's own ServeMux — rewrites this to a shorter, EXISTING endpoint, which answers 200, and the client reports the owner as valid. Percent-encode path segments (url.PathEscape, plus the dot-only cases it leaves alone).", uri, seg)
					}
				}
			}
		})
	}
}

// A slug or login containing a slash must stay one segment, not become two.
func TestURLEscaping_PathSegmentsCannotAddSegments(t *testing.T) {
	s, seen := recordingServer(t, 404, `{}`)
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	_, _ = c.TeamExists("org", "team/../../../user")
	if len(*seen) == 0 {
		t.Fatalf("no request reached the server")
	}
	got := (*seen)[0]
	if strings.Contains(got, "team/") {
		t.Errorf("request %q let a slug expand into extra path segments; one slug must occupy exactly one segment", got)
	}
}

// --branch/ref is interpolated straight into the query string, so a ref
// containing & or = does not describe a ref — it appends parameters the caller
// never wrote, against an endpoint whose other parameters govern what is
// returned.
func TestURLEscaping_RefIsEscapedIntoTheQuery(t *testing.T) {
	s, seen := recordingServer(t, 200, `{"errors":[]}`)
	c := ghapi.New(s.URL, "tok", ghapi.NewMemCache())
	_, _ = c.CodeownersErrors("o", "r", "feature/a b&per_page=1")
	if len(*seen) == 0 {
		t.Fatalf("no request reached the server")
	}
	got := (*seen)[0]
	q := ""
	if i := strings.Index(got, "?"); i >= 0 {
		q = got[i+1:]
	}
	if strings.Count(q, "&") != 0 {
		t.Errorf("request %q: the ref smuggled an extra query parameter in; encode it with url.Values", got)
	}
	if strings.Contains(q, " ") {
		t.Errorf("request %q: an unescaped space in the query", got)
	}
	// And the ref must still arrive intact, or the escaping has simply broken the
	// feature it was protecting.
	r, err := http.NewRequest("GET", s.URL+got, nil)
	if err != nil {
		t.Fatalf("the client emitted a request line that will not parse: %q: %v", got, err)
	}
	if want, got := "feature/a b&per_page=1", r.URL.Query().Get("ref"); got != want {
		t.Errorf("ref round-tripped as %q, want %q", got, want)
	}
}
