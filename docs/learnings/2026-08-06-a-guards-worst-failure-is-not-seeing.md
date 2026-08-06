# A guard's worst failure is not seeing

**TKT-222, TKT-223 (PRs #180, #181) — 2026-08-06**

## What happened

An invariant test scans commerce's production Go source and fails if any
`UPDATE orders SET … customer_id` exists outside one allowlisted statement. It exists because order
attribution is written once and must survive completion and recovery untouched.

Two adversarial passes found two different defects in it, and only one of them was the kind people
look for.

**The one everyone expects — it permits too much.** The predicate check was handed the whole *file*
rather than the matched statement, so a statement with no predicates passed as long as the words
appeared somewhere: in a comment, or in a different function.

**The one that matters more — it cannot see.**

```
UPDATE ONLY orders   SET customer_id = …
UPDATE public.orders SET customer_id = …
```

Both are ordinary PostgreSQL. Neither matched the pattern at all. A second, real attribution writer
in either form would have been **invisible** — and the "exactly one allowlisted statement" count
would still have been satisfied by the legitimate one, so nothing anywhere would have complained.

The same shape had already appeared a ticket earlier: an ADR-019 scan-scope assertion searched a
query plan for `Index Cond` and `customer_id = $1` as *independent* substrings, so an `Index Cond`
from an unrelated join satisfied it while the orders scan filtered after reading everything.

## Why "cannot see" is worse than "permits too much"

- A guard that **permits too much** fails loudly the first time someone reads it: the bad statement is
  right there in the diff, allowlisted, with a name.
- A guard that **cannot see** fails silently forever. Nothing is allowlisted, no count moves, no test
  goes red. The property is simply not being checked any more, and the test's continued green is the
  evidence people cite for believing it is.

Regex-over-source guards are especially prone to this because they are written against *the code that
exists today*. Every accepted alternative spelling — `ONLY`, a schema qualifier, an alias, a
different case, a line break, a comment between tokens — is a hole that opens the day someone writes
it, which is exactly the day the guard was supposed to speak up.

## What to do instead

**For any recogniser, write the test that proves it RECOGNISES before the test that proves it
JUDGES.** A table of the statement shapes that exist plus the ones a future author would plausibly
write, each of which the pattern must match:

```go
func TestTheScannerSeesTheShapesThatExist(t *testing.T) {
    for _, sql := range []string{
        "UPDATE orders SET …",
        "UPDATE orders o SET …",
        "UPDATE orders AS o SET …",
        "UPDATE ONLY orders SET …",
        "UPDATE public.orders SET …",
        "update orders set …",          // case
        "UPDATE orders\n\tSET …",       // line break
    } { /* assert the pattern matches */ }
}
```

That test is what caught a later, unrelated tightening of the same regex on its first run — the
statement-capture change broke matching for fixtures with no trailing terminator, and the
recognition test said so immediately.

**And state the boundary.** A source scan cannot defeat an author trying to defeat it: predicates
hidden in a dollar-quoted string or a subquery will satisfy any substring check. Say which adversary
the guard is for — here, *an honest omission by someone who does not know it exists* — rather than
letting readers assume the stronger claim. Catalog's public-read invalidation guard already draws
that line about itself; copy the sentence, not just the technique.

## The general form

Every detector has two failure modes and only one of them is visible:

| | Symptom | Found by |
|---|---|---|
| Judges wrongly | a bad thing is allowlisted, in the diff | reading the diff |
| **Does not detect** | **nothing, ever** | **a test that asserts detection** |

Related: [a hand-maintained inventory cannot detect its own drift](2026-08-05-a-hand-maintained-inventory-cannot-detect-its-own-drift.md)
(there the *list* was hand-maintained; here the *recogniser* was),
[three ways one test could not fail](2026-08-06-three-ways-one-test-could-not-fail.md).
