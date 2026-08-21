// Policy-field end-to-end tests: `create` in the policy (R-34) and the
// `defaults` block (R-35), from docs/policy.md. Written ahead of the
// implementation per CONTRIBUTING.md.
//
// The vacuity trap in this area is unusually sharp. Today `create` and
// `defaults` are simply unknown top-level fields, so EVERY policy in this
// file — the positive ones included — dies at exit 3, the same code most of
// the negative cases expect. An exit-code-only assertion would therefore be
// satisfied by a binary that has never heard of either field. So every
// negative case asserts a message fragment too, and pfWantExit3 refuses to
// run a fragment that also occurs in the policy PATH: `policy.Error` renders
// the filename first and `t.TempDir()` embeds the test's own name in it, so
// `strings.Contains(stderr, "create")` passes on a message that says only
// "unknown field".
//
// The fragments themselves are picked against today's text, not for
// readability. Today's rejection of `{"defaults": {...}}` reads:
//
//	unknown field "defaults"; the top level of a policy accepts "version",
//	"name", "description", "on_empty", "max_paths_changed", "ops"
//
// which already contains "defaults" AND "on_empty" — the two obvious
// fragments for R-35c — for entirely the wrong reason. What it cannot
// contain is the field set the defaults block accepts, so
// `"on_except_zero_match"` is the discriminator used throughout.
//
// Mutating tests assert EXACT file bytes, never line existence: under
// last-match-wins (S-1) a substring check is satisfied by a file whose line
// ORDER resolves the path to the wrong owners, and `create` is precisely the
// feature that writes a file from nothing where line order is the only thing
// there is.
//
// Three subtests pass against today's binary by design and are labeled as
// pins: the two subtests of TestR34c_MissingFileRefusesWithoutCreate whose
// policy does not mention `create`, and "flag alone still writes" in
// TestR34b_CreateFlagWithPolicyIsExit3. They freeze the behavior R-34c and
// R-34b promise to preserve — a missing file is exit 2 and `--create` still
// works on its own. Everything else fails until the feature lands.
package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
)

// pfRepoNoFile is the R-34 fixture: real content, no CODEOWNERS at any of the
// three locations S-8 reads.
func pfRepoNoFile(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"docs/guide.md": "# guide\n",
		"src/main.go":   "package main\n",
	})
}

// pfRepoWithFile is the R-35 fixture: a root CODEOWNERS with a catch-all, a
// /docs/ that exists, and no /deploy/ or /charts/ — so one op in a policy
// matches the tree and the others match nothing, which is the only situation
// on_zero_match has an opinion about.
func pfRepoWithFile(t *testing.T) string {
	t.Helper()
	return initRepo(t, map[string]string{
		"CODEOWNERS":    "# owners\n* @org/all\n",
		"docs/guide.md": "# guide\n",
		"src/main.go":   "package main\n",
	})
}

func pfWantFile(t *testing.T, path, want string) {
	t.Helper()
	if got := syncReadFile(t, path); got != want {
		t.Errorf("file bytes:\n%s\nwant:\n%s", got, want)
	}
}

func pfWantFragment(t *testing.T, where, got, fragment string) {
	t.Helper()
	if !strings.Contains(got, fragment) {
		t.Errorf("%s must contain %q — the operator has to be able to tell this refusal from every other one.\ngot:\n%s",
			where, fragment, got)
	}
}

// pfWantNoFile proves no CODEOWNERS appeared at any location GitHub reads,
// and that no `.github/` directory was created for a file never written.
func pfWantNoFile(t *testing.T, repo string) {
	t.Helper()
	for _, rel := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(rel))); err == nil {
			t.Errorf("a CODEOWNERS appeared at %s", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".github")); err == nil {
		t.Error("a .github/ directory was created for a file that was never written")
	}
}

// pfRawRecord decodes a sync record WITHOUT the typed struct, for the one
// assertion cli.SyncRecord cannot express: that `codeowners_path` is ABSENT
// rather than empty, which is what the documented `jq -r '.codeowners_path //
// empty'` guard reads.
func pfRawRecord(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("record is not valid JSON: %v\nraw:\n%s", err, out)
	}
	return m
}

