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
- [**A coarse observable can be produced by the broken version too**](./learnings/2026-07-25-coarse-observables-pass-the-broken-build.md) —
  a test asserting a downstream consequence (process exited, container restarted) may be satisfied by
  the very defect it exists to catch. Mutating `waitConsume` to swallow its error still restarted the
  container; only the durable-named diagnostic stayed red. Pair a coarse signal with one only the
  correct path emits — and you find this by *running* the mutation, not by reading the test.
  (TKT-99, PR #112)
- [**Force the interleaving — repetition cannot falsify a race fix**](./learnings/2026-07-29-force-the-interleaving-repetition-cannot-falsify-a-race-fix.md) —
  `-count=N` cannot tell a *closed* race from a *narrowed* one, and on a quiet stack it usually
  cannot produce the red at all (TKT-132's flake: reported 2-in-7 on CI, measured **10/10 green**
  locally). Make the losing interleaving deterministic — a temporary sleep at the seam, longer than
  the runner's tick — and run the same forcing against both versions: old **FAIL 51.17s** with the
  verbatim reported message, new **PASS 3/3**. The forcing is scaffolding and is not committed.
  Measure the baseline *before* designing the red. (TKT-132, PR #123)
- [**The fail-closed validator emits `Cache-Control: no-store` too**](./learnings/2026-07-29-fail-closed-validator-emits-the-same-header-as-the-success-case.md) —
  ADR-028's wrap sets `no-store` on its own 500, so a test asserting only that header passes when the
  200 it wanted was withheld. Refuted for **catalog** (its `env.do` sniffs the mask by body —
  narrowing the contract enum so every draft response drifts made all five cases fail loudly);
  **still live in inventory, commerce, payments and access**, whose unit tests run through the same
  validator with no such guard. Assert the status alongside the header. Settled by running the
  mutation, not reading the code. (TKT-107, PR #126)
- [**`format: uuid` is unenforced, and catalog validates its own rejections**](./learnings/2026-07-29-format-uuid-is-unenforced-and-catalog-validates-its-own-rejections.md) —
  kin-openapi checks string `format` only if you enable it, and nothing here does, so a
  `format: uuid` param constrains nothing. Catalog's 400 for a bad UUID comes from the **codegen
  binder**; the other four services' come from hand-rolled `parseUUID`. And catalog wraps
  `ResponseValidator` **outermost**, so ADR-028 checks the binder's 400 too — nine lifecycle ops
  that declared no `'400'` therefore returned **500 in production** for pure caller error. The
  ticket named the wrong layer *and* called it latent; one throwaway probe at claim time changed
  the severity, the option set and the scope. Probe the stated **mechanism**, not just the symptom.
  Also: a declared response header *is* enforced (enum + `required`), so pin it by feeding the test
  the production constant — a hardcoded wrong value passes no matter what the constant says.
  (TKT-110, PR #127)
- [**A decoded page is not an answered one**](./learnings/2026-07-30-a-decoded-page-is-not-an-answered-one.md) —
  a fail-closed evidence check asked "does a refund already exist?" and treated any response that
  unmarshalled as an answer. Go zero values make a truncated page (`Data == nil, HasMore == false`)
  **bit-for-bit identical** to the genuine `{"data":[],"has_more":false}` — the one answer that
  licenses submitting a second refund. Hand-written fixtures did not save it, because every fixture
  was *complete*: the test that finds this omits a field rather than supplying a strange one. Make
  absence representable (pointers + a positive `object == "list"` check). The same review found the
  mirror bug: a deliberately lenient matcher, defended as "leniency means we don't submit, so it
  fails safe" — true, and wrong, because adopting a refund also **appends a money fact**. On a
  two-sided risk, "fails safe" is not an argument until you say which failure it is safe against;
  the binary return type was the tell, and the fix was a third verdict, `Inconclusive`.
  (TKT-116, PR #129)
- [**Check *why* a test is red, not just that it is**](./learnings/2026-07-30-check-why-a-test-is-red-not-just-that-it-is.md) —
  red-first was followed and still produced two tests that could never fail for their stated reason:
  the stub answered **500 for unregistered routes**, so "the code did the wrong thing" and "the code
  did the right thing against an unconfigured stub" were the same observable. Both would have passed
  with the feature deleted. When the behaviour is an **interaction** ("resolve before submitting",
  "never call X"), assert the **request sequence**, not the returned value — outcomes collide,
  sequences don't. And a permissive stub is a hazard, not a convenience. Corollary for review-fix
  commits, where fix and test land together and the red phase is skipped by construction: run the
  mutation instead of asserting it's obvious — one gate cycle turns "obviously it would fail" into
  one dead test with the intended message. (TKT-116, PR #129)
- [**A fingerprint of a symmetric secret is an oracle**](./learnings/2026-07-30-a-fingerprint-of-a-symmetric-secret-is-an-oracle.md) —
  a ticket asked for a logged HMAC "fingerprint" of the journal signing key so operators could spot a
  mis-paste; the plan gate accepted it and the **code review refuted the premise**. Domain separation
  was proven (30-byte domain vs an always-32-byte signing input), the output was truncated, and a
  test asserted the secret was absent — all true, none of it the risk. A deterministic function of a
  secret over a fixed public message is an **offline verification oracle**; the only defence is key
  entropy, and nothing required any (`minSecretLen` checks length; the default key is the readable
  `local-development-journal-key`). Public-key intuitions — SSH fingerprints, JWK thumbprints — do
  not transfer to symmetric material. Prefer **rejecting** a mistake to reporting it: the replacement
  refuses startup when the active key *decodes* to a ring key, which is smaller and stronger.
  Second lesson from the same ticket: three passes each closed one base64 **representation** (text as
  received → unpadded canonical → padded) before the fix became "compare decoded bytes". When
  successive fixes each close one instance of a class — individually valid, individually shrinking,
  all the same shape — that is enumeration, not convergence. Change the question, and state the
  boundary (URL-safe is excluded *out loud*). (TKT-117, PR #130)
- [**Record a relaxation as a relaxation**](./learnings/2026-07-30-record-a-relaxation-as-a-relaxation.md) —
  ADR-025 §D9 said alarm payloads carry "identifiers and enums **only**"; six fields across the three
  shipped classes never satisfied it, including the integrity payload the clause was written for.
  Correcting it took **three review passes on one paragraph**, each finding a different error the
  previous fix exposed: the amendment prohibited free text the code emits → the rationale claimed it
  "keeps every prohibition" when `only` had prohibited exactly what it now admits → prohibitions that
  `only` already entailed were labelled "Added". **A clause that widens must say so**, as a delta:
  preserved / relaxed / preserved-and-made-explicit. A relaxation recorded as a correction lets the
  next reviewer treat the carve-out as pre-existing policy and measure the next widening against an
  already-widened baseline. Also: half 1 of the same ticket (code, with a test) was approved in pass 1
  and never returned — **every finding landed on the half with no executable check**, so budget
  adversarial passes toward governing prose, not away from it. And when sweeping restatements,
  distinguish them from *citations*: a pointer to a clause stays correct after an amendment and
  editing it is churn. (TKT-119, PR #131)
- [**You cannot classify an event you caused**](./learnings/2026-07-30-you-cannot-classify-an-event-you-caused.md) —
  `durableconsumer.Wait` had to choose, when a cancelled context and a closed subscription were both
  ready, which to report. "Termination wins" is the obvious answer, was implemented, verified,
  documented — and **reverted**: both mains `defer nc.Close()` without joining consumers, so an
  ordinary stop routinely closes the subscription itself, and `Closed()` does not encode its cause.
  Preferring termination emits durable-death alarms on **clean shutdowns**, destroying the evidence
  channel the same ticket had just added. **Before writing an arbitration, ask whether your own
  shutdown path can produce the signal.** If it can, and the signal doesn't say who produced it, the
  safe direction is the one that cannot fire spuriously — false alarms on the common path cost more
  than missed detections on the rare one. Two smaller rules from the same ticket: **a mutex only
  serializes writers that take it** (a `ctx.Err()` guard inside `readinessMu` was a TOCTOU because
  `Wait` latches readiness through a plain atomic outside it — a one-way flag closes what
  re-asserting only shrinks); and **say what each accepted residual costs** — two were called "the
  same" when one gives up an exit code and the other the whole signal. (TKT-122, PR #132)
- [**When a new check fails 27 tests, ask which side is wrong**](./learnings/2026-07-30-ask-whether-the-fixtures-or-production-are-wrong.md) —
  enforcing ADR-009 §5's `type == subject` in inventory broke 27 subtests whose fixtures omitted the
  field. "Stale fixtures, update them" was right, and **from inside the suite it is indistinguishable
  from "the producer omits it too", where the same check terminates every real event**. Both worlds
  show N tests failing on one absent field, because fixtures inherit the assumptions of the code they
  were written against. One grep at the producer settles it (`events.go` sets `Type` for all four
  consumed subjects). Signals you are in this situation: the check is a contract/schema assertion,
  the failures are broad and uniform, and the fixtures predate the contract. Corollary on the repair:
  stamp the missing field at the few construction sites rather than editing every literal — it was
  not those tests' variable — and leave the one test where it *is* the variable unstamped.
  (TKT-123, PR #133)
- [**A gitignore rule can outlive its reason — and predate its victim**](./learnings/2026-07-30-a-gitignore-rule-can-outlive-its-reason.md) —
  a finished architecture review sat untracked for five days, cited by an ADR, contradicting
  `AGENTS.md`'s "documentation is 100% in-repo". Whether it was ignored *deliberately* looked like an
  owner-only judgement call; `git log -S` gave the chronology and commit context that made it
  answerable: the ignore rule was added **thirteen days before the file existed**, inside a commit
  titled "untrack build artifacts" that also deleted a 69 MB binary. That does not read anyone's
  intent — a directory rule can be prospective — but it does establish the rule was no reaction to
  this document, and that no recorded reason exists to preserve. **Before preserving a decision, check that a decision was made** — configuration is
  not evidence of choice, and the tell was an undocumented entry sitting among documented ones.
  Recording an invented rationale is worse than either outcome: it looks like institutional memory
  and stops the next person from pulling the thread. `git log -S` cannot read intent — a directory
  rule can be added prospectively — but a rule that **predates its subject** cannot encode a
  judgement about it, which is the strongest form of the answer. Corollary: a docs-link checker must
  resolve against `git ls-files`, never the filesystem — a filesystem checker goes **green on this
  very defect** on the author's machine and red only in a fresh clone. (TKT-139, PR #136)
