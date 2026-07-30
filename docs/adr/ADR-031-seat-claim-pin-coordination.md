# ADR-031: Seat-claim ↔ catalog pin coordination (hold-then-pin)

Date: 2026-07-21

## Status

Accepted (TKT-80 / US-017)

## Context

Reserved seating enters the claim core (TKT-80): a buyer holds a specific set of seats on
a seated slot, with per-seat oversell impossible. Two facts constrain how this is built:

- **Inventory is the sole write-side correctness boundary and never reads or writes the
  catalog database** (ADR-010, ADR-002). Seat identities and the pin table
  (`seat_map_pins`) live in **catalog**; inventory owns the claim.
- **ADR-029** already defined the pin contract catalog exposes — `PinSeat`/`UnpinSeat`
  under a family advisory lock, keyed on `(map_family_id, seat_identity, pinned_by)`,
  `pinned_by` free-form so a hold writes `"hold:<claim_id>"`. It left *how* an inventory
  hold reaches that contract to this ticket.

Per-seat "at most one live claim" (AC1) cannot be catalog's job: `seat_map_pins` UNIQUE
includes `pinned_by`, so two different holds pin the same seat without conflict. And the
map-edit safety (AC3) *is* catalog's job: `EditSeatMap` hard-rejects an edit that would
orphan a pinned identity, so a held seat must have a pin. So the design must split the two
guarantees across the two services and coordinate them across two databases with no
distributed transaction.

## Possible Solutions

- **Option 1 — hold-then-pin (chosen).** Inventory commits the hold first (per-seat
  uniqueness enforced by an inventory-side partial unique index), then the API-layer
  caller pins the seats in catalog over an internal HTTP batch endpoint. Only the winning
  claimant reaches catalog.
- **Option 2 — pin-first with a durable intent/outbox and a recovery worker.** Pin in
  catalog before committing the hold; a durable `seat_hold_intents` + `seat_unpin_outbox`
  and a background worker reconcile crashes.
- **Option 3 — event-driven pinning.** Inventory emits a seat-held event; catalog consumes
  it and pins. Requires inventory's first event producer and an outbox.

## Decision

**Option 1, hold-then-pin**, with these rules:

1. **Inventory owns per-seat uniqueness.** A `claim_seats` child table (one row per held
   seat under one buyer claim, `quantity = len(seats)`) with a partial unique index
   `(pool_id, seat_identity) WHERE released_at IS NULL`. `released_at IS NULL` covers
   held, finalizing **and** confirmed — the finalizing window is exactly where a
   status-based predicate leaks a double-hold. `released_at` is set **in the same
   transaction** as the claim's terminal (expired/released) flip: an already-expired claim
   is never revisited, so a decoupled update would block the seat forever.

2. **Seated pools are a distinct inventory kind.** `inventory_pools.inventory_kind ∈
   {ga, seated}` + a `seat_map_id`; the GA quantity path and the four staff claim-insert
   entry points reject a seated pool (else fungible tickets sell over reserved seats). The
   pool is provisioned from the **existing catalog schema-4 seated publication** — no event
   schema bump, no access-service change. Its `capacity` is the venue GA snapshot used as a
   **coarse ceiling**; the tight per-seat oversell boundary is the unique index plus
   `PinSeat` existence-validation.

3. **Pin coordination = hold-then-pin + a replay-re-pin invariant.** After the hold
   commits, the caller batch-pins the seats. **A success response is returned only after
   the pins exist, and an idempotent replay re-asserts the batch pin before returning the
   claim** — so commerce can never hold a claim id whose seats were not pinned. A
   *deterministic* pin rejection (a seat absent from the current published map) releases
   the hold and returns 409; a *transient* failure releases the hold and returns 503, and a
   same-key retry re-pins. Confirm/finalize keep the pins (the seat is still held/sold);
   release/expiry unpin.

4. **No worktree/outbox/worker.** Catalog unpins for released/expired holds are fired
   best-effort by the post-commit caller from the swept set the store returns. This fits
   ADR-010's sweeper-free grain (expired capacity is reclaimed on the next mutation).

## Consequences

- **Positive:**
    - Under a high-contention on-sale (a first-class concern), only *winning* claimants
      touch catalog's per-family advisory lock; losers are rejected by the inventory index
      with no catalog round-trip. Pin-first would serialize *every* claimant through catalog
      (pins are per-`pinned_by`, so it cannot even reject losers) — a compensation storm at
      peak load, which is why it was rejected.
    - No new event schema, no access-service coupling, no durable coordination tables, no
      background worker — the smallest surface that satisfies all four ACs.
    - The catalog pin endpoint is a **hand-mounted internal route** (like the existing
      `/internal/*` reads), not part of the public OpenAPI contract — matching convention;
      the response validator skips undeclared internal paths.

