# ADR-009: Contract-first service APIs (OpenAPI + oapi-codegen) and the domain-event envelope

Date: 2026-07-12

## Status

Accepted (approved at the TKT-26 plan gate, 2026-07-12)

## Context

US-002 publishes the platform's first real public contract (catalog) through the gateway, and
emits the platform's first domain event on the PLATFORM stream (ADR-007). The gateway's route
table explicitly waits for "US-002's OpenAPI work". Whatever contract mechanics and event
envelope this story ships become the precedent every later story copies — so both are decided
deliberately here. Constraints: Go services + TypeScript frontends (ADR-001), contract tests
required per touched boundary (PRD quality gates), a gate that must catch drift between spec,
server, and clients.

## Possible Solutions

- **Option 1 — contract-first: the OpenAPI document is the source of truth; code is generated from it (chosen):**
    - Pros: the contract is reviewable before code exists; Go server interface (`oapi-codegen`) and TS types (`openapi-typescript`) are generated from the same document, so cross-language drift is impossible when generation is gate-enforced; runtime request validation comes free (kin-openapi middleware).
    - Cons: generation toolchain to pin and wire into the gate; generated code committed (diff noise, mitigated by a drift check instead of build-time generation).
- **Option 2 — code-first: annotate Go handlers, extract a spec:**
    - Pros: single-language workflow.
    - Cons: the spec becomes a build artifact nobody reviews; TS types lag behind; "contract test" degenerates into "whatever the server does is the contract".
- **Option 3 — no formal contract (hand-written clients):**
    - Cons: rejected outright — the PRD demands contract tests per boundary from US-002 on.

## Decision

We adopt **contract-first APIs**:

1. Each service's public contract lives at `services/<name>/api/openapi.yaml` (OpenAPI 3.x),
   the single source of truth, reviewed in PRs like code.
2. Go server interfaces and types are generated with **oapi-codegen v2** (pinned via the
   module's `tool` directive); TypeScript types use **openapi-typescript**, pinned at the
   workspace root because the applications use TypeScript 7. Generated files are committed;
   **`make generate` regenerates and the gate fails on drift** (`git diff --exit-code HEAD` on
   every output declared by `scripts/generate-api.sh`).
3. Requests are validated against the spec at runtime (kin-openapi middleware); handler tests
   validate **responses** against the spec, so conformance is tested in both directions.
4. The service serves its own contract (`GET /openapi.yaml`), publicly reachable through the
   gateway (`/api/<name>/openapi.yaml`); the smoke suite asserts the served spec is
   byte-identical to the committed file.
5. **Domain-event envelope** (first set by `platform.catalog.performance.published`): JSON
   `{id, type, occurred_at, schema, data}` — `id` unique per emission, `type` equals the NATS
   subject, `schema` an integer version of `data`'s shape, `data` the minimal identifying
   payload (IDs, not entity bodies). Subjects follow `platform.<service>.<entity>.<fact>`.
   Publishes are JetStream ack'd (`js.Publish`), never fire-and-forget core NATS.
   *When `schema` must be bumped — and why parse-compatibility is the wrong test — is decided by
   [ADR-017](./ADR-017-domain-event-schema-evolution.md), which amends this point.*

## Consequences

- **Positive:**
    - Contract review happens at the spec, before implementation; frontends and smoke tests
      consume generated, always-in-sync types.
    - The event envelope precedent is documented rather than accidental.
- **Negative:**
    - Two pinned generators join the toolchain; contributors must run `make generate` after
      spec changes (the gate tells them when they forget).
    - Publish-after-commit without an outbox means a crash between DB commit and JetStream ack
      can lose an emission — **recorded deferral**, owned by US-004's reliability/journal work
      (ADR-007 anticipated per-service outboxes "in later stories").

## References

- TKT-26 (US-002) · [ADR-001](./ADR-001-go-typescript-stack.md) · [ADR-002](./ADR-002-services-from-day-one.md) · [ADR-007](./ADR-007-postgres-nats.md)
- [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen) · [openapi-typescript](https://github.com/openapi-ts/openapi-typescript) · [kin-openapi](https://github.com/getkin/kin-openapi)
