// Package policy parses the JSON policy file: the unit of review for a fleet
// rollout (R-20).
//
// A policy is the complete, version-controlled statement of what ran across N
// repositories, so it is validated strictly. Unknown fields, bad enum values,
// and duplicate keys are hard errors — a typo'd `on_zero_mtach` that silently
// fell back to the default would apply the wrong policy to every repo at once.
//
// Syntax errors fail fast; semantic errors accumulate, because fixing a
// generated 40-op policy one error per run is miserable.
package policy

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
)

// Version is the only policy format version this binary understands.
const Version = 1

// MaxPolicyBytes caps what Load will read into memory.
//
// A policy is reviewed by a human before it runs against a fleet, so 1 MB is
// already far past anything reviewable — the largest plausible generated policy,
// a few thousand ops, is tens of kilobytes. Without a cap `check --policy` reads
// whatever it is pointed at, and the first sign of a generator that ran away is
// the runner being OOM-killed partway through a rollout.
const MaxPolicyBytes = 1 << 20

// The field sets are PER LEVEL, not one merged bag. `description` is legal at
// the top and meaningless on an op; accepting it in both places would let a
// generator put the policy's description on op 17 and nothing would notice.
var (
	topFields = []string{"version", "name", "description", "create", "on_empty", "max_paths_changed", "defaults", "lint", "ops"}
	opFields  = []string{"op", "id", "on_zero_match", "on_except_zero_match", "on_unowned", "except", "owners", "note"}
	// `defaults` carries the genuinely PER-OP settings and nothing else
	// (R-35c). on_empty is one policy for the run and stays top-level: two
	// spellings of one setting is a precedence rule nobody wrote down.
	defaultsFields = []string{"on_zero_match", "on_except_zero_match", "on_unowned"}
	lintFields     = []string{"remove_stale_paths", "on_empty"}
)

// The two enums, in the order the docs present them. Alphabetizing a legal set
// in an error message quietly re-ranks it; `require` is the default and belongs
// first.
var (
	onEmptyValues         = []string{"error", "inherit", "unowned"}
	zeroMatchValues       = []string{ops.ZeroMatchRequire, ops.ZeroMatchSkip, ops.ZeroMatchDeclare}
	exceptZeroMatchValues = []string{ops.ExceptZeroMatchRequire, ops.ExceptZeroMatchAllow}
	unownedValues         = []string{ops.UnownedAssign, ops.UnownedSkip}
)

// Names from revision 1 of the design. A policy generated against the older doc
// fails on a name that no longer exists anywhere, so the message has to carry
// the rename itself — otherwise the operator's only route to the answer is the
// changelog.
var (
	renamedOpFields  = map[string]string{"when_absent": "on_zero_match"}
	renamedZeroMatch = map[string]string{
		"error": ops.ZeroMatchRequire,
		"write": ops.ZeroMatchDeclare,
	}
)

// Policy is a parsed, validated policy file.
type Policy struct {
	Version     int
	Name        string
	Description string
	OnEmpty     string // "" | error | inherit | unowned
	// MaxPathsChanged is R-25's blast-radius ceiling, -1 when the policy sets
	// none. The ceiling belongs in the reviewed artifact rather than in one
	// call site's flags: "this wave touches dozens of files per repo, not
	// thousands" is a claim about the intent, and a ceiling that lives in a
	// shell line survives exactly as long as that shell line.
	MaxPathsChanged int
	// Create is R-34: the reviewed artifact, not a flag, decides whether a
	// repository without a CODEOWNERS file gets one. R-23 still holds, so an
	// existing file is never overwritten and this is safe to leave true for a
	// fleet where only some repositories have a file.
	Create bool
	// Defaults is R-35, supplying the per-op settings an op does not state.
	// It reaches only ops that can carry the setting (R-35e).
	Defaults Defaults
	// Lint is R-36. sync does not act on it, but every command validates it
	// (R-36e): a malformed block that only surfaced when someone ran lint
	// would be a defect riding through the whole fleet unseen.
	Lint  LintPrefs
	Ops   []ops.Op
	Notes map[string]string // op ID -> note
}

// Defaults carries the per-op settings a policy states once (R-35). Only
// settings that are genuinely per-op belong here: on_empty stays top-level
// because it is one policy for the run, and two spellings for one setting is
// the ambiguity R-35c exists to remove.
type Defaults struct {
	OnZeroMatch       string
	OnExceptZeroMatch string
	OnUnowned         string
}

// LintPrefs mirrors lint's flags so the repair policy is reviewed in the same
// artifact as the ownership policy (R-36).
type LintPrefs struct {
	RemoveStalePaths bool
	OnEmpty          string
}

