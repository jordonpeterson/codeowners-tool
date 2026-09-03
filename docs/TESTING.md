# How this tool is tested

This tool rewrites the file that decides who reviews your code, so the interesting
question is not "are there tests" but "what is actually proven, and where can you check".
The 857 tests catalogued in [BEHAVIOR.md](BEHAVIOR.md) answer that. Contributor workflow —
what to run before pushing — is in [CONTRIBUTING.md](../CONTRIBUTING.md).

## The tests are the specification

Every test carries a doc comment naming the requirement it enforces, and `make docs`
regenerates [BEHAVIOR.md](BEHAVIOR.md) from those comments. CI fails if the committed file
differs from what the tests produce, so the documentation cannot drift from what is
verified: a behavior change that skips the doc comment fails the build.

That is why the `R-`, `S-`, `INV-` and `A-` identifiers scattered through these docs are
worth something. Each one resolves to a named test in BEHAVIOR.md, and following it takes
you to the assertion rather than to prose about the assertion.

The specification is also written **first**. Several end-to-end suites were committed
against a document before the implementation existed, failing for the stated reason, and
their doc comments say so — including the warning that a vacuous pass is the real risk
there, since a feature that does not exist yet fails with the same exit code the test
expects. `internal/cli` alone carries 41 test files, most driving the real binary against
real git repositories.

## The two invariants, proven by re-resolution

INV-1 (in scope) and INV-2 (out of scope) are defined in
[GUARANTEES.md](GUARANTEES.md#the-two-invariants), along with how the planner proves them:
re-resolving every tracked file at `--branch` and refusing anything unprovable.

`internal/plan/property_test.go` generates
thousands of `(file, tree, op)` combinations — directories, globs, owner sets, and all
three `on_empty` policies — and checks INV-1, INV-2 and idempotence on each against
expectations computed a second, independent way. Seeds are deterministic, so a failure
reproduces exactly.

Idempotence is proven the same way rather than assumed: applying a policy twice must
report `unchanged` at exit 0 and leave the file byte-identical. That property is what
makes re-running a hundred-repo rollout safe, and it is the one most easily broken by an
innocuous change to line synthesis.

## Differential fuzz against a reference matcher

Pattern matching is the part where being subtly wrong is invisible. CODEOWNERS patterns
are gitignore-*like* but not gitignore — no `!` negation, no `[ ]` ranges, brackets are
literals — and several tools in this space get it wrong by building on a gitignore engine
(see [prior art](GUARANTEES.md#prior-art)).

So the matcher is checked against [hmarr/codeowners](https://github.com/hmarr/codeowners),
vendored verbatim as an oracle:

```sh
make diff-test                        # 500k cases, seed 1 — exactly what CI runs
go run ./tools/difftest 50000         # quicker while iterating
go run ./tools/difftest 500000 42     # a different region of the input space
```

CI runs the full 500k on every PR in its own job. The seed is fixed deliberately: a gate
drawing new inputs each run would fail on PRs that changed nothing. Holding it fixed means
raising the case count only extends the same sequence, so pass a different seed to reach
input the gate never visits. The seed prints with the result, so anything you find
replays exactly.

The oracle is never edited — a change there would hide a difference rather than fix one.

## What CI gates

| Job | What it proves |
|---|---|
| `vet & test` | `go vet`, a `gofmt` check, and the full suite under `-race` |
| `docs are not stale` | `make docs` reproduces the committed BEHAVIOR.md |
| `differential fuzz vs hmarr oracle` | 500k cases, matcher against the reference |
| `lint` | `shellcheck` over the shipped scripts, `actionlint` over the workflows |

Beyond the tool itself, `tools/supplychain` tests the install path — that the script
verifies build provenance rather than only a checksum, that it degrades gracefully on a
machine without `gh`, that no workflow embeds a credential in a URL or holds a
cross-repository one — because a tool distributed by `curl | sh` is only as trustworthy as
that script. Some of those tests assert on the documentation itself, so a README that
stops linking to the install doc fails the build.

## Attribution

The pattern matcher is ported from, and differentially tested against,
[hmarr/codeowners](https://github.com/hmarr/codeowners) (MIT, © 2020 Harry Marr). Its test
corpus is vendored unmodified as `testdata/patterns.json`. Full terms: [NOTICE](../NOTICE).
