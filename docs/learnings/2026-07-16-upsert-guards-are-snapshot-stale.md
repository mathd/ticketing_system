# Upsert guards are snapshot-stale under lock contention

**Ticket:** TKT-76 (PR #57) · **Found by:** ai-review pass 2, proven by a failing test before fixing

## The bug shape

```sql
INSERT INTO inventory_pools (...) VALUES (...)
ON CONFLICT(slot_id) DO UPDATE SET capacity = EXCLUDED.capacity
WHERE ... AND NOT EXISTS (SELECT 1 FROM claim_history WHERE pool_id = EXCLUDED.slot_id)
```

Under READ COMMITTED, the statement's snapshot is fixed when the statement starts. If the
conflicting row is locked by another transaction, the upsert **waits on the row lock** — and when
it wakes, the `DO UPDATE` proceeds against the *latest* row version (EvalPlanQual), but the
`NOT EXISTS` subqueries still evaluate against the **pre-wait snapshot**. Anything the lock-holder
committed while the upsert queued — here, a capacity adjustment plus its audit row — is invisible
to the guard. The guard passes; the overwrite happens; nothing errors.

The failure needs contention to exist at all, so every sequential test passes. TKT-76's first fix
(adding the `claim_history` check to the upsert's WHERE) shipped this bug *as the fix* — it
extended a guard pattern that was already unsound, and the sequential regression test written for
it went green.

## The rule

**A guard that must see concurrent commits cannot live in the same statement that waits for
them.** Lock first, in its own statement; decide in the next one — READ COMMITTED gives every new
statement a fresh snapshot, so a guard that runs *after* the lock is acquired sees everything the
previous holder committed:

```sql
INSERT ... ON CONFLICT (slot_id) DO NOTHING;            -- new-row fast path
SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE;  -- queue here (ADR-010 order)
UPDATE inventory_pools SET capacity=$1 WHERE ... AND NOT EXISTS (...);  -- fresh snapshot
```

This is the same discipline ADR-010 already imposes on every claim mutation ("lock the pool row
first"); the upsert was the one write path that skipped it because a single statement *looked*
atomic. Atomic ≠ current: the statement is atomic against its snapshot, and its snapshot is old.

## Testing it

Sequential tests cannot catch this. The regression queues a real `Provision` behind an
uncommitted adjustment and proves the interleaving with a `pg_stat_activity` handshake — see the
companion learning on vacuous handshakes (same ticket) for how the first version of that test
silently proved nothing.
