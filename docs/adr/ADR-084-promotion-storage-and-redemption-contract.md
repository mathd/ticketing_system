# ADR-084: Promotion storage and redemption contract

Date: 2026-10-13

## Status

Proposed (TKT-380, the S0 spike of epic TKT-7).

**Owner decision, 2026-10-13: reading R2** of "one idempotent terminal transition" (§3; open
question 6 is closed). An expired held claim counts against its code until a terminal write
commits. §3, §4, §7, §8, §11 and Consequences state the R2 contract. The other open questions stay
open, so the ADR stays Proposed.

Nothing in this ADR is implemented. The three probes in
[docs/evidence/TKT-380](../evidence/TKT-380/README.md) ran against a temporary copy of the
Inventory module and a disposable PostgreSQL. They changed no shipped code. Every statement below
is either read from the code at `601d50be` (with `file:line`), observed in a probe (with the
evidence file), or marked as an inference.

## Context

Epic TKT-7 adds buyer promotion codes: single-use and multi-use codes, percentage and fixed
reductions, redemption caps and validity windows. The stories that depend on this ADR are TKT-381
(apply one code to a GA reservation), TKT-382 (cap and window under concurrency), TKT-383 (refund
and exchange at paid face) and TKT-493 (show the promotion in the journal-backed breakdown). TKT-384
(staff comps) is separate.

This ADR answers three questions:

1. Which service owns the code definition, the reservation discount snapshot and the cap count.
2. How the cap is locked and counted under hold expiry, release, finalize, confirm, refund,
   exchange, draw-down, conversion and crash or replay (COS-1).
3. How percentage and fixed reductions compose with the catalog price winner, the fees and the
   journal, with one testable example per rule (COS-2).

### Owner policy that is already settled

The owner settled these rules in
[backlog-decisions-2026-10-10](../product/backlog-decisions-2026-10-10.md) (sections *Promotion
price and fee basis*, *Promotion cap and corrections*, *Comps* and *Waitlist policy*). This ADR
implements them. It does not decide them again.

- One promotion per reservation. It applies after the existing catalog/channel price winner. No
  stacking.
- Paid unit face = `floor(unit face × (10000 − bps) / 10000)`, per ticket, in checked integer
  arithmetic. Reduction = original face − paid face. A fixed reduction is per ticket and clamps at
  the unit face.
- `reservations.unit_amount` is the paid face per ticket. `reservations.face_value_amount` is
  quantity × paid unit. The pre-discount face stays in separate snapshot fields.
- Percentage fees use the paid face with the ADR-046 per-ticket floor. Fixed fees keep their
  configured amount and incidence.
- A code never changes channel eligibility or presale entitlement.
- The cap counts discounted ticket **units**, not orders. Live and finalizing holds reserve units.
  Completed sales consume them. Expired or released unconfirmed holds return them through one
  idempotent terminal transition. A refund does not return a consumed unit.
- A refund returns the original paid face. An exchange credits the stored source face and prices
  the target without a promotion. The source redemption stays consumed. Fee reversal is TKT-407.
- Staff comps (TKT-384) are a staff operation at price 0, not a 100% promotion.
- The first waitlist offer checkout takes no promotion code.
- TKT-382 decision D1: "TKT-380 must prove cross-pool cap locking, crash recovery and exact-once
  terminal release."

### What exists today

| Mechanism | Current behaviour (read at `601d50be`) |
|---|---|
| Presale code definition | Inventory stores codes keyed by `(organizer_id, channel_code, code)` with a cap and a half-open window (`services/inventory/internal/store/migrations/0014_presale_unlock_codes.sql:40`). A claim cites its code in plain text with no foreign key (`:74`). A partial index backs the usage count (`:96`). |
| Cross-pool serialization | `CreateHold` locks its pool row (`services/inventory/internal/store/store.go:524`). `redeemPresaleCode` then locks the code row and reads the window (`services/inventory/internal/store/presale_codes.go:87`). It reads usage in a separate statement under that lock (`:110`). The pool lock alone cannot serialize other pools. `TestPresaleCodeCapHoldsAcrossSlots` proves the cap across three pools (`services/inventory/internal/store/presale_codes_smoke_test.go:165`). |
| Usage count | `codeRedeemedQuantity` sums `consumedQuantity` over `consumingClaims` (`services/inventory/internal/store/channel_allocations.go:78`). `consumingClaims` is confirmed or live (`:60`). `consumedQuantity` subtracts `returned_quantity` from a confirmed claim (`:89`). So a presale refund gives the code its unit back. Promotion policy says the opposite. |
| Liveness | `liveClaims` is held-and-unexpired or finalizing (`store.go:244`). It compares with `now()`, which is transaction-start time. Finalizing claims never expire. `sweepExpired` flips due held claims of ONE pool, inside the caller's pool-locked transaction (`store.go:259`). |
| Time bases | A grant anchors `expires_at` on `clock_timestamp()` (`store.go:778`). The code window and the channel window use `clock_timestamp()` (`channel_allocations.go:43`, `:54`). `Transition` reads `now()` with the claim (`store.go:829`) and `expired()` judges on it (`store.go:246`). Two tests pin this choice for code-less claims on purpose (`services/inventory/internal/store/buyer_ttl_clock_smoke_test.go:452`, `:541`). |
| Transitions | `Transition` locks pool, then claim (`store.go:825`, `:829`). A repeated target is satisfied (`:854`). Finalize against confirmed is satisfied (`:859`). Release against expired is satisfied (`:868`). Confirm adds to the pool's confirmed quantity (`:885`). |
| Draw-down | The child inherits the source's code. The source decrements by the drawn quantity, or turns `released` on a whole draw (`services/inventory/internal/store/reservations.go:323`, `:329`). The source's expiry is judged on `clock_timestamp()` under the pool and claim locks, with no code lock (`reservations.go:268`). |
| Conversion | An operational hold carries no code. The converted buyer child is inserted without one (`services/inventory/internal/store/operational.go:345`). |
| Refund capacity return | Inventory adds to `returned_quantity` and keeps the claim `confirmed` (`services/inventory/internal/store/refund_returns.go:148`, `:160`). |
| Catalog price winner | Catalog resolves the unit price in a read transaction and calls the pure selector (`services/catalog/internal/store/pricing_postgres.go:127`, `:179`; `services/catalog/internal/store/pricing.go:234`, `:424`). |
| Commerce snapshot | Commerce resolves price and fees before the hold (`services/commerce/internal/api/server.go:941`, `:955`). The GA hold sends the resolved unit price as `unit_amount` (`server.go:993`). After the hold it computes the face (`:1079`) and the fees on that unit (`:1080`), builds the fee snapshot (`:1095`) and inserts the reservation (`:1126`). |
| Fee arithmetic | A percentage fee floors per ticket, then multiplies by quantity (`services/commerce/internal/api/catalog_fees.go:151`, `:212`). Incidence decides which fees enter the buyer total (`:178`). |
| Refund and exchange basis | Refund binding reads `unit_amount` and multiplies by the refunded quantity (`services/commerce/internal/store/refunds.go:121`, `:178`). The exchange source loader reads the aggregate `face_value_amount` (`services/commerce/internal/store/exchanges.go:200`). An exchange carries the source's fee to the target (`services/commerce/internal/api/exchanges.go:809`). |
| Journal | Commerce submits order facts with the gross amount and `order_id` (`server.go:1365`). Payments accepts only `order_id` as a payload key (`services/payments/internal/store/store.go:239`), and the payload is part of the canonical bytes (`store.go:125`). |

