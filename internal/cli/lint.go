package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jordonpeterson/codeowners-tool/internal/apply"
	"github.com/jordonpeterson/codeowners-tool/internal/ghapi"
	"github.com/jordonpeterson/codeowners-tool/internal/gittree"
	"github.com/jordonpeterson/codeowners-tool/internal/lint"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

// errReason unwraps ghapi's fail-closed error to the reason an operator can act
// on, without the "inconclusive: " prefix the caller supplies its own framing
// for.
func errReason(err error) string {
	var inc *ghapi.Inconclusive
	if errors.As(err, &inc) {
		return inc.Reason
	}
	return err.Error()
}

// `audit --lint` — the one mode of the read-only verb that writes.
//
// The split is deliberate and is where R-0 actually lives: the audit PACKAGE
// still never writes, and neither does the lint package. Both hand back an
// artifact, and the bytes reach disk through apply.Apply, the same single
// writer that `sync` and `apply` go through — hash pin, size cap, pre-write
// syntax validation, atomic rename, rollback.
//
// Two contracts differ from plain `audit` and both are load-bearing:
//
//   - It reads the WORKING-TREE file, not the one committed at --branch.
//     `audit` asks "what would GitHub do?", and GitHub only sees committed
//     files. Lint asks "what should this file say?", and the file it is about
//     to rewrite is the one on disk. Ownership still resolves against
//     --branch's tracked tree (S-7), exactly as `plan` and `sync` do.
//   - It requires a token and --github-repo. Whether an owner exists is not
//     decidable offline, and stage 2 is the point of the mode; running it
//     without the ability to answer that question would silently degrade to a
//     whitespace tidy while reporting success. One carve-out: with
//     --remove-stale-paths and NEITHER credential given, an offline run may
//     still make the ONE repair that is a tree fact rather than an API fact —
//     deleting dead patterns — with owner checks skipped and disclosed
//     (see runLint).
type lintRun struct {
	repo, branch, filePath string
	githubRepo, token      string
	apiURL, cacheDir       string
	cacheTTL               time.Duration
	cacheTTLSet            bool
	format                 string
	checks                 string
	removeStale            bool
	onEmpty                string
	dryRun                 bool
	// offline marks the tree-only mode: NEITHER a token nor --github-repo was
	// given, --remove-stale-paths requested. Set inside runLint, never by
	// callers — it is a fact about the run (which credentials were absent),
	// not a flag. Partial credentials never reach it: they refuse at exit 5.
	offline bool
	// policy records that a --policy file configured this run, not flags. It
	// governs only how a refusal is WORDED: the remedy for an unset R-6 policy
	// is a field in the file here and a flag under `audit --lint`, and naming
	// the wrong one sends the operator to a second refusal (R-36b).
	policy bool
}

// lintDoc is `--format json`: one object, so a CI step can gate on it without
// parsing prose.
type lintDoc struct {
	Path    string `json:"codeowners_path"`
	Applied bool   `json:"applied"`
	DryRun  bool   `json:"dry_run,omitempty"`
	// NeedsHuman and ExitCode come from the SAME function that produces the
	// process status, so a CI gate is `jq -e .needs_human` rather than a
	// two-clause expression that has to know the string "unrepairable-line" —
	// which a reviewer got wrong on the first try, going green over rot.
	NeedsHuman bool `json:"needs_human"`
	ExitCode   int  `json:"exit_code"`
	// OwnerChecksSkipped marks an offline run: only dead patterns were
	// judged, and nothing here says the owners exist (R-12).
	OwnerChecksSkipped bool          `json:"owner_checks_skipped,omitempty"`
	Actions            []lint.Action `json:"actions"`
	Unverifiable       []string      `json:"unverifiable,omitempty"`
	Changes            []plan.Change `json:"changes"`
	Rows               []plan.Row    `json:"ownership_rows"`
	Diff               string        `json:"diff"`
	Warnings           []string      `json:"warnings,omitempty"`
}

