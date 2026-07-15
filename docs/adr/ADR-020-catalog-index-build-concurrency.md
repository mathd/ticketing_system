# ADR-020: Catalog index builds stay non-concurrent until migrations leave startup

Date: 2026-07-15

## Status

Accepted (approved at the TKT-62 plan gate, 2026-07-15).

**Amended 2026-07-15 (TKT-66):** precondition (1) is now **satisfied** —
[ADR-022](./ADR-022-out-of-band-service-migrations.md) moved migrations out of the startup path.
**Preconditions (2) and (3) remain false, so this ADR's decision is unchanged: CIC is still not
adopted.** The preconditions are conjunctive; satisfying (1) alone changes nothing. TKT-66's premise
that decoupling would let TKT-62's CIC question be re-opened was **incorrect** and was corrected at
its plan gate — re-opening CIC needs a second replica or external writer **and** a populated target
table. Where this ADR describes migrations running at startup, read it as the state that held before
ADR-022; the reasoning is unaffected, because it turned on there being no *other session* to protect,
which is still true.

## Context

Every index in `services/catalog/internal/store/migrations/` is built with a plain `CREATE INDEX`,
which holds a lock conflicting with inserts, updates and deletes for the duration of the build.
The Codex adversarial review of PR #42 (TKT-60) flagged this as a potential catalog outage on
deploy and asked for `CREATE INDEX CONCURRENTLY` (CIC), which Postgres only allows outside a
transaction — goose supports that via `-- +goose NO TRANSACTION`.

The five index sites are not equivalent, and the review's "every index" framing is too broad:

| Site | Table | Hazard |
|---|---|---|
| `0001:44` `performances_public_read` | `performances`, **created at `0001:33`** | none — same migration |
| `0001:56` `ticket_types_by_performance` | `ticket_types`, **created in the same migration** | none |
| `0005:11` `series_by_event` | `series`, **created in the same migration** | none |
| `0006:23` `performances_capacity_group_idx` | pre-existing `performances` | **real** |
| `0007:13` `performances_by_event` | pre-existing `performances` | **real** |

An index built on a table created in the same transactional migration locks a table no other
session can see yet. Only 0006 and 0007 build against a table that may hold rows and take writes.

The decisive constraint is [ADR-008](./ADR-008-embedded-migrations.md): each service embeds its
migrations and runs `goose.Up` **at startup, before listening**, and **fails fast** if migration
fails (`services/catalog/internal/store/postgres.go:23`, `services/catalog/cmd/catalog/main.go:91`).
Catalog runs single-instance, with no advisory lock and no replicas configured. A 30-second
deadline bounds the migration (`services/catalog/cmd/catalog/main.go:92`).

The only other path to the catalog database is `compose.yaml`'s `127.0.0.1:${POSTGRES_PORT:-5432}`
publish — loopback-only, a developer's `psql`, not a production writer. Each service holds its own
database and role (ADR-007), so no sibling service writes `catalog` either.

This decision is needed now because TKT-62 asked whether to change how every index in the service
is built — a repo-wide convention change — and because the answer is non-obvious enough that it
was re-derived twice during planning.

## Possible Solutions

