# ADR-012: Ticket issuance, lifecycle and QR credential boundary

Date: 2026-07-13

## Status

Accepted

## Decision

Access owns ticket issuance, the append-only ticket lifecycle trace, QR credentials and delivery attempts. Commerce owns orders and buyer PII. A completed checkout creates a CSPRNG UUIDv4 `guest_order_ref`, distinct from the deterministic order ID; it is a no-store guest retrieval capability and is not logged or written to the money journal/lifecycle payload.

Commerce publishes its first domain event, `platform.commerce.order.completed` schema 1, only after the completion transaction commits. The ADR-009 envelope has deterministic event and NATS message IDs and contains identifiers only: order, guest reference, organizer, buyer pseudonym, slot, ticket type and quantity. Access consumes it with a durable JetStream consumer and inbox de-duplication keyed by envelope ID.

Access creates one ticket and `issued` lifecycle event per authoritative quantity. QR payloads are versioned compact Ed25519 signatures over ticket, order, organizer and slot identifiers plus issue time; they contain no PII, money or mutable admission state. The active private seed and `kid` are injected configuration. Delivery resolves the recipient email from Commerce at send time using the internal credential; no email is persisted or logged by Access. `delivered` is a unique append-only lifecycle event and the log mailer uses a deterministic delivery ID.

## Consequences

- The gateway exposes only Access's no-store guest bundle/QR endpoints; Commerce's PII endpoint remains internal.
- Ticket issuance is at-least-once safe for JetStream redelivery and checkout replay. A process crash after Commerce commits but before it publishes may delay issuance until a checkout replay. Scheduled/restart outbox recovery remains TKT-43 work.
- TKT-30 can validate the Ed25519 payload and derive redemption from this lifecycle trace without changing ticket storage.

## References

- [ADR-003](./ADR-003-append-only-audit-trail.md) · [ADR-009](./ADR-009-contract-first-apis.md) · TKT-29 · TKT-43
