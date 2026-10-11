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
when lint, test, or build defects are seeded. A GitHub ruleset on `main` requires the `check` job;
see [Main ruleset](#main-ruleset). `gate-selftest` and the security workflow are not required
checks. The security workflow is a separate blocking signal for review: Go findings are
reachability-aware, while pnpm and Trivy fail on high or critical findings.

Run this locally before handoff:

```bash
make deps
make check
```

Use focused package tests while iterating. Do not weaken or skip a failing check to make a branch
green; diagnose flakes and record non-obvious fixes in `docs/LEARNINGS.md`.

### Main ruleset

`main` has a GitHub repository ruleset. Its payload is `.github/rulesets/main.json`. That file is the
source of truth, and the live ruleset is applied from the merged file.

- It requires a pull request and a successful `check` from GitHub Actions (`integration_id` 15368).
  It requires no approvals, no up-to-date branch, and no other check.
- The repository admin role has `always` bypass. On this personal repository, that role is only the
  owner, `mathd`. Agent sessions authenticate as `mathd`, so the host refuses nothing they do. The
  host refusal binds Dependabot and any future collaborator. `AGENTS.md` states the rule that agents
  follow.
- The refusal for an actor without bypass was executed on a twin ruleset with no bypass actor, not on
  `main` itself. TKT-324's closeout records the commands and the output.
- Every PR and every push to `main` runs `check.yaml`, with no path filter. A skipped workflow leaves
  a required check pending.

Apply the ruleset from the merged file:

```bash
gh api --method POST repos/mathd/ticketing_system/rulesets --input .github/rulesets/main.json
```

Read it back and compare it with the file:

```bash
gh api repos/mathd/ticketing_system/rulesets/<id>
gh api repos/mathd/ticketing_system/rules/branches/main
```

Compare `name`, `target`, `enforcement`, `conditions`, `bypass_actors` and `rules`. Ignore ids, URLs
and timestamps. Make sure that `current_user_can_bypass` is `always` for `mathd`. If it is `never`,
delete the ruleset immediately.

To roll back, delete the ruleset, then read `rules/branches/main` again and correct this section:

```bash
gh api --method DELETE repos/mathd/ticketing_system/rulesets/<id>
```

Nothing compares the live ruleset with the file automatically. Compare them by hand after each
change. The gate checks only the static invariants. `make check-check-workflow-trigger` reads the
files line by line, so it accepts only one block-style shape and refuses everything else:

- `on:` holds exactly `push` (only `branches: [main]`) and an empty `pull_request`.
- Each required context is the reported name of a `check.yaml` job: its `name:` when set, else its
  job id.
- That job holds only `name`, `runs-on` and `steps`. It has exactly one `run: make check` step, and
  no step has `if:` or `continue-on-error`.
- No other job in any workflow reports the same name.
- Flow style, quoted keys, duplicate keys, merge keys and a second YAML document are refused as
  unsupported. So is a job `name:` that contains an expression.

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
`make smoke` owns and removes a path-derived Compose project with deterministic shifted ports.
Cloud credentials, deployment approvals, environment secrets, and production rollback procedures
are therefore not current repository concepts.

## Failures and reruns

- Investigate a failed `main` or PR job before rerunning it.
- Treat recurring flakes as defects; do not rerun until green.
- Keep the fast and hermetic smoke paths behaviorally equivalent.
- When a future deployment pipeline exists, document its incident and rollback policy here rather
  than inheriting a template process.
