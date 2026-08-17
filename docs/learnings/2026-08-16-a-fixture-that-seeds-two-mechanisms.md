# A fixture that seeds two mechanisms proves at most one of them

**2026-08-16 — TKT-241.** A ticket whose entire subject was *"this test is green without proving
anything"* shipped a first version that was green without proving one of the two things it seeded.
The cross-model review pass caught it; nothing local did.

## What happened

TKT-241's job was to prove, end to end, that catalog's sales-channel registry is a **lookup, not a
constraint** — an unregistered or disabled channel code sells exactly as a registered one does. The
existing guards were an inventory-store test and a catalog foreign-key test, neither of which
crosses a service boundary, so a registry lookup added in commerce or in catalog's fee or split
resolution would refuse the sale earlier and leave both green.

The new tests buy a real purchase through the gateway. To make that purchase *observable*, each
seeds two things at the channel scope:

- a **fee rule** (ADR-046), and
- a **split schedule** (ADR-047),

because commerce treats *"no rule matched"* as a **successful** resolution with an empty fee set
(`services/commerce/internal/api/server.go:805-808`). A sale with no fee rule completes identically
whether fee resolution ran correctly, ran degenerately, or was skipped — so without a seed the test
traverses the resolver and observes nothing.

That reasoning was written down explicitly, at plan-review, as the justification for the fee seed.
It was then **not applied to the split seed sitting next to it.** The assertions read the fee's code
and amount, and stopped there.

Split selection changes neither value. A fee with **no** split is forwarded with no parts and
payments records it as *collected-and-unattributed* rather than refusing
(`services/commerce/internal/api/catalog_fees.go:648-652`) — deliberately, because fees shipped
before split schedules did. So deleting the split seed, or making the resolver drop this channel,
left all three tests green.

The proof, once the assertion existed: with the split seed removed, every snapshot reports
`"mode": "unsplit", "reason": "no_schedule"` while `passed_on_fees` stays **600**. That unchanged
600 is the whole finding. The fee assertion could not see the split half by construction.

## Why nothing local caught it

Everything downstream of the author's model of correctness agreed with the author. The gate was
green. The tests were green. The mutation set *chosen by the same author* was green, because the
mutations were selected from the same understanding that produced the gap — the fee mechanism was
mutated, the split mechanism was not thought of as a separate thing to mutate.

This is the sharpest available demonstration of why cross-model review is a prerequisite rather
than an option: the author had **already written the correct argument** one stage earlier and still
failed to apply it twice.

## The rules

**1. When a fixture seeds N mechanisms, ask separately of each one what assertion would notice its
absence.** Not "does the test pass" — *"if I delete this seed, what goes red?"* A seed with no
answer is decoration, and it is worse than absent: it makes the test look like it covers a layer it
never observes. Reviewers read the fixture as a statement of scope.

**2. A mutation caught by a lower tier proves the mechanism is live, not that your test is the one
catching it.** Three of the five mutations run for this ticket died in a per-service store suite
that runs before the black-box tier, so the new tests never executed. That is correct
defence-in-depth and useless as evidence for the new test. **The mutation that is evidence is the
one only the new tier can catch** — here, a registry lookup added to catalog's fee resolution.

**3. A ticket whose deliverable is coverage for an existing gap must demonstrate the gap.** The
single most valuable artifact this ticket produced was one run in which:

```
--- FAIL: TestAPublicSaleOnAnUnregisteredChannelCompletesEndToEnd
--- FAIL: TestAPartnerSaleOnAnUnregisteredChannelCompletesEndToEnd
--- FAIL: TestASaleOnADISABLEDRegisteredChannelCompletesEndToEnd
ok  ticketing/services/catalog/internal/store    35.110s
ok  ticketing/services/inventory/internal/store  33.472s
```

New tests red, **both pre-existing guards green**, under a mutation that makes the registry
authoritative on the sale path. Without that run, "we added a test for the gap" is unfalsifiable —
it asserts a counterfactual about tests nobody re-ran.

## Also from this ticket

- **A stale context-mémo is a real cost, and re-resolving at claim time is what pays it.** The mémo
  was baked 2026-08-09. By claim time ADR-024 had taken two amendments (TKT-246 `sold_by`, TKT-250
  allocation revision) and **ADR-054 had become the first predicate in the claim guard while being
  absent from `governingAdrs` entirely**. The re-resolution rule caught all three.
- **A ticket's COS can be unachievable by design, and only a code read reveals it.** TKT-241 asked
  for the channel recorded verbatim on the claim *and* the order from one purchase. The public route
  forwards no channel to inventory — TKT-240 added that forward and it was reverted, because the
  route is unauthenticated. The COS had to be split across two tests, one per authorization shape.
