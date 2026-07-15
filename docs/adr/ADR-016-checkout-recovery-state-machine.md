# ADR-016: Checkout recovery state machine and journal tamper-detection boundary

Date: 2026-07-14

## Status

Accepted (approved at the TKT-43 plan gate)

Amends [ADR-011](./ADR-011-checkout-journal-protocol.md) — its recovery story only; the protocol is
unchanged. Also **scopes [ADR-003](./ADR-003-append-only-audit-trail.md)'s "inalterable history"**
wording (§Decision 7): that phrase describes *application-level append-only behaviour*, not a
rollback-proof guarantee, and ADR-003's Option-3 pro should be read with the threat model below.

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
  inbound checkout replay carrying the same idempotency key and a matching normalized fingerprint
  (the fingerprint trims the name and lowercases/trims the email, `server.go:339`, so the replay need
  not be byte-identical — but it must still carry the original payment token). Nothing in the system
  generates that replay. If the buyer closes the tab, the order is stuck.
- **A stranded claim consumes capacity indefinitely.** Inventory counts `finalizing` against
  availability and exempts it from expiry (`services/inventory/internal/store/store.go:138`) — correct
  per ADR-011, but with no recovery the seat is never returned. Exact replay recovers *some* paths;
  it does not recover the ambiguous-release path below.
- **`release_pending` is returned to the buyer but persisted nowhere.**
  `services/commerce/internal/api/server.go:400` writes the literal to the HTTP response; the string
  appears nowhere else in the repository. On the initial checkout path the order row remains
  `created` and the read API reports `created`; on a replay of an order already parked at
  `payment_unknown` the same branch leaves it `payment_unknown`. Either way the buyer is told a
  state the system does not record.
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
- **The journal verifier detects modification, but not coordinated rollback.** `Verify`
  (`services/payments/internal/store/store.go:189-251`) detects a mutated row, an *uncoordinated*
  deletion (one whose later chain and head survive), a broken head, and a head missing for surviving
  entries. It does **not** detect a coordinated rollback: delete any suffix of a chain, roll
  `journal_heads` back to the surviving entry, and every remaining hash and signature is valid,
  sequences are contiguous, verification passes. Deleting an organizer's entire chain plus its head,
  or restoring the whole database to an older consistent snapshot, has the same property. The chain
  proves *what was written stays written*; nothing inside the database proves *how much* was written.

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

`release_pending` becomes a **durable state with a persisted terminal payment outcome**, recorded
*before* release is attempted, so a recovery pass knows what it is completing. Release is safe to
retry: inventory's transition is already idempotent for a repeated target, including the "released
but response lost" case.

The releasable outcome is **a durably recorded result proving no side effect** — not "a timeout".
ADR-011 is precise here: only a decline, or a fake-PSP timeout *"whose status proves no side
effect"*, releases the claim. A transport timeout is **not** such proof and never becomes one. The
current fake PSP records `payment.timeout` as a deliberate terminal answer
(`services/payments/internal/api/server.go:134-137`), which is a property of the fake, not of
timeouts. The PSP port must therefore expose a **normalized terminal-no-side-effect outcome**; a
transport timeout maps to `payment_unknown`, never to a release.

### 3. Transition ownership: separate the payments-operation lookup from real-PSP status

These are two different capabilities and conflating them is what makes the dependency question look
harder than it is:

- **Payments-operation lookup** — ask payments "what is the stored terminal result for this
  organizer + idempotency key?" The `payment_operations` row already holds it
  (`services/payments/internal/store/store.go:253-286`). This is a **new read endpoint over existing
  data**, not a PSP call.
- **Real-PSP status** — ask the provider what actually happened. Requires the PSP port.

| Parked state | Evidence already held | Needs payments lookup? | Needs real-PSP status? | Recovery action |
|---|---|---|---|---|
| `created` | **ambiguous — see below** | **Yes** | Only if payments has no terminal result | resolve via lookup, then re-drive from the resolved state |
| `confirmation_pending` | capture returned 200 — money is **known captured** (`server.go:420-427`) | No | **No** for the happy retry | retry inventory `confirm`; then complete. **If confirm is terminally impossible** (claim released/expired), park as `reconciliation_required` — compensation needs the port |
| terminal `declined` / `timeout` awaiting release | a **durably recorded outcome proving no side effect** | No | **No** | persist outcome, retry idempotent release, mark terminal, journal `order.failed` |
| `payment_unknown` | nothing — the side effect is genuinely unknown | Yes | **Yes**, if payments has no terminal result | resolve; then confirm or compensate |

**`created` is not one state.** It is the default row value and can mean payment never attempted;
inventory finalized but commerce crashed before persisting `finalizing`; payment captured but
commerce crashed before persisting `confirmation_pending`; payment declined/timed out but commerce
crashed before persisting the outcome; or release committed and its response was lost. The order row
cannot distinguish these, and checking the error on the `confirmation_pending` write does not close
the window — a **crash** between payments' response and that write still lands on `created`.

**The recovery runner cannot replay the charge.** The payment token lives only in the request body:
no commerce table stores it (`migrations/0001_checkout.sql`), and payments folds it into a one-way
SHA-256 fingerprint (`services/payments/internal/api/server.go:109`). A background runner has no
token and cannot reconstruct one from a hash. It therefore **must** resolve `created` through the
payments-operation lookup rather than by re-driving checkout. This is a real dependency and the
lookup endpoint belongs to the recovery slice, not the PSP slice.

**What this buys:** the deterministic-recovery slice needs the payments lookup (cheap, existing data)
but **not** the PSP port. Only unresolvable `payment_unknown` and compensation of impossible
confirmations wait on the port. Blocking *all* recovery behind the port would serialize work that
can proceed now; claiming *no* recovery needs a payments dependency would be equally wrong.

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

