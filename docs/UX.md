# UX specification — `sync`, `check`, and policy files

Working document for the `feat/sync-policy` branch. Folded into the PR description;
**deleted before merge** (the README and `docs/BEHAVIOR.md` are the durable docs).

Revision 2 — incorporates four independent design reviews (CLI ergonomics, config
format, README clarity, fresh-reader). Changes from revision 1 are marked **[r2]**.

## Problem

The tool is built for one repo at a time and for a human who will read a plan. The
target user is a script running against 100 repos. Three things block that today:

1. **`plan` → `apply` is two invocations and a temp file.** The plan JSON exists so a
   human can review it; at 100 repos nobody reads 100 plans.
2. **A no-op exits 1.** Pushing a standard policy at a fleet means *most repos most of
   the time are already correct*. Every `set -e` runner treats that as failure.
3. **A scope matching zero tracked files is invalid input** (`plan.go:143`, R-5). A
   standardized policy applied across heterogeneous repos will zero-match constantly —
   and for the "pre-configure the repo for files that don't exist yet" case, that
   zero-match *is* the intent.

## Design principles

1. **Additive.** `plan` and `apply` keep their exact semantics and exit codes. Every
   existing test passes unmodified.
2. **Monotonic escalation.** Nobody meets a concept before they need it.
3. **Refusal is a feature, not an error.** Across a heterogeneous fleet, some repos
   *should* refuse. The contract must let a script record a refusal and keep going,
   while halting immediately on a broken policy.
4. **Zero dependencies.** `go.mod` stays at module + go version.
5. **No new flag vocabulary. [r2]** `sync` reuses spellings the binary already has
   (`--format` from `audit`, `--out` from `plan`/`snapshot`). A fourth way to ask for
   JSON in one binary is a defect.

## The escalation path

| Need | What the user writes | New concepts |
|---|---|---|
| Try it on one repo | `sync --op 'add_owner(/x/, @a)'` | none |
| Same ops on many repos | `sync --policy policy.json` | a file with an `ops` array of strings |
| Ops that don't apply everywhere | add `on_zero_match` to *those* ops | one field, three values |

`--op` and `--policy` are mutually exclusive. Passing both, neither, or `--policy`
twice is exit 3 with a message naming the conflict — never a silent last-wins. **[r2]**

They deliberately do **not** compose (policy as base + `--op` as override). It's the
obvious next request and it's a footgun: an op appended at one call site is invisible
to the policy file's reviewers.

## Policy file

Minimum viable policy:

```json
{
  "version": 1,
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    "add_owner(/.github/workflows/, @org/ci)"
  ]
}
```

A bare string is **shorthand for** `{"op": "<string>"}` with every other field at its
default. There are not two forms; there is one form with a shorthand. **[r2]**

An op becomes an object only when it needs to say something extra:

```json
{
  "version": 1,
  "name": "org baseline ownership",
  "description": "CI owns workflows everywhere; infra owns Terraform where it exists.",
  "_note": "keys starting with _ are ignored — JSON has no comments",
  "on_empty": "error",
  "ops": [
    "add_owner(/services/api/, @org/api-team)",
    { "id": "tf",
      "op": "add_owner(**/*.tf, @org/infra)",
      "on_zero_match": "skip",
      "note": "Opportunistic — only repos that actually have Terraform." },
    { "id": "ci",
      "op": "add_owner(/.github/workflows/, @org/ci)",
      "on_zero_match": "declare",
      "note": "Baseline; also covers workflows added later." }
  ]
}
```

### Fields

