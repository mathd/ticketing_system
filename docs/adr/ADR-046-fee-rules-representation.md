# ADR-046: Fee rules are typed declarative rows, resolved per fee code

Date: 2026-08-05

## Status

Accepted (TKT-214; decision taken under the owner-waived gates of that run, recorded on the
ticket). First deliverable of the TKT-6 epic. Consumed by TKT-215 (the sale path), TKT-216
(payees and splits), TKT-217 (the settlement ledger).

**§4 and §8 extended to price rules, and §7 amended, by TKT-237 (2026-08-09)** — the channel
eligibility and precedence rules this ADR wrote for fees now govern `price_rules` as well, with one
deliberate divergence: a foreign channel's rule is hidden from **fee** provenance to avoid
publishing the channel fee matrix to other services, and hidden from **price** provenance for a
strictly larger reason — `price-resolution` is a PUBLIC operation where fee-resolution is
`/internal/` (§6). **→ That premise no longer holds: TKT-155 moved price resolution onto the
internal surface. The hiding behaviour is unchanged; see § Amendment (TKT-155) for why.** §7's revisit trigger is unchanged; see the amendment there for why, now that the
two comparators resemble each other more closely than when §7 was written. Decision taken under the
owner-waived gates of that run, recorded on the ticket.

*Extends [ADR-036](./ADR-036-pricing-rules-representation.md); amends nothing in it.*
*Respects [ADR-002](./ADR-002-services-from-day-one.md)'s ownership row without amending it — see
§ 5.*

## Context

TKT-6 needs service fees: schedules per channel, absorbed vs passed-on, rounding rules, and split
accounting across payees. This ADR fixes the **representation and resolution** of fee rules. The
arithmetic that turns a rule into money is TKT-215's, the payees are TKT-216's, and the ledger is
TKT-217's.

The decision is needed now, before any of those, because it fixes the shape of production fee data.
Changing it later is a migration over rows that charged real buyers, not an edit.

ADR-036 already decided the hard half. It chose typed declarative rows over a DSL for pricing,
fixed the five scope levels and how each is *derived* from a ticket type (§1), fixed the comparator
(§4), and fixed the storage and index shape (§3). It also stated the constraint any relative action
must satisfy, and said explicitly why fees were not in scope there:

> Deltas overlap the fee epic (TKT-6) and percentage reductions overlap promotions — both have
> their own epics, and importing their semantics here would be this ticket deciding theirs.

and

> **When a relative action is added**, it must be integer-safe: basis points, checked
> multiplication, and a documented rounding direction. Floats stay banned (ADR-001).

So the open questions are narrower than TKT-5's were: not *"rules or a DSL"*, but *"what shape do
fee rules take inside the model ADR-036 already chose, and where does the money get computed"*.

Current state confirmed by reading the code: there is no fee schema, resolver, payee or settlement
entry anywhere in the repo. `price_rules` and its resolver exist and are the thing being rhymed
with.

**The structural fact that drives everything below: fees are additive, prices are not.** A price
resolution has exactly one winner — that is what "the price" means. A sale can carry a service fee
*and* a facility fee *and* a delivery fee at the same time. Any model that produces one winner per
resolution is wrong for fees, and any model that lets rules stack freely has to define a stacking
order, which is the thing ADR-036 avoided buying.

## Possible Solutions

- **Option 1 — Widen `price_rules`.** Add `fee_code`, `basis`, `rate_bps`, `incidence`,
  `channel_code`; make `amount` nullable; teach `SelectPricingRule` to return a set.
    - Pros: one table, one comparator, one index, one write gate. A future third rule kind lands in
      the same place. No duplicated ranking logic.
    - Cons: the price comparator would carry cases it can never have — a nullable amount, a second
      value column, a code partition, a channel step — and every one of those is a branch on the
      path that decides what a buyer is charged. It would also force a migration on the table that
      prices real sales in order to ship a feature that changes no price. And "one winner" would
      stop being true of the shared type, so every existing caller's assumption becomes conditional.
- **Option 2 — A separate `fee_rules` table, resolved per fee code (chosen).** Same five scope
  levels, same derivation, same `(scope_level, scope_id)` index shape, same window semantics, same
  write gate — a *separate* table and a *separate* resolver.
    - Pros: the price path is untouched, so this ticket adds no pricing regression surface. Each
      resolver's type says what it means (one winner vs a set). The per-code partition is what lets
      ADR-036's comparator be reused verbatim inside it, with no stacking order to define.
    - Cons: the ranking logic is duplicated (§ 7 makes that deliberate and bounds it), and a future
      third rule kind would make the duplication a real problem rather than a small one.
