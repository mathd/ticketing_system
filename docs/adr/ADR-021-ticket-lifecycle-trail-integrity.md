# ADR-021: The ticket lifecycle trail is chained per ticket and checkpointed per organizer

Date: 2026-07-15

## Status

Accepted (approved at the TKT-57 plan gate, 2026-07-15)

**Decision only — the trail is not tamper-evident until the implementation follow-up (TKT-67)
lands.** This ADR closes the open question recorded in [ADR-003](./ADR-003-append-only-audit-trail.md)
§Status; it does not close the gap. Until TKT-67 ships, `lifecycle_events` remains append-only by
policy (a trigger) and nothing in this ADR may be cited as evidence that it is tamper-evident.
No further lifecycle event types should be added ahead of TKT-67 — each one added first is one more
to backfill.

## Context

[ADR-003](./ADR-003-append-only-audit-trail.md) promises "two append-only trails, one discipline",
and the owner's directive (2026-07-12) puts tickets on equal footing with money. The two trails are
not equal:

| | Money journal | Ticket lifecycle trail |
|---|---|---|
| Hash chain | `previous_hash`/`entry_hash`, per organizer | none |
| Signature | HMAC-SHA-256 + `key_id` | none |
| Verifier | `verify-journal`, in the local gate | none |
| Only defence | triggers **and** the above | `lifecycle_events_immutable` trigger alone |

`grep -ciE 'hash|signature'` returns **4** in `services/payments/internal/store/migrations/0001_journal.sql`
and **0** in `services/access/internal/store/migrations/0001_tickets.sql`.

A trigger is DDL: `DROP TRIGGER` and the trail is plain mutable rows. A modified `lifecycle_events`
row leaves no cryptographic evidence, so the trail is *append-only by policy*, not tamper-evident by
construction.

**This is a fraud surface, not only an audit gap.** ADR-003 §Decision 2 makes redemption decisions
*from the trace* — duplicate detection at the gate reads `lifecycle_events` for a `redeemed` row
(`services/access/internal/store/postgres.go:202-214`) — deliberately, rather than from a mutable
flag. That design choice means there is **no independent read model to contradict a tampered
trail**. Editing the trace is editing the answer.

The constraint that shapes the decision: **the money journal's chain is per-organizer, serialized
under a `journal_heads` row lock** (`services/payments/internal/store/store.go:157`,
`SELECT … FOR UPDATE`). Redemption is the opposite workload. Doors open, thousands of scans arrive
at once, all for the same organizer, and `Postgres.Redeem`
(`services/access/internal/store/postgres.go:182`) today locks only the scanned ticket.

## Possible Solutions

- **Option 1 — copy the money journal: per-organizer chain, signature, verifier:**
    - Pros: literal "one discipline"; one commitment across all of an organizer's events makes
      cross-ticket gaps and insertion legible; rollback is loud, because it destroys every
      subsequent entry.
    - Cons: puts **every turnstile for an organizer behind a single row lock**, on the one path
      that most needs concurrency. Serializes unrelated scans at exactly the moment
      (doors-open) when the system is most contended. Rejected.
