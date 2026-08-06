package policy_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/policy"
)

// BLOCKER 3 — the policy parser must be bounded by its input.
//
// scanner.value() ends every value with
//
//	v.raw = string(s.src[start:min(int(s.dec.InputOffset()), len(s.src))])
//
// which materializes a COPY of that value's source span for every value in the
// document. Nested containers each span nearly the whole file, so the copies are
// quadratic in nesting depth. Measured against the current implementation:
//
//	  16 KB policy  →   0.2 s
//	  60 KB policy  →   3.5 s and 962 MB resident
//	 400 KB policy  →   over two minutes
//
// `check --policy` is documented as the first line of every fleet script, and
// policy files are generated — a generator that emits one extra layer of nesting
// turns a routine preflight into an OOM kill on the runner. scanner.value() is also
// unbounded recursion, and Go's stack-growth failure is a fatal error that no
// recover() can catch, so depth needs its own limit rather than falling out of the
// memory fix.
//
// Three separate guards are pinned below because they fail differently: a depth cap
// (structure), a size cap (input), and linear allocation (the raw copies). Fixing
// only the last still leaves a document that recurses until the process dies.

// nestedPolicy builds a syntactically valid document nested depth levels deep.
func nestedPolicy(depth int) []byte {
	var b strings.Builder
	b.WriteString(`{"version":1,"ops":`)
	b.WriteString(strings.Repeat("[", depth))
	b.WriteString(strings.Repeat("]", depth))
	b.WriteString("}")
	return []byte(b.String())
}

// namesReason reports whether err explains itself in terms of one of the given
// words — AFTER deleting the file path from the message.
//
// Deleting the path is not defensive dressing. policy.Error renders the filename
// first, so a document named deep.json makes any error whatsoever "mention" depth,
// and the assertion passes while the guard does not exist. That was a real vacuous
// pass in the first draft of this file: the parser was rejecting the document as
// `ops[0]: an op must be a string ... got an array`, which is incidental to
// nesting, and only the filename made it look like a depth check. Temp-directory
// paths carry the TEST NAME too, so the same trap is one rename away.
func namesReason(err error, path string, words ...string) bool {
	msg := strings.ToLower(err.Error())
	msg = strings.ReplaceAll(msg, strings.ToLower(path), "")
	for _, w := range words {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}

// A policy nested far past anything a legitimate generator emits must be rejected
// for THAT reason, cheaply, before the document is walked.
//
// Today the document is rejected — but as `ops[0]: an op must be a string ..., got
// an array`, which is the format's ordinary type check noticing the first `[`. That
// is not a depth guard: it fires only because the nesting happens to start inside
// `ops`, and it says nothing at all about the 999 levels below it or about the
// recursion that walked them. The error has to name the limit, or there is no way
// to tell a bounded parser from a lucky one.
func TestBounds_RejectsExcessiveNestingDepth(t *testing.T) {
	const depth = 1000 // a real policy nests 3 levels: object → ops array → op object
	const name = "p.json"
	_, err := policy.Parse(nestedPolicy(depth), name)
	if err == nil {
		t.Fatalf("a policy nested %d levels deep parsed without error; want a depth-limit rejection", depth)
	}
	if !namesReason(err, name, "deep", "depth", "nest") {
		t.Errorf("a document nested %d levels was rejected, but not as a depth problem — so nothing bounds the recursion in scanner.value(), and Go's stack-growth failure is a fatal error no recover() can catch.\ngot: %v", depth, err)
	}
}

// The allocation guard. TotalAlloc is cumulative and monotonic, so it measures the
// copies the parser makes even after GC reclaims them — which is the actual defect,
// since the peak is what the OOM killer sees.
//
// The threshold is deliberately loose: a linear parser allocates a small multiple
// of its input, while the current implementation allocates roughly ten thousand
// times it, so nothing about this test is sensitive to allocator details.
func TestBounds_AllocationIsLinearInInputSize(t *testing.T) {
	const depth = 20000
	src := nestedPolicy(depth)
	const maxRatio = 64

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _ = policy.Parse(src, "deep.json")
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	budget := uint64(len(src)) * maxRatio
	if allocated > budget {
		t.Errorf("parsing a %d-byte policy allocated %d bytes (%.0f× the input); want under %d bytes (%d×).\nEvery jsonValue copies its own source span, so nested values re-copy the document once per level — store offsets and slice on demand.",
			len(src), allocated, float64(allocated)/float64(len(src)), budget, maxRatio)
	}
}

// policy.Load reads the whole file with no size limit. A policy is a
// human-reviewed artifact; a multi-megabyte one is a broken generator, and finding
// that out by exhausting the runner's memory is the wrong way round.
func TestBounds_LoadRejectsAnOversizePolicyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.json")
	// Flat, not nested: this isolates the SIZE cap from the depth and allocation
	// guards above, so a fix for one does not vacuously satisfy this test.
	huge := `{"version":1,"name":"` + strings.Repeat("a", 32<<20) + `","ops":["add_owner(/x/, @a)"]}`
	if err := os.WriteFile(path, []byte(huge), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := policy.Load(path)
	if err == nil {
		t.Fatalf("a %d MB policy file loaded without error; want a size-limit rejection", len(huge)>>20)
	}
	if !namesReason(err, path, "size", "large", "big", "limit", "byte") {
		t.Errorf("the file was rejected, but not as a size problem, so nothing caps what policy.Load will read into memory.\ngot: %v", err)
	}
}

// The guards must not be bought by rejecting real policies. This is the positive
// control: an ordinary generated policy — more ops than anyone writes by hand,
// nested exactly as the format requires — still parses.
func TestBounds_OrdinaryGeneratedPolicyStillParses(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"version":1,"name":"fleet","ops":[`)
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"op":"add_owner(/svc/, @org/team-a)","id":"op-`)
		b.WriteString(strings.Repeat("0", 3))
		b.WriteString(itoa(i))
		b.WriteString(`","note":"generated"}`)
	}
	b.WriteString(`]}`)

	p, err := policy.Parse([]byte(b.String()), "generated.json")
	if err != nil {
		t.Fatalf("a 500-op generated policy was rejected: %v", err)
	}
	if len(p.Ops) != 500 {
		t.Errorf("parsed %d ops, want 500", len(p.Ops))
	}
}

// itoa keeps this file free of a strconv import for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
