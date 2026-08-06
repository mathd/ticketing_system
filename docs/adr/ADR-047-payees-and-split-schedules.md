# ADR-047: Payees are rows, splits are basis points, and the cents are allocated

Date: 2026-08-05

## Status

Accepted (TKT-216; decision taken under the owner-waived gates of that run, recorded on the
ticket). Consumed by TKT-217 (the settlement ledger).

*Extends [ADR-046](./ADR-046-fee-rules-representation.md); amends nothing.*
*Respects [ADR-002](./ADR-002-services-from-day-one.md)'s ownership row — see § 1.*

## Context

ADR-046 made a fee resolvable. It did not say **who the fee is owed to**, and TKT-6's second
condition of success needs that: *the settlement ledger attributes every fee cent to a payee; splits
sum exactly to totals.*

The hard part is not the schema. It is that **shares are percentages and money is whole cents, and
the two do not divide.** A 1¢ fee split three ways at 3333/3333/3334 floors to 0/0/0 and the cent
disappears. Every real ticketing settlement has that bug once, and it is unrecoverable after the
fact: nobody reconciles a cent, so nobody notices until the totals have drifted for a year.

## Decision

### 1. Ownership: catalog defines, payments allocates. ADR-002 unamended.

- **catalog** holds the payee registry and split schedules, and resolves them — because a schedule is
  organizer-scoped configuration attached to catalog's own scope hierarchy (venue, event, series,
  slot, ticket type). Resolving it anywhere else means duplicating that derivation.
- **payments** holds the **allocator** and, in TKT-217, the ledger — because that is where the money
  is written.
- **commerce** is untouched by this ticket. It persists catalog's resolution document verbatim
  already, so splits reach the sale's snapshot with no code change.

ADR-002 says catalog owns "rule definitions" and payments owns "settlement ledger". A split schedule
is a rule definition; the allocation is settlement. **The line did not move**, and this section
exists to say so rather than to claim it quietly.

### 2. Payee kinds are rows, not an enum

`payee_kinds` is a table, seeded with `system`, `venue`, `artist`.

A `CHECK` enum would make a new kind a migration. A free-text column would make a typo a new
category. A **foreign key** makes a typo fail loudly and a new kind data — which is what "extensible"
in the ticket means.

**`kind` is descriptive metadata for reporting and must never be read as a routing rule.** Money goes
where the `share_bps` says. A build that infers "the venue gets paid because kind = venue" has
invented a second, contradictory source of truth for where money goes.

### 3. The split rides the fee resolution, and that is a snapshot decision

Each `FeeCodeResolution` carries its `split`. This is deliberately **not** a second endpoint.

TKT-215 persists catalog's fee-resolution document **verbatim** as the reservation's snapshot. Putting
the split inside that document captures it **at sale time** — so a schedule edited afterwards cannot
change who gets paid for a sale that already happened. A second endpoint would have satisfied
"the resolution is readable" while silently leaving settlement to re-resolve later, against mutated
schedules. Same snapshot-not-reference discipline as migrations 0006 and 0014.

**The read stays `/internal/`** (ADR-046 §6, ADR-043). Absorbed fee amounts are the organizer's cost
structure; **split shares are strictly more sensitive than that** — they are who gets paid what.
Publishing them would reverse ADR-046's decision on stronger evidence than it was taken with.

### 4. `unsplit` is a state, with a reason

A fee code resolves `split` (a schedule won) or `unsplit` (considered, nothing applies). The reason
distinguishes:

- `no_schedule` — nobody ever authored one;
- `outside_window` — somebody did, and it stopped applying. **The losing schedules ship**, because
  they are the answer to *"why is this fee not being split?"*.

Only the second is evidence of configuration that needs attention. Collapsing them, or returning an
empty part list, would make an operator's real problem indistinguishable from a fee nobody has
configured yet. This is ADR-046 §9's rule applied one level down.

### 5. Balance is enforced at COMMIT, by a deferred trigger

`split_schedule_parts.share_bps` must sum to exactly **10000** per schedule, and a schedule must have
at least one part.

**Deferred, and it has to be:** a schedule is a header plus N parts, so it is unbalanced for the whole
of its own creating transaction. A per-statement check would make authoring impossible. Checking at
commit is what makes "no unbalanced schedule exists" a property of the **database** rather than of
one code path.

