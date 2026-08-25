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

// Written reports what the write actually moved, MEASURED from the bytes on
// disk rather than repeated from the plan. The plan's own size_before and
// size_after are claims made by the document being applied; echoing them let a
// tampered plan produce a confirmation message that was measurably false.
type Written struct {
	Before int
	After  int
}

// Apply writes a plan's after-content to filePath. Refuses if the file drifted
// from the plan's pinned hash or if after_content does not match the plan's
// own sha256_after, re-checks the size cap, validates the content BEFORE
// writing, and writes atomically (temp file + rename in the same directory),
// so no failure mode leaves a truncated or half-written CODEOWNERS file — a
// partial write would silently change ownership (found in review: the previous
// in-place write could truncate on a full disk). Symlinks are resolved first
// so the link itself is preserved and its target is replaced.
//
// The sha256_after check is skipped when the field is empty, which is how an
// in-process caller (sync, lint) that never serialises its plan reaches the
// same code path. The window this closes belongs to a plan that TRAVELS, so
// the verb that reads one off disk requires the field — see cmdApply.
func Apply(p *plan.Plan, filePath string) (Written, error) {
	target := filePath
	if resolved, err := filepath.EvalSymlinks(filePath); err == nil {
		target = resolved
	}

	current, err := os.ReadFile(target)
	if err != nil {
		return Written{}, &plan.InvalidError{Msg: fmt.Sprintf("read %s: %v", filePath, err)}
	}
	h := sha256.Sum256(current)
	if hex.EncodeToString(h[:]) != p.HashBefore {
		return Written{}, &plan.InvalidError{Msg: fmt.Sprintf(
			"%s changed since the plan was computed (hash mismatch) — the plan's invariants are no longer proven; re-run plan", filePath)}
	}
	// The plan's own integrity. Everything below this line trusts
	// after_content, and until here nothing established that the bytes under
	// review are the bytes about to be written.
	if p.HashAfter != "" && hashHex([]byte(p.AfterContent)) != p.HashAfter {
		return Written{}, &plan.InvalidError{Msg: "plan integrity: after_content does not match the plan's own sha256_after — the plan was corrupted, truncated or edited since it was computed, so what it says it will write and what it would write are different things; nothing written, re-run plan"}
	}
	if len(p.AfterContent) > 3_000_000 {
		return Written{}, &plan.RefusalError{Msg: fmt.Sprintf(
			"refusing: plan content is %d bytes, over the 3 MB limit GitHub silently ignores (S-4)", len(p.AfterContent))}
	}

	// Validate BEFORE writing: only errors the plan would introduce count;
	// pre-existing invalid lines are reported by audit, not blocked here.
	if newErrs := newSyntaxErrors(current, []byte(p.AfterContent)); len(newErrs) > 0 {
		return Written{}, &ValidationError{
			Msg:    fmt.Sprintf("plan content introduces %d syntax error(s); nothing written (R-10)", len(newErrs)),
			Errors: newErrs,
		}
	}

	info, err := os.Stat(target)
	if err != nil {
		return Written{}, &plan.InvalidError{Msg: err.Error()}
	}

	// Atomic write: temp file in the same directory, then rename. Any
	// failure before the rename leaves the original untouched.
	tmp, err := os.CreateTemp(filepath.Dir(target), ".codeowners-tool-*")
	if err != nil {
		return Written{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write([]byte(p.AfterContent)); err != nil {
		tmp.Close()
		cleanup()
		return Written{}, fmt.Errorf("write temp file (original untouched): %w", err)
	}
	if err := tmp.Chmod(info.Mode()); err != nil {
		tmp.Close()
		cleanup()
		return Written{}, fmt.Errorf("chmod temp file (original untouched): %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return Written{}, fmt.Errorf("close temp file (original untouched): %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return Written{}, fmt.Errorf("rename into place (original untouched): %w", err)
	}

	// Post-rename verification: read back and re-validate; restore on any
	// discrepancy, and SURFACE a failed restore rather than claiming
	// success (found in review).
	written, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(written, []byte(p.AfterContent)) {
		return Written{}, rollback(target, current, info.Mode(),
			&ValidationError{Msg: "post-write readback mismatch (R-10)"})
	}
	if newErrs := newSyntaxErrors(current, written); len(newErrs) > 0 {
		return Written{}, rollback(target, current, info.Mode(), &ValidationError{
			Msg:    fmt.Sprintf("written file has %d new syntax error(s) (R-10)", len(newErrs)),
			Errors: newErrs,
		})
	}
	// Measured, not repeated: `current` is pinned by sha256_before, and
	// `written` was just read back off disk.
	return Written{Before: len(current), After: len(written)}, nil
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
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