### Constraints

- ADR-002 gives Catalog the rule definitions and Commerce the "pricing/fee/promo evaluation"
  ([ADR-002](./ADR-002-services-from-day-one.md), the ownership table). ADR-036 §6 reads that
  Commerce row as composition, not resolution. ADR-046 §5 says "catalog resolves, commerce
  computes".
- Inventory has no read path to Catalog's tables. The claim path must not make a cross-service read
  (0014 migration comment; ADR-010).
- A cap must be judged atomically with the claim it counts. Only Inventory has that claim.

## Possible Solutions

### A. Ownership: four options

**Hard constraint for every option:** the window and the cap are judged in the same transaction as
the claim, under the Inventory lock. An option that cannot do this needs a distributed protocol for
the cap.

| | Terms (kind, value, currency, versions) | Code identity, cap, window | Usage count | Meets the hard constraint |
|---|---|---|---|---|
| **A. Inventory owns all** | Inventory | Inventory | Inventory, from claims | Yes |
| **B. Commerce owns definition and count** | Commerce | Commerce | Commerce | No |
| **C. Catalog owns all** | Catalog | Catalog | Catalog | No |
| **H. Hybrid** | Catalog | Inventory | Inventory, from claims | Yes |

- **Option A — Inventory owns definitions and redemption.**
    - Pros: one service and one write per code definition. Inventory can check under the lock that
      the version a hold cites is the current one. It reuses the presale shape (ADR-064).
    - Cons: Inventory stores money terms. The 0014 migration keeps the presale code table off the
      money path on purpose ("emphatically not"). Commerce needs the terms before the hold (to compute the paid
      `unit_amount` the hold carries), so it must add a read of Inventory, the contention hot path,
      before every promoted hold. It puts a rule definition outside Catalog, against ADR-002 and
      ADR-046 §5.
- **Option B — Commerce owns definitions and counting.**
    - Pros: Commerce already owns composition and the snapshot.
    - Cons: Commerce cannot change an Inventory claim in its own transaction. Hold expiry is lazy in
      Inventory (`store.go:244`), so Commerce cannot see it without a call. Every expiry, release,
      draw-down and crash needs a reconciliation protocol across two databases. The cap is
      consistent only after that protocol converges. This fails the hard constraint.
- **Option C — Catalog owns everything, including the count.**
    - Pros: definitions sit beside price and fee rules.
    - Cons: Catalog has no claims. Counting there needs a cross-service write on the claim path, or
      a second lifecycle that copies every Inventory transition. Same failure as B, with one more
      hop.
- **Option H — Hybrid.** Catalog owns the promotion: a stable id, immutable versions of its terms,
  and the mapping from a code string to the promotion. Inventory owns the code's redemption
  control: the code row with its promotion id, its cap in ticket units and its window, plus the
  claim citation and the usage count.
    - Pros: matches ADR-002, ADR-036 §6 and ADR-046 §5 ("catalog resolves, commerce computes");
      Inventory adds a count, as it did for presale codes (ADR-064). Commerce resolves the terms in
      the same pre-hold Catalog read it already makes for price and fees (`server.go:941`, `:955`).
      Inventory never stores a money term.
    - Cons: a code exists in two services. The two definitions must be provisioned together. A code
      present on one side only must fail closed with the uniform refusal. Inventory cannot check
      that a cited version is Catalog's latest; a version published between the Commerce read and
      the hold is applied as the old version. That is the same race a Catalog price edit has today
      (inference from the same pre-hold read order, `server.go:941` before `:993`).

### B. Usage model: two options

