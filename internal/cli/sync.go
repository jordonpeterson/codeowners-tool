package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/apply"
	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/gittree"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
	"github.com/jordonpeterson/codeowners-tool/internal/policy"
)

// The fleet verbs (R-19…R-24).
//
// One rule orders everything in this file: exit 3 is reserved for failures that
// depend on nothing but the policy, and every failure that depends on which repo
// you are standing in is exit 2. Revision 1 had it the other way round for
// zero-match scopes and for "no CODEOWNERS", which halted a 100-repo rollout on
// roughly repo 3 — so each exit-3 verdict below is reached before the repository
// is opened at all, and everything after that point is exit 0 or 2.

// policyGuidance is UX rule 4, rendered here rather than by the parser:
// policy.MultiError deliberately carries only the problems, so that a caller
// printing one problem per line does not get advice interleaved between them.
// Without this line, the operator whose fleet run halted at repo 0 has no reason
// not to retry the same policy against the other 99 clones.
const policyGuidance = "this is a policy error — it will fail identically on every repo; fix the policy, do not retry"

// exit3 reports a member of the exit-3 class and returns its code.
func exit3(stderr io.Writer, err error) int {
	var multi *policy.MultiError
	if errors.As(err, &multi) {
		// Every accumulated problem prints in one run (R-22): fixing a generated
		// 40-op policy one error per invocation is miserable.
		for _, e := range multi.Errs {
			fmt.Fprintln(stderr, "error:", e)
		}
	} else {
		fmt.Fprintln(stderr, "error:", err)
	}
	fmt.Fprintln(stderr, policyGuidance)
	return ExitInvalid
}

// validOnEmpty reports whether s is one of R-6's three policies.
func validOnEmpty(s string) bool {
	return s == "error" || s == "inherit" || s == "unowned"
}

// noRecordNote says out loud that a policy error produced no record.
//
// An exit-3 verdict is reached before the repository is opened, so there is
// nothing to report about it and emitting a row would put a phantom repo in the
// aggregation. The consequence is easy to miss: a loop writing `--out
// records/$repo.json` and aggregating the directory afterwards sees the
// affected repos DISAPPEAR rather than appear as refused, so the count of repos
// needing attention goes down. Silence about that is what made it dangerous;
// the behavior itself is correct.
func noRecordNote(stderr io.Writer, format, outPath, summaryPath string, code int) int {
	if format != "json" && outPath == "" && summaryPath == "" {
		return code
	}
	fmt.Fprintln(stderr, "note: no record was written for this repo — a policy error is decided before the repository is opened, so there is no per-repo outcome to report, and neither --out nor --summary-out was created. A fleet aggregating those files will not see this repo at all; the exit code is the signal.")
	return code
}

// opSource resolves where the ops come from. Everything it can reject is
// decidable from the arguments alone, with no repository open — which is exactly
// what makes these verdicts identical on all 100 repos.
func opSource(opSpecs, policyPaths []string) (*policy.Policy, []ops.Op, error) {
	switch {
	case len(opSpecs) > 0 && len(policyPaths) > 0:
		return nil, nil, errors.New("--op and --policy are mutually exclusive (R-20): the policy file is the complete statement of what ran, and an op appended at one call site is invisible to the people reviewing that file")
	case len(policyPaths) > 1:
		// Never a silent last-wins: the artifact in git would not be the policy
		// that ran, and `check` would have validated something else.
		return nil, nil, fmt.Errorf("--policy given %d times (%s); it takes exactly one file (R-20)",
			len(policyPaths), strings.Join(policyPaths, ", "))
	case len(policyPaths) == 1:
		p, err := policy.Load(policyPaths[0])
		if err != nil {
			return nil, nil, err
		}
		return p, p.Ops, nil
	case len(opSpecs) > 0:
		list, err := ops.ParseAll(opSpecs)
		if err != nil {
			return nil, nil, err
		}
		return nil, list, nil
	default:
		return nil, nil, errors.New("no ops given: pass --op 'add_owner(/x/, @a)' (repeatable) or --policy policy.json (R-20)")
	}
}

// validateScopes rejects a scope the matcher cannot compile — a property of the op
// string alone, so it belongs to the exit-3 class and is settled before any repo
// is opened. Draining it here is what lets everything plan.Build can still call
// InvalidError (zero match, an R-8 overlap in this tree, a removal that empties
// an owner set) map to exit 2 without a second guess about which class it is in.
func validateScopes(list []ops.Op) error {
	for i, op := range list {
		if op.Kind == ops.RenameOwner {
			// A rename's scope comes from current ownership, not a pattern.
			continue
		}
		if _, err := pattern.Compile(op.Scope); err != nil {
			return fmt.Errorf("%s: invalid scope %q: %v", policy.OpLabel(op.ID, i), op.Scope, err)
		}
	}
	return nil
}

// syncRun is one repo's sync, after every repo-independent verdict has been made.
type syncRun struct {
	repoArg  string // D6: the --repo argument VERBATIM, never absolutized
	branch   string
	filePath string
	create   bool
	dryRun   bool
	onEmpty  string
	ops      []ops.Op
	policy   *policy.Policy // nil under --op
	// maxPaths is R-25's ceiling, -1 when unset, and maxPathsSource names where
	// it came from. The source is in the refusal text because the operator's
	// next decision is "raise the wave's ceiling" or "exempt this one repo",
	// and they cannot make it without knowing which knob they are holding.
	maxPaths       int
	maxPathsSource string
}

