# Security Guidelines

This page separates controls enforced in the M1 repository from future production requirements.
The system is a local Docker Compose testbed, not a production deployment.

## Enforced today

- Secrets and local environment files are ignored; credentials in Compose are development-only.
- Each Go service has a separate PostgreSQL database and role. CONNECT is revoked across service
  databases and the smoke suite verifies credential isolation (ADR-007).
- Only the gateway publishes the application port. Infrastructure ports bind to `127.0.0.1`.
- The gateway exposes an explicit route table; `/internal/` service routes are denied publicly.
- Go service images use a distroless non-root runtime. Dependency lockfiles and module checksums are
  committed. Container images are pinned by exact tag and multi-platform digest; workflow actions
  are pinned by full commit SHA.
- Public contracts are request-validated; service tests validate response conformance.
- Money and ticket histories are append-only. QR tickets use a dedicated Ed25519 key namespace and
  redemption verifies signed immutable facts.
- Logs must not contain payment tokens, QR credentials, raw email addresses, or hostile failed-event
  bodies. Access logs only recipient/link hashes and publishes sanitized failure records.
- `make check` runs lint, tests, builds, and real-stack smoke checks in CI and locally.
- The `security` workflow runs on every pull request, weekly, and on demand. It fails on reachable Go
  vulnerabilities, high-or-critical pnpm advisories, or high-or-critical Trivy filesystem findings
  across dependencies, IaC configuration, and detected secrets.

## Input and data handling

- Use parameterized SQL through `database/sql`/pgx; never interpolate untrusted values into SQL.
- Decode JSON through the repository's bounded, strict request policy once available; retain
  endpoint-specific status semantics.
- Money is integer minor units plus currency. Do not log or serialize money through floating point.
- Keep PII in mutable service-owned stores and identifiers in immutable trails (ADR-003).
- Treat guest ticket references and QR payloads as bearer capabilities. Never include them in logs,
  metrics labels, failed-event records, or analytics events.

## Dependencies and executable inputs

Review maintenance, license, scope, and advisories before adding a dependency. Pin application
dependencies through Go modules and `pnpm-lock.yaml`; pin executable container/workflow inputs as
described in [`dependencies-and-versions.md`](dependencies-and-versions.md). Dependabot proposes
weekly updates but does not bypass review or the quality gates. Security findings are blocking at
the thresholds above; suppressions require a narrow, documented rationale with an expiry or removal
condition.

## Not yet production-ready

M1 deliberately lacks staff authentication/RBAC, ticket expiry/revocation/admission-window policy,
a real PSP, managed secrets, WAF/rate limiting, cloud network isolation, backups, disaster recovery,
and a production deployment pipeline. Relevant backlog items and ADRs must settle those controls
before external operation. Local Compose passwords and signing seeds must never be reused outside
development.

## AI-assisted development

- Do not place real secrets, customer data, or production credentials in prompts or fixtures.
- Review generated edits and commands before accepting them; the human owner retains accountability.
- Preserve repository and tool permission boundaries. Destructive or external actions require the
  authority defined by the active workflow.
- Validate security suggestions against current ADRs and code; generic framework advice is not a
  project control.
