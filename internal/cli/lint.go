package cli

import (
	"encoding/json"
	"errors"
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
//     whitespace tidy while reporting success.
type lintRun struct {
	repo, branch, filePath string
	githubRepo, token      string
	apiURL, cacheDir       string
	cacheTTL               time.Duration
	format                 string
	checks                 string
	removeStale            bool
	onEmpty                string
	dryRun                 bool
}

// lintDoc is `--format json`: one object, so a CI step can gate on it without
// parsing prose.
type lintDoc struct {
	Path         string        `json:"codeowners_path"`
	Applied      bool          `json:"applied"`
	DryRun       bool          `json:"dry_run,omitempty"`
	Actions      []lint.Action `json:"actions"`
	Unverifiable []string      `json:"unverifiable,omitempty"`
	Changes      []plan.Change `json:"changes"`
	Rows         []plan.Row    `json:"ownership_rows"`
	Diff         string        `json:"diff"`
	Warnings     []string      `json:"warnings,omitempty"`
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
	if r.token == "" || r.githubRepo == "" {
		fmt.Fprintln(stderr, "error: --lint needs both a token ($GITHUB_TOKEN or --token) and --github-repo: whether a user or team still exists is not decidable offline, and removing owners on a guess is exactly what R-12 forbids — nothing was written")
		return ExitInconclusive
	}
	if len(strings.SplitN(r.githubRepo, "/", 2)) != 2 || strings.Count(r.githubRepo, "/") != 1 {
		fmt.Fprintln(stderr, "error: --github-repo must be owner/name")
		return ExitInvalid
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

	res, buildErr := lint.Build(content, tree, client, lint.Options{
		RemoveStalePaths: r.removeStale,
		WorkTree:         workTree,
		OnEmpty:          r.onEmpty,
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
		return errExit(buildErr, stderr)
	}

	if !r.dryRun {
		if err := apply.Apply(res.Plan, target); err != nil {
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
	if res != nil {
		for _, a := range res.Actions {
			if a.Kind == lint.ActionUnrepairable {
				return ExitFindings
			}
		}
	}
	return ExitOK
}

func emitLint(res *lint.Result, coPath string, applied bool, r lintRun, stdout io.Writer) {
	if r.format == "json" {
		doc := lintDoc{
			Path: coPath, Applied: applied, DryRun: r.dryRun,
			Actions: res.Actions, Unverifiable: res.Unverifiable,
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

	n, stuck := len(res.Plan.Changes), res.CountUnrepairable()
	switch {
	case n == 0 && stuck > 0:
		// Never "lint clean" here. This run exits 4, and a headline saying the
		// file is fine directly above a nonzero exit code is the one output
		// that makes a CI log read green over a red result.
		fmt.Fprintf(stdout, "lint: nothing to fix automatically in %s, but %d line(s) need a human — lint will not guess at them, and GitHub is skipping them meanwhile\n", coPath, stuck)
	case n == 0:
		fmt.Fprintf(stdout, "lint clean: %s\n", coPath)
	case applied:
		fmt.Fprintf(stdout, "lint: %d fix(es) applied to %s\n", n, coPath)
	default:
		fmt.Fprintf(stdout, "lint: %d fix(es) pending in %s (--dry-run; nothing written)\n", n, coPath)
	}
	for _, a := range res.Actions {
		fmt.Fprintf(stdout, "  [%s] (line %d) %s\n", a.Kind, a.Line, lintDetail(a))
	}
	for _, o := range res.Unverifiable {
		fmt.Fprintf(stdout, "  [unverifiable] %s is an email owner; existence cannot be checked via the API (R-13) — left as written\n", o)
	}
	for _, row := range res.Plan.Rows {
		fmt.Fprintf(stdout, "  owners change: %s  %s → %s\n", row.Path, fmtOwners(row.Before), fmtOwners(row.After))
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
