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

It also reads: `snapshot` tells you who owns what today, `audit` finds owners who've
left the company, rules that match no files, and owners who can't actually approve.
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
| See who owns what in this repo, right now | [The 60-second tour](#the-60-second-tour) |
| Understand `add_owner` vs `set_owners` before touching anything | [Basic concepts](#basic-concepts) |
| Follow worked end-to-end changes | [docs/GUIDE.md](docs/GUIDE.md) |
| **Lint** or **repair** a CODEOWNERS file, locally or in CI | [docs/LINTING.md](docs/LINTING.md) |
| Prove a merged PR changed only the owners it declared | [GUIDE.md#reviewing](docs/GUIDE.md#reviewing-the-change-before-it-lands) |
| Roll one policy out over many repos | [docs/FLEET.md](docs/FLEET.md) |
| Look up a flag, exit code, or JSON field | [docs/REFERENCE.md](docs/REFERENCE.md) |
| Look up a term | [docs/CONCEPTS.md](docs/CONCEPTS.md) |
| Know exactly what is guaranteed, and by which test | [docs/BEHAVIOR.md](docs/BEHAVIOR.md) |

## The 60-second tour

Every command is discoverable from the binary, and these are safe to run against
anything — none of them writes:

```sh
codeowners-tool --help       # every command and its flags
codeowners-tool snapshot     # who owns each tracked file, as JSON on stdout
codeowners-tool audit        # what's broken or rotten in the current file
```

`snapshot` is the one to reach for first, because it answers the question CODEOWNERS
itself doesn't:

```console
$ codeowners-tool snapshot | jq .ownership
{
  "README.md": ["@org/everyone"],
  "services/api/main.go": ["@org/api-team"],
  "services/web/app.ts": ["@org/everyone"]
}
```

In that map `[]` means a rule matches the path and deliberately assigns no owners;
`null` means no rule matches it at all. `snapshot` reads the CODEOWNERS **committed**
at `--branch` (default `HEAD`) — the file GitHub sees — so commit before snapshotting.
(Every command also has its own help: `codeowners-tool sync --help`.)

To change ownership, state the intent and preview it — nothing writes until you drop
`--dry-run`:

```console
$ codeowners-tool sync --op 'add_owner(README.md, @org/docs-team)' --dry-run
applied: 1 op(s) applied, 0 skipped; 1 line change(s), 1 path(s) change owners
  ops[0]  applied (proven: tree)
```

`proven: tree` means the claim was checked against the repo's real files, not just
reasoned about. Runs are idempotent, and every untouched byte survives: comments, blank
lines, spacing, ordering. [docs/GUIDE.md](docs/GUIDE.md) walks this change end to end.

## Basic concepts

**Ownership is a property of files, not lines.** `*.go @org/eng` is not a fact; the fact
is that `services/api/main.go` is owned by `@org/eng` — *unless* some later line also
matches it, in which case that line wins outright and `@org/eng` is simply gone. This
tool works in terms of the files, and treats the lines as an implementation detail it
derives for you.

**You state an intent — an *op*.** Same syntax whether you pass it with `--op` or list it
in a policy file. Scope is a directory, file path, or glob, using CODEOWNERS pattern
syntax; a space in a path is escaped with a backslash (`docs/release\ notes.md`).

| Op | What it means |
|---|---|
| `add_owner(scope, owner)` | Owner (or `[owners]`) becomes a **co-owner**. Every pre-existing owner of every path in scope is kept. |
| `set_owners(scope, [owners])` | This exact set owns every path in scope, displacing whoever owned it. `[]` is legal and deliberately un-owns the scope. |
| `remove_owner(scope, owner)` | Owner (or `[owners]`) stops owning every path in scope. If that would empty a rule, you must say what happens — see [`--on-empty`](docs/REFERENCE.md#--on-empty--on_empty-r-6). |
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
second one. (An op can also carve out sub-paths with an `except` clause — see
[REFERENCE.md#operations](docs/REFERENCE.md#operations).)

**Two invariants hold on every write**, or the write doesn't happen:

- **INV-1** — after the change, every path *in scope* is owned exactly as the op says.
- **INV-2** — after the change, every path *out of scope* is owned exactly as it was
  before. This is the product.

Anything it can't prove → it refuses and writes nothing. Refusing is a normal outcome
for some repos, not a bug — [GUIDE.md#when-it-refuses](docs/GUIDE.md#when-it-refuses)
shows what it looks like and what to do.

> **Pattern note.** `README.md` is unanchored, so like gitignore it matches a
> `README.md` at *any* depth. Write `/README.md` if you mean only the one at the root.

## Audit and lint

`audit` reports, `lint` repairs; both leave the file alone until you say otherwise.
Offline, `audit` checks the file against the git tree — dead patterns, case-only
mismatches, shadowed rules, unowned paths. With a token it also checks the owners
themselves: that they exist, are in the org, and have **explicit write access** (the
check that catches the most real rot).

**Exit 4 means findings, 0 means clean** — that's your CI gate. Exit 5 means a check
couldn't reach a conclusion: the audit **fails closed**, and never proposes a removal
it can't verify — an expired token quietly stripping owners is the worst thing this
tool could do, so it can't. Flags, the full check table, and every error it can print:
**[docs/LINTING.md](docs/LINTING.md)**.

## Many repos

Exit `2` means *this repo* needs a human. Exit `3` means *the policy* is broken and
will fail identically everywhere, so the fleet stops instead of recording it a hundred
times. `check --policy` validates a policy file without touching any repository —
run it before repo #1. The resumable rollout script and the `jq` habits that stop a
silent no-op looking like success: **[docs/FLEET.md](docs/FLEET.md)**.

## Where everything else is

- **[docs/GUIDE.md](docs/GUIDE.md)** — worked examples: bootstrap a file, modify one,
  review a change, understand a refusal.
- **[docs/REFERENCE.md](docs/REFERENCE.md)** — commands and flags, policy file fields,
  JSON output, exit codes, op semantics, the audit check table, GitHub semantics the
  tool encodes, design decisions, prior art.
- **[docs/LINTING.md](docs/LINTING.md)** — audit and repair: flags, exit codes, every
  error and what to do about it.
- **[docs/FLEET.md](docs/FLEET.md)** — rolling one policy across many repos.
- **[docs/CONCEPTS.md](docs/CONCEPTS.md)** — glossary, and habits that save you.
- **[docs/BEHAVIOR.md](docs/BEHAVIOR.md)** — generated from the test suite. Every
  `R-`, `S-`, `INV-` and `A-` identifier in these docs is a numbered requirement
  enforced by a named test, and this is where you look it up.
- **[docs/INSTALL.md](docs/INSTALL.md)** — every install route, provenance
  verification, GHES, upgrading, uninstalling.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — a few unusual rules, each with a reason.
- **[SECURITY.md](SECURITY.md)** — reporting vulnerabilities.
- **[CHANGELOG.md](CHANGELOG.md)** — what changed, release by release.

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
