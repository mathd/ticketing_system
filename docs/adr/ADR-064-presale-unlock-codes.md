# ADR-064: Presale unlock codes

Date: 2026-08-10

## Status

Accepted (TKT-239; epic TKT-17, US-CH5)

Supersedes nothing. Extends [ADR-024](./ADR-024-channel-allocations.md) (channel allocations) and
[ADR-054](./ADR-054-per-channel-sales-windows.md) (sales windows).

**Usage counts until a terminal write, and a cap refusal reclaims across pools (2026-10-13,
TKT-543).** Amended below in §2, §3, §4 and §Consequences. A code capped at N could end with more
than N confirmed units: an admission on one pool left out an expired, unswept hold on another
pool, and a finalize that had queued on that pool before the expiry then kept the hold
([TKT-380 evidence](../evidence/TKT-380/README.md), probe 1, S1). The admission count now follows
reading R2 of [ADR-084](./ADR-084-promotion-storage-and-redemption-contract.md) §3: an expired held
claim counts until a terminal write commits. So that an expired hold on a quiet pool does not lock
its code out of every other pool, a cap refusal first tries to reclaim the code's expired claims on
other pools, and decides once more if any of that committed. The refusal stays uniform.

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

**The cross-pool cleanup adds no lock order (TKT-543).** The cleanup of §3 takes pool and claim
locks and never the code lock. It starts only after the refused admission has rolled back, so it
holds no lock when it waits for one. It locks one foreign pool per transaction, commits that
transaction before it opens the next, and goes in pool-id order. A finalize or a draw-down that
holds a foreign pool when the cleanup arrives keeps it; the cleanup waits, then sees the committed
state. The cleanup must not run inside the admission transaction: that transaction holds its own
pool and the code, and a mirror admission on the other pool would wait for the code while holding
the pool the cleanup wants. PostgreSQL did not detect that cycle (one side waits in Go, not on a
lock). With the cleanup moved before the rollback, `TestPresaleMirrorAdmissionsReleaseLocksBeforeCleanup`
did not finish within 10 s when the cleanup's lock timeout was raised to 20 s, and it passed in
2.2 s with the 2 s timeout, which is why the test raises it.

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
more unit: a code capped at 1 sold 2. `TestPresaleCapCountsExpiredHoldWhileFinalizeWaits` and
`TestPresaleCapCountsReservationWhileDrawDownInsertWaits` (the draw-down child path) force that
schedule; both measured 2 confirmed units for a cap of 1 on the old predicate and 1 on the new.

**What is still shared, and what now differs on purpose.**

- The quantity: `consumedQuantity`, net of refund returns, in both counts. A refund returns a
  redemption exactly as it returns a seat.
- Channel and pool accounting keep `consumingClaims`: an expired held claim stops counting there
  as soon as `expires_at` passes.
- The admission count keeps an expired held claim until a **terminal write commits**: a
  `sweepExpired` whose transaction commits, the replay-expiry branch of `CreateHold`, the expiry
  branch of a refused draw-down, `Transition`'s expiry branch when it commits, or a release.
  Passing `expires_at` alone returns nothing. After a terminal write no path can finalize the
  claim, which is why this closes the race.
- The **operator** read (`PresaleCodeStatuses`) keeps `consumingClaims`. It leaves out expired held
  claims that no terminal write has reached, so it is lower than the admission count by the
  expired, unswept quantity. That quantity is only **potentially** reclaimable: the cleanup below
  reclaims it unless a finalize or draw-down that queued on its pool before the expiry gets there
  first and keeps it, as `TestPresaleCapCountsExpiredHoldWhileFinalizeWaits` shows. R2 in the
  operator read would show a code as exhausted for every abandoned cart on a quiet or archived
  pool, and an operator who raised the cap on that reading would oversell once the cleanup ran.
  The cost of the choice is the opposite error, for a shorter time: by the two predicates (no test
  asserts it), while a queued finalize or draw-down keeps an expired claim, the operator read shows
  headroom that admission cannot use. A later operator
  surface (TKT-309) can add a reclaimable field. `TestPresaleUsageCountsUntilTerminalWrite`
  asserts both counts, before and after a terminal write, for each claim state.

**The cross-pool cleanup (TKT-543).** R2 alone would let an expired hold on a quiet pool block its
code on every other pool until some write reached that pool, and ADR-010 promises that expired
capacity is reclaimed by the next mutation without a sweeper. So an admission refused by the code
**count**, and by no other cause, does this in both `CreateHold` and `PlaceGroupReservation`:

1. Its transaction rolls back, and its own-pool sweep with it.
2. Discovery, outside any transaction (`expiredPresalePoolsSQL`): the pools other than its own
   with an expired held claim citing exactly this organizer, channel and code, and the quantity
   on each. It is backed by `claims_presale_usage`. It decides which pools the cleanup locks.
3. If nothing was found, or `redeemed − reclaimable + qty > cap`, the refusal stands at once: no
   cleanup could make the request fit, and a genuinely exhausted code must not make every refused
   request of an on-sale lock the pools that hold its expired claims.
4. Otherwise, one transaction per pool, in pool-id order: a 2 s `lock_timeout` (`SET LOCAL`); lock the
   pool with a statement scoped to the organizer and marked `/* presale-cleanup pool lock */`, and
   skip the pool if no row comes back; expire the due held claims of that pool that cite exactly
   this organizer, channel and code, writing each `claim_history` row exactly as `sweepExpired`
   does (`presaleCleanupExpireSQL`); commit with a plain commit that invalidates **no**
   availability cache (D16, below). There is no offering guard: an archived or closed pool
   refuses new holds, so no hold sweeps it. An archived pool is never swept again; a closed one
   only after it reopens or a staff capacity adjustment.
5. If at least one of those transactions committed, the whole admission runs once more, from the
   idempotency replay onward, with every guard. There is no second cleanup. If none committed, the
   request gets the uniform refusal at once. That is a chosen policy, to keep a refusal whose
   cleanup changed nothing at one admission. It does not claim a retry would have failed:
   something concurrent, such as another terminal write or an operator raising the cap, could have
   made it succeed, and the next request sees that change.

The cleanup lives in its own file, `presale_cleanup.go`, which commits with `tx.Commit()` and is
exempt as a whole file from `TestAvailabilityMutationsUseInvalidatingCommit`.

**Why not `sweepExpired` (D13; blind review finding 2).** `sweepExpired` expires *every* due claim of
the pool. A replay of a GA hold answers 409 when the hold is held and past its expiry, and 200 when
it is already expired. A cleanup that expired an unrelated hold would therefore let that hold's
owner replay it and learn that a code they had guessed is valid and exhausted: a refusal
distinguishable by status and body, which §4 forbids. Scoped to the code, the cleanup's writes change
only claims citing that code. Two more things `sweepExpired` does are left out. `reconcileCapacity`
would settle a draining cut at a moment a code request chose, in a pool the request does not sell
from; it reads demand through `liveClaims`, which already leaves out every due hold, so skipping it
changes no count. The cut settles at the next write on that pool that runs `reconcileCapacity` (a new
hold, placement or capacity adjustment, which sweep first; a release; a refund return; an expiry
that `Transition` or a draw-down commits; not a finalize or a confirm), as it would have without the
cleanup, and reads never wait for it because `effectiveCapacity` re-derives the clamp.
`releaseSeatsForTerminal` has nothing to do: only GA claims can cite a code.
`TestPresaleCleanupTouchesOnlyClaimsCitingTheRequestedCode` asserts all of this, and compares the
expiry row with one a real `sweepExpired` writes. The replay inconsistency itself (200 for an expired
hold, 409 for an unswept one) predates this ticket. ADR-010 says a replay after release or expiry
returns the terminal claim, so changing either answer changes that contract for every caller; it is
recorded as a separate ticket.

**Why no cache invalidation (D16; review pass 2 finding 1).** Committing through
`commitAvailability` evicted every cached variant of the foreign pool's availability, and the public
availability read shows an eviction: a fresh `Age` header and body. Only a valid, exhausted code
triggers a cleanup, so the eviction was the same code oracle again. No answer needs it. The write
flips only held claims that are already due, and every availability answer counts held claims
through `liveClaims` (directly or through `consumingClaims`), which already leaves out a due hold.
That was checked read by read: `Availability`, `SeatOccupancy`, `StaffAvailability` and its channel
rows, `reservedForChannelsSQL`, the sold-out decision of `CreateHold`, and `effectiveCapacity`; the
two caches that receive invalidations wrap `Availability` and `SeatOccupancy`. So a fresh read gives
the same answer before and after the write, and a cached one is no staler than it would be without
the cleanup (the cache TTL bounds it, as before). `TestPresaleAdmissionReclaimsExpiredCodeUsageAcrossPools`
asserts that a cleanup which reclaims P2 triggers no invalidation of P2. A future availability read
that counted a due held claim would make this exemption wrong; it would have to invalidate here.

