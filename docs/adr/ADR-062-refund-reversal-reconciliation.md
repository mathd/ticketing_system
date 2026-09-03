# ADR-062: Outstanding refund reversals are reconciled by a leased runner, and a missing ACCESS_URL fails readiness

Date: 2026-08-18

## Status

Accepted (TKT-163; decision taken under `config.gates: autonomous`, so gates 2–4 were the agent's —
the plan-review critique, the self-made decisions and the overridden objections are on the ticket).

Amends [ADR-038](./ADR-038-refund-reversal-ticket-voiding.md) §7, which named this ticket and
recorded the design below as *rejected for now*. Clarifies
[ADR-040](./ADR-040-event-cancellation-bulk-refund-runs.md)'s "reported, not repaired" rule as
bounded to cancellation runs. Inherits [ADR-010](./ADR-010-postgres-claim-transaction.md)'s lock
discipline and [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)'s adversary language.

## Context

ADR-038 §7 shipped a refund's reversal as **visible and retryable** and left it there:

> Retry is a **replay of the same refund idempotency key** … **Nothing retries on its own**: an
> access outage, or a refund that outruns issuance, leaves the tickets valid until something
> replays the refund. That is the accepted cost, stated rather than papered over, and it is tracked
> as **TKT-163**. A leased runner with claim tokens, attempts, backoff and fenced writes was
> designed and rejected: the requirement is that outstanding work be *visible and retryable*, not
> automatically retried … Adding one later is additive.

The cost was real and the reviewer's sharpest point was about *who notices*: the caller receives a
**200**, so nothing tells it to come back. Discharge depended on a human reading `tickets_voided:
false` and choosing to replay. Meanwhile the tickets still admit.

Two things changed since. TKT-161 added `capacity_returned_at`, so there are now **two** obligations
of identical shape and one mechanism can cover both — the condition the ticket's own deferral named.
And the requirement §7 said nobody had stated has since been stated, as this ticket's COS 1.

## Decision

### 1. A dedicated leased runner in commerce, driving the existing reversal unit

`internal/reversal` claims completed refunds still owing an obligation, leases them, and drives each
through **`refunds.Service.DriveReversal`** — the same unit the staff endpoint and the cancellation
runner already use. It composes no reversal of its own and **cannot move money**: its port exposes
one method, which is a stronger guarantee than a comment saying it does not.

This is ADR-038 §7's "adding one later", exercised. §7's reasoning is not overturned — it was
correct that a second recovery state machine is a large cost against an unstated requirement. The
requirement is now stated.

It is a **third** runner rather than an extension of either existing one, for the reason
`internal/bulkrefund` gave for not merging into `internal/recovery`: different eligibility,
different terminal states, and one state machine serving all three would be readable by nobody.
`bulkrefund` specifically **cannot** fill this gap — it only sees orders enumerated into a
cancellation book, so an ordinary staff refund with a stuck obligation is invisible to it.

Rejected alternatives: an **external cron or admin replay endpoint** (still needs durable claiming,
backoff and fencing, and makes completion depend on another deployment component); an
**event-driven outbox** (the failure is a durable row obligation, not a missing event — an access
outage requires revisiting *existing* rows, which no new event announces).

### 2. Permanently undischargeable obligations are PARKED, never predicted

This is the load-bearing decision and the one the plan got wrong first.

Some obligations can never be discharged. Inventory refuses a **partial** return of a **seated**
claim (`ErrPartialSeatedReturn`, 409): nothing associates an issued ticket with a seat identity, so
"which two of these three seats" is unanswerable (TKT-164). A naive sweep retries such a row
forever, oldest-first, starving everything behind it.

The obvious fix — exclude those rows with a predicate — **cannot be written in commerce**, for three
independent reasons:

1. No commerce code reads `reservations.seat_identities`; the column is written and never read here.
2. Inventory does not decide seatedness from seat identities at all. It counts `claim_seats` rows
   **in its own database**, and its own comment insists seatedness is *any* seat row, released or
   not — an inference from live rows is a defect it already fixed once.
