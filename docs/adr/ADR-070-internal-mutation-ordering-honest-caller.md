# ADR-070: Cross-service ordering is an honest-caller guarantee, enforced in the caller and declared at the boundary

Date: 2026-09-01

## Status

Accepted (TKT-165; decided under the autonomous gates of that run, recorded on the ticket).

Anchored in **[ADR-043](./ADR-043-where-a-service-auth-guard-lives.md)**, which draws the line for
where a guard lives. This does not move that line: no guard is added, moved or removed. It answers a
different question — whether a *callee* should demand proof that its caller did something first — and
records that the answer is no, together with what is done instead.

Closes the question **[ADR-038](./ADR-038-refund-reversal-ticket-voiding.md) §6** hands to TKT-165
and the *"Whether inventory should demand proof of prior voiding"* bullet in
**[ADR-062](./ADR-062-refund-reversal-reconciliation.md) § Open questions**. Both are updated to
point here.

Reached by the same reasoning as **[ADR-053](./ADR-053-channel-operator-read-credential.md)** and
**[ADR-057](./ADR-057-inventory-staff-write-credential.md)**, which answered the same-shaped question
twice by **narrowing a credential** rather than by demanding proof of prior action. §5 records that
one of the endpoints below has already had that remedy applied.

## Context

TKT-161 established a **safety ordering** for a refund's reversal: the tickets must be voided
*before* the capacity is returned, because freeing the seat while the original ticket still admits is
the one sequence that can **oversell** (ADR-038 §1). Commerce enforces it. Inventory's
`POST /internal/holds/{id}/refund-capacity` does not — it validates the claim and the quantity, not
that anyone voided anything.

TKT-161's adversarial review (F1) recorded that as a finding rather than fixing it, and proposed a
capability produced only by a successful voiding: a durable ordered event, or a signed token from
access. That is a real design with real cost — a new cross-service credential, its issuance,
verification and expiry — so it was routed here rather than smuggled into a refund ticket.

**This is the existing trust model, not a new hole.** `POST /internal/holds/{id}/release` has always
been able to free a whole claim's capacity with no evidence of anything, and it is one line:

```go
r.Post("/internal/holds/{id}/release", s.internalOnly(s.transition("released")))
```
<sub>`services/inventory/internal/api/server.go:122`</sub>

The question is therefore about the **model**, not about one endpoint: should a cross-service
ordering invariant be enforced where it is mutated, and does that generalize to every internal
endpoint or only to those guarding an oversell?

## Decision

**No. Cross-service ordering is an honest-caller guarantee. It is enforced structurally in the
caller — the only party that knows both facts — and declared in the callee's served contract so the
assumption is visible where people integrate. No boundary receipt, no proof-of-prior-action, no new
credential.**

### 1. The invariant, stated once

Every endpoint in §4 carries the same shape: a **capacity-versus-entitlement** invariant, whose
unsafe direction is always *"free or sell the seat while the old entitlement still admits."*

The reverse order can only **under-sell** — void first and the seat comes back late; capture first
and the seat is sold late. An under-sell is a lost sale; an oversell is a buyer turned away at the
door with a valid ticket. That asymmetry is why only one direction needs a guard, and it is why the
guard belongs where the sequencing decision is made.

### 2. Where it IS enforced

In commerce, structurally, by an early return rather than a comment:

```go
func (s *Service) driveOrderedReversal(ctx context.Context, rev reversal) reversal {
	rev = s.discharge(ctx, rev, obligationTickets)
	if !rev.TicketsVoided {
		return rev
	}
	return s.discharge(ctx, rev, obligationCapacity)
}
```
<sub>`services/commerce/internal/refunds/service.go:210-216`</sub>

Its doc comment states the reason and the consequence: *"an access outage leaves BOTH obligations
outstanding rather than letting the second run without the first."* The marker is written from the
**recorded** discharge, not from the call having been made — `discharge` deliberately does not mark
optimistically, because *"marking optimistically instead would be the one way to lose the ordering
guarantee — capacity could then be returned on the strength of a voiding that never happened."*

