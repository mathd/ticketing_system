# Inert for two different reasons, and only one of them means delete

**2026-08-30 — TKT-304**

[The mechanism was inert, not the test](2026-08-23-the-mechanism-was-inert-not-the-test.md)
established the rule: when deleting a mechanism changes nothing, ask whether the test cannot see the
mechanism or the mechanism does not do anything — and if it is the second, delete the code.

That rule is right and this note does not weaken it. It adds the question you have to answer first,
because **two very different situations produce the identical mutation result**, and the mutation
alone cannot tell them apart.

**Ask what makes the mechanism unreachable. If it is a property of the algorithm, delete it. If it is
a decision someone plans to reverse, keep it — and give it a test that can reach it.**

## The two shapes

**Structural inertness — TKT-162's ceiling.** The voided feed walks strictly descending, so the
cursor is always at or below any ceiling taken from page one; the keyset predicate is therefore
strictly stronger and the ceiling can never change a result. Nothing about the system can evolve to
make that false. It is arithmetic. Deleting it was correct, and keeping it would have left a
guarantee-shaped thing that guaranteed nothing.

**Contingent inertness — TKT-304's currency guard.** Commerce's exchange handler compares the
resolved target currency against the source order's:

```go
if resolution.ResolvedPrice.Currency != src.Currency {
```

Delete it and the entire commerce suite stays green — the same signal TKT-162 gave. The reason is
that `priceResolution.validate` refuses any resolved price that is not EUR ("commerce sells in EUR
only"), and every reservation's currency is itself written from a validated resolution. Both sides
are EUR by construction, so the comparison cannot be true.

But the thing making it unreachable is **a policy in a different file that the code documents as
temporary**, with a live epic (TKT-10, multi-currency) tracking its removal. The comment beside the
EUR check says so in as many words: *"commerce's own pre-existing limitation, not the rule model's."*

Delete the guard and nothing fails today. The day EUR-only lifts, an exchange settles across
currencies and nothing objects — and the deletion will look, in the diff that removed it, exactly
like the correct TKT-162 one.

## The question that separates them

> Is the thing that makes this unreachable a property of the algorithm, or a decision someone plans
> to reverse?

A descending walk will always stay under its ceiling. "We only sell in EUR" is a sentence in a
backlog epic's crosshairs. The first is a proof; the second is a schedule.

If you cannot answer, look for a **named removal path** — a ticket, an ADR, a comment calling the
constraint temporary. Its absence is not proof of structural inertness, but its presence is close to
proof of contingent inertness.

## Keeping it is not enough — it has to be reachable in a test

A contingently-inert guard kept with no test is the untested-guarantee failure one level along: you
have argued the mechanism matters and shipped no evidence it works. The trap is that the obvious
fixture cannot reach it, because the shadowing check fires first — feeding this handler a USD price
from catalog gets `500 price resolution unusable`, never the currency comparison.

**Reach it by varying the side the shadowing check does not police.** `validate()` polices the
*resolution*; nothing polices the *source reservation's* stored currency. Seeding that row in USD
directly reaches the comparison and yields the 409. That row is not producible by any current
production path — which is the point, not a cheat: it is precisely the state the removal of the
constraint will create, so the test is a rehearsal of the future rather than a fiction.

The first attempt at this test failed the honesty bar in a way worth recording. It asserted the
observed `500`, which is what the code does today — and thereby **pinned the limitation as the
acceptance criterion**. When TKT-10 lands and the correct `409` finally arrives, that test goes red
looking like a regression at the exact moment the guard starts working. Splitting it in two fixed it:
one test pins the EUR-only refusal where it actually lives and says in its own comment that it is not
AC3; the other pins the currency guard. See
[a green test can bless the defect](2026-08-10-a-green-test-can-bless-the-defect.md) — the same
failure, applied to a limitation instead of a bug.

## A second-order deletion trap, from the same ticket

`ValidateExchangeTarget` was genuinely dead: no production caller, only its own smoke tests. Deleting
it was correct. But it was the **only producer** of `ErrExchangeCurrencyMismatch`, so the deletion
would have stranded that sentinel's mapping in `exchangeProblem` and left a table test asserting a
status nothing could produce — a green test beside a dead mechanism, created by a ticket whose whole
subject was removing green tests beside dead mechanisms.

**"No callers" is necessary and not sufficient.** Before deleting an exported symbol, ask what it is
the last *producer* or *consumer* of: sentinels it returns, interfaces it satisfies, constants only
it reads, error cases only it triggers. The fix here was to keep the sentinel reachable by routing
the handler's existing refusal through the mapper — provably a no-op, since the mapper returns the
same status and the same literal the inline write used.

## Evidence, not argument

Three mutations, each answering a question the others could not:

| Mutation | Result | What it settles |
| --- | --- | --- |
| Disable the currency comparison | exchange **succeeds, 200**, USD order settled against an EUR target | The guard is contingently inert, not harmless — its absence has a concrete failure |
| Revert the mapper wiring to a divergent literal | red on the message assertion | The wiring is live, not decorative |
| Delete `validate()`'s EUR-only check | the mismatch surfaces as the 409, exact message | The fixture **can** reach the failing state, so the test is falsifiable |

The third is the one that matters most, and it is the one a "keep the guard" argument usually skips.
Without it, "this becomes load-bearing later" is a promise; with it, it is a demonstration.
