# Early schema acceptance needs semantic completeness, not idempotency

**TKT-180 (PR #150) — 2026-08-03**

## What happened

[ADR-017](../adr/ADR-017-domain-event-schema-evolution.md) §5b′ requires consumers to dispatch on
`schema` before decoding `data`, so consumers must accept a new variant before any producer emits
it. Applied literally, that rule is **unsafe for some consumers**.

A consumer that accepts a new variant and records completion for work it did not do creates a
permanent, silent gap: inventory's `consumed_events` row makes a later, capable binary
short-circuit, so the pool stays rule-less for ever and nothing reports it. Parking the message
would have been safe; accepting it was not.

Access, by contrast, was safe to update early — not because its handler is idempotent, but because
schema 5 changes nothing it owns, so **no future access binary will ever need to redo that event**.

The rule, now in [ADR-041](../adr/ADR-041-orphan-seat-arbitration.md):

> A consumer may accept a new schema variant early only if its current handler performs **every
> effect this consumer will ever require from that variant** — or if the later-required effects
> remain replayable despite any completion marker.

Stated as "safe when handling is idempotent", the rule authorises irreversible partial handling
merely for being repeatable. That was the first, wrong version of the amendment.

## Three consequences worth keeping

- **A shared merge does not order a deployment.** Two services start independently. If the order
  matters, it needs separate revisions — not one PR containing both halves.
- **Parking is not self-healing here.** Quarantined originals are acked, so recovery is
  `reprocess-quarantine` **plus a restart**, by an operator. Do not assert the optimistic version
  in an ADR without checking the recovery path.
- **Slicing a feature for safety can create a window the slices must then close.** Shipping a
  *setting* before its *transport* left records created in the gap permanently un-migrated, and
  nothing in the original plan repaired them. When a split separates a setting from its transport,
  ask what happens to rows created in between — that question produced a whole extra ticket.

## What to do

When a ticket raises a schema ceiling, the plan must state, **per consumer**, what its handler
persists — not merely whether it parses the new variant.
