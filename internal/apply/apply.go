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
	"path/filepath"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/plan"
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
// content BEFORE writing, and writes atomically (temp file + rename in the
// same directory), so no failure mode leaves a truncated or half-written
// CODEOWNERS file — a partial write would silently change ownership (found
// in review: the previous in-place write could truncate on a full disk).
// Symlinks are resolved first so the link itself is preserved and its target
// is replaced.
func Apply(p *plan.Plan, filePath string) error {
	target := filePath
	if resolved, err := filepath.EvalSymlinks(filePath); err == nil {
		target = resolved
	}

	current, err := os.ReadFile(target)
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

	info, err := os.Stat(target)
	if err != nil {
		return &plan.InvalidError{Msg: err.Error()}
	}

	// Atomic write: temp file in the same directory, then rename. Any
	// failure before the rename leaves the original untouched.
	tmp, err := os.CreateTemp(filepath.Dir(target), ".codeowners-tool-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write([]byte(p.AfterContent)); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file (original untouched): %w", err)
	}
	if err := tmp.Chmod(info.Mode()); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file (original untouched): %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file (original untouched): %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return fmt.Errorf("rename into place (original untouched): %w", err)
	}

	// Post-rename verification: read back and re-validate; restore on any
	// discrepancy, and SURFACE a failed restore rather than claiming
	// success (found in review).
	written, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(written, []byte(p.AfterContent)) {
		return rollback(target, current, info.Mode(),
			&ValidationError{Msg: "post-write readback mismatch (R-10)"})
	}
	if newErrs := newSyntaxErrors(current, written); len(newErrs) > 0 {
		return rollback(target, current, info.Mode(), &ValidationError{
			Msg:    fmt.Sprintf("written file has %d new syntax error(s) (R-10)", len(newErrs)),
			Errors: newErrs,
		})
	}
	return nil
}

// rollback restores the original bytes. If the restore itself fails, the
// returned error says so explicitly instead of claiming a clean rollback.
func rollback(target string, original []byte, mode os.FileMode, cause *ValidationError) error {
	if err := os.WriteFile(target, original, mode); err != nil {
		cause.Msg = fmt.Sprintf("%s — AND ROLLBACK FAILED (%v): %s may be in a bad state, restore it manually", cause.Msg, err, target)
		return cause
	}
	cause.Msg += "; rolled back"
	return cause
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
