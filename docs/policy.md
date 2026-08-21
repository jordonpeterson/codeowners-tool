# The policy as the source of truth — specification

**Status: R-33…R-37 implemented. R-38 specified, not implemented.** Each requirement
is the target for e2e suites written against this document before the implementation
exists (CONTRIBUTING.md: the failing test comes first).

## Motivation

A policy file is committed to a repository, reviewed once, and then run across a
fleet. Anything that changes the outcome but lives in a shell line is outside that
review: it is not in the diff, not in the approval, and survives only as long as the
command that carried it. Today `--create` is such a flag, per-op settings cannot be
stated once, `lint` has no policy at all, and one op cannot name two owners.

The rule these requirements enforce: **if it changes what gets written, it is in the
JSON.** Flags stay for what is genuinely per-invocation — which repository, which
ref, which output path.

## R-33 — owner lists on `add_owner` and `remove_owner`

```
add_owner(/services/api/, [@org/api-team, @org/platform])
remove_owner(/docs/, [@org/legacy, @org/former])
```

The single-owner form stays legal and unchanged. `set_owners` already takes a list;
this removes the asymmetry that makes people reach for the displacing verb when they
meant the co-owning one.

- **R-33a Fold equivalence.** A list produces exactly the file the single-owner ops
  it replaces produce, including byte order. `add ∘ add` already commutes, so no new
  refusal case exists.
  **One exception, and the list is the correct side of it.** Under
  `on_empty: inherit`, removing several owners from one scope is refused as a batch —
  the guard is pairwise and cannot prove order-independence — and run as a sequence it
  gives the wrong answer: with `/services/ @org/a @org/keep` above
  `/services/api/ @org/a @org/b`, removing `@org/a` then `@org/b` leaves
  `services/api/main.go` owned by `{@org/a, @org/keep}`. The owner the operator asked
  to remove is back, inherited from the broader rule. The list form resolves the whole
  removal at once and yields `{@org/keep}`, which is what `remove_owner` promises:
  after it, no named owner owns any in-scope path. So the list is not merely shorter
  here — it expresses an intent the batch refuses and the sequence gets wrong.
- **R-33b One intent, one hunk.** N owners on one scope produce **one** line change,
  not N. The plan artifact must never show an intermediate state that is not on disk
  at any point (this is the defect the list exists to fix).
- **R-33c Duplicate inside one list** is exit 3 — repo-independent, so it is a policy
  error, not a per-repo refusal.
- **R-33d Empty list** on `add_owner`/`remove_owner` is exit 3. `set_owners(scope, [])`
  stays legal and still means "nobody owns this".
- **R-33e Order within the list** does not change resolved ownership. It fixes append
  order in the written line, exactly as the equivalent op sequence does today.
- **R-33f `rename_owner` takes no list** — exit 3. It renames one owner to one owner.

## R-34 — `create` in the policy

```json
{ "version": 1, "create": true, "ops": ["add_owner(/docs/, @org/docs-team)"] }
```

- **R-34a** `create: true` makes `sync` write a missing CODEOWNERS, identical in every
  respect to `--create`.
- **R-34b** `--create` passed together with `--policy` is exit 3, the rule already
  applied to `--on-empty` and `--max-paths-changed`: a flag must not silently
  override the reviewed artifact.
- **R-34c** Absent or `false`, a missing file refuses exactly as today.
- **R-34d** R-23 is unchanged: `create` never overwrites an existing file, and is
  therefore safe to leave set for a fleet where some repos have a file and some do not.

## R-35 — a `defaults` block

```json
{ "version": 1,
  "defaults": { "on_zero_match": "declare" },
  "ops": [ "add_owner(/deploy/, @org/sre)",
           { "op": "add_owner(/docs/, @org/docs)", "on_zero_match": "skip" } ] }
```

- **R-35a** A default applies to every op that does not state its own value, so a
  40-op baseline is 40 strings rather than 40 objects. One caveat on that saving:
  `declare` never reaches `remove_owner` (R-35e), so a mixed baseline still spells
  its removals out as objects. `skip` does reach them.
- **R-35b** A per-op value wins over the default, and `check` echoes the resolved
  value so review sees what will run.
- **R-35c** `defaults` carries `on_zero_match` and `on_except_zero_match` only.
  `on_empty` stays top-level: it is one policy for the run, not a per-op setting, and
  two spellings for one setting is the ambiguity this document exists to remove.
- **R-35d** An unknown key inside `defaults` is exit 3, matching R-20's treatment of
  unknown top-level fields.
- **R-35e** A default is applied only to ops that can carry it. `on_zero_match` is
  rejected on `rename_owner` today; a policy with a default and a rename op must not
  fail for that reason — the default simply does not reach it. **This is value-level,
  not only field-level**: `declare` is refused on `remove_owner` (R-21) and on any
  except-carrying op (R-30), so a default of `declare` does not reach those either.
  The governing promise is that a default never causes an exit 3 that the same policy
  written out per-op would not have. The alternative — the default reaches them and
  the policy refuses — makes `defaults` unusable for exactly the mixed 40-op baseline
  it exists to serve.

## R-36 — a `lint` block

```json
{ "version": 1, "lint": { "remove_stale_paths": true, "on_empty": "inherit" },
  "ops": ["add_owner(/docs/, @org/docs-team)"] }
```

- **R-36a** `lint --policy p.json` takes its preferences from the block, so the
  repair policy is reviewed with the ownership policy in one artifact.
- **R-36b** A flag duplicating a block field alongside `--policy` is exit 3, per R-34b.
- **R-36c** `sync` ignores the `lint` block, and `lint` ignores `ops`. One committed
  file serves both commands; neither may fail because the other's section is present.
