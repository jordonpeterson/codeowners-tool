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

// ---------------------------------------------------------------------------
// The three questions a rollout is actually asked.
//
// rollout_test.go covers the mechanics around a wave — clone shapes, the
// commit, the CI gate. This file covers what the wave DOES to a repository,
// under the three headings an operator gets asked about:
//
//	create   — a repo with no CODEOWNERS gets its first one
//	add      — a repo that already has one gains an owner, losing nobody
//	restrain — a repo that needs nothing gets nothing
//
// The third is the one with teeth. CODEOWNERS is not free: every rule adds
// required reviewers, blocks merges and pages people, so a rollout that writes
// where it did not have to is not a tidy default, it is an incident with a
// hundred repos in it. Several tests below assert the ABSENCE of a file, of a
// rule, of an owner, or of a JSON key.
// ---------------------------------------------------------------------------

// scenarioSync runs one sync and returns the parsed record with the exit code.
func scenarioSync(t *testing.T, args ...string) (cli.SyncRecord, int, string) {
	t.Helper()
	return rolloutSync(t, args...)
}

// scenarioBothViews runs ONE sync and returns the record twice: decoded, and
// as a raw key map — the only way to tell an ABSENT key from a present-but-
// empty one, which is the distinction the fleet's `// empty` guard rests on.
//
// One invocation, deliberately. Running sync a second time to collect the
// other view makes every assertion describe a re-run: the first call converges
// the repo and the second reports `unchanged`, so a test asking whether an
// APPLIED record names its file silently examines an unchanged one and fails
// for a reason that has nothing to do with the property under test.
func scenarioBothViews(t *testing.T, args ...string) (cli.SyncRecord, map[string]json.RawMessage, int) {
	t.Helper()
	code, stdout, errOut := runCLI(t, append([]string{"sync", "--format", "json"}, args...)...)
	var rec cli.SyncRecord
	if err := json.Unmarshal([]byte(stdout), &rec); err != nil {
		t.Fatalf("record is not JSON: %v\nraw: %s\nstderr: %s", err, stdout, errOut)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("record is not a JSON object: %v\nraw: %s", err, stdout)
	}
	return rec, m, code
}

