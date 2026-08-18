# ADR-060: A public reservation may not name a sales channel

Date: 2026-08-17

## Status

Accepted (TKT-248; option and two follow-up decisions taken by the owner at Gate 1, recorded on the
ticket). Closes the residual TKT-246 deliberately left open.

**Amends nothing.** In particular it does **not** amend ADR-024, and §"What this does not decide"
exists to keep that from being misread later.

## Context

`POST /reservations` is unauthenticated. Until this decision it accepted `channel_code` in the
request body, and that value reached **catalog**, which resolves on it twice:

- **fee rules** — per-channel since TKT-215 (ADR-046 §4);
- **price rules** — per-channel since **TKT-237**, which extended ADR-046 §4 and §8 to pricing.

TKT-246 closed the *inventory* half of this seam: a channel now reaches inventory only from an
authenticated partner credential, so a public sale consumes public stock and cannot drain a
reseller's allocation. It deliberately did **not** close the pricing half, and recorded that as a
decision rather than an oversight. Its reasoning, which still holds: forwarding a body-supplied
channel from an unauthenticated route is exactly the bypass TKT-240 was reverted for, and binding
allocations to a seller does not rescue it because an unbound allocation admits anyone.

### Why the residual was worse than "fee attribution"

TKT-248 was filed describing a **fee-attribution** defect. Reading ADR-046 §3 at shaping upgraded it:

> `passed_on` — added to what the buyer is charged. `absorbed` — borne by the organizer out of the
> face value. The buyer never sees it. **Incidence is not a display flag.**

So a public buyer naming a channel whose rules are `absorbed` **is charged less, and the organizer
bears the difference**. With TKT-237's price axis, the same caller could also reach a different base
price. That is a revenue leak reachable by anyone who knows or guesses a channel code — and channel
codes are not secrets (ADR-024: exact opaque strings, and the registry is a lookup, not a
constraint).

The defect's signature is why it survived: it does not raise an error. It looks like a discount.

## Decision

**A public reservation has no caller-supplied channel. `channel_code` is removed from
`ReservationCreate`.**

1. **The field is unsubmittable, not validated.** `ReservationCreate` no longer declares the
   property, and it already carries `additionalProperties: false`, so a public body naming a channel
   is a **400** at the contract edge. This follows `PartnerReservationCreate`, whose own description
   already states the principle — *"the guard is the absence of the field, not a check"* — and
   AGENTS.md's rule that a fix which *checks* a submitted value keeps the trust boundary in the
   client.

2. **The handler refuses it too, as a second line.** `reserveWithScope` answers 400 when a nil scope
   arrives with a non-nil channel. This exists for the day the contract is edited and must not
   depend on the first line — the same reasoning already written beside the partner overwrite. It is
   *refused*, not silently cleared: clearing would price the caller correctly while telling them
   nothing, and a partner integrator who lost their credential would be quietly under-billed instead
   of told.

3. **A partner's channel comes from its ADR-056 credential and nowhere else.** Unchanged by this
   decision; `reserveWithScope` overwrites any body value with the credential's, and refuses a
   mismatched organizer rather than rewriting it.

4. **New public reservations persist `NULL` channel attribution.** The `channel_code` and
   `reseller_id` columns stay: historical rows keep their attribution, settlement and exchange
   behaviour depend on them, and existing constraints reference them.

## What this does NOT decide

**ADR-024 is not overturned. The channels registry remains a lookup, not a sale constraint.**

Two questions hid behind one field, and only the second one moved:

| question | answer | owner |
|---|---|---|
| Is the code **registered**? | Do not gate on it — a retired channel must keep selling so historical attribution survives, which is why there is no foreign key | ADR-024 / TKT-235 / TKT-241 — **unchanged** |
| Is the caller **entitled** to price on it? | No, unless authenticated | **this ADR** |

Nothing on the sale path reads catalog's `channels` table, before or after this change. An
unregistered or retired code still sells verbatim **for an authenticated partner**, and
`smoke/channel_registry_lookup_test.go`'s partner half is untouched and remains the end-to-end proof.

Its **public** half was narrowed rather than deleted: the scenario it exercised — a public caller
naming a channel — is the capability this ADR removes, so the assertion could only have been
preserved by preserving the defect. That file's header carries the same rationale, as its own
instructions require.

TKT-246's inventory decision is also unchanged.

