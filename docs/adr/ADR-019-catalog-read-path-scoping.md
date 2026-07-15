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
   which the defect bites. Both subset reads carry both tests: the season pair in
   `season_smoke_test.go`, the festival pair in `festival_smoke_test.go` (TKT-65).

   **Assert the plan under `force_generic_plan`, not a value-bound custom plan** — even for a
   read with no nullable predicate. **This is a mutation-sensitive check on predicate shape, not
   a simulation of production.** It is worth doing for one reason only: it is the condition under
   which the assertion can *fail* when the predicate is wrong.

   A custom plan is built knowing the parameter, so it can use the scoping index *whether or not
   the predicate is sound*. Measured on the festival read, under the test's seeded statistics: an
   `EXPLAIN` of a deliberately widened `(capacity_group_id = $1 OR $1 IS NULL)` still chose
   `performances_capacity_group_idx` when planned with the value bound, and only the generic plan
   refused it. So a custom-plan assertion is green against both the sound and the widened
   predicate — the definition of a test that proves nothing. Planning blind is what distinguishes
   the shapes, so planning blind is what the test does.

   Both reads use the same `explainGenericPlan` helper; forking it per read is how one copy
   quietly stops asserting anything.

   **EXPLAIN the statement production runs — not a copy of it.** Until TKT-63,
   `TestGetPublishedSeasonIsIndexScoped` EXPLAINed a *hand-copied, reduced* query (`SELECT p.id`
   over `performances` + `events` with the scoping predicate retyped) while production
   `publicPerformances` joined four more tables, projected ~24 columns and sorted. That test
   could only prove the predicate was index-*compatible*; the shipped read was free to drift to
   a catalog scan with it green, because it was not testing that query. A reduced copy also
   cannot catch a *construction*-level regression — the assembled production query silently
   ceasing to use the scoped predicate at all.

   The blocker was that a 14-line query string cannot be shared with a test without coupling
   the test to formatting noise. Splitting the read into composed query consts (TKT-63)
   dissolved it: the test now passes `scopedPublicPerformancesQuery` — the exact statement the
   season read executes — to `explainGenericPlan`, with nothing retyped. Both sabotages redden
   it: restoring the nullable `OR`, and re-assembling the scoped query around the unscoped
   predicate.

   Assert only the indexes that carry the *scoping* claim (`events_pkey`,
   `performances_by_event`), not every join's. The other joins may legitimately change access
   path without touching what the test is about.

   **A trap worth naming, because it produced a green test that proved nothing.** The
   generic-plan question cannot be answered by sending `EXPLAIN <query with $1>` through the
   driver: the driver's prepared statement is the `EXPLAIN`, so the inner query is planned with
   the value already bound and you get a **custom** plan whatever `plan_cache_mode` says — both
   predicate shapes then return an identical, indexed, literal-substituted plan and the
   assertion is vacuous. Observing a cached generic plan needs a server-side
   `PREPARE`/`EXPLAIN EXECUTE` (`explainGenericPlan`). A generic plan is recognisable by the
   parameter surviving as `$1` in the output instead of appearing as a literal; the helper
   asserts that, so the trap cannot silently return.

## Consequences

- **Positive:** the next "scope read X to Y" ticket starts by naming an index instead of
  copying a query, which is the step that turns the fix from a no-op into a fix. ADR-004's
  intent gains a miss-path rule it never had, without being rewritten.
- **Negative:** an `EXPLAIN` assertion couples a test to the planner. It needs a seeded
  catalog large enough to make the scan unattractive (2000 events in the season test), which
  is slower than the smoke tests around it, and a future Postgres could pick a different but
  equally scoped plan and fail it for the wrong reason — the assertion is on the index name,
  not on cost.