// scenarioOwners resolves one path's owners from a committed snapshot, and
// reports which of the three states it is in: owned, matched-but-ownerless
// (S-9), or matched by nothing at all.
func scenarioOwners(t *testing.T, repo, path string) (owners []string, matched bool) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "snap.json")
	if code, _, errOut := runCLI(t, "snapshot", "--repo", repo, "--out", out); code != cli.ExitOK {
		t.Fatalf("snapshot: exit %d\nstderr: %s", code, errOut)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Ownership map[string]json.RawMessage `json:"ownership"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	raw, ok := snap.Ownership[path]
	if !ok {
		t.Fatalf("snapshot has no entry for %q", path)
	}
	if string(raw) == "null" {
		return nil, false
	}
	if err := json.Unmarshal(raw, &owners); err != nil {
		t.Fatal(err)
	}
	return owners, true
}

// scenarioRules returns the rule lines of a repo's CODEOWNERS in file order.
func scenarioRules(t *testing.T, repo, rel string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return fleetRuleLines(string(b))
}

// scenarioBaseline is the policy a greenfield wave runs: a catch-all nobody
// argues with, two claimed directories, and a declared rule for a directory a
// repo may not have yet.
const scenarioBaseline = `{
  "version": 1,
  "name": "org baseline ownership",
  "ops": [
    "add_owner(*, @org/everyone)",
    { "id": "api",  "op": "add_owner(/services/api/, @org/api-team)", "on_zero_match": "skip" },
    { "id": "docs", "op": "add_owner(/docs/, @org/docs-team)",        "on_zero_match": "skip" },
    { "id": "ci",   "op": "add_owner(/.github/workflows/, @org/ci)",  "on_zero_match": "declare" }
  ]
}`

// SPEC R-23/R-24 (create): the first CODEOWNERS a repo ever gets contains
// exactly the rules the policy justifies, in a deterministic order, and
// nothing else.
//
// A hundred near-identical PRs are only reviewable if the artifact is
// identical for identical input — so the ordering is asserted as a guarantee
// here rather than left as an accident of iteration order, and the file is
// pinned to rule lines only: no header, no provenance comment, no timestamp.
// A generated header cannot be written once and left alone forever (the next
// wave uses a different policy, the next release a different version), and a
// tool that rewrites comments in a file it otherwise never reformats has given
// up the property the rest of this suite exists to defend.
func TestScenario_CreateWritesExactlyWhatThePolicyJustifies(t *testing.T) {
	t.Parallel()
	policy := rolloutPolicy(t, scenarioBaseline)
	files := map[string]string{
		"README.md":                "# readme\n",
		"docs/guide.md":            "# guide\n",
		"services/api/main.go":     "package api\n",
		"services/web/app.ts":      "//\n",
		".github/workflows/ci.yml": "on: push\n",
	}

	var shas []string
	var dir string
	for i := 0; i < 3; i++ {
		dir = initRepo(t, files)
		rec, code, errOut := scenarioSync(t, "--repo", dir, "--policy", policy, "--create")
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
		}
		if !rec.Created || rec.CodeownersPath != ".github/CODEOWNERS" {
			t.Fatalf("created=%v path=%q, want true/.github/CODEOWNERS", rec.Created, rec.CodeownersPath)
		}
		b, err := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
		if err != nil {
			t.Fatal(err)
		}
		shas = append(shas, string(b))
	}
	if shas[0] != shas[1] || shas[1] != shas[2] {
		t.Errorf("three identical repos produced three different files — a fleet of near-identical PRs is only reviewable if this is deterministic:\n%q\n%q\n%q",
			shas[0], shas[1], shas[2])
	}

	content := shas[0]
	if !strings.HasSuffix(content, "\n") {
		t.Errorf("created file has no trailing newline: %q", content)
	}
	if strings.Contains(content, "\r") {
		t.Errorf("created file is not LF-only: %q", content)
	}
	if strings.Contains(content, "#") {
		t.Errorf("created file carries a comment it was never asked to write; every line must be derivable from an op:\n%s", content)
	}
	rules := fleetRuleLines(content)
	// The order itself, not merely "the same order twice": a deterministic but
	// reshuffled file would satisfy the three-way byte comparison above while
	// changing what every reviewer in the fleet sees. The catch-all leads, and
	// the narrowing rules follow it — which is also what keeps INV-2 true.
	wantRules := []string{
		"* @org/everyone",
		"/.github/workflows/ @org/everyone @org/ci",
		"/docs/ @org/everyone @org/docs-team",
		"/services/api/ @org/everyone @org/api-team",
	}
	if !reflect.DeepEqual(rules, wantRules) {
		t.Fatalf("created file's rules:\n got: %#v\nwant: %#v", rules, wantRules)
	}
	for _, r := range rules {
		if !strings.Contains(r, "@org/everyone") {
			t.Errorf("rule %q dropped the catch-all owner — add_owner co-owns, and a narrowing rule that omits it silently performs set_owners", r)
		}
	}
	// Nobody invented: every owner in the file was named in the policy.
	for _, r := range rules {
		for _, tok := range strings.Fields(r)[1:] {
			switch tok {
			case "@org/everyone", "@org/api-team", "@org/docs-team", "@org/ci":
			default:
				t.Errorf("rule %q names %q, which the policy never mentions", r, tok)
			}
		}
	}
	if _, code, _ := scenarioSync(t, "--repo", dir, "--policy", policy, "--create"); code != cli.ExitOK {
		t.Fatal("second run must converge")
	}
}

// SPEC R-21/R-24 (create, partial applicability): one policy over repos that
// have only some of the directories it names writes rules only for what is
// there, and says which ops reached nothing.
//
// This is what stops a fleet from needing a policy per repo shape. The repo
// below has no /docs/ and no /services/api/; the file it gets must contain no
// rule for either, and the record must say `skipped` with a reason, because a
// count alone cannot tell "this repo has no Terraform" from "the policy's
// prefix is misspelled and reached nothing anywhere".
func TestScenario_CreateSkipsWhatTheRepoDoesNotHave(t *testing.T) {
	t.Parallel()
	policy := rolloutPolicy(t, scenarioBaseline)
	dir := initRepo(t, map[string]string{
		"README.md":  "# readme\n",
		"src/lib.ts": "//\n",
	})

	rec, code, errOut := scenarioSync(t, "--repo", dir, "--policy", policy, "--create")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
	}
	if rec.OpsApplied != 2 || rec.OpsSkipped != 2 {
		t.Errorf("ops_applied=%d ops_skipped=%d, want 2/2 (catch-all + declared ci applied; api and docs skipped)",
			rec.OpsApplied, rec.OpsSkipped)
	}
	for _, id := range []string{"api", "docs"} {
		r := fleetOpResult(t, rec, id)
		if r.Status != "skipped" {
			t.Errorf("op %s: status %q, want skipped", id, r.Status)
		}
		if r.Reason == "" {
			t.Errorf("op %s: a skip with no reason cannot be told from a typo that reached nothing anywhere", id)
		}
	}
	rules := scenarioRules(t, dir, rec.CodeownersPath)
	for _, r := range rules {
		if strings.HasPrefix(r, "/docs/") || strings.HasPrefix(r, "/services/api/") {
			t.Errorf("wrote a rule for a directory this repo does not have: %q", r)
		}
	}
	if len(rules) != 2 {
		t.Errorf("file has %d rules, want 2 (the catch-all and the declared workflows rule):\n%v", len(rules), rules)
	}
}

// SPEC R-23/R-24 (restraint): `--create` is permission to create, not an
// instruction to. A repo the policy has nothing to say about ends up with no
// CODEOWNERS, no `.github/` directory, and no `codeowners_path` in its record.
//
// The failure this prevents is invisible in a fleet summary: a hundred empty
// or near-empty files all reporting exit 0. An empty CODEOWNERS is worse than
// none, because the obvious question afterwards — "which repos still need
// ownership?" — is answered by "which repos have no CODEOWNERS file", and an
// empty one answers *yes, done* forever. The record must therefore be
// indistinguishable, to the commit step, from a repo that was never touched.
func TestScenario_CreateWritesNothingWhenThereIsNothingToSay(t *testing.T) {
	t.Parallel()
	policy := rolloutPolicy(t, `{
  "version": 1,
  "ops": [
    { "op": "add_owner(/services/api/, @org/api-team)", "on_zero_match": "skip" },
    { "op": "add_owner(**/*.tf, @org/infra)",           "on_zero_match": "skip" },
    { "op": "add_owner(/charts/, @org/platform)",       "on_zero_match": "skip" }
  ]
}`)
	dir := initRepo(t, map[string]string{
		"README.md":       "# readme\n",
		"site/index.html": "<!-- -->\n",
	})

	rec, raw, code := scenarioBothViews(t, "--repo", dir, "--policy", policy, "--create")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0 — this repo does not need a human, the policy simply has nothing to say about it", code)
	}
	if rec.Status != cli.StatusSkipped {
		t.Errorf("status %q, want %q — `unchanged` would assert the file already says what you asked, and there is no file",
			rec.Status, cli.StatusSkipped)
	}
	if rec.Created {
		t.Error("created must be false")
	}
	if _, present := raw["codeowners_path"]; present {
		t.Errorf("codeowners_path is present on a run that wrote nothing: %s — the documented `jq -r '.codeowners_path // empty'` guard then stages a path that does not exist, and `git add` fails the whole rollout", raw["codeowners_path"])
	}
	for _, rel := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
			t.Errorf("a CODEOWNERS appeared at %s; --create is permission, not instruction", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".github")); err == nil {
		t.Error("a .github/ directory was created for a file that was never written")
	}
}

// SPEC R-24 (codeowners_path): the key is present exactly when this run
// changed the file — or, under --dry-run, would have. It is absent on every
// other outcome.
//
// The field is a staging instruction, and the documented fleet loop runs
// `git add "$(jq -r '.codeowners_path // empty' …)"`. Emitting it whenever a
// file was merely CHOSEN broke that on the two commonest fleet outcomes: an
// already-correct repo named a file with no diff, `git commit` then failed
// with "nothing to commit", and a `set -euo pipefail` rollout died at whichever
// repo happened to be converged — a failure with nothing to do with that repo.
func TestScenario_CodeownersPathMeansThereIsSomethingToStage(t *testing.T) {
	t.Parallel()
	tree := map[string]string{"services/api/main.go": "package api\n"}
	withFile := func(content string) map[string]string {
		f := map[string]string{".github/CODEOWNERS": content}
		for k, v := range tree {
			f[k] = v
		}
		return f
	}

	cases := []struct {
		name       string
		files      map[string]string
		args       []string
		wantStatus string
		wantKey    bool
	}{
		{
			name: "applied", files: withFile("* @org/eng\n"),
			args:       []string{"--op", "add_owner(/services/api/, @org/api-team)"},
			wantStatus: cli.StatusApplied, wantKey: true,
		},
		{
			// A dry run that WOULD write still names the destination: that is
			// the whole point of a fleet preview.
			name: "applied under dry-run", files: withFile("* @org/eng\n"),
			args:       []string{"--op", "add_owner(/services/api/, @org/api-team)", "--dry-run"},
			wantStatus: cli.StatusApplied, wantKey: true,
		},
		{
			name: "unchanged", files: withFile("* @org/eng\n/services/api/ @org/eng @org/api-team\n"),
			args:       []string{"--op", "add_owner(/services/api/, @org/api-team)"},
			wantStatus: cli.StatusUnchanged, wantKey: false,
		},
		{
			name: "refused", files: map[string]string{
				".github/CODEOWNERS": "infra/ @org/infra-legacy\n",
				"infra/main.tf":      "# tf\n",
				"infra/README.md":    "# infra\n",
			},
			args:       []string{"--op", "add_owner(**/*.tf, @org/infra)"},
			wantStatus: cli.StatusRefused, wantKey: false,
		},
	}
	// The `skipped` outcome is not in this table: `sync --op` carries no
	// on_zero_match knob, so it needs a policy. It is asserted, on a raw key
	// map, in TestScenario_CreateWritesNothingWhenThereIsNothingToSay.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := initRepo(t, tc.files)
			args := append([]string{"--repo", dir}, tc.args...)
			rec, raw, _ := scenarioBothViews(t, args...)
			if rec.Status != tc.wantStatus {
				t.Fatalf("status %q, want %q", rec.Status, tc.wantStatus)
			}
			_, present := raw["codeowners_path"]
			if present != tc.wantKey {
				t.Errorf("codeowners_path present=%v, want %v on a %q record", present, tc.wantKey, tc.wantStatus)
			}
		})
	}

	// The refusal has to say which file it was about even without the field —
	// a refusal in a repo whose ownership lives in docs/ is a different triage
	// job from one in .github/.
	dir := initRepo(t, map[string]string{
		"docs/CODEOWNERS": "infra/ @org/infra-legacy\n",
		"infra/main.tf":   "# tf\n",
		"infra/README.md": "# infra\n",
	})
	rec, code, _ := scenarioSync(t, "--repo", dir, "--op", "add_owner(**/*.tf, @org/infra)")
	if code != cli.ExitRefused {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(rec.Error, "docs/CODEOWNERS") {
		t.Errorf("the refusal does not name the file it was about: %q", rec.Error)
	}
}

// SPEC R-24: the documented fleet commit guard skips a converged repo.
//
// This is the published recipe, run against the outcome it meets most often.
// It reproduced a live bug: the guard passed, `git add` staged an unmodified
// file, and `git commit` exited nonzero with "nothing to commit" — which under
// the `set -euo pipefail` script in docs/FLEET.md ends the rollout.
func TestScenario_FleetCommitGuardSkipsAConvergedRepo(t *testing.T) {
	t.Parallel()
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/eng\n/services/api/ @org/eng @org/api-team\n",
		"services/api/main.go": "package api\n",
	})
	_, raw, _ := scenarioBothViews(t, "--repo", dir, "--op", "add_owner(/services/api/, @org/api-team)")
	if _, present := raw["codeowners_path"]; present {
		t.Fatalf("an already-correct repo named a file to stage (%s); the documented guard would `git add` an unmodified path and the commit that follows fails the run",
			raw["codeowners_path"])
	}
	if got := string(raw["status"]); got != `"unchanged"` {
		t.Fatalf("status = %s, want \"unchanged\"", got)
	}
}

// SPEC INV-1/INV-2 (add): adding an owner to a subtree keeps every existing
// owner, reaches the deeper rules inside the scope, and grants nothing outside
// it — including leaving unowned paths unowned.
//
// The trap this exists for is that appending `/auth/ @org/security` to the end
// of a file *replaces* the owners of /auth/. The second trap is the deeper
// rule: `/auth/tokens/` sits inside the scope, and under last-match-wins an
// edit that touches only `/auth/` leaves the tokens directory without the new
// owner while reporting success.
func TestScenario_AddOwnerKeepsEveryoneAndReachesDeeperRules(t *testing.T) {
	t.Parallel()
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "*                 @org/everyone\n" +
			"/auth/            @org/identity-team\n" +
			"/auth/tokens/     @org/identity-team @org/crypto\n",
		"README.md":            "# readme\n",
		"auth/login.go":        "package auth\n",
		"auth/tokens/jwt.go":   "package tokens\n",
		"services/api/main.go": "package api\n",
	})

	rec, code, errOut := scenarioSync(t, "--repo", dir, "--op", "add_owner(/auth/, @org/security)")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
	}
	want := "*                 @org/everyone\n" +
		"/auth/            @org/identity-team @org/security\n" +
		"/auth/tokens/     @org/identity-team @org/crypto @org/security\n"
	got, err := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("file:\n got: %q\nwant: %q", got, want)
	}
	if rec.PathsChanged != 2 {
		t.Errorf("paths_changed = %d, want 2 (auth/login.go and auth/tokens/jwt.go)", rec.PathsChanged)
	}

	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "add security")
	for path, wantOwners := range map[string][]string{
		"auth/login.go":        {"@org/identity-team", "@org/security"},
		"auth/tokens/jwt.go":   {"@org/identity-team", "@org/crypto", "@org/security"},
		"README.md":            {"@org/everyone"},
		"services/api/main.go": {"@org/everyone"},
	} {
		owners, matched := scenarioOwners(t, dir, path)
		if !matched || strings.Join(owners, " ") != strings.Join(wantOwners, " ") {
			t.Errorf("%s resolves to %v (matched=%v), want %v", path, owners, matched, wantOwners)
		}
	}
}

// SPEC INV-1 (add): an owner already named elsewhere in the file still has to
// be added to the rule that governs the scope.
//
// "They're already listed" is the eyeball check that leaves /auth/ unprotected:
// @org/security owns README.md via the catch-all and owns nothing under /auth/,
// because the narrower rule wins outright. The run must report `applied`, touch
// exactly the one rule that is wrong, and leave the rule that is already
// correct byte-identical.
func TestScenario_OwnerInTheFileIsNotOwnershipOfTheScope(t *testing.T) {
	t.Parallel()
	before := "*                @org/everyone @org/security\n" +
		"/auth/           @org/identity-team\n" +
		"/auth/tokens/    @org/identity-team @org/security\n"
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": before,
		"README.md":          "# readme\n",
		"auth/login.go":      "package auth\n",
		"auth/tokens/jwt.go": "package tokens\n",
	})

	rec, code, errOut := scenarioSync(t, "--repo", dir, "--op", "add_owner(/auth/, @org/security)")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
	}
	if rec.Status != cli.StatusApplied {
		t.Fatalf("status %q, want applied — file-level presence is not ownership, and reporting `unchanged` here is the whole failure mode", rec.Status)
	}
	if len(rec.Changes) != 1 {
		t.Errorf("%d line changes, want exactly 1 — the already-correct rule must not be rewritten", len(rec.Changes))
	}
	got, err := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	want := "*                @org/everyone @org/security\n" +
		"/auth/           @org/identity-team @org/security\n" +
		"/auth/tokens/    @org/identity-team @org/security\n"
	if string(got) != want {
		t.Errorf("file:\n got: %q\nwant: %q", got, want)
	}
	for _, line := range fleetRuleLines(string(got)) {
		if strings.Count(line, "@org/security") > 1 {
			t.Errorf("owner listed twice on %q", line)
		}
	}
}

// SPEC INV-2 (add, restraint): a wave that claims two directories leaves every
// other path unowned, and the two ways of having no owner stay distinct.
//
// `null` in a snapshot means no rule matched — a gap nobody has addressed.
// `[]` means a rule matched and deliberately un-owns the path (S-9), which is
// a decision someone made and defended in review. Collapsing them would hide
// the difference between "we chose to leave vendored code unowned" and "nobody
// has looked at this yet", which is the distinction a coverage report is for.
func TestScenario_UnclaimedPathsStayUnownedAndTheTwoKindsDiffer(t *testing.T) {
	t.Parallel()
	dir := initRepo(t, map[string]string{
		"README.md":            "# readme\n",
		"scripts/release.sh":   "#!/bin/sh\n",
		"vendor/lib/x.go":      "package lib\n",
		"services/api/main.go": "package api\n",
		"services/web/app.ts":  "//\n",
	})
	policy := rolloutPolicy(t, `{
  "version": 1,
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    "add_owner(/services/web/, @org/web-team)"
  ]
}`)

	rec, code, errOut := scenarioSync(t, "--repo", dir, "--policy", policy, "--create")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
	}
	if rec.PathsChanged != 2 {
		t.Errorf("paths_changed = %d, want 2", rec.PathsChanged)
	}
	rules := scenarioRules(t, dir, rec.CodeownersPath)
	if len(rules) != 2 {
		t.Fatalf("file has %d rules, want 2:\n%v", len(rules), rules)
	}
	for _, r := range rules {
		if strings.HasPrefix(r, "*") {
			t.Errorf("a catch-all was synthesized to make coverage look complete: %q", r)
		}
	}

	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "claimed dirs")
	for _, p := range []string{"README.md", "scripts/release.sh", "vendor/lib/x.go"} {
		owners, matched := scenarioOwners(t, dir, p)
		if matched {
			t.Errorf("%s resolves to %v — nobody claimed it, so it must match no rule at all", p, owners)
		}
	}

	// The other kind of unowned: a rule that matches and deliberately owns
	// nobody. This is what un-owning vendored code looks like, and it must be
	// distinguishable from the gap above.
	if code, _, errOut := runCLI(t, "sync", "--repo", dir, "--op", "set_owners(/vendor/, [])"); code != cli.ExitOK {
		t.Fatalf("un-owning vendor: exit %d\nstderr: %s", code, errOut)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "unown vendor")
	owners, matched := scenarioOwners(t, dir, "vendor/lib/x.go")
	if !matched {
		t.Error("vendor/lib/x.go matches no rule; a deliberate un-owning must be visible as a matched rule with zero owners (S-9)")
	}
	if len(owners) != 0 {
		t.Errorf("vendor/lib/x.go resolves to %v, want zero owners", owners)
	}
	if _, stillMatched := scenarioOwners(t, dir, "scripts/release.sh"); stillMatched {
		t.Error("un-owning vendor/ changed an unrelated path's matched state (INV-2)")
	}
}

// SPEC INV-5 (add): a hand-curated file changes by exactly one line.
//
// Column alignment, tabs, section comments, an inline comment carrying a
// warning, and a deliberate ordering are all things a maintainer chose. A tool
// that re-flows any of them turns a one-line review into a whole-file review,
// a hundred times over.
func TestScenario_HandCuratedFileChangesExactlyOneLine(t *testing.T) {
	t.Parallel()
	before := "# ---------------------------------------------------------------\n" +
		"# Ownership map. Order matters: the vendor override MUST stay last.\n" +
		"# ---------------------------------------------------------------\n" +
		"\n" +
		"*                       @org/everyone\n" +
		"\n" +
		"# --- product surfaces ---\n" +
		"/auth/                  @org/identity-team\n" +
		"/billing/\t\t@org/payments          # PCI scope - do not widen\n" +
		"/docs/                  @org/docs-team\n" +
		"\n" +
		"# --- vendored code: nobody reviews this, keep last ---\n" +
		"/third_party/           @org/everyone\n"
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":  before,
		"README.md":           "# readme\n",
		"auth/login.go":       "package auth\n",
		"billing/charge.go":   "package billing\n",
		"docs/guide.md":       "# guide\n",
		"third_party/lib/x.c": "/* */\n",
	})

	if _, code, errOut := scenarioSync(t, "--repo", dir, "--op", "add_owner(/auth/, @org/security)"); code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	beforeLines, afterLines := strings.Split(before, "\n"), strings.Split(string(got), "\n")
	if len(beforeLines) != len(afterLines) {
		t.Fatalf("line count changed: %d → %d\n%s", len(beforeLines), len(afterLines), got)
	}
	var changed []int
	for i := range beforeLines {
		if beforeLines[i] != afterLines[i] {
			changed = append(changed, i+1)
		}
	}
	if len(changed) != 1 {
		t.Fatalf("lines %v changed, want exactly one\n got:\n%s", changed, got)
	}
	if afterLines[changed[0]-1] != "/auth/                  @org/identity-team @org/security" {
		t.Errorf("amended line lost its alignment: %q", afterLines[changed[0]-1])
	}
	if !strings.Contains(string(got), "/billing/\t\t@org/payments          # PCI scope - do not widen") {
		t.Error("the tab separator or the inline comment on an untouched line was rewritten")
	}
	rules := fleetRuleLines(string(got))
	if rules[len(rules)-1] != "/third_party/           @org/everyone" {
		t.Errorf("the deliberately-last rule moved; last is %q", rules[len(rules)-1])
	}
}

// SPEC R-24 (rename): a rename substitutes the identifier IN PLACE, leaving
// every other owner where it was, and collapses rather than duplicates when the
// new name is already on the line.
//
// `rename_owner` is documented as pure identifier substitution — the one op
// safe to read as a text diff. Removing the old name and appending the new one
// preserves the owner SET and GitHub cares about nothing else, but it permutes
// every line that lists the renamed team alongside anyone else, and a reviewer
// of a hundred reorg PRs then has to work out that a reordering means nothing.
func TestScenario_RenameSubstitutesInPlace(t *testing.T) {
	t.Parallel()
	before := "# @org/acq owns the pipeline; see ADR-14\n" +
		"*                  @org/everyone\n" +
		"/pipeline/         @org/acq\n" +
		"/pipeline/ingest/  @org/acq @org/acq-infra\n" +
		"/warehouse/        @org/acq-infra\n" +
		"/collapse/         @org/acq @org/platform-data\n"
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS":   before,
		"pipeline/p.go":        "package p\n",
		"pipeline/ingest/i.go": "package i\n",
		"warehouse/w.go":       "package w\n",
		"collapse/c.go":        "package c\n",
	})

	rec, code, errOut := scenarioSync(t, "--repo", dir, "--op", "rename_owner(@org/acq, @org/platform-data)")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# @org/acq owns the pipeline; see ADR-14\n" +
		"*                  @org/everyone\n" +
		"/pipeline/         @org/platform-data\n" +
		"/pipeline/ingest/  @org/platform-data @org/acq-infra\n" +
		"/warehouse/        @org/acq-infra\n" +
		"/collapse/         @org/platform-data\n"
	if string(got) != want {
		t.Errorf("file:\n got: %q\nwant: %q", got, want)
	}

	// The collapse is a real change of review requirements — two approving
	// teams became one — so it must show up as a changed owner list, not be
	// waved through as "just a rename".
	var sawCollapse bool
	for _, c := range rec.Changes {
		if c.Pattern == "/collapse/" {
			sawCollapse = true
			if len(c.NewOwners) != 1 || c.NewOwners[0] != "@org/platform-data" {
				t.Errorf("/collapse/ new owners = %v, want exactly one", c.NewOwners)
			}
		}
	}
	if !sawCollapse {
		t.Error("the collapsing line is missing from changes")
	}

	// The comment is not rewritten — editing prose is the one thing this op
	// cannot prove — but the run says so, in the record and in the PR body.
	body := filepath.Join(t.TempDir(), "body.md")
	// The two handles sit on DIFFERENT comment lines on purpose. With both on
	// one line, commentLinesNaming reports that line once either way, so the
	// count below stays 1 even with the whole-identifier guard removed — the
	// fixture, not the assertion, is what gives this teeth.
	dir2 := initRepo(t, map[string]string{
		".github/CODEOWNERS": "# @org/acq owns this\n# @org/acq-infra is a different team\n/pipeline/ @org/acq\n",
		"pipeline/p.go":      "package p\n",
	})
	rec2, code2, _ := scenarioSync(t, "--repo", dir2, "--op", "rename_owner(@org/acq, @org/platform-data)", "--summary-out", body)
	if code2 != cli.ExitOK {
		t.Fatalf("exit %d, want 0", code2)
	}
	joined := strings.Join(rec2.Warnings, "\n")
	if !strings.Contains(joined, "line 1") || !strings.Contains(joined, "@org/acq") {
		t.Errorf("no warning naming the stale comment's line: %#v", rec2.Warnings)
	}
	if strings.Contains(joined, "line 2") {
		t.Errorf("the substring handle @org/acq-infra on line 2 was reported as stale; a rename of @org/acq must not match a longer identifier: %#v", rec2.Warnings)
	}
	if len(rec2.Warnings) != 1 {
		t.Errorf("want exactly one stale-comment warning, got %#v", rec2.Warnings)
	}
	b, err := os.ReadFile(body)
	if err != nil {
		t.Fatal(err)
	}
	// Assert the SECTION, not the handle: the Ops table renders the op string
	// `rename_owner(@org/acq, …)`, so `Contains(body, "@org/acq")` passes with
	// the whole warnings section deleted from renderSummary.
	if !strings.Contains(string(b), "## Worth a look") {
		t.Errorf("the PR body has no warnings section — the PR is the one moment a human can fix the prose in the same commit:\n%s", b)
	}
	if !strings.Contains(string(b), "line 1 as this run leaves it: a comment still names") {
		t.Errorf("the warnings section does not carry the stale-comment warning itself:\n%s", b)
	}

	// A rename that matched nothing is a clean no-op and must not warn: a
	// hundred-repo rename would otherwise warn about repos it never touched.
	rec3, code3, _ := scenarioSync(t, "--repo", dir2, "--op", "rename_owner(@org/never-here, @org/x)")
	if code3 != cli.ExitOK || rec3.Status != cli.StatusUnchanged {
		t.Errorf("exit %d status %q, want 0/unchanged", code3, rec3.Status)
	}
	if len(rec3.Warnings) != 0 {
		t.Errorf("a rename that changed nothing warned: %#v", rec3.Warnings)
	}
}

// SPEC R-8/R-20 (restraint): a batch whose order changes the outcome is
// rejected from the op strings alone — the same verdict, exit 3, on every
// repository, whatever its tree looks like.
//
// `set_owners(*, [@org/everyone])` displaces and `add_owner(/services/api/,
// @org/api-team)` co-owns; run in either order they give services/api
// different owners, so the batch has no defined meaning. Deciding that against
// a tree made it a per-repo refusal: `check` passed the policy, every repo
// exited 2, and a hundred entries landed in `needs-human` for a defect that was
// in the policy the whole time. Whether a given repo happens to HAVE a
// services/api/ is not the question — a policy whose meaning depends on which
// files a repo contains is what exit 3 is for.
func TestScenario_OrderDependentBatchIsRejectedEverywhere(t *testing.T) {
	t.Parallel()
	conflicting := rolloutPolicy(t, `{
  "version": 1,
  "ops": ["set_owners(*, [@org/everyone])", "add_owner(/services/api/, @org/api-team)"]
}`)

	// check sees it with no repository at all.
	code, _, errOut := runCLI(t, "check", "--policy", conflicting)
	if code != cli.ExitInvalid {
		t.Fatalf("check: exit %d, want 3\nstderr: %s", code, errOut)
	}
	for _, want := range []string{"R-8", "set_owners(*, [@org/everyone])", "on its own first"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("check's error must contain %q, so the operator knows the remedy:\n%s", want, errOut)
		}
	}

	// Two repos with different trees, one of which cannot possibly overlap:
	// the verdict must be identical, or the fleet learns the policy is broken
	// one repo at a time.
	withAPI := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/eng\n",
		"services/api/main.go": "package api\n",
	})
	withoutAPI := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/eng\n",
		"lib/util.go":        "package lib\n",
	})
	for name, dir := range map[string]string{"with services/api": withAPI, "without services/api": withoutAPI} {
		code, stdout, _ := runCLI(t, "sync", "--repo", dir, "--policy", conflicting, "--format", "json")
		if code != cli.ExitInvalid {
			t.Errorf("%s: sync exit %d, want 3", name, code)
		}
		if stdout != "" {
			t.Errorf("%s: a policy error must emit no record, got %q", name, stdout)
		}
	}

	// The documented remedy converges, twice, at exit 0.
	if code, _, errOut := runCLI(t, "sync", "--repo", withAPI, "--op", "set_owners(*, [@org/everyone])"); code != cli.ExitOK {
		t.Fatalf("displacing op alone: exit %d\nstderr: %s", code, errOut)
	}
	if code, _, errOut := runCLI(t, "sync", "--repo", withAPI, "--op", "add_owner(/services/api/, @org/api-team)"); code != cli.ExitOK {
		t.Fatalf("narrower op second: exit %d\nstderr: %s", code, errOut)
	}

	// An overlap that is NOT provable from the patterns stays a per-repo
	// refusal at exit 2 — the fleet loop must still step over repo-shaped
	// problems rather than halting on them.
	treeOnly := rolloutPolicy(t, `{
  "version": 1,
  "ops": ["set_owners(**/*.tf, [@org/infra])", "add_owner(/infra/, @org/platform)"]
}`)
	if code, _, errOut := runCLI(t, "check", "--policy", treeOnly); code != cli.ExitOK {
		t.Fatalf("check on a policy whose overlap needs a tree: exit %d, want 0 — neither scope provably covers the other\nstderr: %s", code, errOut)
	}
	overlapping := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/eng\n",
		"infra/main.tf":      "# tf\n",
	})
	if code, _, _ := runCLI(t, "sync", "--repo", overlapping, "--policy", treeOnly); code != cli.ExitRefused {
		t.Errorf("tree-only overlap: exit %d, want 2 (per repo, loop continues)", code)
	}
}

