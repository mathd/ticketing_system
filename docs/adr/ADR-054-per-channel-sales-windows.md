# ADR-054: Per-channel sales windows as DB-time predicates on the allocation row

Date: 2026-08-10

## Status

Accepted (TKT-238; decision taken under the owner-waived gates of that run, recorded on the ticket).
Fourth deliverable of the TKT-17 epic. **Redeems ADR-024's deferral**: that ADR scoped sales windows
out to TKT-17 explicitly, and this is that follow-up. It amends nothing — ADR-024's accounting rules
are extended, not changed, and ADR-027's draw-down rule is *cited* rather than amended (§4).

## Context

Nothing channel-scoped decided *when* a channel may sell. The only windows were rule-level
`effective_from`/`effective_until` on price/fee/split rules; the nearest on-sale concept was the
pool's binary `closure_status`. **A presale opening Tuesday and a public on-sale opening Friday were
inexpressible.**

## Decision

### 1. Two nullable columns on `channel_allocations`, not a sibling table

The identity is already exactly `(pool_id, channel_code)`, and PUT replacement, the pool lock,
derived consumption and cache invalidation all operate on that row. A sibling table would add a join
and a **second source of truth to a decision taken under the pool lock** — the one place in this
schema that cannot afford one. Half-open `[opens_at, closes_at)`, NULL unbounded on either side, and
a CHECK makes a reversed window unrepresentable.

### 2. One predicate, on `clock_timestamp()`, never `now()`

    (opens_at IS NULL OR opens_at <= clock_timestamp())
    AND (closes_at IS NULL OR closes_at > clock_timestamp())

`now()` freezes at transaction start, so a hold queued on the pool lock across the cutoff would
decide with stale time and sell a closed channel. ADR-024 wrote this reasoning down for `release_at`;
a window is the same shape.

**One const, reused in all three paths.** `consumedQuantity`'s comment records why: six call sites
summed a claim's consumption and one silently used a different expression. A window predicate copied
by hand into three claim paths would fork the same way, and the fork would be invisible until a hold
succeeded on a closed channel.

### 3. A closed window does NOT release capacity

`release_at` releases: past it, an allocation stops being active and its unsold remainder becomes
publicly claimable. **A window does not.** A presale's cap is a promise about capacity, and a promise
that evaporates until its window opens is not one — the public on-sale would eat the presale's
allocation overnight.

So `windowOpen` appears in the **claim** paths and never in `reservedForChannelsSQL`. Pinned by
`TestClosedWindowAllocationRemainsReservedFromPublic`: capacity 10, closed presale cap 6 → public
availability **4**, not 10.

The asymmetry is deliberate and the two answer different questions: *release_at* says the allocation
is over; *a window* says it is not its turn yet.

### 4. The window gates NEW consumption only

Gated: `CreateHold`, `PlaceGroupReservation`. **Not gated: `DrawDownGroupReservation`.**

ADR-027 already settled the analogous case for `release_at`, on a clause that transfers exactly:

> Draw-down does not re-check allocation activity: **the source already consumed it**, and ADR-024
> lets existing claims finish their lifecycle after `release_at`.

A draw-down is quantity-neutral — it inserts a child and decrements the source in one pool-locked
transaction — so it consumes nothing new. Gating it would strand capacity an agency was granted
*inside* the window, and nobody would report that as a window bug. The same reasoning is why an
existing hold completes after its window closes (the ticket's COS-4).

*(The plan draft proposed gating draw-down; plan-review rejected it on this ADR-027 clause.)*

### 5. A distinct refusal: `channel_window_closed`

A closed window returns `409 {"code":"channel_window_closed","error":"channel sales window closed"}`.

`slot_closed` is **not** reused: that mirrors catalog's offering closure on the whole *slot*, while
this is one allocation row temporally shut. Collapsing them would make two facts with opposite
remedies indistinguishable — wait for the window, versus stop offering the slot.

**Precedence: the window is judged BEFORE any capacity arithmetic.** A window is a property of the
requested *channel*; capacity is a property of the *pool*, and the second running out does not make
the first stop being true. Checking capacity first — which is the natural ordering, and what the
first implementation did — made the identical request answer `channel_window_closed` while the pool
had room and the code-less "insufficient capacity" once it did not. So a closed presale read as a
sellout **exactly when the on-sale was busiest**, telling a caller to join a waitlist when it should
have waited ninety seconds. It also defeated §5's fail-closed policy in the wrong direction: a
code-less 409 is counted as *expected capacity evidence*, so a load run against a closed channel
would have been silently accepted rather than failing loudly. Found at ai-review; pinned by
`TestClosedWindowRefusesAsAWindowEvenOnAnExhaustedPool`.

An **absent** active allocation stays the code-less capacity refusal: there is no channel there to be
closed. That distinction is what selecting the predicate — rather than filtering on it — buys.

The refusal is **not** `ErrUnavailable`, and that took deliberate work: `ErrUnavailable` is the
natural return and yields the code-less sellout shape, which is exactly what the ticket forbade.
Pinned by an exact-body assertion in `server_test.go`.

**The on-sale load harness is deliberately NOT taught to accept it.** `ClassifyHold409` counts any
coded 409 as `KindServerError`, and a window refusal *is* a server error from a load run's
perspective: a run that hit a closed window measured nothing about contention. Counting it as a
capacity rejection would silently bless it as evidence. Pinned by name in `loadtest_test.go`.

### 6. Read staleness is accepted, and it is ADR-024's trade

The availability cache keys on `(organizer, slot, channel)` with a 5-second tier and nothing
invalidates on a time boundary, so **a window opening mid-tier serves a stale "closed" for up to five
seconds**.

This is the identical trade ADR-024 already accepted for `release_at`: *read* visibility may lag DB
time; *write* correctness switches exactly at PostgreSQL time. A buyer may be told "closed" for five
seconds after opening and succeeds on retry. **Nobody can buy outside the window**, because the claim
path decides under the pool lock on `clock_timestamp()`.

### 7. The window is staff-visible, not public

`ChannelAvailability` (staff) carries `opens_at`, `closes_at` and `window_open`. The public read
reports `available: 0` for a closed channel and says nothing about why.

An operator needs to tell "not open yet" from "sold out" — different problems, different remedies.
A buyer does not, and the public read's shape is pinned at exactly two statements by
`smoke/onsale_read_proof_test.go`. Adding a public field later is additive; retracting one is a
contract break.

## Consequences

- **Positive:** presales and staged on-sales are expressible; the refusal is machine-distinguishable;
  no sweeper, no counters, no new lock order. Existing allocations have NULL bounds and behave
  exactly as before.
- **Negative:** a closed window's capacity is unsellable by anyone until it opens — oversell-free
  undersell, and the accepted cost of §3. Read staleness per §6. The load harness fails loudly on a
  window refusal by design (§5), which is correct but will surprise someone once.
- **Revisit when:** a window needs to vary by anything other than (channel, slot) — a per-ticket-type
  window would not fit this row and would need the sibling table §1 rejected.
