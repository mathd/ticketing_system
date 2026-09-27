# ADR-075: Proposed waitlist capacity capture and ADR-023 amendment

Date: 2026-09-27

## Status

Proposed. No policy in this ADR is active. ADR-023 remains accepted and unchanged.

## Context

Inventory owns admission under the per-pool PostgreSQL row lock (ADR-010). The public GA `CreateHold` path admits against confirmed quantity, live claims, the target capacity, and capacity reserved by active channel allocations. A release can commit before Commerce observes it, so a public request can take newly available capacity first. TKT-348's probe forced that ordering six times. It did not test waitlist capture.

Buyer and group expiry are lazy. `liveClaims` stops counting a held row after its database-time expiry, while `sweepExpired` writes the terminal state and history only when a later locked path runs. Scheduled channel allocation release is also lazy: `activeAllocation` changes at its database-time cutoff, but the cutoff writes no event or capture. A reconciliation pass that only reads existing captures cannot create work for a quiet pool.

Commerce owns waitlist order and buyer policy. Inventory cannot publish `platform.*` domain events under ADR-072. Inventory also cannot infer Commerce's queue state from its own claims. A design that protects capacity only after a Commerce message arrives has a race unless the system defines exactly when local Inventory state becomes authoritative.

## Conditions of success

The design must support the parent TKT-38 conditions:

1. "Freed inventory triggers a time-boxed offer to the head of the waitlist; expiry cascades to the next"
2. "Waitlist honors per-event purchase limits and account requirements"
3. "Waitlist-to-purchase conversion is traceable end-to-end (ADR-003)"

## Options

### Public competition after release

Commerce discovers a release and races an ordinary public hold for the unit.

- Advantage: no Inventory waitlist state or priority rule is needed.
- Cost: a public request can win first, so this cannot promise the head of the queue an offer.

### Commerce calls Inventory while the release transaction holds the pool lock

- Advantage: Commerce can select an eligible buyer before Inventory admits another request.
- Cost: a network call under the pool lock adds cross-service latency and failure to every relevant Inventory mutation. It also makes capacity progress depend on Commerce availability.

### Inventory captures capacity locally and Commerce discovers it after commit (proposed)

Inventory records a durable capture under the pool lock. Commerce supplies acknowledged per-pool activation and demand state, owns queue policy and offer delivery, and discovers captures through a durable command/read path.

- Advantage: Inventory serializes capture with public admission; Commerce remains the queue authority; no network call runs in the pool transaction.
- Cost: Inventory gains local activation state, capture state, bounded reconciliation, and retry rules. A stale active generation can reserve capacity while Commerce is unavailable.

### Commerce polls availability

Commerce polls current public availability and offers to the queue head.

- Advantage: no Inventory domain event is needed.
- Cost: availability is advisory and does not reserve a unit. A public hold can take it between the read and claim, so polling alone cannot provide priority.

## Proposed decision

Keep waitlist membership, ordering, account rules, eligibility, and delivery in Commerce. Keep capacity admission and capture in Inventory. This is a proposed contract, not implemented behavior.

### Local activation and admission boundary

Commerce writes each queue change and sends Inventory a versioned, per-pool generation with a conservative snapshot indicating whether eligible demand exists. Inventory persists and acknowledges that generation under the pool lock. An enrollment gains priority only after Inventory acknowledges the generation. A Commerce-only enrollment write does not activate priority, and no timely message is assumed.

Public admission consults only the last acknowledged local generation. If it says eligible demand exists, Inventory protects eligible public headroom with pending captures before it admits a new public hold. If Commerce is unavailable, that acknowledged state remains in force: public admission excludes captured quantity, and Inventory continues to capture newly eligible headroom. This favors safety over public-sale liveness. Stale active state can over-reserve capacity until Commerce recovers and Inventory acknowledges a newer generation. If Inventory cannot read or safely maintain its local activation/capture state, public admission fails closed. An acknowledged inactive generation allows ordinary public admission after its associated pending captures have been safely resolved according to the selected fallback.

