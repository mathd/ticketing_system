<!-- TODO: This file shouldn't ship in actual project repos — it's organizational knowledge, not project-specific.
The doc's own opening says "Organizational standards live in Confluence" but the file itself lives inside every templated project.
Move to a central home (Confluence / internal docs / template parent) and have project repos link to it rather than duplicate it.
-->
# Technical Delivery Standards

This document is complementary to the **Cadre de livraison de projet**, which defines the full lifecycle of a project. The delivery framework answers the question *"how we deliver"*. This vision answers the question **"how we code"**: the engineering practices, conventions, and tools that guide the daily work of technical teams.

- **Organizational standards** (conventions, guidelines, processes) live in Confluence.
- **Project-specific decisions and artifacts** (ADRs, technical documentation, AGENTS.md) live in the project's Git repository.
- The principles below describe what each repository should contain and how teams should work within it.

---

## Founding Principles

### 1. Code as the Master Source

All documentation, architecture decisions, and team conventions live in the Git repository, as close as possible to the code they describe. This ensures versioning, PR reviewability, and synchronization with the code. This is commonly known as the "Docs-as-Code" approach and can help reduce friction and improve collaboration.

See each project's README for its actual repository structure.

### 2. CI/CD: Automate Quality

The CI/CD pipeline is not just a deployment mechanism. It is the **guardian of quality**. Every PR must pass automated quality gates before merging. Pre-commit hooks ensure these checks run locally before push, eliminating unnecessary round-trips with CI.

| Gate | Tool | Purpose |
|------|------|---------|
| **Linting & formatting** | Ruff | Replaces Flake8, isort, and Black in a single fast tool. Zero tolerance for style debates — the formatter decides. |
| **Static typing** | Pyright | Mandatory type annotations on public interfaces. Types are living documentation. |
| **Automated tests** | pytest | Unit, integration, and contract tests. Minimum coverage required, measured by pytest-cov. |
| **Integrated security (DevSecOps)** | Dependabot, Snyk, Ruff security rules | Vulnerability analysis in the pipeline. |
| **Pre-commit hooks** | pre-commit | Ruff, Pyright, detect-secrets — configured in `.pre-commit-config.yaml` and run on every commit. |
| **Docker build** | Docker | Validate that the Dockerfile builds correctly and that tests pass inside the container. |

### 3. Conventional Commits & Semantic Versioning

