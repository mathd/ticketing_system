# TKT-380 evidence: promotion cap concurrency probes

Date: 2026-10-13. Repository head: `601d50be`. Decision record:
[ADR-084](../../adr/ADR-084-promotion-storage-and-redemption-contract.md).

## Results

| Probe | Question | Result |
|---|---|---|
| 1 | On TODAY's presale code path, can a finalize that queued before expiry keep a hold that a cross-pool admission already left out of the count? | **Confirmed.** 6 of 6 runs: the code capped at 1 ends with 2 confirmed units. Control: 6 of 6 runs refuse the admission. |
| 2 | Does the ADR-084 fresh-time invariant (code lock, then `clock_timestamp()`) close it? | **Yes, in both forced schedules.** Each mutation turns one schedule red. 7 unmutated repeats after the mutations gave identical outcomes. |

The race finding and its reproduction: [presale-race-finding.md](presale-race-finding.md).

## What ran

`bash docs/evidence/TKT-380/run.sh docs/evidence/TKT-380/output`

[`run.sh`](run.sh) does this:

1. Copies `services/inventory` and `shared/go` to a temporary directory. Nothing under `services/`
   in the repository changes.
2. Puts [`probe.go`](probe.go) in the copy as `cmd/tkt380probe/main.go`, and
   [`prototype.go`](prototype.go) as `internal/store/tkt380_prototype.go`.
3. Builds three binaries: pristine, mutation "time" and mutation "lock". The mutations are `sed`
   edits of the two lines marked `MUTATION-POINT` in the prototype. The diffs are in
   [`output/mutation-time.diff`](output/mutation-time.diff) and
   [`output/mutation-lock.diff`](output/mutation-lock.diff).
4. Starts a disposable `postgres:18.4-bookworm` container (the digest `compose.yaml` pins) on an
   ephemeral local port. A trap removes the container and the temporary directory on exit.
5. Runs each scenario in a fresh schema with the real Inventory migrations (`store.Migrate`).
6. Writes the outputs to `output/` and refuses to overwrite an existing directory.

Exit codes are in [`output/exit-codes.txt`](output/exit-codes.txt). The distinct outcome lines per
file are in [`output/summary-distinct.txt`](output/summary-distinct.txt). One line per schedule
means every repeat had the same outcome.

## Fixture

- One organizer, two GA pools P1 and P2, capacity 10 each.
- Each pool has a `presale` channel allocation, cap 10, `requires_code = true`. Nothing can refuse
  for capacity.
