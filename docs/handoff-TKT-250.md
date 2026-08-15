# Handoff — TKT-250 (stale allocation saves), written 2026-08-15

**Written at `origin/main` = `88184beb`.** Everything below was true then. Re-verify before acting —
the tracker and the git log are the authority, not this file.

## Start here

1. `python3 .sdlc/server.py` (board at http://localhost:8787/board.html) — the local tracker holds
   ticket state on the `sdlc-state` branch.
2. Read **TKT-250** on the board, and its parent **TKT-17**.
3. TKT-250 is in **Backlog with `readiness: null` and no context-mémo**. It needs
   **shaping before Gate 1** — do not enter at Ready. (`sdlc-ticket` → `references/shaping.md`.)
4. Read the closing comments on **TKT-244** — `kind=metrics` and the `overrides:` list. TKT-250
   exists because of an override recorded there.

## What just shipped (verify, don't assume)

- **TKT-244** — PR #221, merged as `720bfc58`. Inventory got its own staff-write credential
  (**ADR-057**) and the back office got the slot channel-allocation editor.
- **`88184beb`** — retro items: a new learning note, AGENTS.md rules, and two edits to the
  `sdlc-ticket` skill/reference.
- Before that, **TKT-246** (`c28697d1`) made `channel_allocations.sold_by` load-bearing: inventory
  refuses a claim whose seller does not match, judged **under the pool row lock**.

No non-dependabot PRs were open at the time of writing.

## The work: TKT-250

**One sentence:** `PUT /internal/slots/{id}/channel-allocations` replaces a slot's whole allocation
set with **no version or prior-state precondition**, so a save built from a stale read silently
deletes rows added since and overwrites `sold_by`, `requires_code` and the sales window.

**Why it matters more than it used to.** ADR-024 §Consequences already accepted this:

> Full-set PUT has no stale-write protection (`If-Match`); acceptable while allocation editing is
> single-operator.

TKT-244 falsified the premise by shipping the first UI. Allocation editing is now a screen any admin
can open, so two operators on one slot is ordinary rather than hypothetical. And overwriting
`sold_by` is an **authorization regression**, not a lost edit: a reseller's bound stock silently
becomes public.

**The pool lock does not help.** It serializes the DELETE/re-INSERT; it cannot detect that the
*form* was populated before another writer committed.

### The decision the plan gate should settle

**Is the revision REQUIRED, or optional-but-honoured?** Required breaks every existing caller —
**five smoke drivers** and every service-to-service path use this route with `X-Internal-Token`
(`smoke/inventory_contention_test.go`, `partner_reserve_test.go`, `partner_api_test.go`). Decide it
deliberately and record it in the ADR; do not let it get settled while writing a handler.

### Conditions of success (as filed — re-read the ticket, it may have moved)

- The allocation set carries a **revision** the caller presents to replace it, compared **under the
  pool lock** — that is where the decision already happens, and a check in a calling service binds
  only callers who go through it.
- A mismatch refuses with a **coded 409** in the shape TKT-244 established
  (`allocation_caps_exceed_capacity`, `allocation_cap_below_consumption`), so the back office can
  say "your view is stale, reload" instead of reporting a generic failure.
- `X-Internal-Token` callers keep working — see the decision above.
- **A two-reader test**: A and B both read; B changes `sold_by` or adds a row; A submits its stale
  set and is refused. **Assert against the database, not the response** — the regression is what
  survives the write.
- **ADR-024 amended** to record that its single-operator premise no longer holds, and what replaced
  it.

## Where the code is

| What | Where |
|---|---|
| The endpoint | `services/inventory/internal/api/operational.go` → `replaceAllocations` |
| The store write (pool lock, DELETE + re-INSERT) | `services/inventory/internal/store/channel_allocations.go` → `ReplaceChannelAllocations` |
| The two coded refusals + their sentinels | `services/inventory/internal/store/store.go`, mapped in `api/server.go` → `problem()` |
| The contract (closed `Error` enum — a new code is a contract edit) | `services/inventory/api/openapi.yaml` |
| The back-office write path | `web/backoffice/src/lib/allocation-form.ts` → `toAllocationRequest` |
| The page | `web/backoffice/src/pages/slots/[id].astro` |
| Browser proof | `test/browser/slot-allocations.mjs` |

## Traps that cost real time on TKT-244

- **`make check` is not enough for a write form.** `make browser` is a separate target, needs the
  host's real Chrome, and CI cannot run it. Run it from the **repo root** — a mis-cwd'd `make`
  reports `No rule to make target`, which reads like a missing target.
- **Judge a background gate by the sentinel and the log body, never the harness status.** It
  reported "exit code 0" for a run whose sentinel said `EXIT=2`. Write
  `make check > log 2>&1; echo EXIT=$? > done`, wait on `done`, then grep the log for
  `Error [0-9]+|FAIL|drifted`.
- **A negative auth probe must be otherwise VALID.** The OpenAPI request validator runs *before* the
  handler, so a probe missing a required header (e.g. `Idempotency-Key`) gets 400 and the credential
  check never runs. A 401 assertion that only ever sees 400 cannot fail for the reason it claims —
  this bit twice.
- **Inventory refuses its internal surface with 401**, not commerce's 404. Don't copy commerce's
  expectation.
- **`govulncheck` fails repo-wide** on the pinned `go 1.26.5` stdlib — pre-existing on `main` since
  at least 2026-07-27, unrelated to any diff. The required checks are `check` and `hermetic-smoke`.
- Commit regenerated code **before** running the gate; `check-generate` diffs against HEAD.

## Open, and the user's to decide

1. **Whether TKT-250 should have blocked TKT-244 at all.** The ai-review said *do not ship*; the
   previous session shipped anyway, on the grounds that the gap is pre-existing and ADR-024 accepted
   it. Recorded in TKT-244's `overrides:`. If the user's answer is "should have blocked", the move is
   revert-and-sequence, not TKT-250 on top.
   **The previous session's recommendation was: do not revert** — before TKT-244 the same race
   existed, the operator needed a credential that opens *every* inventory operation, and TKT-246's
   deploy prerequisite was unmet. Safety ranking: ship + TKT-250 > ship > revert. **That is a
   recommendation from the same agent that took the override; weigh it accordingly.**
2. **The ADR-055 numbering collision** — two files claim it
   (`ADR-055-on-sale-write-rate-limiting.md`, `ADR-055-presale-unlock-codes.md`). Left alone because
   renumbering breaks citations. Not TKT-250's.
3. **A cheap interim if TKT-250 is not taken now:** amend ADR-024 to say its single-operator premise
   is now false and TKT-250 owns it. Worth doing regardless.

## After TKT-250

**TKT-245** — catalog cannot verify *which* organizer a request is for. Both **ADR-053** and
**ADR-057** name it as the thing that would let them claim something stronger; it is the larger
piece and the one most worth doing next. Other TKT-17 children still open: TKT-241, TKT-243,
TKT-248.
