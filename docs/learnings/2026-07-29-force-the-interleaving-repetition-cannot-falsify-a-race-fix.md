# Force the interleaving — repetition cannot falsify a race fix

**TKT-132, PR #123.** A flaky test reported as "2 failures in 7 runs" invites an obvious protocol:
change the staging, run it N times, ship when N is green. That protocol cannot tell a **closed**
race from a **narrowed** one, and on a quiet machine it usually cannot produce the red at all.

## What happened

`TestRecoveryRefundsCapturedMoneyWithGoneClaim` staged a `confirmation_pending` order already aged
past the recovery grace period, *then* expired the inventory hold. The row is sweep-eligible the
instant it lands, so the 2s recovery runner could claim it while the expiry was still in flight,
confirm a live claim, and complete the order. `completed` is terminal, so the 45s poll for
`refunded` could never converge.

The plan draft's test step said: run `-count=20`; if all 20 pass, repeat until the failure is
captured; do not proceed on an unobserved race.

The measured baseline killed that step before it ran: **10/10 green at ~2.05s** on an isolated
single-tenant stack. The reported 2-in-7 came from a loaded CI box. On this machine the
INSERT→expire gap is sub-millisecond against a 2s tick, so the instruction was an unbounded wait on
an event that does not fire here — and it blocked the ticket on luck.

The deeper problem is that a coincidental red proves nothing anyway. A reorder that merely
*narrows* the window also stops a coincidental red from recurring. Repetition cannot distinguish
the two, because both look like "it stopped failing".

## The rule

**Make the losing interleaving deterministic, then run the same forcing against both versions.**

A temporary `time.Sleep(5s)` — longer than the 2s `RECOVERY_INTERVAL` — injected at the seam
between the two staging statements:

| staging | with the same 5s injection |
|---|---|
| as it was | **FAIL 51.17s**, `order status = "completed", want refunded` — the ticket's verbatim failure |
| reordered | **PASS 3/3 at 7.05s** |

Same injection, opposite outcomes. That is a falsifiable red for a race: the fix is credited only
because the forced loss no longer loses. `-count=20` still ran afterwards, but as *acceptance*, not
as the instrument that proved the fix.

The forcing is scaffolding and does not get committed. A committed sleep slows the suite forever
and pins a timing detail (the runner's tick) that no ADR guarantees; the evidence belongs in the
ticket and the commit message, where it costs nothing at runtime.

## Corollary — measure the baseline before designing the red

The draft proposed an unbounded loop because it had no idea the race does not reproduce locally. A
flake ticket's *first* action is to measure the current failure rate in the environment the fix will
be verified in. If it is 0/N, no repetition-based protocol can work, and that is a fact about the
environment, not about the bug.

## The limit worth stating in the same breath

The forcing proves the window is closed **against that interleaving**. It does not prove no other
interleaving exists — here the argument that closes the remainder is separate and structural:
nothing observes an expired claim before the order row exists, because inventory evaluates expiry
lazily on read (`services/inventory/internal/store/store.go:62`) and runs no reaper
(`services/inventory/cmd/inventory/main.go` has two `go func()`s, neither a ticker). Say which
interleaving your forcing covers, and argue the rest — the same discipline
[coarse observables](./2026-07-25-coarse-observables-pass-the-broken-build.md) demands of
consequence-assertions applies to race fixes.
