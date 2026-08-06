# A mutation check against a stale schema tests last revision's constraints

**TKT-217, 2026-08-05.** A new variant of "a fixture too small cannot show the negative" — this one
is a fixture whose *schema* is too old.

## What happened

A guard was added so a live capture could not be recorded under the migration-only entry kind
`legacy_unattributed`. The mutation check reported the test **passing with the guard removed**,
which reads as a blind fixture.

The fixture was fine. The test database had been migrated from an **earlier revision of the same
unreleased migration file**, before its `entry_kind` CHECK learned the new value — and goose does
not re-run a migration whose file changed after it was applied. The stale CHECK was doing the
refusing, not the code under test. Against a freshly created database the mutant failed properly.

## The rule

**Any DB-backed test touching an unreleased migration must run against a database created from the
current file, or it is asserting against a schema that no longer exists.** This applies to the
mutation check especially: the whole point is to observe the intended failure, and a stale
constraint can produce a pass *or* a failure for reasons that have nothing to do with the change.

Recreate the database before mutation-checking anything schema-adjacent. The check costs a second;
the wrong conclusion costs a shipped defect — or, as here, nearly costs a correct guard being
deleted as unnecessary.

## The neighbouring process error, same ticket

A gate run was started and then the tree was edited while it ran. It failed on a build error and a
test that did not exist when it started. **A gate result only describes the tree it ran on** — start
it, then leave the tree alone, or throw the result away.
