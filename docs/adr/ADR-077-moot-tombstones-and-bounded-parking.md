# ADR-077: Moot tombstones and bounded parking for catalog offering events

Date: 2026-10-09

## Status

Accepted (TKT-317). Owner decision D7 narrows the fallback authority to publication records. The
residual that remains is item 3 under "What Is Not Claimed". It is accepted for TKT-317.

## Context

Inventory consumes four catalog offering subjects: `performance.published`, `archived`, `closed`
and `reopened` (ADR-017). Two situations stopped an event from applying as it arrived.

- **Moot.** Catalog answers 404 for a slot that is not published. The publication or closure
  was acked and wrote nothing. An archive for the same slot then found no pool. It waited for a
  pool that no event would create, and it was redelivered for ever.
- **Unusable answer.** Catalog answers 200, but the body does not decode or fails a check. The
  event was retried with no limit.

Catalog moves a performance from draft to published once, and from published to archived. It has
no path back to draft, and none from archived to published (`postgres_slots.go`,
`postgres_transitions.go`). The three catalog re-emit commands select published slots only, so
none of them re-emits an archived slot.

The re-emit commands do publish a slot that is already published. Each one uses an event id that
differs from the live id (`internal/events/events.go`):

- `reemit-policies` calls `PerformancePublishedBackfill`.
- `reemit-orphan-prevention` calls `PerformancePublishedOrphanCorrection`.
- `reemit-best-available-ordering` calls `PerformancePublishedBestAvailableOrderingCorrection`.

So a slot can have two live publication events. A second publication for a slot is a live state,
not a test fixture.

The publication path writes schema 2, and raises it to 3, 4 or 5 (`performancePublishedEnvelope`).
The current code does not emit a schema-1 performance publication. Schemas 2 to 5 never call the
performance lookup. Schemas 4 and 5 fetch seat-map geometry instead. Only schema-1 publications and
closures call the performance lookup. A schema-1 publication that is still in the stream can
therefore be moot. The durable is created with `DeliverAllPolicy`, so it reads every retained event.

## Possible Solutions

- **Do nothing.** Moot archives and unusable answers wait for ever, with no signal to an operator.
- **Terminate unusable answers.** A catalog blip then loses a publication for good. ADR-017 and
  TKT-307 reject this.
- **Quarantine parked events.** The quarantine table latches readiness. ADR-017 reserves that for
  version skew, so it is not the right table.
- **Record outcomes and bound the retries (chosen).** Record a moot outcome before the ack. Park
  an unusable answer after five deliveries, in its own table, and keep readiness unchanged.

## Decision

- **Records.** When a publication or a closure gets a 404, inventory writes a moot record in
  `moot_slots`. The record holds the organizer, the slot, the source (`publication` or `closure`),
  the source event and a time. The record and the event's `consumed_events` row commit in one
  transaction, and the ack follows the commit. A failed write retries the event. Rows are never
  deleted.
- **Key.** The key is the organizer, the slot and the source. A closure record and a publication
  record for one slot can both exist. If the key were the organizer and the slot only, the first
  record would block the second. A closure record written first would turn a later publication
  record into a no-op. The publication would then lose its authority, and an archive would wait for
  ever again. The source is part of the key, so each source keeps its own first event.
- **Authority (D7).** Only a publication record authorises the fallback. An archive or closure that
  finds no pool is consumed as moot only when a publication record exists for its organizer and
  slot, and the pool is still absent. Both checks are in the statement that records the
  consumption. A closure record is kept, and it authorises nothing. A moot closure is acked and
  recorded. The archive that follows waits for its pool.
- **Why a closure record waits (R1).** A closure's 404 says that catalog does not publish the slot
  now. It does not say that no publication will provision the slot. A schema-2 or later
  publication builds its pool from its payload, and from the seat-map geometry for schemas 4 and 5.
  It never calls the performance lookup. A publication that was NAKed can therefore provision an
  open pool after an archive was consumed on a closure record. Replaying the archive cannot repair
  that. The order is pinned by `TestTKT317ClosureRecordDoesNotLetAnArchiveOvertakeAPendingPublication`:
  the publication is NAKed, the closure's 404 is recorded, the archive waits, the publication
  provisions, and the archive then applies and the pool ends archived.