// SPEC R-25 (restraint): an opt-in ceiling on how many paths one run may move,
// so "the tool put my team on 4000 files" is a decision somebody made rather
// than a diff nobody read.
//
// Over the ceiling the run refuses, writes nothing, and reports the count —
// the count IS the content of that refusal, so it is the one field a refused
// record keeps. It is exit 2, per repo: a ceiling trip is a fact about this
// repo's size, and a fleet records it and carries on. The ceiling lives in the
// policy file for a policy run, because "this wave touches dozens of files per
// repo, not thousands" is a claim about the intent that belongs in the artifact
// a reviewer approves — a ceiling in one shell line survives exactly as long as
// that shell line.
func TestScenario_BlastRadiusCeiling(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		".github/CODEOWNERS": "* @org/eng\n",
		"a/f.go":             "package a\n",
		"b/f.go":             "package b\n",
		"c/f.go":             "package c\n",
	}

	t.Run("over the ceiling refuses and writes nothing", func(t *testing.T) {
		dir := initRepo(t, files)
		before, _ := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
		summary := filepath.Join(t.TempDir(), "body.md")
		rec, raw, code := scenarioBothViews(t, "--repo", dir, "--op", "add_owner(*, @org/wide)",
			"--max-paths-changed", "2", "--summary-out", summary)
		if code != cli.ExitRefused {
			t.Fatalf("exit %d, want 2 — a ceiling trip is this repo's size, not a broken policy", code)
		}
		if rec.Status != cli.StatusRefused {
			t.Errorf("status %q, want refused (never a sixth status value: every existing group_by(.status) would grow a bucket nobody handles)", rec.Status)
		}
		if rec.PathsChanged != 4 {
			t.Errorf("paths_changed = %d, want 4 — a record that refuses on a number and omits the number is useless", rec.PathsChanged)
		}
		if rec.OpsApplied != 0 || len(rec.Changes) != 0 {
			t.Errorf("ops_applied=%d changes=%d, want 0/0 — no byte moved, so nothing may claim it applied", rec.OpsApplied, len(rec.Changes))
		}
		if _, present := raw["codeowners_path"]; present {
			t.Error("codeowners_path present on a refusal — nothing was written, so there is nothing to stage")
		}
		// "2" alone would be satisfied by the "R-25" in the message, so the
		// ceiling is asserted with its unit attached.
		for _, want := range []string{"4 path(s)", "2-path ceiling", "--max-paths-changed"} {
			if !strings.Contains(rec.Error, want) {
				t.Errorf("the refusal must name the count, the ceiling and where the ceiling came from; %q missing from %q", want, rec.Error)
			}
		}
		after, _ := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
		if string(after) != string(before) {
			t.Errorf("the file changed:\n before %q\n after  %q", before, after)
		}
		// The operator decides whether to raise the ceiling by reading what it
		// would have done, so the artifacts still have to be produced.
		if b, err := os.ReadFile(summary); err != nil || !strings.Contains(string(b), "R-25") {
			t.Errorf("--summary-out must still emit and explain the refusal: %v\n%s", err, b)
		}
	})

	t.Run("exactly at the ceiling proceeds", func(t *testing.T) {
		// The value an operator sets after reading "would change the owners of
		// 4 path(s)" and deciding the number is right. A `>=` here would refuse
		// them forever, and nothing else in this test distinguishes the two.
		dir := initRepo(t, files)
		rec, code, errOut := scenarioSync(t, "--repo", dir, "--op", "add_owner(*, @org/wide)", "--max-paths-changed", "4")
		if code != cli.ExitOK {
			t.Fatalf("4 paths against a ceiling of 4: exit %d, want 0 — the ceiling is a maximum, not a limit to stay under\nstderr: %s", code, errOut)
		}
		if rec.Status != cli.StatusApplied || rec.PathsChanged != 4 {
			t.Errorf("status %q paths_changed %d, want applied/4", rec.Status, rec.PathsChanged)
		}
	})

	t.Run("under the ceiling proceeds", func(t *testing.T) {
		dir := initRepo(t, files)
		rec, code, errOut := scenarioSync(t, "--repo", dir, "--op", "add_owner(*, @org/wide)", "--max-paths-changed", "99")
		if code != cli.ExitOK {
			t.Fatalf("exit %d, want 0\nstderr: %s", code, errOut)
		}
		if rec.Status != cli.StatusApplied {
			t.Errorf("status %q, want applied", rec.Status)
		}
	})

	t.Run("dry-run gives the same verdict", func(t *testing.T) {
		dir := initRepo(t, files)
		before, _ := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS"))
		rec, raw, code := scenarioBothViews(t, "--repo", dir, "--op", "add_owner(*, @org/wide)",
			"--max-paths-changed", "2", "--dry-run")
		if code != cli.ExitRefused {
			t.Errorf("dry-run exit %d, want 2 — a rehearsal that does not predict the performance is worse than none", code)
		}
		// The whole record has to match the real run's, not just the code: the
		// rehearsal is what the operator reads to decide about the ceiling.
		if rec.Status != cli.StatusRefused || rec.PathsChanged != 4 || rec.OpsApplied != 0 {
			t.Errorf("dry-run record: status %q paths_changed %d ops_applied %d, want refused/4/0",
				rec.Status, rec.PathsChanged, rec.OpsApplied)
		}
		if _, present := raw["codeowners_path"]; present {
			t.Error("dry-run refusal names a file to stage")
		}
		if after, _ := os.ReadFile(filepath.Join(dir, ".github", "CODEOWNERS")); string(after) != string(before) {
			t.Error("--dry-run wrote to the file")
		}
	})

	t.Run("the ceiling belongs to the policy, not the call site", func(t *testing.T) {
		dir := initRepo(t, files)
		policy := rolloutPolicy(t, `{"version":1,"max_paths_changed":2,"ops":["add_owner(*, @org/wide)"]}`)
		if code, _, errOut := runCLI(t, "check", "--policy", policy); code != cli.ExitOK {
			t.Fatalf("check: exit %d, want 0\nstderr: %s", code, errOut)
		}
		rec, code, _ := scenarioSync(t, "--repo", dir, "--policy", policy)
		if code != cli.ExitRefused {
			t.Fatalf("policy ceiling: exit %d, want 2", code)
		}
		if !strings.Contains(rec.Error, "max_paths_changed") {
			t.Errorf("the refusal must say the ceiling came from the policy file, or the operator cannot tell which knob to turn: %q", rec.Error)
		}
		// Mirrors --on-empty: a flag that could override the reviewed file
		// would let one call site quietly loosen it.
		if code, _, _ := runCLI(t, "sync", "--repo", dir, "--policy", policy, "--max-paths-changed", "99"); code != cli.ExitInvalid {
			t.Errorf("--max-paths-changed with --policy: exit %d, want 3", code)
		}
	})

	t.Run("a nonsense ceiling is a policy error", func(t *testing.T) {
		policy := rolloutPolicy(t, `{"version":1,"max_paths_changed":-3,"ops":["add_owner(*, @org/x)"]}`)
		code, _, errOut := runCLI(t, "check", "--policy", policy)
		if code != cli.ExitInvalid {
			t.Fatalf("check: exit %d, want 3", code)
		}
		if !strings.Contains(errOut, "max_paths_changed") {
			t.Errorf("the error must quote the field: %s", errOut)
		}
	})
}

