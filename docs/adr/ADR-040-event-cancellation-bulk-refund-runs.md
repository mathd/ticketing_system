# ADR-040: A cancellation refund is a durable run with a per-order ledger, and the refund key does not know about the run

Date: 2026-08-01

## Status

Accepted (TKT-159; decision taken under the owner-waived gates of that run, recorded on the ticket
and on epic TKT-9). Fifth slice of TKT-9, and the one that closes its **COS 3**.

Builds on [ADR-037](./ADR-037-post-purchase-refund-money-protocol.md) (refund state is its own
dimension), [ADR-038](./ADR-038-refund-reversal-ticket-voiding.md) (the reversal is independent
obligations, void before capacity) and [ADR-039](./ADR-039-exchange-settles-the-difference.md)
(a multi-obligation operation is not complete until every obligation is).

## Context

Refunding one order is solved. Refunding a **book** is a different problem, and the ticket named
why: it must be resumable, it must not double-refund, one bad order must not stop the run, and the
result has to be readable afterwards. An event cancellation is also the worst blast radius in the
product — every order on the event, at once, all owed money.

Three shapes were available: a synchronous batched call, a durable run driven by a background
runner, and an event-driven consumer of a catalog cancellation.

## Decision

### 1. A durable run row plus a per-order ledger, driven by a background runner

`POST /internal/slots/{id}/cancellation-refunds` creates a run and returns; the runner does the work
in bounded batches. The ledger (`cancellation_refund_orders`, PK `(organizer_id, run_id, order_id)`)
is **both the work queue and the final report**. That is the point: "exactly one outcome per order",
resumability and no-double-refund become **one** mechanism rather than three that can disagree.

Synchronous was rejected because safe resumption rebuilds this job table behind the façade anyway,
and a caller disconnect becomes ambiguous. Event-driven was rejected because the catalog-side
cancellation transition **does not exist yet** (TKT-2/ADR-018) and is out of this ticket's scope;
coupling to it would also make "refund this book on demand" unexpressible. The runner copies
`internal/recovery`'s lifecycle rather than merging into it — checkout recovery and cancellation
reporting have different eligibility, terminal states and retention.

Reverse this only if catalog gains a transactional cancellation outbox and the policy changes from
"refund on demand" to "every cancellation starts exactly one run". The event would then become the
trigger; the durable run and ledger stay either way.

### 2. Membership is the book at a durable cutoff, and what falls outside it is COUNTED

`cutoff_at` is stamped in the same transaction as the run row, so the book is fixed before any work
starts and a replayed create answers with the original cutoff rather than silently widening it.

An order that was on the slot at the cutoff but **not yet completed** cannot be refunded by this
run. It is recorded as `incomplete_at_cutoff` on the report rather than skipped. One `COUNT` is the
difference between "a later run may owe somebody money" and a silent under-refund on the flow where
silence is most expensive.

### 3. The per-order refund idempotency key is derived from `(slot, order)` — never from the run

This is the load-bearing decision, and it is not obvious.

`BindOrderRefund`'s quantity ceiling counts **pending** refunds
(`SELECT sum(quantity) FROM order_refunds WHERE order_id=$1`, no status filter — deliberate, per
ADR-037). The order's `refunded_quantity` projection moves only in `CompleteOrderRefund`. So a run
that dies between the two leaves a `pending` row that **consumes the ceiling but is invisible to the
projection**.

With a run-scoped key, a second run would read the projection, compute the full remaining quantity,
bind a *second* refund, and trip `ErrRefundExceedsOrder` — reporting that order failed forever while
the first attempt stays stranded. With a run-independent key the second run **replays** the first
attempt, which is exactly what the single-order path does for a retry that lost its response.

Two consequences follow, and both are requirements rather than style:

- Cancellation refunds bind under **fixed** `actor`/`reason` constants. `refundFingerprint` covers
  order, quantity, actor and reason, so a second run carrying a different operator's attribution
  would conflict with its own earlier attempt. The operator's attribution lives on the run row.
- The quantity is **fixed before the first external call** (`requested_quantity`, guarded on still
  being NULL) and never recomputed. Recomputing after money moved reads a different remainder, which
  changes the fingerprint and turns a resume into a 409 against itself.

### 4. A successful outcome means EVERY obligation is discharged, and the database enforces it

`refunded` and `already_refunded` both require money returned **and** tickets voided **and** capacity
returned. An order whose money came back with a reversal outstanding is **`failed`**, with
`money_refunded: true` and the truth about which half is missing.

This is ADR-039's rule, and it is a CHECK constraint (`cancellation_refund_orders_success_is_complete`)
rather than a runner convention: a bug in the runner cannot report an under-selling cancellation as
done. The temptation to round it up is real — it makes the double-run test simpler — and it is
exactly the defect TKT-166 shipped and had to fix.

### 5. A failure is terminal **for its run**; retrying means starting another run

AC 3 asks that a failure be recorded with its reason and the run continue, which is what happens: no
attempt counter, no backoff, no parking. A permanently failing order therefore cannot prevent the run
from completing — and the report only becomes readable (`200`) when the run completes, so a run that
could never finish would make the report unreachable.

Retrying is starting another run, which §3 makes safe. This is why there is no `attempts` column: it
would have had no reader.

## Consequences

### What this does not cover, deliberately

- **Zero-price (comped) orders get no reversal at all.** `BindOrderRefund` refuses `unit <= 0`
  (`ErrRefundNoMoney`) while `reservations.unit_amount` permits `0`. So a comped ticket on a
  cancelled event keeps admitting and keeps its seat. The run records it `failed/no_captured_money`
  so it is **visible**; closing it needs a reversal path with no money leg, which is a follow-up, not
  this ticket.
- **A seated order's capacity return stays outstanding forever** — nothing associates an issued
  ticket with a seat identity, so no subset of seats can be derived (TKT-164). Such an order is a
  permanent `failed/reversal_outstanding`. That is honest, not a regression: the buyer has their
  money and the tickets are void.
- **Outstanding obligations on refunds this run did not create are reported, not repaired.** Driving
  them would need their original idempotency keys, which the refund row does not carry.
- **No catalog coupling and no RBAC.** The slot's lifecycle state is never read, and the existing
  internal-token convention stands (a wrong token answers `404`, like every other commerce internal
  route).

### Integrity language — name the adversary (ADR-021)

Everything above is **honest-writer consistency**: it holds against concurrency, crashes and retries.
It is **not tamper-evidence**. Anyone with commerce database write access can alter a run, a ledger
row or a reported outcome. The signed, append-only payments journal remains the evidence that money
moved, and ADR-021's limits on it are unchanged by this decision.
