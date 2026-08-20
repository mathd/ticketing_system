# ADR-064 — Presale unlock codes

- **Status:** Accepted
- **Date:** 2026-08-10
- **Ticket:** TKT-239 (epic TKT-17, US-CH5)
- **Supersedes:** nothing. **Extends:** ADR-024 (channel allocations), ADR-054 (sales windows).

## Context

ADR-024 gave a channel **how much** it may sell. ADR-054 gave it **when**. Neither says **who**.
A presale that opens Tuesday to everyone is not a presale — it is an early on-sale.

No code, voucher or redemption machinery existed anywhere in the repo before this ticket. That
absence is why the decisions below are worth recording: nearly all of them had a plausible
alternative that would have worked in a single-slot demo and failed in production.

## Decisions

### 1. The gate lives in **inventory**, on `channel_allocations`, not on catalog's channel registry

`channel_allocations.requires_code boolean NOT NULL DEFAULT false`.

The obvious home is catalog's `channels` row (TKT-235), and it is wrong twice over:

- That registry is deliberately **a lookup, not a constraint**. `0018_channels.sql` says so at
  length and `TestAnUnregisteredChannelCodeStillSells` pins that an unregistered code still sells.
  A `may this sell` flag on that row reverses a recorded decision.
- **Inventory has no read path to catalog's tables at all** (ADR-002) — separate services, separate
  databases, no client, no consumer, no replica. A gate there means a cross-service read on the
  claim path, which is what ADR-010's single-serialization-point rule exists to prevent.

So the flag sits on the row the claim path **already reads under the pool lock**.

**Accepted cost:** gating is configured per allocation, not once per channel. An organizer gating a
channel across a festival sets it on each slot's allocation. The alternative was a cross-service
dependency on the highest-contention path in the system.

**Owner decision**, taken explicitly when the conflict surfaced at re-shaping.

### 2. Redemption is serialized by the **`presale_codes` row lock**, not the pool lock

`SELECT … FROM presale_codes WHERE (organizer_id, channel_code, code) … FOR UPDATE`, taken **after**
the pool lock.

This is the ticket's load-bearing decision and the one that is easiest to get wrong, because the
wrong version looks right:

Every other derived count in inventory is scoped by `pool_id` — `reservedForChannelsSQL`,
the channel-cap sum, all of them. That is **why** the pool row `FOR UPDATE` suffices for a channel
cap. A presale code is `(organizer_id, channel_code, code)` and **spans every slot in the presale**,
which is the entire point of a code. Two concurrent holds on **different slots** take **different
pool locks**, never block each other, both read usage = N−1, and both insert.

**Verified, not argued:** with the row lock removed, a code capped at 5 granted **7**.

**Lock order is fixed: pool → code**, never the reverse. Nothing else in inventory locks a
`presale_codes` row, so no other order exists and self-deadlock is impossible.

