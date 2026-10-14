# TKT-380 evidence: promotion cap concurrency probes

Date: 2026-10-13. Decision record:
[ADR-084](../../adr/ADR-084-promotion-storage-and-redemption-contract.md).

## Results

| Probe | Question | Result |
|---|---|---|
| 1, S1 | On TODAY's presale code path, can a finalize that queued before expiry keep a hold that a cross-pool admission already left out of the count? | **Yes, a defect.** 6 of 6 runs: a code capped at 1 ends with 2 confirmed units. Control: 6 of 6 refuse. |
| 1, W | On TODAY's path, can an admission that waits on the code lock across `closes_at` be admitted after the window closed? | **Yes, a defect.** 6 of 6 runs admitted. Control (start after close): 6 of 6 refused. |
| 1b, W | Does reading the window in a separate statement after the lock close it? | **Yes.** 6 of 6 runs refused. |
| 2 | Does the R1 invariant (code lock through commit, capped or not, then `clock_timestamp()`) hold the cap in S1, S2 and S3? | **Yes.** Each of the three mutations breaks exactly the schedules it should. 7 unmutated repeats after the mutations: identical. The owner later chose R2, so R1 is the rejected design; this result stays as its evidence. |
| 3 | Does reading R2 (an expired held claim counts until a terminal write) hold the cap in S1 on today's presale path, and what does it cost in liveness (R2L)? | **Yes.** 6 of 6 runs and 7 repeats held in S1 and R2L. With today's predicate (the mutation), S1 and R2L break. |

Both presale defects and their reproduction: [presale-race-finding.md](presale-race-finding.md).

## What ran

`bash docs/evidence/TKT-380/run.sh <empty scratch directory>`, exit 0, "every expectation met".
The run wrote to a scratch directory (the `command=` line in `output/environment.txt`); its
contents were then copied to `output/` unchanged.

The probe sources that ran are committed at the HEAD that `output/environment.txt` records
(`3d85c471`), unchanged since:

    a04aabcfbdd8b50ac6d0ecf2b501c4d44ad2fcd56fb2860e779537d6f4250d8e  probe.go
    fa78f7b4faf70a43a6ebf27565c33fc8b988a3633bb6b6cd11c17dc1f96da4ad  prototype.go
    7c3575895586c9252a99090ed80b4f4f3f100bb7a35c4db3d3dff4ee50bc2789  window-fix.py
    739a9cc2888e5aa8775f771749caa4fe9a2521233210123ddb645cb2c71e0ee3  r2-predicate.py
    ded9de645ee7b52257d402dbc7140492643a6a4b33ed32b74d7ea0d0b9764afe  run.sh

[`run.sh`](run.sh) does this:

1. Copies `services/inventory` and `shared/go` to a temporary directory. Nothing under `services/`
   in the repository changes.
2. Puts [`probe.go`](probe.go) in the copy as `cmd/tkt380probe/main.go`, and
   [`prototype.go`](prototype.go) as `internal/store/tkt380_prototype.go`.
3. Builds six binaries: pristine; three mutations of the prototype (`sed` edits of the lines
   marked `MUTATION-POINT`); a window-fix build, where [`window-fix.py`](window-fix.py) edits
   the temporary copy of `presale_codes.go`; and an R2 build, where
   [`r2-predicate.py`](r2-predicate.py) edits the temporary copy of `channel_allocations.go`. Each
   edit is recorded as a zero-context diff in `output/` (`mutation-time.diff`, `mutation-lock.diff`,
   `mutation-uncapped.diff`, `window-fix.diff`, `r2-predicate.diff`). Each build restores the
   file it edited before the next one.
4. Starts a disposable `postgres:18.4-bookworm` container (the digest `compose.yaml` pins) on an
   ephemeral local port. A trap removes the container and the temporary directory on exit.
5. Runs each scenario in a fresh schema with the real Inventory migrations (`store.Migrate`).
6. **Enforces the expected result.** Each probe invocation has an expected exit code, and each
   schedule an expected verdict in every repeat. Any mismatch is written to
   `output/expectations.txt` and makes `run.sh` exit 1.

Probe exit codes: 0 = every schedule `HELD`; 1 = at least one `BROKEN` (the defect was observed)
and nothing unexpected; 2 = an `UNEXPECTED` outcome, an unmet reach condition, or a harness error.
A reach condition that fails stops the probe with exit 2; it cannot pass silently.

