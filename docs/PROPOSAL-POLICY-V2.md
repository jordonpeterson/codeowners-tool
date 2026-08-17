# Proposal: what an easy-to-use policy file looks like

Companion to [PROPOSAL-AUTHORING.md](PROPOSAL-AUTHORING.md), which lists the friction. This
one answers the design question: if the JSON were shaped for a human, what shape is that?

Two constraints are non-negotiable, because they *are* the product:

1. **The verb stays visible.** Co-own vs displace is the mistake the tool exists to prevent,
   so no format may make `add` vs `set` implicit or inferable.
2. **Layout stays deterministic.** A hundred near-identical PRs are only reviewable if one
   input yields one byte sequence.

## What the measurement says about shape

Op order carries **layout only, never meaning**. The same four ops in reverse produce
different bytes — line order and owner-within-line order follow op order — but resolved
ownership is set-identical:

```
forward:  /services/api/ @org/everyone @org/api-team
reversed: /services/api/ @org/api-team @org/everyone     # same owners, GitHub sees no difference
```

So the ops array is not a sequence of steps — commutativity forbids that — it is an unordered
set of claims that happens to fix the layout. A **map keyed by scope** says exactly that, and
loses no determinism: the policy parser already keeps object members in source order, so
layout stays as reproducible as it is today.

## The shape

```json
{
  "version": 2,
  "name": "org baseline ownership",
  "on_empty": "error",

  "ownership": {
    "*":                   { "add": ["@org/everyone"] },
    "/services/api/":      { "add": ["@org/api-team", "@org/platform"] },
    "/docs/":              { "add": ["@org/docs-team"], "on_zero_match": "skip" },
    "/.github/workflows/": { "add": ["@org/ci", "@org/security"], "note": "release keys" },
    "/legacy/":            { "set": [] },
    "**/*.tf":             { "remove": ["@org/infra-legacy"] }
  },

  "rename": { "@org/acq-team": "@org/platform-api" }
}
```

Scope is the key, the verb is the field, the value is always a list. That is the whole format.

The same policy today is **nine ops, five of them objects**, because two teams on one scope is
two ops and anything carrying `on_zero_match` or a `note` must be wrapped:

```json
"ops": [
  "add_owner(*, @org/everyone)",
  "add_owner(/services/api/, @org/api-team)",
  "add_owner(/services/api/, @org/platform)",
  { "op": "add_owner(/docs/, @org/docs-team)",              "on_zero_match": "skip" },
  { "op": "add_owner(/.github/workflows/, @org/ci)",        "note": "release keys" },
  { "op": "add_owner(/.github/workflows/, @org/security)" },
  "set_owners(/legacy/, [])",
  "remove_owner(**/*.tf, @org/infra-legacy)",
  "rename_owner(@org/acq-team, @org/platform-api)"
]
```

A fleet baseline is where it compounds, because `defaults` absorbs the field that is otherwise
copied onto every entry:

```json
{
  "version": 2,
  "defaults": { "on_zero_match": "declare" },
  "ownership": {
    "/.github/workflows/": { "add": ["@org/ci"] },
    "/deploy/":            { "add": ["@org/sre"] },
    "**/*.tf":             { "add": ["@org/infra"] }
  }
}
```

Forty declared scopes: forty one-line entries, versus forty JSON objects today.

## What this deletes

**The embedded op language.** `ops.Parse`, `splitArgs`, `trimArg` and `checkScope`'s escaping
rules exist to parse a mini-DSL inside a format that already has a parser. Scope keys and owner
lists are JSON strings, so the escaping requirement goes with it — a directory with a space is
`"my dir/"` rather than `my\ dir/`, and the file writer escapes it on the way out, which is
where that concern belongs.

**The `id` field and its duplicate check.** The scope *is* the identity, so per-scope results
key on something stable instead of on `ops[N]`, a positional label that shifts when someone
inserts an entry above it. Fleet queries get better for free: `group_by(.scope)` survives an
edit to the policy, `group_by(.op)` does not.

**Two entries for one scope.** The map makes it impossible, which is precisely what the R-8
error already advises ("state one intent per scope"). `set` plus `add` on one scope is a
contradiction that can no longer be written; `add` plus `remove` is one entry whose two lists
must be disjoint — checked at load, so it lands on repo 0 rather than on whichever repo first
has both owners.

**A verified escape from `check`.** Two renames that chain — `@a→@b` and `@b→@c` — pass `check`
today and are caught only by tree-based R-8, at exit 2, *per repo*: a hundred identical
refusals for a defect that was in the policy the whole time.

```
$ check --op 'rename_owner(@org/a, @org/b)' --op 'rename_owner(@org/b, @org/c)'
ok: ops — 2 op(s), no policy errors                       # then exit 2 on every repo
```

As a map, a key that is also a value is a load-time error at exit 3. Same philosophy the
parser already argues for, one more case brought under it.

## Errors get located properly

Today a bad op is one string, so the position is the string's and the reason is nested:

```
policy.json:1:21: ops[0]: op "add_owner(/services/api/)" is not valid: add_owner takes (scope, owner), got 1 args
```

With fields, the position is the field's and the entry names itself:

```
policy.json:6:26: ownership["/services/api/"]: no verb; give the entry one of "add", "set", "remove"
```

## Alternatives considered

**Keyed by owner** — `{"@org/platform": {"add": ["/services/api/", "/services/web/"]}}`. Reads
well for "what does my team own", and it is the best shape for one team over many directories.
Rejected: an exact owner set is a property of a *scope*, so `set` cannot be expressed from the
owner side at all, and a format that can only co-own is not this tool.

**A bare list as shorthand for `add`** — `{"/docs/": ["@org/docs-team"]}`. Tempting, and the
implied verb would be the safe one. Rejected under constraint 1: the verb is the one choice
this product exists to make conscious, and one word is a fair price for keeping it on screen.

**A plain ownership map with no verbs** — `{"/docs/": ["@a"]}` meaning "these own it". This is
`set` semantics by default, i.e. silently destructive, which is the exact trap in the README's
opening paragraph. Rejected outright.

**Owner and scope lists on the existing op strings** (P1/P2 of the companion doc). Still the
right move if v2 is too much: it is additive, ships in one wave, and v2 subsumes it later.

## Compatibility

`version: 2` selects the map form and v1 files keep working untouched — this is what the
required version marker was for, and a pinned binary already reports "upgrade codeowners-tool"
rather than misreading it. `ops` and `ownership` are mutually exclusive in one file. Ship
`convert --policy v1.json` to emit the v2 equivalent, so nobody hand-migrates a 40-op baseline.

## Open questions for the team

- **Does `phases` (companion doc, P4) become an array of `ownership` blocks?** That is the
  natural v2 spelling for the `set_owners(*, …)` baseline trap, and it would settle whether
  v2 ships with or without it.
- **Is `defaults` allowed to carry `on_empty`?** It is top-level today, and per-scope override
  is a coherent thing to want the moment one removal wants `inherit` and another wants `error`.
- **Should `rename` keep insertion order?** A map is nicer to read and makes chains
  statically detectable; if ordered renames turn out to have a real use case, an array of pairs
  is the fallback.
