# TKT-492 pass 2 repair batch

This batch addresses the four blocking findings in `review2-result.md` and the online body-prefix defect recorded in `verification.md`. The changes stay inside this evidence directory. The fixed profile, six control names, sample counts, thresholds, and workload remain unchanged.

## Repairs

1. The observer stores completed scans in `state.records`, the array used by the waits and readers. `normalizeRecord` now reads `body_prefix` from the online and offline profile records. It no longer reads the absent `prefix` field.
2. The observer records the result section class. The runner and analyzer check class, heading, and body prefix for measured online and offline scans, warm-ups, correctness sequences, and the relevant controls. The existing `correct_wire_wrong_render` control now has two subcases. One changes the heading while preserving class and body. The other changes the class while preserving heading and body.
3. Reconciliation waits for the current response's exact occurrence IDs and each matching IndexedDB row's expected `SYNCED` or `RESOLVED` state. The wait uses the fixed 60-second reconciliation deadline. It does not use a previous sync note. The measured reconciliation order remains B then A, and each response, local row, lifecycle row, conflict, and timestamp is checked against the queued occurrence.
4. The analyzer reports generator validity, delivered correctness, and latency separately. It retains partial samples and writes a classified diagnostic report when full evidence checks fail. A delivered wrong result stays a correctness failure even when the generator also failed. A complete, correct run over the fixed latency threshold is a latency miss. The pure analyzer self-check covers healthy fast, healthy slow, invalid scheduling, unknown transport, and a delivered wrong result with an invalid generator.

## Interaction review

The observer array correction lets the click, POST, IndexedDB, and fresh-render correlation complete. That makes the next prefix access reachable, so the profile field mismatch is repaired in the same path. Render class capture happens at the existing two-frame boundary, alongside heading and body capture. The runner applies these checks before it accepts samples; the analyzer independently rechecks the saved values. Reconciliation uses the captured response index and the exact pre-request occurrence set, then waits for local state to match that response. The fixed A/B commit order and microsecond timestamp checks remain in place.

Partial evidence no longer disappears behind a thrown analyzer error. The analyzer keeps rows it can read, records a validation diagnostic, and reports the three classifications. It marks an attempt as meeting the target only when the complete strict analysis passes. No measured latency conclusion follows from an aborted or incomplete attempt.

## Checks and execution status

Passed locally:

- `node --check docs/evidence/TKT-492/run.mjs`
- `node --check docs/evidence/TKT-492/analyze.mjs`
- `node docs/evidence/TKT-492/analyze.mjs --self-check`
- JSON parsing for `profile.json` and `artifact-provenance.json`

The runner child-process self-check was not run in this repair pass. A prior attempt was denied with `EPERM`; the verification note records that the orchestrator had already run the host check before learning of the denial. No alternate child-process check was attempted here. The live controls and six short positive sequences are for the orchestrator to run. This report makes no claim that they passed.

Four earlier live controls attempts remain under `runs/controls/`. Each aborted before scans, and each has an empty `samples.jsonl`. Their phases are `unexpected_tracked_source_changes`, `setup_or_run_failed`, `access_policy_projection_timeout`, and `setup_or_run_failed`. Those attempts are not controls evidence.

The copied artifact proof records the root gate PASS for source HEAD `036f776f32656cf978b8a397ff51787b7f834de6` and tree digest `7730ce44aeca5f3567d51351c21ed203246b3beb`. It covers the 89 listed binary and frontend distribution files. The gate for the final artifact commit remains pending. The provenance does not cover this repair batch's evidence scripts or profile.

The pass 2 source repair batch is complete. Live controls, both baselines, their analysis, and the final artifact commit gate remain pending. No latency result is claimed.
