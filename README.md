# codeowners-tool

Make safe, provable changes to GitHub CODEOWNERS files — in one repo, or across a
hundred.

**The problem.** CODEOWNERS is written in *lines*, but what anyone cares about is *who
owns which file*. The two are connected by rules that surprise people: the **last**
matching line wins, and owner sets don't combine — appending `/x/ @team-2` **replaces**
the owners of `/x/`, it doesn't add to them.

So you write down the ownership you want — "`@org/platform` should co-own
`/services/api/`" — and this tool works out the lines. Then it checks its own work against
every file in your repo, and **refuses to write anything it can't prove correct.**

It also reads: `snapshot` tells you who owns what today, and `audit` finds departed
owners, dead rules, and owners who can't actually approve. Works with github.com and
GitHub Enterprise Server.

## Install

```sh
brew install jordonpeterson/tap/codeowners-tool
```

Every other route — the `curl | sh` script and its build-provenance verification, direct
download, `go install`, GHES notes, upgrading, uninstalling — is in
**[docs/INSTALL.md](docs/INSTALL.md)**.

## The policy file

**A policy is a JSON file stating the ownership you want.** It is the form to reach for:
it is reviewable as a diff, it is what the tool runs unchanged against one repo or a
hundred, and everything that changes what gets written lives inside it rather than in a
shell line nobody kept.

```json
{
  "version": 1,
  "name": "api co-ownership",
  "ops": [
    "add_owner(/services/api/, @org/platform)",
    "add_owner(/docs/, @org/docs-team)"
  ]
}
```

**Validate it before it touches anything.** `check` reads no repository at all, so it is
the cheapest way to catch a broken policy before repo #1:

```console
$ codeowners-tool check --policy ownership.json
ok: ownership.json — 2 op(s), no policy errors
```

**Preview, then write.** Nothing is written until you drop `--dry-run`:

```console
$ codeowners-tool sync --policy ownership.json --dry-run
applied: 2 op(s) applied, 0 skipped; 2 line change(s), 2 path(s) change owners
  ops[0]  applied (proven: tree)
  ops[1]  applied (proven: tree)
```

`proven: tree` means the claim was checked against the repo's real files, not just
reasoned about. Dropping `--dry-run` writes it, and the file becomes:

```console
$ cat .github/CODEOWNERS
*            @org/everyone
/docs/ @org/everyone @org/docs-team
/services/api/   @org/api-team @org/platform
```

Notice what happened. `@org/everyone` was **carried onto the new `/docs/` line** — they
owned it via `*`, and `add_owner` means co-own, so the new rule restates them or they'd be
dropped. The lines went **where they had to go**, each directly after the rule it narrows.
And `/services/api/` kept its original spacing. Runs are idempotent — a second `sync`
reports `unchanged`, and every untouched byte survives: comments, blank lines, ordering.

