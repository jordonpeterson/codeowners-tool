# Design notes

## GitHub semantics this tool encodes

| # | Property |
|---|---|
| S-1 | Last matching rule wins; owner sets do not union |
| S-2 | Patterns are gitignore-*like*: no `!` negation, no `[ ]` ranges, no `\#` escape; brackets are literals |
| S-3 | Invalid lines are **skipped individually** (current GitHub behavior — older docs claimed the whole file was disabled; this tool implements and tests the current per-line semantics, and still refuses to *write* new invalid lines) |
| S-4 | Files over 3 MB are silently not loaded at all |
| S-5 | Owners need **explicit write access**; org membership is not enough |
| S-6 | Paths are case-sensitive regardless of local filesystem |
| S-7 | The PR **base branch's** CODEOWNERS governs — resolution runs against a named ref's tree |
| S-8 | Only one file is used: `.github/` > root > `docs/`, never merged |
| S-9 | Zero-owner rules are legal and deliberately un-own a subtree |

## Design decisions (resolved from the spec's open list)

1. **`--on-empty` recommendation:** `error`.
2. **Performance:** naive full-tree resolve (twice per plan). Fine to ~100k files; a
   pattern-level pre-filter is a straightforward later optimization.
3. **GitHub Enterprise Server:** in, via `--api-url`; endpoint gaps degrade to exit 5,
   not wrong answers.
4. **Auth:** PAT only for v1 (`--token` / `$GITHUB_TOKEN`). GitHub App is the right v2
   answer for org-wide automation.
5. **Branch handling:** local repo, any ref via `--branch` (git `ls-tree`); plan/apply
   read and write the working-tree file, resolve against the ref's tree.
6. **Fleet automation:** the loop stays in your script. Cloning, auth, hosts,
   parallelism and retries are solved by `gh`/`ghorg`; the tool stays single-repo and
   composes with them.

## Tests as documentation

The test suite is the specification. Every test carries a doc comment naming the spec
requirement it enforces; `make docs` regenerates [BEHAVIOR.md](BEHAVIOR.md) from them
via `go/ast`, so the docs cannot drift from what is verified. Verification layers:

- **Vendored corpus** (`testdata/patterns.json`) from
  [hmarr/codeowners](https://github.com/hmarr/codeowners) — the actively maintained
  reference implementation whose matcher encodes GitHub's observed divergences from
  gitignore.
- **Differential fuzz** (`make diff-test`): 500k random pattern/path cases against
  hmarr's *unmodified* matcher, vendored verbatim as an oracle.
- **Property tests**: thousands of generated (file, tree, op) cases proving INV-1/INV-2
  and idempotence by independent re-resolution — these caught two real bugs during
  development (removal cascade under `inherit`; self-rename churn).
- **Acceptance tests** from the spec, end-to-end CLI tests in real git repos, and
  mocked-API fail-closed tests.

Build from a checkout with `make build`; `make all` runs vet, tests, build, and docs.
See [CONTRIBUTING.md](../CONTRIBUTING.md) before opening a PR.

## Prior art this design learned from

[hmarr/codeowners](https://github.com/hmarr/codeowners) (reference matcher),
[mszostok/codeowners-validator](https://github.com/mszostok/codeowners-validator)
(check taxonomy; also a cautionary tale — it mixes three glob engines),
[snyk/github-codeowners](https://github.com/snyk/github-codeowners) and
[beaugunderson/codeowners](https://github.com/beaugunderson/codeowners) (both built on
the npm `ignore` gitignore engine, whose `!`/`[ ]` support diverges from GitHub —
exactly the trap S-2 warns about),
[toptal/codeowners-checker](https://github.com/toptal/codeowners-checker) (archived;
the only prior mutation tool — it reorders lines, which R-1 forbids).

## Non-goals

GitLab/Bitbucket semantics · reordering or reformatting existing files · auto-deleting
rules that match zero files · inventing owners · resolving conflicting batches by
precedence · opening PRs or any git write · iterating over repos (that's your script's
job) · scope boolean algebra — `except` ([except.md](except.md)) is flat subtraction
only: no unions, no nested or grouped excepts, no except-of-except; the day it becomes
an expression language it stops being the simple spelling of one intent.