- **Option 2 — per-ticket chain only:**
    - Pros: shards naturally — a ticket's history is short and already serialized by the ticket row
      `Redeem` locks; no new contention; detects modification, insertion and reordering.
    - Cons: **leaves the named fraud open.** Un-redeeming one ticket is two row operations
      (delete the `redeemed` row, roll back that ticket's head) that nothing else in the database
      notices. See §Why Option 2 alone fails. Rejected as insufficient.
- **Option 3 — documented exception to "one discipline":**
    - Pros: no migration, signing, storage or scan-latency cost.
    - Cons: leaves a trace-derived *authorization* decision protected only by removable DDL. The QR
      signature authenticates the credential presented; it says nothing about whether a redemption
      row was deleted. Too weak against the owner's directive. Rejected.
- **Option 2+ — per-ticket chain + asynchronous organizer checkpoint (chosen):**
    - Pros: Option 2's contention profile with Option 1's rollback blast radius, at the cost of
      bounded detection latency off the hot path. Creates the seam TKT-11's external anchor plugs
      into.
    - Cons: one more moving part (an async job that can silently stop); detection lags by the
      checkpoint interval.

### Why Option 2 alone fails — the deferral does not transfer

[ADR-016](./ADR-016-checkout-recovery-state-machine.md) §Decision 7 defers **coordinated rollback**
(suffix truncation with the head rolled back, whole-organizer removal, snapshot restore) to TKT-11,
on the grounds that nothing inside the instance attests how long a chain should be.

That deferral was priced against a **per-organizer** chain, where rollback is expensive and wide: it
destroys every subsequent money entry, and receipts, acquirer records and projections all contradict
it. **At per-ticket granularity the same attack is surgical** — two row operations scoped to one
ticket, contradicted by nothing, because ADR-003 §Decision 2 removed the independent read model on
purpose. The result: silently un-redeem a ticket, walk in twice. That is precisely the fraud TKT-57
was opened for.

**A granularity change re-prices the deferral.** Inheriting ADR-016 §D7's exclusion verbatim across
that change would ship a scheme that detects everything *except* its own reason for existing.

## Decision

We adopt **a per-ticket hash chain on the write path, plus an asynchronous per-organizer
checkpoint chain off it**.

1. **Per-ticket chain.** Every lifecycle event carries a ticket-local `sequence`, `previous_hash`
   and `entry_hash` in a **companion integrity table** — `lifecycle_events` itself is immutable and
   is not rewritten. Each ticket has a head. Appends serialize on the ticket row `Redeem` already
   locks: **no organizer-wide lock is introduced, ever.** Lifecycle row and integrity row are
   written in the same transaction, and every verifier asserts one-to-one coverage, so bypassing the
   append path is visible.

2. **Asynchronous organizer checkpoint.** A periodic job commits a signed Merkle root over the
   per-ticket heads that **changed since the previous checkpoint**, chained to that checkpoint.
   Delta, not full scan: cost is proportional to activity, not to an organizer's total ticket count.
   Rolling back a ticket head now contradicts the checkpoint that last included it, so the attacker
   must also rewrite an organizer-wide chain — loud again, which is what made ADR-016 §D7's
   deferral defensible in the first place.

3. **The checkpoint interval is the fraud window — name it, don't default it.** A head rolled back
   *before* any checkpoint ever included it is undetectable. The window is therefore the interval
   itself, not a tunable nobody revisits. **Default: 60 seconds**, and it must be justified per
   deployment rather than inherited.

4. **Ed25519, under a key namespace separate from QR.** Reuse the keyring idiom already in
   `services/access/internal/ticket/token.go` (the `access-qr/<kid>=<key>` convention at
   `token.go:61-78`) under an **`access-lifecycle/`** namespace with distinct key material, so a
   leaked QR signing key does not also authorize rewriting history. Asymmetric is chosen over the
   money journal's HMAC deliberately: a third party can verify the trail without holding the power
   to write it — which is what an audit trail is for.

5. **Sign the head, not every entry — a deliberate divergence from the money journal.** Since
   `entry_hash_n = H(entry_hash_{n-1} ‖ canonical_n)`, a signed head already binds every prior
   entry: modify entry *k* and the recomputed chain no longer reaches the head. One signature verify
   plus *n* SHA-256 hashes detects exactly what *n* Ed25519 verifies would. Per-entry signatures buy
   only the ability to verify a fragment **without** the head — which is the undetectable case
   regardless. This asymmetry with payments is intentional; do not "fix" it back.

6. **Verification fails OPEN at the gate, with a high-severity alarm.** A chain that does not verify
   still admits, alarms, and marks the redemption for review. A verification failure is far likelier
   to be our own bug — key rotation, canonicalization drift, replication lag — than an attacker.
   Wrong in the fail-closed direction denies real customers at a live turnstile at the worst
   possible moment; wrong in the fail-open direction costs one possibly-duplicate entry that review
   catches. **The trail's job is to make tampering evident, not to make the door brittle.** This is
   safe *only because the alarm is load-bearing*: an unmonitored alarm turns this clause into a
   silent bypass.

7. **Offline verifier.** `access verify-lifecycle` verifies events, integrity rows, heads and the
   checkpoint chain using **public keys only**, and runs in the local gate against populated and
   deliberately corrupted data — mirroring what `scripts/smoke.sh:78-105` already does for
   `verify-journal`.

8. **Canonical form.** Versioned and domain-separated; binds ticket, order, organizer and slot
   identifiers, ticket-local sequence, and the lifecycle event's id, type and time. It carries
   **no PII and no `guest_order_ref`** (ADR-003 §Decision 3; ADR-012 makes the guest reference a
   no-store retrieval capability). Timestamp precision, ordering ties, UUID formatting and
   domain/version bytes are pinned by golden tests; changing any of them later requires an explicit
   canonical-version migration.

9. **Backfill.** Existing rows are backfilled into the companion table at startup, before listening,
   bounded and fail-fast per [ADR-008](./ADR-008-embedded-migrations.md), resumable after
   interruption. It **cannot prove legacy rows were honest** — QR credentials anchor ticket identity,
   not event history. Existing history is adopted as the baseline, and that limit is a property of
   the scheme, not a bug in it.

### Threat model

**Detected** — adversary with write access to the Access database but no lifecycle private key:
modification of any signed field; insertion or reordering within a ticket chain; a lifecycle row
with no integrity row, or the reverse; sequence gaps; broken chain links; unknown key ids; head
mismatch; and, via the checkpoint, **targeted rollback of an individual ticket's head**.

**Not detected:** rollback of the checkpoint chain itself (needs an attestation outside the
database — TKT-11, per ADR-016 §D7); validly signed writes from a compromised lifecycle key or a
controlled Access process; tampering predating the backfill baseline; and anything inside the
current checkpoint interval (§Decision 3).

**Not claimed:** this ADR makes no NF525 claim. ADR-003 §Decision 4's "layer on without schema or
flow changes" constraint is scoped to **the money journal** by its own wording and does not extend
to the lifecycle trail.

## Consequences

- **Positive:**
    - The trail becomes tamper-evident **by construction** rather than by policy, closing ADR-003's
      "one discipline" asymmetry — without importing the money journal's contention profile onto a
      turnstile.
    - Targeted per-ticket rollback, the fraud this ticket was opened for, becomes evident.
    - The checkpoint is the seam TKT-11 anchors externally; nothing else in this design changes when
      it lands.
    - Ed25519 lets an auditor verify the trail without holding the ability to forge it.
- **Negative:**
    - **A new async job that can silently stop.** If checkpointing dies, the trail quietly degrades
      to per-ticket-only and the fraud it closes reopens — with no symptom at the gate. Checkpoint
      freshness must be monitored; a stale checkpoint is an integrity alarm, not a shrug.
    - Fail-open is a real fraud window, accepted deliberately, and load-bearing on alarm routing.
    - **Organizer-wide event ordering and global ticket-set completeness are surrendered** in
      exchange for turnstile concurrency. Per-ticket heads do not attest how many tickets or chains
      should exist. This is the trade, not an omission.
    - Every scan gains a short chain read, hash recomputation, one signature verify, an integrity
      insert and a head update while holding the ticket lock — lengthening same-ticket contention,
      though never across tickets.
    - Long-lived passes (future entry/exit streams) grow online verification cost linearly;
      revisit with ticket-local signed checkpoints before that lifecycle ships.
    - Two signing idioms now coexist (payments HMAC, access Ed25519) and two key namespaces within
      Access, each with rotation and historical-key retention duties.

## References

- TKT-57 (this decision) · TKT-67 (implementation follow-up) · TKT-43 (money-path hardening, which
  deferred this) · TKT-11 (fiscal archive; owns the external anchor)
- [ADR-003 — Append-only audit trail](./ADR-003-append-only-audit-trail.md) (§Status gap closed here;
  §D2 trace-derived redemption; §D3 pseudonymity; §D4 NF525 scope)
- [ADR-016 — Checkout recovery state machine](./ADR-016-checkout-recovery-state-machine.md) (§D7
  threat model, re-argued here for per-ticket granularity)
- [ADR-002 — Services from day one](./ADR-002-services-from-day-one.md) (one database per service)
- [ADR-012 — Ticket issuance and QR credentials](./ADR-012-ticket-issuance-and-qr-credentials.md)
- [ADR-008 — Embedded migrations](./ADR-008-embedded-migrations.md) ·
  [ADR-020 — Non-concurrent index builds](./ADR-020-catalog-index-build-concurrency.md) ·
  [ADR-010 — PostgreSQL claim transaction](./ADR-010-postgres-claim-transaction.md)