| Invocation | Expected exit | Expected verdicts |
|---|---|---|
| `probe1-presale` (pristine, `presale 6`) | 1 | control HELD, S1 BROKEN, window-control HELD, W BROKEN |
| `probe1-window-fix` (window-fix, `window 6`) | 0 | window-control HELD, W HELD |
| `probe2-baseline` (pristine, `prototype 1`) | 0 | S1, S2, S3 HELD |
| `probe2-mutation-time` | 1 | S1 BROKEN; S2, S3 HELD |
| `probe2-mutation-lock` | 1 | S2, S3 BROKEN; S1 HELD |
| `probe2-mutation-uncapped` | 1 | S3 BROKEN; S1, S2 HELD |
| `probe2-repeats` (pristine, `prototype 7`) | 0 | S1, S2, S3 HELD in 7 of 7 |
| `probe3-r2` (R2 build, `r2 6`) | 0 | control, r2-S1, r2-liveness HELD |
| `probe3-r2-mutation` (pristine, `r2 1`) | 1 | control HELD; r2-S1, r2-liveness BROKEN |
| `probe3-r2-repeats` (R2 build, `r2 7`) | 0 | control, r2-S1, r2-liveness HELD in 7 of 7 |

Observed: [`output/exit-codes.txt`](output/exit-codes.txt), [`output/expectations.txt`](output/expectations.txt)
(30 of 30 expectations met) and [`output/summary-distinct.txt`](output/summary-distinct.txt) (one
line per schedule means every repeat had the same outcome).

## Fixture

- One organizer, two GA pools P1 and P2, capacity 10 each.
- Each pool has a `presale` channel allocation, cap 10, `requires_code = true`. Nothing can refuse
  for capacity.
- One presale code `PROMO-CAP-1`. It stands in for a promotion code. Cap 1 in S1 and S2; no cap at
  first in S3; no cap and `closes_at` = now + 2 s in W.