Pinned by `TestVoidReturnsCapacityOnlyAfterTicketsAreVoided` and
`TestVoidWaitsForTheRecordedMarkerNotTheCall`
(`services/commerce/internal/refunds/reversal_order_test.go:82,139`). **Those tests are the evidence
for this whole decision.** If they go, the ratification below is unsupported.

### 3. Why no boundary receipt — name the adversary (ADR-021)

The adversary for inventory's and commerce's internal surfaces is **a holder of
`INTERNAL_SERVICE_TOKEN`**. That is not hypothetical framing: inventory's own startup check calls it
the credential that *"opens every inventory operation"*
(`services/inventory/cmd/inventory/main.go:119`).

**State the path precisely first, because an earlier draft of this ADR did not.**
`refund-capacity` is the **only** internal mutation that returns capacity from a claim while that
claim's tickets may still admit. The two endpoints are **disjoint in claim state**, not adjacent
doors:

```go
if c.Status != "held" && c.Status != "finalizing" { return c, ErrConflict }
```
<sub>`services/inventory/internal/store/store.go:841` — so a `confirmed` claim cannot be released</sub>

```go
if status != "confirmed" || kind != "buyer" { return RefundCapacityReturn{}, ErrRefundReturnNotConfirmed }
```
<sub>`services/inventory/internal/store/refund_returns.go:106` — and refund return requires exactly that state</sub>

Tickets exist only after confirmation, so a claim whose ticket admits is never releasable. ADR-016
already says so in as many words. **The oversell path is real, single, and specific to this
endpoint.**

So the decision does not rest on there being an easier path to the same harm through
`release` — there is not. It rests on what the receipt would and would not contain:

1. **A receipt would bind an honest caller, not this adversary.** It is worth being exact about what
   it buys, because overstating this is how the previous draft went wrong. A receipt issued only by a
   successful void **is** obtainable only by performing the void — authority to invoke the issuer is
   not authority to forge its signature (`services/access/internal/api/refunds.go:28-48`). Against a
   buggy or mis-sequenced *honest* caller, a receipt would genuinely enforce the protocol. That is a
   real property and this ADR does not claim otherwise.
2. **It does not contain the named adversary, because the same credential can oversell directly and
   by a wider margin.** `POST /internal/slots/{id}/capacity-adjustments` sits behind the same
   `internalOnly` guard (`services/inventory/internal/api/server.go:139`). Its store refuses only a
   non-positive value and **applies raises freely, with no external upper bound**:

   ```go
   if newCap <= 0 { return CapacityAdjustment{}, false, fmt.Errorf("capacity must be positive") }
   ```
   <sub>`services/inventory/internal/store/capacity.go:29-31`; the header at `:13-14` states the rule —
   *"raises apply freely; a cut below live demand clamps"*</sub>

   Ordinary holds then admit against that raised ceiling. A token holder can therefore sell above the
   venue's real capacity without touching `refund-capacity` at all, in one call, at any magnitude.

**A receipt would lock one door in a wall whose widest door stays open**, and would cost a new
cross-service credential with its issuance, verification, expiry and clock-skew policy. That is the
trade this ADR declines.

**What is NOT the argument here.** `POST /internal/holds` does *not* oversell: `CreateHold` locks the
pool and refuses when confirmed + held + requested exceeds the limit
(`services/inventory/internal/store/store.go:507-524,679-685`), accounting for reserved allocations
on the unchannelled path (`:707-715`). It is named because an earlier draft implied otherwise;
capacity adjustment is the direct path, and it is the only one.