// SPEC R-8/R-20: the static rejection is scoped to ops that must apply. An op
// carrying `on_zero_match: skip` or `declare` is left to the tree, at exit 2.
//
// The first cut of the static check read only the op kinds and scopes, and
// refused `set_owners(*, …)` next to a `skip`-guarded narrower op — the shape
// docs/FLEET.md recommends ("*if* this repo has Terraform"). That turned a
// hundred exit-0 repos into a halted rollout, and it broke the rule the exit
// code rests on: exit 3 is reachable only from facts independent of which repo
// you are standing in. With `skip` the batch is order-dependent exactly in the
// repos that have the scope, which is the exit-2 class.
//
// Two `require` ops still earn exit 3, and the argument is that the policy
// cannot converge ANYWHERE: a repo with the narrower scope refuses on the
// overlap, and a repo without it refuses on the zero match (R-5).
func TestScenario_StaticRejectionRespectsOnZeroMatch(t *testing.T) {
	t.Parallel()
	withAPI := initRepo(t, map[string]string{
		".github/CODEOWNERS":   "* @org/eng\n",
		"services/api/main.go": "package api\n",
	})
	withoutAPI := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/eng\n",
		"lib/util.go":        "package lib\n",
	})
	policyFor := func(zeroMatch string) string {
		narrow := `"add_owner(/services/api/, @org/api)"`
		if zeroMatch != "" {
			narrow = `{ "op": "add_owner(/services/api/, @org/api)", "on_zero_match": "` + zeroMatch + `" }`
		}
		return rolloutPolicy(t, `{"version":1,"ops":["set_owners(*, [@org/everyone])",`+narrow+`]}`)
	}

	t.Run("skip is decided per repo", func(t *testing.T) {
		policy := policyFor("skip")
		if code, _, errOut := runCLI(t, "check", "--policy", policy); code != cli.ExitOK {
			t.Fatalf("check: exit %d, want 0 — a skip-guarded op may legitimately never apply\nstderr: %s", code, errOut)
		}
		if code, _, errOut := runCLI(t, "sync", "--repo", withoutAPI, "--policy", policy); code != cli.ExitOK {
			t.Errorf("repo without the scope: exit %d, want 0 — the narrower op is a no-op here, so there is no order to depend on\nstderr: %s", code, errOut)
		}
		if code, _, _ := runCLI(t, "sync", "--repo", withAPI, "--policy", policy); code != cli.ExitRefused {
			t.Errorf("repo with the scope: exit %d, want 2 — order-dependent here, and per repo, so the loop continues", code)
		}
	})

	t.Run("declare is decided per repo", func(t *testing.T) {
		policy := policyFor("declare")
		if code, _, errOut := runCLI(t, "check", "--policy", policy); code != cli.ExitOK {
			t.Fatalf("check: exit %d, want 0 — a declared rule lands at EOF where last-match-wins settles the outcome\nstderr: %s", code, errOut)
		}
		for name, dir := range map[string]string{"with the scope": withAPI, "without it": withoutAPI} {
			if code, _, _ := runCLI(t, "sync", "--repo", dir, "--policy", policy); code == cli.ExitInvalid {
				t.Errorf("%s: exit 3 — a declare pair is the tree's decision, at exit 2 if anything", name)
			}
		}
	})

	t.Run("require earns exit 3 everywhere", func(t *testing.T) {
		policy := policyFor("")
		for name, args := range map[string][]string{
			"check":              {"check", "--policy", policy},
			"sync with scope":    {"sync", "--repo", withAPI, "--policy", policy},
			"sync without scope": {"sync", "--repo", withoutAPI, "--policy", policy},
		} {
			if code, _, _ := runCLI(t, args...); code != cli.ExitInvalid {
				t.Errorf("%s: exit %d, want 3", name, code)
			}
		}
	})
}

