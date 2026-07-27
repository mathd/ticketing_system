# TKT-125 — is runtime response validation measurably expensive?

**Verdict: no.** At the on-sale NFR window, turning ADR-028 response validation off produces no
change distinguishable from run-to-run noise. The default stays **on everywhere**, which is the
outcome the ticket's COS 3 explicitly names as an honest close.

## Method

Harness: `smoke/onsale_load_test.go`, `ONSALE_PROFILE=full`, stage **`nfr-3000pm`** — 3,000
attempts/min for 180 s, **9,000 lifecycles per run**. That stage is the SLO stage (per-mutation
p99 ≤ 1 s, lifecycle p99 ≤ 3 s) and the one whose sample count makes a p99 meaningful; the gate
profile's 250-sample stage would put the "p99" at the 3rd-worst request, which is why it was not
used.

```sh
OPENAPI_RESPONSE_VALIDATION_ENABLED=<true|false> ONSALE_PROFILE=full SMOKE_TEST_TIMEOUT=40m \
  ONSALE_REPORT=... ./scripts/smoke.sh
```

- Commit `526934f`, Apple M4 Pro, 12 cores, local compose (client and server on one host),
  `DB_MAX_OPEN_CONNS=25`.
- Host load average during the runs: **2.8 – 13.9** (1-min, sampled every 15 s). Earlier attempts at
  load 150–415 were discarded — see § Discarded runs.
- **Positive control:** the off arm's setting was verified *inside the running container*
  (`docker inspect …-inventory-1` → `OPENAPI_RESPONSE_VALIDATION_ENABLED=false`), not inferred from
  the shell. Without this, both arms could silently measure the same configuration and produce a
  confident zero.
- Every run below had a healthy generator at `nfr-3000pm`: 9000/9000 offered→ok, 0 dropped,
  0 client errors, 0 server errors, scheduling lag p99 ≤ 2.7 ms.

## Results — `nfr-3000pm`, 9,000 lifecycles per run

| Run | Validation | hold p50 | hold p95 | **hold p99** | **lifecycle p99** | lag p99 |
|---|---|---|---|---|---|---|
| A | **on** | 2.3 ms | 3.3 ms | **4.7 ms** | **9.1 ms** | 1.2 ms |
| B | **on** | 2.9 ms | 4.2 ms | **8.1 ms** | **17.0 ms** | 2.5 ms |
| C | **off** | 2.7 ms | 3.6 ms | **5.1 ms** | **10.6 ms** | 2.7 ms |

## Reading it honestly

**The off run sits inside the on-vs-on spread, so no effect is measurable.**

- Two runs of the *same* configuration (A, B — both validation on) differ by **4.7 → 8.1 ms**, a
  **+72%** spread on hold p99. That is the noise floor of this harness on this host.
- The off run's 5.1 ms falls **between** them. Against A it is 8.5% *slower*; against B, 37%
  *faster*. **The sign of the "effect" depends on which same-configuration run you compare to** —
  the definition of a null result.
- Lifecycle p99 behaves identically: on = 9.1 / 17.0 ms, off = 10.6 ms, again inside the band.
- The more stable statistic agrees: hold p50 is 2.3 / 2.9 ms on, 2.7 ms off.

Against the materiality bar **pre-registered before any run** (`kind=plan-final`) — *material =
validation-on p99 exceeds validation-off p99 by ≥10% or ≥50 ms on any of hold / finalize /
confirm* — the result is **not material**: the absolute arm is nowhere near 50 ms (the entire p99
is under 10 ms), and the relative arm cannot be evaluated because the same-configuration noise
exceeds any candidate effect.

**A flaw in that bar, stated rather than quietly corrected.** The 10% relative arm is unusable at
these magnitudes: 10% of a 4.7 ms p99 is 0.47 ms, an order of magnitude inside the observed noise.
Had the two on-runs landed differently, the bar would have mechanically returned "material — turn it
off in production" on a sub-millisecond difference. The bar was applied as written and the verdict
does not depend on the flaw (the null result is robust either way), but a future measurement of this
kind should pre-register a **noise floor measured from repeated same-configuration runs**, not a
fixed percentage.

## What this refutes

The architecture review (R1) called unconditional response validation *"the largest self-inflicted
cost standing against the project's stated scalability assumption"*. At the profile the project
actually holds itself to, that is **not supported**: the cost is below this harness's ability to
detect it. The knob still ships (COS 1 requires it) — default **on**, changing nothing today — but
it is a lever for a future production topology, not a fix for a demonstrated problem. Nobody should
turn it off on the strength of this evidence.

## Known gap — finalize/confirm p99 are not reported separately

COS 3 asks for hold **/ finalize / confirm**. Only hold is in the harness's log line; the
per-mutation `finalize_p99_ms` and `confirm_p99_ms` exist solely in the JSON report, and
`writeReport` is the **last** statement of `onsaleFull` — unreachable whenever an earlier stage
aborts. On this single-host setup the ceiling sweep's `sweep-600` stage trips the generator guard
every time (4–9 client-side transport errors at 600 attempts/s, three runs out of three), so no
JSON was ever emitted and `response-validation-{on,off}.json` do not exist.

What stands in for them: **lifecycle p99 is hold + finalize + confirm end to end**, and it shows the
same null result. The two mutations are covered in aggregate, not individually.

Closing the gap needs a one-line harness change (`defer writeReport(t, report)`), which is outside
this ticket's plan — the plan explicitly recorded that `smoke/onsale_load_test.go` would not be
modified. Filed as a follow-up rather than taken here.

## A second overclaim, caught by review

The PR originally said the `context.Background()` fix made validation "observe the request's
cancellation and deadline". **It does not.** `openapi3filter.ValidateResponse` at kin-openapi
v0.142.0 accepts a `ctx` parameter and never reads it — not in `decodeBody`, not in `VisitJSON`,
nowhere in the function body (verified in the module source after the ai-review raised it).

Passing the request context is therefore correct-in-principle and future-proof, and buys **nothing
observable today**. This is the same failure the ticket's own `trace_id` premise made, repeated one
step down; it is recorded here rather than quietly corrected, because a ticket that opens by
debunking an unmeasured claim has no licence to ship one of its own.

## Discarded runs

Recorded so the discards cannot be mistaken for selection:

| Attempt | Load avg | Outcome | Discarded because |
|---|---|---|---|
| 1 | ~150 | `nfr-3000pm` inconclusive, 90 client transport errors | generator verdict — the harness's own guard refuses to publish it as server evidence |
| 2 | ~150 | `TestExpiredChannelHoldFreesItsCap` failed | a 50 ms hold TTL expiring early under host contention; unrelated to the diff |
| 3 | **415** | same store test + `TestScheduledReleaseIsLazyAndObservable`; load stage never reached | host 35× oversubscribed |

`make check` passed green on the same commit between attempts 2 and 3 — the control proving these
were load artifacts, not a regression. Runs A, B and C above are every run that produced a healthy
`nfr-3000pm` stage; none was excluded for its numbers.
