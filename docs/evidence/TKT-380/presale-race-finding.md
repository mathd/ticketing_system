# Findings: two presale code races on today's code

TKT-380 probe 1 found two defects in the presale code path on 2026-10-13. Both are confirmed at the
store level and not fixed. Fixing them is not in TKT-380's scope.

1. **The cap can overshoot** when a queued finalize crosses hold expiry (found at `601d50be`;
   re-confirmed on the fix-round run). Filed as **TKT-543**.
2. **An admission can pass after the code's window closed** when it waited on the code lock across
   `closes_at` (found on the fix-round run, worktree HEAD `275355d6`; no Inventory code changed
   between the two heads). Filed as **TKT-544**.

# Finding 1 (TKT-543): a presale code cap can overshoot when a queued finalize crosses hold expiry

## Summary

A presale code capped at N can end with more than N confirmed units. It happens when a checkout
finalize starts before its hold expires, waits on the hold's pool lock past the expiry, and in that
time an admission on a different pool redeems the same code.

## Mechanism (read from code)

- `redeemPresaleCode` counts the code's usage under the code row lock with `consumingClaims`
  (`services/inventory/internal/store/presale_codes.go:110`,
  `services/inventory/internal/store/channel_allocations.go:78`). That predicate excludes a held
  claim whose `expires_at <= now()` (`services/inventory/internal/store/store.go:244`).
- `Transition` does not take the code row lock. It locks the pool, then reads the claim together
  with `now()` (`store.go:825`, `:829`), and `expired()` judges liveness on that value
  (`store.go:246`). In PostgreSQL `now()` is the transaction start time, so it is earlier than the
  moment the finalize gets the pool lock.
- So the admission judges the hold expired, and the finalize, later, judges it alive. Both commit.
  A finalizing claim never expires (`store.go:244`), so the unit stays counted.

Within one pool this cannot happen: an admission on the same pool runs `sweepExpired` under the
pool lock (`store.go:597`), which writes `expired` before the queued finalize reads the row. Across
pools there is no such write.

## Reproduction (observed)

`bash docs/evidence/TKT-380/run.sh <new output dir>`, then read `probe1-presale.txt`. See the
[README](README.md) for the fixture and schedule.

1. Code `PROMO-CAP-1`, cap 1, on channel `presale` of pools P1 and P2.
2. Hold H, 1 unit, on P2 (TTL 2 s).
3. A blocker transaction holds P2's pool row lock.
4. `Transition(H, "finalizing")` starts and is observed waiting on that lock, with `xact_start`
   before H's `expires_at`.
5. The database clock passes H's `expires_at`.
6. `CreateHold` on P1 with the same code: admitted (`err=nil`). The finalize is still waiting.
7. The blocker rolls back. The finalize returns `status=finalizing err=nil`.
8. Both claims are confirmed. `PresaleCodeStatuses` reports `redeemed=2` for a cap of 1.

Observed in 6 of 6 runs ([`output/probe1-presale.txt`](output/probe1-presale.txt)). The control
schedule (finalize before expiry) refused the admission in 6 of 6 runs.

## Reachability beyond the store (inference, not run)

- A public caller can create a code-citing hold through Inventory's `POST /holds`, which accepts
  `presale_code` (`services/inventory/internal/api/server.go:366`) and which the gateway proxies
  under `/api/inventory/` (`gateway/cmd/gateway/main.go:31`).
- Commerce's GA reservation does not forward a presale code (`services/commerce/internal/api/server.go:991`).
  A code-citing buyer hold does reach Commerce checkout through the group draw-down staff sale:
  the child inherits the source's code (`services/inventory/internal/store/reservations.go:323`)
  and Commerce persists it as a reservation (`server.go:2058`).
- With the default 10-minute TTL the window is narrow: the finalize must start in the last instant
  before expiry and wait on the pool lock past it. Pool lock waits are longest during an on-sale.
  Likelihood was not measured.
- Impact today: one presale code admits more units than its cap. Access only; no money moves
  through a presale code. Under a promotion the same shape would grant extra discounts.

## Related shape, not probed

