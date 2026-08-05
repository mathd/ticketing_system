# ADR-044: Inventory serves public availability from memory

Date: 2026-08-04

## Status

Accepted

*Implements [ADR-004](./ADR-004-cache-first-read-path.md) rule 2, which had been accepted and
unimplemented since 2026-07-12. That ADR's TKT-128 amendment recorded the gap and routed it to
TKT-31; this is the first in-service cache in the stack.*

## Context

An on-sale's load is overwhelmingly reads, and `GET /slots/{id}/availability` is the read buyers
poll hardest — ADR-004 gives it the **seconds tier** precisely because that is where staleness is
most load-bearing. Every one of those polls ran `store.Availability` against Postgres: up to four
round trips (pool row plus live claims, then either the channel-allocation branch or the
reserved-for-channels branch). At the festival NFR — ~50–100k capacity, 100k+ queued buyers — that
is the read load ADR-004 was written to shed, and no part of rule 2 existed anywhere.

The correctness question was already settled and is not reopened here: [ADR-002](./ADR-002-services-from-day-one.md)
and [ADR-010](./ADR-010-postgres-claim-transaction.md) put oversell prevention in the atomic claim.
A stale *display* is acceptable because the claim resolves it. What was missing was a bound on how
stale, and a mechanism that keeps reads off the database between writes.

## Possible Solutions

- **Option 1 — read replicas.** Pros: no invalidation logic. Cons: replica lag is worse staleness
  than a designed TTL and comes without the control; every poll still costs a database query.
  ADR-004 already rejected this shape.
- **Option 2 — a shared cache (Redis, or a CDN in front of the gateway).** Pros: survives process
  restarts, shared across replicas. Cons: a new operational dependency and a second consistency
  problem, for a system whose Compose topology has no shared cache of any kind today. It also does
  not remove the need for write-path invalidation — it moves it across a network.
- **Option 3 — a process-local in-memory cache invalidated from the write path (chosen).** Pros:
  no new dependency; invalidation is a function call in the same process as the commit; the TTL is
  the tier the response already advertises. Cons: process-local, so it does not survive restarts and
  does not extend across replicas.

## Decision

**Inventory serves the public availability read from a process-local, read-through cache whose
lifetime is `cachetier.Seconds` — the same value the response header advertises — invalidated from
the store's commit path.**

Five properties, each with a test that fails without it:

1. **The TTL is not a literal.** It is `cachetier.Seconds.Duration()`, the same registry entry that
   renders the `Cache-Control` header ([TKT-204](./ADR-004-cache-first-read-path.md)). The cache's
   lifetime and the lifetime promised to clients cannot drift apart.
2. **Concurrent misses for one key produce one load.** An expiring hot slot must not stampede
   Postgres — the failure mode a cache is supposed to prevent, not cause.
3. **Invalidation happens after the commit, never before.** Invalidating first lets a concurrent
   read repopulate the entry from the pre-commit row, so the cache would serve the old number for a
   full tier. This is the same rule [ADR-018](./ADR-018-catalog-slot-transition-concurrency.md) sets
   for catalog's state-deriving transitions, and for the same reason.
4. **A load that started before a write committed can never reach a reader that arrived after it.**
   Each slot carries a generation; an in-flight load captures it. Two consequences, and the second is
   the one that is easy to miss: the load's result is **discarded** rather than cached, *and* the
   load is **unjoinable** by later readers. Discarding alone is not enough — a reader arriving after
   the commit could still join the pre-commit load and be handed the old number directly, bypassing
   the cache entirely. Without both, rule 3 is defeated by timing alone.
5. **The claim path never reads the cache.** ADR-010's transaction is the only source of truth for a
   claim. Enforced structurally: only the public display handler may reference the collaborator.

### Where invalidation lives, and why it is not in the handlers

`main.go` builds **one** `*store.Postgres` and hands it to both the API and the NATS consumer. So
the notification is a callback on the store's commit — `commitAvailability(tx, slot)` — rather than a
list of call sites in the API layer. That choice is what makes two otherwise-certain bugs impossible:

- **The consumer is covered without knowing a cache exists.** Publication, archival and closure all
  change offering status, which forces `available` to zero, and none of them pass through a handler.
  A handler-level invalidation list would have shipped a slot that kept reading "available" after the
  event closed it.
- **Forgetting is caught at review time.** `TestAvailabilityMutationsUseInvalidatingCommit` parses
  the store package and rejects raw `tx.Commit()`, with a named exemption list. Adding a write path
  now forces a decision instead of silently serving a stale number.

