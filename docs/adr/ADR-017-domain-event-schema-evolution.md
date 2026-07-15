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
repository revision contains one side without the other. ADR-014's lockstep objection was therefore
answered by *making* the change lockstep — not by sequencing two deploys, which this repo has no
mechanism to order (see §5).

## Possible Solutions

- **Keep ADR-014 §3 as-is, treat TKT-53 as a one-off.** Cheapest, and wrong: the next field that
  changes a consumer's routing or identity keys hits the same trap with no rule to stop it.
- **Bump on every new field.** Unambiguous, and expensive: every consumer grows a case arm for
  changes that cannot affect it. ADR-014 §3 rejected this for good reason — `kind` genuinely needed
  no bump, because inventory does not fork on it (ADR-005).
- **Draw the line at parse compatibility** ("bump only if old consumers fail to parse"). This is the
  rule that TKT-53 would have defeated: the dangerous payload parses perfectly.
- **Draw the line at consumer semantics** (chosen, and per event type). Costs a judgment call per
  field, but it is the line the failure actually falls on. In practice it splits into two triggers —
  old-consumer incorrectness and needing a distinguishable variant to validate against — which are
  not the same question and are kept separate in §3.

## Decision

1. **The compatibility unit is `(type, schema)`, never `schema` alone.** `schema` versions *this
   event type's* `data` shape and carries no meaning across types. The repo already proves it:
   `performance.published` and `performance.archived` **each** fork 2→3 on `capacity_group_id`
   (`events.go:146-148` and `:237-239`) — same number, same trigger, **different payloads**
   (`published` carries `shared_capacity`, `archived` does not) — while
   `performance.closed`/`.reopened` sit at a Schema 1 (`:196`) that has nothing to do with
   `published`'s Schema 1. Only `published` has a consumer today
   (`inventory-performance-provisioner`, `consumer.go:101`); the others are versioned against
   future readers, which is the point — a number is a promise to whoever subscribes to *that*
   subject. Every rule below is scoped to the consumers of one type. "Bump to N+1" is only ever a
   statement about one subject; there is no global version line, and two types reaching the same
   number means nothing.

2. **Additive-without-bump remains the default** (ADR-014 §3 stands): a new optional field may ride
   at the current schema when **every deployed consumer of that type can ignore it and still be
   correct**, and every existing field keeps its meaning and validation. `kind` qualifies —
   inventory does not fork on it.

3. **Bump when either trigger fires. They are distinct, and only the first is about old consumers.**

   a. **Old-consumer incorrectness** — the payload changes what a consumer *does*, not merely what
      it stores: the change alters **identity, routing, idempotency, or ownership keys** (TKT-53:
      the pool key moves from `performance_id` to `capacity_group_id`), or changes the meaning of an
      existing field. The test is not "will old consumers parse it" — the TKT-53 payload parses
      fine — but **"would an old consumer, ignoring this field, do the wrong thing and ack?"**

   b. **The variant must be distinguishable to be validated** — the change adds a **required field
      or a cross-field invariant** that the current schema's arm cannot enforce without rejecting
      payloads that are legal at that schema. This fires even when (a) would not: an old consumer
      may ignore the new field harmlessly while the *producer* still needs a separate variant so
      the invariant has an arm to be checked in. TKT-53 fires both — the pool key moves **and**
      `capacity_group_id`/`shared_capacity` are jointly required at Schema 3 (`consumer.go`).

   Do not collapse these into one sentence: (a) is a compatibility fact about deployed binaries,
   (b) is a validation fact about the schema itself. A change firing only (b) still bumps.

