package cli_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/cli"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// ---------------------------------------------------------------------------
// The synthetic fleet.
//
// Every test in this file runs ONE policy across a set of deliberately
// heterogeneous repositories, exactly as docs/FLEET.md's script does. The
// repos are the shapes a real 100-repo rollout meets on day one: repos that
// converge, repos that are already correct, repos missing the directory an op
// names, repos with no CODEOWNERS at all, repos whose file shape makes the
// intent inexpressible. The fleet is the unit under test — no single repo's
// outcome may change any other repo's outcome.
// ---------------------------------------------------------------------------

// fleetPolicySrc is the worked example from the UX spec: one mandatory op that
// every repo is expected to satisfy, one opportunistic op that skips where the
// files do not exist, one baseline op that is declared for files a repo does
// not have yet. Every op carries an explicit id, because the per-repo records
// are keyed by it.
const fleetPolicySrc = `{
  "version": 1,
  "name": "org baseline ownership",
  "description": "CI owns workflows everywhere; infra owns Terraform where it exists.",
  "ops": [
    { "id": "api", "op": "add_owner(/services/api/, @org/api-team)",
      "note": "Every service repo has one." },
    { "id": "tf",  "op": "add_owner(**/*.tf, @org/infra)", "on_zero_match": "skip",
      "note": "Opportunistic — only repos that actually have Terraform." },
    { "id": "ci",  "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare",
      "note": "Baseline; also covers workflows added later." }
  ]
}`

// fleetTypoPolicySrc is the failure the `skipped` status exists to make
// visible: a policy whose path prefixes are misspelled matches nothing in any
// repo. Both ops use the bare-string-in-an-object form with no id, so this
// also exercises the shorthand alongside fleetPolicySrc's fully-specified ops.
const fleetTypoPolicySrc = `{
  "version": 1,
  "name": "typo'd prefixes",
  "ops": [
    { "op": "add_owner(/servces/api/, @org/api-team)", "on_zero_match": "skip" },
    { "op": "add_owner(/.githbu/workflows/, @org/ci)", "on_zero_match": "skip" }
  ]
}`

// fleetBrokenPolicySrc is the exit-3 class in its most dangerous form: a typo
// in a field name. Silently defaulting `on_zero_mtach` would apply the wrong
// policy to all 100 repos at once, which is the whole reason unknown fields
// are fatal.
const fleetBrokenPolicySrc = `{
  "version": 1,
  "ops": [
    { "id": "api", "op": "add_owner(/services/api/, @org/api-team)", "on_zero_mtach": "skip" }
  ]
}`

// fleetShape is one repository in the synthetic fleet, plus what the fleet
// policy must do to it.
type fleetShape struct {
	name  string
	files map[string]string
	// wantStatus is the record's status under fleetPolicySrc carrying
	// `"create": true` (R-34a).
	wantStatus string
	// wantExit is sync's exit code for this repo under the same run.
	wantExit int
}

// fleetShapes is the fleet. Each entry is a shape a standardized policy meets
// across a real org; together they cover every branch of the exit contract.
func fleetShapes() []fleetShape {
	return []fleetShape{
		{
			// Plain: has a CODEOWNERS, has every path the policy names.
			// All three ops apply against real tracked files.
			name: "plain",
			files: map[string]string{
				".github/CODEOWNERS":       "# owners\n/services/ @org/platform\n",
				"services/api/main.go":     "package api\n",
				"services/web/app.js":      "//\n",
				".github/workflows/ci.yml": "on: push\n",
				"infra/main.tf":            "# tf\n",
			},
			wantStatus: cli.StatusApplied,
			wantExit:   cli.ExitOK,
		},
		{
			// Already correct: every op is already true. The common fleet
			// outcome — it must be exit 0 and must not rewrite the file.
			name: "correct",
			files: map[string]string{
				".github/CODEOWNERS":       "/services/api/ @org/api-team\n**/*.tf @org/infra\n/.github/workflows/ @org/ci\n",
				"services/api/main.go":     "package api\n",
				".github/workflows/ci.yml": "on: push\n",
				"infra/main.tf":            "# tf\n",
			},
			wantStatus: cli.StatusUnchanged,
			wantExit:   cli.ExitOK,
		},
		{
			// No Terraform: the `tf` op (on_zero_match=skip) matches nothing.
			// That op skips; the other two still apply.
			name: "no-tf",
			files: map[string]string{
				".github/CODEOWNERS":       "# owners\n/services/ @org/platform\n",
				"services/api/main.go":     "package api\n",
				".github/workflows/ci.yml": "on: push\n",
			},
			wantStatus: cli.StatusApplied,
			wantExit:   cli.ExitOK,
		},
		{
			// No CODEOWNERS at all. Exit 2 unless the policy says `create`;
			// the fleet run's policy does, so here it converges and the file
			// is created.
			name: "no-file",
			files: map[string]string{
				"services/api/main.go":     "package api\n",
				".github/workflows/ci.yml": "on: push\n",
				"infra/main.tf":            "# tf\n",
			},
			wantStatus: cli.StatusApplied,
			wantExit:   cli.ExitOK,
		},
		{
			// A `*` catch-all and no workflows directory: the `ci` op is
			// declared for files that do not exist yet, and must land at EOF
			// where the catch-all cannot shadow it (INV-6).
			name: "catchall",
			files: map[string]string{
				".github/CODEOWNERS":   "* @org/eng\n",
				"services/api/main.go": "package api\n",
				"infra/main.tf":        "# tf\n",
			},
			wantStatus: cli.StatusApplied,
			wantExit:   cli.ExitOK,
		},
		{
			// Inexpressible: the unanchored rule "infra/" governs paths both
			// inside and outside `**/*.tf`, and no sound narrowing pattern is
			// derivable — amending breaks INV-2, appending breaks INV-1.
			name: "refuse",
			files: map[string]string{
				".github/CODEOWNERS":       "infra/ @infra-legacy\n",
				"services/api/main.go":     "package api\n",
				"infra/main.tf":            "# tf\n",
				"infra/README.md":          "# infra\n",
				".github/workflows/ci.yml": "on: push\n",
			},
			wantStatus: cli.StatusRefused,
			wantExit:   cli.ExitRefused,
		},
		{
			// No /services/api/ at all: the `api` op carries the default
			// on_zero_match=require, so this repo needs a human — exit 2, and
			// emphatically NOT exit 3, which would halt the whole fleet.
			name: "zero-require",
			files: map[string]string{
				".github/CODEOWNERS":       "# owners\n/lib/ @org/eng\n",
				"lib/util.go":              "package lib\n",
				".github/workflows/ci.yml": "on: push\n",
				"infra/main.tf":            "# tf\n",
			},
			wantStatus: cli.StatusRefused,
			wantExit:   cli.ExitRefused,
		},
	}
}

