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
//
// STUB: signatures only. Phase 3 implements these.
package policy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
)

// Version is the only policy format version this binary understands.
const Version = 1

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
type MultiError struct{ Errs []error }

func (m *MultiError) Error() string {
	parts := make([]string, len(m.Errs))
	for i, e := range m.Errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

var errNotImplemented = errors.New("policy: not implemented")

// Parse decodes and fully validates src. filename appears in error messages.
func Parse(src []byte, filename string) (*Policy, error) { return nil, errNotImplemented }

// Load is Parse over a file on disk.
func Load(path string) (*Policy, error) { return nil, errNotImplemented }