- **Option 3 — Fee configuration in `commerce`, beside the evaluation that consumes it.** ADR-002
  gives commerce "pricing/fee/promo evaluation", so put the rules there too.
    - Rejected. It splits the hierarchy: the scope levels are catalog's entities (venue, event,
      series, slot, ticket type), so commerce would either duplicate the derivation or ask catalog
      for it on every sale. ADR-036 §6 moved *resolution* to catalog for exactly this reason. It
      would also mean two services own "a rule attached to a venue", which is the ownership
      ambiguity ADR-002 exists to prevent.

## Decision

**We adopt Option 2.** Fee rules are typed declarative rows in a `fee_rules` table in catalog,
resolved **per fee code** through ADR-036's hierarchy.

### 1. What a fee rule may express

A rule is a row: one scope, one fee code, one typed basis, one incidence, an optional channel, an
optional effective window, an explicit priority and an override flag.

**Basis** is a tagged union with three members:

| `basis` | value column | meaning |
|---|---|---|
| `per_ticket_fixed` | `amount` (minor units) | charged once per ticket |
| `per_order_fixed` | `amount` (minor units) | charged once per order |
| `percentage_bps` | `rate_bps` (0..10000) | basis points of the resolved unit price |

The union is **enforced in the database**, not merely documented: a CHECK requires a fixed basis to
carry an `amount` and no `rate_bps`, and a percentage basis the reverse. Without it a row can claim
a basis whose value is missing, and the resolver would have to guess on a money path.

`rate_bps` is bounded at 10000 — 100%. A rate above that is a fee larger than the thing it is a fee
on, which is never a configuration anyone meant.

**Deliberately not in v1:** tiered/banded fees (different rate above a threshold), min/max caps on a
percentage fee, and fees whose basis is another fee. Each is expressible later by widening the
tagged union — additive, exactly as ADR-036 built `action_kind` to be. None has a story asking for
it.

### 2. Rounding: floor, at the individual ticket

**A percentage fee rounds DOWN (toward zero), computed per individual ticket.**

The unit matters more than the direction, and it is not a detail. Two tickets at 150¢ and one at
100¢, at 333 bps:

| rounding unit | computation | result |
|---|---|---|
| **per ticket (chosen)** | 4 + 4 + 3 | **11¢** |
| per line | ⌊300×333/10000⌋ + ⌊100×333/10000⌋ = 9 + 3 | 12¢ |
| per order | ⌊400×333/10000⌋ | 13¢ |

Per-ticket rounding is the only one of the three whose answer does not depend on **how an order
happens to be grouped into lines** — and line grouping is a UI and cart concern that will change
without anyone thinking they touched money. The other two make the fee a function of the basket's
shape.

Direction is *down* because a fee is charged by the platform to a buyer, and the platform should
not round in its own favour by default. This is a policy choice, not a derivation; it is stated so
it can be disagreed with rather than discovered.

The computation is `amount * rate_bps / 10000` in **integers**, with the multiplication **checked**
for overflow before the divide. Floats stay banned (ADR-001, ADR-036 §2).

**A resolved fee of amount 0 is still a resolved fee.** `⌊1 × 333 / 10000⌋` is 0, and the fee code
must still appear in the breakdown with its payee. Otherwise a code blinks in and out of existence
as a function of price, and TKT-216/217 inherit a payee that is sometimes owed nothing and
sometimes absent — two different states that would look identical in a ledger.

**Catalog does not do any of this arithmetic** (§ 5). This section is the contract TKT-215
implements, written here because the rounding rule belongs with the model rather than with one
consumer of it.

### 3. Incidence

`incidence` is `passed_on` or `absorbed`.

- `passed_on` — added to what the buyer is charged.
- `absorbed` — borne by the organizer out of the face value. The buyer never sees it.

**Incidence is not a display flag.** It changes what the card is charged, and an absorbed fee must
still be *recorded*, because TKT-217 has to pay it to a payee out of money the buyer already paid.
A build that treats incidence as presentation charges the wrong total in one channel and cannot
settle in the other.

Incidence affects **nothing** about eligibility, precedence, currency validation or rounding. It is
carried on the winning rule and is part of provenance.

### 4. Channel

`channel_code` is an **exact opaque string**, matching the shape and length bound of
`claims.channel_code` in inventory (ADR-024). There is no channel registry and none is invented
here — ADR-024 defers that to TKT-17, and a vocabulary invented on this ticket would decide that
story.

