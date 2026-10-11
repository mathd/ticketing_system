# TKT-321: plan of the recovery backlog read

Measured 2026-10-10 on PostgreSQL 18.4 (`postgres:18.4-bookworm`, default settings, normal planner
cost selection: no `enable_seqscan=off`). The schema was migrated by `store.Migrate` into a private
schema. The statement is the shipped constant `recoveryBacklogSQL`
(`services/commerce/internal/store/recovery.go`), captured with
`EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF, SUMMARY OFF)` after `ANALYZE orders`.

**Dataset:** 22,525 `orders` rows.

| Rows | Kind | In `orders_recovery_claimable_idx`? |
|---:|---|---|
| 20,000 | `completed` (terminal) | no (status outside the set) |
| 2,000 | claimable status, due in the future | yes |
| 500 | parked | no (`recovery_parked_at IS NOT NULL`) |
| 25 | eligible now | yes |

The read returned `eligible_unparked = 26` and `parked total = 500`. The extra eligible row is one
of the "future" rows: it was due one minute after the fixture's clock, and seeding took about 100
seconds, so it was due when the statement ran. This is the predicate working, not a defect.

## Plan

```
Nested Loop (actual rows=1.00 loops=1)
  Buffers: shared hit=554
  ->  Aggregate (actual rows=1.00 loops=1)
        Buffers: shared hit=550
        ->  Seq Scan on orders (actual rows=500.00 loops=1)
              Filter: (recovery_parked_at IS NOT NULL)
              Rows Removed by Filter: 22025
              Buffers: shared hit=550
  ->  Aggregate (actual rows=1.00 loops=1)
        Buffers: shared hit=4
        ->  Index Scan using orders_recovery_claimable_idx on orders orders_1 (actual rows=26.00 loops=1)
              Index Cond: (recovery_next_attempt_at <= now())
              Filter: (((recovery_lease_until IS NULL) OR (recovery_lease_until <= now())) AND (updated_at < (now() - '00:02:00'::interval)))
              Index Searches: 1
              Buffers: shared hit=4
```

## Reading

- **Eligible branch:** `Index Scan using orders_recovery_claimable_idx`, with `Index Cond:
  (recovery_next_attempt_at <= now())`. The status set and `recovery_parked_at IS NULL` are the
  index's partial `WHERE` clause (migration `0005_psp_recovery.sql`, Up section), so they need no
  filter. The lease and grace conditions are a `Filter` on the rows the index selects. It touched
  4 shared buffers to return 26 rows out of 22,525.
- **Parked branch:** `Seq Scan on orders`, filtered on `recovery_parked_at IS NOT NULL`. This is
  expected. The parked aggregate is deliberately unindexed (TKT-146's reasoning: the parked
  population is bounded by attempt exhaustion, so a slow scan is itself the finding). TKT-321 does
  not change it.
- **Limit of this evidence.** The index bounds the rows by due time, not by the lease or grace
  filters. A large population that is due but held by live leases is still visited and filtered.
  This plan was taken on one fixture shape. It is not a guarantee for every distribution.

**Decision: no migration.** The eligible branch already uses the existing partial index.