// pfWantExit3 is the exit-3 harness for this file: `check` refuses the policy
// before any repo exists, `sync` refuses it identically and writes nothing,
// and both messages carry every fragment.
//
// The guard on the policy path is the point. Every fragment must be absent
// from the path, because policy.Error prints the filename first and the path
// contains the test's own name — without this, a test named
// "...CreateMustBeABoolean" asserting "create" passes against a binary that
// only ever said "unknown field".
func pfWantExit3(t *testing.T, src string, fragments ...string) {
	t.Helper()
	pol := syncWritePolicy(t, src)
	for _, f := range fragments {
		if strings.Contains(pol, f) {
			t.Fatalf("fragment %q also occurs in the policy path %s: the assertion would pass on a message that says nothing — rename the test or pick a fragment only the message can carry",
				f, pol)
		}
	}
	code, _, errOut := runCLI(t, "check", "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("check: want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	for _, f := range fragments {
		pfWantFragment(t, "check stderr", errOut, f)
	}

	repo := pfRepoWithFile(t)
	snap := checkDirSnapshot(t, repo)
	code, _, errOut = runCLI(t, "sync", "--repo", repo, "--policy", pol)
	if code != cli.ExitInvalid {
		t.Fatalf("sync: want exit 3, got %d\nstderr:\n%s", code, errOut)
	}
	for _, f := range fragments {
		pfWantFragment(t, "sync stderr", errOut, f)
	}
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("a policy error must write NOTHING; repo changed")
	}
}

