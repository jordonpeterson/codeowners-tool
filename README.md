# codeowners-tool

Make safe, provable changes to GitHub CODEOWNERS files — in one repo, or across a
hundred. I have used this tool for rolling out programmatic codeowners changes at my 100+ repo regulated software company.
This enables us to
- easily give platform team control of platform files
- Give AI agent users access to self-approve specific files (with the required codeowners review setting)
I've written thousands of tests for edge cases to verify behavior.

**The problem.** CODEOWNERS is written in *lines*, but what anyone cares about is *who
owns which file*. The two are connected by rules that surprise people: the **last**
matching line wins, and owner sets don't combine — appending `/x/ @team-2` **replaces**
the owners of `/x/`, it doesn't add to them.

So you write down the ownership you want — "`@org/platform` should co-own
`/services/api/`" — and this tool works out the lines, checks its work against every file
in your repo, and **refuses to write anything it can't prove correct.** It also reads:
`snapshot` for who owns what today, `audit` for what has rotted. Works with github.com and
GitHub Enterprise Server.

## Install

```sh
brew install jordonpeterson/tap/codeowners-tool
```

In CI, `uses: jordonpeterson/codeowners-tool@v1`. Every other route — `curl | sh` with
build-provenance verification, direct download, `go install`, GHES, upgrading,
uninstalling — is in **[docs/INSTALL.md](docs/INSTALL.md)**.

## The policy file

**A policy is a JSON file stating the ownership you want.** It is reviewable as a diff, it
runs unchanged against one repo or a hundred, and everything that changes what gets written
lives inside it rather than in a shell line nobody kept.

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

**Validate, then preview.** `check` reads no repository at all — the cheapest way to catch
a broken policy before repo #1 — and nothing is written until you drop `--dry-run`:

```console
$ codeowners-tool check --policy ownership.json
ok: ownership.json — 2 op(s), no policy errors
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
dropped. Each line went **directly after the rule it narrows**, and `/services/api/` kept
its spacing. Runs are idempotent, and every untouched byte survives.

Beyond `ops`, a policy carries `create` (permission to write a CODEOWNERS where a repo has
none), `on_empty`, `max_paths_changed`, `defaults` and a `lint` block — every field in
[POLICY-FILE.md](docs/POLICY-FILE.md#policy-file-fields).

> **One-off changes.** `--op 'add_owner(/docs/, @org/docs-team)'` runs a single op with no file.
> It is for exploring; anything you'd want reviewed, or run twice, belongs in a policy.

## Basic concepts

**Ownership is a property of files, not lines.** `*.go @org/eng` is not a fact; the fact is
that `services/api/main.go` is owned by `@org/eng` — *unless* some later line also matches
it, in which case that line wins outright and `@org/eng` is simply gone. This tool works in
terms of files, and treats the lines as an implementation detail it derives for you.

**Each entry in `ops` is one intent.** Scope is a directory, file path, or glob in
CODEOWNERS pattern syntax; a space or a comma is escaped with a backslash
(`docs/release\ notes.md`, `/a\,b/`).
Where an op takes an owner it also takes a bracketed **list** —
`add_owner(/services/api/, [@org/platform, @org/sre])` — one line change rather than two, and
for `remove_owner` [the only always-correct spelling](docs/OPERATIONS.md#naming-several-owners-in-one-op-r-33-r-39).

| Op | What it means |
|---|---|
| `add_owner(scope, owner)` | Owner — or `[owners]`, or an `owners` array in the policy — becomes a **co-owner**. Every pre-existing owner of every path in scope is kept. |
| `set_owners(scope, [owners])` | This exact set — the list, or an `owners` array — owns every path in scope, displacing whoever owned it. `[]` is legal and deliberately un-owns the scope. |
| `remove_owner(scope, owner)` | Owner — or `[owners]`, or an `owners` array — stops owning every path in scope. If that would empty a rule, you must say what happens — see [`on_empty`](docs/POLICY-FILE.md#--on-empty--on_empty-r-6). |
| `rename_owner(old, new)` | Global identifier substitution — the only op that is safe as plain text replacement. |

`add_owner` and `set_owners` are the two you'll use most, and picking the wrong one is the
mistake this tool exists to prevent. Against `/services/api/ @org/api-team`:

| The op in your policy | The line afterwards |
|---|---|
| `add_owner(/services/api/, @org/team-1)` | `/services/api/   @org/api-team @org/team-1` |
| `set_owners(/services/api/, [@org/team-1])` | `/services/api/   @org/team-1` |

**`@org/api-team` survives the first and is gone from the second** — which is what
`set_owners` was asked for, explicitly. By hand both look like the same one-line edit, and
that's the trap: adding `/services/api/ @org/team-1` at the bottom of a file *silently*
performs the second. (Ops can also carve out sub-paths with an
[`except` clause](docs/OPERATIONS.md#except--carving-paths-out-of-a-scope-r-26r-32).)

**Two invariants hold on every write**, or the write doesn't happen:

- **INV-1** — every path *in scope* ends up owned exactly as the op says.
- **INV-2** — every path *out of scope* ends up owned exactly as it was. This is the product.

Anything it can't prove → it refuses and writes nothing. Refusing is a normal outcome for
some repos, not a bug — [GUIDE.md](docs/GUIDE.md#when-it-refuses) shows what to do.

> **Pattern note.** `README.md` is unanchored, so like gitignore it matches a `README.md`
> at *any* depth. Write `/README.md` if you mean only the one at the root.

## Reading the current state

`snapshot` and `audit` write nothing and are safe against anything. Write your policy
against what they tell you, not against what the file looks like:

```console
$ codeowners-tool snapshot | jq .ownership
{
  "services/api/main.go": ["@org/api-team"],
  "services/web/app.ts": ["@org/everyone"]
}
```

`snapshot` answers the question CODEOWNERS itself doesn't. `[]` means a rule matches and
deliberately assigns no owners, `null` that no rule matches at all. It reads the file
**committed** at `--branch` — what GitHub sees — so commit first.

`audit` reports what's broken or rotten and `lint` repairs a subset. Offline it checks the
git tree: dead patterns, case-only mismatches, shadowed rules, unowned paths. With a token
it also checks that owners exist, are in the org, and have **explicit write access** — the
check that catches the most real rot.

**Exit 4 means findings, 0 means clean** — your CI gate. Exit 5 means a check was
inconclusive: the audit **fails closed** and never proposes a removal it can't verify.
Full check table: **[docs/LINTING.md](docs/LINTING.md)**.

## Applying one policy to another repo

`--repo` points at any local clone, and the policy path stays relative to where you are —
so one reviewed artifact sits outside every repository it governs. The tool never clones;
that stays in your script.

```json
{ "version": 1, "name": "org baseline",
  "ops": [ { "op": "add_owner(/services/api/, @org/platform)", "on_zero_match": "skip" },
           { "op": "add_owner(/services/web/, @org/web-team)", "on_zero_match": "skip" } ] }
