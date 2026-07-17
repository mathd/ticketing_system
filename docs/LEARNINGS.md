# Learnings

Index of recurring lessons. Detailed notes live in [`learnings/`](./learnings/), one file per topic.

## How it works

1. Capture a fresh learning as `learnings/YYYY-MM-DD-short-title.md`.
2. When a lesson stabilizes (seen twice, or actionable for agents), **promote it** into `AGENTS.md`, a path-specific Copilot instruction, or an OpenCode skill — so it actually changes behavior instead of sitting in a passive doc.
3. This page lists only the **top recurring lessons** worth surfacing to every contributor.

## Top recurring lessons

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
- [**A passing test is not evidence it tests anything**](./learnings/2026-07-15-prove-tests-fail.md) —
  green is the default state of a test that is misaimed, drifted, or not running. Break it on purpose
  and confirm it goes red. (TKT-53, TKT-60, PR #43)
- [**A `-run` allowlist silently strands tests**](./learnings/2026-07-15-run-allowlists-strand-tests.md) —
  a test missing from the allowlist never runs and the gate still passes. Cost two merged tests that
  had never executed; the same trap had already bitten another package. (TKT-53, TKT-60)
