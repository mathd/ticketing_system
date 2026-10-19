# TKT-399 verification before independent review

The scanner captures Entry or Exit once, then keeps it through the request, local outcome, recovery notice and reconciliation. This evidence covers code commit `eefd1e9987d74f38c760e3dddf10d41189faacd6`, built from `3752a96a16a2ed6e7fe267206c3dbd05bf5271bf`. The subsequent evidence commit adds only these receipts. The final full gate runs on that commit. Its actual verdict belongs in the board and PR review record.

## Checks

The new Exit tests first failed with 30 failures and one pass. An earlier command selected baseline tests and passed. It was not the red proof. The final scanner run passed 1,840 tests in 41 files. TypeScript compilation passed. The full `make browser` run passed with host Chrome 155.0.8059.79.

The Chrome journeys submit the rendered Entry and Exit controls. They check exact request direction and occurrence identity, online Exit and re-entry, offline Exit followed by Entry mode and reconciliation, reload notices, listed revoked Exit with no new occurrence, and the literal 30,000 ms decision boundary. SQL comparisons use the full stored Access timestamp. A second online event checks that a retired Exit does not send again.

`exit-cells.json` maps all 38 ADR-085 X rows to the enforcing tests. It separates App/controller result classification from shared production-store checks. It does not claim that every response arrival order ran through every storage column end to end. The store tests cover synchronous throw, abort, connection replacement, another live owner after expiry, version change, and actual page loss before or after commit. A queued Entry control makes queue assertions observable. The late-mark test holds the actual write before commit, then proves a late queue decision cannot overwrite ACT.

## Mutations and restored runs

Fourteen primary mutation executions failed on assertions for direction capture, strict decoding, recovery, notices, wire direction, warnings, listed revocation, mode reset, stale camera work, transport classification, status 500 handling, captured send context and the inclusive time boundary. `mutations.json` records each command and log digest source.

Five additional storage-test mutations failed on assertions. Three repeat direction mechanisms through the new storage fixture. Two remove terminal-state and late-mark protection. These are five executions, not five additional independent mechanisms. Eight restored storage runs passed. A sync fixture correction has its own mutation and eight restored full scanner runs.

The CSS mutation blocked pointer input on the rendered direction control. App tests still passed, while Chrome could not submit Exit. The first receipt parser expected the text `TimeoutError`; Playwright reported `locator.check: Timeout 10000ms exceeded`. The recorded failure was checked against the Exit locator and pointer interception. A separate notice mutation changed Exit to Entry and failed the Chrome reload assertion. Eight restored targeted Chrome runs passed after restoration.

The first restored full scanner series failed on run five because the fixture clicked Sync before its queue write committed. The fixture now waits for that exact QUEUED row. Eight subsequent full scanner runs passed. The first late-mark fixture stopped before write success; its corrected barrier waits for the write and all eight restored runs passed. A storage mutation runner reused three old log names. The logs were separated and those three primary mutations were run again. Both observations remain in the receipts.

## Compatibility and measurements

`predecessor-manifest.json` pins the source and built assets of the exact unchanged client extracted at runtime from the baseline commit. No predecessor source copy is added to this branch. That client excludes Exit rows without rewriting them or sending them. Its recovery notice has unknown type and its App has no warning. The current client warns about invalid normal rows and supplies the positive Exit reconciliation control.

`strict-commits.raw.json` is a new measurement from the full TKT-399 browser run. The reused measurement function still labels its raw output TKT-559. This file records its actual TKT-399 context without changing the historical TKT-559 receipt. It contains 100 mint and 100 mark commits after 10 excluded warmups. These host results do not qualify a venue. Venue qualification remains TKT-560.

## Limits and remaining work

Actual page loss before a terminal commit can recover an uncertain Exit fact. A live owner's protected decision does not remove that residual. Connection close alone is not page loss. Protocol exclusions and shared checks are explicit in the cell map.

