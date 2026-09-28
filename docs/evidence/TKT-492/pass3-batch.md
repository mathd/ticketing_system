# TKT-492 pass 3 repair batch

This batch fixes the two accepted blocking findings from review 3. It changes only files in this evidence directory. It does not change the profile, measured workload, sample counts, thresholds, or saved run evidence.

## Repairs

The baseline fixture now creates one separate pass for the correctness scan through `createBaselineFixture`. The four `pass-measured` fixtures remain owned by the measured workload. The exact measured-ticket lifecycle assertion still checks every event ID and rejects extra rows.

The runner now gives lifecycle, reconciliation, and scanner correctness assertions a typed failure with a safe category and code. It writes that record to `correctness.json` and `failure.json`. Other thrown errors keep the inconclusive classification. A timeout or missing evidence does not become a correctness failure.

The analyzer retains a runner assertion only when the same category and code appear in both saved files. It checks intact, hash-matched sample and correctness evidence for database assertion violations before strict analysis can exit on another validation error. It checks each known row even when the sample file is partial; it runs the full event-set comparison only when all planned samples exist. Analyzer-detected database violations and delivered wrong results remain correctness failures when generator validity fails or sample evidence is partial. Missing evidence, unclassified errors, and unfinished work remain inconclusive. A nonzero lifecycle verifier exit is a correctness failure only when its output matches a known integrity violation. Other command failures remain inconclusive. Latency remains inconclusive unless the generator and correctness checks pass.

Pure analyzer checks cover runner and analyzer database failures with invalid or partial runs, plus missing evidence and unclassified errors. The profile remains the workload authority.

## Checks and execution status

The first analyzer self-check exposed a class initialization order error in the new pure test. The class moved above the self-check dispatch. After that change, these checks passed:

- `node --check docs/evidence/TKT-492/run.mjs`
- `node --check docs/evidence/TKT-492/analyze.mjs`
- `node docs/evidence/TKT-492/analyze.mjs --self-check`

Source review confirmed the exact lifecycle event-set assertion still rejects extra rows. The four measured pass aliases remain unchanged and are used only by measurement. Correctness now uses its separate fixture alias. No pre-fix pure negative check was run.

No runner self-check, child-process self-check, Docker command, browser run, listener check, or live evidence run was used for this batch. The prior `EPERM` denial was not retried. Existing run directories were left intact. No baseline result is claimed.
