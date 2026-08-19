# CLI reference

```
sync     (--op 'OP' ... | --policy FILE) [--on-empty error|inherit|unowned]
         [--repo DIR] [--branch REF] [--file PATH] [--create] [--dry-run]
         [--format text|json] [--out FILE] [--summary-out FILE]
check    (--op 'OP' ... | --policy FILE) [--format text|json]
plan     --op 'OP' ... [--on-empty error|inherit|unowned]
         [--repo DIR] [--branch REF] [--file PATH] [--out plan.json]
apply    --plan plan.json [--repo DIR]
audit    [--checks a1,a3,a6] [--format json|text] [--github-repo owner/name]
         [--token T | $GITHUB_TOKEN] [--api-url URL] [--cache-dir D] [--cache-ttl DUR]
         [--repo DIR] [--branch REF]
snapshot [--repo DIR] [--branch REF] [--out snap.json]
verify   --before before.json --after after.json [--scope PATTERN ...]
version  print the build this binary was stamped with
```

> IDs like `R-6`, `S-4`, `INV-2` and `A-9` are numbered requirements from the
> specification, each enforced by a named test — see [BEHAVIOR.md](BEHAVIOR.md),
> generated from the test suite. You never need them to use the tool; they exist so
> every claim in these docs is traceable to something that's actually checked.

## `sync`

Runs the whole pipeline in one step: plan, assert, apply, validate. This is the
command a fleet script calls.

| Flag | Meaning |
|---|---|
| `--op` / `--policy` | Where the ops come from. Mutually exclusive; passing both or neither is exit 3. |
| `--repo` | Local git repository. Default `.`. |
| `--branch` | Ref whose tracked tree governs resolution (S-7). Default `HEAD`. |
| `--file` | CODEOWNERS path override, repo-relative. |
| `--on-empty` | Policy when `remove_owner` empties an owner set. Allowed only with `--op`; with `--policy`, set `on_empty` in the file instead. |
| `--create` | Write CODEOWNERS if the repo has none. Off by default; never overwrites an existing file. |
| `--dry-run` | Makes no change to CODEOWNERS. `--out` and `--summary-out` still emit. |
| `--format` | `text` (default) or `json`. Under `json`, stdout is data and stderr is logs. |
| `--out` | Write the JSON record here instead of stdout. |
| `--summary-out` | Markdown rendering, for a PR body. |

## `check`

```sh
codeowners-tool check --policy policy.json
```

Reads no repository and writes nothing. It exits `0` for a valid policy, `3` for a
broken one, and never `1` — so under `set -e` a good policy always lets the script
continue and a bad one always stops it. Syntax errors stop at the first one;
everything else (bad enum values, ops that can't carry the `on_zero_match` you gave
them, a `remove_owner` with no `on_empty`) is reported all at once, because fixing a
generated 40-op policy one error per run is miserable.

It catches the problems that would fail identically on all 100 repos, so you find
them once instead of a hundred times.

Note that `--on-empty`, `on_zero_match`, and `--policy` all use the word "policy" for
different things: `--policy` is your ops file, while the other two are per-situation
rules the tool follows. The file is always "the policy file".

## `plan` and `apply`

```
intent (ops) ──▶ PLAN ──▶ ASSERT ──▶ APPLY ──▶ VALIDATE
                  │         │
                  │         └── gate; refuses on violation
                  └── resolves ownership before/after over the real git tree
```

`sync` runs that whole pipeline. If you want the reviewable artifact in the middle —
a JSON plan showing resolved ownership per path plus the literal line diff — run the
two halves separately:

```sh
codeowners-tool plan --op 'add_owner(/services/api/, @org/team-1)' --out plan.json
codeowners-tool apply --plan plan.json
```

`plan` has no `--create`: creating a file from nothing is a `sync --create` job.

## `snapshot` and `verify`

Prove in CI that a change touched nothing outside its declared scope:

```sh
codeowners-tool snapshot --branch main --out before.json
codeowners-tool snapshot --branch feature --out after.json
codeowners-tool verify --before before.json --after after.json --scope /services/api/
```

## `audit`

```sh
GITHUB_TOKEN=... codeowners-tool audit --github-repo org/repo --format json
```

Read-only. See [audit.md](audit.md) for the check list and the fail-closed rules.

## Policy file fields

```json
{
  "version": 1,
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    { "op": "add_owner(**/*.tf, @org/infra)",          "on_zero_match": "skip"    },
    { "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare" }
  ]
}
```