// SPEC R-25: the ceiling flag is validated, not silently ignored.
//
// `-1` is the internal "no ceiling" sentinel, so a guard written as `>= 0`
// accepted `--max-paths-changed -5` as "no ceiling at all" — on a wave the
// operator had just told to cap itself — and let it slip past the `--policy`
// exclusion while the identical value in a policy file was rejected at load
// time. A typo, or a shell arithmetic result, silently removes the guard.
func TestScenario_CeilingFlagIsValidated(t *testing.T) {
	t.Parallel()
	dir := initRepo(t, map[string]string{
		".github/CODEOWNERS": "* @org/eng\n",
		"a/f.go":             "package a\n",
	})
	policy := rolloutPolicy(t, `{"version":1,"ops":["add_owner(*, @org/wide)"]}`)

	if code, _, errOut := runCLI(t, "sync", "--repo", dir, "--op", "add_owner(*, @org/wide)",
		"--max-paths-changed", "-5"); code != cli.ExitInvalid {
		t.Errorf("--max-paths-changed -5: exit %d, want 3 — a negative ceiling silently meaning `no ceiling` disarms the guard the operator just asked for\nstderr: %s", code, errOut)
	}
	// Even the sentinel value is a typed flag, and typing it alongside a policy
	// is the same mistake as any other value.
	if code, _, _ := runCLI(t, "sync", "--repo", dir, "--policy", policy, "--max-paths-changed", "-1"); code != cli.ExitInvalid {
		t.Errorf("--max-paths-changed -1 with --policy: exit %d, want 3", code)
	}
	// Precedence: the more fundamental problem on the same command line is
	// reported, not this one.
	_, _, errOut := runCLI(t, "sync", "--repo", dir, "--op", "add_owner(*, @o/x)",
		"--policy", policy, "--max-paths-changed", "5")
	if !strings.Contains(errOut, "mutually exclusive") {
		t.Errorf("--op with --policy must be reported ahead of the ceiling flag's own rule:\n%s", errOut)
	}
}