- `NULL` is **channel-agnostic**: eligible in every channel, including the default/public one.
- A non-NULL code is eligible only on an **exact** string match.
- A request that names **no** channel is the default/public context, in which only channel-agnostic
  rules are eligible. **Omitting the channel is not a wildcard**, and a channel-specific rule never
  applies to a sale that named no channel.

**Channel specificity is a ranking axis below scope and above priority.** At the *same* scope
level, an exact-channel rule beats a channel-agnostic one — a rule authored for one channel is the
narrower statement of intent, which is the same policy that puts an event rule above a venue rule
in ADR-036 §1. A **broader** exact-channel rule does **not** beat a **narrower** channel-agnostic
one: scope is compared first.

Placing channel *above* priority is deliberate and testable: a channel rule wins even when the
agnostic rule carries a higher priority. Priority exists to disambiguate rules of equal
specificity, and a channel rule is not of equal specificity.

**A rule belonging to a different channel is not reported as a loser — it is absent.** Returning it
would publish one caller's whole channel fee matrix to every other caller. Adding an optional
diagnostic later is additive; retracting rows already exposed is not.

### 5. Ownership: catalog resolves, commerce computes. ADR-002 is not amended.

- **Catalog answers**: *which fee rules apply to this ticket type, in this channel, at this instant,
  and why.* It reports each winner's basis and value; it never multiplies.
- **Commerce answers**: *what fee amount belongs in this order total.* Quantity, line composition
  and the order total are its data, not catalog's.

ADR-036 §6 amended ADR-002 once, to move rule-based unit-price **resolution** to catalog. That
amendment was necessary because the *price* is a single number the sale consumes directly. Fees are
not: the number depends on quantity and basket shape, which catalog does not have. So the existing
ADR-002 row — catalog owns rule definitions, commerce owns "pricing/fee/promo evaluation" — is
already correct for fees, and **this ADR amends nothing**.

Stating that explicitly because ADR-036 is emphatic that asserting a boundary change "does not
really change the boundary" is not one of the options. Here there is no boundary change to assert.

### 6. The resolution read is `/internal/`, and that is a security decision

`GET /internal/ticket-types/{id}/fee-resolution`, declared in catalog's OpenAPI document with
`security: []`, guarded by the same inline `X-Internal-Token` check catalog's other internal routes
use, refusing with **401**.

**Why not public, like `price-resolution`.** The response carries `absorbed` fees, which are *the
organizer's cost structure* — what the platform and the venue take out of a face price — rather than
anything the buyer pays. TKT-155 is an open backlog ticket recording that catalog's public
`price-resolution` already discloses unannounced future prices through its `candidates` array, and
calls that a commercial disclosure worth fixing. Mirroring that route here would repeat a known
defect on strictly more sensitive data.

**Why declared-in-contract rather than hand-mounted.** [ADR-043](./ADR-043-where-a-service-auth-guard-lives.md)
draws the line at *contract operation vs internal route* for **where the guard lives**, not for
whether an internal route may be documented. Commerce is the in-repo precedent for the combination
used here: internal paths **documented** in its contract, declaring no security, guarded by an
inline check. Documenting it buys contract-first (ADR-009), fail-closed response validation
(ADR-028) and generated client types; the gateway's edge deny on `/api/<svc>/internal/` is what
makes it unreachable from the internet by construction.

**Why 401 rather than commerce's 404.** ADR-043 argues 404 is the better answer *and* explicitly
declines to churn catalog's existing 401 internal routes. A new catalog route answering 404 would
leave catalog with both codes, which is worse than either. Consistency inside one service wins here;
the cross-service split stays documented and deliberate. A future ticket may unify them — that is an
ADR-043 amendment, not this ticket's call.

**The cost, stated:** absorbed-fee amounts are invisible to the storefront, so a future story that
displays *passed-on* fees to buyers needs a public projection. That is additive. Retracting a
published public field is a contract break. This is the more reversible direction as well as the
safer one.

### 7. Deliberate duplication of the comparator

`SelectFeeRules` duplicates roughly thirty lines of `SelectPricingRule`'s ranking rather than
sharing a helper with it. **This is a decision, not an oversight.**

Extracting a common ranker would mean editing a shipped money path in a ticket that adds no pricing
behaviour — pure regression surface against zero benefit. And the two are not the same function: the
fee comparator adds a channel axis, partitions by code, and returns a set. A helper parameterized
over all of that is two functions wearing one name.

