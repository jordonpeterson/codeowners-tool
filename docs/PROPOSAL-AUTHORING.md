# Proposal: simplify authoring a CODEOWNERS change

Status: proposal (no code changes). Scope: how a human *writes* the create and update
flows. The invariants, the proof step, and the exit-code contract are not in question.

## What the two flows cost today

Measured on a scratch repo (`README.md`, `docs/`, `services/api/`, `services/web/`,
`.github/workflows/`) against the current binary.

**Create.** A realistic bootstrap — a catch-all, two teams on `/services/api/`, a docs
team, two teams on workflows — is **six ops for four scopes**:

```json
"ops": [
  "add_owner(*, @org/everyone)",
  "add_owner(/services/api/, @org/api-team)",
  "add_owner(/services/api/, @org/platform)",
  "add_owner(/docs/, @org/docs-team)",
  { "op": "add_owner(/.github/workflows/, @org/ci)",       "on_zero_match": "declare" },
  { "op": "add_owner(/.github/workflows/, @org/security)", "on_zero_match": "declare" }
]
```

**Update.** Two teams co-owning one directory is two `--op` flags:

```sh
sync --op 'add_owner(/services/web/, @org/web-team)' --op 'add_owner(/services/web/, @org/sre)'
```

## Findings

**F1 — one owner per op.** `add_owner` and `remove_owner` take exactly one owner; the list
form is rejected outright (`add_owner takes a single owner, not a list`). N teams on one
scope is N ops. This is the single biggest source of repetition in both flows.

**F2 — the plan artifact shows a state that never exists.** Two adds on one scope produce
two hunks against the same line, the second undoing the first:

```
@ line 2
+/services/web/ @org/everyone @org/web-team
@ line 2
-/services/web/ @org/everyone @org/web-team
+/services/web/ @org/everyone @org/web-team @org/sre
```

`line_changes` reports 2 for one physical line. The reviewable artifact is the product, so
this is a correctness-of-presentation issue, not just verbosity.

**F3 — one scope per op.** One team owning five directories is five near-identical ops; a
scope list is not accepted.

**F4 — `on_zero_match` has no policy-level default.** It is per-op only, so a baseline
policy of 40 `declare` ops is 40 objects instead of 40 strings. The mirror is also blocked:
`on_empty` is top-level only. Neither field can be expressed at the other level.

**F5 — the baseline trap costs two invocations.** `set_owners(*, …)` batched with anything
narrower is refused at exit 3 (correctly — it does not commute). The remedy is two runs,
which means the fleet script carries two policy files for one reviewed intent. The README
flags this as the thing that "will bite you on the first try."

**F5 is now solved on main.** `except` (R-26…R-32) gives the batch a one-run spelling by
making the two ops disjoint rather than ordered — `set_owners(* except /docs/, [@org/everyone])`
alongside `add_owner(/docs/, @org/docs-team)` applies at exit 0, and re-runs clean. What
remains of F5 is only that you have to know to reach for it.

**F6 — a new file cannot be reviewed through `plan`/`apply`.** `plan` has no `--create` and
resolves the CODEOWNERS path from the ref's tree, so on a repo with none it exits 3. The
documented review path — plan, read the JSON, apply — is unavailable for exactly the flow
that creates the file; `sync --dry-run` is the only preview, and it produces no plan for
`apply`.

## Proposals

Ranked by value over effort. All are additive; every current policy stays valid.

### P1 — owner lists on `add_owner` / `remove_owner` (recommended, do first)

```json
"add_owner(/services/api/, [@org/api-team, @org/platform])"
```

The single-owner form stays legal. Fixes F1, and the bootstrap above drops from six ops to
four.

Semantics are unchanged: `add ∘ add` already commutes, so a list is the fold of the ops it
replaces — no new refusal cases, no invariant change. Two implementation depths:

- *Desugar at parse* — expand to N ops. Ships in hours, but keeps F2's double hunk.
- *Native in the planner* — one insert, one hunk, `line_changes` counts lines again.
  `plan.go` threads `op.Owner` as a single string through ~10 sites (`contains`, `append`,
  `InsertRule`); each becomes a set union.

Take the native route. F2 is the reason the op exists — the plan is what a reviewer reads,
and one intent should be one hunk.

`set_owners` already takes a list, so this also removes the asymmetry that makes people
reach for `set_owners` (displacing) when they meant `add_owner` (co-owning) — the mistake
the tool exists to prevent.

### P2 — scope lists

```json
"add_owner([/services/api/, /services/web/], @org/platform)"
```

Fixes F3. Pure sugar: each scope becomes an independent op at parse time, and the existing
R-8 machinery already handles overlapping scopes in a batch, so a list containing
overlapping scopes fails exactly as the expanded form does. No diff benefit (separate
scopes are separate lines), so desugaring is the right depth. Lower priority than P1.

### P3 — a `defaults` block in the policy

```json
{ "version": 1, "defaults": { "on_zero_match": "declare" }, "ops": [ … ] }
```

Fixes F4. A per-op value overrides it. This does not weaken the per-level strictness rule —
`defaults` is its own level with its own field set, which is the same argument the parser
already makes, rather than the merged bag it rejects.

The real risk is a default that is invisible on op 37. Mitigation: have `check --policy`
print the **effective** `on_zero_match` per op, so the reviewer never holds the default in
their head. Ship the echo in the same change, not after.

### P4 — `phases` for the baseline trap

```json
{ "version": 1, "phases": [ ["set_owners(*, [@org/everyone])"], ["add_owner(/services/api/, @org/api)"] ] }
```

Fixes F5: one reviewed artifact, N sequential batches, each internally commuting and each
proven as it is today. R-8 is preserved rather than relaxed — the ordering becomes something
the author states explicitly instead of something the error message tells them to encode in
bash. Mutually exclusive with `ops` at the top level.

Larger than P1–P3 (`sync` becomes a loop over batches, and the JSON record grows a phase
dimension). Defer to a second wave.

**Superseded — do not build.** `except` fixes F5 on main, and F5 was P4's whole motivation.
The one intent `except` cannot make disjoint is a rename chain (`@a→@b` then `@b→@c`), which
is inherently ordered; [PROPOSAL-POLICY-V2.md](PROPOSAL-POLICY-V2.md) proposes rejecting those
at load instead of sequencing them. Build `phases` only if that rejection proves too strict.

### P5 — `plan --create`

Fixes F6, so a new file gets the same reviewable artifact an update does. Small and
self-contained; worth folding into whichever wave has room.

### Considered and not recommended

**Generating a starter policy from the repo** (e.g. from `audit --checks a9`). The tool
deliberately never synthesizes ownership, and a guessed owner in a generated baseline is
the one output nobody can review. Leave it out.

## Recommendation

Ship **P1 (native) + P2 + P3** as one authoring release. Together they take the measured
bootstrap from six ops to four, take a 40-op baseline from 40 objects to 40 strings, take
the two-team update from two `--op` flags to one, and make the plan diff match the intent.
Nothing about the proof, the invariants, or the exit codes moves.

**P5** follows. **P4 is superseded by `except`** — see its section.

Each lands test-first, with the e2e case naming the requirement it enforces so
`docs/BEHAVIOR.md` picks it up: owner lists fold to the same file as the ops they replace
(and to one hunk), scope lists refuse exactly where the expanded form refuses, and a
`defaults` block plus a per-op override resolve to the value `check` echoes.
