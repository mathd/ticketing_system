# TKT-551 verification

The operator report counts recorded/synced, conflict and rejected results separately. Rejected
results never increase Synced and are explicitly not recorded. Their terminal RESOLVED state
and individual warnings remain. Completed-batch totals are visible before a later failure.

`verification.json` binds the receipts to the implementation commit and approved complete plan.
The baseline run has 12 intended missing-report assertion failures. The focused green run has
53 passes. Every one of the 15 mutants fails at the named App assertion. Source is restored
after each mutant, followed by eight full scanner runs with 1,862 passes each. The mutation
driver and complete raw logs are retained. The first full-suite DOC-01 failure is also retained;
the edited ADR line now qualifies its inherited decision references, and the existing check passes.

The mixed fixture uses production mint, latch, persistence, batching and IndexedDB retirement.
It crosses the real 100-row request limit with 105 rows. Totals are 92/7/6; the first batch is
90/6/4. Entry, Exit and refusal rows occur in the fixture, with rejected Exit rows in both batches.
Exact request ids, complete stored rows and all individual rejection warnings are asserted.
Later transport, HTTP 500, HTTP 401, malformed-body, apply-throw and real incompatible-row cases
retain only earlier applied totals. A real-store matching-retirement case pins alreadyApplied
counting. A migrated legacy case separates isolated-422 warnings from rejected results.

Reports stay transient under owner D2. The next nonempty paired pass resets them before send,
including an automatic reconnection retry which then applies nothing. The tests execute that
sequence. Counts are per page and can include matching results another page already retired;
the test does not claim to run two real tabs. Existing 401/storage exits can leave the queue
display stale until refresh. Last successful sync means a completed pass. The guide states
these limits. This change adds no acknowledgement UI, store policy, API or write control.

Build and lint passed. No browser run is claimed for this ticket. Full gate, CI, independent
code reviews and post-merge acceptance are recorded separately against their exact revisions.
