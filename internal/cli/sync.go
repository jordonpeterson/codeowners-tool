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
	"sort"
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
// the behavior itself is correct. Every exit-3 return in cmdSync goes through
// this (via the exit3s choke point there): the hazard is the same whichever
// pre-repo verdict fired, and covering only some of them lost the rest of the
// repos silently — the exact failure the note exists to disclose.
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

// refuseGitDirPath rejects a --file naming anything inside a git directory.
//
// containedRelPath closes the spellings that leave --repo; this one closes the
// spelling that stays inside it and is still not the repository's content.
// `--file .git/CODEOWNERS --create` was accepted with only the "GitHub does not
// read this path" warning and exit 0, and wrote a real file into git's own
// storage — once per clone across a fleet, in the one directory nobody diffs.
// Nothing GitHub loads can live there (S-8 reads three paths, none of them under
// .git), so the write governs nothing by construction, which makes accepting it
// the "applied, dead on arrival" outcome these verbs exist to prevent.
//
// Any component matches, not just the first: `vendor/lib/.git/CODEOWNERS` is a
// submodule's git directory, and the comparison is case-insensitive because on a
// case-folding filesystem `.GIT/x` is the same directory git protects.
//
// Exit 3, matching `--file ../escape/…`: decidable from the argument alone with
// no repository open, so the verdict is identical on all 100 repos and the run
// halts at the first rather than recording the same refusal a hundred times.
func refuseGitDirPath(p string) error {
	if p == "" {
		return nil
	}
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(filepath.FromSlash(p))), "/") {
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf("--file %q names a path inside a .git directory: GitHub loads CODEOWNERS only from %s, so a file written there governs nothing, and git's own directory is not somewhere this tool may deposit one — name a path in the repository's content instead",
				p, strings.Join(gittree.CodeownersLocations, ", "))
		}
	}
	return nil
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
	// policyPath is that policy's path as the operator spelled it, and is what
	// the record and the PR body name. Both have to say WHICH artifact ran:
	// R-20's claim is that the policy file is the complete statement of what
	// happened, and a record that cannot name it makes the claim uncheckable a
	// week later, when two waves have landed and results.jsonl is all anyone
	// kept.
	policyPath string
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
	create := fs.Bool("create", false, "write .github/CODEOWNERS when the repo has none; never overwrites (R-23) (only with --op)")
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
	// The one choke point for sync's exit-3 class: every verdict in it is
	// reached before the repository is opened, so NONE of them produces a
	// record — and when --format json, --out or --summary-out asked for one,
	// that absence has to be said out loud (see noRecordNote). Routing every
	// exit-3 return through this closure is what keeps a refusal added later
	// from silently dropping a repo out of a fleet's aggregation — the exact
	// hazard the note discloses; the first fix covered only two of the ten
	// paths, and the other eight lost repos silently.
	exit3s := func(err error) int {
		return noRecordNote(stderr, *format, *out, *summaryOut, exit3(stderr, err))
	}
	// Argument-only, like every exit-3 verdict below: a stray positional arg
	// means the parser read nothing after it, flags included.
	if err := rejectLeftoverArgs(fs); err != nil {
		return exit3s(err)
	}
	if err := rejectEmptyRepo(fs, *repo); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitInvalid
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
		return exit3s(fmt.Errorf("unknown --format %q; want text or json", *format))
	}
	// Validated here, not on whichever repo first has a removal empty an owner
	// set. A flag value is decidable from the arguments alone, so it belongs to
	// the exit-3 class — and the policy file's `on_empty` has been validated at
	// load time all along, so leaving the flag lazy made two spellings of one
	// setting disagree: `--on-empty typo` ran a whole fleet at exit 0 and then
	// reported exit 2 ("this repo needs a human") on the one repo that happened
	// to trip it, naming a CODEOWNERS that had nothing to do with the mistake.
	if *onEmpty != "" && !validOnEmpty(*onEmpty) {
		return exit3s(fmt.Errorf("unknown --on-empty %q; want error, inherit or unowned (R-6)", *onEmpty))
	}
	if *onEmpty != "" && len(policyPaths) > 0 {
		return exit3s(errors.New("--on-empty is not allowed with --policy: set \"on_empty\" in the policy file instead, or the artifact in git is not the policy that ran (R-20)"))
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
		return exit3s(err)
	}
	// The third spelling of the same mistake, and the one containment cannot
	// see because it stays inside --repo: see refuseGitDirPath.
	if err := refuseGitDirPath(*filePath); err != nil {
		return exit3s(err)
	}
	// Argument-only, hence exit 3: a fleet run halts at repo 0 rather than
	// recording the same refusal 100 times.
	if err := gittree.ValidateRef(*branch); err != nil {
		return exit3s(err)
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
		return exit3s(err)
	}
	if maxPathsSet && *maxPaths < 0 {
		return exit3s(fmt.Errorf("--max-paths-changed %d must be zero or positive; omit the flag to set no ceiling (R-25)", *maxPaths))
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
		return exit3s(errors.New("--create is not allowed with --policy: set \"create\" in the policy file instead, or the artifact in git is not the policy that ran (R-20/R-34)"))
	}
	if maxPathsSet && len(policyPaths) > 0 {
		// Mirrors --on-empty exactly, and for the same reason: the ceiling is a
		// claim about the INTENT ("this wave touches dozens of files per repo,
		// not thousands"), so it belongs in the artifact a reviewer approves.
		// A flag that could override the file would let one call site quietly
		// loosen a reviewed policy, and any "lower of the two wins" scheme is a
		// new precedence rule for operators to learn.
		return exit3s(errors.New("--max-paths-changed is not allowed with --policy: set \"max_paths_changed\" in the policy file instead, or the artifact in git is not the policy that ran (R-20/R-25)"))
	}
	if err := validateScopes(opList); err != nil {
		return exit3s(err)
	}
	// The statically provable half of R-8, settled here with no repository
	// open, so both verbs give the same verdict for the same policy on every
	// repo. plan.Build keeps the other half — an overlap only a real tree
	// reveals stays exit 2, per repo, and the fleet loop still steps over it.
	if err := ops.StaticConflict(opList); err != nil {
		return exit3s(err)
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
		run.policyPath = policyPaths[0]
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
	// Policy is set on EVERY record this run can produce, refusals included:
	// the refused rows are the ones an operator re-reads afterwards, and "which
	// wave was this?" is the first question asked of a `needs-human` pile.
	rec := SyncRecord{Repo: r.repoArg, Policy: r.policyPath, DryRun: r.dryRun}

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
	// Ahead of the warnings, alone among the guards below: the untracked
	// warning tells the operator to commit the file, and this is the one
	// repository where they cannot. Reporting both would put contradictory
	// advice in one record. See refuseIgnoredCodeowners.
	if err := refuseIgnoredCodeowners(r.repoArg, r.branch, tree, rel); err != nil {
		rec.Status = StatusRefused
		rec.Error = err.Error()
		return rec, ExitRefused
	}
	// Warnings about the FILE, gathered before anything is planned so a repo
	// that goes on to refuse still reports them: the conditions below are
	// exactly the ones an operator wants listed across the fleet afterwards.
	fileWarnings := governingWarnings(tree, r.branch, rel, creating, content)
	rec.Warnings = fileWarnings

	// Where the write would actually land, decided before anything is planned.
	// The path came out of the repository itself — discovery reads the tracked
	// tree — so a committed symlink chooses it, and containedWritePath is what
	// keeps that choice inside the clone. Refusal, not error: the repo was read
	// fine and the tool is declining to write into it, and it is a fact about
	// THIS clone, so exit 2 and the fleet loop steps to the next one.
	target := filepath.Join(r.repoArg, filepath.FromSlash(rel))
	if err := containedWritePath(r.repoArg, target); err != nil {
		rec.Status = StatusRefused
		rec.Error = err.Error()
		return rec, ExitRefused
	}
	// The refusal containment cannot make: a symlink whose target stays INSIDE
	// the clone is still a CODEOWNERS GitHub does not follow, so writing
	// through it is the same dead-on-arrival outcome with a live-looking path.
	// See refuseSymlinkedTarget.
	if err := refuseSymlinkedTarget(target); err != nil {
		rec.Status = StatusRefused
		rec.Error = err.Error()
		return rec, ExitRefused
	}
	// And the one neither of those can see, because it is a fact about the
	// TREE rather than about the filesystem: a submodule mounted at a
	// CODEOWNERS location, or a tracked link no longer on disk. See
	// refuseNonTreeAncestor — ordered after the symlink guard, which names a
	// live link and the component it sits on more precisely.
	if err := refuseNonTreeAncestor(r.repoArg, r.branch, tree, rel); err != nil {
		rec.Status = StatusRefused
		rec.Error = err.Error()
		return rec, ExitRefused
	}
	// And the one about WHICH file governs a repository mid-migration: a
	// higher-precedence CODEOWNERS sitting in the working tree makes the
	// tracked file this run discovered the outgoing one. See
	// refuseWorkTreeSupersede.
	if err := r.refuseWorkTreeSupersede(tree); err != nil {
		rec.Status = StatusRefused
		rec.Error = err.Error()
		return rec, ExitRefused
	}
	// And the one that is about the BYTES just read rather than the path they
	// came from: a CODEOWNERS git still reports as unmerged is both sides of a
	// conflict, not any commit's rules. See refuseUnmergedCodeowners.
	if err := refuseUnmergedCodeowners(r.repoArg, rel); err != nil {
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
	rec.OwnersRemoved = lostAccess(p.Rows)
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
// CODEOWNERS, so proving against another ref is exactly its job — which is
// why `apply`, the verb that turns that artifact into a write, carries this
// same guard against the plan's own ref. Without it the refusal above was an
// instruction to route around itself: `sync --branch main` on a clone standing
// elsewhere refused and offered `plan`, and plan → apply then performed the
// very write it had refused, against `.github` mounted as a submodule.
//
// Refs are compared by resolved commit, not by name, so the ordinary fleet
// invocation `--branch main` on a clone checked out at main writes as it always
// did — as does a tag or a second branch pointing at the same commit, where the
// tree is the same tree.
func (r *syncRun) checkBranchIsWritable() error {
	return checkBranchIsWritable(r.repoArg, r.branch, "sync", r.dryRun)
}

// refPhrase names a ref the way the verb's operator set it, so both refusals
// below agree on wording. `apply` has neither --branch nor --dry-run — its ref
// came from the plan file — so naming a flag its operator never typed would
// send them looking for one that does not exist.
func refPhrase(branch, verb string) string {
	if verb == "apply" {
		return "the ref this plan was computed against, " + branch + ","
	}
	return "--branch " + branch
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
		// A ref that does not resolve here is a fact about this clone — the
		// branch was deleted or renamed since the plan was written, or was
		// never fetched — so it is exit 2 like the mismatch below, not an
		// internal error. It reaches an operator, so it says that rather
		// than echoing `git rev-parse ... fatal: Needed a single revision`,
		// which names a plumbing command nobody ran.
		return &plan.RefusalError{Msg: fmt.Sprintf(
			"%s does not resolve in this clone, so %s cannot prove the change against its tree — fetch it, or re-run `plan` against a ref this clone has (S-7)",
			refPhrase(branch, verb), verb)}
	}
	if head != want {
		// Each verb names the ref the way ITS operator set it and offers only
		// advice it can act on. The `plan` escape hatch is offered only to
		// sync, whose intent is a set of ops and so can be expressed as an
		// artifact for another ref; a lint pass is not expressible that way,
		// and pointing someone at a verb that cannot do what they asked is
		// worse than saying nothing. `apply` has neither --branch nor
		// --dry-run — the ref came from the plan file — so the sync wording
		// would send its operator to two flags that do not exist.
		named := "--branch " + branch
		advice := fmt.Sprintf("re-run with --dry-run to preview it, or check out %s first", branch)
		switch verb {
		case "sync":
			advice += ", or use `plan` to produce an artifact for that ref"
		case "apply":
			named = "this plan was computed against " + branch + ", which"
			advice = fmt.Sprintf("check out %s and re-run, or re-run `plan` against the ref this clone is on", branch)
		}
		return fmt.Errorf("%s is not what this clone has checked out (HEAD is %s): %s proves the change against %s's tree but writes the working tree, so the rule would be justified by one tree and land in another; %s (S-7)",
			named, headLabel(repoDir, head), verb, branch, advice)
	}
	return nil
}

// refuseUnmergedCodeowners refuses to read a CODEOWNERS that git reports as
// unmerged — the file a conflicted merge, rebase or cherry-pick left with both
// sides still in it.
//
// Every invariant is proven against the working-tree bytes, and for an
// unmerged path those bytes are a merge artifact rather than any commit's
// rules. `sync` saw only S-3 syntax errors in the markers, kept BOTH sides'
// conflicting rules live — `=======` is not even invalid, it parses as a
// zero-owner rule (S-9) — and reported `applied (proven: tree)` at exit 0,
// having rewritten a file that is still `UU` and cannot be committed as it
// stands. The "before" ownership it proved against is a state no commit has
// ever had and GitHub will never see; `git add` afterwards would resolve
// somebody's merge with content the tool synthesized from that state.
//
// Scoped to the governing file by pathspec, deliberately: a conflict in some
// OTHER file changes neither these bytes nor the tree the proof resolves
// against, and refusing there would block the CODEOWNERS edit exactly when
// someone is reconciling a merge. A rebase merely in progress is not the
// condition either — the question is what git says about this FILE.
//
// Not lifted by --dry-run, unlike checkBranchIsWritable: there the bytes are
// real and only the ref is wrong, while here a preview reports ownership
// derived from text that is not any version of the file.
//
// Exit 2, not 3: an unmerged index is a fact about THIS clone, so the fleet
// loop records it and steps to the next repo.
func refuseUnmergedCodeowners(repoDir, rel string) error {
	clean := relClean(rel)
	// :(literal) because rel can come from --file, and a path beginning with
	// `:` is pathspec MAGIC to git, not a path: without it, `--file
	// ':weird/CODEOWNERS'` on a repo where that file is unmerged comes back
	// with no matching entry at all, and the run rewrites the conflicted file
	// reporting `applied (proven: tree)` — the very bug this guard exists for.
	// A glob metacharacter cannot cause that: the loop below already compares
	// the whole path exactly, so a wider match would be filtered out.
	//
	// It is load-bearing a second time, in the loop: a single literal path
	// makes rename detection impossible, so git never emits the two-record
	// `R  <to>\0<from>\0` form whose second record carries no status code.
	out, err := gitBytes(repoDir, "status", "--porcelain", "-z", "--untracked-files=no", "--", ":(literal)"+clean)
	if err != nil {
		// Refusal, not an error: `ls-tree` already succeeded here, so a status
		// that fails is this checkout being unreadable in a way the run cannot
		// prove anything through — and exit 3 is reserved for facts that hold
		// in every repo.
		return &plan.RefusalError{Msg: fmt.Sprintf("refusing: cannot tell whether %s is unmerged (%v) — a conflicted CODEOWNERS would be proven against both sides of a merge at once, so this run stops rather than guess; nothing was written", clean, err)}
	}
	if code, ok := unmergedCodeIn(out, clean); ok {
		// `git rm` for a modify/delete conflict (UD/DU), where the resolution
		// may be to keep the deletion rather than a merged file.
		return &plan.RefusalError{Msg: fmt.Sprintf(
			"refusing: %s is unmerged in this checkout (git status --short: %s) — a conflicted merge, rebase or cherry-pick left both sides in the file, and `=======` parses as a valid zero-owner rule (S-9), so the ownership this run would prove against is a conflict-mangled state no commit has ever had and GitHub will never see; resolve the conflict, then `git add %s` (or `git rm` it, for a modify/delete conflict), then re-run — nothing was written",
			clean, code, clean)}
	}
	return nil
}

// unmergedCodeIn reports the status code `git status --porcelain -z` gives
// path, when that code is an unmerged one.
//
// Split out to be testable. Every record git can emit for a single literal
// pathspec is reachable from a repository, but the two malformed shapes this
// guards against are not: the pathspec makes rename detection impossible, so
// the codeless second record of an `R  <to>\0<from>\0` pair never arrives, and
// a decoy path is filtered by the pathspec before the parser sees it. Both
// would be reachable the moment the pathspec changed, and a parser whose
// correctness rests on its caller's argument is one refactor from refusing a
// repository that is perfectly merged.
func unmergedCodeIn(out []byte, path string) (string, bool) {
	for _, entry := range bytes.Split(out, []byte{0}) {
		// A record is "XY<space>path". Requiring the space keeps a PATH that
		// merely begins with two status letters from being read as a code:
		// `UUx.github/CODEOWNERS` would otherwise parse as XY="UU" with
		// path=".github/CODEOWNERS", refusing a fully merged repository.
		if len(entry) < 4 || entry[2] != ' ' || string(entry[3:]) != path {
			continue
		}
		if code := string(entry[:2]); unmergedStatus(code) {
			return code, true
		}
	}
	return "", false
}

// refuseIgnoredCodeowners refuses to write a CODEOWNERS that is untracked AND
// matched by this repository's own ignore rules.
//
// governingWarnings discloses the untracked case and tells the operator to
// commit the file, which is exactly right for D5's nightly-convergence case.
// It is advice nobody can follow here: with `.github/CODEOWNERS` in
// `.gitignore`, `git add` refuses it, so no re-run of any kind can make this
// the file GitHub reads. That is the "applied, dead on arrival" outcome the
// write path already refuses for a symlinked target, a submodule mount and a
// path under `.git` — the write governs nothing BY CONSTRUCTION rather than
// merely governing nothing yet — so it is refused on the same grounds.
//
// Only for a file the ref does not carry: check-ignore consults the index and
// answers "not ignored" for a tracked path, and the tree check makes that
// explicit rather than relying on it.
//
// Exit 2, not 3: what this repository ignores is a fact about THIS repository,
// so the fleet loop records it and steps to the next.
func refuseIgnoredCodeowners(repoDir, ref string, tree []string, rel string) error {
	clean := relClean(rel)
	if trackedAt(tree, clean) || !gitIgnored(repoDir, clean) {
		return nil
	}
	return &plan.RefusalError{Msg: fmt.Sprintf(
		"refusing: %s is not tracked at %s and this repository's ignore rules match it, so `git add %s` refuses it and no re-run can ever make it the CODEOWNERS GitHub reads — the change would be proven against bytes no commit can hold. Drop the ignore rule (or `git add -f %s`) and re-run, or point --file at a path this repository can track — nothing was written",
		clean, ref, clean, clean)}
}

// gitIgnored reports whether git's ignore rules match rel in this checkout.
// `check-ignore -q` exits 0 for an ignored path and 1 for one that is not, and
// consults the index, so a tracked file never reads as ignored. Any other
// failure answers "not ignored": this is a refusal's evidence, and refusing on
// a lookup that never happened would stop runs the tool has nothing against.
func gitIgnored(repoDir, rel string) bool {
	cmd := exec.Command("git", "-C", repoDir, "check-ignore", "-q", "--", rel)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run() == nil
}

// refuseWorkTreeSupersede refuses a run whose discovered CODEOWNERS is already
// being replaced: the tracked file governs today, and a higher-precedence one
// is sitting in the working tree.
//
// Mid-migration from root `CODEOWNERS` to `.github/CODEOWNERS`, with the new
// file staged and not yet committed, discovery saw only the ref — governing()'s
// working-tree fallback fires only when the TREE has zero CODEOWNERS — so
// `sync` amended the OUTGOING file, reported `applied (proven: tree)` at exit
// 0, and put that path in `codeowners_path` for the fleet loop to stage. Under
// S-8 the edit is dead the moment the migration commit lands.
//
// Neither file can be edited soundly: the tracked one governs a state that is
// ending, and the staged one governs nothing yet, so a proof against either is
// a proof about ownership that is not the repository's. Refusal over a warning,
// matching the tool's posture for a write that is dead on arrival — and --file
// is the escape hatch for an operator who knows which half of the migration
// they are in.
//
// Only on the DISCOVERY path: with --file the operator named the file, and
// naming it is precisely the decision this refusal asks for.
//
// Exit 2, not 3: which files a clone has staged is a fact about THIS
// repository, so the fleet loop records it and steps to the next.
func (r *syncRun) refuseWorkTreeSupersede(tree []string) error {
	if r.filePath != "" {
		return nil
	}
	present := gittree.FindCodeownersPaths(tree)
	if len(present) == 0 {
		return nil
	}
	onDisk := r.findOnDisk()
	if !outranksCodeowners(onDisk, present[0]) {
		return nil
	}
	// A file git is told never to track can never supersede anything, so it is
	// not a migration — it is a stray. findOnDisk is a bare os.Stat walk and
	// honours no ignore rules, while audit's half of this check runs over
	// `ls-files --exclude-standard`; without this the two disagreed about one
	// repository, and the refusal's own advice ("commit the migration first")
	// was something `git add` declines.
	if gitIgnored(r.repoArg, onDisk) {
		return nil
	}
	return &plan.RefusalError{Msg: fmt.Sprintf(
		"refusing: this repository is governed by %s at %s, but %s is in the working tree and outranks it — GitHub loads only the first of .github/ > root > docs/ (S-8), so an edit to %s stops applying the moment %s is committed, while %s governs nothing until then. Commit the migration first, or pass --file to say which of the two this run should edit — nothing was written",
		present[0], r.branch, onDisk, present[0], onDisk, onDisk)}
}

// unmergedStatus reports whether a `git status --porcelain` code marks a path
// as unmerged: either side U, both added, or both deleted (git-status(1)).
func unmergedStatus(code string) bool {
	switch code {
	case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
		return true
	}
	return false
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
	// No --end-of-options here: in --abbrev-ref (filter) mode rev-parse ECHOES
	// the operator as an output line, so the S-7 refusal read "HEAD is
	// --end-of-options\nmain (sha)" and the detached check below could never
	// fire. "HEAD" is a literal this function supplies, never operator input,
	// so there is nothing for the operator to smuggle past.
	name, err := gitLine(repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || name == "" || name == "HEAD" {
		return short
	}
	return name + " (" + short + ")"
}

// gitLine runs a git command that answers with a single line.
func gitLine(repoDir string, args ...string) (string, error) {
	out, err := gitBytes(repoDir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitBytes runs a git command whose output is read verbatim — NUL-separated
// records, where trimming would eat the separator the parse depends on.
func gitBytes(repoDir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
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
// A file tracked in the ref but absent from the working tree is the first kind:
// the repository was read, the disagreement between the two was noticed, and the
// tool declined to write over a file it could not read. So is a create that
// would supersede the file this repo is governed by.
func statusForReadFailure(err error) string {
	var missing *noCodeownersError
	if errors.As(err, &missing) {
		return StatusRefused
	}
	var absent *trackedButAbsentError
	if errors.As(err, &absent) {
		return StatusRefused
	}
	var superseding *supersedingCreateError
	if errors.As(err, &superseding) {
		return StatusRefused
	}
	return StatusError
}

// trackedButAbsentError is the ref and the working tree disagreeing: this
// CODEOWNERS is tracked at rel, and there is no file there on disk.
//
// The two answers come from different places by design — discovery runs over
// `git ls-tree` (S-8/D5) while the bytes come from the working tree — and when
// they disagree a create is the one thing that must not happen. os.ErrNotExist
// meant "this repo has no CODEOWNERS", so `create` planned against an empty
// file and wrote the result: a sparse clone that excludes /.github/ had its
// entire ownership file replaced by the policy's ops, reported "applied",
// created:true, paths_changed:1, exit 0. Nothing downstream could catch it —
// a reviewed max_paths_changed ceiling counts changed paths, and one rule
// replacing a whole file is one path.
//
// Exit 2, not 3: which paths a clone has checked out is a fact about THIS
// repository, so the fleet loop records it and steps to the next one.
type trackedButAbsentError struct {
	rel string
	ref string
	// dangling distinguishes "nothing is at that path" from "something is, and
	// it resolves to nothing" — a committed symlink whose target is missing.
	// Both are the same refusal, but telling an operator to restore a file that
	// is sitting right there sends them looking for the wrong thing.
	dangling bool
}

func (e *trackedButAbsentError) Error() string {
	// Deliberately offers no create advice of either spelling. Following it
	// here is what destroys the file: the tracked rules are exactly the ones
	// this run cannot see.
	state := "is missing from the working tree"
	fix := fmt.Sprintf("Restore it (`git sparse-checkout` to include %s, or `git checkout -- %s`) and re-run", e.rel, e.rel)
	if e.dangling {
		state = "is a symlink in the working tree whose target does not exist"
		fix = fmt.Sprintf("Restore what %s points at, or replace the link with a real file, and re-run", e.rel)
	}
	return fmt.Sprintf("refusing: CODEOWNERS is tracked at %s in %s but %s, so this run cannot read the rules it would be rewriting and would replace a file whose contents it never saw. This is a checkout problem — a sparse-checkout that excludes that path, a partial clone, or a file deleted locally — not a repository that has no CODEOWNERS yet. %s",
		e.rel, e.ref, state, fix)
}

// unreadableCodeownersError is a read that failed for any reason that is NOT
// absence: EISDIR, a permission denial, an I/O error.
//
// Absence is the only read failure that can mean "this repo has no CODEOWNERS",
// and treating the others as absence puts trackedButAbsentError's data loss back
// with a different errno in front of it — the file is there, unread, and the run
// writes over it from an empty starting point. Naming the failure also keeps the
// operator from adding a create setting to a policy that was never the problem.
type unreadableCodeownersError struct {
	rel string
	err error
}

func (e *unreadableCodeownersError) Error() string {
	return fmt.Sprintf("refusing: %s could not be read (%v) — every write this tool makes is proven against the file's current bytes, and a read that fails for a reason other than the file being absent leaves nothing to prove against; fix that path in this checkout and re-run", e.rel, e.err)
}

func (e *unreadableCodeownersError) Unwrap() error { return e.err }

// relClean is the canonical spelling of a repo-relative path: slash-separated
// and cleaned, so `./.github/CODEOWNERS`, `.github//CODEOWNERS` and
// `docs/../CODEOWNERS` all spell the file they name. Every comparison against
// a git-reported path goes through it — git lists clean slash paths, and
// comparing an operator's raw --file spelling against one reported a live
// change as "governs nothing" (S-8) in the warning, the --out record and the
// --summary-out PR body.
func relClean(rel string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
}

// trackedAt reports whether rel is one of the paths git lists for the ref.
// Comparison is against the cleaned, slash-separated spelling because rel can
// come from --file, where `./docs/CODEOWNERS` names the tracked `docs/CODEOWNERS`.
func trackedAt(tree []string, rel string) bool {
	want := relClean(rel)
	for _, p := range tree {
		if p == want {
			return true
		}
	}
	return false
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
	// file is the --file argument when one was given, and existing the
	// CODEOWNERS this repository is governed by, "" when it has none.
	//
	// Without them the refusal was built from a fixed head and was false three
	// times over on `sync --file OWNERS` in a repo carrying all three files:
	// it claimed the repo has no CODEOWNERS, it offered `--create` at
	// .github/CODEOWNERS when --create would write OWNERS, and it advised
	// `--file` to somebody who had just passed it. That text is what a fleet's
	// `--out` record carries, so a `needs-human` pile reading those rows
	// concluded those repos had no CODEOWNERS at all.
	file     string
	existing string
}

func (e *noCodeownersError) Error() string {
	if e.file != "" {
		return e.fileError()
	}
	const head = "no CODEOWNERS file found in .github/, root, or docs/ at "
	if e.policy {
		return head + e.ref + `; set "create": true in the policy file to write one at .github/CODEOWNERS, or --file to name a path (R-23/R-34)`
	}
	return head + e.ref +
		"; re-run with --create to write one at .github/CODEOWNERS, or --file to name a path (R-23)"
}

// fileError is the same refusal about the path --file actually named: the
// create remedy points at that path, because that is where --create would
// write, and the alternative offered is the one the operator does not already
// have — amend the governing file, when there is one.
func (e *noCodeownersError) fileError() string {
	rel := relClean(e.file)
	create := fmt.Sprintf("re-run with --create to write one at %s", rel)
	tag := "(R-23)"
	if e.policy {
		create = fmt.Sprintf(`set "create": true in the policy file to write one at %s`, rel)
		tag = "(R-23/R-34)"
	}
	head := fmt.Sprintf("no CODEOWNERS at %s, the path --file names: it is not tracked at %s and not in the working tree", rel, e.ref)
	if e.existing != "" {
		return fmt.Sprintf("%s, while this repository IS governed by %s — drop --file to amend that file, or %s %s", head, e.existing, create, tag)
	}
	return fmt.Sprintf("%s, and this repository has none at %s either — %s %s",
		head, strings.Join(gittree.CodeownersLocations, ", "), create, tag)
}

// governing finds the CODEOWNERS this run edits and reads its current bytes.
//
// Discovery falls back to the WORKING TREE (D5). FindCodeownersPaths runs over
// `git ls-tree`, so a file created by pass 1 and not yet committed is invisible
// to pass 2: the tool would see "no CODEOWNERS" again, --create never overwrites,
// and there is no third outcome — a nightly job could never converge. Bytes come
// from the working tree for the same reason `plan` reads them there: that is what
// apply mutates.
//
// Those two sources can disagree, and that disagreement is a refusal, not a
// create: see trackedButAbsentError (tracked in the ref, absent on disk) and
// unreadableCodeownersError (present and unreadable). "Creating" is reserved
// for the case where NEITHER source has a file, which is the only one where
// planning against empty bytes destroys nothing.
func (r *syncRun) governing(tree []string) (rel string, content []byte, creating bool, err error) {
	switch {
	case r.filePath != "":
		rel = r.filePath
	default:
		rel = r.existingGoverning(tree)
		if rel == "" {
			// Nothing governs yet, so --create writes where GitHub looks first.
			rel = gittree.CodeownersLocations[0]
		}
	}

	b, readErr := os.ReadFile(filepath.Join(r.repoArg, filepath.FromSlash(rel)))
	switch {
	case readErr == nil:
		return rel, b, false, nil
	case !errors.Is(readErr, os.ErrNotExist):
		// Absence is the ONLY read failure that can mean "this repo has no
		// CODEOWNERS"; everything else is a file that exists and was not read.
		return "", nil, false, &unreadableCodeownersError{rel: rel, err: readErr}
	case trackedAt(tree, rel):
		// The ref says the file is there and the disk says it is not. Refused
		// ahead of the create branch AND of the no-CODEOWNERS branch: the
		// latter's advice ("re-run with --create") is precisely what replaces
		// the tracked rules with the ops alone.
		_, lstatErr := os.Lstat(filepath.Join(r.repoArg, filepath.FromSlash(rel)))
		return "", nil, false, &trackedButAbsentError{rel: rel, ref: r.branch, dangling: lstatErr == nil}
	case !r.create:
		miss := &noCodeownersError{ref: r.branch, policy: r.policy != nil}
		if r.filePath != "" {
			// The head sentence ("no CODEOWNERS file found in .github/, root,
			// or docs/") is a claim about DISCOVERY, and discovery did not
			// run: --file named the path, and the repo may well have all
			// three files.
			miss.file, miss.existing = r.filePath, r.existingGoverning(tree)
		}
		return "", nil, false, miss
	default:
		// Creating from nothing is only true when nothing governs yet. A
		// higher-precedence location is not nothing: see supersedingCreateError.
		if existing := r.existingGoverning(tree); existing != "" && outranksCodeowners(rel, existing) {
			return "", nil, false, &supersedingCreateError{rel: rel, existing: existing}
		}
		// Creating from nothing: the "before" state is an empty file, which is
		// what INV-2 is proven against.
		return rel, nil, true, nil
	}
}

// existingGoverning is the CODEOWNERS this repo resolves from today, or "" if
// it has none: the ref's tree first, then the working tree (D5). One copy of
// that order, because discovery and the superseding-create guard below have to
// agree on which file governs — two spellings of it drift the day D5 changes.
func (r *syncRun) existingGoverning(tree []string) string {
	if present := gittree.FindCodeownersPaths(tree); len(present) > 0 {
		return present[0]
	}
	return r.findOnDisk()
}

// supersedingCreateError is `--create` at a location that OUTRANKS the
// CODEOWNERS this repo already has.
//
// The old file is left untouched, which satisfies "create never overwrites",
// and under S-8 it is also never read again: `--file .github/CODEOWNERS
// --create` on a repo governed by docs/CODEOWNERS made one op's worth of rules
// the entire repository's ownership. It reported `applied (proven: tree)`,
// exit 0, because both invariants were proven against the empty bytes of a
// file that governs everything the moment it exists — services/api/main.go
// lost @org/api-team (INV-2) and /docs/ lost @org/everyone (INV-1), and the
// tool's own `verify` calls the same change INVARIANT VIOLATED.
//
// Discovery cannot reach this: it selects the governing file, so `creating` is
// never true where one exists. `--file` names the path directly, which is why
// the guard belongs here rather than in the discovery branch (R-34d).
//
// Exit 2, not 3: which location this clone keeps its CODEOWNERS at is a fact
// about THIS repository, so the fleet loop records it and steps to the next.
type supersedingCreateError struct {
	rel      string
	existing string
}

func (e *supersedingCreateError) Error() string {
	// relClean so the refusal names the file GitHub would load, not the
	// operator's spelling of it: `--file .github//CODEOWNERS` read as
	// "creating .github//CODEOWNERS", a path that appears nowhere else.
	return fmt.Sprintf("refusing: this repository is governed by %s, and creating %s would supersede it — GitHub loads only the first of .github/ > root > docs/ (S-8), so every rule in %s would stop applying and this run's ops would become the repository's entire ownership. Drop --file to amend it where it is, or move it in its own commit first",
		e.existing, relClean(e.rel), e.existing)
}

// governingWarnings reports what is wrong with the FILE this run is about to
// edit, as distinct from what is wrong with the change.
//
// Every condition below exits 0 and reports "applied" — none is a reason to
// refuse an otherwise correct edit — and every one of them is invisible at
// fleet scale unless the run that touched the file says so.
func governingWarnings(tree []string, ref, rel string, creating bool, content []byte) []string {
	var out []string
	// A file git has never recorded. sync's D5 fallback searches the WORKING
	// TREE when the ref has no CODEOWNERS, so a template a provisioning script
	// dropped in, or a half-finished manual edit, became the baseline INV-1 and
	// INV-2 were proven against — while `plan`, `snapshot` and `audit` all exit
	// 3 on the same repository at the same moment, because GitHub reads no
	// CODEOWNERS from it. The run reported `applied (proven: tree)`, exit 0,
	// and R-23's --create gate was never consulted.
	//
	// A warning rather than a refusal, and D5's rationale is the reason: a
	// nightly job that created the file in pass 1 has to converge in pass 2,
	// and refusing every uncommitted file would make that impossible. What was
	// missing is the disclosure — the case where the operator has to commit
	// something before any of this reaches GitHub. The one shape no re-run can
	// fix is refused instead: see refuseIgnoredCodeowners.
	//
	// Skipped while CREATING: the file does not exist anywhere yet, "not
	// tracked" is not news about it, and the record already says created.
	if !creating && !trackedAt(tree, rel) {
		out = append(out, fmt.Sprintf(
			"%s is not tracked at %s — git does not record it at that ref, so GitHub reads nothing from it today and the before-state this run proved against is one GitHub has never seen; commit the file for this change to take effect",
			relClean(rel), ref))
	}
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
		// relClean on the --file side only: present comes from git, already clean.
		if relClean(rel) != present[0] && isCodeownersLocation(rel) {
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
// GitHub actually reads (S-8). Classified over the cleaned spelling, like
// trackedAt: `./.github/CODEOWNERS` governs exactly what `.github/CODEOWNERS`
// governs, and classifying the raw string drew the false "governs nothing"
// warning for an alternate spelling of a governing file.
func isCodeownersLocation(rel string) bool { return codeownersRank(rel) >= 0 }

// codeownersRank is a path's position in GitHub's search order, or -1 for a
// path GitHub never loads (S-8). Cleaned before comparison, like trackedAt.
func codeownersRank(rel string) int {
	clean := relClean(rel)
	for i, loc := range gittree.CodeownersLocations {
		if clean == loc {
			return i
		}
	}
	return -1
}

// outranksCodeowners reports whether a file at rel would take precedence over
// one at other, i.e. whether writing rel demotes other to a file GitHub never
// reads. A path outside the three locations outranks nothing.
func outranksCodeowners(rel, other string) bool {
	r, o := codeownersRank(rel), codeownersRank(other)
	return r >= 0 && o >= 0 && r < o
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
		for _, m := range commentLinesNaming(after, op.Owner) {
			// The line number is the one in the file as this run leaves it, not
			// where the comment sits today: a rule inserted above shifts it
			// down, and the message says so rather than misdirecting whoever
			// reads a --dry-run preview.
			if renamed {
				out = append(out, fmt.Sprintf(
					"%s line %d as this run leaves it: a comment still names %q, which this run renamed to %q; comments are never rewritten (the substitution is proven only over owner tokens), so this one now points at an identifier that no longer exists",
					rel, m.line, m.spelling, op.NewOwner))
				continue
			}
			out = append(out, fmt.Sprintf(
				"%s line %d: no rule in this file names %q, so there was nothing for this run to rename here — but a comment still names %q, and the rename to %q makes that comment point at an identifier that no longer exists",
				rel, m.line, op.Owner, m.spelling, op.NewOwner))
		}
	}
	return out
}

// commentLinesNaming returns the 1-based line numbers whose comment text names
// owner as a whole identifier. The boundary check is what keeps a rename of
// `@org/acq` from reporting a comment that only ever said `@org/acq-infra`.
// commentMatch is one stale comment: the line as this run leaves the file, and
// the spelling the COMMENT uses. The two can differ now that the identity folds
// (R-38a), and the warning must quote the comment's spelling — naming the op's
// instead sends the reader hunting for text the file does not contain.
type commentMatch struct {
	line     int
	spelling string
}

func commentLinesNaming(content, owner string) []commentMatch {
	want := ops.FoldOwner(owner)
	var out []commentMatch
	for i, line := range strings.Split(content, "\n") {
		hash := commentStart(line)
		if hash < 0 {
			continue
		}
		text := line[hash:]
		// Walk candidate starts and fold each equal-length slice, rather than
		// folding the line once and indexing into the result: Unicode lowering
		// can change a string's byte length, which would misplace both the
		// boundary check and any offset taken from it. Folding a slice of the
		// ORIGINAL keeps every index an index into the text as written.
		for at := 0; at+len(owner) <= len(text); at++ {
			cand := text[at : at+len(owner)]
			if ops.FoldOwner(cand) != want {
				continue
			}
			// Same boundary rule as before folding: a rename of @org/acq must
			// stay quiet about a comment that only ever named @org/acq-infra,
			// in any case. Advancing by one byte rather than past the match is
			// what lets a longer handle be rejected and a real one later on the
			// same line still be found.
			//
			// Both ends need it. The LEADING boundary is what keeps `# email
			// someone@old-team` quiet on a rename of @old-team: the match is
			// the tail of a longer token, betrayed by the byte before it being
			// an owner-token byte ('e') — or '@' itself, which ownerTokenByte
			// deliberately excludes as a continuation byte but which glued to
			// the front (`a@@old-team`) still means "embedded, not a mention".
			// `#@old-team` stays a real mention: '#' is neither.
			if at != 0 && (ownerTokenByte(text[at-1]) || text[at-1] == '@') {
				continue
			}
			end := at + len(owner)
			if end != len(text) && ownerTokenByte(text[end]) {
				continue
			}
			out = append(out, commentMatch{line: i + 1, spelling: cand})
			break
		}
	}
	return out
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
//
// Only status "applied" belongs: the caller reads the statuses BEFORE the
// applied→unchanged rewrite, so at that point "unchanged" means "already
// satisfied, changed zero paths" — an op that contributed nothing to the
// number the ceiling refused on. Naming it sent the operator narrowing an op
// that was never behind the count.
func blockedOpLabels(results []plan.OpResult) string {
	var labels []string
	for i, o := range results {
		if o.Status == "applied" {
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
	if _, err := apply.Apply(p, target); err != nil {
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
	// The spelling that ACTUALLY applied. `--policy p.json --create` is exit 3
	// (R-34b), so on a policy run the flag cannot have been passed and the
	// reviewed `"create": true` is what wrote the file — crediting the flag
	// sent the reviewer of this body looking for it in a fleet script that
	// never contained it, and gave the artifact no credit for the one setting
	// that mattered.
	how := "`--create`"
	if r.policy != nil {
		how = fmt.Sprintf("`\"create\": true` in `%s`", r.policyPath)
	}
	if rec.Created && !r.dryRun {
		fmt.Fprintf(&b, "- a new CODEOWNERS file was written (%s)\n", how)
	}
	if rec.Created && r.dryRun {
		fmt.Fprintf(&b, "- a new CODEOWNERS file WOULD be created (%s)\n", how)
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

	// Owners losing access get their own section, above the per-op table. A
	// reviewer reading a rollout PR needs "who stops owning things" before
	// "how many paths changed": five paths changing reads the same whether the
	// run co-owned them or displaced their owners, and only one of those is
	// worth stopping for. The PR is the one moment somebody is already reading.
	if len(rec.OwnersRemoved) > 0 {
		b.WriteString("\n## Owners losing access\n\n")
		for _, line := range lostAccessByOwner(rec.OwnersRemoved) {
			fmt.Fprintf(&b, "- %s\n", line)
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
	if err := rejectLeftoverArgs(fs); err != nil {
		return exit3(stderr, err)
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
			// Resolved is R-35b in the machine format. `ops` stays a COUNT —
			// it is a published key and `jq .ops` on a fleet gate must keep
			// meaning what it meant — so the echo is a new array beside it.
			Resolved []checkResolvedOp `json:"resolved,omitempty"`
		}{Valid: true, Ops: len(opList)}
		if len(policyPaths) == 1 {
			doc.Policy = policyPaths[0]
		}
		if pol != nil {
			doc.Name = pol.Name
			doc.Resolved = resolvedOps(pol, opList)
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
	renderCheckOps(stdout, pol, opList)
	return ExitOK
}

// renderCheckOps echoes each op's RESOLVED zero-match settings under the
// summary line (R-35b). With a `defaults` block the value in force at an op is
// stated nowhere near it, so the reviewer of the committed policy would have to
// fold the block in their head for all 40 ops — the arithmetic the block exists
// to remove — and a misplaced default stays invisible until some repo writes a
// declared rule nobody expected.
//
// One line per op, labelled exactly as sync's per-op lines are, so the two
// verbs name the same op the same way.
func renderCheckOps(w io.Writer, pol *policy.Policy, list []ops.Op) {
	if pol == nil {
		return // --op carries no per-op settings, so there is nothing resolved to echo
	}
	echo := pol.Defaults.OnZeroMatch != "" || pol.Defaults.OnExceptZeroMatch != "" || pol.Defaults.OnUnowned != ""
	for _, o := range list {
		echo = echo || o.OnZeroMatch != "" || o.OnExceptZeroMatch != "" || o.OnUnowned != ""
	}
	if !echo {
		// No knob is set anywhere: every op runs under R-5's require, which the
		// one-line verdict already says. Listing 40 identical lines to say it
		// again would bury the case where a value really does differ.
		return
	}
	width := 0
	for i, o := range list {
		if n := len(policy.OpLabel(o.ID, i)); n > width {
			width = n
		}
	}
	for i, o := range list {
		fmt.Fprintf(w, "  %-*s  on_zero_match: %s", width, policy.OpLabel(o.ID, i), resolvedZeroMatch(pol, o))
		// Only for an op that carries an except clause: R-27.6 makes the field
		// illegal anywhere else, so "require" there would name a setting the op
		// cannot have.
		if len(o.Except) > 0 {
			fmt.Fprintf(w, "; on_except_zero_match: %s", resolvedExceptZeroMatch(o))
		}
		if s, stated := resolvedUnowned(pol, o); stated {
			fmt.Fprintf(w, "; on_unowned: %s", s)
		}
		fmt.Fprintln(w)
	}
}

// resolvedSetting is one echoed value: what the op will actually do, plus the
// note the human render shows in parentheses after it. Both formats render from
// this one value, so `check --format text` and `check --format json` cannot come
// to say different things about the same op — which, for a value whose whole
// purpose is to be reviewed before a fleet runs, is the failure that matters.
type resolvedSetting struct {
	Value string
	Note  string
}

func (s resolvedSetting) String() string {
	if s.Note == "" {
		return s.Value
	}
	return s.Value + " (" + s.Note + ")"
}

// checkResolvedOp is one op's resolved settings in `check --format json` (R-35b).
//
// The op is named by the same label sync's per-op lines use, so an id-less op is
// `ops[3]` in both verbs and a jq report can be read against a sync record. The
// value is the bare setting a gate compares against ("declare", "skip",
// "require"), or "n/a" for a rename, whose scope comes from current ownership
// and can never zero-match; the note carries the parenthetical the human render
// shows, which is where "the defaults block does not reach this op" lives.
//
// on_except_zero_match appears only on an op that carries an `except` clause:
// R-27.6 makes the field illegal anywhere else, so emitting "require" there
// would name a setting the op cannot have.
type checkResolvedOp struct {
	ID                    string `json:"id"`
	OnZeroMatch           string `json:"on_zero_match"`
	OnZeroMatchNote       string `json:"on_zero_match_note,omitempty"`
	OnExceptZeroMatch     string `json:"on_except_zero_match,omitempty"`
	OnExceptZeroMatchNote string `json:"on_except_zero_match_note,omitempty"`
	// on_unowned appears only when the policy states the field somewhere —
	// per-op or in defaults — so a record from a policy that never mentions
	// R-40 stays byte-identical to before the field existed.
	OnUnowned     string `json:"on_unowned,omitempty"`
	OnUnownedNote string `json:"on_unowned_note,omitempty"`
}

// resolvedOps is the JSON echo, one entry per op in policy order.
//
// Unlike the human render it does not suppress the all-built-in case. The
// suppression there is a reading aid — forty identical lines bury the one that
// differs — and a fleet gate has the opposite need: `check --format json | jq`
// is the documented first line of a fleet script, and an assertion like "no op
// in this wave runs under declare" needs a row for every op, not only for the
// ops someone remembered to configure.
func resolvedOps(pol *policy.Policy, list []ops.Op) []checkResolvedOp {
	out := make([]checkResolvedOp, 0, len(list))
	for i, o := range list {
		zero := resolvedZeroMatch(pol, o)
		entry := checkResolvedOp{
			ID:              policy.OpLabel(o.ID, i),
			OnZeroMatch:     zero.Value,
			OnZeroMatchNote: zero.Note,
		}
		if len(o.Except) > 0 {
			except := resolvedExceptZeroMatch(o)
			entry.OnExceptZeroMatch, entry.OnExceptZeroMatchNote = except.Value, except.Note
		}
		if s, stated := resolvedUnowned(pol, o); stated {
			entry.OnUnowned, entry.OnUnownedNote = s.Value, s.Note
		}
		out = append(out, entry)
	}
	return out
}

// resolvedZeroMatch is what this op will actually do at zero match, after the
// defaults block has been folded in by the loader.
func resolvedZeroMatch(pol *policy.Policy, o ops.Op) resolvedSetting {
	if o.OnZeroMatch != "" {
		return resolvedSetting{Value: o.OnZeroMatch}
	}
	if o.Kind == ops.RenameOwner {
		// R-35e: a rename's scope comes from current ownership, so zero match
		// can never fire and no value is legal on it in the first place.
		return resolvedSetting{Value: "n/a", Note: "a rename has no scope to match"}
	}
	if pol.Defaults.OnZeroMatch != "" {
		// R-35e, value level: the block states a value this op cannot carry
		// (`declare` on remove_owner, or on an except-carrying op), so the op
		// keeps the built-in. Naming the block's value on this line would read
		// as if it had reached the op, which is the misreading that ends in a
		// repo refused for a reason the reviewer thought was configured away.
		return resolvedSetting{Value: ops.ZeroMatchRequire, Note: "built-in; the default does not reach this op"}
	}
	return resolvedSetting{Value: ops.ZeroMatchRequire, Note: "built-in"}
}

func resolvedExceptZeroMatch(o ops.Op) resolvedSetting {
	if o.OnExceptZeroMatch != "" {
		return resolvedSetting{Value: o.OnExceptZeroMatch}
	}
	return resolvedSetting{Value: ops.ExceptZeroMatchRequire, Note: "built-in"}
}

// resolvedUnowned is what this op will do about open paths (R-40), after the
// defaults block has been folded in by the loader. stated is false when the
// policy never mentions the field — echoing "assign (built-in)" on every
// add_owner of every pre-R-40 policy would change stable output to state a
// setting nobody asked about.
func resolvedUnowned(pol *policy.Policy, o ops.Op) (s resolvedSetting, stated bool) {
	if o.OnUnowned != "" {
		return resolvedSetting{Value: o.OnUnowned}, true
	}
	if pol.Defaults.OnUnowned == "" {
		return resolvedSetting{}, false
	}
	if o.Kind != ops.AddOwner {
		// R-40's legality table: the field is add_owner's alone, so the
		// default cannot reach this op and no value is legal on it — same
		// treatment as a rename under on_zero_match (R-35e).
		return resolvedSetting{Value: "n/a", Note: "only add_owner has an on_unowned"}, true
	}
	// The one add_owner the default does not reach: a declared op (R-40).
	if o.OnZeroMatch == ops.ZeroMatchDeclare {
		return resolvedSetting{Value: ops.UnownedAssign, Note: "built-in; the default does not reach a declared op"}, true
	}
	// Unreachable while the loader folds the default in, kept as the honest
	// fallback for a caller that built the ops another way.
	return resolvedSetting{Value: ops.UnownedAssign, Note: "built-in"}, true
}

// lostAccess reduces the planner's ownership rows to the paths whose owner set
// SHRANK, with the owners that stop owning each. The rows already carry
// before/after for every path the run moves; this is surfacing, not analysis.
//
// Owner identity is R-38a's throughout (ops.FoldOwner via ownersMissing), so a
// re-spelled handle is not reported as a loss — a fleet that cried "@Org/Team
// loses access" every time a run normalised nothing would be unreadable.
//
// A path going from owned to unowned is the sharpest loss there is, and falls
// out of the same subtraction: every before-owner is missing from an empty
// after-set.
func lostAccess(rows []plan.Row) []LostAccess {
	var out []LostAccess
	for _, r := range rows {
		if lost := ownersLost(r.Before, r.After); len(lost) > 0 {
			out = append(out, LostAccess{Path: r.Path, Owners: lost})
		}
	}
	return out
}

// ownersLost returns the members of before, in before's order, that no longer
// appear in after. Identity is R-38a's -- @handles are case-insensitive on
// GitHub -- so a run that merely re-spells a handle reports no loss. A fleet
// that cried "@Org/Team loses access" every time a spelling settled would be
// unreadable, and worse, would be wrong.
func ownersLost(before, after []string) []string {
	keep := map[string]bool{}
	for _, o := range after {
		keep[ops.FoldOwner(o)] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, o := range before {
		k := ops.FoldOwner(o)
		if keep[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, o)
	}
	return out
}

// summaryPathsPerOwner caps how many paths one owner's line names in the PR
// body. A displacing baseline on a large repository can touch thousands; the
// reviewer's question is "who, and roughly how much", and the record carries
// the full list for anyone who needs it.
const summaryPathsPerOwner = 5

// lostAccessByOwner renders one line per owner rather than one per path: the
// question a reviewer is answering is "which teams stop owning things", and a
// per-path list buries three team names under five hundred rows.
func lostAccessByOwner(lost []LostAccess) []string {
	paths := map[string][]string{}
	var order []string
	for _, l := range lost {
		for _, o := range l.Owners {
			if _, seen := paths[o]; !seen {
				order = append(order, o)
			}
			paths[o] = append(paths[o], l.Path)
		}
	}
	sort.Strings(order)

	out := make([]string, 0, len(order))
	for _, o := range order {
		ps := paths[o]
		shown := ps
		suffix := ""
		if len(ps) > summaryPathsPerOwner {
			shown = ps[:summaryPathsPerOwner]
			suffix = fmt.Sprintf(", and %d more", len(ps)-summaryPathsPerOwner)
		}
		out = append(out, fmt.Sprintf("**%s** stops owning %d path(s): `%s`%s",
			o, len(ps), strings.Join(shown, "`, `"), suffix))
	}
	return out
}
