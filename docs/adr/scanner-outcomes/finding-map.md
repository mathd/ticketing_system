# ADR-085 part 5: Complete finding map

Part of the accepted [scanner outcome decision](../ADR-085-scanner-local-outcome-model.md).
Read its status and scope before using this part. Original evidence and delivery-plan claims
remain bound to their stated baselines; operator behavior is in the [scanner runbook](../../runbooks/scanner.md).

### 5. Complete finding map

#### 5.1 Review passes 1-7 and audits

"Same on M" below means a code-path match established by reading, unless execution is explicitly stated. No case was executed in TKT-556. "Not present" refers only to the named mechanism, not proof that all related behavior is safe.

| Finding | Table cells and invariant answer | Does main have it? Verified evidence and limit |
|---|---|---|
| F1 | O01-X/T,V and all new queue rows; I5/I7; section 7 isolation | The old reader and entry default are on M (E7). M cannot mint exits (E1, `web/scanner/src/App.tsx:312-316`), so the mixed-version exit trigger is not yet on M. Adding exits to shared v2 would expose it. |
| F2 | C01-X/C,T; I5/I6, stop camera before session reset | The session boundary gap exists on M: camera generations are checked at `web/scanner/src/App.tsx:529-603`, but `web/scanner/src/App.tsx:615-630` does not stop the camera. The exit-to-entry symptom needs the new mode. |
| F3 | C01-E/X and D01-X; I6, clear old result at reset | M's reset does not clear the result, `web/scanner/src/App.tsx:615-630`; result rendering still reads it at `:726-736`. Exit-specific wording is not on M. |
| F4 | A01-E/X/S and S02; I3, synchronous transaction-complete observer | M's occurrence App test inspects request shape at `web/scanner/src/App.occurrences.test.tsx:35-47`. It has no transaction observer. It does not prove commit-before-send. B's replacement observer is at B:`web/scanner/src/App.occurrences.test.tsx:39-56`, `:144-160`; its reported mutation results are in the TKT-399 thread and were not rerun for TKT-556. |
| P2-1 | S01-E/X/T; L01/T; I4, readiness plus scan cancellation | No upgrade barrier or bound on M (E2/E12), so that new-tab-v3 queue sequence is absent. M's open/startup is still awaited before mint, `web/scanner/src/App.tsx:284-296`; no elapsed guard follows it. |
| A-1 | Same cells as P2-1; I4; delayed readiness cannot resume a scan | Same classification as P2-1. Three independent module contexts are required; a shared module cache is insufficient. B's three-context test is B:`web/scanner/src/occurrences.test.ts:597-639`. |
| A-2 | V01-E/S,T; I5/I7; refusal has no event type | M already sends only `local_decision` for refusals, `web/scanner/src/App.tsx:448-455`. The false "every row has event_type" claim is historical, from the TKT-399 thread. No such claim is adopted here. |
| P3-1 | S01/C,T, L01/L02; I4, elapsed check even if timer withheld | M has no open timer or elapsed guard (E2/E12). B's late-success protection is B:`web/scanner/src/occurrences.ts:361-386`. Timer-only code must not be copied. |
| P3-2 | L01/L02 boundary tests; I4; independent literal limits | M has no corresponding limit tests because no guard exists, `web/scanner/src/App.tsx:275-372`. B's independent literal-bound test is B:`web/scanner/src/occurrences.test.ts:645-669`. |
| P3-3 | S01/T,V; I4/I5; section 7 states queue scope and unmeasured policy | The inaccurate new open-policy prose is not on M; M opens one named DB at `web/scanner/src/occurrences.ts:149`. Per-storage-key/database queue and timing cost are design reasoning **not verified by a new browser probe**. The TKT-399 thread supplies the finding. |
| P4-1 | S01/C,T and L01/L02; I4 covers lock and recovery, not only open | The unbounded dependencies exist on M, `web/scanner/src/occurrences.ts:190-200`; `web/scanner/src/App.tsx:153-158`, `:284-296`. M has no claimed time bound. |
| P4-2 | L01-L05; I4 production dual clock | M has no elapsed guard (E12). B's dual reader/check is B:`web/scanner/src/scan-clock.ts:34-58`. Physical-phone sleep behavior was unverified in TKT-399 and remains unverified. |
| P4-3 | Every V cell; I2/I8; existing transactions can delay upgrade | M has no version-change handler (E2). The false guarantee was B's amendment, TKT-399 history D18. This ADR promises closure of a connection, not immediate transaction completion or unblocked future upgrade. |
| P4-4 | All storage-delay test fixtures; section 6 bounded helpers | This finding names B's test helpers, B:`web/scanner/src/occurrences.test.ts:445-472`; not M's production path. An equivalent audit of every M test await is **not verified**. |
| P5-1 | L02-L05/F,C,T,V and V02-E/F; I1/I2/I8 | M has no "late" state/guard, but the same failed-terminal-write to recovery shape is present in E4/E5. Adding a late terminal outcome without fixing ownership would inherit it. |
| P5-2 | O01/O02/F,C; L05/all columns; I1/I4/I6 | M awaits queue persistence then publishes offline admission, `web/scanner/src/App.tsx:356-367`, with admission copy at `:735`. No elapsed check exists, so the general late offline-admit behavior is on M. |
| P5-3 | V01-E/T; V02-E/all; I1/I4/I5 | M mints refusals directly QUEUED, E3. It has no late check, `web/scanner/src/App.tsx:287-293`. This is the same premature-sync structure, before a time rule is added. |
| P5-4 | L01-L05; I4; production reader substitution mutation | M has no clock reader (E12). B's reader test is B:`web/scanner/src/scan-clock.test.ts:96-108`. App tests that replace the reader cannot cover it. |
| P5-5 | D01-X/all uses neutral duplicate; D02-X/all uses O-G with A+D; I6 keeps leave text in both | M's only duplicate result is a red alert at `web/scanner/src/App.tsx:736`; Exit mode does not exist. Copying it into Exit would recreate the finding. |
| A6-1 | All PEND C/T/V cells, especially L02-L05 and U01; I2/I8 | M closes the DB and releases the owner at `web/scanner/src/occurrences.ts:325-330`; storage replacement changes the owner at `web/scanner/src/App.tsx:161-172`. Its same live-page recovery window exists, though M has no late declaration. |
| P7-1 | Every R01-R17 E/X F/C/T/V cell; I1/I2/I7 | **Yes for entries.** `web/scanner/src/App.tsx:344-353`, `:161-172`; recovery `web/scanner/src/occurrences.ts:158-185`; reconciliation appends pass facts E9/E10. Exits are not on M. B's bad assertion and rationale are E11. Do not retain either. |
| A7-1 | U01-E/X/F,C,T,V; I1/I2, owner-decided terminal 401 test below | M has the queue-on-401/fallback shape E6. Its fake storage test cannot prove owner retention, `web/scanner/src/App.storage.test.tsx:32-47`. B's scan-time suite has no 401 scenario in the inspected file, B:`web/scanner/src/App.scan-time.test.tsx:1-925`; a text search for `401` has no match. TKT-556 ran no mutation; section 6 requires an exact production-store test of the replacement contract. |

