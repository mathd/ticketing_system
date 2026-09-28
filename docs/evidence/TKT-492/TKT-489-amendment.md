# Proposed TKT-489 description amendment

This is a draft replacement for the description and conditions of success in the current TKT-489 note. It has not been posted to the board. Keep the story in Backlog and retain its existing objective, five points, parent epic, and quality-gate requirement.

## Proposed full description

> A gate operator receives accepted and refused scan results within the epic's local 500 ms target while admissions remain correct.
>
> Points: 5. Parent epic: TKT-19.
>
> ## Conditions of success
>
> 1. A repeatable local browser load run uses the fixed profile `tkt492-local-scanner-v1` in `docs/evidence/TKT-492/profile.json` (SHA256 `497f6069cd79e344bf1605d7f531cbc2a971b7241e9aab8a7414beafd6746813`). It covers online single-entry acceptance, duplicate refusal, pass acceptance, pass-policy refusal, and intentional offline queued decisions. It records the expected wire decision, refusal reason, and scanner-visible result for each class. Each repetition includes four independent contexts, four offered scans per second, 240 warm-ups, and 2,000 measured samples.
>
> 2. Run two independent repetitions. Measure from capture in the click handler to the fresh scanner-visible result at the second animation frame. The second animation frame is a proxy for a rendering opportunity; this interval excludes camera, RFID, and typing time. Every measured sample must complete in strictly less than 500 ms. The maximum across all measured samples decides the target; report p50, p95, and p99 as diagnostic values. Report the host, browser, workload, sample count, profile hash, and evidence links. Generator validity and correctness must pass independently. A generator failure, missed arrival, overlap, unknown transport result, or incomplete sample set makes latency inconclusive, not a server pass or fail. A wrong delivered decision or failed correctness assertion is a correctness failure.
>
> 3. Keep admission correctness on its deciding paths. A concurrent four-context single-entry scan must produce one authoritative acceptance and three duplicate refusals while all request intervals overlap. Preserve pass entry and exit policy. Check offline queue and reconciliation results, including exact conflict and alarm evidence, separately from scan latency. Verify lifecycle append and integrity after the runs. Offline timing ends at the queued scanner result; later reconciliation latency is outside the 500 ms measurement interval and remains subject to the correctness checks.
>
> Run `make check` before completion. For web write forms, add a submitting spec in `test/browser/` and run `make browser`.

## Proposed readiness assessment

The evidence resolves the profile uncertainty and supports the revised acceptance wording. It does not close TKT-492 or change TKT-489's status.

| Readiness field | Proposed state | Basis |
|---|---|---|
| Objective | Met | Keep the current objective text unchanged. |
| Scope | Met | The draft names the measured browser boundary and excludes only later reconciliation time from latency. |
| First slice | Met | The two valid repetitions cover all five decision classes and record result and reason. |
| Context memo | Met | Keep the existing governing decisions and tests. |
| Success | Met | The draft fixes repetitions, maximum statistic, validity rules, and correctness checks. |
| Uncertainties | Met | The profile hash, host, browser, topology, workload, and boundary are recorded. |
| Approach | Met for this profile | The observed maximum is 253.2 ms. This profile alone gives no reason to optimize production code. |
| Dependencies | Open | TKT-492 still has D11 open, with its final gate and fourth review pending. Keep the existing blocked-by link until the owner resolves that work. |

This assessment is proposed text, not a board readiness update. The current board description, readiness fields, and Backlog state remain unchanged. The measured profile does not prove that no optimization is needed for other hosts, workloads, or requirements.
