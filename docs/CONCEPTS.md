# Concepts

What the tool does, in the words you need to follow everything else.

* **CODEOWNERS** : GitHub's file mapping path patterns to owning teams; the last matching rule wins.
* **Owner** : a team or user (`@org/api-team`) that must review changes to its paths.
* **Scope** : a glob (`/docs/`, `**/*.tf`) naming which paths a rule covers.
* **`add_owner` vs `set_owners`** : add co-owns, keeping existing owners; set replaces them entirely. The wrong choice silently strips reviewers — the mistake this tool exists to prevent.
* **Policy** : one JSON file describing the ownership you want, applied unchanged to many repos.
* **Fleet run** : that policy across N repos, each converging on its own.
* **Order-independence (R-8)** : operations must commute; a batch whose outcome depends on order is refused, not guessed at.
* **Exit 3 vs exit 2** : 3 = the policy is broken and fails identically everywhere; fix it, don't retry. 2 = this repo alone was refused.
* **`on_zero_match`** : what to do when a scope matches no files here — repos aren't identical.
* **`on_empty`** : what to do when a removal takes a rule's last owner.
* **Dry run** : produce the plan without writing anything.
* **Snapshot** : the resolved owner list for every tracked path — ground truth for what changed.

## Commands

* **`check`** : validates ops or a policy with no repository at all — the cheapest way to catch a broken policy before it touches 100 repos.
* **`sync`** : the one you'll use — resolves, checks invariants, and writes, in a single step.
* **`plan`** / **`apply`** : the same work split in two, with a reviewable JSON plan in between. `plan` cannot bootstrap: with no CODEOWNERS present it exits 3.
* **`snapshot`** / **`verify`** : capture resolved ownership before and after, then prove only what you intended changed.
* **`audit`** : twelve checks, reports only, never writes.
* **`lint`** : repairs three of the things audit reports; rewrites the working tree, so it needs `--github-repo` and a token.

## Doing the work

* **Create a file** : `sync --op '...' --create` — only `sync` can bootstrap; `plan` and `apply` cannot.
* **Update a file** : the same command. `--create` is safe to leave on permanently: it writes a file where none exists and otherwise just applies the ops, so one fleet command handles both cases.
* **Many changes at once** : `--policy file.json` instead of repeated `--op`; the policy file is the only form that scales to a fleet.
* **Rehearse** : add `--dry-run` — nothing is written, but `--out` and `--summary-out` still emit.
* **Review before writing** : `plan --out plan.json`, read it, then `apply --plan plan.json`.
* **Record what happened** : `--out` writes the JSON record, `--summary-out` writes a markdown PR body.

## Proving it

```
snapshot --out before.json  →  sync ...  →  snapshot --out after.json
verify --before before.json --after after.json --scope '/services/api/'
```

* **`--scope` is a claim** : it declares which paths were allowed to change. Anything that moved outside it is an invariant violation at exit 2.
* **No `--scope` means "nothing should have changed"** : any change at all fails. That is the assertion you want after a no-op refactor.
* **Snapshots need a commit** : resolution reads the tracked tree at `--branch` (default `HEAD`), not your working directory.

## Habits that save you

* **Run `check` first.** Exit 3 means the policy is wrong and will fail the same way on every repo — fix the file, never retry the run.
* **Triage a fleet by exit code, not by log.** 0 converged, 2 needs a human on that repo, 3 stop the rollout.
* **Reach for `add_owner` by default.** `set_owners` is correct only when you mean "these and nobody else".
* **Gate CI on `lint --dry-run`.** A successful write can still exit 4 when a line needs a person, so the writing run is the wrong signal.
* **Start `lint` with `--dry-run` always.** It rewrites files and needs network; one lookup it cannot answer and nothing is written.

## Exit codes

`sync` and `check` use a coarse contract — **0** converged, **2** this repo needs a human, **3** the policy is broken. The other commands use the full ladder: **0** ok, **1** no-op, **2** refused, **3** invalid input, **4** audit findings, **5** inconclusive, **6** rolled back.

Renames proposed for the awkward names above: [PROPOSAL-NAMING.md](PROPOSAL-NAMING.md).
