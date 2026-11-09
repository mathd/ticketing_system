# ADR-086: Parallel CI gate parts

Date: 2026-11-09

## Status

Accepted (owner request, 2026-11-09: reduce CI time and accept more risk to get it). Amends the
single-job `check` shape that TKT-324 put in `check.yaml` and in
`scripts/check-check-workflow-trigger.sh`.

## Context

The required `check` job ran `make check` in one job. On 2026-11-09 the `make check` step took
13 min 40 s (run 42632785109). The smoke stage took about 640 s of it, all in sequence:

| Phase | Time |
|---|---|
| deps, generation, cheap checks, lint, unit tests, build | ~210 s |
| linux static build, image pulls, `compose up` | ~85 s |
| DB-backed service tests (inventory 98 s, access 41 s, catalog 33 s, commerce 17 s, payments 2 s) | ~205 s |
| e2e `smoke/` module, 182 tests, one at a time | ~330 s |

Nine e2e tests took 213 s. The four slowest wait on fixed clocks: the Grafana evaluation interval
and the 1-minute `rate()` window of the checkout alert (92 s), the dashboard export and query
windows (36 s), the timed on-sale load window (22 s), and a NATS durable test (12 s). They run
alone on purpose, so that their traffic does not enter the load proof's measured windows.

`hermetic-smoke` took 15 min 18 s (run 42632785114). About 250 s of it was the same DB-backed
service tests. They run `go test` on the host against PostgreSQL and use no image built in
Docker, which is the only thing that workflow exists to test.

The repository is public, so standard GitHub runners cost nothing. More parallel jobs make CI
faster at no cost.

Constraints:

- The main ruleset requires the context `check`. A required check that is SKIPPED counts as
  passed on GitHub.
- TKT-324's guard refused any `check` job with `needs:` or `if:`, and required a step that is
  exactly `run: make check`.
- `scripts/gate.sh` is the only entry to the stages, and the merge guard trusts only a verdict
  that starts with `PASS `.
- The security guard reads the `check-all:` line field by field, so `check-all` must keep its
  literal stage list.

## Possible Solutions

- **Do nothing.** No risk, and a 14-minute required check.
- **Parallelism inside one job** (the service test blocks at the same time, Make `-j`). It keeps
  the TKT-324 shape. The runner has 4 vCPUs, the e2e suite has a load proof that measures latency,
  and the gain is about 100 s.
- **Several required contexts**, one per part. It needs a ruleset change on GitHub, and each new
  part changes the ruleset again.
- **Parallel part jobs and one verdict job** that keeps the context `check`. The ruleset does not
  change. The guard must learn a new shape.

## Decision

CI runs the gate as five part jobs at the same time, and the required `check` job is the verdict
over them.

1. **Parts.** `gate-static` runs `make check-ci-static`: `scripts/gate.sh --part static` runs
   `make check-all` with `SMOKE_PARTS=none`, so it cannot drift from the local gate. Each
   `gate-smoke-<part>` job runs `make check-ci-smoke-<part>`: `scripts/gate.sh --part
   smoke-<part>` runs `deps` and `smoke` with `SMOKE_PARTS=<part>`, on its own stack. The parts
   are `SMOKE_ALL_PARTS` in `scripts/smoke.sh`: `services-inventory`, `services-rest`, `e2e` and
   `e2e-slow`.
2. **The e2e split is by name.** `SMOKE_SLOW_TESTS` lists the slow tests. `e2e` runs
   `go test -skip R` and `e2e-slow` runs `go test -run R`, with one anchored `R`, so the two
   parts select complementary sets. `e2e-slow` fails unless each listed name reports `--- PASS`,
   so a renamed test cannot make it pass empty.
3. **Verdict.** `check` needs exactly the five part jobs, has `if: always()`, and runs
   `scripts/gate-parts-verdict.sh '${{ toJSON(needs) }}'`. The script fails unless the needed jobs
   are exactly the expected parts and each has the result `success`.
4. **Guard.** `scripts/check-check-workflow-trigger.sh` now refuses a `check` job without
   `if: always()`, with a `needs:` that is not exactly the parts, or without the verdict step. It
   refuses a missing part job, a part job with `if:`, `needs:` or `continue-on-error`, and a part
   job that does not run its own target. It reads the part list from `SMOKE_ALL_PARTS`.
5. **Verdicts.** A part writes `PART-PASS` in `.gate.verdict`. The merge guard accepts only
   `PASS`, so only the full local `make check` unlocks an agent merge. `gate.sh` always sets
   `SMOKE_PARTS` itself, so a stray value in the caller's environment cannot make a partial run
   write `PASS`.
6. **Local gate unchanged.** `make check` runs every stage and every part on one stack, and the
   `smoke/` module once with no filter.
7. **Hermetic.** `make smoke-hermetic` runs only the `e2e` and `e2e-slow` parts.

## Consequences

- Positive: estimated critical path from about 14 min to about 6 min for `check`, and from about
  15 min to about 11 min for `hermetic-smoke`. These are estimates from the logs above, not
  measurements; the PR that adds this ADR measures them.
- Negative: CI and the local gate group the stages differently. Three differences follow:
  - The coverage gate in `TestMain` runs only when `-run` is unset. In CI it runs in the `e2e`
    part and sees only that part's operations, so an operation that only a slow test drives fails
    it. Locally it sees the whole suite.
  - The post-suite integrity checks (`verify-journal`, `verify-lifecycle` with its corruption
    seeds, `reconcile-pins`) run in the `e2e` part only. The slow tests use GA venues, so their
    stack alone has no seat map for `reconcile-pins`. In CI, the journal and lifecycle data that
    only the slow tests write is not verified.
  - A test that depended on state left by a test in the other part would pass locally and fail in
    CI. The alert test is written to work alone on a new stack (its step 2).
- Negative: five stacks start for each push, and each part pays setup and `compose up` (about
  1–2 min).
- Neutral: the guard is a tripwire, as before. It does not stop a writer who changes the Makefile
  targets, `gate.sh`, `smoke.sh` or `gate-parts-verdict.sh`.

## Implementation Notes

- To add a smoke part: add it to `SMOKE_ALL_PARTS`, guard its block with `smoke_part`, and add a
  `gate-smoke-<part>` job and a `needs:` entry in `check.yaml`. The guard refuses the workflow
  until all three agree.
- To move a test into or out of the slow group: edit `SMOKE_SLOW_TESTS` only.
- Not done here: a Go build cache for the hermetic Docker builds (about 150 s per run), and a
  matrix split of the hermetic run.

## References

- TKT-324 (the required-context guard), TKT-42 (host-built smoke), ADR-030 (smoke coverage gate)
- `.github/workflows/check.yaml`, `scripts/gate.sh`, `scripts/smoke.sh`,
  `scripts/gate-parts-verdict.sh`, `scripts/check-check-workflow-trigger.sh`
- [Continuous delivery](../conventions/continuous-delivery.md), [Testing](../testing.md)
