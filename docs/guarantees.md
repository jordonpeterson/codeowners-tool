# Guarantees, refusals, and ops

## Operations (mutation)

Scope is a directory, file path, or glob — same syntax as CODEOWNERS patterns.

| Op | Meaning |
|---|---|
| `add_owner(scope, owner)` | Owner becomes a **co-owner**; every pre-existing owner of every path in scope is retained. |
| `set_owners(scope, [owners])` | Exact owner set for every path in scope, displacing prior owners. `[]` is legal: it deliberately un-owns the scope. |
| `remove_owner(scope, owner)` | Owner stops owning every path in scope. If a rule's owner set would empty, an `--on-empty` policy is **required** (see below). |
| `rename_owner(old, new)` | Global identifier substitution — the only op safe as pure text replacement (it can't change any rule's match set). |

## The two invariants

- **INV-1 (in scope):** after apply, every in-scope path resolves to exactly what the
  op requires.
- **INV-2 (out of scope):** after apply, every out-of-scope path resolves to exactly
  what it did before. **This is the product.**

The planner synthesizes line edits, then *proves* the result by re-resolving every file
git knows about at `--branch` and comparing against an independently computed desired
state. Anything unprovable → refusal, nothing written. Plans are idempotent (re-running
is a no-op) and preserve every untouched byte — comments, blank lines, spacing,
ordering.

## When it can't do what you asked

It refuses, tells you which rule was in the way, and **writes nothing**:

```console
$ codeowners-tool sync --op 'add_owner(/services/api/, @org/team-1)'
error: refusing: rule "*" also governs paths outside scope "/services/api/", and no
sound narrowing pattern is derivable — amending would violate INV-2, appending would
violate INV-1
$ echo $?
2
```

In English: a later `*` rule already covers everything, so any line added for
`/services/api/` would just get overridden (breaking INV-1) — and reordering the file
to fix that would move files you never mentioned (breaking INV-2). There is no line the
tool can write that does what you asked and nothing else, so it writes none. That's a
normal, expected outcome for some repos; the tool *fails closed* and would rather stop
than guess.

Across a fleet that means your script records the handful that stopped and carries on —
see [fleet.md](fleet.md) and the [exit codes](cli.md#exit-codes).

## What `declare` costs

`declare` is the one place a guarantee weakens:

- **INV-2 is unaffected.** A pattern that matches nothing in the repo cannot move any
  existing file's ownership. Proven exactly as usual.
- **INV-1 weakens.** Your repo has no files matching the pattern, so there is nothing
  to check the rule against — the tool cannot prove the rule does what you meant. It
  proves the next best thing: that no rule after it can override it, which it
  guarantees by putting the rule at the end of the file. When someone later adds a
  matching file, this rule takes it. If that wasn't what you wanted, nothing will have
  caught it.

Ops that took this path report `"proven": "structural"` in the JSON and are called out
in `--summary-out`, so a reviewer can find them without reading the diff.

## What `on_except_zero_match: allow` costs

The same class of weakening, reachable from `except` ([except.md](except.md), R-27): an
except pattern that matches nothing tracked means the carve-out the policy promises does
not exist in this repo. The default (`require`) refuses the repo. `allow` writes the
grant with **no carve line** for the unmatched pattern, so a matching file created later
falls under the grant — nothing in the repo today can verify the carve-out you asked
for. Exactly like `declare`: INV-2 is unaffected, INV-1 is only structural, the op
reports `"proven": "structural"`, the inert pattern is listed in `except_unmatched`, and
a warning is emitted. No dead rule is written for the unmatched pattern (R-5).

## `--on-empty` / `on_empty` (R-6)

Removing the sole owner of a rule needs an explicit policy — **there is no default**,
and the documented recommendation is `error`:

- `error` — refuse (recommended: consistent with the tool's fail-closed posture)
- `inherit` — delete the rule; the preceding broader rule takes over (removal
  **cascades** if the fallthrough rule also lists the owner)
- `unowned` — keep the pattern with zero owners (GitHub's sanctioned substitute for `!`
  negation)

Under `inherit`/`unowned` the resulting reassignment is shown in the plan's ownership
rows.
