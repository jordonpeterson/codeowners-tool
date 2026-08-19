# Scope subtraction (`except`) — specification

**Status: implemented.** Requirements R-26…R-32 below are enforced by the e2e
suite in `internal/cli/except_test.go`, which was written against this document
before the implementation existed (see [CONTRIBUTING.md](../CONTRIBUTING.md): the
failing test comes first). One guarantee is deliberately weakenable and only by
explicit opt-in (`on_except_zero_match: allow`, R-28) — it is marked in output the
same way `declare` is, and documented below alongside it.

## Motivation

The policy class "broad grant with a carve-out" — *team_a co-owns `/.github/`,
except `/.github/CODEOWNERS`, which stays with whoever owns it now* — is one human
intent, but today it takes two policy files run in sequence, because its two-op
spelling (`add_owner` + `remove_owner`) overlaps without commuting and R-8
correctly refuses it in one batch.

`except` puts the carve-out inside the intent, where it belongs:

```
add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)
```

The excepted paths are simply **out of the op's scope**: the op neither grants nor
revokes there, and the existing invariants — not new machinery — prove them
untouched. This also dissolves the wider conflict class by turning *ordering*
problems into *disjointness* problems: "carve out and reassign" becomes two
non-overlapping ops that commute, which R-8 already accepts (R-31).

`except` means **don't touch**. It is not a revocation and not a standing
guarantee that the owner never holds the excepted paths — if the grantee already
owns an excepted path, that ownership persists (and is reported, R-32). The
revocation intent remains `remove_owner`; the standing guarantee belongs to
`audit`.

## Grammar (R-26a)

The scope argument of `add_owner`, `set_owners`, and `remove_owner` may carry an
`except` clause:

```
<scope> except <pat> [<pat> ...]
```

- Delimiter is **unescaped whitespace**, which is otherwise illegal inside a
  scope (see `checkScope`) — so every op string legal today parses identically,
  and the keyword cannot collide with a path (a path containing ` except ` spells
  its spaces `\ ` and is not split).
- The keyword is lowercase `except`, exactly. Any other unescaped-whitespace
  spelling (including `EXCEPT`) keeps failing checkScope's existing whitespace
  refusal — no new acceptance is introduced for near-misses.
- Each `<pat>` obeys every rule a scope obeys: non-empty, escaped whitespace, no
  dangling backslash, compilable pattern.
- This is the **only** spelling. There is no separate JSON field for excepts; the
  policy object form carries them inside its `"op"` string. Two spellings of one
  fact is how generators and humans drift apart.
- `rename_owner` has no scope and therefore no `except`; using one is malformed
  (policy error, exit 3).

One new per-op policy field exists (object form only, like `on_zero_match`):
`on_except_zero_match`, values `require` (default) | `allow` — defined in R-28.

## Semantics

### R-26 — effective scope

An op's scope set is
`{tracked paths matching scope} \ {tracked paths matching any except pattern}`.
Every existing per-op mechanism — INV-1, `on_zero_match`, R-5, R-19 idempotence —
operates on this **effective scope**.

