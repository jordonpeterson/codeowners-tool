// Package cli wires the commands and owns the exit-code contract (R-17).
// Distinct, documented, scriptable:
//
//	0  success — applied, or audit found nothing
//	1  no-op — nothing to change
//	2  refused — would violate INV-1 or INV-2 (or S-4 size cap)
//	3  invalid input — malformed op, unresolvable scope, conflicting batch
//	4  audit findings present
//	5  inconclusive — API unavailable, token insufficient, rate limited (R-12)
//	6  validation failed post-write; rolled back
//
// `sync` and `check` run under a coarser three-code contract of their own
// (R-19: only 0, 2 and 3), because their question is "did this repo converge?"
// rather than "what exactly happened?". See sync.go.
//
// `audit --lint` narrows 0 and 4 rather than redefining them: it never returns
// 1, because a file that needs no repair is that verb's SUCCESS and exiting
// no-op would make every healthy repo in a fleet read as a failure under
// `set -e`; and its 4 is still "findings present", counting a fix computed but
// not written (--dry-run) as one. See lint.go.
package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jordonpeterson/codeowners-tool/internal/apply"
	"github.com/jordonpeterson/codeowners-tool/internal/audit"
	"github.com/jordonpeterson/codeowners-tool/internal/ghapi"
	"github.com/jordonpeterson/codeowners-tool/internal/gittree"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
	"github.com/jordonpeterson/codeowners-tool/internal/verify"
)

// Exit codes (R-17).
const (
	ExitOK           = 0
	ExitNoOp         = 1
	ExitRefused      = 2
	ExitInvalid      = 3
	ExitFindings     = 4
	ExitInconclusive = 5
	ExitValidation   = 6
)

// Version is the build this binary is, stamped at link time by the release
// workflow with
//
//	-ldflags "-X github.com/jordonpeterson/codeowners-tool/internal/cli.Version=v0.0.7"
//
// and left as "dev" for anything built locally, which is exactly the honest
// answer for a binary nobody released. Without it an operator holding a binary
// cannot tell whether it is the one with a given fix, so both rollback and CVE
// response are guesswork across a fleet.
var Version = "dev"

// flagParseCode maps a flag-parsing failure to an exit code.
//
// `--help` is a request, not a broken invocation. Returning ExitInvalid for it
// made every `<verb> -h` exit 3, which under the fleet contract in sync.go reads
// as "the policy is broken, halt the run" — so all verbs answer it here rather
// than each deciding for itself.
func flagParseCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	return ExitInvalid
}

// rejectLeftoverArgs refuses positional arguments, which no verb here takes.
//
// Go's flag package stops parsing at the first non-flag token, so `audit
// ../other-repo --checks a999` silently dropped BOTH the stray path and every
// flag after it: the run audited the CWD with all defaults and exited 0, and
// the invalid --checks the parser would have rejected loudly was never seen.
// A tool this strict about flag values must not swallow whole arguments.
// Argument-only and repo-independent, so it is exit 3 in every verb.
func rejectLeftoverArgs(fs *flag.FlagSet) error {
	if fs.NArg() == 0 {
		return nil
	}
	return fmt.Errorf("unexpected argument %q: %s takes every input as a flag (--repo DIR, not a positional path), and the parser reads nothing after a positional argument — flags included",
		fs.Arg(0), fs.Name())
}

// rejectEmptyRepo refuses `--repo ""`, the unset-variable bug every fleet
// script eventually has:
//
//	REPO=$(lookup_repo "$name")     # returned nothing
//	codeowners-tool sync --repo "$REPO" --op '...'
//
// A typo'd path fails correctly. The empty string was the one wrong value that
// got a DEFAULT instead of an error — `.` — so the run targeted whatever
// directory the script happened to be standing in and wrote to it at exit 0,
// while the record carried "repo":"" and a fleet's results.jsonl gained a
// success row that did not say which repository had changed.
//
// It asks whether the flag was PASSED rather than reading its value, because
// `apply --repo` defaults to "" to mean "use the plan's own repo" — there the
// value alone cannot tell an operator's empty string from an absent flag.
//
// Exit 3: an empty --repo is broken invocation, identical on every repository,
// so a fleet should stop at repo 0 rather than report it a hundred times.
func rejectEmptyRepo(fs *flag.FlagSet, value string) error {
	if !isFlagSet(fs, "repo") || value != "" {
		return nil
	}
	return fmt.Errorf(`--repo was given an empty value; pass a path, or omit the flag to use the working directory. An empty string is what a shell produces when the variable behind it is unset, so it is refused rather than defaulting`)
}

