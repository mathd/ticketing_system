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
  **→ CLOSED by the TKT-171 amendment below (2026-08-27): a comped order is now VOIDED. This
  paragraph is retained as the record of what was accepted and for how long.**
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

## Amendment (2026-08-27, TKT-171) — a comped order is voided, not failed

The consequence above accepted that a **zero-price (comped) order got no reversal at all**: its
tickets kept admitting and its seat stayed sold on a cancelled event, reported as
`failed/no_captured_money`. That acceptance is withdrawn. Comped orders are now reversed by a
**void**, and the run drives it.

**Why a void and not a zero-amount refund.** This was the ticket's one open decision and the owner
took it: a comped reversal is **not a refund**. The alternative — relaxing `BindOrderRefund` to accept
`unit == 0` — is the least new code and the wrong shape, because it would record a money fact that did
not occur ([ADR-003](./ADR-003-append-only-audit-trail.md): the journal records what happened, not
what didn't). The database already agreed: `order_refunds` carries `CHECK (unit_amount > 0)` and
`CHECK (amount > 0)`, so a void could not have been written there without relaxing a money constraint.
`ErrRefundNoMoney` is unchanged, and a test pins that it still refuses, so a later ticket cannot take
the shortcut quietly.

**What a void is.** A row in `order_voids` (commerce migration 0025) carrying the order, a quantity
taken from the reservation, staff attribution, and the two progress markers. There is deliberately no
`unit_amount`, no `amount`, no `currency` and no `payment_fact_id` — the invariant *a void moves
tickets and capacity and never money* is enforced by the absence of columns that could record one,
rather than by a convention someone has to remember.

**Identity: derived from the ORDER, not from the request key.** Both downstream legs are keyed on the
field they call `refund_id`, which is their idempotency/correlation key and not a claim that money
moved — access derives a deterministic ticket selection and event id from it, inventory its claim
history, and neither writes a money fact. `store.VoidID(organizer, order)` is therefore stable across a
staff retry, a cancellation-run retry and a process restart, all of which arrive with **different**
request keys. Deriving from the key would have given each its own downstream operation and reversed the
order more than once. Its namespace is distinct from `RefundID`'s, so a void can never replay as a
refund.

**The ordering is unchanged and now has ONE implementation.** `refunds.Service.DriveReversal` was
generalized into `driveOrderedReversal`, with the refund and the void as two adapters over it. Tickets
are voided **before** capacity is returned ([ADR-038](./ADR-038-refund-reversal-ticket-voiding.md) §1):
freeing the seat while the original ticket still admits is the one sequence that can oversell. Copying
the driver for the void would have copied that guarantee into a second place that can drift. The
capacity leg waits on the **recorded** marker, not on the call's success — so a voiding that happened
but was not recorded still blocks the seat's return, and a replay resumes it.

**Outcome vocabulary.** A comped reversal is reported `voided`, with `MoneyRefunded:false` and the
other two flags true. It is **not** reported `refunded`: that would tell an operator money went back to
a buyer when none did. This is what the three independent flags on `CancellationOutcome` were for.

**Eligibility is NOTHING WAS CAPTURED, not "the face was zero".** `reservations.unit_amount` is the
FACE value, and `total = face + passed_on` (commerce migration 0014) — so a ticket priced at 0 carrying
a fixed **passed-on fee** has `unit_amount = 0` and a real, captured `total_amount`. A void that tested
the face alone would return that order's tickets and seat and **keep the buyer's fee**. Both numbers are
therefore read under the order row lock, and a void requires both to be zero.

**What such an order does instead — the owner's decision of 2026-08-27.** It is **refused**: by the
void (money was captured) and by the refund (`ErrRefundNoMoney`, since its unit is 0). It therefore
reports `failed/no_captured_money` and stays visible. Option 1 of three, and the most reversible: a
void that returned fees would be a money path, which is precisely what a void exists to avoid, and
keeping the fees would leave the buyer paying for an event that will not happen. **What a
comped-with-fees sale should do on cancellation is a real open question and is deliberately not
answered here** — a narrower, honestly-reported gap is better than reversing it wrongly.

**`no_captured_money` still exists, and now means something narrower.** The branch condition was
`UnitAmount <= 0`; it is now `UnitAmount == 0 && TotalAmount == 0` for the void, with everything else
falling through. Three shapes reach the failure: a zero face with captured fees (above), a negative
unit amount (corrupt data, unreachable through any supported write), and a genuine no-money order that
is not comped. Each is asserted, so the split is a decision rather than an accident of a comparison
operator.

**Access keeps its `refunded` lifecycle event, deliberately.** A void does not introduce a `voided`
event type. `refunded` is a lifecycle event **type** with a golden canonical form — changing the
vocabulary is a canonical-version migration, and the token is load-bearing elsewhere (the exchange path
treats `refunded` and `exchanged` as the entitlement-revocation set). Its meaning in access is *this
ticket's entitlement was revoked*, which is exactly true of a comped void. **The money distinction
lives in commerce, where the money is.** The naming mismatch is accepted and recorded here so it reads
as a decision rather than an oversight.

**A void and an exchange exclude each other, in BOTH directions.** `BindOrderVoid` refuses an order
that already has an exchange, and `BindOrderExchange` refuses one that already has a void. The second
half is not a restatement of the first: a void leaves the order `completed` and writes no
`order_refunds` row, so the exchange path's existing refund-count guard cannot see it. Without it,
void-then-exchange binds cleanly and the order carries two independent reversals with different
downstream operation ids — duplicate capacity returns, and an exchange of tickets whose source was
already voided. Both directions take the same order row lock, so exactly one can win a race.

**A second caller ADOPTS an existing void rather than conflicting with it.** Unlike a refund, a void has
no parameters to disagree about: its identity is the order and its quantity comes from the reservation.
Actor and reason are a label on the operation, not part of it. So when staff void an order by hand and
the event is then cancelled, the run reaches the same id and drives the same operation, keeping the
first binder's attribution — they are the one who decided to reverse it. Conflicting on attribution
would defeat the deterministic id entirely: it made a staff-bound void unrepairable by the run that
held the correct id for its outstanding capacity leg.

**Not covered by this amendment:** a back-office UI for the void (the staff surface is the credentialed
`/internal/` API), and partial reversal of a comped order — a void is whole-order by construction.

### Integrity language, unchanged (ADR-021)

A void is **honest-writer consistency**, exactly like the refund path it sits beside. The `order_voids`
row, its markers and its reported outcome are all commerce database state, and anyone with write access
to that database can alter them. Nothing here is tamper-evidence.