- **R-36d** An unknown key inside `lint` is exit 3.
- **R-36e** Every command validates the whole file, including the sections it does
  not act on: `sync` exits 3 on a malformed `lint` block and `lint` exits 3 on a
  malformed op. "Ignores" in R-36c means *does not act on*, never *does not
  validate*. One artifact is reviewed once and run everywhere, so a defect that only
  surfaces when someone happens to run the other command is precisely the
  fleet-scale failure the exit-3 class exists to prevent. Validation means the
  *whole* verdict, not just the loader's: `lint` runs the scope-compile and
  static-conflict checks on ops it will never apply, so all three verbs reach the
  same conclusion about the same bytes. A file that `check` calls broken must not
  be a file `lint` calls fine.

## R-37 — `except` as a JSON array

```json
{ "op": "add_owner(/.github/, @org/team-a)", "except": ["/.github/CODEOWNERS", "/.github/workflows/"] }
```

- **R-37a** An op object may carry `except` as an array of patterns, equivalent in
  every respect to the `<scope> except <pat> …` string spelling (R-26a).
- **R-37b** Both spellings on one op is exit 3. One intent, one place.
- **R-37c** The array needs no delimiter, so a pattern is a plain JSON string and the
  *delimiter's* escaping rule does not apply to it. A pattern containing a space is
  written `"my dir/"` and reaches the matcher as one pattern. Only whitespace is
  affected: `*`, `?` and `[` stay live pattern syntax, and a backslash stays the
  pattern language's own escape — an array of literal paths rather than patterns
  would be a different feature. It follows that a pre-escaped element, `"my\\ dir/"`,
  is **not** the same string and is refused; the refusal must say so in those terms,
  because a generator that escapes defensively is the likely author of it.
- **R-37d** An empty array is exit 3: state no `except`, not an empty one.
- **R-37e** Both spellings produce identical plans, identical bytes, and identical
  R-29/R-32 reporting.

## What does not change

The proof, the invariants, and the exit-code contract. Every requirement here is
about where a setting is written, or how many owners one line may name — none of them
reaches resolution or the S-invariants. Exit 3 stays reachable
only from facts independent of which repository you are in, which is why R-33c,
R-33d, R-34b, R-35d, R-36d, R-37b and R-37d are all exit 3 while nothing here adds a
new exit 2.

**One exception, found in review and stated rather than hidden.** `defaults` does
reach R-8's *statically provable* half. `ops.StaticConflict` skips any pair whose
scope is conditional — `on_zero_match` of `skip` or `declare` — because a scope that
may match nothing cannot be proven to overlap. So `defaults: {"on_zero_match":
"skip"}` suppresses that check for every op at once: a batch that `check` refuses at
exit 3 without the block is accepted with it, and the same batch is then refused per
repository at exit 2 by the tree-based check instead. No wrong bytes are written —
`plan.Build` still catches it — and a per-op `skip` has always done the same, so this
is the block inheriting an existing rule rather than a new hole. But it converts a
repo-0 defect into an N-repository one, which is the failure mode R-36e argues
against, and `check`'s echo reports only `skip` per op. **A reviewer reading a policy
with a `skip` or `declare` default should know the static R-8 verdict is weaker than
it looks.** Disclosing that in `check` is the natural follow-up; this document does
not yet require it.

## R-38 — one owner identity, everywhere

`@handles` are case-insensitive on GitHub. `lint` has always known this
(`foldOwner`), and R-33c now uses it to reject `[@Org/Team, @org/team]` as one owner
named twice. Matching does not use it, so the tool refuses that duplicate inside one
list and then writes the same duplicate across two ops:

```
file: /x/ @org/team
add_owner(/x/, @Org/Team)     → "applied", file becomes  /x/ @org/team @Org/Team
remove_owner(/x/, @ORG/TEAM)  → "unchanged", exit 0, and @org/team still owns /x/
```

The second line is the one that matters. A fleet run of "revoke the departed team"
reports **converged** on every repository that capitalised the handle.

- **R-38a One identity.** Two owner tokens are the same owner exactly when
  `ops.FoldOwner` says so: `@handles` compare case-insensitively, e-mail owners
  compare exactly. This identity governs *every* comparison of two owners — add,
  remove, rename's old name, `set_owners`, commutation, and resolution — with no
  site free to use its own.
- **R-38b Add is a no-op when the owner already owns**, whatever spelling either side
  used, and the line is left byte-identical. The file's existing spelling wins: this
  tool does not restyle a handle it was not asked to change, and rewriting it would
  churn a diff on every repository in a fleet.
- **R-38c Remove takes every spelling with it.** `remove_owner(/x/, @org/team)`
  against a hand-written `/x/ @org/team @Org/Team` leaves neither. The removal
  contract is that no named owner owns any in-scope path afterwards; a surviving
  spelling of the same owner breaks it.
- **R-38d Rename matches the old name under the same identity**, and writes the new
  name exactly as the op spells it — a rename is the one verb whose purpose is to
  change the text, so it is also the one place a spelling change is intended.
- **R-38e Idempotence holds across spellings.** Re-running any op with a differently
  cased handle is `unchanged` at exit 0 with a byte-identical file. This is the
  property that makes a fleet re-run safe, and it is the one the old behaviour broke
  most quietly.
- **R-38f Pre-existing duplicates are not silently repaired.** A hand-written line
  naming one owner twice is treated as that owner for every comparison, but `sync`
  does not rewrite the line to collapse it — repair is `lint`'s verb, and a `sync`
  that quietly edits what the policy did not name is the surprise this tool exists to
  avoid. Removing the owner does of course remove both, per R-38c.