The honest boundary is a **threat model**, not a one-line claim. The signing key lives outside the
database (`JOURNAL_SIGNING_KEY`, `services/payments/cmd/payments/main.go:106-112`) and verification
runs in application code, so "integrity within its own database" understates the protection the
external key provides *and* hides the rollback limitation. What `verify-journal` actually guarantees:

| Adversary | Detected | Not detected |
|---|---|---|
| Can mutate the database, **no** signing key | modification, insertion, uncoordinated deletion (later chain/head survive), head mismatch | **consistent prefix rollback**, **suffix truncation with head rolled back**, whole-organizer removal, full-database restore to an older snapshot |
| Has database **and** signing key / application control | — | everything: the chain can be rewritten and re-signed |
| Accidental corruption | the tested mutation, gap, and head-mismatch cases | — |

The undetectable class is *rollback*, not deletion-in-general: any edit that leaves a shorter but
internally consistent chain passes, because nothing inside the instance attests how long the chain
should be. This limit is **accepted and documented for TKT-43**, not silently carried: TKT-43 adds
fault-injection cases that demonstrate it (drop an organizer's entries *and* head; truncate a suffix
and roll the head back → `verify-journal` passes in both), so the hole is a tested, known boundary
rather than an assumption.

When the anchor is built, the candidate shape is a signed checkpoint
`{organizer_id, last_sequence, last_hash, key_id, signature, closed_at}` published outside the
payments database, with `Verify` additionally asserting that the head is at or ahead of the newest
checkpoint and that the chain replays to the checkpointed hash at that sequence. Per ADR-003 §4 any
such mechanism must layer on **without schema or flow changes**, which the current
`journal_entries` / `journal_heads` schema satisfies. Candidate anchors, in ascending independence
and cost: a NATS JetStream stream (already in the stack per [ADR-007](./ADR-007-postgres-nats.md));
an append-only object store with WORM/Object Lock; RFC 3161 third-party timestamping.

This checkpoint is **plausible groundwork for**, not equivalent to, an NF525 period closure. ADR-003
states its own NF525 characterization is *"inferred from how NF525 works, not verified against the
standard's text"*, with verification deferred to TKT-11. This ADR cannot upgrade that inference into
a requirement: whether a checkpoint satisfies a compliant period closure is TKT-11's finding to make,
and the choice of anchor belongs there.

**A checkpoint attests everything up to its sequence, and nothing after it.** A checkpoint at
sequence N commits to the chain hash at N, which transitively covers genesis→N; entries appended
*after* the newest checkpoint are unattested until the next one. With no checkpoint, nothing is
attested. Two consequences worth stating plainly: the guarantee is bounded at the **latest
successfully externalized checkpoint**, and the "head at or ahead of newest checkpoint" rule
therefore still permits rolling the database back *exactly* to that checkpoint. Checkpoint frequency
is the residual exposure window.

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
    - Recovery of `confirmation_pending` and of durably-proven terminal outcomes can be built behind
      a payments-operation **lookup** (existing data, new read endpoint) without the PSP port. Only
      unresolvable `payment_unknown` and compensation of impossible confirmations wait on it.
    - The ambiguous-release bug is fixed by ordering (evidence before `finalize`), not by persisting
      a string that describes a state nobody drives.
    - The outbox closes the commit→publish window with a frozen envelope, so a retried publish is
      byte-identical rather than timing-dependent.
    - The journal's guarantee becomes a stated threat model — modification-evident, rollback-blind —
      instead of an unqualified "inalterable".
    - TKT-11 inherits a candidate checkpoint design and an explicit statement of what it must verify.
- **Negative:**
    - Commerce gains its first background worker and with it replica-ownership, lease and
      shutdown-draining concerns it has never had.
    - The recovery slice needs a new payments read endpoint before it can resolve ambiguous `created`
      orders — the runner cannot replay a charge, because the payment token is never persisted.
    - Two ADR passes on one protocol: the PSP operation model lands in a later amendment.
    - Until an anchor exists, coordinated rollback (suffix truncation, whole-organizer removal,
      snapshot restore) remains undetectable — mitigated only by tests that document it.
    - `confirmation_pending` whose confirm is terminally impossible parks as `reconciliation_required`
      until compensation exists; those orders hold captured money with no automated resolution.
    - Recovery adds retry/backoff, observability and poison-row handling to a service that currently
      fails fast and forgets.
- **Neutral:**
    - ADR-011's protocol (authorize → finalize claim → capture → confirm) is unchanged. This ADR
      amends its **recovery** story only, and scopes ADR-003's "inalterable" wording.
    - The `finalizing`-from-`confirmed` exemption already in inventory (`store.go:187`) is the same
      crash-recovery reasoning applied at a different step; no change needed there.

## References

- [ADR-011 — Checkout finalization and canonical money journal](./ADR-011-checkout-journal-protocol.md) (recovery story amended by this ADR)
- [ADR-003 — Append-only audit trail](./ADR-003-append-only-audit-trail.md) ("inalterable history" scoped by §Decision 7; §1 compensating entries; §4 NF525-without-schema-changes, whose NF525 characterization ADR-003 itself marks as inferred pending TKT-11)
- [ADR-012 — Ticket issuance](./ADR-012-ticket-issuance-and-qr-credentials.md) (names TKT-43 for outbox recovery) · [ADR-007 — PostgreSQL + NATS](./ADR-007-postgres-nats.md) · [ADR-010 — PostgreSQL claim transaction](./ADR-010-postgres-claim-transaction.md)
- TKT-43 · TKT-28 (the walking skeleton this hardens) · TKT-11 (fiscal archive; owns the anchor choice) · TKT-33 (PII erasure machinery)
