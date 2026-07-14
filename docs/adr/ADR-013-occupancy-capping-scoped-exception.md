# ADR-013: Concurrent-occupancy capping is out of the reservation claim core

Date: 2026-07-14

## Status

Accepted (recorded by the TKT-50 / US-008 spike; the scoped exception to [ADR-005](./ADR-005-unified-dated-slot-admission.md))

## Context

ADR-005 unified every admission-granting offer into one dated slot served by one inventory claim
core, and bet that the claim primitive never forks. The TKT-50 spike pressure-tested that bet and
confirmed it **for the reservation path** (see [`docs/spikes/TKT-50-dated-slot-pressure-test.md`](../spikes/TKT-50-dated-slot-pressure-test.md)).

One case does not fit. A park (or any venue with a fire-code / safety limit) may need to enforce a
**live concurrent-occupancy cap** — "no more than N people physically inside right now" — which is a
different quantity than "N admissions sold":

- **Reservation (the claim core, ADR-010):** counts *admissions sold*. Monotonic within a slot's life
  — `confirmed_quantity` only rises until a claim is released or expires; the invariant is
  `confirmed + held ≤ capacity`. A confirmed claim holds its unit for the slot's duration.
- **Occupancy:** counts *bodies currently present*. Rises on `entry` and **falls on `exit`** — a
  non-monotonic gauge with no fixed relationship to admissions sold (a re-entry pass holder can be
  counted in, out, and in again on one entitlement; occupancy at any instant is far below total
  admissions sold).

Forcing an increment-and-decrement occupancy gauge into a claim core whose correctness proof depends
on monotonic accumulation would fork the primitive — exactly what ADR-005 set out to avoid — and
would put a live, high-churn counter on the hottest serialized row (ADR-010's per-pool write lock).

## Decision

**Live concurrent-occupancy capping is out of scope for the reservation claim core and for US-009.**

- The inventory claim core keeps counting only *admissions sold* (ADR-010, unchanged). It never gains
  an exit-decremented occupancy gauge.
- US-009 (TKT-51) stores only slot **identity / kind / operating hours / re-entry policy / closure**.
  It carries **no** live occupancy count — not in Catalog, not in the Inventory pool, not in a claim.
- If live occupancy capping is ever built, it attaches by `slot_id` as a **separate** concern —
  naturally in the Access service (which already owns the `entry`/`exit` event stream and is the
  source of truth for who is physically inside), or a future dedicated capacity-control component. It
  is derived from the append-only entry/exit trail, not from reservations.

This is the single scoped exception to ADR-005; the unified model otherwise holds.

## Consequences

- **Positive:** the claim core's monotonic no-oversell proof stays intact; US-009 ships a slot schema
  that is not dead-ended and carries no premature occupancy machinery; the two distinct numbers
  (admissions sold vs bodies present) stay in the two services that already own them.
- **Negative:** a real occupancy-cap feature later needs its own design (a live gauge over the Access
  entry/exit stream, with its own contention story) — it does not come "for free" from the claim core.
- **Neutral:** re-entry itself (Case 1 of the spike) is **not** blocked by this exception — multi-entry
  is handled by `re_entry_policy` + the Access entry/exit stream under US-009. Only the *capping* of
  concurrent presence is deferred.

## References

- [ADR-005 — Unified dated-slot admission](./ADR-005-unified-dated-slot-admission.md) (this is its scoped exception) · [ADR-010 — PostgreSQL claim transaction](./ADR-010-postgres-claim-transaction.md)
- [TKT-50 spike report — §Verdict](../spikes/TKT-50-dated-slot-pressure-test.md)
