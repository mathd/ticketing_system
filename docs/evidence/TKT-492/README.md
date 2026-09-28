# TKT-492 scanner latency evidence

## Result

The strict analyzer reports `both_repetitions_meet_target` with exit code 0. Both valid runs completed 2,000 measured samples, with no latency at or above 500 ms. Both passed generator validity and correctness checks. This result applies to the local profile below. It does not close TKT-492: the final gate and fourth review are pending, and D11 remains open.

`profile.json` remains the unchanged workload definition and still contains its original `pending` result placeholders. The completed result is recorded in `runs/analysis.json` and `runs/analysis.md`.

See [RESULTS.md](RESULTS.md) for the run details and [TKT-489-amendment.md](TKT-489-amendment.md) for the proposed acceptance-condition replacement. The amendment is a draft for owner review. No board change was made.

## Profile and boundary

The fixed profile is `tkt492-local-scanner-v1`, SHA-256 `497f6069cd79e344bf1605d7f531cbc2a971b7241e9aab8a7414beafd6746813`. Each repetition used four independent Chrome contexts, staggered by 250 ms. Each context offered one scan per second, for four offered scans per second. Each run had 240 warm-up scans and 2,000 measured scans: 400 single-entry acceptances, 400 duplicate refusals, 400 pass acceptances, 400 pass-policy refusals, and 400 offline queued decisions.

Latency starts at the scanner click and ends when the fresh result remains in the DOM at the second animation frame. This is a rendering-opportunity proxy, not proof that pixels reached a physical display. Online timings include the scan request and rendered result. Offline timings end at the queued result; they exclude later server reconciliation. The runs checked reconciliation and admission correctness separately.

The deciding statistic is the maximum across all measured samples. The requirement is strictly less than 500 ms for every sample. A percentile alone could hide a slow decision, so percentiles are diagnostic. A run is valid only when scheduling and transport checks pass, all planned samples arrive, and correctness passes independently.

## Host and topology

Both runs used the same recorded environment: Linux x64 under WSL2, kernel `6.18.33.2-microsoft-standard-WSL2`, a 12th Gen Intel Core i5-12600K, 16 logical CPUs, and 62.8 GiB reported host memory. The browser was headless Chrome `153.0.8010.47`, Playwright `1.62.1`, and Node `v24.11.1`.

The stack used the five Compose files recorded in each run's metadata and 19 services. Access lifecycle checkpoints ran every second. The recorded retry, health-check, telemetry, and database pool settings are in each run's `metadata.json`. Docker reported no per-container CPU or memory limits. These results do not state or assume a global Docker resource limit.

The runner pins source HEAD `036f776f32656cf978b8a397ff51787b7f834de6`. A separate artifact-provenance record lists 89 checked binary and frontend distribution files. The profile hash and each run's metadata provide the run provenance. The published documentation commit cannot run the pinned runner in place: reproduction requires a separate detached checkout at that source revision, with the unchanged evidence scripts and profile copied in, plus the required Node dependencies and build artifacts. Do not change script or profile hashes to bypass the pin.

## Retained attempts and checks

The evidence inventory contains 22 controls invocations: 15 passed and 7 aborted. It contains five baseline invocations: two aborted before measurement, one complete run whose original analysis was invalid because of signed-versus-clamped lag handling, and the two strict-valid runs reported here. The earlier 77.2 ms observation came from the superseded run and is not part of this result. The two warm-up reconciliation aborts followed an asynchronous wait race in the measurement script; they do not establish an Access defect. Raw attempt files remain unchanged.

The orchestrator reports separate harness checks: four analyzer mutations caught with six clean passes; two lag mutations caught with six clean passes; two fixture mutations caught with six clean passes; and an old asynchronous browser wait mutation caught, followed by six clean helper runs. Its always-false poll timed out in 151 ms against a 150 ms deadline. These checks were run by the orchestrator and were not repeated for this report. The runner child-process self-check previously failed with `EPERM` and was not rerun. The supplemental `checkJs` attempt failed because of Node type and JavaScript typing issues; it is not a passing check.

The dated files `pass2-batch.md`, `follow-up-20260927.md`, `async-reconcile-polling-20260927.md`, and `fixture-wait-20260927.md` are historical investigation notes. Their pending-work statements describe earlier states. Use this README and [RESULTS.md](RESULTS.md) for the current evidence status.

## Reproduction

The runner requires the pinned source revision, the exact profile and harness files, the build artifacts, Node dependencies, Chrome, and Docker Compose. It creates and removes an isolated Compose project. The commands below are the reproduction sequence; they were not run while preparing this report.

```sh
node docs/evidence/TKT-492/run.mjs --mode controls
node docs/evidence/TKT-492/run.mjs --mode baseline --attempt 01
node docs/evidence/TKT-492/run.mjs --mode baseline --attempt 02
node docs/evidence/TKT-492/analyze.mjs --all-attempts
```

The controls must pass before either baseline. Run each command from the separate pinned checkout. Preserve the profile and harness hashes. The final repository gate and fourth review remain pending.