// cmdLint is the `lint` verb: the same run, with a flagset that contains only
// the flags that apply to it.
//
// This exists because the mode outgrew being a flag. Under `audit --lint`, six
// of audit's fourteen flags changed meaning or validity on one boolean, and
// three of the error messages below exist solely to police that coupling —
// here they are unreachable, because there is nothing to couple. `--help` is
// also self-grouping: Go's flag package sorts alphabetically, so under audit
// the operator met `-dry-run` ("with --lint, …") three entries before the flag
// that explains what `--lint` is.
//
// `audit --lint` keeps working and routes to the same runLint, so nothing that
// scripts it has to change.
func cmdLint(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var policyPaths multiFlag
	fs.Var(&policyPaths, "policy", "policy file (R-36): the repair preferences come from its \"lint\" block; its ops are validated and never applied")
	repo := fs.String("repo", ".", "path to local git repository")
	branch := fs.String("branch", "HEAD", "ref whose tracked tree governs resolution (S-7); must be what the clone has checked out unless --dry-run")
	filePath := fs.String("file", "", "CODEOWNERS path override (repo-relative); needed for a CODEOWNERS that is not committed yet")
	githubRepo := fs.String("github-repo", "", "owner/name on GitHub — required: owner existence is not decidable offline")
	token := fs.String("token", "", "GitHub token (default $GITHUB_TOKEN)")
	apiURL := fs.String("api-url", "", "API base URL (GHES), default https://api.github.com")
	format := fs.String("format", "text", "text|json")
	removeStale := fs.Bool("remove-stale-paths", false, "also delete rules whose pattern matches nothing tracked and nothing on disk (R-11 keeps this off by default)")
	onEmpty := fs.String("on-empty", "", "R-6 policy when removing a dead owner empties a set: error|inherit|unowned (no default; inherit DELETES the rule line)")
	dryRun := fs.Bool("dry-run", false, "report the fixes and write nothing (exit 4 if any are pending)")
	if err := fs.Parse(args); err != nil {
		return flagParseCode(err)
	}
	if err := rejectLeftoverArgs(fs); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitInvalid
	}
	if err := rejectEmptyRepo(fs, *repo); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitInvalid
	}
	removeStalePaths, onEmptyPolicy := *removeStale, *onEmpty
	if len(policyPaths) > 0 {
		var err error
		if removeStalePaths, onEmptyPolicy, err = lintPrefs(policyPaths, flagsPassed(fs)); err != nil {
			return exit3(stderr, err)
		}
	}
	// Same post-Parse read as cmdAudit, and for the same reason: making
	// os.Getenv the flag's DEFAULT hands the live credential to the flag
	// package, which prints non-empty string defaults in its usage text (CWE-532).
	authToken := *token
	if authToken == "" {
		authToken = os.Getenv("GITHUB_TOKEN")
	}
	return runLint(lintRun{
		repo: *repo, branch: *branch, filePath: *filePath,
		githubRepo: *githubRepo, token: authToken, apiURL: *apiURL,
		format: *format, removeStale: removeStalePaths, onEmpty: onEmptyPolicy, dryRun: *dryRun,
		policy: len(policyPaths) > 0,
	}, stdout, stderr)
}

// flagsPassed reports which flags were actually TYPED. A ban on a flag next to
// --policy is a ban on its PRESENCE, never on its value: `--remove-stale-paths=false`
// overrides a reviewed `"remove_stale_paths": true` just as surely as the bare
// flag overrides a reviewed false, and the zero value of a bool flag cannot
// tell the two apart.
func flagsPassed(fs *flag.FlagSet) map[string]bool {
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })
	return passed
}

