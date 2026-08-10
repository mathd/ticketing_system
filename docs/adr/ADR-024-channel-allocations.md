# ADR-024: Channel Allocations as Derived-Usage Rows Inside the Pool Lock

Date: 2026-07-16

## Status

Accepted

**Sales windows, deferred here, are now decided by [ADR-054](./ADR-054-per-channel-sales-windows.md) (2026-08-10, TKT-238).** This ADR's accounting rules are extended rather than changed: a window gates claims and never releases capacity, so `reservedForChannelsSQL` is untouched, and the same clock_timestamp() discipline applies for the same reason.

## Context

US-015 (TKT-78) splits a slot's sellable capacity across sales channels — opaque codes with
per-channel caps and an optional scheduled give-back (`release_at`) — so presales and resellers
cannot starve the public on-sale. The constraints come from ADR-010: the inventory pool row is
the single serialization point, PostgreSQL time decides lazy transitions, and correctness must
not depend on a background job. Channel registry, per-channel pricing and sales windows are
explicitly out of scope (TKT-17/TKT-5); channel codes here are exact opaque strings — no
normalization, no case folding.

## Possible Solutions

- **Materialized sub-pools** — each channel a child pool with its own capacity row.
    - Pros: reuses the pool accounting shape.
    - Cons: every sale coordinates parent and child capacity, introducing a second lock order
      (against ADR-010); give-back at `release_at` becomes a capacity *transfer* needing a
      sweeper or a mutation, not a predicate.
- **Allocation rows with consumed counters** — cap plus a counter maintained on every hold.
    - Pros: cheap reads.
    - Cons: counter/claim drift is a standing risk; expiry (a lazy state, not an event) cannot
      decrement a counter without a sweeper — the exact failure ADR-010 bans.
- **Allocation rows with derived usage** (chosen) — `channel_allocations(pool_id, channel_code,
  cap, release_at)`; claims carry a nullable `channel_code`; consumption is always a sum over
  claims under the pool lock.
    - Pros: single lock order preserved; zero drift by construction; release is a pure DB-time
      predicate (`release_at IS NULL OR release_at > clock_timestamp()`), symmetric with hold
      expiry. `clock_timestamp()`, not `now()`: `now()` freezes at transaction start, so a hold
      transaction queued on the pool lock across the cutoff would decide with stale time and
      sell a released channel. The global capacity check never depends on this predicate, so
      the two time bases cannot combine into an oversell.
    - Cons: adds indexed claim aggregation to the serialized write path (measured under
      US-019/TKT-82 if it ever matters).

## Decision

We adopt allocation rows with derived usage. Specifics:

- **Accounting (all under the pool `FOR UPDATE`, all int64):** channel consumption =
  confirmed + live claims (the shared `liveClaims` predicate — never a re-derived expiry
  expression) carrying that `channel_code`. A channel hold needs an *active* allocation with
  cap headroom **and** pool headroom. A public hold (claims.channel_code NULL — the implicit
  default channel, no allocation row) additionally may not eat capacity still reserved for
  active allocations: `Σ GREATEST(cap − consumed, 0)` over active allocations.
- **Scheduled give-back is lazy:** past `release_at` an allocation stops matching the active
  predicate — new holds on that channel reject, its unsold remainder is publicly claimable,
  and existing holds finish their lifecycle untouched. No sweeper, no released flag.
- **Administration:** `PUT /internal/slots/{id}/channel-allocations` atomically replaces the
  full set under the pool lock (no transient over-commitment while moving cap between
  channels). Caps must sum ≤ pool capacity and each cap must cover its channel's current
  consumption. State-idempotent PUT — no idempotency-key registry. Omitting a channel closes
  it; historical claims keep their attribution (no FK from claims to allocations).
- **Idempotency (ADR-009):** `channel` joins the buyer-hold request fingerprint. The
  channel-less fingerprint stays byte-identical to the pre-channel format, so idempotency
  records created before this migration keep replaying; a replay that changes or drops the
  channel is a key reuse.
- **Public availability is channel-scoped (semantic change):** `GET /slots/{id}/availability`
  keeps pool aggregates for `capacity`/`held`/`confirmed`, but `available` now means "claimable
  on the requested channel" — the optional `channel` query parameter scopes it; omitted means
  public/default. The endpoint stays in ADR-004's 5-second remaining-capacity tier, so release
  visibility may lag DB time by up to 5s on reads; write correctness switches exactly at
  PostgreSQL time. Staff availability adds `public_available` and a per-channel breakdown.
- **Operational holds (ADR-023) stay unchanneled:** they consume pool capacity only (and can
  therefore exhaust the pool while a channel has nominal headroom — accepted); conversion
  produces a public buyer hold. Allocation configuration changes are sellability config, not
  claim lifecycle mutations — they do not enter `claim_history`.

## Consequences

- **Positive:**
    - No-oversell extends to per-channel caps with the same single-lock proof shape as
      ADR-010; the contention smoke asserts exact grant counts.
    - Nothing can drift: every number is derived from claims; expiry and give-back are
      predicates, not jobs.
- **Negative:**
    - The serialized write path gains per-channel aggregation queries (indexed by
      `claims(pool_id, channel_code, status, expires_at)`); revisit only with US-019 data.
    - Full-set PUT has no stale-write protection (`If-Match`); acceptable while allocation
      editing is single-operator.

## References

- TKT-78 (US-015), parent epic TKT-4; PRD `docs/product/prd-v1.md` §TKT-4
- [ADR-010](ADR-010-postgres-claim-transaction.md) — lock order, lazy DB-time transitions
- [ADR-009](ADR-009-contract-first-apis.md) — fingerprint/replay exactness
- [ADR-023](ADR-023-operational-holds-and-conversion.md) — operational holds interplay
- [ADR-004](ADR-004-cache-first-read-path.md) — availability cache tier
