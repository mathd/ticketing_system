# On-sale load proof (TKT-82 / US-019)

The sustained no-oversell gate: methodology and the published per-pool throughput ceiling.
Harness: `smoke/onsale_load_test.go` + `smoke/internal/loadtest`; profiles and assertions:
`docs/testing.md` §Concurrency proofs.

## What TKT-20 consumes

> **Per-pool ceiling (2026-07-17, owner workstation / local compose):**
> highest stable **300 completed checkout attempts/s** (= 18,000/min, **900 pool
> mutations/s**) for one hot pool under the hold→finalize→confirm mix; first unstable
> offered rate **600 attempts/s** (32% dropped arrivals, lifecycle p99 3.7s). The NFR window
> (3,000 attempts/min × 3 min) ran clean: 9,000/9,000 delivered, lifecycle p99 9ms.
> `claim_history` INSERT inside the pool-locked transaction: **0.044 ms/mutation mean**
> (min 0.020, max 0.839, stddev 0.017; 27,000 calls, 1,175 ms total) — pg_stat_statements
> during the NFR window. Raw run: `docs/evidence/TKT-82/full-profile.json`.

The waiting-room design (TKT-20) chooses its own safety margin against this bracket — this
evidence deliberately bakes in no policy.

## Reading the number honestly

- The ceiling is evidence for **this compose topology** (single shared PostgreSQL,
  `DB_MAX_OPEN_CONNS=25`, client and server on one host), not a universal production number.
  Every run's host metadata is in the evidence JSON; rebaseline when a production-like
  environment exists.
- ADR-010 **deliberately serializes a hot pool** — a low ceiling is a finding for TKT-20's
  design, not a bug in inventory.
- Scheduler lag and the client in-flight cap are reported so client CPU exhaustion is not
  mistaken for the pool ceiling; a sweep stage is only "stable" with zero drops, errors and
  rejections, ≥99% delivery, **and** lifecycle p99 within the 3s SLO — a stage that answers
  slowly forever is past the knee even before the client cap drops arrivals.
- The `claim_history` INSERT line (ADR-023 amendment) is aggregate DB execution time from
  `pg_stat_statements` — overhead attribution, not a causal with/without comparison.

## Reproducing

```sh
make onsale-load-full   # full NFR profile, ~10–15 min, writes docs/evidence/TKT-82/full-profile.json
make check              # includes the scaled gate profile (correctness-fatal only)
```
