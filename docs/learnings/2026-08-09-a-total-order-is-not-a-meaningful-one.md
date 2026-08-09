# A total order is not a meaningful one

**TKT-230**, 2026-08-09. Four learnings from one flaky-test ticket, all about ordering an
append-only trail in PostgreSQL. The first is the one that generalizes.

## 1. `ORDER BY <timestamp>, <random uuid>` reads as correct because it *is* total

`claim_history` was read with `ORDER BY occurred_at, id`. Every reviewer who saw that query saw a
total order — and it is one: the pair is unique, the sort is deterministic *for a given set of
rows*. What it is not is **meaningful**. `id` is `uuid.New()` — UUIDv4, random, no time component —
so two rows tying on `occurred_at` are ordered by a coin flip.

The defect is invisible serially and appears under concurrency. Measured against a real PostgreSQL:

| Scenario | Rows | Distinct `occurred_at` | Collisions |
|---|---|---|---|
| 300 inserts, separate transactions, serial | 300 | 300 | **0** |
| 1200 inserts, 8 concurrent writers, separate transactions | 1200 | 1199 | **1** |

One collision in 1200 — between two *different* writers, identical to the microsecond. That is
enough to make a gate non-deterministic: the assertion `history[0].Action = draw_down, want reserve`
is a tie resolved the wrong way, and it only ever fires when the machine is busy.

**The rule:** if a sort's tie-break carries no information, the sort has no defined behaviour on
ties — it merely hides that fact behind determinism-per-query. Ask what breaks the tie, not whether
the order is total. A monotonic column (`bigserial`, a sequence) is the fix; the in-repo precedent
is `access.lifecycle_event_integrity.sequence`.

**Still open elsewhere:** `access.lifecycle_events` uses the same `ORDER BY occurred_at,id` shape
(`lifecycle_checkpoint.go:317`). Tracked as **TKT-234**.

## 2. `now()` is transaction-start time, so a stamp taken under a lock predates acquiring it

This one caught the ticket's own plan-review, which argued that because every writer holds
`SELECT … FROM inventory_pools … FOR UPDATE` (ADR-010's lock order), the writes are serialized and
so their timestamps must reflect that order.

**False.** `now()` is fixed at the transaction's **first statement**, and every writer does
`BeginTx` *before* blocking on the lock. A transaction that waits keeps a timestamp from before it
waited. Reproduced:

```
A: BEGIN; FOR UPDATE (immediate); ... COMMIT   -> now() = 05:40:12.96   (committed FIRST)
B: BEGIN; SELECT 1; sleep; FOR UPDATE (waits); COMMIT -> now() = 05:40:11.90   (committed SECOND)
```

B committed second and carries the earlier stamp. `History()` would have returned the two in the
wrong causal order and called it chronology.

**The rule:** "we serialize on a row lock, therefore timestamps reflect order" is wrong unless the
stamp is taken *after* the lock — `clock_timestamp()` (evaluated per statement) rather than `now()`.
Any argument of that shape should be tested against a real database, not reasoned about; it took an
adversarial review pass and a two-terminal experiment to overturn here.

Note what `clock_timestamp()` does **not** fix: it still reads a non-monotonic wall clock. It
narrows the window; it does not close it (TKT-234).

## 3. A column DEFAULT does not apply to an explicitly-supplied NULL

```sql
CREATE TABLE t(id uuid PRIMARY KEY, ord bigint DEFAULT nextval('s'));
INSERT INTO t(id) VALUES (…);            -- ord = 1      (DEFAULT applies)
INSERT INTO t(id, ord) VALUES (…, NULL); -- ord = NULL   (DEFAULT bypassed)
```

So a DEFAULT is a convenience for writers that omit the column, never a guarantee about the column.
A `BEFORE INSERT` trigger closes that hole — but only if it **overwrites unconditionally**. A
fill-in-NULLs trigger (`IF NEW.x IS NULL THEN …`) still lets any INSERT, COPY, restore or
replication apply supply its own value.

That distinction is what makes uniqueness a *property* rather than a constraint: if the sequence is
provably the only source of the value, no unique index is needed to enforce it — which matters
because a unique index costs a full-table scan (see 4). The trade is that a restore must disable the
trigger deliberately and resynchronize the sequence.

## 4. `ADD CONSTRAINT … NOT VALID` is the scan-free way to enforce a predicate for new rows

A validated `CHECK` scans every existing row. goose runs a migration inside one transaction, so the
`ACCESS EXCLUSIVE` lock from `ALTER TABLE` is held across that scan — and under ADR-008's 30s bound
(kept by ADR-022), a migration that times out leaves the service unable to start, because ADR-022
gates startup on the migrate job completing.

The reflex is therefore to drop the constraint. That is a false choice: `NOT VALID` enforces the
predicate for **every new row** while skipping the validation pass over old ones. It can be
`VALIDATE`d later, out of band, if ever wanted.

This ticket initially removed a CHECK outright on the "constraint = full scan" belief, and an
adversarial review pass corrected it.

---

**Meta.** Three review passes were needed here, and the counter reset once because pass 2 proved
pass 1's fix wrong at the *root* rather than at the edges. Passes 1 and 2 both accepted a reasoning
error (learning 2) that only execution could catch — the reviewer had a database available the whole
time and so did the implementer. When a plan makes a claim about **database or runtime semantics**
— lock ordering, timestamp evaluation, isolation, constraint cost — run it.
