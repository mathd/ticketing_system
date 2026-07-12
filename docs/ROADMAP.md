# Roadmap

Upcoming milestones. Update this when scope changes, not after the fact.

## In progress

- **M1 — Walking skeleton** (TKT-1, US-001…US-006): five services + gateway + storefront + scanner up on Compose; one GA ticket travels create-event → contended reservation → fake-PSP checkout (append-only journal) → QR delivery → gate scan. See [PRD v1](./product/prd-v1.md).

## Next

- Prioritize capability epics from the board Backlog (TKT-2…TKT-24) — owner decides order at Gate 1. Early candidates per dependency weight: inventory & reservation core (TKT-4), pricing & rules (TKT-5), read-path caching & hot-event serving (TKT-31).

## Later

- Real PSP adapter (TKT-10 tail), multi-tenant onboarding UX, cloud deployment, NF525 compliance profile for the French market (TKT-11, layers on the ADR-003 trail) — all explicitly out of v1 (see [brief](./product/brief.md) non-goals).
