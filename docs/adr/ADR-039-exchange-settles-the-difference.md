# ADR-039: An exchange settles the difference — one provider movement, two gross facts

Date: 2026-07-31

## Status

Accepted (TKT-158; decision taken under the owner-waived gates of that run, recorded on the ticket
and on epic TKT-9). Fourth slice of TKT-9. **Completed by TKT-166** (the entitlement switch), and
**amended by TKT-167** (§3c: an exchange interrupted after the money moved resumes from its
persisted basis — also taken under waived gates, `config.gates: autonomous`).
**Amended by TKT-498** (§2: a settled exchange whose switch Access refused because a source ticket
was admitted is abandoned by an operator; §6: the abandoned exchange keeps its row), taken under
`config.gates: autonomous`.

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

**Before TKT-169, commerce did not check admission before an exchange.** A buyer could exchange a
ticket that had already been used, then receive a fresh unredeemed replacement. TKT-169 adds an
admission read before the target hold; Access still checks under the ticket locks when it switches,
to cover a scan after commerce's read.

**TKT-166's ai-review reversed the first answer here, and the reversal is the interesting part.**
The plan proposed switching anyway and merely documenting the gap, reasoning that refusing would
strand a paid exchange. The review pointed out that the gap is *created* by that ticket — TKT-158
could not produce it, because it never switched anything — and that a follow-up ticket does not make
a shipped double-admission un-shipped.

The switch **refuses a source ticket that has already been admitted**
(`ErrSourceTicketsAlreadyAdmitted`, checked under the same row lock as the void, covering both
`redeemed` and a pass `entry`). If a scan commits after commerce's read, the exchange can already
be settled when this refusal occurs. The outstanding switch is visible as
`tickets_exchanged_at IS NULL`.

**D1 and D2.** TKT-169 moves the ordinary refusal before money. Commerce asks access whether any source ticket
has been admitted before it reprices the target or takes a hold. The rule applies to single-admission
tickets and passes: `redeemed`, pass `entry`, and quarantine admission evidence all count through
`ticketAdmittedUnion`. An unused pass remains exchangeable.

**D3.** Access answers `GET /internal/orders/{id}/admission?organizer_id=…` with `admitted` and
`issued_count`, from one read-only repeatable-read snapshot. An authenticated read with no scoped
tickets returns 200 with `admitted: false` and `issued_count: 0`. A 404 means the internal
credential was refused.

Access has no order record, so it cannot tell an unknown order from one awaiting issuance.
Commerce treats a zero count or a count different from the source quantity as issuance not
confirmed and returns retryable 503 before money. An access 404, transport error, other non-200
answer, or malformed response returns 502. An admitted source returns 409 with code
`source_tickets_already_admitted`. A settled replay does not read admission again. A basis resume
re-reads admission only when `settling_at` is NULL and the ADR-067 payments evidence rule returns
`Absent` for this exchange's money leg. `Present`, `Indeterminate`, or an evidence lookup error
preserves the existing resume path, because refusing could strand a charged buyer. A set
`settling_at` skips both reads. The marker is best-effort and is not evidence that payments was or
was not called. Access's row-locked switch refusal remains the backstop for a scan after the
admission read. If that race refuses the switch after settlement, the exchange reaches the state
tracked by TKT-498.

The read cannot stop a scan that commits after it. Access therefore keeps the same admission check
under the source ticket row lock during the switch. If that check refuses, the exchange is already
settled and the buyer has no replacement. This is the remaining read-to-switch race, and the locked
check is its backstop. **D4. AC-4 is not resolved by ADR-067:** `unwind-exchange` refuses a
settled exchange. TKT-498 resolves it with a separate command: see the amendment below.

#### Amended by TKT-498: a settled exchange whose source was admitted is abandoned

