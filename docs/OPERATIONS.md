# Reference: operations

What each op means, how to name several owners at once, and how `except` carves paths out
of a scope. The fields that carry these ops are in [POLICY-FILE.md](POLICY-FILE.md).

## Operations

Scope is a directory, file path, or glob — same syntax as CODEOWNERS patterns.

| Op | Meaning |
|---|---|
| `add_owner(scope, owner)` | Owner — or a bracketed list `[a, b]` (R-33), or an `owners` array (R-39) — becomes a **co-owner**; every pre-existing owner of every path in scope is retained. |
| `set_owners(scope, [owners])` | Exact owner set for every path in scope, displacing prior owners. `[]` is legal: it deliberately un-owns the scope. |
| `remove_owner(scope, owner)` | Owner — or a bracketed list (R-33), or an `owners` array (R-39) — stops owning every path in scope. If a rule's owner set would empty, an `--on-empty` policy is **required**. |
| `rename_owner(old, new)` | Global identifier substitution — the only op safe as pure text replacement (it can't change any rule's match set). |

### Writing a scope (escapes, and scopes that are refused)

An op string separates its arguments with commas and its `except` patterns with spaces, so
a scope containing either character escapes it with a backslash — the pattern language's
own escape, which stays in the text that is written to the file:

```
add_owner(/docs/release\ notes.md, @org/docs)   # a space in the path
add_owner(/a\,b/, @org/x)                       # a comma in the path
```

Two kinds of scope are refused with no repository open (exit 3, caught by `check`), because
the rule they would write is dead in **every** repo:

- one no path can ever match — `/`, `**/`, `**/**` — which even `on_zero_match: declare`
  would only write down as a line that owns nothing. `x/**/**`, `foo/**/` and `**/*.tf` are
  alive and unaffected;
- one starting with `#`, whose line reads back as a comment rather than as a rule — the
  same standard that refuses `!` and `\#` (S-2).

### Naming several owners in one op (R-33, R-39)

`add_owner` and `remove_owner` take a bracketed list wherever they take a single owner;
`set_owners` always took one. There are two spellings, and they are the same op:

```json
{ "version": 1,
  "ops": [ "add_owner(/services/api/, [@org/platform, @org/sre])",
           { "op": "add_owner(/docs/)", "owners": ["@org/platform", "@org/sre"] } ] }
```

The `owners` array (R-39) is for a generator that would rather emit JSON than build an op
string. It goes on an op naming **only** its scope, and is re-spelled as the bracketed list
before anything else sees it — so one grammar validates both, both are reported as the
list, and a refusal about an array quotes the list form it became.

Against a repo holding `services/api/m.go` and `docs/d.md`, and a CODEOWNERS of exactly
`/services/api/   @org/api-team`, both ops apply and both are reported:

```console
$ codeowners-tool sync --policy ownership.json
applied: 2 op(s) applied, 0 skipped; 2 line change(s), 2 path(s) change owners
  ops[0]  applied (proven: tree)
  ops[1]  applied (proven: tree)
$ cat CODEOWNERS
/docs/ @org/platform @org/sre
/services/api/   @org/api-team @org/platform @org/sre
```

**One intent, one hunk.** N owners on one scope produce **one** line change, not N, so the
plan never shows an intermediate state that is not on disk at any point. Order inside the
list fixes append order in the written line; it does not change resolved ownership.