`DrawDownGroupReservation` judges the source's expiry on `clock_timestamp()` but takes no code
lock (`reservations.go:268`). A draw-down that decides just before expiry and commits after a
cross-pool admission has counted could revive the units. This is probe 2's schedule S2 shape. It is
an inference.

## Candidate fix

Two candidates were executed. Choosing one for presale codes is TKT-543's decision.

- **Reading R2 (probe 3), the reading the owner chose for promotions in ADR-084 §3.** The code's
  usage count includes every `held` claim, expired or not, until a terminal write (sweep, release,
  committed expiry) ends it. Probe 3 changed only that predicate on today's code: the admission on
  P1 is refused and the queued finalize keeps its unit, usage 1, in 6 of 6 runs and 7 repeats; with
  today's predicate the same schedule gives usage 2. No path gains a lock, and the finalize keeps
  transaction-start time. The trade-off: an expired hold blocks the code's units until something
  writes its expiry, and today every sweep is lazy and pool-local (probe 3 schedule R2L). The
  presale count also subtracts refunds (ADR-064); the probe did not change that part.
- **Reading R1 (probe 2), rejected for promotions.** A path that keeps or creates code usage takes
  the code row lock after its pool and claim locks, then judges expiry on `clock_timestamp()`.
  Probe 2 prototyped it on `Transition` and it held the cap in S1, S2 and S3. It changes behaviour
  that two tests pin on purpose for code-less claims
  (`services/inventory/internal/store/buyer_ttl_clock_smoke_test.go:452`, `:541`); a fix scoped to
  code-citing claims leaves those tests valid. The trade-off: a code-citing finalize that waited
  past its hold's expiry is refused, and the cap depends on a non-decreasing clock (ADR-084 §3).

# Finding 2 (TKT-544): a presale code can be redeemed after its window closed

## Summary

An admission that starts before a code's `closes_at`, and waits on the code row lock past it, is
admitted after the window closed.

## Mechanism

- Read from code: `redeemPresaleCode` computes the window in the target list of its
  `SELECT max_redemptions, (codeWindowOpen) … FOR UPDATE`
  (`services/inventory/internal/store/presale_codes.go:87`–`:90`). `codeWindowOpen` uses
  `clock_timestamp()` (`services/inventory/internal/store/channel_allocations.go:54`), so the time
  base is not the problem. The time of evaluation is.
- Inference that matches the observation: PostgreSQL computes the target list before it waits for
  the row lock. When the lock holder commits or rolls back without updating the row, the value is
  not computed again. So "open" is judged before the wait and used after it.

## Reproduction (observed)

`bash docs/evidence/TKT-380/run.sh <new output dir>`, then read schedule W in `probe1-presale.txt`.

1. Code `PROMO-CAP-1`, no cap, `closes_at` = database time + 2 s, on channel `presale` of P1.
2. A blocker transaction runs `SELECT 1 FROM presale_codes … FOR UPDATE` on the code row. It does
   not update the row.
3. `CreateHold` on P1 with the code starts. It is observed waiting on the code row, with
   `query_start` before `closes_at`.
4. The database clock passes `closes_at`.
5. The blocker rolls back.
6. The admission returns `err=nil`; the claim is `held` and cites the code.

Observed in 6 of 6 runs. A control admission that starts after `closes_at` is refused in 6 of 6
runs. With the window read moved to a separate statement after the lock ([`window-fix.py`](window-fix.py)),
the same schedule is refused in 6 of 6 runs ([`output/probe1-window-fix.txt`](output/probe1-window-fix.txt)).

## Reachability and impact (inference, not run)

- In a real system the lock holder is another admission of the same code. It holds the code row
  until its claim transaction commits, so the overrun after `closes_at` is bounded by that hold time
  plus queueing behind other admissions of the code.
- Impact: a presale sells for a short time after its code window closed. Access only; no money
  moves through a presale code. Under a promotion the same shape would grant a discount after the
  promotion ended.

## Candidate fix

Read the window in a separate statement after the `FOR UPDATE` returns. That is all
`window-fix.py` changes: it moves the window read out of the locking statement and leaves the cap
(`max_redemptions`) in it. ADR-084 §4 goes further for promotion codes: it reads the cap, the
window and the usage after the lock. Probe 1b exercised only the window move.
