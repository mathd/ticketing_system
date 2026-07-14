# Continuous Delivery

## Current scope

The repository currently has continuous integration, not an application release or deployment
pipeline. Pull requests and pushes to `main` run the same deterministic quality gate used locally.
Docker Compose is the only runtime topology; no commit is automatically deployed to an environment.

## Workflow structure

GitHub Actions workflows live in `.github/workflows/`:

| Workflow | Trigger | Responsibility |
|---|---|---|
| `check.yaml` | every PR and push to `main` | `make check` plus the gate self-test |
| `hermetic.yaml` | weekly and build-surface PR changes | in-Docker `make smoke-hermetic` |
| `security.yaml` | every PR, weekly, and manual | Go/pnpm advisories plus repository misconfiguration and secret scanning |

Workflow actions are pinned to full commit SHAs with release comments; action-installed tools are
version-pinned too. Dependabot proposes weekly action updates, which still require review. Reusable
automation should only be extracted after real repetition appears.

## Triggers & required checks

`make check` is the merge gate and runs generation drift checks, Go/TypeScript lint and tests,
builds, and the isolated Compose smoke suite. `gate-selftest` independently confirms the gate fails
when lint, test, or build defects are seeded. Repository branch-protection settings are outside this
repo; do not claim a required-review/check policy that is not visible or verified. The security
workflow is a separate blocking signal: Go findings are reachability-aware, while pnpm and Trivy
fail on high or critical findings.

Run this locally before handoff:

```bash
make deps
make check
```

Use focused package tests while iterating. Do not weaken or skip a failing check to make a branch
green; diagnose flakes and record non-obvious fixes in `docs/LEARNINGS.md`.

## Pre-commit ↔ CI alignment

There is no pre-commit or pre-push hook framework configured. Local/CI alignment comes from both
calling the Makefile targets with pinned tool versions. Contributors may use personal hooks, but
they are not project policy and cannot replace `make check`.

## Artifacts and releases

The gate builds local service/frontend artifacts and smoke images only. The project does not publish
versioned images, tarballs, GitHub Releases, or a changelog, and it has no SemVer release process.
If M2 introduces published artifacts, define immutable identifiers, provenance, promotion, rollback,
and signing in an ADR and workflow before calling the process continuous delivery.

## Deployment

There are no dev, staging, or production environments. `docker compose up` starts a local stack;
`make smoke` owns and removes a separate `ticketing-smoke` project. Cloud credentials, deployment
approvals, environment secrets, and production rollback procedures are therefore not current
repository concepts.

## Failures and reruns

- Investigate a failed `main` or PR job before rerunning it.
- Treat recurring flakes as defects; do not rerun until green.
- Keep the fast and hermetic smoke paths behaviorally equivalent.
- When a future deployment pipeline exists, document its incident and rollback policy here rather
  than inheriting a template process.
