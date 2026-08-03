# A fixture too small cannot show the negative

**TKT-172, TKT-182 (×2), TKT-183 (PRs #146, #152, #153) — 2026-08-03**

## What happened

Four times in one epic a test failed while the code under test was correct, because the fixture
could not distinguish the rule from its negation.

- **TKT-183 smoke.** An orphan-prevention row of **three** seats. Three is the minimum in which a
  selection can strand a seat — and too small to show the negative, because *every* pair strands
  the third. The "this selection is allowed" assertion was unprovable there. Four seats fixed it.
- **TKT-182, twice.** A six-seat row was too small for the scenario; then an assertion expected an
  exact singleton where the rule legitimately reported two seats. The rule was right both times.
- **TKT-172.** An `EXPLAIN` index-proof seeded **two** pools. With `n_distinct = 2`, a generic plan
  estimates `pool_id = $1` at half the table and a sequential scan is the *correct* plan. The test
  failed against a correct index. Two hundred distinct pools fixed it.

## Why it is expensive

It looks exactly like a bug in the code. The debugging starts in the implementation and only
reaches the fixture after the implementation has been re-read and found correct.

## What to do

Before debugging a red test, ask **what the fixture can distinguish**. Specifically:

- For a rule that refuses some inputs, the fixture must admit at least one input the rule
  **allows**, or a passing "allowed" case is impossible.
- For a planner assertion, the scoping column needs **many distinct values** — a large
  other-bucket is not the same thing.
- Say in the test comment why the fixture is the size it is. `// four seats: three is too small to
  show the negative` prevents the next person shrinking it.
