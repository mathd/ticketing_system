# Code review

**Date:** 2026-08-19  
**Branch:** `review` at `cdfb7e89`  
**Scope:** Documentation, ADRs, Go services, gateway, TypeScript frontends, SQL migrations, tests, build scripts, Compose, and GitHub Actions.

## Summary

The codebase has a strong correctness culture. Money stays in integer minor units, the public edge is explicit, unsafe JSON writes are bounded, domain-event compatibility is treated as a semantic contract, and the test suite exercises real PostgreSQL, JetStream, contention, recovery, and load behavior. The local gate and the permanent browser suite both pass.

The main defect is in readiness aggregation. The gateway exposes `/readyz`, but it probes each service's liveness endpoint. A service can correctly report itself unready while the public readiness endpoint still returns 200. The remaining findings concern unbounded upstream waits and response bodies, CI path filters, incomplete browser coverage, and documentation governance.

**Finding count:** 1 critical, 8 important, 3 suggestions.

## Validation performed

- `make check`: passed. This covered generated-code drift, dependency drift, Go and TypeScript linting, unit tests, builds, the real-stack smoke suite, and the on-sale load proof.
- `make browser`: passed all five committed browser specs: channels, organizer assertion, rate limiting, password reset, and slot allocations.
- Working tree: clean before this report was created.
- Static audits: local Markdown links, duplicate ADR numbers, server-side fetches, unbounded body reads, workflow path filters, write-form coverage, and stale current-state documentation.
- Network-fed advisory databases were not queried locally. The configured security workflow was reviewed as code.

## Critical