// SPEC R-34a: `"create": true` makes `sync` write a missing CODEOWNERS,
// identical in every respect to `--create` — so the oracle is the flag
// itself, run over a twin repo, compared byte for byte. Exact bytes are the
// whole assertion: a created file has no prior content, so its line ORDER is
// entirely this tool's invention and a substring check would accept a file
// whose rules resolve to the wrong owners. `check` must accept the policy
// too, or the fleet script's first line halts before repo 1.
func TestR34a_PolicyCreateWritesTheMissingFile(t *testing.T) {
	const op = "add_owner(/docs/, @org/docs-team)"
	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":["`+op+`"]}`)
	if code, _, errOut := runCLI(t, "check", "--policy", pol); code != cli.ExitOK {
		t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}

	repo := pfRepoNoFile(t)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	pfWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"), "/docs/ @org/docs-team\n")
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusApplied || !rec.Created {
		t.Errorf("status=%q created=%v, want applied/true", rec.Status, rec.Created)
	}
	if rec.CodeownersPath != ".github/CODEOWNERS" {
		t.Errorf("codeowners_path = %q, want .github/CODEOWNERS — the fleet loop stages what this field names", rec.CodeownersPath)
	}

	twin := pfRepoNoFile(t)
	if code, _, errOut := runCLI(t, "sync", "--repo", twin, "--op", op, "--create"); code != cli.ExitOK {
		t.Fatalf("flag twin: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	pfWantFile(t, filepath.Join(twin, ".github/CODEOWNERS"),
		syncReadFile(t, filepath.Join(repo, ".github/CODEOWNERS")))
}

// SPEC R-34a/R-19: the created file converges. Three runs of the same policy
// leave byte-identical content, and runs 2 and 3 report `unchanged` with
// created:false — the property that lets a nightly fleet job re-run a
// create-bearing policy forever. `created` reporting true on a run that wrote
// nothing would make every night's summary claim 100 new files.
func TestR34a_PolicyCreateIsIdempotent(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":["add_owner(/docs/, @org/docs-team)"]}`)
	repo := pfRepoNoFile(t)
	path := filepath.Join(repo, ".github/CODEOWNERS")

	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("run 1: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	if rec := syncDecodeRecord(t, out); !rec.Created {
		t.Errorf("run 1: created = false, want true")
	}
	first := syncReadFile(t, path)

	for run := 2; run <= 3; run++ {
		code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
		if code != cli.ExitOK {
			t.Fatalf("run %d: want exit 0, got %d\nstderr:\n%s", run, code, errOut)
		}
		rec := syncDecodeRecord(t, out)
		if rec.Status != cli.StatusUnchanged {
			t.Errorf("run %d: status = %q, want %q", run, rec.Status, cli.StatusUnchanged)
		}
		if rec.Created {
			t.Errorf("run %d: created = true on a run that wrote nothing", run)
		}
		if got := syncReadFile(t, path); got != first {
			t.Errorf("run %d changed bytes:\nfirst:\n%s\nnow:\n%s", run, first, got)
		}
	}
}

// SPEC R-34b: `--create` alongside `--policy` is exit 3, the rule already
// applied to `--on-empty` and `--max-paths-changed` — a flag must not
// silently override the reviewed artifact. The fragment mirrors the existing
// refusals verbatim ("--on-empty is not allowed with --policy: …"), because
// an operator meeting the third member of a family should recognize it.
//
// Both halves matter and both are asserted: the rejection does not depend on
// whether the policy happens to state `create` (the reviewed artifact is the
// authority either way), and it is decided before the repository is opened,
// so nothing is written — least of all the file the flag was asking for.
func TestR34b_CreateFlagWithPolicyIsExit3(t *testing.T) {
	for name, src := range map[string]string{
		"policy states it": `{"version":1,"create":true,"ops":["add_owner(/docs/, @org/docs-team)"]}`,
		"policy is silent": `{"version":1,"ops":["add_owner(/docs/, @org/docs-team)"]}`,
		"policy denies it": `{"version":1,"create":false,"ops":["add_owner(/docs/, @org/docs-team)"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			pol := syncWritePolicy(t, src)
			repo := pfRepoNoFile(t)
			snap := checkDirSnapshot(t, repo)
			code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--create")
			if code != cli.ExitInvalid {
				t.Fatalf("want exit 3, got %d\nstderr:\n%s", code, errOut)
			}
			pfWantFragment(t, "stderr", errOut, "--create is not allowed with --policy")
			pfWantNoFile(t, repo)
			if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
				t.Errorf("a policy error must write NOTHING; repo changed")
			}
		})
	}

	// Pin, passes today: R-34b bans the flag NEXT TO --policy, not the flag.
	// `--op … --create` is the per-invocation path and keeps working, so an
	// implementation that removes --create outright goes red here.
	t.Run("flag alone still writes", func(t *testing.T) {
		repo := pfRepoNoFile(t)
		if code, _, errOut := runCLI(t, "sync", "--repo", repo,
			"--op", "add_owner(/docs/, @org/docs-team)", "--create"); code != cli.ExitOK {
			t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		pfWantFile(t, filepath.Join(repo, ".github/CODEOWNERS"), "/docs/ @org/docs-team\n")
	})
}

// SPEC R-34b/R-23: once `--create` is exit 3 next to `--policy`, the R-23
// refusal must stop telling a policy run to "re-run with --create" — that is
// advice whose only outcome is exit 3, handed to an operator at the moment
// they are least able to tell a broken policy from an awkward repo. The
// answer for a policy run is `"create": true` in the artifact.
//
// Asserted as an absence, which is exactly what makes it non-vacuous here:
// today the string is present, so this cannot pass by accident on either
// half.
func TestR34b_PolicyRunNeverAdvisesTheRejectedFlag(t *testing.T) {
	for name, src := range map[string]string{
		"policy is silent": `{"version":1,"ops":["add_owner(/docs/, @org/docs-team)"]}`,
		"policy denies it": `{"version":1,"create":false,"ops":["add_owner(/docs/, @org/docs-team)"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			pol := syncWritePolicy(t, src)
			repo := pfRepoNoFile(t)
			code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
			if code != cli.ExitRefused {
				t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
			}
			if strings.Contains(errOut, "re-run with --create") {
				t.Errorf("the refusal sends a --policy run to a flag R-34b rejects at exit 3; it must name the policy field instead:\n%s", errOut)
			}
		})
	}
}

// SPEC R-34c: absent or false, a missing CODEOWNERS refuses exactly as today
// — exit 2 (this repo needs a human), never exit 3, because a fleet loop has
// to record it and step to the next clone. The `create: false` case is the
// one that matters for review: a policy that states the decision explicitly
// must behave identically to one that omits it, or "we turned it off" and
// "we forgot" are different rollouts.
func TestR34c_MissingFileRefusesWithoutCreate(t *testing.T) {
	// Pin, passes today: the flag path, unchanged by R-34.
	t.Run("pin: flag path", func(t *testing.T) {
		repo := pfRepoNoFile(t)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--op", "add_owner(/docs/, @org/docs-team)")
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
		}
		pfWantFragment(t, "stderr", errOut, "no CODEOWNERS file found")
		pfWantNoFile(t, repo)
	})
	// Pin, passes today: a policy that says nothing about create.
	t.Run("pin: policy is silent", func(t *testing.T) {
		pol := syncWritePolicy(t, `{"version":1,"ops":["add_owner(/docs/, @org/docs-team)"]}`)
		repo := pfRepoNoFile(t)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
		}
		pfWantFragment(t, "stderr", errOut, "no CODEOWNERS file found")
		pfWantNoFile(t, repo)
	})
	t.Run("policy denies it", func(t *testing.T) {
		pol := syncWritePolicy(t, `{"version":1,"create":false,"ops":["add_owner(/docs/, @org/docs-team)"]}`)
		if code, _, errOut := runCLI(t, "check", "--policy", pol); code != cli.ExitOK {
			t.Fatalf("check: want exit 0 — false is a legal value, not a defect: %d\nstderr:\n%s", code, errOut)
		}
		repo := pfRepoNoFile(t)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
		if code != cli.ExitRefused {
			t.Fatalf("want exit 2, got %d — a repo without a file is a fact about THAT repo; exit 3 would halt the fleet on roughly repo 3\nstderr:\n%s", code, errOut)
		}
		pfWantFragment(t, "stderr", errOut, "no CODEOWNERS file found")
		pfWantNoFile(t, repo)
	})
}

