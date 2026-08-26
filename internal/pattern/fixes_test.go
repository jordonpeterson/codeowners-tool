package pattern

import (
	"strings"
	"testing"
)

// SPEC: NeverMatches names exactly the patterns whose language is empty. It is
// the write-side guard behind an exit-3 refusal, so an over-broad answer would
// halt a rollout over a live pattern, and an under-broad one would let a rule
// that owns nothing forever be written and called proven.
func TestNeverMatchesNamesTheDeadFamilies(t *testing.T) {
	dead := []string{"/", "**/", "**/**", "**/**/x", "**/**/*", "**/**/**", "//", "//x", "a//b", "/a//b", "**//x"}
	for _, p := range dead {
		if !NeverMatches(p) {
			t.Errorf("NeverMatches(%q) = false; no repo-relative path can match it", p)
		}
	}
	// Neighbours the fix must not take with it. Each is alive, and the first
	// three are adjacent `**` spellings that differ only in NOT being leading.
	alive := []string{"x/**/**", "**/x/**", "foo/**/", "**/*.tf", "*", "**", "/docs/", "/docs/**", "**/*", "/a/*", "a/**/b", "/x/**/*", "?", "/a\\ b/"}
	for _, p := range alive {
		if NeverMatches(p) {
			t.Errorf("NeverMatches(%q) = true, but it matches real paths", p)
		}
	}
}

// SPEC: NeverMatches agrees with the matcher on a generated corpus — every
// pattern it calls dead has no witness, and every pattern it calls alive has
// one. The corpus is the cross product of segment shapes and candidate paths,
// so a divergence in either direction fails here rather than in a repository.
func TestNeverMatchesAgreesWithTheMatcher(t *testing.T) {
	segs := []string{"", "a", "b", "*", "**", "?", "b*"}
	var pats []string
	var build func(prefix []string, depth int)
	build = func(prefix []string, depth int) {
		if len(prefix) > 0 {
			body := strings.Join(prefix, "/")
			pats = append(pats, body, "/"+body, body+"/", "/"+body+"/")
		}
		if depth == 0 {
			return
		}
		for _, s := range segs {
			build(append(append([]string{}, prefix...), s), depth-1)
		}
	}
	build(nil, 4)

	// Candidate paths: every well-formed path of up to five segments over a
	// two-name alphabet. Five is one more segment than the longest pattern here
	// needs (four literals plus a trailing slash), so a live pattern always has
	// a witness in range.
	names := []string{"a", "b"}
	var paths []string
	var grow func(cur []string, depth int)
	grow = func(cur []string, depth int) {
		if len(cur) > 0 {
			paths = append(paths, strings.Join(cur, "/"))
		}
		if depth == 0 {
			return
		}
		for _, n := range names {
			grow(append(append([]string{}, cur...), n), depth-1)
		}
	}
	grow(nil, 5)

	checked := 0
	for _, ps := range pats {
		p, err := Compile(ps)
		if err != nil {
			continue
		}
		witness := ""
		for _, path := range paths {
			if p.Match(path) {
				witness = path
				break
			}
		}
		checked++
		if got := NeverMatches(ps); got != (witness == "") {
			t.Errorf("NeverMatches(%q) = %v, but the matcher %s", ps, got,
				map[bool]string{true: "matches " + witness, false: "matches nothing in the corpus"}[witness != ""])
		}
	}
	if checked < 1000 {
		t.Fatalf("corpus collapsed to %d patterns — the generator, not the property, is what passed", checked)
	}
}

// SPEC: Compile refuses a bare leading `#` for the reason it already refuses
// `\#` — the line would read back as a comment, so the rule is dead there and a
// mutation tool must never emit one (S-2/S-6).
func TestCompileRefusesLeadingHash(t *testing.T) {
	for _, p := range []string{"#hash/", "#", "#a/b"} {
		err := mustFail(t, p)
		if !strings.Contains(err, "comment") {
			t.Errorf("Compile(%q) error %q does not say the line reads back as a comment", p, err)
		}
	}
	// A `#` anywhere but the first byte is an ordinary literal to GitHub and
	// stays one here.
	for _, p := range []string{"/a#b/", "a/#b"} {
		if _, err := Compile(p); err != nil {
			t.Errorf("Compile(%q) = %v; only the LEADING position is special", p, err)
		}
	}
}

func mustFail(t *testing.T, p string) string {
	t.Helper()
	if _, err := Compile(p); err != nil {
		return err.Error()
	}
	t.Fatalf("Compile(%q) succeeded; a line starting with '#' is a comment", p)
	return ""
}
