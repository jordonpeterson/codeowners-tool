# Reference: JSON output

The record `sync`, `plan` and `snapshot` emit, and the `jq` guards worth knowing. Fleet
aggregation recipes are in [FLEET.md](FLEET.md#the-jq-habit-worth-having).

Real `sync --format json` output, abridged only in `changes`:

```json
{
  "repo": "work/org/foo",
  "codeowners_path": ".github/CODEOWNERS",
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

`status` is `applied`, `unchanged`, `skipped`, `refused`, or `error`. `proven` is `tree`
when the result was checked against real files, `structural` when it wasn't — see
[GUARANTEES.md](GUARANTEES.md#what-declare-costs).

`codeowners_path` is the file this run wrote — one of the three locations in S-8, and
which one differs per repo — so a fleet loop can stage exactly it instead of `git add -A`.

`owners_removed` names, per path, the owners this run takes access **away** from, and is
present only when access is actually lost:

```json
"owners_removed": [
  {"path": "scanners/scan.py", "owners": ["@acme/appsec", "@acme/security-leads"]}
]
```

It is what separates co-owning five paths from displacing five paths' owners — `changes`
is line-level, and an inserted rule has no previous line to carry a before-state.
`--summary-out` renders the same facts grouped by owner, under **Owners losing access**,
because the reviewer's question is which teams stop owning things, not how many paths
moved. A run that only re-spells a handle reports no loss: owner identity is R-38a's.

It is present **exactly when `status` is `applied`**: when this run changed the file, or
under `--dry-run` would have. It is absent on `unchanged`, `skipped`, `refused` and
`error`, because none of those wrote a byte and staging a path they named would either
commit nothing (failing the `git commit` that follows, and with it a `set -e` rollout) or
name a file that does not exist.

**Absent means this run wrote nothing — not that no file was chosen.** An `unchanged`
repo has a perfectly good CODEOWNERS; it simply has nothing to stage. And **a `--dry-run`
record reports `applied` for a run that would have written**, so a preview wave emits the
field while the file on disk is untouched: check `dry_run` before staging anything, or
run the commit step only over records from a real wave.

A refusal that got as far as reading the file names it in the `error` string, as
`(governing file: …)` — a refusal in a repo whose ownership lives in `docs/` is a
different conversation from one in `.github/`. Refusals reached before a file was ever
read (a bad `--branch`, no CODEOWNERS and no `--create`) have no file to name, and a
`--create` run does not name a file it was about to invent. See
[FLEET.md](FLEET.md#committing-the-change-and-opening-the-pr).

`warnings` carries what a human should look at in a repo the tool did not refuse over: a
second CODEOWNERS file GitHub ignores (A-10), a run writing a file that is not the one
GitHub reads, lines GitHub cannot parse and silently skips (S-3), and a comment still
naming an owner a `rename_owner` renamed away. None of these is a reason to refuse a
correct edit, and none of them is visible at fleet scale unless the run that touched the
file reports it. They are independent, so a run can carry several at once, and they ride
on any record whose file was read — including a `refused` one, where the warning may be
the more useful half of the row. They are also rendered into `--summary-out`, under **Worth a look** —
the PR is the one moment somebody is already looking at that file and can fix it in the
same commit.

`created` reports what the run did, or under `--dry-run` what it *would* do, so a preview
of a greenfield fleet reports `"created": true` while writing nothing — not even the
parent directory.

A refusal reports `ops_applied` as 0 and carries no `changes` and no `codeowners_path`,
because no byte moved. (`ops_applied` is one of the unconditional keys, so it is emitted
as `0` rather than omitted.) **R-25's ceiling is the one deliberate exception**: it keeps
`paths_changed`, carrying the count it refused over, because a record that refuses on a
number and then omits the number is useless — and unlike other refusals it keeps `ops`,
with every op reported `unchanged`, so `jq` over `.ops[]` still sees one entry per op.

Each entry in `changes` carries the reason the edit took the shape it did, which is the
part a reviewer wants:

```json
{
  "action": "insert", "line": 2, "pattern": "/services/web/",
  "old_owners": ["@org/everyone"],
  "new_owners": ["@org/everyone", "@org/web-team"],
  "new_line": "/services/web/ @org/everyone @org/web-team",
  "reason": "rule \"*\" also governs out-of-scope paths; inserted narrowing rule \"/services/web/\" immediately after it so out-of-scope resolution is untouched (R-2)"
}
```

Two things to know before you write `jq` against this. `id` appears only on ops your
policy named, so key on it only where you set it. And **`ops_applied` + `ops_skipped`
doesn't have to equal your op count** — an op that was already satisfied is `unchanged`
and counted by neither. Keys with nothing in them are **omitted entirely** rather than
emitted empty, which applies to `ops`, `warnings` and `changes`; guard with `// []`. See
[FLEET.md](FLEET.md#the-jq-habit-worth-having) for the aggregation recipes.