**Name the adversary (ADR-021).** This is **honest-writer consistency, not tamper-evidence.** A writer
who sets `session_replication_role = replica`, or runs `ALTER TABLE … DISABLE TRIGGER`, commits
whatever they like. What it stops is an operator mistake and a buggy write path — which is the
realistic way an unbalanced schedule would otherwise appear.

Tenant integrity is enforced by **composite** foreign keys carrying `organizer_id` into both the
schedule and the payee reference, so a schedule cannot name another organizer's payee. The tenant is
part of the reference rather than a field somebody remembers to check.

### 6. The allocator: largest remainder, exactly

`services/payments/internal/splits.Allocate(amount, shares) → parts`, with one obligation:
**the parts sum to `amount` exactly, for every input.**

Floor each part, then hand the leftover cents to the parts with the largest fractional remainders. At
most `len(shares)-1` cents are ever left over, because each floor discards less than one cent — and
that bound is **asserted at runtime**, not assumed, because "by construction" is exactly the claim
that stops being true when somebody changes the flooring.

**Determinism.** Ties break on payee id, and the output is sorted by payee id. A settlement that pays
a different payee depending on the query plan or the caller's slice order is not one anybody can
reconcile. The id is the only stable identity available: display names and external references are
editable, and row order is not a fact about anything.

**Overflow.** `amount × share_bps` is **never formed** — at a large amount it overflows `int64` even
though the quotient is perfectly representable. TKT-215 hit this exact trap on percentage fees, where
the first implementation multiplied first and refused legitimate inputs, *and a test asserted that
refusal as required behaviour*. The decomposition:

    q, r  = amount / 10000, amount % 10000
    base  = q×bps + (r×bps)/10000        // q×bps ≤ amount
    frac  = (r×bps) % 10000              // r×bps ≤ 99,990,000

**A zero amount still allocates**, naming every payee with 0 — because ADR-046 §2 says a resolved fee
of amount 0 is still a resolved fee, and a payee that vanishes as a function of price is a payee
sometimes owed nothing and sometimes absent.

The allocator **validates its own input** — shares summing to 10000, no duplicate payee, no share out
of range — even though the database guarantees all three for authored schedules. TKT-217 hands it
**persisted snapshots**, and a snapshot is a copy of what was true at sale time, not a row the write
gate is still standing behind.

## Consequences

- **Positive:** the exact-sum property is proved against an oracle that shares no arithmetic with the
  implementation · an unbalanced schedule cannot be committed by any ordinary writer · splits are
  captured at sale time, so settlement reads a fact rather than re-deriving one · payees are
  extensible without migrations.
- **Negative:** a third resolver now duplicates the same ranking axes (ADR-046 §7 set the trigger for
  consolidating at a third *rule kind*; this is the second, so the duplication stands and is now
  costing more) · no authoring API, so schedules can only be created through the store, which means
  the feature is real and unreachable from outside until a back-office ticket · `kind` is
  unenforced metadata and a determined operator can still misclassify.
- **Not decided here:** the ledger and its integrity claim (TKT-217) · payout execution, bank details,
  KYC · what a refund or exchange does to an already-attributed split — which is the same carve-out
  TKT-6 named, now with a concrete consequence: TKT-215 leaves a refund under-refunding the buyer by
  the passed-on fee, and nothing yet reverses the payee attribution of that fee.

## References

- TKT-216 (this ticket) · TKT-6 (epic) · TKT-214, TKT-215 (predecessors) · TKT-217 (consumer)
- [ADR-046](./ADR-046-fee-rules-representation.md) — the fee model; §2 zero-amount fees, §6 the
  internal read, §7 deliberate duplication, §9 considered-vs-absent
- [ADR-036](./ADR-036-pricing-rules-representation.md) — the hierarchy, §3's write gate
- [ADR-002](./ADR-002-services-from-day-one.md) · [ADR-001](./ADR-001-go-typescript-stack.md)
- [ADR-019](./ADR-019-catalog-read-path-scoping.md) · [ADR-020](./ADR-020-catalog-index-build-concurrency.md)
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) — name the adversary
- [ADR-043](./ADR-043-where-a-service-auth-guard-lives.md)
