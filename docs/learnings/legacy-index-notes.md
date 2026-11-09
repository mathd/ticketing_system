# Notes retained from the old learnings index

Historical notes that had no separate topic file. Ticket IDs identify their original context.
Current rules are in [engineering rules](../conventions/engineering-rules.md).

- TKT-216: a forced-partition mutant survived because the fixture selected the same winner
  whether the partition was a filter or a tie-break. A surviving mutant can expose a blind fixture.
- TKT-216: row-level triggers do not fire on TRUNCATE. Payments already guarded its journal
  against TRUNCATE, but a new integrity guard missed that existing solution.
- TKT-215: an overflow test required refusal of a large price at a 100% rate. It pinned broken
  arithmetic as the requirement. Mutation testing cannot repair a wrong expected result.
- TKT-215: a mutation check must compile the mutant and run the intended suite. A build failure
  cannot establish that the test detected the changed behavior.
- TKT-215: `git reset --hard origin/<branch>` destroyed an unpushed local commit. Verify the
  remote commit before claiming promotion. A claim does not recover an unpublished commit.
- TKT-214: a rollback test ran a copied migration block and stayed green without the real Down
  guard. Run the actual migration. Pin enum values as literals instead of copying constants.
- TKT-84: dropping a table-wide UNIQUE also drops its backing index. Audit prefix-dependent
  queries before an [ADR-025](../adr/ADR-025-admission-events-and-offline-reconciliation.md)
  section D7-style replacement. A partial index cannot serve rows outside its predicate.
- TKT-180/183: the old ADR-017 section 2 additive-no-bump example was superseded by the schema-4/5
  fork. Read [ADR-017](../adr/ADR-017-domain-event-schema-evolution.md) for the governing rule.
