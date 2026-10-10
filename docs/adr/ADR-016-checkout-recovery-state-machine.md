# ADR-016: Checkout recovery state machine and journal tamper-detection boundary

Date: 2026-07-14

## Status

Accepted (approved at the TKT-43 plan gate)

Amended by **TKT-262** (2026-08-20, §Amendment below) — records the compensation's refund-before-release
ordering, which this ADR had implemented and left unstated, and bounds what a refused release guarantees.

Amended by **TKT-145** (2026-09-01, §Amendment below) — records why an unparked `release_pending`
checkout is answered 202 rather than a terminal 402/408, distinguishes the two exits that park (only
one of which leaves the row `release_pending`), and states that the attempt budget bounds failed
*claimed re-drives* rather than the buyer's elapsed wait, which has no wall-clock bound.

Amended by **TKT-322** (2026-10-10, §Amendment below) — a release reports whether its own statement parked
the row, a release or abandon that matches no row is reported as a supersession, and the runner logs
what the store reports. Adds no audit, integrity or liveness guarantee.

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

## Amendment (2026-08-20, TKT-262) — a compensation refunds before it releases, and a refused release parks without a contradictory terminal transition

§Consequences above names the `reconciliation_required` population but says nothing about the
**order** in which a compensation moves money and discharges the inventory obligation. That silence
is what this amendment closes. Nothing in the code changes; the decision was already implemented and
merely unrecorded — and, until this ticket, unasserted by any test.

### The intersection

`resolveReconciliation` decides from PSP status alone. Where the provider reports `captured` with a
positive captured amount, it refunds, and only then does `finishRefunded` call `inventory.Release` —
which is the first moment it can learn, from a 409, that the claim is **confirmed**: inventory has
sold the seat. The buyer's money is already back. The order is parked with
`"refunded money against a confirmed claim; manual reconciliation required"`.

This is reachable in production data today, and it is worth being exact about *which* population
makes it so — the entry and the exit are different rows.

**The entrance.** Migration `0005_psp_recovery.sql` cleared `recovery_parked_at` for two named
reason strings — `payment result unknown; needs PSP status (TKT-56)` and `captured payment whose
claim is gone; needs void/refund (TKT-56)` — and rebuilt `orders_recovery_claimable_idx` to include
unparked `reconciliation_required`. Those re-opened rows are claimable, and driving one is how a
pass reaches `resolveReconciliation` and then this branch.

**The exit.** The row this branch *produces* is **not** claimable. `ParkForReconciliation` sets
`recovery_parked_at=now()`, and the claimable index requires `recovery_parked_at IS NULL`, so a
confirmed-claim park stays parked until an operator unparks it. It does not re-enter the runner on
its own, and no backfill re-opens it.

### Decision: refund first, release second

The ordering stands. The trade-off is between two bad states, and they are not symmetric:

| Ordering | Failure state | Who is harmed |
|---|---|---|
| **Refund, then release** *(chosen)* | Buyer refunded, seat sold | Nobody is out of pocket. An operator reconciles a seat. |
| Release, then refund | Seat released, refund then fails | The **buyer's money is stranded** while the seat is resold. |

Money returning to the buyer and staying there is recoverable by a human holding a seat inventory
can re-sell. Money stranded against a released seat is a buyer harmed by our failure, discovered by
them, not by us. A compensation path exists to make the buyer whole; it should not have a branch
whose failure mode is the opposite.

### Rejected: release before refund

Inverts the harm as above, and inverts §Decision 2's evidence-before-action rule — the release would
precede the durable evidence that justifies it.

### Rejected: establish inventory state before refunding

Rejected for two independent reasons, either sufficient.

**It is not implementable against inventory's surface.** `services/inventory/internal/api/server.go`
exposes `POST /internal/holds`, `POST /internal/holds/{id}/{confirm,finalize,release,refund-capacity}`
and `GET /internal/holds/{id}/seating`. There is no read-only hold-status route. This is a proposal
to add an endpoint, not to re-order two calls.

**It would not work if it existed.** The read is stale the moment it returns; a confirm racing the
refund lands in the same intersection with an extra call spent. This is the shape
[ADR-062](./ADR-062-refund-reversal-reconciliation.md) §2 already rejected for the sibling runner:
a downstream refusal is **observed, not predicted**, because a commerce-side predicate over
inventory's state drifts from inventory's rule with every change to it. The runner learns the claim
is confirmed by attempting the release. That is the design, not a shortcut.