// Error is one located problem in a policy file.
type Error struct {
	File    string
	Line    int // 1-based; 0 when not locatable
	Col     int // 1-based; 0 when not locatable
	OpIndex int // -1 when not op-scoped
	OpID    string
	Msg     string
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.File != "" {
		b.WriteString(e.File)
		if e.Line > 0 {
			fmt.Fprintf(&b, ":%d", e.Line)
			if e.Col > 0 {
				fmt.Fprintf(&b, ":%d", e.Col)
			}
		}
		b.WriteString(": ")
	}
	if e.OpIndex >= 0 {
		fmt.Fprintf(&b, "ops[%d]", e.OpIndex)
		if e.OpID != "" {
			fmt.Fprintf(&b, " (id %q)", e.OpID)
		}
		b.WriteString(": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// MultiError carries every semantic problem found in one pass.
//
// It prints the problems and nothing else — no header, no "fix the policy, do
// not retry" advice. That advice belongs to whichever command is reporting, not
// to the parse result, and a caller that wants to render one problem per line
// needs Error() to be exactly that.
type MultiError struct{ Errs []error }

func (m *MultiError) Error() string {
	parts := make([]string, len(m.Errs))
	for i, e := range m.Errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

// Parse decodes and fully validates src. filename appears in error messages.
//
// Syntax and duplicate-key problems return immediately: once the token stream is
// broken, or a key's value is ambiguous, every later finding is about a document
// nobody wrote. Everything after that accumulates, so one run of `check` reports
// every problem in a generated policy rather than the first.
func Parse(src []byte, filename string) (*Policy, error) {
	root, dups, syntax := scanSource(src, filename)
	if syntax != nil {
		return nil, syntax
	}
	if len(dups) > 0 {
		return nil, &MultiError{Errs: dups}
	}
	v := &validator{file: filename, src: src}
	p := v.policy(root)
	if len(v.errs) > 0 {
		// Report in file order. The checks run in passes — every op individually,
		// then the cross-op ones — so ops[4]'s problem can otherwise print before
		// ops[2]'s. An operator fixing a 40-op policy works down the file, and a
		// list that jumps around costs them a second pass over it.
		sort.SliceStable(v.errs, func(i, j int) bool {
			a, b := v.errs[i], v.errs[j]
			if a.Line != b.Line {
				return a.Line < b.Line
			}
			return a.Col < b.Col
		})
		errs := make([]error, len(v.errs))
		for i, e := range v.errs {
			errs[i] = e
		}
		return nil, &MultiError{Errs: errs}
	}
	return p, nil
}

// Load is Parse over a file on disk.
//
// The OS error stays wrapped so callers can errors.Is it: the fleet script's
// first line is `check --policy`, and a typo'd path must be distinguishable from
// a malformed file — one is a shell mistake, the other sends someone to a
// generator.
func Load(path string) (*Policy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read policy file: %w", err)
	}
	defer f.Close()
	// One byte past the cap, rather than trusting Stat: the size a filesystem
	// reports is not always the number of bytes a read produces, and reading
	// first to find out how much was read is what the cap exists to prevent.
	src, err := io.ReadAll(io.LimitReader(f, MaxPolicyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read policy file: %w", err)
	}
	if len(src) > MaxPolicyBytes {
		return nil, &Error{File: path, OpIndex: -1, Msg: fmt.Sprintf(
			"the policy file is larger than the %d-byte size limit; a policy is a human-reviewed artifact, and one this big is a broken generator — finding that out by exhausting the runner's memory is the wrong way round",
			MaxPolicyBytes)}
	}
	return Parse(src, path)
}

// validator accumulates located problems over one document.
type validator struct {
	file string
	src  []byte
	errs []*Error
}

// at records one problem. opIndex is -1 when the problem is not op-scoped.
func (v *validator) at(off, opIndex int, opID, format string, args ...any) {
	line, col := lineCol(v.src, off)
	v.errs = append(v.errs, &Error{
		File:    v.file,
		Line:    line,
		Col:     col,
		OpIndex: opIndex,
		OpID:    opID,
		Msg:     fmt.Sprintf(format, args...),
	})
}

// opInfo is what the cross-op checks need after each op has been validated
// individually. It is kept separate from Policy.Ops because an op whose string
// failed to parse still has an index and an id that later errors must name.
type opInfo struct {
	index  int
	id     string
	idOff  int
	off    int
	kind   ops.Kind
	parsed bool
}

func (v *validator) policy(root *jsonValue) *Policy {
	p := &Policy{MaxPathsChanged: -1}

	// Unknown keys are reported where they appear; known ones are collected and
	// then validated in a fixed order, so the error list reads the same way twice
	// for the same file. Duplicates were already rejected, so one member per key.
	fields := make(map[string]*member, len(root.members))
	for _, m := range root.members {
		if ignoredKey(m.key) {
			continue
		}
		if !contains(topFields, m.key) {
			v.unknownTopField(m)
			continue
		}
		fields[m.key] = m
	}

	v.version(p, fields["version"], root.off)
	p.Name = v.optString(fields["name"], "name", -1, "")
	p.Description = v.optString(fields["description"], "description", -1, "")
	v.create(p, fields["create"])
	v.onEmpty(p, fields["on_empty"])
	v.maxPathsChanged(p, fields["max_paths_changed"])
	// Before the ops: the block supplies what an op does not state, so it has
	// to be resolved by the time each op is built (R-35a).
	v.defaults(p, fields["defaults"])
	// R-36e: validated on every command, including the ones that never act on
	// it. "Ignores" means does not act on, never does not validate — a defect
	// that surfaces only when somebody happens to run the other command is the
	// fleet-scale failure exit 3 exists to prevent.
	v.lint(p, fields["lint"])
	infos := v.opsArray(p, fields["ops"], root.off)
	v.checkDuplicateIDs(infos)
	v.checkOnEmptyRequired(fields["on_empty"] != nil, infos)
	return p
}

// version is required rather than optional-defaulting-to-1: a strict format read
// by pinned binaries across a fleet, with no version marker, is a corner with no
// way out. Absence and `0` are therefore the same rejection — if `"version": 0`
// were accepted it would be indistinguishable from a file that never had one.
//
// A version this binary does not implement and a version that is nonsense are
// two different jobs for the operator: upgrade the tool, or go fix the
// generator. The verdict rides on the two NUMBERS the message names; a
// malformed version has no such pair, so it must never send anyone to upgrade
// over a stray quote mark.
func (v *validator) version(p *Policy, m *member, rootOff int) {
	if m == nil {
		v.at(rootOff, -1, "", `missing required field "version"; add "version": %d — the marker is required, not defaulted, so a pinned binary always knows which format it is reading`, Version)
		return
	}
	val := m.val
	if val.kind != kNumber {
		v.at(val.off, -1, "", `field "version" must be a number, got %s`, val.describe())
		return
	}
	n, err := strconv.Atoi(val.num.String())
	switch {
	case err != nil:
		v.at(val.off, -1, "", `field "version" must be a whole number, got %s`, val.raw())
	case n == 0:
		v.at(val.off, -1, "", `field "version" must be a positive integer, got 0; 0 is what an absent field decodes to, so it cannot also name a real format version`)
	case n < 0:
		v.at(val.off, -1, "", `field "version" must be a positive integer, got %s`, val.raw())
	case n > Version:
		v.at(val.off, -1, "", `policy version %d is newer than this binary understands; this build implements policy version %d, so upgrade codeowners-tool or regenerate the file at version %d`, n, Version, Version)
	default:
		p.Version = n
	}
}

// optString reads an optional string field, reporting the JSON type it actually
// found rather than letting the decoder name a Go type (UX rule 5).
func (v *validator) optString(m *member, name string, opIndex int, opID string) string {
	if m == nil {
		return ""
	}
	if m.val.kind != kString {
		v.at(m.val.off, opIndex, opID, `field %q must be a string, got %s`, name, m.val.describe())
		return ""
	}
	return m.val.str
}

// onEmpty validates R-6's policy at LOAD time. plan.go only reaches "unknown
// --on-empty policy" when a removal actually empties an owner set, so a policy
// saying "inhrit" passes review, works on 46 repos, and blows up on repo 47 —
// precisely the repo-47 surprise `check` exists to turn into a repo-0 one.
func (v *validator) onEmpty(p *Policy, m *member) {
	if m == nil {
		return
	}
	val := m.val
	switch {
	case val.kind != kString:
		v.at(val.off, -1, "", `field "on_empty" must be a string, got %s; legal values are %s`, val.describe(), list(onEmptyValues))
	case val.str == "":
		// Present-and-empty is not "unset". An absent on_empty says the policy
		// makes no removal that could empty an owner set; "" says nothing at all,
		// while reading to a human reviewer as though a choice was made.
		v.at(val.off, -1, "", `field "on_empty" is present but empty; an absent on_empty and an empty one are different states of the file, and "" declares nothing — legal values are %s`, list(onEmptyValues))
	case !contains(onEmptyValues, val.str):
		v.at(val.off, -1, "", `field "on_empty" has unknown value %q%s; legal values are %s`, val.str, hint(val.str, onEmptyValues), list(onEmptyValues))
	default:
		p.OnEmpty = val.str
	}
}

// maxPathsChanged validates R-25's ceiling at LOAD time, so a policy carrying
// a nonsense one fails on repo 0 rather than on whichever repo first exceeds
// it. Zero is legal and means "this wave may change no ownership at all",
// which is a coherent thing to assert about a policy of pure `declare` ops;
// negative is not, because -1 is how an absent ceiling is spelled internally
// and the two must never be confusable.
func (v *validator) maxPathsChanged(p *Policy, m *member) {
	if m == nil {
		return
	}
	val := m.val
	if val.kind != kNumber {
		v.at(val.off, -1, "", `field "max_paths_changed" must be a number of paths, got %s`, val.describe())
		return
	}
	n, err := strconv.Atoi(val.num.String())
	switch {
	case err != nil:
		v.at(val.off, -1, "", `field "max_paths_changed" must be a whole number, got %s`, val.raw())
	case n < 0:
		v.at(val.off, -1, "", `field "max_paths_changed" must be zero or positive, got %s; omit the field to set no ceiling`, val.raw())
	default:
		p.MaxPathsChanged = n
	}
}

// create validates R-34's boolean. A wrong type is a policy error like any
// other: a baseline whose `create` is the STRING "false" would otherwise turn a
// fleet-wide "off" into whatever a non-empty string coerces to, and the message
// has to name the TYPE — "unknown field" and "wrong type" send the operator to
// different edits.
func (v *validator) create(p *Policy, m *member) {
	if m == nil {
		return
	}
	val := m.val
	if val.kind != kBool {
		v.at(val.off, -1, "", `field "create" must be a boolean, got %s; write true or false — whether a repository without a CODEOWNERS gets one is a decision the reviewed artifact makes (R-34)`, val.describe())
		return
	}
	p.Create = val.b
}

// defaults validates R-35's block: the per-op settings a policy states once so
// a 40-op baseline is 40 strings rather than 40 objects.
//
// An empty block is LEGAL and states nothing — deliberately not the treatment
// `"on_zero_match": ""` gets. The strict-parse rule bites where a spelling
// states a decision that was never made; `{}` states no default for anything,
// and rejecting it would fail every repo for a generator that emits the key
// unconditionally and fills it only sometimes.
func (v *validator) defaults(p *Policy, m *member) {
	if m == nil {
		return
	}
	val := m.val
	if val.kind != kObject {
		v.at(val.off, -1, "", `field "defaults" must be an object holding the per-op settings this policy states once, like {"on_zero_match": "skip"}, got %s`, val.describe())
		return
	}
	var zeroM, unownedM *member
	for _, f := range val.members {
		if ignoredKey(f.key) {
			continue
		}
		switch f.key {
		case "on_zero_match":
			if s, ok := v.zeroMatch(f, -1, ""); ok {
				p.Defaults.OnZeroMatch = s
				zeroM = f
			}
		case "on_except_zero_match":
			if s, ok := v.exceptZeroMatch(f, -1, ""); ok {
				p.Defaults.OnExceptZeroMatch = s
			}
		case "on_unowned":
			if s, ok := v.unowned(f, -1, ""); ok {
				p.Defaults.OnUnowned = s
				unownedM = f
			}
		default:
			v.unknownDefaultsField(f)
		}
	}
	// R-40: declare and skip defaulted TOGETHER is refused. For any op stating
	// neither field the two contradict — declare pre-owns files that do not
	// exist yet, skip declines to own even the ones that do — and which one
	// silently won would be a decision nobody reviewed. Repo-independent, so
	// exit 3, once, at repo 0.
	if zeroM != nil && unownedM != nil &&
		p.Defaults.OnZeroMatch == ops.ZeroMatchDeclare && p.Defaults.OnUnowned == ops.UnownedSkip {
		v.at(unownedM.val.off, -1, "", `"defaults" cannot state both on_zero_match %q and on_unowned %q: for any op stating neither field the two contradict — declare writes a rule for files that do not exist yet, which are unowned by definition, while skip declines to own even the unowned files that do exist (R-40) — state one of them per op instead`,
			ops.ZeroMatchDeclare, ops.UnownedSkip)
	}
}

// unknownDefaultsField names the offending key AND the set the block accepts.
// A typo'd default is not "no default", it is the WRONG default applied to
// every op in the policy at once, so this is the higher-stakes place to get an
// enum or a field name wrong — it cannot be the place with the weaker message.
func (v *validator) unknownDefaultsField(m *member) {
	if m.key == "on_empty" {
		// R-35c. "on_empty" is a real top-level field, so the message must say
		// where it belongs rather than merely that it is unknown here.
		v.at(m.keyOff, -1, "", `field "on_empty" is not a per-op default: it is one policy for the whole run and stays at the TOP level of the file, and two spellings of one setting is the ambiguity this format exists to remove (R-35c) — "defaults" accepts %s`,
			list(defaultsFields))
		return
	}
	if contains(opFields, m.key) {
		v.at(m.keyOff, -1, "", `unknown field %q in "defaults": %q is an op field, and only settings that are genuinely per-op can be defaulted; "defaults" accepts %s`,
			m.key, m.key, list(defaultsFields))
		return
	}
	v.at(m.keyOff, -1, "", `unknown field %q in "defaults"%s; "defaults" accepts %s`, m.key, hint(m.key, defaultsFields), list(defaultsFields))
}

// lint validates R-36's block, which lets the repair policy be reviewed in the
// same committed artifact as the ownership policy. `sync` does not act on it
// and validates it anyway (R-36e).
func (v *validator) lint(p *Policy, m *member) {
	if m == nil {
		return
	}
	val := m.val
	if val.kind != kObject {
		v.at(val.off, -1, "", `field "lint" must be an object holding lint's preferences, like {"remove_stale_paths": true}, got %s`, val.describe())
		return
	}
	for _, f := range val.members {
		if ignoredKey(f.key) {
			continue
		}
		switch f.key {
		case "remove_stale_paths":
			if f.val.kind != kBool {
				v.at(f.val.off, -1, "", `field "remove_stale_paths" in the "lint" block must be a boolean, got %s; write true or false — deleting rules whose pattern matches nothing is opt-in (R-11)`, f.val.describe())
				continue
			}
			p.Lint.RemoveStalePaths = f.val.b
		case "on_empty":
			p.Lint.OnEmpty = v.lintOnEmpty(f)
		default:
			v.unknownLintField(f)
		}
	}
}

// lintOnEmpty is R-6's question asked of the lint block. Same enum and same
// present-but-empty refusal as the top-level field: an ABSENT value leaves
// lint with no policy (and it refuses to guess), while "" states no decision
// at all while reading to a reviewer as though one was made.
func (v *validator) lintOnEmpty(m *member) string {
	val := m.val
	switch {
	case val.kind != kString:
		v.at(val.off, -1, "", `field "on_empty" in the "lint" block must be a string, got %s; legal values are %s`, val.describe(), list(onEmptyValues))
	case val.str == "":
		v.at(val.off, -1, "", `field "on_empty" in the "lint" block is present but empty; omitting it leaves lint with no R-6 policy, while "" states no decision at all — legal values are %s`, list(onEmptyValues))
	case !contains(onEmptyValues, val.str):
		v.at(val.off, -1, "", `field "on_empty" in the "lint" block has unknown value %q%s; legal values are %s`, val.str, hint(val.str, onEmptyValues), list(onEmptyValues))
	default:
		return val.str
	}
	return ""
}

func (v *validator) unknownLintField(m *member) {
	if contains(topFields, m.key) || contains(opFields, m.key) {
		v.at(m.keyOff, -1, "", `unknown field %q in "lint": %q belongs to the ownership half of the policy, and the field sets are per level rather than one merged bag; "lint" accepts %s`,
			m.key, m.key, list(lintFields))
		return
	}
	v.at(m.keyOff, -1, "", `unknown field %q in "lint"%s; "lint" accepts %s`, m.key, hint(m.key, lintFields), list(lintFields))
}

func (v *validator) opsArray(p *Policy, m *member, rootOff int) []opInfo {
	if m == nil {
		// True of whichever command is running. `lint --policy` reads the same
		// artifact, and a file carrying only a `lint` block fully specifies a
		// lint run — so a refusal claiming the file "does nothing" was simply
		// false there, and sent its author to add a dummy op that `sync` then
		// applies (adversarial-review finding). What is true for every caller
		// is the pairing: one file, one verdict (R-36c/R-36e).
		v.at(rootOff, -1, "", `missing required field "ops"; one policy file is read by every command, so it has to state the ownership policy the rest of it is paired with (R-36c) — without "ops", `+"`sync --policy`"+` would apply nothing to every repository and exit 0 on all of them, the silent success this format exists to make impossible. A file whose only intent is a `+"`lint`"+` block has no separate spelling: state the ops the repair rides with`)
		return nil
	}
	val := m.val
	switch {
	case val.kind != kArray:
		// A single op string here is the plausible generator mistake, and reading
		// it as one op would be a helpful guess — the exact behavior a strict
		// format forbids.
		v.at(val.off, -1, "", `field "ops" must be an array, got %s; even one op is written as a one-element array`, val.describe())
		return nil
	case len(val.elems) == 0:
		v.at(val.off, -1, "", `field "ops" is empty; an empty array is what a generator emits when its query returned nothing, and it would report success on every repo having changed none of them`)
		return nil
	}
	infos := make([]opInfo, 0, len(val.elems))
	for i, e := range val.elems {
		infos = append(infos, v.op(p, i, e))
	}
	return infos
}

// op validates one entry of the ops array.
//
// A bare string is SHORTHAND for {"op": "<string>"} with every other field at its
// default — not a second form with its own code path. Both land in the same
// ops.Parse call and the same defaults below; if they ever diverged, a reviewer
// reading a mixed ops array could not tell what runs.
func (v *validator) op(p *Policy, i int, e *jsonValue) opInfo {
	info := opInfo{index: i, off: e.off, idOff: e.off}

	spec, specOff := "", e.off
	var zeroM, exceptM, unownedM, exceptArrM, ownersM *member
	note := ""

	switch e.kind {
	case kString:
		spec = e.str
	case kObject:
		// `id` is resolved before anything else so every other error about this
		// op can name it. An op with no id keeps ID "": `ops[N]` is a display
		// label the renderer computes from the position, and storing it would
		// make an unnamed op indistinguishable from one deliberately named
		// "ops[0]", keyed by a name that shifts the moment somebody inserts an op
		// above it.
		if m := e.field("id"); m != nil {
			info.idOff = m.val.off
			info.id = v.optString(m, "id", i, "")
		}
		var opM *member
		for _, m := range e.members {
			if ignoredKey(m.key) {
				continue
			}
			switch m.key {
			case "op":
				opM = m
			case "id":
				// handled above, before the loop
			case "on_zero_match":
				zeroM = m
			case "on_except_zero_match":
				exceptM = m
			case "on_unowned":
				unownedM = m
			case "except":
				exceptArrM = m
			case "owners":
				ownersM = m
			case "note":
				note = v.optString(m, "note", i, info.id)
			default:
				v.unknownOpField(m, i, info.id)
			}
		}
		if opM == nil {
			v.at(e.off, i, info.id, `missing required field "op"; an object carrying only id and note describes nothing, and would put a phantom entry in every repo's per-op results`)
			return info
		}
		if opM.val.kind != kString {
			v.at(opM.val.off, i, info.id, `field "op" must be a string holding one op, like "add_owner(/x/, @a)", got %s`, opM.val.describe())
			return info
		}
		spec, specOff = opM.val.str, opM.val.off
	default:
		// Dispatch is on the JSON type and nothing else. A number or null read as
		// a zero-value op would run an empty op against the whole fleet.
		v.at(e.off, i, "", `an op must be a string like "add_owner(/x/, @a)" or an object like {"op": "add_owner(/x/, @a)"}, got %s`, e.describe())
		return info
	}

	zero, zeroOK := "", true
	if zeroM != nil {
		zero, zeroOK = v.zeroMatch(zeroM, i, info.id)
	}
	exceptZero, exceptZeroOK := "", true
	if exceptM != nil {
		exceptZero, exceptZeroOK = v.exceptZeroMatch(exceptM, i, info.id)
	}
	unowned, unownedOK := "", true
	if unownedM != nil {
		unowned, unownedOK = v.unowned(unownedM, i, info.id)
	}

	// R-39: the `owners` array is folded into the op string BEFORE parsing.
	// `except` can be folded after, because an op string parses without its
	// clause; an op naming a scope and no owners does not parse at all, so the
	// array has to be there by the time ops.Parse sees the string.
	if ownersM != nil {
		spelled, ok := v.ownersArray(spec, specOff, ownersM, i, info.id)
		if !ok {
			return info
		}
		spec = spelled
	}

	parsed, err := ops.Parse(spec)
	if err != nil {
		// Letting ops.Parse's own text out would lose the file, the line, and the
		// op index: the operator gets `unknown op "add_ownr"` with no idea which
		// of 40 ops in which of 100 repos.
		if ownersM != nil {
			// The op string quoted here is the one the array was folded into,
			// which is not the text in the file. Saying so is the difference
			// between an operator finding the defect and hunting for a list
			// they never wrote.
			v.at(specOff, i, info.id, `op %q is not valid: %v (R-39a: an "owners" array is validated exactly as the (scope, [owners]) spelling it is equivalent to, shown here)`, spec, err)
			return info
		}
		v.at(specOff, i, info.id, `op %q is not valid: %v`, spec, err)
		return info
	}
	info.kind, info.parsed = parsed.Kind, true

	// Before every legality check below: the array spelling is the same fact
	// as the string one (R-37a), so R-30 and R-27.6 must see the excepts it
	// carries.
	exceptArrOK := true
	if exceptArrM != nil {
		exceptArrOK = v.exceptArray(&parsed, exceptArrM, i, info.id)
	}

	if zeroM != nil {
		v.checkZeroMatchLegality(zeroM, parsed.Kind, zero, i, info.id)
	}
	v.checkExceptLegality(zeroM, exceptM, parsed, zero, exceptArrOK, i, info.id)
	if unownedM != nil && unownedOK {
		v.checkUnownedLegality(unownedM, parsed.Kind, unowned, zero, i, info.id)
	}

	// R-35a/R-35e: the block fills in only what this op did not state, and only
	// where this op can carry it. The two R-40 exclusions are symmetric: a
	// defaulted declare must not land beside an explicit on_unowned=skip, and a
	// defaulted skip must not land beside an explicit declare — either pairing
	// is the contradiction checkUnownedLegality refuses when both are spelled
	// out, and a default is applied only where the op can carry it.
	if zeroM == nil && p.Defaults.OnZeroMatch != "" && zeroMatchReaches(parsed, p.Defaults.OnZeroMatch) &&
		!(p.Defaults.OnZeroMatch == ops.ZeroMatchDeclare && unowned == ops.UnownedSkip) {
		zero = p.Defaults.OnZeroMatch
	}
	if exceptM == nil && p.Defaults.OnExceptZeroMatch != "" && len(parsed.Except) > 0 {
		exceptZero = p.Defaults.OnExceptZeroMatch
	}
	if unownedM == nil && p.Defaults.OnUnowned != "" &&
		parsed.Kind == ops.AddOwner && zero != ops.ZeroMatchDeclare {
		unowned = p.Defaults.OnUnowned
	}

	parsed.ID = info.id
	if zeroOK {
		parsed.OnZeroMatch = zero
	}
	if exceptZeroOK {
		parsed.OnExceptZeroMatch = exceptZero
	}
	if unownedOK {
		parsed.OnUnowned = unowned
	}
	p.Ops = append(p.Ops, parsed)

	if note != "" {
		if p.Notes == nil {
			p.Notes = make(map[string]string)
		}
		// Notes are keyed by the same label the renderer computes, so a note
		// never goes missing just because nobody named its op.
		p.Notes[OpLabel(info.id, i)] = note
	}
	return info
}

// ownersArray decodes R-39's `owners` array by re-spelling the op string with
// the bracketed list it is equivalent to, and hands that string back for
// ops.Parse to validate.
//
// Re-spelling rather than decoding straight into Op.Owners is the point, and it
// is the same argument R-37a made for `except`: every refusal the list grammar
// already makes — a duplicate owner, a case-variant of one already named, an
// invalid token, an empty list on a verb that cannot mean anything by it —
// applies to the array for free and in the same words. A second copy of those
// checks beside the array is how the two spellings drift: the array would keep
// accepting `["@org/a", "@org/a"]` a year after the list stopped, write it, and
// leave `verify` reporting a rollback-worthy invariant violation over what
// `check` called a clean policy.
//
// The refusals written out here are the ones the re-spelling cannot state for
// itself, each about the array as a JSON value rather than about the owners it
// names. Where the OP STRING is what is wrong, the grammar's own diagnosis is
// reported unchanged: an array riding on a typo must not rename that typo.
func (v *validator) ownersArray(spec string, specOff int, m *member, i int, id string) (string, bool) {
	val := m.val
	if ops.SpelledKind(spec) == ops.RenameOwner {
		// R-39c, the R-33f/R-27.4 reasoning one level up: falling through to
		// "rename_owner takes no list" would name a construct the author did
		// not write, and send them hunting a typo in their op string instead
		// of learning the verb takes no array.
		v.at(val.off, i, id, `rename_owner takes no "owners" array: it renames one owner to one owner, so there is no set for the array to state (R-39c)`)
		return "", false
	}
	if ops.NamesOwners(spec) {
		v.at(val.off, i, id, `this op names owners in its op string AND in an "owners" array; one intent, one place (R-39b) — keep either the `+"`(scope, [owners])`"+` spelling or the array, not both`)
		return "", false
	}
	if val.kind != kArray {
		v.at(val.off, i, id, `field "owners" must be an array of owner tokens like ["@org/team"], got %s (R-39a)`, val.describe())
		return "", false
	}
	// An empty array is deliberately NOT refused here, unlike R-37d's empty
	// `except`: emptiness states an intent for one verb and a defect for the
	// others. `set_owners(scope, [])` is how "nobody owns this" is spelled,
	// while an empty add or remove states nothing at all — a distinction the
	// list grammar already draws (R-33d), and drawing it a second time here is
	// how the two would come to disagree.
	owners := make([]string, 0, len(val.elems))
	for n, el := range val.elems {
		if el.kind != kString {
			v.at(el.off, i, id, `field "owners" element %d must be a string holding one owner, like "@org/team", got %s (R-39a)`, n, el.describe())
			return "", false
		}
		if el.str == "" {
			v.at(el.off, i, id, `field "owners" element %d is empty; every owner names an identity, and "" names none (R-39a)`, n)
			return "", false
		}
		if bad := uncarryableOwnerChars(el.str); bad != "" {
			v.at(el.off, i, id, `field "owners" element %d cannot be carried by an op string: %q contains %s, and an op is one string — a comma separates its arguments, brackets bound the owner list, and whitespace separates one owner from the next — so this text would be read as more than one owner rather than as one identity (R-39d). Only an email owner can reach this; a GitHub @handle cannot contain any of them`,
				n, el.str, bad)
			return "", false
		}
		owners = append(owners, el.str)
	}
	spelled, ok := ops.WithOwners(spec, owners)
	if !ok {
		// Every element carries on its own, so what cannot take the list is the
		// op string. Two shapes reach here — text that is not an op at all
		// (`add_owner(/x/`, ``) and an op whose argument list is empty
		// (`add_owner()`) — and the grammar diagnoses both, in the same words
		// the same typo gets when no array is present. Writing a sentence here
		// instead is how `add_owner(/x/` came to be reported as an op that
		// "has to state a scope", which is the one thing it does state: what it
		// is missing is a closing paren, and an operator told otherwise goes
		// looking at the scope (review finding). An `owners` array must never
		// change what a syntax error is called.
		if _, err := ops.Parse(spec); err != nil {
			v.at(specOff, i, id, `op %q is not valid: %v`, spec, err)
			return "", false
		}
		// Unreachable through the grammar as it stands: an op string Parse
		// accepts states two arguments, and R-39b has already refused that
		// pairing above. A verb with a one-argument form would land here, and
		// waving the array through would attach it to nothing.
		v.at(val.off, i, id, `op %q cannot carry an "owners" array (R-39a)`, spec)
		return "", false
	}
	return spelled, true
}

// uncarryableOwnerChars names the structural characters an owner token holds,
// for the refusal above. Only an email owner can reach it: handleRe admits
// none of these, while emailRe is `[^@\s]+@[^@\s]+\.[^@\s]+` and admits a
// comma, a bracket and everything else that is not whitespace or a second @.
//
// It returns "" when the token is carryable, so the caller reads it as a test.
func uncarryableOwnerChars(owner string) string {
	var names []string
	if strings.Contains(owner, ",") {
		names = append(names, "a comma")
	}
	if strings.ContainsAny(owner, "[]") {
		names = append(names, "a bracket")
	}
	if strings.ContainsAny(owner, " \t") {
		names = append(names, "whitespace")
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// exceptArray decodes R-37's `except` array onto an op that has already
// parsed, and then routes it through the STRING spelling's own validation by
// re-spelling the op with the clause attached and re-parsing it.
//
// Re-parsing rather than re-checking is the point: R-37a promises the two
// spellings are equivalent in every respect, and a second copy of the R-27
// checks here is exactly how the two would drift — the array would keep
// accepting a duplicate pattern a year after the string form stopped. The op
// re-spelled is the REAL one, not a stand-in: it is what the op then reports
// itself as (R-37e), and rename_owner still comes back with "rename_owner
// takes no scope" because the clause lands on the argument it does have
// (R-27.4).
//
// The three refusals before that are the ones the round trip cannot state for
// itself, each an adversarial-review finding: an element escaped for a
// delimiter the array does not have, an element the op string cannot carry at
// all, and an element whose trailing backslash would swallow the next one.
// Each is refused in the operator's terms, because parsing the mangled string
// instead produces a message about a construct nobody wrote.
func (v *validator) exceptArray(parsed *ops.Op, m *member, i int, id string) (ok bool) {
	val := m.val
	if len(parsed.Except) > 0 {
		v.at(val.off, i, id, `this op carries an except clause in its op string AND an "except" array; one intent, one place (R-37b) — keep either the `+"`<scope> except <pat> …`"+` spelling or the array, not both`)
		return false
	}
	if val.kind != kArray {
		v.at(val.off, i, id, `field "except" must be an array of patterns like ["/x/gen/"], got %s (R-37a)`, val.describe())
		return false
	}
	if len(val.elems) == 0 {
		v.at(val.off, i, id, `field "except" is an empty array; state no "except" at all rather than an empty one (R-37d) — a generator emitting [] when it has nothing to carve is asking for the broad grant with no carve-out`)
		return false
	}
	pats := make([]string, 0, len(val.elems))
	for n, el := range val.elems {
		if el.kind != kString {
			v.at(el.off, i, id, `field "except" element %d must be a string, got %s`, n, el.describe())
			return false
		}
		// "" and not TrimSpace: an element holding a space is not empty, and
		// the string spelling accepts `except \ ` — one input landing in two
		// exit classes depending on spelling is the asymmetry R-37a forbids.
		if el.str == "" {
			v.at(el.off, i, id, `field "except" element %d is empty; every except pattern names paths, and "" names none (R-37)`, n)
			return false
		}
		if preEscaped(el.str) {
			v.at(el.off, i, id, `field "except" element %d is already escaped for a delimiter this array does not have: %q (R-37c). The array is not delimited, so an element IS the path — write it literally, as %q. Escaped again it names a different path: a backslash is the pattern language's own escape, so this text carries a literal backslash followed by a bare space, and the bare space then splits one pattern into two`,
				n, el.str, literalExceptPattern(el.str))
			return false
		}
		pat := escapeExceptPattern(el.str)
		if attempt, ok := ops.WithExcept(parsed.Raw, []string{pat}); !ok {
			if named := uncarryableChars(el.str); named != "" {
				v.at(el.off, i, id, `field "except" element %d cannot be carried by an op string: %q contains %s, and an op is one string — a comma separates its arguments and brackets bound its owner list — so this pattern would be read as part of another argument rather than as a path. The `+"`<scope> except <pat> …`"+` spelling cannot express this path either (R-37c); carve a directory that contains it instead`,
					n, el.str, named)
				return false
			}
			// Nothing structural to name, so what is wrong is the CLAUSE this
			// element makes — an element ending in a backslash escapes the
			// comma that ends the scope argument and swallows the rest of the
			// op. Report the grammar's refusal of that string unchanged: it is
			// the same sentence the `<scope> except <pat> …` spelling gets for
			// the same text, which is what R-37e promises.
			if attempt != "" {
				if _, err := ops.Parse(attempt); err != nil {
					v.at(el.off, i, id, `the "except" array is not valid: %v (R-37a: the array is validated exactly as the `+"`<scope> except <pat> …`"+` spelling it is equivalent to)`, err)
					return false
				}
			}
			v.at(el.off, i, id, `field "except" element %d cannot be carried by an op string: %q does not survive being spelled as `+"`<scope> except <pat> …`"+` (R-37c); carve a directory that contains it instead`, n, el.str)
			return false
		}
		pats = append(pats, pat)
	}
	spelled, ok := ops.WithExcept(parsed.Raw, pats)
	if !ok {
		// Every element carries on its own, so what broke is the JOIN: a
		// trailing backslash escapes the space in front of the next pattern
		// and the two are read as one — a pattern for a path that exists
		// nowhere, carving neither of the two the operator named.
		n, el := danglingElement(val.elems)
		if el == "" {
			v.at(val.off, i, id, `the "except" array cannot be written as one clause; the patterns do not survive being spelled as `+"`<scope> except <pat> …`"+` (R-37c)`)
			return false
		}
		v.at(val.elems[n].off, i, id, `field "except" element %d ends with a backslash: %q. The clause is one string, so that backslash escapes the space separating this pattern from the next and the two would be read as a single pattern (R-37c: a backslash is the pattern language's own escape) — drop it, or escape the backslash itself`,
			n, el)
		return false
	}
	re, err := ops.Parse(spelled)
	if err != nil {
		v.at(val.off, i, id, `the "except" array is not valid: %v (R-37a: the array is validated exactly as the `+"`<scope> except <pat> …`"+` spelling it is equivalent to)`, err)
		return false
	}
	parsed.Except = re.Except
	// R-37e: the op reports itself as the intent that was REVIEWED. Every
	// report echoes Raw — the R-8 remedy, the sync record's ops[].op, the
	// --summary-out table, plan.Plan.Ops — and an echo without the carve told
	// one operator to run a set_owners that displaces the very subtree the
	// carve protected.
	parsed.Raw = re.Raw
	return true
}

// preEscaped reports whether s escapes whitespace for a delimiter the array
// does not have: a space or tab preceded by an ODD number of backslashes.
//
// Parity, not "contains a backslash". `my\ dir/` is an escape of the space and
// so is pre-escaped, while `my\\ dir/` is an escaped backslash followed by a
// REAL space — a path whose name contains a backslash, legitimately spelled,
// and a different path.
func preEscaped(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			continue
		}
		bs := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			bs++
		}
		if bs%2 == 1 {
			return true
		}
	}
	return false
}

// literalExceptPattern removes the delimiter escaping from a pre-escaped
// element, so the refusal can show the spelling its author meant. Exactly one
// backslash comes off each odd run, which is what makes `my\\\ dir/` suggest
// `my\\ dir/` — the path with a backslash in its name — rather than dropping
// the name's own character.
func literalExceptPattern(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == ' ' || s[i+1] == '\t') {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// uncarryableChars names the characters that cost a pattern its round trip, so
// the refusal can say which one the operator has to deal with. It is asked
// only after the round trip has already failed: a comma inside a balanced
// character class survives, and R-37c keeps `[` live pattern syntax.
func uncarryableChars(pat string) string {
	var names []string
	if strings.Contains(pat, ",") {
		names = append(names, "a comma")
	}
	if strings.ContainsAny(pat, "[]") {
		names = append(names, "a bracket")
	}
	switch len(names) {
	case 0:
		// "" and not a generic phrase: a message that names no character is no
		// diagnosis, and the caller has a better one to fall back to.
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// danglingElement finds the first element whose trailing backslash run is odd,
// which is the one that swallows the pattern after it.
func danglingElement(elems []*jsonValue) (int, string) {
	for n, el := range elems {
		bs := 0
		for j := len(el.str) - 1; j >= 0 && el.str[j] == '\\'; j-- {
			bs++
		}
		if bs%2 == 1 {
			return n, el.str
		}
	}
	return 0, ""
}

// escapeExceptPattern converts one array element into the pattern text the
// string grammar would carry.
//
// The array drops the DELIMITER, and with it only the escaping the delimiter
// forced: `"my dir/"` is one pattern (R-37c). Everything else is still pattern
// syntax — `*`, `?` and `[` keep their meaning, or the array would be a list of
// literal paths rather than the same patterns spelled without a separator.
//
// Escaping every space is correct only because preEscaped has already refused
// an element that escaped one itself: adding a backslash to an odd run turns
// an escaped space into a literal backslash and a BARE space, which the string
// grammar splits into two patterns.
func escapeExceptPattern(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// zeroMatchReaches is R-35e for on_zero_match: a default is applied only to ops
// that CAN carry it, so a baseline stating one value and happening to contain
// one rename is not rejected on every repo — with the operator's only route
// back being to expand the block into 40 per-op values. The conditions are the
// legality table itself (checkZeroMatchLegality and R-30), read as a question
// rather than as a refusal.
func zeroMatchReaches(op ops.Op, val string) bool {
	switch {
	case op.Kind == ops.RenameOwner:
		return false
	case val == ops.ZeroMatchDeclare && op.Kind == ops.RemoveOwner:
		return false
	case val == ops.ZeroMatchDeclare && len(op.Except) > 0:
		return false
	}
	return true
}

// zeroMatch validates the R-21 enum. It reports whether the value is usable, so
// a rejected value is never written onto the op — an op that carried "SKIP"
// through to the planner would run the default under a spelling that says
// otherwise.
func (v *validator) zeroMatch(m *member, i int, id string) (string, bool) {
	val := m.val
	switch {
	case val.kind != kString:
		v.at(val.off, i, id, `field "on_zero_match" must be a string, got %s; legal values are %s`, val.describe(), list(zeroMatchValues))
	case val.str == "":
		// An ABSENT on_zero_match means require; a PRESENT and empty one is an
		// error. A generator that emitted "" where it meant to emit a decision
		// produced a file that reads, to a reviewer, as though a choice was made
		// — accepting it applies the default across the fleet under a spelling
		// that says otherwise.
		v.at(val.off, i, id, `field "on_zero_match" is present but empty; omitting the field means %q, while "" states no decision at all — legal values are %s`,
			ops.ZeroMatchRequire, list(zeroMatchValues))
	case !contains(zeroMatchValues, val.str):
		v.at(val.off, i, id, `field "on_zero_match" has unknown value %q%s; legal values are %s`,
			val.str, zeroMatchHint(val.str), list(zeroMatchValues))
	default:
		return val.str, true
	}
	return "", false
}

// checkZeroMatchLegality enforces the legality table: whether an op may carry
// on_zero_match at all depends only on the op KIND, which is a repo-independent
// fact and therefore a policy error — caught on repo 0 rather than repo 47.
// Accepting the field and ignoring it is the same class of failure as the typo.
func (v *validator) checkZeroMatchLegality(m *member, kind ops.Kind, zero string, i int, id string) {
	switch kind {
	case ops.RenameOwner:
		// rename_owner's scope comes from current ownership rather than a
		// pattern, so plan.go exempts it from R-5 entirely and zero-match can
		// never fire on it.
		v.at(m.val.off, i, id, `field "on_zero_match" is not meaningful on rename_owner: its scope is derived from current ownership rather than from a pattern, so it can never match zero files — remove the field`)
	case ops.RemoveOwner:
		if zero == ops.ZeroMatchDeclare {
			v.at(m.val.off, i, id, `"declare" is not meaningful on remove_owner: declare exists to write a rule for files that do not exist yet, and there is no rule that declares the absence of an owner — use %q or %q`,
				ops.ZeroMatchRequire, ops.ZeroMatchSkip)
		}
	}
}

// exceptZeroMatch validates the R-28 enum. Like zeroMatch, it reports whether
// the value is usable, so a rejected value is never written onto the op.
//
// The legal pair is rendered as `"require" or "allow"` rather than through
// list(): there are exactly two values and the default comes first, and the
// message doubles as the documentation an operator sees mid-rollout.
func (v *validator) exceptZeroMatch(m *member, i int, id string) (string, bool) {
	val := m.val
	switch {
	case val.kind != kString:
		v.at(val.off, i, id, `field "on_except_zero_match" must be a string, got %s; legal values are %q or %q`,
			val.describe(), ops.ExceptZeroMatchRequire, ops.ExceptZeroMatchAllow)
	case val.str == "":
		// Same rule as on_zero_match: an ABSENT field means require, a PRESENT
		// and empty one states no decision while reading to a reviewer as
		// though a choice was made.
		v.at(val.off, i, id, `field "on_except_zero_match" is present but empty; omitting the field means %q, while "" states no decision at all — legal values are %q or %q`,
			ops.ExceptZeroMatchRequire, ops.ExceptZeroMatchRequire, ops.ExceptZeroMatchAllow)
	case !contains(exceptZeroMatchValues, val.str):
		v.at(val.off, i, id, `field "on_except_zero_match" has unknown value %q%s; legal values are %q or %q`,
			val.str, hint(val.str, exceptZeroMatchValues), ops.ExceptZeroMatchRequire, ops.ExceptZeroMatchAllow)
	default:
		return val.str, true
	}
	return "", false
}

// unowned validates the R-40 enum. Like zeroMatch, it reports whether the
// value is usable, so a rejected value is never written onto the op — an op
// carrying "SKIP" through to the planner would grant every open path under a
// spelling that says otherwise.
func (v *validator) unowned(m *member, i int, id string) (string, bool) {
	val := m.val
	switch {
	case val.kind != kString:
		v.at(val.off, i, id, `field "on_unowned" must be a string, got %s; legal values are %q or %q`,
			val.describe(), ops.UnownedAssign, ops.UnownedSkip)
	case val.str == "":
		// Same rule as on_zero_match: an ABSENT field means assign, a PRESENT
		// and empty one states no decision while reading to a reviewer as
		// though a choice was made.
		v.at(val.off, i, id, `field "on_unowned" is present but empty; omitting the field means %q, while "" states no decision at all — legal values are %q or %q`,
			ops.UnownedAssign, ops.UnownedAssign, ops.UnownedSkip)
	case !contains(unownedValues, val.str):
		v.at(val.off, i, id, `field "on_unowned" has unknown value %q%s; legal values are %q or %q`,
			val.str, hint(val.str, unownedValues), ops.UnownedAssign, ops.UnownedSkip)
	default:
		return val.str, true
	}
	return "", false
}

// checkUnownedLegality enforces R-40's legality table, which depends only on
// the op KIND and its zero-match policy — repo-independent facts, caught on
// repo 0. The field is add_owner's alone: remove_owner cannot touch an open
// path in the first place, set_owners displaces owners by design (a "only
// where owned" set is a different intent, spelled as add_owner), and
// rename_owner has no scope. skip beside declare is the R-30-shaped
// contradiction: a declared rule exists to own files that do not exist yet,
// which are unowned by definition.
func (v *validator) checkUnownedLegality(m *member, kind ops.Kind, unowned, zero string, i int, id string) {
	switch kind {
	case ops.RemoveOwner:
		v.at(m.val.off, i, id, `field "on_unowned" is not meaningful on remove_owner: a path with no owner has nothing to remove, so the op already leaves open paths open — remove the field (R-40)`)
	case ops.SetOwners:
		v.at(m.val.off, i, id, `field "on_unowned" is not meaningful on set_owners: it REPLACES the owners of every path in scope by design, and "only where owned" is a different intent — spell it as add_owner (R-40)`)
	case ops.RenameOwner:
		v.at(m.val.off, i, id, `field "on_unowned" is not meaningful on rename_owner: its scope is derived from current ownership rather than from a pattern, so it can never reach an unowned path — remove the field (R-40)`)
	case ops.AddOwner:
		if unowned == ops.UnownedSkip && zero == ops.ZeroMatchDeclare {
			v.at(m.val.off, i, id, `field "on_unowned" %q cannot be combined with on_zero_match %q: a declared rule exists to own files that do not exist yet, which are unowned by definition, so the two state opposite intents about the same paths (R-40) — drop one of them`,
				ops.UnownedSkip, ops.ZeroMatchDeclare)
		}
	}
}

// checkExceptLegality enforces the two R-27 rules that need the PARSED op next
// to the policy fields — both repo-independent, both caught on repo 0:
//
//   - R-30: on_zero_match=declare cannot ride on an op with an except clause. A
//     declared rule is one literal CODEOWNERS line and CODEOWNERS has no
//     negation (S-2), so the moment a file matching both scope and except comes
//     into existence the declared line governs it — the except would be a
//     comment, not a constraint.
//   - R-27.6: on_except_zero_match on an op with no except clause. The field
//     governs an except pattern that matched nothing; on an op without one it
//     can never apply, and accepting-and-ignoring it is the same class of
//     failure as a typo'd field name.
func (v *validator) checkExceptLegality(zeroM, exceptM *member, parsed ops.Op, zero string, exceptArrOK bool, i int, id string) {
	if zeroM != nil && zero == ops.ZeroMatchDeclare && len(parsed.Except) > 0 {
		v.at(zeroM.val.off, i, id, `on_zero_match %q cannot be combined with an except clause: a declared rule is one literal CODEOWNERS line, and CODEOWNERS has no negation (S-2), so the line cannot encode subtraction (R-30) — split the carve into its own op or drop the declare`,
			ops.ZeroMatchDeclare)
	}
	// exceptArrOK: an op whose `except` array was refused has no parsed carve,
	// and reporting on_except_zero_match as inapplicable there tells the
	// operator to remove the one field that is not the defect.
	if exceptM != nil && exceptArrOK && len(parsed.Except) == 0 {
		v.at(exceptM.val.off, i, id, `field "on_except_zero_match" is set, but this op has no except clause, so the field can never apply — remove it, or add `+"`except <pat>`"+` to the scope (R-27)`)
	}
}

// checkDuplicateIDs rejects two ops sharing an id. Per-op results are keyed by
// id — the summary a reviewer reads, and whatever a fleet script pipes into jq —
// so a repeated id silently overwrites one op's outcome with another's, and the
// reviewer sees a policy that ran N ops report N-1 results.
func (v *validator) checkDuplicateIDs(infos []opInfo) {
	firstAt := make(map[string]int, len(infos))
	for _, info := range infos {
		if info.id == "" {
			// The empty id is the ABSENCE of a name, not a name two ops share;
			// unnamed ops are referred to by position and never collide.
			continue
		}
		if prev, dup := firstAt[info.id]; dup {
			v.at(info.idOff, info.index, info.id, `duplicate op id %q, already used by ops[%d]; per-op results are keyed by id, so one op's outcome would silently overwrite the other's`, info.id, prev)
			continue
		}
		firstAt[info.id] = info.index
	}
}

// checkOnEmptyRequired makes R-6's question a load-time one. Without it, "what
// happens when a removal empties an owner set?" is answered lazily on whichever
// repo first hits it — and a policy with no removals must not be forced to
// answer a question it never asks.
func (v *validator) checkOnEmptyRequired(present bool, infos []opInfo) {
	if present {
		return
	}
	for _, info := range infos {
		if info.parsed && info.kind == ops.RemoveOwner {
			v.at(info.off, info.index, info.id, `this op is a remove_owner, so the policy must set a top-level "on_empty" (%s); leaving it unset settles R-6 lazily, on whichever repo first has a removal empty an owner set`, list(onEmptyValues))
			return
		}
	}
}

func (v *validator) unknownTopField(m *member) {
	if contains(opFields, m.key) {
		v.at(m.keyOff, -1, "", `unknown field %q at the top level: %q belongs to an individual op, not to the policy; the top level accepts %s`,
			m.key, m.key, list(topFields))
		return
	}
	v.at(m.keyOff, -1, "", `unknown field %q%s; the top level of a policy accepts %s`, m.key, hint(m.key, topFields), list(topFields))
}

func (v *validator) unknownOpField(m *member, i int, id string) {
	if to, renamed := renamedOpFields[m.key]; renamed {
		v.at(m.keyOff, i, id, `unknown field %q; it was renamed to %q — an op accepts %s`, m.key, to, list(opFields))
		return
	}
	if contains(topFields, m.key) {
		v.at(m.keyOff, i, id, `unknown field %q on an op: %q is a top-level policy field, and the field sets are per level rather than one merged bag; an op accepts %s`,
			m.key, m.key, list(opFields))
		return
	}
	v.at(m.keyOff, i, id, `unknown field %q%s; an op accepts %s`, m.key, hint(m.key, opFields), list(opFields))
}

// zeroMatchHint prefers the revision-1 rename over a spelling guess: `write` is
// two edits from nothing in the current set, but it is exactly what the old
// design called `declare`, and a policy written against those docs is a likelier
// explanation than a typo.
func zeroMatchHint(bad string) string {
	if to, renamed := renamedZeroMatch[bad]; renamed {
		return fmt.Sprintf(" (revision 1 spelled this %q; it is now %q)", bad, to)
	}
	return hint(bad, zeroMatchValues)
}

// OpLabel is the name one op is filed and displayed under: its policy id, or
// its POSITION when it has none (D2). `ops[N]` is a computed label and never a
// value stored in Op.ID — storing it would make an unnamed op indistinguishable
// from one deliberately named "ops[0]", keyed by a name that shifts the moment
// somebody inserts an op above it.
//
// Both sides of the note lookup go through here. Notes are recorded under this
// label at parse time and read back under it when the PR body is rendered, so a
// second spelling of "ops[N]" anywhere would make notes silently stop appearing
// while every test on either side still passed.
func OpLabel(id string, index int) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("ops[%d]", index)
}

func contains(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}
