# A time cutoff decided behind a lock queue must use decision time, not transaction time

**Seen:** TKT-78 (PR #54), found by adversarial review pass 1, 2026-07-16.

## The bug shape

PostgreSQL `now()` is frozen at transaction start. Any transaction that BEGINs, then *waits on
a row lock* across a time boundary, decides that boundary with stale time once the lock frees.
In TKT-78 the channel-allocation release predicate (`release_at > now()`) let a channel hold
queued on the pool lock across `release_at` sell a released channel — exactly during the
high-contention windows the feature targets.

**Fix:** the boundary predicate uses `clock_timestamp()` (decision time). The claim-expiry
predicate (`liveClaims`) deliberately stays on `now()` — the split is safe because the global
capacity check is independent of the allocation predicate, so mixed time bases cannot combine
into an oversell. That independence argument is load-bearing: re-make it before splitting time
bases anywhere else. (ADR-024 records both.)

## The test trap (review pass 2)

A regression test for this bug is **vacuous unless it proves the waiter queued before the
boundary**. The naive shape — start a goroutine, sleep past the cutoff, assert rejection —
passes under the broken code too whenever the goroutine starts late (its transaction then
begins after the cutoff, and stale `now()` is already past it). The honest shape:

1. Establish and cross the cutoff by **database** clock, never host time.
2. Observe the hold's backend in `pg_stat_activity` waiting on the lock (`wait_event_type='Lock'`)
   while DB time is still before the cutoff — fail the setup, not the assertion, if it never queues.
3. Mutation-check it: revert the predicate to `now()` and confirm the test fails.

See `TestReleaseCutoffHoldsUnderPoolLockContention` in
`services/inventory/internal/store/channel_allocations_smoke_test.go`.

## Where this applies next

Every future DB-time boundary decided inside a lock-serialized transaction: sales-window
open/close (TKT-17/TKT-5), pricing rule effectivity, entitlement windows.
