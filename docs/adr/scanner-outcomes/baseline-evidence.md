# ADR-085 part 1: Scope, evidence, and current code

Part of the accepted [scanner outcome decision](../ADR-085-scanner-local-outcome-model.md).
Read its status and scope before using this part. Original evidence and delivery-plan claims
remain bound to their stated baselines; operator behavior is in the [scanner runbook](../../runbooks/scanner.md).

### 1. Scope, evidence, and current code

#### 1.1 Citation notation and limits

- A plain `path:line` citation refers to main at `fa4fbe5678ad751e3a8cccecdd62b2b94bd87f41`. "M"
  and "main" mean that commit.
- `B:` means the failed TKT-399 reference branch `TKT-399-pass-exit-mode` at
  `24f1c2096c0213db21500deca59c39ea9e02ecfc`. Existing B behavior is not approved because it exists.
- "TKT-399 thread" means the TKT-399 ticket thread on the sdlc board.
  Every numbered decision reference names its source ticket, for example `ADR-085 Dn of TKT-556`
  or `ADR-085 Dn of TKT-399`. `TKT-558 Dn` is a separate explicit ticket namespace. The ADR prefix
  identifies the containing record, not decision authority. There is no default source ticket.
  Parts of a grouped reference each name their source. The original shorthand and its context
  remain available at baseline commit `57dc4f4e`.
  ADR-085 D30 of TKT-556 was replaced by ADR-085 D32 of TKT-556.
  The owner time rule is ADR-085 D30 of TKT-399.
- F1-F4, P2-1 to P7-1, A-1, A-2, A6-1, A7-1, plan findings 1-21 and N1-N9 are TKT-399 review
  findings. DR1-x to DR4-x and DA1-x to DA4-x are TKT-556 plan-review findings.
- Citations into `net/http`, `net/http/httputil`, nethttp-middleware v1.2.0 (`oapi_validate.go`) and
  kin-openapi v0.149.0 (`openapi3filter/...`) are reads of the local Go 1.27.2 and module sources.
  These versions are the ones in `shared/go/go.mod`. They are library facts, not repository code and
  not executed probes.
- TKT-556 read targeted sections of 18 repository files: scanner App, occurrences, Vite config, main
  and package file; Access API server, scanner-device store, scan store, postgres store, policy store,
  entrypoint, OpenAPI and go.mod; shared contract implementation and tests, runtimecfg, gateway
  entrypoint and Compose. This is not a full-repository read.
- Transparent POST resend is general Chromium knowledge. It is not verified in this repository or by
  a Chrome probe. pgx dial behavior is not verified.
- Sections 2 to 8 define the accepted model. TKT-557 delivered the ticket-0 server subset.
  TKT-558 delivers the untimed Entry core, page ownership, storage barrier, recovery and disposal.
  Time behavior remains pending in this historical note. TKT-399 delivers Exit behavior. TKT-556 itself ran no implementation test, mutation,
  browser probe or gate.

#### 1.2 What the current code establishes

| ID | Verified fact | Evidence |
|---|---|---|
| E1 | M has five occurrence states, an actuation boolean, an optional owner and lease, and an optional local refusal. It has no direction field. | `web/scanner/src/occurrences.ts:11-26` |
| E2 | M opens `gate-occurrences` at version 2. Its open path has no blocked or version-change handler. It takes the owner lock and runs recovery after the database open. | `web/scanner/src/occurrences.ts:138-205`; a source search found neither `onblocked` nor `onversionchange` |
| E3 | M commits an ordinary mint as PENDING before returning. A local refusal is born QUEUED without an owner. Actuation checks PENDING, never-actuated, and owner in one transaction. Main's `markQueued` and `markSynced` accept any prior state. | `web/scanner/src/occurrences.ts:238-281`, `:304-317` |
| E4 | Recovery checks the owner's browser lock. It can queue a foreign PENDING row after lease expiry, or after a same-tab reload with the predecessor id. Closing a store releases its owner lock. | `web/scanner/src/occurrences.ts:158-185`, `:325-330`; owner creation `web/scanner/src/occurrence-owner.ts:6-29` |
| E5 | M sends a live scan after mint. A rejection stays on screen if its terminal write fails, but storage replacement releases its owner and makes the row recoverable. This is P7-1 for entries. | `web/scanner/src/App.tsx:295-320`, `:344-353`, `:161-172`; E4 |
| E6 | M queues a live-scan 401, then clears pairing. A failed queue write falls into the general queue path. JSON is read before the status branch. | `web/scanner/src/App.tsx:323-332`, `:356-367` |
| E7 | M lists all QUEUED rows, regardless of owner. Its reconciliation serializer sends no event type. Access interprets absent or empty event type as entry. Adding a direction property alone cannot protect an exit from an old tab. | `web/scanner/src/occurrences.ts:318-322`; `web/scanner/src/App.tsx:448-455`; `services/access/internal/api/server.go:684-693` |
| E8 | Access rejects a row with both a local decision and a nonempty event type. Local refusals go to a separate table with no direction column in its insert. | `services/access/internal/api/server.go:675-720`; `services/access/internal/store/reconcile.go:68-119` |
| E9 | Access reconciliation checks identity and Exit applicability. The pass path records entry/exit without the live policy or commercial-void refusals. The single path records facts but checks commercial voids and owes an alarm; a prior distinct admission records duplicate_admit and returns conflict. A failed local rejection write must not send that attempt here. | `services/access/internal/store/reconcile.go:146-260`, `:290-352`, `:359-381`; compare live refusals `services/access/internal/store/scan.go:95-114`, `:193-202` |
| E10 | Access replays a recorded occurrence before another append. On a verified chain, a new pass fact goes through the lifecycle append path. That path writes integrity data and signs the head. On a failed chain verification, the fact is recorded in `lifecycle_integrity_quarantine` and the append path is not reached (`services/access/internal/store/reconcile.go:208-236`). A signature does not prove that an admission happened. | `services/access/internal/store/reconcile.go:192-205`, `:239-260`; `services/access/internal/store/lifecycle.go:184-246`; `docs/adr/ADR-025-admission-events-and-offline-reconciliation.md:312-328`; `docs/adr/ADR-021-ticket-lifecycle-trail-integrity.md:158-198` |
| E11 | B protects late rows and tracks in-flight rows, but releases a rejected row after a failed terminal write. B's test explicitly expects that rejection to reconcile. The cited ADR paragraph calls that recovery the decided outcome. | B:`web/scanner/src/occurrences.ts:113-195`, `:548-555`; B:`web/scanner/src/App.tsx:499-509`; B:`web/scanner/src/App.scan-time.test.tsx:862-879`; B:`docs/adr/ADR-025-admission-events-and-offline-reconciliation.md:301-306` |
| E12 | B has 3000 ms to mint/send, 30000 ms to open, and a 1000 ms clock-disagreement rule. It checks both clock readings. M has no such scan guard. | B:`web/scanner/src/scan-clock.ts:20-58`; `web/scanner/src/App.tsx:275-372` |