**Could inventory enforce the ordering locally instead, with no new credential?** No, and this was
checked rather than assumed. Inventory holds no ticket, order or void state — its claims table
carries none (`services/inventory/internal/store/migrations/0001_inventory.sql:12-23`) and the refund
migration adds only `returned_quantity` (`migrations/0010_refund_capacity_returns.sql:7-16`). Its
consumer subscribes to catalog publication and lifecycle subjects, not to access ticket events
(`services/inventory/internal/consumer/consumer.go:21-37`). Access's voided-ticket feed reports
ticket ids, not a refund/hold/quantity binding (`services/access/internal/store/voided_feed.go:25-32`),
so consuming it would still need a new projection and a cross-service identity mapping. **Enforcement
here necessarily means new cross-service information.** That is the cost being weighed, and it is why
the answer is not "add a cheap local check".

So the honest statement is the ADR-021 one: **this ordering holds against concurrency, crashes,
restarts and mistakes. It does not hold against a token holder, and no in-system check at the callee
could make it** — because that token already commands a larger oversell directly.

What must **not** be concluded from that: the token is *not* worthless. It authenticates that the
caller is an internal process, which is what keeps the public internet away from these routes: the
gateway registers `/api/<svc>/internal/` to a not-found handler, so those paths are unreachable from
the edge (`gateway/cmd/gateway/main.go:42-49,226`, which names ADR-002's route-table-as-boundary
rule). What it does not authenticate is **ordering**, or
**which** internal process is calling.

### 4. The endpoints this applies to

**Scope of the sweep, stated exactly.** Counted at HEAD, the five services register **51 internal
routes, of which 31 are mutations**:

```
grep -hE 'r\.(Get|Post|Put|Patch|Delete)\("/internal' services/*/internal/api/server.go | wc -l   -> 51
grep -hE 'r\.(Post|Put|Patch|Delete)\("/internal' services/*/internal/api/server.go | wc -l       -> 31
```

Catalog's generated router installs three further internal GETs that this grep does not see
(`services/catalog/internal/api/openapi_gen.go:3467-3473`); they are reads and carry no ordering
invariant. **An earlier draft of this ADR said "all 26 internal endpoints", which was never the
population.** The claim below is scoped to what was actually examined — the 31 mutations — and says
so rather than asserting an exhaustive sweep it cannot support.

Of those 31 mutations, **four** depend on the caller having acted first. ADR-038 §6 named three, one
of which (`confirm`) it named correctly and two of which are payments operations that are **not**
ordering-dependent — see *Considered and excluded* below.

| # | Endpoint | Service | The caller must have done this first |
|---|---|---|---|
| 1 | `POST /internal/holds/{id}/refund-capacity` | inventory | voided the refunded tickets |
| 2 | `POST /internal/holds/{id}/release` | inventory | ensured the entitlement can no longer admit |
| 3 | `POST /internal/holds/{id}/confirm` | inventory | captured the payment |
| 4 | `POST /internal/exchanges/{id}/tickets-switched` | commerce | committed the entitlement switch |

**#3 deserves emphasis, because it is the highest-traffic internal mutation in the system and was
named nowhere before this ADR.** Commerce charges and then confirms; inventory's
`Transition(…, "confirmed")` checks only the claim's own status before permanently incrementing
`confirmed_quantity` (`services/inventory/internal/store/store.go:841-847`). It never sees payment. A
confirm without a capture is sold capacity that never paid.

**#4 is worth noting for the opposite reason: it exists to make an ordering checkable and is itself
ordering-dependent.** Access calls it after its switch transaction commits; commerce records
`tickets_exchanged_at` and only then returns the old capacity.

#### Considered and excluded

- **`POST /internal/psp/refund` and `POST /internal/psp/partial-refund`** (payments) — **excluded,
  correcting both an earlier draft of this ADR and ADR-038 §6.** Both were listed as
  ordering-dependent. They are not: they are the **start** of the sequence, not a step that depends on
  a prior cross-service action. Commerce refunds the money and only then drives the reversal —

  ```go
  factID, err := s.refundPayment(ctx, refund)   // :94  money moves FIRST
  ...
  return Result{Refund: s.DriveReversal(ctx, refund)}, nil   // :107  tickets and capacity AFTER
  ```
  <sub>`services/commerce/internal/refunds/service.go:93-107`; recovery takes the same order at
  `services/commerce/internal/recovery/runner.go:494-508,526-532`</sub>

  Describing them as ordering-dependent would **declare an ordering production does not follow**,
  which is worse than the silence it replaced: an integrator would sequence against a rule the system
  contradicts. Their served prose says what they do and what the caller owns, and asserts no prior
  action. (Their adversary is also narrower — §5.)

