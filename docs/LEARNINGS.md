# Learnings index

One topic per file. Load the note needed for the task. These are dated incident records, not
current ticket status. Binding test and review rules are in [engineering rules](conventions/engineering-rules.md).
Add a topic file as `learnings/YYYY-MM-DD-title.md` and add its index row. Promote stable rules to
the applicable engineering section or ADR; keep the incident here as evidence.

| Date or record | Topic |
|---|---|
| 2026-07-15 | [Name what a control reaches, not what it is for](learnings/2026-07-15-name-what-a-control-reaches.md) |
| 2026-07-15 | [A passing test is not evidence it tests anything](learnings/2026-07-15-prove-tests-fail.md) |
| 2026-07-15 | [A `-run` allowlist silently strands tests](learnings/2026-07-15-run-allowlists-strand-tests.md) |
| 2026-07-16 | [Judge idempotent replays by lifecycle state, never by timestamp](learnings/2026-07-16-judge-replays-by-lifecycle-state.md) |
| 2026-07-16 | [A lock-queue handshake must pin the exact statement under test](learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md) |
| 2026-07-16 | [A time cutoff decided behind a lock queue must use decision time, not transaction time](learnings/2026-07-16-lock-queue-time-cutoffs.md) |
| 2026-07-16 | [An anti-replay rule must be re-checked against the retry it exists to serve](learnings/2026-07-16-protocol-rules-safety-and-liveness.md) |
| 2026-07-16 | [A finding's mechanics being true does not make its trigger reachable](learnings/2026-07-16-refute-the-trigger-not-the-mechanics.md) |
| 2026-07-16 | [Upsert guards are snapshot-stale under lock contention](learnings/2026-07-16-upsert-guards-are-snapshot-stale.md) |
| 2026-07-16 | [Per-entity version counters do not survive convergence onto a shared resource](learnings/2026-07-16-version-scope-vs-convergence-scope.md) |
| 2026-07-17 | [Batch endpoints with per-item results must not carry per-item format constraints in the request schema](learnings/2026-07-17-batch-item-schemas-block-per-item-results.md) |
| 2026-07-17 | [Storage can reject values the code accepts](learnings/2026-07-17-column-range-narrower-than-code-accepts.md) |
| 2026-07-17 | [A load test's published number is a claim about the server only if the generator's health is part of the verdict](learnings/2026-07-17-load-test-generator-health-is-part-of-the-claim.md) |
| 2026-07-17 | [Reconciliation needs positive assertions](learnings/2026-07-17-reconciliation-acts-only-on-positive-assertions.md) |
| 2026-07-17 | [Seed the failure state; don't race a kill](learnings/2026-07-17-seed-the-failure-state-dont-race-a-kill.md) |
| 2026-07-18 | [Use relative dates in time-window fixtures](learnings/2026-07-18-time-window-fixtures-must-be-relative.md) |
| 2026-07-20 | [Browser submission catches Origin failures that API smoke misses](learnings/2026-07-20-browser-submit-is-the-only-checkorigin-catch.md) |
| 2026-07-20 | [`FOR UPDATE` on "the current row" doesn't serialize against a new-row INSERT](learnings/2026-07-20-for-update-does-not-lock-a-row-that-does-not-exist-yet.md) |
| 2026-07-21 | [Catalog's internal service-to-service routes are hand-mounted, outside the OpenAPI contract](learnings/2026-07-21-catalog-internal-routes-are-hand-mounted-outside-openapi.md) |
| 2026-07-21 | [A `pgrep`-for-the-command-name watcher self-matches its own command line](learnings/2026-07-21-pgrep-watchers-self-match.md) |
| 2026-07-21 | [A test-side response validator downstream of the production wrap sees laundered statuses, not real ones](learnings/2026-07-21-test-validator-downstream-of-production-wrap-sees-laundered-statuses.md) |
| 2026-07-25 | [A coarse observable can be produced by the broken version too](learnings/2026-07-25-coarse-observables-pass-the-broken-build.md) |
| 2026-07-29 | [A no-store header alone does not prove success-path behavior](learnings/2026-07-29-fail-closed-validator-emits-the-same-header-as-the-success-case.md) |
| 2026-07-29 | [Force the interleaving when testing a race fix](learnings/2026-07-29-force-the-interleaving-repetition-cannot-falsify-a-race-fix.md) |
| 2026-07-29 | [`format: uuid` is unenforced, and catalog validates its own rejections](learnings/2026-07-29-format-uuid-is-unenforced-and-catalog-validates-its-own-rejections.md) |
| 2026-07-30 | [A decoded page is not an answered one](learnings/2026-07-30-a-decoded-page-is-not-an-answered-one.md) |
| 2026-07-30 | [A fingerprint of a symmetric secret is an oracle](learnings/2026-07-30-a-fingerprint-of-a-symmetric-secret-is-an-oracle.md) |
| 2026-07-30 | [A gitignore rule can outlive its reason](learnings/2026-07-30-a-gitignore-rule-can-outlive-its-reason.md) |
| 2026-07-30 | [When a new check fails 27 tests, ask which side is wrong](learnings/2026-07-30-ask-whether-the-fixtures-or-production-are-wrong.md) |
| 2026-07-30 | [Check *why* a test is red, not just that it is](learnings/2026-07-30-check-why-a-test-is-red-not-just-that-it-is.md) |
| 2026-07-30 | [Record a relaxation as a relaxation](learnings/2026-07-30-record-a-relaxation-as-a-relaxation.md) |
| 2026-07-30 | [You cannot classify an event you caused](learnings/2026-07-30-you-cannot-classify-an-event-you-caused.md) |
| 2026-08-03 | [A fixture too small cannot show the negative](learnings/2026-08-03-a-fixture-too-small-cannot-show-the-negative.md) |
| 2026-08-03 | [Driver, migration and Compose gotchas that each cost a debugging cycle](learnings/2026-08-03-driver-migration-and-compose-gotchas.md) |
| 2026-08-03 | [Retry-vs-terminate is asymmetric, and a comment is not evidence](learnings/2026-08-03-retry-vs-terminate-and-http-boundary-dispositions.md) |
| 2026-08-03 | [Say what a check establishes, not what it is named after](learnings/2026-08-03-say-what-a-check-establishes.md) |
| 2026-08-03 | [Early schema acceptance needs semantic completeness, not idempotency](learnings/2026-08-03-schema-rollout-ordering-and-early-acceptance.md) |
| 2026-08-03 | [A component can be correct and the page still wrong](learnings/2026-08-03-storefront-island-styling-and-fetch-gotchas.md) |
| 2026-08-03 | [Two correct fixes can compose into a new defect](learnings/2026-08-03-two-correct-fixes-can-compose-into-a-new-defect.md) |
| 2026-08-03 | [TypeScript 7 blocks on tooling, not on our code](learnings/2026-08-03-typescript-7-has-no-js-compiler-api.md) |
| 2026-08-05 | [A guard inside a generated handler is not the first thing that runs](learnings/2026-08-05-a-guard-inside-a-generated-handler-is-not-first.md) |
| 2026-08-05 | [A hand-maintained inventory cannot detect the drift it exists to catch](learnings/2026-08-05-a-hand-maintained-inventory-cannot-detect-its-own-drift.md) |
| 2026-08-05 | [A refusal placed after the point of no return is not a safety check](learnings/2026-08-05-a-refusal-after-the-point-of-no-return-loses-money.md) |
| 2026-08-05 | [A mutation check against a stale schema tests last revision's constraints](learnings/2026-08-05-a-schema-too-old-cannot-fail-a-mutation-check.md) |
| 2026-08-05 | ["At startup" is a claim about a runtime, not about where you put the call](learnings/2026-08-05-at-startup-is-a-claim-about-a-runtime.md) |
| 2026-08-06 | [A cap inherited across a trust boundary is not a bound](learnings/2026-08-06-a-cap-inherited-across-a-trust-boundary-is-not-a-bound.md) |
| 2026-08-06 | [A fake store cannot see the driver](learnings/2026-08-06-a-fake-store-cannot-see-the-driver.md) |
| 2026-08-06 | [A fix can be correct and still lie about itself](learnings/2026-08-06-a-fix-can-be-correct-and-still-lie-about-itself.md) |
| 2026-08-06 | [A guard's worst failure is not seeing](learnings/2026-08-06-a-guards-worst-failure-is-not-seeing.md) |
| 2026-08-06 | [Three ways one test could not fail, and "observe it red" caught none of them](learnings/2026-08-06-three-ways-one-test-could-not-fail.md) |
| 2026-08-07 | [A failed optional dependency is skipped silently, and fails later as someone else's bug](learnings/2026-08-07-a-failed-optional-dependency-is-skipped-silently.md) |
| 2026-08-07 | [`Referrer-Policy: no-referrer` nulls the `Origin` header, and 403s every form POST](learnings/2026-08-07-no-referrer-nulls-the-origin-header.md) |
| 2026-08-09 | [A proxy assertion can miss the guarantee](learnings/2026-08-09-a-proxy-can-fail-while-the-guarantee-holds.md) |
| 2026-08-09 | [A total order is not a meaningful one](learnings/2026-08-09-a-total-order-is-not-a-meaningful-one.md) |
| 2026-08-09 | [An assertion over a `min()` only tests the winning arm](learnings/2026-08-09-an-assertion-over-a-min-only-tests-the-winning-arm.md) |
| 2026-08-09 | [pgx returns `infinity` timestamps as a string](learnings/2026-08-09-pgx-returns-infinity-timestamps-as-a-string.md) |
| 2026-08-10 | [A green test can assert the defect as correct](learnings/2026-08-10-a-green-test-can-bless-the-defect.md) |
| 2026-08-10 | [A green test that cannot reach the failing state](learnings/2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md) |
| 2026-08-11 | [Making a value load-bearing is an authorization change, not plumbing](learnings/2026-08-11-forwarding-a-value-is-an-authorization-change.md) |
| 2026-08-15 | [A precondition that cannot fail is worse than no precondition](learnings/2026-08-15-a-precondition-that-cannot-fail.md) |
| 2026-08-15 | [Remove protected fields from the submitted type](learnings/2026-08-15-make-it-unsubmittable-not-validated.md) |
| 2026-08-16 | [A fixture that seeds two mechanisms proves at most one of them](learnings/2026-08-16-a-fixture-that-seeds-two-mechanisms.md) |
| 2026-08-17 | [A harness that cannot catch what it hunts, and a guard that tests the mechanism instead of the wiring](learnings/2026-08-17-a-harness-that-cannot-catch-what-it-hunts.md) |
| 2026-08-17 | [A test that restates its helper does not pin the wiring](learnings/2026-08-17-a-test-that-restates-its-helper.md) |
| 2026-08-18 | [A test for a SWAP needs the two swapped things distinguishable in every position](learnings/2026-08-18-a-test-for-a-swap-needs-distinguishable-positions.md) |
| 2026-08-18 | ["Ancestor of the review base" is not "shipped"](learnings/2026-08-18-ancestor-of-the-base-is-not-shipped.md) |
| 2026-08-18 | [Harmless is not free: a fix that leaves work in a bounded queue must be checked against its ORDERING](learnings/2026-08-18-harmless-is-not-free-in-a-bounded-queue.md) |
| 2026-08-19 | [A test that pins the harness, not the contract](learnings/2026-08-19-a-test-that-pins-the-harness-not-the-contract.md) |
| 2026-08-21 | [A refusal for the wrong reason is a green test that cannot fail](learnings/2026-08-21-a-refusal-for-the-wrong-reason-is-a-green-test.md) |
| 2026-08-23 | [A test that asserts the EFFECT cannot tell you which layer produced it](learnings/2026-08-23-a-test-that-asserts-the-effect-not-the-layer.md) |
| 2026-08-23 | [A guard added to close a race recreated the bug it was added to prevent](learnings/2026-08-23-the-guard-recreated-the-bug-it-prevented.md) |
| 2026-08-23 | [The mechanism was inert, not the test](learnings/2026-08-23-the-mechanism-was-inert-not-the-test.md) |
| 2026-08-30 | [Inert for two different reasons, and only one of them means delete](learnings/2026-08-30-inert-for-two-different-reasons.md) |
| 2026-08-31 | [A design space has no compiler](learnings/2026-08-31-a-design-space-has-no-compiler.md) |
| 2026-08-31 | [A green mutation can mean your generator is blind, not that the guard is redundant](learnings/2026-08-31-a-mutation-your-generator-cannot-reach.md) |
| 2026-08-31 | [A structural match claims errors you never enumerated, and the tier that sees it is not the tier that made it](learnings/2026-08-31-a-structural-match-claims-errors-you-never-enumerated.md) |
| 2026-08-31 | [Deterministic or discriminating, but not both](learnings/2026-08-31-deterministic-or-discriminating-but-not-both.md) |
| 2026-09-25 | [A SQLSTATE does not name the constraint](learnings/2026-09-25-a-sqlstate-does-not-name-the-constraint.md) |
| 2026-09-25 | [A stub that ignores its arguments](learnings/2026-09-25-a-stub-that-ignores-its-arguments.md) |
| 2026-09-26 | [A rising finding count indicts the mechanism, not the last patch](learnings/2026-09-26-a-rising-finding-count-indicts-the-mechanism.md) |
| 2026-09-26 | [pgx does not wrap context.DeadlineExceeded for an interrupted query](learnings/2026-09-26-pgx-does-not-wrap-deadline-exceeded.md) |
| 2026-10-13 | [A Go patch release can break the linter](learnings/2026-10-13-a-go-patch-release-can-break-the-linter.md) |
| 2026-10-13 | [A version outside the advisory range is not a fix](learnings/2026-10-13-a-version-outside-the-advisory-range-is-not-a-fix.md) |
| 2026-10-18 | [Classify by state you own, not by how the driver spells the error](learnings/2026-10-18-classify-by-state-you-own-not-by-error-spelling.md) |
| 2026-10-18 | [Probe the production connection string, not a convenient one](learnings/2026-10-18-probe-the-production-connection-string.md) |
| 2026-10-18 | [Seven review passes mean the plan has no model](learnings/2026-10-18-seven-review-passes-mean-the-plan-has-no-model.md) |
| TKT-84 | [TKT-84 migration 0004 measurements](learnings/TKT-84-lifecycle-migration-measurement.md) |
| TKT-85 | [TKT-85 migration 0005 measurements](learnings/TKT-85-quarantine-migration-measurement.md) |
| TKT-87 | [TKT-87 migration 0006 measurements](learnings/TKT-87-policy-migration-measurement.md) |
| Legacy notes | [Notes retained from the old learnings index](learnings/legacy-index-notes.md) |

## Historical instances

The original index marked these instances as removed or superseded. The lesson remains:

- [Run allowlists](learnings/2026-07-15-run-allowlists-strand-tests.md): the smoke allowlist was removed, TKT-64.
- [pgrep watcher](learnings/2026-07-21-pgrep-watchers-self-match.md): the self-matching watcher no longer exists, TKT-108.
- [Gitignore exclusion](learnings/2026-07-30-a-gitignore-rule-can-outlive-its-reason.md): `reviews/` was removed in `e3744ccd`.
- [ADR-017 additive example](learnings/legacy-index-notes.md): the old section 2 example was superseded by the schema-4/5 fork, TKT-180/183. Read the current ADR.
