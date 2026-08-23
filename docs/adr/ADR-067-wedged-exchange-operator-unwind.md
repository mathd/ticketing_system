# ADR-067: A wedged exchange is unwound by an operator, and the refusal is decided by payments

Date: 2026-08-23

## Status

Accepted (TKT-255; decision taken under `config.gates: autonomous`, so gates 2–4 were the agent's —
the plan-review critique, the self-made decisions and any overridden objections are on the ticket).

Closes the gap [ADR-039](./ADR-039-exchange-settles-the-difference.md) leaves open and that
TKT-167's adversarial review found and pinned. Inherits ADR-039's exchange lifecycle,
[ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)'s adversary language,
[ADR-022](./ADR-022-out-of-band-service-migrations.md)'s migration placement, and
[ADR-032](./ADR-032-stripe-behind-the-psp-port.md)'s provider-neutral evidence reads. Deliberately
does **not** extend [ADR-063](./ADR-063-exchange-reversal-reconciliation.md) — §2 says why.

## Context

An exchange records its basis, and its inventory target claim then goes terminal — `expired`, or an
explicit `finalizing -> released`, which `Postgres.Transition` accepts. Inventory refuses every
transition out of a terminal state, so `finalize` answers conflict and commerce answers
**409 "exchange target is unavailable"**, forever.

The durable `order_exchanges` row is what makes that permanent rather than merely annoying. Two
readers treat *any* row for a source order as a live exchange:

- `order_exchanges_one_per_source` (migration 0010) blocks a corrected attempt;
- `BindOrderRefund` counts `order_exchanges` rows for the source with **no state predicate at all**
  and answers `ErrOrderNotRefundable`.

So the source order is stuck in both directions, and nothing in the service can unstick it —
migration 0010 records the reason in its own comment: an exchange has no cancelled or inactive
state. When the terminal transition follows a successful capture, the buyer is **charged**, holds no
target inventory, and has no recovery.

Both states were executed and pinned as tests by TKT-167 rather than argued, and that history is the
reason this ADR is careful about what it claims. Three successive attempts to bound the hazard were
each refuted by the next review pass: *"expiry cannot strand money"* (false — an explicit release
can), *"no in-system caller can reach it"* (false — a shared-`ExchangeID` bind race could, and was
closed inside TKT-167), and finally the current bound, which is that the remaining producers are
outside this system's callers. That third bound is executed, and should still be treated as a bound
that could fall.

## Decision

### 1. An operator CLI command pair, not a sweep and not part of the resume

`commerce list-wedged-exchanges` and `commerce unwind-exchange <organizer> <exchange> <reason>`,
following commerce's own `list-parked` / `unpark-order` shape (TKT-146) down to the durable-evidence
table. Three alternatives were considered and each is wrong for a reason this codebase already
recorded:

- **Not a background sweep.** ADR-063 §2's load-bearing lesson is that a runner must never assert a
  fact it cannot establish. *"This exchange should be abandoned"* is a judgement, not an
  observation, and the wedge is not self-healing, so there is nothing for a runner to wait for.
- **Not part of the resume.** ADR-039 §3c makes the resume's contract *"drive the same persisted
  basis to completion"*. Abandoning is the opposite of that contract, not an extension of it.
- **Not an HTTP endpoint.** `recovery_operations.go` states the rule this repo follows: a command
  that must work during an incident should not depend on the service being up. It also avoids three
  separate route audits (`staff_credential_test.go`'s router walk, `smoke/coverage_test.go`'s
  empty-allowlist 2xx coverage gate, and the cachetier `Cache-Control` audit in another module).

### 2. The refusal is decided by PAYMENTS, and the delta's SIGN selects which record to read

This is the part that is easy to get wrong, and getting it wrong deletes a refunded buyer's binding.

A commerce flag cannot answer *"did money move?"* — the wedge exists **precisely because** the
charge succeeded and the row was never updated to say so. So the unwind asks payments, and which
question it asks depends on the sign of the delta, because an upgrade and a downgrade record their
money in **different tables reached by different endpoints**:

| Delta | What settlement did | Where the evidence lives |
|---|---|---|
| `> 0` | charged `exchange-charge:<id>` | `GET /internal/operations` |
| `< 0` | refunded through `exchange-refund:<id>` | `GET /internal/refund-legs` |
| `= 0` | called nobody at all | nothing to read |
| no basis | never reached payments (the basis precedes the provider call) | nothing to read |

A check consulting only `/internal/operations` finds nothing for **every downgrade** — the
operations endpoint answers 404 for a key it never bound — and 404 is the one answer that permits an
unwind. The defect would present as a clean success. It is pinned at two tiers: against a fake port
in `internal/exchangeunwind`, and against the real HTTP shapes in the api smoke suite, where the
fixture answers 404 on one endpoint and 200 on the other for the same exchange.

Both reads are strictly read-only — they bind nothing, call no provider and append no fact — which
is what makes them safe for this use. The port the unwind holds exposes exactly those two methods:
it is structurally incapable of moving money, the same guarantee ADR-063 §2 builds for
`DriveExchange`.

### 3. 404 is the ONLY proof of absence

Every other answer refuses. A 200 with no capture evidence, a 200 that is resolved-but-declined, a
bound-but-uncompleted refund leg, a 5xx, a transport failure, a body with trailing content — all
**indeterminate**, all refusing, and all reported distinguishably from *"the buyer was charged"*,
because the two send an operator to different places.

The `MoneyEvidence` zero value is `Indeterminate` deliberately: a caller that forgets to decide, or
a future branch that falls through, refuses rather than permits. The guard fails closed by
construction rather than by discipline.

A resolved-but-declined operation moved no money and is nonetheless refused. That is a deliberate
under-approximation: concluding it is safe means reasoning about provider semantics from a status
string, and the cost of being wrong is a charged buyer's binding deleted. The consequence — a wedge
that stays wedged — is visible and reversible; the alternative is not.

### 4. A delete, not a tombstone column

A nullable `unwound_at` would have to be excluded by every reader of `order_exchanges`, and the two
that matter are exactly the two whose purpose is to treat any row as live: the unique index (which
cannot be made partial without changing what *"one exchange per order"* means) and the refund count.
A tombstone turns one invariant into two predicates that must agree forever.

**Nothing is orphaned, and the schema guarantees it rather than the application.** No table anywhere
references `order_exchanges`. An unsettled row — the only kind that can be unwound — necessarily has
`replacement_order_id IS NULL` (`order_exchanges_settlement_shape`) and `tickets_exchanged_at IS NULL`
(`order_exchanges_switch_after_settlement`). It owns no order, no switch and no capacity obligation,
and it is invisible to ADR-063's sweep, whose every claim conjunct and gauge filters
`settled_at IS NOT NULL`.

Migration `0024` records the pre-state — the money shape, whether a basis existed, the target hold,
the buyer's idempotency key and the actor — because the row it describes is about to be destroyed.
Its Down fails closed once evidence exists, and that guard is stronger than 0023's for a reason: an
unpark row describes an order that still exists, while an unwind row is the **only** account of an
exchange that does not.

### 5. The lock is the SOURCE ORDER's, and the residual is stated rather than claimed away

`UnwindWedgedExchange` takes `FOR UPDATE` on `orders` — the lock `BindOrderExchange` and
`BindOrderRefund` both take, and which `refunds.go` calls *"what makes the check meaningful rather
than advisory"*. A resume cannot reach `completeExchangeFromBasis` without passing
`BindOrderExchange` first, so it queues behind the same lock. Locking the `order_exchanges` row
instead would lock the artefact rather than the identity that arbitrates access to it — the mistake
[ADR-029](./ADR-029-seat-identity-pinning-contract.md) documents for seat maps, where a blocked
writer rechecked a stale row.

**What that does not buy:** the payments read cannot be atomic with the delete, because payments is
another service and no lock here reaches it. Holding the order's row lock across a network round
trip would block every checkout for that order on payments' latency. The honest guarantee is
therefore *"the evidence was true when it was read, and the write window is one transaction wide"* —
never *"impossible"*, which the acceptance criteria's wording invites and which nothing here can
deliver.

