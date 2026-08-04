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
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
)

// Version is the only policy format version this binary understands.
const Version = 1

// The field sets are PER LEVEL, not one merged bag. `description` is legal at
// the top and meaningless on an op; accepting it in both places would let a
// generator put the policy's description on op 17 and nothing would notice.
var (
	topFields = []string{"version", "name", "description", "on_empty", "ops"}
	opFields  = []string{"op", "id", "on_zero_match", "note"}
)

// The two enums, in the order the docs present them. Alphabetizing a legal set
// in an error message quietly re-ranks it; `require` is the default and belongs
// first.
var (
	onEmptyValues   = []string{"error", "inherit", "unowned"}
	zeroMatchValues = []string{ops.ZeroMatchRequire, ops.ZeroMatchSkip, ops.ZeroMatchDeclare}
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
	Ops         []ops.Op
	Notes       map[string]string // op ID -> note
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
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read policy file: %w", err)
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
	p := &Policy{}

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
	v.onEmpty(p, fields["on_empty"])
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
		v.at(val.off, -1, "", `field "version" must be a whole number, got %s`, val.raw)
	case n == 0:
		v.at(val.off, -1, "", `field "version" must be a positive integer, got 0; 0 is what an absent field decodes to, so it cannot also name a real format version`)
	case n < 0:
		v.at(val.off, -1, "", `field "version" must be a positive integer, got %s`, val.raw)
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

func (v *validator) opsArray(p *Policy, m *member, rootOff int) []opInfo {
	if m == nil {
		v.at(rootOff, -1, "", `missing required field "ops"; a policy with no ops does nothing on every repo and exits 0 on all of them, which is the silent success this format exists to make impossible`)
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
	var zeroM *member
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

	parsed, err := ops.Parse(spec)
	if err != nil {
		// Letting ops.Parse's own text out would lose the file, the line, and the
		// op index: the operator gets `unknown op "add_ownr"` with no idea which
		// of 40 ops in which of 100 repos.
		v.at(specOff, i, info.id, `op %q is not valid: %v`, spec, err)
		return info
	}
	info.kind, info.parsed = parsed.Kind, true

	if zeroM != nil {
		v.checkZeroMatchLegality(zeroM, parsed.Kind, zero, i, info.id)
	}

	parsed.ID = info.id
	if zeroOK {
		parsed.OnZeroMatch = zero
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