4. **Schemas are coexisting variants, not a producer version counter.** The producer chooses per
   emission from the payload's own shape — catalog emits Schema 3 only when `capacity_group_id` is
   set, Schema 2 otherwise, independently for `published` (`events.go:140-155`) and `archived`
   (`:231-243`). Consumers switch on the variant and **validate the variant's invariants**,
   including negatively: Schema 2 must *reject* festival fields rather than tolerate them
   (`consumer.go`'s `case 2`). A tolerant old arm is how a mis-versioned event becomes silent
   corruption.

5. **Ordering — three distinct cases, only one of which "consumer-first" solves.**

   a. **Forward rollout — landing both sides in one commit is necessary, and it is not
      sufficient.** Where producer and consumer *can* run at different versions, the consumer that
      understands schema N+1 must ship before the producer emits it. **This repo has no mechanism
      to order that today** — `compose.yaml` gives catalog and inventory no dependency on each
      other (both wait only on postgres and nats, `compose.yaml:25-29`), so `up` may recreate them
      concurrently or individually. TKT-53 therefore did the only thing available: **land both
      sides in one change** (`3879c13`).

      Be precise about what that buys. An atomic commit guarantees no *revision* carries one side
      alone — it removes the "someone forgot to ship the consumer" failure entirely. It does
      **not** order the rollout: during a `git pull` + `up`, a new catalog can start and emit
      Schema 3 while the previous inventory container is still running, and that container will
      `Term()` the event (§5b). The window is real, not theoretical.

      **So the atomic commit is the floor, not the guarantee.** Emitting N+1 with no consumer that
      understands it is never acceptable; where the skew window matters, it needs an enforced
      mechanism — replacing the consumer first and gating on its readiness, pausing producer
      emission across the swap, or a `depends_on`-style ordering constraint. This repo has none of
      those, and its exposure is bounded only by being a testbed with no live stream to protect.
      **A production deployment of this system must close the window before relying on §5a.**

   b. **Rollback and consumer recreation — consumer-first does *not* cover this, and the event is
      dropped with no automated recovery.** Once Schema 3 is on the stream, running an inventory
      binary that predates it — by rollback, by rebuilding its durable consumer, or simply by
      restarting it across a window where catalog emitted Schema 3 (see (c)) — makes
      `provisionInput` hit its `default` arm and return `unsupported publication schema N`
      (`consumer.go:91-92`); the caller's `if e.Schema == 1` test (`:114-122`) is what picks `Nak`
      vs `Term`, so an unrecognized schema falls through to **`msg.Term()`** — permanently
      advancing past the event with no provisioning and no retry.

      The failure is **diagnosable but not actionable**. It logs at error level, and the `err`
      attribute carries `unsupported publication schema N`, which is distinct from the malformed
      -payload errors — so an operator reading logs *can* tell version skew from a bad producer
      write. What is missing is everything that would make them look: the headline is the generic
      `"invalid publication event"` shared with corrupt payloads, `c.ready` stays `true` (`:105`)
      so the service reports healthy, nothing alerts, and `Term()` means there is no retry to
      notice. Inventory is simply absent for those performances until someone asks why.

      **Therefore: do not roll a consumer back past a schema the stream still retains**, and treat
      "recreate the durable consumer" as equivalent to a rollback unless the binary understands
      every retained variant. This is a real gap, not a hypothetical — see Consequences.

   c. **Ordinary restart is not replay — but "not replay" is not "no schema risk."** A durable
      consumer resumes from its stored position and does **not** re-read acknowledged history, so
      restarting cannot resurrect an event it already processed. That is the only guarantee it
      gives. Events published **while the consumer was stopped are pending, not history**
      (`AckExplicitPolicy`, `MaxDeliver: -1`, `consumer.go:101`), so if catalog emitted Schema 3
      during inventory's downtime, restarting the *old* binary against the same durable delivers
      them — straight to the `default` arm and `Term()`. No reset and no recreation required; the
      downtime window alone is enough.

      **The general rule, of which (b) is one instance:** a consumer is safe to start only if it
      understands every variant that is pending or still being produced. Rollback and durable
      recreation are the loud versions of that; a plain restart across a deploy boundary is the
      quiet one.

   Keep the rollout note at the code that depends on it (`consumer.go`'s Schema 3 arm) rather than
   relying on this ADR alone.

## Consequences

- **Positive:**
    - The rule catches the failure that parse-compatibility misses: an event that deserializes
      cleanly and drives the wrong write.
    - ADR-014 §3's reasoning is preserved rather than reversed — its bump objection ("inventory would
      have to change in lockstep") is accepted, not dismissed: §5a makes lockstep the *method* while
      the stack has no way to sequence one service ahead of another, and is explicit that this
      removes the forgotten-consumer failure without closing the rollout window.
    - Schema 2's explicit rejection of festival fields becomes a documented pattern, not an accident
      of TKT-53.
    - Scoping the version to `(type, schema)` (§1) keeps the two independent 2→3 forks
      (`published`, `archived`) from being read as one migration, and stops a future consumer from
      inferring anything from another type's number.
- **Negative:**
    - "Changes what a consumer does" needs a judgment call per field. §3a's enumeration (identity,
      routing, idempotency, ownership, field meaning) bounds it, but the boundary is reviewed, not
      compiled — a field that silently changes behavior in a way no one anticipated can still slip
      through. §3b is the more mechanical of the two triggers and should be checked first.
    - Every bump costs an arm in each consumer of that type and keeps old arms alive as long as the
      stream retains them. There is no retirement policy for old schema arms yet; the first removal
      will need one (how far back can the stream be re-read?).
    - **§5's hazards are live, documented, and unmitigated — and the trigger is more ordinary than
      it first looks.** Rollout skew (a), rollback/recreation (b), and a plain restart across a
      deploy boundary (c) all bottom out in the same place: an inventory that meets a schema it
      doesn't know `Term()`s the event — no retry, no readiness change, no alert. Case (c) is the
      uncomfortable one: it needs no operator mistake at all, only that inventory was down while
      catalog emitted. This ADR states the rules; nothing *enforces* any of them. `Nak`/parking
      instead of `Term`ing an unknown schema, gating consumer startup on a max-known-schema check,
      or ordering the swap in Compose would each convert a quiet drop into a signal — **deferred
      to TKT-61.**
    - §5's ordering rests on review, not machinery: no test exercises mixed-version skew, and
      `compose.yaml` expresses no ordering between catalog and inventory to exercise.

## References

- TKT-59 (this ADR) · TKT-53 / PR #37 (the proving case) · TKT-14 (consumes the shared-capacity pool)
- [ADR-009](./ADR-009-contract-first-apis.md) §5 (envelope + `schema`) ·
  [ADR-014](./ADR-014-typed-dated-slot-implementation.md) §3 (additive `kind`, bump rejected) ·
  [ADR-005](./ADR-005-unified-dated-slot-admission.md) (inventory does not fork on `kind`) ·
  [ADR-007](./ADR-007-postgres-nats.md) (JetStream; replay is why §5 binds today)
- Reference impls: `services/catalog/internal/events/events.go` (producer picks the variant) ·
  `services/inventory/internal/consumer/consumer.go` (consumer switches + validates negatively)