**The seated seam (TKT-176) is not "unchanged" — it is now unreachable, which is a stronger
statement and was worth getting right** (found in adversarial review; an earlier draft of this ADR
said "unchanged" and that was misleading). A seated claim carries no channel and ignores allocations
entirely, and there is now no route by which a seated request could carry one at all:

- the public contract has no `channel_code` (this ADR);
- `PartnerReservationCreate` is **GA-only** — it declares no `seat_identities`, and
  `commerce/api/openapi.yaml` says why: *"seated pools do not consult channel allocations at all
  (TKT-176 owns that seam), so a seated partner sale would claim an authorization this contract
  cannot deliver."*

So the defect TKT-176 owns is now shut at the contract rather than open and forwarding nothing.
TKT-176 still owns re-opening it deliberately, with allocations that seated claims actually
consult.

## Consequences

- **A public integrator sending `channel_code` breaks, loudly.** Intended. No storefront or
  back-office code sends it (verified: nothing under `web/` references the field outside generated
  types), so no first-party flow is affected.

- **A replay of a pre-existing public reservation that carried a channel answers 409.** Accepted
  deliberately, by the owner, rather than papered over. The reserve idempotency comparison
  (`sameTerms` → `sameChannel`) is exact, so a stored channel no longer matches a channel-less
  retry. A "legacy public replay" exception was designed and **rejected**: its only available
  predicate — `channel_code IS NOT NULL AND reseller_id IS NULL` — is also the shape of a row the
  exchange tests call "legal and routine", so it cannot distinguish what it claims, and it would put
  a permanent conditional on a money path for a transient condition. A 409 **refuses rather than
  mischarges**, and the caller's remedy is a new idempotency key. Pinned by a test asserting the 409
  as accepted behaviour: if that test ever fails, update this ADR rather than deleting it (the
  ADR-021 idiom).

- **The exchange path is closed on the same rule, in two places.** The reseller — not the channel —
  is the authority, so an exchange takes the source's channel only when `src.ResellerID != nil`.
  That guard already governed the inventory forward (`holdExchangeTarget`); this ADR extends it to
  **price resolution** (`repricingChannel`, `exchanges.go`).

  The second half was found in adversarial review and is worth recording, because an earlier draft
  of this ADR argued the opposite and was **wrong**. That draft accepted legacy repricing on the
  grounds that "its own purchase was already priced that way". It is not the same purchase: an
  exchange prices a **different ticket type**, named in the *current* request, against **current**
  rules — which may not have existed when the source was bought. So a pre-ADR-060 public row,
  carrying whatever channel an unauthenticated caller once typed into its body, could still pick the
  price basis for a brand-new sale. Staff authentication on the exchange does not rescue it: a
  perfectly normal staff exchange would honour an attacker-chosen basis.

  **Keeping the stored attribution does not require pricing on it.** Historical rows keep their
  `channel_code` and `reseller_id` untouched — that is what ADR-024 protects — and the resolution
  simply stops trusting an attribution no credential ever vouched for. A source with no reseller
  reprices publicly, which is what its original hold already was.

- **`maxProperties` on `ReservationCreate` returns to 3.** The `not: {required: [quantity,
  seat_identities]}` clause **stays** even though the count alone would now exclude that shape: the
  count matching is a coincidence of today's field list — it was 4 yesterday and the next optional
  property makes it 4 again — while `not` states the XOR directly.

- **Two stale pointers corrected.** `channel_seam_test.go` and `server.go` both attributed this
  residual to *TKT-247*, which is a scanner-device flake and never owned any of it.

## The adversary

Worth naming, per ADR-021's discipline. This closes **an unauthenticated caller choosing their own
price basis** — at the point of sale *and*, after the review pass above, at the point of exchange,
which is where the same choice could otherwise have been laundered through a legacy row into a
brand-new sale.

It does not claim anything about an authenticated partner naming its own channel — that is what the
credential is for — nor about an operator with database access, who can write any `channel_code` to
any row directly. This is an entitlement boundary on a request, not tamper-evidence.

**What is deliberately NOT changed: historical rows.** A pre-ADR-060 public reservation keeps the
channel it was sold under, and the order and settlement records that reference it are untouched.
Their money already moved; rewriting their attribution would change what those sales *were*, which
is the thing ADR-024 exists to protect. The entitlement rule governs what happens NEXT — pricing a
new reservation, or an exchange target — never what already happened.
