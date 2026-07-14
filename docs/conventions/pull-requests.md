# Pull Requests & Code Review

## What is a Pull Request?

A pull request (PR) is a request to merge changes from one branch into another (typically into `main` or `dev`). It allows you to propose code changes, have them reviewed by peers, and validate that everything works before final integration.

It is the most common means of contribution and collaboration among developers. Establishing clear conventions for creating them (naming, size, description, etc.) is therefore essential.

---

## PRs as the Unit of Integration

A pull request is the atomic unit of reviewed change in this workflow. It is validated by CI and
merged into `main`; this repository does not automatically deploy it to an environment. Every PR
must leave the local Compose system and gate in a working state.

Two practical consequences:

- **PR quality directly affects the next milestone.** A weak change becomes the baseline for later
  AI-assisted work even though it is not automatically deployed.
- **Small, focused PRs are easier to roll back.** If a problem surfaces post-merge, the smaller the change, the cheaper the recovery.

---

## Writing a Good Pull Request

### Complete Description

At minimum, every PR should identify its TKT ticket, explain motivation and risk, summarize the
change, list validation evidence, and call out documentation or ADR impact. No repository PR
template is currently configured.

### Small, Focused, and Precise

- A PR should have a **single responsibility** (one feature or one fix).
- Keep changes easy to review: aim for **~250 lines of code** across a few files.
- Larger PRs are sometimes necessary but should not be the norm. In that case, open your PR as a **draft early** to get upstream feedback.

### Self-Review

- Review your own PR before submitting it to your colleagues.
- Ensure the PR does what it is supposed to do and that there is no superfluous code.
- **For LLM-generated code**: read every line, confirm it fits project style — you own the code regardless of how it was written.

### Clear Commits

- Use meaningful and consistent commit messages.
- Follow the repository's [commit convention](commits-and-branching.md#commit-messages); it is
  review-enforced, not hook-enforced.

### Tests & Validation

- Ensure all tests pass — the [CI pipeline](continuous-delivery.md#triggers--required-checks) will block merge if any required check fails.
- For changes that depend on real infrastructure (UI, data migrations, external APIs, infra/config), validate in a non-prod environment before requesting review — note the steps in the PR description.

### Before Submitting

- Sync your branch with `main` and resolve any conflicts — [rebase before opening the PR](commits-and-branching.md#commit-hygiene) so they're caught locally, not at review time.
- Validate that your implementation follows the team's coding best practices.
- Run `make check` before handoff; there is no project pre-push hook.

---

## Code Review

A code review is an analysis and validation of the changes proposed in a PR. It is performed by one or more peers before integration into the main branch, in order to detect errors, ensure quality, and share knowledge.

### Why Good Reviews Matter

Peer code reviews improve code quality, help maintain consistency and robustness, and catch bugs earlier through external perspectives.

**Core principles:**

- Critique the code, not the person's abilities.
- Assume good faith from contributors.
- Highlight what is done well, not just the errors.

### Who Should Review

A pull request should be reviewable by anyone, regardless of seniority level. This encourages better knowledge sharing and brings different perspectives on implementations.

### Review Best Practices

#### Response Time

- Aim for a first response within **24 to 48 hours**.
- A PR that lingers loses its context and slows down the team.

#### General Approach

- A PR should be easy to follow and understandable.
- If you find yourself asking many questions and struggling to understand what is happening, that is a **red flag**. Schedule a time to talk directly with the PR author(s).
- Look at files in their entirety, not just the modified lines.
- Ask yourself: does this PR improve the overall health of the code, or does it add complexity?

#### Commenting

- Use a constructive tone.
- Prefer questions over assertions (*"Have you considered..."* rather than *"This is wrong"*).
- Propose solutions or alternatives when possible.
- Don't hesitate to comment when something is done well.

#### What to Avoid

- Never block a PR for personal preferences.
- Do not impose style changes not defined by the team.

> Source: [What to look for in a code review](https://google.github.io/eng-practices/review/reviewer/looking-for.html)

### LLM-Assisted Reviews

LLMs (Claude Code, Copilot, Cursor) can catch obvious issues — typos, dead code, style nits — before a human reviewer engages. Use them, with limits.

- **Never replace human review.** LLMs miss contextual nuance and broader-system implications. A human approval is required before merge.
- **Validate every suggestion.** Technically-correct ≠ project-appropriate — suggestions may conflict with team style or an ADR the tool can't see.
- **The code lands under your name.** "The LLM said it was fine" isn't a justification.

---

## Review Checklist

### Functionality

- Does the code do what it is supposed to do? Validate against the attached story or task.
- Does the implementation match the intended behavior?
- Think about limitations and edge cases.

### Design

- Do the interactions between different parts of the code make sense?
- Could this code break a public contract, durable event, migration, or existing client? Record and
  review compatibility explicitly; there is no automated SemVer release process.
- Is this change in the right repository or the right part of the repository?
- Does it integrate well with the rest of the system?

### Complexity

- Can the code be understood quickly by another developer?
- Is there unnecessary over-engineering?
- Does the PR solve the right problem, and not potential or nonexistent problems?

### Tests

- Do the tests cover all crucial elements of the new code?
- Will the tests actually fail if the code is broken? (watch out for false positives)
- Are the tests simple, useful, and well-separated?
- Check for unnecessary redundancy.
- Are integration, E2E, or other tests needed beyond unit tests?

### Naming

- Are variable, function, and class names clear and informative?
- Avoid meaningless names (e.g., `data`, `temp`, `x`).

### Comments

- Are the comments relevant and do they explain the *why*?
- Are there unnecessary comments to remove or update?
- Are there TODOs that can be resolved now? (often indicators of technical debt)
- Is the code clear enough to not need comments?

### Documentation

- Are function and class docstrings clear and do they explain behavior well?
- If behavior changes, is the documentation (README, etc.) updated?
- If code is removed, should the associated documentation be removed too?

### Style

- Does the code pass the repository's Go/TypeScript lint rules and follow nearby conventions?

---

## Handling Disagreements

If a disagreement persists after a few exchanges:

1. **Discuss in person** (or a call) — text can lose nuance.
2. **Involve a third team member** for an external opinion.
3. **Remember the common goal**: deliver quality code, not be right.