- **Option 1 — Adopt CIC now: `NO TRANSACTION` migrations, reconcile the two existing indexes, remove the 30s deadline:**
    - Pros: matches the standard production playbook; the convention is settled in code, not prose;
      no future migration can forget it.
    - Cons: **buys nothing at this topology** — CIC exists so *other sessions* keep writing during
      the build, but under ADR-008 the migration runs before this service listens, single-instance,
      so during a catalog deploy there is no other writer to protect. It costs a materially slower
      build, and startup-before-listen adds that time directly to catalog API downtime. Worse, a
      cancelled CIC leaves an **INVALID** index: under fail-fast startup the next boot either
      strands a phantom index that `IF NOT EXISTS` hides (defeating ADR-019's scan assertions) or
      fails forever, so catalog never starts. Removing the deadline to avoid that accepts unbounded
      startup on a service that must migrate before it listens. The reconciliation machinery needed
      to spare already-migrated databases a rebuild protects databases that do not exist — every
      database here is built from the chain on a clean clone.
- **Option 2 — Record the convention, defer adoption until migrations are decoupled from startup (chosen):**
    - Pros: keeps the deploy behaviour we can reason about; records the corrected premise and the
      adoption traps while they are understood; names the actual blocker instead of hiding it behind
      a slower, brickable deploy.
    - Cons: the convention is prose, not enforced by code; if a populated `performances` table
      appears before migrations are decoupled, the next index migration stalls writes for the build.
- **Option 3 — Adopt CIC *and* decouple migrations from service startup in one change:**
    - Pros: the only combination where CIC delivers its benefit.
    - Cons: rewrites ADR-008's deploy model for all five services; far beyond an index-build
      decision, and it would bury the topology change inside a migration-hygiene ticket.

## Decision

We **do not adopt `CREATE INDEX CONCURRENTLY` in catalog migrations yet**, and we leave all five
existing index migrations and the 30-second migration deadline unchanged.

CIC is the right long-term target, but its benefit is unreachable while ADR-008 runs migrations at
service startup, before listen, single-instance. Adopting it first takes the cost (slower builds
landing on API downtime, an INVALID-index startup brick) without the benefit. **The blocker is the
startup coupling, not the DDL.**

**Preconditions that unblock adoption** — when all hold, revisit this ADR:

1. ~~Migrations are decoupled from service startup (run out-of-band, or advisory-locked and run once)
   — this is ADR-008's own "revisit when a second replica exists" hook~~ — **satisfied** by
   [ADR-022](./ADR-022-out-of-band-service-migrations.md) (TKT-66); **and**
2. more than one replica or an external writer exists, so there is a session for CIC to protect; **and**
3. a target table is populated enough that the build duration matters.

**Traps for whoever adopts it** (recorded so they are not re-derived):

- CIC cannot run inside a transaction: the migration needs `-- +goose NO TRANSACTION`, which applies
  to both its Up and its Down (goose parses it per file, not per direction).
- **The 30s deadline does reach a non-transactional statement.** goose's no-transaction branch runs
  `db.ExecContext(ctx, query)` (`migration_sql.go:71-77` in v3.27.2), so the deadline's context is
  propagated: a build still running at 30s is cancelled, not merely un-awaited. This is why the
  deadline and CIC cannot coexist — verified in the dependency source, not assumed. Since ADR-022
  the deadline lives in each binary's `migrate` subcommand
  (`services/catalog/cmd/catalog/main.go:55`), not on the startup path; it is still propagated, so
  this trap is unchanged.
- A cancelled or failed CIC leaves an **INVALID** index behind. `CREATE INDEX CONCURRENTLY IF NOT
  EXISTS` then sees a same-named relation and does nothing: the index exists, is never used by the
  planner, and ADR-019's scan assertions are the only thing that would catch it. Reconcile the
  invalid index explicitly (drop it, then rebuild) rather than relying on `IF NOT EXISTS`.
- Recovery must use a plain `DROP INDEX`; `DROP INDEX CONCURRENTLY` cannot run inside a `DO` block.
- The 30s deadline (now in the `migrate` subcommand — `services/catalog/cmd/catalog/main.go:55`)
  must be removed **in the same change** that adopts CIC, never before — it is correct for the plain
  builds we keep, and fatal to a concurrent one.
- Scope any adoption to indexes added to **pre-existing** tables. Making 0001/0005 non-transactional
  would weaken schema atomicity and slow bootstrap to protect a table nobody can see.

**Partial `performances_by_event` — deferred.** TKT-62 also asked whether the index should become
`(event_id) WHERE status='published'`, since its only consumer filters on published. We keep
`(event_id, status)` unchanged: the repo has no representative published/draft distribution or write
workload to measure against, and `TestGetPublishedSeasonIsIndexScoped` seeds almost entirely
published rows, which would rig the comparison in the partial index's favour. Settling it needs a
representative status ratio, `EXPLAIN (ANALYZE, BUFFERS)`, index size, and publish/archive write
cost — measured, not asserted. Until then [ADR-019](./ADR-019-catalog-read-path-scoping.md)'s named
index stands.

## Consequences

- **Positive:**
    - Deploy behaviour stays bounded and reasonable about: a failed migration fails fast in 30s
      rather than hanging startup indefinitely or stranding an unusable index.
    - The corrected premise is written down — "every index" was wrong, and the 0001/0005 sites were
      never a hazard, so no future ticket re-opens them.
    - The real constraint (startup-coupled migration) is surfaced as its own decision instead of
      being worked around.
- **Negative:**
    - The convention is prose, not enforced. A future index migration on a populated table can still
      be written as a plain `CREATE INDEX` with nothing but review to catch it.
    - If a populated `performances` table appears before migrations are decoupled, that build stalls
      writes. Accepted: at that point startup-before-listen stalls the whole service anyway, so the
      index build is not the binding constraint — the architecture is.
    - This ADR is aspirational by construction; it records a convention not yet in force. Mitigated
      by the concrete preconditions above.

## References

- TKT-62 · TKT-60 (PR #42, where the Codex review raised it) · TKT-2
- [ADR-008](./ADR-008-embedded-migrations.md) — the startup-coupled migration model this defers to
- [ADR-019](./ADR-019-catalog-read-path-scoping.md) — `performances_by_event` and the scan assertions
- [goose annotations](https://pressly.github.io/goose/documentation/annotations/)
- [PostgreSQL CREATE INDEX](https://www.postgresql.org/docs/18/sql-createindex.html)
