# ADR-063: Outstanding exchange obligations are swept by a second leased runner, and the sweep never invents the switch

Date: 2026-08-18

## Status

Accepted (TKT-259; decision taken under `config.gates: autonomous`, so gates 2–4 were the agent's —
the plan-review critique, the self-made decisions and any overridden objections are on the ticket).

Closes the gap [ADR-062](./ADR-062-refund-reversal-reconciliation.md) named in its own
*"What this does not cover, deliberately"*. Inherits [ADR-038](./ADR-038-refund-reversal-ticket-voiding.md) §1's
ordering rule, [ADR-039](./ADR-039-exchange-settles-the-difference.md)'s exchange lifecycle,
[ADR-010](./ADR-010-postgres-claim-transaction.md)'s lock discipline and
[ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md)'s adversary language.

## Context

ADR-062 built a leased runner for the REFUND side of this problem and recorded exchanges as a
deliberate omission:

> **Exchanges.** `order_exchanges` carries obligations of the same shape and **no commerce-side
> sweep exists at all** — their only retry is access's JetStream redelivery, driven by commerce
> answering 502. If that consumer dead-letters, nothing revisits those rows.

That is the whole context. `order_exchanges` has carried `tickets_exchanged_at` since migration 0010
and `capacity_returned_at` since 0011, and no query anywhere selected exchanges by outstanding
obligation. The 502-and-redelivery mechanism works while access's consumer is healthy; when it
dead-letters, or when `order.exchanged` is never consumed at all, the source line's capacity stays
out of sale with nothing driving it and nobody counting it.

## Decision

### 1. A second runner (`internal/exchangesweep`), not an extension of `internal/reversal`

The shape is copied; the state machine is not shared. Three independent reasons, in ascending order
of how hard they are to argue with:

1. **Types.** `reversal.Store` and `reversal.Reverser` are typed on `store.Refund` and
   `refunds.Service.DriveReversal`. There is no `Refund` to hand them for an exchange.
2. **Precedent.** ADR-062 §1 took this exact decision for this exact reason when it declined to
   extend `internal/recovery` or `internal/bulkrefund`: *"different eligibility, different terminal
   states, and one state machine serving all three would be readable by nobody."* Extending
   `reversal` now would contradict the ADR whose shape is being copied.
3. **Eligibility genuinely differs** — see §3, which is the interesting part.

What *is* reused, deliberately and verbatim in shape: claim under a lease with a fencing claim token,
`FOR UPDATE SKIP LOCKED`, attempt charged on release rather than on claim, exponential backoff capped
at five minutes, progress observed against the row rather than predicted, parking at a bounded budget,
one drive per row per pass, a bounded drain, and gauges. Reusing the shape without abstracting it into
a generic engine is the point: an abstraction over two lifecycles would have to be read by anyone
touching either.

### 2. The sweep NEVER writes `tickets_exchanged_at` — and that is a safety property, not a scope note

This is the load-bearing decision, and it is where a sweep copied carelessly from the refund side
would be wrong.

An exchange has two obligations, and **they do not belong to the same service**. The capacity return
is commerce's to drive: it is an inventory call commerce makes. The switch marker is *access's fact* —
it records that the old tickets stopped admitting, which only access can establish. Migration 0011's
CHECK gates the capacity return on that marker precisely because freeing the seat while the ticket
still admits is the one ordering that can **oversell** (ADR-038 §1).

So a sweep that set the marker itself would be asserting a fact about another service's state in
order to unlock the one write that oversells. It would turn a visible, safe under-sell into a silent
double-sale.

The guarantee is **structural, not documentary**: the `Discharger` port the runner drives exposes one
method, `DriveExchange`, and that unit's first line refuses an unswitched exchange. There is no method
on the runner's ports that could write the marker. A test (`TestTheSweepNeverInventsTheSwitchMarker`)
pins it end to end against real PostgreSQL, and the runner's unit test pins the decision.

**Consequently the sweep closes one of the two obligations, and COS 1 is met in two different
branches** — worth stating plainly, because the alternative is an ADR that claims more than it
delivers:

| Row state | What happens | COS 1 branch |
|---|---|---|
| settled, switched, capacity outstanding | **driven to completion** by the sweep | "driven to completion without a human" |
| settled, switch never confirmed | **claimed, counted, released — never completed** | "explicitly monitored and documented" |

The second row is not a lesser outcome papered over: it is the only *correct* outcome. The incident
it represents (an access consumer that stopped delivering) has a different owner and a different fix
from the first (inventory refusing), which is why it gets its own gauge rather than being folded into
`outstanding`.

**An awaiting-switch row is not claimed at all** — and it took three review passes to land there,
which is worth recording because each intermediate position looked correct.

The first implementation claimed such rows and *charged them an attempt*, exactly as a failed capacity
return is charged. That parks the row after ten passes — and because the claim predicate excludes
parked rows, when access finally confirms the switch and its own capacity return fails, the sweep can
never reclaim it. The capacity would have been stranded permanently *by the mechanism added to prevent
that* (ai-review F1).

The second stopped charging them but still claimed them. Two defects survived. The release still wrote
`reversal_last_error`, and 0022's rollback guard refuses on any non-NULL error — so the routine,
transient state of waiting for access would have made the migration unrollbackable, for work where
nothing was even attempted (F3). And the claim is `ORDER BY reversal_next_attempt_at` with a `LIMIT`,
inside a pass bounded at `MaxBatchesPerPass` batches, so a large awaiting-switch backlog after an
access outage fills every batch with rows commerce can do nothing about and pushes genuinely actionable
capacity returns past the bound — head-of-line blocking that delays exactly what the sweep exists to do
(F4).

So the actionable set is now `settled AND switched AND capacity outstanding AND unparked`, in the claim
and in the partial index alike. Excluding awaiting-switch rows costs nothing, because there was never
anything to do with them: `DriveExchange` refuses an unswitched row anyway. **The sweep is not how they
are observed — the `awaiting_switch` gauge is**, and it reads them directly. When access confirms, the
row becomes claimable immediately, with its budget untouched and no backoff to wait out.

The release still carries the awaiting-switch arms as a second line of defence, because the state
remains *reachable*: the marker is read from the row at release time and a concurrent writer can clear
it between claim and release. The rule in one sentence — **a budget and an error describe what a row
asked a downstream and how that went; a row that asked nobody has neither.**

### 3. Parking is copied; ADR-062's REASON for it is not

ADR-062 §2's parking rationale is inventory's refusal of a **partial return of a seated claim**
(`ErrPartialSeatedReturn`, TKT-164), and its argument is that commerce cannot predict that refusal
because seatedness and "partial" are both inventory-side state.

**That case is essentially unreachable for exchanges.** ADR-039's Consequences records that exchanges
are **whole-line only**, and TKT-158 refuses a **seated source** outright. So the source claim is GA
by construction and the return is FULL — which is the one case ADR-038 §9 says seated claims accept
anyway.

Parking is still copied, on ADR-062 §2's *general* argument, which stands on its own: a permanently
undischargeable shape nobody has enumerated is recognised by **making no progress**, never by a
predicate that would be wrong in both directions and would drift from inventory's rule with every
change to it. A bounded budget spent by observation covers refusals nobody has thought of.

Saying which half transfers matters more than it looks. Restating the seated rationale here would
have produced an ADR that is fluent, plausible, and wrong about its own system — and it is exactly
what copying the neighbouring document would have produced.

### 4. Attempts reset on progress — unchanged from ADR-062 §3, and still what makes a bound safe

