# A green mutation can mean your generator is blind, not that the guard is redundant

**2026-08-31 — TKT-305**

Two notes already cover the green-mutation result. [A green test that cannot reach the failing
state](2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md) says the test may not see the
mechanism. [The mechanism was inert](2026-08-23-the-mechanism-was-inert-not-the-test.md) says the
mechanism may do nothing, and [inert for two different
reasons](2026-08-30-inert-for-two-different-reasons.md) splits that by *why* it is unreachable.

This is the third reading, and it is the one that ships a defect. **The mechanism is live, the test
is honest, and the mutation is blind — because the input class that separates the two guards was
never in the generator.**

**Before deleting a guard because a mutation stayed green, ask what input class the guard you are
deleting accepts that the guard you are keeping does not — and check your table contains it.**

## The instance

Partner availability decoded inventory's answer into `struct{ Available *int }`, discarded the
`json.Unmarshal` error, and defaulted a nil pointer to `0`. An inventory outage therefore read as a
sellout, and a reseller polling the endpoint stopped selling a show with seats.

The fix wrote two guards — refuse an unparseable body, then refuse one missing the field — and then
removed the first, on this argument:

> `encoding/json` never leaves a pointer field populated on a body it rejected, so every input that
> errors also arrives nil. The nil guard subsumes the decode guard.

That was not a guess. It was probed, and it was mutated: remove the decode check, run the tests,
watch them stay green. The conclusion drawn was "redundant guard, delete it".

**The probe contained only syntax errors.** `<html>…`, a truncated object, a bare `[]`. All three do
arrive nil, so the argument held for every input anyone had thought of.

A **type** error does not. `encoding/json` populates the field and reports the error afterwards:

| body | errors? | `Available` | shipped as |
| --- | --- | --- | --- |
| `<html>…` | yes | `nil` | 502 ✓ |
| `{"slot_id":"x","avail` | yes | `nil` | 502 ✓ |
| `{"available":"bad"}` | yes | `&0` | **200 `available: 0`** |
| `{"available":1.5}` | yes | `&0` | **200 `available: 0`** |
| `{"available":7,"available":"bad"}` | yes | `&7` | **200 `available: 7`** |

The first two rows are the whole test table. Rows three to five are the defect the ticket exists to
remove, still shipping — and the last is *worse* than the original bug, because a fabricated non-zero
reads as authoritative rather than as a sellout.

## Why the mutation could not catch it

Mutation testing asks *"if I break the mechanism, does a test notice?"* It is only ever as good as the
inputs the tests supply. Deleting the decode guard changes the answer **only** for inputs where json
errors *and* populates — precisely the class absent from the table. Every input that was there still
routed through the nil guard, still produced 502, still passed.

So the mutation was not evidence of redundancy. It was evidence that the table did not distinguish
the two guards, which is a fact about the table.

The tell is available *before* running anything: two guards are redundant only if they accept the
same input class. Write down what each accepts. `json.Unmarshal(...) != nil` accepts "the bytes are
not a valid encoding of this shape"; `Available == nil` accepts "the field is absent **or** the
decode gave up before setting it". Those are different sets, and the difference is exactly the type
error. Nothing had to be run to see it.

## The general rule

For a property of the form *"malformed input must be refused"*, enumerate malformation **classes**,
not examples:

- **syntax** — the bytes are not JSON at all
- **type** — valid JSON, wrong type for the field
- **absence** — valid, well-typed, field missing (and its cousins: an explicit `null`, an error
  envelope, the field nested a level deeper, `{}`, the bare literal `null`)
- **identity** — a perfectly valid answer *about something else* (here: another slot's availability,
  republished under the requested id)
- **range** — valid, well-typed, present, and impossible (here: a negative count, which had been
  *clamped to zero* and so reported as a sellout — the same substitution the ticket was closing)

The shipped handler needs a guard for each, and the table needs a row for each. Identity and range
were both found by an adversarial pass after the syntax/type/absence set was already green.

This is the same family as [a harness that cannot catch what it
hunts](2026-08-17-a-harness-that-cannot-catch-what-it-hunts.md) — a brute-force sweep that ran 576
arrangements while never placing the value that leaks in the position that leaks. There the generator
was blind to a *position*; here to an *error class*. Both look like thorough coverage from inside.

## The corollary about comments

The second review pass on this ticket found three defects, and every one was a **sentence written
while fixing the first pass** — not broken code:

- a comment promising a failed queue write was safe because "the next sync picks it up", when the
  recovery query filters on a state that write is what sets;
- a comment justifying the negative clamp by a `minimum: 0` that lives on a *different service's*
  schema;
- a test fixture whose determinism was asserted and not measured — it failed one run in five.

Nothing gates prose. Types, tests, mutations and the gate are all downstream of the code; a comment
can say anything. Fix-momentum is where the confident-and-false ones get written, so re-read every
sentence added during a fix round and ask **which function did I actually open to verify this?**
