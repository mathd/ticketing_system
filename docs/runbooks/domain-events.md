# Domain-event recovery

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## Access failed-event recovery

Access classifies invalid `order.completed` envelopes as permanent and records only their
source identifier/fingerprint plus a bounded reason on
`platform.access.ticket-issuance.failed`. Transient issuance or delivery failures retry four
times on the configured JetStream backoff, then produce the same sanitized terminal record.
Failure-record publication happens before termination. Processing is bounded at four attempts,
while a failed terminal-record publication leaves the source message eligible for redelivery
without a JetStream delivery ceiling. Counters use the low-cardinality `reason` and `stage` labels.

To recover, inspect the failed record, repair the producer or downstream dependency, locate the
original event in the durable `PLATFORM` stream by `source_event_id`, and republish that original
envelope with a new `Nats-Msg-Id` replay suffix. Keep the event's own `id`: Access issuance and
delivery are idempotent on that identifier. Never manufacture a payload from the failed record;
it deliberately does not retain attacker-controlled event data. If the failure subject itself is
unavailable through all six deliveries, JetStream's max-deliver advisory is the operator signal
to restore the stream and replay the original message.

## Inventory catalog-event quarantine operations

Future-schema catalog events (version skew, ADR-017 §5b) are no longer parked against the
durable's ack window: inventory persists the raw envelope to the bounded
`catalog_event_quarantine` table (cap 10 000 unresolved rows) and acks the original, so variants
this binary *does* understand keep flowing (TKT-68). Readiness still latches false on the first
quarantined event and stays down across restarts while unresolved rows exist — the alarm is
Postgres-backed, not ack-window-backed.

To recover: deploy an inventory binary whose schema registry covers the quarantined variants,
then run `inventory reprocess-quarantine`. It is a one-shot subcommand. Since TKT-326 it applies
each stored envelope **in-process**: it runs the live consumer's handler on the exact stored bytes,
against inventory's own database. It publishes nothing, and it needs no NATS connection and no NATS
credential. It needs `DATABASE_URL`, `CATALOG_URL` and `INTERNAL_SERVICE_TOKEN`, because the
handler resolves some events against catalog (schema-1, schema-4 and schema-5 publications, and
every closure). It refuses to start, before it reads a row, when `CATALOG_URL` or
`INTERNAL_SERVICE_TOKEN` is empty. The inventory container already has all three:

```bash
docker compose run --rm inventory reprocess-quarantine
```

**The subcommand is `reprocess-quarantine`, not `/app reprocess-quarantine`.** The image declares
`ENTRYPOINT ["/app"]` (`build/go.Dockerfile:21`), so an explicit `/app` makes argv
`/app /app reprocess-quarantine`; `os.Args[1]` is then `/app`, which is not a registered
subcommand, and the binary **starts in server mode** instead of reprocessing
(`services/inventory/cmd/inventory/main.go:39-41`; found by ai-review F4).

Each run is one pass over the unresolved rows, oldest first. It prints
`applied=N terminal=N unsupported=N deferred=N`:

- **applied**: the handler acked the event. Its projection, or its moot record, is committed, and
  the row is resolved.
- **terminal**: the handler made its terminal decision and terminated the event. The row is
  resolved. Its envelope bytes stay in the row for inspection until retention prunes them. A
  terminal decision usually means the payload is invalid at a schema this binary knows, but not
  always: a valid seated publication that meets conflicting stored adjacency is refused with
  `ErrSeatProjectionIncomplete` and terminates (its transaction rolls back). For such a row,
  inspect the stored projection, not the envelope. Repairing that stored state can make the same
  bytes applicable again.
- **unsupported**: this binary still cannot read the schema. The row stays pending.
- **deferred**: the handler asked for a delayed retry and no store write failed. The causes are an
  offering event that arrives before its pool, a catalog outage, and an unusable catalog answer.
  The handler's log line gives the cause. The row stays pending. A deferred row is never parked: a
  quarantine row has no broker delivery count, so the ADR-077 park bound cannot apply to it.

The exit status is zero when rows stay unsupported or deferred. It is non-zero on a store failure
(an immediate retry, or a failed moot record or moot fallback write), on a failed mark, on a
missing catalog configuration, and when the run is cancelled (SIGINT or SIGTERM). **A catalog
outage is a deferral, so the run exits zero: read the `deferred` count.** A pool-kind mismatch
(`ErrPoolKindMismatch`) is an immediate retry in the live handler, so the command exits non-zero on
every run until the conflict is repaired. That is loud on purpose.