// SPEC R-24 (S-8): a `--file` naming a path GitHub never loads is warned about
// even when the repository has no CODEOWNERS at all.
//
// `--file build/OWNERSFILE --create` on a greenfield repo wrote a file,
// reported `applied` at exit 0, and emitted `codeowners_path` for the fleet
// loop to stage — with no warning, because the check compared against the
// CODEOWNERS files in the tree and there were none. That is the "applied, dead
// on arrival" outcome in its purest form: a hundred PRs, a hundred green rows,
// and no ownership anywhere.
func TestScenario_FileOutsideTheThreeLocationsIsWarned(t *testing.T) {
	t.Parallel()
	dir := initRepo(t, map[string]string{"x/f.go": "package x\n"})
	rec, code, errOut := scenarioSync(t, "--repo", dir, "--create",
		"--file", "build/OWNERSFILE", "--op", "add_owner(/x/, @org/b)")
	if code != cli.ExitOK {
		t.Fatalf("exit %d, want 0 — an explicit --file is the operator's call\nstderr: %s", code, errOut)
	}
	got := strings.Join(rec.Warnings, "\n")
	if !strings.Contains(got, "S-8") || !strings.Contains(got, "build/OWNERSFILE") {
		t.Errorf("no warning that the written file is not one GitHub reads; warnings = %#v", rec.Warnings)
	}
}

