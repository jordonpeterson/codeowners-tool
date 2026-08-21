// Stale-comment reporting under R-38's owner identity.
//
// R-38a says one identity governs every comparison of two owners.
// commentLinesNaming was missed: it finds the old handle in a comment with a
// byte-exact substring search. Before R-38 that was harmless, because a rename
// of @org/team never touched a rule spelling @Org/Team — so a comment in the
// other case named an owner the run had not renamed. R-38 makes the rename land
// on both spellings, which turns that comment into the stale one this warning
// exists to find, and it goes unreported.
//
// Two pins here freeze behaviour the fix must NOT change: the token boundary
// that keeps a rename of @org/acq quiet about @org/acq-infra, and e-mail owners
// staying case-sensitive. A fix that folds too eagerly breaks both, so they are
// asserted in the same file as the widening.
//
// Every negative case asserts the run APPLIED as well as the absence of a
// warning: "no warning" is also what a crashed run produces, and this whole
// feature is stderr-only.
package cli_test

import (
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// ccRepo seeds a repo whose CODEOWNERS is exactly what the case needs. The
// tracked files are fixed so a scope of /x/ always matches something and the
// rename is never a no-op for R-5 reasons rather than the reason under test.
func ccRepo(t *testing.T, codeowners string) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"CODEOWNERS": codeowners,
		"x/a.go":     "package x\n",
		"top.md":     "top\n",
	})
}

func ccSync(t *testing.T, repo string, op string) (int, cli.SyncRecord, string) {
	t.Helper()
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--format", "json", "--op", op)
	if code != cli.ExitOK && code != cli.ExitRefused {
		return code, cli.SyncRecord{}, errOut
	}
	return code, syncDecodeRecord(t, out), errOut
}

// ccWantWarning fails unless stderr carries a stale-comment warning naming the
// given line and the spelling the COMMENT uses. Naming the op's spelling would
// send the reader looking for text the file does not contain.
func ccWantWarning(t *testing.T, errOut, line, spelling string) {
	t.Helper()
	var hit string
	for _, l := range strings.Split(errOut, "\n") {
		if strings.Contains(l, "comment still names") || strings.Contains(l, "but a comment still names it") {
			hit = l
			break
		}
	}
	if hit == "" {
		t.Fatalf("no stale-comment warning at all\nstderr:\n%s", errOut)
	}
	if !strings.Contains(hit, "line "+line) {
		t.Errorf("warning does not name line %s — the reader has to find the comment:\n%s", line, hit)
	}
	if !strings.Contains(hit, spelling) {
		t.Errorf("warning does not quote %q, the spelling the comment actually uses:\n%s", spelling, hit)
	}
}

func ccWantNoWarning(t *testing.T, errOut string) {
	t.Helper()
	for _, l := range strings.Split(errOut, "\n") {
		if strings.Contains(l, "comment still names") || strings.Contains(l, "but a comment still names it") {
			t.Errorf("reported a comment that is not stale:\n%s", l)
		}
	}
}

// SPEC R-38a: a comment naming the renamed owner is reported however either
// side spells the handle. The rename lands on the rule in any case, so the
// comment is stale in any case, and the warning is the only thing that will
// ever find it — comments are never rewritten.
func TestR38a_StaleCommentIsFoundWhateverTheCase(t *testing.T) {
	cases := map[string]struct {
		seed     string
		op       string
		spelling string
	}{
		"comment upper, rule and op lower": {
			seed:     "# owned by @Org/Team, ping them first\n/x/ @org/team\n",
			op:       "rename_owner(@org/team, @org/new)",
			spelling: "@Org/Team",
		},
		"comment lower, rule and op upper": {
			seed:     "# owned by @org/team, ping them first\n/x/ @ORG/TEAM\n",
			op:       "rename_owner(@ORG/TEAM, @org/new)",
			spelling: "@org/team",
		},
		"comment, rule and op all differ": {
			seed:     "# see @ORG/TEAM\n/x/ @Org/Team\n",
			op:       "rename_owner(@org/team, @org/new)",
			spelling: "@ORG/TEAM",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := ccRepo(t, tc.seed)
			code, rec, errOut := ccSync(t, repo, tc.op)
			if code != cli.ExitOK {
				t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
			}
			if rec.Status != cli.StatusApplied {
				t.Fatalf("status = %q, want %q — R-38 renames the rule whatever its case, so there was work to do",
					rec.Status, cli.StatusApplied)
			}
			ccWantWarning(t, errOut, "1", tc.spelling)
		})
	}
}

