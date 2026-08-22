# ADR-065: Parked recovery orders are a gauge, not an alarm

Date: 2026-08-21

## Status

Accepted (TKT-263)

Extends [ADR-016](./ADR-016-checkout-recovery-state-machine.md) §Consequences, which named
"observability" among the things recovery adds and left it unbuilt. Follows the shape set by
[ADR-062](./ADR-062-refund-reversal-reconciliation.md) §Three gauges and
[ADR-063](./ADR-063-exchange-reversal-reconciliation.md) §6.

## Context

A parked order is one the recovery runner has given up on. Two paths produce one:
`ReleaseStuckOrder` parks after `MaxRecoveryAttempts` (10), and `ParkForReconciliation` parks the
harder case as `reconciliation_required`. Parking is terminal by design —
[ADR-016](./ADR-016-checkout-recovery-state-machine.md) §Decision 1 is explicit that an order which
failed ten re-drives should not keep failing them on a timer — and `ClaimStuckOrders` excludes
parked rows, so nothing in the service revisits one.

That is correct, and it leaves a **discovery** gap rather than a mechanism gap. TKT-146 shipped the
operator's read (`commerce list-parked`) and write (`commerce unpark-order`), but both require an
operator to *think to run them*. The only automatic notice is a single log line, and the code says
so itself at `services/commerce/internal/recovery/runner.go`:

> `// Parked: never claimed again, so this is the last notice anyone gets`

TKT-146 deferred the alarm question with two stated reasons: commerce had no metrics surface, and
the operator read was a prerequisite. **Both are now spent.** The read shipped, and commerce grew a
metrics surface in ADR-062 and ADR-063 — four gauges across two reconcilers, on a `MeterProvider`
live since `obs.Setup`. The question is no longer *whether commerce can* report this, but *what it
should report*.

The population matters to the decision. A parked `reconciliation_required` order may hold **captured
money** ([ADR-016](./ADR-016-checkout-recovery-state-machine.md) §Consequences), and what a re-drive
would do to one depends on provider evidence ([ADR-032](./ADR-032-stripe-behind-the-psp-port.md)).
An attempt-exhausted `release_pending` order generally does not. Those are different urgencies
wearing one word.

## Possible Solutions

- **Option 1 — A second log line at park time.**
    - Pros: no new surface; trivially implemented.
    - Cons: a one-shot line cannot answer either question an operator actually has — *how many are
      parked now*, and *how long has the oldest been waiting*. The line already exists and did not
      close the gap; a second one would not either.
- **Option 2 — A domain event on parking.**
    - Pros: durable, consumable, fits the existing JetStream fabric.
    - Cons: a payload and schema decision under
      [ADR-017](./ADR-017-domain-event-schema-evolution.md) for a fact with **no consumer**. Nothing
      would subscribe. It also states an edge ("this order just parked") where the operator's
      question is about a level ("how much is parked"), so recovering the level would mean a
      consumer maintaining its own counter.
- **Option 3 — Observable gauges over the parked population *(chosen)*.**
    - Pros: answers both questions directly; matches two precedents in the same service; costs one
      read-only aggregate query and a registration.
    - Cons: pull-shaped — nothing fires. It makes the population *visible*, not *noticed*.
- **Option 4 — Gauges plus an alert rule and a threshold.**
    - Pros: something actually fires.
    - Cons: the repo hosts no alerting surface at all — no Alertmanager, no rule files, no
      dashboards. Inventing one inside this ticket repeats exactly the mistake TKT-146 avoided. And
      the threshold cannot be chosen honestly yet: see §Decision 4.

## Decision

**Option 3.** Four `Int64ObservableGauge`s registered on the recovery runner, backed by one
aggregate read, following ADR-062/ADR-063's shape including their rule — *observability, not a
gate*: a failed registration is logged and commerce keeps taking traffic.

### 1. Only PARKED rows are counted, and the predicate is the decision

`ReadRecoveryBacklog` filters on `recovery_parked_at IS NOT NULL`. This is not a detail.
`QueueForCompensation` moves an order to `reconciliation_required` while deliberately leaving it
**unparked**, and migration `0005_psp_recovery.sql` states the distinction directly:

> an UNPARKED `reconciliation_required` row is a queued compensation, not a human's inbox.

A read that counted by status alone would report the runner's own in-flight work as an operator
backlog — the metric would rise precisely when the system was working. Pinned by a smoke test that
seeds an unparked `reconciliation_required` control and asserts the split delta is one while two
rows of that status exist.