3. Even with perfect seatedness knowledge, **"partial" is not computable in commerce**. The refusal
   fires when `returned != 0 || quantity != quantityTotal`, and `returned` is
   `claims.returned_quantity` — inventory-side state that moves when *any other refund of the same
   order* returns capacity. A refund for the reservation's full quantity is still a partial return if
   an earlier refund already returned some.

A predicate would therefore be wrong in both directions and would drift from inventory's rule with
every change to it. So the refusal is **observed, not predicted**: attempts are charged, backoff
grows, and a row that never progresses parks itself. This generalises to permanently-refused shapes
nobody has enumerated, which a hand-written predicate cannot.

The lifecycle is `orders`' recovery lifecycle, copied: `FOR UPDATE SKIP LOCKED` claim in a CTE, a
lease with a claim token fencing every later write, exponential backoff capped at five minutes,
`parked_at` at `MaxReversalAttempts = 10`.

### 3. Attempts reset on progress — which is what makes a bound safe

A bounded budget and an outage of unknown length are only compatible because **a pass that
discharges an obligation it had not discharged before resets `attempts` to zero**. Without that, a
long access outage would retire a refund that was about to recover — reintroducing the exact failure
this ADR closes, in the name of fixing it.

Discharging **one** of two obligations counts. Otherwise a refund whose voiding succeeds and whose
capacity return keeps failing spends its whole budget on the half that already works.

The plan drafted here originally proposed *unlimited* retries to avoid that failure, while
simultaneously listing "a permanently refused return spins forever" as a top risk — the two are
incompatible, and progress-reset is what satisfies both.

### 4. Parking is only honest if someone can see it

Bounding attempts converts "retries forever" into "stops". A mechanism that stops **silently** is
worse than one that spins loudly, because spinning at least surfaces somewhere. Three gauges make
stopping visible — `commerce.refund.reversal.{outstanding,parked,oldest_age_seconds}` — and they are
what earns COS 1's second branch ("an explicitly monitored, documented reconciler that is part of
the deployment contract"). The age gauge exists because a count cannot distinguish a small, old
backlog from a large, fresh one, and those are different incidents.

These are commerce's **first** metrics. The MeterProvider has been live since `obs.Setup`; nothing
had ever registered an instrument. Following access's shape, including its rule: *observability, not
a gate* — a failure to register gauges is logged and the service still refunds.

**Unparking is not in scope.** An operator path to resolve or unpark is TKT-146, which already owns
the identical problem for parked recovery orders. Inventing a second one here would guarantee two.

### 5. A missing `ACCESS_URL` now fails READINESS, and `/healthz` and `/readyz` split

They were the same handler since commerce was scaffolded. Now:

- **`/healthz`** — "this process is working": database and broker. It is what the container
  healthcheck probes and what the gateway's `depends_on: service_healthy` waits on, so anything
  added here can keep the stack from starting.
- **`/readyz`** — additionally, "this deployment is configured to keep its promises".

ADR-038 §7 made `ACCESS_URL`'s absence a deliberate degradation: a refund still returns the money
and leaves voiding outstanding and *retryable*. **This ticket makes that worse rather than better.**
The reconciler drives voiding through that URL, so without it there is now a mechanism that appears
to guarantee completion and silently cannot — converting a visible outstanding obligation into an
invisible one. A misconfigured deployment must not take traffic.

**It checks CONFIGURATION, never reachability.** ADR-021 §D6 rejected gating readiness on a runtime
dependency's liveness — *"a broker blip would close every turnstile"* — and that reasoning is sound
and binding for anything that can flap. A missing environment variable cannot flap: it is settled at
startup and is either wrong for the process's whole life or not at all. That distinction is the
whole justification, and a future reader who reads this as contradicting §D6 should re-read it.

Verified rather than assumed: commerce's healthcheck probes `/healthz`, so a failing `/readyz`
cannot deadlock compose startup. Had it probed `/readyz`, this decision would have been unshippable
as written.

The binary still **boots** without `ACCESS_URL`. Booting degraded and being routed traffic are
different questions, and only the second was ever really answered by "optional".

### 6. The void-before-capacity ordering becomes a database rule

