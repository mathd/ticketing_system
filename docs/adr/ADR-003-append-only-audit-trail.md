# ADR-003: Append-only audit trail for money and ticket lifecycles (NF525-ready)

Date: 2026-07-12 (reworked same day: NF525 demoted from v1 goal to later-phase profile; trail extended to tickets)

## Status

Accepted

The two-trail decision stands. Its **"inalterable history"** wording (Option 3) is scoped by
[ADR-016](./ADR-016-checkout-recovery-state-machine.md) §Decision 7 (TKT-43): the money journal is
append-only and tamper-*evident* at the application level, but `verify-journal` cannot detect a
coordinated rollback (suffix truncation with the head rolled back, whole-organizer removal, or a
database restore to an older snapshot) until an attestation exists outside the database. ADR-016
carries the threat model.

## Context

The owner's directive (2026-07-12): **traceability is a must** — the same audit-trail thinking must apply to money flows *and* to tickets. The French market and its anti-VAT-fraud regime (art. 286-I-3° bis CGI, commonly met via NF525 certification, requiring inalterability/security/conservation/archiving of receipts) is **not an immediate goal**, but it is far cheaper to design for from the start than to retrofit: NF525 changes how internal flows are tracked, and bolting inalterability onto a mutable store later is effectively a rewrite. The decision is needed before the first order is stored (US-004). *Note: the ISCA characterization is inferred from how NF525 works, not verified against the standard's text; verification happens when the compliance profile is prioritized (TKT-11).*

## Possible Solutions

- **Option 1 — Mutable CRUD now, traceability/compliance later:**
    - Pros: fastest walking skeleton.
    - Cons: guarantees a money-path rewrite when traceability or NF525 lands; history reconstructed from logs is not an audit trail. Rejected.
- **Option 2 — Full event sourcing across all services:**
    - Pros: traceability everywhere by construction.
    - Cons: heavy modeling tax on domains that don't need it (catalog, seat maps); slows every epic.
- **Option 3 — Append-only audit trail on money and ticket/entitlement paths only (chosen):**
    - Pros: inalterable history exactly where value and entitlements move; CRUD ergonomics everywhere else; NF525 becomes an additive profile.
    - Cons: two persistence idioms in the codebase; projections add moving parts.

## Decision

Two append-only trails, one discipline:

1. **Money journal** — every money-relevant record (orders, payments, refunds, exchanges, POS receipts, cashless top-ups/spends) is immutable, sequence-numbered, hash-chained (each record embeds the previous record's hash) and signed. Corrections are compensating entries, never updates. Read models are projections. A `verify-journal` tool checks chain integrity end-to-end and runs in the local gate from US-004 onward.
2. **Ticket lifecycle trace** — every ticket/entitlement state change (issued, delivered, transferred, resold, exchanged, redeemed, reissued, invalidated, pass entry) is an append-only lifecycle event linked to its order/journal entries. Any ticket's full history is reconstructible from its trace alone; redemption and reissue decisions are made *from* that trace (e.g. duplicate detection), not from a mutable flag.
3. **Trails are pseudonymous; PII is erasable** — journal records and lifecycle events reference customers by pseudonymous ID only; PII (names, emails, addresses) lives in a separate, erasable store. A GDPR erasure request destroys the PII record (or its key) and never touches the trails — inalterability and the right to erasure stop conflicting by construction. "No raw PII in any append-only store" is an enforced invariant from US-004; the erasure/retention machinery itself is a later epic (TKT-33).
4. **NF525 is a later-phase profile, not a v1 requirement** — period closures (Z/daily/monthly/fiscal-year), fiscal archives and audit export (TKT-11) must layer on the money journal **without schema or flow changes** when the French market is prioritized. That constraint is what the journal design is validated against; certification itself is out of scope.

## Consequences

- **Positive:**
    - Money and tickets get "where did this come from, who touched it, when" answered by construction; disputes, fraud investigation, transfers and resale get a natural substrate.
    - NF525 (and similar fiscal regimes) becomes additive; no rewrite when the French market lands.
- **Negative:**
    - Every money/ticket feature pays the trail discipline (no quick UPDATEs; compensating entries; lifecycle events on every transition) — slower to write, easier to trust.
    - Projection lag/consistency between trails and read models must be managed and tested.

## References

- [PRD](../product/prd-v1.md) (US-004…US-006, TKT-11) · [ADR-002](./ADR-002-services-from-day-one.md)