#### 5.2 Earlier TKT-399 plan critiques and coverage notes

The whole history also has findings numbered 1-21 and N1-N9, plus two deferred notes. They must not disappear merely because later reviews gave findings lettered ids.

| Historical finding | Answer and cell/invariant | Main status and evidence |
|---|---|---|
| Plan 1-4, 16-19, 20: verified mechanics and reference corrections | Capture and transport in I5/I7; O01-X, A01-X; section 1 records exact current locations. No new conclusion from old line numbers. | Live direction and reconcile defaults exist in `services/access/internal/api/server.go:536-550`, `:684-693`; client mode does not, E1. Other old reference claims are not treated as current evidence. |
| Plan 5/6 and N4: direction belongs on required mint argument; no optional-before-required signature | Section 2.2; I5; A01-X/S. Normal mint takes credential, time, required direction. The Entry-only core supplies explicit entry; Exit delivery later extends the value set and selector. Refusal uses its own operation. | M's current optional third parameter is local decision, `web/scanner/src/occurrences.ts:30-41`, `:255-273`. A new signature is proposed, not implemented. |
| Plan 7, N5: corrupt direction and own undefined | Sections 7.1/7.4 legacy/corruption rules; I5/I7; O01-X/T. Test absence, own undefined, null, empty string, number, unknown nonempty string, entry, exit. | M has no direction field E1; Access treats empty event type as entry and unknown nonempty strings as per-row rejection, `services/access/internal/api/server.go:684-693`. |
| Plan 8/9, N9: unbuildable fixture; unused count-limit seed; single-policy case observes only policy | Section 6 browser fixture; no unrelated issuance path or decorative seed. R05-X and O01-X each pin their own mechanism. | Random ticket fixture exists `test/browser/scanner.mjs:24-59`; missing policy defaults single `services/access/internal/store/policy.go:73-81`. A new pass browser fixture is proposed, not tested here. |
| Plan 10/11/12: exact lifecycle identity/time, pair each context, exact new-entry request | Section 6; I3/I5/I7; A01-E/X and O01-X. Never weaken an assertion to allow an omitted new entry type. | Existing exact refusal readback uses id/time `test/browser/scanner.mjs:165-185`; pairing `test/browser/scanner.mjs:89-99`; M request-shape test at `web/scanner/src/App.occurrences.test.tsx:35-47`. |
| Plan 13/14/15 and N6: leave wording, refusal protocol, reset, old result, accepted Exit actuation | I5/I6; all X rows, V04-X, C01-X; sections 2/3 and D8. | M Entry-only UI `web/scanner/src/App.tsx:704-736`; local-refusal protocol E8. New Exit choices are proposed here, not silently inherited from B. |
| Plan 21 and N7: repeat fixtures, browser-only mutation | Section 6 repeat rule and browser-only control mutation. | This is a verification requirement, not a current source defect. No repeat or browser run occurred here. |
| N1/N2/N3: stale plan alternatives, Entry copy contradiction, ADR-066 deviation | This ADR has one active model; I6 and V01/V04 specify it. Section 8 requires matching ADR-025/066 amendments and scopes Entry-reason work explicitly. | M ADR-066 still describes its Entry-only scanner at `docs/adr/ADR-066-scanner-voided-ticket-feed.md:183-204`; M generic Entry denial is `web/scanner/src/App.tsx:734`. |
| N8: historical unknown-event-type test gap | Addressed on M by TKT-553. The test sends a valid Exit control to recording, then asserts the unknown type is rejected per item without reaching recording. Read, not executed in TKT-556. | `services/access/internal/api/server_test.go:324-400`; guard `services/access/internal/api/server.go:691-693`. |
| "missed 4": rejected reconciliation reported as Synced | I7 names correct semantics; section 8 leaves full reporting feature to existing TKT-551. New tests assert actual per-row result and durable state, not a count labelled Synced. | Historical retirement/count citation: `web/scanner/src/App.tsx:501-510` in the reviewed baseline. Delivered: TKT-551, see I7. |
| "missed 5": offline single-policy exit browser case could bless rejection | Section 6 has a server-policy assertion separate from sync UI. Do not bless the generic Synced count as success. | Single-policy reconciliation refuses Exit at `services/access/internal/store/reconcile.go:181-185`; full reporting was allocated to TKT-551. Delivered: TKT-551, see I7. |
| Audit pass 2: unlogged F4 seam; stale gate/browser evidence | D11 records the synchronous transaction observer. Each delivery ticket requires evidence on its reviewed final head, never the earlier B run. | B observer is cited in F4 above. TKT-556 claims no gate or browser evidence. |

