# Ticketing System

Event ticketing software, rebuilt from scratch with AI-assisted development.

## Context

The owner has built **three** event ticketing platforms over their career and sold **tens of
millions of tickets** on them; the most recent exceeded **1M lines of code**. Treat them as a
**domain expert** in event ticketing — inventory, holds/reservations, seating, pricing, fees,
payments, fraud, high-contention on-sales, etc. Do not over-explain ticketing basics; do surface
non-obvious engineering trade-offs.

## Goal of this repo

This is a **testbed**, not a client engagement. The point is to evaluate AI-assisted software
development on a **non-trivial** application by:
- authoring specs and PRDs from the owner's domain knowledge, and
- trying **different AI / development flows** to plan and build the system (e.g. the `sdlc-ticket`
  skill and its board).

Optimize for learning how well a given flow produces a real system — not for shipping the fastest
possible MVP. When a flow or process choice is being exercised, follow it faithfully so the
experiment stays valid.

## Working agreement

- **The stack is decided and scaffolded.** Five Go services (catalog, inventory, commerce,
  payments, access) behind a Go gateway, TypeScript + React frontends, PostgreSQL (one database
  per service), NATS JetStream, all under one `docker compose up`. Layout and service ownership:
  `docs/architecture.md` (ADR-001/002/007). Don't propose a new stack; extend this one, and
  record deviations as ADRs.
- **Money is integer minor units + ISO currency code — floats are banned on money paths.**
- **Local gate:** `make check` (lint + test + build, Go & TS, plus the gateway smoke suite;
  run `make deps` first on a clean clone). CI runs the same gate — keep them mirrored.
  **Commit regenerated code (`openapi_gen.go`, `api-types.gen.ts`) before running the gate** —
  `check-generate` diffs against HEAD, so an uncommitted regen reads as drift and fails the run
  (cost two gate runs on TKT-114).
- **A web-UI ticket isn't verified until a browser has *submitted* its forms.** `make check`'s
  smoke suite does submit selected Astro forms through the real SSR layer, but it uses a Go client
  that constructs its own target, headers, cookies, and redirect behavior. It does not read the
  rendered form action, reproduce browser-generated `Origin`/`Referer` and SameSite behavior, or
  execute JavaScript. A broken action, cookie path, CSP rule, or browser navigation can therefore
  pass `make check`.
  For any back-office/storefront/scanner ticket that adds or changes a write form, run
  **`make browser`** and add a spec to `test/browser/` that submits the write path, not just the
  render. It is not part of `make check` — it needs the host's real Chrome, so CI cannot run it.
  **A browser spec cannot see a request the SSR handler makes.** The page submits a form POST and
  the upstream call happens server-side, so counting it from Playwright observes zero — TKT-244's
  "one save is one PUT" assertion was vacuous until it targeted the form submit instead. Assert what
  the browser can see (the submit, the redirect, the rendered result) or what the write left behind
  (re-read the database) — and re-read it **exactly**: that spec seeded whole-minute timestamps and
  asserted non-null, so it stayed green through a truncation that moved a sales window.
  (Why: [browser-submit is the only checkOrigin catch](docs/learnings/2026-07-20-browser-submit-is-the-only-checkorigin-catch.md), TKT-105;
  TKT-226 caught a `no-referrer` that 403'd every reset. Promoted from per-ticket throwaway
  scripts in TKT-228. R6 of the 2026-08-19 review proved the distinction by misrouting three form
  actions: `make check` passed while all three browser specs failed.)
- **A hidden input is not a checkbox, and `git add -A <dir>` is not `git add <file>`.** Two small
  rules that each cost a real defect. *Absent means false* holds only for inputs that CAN be absent:
  a hidden field always submits, so `value=""` is present-and-empty and reads as true — which is how
  renaming a disabled channel silently re-enabled it (TKT-236). And `git add -A docs/` swept an
  unrelated untracked file into a ticket's merge (TKT-237); stage the paths you edited, by name.
- **A test must live at the tier its mechanism does.** A cross-tenant assertion passed at the API tier
  with the SQL predicate deleted, because the in-memory fake scopes in Go (TKT-236). Ask which layer
  actually enforces the thing, and put the assertion there — a green test one tier above the
  mechanism proves the fake and the handler agree, and nothing else.
