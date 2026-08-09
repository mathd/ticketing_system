# Code Review

**Date**: 2026-07-13  
**Scope**: Full M1 codebase, tests, build/CI configuration, `README.md`, `AGENTS.md`,
`docs/architecture.md`, and the documentation tree as the source of truth for M2.

## Summary

M1 is a credible walking skeleton: the service boundaries are visible, money uses integer minor
units, the inventory claim and ticket redemption paths have real PostgreSQL contention proofs,
the money/ticket trails are append-only, and `make check` passes end to end. The baseline is not
yet clean enough to start M2, however. One concurrent checkout race can return a guest capability
that was never persisted, the non-Catalog OpenAPI contracts are not enforced against handlers,
and several repository-level documents contradict the running stack or are untouched Python/AI
template material.

Review evidence:

- `make check`: PASS (lint, unit tests, builds, Compose smoke, concurrent append verification,
  and journal corruption checks).
- `go test -race ./...`: PASS for the local unit-test modules; this does not exercise the
  PostgreSQL stores, which mostly have no package-level tests.
- Worktree was clean before and after the review.

## Strengths

- The five-service ownership model is clear in code and mostly respected; database credentials
  enforce cross-service isolation in the smoke suite.
- Inventory capacity and Access redemption use database locking at the authoritative write seam,
  with black-box concurrent tests proving no oversell and one-time redemption.
- The Payments journal has canonical serialization, per-organizer serialization, immutable rows,
  deterministic replay, PII field guards, and active corruption checks in the local gate.
- Internal mutation routes fail closed behind a service credential and the gateway explicitly
  hides internal Inventory routes.
- The gate is unusually strong for a young repository: generated Catalog contract drift, lint,
  tests, native and container builds, smoke behavior, telemetry, and gate self-tests are covered.
- ADR-010 through ADR-012 state the walking-skeleton reliability boundaries instead of implying
  production guarantees that do not yet exist.

## Recommendations

### Critical (Must Fix)

- [x] **R1 — Make checkout completion a compare-and-return transaction**: Two same-key checkout
  requests can both observe a non-completed order. After the first commits `guest_order_ref`, the
  second can update zero rows, ignore `RowsAffected`, publish and return its newly generated but
  unpersisted reference. Move completion behind a locked/CAS operation that either writes and
  returns the new reference or reloads and returns the already-persisted reference; publish only
  the persisted value. Add a real-PostgreSQL test with concurrent identical checkouts that asserts
  one durable order reference and a retrievable ticket bundle. Files:
  `services/commerce/internal/api/server.go:425`, `services/commerce/internal/api/server.go:435`,
  `services/commerce/internal/api/server.go:443`, `smoke/checkout_test.go`.
  **Resolved in TKT-46:** completion now locks the canonical order row, checks both state
  transitions, and returns only the committed reference. A real-PostgreSQL store race plus the
  end-to-end same-key checkout/bundle smoke path prove convergence.

### Important (Should Fix)

- [x] **R2 — Enforce every committed OpenAPI contract in the gate**: Catalog alone uses generated
  handlers plus request/response contract validation. Inventory and Access merely serve embedded
  YAML, while Commerce and Payments do not even serve their committed specs; a green build can
  therefore ship handler/spec drift. Add served-spec checks, request validation, response contract
  tests, and deterministic generation/drift checks for all four services, preserving internal-only
  routes where applicable. Files: `Makefile:23`, `services/inventory/api/openapi.yaml`,
  `services/commerce/api/openapi.yaml`, `services/payments/api/openapi.yaml`,
  `services/access/api/openapi.yaml`.
  **Resolved in TKT-46:** all four contracts now define operation IDs plus request, success, and
  material error bodies; generated models are drift-checked; every service validates requests and
  responses at its router boundary; smoke asserts byte-identical served specs, schema completeness,
  observed response conformance, generic internal-route denial, and all Inventory transition
  denials.

- [x] **R3 — Stop retrying permanently invalid Access events forever**: Syntactically valid but
  semantically invalid/unsupported `order.completed` events enter an unbounded one-second NAK loop
  because validation and transient store/delivery failures share one error path and `MaxDeliver`
  is unlimited. Classify permanent contract errors and terminate or route them to a failed-event
  subject; bound transient retries and expose failure metrics. Add tests for malformed identifiers,
  unsupported schema, and transient store failure. Files:
  `services/access/internal/consumer/consumer.go:68`,
  `services/access/internal/consumer/consumer.go:137`,
  `services/access/internal/consumer/consumer.go:150`.

  Resolved on TKT-46: Access now separates permanent contract failures from transient processing
  failures, uses a finite JetStream delivery/backoff policy, publishes sanitized failed-event
  records before termination, emits low-cardinality metrics, and scopes delivery retries to the
  affected order so a poison order cannot block later checkouts. Unit tests and the real-NATS smoke
  suite cover permanent rejection, publication failure, exhaustion, sanitization, metrics, and
  recovery of subsequent valid traffic.