func cmdSync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opSpecs, policyPaths multiFlag
	fs.Var(&opSpecs, "op", "operation (repeatable); mutually exclusive with --policy")
	fs.Var(&policyPaths, "policy", "policy file (R-20); mutually exclusive with --op")
	repo := fs.String("repo", ".", "path to local git repository")
	branch := fs.String("branch", "HEAD", "ref whose tracked tree governs resolution (S-7)")
	filePath := fs.String("file", "", "CODEOWNERS path override (repo-relative)")
	onEmpty := fs.String("on-empty", "", "R-6 policy when remove_owner empties a set: error|inherit|unowned (only with --op)")
	create := fs.Bool("create", false, "write .github/CODEOWNERS when the repo has none; never overwrites (R-23)")
	maxPaths := fs.Int("max-paths-changed", -1, "R-25 ceiling: refuse (exit 2) if more than N paths would change owners; omit for no ceiling (only with --op)")
	dryRun := fs.Bool("dry-run", false, "change no CODEOWNERS; --out and --summary-out still emit")
	format := fs.String("format", "text", "text|json — governs stdout only")
	// Trusted operator input, deliberately not contained to --repo: unlike --file
	// and the discovered CODEOWNERS path, no repository can influence these. Their
	// real uses are outside the clone anyway (`--out records/$repo.json`,
	// `--summary-out "$GITHUB_STEP_SUMMARY"`), and no O_EXCL because a re-run has
	// to overwrite last run's records.
	out := fs.String("out", "", "write the JSON record here (always JSON, whatever --format says); trusted operator path — overwritten, and not contained to --repo")
	summaryOut := fs.String("summary-out", "", "write a markdown PR body here; trusted operator path — overwritten, and not contained to --repo")
	if err := fs.Parse(args); err != nil {
		return flagParseCode(err)
	}
	// Which flags were TYPED, as opposed to which hold a non-zero value. Every
	// "not allowed with --policy" ban below is a ban on the flag being
	// PRESENT: `--create=false` overrides a reviewed `"create": true` exactly
	// as `--create` overrides a reviewed `false`, and a ban that fired only on
	// the true value would miss half of it.
	passed := flagsPassed(fs)

	if *format != "text" && *format != "json" {
		// Never a silent fallback to text: the fleet script's `>> results.jsonl`
		// would then collect human prose that `jq -s` cannot read, after the
		// whole rollout has already written its CODEOWNERS files.
		return exit3(stderr, fmt.Errorf("unknown --format %q; want text or json", *format))
	}
	// Validated here, not on whichever repo first has a removal empty an owner
	// set. A flag value is decidable from the arguments alone, so it belongs to
	// the exit-3 class — and the policy file's `on_empty` has been validated at
	// load time all along, so leaving the flag lazy made two spellings of one
	// setting disagree: `--on-empty typo` ran a whole fleet at exit 0 and then
	// reported exit 2 ("this repo needs a human") on the one repo that happened
	// to trip it, naming a CODEOWNERS that had nothing to do with the mistake.
	if *onEmpty != "" && !validOnEmpty(*onEmpty) {
		return exit3(stderr, fmt.Errorf("unknown --on-empty %q; want error, inherit or unowned (R-6)", *onEmpty))
	}
	if *onEmpty != "" && len(policyPaths) > 0 {
		return exit3(stderr, errors.New("--on-empty is not allowed with --policy: set \"on_empty\" in the policy file instead, or the artifact in git is not the policy that ran (R-20)"))
	}
	// A non-HEAD --branch may not write — checkBranchIsWritable enforces that
	// for creates and edits alike, by comparing RESOLVED COMMITS rather than
	// the literal string "HEAD". Comparing strings rejected `--branch main` on
	// a clone already standing on main: a completely ordinary fleet
	// invocation, and being argument-shaped it exited 3, halting the whole
	// rollout at repo 0 over an argument that was never wrong (R-23).
	// --file is joined onto --repo by everything below, so it is only meaningful
	// as a repo-relative path. Argument-only, repo-independent, hence exit 3 —
	// and it is checked BEFORE the repository is opened, because with --create
	// the write happens outside the repository the moment we get that far.
	if err := containedRelPath(*filePath); err != nil {
		return exit3(stderr, err)
	}
	// Argument-only, hence exit 3: a fleet run halts at repo 0 rather than
	// recording the same refusal 100 times.
	if err := gittree.ValidateRef(*branch); err != nil {
		return exit3(stderr, err)
	}
	// Whether the ceiling flag was TYPED, not merely whether its value looks
	// set. -1 is the internal "no ceiling" sentinel, so guarding on `>= 0` let
	// `--max-paths-changed -5` — a typo, or a shell arithmetic result — mean
	// "no ceiling at all" on a wave that was supposed to be capped, silently,
	// and slip past the --policy guard below while the policy field rejected
	// the identical value at load time.
	maxPathsSet := passed["max-paths-changed"]
	// After opSource, so the more fundamental problems in the same command line
	// (--op with --policy, a policy that does not parse) are reported first
	// rather than hidden behind this one.
	pol, opList, err := opSource(opSpecs, policyPaths)
	if err != nil {
		return noRecordNote(stderr, *format, *out, *summaryOut, exit3(stderr, err))
	}
	if maxPathsSet && *maxPaths < 0 {
		return exit3(stderr, fmt.Errorf("--max-paths-changed %d must be zero or positive; omit the flag to set no ceiling (R-25)", *maxPaths))
	}
	// The third member of that family (R-34b), and the one whose false default
	// hid it: `--policy p.json --create` used to create the file at exit 0
	// while `--on-empty` in the same position was refused, so the artifact in
	// git was not the policy that ran. Keyed on presence rather than value for
	// the reason `passed` exists, and placed after opSource for the reason the
	// ceiling's ban is — a policy that does not parse is the more fundamental
	// problem in the same command line, and reporting the flag first would let
	// "the broken policy halted the fleet" be proven by the flag instead.
	if passed["create"] && len(policyPaths) > 0 {
		return exit3(stderr, errors.New("--create is not allowed with --policy: set \"create\" in the policy file instead, or the artifact in git is not the policy that ran (R-20/R-34)"))
	}
	if maxPathsSet && len(policyPaths) > 0 {
		// Mirrors --on-empty exactly, and for the same reason: the ceiling is a
		// claim about the INTENT ("this wave touches dozens of files per repo,
		// not thousands"), so it belongs in the artifact a reviewer approves.
		// A flag that could override the file would let one call site quietly
		// loosen a reviewed policy, and any "lower of the two wins" scheme is a
		// new precedence rule for operators to learn.
		return exit3(stderr, errors.New("--max-paths-changed is not allowed with --policy: set \"max_paths_changed\" in the policy file instead, or the artifact in git is not the policy that ran (R-20/R-25)"))
	}
	if err := validateScopes(opList); err != nil {
		return exit3(stderr, err)
	}
	// The statically provable half of R-8, settled here with no repository
	// open, so both verbs give the same verdict for the same policy on every
	// repo. plan.Build keeps the other half — an overlap only a real tree
	// reveals stays exit 2, per repo, and the fleet loop still steps over it.
	if err := ops.StaticConflict(opList); err != nil {
		return noRecordNote(stderr, *format, *out, *summaryOut, exit3(stderr, err))
	}

	run := &syncRun{
		repoArg:  *repo,
		branch:   *branch,
		filePath: *filePath,
		create:   *create,
		dryRun:   *dryRun,
		onEmpty:  *onEmpty,
		ops:      opList,
		policy:   pol,

		maxPaths:       *maxPaths,
		maxPathsSource: "--max-paths-changed",
	}
	if pol != nil {
		// R-34a: the same code path --create drives, reached from the reviewed
		// artifact instead. R-23 is unchanged — create never overwrites — so
		// this stays safe on a fleet where only some repos have a file.
		run.create = pol.Create
		run.onEmpty = pol.OnEmpty
		run.maxPaths = pol.MaxPathsChanged
		run.maxPathsSource = fmt.Sprintf("\"max_paths_changed\" in %s", policyPaths[0])
	}

	rec, code := run.execute()
	if rec.Error != "" {
		fmt.Fprintln(stderr, "error:", rec.Error)
	}
	for _, w := range rec.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	emitRecord(rec, run, *format, *out, *summaryOut, stdout, stderr)
	return code
}

