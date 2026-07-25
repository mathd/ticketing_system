# A coarse observable can be produced by the broken version too

**TKT-99, PR #112.** When a test proves "the service reacted to X", the cheap observable is usually
a *consequence* — the process exited, the container restarted, the row disappeared. That observable
is often reachable by the very bug you are trying to exclude, and then the test is decorative.

## What happened

TKT-99 proves that deleting a JetStream durable out from under a live consumer makes access
terminate. Two observables were available:

- the container restarting (`RestartCount` / `State.StartedAt` advancing), and
- the exact diagnostic `waitConsume` writes, naming the durable.

The restart is the obvious one and the tempting one — it needs no log scraping and no string
literal to keep in sync. The test required **both**.

That turned out to be load-bearing on the very first RED run. Mutating `waitConsume` to return
`nil` on its `closed` arm — the exact defect the test exists to catch — **still restarted the
container**: the caller's tail then formatted a nil `ctx.Err()` and the process exited anyway
(`access: policy consumer stopped: %!w(<nil>)`). A restart-only assertion would have gone green
against the broken build.

## The rule

**Before settling on an observable, ask what else produces it — and specifically, whether the bug
under test produces it.** A test asserting a downstream consequence proves the consequence
happened, not that your cause produced it. Pair a coarse observable with one that only the correct
code path can emit (a specific error string, a named identifier, a state transition nothing else
performs).

Corollary, and the reason this is not just "write better assertions": you cannot find this by
reading the test. It only shows up when you **run the mutation** and watch the coarse signal fire
anyway. This is [prove-tests-fail](./2026-07-15-prove-tests-fail.md) with a sharper edge — it is
not enough that the test goes red for *some* mutation; check that it goes red for the mutation and
not merely for the ones that break everything.

## The limit worth stating in the same breath

Even the paired assertion in TKT-99 does not close causality: the diagnostic reads
`durable deleted or subscription terminated`, so a coincident subscription failure would satisfy
both counters. That residue is filed as TKT-123 rather than hand-waved. **Say which adversary or
coincidence your observable still admits** — the same discipline ADR-021 demands of integrity
claims applies to test claims.