- **An error mapper matching on a STRUCTURAL interface claims every error that satisfies it — and the
  tier that sees the misrouting is not the tier that made the error.** Two rules from one defect
  (TKT-307). *One:* `problem()` matches `belowConsumption` on `interface{ Channel() string }` and runs
  it first, so a new 400 refusal — which had to name its channel, as every per-row refusal must —
  answered **409 with another failure's code**. That is worse than the 500 it replaced: a 500 says
  something broke, a confident wrong 409 sends the operator to fix a field that is fine. Before adding
  a sentinel, `git grep` the mapper for **shape** matches (`errors.As` on an anonymous interface), not
  just the sentinels it names, and place the new case above every shape match it satisfies. *Two:* the
  tier rule below splits in a way that is easy to satisfy and still miss. The *mechanism* was a DB
  CHECK surfacing, so a store smoke test was right and was written and stayed green; the
  *classification* is decided one tier up, where that test never runs. **When a COS names a status, a
  response body, or what a client sees, assert it where that is decided** — however deep the mechanism
  producing it lives. Corollary, same ticket: **an over-broad condition can be the only thing holding a
  correctness property.** `if e.Schema == 1 || errors.Is(err, errResolveUnavailable)` read as laziness;
  the first arm was the only thing making a real catalog outage retry, so narrowing it alone would have
  terminated outages and permanently lost publications. Evaluate each arm's coverage separately, ask
  what is reachable **only** through the arm you are deleting, and if that set is non-empty fix the
  classification upstream before narrowing.
  ([a structural match claims errors you never enumerated](docs/learnings/2026-08-31-a-structural-match-claims-errors-you-never-enumerated.md))
  **And the false-claim habit ITERATES when you write about a DESIGN SPACE — prose about what could
  be built needs the same verification as code.** TKT-306 took three attempts at one ADR paragraph
  ("structurally impossible" → "three designs avoid the signature change" → one viable design), each
  written to fix the previous one's false claim; two of the three proposed designs could not observe
  the condition at all, because the loader collapses rows by id and repeated ids are how a multi-part
  row is *stored*. A comment about code has the code beside it; a paragraph about designs that do not
  exist has no compiler, no test and no adjacent line to contradict it, so it reads as reasoning
  rather than assertion. **Two tells: the paragraph names a function you have not opened this
  session, or you are describing a design space rather than a mechanism.** Verify each alternative
  like a code change, or write that you have not checked. Corollary: **"unreachable through the
  database" does not justify changing a guard's semantics** — the fee duplicate guard was reordered
  past the currency check on the grounds that the id is a primary key, but the guard exists for the
  PURE SEAM that accepts hand-built input, and the same argument would make the guard itself
  pointless. Evaluate a defence's changes at the seam it defends.
  ([a design space has no compiler](docs/learnings/2026-08-31-a-design-space-has-no-compiler.md))
  **And a test has TWO independent properties — deterministic, and discriminating — so fixing one
  routinely destroys the other.** TKT-308 took four versions of one fixture: a sequential warm was
  stable and blind to the mutation; concurrent caught it and was racy; a barrier on the first burst
  restored determinism and the mutation passed again; barriers on both finally did both. **Every
  intermediate version looked finished**, because each correctly fixed the defect it addressed and
  nobody re-checked the property that had been fine a moment earlier — the mutation confirms the one
  you were just working on, and so does the repeat loop. **After any change to a FIXTURE, run both:
  the mutation (break the mechanism, confirm red), then 6–8 unmutated runs.** Every time, even when
  the change was "obviously" about only one of them. A test that has not flaked yet is not evidence
  it is deterministic — where a fixture depends on scheduling, force the schedule. And **a new test
  is a change that earns the same treatment as production code**: that ticket's two review passes
  produced six findings and **none touched the four-line production change** — the change was
  measured and checked, the tests were written plausibly. Two of the six were assertions written
  *because* a hazard had been identified and unable to see it (a traceparent header proving a span; a
  global provider "restored" when its delegate is set once).
  ([deterministic or discriminating, but not both](docs/learnings/2026-08-31-deterministic-or-discriminating-but-not-both.md))
