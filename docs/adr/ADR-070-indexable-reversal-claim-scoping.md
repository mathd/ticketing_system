# ADR-070: The reversal claims' organizer check is a column comparison, not a correlated subquery

Date: 2026-09-02

## Status

Accepted

## Context

Both reversal reconciliation queues, refunds (ADR-062) and exchanges (ADR-063), claim work with
the same three-stage statement: a `claimable` CTE selects under `FOR UPDATE ... SKIP LOCKED`, a
`claimed` CTE leases what it selected, and a final join fetches the identifiers the drive needs.

TKT-267 closed a liveness defect in that shape. A malformed queue row, one whose source
reservation belongs to a different organizer than the queue row itself, was selected and leased
before the final join ran, then dropped by that join. Never returned meant never released and
never abandoned: nothing charged an attempt, nothing parked it, nothing cleared the lease, and it
retook a claim slot on every lease expiry while the function reported no error. Worse than losing
one slot, a leased-then-dropped row makes the store return fewer rows than it leased, and the
runner reads a short batch as a drained queue and ends the pass.

The fix moved the check into `claimable` as a correlated `EXISTS` over `orders` and
`reservations`. That was correct. It also could not be part of the partial queue index, so
PostgreSQL evaluated it once per candidate row and the `LIMIT` could not stop early on the rows it
rejected. The claim's cost became linear in the size of the rejected prefix rather than bounded by
the batch.

Measured on PostgreSQL 18.4 during TKT-267's review, `order_exchanges`, batch 16:

