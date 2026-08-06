# Three ways one test could not fail, and "observe it red" caught none of them

**TKT-220 (PR #178) — 2026-08-06**

## What happened

One assertion — *session expiry does not depend on the wall clock* — was written three times. Each
version was observed red before its fix and green after. Each version could not fail against a wrong
implementation, for a different reason.

**Version 1 — the assertion was defeated by a side effect of the code under test.**

```ts
expect(lookupSession(token, t0 + SESSION_TTL_MS)).toBeUndefined();  // expired
expect(lookupSession(token, t0 + 1)).toBeUndefined();               // clock rolled back
```

The second line proved nothing: `lookupSession` **deletes on read**, so the first call had already
removed the entry and the second returned `undefined` whatever the clock did. Worse, the *fix* it was
written for — clamping `Date.now()` to a non-decreasing floor — was itself a no-op, for the same
reason: the floor only rises when something calls in at the higher time, and every such call has
already deleted the entry. **Caught by mutation-checking**: reverting the fix left the test green.

**Version 2 — it asserted only the positive half.**

```ts
Date.now = () => real() + SESSION_TTL_MS * 10;
expect(lookupSession(token)).toEqual(alice);   // survives a wall-clock leap
```

An implementation that never expires anything at all also passes this. **Caught by review pass 2.**

**Version 3 — it never exercised the production clock.**

Every `createSession`/`lookupSession` call supplied an explicit `now`. The `Date.now` monkey-patch
was therefore irrelevant, and swapping `monotonicNow()` back to `Date.now()` would have left the
whole test green. **Caught by review pass 3.**

The version that ships drives only the **default** arguments, controls `performance.now` (the clock
the code is supposed to use), and moves `Date.now` in both directions underneath. It fails against
the wall-clock implementation and against a never-expires implementation.

## Why "observe it red" did not help

The repo's standing rule — [prove tests fail](2026-07-15-prove-tests-fail.md) — is about the
*author's* discipline: write the test, watch it fail, then make it pass. All three versions did fail
before their fix and pass after. That is what makes this worth writing down: **red-then-green
certifies the test can distinguish "before" from "after". It says nothing about whether it can
distinguish "correct" from "a different wrong thing".**

Version 1 went red before the clamp and green after, because the clamp changed *something*. It just
did not change the thing the test was named for.

## What to do instead

Three questions, and they are cheap:

1. **Does the code under test have a side effect that could satisfy my assertion?** Here, expiry
   deletes on read — so *any* assertion of the form "then it is gone" is satisfied by the read
   itself. Assert against a path the side effect has not touched.
2. **Would the opposite failure also pass?** If the test says "still valid after X", write down what
   makes it *invalid* and assert that in the same test. A one-sided assertion is half a test.
3. **Am I testing the production path, or a path only the test uses?** Injectable clocks, fakes and
   seams are how these assertions become deterministic — and they are how the real default silently
   escapes coverage. At least one assertion must run the defaults.

The general form: **mutate the production code in the direction of the bug you fear, and check this
specific test fails.** Not "does the suite fail" — a different test catching it is not this test
being adequate.

## Where this bites hardest

All three versions were written **while fixing a review finding**, not from the plan. That is the
documented lapse point: under fix-momentum a test is a means to closing a finding rather than the
point of the work. `AGENTS.md` already requires the same mutation check for a test written mid-fix as
for a planned one — this is what it looks like when that rule earns its place three times on one
file.

Related: [a fixture too small cannot show the negative](2026-08-03-a-fixture-too-small-cannot-show-the-negative.md),
[check why a test is red](2026-07-30-check-why-a-test-is-red-not-just-that-it-is.md).
