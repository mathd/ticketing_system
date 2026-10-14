# ADR-064: Presale unlock codes

Date: 2026-08-10

## Status

Accepted (TKT-239; epic TKT-17, US-CH5)

Supersedes nothing. Extends [ADR-024](./ADR-024-channel-allocations.md) (channel allocations) and
[ADR-054](./ADR-054-per-channel-sales-windows.md) (sales windows).

**A code's usage counts a held claim until a terminal write (2026-10-14, TKT-543).** Amended below
in §3 and §Consequences. A code capped at N could end with more than N confirmed units: an
admission on one pool left out an expired, unswept hold on another pool, and a finalize that had
queued on that pool before the expiry then kept the hold
([TKT-380 evidence](../evidence/TKT-380/README.md), probe 1, S1). The admission count now follows
reading R2 of [ADR-084](./ADR-084-promotion-storage-and-redemption-contract.md) §3. The cost is
accepted and recorded in §3: an expired hold on a quiet pool keeps its code's units blocked for
other pools until a write on that pool sweeps it; TKT-547 is the remedy.

## Context

ADR-024 gave a channel **how much** it may sell. ADR-054 gave it **when**. Neither says **who**.
A presale that opens Tuesday to everyone is not a presale — it is an early on-sale.

No code, voucher or redemption machinery existed anywhere in the repo before this ticket. That
absence is why the decisions below are worth recording: nearly all of them had a plausible
alternative that would have worked in a single-slot demo and failed in production.

## Decisions

### 1. The gate lives in **inventory**, on `channel_allocations`, not on catalog's channel registry

`channel_allocations.requires_code boolean NOT NULL DEFAULT false`.

The obvious home is catalog's `channels` row (TKT-235), and it is wrong twice over:

- That registry is deliberately **a lookup, not a constraint**. `0018_channels.sql` says so at
  length and `TestAnUnregisteredChannelCodeStillSells` pins that an unregistered code still sells.
  A `may this sell` flag on that row reverses a recorded decision.
- **Inventory has no read path to catalog's tables at all** (ADR-002) — separate services, separate
  databases, no client, no consumer, no replica. A gate there means a cross-service read on the
  claim path, which is what ADR-010's single-serialization-point rule exists to prevent.

So the flag sits on the row the claim path **already reads under the pool lock**.

**Accepted cost:** gating is configured per allocation, not once per channel. An organizer gating a
channel across a festival sets it on each slot's allocation. The alternative was a cross-service
dependency on the highest-contention path in the system.

**Owner decision**, taken explicitly when the conflict surfaced at re-shaping.

### 2. Redemption is serialized by the **`presale_codes` row lock**, not the pool lock

`SELECT … FROM presale_codes WHERE (organizer_id, channel_code, code) … FOR UPDATE`, taken **after**
the pool lock.

This is the ticket's load-bearing decision and the one that is easiest to get wrong, because the
wrong version looks right:

Every other derived count in inventory is scoped by `pool_id` — `reservedForChannelsSQL`,
the channel-cap sum, all of them. That is **why** the pool row `FOR UPDATE` suffices for a channel
cap. A presale code is `(organizer_id, channel_code, code)` and **spans every slot in the presale**,
which is the entire point of a code. Two concurrent holds on **different slots** take **different
pool locks**, never block each other, both read usage = N−1, and both insert.

**Verified, not argued:** with the row lock removed, a code capped at 5 granted **7**.

**Lock order is fixed: pool → code**, never the reverse. Nothing else in inventory locks a
`presale_codes` row, so no other order exists and self-deadlock is impossible.