- **(i) Claim-row citation.** The claim carries the code, the promotion id and the version, as a
  claim carries a presale code today (0014 `:74`). Usage = the sum of `quantity` over claims
  citing the code whose status is `confirmed` (NOT net of `returned_quantity`), `finalizing` or
  `held`. Expiry alone does not remove a `held` claim from the sum; that is reading R2 (§3).
    - Pros: claim and redemption commit in one row, so they cannot disagree. The claim's own
      idempotent terminal transition is the one terminal release. Draw-down conserves units without
      new code: the source decrements and the child inherits the citation (`reservations.go:323`,
      `:329`). Under reading R2 (the owner's choice, §3), an expired claim returns its units at
      its terminal write.
    - Cons: a separate usage expression from the presale one, because refunds must not return
      units. An index is needed, as for presale (0014 `:96`).
- **(ii) Separate redemption records.** A table of redemptions with its own status.
    - Pros: an explicit audit row per redemption.
    - Cons: every path that writes a claim must also write the record in the same transaction. That
      is at least `CreateHold`, its replay-expiry branch, `Transition`, draw-down, and the seven
      `sweepExpired` call sites. A missed path makes the record and the claim disagree: under R2,
      every terminal write of the claim must also end the record in the same transaction.

### C. Time rule for paths that keep usage: four options

Probe 1 shows the problem on today's presale path. A finalize that starts before a hold expires and
waits on its pool lock past expiry judges the hold alive on its transaction-start `now()`. An
admission on another pool, after expiry, already left that hold out of its count. Both commit. The
cap overshoots (see Consequences, *Presale races*).

- **T1. Keep transaction-start time (today).** Observed to overshoot (probe 1, 6 of 6 runs).
- **T2. The admission writes `expired` on the foreign claims it leaves out.** It must lock claims in
  other pools after the code lock. That breaks the lock order and can deadlock with a finalize that
  holds the claim and waits for the code (inference).
- **T3. Count expired-but-unswept held claims until a terminal write.** Safe for the cap. Its cost
  is liveness, not correctness: on a quiet pool, expired holds block the code's units until a sweep
  or other cleanup writes the terminal state. This is reading R2, which the owner chose (§3).
- **T4. Every path in a named locking list (§4) takes the code lock and judges expiry on a fresh
  time read after it.** Probe 2 shows it closes the race in all three forced schedules. It needs
  reading R1 (§3), which the owner did not choose.

**Chosen: T3 for usage.** The fresh time read after the code lock stays, for the window check only
(§4).

## Decision

### 1. Ownership: Option H, the hybrid

We adopt the hybrid. The comparison does not separate A and H on the concurrency contract: the
lock, the window, the cap and the usage count are the same in both. It separates them only on
where the terms live, and there the accepted ADRs (002, 036 §6, 046 §5) point to Catalog. B and C
fail the hard constraint. This follows the plan's default rule for a non-decisive comparison
(TKT-380 D10).

| Fact | Owner | Note |
|---|---|---|
| Promotion id, immutable versions, kind (percentage in bps, or fixed amount and currency), code → promotion mapping | Catalog | Resolved before the hold, in the read that already resolves price and fees. |
| Code row: promotion id, cap in ticket units, window `[opens_at, closes_at)` | Inventory | Read in a separate statement after the code row lock is held, on `clock_timestamp()` (§4). Not in the locking statement, as `redeemPresaleCode` does today. |
| Usage count and redemption state | Inventory | Derived from claims, model (i). |
| Composition, rounding, the reservation snapshot, refund and exchange amounts | Commerce | Never repriced from a current definition. |
| Journal facts | Payments | Unchanged. |

How the two code definitions are provisioned together (one staff write that calls both, or an
event that Inventory consumes) is open; TKT-382 designs it. The rule is fixed here: a code that
exists on one side only (a Catalog mapping without an Inventory row, or the reverse) fails closed
with the uniform refusal.

### 2. The code is the capped and locked entity

- One code row, with its cap in ticket units and its window, maps to one promotion. A batch of
  single-use codes is many rows.
- Caps across many codes of one campaign are out of scope. They would need a second lock (open
  question).
- "Single-use" is not defined by policy in unit terms. The default here is the strictest reading: a
  cap of **1 ticket unit** (open question).
- **Admission is all-or-nothing.** If the remaining units are fewer than the requested quantity,
  the code is refused for the whole request. There is no partial application. One reservation has
  one snapshot and one paid `unit_amount`, so it cannot mix discounted and undiscounted tickets.

### 3. Usage model (i): the claim cites the code

Usage of a code is the sum of `quantity` over the claims that cite the code and match the
**promotion usage predicate**:

    status = 'confirmed'      -- returned_quantity is NOT subtracted
    OR status = 'finalizing'
    OR status = 'held'        -- reading R2: expiry alone does not end usage

The predicate does not judge expiry, so it needs no decision time. An expired `held` claim counts
until a terminal write commits (below).

This is a new predicate and a new expression. It must not reuse `consumedQuantity`, and it must not
use `liveClaims`: `liveClaims` judges on transaction-start `now()` (`store.go:244`). `liveClaims`
stays unchanged for pool capacity. A test that refunds and rebuys must fail if someone swaps
`consumedQuantity` in.

Paths that write usage, and what each one does to it. The "Code lock" column is the locking list
of §4; this table and §4 must stay identical.

| Path | Effect on usage | Code lock through commit |
|---|---|---|
| Admission: `CreateHold` with a code (`store.go:503`) | Creates `+qty` | **Yes** |
| Finalize from `held`: `Transition` to `finalizing` | Keeps it | No: it never adds usage |
| Direct confirm from `held`: `Transition` to `confirmed` (zero-total path) | Keeps it, permanently | No: it never adds usage |
| Draw-down child (`reservations.go:268`–`:337`) | A successful draw-down moves units from source to child; total constant. A draw-down whose source has expired commits the source's expiry and refuses (`:281`–`:294`): a terminal write, listed in the next rows | No: the total is constant (the later slice confirms this) |
| Group placement with a code (`reservations.go:209`) | Creates `+qty` | **Yes** (later slice) |
| Conversion (`operational.go:345`) | None in the first slice: it applies no promotion | **Yes**, if a later design lets it carry a code |
| Cap or window edit of a code | None: an edit never releases usage | **Yes**, `FOR UPDATE` |
| Confirm from `finalizing`: `Transition` to `confirmed` | Keeps it, permanently | No: the claim already counts and never expires |
| Expiry: the claim passes `expires_at` | None. The claim counts until a terminal write | No: it is not a write |
| `sweepExpired` (`store.go:259`; called at `store.go:597`, `reservations.go:105`, `operational.go:177`, `capacity.go:60`, `seat_claims.go:754`, `:1165`, `:1652`), the replay-expiry branch of `CreateHold` (`store.go:575`), the expiry branch of a refused draw-down (`reservations.go:281`–`:294`) and the expiry branch of `Transition` (`store.go:839`) | Ends it when the `expired` write commits: a terminal write. `sweepExpired` writes inside its caller's transaction, so its write is terminal only if the caller commits: an admission that sweeps and is then refused (for example by the code count, `store.go:683`, deferred rollback `:516`) rolls the sweep back, and the claim keeps counting (probe 3 mutation, R2L: `h_after_b=held`). The replay-expiry and draw-down expiry branches commit their write before they refuse. `Transition`'s expiry branch commits it only on a release; on a refused finalize or direct confirm it returns `ErrConflict` and its write rolls back (`store.go:882`–`:883`, deferred rollback `:824`), so the claim counts until another terminal write. That costs liveness, not correctness | No; `sweepExpired` **never** |
| Explicit release: `Transition` to `released` (`store.go:891`) | Ends it: the terminal transition for release | No: it never adds usage |
| Refund capacity return (`refund_returns.go:148`) | None | No: it never adds usage |

**One idempotent terminal transition: reading R2 (owner decision, 2026-10-13).** The owner policy
says expired or released unconfirmed holds return their units "through one idempotent terminal
transition" (backlog-decisions, *Promotion cap and corrections*). The owner chose this reading:

- **R2, terminal write.** An expired `held` claim keeps counting until a terminal write commits:
  `sweepExpired` when its caller commits, the replay-expiry branch, a refused draw-down's expiry
  branch, `Transition`'s expiry branch when it commits, or explicit release. Explicit release is the existing guarded release write (`store.go:891`); a
  repeat, and a release after expiry, are satisfied (`store.go:854`, `:868`). Each terminal write is
  a guarded status change, so a repeat returns nothing.
- R2 does not depend on a monotonic clock for cap safety: a clock step can only move a terminal
  write earlier or later; the claim counts until that write commits, and after it no path can
  finalize the claim (`store.go:882`–`:883`). A terminal write only decreases usage, so it needs
  **no** code lock: a concurrent admission sees either the old row, counts it and refuses
  (conservative), or the committed terminal row. The existing pool and claim locks serialize the
  write against a finalize.
- R2 removes the cross-pool overshoot of probe 1 S1 by itself: the expired but unswept claim H
  still counts, so the admission on P1 is refused and the queued finalize keeps the unit. Probe 3
  executed this on today's presale path with only the usage predicate changed (Consequences,
  *Reading R2 executed*).

