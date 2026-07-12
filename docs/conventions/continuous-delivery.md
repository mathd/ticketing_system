# Continuous Delivery

These conventions cover how code moves from commit to running in production: the pipeline, hooks, release flow, and deployment strategy.

The goal is to keep `main` in a state where it can be deployed at any time. This is the **Continuous Delivery** approach described in *Continuous Delivery: Reliable Software Releases through Build, Test, and Deployment Automation* (Humble & Farley, 2010). We prefer it when feasible for three reasons:

- **Lower deployment risk** — small, frequent changes are easier to test, review, and roll back than batched releases.
- **Faster feedback** — defects surface within minutes of being introduced, not weeks.
- **Reduced release overhead** — releases become routine operations rather than crisis events.

> **These are guidelines, not mandates.** The tools and patterns documented here (GitHub Actions, Docker, Commitizen, uv) are the defaults of this template. Individual projects can deviate when there's a real reason — different deployment target, regulatory constraints, client tooling, etc. When you deviate, capture the decision in an [ADR](../adr/) so the next dev understands why.

For the high-level engineering principles this builds on, see [technical-delivery-standards.md](../technical-delivery-standards.md).

---

## Workflow Structure

GitHub Actions workflows live in `.github/workflows/`. Composite actions (shared setup steps) live in `.github/actions/`.

**File naming:**

| File | Purpose |
|---|---|
| `ci.yaml` | Quality gates on every PR and push to `main` (lint, types, tests, build) |
| `release.yaml` | Triggered by a `v*` tag push — builds release artifacts, creates GitHub Release |
| `deploy-<env>.yaml` | One per deployment environment (e.g. `deploy-dev.yaml`, `deploy-prod.yaml`) |
| `nightly.yaml` | Scheduled heavy jobs (E2E, image scans, perf) |

**Rules:**

- One workflow per concern — don't pile deploy steps into `ci.yaml`.
- Setup logic that repeats across jobs goes into a composite action under `.github/actions/` (see the existing `setup-project`).
- Workflow names are kebab-case; the `name:` field is human-readable Title Case.

---

## Triggers & Required Checks