- Hold TTL 2 seconds (the store's `ttl` parameter).
- Usage is read through `PresaleCodeStatuses`, which uses the shipped expression.

## Schedules

Every wait is on an observed database state (`pg_blocking_pids`, the database clock), not on a
sleep that hopes the other side arrived.

- **Control.** H (1 unit) on P2. Finalize H at once. Wait until the database clock is past H's
  expiry. Admission A (same code) on P1. HELD = A refused, usage 1.
- **S1 (time base).** H on P2. A blocker transaction takes P2's pool row lock. Finalize F starts
  and is observed blocked by the blocker; reach: its `xact_start` is before H's expiry. Wait until
  past expiry. A on P1 runs; reach: F is still blocked when A returns. Release the blocker. Confirm
  every surviving claim. HELD = A admitted, F refused (`expired`), usage 1. BROKEN = both succeed,
  usage 2.
- **S2 (code lock), prototype.** H on P2. F decides before expiry; a probe hook holds it open, waits
  until past expiry and starts A on P1, then returns when A finishes or is observed blocked by F.
  Reach: F decided before expiry and finalized. HELD = A waited on F and was refused, usage 1.
  BROKEN = A finished while F was open, usage 2.
- **S3 (cap edit), prototype.** As S2, but the code has no cap. In the hook, after expiry, an
  operator sets cap 1 (`IssuePresaleCode`, an upsert that locks the row). If the edit finishes
  while F is open, A runs at once; otherwise A runs after F commits and the edit completes.
  HELD = the edit waited on F, A refused, usage 1, cap 1. BROKEN = the edit finished while F was
  open, A admitted, usage 2, cap 1.
- **Window control.** Code closes in 2 s. Wait until past `closes_at`. A on P1. HELD = refused.
- **W (code window).** Code closes in 2 s. A blocker locks the code row `FOR UPDATE` and does not
  update it. A on P1 starts; reach: it is observed blocked by the blocker on a `presale_codes`
  statement whose `query_start` is before `closes_at`. Wait until past `closes_at`. Release the
  blocker (rollback). HELD = A refused. BROKEN = A admitted.
- **R2L (cleanup), R2 build.** Code capped at 1. H (1 unit) on P2. Wait until the database clock
  is past H's expiry; nothing writes the expiry. Admission A on P1, then read H's status. Hold B on
  P2 (`CreateHold` sweeps P2 before it takes the code lock), then read H's status again.
  HELD = A refused while H is `held`, B admitted, H `expired` after B, usage 1. BROKEN = A
  admitted (the R1 outcome: an expired H with no terminal write is left out of the count).

In probe 3, S1 runs on today's `Transition` (not the prototype). Its HELD case for R2 is: A refused,
F succeeds, usage 1 after confirm.

## Probe 1: today's code (`probe1-presale.txt`)

Drives the shipped `CreateHold` and `Transition`. Distinct summaries, each identical in all 6 runs:

    SUMMARY presale-control f_status=finalizing f_err=nil a_err=ErrPresaleCodeInvalid redeemed=1 cap=1 verdict=HELD
    SUMMARY presale-S1 a_err=nil f_status=finalizing f_err=nil redeemed_after_finalize=2 redeemed_after_confirm=2 cap=1 verdict=BROKEN
    SUMMARY window-control a_err=ErrPresaleCodeInvalid redeemed=0 verdict=HELD
    SUMMARY W a_err=nil redeemed=1 verdict=BROKEN

Run 1 of S1 and W, verbatim from [`output/probe1-presale.txt`](output/probe1-presale.txt):

    [run 1 presale-S1] H created on P2: expires_at=2026-10-13T23:38:35.591383Z
    [run 1 presale-S1] finalize F observed blocked on P2: wait=Lock xact_start=2026-10-13T23:38:33.609375Z (before expiry: true) query="SELECT 1 FROM inventory_pools WHERE slot_id=$1 FOR UPDATE"
    [run 1 presale-S1] DB clock now 2026-10-13T23:38:35.796644Z, past H expiry
    [run 1 presale-S1] admission A on P1 with the same code: err=nil; F still blocked after A returned: true
    [run 1 presale-S1] finalize F returned: status=finalizing err=nil
    [run 1 presale-S1] after confirm   H | P2 | confirmed | 1 | PROMO-CAP-1 | 2026-10-13T23:38:35.591383Z | true
    [run 1 presale-S1] after confirm   A | P1 | confirmed | 1 | PROMO-CAP-1 | 2026-10-13T23:38:37.816864Z | false
    [run 1 W] code window closes_at=2026-10-13T23:38:40.097173Z
    [run 1 W] admission A observed blocked on the code row: query_start=2026-10-13T23:38:38.276671Z (before closes_at: true) query="SELECT max_redemptions, ((opens_at IS NULL OR opens_at <= clock_timestamp())\n\tAND (closes_at IS NULL OR closes_at > clock_timestamp())) FROM presale_codes\n\t\t WHERE organizer_id=$1 AND channel_code=$2 AND code=$3 FOR UPDATE"
    [run 1 W] DB clock now 2026-10-13T23:38:40.310505Z, past closes_at; releasing the lock without an update
    [run 1 W]   A | P1 | held | 1 | PROMO-CAP-1 | 2026-10-13T23:38:42.313337Z | false

## Probe 1b: the window rule (`probe1-window-fix.txt`)

[`window-fix.py`](window-fix.py) splits `redeemPresaleCode`'s statement into the `FOR UPDATE` and a
separate window `SELECT` after it ([`output/window-fix.diff`](output/window-fix.diff)). The same W
schedule: `SUMMARY W a_err=ErrPresaleCodeInvalid redeemed=0 verdict=HELD`, 6 of 6 runs.

## Probe 2: the D13 prototype

[`prototype.go`](prototype.go) is a copy of `Transition` with three additions for a code-citing
claim on finalize or confirm: take the code row lock after the claim lock, capped or not; then
read `clock_timestamp()`; judge expiry on it. Admission is today's `CreateHold`, unchanged.

| Run | S1 | S2 | S3 | Exit |
|---|---|---|---|---|
| Baseline (1 run) | HELD | HELD | HELD | 0 |
| Mutation "time": `clock_timestamp()` → `now()` | **BROKEN** (usage 2) | HELD | HELD | 1 |
| Mutation "lock": lock removed | HELD | **BROKEN** (usage 2) | **BROKEN** (usage 2) | 1 |
| Mutation "uncapped": lock skipped when the code has no cap | HELD | HELD | **BROKEN** (usage 2, cap 1) | 1 |
| Repeats (7 runs, pristine) | HELD 7/7 | HELD 7/7 | HELD 7/7 | 0 |

Each part of the invariant is caught by a schedule that only it can break. Without S3, the
"uncapped" mutation would survive.

## Probe 3: reading R2

The owner chose reading R2 after probes 1 and 2 (ADR-084 §3). [`r2-predicate.py`](r2-predicate.py)
changes one line of the temporary `channel_allocations.go`: the usage count that `redeemPresaleCode`
reads under the code lock counts every `confirmed`, `finalizing` or `held` claim, with no expiry
test ([`output/r2-predicate.diff`](output/r2-predicate.diff)). `CreateHold` and `Transition` are
today's code. The mutation is the same schedules on the pristine build, that is, today's predicate.

| Run | Control | S1 | R2L | Exit |
|---|---|---|---|---|
| R2 build (6 runs) | HELD | HELD: A refused, F `finalizing`, usage 1 after confirm | HELD: A refused, H `held`; B admitted, H `expired`; usage 1 | 0 |
| Mutation: today's predicate (1 run) | HELD | **BROKEN**: A admitted, usage 2 after confirm | **BROKEN**: A admitted, B refused, H stays `held` | 1 |
| Repeats (R2 build, 7 runs) | HELD 7/7 | HELD 7/7 | HELD 7/7 | 0 |

Run 1 of R2L on the R2 build, verbatim from [`output/probe3-r2.txt`](output/probe3-r2.txt):

    [run 1 r2-liveness] H created on P2: expires_at=2026-10-13T23:41:21.958323Z; DB clock now 2026-10-13T23:41:22.17064Z
    [run 1 r2-liveness] admission A on P1: err=ErrPresaleCodeInvalid
    [run 1 r2-liveness] admission B on P2: err=nil
    [run 1 r2-liveness]   H | P2 | expired | 1 | PROMO-CAP-1 | 2026-10-13T23:41:21.958323Z | true
    [run 1 r2-liveness]   B | P2 | held | 1 | PROMO-CAP-1 | 2026-10-13T23:41:24.181061Z | false

S1 is the cap race of probe 1, so under R2 that defect does not occur. R2L shows the liveness cost:
the code stays blocked for P1 until a transaction on P2 writes H's expiry.

## Observations beside the question

- When `Transition` (and the prototype) finds a held claim expired and the target is `finalizing`,
  it returns `ErrConflict` without committing (`store.go:882`). The `expired` write is rolled back,
  so the row stays `held`. The lazy predicate still excludes it. This is today's behaviour, not a
  defect found here.

## Limits

- The probes drive the Inventory store directly. They do not run Commerce, the HTTP handlers or the
  gateway.
- The presale code stands in for the future promotion code. The prototypes are not the
  implementation; TKT-381 and TKT-382 must build and test the real paths.
- The D13 prototype changes only finalize and confirm. Draw-down was not prototyped or probed. The
  fresh-time admission count was not prototyped. The `FOR SHARE` variant for uncapped codes was not
  probed. The edit in S3 is today's `IssuePresaleCode` upsert; it does not read usage.
- Probe 3 changed only the usage predicate. It did not move the window read (probe 1b did that
  separately), did not edit a cap under R2, and did not test a cleanup design. The operator read
  (`PresaleCodeStatuses`) was not changed, so the usage the probes report still judges expiry.
- S1's HELD outcome depends on the mode: in modes `presale` and `prototype` only the R1 outcome (A
  admitted, F refused) is HELD; in mode `r2` only the R2 outcome (A refused, F succeeds) is HELD.
  So an R2 run cannot pass on the R1 outcome.
- The W explanation (PostgreSQL computes the target list before the row-lock wait and does not
  recompute it for a row that was not updated) is an inference that matches the observation. It
  was not read in the PostgreSQL source during this spike.
- A 2-second TTL and window make the schedules fast. A real TTL is 10 minutes by default
  (`compose.yaml`, `HOLD_TTL`). The probes show the races are reachable, not how often they happen.
- Repeats show determinism for these forced schedules on one machine. They are not a load test.

## Effect on the gate

`docs/evidence` is outside every Go module in `go.work`, and `make check` builds, vets and lints
per module directory. So `probe.go` and `prototype.go` are not compiled by the gate. TKT-348 and
TKT-356 committed Go probes here on the same basis. `make check` runs no `git diff --check` or
whitespace lint; the stored diffs are zero-context anyway, so `git diff --check` finds nothing in
`output/`.
