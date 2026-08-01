# ADR-039: An exchange settles the difference — one provider movement, two gross facts

Date: 2026-07-31

## Status

Accepted (TKT-158; decision taken under the owner-waived gates of that run, recorded on the ticket
and on epic TKT-9). Fourth slice of TKT-9. **Completed by TKT-166** (the entitlement switch).

Builds on [ADR-037](./ADR-037-post-purchase-refund-money-protocol.md) (refund money protocol) and
[ADR-038](./ADR-038-refund-reversal-ticket-voiding.md) (reversal obligations and their ordering).

## Context

An exchange is a **reversal and a sale in one operation**. The refund machinery it would compose
from is all merged and sitting right there — which is exactly what makes the wrong answer tempting.

## Decision

### 1. One net provider movement, two gross journal facts

Modelling an exchange as *a refund of the old line plus a checkout of the new* would refund the old
**gross** amount and capture the new **gross** amount: **two** provider movements, and a cash-flow
story no buyer recognizes. An exchange moves exactly the **difference**:

- upgrade (target > source) → one charge for the delta
- downgrade → one partial-refund leg for the delta, against the **original** charge
- equal → **no provider call at all**

The trail is a different question and gets the full picture: `order.exchange.reversed` for the
source total and `order.exchange.sold` for the target total, both written whichever way the money
went. **What the provider does and what the trail records are not the same fact**, and conflating
them is how the AC that says "exactly the difference" and the AC that says "both legs journalled"
appear to contradict each other. They do not.

### 2. Every refusal happens before any money moves — and here that is worth a round trip

Seated source, currency mismatch, unavailable target: all refused before settlement.

This is the **opposite** of the call [ADR-038 §9](./ADR-038-refund-reversal-ticket-voiding.md) made
for refunds, deliberately. There, a pre-money eligibility check was rejected because refusing a
buyer's *refund* to protect a resale is the wrong trade — the refund still completes and the
failure merely under-sells.

**An exchange has no equivalent safe partial state.** A settled delta plus a half-done exchange
leaves the buyer holding the wrong thing. So the same round trip that was not worth it there is
worth it here, and inventory gains a read-only `GET /internal/holds/{id}/seating` because
seatedness is a property only inventory owns.

Seatedness is **any** `claim_seats` row, released or not — the rule TKT-161's review established:
`claims.status` and `claim_seats.released_at` are not schema-coupled, so a seated claim whose rows
were already released is representable and is still seated.

**Qualification (TKT-166): "every refusal" is not every refusal.** This clause does not cover a
source ticket that has **already been redeemed**. Commerce checks seatedness, currency and
availability; nothing asks access whether the old tickets have been used. So a buyer can exchange,
scan in during `switch_pending`, and then have the used ticket voided and a fresh unredeemed
replacement issued — **two admissions for one exchange**. The window is normally short and grows
with any delay in the switch.

**TKT-166's ai-review reversed the first answer here, and the reversal is the interesting part.**
The plan proposed switching anyway and merely documenting the gap, reasoning that refusing would
strand a paid exchange. The review pointed out that the gap is *created* by that ticket — TKT-158
could not produce it, because it never switched anything — and that a follow-up ticket does not make
a shipped double-admission un-shipped.

So the switch now **refuses a source ticket that has already been admitted**
(`ErrSourceTicketsAlreadyAdmitted`, checked under the same row lock as the void, covering both
`redeemed` and a pass `entry`). The cost is a settled exchange that never switches — which is not a
new failure state at all: it is exactly what TKT-158 shipped for *every* exchange. The refusal keeps
that previously-safe behaviour for the one case where switching is unsafe, and the outstanding
obligation is visible as `tickets_exchanged_at IS NULL`.

That is a safe default, not the answer. Whether a used ticket should be exchangeable **at all** —
refused before the money moves — or whether the entry should **carry forward** to the replacement,
which is not even binary for a multi-entry pass (ADR-005), is a product decision nobody has taken.
**TKT-169** owns it. Until then this clause reads: every refusal this ADR *enumerates* happens
before money moves, and the one it does not enumerate happens after, by refusing the switch.

### 3. `switch_pending` is a real, safe, durable state

This slice ends with the delta settled and the replacement order confirmed, while **the buyer still
holds valid old tickets**. It under-sells, cannot oversell, and never leaves the buyer with
nothing — the same safety argument ADR-037 §6 and ADR-038 make for their own intermediate states.

The four orderings rejected to arrive at it:

- release the old capacity first → **can oversell**
- invalidate the old tickets first → can leave the buyer with **neither**
- issue the new tickets first → a **both-admit** window
- block on asynchronous issuance → contradicts the outbox model

Progress is nullable timestamps (`settled_at`, `tickets_exchanged_at`), the shape ADR-038 §6
settled on, with a CHECK refusing a switch that precedes settlement. Independent storage,
**safety-ordered execution** — the correction ADR-038 §6 itself had to make.

### 3b. The switch commits, then the capacity comes back — and the marker precedes both (TKT-166)

Access commits its switch transaction, then calls commerce
(`POST /internal/exchanges/{id}/tickets-switched`). Commerce records `tickets_exchanged_at` and
only **then** asks inventory to return the old capacity. Freeing capacity while the old tickets
still admit is the one ordering that can **oversell** ([ADR-038 §1](./ADR-038-refund-reversal-ticket-voiding.md)),
and the callback is what makes that ordering checkable across a boundary the databases do not share.

The marker is set **before** the inventory call, not after. Marking after would invert the evidence:
a crash could free capacity while the row still claimed the switch never happened. Marking first
leaves the opposite substate — switched, capacity outstanding — which under-sells and is visible.

Naming that substate needs a **third** timestamp, `capacity_returned_at`, added in migration 0011.
0010 shipped two, which was one short: the reversal has three facts, and the third is separated from
the second by a network call. `order_refunds` already carries this column for the same reason
([ADR-038 §6](./ADR-038-refund-reversal-ticket-voiding.md)); a state the projection cannot express
is strictly worse than a nullable column.

This is an **honest-caller** guarantee, not tamper-evidence ([ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)):
anyone holding the internal token can call inventory directly and skip all of it. The adversary here
is a crash, not a writer.

### 4. The replacement order deliberately owes no issuance event

`persistExchangeReplacement` writes the replacement reservation and order **without** an
`order.completed` outbox row. Owing one would make access issue the new tickets while the old ones
still admit — the both-admit window §3 rejects.

### 5. TKT-166 publishes a new subject, not `order.completed` schema 2

The draft proposed bumping `order.completed` to schema 2 so access could switch the tickets
atomically. Rejected: that drags ADR-017 across the issuance path **every consumer already depends
on**, and this repo has the scar — TKT-61 shipped the schema-ordering bug twice, past a
mutation-checked suite and a full review pass.

`platform.commerce.order.exchanged` (schema 1) carries the same information, needs no bump, and
cannot break the existing decode. The outbox is already subject-generic (`completion_outbox.subject`),
so this costs one consumer arm and no migration.

Note the distinction from the draft's own rejection of "a new event *followed by* ordinary
issuance": that is non-atomic and correctly rejected. This is a new event **instead of** ordinary
issuance for the exchange path, which is atomic in one access transaction.

**Settlement and the owed event commit together (TKT-166).** `CompleteExchangeSettlement` sets
`settled_at` and inserts the `order.exchanged` outbox row in one transaction, freezing the bytes at
the persisted `settled_at` so a republish cannot differ. This is [ADR-016](./ADR-016-checkout-recovery-state-machine.md)
§Decision 6's rule applied to a second subject, and it matters more here than for `order.completed`:
because §4 leaves the replacement owing no completion event, this row is the **only** thing that
will ever issue its tickets. A settlement that committed without it would be a permanent
`switch_pending` no retry could notice.

### 6. An order is reversed once

A refund and an exchange both take the source order's row lock, and each refuses an order the other
already owns. A unique index on `source_order_id` enforces one exchange per order under
concurrency — a read either path could lose is not enough.

**Correction (TKT-166).** This clause said *partial* unique index; migration 0010 creates an
unconditional one, and the code is right. A partial index would need a predicate, and there is
nothing to predicate on: an exchange has no cancelled or inactive state, so every row is live. The
prose described a hedge the design does not need and does not have.

## Consequences

- Between TKT-158 and TKT-166 an exchange is paid for but not yet real at the gate. Under-sells.
- A seated order cannot be exchanged at all (**TKT-164**). Not narrowed to partial, as the refund
  path was: an exchange of *any* seated line cannot say which seat leaves.
- Exchanges are whole-line only, preserving quantity. Partial-line and chained exchanges need
  monetary-basis allocation across historical charges — a different problem.
- **Name the adversary (ADR-021).** Honest-caller consistency: the ordering and the one-reversal
  rule are enforced by application logic and constraints inside databases a writer with access can
  edit. What they protect against is concurrency and crash-retry. The hash-chained journal is what
  makes the *record* tamper-evident, and it is unchanged.