- One presale code `PROMO-CAP-1`, capped at 1 unit. It stands in for a promotion code.
- Hold TTL 2 seconds (the store's `ttl` parameter).
- Usage is read through `PresaleCodeStatuses`, which uses the shipped expression.

## Schedules

Every wait is on an observed database state, not on a sleep that hopes the other side arrived.

- **Control.** H (1 unit) on P2. Finalize H at once. Wait until the database clock is past H's
  expiry. Admission A (1 unit, same code) on P1. Expect A refused.
- **S1 (time base).** H on P2. A blocker transaction takes P2's pool row lock. Finalize F starts and
  is observed blocked by the blocker's pid (`pg_blocking_pids`); its `xact_start` is recorded and
  checked to be before H's expiry. Wait until the database clock is past expiry. A on P1 commits.
  Check F is still blocked. Release the blocker. F returns. Then confirm every surviving claim.
- **S2 (code lock), prototype only.** H on P2. F (prototype) decides and, before it commits, a probe
  hook waits until the database clock is past expiry and starts A on P1. The hook returns when A
  either finishes or is observed blocked by F's pid. F commits.

Reach checks are part of each summary line (`reach_*`). A run whose reach check fails reports
`NOT-REACHABLE` instead of a verdict. No run reported it.

## Probe 1: today's code (`probe1-presale.txt`)

Drives the shipped `CreateHold` and `Transition`. Summary, identical in all 6 runs:

    SUMMARY presale-control f_status=finalizing f_err=nil a_err=ErrPresaleCodeInvalid redeemed=1 cap=1
    SUMMARY presale-S1 reach_f_started_before_expiry=true reach_f_blocked_through_admission=true a_err=nil f_status=finalizing f_err=nil redeemed_after_finalize=2 redeemed_after_confirm=2 cap=1 verdict=CONFIRMED

Run 1, verbatim from [`output/probe1-presale.txt`](output/probe1-presale.txt):

    [run 1 presale-S1] H created on P2: expires_at=2026-10-13T18:12:59.775562Z
    [run 1 presale-S1] finalize F observed blocked on P2: pid=96 wait=Lock xact_start=2026-10-13T18:12:57.788488Z (before expiry: true) query="SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE"
    [run 1 presale-S1] DB clock now 2026-10-13T18:12:59.994284Z, past H expiry
    [run 1 presale-S1] admission A on P1 with the same code: err=nil; F still blocked after A returned: true
    [run 1 presale-S1] finalize F returned: status=finalizing err=nil
    [run 1 presale-S1] after F   H | P2 | finalizing | 1 | PROMO-CAP-1 | 2026-10-13T18:12:59.775562Z | true
    [run 1 presale-S1] after F   A | P1 | held | 1 | PROMO-CAP-1 | 2026-10-13T18:13:02.012479Z | false
    [run 1 presale-S1] after confirm   H | P2 | confirmed | 1 | PROMO-CAP-1 | 2026-10-13T18:12:59.775562Z | true
    [run 1 presale-S1] after confirm   A | P1 | confirmed | 1 | PROMO-CAP-1 | 2026-10-13T18:13:02.012479Z | false

## Probe 2: the D13 prototype

[`prototype.go`](prototype.go) is a copy of `Transition` with two additions for a code-citing claim
on finalize or confirm: take the code row lock after the claim lock, then read
`clock_timestamp()` and judge expiry on it. Admission is today's `CreateHold`, unchanged.

| Run | S1 | S2 | Exit |
|---|---|---|---|
| Baseline (1 run) | F refused (`expired`, `ErrConflict`), A admitted, usage 1: CAP-HELD | A blocked on F, then refused, usage 1: CAP-HELD | 0 |
| Mutation "time": `clock_timestamp()` → `now()` | F finalizes, usage 2: **CAP-OVERSHOT** | CAP-HELD | 1 |
| Mutation "lock": code lock removed | CAP-HELD | A finishes while F is open, usage 2: **CAP-OVERSHOT** | 1 |
| Repeats (7 runs, pristine) | CAP-HELD, identical 7 of 7 | CAP-HELD, identical 7 of 7 | 0 |

Each half of the invariant is caught by exactly one schedule. A single schedule would have let one
mutation survive.

## Observations beside the question

- When `Transition` (and the prototype) finds a held claim expired and the target is `finalizing`,
  it returns `ErrConflict` without committing (`store.go:882`). The `expired` write is rolled back,
  so the row stays `held` (visible in the prototype S1 rows). The lazy predicate still excludes it.
  This is today's behaviour, not a defect found here.

## Limits

- The probes drive the Inventory store directly. They do not run Commerce, the HTTP handlers or the
  gateway.
- The presale code stands in for the future promotion code. The prototype is not the
  implementation; TKT-381 and TKT-382 must build and test the real paths.
- The prototype changes only finalize and confirm. Draw-down was not prototyped or probed. The
  fresh-time admission count that D13 also asks for was not prototyped.
- A 2-second TTL makes the schedule fast. A real TTL is 10 minutes by default (`compose.yaml`,
  `HOLD_TTL`). The probes show the race is reachable, not how often it happens.
- Seven repeats show determinism for these forced schedules on one machine. They are not a load
  test.

## Effect on the gate

`docs/evidence` is outside every Go module in `go.work`, and `make check` builds, vets and lints
per module directory. So `probe.go` and `prototype.go` are not compiled by the gate. TKT-348 and
TKT-356 committed Go probes here on the same basis.