The shipped `ci.yaml` runs on PRs and on direct pushes to `main` / `develop`, plus manual `workflow_dispatch`. These automated checks complement the human gate of [code review](pull-requests.md#code-review) — a PR can only merge when both pass.

| Job | What it does | Blocking for merge to `main`? |
|---|---|---|
| `code-quality` | `pyright` types, `ruff` lint, `ruff format`, pre-commit | **Yes** |
| `test` | `pytest` with coverage | **Yes** |
| `build` | Docker image build (`docker/build-push-action`) | **Yes** |

**Branch protection (configure in GitHub repo settings):**

- `main` requires all three checks above to pass
- At least one review approval (see [pull-requests.md](pull-requests.md))
- No direct pushes — PRs only
- No force-pushes

---

## Speed Conventions

> **Target: PR pipeline under 5 minutes.** Slow pipelines erode trust and push devs to "skip CI."

- **Parallelize** quality checks. The shipped `code-quality` job runs `types`, `lint`, `format`, and pre-commit with `if: always()` so one failure doesn't mask the others.
- **Sequence only what's necessary.** The `build` job depends on `code-quality` + `test` — don't burn build minutes on broken code.
- **Cache aggressively** via the `setup-project` composite action (uv cache, Python install).
- **Move heavy work off the PR path.** E2E, image scans, dependency audits, perf benchmarks → nightly cron or `workflow_dispatch`.

---

## Pre-commit ↔ CI Alignment

Pre-commit hooks (see [`.pre-commit-config.yaml`](../../.pre-commit-config.yaml)) run locally on every commit and at `pre-push`. CI re-runs them via `pre-commit-ci/lite-action` to catch hooks devs skipped (`--no-verify`) or hooks that were updated after the last `pre-commit install`.

**Rules:**

- **Pin the same versions everywhere.** A hook's `rev:` in `.pre-commit-config.yaml` and the equivalent dev dependency in `pyproject.toml` (e.g. `ruff`, `pyright`, `commitizen`) must match — local and CI must produce the same result.
- **Don't add a CI step for what pre-commit already does.** If a check runs in pre-commit, the `pre-commit-ci/lite-action` step is enough — don't re-implement it as a separate job.
- **Fast hooks only in pre-commit.** Anything over ~5 seconds total belongs in CI, not in the commit loop.

---

## Secrets in CI

| Use | How |
|---|---|
| Tokens, API keys | GitHub Actions repository or environment secrets |
| Cloud auth (GCP, AWS, Azure) | **OIDC federation** — no long-lived service account keys (see [Workload Identity](security.md#workload-identity)) |
| Sensitive runtime values | `::add-mask::` so they don't leak in logs |

Detailed guidance lives in [security.md](security.md#secrets-management).

---

## Artifact Versioning

Every deployable artifact (Docker image, release tarball, etc.) needs two pieces of information:

| Identifier | Purpose | Example |
|---|---|---|
| **Git SHA** | Immutable traceability — exactly which commit is running | `sha-abc1234` |
| **Semver tag** | Human-readable release identity, drives changelog and rollback | `v1.4.2` |

For Docker images, tag every build with both where applicable:

```
app:sha-abc1234    # immutable, one per commit — what continuous deployment uses
app:v1.4.2         # only on tagged releases — what staging/prod reference
app:latest         # mutable, points to the most recent main build (use sparingly)
```

The combined PEP 440 form `1.4.2+sha.abc1234` is useful for the `__version__` string in code — the part after `+` is build metadata, doesn't affect version ordering, but preserves the link back to the source commit.

---

## Releases

Tagged releases are driven by [Conventional Commits](commits-and-branching.md#conventional-commits) and [Commitizen](https://commitizen-tools.github.io/commitizen/). The config in `pyproject.toml` (`[tool.commitizen]`) uses:

- `version_provider = "pep621"` — version read from `[project].version` (single source of truth)
- `tag_format = "v$version"` — tags look like `v1.4.2`
- `update_changelog_on_bump = true` — `CHANGELOG.md` is regenerated on every bump

**Flow:**

```bash
# From an up-to-date main
uv run cz bump        # bumps [project].version, regenerates CHANGELOG.md, creates v$version tag
git push --follow-tags
```

Pushing the `v*` tag triggers `release.yaml`, which builds release artifacts and publishes a GitHub Release with notes from `CHANGELOG.md`.

**Rules:**

- **Only bump from `main`** — never from a feature branch.
- **Never edit `[project].version` by hand.** Always go through `cz bump`.
- **Breaking changes** (commits with `!` or `BREAKING CHANGE:` footer) bump MAJOR — see the [SemVer table](commits-and-branching.md#conventional-commits).

---

## Continuous Deployment

Every commit on `main` ships to `dev` automatically, traceable by its git SHA. Higher environments (`staging`, `prod`) promote by semver tag.

| Environment | Trigger | Artifact identifier | Approval |
|---|---|---|---|
| `dev` | Merge to `main` | `app:sha-<short>` | None |
| `staging` | `v*` tag push | `app:v<version>` | None |
| `prod` | Manual promotion from `staging` | `app:v<version>` | Required reviewer |

> **Note:** The "Required reviewer" for `prod` is a **deployment approver** — a separate gate from the [PR code reviewer](pull-requests.md#code-review). The two roles may overlap (same person) or be distinct (e.g., SRE/ops approves deploys, engineering approves code), depending on the team.

**Why both SHA and tag identifiers?**

- The **SHA-based deployment to `dev`** gives every commit a real environment to be observed in — feedback within minutes, not at the next release.
- The **tag-based promotion to `staging` / `prod`** gives operators a stable, human-readable identifier to deploy and roll back against.

**Build once, deploy many.** The Docker image built at CI time is the same image promoted through every environment — no re-builds between `dev`, `staging`, and `prod`. This is a core CD principle from Humble & Farley: the artifact tested in `dev` must be byte-for-byte identical to what runs in `prod`.

**Implementation:**

- One workflow per environment under `.github/workflows/deploy-<env>.yaml`.
- Use [GitHub Deployment Environments](https://docs.github.com/en/actions/deployment/targeting-different-environments/managing-environments-for-deployment) to scope secrets and require approvals per environment.
- Production secrets live in the `prod` environment only — they must not be readable from `ci.yaml` or non-prod jobs.

---

## Dependency Updates

Use **Dependabot** or **Renovate** to keep dependencies current.

| Update type | Policy |
|---|---|
| Patch (`x.y.Z`) | Auto-merge after CI passes |
| Minor (`x.Y.0`) | Manual review |
| Major (`X.0.0`) | Manual review + smoke test |
| Security advisories | Triage within 24h, regardless of type |

GitHub Actions versions (`uses: actions/checkout@v4`) are updated the same way — pin to a major version, let the tool handle minor/patch bumps.

---

## Failures & Re-runs

**On a failure on `main`:**

- Alert the team channel (Slack/Teams) — failed `main` pipelines block everyone's releases.
- Investigate before re-running. **Don't blanket-retry.**

**On a flaky job:**

- First flake: open an issue, link to the failed run.
- Recurring flake: triage as a bug, not "re-run until green." Document the root cause in [LEARNINGS.md](../LEARNINGS.md) once resolved.

**Re-runs are tracked.** GitHub records the re-run count on each workflow run — high re-run counts on a single workflow are a signal to fix the underlying flake, not a measure of resilience.
