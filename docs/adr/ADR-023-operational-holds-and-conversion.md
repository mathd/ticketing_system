# ADR-023: Operational holds are claims; conversion swaps under the pool lock

Date: 2026-07-16

## Status

Accepted (approved at the TKT-77 plan gate, 2026-07-16)

## Context

Operators need house seats, artist allotments and kills held out of public sale, releasable
and sellable later, without ever racing the public (TKT-77 / US-014). ADR-010 defines one
claim lifecycle with a per-pool lock; ADR-005 forbids forking the claim primitive per
vertical. The open choices were representation (parallel table vs the claims table),
partial-operation mechanics, what a conversion produces, and where the audit trail lives.

## Possible Solutions

- **Extend `claims` with a kind (chosen)** — one capacity predicate, one lock order; the
  invariant `confirmed + live-held ≤ capacity` covers operational quantity by construction.
- **A parallel operational-inventory table** — two accounting paths in every capacity
  check; exactly the fork ADR-005 rules out.
- **Convert by direct-confirm** — would mint confirmed admissions with no order, no journal
  entry (ADR-003/ADR-011 violation).

## Decision

- **Representation.** `claims.claim_kind ∈ buyer|operational`; operational claims carry
  `operational_purpose ∈ house|artist|kill|other` + a label and have `expires_at IS NULL`.
  A DB check (`claims_kind_shape`) makes NULL expiry impossible on buyer claims, so the
  single live-claim predicate (`store.go` `liveClaims`) keys on expiry alone: operational
  holds never expire, always count.
- **Partial operations decrement the source**; a whole release/convert turns the row
  terminal (`released`) keeping its original quantity for the record. The staff-facing hold
  ID survives partial operations.
- **Conversion produces a normal buyer hold with TTL** — never a confirmation; payment has
  not happened. Inventory swaps quantity (insert child + decrement source) in one
  transaction under the pool lock, so converted capacity is quantity-neutral and never
  publicly claimable in between. **An expired converted child returns its capacity to the
  public pool, not to the source hold** — re-carving is a new staff operation, by design.
- **Commerce orchestrates the staff sale** (`POST /internal/operational-holds/{id}/convert`):
  it resolves the offer through the existing catalog seam (no price override), rejects a
  ticket type whose performance is not the hold's slot, calls inventory's conversion, and
  persists the standard reservation row with a deterministic, namespaced identity
  (`reservation:op-convert:<org>:<key>`) so a crash between the two writes is repaired by
  replaying the same request. The existing public checkout completes the sale.
- **Idempotency.** Staff mutations register their organizer-scoped key + fingerprint in
  `claim_history` (unique partial index), never in the claims-table key namespace; the
  converted child's claims-row key is namespaced with the source id. Replay returns the
  history row's immutable outcome, not the source's current state.
- **Audit.** Append-only `claim_history` records action/actor/reason/quantities for every
  claim mutation, buyer paths included (`create`, `finalize`, `confirm`, `release`,
  `expire` by `system/ttl_elapsed`). History starts at migration 0003 — no invented
  baseline rows. An UPDATE/DELETE-rejecting trigger is an application-level guard only:
  **this is not tamper evidence against a database writer** (ADR-021), and `actor` is an
  assertion by an authenticated internal caller, not verified staff identity (staff RBAC
  is TKT-22). Inventory still emits no domain events.
- **Exposure.** All staff endpoints live under `/internal/` (gateway 404s them), require
  `X-Internal-Token`, and fail closed on an empty configured token. Staff availability
  (`buyer_held`/`operational_held` breakdown) and history are `no-store`; the public
  availability shape and its 5s tier are unchanged — `held` stays the aggregate.

## Consequences

- **Positive:** one no-oversell proof covers operational inventory; the gapless-convert
  property is enforced by construction and pinned by a race test in the smoke gate; staff
  keys can never collide with buyer keys.
- **Negative:** history INSERTs ride every claim mutation, adding a row write inside the
  hottest transaction (bulk-batched for sweeps); the per-pool serialization cost (ADR-010)
  now also covers staff operations. TKT-82 measures the ceiling.
- **Neutral:** pricing of converted seats follows the ticket type's offer; overrides and
  box-office pricing policy stay in TKT-5/TKT-16.

## References

- [ADR-005](./ADR-005-unified-dated-slot-admission.md) · [ADR-010](./ADR-010-postgres-claim-transaction.md) · [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) (why no tamper-evidence claim)
- PRD §TKT-4 US-014 (`docs/product/prd-v1.md`) · Board TKT-77