The cleanup waits for a busy pool rather than skipping it: the pools that matter are held by a
finalize or a draw-down, and neither sweeps. **One failed pool does not end the cleanup (D14; blind
review finding 1).** An error on one pool, its lock timeout included, ends that pool's transaction
only, and the next pool is tried. Stopping at the first failure would let a busy pool that sorts
first keep a quiet pool's expired usage counted for as long as it stays busy, and would give up a
retry that the pools already cleaned make fit (`TestPresaleCleanupContinuesPastALockedPool`, both
orders). A discovery error ends the cleanup with the uniform refusal. Terminal writes already
committed stand on their own. A group placement's second attempt judges its explicit expiry again
on `clock_timestamp()`, so a placement whose `expires_at` passed during the cleanup ends in
`ErrConflict`, as a placement queued past its expiry already does.

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

**The cross-pool cleanup does not change the refusal (TKT-543).** Inside the store the exhausted
cause is an unexported signal, so that the two admission paths can tell it from the other four
and reclaim only for it. Each path turns it into `ErrPresaleCodeInvalid` in one place before it
returns, and the signal does not wrap the public sentinel, so a path that forgot would answer 500
and `TestPresaleRefusalsStayUniformAfterCrossPoolCleanup` would fail on the exhausted cause. The
cleanup's own errors, a lock timeout included, are never returned: they would be a 500 that only a
valid, exhausted code can produce. The cleanup runs after the window and seller checks
(`TestPresaleCleanupStaysBehindTheWindowAndSellerChecks`), so a caller who may not sell never
reaches it. Its writes change only claims that cite the requested code (§3, D13), they change no
availability answer, and they invalidate no cache (§3, D16). Through a status or a body they are
therefore visible only to a caller who holds a claim citing that code (by replaying it) or who
presents the code. The pool locks the cleanup takes show only as latency.

What it does change is **latency**, which this section already does not claim. An exhausted-code
refusal now also costs a discovery query; when expired usage could cover the shortfall, it costs one
cleanup transaction per discovered pool, each of which can wait up to 2 s for a busy pool, and a
second admission when at least one of them committed.

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
- **Cost of a cap refusal (TKT-543).** At most two admission transactions, one discovery query, and
  one cleanup transaction per discovered pool. One retry bounds the work, not the wait: each
  cleanup can wait up to 2 s for a busy pool, and since one failed pool does not end the cleanup
  (D14), the request waits for all of them. A refusal with no reclaimable usage, or with too little
  to cover the shortfall, adds only the discovery query. Successful admissions pay nothing.
- **A failed cleanup is silent (TKT-543).** The store has no logger, and the error is not returned
  (§4). It costs liveness, not correctness: the expired claim keeps counting until the next cleanup
  or sweep reaches its pool. A persistent failure would show only as a code that refuses with
  usage the operator read does not report.
- **Evidence and its limits (TKT-543).** Executed against PostgreSQL 18.4 with forced schedules
  (`pg_blocking_pids` handshakes, database-clock boundaries): the finalize and draw-down races, the
  quiet-pool reclamation (open, archived and closed foreign pool) with no cache invalidation of the
  cleaned pool, mirror admissions, discovery scope (end to end, by holding each wrong pool locked
  and watching for the cleanup's wait), the scope of the cleanup's write within a pool and the
  shape of its history rows, a locked
  pool sorting before and after a quiet one, the reclaimable-quantity bound, the lock timeout, and
  precedence behind the window and the seller; the wire body on both admission endpoints through
  the validated router. Each has a mutation it turns red, and each forced schedule passed 8 of 8
  repeats. The draw-down schedule depends on reaching the child INSERT within a 2 s margin, so its
  test rebuilds the fixture, at most three times, when that reach condition is missed; once reached,
  its cap assertions always run. The organizer predicate of the cleanup's write has no test: the
  store writes into a pool only claims of that pool's organizer, so no test can build the row it
  would exclude. In every run, the cleanup
  queued behind the waiting finalize or draw-down and got the pool after it. That queue order is
  observed PostgreSQL behaviour, not a documented guarantee. If it changed, the cleanup would expire
  the hold first and the finalize would be refused (`Transition` refuses a finalize of an `expired`
  claim): the cap would still hold, with the opposite winner. That is argued from the code, not
  executed. Not tested: more than two concurrent admissions racing through the cleanup, and a cap or
  window edit during a cleanup.
- **Not fixed here.** TKT-380 probe W: an admission that waits on the code row lock across the
  code's `closes_at` is admitted after the window closed, because the window is read in the locking
  statement. ADR-084 §4 records the fix for promotions; the presale path keeps today's statement.