// execute reads the repository and converges it. From here on the only codes are
// 0 and 2 — every remaining failure is a fact about THIS repo, and a fleet script
// records it and steps to the next clone.
func (r *syncRun) execute() (SyncRecord, int) {
	rec := SyncRecord{Repo: r.repoArg, DryRun: r.dryRun}

	tree, err := gittree.ListTracked(r.repoArg, r.branch)
	if err != nil {
		// StatusError, not StatusRefused: the tool never got far enough to read
		// this repo, let alone decline to touch it. Grouping on .status is how an
		// operator separates "12 awkward CODEOWNERS files" from "12 clones that
		// failed and were never synced".
		rec.Status = StatusError
		rec.Error = err.Error()
		return rec, ExitRefused
	}

	// Both guards below are refusals, not errors: the repository was read
	// successfully and the tool is declining to write into it. Both are also
	// facts about THIS clone — the next one may be laid out differently, or be
	// checked out on the ref that was asked for — so both are exit 2, and a
	// fleet loop records them and steps to the next repo.
	if err := r.checkRepoRoot(); err != nil {
		rec.Status = StatusRefused
		rec.Error = err.Error()
		return rec, ExitRefused
	}
	if err := r.checkBranchIsWritable(); err != nil {
		rec.Status = StatusRefused
		rec.Error = err.Error()
		return rec, ExitRefused
	}

	rel, content, creating, err := r.governing(tree)
	if err != nil {
		rec.Status = statusForReadFailure(err)
		rec.Error = err.Error()
		return rec, ExitRefused
	}
	// Warnings about the FILE, gathered before anything is planned so a repo
	// that goes on to refuse still reports them: the two conditions below are
	// exactly the ones an operator wants listed across the fleet afterwards.
	fileWarnings := governingWarnings(tree, rel, content)
	rec.Warnings = fileWarnings

	// Where the write would actually land, decided before anything is planned.
	// The path came out of the repository itself — discovery reads the tracked
	// tree — so a committed symlink chooses it, and containedWritePath is what
	// keeps that choice inside the clone. Refusal, not error: the repo was read
	// fine and the tool is declining to write into it, and it is a fact about
	// THIS clone, so exit 2 and the fleet loop steps to the next one.
	if err := containedWritePath(r.repoArg, filepath.Join(r.repoArg, filepath.FromSlash(rel))); err != nil {
		rec.Status = StatusRefused
		rec.Error = err.Error()
		return rec, ExitRefused
	}

	p, buildErr := plan.Build(content, tree, r.ops, plan.Options{OnEmpty: r.onEmpty})
	var noop *plan.NoOpError
	converged := errors.As(buildErr, &noop)
	// D1 says the no-op path returns a POPULATED plan. The nil guard is not
	// dead code insurance for a hypothetical: dereferencing nil here would
	// panic, and a panic in an unattended fleet run loses the record for this
	// repo and every subsequent line the loop would have appended.
	if p == nil && converged {
		buildErr, converged = errors.New("planner reported a no-op without a plan"), false
	}
	if buildErr != nil && !converged {
		// Refusal, zero-match under `require`, an R-8 overlap in this tree, the
		// size cap: all repo-dependent, all exit 2. Mapping any of them to 3 is
		// what strands the other 99 repos.
		rec.Status = StatusRefused
		// The record carries no codeowners_path on this path — nothing was
		// written — so the file names itself here instead. Triaging a refusal
		// in a repo whose ownership lives in docs/CODEOWNERS is a different
		// conversation from one in .github/, and the operator reading
		// `needs-human` has only this string to go on.
		rec.Error = withGoverningFile(buildErr.Error(), rel, creating)
		return rec, ExitRefused
	}

	rec.Ops = p.OpResults
	for _, o := range p.OpResults {
		switch o.Status {
		case "applied":
			rec.OpsApplied++
		case "skipped":
			rec.OpsSkipped++
		}
	}
	rec.PathsChanged = len(p.Rows)
	rec.Warnings = append(fileWarnings, p.Warnings...)

	// R-25: the blast-radius ceiling. Opt-in, no default — a default would
	// break every legitimate `set_owners(*, …)` baseline on upgrade, which
	// teaches operators to pass an enormous number reflexively and stop
	// thinking about it. Checked here, so the count it reports is the same
	// number the record carries and the same one --dry-run previews.
	if r.maxPaths >= 0 && rec.PathsChanged > r.maxPaths {
		rec.Status = StatusRefused
		// The contributing ops are named because the per-op array cannot carry
		// them: every op is rewritten to `unchanged` below (nothing applied),
		// which is right for the counts and leaves a ceiling-blocked op
		// indistinguishable from one that was already satisfied. The operator's
		// next question is "which op did this", so the answer goes in the text.
		rec.Error = withGoverningFile(fmt.Sprintf(
			"refusing: this run would change the owners of %d path(s), over the %d-path ceiling set by %s (R-25) — nothing was written; the op(s) behind the number: %s. Re-run with `--dry-run --out preview.json` to see which paths, raise the ceiling if the number is right, or narrow the ops if it is not",
			rec.PathsChanged, r.maxPaths, r.maxPathsSource, blockedOpLabels(rec.Ops)), rel, creating)
		// Same normalization the failed-write path applies: no byte moved, so no
		// op may still claim it applied. paths_changed is the deliberate
		// exception — a record that refuses on a number and omits the number is
		// useless.
		for i := range rec.Ops {
			if rec.Ops[i].Status == "applied" {
				rec.Ops[i].Status = "unchanged"
			}
		}
		rec.OpsApplied, rec.Changes = 0, nil
		return rec, ExitRefused
	}
	rec.Changes = p.Changes
	rec.Status = syncStatus(rec)
	if converged {
		// A run that wrote nothing must not report line changes. plan can
		// synthesize an edit that renders byte-identical text; a `changes` array
		// under "status":"unchanged" would make every converged repo look like a
		// pending diff in the fleet preview.
		rec.Changes = nil
	}

	if !converged && !r.dryRun {
		if err := r.write(rel, p, creating); err != nil {
			rec.Status = StatusRefused
			rec.Error = err.Error()
			rec.Created = false
			// Nothing reached disk, so the record must not read like a run that
			// changed something. Leaving ops_applied, paths_changed and the
			// changes array populated made `jq '[.[].ops_applied] | add'`
			// overcount the fleet by exactly the repos where the write FAILED —
			// the rollout summary would claim ownership moved on repos whose
			// CODEOWNERS is byte-for-byte what it was. Ops are rewritten the same
			// way the already-converged path rewrites them (plan.Build), so the
			// per-op array and the counts still agree with each other: an op that
			// was skipped at planning time is still reported skipped, and no op is
			// left claiming an edit that does not exist.
			for i := range rec.Ops {
				if rec.Ops[i].Status == "applied" {
					rec.Ops[i].Status = "unchanged"
				}
			}
			rec.OpsApplied = 0
			rec.PathsChanged = 0
			rec.Changes = nil
			return rec, ExitRefused
		}
	}
	// Reached only when the run is going to write: every refusal above returns
	// before this point. A warning about comments in a file that was never
	// written describes a change that did not happen, with line numbers taken
	// from a file nobody will see — so this must stay below them.
	rec.Warnings = append(rec.Warnings, staleCommentWarnings(p.AfterContent, r.ops, p.OpResults, rel)...)

	// `created` reports what this run did, or under --dry-run what it would have
	// done — a converged repo needs no file written, so nothing is created for it
	// even with --create.
	rec.Created = creating && !converged
	// Emitted ONLY here, on the applied path: the fleet loop stages what this
	// field names, so it has to mean "there is something to stage". See the
	// field's own comment for what emitting it more widely cost.
	if rec.Status == StatusApplied {
		rec.CodeownersPath = rel
	}
	return rec, ExitOK
}