// isFlagSet reports whether the operator actually PASSED a flag, as opposed to
// inheriting its default. --cache-ttl has a non-zero default, so "did you ask
// for this?" cannot be read off its value.
func isFlagSet(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

// SyncRecord is one `sync` run, rendered as one JSON object (R-24). One line
// per repo is what lets `jq -s` aggregate a fleet without parsing stderr.
type SyncRecord struct {
	Repo string `json:"repo"`
	// Policy is the --policy path as the operator spelled it, absent on an --op
	// run — the same key, and the same omitempty meaning, as `check --format
	// json`. R-20 rests on the policy file being the complete statement of what
	// ran; a directory of `--out records/$repo.json` that cannot say which
	// artifact produced it cannot check that claim, and two waves in one week
	// leave an aggregation nobody can attribute. Absent means "no policy file
	// was involved", which an empty string would not: `group_by(.policy)` would
	// bucket every ad-hoc --op run together with a policy that has no name.
	Policy string `json:"policy,omitempty"`
	// CodeownersPath is the repo-relative file this run CHANGED, or under
	// --dry-run would have. A rollout has to stage and commit that file, and
	// which of the three legal locations it is differs per repo (S-8), so a
	// loop without this field can only `git add -A` and hope nothing else moved
	// in the clone.
	//
	// It is emitted only when Status is StatusApplied. Absent therefore means
	// "this run wrote nothing", NOT "no file was chosen": an `unchanged` repo
	// has a perfectly good CODEOWNERS and simply nothing to stage. A refusal
	// names its file in Error instead. One caveat the fleet script has to
	// respect: a --dry-run record reports `applied` for a run that would have
	// written, so DryRun must be checked before anything is staged.
	CodeownersPath string          `json:"codeowners_path,omitempty"`
	Status         string          `json:"status"` // applied|unchanged|skipped|refused|error
	Ops            []plan.OpResult `json:"ops,omitempty"`
	OpsApplied     int             `json:"ops_applied"`
	OpsSkipped     int             `json:"ops_skipped"`
	PathsChanged   int             `json:"paths_changed"`
	Created        bool            `json:"created"`
	// DryRun marks a record produced under --dry-run. Without it a preview row
	// and a row from a real rollout are byte-identical, so an operator holding
	// results.jsonl cannot tell whether the fleet was changed or only modelled —
	// and neither can the script that reads it. omitempty keeps the common case
	// (a real run) at the same six unconditional keys as before.
	DryRun   bool          `json:"dry_run,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	Changes  []plan.Change `json:"changes,omitempty"`
	Error    string        `json:"error,omitempty"`
}

// Sync statuses (R-24). "skipped" is distinct from "unchanged" so a policy
// that matches nothing anywhere cannot read as "already correct" across a
// whole fleet.
const (
	StatusApplied   = "applied"
	StatusUnchanged = "unchanged"
	StatusSkipped   = "skipped"
	StatusRefused   = "refused"
	StatusError     = "error"
)

// planFile is Plan plus the apply-time context the CLI adds.
type planFile struct {
	plan.Plan
	Repo           string `json:"repo"`
	Ref            string `json:"ref"`
	CodeownersPath string `json:"codeowners_path"`
}

// Run executes argv (without the program name) and returns the exit code.
func Run(argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		usage(stderr)
		return ExitInvalid
	}
	switch argv[0] {
	case "sync":
		return cmdSync(argv[1:], stdout, stderr)
	case "check":
		return cmdCheck(argv[1:], stdout, stderr)
	case "plan":
		return cmdPlan(argv[1:], stdout, stderr)
	case "apply":
		return cmdApply(argv[1:], stdout, stderr)
	case "audit":
		return cmdAudit(argv[1:], stdout, stderr)
	case "lint":
		return cmdLint(argv[1:], stdout, stderr)
	case "verify":
		return cmdVerify(argv[1:], stdout, stderr)
	case "snapshot":
		return cmdSnapshot(argv[1:], stdout, stderr)
	case "version", "--version":
		// Exit 0: `version` is a question with an answer, not a no-op. Under the
		// fleet contract a nonzero code here would abort a `set -e` script on the
		// line that was only recording which build it was about to run.
		fmt.Fprintln(stdout, Version)
		return ExitOK
	case "-h", "--help", "help":
		usage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", argv[0])
		usage(stderr)
		return ExitInvalid
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `codeowners-tool — safe, intent-level, verifiable CODEOWNERS changes

  sync     (--op 'OP' ... | --policy FILE) [--on-empty error|inherit|unowned]
           [--repo DIR] [--branch REF] [--file PATH] [--create] [--dry-run]
           [--max-paths-changed N] [--format text|json] [--out FILE] [--summary-out FILE]
  check    (--op 'OP' ... | --policy FILE) [--format text|json]
  plan     --op 'add_owner(/services/api, @org/team-1)' [--op ...] [--on-empty error|inherit|unowned]
           [--repo DIR] [--branch REF] [--file PATH] [--out plan.json]
  apply    --plan plan.json [--repo DIR]
  audit    [--checks a1,a3,a6] [--fail-on any|warning|error|never] [--format json|text]
           [--github-repo owner/name] [--token T | $GITHUB_TOKEN] [--api-url URL]
           [--cache-dir D] [--cache-ttl DUR] [--repo DIR] [--branch REF] [--file PATH]
  lint     --github-repo owner/name [--token T | $GITHUB_TOKEN] [--api-url URL]
           [--remove-stale-paths] [--on-empty error|inherit|unowned] [--dry-run]
           [--policy FILE] [--repo DIR] [--branch REF] [--file PATH] [--format text|json]
  snapshot [--repo DIR] [--branch REF] [--out snap.json]
  verify   --before before.json --after after.json [--scope PATTERN ...]
  version  print the build this binary was stamped with

audit REPORTS — twelve checks, and it never writes. lint REPAIRS three of the
things audit reports: @handles that whitespace has split, owners that no longer
exist, and (only with --remove-stale-paths) rules matching nothing. It rewrites
the WORKING-TREE file, so it needs a token and --github-repo; one lookup it
cannot answer and nothing is written at all. Start with --dry-run.

audit --lint is the older spelling of lint and still works.

Exit 4 from lint means the file still needs a person — fixes pending under
--dry-run, or a line lint would not guess at. A successful write can also exit
4 for that reason, so gate CI on lint --dry-run, not on the writing run.

Exit codes: 0 ok · 1 no-op · 2 refused (invariant/size) · 3 invalid input
            4 audit findings · 5 inconclusive (fail-closed) · 6 rolled back
sync/check use a coarser contract and return only:
            0 converged · 2 this repo needs a human · 3 the policy is broken
`)
}

func errExit(err error, stderr io.Writer) int {
	fmt.Fprintln(stderr, "error:", err)
	var noop *plan.NoOpError
	var ref *plan.RefusalError
	var inv *plan.InvalidError
	var val *apply.ValidationError
	switch {
	case errors.As(err, &noop):
		return ExitNoOp
	case errors.As(err, &ref):
		return ExitRefused
	case errors.As(err, &inv):
		return ExitInvalid
	case errors.As(err, &val):
		return ExitValidation
	default:
		return ExitInvalid
	}
}

// locate finds the governing CODEOWNERS file (S-8 precedence) and the
// tracked tree at ref. filePath overrides discovery.
func locate(repoDir, ref, filePath string) (tree []string, path string, all []string, err error) {
	tree, err = gittree.ListTracked(repoDir, ref)
	if err != nil {
		return nil, "", nil, &plan.InvalidError{Msg: err.Error()}
	}
	all = gittree.FindCodeownersPaths(tree)
	if filePath != "" {
		// Same containment guard as `sync` (see containedRelPath): --file is
		// documented as repo-relative and is joined onto --repo everywhere, so a
		// path that is absolute or climbs out with .. names a file this
		// repository does not own. These verbs only ever READ through this path,
		// so today the escape merely fails late and obscurely; the guard makes it
		// fail at the argument, in the same exit-3 class it already lands in.
		if err := containedRelPath(filePath); err != nil {
			return nil, "", nil, &plan.InvalidError{Msg: err.Error()}
		}
		return tree, filePath, all, nil
	}
	if len(all) == 0 {
		return nil, "", nil, &plan.InvalidError{Msg: "no CODEOWNERS file found in .github/, root, or docs/ at " + ref + " (use --file to specify one)"}
	}
	return tree, all[0], all, nil
}

// containedRelPath rejects a --file that names anything outside --repo.
//
// Every caller joins --file onto --repo, so the flag is only meaningful as a
// repo-relative path. Two spellings break that, and both used to be accepted
// silently:
//
//   - `--file ../ESCAPED/CODEOWNERS` addresses a sibling of the clone. Under
//     `sync --create` that is not a read that fails but a WRITE: os.MkdirAll
//     builds the tree and a CODEOWNERS lands outside the repository, reported as
//     applied at exit 0. A fleet loop pointed at 100 clones writes 100 files
//     into whatever happens to sit next to them.
//   - `--file /tmp/x/ABS.txt` is not rejected but REINTERPRETED: filepath.Join
//     makes it repo/tmp/x/ABS.txt, so the operator who typed an absolute path
//     gets a lookalike tree inside the clone and a success record.
//
// Both are decidable from the argument alone — no repository is opened to know
// them — so they belong to the exit-3 class in sync.go's terms.
func containedRelPath(p string) error {
	if p == "" {
		return nil
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || filepath.VolumeName(p) != "" {
		return fmt.Errorf("--file %q must be repo-relative: an absolute path is not silently reinterpreted, because joining it onto --repo would build a lookalike tree inside the repository and report success", p)
	}
	clean := filepath.Clean(filepath.FromSlash(p))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("--file %q escapes the repository: it resolves to %q, outside --repo — with --create the tool would create the directories and write a CODEOWNERS there", p, filepath.ToSlash(clean))
	}
	return nil
}

// containedWritePath refuses to write a CODEOWNERS whose real location is
// outside --repo.
//
// containedRelPath closes the spellings that are visible in the ARGUMENTS.
// This one closes the spelling that needs no flag at all: a committed symlink.
// apply.Apply resolves symlinks and replaces the TARGET's bytes — deliberately,
// so a repo that keeps its CODEOWNERS behind a link keeps the link — but the
// target of a link committed to the repository is a path chosen by whoever can
// push to it. `.github/CODEOWNERS -> ../../VICTIM.txt` makes a fleet runner edit
// a file in the clone NEXT DOOR and report "applied", exit 0.
//
// It is wrong with no attacker at all. GitHub does not follow a symlinked
// CODEOWNERS, so a run that writes through one has edited a file that governs
// nothing while reporting success — the "applied, dead on arrival" outcome the
// fleet verbs exist to prevent.
//
// The check lives here rather than in apply.Apply because apply.Apply has no
// notion of a repository: it is handed one path and one plan, and follows
// whatever link it is handed. The CLI is the layer that knows --repo, so it is
// the layer that can tell "inside" from "outside" — and the layer that refuses
// a link even when it stays inside (see refuseSymlinkedTarget).
//
// Both sides are resolved before comparison, for the same reason checkRepoRoot
// resolves both: on macOS t.TempDir() hands out /var/folders/... while the real
// path is /private/var/folders/..., and comparing raw strings would refuse every
// repo on a developer laptop.
func containedWritePath(repoDir, target string) error {
	root, err := resolveDir(repoDir)
	if err != nil {
		return err
	}
	dest, err := resolveLinkChain(target)
	if err != nil {
		return err
	}
	if dest == root || strings.HasPrefix(dest, root+string(filepath.Separator)) {
		return nil
	}
	return &plan.RefusalError{Msg: fmt.Sprintf(
		"refusing to write %s: it resolves to %s, outside the repository at %s — a symlinked CODEOWNERS is a path chosen by whoever can push to this repo, and GitHub does not follow one anyway, so the write would land outside the clone while governing nothing; replace the link with a real file",
		filepath.ToSlash(target), filepath.ToSlash(dest), filepath.ToSlash(root))}
}

// resolveLinkChain answers "which file would a write to p actually hit?".
//
// filepath.EvalSymlinks is not enough on its own: it fails outright when any
// component is missing, and a DANGLING link is precisely the `--create` case —
// the link resolves to nothing, O_EXCL therefore has nothing to collide with,
// and the new file lands wherever the link pointed. So the final component's
// link chain is walked by hand, and the directory part is resolved as far as it
// exists, with the not-yet-created tail re-joined on.
func resolveLinkChain(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	// The walk is bounded — `a -> b -> a` would otherwise spin here forever — and
	// running out of hops is a REFUSAL, not a shrug. Stopping quietly would leave
	// abs on the last link inspected, which reads as contained while EvalSymlinks
	// (which apply.Apply uses, and which follows far more hops than this) would
	// go on to the real target outside the repository. A chain nobody can follow
	// to the end is a chain whose destination is unknown, and an unknown
	// destination is not one to write to.
	const maxHops = 64
	for hop := 0; ; hop++ {
		fi, err := os.Lstat(abs)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			// A real file, or nothing there yet: the `--create` case, where the
			// directory part below is what decides containment.
			break
		}
		if hop == maxHops {
			return "", &plan.RefusalError{Msg: fmt.Sprintf(
				"refusing to write %s: its symlink chain is still unresolved after %d hops, so where the write would actually land cannot be established; replace the link with a real file",
				filepath.ToSlash(p), maxHops)}
		}
		dst, err := os.Readlink(abs)
		if err != nil {
			break
		}
		if filepath.IsAbs(dst) {
			abs = filepath.Clean(dst)
		} else {
			abs = filepath.Clean(filepath.Join(filepath.Dir(abs), dst))
		}
	}
	dir, base := filepath.Split(abs)
	return filepath.Join(resolveExistingPrefix(filepath.Clean(dir)), base), nil
}

// resolveExistingPrefix resolves the longest existing ancestor of dir and
// re-attaches the components that do not exist yet. `sync --create` may name a
// directory nobody has made, and refusing to decide containment because of that
// would leave the create path unguarded.
func resolveExistingPrefix(dir string) string {
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{filepath.Clean(resolved)}, tail...)...)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Join(append([]string{dir}, tail...)...)
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
		dir = parent
	}
}

// refuseSymlinkedTarget refuses to write a CODEOWNERS that is a symlink, or
// that sits BEHIND one — any component of the write path below the repository
// root, wherever the link points.
//
// containedWritePath keeps the write INSIDE the clone; this closes the cases
// it lets through. The final component: GitHub does not follow a symlinked
// CODEOWNERS, so writing through one edits a file that governs nothing while
// reporting success — the "applied, dead on arrival" outcome the write verbs
// exist to prevent, only with the dead file hidden behind a live-looking path.
// A PARENT component is the same outcome one level up: git commits a symlinked
// directory as a link BLOB, not a tree, so with `.github -> real-gh` there is
// no `.github/CODEOWNERS` in the tree GitHub reads, and Lstat'ing only the
// final component (a real file, reached through the link) let sync, apply and
// lint all write through it at exit 0. Refusal over a warning, matching the
// tool's fail-closed posture — a warning under a fleet's `--format json >>
// results` is a count nobody reads.
//
// Only the components of the write path below the repository root are
// Lstat'ed, so a symlink anywhere else in the repository stays irrelevant, an
// absent component (the --create case) has no link to refuse, and a --repo
// reached through links above the root (macOS /var -> /private/var) stays
// legitimate. The walk is bounded by the path's own depth.
func refuseSymlinkedTarget(target string) error {
	// Abs can only fail when the CWD is gone; degrade to the raw spelling —
	// the walk then covers the final component, the pre-walk behavior.
	abs, err := filepath.Abs(target)
	if err != nil {
		abs = filepath.Clean(target)
	}
	for _, comp := range writePathComponents(abs) {
		fi, err := os.Lstat(comp)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		dest, err := os.Readlink(comp)
		if err != nil {
			dest = "(unreadable link target)"
		}
		if comp != abs {
			return &plan.RefusalError{Msg: fmt.Sprintf(
				"refusing to write %s: %s is a symlink to %s, and git records a symlinked directory as a link, not a tree, so no CODEOWNERS exists behind it at the path GitHub reads — the write would edit a file that governs nothing while reporting applied; replace the link with a real directory — nothing was written",
				filepath.ToSlash(target), filepath.ToSlash(comp), filepath.ToSlash(dest))}
		}
		return &plan.RefusalError{Msg: fmt.Sprintf(
			"refusing to write %s: it is a symlink to %s, and GitHub does not follow a symlinked CODEOWNERS, so the write would edit a file that governs nothing while reporting applied; replace the link with a real file — nothing was written",
			filepath.ToSlash(target), filepath.ToSlash(dest))}
	}
	return nil
}

// writePathComponents lists abs and every directory between it and the
// enclosing repository's root, root EXCLUSIVE — those are exactly the
// components a committed symlink can occupy, and the ones GitHub resolves
// through the tracked tree.
//
// The root is found lexically — the nearest ancestor holding a .git entry —
// rather than by asking git, because rev-parse resolves symlinks in its answer
// while this walk must stay on the spelling the caller joined --repo and the
// repo-relative path into; a lexical boundary is what keeps everything at or
// above the root uninspected. Every caller has already passed checkRepoRoot,
// so that ancestor IS the --repo the write is contained to. Found no root at
// all, the walk degrades to the final component alone — the pre-walk behavior,
// and the only thing decidable without a boundary.
func writePathComponents(abs string) []string {
	var comps []string
	for dir := abs; ; {
		parent := filepath.Dir(dir)
		if parent == dir {
			return []string{abs}
		}
		comps = append([]string{dir}, comps...)
		if _, err := os.Stat(filepath.Join(parent, ".git")); err == nil {
			return comps
		}
		dir = parent
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opSpecs multiFlag
	fs.Var(&opSpecs, "op", "operation (repeatable)")
	repo := fs.String("repo", ".", "path to local git repository")
	branch := fs.String("branch", "HEAD", "ref whose tracked tree governs resolution (S-7)")
	filePath := fs.String("file", "", "CODEOWNERS path override (repo-relative)")
	onEmpty := fs.String("on-empty", "", "policy when remove_owner empties a set: error|inherit|unowned (R-6, no default)")
	// Same class as sync's --out; see the note there.
	out := fs.String("out", "", "write plan JSON here (default stdout); trusted operator path — overwritten, and not contained to --repo")
	maxSize := fs.Int("max-size", 3_000_000, "hard size cap in bytes (S-4)")
	warnSize := fs.Int("warn-size", 2_500_000, "warn threshold in bytes (R-9)")
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
	if len(opSpecs) == 0 {
		fmt.Fprintln(stderr, "error: at least one --op is required")
		return ExitInvalid
	}
	parsed, err := ops.ParseAll(opSpecs)
	if err != nil {
		return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
	}
	// The same repo-root guard `sync` enforces (checkRepoRoot), for the verb
	// that produces the ARTIFACT: pointed below the root, discovery resolves
	// against the ROOT's tree while codeowners_path joins onto the
	// subdirectory, so the plan describes an edit at a path GitHub never reads
	// — and a human approves it before any downstream refusal fires. Refused
	// before --out exists: a refused run writes nothing.
	if err := checkRepoRoot(*repo); err != nil {
		return errExit(&plan.RefusalError{Msg: err.Error()}, stderr)
	}
	tree, coPath, _, err := locate(*repo, *branch, *filePath)
	if err != nil {
		return errExit(err, stderr)
	}
	// Not the write (sync and apply refuse on their own) but the ARTIFACT: a plan
	// pointing outside the clone describes an edit to a file GitHub never reads,
	// and a human approves it before any downstream refusal fires.
	if err := containedWritePath(*repo, filepath.Join(*repo, filepath.FromSlash(coPath))); err != nil {
		return errExit(err, stderr)
	}
	// File bytes come from the working tree — that is what apply mutates.
	content, err := os.ReadFile(filepath.Join(*repo, filepath.FromSlash(coPath)))
	if err != nil {
		return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
	}
	p, err := plan.Build(content, tree, parsed, plan.Options{OnEmpty: *onEmpty, MaxSize: *maxSize, WarnSize: *warnSize})
	if err != nil {
		return errExit(err, stderr)
	}
	pf := planFile{Plan: *p, Repo: *repo, Ref: *branch, CodeownersPath: coPath}
	b, _ := json.MarshalIndent(pf, "", "  ")
	if *out != "" {
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return errExit(err, stderr)
		}
		fmt.Fprintf(stdout, "plan written to %s\n", *out)
	} else {
		fmt.Fprintln(stdout, string(b))
	}
	for _, w := range p.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	fmt.Fprintf(stderr, "%d line change(s), %d path(s) change owners, %d → %d bytes\n",
		len(p.Changes), len(p.Rows), p.SizeBefore, p.SizeAfter)
	return ExitOK
}

func cmdApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	planPath := fs.String("plan", "", "plan JSON produced by `plan`")
	repo := fs.String("repo", "", "path to local git repository (default: plan's repo)")
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
	if *planPath == "" {
		fmt.Fprintln(stderr, "error: --plan is required")
		return ExitInvalid
	}
	b, err := os.ReadFile(*planPath)
	if err != nil {
		return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
	}
	var pf planFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return errExit(&plan.InvalidError{Msg: "parse plan: " + err.Error()}, stderr)
	}
	repoDir := pf.Repo
	if *repo != "" {
		repoDir = *repo
	}
	// The same repo-root guard `sync` enforces (checkRepoRoot), re-checked at
	// apply time for the same reason containment is: --repo can point this
	// verb at a different clone than the one planned against, and pointed
	// below the root the join below addresses a file at a path GitHub never
	// reads while the file that governs stays untouched.
	if err := checkRepoRoot(repoDir); err != nil {
		return errExit(&plan.RefusalError{Msg: err.Error()}, stderr)
	}
	target := filepath.Join(repoDir, filepath.FromSlash(pf.CodeownersPath))
	// The same containment `sync` enforces, enforced again here. A plan is an
	// artifact: it is reviewed in one place and applied somewhere else entirely,
	// possibly against a different clone via --repo, so the decision cannot be
	// left behind in the verb that produced it.
	if err := containedWritePath(repoDir, target); err != nil {
		return errExit(err, stderr)
	}
	// And the symlink refusal those two do not cover: a link that stays inside
	// the clone still lands the write on a file GitHub never reads.
	if err := refuseSymlinkedTarget(target); err != nil {
		return errExit(err, stderr)
	}
	if err := apply.Apply(&pf.Plan, target); err != nil {
		return errExit(err, stderr)
	}
	fmt.Fprintf(stdout, "applied: %s (%d → %d bytes)\n", target, pf.SizeBefore, pf.SizeAfter)
	return ExitOK
}