| Field | Where | Required | Meaning |
|---|---|---|---|
| `version` | top | yes | Format version. `1`. |
| `ops` | top | yes | Op strings, or objects. A bare string is shorthand for `{"op": "..."}` with everything else defaulted. |
| `name`, `description` | top | no | Surfaced in `--summary-out`, so PR reviewers know why. |
| `on_empty` | top | if any `remove_owner` | `error` \| `inherit` \| `unowned` |
| `op` | per op | yes | Op string, same syntax as `--op`. |
| `id` | per op | no | Short label used in JSON results and error messages. |
| `on_zero_match` | per op | no | `require` (default) \| `skip` \| `declare` |
| `note` | per op | no | Reaches the PR reviewer via `--summary-out`. |

Unknown fields are a hard error — a typo'd `on_zero_mtach` that silently fell back to
the default would apply the wrong policy to every repo at once. JSON has no comments, so
keys beginning with `_` (and the key `//`) are always ignored and can hold one.

`on_zero_match` is rejected on `rename_owner` (its scope comes from current ownership,
not a pattern) and `declare` is rejected on `remove_owner` (there is no rule to write).

## JSON output

Real output, abridged only in `changes`:

```json
{
  "repo": "work/org/foo",
  "status": "applied",
  "ops": [
    {"op": "add_owner(/services/api/, @org/api-team)", "status": "applied", "proven": "tree"},
    {"id": "tf", "op": "add_owner(**/*.tf, @org/infra)", "status": "skipped",
     "reason": "scope \"**/*.tf\" matches zero tracked files and on_zero_match=skip (R-21)"},
    {"id": "ci", "op": "add_owner(/.github/workflows/, @org/ci)", "status": "applied",
     "proven": "structural"}
  ],
  "ops_applied": 2, "ops_skipped": 1, "paths_changed": 37,
  "created": false, "changes": [ ]
}
```

`status` is `applied`, `unchanged`, `skipped`, `refused`, or `error`. `proven` is
`tree` when the result was checked against real files, `structural` when it wasn't —
see [what `declare` costs](guarantees.md#what-declare-costs).

Two things to know before you write `jq` against this. `id` appears only on ops your
policy named, so key on it only where you set it. And **`ops_applied` + `ops_skipped`
doesn't have to equal your op count** — an op that was already satisfied is
`unchanged` and counted by neither. If you want "did this policy actually do anything
anywhere", read `.status`; if you want "is this op reaching any repo at all", count it
out of `.ops[]`:

```sh
jq -s '[.[] | (.ops // [])[]] | group_by(.op) | map({op: .[0].op, n: length,
        applied: (map(select(.status=="applied")) | length)})' results.jsonl
```

Note the `// []`. Keys with nothing in them are **omitted entirely** rather than
emitted empty — that applies to `ops`, `warnings` and `changes`. A refused repo has no
`.ops` at all, so the same query without the guard dies with `Cannot iterate over
null` on the first repo that needed a human, which is the one you most wanted to see.

## Exit codes

`sync` uses a coarse three-code contract — its question is "did this repo converge?"
and it returns exactly `0`, `2`, or `3`, never anything else. Every other command uses
the precise taxonomy below.

| Exit | `sync` meaning | In a fleet script |
|---|---|---|
| 0 | Done — changed it, or it was already correct | continue |
| 2 | **This repo** needs a human | record it, continue |
| 3 | **The policy** is broken — it'll fail the same way everywhere | stop the run |

| Code | Precise meaning |
|---|---|
| 0 | Success — applied, or audit found nothing |
| 1 | No-op — nothing to change |
| 2 | Refused — would violate INV-1/INV-2, or exceed the 3 MB cap |
| 3 | Invalid input — malformed op, zero-match scope, conflicting batch |
| 4 | Audit findings present |
| 5 | Inconclusive — API unavailable, token insufficient, rate limited |
| 6 | Validation failed post-write; rolled back |

**The two tables do not use the same numbers for the same things**, so don't read
across. `sync` maps the precise codes onto its own by asking a single question — *is
this about the policy, or about this repo?*

| Precise code | Under `sync` | Why |
|---|---|---|
| 1 no-op | **0** | "Already correct" is the common fleet outcome; special-casing it defeats the point |
| 2 refused | **2** | This repo's file has an awkward shape |
| 3 zero-match scope | **2** | Whether a path exists is the most repo-specific fact there is |
| 3 malformed op, bad policy | **3** | Will fail identically on all 100 |
| 6 rolled back | **2** | A rolled-back write is about that one repo, not your policy |

`sync` makes no network calls, so it never returns 4 or 5.
