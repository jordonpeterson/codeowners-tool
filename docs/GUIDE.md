# Guide: making changes end to end

Worked examples for each kind of change. Concepts: [README](../README.md#basic-concepts);
every flag and exit code: [REFERENCE.md](REFERENCE.md).

## A basic example

A small repo, with a `README.md` nobody owns in particular:

```
README.md
docs/guide.md
services/api/main.go
services/web/app.ts
```

```console
$ cat .github/CODEOWNERS
*            @org/everyone
/services/api/   @org/api-team
```

Docs team should co-own the README — look before you leap:
```console
$ codeowners-tool sync --op 'add_owner(README.md, @org/docs-team)' --dry-run
applied: 1 op(s) applied, 0 skipped; 1 line change(s), 1 path(s) change owners
  ops[0]  applied (proven: tree)
```

`proven: tree` means the claim was checked against the repo's real files, not just
reasoned about. Drop `--dry-run` to write it:

```console
$ codeowners-tool sync --op 'add_owner(README.md, @org/docs-team)'
applied: 1 op(s) applied, 0 skipped; 1 line change(s), 1 path(s) change owners
  ops[0]  applied (proven: tree)
$ cat .github/CODEOWNERS
*            @org/everyone
README.md @org/everyone @org/docs-team
/services/api/   @org/api-team
```

Three things happened that are worth noticing:

- `@org/everyone` was **carried onto the new line** — they owned `README.md` via `*`,
  and `add_owner` means co-own, so the new rule restates them or they'd be dropped.
- The line went **in the middle, not at the end** — directly after the rule it narrows,
  which is what keeps out-of-scope ownership (INV-2) untouched.
- `/services/api/` was **not touched at all**, including its original spacing.

Run it again and nothing happens: the second run reports `unchanged` and changes
zero bytes.

> **Pattern note.** `README.md` is unanchored, so like gitignore it matches a
> `README.md` at *any* depth. Write `/README.md` if you mean only the one at the root.

## Writing a new CODEOWNERS file

For a repo with no CODEOWNERS at all, `--create` grants permission to write one at
`.github/CODEOWNERS`. It never overwrites an existing file, it's off by default, and a
run with nothing to write creates nothing. The smallest version is one op:

```console
$ codeowners-tool sync --op 'set_owners(*, [@org/everyone])' --create
applied: 1 op(s) applied, 0 skipped; 1 line change(s), 4 path(s) change owners
  ops[0]  applied (proven: tree)
  created a new CODEOWNERS file
```

For a real file, put the ops in a policy so the whole shape is reviewable in one diff
(a policy run states `"create": true` in the file instead of the flag —
[why](REFERENCE.md#creating-a-file-r-23-and-not-creating-one)):

```json
{
  "version": 1,
  "name": "bootstrap ownership",
  "create": true,
  "ops": [
    "add_owner(*, @org/everyone)",
    "add_owner(/services/api/, @org/api-team)",
    "add_owner(/docs/, @org/docs-team)",
    { "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare" }
  ]
}
```

```console
$ codeowners-tool check --policy bootstrap.json
ok: bootstrap.json — 4 op(s), no policy errors
$ codeowners-tool sync --policy bootstrap.json
applied: 4 op(s) applied, 0 skipped; 4 line change(s), 4 path(s) change owners
  created a new CODEOWNERS file
```

Two things that will bite you on the first try:

- **Use `add_owner` for the catch-all, not `set_owners`.** `add_owner` ops commute, so
  any number can share one run. `set_owners(*, …)` overlaps every other scope and does
  not commute with them, so the batch is refused at exit 3 (R-8) — run it on its own
  first, previewed with `--dry-run`, since it *replaces* the owners of everything.
- **A rule for files that don't exist yet needs `on_zero_match: "declare"`.** The
  default `require` treats a scope matching nothing as a problem with this repo,
  because it usually is a typo. `declare` writes the rule at the end of the file for
  files added later, and reports `proven: structural` — see
  [what `declare` costs](REFERENCE.md#what-declare-costs).

## Modifying an existing file

Same command; the interesting part is what it protects you from. Starting from the
two-line file above, each row is one run, and the second column is the line it leaves
behind — original spacing intact:

| Run | `/services/api/` afterwards |
|---|---|
| `sync --op 'add_owner(/services/api/, @org/platform)'` | `/services/api/   @org/api-team @org/platform` |
| `sync --op 'set_owners(/services/api/, [@org/platform, @org/api-team])'` | `/services/api/   @org/platform @org/api-team` |
| `sync --op 'rename_owner(@org/api-team, @org/platform-api)'` | `/services/api/   @org/platform-api` |

The first is the common case and the one hand-editing gets wrong. The second is the
same edit stated deliberately. The third is what a reorg needs — a global identifier
substitution that can't change any rule's match set.

**Removing an owner** stops and asks when it would empty a rule's owner set — there is
deliberately no default:

```console
$ codeowners-tool sync --op 'remove_owner(/services/api/, @org/api-team)'
error: removing @org/api-team empties the owner set of "/services/api/"; an explicit --on-empty policy (error|inherit|unowned) is required — there is deliberately no default (R-6) (governing file: .github/CODEOWNERS)
```

`--on-empty inherit` deletes the rule and lets the preceding broader one take over;
`unowned` keeps the pattern with zero owners (GitHub's sanctioned substitute for `!`
negation); `error` refuses outright, and is the recommendation.

## Reviewing the change before it lands

`sync` is plan-assert-apply-validate in one step. Split it when you want the artifact
in the middle — a JSON plan with resolved ownership per path and the literal line diff:

```console
$ codeowners-tool plan --op 'add_owner(/services/web/, @org/web-team)' --out plan.json
plan written to plan.json
1 line change(s), 1 path(s) change owners, 58 → 101 bytes
$ jq '.ownership_rows, .diff' plan.json
[
  {
    "path": "services/web/app.ts",
    "owners_before": ["@org/everyone"],
    "owners_after": ["@org/everyone", "@org/web-team"]
  }
]
"@ line 2\n+/services/web/ @org/everyone @org/web-team\n"
$ codeowners-tool apply --plan plan.json
applied: .github/CODEOWNERS (58 → 101 bytes)
```

Every change carries the reason it took that shape — the part a reviewer actually
wants. And to prove after the fact that a merged change moved nothing it didn't
declare:

```sh
codeowners-tool snapshot --branch main    --out before.json
codeowners-tool snapshot --branch feature --out after.json
codeowners-tool verify --before before.json --after after.json --scope /services/api/
```

Two hygiene rules make that proof trustworthy. `snapshot` reads the **committed**
CODEOWNERS at `--branch` (default `HEAD`) — so commit the change before taking the
"after" snapshot. And leave the evidence files themselves uncommitted: a path that
enters the tracked tree counts as an ownership change, so `verify` will rightly flag
your own `before.json` as out of scope.

## When it refuses

Sometimes there is no line that does what you asked and nothing else. Given
`infra/main.tf` and `infra/README.md`, and a CODEOWNERS of exactly `infra/
@org/infra-legacy`:

```console
$ codeowners-tool sync --op 'add_owner(**/*.tf, @org/infra)'
error: refusing: rule "infra/" also governs paths outside scope "**/*.tf", and no sound narrowing pattern is derivable — amending would violate INV-2, appending would violate INV-1 (governing file: .github/CODEOWNERS)
```

In English: `infra/` covers the `.tf` file *and* the README. Editing that line would
change the README's owners, which you never asked for (INV-2). Adding a `**/*.tf` line
before it would be overridden by it (INV-1). So it stops.

This is a normal outcome for some repos, not a bug — the tool fails closed rather than
guessing. Two ways out, both stated in the op itself:

- **Narrow the scope** to something a sound rule *can* be written for — the concrete
  paths, or a directory-local glob: `add_owner(/infra/*.tf, @org/infra)`.
- **Carve the conflicting paths out** with an `except` clause:
  `add_owner(infra/ except infra/README.md, @org/infra)` — see [except.md](except.md).

(A same-owners `set_owners` does not help: it changes no ownership, so the tool reports
`unchanged` and writes nothing.) Exit `2` means *this repo* needs a human; exit `3` means
*the policy* is broken and will fail identically everywhere — the split that makes a
hundred-repo run survivable ([FLEET.md](FLEET.md)).
