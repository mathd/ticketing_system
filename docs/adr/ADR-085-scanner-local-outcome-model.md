# ADR-085: Scanner local outcome model

Date: 2026-10-17

## Status

Accepted (TKT-556, a spike; owner decisions ADR-085 D1 of TKT-556, ADR-085 D38 and ADR-085 D40). Amends ADR-025 §3 and
ADR-066 §8.

This ADR records the accepted model and its staged delivery. TKT-557 delivered the Access and
gateway response contract, TKT-558 the Entry core, TKT-559 the time contract, and TKT-399 the
Exit behavior. The delivered time and Exit commits are `3752a96a` and `371e44b3`, both ancestors
of the documentation-review baseline. Current operator instructions are in the
[scanner runbook](../runbooks/scanner.md). Venue qualification and owner-selected bounds/offline
spill remain TKT-560 requirements; this review did not read live ticket status.

The detailed decision is split into the linked parts below. Their accepted rules remain part of
ADR-085. This reorganization changes no outcome, bound, test obligation, or owner decision.
Read only the part needed for the change, including applicable limits and delivery notes.
"Main" and "today" in baseline evidence, finding maps, and original delivery plans refer to the
commits those sections name. They do not describe the present tree. The 500 ms pre-decision
bounds remain provisional. ADR-025 and ADR-066 amendments remain binding.

## Context

The scanner keeps one durable IndexedDB record for each scan occurrence, ADR-025 §3. A live scan can
end in many local outcomes: accepted, rejected, unauthorized, offline, unconfirmed, late, cancelled or
local failure. Each outcome needs a local write. That write can fail. Its connection can close,
another tab can be open, a version change can arrive, or the page can be lost. The inspected pre-core baseline had no model of
this product of outcome and failure point. The product decides what the scanner sends to
reconciliation, and reconciliation records an admission fact: on a verified chain it appends a signed
lifecycle event, ADR-021; on a broken chain it records the fact on the quarantine side. A wrong cell
therefore gives a false admission fact, signed when the chain verifies.

