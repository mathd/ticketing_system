# ADR-021: The ticket lifecycle trail is chained per ticket and checkpointed per organizer

Date: 2026-07-15

## Status

Accepted (approved at the TKT-57 plan gate, 2026-07-15) · **Implemented by TKT-67, 2026-07-15**

Amended by **TKT-234** (2026-09-01, §Amendment below) — states, per trail, what fixes order versus
what merely displays it: access's per-ticket order is fixed by the chain and `occurred_at` is a
claim, while inventory's `claim_history` has no chain, so its sort key *is* its ordering guarantee
and it is honest-writer consistency rather than tamper-evidence. No code changes; TKT-295 owns the
inventory half.

**Implemented.** `lifecycle_events` is now chained per ticket in a companion integrity table with
signed heads, checkpointed per organizer, and verified by `access verify-lifecycle` in the local
gate against populated *and* deliberately corrupted data. This ADR closed the open question
recorded in [ADR-003](./ADR-003-append-only-audit-trail.md) §Status; TKT-67 closed the gap — **the
part of it that is closable inside this database.**

Implementation: `services/access/internal/lifecycle/` (canonical forms, keyring),
`services/access/internal/store/lifecycle*.go` (append, verify, checkpoint, backfill),
`services/access/internal/lifecyclejob/` (workers), migration `0003_lifecycle_integrity.sql`.

**What shipped is exactly what §The trust boundary scopes, and no more:** tamper-evident against
modification, insertion and reordering, for an adversary who cannot re-sign the chain. **Targeted
rollback and current-key compromise remain open**, and no in-database mechanism reaches them; they
are TKT-11's. The rollback gap is pinned by a test that fails if it ever silently changes
(`TestVerifyLifecycleAcceptsACoordinatedRollback`) — it asserts that a coordinated rollback
verifies *clean*, so the limitation cannot rot into a false claim unnoticed.

**One clause needed correcting during implementation, and it is the fourth time this document has
described a control by its intent rather than its reach:** §Decision 6's alarm-routing requirement
is amended below — TKT-67 delivered routing and retention, but **monitoring is a deployment
obligation no boot check can discharge**, and the repository ships no consumer for the alarm
durable. §The trust boundary catalogues three earlier instances; this is the fourth, and it was
caught by review rather than by the author.

Two TKT-67 decisions this ADR left open, now settled:
- **§Decision 2's signer memory:** no durable attacker-independent location exists in this repo, so
  **no freshness tripwire exists until TKT-11**. The worker keeps its last observed root in process
  memory and refuses to extend a regression it can see, which stops it laundering one under a fresh
  signature while it is watching. It dies with the process and is not detection.
- **§Decision 9's backfill:** it does **not** fit the migrate job. It runs as its own resumable
  one-shot job (`access-lifecycle-backfill`) outside ADR-008's 30-second deadline.

New lifecycle event types are no longer blocked, but each one added is one more the chain covers
from its first write — add them through the append path, never straight into `lifecycle_events`.

**And when TKT-67 does land, it closes *modification and insertion* — not targeted rollback.**
Rollback needs an attestation outside the database (TKT-11); no in-database mechanism reaches it,
and this ADR's own §Decision 2 works through why. §The trust boundary is the section to read
first — three separate clauses of this ADR claimed protection it could not deliver, across two
adversarial review passes, and the boundary is what each of them crossed. **The scheme's scope is
therefore: tamper-evident against an adversary who cannot re-sign the chain; silent against one who
can roll it back wholesale.** Cite it that way or not at all.

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
    - Cons: **leaves the named fraud open** — un-redeeming one ticket is two row operations that
      nothing else in the database notices — **and leaves TKT-11 nothing affordable to anchor**,
      forcing a retrofit onto a populated trail. Note the chosen option does not close that fraud
      either; see §Why Option 2 alone fails. Rejected on build order, not on protection.
- **Option 3 — documented exception to "one discipline":**
    - Pros: no migration, signing, storage or scan-latency cost.
    - Cons: leaves a trace-derived *authorization* decision protected only by removable DDL. The QR
      signature authenticates the credential presented; it says nothing about whether a redemption
      row was deleted. Too weak against the owner's directive. Rejected.