// SPEC R-38a: the same-case comment is the premise the widening rests on. If
// this ever stops warning, the tests above would pass by reporting nothing.
func TestR38a_PinStaleCommentInTheSameCaseStillWarns(t *testing.T) {
	repo := ccRepo(t, "# owned by @org/team, ping them first\n/x/ @org/team\n")
	code, rec, errOut := ccSync(t, repo, "rename_owner(@org/team, @org/new)")
	if code != cli.ExitOK || rec.Status != cli.StatusApplied {
		t.Fatalf("exit %d status %q\nstderr:\n%s", code, rec.Status, errOut)
	}
	ccWantWarning(t, errOut, "1", "@org/team")
}

// SPEC R-38a: folding must not widen the token boundary. A rename of @org/acq
// says nothing about a comment that only ever named @org/acq-infra — that is a
// different owner, and it stays a different owner in every case.
func TestR38a_PinFoldingDoesNotWidenTheTokenBoundary(t *testing.T) {
	for name, seed := range map[string]string{
		"comment names a longer handle":             "# ask @org/acq-infra\n/x/ @org/acq\n",
		"comment names a longer handle, mixed case": "# ask @Org/Acq-Infra\n/x/ @org/acq\n",
	} {
		t.Run(name, func(t *testing.T) {
			repo := ccRepo(t, seed)
			code, rec, errOut := ccSync(t, repo, "rename_owner(@org/acq, @org/new)")
			if code != cli.ExitOK {
				t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
			}
			if rec.Status != cli.StatusApplied {
				t.Fatalf("status = %q, want %q — the rule names @org/acq, so the rename applies",
					rec.Status, cli.StatusApplied)
			}
			ccWantNoWarning(t, errOut)
		})
	}
}

// SPEC R-38a: e-mail owners compare exactly, so a comment naming a differently
// cased address names a DIFFERENT owner and is not stale. This is the pin that
// fails if a fix folds everything rather than @handles.
func TestR38a_PinEmailCommentsStayCaseSensitive(t *testing.T) {
	repo := ccRepo(t, "# ask DEV@EXAMPLE.COM\n/x/ dev@example.com\n")
	code, rec, errOut := ccSync(t, repo, "rename_owner(dev@example.com, @org/new)")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	if rec.Status != cli.StatusApplied {
		t.Fatalf("status = %q, want %q", rec.Status, cli.StatusApplied)
	}
	ccWantNoWarning(t, errOut)
}

// SPEC R-38a: the repo where the old handle survives ONLY in a comment is
// exactly the repo this warning exists for — the rename correctly changes
// nothing and the record reads unchanged. Case must not decide whether that
// last trace is found.
func TestR38a_CommentOnlySurvivorIsFoundAcrossCase(t *testing.T) {
	repo := ccRepo(t, "# retired: @Org/Gone\n/x/ @org/keep\n")
	code, rec, errOut := ccSync(t, repo, "rename_owner(@org/gone, @org/new)")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	if rec.Status != cli.StatusUnchanged {
		t.Fatalf("status = %q, want %q — no rule names the owner, so the rename changes nothing",
			rec.Status, cli.StatusUnchanged)
	}
	ccWantWarning(t, errOut, "1", "@Org/Gone")
}

// SPEC R-38a: case-insensitive matching must not be done by folding the whole
// line and indexing into the result — Unicode lowering can change a string's
// byte length, which would misplace the token-boundary check and the reported
// line. The comment here carries multi-byte text on both sides of the handle.
func TestR38a_MultiByteCommentTextDoesNotDisturbTheReport(t *testing.T) {
	repo := ccRepo(t, "/x/ @org/team\n# İstanbul café — ask @Org/Team — ünicode\n")
	code, rec, errOut := ccSync(t, repo, "rename_owner(@org/team, @org/new)")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	if rec.Status != cli.StatusApplied {
		t.Fatalf("status = %q, want %q", rec.Status, cli.StatusApplied)
	}
	ccWantWarning(t, errOut, "2", "@Org/Team")
}
