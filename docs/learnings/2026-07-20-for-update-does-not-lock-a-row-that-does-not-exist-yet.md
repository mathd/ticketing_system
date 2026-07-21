# `FOR UPDATE` on "the current row" doesn't serialize against a new-row INSERT

Date: 2026-07-20 · From TKT-104 (PR #81, ADR-029) · Status: fixed in catalog seat-map edit; the lesson generalizes

## What happened

TKT-104 needed edit-a-published-seat-map and pin-a-seat to serialize so a concurrent edit could
never orphan a pinned seat. The first implementation locked "the family's current published
version" with:

```sql
SELECT id, version FROM seat_maps
WHERE map_family_id = $1 AND status = 'published'
ORDER BY version DESC LIMIT 1
FOR UPDATE
```

…and had both `EditSeatMap` and `PinSeat` take that lock, reasoning "same row → serialized."

It is not serialized. `EditSeatMap` creates a new version by **INSERTing a new row** — it never
`UPDATE`s the row it locked. A `FOR UPDATE` lock on version *N* does not conflict with an INSERT of
version *N+1*. So a `PinSeat` that blocks on version *N*'s lock, once the edit commits *N+1* and
releases, does **not** re-run the `ORDER BY … LIMIT` — PostgreSQL's lock-wait recheck re-fetches the
**same** locked row (still version *N*, still `published`, so the predicate still holds) and returns
it. `PinSeat` then validates the seat against the **stale** version *N* geometry (which still has the
seat), inserts the pin, and both operations succeed — leaving a pin for a seat absent from the
current version *N+1*. The same stale row lets two concurrent edits both derive `version+1` and
collide.

Both adversarial ai-review arms and a hand-written probe reproduced it deterministically (pin
landed; seat absent from the current version). The original race test *passed* anyway, because its
ground-truth check asked "is this seat in **any** published version?" — and the predecessor stays
published, so the dropped seat was still found there. The invariant has to bind to the **current**
(max-version) published version, not any published version.

## The rule

- **A row lock serializes access to that row, not to the *concept* the row currently represents.**
  If the "current X" can change by an **INSERT of a new row** (a new version, a new head, a new
  latest), then `SELECT … ORDER BY … LIMIT 1 FOR UPDATE` locks a row that a concurrent insert can
  demote out from under you, and the blocked waiter rechecks the *stale* row — it does not re-run
  the `ORDER BY … LIMIT`. Serialize on a **stable identity** instead: here, a transaction-scoped
  advisory lock keyed on the family UUID (`pg_advisory_xact_lock(hashtextextended(uuid::text, 0))`),
  taken **before** resolving the current row, so every operation on the family queues on the same
  key regardless of which row is current. Add a `UNIQUE(family, version)` as the backstop.
- **When a version bump is an INSERT, the predecessor is still there** — so any "is it still valid?"
  check (in code or in a test) must bind to the *current/max-version* row, not to "any row that
  matches." A check against "any published version" is a false negative that masks exactly this bug.
- Contrast with a monotonic `UPDATE`-in-place transition (`PublishSeatMap`, `PublishPerformance`):
  those *are* safe under a plain conditional UPDATE with no lock, because the identity they own
  can't be invalidated by a racing row. The hazard is specific to "current = newest row" resolved by
  ORDER BY over rows an INSERT can add.

Related: [lock handshakes pin the exact statement](./2026-07-16-lock-handshakes-pin-the-exact-statement.md),
[upsert guards are snapshot-stale](./2026-07-16-upsert-guards-are-snapshot-stale.md),
[version-scope vs convergence-scope](./2026-07-16-version-scope-vs-convergence-scope.md). The
binding rule for agents lives in `AGENTS.md` (ADR-029 pointer) and the ADR itself.