// SPEC R-34d: R-23 is unchanged — `create` never overwrites, so the field is
// safe to leave set for a fleet where some repos have a file and some do not.
// Exact bytes prove the existing file was AMENDED rather than regenerated:
// the comment survives, the catch-all keeps its position, and the narrowing
// rule lands after it (R-2).
//
// The second assertion is the one that would be missed. "Never overwrites" is
// also satisfied by writing a SECOND file at .github/CODEOWNERS, which leaves
// the original untouched and, under S-8, silently demotes it to a file GitHub
// never loads — the whole repo's ownership replaced by one op's worth of
// rules, with the old file still sitting there looking authoritative.
func TestR34d_PolicyCreateNeverOverwrites(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":["add_owner(/docs/, @org/docs-team)"]}`)
	repo := pfRepoWithFile(t)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	pfWantFile(t, filepath.Join(repo, "CODEOWNERS"),
		"# owners\n"+
			"* @org/all\n"+
			"/docs/ @org/all @org/docs-team\n")
	if _, err := os.Stat(filepath.Join(repo, ".github/CODEOWNERS")); err == nil {
		t.Error("create wrote a second CODEOWNERS at the higher-precedence location; under S-8 the repo's real file now governs nothing")
	}
	rec := syncDecodeRecord(t, out)
	if rec.Created {
		t.Error("created = true on a repo that already had a file")
	}
	if rec.CodeownersPath != "CODEOWNERS" {
		t.Errorf("codeowners_path = %q, want CODEOWNERS", rec.CodeownersPath)
	}
}

