// Package audit is Engine B: read-only rot detection over a CODEOWNERS file.
//
// It never writes (R-0). Its output is findings plus, where a fix is
// expressible, Engine A operation strings a human reviews and feeds through
// plan/apply — the system's single writer path.
//
// API-dependent checks (A-1..A-3) fail closed (R-12): an inconclusive lookup
// produces status "unknown", a non-empty Report.Inconclusive (exit 5), and
// never a removal proposal.
package audit

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/ghapi"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/pattern"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
	"github.com/jordonpeterson/codeowners-tool/internal/resolve"
)

// Finding is one audit result.
type Finding struct {
	Check            string     `json:"check"`
	Severity         string     `json:"severity"` // error | warning | info
	Line             int        `json:"line,omitempty"`
	Pattern          string     `json:"pattern,omitempty"`
	Owner            string     `json:"owner,omitempty"`
	Status           string     `json:"status,omitempty"` // dead | not-in-org | no-write-access | unknown | unverifiable
	Message          string     `json:"message"`
	SuggestedPattern string     `json:"suggested_pattern,omitempty"` // A-5
	FixOps           []string   `json:"fix_ops,omitempty"`           // Engine A ops (R-0)
	Reassignment     []plan.Row `json:"reassignment,omitempty"`      // R-14
}

// Report is the audit output.
type Report struct {
	Findings     []Finding `json:"findings"`
	Inconclusive []string  `json:"inconclusive,omitempty"` // non-empty → exit 5 (R-12)
}

// Input configures a run.
type Input struct {
	Content        []byte
	Tree           []string
	CodeownersPath string   // for A-11
	AllPresent     []string // CODEOWNERS files at the three S-8 locations (A-10)
	NestedPresent  []string // CODEOWNERS files GitHub never searches (A-10)
	// StagedHigher is the CODEOWNERS files sitting in the WORKING TREE at an
	// S-8 location that OUTRANKS CodeownersPath and are not committed at the
	// ref (A-10). The caller establishes both facts; audit only reports them.
	StagedHigher  []string
	SymlinkTarget string          // set when CodeownersPath is a symlink at ref (A-10)
	Checks        map[string]bool // nil = all
	WarnSize      int             // default 2,500,000

	// API-dependent checks run only when Client is set.
	Client              *ghapi.Client
	RepoOwner, RepoName string
}

func (in *Input) enabled(check string) bool {
	return in.Checks == nil || in.Checks[check]
}

