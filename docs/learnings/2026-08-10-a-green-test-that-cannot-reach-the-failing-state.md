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

**4. An earlier refusal short-circuited the predicate under test.** (TKT-251)
A guard with *two* independent predicates — both row lookups of an attach operation scoped to the
caller's organizer — got one test, whose fixture gave the attacker a victim-owned parent *and* a
victim-owned child. The **parent** lookup refuses first, so the child predicate never executes:
deleting `AND organizer_id=$2` from the child left the suite green. The unscoped child would then
answer `ErrOrganizerMismatch` for a real id and `ErrNotFound` for an unknown one — disclosing
existence while the write stayed refused, which was the exact property the test was written to
prove. Reaching predicate 2 requires a fixture that *passes* predicate 1: attacker owns the parent,
victim owns the child.

**5. The expected value was a constant the code could also reach by accident.** (TKT-251)
A test asserting that all 13 handlers pass the *verified* organizer to the store drove every case
with the test env's default tenant — which is the package-global `orgID`. Hard-coding that same
constant inside a handler kept it green. It proved the argument was not `uuid.Nil`; it could not
prove the value came from the request's assertion. The fix is two runs under two freshly generated
organizers: one run cannot distinguish "read it from the assertion" from "happens to equal this
fixture's tenant".

## What they share

None was a forgotten case. In all five the case was *named*, a test for it *existed*, and the test
was *green*. The gap was between the state the test named and the state it actually constructed.

Instances 4 and 5 add an edge worth stating on its own: **the test most likely to be unfalsifiable
is the one whose name matches the acceptance criterion.** Both were written specifically to prove a
security boundary, in a ticket whose shaping had already predicted the underlying trap, and both
were caught only by cross-model review.

## The check

When a test is the evidence for a claim, ask:

1. **Would it go red if the mechanism it names were deleted?** Delete it and run — a mutation check
   on the specific line, not the feature. (Instance 1 dies here.)
2. **Does the setup destroy the precondition?** Read the fixture forwards and ask what state the
   assertion actually runs against. (Instance 2.)
3. **Can this fixture even build the failing state?** If the state needs a mechanism the fixture
   doesn't use, no amount of tuning gets there — the fixture is the wrong shape. (Instance 3.)
4. **How many independent predicates does the guard have, and does something refuse before the one
   I am testing?** A guard with N predicates needs N cases, each *passing* the earlier ones, and
   each predicate mutated separately. One green case proves one predicate. (Instance 4.)
5. **Could the expected value arrive by coincidence?** If the value under test is a constant the
   code could plausibly hard-code — a default tenant, a fixture id, a zero value — vary it and run
   twice. A single run cannot separate "computed correctly" from "equal by accident". (Instance 5.)

Question 3 is the one with no natural prompt, and the only defence is asking it deliberately.
Questions 4 and 5 have a cheap mechanical trigger: *count the predicates*, and *ask whether the
expected value is distinct from every constant in scope*.

## Related

- [a fixture too small cannot show the negative](2026-08-03-a-fixture-too-small-cannot-show-the-negative.md)
  — the same family: there the fixture lacked *values*, here it lacks *reachable states*.
- [two correct fixes can compose into a new defect](2026-08-03-two-correct-fixes-can-compose-into-a-new-defect.md)