- [x] **R4 — Add the Access forward-upgrade migration test already captured by TKT-44**: Start a
  real database at migration 0001 with issued/delivered history, apply 0002, and prove history is
  preserved and redemption succeeds. This is the supported rollback posture for immutable ticket
  history and currently has no automated proof. Files:
  `services/access/internal/store/migrations/0001_tickets.sql`,
  `services/access/internal/store/migrations/0002_redeemed_lifecycle.sql`.

  Resolved on TKT-46: a smoke-tagged Access store test applies the exact embedded migrations to an
  isolated schema in the real Compose PostgreSQL instance, seeds issued/delivered history at 0001,
  upgrades to 0002, proves identifiers and timestamps are preserved, redeems the ticket, and
  confirms lifecycle immutability remains enforced. The targeted real-database test passes; the
  subsequent full smoke run reached the separately recorded R5 forged-token fixture defect.

- [x] **R5 — Complete the scanner hardening already captured by TKT-45**: Catch asynchronous
  `BarcodeDetector.detect()` failures inside the animation loop, stop the media stream, tell the
  operator why camera scanning stopped, and restore the paste path. Replace last-character token
  mutation with deterministic signed-byte corruption in tests. Files: `web/scanner/src/App.tsx:83`,
  `web/scanner/src/App.test.tsx`, `services/access/internal/ticket/token_test.go`.

  Resolved on TKT-46: the camera preview now exists before activation, asynchronous detector and
  startup failures stop all media tracks, restore controls, and direct the operator to the working
  paste path. UI coverage exercises detector rejection through successful paste fallback. Access
  unit and checkout smoke tests now decode the Ed25519 signature, flip a signed byte, and re-encode
  it, removing the base64url unused-bit flake. The full `make check` gate passes.

- [x] **R6 — Resolve PostgreSQL version authority in `AGENTS.md`**: The working agreement says
  PostgreSQL 17, while Compose, ADR-007, ADR-011, architecture, Docker docs, and dependency anchors
  say/use 18.4. Pick the accepted value and make the highest-authority agent instructions and ADR
  agree; do not leave future agents to infer which source may override the other. Files:
  `AGENTS.md:29`, `compose.yaml:33`, `docs/adr/ADR-007-postgres-nats.md:33`,
  `docs/adr/ADR-011-checkout-journal-protocol.md:57`.

  Resolved on TKT-46: PostgreSQL 18.4 remains the accepted ADR-007 and implemented Compose version.
  `AGENTS.md` now matches that authority, ADR-011 records the drift as resolved, and a repository
  scan found no remaining PostgreSQL 17 reference outside this historical review finding.

- [x] **R7 — Reconcile M1 documentation with the running system**: Mark M1 complete and M2 next in
  the roadmap; replace the README/architecture/development/Docker claims that Storefront is static,
  framework-free, or awaiting ADR-006; update the architecture heading from “as of US-001” and
  show the actual Catalog→Inventory and Commerce→Access event flows. Files: `docs/ROADMAP.md:5`,
  `README.md:38`, `docs/architecture.md:3`, `docs/architecture.md:20`,
  `docs/development.md:21`, `docs/docker.md:10`.

  Resolved on TKT-46: the roadmap marks M1 complete and M2 capability selection next; README,
  development, and Docker docs describe the Astro 7 SSR/React storefront and React/Vite scanner.
  The architecture page is now the completed M1 baseline and shows the durable Catalog→Inventory
  and Commerce→Access JetStream flows. `docker compose config` and touched-document local-link
  validation pass, with no remaining framework-pending/static-Storefront claim.

- [x] **R8 — Remove or explicitly quarantine obsolete template documentation**: `solution-design`,
  `technical-delivery-standards`, `conventions/security`, and `conventions/continuous-delivery`
  still prescribe Python/FastAPI/Pydantic/Ruff/pytest/uv, Jira/Confluence, pre-commit hooks,
  deployment workflows, and files that do not exist. Rewrite the applicable guidance for this
  Go/TS testbed or move the templates out of the project documentation index so agents cannot treat
  them as project instructions. Files: `docs/solution-design.md`,
  `docs/technical-delivery-standards.md`, `docs/conventions/security.md`,
  `docs/conventions/continuous-delivery.md`.

  Resolved on TKT-46: all four documents were rewritten as project-specific Go/TypeScript,
  OpenAPI, Compose, local-board, and `make check` guidance. Linked commit/PR conventions, the ADR
  template, docs index, and stale product-brief feasibility note were reconciled too. Repository
  link validation passes; remaining Jira/Confluence/hook/tool names only state explicitly that
  those systems are not configured, while historical accepted ADR context remains intact.

