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
	dryRun := fs.Bool("dry-run", false, "change no CODEOWNERS; --out and --summary-out still emit")
	format := fs.String("format", "text", "text|json — governs stdout only")
	// TRUSTED OPERATOR INPUT, deliberately not contained to --repo.
	//
	// What matters is who chooses the path. --file and the discovered CODEOWNERS
	// path come from the REPOSITORY, so a committed symlink lets anyone with push
	// access steer a fleet runner's write — that is what containedWritePath is for.
	// These two are typed on the command line next to the `>` they replace.
	//
	// Containing them would also break their only real uses, all outside the clone
	// on purpose: `--out records/$repo.json` and `--summary-out
	// "$GITHUB_STEP_SUMMARY"`. No O_EXCL for the same reason — a re-run must
	// overwrite last run's records, not fail on every repo.
	out := fs.String("out", "", "write the JSON record here (always JSON, whatever --format says); trusted operator path — overwritten, and not contained to --repo")
	summaryOut := fs.String("summary-out", "", "write a markdown PR body here; trusted operator path — overwritten, and not contained to --repo")
	if err := fs.Parse(args); err != nil {
		return flagParseCode(err)
	}

	if *format != "text" && *format != "json" {
		// Never a silent fallback to text: the fleet script's `>> results.jsonl`
		// would then collect human prose that `jq -s` cannot read, after the
		// whole rollout has already written its CODEOWNERS files.
		return exit3(stderr, fmt.Errorf("unknown --format %q; want text or json", *format))
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
	// Argument-only and repo-independent, hence exit 3 — checked here rather than
	// left to gittree so a fleet run halts at repo 0 instead of recording the same
	// exit-2 refusal 100 times.
	if err := gittree.ValidateRef(*branch); err != nil {
		return exit3(stderr, err)
	}
	pol, opList, err := opSource(opSpecs, policyPaths)
	if err != nil {
		return exit3(stderr, err)
	}
	if err := validateScopes(opList); err != nil {
		return exit3(stderr, err)
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
	}
	if pol != nil {
		run.onEmpty = pol.OnEmpty
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
		rec.Error = buildErr.Error()
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
	rec.Warnings = p.Warnings
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
	// `created` reports what this run did, or under --dry-run what it would have
	// done — a converged repo needs no file written, so nothing is created for it
	// even with --create.
	rec.Created = creating && !converged
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
func (r *syncRun) checkRepoRoot() error {
	root, err := gitLine(r.repoArg, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	same, err := sameDir(r.repoArg, root)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("--repo %s is inside the repository rooted at %s, not that root: git resolves the tracked tree against the root, so the CODEOWNERS this run would write is at a path GitHub never reads, while the file that does govern (%s) stays untouched; re-run with --repo %s",
			r.repoArg, root, filepath.ToSlash(filepath.Join(root, gittree.CodeownersLocations[0])), root)
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
	if r.branch == "HEAD" || r.dryRun {
		return nil
	}
	// --end-of-options on top of cmdSync's guard: rev-parse has no trailing `--`
	// for it to swallow (unlike ls-tree — see gittree.ValidateRef), so it is free.
	head, err := gitLine(r.repoArg, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return err
	}
	want, err := gitLine(r.repoArg, "rev-parse", "--verify", "--end-of-options", r.branch+"^{commit}")
	if err != nil {
		return err
	}
	if head != want {
		return fmt.Errorf("--branch %s is not what this clone has checked out (HEAD is %s): sync proves the change against %s's tree but writes the working tree, so the rule would be justified by one tree and land in another; re-run with --dry-run to preview it, check out %s first, or use `plan` to produce an artifact for that ref (S-7)",
			r.branch, head[:min(len(head), 12)], r.branch, r.branch)
	}
	return nil
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
type noCodeownersError struct{ ref string }

func (e *noCodeownersError) Error() string {
	return "no CODEOWNERS file found in .github/, root, or docs/ at " + e.ref +
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
		return "", nil, false, &noCodeownersError{ref: r.branch}
	default:
		// Creating from nothing: the "before" state is an empty file, which is
		// what INV-2 is proven against.
		return rel, nil, true, nil
	}
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

	if len(rec.Ops) > 0 {
		b.WriteString("\n## Ops\n\n| id | op | status | proven | note |\n|---|---|---|---|---|\n")
		for i, o := range rec.Ops {
			id := policy.OpLabel(o.ID, i)
			note := ""
			if r.policy != nil {
				note = r.policy.Notes[id]
			}
			detail := o.Proven
			if o.Reason != "" {
				detail = o.Reason
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s | %s |\n", id, o.Op, o.Status, detail, note)
		}
	}

	var structural []string
	for i, o := range rec.Ops {
		if o.Proven == "structural" {
			structural = append(structural, fmt.Sprintf("- `%s` — `%s`", policy.OpLabel(o.ID, i), o.Op))
		}
	}
	if len(structural) > 0 {
		b.WriteString("\n## Proven structurally, not against the tree (INV-6)\n\n")
		b.WriteString("Nothing tracked in this repository matches these scopes, so the rule was appended\n" +
			"at EOF where no later rule can override it, and that ordering is the whole proof —\n" +
			"the tool cannot show the rule does what you meant. Read these lines in the diff.\n\n")
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
