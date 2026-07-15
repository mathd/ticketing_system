<!--
    Architecture Decision Record.
    Ref: https://www.architectviewmaster.com/blog/building-an-architecture-decision-record-adr-library/
-->

# ADR-017: Domain-event schema evolution — when additive stops being safe

Date: 2026-07-15

## Status

Accepted (TKT-59; promotes a learning proven by TKT-53/PR #37)

Amends [ADR-009](./ADR-009-contract-first-apis.md) §5 (which defines the envelope's `schema` field
but not how to evolve it) and refines [ADR-014](./ADR-014-typed-dated-slot-implementation.md) §3
(which decided "additive optional fields are backward compatible; a bump is reserved for a breaking
change"). Neither is superseded: ADR-014 §3 was correct for `kind`, and this ADR says why `kind` and
`capacity_group_id` land differently.

## Context

ADR-009 §5 fixed the envelope — `{id, type, occurred_at, schema, data}`, `schema` an integer version
of `data`'s shape — but is silent on when that integer must change. ADR-014 §3 filled the silence
with a working rule: **add optional fields additively at the current schema; reserve a bump for a
breaking change.** It applied that rule to `kind` and explicitly weighed a `2→3` bump against it,
rejecting the bump because "inventory's consumer hard-switches on `Schema == 2` and rejects 3, so a
bump would stop provisioning unless inventory changes in lockstep."

TKT-53 (festivals) then hit the case the rule doesn't cover. Its new field, `capacity_group_id`, is
optional and additive in shape — but it **changes what the consumer does**: inventory provisions the
shared-capacity pool keyed by `capacity_group_id` instead of `performance_id`
(`services/inventory/internal/consumer/consumer.go`). Had that field ridden along at Schema 2, an
inventory build that predates festivals would have parsed the event, ignored the unknown field,
provisioned a **per-performance pool that should not exist**, and ack'd it — silent, durable,
wrong-by-construction inventory. "Backward compatible" held at the parser and broke at the semantics.

TKT-53 instead bumped to Schema 3 and taught inventory both variants **in the same change**
(`3879c13`): the producer's Schema 3 fork and the consumer's Schema 3 arm landed together, so no
deployed consumer ever met a variant it didn't know. ADR-014's lockstep objection was therefore
answered by *making* the change lockstep — not by sequencing two deploys, which this repo cannot do
(see §4).

## Possible Solutions

- **Keep ADR-014 §3 as-is, treat TKT-53 as a one-off.** Cheapest, and wrong: the next field that
  changes a consumer's routing or identity keys hits the same trap with no rule to stop it.
- **Bump on every new field.** Unambiguous, and expensive: every consumer grows a case arm for
  changes that cannot affect it. ADR-014 §3 rejected this for good reason — `kind` genuinely needed
  no bump, because inventory does not fork on it (ADR-005).
- **Draw the line at parse compatibility** ("bump only if old consumers fail to parse"). This is the
  rule that TKT-53 would have defeated: the dangerous payload parses perfectly.
- **Draw the line at consumer semantics** (chosen). Costs a judgment call per field, but it is the
  line the failure actually falls on.

## Decision

1. **Additive-without-bump remains the default** (ADR-014 §3 stands): a new optional field may ride
   at the current schema when **every deployed consumer can ignore it and still be correct**, and
   every existing field keeps its meaning and validation. `kind` qualifies — inventory does not fork
   on it.

2. **Bump the schema when the payload changes what a consumer *does*, not merely what it stores.**
   Concretely, a bump is required when the change alters **identity, routing, idempotency, or
   ownership keys** (TKT-53: the pool key moves from `performance_id` to `capacity_group_id`),
   introduces a **required field or a cross-field invariant**, or changes the meaning of an existing
   field. The test is not "will old consumers parse it" — the TKT-53 payload parses fine — but
   **"would an old consumer, ignoring this field, do the wrong thing and ack?"** If yes, bump.

3. **Schemas are coexisting variants, not a producer version counter.** The producer chooses per
   emission from the payload's own shape — catalog emits Schema 3 only when `capacity_group_id` is
   set, Schema 2 otherwise (`services/catalog/internal/events/events.go`). Consumers switch on the
   variant and **validate the variant's invariants**, including negatively: Schema 2 must *reject*
   festival fields rather than tolerate them (`consumer.go`). A tolerant old arm is how a
   mis-versioned event becomes silent corruption.

4. **Ordering — three distinct cases, only one of which "consumer-first" solves.**

   a. **Forward rollout.** Where producer and consumer *can* run at different versions (rolling
      deploys, independently deployed services), the consumer that understands schema N+1 ships
      before the producer emits it. **This repo cannot deploy that way today** — one
      `docker compose up`, whole stack as a unit, no release pipeline — so it satisfies the
      requirement the only other way available: **land both sides in one change**, as TKT-53 did.
      Either discipline is acceptable; emitting N+1 with no consumer that understands it is not.

   b. **Rollback and consumer recreation — consumer-first does *not* cover this, and the current
      failure mode is silent.** Once Schema 3 is retained on the stream, reverting inventory to a
      binary that predates it (or rebuilding its durable consumer with one) makes that binary hit
      the `default` arm and call `msg.Term()` (`consumer.go`) — permanently advancing past the
      event with **no provisioning and no retry**. Nothing errors loudly; inventory is simply
      missing. **Therefore: do not roll a consumer back past a schema the stream still retains**,
      and treat "recreate the durable consumer" as equivalent to a rollback unless the binary
      understands every retained variant. This is a real gap, not a hypothetical — see Consequences.

   c. **Ordinary restart is not replay.** A durable consumer resumes from its stored position and
      does **not** re-read acknowledged history, so a normal restart carries no schema risk. Replay
      risk arises only when the position is reset or the consumer is recreated — i.e. case (b).

   Keep the rollout note at the code that depends on it (`consumer.go`'s Schema 3 arm) rather than
   relying on this ADR alone.

## Consequences

- **Positive:**
    - The rule catches the failure that parse-compatibility misses: an event that deserializes
      cleanly and drives the wrong write.
    - ADR-014 §3's reasoning is preserved rather than reversed — its bump objection ("inventory would
      have to change in lockstep") is accepted, not dismissed: §4a makes lockstep the *method* while
      the stack deploys as a unit.
    - Schema 2's explicit rejection of festival fields becomes a documented pattern, not an accident
      of TKT-53.
- **Negative:**
    - "Changes what a consumer does" needs a judgment call per field. §2's enumeration (identity,
      routing, idempotency, ownership, required fields, cross-field invariants) bounds it, but the
      boundary is reviewed, not compiled — a field that silently changes behavior in a way no one
      anticipated can still slip through.
    - Every bump costs an arm in each consumer and keeps old arms alive as long as the stream retains
      them. There is no retirement policy for old schema arms yet; the first removal will need one
      (how far back can the stream be re-read?).
    - **§4b is a known live hazard, documented but unmitigated:** an inventory rollback past a
      retained schema silently drops those events via `msg.Term()` — no error, no retry, just absent
      inventory. This ADR states the rule ("don't roll back past a retained schema"); nothing
      *enforces* it. Making the default arm `Nak`/park instead of `Term`, or gating consumer startup
      on a max-known-schema check, would turn a silent gap into a loud one — **deferred, and worth
      its own ticket.**
    - §4's ordering rests on review, not machinery: no test exercises mixed-version skew, and Compose
      cannot express it.

## References

- TKT-59 (this ADR) · TKT-53 / PR #37 (the proving case) · TKT-14 (consumes the shared-capacity pool)
- [ADR-009](./ADR-009-contract-first-apis.md) §5 (envelope + `schema`) ·
  [ADR-014](./ADR-014-typed-dated-slot-implementation.md) §3 (additive `kind`, bump rejected) ·
  [ADR-005](./ADR-005-unified-dated-slot-admission.md) (inventory does not fork on `kind`) ·
  [ADR-007](./ADR-007-postgres-nats.md) (JetStream; replay is why §4 binds today)
- Reference impls: `services/catalog/internal/events/events.go` (producer picks the variant) ·
  `services/inventory/internal/consumer/consumer.go` (consumer switches + validates negatively)