// lintPrefs resolves `lint --policy` (R-36a): the repair preferences come from
// the file's "lint" block, and every verdict it can reach is decidable from the
// arguments and the file alone — hence exit 3, before any repository is opened.
//
// The whole file is loaded and validated, ops included, even though lint never
// applies one (R-36e). "Ignores" in R-36c means does not ACT on: one artifact
// is reviewed once and run everywhere, so a malformed op that only `sync`
// diagnosed would ride through the fleet unseen until someone happened to run
// the other command.
func lintPrefs(policyPaths []string, passed map[string]bool) (removeStale bool, onEmpty string, err error) {
	pol, opList, err := opSource(nil, policyPaths)
	if err != nil {
		return false, "", err
	}
	// The same two repo-independent op checks `sync` and `check` make, so all
	// three verbs reach the same verdict on the same bytes (R-36e).
	if err := validateScopes(opList); err != nil {
		return false, "", err
	}
	if err := ops.StaticConflict(opList); err != nil {
		return false, "", err
	}
	// R-36b, the rule `sync` already applies to --on-empty and
	// --max-paths-changed. Unconditional, and deliberately not "only when the
	// block sets the field": "the flag agreed with the file this time" is not
	// something a reviewer reading the artifact can see.
	//
	// AFTER the policy is loaded and validated, which is where `sync` moved
	// its own bans in db7fc71: a policy that does not load is the more
	// fundamental problem on the same command line, and reporting the flag
	// first let "the broken policy halted the run" be proven by the flag with
	// the policy never read at all. The operator pays for it twice — they drop
	// the flag, re-run, and meet a completely different failure.
	for _, dup := range []struct{ flag, field string }{
		{"remove-stale-paths", "remove_stale_paths"},
		{"on-empty", "on_empty"},
	} {
		if passed[dup.flag] {
			return false, "", fmt.Errorf("--%s is not allowed with --policy: set %q in the policy file's \"lint\" block instead, or the artifact in git is not the policy that ran (R-20/R-36)", dup.flag, dup.field)
		}
	}
	return pol.Lint.RemoveStalePaths, pol.Lint.OnEmpty, nil
}

// The two remedies for one refusal: R-6 has no default, and the way to state a
// policy differs by how this run was configured. `sync`'s noCodeownersError is
// the same shape for the same reason — under --policy the flag named by the
// flag-mode sentence is itself exit 3 (R-36b), so an operator who followed the
// advice met a second and worse refusal at the moment they could least tell a
// broken policy from an awkward repository.
const (
	onEmptyRemedyFlag   = "an explicit --on-empty policy (error|inherit|unowned) is required"
	onEmptyRemedyPolicy = `an explicit R-6 policy is required: set "on_empty" (error|inherit|unowned) in the policy file's "lint" block`
)

// policyRemedy re-points that advice at the reviewed artifact. It matches the
// sentence rather than an error type because the refusal is raised inside
// lint.Build, which is handed an OnEmpty and knows nothing about where it came
// from. The cost of that is drift: a reworded refusal upstream would make this
// a silent no-op and hand the illegal advice back. The flag-mode control in
// TestR36a_TheR6RemedyNamesTheBlockUnderPolicy pins the sentence for exactly
// that reason — the drift fails a test instead of reaching an operator.
func policyRemedy(err error) error {
	var inv *plan.InvalidError
	if !errors.As(err, &inv) || !strings.Contains(inv.Msg, onEmptyRemedyFlag) {
		return err
	}
	return &plan.InvalidError{Msg: strings.Replace(inv.Msg, onEmptyRemedyFlag, onEmptyRemedyPolicy, 1)}
}

