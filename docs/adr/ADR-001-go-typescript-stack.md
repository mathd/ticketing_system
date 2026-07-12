# ADR-001: Go backend services, TypeScript/React frontends

Date: 2026-07-12

## Status

Accepted

## Context

No stack exists (greenfield; the Python scaffold docs in `docs/` are template leftovers, not a decision). The system's hardest runtime problem is a high-contention inventory hot path (on-sales with zero oversell). The team is the owner + AI agents; the codebase must be one AI tooling works well in. Deployment is local Docker Compose only in v1. The owner chose the direction during discovery (2026-07-12); this ADR records it.

## Possible Solutions

- **Option 1 — Go backend + TypeScript/React frontends (chosen):**
    - Pros:
        - Straightforward concurrency primitives for the inventory hot path; small static binaries suit a many-service Compose estate.
        - Strong AI-generation coverage for both Go and TS/React.
    - Cons:
        - Two languages to maintain; Go's domain-modeling ergonomics (no sum types) make rich ticketing domains more verbose.
- **Option 2 — Kotlin/Java (JVM) + TS:**
    - Pros: richest domain-modeling ergonomics and ecosystem (money, fiscal libs).
    - Cons: heavier per-service footprint on a local Compose estate; slower feedback loops.
- **Option 3 — TypeScript full-stack:**
    - Pros: one language everywhere.
    - Cons: weakest story for the CPU-bound contention core; owner explicitly preferred a compiled backend.
- **Option 4 — Python (per existing docs scaffold):**
    - Pros: docs scaffold already assumes it.
    - Cons: weakest concurrency/performance fit for the hot path; scaffold is an accident, not a reason.

## Decision

We adopt **Go for all backend services** and **TypeScript + React for the three frontends** (storefront, back office, scanner). Money is represented as integer minor units + ISO currency code everywhere; floats are banned on money paths.

## Consequences

- **Positive:**
    - Contention core gets a language built for it; services stay cheap to run side-by-side locally.
    - Uniform toolchain per tier: one `make check` covers Go lint/test and TS lint/test/build (US-001).
- **Negative:**
    - Cross-tier types must be kept in sync via OpenAPI/contract tests — a standing tax on every boundary change.
    - `docs/{configuration,development,docker,testing}.md` (Python scaffold) are obsolete and must be rewritten as Go/TS equivalents (backlog: TKT-1 epic scope).

## References

- [brief](../product/brief.md) · [PRD](../product/prd-v1.md) · [ADR-002](./ADR-002-services-from-day-one.md)
