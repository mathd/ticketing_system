# ADR-074: Associate seats with tickets at issuance

Date: 2026-09-25

## Status

Accepted (TKT-164, slice 1)

## Context

Commerce stores the seat identities on a reservation. Access issues tickets from
`order.completed`, whose payload has the order line but no seat set. A ticket row
currently has no field for a seat identity. Older seated tickets have no reliable
ticket-to-seat mapping, so their identities cannot be reconstructed.

This slice records seat identity for newly issued tickets. It does not change the
partial seated-refund refusal. That work moved to TKT-497.

## Possible solutions

- **Keep seat identities only in Commerce.** This avoids an Access migration, but
  Access cannot answer which seat an issued ticket represents.
- **Add the seat set to `order.completed`.** This avoids a synchronous read, but
  changes a shared event payload and its consumer-skew rules (ADR-017).
- **Add a seat identity to each Access ticket row and read the order at issuance.**
  This keeps the association with the ticket and uses the order ID already carried
  by the event.

## Decision

Access owns the ticket-to-seat association. Before it starts the issuance
transaction, it reads Commerce with `GET /internal/orders/{id}/seats?organizer_id=`
using `X-Internal-Token`. It validates the returned order line against the event,
sorts the seat identities, and assigns them to ticket ordinals in that order.

The read returns 200 for an existing order. For an unseated order it returns
`seated: false` and an empty `seat_identities` array. A non-NULL empty seat set is
invalid stored data. Missing and foreign-organizer orders both return 404. Invalid
IDs or a missing organizer query return 400; read failures and invalid stored data
return 500.

The `tickets.seat_identity` column is nullable. New seated tickets receive one
identity each in the same transaction that inserts the ticket and its `issued`
lifecycle event. New unseated tickets and historical tickets keep NULL. Access does
not add the seat identity to a lifecycle event or its canonical form.

This is honest-writer data, not tamper evidence. The claim is that the Access
application writes the association with the ticket. A database writer can change
the `seat_identity` column without detection from the lifecycle trail, whose
canonical form does not include this field. No claim is made against that adversary.

## Consequences

- Partial seated refunds remain refused for historical tickets with NULL seat
  identity. No mapping is fabricated, and this ADR does not change inventory,
  catalog, or refund behavior.
- Every `order.completed` issuance, including an unseated order, now depends on a
  successful Commerce read. Access retries issuance failures up to four processing
  attempts. A count, line, or identity mismatch also retries, then appears in the
  existing `issuance_exhausted` failure record with stage `issuance`. Access does
  not start its transaction or issue a NULL-associated ticket when the response is
  invalid. The reason label does not distinguish a permanent mismatch from an outage.
- The seat read does not authenticate `order.completed` and is not a security control. It checks the order ID,
  organizer, slot, ticket type, quantity, and seat identities. It does not check order status, buyer ID, or
  guest order reference. A forger with commerce's NATS credentials can use an unpaid order that still has a
  reservation row, including one whose seat hold may have been released, or keep those checked fields from a
  real order and choose a different buyer or guest reference. A compromised principal can read order lines
  from `order.completed` payloads as described in ADR-072 §6(a); the smoke test reads the line from the
  database for convenience and pins only the completed-order case. Signed event envelopes tracked by TKT-296
  remain the fix.
- On a prolonged Commerce outage, Access commits neither tickets nor a consumed
  event receipt for the failed issuance. Operators use the existing failed-event
  recovery procedure: inspect the failure record, repair the dependency, find the
  original envelope in the durable `PLATFORM` stream by `source_event_id`, then
  republish that envelope with a new `Nats-Msg-Id` replay suffix. Keep the original
  event ID. Access uses it for idempotency. Do not build a replacement payload from
  the failure record.
- `order.exchanged` uses a separate issuance path. Its replacement tickets remain
  unassociated and NULL in this slice. Commerce currently refuses seated exchanges;
  a later seated-exchange change must read seats by the replacement order ID.
- The seat identity records the stable family identity string, not a seat-map row or
  version ID. A seat-map edit that preserves that identity does not change the
  ticket's association.

## References

- TKT-164, slice 1
- TKT-497, partial seated-refund return
- [ADR-017: Domain event schema evolution](ADR-017-domain-event-schema-evolution.md)
- [ADR-021: Ticket lifecycle trail integrity](ADR-021-ticket-lifecycle-trail-integrity.md)
- [ADR-029: Seat identity pinning contract](ADR-029-seat-identity-pinning-contract.md)
- [Access failed-event recovery](../development.md#access-failed-event-recovery)