- **Option 2+ — per-ticket chain + asynchronous organizer checkpoint (chosen):**
    - Pros: Option 2's contention profile and identical modification/insertion guarantees, **plus
      the one structure that makes TKT-11's external anchor affordable** — a single root per
      interval to attest, rather than one per ticket head.
    - Cons: one more moving part (an async job that can silently stop). **It buys no rollback
      detection today** (§Decision 2): truncating the checkpoint suffix is one more `DELETE`, and
      the staleness tripwire is a property of an external continuity witness this repo does not yet
      have. Chosen as **scaffolding**, honestly labelled — not as a control.

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

**But Option 2+ does not fix it either — and two review passes were needed to stop this document
pretending otherwise.** The checkpoint buys **no** cryptographic detection of this attack, and
calling it "expensive" was the same overclaim shrunk: the extra attacker work is one `DELETE`
(§Decision 2). Coordinated ticket-plus-checkpoint rollback is simply **undetected**, exactly as
ADR-016 §D7 says, and it stays that way until TKT-11 puts an attestation outside the database.

So why choose Option 2+ over Option 2 at all? **Not for protection — for affordability of the
protection that eventually arrives.** The anchor must attest something; a delta checkpoint root is
one small value per interval that transitively commits every head changed in it, where per-ticket
heads would demand one attestation each. Option 2 leaves TKT-11 nothing anchorable at sane cost and
forces this structure to be retrofitted onto a populated trail. That is the entire case, and it is
enough — **but it is a build-order argument, not a security one, and must never again be dressed up
as the latter.**

**The honest scope, until TKT-11: tamper-evident against modification and insertion; silent against
targeted rollback.** Option 2 is rejected because it makes rollback *equally* silent while making
the eventual fix more expensive.

## The trust boundary — read this before any clause below

**Everything in this ADR rests on one line, and two adversarial review passes found this ADR
crossing it three times. Do not cross it again.**

The adversary is defined as **someone with write access to the Access database**. It follows, with
no exceptions, that **any state living in that database is state the adversary owns.** A control
whose evidence, counter, signature or memory is a row in Access cannot constrain an adversary who
can write rows in Access. It can be deleted, reset, or forged wholesale.

That rules out, as *adversarial* controls:

- **The chain and heads themselves** against rollback — a consistently truncated chain verifies.
- **Quarantine records and threshold counters** (§Decision 6) — deletable between scans.
- **Retained epoch signatures** (§Decision 5) — deletable, and §Consequences surrenders the global
  completeness that would prove one *should* exist.
- **The checkpoint chain** (§Decision 2) — truncatable, and a signer whose memory of the last root
  is also in the database will re-bless the result.

What *does* constrain that adversary is a hash chain over data they cannot re-sign (which is why
modification and insertion **are** genuinely closed here), and **state held outside their reach**.
This database has no such outside today. **Creating it is TKT-11's job** — an attestation outside
the instance. Every protection in this ADR above "modification and insertion" is therefore either
**contingent on TKT-11**, or **scoped to the non-adversarial case** (our own bugs), and each clause
below says which. None of them may be cited as adversarial containment before TKT-11 lands.

