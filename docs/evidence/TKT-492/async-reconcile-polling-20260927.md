# TKT-492 async reconciliation polling, 2026-09-27

The retained baseline attempt `run-20260927T232228992Z-80b74948` finished its 240 warm-up scans, then stopped at `offline_reconcile_warmup_result_incomplete`. Its third reconciliation response had status 200 and all 20 expected unique occurrences. At the saved diagnostic point, 7 local rows were `SYNCED` and 13 were still `QUEUED`.

The runner waited for this state with `page.waitForFunction(async (...) => ...)`. Playwright 1.62.1's installed `coreBundle.js` checks the predicate result before it awaits it. The host probe confirmed the effect: an async predicate that returned false completed in 29 ms, while a synchronous false predicate timed out at 303 ms. The async callback's Promise was treated as truthy, so the runner could leave the wait before IndexedDB reconciliation finished.

`reconcilePage` now calls `pollPageEvaluation`. The helper awaits each `page.evaluate` result, checks the existing response occurrence set and each local state/result pair, and polls until the existing 60-second deadline. The existing diagnostic catch, occurrence mismatch and saved-state mismatch classification, diagnostics, and final assertions remain in place. No workload, threshold, or product code changed.

The retained attempt and its original classification remain unchanged. Its `correctness.json` records `offline_reconcile_warmup_result_incomplete`, but that classification followed the runner's early return. The evidence identifies a polling race and does not demonstrate product corruption.

The isolated check extracted `pollPageEvaluation` without importing the runner. A page stub whose `evaluate` awaited the predicate returned `false`, `false`, then `true` across three evaluations. An always-false predicate timed out in 35 ms with a 35 ms deadline. `node --check docs/evidence/TKT-492/run.mjs` passed. No `waitForFunction(async ...)` remains in the TKT-492 code. No live browser, Chrome, Docker, network, listener, or runner self-check was used for this fix.