Beyond `ops`, a policy carries `create` (permission to write a CODEOWNERS where a repo has
none), `on_empty`, `max_paths_changed`, `defaults`, and a `lint` block — every field in
[REFERENCE.md#policy-file-fields](docs/REFERENCE.md#policy-file-fields).

> **One-off changes.** `--op 'add_owner(/docs/, @org/docs-team)'` runs a single op with no
> file. It is for exploring; anything you'd want reviewed, or run twice, belongs in a policy.

## Basic concepts

**Ownership is a property of files, not lines.** `*.go @org/eng` is not a fact; the fact
is that `services/api/main.go` is owned by `@org/eng` — *unless* some later line also
matches it, in which case that line wins outright and `@org/eng` is simply gone. This
tool works in terms of the files, and treats the lines as an implementation detail it
derives for you.

**Each entry in `ops` is one intent.** Scope is a directory, file path, or glob, using
CODEOWNERS pattern syntax; a space in a path is escaped with a backslash
(`docs/release\ notes.md`).

| Op | What it means |
|---|---|
| `add_owner(scope, owner)` | Owner (or `[owners]`) becomes a **co-owner**. Every pre-existing owner of every path in scope is kept. |
| `set_owners(scope, [owners])` | This exact set owns every path in scope, displacing whoever owned it. `[]` is legal and deliberately un-owns the scope. |
| `remove_owner(scope, owner)` | Owner (or `[owners]`) stops owning every path in scope. If that would empty a rule, you must say what happens — see [`on_empty`](docs/REFERENCE.md#--on-empty--on_empty-r-6). |
| `rename_owner(old, new)` | Global identifier substitution — the only op that is safe as plain text replacement. |

`add_owner` and `set_owners` are the two you'll use most, and picking the wrong one is
the mistake the tool exists to prevent. Against `/services/api/ @org/api-team`:

| The op in your policy | The line afterwards |
|---|---|
| `add_owner(/services/api/, @org/team-1)` | `/services/api/   @org/api-team @org/team-1` |
| `set_owners(/services/api/, [@org/team-1])` | `/services/api/   @org/team-1` |

**`@org/api-team` survives the first and is gone from the second** — which is what
`set_owners` was asked for, explicitly. Editing the file by hand, both look like the same
one-line edit. That's the trap: adding `/services/api/ @org/team-1` at the bottom of a
file *silently* performs the second one. (An op can also carve out sub-paths with an
`except` clause — see [REFERENCE.md#operations](docs/REFERENCE.md#operations).)

**Two invariants hold on every write**, or the write doesn't happen:

- **INV-1** — after the change, every path *in scope* is owned exactly as the op says.
- **INV-2** — after the change, every path *out of scope* is owned exactly as it was
  before. This is the product.

Anything it can't prove → it refuses and writes nothing. Refusing is a normal outcome
for some repos, not a bug — [GUIDE.md#when-it-refuses](docs/GUIDE.md#when-it-refuses)
shows what it looks like and what to do.

> **Pattern note.** `README.md` is unanchored, so like gitignore it matches a
> `README.md` at *any* depth. Write `/README.md` if you mean only the one at the root.

## Reading the current state

`snapshot` and `audit` write nothing and are safe to run against anything. Write your
policy against what they tell you, not against what the file looks like:

```console
$ codeowners-tool snapshot | jq .ownership
{
  "README.md": ["@org/everyone"],
  "services/api/main.go": ["@org/api-team"],
  "services/web/app.ts": ["@org/everyone"]
}
```

`snapshot` answers the question CODEOWNERS itself doesn't. In that map `[]` means a rule
matches the path and deliberately assigns no owners; `null` means no rule matches it at
all. It reads the CODEOWNERS **committed** at `--branch` (default `HEAD`) — the file
GitHub sees — so commit before snapshotting.

`audit` reports what's broken or rotten; `lint` repairs a subset of it. Offline, `audit`
checks against the git tree — dead patterns, case-only mismatches, shadowed rules,
unowned paths. With a token it also checks the owners themselves: that they exist, are in
the org, and have **explicit write access** (the check that catches the most real rot).

**Exit 4 means findings, 0 means clean** — that's your CI gate. Exit 5 means a check
couldn't reach a conclusion: the audit **fails closed** and never proposes a removal it
can't verify, because an expired token quietly stripping owners is the worst thing this
tool could do. Full check table: **[docs/LINTING.md](docs/LINTING.md)**.

## Many repos

One policy file, N repos, each converging on its own. Exit `2` means *this repo* needs a
human; exit `3` means *the policy* is broken and will fail identically everywhere, so the
fleet stops instead of recording it a hundred times — which is why `check --policy` is
worth running before repo #1. The resumable rollout script and the `jq` habits that stop
a silent no-op looking like success: **[docs/FLEET.md](docs/FLEET.md)**.

## Documentation

- **[GUIDE.md](docs/GUIDE.md)** — worked end-to-end changes: bootstrap a file, modify
  one, review a change, understand a refusal.
- **[REFERENCE.md](docs/REFERENCE.md)** — every flag, policy field, JSON field, exit
  code, op semantic and audit check; plus GitHub semantics the tool encodes.
- **[LINTING.md](docs/LINTING.md)** — audit and repair, and every error they can print.
- **[FLEET.md](docs/FLEET.md)** — rolling one policy across many repos.
- **[CONCEPTS.md](docs/CONCEPTS.md)** — glossary, and habits that save you.
- **[BEHAVIOR.md](docs/BEHAVIOR.md)** — generated from the test suite. Every `R-`, `S-`,
  `INV-` and `A-` identifier in these docs is a numbered requirement enforced by a named
  test, and this is where you look it up.
- **[INSTALL.md](docs/INSTALL.md)** — every install route, provenance verification, GHES.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** · **[SECURITY.md](SECURITY.md)** · **[CHANGELOG.md](CHANGELOG.md)**

## Tests as documentation

The test suite is the specification: every test carries a doc comment naming the
requirement it enforces, and `make docs` regenerates
[docs/BEHAVIOR.md](docs/BEHAVIOR.md) from those comments, so the docs cannot drift from
what is verified. On top of the unit and end-to-end tests: a 500k-case differential
fuzz against the [hmarr/codeowners](https://github.com/hmarr/codeowners) matcher as an
oracle, and property tests that prove INV-1/INV-2 and idempotence by independent
re-resolution.

## License

MIT. The pattern matcher is ported from, and differentially tested against,
[hmarr/codeowners](https://github.com/hmarr/codeowners) (MIT, © Harry Marr) — see
[NOTICE](NOTICE).
