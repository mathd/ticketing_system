# Learnings

One file per topic in [`learnings/`](./learnings/); this page is the **index**, one line each.
Capture as `learnings/YYYY-MM-DD-title.md`. When a lesson stabilizes, **promote it** into
`AGENTS.md` or an ADR so it changes behaviour — then shorten its line here to a pointer.

Re-validated against the tree on 2026-08-03. Entries whose repo-specific instance no longer
exists are in *Historical* at the bottom; their files are kept.

## Testing — what a test can and cannot prove

- [**A guard's worst failure is not seeing**](./learnings/2026-08-06-a-guards-worst-failure-is-not-seeing.md) —
  a source-scanning invariant matched neither `UPDATE ONLY orders` nor `UPDATE public.orders`, so a
  real attribution writer would have been invisible while the allowlist count stayed satisfied. A
  guard that permits too much fails loudly; a guard that cannot SEE fails silently forever. For any
  recogniser, write the test that proves it recognises before the one that proves it judges. (TKT-222,
  TKT-223)
- [**A fake store cannot see the driver**](./learnings/2026-08-06-a-fake-store-cannot-see-the-driver.md) —
  three defects in one query, all compile-clean and all fatal against real Postgres: an uninferable
  array parameter, a jsonb column with no Scanner, and a NULL scanned into a value type. A fake
  returns Go values; a driver returns driver.Value, so the whole class lives below the seam. A new
  query gets a real-Postgres test that SCANS A ROW before it gets a fake-store test — and each test
  written to catch one of these was blind to the next until the fixture grew. (TKT-222)
- [**A fix can be correct and still lie about itself**](./learnings/2026-08-06-a-fix-can-be-correct-and-still-lie-about-itself.md) —
  two [high] findings on a money path were both about a code comment and a line of UI copy, not
  behaviour: "nothing was submitted, so a retry is safe" (a disconnect can land after the charge) and
  "your seats are still held" (a 401 can arrive on a completed purchase). On a money path the claim
  IS part of the product. (TKT-221)
- [**Three ways one test could not fail, and "observe it red" caught none of them**](./learnings/2026-08-06-three-ways-one-test-could-not-fail.md) —
  the assertion was satisfied by a side effect of the code under test; then it asserted only the
  positive half; then it never ran the production default. Red-then-green proves a test can tell
  "before" from "after", not "correct" from "a different wrong thing". (TKT-220)
- [**A cap inherited across a trust boundary is not a bound**](./learnings/2026-08-06-a-cap-inherited-across-a-trust-boundary-is-not-a-bound.md) —
  a per-principal session cap bounds the map only if principals are bounded. Copied from a staff tool
  (operator-provisioned) to a storefront (public registration), every fact stayed true and the premise
  was deleted in transit. (TKT-220)
- **A surviving mutant may be exposing a blind fixture, not equivalent code** — a forced-partition
  mutant survived because the only forced case in the table gave the same winner whether the
  partition was a filter or a tie-break. The usual reading of a surviving mutant is "the change is
  equivalent"; sometimes it means "this fixture cannot observe the property". (TKT-216)
- **Check how the codebase already solved it before writing a new integrity guard** — payments has
  guarded TRUNCATE on its append-only journal since the journal existed; a new trigger written from
  first principles in the same repo shipped the identical hole. Row-level triggers do not fire on
  TRUNCATE. (TKT-216)
- **A test can defend the bug it was written to catch** — TKT-215's overflow test asserted that a
  large price at a 100% rate *must be refused*, pinning broken arithmetic as required behaviour.
  Mutation testing does not help: the mutant dies correctly against a wrong oracle. Ask what the
  test WANTS, not only whether it can fail. (TKT-215)
- **A mutation check that does not compile is not a check** — it reports "no failure" and looks
  exactly like a passing one. Assert the mutant builds and the suite actually ran before reading the
  result. (TKT-215)
- **`git reset --hard origin/<branch>` destroys unpushed local commits** — a direct commit that was
  never pushed vanished while a closeout comment claimed it had been promoted. Push immediately and
  verify `git log origin/<branch> -1` before claiming it anywhere. (TKT-215)
- **A test that copies the code under test cannot fail** — a rollback-guard test that ran a `DO`
  block *copied out of* the migration would have passed with the migration's entire `Down` guard
  deleted; expectations naming production *constants* likewise survive aliasing one to another's
  value. Run the real migration; pin enum values as literals. (TKT-214)
- [**A fixture too small cannot show the negative**](./learnings/2026-08-03-a-fixture-too-small-cannot-show-the-negative.md) —
  a 3-seat row where *every* pair strands the third; an EXPLAIN proof with 2 distinct values where
  a seq scan is correct. Code right, fixture blind. Ask what the fixture can distinguish *before*
  debugging. (TKT-172/182/183)
- [**A passing test is not evidence it tests anything**](./learnings/2026-07-15-prove-tests-fail.md) —
  observe red first, or mutate the line and watch it fail. (TKT-64)
- [**A hand-maintained inventory cannot detect its own drift**](./learnings/2026-08-05-a-hand-maintained-inventory-cannot-detect-its-own-drift.md) —
  an "only X may do Y" test counted a hand-written list against a hand-written 7; a ninth route left
  it green while the credential opened it. Read the candidate set from the system. (TKT-194)
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
  promoted into `AGENTS.md`, and since TKT-228 into `make browser` / `test/browser/`; a web-UI
  ticket is not verified until a browser has *submitted* its forms. (TKT-105, TKT-220, TKT-226)
- [**A component can be correct and the page still wrong**](./learnings/2026-08-03-storefront-island-styling-and-fetch-gotchas.md) —
  global-stylesheet specificity beat a correct component. Scope an island's rules under a container
  class. Plus: `fetch()` resolves on headers, not the body; `DOMException` is an `Error`. (TKT-174)
- [**`format: uuid` is unenforced, and catalog validates its own rejections**](./learnings/2026-07-29-format-uuid-is-unenforced-and-catalog-validates-its-own-rejections.md) (TKT-104)
- [**The fail-closed validator emits `Cache-Control: no-store` too**](./learnings/2026-07-29-fail-closed-validator-emits-the-same-header-as-the-success-case.md) —
  the header cannot distinguish a served page from a refused one. (TKT-104)
- [**A decoded page is not an answered one**](./learnings/2026-07-30-a-decoded-page-is-not-an-answered-one.md) (TKT-67)
- [**Name what a control reaches, not what it is for**](./learnings/2026-07-15-name-what-a-control-reaches.md) (TKT-64)
- [**TypeScript 7 blocks on tooling, not on our code**](./learnings/2026-08-03-typescript-7-has-no-js-compiler-api.md) —
  the Go port exports no JS compiler API, so `astro check`/Volar and `openapi-typescript` die on
  import. Source needed no changes; `.astro` frontmatter is now unchecked. pnpm `overrides` cannot
  pin a peer.
- [**A failed optional dependency is skipped silently**](./learnings/2026-08-07-a-failed-optional-dependency-is-skipped-silently.md) —
  TypeScript 7 ships its compiler as one optionalDependency per platform, and pnpm does not fail an
  install when one fails to fetch. So a transient hiccup yields a green `pnpm install` and a
  confusing "your platform is unsupported" from `tsc` minutes later — which sent TKT-227 after musl
  and the lockfile, neither of which was involved. Does not reproduce; landed as a diagnosis.

## Security

- [**A fingerprint of a symmetric secret is an oracle**](./learnings/2026-07-30-a-fingerprint-of-a-symmetric-secret-is-an-oracle.md) —
  and see [ADR-021](./adr/ADR-021-ticket-lifecycle-trail-integrity.md): name the adversary before
  writing "tamper-evident". (TKT-67)
- [**"At startup" is a claim about a runtime**](./learnings/2026-08-05-at-startup-is-a-claim-about-a-runtime.md) —
  a credential check at module scope in Astro middleware runs on the first REQUEST, not at boot: the
  framework lazily imports its own hooks. Name the runtime event, and test the process. (TKT-194)
- [**A guard inside a generated handler is not first**](./learnings/2026-08-05-a-guard-inside-a-generated-handler-is-not-first.md) —
  oapi-codegen binds and validates parameters *before* HandlerMiddlewares, so an in-handler
  credential check answers 400-with-detail to a caller holding none. Guard outside the generated
  handler; the guard and the router must read the same path. (TKT-214)

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
- [A refusal after the point of no return is not a safety check](learnings/2026-08-05-a-refusal-after-the-point-of-no-return-loses-money.md)
  — TKT-217. Validating a settlement plan after the PSP call loses money; after `BindOperation`
  leaves a recoverable operation with no ledger; before the idempotency lookup breaks replay. Ask
  what is already irreversible where the check runs. A durable row another process will act on is
  past the line and looks like bookkeeping.
- [A mutation check against a stale schema tests last revision's constraints](learnings/2026-08-05-a-schema-too-old-cannot-fail-a-mutation-check.md)
  — TKT-217. A mutant survived because the test database was migrated from an earlier revision of
  the same unreleased migration; goose does not re-run a changed file. Recreate the database before
  mutation-checking anything schema-adjacent. And a gate result only describes the tree it ran on.
- [`Referrer-Policy: no-referrer` nulls the `Origin` header](learnings/2026-08-07-no-referrer-nulls-the-origin-header.md)
  — TKT-226. Chrome sends `Origin: null` on a form POST from a `no-referrer` page, so the
  storefront's origin check 403'd **every password reset before the handler ran** — past four green
  gates, two adversarial passes and 166 unit tests. `Referrer-Policy: origin` keeps `Origin` intact
  and still strips path and query. A header that changes what the browser *sends next* cannot be
  judged from either file alone. Third instance of render-is-not-submit (TKT-105, TKT-220); the
  evidence for **TKT-196**.
- [A total order is not a meaningful one](learnings/2026-08-09-a-total-order-is-not-a-meaningful-one.md)
  — TKT-230. `ORDER BY occurred_at, id` on an append-only trail reads as correct *because* it is
  total, but `id` was a random UUIDv4, so ties were broken by a coin flip — invisible serially,
  and one collision per 1200 rows under 8 concurrent writers, which is enough to make `make check`
  non-deterministic. Three more from the same ticket: **`now()` is transaction-start time**, so
  "we hold a row lock, therefore our timestamps are ordered" is false unless the stamp is
  `clock_timestamp()` (this one survived plan-review and two adversarial passes before a
  two-terminal experiment overturned it); a column **DEFAULT does not apply to an explicit NULL**,
  so only an unconditionally-overwriting trigger makes "the sequence is the sole source" true; and
  **`ADD CONSTRAINT … NOT VALID`** enforces a predicate for new rows without the full-table scan
  that would blow ADR-008's 30s migration bound. When a plan claims something about database or
  runtime semantics, run it — a database was available the whole time.
- [An assertion over a `min()` only tests the winning arm](learnings/2026-08-09-an-assertion-over-a-min-only-tests-the-winning-arm.md)
  — TKT-233. A test asserted channel availability was 3 after a hold expired and called that proof
  the expired hold had left the live-claims accounting. `Available` is
  `min(pool remaining, cap − consumed)` over **two independent predicates**, so a regression in the
  pool-level one left `min(7,3)` still equal to 3 and the test passed straight through it — verified
  by mutating that arm alone. **A single assertion over a `min()`/`max()`/clamp of N predicates can
  only discriminate the winning arm**; proving all N means asserting the components, not the
  aggregate. Hard to spot because the expected value is arithmetically *correct* — nothing about a
  green test says the number would be 3 anyway if the code were broken. Sibling of
  [a fixture too small](learnings/2026-08-03-a-fixture-too-small-cannot-show-the-negative.md): there
  the fixture admitted no failing input, here the assertion cannot express the failure.
- [pgx returns `infinity` timestamps as a string](learnings/2026-08-09-pgx-returns-infinity-timestamps-as-a-string.md)
  — TKT-233. `'infinity'::timestamptz` is the cleanest way to pin a row live in a fixture (it
  satisfies `liveClaims` and the non-NULL buyer-expiry CHECK, so nothing can expire out from under
  the next statement), but Go's `time.Time` cannot represent it and pgx hands it back as a string:
  scanning it fails with `unsupported Scan, storing driver.Value type string`. Fine to write, fine
  for SQL to compare — never scan it into Go. Keep such a pin short-lived, restore a finite value
  before anything reads the row, and comment the pin, because the error surfaces far from its cause.
