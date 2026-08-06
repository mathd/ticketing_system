# A refusal placed after the point of no return is not a safety check

**TKT-217, 2026-08-05.** Found twice in one ticket, by two different mechanisms.

## What happened

The settlement ledger refuses a capture it cannot attribute. Correct rule, wrong place — twice.

**First**, the plan was validated *after* the PSP call. A charge with an unusable plan got an error
back while the provider had already taken the money, leaving no `payment.captured` fact and no
ledger. The deferred triggers could not repair it: they govern journal commits, not the outside
world.

**Second**, after moving validation ahead of the provider, it still sat *after* `BindOperation`. A
bound operation is not inert — it carries no terminal status, which is the `payment_unknown` case,
and the recovery path resolves such an operation against the provider and can complete the order
from it. The refused charge became a capture waiting to happen with no ledger able to record it.

A third variant appeared when the fix over-corrected: validating *before* the idempotency lookup
made replay depend on the settlement plan, which is not part of the request fingerprint. A replay of
an already-captured charge could get a plan error instead of the capture it already had.

## The rule

**Ask what is already irreversible at the point the check runs.** A refusal is only a safety
property if nothing irreversible has happened yet. Money at the provider, a durable row another
process will act on, and a committed fact are all past the line — and the second one is easy to miss
because it looks like bookkeeping rather than an action.

Ordering the charge path took four revisions, each fixing a defect the previous position caused:

    validate after PSP        -> money captured, no fact, no ledger
    validate after Bind       -> recoverable operation with no usable ledger
    validate before lookup    -> idempotency depended on the plan
    fingerprint -> lookup -> validate -> bind -> PSP

## The related trap

The same ticket refused unsplit fees at first. That refusal was correctly placed but wrongly scoped:
fees shipped before split schedules did, so every fee sold in that window had no schedule, and the
refusal failed those sales **at checkout, after the buyer committed**. Recording them as *collected
and unattributed* keeps the ledger balanced and makes the gap queryable — which is what an operator
needs.

**A rule that is right about the state and wrong about the timing costs more than no rule**: it
converts a data-quality problem into a lost sale or lost money.