// checkRepoRoot refuses a --repo that points BELOW a repository's root.
//
// gittree.ListTracked runs `git -C <repo>`, and git walks UP to the enclosing
// repository rather than refusing: pointed at rK/sub it answers with rK's tree
// minus the `sub/` prefix. Nothing looks wrong from inside — the scopes match
// that tree, the plan is proven against it, the write succeeds — and the run
// reports "applied", exit 0. What it produced is rK/sub/.github/CODEOWNERS, and
// GitHub loads only the CODEOWNERS at the repository ROOT, so the file governs
// nothing; the rules in it are anchored at that root, where the paths they name
// do not exist; and rK's real CODEOWNERS was never read, because discovery
// looked below it. A fleet whose clone layout carries one extra directory level
// (…/clones/<repo>/checkout is a common one) writes 100 dead files and reports
// 100 successes. That is precisely the "reported applied, dead on arrival"
// outcome this whole verb exists to prevent, so it is refused.
//
// The comparison resolves symlinks on BOTH sides and never touches
// SyncRecord.Repo: on macOS `t.TempDir()` hands out /var/folders/... while git
// reports /private/var/folders/..., one directory under two names. Comparing
// the raw strings would refuse every repo on a developer laptop while CI on
// Linux stayed green; deriving .repo from the resolved path instead would break
// every fleet lookup that keys on the argument (D6).
func (r *syncRun) checkRepoRoot() error { return checkRepoRoot(r.repoArg) }

// checkRepoRoot is the free function behind it, shared with `audit --lint` for
// the same reason checkBranchIsWritable is: both verbs resolve against a tree
// git reports relative to the ROOT and then join the discovered CODEOWNERS path
// onto --repo. Pointed one level down, discovery finds the ROOT's
// .github/CODEOWNERS in that tree and the join addresses a DIFFERENT file of
// the same name under the subdirectory — so the run reads one file, writes
// another, names the first in its output, and leaves the file that actually
// governs untouched.
func checkRepoRoot(repoDir string) error {
	root, err := gitLine(repoDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	same, err := sameDir(repoDir, root)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("--repo %s is inside the repository rooted at %s, not that root: git resolves the tracked tree against the root, so the CODEOWNERS this run would write is at a path GitHub never reads, while the file that does govern (%s) stays untouched; re-run with --repo %s",
			repoDir, root, filepath.ToSlash(filepath.Join(root, gittree.CodeownersLocations[0])), root)
	}
	return nil
}

// checkBranchIsWritable refuses to WRITE while proving against another ref.
//
// --branch names the ref whose tracked tree governs resolution (S-7), and every
// invariant this tool proves is proven against that tree. The bytes, though,
// come from the working tree, and the working tree is whatever is checked out —
// so `sync --branch old` on a clone standing on main proved INV-2 against old's
// tree and then wrote main's file, exit 0, "applied": a rule that is dead where
// it landed, justified by a tree nobody wrote to.
//
// Refusing is chosen over implying --dry-run. An implied dry-run exits 0 having
// written nothing, which under the fleet contract in this file reads as "this
// repo converged" — 100 repos silently unchanged and 100 green rows is a worse
// failure than the one being fixed, and it is invisible until someone opens a
// PR that is not there. --dry-run remains fully available; it just has to be
// asked for. `plan` is unaffected: it emits an artifact and writes no
// CODEOWNERS, so proving against another ref is exactly its job.
//
// Refs are compared by resolved commit, not by name, so the ordinary fleet
// invocation `--branch main` on a clone checked out at main writes as it always
// did — as does a tag or a second branch pointing at the same commit, where the
// tree is the same tree.
func (r *syncRun) checkBranchIsWritable() error {
	return checkBranchIsWritable(r.repoArg, r.branch, "sync", r.dryRun)
}

