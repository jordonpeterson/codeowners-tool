// Findings in the op/pattern layer: rules the tool writes or refuses where the
// outcome, or the explanation for it, does not survive contact with a real
// tree. Nothing here is a matcher bug — 300k differential cases against the
// vendored oracle and 1.2M Contains pairs came back clean, and no INV-2
// violation got past the prover for a tracked path. These are the edges around
// it.
//
// Each test is a FAILING repro of a confirmed defect, written first per
// CONTRIBUTING.md.
package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// FINDING: `on_zero_match: declare` writes patterns that can never match
// anything in any repo, and reports them `applied (proven: structural)`.
//
//	$ codeowners-tool check --policy p.json      # {"op":"add_owner(**/, @org/security)","on_zero_match":"declare"}
//	ok: p.json — 1 op(s), no policy errors
//	$ codeowners-tool sync --policy p.json
//	applied: 1 op(s) applied, 0 skipped; 1 line change(s), 0 path(s) change owners
//	  ops[0]  applied (proven: structural)
//	$ cat CODEOWNERS
//	*  @org/everyone
//	**/ @org/security
//
// The codebase already knows this pattern is dead by construction:
// contains.go says `**/` "compiles to `\A(?:.+/)?/.*\z` — no repo-relative
// path can ever match it", and pattern.go has `case pattern == "/": // "/"
// doesn't match anything`. So the guarantee `declare` trades down to —
// GUARANTEES.md: "When someone later adds a matching file, this rule takes
// it" — is not weaker here, it is void. R-5 exists to refuse "creating a dead
// rule"; declare bypasses it for exactly the patterns that are dead
// everywhere rather than merely dead here.
//
// The same policy WITHOUT declare is refused per repo at exit 2 ("this repo
// alone was refused") rather than exit 3 ("broken everywhere; fix it, don't
// retry"), so a fleet triaging on exit codes sends a human to all N repos for
// one policy typo — and `check`, "the cheapest way to catch a broken policy
// before repo #1", passes it.
func TestDeclareRefusesAPatternThatCanNeverMatch(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "*  @org/everyone\n",
		"src/a.go":   "",
		"a/b/c/z.md": "",
	})
	policy := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(policy, []byte(
		`{"version":1,"name":"baseline","ops":[{"op":"add_owner(**/, @org/security)","on_zero_match":"declare"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, _ := runCLI(t, "check", "--policy", policy); code == cli.ExitOK {
		t.Errorf("check passes a policy whose only op names a pattern no path can ever match;\n" +
			"check reads no repository, which is exactly the level this belongs at")
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--policy", policy)
	out := stdout + stderr
	if code == cli.ExitOK {
		t.Errorf("declare wrote a rule that can never match anything and called it proven (exit 0)\noutput:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "CODEOWNERS")); strings.Contains(string(b), "**/ @org/security") {
		t.Errorf("the file now carries a line that looks authoritative and owns nothing, forever:\n%s", b)
	}

	// And without declare, the verdict is a per-repo refusal for a policy that
	// is broken in every repo.
	if err := os.WriteFile(policy, []byte(
		`{"version":1,"name":"baseline","ops":["add_owner(**/, @org/security)"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "sync", "--repo", repo, "--policy", policy, "--dry-run"); code == cli.ExitRefused {
		t.Errorf("a pattern that matches nothing in ANY repo is exit 2 (this repo needs a human) rather than\n"+
			"exit 3 (the policy is broken everywhere), so a fleet sends a human to all N repos\nstderr: %s", stderr)
	}
}

// FINDING: `set_owners` strands a pre-existing narrower rule as permanently
// dead and says nothing, turning a repo that audits clean into one that
// fails its own CI gate.
//
//	$ codeowners-tool audit            → audit clean (exit 0)
//	$ codeowners-tool sync --op 'set_owners(/docs/, [@org/new])'
//	applied: 1 op(s) applied, 0 skipped; 1 line change(s), 2 path(s) change owners
//	  ops[0]  applied (proven: tree)          ← no warning of any kind
//	$ cat CODEOWNERS
//	*         @org/everyone
//	/docs/x/  @org/x-team                     ← dead now; still names an owner
//	/docs/ @org/new
//	$ codeowners-tool audit
//	[A-6/warning] (line 2) rule "/docs/x/" is fully shadowed by line 3 … (S-1)   → exit 4
//
// The resolved ownership is right — those paths are in scope — so the
// invariants hold; the defect is the undisclosed rot the write authors.
// plan.go discloses this hazard only when the stranded rule's pattern is
// BYTE-EQUAL to the op's scope, and its own comment gives the reason the
// disclosure exists: "without this, the run creating the dead line is the one
// run that says nothing about it (pre-release finding)". A narrower pattern is
// exactly that case. Amending `/docs/x/` instead of stranding it would leave
// audit clean.
func TestSetOwnersDisclosesTheRuleItStrands(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":  "*         @org/everyone\n/docs/x/  @org/x-team\n",
		"docs/b.md":   "",
		"docs/x/f.md": "",
	})
	if code, _, _ := runCLI(t, "audit", "--repo", repo); code != cli.ExitOK {
		t.Fatal("fixture: this repo is supposed to audit clean before the change")
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--op", "set_owners(/docs/, [@org/new])")
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Fatalf("sync: exit %d: %s", code, out)
	}
	if !strings.Contains(out, "/docs/x/") {
		t.Errorf("the run stranded \"/docs/x/\" as a rule that can never take effect and never named it;\n"+
			"audit reports it as A-6 on the very next run, so this repo goes from clean to exit 4\noutput:\n%s", out)
	}
}

