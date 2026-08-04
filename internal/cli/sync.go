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

// syncCodeownersLocations is S-8's search order, used for the WORKING-TREE
// fallback (D5). gittree owns the same list for the tracked tree; it is repeated
// rather than exported because the two lookups answer different questions —
// "what governs at this ref" versus "what is on disk right now".
var syncCodeownersLocations = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

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

// opLabel is D2: an op with no policy id is referred to by POSITION. `ops[N]` is
// a display label computed here, never a value stored in Op.ID — storing it would
// make an unnamed op indistinguishable from one deliberately named "ops[0]".
func opLabel(op ops.Op, i int) string {
	if op.ID != "" {
		return op.ID
	}
	return fmt.Sprintf("ops[%d]", i)
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
			return fmt.Errorf("%s: invalid scope %q: %v", opLabel(op, i), op.Scope, err)
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
	out := fs.String("out", "", "write the JSON record here (always JSON, whatever --format says)")
	summaryOut := fs.String("summary-out", "", "write a markdown PR body here")
	if err := fs.Parse(args); err != nil {
		// A help request is not a broken policy. Without this arm `sync --help`
		// exits 3, which under the documented fleet script means "halt the run".
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitInvalid
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
	if *create && *branch != "HEAD" {
		return exit3(stderr, fmt.Errorf("--create cannot be combined with --branch %q: there is nothing to create a file \"at\" on a ref you are not standing on, and writing it into the working tree instead would be the wrong file in the wrong place (R-23)", *branch))
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
	if err := emitRecord(rec, run, *format, *out, *summaryOut, stdout); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitRefused
	}
	return code
}

// execute reads the repository and converges it. From here on the only codes are
// 0 and 2 — every remaining failure is a fact about THIS repo, and a fleet script
// records it and steps to the next clone.
func (r *syncRun) execute() (SyncRecord, int) {
	rec := SyncRecord{Repo: r.repoArg}

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

	rel, content, creating, err := r.governing(tree)
	if err != nil {
		rec.Status = statusForReadFailure(err)
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
			return rec, ExitRefused
		}
	}
	// `created` reports what this run did, or under --dry-run what it would have
	// done — a converged repo needs no file written, so nothing is created for it
	// even with --create.
	rec.Created = creating && !converged
	return rec, ExitOK
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
			rel = syncCodeownersLocations[0]
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
	for _, cand := range syncCodeownersLocations {
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
func emitRecord(rec SyncRecord, r *syncRun, format, outPath, summaryPath string, stdout io.Writer) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line := append(b, '\n')
	if outPath != "" {
		if err := os.WriteFile(outPath, line, 0o644); err != nil {
			return fmt.Errorf("write --out %s: %w", outPath, err)
		}
	}
	if summaryPath != "" {
		if err := os.WriteFile(summaryPath, []byte(renderSummary(rec, r)), 0o644); err != nil {
			return fmt.Errorf("write --summary-out %s: %w", summaryPath, err)
		}
	}
	if format == "json" {
		_, err = stdout.Write(line)
		return err
	}
	renderRecordText(stdout, rec)
	return nil
}

// renderRecordText is the human render. It is stdout's content under --format text and
// deliberately not JSON: a consumer that piped text into `jq` should fail loudly
// rather than parse a lookalike.
func renderRecordText(w io.Writer, rec SyncRecord) {
	fmt.Fprintf(w, "%s: %d op(s) applied, %d skipped; %d line change(s), %d path(s) change owners\n",
		rec.Status, rec.OpsApplied, rec.OpsSkipped, len(rec.Changes), rec.PathsChanged)
	for i, o := range rec.Ops {
		label := o.ID
		if label == "" {
			label = fmt.Sprintf("ops[%d]", i)
		}
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
	if rec.Created {
		fmt.Fprintf(&b, "- a new CODEOWNERS file was written (`--create`)\n")
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
			id := o.ID
			if id == "" {
				id = fmt.Sprintf("ops[%d]", i)
			}
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
			id := o.ID
			if id == "" {
				id = fmt.Sprintf("ops[%d]", i)
			}
			structural = append(structural, fmt.Sprintf("- `%s` — `%s`", id, o.Op))
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
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitInvalid
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