The honest summary, and the one to quote: **this design closes modification and insertion now, and
is the scaffolding that lets TKT-11 close the rest. It does not close targeted rollback, and no
in-database mechanism can.**

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

   **What it actually buys — and it is not "cost".** A second review pass killed that framing too:
   truncating the checkpoint suffix is **one more `DELETE`**, and destroyed commitments produce no
   evidence by themselves. Calling that "expensive" was the same overclaim in a smaller font. The
   checkpoint's honest value is exactly one thing:

   - **It is the scaffolding TKT-11 anchors.** An external attestation has to attest *something*.
     Anchoring per-ticket heads directly is unaffordable — one attestation per ticket. A delta
     checkpoint root is a single small value per interval that transitively commits every head
     changed in it, so anchoring becomes one attestation per interval regardless of volume. Without
     this structure there is nothing anchorable at sane cost, and TKT-11 would have to invent it.
     **We build it now because the alternative is building it later and backfilling it.**

   A **freshness tripwire** exists *only if* the signer refuses to extend a chain whose head
   regressed against a last-observed root **held outside the database** (see §The trust boundary).
   If that external memory exists, the detection comes from **it** — an independent continuity
   witness — and not from the checkpoint. If it does not, the signer launders the rollback and
   freshness simply resumes. So freshness is a property of the witness, and this ADR does not
   pretend the checkpoint supplies one. **TKT-67 must either specify a durable,
   attacker-independent location for the signer's last-observed root, or state plainly that no
   tripwire exists until TKT-11.** It may not leave this undecided — that ambiguity is what made
   two drafts of this clause wrong.

