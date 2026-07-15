# ADR-016: Checkout recovery state machine and journal tamper-detection boundary

Date: 2026-07-14

## Status

Accepted (approved at the TKT-43 plan gate; amends [ADR-011](./ADR-011-checkout-journal-protocol.md))

## Context

[ADR-011](./ADR-011-checkout-journal-protocol.md) shipped checkout as a walking skeleton and recorded
its own limits: *"Recovery in this walking skeleton is retry-driven … Automated scheduling and
real-PSP status/compensation are required before replacing the fake PSP."* TKT-43 exists to remove
those limits. [ADR-012](./ADR-012-ticket-issuance-and-qr-credentials.md) additionally names TKT-43
as the owner of *"scheduled/restart outbox recovery"*.

Reading the shipped code establishes what "retry-driven" means in practice. Every fact below was
verified against the TKT-43 worktree, not inferred from the ADR text:

- **Nothing revisits a parked order.** Commerce has no ticker, cron, consumer, or restart scan; the
  only `go func` in the service is the HTTP listener (`services/commerce/cmd/commerce/main.go:134`).
  `payment_unknown` and `confirmation_pending` are durable and visible, and are advanced only by a
  byte-identical checkout replay. Nothing in the system generates that replay. If the buyer closes
  the tab, the order is stuck.
- **A stranded claim consumes capacity indefinitely.** Inventory counts `finalizing` against
  availability and exempts it from expiry (`services/inventory/internal/store/store.go:138`) — correct
  per ADR-011, but with no recovery the seat is never returned. Exact replay recovers *some* paths;
  it does not recover the ambiguous-release path below.
- **`release_pending` is returned to the buyer but persisted nowhere.**
  `services/commerce/internal/api/server.go:400` writes the literal to the HTTP response; the string
  appears nowhere else in the repository. The order row remains `created` and the read API reports
  `created`.
- **The release outcome is ambiguous, and replay resolves it wrongly.** A transport error on release
  is not evidence that inventory did nothing — inventory may have committed `released` and lost the
  response. Inventory's transition is idempotent for a repeated target
  (`store.go:182`, `if c.Status == target`), but a `finalize` against an already-`released` claim
  falls through to `ErrConflict` (`store.go:198`). Commerce replays `finalize`
  (`server.go:377`) **before** consulting payments, so a genuinely-released claim returns
  `409 hold expired` and the order stays `created` forever. Persisting the literal `release_pending`
  would not fix this.
- **Two crash boundaries write the canonical journal before the local projection.** Commerce
  journals `order.completed` (`server.go:431`) before committing `CompleteOrder` (`server.go:440`).
  Payments appends the terminal fact (`services/payments/internal/api/server.go:149`) before calling
  `CompleteOperation` (`:154`). Either crash leaves the canonical money journal asserting an outcome
  the projection does not reflect. TKT-43 explicitly calls for repairing the second.
- **Error-swallowing on the recovery-relevant writes.** `markUnknown` discards both results
  (`server.go:287-290`, `_, _ = s.db.ExecContext(...)`), as does the `confirmation_pending` write
  (`server.go:427`). The durable projections ADR-011's recovery story depends on can fail silently.
- **Publication is unprotected after commit.** `CompleteOrder` commits at `server.go:440`; the
  JetStream publish happens at `server.go:446`. A crash between leaves a `completed` order with no
  `platform.commerce.order.completed`, so access never issues the ticket. No outbox exists in
  commerce or payments.
- **The journal verifier trusts its own database.** `Verify`
  (`services/payments/internal/store/store.go:189-251`) detects a mutated row, a deleted entry, a
  broken head, and a head missing for surviving entries. Every one of those defences — the hash
  chain, `UNIQUE (organizer_id, sequence)`, the append-only triggers — is enforced by the same
  PostgreSQL instance being attested. Deleting an organizer's entire entry chain *and* its
  `journal_heads` row leaves a state that verifies clean.

A decision is needed before code because the slices that follow (outbox, deterministic recovery, PSP
port, keyring) each assume an answer to "who owns which transition, on what evidence."

## Possible Solutions

- **Option 1 — Implement recovery per slice, record decisions as they land:**
    - Pros: no up-front doc work; each PR argues its own design.
    - Cons: the contested calls (ambiguous release, PSP-status dependency, compensation identity) get
      re-litigated in every PR; slices 3–5 cannot start until slice 2's implicit choices are read out
      of merged code. Rejected.