- **Negative / limits (name the adversary — ADR-021):**
    - **This is honest-writer consistency, not tamper-evidence.** A writer with catalog DB
      access can insert/delete `seat_map_pins` at will; a writer with inventory DB access can
      forge `claim_seats`. The coordination guards our own bugs and concurrent honest
      writers, not an attacker.
    - **Crash window (bounded, self-healing).** A crash between hold-commit and pin leaves a
      *zombie held* claim with no pin for ≤ `HOLD_TTL` (default 10m); it expires unconfirmed,
      and a later re-hold of that seat fails closed at `PinSeat`. The replay-re-pin invariant
      is what keeps this from ever becoming a *confirmed* orphaned seat.
    - **Leaked pins fail safe.** A pin whose hold expired on a pool that is never touched
      again lingers; it *blocks a now-safe map edit* (409), it never orphans or oversells.
      Operator remedy: the internal batch-unpin endpoint is manually callable; a one-shot
      `reconcile-pins` subcommand is deferred to a follow-up ticket. Losing the *ability* to
      clean up would not be acceptable; deferring the automation is.
      **Closed by TKT-112** — see §Amendment below. §4's refusal of a worker still stands: the
      remedy is a command an operator runs, not a process that runs itself.
    - **Undersell if seat count > GA snapshot.** The coarse ceiling assumes a seated seat
      count ≤ the venue standing capacity (physically true, not schema-enforced). A pathological
      config would cap seated sales early — fail-closed (`ErrUnavailable`), never oversell.
      Escape hatch: an exact seat count can later ride the existing `CatalogResolver` HTTP
      contract (the schema-1 provisioning precedent) with no event bump.
    - **Open question — performance-to-map-version binding.** The pool binds to its
      publish-time `seat_map_id`, but `PinSeat` validates against the *current* published
      version (ADR-029's proven contract). Whether a performance should follow the current
      version or stay pinned to its publish version is undecided; TKT-80 keeps the
      current-version-only check (the shipped, most reversible behavior). If a v2-added seat
      must not be sellable against a v1 performance, add an exact-version membership check to
      `PinSeats` under the same lock — one guarded SELECT.

## Amendment — leaked-pin reconciliation (TKT-112, 2026-07-30)

The deferred remedy shipped as `inventory reconcile-pins`, a one-shot operator subcommand
(ADR-022 placement, alongside `migrate` and `reprocess-quarantine`). §4 is unchanged: still no
worker, outbox or scheduler. Four decisions are worth recording because each rules out an
implementation that looks obviously right.

1. **Inventory hosts it, not catalog.** A liveness verdict cannot be a pure read. Deciding that
   an expired hold is dead requires *making* it dead — taking the pool lock and running the
   ordinary `sweepExpired` — because `now()` is frozen at transaction start
   (`docs/learnings/2026-07-16-lock-queue-time-cutoffs.md`), so a finalize that BEGAN before the
   TTL elapsed and is queued behind the reconciler would otherwise still succeed after the pin
   was removed. Flipping the status makes the verdict a fact the waiter re-reads and refuses.
   Only inventory can do that, so only inventory can own the verdict.

2. **Neither service reads the other's database** (ADR-010, ADR-002), so catalog exposes the
   *read* side of the pin contract — `GET /internal/seat-map-pins`, keyset-paged over the
   primary key, hand-mounted and outside the public contract like its pin/unpin siblings
   (ADR-009). It returns every `pinned_by` namespace; the classification belongs to the caller,
   because a catalog-side filter would define which pins are reclaimable in the service that
   has no way to know.

3. **A pin naming an unknown claim is reported, never reclaimed.** Tempting to unpin — claims
   are never deleted, and hold-then-pin commits the claim first, so "unknown" should be
   impossible. But `hold:` pins cover **confirmed** claims too (§3: confirm/finalize keep the
   pin), so an unknown reference is exactly what an inventory database restored *behind* catalog
   looks like, and unpinning there strips the protection from a sold seat — the one outcome
   ADR-029 exists to prevent. Unpinning is also irreversible (catalog keeps no pin history and
   inventory cannot re-pin what it does not know), while leaving the pin costs one blocked edit
   and stays fixable by hand. Same for a malformed `hold:<not-a-uuid>`; `pinned_by` is free-form
   text, so that is reachable.

4. **Fail-safe residue exits zero.** `unknown` and `malformed` counts are the expected state, not
   a failure; a non-zero exit is reserved for a store, transport or unpin failure, so an operator
   can distinguish "nothing more to do" from "it broke". Reclaims are keyed to a specific dead
   `pinned_by`, and a claim id is unique to its hold — so a reclaim can never delete the pin a
   *newer* hold wrote for the same seat identity, which is also why paging without a snapshot is
   safe (a pin inserted behind the cursor is picked up by a later run, never wrongly deleted).

Scope is unchanged from §Negative: this is **honest-writer consistency, not tamper-evidence**
(ADR-021). The reconciler derives its verdict from inventory's own tables and acts on catalog's;
a writer with access to either can defeat it, and it is not built to notice.

## References

- [ADR-029](ADR-029-seat-identity-pinning-contract.md) — the pin contract this consumes.
- [ADR-010](ADR-010-postgres-claim-transaction.md) — claim core; inventory never reads catalog DB.
- [ADR-005](ADR-005-unified-dated-slot-admission.md) — seated is an adapter, not a fork.
- [ADR-026](ADR-026-inventory-capacity-adjustment-clamp.md) — never-strand-a-confirmed-claim spirit.
- [ADR-021](ADR-021-ticket-lifecycle-trail-integrity.md) — name-the-adversary discipline.
- [ADR-009](ADR-009-contract-first-apis.md) — internal service-to-service routes follow the
  hand-mounted convention, outside the public contract.
- TKT-80 (US-017: seat-level claims).