Adopt the [Conventional Commits](https://conventionalcommits.org) specification to structure every commit message using the format: `type(scope): description`. This enables automatic changelog generation, semantic versioning (SemVer), and clear traceability of the project's evolution.

**Standard commit types:**

| Type | Purpose | SemVer Impact |
|------|---------|---------------|
| `feat:` | New feature | Increments MINOR |
| `fix:` | Bug fix | Increments PATCH |
| `docs:` | Documentation changes only | — |
| `refactor:` | Refactoring without functional change | — |
| `ci:` | CI/CD pipeline changes | — |
| `chore:` | Maintenance tasks (dependencies, config) | — |

### 4. PR Template & Structured Code Review

A standardized pull request template (`.github/pull_request_template.md`) ensures every PR contains the context needed for an effective review. Google's style guide emphasizes that a good review documents changes in the same CL as the code, keeping documentation up to date.

**Template sections:** Description, Motivation/Context, Type of change, Checklist (tests, docs, lint).

**ADR links:** Reference the architectural decision if the change stems from one.

**Screenshots / videos:** For any UI change.

For code review, adopt a structured three-level approach:

1. **Correctness** — Does the code work? Do the tests pass?
2. **Clarity** — Is the code readable, well-named, and free of unnecessary complexity?
3. **Architecture** — Are the abstractions appropriate? Does the design respect the ADRs?

### 5. Architecture Diagrams

Every project must produce and maintain architecture diagrams in the repository (see [architecture.md](./architecture.md)). Diagrams are living documents — they are updated as the system evolves and reviewed alongside code changes.

**Required diagrams:**

| Diagram | Must Cover |
|---|---|
| **System Architecture** | Inputs & outputs, network & security, application layer, AI/ML components, data, external integrations, ops & observability |
| **Cloud Infrastructure** | Cloud resources, network & security (VPC, subnets, IAM), environment isolation (dev, staging, prod) |
| **Data Pipeline** | End-to-end data flow from ingestion to serving |

Use [Mermaid](https://mermaid.js.org/) as-code in the repo, or link to an external tool (draw.io, Miro). As-code diagrams are preferred — they are versioned, reviewable in PRs, and always in sync.

### 6. Architecture Decision Records (ADR)

Every significant architectural decision is captured in an ADR (Architecture Decision Record), stored as Markdown in the repository.

**Recommended format:** The Context / Decision / Consequences model, adopted by the industry and documented at [adr.github.io](https://adr.github.io). ADRs are never deleted — they are marked as `Deprecated` or `Superseded` with a reference to the new ADR.

### 7. Infrastructure as Code & Containerization

In a cloud context, infrastructure files are code in their own right. Dockerfiles, docker-compose.yml, Bash scripts, and Terraform/Pulumi modules live in the repository and follow the same rules of PR review, versioning, and CI. This eliminates "it works on my machine" and makes every environment reproducible.

- **Multi-stage Dockerfile:** A standard template for each project, optimized for image size and security (non-root user, .dockerignore).
- **Bash scripts in `scripts/`:** Setup, migrations, data seeding — documented and idempotent. No more commands shared on Slack.
- **Declarative infrastructure:** Cloud resources (Terraform, Pulumi, CloudFormation) are versioned and deployed via the CI/CD pipeline.
- **Secrets:** Never in the code. Use secret managers (AWS Secrets Manager, HashiCorp Vault) and detect-secrets in pre-commit.

### 8. Monitoring & Observability

Every production system must be observable. Instrument early — not after the first incident.

**The three pillars:**

| Pillar | What | Standard |
|---|---|---|
| **Logs** | Structured JSON events with context (`trace_id`, `request_id`) | Use structured logging in production. Never log PII or secrets. |
| **Metrics** | Numeric measurements over time (latency, error rate, throughput) | Expose RED metrics (Rate, Errors, Duration) for all services. |
| **Traces** | Request flow across services | Use [OpenTelemetry](https://opentelemetry.io/) — instrument once, export to any backend. |

**ML-specific:** Monitor data drift, prediction drift, and model performance over time. Define retraining triggers.

**GenAI-specific:** Track token usage and cost, response latency, and output quality. Use tracing tools ([Langfuse](https://langfuse.com/), LangSmith) for multi-step agent debugging.

**Alerting:** Alert on what matters — error rate spikes, latency breaches, drift thresholds. Avoid alert fatigue.

**Health checks:** Expose `/health` (liveness) and `/ready` (readiness) endpoints for all deployed services.

### 9. AGENTS.md

AGENTS.md is an open standard. It is a Markdown file placed at the project root that serves as a **"README for AI agents"**. It provides tools like Claude Code, GitHub Copilot, Cursor, and Codex with project-specific instructions: Python stack, conventions, commands (pytest, docker compose up, Bash scripts), and rules to follow.

Core elements AGENTS.md files usually cover:

1. **Commands** — How to build, test, and run
2. **Tests** — How to run tests, what framework is used
3. **Project structure** — Where things live
4. **Code style** — Conventions and linting rules
5. **Git workflow** — Branching, commits, PR process
6. **Boundaries** — What the agent must never touch (secrets, production configs, sensitive data)

---

## Implementation Plan

This framework will be materialized in a **reusable template repository** for each new engagement. The goal: a developer can clone the template and be productive in less than one hour.

| Phase | Actions | Deliverables |
|-------|---------|--------------|
| **1. Foundation** | Define conventions (commits, Ruff, PR). Create the repo template (pyproject.toml, Dockerfile, pre-commit). Write the first ADRs. | Repo template + AGENTS.md + Dockerfile. CI/CD pipeline (Ruff, mypy, pytest, Docker build). |
| **2. Adoption** | Pilot on 1–2 active projects. Train the team (lunch & learn). Gather feedback. | Adoption retro. Best practices guide v1. |
| **3. Scale** | Deploy across all engagements. Automate repo creation via agents. | Functional agentic workflow. Quality metrics tracked. |

---

## Sources & References

### Standards & Specifications

- [Conventional Commits](https://conventionalcommits.org)
- [Architecture Decision Records](https://adr.github.io) (Nygard, 2011)
- [AGENTS.md](https://agents.md) (Linux Foundation / Agentic AI Foundation)
- [Google Engineering Documentation Best Practices](https://google.github.io/styleguide/docguide)