- **Option 2 — One ADR fixing the full recovery state machine, including the PSP compensation model:**
    - Pros: total up-front clarity.
    - Cons: the PSP port's durable operation model (provider reference identity, void/refund
      idempotency, captured-vs-authorized) cannot be honestly specified before the port exists — the
      current `payment_id` is a journal fact UUID, not a provider reference. Writing it now means
      inventing detail we would rewrite.
- **Option 3 — Fix the state machine, evidence rules and transition ownership now; defer the PSP
  operation model to its own slice (chosen):**
    - Pros: unblocks the three slices that need no PSP dependency; keeps the contested calls in one
      reviewable place; leaves the PSP model to be written against a real port.
    - Cons: two ADR passes on the same protocol.
- **Option 4 — Build the tamper anchor inside TKT-43:**
    - Pros: closes the whole-chain-deletion hole now.
    - Cons: an anchor *is* an NF525 period closure; building it here pulls TKT-11's fiscal-archive
      design forward before the French market is prioritized, which [ADR-003](./ADR-003-append-only-audit-trail.md) §4
      deliberately deferred. Rejected for TKT-43; see the boundary below.

## Decision

We adopt Option 3, and we record the tamper-detection boundary rather than closing it.

### 1. Recovery is driven, not awaited

Commerce gains a **restart-and-scheduled recovery runner**. Every non-terminal order state carries a
durable claimant: rows are claimed with `FOR UPDATE SKIP LOCKED` under a bounded lease, retried with
backoff, and released on graceful shutdown. A parked state with no scheduler is not recovery — it is
another parking spot. This is commerce's first background worker; the service has no scheduled work
today, so replica ownership and shutdown draining are part of the slice that introduces it, not an
afterthought.

### 2. Evidence before action, and never `finalize` first on replay

