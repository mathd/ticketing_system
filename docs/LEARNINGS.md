# Learnings

One file per topic in [`learnings/`](./learnings/); this page is the **index**, one line each.
Capture as `learnings/YYYY-MM-DD-title.md`. When a lesson stabilizes, **promote it** into
`AGENTS.md` or an ADR so it changes behaviour — then shorten its line here to a pointer.

Re-validated against the tree on 2026-08-03. Entries whose repo-specific instance no longer
exists are in *Historical* at the bottom; their files are kept.

## Testing — what a test can and cannot prove

- [**A fixture too small cannot show the negative**](./learnings/2026-08-03-a-fixture-too-small-cannot-show-the-negative.md) —
  a 3-seat row where *every* pair strands the third; an EXPLAIN proof with 2 distinct values where
  a seq scan is correct. Code right, fixture blind. Ask what the fixture can distinguish *before*
  debugging. (TKT-172/182/183)
- [**A passing test is not evidence it tests anything**](./learnings/2026-07-15-prove-tests-fail.md) —
  observe red first, or mutate the line and watch it fail. (TKT-64)
- [**Check *why* a test is red, not just that it is**](./learnings/2026-07-30-check-why-a-test-is-red-not-just-that-it-is.md) —
  a red test passing for a compile error is a green test wearing a costume. (TKT-67)
- [**When a new check fails 27 tests, ask which side is wrong**](./learnings/2026-07-30-ask-whether-the-fixtures-or-production-are-wrong.md) —
  mass fixture failure is evidence about production as often as about fixtures. (TKT-67)
- [**A coarse observable can be produced by the broken version too**](./learnings/2026-07-25-coarse-observables-pass-the-broken-build.md) —
  assert the thing that differs, not the thing that follows. (TKT-114)
- [**Force the interleaving — repetition cannot falsify a race fix**](./learnings/2026-07-29-force-the-interleaving-repetition-cannot-falsify-a-race-fix.md) —
  loop counts prove nothing; drive the schedule. (TKT-104)