*This rejection does not rest on the external-call budget.* `MaxCallsPerOrder` is 6 and the executed
chain on this path is shorter, so one more call would fit. The reasons above stand on their own.

### What is guaranteed, and what is not

State this precisely, in the discipline [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)
applies to "tamper-evident" — name the claim before making it.

**Guaranteed, on this branch, per recovery pass:** when the release is *refused*, the pass ends
parked and takes **no contradictory terminal transition** — the order is not marked refunded, not
marked released, and no `order.failed` fact is journalled. Pinned by
`TestReconciliationRequiredRefundAgainstConfirmedClaimIsParkedOnly`.

*Per pass*, not per lifetime. Nothing marks the row as having been refused before, so an operator
who unparks a still-confirmed row gets a second refusal and a second park, writing the same reason
again. That is the intended behaviour — the guarantee is that no single pass takes two contradictory
terminal transitions, not that the refusal can only ever happen once.

Scope this claim to the refusal branch and nowhere else. On the **ordinary** path the release
*succeeds*, and `finishRefunded` then journals `order.failed` and calls `MarkRefunded` — so commerce
routinely and correctly records a refund alongside a discharged seat. That is the success case, not a
contradiction: the seat came back. The guarantee here is narrower and is about the one branch where
the seat did **not** come back.

**Not guaranteed:** the two *external* effects are not atomic and cannot be made so from here. A
completed PSP refund and a confirmed inventory claim can and do coexist; no ordering of two calls to
two services over a network prevents it. What the system provides is that each refusing pass
**parks the row with a reason an operator can act on**, rather than resolving it one way and hiding
the other — not that the contradiction cannot occur, and not that it is recorded only once. Repeated
intervention appends: every unpark writes an `order_recovery_unparks` row, and each subsequent
refusal rewrites `recovery_parked_at` and `recovery_last_error`.

Reconciling such a row is a human's job. The operator surface exists — **TKT-146** shipped
`commerce unpark-order`, and `docs/development.md` §Parked recovery orders documents this ordering
from the operator's side, including the warning that unparking a `reconciliation_required` row asks
the runner to re-decide on PSP evidence alone. This amendment records the decision behind that
behaviour; it does not add a second operator path (ADR-062 §4).

What unparking such a row does depends entirely on whether the claim is still confirmed, and both
outcomes are worth stating because an operator needs to predict which one they are about to get.
The runner re-reads PSP status — now `refunded` — and `actOnProviderStatus` routes back to
`finishRefunded` (`runner.go`). From there:

- **The release maps to `nil`** → `order.failed` is journalled, `MarkRefunded` runs, and the order
  reaches its correct terminal state. `clients.Release` maps exactly two answers to `nil`: a **200**
  (the claim is already `released`, or it is `expired` — expiry already freed the seats) and a
  **404** (no such claim, so there is no obligation left to discharge).
- **The release is refused again (409)** → the row **re-parks** with the same reason, having spent
  only a fresh retry budget.
- **Anything else** — a transport failure, a 5xx — is neither: `finishRefunded` returns the error and
  `RunOnce` hands it to `fail`, which re-drives the order with backoff. Say the ending precisely:
  `ReleaseStuckOrder` returns it to the claimable set *only while attempts remain*, and sets
  `recovery_parked_at` once `recovery_attempts >= MaxRecoveryAttempts`. A persistent dependency
  failure therefore does not retry forever — it ends **parked**, by the ordinary exhaustion path
  rather than by this branch, and with a different `recovery_last_error`.

Be precise about what counts as a repair, because the obvious one is not enough. Inventory's
`Transition` refuses any claim that is not `held` or `finalizing`, so **`confirmed` cannot be
released** — and returning capacity out of band does **not** move `claims.status` off `confirmed`
(`ReturnRefundedCapacity` leaves the status alone).

Nor is there a two-step way around it. `Transition` does accept `finalizing` *for* a `confirmed`
claim, but that branch is an idempotency no-op for a crashed checkout replaying its finalize — it
returns early and never writes `claims.status`, so the claim is still `confirmed` afterwards and a
following release is refused exactly as before.
An operator who returns capacity and then unparks gets the 409 branch, not the success branch. The
claim itself has to reach a state whose release maps to `nil`.

