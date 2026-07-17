# ADR-003: Append-only audit trail for money and ticket lifecycles (NF525-ready)

Date: 2026-07-12 (reworked same day: NF525 demoted from v1 goal to later-phase profile; trail extended to tickets)

## Status

Accepted

The two-trail decision stands. Its inalterability language is **scoped** by
[ADR-016](./ADR-016-checkout-recovery-state-machine.md) §Decision 7 (TKT-43), which carries the
threat model. Read the following claims in this ADR with that scope:

- **"inalterable history exactly where value and entitlements move"** (Option 3) and **"immutable,
  sequence-numbered, hash-chained … and signed"** (Decision 1) describe *application-level
  append-only, tamper-evident* behaviour. `verify-journal` detects modification and insertion, but
  **cannot detect a coordinated rollback** — suffix truncation with the head rolled back,
  whole-organizer removal, or a database restore to an older snapshot. Nothing inside the instance
  attests how long a chain should be. Closing that requires an attestation outside the database;
  the anchor is deferred to TKT-11 (see ADR-016 §Decision 7).
- **"Two append-only trails, one discipline"** (Decision) now holds, in the precise sense
  [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) (TKT-57) defines and **TKT-67
  implemented**. The **money journal** is hash-chained, HMAC-signed and verified
  (`verify-journal`, in the local gate). The **ticket lifecycle trail** is now hash-chained per
  ticket in a companion integrity table, Ed25519-signed at the head, checkpointed per organizer,
  and verified (`verify-lifecycle`, in the local gate against populated and corrupted data). Its
  defence is no longer only the append-only trigger (`lifecycle_events_immutable`,
  `services/access/internal/store/migrations/0001_tickets.sql`) — which is DDL and therefore
  removable by anyone who can alter the schema, and never was evidence.

  **This closes modification and insertion. It does not close targeted rollback**, so the
  symmetry is real but bounded, and both trails share the same bound for the same reason.

  ADR-021 deliberately does **not** give the lifecycle trail the money journal's treatment
  unchanged: "one discipline" means *tamper-evident by construction*, not *identical mechanism*.
  The journal's chain is per-organizer and serialized under a `journal_heads` row lock, which on the
  redemption path would put every turnstile for an organizer behind one lock. ADR-021 chains **per
  ticket**, signs with Ed25519 rather than HMAC, signs the **head** rather than every entry, and
  adds an **asynchronous per-organizer checkpoint** — the last of which is **scaffolding to make
  TKT-11's external anchor affordable, not a rollback control**. Each divergence is argued there.

  **Scope it precisely when citing it, in ADR-021's own words: tamper-evident against an adversary
  who cannot re-sign the chain; silent against one who can roll it back wholesale.** Modification
  and insertion are closed; targeted rollback and current-key compromise are not, and no
  in-database mechanism reaches them — ADR-016 §D7's deferral stands until TKT-11. ADR-021 §The
  trust boundary explains why, and exists because three of its own clauses got this wrong.

  Note what §Decision 2 below now depends on: redemption decisions are made *from* this trace, and
  TKT-67 made the trace tamper-evident **against modification** — so editing the answer now leaves
  evidence. Rolling it back still does not. "The lifecycle trail is tamper-evident", with no
  adversary named, remains an overstatement of what shipped.

  §Decision 2 was **amended by [ADR-025](./ADR-025-admission-events-and-offline-reconciliation.md)**
  (TKT-73): the one-redemption-per-ticket cap made multi-entry passes and offline double-admits
  unrepresentable, so "duplicate detection from the trace" was aspirational. The amended wording
  bounds the trace claim at reconciliation and names the event vocabulary (`redeemed` unique,
  `entry`/`exit` per ADR-005, `duplicate_admit` for reconciled conflicts).
- **"A GDPR erasure request … never touches the trails — inalterability and the right to erasure stop
  conflicting by construction"** (Decision 3) holds only while the pseudonym is unlinkable. The buyer
  UUID is an *unsalted* SHA-1 of the reservation ID (`services/commerce/internal/api/server.go:162`),
  so it is derived, not keyed — "destroy the key" is not available as an erasure mechanism here. The
  erasure machinery is TKT-33; that ticket owns the question.

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
2. **Ticket lifecycle trace** *(amended by [ADR-025](./ADR-025-admission-events-and-offline-reconciliation.md), TKT-73)* — every ticket/entitlement state change — including pass `entry`/`exit` and a physical offline admission that conflicts at reconciliation (`duplicate_admit`) — is an append-only lifecycle event linked to its ticket and order/journal entries. Any ticket's **authoritative reconciled history** is reconstructible from its trace; redemption, reissue and admission-policy decisions are made *from* that trace (e.g. duplicate detection), not from a mutable flag. A single-entry ticket's first authoritative admission produces a unique `redeemed`; each distinct admission already granted offline that later conflicts produces a repeatable `duplicate_admit` (single-entry only — the classification is arrival-order-independent there). A connected duplicate denied at the gate, and a transport retry, produce no new lifecycle event. Multi/count-limited admission uses repeatable `entry`/`exit` (ADR-005); pass policy conflicts are derived projections that re-evaluate as late events arrive, never minted into the trail (ADR-025 §D2). **Two bounds on this claim**: it begins at reconciliation — an occurrence a gate never synchronized is not represented — and it excludes degraded admissions under [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) §Decision 6, which are deliberately recorded only in the quarantine table (appending onto an unverified predecessor would poison the chain), so **authoritative admission history is the union of the lifecycle trace and the quarantine record — and admission *decisions* consult that union, not the trace alone**: a quarantine row denies every later **distinct occurrence** even after the chain verifies clean again (ADR-021 §D6's admit-once, occurrence-qualified by ADR-025 §D3: a lost-response retry of the occurrence that took the one degraded admission returns a distinguishable replay result — an idempotency key, never admission authorization; it completes exactly once on the device that holds the pending record and never actuates anywhere else), which the trace by itself cannot express. ADR-021's rollback and key-compromise limits continue to apply.
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