Excepted paths are out of scope **of this op**, nothing more. *This op* must not
change their resolution; a **sibling op in the same batch may legitimately govern
them** (that disjointness is R-31's entire point). Batch-level INV-2 therefore
binds exactly the paths in *no* op's effective scope — which is what the prover
already enforces — and a path excepted by one op and scoped by another is proven
under the sibling's INV-1, not under INV-2.

### R-27 — static validation (policy errors, exit 3)

All of the following are properties of the policy alone, are reported by
`check --policy` with file/line/op-id, and exit 3 from `sync` before anything is
read from the repo. Pattern comparisons in this section are over the **pattern
language** (containment in both directions, the planner's existing
`samePatternLanguage` discipline), never string equality — `/x/**` and `/x/` are
the same language spelled differently, and a string check would wave through a
policy that empties its own scope in every repo, then fail per-repo at exit 2 a
hundred times where the contract demands one exit 3.

1. Every except pattern must be **provably contained** in its op's scope, by the
   same conservative containment relation the planner already uses
   (`pattern.Contains`). Unprovable containment is refused — the fail-closed
   posture extends to the policy language. Containment is sound over the pattern
   language, so every tracked except match is an in-scope path on every tree;
   there is no "except bites a foreign subtree" state.
2. An except pattern whose language equals or contains its op's scope empties the
   op by construction — refused.
3. Two except patterns of one op whose languages are equal are duplicates —
   refused (a generator bug, not a choice).
4. `except` on `rename_owner` — refused ("rename_owner takes no scope").
5. `on_zero_match: declare` combined with `except` — refused (R-30: a declared
   line cannot encode subtraction).
6. `on_except_zero_match` with any value other than `"require"` or `"allow"`, or
   on an op with no except clause — refused.
7. An `except` keyword followed by no patterns — refused ("except clause has no
   patterns").

### R-28 — zero-match interplay (per repo)

Two emptiness questions exist, and they are **ordered**:

1. **First, the effective scope.** If it is empty, `on_zero_match` disposes of
   the op — `require` refuses (exit 2), `skip` moves on, exactly as today — and
   `on_except_zero_match` is **never consulted**: an op that writes nothing can
   reopen nothing. This keeps the "*if* this repo has X" idiom working: scope
   `/services/` with `on_zero_match: skip` skips repos without `/services/`,
   regardless of what the except would have matched there. The refusal message
   under `require` distinguishes "scope matches zero tracked files" from "every
   in-scope path is excepted", because the second is how a too-broad except
   announces itself.
2. **Then, only if the op will write:** an except pattern matching zero tracked
   files is governed by `on_except_zero_match`:
   - `require` *(default)*: refuse this repo (exit 2, "except pattern …matches
     zero tracked files"), write nothing. An except that bites nothing means the
     carve-out this policy promises does not exist here — in the motivating
     case, a repo whose CODEOWNERS still lives at the root would otherwise
     receive the broad `.github/` grant with **no** carve line, and a
     later-created `/.github/CODEOWNERS` would fall under the grant: the
     precedence-escalation hole (S-8), reopened silently. Exit 2 is the tool
     saying "normalize this repo first".
   - `allow`: proceed; the inert pattern is listed in the JSON record
     (`except_unmatched`, R-32) and as a warning. **This is a declare-class
     weakening of INV-1 and is marked like one**: the grant is written with no
     carve for the unmatched pattern, so a matching file created later falls
     under the grant — nothing in the repo today can verify the carve-out you
     asked for. Ops that take this path report `proven: "structural"`, exactly
     as `declare` does, and the cost is documented in
     [REFERENCE.md](REFERENCE.md#what-on_except_zero_match-allow-costs) beside
     declare's. No dead rule is written for the unmatched pattern (R-5).

### R-29 — carve synthesis

Applies to **every line an except-carrying op writes or amends**, whatever the
verb — `set_owners` and `remove_owner` amendments capture excepted paths exactly
as an inserted grant does, and a spec that only carved for `add_owner` would make
`except` add_owner-only in practice.

When such a line would (by last-match-wins) govern one or more excepted paths,
the planner synthesizes carve lines as follows:

- **Unit of synthesis:** one carve line per *(written/amended line ×
  owner-homogeneous excepted region)*. Where one except pattern covers paths
  whose current owners differ, the planner derives narrower patterns per
  homogeneous region using the existing intersection machinery
  (`deriveIntersection` and friends), with the same warning discipline for
  inexact narrowings. Only when no sound pattern is derivable for some region
  does it refuse (exit 2, same message shape as the existing narrowing refusal).
- **Restated owners** are the excepted paths' effective owners **on the evolving
  file at synthesis time** — not the before-batch snapshot. (A sibling
  `rename_owner` ordered earlier must see its rename respected; restating stale
  before-batch owners would force a spurious gate refusal.)
- **Placement is structural, not tree-observed:** each carve line goes
  **immediately after the line it corrects**, before any pre-existing rule that
  follows it. Placement may never move a carve past a pre-existing rule. The
  tree-observable alternative ("anywhere it wins for the excepted paths and
  nothing else") admits an end-of-file placement that the proof gate cannot
  reject — the gate ranges tracked paths, and a pre-existing rule matching zero
  tracked files (a dead security rule like `/x/gen/secret/ @Sec`) would be
  silently shadowed for every *future* file it exists to guard. The project
  already treats tree-exact-but-future-wrong output as a wrong write
  (`anchoredDirPrefix`), and carve placement inherits that standard.
- Every carve line appears in `changes[]` with a reason naming the except clause
  — reviewers must never meet an unexplained owner in a diff.

One refusal edge is unfixable by synthesis: **an excepted path that currently
matches no rule.** Unmatched (`nil`) and explicitly zero-owned (`[]`, S-9) are
distinct resolved states and never equal (`OwnersEqual`); no writable line can
restore "unmatched" once a broad line captures the path, so the op refuses
(exit 2, "matches no rule") rather than quietly converting "nobody owns this"
into "a rule says nobody owns this".

### R-30 — `declare` cannot carry an except

A declared rule is one literal CODEOWNERS line, and CODEOWNERS has no negation
(S-2): the moment a file matching both the scope and the except comes into
existence, the declared line governs it — the except would be a comment, not a
constraint. Policy error, exit 3 (a static fact about the syntax, identical in
every repo).

### R-31 — R-8 runs on effective scopes

The tree-path form of R-8 (overlap and commutation over tracked paths) uses
**effective scopes**, as does the rename-widening fixpoint that feeds it. Ops
made disjoint by their excepts commute and are accepted in one policy:

```json
{
  "version": 1,
  "ops": [
    "add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)",
    "add_owner(/.github/CODEOWNERS, @org/platform)"
  ]
}
```

The **declared-pattern form** of R-8 (zero-match declares, decided over patterns
because there are no paths to test) cannot use effective scopes — scope-minus-
except has no pattern representation under S-2. It uses the except-carrying op's
**raw scope** as a conservative over-approximation: sound, refusing at worst a
batch that per-path analysis might have admitted, never admitting one it
shouldn't.

Ops whose effective scopes still overlap without commuting refuse exactly as
today. Nothing that R-8 accepts today becomes a refusal.

### R-32 — the record explains the carve-outs

Under `--format json`, a per-op result with an except clause additionally
reports:

- `excepted`: every tracked excepted path with its resolved owners **in the
  final after-batch state** — `[{"path": "...", "owners": ["..."]}]`. For a path
  no sibling op governs this equals the before state (R-26); when a sibling op
  reassigns a carved path (R-31's layered policy), the after-batch owners are
  the ones an operator auditing "who ended up holding the carve-outs" needs.
  This is also the per-repo surface for the *don't-touch-≠-revoke* misread: a
  grantee already owning an excepted path is visible here, not silent.
- `except_unmatched`: except patterns that matched zero tracked files (present
  only under `on_except_zero_match: allow`, which is the only way to reach that
  state with a written file), alongside `proven: "structural"` (R-28).

`--dry-run`, human output, and `--summary-out` render the same facts. Schema
changes are additive and `omitempty`; records for ops without an except clause
are byte-identical to today's.

## `--create`

A repo with no CODEOWNERS plus `--create` meets `except` fail-closed. Under the
default `on_except_zero_match: require`, an except-carrying op refuses before the
file is created: either the excepted paths are untracked (zero-match, R-28) or
they are tracked but resolve to no rule in an empty file (unmatched, R-29's
refusal edge) — and a refusal creates nothing (`created: false`). Under `allow`,
a created file receives the grant with no carve, marked `proven: "structural"`
like any other allow-path write. The motivating fleet case should not pass
`--create` at all: a repo with no CODEOWNERS has no original owners to preserve,
which is a human decision, not a default.

## Worked example

Given a repo whose file is at `.github/CODEOWNERS` — the highest-precedence
location (S-8); a root- or docs/-located file plus this op refuses under the
R-28 default until the repo is normalized, which is deliberate:

```
* @org/original_team
```

```console
$ codeowners-tool sync --op 'add_owner(/.github/ except /.github/CODEOWNERS, @org/team_a)'
```

```
* @org/original_team
/.github/ @org/original_team @org/team_a
/.github/CODEOWNERS @org/original_team
```

One op, one proof, one write; the original team is discovered, not named; re-run
is a byte-identical no-op (R-19). The old two-policy sequence remains valid and
documented as the escape hatch for sequences `except` cannot express.

## Non-goals

Flat subtraction only. No unions, no nested or grouped excepts, no
except-of-except, no scope boolean algebra — the day `except` becomes an
expression language it stops being the simple spelling of one intent, which is
its entire reason to exist. The Operations section of
[REFERENCE.md](REFERENCE.md#operations) carries the same limits.