**Rejected: R1, logical expiry.** Units would return at the first code-locked decision after
expiry, with no write. That needs finalize and direct confirm on the locking list and a
non-decreasing decision time (a high-water column on the code row), because a backward clock step
could otherwise let a queued finalize revive a claim that an admission already left out. That guard
was argued, not tested. Probe 2 prototyped the R1 lock and time rule; it stays as evidence for the
rejected design.

**The costs of R2.** **Cleanup:** a code's units stay blocked on a quiet pool until something
writes the terminal state there. Today every `sweepExpired` call is lazy and pool-local, inside a
claim transaction on that pool (the seven call sites above); no background sweeper exists. So R2
needs a cleanup design for liveness (not for correctness); TKT-382 owns it. **Lock order:** an
admission may sweep its own pool before it takes the code lock, as `CreateHold` already does (pool
lock `store.go:524`, `sweepExpired` `:597`, then `redeemPresaleCode` `:682`); probe 3 schedule R2L
observed this. It cannot clean up foreign pools that it discovers under the code lock, because that
would take pool and claim locks after the code lock, which §4 forbids; that cleanup must run in its
own transactions.

**First slice.** Promotions apply to GA reservation checkout only. Seated and group paths refuse a
promotion code. Operational conversion applies none. A later slice that adds a path must design it
against this table.

### 4. Lock order and the fresh-time invariant

**Lock order:** pool → claim → presale code → promotion code. After the promotion code lock is held,
a transaction takes no pool lock and no claim lock. When a path needs several locks of one kind, it
takes them in sorted identity order. `sweepExpired` never takes a promotion code lock.

**The locking list.** These paths take the promotion code lock and hold it until they commit. They
are the paths that add usage, and the edit:

1. admission;
2. group placement with a code (later slice);
3. conversion, if a later design lets it carry a code (in the first slice it applies none);
4. a cap or window edit.

These paths do not take it, because none of them adds usage under R2: finalize and direct confirm
from `held` (the claim already counts, expired or not); confirm from `finalizing`; the draw-down
child (the total is constant; the later slice confirms this); release, expiry, `sweepExpired`, the
replay-expiry and `Transition` expiry branches; and the refund capacity return. §3's table and §11
use this same list.

**Fresh-time invariant.** Every path in the locking list takes the lock **whether or not the code
has a cap**, because a cap can be added while the transaction is open: an uncapped admission that
skipped the lock could commit after an edit set a cap and after a second admission counted usage
without it (reasoning; probe 2 S3 observed the same overshoot for an uncapped finalize under the R1
prototype, not for an admission). After the lock is held, the path reads the cap, the window and
the usage in **separate statements**. It judges the window on `clock_timestamp()` read in that
statement, never on transaction-start `now()`. The usage predicate (§3) judges no time.

**Read after the lock, not in the locking statement.** Today `redeemPresaleCode` computes the
window in the target list of the `SELECT … FOR UPDATE` itself (`presale_codes.go:87`–`:90`). The
expression uses `clock_timestamp()`, not `now()` (`channel_allocations.go:54`), so the time base is
correct. The problem is when it is evaluated. Probe 1 schedule W held the code row with a
`FOR UPDATE` that did not update it, across the window's `closes_at`. An admission that started
before `closes_at` waited on that lock and was admitted after the window closed: 6 of 6 runs. The
same probe with the window read moved to a separate statement after the lock refused it: 6 of 6
runs ([evidence](../evidence/TKT-380/README.md)). The explanation is an inference that matches the
observation: PostgreSQL computes the target list before it waits for the row lock, and computes it
again only if the row was updated meanwhile.

**Cap and window edits.** An edit of a code's cap or window takes the code row `FOR UPDATE` (an
`UPDATE` does this). So it waits for every open transaction that holds the code lock, and every
later path sees the edit. Under the lock, the edit reads usage on `clock_timestamp()` in a separate
statement and returns it to the operator. A cap below usage is accepted (§6): it refuses new
admissions and releases nothing.

Why it works (argument, then observation). Under the code lock, the paths that add usage are
serial, and each reads usage in a statement that starts after the previous one committed. Outside
the lock, a write can only keep usage (finalize, confirm, draw-down) or end it (a terminal write).
A claim that a terminal write ended cannot be finalized again (`store.go:882`–`:883`). So no write
outside the lock can make usage smaller and then larger: usage that existed when an admission read
it cannot grow before that admission commits. An admission is accepted only if the usage it read
plus its own quantity is within the cap (`presale_codes.go:115`), so the usage at its commit is at
most that sum. Probe 3 observed the cap hold on today's presale path with the R2
predicate in schedule S1 (6 of 6 runs, then 7 repeats), and observed it break with today's
predicate in its place (Consequences, *Reading R2 executed*). Probe 1 schedule W shows why the window read must
come after the lock.

This specializes promoted paths only. `liveClaims` and the shared capacity predicates of ADR-010
and ADR-024 stay unchanged.

A note on the evidence, not on the contract: probe 3 changed only the presale usage predicate. It
did not move the window read (probe 1b did that separately), and it did not test a cap edit under
R2. The contract above is the one rule for production.

### 5. Immutable versions, the hold fingerprint and `unit_amount`

- A promotion version never changes. An edit of terms creates a new version. A reservation keeps
  the version it was bound to.
- The Inventory hold fingerprint (`store.go:368`) adds the promotion code, id and version. They are
  appended only when present and length-framed, exactly as the presale code is (`store.go:395`), so
  a request without a promotion keeps its existing fingerprint bytes. TKT-381/TKT-382 must test both
  that compatibility and that two requests that differ ONLY in their promotion inputs do not
  collide. That is all the frames can give: two requests that already collide without them, and
  carry the same promotion inputs, still collide (ADR-064 §9). Each new field must be length-framed with its own marker, and replay
  must still compare the stored channel and the stored form of the currency, because the
  fingerprint alone is not injective (ADR-064 §9, TKT-510).
- The hold sends the **paid** unit face as `unit_amount`. The pre-discount face is not sent to
  Inventory. It lives in the Commerce snapshot.
