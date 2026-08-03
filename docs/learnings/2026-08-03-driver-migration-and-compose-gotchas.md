# Driver, migration and Compose gotchas that each cost a debugging cycle

**TKT-173, TKT-179 (PRs #147, #149) — 2026-08-03**

Small, concrete, and each one silent until something specific happens.

## `text[]` does not round-trip through `database/sql` + `pgx/v5/stdlib`

"pgx supports `[]string`" is true of pgx's **native** interface and false through
`database/sql` with `pgx/v5/stdlib`: the write succeeds and only the **read** fails
(`unsupported Scan, storing driver.Value type string into type *[]string`). If the only reader is a
replay path, the defect surfaces on a retry in production and nowhere earlier.

This repo has no `text[]` anywhere — **`jsonb` is the established array shape**. A 20-line probe
settled it at plan review. *A driver assumption is a fact, not an opinion: go and run it.*

## `provider.Down` rolls back exactly one migration

A test asserting "migration N's Down guard" silently stops testing N the moment N+1 lands — it
rolls back the *new* one, sees success, and reports a passing guard for a migration it never
touched. Use **`DownTo`**.

## Adding a column means auditing every positional read

Knowing the trap is not enough — it was written into a review prompt and a site was still missed.
What caught it was an **exhaustive enumeration** rather than a targeted look. After adding a
column, `git grep` the table name and check **every projection/Scan pair by arity**. Where a column
list is documented as order-load-bearing, append rather than insert.

## `docker compose up --no-deps` skips the migration job

[ADR-022](../adr/ADR-022-out-of-band-service-migrations.md) runs migrations as a one-shot Compose
job the service waits on. Rebuilding a single service with `--no-deps` leaves the schema behind and
produces a confusing 500 from an entirely correct binary. Run the service's `migrate` job
explicitly.

## When two services canonicalise "the same request", say which one owns it

Commerce mirrors inventory's trim/de-duplicate/sort so it can answer idempotency and pricing
locally, before any call. That is a **second definition of the same thing**, survivable only
because the claimed set is asserted equal to the local one afterwards. If a third caller ever needs
it, the definition belongs in one place.