| Field | Where | Required | Meaning |
|---|---|---|---|
| `version` | top | **yes** | Format version. `1`. |
| `ops` | top | yes | Op strings, or objects. Empty array is an error. |
| `name`, `description` | top | no | Surfaced in `--summary-out`, so PR reviewers know why. **[r2]** |
| `on_empty` | top | conditional | R-6 policy: `error` \| `inherit` \| `unowned`. **Required if any op is a `remove_owner`** — validated statically. **[r2]** |
| `op` | per op | yes | The op string, same syntax as `--op`. |
| `id` | per op | no | Short label used in JSON results and errors. Defaults to `ops[N]`. **[r2]** |
| `on_zero_match` | per op | no | `require` (default) \| `skip` \| `declare`. |
| `note` | per op | no | Reaches the PR reviewer via `--summary-out`. **[r2]** |

`version` is required, not optional-defaulting-to-1. A strict format read by pinned
binaries across a fleet with no version marker is a corner you cannot get out of. **[r2]**

Keys beginning with `_`, and the key `//`, are always ignored, at every level. JSON has
no comments and unknown fields are fatal, so without this the usual `"_comment"`
convention would be *illegal*. It doesn't weaken typo detection — `on_zero_mtach`
doesn't start with `_`. **[r2]**

### `on_zero_match`

Zero-match has three legitimate meanings and only the policy author knows which:

| Value | Meaning | Use it for |
|---|---|---|
| `require` (default) | This op must apply here. Zero match fails **this repo** (exit 2). | Paths every repo is known to have |
| `skip` | No-op here, reported as skipped. | Opportunistic ops — "if this repo has Terraform, `@org/infra` owns it" |
| `declare` | Write the rule anyway, for files that don't exist yet. | Standardized baselines identical across every repo |

**Naming rationale [r2].** Was `when_absent: error|skip|write`. Three problems, all
unfixable after release: *every* op writes, so `write` didn't distinguish this case
from the default and the docs needed a paragraph to explain it; `error` read as "do the
error thing" rather than "this op is mandatory here"; and `absent` collides with the
tool's large existing notion of absent *owners* (audit A-1/A-2/A-3), in a file whose
neighbouring key is `on_empty`. `require | skip | declare` all answer the same question
in the same voice: *does this op have to apply here?*

A global flag can't express this: a real fleet policy needs `skip` and `declare` ops in
the same run.

### Legality by op kind [r2]

`on_zero_match` is not meaningful on every op, and silently ignoring it is the same
class of failure as the typo the strictness rule exists to catch. `rename_owner`'s
scope is derived from current ownership, not a pattern — `plan.go:126-145` exempts it
from R-5 entirely. Enforced at `check` time:

| op kind | `require` | `skip` | `declare` |
|---|---|---|---|
| `add_owner` | yes | yes | yes |
| `set_owners` | yes | yes | yes (incl. `[]` → zero-owner rule, S-9) |
| `remove_owner` | yes | yes | **reject at parse** — nothing to write |
| `rename_owner` | **reject at parse** | **reject at parse** | **reject at parse** |

### What `declare` costs

The only place a guarantee weakens, stated plainly rather than buried:

- **INV-2 is unaffected** — proven over the tree exactly as today. A pattern matching
  nothing tracked cannot move any existing path's resolution.
- **INV-1 weakens (INV-6).** There is nothing in the repo to check the rule against, so
  the tool cannot prove the rule does what you meant. It proves the next best thing —
  that no rule after it can override it — and guarantees that by appending at EOF. When
  someone later adds a matching file, this rule takes it. If that wasn't what you
  wanted, nothing will have caught it.

Ops that took this path are reported with `"proven": "structural"` per op, and called
out in `--summary-out`, so a reviewer can find them without reading the diff.

## CLI surface

```
sync   (--op 'OP' ... | --policy FILE) [--on-empty error|inherit|unowned]
       [--repo DIR] [--branch REF] [--file PATH]
       [--create] [--dry-run]
       [--format text|json] [--out FILE] [--summary-out FILE]

check  (--op 'OP' ... | --policy FILE) [--format text|json]
```

