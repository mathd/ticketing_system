<!-- TODO: Refactor this file when we position ourselves for our recommendation for diagram-as-code. Note: Not have diagram within a specific MD file. It's harder to maintain. -->
<!-- TODO: Split in two files to have infra architecture vs solution design architecture. -->
# Security Guidelines

## Secrets Management

**Never commit secrets.** Secrets are managed differently per context:

| Context | How | Tool |
|---|---|---|
| Local dev | **Prefer cloud secret manager via dev auth**; fall back to `.env` (always in `.gitignore`) | `gcloud auth application-default login`, python-dotenv |
| CI/CD | Pipeline secrets | GitHub Actions secrets, OIDC for cloud auth |
| Production | Cloud-native secret managers | GCP Secret Manager, AWS Secrets Manager, Azure Key Vault |

**Prefer cloud-native secrets managers for local dev too.** Authenticate with personal cloud credentials and let the application fetch secrets from the same source it uses in production — no `.env` distribution, local behavior matches prod. Reserve `.env` for initial setup, quick local testing, or offline work.

The `detect-secrets` pre-commit hook is configured to catch accidental leaks before they reach the repo.

## Workload Identity

The best secret is no secret. Wherever possible, use the cloud's identity system to authenticate workloads — there's nothing to leak, rotate, or expire.

| Cloud | Mechanism | Use case |
|---|---|---|
| **GCP** | Service accounts (attached to Cloud Run, GKE, GCE); Workload Identity Federation | Cloud → Cloud, GitHub Actions → GCP |
| **AWS** | IAM roles (attached to EC2, ECS, Lambda); IAM Roles Anywhere | Cloud → Cloud, on-prem → AWS |
| **Azure** | Managed Identity (attached to VMs, App Service, Functions) | Cloud → Cloud, GitHub Actions → Azure |
| **CI/CD** | OIDC federation (GitHub Actions → cloud provider) | CI → Cloud, no long-lived keys |

**Rules:**

- **No service account keys in code or CI.** If a workflow needs cloud access, configure OIDC federation — not a JSON key file.
- **Scope identities tightly.** Each workload gets its own identity with only the IAM bindings it needs — never reuse a generic "deploy" account.
- **Fall back to secrets only when unavoidable.** Vendor APIs without OIDC and legacy systems still need static credentials; store them in a secret manager (see [Secrets Management](#secrets-management)).

## Static Analysis (SAST)

| Tool | What it does | Where it runs |
|---|---|---|
| **Ruff `S` rules** | Bandit-equivalent security checks (SQL injection, hardcoded passwords, insecure functions) | Pre-commit + CI |
| **Semgrep** | Semantic code analysis with lower false positives than pattern matching | CI (recommended) |

> Ruff's `S` rules are already enabled in `pyproject.toml`. No need for standalone Bandit.

## Dynamic Analysis (DAST)

For projects exposing web APIs. Run against a staging environment, not on every PR (too slow).

| Tool | When to use |
|---|---|
| **OWASP ZAP** | Automated API scanning in CI (feed it your OpenAPI spec) |
| **Burp Suite** | Manual penetration testing engagements |

## Dependencies

**Before adding a dependency**, check that it's:

- **Actively maintained** — recent releases, responsive maintainers
- **Trusted** — significant adoption, well-known maintainer or org
- **Appropriately licensed** — compatible with the project's distribution model
- **Focused** — prefer narrow libraries over kitchen-sink ones (smaller surface = smaller attack surface)

Packages with no commits in 2+ years and open security advisories are a hard no. The cheapest vulnerability is the one you never added.

**Once installed**, scan continuously:

| Tool | What it does |
|---|---|
| **Dependabot** | Auto-opens PRs for vulnerable dependencies (enable on GitHub) |
| **pip-audit** | Scans against OSV vulnerability database, can generate SBOMs |
| **Snyk** | Deeper analysis + license compliance (for enterprise clients) |

> Lock files (`uv.lock`) with `--frozen` in Dockerfiles ensure reproducible builds.

## Container Security

The project Dockerfile already follows best practices (non-root user, multi-stage build, slim base image). Additionally:

- **Scan images in CI** with [Trivy](https://github.com/aquasecurity/trivy): `trivy image --severity HIGH,CRITICAL --exit-code 1 app:latest`
- **Pin base image digests** for reproducibility: `python:3.13-slim@sha256:...`
- **Verify no secrets leak** into the image (no `.env`, no credentials)

## Network Security

- **VPC / Private networking** — All production databases, model endpoints, and internal services behind private subnets
- **Private endpoints** — Use cloud-native private connectivity (GCP Private Service Connect, AWS PrivateLink, Azure Private Endpoint)
- **Ingress** — WAF (Cloud Armor, AWS WAF) in front of public APIs. Allowlist known IPs for admin endpoints
- **Egress** — Restrict outbound traffic. Services should only reach required destinations (Cloud NAT for controlled outbound)

## Access Control (RBAC)

For APIs, use FastAPI dependency injection for authorization:

- Encode roles in JWT tokens, validate with a `Depends()` function
- Use an external IdP (Keycloak, Auth0, GCP Identity Platform) for token issuance
- Apply least-privilege: define permissions per role, enforce at the route level

## Input Validation

- **Pydantic models** for all API inputs — request bodies, query params, path params
- **Parameterized queries** via SQLAlchemy — never use f-strings for SQL
- **Prompt injection** (GenAI) — validate and sanitize user inputs before passing to LLMs. Use template-based prompts, not raw string concatenation

## PII & Data Protection

- **Classify data** — tag fields as PII/sensitive using Pydantic field metadata
- **Encrypt at rest** — use cloud-native encryption (CMEK for storage and databases)
- **Mask in logs** — never log PII. Use structured logging processors to redact sensitive fields
- **Data retention** — define TTLs and implement automated deletion for data containing PII
- **Compliance** — consider applicable regulations (GDPR, Quebec Law 25, PIPEDA) for client data

## AI Coding Agents

Coding agents (Claude Code, Copilot, Cursor) accelerate development but expand the attack surface. Treat them as untrusted code paths that happen to run on your machine.

- **Never paste confidential data into prompts.** Client data, real secrets, PII, internal architecture details — none of it lands in agent context. Use redacted examples instead.
- **Restrict agent file access.** Configure the agent (via `AGENTS.md`, settings) to not read `.env`, `conf/prod/`, or other sensitive paths. Use the agent's permission system to gate destructive operations.
- **Validate every action before accepting it.** Edits, shell commands, file deletes — review the diff or command. Don't auto-approve.
- **You are responsible for what the agent does under your name.** A leaked secret committed via agent is still your leak. A force-push via agent is still your incident.

Pairs with [LLM-Assisted Reviews](pull-requests.md#llm-assisted-reviews) — accountability stays with the human.
