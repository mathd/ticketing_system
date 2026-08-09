# An assertion over a `min()` only tests the winning arm

**TKT-233**, found by the adversarial ai-review pass, confirmed by mutation.

`TestExpiredChannelHoldFreesItsCap` asserted that a channel's availability was `3` after a hold
expired, and treated that as proof that the expired hold had left *the* live-claims accounting. It
wasn't. `Availability` computes:

```go
a.Available = clampAvailable(min(remaining, int64(chCap)-consumed))
```

— where `remaining` derives from the **pool-level** `liveClaims` sum (`store.go:409`) and `consumed`
from the **channel-level** `consumingClaims` (`channel_allocations.go:29`). Two independent
predicates, one number.

With pool capacity 10 and a quantity-3 hold, a regression that kept counting the expired hold
pool-side leaves `remaining = 10 - 0 - 3 = 7`, and `min(7, 3)` is **still 3**. The channel arm wins
the `min()` either way, so the assertion passes and the pool-level regression is invisible. Verified
by mutating only the pool-level predicate and leaving the channel one intact: the test passed
straight through it.

## The rule

**A single assertion over a `min()` / `max()` / clamp of N predicates can only discriminate the
winning arm.** Proving all N requires asserting the components, not the aggregate. Here that meant
adding `ch.Held == 0` — which reads the pool-level sum directly — alongside `ch.Available == 3`.
After that, mutating either predicate reddens a different assertion:

| mutation | assertion that fires |
|---|---|
| pool-level `liveClaims` in `Availability` | `held=3 want 0` |
| `consumingClaims` | `available=0 want 3` |

## Why it is easy to miss

The expected value is *arithmetically correct* — 3 really is the right answer — so the assertion
looks right, passes, and reviews clean. Nothing about a green test says "this number would also be 3
if the code were broken." The aggregate is the value the production code returns and therefore the
obvious thing to assert; the components feel like implementation detail. That instinct is backwards
for a test whose whole job is to pin a specific predicate.

This is a sibling of
[a fixture too small cannot show the negative](2026-08-03-a-fixture-too-small-cannot-show-the-negative.md):
there the fixture admitted no failing input, here the *assertion* cannot express the failure. Both
produce a test that is green, plausible, and load-bearing for nothing. Both are caught the same way
— break what it covers and watch it fail, once per thing it claims to cover.

## Where else this applies

`clampAvailable(min(...))` and `GREATEST(... , 0)` appear across the availability and capacity paths
(`reservedForChannelsSQL`, `effectiveCapacity`, the seat-occupancy queries). Any test asserting one
of those outputs should ask which arm it is actually pinning.
