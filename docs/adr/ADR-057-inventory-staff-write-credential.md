# ADR-057: The back office edits channel allocations with its own inventory credential

Date: 2026-08-14

## Status

Accepted (TKT-244; decision taken under the owner-waived gates of that run, recorded on the ticket).
Fourth deliverable of the TKT-17 epic. Amends nothing; **ADR-043 draws the line for where a guard
lives, and this does not move it** — the guard stays exactly where it was, an inline check on
inventory's `/internal/` surface, and two routes gain a second accepted credential.

Read alongside **ADR-053**, which answered the same-shaped question for catalog three days earlier
and reached the *opposite* conclusion. The difference is the whole content of this ADR.

## Context

Channel allocations shipped in TKT-78 (ADR-024): `PUT /internal/slots/{id}/channel-allocations`
atomically replaces a slot's whole allocation set under the pool lock. **There has never been a UI**,
and there could not be one, because the back office cannot reach the endpoint.

TKT-246 raised the stakes. It made `channel_allocations.sold_by` load-bearing — inventory now refuses
a claim whose seller does not match, judged under the pool row lock — and inherited a deploy
prerequisite from TKT-240's analysis: **a channel with no active allocation is refused outright**. So
every channel in use needs an allocation configured before that path is safe, and no operator has any
way to configure one.

The back office **does not hold `INTERNAL_SERVICE_TOKEN`** and is deliberately denied it:

> that one value opens every service's internal surface, and this is a public-facing SSR process
> — `compose.yaml`

That refusal is enforced by a test (`web/backoffice/test/api.test.ts`: the client must never send
`X-Internal-Token`) and is the trap TKT-190 walked into and plan-review caught.

Inventory has **no staff-write credential at all** — only the shared internal token. Catalog's
equivalent exists because TKT-191 created it; commerce's because TKT-194 did. Inventory's does not
exist, which is why this was carved out of TKT-236 as a ticket of its own rather than settled while
building a form.

The editor needs **two** operations, not one: `PUT …/channel-allocations` to save, and
`GET /internal/slots/{id}/availability` to read, because showing each channel's **current
consumption** is a condition of success and that staff read is the only thing that reports it. Both
sit behind `internalOnly`.

## Possible Solutions

- **Give the back office `INTERNAL_SERVICE_TOKEN`.**
    - Pros: no new code.
    - Cons: hands a public-facing SSR process every service's internal surface — commerce's refunds,
      inventory's operational holds and capacity adjustments. Explicitly refused in compose and
      pinned by a test.
- **Reuse `CATALOG_STAFF_WRITE_TOKEN`, following ADR-053.**
    - Pros: no new secret to generate or rotate; the smallest diff; consistent with the decision
      taken for the operator channel read.
    - Cons: **ADR-053's reasoning does not transfer, and this is the ticket's central finding.** That
      decision rested on the catalog token *already holding* create and update power over the very
      channels it was then allowed to list — the allowance added bulk enumeration (amplification) but
      **no new capability class**. The back office holds **nothing** for inventory. Accepting a
      catalog credential here would grant power across a service boundary and turn a catalog-token
      leak into an inventory-write compromise: capacity configuration, and the seller bindings
      TKT-246 made load-bearing. Same shape, different answer, because the premise is different.
- **Accept a new credential across inventory's whole `/internal/` surface.**
    - Pros: one wiring decision; consistent with how a service-wide credential usually reads.
    - Cons: grants a public-facing SSR process every inventory operational capability — holds and
      their transitions, operational holds, group reservations and draw-down, capacity adjustments,
      the availability kill-switch — for a screen that needs two reads. Narrowing later is a security
      migration; widening later is additive. The reversible direction wins.
- **A new credential accepted on exactly the two operations the editor needs** (chosen).
    - Pros: the smallest grant that makes the screen work; a compromise of the SSR process reaches
      allocation configuration and a staff availability read, and nothing else.
    - Cons: one more secret to generate, wire and rotate; the allowlist is in code rather than in the
      contract (which ADR-043 already settled for internal routes); and it is only *narrow* if
      something proves it stays narrow — see § What keeps this narrow.

## Decision

**We add `INVENTORY_STAFF_WRITE_TOKEN`, presented as `X-Inventory-Staff-Write-Token`, accepted on
exactly two operations and nothing else:**

- `GET /internal/slots/{id}/availability`
- `PUT /internal/slots/{id}/channel-allocations`

The back office reaches them **directly, in-network** (`INVENTORY_URL`), because the gateway
edge-denies every `/api/<svc>/internal/` route by construction (ADR-002) — the same shape as the
staff refund reaching commerce (TKT-194) and the operator channel read reaching catalog (ADR-053).

Specifics:

- **Method+path exact, not a prefix.** The other thirteen internal routes keep the plain
  `internalOnly` wrapper.
- **Additional, not replacing.** `X-Internal-Token` still works on both routes; five smoke drivers
  and every service-to-service caller present it.
- **Fail-closed when unconfigured**, and constant-time compared (`httpx.CredentialMatches`), like
  every other credential here.