So unparking is the *second* step, not the fix: it is how a repaired order is returned to the runner.
Unparking alone, with the claim untouched, only buys another refusal.


## Amendment (2026-09-01, TKT-145) — an unparked `release_pending` checkout answers 202, and what bounds the buyer's wait

§Decision 2 makes `release_pending` a durable state carrying a persisted terminal payment outcome
(the paragraph beginning *"`release_pending` becomes a durable state"*). It does not say what a
**buyer** is told when they retry a checkout that is sitting in
it. Both code paths have answered **202 with the durable status** since TKT-116, and both carry
comments defending it — but the choice was never recorded as a decision, so it read as an
implementation detail rather than as something weighed and settled.

Nothing in the code changes. Unlike the TKT-262 amendment above, the behaviour here is **also
already asserted by a test**: TKT-280's `TestParkedReleasePendingGetsTheSameAnswerFromBothPaths`
(`services/commerce/internal/api/recovery_parked_answer_smoke_test.go:47`) pins the unparked 202 on
both paths. What was missing was only the *decision*, and the reason it is not the obvious 402/408.

### The decision

**An unparked `release_pending` order is answered `202` echoing the durable status, on both paths,
with no `error` field.** The alternative — deriving a terminal 402/408 from `terminal_outcome` — is
rejected; the refutation is below so it is not re-proposed.

### Why 202 and not the terminal outcome

`terminal_outcome` is written by `RecordTerminalOutcome` in the **same statement** as
`status='release_pending'`, gated on `terminal_outcome IS NULL` and fenced on the recovery claim
(`services/commerce/internal/store/recovery.go:197-205`), and `releasableOutcome` (:167) admits only
outcomes that **prove no side effect**: `declined`, `timeout`, `not_attempted`, `no_side_effect`. So
at this point the system does know, durably, that **no money was captured**.

What it does not know is **how the order ends**. `terminal_outcome` records why the *payment*
finished; the *order* still owes an inventory release, and that release can still be refused. A 402
here would be a claim the state machine is able to falsify moments later — precisely the discipline
[ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) applies to "tamper-evident": name what the
evidence actually proves before asserting it. 202 with the durable status says *the payment is
resolved and the order is not* — which is exactly what is known — and it stays true under every
continuation below. A 402 that a later park contradicts is worse than a 202 that never lied.

There is a second, independent objection. `terminalCheckoutCode` (`api/server.go:1536`) maps
`timeout`→408 and everything else→402, and it takes the **order status**, not the outcome. The
order's terminal status comes from `store.TerminalStatus` (`store/recovery.go:175`), which folds
`not_attempted` and `no_side_effect` into `timeout` deliberately — *"from their side nothing was
charged either way"*. An API-tier shortcut feeding `terminal_outcome` straight into
`terminalCheckoutCode` would therefore tell a `not_attempted` buyer **402 Payment Required** about a
charge payments never bound. Answering terminally from here means reimplementing that mapping at a
second site, where it can drift from the one at `store/recovery.go:279` that decides the order's
real ending.

### Where the buyer's non-202 answer actually comes from — two exits, not one

`release_pending` has three exits, and being exact about them is the whole content of this
amendment, because the two non-terminal ones **park differently**:

| Exit | Mechanism | Row afterwards | Buyer sees |
|---|---|---|---|
| Release succeeds | `MarkReleased` | terminal failure status | 402 / 408, the ordinary ending |
| Release refused — the claim is **confirmed** while payment did not capture | `releaseAndFail` → **`ParkForReconciliation`** (`recovery/runner.go:556-565`) | **`status='reconciliation_required'`**, `recovery_parked_at` set | 409, through the `reconciliation_required` branch |
| Attempt budget exhausted | `fail` → **`ReleaseStuckOrder`** (`store/recovery.go:123-133`) | **still `release_pending`**, `recovery_parked_at` set | 409, through the parked check |

`ParkForReconciliation` changes the status; `ReleaseStuckOrder` deliberately does not
(`recovery_parked_at=CASE WHEN recovery_attempts+1>=$4 THEN now() ELSE NULL END`, status untouched; the `+1` is the attempt this statement records).
The second row is the only one that is `release_pending` **and** parked — and it is the reason both
answer paths read `recovery_parked_at` rather than status alone. Reading it as *"both non-terminal
exits leave a parked `release_pending` row"* would make the parked check look redundant on one exit
and absent on the other; it is neither.

