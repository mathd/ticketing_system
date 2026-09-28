# TKT-492 resume note

TKT-492 is BLOCKED after review 4. Do not start another ticket. The owner asked to stop after this ticket and then asked to commit the work for later.

## Saved result

Evidence commit: `fbcac39a2c0374dfa383d5a4b617cded50b7c724`.

Both final local measurements passed the strict analyzer. There are 4,000 measured samples, with a maximum of 253.2 ms and none at or above 500 ms. See `RESULTS.md` and the saved raw runs. No production code changed.

`make check` passed on that commit with tree digest `3acd0f70b5e1c925d3dd13f895d8db55bdf794d1`. After completion, HEAD and the current gate digest matched `.gate.verdict`, and `.gate.lock` was absent. This result predates this resume note and the configuration commit. Statements in the original report that the gate and fourth review are pending describe their state when that report was committed.

## Unresolved review findings

Review 4 used gpt-6-sol with high effort, session `01a0e566-b41f-7c91-8f0c-ae6db5234d09`. Both high findings were checked against the code and accepted.

1. `scheduledOfflineScan` in `run.mjs` overwrites observed transport and control fields with intentional-offline values. The offline branch in `analyze.mjs` can then classify an invalid control or unknown transport with no result as a correctness failure. Preserve the observed outcome and establish that the control worked before assigning a product correctness failure.
2. The approved plan requires the rendered operator conflict notice. Reconciliation checks the response, IndexedDB, database events, and alarms, but never asserts the scanner's visible `.sync-note`. The test would pass if that notice disappeared.

These findings concern the measurement script. They do not establish an Access product defect or change the observed latency results.

Blocking finding counts were 9, 4, 2, and 2. The SDLC absolute review cap is four. Counts did not strictly decrease, and the second finding is an unchanged coverage gap, so the convergence exception does not apply. No fix was made after review 4. Owner authorization for another fix-and-review cycle is pending. The request to save commits does not waive these findings or authorize publication.

## Dependent ticket

D11 also remains open. `TKT-489-amendment.md` contains the concrete proposed replacement criteria, but TKT-489's actual description and readiness remain unchanged. It is still Backlog. The board's supported existing-ticket write endpoint ignores those fields. Do not bypass that write path without authorization. The actual criteria update is required before TKT-492 can be Done.

## Where to continue

Use branch `TKT-492-scanner-latency-spike`. The board records the full fourth review and final gate result. TKT-492 is BLOCKED with its prior `agent:ai-review` label, as required by the local tracker rules. Nothing has been pushed or merged.

The temporary working records are in `/tmp/tkt492-4ndfo86_`, including the approved plan and review reports. The committed evidence and board records are the durable sources if those temporary files are removed.
