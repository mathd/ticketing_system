# TKT-492 results

## Decision

Both strict-valid repetitions meet the fixed profile. The combined analyzer result is `both_repetitions_meet_target`, and the analyzer exited 0. Each run had 2,000 measured samples, zero samples at or above 500 ms, valid scheduling and transport, and passing correctness checks.

| Run | Samples | p50 | p95 | p99 | Maximum | Scheduler lag p99 / max |
|---|---:|---:|---:|---:|---:|---:|
| [Baseline 01](runs/baseline-01/run-20260927T234917112Z-9db69da7/) | 2,000 | 40.0 ms | 53.2 ms | 59.6 ms | 74.3 ms | 0.400 / 6.1 ms |
| [Baseline 02](runs/baseline-02/run-20260928T000044752Z-f83a75d0/) | 2,000 | 41.7 ms | 58.0 ms | 74.1 ms | 253.2 ms | 2.400 / 11.9 ms |

The maximum decides the 500 ms target. Both maxima are below it. Percentiles describe the distribution; they do not decide acceptance.

## Results by class

The following figures combine the two strict-valid runs. Each class has 800 online samples or 400 offline samples. The analyzer's quantile method produced the percentiles. The 500 ms test applies to every sample, including each class maximum.

### Accepted online

| Class | Samples | p50 | p95 | p99 | Maximum |
|---|---:|---:|---:|---:|---:|
| Single-entry acceptance | 800 | 43.6 ms | 57.2 ms | 69.0 ms | 109.8 ms |
| Pass acceptance | 800 | 44.3 ms | 57.2 ms | 69.3 ms | 122.6 ms |

### Refused online

| Class | Samples | p50 | p95 | p99 | Maximum |
|---|---:|---:|---:|---:|---:|
| Duplicate refusal, `already_redeemed` | 800 | 40.8 ms | 53.3 ms | 63.3 ms | 126.7 ms |
| Pass-policy refusal, `exit_required` | 800 | 41.1 ms | 53.3 ms | 60.9 ms | 253.2 ms |

### Offline queued

| Class | Samples | p50 | p95 | p99 | Maximum |
|---|---:|---:|---:|---:|---:|
| Single-entry queue | 400 | 37.8 ms | 57.8 ms | 68.8 ms | 79.7 ms |
| Pass queue | 400 | 36.4 ms | 60.3 ms | 71.9 ms | 125.6 ms |

Each run included exactly 400 samples in each of the four online classes and 400 offline queued samples. Neither run recorded a missed arrival, overlapping scan, unknown transport failure, or latency violation.

## Correctness evidence

Both `correctness.json` files report `passed`, six passing correctness sequences, no failures, a passing lifecycle verifier, and a passing four-context single-entry race. Each race observed one accepted scan and three `already_redeemed` refusals while all four request intervals overlapped.

Each run also checked the persisted admission and reconciliation state. The assertions found 800 accepted online decisions, 800 online refusals, 350 recorded offline decisions, 50 offline conflicts, no pass-policy conflicts, and 50 admission conflict alarms. The six sequences covered single acceptance, duplicate refusal, multi-entry pass admission and exit policy, offline queueing, and offline reconciliation. These checks establish correctness separately from the latency classification.

## Environment and limits

The exact profile SHA-256 is `497f6069cd79e344bf1605d7f531cbc2a971b7241e9aab8a7414beafd6746813`. Both runs used source HEAD `036f776f32656cf978b8a397ff51787b7f834de6`, headless Chrome `153.0.8010.47`, Playwright `1.62.1`, Node `v24.11.1`, and Linux x64 under WSL2 on a 12th Gen Intel Core i5-12600K host with 16 logical CPUs and 62.8 GiB reported memory. Docker reported zero per-container CPU and memory limits. The 19-service stack used `compose.yaml`, `compose.direct-ports.yaml`, `compose.onsale-load.yaml`, `compose.smoke-cadence.yaml`, and `compose.smoke.yaml`. The metadata records Access lifecycle checkpoints at 1 s, event retry delays of 100, 200, 400, 800, 1,000, and 1,000 ms, commerce recovery and refund reversal at 2 s, health checks and telemetry export at 5 s, and database pools of 25 open and 10 idle connections with 30 min maximum lifetime and 5 min maximum idle time. Service image IDs and references are retained in each run's `metadata.json`.

Four contexts offered four scans per second, staggered by 250 ms, with one request at a time per context. Each run used 240 warm-up scans followed by 2,000 measured scans. The scanner timing starts at click and ends after the fresh result remains in the DOM at a second animation frame. That point is a rendering-opportunity proxy; it does not prove physical display time. Offline timing ends at the queued result and excludes later reconciliation latency. Reconciliation, admission policy, and lifecycle checks ran outside that latency interval.

This result is local to the recorded host, Compose topology, browser, workload, and measurement boundary. It does not establish a general latency guarantee. It does not show that production optimization is needed. No production implementation was changed for this evidence report.

## Status

The retained evidence inventory has 22 controls attempts: 15 passed and 7 aborted. Five baseline invocations remain: two aborted before measurement, one complete attempt with an original analysis invalidated by signed-versus-clamped lag handling, and the two valid repetitions above. The superseded 77.2 ms observation is not part of these results. The two warm-up reconciliation aborts were measurement-script asynchronous wait races; they do not prove an Access defect. All retained raw files remain unchanged.

The analyzer run is complete. The final repository gate and fourth review remain pending. D11 remains open. TKT-489 has not been amended, and the proposed text is in [TKT-489-amendment.md](TKT-489-amendment.md). No completion, PR, merge, push, or board change is claimed.