```

```console
$ codeowners-tool sync --repo clones/api-service --policy baseline.json
applied: 1 op(s) applied, 1 skipped; 1 line change(s), 1 path(s) change owners
  ops[0]  applied (proven: tree)
  ops[1]  skipped: scope "/services/web/" matches zero tracked files and on_zero_match=skip (R-21)
$ codeowners-tool sync --repo clones/web-app --policy baseline.json
applied: 1 op(s) applied, 1 skipped; 1 line change(s), 1 path(s) change owners
  ops[0]  skipped: scope "/services/api/" matches zero tracked files and on_zero_match=skip (R-21)
  ops[1]  applied (proven: tree)
```

Same bytes, two repos, each converging on what it actually has. `skip` means "*if* this
repo has it"; the default `require` treats a scope matching nothing as a problem with this
repo and exits `2` — what you want when every repo really should have the path. Exit `3`
means *the policy* is broken everywhere, so a fleet stops rather than recording it a
hundred times, which is why `check --policy` is worth running before repo #1. The rollout
script and the `jq` habits that stop a silent no-op looking like success:
**[docs/FLEET.md](docs/FLEET.md)**.

## Documentation

- **[GUIDE.md](docs/GUIDE.md)** — worked end-to-end changes: bootstrap a file, modify one,
  review a change, understand a refusal.
- **[REFERENCE.md](docs/REFERENCE.md)** — the lookup tables: flags, policy fields, op
  semantics, JSON fields, exit codes, audit checks, guarantees.
- **[LINTING.md](docs/LINTING.md)** — audit and repair, and every error they can print.
- **[FLEET.md](docs/FLEET.md)** — rolling one policy across many repos.
- **[CONCEPTS.md](docs/CONCEPTS.md)** — glossary, and habits that save you.
- **[BEHAVIOR.md](docs/BEHAVIOR.md)** — generated from the tests; every `R-`, `S-`, `INV-` and `A-` id is looked up here.
- **[TESTING.md](docs/TESTING.md)** — what the suite proves; **[INSTALL.md](docs/INSTALL.md)** — every install route, provenance, GHES.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** · **[SECURITY.md](SECURITY.md)** · **[CHANGELOG.md](CHANGELOG.md)**

## Tests as documentation

The test suite is the specification and BEHAVIOR.md is generated from it — what that
proves, and how: **[docs/TESTING.md](docs/TESTING.md)**.

## License

MIT — see **[LICENSE](LICENSE)**, and **[NOTICE](NOTICE)** for vendored-code attribution.