- **A security claim is a hypothesis until it is executed.** Two consecutive adversarial passes
  rejected two different claims about one guard, both plausible, both written in good faith, both
  false; what settled it was running the sequence and watching it return 200 (TKT-236, ADR-053). When
  a gap turns out to be real and is not this ticket's to close, **pin it as a test that asserts it is
  present** — as ADR-021's rollback-gap test does — so the claim cannot drift from the behaviour.
- **A round of review fixes needs a review of the fixes' *interaction*, not each in isolation.**
  Four tickets in TKT-35 had a pass find a defect created by the previous pass's fix, every fix
  correct on its own; a bounded or optimized replacement is a new implementation, so ask what the
  version it replaced would have caught. And before debugging a red test, ask what its **fixture
  can distinguish** — a rule-refuses test whose fixture admits no allowed input, or a planner proof
  with too few distinct values, fails while the code is correct. ([fixes compose](docs/learnings/2026-08-03-two-correct-fixes-can-compose-into-a-new-defect.md),
  [fixture too small](docs/learnings/2026-08-03-a-fixture-too-small-cannot-show-the-negative.md))
- **And before trusting a *green* test, ask whether its fixture can reach the state that would fail.**
  A red test announces itself; a test structurally incapable of failing looks exactly like one that
  proves something. Three defects in TKT-236/TKT-238 hid behind tests that named the right case and
  were green — asserting against a fake that enforces in Go what the shipped SQL doesn't, repairing
  the precondition during setup, and naming a state the fixture cannot construct. Delete the
  mechanism and re-run: if it stays green, the test is about something else.
  ([a green test that cannot reach the failing state](docs/learnings/2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md))
  **But that mutation has TWO readings, and the second one means delete code, not fix the test.**
  A green mutation says either "the test cannot see the mechanism" or "the mechanism does not do
  anything" — and the reflex to harden the fixture is wasted on the latter. TKT-162 added a
  high-water ceiling to a newest-first keyset feed, wrote a test for it, and rewrote that test twice
  before working out on paper that a descending walk keeps the cursor at or below the ceiling, so
  the keyset predicate is strictly stronger and the ceiling could never change a result. The tells:
  successive fixture rewrites all fail to catch the removal, and you cannot state in one sentence an
  input for which the mechanism changes the output. Delete it — a dead mechanism with a green test
  beside it reads as a guarantee while the race it was meant to close stays open.
  ([the mechanism was inert](docs/learnings/2026-08-23-the-mechanism-was-inert-not-the-test.md))
  **And "the mechanism does nothing" splits again — ask WHY it is unreachable before deleting it.**
  Two situations produce the identical green mutation. TKT-162's ceiling was unreachable
  *structurally*: a descending walk can never exceed a page-one ceiling, so no evolution of the
  system makes it matter, and deleting it was right. TKT-304's exchange currency guard is
  unreachable *contingently* — `validate()` refuses any non-EUR resolution first — but that is a
  policy in **another file** which the code itself calls temporary, with TKT-10 tracking its removal.
  Delete it and the day multi-currency ships an exchange settles across currencies with nothing
  objecting. Ask: **is the thing making this unreachable a property of the algorithm, or a decision
  someone plans to reverse?** Structural → delete. Contingent → keep, and *reach it in a test* by
  varying the side the shadowing check does not police (seed the SOURCE row in the other currency —
  the state the removal will create), because a kept guard with no reachable test is an untested
  guarantee. Two corollaries: a test asserting the **currently observed** refusal pins the limitation
  as the acceptance criterion and goes red looking like a regression the day the guard starts
  working; and before deleting an exported symbol, "no callers" is necessary and **not sufficient** —
  ask what it is the last *producer* of. `ValidateExchangeTarget` was dead, and was also the only
  source of a sentinel whose mapping and table test its removal would have stranded, recreating this
  exact defect one layer up.
  ([inert for two different reasons](docs/learnings/2026-08-30-inert-for-two-different-reasons.md))
  **And a THIRD reading: the mechanism is live and the MUTATION is blind.** Deleting a guard and
  seeing green also happens when the input class that separates it from the guard beside it was never
  generated. TKT-305 removed a `json.Unmarshal` error check as redundant behind a nil check, having
  probed only JSON *syntax* errors — which do arrive nil. A *type* error populates the pointer before
  reporting, so `{"available":"bad"}` shipped as `200 available:0` and `{"available":7,"available":"bad"}`
  as `200 available:7`, a fabricated number worse than the original defect. **Two guards are redundant
  only if they accept the same input class**, and writing down what each accepts settles it without
  running anything. For any *malformed input must be refused* property, enumerate malformation
  **classes** — syntax, type, absence, **identity** (a valid answer about something else), **range**
  (valid and impossible, e.g. a negative count that was being clamped to zero and so reported as a
  sellout) — not examples. Same family as the harness that ran 576 arrangements without the value it
  hunted. **Corollary:** a second review pass finds false CLAIMS where the first found broken code —
  all three of TKT-305's pass-2 findings were sentences written while fixing pass 1, and nothing gates
  prose. Re-read every sentence added during a fix round and ask which function you actually opened.
  ([a mutation your generator cannot reach](docs/learnings/2026-08-31-a-mutation-your-generator-cannot-reach.md))