- **Bound.** An unusable answer is retried until the delivery count reaches five (`NumDelivered`,
  which counts every delivery, including transport failures). At five or more, the exact envelope
  is written to `catalog_event_parked` (the first copy is kept), the park is counted in
  `inventory.catalog.events.parked`, it is logged at ERROR, and the message is terminated.
- **Body cap.** The performance lookup reads at most 64 KiB. A longer body is an unusable answer.
- **All-zero group.** A festival group of all zeros names no group. The answer is unusable.
- **Not bounded.** A failure to reach catalog, or a status other than 200 or 404, retries with no
  limit, as before. Organizer conflicts stay poison and terminate.
- **Readiness.** Neither disposition changes readiness.
- **Migration 0020** creates both tables. Its Down step takes a `SHARE ROW EXCLUSIVE` lock on both
  tables before it checks them. A writer with an uncommitted row blocks the lock until that writer
  ends, so the check sees the row. Then the step refuses while either table has rows.

## Consequences

- **Positive.** An archive for a slot that has a publication record no longer waits for ever. A
  catalog that answers badly for good now stops occupying an ack slot, and an operator can see it.
- **Positive.** Each record commits before its ack. A crash between the commit and the ack is
  safe, because the redelivery writes the same record again.
- **Negative.** A parked event is terminated, and nothing re-drives it automatically. The
  operator steps are in `docs/development.md`.
- **Negative.** A parked closure leaves the pool's closure state stale until a restart or a manual
  re-drive. Holds are refused only while the state is closed, so a closed slot can keep taking
  holds until then.
- **Negative.** An archive whose slot has only a closure record waits for its pool. If no
  publication ever provisions the pool, the archive waits for ever, as it did before TKT-317.
- **Negative.** A schema-1 publication with an all-zero group was terminated before this change. It
  is now retried to the bound and parked.
- **Negative.** Two tables and one metric to operate.

## What Is Not Claimed

1. **No automated recovery for parked rows.** No inventory code re-drives them. The re-drive
   steps in `docs/development.md` are manual, and TKT-317 did not run them.
2. **The broker's redelivery clock is not tested.** The tests set the delivery count directly.
   They do not test the five-second delay, the broker's `MaxDeliver` setting, or how often the
   broker redelivers.
3. **Two publications of one slot can consume an archive before the pool exists (accepted
   residual).** Publication P1 is NAKed. A later schema-1 publication P2 for the same slot is
   moot, and it writes a publication record. The archive is then consumed on P2's record. P1
   provisions an open pool later, and nothing archives that pool. The current code does not emit
   schema-1 performance publications, so P2 must be a schema-1 event that is still in the stream.
   No test reproduces this. Only a publication record can cause it. A closure record cannot (D7).
   Grouped slots were not analysed.
4. **The fallback is a predicate, not a lock.** The pool row is absent, so there is no row to lock.
   A provisioning that commits after the fallback statement's snapshot is not excluded. The race
   needs a second writer, such as another inventory process that provisions the same pool.
5. **Time is not bounded.** The bound counts deliveries. It does not measure how long a catalog
   answer stayed unusable.
6. **Trailing bytes are not checked.** Catalog's performance decoder reads the first JSON value
   and ignores what follows. TKT-317 did not change this. The body cap bounds the size of the body,
   not its trailing content.
7. **Not tamper-evident (ADR-021).** This is honest-writer consistency. A writer with database
   access can add or delete moot records and parked rows.

## Amendment (TKT-317 review)

The first accepted version said two things that were wrong:

- A closure record let an archive be consumed. Owner decision D7 removes that authority.
- A second publication for a slot was called a test fixture. The re-emit commands publish a slot
  that is already published (see Context).

The review also added the body cap, the refusal of an all-zero group, and the lock in the
migration's Down step. Each one has a test that fails without it.

## References

- TKT-317
- [ADR-009](./ADR-009-contract-first-apis.md) (domain-event envelope, decision point 5: the id is unique per emission)
- [ADR-017](./ADR-017-domain-event-schema-evolution.md) (version skew, poison, readiness)
- [ADR-018](./ADR-018-catalog-slot-transition-concurrency.md) (catalog slot transitions)
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) (adversary wording)
- [ADR-022](./ADR-022-out-of-band-service-migrations.md) (migrations run out of band)
- [ADR-072](./ADR-072-nats-publisher-acls.md) (`inventory-reprocess` publish permission)