This boundary deliberately means an enrollment whose Inventory acknowledgment has not completed has no priority guarantee. The client or Commerce workflow must not represent it as active before acknowledgment.

### Capture command and quiet-pool progress

Inventory exposes a bounded reconciliation command that takes the pool lock and performs maintenance plus capture materialization. Transactions that create public headroom, including refunds, operational releases, capacity increases, and explicit claim releases, durably mark the pool for reconciliation in the same transaction. A durable due-pool scanner independently finds elapsed buyer/group expiries and scheduled channel give-backs from their database predicates, including pools with no later write. The command settles expiry/history where applicable and creates captures from eligible public headroom. It must materialize captures; reading rows that already exist is insufficient.

Commerce runs durable due-pool discovery and retries independently of future sales or staff writes. Discovery state and retry survive process restarts. The worker invokes the Inventory command and then reads the resulting durable capture state. Inventory publishes no domain event. Repeated command calls are idempotent and converge on the same source/generation capture identity.

Bound candidate rows and work under each hot pool lock. Persist continuation state so another pass can resume without rescanning or skipping due sources. A pass must not loop without a fixed work limit. A fresh public admission may proceed only when reconciliation has established that no due source remains unprocessed at its decision point. If the work limit is reached first, commit safe maintenance already completed, refuse the fresh admission with a retryable maintenance-incomplete result, and let the next bounded pass continue. This result applies only to fresh admission. Replay lookup and its existing result happen before general bounded maintenance because a replay admits no new demand. Maintenance or capture errors that prevent a safe headroom decision fail closed for fresh admission: do not admit the request or report a business capacity refusal as if maintenance succeeded.

### Preserving maintenance on a refused public hold

The public admission flow needs a transaction boundary that commits maintenance even when new request-specific work is refused. Replays keep the current early-return behavior because they admit no new demand. In the current `CreateHold`, replay lookup runs before `sweepExpired`. A matching fingerprint on a live or terminal row returns its existing result without requiring general reconciliation to finish. A fingerprint mismatch returns `ErrIdempotency` without general reconciliation. A matching fingerprint on a held row whose expiry is at or before the captured transaction-start `snapshotTime` writes expiry and history, commits availability, and returns `ErrConflict`. The held-and-elapsed path must also write a durable due-pool reconciliation marker in that same commit so the worker can capture any newly eligible headroom later. This targeted expiry does not require draining all due work. These replay results remain distinct, and maintenance-incomplete must never replace them. Database or internal errors, including failure to commit the targeted expiry and marker, remain errors.

Proposed shape:

1. Begin a transaction and lock the pool. Preserve the current replay lookup and early returns before general maintenance. A live or terminal matching replay returns its existing claim; a fingerprint mismatch returns `ErrIdempotency`.
2. For a matching held-and-elapsed replay, use the existing targeted expiry and history write, add the durable due-pool marker to that commit, and return `ErrConflict`. Do not run general bounded maintenance on this path. If the expiry, history, marker, or commit fails, return the database or internal error.
3. Only when the idempotency key is absent, run bounded expiry/give-back maintenance and materialize captures. If the work limit leaves due work unprocessed, commit safe maintenance already completed and return retryable maintenance-incomplete. This outcome applies only to this fresh admission.
4. After maintenance completes, create a savepoint for request-specific work. Run the offering guard, channel and seller checks, presale redemption, capacity checks, and hold creation inside it.
5. On a business refusal, roll back to the savepoint, commit maintenance and captures, then return the refusal. On a successful new hold, commit and return the new result.

This requires changing the fresh-admission path in `CreateHold` while retaining its existing replay branches. Keep request-specific idempotency writes and presale redemption out of the fresh-request savepoint boundary until after general maintenance. A refusal can then roll back request-specific work without undoing maintenance. Any database or internal error that prevents safe maintenance or its commit prevents fresh admission. A refused new request must not consume a presale redemption.

### Public headroom and source eligibility

