# ADR-019: Catalog read-path scoping and the evidence a scoping fix owes

Date: 2026-07-15

## Status

Accepted (TKT-53 / TKT-60; recorded here in TKT-64)

## Context

ADR-004 decided that public catalog reads are cache-first: on a hot on-sale, read load must
scale with cache and memory, not with the database. It fixed the *intent* and the TTL
discipline. It said nothing about what a read costs **on a cache miss** — and a miss is
exactly when an on-sale is at its worst.

Two subset reads have now been fixed for the same defect, one ticket apart:

- **`GetPublishedFestival` (TKT-53).** Loaded the whole published catalog and filtered in
  memory to the festival's days.
- **`GetPublishedSeason` (TKT-60).** Called `ListPublishedEvents()` — every published event,
  all organizers — and filtered in memory to the season's event set. One season read scaled
  with all published inventory.

Both endpoints promise a read whose cost is proportional to *its own* subset. Both delivered
a read proportional to the global catalog. The results were correct in both cases, which is
why neither was caught by a test.

The repair taught two things that the code alone does not say, and that cost real time:

- **TKT-60 was filed as "do what TKT-53 did".** Doing exactly that would have shipped a
  no-op. `GetPublishedFestival` scales because `performances_capacity_group_idx` (0006)
  exists — not because of its bespoke query shape. Seasons filter by `event_id`, which had
  no index until TKT-60 added `performances_by_event` (0007). The query shape was the
  visible half of TKT-53's fix; the index was the half that did the work.
- **TKT-53's test asserted the output only** — that the festival read returned the right
  days — and would have passed against the unfixed code, because the unfixed code also
  returned the right days. It proved the wrong claim, greenly.

## Possible Solutions

- **Option 1: a bullet in `AGENTS.md`.**
    - Pros: agents see it without a second read; smallest diff.
    - Cons: `AGENTS.md` is loaded on every task, so the cost is paid whether or not a
      catalog read is in scope; it is the shape ADR-018 has just finished extracting *out*
      of that file; and the rules need their evidence to be usable, which is more prose than
      an index file should carry.
- **Option 2: extend ADR-004.**
    - Pros: ADR-004 owns the read path, and this is the miss-path half it never covered.
    - Cons: ADR-004 decided cache-first *intent* — TTLs, CDN-compatibility, staleness as a
      reviewed decision. These are implementation and test-evidence rules learned afterwards
      from two defects. Folding them in would retroactively restate a decision accepted at
      its own gate, and would bury a narrow rule inside a broad one.
- **Option 3: a new ADR, with `AGENTS.md` reduced to a pointer.**
    - Pros: the rule lands in the decision log where it is discoverable and can be
      superseded; `AGENTS.md` stays an index; the evidence stays one hop away for the tasks
      that need it; symmetric with ADR-018, which covers the *write* side of the same table.
    - Cons: an agent scoping a catalog read must follow the link.

## Decision

We adopt Option 3. Two rules, scoped narrowly to **catalog reads whose contract claims cost
proportional to a subset**.

1. **A scoped read is only scoped if an index backs the filter.** Before claiming a read is
   scoped to a subset, name the index that serves the filter and confirm it exists. Copying
   the query shape of a read that already scales copies the visible half of the fix; the
   index is the half that does the work. `GetPublishedFestival` filters on
   `capacity_group_id` and is served by `performances_capacity_group_idx` (0006);
   `GetPublishedSeason` filters on `event_id` and is served by `performances_by_event`
   (0007), which TKT-60 had to add. A filter with no index behind it is a sequential scan
   wearing a `WHERE` clause.

   The scoped path in `publicPerformances` (`services/catalog/internal/store/postgres.go`)
   carries this as a comment at the seam. **This is not a general "add an index" edict** —
   it applies where a read's contract or COS claims subset-proportional cost. An unscoped
   listing that is *meant* to read the catalog needs no such index, and an index added
   without a read that needs it is write-path tax for nothing.

2. **A scoping fix owes two tests, because it makes two claims.** *Result scope* and
   *physical scan cost* are different assertions, and only the second one is the fix:

   - **Result scope** — a poison row. Seed a published row *outside* the subset that the
     read must never load, then assert the result excludes it.
     (`TestGetPublishedSeasonDoesNotScanForeignEvents`.)
   - **Physical scan cost** — an `EXPLAIN` assertion. A correct result can still be produced
     by reading the entire catalog and discarding rows; that is precisely the defect. Assert
     the plan reaches the rows through the named index.
     (`TestGetPublishedSeasonIsIndexScoped`.)

   An output-only assertion cannot distinguish the fixed code from the broken code, because
   the broken code returns the correct answer. A plan assertion is only meaningful once a
   sequential scan is the *wrong* choice — on a two-row table Postgres rightly ignores the
   index and the assertion fails for an unrelated reason — so the test seeds a catalog large
   enough that scanning it is the expensive option, which is also the only condition under
   which the defect bites. Both tests live in
   `services/catalog/internal/store/season_smoke_test.go`.

## Consequences

- **Positive:** the next "scope read X to Y" ticket starts by naming an index instead of
  copying a query, which is the step that turns the fix from a no-op into a fix. The
  `EXPLAIN` assertion makes a scoping regression fail loudly rather than silently costing a
  catalog scan on every miss. ADR-004's intent gains a miss-path rule it never had, without
  being rewritten.
- **Negative:** an `EXPLAIN` assertion couples a test to the planner. It needs a seeded
  catalog large enough to make the scan unattractive (2000 events in the season test), which
  is slower than the smoke tests around it, and a future Postgres could pick a different but
  equally scoped plan and fail it for the wrong reason — the assertion is on the index name,
  not on cost. It also does not prove the plan holds in production: it is measured under the
  planner's `auto` mode with the test's statistics. TKT-63 measured the shipped query under
  `force_generic_plan` and found `events` degrades to a sequential scan there — a latent
  robustness gap under a mode nothing sets, filed rather than fixed.

## References

- TKT-53 (festival scoped read), TKT-60 (season scoped read + `performances_by_event`), TKT-64 (promotion)
- [ADR-004](./ADR-004-cache-first-read-path.md) — cache-first intent; this ADR covers the miss path
- [ADR-018](./ADR-018-catalog-slot-transition-concurrency.md) — the write side of the same table
- `services/catalog/internal/store/postgres.go` (`publicPerformances`), `season_smoke_test.go`
- `docs/learnings/2026-07-15-prove-tests-fail.md` — the general form of rule 2