// Run executes the audit.
func Run(in Input) *Report {
	if in.WarnSize == 0 {
		in.WarnSize = 2_500_000
	}
	rep := &Report{}

	// A-10, symlink shape: the governing entry is a link blob, so nothing in
	// this repository is owned and Content is the LINK TARGET, not a document.
	//
	// `cat-file` hands back a symlink's target as its content, and parsing that
	// produced `[A-4/warning] pattern "../OWNERS.real" matches zero tracked
	// files` — a finding about a filesystem path, leaving the operator to infer
	// the real defect from A-9's "100% of tracked paths have no owner". `sync`
	// already refuses this repository and names the link; audit is where the
	// same fact belongs, so it is reported here and everything derived from the
	// bytes below is skipped rather than invented.
	if in.SymlinkTarget != "" {
		if in.enabled("A-10") {
			rep.add(Finding{Check: "A-10", Severity: "error",
				Message: fmt.Sprintf("%s is a symlink to %q, and GitHub does not follow a symlinked CODEOWNERS (S-8) — it loads no rules at all, so every path in this repository is unowned; replace the link with a real file. No other check ran: the file's bytes are the link target, not a CODEOWNERS document",
					in.CodeownersPath, in.SymlinkTarget)})
		} else {
			// A subset that excludes A-10 must not come back clean over bytes
			// that are not a CODEOWNERS at all: the requested check could not
			// be run, which is exit 5, not a vacuous pass (R-12).
			rep.inconclusive(fmt.Sprintf("%s is a symlink to %q; its bytes are the link target, not a CODEOWNERS document — run `audit --checks a10` for the finding",
				in.CodeownersPath, in.SymlinkTarget))
		}
		return rep
	}

	f := file.Parse(in.Content)
	rules := f.Rules()
	res := resolve.All(f, in.Tree)
	winners := map[int][]string{}
	for p, r := range res {
		if r.Matched {
			winners[r.LineIndex] = append(winners[r.LineIndex], p)
		}
	}

	// A-8: syntax errors (each invalid line is skipped by GitHub, not the
	// whole file — but every one is a rule someone thinks exists).
	if in.enabled("A-8") {
		for _, ln := range f.InvalidLines() {
			rep.add(Finding{Check: "A-8", Severity: "error", Line: ln.Err.Line,
				Message: fmt.Sprintf("%s: %s (GitHub skips this line: %q)", ln.Err.Kind, ln.Err.Message, ln.Err.Source)})
		}
	}

	// Per-rule pattern checks: A-4 (dead), A-5 (case), A-6 (shadowed).
	for _, r := range rules {
		matches := 0
		for _, p := range in.Tree {
			if r.Pattern.Match(p) {
				matches++
			}
		}
		if matches == 0 {
			if sugg, caseOnly := caseCorrected(r, in.Tree); caseOnly {
				if in.enabled("A-5") {
					msg := fmt.Sprintf("pattern %q matches zero files ONLY because of case — CODEOWNERS is case-sensitive (S-6)", r.PatternText)
					if sugg != "" {
						msg += fmt.Sprintf("; %q would match", sugg)
					} else {
						// The real tree casing is not simply lowercase; a
						// mechanical suggestion would be wrong (found in
						// review) — the user must correct the casing by hand.
						msg += "; correct the pattern to the tree's actual casing"
					}
					rep.add(Finding{Check: "A-5", Severity: "warning", Line: r.LineIndex + 1, Pattern: r.PatternText,
						SuggestedPattern: sugg, Message: msg})
				}
			} else if sugg, collides, normOnly := normalizationCorrected(r, in.Tree); normOnly {
				if in.enabled("A-5") {
					// A-5 rather than a new check: same defect class as the
					// case miss it already covers — the pattern is dead only
					// because of an invisible spelling difference, the remedy
					// is to retype it from the tree, and the severity and the
					// suggested_pattern field are the same. Case is one
					// invisible difference; NFC vs NFD is the other.
					msg := fmt.Sprintf("pattern %q matches zero tracked files, but %q differs from it ONLY by Unicode normalization", r.PatternText, collides)
					if cp := composedCodepoints(r.PatternText); cp != "" {
						msg += " (" + cp + ")"
					}
					msg += " — CODEOWNERS matches bytes, so two spellings that render identically never match; retype the pattern from the tree's own spelling. Compared under a partial NFD over Latin-1 accented letters, not a full Unicode normalization"
					rep.add(Finding{Check: "A-5", Severity: "warning", Line: r.LineIndex + 1, Pattern: r.PatternText,
						SuggestedPattern: sugg, Message: msg})
				}
			} else if in.enabled("A-4") {
				// R-11: report-only, permanently. A dead pattern may be
				// deliberate and forward-looking; deleting it destroys intent.
				rep.add(Finding{Check: "A-4", Severity: "warning", Line: r.LineIndex + 1, Pattern: r.PatternText,
					Message: fmt.Sprintf("pattern %q matches zero tracked files (report-only: may be deliberate, R-11)", r.PatternText)})
			}
			continue
		}
		if in.enabled("A-6") && len(winners[r.LineIndex]) == 0 {
			shadowers := map[int]bool{}
			for _, p := range in.Tree {
				if r.Pattern.Match(p) {
					shadowers[res[p].LineIndex] = true
				}
			}
			var lines []string
			for l := range shadowers {
				lines = append(lines, fmt.Sprintf("line %d", l+1))
			}
			sort.Strings(lines)
			rep.add(Finding{Check: "A-6", Severity: "warning", Line: r.LineIndex + 1, Pattern: r.PatternText,
				Message: fmt.Sprintf("rule %q is fully shadowed by %s and can never take effect (S-1)", r.PatternText, strings.Join(lines, ", "))})
		}
	}

	// A-7: duplicate patterns.
	if in.enabled("A-7") {
		byPattern := map[string][]int{}
		for _, r := range rules {
			byPattern[r.PatternText] = append(byPattern[r.PatternText], r.LineIndex+1)
		}
		var pats []string
		for pat, lines := range byPattern {
			if len(lines) > 1 {
				pats = append(pats, pat)
			}
		}
		sort.Strings(pats)
		for _, pat := range pats {
			lines := byPattern[pat]
			rep.add(Finding{Check: "A-7", Severity: "warning", Pattern: pat,
				Message: fmt.Sprintf("pattern %q appears on lines %v; only the last takes effect (S-1)", pat, lines)})
		}
	}

	// A-9: unowned coverage.
	if in.enabled("A-9") {
		var unowned []string
		for _, p := range in.Tree {
			if !res[p].Matched {
				unowned = append(unowned, p)
			}
		}
		if len(unowned) > 0 {
			sort.Strings(unowned)
			sample := unowned
			if len(sample) > 10 {
				sample = sample[:10]
			}
			rep.add(Finding{Check: "A-9", Severity: "info",
				Message: fmt.Sprintf("%d of %d tracked paths (%.0f%%) have no owner; e.g. %s",
					len(unowned), len(in.Tree), 100*float64(len(unowned))/float64(len(in.Tree)), strings.Join(sample, ", "))})
		}
	}

	// A-10: multiple CODEOWNERS files (S-8: first found wins, never merged).
	if in.enabled("A-10") && len(in.AllPresent) > 1 {
		rep.add(Finding{Check: "A-10", Severity: "error",
			Message: fmt.Sprintf("multiple CODEOWNERS files present (%s); GitHub uses only %q and silently ignores the rest (S-8)",
				strings.Join(in.AllPresent, ", "), in.AllPresent[0])})
	}

	// A-10, nested shape: package-level CODEOWNERS files GitHub never searches.
	//
	// Left behind by Bazel/Gerrit OWNERS migrations and by merging split repos,
	// they look authoritative and people route review by them; GitHub honors
	// not one line. Discovery only ever tested the three root-level S-8
	// locations, so `audit clean`, exit 0 — the CI gate asserting this
	// repository has no ownership rot — was asserting it over whole files of
	// assignments that do nothing.
	//
	// `warning`, not the `error` the case above carries. Two root-level files
	// are ambiguity in the document that GOVERNS — both are searched, one
	// silently loses. A nested file is not searched at all, so what governs is
	// never in doubt, and it is often a deliberate artifact another tool still
	// consumes. `--fail-on error` is the tier for "GitHub is doing something
	// other than what the file says"; ranking this there turns that gate red on
	// every migrated monorepo over a condition no edit to the governing file
	// resolves. It is still rot, so it clears `info` and trips the default gate.
	if in.enabled("A-10") && len(in.NestedPresent) > 0 {
		rep.add(Finding{Check: "A-10", Severity: "warning",
			Message: fmt.Sprintf("%d CODEOWNERS file(s) GitHub never loads: %s — only .github/, root and docs/ at the repository ROOT are searched (S-8), so no rule in them takes effect however the team reads them",
				len(in.NestedPresent), strings.Join(in.NestedPresent, ", "))})
	}

	// A-10, mid-migration shape: a higher-precedence CODEOWNERS in the working
	// tree, not yet committed.
	//
	// The check above is tree-only, so a repo moving from root `CODEOWNERS` to
	// `.github/CODEOWNERS` — the new file staged, the migration commit not yet
	// made — came back `audit clean`, exit 0, while every rule in the file this
	// report describes is one commit away from being ignored. Reporting it does
	// not change what audit RESOLVES against: ownership below is still the ref's,
	// because that is what GitHub reads. This is the one fact about the checkout
	// that changes what the report will mean tomorrow.
	//
	// `error`, like two committed files and unlike the nested shape: both are
	// ambiguity about which document GOVERNS, and the resolution here is a
	// single `git commit` away rather than a permanent property of a migrated
	// monorepo.
	if in.enabled("A-10") && len(in.StagedHigher) > 0 {
		rep.add(Finding{Check: "A-10", Severity: "error",
			Message: fmt.Sprintf("%s is in the working tree but not committed at this ref, and outranks %s under S-8 (.github/ > root > docs/, first found wins, never merged) — this report describes %s, and every rule in it stops applying the moment that file is committed",
				strings.Join(in.StagedHigher, ", "), in.CodeownersPath, in.CodeownersPath)})
	}

	// A-11: the CODEOWNERS file itself is unowned. A zero-owner match is
	// exactly as unowned as no match at all (found in review).
	if in.enabled("A-11") && in.CodeownersPath != "" {
		if r, ok := res[in.CodeownersPath]; !ok || !r.Matched || len(r.Owners) == 0 {
			rep.add(Finding{Check: "A-11", Severity: "warning",
				Message: fmt.Sprintf("%s itself has no owner — anyone with write access can change ownership rules unreviewed", in.CodeownersPath)})
		}
	}

	// A-12: size approaching the 3 MB cliff (S-4).
	if in.enabled("A-12") {
		size := len(in.Content)
		switch {
		case size > 3_000_000:
			rep.add(Finding{Check: "A-12", Severity: "error",
				Message: fmt.Sprintf("file is %d bytes — OVER 3 MB: GitHub is not loading it at all; ownership is silently off (S-4)", size)})
		case size >= in.WarnSize:
			rep.add(Finding{Check: "A-12", Severity: "warning",
				Message: fmt.Sprintf("file is %d bytes, approaching the 3 MB limit at which GitHub silently stops loading it (S-4)", size)})
		}
	}

	// A-1/A-2/A-3: owner liveness and access — API required, fail closed.
	if in.Client != nil && (in.enabled("A-1") || in.enabled("A-2") || in.enabled("A-3")) {
		auditOwners(&in, rep, f, rules, winners)
	}
	return rep
}