Both parked shapes answer **409 "order awaiting payment reconciliation"**, because parked means the
same thing to a buyer either way: no worker will advance this without a human. `ClaimStuckOrders`
excludes parked rows (`store/recovery.go:79`, and the claimable index in `0004`/`0005`), so that is
a statement about the system, not a guess.

### What ends the 202, and the narrow thing the attempt budget actually bounds

**The 202 ends when a claimed re-drive reaches the failure-release path with the budget spent.**
That sentence is deliberately narrow; every wider reading of it is false, and one of them was in the
first draft of this amendment.

`MaxRecoveryAttempts` is 10 (`store/recovery.go:51`). **At steady state, after migrations, the
counter moves in two places at runtime, and both are after a drive.** TKT-300 moved the charge from
claim time to release time. The sentences in this amendment that placed the charge at claim time, or
that said an abandon refunds it, were true before TKT-300 and are corrected here. (A third writer is
a one-off: migration `0005_psp_recovery.sql:38` resets it to zero for the two populations it
re-opens, which is an upgrade-time transition, not a runtime path.)

- `ReleaseStuckOrder` **increments** it, in the same statement that decides parking. A failed drive
  is charged here and nowhere else.
- `UnparkOrder` **resets it to zero** (`:535`) — an operator intervention, not a runner path, and
  the reason its comment calls that reset "not cosmetic" is that `ReleaseStuckOrder` would otherwise
  re-park the row on its next failure.

`ClaimStuckOrders` does not touch the counter, and neither does `AbandonRecoveryClaim`. A claim is
not an attempt. An undriven hand-back costs nothing, so it needs no refund. At runtime the budget is
therefore spent only by failed drives, and cleared only by an operator. Parking happens in
`ReleaseStuckOrder`, whose parked flag is read back from its own statement (TKT-322), or in
`ParkForReconciliation`. Reaching either is not sufficient, for the reason below the table.

**How a claimed pass can end.** Do not read this as a list of failures. The first two rows are the
ordinary outcomes. Since TKT-300, neither of them charges an attempt, and neither does a claim:

| Outcome of a claimed pass | Attempts afterwards | Parked? |
|---|---|---|
| Drive **succeeded** — terminal state reached, `ClearRecoveryClaim` | unchanged (the row is done) | No — the row is done |
| Drive routed to `ParkForReconciliation` and it succeeded | unchanged | **Yes**, at *any* attempt count — this exit does not wait for the budget |
| Drive failed, `fail` ran, `ReleaseStuckOrder` **succeeded** | **+1** | **Yes** when the release's own statement parks the row, once the count reaches 10. The runner logs parked only on that result (TKT-322). |
| Drive failed, `fail` ran, `ReleaseStuckOrder` **failed on a write error** | unchanged, or **+1** if the write committed and its result was lost | **No, as logged.** The runner logs `release stuck order` at ERROR and stops. If the write committed, the row may be parked with no parked line. |
| Drive failed, `fail` ran, the claim was **superseded** (the release matched no row) | unchanged for this claimant; the successor owns the row | **No** — the runner logs `recovery claim superseded` at INFO and stops (TKT-322) |
| Runner stopped **before** claiming | untouched | No — nothing advances the row at all |
| Orderly shutdown, order in the **undriven suffix** | unchanged; `AbandonRecoveryClaim` releases the lease | No, and correctly so |
| Orderly shutdown **during a drive** | **+1** when the drive's failure reaches `fail`, which uses a fresh five-second context | Only if that charge reaches 10 |
| **Crash after claiming** | unchanged | No — the lease holds the row for the full lease, then it is claimable again |

**And every row that says "succeeded" means the SQL returned no error, which is not the same as
having changed the row.** That was the state before TKT-322, and the gap was open. `ReleaseStuckOrder`
and `AbandonRecoveryClaim` are both fenced on `recovery_claim_id=$2`, and neither checked whether its
statement changed a row. A claimant whose lease lapsed, and whose row was re-claimed by a successor,
got `nil` back from a statement that matched nothing. The fencing is correct. It is what stops a stale
claimant from disturbing its successor. TKT-322 closes the gap in both functions: a zero-row match now
returns `ErrRecoveryConflict`, as `ParkForReconciliation` already did. **`ClearRecoveryClaim` still has
the same unobserved zero-row match.** It is out of scope for TKT-322 and is recorded as a separate open
item.

