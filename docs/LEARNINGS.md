# Learnings

One file per topic in [`learnings/`](./learnings/); this page is the **index**, one line each.
Capture as `learnings/YYYY-MM-DD-title.md`. When a lesson stabilizes, **promote it** into
`AGENTS.md` or an ADR so it changes behaviour — then shorten its line here to a pointer.

Re-validated against the tree on 2026-08-03. Entries whose repo-specific instance no longer
exists are in *Historical* at the bottom; their files are kept.

## Testing — what a test can and cannot prove

- [**A proxy can fail while the guarantee holds — and pass while it is gone**](./learnings/2026-08-09-a-proxy-can-fail-while-the-guarantee-holds.md) —
  a smoke test measured Compose's `depends_on` guarantee by comparing container timestamps; it
  inverted by 519ms under load with nothing wrong, and would equally have passed on an idle box with
  the condition deleted. Assert the mechanism, not a symptom of it — then check the encoding you
  read carries every field the mechanism honours: `depends_on.required` is absent from the label,
  and `required: false` lets Compose skip a *failed* migrate job and start the service anyway.
  Also: `docker compose -p <p> config` without `-f` silently drops the overrides the stack was
  created with, and `go test` will serve a **cached** pass for a mutation run against external
  state. (TKT-232)
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
  **Second half (TKT-227 closeout):** by the time the ticket was worked, `make up` was broken again
  for an unrelated reason — TKT-244 made a credential mandatory in `compose.yaml` without adding it
  to `scripts/env-bootstrap.sh`, and the interpolation error told the developer to run the very
  command that had just failed. `make check` stayed green because its smoke stage builds its
  environment from `scripts/stack-env.sh` instead. **A gate that supplies its own version of a
  shared input cannot notice that the real one is missing** — `scripts/check-required-env.sh` now
  compares the two, deriving the expectation from the requirement rather than from the behaviour.