func (r *Report) add(f Finding) { r.Findings = append(r.Findings, f) }

func (r *Report) inconclusive(reason string) {
	for _, x := range r.Inconclusive {
		if x == reason {
			return
		}
	}
	r.Inconclusive = append(r.Inconclusive, reason)
}

type ownerSites struct {
	owner string
	rules []*file.Rule
}

func auditOwners(in *Input, rep *Report, f *file.File, rules []*file.Rule, winners map[int][]string) {
	sites := map[string]*ownerSites{}
	var order []string
	for _, r := range rules {
		for _, o := range r.Owners {
			if sites[o] == nil {
				sites[o] = &ownerSites{owner: o}
				order = append(order, o)
			}
			sites[o].rules = append(sites[o].rules, r)
		}
	}

	for _, o := range order {
		s := sites[o]

		// R-13: email owners resolve via verified email; the API cannot
		// validate them reliably. Unverifiable, never dead.
		if file.IsEmailOwner(o) {
			rep.add(Finding{Check: "A-1", Severity: "info", Owner: o, Status: "unverifiable",
				Message: fmt.Sprintf("%s is an email owner; existence and access cannot be verified via the API (R-13) — verify manually", o)})
			continue
		}

		if org, slug, isTeam := splitTeam(o); isTeam {
			auditTeam(in, rep, s, org, slug, rules, winners)
		} else {
			auditUser(in, rep, s, strings.TrimPrefix(o, "@"), rules, winners)
		}
	}
}

