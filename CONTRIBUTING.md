# Contributing

A few unusual rules, each with a reason. Please read before opening a PR.

## Hard constraints

**Zero dependencies.** `go.mod` lists none and there is no `go.sum`, so the
binary's supply chain is the Go toolchain's. Adding one needs an argument in the
PR description; "it's a small library" is not it, because size is not the cost.

**Tests are the specification.** `docs/BEHAVIOR.md` is generated from test doc
comments by `make docs`, and CI fails if the committed file differs. Write the
doc comment as the statement of behavior, then regenerate.

**Never weaken a test to make it pass.** If a test looks wrong, say so in the PR
and leave it failing. A deleted test is indistinguishable afterwards from one
that never existed.

**Write the failing test first**, then confirm it fails *for the reason you
think*. Vacuous passes have bitten people here: `policy.Error` renders the
filename first, so a fixture named `deep.json` makes any error "mention" depth,
and `t.TempDir()` embeds the test's own name in the path.

**Comments explain why, with the concrete failure.** A named scenario and its
consequence, not a restatement of the code. If a comment would only say what the
next line says, delete it.

## Before you push

```sh
make vet     # go vet + gofmt gate
make test    # or: go test -race ./...
make docs    # regenerate docs/BEHAVIOR.md; commit the result
```

`go test -race ./...` takes about five minutes (`internal/cli` alone is two). Let
it finish.

Touching the pattern matcher also means the differential fuzz against the
vendored `hmarr/codeowners` oracle:

```sh
make diff-test                        # 500k cases, seed 1 — what CI runs
go run ./tools/difftest 50000         # quicker while iterating
go run ./tools/difftest 500000 42     # a different region of the input space
```

CI runs the full 500k with the default seed on every PR, in its own parallel job.
Holding the seed fixed means raising the case count only extends the same
sequence, so pass a different one to reach input that gate never visits. The seed
prints with the result, so anything you find replays exactly.

## Exit codes are a contract

`internal/cli` documents seven; `sync`/`check` deliberately use a coarser
three-code contract. Scripts depend on the difference between "this repo needs a
human" (2) and "the policy is broken, stop the rollout" (3). Moving a failure
between classes is a breaking change even with an identical message — the rule is
that exit 3 is reachable only from facts independent of which repo you are in.

## Vendored code

`tools/difftest/oracle_hmarr.go` is `hmarr/codeowners`, verbatim and unmodified,
MIT © Harry Marr, attributed in `NOTICE`. It is the oracle the matcher is tested
against, so **do not edit it** — a change there hides a difference rather than
fixing one. `NOTICE` is legally load-bearing; leave it alone.

## Security

Do not open a public issue for a security problem. See [SECURITY.md](SECURITY.md).

## Commits and PRs

- One reviewable change per PR; keep unrelated cleanups out.
- Explain the failure the change prevents, not the code it adds.
- Signed commits are encouraged (`git config commit.gpgsign true`).
- By contributing you agree your work is licensed under the [MIT License](LICENSE).
