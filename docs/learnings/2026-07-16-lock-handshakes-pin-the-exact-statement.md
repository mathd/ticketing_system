# A lock-queue handshake must pin the exact statement under test

**Ticket:** TKT-76 (PR #57) · **Found by:** ai-review pass 3, against the author's own regression test

## What happened

TKT-76's concurrency regression needed to prove: *Provision's `SELECT ... FOR UPDATE` queues
behind an uncommitted adjustment, and the guarded UPDATE that follows sees the adjustment*. The
handshake watched `pg_stat_activity` for **any** waiter whose query matched
`'%inventory_pools%'` — and one appeared, so the test proceeded and passed.

The waiter was the wrong statement. The staging had the adjustment transaction *update* the row
before Provision started, so Provision's `INSERT ... ON CONFLICT DO NOTHING` hit an uncommitted
conflicting tuple and absorbed the wait itself. The `SELECT ... FOR UPDATE` under test ran after
the commit, uncontended, never observed. Deleting it entirely still passed the test — the
regression for a concurrency fix did not exercise the fix. (TKT-78's lock-queue learning already
said "prove the waiter queued first"; this is the sharper corollary: prove *which* waiter.)

## The rules

1. **Pin the predicate to the statement's exact text**, not a table-name substring:
   `query LIKE '%FROM inventory_pools WHERE slot_id=$1 FOR UPDATE%'`. A broad predicate matches
   whichever statement happens to block — including ones that make the test vacuously green, and
   unrelated waiters elsewhere in the cluster.
2. **Stage so the statement under test is the one that must wait.** Here: the blocker takes only
   the row lock (no data change) before the handshake — against a merely-locked committed tuple,
   `ON CONFLICT DO NOTHING` resolves without waiting, so the first thing that can queue is the
   `FOR UPDATE`. The blocker's writes land only after the queue is proven.
3. **Mutation-check the handshake:** delete the statement under test and run the test — it must
   fail (here: the tightened handshake times out instead of observing a waiter). This is the
   concurrency-test form of "a passing test is not evidence it tests anything" (2026-07-15): the
   deleted-guard mutant is cheap and definitive.