func auditUser(in *Input, rep *Report, s *ownerSites, login string, rules []*file.Rule, winners map[int][]string) {
	unknown := func(check, reason string) {
		rep.inconclusive(reason)
		rep.add(Finding{Check: check, Severity: "warning", Owner: s.owner, Status: "unknown",
			Message: fmt.Sprintf("%s could not be verified (%s) — no removal proposed (R-12)", s.owner, reason)})
	}

	if in.enabled("A-1") {
		exists, err := in.Client.UserExists(login)
		if err != nil {
			unknown("A-1", errReason(err))
			return
		}
		if !exists {
			fixes, rows := proposeRemoval(s, rules, winners)
			rep.add(Finding{Check: "A-1", Severity: "error", Owner: s.owner, Status: "dead",
				Message:      fmt.Sprintf("user %s does not exist (deleted or renamed); review requests to it silently do nothing", s.owner),
				FixOps:       fixes,
				Reassignment: rows})
			return
		}
	}

	org := in.RepoOwner
	if in.enabled("A-2") && org != "" {
		if err := in.Client.ProbeOrg(org); err != nil {
			unknown("A-2", errReason(err))
			return
		}
		member, err := in.Client.UserIsOrgMember(org, login)
		if err != nil {
			unknown("A-2", errReason(err))
			return
		}
		if !member {
			fixes, rows := proposeRemoval(s, rules, winners)
			rep.add(Finding{Check: "A-2", Severity: "warning", Owner: s.owner, Status: "not-in-org",
				Message:      fmt.Sprintf("%s exists but is not a member of %s (note: outside collaborators can still hold write access — A-3 is authoritative)", s.owner, org),
				FixOps:       fixes,
				Reassignment: rows})
		}
	}

	if in.enabled("A-3") && in.RepoOwner != "" && in.RepoName != "" {
		if err := in.Client.ProbeRepo(in.RepoOwner, in.RepoName); err != nil {
			unknown("A-3", errReason(err))
			return
		}
		hasWrite, err := in.Client.UserHasWrite(in.RepoOwner, in.RepoName, login)
		if err != nil {
			unknown("A-3", errReason(err))
			return
		}
		if !hasWrite {
			fixes, rows := proposeRemoval(s, rules, winners)
			rep.add(Finding{Check: "A-3", Severity: "error", Owner: s.owner, Status: "no-write-access",
				Message:      fmt.Sprintf("%s lacks explicit write access to %s/%s — it will NEVER receive a review request (S-5)", s.owner, in.RepoOwner, in.RepoName),
				FixOps:       fixes,
				Reassignment: rows})
		}
	}
}

