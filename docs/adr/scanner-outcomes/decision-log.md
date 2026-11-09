# ADR-085 part 9: Rules and decisions this ADR rests on

Part of the accepted [scanner outcome decision](../ADR-085-scanner-local-outcome-model.md).
Read its status and scope before using this part. Original evidence and delivery-plan claims
remain bound to their stated baselines; operator behavior is in the [scanner runbook](../../runbooks/scanner.md).

### 9. Rules and decisions this ADR rests on

The full records (why, rejected options, artifacts) are on the TKT-556 and TKT-399 tickets. A later
decision in this list supersedes an earlier one only where it says so.

#### 9.1 Owner rules

- **TKT-399 D29** (owner, 2026-10-16): nothing is minted or sent 3 s or more after scan start. Before
  actuation the limit is 30 s from scan start.
- **TKT-399 D30** (owner, 2026-10-17): a scan that the time limit stopped is never synced to Access,
  and the scanner never tells staff to admit after the limit. A row that the scan declared late is
  never reconciled, even when its terminal write fails in this page. The offline admission result is
  shown only when the scan is still inside the 30 s limit at the moment the result is decided. A late
  revocation-refusal mint is closed, not synced, and still says not valid for entry. The irreducible
  residual is page loss after a failed terminal write. It stays the ADR-025 §3 crash window.
- **TKT-399 D39** (owner, 2026-10-17): stop TKT-399 without a merge. Design the scanner local outcome
  model first (TKT-556), then re-plan TKT-399.
- **D1** (owner): deliver the outcome-table ADR amendment before product code; re-plan TKT-399 from
  the design. B remains reference input only.
- **D13-1** (owner): a live 401 is terminal. A 409/422 with an unreadable body is a terminal
  unknown-reason rejection. Keep previously queued facts across a sync 401. Cost of the live 401: no
  Access scan record for this HTTP request; occurrence U remains under P13; re-pair and rescan after
  token expiry. Never synced while the page lives. The TKT-399 D30 crash window (page loss after a
  failed terminal write) still applies, and the reloaded page shows O-W. Rows U01/R17.
- **D13-2** (owner): the Entry core may merge alone while main still lacks time safety. Exit must
  follow the time ticket. Section 8 defines merge dependencies.
- **D13-3** (owner): hold legacy PENDING, keep old QUEUED syncable, and include the operator
  export/acknowledge disposal in this delivery, in ticket 2 with the count, warning, export and
  acknowledgement, D25.
- **D13-4** (owner): WAITING page loss or reload is accepted under the page-loss rule only if the
  reloaded page shows the recovered scans. W01 and section 2.3 define visible recovery and its
  ordinary Access admission consequence.
- **D14-1** (owner): an offline decision made in time, with its instruction before the queue write,
  stands even if the write finishes past 30 s. Retry that same queue decision.
- **D14-2** (owner): V04 approved. A revoked Exit creates no row or request and says the holder may
  leave. The absent-exit `exit_required` consequence is accepted, residual E,
  `services/access/internal/store/conflicts.go:123-145`.
- **D14-3** (owner): the shared database at version 3. The separate-name option in section 7.2 is a
  rejected alternative.
- **D14-4** (owner): Entry reason copy stays in TKT-552. Exit work does not expand it.
- **D31** (owner): pre-upgrade QUEUED rows sync as on main, D13-3 read literally. They have no cause
  field. Main can queue a live 401 including a database outage, a body-first fallback after a decided
  response, foreign or reload PENDING recovery, and a refusal mint. Separately, `markQueued` accepts
  any prior state; no reaching App path from ACT was found. QUEUED with `actuated:true` is a shape
  fixture, not a proved producer. The owner accepts possible false facts, indistinguishable, and
  requires their ids in the D25 export. This relaxes D13-1 for pre-upgrade rows only. M02 records all
  six failure columns. It does not relax terminal new live 401 or known denial behavior.
- **D38** (owner): finish planning without a further review round. Apply wording, scoping, premise and
  test-plan fixes without a change to any cell's admission or sync outcome. If a fidelity check finds
  an outcome change, stop and return to the owner. D40 is the one change the owner approved that way.
- **D40** (owner, 2026-10-17): any 502/503/504 selects O02 with occurrence U in the fixed client,
  because the client cannot tell an external proxy's status from the gateway's and has no switch.
  ADR-025, through this amendment, states the Compose-only topology as a premise. Any intermediary
  before the gateway needs a new ADR and qualification first. D40 supersedes the earlier
  unknown-origin terminal U03 and disabled-fallback proposal, and D33's external-proxy clause.

