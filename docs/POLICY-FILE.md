# Reference: the policy file

Every field a policy may carry, and the three settings that decide what a run is allowed
to do. What the ops themselves mean is in [OPERATIONS.md](OPERATIONS.md); commands and
flags in [COMMANDS.md](COMMANDS.md).

## Policy file fields

| Field | Where | Required | Meaning |
|---|---|---|---|
| `version` | top | yes | Format version. `1`. |
| `ops` | top | yes | Op strings, or objects. A bare string is shorthand for `{"op": "..."}` with everything else defaulted. |
| `name`, `description` | top | no | Surfaced in `--summary-out`, so PR reviewers know why. |
| `create` | top | no | Boolean. Permission to write a CODEOWNERS where the repo has none — the policy's spelling of `--create`, which is exit 3 alongside `--policy` (R-34). Never overwrites, so it is safe to leave set for a fleet where only some repos have a file. |
| `on_empty` | top | if any `remove_owner` | `error` \| `inherit` \| `unowned` |
| `max_paths_changed` | top | no | R-25 ceiling, as a whole number of paths. Zero is legal and asserts the wave changes no ownership at all. |
| `defaults` | top | no | Object supplying what an op does not state, so a 40-op baseline stays 40 strings (R-35). Accepts `on_zero_match` and `on_except_zero_match` only; a per-op value wins, and `check` echoes the resolved one. |
| `lint` | top | no | Object holding `lint`'s preferences — `remove_stale_paths` (boolean) and `on_empty` — so the repair policy is reviewed in the same artifact (R-36). `sync` ignores it and `lint` ignores `ops`, but **every command validates the whole file**. |
| `op` | per op | yes | Op string, same syntax as `--op`. |
| `id` | per op | no | Short label used in JSON results and error messages. |
| `on_zero_match` | per op | no | `require` (default) \| `skip` \| `declare` |
| `on_except_zero_match` | per op | no | `require` (default) \| `allow` — only on ops whose scope carries an `except` clause; governs an except pattern that matches zero tracked files ([OPERATIONS.md](OPERATIONS.md#except--carving-paths-out-of-a-scope-r-26r-32), R-28) |
| `except` | per op | no | Carve-out as a JSON array — `["/.github/CODEOWNERS"]` — equivalent to the `<scope> except <pat> …` string spelling, and exit 3 alongside it (R-37). Array elements need no delimiter escaping, so a space is written plainly: `"my dir/"`. |
| `note` | per op | no | Reaches the PR reviewer via `--summary-out`. |

Unknown fields are a hard error — a typo'd `on_zero_mtach` that silently fell back to the
default would apply the wrong policy to every repo at once. That applies at every level:
`defaults` and `lint` accept only the keys listed above, and the refusal names the set it
does accept. JSON has no comments, so keys beginning with `_` (and the key `//`) are
always ignored and can hold one.

`on_zero_match` is rejected on `rename_owner` (its scope comes from current ownership, not
a pattern) and `declare` is rejected on `remove_owner` (there is no rule to write).

Ops in one batch must **commute**. Two ops whose scopes overlap on a path and whose order
would change the outcome are refused rather than resolved by position (R-8):

```
error: ops "set_owners(*, [@org/everyone])" and "add_owner(/services/api/, @org/api-team)" do not commute, and "*" provably governs every path "/services/api/" does — so the batch is order-dependent on every repository that has one (R-8); run "set_owners(*, [@org/everyone])" on its own first and the narrower op(s) in a second run — but preview that first run with --dry-run or `plan --out`: "set_owners(*, [@org/everyone])" REPLACES the owners of every path in scope, so anyone owning those paths today and not listed in it loses them
```

`add_owner` ops commute with each other, so any number of them can share a run. A
`set_owners` on a broad scope generally cannot share a run with anything narrower — split
it into two invocations.

The conflict is caught in **two places, with two different exit codes**, and the split
matters to a fleet:

- **Provable from the op strings alone**, and only when **both ops must apply** (the
  default `on_zero_match: require`) — is exit **3**, from `check` (with no repository at
  all) and from `sync` (before it opens one), identically on every repo whatever its tree
  contains. Such a policy cannot converge anywhere: a repo that has the narrower scope
  refuses on the overlap, and a repo that does not refuses on the zero match, so saying so
  once at repo 0 is strictly better than a hundred times. The remedy is in the error: run
  the displacing op alone first.
- **An op carrying `skip` or `declare`** is never decided here. `skip` means "if this repo
  has it", so the batch is order-dependent only in the repos that do — a fact about the
  tree, and therefore exit 2, per repo. A `declare`d rule lands at EOF where
  last-match-wins settles the outcome, so there is no order ambiguity to refuse.
- **Only visible against a real tree** — two scopes that neither provably contains, which
  happen to meet on a path this repo has — stays exit **2**, per repo, so the fleet loop
  records it and steps to the next clone.

The static half is sound rather than complete: it reports only what `pattern.Contains`
proves, because exit 3 halts a rollout and a false positive there is the expensive
direction.

## `--on-empty` / `on_empty` (R-6)

Removing the sole owner of a rule needs an explicit policy — **there is no default**, and
the documented recommendation is `error`:

- `error` — refuse (recommended: consistent with the tool's fail-closed posture)
- `inherit` — delete the rule; the preceding broader rule takes over (removal **cascades**
  if the fallthrough rule also lists the owner)
- `unowned` — keep the pattern with zero owners (GitHub's sanctioned substitute for `!`
  negation)

Under `inherit`/`unowned` the resulting reassignment is shown in the plan's ownership rows.

## Creating a file (R-23), and not creating one

`create` is permission, not instruction — `"create": true` in a policy, or `--create` on
an `--op` run; passing the flag with `--policy` is exit 3 (R-34b). A run whose ops all
skip, or that has nothing to write, creates **no file and no `.github/` directory**, reports `"status": "skipped"`
with `"created": false`, emits no `codeowners_path`, and exits 0. An empty CODEOWNERS
would be worse than none: "which repos still need ownership?" is answered by "which repos
have no CODEOWNERS", and an empty file answers *done* forever.

What a created file contains is exactly one rule line per op that applied — no header, no
provenance comment, no timestamp. The provenance belongs in the PR body (`--summary-out`
names the policy) and in the commit message, both of which are bound to the change that
made them and cannot go stale. A header naming the policy would be a confident lie the
moment wave 2 ran, and a tool that rewrites comments in a file it otherwise never
reformats has given up the guarantee everything else here rests on. A header you write by
hand is preserved byte-for-byte forever, including across re-runs.

Identical inputs produce an identical file: three fresh repos given one policy produce one
byte sequence. A hundred near-identical PRs are only reviewable if that holds.

## The blast-radius ceiling (R-25)

`--max-paths-changed N`, or `max_paths_changed` in a policy, refuses a run that would
change the owners of more than N paths:

```console
$ codeowners-tool sync --op 'add_owner(*, @org/platform)' --max-paths-changed 200
error: refusing: this run would change the owners of 251 path(s), over the 200-path ceiling set by --max-paths-changed (R-25) — nothing was written; the op(s) behind the number: ops[0]. Re-run with `--dry-run --out preview.json` to see which paths, raise the ceiling if the number is right, or narrow the ops if it is not (governing file: .github/CODEOWNERS)
refused: 0 op(s) applied, 0 skipped; 0 line change(s), 251 path(s) change owners
  ops[0]  unchanged (proven: tree)
  refusing: this run would change the owners of 251 path(s), over the 200-path ceiling set by --max-paths-changed (R-25) — nothing was written; the op(s) behind the number: ops[0]. Re-run with `--dry-run --out preview.json` to see which paths, raise the ceiling if the number is right, or narrow the ops if it is not (governing file: .github/CODEOWNERS)
$ echo $?
2
```

Off by default, because a default ceiling would break every legitimate `set_owners(*, …)`
baseline on upgrade and teach operators to pass an enormous number reflexively. Exit 2,
not 3: how many paths a repo has is the most repo-specific fact there is, so a fleet
records it and carries on. `--dry-run` gives the same verdict, and `--out`/`--summary-out`
still emit — you decide whether to raise the ceiling by reading what it would have done.
The refusal also names the ops behind the number, because the per-op array reports them
all as `unchanged` (nothing applied) and a blocked op would otherwise be indistinguishable
from one that was already satisfied.

The ceiling gates `sync`. `plan`/`apply` is the two-step path where a human reads the
artifact before anything is written, and carries no ceiling of its own — the review *is*
the gate there. A negative value is rejected rather than read as "no ceiling": omit the
flag for that.

The flag is allowed only with `--op`, and the field only in a policy, exactly like
`--on-empty`/`on_empty`. The ceiling is a claim about the intent ("this wave touches
dozens of files per repo, not thousands"), so for a policy run it belongs in the artifact
a reviewer approves; a ceiling in one shell line survives exactly as long as that shell
line. There is no precedence rule to learn because there is no overlap: passing the flag
with `--policy` is exit 3.

