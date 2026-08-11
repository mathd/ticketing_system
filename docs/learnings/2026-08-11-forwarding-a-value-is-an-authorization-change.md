# Making a value load-bearing is an authorization change, not plumbing

**Date:** 2026-08-11 · **Ticket:** TKT-240 (epic TKT-17, US-CH6) · **Follow-up:** TKT-246

## The one-sentence rule

Before making a request field load-bearing, ask **which other callers can supply it, and are they
authenticated** — because the guard you need is at the place that consumes the value, not the path
you happened to be editing.

## What happened

Commerce sent `channel_code` to catalog for fee resolution and deliberately not to inventory, so a
reseller-channel sale took reseller fees while consuming public stock. The ticket, the plan, and my
own adversarial plan-review all described the fix the same way: forward the field on the GA hold.
One key in one map.

It shipped, with a passing gate and a test asserting the exact wire key — including a mutation check
proving that sending the wrong key (`channel_code` instead of inventory's `channel`) turned the test
red. All of that was correct and none of it was the problem.

`POST /reservations` is **unauthenticated** and takes `channel_code` from the request body. Once the
field reached inventory, any caller could name a reseller's channel and consume its allocation with
no credential. Executed against the code, not argued:

```
status=409 inventory-body=map[channel:reseller-acme ...]
BYPASS CONFIRMED: unauthenticated caller sent channel=reseller-acme to inventory
```

The same ticket had just built a carefully scoped credential class whose central claim was that a
partner is confined to its own organizer and channel. The forward made that claim false through the
front door.

## Why every layer missed it

Each layer reasoned about **the path being changed**. The seam lived on the reserve path; the plan
described the reserve path; the test drove the reserve path; the mutation check mutated the reserve
path. Nobody asked what *else* reaches the code that now consults the value.

Two more instances of the same blind spot were found in the same review pass: the persisted-
reservation **replay** and the **exchange target** hold both build their own inventory bodies and
neither carried the channel. One of four GA hold paths was closed while the PR said "the seam is
closed".

## The general shape

A value becomes load-bearing when something starts *deciding* on it — capacity, permission, price,
routing. At that moment every producer of that value becomes part of the trust boundary, whether or
not it was touched by the change. So:

1. **Enumerate the producers**, not the paths you edited. `git grep` the field name, including
   struct tags and JSON keys.
2. **Ask which of them are authenticated.** An unauthenticated producer of an authority-bearing
   value is a bypass, no matter how well the authenticated one is written.
3. **Put the guard where the decision is.** Inventory owns stock, so the rule "who may sell this
   channel" belongs on the allocation row and must be judged under the pool lock — the shape ADR-055
   already used for `requires_code`. A check in commerce would bind only callers who go through
   commerce.

## What it cost, and what caught it

A full build-review-revert cycle: the closure was implemented, gated green, pushed, and reverted.
The **cross-model adversarial review** caught it — the implementer's own tests could not, because
they encoded the implementer's model of the change.

Then the fix created a second defect: reverting the forward left the partner *write* exposed, so a
partner hold consumed public stock while the handler, the ADR and the smoke test all still claimed
confinement. That one was caught by the **second** review pass, run because the fix diff is code
nobody has reviewed. Both findings were mine; neither was findable from inside my own reasoning.

## Appliable

- `shaping.md`, § the shaping pass: when a ticket makes a request field load-bearing, add the
  producer enumeration above to the DoR's `approach` item.
- Reviewing such a change: grep for every producer before reading the diff.
- Writing such a change: the test that proves the value reaches the consumer is necessary and not
  sufficient. The test that proves an *unauthorized* producer is refused is the one that matters.