// FINDING: a scope whose written line would start with `#` passes `check` and
// then fails at write time with a refusal that never names the cause.
//
//	$ codeowners-tool check --op 'add_owner(#hash/, @org/x)'
//	ok: ops — 1 op(s), no policy errors
//	$ codeowners-tool sync --dry-run --op 'add_owner(#hash/, @org/x)'
//	error: refusing: synthesized edits do not satisfy the invariants
//	  INV-1 (in-scope result wrong): #hash/a.md — want {…, @org/x}, would get {…}
//
// The real reason is that the synthesized line `#hash/ @org/x` reads back as a
// COMMENT, and it appears nowhere in the message. pattern.go rejects `!` and
// `\#` at compile time with the standard this case fails too — such a rule
// "would be dead there … a mutation tool must never accept or emit one" — so
// the bare leading `#` should be the same exit-3 refusal as its two siblings
// rather than an opaque invariant failure at write time. `declare` already
// gets this right: "the line written for scope \"#future/\" does not read back
// as a rule for that pattern (INV-6)". Nothing wrong is written; the cost is a
// debugging session.
func TestLeadingHashScopeIsRefusedAtCheckTime(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @org/everyone\n",
		"#hash/a.md": "",
	})

	if code, stdout, _ := runCLI(t, "check", "--op", "add_owner(#hash/, @org/x)"); code == cli.ExitOK {
		t.Errorf("check accepts a scope whose written line reads back as a comment;\n"+
			"`!` and `\\#` are both compile-time refusals for the same reason\ncheck said: %s", stdout)
	}

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--dry-run", "--op", "add_owner(#hash/, @org/x)")
	out := stdout + stderr
	if code == cli.ExitOK {
		t.Fatalf("expected a refusal, got exit 0:\n%s", out)
	}
	if !strings.Contains(out, "comment") {
		t.Errorf("the refusal blames the invariants without saying the synthesized line is read back as a comment\noutput:\n%s", out)
	}
}

// FINDING: a tracked path containing a comma is unreachable by every op
// spelling, and the policy-file spelling blames the operator for a mistake
// they did not make.
//
//	$ … --op 'add_owner(/a,b/, @org/x)'
//	error: add_owner takes (scope, owner) or (scope, [owners]), got 3 args
//	$ … --op 'add_owner(/a\,b/, @org/x)'
//	error: scope "/a\\" ends with a dangling backslash — it would silently match a different path
//	$ … policy {"op":"add_owner(/a,b/)","owners":["@org/x"]}
//	error: ops[0]: this op names owners in its op string AND in an "owners" array;
//	       one intent, one place (R-39b) — keep either the `(scope, [owners])` spelling or the array
//
// The op string names no owners at all: splitArgs splits on top-level commas
// with no escape awareness, so `b/` became the second "owner". A comma is a
// legal git path byte and `[`/`]` are legal too (S-2 makes them literal, and
// the matcher handles them), so this is a real path a monorepo can hold. The
// escape README documents is for spaces only, and no spelling reaches such a
// path. Nothing is written wrongly; the cost is that the last message sends
// the reader looking for an owners array they did not write.
func TestCommaInScopeIsReachableOrHonestlyRefused(t *testing.T) {
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @org/everyone\n",
		"a,b/f.md":   "",
	})

	code, stdout, stderr := runCLI(t, "sync", "--repo", repo, "--dry-run", "--op", `add_owner(/a\,b/, @org/x)`)
	out := stdout + stderr
	if code != cli.ExitOK {
		t.Errorf("a backslash-escaped comma is the natural spelling for a path holding one, and it is not accepted:\n%s", out)
	}

	policy := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(policy, []byte(
		`{"version":1,"ops":[{"op":"add_owner(/a,b/)","owners":["@org/x"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stdout, stderr = runCLI(t, "check", "--policy", policy)
	if out := stdout + stderr; strings.Contains(out, "R-39b") {
		t.Errorf("the op string names NO owners, and the error accuses it of naming them twice;\n"+
			"the comma in the path was split off as a second argument\noutput:\n%s", out)
	}
}

// FINDING: a pattern and a path that differ only in Unicode normalization get
// the generic "may be deliberate" A-4, while the exactly analogous case
// mismatch gets A-5 and a fix.
//
// A monorepo touched by both macOS and Linux carries NFD and NFC spellings of
// the same name. CODEOWNERS matches bytes, so the NFC pattern is dead against
// the NFD path — correctly — but every string the reader can see is
// identical:
//
//	[A-4/warning] (line 2) pattern "/docs/réunion/" matches zero tracked files (report-only: may be deliberate, R-11)
//
// A-5 exists precisely to diagnose the other invisible mismatch: "matches zero
// files ONLY because of case — CODEOWNERS is case-sensitive (S-6); correct the
// pattern to the tree's actual casing". Normalization is not mentioned
// anywhere in the code or the docs, so the reader is left staring at two
// identical-looking strings and concluding the tool is broken.
func TestAuditDiagnosesAUnicodeNormalizationMismatch(t *testing.T) {
	const nfd = "docs/réunion/notes.md" // e + combining acute, as macOS stores it
	const nfc = "/docs/réunion/"         // precomposed é, as the pattern is typed
	repo := initRepo(t, map[string]string{
		"CODEOWNERS": "* @org/everyone\n" + nfc + " @org/docs-team\n",
		nfd:          "",
	})

	_, stdout, stderr := runCLI(t, "audit", "--repo", repo)
	out := strings.ToLower(stdout + stderr)
	if !strings.Contains(out, "normaliz") && !strings.Contains(out, "unicode") {
		t.Errorf("audit reports only the generic \"matches zero tracked files (may be deliberate)\" for a pattern\n"+
			"that is byte-different but visually identical to a tracked path; A-5 diagnoses the analogous\n"+
			"case mismatch by name\noutput:\n%s%s", stdout, stderr)
	}
}