- [**Seed the failure state; don't race a kill**](./learnings/2026-07-17-seed-the-failure-state-dont-race-a-kill.md) —
  fabricate the dirty state directly; red and green become observable in seconds. (TKT-70)
- [**Time-window fixtures must be relative to now**](./learnings/2026-07-18-time-window-fixtures-must-be-relative.md) —
  a calendar literal is green at merge and fails once the clock crosses it. (TKT-85)
- [**A lock-queue handshake must pin the exact statement under test**](./learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md) —
  and *transitively*: PostgreSQL queues row-lock waiters, so the second waiter names the **first
  waiter**, not the lock holder. A direct-blocker barrier waits for ever. (TKT-63, TKT-182)
- [**A test validator downstream of the production wrap sees laundered statuses**](./learnings/2026-07-21-test-validator-downstream-of-production-wrap-sees-laundered-statuses.md) —
  detect the mask body itself; the status was rewritten before the helper saw it. (TKT-108)

## Review and process

- [**Two correct fixes can compose into a new defect**](./learnings/2026-08-03-two-correct-fixes-can-compose-into-a-new-defect.md) —
  four tickets had a pass find a defect *created by the previous pass's fix*. A bounded replacement
  is a new implementation: ask what the old one would have caught. Why the churn cap resets.
  (TKT-174/180/181/182)
- [**Say what a check establishes, not what it is named after**](./learnings/2026-08-03-say-what-a-check-establishes.md) —
  "fail-closed" for *requested* rows only; a required field guarantees shape, not value; an index
  proof satisfiable by a full scan of a partial index. (TKT-172/179/182)
- [**Batch item schemas block per-item results**](./learnings/2026-07-17-batch-item-schemas-block-per-item-results.md) —
  a bulk operation that returns one status cannot report a per-item outcome later without a
  breaking change. (TKT-77)
- [**A finding's mechanics being true does not make its trigger reachable**](./learnings/2026-07-16-refute-the-trigger-not-the-mechanics.md) —
  verify the chain, then grep for what starts it. (TKT-79)
- [**Record a relaxation as a relaxation**](./learnings/2026-07-30-record-a-relaxation-as-a-relaxation.md) —
  a weakened check that reads as a fix is invisible at the next review. (TKT-67)
- [**A load test's number is a claim about the server only if the generator's health is in the verdict**](./learnings/2026-07-17-load-test-generator-health-is-part-of-the-claim.md) —
  the first ceiling measured the client's in-flight cap. (TKT-82)

## Distributed behaviour and events

- [**Early schema acceptance needs semantic completeness, not idempotency**](./learnings/2026-08-03-schema-rollout-ordering-and-early-acceptance.md) —
  a consumer that records completion for work it did not do creates a permanent silent gap. Also:
  a shared merge does not order a deployment; parking is not self-healing; a *setting* shipped
  before its *transport* strands every record made in the gap. (TKT-180)
- [**Retry-vs-terminate is asymmetric, and a comment is not evidence**](./learnings/2026-08-03-retry-vs-terminate-and-http-boundary-dispositions.md) —
  permanence must be a narrow enumerated list; decode from a buffer; assert the disposition per
  branch. (TKT-181)
- [**You cannot classify an event you caused**](./learnings/2026-07-30-you-cannot-classify-an-event-you-caused.md) —
  a detector that also acts loses the ability to tell the two apart. (TKT-67)
- [**Reconciliation acts only on positive assertions — absence is never death**](./learnings/2026-07-17-reconciliation-acts-only-on-positive-assertions.md) —
  a 404 cannot distinguish archived from a different-kind pool. (TKT-90)
- [**An anti-replay rule must be re-checked against the retry it exists to serve**](./learnings/2026-07-16-protocol-rules-safety-and-liveness.md) —
  safety rules that kill liveness. (TKT-63)
- [**Judge idempotent replays by lifecycle state, never by timestamp**](./learnings/2026-07-16-judge-replays-by-lifecycle-state.md) (TKT-65)
- [**Per-entity version counters do not survive convergence onto a shared resource**](./learnings/2026-07-16-version-scope-vs-convergence-scope.md) (TKT-75)

## Postgres and storage

- [**Upsert guards are snapshot-stale under lock contention**](./learnings/2026-07-16-upsert-guards-are-snapshot-stale.md) —
  `ON CONFLICT DO UPDATE ... WHERE NOT EXISTS(...)` evaluates against the pre-wait snapshot. (TKT-63)
- [**A time cutoff decided behind a lock queue must use decision time, not transaction time**](./learnings/2026-07-16-lock-queue-time-cutoffs.md) (TKT-63)
- [**`FOR UPDATE` does not lock a row that does not exist yet**](./learnings/2026-07-20-for-update-does-not-lock-a-row-that-does-not-exist-yet.md) —
  and an insert never conflicts with a row lock on a *different* row, which is why ADR-029's
  edit-vs-sale race needs a family-scoped advisory lock. (TKT-102, TKT-104)
- [**Driver, migration and Compose gotchas**](./learnings/2026-08-03-driver-migration-and-compose-gotchas.md) —
  `text[]` writes fine and fails to *read* through `database/sql` + `pgx/v5/stdlib` (use `jsonb`);
  `provider.Down` rolls back one migration, so a Down-guard test stops testing its own migration
  once the next lands (use `DownTo`); `--no-deps` skips the ADR-022 migration job. (TKT-173/179)
- **Dropping a table-wide UNIQUE drops its backing index** — audit every query that used the index
  prefix before an [ADR-025](./adr/ADR-025-admission-events-and-offline-reconciliation.md) §D7-style
  swap; a partial replacement cannot serve rows outside its predicate. (TKT-84)
- [**A column narrower than the value the code accepts turns storage into a validator**](./learnings/2026-07-17-column-range-narrower-than-code-accepts.md) (TKT-75)

## HTTP, contracts and the web layer

- [**API smoke can't see the SSR layer — a browser-submit test is the only checkOrigin catch**](./learnings/2026-07-20-browser-submit-is-the-only-checkorigin-catch.md) —
  promoted into `AGENTS.md`; a web-UI ticket is not verified until a browser has *submitted* its
  forms. (TKT-105)
- [**A component can be correct and the page still wrong**](./learnings/2026-08-03-storefront-island-styling-and-fetch-gotchas.md) —
  global-stylesheet specificity beat a correct component. Scope an island's rules under a container
  class. Plus: `fetch()` resolves on headers, not the body; `DOMException` is an `Error`. (TKT-174)
- [**`format: uuid` is unenforced, and catalog validates its own rejections**](./learnings/2026-07-29-format-uuid-is-unenforced-and-catalog-validates-its-own-rejections.md) (TKT-104)
- [**The fail-closed validator emits `Cache-Control: no-store` too**](./learnings/2026-07-29-fail-closed-validator-emits-the-same-header-as-the-success-case.md) —
  the header cannot distinguish a served page from a refused one. (TKT-104)
- [**A decoded page is not an answered one**](./learnings/2026-07-30-a-decoded-page-is-not-an-answered-one.md) (TKT-67)
- [**Name what a control reaches, not what it is for**](./learnings/2026-07-15-name-what-a-control-reaches.md) (TKT-64)

## Security

- [**A fingerprint of a symmetric secret is an oracle**](./learnings/2026-07-30-a-fingerprint-of-a-symmetric-secret-is-an-oracle.md) —
  and see [ADR-021](./adr/ADR-021-ticket-lifecycle-trail-integrity.md): name the adversary before
  writing "tamper-evident". (TKT-67)

## Historical — the repo instance is gone, the lesson is kept

- [**A `-run` allowlist silently strands tests**](./learnings/2026-07-15-run-allowlists-strand-tests.md) —
  fixed and now enforced by comments in `scripts/smoke.sh`; no allowlist remains. (TKT-64)
- [**A `pgrep`-for-the-command-name watcher self-matches**](./learnings/2026-07-21-pgrep-watchers-self-match.md) —
  the watcher it described no longer exists in the tree. (TKT-108)
- [**A gitignore rule can outlive its reason — and predate its victim**](./learnings/2026-07-30-a-gitignore-rule-can-outlive-its-reason.md) (TKT-67)
- **ADR-017 §2 additive-no-bump** — superseded as a worked example by the schema-4/5 fork
  (TKT-180/183); [ADR-017](./adr/ADR-017-domain-event-schema-evolution.md) itself is the reference.
- [**Catalog's `/internal/*` routes are hand-mounted**](./learnings/2026-07-21-catalog-internal-routes-are-hand-mounted-outside-openapi.md),
  outside the OpenAPI contract — still true (`services/catalog/internal/api/server.go`), and now a
  convention rather than a surprise.
