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

It also reads: `codeowners-tool audit` finds owners who've left the company, rules that
match no files, and owners who don't actually have permission to approve. Auditing
never writes anything.

Works with github.com and GitHub Enterprise Server.

## Install

```sh
brew install jordonpeterson/tap/codeowners-tool
```

Or, with Go 1.24+: `go install github.com/jordonpeterson/codeowners-tool/cmd/codeowners-tool@latest`

Prebuilt binaries, the `curl | sh` installer, and how to verify a download's checksum
and build provenance: **[docs/installation.md](docs/installation.md)**.

## Concepts

### Ops: say the intent, not the line

An **op** is one intent. Four exist; the whole design turns on the first two being
different:

| Op | Meaning |
|---|---|
| `add_owner(scope, owner)` | **Adds a co-owner.** Every pre-existing owner of every path in scope is kept. |
| `set_owners(scope, [owners])` | **Replaces** the owner set for every path in scope. `[]` deliberately un-owns it. |
| `remove_owner(scope, owner)` | Owner stops owning every path in scope. |
| `rename_owner(old, new)` | Global identifier substitution. |

Reach for `add_owner` unless you mean to displace people. Given:

```
*                 @org/everyone
/services/api/    @org/api-team
```

```console
$ codeowners-tool sync --op 'add_owner(/services/api/, @org/team-1)'
1 line changed, 12 paths change owners, 64 → 78 bytes
```

```
*                 @org/everyone
/services/api/    @org/api-team @org/team-1
```

`@org/api-team` is still there. Writing that line by hand — or using `set_owners` —
would have silently dropped them. Run it again and nothing happens; it's already true.
`--dry-run` shows the change without making it.

Scope is a directory, file path, or glob, in CODEOWNERS pattern syntax.

### `on_zero_match`: when nothing in the repo matches the scope

Your repos aren't identical, so each op says what to do when its scope matches no
tracked file:

| `on_zero_match` | What happens |
|---|---|
| `require` *(default)* | Treat it as a problem. This repo gets no changes and exits 2; your script records it and carries on. Use it for paths every repo really does have. |
| `skip` | Move on. "*If* this repo has Terraform, `@org/infra` owns it." |
| `declare` | Write the rule anyway, at the end of the file, ready for files added later. |

`declare` is how you get an identical baseline into every repo without editing it again
each time someone adds a file — at the cost of a weaker guarantee, explained in
[docs/guarantees.md](docs/guarantees.md#what-declare-costs). It's settable per op only
in a policy file, not on the command line.

### When the repo has no CODEOWNERS

By default that's a refusal (exit 2), not a crash — the tool won't create a file you
didn't ask for. Add `--create` and one is written at `.github/CODEOWNERS`:

```sh
codeowners-tool sync --op 'add_owner(/services/api/, @org/team-1)' --create
```

`--create` never overwrites an existing file, and `--file PATH` names a different
location. Discovery order is `.github/` > root > `docs/` — GitHub uses the first it
finds and never merges them. A created file goes through the same proof, syntax
validation and atomic write as an edit; `created: true` appears in the JSON record.

### Command line vs. policy file, text vs. JSON

Two independent choices — *where the ops come from*, and *what comes out*:

```sh
# ad hoc: ops on the command line, human-readable output
codeowners-tool sync --op 'add_owner(/services/api/, @org/api-team)'

# repeatable: ops in a file, machine-readable output, one JSON line per repo
codeowners-tool sync --policy policy.json --format json >> results.jsonl
```

`--op` and `--policy` are mutually exclusive. A policy file is the only way to attach
`on_zero_match`, `id` or `note` to individual ops, and the only form `check` can
validate ahead of a rollout:

```json
{
  "version": 1,
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    { "op": "add_owner(**/*.tf, @org/infra)", "on_zero_match": "skip" }
  ]
}
```

```sh
codeowners-tool check --policy policy.json   # reads no repo, writes nothing
```

Under `--format json`, stdout is data and stderr is logs. `--summary-out` additionally
writes a markdown summary suitable for a PR body.

### It refuses rather than guess

Every write is proven against two invariants: **INV-1**, the files you named end up
owned the way you asked; **INV-2**, every other file in the repo ends up exactly as it
was. The tool re-resolves every file git knows about and compares — anything it can't
prove is a refusal with nothing written, naming the rule that got in the way.

`sync` returns exactly three codes, and the split is the whole contract:

| Exit | Meaning | In a fleet script |
|---|---|---|
| 0 | Done — changed it, or it was already correct | continue |
| 2 | **This repo** needs a human | record it, continue |
| 3 | **The policy** is broken — it'll fail the same way everywhere | stop the run |

Other commands use a finer taxonomy ([exit codes](docs/cli.md#exit-codes)).

## Documentation

| Document | Read it for |
|---|---|
| [docs/installation.md](docs/installation.md) | Every install route; verifying checksums and build provenance |
| [docs/cli.md](docs/cli.md) | All commands and flags, policy file fields, JSON output schema, exit codes |
| [docs/guarantees.md](docs/guarantees.md) | The ops in full, INV-1/INV-2, what refusals mean, `declare` and `on_empty` |
| [docs/fleet.md](docs/fleet.md) | The resumable loop for rolling a policy across an org |
| [docs/audit.md](docs/audit.md) | The 12 audit checks and the fail-closed rules |
| [docs/design.md](docs/design.md) | GitHub semantics encoded (S-1..S-9), design decisions, prior art, non-goals |
| [docs/BEHAVIOR.md](docs/BEHAVIOR.md) | Generated from the test suite — the specification, as verified |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Hard constraints before opening a PR |

## License

MIT. The pattern matcher is ported from, and differentially tested against,
[hmarr/codeowners](https://github.com/hmarr/codeowners) (MIT, © Harry Marr) — see
NOTICE.
