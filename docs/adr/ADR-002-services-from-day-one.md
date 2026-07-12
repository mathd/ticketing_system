# ADR-002: Services from day one, in a coarse five-service cut

Date: 2026-07-12

## Status

Accepted

## Context

The owner decided during discovery (2026-07-12) that the system is built as separate services from the start — isolating the contention-heavy inventory core early is the point, and exercising AI-assisted development against a distributed estate is part of the testbed's purpose. Deployment is Docker Compose only; the team is one owner + AI agents. The risk to manage is integration tax: with ~24 capability epics, a fine-grained service cut would spend the project on plumbing.

## Possible Solutions

- **Option 1 — Modular monolith with enforced module boundaries:**
    - Pros: lowest integration tax for a solo/AI team; extract services later along proven seams.
    - Cons: rejected by the owner — does not exercise the distributed-development flow, and the inventory hot path is not isolated at runtime.
- **Option 2 — Fine-grained services (one per capability, ~12–15):**
    - Pros: maximal isolation.
    - Cons: integration/deployment tax overwhelms a Compose-based solo project; most boundaries would be guesses.
- **Option 3 — Coarse cut of five services (chosen):**
    - Pros: real network boundaries where they pay (inventory isolation, fiscal money path), few enough to hold in one head; each can split later.
    - Cons: some epics span services; boundary mistakes are costlier than in a monolith.

## Decision

We build **five Go services** behind an API gateway, with an event bus for the domain event stream:

| Service | Owns |
|---|---|
| `catalog` | organizers/tenants, venues, seat maps, events, performances, series/seasons, festival structure, rule definitions |
| `inventory` | every reservation model (GA, seats, entitlements/passes, lodging calendars, wristband media), holds, allocations — the single-writer contention hot path |
| `commerce` | cart, pricing/fee/promo evaluation, orders, post-purchase lifecycle |
| `payments` | PSP port (fake provider first), wallets/cashless, NF525 journal, settlement ledger |
| `access` | ticket issuance & delivery, scanning/redemption, pass & wristband validation |

Frontends: `storefront`, `backoffice`, `scanner` (TS/React). Everything runs under one Docker Compose file; every entity carries a tenant/organizer id from day one.

## Consequences

- **Positive:**
    - The oversell-critical path lives in one service with a single write authority — the core correctness claim is testable in isolation and under load.
    - NF525 mechanics concentrate in `payments` rather than smearing across the codebase.
- **Negative:**
    - Every walking-skeleton story pays a cross-service integration cost before domain logic exists (accepted knowingly; this is the highest-friction combination in the scope and it is deliberate).
    - Boundary changes require contract-test churn; wrong seams cost real migrations. Mitigation: coarse cut, split only under evidence.

## References

- [brief](../product/brief.md) (pre-mortem) · [PRD](../product/prd-v1.md) · [ADR-001](./ADR-001-go-typescript-stack.md)
