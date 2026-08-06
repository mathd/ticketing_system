# ADR-048: The settlement ledger balances against the capture, in the capture's transaction

Date: 2026-08-05

## Status

Accepted (TKT-217; decision taken under the owner-waived gates of that run, recorded on the
ticket). Final deliverable of the TKT-6 epic.

*Consumes [ADR-047](./ADR-047-payees-and-split-schedules.md) and
[ADR-046](./ADR-046-fee-rules-representation.md); amends neither.*

## Context

TKT-6's second condition of success: *the settlement ledger attributes every fee cent to a payee;
splits sum exactly to totals.* ADR-002 puts the ledger in `payments`.

Three tickets already built everything except the ledger: catalog resolves fee rules and split
schedules, commerce composes the money and persists a verbatim snapshot of the whole resolution, and
`splits.Allocate` turns a share into whole cents exactly. What remained was to write down who is
owed what, once, at the moment the money moves.

## Decision

### 1. The identity

    (face − absorbed) + passed_on + absorbed  =  face + passed_on  =  captured

The organizer's line is face value **less the fees they absorbed**; the fee lines are **every** fee,
passed-on and absorbed alike.

**Absorbed fees are the case that gets built wrong**, and the identity shows why: absorbed appears on
both sides. It reduces the organizer's line and is still owed to a payee, out of money the buyer
already paid. A ledger that records only passed-on fees **still balances against the charge** — and
is wrong about who earned what. That is the failure this section exists to prevent, and it is
invisible to any check that only compares the total.

Worked: face 5000, passed-on 600, absorbed 400, captured 5600 → organizer 4600, fee lines 1000, sum
5600.

**A negative organizer line is allowed and recorded.** When absorbed fees exceed the face value the
organizer nets negative — a real, if misconfigured, sale — and the identity still holds. Refusing it
would make **payout configuration able to refuse a purchase**, which is the trade this epic already
declined once on TKT-216. A ledger's job is to be true; reporting (TKT-23) is where that surfaces.

### 2. Entries ride the capture's transaction

`Journal.AppendWithSettlement` writes the fact and its ledger lines in **one** transaction, so the
entries exist **if and only if** the captured fact does.

The alternative considered and rejected was a separate finalization step — a provider-neutral
`FinalizeCapture` endpoint, a PSP port change, and recovery-runner changes — to reconcile the two
afterwards. It was unnecessary: `payment.captured` is appended in exactly one place, the fact id is
deterministic (`uuid.NewSHA1("payment:"+org+":"+key+":"+type)`), and `Append` already returns
`replay=true` from its existing-fact branch **without writing**. So **replay idempotency is a
property of the fact id**, not of a new state machine — and the rejected design would have
restructured the most sensitive path in the system to buy something it already had.

### 3. Sum-exactness at two levels, deliberately

- **Application** (`BuildSettlementEntries`) refuses a plan before the provider is called: an
  unattributable fee, a persisted split that does not balance, a currency mismatch, totals that
  disagree with the fees they claim to describe, a captured amount that differs from the plan.
- **Database**, two deferred constraint triggers: the entry set for a capture must sum to the
  journalled amount, **and** a `payment.captured` fact must have entries at all.

A per-row `CHECK` cannot see a set, and the application builder can be bypassed by direct SQL — so
neither level alone makes the invariant a property of the system. The second trigger also constrains
the generic `/internal/facts` endpoint, which allowlists `payment.captured`; that is **correct rather
than incidental**, because a captured fact with no settlement is exactly what is being forbidden,
wherever it comes from.

Aggregation is in `numeric`, not `bigint`: the organizer line is signed, and a sum that overflowed
while checking for imbalance would be the worst possible failure of this constraint.

### 4. Payee identity is snapshotted

Each entry carries the payee's id, kind, display name and external reference **as they were**, with
no foreign key — catalog is a different service and a different database. A settlement row must keep
saying who was paid at the time they were paid, and a display name is editable. Same discipline as
the price and fee snapshots.

### 5. The integrity claim, in ADR-021's words

**Honest-writer consistency. Not tamper-evidence.**

Settlement entries are append-only — `BEFORE UPDATE OR DELETE` per row and `BEFORE TRUNCATE` per
statement, copied verbatim from `journal_entries` rather than written afresh, because TKT-216 wrote
one from first principles in this same repo and shipped a TRUNCATE hole that review had to find.

But they are **not** in the journal's hash chain. A writer with payments database access can drop
these triggers and rewrite any row. What the schema stops is an application bug and an operator
mistake — which is the realistic way a misattribution would otherwise appear.

**Joining the chain is a real option and is deliberately not taken here.** It would mean canonicalising
entries into the signed payload and deciding what a chain over two tables means for verification.
That is a decision worth taking on its own evidence, not as a side effect of building the ledger.
Until then, do not describe these entries as tamper-evident.

## Consequences

- **Positive:** a capture and its attribution commit together, so "captured but unattributed" is not
  a state the database can hold · the absorbed-fee case is proved by an identity rather than by
  inspection · replay writes nothing new, by construction · the allocator is reused, not
  reimplemented.
- **Negative:** every captured charge must now carry a settlement plan, so a caller that omits one is
  refused — including direct `/internal/charges` fixtures, which had to be updated · the ledger is
  not tamper-evident, and says so · there is no settlement read surface for operators yet (TKT-23) ·
  a fee that resolved `unsplit` refuses the capture, which means payout configuration **can** stop a
  capture even though it deliberately cannot stop a reserve.
- **Not decided here:** reversal. Nothing un-attributes a settled fee on refund, exchange or
  cancellation. TKT-215 left a refund returning face value only, and ADR-046 records that; this
  ledger now makes the other half concrete — the payee attribution of a refunded fee stands. That is
  the epic's largest remaining open question and it is named, not hidden.

## References

- TKT-217 (this ticket) · TKT-6 (epic) · TKT-214, TKT-215, TKT-216
- [ADR-047](./ADR-047-payees-and-split-schedules.md) — the allocator, and §5's two integrity escapes
- [ADR-046](./ADR-046-fee-rules-representation.md) — §2 zero-amount fees, §3 incidence
- [ADR-003](./ADR-003-append-only-audit-trail.md) · [ADR-011](./ADR-011-checkout-journal-protocol.md)
- [ADR-016](./ADR-016-checkout-recovery-state-machine.md) · [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)
- [ADR-002](./ADR-002-services-from-day-one.md) · [ADR-032](./ADR-032-stripe-behind-the-psp-port.md)
