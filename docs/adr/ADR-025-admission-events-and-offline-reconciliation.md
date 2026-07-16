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
   chronologically second device timestamp. A connected duplicate that was **denied** is not an
   admission and produces no lifecycle event (today's behaviour, kept).
3. **Occurrence identity.** Every physical gate decision gets a gate-generated UUIDv4
   **occurrence id**, durably persisted by the scanner before the gate opens or the request is
   sent. Transport retries reuse it; a distinct physical decision gets a new one. The lifecycle
   event's existing `id` column stores it. The deterministic `ticketID+":redeemed"` id remains
   correct for the singleton `redeemed` type and existing rows are untouched.
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
   authoritative trace would have rejected, Access appends `duplicate_admit`, returns an explicit
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
   UNIQUE, then the old CHECK. Plain `CREATE UNIQUE INDEX`, never `CONCURRENTLY` (ADR-020);
   out-of-band migrate job under the 30-second fail-fast bound (ADR-022/ADR-008) — the index
   scans existing history, so the implementation ticket must measure the build at representative
   volume rather than assume it fits.
8. **Integrity sequencing.** New event types use the existing chained append path. No existing
   row, id, hash, signature or checkpoint is rewritten or re-backfilled, and canonical v1 is
   unchanged (see Context) — no gate identity or conflict reason enters the canonical form;
   reasons live in the alarm payload. Structured, signed gate provenance would be a
   canonical-version design and is out of scope.
9. **PII.** Occurrence ids and alarm payloads carry bounded identifiers and enums only — no
   buyer, no guest reference, no raw scanner-operator identity (ADR-003 §D3).
10. **Contracts.** Redemption emits no cross-service domain event today (verified: Access
    publishes exactly one subject, the ADR-021 integrity alarm, via its outbox). These additions
    are Access-local. The admission-conflict alarm starts as its own schema-1 contract and
    follows ADR-017 thereafter.

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
    - The trace's completeness claim is now explicitly bounded at reconciliation: a lost or
      never-synced gate occurrence is invisible, and the ADR says so rather than pretending
      otherwise.
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