Two rows deserve emphasis. The `ReleaseStuckOrder` **errored** row used to be a trap. Before TKT-322,
`fail` logged *"stuck order parked after exhausting recovery attempts"* whenever the attempt count read
at claim time, plus the failure being recorded, reached `MaxRecoveryAttempts`. That sum was already the
boundary calculation before TKT-322. What TKT-322 changes is the source of the message: the database
result, not that sum, decides it. The old message **did not** depend on whether the parking write
succeeded or matched a row. So the log could assert a park that did not happen. TKT-322 removes
that. The parked line now comes only from the release's own result, and a write error is logged as a
release failure and nothing else. The last row is the liveness one. A runner that crash-loops just
after `ClaimStuckOrders` charges no attempt, because a claim is not an attempt (TKT-300). Each such claim
still holds `recovery_lease_until` for the full lease — `batch × MaxCallsPerOrder × callTimeout + 60s`
(`recovery/runner.go:197-206`), which at the defaults (16 × 6 × 10s + 60s) is **exactly 17 minutes** —
before the row is claimable again. Nothing counts those claims. So such an order can be claimed more than
ten times and stay unparked indefinitely.

**And even on the ordinary path, ten attempts is not a deadline.** It bounds failed claimed
re-drives, not elapsed time, and several things sit between the two:

- the ticker period, configurable via `RECOVERY_INTERVAL`, default 30s
  (`workerConfigFromEnv` in `cmd/commerce/main.go`);
- `recovery_next_attempt_at`, an exponential backoff — and note the **effective ceiling is 256
  seconds, not five minutes**: the SQL is
  `least(make_interval(secs => power(2, least(recovery_attempts, 8))), interval '5 minutes')`
  (`store/recovery.go:129`), and the inner cap at 2^8 means the five-minute operand can never win;
- a two-minute `updated_at` grace that keeps recovery off live checkouts (`store/recovery.go:84`);
- `recovery_lease_until`, which withholds a claimed row for the lease above;
- batch size (`RECOVERY_BATCH`, default 16), so a backlog delays a given row.

Under normal operation this resolves in a small number of cycles. **The buyer's wait has no
wall-clock bound, and this ADR does not claim one.**

An HTTP-level deadline is therefore not simply redundant, and should be judged on its own merits
rather than dismissed here. The narrow objection that does hold: answering terminally reports a
*declined payment* for an order that is merely unattended, which is a false statement about money
rather than a pessimistic one about time. Whether a product-level deadline with some **other**
buyer-facing wording is worth having is left open.

### The residual, and exactly how much of it is observable

A runner that stops advancing orders leaves a buyer on 202 indefinitely. A terminal status code does
not fix that — it converts an accurate "still working" into an inaccurate "declined" while the order
stays exactly as stuck. What *would* help is noticing, so be precise about what notices today.

**Partly observable.** A runner that cannot claim logs `claim stuck orders` at ERROR on every
non-cancellation error (`recovery/runner.go:243-249`), and a database outage severe enough to cause
that also fails the health probes (`cmd/commerce/main.go`, `mountHealth`).

Do not read that as *"a claim error costs nothing"*, though. The lease is set **inside** the UPDATE CTE
(`store/recovery.go:92`), and the error can surface afterwards from `Scan`/`rows.Err` — so a statement
PostgreSQL committed whose result stream was then lost leaves the lease durable while `RunOnce` drives
nothing and returns 0. Since TKT-300 that durable state is the lease alone, and no attempt is charged.
**A claim failure is outcome-ambiguous** unless it is known the statement never executed, and the
ambiguous case lands in the unparked-and-unmeasured population below.

**Not observable.** A recovery goroutine that is starved, deadlocked, or never started inside an
otherwise healthy process — and the crash-after-claim shape above — produce **no signal at all**.
No metric measures the count or age of *eligible, unparked* recovery rows, which is the quantity
that grows when nothing is draining. `ReadRecoveryBacklog`, which feeds every `commerce.recovery.*`
gauge, is scoped `WHERE recovery_parked_at IS NOT NULL` (`store/recovery.go:601-613`), so unparked
rows are excluded by construction.

Two corrections to how those gauges are sometimes described, since this amendment relies on them:
they do **not** correspond to the two exits above, and neither is scoped to `release_pending`.
`commerce.recovery.parked` counts parked rows whose status `IS DISTINCT FROM
'reconciliation_required'`; `commerce.recovery.parked.reconciliation_required` counts the rest.
Together they **partition every parked row**, with no `status IN (…)` restriction at all — so a
parked row outside the five claimable statuses would be counted too.

