# ADR-026: Inventory Capacity Adjustment — Derived Clamp with a Pending Target

Date: 2026-07-16

## Status

Accepted

## Context

The ADR-005 amendment decided the *behaviour* of post-publication capacity adjustment verbatim —
raises apply freely; a cut below live demand clamps to the invariant floor
`max(new, confirmed + held)` and blocks new claims until demand drains to the target, never
force-releasing a confirmed admission (forward-only) — and explicitly left the *storage
representation* to the inventory-side ticket (TKT-76). The open question: does the "blocked while
demand exceeds the target" state need its own column/flag, and does capacity itself need to
change shape?

Constraints: ADR-010's pool-row lock order and `confirmed + unexpired-held ≤ capacity` invariant;
expiry is lazy (no sweeper may be load-bearing); the `capacity > 0` CHECK; staff-op idempotency
lives in the `claim_history` registry, whose `claim_id` was `NOT NULL`.

## Possible Solutions

- **Option 1 — nullable `target_capacity`, blocking derived:** `capacity` stays the applied
  ceiling (never below live demand); `target_capacity` is non-null only while a cut drains.
  Admission checks compare demand against `COALESCE(target_capacity, capacity)`; reads derive
  effective capacity as `max(COALESCE(target_capacity, capacity), confirmed + live held)`.
    - Pros:
        - No flag to keep in sync; "blocked" cannot contradict the counters it derives from.
        - `capacity` keeps its ADR-010 meaning; the DB CHECK (`target < capacity` while pending)
          makes an inverted state unrepresentable.
        - Correctness never depends on reconciliation running — reads re-derive from `liveClaims`.
    - Cons:
        - Two numbers describe one intent while a cut drains; staff surfaces must show both.
        - A `reconcileCapacity` bookkeeping step (at demand-decreasing seams) exists purely to
          settle the materialized row.
- **Option 2 — store requested capacity directly in `capacity` + a `blocked` flag:**
    - Pros: one number.
    - Cons: breaks `confirmed + held ≤ capacity` (the ADR-010 invariant every admission check
      leans on); the flag is derivable state that can go stale — the class of bug ADR-021 warns
      about, in the money path.
- **Option 3 — separate `capacity_adjustments` table, capacity fully derived:**
    - Pros: full event-sourced history.
    - Cons: every admission check becomes a join/fold; the hot path (ADR-010 lock section) pays
      for an audit concern `claim_history` already covers.

## Decision

We adopt Option 1. Pool-level adjustment records join the existing `claim_history`
audit/idempotency registry (`claim_id` made nullable, `pool_id` + `target_capacity` added, a
shape CHECK keeping every row exactly claim-shaped or pool-capacity-shaped, action
`adjust_capacity`). The adjustment operation is a staff op in the ADR-023 mould: pool `FOR
UPDATE`, registry-replay-before-guard, `sweepExpired` before accounting, archived pools reject
(terminal) while closed pools stay adjustable (closure is reversible). Channel allocations are
not resized by a cut — caps are upper bounds — but replacement sets validate against
`COALESCE(target_capacity, capacity)`.

## Consequences

- **Positive:**
    - Blocking, effective capacity, and the drain-to-target trajectory are all derived from live
      claims — no sweeper, no flag, no second source of truth on the admission path.
    - One shared idempotency/audit registry for every staff mutation, claim-level or pool-level.
    - The adversarial adjust-during-holds interleaving is deterministic under the ADR-010 lock
      queue and pinned by a smoke test.
- **Negative:**
    - `capacity` alone no longer tells the whole story while a cut drains; staff reads carry
      `target_capacity` and every operator surface must render it.
    - `reconcileCapacity` runs (cheaply, guarded by `target_capacity IS NOT NULL`) inside
      release/expiry transactions — a small hot-path cost for a settled materialized row.
    - `capacity = 0` remains unrepresentable by decision: stopping all sales is closure's job
      (TKT-75), not a capacity of zero.

## References

- TKT-76 (board), TKT-50 spike
- [ADR-005 amendment](./ADR-005-unified-dated-slot-admission.md) — the decided behaviour
- [ADR-010](./ADR-010-postgres-claim-transaction.md) · [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) · [ADR-023](./ADR-023-operational-holds-and-conversion.md)