- **A guard with N predicates needs N tests, and scoping a write means scoping its FAILURE path too.**
  Two shapes from one ticket (TKT-251), both invisible to a test that only asserts "the write was
  refused". *One:* when several predicates guard one operation, an earlier refusal **short-circuits**
  the rest — an attach test giving the attacker a victim-owned parent *and* child never reaches the
  child predicate, so deleting it stays green. One case per predicate, each passing the earlier ones,
  each mutated separately. *Two:* a conditional write's **"why did nothing happen?" read is a second
  information channel**. `PublishPerformance` scoped its `UPDATE` and still answered
  `ErrNotSellable` about a victim's row — the write closed, the disclosure open. And when the
  mechanism moves tiers, **move the fake with it**: an untouched contract test whose fake still
  scopes the old way silently becomes an assertion that the *old* behaviour is correct.
  ([a green test that cannot reach the failing state](docs/learnings/2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md) §4–5)
- **And a green test can assert the DEFECT as correct — mutation testing cannot catch that.** When a
  test pins a *number* or a *state* rather than a refusal, derive the expected value from the
  requirement, never from a run: an assertion written by observing what the code did pins the
  behaviour, not the rule. TKT-239 asserted a code's usage was 6 after drawing 4 of 10 — exactly what
  the broken code produced — and every mutant died, because the mutant flips the mechanism and the
  assertion was written to match the mechanism. Say the invariant in one sentence without naming the
  implementation ("a draw-down moves a redemption, never creates or destroys one"), and prefer it to
  a value. Nothing local catches this: types, tests, mutants and the gate are all downstream of the
  author's model of correctness, which is why cross-model review is a prerequisite, not an option.
  The blessed value need not be a number: TKT-244's `takes a new row entirely from the form` blessed
  an **authorization fallback**, green and well-named, and only cross-model review caught it.
  ([a green test can bless the defect](docs/learnings/2026-08-10-a-green-test-can-bless-the-defect.md),
  [make it unsubmittable](docs/learnings/2026-08-15-make-it-unsubmittable-not-validated.md))
- **A fixture that seeds N mechanisms proves at most one of them — ask of EACH seed what would
  notice its absence.** Not "does the test pass": *"if I delete this seed, what goes red?"* A seed
  with no answer is decoration that makes the test look like it covers a layer it never observes,
  and reviewers read the fixture as a statement of scope. TKT-241 — a ticket whose entire subject
  was vacuous tests — seeded a fee rule **and** a split schedule, asserted only the fee's code and
  amount, and stayed green with the split seed deleted: a fee with no split is forwarded with no
  parts and settles as collected-and-unattributed. The correct argument for the fee seed had been
  written down one stage earlier and simply not applied to the seed beside it. Two corollaries.
  *One:* **a mutation caught by a lower tier proves the mechanism is live, not that your test caught
  it** — three of five mutations died in a per-service suite that runs first, so the new tier never
  executed; the mutation that is evidence is the one **only** the new tier can catch. *Two:* **a
  ticket whose deliverable is coverage for an existing gap must demonstrate the gap** — one run in
  which the new tests go red and the pre-existing guards stay green. Without it, "we added a test
  for the gap" is unfalsifiable.
  ([a fixture that seeds two mechanisms](docs/learnings/2026-08-16-a-fixture-that-seeds-two-mechanisms.md))