The initial full gate passed lint, tests and builds, then failed when Compose could not bind port 15440. An unrelated TKT-504 worktree owns that port. It was left running. The final gate uses the same commit in a separate validation worktree with smoke slot 21. This changes no product or gate configuration.

TKT-551 still owns offline Single/Rejected semantics. TKT-552 still owns Entry-copy changes. ADR-085 updates here mark only shipped Exit work. The original frozen TKT-558 decoder fixture is unchanged. No tag or deployment is part of this work.


## Round 8 corrections and retained limits

The new diff, plan, review fixes and verification records are authored by gpt-6.1-sol at high effort. The unchanged TKT-558 decoder fixture has gpt-6.1-sol and claude-opus-5-5 contributions. The aliases opus-5.5 and claude-opus-5-5 name one model. The frozen predecessor is built from exact baseline Git objects at runtime. No code or fixes from abandoned 24f1c209, its branch or PR #451 enter this diff. TKT-399 D44/D45 record this provenance.

Entry retains a known lookup-error difference. A closed connection during revocation lookup produces Entry X01, while Exit produces O-S when readiness changed. This ticket changes only Exit. No correction ticket was opened for that known Entry residual, as the approved plan requires.

ADR-085 retains two historical Entry-time markers from the baseline: section 1.3's time-pending note and section 2.4's deadlines/late-branches note. Their current delivery is recorded in section 3.1. The Exit markers in those notes now name TKT-399. The invariant markers and section 6 refer to section 3.1 instead of restating the stale time-pending claim. Section 3.7 records the shipped Exit branches and points to the existing Entry time delivery. No Entry outcome or copy requirement changes.

Round 8 found missing rare-result assertions. `App.exit-results.test.tsx` submits Exit through production storage and checks the complete result for lookup/mint at 3000 ms, accepted body/mark at 30000 ms, clock capture failure and invalid Request headers before send. `scan-result.exit.test.ts` asserts exact sent/unsent cancellation presentation. Pairing reset clears the transient cancellation result in the same handler, so no persistent App cancellation region is claimed. Controller tests retain the captured-context assertions. The cell map names these tiers explicitly.

D50 corrects the approved plan's broad originalScanAt clause. Only already_redeemed shows Already redeemed at the supplied time. A quarantined Exit does not claim that result. The Entry introduction is restored. The browser re-entry journey checks that the requires_exit seed changes exactly one row, then requires an Entry/Entry refusal with no added lifecycle fact before Exit permits re-entry. The predecessor control checks the exact two eligible Exit payloads, both complete retired rows, and the unchanged PENDING row inside its lease.

The new copy tests first failed on the quarantine label and Entry introduction, with nine other new cases passing. The round 8 verification receipt records subsequent mutation, repetition and gate results separately from the pre-review evidence above.


The round 8 scanner suite passes 1,851 tests in 43 files. Six targeted mutations each fail on the new assertions, followed by eight passing restored runs of the 11 new unit/App cases. Two browser mutations fail at the new browser checks, followed by eight passing restored runs of each journey. The full `make browser` run also passes. `review-r8-verification.json` records the commands, observed times and log digests. These fixes are below the repeat threshold and are not independently re-reviewed. They do not change the architecture, permissions, data handling or outcome classifications.

The first browser fixture used a full clock mock on the reload page and failed to show recovery. A diagnostic repeat failed the same way. Playwright's Date-only helper also failed. The final fixture overrides Date.now only, preserves native navigation entries, and asserts navigation type reload. Both mutation checks and all restored runs use that final fixture. The first baseline log was overwritten during retry; the diagnostic and later failure logs remain. This is a fixture correction, not a product recovery fix.

The final browser run writes fresh measurements to `review-r8-strict-commits.raw.json`. Its reused raw labels still say TKT-559. This run belongs to TKT-399, and the committed historical TKT-559 receipt is restored byte for byte before the gate. I8-5's separate follow-up requires owner approval and does not change S-2 membership.