Rejected: `pg_advisory_xact_lock` on a hash of the code identity (ADR-029's shape). That pattern
exists in *catalog* precisely because a seat-map family identity **is not a row**. Here the row
exists, and `refund_returns.go` already does pool-lock-then-second-row-lock in this service. The
exotic mechanism would have needed a justification it does not have.

### 3. Consumption is **derived**, never a counter

`codeRedeemedQuantity` sums `consumedQuantity` over claims citing the code, filtered by
`consumingClaims` — the same expressions that count against a channel cap, so the two can never
disagree.

A counter cannot be decremented by **lazy** hold expiry without a sweeper, and ADR-010 forbids
requiring one for correctness. Derived also gets refunds right for free: `consumedQuantity` is net
of `returned_quantity`, so a refund returns the redemption exactly as it returns the seat.

The **confirmed** half of `consumingClaims` is load-bearing and easy to lose: a live-only count
would hand every redemption back the moment a buyer paid, so a code capped at N would sell
unbounded tickets — failing only *after* payment. A mutation check caught this gap in the test
suite, not in the code.

### 4. The refusal is **uniform across five causes**

Absent, unknown, wrong-channel, exhausted, and out-of-window all return `ErrPresaleCodeInvalid`
with an identical message and an identical wire body: `409 {"code":"presale_code_invalid",
"error":"invalid presale code"}`.

A distinguishing refusal is an **enumeration oracle** — submitting candidates and learning "exists
but spent" versus "no such code" is how presales get scraped.

**Name the adversary (ADR-021).** This defeats a scraper reading **response bodies**. It does **not**
defeat one measuring **latency** — an exhausted-code refusal costs a usage aggregation an
unknown-code refusal does not — nor one who simply observes that a channel is gated at all. Rate
limiting (ADR-051) is the control for those and is **not claimed here**.

The **operator** read (`PresaleCodeStatuses`) does distinguish them, deliberately: an operator
debugging "my code doesn't work" needs the answer, and that surface is internal-only.

### 5. Refusal **precedence**: window → code → capacity

1. No/released allocation → code-less `insufficient capacity` (there is no channel to gate)
2. **Closed channel window** → `channel_window_closed`
3. **Bad presale code** → `presale_code_invalid`
4. Pool or channel capacity → code-less `insufficient capacity`

Window before code: a closed channel is not selling to anyone, so "wrong code" would be a misleading
answer to a request a *valid* code would also have refused. Code before capacity, for the reason
TKT-238's review established — a channel-property refusal masked by a full pool makes a gated
presale read as a **sellout exactly when the on-sale is busiest**.

### 6. A draw-down **moves** a redemption — the child inherits the citation

The source reservation cites the code, and **every draw-down child inherits it**.

**This reverses the decision this ADR originally recorded, and the reversal is the point.** The
first version had children *not* inherit, reasoning that `consumingClaims` counts a live source and
its live children, so citing both would double-count and exhaust a cap at half value.

That reasoning is false. A draw-down **decrements the source by exactly the drawn quantity**, or
releases it whole when fully drawn — source + children always sums to the original. Citing both is
conservative, not duplicative.

The consequence of the wrong version was measured, not argued: drawing a 10-unit reservation fully
down took derived usage from 10 to **zero** while all 10 units remained consumed, and the same
"capped at 10" code then granted **10 more**. Twenty units from a cap of ten.

It survived a mutation check and a green test, because the test asserted the *wrong invariant* —
usage `== 6` after drawing 4 of 10, which is exactly what the defective code produced. A test can
encode a mistaken rule as confidently as a correct one.

The source's code is read **under the same row lock** as the rest of the source claim, so a child
cannot inherit a citation that disagrees with the units being moved.

Still true, and still the reason gating a draw-down would be wrong: a draw-down consumes nothing
new, so it **redeems** nothing new. Inheriting a citation is not redeeming — the redemption already
happened at placement.

### 7. Codes are **exact opaque strings**

No `citext`, no `lower()`, no trimming — at the schema, in Go validation, and at the HTTP decode.
`VIP`, `vip` and ` vip ` are three different codes, exactly as channel codes are (ADR-024, ADR-046
§4). Normalizing anywhere would disagree with the exact-match lookup and produce codes that can be
issued but never redeemed.

### 8. The claim's citation is **plain text with no foreign key**

`claims.presale_code`, nullable, length-checked, with a CHECK that it is never set without a
channel. No FK, for the reason `claims.channel_code` has none: historical attribution must survive
a code being edited or deleted. A claim records what happened, and what happened does not change
when configuration does.

A code presented to an **ungated** allocation is ignored **and not recorded** — persisting it would
let any caller write arbitrary strings into a column reporting reads, on a path where nothing
validated them.

### 9. The idempotency fingerprint appends the code **only when non-empty**, and **frames** it

A code must enter the fingerprint — on **both** the hold and the group-placement paths. Two requests
sharing a key but presenting different codes are different requests, and replaying one as the other
grants the second the first's redemption *without its code ever being checked*. Plan-review caught
this for `CreateHold`; ai-review caught that `PlaceGroupReservation` had the identical defect.

An **unconditional** append rehashes every claim in the database, so every in-flight retry stops
replaying and re-executes — a system-wide double-sell on retry, from what looks like adding a field.
Golden literals computed outside the package pin the pre-TKT-239 values; changing them is a
wire-compatibility break, not a test update.

And the appended part is **length-framed**, not colon-joined. Channel codes and presale codes are
both arbitrary opaque strings that may contain the delimiter, so a bare join is ambiguous:
`(channel="a", code="b:c")` and `(channel="a:b", code="c")` hashed **identically** — measured — and
the second request replayed the first before its allocation or code was examined. Framing applies
only to code-bearing requests, so **no fingerprint that can already exist in a database changes**.

A second review pass argued the framing broke compatibility with fingerprints written by this
ticket's own first commit. **Rejected on its premise:** that commit lives only on an unmerged
feature branch, never ran against a database, and a code-bearing request could not have existed
before the feature that introduced codes. The only compatibility boundary is pre-TKT-239 →
shipped, every stored fingerprint is code-less by construction, and
`TestOnlyCodeLessFingerprintsNeedBackwardCompatibility` pins it.

## Guarantees, and what they are not

**Honest-writer consistency, not tamper-evidence** (ADR-021). The row lock and the derived count
stop concurrent over-redemption and operator mistakes. Anyone with **inventory database access**
mints redemptions, edits caps, or inserts claims at will. Nothing here is tamper-evident, and no
claim in this ADR should be read as saying otherwise.

## Consequences

- `presale_code_invalid` joins the **closed** `Error.code` enum. Under ADR-028 an undeclared value
  is a **500 in production**, so the enum entry, the handler mapping, the exact-body assertion and
  the load-harness classification move as one unit.
- The load harness keeps it as `KindServerError`: a run against a gated channel measures the refusal
  path, not contention, so counting it as capacity evidence would let a misconfigured on-sale proof
  report a clean sellout curve while selling nothing.
- `make browser` is not required — there is no web surface in this ticket. Issuance is API-only; a
  back-office UI is out of scope.
