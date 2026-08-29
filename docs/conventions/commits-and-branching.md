# Commits & Branching

## Commit messages

Use concise conventional-style messages for implementation commits:

```text
type(scope): imperative description
```

Common types are `feat`, `fix`, `docs`, `refactor`, `test`, `ci`, `build`, and `chore`.
The scope names the affected service or concern when useful. Examples:

```text
feat(inventory): add bounded hold renewal
fix(access): reject expired ticket credentials
docs(adr): record admission policy ownership
```

Explain non-obvious motivation and trade-offs in the body. Reference local ticket IDs such as
`TKT-46` when the branch/PR does not already make the relationship clear. No Commitizen or commit
hook enforces this convention; review and focused commits do.

## Branch names

The project workflow creates ticket-prefixed branches from `main`:

```text
TKT-<number>-short-description
```

Use lowercase hyphenated descriptions after the uppercase ticket key, for example
`TKT-46-m2-readiness-cleanup`. Do not create a second implementation branch for the same active
ticket without reconciling its board state.

## Branching strategy

- `main` is the integration branch.
- Work happens on a short-lived ticket branch and lands through a PR.
- Sync with `main` before final verification and resolve conflicts deliberately.
- Never force-push or rewrite `main`.
- Do not claim deployment behavior from a merge; this repository currently has CI only.

## Commit hygiene

- Keep each commit to one logical, reviewable change that leaves its focused tests passing.
- Preserve unrelated user changes in a dirty worktree.
- Avoid drive-by formatting or generated changes outside the ticket.
- Large owner-approved PRs should retain risk-ordered commits and a review guide.
- Never commit secrets, local databases, generated build output, or editor state.

The full branch must pass `make check` before handoff. Squash only when intermediate commits do not
carry useful review structure; otherwise preserve meaningful atomic commits.
