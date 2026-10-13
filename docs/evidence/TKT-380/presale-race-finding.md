# Finding: a presale code cap can overshoot when a queued finalize crosses hold expiry

Found by TKT-380 probe 1 on 2026-10-13, at repository head `601d50be`. Status: confirmed at the
store level; not fixed. Fixing it is not in TKT-380's scope.

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

ADR-084's fresh-time invariant: a path that keeps or creates code usage takes the code row lock
after its pool and claim locks, then judges expiry on `clock_timestamp()`. Probe 2 prototyped it on
`Transition` and it held the cap in both schedules. It changes behaviour that two tests pin on
purpose for code-less claims (`services/inventory/internal/store/buyer_ttl_clock_smoke_test.go:452`,
`:541`); a fix scoped to code-citing claims leaves those tests valid. The trade-off: a code-citing
finalize that waited past its hold's expiry is refused.