- **Positive — rule 2 is now a gate for both subset reads, not a review standard.** This ADR
  first recorded that the rule outran its enforcement: the season `EXPLAIN` test duplicated a
  reduced query (so a regression in `publicPerformances` would not have reddened it), and the
  festival read had no plan assertion at all. TKT-63 closed the first by EXPLAINing
  `scopedPublicPerformancesQuery`; TKT-65 closed the second by extracting
  `publishedFestivalPerformancesQuery` and EXPLAINing that. Each read's physical-cost assertion
  is now bound to the statement it actually executes, and each is sabotage-verified — including,
  for the festival, the nullable-`OR` regression transplanted from TKT-63.
- **Negative — the evidence is conditional, and saying so is part of the rule.** Both plan tests
  measure `force_generic_plan` against *seeded* statistics. They do not prove what production's
  planner does against production's data distribution, and a future Postgres could pick a
  different but equally scoped plan and redden them for the wrong reason (the assertion is on the
  index name, not on cost). What they do prove is that the shipped statement's predicate is
  index-compatible when planned blind — which is the claim the two defects this ADR came from
  both violated.
- **Negative — a scoping test costs a query const.** Binding a plan assertion to the shipped
  statement requires the query to *be* a referenceable value rather than a literal built inside
  its function. That is a real constraint on how these reads are written, and it is the price of
  the assertion being about production instead of about a copy.
- **Negative — the plan assertion measures a mode production may never run, and cannot tell you
  whether it does.** `force_generic_plan` is what makes the predicate's *shape* testable: a plan
  built with the parameter in hand can use the scoping index whether the predicate is sound or
  widened, so a value-bound plan cannot distinguish them. A blind plan can.

  Production runs `auto`, and `auto` is conditional: it uses custom plans for roughly the first
  five executions, then builds a generic plan and compares its estimated cost against the average
  custom-plan cost (which includes repeated planning), adopting the generic plan only if it is not
  more expensive and otherwise continuing to re-plan per execution. **Whether these reads ever run
  a generic plan in production is therefore not something this ADR knows, and not something these
  tests can establish** — the experiment above ran against seeded fixture statistics, not
  production's data distribution, and plan choice is a function of that distribution.

  So the assertion's scope is exactly: *the shipped predicate is index-compatible when planned
  blind, under the fixture's statistics, and the assertion reddens if the predicate is widened.*
  That is a canary on query shape. It is worth having because it is the only mechanism here that
  reddens on the regression — and it must not be sold as more. Both directions of overclaim are
  available and both are wrong: "this is what production runs" and "production would never run
  this" are equally unsupported. Reaching for either would be this ADR's own failure mode.
- **Resolved by TKT-63 — the generic-plan gap on `events`.** The `($1::uuid[] IS NULL OR e.id =
  ANY($1))` predicate had to be planned for a NULL `$1` as well, so under `force_generic_plan`
  the planner could not use `events_pkey` and fell back to scanning `events`
  (`Filter: (($1 IS NULL) OR (id = ANY ($1)))`). TKT-63 split the scoped and unscoped reads into
  separate SQL texts; the scoped one filters unconditionally and keeps `events_pkey` and
  `performances_by_event` in every plan mode. This was robustness against a mode nothing here
  sets, not a live defect — the season read was correctly scoped under `auto` either way.

## References

- TKT-53 (festival scoped read), TKT-60 (season scoped read + `performances_by_event`), TKT-64 (promotion),
  TKT-63 (scoped/unscoped SQL split; generic-plan robustness; binds the season plan assertion to the shipped SQL),
  TKT-65 (festival plan assertion + result-scope test; rule 2 becomes a gate for both reads)
- [ADR-004](./ADR-004-cache-first-read-path.md) — cache-first intent; this ADR covers the miss path
- [ADR-018](./ADR-018-catalog-slot-transition-concurrency.md) — the write side of the same table
- `services/catalog/internal/store/postgres.go` (`publicPerformances`, `publishedFestivalPerformancesQuery`),
  `season_smoke_test.go` (`explainGenericPlan`), `festival_smoke_test.go`
- `docs/learnings/2026-07-15-prove-tests-fail.md` — the general form of rule 2
