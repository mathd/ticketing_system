# Learnings

Index of recurring lessons. Detailed notes live in [`learnings/`](./learnings/), one file per topic.

## How it works

1. Capture a fresh learning as `learnings/YYYY-MM-DD-short-title.md`.
2. When a lesson stabilizes (seen twice, or actionable for agents), **promote it** into `AGENTS.md`, a path-specific Copilot instruction, or an OpenCode skill — so it actually changes behavior instead of sitting in a passive doc.
3. This page lists only the **top recurring lessons** worth surfacing to every contributor.

## Top recurring lessons

- [**A passing test is not evidence it tests anything**](./learnings/2026-07-15-prove-tests-fail.md) —
  green is the default state of a test that is misaimed, drifted, or not running. Break it on purpose
  and confirm it goes red. (TKT-53, TKT-60, PR #43)
- [**A `-run` allowlist silently strands tests**](./learnings/2026-07-15-run-allowlists-strand-tests.md) —
  a test missing from the allowlist never runs and the gate still passes. Cost two merged tests that
  had never executed; the same trap had already bitten another package. (TKT-53, TKT-60)
