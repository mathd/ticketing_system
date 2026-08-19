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

### 2. The cutoff bounds the RESERVATION set, not order completion

`cutoff_at` is stamped in the same transaction as the run row, and it bounds the set of
**reservations** the run pages through. That is what makes the book finite and the run resumable: the
keyset cursor walks a set that cannot grow underneath it.

It deliberately does **not** bound order completion. Whether an order is `completed` is necessarily
sampled when its page is read, because there is no completion timestamp to filter on. Two consequences,
and both are the honest ones rather than accidents:

- an order still in flight at the cutoff but completed before its page is read **is** refunded — which
  is what an operator cancelling an event wants;
- an order completed *after* its page has been passed is **not** in this run. It is counted as
  `incomplete_at_enumeration` on the report — named for enumeration, not the cutoff, because that is
  when the decision was actually taken.

One `COUNT` is the difference between "a later run may owe somebody money" and a silent under-refund
on the flow where silence is most expensive. Membership is therefore reproducible only up to runner
timing; the count is what makes that visible instead of hidden.

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

### 5. A DEFINITE refusal is terminal at once; an AMBIGUOUS failure is retried, bounded

The two are not the same failure and must not share a verdict.

A **definite** refusal — the order is not refundable, there is no captured money — is terminal on
the first attempt. Retrying it only burns the book.

A **moved ceiling** is not a refusal even though the ceiling raised it: a staff refund landed between
the runner reading the remainder and binding it, so the fixed quantity is stale. That quantity is
cleared and the row retried, which is the only way the remaining tickets are ever refunded. (The
first version of this fix cleared the quantity and then finalized anyway, so nothing recomputed it —
it stranded exactly the tickets it was meant to rescue.)

An **ambiguous** failure is one where the money may already have moved: a provider timeout, an
unavailable journal, a completion that did not persist. Finalizing those terminally is what leaves
money gone with the tickets still valid and *nothing* driving the reversal — the run then completes
over the top of it and only a new run would ever pick it up. So they are retried within the run, up
to `maxAttempts`, and the row's **lease is deliberately left in place** rather than released: the
lease is the backoff. Releasing it lets the very next claim in the same pass re-drive an unavailable
downstream and burn the whole budget in a tight loop. (Both halves of this were wrong in the first
implementation and were caught in review.)

An outstanding **reversal** is retryable when it belongs to **this run's own refund** — replaying that
refund re-drives the obligation, so a retry can genuinely discharge it. One belonging to a refund the
run does **not** own is terminal at once: repairing it would need that refund's own idempotency key,
which the ledger row does not carry, so retrying would only re-read the same state until the budget
ran out.

Attempts are charged at **claim** time, and **refunded when a claim is released undriven** (a
shutdown, a lapsed lease) — the same thing `AbandonRecoveryClaim` does. Without the refund a row can
arrive at its first real ambiguous failure with its budget already spent on work that never happened.

The bound is what keeps §5 compatible with the report: a run only becomes readable (`200`) once every
row is terminal, so an unbounded retry would make the report unreachable. This is why the `attempts`
column exists; an earlier draft dropped it on the grounds that nothing read it, which was true only
while ambiguous failures were (wrongly) terminal.

Once attempts are spent the row is finalized `failed` with its last reason, and retrying then means
starting another run — which §3 makes safe.

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
  *Bounded to this runner by [ADR-062](./ADR-062-refund-reversal-reconciliation.md) (TKT-163): it is
  a limitation of a **cancellation run**, not a system-wide prohibition on repairing a foreign
  refund's reversal. The reason it holds here is that a ledger row cannot re-derive the refund it
  does not own; a dedicated reconciler claims the `order_refunds` row itself and calls
  `DriveReversal` with it, which needs no idempotency key at all. Such obligations are therefore now
  repaired — just not by this runner, and a run that reports `failed/reversal_outstanding` is still
  reporting the truth as of when it looked.*
- **`already_refunded` is decided from whether a PREVIOUS run had already refunded the order**, not
  from the refund unit's replay flag. Replay cannot tell a second run apart from this run resuming
  after a crash, and would mis-attribute both directions. The answer is recorded on the ledger row
  when the quantity is fixed, so a resumed attempt attributes the outcome the way the first one would
  have. **Two attribution residues remain, both accepted:** if run A moved provider money but ended
  ambiguously and run B completes it, B reports the refund as its own; and two runner instances that
  both read before either binds will both record "no prior run". In every case both reports agree the
  money came back and every obligation is discharged, which is the property that matters — the
  disagreement is only over which run gets the credit.
- **A verdict can still be committed microseconds after the context ends.** The check and the
  write are not atomic and no check-then-write can be. It is acceptable because of *which* failures
  survive both checks: one caused by the shutdown surfaces as a context error and is handled before
  the write is reached, so what gets through is a genuine failure that deserves committing.
- **A claimant whose lease lapsed can still be in flight while its successor works.** Both derive the
  same refund key (§3), so they converge on one refund and cannot double-refund; the loser's verdict
  is dropped by the claim fence. What is not guaranteed is which of the two verdicts is recorded.
- **The `cancel:` idempotency-key prefix is reserved.** The staff refund endpoint rejects it. A staff
  refund under a derived key would produce the same refund identity with a different request
  fingerprint, and every cancellation run would then report that order failed forever — including one
  whose staff refund had fully succeeded.
- **No catalog coupling and no RBAC.** The slot's lifecycle state is never read, and the existing
  internal-token convention stands (a wrong token answers `404`, like every other commerce internal
  route).

### Integrity language — name the adversary (ADR-021)

Everything above is **honest-writer consistency**: it holds against concurrency, crashes and retries.
It is **not tamper-evidence**. Anyone with commerce database write access can alter a run, a ledger
row or a reported outcome. The signed, append-only payments journal remains the evidence that money
moved, and ADR-021's limits on it are unchanged by this decision.
