# Code slop audit

**Date:** 2026-09-03
**Scope:** Go services and commands, TypeScript and Astro frontends, tests, browser harnesses, generated contracts, build scripts, tracked support files, and production comments.
**Method:** Parallel Go, test, and frontend audits; targeted source searches; call-site checks; test-mechanism checks; module-isolation checks; and review of the latest local gate log.

## Summary

The repository is not broadly low quality. Its domain boundaries, database-backed tests, generated-contract gate, smoke suite, and adversarial learnings are stronger than most codebases of this size. The slop is concentrated in seams that agents tend to grow around otherwise sound code: tests that restate an implementation instead of exercising it, optional constructors that make production dependencies look optional, hand-written JSON shapes beside unused generated types, historical review narratives embedded in source, and one-off wrappers or harnesses that survive after their original purpose has moved.

Three findings are correctness defects, not cosmetic cleanup. Four Go services validate an HTTP request before the shared body limit is applied. The scanner can permanently strand a durable occurrence in `PENDING`. The scanner also performs storage initialization and occurrence minting before its error boundary, which can leave submission permanently disabled after an IndexedDB failure.

The code-to-test balance also deserves attention. Excluding generated files, the audit counted about 43,000 lines of production and support code and 82,700 lines of tests. A large test suite is appropriate for this domain, but several examples below add volume without independently discriminating a production defect. There are also about 47,500 comment lines across production and tests, and 927 source or test references to review passes, mutations, AI review, or similar development archaeology. Those numbers are signals, not findings by themselves; the findings below identify concrete cases.

**Finding count:** 3 critical, 16 important, 18 suggestions.

## Validation and limits

- The current commit had an existing passing `make check` verdict from 2026-09-03 at 22:28. This audit did not rerun the gate and did not treat that earlier result as proof for mechanisms outside its coverage.
- All configured frontend `oxlint --deny-warnings` checks pass.
- `go vet` passes in all eight Go modules in the workspace.
- A workspace-disabled, network-disabled `go list -mod=readonly ./...` check passes for Gateway and Smoke, but fails for Shared, Catalog, Inventory, Commerce, Payments, and Access because their standalone `go.sum` files are incomplete. The Go workspace currently masks this drift.
- The installed `staticcheck` cannot read this repository's Go 1.27 export data, so this audit does not claim a current staticcheck result.
- No production code or tests were changed during the audit.

## Critical