func cmdSnapshot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "path to local git repository")
	branch := fs.String("branch", "HEAD", "ref to snapshot")
	filePath := fs.String("file", "", "CODEOWNERS path override")
	out := fs.String("out", "", "write snapshot JSON here (default stdout); trusted operator path — overwritten, and not contained to --repo")
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
	tree, coPath, _, err := locate(*repo, *branch, *filePath)
	if err != nil {
		return errExit(err, stderr)
	}
	content, err := gittree.ReadFileAtRef(*repo, *branch, coPath)
	if err != nil {
		return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
	}
	res := plan.ResolveContent(string(content), tree)
	snap := verify.Snapshot{Repo: *repo, Ref: *branch, Path: coPath, Ownership: map[string][]string{}}
	h := sha256.Sum256(content)
	snap.SHA256 = hex.EncodeToString(h[:])
	for p, r := range res {
		snap.Ownership[p] = r.Owners // nil (null) = unowned
	}
	b, _ := json.MarshalIndent(snap, "", "  ")
	if *out != "" {
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return errExit(err, stderr)
		}
		fmt.Fprintf(stdout, "snapshot written to %s (%d paths)\n", *out, len(snap.Ownership))
	} else {
		fmt.Fprintln(stdout, string(b))
	}
	return ExitOK
}

func cmdVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	beforePath := fs.String("before", "", "before snapshot")
	afterPath := fs.String("after", "", "after snapshot")
	var scopes multiFlag
	fs.Var(&scopes, "scope", "pattern where change is allowed (repeatable; none = assert no change)")
	if err := fs.Parse(args); err != nil {
		return flagParseCode(err)
	}
	if err := rejectLeftoverArgs(fs); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitInvalid
	}
	if *beforePath == "" || *afterPath == "" {
		fmt.Fprintln(stderr, "error: --before and --after are required")
		return ExitInvalid
	}
	before, err := verify.Load(*beforePath)
	if err != nil {
		return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
	}
	after, err := verify.Load(*afterPath)
	if err != nil {
		return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
	}
	res, err := verify.Compare(before, after, scopes)
	if err != nil {
		return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
	}
	for _, c := range res.Changed {
		fmt.Fprintf(stdout, "changed: %s  %s → %s\n", c.Path, fmtOwners(c.Before), fmtOwners(c.After))
	}
	// Tree deltas are reported, never fatal (R-18). They are the ordinary
	// difference between two refs, so they print after the ownership changes
	// rather than competing with them for a reader's attention.
	for _, c := range res.Added {
		fmt.Fprintf(stdout, "added:   %s  %s\n", c.Path, fmtOwners(c.After))
	}
	for _, c := range res.Removed {
		fmt.Fprintf(stdout, "removed: %s  %s\n", c.Path, fmtOwners(c.Before))
	}
	if !res.OK() {
		// The loudest string in the tool is reserved for the case it is about:
		// a run that DECLARED where change was allowed and found change
		// elsewhere. With no --scope, verify is a query — "did anything
		// change?" — and a negative answer is information, not an alarm. Three
		// of five user tests flagged the old wording independently, one of them
		// on the documented post-merge recipe ("a scary word for a routine
		// query"). The exit code is unchanged in both cases; only the sentence
		// moves.
		if len(scopes) == 0 {
			fmt.Fprintf(stderr, "%d path(s) changed, and no --scope was declared — with no scope, verify asserts that NOTHING changed (--scope is repeatable; pass one per intended scope)\n", len(res.Violations))
		} else {
			fmt.Fprintf(stderr, "INVARIANT VIOLATED: %d path(s) changed that no --scope declared (--scope is repeatable; pass one per intended scope)\n", len(res.Violations))
		}
		for _, v := range res.Violations {
			fmt.Fprintf(stderr, "  %s  %s → %s\n", v.Path, fmtOwners(v.Before), fmtOwners(v.After))
		}
		return ExitRefused
	}
	fmt.Fprintf(stdout, "ok: %d change(s), all within scope%s\n", len(res.Changed), fmtTreeDeltas(res))
	return ExitOK
}