**TKT-399 history, in brief.** TKT-399 (pass Exit mode) went through seven review passes. Its fix for
F1, the mixed-version Exit defect, grew into a time-safety layer on ADR-025's offline protocol. The
owner set the time rule in ADR-085 D29 of TKT-399 and ADR-085 D30 of TKT-399: no mint or send at or after 3 s from scan
start, no admission decision at or after 30 s, and no sync for a scan that the limit stopped. Passes 5
to 7 each found a new cell of the same unmodelled table. Each fix was correct for its own cell. Pass 7
found P7-1: a rejected scan whose terminal write failed was recovered and synced as an admission.
P7-1 also exists on main for Entry scans, section 1.3. Owner decision ADR-085 D39 of TKT-399 stopped TKT-399
without a merge (PR #451 closed) and asked for this model before any code. The branch
`TKT-399-pass-exit-mode` at `24f1c209` stays as reference input only, ADR-085 D1 of TKT-556.

**Constraints.** ADR-025 already sets the governing distinction. A connected denial is not an
admission. Reconciliation records facts; it does not decide permission. Pending-before-send, own-row
actuation and mark-before-open stay binding. An ADR-066 local refusal creates no admission. ADR-021's
trust boundary does not change. This ADR covers honest scanner code and storage failures. It does not
cover a compromised same-origin page, a scanner under attacker control, or a database writer.

## Possible Solutions

- **Option 1: fix P7-1 in a TKT-399 round 8.**
    - Pros: smallest change.
    - Cons: passes 5 to 7 show that each fix opens a new cell of the same table. The owner rejected
      this option in ADR-085 D39 of TKT-399 and ADR-085 D1 of TKT-556.
- **Option 2: model every local outcome against every failure point first, then deliver in tickets.**
    - Pros: each cell is decided once, in writing, before code. Each invariant gets a test tier and a
      mutation.
    - Cons: a large ADR and five delivery tickets (0 to 4).

Option 2 has four sub-choices with real alternatives:

- **Old-tab barrier (F1).** A shared database at version 3 (owner ADR-085 D14-3, chosen) or a separate
  database name. Section 7.2 records the costs of the rejected alternative.
- **Live HTTP 500.** Terminal with no server change stops every gate on an Access store outage.
  Offline as on main turns a validator-hidden refusal into a signed false fact. The chosen split keeps
  exactly 502/503/504 offline-eligible, makes 500 terminal, and adds Access ticket 0 for proved
  pre-decision 503, ADR-085 D24 as amended by ADR-085 D33-ADR-085 D37 and ADR-085 D39.
- **Live 401.** Queue as on main, or terminal. The owner chose terminal, ADR-085 D13-1.
- **Legacy QUEUED rows at upgrade.** Sync as on main, with possible false facts, or hold all of them,
  with re-entry risk. The owner chose sync, ADR-085 D31.

## Decision

We adopt one local decision per scanner occurrence. The page decides each occurrence once, before its
final outcome write. No write failure changes that decision. Only an in-time queue decision from a
verified producer makes a new live-page row reconcilable. Sections 1 to 8 define the model. Section 9
lists the owner rules and agent decisions that it rests on.

### 1. Scope, evidence, and current code

Read [1. Scope, evidence, and current code](scanner-outcomes/baseline-evidence.md).

#### 1.1 Citation notation and limits

See [this section](scanner-outcomes/baseline-evidence.md#11-citation-notation-and-limits).

#### 1.2 What the current code establishes

See [this section](scanner-outcomes/baseline-evidence.md#12-what-the-current-code-establishes).

#### 1.3 Gaps on main today

See [this section](scanner-outcomes/baseline-evidence.md#13-gaps-on-main-today).

### 2. Actors, fields, and states

Read [2. Actors, fields, and states](scanner-outcomes/state-model.md).

<a id="21-actors-and-ownership-d2-and-d3"></a>

#### 2.1 Actors and ownership, ADR-085 D2 and ADR-085 D3

See [this section](scanner-outcomes/state-model.md#21-actors-and-ownership-d2-and-d3).

<a id="22-durable-record-d4"></a>

#### 2.2 Durable record, ADR-085 D4

See [this section](scanner-outcomes/state-model.md#22-durable-record-d4).

#### 2.3 Allowed transitions and repair

See [this section](scanner-outcomes/state-model.md#23-allowed-transitions-and-repair).

<a id="24-client-knowledge-is-an-independent-axis-d15-d17"></a>

#### 2.4 Client knowledge is an independent axis, ADR-085 D15-ADR-085 D17

See [this section](scanner-outcomes/state-model.md#24-client-knowledge-is-an-independent-axis-d15-d17).

<a id="24a-producer-by-layer-matrix-d33"></a>

#### 2.4a Producer-by-layer matrix, ADR-085 D33

See [this section](scanner-outcomes/state-model.md#24a-producer-by-layer-matrix-d33).

<a id="24b-runtime-limits-and-closed-classes-d35d39"></a>

#### 2.4b Runtime limits and closed classes, ADR-085 D35/ADR-085 D39

See [this section](scanner-outcomes/state-model.md#24b-runtime-limits-and-closed-classes-d35d39).

#### 2.4c Derivation of the outcome rows

See [this section](scanner-outcomes/state-model.md#24c-derivation-of-the-outcome-rows).

### 3. Decision time, operator result, and the full outcome table

Read [3. Decision time, operator result, and the full outcome table](scanner-outcomes/outcome-tables.md).

<a id="31-time-and-response-classification-d5-d7-and-d24"></a>

#### 3.1 Time and response classification, ADR-085 D5-ADR-085 D7 and ADR-085 D24

See [this section](scanner-outcomes/outcome-tables.md#31-time-and-response-classification-d5-d7-and-d24).

#### 3.2 Operator codes and residual codes

See [this section](scanner-outcomes/outcome-tables.md#32-operator-codes-and-residual-codes).

#### 3.3 Reading the six failure columns

See [this section](scanner-outcomes/outcome-tables.md#33-reading-the-six-failure-columns).

#### 3.4 Accepted, duplicate, and authentication outcomes

See [this section](scanner-outcomes/outcome-tables.md#34-accepted-duplicate-and-authentication-outcomes).

#### 3.5 Every live rejection, by reason and direction

See [this section](scanner-outcomes/outcome-tables.md#35-every-live-rejection-by-reason-and-direction).

#### 3.6 Offline, partial knowledge, local exceptions, storage, cancellation, and reload

See [this section](scanner-outcomes/outcome-tables.md#36-offline-partial-knowledge-local-exceptions-storage-cancellation-and-reload).

#### 3.7 Every late checkpoint

See [this section](scanner-outcomes/outcome-tables.md#37-every-late-checkpoint).

#### 3.8 Local revocation and exits of revoked tickets

See [this section](scanner-outcomes/outcome-tables.md#38-local-revocation-and-exits-of-revoked-tickets).

### 4. Invariants and the findings they answer

Read [4. Invariants and the findings they answer](scanner-outcomes/invariants.md).

### 5. Complete finding map

Read [5. Complete finding map](scanner-outcomes/finding-map.md).

#### 5.1 Review passes 1-7 and audits

See [this section](scanner-outcomes/finding-map.md#51-review-passes-1-7-and-audits).

#### 5.2 Earlier TKT-399 plan critiques and coverage notes

See [this section](scanner-outcomes/finding-map.md#52-earlier-tkt-399-plan-critiques-and-coverage-notes).

#### 5.3 Accepted TKT-556 plan-review findings, round 3

See [this section](scanner-outcomes/finding-map.md#53-accepted-tkt-556-plan-review-findings-round-3).

<a id="6-tests-and-mutations-by-invariant-d11"></a>

### 6. Tests and mutations, by invariant, ADR-085 D11

Read [6. Tests and mutations, by invariant, ADR-085 D11](scanner-outcomes/tests-and-mutations.md).

### 7. F1: the old-tab barrier and the cost of each option

Read [7. F1: the old-tab barrier and the cost of each option](scanner-outcomes/storage-upgrade.md).

<a id="71-owner-selected-storage-shared-database-version-3-d9-and-d14-3"></a>

#### 7.1 Owner-selected storage: shared database, version 3, ADR-085 D9 and ADR-085 D14-3

See [this section](scanner-outcomes/storage-upgrade.md#71-owner-selected-storage-shared-database-version-3-d9-and-d14-3).

#### 7.2 A real option without bumping the existing database

See [this section](scanner-outcomes/storage-upgrade.md#72-a-real-option-without-bumping-the-existing-database).

<a id="73-do-not-turn-open-completion-into-permission-to-resume-a-scan-d10"></a>

#### 7.3 Do not turn open completion into permission to resume a scan, ADR-085 D10

See [this section](scanner-outcomes/storage-upgrade.md#73-do-not-turn-open-completion-into-permission-to-resume-a-scan-d10).

<a id="74-legacy-rows-need-an-explicit-classification-owner-d13-3"></a>

#### 7.4 Legacy rows need an explicit classification, owner ADR-085 D13-3

See [this section](scanner-outcomes/storage-upgrade.md#74-legacy-rows-need-an-explicit-classification-owner-d13-3).

<a id="8-delivery-split-and-merge-order-d19-d24-and-d25"></a>

### 8. Delivery split and merge order, ADR-085 D19, ADR-085 D24, and ADR-085 D25

Read [8. Delivery split and merge order, ADR-085 D19, ADR-085 D24, and ADR-085 D25](scanner-outcomes/delivery-plan.md).

### 9. Rules and decisions this ADR rests on

Read [9. Rules and decisions this ADR rests on](scanner-outcomes/decision-log.md).

#### 9.1 Owner rules

See [this section](scanner-outcomes/decision-log.md#91-owner-rules).

#### 9.2 Agent decisions

See [this section](scanner-outcomes/decision-log.md#92-agent-decisions).

## Consequences

- **Positive:**
    - Each local outcome and failure point has one written cell. A write failure cannot turn a known
      refusal, a live 401 or an unconfirmed answer into an offline admission.
    - Ticket 2 closes Entry P7-1 and the other section 1.3 gaps assigned to ticket 2, for new rows.
      TKT-559 supplies the Entry time checks. TKT-399 implements ticket 4 Exit.
    - Each invariant names its enforcing test tier and the mutation that must turn it red.
    - The delivery split lets the Entry core merge before time safety and Exit, ADR-085 D13-2.
- **Negative:**
    - W01 can turn an unsent attempt into an Access fact. A lost failed terminal write (P) and M02 can
      also give false facts. O01/O02 and transparent resend keep U. An ambiguous 500 can leave A.
      ACT can survive without an opening, H. An absent Exit can cause `exit_required`, E. Never
      reopen from ACT.
    - A 500, U02 or U03 stops a valid holder and needs an operator, where main queued an offline
      admission. Staff can force offline by cutting the network; that is a known workaround, not a
      guarantee.
    - Held true admissions can stay unsynced. The export contains credentials and can expose them. A
      browser cannot prove that a file was saved.
    - A rollback to the old bundle needs a compatible v3 reader. The upgrade waits for repairs, ADR-085 D27.
      The page holds its owner lock for its whole life, ADR-085 D3.
    - External generators, browser resend and rejection shapes, device sleep, paint and pgx dial
      behavior stay unverified. TKT-556 ran static document and source checks only. TKT-557 verified
      only the real-driver producers of its ticket-0 class ledger, section 8.

## References

- TKT-556 (this spike, decision log ADR-085 D1-ADR-085 D41); TKT-399 (thread, PR #451 closed, branch
  `TKT-399-pass-exit-mode` at `24f1c209`).
- TKT-551 (sync reporting delivered, see I7), TKT-552 (two Entry reason instructions, section 3.2), TKT-565 (remaining Entry reason copy), TKT-553 (unknown event-type test), TKT-500
  (revocation freshness ceiling and override).
- TKT-557 (ticket 0, the Access response-producer contract) and TKT-560 (load qualification of its
  PROVISIONAL bounds; a venue-use precondition after TKT-558, section 8).
- [ADR-025](./ADR-025-admission-events-and-offline-reconciliation.md) §3, amended by this ADR.
- [ADR-066](./ADR-066-scanner-voided-ticket-feed.md) §8, amended by this ADR.
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md), the trust boundary.
- [Operator guide: scanner](../development.md#scanner-device-enrolment-ai-review-s1) and
  [HTTP server configuration](../configuration.md#http-servers-services-and-gateway).