- A version freezes the reduction terms only. It does not freeze the Catalog price winner or the
  fees.
- Inventory does not know Catalog's latest version. A version that Commerce resolved before the
  hold is accepted even if Catalog publishes a newer version before the hold commits. The
  reservation keeps the version it was admitted with.

**Recovery of the complete quote: re-resolution before the reservation exists (option b).** The
crash window is between the Inventory hold commit and the Commerce reservation insert
(`server.go:1126`). On a retry, Commerce finds no reservation (`server.go:800`), resolves price,
promotion and fees again (`:941`, `:955`), and sends the hold again. Then:

- **Guaranteed equal to the first attempt:** every field in the hold fingerprint
  (`store.go:368`): organizer, slot, ticket type, quantity, paid `unit_amount`, currency, channel,
  presale code, reseller, and the promotion code, id and version. If any of them differs, the retry
  is refused as key reuse (`store.go:572`). The refusal returns before any expiry write or sweep
  (`store.go:573`, then `:575`, `:597`) and commits nothing, so under R2 the first hold keeps its
  units until a terminal write ends it: a sweep of its pool, or the cleanup TKT-382 owns (§3).
- **May differ from the first attempt:** the original unit face and the reduction (for example
  1500 and 1501 both give paid 1350 at 1000 bps), the price provenance, the fee amounts, the fee
  snapshot and `total_amount`.
- **Why that is safe:** before the reservation row exists, no quote was persisted or returned to the
  buyer. The reservation is written from one resolution, so it is internally consistent. Checkout
  charges the persisted `total_amount` (`server.go:1917`, ADR-011) and settles from the persisted
  snapshot (`server.go:1912`), so the buyer pays what the reservation response showed. Inventory
  needs only the fingerprinted fields, and those are equal. The cap counts units, which are equal.
- This is today's behaviour for a Catalog price or fee edit in the same window (inference from the
  same order of calls). There is no "same result" guarantee for the unfingerprinted fields.

Rejected: (a) a durable quote before the hold needs a new Commerce representation (the reservation
row requires a hold id, `services/commerce/internal/store/migrations/0001_checkout.sql:3`). (c)
fingerprinting the original face and a fee digest would turn every price or fee edit in the crash
window into a refused retry and a leaked hold, to protect fields that no one has seen yet. Option
(b) adds nothing, so it is the most reversible: (a) or (c) can be added later.

### 6. Refusals and edit rules

- Unknown, wrong-scope, outside-window, exhausted, and one-side-only codes get **one** refusal,
  with one message and one body. This follows ADR-064 §4 and has the same limit: it does not stop
  a latency measurement. The operator read may tell the causes apart.
- A cap lowered below current usage refuses new admissions. It never releases existing usage.
- A window change applies to new admissions only. An admitted hold may finalize after the window
  closes.
- A code row is disabled, never deleted. Claims cite it without a foreign key, as for presale
  codes. The kept row keeps the code's identity and its lock target for the claims that cite it.

### 7. Redemption behaviour by lifecycle event (COS-1)

| Event | What happens to usage | Rule |
|---|---|---|
| Admission | `+qty`, all or nothing | Pool → (presale) → code lock; then cap, window and usage in separate statements; the window on fresh time (§4). |
| Hold expiry (lazy) | None until a terminal write commits; then `qty` returns once | R2 (§3). The terminal write is a committed `sweepExpired`, the replay-expiry branch, a refused draw-down's expiry branch, a committed `Transition` expiry branch, or a release. A sweep inside a refused admission rolls back. A repeat returns nothing. On a quiet pool the units stay blocked until cleanup runs (TKT-382). |
| Explicit release | Returns `qty`: the terminal transition for release (§3) | The guarded `Transition` to `released`. Repeats and release-after-expiry are satisfied (`store.go:854`, `:868`). No code lock. |
| Finalize from `held` | Unchanged; now non-expiring | No code lock. Judged on transaction-start `now()`, as for code-less claims (`store.go:829`). A finalize that waited on its pool lock past expiry still succeeds: the claim counted the whole time (probe 3 S1). |
| Confirm | Consumed permanently | From `held` (direct confirm): no code lock, as finalize. From `finalizing`: no code lock; the claim already counts and never expires. |
| Payment outcome unknown | Retained while `finalizing` | Released only on authoritative evidence (ADR-016). |
| Refund (full, partial, cancellation run) | Unchanged | Usage counts the confirmed quantity, not net of `returned_quantity`. Capacity return is separate (ADR-038 §8). |
| Exchange | Source unchanged; target carries no code | Owner policy. Exchange calls Inventory for the target hold (`exchanges.go:560`, `:572`, `:680`). |
| Draw-down | Total constant: a draw of 4 from 10 leaves 6 on the source and 4 on the child | No code lock under R2: the total is constant (the later slice confirms this). The child's terminal write returns its 4 to the code, not to the source (ADR-027). Not in the TKT-381 slice. |
| Conversion | None | Operational holds carry no code. Conversion applies no promotion in the first slice; a later design that lets it carry one puts it on the locking list. |
| Crash before the Inventory commit | No usage | The claim and its citation are one row in one transaction. |
| Crash after the Inventory commit | One usage | Retry with the same key and terms replays the claim (`store.go:569`). Retry after expiry gets a conflict and creates no new usage (`store.go:575`). A retry re-resolves the quote; only the fingerprinted fields must match (§5). A newer version or a new paid unit is refused as key reuse before any expiry write (`store.go:572`–`:573`); the first hold keeps its units until a terminal write ends it (§3, cleanup: TKT-382). |

On TKT-382 D1: cross-pool cap locking is already proven for the presale shape by
`TestPresaleCodeCapHoldsAcrossSlots`. Under model (i), crash recovery and exactly-once terminal
release reduce to two existing claim properties: one-row atomic writes and the idempotent
`Transition`. TKT-380 established those by **code inspection only**. It did not execute a
crash/lost-response proof or a release-replay proof. **TKT-382 must execute both on the promoted
paths**; this ADR moves that proof obligation to TKT-382 rather than claiming it.

### 8. Composition rules and examples (COS-2)

All amounts are integer minor units in one currency. The expected values come from owner policy
and the cited ADRs, not from any implementation output. Each row is one testable example.

