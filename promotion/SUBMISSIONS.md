# Submission drafts

Copy-paste source for listing codeowners-tool in awesome-lists and for answering
CODEOWNERS questions. Nothing here has been submitted — all of it is yours to file.

## awesome-actions — sdras/awesome-actions

Good fit: the list already carries a direct peer,
[codeowners-validator](https://github.com/mszostok/codeowners-validator), under
**GitHub Tools and Management**, and we ship `action.yml`.

Per `contributing.md`: one PR per suggestion, entry appended to the end of its
category, Title Case name, a real PR title, repo link in the commit body.

**Entry** (append to the end of the *GitHub Tools and Management* list in `README.md`):

```markdown
- [Codeowners Tool](https://github.com/jordonpeterson/codeowners-tool) - Applies reviewable CODEOWNERS policies across one or many repositories, verifying every path's owners before it writes. Supports github.com and GitHub Enterprise Server.
```

**PR title:** `Add Codeowners Tool to GitHub Tools and Management`

**Commit body / PR body:**

```
https://github.com/jordonpeterson/codeowners-tool

A CLI and composite action for changing CODEOWNERS files. Ownership is declared
as a JSON policy; the tool derives the lines, checks the result against every
tracked file, and refuses to write anything it cannot prove correct. Sits
alongside codeowners-validator, which checks a file rather than changing one.

Disclosure: I am the author.
```

## awesome-devops — wmariuss/awesome-devops

Weaker fit, and worth knowing before you spend the PR: **Source Code Management**
and **Code review** list platforms (GitHub, Gitea, Gerrit), not single-purpose
CLIs, so a maintainer may reasonably read this as off-altitude. Submit it to
awesome-actions first.

Per `CONTRIBUTING.md`: `[RESOURCE](LINK) - DESCRIPTION.`, description under 80
characters, ends in a period, imperative PR title, no trailing whitespace.

**Entry** (in `README.md`, under *Code review*, alphabetical position):

```markdown
* [codeowners-tool](https://github.com/jordonpeterson/codeowners-tool) - Provable CODEOWNERS changes across many repos.
```

**PR title:** `Add codeowners-tool to Code review`

**PR body:**

```
Application: codeowners-tool
Category: Code review
Link: https://github.com/jordonpeterson/codeowners-tool

Declares CODEOWNERS ownership as a reviewable policy file, proves the result
against the repo's real files, and refuses writes it cannot verify. MIT.

Disclosure: I am the author.
```

## Answer drafts

Post these yourself, in your own voice, only on questions they genuinely answer.
Keep the disclosure line — Stack Overflow allows linking your own project when
the answer stands on its own and the affiliation is stated, and treats
link-first answers posted at volume as spam. Note that SO also prohibits
AI-generated answers, so rewrite rather than paste.

### Gotcha 1 — "I added a team and the previous owners stopped being requested"

> CODEOWNERS does not merge owners. GitHub takes the **last** pattern in the file
> that matches a given file and uses only that line's owners — every earlier
> match is discarded. So appending `/services/api/ @org/platform` for a path that
> `*` or another rule already covered replaces the owners rather than adding to
> them.
>
> To co-own, restate the existing owners on the winning line:
>
> ```
> /services/api/ @org/api-team @org/platform
> ```
>
> Two things that bite alongside it: "last wins" is positional, not
> specificity-based, so a broad pattern near the bottom silently overrides
> everything above it; and patterns are gitignore-style, so `README.md` matches
> at any depth while `/README.md` matches only the root one.
>
> Verify by file rather than by reading lines — open a throwaway PR touching the
> path and see who actually gets requested.
>
> Disclosure: I wrote codeowners-tool, which exists for this exact trap —
> `add_owner(/services/api/, @org/platform)` works out the line, carries the
> pre-existing owners onto it, and checks the outcome against every tracked file
> before writing: https://github.com/jordonpeterson/codeowners-tool

### Gotcha 2 — "My CODEOWNERS rule has no effect"

> Work down this list; it is almost always one of them.
>
> 1. **Location.** Only one file is used: `.github/CODEOWNERS`, root `CODEOWNERS`,
>    or `docs/CODEOWNERS`, in that order. A second one is ignored, not merged.
> 2. **Branch.** The file is read from the PR's *base* branch, so a rule added on
>    the feature branch does nothing for that PR.
> 3. **Access.** An owner needs explicit write access; a team must be
>    `@org/team-name` and visible to the repo. Unreachable owners are dropped
>    silently.
> 4. **Shadowing.** A later matching line wins outright — check what is *below*
>    your rule, not above it.
> 5. **Syntax.** The pattern subset is narrower than gitignore: no `!` negation,
>    no character ranges. An invalid line is discarded whole.
>
> Disclosure: I wrote codeowners-tool; `snapshot` prints the effective owner set
> per file, which answers "did my rule apply" without a test PR, and `audit`
> reports dead patterns and shadowed rules:
> https://github.com/jordonpeterson/codeowners-tool

### Gotcha 3 — "How do I find stale owners across many repos?"

> The rot that matters is not syntactic. Teams get renamed, members leave, and a
> team that lost write access stays in the file looking valid while quietly
> owning nothing — GitHub will not warn you. Checking it needs the API, not just
> the file: for every owner, does the account or team still exist, is it still in
> the org, and does it still have write access on this repo.
>
> Worth gating in CI so it fails on the way in rather than being discovered
> during an incident.
>
> Disclosure: I wrote codeowners-tool; `audit` runs those checks (exit 4 on
> findings, 0 on clean, and it fails closed rather than proposing a removal it
> could not verify): https://github.com/jordonpeterson/codeowners-tool
