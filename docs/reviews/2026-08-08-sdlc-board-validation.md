# SDLC board validation — 2026-08-08

**Method.** Ticket state read from the `sdlc-state` branch worktree
(`.sdlc/tickets/*.json`, 221 files), reconciled against `git log main` and the file tree on
`main`.

**Status: reconciled.** The drift below was found first, then corrected on the board via
`POST /ticket` (one commit per transition on `sdlc-state`, so the metrics timeline stays intact).
Board went 221 → 222 tickets; Done 144 → **153**; Backlog 75 → 67; nothing in flight, two `Ready`.
See [What was changed](#what-was-changed-on-the-board) at the end.

**TKT-224 and TKT-228 were accepted to Done by the owner on 2026-08-08.** Neither needed merging —
both were already on `main`, committed directly with no PR, which is *why* the board never recorded
them (the state write is coupled to Gate 3, and Gate 3 was bypassed). Both closeouts deliberately
carry **no stage-duration table**: their runs were never recorded while they happened, so no honest
per-stage timings exist. Both are marked **excluded from cycle-time analysis** rather than given
invented numbers.

> **A red gate was found during this pass and is not in the original validation below.**
> `make check` **fails on `main`** at `ab9920b` — reproduced twice, with a *different* set of ~30
> commerce store tests each time. `internal/store` and `internal/bulkrefund` both call
> `store.Migrate` against the same `COMMERCE_TEST_DATABASE_URL`, and `go test ./internal/...` runs
> packages concurrently. Full diagnosis is on **TKT-198**, now raised to `Ready`.

## Headline

| | |
|---|---|
| Tickets on board | 221 |
| Done | 144 |
| Ready | 2 (TKT-224, TKT-225) |
| Backlog | 75 |
| In flight (Planning / Building / PO Review) | **0** |

The board is **stale by two tickets**. Its last write was 2026-08-06 22:03; `main` has two
substantive commits after that which the board never recorded.

## Drift found

### 1. TKT-224 — shipped on main, still `Ready` on the board

`ab9920b` (2026-08-07) implements it: `shared/go/ratelimit/`, `services/commerce/internal/api/ratelimit.go`,
373 lines of handler tests, `ADR-051-public-customer-surface-rate-limiting.md`, a
`test/browser/rate-limit.mjs` browser-submit spec, and storefront form wiring. The ticket never
left `Ready` — no Planning/Building/PO Review transitions were ever committed to the state branch.

This is the only case where the board *understates* completed work on a ticket it tracks. The
ticket's own ACs (limiter placement stated, forms covered, browser spec) appear satisfied by the
commit contents, but **PO acceptance was never recorded**, so "Done" is not mine to assert.

### 2. TKT-228 — shipped on main, has no ticket file at all

`a9b81de` (2026-08-06) promoted the browser-submit gate out of per-ticket throwaways: `make browser`,
`scripts/browser.sh`, `scripts/stack-env.sh`, `test/browser/reset-password.mjs`, plus an AGENTS.md
working-agreement change. There is **no `.sdlc/tickets/TKT-228.json`** — the key was used in a
commit without ever being created on the board.

Note this partly overlaps **TKT-196** ("Browser-submit harness: make the manual web-UI gate
runnable"), which is still sitting in Backlog. TKT-228 looks like it delivered TKT-196's substance
under a different key.

### 3. TKT-2 — every child Done, epic still `Backlog`

19 of 19 children Done. Compare TKT-5, TKT-6, TKT-35 (all-children-done epics correctly closed).
TKT-4 is a similar case: decomposed in `2d8b25d`, never advanced.

### 4. Board noise — six tickets marked `[MERGED into …]` still occupying Backlog

TKT-111, TKT-118, TKT-120, TKT-133, TKT-135, TKT-137. Their summaries declare them merged
elsewhere; they inflate the Backlog count by 6. TKT-69 is marked `[DUPLICATE]` but was correctly
closed to Done.

### Not drift (checked and explained)

Thirteen Done tickets are never named in a `main` commit subject. All are explainable: epics closed
on child completion (TKT-1/5/6/9/31), tickets whose work merged via PR number rather than a keyed
commit (TKT-28/29/44/45/113), a ticket split into successors (TKT-175), one closed inside another
ticket's branch (TKT-219, closed inside TKT-217), and a duplicate (TKT-69). No missing work here.

## What is done

The last completed arc is **TKT-21, Customer accounts & wallet** (Done): register/sign-in/sign-out
(TKT-220), purchase-to-account binding (TKT-221), the wallet (TKT-222), guest-order claim (TKT-223),
and a real mail path with password recovery (TKT-226). Before it, **TKT-6 / Service fees & revenue
splits** closed with the settlement ledger (TKT-216, TKT-217, TKT-219).

Closed epics: TKT-1 (M1 skeleton), TKT-5 (pricing), TKT-6 (fees & splits), TKT-9 (orders — closed
with 6 children still open), TKT-21 (customer accounts), TKT-22 (back office & RBAC),
TKT-31 (read-path caching), TKT-35 (interactive seat selection).

## What is not

Nothing is in flight. The Backlog's 75 items are, minus the 6 merged markers, roughly:

- **25 undecomposed epics** — seat maps (TKT-3), promotions (TKT-7), cart/packages (TKT-8),
  payments/multi-currency (TKT-10), passes (TKT-12), festivals (TKT-14), cashless (TKT-15),
  box office (TKT-16), reseller API (TKT-17), delivery (TKT-18), access control (TKT-19),
  waiting room (TKT-20), reporting (TKT-23), taxes (TKT-32), GDPR (TKT-33), invoicing (TKT-34),
  i18n (TKT-36), observability (TKT-37), waitlists (TKT-38), SEO/agent access (TKT-40),
  NF525 (TKT-11).
- **Follow-ups filed by shipped tickets** — the largest cluster. TKT-9's six open children
  (transfers TKT-160, refund-reversal retries TKT-163, interrupted exchange TKT-167,
  comped-order cancellation TKT-171, …), TKT-31's three (TKT-209, TKT-211, TKT-212),
  TKT-22's three (TKT-195, TKT-201, TKT-203).
- **Four known-flaky or red tests** — TKT-149 (`TestOnsaleLoadProof`), TKT-198
  (`TestRunnerDrainsARealBook`), TKT-213 (`TestStatusReplayWindowExpiryAnswers409`, **fails in
  hermetic smoke on main**), TKT-218 (storefront call-budget, **one fails OPEN**).
- **One broken developer path** — TKT-227: `make up` fails on the in-Docker scanner build
  (typescript@7.0.2). Related to the standing `ts7-blocked-by-openapi-typescript` constraint.

### Security follow-ups worth naming

TKT-195 (back-office login rate limiting) has been open since TKT-190 and is the sibling TKT-224's
own shaping notes flagged: "two half-solutions would be the bad outcome." TKT-224 shipped the
customer half; the staff half is still Backlog. Also open: TKT-199 (catalog publish/archive accept
a slot id with no organizer — cross-tenant by construction), TKT-202 (gateway logs `guest_order_ref`,
which ADR-012 says is never logged), TKT-155 (price-resolution provenance publicly readable).

## Recommended next actions

**Reconcile the board first** (cheap, and the board is the experiment's instrument):

1. Advance **TKT-224** to PO Review or Done — the code is on main and unrecorded.
2. Create **TKT-228** retroactively, and decide whether **TKT-196** is closed by it.
3. Close **TKT-2**; decide **TKT-4**.
4. Close the six `[MERGED into …]` tickets so Backlog reflects real work.

**Then pick up work.** Two candidates, in order:

- **TKT-213 and TKT-218** — a test that fails on main and a budget test that *fails open* are
  corrosive to every subsequent ticket's gate. TKT-218's fail-open is the more dangerous of the two:
  it reports green while proving nothing.
- **TKT-195** — closes the rate-limiting story TKT-224 half-finished, while the design context
  (ADR-051, `shared/go/ratelimit/`) is fresh and reusable.

**TKT-225** is the only other shaped, Ready ticket (readiness all `met` except a deferred
uncertainties entry), so it is the lowest-friction feature pickup if you would rather ship than
repair.

---

## What was changed on the board

All mutations went through `POST /ticket` so each is its own commit on `sdlc-state` — the board
derives its stage metrics from that log, so batch-editing the JSON would have destroyed the history
it reports. 22 commits.

**Drift corrected**

| Ticket | Change | Note |
|---|---|---|
| TKT-224 | `Ready` → **Done** | Walked through Planning/Building with stage comments recovered from `ab9920b`, not invented. Owner granted Gate 4 on 2026-08-08 |
| TKT-228 | **created** → **Done** | The key was used in a commit with no ticket file. Reconstructed in its shipped state, then accepted |
| TKT-196 | disposition comment | TKT-228 delivered its substance; checked against its own asks, not by title. **Left in Backlog** — closing another's ticket is the filer's call. Residue noted: still no committed spec for the three TKT-190 staff-session behaviours |
| TKT-2 | `Backlog` → **Done** | 19/19 children Done; `kind=metrics` closeout posted |
| TKT-4 | parent links repaired, **stays Backlog** | Its 8 children (TKT-75…82) carried no `parent`, so the epic rolled up as 0/0 and looked untouched. Real state is 8/9 — **TKT-81 is still open**, so it does not close |
| TKT-111, 118, 120, 133, 135, 137 | `Backlog` → **Done** | Each `[MERGED into X]`; every target verified Done first |

**Tickets improved (shaped so Gate 1 stops hard-blocking them)**

These were well-described but had no `readiness` object, which the board requires before a human can
prioritize. Shaping makes them *eligible*, not prioritized — they stay in Backlog.

- **TKT-198** — reproduction, mechanism, and the racing helpers named; the one exception, raised to
  **Ready** because the gate is red now. Summary retitled from the narrow
  `TestRunnerDrainsARealBook` symptom to the actual fault.
- **TKT-218** — the call-budget proof that fails **open**. Flagged that two `open` readiness items
  mean it may be better re-typed as a **Spike**: its product is an answer, not a diff.
- **TKT-213**, **TKT-149** — shaped, with the `AGENTS.md` traps named for each (ask what the fixture
  can distinguish; a bounded replacement is a new implementation).
- **TKT-195** — shaped and cross-linked to TKT-224, which deliberately built its mechanism
  (`shared/go/ratelimit`) as a shared package. Four design decisions are already paid for; the one
  open question is **placement**, because TKT-224's "the API is reachable directly through the
  gateway" argument may not transfer to a surface reachable only through the SSR form.

**Not changed, deliberately**

- No source code was touched. The red gate is diagnosed on TKT-198, not fixed.
- **Verification claims are attributed, not adopted.** TKT-224's closeout records `make check` /
  `make browser` green as *the commit's own account*, because neither could be re-run today —
  `make check` is red on `main` for the unrelated TKT-198 race, and `make browser` was not re-run.
  Both tickets were accepted on the artifacts present on `main`, and the closeouts say so.

### One more integrity gap, left alone

Seven Done non-Spike tickets carry **no `kind=metrics` closeout**, which the board's Done gate is
supposed to require: TKT-28, TKT-35, TKT-59, TKT-175, TKT-182, TKT-183, TKT-219. All pre-date this
pass.

Three of them — **TKT-175, TKT-182, TKT-219** — went `Backlog → Done` in a single transition,
skipping the pipeline entirely. That is the signature of a human dragging the card on the board
rather than the agent closing it out. The drag path enforces the metrics rule in `board.html`
(`line 546`), so these were most likely written directly to the state branch, or dragged before that
check existed.

I did not backfill them. A closeout is a record of what happened during a run; writing one for a run
I did not observe would put invented stage durations into the metrics timeline, which is exactly the
data this board exists to produce. The honest options are to leave them, or to have whoever ran them
close them out from memory.

**Worth noting for the experiment:** the gap is mechanically detectable, and so were both drift
classes found today (a Done epic with open children; a ticket key in a commit with no ticket file).
A validation step in the `sdlc-ticket` skill could catch all three without a human noticing first —
which is the retro item recorded on TKT-2.
