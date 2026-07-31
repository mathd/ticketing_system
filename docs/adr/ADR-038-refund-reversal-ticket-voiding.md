# ADR-038: A refund's reversal is a set of independent obligations, and voiding tickets is the first

Date: 2026-07-31

## Status

Accepted (TKT-157; decision taken under the owner-waived gates of that run, recorded on the ticket
and on epic TKT-9). Second slice of TKT-9. **Extended by TKT-161** with the capacity-return half.
Consumed by TKT-158 (exchange) and TKT-159 (event-cancellation bulk refund).

Builds on [ADR-037](./ADR-037-post-purchase-refund-money-protocol.md), which returned the money and
deliberately left the seat sold and the ticket valid.

## Context

ADR-037 §6 named this ticket's work and named why stopping short of it was safe: money came back,
the seat stayed sold, so the system **under-sold** and could not oversell. That is now the shape of
the whole reversal, not a temporary state.

A refund has two downstream obligations — void the buyer's tickets, and give the capacity back —
and they are independent. Neither needs the other, they touch different services, and each can fail
on its own. The original ticket carried both; the plan established the pair could not fit one
reviewable PR and the split runs **by obligation**, so each half is end-to-end and demoable.

## Decision

### 1. Voiding goes first, and that ordering is a ticket dependency, not a runtime rule

Freeing capacity while the original ticket still admits is the one ordering that can **oversell**:
the seat is resold while the refunded holder can still walk through the gate. Voiding first can only
under-sell. Making it a dependency between tickets rather than a sequencing rule inside a
distributed flow means there is no window in which the wrong order is even expressible.

### 2. `refunded` is a lifecycle event, appended through the normal path

It joins the once-per-ticket set alongside `issued`/`delivered`/`redeemed`, enforced by the partial
unique index. It is appended through `store.appendLifecycle` like every other lifecycle event — the
verifier asserts one-to-one coverage between events and integrity rows, so a direct INSERT reads as
tampering and fails `access verify-lifecycle` in the gate (ADR-021).

**No canonical-version migration.** `event_type` is already a canonical field, so a new *value*
changes no signed bytes. A new canonical *field* would.

### 3. Selection is deterministic; stability comes from the event id, not a second table

Which q of Q tickets get voided: **the lowest ticket ids ascending, among those not yet refunded.**
Deterministic, and it needs no issuance ordinal that does not exist.

Deterministic is not sufficient. Recomputed after a first pass, "the lowest unrefunded ids" selects
the **next** q — a replayed refund would void the order twice over. So the choice has to be
remembered.

It is remembered by the lifecycle event id, derived from `(refund_id, ticket_id)`. An event under
that id can only have been written by that refund against that ticket, so a replay finds its own
work and answers with it.

That covers *which tickets*, and it took an adversarial review to notice it is not the whole
binding. It says nothing about which **order** a refund id belongs to: presented against a different
order, the same refund id derives different event ids, finds nothing of its own, and voids a second
batch. `ticket_refund_batches` therefore exists — but for that one fact only, `(refund → order,
quantity)`. It deliberately does **not** store the ticket ids, which the trail already holds and
which a second copy could only diverge from.

Concurrent refunds of one order lock **every** ticket of the order in id order before selecting.
Two refunds would otherwise read the same unrefunded set; locking in a total order also means they
can only queue, never deadlock.

### 4. The refusal is its own verdict, checked before anything that can fail open

A scan of a refunded ticket answers `DecisionRefunded`, on both the single-entry and pass paths.

**It is checked before `verifyTicketChain`, and the ordering is the decision.** A chain that does
not verify takes ADR-021 §D6's degraded posture and **admits once** — deliberately, because a
verification failure is likelier our own bug than an attacker, and denying a real customer at a live
turnstile is the worse failure. But a refunded holder is not a real customer, and a refund check
placed after that branch would let them through exactly once. Commercial validity is not a
chain-health question and must not be decided downstream of one.

Not folded into `already_redeemed`, which would assert an admission that never happened, nor into an
integrity verdict, which describes chain health. `ScanRejected.reason` is an unconstrained string by
design, so the new verdict reaches the wire without a contract change.

**Offline reconciliation is out of scope, and that bounds the claim.** Reconciliation records
occurrences that already physically happened; refusing to record one would falsify the trail rather
than protect a gate — the person is already inside. So say it precisely: **a refunded ticket is
refused by a LIVE gate. An offline scanner will still admit it, and the sync will faithfully record
that it did.**

The missing capability is revocation propagation to offline scanners, which is a scanner/offline
distribution feature (ADR-025's territory), not something this path can fix by denying a fact.
Tracked as **TKT-162**. Raised by the TKT-157 adversarial review, which was right that the
unqualified claim "a refunded ticket stops working at the gate" was too broad.

### 5. "Not enough tickets" is *not yet*, never *nothing to void*

Issuance is asynchronous — commerce's outbox → JetStream → access — so a prompt refund can genuinely
outrun it. Access answers **503** and voids nothing. Reading that as "voided zero tickets" would
report a discharged obligation nobody discharged, and the refund would look complete forever.

### 6. Reversal progress is nullable timestamps, and the money path never fails on it

`order_refunds.tickets_voided_at`; TKT-161 adds `capacity_returned_at` beside it. Two independent
obligations, each either discharged or not, in no required order. A `reversal_status` CHECK would
have to be migrated by TKT-161 to admit its own terminal value; a second nullable column is purely
additive.

**Not backfilled.** Refunds written before this migration returned money with their tickets still
valid; stamping them would assert a voiding that never happened.

A voiding failure **never fails the refund request**. The money has already moved and the refund row
is durable, so the honest answer is a successful refund reporting `tickets_voided: false` — and the
field is **required** in the response, not omitted, because a caller who cannot tell "voided" from
"we did not get to it" will assume the first.

### 7. No recovery runner

Retry is a **replay of the same refund idempotency key**, which resumes whatever is outstanding —
the path ADR-037 already built. **Nothing retries on its own**: an access outage, or a refund that
outruns issuance, leaves the tickets valid until something replays the refund. That is the accepted
cost, stated rather than papered over, and it is tracked as **TKT-163**. A leased runner with claim tokens, attempts, backoff and fenced
writes was designed and rejected: the requirement is that outstanding work be *visible and
retryable*, not automatically retried, and a second recovery state machine in commerce is a large
cost against a requirement nobody stated. Adding one later is additive; shipping one now would
commit schema and a service dependency that TKT-158 and TKT-159 would have to reason about.

`ACCESS_URL` is optional in commerce for the same reason: without it a refund still returns the
money and leaves voiding outstanding. Degrading beats refusing to start, for an obligation
discharged after the money has already moved.

## Consequences

- Between TKT-157 and TKT-161 a refund voids tickets without returning capacity. Under-sells only.
- A refunded ticket cannot be admitted even when its integrity chain is broken. That is a
  deliberate narrowing of §D6's fail-open, and it is narrow: it applies only to tickets a refund has
  voided.
- Access has an inbound internal HTTP surface for the first time. It previously used
  `X-Internal-Token` only outbound, from its consumer, so the auth path is new and fails closed to
  404 like commerce's staff operations.
- **Name the adversary (ADR-021).** Everything here is *honest-writer* consistency. The refusal is a
  row read from a database; a writer with access to that database can delete the `refunded` event
  and re-admit the holder. What the chain provides is that such a deletion is **evident**, not that
  it is prevented — modification and insertion are closed, targeted rollback is not (TKT-11's, still
  open). The denial protects against a refunded buyer walking up to a gate, which is the realistic
  case.