func cmdAudit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "path to local git repository")
	branch := fs.String("branch", "HEAD", "ref to audit")
	filePath := fs.String("file", "", "CODEOWNERS path override")
	checksFlag := fs.String("checks", "", "comma-separated subset, e.g. a1,a3,a6 (default: all)")
	failOn := fs.String("fail-on", auditFailOnAny, "lowest severity that exits 4: any|warning|error|never (report is unaffected)")
	format := fs.String("format", "text", "text|json")
	githubRepo := fs.String("github-repo", "", "owner/name on GitHub, for A-1..A-3")
	// The default is "" and the environment is read AFTER Parse, never before.
	// Making os.Getenv("GITHUB_TOKEN") the flag's DEFAULT handed the live
	// credential to the flag package, which prints non-empty string defaults in
	// its usage text — so `audit --help` and, far worse, any mistyped flag wrote
	// the PAT to stderr, where CI captures it into the build log (CWE-532).
	// GitHub Actions masks secrets it registered; Jenkins, GitLab and a terminal
	// on a screen share mask nothing.
	token := fs.String("token", "", "GitHub token (default $GITHUB_TOKEN)")
	apiURL := fs.String("api-url", "", "API base URL (GHES), default https://api.github.com")
	cacheDir := fs.String("cache-dir", "", "disk cache directory (R-15); empty = memory only")
	cacheTTL := fs.Duration("cache-ttl", 24*time.Hour, "disk cache TTL")
	lintMode := fs.Bool("lint", false, "repair the whole file instead of only reporting: rejoin @handles split by whitespace, then remove owners that do not exist (needs --token and --github-repo)")
	removeStale := fs.Bool("remove-stale-paths", false, "with --lint, also delete rules whose pattern matches zero tracked files (R-11 keeps this off by default)")
	onEmpty := fs.String("on-empty", "", "with --lint, R-6 policy when removing a dead owner empties a set: error|inherit|unowned (no default)")
	dryRun := fs.Bool("dry-run", false, "with --lint, compute and report the fixes but write nothing (exit 4 if any are pending)")
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
	// The same validation sync, check and lint apply, and for the same reason:
	// never a silent fallback to text. `--format jsn` fell back to prose here,
	// and the CI step piping stdout to jq failed on precisely the healthy runs.
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "error: unknown --format %q; want text or json\n", *format)
		return ExitInvalid
	}
	// The three lint-only flags are rejected rather than ignored when --lint is
	// absent. `audit --remove-stale-paths` silently reporting instead of
	// deleting is the shape of mistake that only surfaces months later, when
	// somebody notices the rules they believed were cleaned up are all still
	// there.
	if !*lintMode {
		var only []string
		if *removeStale {
			only = append(only, "--remove-stale-paths")
		}
		if *onEmpty != "" {
			only = append(only, "--on-empty")
		}
		if *dryRun {
			only = append(only, "--dry-run")
		}
		if len(only) > 0 {
			verb := "applies"
			if len(only) > 1 {
				verb = "apply"
			}
			fmt.Fprintf(stderr, "error: %s %s only to the `lint` verb (or `audit --lint`); plain audit reports and never writes, so there is nothing for them to govern\n", strings.Join(only, " and "), verb)
			return ExitInvalid
		}
	}
	// Symmetrically, --fail-on governs the REPORT's exit code and means nothing
	// to a repair pass, which reports what it fixed. Rejected rather than
	// ignored, for the reason above.
	failOnSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "fail-on" {
			failOnSet = true
		}
	})
	if *lintMode && failOnSet {
		fmt.Fprintln(stderr, "error: --fail-on applies to the audit report, not to a repair pass; drop it, or drop --lint")
		return ExitInvalid
	}
	gate, ok := auditFailOnLevel(*failOn)
	if !ok {
		// Never a fallback to the permissive end: `--fail-on eror` quietly
		// meaning "never" is a CI gate that reports green while checking
		// nothing, on every repo, until someone reads the workflow file.
		fmt.Fprintf(stderr, "error: unknown --fail-on %q; want any, warning, error or never\n", *failOn)
		return ExitInvalid
	}
	// $GITHUB_TOKEN remains the documented fallback; it is just resolved past
	// the point where anything can render it. An explicit --token still wins.
	authToken := *token
	if authToken == "" {
		authToken = os.Getenv("GITHUB_TOKEN")
	}

	if *lintMode {
		return runLint(lintRun{
			repo: *repo, branch: *branch, filePath: *filePath,
			githubRepo: *githubRepo, token: authToken,
			apiURL: *apiURL, cacheDir: *cacheDir, cacheTTL: *cacheTTL,
			cacheTTLSet: isFlagSet(fs, "cache-ttl"),
			format:      *format, checks: *checksFlag,
			removeStale: *removeStale, onEmpty: *onEmpty, dryRun: *dryRun,
		}, stdout, stderr)
	}

	tree, coPath, all, err := locate(*repo, *branch, *filePath)
	if err != nil {
		return errExit(err, stderr)
	}
	content, err := gittree.ReadFileAtRef(*repo, *branch, coPath)
	if err != nil {
		return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
	}

	in := audit.Input{Content: content, Tree: tree, CodeownersPath: coPath, AllPresent: all}
	if *checksFlag != "" {
		in.Checks = map[string]bool{}
		for _, c := range strings.Split(*checksFlag, ",") {
			// Accept a4, A4, a-4, A-4 — including the canonical dashed form
			// the tool itself prints. An unrecognized name is a hard error:
			// silently matching nothing would make audits pass vacuously
			// (found in review).
			norm := strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(c)), "-", "")
			n := 0
			if len(norm) >= 2 && norm[0] == 'A' {
				n, _ = strconv.Atoi(norm[1:])
			}
			if n < 1 || n > 12 {
				fmt.Fprintf(stderr, "error: unknown check %q (want a1..a12)\n", c)
				return ExitInvalid
			}
			in.Checks[fmt.Sprintf("A-%d", n)] = true
		}
	}
	apiChecksWanted := in.Checks == nil || in.Checks["A-1"] || in.Checks["A-2"] || in.Checks["A-3"]
	if apiChecksWanted && authToken != "" && *githubRepo != "" {
		parts := strings.SplitN(*githubRepo, "/", 2)
		if len(parts) != 2 {
			fmt.Fprintln(stderr, "error: --github-repo must be owner/name")
			return ExitInvalid
		}
		var cache ghapi.Cache = ghapi.NewMemCache()
		if *cacheDir != "" {
			cache = ghapi.NewDiskCache(*cacheDir, *cacheTTL)
		}
		in.Client = ghapi.New(*apiURL, authToken, cache)
		in.RepoOwner, in.RepoName = parts[0], parts[1]
	} else if apiChecksWanted && in.Checks != nil {
		// API checks explicitly requested but not runnable: that is an
		// inconclusive run (R-12), not a silent skip.
		fmt.Fprintln(stderr, "error: A-1/A-2/A-3 requested but --token and --github-repo are not both set")
		return ExitInconclusive
	} else if in.Client == nil && apiChecksWanted {
		fmt.Fprintln(stderr, "note: no token/--github-repo — running offline checks only (A-4..A-12)")
	}

	rep := audit.Run(in)

	if *format == "json" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(b))
	} else {
		for _, f := range rep.Findings {
			line := ""
			if f.Line > 0 {
				line = fmt.Sprintf(" (line %d)", f.Line)
			}
			fmt.Fprintf(stdout, "[%s/%s]%s %s\n", f.Check, f.Severity, line, f.Message)
			for _, op := range f.FixOps {
				fmt.Fprintf(stdout, "    fix: %s\n", op)
			}
			for _, r := range f.Reassignment {
				fmt.Fprintf(stdout, "    reassigns: %s  %s → %s\n", r.Path, fmtOwners(r.Before), fmtOwners(r.After))
			}
		}
		for _, reason := range rep.Inconclusive {
			fmt.Fprintf(stdout, "[inconclusive] %s\n", reason)
		}
	}

	if len(rep.Inconclusive) > 0 {
		return ExitInconclusive
	}
	if len(rep.Findings) > 0 {
		if auditGateTripped(rep.Findings, gate) {
			return ExitFindings
		}
		// Exiting 0 with findings on screen needs saying out loud, or the next
		// person to read the log concludes the checks found nothing.
		fmt.Fprintf(stderr, "note: %d finding(s) reported, none at or above --fail-on %s; exiting 0\n",
			len(rep.Findings), *failOn)
		return ExitOK
	}
	// The verdict line is text-mode furniture. Under --format json, stdout is
	// exactly one JSON document — printing it after the object broke `| jq` on
	// precisely the run CI most wants to pipe: the healthy repo.
	if *format != "json" {
		fmt.Fprintln(stdout, "audit clean")
	}
	return ExitOK
}

