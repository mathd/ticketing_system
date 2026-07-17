# A finding's mechanics being true does not make its trigger reachable

**Seen:** TKT-79 (PR #58), ai-review triage, 2026-07-16.

## The shape

The adversarial reviewer's highest-severity finding on TKT-79 was mechanically correct:
the commerce staff-sale seam repairs a crash between the inventory commit and the
reservation insert by replaying the request, and the replay reconstructs the inventory
idempotency fingerprint from *live catalog data* — so a catalog price change inside a
crash-repair window would strand a committed carve behind a fingerprint mismatch.

Every sentence of that is true. The finding was still not a bug in the shipped system,
because the trigger does not exist: catalog ticket-types are **create-only** at HEAD — no
update endpoint, no mutation path. One grep refuted the reachability of a "high" finding
whose mechanics survived any amount of code reading.

## The rule

Triage a reviewer's finding in two steps, in order:

1. **Mechanics** — is the causal chain real in this code? (Read the code.)
2. **Trigger** — can the initiating event actually happen in this system, today?
   (Grep for the mutation path / config / caller that the chain starts from.)

A finding that passes 1 and fails 2 is not *rejected* — the mechanics become a landmine
the moment someone ships the trigger. Record it as a backlog ticket **gated on the ticket
that would introduce the trigger** (TKT-79 → TKT-91, gated on catalog price mutability),
and say in the triage which grep proved the trigger absent, so the next reader can re-run
it.

## Relation to existing rules

This is the review-side dual of *premises rot between shaping and claim* (TKT-73): there,
a plan's stated premises must be re-verified before building; here, a finding's unstated
premise (the trigger) must be verified before fixing. Both fail the same way — everything
asserted is true, and the conclusion still doesn't hold.
