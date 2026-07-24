# codeowners-tool

A single CLI for making **safe, intent-level, verifiable** changes to large
GitHub CODEOWNERS files, plus an auditor that finds rot (dead owners, dead
rules, inert owners). GitHub only (github.com and GHES); GitLab/Bitbucket are
explicit non-goals.

**The problem it solves:** CODEOWNERS edits are expressed in *lines*, but what
anyone cares about is *resolved ownership per file path*. The two are related
by non-obvious semantics (last match wins; owner sets don't union; appending
`/x/ @team-2` **replaces** the owners of `/x/`, it doesn't add to them). This
tool makes you express intent over resolved ownership and refuses to apply
anything it can't prove.

```
intent (ops) ──▶ PLAN ──▶ ASSERT ──▶ APPLY ──▶ VALIDATE
                  │         │
                  │         └── gate; refuses on violation
                  └── resolves ownership before/after over the real git tree
```

## Quick start

```sh
go build -o codeowners-tool ./cmd/codeowners-tool

# Add a co-owner to everything under /services/api — existing owners retained:
./codeowners-tool plan --op 'add_owner(/services/api/, @org/team-1)' --out plan.json
# Review plan.json (ownership rows + literal line diff), then:
./codeowners-tool apply --plan plan.json

# Audit for rot:
GITHUB_TOKEN=... ./codeowners-tool audit --github-repo org/repo --format json

# Prove in CI that a change touched nothing outside its declared scope:
./codeowners-tool snapshot --branch main --out before.json
./codeowners-tool snapshot --branch feature --out after.json
./codeowners-tool verify --before before.json --after after.json --scope /services/api/
```

## Operations (Engine A — mutation)

Scope is a directory, file path, or glob — same syntax as CODEOWNERS patterns.

| Op | Meaning |
|---|---|
| `add_owner(scope, owner)` | Owner becomes a **co-owner**; every pre-existing owner of every path in scope is retained. |
| `set_owners(scope, [owners])` | Exact owner set for every path in scope, displacing prior owners. `[]` is legal: it deliberately un-owns the scope. |
| `remove_owner(scope, owner)` | Owner stops owning every path in scope. If a rule's owner set would empty, `--on-empty` is **required** (see below). |
| `rename_owner(old, new)` | Global identifier substitution — the only op safe as pure text replacement (it can't change any rule's match set). |

### The two invariants

- **INV-1 (in scope):** after apply, every in-scope path resolves to exactly
  what the op requires.
- **INV-2 (out of scope):** after apply, every out-of-scope path resolves to
  exactly what it did before. **This is the product.**

The planner synthesizes line edits, then *proves* the result by re-resolving
the entire tracked tree (at `--branch`, default HEAD) and comparing against an
independently computed desired state. Anything unprovable → exit 2, nothing
written. Plans are idempotent (re-running is a no-op) and preserve every
untouched byte — comments, blank lines, spacing, ordering.

### `--on-empty` (R-6)

Removing the sole owner of a rule needs an explicit policy — **there is no
default**, and the documented recommendation is `error`:

- `error` — refuse (recommended: consistent with the tool's fail-closed posture)
- `inherit` — delete the rule; the preceding broader rule takes over
  (removal **cascades** if the fallthrough rule also lists the owner)
- `unowned` — keep the pattern with zero owners (GitHub's sanctioned
  substitute for `!` negation)

Under `inherit`/`unowned` the resulting reassignment is shown in the plan's
ownership rows.

## Audit (Engine B)

Read-only. **Never writes** — where a fix is expressible it emits Engine A op
strings for a human to review and run through `plan`/`apply`, the system's
single writer path.

| ID | Check | API | Auto-fix |
|---|---|---|---|
| A-1 | Owner doesn't exist (deleted/renamed user or team) | yes | proposes `remove_owner` |
| A-2 | Owner exists but isn't in the org | yes | proposes `remove_owner` |
| A-3 | Owner lacks **explicit write access** (org membership isn't enough) | yes | proposes `remove_owner` |
| A-4 | Rule matches zero tracked files | no | report only, permanently — a dead pattern may be deliberate intent |
| A-5 | Rule dead **only because of case** (`/Src/` vs `src/`) | no | suggests corrected pattern |
| A-6 | Rule fully shadowed by later rules | no | report only |
| A-7 | Duplicate pattern | no | report only |
| A-8 | Syntax errors | optional | no |
| A-9 | Unowned path coverage | no | n/a |
| A-10 | Multiple CODEOWNERS files present | no | error — GitHub uses only the first |
| A-11 | CODEOWNERS file itself unowned | no | report only |
| A-12 | File size approaching 3 MB | no | n/a |

**Fail closed (R-12):** a 404 can mean deleted, renamed, invisible to the
token, or rate-limited. The client probes org/repo visibility first; anything
inconclusive is reported `unknown`, exits 5, and **never proposes a removal**.
An expired token quietly stripping owners is the worst failure this tool can
produce, so it can't. Email owners are `unverifiable`, never dead (R-13).
Removing a sole owner is presented as a **reassignment** with before → after
owners per path, never a bare line deletion (R-14). Lookups are cached in
memory per run and optionally on disk (`--cache-dir`, `--cache-ttl`).

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success — applied, or audit found nothing |
| 1 | No-op — nothing to change |
| 2 | Refused — would violate INV-1/INV-2, or exceed the 3 MB cap |
| 3 | Invalid input — malformed op, zero-match scope, conflicting batch |
| 4 | Audit findings present |
| 5 | Inconclusive — API unavailable, token insufficient, rate limited |
| 6 | Validation failed post-write; rolled back |

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
2. **Performance:** naive full-tree resolve (twice per plan). Fine to ~100k
   files; a pattern-level pre-filter is a straightforward later optimization.
3. **GHES:** in, via `--api-url`; endpoint gaps degrade to exit 5, not wrong answers.
4. **Auth:** PAT only for v1 (`--token` / `$GITHUB_TOKEN`). GitHub App is the
   right v2 answer for org-wide automation.
5. **Branch handling:** local repo, any ref via `--branch` (git `ls-tree`);
   plan/apply read and write the working-tree file, resolve against the ref's tree.

## Tests as documentation

The test suite is the specification. Every test carries a doc comment naming
the spec requirement it enforces; `make docs` regenerates
[docs/BEHAVIOR.md](docs/BEHAVIOR.md) from them via `go/ast`, so the docs
cannot drift from what is verified. Verification layers:

- **Vendored corpus** (`testdata/patterns.json`) from
  [hmarr/codeowners](https://github.com/hmarr/codeowners) — the actively
  maintained reference implementation whose matcher encodes GitHub's observed
  divergences from gitignore.
- **Differential fuzz** (`make diff-test`): 500k random pattern/path cases
  against hmarr's *unmodified* matcher, vendored verbatim as an oracle.
- **Property tests**: thousands of generated (file, tree, op) cases proving
  INV-1/INV-2 and idempotence by independent re-resolution — these caught two
  real bugs during development (removal cascade under `inherit`; self-rename
  churn).
- **Acceptance tests T-1…T-11** from the spec, end-to-end CLI tests in real
  git repos, and mocked-API fail-closed tests.

## Prior art this design learned from

[hmarr/codeowners](https://github.com/hmarr/codeowners) (reference matcher),
[mszostok/codeowners-validator](https://github.com/mszostok/codeowners-validator)
(check taxonomy; also a cautionary tale — it mixes three glob engines),
[snyk/github-codeowners](https://github.com/snyk/github-codeowners) and
[beaugunderson/codeowners](https://github.com/beaugunderson/codeowners)
(both built on the npm `ignore` gitignore engine, whose `!`/`[ ]` support
diverges from GitHub — exactly the trap S-2 warns about),
[toptal/codeowners-checker](https://github.com/toptal/codeowners-checker)
(archived; the only prior mutation tool — it reorders lines, which R-1 forbids).

## Non-goals

GitLab/Bitbucket semantics · reordering or reformatting existing files ·
auto-deleting rules that match zero files · inventing owners · resolving
conflicting batches by precedence · opening PRs or any git write.

## License

MIT. The pattern matcher is ported from, and differentially tested against,
[hmarr/codeowners](https://github.com/hmarr/codeowners) (MIT, © Harry Marr) —
see NOTICE.
