# When a value must not be client-chosen, make it unsubmittable — validating it is the slower way to lose

**Date:** 2026-08-15 · **Ticket:** TKT-244 (epic TKT-17, US-CH2b) · **ADR:** ADR-057 · **Follow-up:** TKT-250

## The one-sentence rule

If a request must not be able to choose a value, **remove the value from what the request can
express** — every fix that instead *checks* the submitted value keeps the trust boundary in the
client, and each check merely moves where it leaks.

## What happened

TKT-244 shipped a back-office screen editing a slot's channel allocations. The endpoint it drives,
`PUT /internal/slots/{id}/channel-allocations`, is a **full-set atomic replace**: it DELETEs every
allocation row and re-INSERTs from what arrives (ADR-024). So the form has to submit rows it does not
edit — and one of the fields riding along is `sold_by`, which TKT-246 made load-bearing three days
earlier. Inventory refuses a claim whose seller does not match, judged under the pool row lock, so
losing that value returns a reseller's bound stock to the public pool.

Three adversarial review passes each found the same defect wearing a different mask, and each of my
fixes was locally reasonable:

1. **Carry the true values in hidden inputs.** Pass 2's objection: *both* the value and the "was it
   edited?" evidence come from the POST body. A crafted request supplies whatever it likes and the
   server writes it verbatim — including `opens_at`/`closes_at`, which the screen never exposes for
   editing at all.
2. **Read the current set server-side and merge from it, keyed on the channel.** Pass 3's objection:
   the channel key *is also client-supplied*. A row naming a code inventory does not hold misses the
   map and falls through to the client's values. Same defect, one indirection further out.
3. **Refuse a row that matches nothing.** Pass 4's objection: a subset check is half a boundary. On a
   full-set replace, **omission is deletion** — an absent `channel.0` parses to zero rows and clears
   every allocation while the page redirects as though it saved.

What finally held was not a better check. It was **deleting the capability**: `AllocationRow` carries
only `{channel, cap, releaseAt}`, the parser ignores the other keys if a request sends them, and the
submitted set must match the current one exactly in both directions. The dangerous fields are not
validated — they are **inexpressible**.

## Why the earlier fixes all felt right

Each one made the *specific* attack in the previous finding impossible, which is exactly what a fix is
supposed to do, and each left the boundary where it was: on data the client controls. The tell is
structural rather than clever — after fixes 1 and 2 the answer to *"can the client influence this
field?"* was still **"yes, but only if it lies in a way I now check for."** That sentence is the
defect. After the final fix the answer is "no, the field is not in the request."

Note also what could **not** save this. Inventory validates the channel, the cap, duplicates, pool
capacity and consumption — and **never constrains `sold_by`**. There was no downstream authority to
fall back on, so the back office was the only place the boundary could exist. Check that before
assuming a service will catch what a UI lets through (ADR-021: name the adversary — here, a
compromised or buggy deputy, not a database writer).

## The test that blessed the defect

Pass 3's finding cited a test **I had written**:

```ts
// A row inventory does not hold is NEW, so the form is the only source it has.
it('takes a new row entirely from the form', () => { … })
```

It was green, it named a real case, and it asserted the defect as the requirement. Mutation testing
cannot catch this class: the mutant flips the mechanism, and the assertion was written to match the
mechanism, so the mutant dies exactly as it should. This is the second worked example in the repo
after [a green test can bless the defect](2026-08-10-a-green-test-can-bless-the-defect.md), and the
first where the blessed value was an **authorization fallback** rather than a number.

Its replacement states the requirement without naming the implementation: *this screen changes
existing allocations; it creates none and deletes none.* Both halves of that sentence are now
enforced, and the second half is what pass 4 found missing.

## What to do

- When a value must not be client-chosen, ask **"can the request express it at all?"** before asking
  "do I validate it correctly?" A field absent from the type and ignored by the parser needs no check.
- On a **full-set replace**, enumerate both directions: a row that should not be there, and a row that
  should be and is not. Omission is a write.
- Before relying on a downstream service to constrain a value, **read its validation** and confirm it
  does. Ours validated five things and not the one that mattered.
- When a review pass refutes your fix rather than the original code, treat the *shape* of the fix as
  suspect, not just its details — three passes here were the same defect at increasing distance.

## What this did not fix

A **stale** full-set save can still overwrite a concurrent edit, `sold_by` included: the page reads
the current set and writes it back without a precondition, so two operators can clobber each other.
That is pre-existing, explicitly accepted in ADR-024 (*"acceptable while allocation editing is
single-operator"*) — a premise this ticket weakened by shipping the first UI — and closing it needs a
revision/compare-and-swap under the pool lock in inventory. **TKT-250.**