- **Inventory refuses to start** when the value is absent or equal to `INTERNAL_SERVICE_TOKEN`,
  before any dependency is contacted. The back office refuses to start when any two of the three
  credentials it holds are equal. Neither error echoes a value.
- **Not in the contract.** ADR-043's rule: declared `security:` guards a service's public contract,
  an inline check guards its internal surface. This adds a *credential*, not a new *placement*.

## What keeps this narrow

An allowance is only narrow if something refuses what is next to it, and **ADR-053 recorded that a
route-level test cannot prove this**: there, widening the allowance to a whole prefix killed **no
test**, because the hand-mounted handlers carried their own checks — the guard stopped being narrow
while every route-level test stayed green.

So narrowness is asserted by a **router walk** (`services/inventory/internal/api/staff_credential_test.go`),
adapted from commerce's equivalent (TKT-194): it enumerates every `/internal/` route the real chi
router serves, probes each with the staff credential, and fails on **both** an uncovered route and a
stale entry, with a floor assertion that catches a broken walk. Two traps it must keep respecting:

- Each probe fixture is **otherwise valid** — real UUIDs, required body, `Idempotency-Key` — because
  the request validator runs *before* the handler and answers 400. A fixture the validator rejects is
  refused without the credential check ever running, and would pass just as happily with the
  credential wired to every route.
- The expected refusal is **401**, inventory's answer on its internal surface. Commerce answers 404
  for reasons ADR-043 states; copying that expectation here would assert nothing.

## What this costs

**Name the adversary (ADR-021).** Inventory authenticates the calling **process**, not the staff
member behind it. This credential limits the blast radius of a compromised or buggy back office to
two inventory operations. It is **not** tamper-evidence: anyone with inventory database access can
write `channel_allocations` directly, and no credential in inventory changes that.

What the grant actually adds, stated plainly:

- A holder can **read any slot's staff availability** for a caller-supplied organizer — capacity, the
  derived counters, and per-channel consumption. This is operator configuration, not buyer data, but
  it is more than the public read exposes.
- A holder can **replace any slot's allocation set** for a caller-supplied organizer. That includes
  clearing a `sold_by` binding, which under TKT-246 returns a reseller's stock to the public pool.
  **This is the sharpest edge of the grant** and the reason the editor round-trips every field it
  does not render.
- The organizer is **caller-supplied on both**, exactly as it is for catalog (ADR-053) and for the
  same reason: inventory has no organizer identity it can verify independently of the request. So a
  stolen credential is not confined to one tenant. That gap is **TKT-245's**, it is not new here, and
  this ADR does not claim to close it.

Everything else rests on the same assumption ADR-053 named: **the back office is not compromised.**

## Consequences

- **Positive:**
    - Channel allocations become operable by a human. TKT-246's deploy prerequisite — every channel
      in use needs an allocation — stops being a manual database task.
    - The editor round-trips `sold_by`, `requires_code` and the sales window, so a cap edit cannot
      silently unbind a reseller. The staff availability read now reports those three fields, which
      is what makes the round trip possible at all.
    - Two refusals that were indistinguishable bare 409s now carry machine-readable codes, and the
      cap-below-consumption one names its channel — so the message can land on the field the operator
      must fix. The client deliberately does **not** re-derive which channel failed: consumption is
      live and moves between the read and the write, so a local guess can name the wrong row.
- **Negative:**
    - A fourth credential class in the system (ADR-042 staff sessions, ADR-043 internal tokens,
      ADR-049 customer sessions, ADR-056 partner credentials, and now this). Each is justified
      separately; the count itself is a cost.
    - One more secret to generate, distribute and rotate, and one more pair the back office must
      compare at startup.
    - The allowlist lives in code, so it is only as narrow as the router-walk test keeps it.
- **Revisit when:** TKT-245 lands (an organizer identity inventory can verify would let this ADR say
  something much stronger), or a second consumer needs one of these two operations — two callers on a
  route-specific allowance is the point at which the allowance should become a mechanism.

## References

- TKT-244 (this ticket), TKT-17 (epic), TKT-236 (which carved this out), TKT-246 (`sold_by`), TKT-245 (the open tenancy gap)
- [ADR-024](./ADR-024-channel-allocations.md) — allocation semantics: full-set atomic replace under the pool lock
- [ADR-043](./ADR-043-where-a-service-auth-guard-lives.md) — contract operation vs internal route; where a guard lives
- [ADR-053](./ADR-053-channel-operator-read-credential.md) — the same question for catalog, answered the other way
- [ADR-002](./ADR-002-services-from-day-one.md) — the gateway edge-denies `/internal/`
- [ADR-021](./ADR-021-ticket-lifecycle-trail-integrity.md) — name the adversary before claiming a guarantee
- [ADR-042](./ADR-042-staff-identity-and-backoffice-sessions.md), [ADR-049](./ADR-049-customer-identity-and-storefront-sessions.md), [ADR-056](./ADR-056-partner-credential-identity.md) — the other identity classes