These are exit 3, caught by `check` with no repository open: a duplicate owner in one list,
an empty list on `add_owner`/`remove_owner` (`set_owners` keeps it, still meaning "nobody
owns this"), a list or an `owners` array on `rename_owner` (it renames one owner to one
owner), owners in the op string **and** in an array at once (R-39b — one intent, one
place), and — per [R-38](#owner-identity-r-38) — two spellings of the same handle in one
list.

#### Why the list is not just shorter

For `add_owner` a list is convenience: those ops commute, so N single-owner ops in one run
produce the same file. For `remove_owner` under `on_empty: inherit` it is the **only**
spelling that gives the right answer. Given

```
/services/     @org/a @org/keep
/services/api/ @org/a @org/b
```

removing both `@org/a` and `@org/b` from `/services/api/` behaves three different ways:

| Spelling | Outcome for `services/api/main.go` |
|---|---|
| Two ops, one run | **Refused** (exit 2) — `inherit` makes their order-independence unprovable (R-8) |
| Two runs, in sequence | `{@org/a, @org/keep}` — exit 0, and **`@org/a` is back** |
| One list | `{@org/keep}` — correct |

The middle row is the trap: deleting the rule to let `/services/` take over re-grants
`@org/a` by inheritance, so a fleet run of "revoke the departed team" reports converged on
every repo while the owner you removed still owns the path. The list resolves the whole
removal at once, which is what `remove_owner` promises — after it, no named owner owns any
in-scope path.

### Owner identity (R-38)

`@handles` compare case-insensitively, e-mail owners compare exactly, and that one
identity governs every comparison: add, remove, rename's old name, `set_owners`, and
duplicate detection inside a list. So `add_owner(/x/, [@ORG/SRE])` against a file that
already says `@org/sre` is `unchanged` at exit 0 with the file byte-identical — **the
file's spelling wins**, because restyling a handle nobody asked to change would churn a
diff on every repository in a fleet. Conversely a removal takes every spelling with it.

### Two reachability models, on purpose

`remove_owner` is **path-scoped**: its scope is derived from tracked paths, so a rule whose
pattern matches nothing today — a `declare`d rule, or one for a directory that has since
gone — has no path to derive from and the op cannot reach it. `rename_owner` is **textual**
and substitutes in place, so it reaches that same rule.

This matters in a reorg, where both ops appear in one wave: renaming a team touches its
dormant rules, dissolving one does not. A run that leaves such a rule naming an owner it
was asked to remove now says so, naming the line, rather than reporting `unchanged` with
no warnings — otherwise a fleet grouped on `.status` reads that repo as already correct
while the owner keeps a claim that takes effect the moment a matching path appears. Clear
the line with `lint --remove-stale-paths`, or by hand.

### `except` — carving paths out of a scope (R-26…R-32)

An op's effective scope is `{tracked paths matching scope}` minus
`{tracked paths matching any except pattern}`. Excepted paths are simply **out of this
op's scope**: it neither grants nor revokes there. `except` is not a revocation — if the
grantee already owns an excepted path, that ownership persists and is reported. It is also
not a standing guarantee: `remove_owner` is the revoking verb, `audit` the watching one.

Both spellings are equivalent, and carrying both on one op is exit 3 (R-37b):

```
add_owner(/.github/ except /.github/CODEOWNERS /.github/workflows/, @org/team-a)
{ "op": "add_owner(/.github/, @org/team-a)", "except": ["/.github/CODEOWNERS"] }
```

In the string form the delimiter is unescaped whitespace, so a pattern containing a space
escapes it (`my\ dir/`); in the array form each element is a plain JSON string and a
pre-escaped one is refused. Flat subtraction only — no unions, no nesting, no
except-of-except — and `rename_owner` has no scope, so it takes no `except`.

These are policy errors (exit 3, caught by `check` with no repository): an except pattern
not **provably contained** in its op's scope, one whose language equals or contains the
scope (it would empty the op), two patterns of one op with equal languages, an `except`
keyword with no patterns, and `on_zero_match: declare` alongside `except` (R-30 — a
declared line cannot encode subtraction).

Per repo, the two emptiness questions are **ordered**: an empty effective scope is
disposed of by `on_zero_match` first, and `on_except_zero_match` is never consulted — an
op that writes nothing can reopen nothing. Only if the op will write does an except
pattern matching zero tracked files reach `on_except_zero_match`
([cost](GUARANTEES.md#what-on_except_zero_match-allow-costs)).

Where a written or amended line would otherwise govern an excepted path, the planner
synthesizes a **carve line** restating that path's current owners, placed immediately
after the line it corrects, and names the except clause in the `changes[]` reason. One
edge refuses rather than synthesizing: an excepted path that currently matches **no** rule
(exit 2). Unmatched and explicitly zero-owned are distinct states (S-9), and no writable
line restores "unmatched" once a broad line captures the path.