- [x] **R9 — Pin executable supply-chain inputs**: Replace `natsio/nats-box:latest`, untagged
  `grafana/otel-lgtm`, floating Alpine image tags, and mutable major-only GitHub Action references
  with reviewed versions/digests; add automated Go, pnpm, container, and workflow dependency scans.
  The current security docs claim Dependabot/detect-secrets/SAST coverage that is not configured.
  Files: `compose.yaml:67`, `compose.yaml:80`, `build/go.Dockerfile`,
  `web/scanner/Dockerfile`, `web/storefront/Dockerfile`, `.github/workflows/check.yaml`, `.github/`.

  Resolved on TKT-46: all Compose and Dockerfile images now carry exact tags and verified
  multi-platform digests; Actions and scanner binaries are pinned to immutable revisions. Weekly
  Dependabot coverage spans all Go modules, the pnpm workspace, workflows, Compose, and Dockerfile
  surfaces. The new read-only security workflow runs govulncheck, pnpm audit, and Trivy on PRs,
  weekly, and manually. Enabling it exposed and fixed the module patch-level declaration, a stale
  vulnerable smoke dependency, and root frontend runtimes. `make check`, all image builds, local
  advisory scans, and Trivy HIGH/CRITICAL dependency/configuration/secret scanning pass.

### Suggestions

- [x] **R10 — Establish bounded database and HTTP resource policies before load-oriented M2 work**:
  Configure `SetMaxOpenConns`, `SetMaxIdleConns`, connection lifetime/idle time, and server
  `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` consistently, with values exposed as documented
  configuration. Defaults are not a safe capacity contract for the Inventory and Commerce hot
  paths. Files: `services/*/cmd/*/main.go`, `gateway/cmd/gateway/main.go`,
  `docs/configuration.md`.

  Resolved on TKT-46: a tested shared runtime policy now applies positive, validated HTTP read
  header/read/write/idle timeouts to every service and the gateway, plus bounded open/idle database
  pools and connection lifetime/idle limits to all five services. Compose forwards documented
  overrides, invalid or contradictory values fail startup, and `make check` passes.

- [x] **R11 — Standardize strict JSON decoding on public write endpoints**: Use one bounded decoder
  that rejects unknown fields and trailing JSON. Commerce and Payments reject unknown fields but
  accept trailing documents; Inventory accepts both; Access already checks both. Add table tests so
  client contract mistakes fail consistently. Files: `services/commerce/internal/api/server.go:56`,
  `services/payments/internal/api/server.go:47`, `services/inventory/internal/api/server.go:55`,
  `services/access/internal/api/server.go:106`.

  Resolved on TKT-46: Inventory, Commerce, Payments, and Access now share one explicit-limit JSON
  decoder that rejects malformed or oversized bodies, unknown fields, and trailing values while
  accepting trailing whitespace. Access retains its tighter 8 KiB credential limit; the other
  paths retain 1 MiB. Shared rejection-matrix tests and endpoint regressions pass with `make check`.

- [x] **R12 — Remove stale scaffold comments and tracked generated Astro state**: Service entrypoint
  comments still say “No domain routes yet,” the Access header is corrupted by repeated ownership
  text, and two `.astro` files remain tracked despite `.astro/` being ignored. Update the headers and
  remove generated files from the index so code navigation reflects the M1 baseline. Files:
  `services/*/cmd/*/main.go:1`, `web/storefront/.astro/types.d.ts`,
  `web/storefront/.astro/content.d.ts`, `.gitignore`.

  Resolved on TKT-46: Inventory, Commerce, Payments, and Access now describe their actual M1
  ownership and implemented capabilities; the corrupted Access header is gone. The Astro files
  named by the finding were confirmed ignored and absent from both `origin/main` and repository
  history, so no index deletion or ignore-rule change was required.

- [ ] **R13 — Make review reports repository artifacts**: `docs/reviews/` is ignored even though
  the project says documentation is 100% in-repo and the review workflow writes its output there.
  Remove that ignore rule and link the latest review from `docs/README.md` so M2 tickets can cite
  this audit without relying on a local file. Files: `.gitignore`, `docs/README.md`.
