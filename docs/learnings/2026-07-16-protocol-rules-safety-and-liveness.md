# An anti-replay rule must be re-checked against the retry it exists to serve

**Ticket:** TKT-73 (PR #55) — decision ADR for the admission event model.

## What happened

Three consecutive review rounds (two codex, one human) each found a real hole in the *same*
protocol rule, and each fix opened the next hole:

1. **R1 (codex):** the redeemed event kept a deterministic id while replay detection keyed on
   event ids — a transport retry of the occurrence that became `redeemed` would be *forged into a
   `duplicate_admit`*, i.e. into evidence of a second physical admission.
2. **Fix → R4 (codex):** making the retry idempotently return the original `accepted` turned a
   captured `(qr_payload, occurrence_id)` pair into a **replay token** — an admission oracle.
3. **Fix → Gate-3 (human):** "replays never actuate" then contradicted "a lost-response retry
   completes exactly once": if the acceptance response is lost, the retry only ever sees a replay
   result and the gate never opens.

The stable resolution needed *both properties stated together*: actuation keyed on the
originating device's durable pending record (safety — copied ids never actuate) **and** the
pending record letting a replay complete on the device that owns it (liveness — the lost-response
retry admits exactly once), with the crash window named and forced fail-closed
(mark-actuated-before-open).

## The lesson

A protocol-shaped rule has a safety side (what must never happen) and a liveness side (what must
still happen). Reviews and fixes naturally fixate on one at a time, and each one-sided fix is
locally correct — the contradiction only appears when you re-run the *motivating scenario* of the
opposite side. When writing or fixing an idempotency/anti-replay/dedup rule:

- After closing the abuse case, re-run the legitimate retry it exists to serve, step by step.
- After enabling the retry, re-run the abuse case from a device/session that shouldn't succeed.
- Name the crash window explicitly and pick a failure direction (fail-closed vs fail-open) on
  purpose.

Related: [judge replays by lifecycle state](./2026-07-16-judge-replays-by-lifecycle-state.md) —
same family (replay semantics), different failure axis.

## Two smaller lessons from the same ticket

- **Resolve governing ADRs by grepping the registry for the ticket's domain nouns.** Shaping
  associated by topic (integrity, migrations) and missed ADR-005's accepted `entry`/`exit`
  amendment — the exact vocabulary the ticket was deciding. The fresh-context plan drafter found
  it, and it collapsed half the decision space (option A would have reversed an accepted ADR).
- **Ticket premises rot between shaping and claim.** TKT-67 shipped in the interval and inverted
  the ticket's "decide before TKT-67 or pay re-backfill" sequencing argument. Re-verify a
  ticket's sequencing/dependency claims against merged work at claim time, not just at shaping.
