// R-37's round trip, end to end: the `except` array is validated, applied and
// REPORTED as the `<scope> except <pat> …` string spelling it claims to be
// equivalent to (R-37a/R-37e), so anything an operator can see must be the same
// between the two.
//
// The tests here came out of an adversarial review that ran the two spellings
// side by side and diffed what each produced. Three of the four defects it
// found were invisible to a test that only asserted the array's own result:
// a pre-escaped element applied a carve to a subtree nobody named at exit 0,
// an array-spelled op echoed itself WITHOUT its carve in the R-8 remedy, and a
// pattern the op string cannot carry was refused with an arity error about an
// owner list the operator never wrote. So the last test in this file is the
// differential harness itself: same carve, both spellings, every visible
// artifact compared.
//
// Vacuity, per CONTRIBUTING.md: policy.Error prints the FILE first and
// t.TempDir embeds the test's own name in the path, so no fixture and no
// subtest below is named with a fragment any assertion looks for.
package cli_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// xrRepo is the reviewer's reproduction fixture: one directory whose name
// contains a space, and one whose name is the tail of it. The pair is the
// point — a pattern split at the space carves `dir/` instead of `my dir/`,
// and both exist here, so the wrong carve LOOKS like it worked.
func xrRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"CODEOWNERS":       "* @org/original_team\n",
		"my dir/x.txt":     "x\n",
		"dir/y.txt":        "y\n",
		"docs/keep.md":     "keep\n",
		"docs/my dir/f.md": "f\n",
		"docs/a.md":        "a\n",
		"docs/gen/g.go":    "package gen\n",
	})
}

func xrPolicy(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// SPEC R-37c: a pre-escaped array element is refused, and nothing is written.
//
// The reviewer's reproduction, verbatim. `"my\\ dir/"` in JSON is the pattern
// text `my\ dir/`; escaping it again for the file's grammar produces an escaped
// backslash followed by a BARE space, which the string grammar splits in two.
// Before the fix this repo came back at exit 0 with a carve line for `dir/` —
// a directory the policy never named — while `my dir/x.txt`, the path the carve
// existed for, resolved to the grantee. The refusal has to name the escaping,
// because the likely author is a generator that escaped defensively.
func TestR37c_PreEscapedArrayElementIsRefusedEndToEnd(t *testing.T) {
	const src = `{"version":1,"ops":[{"op":"add_owner(*, @org/team_a)","except":["my\\ dir/"],"on_except_zero_match":"allow"}]}`

	repo := xrRepo(t)
	pol := xrPolicy(t, src)
	snap := checkDirSnapshot(t, repo)
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "stderr", errOut, "already escaped")
	exceptWantFragment(t, "stderr", errOut, `"my dir/"`)
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("a policy error must write NOTHING; repo changed")
	}
	// The fabricated pattern must not appear anywhere: before the fix the
	// warning named `my\\`, a pattern nobody wrote.
	if strings.Contains(errOut, `my\\ matched`) || strings.Contains(errOut, "dir/ @org/original_team") {
		t.Errorf("stderr reports a pattern the operator never wrote:\n%s", errOut)
	}

	// `check` reaches the same verdict without opening a repo — the defect is
	// a fact about the artifact, so a fleet must learn it once, at repo 0.
	code, _, errOut = runCLI(t, "check", "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "check stderr", errOut, "already escaped")
}

// SPEC R-37c: the literal spelling of the same intent still carves what it
// names. The half that was already tested and the half that was not are
// asserted together, because that asymmetry is what let the defect through.
func TestR37c_LiteralArrayElementCarvesTheDirectoryItNames(t *testing.T) {
	const want = "* @org/original_team @org/team_a\n" +
		`my\ dir/ @org/original_team` + "\n"

	repo := xrRepo(t)
	pol := xrPolicy(t, `{"version":1,"ops":[{"op":"add_owner(*, @org/team_a)","except":["my dir/"]}]}`)
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFile(t, filepath.Join(repo, "CODEOWNERS"), want)
}