- [x] **R1: Apply the request-body ceiling before OpenAPI validation.** The shared middleware installs kin-openapi request validation in [`shared/go/contract/http.go`](../../shared/go/contract/http.go#L191), while the body limit in [`shared/go/httpx/json.go`](../../shared/go/httpx/json.go#L13) is reached only when a handler decodes the already validated request. Payments, Inventory, Access, and Commerce therefore allow the validator to read an unbounded body before the handler's limit runs. Catalog already wraps the request with `http.MaxBytesReader` outside validation in [`services/catalog/internal/api/server.go`](../../services/catalog/internal/api/server.go#L264). Put a configurable ceiling around the body in the shared validation middleware and add a test whose reader proves the validator cannot consume beyond the limit. Keep operation-specific smaller limits in handlers where they are useful.

- [x] **R2: Recover scanner occurrences left in `PENDING`.** [`queued()`](../../web/scanner/src/occurrences.ts#L112) returns only `QUEUED` records. The scanner persists `PENDING` before the network call in [`App.tsx`](../../web/scanner/src/App.tsx#L128), and its own failure-path comment acknowledges that a failed `markQueued` can leave that record invisible forever. A crash between mint and transition has the same result. On startup, recover pre-existing `PENDING` records to `QUEUED` before accepting a new scan. Do not indiscriminately recover the current in-flight occurrence. Add a reload test that seeds `PENDING`, recreates the application, and observes one retry with the same occurrence identity.

- [x] **R3: Put scanner storage acquisition and minting inside the handled operation.** Submission sets `submitting` and then awaits the module-global store promise and occurrence minting before entering the `try` block in [`App.tsx`](../../web/scanner/src/App.tsx#L126). If IndexedDB initialization or minting rejects, the error is unhandled, the `finally` block never runs, and the scan button remains disabled. Mount refresh and online synchronization contain similar unguarded reads. Move acquisition, minting, and queue reads inside a common handled boundary, expose a retryable storage-unavailable state, and avoid caching a permanently rejected store promise. Test both database-open and mint failures.

## Important

- [x] **R4: Either use the generated Access contracts or stop generating them.** The root generator and drift gate maintain 507-line Access type files for both Scanner and Back Office, but neither application imports them. Scanner instead asserts hand-written optional response shapes in [`App.tsx`](../../web/scanner/src/App.tsx#L161), even though the generated contract makes decision fields required and constrains reconciliation results. Back Office independently reconstructs lifecycle and ticket shapes in [`access.ts`](../../web/backoffice/src/lib/access.ts#L16). Use generated types behind runtime decoders that validate required discriminants, identities, dates, and enums. If Back Office deliberately does not consume the schema, remove that generated target and its drift entry instead of carrying 507 dead generated lines.

- [x] **R5: Make allocation replacement inputs structurally required.** [`buildAllocationReplacement`](../../web/backoffice/src/lib/allocation-form.ts#L473) defaults the trusted current set to `[]` and makes the optimistic revision optional, although the preceding contract says the current set is the only trusted source and the revision prevents lost updates. Missing current data can therefore construct a full-set deletion, and missing revision becomes an unconditional write. Require both arguments in the type. If an internal unconditional operation is genuinely needed, give it a separate, explicit name and call surface. Reverse the test that currently blesses revision omission.

- [x] **R6: Refuse malformed channel-list responses instead of presenting an empty system.** [`listChannels`](../../web/backoffice/src/lib/catalog.ts#L499) accepts an optional `channels` property and returns `[]` when it is missing. The UI then renders "No channels yet," indistinguishable from a valid empty catalog. Validate that `channels` is an array with the minimum required row shape and surface upstream unavailability on malformed data. Replace the test that expects a missing property to become an empty list.

- [x] **R7: Validate Commerce worker configuration once at startup.** [`services/commerce/cmd/commerce/main.go`](../../services/commerce/cmd/commerce/main.go#L531) contains parallel duration and batch-size parsers that silently substitute defaults for invalid or non-positive values. A mistyped recovery setting therefore starts successfully with different behavior from the operator's configuration, and batch values are read more than once while sizing leases and runners. Parse one typed `WorkerConfig`, return explicit errors, and pass the validated values to every dependent component. Add table tests for malformed, zero, negative, and valid values.

- [x] **R8: Split Catalog's 72-method store interface at consumer boundaries.** [`store.Store`](../../services/catalog/internal/store/store.go#L538) is held wholesale by the API server. Its fake has grown to roughly 3,000 lines and implements 71 methods, including no-op methods present only for interface satisfaction. Define narrow interfaces around cohesive handler groups or inject small domain services. This should be a mechanical dependency-boundary change, not a service split. Delete fake methods once no consumer requires them.

- [x] **R9: Replace partially initialized and optional production constructors.** Commerce's API constructor builds a refund service before Access configuration exists, then [`WithAccess`](../../services/commerce/internal/api/server.go#L160) mutates the server and rebuilds it. Access uses variadic token or option arguments even though production always supplies them. Payments exposes `New`, `NewWithPSP`, and `NewWithPSPRetention`; the shortest constructor selects a fake PSP and is used only by tests. Introduce typed dependency/config structs, require production credentials and providers at construction, and keep convenient defaults in `_test.go` helpers. This removes invalid intermediate states and prevents a fake provider from becoming an accidental production default.

- [x] **R10: Make the Go build-list self-test execute the production checker.** Case 8g in [`scripts/gate-selftest.sh`](../../scripts/gate-selftest.sh#L199) reproduces the workspace-copy logic from [`check-go-build-list-lag.sh`](../../scripts/check-go-build-list-lag.sh#L68) instead of invoking it. A regression in the production checker can leave the self-test green. Give the production checker a fixture or diagnostic mode, invoke that exact code from the self-test, and assert its result and diagnostic.

- [x] **R11: Test toolchain anchors, not only the version comparator.** [`check-go-toolchain.sh`](../../scripts/check-go-toolchain.sh#L24) discovers versions from `go.mod`, Dockerfiles, and workflows, but the self-test feeds hand-written version lines only. Removing an anchor from discovery or wiring can therefore pass. Build a temporary repository fixture, run the real anchor extraction and comparison, and cover missing, malformed, mismatched, and matching anchors with their source labels.

- [x] **R12: Make browser sign-in assertions exclude the login page.** The shared helper starts on `/admin/login` and waits for `**/admin**` in [`test/browser/lib/support.mjs`](../../test/browser/lib/support.mjs#L26), a pattern already satisfied before submission. Several specs repeat the same check or use `url().includes('/admin')`. Wait for an exact authenticated destination or explicitly reject `/admin/login`, then assert a stable authenticated element. Consolidate the copied sign-in implementations into the shared helper.

- [x] **R13: Remove or repair the stale Python checkout verifier.** The removed `scripts/verify-checkout-browser.py` imported Python Playwright without a declared Python environment and navigated directly to Scanner without completing the device-pairing flow required by [`App.tsx`](../../web/scanner/src/App.tsx#L394). The supported [browser harness](../../test/browser/checkout.mjs) now owns the checkout-and-scan scenario, and [`docs/testing.md`](../testing.md#L66) no longer presents the Python script as reproducible.

- [x] **R14: Replace Catalog reemit tests that cannot observe publication wiring.** [`reemit_test.go`](../../services/catalog/cmd/catalog/reemit_test.go#L94) compares two event-ID helper results and does not invoke the publisher wired in [`reemit.go`](../../services/catalog/cmd/catalog/reemit.go#L87). [`reemit_orphan_prevention_test.go`](../../services/catalog/cmd/catalog/reemit_orphan_prevention_test.go#L26) uses a fake that cannot observe the emitted schema. Wrong publisher wiring can leave both suites green, while event identity and the real path are already covered elsewhere. Either inject a publisher and capture the emitted envelope at this command boundary, or delete the duplicate claims and retain the stronger events and smoke tests.

- [x] **R15: Remove service-local durable-consumer facades and test production wiring.** Access and Inventory each wrapped the shared durable-consumer wait helper in a near-identical `run.go` file and repeated its unit suite. Production could bypass or miswire those wrappers while the duplicated tests remained green. Both services now call the shared package directly, the facade tests are gone, and [Access enters each production Run path](../../services/access/internal/consumer/run_test.go#L61) to observe termination. Inventory retains its production-path test.

- [x] **R16: Type-check Astro source, not only extracted TypeScript modules.** The frontend gate runs `astro sync`, `tsc --noEmit`, and `astro build`, but not `astro check`. A current mismatch demonstrates the gap: `AllocationRow.clearRelease` is required in [`allocation-form.ts`](../../web/backoffice/src/lib/allocation-form.ts#L50), while the mapping in [`slots/[id].astro`](<../../web/backoffice/src/pages/slots/[id].astro#L200>) omits it and still builds. Add `astro check` when compatible with the pinned stack. Until then, move nontrivial frontmatter transformations into checked `.ts` modules. Fix the missing field and add a focused mapping test.

- [x] **R17: Restore standalone Go module dependency integrity.** Every workspace module now resolves with `GOWORK=off`, `GOPROXY=off`, and `-mod=readonly`. The [standalone checker](../../scripts/check-go-standalone.sh) enforces that property, while the separate [build-list checker](../../scripts/check-go-build-list-lag.sh) retains its vertical dependency check. Both compare their canonical arguments with `go.work` before resolving anything. The gate self-test runs both production checkers against a missing argument, an extra argument, two spellings of one canonical argument, and a duplicate `go.work` entry.

- [x] **R18: Make the staff-role list exhaustive by construction.** [`STAFF_ROLE_MEMBERS`](../../web/backoffice/src/lib/authorization.ts#L24) is now an exhaustive `Record<StaffRole, true>`, and `STAFF_ROLES` is derived from it. Request-boundary roles remain `unknown` until the own-property guard narrows them, so an absent, non-string, inherited, or unknown role fails closed.

- [x] **R19: Put runtime decoding between JSON and identity-bearing frontend state.** Back Office and Storefront repeatedly cast `response.json()` to hand-written interfaces in catalog, inventory, commerce, and customer API helpers. Generated TypeScript types provide compile-time shape only and do not validate network data. Prioritize small decoders for authenticated principals, staff credentials, IDs, money, arrays, dates, and discriminated results. Keep decoders close to the boundary and return typed domain values; do not build a second generic schema framework.

## Suggestions

- [x] **R20: Delete historical negative-control tests for obsolete timeout values.** The exchange-sweep and reversal suites each contain a test named `TestSizingTheLeaseFromTheWrongTimeoutUnderCoversTheBatch`. They pass an intentionally wrong 10-second timeout, can self-skip when the premise changes, and do not prove the constructed production runner uses a safe relationship. Centralize lease derivation and assert that each constructed runner's claim lease exceeds its actual operation timeout and batch envelope.

- [x] **R21: Delete parse-only and duplicate tests that add no independent signal.** Stripe's parse-only fixture check, Storefront's duplicate money assertions, and Back Office's duplicate authorization case are gone. Scanner now holds a browser-managed lock for each live page owner, so timer throttling cannot make an in-flight row recoverable. Recovery preserves the occurrence ID and requires either a reload predecessor or an expired lease, plus an absent owner lock. Tests cover delayed heartbeat delivery, a live lock, exact recovery after release, and the lease boundary.

- [x] **R22: Remove test-only exports from production packages.** Catalog validation and tracing tests now use package-local seams, and locale tests exercise the production parser. Back Office unresolved-work trackers and Storefront sessions expose factories instead of reset, count, or operational clock controls. Production owns one instance of each store; tests create isolated instances with clocks and limits supplied at construction.

- [x] **R23: Inline confirmed pass-through wrappers.** Commerce's `credentialMatches` only forwards to `httpx.HeaderCredentialMatches`; Access's `ticketAdmitted` only forwards to `ticketAdmittedUnion`; bulk-refund creates `context.WithoutCancel(context.Background())`, which is equivalent to the background context it already has. Inline these calls unless the wrapper owns a named policy or is the intended injection seam.

- [x] **R24: Replace source-text command-dispatch tests with executable dispatch.** Access, Catalog, Commerce, Inventory, and Payments now route commands through the shared pure dispatcher. Each binary invokes every registered command through its `execute` path in tests. The dispatcher returns errors and exit codes to `main`; command callbacks no longer exit the process.

- [x] **R25: Remove confirmed dead frontend declarations and dependencies.** Confirmed examples are `hasExplicitZone`, `PublicEventSummary`, `CustomerOrderSummary`, Scanner's second `index.css` import, and Scanner's unused `@vitest/coverage-v8` dependency. Remove them in one mechanical pass and run the normal type, lint, and test commands. Also rename `hold-picker.test.ts` or `HoldPicker.test.tsx` so the pair does not differ only by case on case-insensitive filesystems.

- [x] **R26: Retire the superseded `.sdlc` implementation.** The 1,335-line server, board, README, and configuration stub has been removed. Current instructions point at the vault-backed board, historical references are labelled as history, and git history is the rollback mechanism.

- [x] **R27: Remove tracked operating-system metadata.** The 8 KB macOS Finder file is deleted and `.gitignore` excludes `.DS_Store` at every depth.

- [x] **R28: Replace development archaeology in source with current invariants.** Comments in the touched command, reversal, scanner, session, and gate self-test paths now state the current rule and threat without reviewer identities or rejected-version chronology. Credential prose consistently describes Commerce's four credentials and six pairwise comparisons. This was a targeted cleanup of reviewed code, not a repository-wide comment deletion.

- [x] **R29: Shorten the refund-reversal smoke cadence.** The latest passing gate spent about 68 seconds in `TestARefundTakenWhileAccessIsDownCompletesItselfAfterwards`. The smoke Compose overlay shortens other recovery intervals but leaves refund reversal at its one-minute default. Set an explicit short smoke-only reversal interval and make the test derive its wait budget from that configuration. Preserve at least one test of the default parser separately.

- [x] **R30: Consolidate policy analyzers before adding more source scanners.** The repository carries bespoke AST analyzers for status contracts, lifecycle not-found behavior, and bad-request behavior. Some currently distinguish almost no operations but still duplicate policy in analyzer code, adapters, allow-lists, and tests. Do not delete useful enforcement. Move operation policy toward one declarative registry or a dedicated contract-lint command, then require each rule to demonstrate a mutation that only that rule catches.

- [x] **R31: Centralize JSON response writing with explicit cache policy.** Five services carry similar `writeJSON` helpers with small differences in headers and cache behavior. Add `httpx.WriteJSON` with an explicit cache-policy argument or small named variants. Migrate one service at a time and preserve existing status, content type, no-store behavior, and encoding-error handling.

- [x] **R32: Consolidate repeated lease-sizing arithmetic.** Recovery, reversal, and exchange-sweep runners contain near-identical `LeaseFor` formulas. Extract the arithmetic into a small shared helper that accepts calls per item, batch size, operation timeout, and safety margin. Keep domain defaults in each worker package so a generic helper does not become a dumping ground.

- [x] **R33: Split catalog cache construction from test instrumentation.** [`public_read_cache.go`](../../services/catalog/internal/api/public_read_cache.go#L135) exposes functional options used only by tests, and `entryCount` duplicates information available from status reporting. Use an explicit cache config for production-owned settings, keep clock or capacity controls in same-package test construction, and make tests assert through the public status shape where practical.

- [x] **R34: Use keyed locale catalogs instead of Python-style string dictionaries.** Storefront translations are `Record<Locale, Record<string, string>>`, so misspelled keys compile and a missing French entry is still typed as `string`. Define the canonical locale object `as const`, derive `MessageKey`, and require every other locale to satisfy `Record<MessageKey, string>`. Keep dynamic lookup only at the external-input boundary after narrowing the key.

- [x] **R35: Split authenticated and unauthenticated catalog header builders.** [`writeHeaders(assertion?: string)`](../../web/backoffice/src/lib/catalog.ts#L67) makes an authorization assertion optional because one authentication operation is exceptional, contradicting the nearby claim that omission should not compile. Give staff-credential creation its own header builder and require `assertion: string` for catalog writes.

- [x] **R36: No shared CSRF-token primitive exists to extract.** Follow-up inspection invalidated the original finding: neither application parses or compares a cookie/header CSRF token. They independently enforce forwarded-origin policy, whose route and proxy assumptions remain application-owned. No abstraction was added for a mechanism the codebase does not have.

- [x] **R37: Make generated-output mapping single-sourced.** The root generation script chains per-application commands while the Makefile separately enumerates expected outputs. Put the generator-to-output mapping in one script or manifest and make both generation and drift checks consume it. This prevents an output from being generated but omitted from drift enforcement, or vice versa.

## Findings that should not be "cleaned up"

- The browser suite itself is the correct tier for form submission. Its duplicated and vacuous authentication helpers need repair, not replacement with Go smoke clients.
- The password-reset form-action source check is brittle, but it guards a browser-sensitive regression and is paired with a real browser test. Keep it unless a stronger executable assertion fully replaces it.
- Lifecycle rollback-gap tests intentionally assert the documented open gap. They are executable claims, not tests for removed behavior.
- Refusing migration down operations is a current operational contract, not dead backward-looking coverage.
- Narrow database-store adapters can be useful when they enforce the tier boundary. The problem is broad consumer interfaces and fakes that mirror them, not every adapter.
- The repository's different root and application TypeScript versions appear deliberate. No version unification is proposed by this audit.
- Generated code that is imported and checked for drift is not slop merely because it is verbose.

## Proposed order of work

1. Fix R1 through R3 first and add discriminating failure tests before refactoring the surrounding code.
2. Close trust and decoding gaps in R4 through R6, R16, R18, and R19. These changes define what malformed or incomplete external data means.
3. Repair the false-confidence tests and harnesses in R10 through R15, R20, R21, and R24. For each replacement, demonstrate the old gap by making only the new test fail.
4. Simplify construction and package seams in R7 through R9, R22, R23, R31 through R33, R35, and R36.
5. Remove confirmed dead artifacts and stale process residue in R25 through R28, then address the lower-risk maintenance improvements.
6. Run targeted tests after every work unit. Run `make browser` for the Scanner and authentication changes. Commit regenerated outputs before the final `make check`, then run the gate with an untouched working tree.

## Notable strengths

- Tests routinely exercise PostgreSQL, JetStream, recovery workers, contention, and full-stack behavior instead of relying only on mocks.
- The gate checks generated output, dependency shape, compilation, smoke behavior, and load behavior. Its lock and verdict design avoids several common false-pass modes.
- Security and lifecycle documentation names concrete adversaries and known gaps instead of using vague guarantee language.
- Money representation, service ownership, event-schema handling, and browser-write verification are explicit repository policies.
- Many tests are already written at the enforcing tier and use mutations or exact persisted-state checks. The recommendations above target the exceptions, not the overall testing strategy.

## Appendix: resolution and current validation (2026-09-04)

The summary and **Validation and limits** sections above are the audit snapshot from 2026-09-03.
They describe the defects and repository state that prompted this work. The checked boxes record
the corresponding changes in the current working tree; they do not revise the historical snapshot.

Current validation of the completed follow-up includes:

- All eight Go modules passed lint, tests, and builds. After API regeneration, Catalog and Commerce
  also passed fresh `go test -count=1 ./...` and `go vet ./...` runs.
- Scanner passed its lint and build checks and 62 tests. Storefront passed lint, explicit TypeScript
  checking, its Astro build, and 334 tests. Back Office passed lint, its Astro build and type check,
  and 485 tests.
- The focused Scanner, Storefront, and Back Office regression suites each passed eight fresh
  repeats. The Compose identity fixture and the production hermetic-workflow and
  generator-registry checks also passed eight fresh repeats after their mutations were shown to
  fail.
- The dependency-drift, build-list-lag, and standalone-module checks passed for the exact eight
  modules in `go.work`. Both fast and hermetic Compose overlay sets parsed successfully.
- Workflow guards, shell syntax, the Make dependency graph, Python syntax, generated-output
  registry coverage, and Markdown links passed. The link check covered 201 tracked Markdown files.
- Controlled SIGINT and SIGTERM probes exercised `gate.sh`, `smoke.sh`, `browser.sh`, and
  `gate-selftest.sh`; each returned 130 or 143 respectively, and the gate removed its lock.

API generation is idempotent and its 12 registry entries cover all 12 tracked generated outputs.
Five generated files changed with their OpenAPI inputs. The final integration commit includes those
outputs before the untouched-tree `make check`, because the HEAD-based generation stage correctly
treats an uncommitted regeneration as drift.
