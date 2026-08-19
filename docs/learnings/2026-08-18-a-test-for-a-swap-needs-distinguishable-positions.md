# A test for a SWAP needs the two swapped things distinguishable in every position

**TKT-259.** Three successive versions of one assertion could not detect the defect the assertion was
named for. The third review pass caught it by executing the mutation and watching the test stay green.

## The mechanism under test

`ReleaseExchangeReversalClaim` decides whether an obligation made progress:

```sql
progressed :=
     (tickets_exchanged_at  IS NOT NULL AND NOT $7)   -- $7 = switchedAtClaim
  OR (capacity_returned_at  IS NOT NULL AND NOT $8)   -- $8 = capacityAtClaim
```

Crossing `$7` and `$8` **type-checks, runs, and reports progress backwards**. The source comment warns
about it. A test was written specifically to pin it.

## Three fixtures, two of which prove nothing

| # | Row state | Observations passed | Correct | Crossed | Detects? |
|---|---|---|---|---|---|
| 1 | switched **set**, capacity **null** | `false, false` | true | true | **no** — both arms read the same flag |
| 2 | switched **set**, capacity **set** | `true, false` | true | true | **no** — each arm rescues the other |
| 3 | switched **set**, capacity **null** | `true, false` | **false** | **true** | **yes** |

Version 1 was the original. Version 2 was written *while fixing* version 1, and looked strictly
better — it introduced an asymmetry in the observations. It was still undetectable, for a different
reason: with both columns set, either arm alone can carry the `OR`, so swapping which flag guards
which column changes nothing.

Only version 3 discriminates: exactly **one** column set, with the observations asymmetric **and
matched to that column**. Correct pairing asks "did the switch move since I saw it?", answers no, and
spends the budget. Crossed pairing tests the switch against the capacity's flag and hands a failing
row a fresh budget every pass — so it can never park.

## The rule

**To test that two things are not swapped, make them distinguishable in every position the assertion
touches.** Concretely, for a predicate over pairs `(column_a, flag_a)` and `(column_b, flag_b)`:

- the two columns must differ (one set, one null) — otherwise either arm can satisfy the `OR`;
- the two flags must differ — otherwise both arms evaluate identically;
- and the flag that differs must be the one guarding the column that differs.

Get any of those wrong and the test asserts that the query *runs*, not that its wiring is right.

## Why the usual defences missed it

- **Mutation testing did catch the crossing** — three *other* tests went red. But the test named for
  the pairing stayed green, so a reader would conclude the pairing was covered. A mutation caught by
  a neighbouring test proves the mechanism is live, not that your test caught it
  ([a fixture that seeds two mechanisms](2026-08-16-a-fixture-that-seeds-two-mechanisms.md)).
- **The comment was right.** The source explained the trap correctly; the test beneath it did not
  implement the explanation. Prose about a trap is not a guard against it.

This is the [green test that cannot reach the failing
state](2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md) rule, specialised: for a
*swap*, "reachable" means the two candidates must produce **different** answers, which is a stronger
condition than the fixture merely reaching the code.