// SPEC R-34a/R-24: `create` is permission to create, not an instruction to.
// When every op skips there is nothing to write, so no file and no `.github/`
// directory appear and the record is `skipped` with no `codeowners_path` —
// indistinguishable, to the commit step, from a repo that was never touched.
//
// The failure this prevents is invisible in a fleet summary: an empty
// CODEOWNERS answers "which repos still need ownership?" with *yes, done*
// forever. Nothing here may synthesize a file to make a repo look covered.
func TestR34a_PolicyCreateWritesNothingWhenEveryOpSkips(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,"create":true,"ops":[
		{"id":"deploy","op":"add_owner(/deploy/, @org/sre)","on_zero_match":"skip"},
		{"id":"charts","op":"add_owner(/charts/, @org/platform)","on_zero_match":"skip"}]}`)
	repo := pfRepoNoFile(t)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d — this repo does not need a human, the policy simply has nothing to say about it\nstderr:\n%s", code, errOut)
	}
	rec := syncDecodeRecord(t, out)
	if rec.Status != cli.StatusSkipped {
		t.Errorf("status = %q, want %q — `unchanged` would assert the file already says what you asked, and there is no file", rec.Status, cli.StatusSkipped)
	}
	if rec.Created {
		t.Error("created must be false")
	}
	if v, present := pfRawRecord(t, out)["codeowners_path"]; present {
		t.Errorf("codeowners_path is present on a run that wrote nothing: %v — the documented `jq -r '.codeowners_path // empty'` guard then stages a path that does not exist", v)
	}
	pfWantNoFile(t, repo)
}

// SPEC R-34/R-20: `create` is a boolean, and a wrong type is a policy error
// like any other — a 40-op baseline whose `create` is the STRING "false"
// would otherwise turn a fleet-wide "off" into whatever a non-empty string
// coerces to. The fragment names the type, because "unknown field" (today's
// message) and "wrong type" send the operator to different edits.
func TestR34a_CreateMustBeABoolean(t *testing.T) {
	for name, val := range map[string]string{
		"quoted":  `"true"`,
		"numeric": `1`,
		"null":    `null`,
	} {
		t.Run(name, func(t *testing.T) {
			pfWantExit3(t, `{"version":1,"create":`+val+`,"ops":["add_owner(/docs/, @org/docs-team)"]}`,
				"must be a boolean")
		})
	}
}

// SPEC R-35a: a default reaches every op that does not state its own value,
// so a 40-op baseline is 40 strings rather than 40 objects. The oracle is
// FOLD EQUIVALENCE: the same policy with the value written out on every op
// must produce the same file, byte for byte, and the same per-op outcomes.
// Asserting only "the two ops skipped" would be satisfied by a default that
// reached the first op and by luck nothing else.
func TestR35a_DefaultReachesEveryOpThatStatesNoValue(t *testing.T) {
	const opDocs = "add_owner(/docs/, @org/docs-team)"
	const opDeploy = "add_owner(/deploy/, @org/sre)"
	const opCharts = "add_owner(/charts/, @org/platform)"

	folded := syncWritePolicy(t, `{"version":1,
		"defaults":{"on_zero_match":"skip"},
		"ops":["`+opDocs+`","`+opDeploy+`","`+opCharts+`"]}`)
	if code, _, errOut := runCLI(t, "check", "--policy", folded); code != cli.ExitOK {
		t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	repo := pfRepoWithFile(t)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", folded, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	// Only /docs/ exists here; the default disposes of the other two, and the
	// file it leaves is the one op the tree justified.
	want := "# owners\n" +
		"* @org/all\n" +
		"/docs/ @org/all @org/docs-team\n"
	pfWantFile(t, filepath.Join(repo, "CODEOWNERS"), want)
	rec := syncDecodeRecord(t, out)
	if rec.OpsApplied != 1 || rec.OpsSkipped != 2 {
		t.Errorf("ops_applied=%d ops_skipped=%d, want 1/2 — the default has to reach BOTH unmatched ops", rec.OpsApplied, rec.OpsSkipped)
	}

	unfolded := syncWritePolicy(t, `{"version":1,"ops":[
		{"op":"`+opDocs+`","on_zero_match":"skip"},
		{"op":"`+opDeploy+`","on_zero_match":"skip"},
		{"op":"`+opCharts+`","on_zero_match":"skip"}]}`)
	twin := pfRepoWithFile(t)
	code, out, errOut = runCLI(t, "sync", "--repo", twin, "--policy", unfolded, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("unfolded twin: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	pfWantFile(t, filepath.Join(twin, "CODEOWNERS"), want)
	if twinRec := syncDecodeRecord(t, out); twinRec.OpsApplied != rec.OpsApplied || twinRec.OpsSkipped != rec.OpsSkipped {
		t.Errorf("folded %d/%d vs written-out %d/%d applied/skipped — the block is shorthand, not a second behavior",
			rec.OpsApplied, rec.OpsSkipped, twinRec.OpsApplied, twinRec.OpsSkipped)
	}
}

// SPEC R-35b: a per-op value wins over the default. Both ops below match
// nothing in this repo, so the outcome is decided entirely by which value
// reached which op: the defaulted one declares its rule, the one that states
// `skip` writes nothing. Exact bytes are the assertion — a file carrying the
// charts rule, or missing the deploy rule, is a default applied to the wrong
// op, and either one resolves ownership differently for every file added
// under those directories later.
func TestR35b_PerOpValueWinsOverTheDefault(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,
		"defaults":{"on_zero_match":"declare"},
		"ops":[{"id":"deploy","op":"add_owner(/deploy/, @org/sre)"},
		       {"id":"charts","op":"add_owner(/charts/, @org/platform)","on_zero_match":"skip"}]}`)
	repo := pfRepoWithFile(t)
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	pfWantFile(t, filepath.Join(repo, "CODEOWNERS"),
		"# owners\n"+
			"* @org/all\n"+
			"/deploy/ @org/sre\n")
	rec := syncDecodeRecord(t, out)
	if got := fleetOpResult(t, rec, "deploy"); got.Status != "applied" || got.Proven != "structural" {
		t.Errorf("deploy: status=%q proven=%q, want applied/structural — the default reached it", got.Status, got.Proven)
	}
	if got := fleetOpResult(t, rec, "charts"); got.Status != "skipped" {
		t.Errorf("charts: status=%q, want skipped — its own value must win over the default", got.Status)
	}
}

