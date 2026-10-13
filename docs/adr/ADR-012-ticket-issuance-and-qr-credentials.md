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
segments**. The rule is a table of route shapes (`shared/go/obs/capability_path.go`), so a future
capability URL inherits it by adding a row.

**Matching is deliberately broader than routing, and that is a decision, not an accident.** The
sanitiser reduces `.`/`..`/empty segments the way `net/http`'s ServeMux does, and compares literals
case-insensitively — so it also redacts the capability position of requests the routers would answer
`404` or `307` to. The two errors are not symmetric: redacting a route-shaped 404 costs an operator
one segment of a request that did nothing, whereas declining to redact it writes a **live,
redeemable, unauthenticated credential** into the log. A 404 does not make the reference less valid,
and the request logger runs *before* the router, so it records the path either way. Sanitisation
therefore follows the **shape of the secret**, not route identity.

Consequently a path that is not a declared capability route is returned **byte-for-byte**, but "not a
declared capability route" is judged on the reduced, case-folded form. `/healthz` and every ordinary
route are unaffected.

One further consequence, found by review rather than by design: on a path that **does** resolve to a
capability route, every segment that `..` discarded is redacted too. `/api/access/orders/<ref-1>/../<ref-2>/tickets`
resolves on `<ref-2>`, and redacting only the surviving segment left `<ref-1>` — an equally live,
equally redeemable reference — in the line. Whether a discarded segment was a credential is not
knowable at that point, so the rule is blunt on purpose: a segment thrown away by path traversal on a
request that resolves to a capability route has no diagnostic value worth the risk.

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

---

## TKT-355 amendment: one ticket-validity verdict in the bundle (2026-10-12)

The decision above is unchanged. This amendment adds one field to the guest bundle and records the
rule that sets it.

**The field.** Each `Ticket` in the bundle has a required `validity`. It is `{status: valid}` or
`{status: voided, reason: refunded | exchanged}`. The contract is a `oneOf` with the discriminator
`status`, so a reason exists only on a voided verdict. The spec comment on `TicketValidity` says why
`oneOf` is correct here, although Commerce and Catalog schemas avoid it.

**The rule.** Access derives the verdict each time it serves the bundle, from the same history slice
that the response carries. It does not store the verdict, and there is no migration.

1. If the history has a `refunded` event, the verdict is voided with reason `refunded`.
2. If not, and the history has an `exchanged` event, the verdict is voided with reason `exchanged`.
3. If not, the verdict is valid.

The order of the events has no effect. A later event does not restore validity. Access can record
`delivered`, `redelivered` and admission events after a void, and each of them is a fact about a dead
ticket.

**Refund precedence.** Rule 1 comes before rule 2 because the live gate checks in the same order. The
single-entry scan and the pass scan check for a refund first and for an exchange second (`postgres.go`
and `scan.go` in the Access store). Offline reconciliation does not choose a reason: it raises one
alarm for `refunded || exchanged` (`reconcile.go`).
[ADR-038](./ADR-038-refund-reversal-ticket-voiding.md) records the refund decision. Thus the page and
the gate give the same reason. Today Commerce refuses to refund an exchanged order and to exchange a refunded order, so a
history with both events comes only from the Access writers. This changes the page for that one
case. Before, the page showed the first void event in the history, so exchange then refund showed
"exchanged". Now it shows "refunded".

**Every type is classified.** A table in the Access store gives the effect of each of the nine
lifecycle types. A store smoke test (`TestEveryLifecycleTypeHasAValidityEffect`) reads the
`event_type` CHECK from a migrated database. It fails when the CHECK is not a plain `IN` list of text
literals (the shape this test supports, for which the literals are the admitted set), and when the table and the
CHECK differ in either direction. Thus a migration that adds a type, or rewrites the CHECK in another
shape, cannot pass the gate until the test and the table are updated. Because of this test, the derivation keeps the verdict for an unknown type, as the gate does.
It does not refuse the bundle. A refusal would return 500 for the whole page, valid tickets included,
and it would be stricter than the gate.

**Presentation only.** The verdict controls what a ticket view shows. The storefront shows a QR only
for a valid ticket and shows the reason for a voided ticket. It does not read event types to decide.
The verdict is not admission: the gate decides admission from Access state on its own path. The
verdict is not revocation: the bundle still carries the voided ticket's credential, and the QR image
endpoint still serves it to a holder of a live link. Later PDF and email views must use this verdict
and must not make their own rule.

**No integrity claim.** The verdict reads the history rows as they are. It adds no guarantee to the
lifecycle trail. [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) says what the trail proves
and what it does not.

**Deploy order.** Deploy Access before the storefront. The storefront refuses a bundle without a
verdict and shows "Service unavailable" (503). It does not fall back to history. An old storefront
ignores the new field and continues to work.

**Follow-ups, not fixed here.** A staff resend selects every ticket of the order, voided tickets
included, and records `redelivered` on them. The refund writer counts an exchanged ticket as
refundable. Commerce blocks this path today.

## References

- [ADR-003](./ADR-003-append-only-audit-trail.md) · [ADR-009](./ADR-009-contract-first-apis.md) · TKT-29 · TKT-43
- TKT-202 · [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) · [ADR-049](./ADR-049-customer-identity-and-storefront-sessions.md)
- TKT-355 · [ADR-038](./ADR-038-refund-reversal-ticket-voiding.md)


## TKT-360 delivery-retry digest allowance, 2026-10-10

[ADR-078](ADR-078-optional-resend-and-bounded-delivery-retries.md) accepts a future keyed content digest in Access for bounded real-mail retries. It is pseudonymous personal data, not anonymous data, and TKT-465 must include it in the data map. Access still resolves raw recipient email from Commerce at each attempt and must not persist or log that email or the message body. The adapter and digest storage are not implemented by this decision pass.