// checkBranchIsWritable is the free function behind it, shared with
// `audit --lint` — the other verb that proves against --branch's tree and
// writes the working tree, and which therefore has exactly the same way to
// land a change justified by a tree nobody wrote to. Sharing it is the point:
// a second copy of this reasoning is a second chance to omit it.
func checkBranchIsWritable(repoDir, branch, verb string, dryRun bool) error {
	if branch == "HEAD" || dryRun {
		return nil
	}
	// rev-parse has no trailing `--` for --end-of-options to swallow (unlike
	// ls-tree — see gittree.ValidateRef), so it is free here.
	head, err := gitLine(repoDir, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return err
	}
	want, err := gitLine(repoDir, "rev-parse", "--verify", "--end-of-options", branch+"^{commit}")
	if err != nil {
		return err
	}
	if head != want {
		// The `plan` escape hatch is offered only to sync, whose intent is a
		// set of ops and so can be expressed as an artifact for another ref.
		// A lint pass is not expressible that way, and pointing someone at a
		// verb that cannot do what they asked is worse than saying nothing.
		alt := ""
		if verb == "sync" {
			alt = ", or use `plan` to produce an artifact for that ref"
		}
		return fmt.Errorf("--branch %s is not what this clone has checked out (HEAD is %s): %s proves the change against %s's tree but writes the working tree, so the rule would be justified by one tree and land in another; re-run with --dry-run to preview it, or check out %s first%s (S-7)",
			branch, headLabel(repoDir, head), verb, branch, branch, alt)
	}
	return nil
}

// headLabel names what HEAD actually is — "main (f800559)" rather than a bare
// abbreviated SHA. The operator has to decide whether to check out the ref they
// asked for, and "you are not on it" is not enough to act on; they need to know
// where they ARE. A detached HEAD keeps the SHA alone, which is the honest
// answer there.
func headLabel(repoDir, head string) string {
	short := head
	if len(short) > 7 {
		short = short[:7]
	}
	name, err := gitLine(repoDir, "rev-parse", "--abbrev-ref", "--end-of-options", "HEAD")
	if err != nil || name == "" || name == "HEAD" {
		return short
	}
	return name + " (" + short + ")"
}

// gitLine runs a git command that answers with a single line.
func gitLine(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// sameDir reports whether two paths name one directory, symlinks resolved.
func sameDir(a, b string) (bool, error) {
	ra, err := resolveDir(a)
	if err != nil {
		return false, err
	}
	rb, err := resolveDir(b)
	if err != nil {
		return false, err
	}
	return ra == rb, nil
}

func resolveDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	// EvalSymlinks failing is not fatal here: the absolutized path is still a
	// usable answer, and the only cost is a refusal that reads as a mismatch
	// rather than as an I/O error.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	return filepath.Clean(abs), nil
}

// statusForReadFailure separates the two ways reading a repo can fail. A missing
// CODEOWNERS is a considered refusal — the tool read the repo and declined,
// because --create is off by default; an I/O failure is one it never got to read.
func statusForReadFailure(err error) string {
	var missing *noCodeownersError
	if errors.As(err, &missing) {
		return StatusRefused
	}
	return StatusError
}

// noCodeownersError is R-23: this repo has no CODEOWNERS and --create was not
// given. Exit 2, never 3 — --create is off by default, so treating it as a policy
// error halted revision 1's fleet run at roughly repo 3.
type noCodeownersError struct {
	ref string
	// policy is set on a --policy run, where R-34b makes --create exit 3.
	// Advising it there costs the operator a second and worse failure at the
	// moment they can least tell a broken policy from an awkward repo.
	policy bool
}

func (e *noCodeownersError) Error() string {
	const head = "no CODEOWNERS file found in .github/, root, or docs/ at "
	if e.policy {
		return head + e.ref + `; set "create": true in the policy file to write one at .github/CODEOWNERS, or --file to name a path (R-23/R-34)`
	}
	return head + e.ref +
		"; re-run with --create to write one at .github/CODEOWNERS, or --file to name a path (R-23)"
}

// governing finds the CODEOWNERS this run edits and reads its current bytes.
//
// Discovery falls back to the WORKING TREE (D5). FindCodeownersPaths runs over
// `git ls-tree`, so a file created by pass 1 and not yet committed is invisible
// to pass 2: the tool would see "no CODEOWNERS" again, --create never overwrites,
// and there is no third outcome — a nightly job could never converge. Bytes come
// from the working tree for the same reason `plan` reads them there: that is what
// apply mutates.
func (r *syncRun) governing(tree []string) (rel string, content []byte, creating bool, err error) {
	switch {
	case r.filePath != "":
		rel = r.filePath
	default:
		if all := gittree.FindCodeownersPaths(tree); len(all) > 0 {
			rel = all[0]
		} else if onDisk := r.findOnDisk(); onDisk != "" {
			rel = onDisk
		} else {
			rel = gittree.CodeownersLocations[0]
		}
	}

	b, readErr := os.ReadFile(filepath.Join(r.repoArg, filepath.FromSlash(rel)))
	switch {
	case readErr == nil:
		return rel, b, false, nil
	case !errors.Is(readErr, os.ErrNotExist):
		return "", nil, false, readErr
	case !r.create:
		return "", nil, false, &noCodeownersError{ref: r.branch, policy: r.policy != nil}
	default:
		// Creating from nothing: the "before" state is an empty file, which is
		// what INV-2 is proven against.
		return rel, nil, true, nil
	}
}

