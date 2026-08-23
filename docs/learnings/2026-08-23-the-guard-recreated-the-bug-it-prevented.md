# A guard added to close a race recreated the bug it was added to prevent

**2026-08-23 — TKT-255.** The ticket's whole subject was a **wedge**: an exchange whose inventory
target claim went terminal could never settle, and its durable row left the source order neither
exchangeable nor refundable, forever. The fix was an operator command to unwind it.

A review pass then found a race — an unwind could delete the binding while a settlement was in
flight — and the fix for *that* was a durable `settling_at` marker: written once the settlement
passes the point where it can move money, read by the unwind, which refuses while it is set.

The marker was write-once and nothing cleared it. So a settlement that failed **definitively** —
payments refusing, or unreachable long enough that the caller gave up — left the marker set forever
and the source order **permanently un-unwindable**.

That is the original wedge, reintroduced by the mechanism added to protect against a race in the fix
for it. And it was strictly worse than the wedge it came from, because the original at least had an
operator command as a way out; this one blocked that command too.

## Why it survived its own tests

Every test around the marker was correct and passed honestly. `MarkExchangeSettling` marked. The
guard refused a marked exchange. The listing showed the marker. Mutating any of them went red. The
mechanism worked exactly as designed — the design was what had no exit.

Mutation testing cannot find this, for the reason `AGENTS.md` already records about tests that bless
a defect: every test was written from the same model of correctness that produced the gap. The
question that finds it is not about any test at all:

> **This new state waits for something. What happens when that thing never arrives?**

`settling_at` waits for a settlement to finish. Nobody asked what it does when the settlement never
finishes, and "never" is the ordinary outcome of a payments outage.

## The shape, stated generally

A guard that grants no exit is a **lease with no expiry**, whatever it is called. Whenever a
mechanism blocks an action while some condition holds, three things must be true and each should be
said out loud:

1. **Something clears the condition**, or the condition **times out**, or the block **degrades to a
   weaker check**. Pick one explicitly; "it will be cleared by the happy path" is not one, because
   the case being guarded is the unhappy path.
2. The bound is **derived, not chosen**. Here the window is five minutes because every cross-service
   call is bounded at `obs.ClientTimeout` = 30 seconds, so a settlement is returned or cut off ten
   times over before it elapses. The constant carries the derivation *and the condition that would
   invalidate it* — if that timeout ever grows past a minute or two, revisit them together. A number
   with no derivation drifts silently the moment the thing it was implicitly sized against changes.
3. Past the bound the guard **stops deciding** rather than starting to permit. Here the marker's
   expiry hands the decision to the authoritative check — payments' own records — which refuses a
   charged exchange whatever the marker says. That distinction is what keeps "wait five minutes" from
   becoming a route to unwinding a charged buyer, and it has its own test.

## The related trap in the same ticket

A third review pass called the five-minute window a "[critical] unsafe lease". It was not, and the
refutation was a **constant in shared code** rather than an argument: the transport bound makes the
scenario unreachable. Two of three passes found real defects — one of them this ticket's central one
— so the reviewer was not crying wolf. But a bound the *system enforces* is stronger evidence than a
bound the reviewer imagines, and going to read it costs a minute. Refute with the code or accept the
finding; do not argue.
