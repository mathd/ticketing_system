# Learnings

Index of recurring lessons. Detailed notes live in [`learnings/`](./learnings/), one file per topic.

## How it works

1. Capture a fresh learning as `learnings/YYYY-MM-DD-short-title.md`.
2. When a lesson stabilizes (seen twice, or actionable for agents), **promote it** into `AGENTS.md`, a path-specific Copilot instruction, or an OpenCode skill — so it actually changes behavior instead of sitting in a passive doc.
3. This page lists only the **top recurring lessons** worth surfacing to every contributor.

## Top recurring lessons

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
