# You cannot classify an event you caused

**TKT-122, PR #132.** A shared primitive had to decide, when a parent context was cancelled *and* a
JetStream subscription had closed, which one to report. It took four review passes and one full
reversal to arrive at the answer, and the answer is the counter-intuitive one.

## What happened

`durableconsumer.Wait` classifies a consumer's exit: clean shutdown (return `nil`) or asynchronous
termination (latch unready, return a durable-named diagnostic). Its `select` has two arms, and when
both are ready Go picks at random. The ADR called that acceptable — *"both answers are defensible, so
callers must not depend on which"* — which held only while the answer changed nothing but an exit
code nothing reads.

TKT-122 made the classification carry the operator's evidence, so the randomness became a defect. The
obvious fix, and the one review recommended: **termination wins**, because it is the more serious
state and the only one with a diagnostic.

That was implemented, verified, documented in the ADR — and it was wrong.

Both service `main`s `defer nc.Close()` **without joining their consumer goroutines**. On an ordinary
stop, `awaitShutdown` returns, `run` returns, the connection closes, and *that* closes the
subscription — underneath a goroutine that has not yet arbitrated its already-cancelled context.
`Closed()` does not encode its cause. So "termination wins" emits a durable-deletion diagnostic on
**clean shutdowns**, on every stop that loses the race, corrupting the very operator evidence the
ticket added.

The final answer: **shutdown wins**, deterministically. A durable that genuinely dies at that instant
is suppressed entirely.

## The rule

**When your own shutdown path causes the event you are trying to classify, the event carries no
information.** Ask, before writing the arbitration: *can I produce this signal myself?* If yes, and
the signal does not say who produced it, then the branch is not a classifier — it is a coin flip
dressed as one, and preferring the "interesting" answer converts a rare miss into a routine false
alarm.

False alarms on the common path are worse than missed detections on the rare path, because they
destroy trust in the channel that the detection would have used. Here the ticket had just added
producer-side logging so a late failure would leave evidence; preferring termination would have made
that log fire on ordinary stops, making it worthless for the case it existed to catch.

**The safe direction is the one that cannot fire spuriously.** Before cancellation nothing is
ambiguous and termination is always reported — which is the case that actually matters, a durable
deleted under a live consumer.

## Two smaller rules from the same ticket

**A mutex only serializes writers that take it.** The ticket added a `ctx.Err()` check inside
`readinessMu`, reasoning that the mutex made it atomic against the other readiness writer. It did
not: `Wait` latches `ready=false` through a plain `atomic.Bool` with **no mutex** — a third writer
the mutex was never designed to cover. The check was a TOCTOU. Re-asserting under the mutex only
*shrinks* the window to a handoff; a **one-way flag** closes it, because termination never
self-heals, so a latch that outlives the flag can never be right. When adding a guard to a
mutex-protected invariant, enumerate every writer of the value — not every writer of the mutex.

**Say what each accepted residual actually costs.** This ticket accepted two, and an early draft of
the ADR called them "the same". They are not:

| | what `Wait` returns | logged? |
|---|---|---|
| Arbitration | `nil` | **nothing at all** |
| Drain snapshot | the diagnostic | **yes**, by the producer |

One gives up the exit code; the other gives up the whole signal. "These two are the same" is exactly
the kind of tidy summary that hides the expensive half — the same failure as
[record a relaxation as a relaxation](./2026-07-30-record-a-relaxation-as-a-relaxation.md).

## And a process note

Two of this ticket's four review passes found errors that had already been *verified* — once by me,
tracing `cc.Stop()` and concluding a clean shutdown could never be misreported, having missed the
outer `nc.Close()`. That false claim went into an ADR and **survived a review pass** before a later
one disproved it. A verification that checks the nearest cause and stops is not a verification;
enumerate every producer of the effect, including the ones in a different file.