| Flag | Meaning |
|---|---|
| `--on-empty` | R-6 policy. **[r2]** Allowed only with `--op`; with `--policy` it is a hard error, because a flag that silently beats a policy field means the artifact in git is not the policy that ran. |
| `--create` | Write CODEOWNERS if the repo has none. **Off by default.** Never overwrites. Honors `--file` when given; hard error with a non-`HEAD` `--branch`. **[r2]** |
| `--dry-run` | Makes no change to CODEOWNERS. `--out` and `--summary-out` still emit — that is the point of a fleet preview. **[r2]** |
| `--format` | `text` (default) or `json`. Under `json`, stdout is data and stderr is logs. |
| `--out` | Write the JSON record here instead of stdout. Same spelling as `plan`/`snapshot`. |
| `--summary-out` | Markdown rendering for a PR body. |

### `check` is a verb, not a flag [r2]

Was `sync --check-policy`. A flag whose defining property is that it *invalidates*
every other flag on the verb (`--repo`, `--branch`, `--file`, `--create`, `--dry-run`,
`--summary-out`) should be a verb. Three further reasons:

- It sits one token away from `--dry-run`, where the failure mode is **exit 0 across
  all 100 repos having read and written nothing** — silent success, the worst outcome
  this design can produce.
- Its JSON output needs a different schema. A `{"status":"unchanged"}` row landing in
  `results.jsonl` would report a phantom converged repo to the aggregation.
- As a verb it carries no repo flags at all, so the shape enforces the contract.

`check` validates exactly and only the exit-3 class (below). **Syntax errors fail fast;
semantic errors accumulate and all print** — fixing a generated 40-op policy one error
per run is miserable. **[r2]**

`check` exits **`0` valid / `3` broken, and never `1`. [r2b]** It is the first line of
every fleet script, under `set -e`; a clean policy returning the no-op code would abort
the run before the loop starts. The fresh-reader retest could not resolve this from the
README and named it as a reason to go read the source, so it is now stated in both.

### `--create` is off by default

Creating a file is the one action with no prior artifact to diff against — there is no
"before" to prove INV-2 against, only an empty one. Consistent with the tool's
fail-closed posture. It costs the fleet script one flag, once.

## Exit contract for `sync` [r2 — substantially revised]

**The rule: exit 3 is reserved for repo-*independent* failures.** Anything that depends
on which repo you are standing in is exit 2.

| Code | Meaning | Fleet script |
|---|---|---|
| 0 | Converged — applied, or already correct | continue |
| 2 | This repo needs a human — refused (INV-1/INV-2 unprovable), zero-match under `require`, no CODEOWNERS without `--create`, R-8 overlap in this tree, post-write validation rolled back, size cap | record and continue |
| 3 | The policy is broken — syntax, unknown field, bad enum, unsupported version, `--op`/`--policy` misuse. Will fail identically on all 100. | **halt** |

`sync` returns **exactly these and nothing else**. It makes no network calls, so 4 and
5 are unreachable — revision 2 initially reserved 5 "for a future API preflight", which
the fresh-reader retest caught: the documented example script's `*)` catch-all would
have turned a GitHub rate limit into a halted rollout. Reserve nothing. **[r2b]**

The mapping from the precise R-17 codes, which the README now states explicitly because
a reader tried to read across the two tables and hit a contradiction: **[r2b]**

| Precise code | Under `sync` | Why |
|---|---|---|
| 1 no-op | 0 | "Already correct" is the common fleet outcome |
| 2 refused | 2 | This repo's file has an awkward shape |
| 3 zero-match scope | **2** | Whether a path exists is the most repo-specific fact there is |
| 3 malformed op / bad policy | 3 | Will fail identically on all 100 |
| 6 rolled back | 2 | About that one repo, not the policy |

Revision 1 said "3+ = halt", which was unsafe in three concrete ways, all found by
review:

- **The default halted the fleet on repo 1.** Zero-match under the default value mapped
  to exit 3, so a policy naming `/services/api/` died on the first repo without it —
  while the README promised, 30 lines earlier, that not-applying was graceful.
- **Exit 6 (rolled back) is reachable from `sync`** and is a per-repo anomaly with
  nothing written. Under an open range, one weird repo aborted the fleet at repo 40 and
  the operator diagnosed "malformed policy."
