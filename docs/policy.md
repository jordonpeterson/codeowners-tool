# The policy as the source of truth — specification

**Status: specified, not implemented.** Requirements R-33…R-37 are the target for
the e2e suites written against this document before the implementation exists
(CONTRIBUTING.md: the failing test comes first).

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
  40-op baseline is 40 strings rather than 40 objects.
- **R-35b** A per-op value wins over the default, and `check` echoes the resolved
  value so review sees what will run.
- **R-35c** `defaults` carries `on_zero_match` and `on_except_zero_match` only.
  `on_empty` stays top-level: it is one policy for the run, not a per-op setting, and
  two spellings for one setting is the ambiguity this document exists to remove.
- **R-35d** An unknown key inside `defaults` is exit 3, matching R-20's treatment of
  unknown top-level fields.
- **R-35e** A default is applied only to ops that can carry it. `on_zero_match` is
  rejected on `rename_owner` today; a policy with a default and a rename op must not
  fail for that reason — the default simply does not reach it.

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

## R-37 — `except` as a JSON array

```json
{ "op": "add_owner(/.github/, @org/team-a)", "except": ["/.github/CODEOWNERS", "/.github/workflows/"] }
```

- **R-37a** An op object may carry `except` as an array of patterns, equivalent in
  every respect to the `<scope> except <pat> …` string spelling (R-26a).
- **R-37b** Both spellings on one op is exit 3. One intent, one place.
- **R-37c** The array needs no delimiter, so a pattern is a plain JSON string and the
  escaping rules the string grammar imposes do not apply to it. A pattern containing
  a space is written `"my dir/"`, and reaches the matcher as one pattern.
- **R-37d** An empty array is exit 3: state no `except`, not an empty one.
- **R-37e** Both spellings produce identical plans, identical bytes, and identical
  R-29/R-32 reporting.

## What does not change

The proof, the invariants, and the exit-code contract. Every requirement here is
about where a setting is written, or how many owners one line may name — none of them
reaches resolution, R-8 commutativity, or the S-invariants. Exit 3 stays reachable
only from facts independent of which repository you are in, which is why R-33c,
R-33d, R-34b, R-35d, R-36d, R-37b and R-37d are all exit 3 while nothing here adds a
new exit 2.