| # | Rule | Testable example | Implementing boundary |
|---|---|---|---|
| 1 | Winner first; one code | Base 2000, Catalog winner 1501, promotion 1000 bps → paid unit 1350. A second code is **unsubmittable**: the request has one optional code field. | TKT-381 request schema and composition |
| 2 | Percentage reduction | `floor(1501 × 9000 / 10000) = 1350`; reduction 151. | TKT-381 pure arithmetic |
| 3 | Fixed reduction | Face 1200, fixed 250 → paid 950. The reduction currency must match the sale currency. That rule is **derived** from the money rule (integer minor units plus ISO currency), not stated in the owner policy. | TKT-381 arithmetic and term validation |
| 4 | Clamp at face | Face 200, fixed 500 → paid 0, reduction 200. | TKT-381 arithmetic |
| 5 | Zero original face | Face 0, fixed 50 → paid 0, reduction 0. The snapshot keeps the code identity. **Cap count: no expected value** (open question). | TKT-381 snapshot; TKT-382 usage |
| 6 | Per-ticket rounding with quantity | Two tickets at 1501, 1000 bps → original face 3002, paid unit 1350, paid face 2700, reduction 302. Aggregate-first rounding gives 2701 and is wrong. | TKT-381 arithmetic and the persisted row |
| 7 | Percentage fee on paid face | Paid unit 1350, fee 333 bps → 44 per ticket; two tickets → 88. | TKT-381 `computeFeeBreakdown` input (`server.go:1080`) |
| 8 | Fixed fees unchanged | Two paid units at 1350; fixed fee 100 per ticket and 250 per order, both **passed on** → fees 450, gross 3150. | TKT-381 fee composition |
| 9 | Incidence | Paid face 2700, fee 88. Passed on: gross 2788. Absorbed: gross 2700, organizer line 2612, fee line 88 (ADR-048 §1). The fee stays 88 either way. | Commerce checkout and Payments settlement |
| 10 | Zero paid face | 10000 bps reduction plus a passed-on fixed fee of 100 → gross 100, a real charge. With no fees → gross 0, no capture (ADR-011 §1). Neither grants staff comp authority. | TKT-381 checkout; TKT-384 stays separate |
| 11 | Partial refund | Two tickets, paid face 2700, passed-on fee 88. Refund one → 1 × `unit_amount` 1350 (`refunds.go:178`). Usage stays 2. | TKT-383 refund binding; TKT-382 usage |
| 12 | Exchange from a discounted source | Source as in #11. Target: two undiscounted tickets at 1501 → face 3002. Upgrade = 3002 − 2700 = 302, the original reduction. The source fee 88 is carried (`exchanges.go:809`), so the journal legs are 2788 reversed and 3090 sold; their difference is 302. Both legs carry the source `order_id`. The reversed leg's breakdown is the source snapshot (promotion applied); the sold leg's is the replacement reservation (face 3002, no promotion) plus the carried 88 (§10). Source usage stays 2. | TKT-383 source loader, delta and fact legs; TKT-493 correlation |
| 13 | Integer safety | A valid large unit amount must not fail because a naive intermediate product overflows. A true aggregate overflow is refused before the hold. | TKT-381 checked arithmetic; no-side-effect test |
| 14 | Access independence | A valid promotion code cannot admit a caller whom the channel, seller or presale check refuses. | TKT-382 Inventory admission |

Cap examples (model (i), code cap as stated):

| # | Rule | Testable example |
|---|---|---|
| C1 | All-or-nothing | Cap 3, usage 1. A request for 3 is refused whole. A request for 2 is admitted; usage 3. |
| C2 | Single-use default | Cap 1. A request for 2 is refused. A request for 1 is admitted. |
| C3 | Expiry returns units at the terminal write | Cap 2. A 2-unit hold on P2 expires unconfirmed. A 2-unit admission on P1 is refused while nothing has written the expiry. A 2-unit admission on P2 sweeps P2 first and succeeds (probe 3 R2L, with cap 1). |
| C4 | Finalizing keeps units | Cap 1. A finalizing hold past its `expires_at` still counts; a new admission is refused. |
| C5 | Refund keeps units | Cap 2. Two units confirmed, one refunded. A new 1-unit admission is refused. |
| C6 | Draw-down conserves | Cap 10, a group of 10 citing the code. Draw 4: usage 10. The child expires and its expiry is written: usage 6. |
| C7 | Expiry against a queued finalize | Probe 1 schedule S1 with a cap of 1. The cross-pool admission is refused and the finalize succeeds; usage 1 (probe 3 S1). |
| C8 | Cap added while a hold is open | Uncapped code. A hold on P2 is open (held or finalizing, expired or not, with no terminal write). The cap is set to 1. A cross-pool admission is refused; usage 1. Not probed under R2. |
| C9 | Window closes while an admission waits | Probe 1 schedule W: an admission that waits on the code lock across `closes_at` is refused. |

### 9. The Commerce snapshot

- `reservations.unit_amount` = paid unit. `face_value_amount` = quantity × paid unit.
  `total_amount` = `face_value_amount` + passed-on fees.
- A versioned promotion snapshot holds: code, promotion id, version, kind, value (bps, or amount
  and currency), original unit face, unit reduction, paid unit, quantity, aggregate original face,
  aggregate reduction and currency.
- The Catalog price provenance stays as it is. The fee snapshot is computed on the paid unit.
- Pin these equations in TKT-381 tests: `paid_unit = original_unit − unit_reduction` and
  `face_value_amount = quantity × paid_unit`.

### 10. Journal correlation and the signature boundary

No journal change. The breakdown lives in Commerce. Every journal fact carries a `fact_id`
(`services/payments/internal/store/store.go:68`). For the six Commerce fact types in the table
below, Commerce derives the `fact_id` deterministically from the record that produced it. Other
fact types have other derivations. Payments derives the fact id of a charge fact
(`services/payments/internal/api/server.go:412`, `:444`) and of a whole-operation compensation
(`services/payments/internal/api/psp.go:345`) from `payment:`, the organizer, the payment key and
the type. A partial-refund leg's `payment.refunded` derives it from `payment-leg:`, the organizer,
the source key and the refund key (`services/payments/internal/api/partial_refund.go:36`–`:37`,
`:121`–`:126`). TKT-493
correlates **by fact type and `fact_id`**, never by `order_id` alone: the payload `order_id` of an
exchange fact is the **source** order for both legs (`services/commerce/internal/api/exchanges.go:841`, `:848`).

