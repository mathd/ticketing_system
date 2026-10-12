# ADR-082: A coded refusal for a GA sellout

Date: 2026-10-12

## Status

Accepted (TKT-341, decision D1).

## Context

A GA quantity hold that inventory refuses for capacity answered one code-less 409
(`ErrUnavailable`, "insufficient capacity"). That one answer had five causes on the GA claim
path in `services/inventory/internal/store/store.go`:

- a seller mismatch on a bound channel allocation (TKT-246);
- a pool with too little headroom;
- a channel request with no active allocation;
- a channel cap with too little headroom;
- a public request that would use capacity reserved for active channel allocations.

Commerce flattened every inventory refusal to `409 {"error":"inventory unavailable"}`, and the
storefront showed "Quantity unavailable" for all of them. A buyer could not tell a sold-out pool
from a closed slot, a closed channel window or a partial shortage.

The waitlist epic (TKT-38) needs a refusal that means "sold out". A later story uses it as the
condition to offer a waitlist. A waitlist must not be offered for a closed slot or for a failed
request.

Constraints:

- Inventory decides capacity under the pool row lock (ADR-010). A cached availability read
  cannot identify a sellout with authority.
- A closed sales window has its own code and must not look sold out (ADR-054).
- Channel refusals are uniform on purpose. A distinct code for one channel cause tells a prober
  which channels exist and who sells them (TKT-246). `presale_code_invalid` is uniform for the
  same reason (ADR-064).
- Both services validate their responses against a closed `Error.code` enum and fail closed
  (ADR-028). An undeclared code becomes a 500.
- Capacity is per slot (pool), not per ticket type.

## Possible Solutions

- **Option 1: Do nothing.**
    - Pros:
        - No change.
    - Cons:
        - The waitlist story has no entry condition.
- **Option 2: Label every code-less capacity refusal as sold out.**
    - Pros:
        - The smallest change.
    - Cons:
        - A partial shortage reads as a sellout, but the buyer can buy fewer now.
        - The channel causes become distinguishable from each other, which opens the TKT-246
          oracle.
- **Option 3: Distinguish "held out" (zero because of live holds) from "sold out".**
    - Pros:
        - The copy can say that tickets can come back.
    - Cons:
        - It gives the waitlist two entry conditions. The proposed capture formula (ADR-075) does
          not make that split: buyer hold expiry is a capture source.
- **Option 4: A `sold_out` code for zero public headroom on an unchannelled GA hold.**
    - Pros:
        - It is the admission expression, so it means the same thing as the capture headroom
          that ADR-075 proposes.
        - Channel uniformity and partial shortages do not change.
    - Cons:
        - A pool that is held out but not sold shows "Sold out" until its holds expire.

## Decision

We adopt Option 4.

**Definition.** Inventory returns `ErrSoldOut` only for an unchannelled GA hold where:

```text
limit            = target_capacity while a cut drains, otherwise capacity
public_remaining = limit - confirmed - live_held - reserved_for_active_channels
sold out         = public_remaining <= 0
```

`live_held` is `liveClaims`: buyer holds that have not expired, finalizing claims and
operational holds. `reserved_for_active_channels` is `reservedForChannelsSQL`: unused capacity of
every active allocation, also when its sales window is closed (ADR-054 §Decision B). The
classification uses the same expression as admission, in the same transaction, under the pool row
lock. A demand term that a later change adds to admission (for example the pending captures and
offers of ADR-075) must go into that expression, and then it moves both.

**What stays code-less.** A partial shortage (`0 < public_remaining < quantity`) stays
`ErrUnavailable`: the buyer can buy fewer now, so it is not a waitlist condition. Every refusal of
a request that names a channel stays `ErrUnavailable`: pool, cap, no allocation and seller
mismatch, whatever the pool looks like. Group, operational and seated refusals stay
`ErrUnavailable`.

**Precedence.** Replay, offering state, channel window, seller and presale code are judged before
capacity, as before. So a closed or archived slot answers its own code, also on a full pool. The
global bound (`confirmed + live_held + quantity > limit`) is kept: the public path classifies
first and then refuses with it.

**Point-in-time.** `sold_out` is the state at the locked decision. It is not permanent. Holds
expire and channel reservations release, so the same request can succeed later. The contract and
the storefront copy do not say "permanently".

**Wire contract.**

- `ErrSoldOut` wraps `ErrUnavailable`. It has no `Channel()` method. Inventory's mapper matches it
  above the structural `belowConsumption` match and above the generic branch (TKT-307), and
  answers `409 {"error": "<message>", "code": "sold_out"}`.
- Commerce forwards it only for a public GA reserve: no partner scope, no call error, status 409,
  a body that decodes with a string `error` and exactly `code: "sold_out"`. It rebuilds a static
  body, `409 {"error":"sold out","code":"sold_out"}`. It applies the same check on the first
  reserve and on the persisted replay. Partner routes never send it, and the exchange target
  refusal does not carry it.
- `sold_out` is in both `Error.code` enums.
- The storefront shows "Sold out" / "Complet" only for that exact GA answer. It shows no waitlist
  action.
- The load harness counts a `sold_out` 409 as a capacity rejection. Every other code still fails
  closed.

The later waitlist join story can use `sold_out` as its entry condition. It must still check
account eligibility on the server.

## Consequences

- **Positive:**
    - A buyer can tell a sellout from a closed slot, a closed window and a partial shortage.
    - The waitlist has an entry condition that matches the proposed capture headroom.
    - The channel refusals stay uniform, so the TKT-246 oracle stays closed.
- **Negative:**
    - A pool that is held out but not sold shows "Sold out" until holds expire. The owner can
      decide later whether the copy adds "tickets may become available".
    - `sold_out` is per slot (pool). A slot with more than one GA ticket type sells out as one.
    - Name the adversary (ADR-021): the code adds no information that the public availability
      read does not already give (`available`, `held`, `offering_status`). It gives a fresher
      answer, because inventory takes it under the lock.

## References *(optional)*

- TKT-341, TKT-38
- [ADR-010: PostgreSQL claim transaction](ADR-010-postgres-claim-transaction.md)
- [ADR-021: Ticket lifecycle trail integrity](ADR-021-ticket-lifecycle-trail-integrity.md)
- [ADR-024: Channel allocations](ADR-024-channel-allocations.md)
- [ADR-028: Response drift fails closed](ADR-028-response-drift-fail-closed.md)
- [ADR-054: Per-channel sales windows](ADR-054-per-channel-sales-windows.md)
- [ADR-064: Presale unlock codes](ADR-064-presale-unlock-codes.md)
- [ADR-075: Proposed waitlist capacity capture](ADR-075-proposed-waitlist-capacity-capture.md)