Invalidation is indexed by **slot**, deliberately broader than the read key (organizer + slot +
channel): the consumer paths know the pool but not always its organizer. Dropping too much costs one
reload; dropping too little serves a wrong number for a tier.

### The `Age` header

The response is declared publicly cacheable for five seconds. Without `Age`, an entry already four
seconds old inside the service would grant a conformant client another full five — ten seconds of
observable staleness against a tier that promises five. `Age` (RFC 9111) is declared **required**,
an integer in `[0,5]`, and rounded **up** so it is never optimistic.

Varying `Cache-Control` by remaining freshness — what `web/storefront/src/middleware.ts` does for
pages — is not available here: [ADR-028](./ADR-028-response-drift-fail-closed.md) makes that header a
required single-valued enum on this operation, so a second value is a 500.

### Not adopted: negative caching

`ErrNotFound` is **not** cached. It would protect a single indexed primary-key lookup that returns no
rows — the cheapest query in the service — at the cost of a failure mode nobody asked for: a newly
published slot answering 404 until its entry expired. Revisit only if TKT-207's load proof shows
unknown-UUID probing actually costs something.

## Consequences

- **Positive:** buyer polls for a hot slot cost one query per tier per key instead of one per
  request; staleness is bounded by a number that cannot drift from the header; the consumer and every
  future write path are covered by construction.
- **Negative:** a cache-invalidation bug class now exists in inventory. The mitigations are the five
  tested properties above, the architecture guard, and the kill-switch (TKT-210).

## What this does NOT guarantee — name the adversary

[ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)'s rule: say which adversary a guarantee is
against, because "the cache is invalidated" is exactly the kind of claim that overreaches.

- **Honest-writer consistency, not tamper-evidence.** Every guarantee here holds against code in this
  process writing through `*store.Postgres`. Anyone who can write to inventory's database directly
  changes rows without invoking any Go callback, and the cache will serve the old answer for a tier.
  Nothing here detects that.
- **Process-local.** With a second inventory replica, a write invalidates only the writer's process;
  the others stay stale until the tier expires. There is one inventory process today. **Trigger to
  revisit: the first time inventory is scaled horizontally** — not "eventually".
- **Bounded memory is not bounded load — but concurrency IS bounded.** Three ceilings, not two:
  10,000 entries globally, 128 per slot, and **1,000 concurrent source calls**.

  The third is the one that matters under attack, and it bounds *queries*, not bookkeeping. The
  first two count an entry only once its load **completes**, so they constrain nothing about a
  caller sending unique slot ids — both key components are caller-supplied on a public route. The
  ceiling is a semaphore taken before a load starts and released when it ends, which also bounds the
  in-flight map by the same number for free, since a load exists only while it holds a slot.

  It **queues rather than sheds**: a request waits for a slot, cancellable by its own context.
  Shedding would turn a cache into an availability outage, a worse failure than a slow read. Each
  load carries a **query budget** (10s) — not a backstop but an actual budget, because a load holds
  its slot for its whole life, and enough hung queries would take every slot and stop the cache
  serving misses at all.

  What remains open, and must not be overstated: an attacker can still make this service issue up to
  1,000 concurrent real queries against a 25-connection pool. **This is not a rate limiter**, and it
  must not be cited as one.
- **"The next read reflects the write" means the next request reaching this process.** An HTTP
  response already delivered to a client cannot be recalled; it expires within the declared tier,
  which is what `Age` keeps true.

## References

- [ADR-004](./ADR-004-cache-first-read-path.md) (rule 2, and the TKT-128 amendment that routed this
  gap) · [ADR-002](./ADR-002-services-from-day-one.md) · [ADR-010](./ADR-010-postgres-claim-transaction.md)
  (correctness at claim time) · [ADR-018](./ADR-018-catalog-slot-transition-concurrency.md)
  (after-commit ordering) · [ADR-024](./ADR-024-channel-allocations.md) (why channel is in the key) ·
  [ADR-028](./ADR-028-response-drift-fail-closed.md) · [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)
- `services/inventory/internal/availability/cache.go` · `services/inventory/internal/store/availability_invalidation.go`
  (`commitAvailability`) · `services/inventory/internal/api/server.go` (`availability`) ·
  `services/inventory/api/openapi.yaml` (`AvailabilityAge`)
- Kill-switch: TKT-210, carrying ADR-004's incident-bypass requirement.