### 6. Four predicates, four refusals, reported separately

Existence, unsettled, no-money, non-blank reason. The order matters the way `UnparkOrder`'s does:
reporting *"money moved"* for an exchange that does not exist sends an operator hunting for a charge
that was never made. The blank reason is refused before a transaction opens; the state is re-read
**under the lock** rather than trusted from the caller's earlier read; the `DELETE` carries
`settled_at IS NULL` as defence in depth, and a zero-row result aborts rather than committing
evidence for an intervention that did not happen.

## Consequences

### What this does not cover, deliberately

- **Compensating a charged buyer.** Out of scope, and the refusal in §2 is what enforces it.
  Choosing between refunding them and re-selling them a target is a product decision nobody has
  taken, and ADR-039 §2's *"an exchange has no safe partial state"* is why it cannot be settled by
  default. `TestAnExchangeChargedThenReleasedIsWedgedWithTheBuyerPaid` still asserts zero refund
  submissions — an outcome this ADR states rather than an assertion left stale.
- **Resurrecting the exchange onto a fresh target claim.** That is a new sale at a price the buyer
  never agreed to.
- **Verifying that the target claim is actually terminal.** Commerce holds no copy of inventory's
  claim state, so the listing reports **candidates** and the CLI says so: an exchange in flight right
  now is unsettled for a few hundred milliseconds and looks identical. The operator establishes
  terminality in inventory before acting. The listing returns `created_at` so a second-old row is
  distinguishable from a week-old one.
- **Unparking.** TKT-146 owns that surface for parked recovery orders, and ADR-063 deferred the
  refund side to it for the same reason: a third bespoke unpark surface would guarantee three.

### What an operator can do wrong, named rather than discovered at review

The command will unwind a **healthy** exchange whose target claim is fine, because it cannot check.
Two things make that acceptable and both are present: the money guard means the worst case is losing
an unsettled, unpaid binding the buyer can simply re-request with a new idempotency key, and the
reason is mandatory and durable.

### Integrity language — name the adversary (ADR-021)

Everything above is **honest-operator consistency**. It holds against concurrency, crashes and
retries between commerce's own callers. It is **not tamper-evidence**: anyone holding commerce's
database credentials can delete an `order_exchanges` row directly and leave no evidence, or forge or
delete rows in `order_exchange_unwinds`. The signed, append-only payments journal remains the
evidence that money moved — which is exactly why §2 decides the refusal on payments' answer and
never on a commerce flag.

### Testing note — what the api-tier proof is and is not

COS 1 asks for a proof against *"a real terminal claim, not a stubbed conflict"*. The previous
fixture used a boolean that made `finalize` refuse unconditionally, which could not distinguish *"the
claim is terminal"* from *"the stub was told to say no"* and would have stayed green against a
perfectly healthy claim. The stub now carries inventory's **transition table, transcribed from
`Postgres.Transition`** including its three already-satisfied cases, and the tests drive a claim
terminal by releasing it, as the threat model's service-token holder would.

That is a **reproduction** of inventory's rule, not inventory itself: commerce's api smoke package
gets a database and stubs, and no exchange test exists at the `smoke/` gateway tier where the real
service runs. Building the first is a candidate follow-up, recorded here rather than glossed —
the distinction between *"the rule is reproduced"* and *"the rule is used"* is one this repo has been
burned by before.

### Operational

Migration `0024` runs out-of-band via commerce's `migrate` subcommand (ADR-022). `unwind-exchange`
needs `DATABASE_URL`, `PAYMENTS_URL` and `PAYMENTS_INTERNAL_TOKEN` (falling back to
`INTERNAL_SERVICE_TOKEN`, as `internalTokenFor` does); it refuses up front when payments is not
configured, because a command that cannot ask payments cannot do its job. No index was added for the
listing — 0023 declined the same thing for the same ADR-019 reason, and that decision is recorded in
the migration rather than left implicit. Operator surface and the meaning of each state:
`docs/development.md` § Wedged exchange operator unwind.