#### 9.2 Agent decisions

Each agent decision states its cost.

- **D2/D3/D4:** latch one outcome; keep the five durable states with an explicit format and outcome
  tag; one page-owner lock. Cost: a lock held for the page's life and new record validation.
- **D5:** enforce the owner's TKT-399 D29/D30 limits at the scan boundary, in ticket 3. Cost: valid
  holders stopped after the limit.
- **D6:** terminal live 401, later accepted by owner D13-1 and superseded in form by D15. Cost: no
  Access scan record for this HTTP request, occurrence U under P13, and re-pair and rescan after token
  expiry.
- **D7, amended by D17:** an accepted answer with a failed actuation mark is terminal locally. Cost:
  A/H, an Access record without an opening, or a committed mark without an opening. Never reopen from
  ACT.
- **D8:** make direction, session, refusal and operator result one contract. Cost: camera generations
  stop before reset, prior results clear, and every Exit failure permits leaving.
- **D9, superseded by owner D14-3 and D23:** the version-3 barrier with explicit legacy classification.
- **D10/D11:** disable scans until compatible storage is ready; pin each guarantee at its enforcing
  test tier with mutations and 6-8 unmutated fixture runs. Cost: readiness stops and separate
  store/App/API/browser proof.
- **D12, superseded by D19.**
- **D15-D23:** status before body (D15); terminal local failure (D16); replay and CAS split (D17);
  PENDING-first refusal and the ADR-066 §8 amendment (D18); merge order (D19); held-row disposal
  (D20, superseded by D25); visible recovery (D21); observed mark completion governs opening (D22);
  shared v3 proved against a frozen main-v2 reader (D23). Cost: operator intervention, retained
  notices and a v3 rollback constraint.
- **D19:** merge in the order 1 -> 0 -> 2 -> 3 -> 4; ticket 0 may merge with ticket 2. Cost: interim
  Entry operation without time safety until ticket 3, and delayed Exit delivery.
- **D24, amended by D33-D37, D39 and owner D40:** exactly 502/503/504 select O02 with U;
  500/501/505-599 select U03 with `unconfirmed_server_error`. This ADR states the Compose-only premise.
  Cost: operator stops, possible A/U, and a new ADR plus qualification before any intermediary
  deployment. No client switch exists.
- **D25:** the held count, warning, export and acknowledgement ship with ticket 2; with D31,
  legacy-queued ids stay in the export after sync. Cost: a larger core and a local file that contains
  credentials.
- **D26-D29, D32:** test each X01 site with the input class each mutation changes (D26); repair before
  version-close (D27); retain recovery notices on all compatible pages (D28); mark unimplemented ADR
  rules by ticket (D29); reserve O-D for this page's own ACT (D32, which replaces the former TKT-556
  D30). Cost: more tests, upgrade delay, notice retention, document upkeep and operator resolution of
  other replay cases.
- **D33:** derive admission eligibility from the producer, through the producer-by-layer matrix.
  Owner D40 superseded its external-proxy clause.
- **D34-D37:** split device lookup into 401/503/500 on S/R/F (D34); add outer Access recover and
  check both write timeouts (D35); limit unknown-Exit copy by send status (D36); assert exact
  contracts and limit the invalid_credential copy (D37). Cost: a broader ticket 0, configuration
  limits and separate test tiers. Shipped in TKT-557.
- **D39, amends D34:** bound the read-only lookup SELECT, slot-policy read and BeginTx well inside
  3 s. Expiry is availability 503 for this HTTP request. Ticket-0 review picks the deadline and the
  supported classes and tests real pgx behavior. Cost: deadline design and driver tests. pgx dial
  behavior is not verified.
  TKT-557 amendment: ticket 0 ships a provisional value; TKT-560 qualifies it. Venue use after
  TKT-558 requires TKT-560 Done and the owner's selected bound and acceptable offline-spill rate.
- **D41:** record the model in this new ADR-085, which amends ADR-025 §3 and ADR-066 §8, instead of
  inside ADR-025. ADR-025 §3 and ADR-066 §8 get short amendment notes; their accepted text stays.
  Why: the tables are too large for a section of ADR-025, and a separate number keeps every existing
  ADR-025 citation stable. Cost: readers follow one more link.

No earlier proposal remains operative where these decisions supersede it.
