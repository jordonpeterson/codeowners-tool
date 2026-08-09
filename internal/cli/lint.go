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
	"github.com/jordonpeterson/codeowners-tool/internal/lint"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
)

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

	var cache ghapi.Cache = ghapi.NewMemCache()
	if r.cacheDir != "" {
		cache = ghapi.NewDiskCache(r.cacheDir, r.cacheTTL)
	}
	client := ghapi.New(r.apiURL, r.token, cache)

	res, buildErr := lint.Build(content, tree, client, lint.Options{
		RemoveStalePaths: r.removeStale,
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

	n := len(res.Plan.Changes)
	switch {
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
