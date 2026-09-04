# ADR-025: Admission lifecycle events and offline reconciliation

Date: 2026-07-16

## Status

Accepted (TKT-73, plan gate 2026-07-16)

This is a **modelling decision**, not an implementation. Nothing here ships code: the migration
sketch and the occurrence-identity protocol below are **binding constraints on the follow-up
tickets** that implement them, recorded here so the decision cannot drift when they land. The
offline durability contract (§Decision 6) is a constraint handed to TKT-19, whose design owns how
it is met.

## Context

The lifecycle trail can physically record **at most one redemption per ticket, ever** — verified
at HEAD, not inferred:

- `services/access/internal/store/migrations/0001_tickets.sql` — `lifecycle_events` carries a
  table-wide `UNIQUE(ticket_id, event_type)`. `0002_redeemed_lifecycle.sql` widened only the
  `event_type` CHECK; the UNIQUE is untouched.
- `services/access/internal/store/postgres.go` (`Redeem`) — an existing `redeemed` row is
  idempotent success (`DecisionAlreadyRedeemed`, original timestamp returned); the event id is
  deterministic (`uuid.NewSHA1(uuid.NameSpaceOID, ticketID+":redeemed")`), i.e. designed for
  exactly-one.

Two product promises are therefore unrepresentable: **multi-entry passes** (PRD: park/season
passes, RFID wristbands; TKT-19 COS "passes follow their multi-entry semantics" — entry #2 cannot
be written) and **offline double-admits** (two gates admit the same ticket offline; reconciliation
writes one `redeemed` row and the second admission vanishes into the idempotent path — no evidence
it happened). [ADR-003](./ADR-003-append-only-audit-trail.md) §Decision 2 makes redemption
decisions *from the trace* precisely so duplicates are detectable; the trace could not express the
duplicate.

Two facts narrow the decision space, both established at the plan gate:

1. **[ADR-005](./ADR-005-unified-dated-slot-admission.md)'s accepted amendment already decides
   the pass vocabulary**: multi/count-limited entries are an append-only Access `entry`/`exit`
   stream, and the `UNIQUE(ticket_id,'redeemed')` single-redemption guarantee is retained for
   `redeemed` and simply not applied to `entry`/`exit`. Reopening that is not on the table here.
2. **[ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)'s chain has shipped (TKT-67)** —
   the ticket's original "decide before TKT-67 or re-backfill a signed trail" urgency is stale.
   `appendLifecycle` chains any genuinely-new event at append time, so a **new event type needs no
   backfill**; only rewriting or reinterpreting *existing* rows would pay the signed-trail cost,
   and nothing below does either. Likewise the canonical form is untouched: `Type` is a free
   string in the v1 event form (`services/access/internal/lifecycle/canonical.go`), so new event
   type *values* are not a canonical-version migration and the golden literals stand.

## Possible Solutions

- **Option A — `redeemed` becomes repeatable** (drop its UNIQUE, per-event ids, ordering
  semantics):
    - Pros: one admission vocabulary for everything.
    - Cons: reverses ADR-005's accepted decision; conflates entitlement consumption with physical
      admission; makes the "original redemption time" ambiguous; invalidates every reader that
      assumes a singleton redemption. Still needs per-occurrence identity to tell a transport
      retry from a second admission. Rejected.
- **Option B — distinct repeatable `entry`/`exit` events for passes; `redeemed` stays
  single-shot:**
    - Pros: already decided by ADR-005; represents re-entry and count-limited policy naturally;
      ADR-003 §D2's original enumeration anticipated "pass entry" as its own event type.
    - Cons: alone, it cannot represent an offline double-admit of a *single-entry* ticket.
- **Option C — a dedicated reconciliation-conflict event type (`duplicate_admit`), `redeemed`
  stays unique:**
    - Pros: the duplicate becomes representable without touching redemption semantics; online
      behaviour is unchanged.
    - Cons: alone, it does nothing for multi-entry passes; one more event type to reason about.