func auditTeam(in *Input, rep *Report, s *ownerSites, org, slug string, rules []*file.Rule, winners map[int][]string) {
	unknown := func(check, reason string) {
		rep.inconclusive(reason)
		rep.add(Finding{Check: check, Severity: "warning", Owner: s.owner, Status: "unknown",
			Message: fmt.Sprintf("%s could not be verified (%s) — no removal proposed (R-12)", s.owner, reason)})
	}

	// A team 404 is only meaningful after proving we can enumerate the org.
	// Attribute the probe failure to whichever team check is actually on.
	probeCheck := "A-1"
	if !in.enabled("A-1") {
		probeCheck = "A-3"
	}
	if err := in.Client.ProbeOrg(org); err != nil {
		unknown(probeCheck, errReason(err))
		return
	}

	if in.enabled("A-1") {
		exists, err := in.Client.TeamExists(org, slug)
		if err != nil {
			unknown("A-1", errReason(err))
			return
		}
		if !exists {
			fixes, rows := proposeRemoval(s, rules, winners)
			rep.add(Finding{Check: "A-1", Severity: "error", Owner: s.owner, Status: "dead",
				Message:      fmt.Sprintf("team %s does not exist (deleted or renamed)", s.owner),
				FixOps:       fixes,
				Reassignment: rows})
			return
		}
	}

	if in.enabled("A-3") && in.RepoOwner != "" && in.RepoName != "" {
		hasWrite, err := in.Client.TeamHasWrite(org, slug, in.RepoOwner, in.RepoName)
		if err != nil {
			unknown("A-3", errReason(err))
			return
		}
		if !hasWrite {
			fixes, rows := proposeRemoval(s, rules, winners)
			rep.add(Finding{Check: "A-3", Severity: "error", Owner: s.owner, Status: "no-write-access",
				Message:      fmt.Sprintf("team %s lacks explicit write access to %s/%s — it will NEVER receive a review request (S-5)", s.owner, in.RepoOwner, in.RepoName),
				FixOps:       fixes,
				Reassignment: rows})
		}
	}
}