- **The fixture-that-cannot-fail question applies to your ATTACK HARNESS, and a guard can test the
  mechanism while never testing the WIRING.** Two shapes from TKT-202, each one level up from a rule
  already here. *One:* a brute-force sweep written to attack a redaction ran 576 arrangements and
  passed while the defect was live — it put a harmless placeholder in the position that leaked, so it
  could never observe one. For a "value must not appear in output" property, **every position the
  generator can fill must be filled with the value that must not appear**. *Two:* deleting a span
  processor from `setup.go` left the whole suite green, because the test built its own provider and
  proved the processor *worked*; extracting a shared helper did not fix it, since replacing the call
  **inside** `Setup` still compiled, still leaked, still passed. Ask which edit your test catches —
  breaking the mechanism, or **removing it from the place that uses it** — and assert at the boundary
  the value crosses on its way out (the wire, the row), not at the component supposed to be in the
  path. Corollary for redaction specifically: sanitise on the **shape of the secret**, not route
  identity — a check keyed on "is this a real route?" inherits the router's case and normalisation
  rules and leaks on every spelling the router rejects, and the request logger runs *before* the
  router. ([a harness that cannot catch what it hunts](docs/learnings/2026-08-17-a-harness-that-cannot-catch-what-it-hunts.md),
  ADR-012 § TKT-202)
- **When a value must not be client-chosen, make it UNSUBMITTABLE — validating it is the slower way
  to lose.** Every fix that *checks* a submitted value keeps the trust boundary in the client and
  merely moves where it leaks. The tell is structural: if *"can the client influence this field?"*
  still answers "yes, but only if it lies in a way I now check for", it is not fixed. TKT-244 took
  three review passes to learn it — hidden inputs, then a server merge keyed on a client-supplied
  key, then a refusal — before deleting the fields from the form's type entirely. And on a **full-set
  replace, omission is deletion**: enumerate both directions, a row that should not be there and one
  that should be and is not. Check what the downstream service actually validates before assuming it
  backstops you — inventory validates channel, cap, duplicates, capacity and consumption, and never
  `sold_by`. ([make it unsubmittable](docs/learnings/2026-08-15-make-it-unsubmittable-not-validated.md))
- **A precondition that CANNOT FAIL is worse than none — ask where the compared value comes from.**
  The tests-that-cannot-fail rule, one level up: not a test that cannot go red, but a *mechanism*
  that cannot refuse, while every test around it passes honestly because they are right and the
  thing they test is inert. If the value you compare against traces back to a read the **server**
  took during the same request, it is being compared with itself and matches every time. An optimistic
  token must originate from the state the client actually acted on — the rendered page — and travel
  back unchanged. TKT-250's editor has two reads of one set for opposite reasons, and only the browser
  tier can tell them apart, because the seam exists only in a real request. Two corollaries that each
  cost a cycle: after a refusal re-render the **submitted** token (re-render the fresh one and the
  second click applies the set the refusal just stopped), and a fixture that writes the guarded table
  **directly** does not move the counter, so the "stale" value still matches and the spec proves
  nothing. ([a precondition that cannot fail](docs/learnings/2026-08-15-a-precondition-that-cannot-fail.md))
  **Same rule, in SQL: a guard's predicate must be NULL-safe, or it fails OPEN.** TKT-254's drain
  barrier counted busy backends with `state <> 'idle'`, which reads as "anything not idle" and is
  **NULL** for a backend whose state is unknown — and `WHERE` discards NULL, so an unknown backend was
  counted as *drained* by a check whose entire job was to fail closed. Use `IS DISTINCT FROM`. Ask of
  any guard predicate what its **unknown** case does: if unknown means *pass*, the guard is a
  suggestion. And scope it to what can actually cause the thing you are guarding against — the same
  barrier counted autovacuum workers, which carry `datname` and cannot append, so unrelated
  maintenance could fail the whole gate (`backend_type='client backend'`). A guard that can fire on
  something it does not guard is a red gate with no defect behind it, which is worse than the flake.
