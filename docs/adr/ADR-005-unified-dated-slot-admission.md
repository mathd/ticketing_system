# ADR-005: Unified dated-slot admission model

Date: 2026-07-12

## Status

Accepted

## Context

The system sells admission to fundamentally different-looking things: dated performances of shows, festival days, park operating days, and (indirectly) passes that grant entry to many of them. Parks have an operating calendar, not performances, so the catalog/inventory boundary needs a decision: force everything through one model or maintain parallel domains. The owner decided during the backlog grilling (2026-07-12). The decision shapes TKT-2 (catalog), TKT-12 (passes), TKT-13 (lodging), TKT-14 (festivals) and the inventory core's claim API, and is hard to reverse once inventory ships.

## Possible Solutions

- **Option 1 — Unified dated-slot model (chosen):**
    - Pros: one claim core, one availability/caching model (ADR-004), passes and lodging compose against the same primitive; festivals and parks stop being special cases.
    - Cons: "slot" must be generic enough to carry performance-like and calendar-like semantics without becoming mush; park-specific concepts (operating hours, weather closure) live as slot attributes.
- **Option 2 — Separate visit-calendar domain for parks:**
    - Pros: each domain modeled in its own natural language.
    - Cons: two claim paths through the inventory core, double the contention/caching/traceability machinery, passes need to span both.
- **Option 3 — Spike/defer to epic planning:**
    - Pros: decide with code in hand.
    - Cons: US-002/US-003 already bake catalog and claim shapes; deferring means either blocking M1 or accepting rework.

## Decision

**Every admission-granting offer is a dated slot in one inventory machinery.** A show's performance is a slot; a festival day is a slot (drawing on shared festival capacity); a park operating day is an admission slot with capacity. Season tickets and park passes are **entitlements to claim slot entries** (a venue season ticket claims its seat across a season's slots; a park pass grants unlimited entry claims on operating-day slots, named-holder-validated). Day-slot resources (cabanas, parking) and nightly lodging attach to slots/date ranges but reserve through the same contention-safe core (ADR-002). Slot attributes carry domain specifics (seated vs GA vs zoned, operating hours, closures) — the claim primitive does not fork.

## Consequences

- **Positive:**
    - One contention core, one no-oversell proof, one availability caching tier, one lifecycle-trace shape (ADR-003) covers concerts, festivals, parks, and everything later.
    - Multi-park passes are just entitlements scoped to multiple venues' slots — no new machinery.
- **Negative:**
    - The slot abstraction is load-bearing: a bad generic design hurts every vertical; the TKT-2/TKT-12 plans must pressure-test it against the weirdest cases (re-entry, mid-day capacity changes, weather closures) before it hardens.
    - Park semantics expressed as attributes may read less naturally in code than a dedicated model.

## References

- [PRD](../product/prd-v1.md) (TKT-2, TKT-12, TKT-13, TKT-14) · [ADR-002](./ADR-002-services-from-day-one.md) · [ADR-004](./ADR-004-cache-first-read-path.md)