**The trigger for revisiting is a THIRD rule kind, not a second.** Two similar comparators are
cheaper to keep honest than one over-general one; three are not.

#### Amended by TKT-237 — the gap narrowed, the decision stands

TKT-237 gave `SelectPricingRule` the channel axis, so **one of the three differences named above is
now gone**. The honest position after that ticket:

| | fee comparator | price comparator |
|---|---|---|
| channel axis | yes | **yes** (was: no) |
| partitions by code | yes | no |
| returns | a set — one winner per fee code | one winner |

What remains is **arity**, and it is not a detail that could be parameterized away: a fee resolution
answers "one winner per code" and a price resolution answers "the winner". A helper spanning both
returns either a set the price path must unwrap or a single value the fee path must call in a loop,
and in both directions the shared function stops describing what either caller asked.

**The trigger is unchanged: a third rule kind.** Stated explicitly *because* the gap narrowed — the
next reader will see two comparators that now rank on the same axes in the same order and reasonably
conclude §7 is stale. It is not. The argument was never "they are very different"; it was that
merging them edits a shipped money path for zero behavioural gain, and TKT-237 did not change that.

Two things did change, and both are recorded so a future merge starts from facts rather than from
the resemblance:

- **The ranking is duplicated; the loss-reason vocabulary is not.** `ReasonLessChannelSpecific` is
  declared once (in the fee resolver) and used by both, because it is a value in **two closed
  OpenAPI enums** and a price resolution emitting a different spelling from a fee resolution would
  be two contract bugs wearing one typo. Sharing data is not sharing logic.