// governingWarnings reports what is wrong with the FILE this run is about to
// edit, as distinct from what is wrong with the change.
//
// Every condition below exits 0 and reports "applied" — none is a reason to
// refuse an otherwise correct edit — and every one of them is invisible at
// fleet scale unless the run that touched the file says so.
func governingWarnings(tree []string, rel string, content []byte) []string {
	var out []string
	// Independent facts, deliberately not an either/or: a repo can be in both,
	// and that is the one where the operator most needs both halves.
	// A path GitHub never loads. The tree check below cannot catch this on a
	// repo that has no CODEOWNERS at all — there is nothing to compare against
	// — so `--file build/OWNERSFILE --create` wrote a file governing nothing
	// and reported `applied`.
	if !isCodeownersLocation(rel) {
		out = append(out, fmt.Sprintf(
			"this run writes %s, which is not a path GitHub loads CODEOWNERS from (S-8 reads only %s) — whatever is written there governs nothing",
			rel, strings.Join(gittree.CodeownersLocations, ", ")))
	}
	if present := gittree.FindCodeownersPaths(tree); len(present) > 0 {
		if rel != present[0] && isCodeownersLocation(rel) {
			out = append(out, fmt.Sprintf(
				"this run writes %s, but GitHub resolves ownership from %s (S-8: .github/ > root > docs/, first found wins, never merged) — the rules written here govern nothing until that is the file being edited",
				rel, present[0]))
		}
		if len(present) > 1 {
			out = append(out, fmt.Sprintf(
				"this repository has %d CODEOWNERS files (%s); GitHub loads only %s, so the rest govern nothing and were left untouched — delete them (A-10)",
				len(present), strings.Join(present, ", "), present[0]))
		}
	}
	if invalid := file.Parse(content).InvalidLines(); len(invalid) > 0 {
		first := invalid[0].Err
		out = append(out, fmt.Sprintf(
			"%s has %d line(s) GitHub cannot parse and silently skips (S-3), first at line %d: %s — this run left them exactly as they were; `audit --checks a8` lists them all",
			rel, len(invalid), first.Line, first.Message))
	}
	return out
}

// isCodeownersLocation reports whether a repo-relative path is one of the three
// GitHub actually reads (S-8).
func isCodeownersLocation(rel string) bool {
	for _, loc := range gittree.CodeownersLocations {
		if rel == loc {
			return true
		}
	}
	return false
}

// withGoverningFile appends the CODEOWNERS a refusal was about. A refused
// record carries no codeowners_path, so this string is all a `needs-human` pile
// has to go on. Omitted while CREATING: the repo has no CODEOWNERS yet, and
// naming the one that would have been written points at nothing.
func withGoverningFile(msg, rel string, creating bool) string {
	if creating || rel == "" {
		return msg
	}
	return fmt.Sprintf("%s (governing file: %s)", msg, rel)
}

// staleCommentWarnings reports comments that still name an owner this run
// renamed away.
//
// `rename_owner` deliberately does not touch prose — editing a comment is the
// one thing this op could do that it cannot prove — so the comment is left
// lying, pointing the next engineer at a team that no longer exists. Nothing
// else finds these: `audit` does not read prose, and the handle is gone, so no
// lookup will trip on it. The line number is the point, and the fix goes in the
// PR the human is already reading, which is why this reaches --summary-out too.
func staleCommentWarnings(after string, opList []ops.Op, results []plan.OpResult, rel string) []string {
	var out []string
	for i, op := range opList {
		if op.Kind != ops.RenameOwner {
			continue
		}
		// The warning fires whether or not this repo had a rule to rename. Gating
		// it on "applied" was backwards: in a repo where the old handle survives
		// ONLY in a comment, the rename correctly changes nothing, the record
		// reads `unchanged` — and that is exactly the repo where the comment is
		// the last trace of a retired team and this warning is the only thing
		// that will ever find it. What the two cases must not share is wording,
		// because claiming "this run renamed X" where nothing was renamed is the
		// overclaim the gate was added to prevent.
		renamed := i < len(results) && results[i].Status == "applied"
		for _, line := range commentLinesNaming(after, op.Owner) {
			// The line number is the one in the file as this run leaves it, not
			// where the comment sits today: a rule inserted above shifts it
			// down, and the message says so rather than misdirecting whoever
			// reads a --dry-run preview.
			if renamed {
				out = append(out, fmt.Sprintf(
					"%s line %d as this run leaves it: a comment still names %q, which this run renamed to %q; comments are never rewritten (the substitution is proven only over owner tokens), so this one now points at an identifier that no longer exists",
					rel, line, op.Owner, op.NewOwner))
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s line %d: no rule in this file names %q, so there was nothing for this run to rename here — but a comment still names it, and the rename to %q makes that comment point at an identifier that no longer exists",
				rel, line, op.Owner, op.NewOwner))
		}
	}
	return out
}

// commentLinesNaming returns the 1-based line numbers whose comment text names
// owner as a whole identifier. The boundary check is what keeps a rename of
// `@org/acq` from reporting a comment that only ever said `@org/acq-infra`.
func commentLinesNaming(content, owner string) []int {
	var lines []int
	for i, line := range strings.Split(content, "\n") {
		hash := commentStart(line)
		if hash < 0 {
			continue
		}
		text := line[hash:]
		for at := 0; ; {
			idx := strings.Index(text[at:], owner)
			if idx < 0 {
				break
			}
			end := at + idx + len(owner)
			if end == len(text) || !ownerTokenByte(text[end]) {
				lines = append(lines, i+1)
				break
			}
			at = end
		}
	}
	return lines
}

// commentStart returns the index where a line's comment begins, or -1.
//
// A '#' only opens a comment at the start of a line or after whitespace. In a
// CODEOWNERS pattern the character is literal — the pattern token runs to the
// first unescaped space — so scanning from the first '#' anywhere reported the
// rule line `a#@org/acq @org/other` as a comment naming @org/acq.
func commentStart(line string) int {
	for i := 0; i < len(line); i++ {
		if line[i] != '#' {
			continue
		}
		if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
			return i
		}
	}
	return -1
}

// blockedOpLabels names the ops that would have applied, for R-25's refusal.
func blockedOpLabels(results []plan.OpResult) string {
	var labels []string
	for i, o := range results {
		if o.Status == "applied" || o.Status == "unchanged" && o.Reason == "" {
			labels = append(labels, policy.OpLabel(o.ID, i))
		}
	}
	if len(labels) == 0 {
		return "(none reported)"
	}
	return strings.Join(labels, ", ")
}