// The --fail-on thresholds, and the severity ladder they are compared on.
//
// A baseline rolled out with `on_zero_match: declare` writes a rule matching
// zero files into every repo — that is what a baseline IS — and A-4 reports
// each one forever, at `warning` and explicitly report-only. With exit 4 as the
// only verdict the documented rollout and the documented CI gate contradict
// each other. `--checks` is not the answer: naming the other eleven silently
// opts out of any check added later.
//
// The flag moves the GATE and never the report — every finding prints under
// every setting — and leaves exit 5 alone (R-12): an API that could not be
// reached is not a finding whose severity you can weigh.
const (
	auditFailOnAny = "any"
)

var auditFailOnLevels = map[string]int{
	auditFailOnAny: 1,
	"warning":      2,
	"error":        3,
	"never":        4, // above every severity, so nothing ever reaches it
}

var auditSeverityRank = map[string]int{"info": 1, "warning": 2, "error": 3}

func auditFailOnLevel(name string) (int, bool) {
	level, ok := auditFailOnLevels[name]
	return level, ok
}

// auditGateTripped reports whether any finding is at or above the threshold.
// A severity this build does not know ranks as `error`: a new, unrecognized
// severity must fail a gate rather than slip under one.
func auditGateTripped(findings []audit.Finding, level int) bool {
	for _, f := range findings {
		rank, known := auditSeverityRank[f.Severity]
		if !known {
			rank = auditSeverityRank["error"]
		}
		if rank >= level {
			return true
		}
	}
	return false
}

func fmtOwners(o []string) string {
	if o == nil {
		return "(unowned)"
	}
	if len(o) == 0 {
		return "{}"
	}
	return "{" + strings.Join(o, ", ") + "}"
}

// fmtTreeDeltas renders the tree-delta counts as a suffix to verify's success
// line. They are reported rather than folded into the change count: a reader
// checking "all within scope" against a number needs that number to be the
// ownership changes it is a claim about.
func fmtTreeDeltas(res *verify.Result) string {
	if len(res.Added) == 0 && len(res.Removed) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d added, %d removed — tree deltas, not ownership changes)",
		len(res.Added), len(res.Removed))
}
