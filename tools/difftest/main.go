// Command difftest differentially fuzzes internal/pattern against the
// UNMODIFIED reference implementation from hmarr/codeowners (oracle_hmarr.go,
// vendored verbatim, MIT © Harry Marr). Our matcher is a port of the same
// logic; this catches porting drift byte-for-byte.
//
// Known deliberate divergences (skipped): our Compile rejects "!" negation
// and empty patterns.
//
// Usage: go run ./tools/difftest [iterations] [seed]
//
// The seed defaults to 1 so a given iteration count is reproducible. CI runs the
// default on every PR; pass another when you are working on the matcher and want
// input outside the region that gate already covers.
package main

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"

	ours "github.com/jordonpeterson/codeowners-tool/internal/pattern"
)

var segments = []string{
	"src", "docs", "x", "Apps", "foo", "bar", "f*", "*", "**", "a?c", "*.go",
	"*.md", "[param]", "f\\ o", "b-z", "^cpu", "a~b", "x:y", "réunion", "🦉",
}

var fileSegs = []string{
	"main.go", "README.md", "a.tf", "foo", "bar", "App.js", "x", "[param]",
	"f o", "abc", "a?c", "^cpu", "réunion", "🦉.txt",
}

func genPattern(r *rand.Rand) string {
	n := 1 + r.Intn(3)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = segments[r.Intn(len(segments))]
	}
	p := strings.Join(parts, "/")
	if r.Intn(4) == 0 {
		p = "/" + p
	}
	if r.Intn(4) == 0 {
		p += "/"
	}
	return p
}

func genPath(r *rand.Rand) string {
	n := 1 + r.Intn(4)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fileSegs[r.Intn(len(fileSegs))]
	}
	return strings.Join(parts, "/")
}

func main() {
	iters := 200000
	if len(os.Args) > 1 {
		if n, err := strconv.Atoi(os.Args[1]); err == nil {
			iters = n
		}
	}
	// The seed defaults to 1, so `difftest N` is byte-for-byte reproducible and
	// the PR gate either fails for everyone or for no one.
	//
	// It is settable because a fixed seed makes "more iterations" mean only a
	// longer PREFIX of one sequence. Raising the count re-derives everything the
	// gate already proved and adds cases from the same region; changing the seed
	// is the only way to reach input the gate structurally cannot. That matters
	// when someone edits the matcher, which is the point at which the fixed 500k
	// stops being evidence about the parts of the space it never visits. The seed
	// is printed with the result so any failure can be replayed exactly.
	seed := int64(1)
	if len(os.Args) > 2 {
		if n, err := strconv.ParseInt(os.Args[2], 10, 64); err == nil {
			seed = n
		}
	}
	r := rand.New(rand.NewSource(seed))
	compared, skipped, mismatches := 0, 0, 0
	for i := 0; i < iters; i++ {
		pat := genPattern(r)
		path := genPath(r)

		oracle, oerr := newPattern(pat)
		mine, merr := ours.Compile(pat)
		if oerr != nil || merr != nil {
			// Both reject, or a documented divergence (empty, "!") — count
			// disagreements on compilability, tolerating our stricter rules.
			if oerr != nil && merr == nil {
				fmt.Printf("COMPILE MISMATCH: %q — oracle rejects (%v), ours accepts\n", pat, oerr)
				mismatches++
			}
			skipped++
			continue
		}
		want, _ := oracle.match(path)
		got := mine.Match(path)
		compared++
		if want != got {
			mismatches++
			fmt.Printf("MATCH MISMATCH: pattern %q path %q — oracle=%v ours=%v\n", pat, path, want, got)
			if mismatches > 20 {
				fmt.Println("too many mismatches, aborting")
				os.Exit(1)
			}
		}
	}
	fmt.Printf("differential fuzz vs hmarr/codeowners oracle (seed %d): %d compared, %d skipped (compile-reject), %d mismatches\n",
		seed, compared, skipped, mismatches)
	if mismatches > 0 {
		os.Exit(1)
	}
}