3. **The checkpoint interval bounds what an anchor can ever cover — name it, don't default it.**
   A head rolled back *before* any checkpoint included it was never committed to anything, so no
   anchor and no witness can ever speak to it. The interval is therefore a **permanent** hole in
   what TKT-11 will eventually be able to attest, not a tunable nobody revisits: shortening it later
   does not retroactively cover it. **Default: 60 seconds**, justified per deployment rather than
   inherited. (Note the interval is *not* today's fraud window — §Decision 2 and §Threat model are
   blunt that targeted rollback is undetected at **any** interval until the external witness exists.
   The interval sets the floor on what that witness will be worth, which is why it is decided here
   rather than left to TKT-67.)

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

   **Partial mitigation — epoch signatures, at rotation granularity, and they do NOT close it.**
   On every key rotation, the outgoing key signs the ticket head as it stands and that epoch
   signature is retained. A later compromise of the new key cannot *forge* a signature under a
   retired, destroyed key. **But it does not need to: it can delete the epoch signature.** Retained
   where? In Access — which §The trust boundary says the adversary owns. And because this ADR
   surrenders global ticket-set completeness (§Consequences), **no rule says an epoch signature
   must exist**, so a verifier cannot distinguish a deleted one from a ticket that legitimately has
   none. Closing that needs a signed completeness manifest, retained externally, binding every
   applicable ticket to its epoch head at each rotation — which is TKT-11's trust domain again.

   So the honest claim: epoch signatures **raise the work** of a current-key compromise from
   "re-sign a head" to "re-sign a head and delete the epoch rows", and they become **real
   containment only once the rotation manifest is externally retained**. Until then, a compromised
   current key plus database write access can rewrite a ticket's entire history. That is listed as
   undetected in §Threat model, and this clause does not walk it back. The head signature must bind
   ticket identity, ticket-local sequence, canonical version and key id — not the head hash alone.

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
     every subsequent scan of that ticket is denied and escalated. *(Qualified by
     [ADR-025](./ADR-025-admission-events-and-offline-reconciliation.md) §D3 once occurrence
     identity ships: "every subsequent scan" means every later **distinct occurrence** — a
     lost-response retry carrying the occurrence id that took the one admission returns a
     **distinguishable replay result** — the occurrence id is an idempotency key, never
     admission authorization; actuation is keyed on the originating scanner's durable pending
     record, so that retry completes exactly once while a copied occurrence id from another
     device never actuates — without a second admission or a second alarm. And the denial must
     hold on the **verified** path too, not only inside the degraded path — TKT-89.)*
   - **Threshold escalation.** Integrity failures above a configured rate stop being "our bug" and
     start being an attack. Crossing the threshold flips the organizer into an
     **operator-controlled** mode — the choice to keep admitting is then a human's, made knowingly,
     not a default the system takes silently at 3am.
   - **Alarm and response SLOs.** Fail-open is safe only while the alarm reaches someone who acts.
     Unrouted, this clause is a silent bypass with extra steps. TKT-67 owns routing; an unmonitored
     deployment must not run this scheme in fail-open.

     **Amended 2026-07-15 (TKT-67, PR #51 review): TKT-67 delivered routing and retention. It did
     NOT deliver monitoring, and this clause was written as though the two were the same thing.**
     Alarms are committed to an outbox with the decision and published to a durable the service
     refuses to boot without — so an alarm is never dropped, and an operator inbox provably exists.
     But **the repository ships no consumer for that durable**, so the default deployment satisfies
     every check here and is still unmonitored. That is not an implementation gap to be closed
     later: **no in-process check can prove a human will act on a page.** "An unmonitored deployment
     must not run this scheme in fail-open" is therefore a **deployment obligation**, enforceable by
     whoever operates the system, not by this code — and it must be written that way rather than
     asserted as though the service enforced it.

     The one thing the system *can* say: a durable nobody drains accumulates. TKT-67 exposes
     `access.lifecycle.alarm.durable_pending`, and sustained non-zero is the closest observable proxy
     for "nobody is reading". Alert on it, along with `…alarm.dead_lettered` — every dead letter is a
     degraded admission that will never be reported.

     This correction is the same failure mode §The trust boundary catalogues, in a new place: a
     control described by the job it was *meant* to do rather than the one it *does*. That makes
     four.

   **These bound our bugs. They do not bound the adversary — do not confuse the two.** Quarantine
   records and failure counters are rows in Access, so the database-write adversary who caused the
   verification failure deletes the quarantine between scans and resets the counter below the
   threshold (§The trust boundary). Against *that* adversary, **fail-open is unbounded, full stop**,
   and these controls are not containment. Their real value is the far more likely case: a
   canonicalization bug or a botched rotation corrupts many chains at once, and quarantine plus
   threshold escalation stop that from silently becoming free re-entry while keeping the doors open.
   Bounding the adversarial case needs the controls' state in an attacker-independent domain —
   TKT-11's, again — and TKT-67 must not imply otherwise in code comments or dashboards.

   The residual, named plainly: **an adversary with database write access can walk in, repeatedly,
   and this design does not stop them — it only makes their edits to the trail evident where the
   chain covers them.** We accept that in exchange for never denying a legitimate customer over our
   own bug. Anyone who finds that trade wrong should reopen §Decision 6, not quietly flip the
   default.

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

9. **Backfill.** Existing rows are backfilled into the companion table **out-of-band, in the
   service's one-shot `migrate` job**, bounded and fail-fast, resumable after interruption. It
   **cannot prove legacy rows were honest** — QR credentials anchor ticket identity, not event
   history. Existing history is adopted as the baseline, and that limit is a property of the scheme,
   not a bug in it.

   **Amended 2026-07-15 (TKT-66/ADR-022), same day this ADR landed.** The clause originally read
   *"backfilled at startup, before listening, per ADR-008"*. [ADR-022](./ADR-022-out-of-band-service-migrations.md)
   superseded ADR-008 **on placement** hours later: migrations no longer run on the server path, so
   there is no "before listening" left to run in. ADR-008's *other* rulings — goose as a library,
   `embed.FS`, per-service schema ownership, fail-fast under a **30-second deadline** — still stand
   and still bind this backfill.

   **This is not a search-and-replace, and TKT-67 must not treat it as one.** The 30-second deadline
   was survivable when it bounded a service's own boot; it now bounds a **one-shot job the service
   `depends_on` with `service_completed_successfully`**, so a backfill that exceeds it fails the job
   and the service never starts at all. This backfill is **not** pure DDL — it Ed25519-signs every
   existing lifecycle row, so its cost scales with history and it is precisely the kind of work that
   blows a 30-second budget on a populated database. TKT-67 owns deciding whether it fits, or whether
   it needs its own resumable job outside the migrate deadline. Do not assume it fits.

## Amendment (2026-09-01, TKT-234) — what fixes a trail's order, per trail, and what merely displays it

TKT-230's third review pass raised a [high] finding: append-only trails `ORDER BY occurred_at`,
making a non-monotonic system clock the leading sort key of an audit trail. A backward step — NTP
correction, VM migration, a manual `date -s` — can make a later append sort before an earlier one.

That reading is **correct about the SQL and wrong about the guarantee**, and the difference is
entirely the presence or absence of a chain. This amendment states it per trail, because the two
trails this platform runs are not in the same position and treating them alike would either
overstate one or understate the other. **Nothing in the code changes.** §Threat model already asserts
that reordering within a ticket chain is detected cryptographically; this makes the consequence for
the *sort key* explicit, and says plainly where no such consequence is available.

### Access `lifecycle_events` — the chain fixes the order; `occurred_at` is a claim

| | |
|---|---|
| **What fixes order** | `lifecycle_event_integrity.sequence` — `bigint NOT NULL CHECK (sequence > 0)`, `UNIQUE (ticket_id, sequence)`, linked by `previous_hash`/`entry_hash` and terminated by a signed head (`0003_lifecycle_integrity.sql:23-34`). Assigned as `head.sequence + 1` at append. |
| **What displays** | `occurred_at` — the gate's *claimed* physical time. It is inside the signed canonical form, so it cannot be modified undetected; it is **not** the ordering authority. |
| **Adversary** | A database writer without a lifecycle private key cannot modify or reorder signed history undetected (§Threat model, unchanged). |

**So a clock step moves the display and not the guarantee.** Two events appended in one order and
timestamped in the other are both *honestly signed*; the chain records the order they were appended
in, and verification is indifferent to whether their timestamps ascend. `History` already sorts by
`i.sequence NULLS LAST, e.occurred_at, e.id` (`postgres.go:237`) — sequence first — and
[ADR-025](./ADR-025-admission-events-and-offline-reconciliation.md) §Decision 5 states the rule
verbatim: *"The integrity `sequence` is the authoritative append/reconciliation order; `occurred_at`
is the gate-recorded (claimed) physical time. Cross-device offline events may reconcile out of
timestamp order; existing rows are never relabeled or reordered to impose device-clock order."*
This amendment does not invent that; it records what the *chain* therefore guarantees about it.

Pinned by `TestHistoryUsesIntegritySequenceNotClaimedTime`
(`services/access/internal/store/lifecycle_smoke_test.go`), which appends two events with inverted
timestamps and asserts three separable things: `History` returns append order, the stored
`sequence` values follow append order, and **`VerifyLifecycle` passes**. The third is this
amendment's claim; without it the test would prove only that a read sorts correctly.

**One distinction this amendment must not blur.** *Inverting the order of two honestly-signed
timestamps* is not tampering and verifies clean. *Modifying a stored `occurred_at`* is tampering and
**fails** — measured, not assumed: it returns `entry hash mismatch on ticket … at sequence N`,
because `occurred_at` is inside `CanonicalEvent` and the verifier recomputes from the stored value.
A claim that verification is indifferent to `occurred_at` **as a value** would be false and would
contradict §Threat model.

**The legacy exception, stated rather than glossed.** For rows that predate the chain, the backfill
adopts `(occurred_at, id)` as the baseline order and chains them in it
(`lifecycle_checkpoint.go:317`, whose comment says it *"adopts the order the trail has always
presented rather than inventing a new one"*). That path runs once, only for a ticket with events and
no head, and refuses outright on partial coverage. So for legacy rows **the chain makes the
wall-clock order tamper-evident; it does not make it true.** Live appends never touch that query.

### Inventory `claim_history` — the sort key IS the ordering guarantee, and there is no chain

| | |
|---|---|
| **What fixes order** | Nothing cryptographic. `append_order` is a sequence-backed tie-break only: both reads are `ORDER BY occurred_at, append_order NULLS FIRST, id` (`capacity.go:132`, `operational.go:369`). |
| **What displays** | The same clause. There is no second order to fall back on. |
| **Adversary** | **Honest-writer consistency, NOT tamper-evidence.** |

**Do not write "tamper-evident" about `claim_history`.** It has no chain, so per §The trust boundary
there is nothing here that constrains a writer with database access: they can set `occurred_at` and
`append_order` alike, and the `BEFORE INSERT` trigger that owns `append_order` is DDL, which the same
adversary can drop.

And the honest-writer case is genuinely exposed, which is why this is a real gap rather than a
wording problem: **a backward clock step reorders history that was appended in a definite order**,
because the wall clock leads. Access survives this and `claim_history` does not — the asymmetry is
the whole point of stating them separately.

**Not closed here. TKT-295** owns promoting `append_order` to primary, its legacy-NULL boundary, and
the guarded restore path. Per this ADR's pin-the-gap discipline, the exposure is asserted as
**present** by `TestHistoryOrdersDistinctTimestampsByOccurredAt`
(`inventory/internal/store/history_order_smoke_test.go`), which records the current wall-clock-first
preference and carries a comment saying that TKT-295 must **reverse it deliberately rather than
delete it**. Its sibling `TestHistoryOrdersTiedTimestampsByAppendOrder` is independent and must stay
green through that change.

### Restore and replication, for both trails

`append_order` is renumbered by a `BEFORE INSERT` trigger on any insert **that fires it**, so on
that path it preserves relative order only if rows are reinserted in their original order.
`occurred_at` survives a restore verbatim. Neither is evidence about the other.

**Two paths do not fire it, and they differ from each other** — a distinction TKT-295 must settle
rather than inherit:

- **`COPY`** does fire the trigger, so a plain restore renumbers. Migration `0012` says so and calls
  it deliberate, with `ALTER TABLE … DISABLE TRIGGER` named as the escape for a restore that must
  preserve the originals.
- **Logical replication apply** does **not**. `claim_history_set_append_order` is an ordinary
  trigger with no `ENABLE REPLICA`/`ENABLE ALWAYS`, and the apply worker runs with
  `session_replication_role = replica`, which skips ordinary triggers. A subscriber therefore keeps
  the **publisher's** `append_order` values rather than minting its own. That is arguably the
  desired behaviour — it preserves order across the link — but it is not currently a stated
  guarantee, and nothing tests it.

Designing the guarded restore path belongs with **TKT-295**, beside the promotion that would make
`append_order` load-bearing, and it must state the replication policy explicitly. Guarding a value
that is currently a tie-break would be guarding the wrong thing; guarding it while assuming the
trigger is always in force would be guarding it wrongly.

### What this amendment does not claim

- **Not** that any wall clock is monotonic. None is; that is the premise, not a conclusion.
- **Not** that a sort key detects tampering. Only the chain does, and only for access.
- **Not** that the legacy backfill reconstructs true historical append order — it adopts the
  wall-clock order and then makes *that* tamper-evident.
- **Not** that `claim_history` is tamper-evident, or that this ticket closes its exposure.
- **Not** that this ticket adds a restore path.
- **Not** anything about targeted rollback or current-key compromise, which remain open and remain
  TKT-11's (§The trust boundary, §Threat model).

### Threat model

Scoped to the **database-write adversary who does not hold a lifecycle private key**. Read
§Decision 5 for how key compromise changes this.

**Detected cryptographically** — modification of any signed field; insertion or reordering within a
ticket chain; a lifecycle row with no integrity row, or the reverse; sequence gaps; broken chain
links; unknown key ids; head mismatch.

**Not detected — targeted rollback**, i.e. rolling back a ticket's head coordinated with truncating
the organizer's checkpoint suffix. The truncated chain is internally consistent and **verifies
clean**; the surviving uncommitted heads are indistinguishable from ordinary activity inside the
current interval. This is the coordinated-rollback class ADR-016 §D7 defers to TKT-11, and the
checkpoint does not change its status (§Decision 2). *Conditionally*, a staleness tripwire exists —
but only if the signer holds its last-observed root **outside** the database and refuses to extend
a regressed chain, which no component does today; TKT-67 must specify that location or state
plainly that no tripwire exists. Detection then belongs to that external witness, not to this
scheme. **Until it is specified and built, record this attack as undetected.**

**Also not detected:** rollback of the checkpoint chain itself; validly signed writes from a
compromised lifecycle key or a controlled Access process — including, under head-only signing,
rewriting a ticket's whole history within the current key epoch, since the retained epoch signature
is itself deletable absent an external manifest (§Decision 5); deletion or reset of quarantine and
threshold state (§Decision 6); tampering predating the backfill baseline; and anything inside the
current checkpoint interval (§Decision 3).

**The pattern is one thing, not six** (§The trust boundary): everything above needs state the
database-write adversary cannot reach, and this database has no outside until TKT-11.

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
    - The checkpoint makes TKT-11 **affordable**: one external attestation per interval, instead of
      one per ticket head. That, and not rollback protection, is why it is in this ADR (§Decision 2).
      Nothing else in the design changes when the anchor lands.
    - Ed25519 lets an auditor verify the trail without holding the ability to forge it.
- **Negative:**
    - **A new async job that can silently stop**, whose failure has no symptom at the gate.
      Checkpoint freshness must be monitored — but note §Decision 2: freshness is a property of the
      signer's *external* continuity witness, not of the checkpoint. Without that witness the job
      is pure scaffolding for TKT-11 and buys no detection at all today. That is an acceptable
      reason to build it; it is not an acceptable thing to be vague about.
    - **Fail-open is unbounded against a database-write adversary.** Quarantine and thresholds bound
      *our bugs*, not attackers (§Decision 6). Accepted deliberately, on the judgement that our own
      bugs are likelier and that denying real customers at a live turnstile is the worse failure —
      but this is a fraud window, not a rounding error.
    - **This ADR is easy to over-cite, and has been wrong in that direction three times.**
      Modification and insertion are closed cryptographically. Targeted rollback and current-key
      compromise are **not closed at all** until TKT-11. Anyone saying "the lifecycle trail is
      tamper-evident" without naming the adversary is overstating this design — including future
      readers of the §Decision headings, which is why §The trust boundary leads the document.
    - **A dependency this repo cannot satisfy alone.** Three separate controls here (signer memory,
      quarantine/threshold state, epoch manifests) need storage outside the Access database. TKT-11
      now owns strictly more than "fiscal archive": it owns the trust domain the ticket trail's
      rollback protection depends on. That coupling is new, and real.
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
- TKT-230 (raised the wall-clock ordering finding) · **TKT-234** (the 2026-09-01 amendment above) ·
  **TKT-295** (owns the inventory half: promoting `append_order` to primary, its legacy-NULL
  boundary, and the guarded restore path)
- [ADR-003 — Append-only audit trail](./ADR-003-append-only-audit-trail.md) (§Status gap closed here;
  §D2 trace-derived redemption; §D3 pseudonymity; §D4 NF525 scope)
- [ADR-016 — Checkout recovery state machine](./ADR-016-checkout-recovery-state-machine.md) (§D7
  threat model, re-argued here for per-ticket granularity)
- [ADR-025 — Admission events and offline reconciliation](./ADR-025-admission-events-and-offline-reconciliation.md)
  (§Decision 5: the integrity sequence is the authoritative order, `occurred_at` is a claim — the
  rule the TKT-234 amendment records the chain's guarantee for)
- [ADR-002 — Services from day one](./ADR-002-services-from-day-one.md) (one database per service)
- [ADR-012 — Ticket issuance and QR credentials](./ADR-012-ticket-issuance-and-qr-credentials.md)
- [ADR-022 — Out-of-band service migrations](./ADR-022-out-of-band-service-migrations.md) (supersedes
  ADR-008 **on placement**; §Decision 9 is amended for it) ·
  [ADR-008 — Embedded migrations](./ADR-008-embedded-migrations.md) (goose-as-library, `embed.FS`,
  per-service ownership and the 30s fail-fast deadline still stand) ·
  [ADR-020 — Non-concurrent index builds](./ADR-020-catalog-index-build-concurrency.md) (still no
  `CONCURRENTLY`: ADR-022 satisfied precondition (1), but they are conjunctive and (2)/(3) remain
  false) ·
  [ADR-010 — PostgreSQL claim transaction](./ADR-010-postgres-claim-transaction.md)