- [**A test that restates its helper does not pin the wiring**](./learnings/2026-08-17-a-test-that-restates-its-helper.md) —
  TKT-248 shipped three tests that could not fail, all caught by cross-model review: one proved the
  SCHEMA while the handler guard was deletable (request validation is unconditional —
  `Router`'s bool is `validateResponses`), one returned arithmetic over values its own fake had
  assigned and called it money, and one called the decision helper directly so reverting the CALL
  SITE left it green. One shape: asserting inside the process instead of at the boundary the value
  crosses. For every guard, name the edit that REMOVES IT FROM THE PLACE THAT USES IT — not the one
  that breaks it — and check the test catches that.

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
- [A green test that cannot reach the failing state](learnings/2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md)
  — TKT-236, TKT-238. `AGENTS.md` already says to ask what a *red* test's fixture can distinguish;
  this is the other half. **Before trusting a green test, ask whether it can reach the state that
  would fail.** Three defects in one epic hid behind tests that named the right case and were green:
  one asserted against an in-memory fake that scopes in Go while the shipped SQL predicate was
  deleted (wrong tier), one re-enabled the row in setup before testing that a rename doesn't
  re-enable it (the fixture repaired the precondition), and one needed *pool full + channel cap free
  + window shut*, a state the obvious fixture cannot build because filling the pool through the
  public channel leaves the presale's reservation intact by construction. None was a forgotten case.
  The gap is between the state a test *names* and the state it *constructs*, and unlike a red test
  nothing prompts you to look.
- [A green test can assert the defect as correct](learnings/2026-08-10-a-green-test-can-bless-the-defect.md)
  — TKT-239, and the sibling of the entry above. A test that encodes the WRONG invariant is green,
  stays green, and **kills every mutant** — the mutant flips the mechanism, the assertion was written
  to match the mechanism, so they agree and the report says covered. A draw-down test asserted usage
  `== 6` after drawing 4 of 10, which is exactly what the defective code produced; the real invariant
  was that usage never changes. Cost: a code capped at 10 granted 20. Nothing local catches this —
  types, tests, mutants and the gate are all downstream of the author's model of correctness — which
  is why cross-model review is a prerequisite, not an option. Derive expected values from the
  requirement, never from a run, and prefer an invariant to a number.

## 2026-08-11 — Making a value load-bearing is an authorization change (TKT-240)

Commerce forwarded `channel_code` to inventory so a channelled sale would consume its own channel's
allocation. One key in one map, with a test asserting the exact wire key and a mutation check proving
the wrong key turned it red. All correct, and none of it the problem: `POST /reservations` is
**unauthenticated** and takes that field from the request body, so once inventory decided on it, any
caller could drain a reseller's allocation with no credential — in the same ticket that built a
credential class whose central claim was that partners are confined to their own channel. Two sibling
hold paths (persisted replay, exchange target) were missed identically, because every layer reasoned
about *the path being changed* rather than *every producer of the value*. Enumerate the producers, ask
which are authenticated, and put the guard where the decision is. Caught by cross-model review; the
fix then exposed a second defect that only the second pass caught.
[full note](learnings/2026-08-11-forwarding-a-value-is-an-authorization-change.md)

## 2026-08-15 — Make it unsubmittable, don't validate it (TKT-244)

A back-office form drove a **full-set replace**, so it had to submit rows it did not edit — including
`sold_by`, which TKT-246 had just made load-bearing under the pool row lock. Three adversarial review
passes found the same defect in three disguises, and each fix was locally reasonable: carry the true
values in hidden inputs (both the value *and* the "was it edited?" evidence come from the POST body);
merge them server-side keyed on the channel (the key is client-supplied too, so an unmatched row falls
through to client values); refuse an unmatched row (a subset check is half a boundary — on a full-set
replace, **omission is deletion**, and an empty submission clears everything). The tell was
structural: after each fix, *"can the client influence this field?"* still answered **"yes, but only
if it lies in a way I now check for."** What held was deleting the capability — the row type carries
only what the screen renders, the parser ignores the rest, and the submitted set must match the
current one in both directions. Inventory could not backstop any of it: it validates channel, cap,
duplicates, capacity and consumption, and never `sold_by`. One of my own tests had asserted the
defect as the requirement — green, well-named, and immune to mutation testing, because the mutant
flips the mechanism the assertion was written to match.
[full note](learnings/2026-08-15-make-it-unsubmittable-not-validated.md)

## 2026-08-15 — A precondition that cannot fail is worse than none (TKT-250)

Adding optimistic concurrency to the allocation editor meant carrying a revision from the page to
the server. The editor has **two** reads of the same set, for opposite reasons: a *fresh* read taken
during the POST — the only trustworthy source for the fields the form deliberately cannot carry
(TKT-244) — and the revision the page held when it was **rendered**. Send the first as the
precondition and the server compares its current value against itself, matches every time, and the
protection is inert while the UI claims it works. Nothing local sees it: the store, API and wire
tests all construct their own requests and pass honestly, and a mutation check passes too, because
the *store* is correct — the defect is in which value the caller chose to send. Only the browser
tier can, since the seam exists only in a real request. Corollaries: after a refusal re-render the
**submitted** token, or the second click applies the set the refusal just stopped; and a fixture
writing the guarded table directly does **not** move the counter, so the "stale" value still
matches. Also: the ticket was filed as an authorization defect that the code does not have — the
remedy survived, the justification did not, caught only because shaping verifies a proposed remedy
against the code instead of inheriting it.
[full note](learnings/2026-08-15-a-precondition-that-cannot-fail.md)

## 2026-08-16 — A fixture that seeds two mechanisms proves at most one (TKT-241)

A ticket whose entire subject was *"this test is green without proving anything"* shipped a first
version that was green without proving one of the two things it seeded. Each test seeded a
channel-scoped **fee rule** (ADR-046) *and* a **split schedule** (ADR-047), because commerce treats
"no rule matched" as a successful resolution with an empty fee set — so without a seed the sale
completes identically whether the resolver ran or not. That argument was written down at plan-review
as the justification for the fee seed, and simply not applied to the split seed beside it: the
assertions read the fee's code and amount, and split selection changes neither. A fee with no split
is forwarded with no parts and settles as collected-and-unattributed, so deleting the split seed
left all three tests green. The proof, once the assertion existed: every snapshot reports
`mode: "unsplit", reason: "no_schedule"` while `passed_on_fees` stays **600** — that unchanged 600
is the finding. Nothing local caught it; the gate, the tests, and the author's own mutation set were
all downstream of the same understanding that produced the gap. Two corollaries: **a mutation caught
by a lower tier proves the mechanism is live, not that your test caught it** (three of five died in
a per-service suite that runs first, so the new tier never executed), and **a ticket delivering
coverage for a known gap must demonstrate the gap** — one run with the new tests red and the
pre-existing guards green, or "we added a test for it" is unfalsifiable.
[full note](learnings/2026-08-16-a-fixture-that-seeds-two-mechanisms.md)

## 2026-08-17 — A harness that cannot catch what it hunts (TKT-202)

Nine findings across four adversarial passes on one diff; three were the same class, and two were
defects in the *previous* pass's fixes. The class is already in this file — a fixture that cannot
reach the failing state — but TKT-202 shows it biting one level up, in the **attack harness**. A
brute-force sweep written specifically to attack the sanitiser ran **576 arrangements of `.`, `..`,
empty and junk segments and passed while the defect was live**: it put a harmless `"x"` in the junk
position and asserted only that the *surviving* reference was gone, so it could never observe a leak
in the popped position. Re-run with live references in every slot it found the defect immediately,
then found **twelve more spellings** the first fix still missed. For a "value must not appear in
output" property, **every position the generator can fill must be filled with the value that must not
appear**; a placeholder is a blind slot. The second shape: **a guard can test the mechanism and never
test the wiring.** Deleting the span processor from `setup.go` left the whole suite green — the test
built its own provider and proved the processor worked, not that production used it. Extracting a
shared helper did *not* fix it: replacing the call *inside* `Setup` still compiled, still leaked,
still passed, because the test exercised the helper. What held was asserting on **bytes that actually
left the process** (the real `Setup`, a local collector, grep the OTLP payload). Ask which edit your
test catches: breaking the mechanism, or removing it from the place that uses it.
A same-day corollary from TKT-199: **a mutation that breaks the mechanism *syntactically* proves
nothing.** Deleting a SQL tenant predicate by replacing it with an untyped `$2` made Postgres refuse
the statement, so eight tests failed and it read as strong evidence — of the query being reachable,
not of the predicate being load-bearing. The valid mutation keeps the statement well-formed and flips
only the predicate's truth; then **one** test fails, saying `cross-tenant publish = <nil>, want
ErrNotFound`. After a mutation goes red, read which tests failed and what they said: evidence is a
**semantic** failure, not a **structural** one, and a broad cluster of unrelated failures usually means
you broke the scaffolding.
[full note](learnings/2026-08-17-a-harness-that-cannot-catch-what-it-hunts.md)

## "Ancestor of the review base" is not "shipped"

A review scoped to a diff sees **commit topology**, never **deployment state**, and the two look
identical from inside the diff. Two of TKT-259's third-pass findings — one `[high]` — reasoned
correctly from a false premise: migration `0022` was introduced in a commit that was an *ancestor of
the review base*, so the reviewer concluded databases had applied it and demanded a forward migration
to repair legacy rows and replace an index. That commit existed only on the unmerged branch;
`git merge-base --is-ancestor <sha> origin/main` returned false and both findings evaporated. Any
finding of the form *"this breaks existing data / deployments / consumers"* carries an unstated
shipped-premise — resolve it against `origin/main` before accepting **or** refuting. And record the
refutation: the remediation is exactly what the *next* change to that file will need. Note also that
**refuted ≠ overridden** — a gateless run's `overrides:` list should not conflate an objection you
proceeded against with one that rested on a checkably false fact.
[full note](learnings/2026-08-18-ancestor-of-the-base-is-not-shipped.md)

## A test for a SWAP needs the two swapped things distinguishable in every position

The [green-test-that-cannot-reach-the-failing-state](learnings/2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md)
rule has a sharper form for swapped arguments. TKT-259's release query pairs each column with its own
claim-time observation (`$7` with the switch, `$8` with the capacity); crossing them type-checks, runs
and reports progress backwards. **Three** successive versions of the test named for that pairing could
not detect it: `false,false` against one set column made both arms read the same flag, and
`true,false` against *two* set columns let either arm carry the `OR` alone. Only one set column with
asymmetric flags matched to it discriminates. For a predicate over `(column_a, flag_a)` and
`(column_b, flag_b)`: the columns must differ, the flags must differ, and the differing flag must
guard the differing column. Mutation testing did go red — on three *other* tests, which is how the
vacuous one kept looking covered.
[full note](learnings/2026-08-18-a-test-for-a-swap-needs-distinguishable-positions.md)

## Harmless is not free: check a fix against the QUEUE's ordering, not just its correctness

TKT-259's first review pass found rows being charged retries for a condition they could not influence,
until they parked and became permanently unreachable. The fix — stop charging them — closed the
finding exactly and was still wrong: the claim is `ORDER BY next_attempt_at` with a `LIMIT` inside a
bounded pass, so a backlog of those now-harmless rows filled every batch and pushed actionable work
past the bound. Harmless individually, expensive collectively, and the cost fell on exactly the work
the sweep existed to do. When a fix's shape is *"this is now harmless to process"*, ask whether it
still occupies a slot in something **bounded** (`LIMIT`, batch, pass cap, pool, buffer) and what the
**ordering** does — with `ORDER BY` + `LIMIT`, older unresolvable rows outrank newer actionable ones
precisely because nothing ever resolves them. If the processor does nothing with them, exclude them;
visibility is a gauge's job, not the work queue's. The tell for the meta-failure: pass 1's fix
answered the question asked and made the better answer harder to see.
[full note](learnings/2026-08-18-harmless-is-not-free-in-a-bounded-queue.md)

## A test that pins the HARNESS, not the contract — and the coordinated reversion that exposes it

TKT-254's snapshot test asserted that its interleaved append had *landed*, plus a guard that the race
had actually been constructed. It was observed red before the fix, for the right reason, and every
single mutation of the production code killed it. It still could not fail: a reviewer proposed
reverting the transaction **and** moving the callback above the scan it was supposed to follow, and
the test passed with the defect fully live. The assertions were facts about the **harness** ("the
callback ran", "the append landed") and about the **verdict**, never about what the code **read** —
so the seam let the test *place* an event and the test then asserted the event had been placed. The
tell: every assertion was satisfiable by editing the instrumentation rather than the logic. The fix
is to make the seam **report what the code observed** (each scan's highest sequence) and assert those
**agree** — an invariant no callback placement can manufacture, because the divergence is created by
the commit, not the call. Two corollaries: report such a probe by `defer`, or it never fires on
exactly the runs where the contract is broken; and try the **coordinated** reversion (production
change reverted *and* the seam adjusted as a careless refactor would), because a test surviving every
single mutation can still be pinning its own instrumentation.
[full note](learnings/2026-08-19-a-test-that-pins-the-harness-not-the-contract.md)
