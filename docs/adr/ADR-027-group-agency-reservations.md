# ADR-027: Group Reservations as Expiring Claims; Draw-Down Reuses the Conversion Carve

Date: 2026-07-16

## Status

Accepted (approved at the TKT-79 plan gate, 2026-07-16)

## Context

Organizers grant groups and agencies long-lived reservations drawn down over time
(TKT-79 / US-016): an explicit expiry date instead of the cart TTL, a named counterparty,
partial and repeated conversion to confirmed orders, and lazy DB-time give-back of the
unconverted remainder — without the remainder ever passing through a publicly claimable
state. ADR-010 fixes the lock order and lazy-expiry rules; ADR-023 already built typed
claims and the quantity-neutral conversion carve; ADR-024 built channel accounting.
Pay-later terms, invoicing and dunning are explicitly TKT-34.

## Possible Solutions

- **A third claim kind, `reservation` (chosen)** — non-NULL `expires_at`, a
  `reservation_counterparty`, optionally a `channel_code`; draw-down clones the ADR-023
  carve.
    - Pros: one capacity predicate, one lock order; expiry rides `liveClaims` (which keys
      on `expires_at` alone), so give-back is lazy DB-time for free.
- **Extend `operational` with an optional expiry** — contradicts its defining shape:
  operational claims never expire and stay unchanneled (`claims_kind_shape`, ADR-023/024);
  every expiry predicate would need kind-awareness.
- **A parallel reservations table** — two accounting paths in every capacity check; the
  fork ADR-005 rules out (same reasoning as ADR-023).

## Decision

- **Representation.** `claims.claim_kind ∈ buyer|operational|reservation` (migration
  0007). A reservation has non-NULL `expires_at`, a non-blank `reservation_counterparty`,
  no operational fields, and may carry a `channel_code` — enforced by the rebuilt
  `claims_kind_shape`. Because `liveClaims` keys on expiry alone, a live reservation
  counts against capacity like any claim and stops counting at expiry with no sweeper;
  `sweepExpired` settles it (action `expire`) on the next pool mutation.
- **Placement** (`POST /internal/group-reservations`, staff-only) follows the ADR-023
  staff-op shape: pool lock → `claim_history` registry replay → offering guard → sweep →
  capacity check (target-aware, ADR-026), plus the ADR-024 channel arm — a channeled
  reservation needs an active allocation with headroom; an unchanneled one may not eat
  capacity reserved for active allocations.
- **Draw-down** (`POST /internal/group-reservations/{id}/draw-down`) clones the ADR-023
  conversion carve: insert a normal buyer child (cart TTL) + decrement the source in one
  pool-locked transaction — quantity-neutral, so the remainder is never publicly claimable
  in between. The child **inherits the source's `channel_code`**; commerce cannot
  reattribute consumption across allocations. Draw-down does not re-check allocation
  activity: the source already consumed it, and ADR-024 lets existing claims finish their
  lifecycle after `release_at`. Commerce orchestrates through the same staff-sale seam as
  ADR-023 (catalog-priced, expected-slot precondition, deterministic
  `reservation:group-draw-down:<org>:<key>` identity, replays judged by the child's
  lifecycle state); the public checkout completes the sale. Child expiry returns capacity
  to the pool, not to the source — re-carving is a new staff operation.
- **Time bases.** The reservation's expiry is a cutoff decided *inside* lock-serialized
  transactions, so the two new decisions use **decision time** (`clock_timestamp()`):
  placement rejects an expiry not in the future, and draw-down rejects (and lazily
  settles) a source whose expiry passed while the transaction queued on the pool lock —
  the TKT-78 rule. **`liveClaims` and `sweepExpired` deliberately stay on `now()`**
  (ADR-024): the split is safe because the capacity check still counts an unswept expired
  source — the error is undersell-shaped (a rejected draw), never oversell. Re-make this
  independence argument before splitting time bases anywhere else.
- **Idempotency & audit.** Staff keys register in `claim_history` (actions `reserve`,
  `draw_down`), never the claims key namespace; the child's claims-row key is namespaced
  `grp-draw:<source>:<key>`. The counterparty stays inventory metadata — it never enters
  the payment journal.

## Consequences

- One new migration (0007), no commerce schema change; the checkout, recovery and journal
  paths are untouched.
- The per-draw quantity keeps the checkout seam's 1–50 bound; large blocks are drawn in
  repeated batches — the story's intended model.
- Early release of an unexpired reservation and TKT-34's pay-later flows are future staff
  operations; nothing here precludes them.
