# codeowners-tool

Make safe, provable changes to GitHub CODEOWNERS files — in one repo, or across a
hundred.

**The problem.** CODEOWNERS is written in *lines*, but what anyone cares about is *who
owns which file*. The two are connected by rules that surprise people: the **last**
matching line wins, and owner sets don't combine — appending `/x/ @team-2` **replaces**
the owners of `/x/`, it doesn't add to them.

So you say what you want — "`@team-2` should co-own `/services/api/`" — and this tool
works out the lines. Then it checks its own work against every file in your repo, and
**refuses to write anything it can't prove correct.**

It also reads: `snapshot` tells you who owns what today, and `audit` finds owners who've
left the company, rules that match no files, and owners who don't actually have
permission to approve. Neither writes anything.

Works with github.com and GitHub Enterprise Server.

## Install

```sh
brew install jordonpeterson/tap/codeowners-tool
```

Every other route — the `curl | sh` script and its build-provenance verification, direct
download, `go install`, GHES notes, upgrading, uninstalling — is in
**[docs/INSTALL.md](docs/INSTALL.md)**.

Check it works:

```console
$ codeowners-tool version
```

## Start here

| You want to… | Go to |
|---|---|
| See who owns what in this repo, right now | [Find out what you have](#find-out-what-you-have) |
| Understand `add_owner` vs `set_owners` before touching anything | [Basic concepts](#basic-concepts) |
| Follow one small end-to-end change | [A basic example](#a-basic-example) |
| **Lint** a CODEOWNERS file, locally or in CI — and fix it | [How to: lint](#how-to-lint-a-codeowners-file) |
| **Write** a CODEOWNERS file for a repo that has none | [How to: write a new file](#how-to-write-a-new-codeowners-file) |
| **Modify** a CODEOWNERS file that already exists | [How to: modify an existing file](#how-to-modify-an-existing-codeowners-file) |
| Roll one policy out over many repos | [docs/FLEET.md](docs/FLEET.md) |
| Look up a flag, exit code, or JSON field | [docs/REFERENCE.md](docs/REFERENCE.md) |
| Know exactly what is guaranteed, and by which test | [docs/BEHAVIOR.md](docs/BEHAVIOR.md) |

## Find out what you have

Every command is discoverable from the binary, and the three read-only ones are safe to
run against anything:

```sh
codeowners-tool --help                 # every command and its flags
codeowners-tool snapshot               # who owns each tracked file, as JSON on stdout
codeowners-tool audit                  # what's broken or rotten in the current file
codeowners-tool check --policy p.json  # is this policy well-formed? (reads no repo)
```

`snapshot` is the one to reach for first, because it answers the question CODEOWNERS
itself doesn't:

```console
$ codeowners-tool snapshot | jq .ownership
{
  ".github/CODEOWNERS": ["@org/everyone"],
  "README.md": ["@org/everyone"],
  "docs/guide.md": ["@org/everyone"],
  "services/api/main.go": ["@org/api-team"],
  "services/web/app.ts": ["@org/everyone"]
}
```

Nothing below writes anything either until you drop `--dry-run`.

## Basic concepts

**Ownership is a property of files, not lines.** `*.go @org/eng` is not a fact; the fact
is that `services/api/main.go` is owned by `@org/eng` — *unless* some later line also
matches it, in which case that line wins outright and `@org/eng` is simply gone. This
tool works in terms of the files, and treats the lines as an implementation detail it
derives for you.

**You state an intent — an *op*.** Same syntax whether you pass it with `--op` or list it
in a policy file. Scope is a directory, file path, or glob, using CODEOWNERS pattern
syntax.

| Op | What it means |
|---|---|
| `add_owner(scope, owner)` | Owner becomes a **co-owner**. Every pre-existing owner of every path in scope is kept. |
| `set_owners(scope, [owners])` | This exact set owns every path in scope, displacing whoever owned it. `[]` is legal and deliberately un-owns the scope. |
| `remove_owner(scope, owner)` | Owner stops owning every path in scope. If that would empty a rule, you must say what happens — see [`--on-empty`](docs/REFERENCE.md#--on-empty--on_empty-r-6). |
| `rename_owner(old, new)` | Global identifier substitution — the only op that is safe as plain text replacement. |

`add_owner` and `set_owners` are the two you'll use most, and picking the wrong one is
the mistake the tool exists to prevent:

```console
$ codeowners-tool sync --op 'add_owner(/services/api/, @org/team-1)'
```
> `/services/api/ @org/api-team` becomes `/services/api/ @org/api-team @org/team-1`.
> **`@org/api-team` is still there.**

```console
$ codeowners-tool sync --op 'set_owners(/services/api/, [@org/team-1])'
```
> `/services/api/ @org/api-team` becomes `/services/api/ @org/team-1`.
> **`@org/api-team` no longer owns it** — which is what you asked for, explicitly.

Editing the file by hand, both of those look like the same one-line edit. That's the
trap: adding `/services/api/ @org/team-1` at the bottom of a file *silently* performs the
second one.

**Two invariants hold on every write**, or the write doesn't happen:

- **INV-1** — after the change, every path *in scope* is owned exactly as the op says.
- **INV-2** — after the change, every path *out of scope* is owned exactly as it was
  before. This is the product.

The tool synthesizes line edits, then re-resolves every file git knows about and compares
against an independently computed desired state. Anything it can't prove → it refuses and
writes nothing. Runs are idempotent, and every untouched byte survives: comments, blank
lines, spacing, ordering.

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

Docs team should co-own the README. Look before you leap:

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

Three things happened that are worth noticing.

`@org/everyone` was **carried onto the new line**. They owned `README.md` via `*`, and
`add_owner` means co-own, so the new rule has to restate them or they'd be dropped.

The line went **in the middle, not at the end**. Appending it would have placed it after
`/services/api/`, which is harmless here — but the general habit isn't, and putting it
directly after the rule it narrows is what keeps INV-2 true.

`/services/api/` was **not touched at all**, including its original spacing.

Run it again and nothing happens:

```console
$ codeowners-tool sync --op 'add_owner(README.md, @org/docs-team)'
unchanged: 0 op(s) applied, 0 skipped; 0 line change(s), 0 path(s) change owners
  ops[0]  unchanged (proven: tree)
```

> **Pattern note.** `README.md` is unanchored, so like gitignore it matches a `README.md`
> at *any* depth. Write `/README.md` if you mean only the one at the root.

## How to: lint a CODEOWNERS file

`audit` is the linter. On its own it is read-only — where a fix is expressible it prints
an op string for a human to run, and never applies one itself. Adding `--lint` is the one
mode that writes; that's [below](#fixing-what-it-finds-audit---lint).

```console
$ codeowners-tool audit
note: no token/--github-repo — running offline checks only (A-4..A-12)
[A-4/warning] (line 3) pattern "/nonexistent/" matches zero tracked files (report-only: may be deliberate, R-11)
[A-5/warning] (line 4) pattern "/Docs/" matches zero files ONLY because of case — CODEOWNERS is case-sensitive (S-6); "/docs/" would match
$ echo $?
4
```

**Exit 4 means findings, 0 means clean** — that's your CI gate. Exit 5 means the audit
couldn't reach a conclusion (see below), which you also want to fail on.

Offline it checks the file and the git tree: dead patterns, case-only mismatches, shadowed
and duplicate rules, syntax errors, unowned paths, more than one CODEOWNERS file, and the
3 MB cliff. Add a token and a repo and it also checks the owners themselves — that they
exist, are in the org, and have **explicit write access** (org membership is not enough,
and this is the check that catches the most real rot):

```console
$ GITHUB_TOKEN=... codeowners-tool audit --github-repo org/repo --format json
```

Two things to know before you wire it into CI.

**`audit` reads the CODEOWNERS committed at `--branch`** (default `HEAD`), not the copy in
your working directory — it is asking what GitHub would do, and GitHub only ever sees
committed files. An uncommitted edit will not show up. (`sync`, `plan` and `apply` are the
other way round: they read and write the working-tree file, and resolve ownership against
the ref's tree.)

**It fails closed.** A 404 from the API can mean deleted, renamed, invisible to your
token, or rate-limited. Anything inconclusive is reported as `unknown`, exits 5, and
**never proposes a removal** — an expired token quietly stripping owners is the worst
thing this tool could do, so it can't. Pin the exit code you gate on accordingly:

```yaml
- run: codeowners-tool audit --github-repo ${{ github.repository }}
  env:
    GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

The full check table (A-1 … A-12, which ones the API is needed for, which propose fixes)
is in [docs/REFERENCE.md](docs/REFERENCE.md#audit-checks). Run a subset with
`--checks a1,a3,a6`.

### Fixing what it finds: `audit --lint`

`--lint` repairs the whole file instead of only describing it. Three things, in this
order:

1. **Rejoins `@`handles that whitespace has split.** `/x/ @ org/team` looks like a rule
   with an owner. It isn't — GitHub can't parse the line, skips it entirely, and that team
   owns nothing. The handle is rejoined *before* anyone asks whether the team exists: `@`
   and `org/team` aren't two owners that don't exist, they're one owner nobody has looked
   up yet.
2. **Removes users and teams that don't exist.** A deleted or renamed team is a review
   request that silently goes nowhere.
3. **Removes rules that match no files** — only with `--remove-stale-paths`. Off by
   default, because a dead pattern is often deliberate, waiting on a directory that's
   coming, and deleting it destroys that intent. It takes you saying so.

```console
$ codeowners-tool audit --lint --github-repo org/repo --dry-run
lint: 2 fix(es) pending in .github/CODEOWNERS (--dry-run; nothing written)
  [repair-owner-spacing] (line 3) "/x/ @ org/api" → "/x/ @org/api"
  [remove-dead-owner] (line 5) @org/gone removed from "/y/": team @org/gone does not exist
  owners change: x/main.go  (unowned) → {@org/api}
$ echo $?
4
```

Drop `--dry-run` to write it. **Exit 4 means the file still needs a human** — fixes
pending under `--dry-run`, or a line lint refused to guess at — which makes
`--lint --dry-run` a CI gate that fails on rot rather than merely narrating it.

Three things to know:

- **It needs a token and `--github-repo`.** Whether an owner still exists isn't decidable
  offline, and that's the point of the mode. Without them it exits 5 and writes nothing.
- **It fails closed for the entire run.** If *any* owner lookup is inconclusive — rate
  limit, expired token, an org your token can't see — nothing is written, not even the
  offline whitespace fixes. Partial knowledge doesn't earn a partial edit.
- **It reads the working-tree file**, unlike plain `audit`, because that's the file it's
  about to rewrite. Ownership still resolves against `--branch`'s tree.

It is not a shortcut around the invariants: the edits are proven against every tracked
file first, then written through the same `apply` path as everything else — hash pinned,
validated, renamed atomically, rolled back on failure. If removing a dead owner would
leave a rule with no owners you have to say what happens, with
[`--on-empty`](docs/REFERENCE.md#--on-empty--on_empty-r-6). Lines it can't repair without
guessing are reported and left exactly as written.

There's a second kind of lint that has nothing to do with a repo. If you keep your ops in
a policy file, `check` validates the file itself:

```console
$ codeowners-tool check --policy policy.json
ok: policy.json — 3 op(s), no policy errors
```

```console
$ codeowners-tool check --policy bad.json
error: bad.json:1:21: ops[0]: op "add_owner(/services/api/)" is not valid: add_owner takes (scope, owner), got 1 args
error: bad.json:1:78: ops[1]: unknown field "on_zero_mtach" (did you mean "on_zero_match"?); an op accepts "op", "id", "on_zero_match", "note"
this is a policy error — it will fail identically on every repo; fix the policy, do not retry
$ echo $?
3
```

`check` reads no repository and writes nothing. It exits `0` or `3` and never `1`, so
under `set -e` a good policy always lets the script continue.

## How to: write a new CODEOWNERS file

For a repo with no CODEOWNERS at all, `--create` writes one at `.github/CODEOWNERS`.
It never overwrites an existing file, and it's off by default.

The smallest version is one op:

```console
$ codeowners-tool sync --op 'set_owners(*, [@org/everyone])' --create
applied: 1 op(s) applied, 0 skipped; 1 line change(s), 4 path(s) change owners
  ops[0]  applied (proven: tree)
  created a new CODEOWNERS file
```

For a real file, put the ops in a policy so the whole shape is reviewable in one diff:

```json
{
  "version": 1,
  "name": "bootstrap ownership",
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
$ codeowners-tool sync --policy bootstrap.json --create
applied: 4 op(s) applied, 0 skipped; 4 line change(s), 4 path(s) change owners
  ops[0]  applied (proven: tree)
  ops[1]  applied (proven: tree)
  ops[2]  applied (proven: tree)
  ops[3]  applied (proven: structural)
  created a new CODEOWNERS file
$ cat .github/CODEOWNERS
* @org/everyone
/docs/ @org/everyone @org/docs-team
/services/api/ @org/everyone @org/api-team
/.github/workflows/ @org/ci
```

Two things that will bite you on the first try.

**Use `add_owner` for the catch-all, not `set_owners`.** `add_owner` ops commute, so any
number of them can share one run. `set_owners(*, …)` overlaps every other scope and does
*not* commute with them, so the batch is refused rather than silently resolved by order:

```console
$ codeowners-tool sync --policy bootstrap.json --create
error: ops "set_owners(*, [@org/everyone])" and "add_owner(/services/api/, @org/api-team)"
overlap on "services/api/main.go" and do not commute (R-8: refusing order-dependent batch)
```

If you genuinely want a displacing baseline, run `set_owners(*, …)` on its own first and
the narrower ops in a second run.

**A rule for files that don't exist yet needs `on_zero_match: "declare"`.** The default is
`require`: a scope matching nothing is treated as a problem with this repo, because it
usually is a typo. `declare` says you meant it — the rule is written at the end of the
file, ready for files added later. That op reports `proven: structural` rather than
`proven: tree`: with no matching files there is nothing to check the rule against, so the
tool proves only that no later rule can override it. See
[what `declare` costs](docs/REFERENCE.md#what-declare-costs).

## How to: modify an existing CODEOWNERS file

Same command; the interesting part is what it protects you from. Starting from:

```
*            @org/everyone
/services/api/   @org/api-team
```

Each of these is one run against that file, and the second column is the line it leaves
behind. Notice that the original spacing survives every one of them.

| Run | `/services/api/` afterwards |
|---|---|
| `sync --op 'add_owner(/services/api/, @org/platform)'` | `/services/api/   @org/api-team @org/platform` |
| `sync --op 'set_owners(/services/api/, [@org/platform, @org/api-team])'` | `/services/api/   @org/platform @org/api-team` |
| `sync --op 'rename_owner(@org/api-team, @org/platform-api)'` | `/services/api/   @org/platform-api` |

The first is the common case and the one hand-editing gets wrong. The second is the same
edit stated deliberately. The third is what a reorg needs — it substitutes the identifier
everywhere it appears, and it's the only op that is safe as plain text replacement,
because it can't change any rule's match set.

**Removing an owner** is the one that stops and asks. If it would empty a rule's owner set
there is deliberately no default:

```console
$ codeowners-tool sync --op 'remove_owner(/services/api/, @org/api-team)'
error: removing @org/api-team empties the owner set of "/services/api/"; an explicit
--on-empty policy (error|inherit|unowned) is required — there is deliberately no default (R-6)
refused: 0 op(s) applied, 0 skipped; 0 line change(s), 0 path(s) change owners
$ echo $?
2
```

Say which you meant, and it proceeds — here `inherit` deletes the rule and lets the
preceding broader one take over:

```console
$ codeowners-tool sync --op 'remove_owner(/services/api/, @org/api-team)' --on-empty inherit
applied: 1 op(s) applied, 0 skipped; 1 line change(s), 1 path(s) change owners
  ops[0]  applied (proven: tree)
$ cat .github/CODEOWNERS
*            @org/everyone
```

`unowned` would instead keep the pattern with zero owners — GitHub's sanctioned substitute
for `!` negation — and `error` refuses outright. The recommendation is `error`.

### Reviewing the change before it lands

`sync` is plan-assert-apply-validate in one step. Split it in two when you want the
artifact in the middle — a JSON plan with resolved ownership per path and the literal line
diff:

```console
$ codeowners-tool plan --op 'add_owner(/services/web/, @org/web-team)' --out plan.json
plan written to plan.json
1 line change(s), 1 path(s) change owners, 64 → 107 bytes
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
applied: .github/CODEOWNERS (64 → 107 bytes)
```

Every change also carries the reason it took that shape, which is the part a reviewer
actually wants:

```
"rule \"*\" also governs out-of-scope paths; inserted narrowing rule \"/services/web/\"
 immediately after it so out-of-scope resolution is untouched (R-2)"
```

And to prove after the fact that a merged change moved nothing it didn't declare:

```sh
codeowners-tool snapshot --branch main    --out before.json
codeowners-tool snapshot --branch feature --out after.json
codeowners-tool verify --before before.json --after after.json --scope /services/api/
```

### When it refuses

Sometimes there is no line that does what you asked and nothing else. Given a repo with
`infra/main.tf` and `infra/README.md`, and a CODEOWNERS of exactly:

```
infra/ @org/infra-legacy
```

the tool says so and writes nothing:

```console
$ codeowners-tool sync --op 'add_owner(**/*.tf, @org/infra)'
error: refusing: rule "infra/" also governs paths outside scope "**/*.tf", and no sound
narrowing pattern is derivable — amending would violate INV-2, appending would violate INV-1
refused: 0 op(s) applied, 0 skipped; 0 line change(s), 0 path(s) change owners
$ echo $?
2
```

In English: `infra/` covers `infra/main.tf` *and* `infra/README.md`. Editing that line
would change the README's owners, which you never asked for (INV-2). Adding a `**/*.tf`
line before it would be overridden by it (INV-1). So it stops.

This is a normal outcome for some repos, not a bug — the tool fails closed rather than
guessing. The fix is usually to replace the over-broad rule the error names with narrower
ones, then re-run.

Exit `2` means *this repo* needs a human. Exit `3` means *the policy* is broken and will
fail identically everywhere. That split is what makes a hundred-repo run survivable —
see [docs/FLEET.md](docs/FLEET.md).

## Where everything else is

- **[docs/INSTALL.md](docs/INSTALL.md)** — every install route, provenance verification,
  GHES, upgrading, uninstalling.
- **[docs/REFERENCE.md](docs/REFERENCE.md)** — commands and flags, policy file fields,
  JSON output, exit codes, op semantics, the audit check table, GitHub semantics the tool
  encodes, design decisions, prior art.
- **[docs/FLEET.md](docs/FLEET.md)** — rolling one policy across many repos: a resumable
  script, `--format json`, and the `jq` habits that stop a silent no-op looking like
  success.
- **[docs/BEHAVIOR.md](docs/BEHAVIOR.md)** — generated from the test suite. Every `R-`,
  `S-`, `INV-` and `A-` identifier in these docs is a numbered requirement enforced by a
  named test, and this is where you look it up.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — a few unusual rules, each with a reason.

## Tests as documentation

The test suite is the specification. Every test carries a doc comment naming the
requirement it enforces, and `make docs` regenerates
[docs/BEHAVIOR.md](docs/BEHAVIOR.md) from those comments via `go/ast`, so the docs cannot
drift from what is verified. On top of the unit and end-to-end tests: a vendored pattern
corpus from [hmarr/codeowners](https://github.com/hmarr/codeowners), a 500k-case
differential fuzz against that matcher unmodified as an oracle, and property tests that
prove INV-1/INV-2 and idempotence by independent re-resolution.

```sh
make build     # ./bin/codeowners-tool
make all       # vet, test, build, docs
```

## Non-goals

GitLab/Bitbucket semantics · reordering or reformatting existing files · auto-deleting
rules that match zero files · inventing owners · resolving conflicting batches by
precedence · opening PRs or any git write · iterating over repos (that's your script's
job).

## License

MIT. The pattern matcher is ported from, and differentially tested against,
[hmarr/codeowners](https://github.com/hmarr/codeowners) (MIT, © Harry Marr) — see
[NOTICE](NOTICE).
