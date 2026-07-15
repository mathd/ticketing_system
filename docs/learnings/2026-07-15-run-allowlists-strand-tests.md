# A `-run` allowlist silently strands tests

Date: 2026-07-15 · From TKT-53, TKT-60 · Status: fixed in `scripts/smoke.sh`; the lesson generalizes

## What happened

`scripts/smoke.sh` ran the catalog store package under a `-run` allowlist naming six tests. A test
added to that package but not to the allowlist never ran — and the gate stayed green, because
nothing failed. Two tests were stranded that way and were merged having never executed once:

- `TestDirectArchiveRacingFestivalPublishCannotDesync`
- `TestGetPublishedFestivalOrdersDaysAcrossEventsChronologically` (TKT-53's scoped-read test)

The second one mattered: TKT-53's scoped-read test was the evidence for a fix whose successor
ticket (TKT-60) turned out to need a different fix entirely. The test that would have been asked to
prove it had never run.

**This is the second time in this repo.** The commerce block already carried the lesson as a comment
from a prior incident — *"an allowlist means a newly added test silently never runs and the gate
still passes green — which is exactly what happened to this file's first six tests"* — and the
catalog block, written later, did not adopt it. Same trap, same file, twice.

## The practice

**Don't filter a gate's test package with `-run`.** Run the whole package; if a test is too slow or
too flaky to run every time, that is a fact about the test, and the fix belongs in the test.

Where a filter is genuinely load-bearing, it needs a comment saying why, and it must be understood
as an allowlist someone has to remember to update — which is the failure mode, not a mitigation.
`scripts/smoke.sh` still carries one for inventory
(`-run TestGroupedDaysConvergeOnOneInventoryPool`); it is the exception to look at first the next
time a test there mysteriously never fails.

**The general form of this trap is [`2026-07-15-prove-tests-fail.md`](./2026-07-15-prove-tests-fail.md)** —
a test that never runs is a passing test that proves nothing, and the check that catches both is the
same one.

## Evidence

- `scripts/smoke.sh` — the catalog and commerce blocks, and their comments explaining the removal
- TKT-60 `kind=metrics` on the `.sdlc/` board — where the stranding was found
