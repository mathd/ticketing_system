# ADR-077: Moot tombstones and bounded parking for catalog offering events

Date: 2026-10-09

## Status

Accepted (TKT-317). Residual 3 under "What is not claimed" is open and needs an owner decision.

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
none of them re-emits an archived slot. A second publication for a slot that has a tombstone is
therefore a test fixture today, and not a live state.

Live schema 2 to 5 publications never call the performance lookup. Schemas 4 and 5 fetch
seat-map geometry instead. Only schema-1 publications and closures call the performance lookup.

## Possible Solutions

- **Do nothing.** Moot archives and unusable answers wait for ever, with no signal to an operator.
- **Terminate unusable answers.** A catalog blip then loses a publication for good. ADR-017 and
  TKT-307 reject this.
- **Quarantine parked events.** The quarantine table latches readiness. ADR-017 reserves that for
  version skew, so it is not the right table.
- **Record outcomes and bound the retries (chosen).** Record a moot outcome before the ack. Park
  an unusable answer after five deliveries, in its own table, and keep readiness unchanged.

## Decision

- **Tombstones.** When a publication or a closure gets a 404, inventory writes `moot_slots` (the
  organizer, the slot, the source event and a time) in the same transaction as the event's
  `consumed_events` row. The ack follows the commit. A failed write retries the event. Rows are
  never deleted.
- **Fallback.** An archive or closure that finds no pool is consumed as moot only when a tombstone
  exists for its organizer and slot, and the pool is still absent. Both checks are in the statement
  that records the consumption. Otherwise the event waits, as before.
- **Bound.** An unusable answer is retried until the delivery count reaches five (`NumDelivered`,
  which counts every delivery, including transport failures). At five or more, the exact envelope
  is written to `catalog_event_parked` (the first copy is kept), the park is counted in
  `inventory.catalog.events.parked`, it is logged at ERROR, and the message is terminated.
- **Not bounded.** A failure to reach catalog, or a status other than 200 or 404, retries with no
  limit, as before. Organizer conflicts stay poison and terminate.
- **Readiness.** Neither disposition changes readiness.
- **Migration 0020** creates both tables. Its Down step refuses while either table has rows.

## Consequences

- **Positive.** An archive for a moot slot no longer waits for ever. A catalog that answers badly
  for good now stops occupying an ack slot, and an operator can see it.
- **Positive.** Each record commits before its ack. A crash between the commit and the ack is
  safe, because the redelivery writes the same record again.
- **Negative.** A parked event is terminated, and nothing re-drives it automatically. The
  operator steps are in `docs/development.md`.
- **Negative.** A parked closure leaves the pool's closure state stale until a restart or a manual
  re-drive. Holds are refused only while the state is closed, so a closed slot can keep taking
  holds until then.
- **Negative.** Two tables and one metric to operate.

## What Is Not Claimed

1. **No automated recovery for parked rows.** No inventory code re-drives them. The re-drive
   steps in `docs/development.md` are manual, and TKT-317 did not run them.
2. **The broker's redelivery clock is not tested.** The tests set the delivery count directly.
   They do not test the five-second delay, the broker's `MaxDeliver` setting, or how often the
   broker redelivers.
3. **A closure tombstone does not prove that the slot's publication is settled (open).** A
   publication can still be pending when a closure and a later archive for the same slot are
   handled. This happens if the publication's delivery was NAKed and is redelivered later. The
   closure's 404 writes a tombstone, the archive is consumed, and the publication then provisions
   the pool. For a solo slot, nothing archives that pool afterwards. This was not reproduced by a
   test. A decision is needed. Either a closure's 404 writes no tombstone, or the fallback waits
   until the publication settles. Grouped slots were not analysed.
4. **The fallback is a predicate, not a lock.** The pool row is absent, so there is no row to lock.
   A provisioning that commits after the fallback statement's snapshot is not excluded. The race
   needs a second writer, such as another inventory process that provisions the same pool.
5. **Time is not bounded.** The bound counts deliveries. It does not measure how long a catalog
   answer stayed unusable.
6. **Trailing bytes are not checked.** Catalog's performance decoder reads the first JSON value
   and ignores what follows. This is unchanged by TKT-317.
7. **Not tamper-evident (ADR-021).** This is honest-writer consistency. A writer with database
   access can add or delete tombstones and parked rows.

## References

- TKT-317
- [ADR-009](./ADR-009-contract-first-apis.md) (domain-event envelope, decision point 5: the id is unique per emission)
- [ADR-017](./ADR-017-domain-event-schema-evolution.md) (version skew, poison, readiness)
- [ADR-018](./ADR-018-catalog-slot-transition-concurrency.md) (catalog slot transitions)
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) (adversary wording)
- [ADR-022](./ADR-022-out-of-band-service-migrations.md) (migrations run out of band)
- [ADR-072](./ADR-072-nats-publisher-acls.md) (`inventory-reprocess` publish permission)
