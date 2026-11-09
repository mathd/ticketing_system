# Notes retained from the old learnings index

Historical incident notes that had no separate topic file in the previous index. Ticket IDs
identify their original context. Current rules are in [engineering rules](../conventions/engineering-rules.md).

- **A surviving mutant may be exposing a blind fixture, not equivalent code**. a forced-partition
  mutant survived because the only forced case in the table gave the same winner whether the
  partition was a filter or a tie-break. The usual reading of a surviving mutant is "the change is
  equivalent"; sometimes it means "this fixture cannot observe the property". (TKT-216)

- **Check how the codebase already solved it before writing a new integrity guard**. payments has
  guarded TRUNCATE on its append-only journal since the journal existed; a new trigger written from
  first principles in the same repo shipped the identical hole. Row-level triggers do not fire on
  TRUNCATE. (TKT-216)

- **A test can defend the bug it was written to catch**. TKT-215's overflow test asserted that a
  large price at a 100% rate *must be refused*, pinning broken arithmetic as required behaviour.
  Mutation testing does not help: the mutant dies correctly against a wrong oracle. Ask what the
  test WANTS, not only whether it can fail. (TKT-215)

- **A mutation check that does not compile is not a check**. it reports "no failure" and looks
  exactly like a passing one. Assert the mutant builds and the suite actually ran before reading the
  result. (TKT-215)

- **`git reset --hard origin/<branch>` destroys unpushed local commits**. a direct commit that was
  never pushed vanished while a closeout comment claimed it had been promoted. Push immediately and
  verify `git log origin/<branch> -1` before claiming it anywhere. (TKT-215)

- **A test that copies the code under test cannot fail**. a rollback-guard test that ran a `DO`
  block *copied out of* the migration would have passed with the migration's entire `Down` guard
  deleted; expectations naming production *constants* likewise survive aliasing one to another's
  value. Run the real migration; pin enum values as literals. (TKT-214)

- **Dropping a table-wide UNIQUE drops its backing index**. audit every query that used the index
  prefix before an [ADR-025](../adr/ADR-025-admission-events-and-offline-reconciliation.md) §D7-style
  swap; a partial replacement cannot serve rows outside its predicate. (TKT-84)

- **ADR-017 §2 additive-no-bump**. superseded as a worked example by the schema-4/5 fork
  (TKT-180/183); [ADR-017](../adr/ADR-017-domain-event-schema-evolution.md) itself is the reference.