- **"No CODEOWNERS without `--create`" was 3+**, and `--create` is off by default — so
  a fleet run halted on roughly repo 3.

This rule also gives `check` an exact definition: it checks the exit-3 class, and
nothing else. That sentence is now in the README.

`plan`'s R-17 codes are **unchanged**. `sync` gets its own coarser contract because its
question is different ("did this repo converge?" vs "what exactly happened?").

### Pre-existing bug to fix on this branch [r2]

`plan -h`, `plan --help`, and `audit -h` currently exit **3**: every `cmdX` returns
`ExitInvalid` on `fs.Parse` error and `flag.ErrHelp` is not special-cased
(`internal/cli/cli.go:153` and the equivalent in each command). Under any fleet
contract, `--help` reading as "halt the run" is wrong. Fix:
`if errors.Is(err, flag.ErrHelp) { return ExitOK }`.

## JSON output

One object per run, stable schema:

```json
{
  "repo": "work/org/foo",
  "status": "applied",
  "ops": [
    {"id": "api", "status": "applied", "proven": "tree"},
    {"id": "tf",  "status": "skipped", "reason": "scope matched zero tracked files"},
    {"id": "ci",  "status": "applied", "proven": "structural"}
  ],
  "ops_applied": 2,
  "ops_skipped": 1,
  "paths_changed": 37,
  "created": false,
  "warnings": [],
  "changes": [ /* plan.Change */ ]
}
```

`status` ∈ `applied` | `unchanged` | `skipped` | `refused` | `error`.

**`skipped` is a distinct status [r2]**, used when at least one op skipped and none
applied. Without it, a policy with one typo'd path prefix skips everywhere and reports
100 × `unchanged` — and the obvious `jq` recipe groups on `.status`, so the operator
reads "100 already correct, great" when the truth is "the policy matched nothing
anywhere." That is the difference between a caught typo and a no-op rollout.

**Per-op results replace bare counts [r2].** `"ops_skipped": 1` cannot answer the
question that motivates `skip` in the first place — *which* repos lack Terraform.
`proven: "tree" | "structural"` subsumes revision 1's `unproven_over_tree` array and
reads better. Counts stay; they're free and `jq`-friendly.

## Error messages [r2]

For someone debugging a fleet run at 2am, in priority order:

1. **File, line, column.** `encoding/json` gives byte offsets; nobody can use a byte
   offset. Convert by counting newlines in the source already in hand. `policy.json:14:22:`
2. **Which op — index *and* id *and* text.** `ops[2] (id "tf", "add_owner(**/*.tf, @org/infra)")`
3. **Bad value quoted, legal set enumerated, did-you-mean.** Levenshtein over the known
   field set is ~20 lines and zero dependencies.
4. **What to do.** `this is a policy error — it will fail identically on every repo; fix the policy, do not retry.`
5. **Never let Go's default message escape.** `json: cannot unmarshal object into Go
   value of type string` names Go types and points at the wrong concept.

## Implementation notes that are not obvious

**`DisallowUnknownFields` does not survive a custom unmarshaler. [r2]** It is a property
of `encoding/json`'s *struct decode path*; the moment a type implements
`json.Unmarshaler` the decoder hands it raw bytes and the flag stops propagating. The
natural implementation of the string-or-object `ops` array therefore silently accepts
`on_zero_mtach` and applies the wrong policy to 100 repos — exactly the failure
strictness exists to prevent. Required approach: decode `ops` as `[]json.RawMessage`,
dispatch on the first non-space byte (`"` → string form, `{` → object form, anything
else → a typed error), and build a fresh `json.Decoder` with `DisallowUnknownFields()`
per element. This also preserves per-element byte offsets, without which the
`policy.json:14:22` messages above are impossible.