- **Option D — documented exception: offline double-admits write no trail:**
    - Pros: zero work now.
    - Cons: knowingly leaves a physical entitlement movement outside ADR-003's trace and makes
      "duplicate detection from the trace" false — the trail is now signed (TKT-67), and a signed
      record that cannot describe a double-admit invites misplaced trust. Rejected.

**Chosen: B + C.** B covers passes (and is already accepted); C covers single-entry offline
conflicts. Together every physical admission that Access learns about is representable.

## Decision

1. **Admission vocabulary.** `issued`, `delivered` and `redeemed` remain **singleton** event
   types. `entry`, `exit` (ADR-005) and `duplicate_admit` (new) are **repeatable**. A single-entry
   ticket has at most one `redeemed`, no `entry`/`exit`, and zero or more `duplicate_admit`
   events. A multi/count-limited ticket has zero `redeemed` and repeatable `entry`/`exit`.
2. **Representability.** A reconciled double-admit is one `redeemed` plus one `duplicate_admit`
   per additional distinct physical admission. `duplicate_admit` means *"a physical admission
   that conflicted with the authoritative trace at reconciliation"* — not necessarily the
   chronologically second device timestamp. **`duplicate_admit` is scoped to single-entry
   tickets**, where the classification is arrival-order-independent: a single-entry ticket has
   no legitimate second admission, so any occurrence beyond the one `redeemed` is a conflict no
   matter which device syncs first. **Pass (multi/count-limited) reconciliation never mints
   conflict events**: it records the factual `entry`/`exit` events only, and policy conflicts —
   e.g. ADR-005's `requires_exit` re-entry without a prior exit — are **derived projections**
   that re-evaluate as late events arrive. Sequence order is append order, and cross-device
   sync order is not physical order: an exit at gate A syncing after a re-entry at gate B would
   otherwise be baked into the immutable signed trail as a false `duplicate_admit` that no
   later event could retract. Derived conflicts are alarmed conservatively and marked
   revisable; an alarm can be withdrawn, an appended event cannot. A connected duplicate that was **denied** is not an
   admission and produces no lifecycle event (today's behaviour, kept). One admission class
   stays outside the lifecycle trace by prior decision: a **degraded admission** under ADR-021
   §Decision 6 is recorded only on the quarantine side — appending onto an unverified
   predecessor would poison the chain — so **authoritative admission history is the union of
   the lifecycle trace and the quarantine record**, and both readers *and admission decisions*
   must consult the union. For that union to actually be complete, **the quarantine-side
   admission record must be per-occurrence (repeatable)**: today's
   `lifecycle_integrity_quarantine` is one row per ticket, and a broken chain refuses appends —
   so two distinct offline admissions reconciled while the chain is invalid would leave the
   second recorded nowhere. Reconciliation of admissions that already physically happened is
   **recording, not deciding**: ADR-021 §D6's one-admission rule governs *live* scans, and it
   cannot retroactively deny an offline admission — every such occurrence Access learns about
   lands an occurrence-keyed quarantine-side record. The schema shape (extra table vs widened
   key) is the follow-up's; the per-occurrence requirement is decided here. The trace alone cannot express ADR-021 §D6's admit-once:
   today's verified redemption path checks only `lifecycle_events`
   (`services/access/internal/store/postgres.go`), so a ticket quarantine-admitted under a
   verifier bug would be admitted a *second* time once the chain verifies clean again — a
   pre-existing gap this ADR names and hands to a follow-up ticket, not new scope here.

   **Who the consumers are, and what each one asks (TKT-299).** "Both readers and admission
   decisions" was left to inference, and two consumers were reading the trace alone. They do
   not all ask the same question, which is why they do not all share one query:

   - **`SwitchExchange`'s source-ticket guard** (`ticketAdmitted`) and **reconciliation's
     prior-admission check** (`ReconcileAdmission`) ask *"has anyone been admitted on this
     ticket, by any route?"* Both were reading the trace alone. The exchange guard voided a
     ticket its holder had already entered on and issued a fresh unredeemed replacement — the
     double admission that guard exists to prevent — and reconciliation concluded "no prior
     admission" and minted a second admission record for one physical person, with no conflict
     alarm. Neither was caught by the `redeemed` singleton index, because an admission
     recorded quarantine-side leaves no `redeemed` row to collide with. These two now share
     one definition (`services/access/internal/store/admission.go`).
   - **`redeemSingle`** asks something narrower — *"has this ticket already taken its ONE §D6
     degraded admission?"* — and so it deliberately keys on `admitted_at IS NOT NULL` alone.
     A reconciliation-learned record must not turn a verified live scan into a denial.
   - **`admissionFacts`** asks *"what has physically happened on this pass?"*, and needs the
     facts themselves rather than a boolean, including `duplicate_admit` (see below).

   Three questions, three queries, one shared storage shape. The mistake to avoid is assuming
   the first question's answer serves the other two.

   Two distinctions that definition must keep, both load-bearing and both easy to lose.
   First, `duplicate_admit` marks an occurrence the record treats as a **conflict** rather than
   as this ticket's admission — reconciliation appends it for an offline occurrence arriving
   after the ticket was already admitted; a live denial appends nothing at all. It therefore
   does not make a ticket "admitted" for an admission *decision*: counting it would deny an
   exchange to someone whose second scan was correctly turned away. That is narrower than "it
   is never evidence of anything" — `admissionFacts` deliberately consumes a `duplicate_admit`
   as a physical entry when deriving **pass allowance**, because the person did walk in.

   Second, and this is the distinction that produced the incomplete first fix: on the
   quarantine side, **`admitted_at` says who decided, not whether anyone entered.** A row with
   `admitted_at` set is Access admitting under §D6 on a chain that did not verify. A row with
   `admitted_at` NULL and an admitting `event_type` is an **offline gate's admission**, learned
   later — the person is just as inside. So an admission *decision* must key on the
   `event_type`, and a predicate keyed on `admitted_at IS NOT NULL` silently excludes every
   offline admission. Only `redeemSingle`, asking its narrower §D6-specific question, may key
   on `admitted_at`.

   Scope, per [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md): the union is
   **honest-writer consistency, not tamper-evidence**. A writer with database access can
   insert or delete quarantine rows at will, and nothing here constrains that.
3. **Occurrence identity.** Every physical gate decision gets a gate-generated UUIDv4
   **occurrence id**, durably persisted by the scanner before the gate opens or the request is
   sent. Transport retries reuse it; a distinct physical decision gets a new one. The lifecycle
   event's existing `id` column stores it — **for every admission event type, including
   `redeemed`**. One identity model, no exceptions: if the occurrence selected as `redeemed`
   kept a deterministic id, its own retry could not be matched by event id and would be
   misrecorded as a `duplicate_admit` — a transport retry forged into evidence of a second
   physical admission. The deterministic `ticketID+":redeemed"` id is **grandfathered** on
   existing rows and remains in use only until the implementation ticket lands the occurrence
   protocol; the implementation must test that a retry of the occurrence that became `redeemed`
   is idempotent success and can never append `duplicate_admit`. **The identity rule extends to
   degraded admissions**: the quarantine row (today only `ticket_id, organizer_id, reason,
   admitted_at` — `0003_lifecycle_integrity.sql`) must also persist the occurrence id, and a
   retry of the *same* occurrence that took the one degraded admission returns idempotent
   success — the original result, no second admission, no second alarm — not the quarantine
   denial; only a *distinct* (or absent) occurrence id is denied. **Decision order is binding:
   occurrence identity is checked before the quarantine denial**, on both the degraded and the
   verified path, so ADR-021 §D6's "every subsequent scan is denied" reads as "every later
   distinct occurrence" (§D6 carries the matching qualification). Without this, a lost response
   to the degraded admit is indistinguishable from a second scan and a retry gets misdenied.
   The implementation must test broken-chain retry, recovered-chain retry, and a distinct
   occurrence, through both paths.

   **The occurrence id is an idempotency key, never admission authorization.** A replay-matched
   response is returned as an explicitly **distinguishable replay result** (e.g. a `replay`
   marker or its own decision value), never as a bare `accepted`. **Actuation is keyed on the
   originating scanner's own durable state, not on the response type alone**: the scanner
   persists a *pending* record for the occurrence before sending, and a response — first-time
   or replay — may actuate the gate **iff this device holds a durably pending, never-actuated
   record for that occurrence id**. That is what lets a genuine lost-response retry complete
   exactly once (the pending record is still un-actuated when the replay result arrives), while
   a captured `(qr_payload, occurrence_id)` pair replayed from any other device — which holds
   no pending record — never actuates: the admission-oracle hole stays closed without
   sacrificing the retry. The record is marked actuated **before** the gate opens
   (fail-closed): the irreducible hardware window is a crash *between marking and opening*,
   which strands one admission a person at the gate resolves with an operator — never a double
   actuation. Binding the occurrence id to an authenticated scanner identity is TKT-85's design
   space; the floor decided here is: distinguishable replay response + actuation gated on the
   local pending record + mark-before-open. Adversarial tests: a copied occurrence id from
   another scanner/session, and a repeated already-actuated occurrence, must not cause a second
   actuation — while a genuine lost-response retry still completes exactly once.

   **Pending records have a page owner and a renewable lease.** Each page load gets a fresh owner
   id. A real same-tab reload carries only the prior owner id through `sessionStorage`, so it can
   queue its interrupted request immediately without treating another live tab's request as its
   own. Every pending row also carries a 30-second lease which its live page renews every 10
   seconds. A different owner may queue the row only after that lease expires. This gives a tab
   without session storage, or a second tab left open after the first crashes, a bounded recovery
   path. Actuation still requires `PENDING`, `actuated:false`, and the current owner id in one
   IndexedDB transaction. Recovery can therefore make a request retryable, never gate-opening.
4. **Idempotency stays outside the append path.** Event-id replay is resolved *before*
   `appendLifecycle`, under the ticket lock — the append module is never invoked for an
   already-recorded occurrence (its documented contract at
   `services/access/internal/store/lifecycle.go`).
5. **Ordering.** The integrity `sequence` is the authoritative append/reconciliation order;
   `occurred_at` is the gate-recorded (claimed) physical time. Cross-device offline events may
   reconcile out of timestamp order; existing rows are never relabeled or reordered to impose
   device-clock order. Offline `occurred_at` is the device's persisted admission time; skew is
   validated at reconciliation, and device time is never claimed to be attested.
6. **Offline outcome and durability.** When reconciliation finds an offline admit the
   authoritative trace would have rejected (single-entry tickets only — §D2 scopes
   `duplicate_admit`; pass conflicts are derived projections), Access appends `duplicate_admit`, returns an explicit
   conflict result to the gate, and owes a durable **admission-conflict alarm** — a new
   operational alarm class, deliberately separate from ADR-021's integrity alarms because the
   chain is valid; the world disagreed with it. The scanner-side durability contract (occurrence
   committed to a local durable queue before gate-open) is a **constraint on TKT-19**; if the
   hardware cannot guarantee it, TKT-19 must narrow "every reconciled admission" explicitly and
   name the irreducible lost-admission window.
7. **Schema invariant.** Singleton uniqueness applies to `issued`, `delivered`, `redeemed` only —
   as a **partial** unique index, preserving ADR-012's unique `delivered`. Repeatable types are
   protected from retry duplication by the event primary key (the occurrence id). The migration
   order is binding on the implementation ticket: add the widened CHECK, create the plain partial
   unique index *while the table-wide UNIQUE still protects the table*, then drop the table-wide
   UNIQUE, then the old CHECK. The widened CHECK needs a **distinct constraint name** while the
   old one still exists (rename or drop-old afterwards), and `ADD CONSTRAINT … CHECK` **validates
   existing rows with its own full-table scan** — so the migration performs two scans (CHECK
   validation + index build), not one. Plain `CREATE UNIQUE INDEX`, never `CONCURRENTLY`
   (ADR-020); out-of-band migrate job under the 30-second fail-fast bound (ADR-022/ADR-008) —
   the implementation ticket must measure the **complete migration**, not the index alone, at
   representative volume. `NOT VALID` + `VALIDATE CONSTRAINT` is acceptable only if validation
   still completes inside the same job's bound; deferring validation past the job is not.
8. **Integrity sequencing.** New event types use the existing chained append path. No existing
   row, id, hash, signature or checkpoint is rewritten or re-backfilled, and canonical v1 is
   unchanged (see Context) — no gate identity or conflict reason enters the canonical form;
   reasons live in the alarm payload. Structured, signed gate provenance would be a
   canonical-version design and is out of scope.
9. **PII.** *Amended (TKT-119):* Occurrence ids and alarm payloads carry bounded identifiers,
   enums drawn from fixed vocabularies, operational scalars that are not themselves direct person
   identifiers (timestamps, counters, booleans, version numbers), and — on the integrity class
   only — a **service-produced diagnostic reason string**. They carry **no device- or user-supplied
   free text and no nested objects**, no buyer, no guest reference and no raw scanner-operator
   identity (ADR-003 §D3).

   This is a producer-schema constraint on honest application changes; it is **not** a privacy or
   non-linkability guarantee, and **not** containment against an adversary with write access to the
   Access database (ADR-021 §The trust boundary). Two specifics, because both have already caught
   this clause out:

   - `device_occurred_at` is device-*claimed* and correlates with a physical gate event. Bounded is
     not anonymous.
   - `reason` (`alarmData`) is the **one unbounded field** in any alarm payload: it is an internal
     error string (`cause.Error()`). Nothing enforces its content, so this clause is a **discipline
     on the producer** — a diagnostic reason must be built from this service's own errors and never
     from scanner, device or buyer input. Replacing it with a fixed reason-code vocabulary is the
     stronger fix and is a payload change (ADR-017 §3 / ADR-033), tracked separately.

   *Why this changed, stated as a delta rather than as a tidy-up.* The original wording —
   "bounded identifiers and enums **only**" — was never satisfied by any shipped payload. Six
   fields across all three classes fall outside "identifiers and enums": `alarmData.occurred_at`
   and `alarmData.reason`; `conflictAlarmData.device_occurred_at` and
   `conflictAlarmData.skew_flagged`; `policyConflictAlarmData.version` and
   `policyConflictAlarmData.revisable`. §D5 *requires* the device-claimed time, so the clause
   contradicted its own ADR.

   So this amendment is **not purely a correction — it is also a deliberate relaxation**, and
   recording it otherwise would let a future reviewer treat the reason exception and the scalar
   expansion as pre-existing policy instead of an accepted risk. Precisely:

   - **Preserved** — all three original identity exclusions: no buyer, no guest reference, no raw
     scanner-operator identity.
   - **Relaxed** — the word "only". Operational scalars (timestamps, counters, booleans, version
     numbers) and one free-text diagnostic `reason` were prohibited by it and are now admitted,
     because they already ship and §D5 mandates one of them.
   - **Preserved and made explicit** — no device- or user-supplied free text, and no nested
     objects. These are *not* new: "identifiers and enums only" already entailed them, since
     anything that is neither an identifier nor an enum was excluded. The original never
     enumerated them, so the amendment spells them out — they are the surviving boundary of the
     original rule, retained in full except for the single service-produced `reason` carve-out
     above, and they are what stop "operational scalars" from becoming a licence for arbitrary
     payload growth.

10. **Contracts.** The verified inventory at HEAD, NATS side: redemption emits **no**
    cross-service domain event; the lifecycle alarm outbox carries exactly one subject
    (`platform.access.lifecycle-integrity.alarm`); Access separately publishes
    `platform.access.ticket-issuance.failed` from its order-completed consumer
    (`services/access/internal/consumer/consumer.go`) — unrelated to admission and untouched
    here. The admission-conflict alarm is the one new NATS contract and starts as its own
    schema-1 contract, following ADR-017 thereafter.

    **HTTP side — the new event types are *not* merely Access-local rows.** Two public contracts
    move (`services/access/api/openapi.yaml`), and the follow-up tickets own an explicit
    rollout for each:
    - `GET /orders/{ref}/tickets` exposes each ticket's lifecycle history; `entry`, `exit` and
      `duplicate_admit` will appear in it (the `LifecycleEvent.type` field is a free string, so
      this is additive), and the follow-up must state whether quarantine admissions surface
      there or remain operator-only.
    - `POST /scans` accepts only `qr_payload` with `additionalProperties: false` — a scanner
      sending `occurrence_id` today is **rejected**. The occurrence protocol is therefore an
      expand/contract rollout: the server accepts the field first (optional), scanners adopt it,
      and only then may it become required. Old-scanner/new-server (no occurrence id: server
      falls back to today's semantics) and new-scanner/old-server (field rejected: scanner must
      tolerate and retry without it, losing occurrence idempotency, never admission safety) are
      both named states, not accidents.

## Claims and named adversaries

Per ADR-021 §The trust boundary, every detection claim names its adversary:

- **Database writer without the lifecycle private key** — once reconciled, modification,
  insertion or reordering of admission events is cryptographically evident. **Targeted rollback
  remains undetected** (still TKT-11's).
- **Current-key compromise / controlled Access process** — the chain cannot establish truth;
  validly re-signed history remains possible. Unchanged by this ADR.
- **Our own bugs** — partial uniqueness prevents a second singleton event; occurrence ids make
  transport retry idempotent. The chain will still faithfully sign a *misclassified* or invented
  occurrence — it proves append integrity, not classification correctness.
- **Offline-gate skew** — reconciliation makes a double-admit *visible*; nothing here prevents
  the physical admission that already happened.
- **A gate that never syncs** — an occurrence Access never received is not represented. The
  trace claim begins at reconciliation.
- **Gate clock / compromised scanner** — `occurred_at` is a claimed time, not proof.

The admission-conflict alarm bounds operational skew and our bugs; it is visibility, not
containment against the database adversary.

## Consequences

- **Positive:**
    - Multi-entry passes and offline double-admits become representable without touching
      redemption semantics, existing rows, or the canonical form; ADR-003 §D2's
      "duplicate detection from the trace" becomes true rather than aspirational.
    - Online single-entry behaviour — including the idempotent duplicate-scan answer — is
      unchanged; no reader breaks before the implementation ticket chooses to change it.
- **Negative:**
    - Two admission vocabularies coexist (`redeemed` vs `entry`/`exit`), plus a third event type
      for conflicts — readers must dispatch on event type.
    - The trace's completeness claim is now explicitly bounded: a lost or never-synced gate
      occurrence is invisible, and ADR-021 §D6 degraded admissions live only in the quarantine
      record — the ADR says so rather than pretending otherwise, and admission readers must
      consult trace ∪ quarantine.
    - The implementation ticket inherits a measured-migration obligation (partial index build
      under the 30s bound) and a scanner durability contract it cannot silently weaken.

## References

- TKT-73 (this decision), TKT-67 (shipped chain), TKT-19 / TKT-16 (passes, offline — consumers of
  this decision), TKT-11 (external anchor; rollback stays open)
- [ADR-003](./ADR-003-append-only-audit-trail.md) §Decision 2 — amended by this ADR
- [ADR-005](./ADR-005-unified-dated-slot-admission.md) — `entry`/`exit` decided there, not here
- [ADR-012](./ADR-012-ticket-issuance-and-qr-credentials.md) — unique `delivered`, preserved
- [ADR-017](./ADR-017-domain-event-schema-evolution.md) — governs the alarm contract's evolution
- [ADR-020](./ADR-020-catalog-index-build-concurrency.md), [ADR-022](./ADR-022-out-of-band-service-migrations.md) — migration constraints
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) — trust boundary, append path, backfill
