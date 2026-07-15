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
    - Pros: Option 2's contention profile, while **raising the cost** of targeted rollback (the
      attacker must also truncate the organizer's checkpoint chain and evade freshness monitoring)
      and creating the seam TKT-11's external anchor plugs into — which is what actually closes the
      class.
    - Cons: one more moving part (an async job that can silently stop); detection lags by the
      checkpoint interval. **It does not make targeted rollback cryptographically detectable** —
      only more expensive and, conditional on monitoring, visible as staleness (§Decision 2). This
      is a tripwire bolted onto Option 2, not Option 1's blast radius recovered.

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

**But be honest about how far Option 2+ gets.** The checkpoint does **not** buy back cryptographic
detection of this attack — §Decision 2 works through why, and an earlier draft of this very ADR
claimed otherwise and was wrong. Coordinated ticket-plus-checkpoint rollback remains undetected;
only its *cost* rises and its *staleness* becomes visible to monitoring. So Option 2+ is chosen not
because it closes the named fraud — **nothing short of TKT-11's external anchor does** — but
because, at equal contention cost to Option 2, it raises the attacker's bar and builds the one
structure the anchor can later attest. Option 2 is rejected for offering neither. **Until TKT-11
lands, this trail is tamper-evident against modification and insertion, and merely
tamper-*expensive* against targeted rollback.**

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

   **What this does and does not buy — stated precisely, because the obvious claim is false.**
   It is tempting to say "rolling back a ticket head now contradicts the checkpoint that included
   it, so the attacker must rewrite an organizer-wide chain — loud again." **That is wrong, and no
   reader should repeat it.** An adversary with database write access does not *rewrite* the
   checkpoint chain; they **truncate** it — delete the suffix starting with the root that first
   committed the rolled-back head. Because checkpoints are deltas, that suffix does not drag other
   tickets' heads back with it: those heads survive, merely uncommitted, and an uncommitted recent
   head is **indistinguishable from ordinary activity inside the current interval**. The truncated
   chain is internally consistent, and a naive signer will happily extend it — laundering the
   rolled-back state under a fresh valid signature. Checkpoint-chain truncation is exactly the
   coordinated-rollback class this ADR concedes is undetectable without an external anchor
   (§Threat model), so **the checkpoint does not make targeted rollback cryptographically
   detectable.** Claiming otherwise would make this ADR self-contradicting.

   **What it actually buys**, which is still worth the moving part:
   - **Cost.** Targeted rollback goes from *two row operations with zero collateral and zero signal*
     to *additionally truncating the organizer's checkpoint chain*, destroying the commitment for
     every ticket touched since that root, and evading freshness monitoring.
   - **A liveness signal, not a proof.** A truncated chain is a **stale** chain. Detection is
     therefore a freshness alarm — real, but conditional on monitoring actually working, and
     categorically weaker than the hash chain's cryptographic evidence. It is a tripwire, not a lock.
   - **The seam.** Anchoring checkpoint roots externally (TKT-11) is what *actually* closes this
     class. The checkpoint is the thing TKT-11 anchors; without it there is nothing anchorable
     short of anchoring every ticket head.

   **The signer must refuse to launder a rollback.** The checkpoint signer must not extend a chain
   whose head has regressed relative to the last root it observed, and must refuse to sign and
   escalate instead. That memory has to live somewhere the database adversary does not control —
   otherwise the check is circular and buys nothing. Pinning *where* is TKT-67's, and it is a
   prerequisite, not a nicety.

3. **The checkpoint interval is the fraud window — name it, don't default it.** A head rolled back
   *before* any checkpoint ever included it is undetectable even as a freshness signal. The window
   is therefore the interval itself, not a tunable nobody revisits. **Default: 60 seconds**, and it
   must be justified per deployment rather than inherited.

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

   **The equivalence is scoped to the database-write / no-private-key adversary, and only that
   one.** Under **key compromise the two are not equivalent**, and the difference is not academic:
   with head-only signing, an attacker holding the *current* lifecycle key can rewrite a ticket's
   entire history and sign a fresh head, and it verifies. Per-entry signatures made under a
   **retired and destroyed** key would have preserved those earlier entries, because the attacker
   cannot forge signatures under a key that no longer exists. Head-only signing therefore has a
   materially larger compromise blast radius — which matters precisely because §Decision 4 mandates
   rotation and historical-key retention.

   **Mitigation — epoch signatures, at rotation granularity, not per entry.** On every key
   rotation, the outgoing key signs the ticket head as it stands, and that **epoch signature is
   retained**. A later compromise of the new key then cannot rewrite pre-rotation history without
   breaking an epoch signature made under a key it does not hold. This recovers most of per-entry
   signing's compromise containment at one signature per rotation rather than one per entry. The
   head signature must bind ticket identity, ticket-local sequence, canonical version and key id —
   not the head hash alone.