func runLint(r lintRun, stdout, stderr io.Writer) int {
	// --checks selects a SUBSET of audit's checks. Lint is defined over the
	// whole file — a subset of it is not a smaller lint, it is an ambiguous
	// one, and silently ignoring the flag would let a CI job believe it had
	// narrowed a run that rewrote everything.
	if r.checks != "" {
		fmt.Fprintln(stderr, "error: --checks selects a subset of audit's checks and has no meaning with --lint, which covers the entire file; drop one of the two")
		return ExitInvalid
	}
	if r.format != "text" && r.format != "json" {
		fmt.Fprintf(stderr, "error: unknown --format %q; want text or json\n", r.format)
		return ExitInvalid
	}
	switch r.onEmpty {
	case "", "error", "inherit", "unowned":
	default:
		fmt.Fprintf(stderr, "error: unknown --on-empty policy %q; want error, inherit or unowned (R-6)\n", r.onEmpty)
		return ExitInvalid
	}
	// R-12 before anything is read: without the ability to ask whether an owner
	// exists, the headline stage cannot run, and a lint that quietly skipped it
	// would report success over a file still full of owners that go nowhere.
	// Name what is MISSING, not the whole precondition. The message used to
	// list both requirements whatever you had passed, so an operator holding a
	// valid token had to diff the sentence against their own command line to
	// find the one word they needed.
	var missing []string
	if r.token == "" {
		missing = append(missing, "a token ($GITHUB_TOKEN or --token)")
	}
	if r.githubRepo == "" {
		missing = append(missing, "--github-repo owner/name")
	}
	// Checked whenever the flag was TYPED, token or no token: a malformed
	// --github-repo is a misspelled argument the operator plainly meant to use,
	// and letting the credential refusal (or the offline mode) speak first
	// would silently ignore the value they typed.
	if r.githubRepo != "" {
		if len(strings.SplitN(r.githubRepo, "/", 2)) != 2 || strings.Count(r.githubRepo, "/") != 1 {
			fmt.Fprintln(stderr, "error: --github-repo must be owner/name")
			return ExitInvalid
		}
	}
	// One narrow carve-out: R-12 is about OWNER existence, an API fact. Whether
	// a pattern matches zero tracked files is a git-TREE fact the offline audit
	// (A-4/A-5) itself proves, so when --remove-stale-paths names that repair
	// as what this run is for, the run proceeds OFFLINE — stage 3 alone, owner
	// checks skipped and said so, no owner removed, verified, or repaired.
	//
	// Only when NEITHER credential was offered. A run that named a repo or
	// held a token asked for the credentialed lint, and silently narrowing it
	// to dead patterns would report success over owner checks the operator
	// believes ran — so partial credentials refuse below, naming what is
	// absent, exactly as before the mode existed.
	r.offline = r.token == "" && r.githubRepo == ""
	switch {
	case r.offline && !r.removeStale:
		// The escape hatch is named in the operator's own dialect: the flag is
		// exit-3-banned next to --policy (R-36b), so pointing a policy-mode
		// run at it would send them straight into a second refusal.
		hatch := "pass --remove-stale-paths"
		if r.policy {
			hatch = `set "remove_stale_paths": true in the policy file's "lint" block`
		}
		fmt.Fprintf(stderr, "error: lint needs %s. Whether an owner still exists is not decidable offline, and removing owners on a guess is what R-12 forbids — nothing was written. The one repair decidable offline is removing dead patterns (a tree fact); %s to run that repair alone, with owner checks skipped.\n", strings.Join(missing, " and "), hatch)
		return ExitInconclusive
	case !r.offline && len(missing) > 0:
		msg := fmt.Sprintf("error: lint needs %s. Whether an owner still exists is not decidable offline, and removing owners on a guess is what R-12 forbids — nothing was written.", strings.Join(missing, " and "))
		if r.removeStale {
			msg += " The tree-only offline mode (dead-pattern removal alone) engages only when neither a token nor --github-repo is given — supply what is missing, or drop the one you passed."
		}
		fmt.Fprintln(stderr, msg)
		return ExitInconclusive
	}
	// The disk cache is refused rather than used, and this is the one place
	// where lint must NOT inherit audit's behavior.
	//
	// ghapi.cachedBool serves a cached boolean without revalidation, and a
	// cached FALSE is "this owner does not exist" — which under audit produces
	// a finding a human reads, and under --lint authorizes a deletion. Two ways
	// that goes wrong, neither hypothetical: the values are trivially
	// invertible on disk, so anything that can write to a shared cache
	// directory (a CI runner, /tmp, a restored actions/cache artifact from
	// another branch) can make every owner read as dead with ZERO network
	// traffic; and with no attacker at all, a 24h TTL means a team deleted and
	// recreated yesterday is still remembered as gone. A --dry-run would even
	// populate the negatives that authorize tomorrow's real write.
	//
	// Per-run memory caching still applies, so an owner named on twenty lines
	// is still looked up once.
	if r.cacheTTLSet {
		fmt.Fprintln(stderr, "error: --cache-ttl has no effect with --lint: the disk cache is not used here at all (see --cache-dir below), so a TTL would govern nothing; drop it")
		return ExitInvalid
	}
	if r.cacheDir != "" {
		fmt.Fprintln(stderr, "error: --cache-dir is not available with --lint: a cached \"this owner does not exist\" is served without revalidation, and here that answer deletes an owner rather than printing a finding — a stale or tampered-with cache entry would strip ownership with no network call at all; drop --cache-dir (lookups are still cached in memory for the run)")
		return ExitInvalid
	}

	// Both of `sync`'s repository guards, for the same reasons it carries them.
	// Neither is optional here: --lint resolves against a tracked tree git
	// reports relative to the repository ROOT and then rewrites a file on disk,
	// which is exactly the pair of assumptions these two protect. Applied even
	// under --dry-run for the root check, because a preview computed against
	// the wrong file is a preview of nothing.
	if err := checkRepoRoot(r.repo); err != nil {
		return errExit(&plan.RefusalError{Msg: err.Error()}, stderr)
	}
	// The same guard `sync` carries, and for the same reason it exists there:
	// lint proves its edits against --branch's tracked tree but rewrites the
	// WORKING-TREE file, so on a clone standing anywhere else the two are
	// different trees. Concretely, a path that exists on HEAD but not on
	// --branch makes its rule look stale, and --remove-stale-paths deletes the
	// line — the file that lands on disk has just un-owned a directory that is
	// sitting right there, reported as applied, exit 0. Refusing beats implying
	// --dry-run: a run that exits 0 having written nothing reads as "this repo
	// was already clean".
	if err := checkBranchIsWritable(r.repo, r.branch, "lint", r.dryRun); err != nil {
		return errExit(&plan.RefusalError{Msg: err.Error()}, stderr)
	}
	tree, coPath, _, err := locate(r.repo, r.branch, r.filePath)
	if err != nil {
		return errExit(err, stderr)
	}
	target := filepath.Join(r.repo, filepath.FromSlash(coPath))
	// The path came out of the repository itself, so a committed symlink can
	// choose it; this is what keeps the write inside the clone.
	if err := containedWritePath(r.repo, target); err != nil {
		return errExit(err, stderr)
	}
	// Same refusal as sync and apply: a link that stays INSIDE the clone is
	// still a CODEOWNERS GitHub does not follow, so a repair written through it
	// repairs a file that governs nothing. See refuseSymlinkedTarget.
	if err := refuseSymlinkedTarget(target); err != nil {
		return errExit(err, stderr)
	}
	// And the gitlink the filesystem cannot show: --file can name a path inside
	// a submodule mounted at a CODEOWNERS location, where a repair would land
	// in another repository's checkout. See refuseGitlinkedTarget.
	if err := refuseGitlinkedTarget(tree, coPath); err != nil {
		return errExit(err, stderr)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
	}

	// Stage 3 judges staleness, and a rule is only stale if it matches nothing
	// in the committed tree AND nothing on disk. Without the second list a
	// directory that exists in the checkout but has not been committed yet
	// reads as "matches zero tracked files" and its owners are deleted out from
	// under it — while the file being rewritten is the working-tree one, where
	// that directory is plainly visible.
	var workTree []string
	if r.removeStale {
		workTree, err = gittree.ListWorkTree(r.repo)
		if err != nil {
			return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
		}
	}

	// Offline the client is never built and ProbeRepo never runs: there is no
	// credential to prove anything with, and the run removes no owners for it
	// to vouch for — lint.Build never calls the Verifier under SkipOwnerChecks.
	var verifier lint.Verifier
	if !r.offline {
		client := ghapi.New(r.apiURL, r.token, ghapi.NewMemCache())
		// --github-repo is a precondition, not decoration: it establishes that this
		// token can see the repository whose CODEOWNERS is about to be rewritten.
		// Without it the flag was required and then never used — the org for every
		// team lookup comes from the owner token itself — so a token with no
		// relationship to this repo ran to completion, and an operator asking "why
		// did lint delete my team" had no signal that the credential was wrong.
		// Fails closed, like every other lookup here.
		owner, name, _ := strings.Cut(r.githubRepo, "/")
		if err := client.ProbeRepo(owner, name); err != nil {
			fmt.Fprintf(stderr, "error: cannot confirm this token can see %s (%s) — refusing to remove owners on a credential whose access to the repository is unproven (R-12); nothing was written\n", r.githubRepo, errReason(err))
			return ExitInconclusive
		}
		verifier = client
	}

	res, buildErr := lint.Build(content, tree, verifier, lint.Options{
		RemoveStalePaths: r.removeStale,
		WorkTree:         workTree,
		OnEmpty:          r.onEmpty,
		SkipOwnerChecks:  r.offline,
	})

	var inconclusive *lint.InconclusiveError
	var noop *plan.NoOpError
	switch {
	case errors.As(buildErr, &inconclusive):
		fmt.Fprintln(stderr, "error:", inconclusive)
		return ExitInconclusive
	case errors.As(buildErr, &noop):
		// Clean, or clean apart from lines nobody can repair mechanically.
		//
		// The nil guard is not defensive padding: sync.execute carries the same
		// one, because a panic in an unattended fleet run loses the record for
		// this repo and every line the loop would have appended after it.
		if res == nil {
			return errExit(&plan.InvalidError{Msg: "lint reported nothing to do without a result"}, stderr)
		}
		//
		// This arm must stay ahead of errExit and must not be folded into it.
		// errExit maps *plan.NoOpError to ExitNoOp, which is right for `plan`
		// — "you asked for a change and there was none to make" — and wrong
		// here: a CODEOWNERS that needs no repair is the SUCCESS case of a lint
		// run, and every scheduled job that lints a fleet would see its healthy
		// repos exit 1 and read as failures under `set -e`.
		emitLint(res, coPath, false, r, stdout)
		return lintCode(res, false)
	case buildErr != nil:
		if r.policy {
			buildErr = policyRemedy(buildErr)
		}
		return errExit(buildErr, stderr)
	}

	if !r.dryRun {
		if _, err := apply.Apply(res.Plan, target); err != nil {
			return errExit(err, stderr)
		}
	}
	for _, w := range res.Plan.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	emitLint(res, coPath, !r.dryRun, r, stdout)
	return lintCode(res, r.dryRun)
}