**A loose precedent exists, and its differences matter as much as its shape.** The sibling reversal
runner publishes `commerce.refund.reversal.outstanding` beside its parked gauge, with an
`oldest_age_seconds` (`reversal/metrics.go:26-38`) — so it does have the *outstanding-work* signal
recovery lacks. It is **not** a drop-in model, and copying it uncritically would import the wrong
population: that gauge counts every outstanding obligation **parked included**, so a sustained nonzero
can mean permanent parked work awaiting a human rather than a downstream failing to recover. The charge
point is no longer a difference. Its runner charges an attempt only **after** a failed drive
(`ReleaseReversalClaim`), and since TKT-300 recovery does the same. The two runners share that charge
point, so the difference that matters is the population each gauge counts.

State recovery's requirement directly instead of by analogy: **the count and age of rows in a
claimable status with `recovery_parked_at IS NULL` that are eligible now** — past
`recovery_next_attempt_at`, past the `updated_at` grace, lease lapsed or absent. That is the
quantity that grows when nothing is draining, and it is the one signal that would surface every
unparked shape in the table above, the ambiguous claim failure included. **Out of scope here,
recorded as an open item** — not as an implied guarantee.

### What this amendment does not claim

Per ADR-021's name-the-claim discipline, and because prose is not compiled:

- **Not** that every 202 eventually becomes terminal. A wedged runner is the case above.
- **Not** that the buyer's wait is bounded in wall-clock time. `MaxRecoveryAttempts` bounds failed
  *claimed* re-drives; the schedule, the backoff, the batch size and whether the runner is running
  at all sit between it and any elapsed-time figure.
- **Not** that a wedged runner is observed. It is not — see the residual above.
- **Not** that `terminal_outcome` proves the order failed. It proves **no capture**, and that is all
  it is read for here.
- **Not** that the two paths agree by construction. They agree because
  `TestParkedReleasePendingGetsTheSameAnswerFromBothPaths` compares them to each other on one seeded
  row; that test is the tripwire, and it was written precisely because an earlier comment claimed
  the agreement was structural and a one-sided fix falsified it.

## Amendment (2026-10-04, TKT-285) — a zero-total order is classified by the operation lookup, and a PSP-skipped order is completed, not released

A zero-total checkout skips the PSP (ADR-011, TKT-285 amendment), so payments holds no operation for
the order. The old reading of "no operation" in `resolveCreated` is "payment was never attempted":
record `not_attempted` and release the seat. Applied to a PSP-skipped order that was already
finalized, it would silently cancel a sale the buyer was promised. This amendment closes that.

### Decision

The discriminator is the persisted gross total (`StuckOrder.Amount == 0`). It narrows the decision
table. It does not replace the operation lookup, which stays the evidence (§2).

| Zero-total order state | Resolution |
|---|---|
| `created`, lookup **error** (transport, 5xx) | Retry with backoff. Never `not_attempted`, never release. |
| `created`, lookup **found** | A legacy order that bound an operation. Every existing `resolveCreated` branch applies, unchanged: operation status, provider status for an unresolved operation, and compensation for proven captured money with a gone claim. |
| `created`, not found, `order.created` fact present | Replay `order.created`, confirm, submit `order.completed`, run `CompleteOrder`, clear the claim. |
| `created`, not found, no `order.created` fact | Record `not_attempted` and release. The same path as a paid order with no operation. |
| `confirmation_pending`, lookup **error** | Retry with backoff. |
| `confirmation_pending`, lookup **found** | Park for reconciliation in one pass (D22). No producer creates this state: the old charge path could not answer 200 for zero, and checkout writes a zero `confirmation_pending` row only for an order it inserted, which has no operation. Parking is uniform rather than per-outcome: a resolved `captured` operation could still complete through `confirmAndComplete` (`CompleteOrder` accepts `confirmation_pending`), but the `declined` and `timeout` branches cannot finish, because `RecordTerminalOutcome` excludes `confirmation_pending`. |
| `confirmation_pending`, not found | The same completion as above, with no intent read: confirm only runs after the intent fact was written. |
| `payment_unknown`, `reconciliation_required`, `release_pending` | Existing paths, unchanged. |

