// Package cli wires the four commands and owns the exit-code contract
// (R-17). Distinct, documented, scriptable:
//
//	0  success — applied, or audit found nothing
//	1  no-op — nothing to change
//	2  refused — would violate INV-1 or INV-2 (or S-4 size cap)
//	3  invalid input — malformed op, unresolvable scope, conflicting batch
//	4  audit findings present
//	5  inconclusive — API unavailable, token insufficient, rate limited (R-12)
//	6  validation failed post-write; rolled back
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

  plan     --op 'add_owner(/services/api, @org/team-1)' [--op ...] [--on-empty error|inherit|unowned]
           [--repo DIR] [--branch REF] [--file PATH] [--out plan.json]
           [--max-size N] [--warn-size N]
  apply    --plan plan.json [--repo DIR]
  audit    [--checks a1,a3,a6] [--format json|text] [--github-repo owner/name]
           [--token T | $GITHUB_TOKEN] [--api-url URL] [--cache-dir D] [--cache-ttl DUR]
           [--repo DIR] [--branch REF] [--file PATH]
  snapshot [--repo DIR] [--branch REF] [--file PATH] [--out snap.json]
  verify   --before before.json --after after.json [--scope PATTERN ...]
           (no --scope means: assert NOTHING changed)

Exit codes: 0 ok · 1 no-op · 2 refused (invariant/size) · 3 invalid input
            4 audit findings · 5 inconclusive (fail-closed) · 6 rolled back
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
		return tree, filePath, all, nil
	}
	if len(all) == 0 {
		return nil, "", nil, &plan.InvalidError{Msg: "no CODEOWNERS file found in .github/, root, or docs/ at " + ref + " (use --file to specify one)"}
	}
	return tree, all[0], all, nil
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
	out := fs.String("out", "", "write plan JSON here (default stdout)")
	maxSize := fs.Int("max-size", 3_000_000, "hard size cap in bytes (S-4)")
	warnSize := fs.Int("warn-size", 2_500_000, "warn threshold in bytes (R-9)")
	if err := fs.Parse(args); err != nil {
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
	tree, coPath, _, err := locate(*repo, *branch, *filePath)
	if err != nil {
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
	target := filepath.Join(repoDir, filepath.FromSlash(pf.CodeownersPath))
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
	out := fs.String("out", "", "write snapshot JSON here (default stdout)")
	if err := fs.Parse(args); err != nil {
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
	// Tree deltas are informational: the refs differ, so files come and go.
	// They are never violations (see verify.Compare).
	for _, t := range res.Added {
		fmt.Fprintf(stdout, "added:   %s  %s\n", t.Path, fmtOwners(t.Owners))
	}
	for _, t := range res.Removed {
		fmt.Fprintf(stdout, "removed: %s  %s\n", t.Path, fmtOwners(t.Owners))
	}
	if !res.OK() {
		fmt.Fprintf(stderr, "INVARIANT VIOLATED: %d path(s) changed outside the declared scope\n", len(res.Violations))
		for _, v := range res.Violations {
			fmt.Fprintf(stderr, "  %s  %s → %s\n", v.Path, fmtOwners(v.Before), fmtOwners(v.After))
		}
		return ExitRefused
	}
	fmt.Fprintf(stdout, "ok: %d change(s), all within scope (%d path(s) added, %d removed)\n",
		len(res.Changed), len(res.Added), len(res.Removed))
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
	token := fs.String("token", os.Getenv("GITHUB_TOKEN"), "GitHub token (default $GITHUB_TOKEN)")
	apiURL := fs.String("api-url", "", "API base URL (GHES), default https://api.github.com")
	cacheDir := fs.String("cache-dir", "", "disk cache directory (R-15); empty = memory only")
	cacheTTL := fs.Duration("cache-ttl", 24*time.Hour, "disk cache TTL")
	if err := fs.Parse(args); err != nil {
		return ExitInvalid
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
	if apiChecksWanted && *token != "" && *githubRepo != "" {
		parts := strings.SplitN(*githubRepo, "/", 2)
		if len(parts) != 2 {
			fmt.Fprintln(stderr, "error: --github-repo must be owner/name")
			return ExitInvalid
		}
		var cache ghapi.Cache = ghapi.NewMemCache()
		if *cacheDir != "" {
			cache = ghapi.NewDiskCache(*cacheDir, *cacheTTL)
		}
		in.Client = ghapi.New(*apiURL, *token, cache)
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