6. **Verification fails OPEN at the gate, with a high-severity alarm — inside a bounded degraded
   mode.** A chain that does not verify still admits, alarms, and marks the redemption for review.
   The reason is asymmetric likelihood: a verification failure is far likelier to be our own bug —
   key rotation, canonicalization drift, replication lag — than an attacker. Wrong in the
   fail-closed direction denies real customers at a live turnstile at the worst possible moment.
   **The trail's job is to make tampering evident, not to make the door brittle.**

   **Priced honestly: against an adversary, fail-open is unbounded, not "one duplicate entry".**
   Fail-open means an attacker does not even need a clean rollback — crudely deleting a `redeemed`
   row breaks the chain, and a broken chain *admits*. The alarm fires, but the person is already
   inside, and an adversary with database write access can do this to **many** tickets, repeatedly.
   Review happens after physical admission and cannot recover the entry. **An alarm is visibility,
   not containment**, and this clause is therefore only defensible with the bounds below — which
   are part of the decision, not implementation detail:

   - **Quarantine.** A ticket whose chain fails verification is admitted **once** and quarantined:
     every subsequent scan of that ticket is denied and escalated. This bounds the adversary to one
     extra entry *per corrupted ticket* instead of unlimited reuse of one.
   - **Threshold escalation.** Integrity failures above a configured rate stop being "our bug" and
     start being an attack. Crossing the threshold flips the organizer into an
     **operator-controlled** mode — the choice to keep admitting is then a human's, made knowingly,
     not a default the system takes silently at 3am.
   - **Alarm and response SLOs.** Fail-open is safe only while the alarm reaches someone who acts.
     Unrouted, this clause is a silent bypass with extra steps. TKT-67 owns routing; an unmonitored
     deployment must not run this scheme in fail-open.

   The residual is deliberate and named: within the threshold, an adversary buys one entry per
   ticket they corrupt, in exchange for never denying a legitimate customer over our own bug.

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

Scoped to the **database-write adversary who does not hold a lifecycle private key**. Read
§Decision 5 for how key compromise changes this.

**Detected cryptographically** — modification of any signed field; insertion or reordering within a
ticket chain; a lifecycle row with no integrity row, or the reverse; sequence gaps; broken chain
links; unknown key ids; head mismatch.

**Detected only as a liveness signal, not cryptographically** — targeted rollback of an individual
ticket's head *coordinated with truncation of the organizer's checkpoint chain*. The truncated
chain is internally consistent and verifies; what betrays it is **staleness**, and only if
freshness monitoring works and the signer refuses to extend a regressed chain (§Decision 2). Do not
record this as detection. It is a tripwire.

**Not detected:** rollback of the checkpoint chain itself, which is the coordinated-rollback class
deferred to TKT-11 per ADR-016 §D7 and needs an attestation *outside* the database; validly signed
writes from a compromised lifecycle key or a controlled Access process — including, under head-only
signing, rewriting a ticket's whole history within the current key epoch (§Decision 5); tampering
predating the backfill baseline; and anything inside the current checkpoint interval (§Decision 3).

**Not prevented, by construction:** admission itself. Fail-open (§Decision 6) means detected
corruption still admits, bounded by quarantine and threshold escalation. This scheme produces
*evidence*, not *refusal* — with the single exception of a quarantined ticket's second scan.

**Not claimed:** this ADR makes no NF525 claim. ADR-003 §Decision 4's "layer on without schema or
flow changes" constraint is scoped to **the money journal** by its own wording and does not extend
to the lifecycle trail.

## Consequences

- **Positive:**
    - The trail becomes tamper-evident **by construction** rather than by policy for modification,
      insertion and reordering, closing ADR-003's "one discipline" asymmetry — without importing the
      money journal's contention profile onto a turnstile.
    - Targeted per-ticket rollback becomes **expensive and stale-visible** rather than free and
      silent. It does **not** become cryptographically evident; TKT-11's anchor is what would do
      that (§Decision 2).
    - The checkpoint is the seam TKT-11 anchors externally; nothing else in this design changes when
      it lands.
    - Ed25519 lets an auditor verify the trail without holding the ability to forge it.
- **Negative:**
    - **A new async job that can silently stop.** If checkpointing dies, the trail quietly degrades
      to per-ticket-only — with no symptom at the gate. Checkpoint freshness must be monitored; a
      stale checkpoint is an integrity alarm, not a shrug. Note the sharp edge: **freshness
      monitoring is not a nice-to-have here, it is the entire detection mechanism** for targeted
      rollback (§Decision 2). Unmonitored, the checkpoint buys cost and nothing else.
    - **Fail-open is unbounded against an adversary** and is only made tolerable by quarantine and
      threshold escalation (§Decision 6). Accepted deliberately, on the judgement that our own bugs
      are likelier than attackers and denying real customers at a live turnstile is the worse
      failure — but it is a fraud window, not a rounding error.
    - **Two detection tiers now coexist and will be confused.** Modification/insertion are caught
      cryptographically; targeted rollback is caught, if at all, by a liveness alarm. Anyone citing
      "the trail is tamper-evident" without saying against *what* will overstate this design.
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