// fleetBuild materializes the named shapes as real git repositories and
// returns name -> repo directory, plus the processing order.
func fleetBuild(t *testing.T, names ...string) (map[string]string, []string) {
	t.Helper()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	dirs := map[string]string{}
	var order []string
	for _, s := range fleetShapes() {
		if len(names) > 0 && !want[s.name] {
			continue
		}
		dirs[s.name] = initRepo(t, s.files)
		order = append(order, s.name)
	}
	if len(names) > 0 && len(order) != len(names) {
		t.Fatalf("fleetBuild: asked for %v, built %v — unknown shape name", names, order)
	}
	return dirs, order
}

// fleetPolicyFile writes a policy OUTSIDE every clone — docs/FLEET.md is
// explicit that anything written inside one is a `git add -A` away from being
// committed.
func fleetPolicyFile(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// fleetCodeowners returns a repo's CODEOWNERS content, or ok=false when the
// repo has none. It looks in every location the tool itself considers.
func fleetCodeowners(t *testing.T, dir string) (string, bool) {
	t.Helper()
	for _, rel := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err == nil {
			return string(b), true
		}
	}
	return "", false
}

// fleetSnapshot records every repo's CODEOWNERS bytes so a test can prove a
// whole fleet run wrote nothing where it must not.
func fleetSnapshot(t *testing.T, dirs map[string]string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	for name, dir := range dirs {
		content, ok := fleetCodeowners(t, dir)
		if !ok {
			snap[name] = "\x00absent"
			continue
		}
		snap[name] = content
	}
	return snap
}

// fleetOutcome is one pass of docs/FLEET.md's loop over the fleet. A code of -1
// means the loop never reached that repo.
type fleetOutcome struct {
	codes     map[string]int
	stderr    map[string]string
	processed []string // repos the loop actually reached, in order
	halted    string   // repo whose exit code aborted the loop; "" if none
	jsonl     string   // the concatenated records, i.e. results.jsonl
}

// fleetRunPolicy walks the fleet exactly as docs/FLEET.md's script does: run
// sync, append stdout to results.jsonl, continue on 0 and 2, and abort the
// whole run on anything else. The abort is the contract under test — a fleet
// script cannot be more forgiving than the tool's exit codes let it be.
func fleetRunPolicy(t *testing.T, dirs map[string]string, order []string, policy string, extra ...string) fleetOutcome {
	t.Helper()
	out := fleetOutcome{codes: map[string]int{}, stderr: map[string]string{}}
	for _, name := range order {
		// -1 until the loop reaches it, so an unprocessed repo can never be
		// mistaken for one that exited 0.
		out.codes[name] = -1
	}
	for _, name := range order {
		args := append([]string{"sync", "--repo", dirs[name], "--policy", policy, "--format", "json"}, extra...)
		code, stdout, errOut := runCLI(t, args...)
		out.jsonl += stdout
		out.codes[name] = code
		out.stderr[name] = errOut
		out.processed = append(out.processed, name)
		if code != cli.ExitOK && code != cli.ExitRefused {
			out.halted = name
			return out
		}
	}
	return out
}

// fleetRecords parses results.jsonl the way `jq -s` does: one object per line,
// nothing trailing on any line. A stray log line or a pretty-printed record
// breaks every fleet consumer, so the parse itself is an assertion.
func fleetRecords(t *testing.T, jsonl string) []cli.SyncRecord {
	t.Helper()
	var recs []cli.SyncRecord
	if jsonl == "" {
		return recs
	}
	if !strings.HasSuffix(jsonl, "\n") {
		t.Errorf("results.jsonl must end with a newline, or `>>` concatenates two records into one line:\n%q", jsonl)
	}
	for i, line := range strings.Split(strings.TrimSuffix(jsonl, "\n"), "\n") {
		dec := json.NewDecoder(strings.NewReader(line))
		var rec cli.SyncRecord
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("results.jsonl line %d is not valid JSON: %v\nraw: %s", i+1, err, line)
		}
		var trailing json.RawMessage
		if err := dec.Decode(&trailing); err != io.EOF {
			t.Fatalf("results.jsonl line %d must hold exactly one object, got trailing %s\nraw: %s", i+1, trailing, line)
		}
		recs = append(recs, rec)
	}
	return recs
}