The governing distinction is already in ADR-025. A connected denial is not an admission. Reconciliation records facts rather than deciding permission, `docs/adr/ADR-025-admission-events-and-offline-reconciliation.md:84-111`. Pending-before-send, own-row actuation, and mark-before-open remain binding, `docs/adr/ADR-025-admission-events-and-offline-reconciliation.md:160-209`. ADR-066's local refusal creates no admission, `docs/adr/ADR-066-scanner-voided-ticket-feed.md:183-204`.

#### 1.3 Gaps on main today

At the inspected baseline, main had these gaps and `docs/development.md` carried the operator
list. This is historical evidence. Use the [scanner runbook](../../runbooks/scanner.md) for current
behavior, delivered stages and remaining qualification limits.

1. **Entry P7-1.** A rejected live Entry scan stays on screen when its terminal write fails. Storage
   replacement then releases the owner, and a later open recovers the PENDING row into QUEUED.
   Reconciliation appends it as an admission fact, E9/E10. `web/scanner/src/App.tsx:344-353`,
   `:161-172`; `web/scanner/src/occurrences.ts:158-185`. Ticket 2.
2. **A live 401, including a database outage, is queued and syncs as an admission.** The scanner
   queues a live-scan 401, `web/scanner/src/App.tsx:323-332`. Before TKT-557, Access mapped every
   device lookup error to 401, so a database outage reached the scanner as 401. Ticket 0 (TKT-557)
   split the lookup result into 401, lookup 503 and lookup 500. The main scanner still queues all
   three, item 7. Ticket 2 makes a live 401 terminal.
3. **Body-first decode with a generic queue fallback.** The scanner reads JSON before the status
   branch. A general response failure goes to the offline queue, `web/scanner/src/App.tsx:323-366`.
   The denial decoder can throw on a denial body, `web/scanner/src/access-contract.ts:79-87`. A
   decided 409/422, an accepted 200, a 500, an unreadable body at any status, or any status outside
   200/409/422 can therefore become an offline admission. Ticket 2.
4. **Refusals are minted QUEUED.** `web/scanner/src/occurrences.ts:270-281`. No time check follows
   the refusal mint, `web/scanner/src/App.tsx:287-293`. Ticket 2 mints them PENDING first; ticket 3
   adds the time check.
5. **No ADR-085 D30 of TKT-399 checks.** Main has no elapsed-time guard, `web/scanner/src/App.tsx:275-372`, E12.
   It publishes the offline result after the queue write, `web/scanner/src/App.tsx:356-367`.
   Ticket 3.
6. **Queue and sync transitions accept any prior state.** `markQueued` and `markSynced`,
   `web/scanner/src/occurrences.ts:311-317`. TKT-556 found no App path that queues an ACTUATED row.
   Ticket 2.
7. **Ticket 0 shipped in TKT-557; the client does not use it yet.** Access now splits the device
   lookup into 401, 503 `scanner device lookup unavailable` and 500 `scanner device lookup failed` on
   S/R/F; adds S 503 `scanner store unavailable` for proved failures at the initial policy read and the
   two acquisitions; bounds those four steps and the advisory Touch at a PROVISIONAL 500 ms each,
   which TKT-560 qualifies; declares F 503; recovers a panic before headers as exact 500 `request
   failed`; and refuses a write timeout below 30 s in Access and the gateway. Until ticket 2, main's
   scanner still queues lookup 500, recovery 500 and every new 503 as offline admissions and keeps
   pairing on them. A panic after headers, kill, shutdown, network loss and an expired write still
   lose a response, U/A.

One more item is an accepted residual, not a gap that a ticket closes. After ticket 2, rows that main
queued before the upgrade keep syncing as on main, owner ADR-085 D31 of TKT-556 and row M02. They have no cause field.
Some can be false admission facts that nobody can tell apart. Their ids go into the ADR-085 D25 of TKT-556 export.