// SPEC R-35b: `check` echoes the RESOLVED value, so review sees what will
// run. Without it the reviewer of a 40-op policy has to fold the defaults
// block in their head, which is the arithmetic the block was added to remove
// — and a misplaced default is invisible until a repo somewhere writes a
// declared rule nobody expected.
//
// The assertion is per-op, not "the output mentions declare somewhere": a
// renderer printing the defaults block verbatim and nothing else would
// satisfy that while telling the reviewer nothing about which op runs which
// way. Human output rather than JSON, because `check --format json` reports
// `ops` as a COUNT and R-35b must not be read as a demand to reshape it.
func TestR35b_CheckEchoesTheResolvedValue(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,
		"defaults":{"on_zero_match":"declare"},
		"ops":[{"id":"deploy","op":"add_owner(/deploy/, @org/sre)"},
		       {"id":"charts","op":"add_owner(/charts/, @org/platform)","on_zero_match":"skip"}]}`)
	// The path is printed by `check` itself and carries this test's name, so a
	// value that also occurred in it would make the assertion vacuous.
	for _, v := range []string{"declare", "skip"} {
		if strings.Contains(pol, v) {
			t.Fatalf("policy path %s contains %q; rename the test", pol, v)
		}
	}
	code, out, errOut := runCLI(t, "check", "--policy", pol)
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	for id, want := range map[string]string{"deploy": "declare", "charts": "skip"} {
		found := false
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, id) {
				continue
			}
			found = true
			if !strings.Contains(line, want) {
				t.Errorf("check line for op %q does not echo its resolved on_zero_match %q:\n%s", id, want, line)
			}
		}
		if !found {
			t.Errorf("check output names no op %q, so it cannot be echoing anything per op:\n%s", id, out)
		}
	}
}

// SPEC R-35a/R-28: the default carries `on_except_zero_match` too. This repo
// has no .github/CODEOWNERS, so the promised carve-out does not exist; under
// the block's `allow` the grant is written anyway, the inert pattern is
// disclosed, the op is marked proven:"structural", and the run warns.
//
// Exact bytes matter for the same reason they do under a per-op `allow`: an
// implementation that reads the default as "skip the op" while still
// reporting the unmatched pattern is the wall of silent green this knob must
// not become.
func TestR35a_DefaultReachesExceptOps(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,
		"defaults":{"on_except_zero_match":"allow"},
		"ops":[{"id":"g","op":"add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)"}]}`)
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":               "* @org/original_team\n",
		".github/workflows/ci.yml": "name: ci\n",
	})
	code, out, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol, "--format", "json")
	if code != cli.ExitOK {
		t.Fatalf("want exit 0, got %d\nstderr:\n%s", code, errOut)
	}
	pfWantFile(t, filepath.Join(repo, "CODEOWNERS"),
		"* @org/original_team\n"+
			"/.github/ @org/original_team @org/team_a\n")
	rec := syncDecodeRecord(t, out)
	op := fleetOpResult(t, rec, "g")
	if !reflect.DeepEqual(op.ExceptUnmatched, []string{"/.github/CODEOWNERS"}) {
		t.Errorf("except_unmatched = %v, want [/.github/CODEOWNERS]", op.ExceptUnmatched)
	}
	if op.Proven != "structural" {
		t.Errorf("proven = %q, want structural — allow is a declare-class weakening however it was spelled", op.Proven)
	}
	if len(rec.Warnings) == 0 {
		t.Errorf("an inert except must warn, whether the allow came from the op or the defaults block:\n%s", out)
	}
}

// SPEC R-35b/R-28: the per-op value wins in the RESTRICTIVE direction too. A
// block-wide `allow` with one op stating `require` must refuse that op's
// repo, exit 2, and write nothing — otherwise "we relaxed this fleet-wide
// except for the one op that guards .github/" is not expressible, and the
// block becomes a way to weaken an op whose author asked for the opposite.
func TestR35b_PerOpExceptValueWinsOverTheDefault(t *testing.T) {
	pol := syncWritePolicy(t, `{"version":1,
		"defaults":{"on_except_zero_match":"allow"},
		"ops":[{"id":"g","op":"add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)",
		        "on_except_zero_match":"require"}]}`)
	repo := initRepo(t, map[string]string{
		"CODEOWNERS":               "* @org/original_team\n",
		".github/workflows/ci.yml": "name: ci\n",
	})
	snap := checkDirSnapshot(t, repo)
	code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
	if code != cli.ExitRefused {
		t.Fatalf("want exit 2, got %d\nstderr:\n%s", code, errOut)
	}
	pfWantFragment(t, "stderr", errOut, "except pattern")
	pfWantFragment(t, "stderr", errOut, "matches zero tracked files")
	if after := checkDirSnapshot(t, repo); !reflect.DeepEqual(snap, after) {
		t.Errorf("a refusal must write nothing; repo changed")
	}
}