// SPEC R-8/R-37e: the remedy names the op that was REVIEWED, carve included.
//
// `ops.StaticConflict` quotes Op.Raw, and an array-spelled op stored the op
// string without its carve — so the exit-3 advice was to run
// `set_owners(*, [@org/every])` on its own first: an op that drops `except
// /src/` and displaces the owners of the subtree the reviewed op protected.
// The warning attached to that same sentence exists because an operator
// followed such advice on a security repo and stripped two teams from it.
func TestR8_ArraySpelledOpRemedyNamesTheOpThatWasReviewed(t *testing.T) {
	const src = `{"version":1,"ops":[{"op":"set_owners(*, [@org/every])","except":["/src/"]},` +
		`"add_owner(/.github/, @org/api)"]}`
	code, _, errOut := runCLI(t, "check", "--policy", xrPolicy(t, src))
	if code != cli.ExitInvalid {
		t.Fatalf("want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	exceptWantFragment(t, "stderr", errOut, "set_owners(* except /src/, [@org/every])")
	// The carve-less spelling must not appear at all. It is not a wording
	// preference: that string is an op the operator can paste into a shell,
	// and running it does something the reviewed policy never asked for.
	if strings.Contains(errOut, "set_owners(*, [@org/every])") {
		t.Errorf("the remedy quotes an op that drops the carve — running it displaces /src/'s owners:\n%s", errOut)
	}
}

// xrArtifacts is everything one sync run leaves behind for a human or a script
// to read.
type xrArtifacts struct {
	code    int
	stdout  string
	stderr  string
	file    string
	record  string
	summary string
}

// xrRun performs one sync against a fresh copy of the fixture and collects
// every artifact, with the two paths that legitimately differ between runs
// (the clone and the policy file) replaced by fixed text.
func xrRun(t *testing.T, src string) xrArtifacts {
	t.Helper()
	repo := xrRepo(t)
	pol := xrPolicy(t, src)
	dir := t.TempDir()
	recPath, sumPath := filepath.Join(dir, "rec.json"), filepath.Join(dir, "sum.md")

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--policy", pol,
		"--format", "json", "--out", recPath, "--summary-out", sumPath)

	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return string(b)
	}
	norm := func(s string) string {
		s = strings.ReplaceAll(s, repo, "<repo>")
		s = strings.ReplaceAll(s, pol, "<policy>")
		return s
	}
	file := ""
	if b, err := os.ReadFile(filepath.Join(repo, "CODEOWNERS")); err == nil {
		file = string(b)
	}
	return xrArtifacts{
		code:    code,
		stdout:  norm(stdout),
		stderr:  norm(stderr),
		file:    file,
		record:  norm(read(recPath)),
		summary: norm(read(sumPath)),
	}
}

// xrJSON escapes a pattern text for a JSON string literal. The two spellings
// differ in the pattern text they carry, not in how JSON quotes it, so the
// cases below hold the text a run actually sees and this puts it in the file.
func xrJSON(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }

// xrDiagnosis returns the part of a policy error after "is not valid: ", which
// is the sentence that tells the operator what is wrong. The two spellings
// legitimately differ around it — the array's error points at the "except"
// field and says so — but R-37a promises the DIAGNOSIS itself is the string
// spelling's own (the rule TestR37_ArrayDefectsRefuseLikeTheStringSpelling
// states one case at a time).
func xrDiagnosis(t *testing.T, stderr string) string {
	t.Helper()
	const marker = "is not valid: "
	i := strings.Index(stderr, marker)
	if i < 0 {
		t.Fatalf("policy error has no diagnosis:\n%s", stderr)
	}
	line := stderr[i+len(marker):]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	return line
}

