# ADR-011: Checkout finalization and canonical money journal

Date: 2026-07-12

## Status

Accepted (approved at the TKT-28 plan gate)

The protocol below stands unchanged. Its **recovery** story — retry-driven, with automated scheduling
and real-PSP status/compensation named as prerequisites — is amended by
[ADR-016](./ADR-016-checkout-recovery-state-machine.md) (TKT-43), which also bounds the journal's
tamper-detection claim.

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
- PostgreSQL 18.4 is the scaffold used by Compose and the version accepted by ADR-007. TKT-46
  aligned the working agreement with that existing authority.

## Amendment (2026-10-04, TKT-285) — a zero-total checkout journals no charge, and recovery can complete from `held`

This amendment adds two exceptions to the protocol above. The rest of it is unchanged.

**1. No charge for a zero total.** A checkout whose persisted gross `reservations.total_amount` is
zero does not call payments `POST /internal/charges`. No payment operation exists for the order, and
no `payment.*` fact is written. The order still follows the protocol in every other way: commerce
records `order.created` and `order.completed` in `order_facts` with deterministic fact IDs, and
submits both to payments `/internal/facts`. Both facts carry amount 0 and the order currency. The
journal accepts them because neither type is a money-moving type (`moneyMovingTypes`), so the
positive-amount rule does not apply. Skipping the charge does not make checkout independent of
payments: the fact submission still needs it.

The key is the **gross** total, which includes passed-on fees. A zero face value with a passed-on fee
has a positive total and is charged.

**2. Recovery can confirm from `held`.** The recovery runner completes a zero-total order that
crashed after its `order.created` fact. If the crash came before the inventory finalize, the claim is
still `held`, and the runner confirms it directly. Inventory accepts a confirm from both `held` and
`finalizing`. Only the `finalizing` history entry is missing for such an order. The runner replays
`order.created`, confirms, submits `order.completed`, and then runs the shared completion
transaction. Recovery did not submit `order.completed` for paid orders before this change, and it
still does not (see ADR-016, TKT-285 amendment).

**What this does not claim.** An absorbed fee on a zero-gross order is **not** recorded by this
change. There is no capture, so there is no capture ledger (ADR-048 ties a ledger to
`payment.captured`), and recording one would invent a payment fact (ADR-003). It does **not** follow
that nothing is owed: ADR-048 allows a negative organizer line when absorbed fees exceed face value,
so a comp can still owe a platform fee. Where that obligation lives is open and tracked as TKT-502.
This is ordinary application code. It is not a tamper-evidence claim (ADR-021).