| Fact type | `fact_id` derived from | Breakdown basis |
|---|---|---|
| `order.created`, `order.completed`, `order.failed` | the order id and the type (`services/commerce/internal/api/server.go:1366`) | The order's reservation and its promotion and fee snapshot. |
| `order.refunded` | the refund id (`services/commerce/internal/store/refunds.go:96`) | The refund record, then its order → `orders.reservation_id` → the source reservation and snapshot (the join at `refunds.go:122`). Amount = quantity × that reservation's `unit_amount` (the paid face). Show the source promotion as the basis, not as a new discount. |
| `order.exchange.reversed` | the exchange id (`services/commerce/internal/store/exchanges.go:138`) | The exchange row's stored `source_order_id` → `orders.reservation_id` → the source reservation and snapshot. `SourceReservation` is not a stored exchange field: `lookupExchange` leaves it empty (`exchanges.go:318`–`:329`), and `BindOrderExchange` fills it from that same join (`:235`–`:237`, `:256`). |
| `order.exchange.sold` | the exchange id (`services/commerce/internal/store/exchanges.go:141`) | The exchange row's stored `replacement_order_id` → `orders.reservation_id` → the replacement reservation: the target face with no promotion. The row also stores `replacement_reservation_id` (`exchanges.go:329`); the two must agree, or the breakdown shows as inconsistent. The carried fee is the row's `source_gross_total` − `source_total` (as `replacementGross` computes it, `services/commerce/internal/api/exchanges.go:800`). |

To find the refund or exchange record, TKT-493 lists the refunds or exchanges of the payload
`order_id` and matches the derived `fact_id`. A fact that matches no record shows as unavailable.
A fact type not in the table (for example `payment.*`) is shown without a promotion breakdown.
TKT-493 may ship the sale facts first and add the correction facts later. Until then it shows no
breakdown for a correction fact; it must not show the source order's snapshot for it.

The breakdown is **correlated with** the signed money fact. It is **not covered by** the signature.
TKT-493 must say so on screen or in its contract. Missing or inconsistent snapshot data shows as
unavailable or inconsistent. It is never rebuilt from a current definition. Signed line detail
would need a separate journal contract decision; historical canonical bytes must not change, and a
fiscal line-data extension must also resolve the ADR-003 conflict that
[TKT-374 §9](../spikes/TKT-374-fr-cash-scope.md) records.

### 11. Throughput

Every path in §4's locking list serializes on the code row, from the code lock to the commit. That
is a throughput ceiling per code. It was not measured here. A code shared by a whole on-sale is the
worst case. The paths outside the list (finalize, confirm, draw-down, release, expiry, sweeps,
refund capacity return) do not take the lock and do not add to it.

In the first slice every path in the locking list takes the code row `FOR UPDATE`, capped or not. A later
optimization may let paths on an **uncapped** code take `FOR SHARE`, so they do not serialize with
each other. That is correct only if all three of these hold: a cap or window edit takes
`FOR UPDATE` on the same row; the path reads the cap again in a separate statement after its share
lock; and a path that then finds a cap stops and retries with `FOR UPDATE`. A path must never skip
the lock because the code is uncapped (§4). The `FOR SHARE` variant was not probed.

### 12. Out of scope

Staff comps, the first waitlist offer checkout and fee reversal (TKT-407) do not use promotions. A
100% buyer promotion is not staff authority. Zero-gross orders with absorbed fee obligations stay
a TKT-502 limitation; no capture is invented to settle them.

### Open questions for the owner

1. **Single-use.** Does "single-use" mean one ticket unit (the default here), one order, or one
   buyer?
2. **Campaign caps.** Is a cap across many codes of one campaign needed? It needs a second lock.
3. **Zero-face counting.** Does a code applied to a zero-face ticket (reduction 0) count against
   the cap?
4. **Quote stability across a crash.** This ADR lets a retry after a crash re-resolve the price,
   the original face, the reduction split and the fees (§5, option b). A retry that meets a new
   promotion version or a new paid unit is refused, and the first hold expires. Should the quote
   instead be frozen before the hold (a durable quote intent), so a retry never re-resolves?
5. **Refusal UX.** May the buyer see "expired" or "used up" instead of the uniform refusal? That
   trades the enumeration guard for clarity.
6. **"One idempotent terminal transition."** Closed on 2026-10-13: the owner chose R2 (§3).

## Consequences

- **Positive:**
    - One owner per fact, consistent with ADR-002, ADR-036 and ADR-046.
    - The cap is judged with the claim in one transaction. Usage cannot drift from the claims.
    - Draw-down conservation and the one terminal release come from existing claim mechanics.
    - Under R2 the cap does not depend on a monotonic clock, and only the paths that add usage take
      the code lock.
    - A promoted finalize keeps transaction-start time, as every other claim does (`store.go:829`).
      A buyer whose finalize waited past expiry keeps the checkout; the trade that TKT-148 declined
      three times is not needed.
    - R2 was executed on today's presale path, mutated and repeated (probe 3).
    - The window-after-lock rule was prototyped and closes probe 1's window race.
    - No journal change and no rewrite of canonical bytes.
- **Negative:**
    - A code exists in Catalog and Inventory. Provisioning must keep them together, and a
      one-sided code must fail closed.
    - An expired hold blocks its code's units until a terminal write. Every sweep today is lazy
      and pool-local, so a quiet pool can block a code with no end. A buyer can be refused a code
      that has units free in principle. TKT-382 must add cleanup: a liveness need, not a
      correctness one.
    - The window check uses `clock_timestamp()` with no monotonic guard. A backward clock step can
      admit just after `closes_at`. The channel and presale windows have the same property
      (`channel_allocations.go:43`, `:54`).
    - One hot row per code, capped or not, in the first slice. Only admissions and edits serialize
      on it. Throughput is not measured.
    - A crash retry may record a different original face, reduction split and fees than the first
      attempt would have (§5). A retry across a version or paid-unit change fails as key reuse.
    - TKT-493 needs a per-fact-type lookup, not a single `order_id` join (§10).

### Presale races (probe 1): two confirmed on today's code

**Finding 1, the cap (filed as TKT-543).** Today's presale code path overshoots its cap. Probe 1 forced this schedule with real Inventory
migrations and the real `CreateHold` and `Transition`, against PostgreSQL 18.4:

1. Hold H (1 unit, code capped at 1) on pool P2.
2. Another transaction holds P2's pool lock. A finalize of H starts and waits on it. Its
   transaction starts before H expires.
3. H expires. An admission on pool P1 with the same code commits. It left H out of the count.
4. The pool lock is released. The finalize judges H on its transaction-start `now()`, finds it
   alive, and commits `finalizing`.
5. Both claims confirm. The code capped at 1 shows 2 confirmed units.

Result: confirmed in 6 of 6 runs. A control run (finalize before expiry) refused the admission in 6
of 6 runs, so the fixture can refuse. Details: [presale-race-finding.md](../evidence/TKT-380/presale-race-finding.md).

