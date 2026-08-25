# Reference: guarantees and design

What the tool proves, where a guarantee deliberately weakens, and the GitHub semantics it
encodes.

## The two invariants

- **INV-1 (in scope):** after apply, every in-scope path resolves to exactly what the op
  requires.
- **INV-2 (out of scope):** after apply, every out-of-scope path resolves to exactly what
  it did before. **This is the product.**

The planner synthesizes line edits, then *proves* the result by re-resolving every file
git knows about at `--branch` and comparing against an independently computed desired
state. Anything unprovable → refusal, nothing written. Plans are idempotent (re-running is
a no-op) and preserve every untouched byte — comments, blank lines, spacing, ordering.

When there is no line that satisfies both invariants, the refusal names the rule in the
way:

```
error: refusing: rule "infra/" also governs paths outside scope "**/*.tf", and no sound narrowing pattern is derivable — amending would violate INV-2, appending would violate INV-1 (governing file: .github/CODEOWNERS)
```

## What `declare` costs

`declare` is the one place a guarantee weakens:

- **INV-2 is unaffected.** A pattern that matches nothing in the repo cannot move any
  existing file's ownership. Proven exactly as usual.
- **INV-1 weakens.** Your repo has no files matching the pattern, so there is nothing to
  check the rule against — the tool cannot prove the rule does what you meant. It proves
  the next best thing: that no rule after it can override it, which it guarantees by
  putting the rule at the end of the file. When someone later adds a matching file, this
  rule takes it. If that wasn't what you wanted, nothing will have caught it.

Ops that took this path report `"proven": "structural"` in the JSON and are called out in
`--summary-out`, so a reviewer can find them without reading the diff.

## What `on_except_zero_match: allow` costs

The same class of weakening, reachable from `except` ([OPERATIONS.md](OPERATIONS.md#except--carving-paths-out-of-a-scope-r-26r-32), R-28): an
except pattern that matches nothing tracked means the carve-out the policy promises does
not exist in this repo, so under `allow` the grant is written with **no carve line** — a
matching file created later falls under the grant, and nothing in the repo today can
verify the carve-out you asked for. INV-2 is unaffected. The op reports
`"proven": "structural"`, the inert pattern is listed in `except_unmatched`, and a warning
is emitted. The default (`require`) refuses instead — exit 2, "normalize this repo first".

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

## Design decisions

Resolved from the specification's open list.

1. **`--on-empty` recommendation:** `error`.
2. **Performance:** naive full-tree resolve (twice per plan). Fine to ~100k files; a
   pattern-level pre-filter is a straightforward later optimization.
3. **GitHub Enterprise Server:** in, via `--api-url`; endpoint gaps degrade to exit 5, not
   wrong answers.
4. **Auth:** PAT only for v1 (`--token` / `$GITHUB_TOKEN`). GitHub App is the right v2
   answer for org-wide automation.
5. **Branch handling:** local repo, any ref via `--branch` (git `ls-tree`); plan/apply read
   and write the working-tree file, resolve against the ref's tree.
6. **Fleet automation:** the loop stays in your script. Cloning, auth, hosts, parallelism
   and retries are solved by `gh`/`ghorg`; the tool stays single-repo and composes with
   them.

## Prior art

[hmarr/codeowners](https://github.com/hmarr/codeowners) (reference matcher),
[mszostok/codeowners-validator](https://github.com/mszostok/codeowners-validator) (check
taxonomy; also a cautionary tale — it mixes three glob engines),
[snyk/github-codeowners](https://github.com/snyk/github-codeowners) and
[beaugunderson/codeowners](https://github.com/beaugunderson/codeowners) (both built on the
npm `ignore` gitignore engine, whose `!`/`[ ]` support diverges from GitHub — exactly the
trap S-2 warns about),
[toptal/codeowners-checker](https://github.com/toptal/codeowners-checker) (archived; the
only prior mutation tool — it reorders lines, which R-1 forbids).
