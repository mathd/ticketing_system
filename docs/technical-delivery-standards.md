# Technical Delivery Standards

These are the delivery rules actually used by this repository. `AGENTS.md` is the highest-level
working agreement; accepted ADRs bind architectural choices.

## Sources of truth

- Product briefs and PRDs: `docs/product/`
- Architecture decisions: `docs/adr/`
- Current topology and ownership: `docs/architecture.md`
- Ticket state and delivery context: the git-derived `.sdlc/` board
- API behavior: `services/*/api/openapi.yaml` plus implementation conformance tests
- Dependencies and executable versions: manifests, lockfiles, Dockerfiles, Compose, and workflows

Documentation is entirely in this repository. There is no Jira, Confluence, or external project
wiki attached to this testbed.

## Local and CI gate

Run `make deps` on a clean clone, then `make check`. CI invokes the same gate.

| Stage | Current tooling | Purpose |
|---|---|---|
| Generate | `oapi-codegen`, `openapi-typescript` | Detect committed contract/type drift |
| Dependency declarations | `go mod edit -json` over every module | One version per shared Go dependency (ADR-035) |
| Go lint | pinned `golangci-lint` | Static analysis for every Go module |
| TypeScript lint | `oxlint` | Frontend static analysis |
| Tests | `go test`, Vitest | Package and component behavior |
| Build | Go toolchain, Astro, Vite | Compile every service and frontend |
| Smoke | isolated Docker Compose project | Real gateway, PostgreSQL, NATS, web, and telemetry seams |

`scripts/gate-selftest.sh` proves that seeded lint, test, build, and dependency-drift defects fail
the gate. The drift case is preceded by a positive control asserting the clean baseline passes —
without it, a checker that always failed would satisfy its own seeded test.
`make smoke-hermetic` exercises in-container builds on the scheduled/path-triggered CI workflow.
There is no project pre-commit framework; run focused tests during development and `make check`
before handoff.

## Engineering rules

- Write or refine the spec before implementation; do not infer ticketing behavior that the ticket
  or PRD has not settled.
- Preserve the service ownership in ADR-002. Shared code is limited to cross-cutting policy and
  requires deliberate review.
- Use integer minor units and ISO currency on every money path; floats are forbidden.
- Treat money and ticket lifecycle history as append-only.
- Keep public/internal HTTP boundaries explicit and committed OpenAPI contracts enforceable.
- Use deterministic tests. Infrastructure behavior belongs in the isolated smoke seam, not timing
  assumptions or mocks that cannot prove database/bus semantics.
- Record significant or cross-cutting decisions as ADRs. Correct documentation in the same change
  when behavior or topology changes.

## Review and commits

Use ticket-prefixed branches created by the project workflow and focused conventional-style commit
messages. A PR must explain its ticket, risks, validation, and any ADR impact. Review correctness,
clarity, architecture, security, and operational recovery; AI assistance does not replace owner or
human approval at the workflow gates. See [`conventions/`](./conventions/) for the detailed local
conventions.

## Runtime baseline

Docker Compose is the only deployment topology. Services expose health/readiness endpoints, emit
structured logs, and export OpenTelemetry signals. Distroless Go images run as non-root; frontend
containers use their documented Node/nginx runtimes. Cloud infrastructure, release automation,
SLOs, and production alerting remain out of scope until explicitly designed.
