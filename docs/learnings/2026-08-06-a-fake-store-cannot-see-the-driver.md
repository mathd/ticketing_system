# A fake store cannot see the driver

**TKT-222 (PR #180) — 2026-08-06**

## What happened

One new query. Three defects. All three compiled, all three passed every unit test, and all three
were fatal the first time a real driver executed them:

1. **`= ANY($1)` with no `::uuid[]` cast.** The driver cannot infer the parameter's type from the
   predicate alone. (The scoped public-performance read three hundred lines away carries the cast.)
2. **`events.name` is `jsonb` and `LocalizedText` is a plain `map[string]string`** with no
   `sql.Scanner`. Scanning straight into it fails with *"unsupported Scan, storing driver.Value type
   []uint8"*.
3. **`performances.starts_at` is NULL for a festival day** — an operating date and opening hours
   instead of an instant (ADR-014) — scanned into a value `time.Time`.

They were found by driving a browser against the real stack, not by the test suite.

## Why a fake store is structurally blind to this

A fake store hands back **Go values**. A real one hands back **`driver.Value`** and asks `Scan` to
convert. Every mismatch between the Go type and the SQL type therefore lives in a layer the fake does
not have:

| Defect | What the fake does | What the driver does |
|---|---|---|
| Untyped array parameter | never sees the SQL | must infer a type it cannot |
| jsonb → map | assigns a map to a map | offers `[]uint8`, finds no Scanner |
| NULL → value type | assigns a zero value | offers `nil`, finds no pointer |

This is not a gap in any particular fake. It is what "fake" means here: the seam is *above* the place
these bugs live.

## ADR-028 makes the symptom worse before it makes it better

The fail-closed response wrap rewrites an undeclared status into a generic 500 and logs *"response
violates OpenAPI contract"*. That is correct behaviour and it is excellent at hiding this class: the
reader is sent to the **spec** to look for a missing status when the actual fault is a `Scan` three
layers down. Two of the three defects above presented exactly that way.

If you see "response violates OpenAPI contract" on a route you just wrote, read the service's own
`ERROR` log before touching the contract.

## What to do instead

**A new query gets a real-Postgres test that scans at least one row, before it gets a fake-store
test.** Not instead of — before. The fake test is for branching and error mapping; the real one is
for the query.

And the row matters. The progression in this ticket is the lesson in miniature:

- The first real-Postgres test resolved only **unknown ids**. That proves the query *executes* —
  enough to catch (1) — and never scans a row, so it was blind to (2) by construction.
- Seeding a row exposed (2).
- The seeded row happened to be a **festival day**, which exposed (3).

Each test was written to catch the previous bug and could not see the next one. Choose the fixture by
asking *which shapes of row exist in production*, not *which row makes the test pass*: nullable
columns, jsonb columns, and the type that is a special case (a festival day, a guest order, a
compensated refund).

## The related trap, from the other direction

Correcting a plan-proof fixture in the same ticket — so that the "other customers'" rows really
belonged to other customers — made it **fail against correct code**. With forty rows the planner
rightly prefers a sequential scan, so the assertion had quietly become a statement about the fixture.
An ADR-019 scan proof needs enough rows for the planner to *have a choice* before it means anything.

See [a fixture too small cannot show the negative](2026-08-03-a-fixture-too-small-cannot-show-the-negative.md)
— it cannot show the positive either.
