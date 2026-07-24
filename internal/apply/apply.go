// Package apply is the writer — the only code path in the system that
// modifies bytes on disk (R-0). Engine B never imports this; its output is
// Engine A ops that flow through plan.Build first.
package apply

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/jordonpropm/codeowners-tool/internal/file"
	"github.com/jordonpropm/codeowners-tool/internal/plan"
)

// ValidationError: the written file failed post-write validation and was
// rolled back (R-10, exit 6).
type ValidationError struct {
	Msg    string
	Errors []file.SyntaxError
}

func (e *ValidationError) Error() string { return e.Msg }

// Apply writes a plan's after-content to filePath. Refuses if the file
// drifted from the plan's pinned hash, re-checks the size cap, validates the
// result, and rolls back on any new syntax error.
func Apply(p *plan.Plan, filePath string) error {
	current, err := os.ReadFile(filePath)
	if err != nil {
		return &plan.InvalidError{Msg: fmt.Sprintf("read %s: %v", filePath, err)}
	}
	h := sha256.Sum256(current)
	if hex.EncodeToString(h[:]) != p.HashBefore {
		return &plan.InvalidError{Msg: fmt.Sprintf(
			"%s changed since the plan was computed (hash mismatch) — the plan's invariants are no longer proven; re-run plan", filePath)}
	}
	if len(p.AfterContent) > 3_000_000 {
		return &plan.RefusalError{Msg: fmt.Sprintf(
			"refusing: plan content is %d bytes, over the 3 MB limit GitHub silently ignores (S-4)", len(p.AfterContent))}
	}

	// Validate BEFORE writing: only errors the plan would introduce count;
	// pre-existing invalid lines are reported by audit, not blocked here.
	if newErrs := newSyntaxErrors(current, []byte(p.AfterContent)); len(newErrs) > 0 {
		return &ValidationError{
			Msg:    fmt.Sprintf("plan content introduces %d syntax error(s); nothing written (R-10)", len(newErrs)),
			Errors: newErrs,
		}
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filePath, []byte(p.AfterContent), info.Mode()); err != nil {
		return err
	}

	// Post-write verification: read back and re-validate; roll back on any
	// discrepancy (torn write, filesystem surprise).
	written, err := os.ReadFile(filePath)
	if err != nil || !bytes.Equal(written, []byte(p.AfterContent)) {
		_ = os.WriteFile(filePath, current, info.Mode())
		return &ValidationError{Msg: "post-write readback mismatch; rolled back (R-10)"}
	}
	if newErrs := newSyntaxErrors(current, written); len(newErrs) > 0 {
		_ = os.WriteFile(filePath, current, info.Mode())
		return &ValidationError{
			Msg:    fmt.Sprintf("written file has %d new syntax error(s); rolled back (R-10)", len(newErrs)),
			Errors: newErrs,
		}
	}
	return nil
}

// newSyntaxErrors returns syntax errors present in after but not in before,
// compared by (kind, source line text) so unchanged bad lines don't block.
func newSyntaxErrors(before, after []byte) []file.SyntaxError {
	seen := map[string]bool{}
	for _, ln := range file.Parse(before).InvalidLines() {
		seen[ln.Err.Kind+"\x00"+ln.Err.Source] = true
	}
	var out []file.SyntaxError
	for _, ln := range file.Parse(after).InvalidLines() {
		if !seen[ln.Err.Kind+"\x00"+ln.Err.Source] {
			out = append(out, *ln.Err)
		}
	}
	return out
}