Then restart inventory — startup confirms no unresolved rows remain and readiness returns.
Unsupported and deferred rows stay unresolved and keep readiness down after the restart. **This
changed with TKT-326:** before, a supported row was marked as soon as the broker accepted its
republication, so a row that could not apply yet did not keep readiness down. Now it does, until a
later run applies it. Repair the cause (provision the pool, restore catalog), then run the command
again and restart.

`reinjected_at` keeps its TKT-68 name. Since TKT-326 a non-null value means that the handler
applied or terminated the event. A value written by an earlier binary means only that the broker
accepted a republication, not that inventory business processing completed (that is
`consumed_events`' job). Resolved rows are pruned 7 days after resolution; unresolved rows never
age out. If the quarantine itself fills, new future-schema events fall back to delayed NAKs — a
deliberate, loud stall at an inventory-owned bound, not a drop.

## Inventory moot slots and parked catalog events (TKT-317)

Two kinds of catalog event leave a durable record. ADR-077 gives the decisions and the limits.

- `moot_slots` holds moot records: slots that catalog did not publish when one of their events was
  handled. Each record has a `source`. A `publication` record lets a later archive or closure for
  that slot be consumed without a pool. A `closure` record is kept, and it authorises nothing: the
  archive waits for its pool. Rows are never deleted.
- `catalog_event_parked` holds a known catalog event whose catalog answer stayed unusable through
  five deliveries. The broker message was terminated. The row keeps the exact bytes that were
  delivered, and `reason` holds the catalog error. Only schema-1 publications and closures park,
  because only they call the performance lookup. Organizer conflicts and transport failures do
  not park.

Neither record changes readiness. A parked event is not version skew, so `reprocess-quarantine`
does not apply to it.

### Inspect

The inventory database is `inventory`:

```bash
docker compose exec postgres psql -U postgres -d inventory
```

```sql
-- Parked catalog events, oldest first. envelope is the exact delivered bytes.
SELECT subject, event_id, schema, organizer_id, slot_id, delivery_count, reason, parked_at,
       encode(envelope, 'escape') AS envelope
FROM catalog_event_parked
ORDER BY parked_at;

-- Moot records: slots catalog did not publish when one of their events was handled. Only a
-- 'publication' source lets a later archive or closure through without a pool.
SELECT organizer_id, slot_id, source, source_event_id, recorded_at
FROM moot_slots
ORDER BY recorded_at;
```

### Signals

- Counter `inventory.catalog.events.parked`. It rises by one for each newly parked event. The
  `subject` attribute names the catalog subject. A duplicate park of the same bytes does not rise.
- ERROR log `catalog event parked after unusable catalog answers; terminating`, with `subject`,
  `event_id`, `schema`, `slot_id`, `deliveries` and `err`.
- Below the bound, each retry logs ERROR `catalog answer unusable; retrying`.
- This change adds no alert rule. The counter and the log are signals. The row is the durable
  evidence.

### Re-drive

No command re-drives a parked event. The steps below are manual.

- `inventory reprocess-quarantine` reads `catalog_event_quarantine` only. It does not read
  `catalog_event_parked`.
- No catalog command re-emits a closure or an archive. The catalog re-emit commands
  (`reemit-policies`, `reemit-orphan-prevention`, `reemit-best-available-ordering`) re-emit
  publications only, and none of them targets one event. Each one selects the slots that are
  published when it reads a batch, so a slot archived after that read can still be re-emitted.
  `reemit-policies` re-emits every ungrouped slot that is published when its batch is read.

1. Read the `reason` column, and repair the catalog answer.
2. Restart inventory. For a parked closure whose pool exists, startup reconciliation reads the
   closure state from catalog and applies it. A parked publication creates no pool, so
   reconciliation does not repair it. If reconciliation fails at startup, inventory starts
   fail-open, and the event stream is the only convergence path. The existing durable does not
   redeliver a terminated message.
3. If the event must be applied, publish its stored `envelope` unchanged to the subject in the
   `subject` column. Use a new `Nats-Msg-Id`. The broker drops a message whose id it already holds
   inside its duplicate window. Publish with the `platform-admin` principal
   (`NATS_ADMIN_PASSWORD`). Besides catalog, it is the only principal with publish rights on
   catalog subjects (ADR-072). TKT-326 removed the operator principal that this step used before.
   This step is not automated, and TKT-317 did not run it.
4. A re-driven event is not in `consumed_events` until it applies, so it applies once. If it
   parks again with the same bytes, the row does not change. Different bytes under the same
   event id are refused, and the first copy stays.

While a parked closure stays unrepaired, the pool keeps its old closure state. An open pool in
inventory still accepts new holds, even if catalog has closed the slot.