- [x] **R1: Make gateway readiness probe downstream readiness.** The gateway registers the same handler for [`/healthz/all` and `/readyz`](../../gateway/cmd/gateway/main.go#L212), and [`backends()` always appends `/healthz`](../../gateway/cmd/gateway/main.go#L300). This defeats service-specific readiness checks such as Commerce's `access_configured` guard, whose documented purpose is to stop a misconfigured deployment from receiving traffic. It also hides Inventory consumer skew or termination while the process remains live. Keep `/healthz/all` as the liveness fan-out and make `/readyz` fan out to each service's `/readyz`. Add a gateway test with a backend that returns 200 from `/healthz` and 503 from `/readyz`; the two public endpoints must then disagree in the same way.

## Important

- [x] **R2: Put a cache-owned deadline on storefront single-flight loads.** [`PageDataCache`](../../web/storefront/src/lib/cache.ts#L153) stores the in-flight promise until it settles, while [`#fetchThrough`](../../web/storefront/src/lib/cache.ts#L164) calls `fetch` without a signal. A stalled catalog response pins every coalesced request for that URL for the transport's full lifetime. Use a deadline owned by the cache, not by the first waiting browser request, and preserve the existing `finally` cleanup. Add a fake fetch that waits for abort and prove that the in-flight entry is removed and the next request retries.

- [x] **R3: Give all server-side web upstream calls an explicit deadline.** Back-office reads and writes in [`upstream.ts`](../../web/backoffice/src/lib/upstream.ts#L20), [`catalog.ts`](../../web/backoffice/src/lib/catalog.ts#L88), [`inventory.ts`](../../web/backoffice/src/lib/inventory.ts#L90), and [`commerce.ts`](../../web/backoffice/src/lib/commerce.ts#L104), plus the storefront account helper in [`customer-api.ts`](../../web/storefront/src/lib/customer-api.ts#L39), rely on transport defaults. A slow sibling can retain SSR workers after the browser has gone away. Add a shared bounded-fetch policy and operation-level tests. Refund timeouts must continue to be classified as ambiguous and must retain the same idempotency key.

- [x] **R4: Run the repository secret scan on every pull request.** The [`security` workflow's top-level path filter](../../.github/workflows/security.yaml#L10) skips code-only pull requests, so the Trivy `secret` scanner in [`repository-scan`](../../.github/workflows/security.yaml#L81) does not inspect the changes most likely to introduce a credential. The security guide nevertheless says the workflow runs on every pull request. Move the filesystem secret and misconfiguration scan to an all-PR workflow or remove the top-level filter. Dependency advisory jobs may remain path-filtered. Add a workflow test or documented dry run that shows a source-only change schedules the repository scan.

- [x] **R5: Cover every production web image input in the hermetic workflow filter.** The [`hermetic-smoke` paths](../../.github/workflows/hermetic.yaml#L10) include the scanner package and Dockerfile and the storefront Dockerfile, but omit `web/backoffice/Dockerfile`, `web/backoffice/package.json`, and `web/storefront/package.json`. `make check` uses smoke Dockerfiles, so a broken back-office production image can merge without either build path running. Replace the enumerated entries with narrow `web/*/Dockerfile` and `web/*/package.json` globs, then verify that one representative change for each frontend schedules the workflow.

- [x] **R6: Backfill real-browser submission coverage for the remaining back-office write pages.** The permanent browser suite covers channels and slot allocations, but not [`events/new.astro`](../../web/backoffice/src/pages/events/new.astro), [`venues/[id].astro`](<../../web/backoffice/src/pages/venues/[id].astro>), or [`orders.astro`](../../web/backoffice/src/pages/orders.astro). The smoke suite submits some of these forms with a Go client, which does not reproduce browser Origin, Referer, cookie, redirect, or JavaScript behavior. Add Playwright specs that sign in, submit each write path through the rendered form, and assert the stored result or exact refusal.

- [x] **R7: Reconcile current-state documentation with the running system.** [`architecture.md`](../architecture.md#L3) still describes the pre-M2 system, omits the back office, calls scanning unauthenticated, and says read caches are future work. [`ROADMAP.md`](../ROADMAP.md#L9) still lists delivered M2 capabilities as next work. [`security.md`](../conventions/security.md#L48) says staff authentication and rate limiting do not exist, [`configuration.md`](../configuration.md#L36) omits `BACKOFFICE_URL`, and the root [`README`](../../README.md#L20) omits the back-office URL and directory. Update these current-state entry points in one pass. Keep future production controls separate from controls already enforced in this testbed. Also correct the `AGENTS.md` and `Makefile` claim that smoke only renders forms, while preserving the rule that only a real browser proves browser submission behavior.

- [x] **R8: Give every ADR a unique number.** Both [`ADR-055-on-sale-write-rate-limiting.md`](../adr/ADR-055-on-sale-write-rate-limiting.md) and `ADR-055-presale-unlock-codes.md` are accepted, and [`ADR-056`](../adr/ADR-056-partner-credential-identity.md#L13) records the collision instead of resolving it. Bare `ADR-055` references in code, migrations, OpenAPI, tests, and `AGENTS.md` are now ambiguous. Renumber the presale ADR to the next unused number, update every presale reference at its source, regenerate API types, and add a uniqueness check for `docs/adr/ADR-NNN-*.md`.

- [x] **R9: Bound Go upstream response bodies before reading them into memory.** The Stripe adapter calls [`io.ReadAll(resp.Body)`](../../services/payments/internal/psp/stripe.go#L159), and Commerce's shared service client does the same in [`Server.call`](../../services/commerce/internal/api/server.go#L326). Timeouts limit duration, not bytes. A malformed provider or sibling response can allocate without a ceiling on a checkout or recovery path. Read through `io.LimitReader(max+1)`, reject oversize bodies before decoding, and test the exact boundary plus one byte. Preserve unknown or ambiguous payment semantics when the provider response is too large to classify safely.

## Suggestions

- [x] **R10: Correct the broken ADR-010 link.** [`ADR-062`](../adr/ADR-062-refund-reversal-reconciliation.md#L13) links to `ADR-010-inventory-claim-transaction.md`, but the file is named `ADR-010-postgres-claim-transaction.md`. Update the target so the inherited locking decision is reachable.

- [x] **R11: Add an internal Markdown link check to the documentation gate.** The broken ADR-062 reference survives because no gate resolves repository-relative Markdown targets. Add a local, network-free checker for tracked Markdown. It should ignore external URLs and template placeholders, resolve anchors when practical, and fail with the source file and line.

- [x] **R12: Split Catalog's largest files by aggregate without changing the service boundary.** [`internal/store/postgres.go`](../../services/catalog/internal/store/postgres.go) is 2,416 lines and [`internal/api/server.go`](../../services/catalog/internal/api/server.go) is 1,809 lines. They mix venues, seat maps, events, series, festivals, slots, publication, and staff operations. Move related methods into aggregate-focused files inside the same packages. Keep transactions, locks, interfaces, and generated contracts unchanged. This reduces review surface and makes concurrent ticket work less conflict-prone.

## Notable strengths

- The gate is unusually complete for a testbed. It checks generated contracts, builds every component, starts the full stack, exercises contention and recovery, and measures the load path.
- High-risk invariants are tested at the enforcing tier. The repository has strong examples for SQL tenant scoping, seat-map pinning, event-schema dispatch, idempotency, partial refunds, and lifecycle integrity.
- The edge and credential model are explicit. Gateway routes are allow-listed, generic internal paths are denied, services use separate database roles, and payment credentials are separated from the shared internal token.
- Documentation records failure modes and adversarial lessons instead of only happy-path design. The main documentation issue is freshness, not lack of depth.
