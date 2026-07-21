# Learnings

Index of recurring lessons. Detailed notes live in [`learnings/`](./learnings/), one file per topic.

## How it works

1. Capture a fresh learning as `learnings/YYYY-MM-DD-short-title.md`.
2. When a lesson stabilizes (seen twice, or actionable for agents), **promote it** into `AGENTS.md`, a path-specific Copilot instruction, or an OpenCode skill — so it actually changes behavior instead of sitting in a passive doc.
3. This page lists only the **top recurring lessons** worth surfacing to every contributor.

## Top recurring lessons

- [**A test validator downstream of the production wrap sees laundered statuses**](./learnings/2026-07-21-test-validator-downstream-of-production-wrap-sees-laundered-statuses.md) —
  when production middleware rewrites contract drift into a documented generic 500, a test-side
  re-validation of the recorded response can only pass — the undocumented status was laundered
  before the helper saw it. Detect the mask body itself (written by exactly one place) instead of
  re-checking the status. (TKT-108, PR #84)

- [**API smoke can't see the SSR layer — a browser-submit test is the only checkOrigin catch**](./learnings/2026-07-20-browser-submit-is-the-only-checkorigin-catch.md) —
  Astro's default SSR `checkOrigin` 403s every back-office POST behind the gateway reverse proxy
  (`Origin` ≠ `Host`); it silently broke *all* back-office write forms from TKT-102 on, invisible
  to the smoke suite because smoke hits the catalog API directly and only renders `/admin/`, never
  submitting a form. A web-UI ticket isn't verified until a browser has submitted its write path.
  (TKT-105, PR #82)

- [**Time-window fixtures must be relative to now**](./learnings/2026-07-18-time-window-fixtures-must-be-relative.md) —
  a calendar-literal fixture judged against `now()` with a bound is green at merge and fails when
  the clock crosses the literal plus the bound; CI, mutation testing and review all pass because
  the defect only exists later. Build such fixtures from `time.Now()` with a deliberate offset,
  pinned once per run. (TKT-85 fixture, caught by TKT-93's gate run, PR #71)

- [**Seed the failure state; don't race a kill**](./learnings/2026-07-17-seed-the-failure-state-dont-race-a-kill.md) —
  for leftover-state defects (trap ordering, pre-clean, stale locks), fabricate the dirty state
  directly (compose up the stateful piece, plant a marker, `docker kill` the container) instead of
  SIGKILLing the real run on a timer; red and green each become observable in seconds. (TKT-70, PR #68)

- [**Reconciliation acts only on positive assertions — absence is never death**](./learnings/2026-07-17-reconciliation-acts-only-on-positive-assertions.md) —
  a published-only per-id lookup makes "archived solo slot" and "live festival group pool"
  indistinguishable (both 404), and inventory pools carry no kind marker; a converge pass that
  treats 404 as death archives live sellable inventory. Converge only on an endpoint that answers
  for the id's whole kind-union, in any lifecycle. Caught at plan review on TKT-90. (TKT-90, PR #67)
- **Dropping a table-wide UNIQUE drops its backing index** — audit every query that used the
  index prefix before an ADR-025-§D7-style constraint swap; the partial replacement cannot serve
  rows outside its predicate, so per-scan reads (chain verification, History) silently become
  sequential scans. Caught at plan review on TKT-84 only because a crashed drafter run's log was
  salvaged. (TKT-84, PR #62)
- [**A load test's published number is a claim about the server only if the generator's health is part of the verdict**](./learnings/2026-07-17-load-test-generator-health-is-part-of-the-claim.md) —
  the first accepted per-pool ceiling was defined by the client's in-flight cap, not the pool;
  three review passes closed the vacuous routes (all-409 windows passing on empty percentile
  sets, cap-defined knees, missed schedules). Rules now in the harness: per-stage outcome
  expectations, Little's-law cap sizing above `rate × SLO`, and inconclusive-≠-unstable as
  distinct outcomes. (TKT-82, PR #59)
- [**A finding's mechanics being true does not make its trigger reachable**](./learnings/2026-07-16-refute-the-trigger-not-the-mechanics.md) —
  triage reviewer findings in two steps: verify the causal chain in the code, then grep for the
  event that starts it. A true mechanism with an absent trigger is not a PR fix — it's a backlog
  ticket gated on whatever would ship the trigger. The review-side dual of premise rot (TKT-73).
  (TKT-79, PR #58)
- [**Upsert guards are snapshot-stale under lock contention**](./learnings/2026-07-16-upsert-guards-are-snapshot-stale.md) —
  `ON CONFLICT DO UPDATE ... WHERE NOT EXISTS(...)` waits on the row lock but evaluates its
  subqueries against the pre-wait snapshot: state committed while it queued is invisible and the
  guard silently passes. A guard that must see concurrent commits cannot live in the statement
  that waits for them — lock first (own statement), decide in the next. Shipped *as a fix* past a
  sequential regression test; caught by ai-review pass 2. (TKT-76, PR #57)
- [**A lock-queue handshake must pin the exact statement under test**](./learnings/2026-07-16-lock-handshakes-pin-the-exact-statement.md) —
  a `pg_stat_activity` predicate matching any table waiter observed the *wrong* statement (the
  ON CONFLICT insert absorbed the wait) and the concurrency regression passed with the guarded
  statement deleted. Pin the statement's exact text, stage so it is the one that must wait, and
  mutation-check by deleting it. Corollary of TKT-78's "prove the waiter queued first": prove
  *which* waiter. (TKT-76, PR #57)
- [**Per-entity version counters do not survive convergence onto a shared resource**](./learnings/2026-07-16-version-scope-vs-convergence-scope.md) —
  a version orders events only within the scope that issues it. Grouped days each carry their own
  monotonic `closure_version` but share one pool; a pool-level comparison judged day B's first
  closure "stale" against day A's counter and kept selling a weather-closed day. Order per source
  entity, derive the shared state under the lock — and the regression test needs *two* entities
  with overlapping sequences. Drafter, critic and implementer all missed it; adversarial review
  caught it. (TKT-75, PR #56)
- [**An anti-replay rule must be re-checked against the retry it exists to serve**](./learnings/2026-07-16-protocol-rules-safety-and-liveness.md) —
  three review rounds each fixed one side of the same idempotency rule (retry forged into a
  conflict → replay oracle → lost-retry never admits). Protocol rules have a safety side and a
  liveness side; every one-sided fix is locally correct, so state both together and re-run the
  opposite side's motivating scenario after each fix. (TKT-73, PR #55)
- [**A time cutoff decided behind a lock queue must use decision time, not transaction time**](./learnings/2026-07-16-lock-queue-time-cutoffs.md) —
  `now()` freezes at transaction start, so a transaction that waits on a row lock across a time
  boundary decides it with stale time. And the regression test is vacuous unless it proves the
  waiter queued *before* the boundary (DB clock + `pg_stat_activity` handshake + mutation check) —
  the sleep-based version passes under the broken code too. (TKT-78, PR #54)
- [**A column narrower than the value the code accepts turns storage into a validator**](./learnings/2026-07-17-column-range-narrower-than-code-accepts.md) —
  an int32 column behind a Go `int` range check meant a huge-but-"valid" wire value passed every
  explicit check and died in the INSERT — landing in the generic retry arm as a permanent NAK loop.
  Match the column to the full range the code accepts, or classify overflow explicitly before the
  write; and ask which disposition arm catches a DB range error. (TKT-68, PR #64)
- [**Judge idempotent replays by lifecycle state, never by timestamp**](./learnings/2026-07-16-judge-replays-by-lifecycle-state.md) —
  a replay guard keyed on a derived signal (elapsed TTL) was wrong in both directions, and its
  catch-all turned unknown states into "convert again" — a double-carve instruction. Unknown over a
  service boundary is an invalid response, not a terminal state (ADR-017 applied to HTTP). Took
  three review passes to converge; tests built from the same mental model never objected. (TKT-77, PR #53)
- [**Name what a control reaches, not what it is for**](./learnings/2026-07-15-name-what-a-control-reaches.md) —
  a check gets named for the job it was meant to do, and the name is then read as a guarantee. Every
  fact correct, tests green, sentence false — so tests, the author, and consistency are all blind to
  it. Four instances across two tickets; every one caught by review, none by the gate. (TKT-57,
  TKT-67, PR #51)
- [**A `pgrep`-for-the-command-name watcher self-matches its own command line**](./learnings/2026-07-21-pgrep-watchers-self-match.md) —
  a background-gate "is it still running?" poll written as `until ! pgrep -f "make check"` reported
  "still running" for minutes after the gate had exited, because the watcher's own command line
  contains `make check`. Judge a backgrounded gate by an explicit exit-code sentinel + log-body scan,
  never by process-name liveness. Extends the TKT-101 "background exit code lies" family. (TKT-106, PR #83)
- [**A passing test is not evidence it tests anything**](./learnings/2026-07-15-prove-tests-fail.md) —
  green is the default state of a test that is misaimed, drifted, or not running. Break it on purpose
  and confirm it goes red. (TKT-53, TKT-60, PR #43)
- [**A `-run` allowlist silently strands tests**](./learnings/2026-07-15-run-allowlists-strand-tests.md) —
  a test missing from the allowlist never runs and the gate still passes. Cost two merged tests that
  had never executed; the same trap had already bitten another package. (TKT-53, TKT-60)
- **ADR-017 §2 additive-no-bump now has a worked precedent** — `re_entry` rides
  `performance.published` at unchanged schemas 2/3; neither §3 trigger fired (no deployed
  consumer forks on it, the invariant is conditional-on-presence). Both TKT-87 plan drafts
  independently reached for heavier designs (schema bump / dedicated subject + backfill) and the
  critique had to pull them back to the ADR's own default — check §2 before inventing
  distribution machinery for a new optional field. (TKT-87, PR #73)
- **Catalog's `/internal/*` service-to-service routes are hand-mounted, outside the OpenAPI
  contract** — not in `catalog/api/openapi.yaml`, not codegen'd; the ADR-028 response validator
  skips paths it can't resolve, so a new internal endpoint needs no spec change or `make generate`.
  Inventory is the opposite (its request validator rejects undeclared paths; every route is
  declared). Both TKT-80 plan drafts over-scoped the catalog pin endpoint as an OpenAPI change —
  check the target service's existing `/internal/*` routes before planning. See
  `docs/learnings/2026-07-21-catalog-internal-routes-are-hand-mounted-outside-openapi.md`. (TKT-80, PR #86)