| malformed rows ahead of the queue | execution | buffers |
|---|---|---|
| 0 (today's population) | ~0.1 ms of subplan work, 16 probes | 112 |
| 500 | ~4 ms | n/a |
| 50,000 | 263 ms | 350,585 |
| 50,000, without the predicate | 0.34 ms | 35 |

TKT-267 accepted this because the alternative is worse. Without the check those rows are leased
and dropped, which wedges the sweep outright. Slow beats stuck. But nothing bounds how large a
malformed backlog can get. No code path writes a mismatched pair, and a writer with commerce
database access can, with a botched repair script the realistic author of a large one.

Two facts constrain any remedy.

The narrow lock was bought by the `EXISTS` being a `WHERE`-clause subquery rather than a
`FROM`-list entry. Rewriting the check as a join against those tables regresses `orders` and
`reservations` from `AccessShareLock` to `RowShareLock`, which puts this hot sweep in contention
with checkout. `TestTheClaimLocksTheQueueRowOnlyAndNeverTwice` is the tripwire.

And `orders` carries no `organizer_id`. Its primary key is `id` alone, and `reservations`'
`organizer_id` is a plain column outside its key. The organizer is only reachable as
`order -> reservation -> organizer_id`.

## Possible Solutions

- **Option 1: make the mismatched pair unwritable with a composite foreign key.**
    - Pros:
        - The relationship becomes a schema guarantee. The `EXISTS` disappears and the queue
          index alone decides claimability.
        - Harder to drift than a maintained copy: no trigger to forget.
    - Cons:
        - Not writable against today's schema. It needs `organizer_id` on `orders`, maintained
          from `reservations`; a `UNIQUE (id, organizer_id)` on `orders` to be referenced; and two
          composite foreign keys. That denormalizes a third table to avoid denormalizing two.
        - It makes the malformed state unrepresentable, which destroys the fixtures that prove any
          of this works. The acceptance test seeds 50,000 malformed rows, and both TKT-267
          regression tests construct a mismatched pair directly.
- **Option 2: denormalize the source organizer onto the queue rows.**
    - Pros:
        - Puts the value the claim needs on the row the claim scans, so the partial index can
          carry it and malformed rows never enter the index.
        - Leaves the lock property untouched: the statement stops reading `orders` and
          `reservations` altogether.
        - Malformed rows stay representable and diagnosable.
    - Cons:
        - Duplicated identity needs maintenance on every path that can change the relationship,
          including the parent side.
        - A careless derivation is worse than none. Copying the queue row's own `organizer_id`
          makes the predicate true by construction, so it can never refuse.
- **Option 3: accept the cost and bound it.**
    - Pros:
        - No schema change.
    - Cons:
        - Capping the inspected prefix moves starvation to the first window rather than removing
          it.
        - Parking a rejected row records a structurally malformed row as a failed reversal, when
          no reversal was attempted. That changes what the queue's state means.

## Decision

We adopt Option 2. Both `order_refunds` and `order_exchanges` carry a `source_organizer_id`
derived from the source reservation, and both claim statements test `source_organizer_id =
organizer_id` as a plain column comparison. The same predicate is in both partial queue indexes,
so a malformed row is absent from the index rather than read and discarded.

The value is derived by database trigger, never by the caller and never from the queue row's own
`organizer_id`. The trigger overwrites whatever a writer submits. A copy of `organizer_id` would
make the predicate true by construction, which is a precondition that cannot fail. Parent-side
changes to `orders.reservation_id` or `reservations.organizer_id` move the derived value with
them, so the guarantee does not rest on the convention that nobody rewrites those links.

No `CHECK (source_organizer_id = organizer_id)`. Making the malformed state unrepresentable would
make every test that proves this mechanism unbuildable.

Both halves are load-bearing and neither is sufficient. With the predicate in the query but not
the index, the malformed rows are still in the index, read, and filtered out afterwards: the
naming of an index is not evidence that the scan is narrow. With it in the index but not the
query, the planner cannot use that index for this statement.

Measured with the shipped shape at a 50,000-row malformed prefix, batch 16: 2 buffers, 0.039 ms,
zero rows removed by filter. Removing the equality from the index alone returns the scan to
reading and discarding all 50,000.

## Consequences

- **Positive:**
    - The claim's cost is bounded by the batch. A malformed backlog no longer slows the sweep in
      proportion to its size.
    - The claim statement no longer reads `orders` or `reservations` at all, so it takes no lock
      on them whatever. The lock property TKT-267 defended is strengthened rather than preserved.
    - TKT-267's regression tests keep pinning the same property against the new mechanism, and the
      acceptance test turns measurements that lived only in a ticket into something the gate runs.
- **Negative:**
    - Two more maintained columns and four triggers. The parent-side maintenance exists for a case
      production does not currently exercise, which makes it the half most likely to be dropped by
      a future edit; it has its own test for that reason.
    - Rolling migration 0026 back is refused while any malformed row exists, because rolling back
      restores a claim scan whose cost is linear in exactly those rows. Operators must repair the
      mismatches or roll forward.
    - The lock-property comment in the exchange query now describes a hazard that no longer applies
      to the shipped statement. It is kept because the edit that reintroduces it, rewriting the
      check as a join, is exactly the edit a future reader is likely to attempt.

### Name the adversary (ADR-021)

This is **honest-writer consistency and bounded claim work. It is not tamper-evidence.**

It holds against concurrency, crashes, restarts, replicas, and application code that writes a
mismatched pair by mistake. Every writer that goes through the triggers gets an authoritative
source organizer, whatever it submits.

It constrains an adversary with commerce database access **not at all**. That writer can disable
or replace the triggers, forge both organizer columns, drop or redefine the index, or edit rows
after the fact. The scope is identical to the ADR-021 paragraphs in ADR-062 and ADR-063, and
unchanged by this decision: the signed, append-only payments journal remains the evidence that
money moved.

## References

- TKT-268; TKT-267 (the defect this bounds the cost of)
- [ADR-062](./ADR-062-refund-reversal-reconciliation.md), [ADR-063](./ADR-063-exchange-reversal-reconciliation.md) — the two queues
- [ADR-019](./ADR-019-catalog-read-path-scoping.md) — a scoped read is only scoped if an index backs the filter
- [ADR-020](./ADR-020-catalog-index-build-concurrency.md) — plain `CREATE INDEX`; `CONCURRENTLY` is still not adopted
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) — name the adversary
- [ADR-022](./ADR-022-out-of-band-service-migrations.md) — out-of-band migration placement
- `services/commerce/internal/store/migrations/0026_reversal_source_organizers.sql`