**`encoding/json` silently takes the last duplicate key. [r2]** A generator
concatenating fragments produces `{"on_empty":"error", ..., "on_empty":"inherit"}` and
`inherit` wins silently — the same failure class as the typo. Detecting it needs a
token-level pass (`json.Decoder.Token()` tracking seen keys per object).

**`on_empty` is validated lazily today.** `plan.go:692` and `:718` only reach
`unknown --on-empty policy %q` when a removal actually empties an owner set. A policy
with `"on_empty": "inhrit"` would pass `check`, work on 46 repos, and blow up on repo
47 — precisely what `check` exists to prevent. Validate the enum at load.

**Op-string escaping nests inside JSON.** `checkScope` requires escaped spaces, so a
path with a space is `"add_owner(a\\ b, @x)"` in JSON — a double-escape a generator
will get wrong. Worse, `splitArgs` splits on top-level commas with no escape mechanism,
so a scope containing a literal comma is unrepresentable. Pre-existing `ops.go` limits,
but the policy file is where machines generate these, so it surfaces there first.
Document both; do not let the dispatch design foreclose a future structured op form.

## The script this is for

```bash
#!/usr/bin/env bash
set -euo pipefail

codeowners-tool check --policy policy.json     # fail on repo 0, not 100 times

while read -r repo; do
  gh repo clone "$repo" "work/$repo" -- --depth 1 -q
  code=0
  codeowners-tool sync --repo "work/$repo" --policy policy.json \
    --create --format json --summary-out "bodies/$repo.md" >> results.jsonl || code=$?
  case $code in
    0) ;;                                       # converged
    2) echo "$repo" >> needs-human ;;           # this repo, not the policy
    *) exit "$code" ;;                          # policy broken — stop
  esac
done < repos.txt

jq -s 'group_by(.status)|map({status:.[0].status, n:length})' results.jsonl
```

Note `--summary-out` writes **outside** the clone. **[r2]** Revision 1 put `.pr-body.md`
inside it, one `git add -A` away from being committed.

Add `--dry-run` for the fleet preview. That is the review step — at fleet granularity,
which is the only granularity at which review is possible with 100 repos.

## Rejected alternatives

| Rejected | Why |
|---|---|
| Plain-text policy file | Nicer for humans, but fleet policies are often generated. JSON chosen; simplicity preserved via the bare-string shorthand. |
| YAML policy file | Would be the first dependency in a zero-dependency repo. |
| Global `--allow-unmatched-scope` flag | Can't express a policy needing `skip` *and* `declare` ops in the same run. |
| `--check-policy` as a flag on `sync` | Invalidates every other flag on the verb; one token from `--dry-run`, where the failure is silent success across the fleet. **[r2]** |
| `--json` boolean | Would be a fourth spelling of "give me JSON" in one binary. `--format`/`--out` already exist. **[r2]** |
| `--op` composing with `--policy` | An op appended at one call site is invisible to the policy file's reviewers. **[r2]** |
| `--on-empty` overriding the policy's `on_empty` | The artifact in git would not be the policy that ran, and `check` would validate something other than what executes. **[r2]** |
| `except_repos` / per-repo exceptions | Real gap — every fleet policy acquires one. Deferred to v2 to keep this PR focused; the v1 workaround is a second policy file for the exception repos. **[r2]** |
| Multi-repo iteration inside the tool | Cloning, auth, hosts, parallelism, retries are solved by `gh`/`ghorg`. The tool stays single-repo and composes. |
| Git writes (branch/commit/PR) | Existing non-goal, correctly. `--summary-out` gives the script what it needs for a PR body. |
| Folding `sync` into `apply --policy` | `apply` means "write this exact proven plan". Overloading it blurs a contract worth keeping sharp. |
| Keeping exit 1 (no-op) under `sync` | "Already correct" is the *common* fleet outcome; making callers special-case it is the tax being removed. |
| Reusing the unowned-path insert point for `declare` | Inserts before all rules, so a `*` catch-all shadows the whole policy — 100 PRs that do nothing. `declare` appends at EOF. |