The `order.created` fact is the proof that checkout reached the buyer-details write: checkout writes
`buyer_pii` before it. The runner reads it with a pure `SELECT` on `order_facts` by the deterministic
fact ID. No migration is needed.

A gone claim during the PSP-skipped completion **parks** the order for reconciliation. It never enters
the captured-money compensation arm, because nothing was captured. The PSP-skipped completion calls
payments for the operation lookup and for fact submission only. It never calls status, void, refund
or charge.

### One classifier, in recovery (D21)

Operation evidence classifies a zero-total order in exactly one place: the recovery runner. Checkout
keeps no second classifier. Checkout skips the PSP only for an order **this request inserted**:
payments binds an operation only after commerce has inserted the order, so a new order cannot have
one. A checkout that **resumes** an existing zero-total order (`created`, `payment_unknown` or
`confirmation_pending`) answers `202` with the order's durable status and does nothing else. It makes
no lookup, writes nothing (no buyer details, no fact, no finalize, no status change), and calls neither
inventory nor payments. `claimOrder` also does not refresh `updated_at` for a zero-total resumed order.
Recovery's two-minute grace runs on that column, so a refresh would let a buyer who replays every 30
seconds keep recovery from ever claiming the order. This is the same reasoning the code already applies
to `release_pending`. A paid resumed order still has its `updated_at` refreshed, as before.

Recovery treats a zero-total `created` **or** `confirmation_pending` order the same way up to the
lookup: it calls the operation lookup first, and a lookup error retries. After a found operation the
two statuses differ. A `created` order takes the existing evidence branches. A `confirmation_pending`
order **parks** at once (D22), for the reason in the table. Not found takes the PSP-skipped path
described above, and it does not always complete. A crash after the order insertion but before the
`order.created` fact leads to `not_attempted`, a release, and a `timeout` answer. A gone claim parks.
Only an order that has its intent fact and a live claim completes. A buyer whose zero checkout crashed
mid-way sees `202` on a replay until recovery resolves the order, as for a paid order stuck in
`payment_unknown`.

### Rejected: a durable "PSP skipped" marker

A column on `orders` would need a data migration, and the lookup already supplies the evidence. It
also leaves legacy orders alone: a zero-total order that bound an operation before this change is
resolved by operation evidence, as any other order is. The zero-total dispatch does read one thing
the lookup does not prove: that the persisted total is the total the buyer was quoted. No production
code updates `reservations.total_amount` after insert (checked by `grep` for `UPDATE reservations`
statements that write an amount; none found). If code ever starts to do so after a charge, the
dispatch needs a durable marker. The adversary is the honest writer (ADR-021): a database writer can
change the total, and nothing here defends against that.

### Known residuals (not fixed here)

- **Unfenced checkout against recovery.** Checkout's writes after `claimOrder` are not fenced by the
  recovery claim. A zero checkout stalled for over two minutes before its intent fact can be released
  as `not_attempted` while the request later resumes. A paid order has the same interleaving today
  (TKT-319, claim and lease contract). The zero version cannot capture money.
- **Buyer copy.** A `not_attempted` release replays as 408, which the storefront shows as "Payment
  declined". This change adds one more producer of that answer, a zero order with no intent fact.
  The buyer-facing fix is TKT-503.
- **Paid recovery journal.** Recovery still completes a PAID order without submitting
  `order.completed`. Only the PSP-skipped path submits it.

## Amendment (2026-10-10, TKT-322) — a release reports whether it parked the row, and a superseded claim is reported as one

Before this amendment, the runner's log decision used the claim-time snapshot of the attempt count, plus
one for the failure being recorded, regardless of the release write's result. So the log could say
*parked* for a row the write did not park, and a release that matched no row could look like a failed
write. The write, not that snapshot, decides the real state. TKT-322 changes what the store reports, and
the runner now reads that report. The accounting that TKT-300 changed is corrected in place in the
TKT-145 amendment above.

### Decision D1

1. `ReleaseStuckOrder` returns the parking result of its own statement. The statement ends with
   `RETURNING (recovery_parked_at IS NOT NULL)`, so the value is the one the row now holds. The caller
   does not compute it.
2. A release that matches no row returns `ErrRecoveryConflict`. `AbandonRecoveryClaim` returns the same
   error for a zero-row match, and it propagates its other errors.
3. The claim predicates, the backoff arithmetic and the parking threshold are unchanged.

### Log outcomes

