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

---

## TKT-202 amendment — "not logged" audited, and made true (2026-08-17)

The decision above is unchanged. This records **which sinks were audited**, because the "not logged"
clause was false in practice from the day guest retrieval shipped until TKT-202, and the next reader
should not have to re-derive whether traces were considered.

`guest_order_ref` appears in a **URL path segment** on three route shapes — the guest bundle, the QR
image, and the storefront ticket page (proxied under the gateway's `/` catch-all). Three sinks wrote
that path:

1. `shared/go/obs/requestlog.go` — one line per request, in **all six binaries**, not only the gateway.
2. `shared/go/contract/http.go` — the contract-drift error log; an independent second emitter.
3. The **OTel server span**: `otelhttp` sets `url.path` from the raw path inside the dependency, and
   `shared/go/obs/setup.go` exports spans over OTLP. Invisible to any grep of this repo, and the
   worst of the three, since the value left the process to a collector.

All three now route the path through `obs.SanitizedPath`, which replaces **declared capability
segments** and leaves every other route byte-identical. The rule is a table of route shapes
(`shared/go/obs/capability_path.go`), so a future capability URL inherits it by adding a row.

**The adversary, named (ADR-021).** This bounds **whoever can read this platform's logs and traces**,
from the change forward. It bounds **nothing** against: a reference already written to a retained log
or an exported trace (still valid — there is no rotation of the underlying capability); anyone with
the access database; the collector or any hop upstream of it; browser history, referrer headers, or
proxies outside this repo. "Not logged" is now true of this platform's emitters. It is not a claim
that the reference is unexposed.

**Not in scope, deliberately.** The query string is still not logged anywhere in this repo, and
nothing here starts logging it — ADR-049 § *TKT-222 amendment* and ADR-050 § 5 both rely on that. The
QR image link's `sig`/`exp` were checked and are unaffected. The `{ticket}` segment on the QR route is
preserved on purpose: it is not redeemable alone (ADR-050's signed-link check gates the image), and
keeping it is what lets an operator correlate a specific fetch with a report.

## References

- [ADR-003](./ADR-003-append-only-audit-trail.md) · [ADR-009](./ADR-009-contract-first-apis.md) · TKT-29 · TKT-43
- TKT-202 · [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) · [ADR-049](./ADR-049-customer-identity-and-storefront-sessions.md)