- **`POST /internal/holds/{id}/finalize`** — sits beside `confirm` behind the same guard, and is
  excluded deliberately. Finalizing moves a claim to `finalizing`, which inventory counts against
  availability and exempts from expiry; it commits nothing and frees nothing, so no ordering by the
  caller can make it oversell. Named here so the enumeration above can be checked against the router
  without a reader wondering.
- **`POST /internal/operational-holds/{id}/convert` and
  `POST /internal/group-reservations/{id}/draw-down`** (commerce's staff sales, both
  `staffSale` at `services/commerce/internal/api/server.go:1876`) — excluded because **the ordering
  is commerce's own**: it calls inventory and then persists locally, so there is no *"the caller must
  have done X first"* invariant available for a token holder to violate by calling out of order.

  Separately, and not the reason for the exclusion: a crash *between* those two steps is repairable.
  The reservation id is deterministic — `uuid.NewSHA1(NameSpaceOID, ns+organizer+":"+key)`
  (`:1977`) — the INSERT is `ON CONFLICT(id) DO NOTHING` (`:1982`), and the idempotency key is
  forwarded to inventory, so replaying the same request completes the pair. That is a good property;
  it is **not** what makes these endpoints outside this ADR's scope, and the two should not be
  conflated.

### 5. Payments already has the remedy this ADR declines to generalize

`POST /internal/psp/refund` and `/internal/psp/partial-refund` are **not** behind
`INTERNAL_SERVICE_TOKEN`. Payments authenticates on the same header name against its **own**
credential, `PAYMENTS_INTERNAL_TOKEN` (`services/payments/internal/api/server.go:82`,
`shared/go/runtimecfg/runtimecfg.go:276`), and `PaymentsTokenFromEnv` **refuses a value equal to the
shared token**:

> `PAYMENTS_INTERNAL_TOKEN must not equal INTERNAL_SERVICE_TOKEN: the separate credential exists so
> a compromise elsewhere does not reach the money surface, and an identical value removes that
> boundary while looking configured`

Commerce is the only holder. The comment recording the change says what it bought: payments *"used
to authenticate on INTERNAL_SERVICE_TOKEN, the one value every service holds — so a compromise of
any of them opened charge, void, refund and partial refund"*
(`services/payments/cmd/payments/main.go:333-336`).

**This is the ADR-053/ADR-057 answer, already applied.** It matters twice over:

- The adversary for the two payments legs is **narrower** than for #1–#4, and this ADR must not
  flatten the two into one. Naming a uniform adversary would understate what the system actually
  protects. This is also why they keep their served declaration (§6) even though they are excluded
  from the ordering table: what an integrator needs told is that payments knows nothing about the
  seat, which is a statement about scope rather than about sequence.
- It shows the shape of the remedy if a specific operation ever warrants one: **narrow the
  credential**, which reduces who can call at all, rather than demand a receipt, which the caller can
  always produce. A receipt asks *"did you do the thing?"* of a party that can always answer yes; a
  narrowed credential asks *"are you the one party allowed to ask?"*, which is a question with a
  different answer for different callers.

**No credential changes here.** §5 is a citation of a decision already taken, not a proposal.

### 6. What is done instead: declare it in the served contract

The one real gap this ticket closes. **Six operations carry the declaration**: the four in the table
above, plus the two payments legs, whose statement is that payments knows nothing about the
entitlement (§5). Of those six, only two carried any statement at all, and **both were YAML `#`
comments** — invisible to every generated client and to the rendered spec. Two (`confirm`, `release`)
carried no operation prose at all.