#### 5.3 Accepted TKT-556 plan-review findings, round 3

| Finding or decision | Required change and proof location |
|---|---|
| DR3 principle / DA3-6 / D33 | Matrix P01-P12 and derivation in section 2.4; producer received no Access response or Access proved a pre-decision outcome; owner D40 supersedes the former unknown-producer terminal proposal with O02 and U for external 502/503/504, outside the Compose-only premise; V01/W01/M02/repair have explicit authority. |
| D31 | Section 7.4 M02 S/F/C/T/V/P, P10 producer list, D25 export membership, I1/I5/I7/I8 exceptions, section 1.3 main gaps and the M02 residual; possible false facts, indistinguishable. |
| DR3-1 / DA3-3 / D34 | P03 auth SELECT before handler; ticket 0 three-way S/R/F split, explicit discriminator, exact API cases and availability-to-401 mutation; amended D24 rationale and interim main 503 copy. |
| DR3-2 / DA3-2 / D35 | P05/P07 runtime origins and section 2.4b; outer Access recover, both write limits, panic-after-commit API mutation; ReadTimeout excluded as handler timer. |
| DR3-4 / DA3-4 / D36 | D24 amendment, section 3 copy by sent/unsent/refused/known accepted result; I1/I6 exact copy tests. |
| DR3-5 / D37 | P01/P03 and R06 producer list; request-validator failures include oversized body and route lookup; operator guide cannot equate invalid_credential with signature failure. |
| DR3-6 / D37 | Section 6 ticket-0 API batch uses Router(log, true), exact status/body and malformed-503-body mutation; feed contract declares new 503. |
| DR3-7 | I1 App test claims only terminal U03/no reconcile for 500; actual decided refusal behind 500 is proved at the Access contract tier. |
| DR3-8 | E9/P12 pass-only refusal-bypass statement, single-path alarm, I7 conflict as recorded/alarmed duplicate_admit and terminal locally. |
| DA3-5 / D32 | O-D own-ACT reservation renumbered; TKT-399 D30 names the owner time rule in this ADR. |
