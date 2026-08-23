# The mechanism was inert, not the test

**2026-08-23 — TKT-162**

`AGENTS.md` already carries the rule: *delete the mechanism and re-run — if it stays green, the test
is about something else.* Every existing instance reads the result the same way: the mechanism is
real, the test is at fault, go fix the test.

This is the case where that inference is **wrong**. The mutation stayed green because there was
nothing to catch: the mechanism could not change any result, and the test was as good as a test of
inert code can be.

**When deleting a mechanism changes nothing, ask which of two things you have learned — that the
test cannot see the mechanism, or that the mechanism does not do anything. Only the second is
fixed by deleting code.**

## The instance

The voided-ticket feed pages newest-first with a keyset cursor: `(occurred_at, id) < ($2, $3)`,
`ORDER BY occurred_at DESC, id DESC`. An adversarial pass found that a ticket voided *during* a page
walk is newer than every remaining cursor, so it is excluded from the rest of the walk while
`next_cursor: null` still reports the walk complete. Real, reproduced, and for a revocation feed it
is the central failure mode.

The fix looked obvious: a **high-water mark**. Capture the walk's upper edge on page one, carry it
in the cursor, add `AND e.occurred_at <= $ceiling`. That is a standard answer to a standard problem,
it reads as deliberate, and it shipped with a test named
`TestVoidedFeedWalkIsASnapshotAndDoesNotLoseMidWalkVoids`.

The next review pass said the test "does not prove the late-void behavior it names". A mutation
confirmed it: neuter the ceiling predicate, everything stays green.

The reflex at that point — and the one the existing rule invites — is *the fixture is too weak,
build a harder one*. I rewrote the test twice. Both rewrites also passed with the predicate deleted.

The third attempt was to work out **why**, on paper:

- the walk descends, so the cursor decreases monotonically;
- the ceiling is fixed at page one's newest row;
- therefore `cursor <= ceiling` always;
- the keyset excludes everything `>= cursor`, the ceiling excludes everything `> ceiling`;
- **the keyset predicate is strictly stronger, so the ceiling can never change a result.**

The ceiling was dead code. No fixture could catch its removal, because removing it does nothing.
Three attempts at a better test were three attempts to observe something that was not there.

## Why this matters more than a wasted afternoon

A dead mechanism with a green test beside it is **worse than no mechanism**. It reads as a
guarantee. The next person sees `Ceiling`, sees a test named for a snapshot, and reasons about the
feed as though walks were consistent — which they are not, and the underlying race the ceiling was
supposed to fix was still wide open the whole time.

It also survived a full review pass. The first reviewer read the ceiling as a plausible fix for the
gap it named, because it is one *in general* — just not against a strictly descending keyset, where
the ordering predicate already dominates it.

## What to do instead

**When a mutation of a mechanism leaves everything green, before touching the test, ask whether the
mechanism is reachable at all.** Write down what it excludes, write down what the surrounding
predicates already exclude, and check whether the first is a subset of the second. That is a
five-minute argument on paper and it is the difference between deleting ten lines and writing a
third fixture that cannot work.

Two tells that you are in this case rather than the ordinary one:

- **Successive fixture rewrites all fail to catch it.** One weak fixture is normal; three is
  evidence about the code, not about your test-writing.
- **You cannot state, in one sentence, an input for which the mechanism changes the output.** If
  the sentence keeps coming out as "well, if the cursor were above the ceiling…" and the cursor
  cannot be above the ceiling, the mechanism is inert.

And when it is inert, **delete it**. Do not keep it with a comment explaining that it is
belt-and-braces: the comment does not survive, and the mechanism will be read as load-bearing by
whoever next changes the query. TKT-162 deleted the ceiling and replaced the test with one that
asserts only what is true — the walk terminates, does not lose rows that existed when it began,
excludes voids created during it, and those deferred voids appear in the next walk.

The underlying race was then recorded as an **accepted limitation** in ADR-066 §4b, with what a real
fix would cost (commit ordering on an append-only, hash-chained, signed table) and why the scanner's
next pull is an adequate mitigation. That is the honest end state: a named gap beats a mechanism
that pretends to close it.
