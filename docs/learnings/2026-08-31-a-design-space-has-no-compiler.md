# A design space has no compiler

**2026-08-31 — TKT-306**

Four tickets in one session recorded the same shape: after the first review pass, findings stop being
broken code and become **sentences written while fixing the previous pass** (see
[a mutation your generator cannot reach](2026-08-31-a-mutation-your-generator-cannot-reach.md) and
[a structural match claims errors you never enumerated](2026-08-31-a-structural-match-claims-errors-you-never-enumerated.md)).

This note is about the version that iterates, because one paragraph took **three** attempts and each
attempt was written specifically to fix the previous one's false claim.

**Prose about what COULD be built needs the same verification as code. Open the file, or write "I
have not checked".**

## The three versions

`SelectPricingRule` and `SelectFeeRules` both refuse two rules sharing an id — the last tie-break is
the id, so duplicates make the winner depend on input order. `SelectSplitSchedule`, the third
comparator, has no such guard. The ticket asked why, and the answer went into an ADR:

**v1 — "structurally impossible."** `SelectSplitSchedule` returns a bare `SplitSelection` with no
error, so it cannot report. *False:* it cannot report **in that shape**. Review pass 1 pointed out
that this suppresses the ADR's own revisit trigger on a money-allocation path.

**v2 — "three designs avoid the signature change":** validate the loaded set in the caller; carry an
invalid state on `SplitSelection`; reject duplicates at the load path. *Two of the three are
impossible.* `loadSplitSchedules` collapses rows through a `byID` map, because **repeated ids are how
a multi-part schedule is stored** — one row per part. The caller never sees a duplicate to reject,
and loader-side rejection of repeated ids would refuse every legitimate multi-part schedule.

**v3 — one viable home**, inside `SelectSplitSchedule` itself, plus a written record of *why* the
obvious first idea fails.

## What made this different from an ordinary wrong comment

Every version was produced *while correcting* the previous one, so the usual defence — "I was careful
because I had just been caught" — was active each time and did not help.

The reason is in what the sentences were about. A comment describing code is checkable against the
code beside it, and the pull to check is strong. A paragraph describing **designs that do not exist**
has nothing beside it. There is no compiler, no test, no mutation, and no adjacent line to contradict
it. It reads as reasoning rather than as assertion, and reasoning feels self-validating.

`loadSplitSchedules` was two greps away, all three times. I never opened it, because the question felt
like *what could we build?* rather than *what does this do?* — and only the second phrasing sends you
to a file.

## The rule

When a fix round produces prose about alternatives, **each alternative is a claim about existing
code**: about what a caller receives, what a loader produces, what a signature permits. Verify each
one the way you would verify a code change, or say plainly that you have not.

Two cheap tells, both available before a reviewer sees it:

- **The paragraph names a function you have not opened this session.** That is a claim, not a fact.
- **You are writing about a design space rather than about a mechanism.** Design spaces are where
  confident wrongness is cheapest to produce and most expensive to catch — it took two review passes
  and a third correction here.

## The corollary, from the same ticket

A second finding in this run had the same root in a different disguise: **"unreachable through the
database" does not justify skipping a guard's semantics.**

The fee duplicate guard was moved past the currency check, on the reasoning that the reordering was
unobservable since `fee_rules.id` is a primary key. But the guard exists **for the pure comparator
seam**, which accepts hand-built candidates — the identical PK argument would make the guard itself
pointless. A defence built for a seam must have its changes evaluated *at* that seam, not at the
storage layer whose reachability the defence already declines to trust.

The two findings share a cause. Both substituted a plausible argument for the two-minute check that
would have settled it: one about a loader, one about the seam a guard was written for.

## Measuring beats arguing

The one thing in this ticket that came out right first time was the claim that the split comparator is
order-dependent — because it was **run** rather than argued:

```
AB payee=1111…   BA payee=2222…      order dependent
AB candidates=0  BA candidates=0     and invisible in provenance
```

Same input, two orderings, two different payees on the payout path, with both duplicates absent from
the reported candidates. That took one throwaway test and turned a plausible statement into a pinned
one — and the pinning is now a gap test in ADR-021's shape, so the claim cannot drift from the
behaviour.
