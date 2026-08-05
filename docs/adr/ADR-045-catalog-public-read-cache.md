# ADR-045: Catalog serves its public reads from memory

Date: 2026-08-04

## Status

Accepted

*Applies [ADR-004](./ADR-004-cache-first-read-path.md) rule 2 to the **minutes** tier, after
[ADR-044](./ADR-044-inventory-availability-cache.md) proved the shape on inventory's seconds tier.*

## Context

Catalog's four public reads — the event list, event detail, season detail, festival detail — are what
every storefront page view lands on. Each ran its store query on every request.

A cache already sat in front of three of them: the storefront's SSR `PageDataCache`
(ADR-004 § amendment TKT-128). It is process-local to the Astro SSR process, so it helps only the
storefront. The back office reads through plain `fetch` and discards the tier, the gateway caches
nothing, and any second consumer starts cold.

## Decision

**Catalog serves the four public reads from a process-local, read-through cache whose lifetime is
`cachetier.Minutes`, invalidated by classified write paths, with `Age` propagated to the storefront.**

### Not a shared package — and the trigger to change that

The machinery (LRU, single flight, source-call semaphore, generations) is the same as inventory's.
The **semantics** are not: inventory invalidates every variant of one slot, which every write knows;
catalog's list is global and its detail dependencies are not derivable from a write.

So the code is duplicated, deliberately. Recording the shape so the next person does not re-derive
it: both caches are *the same machine over a different key and a different invalidation group* — a
`readcache[K comparable, G comparable, V any]` with a `groupOf func(K) G` would cover inventory
(group = slot) and catalog (group = list/detail class). **The trigger to extract it is the third
consumer**, not the feeling that there is duplication.

### Invalidation is scoped but global, and that is a correctness decision

Two generations, `list` and `detail`. Both **global** — not per id.

The list endpoint has no organizer filter, so any newly listable event changes it. Detail keys *look*
per-id-invalidatable and are not: catalog's writes carry no complete dependency graph, so a
"precise" scheme misses exactly the case that matters — **a member that was ABSENT from a cached
response becoming present**. Adding a ticket type can make an already-published slot listable;
publishing can reach a season directly or through a series; ADR-018 lets a festival day participate
in a series.

Over-invalidating costs a reload. Under-invalidating serves a wrong answer.

**Known operating characteristic, not a surprise to discover under load:** bulk authoring clears all
detail entries repeatedly and depresses the hit rate, bounded by the 16-concurrent-source-call
ceiling.

### The write classification is pinned, because forgetting is the failure mode

`publicReadEffect` in `services/catalog/internal/store/public_read_invalidation_test.go` classifies
every mutating `Store` method as list+detail, detail-only, or none. A method added without a
classification fails the test. **`none` is a claim**, and each was checked against the projection
rather than assumed — `CloseSlot`/`ReopenSlot` because closure is not in the aggregate at all,
`UpdateVenueGACapacity` because `PublicVenue` in the contract is `{id, name}` however much the query
selects, `CreatePriceRule` because these reads carry the base `ticket_types.price_amount`.

The guard found a method missing from the classification on its first run.

### Two entry points, because catalog's writes are not all transactional

This is where catalog **departs from ADR-044**, and it is the single most important fact in this ADR.

Inventory's writes are all transactional, so a post-commit callback on `tx.Commit()` catches every
one. **Catalog's are not: `PublishPerformance` is one atomic `UPDATE` with no transaction.** A
commit-only hook would have looked complete and silently missed publishing — the one write a buyer
notices immediately.

So there are two: `commitPublicRead` for transactional writes, and `notifyPublicRead` for autocommit
ones, called after the statement that changed the row succeeded. `PublishPerformance` announces
**before** its canonical re-read, deliberately: the row is already public, and if that re-read failed
the invalidation must still have happened.

Ordering is post-commit, never pre-commit — invalidating first lets a concurrent read repopulate from
the pre-write row. Same rule, same reason, as [ADR-018](./ADR-018-catalog-slot-transition-concurrency.md)
for event emission.

### `Age`, and why the storefront changed with it

The response is declared publicly cacheable for five minutes and `PageDataCache` started every entry
it fetched at age zero. Adding catalog's cache without propagation would have let a 299-second-old
entry receive another 300 in Astro: **~600 seconds of buyer-visible staleness against a tier that
promises 300.** This ticket would have *introduced* that, so the propagation is not severable from it.

`Age` is declared required, `[0,300]`, rounded up and clamped. `PageDataCache` seeds an entry with
upstream `Age`, counts local elapsed time on top, and does not retain a response that arrived at or
past its max-age. Varying `Cache-Control` instead — what the storefront middleware does for pages —
is unavailable: ADR-028 validates the declared header, so the tier is a fixed value.

`cache.ts`'s header comment claimed *"total staleness never exceeds the tier (no cache stacking)"*.
That was true while it was the only cache in the chain; this change made it false and then true
again, and the comment now says which.

## Consequences

- **Positive:** every consumer gets the saving, not just the storefront SSR process; an authoring
  write is visible on the next read to that process instead of up to five minutes later; total
  chained staleness is ~300s, unchanged from before the cache existed.
- **Negative:** a cache-invalidation bug class now exists in catalog, and the classification map is a
  thing to maintain. Bulk authoring is colder.

## What this does NOT guarantee — name the adversary

[ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)'s rule.

- **Honest-writer consistency, not tamper-evidence.** Direct database writes bypass every callback.
- **Process-local.** A second catalog replica keeps its own entries for the tier. **Trigger to
  revisit: the first time catalog is scaled horizontally.**
- **"The next read" means the next request reaching the writer catalog process.** An already-filled
  storefront entry still serves the pre-write response for up to 300 seconds after the commit. `Age`
  prevents *stacking*; it does not provide end-to-end invalidation. Authoritative cross-process
  invalidation is a separate problem and is not solved here.
- **Bounded cardinality is not bounded bytes.** 2,048 entries and 16 concurrent source calls bound
  the count and the query concurrency; the single global list value can itself be large.
- **Not a rate limiter.** A hostile caller can still force real queries with unknown detail ids.
- **The classification guard stops honest omissions**, not someone editing the map in the same
  commit, and it checks completeness only — that every write is *classified*, not that each one's
  implementation reaches the helper. The behavioural and smoke tests cover that half.

## References

- [ADR-004](./ADR-004-cache-first-read-path.md) · [ADR-044](./ADR-044-inventory-availability-cache.md)
  (the seconds-tier precedent) · [ADR-018](./ADR-018-catalog-slot-transition-concurrency.md)
  (after-commit ordering) · [ADR-019](./ADR-019-catalog-read-path-scoping.md) (the miss path stays
  index-scoped) · [ADR-028](./ADR-028-response-drift-fail-closed.md) · [ADR-006](./ADR-006-astro-storefront-shell.md)
  (storefront caching ownership) · [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)
- `services/catalog/internal/api/public_read_cache.go` ·
  `services/catalog/internal/store/public_read_invalidation.go` ·
  `services/catalog/api/openapi.yaml` (`PublicReadAge`) · `web/storefront/src/lib/cache.ts`
- Kill-switch: TKT-210, which owns ADR-004's incident-bypass requirement for every service.