// SPEC R-24: the stale-comment warning fires only on a run that actually
// renamed something, and never on a run that wrote nothing.
//
// The first cut scanned for the old identifier regardless of whether the
// rename matched anything, so a fleet-wide rename warned about a rename that
// did not happen in every repo whose comments merely mentioned the old team —
// in the record and in the PR body — and kept warning on every later run. It
// also fired on runs that refused, describing line numbers in a file that was
// never written.
func TestScenario_StaleCommentWarningOnlyFollowsARealRename(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		".github/CODEOWNERS": "# @org/acq owns the pipeline\n/pipeline/ @org/other\n",
		"pipeline/p.go":      "package p\n",
	}

	// The comment names @org/acq; no rule does. Nothing is renamed.
	dir := initRepo(t, files)
	rec, code, _ := scenarioSync(t, "--repo", dir, "--op", "rename_owner(@org/acq, @org/new)")
	if code != cli.ExitOK || rec.Status != cli.StatusUnchanged {
		t.Fatalf("exit %d status %q, want 0/unchanged", code, rec.Status)
	}
	if len(rec.Warnings) != 0 {
		t.Errorf("a rename that renamed nothing warned about a stale comment: %#v", rec.Warnings)
	}

	// A refusal writes nothing, so it has no file whose comments to describe.
	refusing := initRepo(t, map[string]string{
		".github/CODEOWNERS": "# @org/acq owns the pipeline\n/pipeline/ @org/acq\n",
		"pipeline/p.go":      "package p\n",
	})
	rec2, code2, _ := scenarioSync(t, "--repo", refusing,
		"--op", "rename_owner(@org/acq, @org/new)", "--max-paths-changed", "0")
	if code2 != cli.ExitRefused {
		t.Fatalf("exit %d, want 2", code2)
	}
	for _, w := range rec2.Warnings {
		if strings.Contains(w, "comment still names") {
			t.Errorf("a refusal warned about comments in a file it never wrote: %q", w)
		}
	}

	// And a '#' inside a pattern is not a comment.
	pattern := initRepo(t, map[string]string{
		".github/CODEOWNERS": "/x/ @org/acq\na#@org/acq @org/other\n",
		"x/f.go":             "package x\n",
	})
	rec3, code3, _ := scenarioSync(t, "--repo", pattern, "--op", "rename_owner(@org/acq, @org/new)")
	if code3 != cli.ExitOK {
		t.Fatalf("exit %d, want 0", code3)
	}
	for _, w := range rec3.Warnings {
		if strings.Contains(w, "comment still names") {
			t.Errorf("a rule line whose PATTERN contains '#' was read as a comment: %q", w)
		}
	}
}
