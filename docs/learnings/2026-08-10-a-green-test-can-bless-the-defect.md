# A green test can assert the defect as correct

**2026-08-10 — TKT-239 (epic TKT-17)**

Sibling of [a green test that cannot reach the failing state](2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md),
written the same day, and the more dangerous of the two — because every tool we use to check test
quality reports success.

**A test that encodes the wrong invariant is green, stays green, and kills every mutant.**

## What happened

TKT-239 caps a presale code at N redemptions, counted by summing consumed quantity over claims that
cite the code. A group reservation can be *drawn down* into child claims.

I decided the children should NOT cite the code, reasoning that the count sums a live source *and*
its live children, so citing both would double-count and exhaust a cap at half value. I wrote it
down as a plan decision, recorded it in an ADR, and wrote a test:

```go
// after drawing 4 of a 10-unit reservation
if after != 6 { t.Fatalf("derived usage is %d, want 6", after) }
```

Green. It stayed green through the whole ticket.

The reasoning was false. A draw-down **decrements the source by exactly the drawn quantity** (or
releases it whole), so source + children always sums to the original — citing both is conservative,
not duplicative. Without the citation the units stay consumed while the count forgets them:

```
after placing 10:      usage=10
after drawing ALL 10:  usage=0     <- units still consumed
second redemption:     GRANTED     <- 20 units from a cap of 10
```

## Why nothing caught it

- **The test was green** — it asserted `6`, and the defective code produced `6`.
- **Mutation testing could not help.** A mutant flips the mechanism; the assertion was written to
  match the mechanism. Mutant and assertion agree, the mutant dies, the report says covered.
- **The ADR made it worse**, not better: it recorded confident reasoning, so re-reading my own notes
  re-confirmed the error.

Test-quality tooling measures whether a test *discriminates*. It cannot measure whether the thing it
discriminates *is the thing you want*. That gap is invisible from inside the change.

## What closed it

A cross-model adversarial review, reading the invariant rather than the test. It refused the claim,
and running the sequence settled it in one command.

This is the strongest argument yet for the cross-model rule being a **prerequisite, not an option**:
the entire local toolchain — types, tests, mutants, the gate — is downstream of the author's model of
correctness. Only a reader who does not share that model can falsify it.

## The check

When a test asserts a *number* or a *state* rather than a refusal:

1. **Derive the expected value from the requirement, not from a run.** If you wrote the assertion by
   observing what the code did, you have pinned the behaviour, not the rule.
2. **Say the invariant in one sentence, without naming the implementation.** "A draw-down moves a
   redemption; it never creates or destroys one" survives; "usage is 6 after drawing 4" does not.
3. **Ask what the number would be if the mechanism were absent** — and whether you would notice the
   difference. Here, absent-mechanism gave 6 and correct gave 10; my fixture happened to make the
   wrong answer look arithmetically reasonable.
4. **Prefer an invariant over a value where one exists.** The fixed test asserts usage is unchanged
   across partial *and* full draw-down, and that the cap still binds — three ways of saying the same
   rule, none of which the defective code satisfies.

## Related

- [a green test that cannot reach the failing state](2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md)
  — the fixture cannot build the state. Here it builds it and approves it.
- [a fixture too small cannot show the negative](2026-08-03-a-fixture-too-small-cannot-show-the-negative.md)
