# Roadmap

Milestone state and upcoming capability work. Update this when scope changes.

## Completed

- **M1 — Walking skeleton** (TKT-1, US-001…US-006): five services + gateway + storefront + scanner up on Compose; one GA ticket travels create-event → contended reservation → fake-PSP checkout (append-only journal) → QR delivery → gate scan. See [PRD v1](./product/prd-v1.md).

## Next

- **M2 — Capability expansion:** prioritize one capability epic from the board Backlog at Gate 1;
  the owner decides the order. Early candidates by dependency weight are inventory and reservation
  core (TKT-4), pricing and rules (TKT-5), and read-path caching/hot-event serving (TKT-31).

## Later

- Real PSP adapter (TKT-10 tail), multi-tenant onboarding UX, cloud deployment, NF525 compliance profile for the French market (TKT-11, layers on the ADR-003 trail) — all explicitly out of v1 (see [brief](./product/brief.md) non-goals).