Rejected: `pg_advisory_xact_lock` on a hash of the code identity (ADR-029's shape). That pattern
exists in *catalog* precisely because a seat-map family identity **is not a row**. Here the row
exists, and `refund_returns.go` already does pool-lock-then-second-row-lock in this service. The
exotic mechanism would have needed a justification it does not have.

### 3. Consumption is **derived**, never a counter

`codeRedeemedQuantity` sums `consumedQuantity` over the claims citing the code whose status is
`confirmed`, `finalizing` or `held`, with **no expiry test** (`presaleUsageClaims`; amended by
TKT-543, reading R2 of ADR-084 §3).

**What this section first said, and why it changed.** It filtered by `consumingClaims`, the predicate
that counts against a channel cap, "so the two can never disagree". That agreement was the defect.
A channel's count is pool-scoped, so the pool lock orders it against every finalize on that pool.
A code's count spans pools, and a held claim's expiry is judged by whichever transaction on *its*
pool runs next. `Transition` judges it on transaction-start `now()` (ADR-024, TKT-148), so a
finalize that queued on its pool before the hold expired still finalizes it afterwards. An
admission on another pool had already left the expired hold out of the count and admitted one
more unit: a code capped at 1 sold 2. The same happens on the draw-down child path, whose decision
precedes its commit. TKT-380 probe 3 executed R2 on this exact path (the cap held in 6 of 6 runs and
7 repeats; today's predicate broke it). `TestPresaleCapCountsExpiredHoldWhileFinalizeWaits` and
`TestPresaleCapCountsReservationWhileDrawDownInsertWaits` force both schedules: 2 confirmed units for
a cap of 1 with the old predicate, 1 with R2.

**What is still shared, and what now differs on purpose.**

- The quantity: `consumedQuantity`, net of refund returns, in every count. A refund returns a
  redemption exactly as it returns a seat.
- Channel and pool accounting keep `consumingClaims`: an expired held claim stops counting there
  as soon as `expires_at` passes.
- The admission count keeps an expired held claim until a **terminal write commits**: a
  `sweepExpired` whose transaction commits, the replay-expiry branch of `CreateHold`, the expiry
  branch of a refused draw-down, `Transition`'s expiry branch when it commits, or a release.
  Passing `expires_at` alone returns nothing. After a terminal write no path can finalize the
  claim, which is why this closes the race.
- The **operator** read (`PresaleCodeStatuses`) keeps `consumingClaims` (D19). It leaves out
  expired held claims that no terminal write has reached, so it reports the usage as it will be
  after the next sweep of each pool. Until then, admission can refuse a code that this read shows
  with headroom. R2 in the operator read would show a code as exhausted for every abandoned cart,
  and an operator who raised the cap on that reading would oversell once the carts were swept.
  The read has no caller outside tests yet; TKT-309, which owns the operator surface, must show
  both numbers or explain the gap. `TestPresaleUsageCountsUntilTerminalWrite` asserts both counts,
  before and after a terminal write, for each claim state.

**The accepted cost: liveness across pools, and a departure from ADR-010.** Every terminal write in
the request path is pool-local: an admission sweeps only its own pool before it counts. So an
expired, unswept hold on a quiet pool keeps its code's units counted for every other pool until a
write on that pool sweeps it, and a single-use code can refuse its own buyer elsewhere for that
long. Same-pool reclamation is unchanged. For a code across pools this departs from ADR-010's
"expired capacity is reclaimed on the next mutation": the next mutation of the code (an admission
on another pool) does not reclaim it. `TestPresaleCodeStaysBlockedByAnExpiredHoldUntilItsPoolIsSwept`
pins the cost. **TKT-547**, a background sweeper, is the remedy, and will change that test's first
half.

**Rejected: a request-triggered cleanup (owner decision D18).** It was built: on a cap refusal, the
admission rolled back, expired the code's due claims on the other pools in their own transactions,
then retried. Four adversarial review passes each found a way for a caller without the code to tell
a valid, exhausted code from an unknown one, because only that code triggered the cleanup and every
effect it left was visible:

- the replay answer of an unrelated expired hold on a cleaned pool (409 before the cleanup, 200
  after), while the cleanup used `sweepExpired`;
- the eviction of the cleaned pool's availability cache, visible on the public read (`Age` header
  and body);
- the retry's own refusal: a channel window that closed while the cleanup waited answered
  `channel_window_closed`, where an unknown code answered `presale_code_invalid`;
- the answer of a concurrent code-less hold on the cleaned pool whose transaction began before a
  hold there expired: 201 if a cleanup had expired that hold meanwhile, 409 `sold_out` if not;
- a commit's ambiguity: answering every failed retry with the code refusal turned a retry whose
  commit acknowledgement was lost into a definite refusal that left a claim and a consumed key
  behind, which an unknown-code refusal never does.

The first three were reproduced by tests before each fix; the last two were found by reading the
code in the fourth pass and were not executed.

Each fix closed one channel and the next pass found another; the class is the design, not a local
defect. Do not rebuild it in the request path: anything a code guess can trigger is an oracle
(§4). Liveness belongs to a process that no request triggers (TKT-547).

A counter cannot be decremented by **lazy** hold expiry without a sweeper, and ADR-010 forbids
requiring one for correctness. Derived also gets refunds right for free: `consumedQuantity` is net
of `returned_quantity`, so a refund returns the redemption exactly as it returns the seat.

The **confirmed** half of `consumingClaims` is load-bearing and easy to lose: a live-only count
would hand every redemption back the moment a buyer paid, so a code capped at N would sell
unbounded tickets — failing only *after* payment. A mutation check caught this gap in the test
suite, not in the code.

### 4. The refusal is **uniform across five causes**

Absent, unknown, wrong-channel, exhausted, and out-of-window all return `ErrPresaleCodeInvalid`
with an identical message and an identical wire body: `409 {"code":"presale_code_invalid",
"error":"invalid presale code"}`.

A distinguishing refusal is an **enumeration oracle** — submitting candidates and learning "exists
but spent" versus "no such code" is how presales get scraped.

**Name the adversary (ADR-021).** This defeats a scraper reading **response bodies**. It does **not**
defeat one measuring **latency** — an exhausted-code refusal costs a usage aggregation an
unknown-code refusal does not — nor one who simply observes that a channel is gated at all. Rate
limiting (ADR-051) is the control for those and is **not claimed here**.

The **operator** read (`PresaleCodeStatuses`) does distinguish them, deliberately: an operator
debugging "my code doesn't work" needs the answer, and that surface is internal-only.

### 5. Refusal **precedence**: window → code → capacity

1. No/released allocation → code-less `insufficient capacity` (there is no channel to gate)
2. **Closed channel window** → `channel_window_closed`
3. **Bad presale code** → `presale_code_invalid`
4. Pool or channel capacity → code-less `insufficient capacity`

Window before code: a closed channel is not selling to anyone, so "wrong code" would be a misleading
answer to a request a *valid* code would also have refused. Code before capacity, for the reason
TKT-238's review established — a channel-property refusal masked by a full pool makes a gated
presale read as a **sellout exactly when the on-sale is busiest**.

### 6. A draw-down **moves** a redemption — the child inherits the citation

The source reservation cites the code, and **every draw-down child inherits it**.

**This reverses the decision this ADR originally recorded, and the reversal is the point.** The
first version had children *not* inherit, reasoning that `consumingClaims` counts a live source and
its live children, so citing both would double-count and exhaust a cap at half value.

That reasoning is false. A draw-down **decrements the source by exactly the drawn quantity**, or
releases it whole when fully drawn — source + children always sums to the original. Citing both is
conservative, not duplicative.

The consequence of the wrong version was measured, not argued: drawing a 10-unit reservation fully
down took derived usage from 10 to **zero** while all 10 units remained consumed, and the same
"capped at 10" code then granted **10 more**. Twenty units from a cap of ten.

It survived a mutation check and a green test, because the test asserted the *wrong invariant* —
usage `== 6` after drawing 4 of 10, which is exactly what the defective code produced. A test can
encode a mistaken rule as confidently as a correct one.

The source's code is read **under the same row lock** as the rest of the source claim, so a child
cannot inherit a citation that disagrees with the units being moved.

Still true, and still the reason gating a draw-down would be wrong: a draw-down consumes nothing
new, so it **redeems** nothing new. Inheriting a citation is not redeeming — the redemption already
happened at placement.

### 7. Codes are **exact opaque strings**

No `citext`, no `lower()`, no trimming — at the schema, in Go validation, and at the HTTP decode.
`VIP`, `vip` and ` vip ` are three different codes, exactly as channel codes are (ADR-024, ADR-046
§4). Normalizing anywhere would disagree with the exact-match lookup and produce codes that can be
issued but never redeemed.

### 8. The claim's citation is **plain text with no foreign key**

`claims.presale_code`, nullable, length-checked, with a CHECK that it is never set without a
channel. No FK, for the reason `claims.channel_code` has none: historical attribution must survive
a code being edited or deleted. A claim records what happened, and what happened does not change
when configuration does.

A code presented to an **ungated** allocation is ignored **and not recorded** — persisting it would
let any caller write arbitrary strings into a column reporting reads, on a path where nothing
validated them.

### 9. The idempotency fingerprint appends the code **only when non-empty**, and **frames** it

A code must enter the fingerprint — on **both** the hold and the group-placement paths. Two requests
sharing a key but presenting different codes are different requests, and replaying one as the other
grants the second the first's redemption *without its code ever being checked*. Plan-review caught
this for `CreateHold`; ai-review caught that `PlaceGroupReservation` had the identical defect.

An **unconditional** append rehashes every claim in the database, so every in-flight retry stops
replaying and re-executes — a system-wide double-sell on retry, from what looks like adding a field.
Golden literals computed outside the package pin the pre-TKT-239 values; changing them is a
wire-compatibility break, not a test update.

And the appended part is **length-framed**, not colon-joined. Channel codes and presale codes are
both arbitrary opaque strings that may contain the delimiter, so a bare join is ambiguous:
`(channel="a", code="b:c")` and `(channel="a:b", code="c")` hashed **identically** — measured — and
the second request replayed the first before its allocation or code was examined. Framing applies
only to code-bearing requests, so **no fingerprint that can already exist in a database changes**.

A second review pass argued the framing broke compatibility with fingerprints written by this
ticket's own first commit. **Rejected on its premise:** that commit lives only on an unmerged
feature branch, never ran against a database, and a code-bearing request could not have existed
before the feature that introduced codes. The only compatibility boundary is pre-TKT-239 →
shipped, every stored fingerprint is code-less by construction, and
`TestOnlyCodeLessFingerprintsNeedBackwardCompatibility` pins it.

## Guarantees, and what they are not

**Honest-writer consistency, not tamper-evidence** (ADR-021). The row lock and the derived count
stop concurrent over-redemption and operator mistakes. Anyone with **inventory database access**
mints redemptions, edits caps, or inserts claims at will. Nothing here is tamper-evident, and no
claim in this ADR should be read as saying otherwise.

## Consequences

- `presale_code_invalid` joins the **closed** `Error.code` enum. Under ADR-028 an undeclared value
  is a **500 in production**, so the enum entry, the handler mapping, the exact-body assertion and
  the load-harness classification move as one unit.
- The load harness keeps it as `KindServerError`: a run against a gated channel measures the refusal
  path, not contention, so counting it as capacity evidence would let a misconfigured on-sale proof
  report a clean sellout curve while selling nothing.
- `make browser` is not required — there is no web surface in this ticket. Issuance is API-only; a
  back-office UI is out of scope.
- **Evidence for reading R2, and its limits (TKT-543).** Executed against PostgreSQL 18.4 with
  forced schedules (`pg_blocking_pids` handshakes, database-clock boundaries): the finalize race and
  the draw-down race each end with 1 confirmed unit for a cap of 1, and with 2 when
  `codeRedeemedQuantity` goes back to `consumingClaims`; 8 of 8 repeats each. The draw-down
  schedule depends on reaching the child INSERT within a 2 s margin, so its test rebuilds the
  fixture, at most three times, only when the draw-down refused with the source's expiry committed
  and the claim's `updated_at` (that transaction's start) is at or after `expires_at`. That rule
  assumes the database clock does not step backwards inside one transaction; a contradictory
  sample fails the test. Not tested: a cap edit while a finalize or draw-down is queued.
- **Not fixed here.** TKT-380 probe W: an admission that waits on the code row lock across the
  code's `closes_at` is admitted after the window closed, because the window is read in the locking
  statement. ADR-084 §4 records the fix for promotions; the presale path keeps today's statement.