The race above leaves an exchange settled and unswitched for good: the delta moved, the replacement
order exists and holds the target capacity, and the buyer used a source ticket. The owner decided
on 2026-10-12 to **abandon** such an exchange and never to issue the replacement. An operator runs
`commerce abandon-exchange <organizer-id> <exchange-id> "<operator-name>" "<reason>"`; the procedure
is in the [order recovery runbook](../runbooks/order-recovery.md#settled-exchange-without-a-ticket-switch-tkt-498).
`commerce list-settled-unswitched-exchanges <organizer-id>` lists the candidates from Commerce
alone.

**The admitted source is proved by two ordered reads.** Commerce reads the existing Access
admission summary, `GET /internal/orders/{id}/admission`, twice. It reads the **source** first and
requires `admitted: true` with `issued_count` equal to the exchange quantity. Only then does it read
the **replacement** and require `issued_count: 0`. A null `tickets_exchanged_at` alone proves
nothing, because Access can have switched while its Commerce callback lags. The order is the proof:

- Access's switch inserts the replacement tickets in the transaction that voids the source, and
  under the source ticket row locks it refuses an admitted source. So once the first read observes an
  admission, no later switch can commit.
- A switch that committed before the first read left replacement tickets, which nothing deletes and
  which the second read counts. A late offline admission recorded after such a switch does not hide
  them.
- Reading the replacement first would miss a switch that commits between the two reads.

The proof assumes honest Access writers and a consistent database history (ADR-021). Access's wire
contract is unchanged.

**What counts as an Access answer.** Every Commerce admission read uses one Access caller: the
exchange's pre-money check, the replacement refund dispatch check below, and both abandonment reads.
That caller sends the shared internal token, which each call site names, and never follows a
redirect. A redirect is an unresolved answer, and the redirect target receives no request. The answer
is decoded exactly: both members, with names matched exactly and not null, and no unknown or
duplicate member. A case variant such as `Issued_Count` is a different name, not a second value for
`issued_count`. An answer that fails this rule, or with `admitted: true` and `issued_count: 0`,
proves nothing: Access reports an admission only for one of the order's issued tickets. The
pre-money check answers such a reply with its existing `502`.

**Three branches, by the sign of the stored delta.** Every branch keeps the exchange, both orders and
the two original gross facts, and returns the whole target claim to inventory under one receipt
derived from the organizer and the exchange. The source's admitted tickets and its capacity stay.

| Delta | Money | Recorded on the exchange |
|---|---|---|
| Upgrade | Payments refunds exactly the delta against `exchange-charge:<id>`, under the same derived key. | The Payments fact of that refund. |
| Downgrade | Nothing moves. The settlement's refund leg stays the only provider refund. | The write-off, `-delta_amount`, in the stored currency. |
| Even | Nothing moves. | Neither. |

**One compensating journal fact, `order.exchange.abandoned`.** ADR-003 requires a correction to be
a new entry, and the two original gross facts would otherwise go on describing a target sale and a
source reversal that no longer stand. The fact uses the existing Payments fact envelope. It is a
journal fact, not a JetStream event, so ADR-017 does not govern it and no event schema changes.

| Field | Value |
|---|---|
| `fact_id` | Derived from the organizer and the exchange, so every retry sends the same id. |
| `buyer_id`, `currency` | The exchange's buyer and stored currency. |
| `occurred_at` | The recorded instant at which inventory returned the target. |
| `amount` | The target gross that the abandonment removes: the target face plus the fee retained from the source, which is the amount of `order.exchange.sold`. Zero is legal. |
| payload `order_id` | The source order. |
| payload `exchange_id` | The exchange. |
| payload `source_gross` | The source gross that the abandonment restores, which is the amount of `order.exchange.reversed`. |
| payload `delta_amount` | The signed delta. `amount - source_gross = delta_amount`. |
| payload `refund_fact_id` | Upgrade only: the Payments fact of the delta refund. |
| payload `written_off_amount` | Downgrade only: the delta the platform writes off. |

Money in the payload is base-10 integer minor units in canonical form, and ids are canonical UUIDs,
because the journal signs the payload bytes. Payments accepts exactly the key set that the sign of
`delta_amount` selects. Every other fact type keeps its `order_id`-only payload rule. The fact adds no
settlement ledger entry.

**Why one fact, and why the target gross is its amount.** The abandonment is one disposition. One
fact can be journaled whole or not at all; a pair could be journaled half. The amount is what leaves
the recorded exchange sale. The restored source gross and the downgrade's write-off describe the
same disposition, so they travel in its payload. The upgrade's actual refund is already its own
`payment.refunded` fact, which the payload names.

Commerce sends the fact to Payments first and requires the journal's acknowledgment. Only then does
it record the same fact in `order_facts` and set `abandoned_at` and the write-off, in one transaction.
A refused or unanswered append, a Payments 4xx included, leaves the abandonment started and
retryable, never terminal. Commerce rebuilds the payload from write-once exchange fields, so a retry
sends identical bytes and needs no stored payload.

**Why the target capacity can return without a void (ADR-038 §1).** ADR-038 §1 forbids freeing
capacity while a ticket on it can still admit, because that ordering can oversell. Here the ordered
reads establish that the replacement has **zero** issued tickets, and none can issue afterwards: the
switch is the replacement's only issuer (§4), and Access's locked check refuses every switch of the
now-admitted source. With no ticket on the target, there is nothing to void, and returning the
target cannot oversell. The source capacity stays claimed, because its ticket was used.

**A provider's terminal refusal of the upgrade refund stops the command.** ADR-032 and ADR-087
forbid a new submission after a terminal provider failure, and every other move would choose an
unresolved financial outcome. The exchange then stays in this state: settled; abandonment started;
the provider's `re_` reference recorded; the Payments leg bound, with its amount still counted
against the delta charge; the buyer still charged; the target still held; the replacement inert for
money, issuance and capacity; no terminal marker. The exchange stays in the listing and in the
reversal backlog. **TKT-588** owns the next disposition.

**Replacement refunds.** Once the abandonment starts, the refund binder refuses a fresh bind and a
replay on the replacement order, with the existing staff `409` and bulk `not_refundable`. The same
locked read gives the replacement identity and the switch marker. A replacement whose switch
Commerce recorded (`tickets_exchanged_at`) goes to Payments with no Access read: Commerce writes the
marker only after Access committed the switch, so the replacement has issued tickets and no
abandonment can start. For a replacement whose switch is not recorded, the shared refund dispatch
requires Access to report issued replacement tickets before it calls Payments. Without them, or
without an answer, the refund stays pending behind the existing retry path (staff `502`, bulk
`unavailable`). A dispatch that passes this check cannot race a successful abandonment, because
issued replacement tickets persist and defeat the second read. A replacement refund can reach money
only through the **TKT-586** checkout-key alias: Payments looks up the exact (organizer, key) pair,
and a same-organizer checkout that used the literal key `exchange:<id>` holds a real charge under the
replacement's key. This prerequisite exists because of that alias; reconsider it when TKT-586 removes
the alias. The abandonment refuses a completed replacement refund or one for which Payments holds a
leg. Only Payments' own leg-absence answer counts as no leg; any other 404 stops the command for a
retry. A pending replacement refund with Payments' leg-absence answer does not stop the abandonment,
and stays pending afterwards.

The Back Office aborts a staff refund after 5 s, and for a replacement whose switch is not recorded
the Access read runs inside that deadline. The deadline stays. When it crosses during the Access read
or during the Payments call, the request is cancelled and the refund stays pending under the same
key. The same key replayed later sends the refund's own id as the Payments leg key, so Payments binds
one leg.

**What counts as a Payments or inventory answer.** The command decodes every answer exactly: member
names matched exactly, no unknown or duplicate member, no trailing content, and no null member.
Payments omits a value it does not have; it never writes null. Then the answer must be one that its
producer can write:

- a found operation has a valid bind time; a resolved one has a fact id and no status-replay
  deadline; only a resolved operation with status `captured` and the stored delta proves the
  capture;
- a confirmation pair (`confirmed_captured_amount` or `confirmed_amount`, and `confirmed_currency`)
  is complete and agrees with the money, or is omitted on a legacy row; a refund leg that is not
  completed carries none;
- the typed failure is `422` with exactly `error: provider refund failed`,
  `code: provider_refund_failed` and a `re_` provider reference;
- a journal acknowledgment has a positive sequence, a boolean replay flag and a hash of 64
  lowercase hex digits (a SHA-256 entry hash).

Any other answer is not evidence. It records nothing, and the same command can retry. Only the typed
`422` records the provider failure that stops the abandonment. The Commerce check of the journal
acknowledgment is a shape check: chain verification stays in Payments, within ADR-021's limit.

**Replacement voids.** A zero-gross replacement is reversed by a void, not a refund. Once the
abandonment starts, the void binder refuses a fresh bind and a replay on the replacement order, with
the existing staff `409` and bulk `refund_refused`: the abandonment makes zero issuance permanent, so
the void's ticket obligation could never be discharged. An existing void row stays as history.

**The exchange replay.** A replay of the original exchange request after a terminal abandonment
answers `200` with status `abandoned` and the original exchange and money values, and does not repair
the switch outbox. A started abandonment that is not terminal still answers `switch_pending`. The
handler gives this answer at each point where it observes the settlement or the abandonment:

- **Settled row.** The request finds a settled exchange at its first read, or a bind reloads one
  because another request with the same key settled it after that read. Both binds do this: a fresh
  request's bind after its target hold, and a basis resume's bind. The request answers from Commerce
  with no dependency call: `abandoned` for a terminal row, otherwise the idempotent settlement repair.
- **Unsettled basis.** The replay resumes from the basis and runs its forward steps. Its final
  settlement decides under the exchange row lock that the terminal transaction also takes. If
  another request settled the exchange and the abandonment finished in the meantime, the locked
  read refuses the settlement and the outbox write, and the replay answers `abandoned`.

The guarantee applies only at these points. A request that read the exchange before the terminal
commit makes no dependency call after it observes the terminal row, and writes no outbox row. Before
that point it runs its forward steps, and a dependency can fail or refuse one of them: a fresh
request's checks and target hold before its bind, or a resume's steps before its final settlement.
The request then answers that failure or refusal. The same request sent again answers `abandoned`.

**Attribution, and the adversary (ADR-021).** The command records the trimmed typed operator name,
the reason, and the PostgreSQL `session_user`. Only the first name and reason pair can resume or
replay the abandonment. The typed name is the operator's attestation; `session_user` is the database
login, which can be shared. A write-once trigger refuses any change to a populated abandonment field,
which protects against application defects only. A writer with Commerce database privileges can
alter the record, and a coordinated restore can roll it back. The Payments journal entry is
tamper-evident within ADR-021's rollback limit, not beyond it.

**Rollout.** Deploy Payments first, because only it accepts the new fact. Then run Commerce
migration 0028 out of band and replace the Commerce container. Wait at least 60 s after the old
container exits before the first abandonment, so that no old refund request without the issuance
check is still in flight.

### 3. `switch_pending` is a real, safe, durable state

This slice ends with the delta settled and the replacement order confirmed, while **the buyer still
holds valid old tickets**. It under-sells, cannot oversell, and never leaves the buyer with
nothing — the same safety argument ADR-037 §6 and ADR-038 make for their own intermediate states.

**Amended by TKT-498.** An exchange abandoned under §2 leaves `switch_pending` for good: its switch
will never happen. A replay of the exchange request then reports the terminal status `abandoned`.

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

### 3c. An exchange interrupted after the money moved resumes from its persisted basis (TKT-167)

`switch_pending` is the safe state this design *ends* in. There is a second intermediate state it
passes *through*, and it is not safe: **the delta charged, `settled_at` still NULL.** The buyer is
out of pocket and commerce has recorded nothing. Everything between the provider call and
`CompleteExchangeSettlement` — the confirm, both gross facts, the replacement write — can land there.

TKT-158 built the answer and did not connect it. It persists the full basis (target hold,
replacement reservation, unit amount, slot, total, signed delta, price snapshot) **before** calling
the provider, precisely so a retry settles the same numbers. But an unsettled replay fell through to
the forward path, which re-prices through catalog and re-submits the target hold *before* it loads
the basis. Both re-derivations fail exactly where recovery is needed:

- **catalog unreachable** → 502 at the reprice. Recovery blocked by a dependency the exchange had
  already finished with.
- **the target price moved** → inventory's claim fingerprint covers `unit_amount`, so the same
  `exchange-target:<exchange id>` key at a new price is `ErrIdempotency`, not a replay. The guard is
  working as designed; the retry is simply asking the wrong question.

So an unsettled replay with a recorded basis now **branches before any external call** and drives
only the steps after the basis. It calls no catalog and takes no new hold.

**This does not weaken [ADR-036](./ADR-036-pricing-rules-representation.md).** Catalog *was* the
single authority for this price; `target_unit_amount` / `target_total` / `target_price_snapshot` are
that authority's answer, persisted at the moment it was given. Re-resolving would not be more
faithful to ADR-036 — it would settle money against a price the buyer never agreed to and journal a
provenance snapshot describing a resolution that happened *after* the charge. Consuming the stored
resolution is the §5 provenance rule working, not an exception to it.

**Eligibility is deliberately not re-checked.** The resume reads the source through
`BindOrderExchange`, whose replay branch returns before the `completed` check and the refund
exclusion. A source that became ineligible *after* the provider moved money still resumes: refusing
there strands a charge, and there is no state to protect that is worth that.

**The resume re-submits the charge, and must.** Commerce cannot know whether the first submission
reached the provider — that is the whole predicament. What makes the repeat harmless is the
deterministic key `exchange-charge:<exchange id>`: payments converges it onto the one operation
(ADR-032, `payment_operations`' primary key). The invariant is therefore *one key*, not *one
submission*, and stating it the other way round would ask commerce to guess that the money already
moved, which is the guess this whole mechanism exists to remove.

`RecordExchangeBasis` became **record-or-load** for the same reason. Reporting only "you lost the
race" left the losing writer with nothing to do but refuse; it now receives the basis that actually
persisted and continues on it. A missing exchange returns `sql.ErrNoRows` rather than a zero basis —
zero total, zero delta, nil hold reads as an ordinary even exchange settling nothing.

**The 202 was undeclared.** The handler has answered `202 confirmation_pending` since TKT-158 on the
confirm-failure branch, and the contract did not list it, so the response validator turned it into a
500 ([ADR-028](./ADR-028-response-drift-fail-closed.md)). The one state a retry can recover from was
being reported to its caller as an outage. Declared in TKT-167.

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

**Amended by TKT-498: an abandoned exchange stays a live row.** `abandoned_at` marks the
replacement as dead: no ticket, no capacity, no further refund. It does not remove the exchange. The
row deliberately stays live for the unique source index and for the refund count, so the source
cannot be refunded, voided or exchanged again: its ticket was used, and the abandonment's journal
fact already restored the source gross. This is why the abandonment does not use the delete of
ADR-067 §4. An unwind removes an exchange that never settled and must stop blocking its source; a
tombstone there would have to be excluded from both rules. An abandonment must keep blocking its
source, so neither rule changes. Only the reversal backlog read gains a predicate,
`abandoned_at IS NULL` (ADR-063 §6).

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
