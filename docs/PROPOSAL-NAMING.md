# Proposal: naming the vocabulary

Third in a set, after [PROPOSAL-AUTHORING.md](PROPOSAL-AUTHORING.md) (the friction) and
[PROPOSAL-POLICY-V2.md](PROPOSAL-POLICY-V2.md) (the shape). This one is about the words.

Two tests, applied to every name below.

**The sentence test.** A setting and its value should read as a sentence when you put them
together. `on_zero_match: skip` does not — "on zero match, skip" skips *what*, and matches
*what* against *what*? `if_no_files: skip` does.

**No distinction may rest on one letter.** The difference between co-owning and displacing is
the mistake this tool exists to prevent, and today it is carried by the `s` on the end of
`set_owners` versus `add_owner`. That is the least visible way to encode the most important
distinction in the product.

## The dangerous verb

`set_owners` is the problem name. "Set" is the weakest possible word for it: in most APIs
setting a field means assigning it, not deleting whatever was there, so the destructive half is
invisible. Paired with `add_owner` it is also near-identical in length and shape, differing by
one character that a reader skims past.

| today | proposed | why |
|---|---|---|
| `add_owner` | `add` | already honest; the `_owner` suffix carries nothing when everything here is an owner |
| `set_owners` | `replace_all` | "replace all owners with these" — the displacement is in the name, and it is visibly longer than its safe sibling |
| `remove_owner` | `remove` | fine as-is |
| `rename_owner` | `rename` | fine as-is; in v2 it already sits at top level, which conveys that it is not scoped |

`replace_all` over the shorter `replace` for one reason: `replace` collides with rename in a
reader's head — `{"replace": ["@a"]}` could be misread as "replace @a with something". `all`
kills that reading and says the value is the complete list.

The runner-up was `only` — `{"/docs/": {"only": ["@org/docs-team"]}}`, "docs is owned only by
docs-team" — which is arguably the most intuitive of any candidate. It lost because `add`,
`remove` and `rename` are all verbs, so a fourth verb makes the group scan as four
alternatives for one slot, and `only` would read like a modifier instead.

Asymmetric length is deliberate. The safe verb is three letters, the destructive one is eleven.

## The two conditionals

These are the names a newcomer has no chance with, because both are written in the vocabulary
of the implementation rather than of the question being asked.

| today | proposed |
|---|---|
| `on_zero_match: require \| skip \| declare` | `if_no_files: error \| skip \| write_anyway` |
| `on_empty: error \| inherit \| unowned` | `if_no_owners_left: error \| inherit \| unowned` |

`on_zero_match` fails the sentence test three times over. Its subject is missing (it is the
*scope* that matched nothing, and what it matched against is *tracked files*), and its three
values come from three unrelated families: `require` requires what, `skip` skips what,
`declare` declares what? Renamed, each one completes the sentence — "if no files: error", "if
no files: skip", "if no files: write anyway" — and `require` becomes `error`, which is what it
actually does and which the other conditional already calls it.

`on_empty` has the same missing subject: what is empty is the rule's owner list. Its values
survive intact, and `inherit` versus `unowned` should stay exactly as it is — the tempting
rewrite of `inherit` to `delete_rule` describes the mechanism but blurs the one distinction
this setting exists to draw, since deleting the rule is how *both* outcomes happen and the
difference is whether a broader rule then applies.

The larger win is the shared prefix: two one-off names become one learnable pattern,
`if_<condition>: <what to do>`, with `error` meaning the same thing in both.

**One cost, stated plainly.** `declare` gives the docs a noun — "a declared rule", used
throughout FLEET.md. `write_anyway` has no noun form, so those sentences become "a rule written
for files that don't exist yet". If the team values the noun more than the sentence test,
keeping `declare` is defensible; it is the one rename here I would not insist on.

## Cumulative effect

Before:

```json
{ "version": 1, "on_empty": "inherit", "ops": [
  { "op": "set_owners(/docs/, @org/docs-team)" },
  { "op": "add_owner(/.github/workflows/, @org/ci)", "on_zero_match": "declare" },
  { "op": "remove_owner(**/*.tf, @org/infra-legacy)", "on_zero_match": "skip" }
]}
```

After, with the v2 shape:

```json
{ "version": 2, "if_no_owners_left": "inherit", "ownership": {
  "/docs/":              { "replace_all": ["@org/docs-team"] },
  "/.github/workflows/": { "add": ["@org/ci"], "if_no_files": "write_anyway" },
  "**/*.tf":             { "remove": ["@org/infra-legacy"], "if_no_files": "skip" }
}}
```

Nothing here needs a glossary. That is the whole bar.

## Names that are already right

Worth saying, because they are the model to copy. `max_paths_changed` describes its own unit
and limit. `paths_changed` and `ops_applied` say what was counted. `dry_run` is universal.
`--fix-ops` outputs the same vocabulary the input uses. The pattern these share: the name
names the *thing being counted or done*, not the internal event that triggers it.

One lower-priority candidate: `scope`. It is a glob pattern, GitHub's own docs call it a
pattern, and "pattern" is a word people already know — whereas `scope` has to be taught. In v2
it is the map key so it mostly vanishes from the file, but it still appears in every error
message and doc heading. Worth changing there, worth nothing on its own.

## How this ships: attached to v2, not as its own wave

The blast radius is real — 534 occurrences across the Go source and 147 across the docs for
the four op names, plus 149 more for the two conditionals — and every existing policy file
in every consumer repo. So do not spend a breaking change on it.

The renames ride in on `version: 2`, which is already a new dialect:

- **v2 files accept only the new names.** Clean slate, no dual vocabulary to document.
- **v1 files accept only the old names, forever.** Nothing anyone has written breaks, and the
  pinned-binary story is unchanged.
- **`convert --policy v1.json`** (proposed in the v2 doc) emits new names, so nobody
  hand-migrates a 40-op baseline.
- **Old names in a v2 file get the existing did-you-mean treatment**, pointed at the new
  spelling: `unknown field "on_zero_match" (in version 2, this is "if_no_files")`. That single
  message does most of the teaching for anyone arriving from an older policy.

Nothing here is user-visible until someone opts into v2 by writing `"version": 2`.

## Open questions for the team

- **Is `write_anyway` worth losing the noun "declared rule"?** The one call above I would put
  to whoever owns FLEET.md rather than decide here.
- **Does `replace_all` beat `only`?** I chose grammatical consistency over raw readability;
  reasonable people will weigh those the other way.
- **Should `scope` → `pattern` happen in v1 error messages too?** It is not a format change,
  just wording, so it could ship independently and early.
