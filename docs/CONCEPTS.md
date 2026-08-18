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

Renames proposed for the awkward names above: [PROPOSAL-NAMING.md](PROPOSAL-NAMING.md).