A pass that discharges an obligation it had not discharged before resets `reversal_attempts` to zero.
Without it, a long inventory outage would retire an exchange that was about to recover, in the name of
fixing exactly that failure. Progress is measured **in SQL, against the row as it stands**, from the
obligations observed at claim time — because the tickets-switched callback drives the same obligation
whenever access redelivers, and a verdict computed from this claimant's own before/after would call a
concurrent discharge "no progress" and park a recovering row (ADR-062's ai-review F2, inherited).

### 5. The 502-and-redelivery path stays as the first line; the sweep is the backstop

Not replaced. The callback discharges in milliseconds on the happy path against a sweep interval of a
minute, and removing it would make every exchange wait on a poll for no benefit. The sweep exists for
the rows redelivery gave up on. Both drive the **same** discharge unit (`internal/exchanges`), lifted
out of the HTTP handler by this ticket for that purpose — before it, the logic took a `*http.Request`
and no background runner could call it at all. One discharge path, two callers, mirroring ADR-062's
"one reversal path, three callers".

### 6. Four gauges, and the age is measured from SETTLEMENT

`commerce.exchange.reversal.{outstanding,parked,awaiting_switch,oldest_age_seconds}`.

`awaiting_switch` is this sweep's addition and is what earns COS 1's second branch (§2).

The age is measured from `settled_at`, **not** `created_at` as the refund side measures it. An
exchange row is created at bind time, before settlement, so a long-lived unsettled bind would inflate
the gauge while owing nothing. ADR-062 §4's justification for having an age gauge at all — a small old
backlog and a large fresh one are different incidents — only holds if the age means what the operator
reading it thinks it means.

As on the refund side: observability, not a gate. A failure to register a gauge is logged and commerce
still exchanges.

### 7. No readiness change, and the tuning variables are plain defaults

ADR-062 §5 made a missing `ACCESS_URL` fail readiness because that variable was *optional* and the
reconciler made its absence newly dangerous. Nothing analogous applies here: `INVENTORY_URL` is
already fail-fast at startup (commerce refuses to boot without it), so there is no new configuration
under which this sweep silently cannot work. `/healthz` and `/readyz` are untouched.

`EXCHANGE_REVERSAL_INTERVAL` (default `1m`) and `EXCHANGE_REVERSAL_BATCH` (default `16`) are read with
`os.Getenv` and given no `${VAR:?}` marker in compose. A mandatory marker with no matching emitter in
`scripts/env-bootstrap.sh` fails `make check`'s `check-required-env` stage (TKT-227), and these are
tuning knobs, not credentials.

## Consequences

### What this does not cover, deliberately

- **Unparking.** TKT-146 owns the operator path for parked recovery orders and ADR-062 deferred the
  refund side to it for the same reason. A third bespoke unpark surface would guarantee three.
- **Repairing a never-confirmed switch.** Out of scope by §2, permanently, and not for lack of effort:
  it is not commerce's fact to assert. The remedy for a stuck `awaiting_switch` count is in access —
  its consumer, its dead-letter queue — and the gauge is what points an operator there.
- **The exchange money leg.** `settled_at` is durable and TKT-167's resume already recovers an
  exchange interrupted after the money moved (ADR-039 §3c). This sweep never touches money and its
  port cannot express a settlement.
- **`LoadExchangeSwitch`'s unscoped reservation join.** Found while shaping this ticket and filed
  separately rather than widened into it. The new claim query is organizer-scoped on both joins and
  does not inherit the shape.

### Integrity language — name the adversary (ADR-021)

Everything above is **honest-writer consistency**: it holds against concurrency, crashes, restarts and
two replicas. It is **not tamper-evidence**. Anyone with commerce database write access can clear a
lease, unpark a row, or forge a discharge timestamp — including `tickets_exchanged_at`, which makes
§2's guarantee one about *this code*, not about *that database*. The signed, append-only payments
journal remains the evidence that money moved; ADR-021's limits on it are unchanged.

### Operational

Migration `0022` adds the reconciliation columns to `order_exchanges` and a partial queue index; its
Down fails closed once any row has parked, been retried or recorded an error, exactly as 0021's does,
because rolling back would unpark permanently refused obligations with a fresh budget and erase why
they parked. Migrations run out-of-band via commerce's `migrate` subcommand (ADR-022).

Operator surface and the meaning of each state: `docs/development.md` § Exchange obligation sweep.