Recovery and replay decide from **durable local evidence**, then from the authority that owns the
fact — never from an inference about what a failed transport did. Specifically, the replay path must
consult the persisted terminal outcome (and, once available, payments' operation record) **before**
calling `finalize`. The current order — finalize, then discover the outcome — is what converts an
ambiguous release into a permanent `409 hold expired`.

`release_pending` becomes a **durable state with a persisted terminal payment outcome**
(`declined` vs. no-side-effect `timeout`), recorded *before* release is attempted, so a recovery pass
knows what it is completing. Release is safe to retry: inventory's transition is already idempotent
for a repeated target, including the "released but response lost" case.

### 3. Transition ownership and the PSP-status dependency

Recovery does **not** uniformly depend on the PSP port:

| Parked state | Evidence already held | Needs PSP status? | Recovery action |
|---|---|---|---|
| `confirmation_pending` | capture returned 200 — money is **known captured** (`server.go:420-427`) | **No** | retry inventory `confirm`; then complete |
| terminal `declined` / `timeout` awaiting release | PSP returned a terminal answer | **No** | persist outcome, retry idempotent release, mark terminal, journal `order.failed` |
| `payment_unknown` | nothing — the PSP's side effect is genuinely unknown | **Yes** | resolve via PSP status; then confirm or compensate |

Only `payment_unknown` requires status resolution. Blocking all recovery behind the PSP port would
serialize work that can proceed immediately.

### 4. Compensating entries are facts, not updates

Void and refund are **compensating journal entries** (ADR-003 §1), never mutations. The journal's
fact-type allowlist (`services/payments/internal/store/store.go:110`) gains `payment.voided` and
`payment.refunded` when the PSP port lands. Adding the types is not blocked by the port; the
*operation model* is — provider reference identity, void/refund idempotency keys,
captured-vs-authorized, and a durable result model are specified in the PSP slice, against a real
port.

### 5. Canonical-before-projection crash boundaries are named invariants

Both boundaries above are repairable-by-design, not incidental: where the canonical journal records
an outcome the local projection has not yet written, the projection is repaired forward from the
journal — never the reverse, and the journal is never rewritten to match a projection. Each boundary
gets a named invariant and a fault-injection test in the slice that owns it.

### 6. Outbox publication carries a frozen envelope

Commerce's completion event moves to a **transactional outbox**: the row is written inside
`CompleteOrder`'s transaction and drained after commit, at-least-once, with the JetStream ack
preceding the sent-marking. The outbox persists the **canonical envelope**, including a frozen
`occurred_at`. Today's publisher rebuilds the envelope on every attempt with a new timestamp while
the deterministic `Nats-Msg-Id` stays fixed (`services/commerce/internal/events/events.go:48-59`),
which makes the payload a function of retry timing. This follows the owed-marker discipline in
`AGENTS.md`: decide under the row lock, commit, then emit — never publish inside the transaction.

### 7. Tamper detection: the boundary is stated, the anchor is deferred

`verify-journal` attests **integrity within its own database**. It cannot detect deletion of an
organizer's complete chain together with its head row, because no attestation exists outside the
instance under attack. This limit is **accepted and documented for TKT-43**, not silently carried:
TKT-43 adds a fault-injection case that demonstrates it (drop an organizer's entries *and* head →
`verify-journal` currently passes), so the hole is a tested, known boundary rather than an assumption.

When the anchor is built, it takes the shape NF525 already requires rather than a parallel
mechanism — a signed checkpoint
`{organizer_id, last_sequence, last_hash, key_id, signature, closed_at}` published outside the
payments database, with `Verify` additionally asserting that the head is at or ahead of the newest
checkpoint and that the chain replays to the checkpointed hash at that sequence. Per ADR-003 §4 this
must layer on **without schema or flow changes**, which the current `journal_entries` /
`journal_heads` schema satisfies. Candidate anchors, in ascending independence and cost: a NATS
JetStream stream (already in the stack per [ADR-007](./ADR-007-postgres-nats.md)); an append-only
object store with WORM/Object Lock; RFC 3161 third-party timestamping. The choice belongs to TKT-11
(fiscal archive), whose consumer this checkpoint is.

**A checkpoint only protects what it has covered**: entries appended before the first checkpoint
remain unattested. If the anchor is wanted for NF525, building it earlier is strictly cheaper.

### 8. Key rotation is production behaviour

The journal holds exactly one key and `Verify` rejects any entry whose `key_id` differs
(`store.go:213-215`), so rotating the key today invalidates all history. Rotation is therefore a
**keyring**: sign with the active key, verify against historical keys, validate at startup, and a
stated retirement policy — retiring a verification key necessarily makes that era unverifiable, which
is a retention decision, not an implementation detail. `services/access` already implements this
pattern (`ACCESS_QR_KID` + `ACCESS_QR_PUBLIC_KEYS`, `cmd/access/main.go:111-115`) and is the
precedent. This is production code, not test scaffolding.

## Consequences

- **Positive:**
    - The three recovery paths that need no PSP dependency can be built immediately; only
      `payment_unknown` waits on the port.
    - The ambiguous-release bug is fixed by ordering (evidence before `finalize`), not by persisting
      a string that describes a state nobody drives.
    - The outbox closes the commit→publish window with a frozen envelope, so a retried publish is
      byte-identical rather than timing-dependent.
    - The journal's tamper claim becomes precise: "integrity within the database" is defensible;
      "inalterable" was not.
    - NF525 (TKT-11) inherits the checkpoint design instead of discovering it.
- **Negative:**
    - Commerce gains its first background worker and with it replica-ownership, lease and
      shutdown-draining concerns it has never had.
    - Two ADR passes on one protocol: the PSP operation model lands in a later amendment.
    - Until the anchor exists, whole-chain deletion remains undetectable — mitigated only by a test
      that documents it.
    - Recovery adds retry/backoff, observability and poison-row handling to a service that currently
      fails fast and forgets.
- **Neutral:**
    - ADR-011's protocol (authorize → finalize claim → capture → confirm) is unchanged. This ADR
      amends its **recovery** story and its tamper-claim wording only.
    - The `finalizing`-from-`confirmed` exemption already in inventory (`store.go:187`) is the same
      crash-recovery reasoning applied at a different step; no change needed there.

## References

- [ADR-011 — Checkout finalization and canonical money journal](./ADR-011-checkout-journal-protocol.md) (amended by this ADR)
- [ADR-003 — Append-only audit trail](./ADR-003-append-only-audit-trail.md) (§1 compensating entries, §4 NF525-without-schema-changes) · [ADR-012 — Ticket issuance](./ADR-012-ticket-issuance-and-qr-credentials.md) (names TKT-43 for outbox recovery) · [ADR-007 — PostgreSQL + NATS](./ADR-007-postgres-nats.md) · [ADR-010 — PostgreSQL claim transaction](./ADR-010-postgres-claim-transaction.md)
- TKT-43 · TKT-28 (the walking skeleton this hardens) · TKT-11 (fiscal archive; owns the anchor choice) · TKT-33 (PII erasure machinery)