// lintCode is the exit contract, stated as one rule: 0 when the file needs
// nothing further from a human, 4 when it does.
//
// Two things leave it needing something: fixes that were computed but not
// written (--dry-run — this is the CI gate), and lines lint refused to guess
// at. Reporting 0 for the second would let a job go green over a CODEOWNERS
// whose broken lines GitHub is still skipping.
func lintCode(res *lint.Result, pending bool) int {
	if pending && res != nil && len(res.Plan.Changes) > 0 {
		return ExitFindings
	}
	if res != nil && res.NeedsHuman() > 0 {
		return ExitFindings
	}
	return ExitOK
}

func emitLint(res *lint.Result, coPath string, applied bool, r lintRun, stdout io.Writer) {
	if r.format == "json" {
		code := lintCode(res, r.dryRun)
		doc := lintDoc{
			Path: coPath, Applied: applied, DryRun: r.dryRun,
			NeedsHuman: code == ExitFindings, ExitCode: code,
			OwnerChecksSkipped: r.offline,
			Actions:            res.Actions, Unverifiable: res.Unverifiable,
			Changes: res.Plan.Changes, Rows: res.Plan.Rows,
			Diff: res.Plan.Diff, Warnings: res.Plan.Warnings,
		}
		if doc.Actions == nil {
			doc.Actions = []lint.Action{}
		}
		if doc.Changes == nil {
			doc.Changes = []plan.Change{}
		}
		if doc.Rows == nil {
			doc.Rows = []plan.Row{}
		}
		b, _ := json.Marshal(doc)
		fmt.Fprintln(stdout, string(b))
		return
	}

	n, stuck := len(res.Plan.Changes), res.NeedsHuman()
	switch {
	case n == 0 && stuck > 0:
		// Never "lint clean" here. This run exits 4, and a headline saying the
		// file is fine directly above a nonzero exit code is the one output
		// that makes a CI log read green over a red result.
		fmt.Fprintf(stdout, "lint: nothing to repair automatically in %s, but %d line(s) need a person\n", coPath, stuck)
	case n == 0 && r.offline:
		// The offline mode's success claim is narrower than lint's, and the
		// headline must not borrow the wider one: "owners that do not exist"
		// names a check this run skipped.
		fmt.Fprintf(stdout, "lint: nothing to remove in %s — this offline run covered only dead patterns (--remove-stale-paths); add a token and --github-repo for the owner checks\n", coPath)
	case n == 0:
		// Scoped, not "clean". Lint covers three things; `audit` runs twelve
		// checks, and a bare "lint clean" sends somebody away from a file that
		// `audit` would still flag — which is exactly what it did to a reviewer
		// asked to "clean up our CODEOWNERS".
		fmt.Fprintf(stdout, "lint: nothing to repair in %s — lint covers handle spacing, owners that do not exist, and (with --remove-stale-paths) dead patterns; run `audit` for the other checks\n", coPath)
	case applied:
		fmt.Fprintf(stdout, "lint: %d fix(es) applied to %s — the file was written\n", n, coPath)
	default:
		fmt.Fprintf(stdout, "lint: %d fix(es) pending in %s (--dry-run; nothing written)\n", n, coPath)
	}
	if r.offline {
		// The disclosure the offline mode is conditioned on: what was skipped,
		// and that the fail-closed contract for owners is intact (R-12).
		fmt.Fprintf(stdout, "  note: owner checks were skipped — this run was given neither a token nor --github-repo, so no owner was verified, repaired, or removed (R-12); dead patterns were judged against the tree, and invalid lines were reported but left as written\n")
	}
	// What lint DID, then what it deliberately did not: a flat list made the
	// one line that is not a fix look like one, and made the headline count
	// disagree with the number of bullets under it.
	for _, a := range res.Actions {
		if a.Kind == lint.ActionUnrepairable || a.Kind == lint.ActionKeptCaseMismatch {
			continue
		}
		fmt.Fprintf(stdout, "  [%s] (line %d) %s\n", a.Kind, a.Line, lintDetail(a))
	}
	for _, row := range res.Plan.Rows {
		fmt.Fprintf(stdout, "  owners change: %s  %s → %s\n", row.Path, fmtOwners(row.Before), fmtOwners(row.After))
	}
	for _, o := range res.Unverifiable {
		fmt.Fprintf(stdout, "  [unverifiable] %s is an email owner; existence cannot be checked via the API (R-13) — left as written\n", o)
	}
	if stuck > 0 {
		// Named as the reason for the exit code, so "applied" above and a
		// nonzero status below stop looking like a contradiction.
		fmt.Fprintf(stdout, "still needs a person (exit %d) — this is not a failed write:\n", ExitFindings)
		for _, a := range res.Actions {
			if a.Kind == lint.ActionUnrepairable || a.Kind == lint.ActionKeptCaseMismatch {
				fmt.Fprintf(stdout, "  [%s] (line %d) %s\n", a.Kind, a.Line, lintDetail(a))
			}
		}
	}
}

func lintDetail(a lint.Action) string {
	switch a.Kind {
	case lint.ActionRepairOwner:
		return fmt.Sprintf("%q → %q", a.Before, a.After)
	case lint.ActionRemoveOwner:
		return fmt.Sprintf("%s removed from %q: %s", a.Owner, a.Pattern, a.Reason)
	case lint.ActionRemoveStale:
		return fmt.Sprintf("deleted %q: %s", a.Before, a.Reason)
	default:
		return a.Reason
	}
}
