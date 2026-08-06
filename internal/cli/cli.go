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

// SyncRecord is one `sync` run, rendered as one JSON object (R-24). One line
// per repo is what lets `jq -s` aggregate a fleet without parsing stderr.
type SyncRecord struct {
	Repo         string          `json:"repo"`
	Status       string          `json:"status"` // applied|unchanged|skipped|refused|error
	Ops          []plan.OpResult `json:"ops,omitempty"`
	OpsApplied   int             `json:"ops_applied"`
	OpsSkipped   int             `json:"ops_skipped"`
	PathsChanged int             `json:"paths_changed"`
	Created      bool            `json:"created"`
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
           [--format text|json] [--out FILE] [--summary-out FILE]
  check    (--op 'OP' ... | --policy FILE) [--format text|json]
  plan     --op 'add_owner(/services/api, @org/team-1)' [--op ...] [--on-empty error|inherit|unowned]
           [--repo DIR] [--branch REF] [--file PATH] [--out plan.json]
  apply    --plan plan.json [--repo DIR]
  audit    [--checks a1,a3,a6] [--format json|text] [--github-repo owner/name]
           [--token T | $GITHUB_TOKEN] [--api-url URL] [--cache-dir D] [--cache-ttl DUR]
           [--repo DIR] [--branch REF]
  snapshot [--repo DIR] [--branch REF] [--out snap.json]
  verify   --before before.json --after after.json [--scope PATTERN ...]
  version  print the build this binary was stamped with

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
// notion of a repository: it is handed one path and one plan, and legitimately
// follows a link within the repo (a CODEOWNERS symlinked to docs/OWNERS is a
// real layout). The CLI is the layer that knows --repo, so it is the layer that
// can tell "inside" from "outside".
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
	if len(opSpecs) == 0 {
		fmt.Fprintln(stderr, "error: at least one --op is required")
		return ExitInvalid
	}
	parsed, err := ops.ParseAll(opSpecs)
	if err != nil {
		return errExit(&plan.InvalidError{Msg: err.Error()}, stderr)
	}
	tree, coPath, _, err := locate(*repo, *branch, *filePath)
	if err != nil {
		return errExit(err, stderr)
	}
	// Not defending the write — sync and apply each refuse on their own. This
	// defends the ARTIFACT: a plan whose codeowners_path resolves outside the clone
	// describes an edit to a file GitHub never reads (it does not follow a symlinked
	// CODEOWNERS), and every downstream refusal fires after a human has already
	// reviewed and approved it. Deciding here means it never exists to approve.
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
	target := filepath.Join(repoDir, filepath.FromSlash(pf.CodeownersPath))
	// The same containment `sync` enforces, enforced again here. A plan is an
	// artifact: it is reviewed in one place and applied somewhere else entirely,
	// possibly against a different clone via --repo, so the decision cannot be
	// left behind in the verb that produced it.
	if err := containedWritePath(repoDir, target); err != nil {
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
	if !res.OK() {
		fmt.Fprintf(stderr, "INVARIANT VIOLATED: %d path(s) changed outside the declared scope\n", len(res.Violations))
		for _, v := range res.Violations {
			fmt.Fprintf(stderr, "  %s  %s → %s\n", v.Path, fmtOwners(v.Before), fmtOwners(v.After))
		}
		return ExitRefused
	}
	fmt.Fprintf(stdout, "ok: %d change(s), all within scope\n", len(res.Changed))
	return ExitOK
}

func cmdAudit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "path to local git repository")
	branch := fs.String("branch", "HEAD", "ref to audit")
	filePath := fs.String("file", "", "CODEOWNERS path override")
	checksFlag := fs.String("checks", "", "comma-separated subset, e.g. a1,a3,a6 (default: all)")
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
	if err := fs.Parse(args); err != nil {
		return flagParseCode(err)
	}
	// $GITHUB_TOKEN remains the documented fallback; it is just resolved past
	// the point where anything can render it. An explicit --token still wins.
	authToken := *token
	if authToken == "" {
		authToken = os.Getenv("GITHUB_TOKEN")
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
		return ExitFindings
	}
	fmt.Fprintln(stdout, "audit clean")
	return ExitOK
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