// ownerTokenByte reports whether c can continue a GitHub owner identifier, so
// a match that runs into one is a longer handle rather than the one renamed.
func ownerTokenByte(c byte) bool {
	return c == '-' || c == '_' || c == '.' || c == '/' ||
		('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

func (r *syncRun) findOnDisk() string {
	for _, cand := range gittree.CodeownersLocations {
		if _, err := os.Stat(filepath.Join(r.repoArg, filepath.FromSlash(cand))); err == nil {
			return cand
		}
	}
	return ""
}

// syncStatus derives R-24's verdict from the per-op results. `skipped` is not
// cosmetic: without it, a policy with one typo'd path prefix skips on every repo
// and reports 100 × `unchanged`, and the operator grouping on .status reads
// "already correct" and ships a no-op rollout.
func syncStatus(rec SyncRecord) string {
	switch {
	case rec.OpsApplied > 0:
		return StatusApplied
	case rec.OpsSkipped > 0:
		return StatusSkipped
	default:
		return StatusUnchanged
	}
}

// write converges the file on disk. Creation is seeded with an empty file rather
// than bypassing apply: creating a CODEOWNERS is the one write with no prior
// artifact to prove INV-2 against, so it gets the hash pin, the syntax validation
// and the atomic rename (R-10), not fewer of them. O_EXCL makes "--create never
// overwrites" a property of the syscall rather than of the discovery logic above.
func (r *syncRun) write(rel string, p *plan.Plan, creating bool) error {
	target := filepath.Join(r.repoArg, filepath.FromSlash(rel))
	if creating {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if err := apply.Apply(p, target); err != nil {
		if creating {
			// Never leave the empty seed behind: a zero-byte CODEOWNERS is a file
			// that governs nothing while making every later run think one exists.
			_ = os.Remove(target)
		}
		return err
	}
	return nil
}

// emitRecord writes the record to every sink the operator asked for.
//
// --out ALWAYS writes the JSON record, whatever --format says, and does not
// suppress stdout. The alternative — --out emitting whatever --format names —
// destroys the artifact the flag exists for: `--out records/$repo.json` under the
// default text format would leave a directory of prose, and the aggregation over
// it fails on the first file, after the rollout has already written every
// CODEOWNERS.
//
// STDOUT GOES FIRST, AND UNCONDITIONALLY. The fleet script's `>> results.jsonl`
// is fed from stdout and is the only durable trace of what happened to a repo;
// it must not be lost because some other sink was unwritable. Writing the file
// sinks first, returning on the first error and mapping that to ExitRefused,
// cost exactly that: `--out /nonexistent-dir/rec.json` on a repo whose
// CODEOWNERS had ALREADY been rewritten produced exit 2 and an empty stdout, so
// the script filed a converged repo under `needs-human` and left no record of
// the change it had just made. That is a reporting failure being reported as a
// repository failure, which is the one lie this record exists to prevent.
//
// A sink failure is therefore a warning and never an exit code. The verdict
// belongs to the repository: once the file on disk has converged, no unwritable
// --out path makes that false, and inventing a refusal sends a human to inspect
// a repo that is already correct. The warning goes to stderr immediately and
// into rec.Warnings, so the sinks still downstream (the summary) carry it too.
func emitRecord(rec SyncRecord, r *syncRun, format, outPath, summaryPath string, stdout, stderr io.Writer) {
	b, err := json.Marshal(rec)
	if err != nil {
		// Unreachable in practice; degrade to the human render rather than
		// emitting nothing at all.
		fmt.Fprintln(stderr, "warning: could not render the JSON record:", err)
	}
	line := append(b, '\n')

	switch {
	case err != nil && format == "json":
		// Nothing valid to write; the warning above is the whole report.
	case format == "json":
		if _, werr := stdout.Write(line); werr != nil {
			fmt.Fprintln(stderr, "warning: could not write the record to stdout:", werr)
		}
	default:
		renderRecordText(stdout, rec)
	}
	if err != nil {
		return
	}

	if outPath != "" {
		if werr := os.WriteFile(outPath, line, 0o644); werr != nil {
			w := fmt.Sprintf("write --out %s: %v (the record above is on stdout; this repo's outcome is unaffected)", outPath, werr)
			fmt.Fprintln(stderr, "warning:", w)
			rec.Warnings = append(rec.Warnings, w)
		}
	}
	if summaryPath != "" {
		if werr := os.WriteFile(summaryPath, []byte(renderSummary(rec, r)), 0o644); werr != nil {
			fmt.Fprintf(stderr, "warning: write --summary-out %s: %v (this repo's outcome is unaffected)\n", summaryPath, werr)
		}
	}
}

// renderRecordText is the human render. It is stdout's content under --format text and
// deliberately not JSON: a consumer that piped text into `jq` should fail loudly
// rather than parse a lookalike.
func renderRecordText(w io.Writer, rec SyncRecord) {
	fmt.Fprintf(w, "%s: %d op(s) applied, %d skipped; %d line change(s), %d path(s) change owners\n",
		rec.Status, rec.OpsApplied, rec.OpsSkipped, len(rec.Changes), rec.PathsChanged)
	for i, o := range rec.Ops {
		label := policy.OpLabel(o.ID, i)
		switch {
		case o.Reason != "":
			fmt.Fprintf(w, "  %s  %s: %s\n", label, o.Status, o.Reason)
		case o.Proven != "":
			fmt.Fprintf(w, "  %s  %s (proven: %s)\n", label, o.Status, o.Proven)
		default:
			fmt.Fprintf(w, "  %s  %s\n", label, o.Status)
		}
		// R-32: the carve-out facts render in every format, not just JSON — an
		// operator reading the text output must see who ended up holding the
		// excepted paths without re-running under --format json.
		for _, e := range o.Excepted {
			fmt.Fprintf(w, "    excepted: %s stays with %s\n", e.Path, fmtOwners(e.Owners))
		}
		for _, pat := range o.ExceptUnmatched {
			fmt.Fprintf(w, "    except %s matched no tracked file; the grant carries no carve for it (on_except_zero_match=allow)\n", pat)
		}
	}
	if rec.Created {
		fmt.Fprintln(w, "  created a new CODEOWNERS file")
	}
	if rec.Error != "" {
		fmt.Fprintln(w, "  "+rec.Error)
	}
}

// renderSummary is the PR body (R-24). It names the policy — that is what `name`
// and `description` are for — and calls out every op proven only structurally, so
// a reviewer finds the weakened INV-1 cases (INV-6) without reading the diff.
func renderSummary(rec SyncRecord, r *syncRun) string {
	var b strings.Builder
	title := "CODEOWNERS sync"
	if r.policy != nil && r.policy.Name != "" {
		title += " — " + r.policy.Name
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	if r.policy != nil && r.policy.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", r.policy.Description)
	}
	fmt.Fprintf(&b, "- repo: `%s`\n", rec.Repo)
	// The file the PR changes. A reviewer of one of a hundred identical PRs
	// needs to know whether this repo's ownership lives in .github/, the root
	// or docs/ before the diff means anything.
	if rec.CodeownersPath != "" {
		fmt.Fprintf(&b, "- file: `%s`\n", rec.CodeownersPath)
	}
	fmt.Fprintf(&b, "- status: `%s`\n", rec.Status)
	fmt.Fprintf(&b, "- ops applied: %d, skipped: %d\n", rec.OpsApplied, rec.OpsSkipped)
	fmt.Fprintf(&b, "- paths whose owners change: %d\n", rec.PathsChanged)
	// Under --dry-run `created` reports what the run WOULD do, so the past tense
	// here contradicted the --dry-run bullet three lines below in the same PR
	// body ("a new CODEOWNERS file was written" … "nothing was written"). A
	// reviewer reading a preview cannot be left to guess which sentence is true.
	if rec.Created && !r.dryRun {
		fmt.Fprintf(&b, "- a new CODEOWNERS file was written (`--create`)\n")
	}
	if rec.Created && r.dryRun {
		fmt.Fprintf(&b, "- a new CODEOWNERS file WOULD be created (`--create`)\n")
	}
	if r.dryRun {
		fmt.Fprintf(&b, "- `--dry-run`: nothing was written; this is what the run would do\n")
	}
	if rec.Error != "" {
		fmt.Fprintf(&b, "\n## Not applied\n\n%s\n", rec.Error)
	}

	// Warnings belong in the PR body, not only in results.jsonl. Each one is a
	// thing a human should look at in a repo that nonetheless converged — a
	// second CODEOWNERS file, a line GitHub cannot parse, a comment naming a
	// team this run renamed away — and the PR is the one moment someone is
	// already looking at that file and can fix it in the same commit. Left to
	// the record alone they get aggregated into a count and forgotten.
	if len(rec.Warnings) > 0 {
		b.WriteString("\n## Worth a look\n\n")
		for _, w := range rec.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}

	if len(rec.Ops) > 0 {
		// `proven` holds only tree/structural. It used to double as the skip
		// reason, which put a full sentence in the column a reviewer scans for
		// one of two short words — across a hundred near-identical PRs.
		b.WriteString("\n## Ops\n\n| id | op | status | proven | why | note |\n|---|---|---|---|---|---|\n")
		for i, o := range rec.Ops {
			id := policy.OpLabel(o.ID, i)
			note := ""
			if r.policy != nil {
				note = r.policy.Notes[id]
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s | %s | %s |\n", id, o.Op, o.Status, o.Proven, o.Reason, note)
		}
	}

	// R-32: the PR reviewer reads this file, not results.jsonl, so the
	// carve-out facts have to be here too — who ended up holding each excepted
	// path, and any except pattern that bit nothing.
	var carve []string
	for i, o := range rec.Ops {
		label := policy.OpLabel(o.ID, i)
		for _, e := range o.Excepted {
			carve = append(carve, fmt.Sprintf("- `%s`: `%s` stays with %s", label, e.Path, fmtOwners(e.Owners)))
		}
		for _, pat := range o.ExceptUnmatched {
			carve = append(carve, fmt.Sprintf("- `%s`: except `%s` matched no tracked file — the grant carries NO carve for it (`on_except_zero_match: allow`)", label, pat))
		}
	}
	if len(carve) > 0 {
		b.WriteString("\n## Carve-outs (`except`)\n\n")
		b.WriteString(strings.Join(carve, "\n") + "\n")
	}

	var structural []string
	for i, o := range rec.Ops {
		if o.Proven == "structural" {
			structural = append(structural, fmt.Sprintf("- `%s` — `%s`", policy.OpLabel(o.ID, i), o.Op))
		}
	}
	if len(structural) > 0 {
		b.WriteString("\n## Proven structurally, not against the tree (INV-6)\n\n")
		b.WriteString("Nothing tracked in this repository matches these scopes (or, for an `except` op\n" +
			"allowed past its zero match, matches the promised carve-out), so structure is the\n" +
			"whole proof — the tool cannot show the rule does what you meant. Read these lines\n" +
			"in the diff.\n\n")
		b.WriteString(strings.Join(structural, "\n") + "\n")
	}
	return b.String()
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opSpecs, policyPaths multiFlag
	fs.Var(&opSpecs, "op", "operation to syntax-check (repeatable); mutually exclusive with --policy")
	fs.Var(&policyPaths, "policy", "policy file to validate; mutually exclusive with --op")
	format := fs.String("format", "text", "text|json")
	// No --repo, --branch, --file, --create, --dry-run or --summary-out: check
	// reads no repository, and the shape of the verb is what enforces that (R-22).
	// An unknown flag is a parse error, which is exit 3 below.
	if err := fs.Parse(args); err != nil {
		return flagParseCode(err)
	}
	if *format != "text" && *format != "json" {
		return exit3(stderr, fmt.Errorf("unknown --format %q; want text or json", *format))
	}
	pol, opList, err := opSource(opSpecs, policyPaths)
	if err != nil {
		return exit3(stderr, err)
	}
	if err := validateScopes(opList); err != nil {
		return exit3(stderr, err)
	}
	// The statically provable half of R-8, settled here with no repository
	// open, so both verbs give the same verdict for the same policy on every
	// repo. plan.Build keeps the other half — an overlap only a real tree
	// reveals stays exit 2, per repo, and the fleet loop still steps over it.
	if err := ops.StaticConflict(opList); err != nil {
		return exit3(stderr, err)
	}

	// Exit 0, never 1. `check` is the first line of every fleet script under
	// `set -e`; a clean policy returning the no-op code would abort the run
	// before the loop starts.
	if *format == "json" {
		doc := struct {
			Valid  bool   `json:"valid"`
			Policy string `json:"policy,omitempty"`
			Name   string `json:"name,omitempty"`
			Ops    int    `json:"ops"`
		}{Valid: true, Ops: len(opList)}
		if len(policyPaths) == 1 {
			doc.Policy = policyPaths[0]
		}
		if pol != nil {
			doc.Name = pol.Name
		}
		b, err := json.Marshal(doc)
		if err != nil {
			return exit3(stderr, err)
		}
		fmt.Fprintln(stdout, string(b))
		return ExitOK
	}
	what := "ops"
	if len(policyPaths) == 1 {
		what = policyPaths[0]
	}
	fmt.Fprintf(stdout, "ok: %s — %d op(s), no policy errors\n", what, len(opList))
	return ExitOK
}
