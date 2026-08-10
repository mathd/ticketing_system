# A green test that cannot reach the failing state

**2026-08-10 — TKT-236, TKT-238 (epic TKT-17)**

`AGENTS.md` already says: *before debugging a red test, ask what its fixture can distinguish*. This
is the other half, and it is the more dangerous one, because nothing prompts you to ask it.

**Before trusting a green test, ask whether it can reach the state that would fail.**

A red test announces itself. A green test that is *structurally incapable* of failing looks exactly
like a green test that proves something — same output, same coverage line, same confidence. Three
defects in one epic hid behind one, and each was found by an adversarial reviewer reading the code,
never by the suite.

## The three instances

**1. Wrong tier — the fake enforced what the SQL did not.** (TKT-236)
A cross-tenant write test passed against the in-memory store while the SQL predicate
(`AND organizer_id = $2`) was deleted. The fake scopes by organizer *in Go*, so the assertion was
true about the fake and silent about the query that ships. The test was at the wrong tier for the
mechanism it claimed to check.

**2. The fixture repaired the state before testing it.** (TKT-236)
A browser spec for "renaming a disabled channel must not re-enable it" re-enabled the row during
setup, then renamed it. The defect — a hidden `value=""` reading as `true` — needs the row to be
*disabled at rename time*. The spec's own setup made the bug unreachable.

**3. The failing state was unreachable by construction.** (TKT-238)
Refusal precedence broke only with *pool full, channel cap free, window shut*. Filling the pool
through the public channel leaves the presale's reservation intact **by construction**; the state
requires an operational hold, which ADR-024 defines as pool-only and unchanneled. The obvious
fixture cannot build it. My first probe wrote exactly that fixture and showed both cases passing —
the bug was real and running at the time.

## What they share

None was a forgotten case. In all three the case was *named*, a test for it *existed*, and the test
was *green*. The gap was between the state the test named and the state it actually constructed.

## The check

When a test is the evidence for a claim, ask:

1. **Would it go red if the mechanism it names were deleted?** Delete it and run — a mutation check
   on the specific line, not the feature. (Instance 1 dies here.)
2. **Does the setup destroy the precondition?** Read the fixture forwards and ask what state the
   assertion actually runs against. (Instance 2.)
3. **Can this fixture even build the failing state?** If the state needs a mechanism the fixture
   doesn't use, no amount of tuning gets there — the fixture is the wrong shape. (Instance 3.)

Question 3 is the one with no natural prompt, and the only defence is asking it deliberately.

## Related

- [a fixture too small cannot show the negative](2026-08-03-a-fixture-too-small-cannot-show-the-negative.md)
  — the same family: there the fixture lacked *values*, here it lacks *reachable states*.
- [two correct fixes can compose into a new defect](2026-08-03-two-correct-fixes-can-compose-into-a-new-defect.md)
