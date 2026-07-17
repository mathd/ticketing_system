# A load test's published number is a claim about the server only if the generator's health is part of the verdict

**Context (TKT-82, PR #59).** The on-sale load proof measures the per-pool throughput ceiling
that the waiting-room design (TKT-20) must respect. The first accepted run published
"highest stable 300 attempts/s, first unstable 600" — and the 600/s instability signature was
5,880 arrivals dropped at the client's fixed 512 in-flight cap while every started attempt
succeeded within the latency SLO. The knee being published was the load generator's, not the
pool's. (The pool's knee happened to be real — achieved throughput plateaued at ~398 ok/s —
but the evidence could not distinguish the two.)

Three adversarial review passes were needed to close all the routes by which the harness
could publish a claim its own client invalidated:

1. **Vacuous success.** Non-sellout stages classified every 409 as an "expected rejection",
   so a claim path wrongly rejecting *everything* still passed — with empty latency sample
   sets that satisfied the p99 SLO vacuously (empty percentile = 0). Rule: every non-sellout
   stage requires `OK == Started > 0` and zero rejections; SLO checks only run over real
   samples.
2. **Cap-defined knees.** Stability judged only on drops/errors lets the client's
   concurrency cap decide the bracket. Rule: size the cap by Little's law above
   `rate × latency-SLO` (here `rate × 4s` for a 3s SLO) so that *at the cap, the SLO is
   already violated* — then re-judge: drops while the SLO holds = **inconclusive run**
   (generator limit, abort loudly), SLO violated = genuine instability regardless of drops.
3. **Missed schedules.** An overloaded generator starts arrivals late (scheduler lag)
   without dropping any, publishing a rate that was never actually offered. Rule: lag p99
   over a hard bound (1s against a ~2ms nominal) aborts as inconclusive.

**The general rule.** For every "server X is stable at rate R" claim, the harness must be
able to answer: *did the client actually offer R on schedule, and would the failure mode
observed also occur on a bigger client?* If either answer is unknown, the run is
inconclusive — never a ceiling. Publish the generator-health evidence (lag, cap occupancy,
drops, achieved rate) alongside the number, and encode "inconclusive ≠ unstable" as distinct
outcomes in the harness, because a human reading a red run will otherwise conflate them.

**Where this lives now.** `smoke/internal/loadtest` + `smoke/onsale_load_test.go` (sweep
stability predicate, Little's-law cap, lag guard); documented in
`docs/verification/on-sale-load/README.md`. TKT-31/TKT-37 inherit the harness and these
rules; the remaining hardening (client/server error taxonomy, connection bounding) is
TKT-92.
