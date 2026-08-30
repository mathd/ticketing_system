# ADR-069: The payment token is opaque to commerce, and an exchange upgrade needs a real one

Date: 2026-08-30

## Status

Accepted (TKT-301; decided under the autonomous gates of that run, recorded on the ticket).

Enforces [ADR-032](./ADR-032-stripe-behind-the-psp-port.md) rather than amending it: ADR-032 already
put provider semantics behind payments' port, and this records that a *different service* had
foreclosed it. Touches the exchange lifecycle governed by
[ADR-063](./ADR-063-exchange-reversal-reconciliation.md) and
[ADR-067](./ADR-067-wedged-exchange-operator-unwind.md), and neither changes.

## Context

ADR-032 decided that provider semantics live behind payments' PSP port. Payments implements that
faithfully: `AuthorizeRequest.PaymentToken` is documented as an opaque provider reference ("for the
fake PSP it is one of the fakepsp tokens; for Stripe it is a PaymentMethod/PaymentIntent reference"),
the fake wraps an unrecognised token in the **port-level** `psp.ErrInvalidToken`, and the charge
handler answers a provider-neutral `400 {"error": "invalid payment token"}` — with a comment saying
the wording is deliberate so a Stripe adapter can map "no such payment method" onto it. Payments
builds a real Stripe adapter when a test key is configured.

Two things in **commerce** made that unreachable.

**1. Public checkout judged the token itself.** `POST /reservations/{id}/checkout` called
`fakepsp.ValidToken`, importing `shared/go/fakepsp` — the local simulator's package — and refusing
anything outside its four values (`fake-ok`, `fake-decline`, `fake-timeout`, `fake-auth-hold`) with
its own `400 "invalid checkout"`. No Stripe PaymentMethod reference could survive it. Notably this
made commerce **stricter than commerce's own published contract**, which declares
`payment_token: {type: string, minLength: 1}` and no vocabulary at all.

**2. Exchange upgrades charged a literal.** `settleExchangeDelta`'s `delta > 0` arm submitted
`"payment_token": "fake-ok"`. The consequence is worth stating plainly rather than as a coupling
problem: **no buyer payment instrument was collected for an upgrade anywhere in the flow.** The
charge "succeeded" because the simulator accepts what it is handed. Against a real provider the
literal is not a token, so upgrades would have silently stopped charging anyone the moment one was
configured — and unlike the mail fake's equivalent gap, which is loudly logged and ADR-recorded,
this one was written down nowhere.

Both are one decision applied at two call sites. The checkout half also answers the question the
upgrade half raises: if the token is opaque and buyer-supplied, where does an upgrade's instrument
come from?

## Possible Solutions

- **Option 1 — Leave it; treat the fake vocabulary as the system's token vocabulary.**
    - Pros: no work; every existing test and smoke flow untouched.
    - Cons: ADR-032 is decided and this contradicts it. The failure is silent and arrives at the
      worst moment — the day a real key is configured, checkout refuses every real token and upgrades
      charge nobody. Neither failure is visible before then.
- **Option 2 — Commerce validates "real" tokens too (a prefix or shape rule).**
    - Pros: keeps a local guard; superficially provider-aware.
    - Cons: provider semantics under a different name. Every provider spells its references
      differently and they change; commerce would encode a second, drifting copy of a rule payments
      already owns. It also re-breaks the same boundary it is meant to fix.
- **Option 3 (chosen) — Commerce forwards the token opaquely; an upgrade carries its own
  instrument, and is refused without one.**
    - Pros: restores ADR-032's boundary exactly. Payments already refuses unknown tokens in
      provider-neutral terms, so the judge exists and simply stops being pre-empted. The upgrade gap
      becomes visible and refused instead of silently uncharged.
    - Cons: an upgrade now needs a caller to supply an instrument, and no UI collects one — so in
      practice upgrades are refused until a product slice builds that. The refusal is the honest form
      of a gap that already existed.

## Decision

**Commerce treats `payment_token` as opaque.** Checkout validates only that one is present and
non-empty — presence is not a provider question — and forwards it verbatim. Payments alone decides
whether a provider will accept it. No `shared/go/fakepsp` import remains anywhere in commerce, and no
payment-token literal remains in its production code.

**An exchange upgrade carries its own instrument.** `ExchangeCreate` gains an optional
`payment_token`, required in exactly one case: a positive delta, the only exchange shape where the
buyer owes money. Absent it, the upgrade is **refused** with `409` and
`ErrUpgradeNeedsInstrument` — never charged against a token commerce invented.

Three details of that refusal are load-bearing:

- **It is mapped by error type, not by position.** `settleExchangeDelta`'s failures otherwise answer
  `502 "exchange settlement unresolved"`, which is correct for transient provider trouble and wrong
  here: a permanent refusal answered as "unresolved" invites a retry loop against something that can
  never succeed. Equally, answering every settlement failure `409` would hide a real outage behind a
  permanent-refusal signal.
- **It happens where the instrument is needed**, inside the settlement arm — not before the target
  hold. Refusing earlier would make the charged-then-wedged state unreachable through the forward
  path, and that state is precisely what ADR-067's operator unwind exists for.
- **The token travels as a per-request argument, never on the exchange row.** A resume re-supplies it
  exactly as the original request did. Persisting a payment instrument beside an exchange would be
  storing a credential, which is a different decision with a different adversary.

Downgrades and equal exchanges are untouched: a downgrade refunds against the **original** charge by
its idempotency key and needs no new instrument, and an equal exchange moves no money.

## Consequences

- **Positive:**
    - ADR-032's provider neutrality is reachable again. A Stripe PaymentMethod reference now survives
      checkout, and the refusal a bad token gets is payments' provider-neutral one.
    - Commerce no longer contradicts its own contract, which always declared the token opaque.
    - The upgrade gap is visible. An upgrade that cannot be charged is refused, not settled against
      money that never moved.
    - `shared/go/fakepsp` is once again a payments-only detail.
- **Negative:**
    - **Exchange upgrades are refused in practice**, because nothing collects a buyer instrument for
      one. This ADR does not close that gap; it stops the gap from being silent. Collecting an
      instrument is a product slice and needs its own ticket.
    - An invalid token now fails later — in payments, after commerce has created the order and
      finalized the hold, rather than in commerce before either. Commerce's handling of payments' 400
      is unchanged by this ticket; whether that path should also release the hold and fail the order
      is a separate question this ADR deliberately does not answer.

## What this does NOT decide

Stated explicitly, because a reader could otherwise take "upgrades are handled" from the above:

- **Buyer instrument collection for exchanges.** No UI, API surface or stored-instrument mechanism is
  introduced. Until one exists, every upgrade is refused.
- **Real-PSP upgrade charging end to end.** Never exercised; the fake remains the offline provider
  and only the *judge* moved.
- **Compensation for exchanges charged against the old literal.** Rows settled before this change
  moved real money in the fake's terms only; if a deployment carries such rows, ADR-067 governs
  operator handling and this ADR adds nothing.
- **Payments' behaviour on an invalid token.** It already answers 400 and binds an operation; that
  operation's terminal state is payments' question, not commerce's, and is out of scope here.

Per [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)'s discipline: nothing here is a
tamper-evidence claim. This is a boundary and a refusal, both enforced by ordinary code that a writer
with database or deployment access can change.

## References

- TKT-301 (architecture finding R1, code finding R9 of the 2026-08-28 review)
- [ADR-032](./ADR-032-stripe-behind-the-psp-port.md) — the port and the opaque-token rule this enforces
- [ADR-039](./ADR-039-exchange-settles-the-difference.md) — exchange settlement, whose upgrade arm changes
- [ADR-063](./ADR-063-exchange-reversal-reconciliation.md), [ADR-067](./ADR-067-wedged-exchange-operator-unwind.md) — the exchange lifecycle, unchanged
- [ADR-048](./ADR-048-settlement-ledger.md) — why the delta charge still carries a settlement plan
