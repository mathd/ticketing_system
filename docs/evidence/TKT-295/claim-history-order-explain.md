# TKT-295 — `claim_history` read plans before and after `append_order` leads

Measured 2026-10-07 on PostgreSQL 18.3 (`postgres:18-alpine`, default settings), against a
database migrated by `inventory migrate`. Each clause ran three times, alternating, with
`EXPLAIN (ANALYZE, BUFFERS)`.

**Dataset:** 1,020,000 `claim_history` rows. 20,000 claims × 50 numbered rows; 10,000 legacy rows
(NULL `append_order`, 5 per claim on 10% of claims); one hot claim with 5,050 rows; one hot pool
with 5,000 `adjust_capacity` rows. Indexes: the existing `claim_history_claim (claim_id,
occurred_at)` and `claim_history_pool (pool_id, occurred_at) WHERE pool_id IS NOT NULL`.

Old clause: `ORDER BY occurred_at, append_order NULLS FIRST, id`.
New clause: `ORDER BY append_order NULLS FIRST, occurred_at, id`.

| Read | Rows | Clause | Plan | Time (3 runs, ms) | Sort |
|---|---|---|---|---|---|
| `History`, hot claim | 5,050 | old | Sort > Bitmap Heap Scan > Bitmap Index Scan (`claim_history_claim`) | 3.11, 2.79, 2.76 | quicksort 626 kB |
| `History`, hot claim | 5,050 | new | same | 2.74, 2.77, 3.34 | quicksort 626 kB |
| `History`, typical claim with legacy rows | 55 | old | same | 0.76, 0.46, 0.44 | quicksort 29 kB |
| `History`, typical claim with legacy rows | 55 | new | same | 0.46, 0.48, 0.48 | quicksort 29 kB |
| `CapacityHistory`, hot pool | 5,000 | old | Incremental Sort > Index Scan (`claim_history_pool`) | 2.60, 2.58, 2.74 | incremental |
| `CapacityHistory`, hot pool | 5,000 | new | Sort > Index Scan (`claim_history_pool`) | 4.76, 2.85, 3.28 | quicksort 661 kB |

No run wrote temporary blocks. `History` keeps its plan. `CapacityHistory` loses the incremental
sort the `(pool_id, occurred_at)` index allowed and sorts in memory instead — at 5,000 rows a
difference within run-to-run variation.

**Decision: no index.** Nothing here is a repeatable material regression or a spilling sort. An
index on `(…, append_order NULLS FIRST, occurred_at, id)` would need a new migration under
ADR-020/ADR-022 rules, and this evidence does not justify one. Revisit if a single history grows to
a size where the in-memory sort spills.