Each of the six now states, in its served `summary`/`description`: what the honest caller must
preserve, what the callee checks locally, and that the callee does **not** prove the cross-service
action. That text reaches `openapi3.T`, `GET /openapi.yaml`, and the generated
TypeScript clients; a `#` comment reaches none of them.

Pinned per service by a test that loads the embedded spec and asserts the marker is present on each
of the six operations. The discriminating mutation is exact: **move a sentence back into a `#`
comment.** Behaviour tests and `check-generate` stay green; the declaration test goes red — which is
the failure mode being fixed, reproduced.

## Consequences

- **Positive.** The assumption is where an integrator reads it. The next reviewer finds a decision
  rather than a gap. The enumeration is scoped to the 31 mutations examined and is checkable against
  the router, and #3 — the highest-traffic case — is documented for the first time.
- **Positive.** No mechanism, so no new credential to issue, verify, expire or rotate, and no new
  failure mode between services.
- **Negative, and accepted.** A holder of `INTERNAL_SERVICE_TOKEN` can still return capacity against
  un-voided tickets, or confirm a claim that never paid. That is unchanged by this ADR and is not
  closable at the callee — the same credential raises a pool's capacity outright (§3).
- **Negative, and accepted.** A **buggy honest caller** that returns capacity before voiding is not
  caught at the boundary either; nothing detects it until the oversell is observed at the door.
  A receipt would catch that case. This ADR judges the cost of a new cross-service credential higher
  than the residual risk, given that commerce enforces the sequence structurally (§2) with tests
  pinning it. **If that enforcement is ever weakened, this trade changes** — which is why §2 names
  those tests as the evidence for the whole decision.
- **Neutral.** ADR-043's line is untouched; ADR-038's and ADR-062's deferrals now point here.

### When to revisit

When an operation's blast radius stops matching its callers — that is the ADR-053/ADR-057 trigger,
and §5 is what taking it looks like.

**Revisit the receipt specifically if either premise of §3 changes:**

1. **Capacity adjustment stops being an unbounded raise** behind the shared credential — a ceiling,
   or its own narrowed credential. That removes the wider door, and the receipt would then be closing
   the last one rather than one of two.
2. **Commerce's structural ordering (§2) is weakened or removed**, making a buggy honest caller the
   live risk rather than a hypothetical one.

Do **not** revisit it merely because someone proposes a receipt on the grounds that the ordering is
unenforced: that is answered here, and the answer is that it is unenforced *deliberately*. Cite §3
rather than re-run it — but note §3 does **not** claim a receipt is worthless, only that it does not
bind the named adversary.

## References

- [ADR-043 — Where a service's auth guard lives](./ADR-043-where-a-service-auth-guard-lives.md) (the anchor; this ADR moves no guard)
- [ADR-053 — Channel operator read credential](./ADR-053-channel-operator-read-credential.md) · [ADR-057 — Inventory staff write credential](./ADR-057-inventory-staff-write-credential.md) (the same question answered by narrowing a credential)
- [ADR-021 — Ticket lifecycle trail integrity](./ADR-021-ticket-lifecycle-trail-integrity.md) (name the adversary before making a claim)
- [ADR-038 — Refund reversal ticket voiding](./ADR-038-refund-reversal-ticket-voiding.md) §1 (the ordering is a safety property), §6 (deferred the general question here)
- [ADR-062 — Refund reversal reconciliation](./ADR-062-refund-reversal-reconciliation.md) § Open questions (deferred here) · §2 *observed, not predicted*
- [ADR-002 — Services from day one](./ADR-002-services-from-day-one.md) (the route table is the security boundary; the gateway's edge denial of `/internal/` cites it) · [ADR-009 — Contract-first APIs](./ADR-009-contract-first-apis.md) (the served contract is the artifact)
- TKT-161 (raised it, F1) · TKT-165 (this decision) · TKT-166 (#4) · TKT-156 (the partial-refund leg)
