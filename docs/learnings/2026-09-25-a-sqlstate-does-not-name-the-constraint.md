# A SQLSTATE does not name the constraint

**2026-09-25 — TKT-147**

A test that proves "the database refuses this value" usually inserts a bad row and asserts an error.
That assertion is only as specific as the thing it checks. In TKT-147 one assertion was vacuous
**twice** before it could fail. Each version passed with the constraint under test removed from
the migration.

## The sequence

The test had to prove that migration 0012's CHECK on `lifecycle_integrity_quarantine.reason_code`
refuses a value outside the vocabulary.

| version | what refused the row | red with the CHECK removed? |
|---|---|---|
| `err == nil` → fail, with a random `ticket_id` | the foreign key to `tickets(id)` | no |
| a real issued ticket, and require SQLSTATE `23514` | the table's `time_check` (the row set neither `admitted_at` nor `occurred_at`), also `23514` | no |
| a row that satisfies every other constraint, and require `ConstraintName == lifecycle_integrity_quarantine_reason_code_check` | the `reason_code` CHECK | **yes** |

Each version looked finished. Only a mutation run, with the CHECK deleted from the migration, showed
which constraint was actually firing. The command was: remove the `CHECK (...)` clause from
`0012_integrity_alarm_reason_codes.sql`, then run
`go test -tags smoke -run TestIntegrityAlarmReasonMigrationAndConstraint ./internal/store/`. It stayed
green on versions 1 and 2. On version 3 it failed at the insert assertion.

## The rule

- A SQLSTATE names a **class** of failure (`23514` = any check violation, `23503` = any FK). On a
  table with more than one constraint of that class, it cannot say which one refused the row. Assert
  `pgconn.PgError.ConstraintName`.
- Build the bad row so that it satisfies **every other** constraint. Otherwise another constraint may
  refuse it first.
- The same ticket had a sibling mistake: a `pg_constraint` lookup by `conname` alone is **not
  schema-scoped**. The access tests run several schemas in one database, so the gate counted the
  constraint twice. Scope such a lookup with `conrelid='<table>'::regclass`, which resolves through the
  test's `search_path`.

This is the fixture-that-cannot-fail rule in `AGENTS.md`, applied to database errors. What notices the
constraint's absence? Here it was not the refusal, only the constraint's name.