// SPEC R-35c: `on_empty` inside `defaults` is exit 3. It is one policy for
// the run, not a per-op setting, and two spellings for one setting is the
// ambiguity docs/policy.md exists to remove — accepting it in both places
// would leave "which one wins" as a precedence rule nobody wrote down.
//
// The fragment cannot be "on_empty" or "defaults": today's rejection of the
// unknown top-level field prints the legal top-level set, which contains
// BOTH. `"on_except_zero_match"` is the discriminator — no message about the
// top level lists it, and every message about the defaults block's field set
// must.
func TestR35c_OnEmptyInsideDefaultsIsExit3(t *testing.T) {
	pfWantExit3(t, `{"version":1,
		"defaults":{"on_empty":"error"},
		"ops":["add_owner(/docs/, @org/docs-team)"]}`,
		`"on_except_zero_match"`)
}

// SPEC R-35d: an unknown key inside `defaults` is exit 3, matching R-20's
// treatment of unknown top-level fields. The dangerous case is the near miss:
// a typo'd default is not "no default", it is the WRONG default applied to
// every op in the policy at once, which is the failure the whole strict-parse
// rule exists to prevent.
//
// Both fragments are load-bearing: the offending key (today's message names
// only "defaults", so this cannot pass vacuously) and the block's field set,
// which is what tells the operator where the key was rejected FROM.
func TestR35d_UnknownKeyInsideDefaultsIsExit3(t *testing.T) {
	t.Run("near miss of a legal key", func(t *testing.T) {
		pfWantExit3(t, `{"version":1,
			"defaults":{"on_zero_mtach":"skip"},
			"ops":["add_owner(/docs/, @org/docs-team)"]}`,
			`"on_zero_mtach"`, `"on_except_zero_match"`)
	})
	// Legal on an op, and meaningless as a default: the field sets are per
	// level rather than one merged bag (R-20).
	t.Run("legal one level down", func(t *testing.T) {
		pfWantExit3(t, `{"version":1,
			"defaults":{"note":"applies everywhere"},
			"ops":["add_owner(/docs/, @org/docs-team)"]}`,
			`"note"`, `"on_except_zero_match"`)
	})
	t.Run("not an object", func(t *testing.T) {
		pfWantExit3(t, `{"version":1,
			"defaults":"on_zero_match=skip",
			"ops":["add_owner(/docs/, @org/docs-team)"]}`,
			`"defaults" must be an object`)
	})
}

// SPEC R-35d/R-21: a bad enum in the defaults block is exit 3, with the same
// diagnosis the per-op field gets. A default is the higher-stakes place to
// get an enum wrong — one bad value reaches every op in the policy — so it
// cannot be the place with the weaker message.
//
// The present-but-empty case is the same rule as the per-op field: an ABSENT
// default means every op keeps the built-in `require`, while "" states no
// decision at all, and reads to a reviewer as though one was made.
func TestR35d_BadEnumInsideDefaultsIsExit3(t *testing.T) {
	t.Run("unknown zero-match value", func(t *testing.T) {
		pfWantExit3(t, `{"version":1,
			"defaults":{"on_zero_match":"maybe"},
			"ops":["add_owner(/docs/, @org/docs-team)"]}`,
			`"maybe"`, `"require", "skip", "declare"`)
	})
	t.Run("unknown except value", func(t *testing.T) {
		pfWantExit3(t, `{"version":1,
			"defaults":{"on_except_zero_match":"maybe"},
			"ops":["add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)"]}`,
			`"maybe"`, `"require" or "allow"`)
	})
	t.Run("present but empty", func(t *testing.T) {
		pfWantExit3(t, `{"version":1,
			"defaults":{"on_zero_match":""},
			"ops":["add_owner(/docs/, @org/docs-team)"]}`,
			"is present but empty")
	})
	t.Run("not a string", func(t *testing.T) {
		pfWantExit3(t, `{"version":1,
			"defaults":{"on_zero_match":true},
			"ops":["add_owner(/docs/, @org/docs-team)"]}`,
			`must be a string`)
	})
}

