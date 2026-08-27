package policy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/policy"
)

// These tests cover R-40's policy surface: the per-op `on_unowned` field
// (assign | skip), its legality table (add_owner only, never beside declare),
// and its defaults-block plumbing (R-35). Like on_zero_match, the field is
// policy-only — the reviewed artifact, not a shell line, states that a wave
// deliberately leaves open paths open.

// SPEC R-40: the field parses on add_owner and lands on the op. An absent
// field is "", which plan.Build reads as assign — today's behavior.
func TestR40_OnUnownedComesFromTheFile(t *testing.T) {
	for _, val := range []string{ops.UnownedAssign, ops.UnownedSkip} {
		t.Run(val, func(t *testing.T) {
			p := mustParse(t, `{"version":1,"ops":[{"id":"gh","op":"add_owner(/.github/, @org/platform)","on_unowned":"`+val+`"}]}`)
			if p.Ops[0].OnUnowned != val {
				t.Errorf("OnUnowned = %q, want %q", p.Ops[0].OnUnowned, val)
			}
		})
	}
	p := mustParse(t, `{"version":1,"ops":["add_owner(/.github/, @org/platform)"]}`)
	if p.Ops[0].OnUnowned != "" {
		t.Errorf("OnUnowned = %q, want the zero value on an op that never stated one", p.Ops[0].OnUnowned)
	}
}

// SPEC R-40: bad values are rejected at load with the legal set enumerated,
// and a PRESENT-but-empty value is not the same as an absent one — "" states
// no decision while reading to a reviewer as though a choice was made.
func TestR40_BadOnUnownedValueRejected(t *testing.T) {
	for _, bad := range []string{"SKIP", "Assign", "own", "true", ""} {
		t.Run("value_"+bad, func(t *testing.T) {
			err := mustReject(t, `{"version":1,"ops":[{"op":"add_owner(/x/, @a)","on_unowned":"`+bad+`"}]}`)
			assertNoGoInternals(t, err)
			assertMentions(t, err, "on_unowned")
			for _, legal := range []string{"assign", "skip"} {
				if !strings.Contains(err.Error(), legal) {
					t.Errorf("error must enumerate the legal set (missing %q):\n%s", legal, err.Error())
				}
			}
		})
	}
	err := mustReject(t, `{"version":1,"ops":[{"op":"add_owner(/x/, @a)","on_unowned":true}]}`)
	assertNoGoInternals(t, err)
	assertMentions(t, err, "on_unowned")
}

// SPEC R-40: the field is legal only on add_owner. remove_owner cannot touch
// an open path anyway, set_owners displaces owners by design, and
// rename_owner has no scope — accepting-and-ignoring the field on any of them
// is the same class of failure as a typo'd field name.
func TestR40_OnUnownedRejectedOnOtherVerbs(t *testing.T) {
	cases := map[string]string{
		"set_owners":   `{"version":1,"ops":[{"op":"set_owners(/x/, [@a])","on_unowned":"skip"}]}`,
		"remove_owner": `{"version":1,"on_empty":"error","ops":[{"op":"remove_owner(/x/, @a)","on_unowned":"skip"}]}`,
		"rename_owner": `{"version":1,"ops":[{"op":"rename_owner(@a, @b)","on_unowned":"skip"}]}`,
	}
	for verb, src := range cases {
		t.Run(verb, func(t *testing.T) {
			err := mustReject(t, src)
			assertMentions(t, err, "on_unowned", verb)
			assertNoGoInternals(t, err)
		})
	}
}

// SPEC R-40b: skip rides beside on_zero_match=declare, and both land on the
// op. The pair ranges over disjoint domains — declare acts where the scope
// matches no tracked file, skip on tracked files with no owner — so it states
// one intent across two repo shapes: pre-own what does not exist, never close
// what is open today. It was refused on the reasoning that nonexistent files
// are "unowned by definition"; they are in no op's path universe at all.
func TestR40b_SkipComposesWithDeclare(t *testing.T) {
	for _, val := range []string{ops.UnownedAssign, ops.UnownedSkip} {
		t.Run(val, func(t *testing.T) {
			p := mustParse(t, `{"version":1,"ops":[{"op":"add_owner(/x/, @a)","on_zero_match":"declare","on_unowned":"`+val+`"}]}`)
			if p.Ops[0].OnUnowned != val {
				t.Errorf("OnUnowned = %q, want %q", p.Ops[0].OnUnowned, val)
			}
			if p.Ops[0].OnZeroMatch != ops.ZeroMatchDeclare {
				t.Errorf("OnZeroMatch = %q, want declare — lifting the pair must not drop the other half", p.Ops[0].OnZeroMatch)
			}
		})
	}
	// R-30 is a different rule and stays: a declared line captures future
	// files under the excepted pattern, so THAT carve promise is void.
	err := mustReject(t, `{"version":1,"ops":[{"op":"add_owner(/x/ except /x/gen/, @a)","on_zero_match":"declare","on_unowned":"skip"}]}`)
	assertMentions(t, err, "declare", "except")
	assertNoGoInternals(t, err)
}

// SPEC R-40/R-35: `defaults` carries on_unowned, so a 40-op baseline states
// "leave open paths open" once. It reaches only ops that can carry the field
// — add_owner without a declare — and a per-op value always wins.
func TestR40_DefaultsSupplyOnUnowned(t *testing.T) {
	src := `{"version":1,"on_empty":"error","defaults":{"on_unowned":"skip"},"ops":[
		"add_owner(/a/, @a)",
		{"op":"add_owner(/b/, @b)","on_unowned":"assign"},
		"set_owners(/c/, [@c])",
		"remove_owner(/d/, @d)",
		"rename_owner(@e, @f)",
		{"op":"add_owner(/g/, @g)","on_zero_match":"declare"}
	]}`
	p := mustParse(t, src)
	// The declared op (index 5) DOES receive the default now (R-40b): the two
	// settings compose, so the block reaches every add_owner.
	want := []string{"skip", "assign", "", "", "", "skip"}
	for i, w := range want {
		if got := p.Ops[i].OnUnowned; got != w {
			t.Errorf("Ops[%d] (%s) OnUnowned = %q, want %q", i, p.Ops[i].Raw, got, w)
		}
	}
}