| Release result | Log line | Level |
|---|---|---|
| `ErrRecoveryConflict` | `recovery claim superseded` | INFO |
| Any other error | `release stuck order` | ERROR |
| Success, parked | `stuck order parked after exhausting recovery attempts` | ERROR |
| Success, not parked | `re-drive stuck order` | WARN |

The attempt count from the claim snapshot stays on the log line as an attribute. It never selects the
message.

### What the conflict proves, and what it does not

- **Ownership.** Claim fencing is unchanged. A conflict proves that the claim token the caller supplied
  matched no row. It does not prove who holds the row now. The runner does not read the row again to
  find the owner. This amendment does not claim a verified successor identity.
- **Adversary: none.** This is honest-caller observability. It defends against no adversary. A caller
  that lies, or a writer with database access, is outside this change. The change adds no audit,
  integrity, deadline or liveness guarantee.
- **A write error can be outcome-ambiguous.** A database error can follow a committed write. If the write
  committed and its result was lost, the row may be parked with no parked line. The runner logs the error
  and does not claim that parking did not happen.
- **A zero-row match has other causes.** A claim that another path cleared, or a row that was removed,
  also matches no row. The message keeps the name `recovery claim superseded`, but it states only the
  mismatch.

### Shutdown

A drive that fails after shutdown still releases its claim. The release runs on a fresh context bounded
at five seconds, because the context the drive ran under is already cancelled. An undriven hand-back
continues after an individual failure, including a superseded claim, and it counts only the claims it
actually returned. This is evidenced at the runner tier only. No connected test drives a cancelled write
against PostgreSQL.

### Scope

The buyer-facing 202 decision (TKT-145) is unchanged, and so is the absence of a wall-clock bound.
`ClearRecoveryClaim` has the same unobserved zero-row match. It is out of scope for TKT-322 and remains
open.

### Evidence

- Runner decision tests in `services/commerce/internal/recovery/runner_test.go`: the superseded release,
  the write error, the parking decision by release result, the fresh shutdown context, and the hand-back
  that continues after a superseded claim.
- Connected supersession test in `services/commerce/internal/store/recovery_runner_smoke_test.go`. It
  writes a live successor claim onto the row before the drive, then checks the log and the row after the
  release.
- Connected parking test in the same file. The real release parks the row, and the log matches it.
- PostgreSQL tests in `services/commerce/internal/store/recovery_smoke_test.go` and
  `recovery_unpark_smoke_test.go`: a stale release at the parking boundary and a stale abandon, each
  leaving the successor's row unchanged field by field; a database failure that is not reported as a
  supersession; and the parking boundary from both sides.

## References

- [ADR-011 — Checkout finalization and canonical money journal](./ADR-011-checkout-journal-protocol.md) (recovery story amended by this ADR)
- [ADR-003 — Append-only audit trail](./ADR-003-append-only-audit-trail.md) ("inalterable history" scoped by §Decision 7; §1 compensating entries; §4 NF525-without-schema-changes, whose NF525 characterization ADR-003 itself marks as inferred pending TKT-11)
- [ADR-012 — Ticket issuance](./ADR-012-ticket-issuance-and-qr-credentials.md) (names TKT-43 for outbox recovery) · [ADR-007 — PostgreSQL + NATS](./ADR-007-postgres-nats.md) · [ADR-010 — PostgreSQL claim transaction](./ADR-010-postgres-claim-transaction.md)
- [ADR-062 — Refund reversal reconciliation](./ADR-062-refund-reversal-reconciliation.md) (§2 *observed, not predicted* is the precedent the TKT-262 amendment applies; §4 assigns unparking to TKT-146)
- [ADR-032 — Stripe behind the PSP port](./ADR-032-stripe-behind-the-psp-port.md) (the status/refund contract `resolveReconciliation` reads; its TKT-115 amendment shipped the same-pass refund)
- TKT-116 (the 202 branch this ADR's TKT-145 amendment records) · TKT-280 (`TestParkedReleasePendingGetsTheSameAnswerFromBothPaths`, the tripwire that pins both the parked 409 and the unparked 202)
- TKT-285 (the PSP-skipped zero-total order) · TKT-319 · TKT-502 · TKT-503
- TKT-43 · TKT-28 (the walking skeleton this hardens) · TKT-11 (fiscal archive; owns the anchor choice) · TKT-33 (PII erasure machinery)