// SPEC R-35e: a default is applied only to ops that can carry it. Both fields
// below are REJECTED on the op they are paired with when written per-op —
// `on_zero_match` on rename_owner (its scope comes from current ownership, so
// zero-match can never fire) and `on_except_zero_match` on an op with no
// except clause (R-27.6) — and a policy that states them as defaults must not
// fail for that reason. The default simply does not reach those ops.
//
// This is the requirement that decides whether the block is usable at all: a
// fleet baseline that states `on_zero_match` once and happens to contain one
// rename would otherwise be rejected on every repo, and the operator's only
// route back is to expand the block into 40 per-op values.
func TestR35e_DefaultsNeverReachAnOpThatCannotCarryThem(t *testing.T) {
	t.Run("rename op", func(t *testing.T) {
		pol := syncWritePolicy(t, `{"version":1,
			"defaults":{"on_zero_match":"declare"},
			"ops":["rename_owner(@org/all, @org/everyone)"]}`)
		if code, _, errOut := runCLI(t, "check", "--policy", pol); code != cli.ExitOK {
			t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		repo := pfRepoWithFile(t)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("sync: want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		pfWantFile(t, filepath.Join(repo, "CODEOWNERS"), "# owners\n* @org/everyone\n")
	})
	t.Run("op with no except clause", func(t *testing.T) {
		pol := syncWritePolicy(t, `{"version":1,
			"defaults":{"on_except_zero_match":"allow"},
			"ops":["add_owner(/docs/, @org/docs-team)"]}`)
		if code, _, errOut := runCLI(t, "check", "--policy", pol); code != cli.ExitOK {
			t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		repo := pfRepoWithFile(t)
		code, _, errOut := runCLI(t, "sync", "--repo", repo, "--policy", pol)
		if code != cli.ExitOK {
			t.Fatalf("sync: want exit 0, got %d\nstderr:\n%s", code, errOut)
		}
		pfWantFile(t, filepath.Join(repo, "CODEOWNERS"),
			"# owners\n"+
				"* @org/all\n"+
				"/docs/ @org/all @org/docs-team\n")
	})
}

// SPEC R-35: an empty `defaults` block is legal and states nothing, so the
// policy behaves exactly as one with no block at all — every op keeps the
// built-in `require`, and a zero-match scope still refuses this repo at exit
// 2.
//
// Deliberately NOT the treatment `"on_zero_match": ""` gets. The strict-parse
// rule bites where a spelling states a decision that was never made; `{}`
// states no default for anything, there is no enum to misread, and rejecting
// it would fail every repo for a generator that emits the key unconditionally
// and fills it only sometimes.
//
// The oracle is the stderr of the block-free policy, compared verbatim: "same
// exit code" alone would be satisfied by a refusal for an entirely different
// reason.
func TestR35_EmptyDefaultsBlockIsIndistinguishableFromAbsence(t *testing.T) {
	const opsField = `"ops":["add_owner(/deploy/, @org/sre)"]}`
	empty := syncWritePolicy(t, `{"version":1,"defaults":{},`+opsField)
	absent := syncWritePolicy(t, `{"version":1,`+opsField)
	if code, _, errOut := runCLI(t, "check", "--policy", empty); code != cli.ExitOK {
		t.Fatalf("check: want exit 0, got %d\nstderr:\n%s", code, errOut)
	}

	withBlock := pfRepoWithFile(t)
	snap := checkDirSnapshot(t, withBlock)
	gotCode, _, gotErr := runCLI(t, "sync", "--repo", withBlock, "--policy", empty)
	wantCode, _, wantErr := runCLI(t, "sync", "--repo", pfRepoWithFile(t), "--policy", absent)
	if gotCode != wantCode || gotErr != wantErr {
		t.Errorf("`defaults: {}` behaves differently from no defaults block:\n exit %d, stderr:\n%s\nwant exit %d, stderr:\n%s",
			gotCode, gotErr, wantCode, wantErr)
	}
	if gotCode != cli.ExitRefused {
		t.Errorf("want exit 2: with no default stated, /deploy/ still matches zero tracked files under the built-in require, got %d", gotCode)
	}
	if after := checkDirSnapshot(t, withBlock); !reflect.DeepEqual(snap, after) {
		t.Errorf("a refusal must write nothing; repo changed")
	}
}