- **A test can pin your HARNESS instead of the contract, and single-mutation testing will not show
  it.** One level past the green-test rules above: the mechanism is present, the fixture *can* reach
  the failing state, the test went red before the fix — and it is still about something else, because
  every assertion is a fact about the *test* ("the callback ran", "the append landed") rather than
  about what the code *read*. TKT-254's snapshot test survived every mutation of `store.go` and still
  passed under a **coordinated reversion**: the transaction removed *and* the seam moved the way a
  careless refactor would. The tell is that each assertion is satisfiable by editing the
  instrumentation rather than the logic. Make the seam **report what the code observed** and assert
  those values agree; report it by `defer`, or the probe never fires on the runs where the contract is
  broken. ([a test that pins the harness](docs/learnings/2026-08-19-a-test-that-pins-the-harness-not-the-contract.md))
- **Making a request field load-bearing is an AUTHORIZATION change, not plumbing.** Before something
  starts *deciding* on a value — capacity, permission, price, routing — enumerate **every producer of
  that value and ask which are authenticated**, not just the path you are editing. TKT-240 forwarded
  `channel_code` to inventory so a channelled sale would consume its channel's allocation: one key in
  one map, tested to the wire key, mutation-checked. But `POST /reservations` is unauthenticated and
  takes that field from the request body, so any caller could then drain a reseller's allocation with
  no credential — executed, not argued — and the same ticket's whole point was a credential confining
  partners to their own channel. Two sibling paths (the persisted replay, the exchange target) were
  missed for the identical reason: every layer reasoned about the path being changed. The guard
  belongs where the decision is (inventory, under the pool lock — ADR-064's `requires_code` shape),
  because a check in the calling service binds only callers who go through it. Cost: a full
  build-review-revert cycle, and the revert then exposed a second defect that only a second review
  pass caught. ([forwarding a value is an authorization change](docs/learnings/2026-08-11-forwarding-a-value-is-an-authorization-change.md))
- **Never edit a tracked file while the gate is running.** The gate reads the working tree as it
  goes, so an edit mid-run is read half-written and fails for a reason that does not exist. TKT-240
  edited `scripts/smoke.sh` at 23:01:47 during a run that ended 23:02:33 and spent a full cycle
  diagnosing it. The neighbouring rule — never chain a command onto the gate — already exists; this
  is its twin.
- **Specs before code.** Prefer writing/refining a spec or PRD before implementing. Ground work in
  the written spec, not in assumptions about how ticketing "usually" works.
- **Record decisions.** Capture architecture and design decisions as ADRs in `docs/adr/`
  (see `registry.bindingPath` in `.claude/sdlc.config.json`; template in `docs/adr/[template].md`).
- **Documentation is 100% in-repo** — no Confluence or external wiki. PRDs and briefs in
  `docs/product/`, ADRs in `docs/adr/`, learnings in `docs/LEARNINGS.md` + `docs/learnings/`,
  ticket context on the sdlc board; see `docs/README.md` for the rest. Anything the
  sdlc-ticket skill would send to a wiki goes to `docs/` instead.
- The `sdlc-ticket` skill and its board are the default workflow scaffolding for planning and
  tracking work here. **Ticket state is not in this repo.** It lives as one note per ticket in a
  Fast Note Sync vault, served by the board in `~/sources/sdlc-board` (`python3 server.py 8787`);
  read and write it through that server's HTTP API, per
  `.claude/skills/sdlc-ticket/references/local-tracker.md`. The `.sdlc/` directory here is a
  **superseded rollback stub** — its `server.py`, `board.html` and `config.json` are no longer the
  source of truth and must not be run. The `sdlc-state` branch is likewise a **read-only archive**
  refreshed by a scheduled pull; never write to it.
- **Changing a domain event's payload, or consuming one? Read [ADR-017](docs/adr/ADR-017-domain-event-schema-evolution.md) first.**
  Bump on **consumer semantics**, not parse compatibility — the dangerous payload deserializes fine
  (§3). And a consumer must **dispatch on `schema` before decoding `data`** (§5b′): a bump exists
  *because* `data` changed, so judging a future variant with today's struct rejects it as malformed
  and drops it. Two traps the ADR names: the poison/skew line has a **bottom end** (`schema <= 0` is a
  broken envelope, not the future — terminate it, and don't let it touch readiness), and a fixture
  **built from the type under test cannot fail** — it encodes the compatibility it claims to prove.
  TKT-61 shipped the ordering bug twice, past a mutation-checked test suite and a full review pass.
- **Touching a catalog slot transition? Read [ADR-018](docs/adr/ADR-018-catalog-slot-transition-concurrency.md) first.**
  State-deriving transitions decide under a row lock and emit after commit; grouped members
  (festival days) refuse their own publish/archive. Both rules are narrower than they sound —
  the ADR draws the lines.
- **Scoping a catalog read to a subset? Read [ADR-019](docs/adr/ADR-019-catalog-read-path-scoping.md) first.**
  A scoped read is only scoped if an index backs the filter — copying a query shape that scales
  ships a no-op. Proving it takes two tests: the result is scoped, *and* the scan is. Narrower
  than it sounds — the ADR draws the lines.
- **Adding an index to a catalog migration? Read [ADR-020](docs/adr/ADR-020-catalog-index-build-concurrency.md) first.**
  Plain `CREATE INDEX` stays — `CONCURRENTLY` is still *not* adopted. [ADR-022](docs/adr/ADR-022-out-of-band-service-migrations.md)
  satisfied its precondition (1), but the preconditions are conjunctive and (2) and (3) remain false,
  so nothing changed. The ADR names the preconditions and the traps.
- **Claiming an audit or integrity guarantee? Read [ADR-021](docs/adr/ADR-021-ticket-lifecycle-trail-integrity.md) first.**
  **State inside the database cannot constrain an adversary who writes to the database** — so a
  quarantine row, a retry counter, a retained signature or a chain head proves nothing against one.
  A hash chain over data they cannot re-sign *does*, which is why modification and insertion are
  closed and rollback is not. Say which adversary you mean before writing "tamper-evident". Three
  clauses of ADR-021 broke this rule across two adversarial review passes — it is easy to get
  wrong while every individual fact is correct.
  **Shipped in TKT-67** (chain, checkpoint, `access verify-lifecycle` in the gate). That changed
  what is true, not what is claimable: modification and insertion are now evident; **targeted
  rollback and current-key compromise are still open and still TKT-11's**. A test pins the rollback
  gap by asserting it verifies clean — if it ever fails, update the ADR, don't delete the test.

- **Touching the ticket lifecycle trail — new event type, canonical form, or a claim about it?**
  Every lifecycle event goes through the append path (`store.appendLifecycle`), never straight into
  `lifecycle_events`: the verifier asserts one-to-one coverage, so a direct insert reads as tampering.
  The canonical forms are pinned by golden literals — changing a byte is a canonical-version
  migration, not a test update. `docs/development.md` §Access ticket lifecycle trail operations has
  the operator surface and the exact wording to reuse for any claim.
- **Touching how or where migrations run? Read [ADR-022](docs/adr/ADR-022-out-of-band-service-migrations.md) first.**
  Migrations run **out-of-band**: each binary's `migrate` subcommand, as a one-shot Compose job the
  service waits on (`service_completed_successfully`). The server path never migrates. ADR-008 still
  governs everything else (goose-as-library, embedded SQL, per-service ownership, fail-fast, 30s
  bound) — ADR-022 superseded it on *placement only*.
- **Editing a published seat map, or claiming a seat is pin-safe? Read [ADR-029](docs/adr/ADR-029-seat-identity-pinning-contract.md) first.**
  An edit produces a **new published version**; the old one stays immutable. A seat identity a
  sale/hold pins (`seat_map_pins`, family-scoped) is preserved across versions — an orphaning edit is
  **hard-rejected**, not silently applied. The edit-vs-sale race is closed by a **family-scoped
  advisory lock** (`pg_advisory_xact_lock` on `map_family_id`), *not* `SELECT … FOR UPDATE` on the
  current-version row: an edit makes a new version by INSERTing a row, which never conflicts with a
  row lock on the old one, so a blocked pin would recheck the **stale** row and orphan a seat. Lock
  the family identity, then re-resolve the current version. And name the adversary (ADR-021): this is
  **honest-writer consistency, not tamper-evidence** — a writer with catalog DB access can
  insert/delete pins at will. Shipped in TKT-104.
