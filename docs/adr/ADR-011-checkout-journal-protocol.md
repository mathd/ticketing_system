# ADR-011: Checkout finalization and canonical money journal

Date: 2026-07-12

## Status

Accepted (approved at the TKT-28 plan gate)

## Context

Checkout crosses catalog pricing, inventory expiry, commerce orders and a PSP. There is no
distributed transaction. Capturing before confirming can strand money; confirming before capture
can strand inventory. A transport timeout is not evidence that a PSP performed no side effect.

## Decision

Commerce is the public checkout coordinator. It resolves the catalog-owned ticket type and creates
an immutable EUR price snapshot before asking inventory to hold it; browser-supplied amounts are
never trusted. Checkout uses organizer-scoped idempotency keys.

Inventory adds `finalizing`: an idempotent transition available only from a live hold. A finalizing
claim does not expire while commerce resolves payment. Each reservation has exactly one checkout;
commerce binds its key to a request fingerprint under a database advisory lock, and payments binds
the organizer/key pair to the full charge fingerprint and one stable result. Conflicting reuse is
rejected. The protocol is authorize, finalize claim,
capture, confirm. A terminal decline or fake-PSP timeout whose status proves no side effect releases
the claim. Unknown results remain finalizing for recovery. Captured claims are retried to confirm;
if confirmation is provably impossible, the PSP is voided/refunded idempotently and the order is
marked for reconciliation. This amends ADR-010's terminal-state model only by adding the guarded
pre-terminal `finalizing` state.

Payments is the sole writer of the canonical money journal. Commerce owns order workflow facts and
projections, and submits order facts idempotently to payments. Journal chains are scoped per
organizer. Version 1 canonical input is the UTF-8 concatenation of version, organizer UUID,
sequence, fact UUID, fact type, UTC RFC3339Nano timestamp, pseudonymous buyer UUID, integer amount,
currency, and payload JSON, separated by newline. The entry hash is SHA-256 of the previous hash
bytes followed by that canonical input. The signature is HMAC-SHA-256 over the entry hash with the
configured key. Entries carry a key ID; missing key material fails startup. HMAC is the v1 meaning
of “signed”; asymmetric or fiscal signatures can be added as a new canonical version.

A per-organizer chain-head row is locked `FOR UPDATE`; inserting the entry and advancing the head
are one PostgreSQL transaction. `(organizer_id, sequence)` and fact IDs are unique. Journal schemas
accept pseudonymous IDs only. Buyer name/email live in a separately deletable commerce table.

Commerce records intent and completion facts durably. `platform.commerce.order.completed` uses the
ADR-009 envelope and contains identifiers only. Recovery is coordinator-owned; unresolved captured
payments are never silently released. Recovery in this walking skeleton is retry-driven: durable
`payment_unknown` and `confirmation_pending` projections are visible through the order read API,
and an exact checkout replay resumes the idempotent protocol. Automated scheduling and real-PSP
status/compensation are required before replacing the fake PSP.

## Consequences

- Finalizing claims can consume capacity while a PSP is unavailable; recovery and operational
  visibility are required before a real PSP launch.
- One hot organizer journal serializes appends. Sharding is a later compatible optimization.
- PostgreSQL 18.4 is the actual scaffold used by Compose; the working-agreement reference to 17 is
  documentation drift, not authority to change the stack in this story.