// proposeRemoval builds the Engine A ops that would remove the owner, plus —
// where the owner is a rule's SOLE owner — the reassignment preview (R-14):
// affected paths with before → after owner sets, never a bare line deletion.
func proposeRemoval(s *ownerSites, allRules []*file.Rule, winners map[int][]string) ([]string, []plan.Row) {
	var fixes []string
	var rows []plan.Row
	for _, r := range s.rules {
		fix := fmt.Sprintf("remove_owner(%s, %s)", r.PatternText, s.owner)
		// Patterns unrepresentable in the op DSL (top-level commas, unbalanced
		// brackets) cannot become fix ops; emitting an unparseable one would
		// fail downstream (review findings) — omit it and leave the
		// reassignment preview as the guide.
		if _, err := ops.Parse(fix); err == nil {
			fixes = append(fixes, fix)
		}
		if len(r.Owners) != 1 {
			continue
		}
		// Sole owner: show what each affected path falls through to.
		var without []*file.Rule
		for _, x := range allRules {
			if x.LineIndex != r.LineIndex {
				without = append(without, x)
			}
		}
		affected := append([]string(nil), winners[r.LineIndex]...)
		sort.Strings(affected)
		for _, p := range affected {
			after := resolve.One(without, p)
			rows = append(rows, plan.Row{Path: p, Before: []string{s.owner}, After: after.Owners})
		}
	}
	return fixes, rows
}

// caseCorrected reports whether the rule matches zero files ONLY because of
// case (A-5 / S-6), and — separately — a suggested corrected pattern, which
// is non-empty only when it VERIFIABLY matches real tree paths
// case-sensitively. A case-only miss whose real casing isn't simply
// lowercase yields ("", true): the finding fires, but no unverified
// suggestion is made.
func caseCorrected(r *file.Rule, tree []string) (suggestion string, caseOnly bool) {
	lowered := strings.ToLower(r.PatternText)
	p, err := pattern.Compile(lowered)
	if err != nil {
		return "", false
	}
	for _, path := range tree {
		if lowered != r.PatternText && p.Match(path) {
			return lowered, true
		}
	}
	for _, path := range tree {
		if p.Match(strings.ToLower(path)) {
			return "", true
		}
	}
	return "", false
}

// normalizationCorrected reports whether the rule matches zero files ONLY
// because the pattern and the tree disagree about Unicode normalization, and
// names the tracked path it collides with.
//
// Both sides go through the same partial decomposition (see normalize.go), so
// the test is symmetric: whichever side is composed, they agree afterwards only
// if the difference was composition. Two names that differ by a real accent —
// "réunion" against "rèunion" — decompose to different marks and are correctly
// left to A-4.
//
// suggestion is non-empty only when the decomposed pattern VERIFIABLY matches a
// real tree path byte for byte, matching caseCorrected's rule: an unverified
// suggestion between two identical-looking strings is worse than none.
func normalizationCorrected(r *file.Rule, tree []string) (suggestion, collides string, normOnly bool) {
	nfd := decompose(r.PatternText)
	p, err := pattern.Compile(nfd)
	if err != nil {
		return "", "", false
	}
	for _, path := range tree {
		if !p.Match(decompose(path)) {
			continue
		}
		if collides == "" {
			collides = path
		}
		// The decomposed pattern is offered as a fix only once it is shown to
		// match a path exactly as the tree committed it.
		if suggestion == "" && nfd != r.PatternText && p.Match(path) {
			suggestion, collides = nfd, path
		}
	}
	return suggestion, collides, collides != ""
}

func splitTeam(owner string) (org, slug string, ok bool) {
	if !strings.HasPrefix(owner, "@") || !strings.Contains(owner, "/") {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(owner, "@"), "/", 2)
	return parts[0], parts[1], true
}

func errReason(err error) string {
	var inc *ghapi.Inconclusive
	if errors.As(err, &inc) {
		return inc.Reason
	}
	return err.Error()
}