The capacity contract counts pending captures and issued offers that have not become buyer claims as existing demand. A pending capture is a durable row with a quantity and source/generation identity. Issuing an offer keeps that quantity protected. When transfer creates a buyer claim, Inventory changes the capture/offer state and creates the claim under the same pool lock; the transfer is quantity-neutral. If an offered claim already appears in `liveClaims`, its quantity is counted there and excluded from the separate protected-capture sum. No cut cancels an issued offer or drops its reserved quantity.

For new captures, available public headroom is:

`max(0, new-demand limit - confirmed demand - live held/finalizing demand - unused reservations on active channel allocations - pending capture demand - offered demand not already represented in liveClaims)`

`new-demand limit` is `target_capacity` while a cut is draining, otherwise physical capacity. On a capacity cut to target `T`, preserve the demand counted by the capacity floor and clamp physical capacity to `max(T, confirmed demand + live held/finalizing demand + protected capture demand)`. Protected capture demand includes pending captures and offered quantity not already represented in `liveClaims`. Unused active channel reservations do not enter this floor because they are reservations, not held or sold units. Keep `target_capacity = T` while the floor exceeds `T`. The target blocks new demand: new public claims and captures must fit within `T`, even while physical capacity remains above it to honor counted demand. An offer-to-claim transfer for an already-reserved quantity remains allowed during draining when identity and eligibility checks pass, because it changes no demand. The offer must not be silently canceled by the cut.

Active unused channel reservations continue to reduce headroom for new general public admissions and captures. After a cut, their caps can exceed physical free capacity. The cut does not promise to preserve their full usable channel capacity, and this proposal adds no automatic cap reduction. Channel-specific new demand must still fit the global target while draining, or physical capacity otherwise, as well as its channel cap. The global limit prevents oversell even when an allocation's unused cap is larger than remaining physical capacity.

This contract must be added to every relevant capacity reader and mutator. At minimum, extend `AdjustCapacity`, `effectiveCapacity`, `reconcileCapacity`, `liveClaims`-based demand calculations, and the admission and availability paths for GA holds, seat holds, and best-available seats. Extend release and expiry paths that reconcile demand, including `Transition`, `ReleaseOperational`, refund return, group placement/draw-down, and scheduled or explicit channel-allocation release/replacement. Any other path that reads or changes pool capacity, confirmed quantity, live claims, allocation reservations, or availability must use the same accounting. Current code does not count captures or offers in these paths. This is unimplemented proposal work, and no current invariant or helper magically covers it. Seated capture remains outside this proposal's proven correctness; the owner must decide whether the same accounting applies there before implementation.

The physical ceiling is the safety bound for existing demand. The target is the admission bound for new demand. A channel-specific claim expiring while its allocation remains active does not create general public headroom. Its unused channel reservation remains excluded from the public calculation. Whether a source is eligible for a channel-specific waitlist, how channel-specific policy works, and which other source types qualify remain open owner decisions. Capture cannot bypass channel caps, window, seller, or unlock-code rules.

### Identity, retry, cascade, and audit links

Use durable non-PII links for organizer/pool/entry/generation → capture → offer → claim → reservation → order. Record applicable links in Inventory history and Commerce audit trails. Keep them across retries, cascade, and worker recovery. They identify lifecycle operations and do not require buyer PII.

Capture and offer operations use stable server-issued identities scoped to their source and generation. A retry reads or advances that identity; it never creates a second capture for the same source/generation. When an offer or capture lease expires, the same capture returns to pending state within the same priority generation before Commerce offers it to the next eligible entry. The generation and capture remain fenced during cascade. Capacity returns to public admission only through the current generation's explicit terminal/fallback transition under the pool lock. Once a capture or offer identity is terminal or stale, a retry with it must refuse or return its recorded result; it must not allocate fresh capacity. Exact FIFO rules, offer TTL, partial fill, skip, decline, and no-eligible-buyer fallback remain open.

### Proposed inactive amendment to ADR-023

If the owner accepts waitlist-first capture, amend ADR-023's converted-child expiry rule as follows:

> When a converted buyer child expires, Inventory marks it expired and makes its quantity eligible for the acknowledged waitlist-capture policy under the pool lock. It does not restore quantity to the source operational hold. If no active acknowledged waitlist demand protects the quantity, it follows the owner-approved public fallback. Replaying the conversion after child expiry remains a conflict and creates no new child or source restoration. A new operational conversion requires a new staff operation and key.

This amendment is inactive. Until an owner accepts it, ADR-023 remains in force, including its statement that expired converted-child capacity returns to the public pool. In this proposal, every public-give-back statement for converted-child expiry is conditional on the acknowledged activation state and the owner-approved fallback. This proposal does not change the accepted ADR-023 rule by implication.

## Implications for existing ADRs

This proposal qualifies, but does not activate changes to, ADR-024 and ADR-027. ADR-024 currently makes unsold allocation remainder publicly claimable when `activeAllocation` becomes false. Under this proposal, that remainder becomes eligible for general waitlist capture at the same locked reconciliation boundary before public admission; the active allocation reservation must still be subtracted until its cutoff. ADR-027 currently says an expired group-reservation child returns capacity to the pool. Under this proposal, it becomes eligible for capture under acknowledged activation state, without restoring the source reservation. ADR-027's quantity-neutral draw-down and inherited `channel_code` remain unchanged. Owners must decide whether and how channel-specific waitlists affect these give-backs before either ADR changes.

`ReturnRefundedCapacity` currently discovers the pool before `BeginTx`, then locks pool → claim and commits the return. Any implementation that adds capture to refunds must preserve that ordering. Operational release, capacity adjustment, group placement/draw-down, and other release paths also need source-specific capture integration at their existing pool-lock transaction boundary. This proposal does not establish seated capture correctness. Partial seated refunds remain refused because Inventory has no ticket-to-seat subset mapping; any seated waitlist policy needs a separate design.

## Safety and liveness

Safety means public admission cannot consume capacity reserved by an acknowledged active generation. Confirmed demand, live demand, and protected capture demand stay within physical capacity; unused active channel reservations remain excluded from new general public and capture headroom. They are not counted as held or sold demand in the cut floor, so a cut can leave their caps above physical free capacity. New demand stays within the target while a cut drains, and channel-specific admission remains subject to that global limit and the channel cap. The pool lock, idempotent source/generation identity, and shared accounting contract define this proposal. They are not implemented by current code.

Liveness means eligible waiters eventually receive an offer and unused capacity eventually returns to public sale under an owner-approved fallback. Durable due-pool discovery and retry support progress when pools are quiet, but Commerce unavailability or stale acknowledged demand can delay offers and public sale. The design accepts this over-reservation tradeoff to preserve acknowledged priority. The exact bounds for retries and offer lifetime remain open.

## Consequences if accepted

- Public admission and capture share the Inventory pool lock.
- Commerce owns queue policy and must obtain Inventory acknowledgment before treating enrollment as active.
- Inventory runs bounded reconciliation that creates due captures, not just reads them.
- Public admission must commit safe maintenance on a business refusal while rolling back request-specific work, including presale redemption.
- Inventory stores local activation generations, pending captures, and durable retry state.
- End-to-end lifecycle links are durable in Inventory history and Commerce audit trails without PII.
- Stale active state favors priority safety and can over-reserve capacity while Commerce is unavailable.
- Source eligibility, channel-specific policy, FIFO details, offer TTL, partial-fill policy, fallback, purchase limits, account rules, and seated participation remain owner decisions.

## Related ADRs and evidence

- [ADR-003: Append-only audit trail](ADR-003-append-only-audit-trail.md)
- [ADR-010: PostgreSQL claim transaction](ADR-010-postgres-claim-transaction.md)
- [ADR-023: Operational holds and conversion](ADR-023-operational-holds-and-conversion.md)
- [ADR-024: Channel allocations](ADR-024-channel-allocations.md)
- [ADR-027: Group agency reservations](ADR-027-group-agency-reservations.md)
- [ADR-072: NATS publisher access control](ADR-072-nats-publisher-acls.md)
- [TKT-348 investigation and probe](../evidence/TKT-348/README.md)