Limits: the probe drove the Inventory store, not the HTTP services. By reading only, a code-citing
buyer hold reaches Commerce checkout through the group draw-down staff sale
(`services/commerce/internal/api/server.go:2058`; the child inherits the code at
`reservations.go:323`). The window is narrow with a 10-minute TTL: the finalize must start just
before expiry and wait past it. That is an inference about likelihood, not a measurement. The
draw-down child has the S2 shape (fresh time, no code lock, `reservations.go:268`); it was not
probed. Fixing the presale path is out of this spike's scope. Two candidate fixes were executed: the
R1 prototype in probe 2, and the R2 usage predicate in probe 3. The presale count also subtracts
refunds (ADR-064), so either fix keeps that part.

**Finding 2, the window (filed as TKT-544).** Today's presale code path can admit after the code's window closed.
`redeemPresaleCode` computes the window inside its `SELECT … FOR UPDATE` (`presale_codes.go:87`).
Probe 1 schedule W: a transaction locks the code row without updating it; an admission that starts
before `closes_at` waits on that lock; the clock passes `closes_at`; the lock is released. The
admission is admitted: 6 of 6 runs. A control admission that starts after `closes_at` is refused:
6 of 6 runs. With the window read moved to a separate statement after the lock (`window-fix.py`),
the same schedule is refused: 6 of 6 runs. The blocker in a real system is any concurrent admission
of the same code; the overrun is bounded by how long that admission holds the lock (inference).

### Reading R2 executed (probe 3)

Probe 3 built today's Inventory store with one change: the usage count that `redeemPresaleCode`
reads under the code lock (`codeRedeemedQuantity`) counts every `confirmed`, `finalizing` or `held`
claim, with no expiry test ([`r2-predicate.diff`](../evidence/TKT-380/output/r2-predicate.diff)).
`CreateHold` and `Transition` are today's code. Code capped at 1, two pools, hold TTL 2 seconds.
Observed summaries ([evidence](../evidence/TKT-380/README.md)):

| Schedule | R2 build: 6 runs, then 7 repeats | Today's predicate (the mutation): 1 run |
|---|---|---|
| Control (finalize before expiry) | Admission refused, usage 1 | Admission refused, usage 1 |
| S1 (probe 1's cap race) | Admission on P1 refused; the queued finalize succeeded; usage 1 after confirm | Admission admitted; finalize succeeded; usage 2 after confirm |
| R2L (an expired hold H on P2, no terminal write) | Admission on P1 refused while H was `held`; a hold on P2 swept H to `expired` and was admitted; usage 1 | Admission on P1 admitted on the expired H; the hold on P2 was refused; H stayed `held` |

Every repeat gave the same summary. The mutation turns S1 and R2L from held to broken, and the
control stays held, so the probe can tell R2 from today's count.

What this shows, and what it does not:

- Under R2 the S1 overshoot does not occur, and a queued finalize keeps its unit. That is the R2
  side of §3's claim, observed on today's presale path.
- R2L shows the liveness cost and its pool-local relief: the code stays blocked for P1 until a
  transaction on P2 writes H's expiry. It does not test a cleanup design; TKT-382 owns that.
- Probe 3 changed only the predicate. It did not move the window read, did not edit a cap under
  R2, and did not run Commerce or the HTTP layer. The operator read (`PresaleCodeStatuses`) was not
  changed, so the usage it reports still judges expiry.

### Pre-mortem: four ways this ADR is wrong in six weeks

| Failure | Prevention |
|---|---|
| A refund gives back a promotion unit, because someone reused `consumedQuantity`. | Keep a separate usage expression. Test refund-then-rebuy, and mutate the expression to `consumedQuantity` to show the test goes red. |
| Someone makes the usage count judge expiry again (for example, reuses `liveClaims`), and a queued finalize overshoots the cap. | Keep probe 3's schedules (S1, R2L) and probe 1's W as store tests. Mutate the predicate to `liveClaims` and show S1 goes red. |
| Units stay blocked on a quiet pool, and buyers are refused a code that has free units. | TKT-382's cleanup. Test that a cross-pool admission succeeds after cleanup runs, with no buyer action on the quiet pool. |
| A reader takes the original face for the paid face, or presents the Commerce breakdown as signed journal data. | Pin the snapshot equations. Test refund unit amounts, exchange aggregates and journal legs separately. State the signature boundary in TKT-493. |

## References

- TKT-380 (this spike), epic TKT-7, stories TKT-381, TKT-382, TKT-383, TKT-384, TKT-407, TKT-493,
  TKT-502
- [Owner decisions, 2026-10-10](../product/backlog-decisions-2026-10-10.md)
- [TKT-380 evidence and probes](../evidence/TKT-380/README.md)
- [ADR-002](./ADR-002-services-from-day-one.md) — service ownership
- [ADR-003](./ADR-003-append-only-audit-trail.md), [ADR-011](./ADR-011-checkout-journal-protocol.md),
  [ADR-016](./ADR-016-checkout-recovery-state-machine.md) — journal, zero-total checkout, recovery
- [ADR-010](./ADR-010-postgres-claim-transaction.md) — the claim transaction and lazy expiry
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) — every consistency claim here is
  honest-writer consistency, not tamper-evidence
- [ADR-023](./ADR-023-operational-holds-and-conversion.md),
  [ADR-027](./ADR-027-group-agency-reservations.md) — conversion and draw-down
- [ADR-024](./ADR-024-channel-allocations.md) — channel accounting and the clock split
- [ADR-036](./ADR-036-pricing-rules-representation.md) — the catalog price winner (§6)
- [ADR-037](./ADR-037-post-purchase-refund-money-protocol.md),
  [ADR-038](./ADR-038-refund-reversal-ticket-voiding.md) — refunds and capacity returns
- [ADR-039](./ADR-039-exchange-settles-the-difference.md),
  [ADR-063](./ADR-063-exchange-reversal-reconciliation.md) — exchanges
- [ADR-040](./ADR-040-event-cancellation-bulk-refund-runs.md),
  [ADR-062](./ADR-062-refund-reversal-reconciliation.md) — cancellation runs and reversal
- [ADR-046](./ADR-046-fee-rules-representation.md) — fee rounding, incidence, "catalog resolves,
  commerce computes"
- [ADR-048](./ADR-048-settlement-ledger.md) — the settlement identity
- [ADR-064](./ADR-064-presale-unlock-codes.md) — the code row lock this ADR reuses
- [ADR-069](./ADR-069-opaque-payment-tokens-and-exchange-upgrade-instruments.md) — exchange upgrade
  instrument; zero-total checkout token
- [ADR-075](./ADR-075-proposed-waitlist-capacity-capture.md) — proposed; not activated here
- [TKT-374 spike §9](../spikes/TKT-374-fr-cash-scope.md) — the journal payload limit