// fleetByRepo keys the records by the --repo value each run was given. The
// record must name its repo, or an aggregation over 100 lines cannot say
// WHICH repo needs a human.
func fleetByRepo(t *testing.T, recs []cli.SyncRecord, dirs map[string]string) map[string]cli.SyncRecord {
	t.Helper()
	byPath := map[string]cli.SyncRecord{}
	for _, r := range recs {
		byPath[r.Repo] = r
	}
	out := map[string]cli.SyncRecord{}
	for name, dir := range dirs {
		rec, ok := byPath[dir]
		if !ok {
			t.Errorf("no record whose .repo is %q (repo %s); records name %v", dir, name, byPath)
			continue
		}
		out[name] = rec
	}
	return out
}

// fleetGroupByStatus mirrors `jq -s 'group_by(.status)|map({status, n:length})'`
// — the last line of docs/FLEET.md's script, and the only view an operator of a
// 100-repo rollout actually reads.
func fleetGroupByStatus(recs []cli.SyncRecord) map[string]int {
	counts := map[string]int{}
	for _, r := range recs {
		counts[r.Status]++
	}
	return counts
}

// fleetOpResult finds one per-op result by policy id.
func fleetOpResult(t *testing.T, rec cli.SyncRecord, id string) plan.OpResult {
	t.Helper()
	for _, r := range rec.Ops {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("record for %q has no op result with id %q: %+v", rec.Repo, id, rec.Ops)
	return plan.OpResult{}
}

// fleetRuleLines returns the CODEOWNERS rule lines (comments and blanks
// dropped) in file order — enough to reason about last-match-wins.
func fleetRuleLines(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// SPEC R-19/R-20/R-24: one policy, one pass, seven deliberately different
// repositories. This is the whole product claim in a single test — that a
// standardized policy can be pushed at a heterogeneous fleet, that each repo
// gets the outcome its own shape earns, and that the aggregate is readable
// afterwards. If any single repo's failure can change another repo's outcome,
// the fleet script in docs/FLEET.md is a lie.
func TestFleet_OnePolicyAcrossHeterogeneousRepos(t *testing.T) {
	t.Parallel()
	dirs, order := fleetBuild(t)
	policy := fleetPolicyFile(t, policyWithCreate(t, fleetPolicySrc))
	before := fleetSnapshot(t, dirs)

	out := fleetRunPolicy(t, dirs, order, policy)

	// The run COMPLETES. A refusal at repo 6 must not cost you repo 7.
	t.Run("run completes", func(t *testing.T) {
		if out.halted != "" {
			t.Fatalf("fleet halted at %q (exit %d): no per-repo outcome may abort the run\nstderr: %s",
				out.halted, out.codes[out.halted], out.stderr[out.halted])
		}
		if !reflect.DeepEqual(out.processed, order) {
			t.Fatalf("processed %v, want every repo in order %v", out.processed, order)
		}
	})

	// Exactly the expected status and exit code per repo.
	t.Run("per-repo statuses", func(t *testing.T) {
		recs := fleetRecords(t, out.jsonl)
		byRepo := fleetByRepo(t, recs, dirs)
		for _, s := range fleetShapes() {
			if got := out.codes[s.name]; got != s.wantExit {
				t.Errorf("%s: exit %d, want %d\nstderr: %s", s.name, got, s.wantExit, out.stderr[s.name])
			}
			rec, ok := byRepo[s.name]
			if !ok {
				continue
			}
			if rec.Status != s.wantStatus {
				t.Errorf("%s: status %q, want %q", s.name, rec.Status, s.wantStatus)
			}
		}
	})

	// Per-op detail: the reason `skip` and `declare` exist at all is that a
	// bare count cannot say WHICH repos lack Terraform.
	t.Run("per-op results", func(t *testing.T) {
		recs := fleetRecords(t, out.jsonl)
		byRepo := fleetByRepo(t, recs, dirs)

		plain := byRepo["plain"]
		for _, id := range []string{"api", "tf", "ci"} {
			r := fleetOpResult(t, plain, id)
			if r.Status != "applied" {
				t.Errorf("plain op %s: status %q, want applied", id, r.Status)
			}
			if r.Proven != "tree" {
				t.Errorf("plain op %s: proven %q, want tree — every op here matched real tracked files", id, r.Proven)
			}
		}
		if plain.OpsApplied != 3 || plain.OpsSkipped != 0 {
			t.Errorf("plain: ops_applied=%d ops_skipped=%d, want 3/0", plain.OpsApplied, plain.OpsSkipped)
		}

		noTF := byRepo["no-tf"]
		if r := fleetOpResult(t, noTF, "tf"); r.Status != "skipped" {
			t.Errorf("no-tf op tf: status %q, want skipped (on_zero_match=skip)", r.Status)
		} else if r.Reason == "" {
			t.Error("no-tf op tf: a skipped op must carry a reason — otherwise the record cannot say why")
		}
		for _, id := range []string{"api", "ci"} {
			if r := fleetOpResult(t, noTF, id); r.Status != "applied" {
				t.Errorf("no-tf op %s: status %q, want applied — one skipped op must not suppress the others", id, r.Status)
			}
		}
		if noTF.OpsApplied != 2 || noTF.OpsSkipped != 1 {
			t.Errorf("no-tf: ops_applied=%d ops_skipped=%d, want 2/1", noTF.OpsApplied, noTF.OpsSkipped)
		}

		correct := byRepo["correct"]
		for _, id := range []string{"api", "tf", "ci"} {
			if r := fleetOpResult(t, correct, id); r.Status != "unchanged" {
				t.Errorf("correct op %s: status %q, want unchanged", id, r.Status)
			}
		}

		if got := byRepo["no-file"].Created; !got {
			t.Error("no-file: created must be true — the record is how the fleet script learns a file was written from nothing")
		}
		for _, name := range []string{"plain", "correct", "no-tf", "catchall"} {
			if byRepo[name].Created {
				t.Errorf("%s: created must be false — the repo already had a CODEOWNERS", name)
			}
		}
	})

	// Nothing partially written. A refusal is a promise about the bytes.
	t.Run("exit 2 leaves the file byte-identical", func(t *testing.T) {
		// The refusing SET is pinned before any byte is compared. Skipping
		// every repo whose code is not 2 and then comparing bytes is a test
		// that reports success when sync refuses nothing at all — or refuses
		// everything — because the loop body simply never runs. The promise
		// under test ("a refusal writes no bytes") is only meaningful once we
		// know which repos refused, so that comes first.
		var refused []string
		for _, name := range order {
			if out.codes[name] == cli.ExitRefused {
				refused = append(refused, name)
			}
		}
		// `order` is fleetShapes() order, so this is deterministic: the only
		// two shapes that cannot be converged are the inexpressible one and
		// the one missing the directory a `require` op names.
		wantRefused := []string{"refuse", "zero-require"}
		if !reflect.DeepEqual(refused, wantRefused) {
			t.Fatalf("repos that exited 2 = %v, want exactly %v (all codes %v)", refused, wantRefused, out.codes)
		}
		after := fleetSnapshot(t, dirs)
		for _, name := range refused {
			if after[name] != before[name] {
				t.Errorf("%s exited 2 but its CODEOWNERS changed:\nbefore %q\nafter  %q", name, before[name], after[name])
			}
		}
		// And the converse: "unchanged" means unchanged, to the byte.
		if after["correct"] != before["correct"] {
			t.Errorf("correct: status unchanged must mean byte-identical:\nbefore %q\nafter  %q", before["correct"], after["correct"])
		}
	})

	// The aggregate the operator actually reads.
	t.Run("aggregate jsonl groups by status", func(t *testing.T) {
		recs := fleetRecords(t, out.jsonl)
		if len(recs) != len(order) {
			t.Fatalf("results.jsonl has %d records for %d repos — every repo must emit exactly one line, including the ones that refused", len(recs), len(order))
		}
		want := map[string]int{}
		for _, s := range fleetShapes() {
			want[s.wantStatus]++
		}
		if got := fleetGroupByStatus(recs); !reflect.DeepEqual(got, want) {
			t.Errorf("group_by(.status) = %v, want %v", got, want)
		}
	})
}

// SPEC INV-6 (R-21 declare): a repo whose CODEOWNERS opens with a `*`
// catch-all is the case that makes `declare` non-trivial. Reusing the
// unowned-path insert point would put the declared rule BEFORE the catch-all,
// where last-match-wins hands every future workflow file back to `@org/eng` —
// 100 PRs that do nothing. The rule must be appended at EOF, and it must
// actually govern its scope when a matching file finally appears.
func TestFleet_DeclareLandsAtEOFUnderCatchAll(t *testing.T) {
	t.Parallel()
	dirs, order := fleetBuild(t, "catchall")
	policy := fleetPolicyFile(t, fleetPolicySrc)

	out := fleetRunPolicy(t, dirs, order, policy)
	if out.codes["catchall"] != cli.ExitOK {
		t.Fatalf("catchall: exit %d, want 0\nstderr: %s", out.codes["catchall"], out.stderr["catchall"])
	}

	rec := fleetByRepo(t, fleetRecords(t, out.jsonl), dirs)["catchall"]
	ci := fleetOpResult(t, rec, "ci")
	if ci.Status != "applied" {
		t.Errorf("op ci: status %q, want applied — declare writes the rule even with nothing to match", ci.Status)
	}
	if ci.Proven != "structural" {
		t.Errorf("op ci: proven %q, want structural — there is nothing tracked to prove it against (INV-6)", ci.Proven)
	}

	content, ok := fleetCodeowners(t, dirs["catchall"])
	if !ok {
		t.Fatal("catchall lost its CODEOWNERS")
	}
	rules := fleetRuleLines(content)
	if len(rules) == 0 {
		t.Fatalf("no rules left in:\n%s", content)
	}
	last := rules[len(rules)-1]
	if !strings.HasPrefix(last, "/.github/workflows/") {
		t.Errorf("last rule is %q, want the declared /.github/workflows/ rule at EOF — nothing after it can override it\nfile:\n%s", last, content)
	}
	if rules[0] != "* @org/eng" {
		t.Errorf("the pre-existing catch-all must survive untouched (INV-2), got %q\nfile:\n%s", rules[0], content)
	}

	// The point of declare: when the file eventually exists, this rule takes
	// it. Resolve a hypothetical future path against the written bytes.
	future := ".github/workflows/ci.yml"
	res := plan.ResolveContent(content, []string{future})[future]
	if !res.Matched {
		t.Fatalf("%s matches no rule after declare\nfile:\n%s", future, content)
	}
	var hasCI bool
	for _, o := range res.Owners {
		if o == "@org/ci" {
			hasCI = true
		}
	}
	if !hasCI {
		t.Errorf("%s resolves to %v — the declared rule is shadowed by the catch-all, which is the exact failure declare exists to avoid\nfile:\n%s",
			future, res.Owners, content)
	}
}

// SPEC R-23/R-34a: a repo with no CODEOWNERS is exit 2 when the policy does not
// say `create` — a per-repo fact, recorded and skipped past, never a halt. The
// same policy carrying `"create": true` writes the file at .github/CODEOWNERS,
// which is the path the README promises.
//
// Two policies, differing in exactly the field R-34b moved out of the command
// line, are the whole test: one must refuse and one must create. Running one
// policy twice would stop distinguishing them and leave the requirement
// untested, so the fixtures are derived from a single source and the refusing
// leg asserts the absence of the file the creating leg asserts the presence of.
func TestFleet_MissingCodeownersNeedsCreate(t *testing.T) {
	t.Parallel()
	dirs, order := fleetBuild(t, "no-file")
	policy := fleetPolicyFile(t, fleetPolicySrc)
	creating := fleetPolicyFile(t, policyWithCreate(t, fleetPolicySrc))
	dir := dirs["no-file"]

	out := fleetRunPolicy(t, dirs, order, policy)
	if got := out.codes["no-file"]; got != cli.ExitRefused {
		t.Fatalf("no CODEOWNERS and no `create` in the policy: exit %d, want 2 — exit 3 would halt a fleet on roughly repo 3\nstderr: %s",
			got, out.stderr["no-file"])
	}
	if _, ok := fleetCodeowners(t, dir); ok {
		t.Error("exit 2 must write nothing at all — a file appeared for a policy that never asked for one")
	}
	// An exit-2 run DOES emit its record: `needs-human` is a list of repos
	// someone has to triage, and it is built from results.jsonl. Tolerating a
	// count other than 1 here would let a refusal emit nothing — the exact
	// hole this assertion exists to close — and still report a pass.
	recs := fleetRecords(t, out.jsonl)
	if len(recs) != 1 {
		t.Fatalf("exit 2 emitted %d records for 1 repo, want exactly 1 — a refused repo missing from results.jsonl is a hole in the aggregation precisely where the operator has to look\nraw: %q",
			len(recs), out.jsonl)
	}
	if recs[0].Created {
		t.Error("created must be false when nothing was created")
	}

	out = fleetRunPolicy(t, dirs, order, creating)
	if got := out.codes["no-file"]; got != cli.ExitOK {
		t.Fatalf("policy `\"create\": true`: exit %d, want 0\nstderr: %s", got, out.stderr["no-file"])
	}
	created := filepath.Join(dir, ".github", "CODEOWNERS")
	b, err := os.ReadFile(created)
	if err != nil {
		t.Fatalf("policy `\"create\": true` must write .github/CODEOWNERS: %v", err)
	}
	if len(b) == 0 {
		t.Error("created CODEOWNERS is empty")
	}
	rec := fleetByRepo(t, fleetRecords(t, out.jsonl), dirs)["no-file"]
	if !rec.Created {
		t.Error("created must be true in the record")
	}
	if rec.Status != cli.StatusApplied {
		t.Errorf("status %q, want applied", rec.Status)
	}
}

// SPEC R-21/R-19: an op whose scope matches zero tracked files under the
// DEFAULT on_zero_match=require is exit 2, not 3. Whether a path exists is the
// most repo-specific fact there is; revision 1 mapped it to 3 and a policy
// naming /services/api/ killed the fleet on the first repo that lacked it.
func TestFleet_ZeroMatchUnderRequireIsTwoNotThree(t *testing.T) {
	t.Parallel()
	dirs, order := fleetBuild(t, "zero-require")
	policy := fleetPolicyFile(t, policyWithCreate(t, fleetPolicySrc))
	before := fleetSnapshot(t, dirs)

	out := fleetRunPolicy(t, dirs, order, policy)
	got := out.codes["zero-require"]
	if got == cli.ExitInvalid {
		t.Fatalf("zero-match under require exited 3 — that halts the whole fleet for a fact about one repo\nstderr: %s", out.stderr["zero-require"])
	}
	if got != cli.ExitRefused {
		t.Fatalf("zero-match under require: exit %d, want 2\nstderr: %s", got, out.stderr["zero-require"])
	}
	if after := fleetSnapshot(t, dirs); after["zero-require"] != before["zero-require"] {
		t.Errorf("exit 2 wrote to the file:\nbefore %q\nafter  %q", before["zero-require"], after["zero-require"])
	}
	rec := fleetByRepo(t, fleetRecords(t, out.jsonl), dirs)["zero-require"]
	if rec.Status != cli.StatusRefused {
		t.Errorf("status %q, want refused", rec.Status)
	}
	if rec.Error == "" {
		t.Error("a refused record must carry the reason — `needs-human` is a list of repos someone has to triage")
	}
}

// SPEC R-19: a refusal is per-repo and RECOVERABLE. The repo that cannot
// express the intent exits 2 with byte-identical bytes, and the repos after it
// in the loop converge exactly as if it had never been there.
func TestFleet_RefusalIsRecoverableAndIsolated(t *testing.T) {
	t.Parallel()
	dirs, order := fleetBuild(t, "refuse", "plain")
	policy := fleetPolicyFile(t, policyWithCreate(t, fleetPolicySrc))
	before := fleetSnapshot(t, dirs)

	out := fleetRunPolicy(t, dirs, order, policy)
	if out.halted != "" {
		t.Fatalf("the loop stopped at %q; a refusal must be recorded and stepped over", out.halted)
	}
	if got := out.codes["refuse"]; got != cli.ExitRefused {
		t.Fatalf("refuse: exit %d, want 2\nstderr: %s", got, out.stderr["refuse"])
	}
	if got := out.codes["plain"]; got != cli.ExitOK {
		t.Fatalf("plain (after a refusal): exit %d, want 0 — one repo's refusal must not touch the next\nstderr: %s",
			got, out.stderr["plain"])
	}
	after := fleetSnapshot(t, dirs)
	if after["refuse"] != before["refuse"] {
		t.Errorf("refused repo was written to:\nbefore %q\nafter  %q", before["refuse"], after["refuse"])
	}
	if after["plain"] == before["plain"] {
		t.Error("plain must have converged after the refusal")
	}
	byRepo := fleetByRepo(t, fleetRecords(t, out.jsonl), dirs)
	if byRepo["refuse"].Status != cli.StatusRefused {
		t.Errorf("refuse: status %q, want refused", byRepo["refuse"].Status)
	}
	if byRepo["refuse"].Error == "" {
		t.Error("refused record must name the rule that was in the way")
	}
	if byRepo["plain"].Status != cli.StatusApplied {
		t.Errorf("plain: status %q, want applied", byRepo["plain"].Status)
	}
}

// SPEC R-24: the record's `.repo` is the `--repo` argument BYTE-FOR-BYTE —
// never absolutized, never symlink-resolved.
//
// Six tests in this file join records back to repos through fleetByRepo, which
// keys on exactly that field; so does docs/FLEET.md's script, which pairs
// `needs-human` entries with the paths it passed in. Deriving `.repo` from the
// repository instead of from the argument breaks every one of them, and it
// breaks them on developer laptops only: on macOS `t.TempDir()` hands out
// `/var/folders/...` while `git rev-parse --show-toplevel` reports the same
// directory as `/private/var/folders/...`, because `/var` is a symlink to
// `/private/var`. The two strings name one directory and compare unequal, so
// every lookup misses, every record looks orphaned, and CI on Linux — where no
// such symlink exists — stays green the whole time.
func TestFleet_RecordRepoIsTheArgumentVerbatim(t *testing.T) {
	t.Parallel()
	// One repo that converges and one that refuses: the refused row is the one
	// an operator has to find again by path, so exit 2 must carry it too.
	dirs, order := fleetBuild(t, "plain", "refuse")
	policy := fleetPolicyFile(t, policyWithCreate(t, fleetPolicySrc))

	out := fleetRunPolicy(t, dirs, order, policy)
	recs := fleetRecords(t, out.jsonl)
	if len(recs) != len(order) {
		t.Fatalf("%d records for %d repos", len(recs), len(order))
	}
	byArg := map[string]bool{}
	for _, r := range recs {
		byArg[r.Repo] = true
	}
	for _, name := range order {
		dir := dirs[name]
		if !byArg[dir] {
			t.Errorf("%s: no record whose .repo is the --repo argument %q; records name %v", name, dir, byArg)
		}
		// Name the specific wrong answer, so a failure reads as a diagnosis
		// rather than a mystery.
		if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
			if byArg[resolved] {
				t.Errorf("%s: .repo is the symlink-RESOLVED path %q, but --repo was given %q — fleet aggregation keys on the argument and finds nothing",
					name, resolved, dir)
			}
		}
	}
}

// SPEC R-24: `skipped` is a status of its own. A policy with a typo'd path
// prefix matches nothing in any repo; if those runs reported `unchanged`, the
// jq recipe in docs/FLEET.md would show a wall of "already correct" and the
// operator would read a no-op rollout as a success.
func TestFleet_NothingMatchesAnywhereIsSkippedNotUnchanged(t *testing.T) {
	t.Parallel()
	// Only repos that already have a CODEOWNERS: whether a create-nothing run
	// creates a file is a different question, asked elsewhere.
	dirs, order := fleetBuild(t, "plain", "correct", "no-tf")
	policy := fleetPolicyFile(t, fleetTypoPolicySrc)
	before := fleetSnapshot(t, dirs)

	out := fleetRunPolicy(t, dirs, order, policy)
	for _, name := range order {
		if got := out.codes[name]; got != cli.ExitOK {
			t.Errorf("%s: exit %d, want 0 — a skip is a deliberate no-op, not a repo that needs a human\nstderr: %s",
				name, got, out.stderr[name])
		}
	}
	recs := fleetRecords(t, out.jsonl)
	if len(recs) != len(order) {
		t.Fatalf("%d records for %d repos", len(recs), len(order))
	}
	byRepo := fleetByRepo(t, recs, dirs)
	for _, name := range order {
		rec := byRepo[name]
		if rec.Status != cli.StatusSkipped {
			t.Errorf("%s: status %q, want %q — every op skipped and none applied", name, rec.Status, cli.StatusSkipped)
		}
		if rec.OpsApplied != 0 || rec.OpsSkipped != 2 {
			t.Errorf("%s: ops_applied=%d ops_skipped=%d, want 0/2", name, rec.OpsApplied, rec.OpsSkipped)
		}
		// The length is asserted before the loop: ranging over an empty Ops
		// slice makes every per-op claim below vacuously true, so a record
		// that dropped its per-op detail entirely would read as a pass.
		if len(rec.Ops) != 2 {
			t.Errorf("%s: %d per-op results, want one per policy op (2) — without them the record cannot say WHICH op skipped", name, len(rec.Ops))
		}
		for _, r := range rec.Ops {
			if r.Status != "skipped" {
				t.Errorf("%s: op %q status %q, want skipped", name, r.Op, r.Status)
			}
		}
	}
	if got := fleetGroupByStatus(recs); !reflect.DeepEqual(got, map[string]int{cli.StatusSkipped: len(order)}) {
		t.Errorf("group_by(.status) = %v, want all %d skipped", got, len(order))
	}
	if after := fleetSnapshot(t, dirs); !reflect.DeepEqual(after, before) {
		t.Error("a policy that matched nothing must have written nothing")
	}
}

// SPEC R-20/R-22: a broken policy is exit 3 on the FIRST repo, before a single
// byte is read from it or written to it. That is what makes the README's "fail
// on repo 0, not 100 times" true: the same policy would fail identically
// everywhere, so the run halts instead of producing 100 identical errors — and
// `check` catches the same class with no repo at all.
func TestFleet_BrokenPolicyHaltsOnTheFirstRepo(t *testing.T) {
	t.Parallel()
	dirs, order := fleetBuild(t)
	// The create-bearing spelling, because that is the artifact a real fleet
	// run carries — and the defect must be what stops it, not the shape of the
	// rest of the file.
	policy := fleetPolicyFile(t, policyWithCreate(t, fleetBrokenPolicySrc))
	before := fleetSnapshot(t, dirs)

	// The first line of the fleet script: this is where it should die.
	if code, _, errOut := runCLI(t, "check", "--policy", policy); code != cli.ExitInvalid {
		t.Errorf("check on a broken policy: exit %d, want 3\nstderr: %s", code, errOut)
	} else if !strings.Contains(errOut, "on_zero_mtach") {
		t.Errorf("the error must quote the offending key, or nobody can find it in a 40-op policy:\n%s", errOut)
	}

	out := fleetRunPolicy(t, dirs, order, policy)
	if out.halted != order[0] {
		t.Fatalf("halted at %q, want the FIRST repo %q (codes %v)", out.halted, order[0], out.codes)
	}
	if got := out.codes[order[0]]; got != cli.ExitInvalid {
		t.Fatalf("broken policy: exit %d, want 3", got)
	}
	if len(out.processed) != 1 {
		t.Errorf("processed %v — a broken policy must not reach a second repo", out.processed)
	}
	if out.jsonl != "" {
		t.Errorf("a policy error must emit NO record: a {\"status\":...} line in results.jsonl reports a phantom repo to the aggregation\ngot: %q", out.jsonl)
	}
	// The offending key, not merely "some exit 3": this test's whole claim is
	// that the POLICY halted the run, so any other exit-3 reason satisfying it
	// — a banned flag, a future argument rule — would be a vacuous pass.
	if !strings.Contains(out.stderr[order[0]], "on_zero_mtach") {
		t.Errorf("exit 3 must explain itself on stderr, naming the key that broke:\n%s", out.stderr[order[0]])
	}
	// Precedence, pinned: even on a command line that ALSO carries the flag
	// R-34b bans, the policy defect is what is reported. The flag ban sits
	// after the policy is loaded for exactly this reason — reporting it first
	// would let "the broken policy halted the fleet" be proven by the flag,
	// with the policy never read at all.
	if _, _, errOut := runCLI(t, "sync", "--repo", dirs[order[0]], "--policy", policy,
		"--create", "--format", "json"); !strings.Contains(errOut, "on_zero_mtach") {
		t.Errorf("--create alongside a BROKEN policy reported the flag instead of the defect:\n%s", errOut)
	}
	if after := fleetSnapshot(t, dirs); !reflect.DeepEqual(after, before) {
		t.Error("a broken policy must touch no file in any repo")
	}
}

// SPEC R-19/R-24: --dry-run is the fleet preview — the only granularity at
// which reviewing a 100-repo change is possible. It must change no CODEOWNERS
// anywhere while still emitting the complete results.jsonl and every
// --summary-out file, because those artifacts ARE the review.
func TestFleet_DryRunChangesNothingButStillEmits(t *testing.T) {
	t.Parallel()
	dirs, order := fleetBuild(t)
	policy := fleetPolicyFile(t, policyWithCreate(t, fleetPolicySrc))
	before := fleetSnapshot(t, dirs)
	bodies := t.TempDir() // outside every clone, per the README

	out := fleetOutcome{codes: map[string]int{}, stderr: map[string]string{}}
	summaries := map[string]string{}
	for _, name := range order {
		summaries[name] = filepath.Join(bodies, name+".md")
		code, stdout, errOut := runCLI(t, "sync",
			"--repo", dirs[name], "--policy", policy, "--dry-run",
			"--format", "json", "--summary-out", summaries[name])
		out.jsonl += stdout
		out.codes[name] = code
		out.stderr[name] = errOut
		out.processed = append(out.processed, name)
	}

	if after := fleetSnapshot(t, dirs); !reflect.DeepEqual(after, before) {
		for name := range dirs {
			if after[name] != before[name] {
				t.Errorf("--dry-run wrote to %s:\nbefore %q\nafter  %q", name, before[name], after[name])
			}
		}
	}

	recs := fleetRecords(t, out.jsonl)
	if len(recs) != len(order) {
		t.Fatalf("--dry-run produced %d records for %d repos — the preview must be complete", len(recs), len(order))
	}
	byRepo := fleetByRepo(t, recs, dirs)
	for _, s := range fleetShapes() {
		if got := out.codes[s.name]; got != s.wantExit {
			t.Errorf("%s: dry-run exit %d, want %d (same verdict as the real run)\nstderr: %s",
				s.name, got, s.wantExit, out.stderr[s.name])
		}
		if rec := byRepo[s.name]; rec.Status != s.wantStatus {
			t.Errorf("%s: dry-run status %q, want %q", s.name, rec.Status, s.wantStatus)
		}
	}

	// Every repo that would converge must leave a PR body to review.
	for _, name := range order {
		if out.codes[name] != cli.ExitOK {
			continue
		}
		b, err := os.ReadFile(summaries[name])
		if err != nil {
			t.Errorf("%s: --summary-out wrote nothing under --dry-run: %v", name, err)
			continue
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			t.Errorf("%s: --summary-out is empty", name)
		}
	}
}

// SPEC R-19/R-22/R-24: the facts the copy-paste fleet script in docs/FLEET.md
// depends on, pinned one by one. Every assertion here corresponds to a line of
// that script; if this test fails, someone who pasted the script gets a halted
// rollout, a silent no-op, or an empty PR body.
func TestFleet_DocumentedScriptContract(t *testing.T) {
	t.Parallel()
	dirs, order := fleetBuild(t, "plain", "correct", "no-file", "refuse")
	policy := fleetPolicyFile(t, policyWithCreate(t, fleetPolicySrc))

	// Line 1: `codeowners-tool check --policy policy.json` under `set -euo
	// pipefail`. A valid policy MUST exit 0 — a "nothing to do" 1 would abort
	// the script before the first clone.
	t.Run("check exits 0 on a good policy and never 1", func(t *testing.T) {
		code, _, errOut := runCLI(t, "check", "--policy", policy)
		if code == cli.ExitNoOp {
			t.Fatal("check returned 1 on a valid policy: under `set -e` that halts the run before repo 0")
		}
		if code != cli.ExitOK {
			t.Fatalf("check: exit %d, want 0\nstderr: %s", code, errOut)
		}
	})

	t.Run("check exits 3 on a broken policy", func(t *testing.T) {
		broken := fleetPolicyFile(t, fleetBrokenPolicySrc)
		if code, _, _ := runCLI(t, "check", "--policy", broken); code != cli.ExitInvalid {
			t.Fatalf("check on a broken policy: exit %d, want 3", code)
		}
	})

	// `case $code in 0) ;; 2) ... ;; *) exit "$code" ;; esac` — the catch-all
	// arm halts the rollout, so anything sync can return that is not 0 or 2
	// must genuinely be a reason to stop.
	out := fleetRunPolicy(t, dirs, order, policy)
	t.Run("sync returns only 0, 2 or 3", func(t *testing.T) {
		for _, name := range order {
			switch out.codes[name] {
			case cli.ExitOK, cli.ExitRefused, cli.ExitInvalid:
			case -1:
				t.Errorf("%s: the loop never reached it (halted at %q)", name, out.halted)
			default:
				t.Errorf("%s: sync returned %d — the script's `*)` arm would halt the whole rollout\nstderr: %s",
					name, out.codes[name], out.stderr[name])
			}
		}
		if got := out.codes["correct"]; got != cli.ExitOK {
			t.Errorf("already-correct repo: exit %d, want 0 — never 1, which is the tax this contract removes", got)
		}
	})

	// `2) echo "$repo" >> needs-human` — recoverable, and the loop keeps going.
	t.Run("exit 2 is recoverable and the loop continues", func(t *testing.T) {
		if out.halted != "" {
			t.Fatalf("loop halted at %q", out.halted)
		}
		if !reflect.DeepEqual(out.processed, order) {
			t.Fatalf("processed %v, want %v", out.processed, order)
		}
		var needsHuman []string
		for _, name := range order {
			if out.codes[name] == cli.ExitRefused {
				needsHuman = append(needsHuman, name)
			}
		}
		if !reflect.DeepEqual(needsHuman, []string{"refuse"}) {
			t.Errorf("needs-human = %v, want exactly [refuse]", needsHuman)
		}
	})

	// `>> results.jsonl` then `jq -s` at the end: one line per repo, refusals
	// included, or the counts do not add up to the fleet.
	t.Run("results.jsonl is one object per repo", func(t *testing.T) {
		recs := fleetRecords(t, out.jsonl)
		if len(recs) != len(order) {
			t.Fatalf("%d records for %d repos", len(recs), len(order))
		}
		total := 0
		for _, n := range fleetGroupByStatus(recs) {
			total += n
		}
		if total != len(order) {
			t.Errorf("group_by(.status) totals %d, want %d", total, len(order))
		}
	})

	// `--summary-out "bodies/${repo//\//__}.md"` — an absolute path outside
	// the clone, written even under --dry-run, which is the fleet preview.
	t.Run("summary-out honors its path under dry-run", func(t *testing.T) {
		bodies := t.TempDir()
		path := filepath.Join(bodies, "org__plain.md")
		code, _, errOut := runCLI(t, "sync", "--repo", dirs["plain"], "--policy", policy,
			"--dry-run", "--format", "json", "--summary-out", path)
		if code != cli.ExitOK {
			t.Fatalf("dry-run sync: exit %d\nstderr: %s", code, errOut)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("--summary-out must write exactly the path it was given: %v", err)
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			t.Fatal("--summary-out wrote an empty file: there is nothing to review")
		}
		// And nothing landed inside the clone, where `git add -A` would
		// commit it.
		if _, err := os.Stat(filepath.Join(dirs["plain"], filepath.Base(path))); err == nil {
			t.Error("summary must not be written inside the clone")
		}
	})
}