`MarkRefundCapacityReturned` gains `AND tickets_voided_at IS NOT NULL`, plus a CHECK constraint —
matching what `order_exchanges` has carried since migration 0011.

This **reverses a choice TKT-161 stated deliberately**: *"Commerce enforces that ordering — it will
not attempt the return until `tickets_voided_at` is set."* That was a sufficient guarantee with
**one** caller. This ADR adds a second, and one-caller-enforces-it stops being a guarantee once
callers multiply. Freeing the seat while the ticket still admits is the one ordering that can
**oversell** (ADR-038 §1), which is too expensive to leave to a convention.

Safe to add: `capacity_returned_at` has exactly one writer in commerce, downstream of
`DriveReversal`'s guard, so no existing row can violate the constraint.

## Consequences

### What this does not cover, deliberately

- **Exchanges.** `order_exchanges` carries obligations of the same shape and **no commerce-side
  sweep exists at all** — their only retry is access's JetStream redelivery, driven by commerce
  answering 502. If that consumer dead-letters, nothing revisits those rows. All four COS name
  refunds and exchanges have a different eligibility model, so they are a follow-up ticket, filed at
  this ticket's closeout. Named here because a gap recorded in an ADR is findable and a gap recorded
  nowhere is not. **Closed by [ADR-063](./ADR-063-exchange-reversal-reconciliation.md) (TKT-259)**,
  which found the eligibility difference to be sharper than "different": an exchange's switch marker
  is *access's* fact, so a sweep can drive the capacity half and must never assert the other —
  §2 there, and the reason §2 of THIS ADR does not transfer wholesale.
- **The claim query's organizer scoping, before TKT-267.** The final join has always compared the
  source reservation's organizer to the refund's, so no foreign `hold_id` ever escaped. But
  `claimable` selected and `claimed` **leased** before that join ran, so a malformed row — completed,
  obligations outstanding, whose source reservation belongs to another organizer — took a lease and a
  claim slot and then vanished at the join. Never returned means never released and never abandoned:
  nothing charged an attempt, nothing parked it, nothing cleared the lease, and it re-took a slot on
  every lease expiry while the function reported no error. Worse than one slot, because a
  leased-then-dropped row makes the store return fewer rows than it leased and `RunOnce` reads a short
  batch as a drained queue (`len(claimed) < r.batch`) and ends the pass. **Closed by TKT-267**, which
  moved the check into `claimable` for this query and ADR-063's alike; the final join stays as defence
  in depth. TKT-267's `EXISTS` could not be part of the partial queue index, so its cost was linear
  in the rejected prefix; **[ADR-070](./ADR-070-indexable-reversal-claim-scoping.md) (TKT-268)
  replaced it with an indexed column comparison** and the claim's work is now bounded by the batch. This was a **liveness** defect, not a leak, and per ADR-021 it is honest-writer
  consistency: no code path writes a mismatched pair, a writer with commerce database access still
  can, and the predicate constrains that writer not at all.
- **Unparking.** TKT-146, above.
- **Seated partial returns.** TKT-164 owns the repair. Here they are parked and counted.
- **Whether inventory should demand proof of prior voiding.** TKT-165, unchanged.
- **Comped (zero-price) orders** get no reversal at all, because there is no money leg to refund
  against — ADR-040 recorded this and it is untouched.

### Integrity language — name the adversary (ADR-021)

Everything above is **honest-writer consistency**: it holds against concurrency, crashes, restarts
and two replicas. It is **not tamper-evidence**. Anyone with commerce database write access can
clear a lease, unpark a row, or forge a discharge timestamp — and the reconciler would believe them.
The signed, append-only payments journal remains the evidence that money moved; ADR-021's limits on
it are unchanged by this decision.

### Operational

`REFUND_REVERSAL_INTERVAL` (default `1m`, the longest of commerce's four workers — every row it
claims has already failed against something unavailable, and re-asking a downed service every ten
seconds is how a reconciler becomes a second outage) and `REFUND_REVERSAL_BATCH` (default `16`). A
restart drains immediately regardless, so the interval bounds the steady state, not recovery from a
deploy. Operator surface and the meaning of each state: `docs/development.md` § Refund reversal
reconciliation.
