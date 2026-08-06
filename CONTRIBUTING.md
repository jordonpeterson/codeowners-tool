# Contributing

Thanks for looking. This project has a few unusual rules, and they exist for
reasons — please read this before opening a PR.

## The hard constraints

**Zero dependencies.** `go.mod` lists no requirements and there is no `go.sum`,
so the supply chain of the binary is the supply chain of the Go toolchain and
nothing else. A PR that adds a dependency needs to argue for it in the
description; "it is only a small library" is not the argument, because the cost
being paid is not size.

**Tests are the specification.** `docs/BEHAVIOR.md` is generated from test doc
comments by `make docs`, and CI fails if the committed file differs from what the
tests produce. Write the doc comment on the test as the statement of behavior,
then regenerate.

**Never weaken a test to make it pass.** If a test looks wrong, say so in the PR
and leave it failing. A test that was deleted to go green is indistinguishable
afterwards from one that never existed.

**Write the failing test first.** Then confirm it fails *for the reason you
think*. This codebase has bitten people with vacuous passes: `policy.Error`
renders the filename first, so a fixture named `deep.json` makes any error
"mention" depth, and `t.TempDir()` embeds the test's own name in the path.

**Comments explain why, with the concrete failure.** The style throughout is a
named scenario and its consequence — "a fleet loop pointed at 100 clones writes
100 files into whatever happens to sit next to them" — not a restatement of the
code. If a comment would only say what the next line says, delete it.

## Before you push

```sh
make vet     # go vet + gofmt gate
make test    # or: go test -race ./...
make docs    # regenerate docs/BEHAVIOR.md; commit the result
```

`go test -race ./...` takes about five minutes; `internal/cli` alone is about
two. Let it finish.

For a change to the pattern matcher, also run the differential fuzz against the
vendored `hmarr/codeowners` oracle:

```sh
make diff-test                        # 500k cases, seed 1 — what CI runs on every PR
go run ./tools/difftest 50000         # quicker while iterating
go run ./tools/difftest 500000 42     # a different region of the input space
```

CI runs the full 500k with the default seed on every PR, in its own parallel job
(about 35 seconds, so it never sits on the critical path). Same count, same seed
as `make diff-test` — a green run here is a green gate there.

The seed matters when you touch the matcher: holding it fixed means raising the
case count only extends one sequence, so it re-derives what CI already proved.
Pass a different seed to explore somewhere new. The seed is printed with the
result, so anything you find replays exactly.

## Exit codes are a contract

`internal/cli` documents seven exit codes, and `sync`/`check` deliberately use a
coarser three-code contract of their own. Scripts depend on the difference
between "this repo needs a human" (2) and "the policy is broken, stop the
rollout" (3). Changing which class a failure lands in is a breaking change even
when the message is identical — the rule is that exit 3 is reachable only from
facts that do not depend on which repository you are standing in.

## Vendored code

`tools/difftest/oracle_hmarr.go` is `hmarr/codeowners`, vendored verbatim and
unmodified, MIT © Harry Marr, and attributed in `NOTICE`. It is the oracle the
matcher is tested against, so **do not edit it** — a change there does not fix a
difference, it hides one. `NOTICE` is legally load-bearing; leave it alone.

## Security

Do not open a public issue for a security problem. See [SECURITY.md](SECURITY.md).

## Commits and PRs

- One reviewable change per PR; keep unrelated cleanups out of it.
- Explain the failure the change prevents, not the code it adds.
- Signed commits are encouraged (`git config commit.gpgsign true`).
- By contributing you agree your work is licensed under the [MIT License](LICENSE).
