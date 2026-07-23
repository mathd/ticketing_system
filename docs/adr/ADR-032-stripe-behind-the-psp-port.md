# ADR-032: Stripe behind a provider-agnostic PSP port

Date: 2026-07-22

## Status

Accepted

Completes the PSP-port, compensation and keyring surface deferred by ADR-016 §Decision 3/4/8
(TKT-56). ADR-016 fixed the recovery state machine and named the port's obligations; this ADR
fixes the port's *contract* and the Stripe-specific mappings behind it. It does not supersede
ADR-016 or ADR-011 — it satisfies preconditions they left open.

## Context

TKT-43 shipped the checkout recovery state machine (ADR-016) against a **fake PSP** whose
authorize/capture/decline/timeout behaviour is an inline `switch` in the payments charge handler
(`services/payments/internal/api/server.go`). ADR-016 deferred everything that only makes sense
against a real processor, because "provider reference identity, status semantics, void-vs-refund
rules and the error taxonomy are all provider-specific, so an abstraction built over the fake PSP
would be rewritten on contact with a real one."

The processor was chosen on 2026-07-21: **Stripe, in test mode**, behind a provider-agnostic port,
with the fake PSP retained as a second implementation for the offline gate and local development.

The decision this ADR records is **the shape of the port contract** — the seam every provider
implements and that the money journal and recovery state machine are written against — plus the
concrete Stripe mappings. Constraints:

- **Money is integer minor units + ISO currency; floats are banned on money paths.**
- **The local gate (`make check`) must run offline and deterministically** — CI holds no Stripe
  key. The Stripe implementation is therefore tested against recorded response fixtures through an
  injected HTTP client, never live Stripe; live `sk_test_` verification is a manual, out-of-gate
  step.
- **The journal is the canonical money record** (ADR-011): HMAC-SHA256 canonical v1, per-organizer
  hash chain, sole-writer payments. Compensations are *appended facts*, never mutations (ADR-003 §1,
  ADR-016 §Decision 4).
- **Name the adversary** (ADR-021): the journal's integrity is honest-writer + rotation, not
  verify-without-sign-power. This ADR must not claim otherwise.

## Possible Solutions

- **Option 1 — Call Stripe directly from commerce recovery:**
    - Pros: fewer hops for the recovery runner.
    - Cons: commerce would hold PSP credentials and learn Stripe's status vocabulary; today commerce
      never persists the payment token and payments retains only a one-way fingerprint
      (`server.go:149`). This inverts the existing layering and spreads secrets.
- **Option 2 — A provider-agnostic port inside payments, exposed to commerce as provider-neutral
  `/internal` operations (chosen):**
    - Pros: payments stays the sole PSP + journal boundary; commerce receives only normalized states
      and replay metadata; the fake and Stripe implementations are swappable by config; the offline
      gate keeps working via the fake.
    - Cons: more endpoints; the port must be designed to hide Stripe semantics without erasing them
      (manual capture, PaymentIntent status, charge/refund identity still need explicit mappings).
- **Option 3 — Migrate journal signing to Ed25519 as part of this work:**
    - Pros: would give verify-without-sign-power.
    - Cons: `journal_entries.signature` is `bytea` fixed at 32 bytes (`0001_journal.sql`); Ed25519 is
      64 bytes → a schema + canonical-version migration, and a trust-model change ADR-011 reserves for
      a new canonical version. That is TKT-11's current-key-compromise territory, not this ticket's.

## Decision

**We adopt Option 2.** A single provider-agnostic `PSP` port
(`services/payments/internal/psp`) normalizes every provider into one vocabulary:

- **Operations:** `Authorize`, `Capture`, `Void`, `Refund`, `Status`.
- **Normalized outcomes:** `authorized`, `captured`, `declined`, `timeout` (a status-proven
  no-side-effect), and `unknown` (a transport failure whose side effect is genuinely undetermined).
  *Amended (TKT-114/S2):* two compensation outcomes added — `voided` (a successful void: the hold is
  released, nothing moved, terminal-no-side-effect) and `refunded` (a successful refund: money moved
  and came back — a real side effect, so **not** terminal-no-side-effect; recovery must never read a
  refund as "no side effect").
  A `Result` additionally carries `Captured`, `Authorized`, `TerminalNoSideEffect` and an opaque
  `ProviderRef`. Recovery may release a claim on `TerminalNoSideEffect`; it must **never** release on
  `unknown` (ADR-016 §Decision 3).
- **Two implementations:** the **fake PSP** (deterministic, offline, no `ProviderRef`) and the
  **Stripe test-mode adapter** (real HTTP behind an injected client/base URL). Payments selects the
  adapter by config: a configured test-mode `STRIPE_SECRET_KEY` selects Stripe, otherwise the fake —
  so the gate runs offline by default and a live-mode key is rejected at startup.
- **The port never touches the journal.** The payments handler derives the journal fact type, the
  operation status and the HTTP code from the normalized `Result`; the journal and its keyring remain
  the store's concern. This keeps the port provider-agnostic and the money journal provider-blind.

**Provider reference identity lives on the operation, not in the journal.** Stripe `pi_…`/`ch_…`/
`re_…` values, captured-vs-authorized amounts and the durable original request are stored by extending
`payment_operations` (and a `payment_compensations` child table keyed by source operation + kind);
the journal payload stays restricted to `order_id` (`store.go` payload guard). Provider IDs are
mutable operational evidence, not canonical business facts.

