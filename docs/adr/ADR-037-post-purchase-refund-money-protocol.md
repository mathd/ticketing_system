# ADR-037: Post-purchase refunds are their own dimension, and their own payments identity

Date: 2026-07-31

## Status

Accepted (TKT-156; decision taken under the owner-waived gates of that run, recorded on the
ticket and on epic TKT-9). First slice of TKT-9. Consumed by TKT-157 (capacity return + ticket
voiding), TKT-158 (exchange), TKT-159 (event-cancellation bulk refund).

***Amends [ADR-032](./ADR-032-stripe-behind-the-psp-port.md) on the refund-amount source only** —
see § 3. Every other clause of ADR-032, including the whole-refund resolution rules, stands
unchanged.*

## Context

A completed order can now be refunded, in whole or in part, by staff. Two pieces of existing
machinery look like they should carry it, and neither can.

**1. `orders.status` already contains `refunded`.** It was added by commerce migration 0005 for the
*recovery* runner: captured money for a checkout that never completed, returned via
`/internal/psp/refund`. Its own migration comment says *"Terminal — never claimed again"*, and
`classifyRecovered` maps it to HTTP 402 alongside `declined` and `timeout`. The checkout handler has
a dedicated `orderStatus == "refunded"` branch that journals `order.failed` and answers 402.

A post-purchase refund is the opposite fact: the checkout **succeeded**. Reusing the token would
make a successful, refunded purchase answer *"payment declined"* on a checkout replay, and would put
a completed order into a vocabulary three recovery claim queries filter on.

**2. `payment_compensations` allows exactly one refund per charge.** Its primary key is
`(organizer_id, source_idempotency_key, kind)` and, per its own migration comment, that key is
*"what makes a duplicate/concurrent void or refund converge on ONE deterministic provider
idempotency key"*. `compensationBasis` deliberately ignores caller-supplied amounts and returns the
whole `captured_amount`. `GET /internal/psp/status` reads a completed refund compensation as *this
operation now holds no money* and answers with zeroed amounts.

Every one of those properties is correct for the recovery path and wrong for a repeatable partial
refund.

## Decision

### 1. Refund state is a separate dimension on `orders`; `orders.status` is not extended

`orders.status` keeps meaning **how the checkout ended**. Refunds are reported by
`refund_status ∈ {none, partial, full}` plus `refunded_quantity` and `refunded_amount`, with one
`order_refunds` row per attempt.

Consequence, and the point: a refunded purchase is still `status='completed'`. Every existing reader
of `orders.status` — `classifyRecovered`, `claimOrder`, `markUnknown`, `markTerminalFailure`, the
checkout replay branches, `CompleteOrder`, `BackfillCompletionOutbox`, and the three recovery claim
queries — keeps behaving exactly as it did, because none of them can see a new token. That is a
property of the decision, not a claim about the implementation, and
`TestClassifyRecoveredCoversTheStatusVocabulary` still pins the vocabulary at nine.

### 2. Partial refunds get their own payments identity: `payment_refund_legs`

One row per `(organizer_id, source charge key, refund key)`; the provider idempotency key is derived
from those three (`store.RefundLegKey`, namespaced apart from `CompensationKey` so a leg and a whole
refund can never collide). `payment_compensations` and `/internal/psp/refund` are untouched.

The two paths are **mutually exclusive** against one charge: `BindRefundLeg` refuses when a refund
compensation exists, and `compensate(kind="refund")` refuses when any leg exists. They track their
ceilings in different tables, so neither may run while the other has a claim. For legitimate traffic
this is unreachable — commerce only permits a refund on a `completed` order, which recovery never
claims — so it is a guard, not a flow.

### 3. The refund amount may come from the caller — on this path only (amends ADR-032)

ADR-032 § `Refund` states the refund amount *"come[s] from the durable stored operation, never from
a caller-supplied value"*. A partial refund cannot: only commerce knows that two of three tickets are
being returned.

The amended rule: **a caller-supplied refund amount is accepted only where payments validates it
against that operation's own durable captured evidence, under the operation row lock, before any
provider call.** `BindRefundLeg` does exactly that, and the sum of bound and completed legs can never
exceed `captured_amount`. So the amount is still bounded by evidence payments holds — the caller
chooses a *subset* of money payments already knows moved, and can never name money it did not.

The recovery whole-refund path keeps the original rule verbatim.

### 4. Two ceilings, each at its owning boundary

Commerce owns the **quantity** ceiling (cumulative refunded quantity ≤ the reservation's quantity)
under the order row lock. Payments owns the **captured-money** ceiling under the `payment_operations`
row lock. Neither service can enforce the other's honestly, and a cross-service transaction is not on
offer, so both enforce their own.

**Bound-but-unresolved attempts count against both ceilings.** An unresolved refund may still settle;
releasing its allowance is precisely how a charge gets over-refunded. The cost — a stuck leg holds
allowance until it resolves — is accepted deliberately.

No lock is held across a provider call. The row locks bound a database transaction only; the PSP call
happens after the commit.

### 5. Compensating facts are per-refund, and their `occurred_at` is a stored column

The checkout helper `(*Server).fact` derives its fact id from `(order, type)` and always journals the
reservation's full total. Both are wrong for a repeatable partial fact: a second refund of the same
order would collide on the id and carry the wrong amount. Refund facts are namespaced on the refund's
own identity (`RefundFactID`) and carry the leg amount.

`occurred_at` is the refund/leg row's stable creation timestamp, **never the clock**. The fact id is
deterministic and the journal's replay dedupe compares the whole canonical fact, so a retry across
the append/complete crash boundary must rebuild byte-identical content — a fresh timestamp fails
*"fact id reused with different content"* and wedges the refund permanently. This is the same trap
ADR-032's `comp.BoundAt` already records for the whole-refund path.

### 6. What the money leg deliberately does not do

TKT-156 returns money and writes the trail. It does **not** return inventory capacity or void
tickets — that is TKT-157. The intermediate state is safe in one direction only, and that direction
is the reason it is acceptable: the seat stays sold, so the system **under-sells**. It cannot
oversell. `reservations.status` stays `completed`; the reservation records the sale that happened.

## Consequences

- Refund state is a projection maintained alongside `order_refunds`, so completion updates the leg
  and the aggregate in one transaction under the order lock. Drift is possible only through a
  direct database write.
- Payments now has two refund entry points with different contracts. The whole-refund one is
  recovery's and is frozen; the leg one is commerce's post-purchase path.
- A zero-value order cannot be refunded (`409`). No provider issues a zero-amount refund and
  `compensationAllowed` requires captured money > 0; recording a refund that no provider performed
  would fabricate a money fact.
- **Name the adversary (ADR-021).** Everything here is *honest-writer* consistency. Both ceilings,
  the mutual exclusion and the projection are enforced by application logic and constraints inside
  databases that a writer with access can edit. What they protect against is concurrency and
  crash-retry, which is the realistic way money gets returned twice — not against an adversary.
  The hash-chained journal is what makes the *record* of a refund tamper-evident, and it is
  unchanged: refunds append compensating entries and never touch the sale fact, which the
  append-only trigger enforces at the database.