### 2. Parked `reconciliation_required` is its own series

The split is over **kind**, not degree, which is why it is a separate series rather than a separate
threshold on one number. These rows may hold captured money; the others generally do not. An
operator reading a single count could not tell which problem they have. This is the same move
ADR-063 §6 made with `awaiting_switch`.

The gauge descriptions say *may* hold captured money, never *does*: whether any given row does is a
provider-evidence question ADR-032 owns, and a metric description is the wrong place to assert it.

### 3. Count, split, total, and age — four, not two

- `commerce.recovery.parked` — parked, excluding `reconciliation_required`.
- `commerce.recovery.parked.reconciliation_required` — the split above.
- `commerce.recovery.parked.total` — every parked row. Carried so the two splits can be reconciled
  against it; a total that disagrees with its own parts is a defect neither part alone can show.
- `commerce.recovery.parked.oldest_age_seconds` — measured from `recovery_parked_at`.

The age gauge exists for ADR-062's reason, which applies unchanged: a count cannot distinguish a
small, old backlog from a large, fresh one, and those are different incidents — the first is
something stuck, the second something down. Measured from the park, not from `created_at`: the
question is how long the order has been waiting for a human.

All four are observed from **one** `RegisterCallback` reading **one** snapshot. Separate callbacks
would read separately and could publish a total contradicting its own splits.

### 4. No threshold, no alert rule — deliberately, and this is the answer to the ticket's question

TKT-263 asked whether parked orders should *alarm*. The answer is that they should be **observable**,
and that the threshold is not this repo's to choose yet.

The ticket contains its own best argument: *"an alert on `> 0` will be muted within a week."* One
parked order is ordinary operational residue. A growing count is an incident. A single order parked
six hours and untouched may be a worse incident than twenty parked in the last minute. Which of
those is worth waking someone for depends on production volumes this system has never seen, and on
an on-call rota that does not exist. Encoding a number now would encode a guess, and a muted alert
is worse than no alert because it looks like coverage.

What this ADR does instead is make every one of those thresholds **expressible**: count, split by
urgency, and age are exactly the three inputs any such rule needs. The rule belongs with whoever
operates the deployment, alongside the Alertmanager this repo does not have.

### 5. No index, following TKT-146

The read scans `orders` on an unindexed predicate. TKT-146's migration `0023` deliberately added no
index for `list-parked`, reasoning that the parked population is bounded by attempt exhaustion — a
scan long enough to matter would itself be the finding. A gauge over the same bounded population
inherits that reasoning. Adding one would mean a migration, and revisiting it is a separate decision
with real evidence attached.

## Consequences

- **Positive:**
    - The parked population stops depending on someone thinking to look. ADR-016's "the last notice
      anyone gets" is no longer the only notice.
    - The two urgencies are countable apart, so an operator can tell a captured-money backlog from a
      retry-exhausted one before opening a terminal.
    - Any future alert rule has its inputs already exported; adopting one is a deployment change,
      not a code change.
    - Commerce's third runner now matches the other two, so the reconciler-observability shape is a
      pattern rather than a pair of instances.
- **Negative:**
    - Nothing fires. This makes the population visible, not noticed — an operator or a dashboard
      still has to look, and until an alerting surface exists that remains true.
    - The read is an unindexed aggregate over `orders` on every collection interval. Bounded today
      by §5's reasoning; if that bound ever fails, this query is one of the places it will show.
    - The wiring is proven by a smoke assertion, which means it depends on the full stack. The
      registration call in `main.go` has no cheaper guard.
- **Neutral:**
    - Parking, unparking and the runner's decision table are untouched. This ADR adds a read and
      changes no behaviour.
    - `list-parked` remains the order-level tool; the gauges answer *how many and how old*, never
      *which*.

## References

- [ADR-016](./ADR-016-checkout-recovery-state-machine.md) — parking is terminal; the
  `reconciliation_required` population and its captured money.
- [ADR-017](./ADR-017-domain-event-schema-evolution.md) — what an event would have cost (Option 2).
- [ADR-032](./ADR-032-stripe-behind-the-psp-port.md) — provider evidence decides what a re-drive does.
- [ADR-062](./ADR-062-refund-reversal-reconciliation.md) §Three gauges — the shape, and the age
  gauge's rationale.
- [ADR-063](./ADR-063-exchange-reversal-reconciliation.md) §6 — the precedent for splitting a
  categorically different sub-population into its own series.
- `docs/development.md` §Parked recovery orders — the operator surface.