- **The two comparators are pinned by two independent truth tables** (`fees_test.go`'s and
  `pricing_test.go`'s), each derived from its own §. A merged ranker would have to satisfy both, and
  the tests are the cheapest existing statement of what "both" means. Whoever revisits this should
  read them first — if a single implementation cannot pass both unchanged, that is the answer.

A third rule kind gaining a channel axis is the point at which the duplication stops being cheaper
than the abstraction. TKT-237 is the second, and the count is what the rule turns on.

#### Amended by TKT-306 — the trigger HAS fired, and the determinism posture is now stated once

**The third rule kind exists.** `SelectSplitSchedule` (`splits.go`) ranks split schedules on the same
axes, with a channel axis. By §7's own test the duplication should now be revisited, and TKT-306 did
not do it — that ticket's scope explicitly excluded de-duplicating, and a ticket whose subject is
"the copies drifted apart" is the wrong place to merge them. **Recorded here so the next reader does
not have to re-derive that the trigger fired.** The decision is owed; it is not made.

What TKT-306 did do is align the copies it found drifting, and the rule they now share is stated
here once instead of three times in code:

> **The duplicate-id guard protects the determinism of the ANSWER, so it runs AFTER the channel
> filter.** The last tie-break is the id, so two rules sharing one are inseparable and the winner
> would depend on input order. A rule ineligible for the requested channel is dropped and never
> ranks, so it cannot make the answer ambiguous — refusing on it would reject a resolution that has
> exactly one correct result. Eligible duplicates still error, including ones that lost on their
> window: those are still REPORTED in provenance, and two of them under one id is the same
> order-dependence the caller reads.

Where each resolver stands against it:

| resolver | guard | position |
|---|---|---|
| `SelectPricingRule` | `ErrDuplicatePriceRuleID` | after the channel filter (narrowed by TKT-237) |
| `SelectFeeRules` | `ErrDuplicateFeeRuleID` | after the channel filter (**moved there by TKT-306**; it ran before) |
| `SelectSplitSchedule` | **none — deferred, see below** | — |

`SelectSplitSchedule` returns a bare `SplitSelection` with no error, so it cannot carry this guard
*in the same shape* without changing its signature — and its one production caller
(`fees_postgres.go`) runs it inside a loop over fees on a money path, which would make fee resolution
newly failable mid-loop. That is a behaviour change, not an alignment, so TKT-306 left it alone.

**DEFERRED, NOT IMPOSSIBLE — and the first version of this amendment said "structurally impossible",
which was false** (TKT-306 ai-review). At least three designs avoid the signature change: the caller
can validate the loaded schedule set once, before its fee loop; `SplitSelection` can carry an invalid
state the caller converts; or the load path can reject duplicates where it reads them. Writing it off
as structural suppresses §7's revisit trigger on a **money-allocation** path, which is the opposite
of what recording it is for.

**What is true today, measured rather than argued.** Two schedules sharing an id and tied on every
ranking axis produce an **order-dependent winner**: feeding `[a, b]` and `[b, a]` returns different
payees, and both duplicates are absent from `Candidates`, so the provenance does not even show the
ambiguity. Unreachable through Postgres — `fee_split_schedules.id` is the primary key — exactly as
for the other two resolvers, whose guards exist anyway *because* a pure seam should not rely on the
storage layer's luck.

So the honest statement is: **the price and fee comparators refuse this input; the split comparator
silently picks one.** That asymmetry is a real gap, it sits on the payout path, and it is owed a
decision along with the §7 revisit trigger it belongs to.

The determinism guard is unreachable through Postgres in all three cases — id is the primary key.
The two that exist are pure seams refusing to pretend the invariant holds by luck; the third has not
been written.

### 8. Precedence, in full

Filters first, then ranking. Getting this order wrong — in particular treating the forced partition
as a tie-break rather than a filter — lets a narrower unforced rule beat a forced one, which is the
exact inversion `force_ancestor_override` exists to prevent.

    FILTERS                              RANKING (within one fee code)
    0. (scope_level, scope_id) match     5. scope level      (inverted if forced)
    1. currency, on non-past rules       6. channel specificity (inverted if forced)
    2. effective window                  7. priority
    3. channel eligibility               8. stable rule id
    4. forced partition, per code

**Both specificity axes invert under the forced partition.** A forced rule is a house floor that
refuses to be undercut, so the **broadest** such statement binds — and a channel-agnostic rule is the
broader statement. Left undefined, this case would fall through to priority and be decided by
accident, on a money path.

**Currency is checked before the window filter and independently of channel.** A rule whose currency
differs from the ticket type's fails the resolution loudly rather than being skipped — silently
skipping a misconfigured rule on a money path charges the wrong fee and looks like nothing happened.
The two edges, both inherited from ADR-036 §4 step 1:

- a rule whose window has **closed** is inert and is *not* checked: it can never charge anything
  again, and failing on its account would be permanent and unrecoverable, since currency is
  immutable and `effective_until` only shortens;
- a rule for **another channel** *is* checked: it is still misconfigured, and it will charge the
  moment a sale arrives on that channel. Failing now is what makes it findable then.

### 9. Considered-and-inapplicable is not the same as absent

A fee code whose every rule is outside its effective window resolves to an entry with a **null
winner** and the expired rules as candidates. A code no rule carries produces **no entry at all**.

TKT-152 had to relearn this on the price side: dropping the provenance satisfies "an expired rule is
not charged" while destroying the only answer to the question anyone actually asks, which is *"why
is the booking fee not showing up?"*.

## Consequences

- **Positive:**
    - The price path is untouched. This ticket cannot regress a price.
    - Fee rules inherit the hierarchy, the windows, the write gate and the index proof that ADR-036
      already argued and TKT-151/152 already built, at the cost of one duplicated comparator.
    - Per-code resolution means fees are additive with **no stacking order to define** — the thing
      that would otherwise have needed its own decision.
    - A fee schedule flips by the clock alone: no cron, no scheduled write, no job that can fail at
      00:00 on an on-sale.
    - Absorbed fees are not published.
- **Negative:**
    - Two comparators to keep semantically aligned (§ 7 bounds when that stops being acceptable).
    - No authoring API. Fee rules can only be created through the store, so operators cannot yet
      author them — TKT-216 or a back-office ticket owns that surface, and until then the feature is
      real but unreachable from outside.
    - Fee codes are opaque and unvalidated, so a typo is a silently-not-applied fee. Deliberate: a
      registry is TKT-17's.
    - Channel codes are equally opaque, so a fee can be authored for a channel no sale will ever
      name (ADR-024 accepts the same exposure for allocations).
    - Nothing consumes this yet. The whole feature is inert until TKT-215.
- **Not decided here, and owned elsewhere:** the payee model and split arithmetic (TKT-216), the
  settlement ledger and its integrity claim (TKT-217), and what happens to fees and splits on a
  refund or a cancellation run — carved out of TKT-6 explicitly, and **open**.

  **Corrected at TKT-215 (this paragraph previously said the opposite, and it mattered).** An
  earlier revision claimed *"a refund returns the buyer's money including passed-on fees, while the
  settlement entries attributing those fees to payees are not reversed"*. That is wrong, and wrong
  in the more dangerous direction. `store/refunds.go` computes the refund as
  `quantity × unit_amount` — deliberately, so the money path carries no division — and
  `unit_amount` is the **face** price. So once TKT-215 charges face + passed-on fees, a full refund
  returns **face value only** and the buyer is **under-refunded by the fees they actually paid**.
  Not a bookkeeping asymmetry between refund and settlement: a buyer-facing shortfall on money
  collected, which arrives as a chargeback rather than as a reconciliation discrepancy. Cancellation
  runs inherit it through the same projection.

  It stays deferred because whether a service fee is refundable is a **product** decision — many
  ticketing platforms deliberately do not refund fees — and neither TKT-215 nor this ADR should
  answer it by accident in either direction. What is *not* deferred is saying it accurately.

  **Exchanges are a separate case and were NOT deferred.** An exchange compares
  `targetTotal − sourceTotal` where the target is a rule-resolved price carrying no fee, so reading
  a fee-inclusive source would have refunded the fee on an *even* exchange. That is arithmetic, not
  policy, and TKT-215 fixed it by storing the face value explicitly and pointing the delta at it —
  while the `order.exchange.reversed` money fact continues to reverse the **gross**, because the
  journal must agree with what was captured.

## Amendment (2026-08-27, TKT-155) — price resolution is internal too, and §7's justification narrows

`GET /ticket-types/{ticketTypeId}/price-resolution` moved to
`GET /internal/ticket-types/{id}/price-resolution`. Both of catalog's resolution reads are now on the
internal surface, for two different disclosures: fee resolution carries `absorbed` fees (the
organizer's cost structure), and price resolution carries `candidates` — **every considered rule**,
including those that lost with `outside_window_future`. That is an organizer's unannounced future
prices and the shape of their whole rule ladder, readable before the on-sale by a competitor, a
reseller, or a buyer waiting for a drop.

**What this does to the §4/§7 amendment of TKT-237.** That amendment hid a foreign channel's rule
from **price** provenance "for a strictly larger reason" than it hid it from fee provenance — the
larger reason being that `price-resolution` was PUBLIC where fee-resolution was `/internal/` (§6).
**That premise is now false.** The two operations sit on the same surface behind the same credential.

**The behaviour is deliberately UNCHANGED.** Foreign-channel rules stay absent from price
`candidates`. The justification narrowed; the conduct did not, for two reasons:

1. Relaxing it would **widen a payload** on a ticket whose entire purpose is narrowing an audience.
   That is the wrong direction to take incidentally, inside a security change.
2. The remaining justification still stands on its own — the one fee resolution always had. Reporting
   other channels' rules publishes which channels carry bespoke pricing and at what amounts, to every
   service holding the internal credential. Sharing a trust boundary is not the same as having no
   reason to withhold.

If anyone wants that relaxed, it is a deliberate ticket with its own argument, not a side effect of
this one. §7's revisit trigger is otherwise unchanged.

**Consequence for callers.** Commerce's sale path (`services/commerce/internal/api/catalog_pricing.go`)
now sends the internal credential to this route. Before the move it deliberately withheld it, on the
grounds that putting a service credential on a publicly routable path would be strictly worse than the
exposure — both halves of that reasoning moved together. No storefront or back-office code called the
operation.

## References

- TKT-214 (this ticket) · TKT-6 (epic) · TKT-215, TKT-216, TKT-217 (consumers)
- [ADR-036: pricing rules representation](./ADR-036-pricing-rules-representation.md) — the model
  extended here; §1 scope levels, §2 integer safety for relative actions, §3 storage and index, §4
  the comparator
- [ADR-002: services from day one](./ADR-002-services-from-day-one.md) — the ownership row, unamended
- [ADR-001](./ADR-001-go-typescript-stack.md) — money is integer minor units; floats banned
- [ADR-009](./ADR-009-contract-first-apis.md) · [ADR-028](./ADR-028-response-drift-fail-closed.md) —
  contract-first and fail-closed response validation
- [ADR-019](./ADR-019-catalog-read-path-scoping.md) · [ADR-020](./ADR-020-catalog-index-build-concurrency.md) —
  the scoped-read evidence pair and plain `CREATE INDEX`
- [ADR-024: channel allocations](./ADR-024-channel-allocations.md) — channel codes are opaque strings
- [ADR-043: where a service's auth guard lives](./ADR-043-where-a-service-auth-guard-lives.md) — the
  contract-vs-internal line, and catalog's 401
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) — name the adversary: the write gate is
  honest-writer consistency, not tamper-evidence