// SPEC R-37a/R-37e: equivalent in every respect, checked by running both
// spellings and diffing everything visible — exit code, stdout, stderr, the
// written CODEOWNERS bytes, the --out record and the --summary-out body.
//
// This is the harness the adversarial review used, kept as a test because
// three of the four defects it found were differences no single-spelling
// assertion could see. The op string is NOT redacted here, unlike the
// per-requirement tests: R-37e's promise is that an array-spelled op renders
// the intent that was reviewed, so a difference there is the finding, not
// noise.
//
// Exit 3 is the one comparison that has to be narrower. A policy error names
// the FIELD it came from — file:line:col, and the array's own wrapper — so the
// two cannot be byte-equal without lying about where the defect is. What must
// match there is the diagnosis.
func TestR37e_BothSpellingsAreIndistinguishable(t *testing.T) {
	cases := []struct {
		name      string
		pats      []string // as the array carries them: literal paths
		clause    string   // as the op string carries them: the delimiter escaped
		policyErr bool
	}{
		{name: "plain directory", pats: []string{"/docs/gen/"}, clause: "/docs/gen/"},
		{name: "a space in the path", pats: []string{"/docs/my dir/"}, clause: `/docs/my\ dir/`},
		// A path whose name really does contain a backslash: even parity, so
		// the element is NOT pre-escaped, and the space it carries is escaped
		// for the file's grammar like any other. Neither spelling matches a
		// tracked file here, so both refuse this repo at exit 2 — identically.
		{name: "a literal backslash in the path", pats: []string{`/docs/my\\ dir/`}, clause: `/docs/my\\\ dir/`},
		{name: "a character class", pats: []string{"/docs/[ab].md"}, clause: "/docs/[ab].md"},
		{name: "a glob", pats: []string{"/docs/*.md"}, clause: "/docs/*.md"},
		{name: "two patterns", pats: []string{"/docs/gen/", "/docs/keep.md"}, clause: "/docs/gen/ /docs/keep.md"},
		{name: "a pattern matching no tracked file", pats: []string{"/docs/absent.md"}, clause: "/docs/absent.md"},
		// Refusals are part of "every respect" too.
		{name: "a pattern outside the scope", pats: []string{"/dir/y.txt"}, clause: "/dir/y.txt", policyErr: true},
		{name: "a pattern covering the scope", pats: []string{"/docs/"}, clause: "/docs/", policyErr: true},
		{name: "a duplicate pattern", pats: []string{"/docs/gen/", "/docs/gen/**"}, clause: "/docs/gen/ /docs/gen/**", policyErr: true},
		{name: "a dangling backslash", pats: []string{`/docs/gen\`}, clause: `/docs/gen\`, policyErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quoted := make([]string, len(tc.pats))
			for i, p := range tc.pats {
				quoted[i] = `"` + xrJSON(p) + `"`
			}
			array := xrRun(t, `{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/, @org/team_a)","except":[`+
				strings.Join(quoted, ",")+`]}]}`)
			str := xrRun(t, `{"version":1,"ops":[{"id":"g","op":"add_owner(/docs/ except `+
				xrJSON(tc.clause)+`, @org/team_a)"}]}`)

			if array.code != str.code {
				t.Fatalf("exit code differs: array %d, string %d\n array stderr:\n%s\n string stderr:\n%s",
					array.code, str.code, array.stderr, str.stderr)
			}
			if want := cli.ExitInvalid; tc.policyErr != (array.code == want) {
				t.Fatalf("want policy error %v, got exit %d\nstderr:\n%s", tc.policyErr, array.code, array.stderr)
			}
			if tc.policyErr {
				if got, want := xrDiagnosis(t, array.stderr), xrDiagnosis(t, str.stderr); !strings.HasPrefix(got, want) {
					t.Errorf("the two spellings diagnose the same defect differently\n array form:  %s\n string form: %s", got, want)
				}
				return
			}
			for _, part := range []struct {
				what             string
				fromArr, fromStr string
			}{
				{"stdout", array.stdout, str.stdout},
				{"stderr", array.stderr, str.stderr},
				{"CODEOWNERS bytes", array.file, str.file},
				{"--out record", array.record, str.record},
				{"--summary-out body", array.summary, str.summary},
			} {
				if part.fromArr != part.fromStr {
					t.Errorf("%s differs between spellings; R-37e promises identical plans, bytes and reporting\n array form:\n%s\n string form:\n%s",
						part.what, part.fromArr, part.fromStr)
				}
			}
		})
	}
}