// SPEC R-40b/R-35: a defaulted declare DOES reach an op that explicitly states
// on_unowned=skip. The exclusion existed only because the pairing was illegal
// — folding a default that produced a refusal would have failed a policy on a
// combination its author never wrote. With the pair legal, R-35's plain rule
// applies: the block fills in what the op did not state.
//
// This is the one behavior change of the lift that touches a policy which was
// already legal: before, the op ran under `require` and refused (exit 2) in a
// repo whose scope matched nothing; now it declares and exits 0.
func TestR40b_DefaultedDeclareReachesSkipOp(t *testing.T) {
	src := `{"version":1,"defaults":{"on_zero_match":"declare"},"ops":[
		"add_owner(/a/, @a)",
		{"op":"add_owner(/b/, @b)","on_unowned":"skip"}
	]}`
	p := mustParse(t, src)
	for i := range p.Ops {
		if p.Ops[i].OnZeroMatch != ops.ZeroMatchDeclare {
			t.Errorf("Ops[%d].OnZeroMatch = %q, want the defaulted declare", i, p.Ops[i].OnZeroMatch)
		}
	}
	if p.Ops[1].OnUnowned != ops.UnownedSkip {
		t.Errorf("Ops[1].OnUnowned = %q, want the op's own skip to survive", p.Ops[1].OnUnowned)
	}
}

// SPEC R-40b/R-35: the mirror — a defaulted skip reaches an op that explicitly
// declares, for the same reason.
func TestR40b_DefaultedSkipReachesDeclaredOp(t *testing.T) {
	src := `{"version":1,"defaults":{"on_unowned":"skip"},"ops":[
		{"op":"add_owner(/b/, @b)","on_zero_match":"declare"}
	]}`
	p := mustParse(t, src)
	if p.Ops[0].OnUnowned != ops.UnownedSkip {
		t.Errorf("OnUnowned = %q, want the defaulted skip", p.Ops[0].OnUnowned)
	}
	if p.Ops[0].OnZeroMatch != ops.ZeroMatchDeclare {
		t.Errorf("OnZeroMatch = %q, want the op's own declare", p.Ops[0].OnZeroMatch)
	}
}

// SPEC R-40b/R-35: a defaults block may state BOTH, and both reach every op
// that can carry them. One reviewed line then states the whole fleet posture:
// pre-own what does not exist, never close what is open today.
func TestR40b_DefaultsMayStateBoth(t *testing.T) {
	p := mustParse(t, `{"version":1,"defaults":{"on_zero_match":"declare","on_unowned":"skip"},"ops":["add_owner(/x/, @a)"]}`)
	if p.Ops[0].OnZeroMatch != ops.ZeroMatchDeclare || p.Ops[0].OnUnowned != ops.UnownedSkip {
		t.Errorf("Ops[0] = {zero:%q unowned:%q}, want {declare skip}", p.Ops[0].OnZeroMatch, p.Ops[0].OnUnowned)
	}
	if p.Defaults.OnZeroMatch != ops.ZeroMatchDeclare || p.Defaults.OnUnowned != ops.UnownedSkip {
		t.Errorf("Defaults = %+v, want both recorded", p.Defaults)
	}
}

// SPEC R-40: a bad value inside defaults is rejected exactly as it is per-op
// — a typo'd default is the WRONG default applied to every op at once.
func TestR40_BadOnUnownedInDefaultsRejected(t *testing.T) {
	err := mustReject(t, `{"version":1,"defaults":{"on_unowned":"skpi"},"ops":["add_owner(/x/, @a)"]}`)
	assertMentions(t, err, "on_unowned")
	assertNoGoInternals(t, err)
	for _, legal := range []string{"assign", "skip"} {
		if !strings.Contains(err.Error(), legal) {
			t.Errorf("error must enumerate the legal set (missing %q):\n%s", legal, err.Error())
		}
	}
}

// SPEC R-40/R-20: the near-miss typo names the field it resembles, on the op
// and in defaults, so a generator drift is a one-edit fix rather than a hunt.
func TestR40_NearMissTypoGetsAHint(t *testing.T) {
	err := mustReject(t, `{"version":1,"ops":[{"op":"add_owner(/x/, @a)","on_unownd":"skip"}]}`)
	assertMentions(t, err, "on_unownd", "on_unowned")
	assertNoGoInternals(t, err)
}

// SPEC R-40: check-level validation is load-level validation — a policy
// carrying the field parses identically through Load, so `check --policy`
// (which opens no repository) is the thing that catches every refusal above
// at repo 0. Sanity-check the happy path end to end.
func TestR40_LoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	src := `{"version":1,"defaults":{"on_unowned":"skip"},"ops":[{"id":"gh","op":"add_owner(/.github/, @org/platform)"}]}`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ops[0].OnUnowned != ops.UnownedSkip {
		t.Errorf("OnUnowned = %q, want skip via defaults", p.Ops[0].OnUnowned)
	}
	if p.Defaults.OnUnowned != ops.UnownedSkip {
		t.Errorf("Defaults.OnUnowned = %q, want skip", p.Defaults.OnUnowned)
	}
}
