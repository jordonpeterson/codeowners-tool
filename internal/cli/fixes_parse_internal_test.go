// Package cli internal test: the `git status --porcelain -z` record parser,
// which no end-to-end test can reach in its interesting cases.
//
// The pathspec the caller passes filters out both malformed shapes before the
// parser sees them, so an e2e assertion about them passes whether the parser
// is right or wrong — the vacuous pass CONTRIBUTING singles out. Testing the
// function directly is the only honest way to pin it.
package cli

import "testing"

// SPEC R-23: a path whose NAME begins with two status letters is not read as a
// status code, and a record with no code is not read as one either.
func TestUnmergedCodeInParsesRecordsStrictly(t *testing.T) {
	const target = ".github/CODEOWNERS"
	rec := func(parts ...string) []byte {
		var b []byte
		for _, p := range parts {
			b = append(b, p...)
			b = append(b, 0)
		}
		return b
	}

	for _, tc := range []struct {
		name     string
		out      []byte
		wantCode string
		wantOK   bool
	}{
		{"a genuine conflict", rec("UU " + target), "UU", true},
		{"modify/delete counts too", rec("UD " + target), "UD", true},
		{"an ordinary modification does not", rec(" M " + target), "", false},
		{"a staged add does not", rec("A  " + target), "", false},
		{"another file's conflict does not", rec("UU docs/CODEOWNERS"), "", false},
		{"the conflict is found among others", rec(" M a.go", "UU "+target, "?? b.go"), "UU", true},

		// The two shapes the pathspec hides today.
		{
			// `UUx.github/CODEOWNERS` renamed: the FROM record carries no
			// code, and without the separator check it reads as XY="UU"
			// over a path equal to the target.
			"a codeless rename record is not a conflict",
			rec("R  "+target, "UUx"+target),
			"", false,
		},
		{
			// The same bytes as a plain path: still not a status code.
			"a path beginning with status letters is not a code",
			rec("UU UUx" + target),
			"", false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, ok := unmergedCodeIn(tc.out, target)
			if ok != tc.wantOK || code != tc.wantCode {
				t.Errorf("unmergedCodeIn(%q) = (%q, %v), want (%q, %v)", tc.out, code, ok, tc.wantCode, tc.wantOK)
			}
		})
	}
}
