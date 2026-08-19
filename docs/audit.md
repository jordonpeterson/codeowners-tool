# Audit — find rot without writing anything

```sh
GITHUB_TOKEN=... codeowners-tool audit --github-repo org/repo --format json
```

Read-only. **Never writes** — where a fix is expressible it emits op strings for a
human to review and run through `plan`/`apply`, the system's single writer path.

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

Run a subset with `--checks a1,a3,a6`. The checks that need the API are skipped
without `--github-repo`.

**Fail closed (R-12):** a 404 can mean deleted, renamed, invisible to the token, or
rate-limited. The client probes org/repo visibility first; anything inconclusive is
reported `unknown`, exits 5, and **never proposes a removal**. An expired token quietly
stripping owners is the worst failure this tool can produce, so it can't. Email owners
are `unverifiable`, never dead (R-13). Removing a sole owner is presented as a
**reassignment** with before → after owners per path, never a bare line deletion
(R-14). Lookups are cached in memory per run and optionally on disk (`--cache-dir`,
`--cache-ttl`).

Audit exits `4` when it finds something and `5` when it couldn't tell — see
[exit codes](cli.md#exit-codes).