**Compensation is journalled as facts.** Void appends `payment.voided`; refund appends
`payment.refunded` — appended entries reversing an authorize/capture, never mutations (ADR-016
§Decision 4). These fact types are added to the allowlist in Slice 1; nothing emits them until the
compensation slice.

**Journal key rotation uses a keyring over the existing HMAC-SHA256**, not Ed25519: sign with the
active key, verify against historical keys, validate at startup, and retire a key only when no
retained entry references it. This satisfies ADR-016 §Decision 8 (rotation) with zero schema change.
The integrity claim is bounded precisely: **modification and insertion by a database writer who does
not hold the HMAC keys are evident; a holder of any historical key can forge history under it, and
targeted rollback and current-key compromise remain out of scope (TKT-11).**

**Stripe mappings** (pinned so the adapter is not re-derived per reader):

- `Authorize`: confirm a PaymentIntent with **manual capture**, integer minor-unit amount, ISO
  currency (EUR-pinned for now, per ADR-011's immutable-EUR snapshot); retain the PaymentIntent
  reference and any resulting charge id as `ProviderRef`.
- `Capture`: capture that authorization under a stable, operation-derived idempotency key.
- `Void`: cancel an uncaptured authorization; append `payment.voided` only after the provider result
  is durable.
- `Refund`: refund the captured charge under a stable **compensation** idempotency key; the refund
  amount/currency come from the **durable stored operation**, never from a caller-supplied value;
  append `payment.refunded`.
- `Status`: retrieve the known provider object. If the process timed out **before** persisting the
  provider reference, replay the identical creation request under the **same** Stripe idempotency key
  and the persisted request body — not a freshly generated key. A transport timeout is `unknown`;
  only a retrieved/replayed provider result proving no authorization or capture maps to
  `terminal_no_side_effect`.
- Unknown or future Stripe statuses **fail closed** as `unknown`/error — never silently interpreted
  as a decline.

**A distinct terminal-outcome vocabulary for "PSP status proved no side effect."** Commerce's
`terminal_outcome` today is `declined | timeout | not_attempted`. A Stripe-status-proven no-side-effect
resolution is none of these; recording it as `declined`/`timeout` would make the column lie to its
reader. The commerce compensation slice adds a distinct value (`no_side_effect`) to the
`terminal_outcome` CHECK, alongside adding `refunded` to `orders_status_check`.

## Consequences

- **Positive:**
    - Payments remains the single PSP + journal boundary; commerce never learns Stripe's dialect and
      never holds Stripe credentials.
    - The offline gate is preserved: the fake PSP is the default, and the Stripe adapter is tested
      against hand-written literal fixtures through an injected client.
    - The journal format and 32-byte signature are untouched; rotation is additive.
    - Compensation is audit-clean by construction (appended facts, DB trigger already forbids
      mutation).
- **Negative:**
    - More `/internal` endpoints and a durable operation/compensation schema to maintain.
    - Recorded Stripe fixtures prove we parse Stripe's *format*, not that Stripe still returns it — a
      real API change is invisible to the gate until a human re-verifies with a live key.
    - HMAC historical keys are effectively non-retirable while their journal era is retained — an
      operational burden, and removing one intentionally makes that era unverifiable.
    - Longer recovery call chains (status + capture + refund) exceed the current three-call bound, so
      `MaxCallsPerOrder` and `LeaseFor` (`services/commerce/internal/recovery/runner.go`) must grow
      together or a lease can lapse mid-external-action.

### Delivery slices (TKT-56)

This ADR governs the whole ticket; the code lands in dependency-ordered slices:

1. **Slice 1 (this ADR + the port seam):** `psp` port contract + fake adapter refactored out of the
   inline charge switch + `payment.voided`/`payment.refunded` fact types. No Stripe, no keyring, no
   recovery change yet.
2. **Slice 2:** Stripe test-mode adapter + `/internal/psp/*` endpoints + OpenAPI + durable
   operation/compensation schema.
3. **Slice 3:** commerce recovery — resolve `payment_unknown` via PSP status, refund
   `reconciliation_required` (re-deriving from evidence: refund only when the operation proves
   captured money), the CHECK-constraint migration, and lease sizing.
4. **Slice 4:** HMAC journal keyring + PostgreSQL fault-injection matrix (concurrent appends,
   conflicting replay, tampering, head consistency, rotation, restarts, crash boundaries).

## References

- TKT-56
- [ADR-016: Checkout recovery state machine](ADR-016-checkout-recovery-state-machine.md) — §Decision
  3 (normalized terminal-no-side-effect, lookup-vs-status split), §Decision 4 (compensation as facts),
  §Decision 8 (keyring)
- [ADR-011: Checkout finalization and canonical money journal](ADR-011-checkout-journal-protocol.md)
- [ADR-003: Append-only audit trail](ADR-003-append-only-audit-trail.md) — §1 compensating entries
- [ADR-021: Ticket lifecycle trail integrity](ADR-021-ticket-lifecycle-trail-integrity.md) — naming
  the adversary; the ACCESS keyring (`services/access/internal/lifecycle/keys.go`) is the precedent
  for Slice 4
- [ADR-022: Out-of-band service migrations](ADR-022-out-of-band-service-migrations.md) — the Slice
  2/3 migrations run via the `migrate` subcommand, not at server startup
