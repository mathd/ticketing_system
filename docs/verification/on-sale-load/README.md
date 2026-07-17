# On-sale load proof (TKT-82 / US-019)

The sustained no-oversell gate: methodology and the published per-pool throughput ceiling.
Harness: `smoke/onsale_load_test.go` + `smoke/internal/loadtest`; profiles and assertions:
`docs/testing.md` §Concurrency proofs.

## What TKT-20 consumes

> **Per-pool ceiling: pending first accepted `make onsale-load-full` run.**
> Publish here: highest stable completed checkout attempts/s for one hot pool under the
> hold→finalize→confirm mix, its corresponding pool mutations/s (3× checkouts/s), and the
> first unstable offered rate. Raw run: `docs/evidence/TKT-82/full-profile.json`.

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
  mistaken for the pool ceiling; a stage is only "stable" with zero drops/errors and ≥99%
  delivery.
- The `claim_history` INSERT line (ADR-023 amendment) is aggregate DB execution time from
  `pg_stat_statements` — overhead attribution, not a causal with/without comparison.

## Reproducing

```sh
make onsale-load-full   # full NFR profile, ~10–15 min, writes docs/evidence/TKT-82/full-profile.json
make check              # includes the scaled gate profile (correctness-fatal only)
```
