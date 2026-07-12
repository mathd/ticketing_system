# Commits & Branching

## Conventional Commits

This project uses the [Conventional Commits](https://conventionalcommits.org) specification, enforced by [Commitizen](https://commitizen-tools.github.io/commitizen/) via pre-commit hooks.

**Format:**

```
type(scope): description

[optional body]

[optional footer]
```

**Types:**

| Type | Purpose | SemVer |
|---|---|---|
| `feat` | New feature | MINOR |
| `fix` | Bug fix | PATCH |
| `docs` | Documentation only | — |
| `refactor` | Code change without behavior change | — |
| `style` | Formatting, whitespace (no logic change) | — |
| `test` | Adding or updating tests | — |
| `ci` | CI/CD changes | — |
| `chore` | Maintenance (deps, config, tooling) | — |
| `perf` | Performance improvement | — |
| `build` | Build system changes | — |
| `revert` | Reverting a previous commit | — |

**Scope** is a noun describing the area affected: `feat(auth):`, `fix(api):`, `docs(readme):`.

**Breaking changes** — use `!` after the type/scope:

```
feat(api)!: remove deprecated endpoint
```

Or add a `BREAKING CHANGE:` footer for details.

**Rules:**
- Imperative mood: *"add login flow"* not *"added login flow"*
- Subject under 50 characters
- Body explains the *why*, not the *what* — wrap at 72 characters
- Reference tickets in footer: `Refs: PROJ-1234`

**Examples:**

```bash
# Good
feat(auth): add OAuth2 PKCE flow
fix(pipeline): handle null values in feature extraction
docs(adr): add ADR-003 for vector DB selection

# Bad
fixed stuff
update
WIP
feat: Add New Amazing Feature That Does Many Things At Once
```

> See also: [Conventional Commits spec](https://www.conventionalcommits.org/en/v1.0.0/)

---

## Branch Naming

**Format:**

```
type/ticket-id-short-description
type/short-description
```

**Prefixes:**

| Prefix | When |
|---|---|
| `feature/` | New functionality |
| `bugfix/` | Non-urgent bug fix |
| `hotfix/` | Urgent production fix |
| `refactor/` | Code improvement, no behavior change |
| `docs/` | Documentation only |
| `chore/` | Tooling, deps, config |

**Rules:**
- Lowercase only
- Hyphens as separators (no underscores, no camelCase)
- Keep under 50 characters
- Include ticket ID when available

**Examples:**

```bash
# Good
feature/proj-456-dark-mode-toggle
bugfix/gh-789-pagination-offset
hotfix/memory-leak-image-upload
docs/add-adr-003

# Bad
Feature/User-Auth        # wrong case
fix_login_issue          # underscores
johns-branch             # no type, personal name
feature/updates          # too vague
```

> See also: [Conventional Branch spec](https://conventional-branch.github.io/), [Graphite guide](https://graphite.com/guides/git-branch-naming-conventions)

---

## Branching Strategy

**Default: [GitHub Flow](https://docs.github.com/en/get-started/using-git/github-flow)**

1. `main` is always deployable
2. Create a short-lived branch from `main`
3. Open a PR, get review, merge
4. Deploy

Simple, PR-centric, and fits consulting projects where traceability and client visibility matter.

**When to consider alternatives:**

| Strategy | When | Trade-off |
|---|---|---|
| [Trunk-based](https://trunkbaseddevelopment.com/) | High-velocity teams with strong CI, feature flags | Requires robust test automation |
| [Gitflow](https://nvie.com/posts/a-successful-git-branching-model/) | Versioned libraries, scheduled releases, regulated industries | Complex, merge-heavy |

> Even Gitflow's creator [recommends simpler workflows](https://nvie.com/posts/a-successful-git-branching-model/) for continuously delivered software.

---

## Commit Hygiene

**Atomic commits** — each commit is one logical change that leaves the codebase in a working state. If you need to cherry-pick or revert it, it should work in isolation.

**PR size** — Using LLMs make large changes easier, but big PRs get lower-quality reviews. Split work into focused commits and aim for ~250 lines across a few files. See [Pull Requests & Code Review](./pull-requests.md) for more details.

**Squash vs merge:**
- **Squash** when your branch has messy WIP commits — produces a clean single commit on `main`
- **Keep commits** when each one is already atomic and meaningful (e.g. a PR with three distinct logical changes)

**Rebase:**
- Rebase your branch onto `main` before opening a PR to catch conflicts early
- Never rebase shared/public branches — it rewrites history and breaks collaborators
- Never force-push to `main`

---

## AI/ML Considerations

**Data & model versioning** — never commit large files (datasets, model weights) to Git. Use:
- [DVC](https://dvc.org/) — Git-integrated, stores pointers in Git, data in remote storage (S3, GCS)
- [Git LFS](https://git-lfs.com/) — for binary files like model checkpoints

**Notebook hygiene** — Jupyter notebooks create noisy diffs. Use:
- [nbstripout](https://github.com/kynan/nbstripout) — pre-commit hook that strips cell outputs
- [Jupytext](https://jupytext.readthedocs.io/) — converts notebooks to reviewable `.py` files

**Prompt versioning** — treat prompts like code. Additionally, if you store prompts within a codebase repository:
- Store prompts as files in Git alongside code
- Version changes with conventional commits: `feat(prompts): update system prompt for summarization`
- Run regression tests (golden Q&A pairs) on prompt changes in CI