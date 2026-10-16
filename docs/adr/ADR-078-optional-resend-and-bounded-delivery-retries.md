# ADR-078: Optional Resend transport and bounded delivery retries

Date: 2026-10-10

## Status

Accepted as requirements for TKT-360. The adapter is not implemented. TKT-359 and the TKT-293 concurrency proof remain prerequisites. Local Compose and gates keep the capture transport.

## Context

TKT-359 separates message composition from transport. TKT-360 needs a first real transport so its provider-selection question does not block planning. ADR-012 keeps buyer PII in Commerce. A retry must preserve its message identity and content without storing recipient addresses or message bodies in Access.

## Possible solutions

Keep capture-only delivery and leave the real adapter question open, or select an optional real adapter behind the same port. Provider deduplication alone cannot establish indefinite safe retries. Resend's documented key retention is 24 hours.

## Decision

Select Resend for the first optional real adapter. Send the existing stable message identity as `Idempotency-Key`. Access stores a purpose-specific, versioned HMAC digest of recipient, locale, template version and link. It stores neither recipient nor message body. The digest is pseudonymous personal data held in Access; TKT-465 must include it in the data map. This is the narrow delivery-retry allowance to ADR-012; raw email remains forbidden in Access storage and logs. Resolve the recipient from Commerce for every attempt and regenerate with the stored template version. Retry only when the digest matches; otherwise require reconciliation.

Use a dedicated configured digest key and record its version. Stop automatic retries 23 hours after the first recorded attempt. Unresolved attempts then require operator reconciliation. A deliberate later re-send is a new operation with an explicit possible-duplicate warning. TKT-293 must prove durable local serialization of concurrent resumes.

## Consequences

The adapter has a concrete contract and a bounded retry policy. Changed delivery details and expired retry windows can require operator work. A digest protects recipient guesses from a database reader without its key; it does not protect against a database writer or a compromised service. Transport acceptance does not prove mailbox receipt.

This decision does not authorize account creation, domain setup, an external send or deployment. Configuration validation and hermetic provider tests remain TKT-360 work.

## References

- [Product choices](../product/backlog-decisions-2026-10-10.md#mail-port-and-real-sender)
- [ADR-012](ADR-012-ticket-issuance-and-qr-credentials.md)
- [ADR-050](ADR-050-transactional-mail-and-password-recovery.md)
- [Resend send API](https://resend.com/docs/api-reference/emails/send-email)
- [Resend idempotency keys](https://resend.com/docs/dashboard/emails/idempotency-keys), checked 2026-10-10
