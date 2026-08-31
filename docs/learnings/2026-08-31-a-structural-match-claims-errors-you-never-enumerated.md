# A structural match claims errors you never enumerated, and the tier that sees it is not the tier that made it

**2026-08-31 — TKT-307**

The ticket was small: make a reversed sales window answer 400 instead of 500, applying a rule the
service had already written down for presale codes. The first version answered **409 with the wrong
code**, which is worse than the 500 it replaced — a 500 says *something broke*, a confident 409 says
*this cap is wrong* about a cap that is fine, while the actual problem stays.

Three things went wrong, and each is worth separating from the others.

## 1. The mapper matched on shape, not identity

`problem()` classifies store errors into statuses. One of its branches is:

```go
func belowConsumption(err error) interface{ Channel() string } {
	var e interface{ Channel() string }
	if errors.As(err, &e) { return e }
	return nil
}
// ...
case belowConsumption(err) != nil:
	// 409 allocation_cap_below_consumption
```

It matches **any error implementing `Channel() string`**. The new refusal implemented it — as it had
to, because every per-row refusal must name the row the UI attributes it to. So it was claimed by
that branch, which ran first, and answered a status and a code belonging to a different failure.

**A dispatcher matching on a structural interface claims every error satisfying it, including ones
written after the dispatcher.** The author of the branch enumerated nothing; the compiler enforces
nothing; and the new error's own `case` sat below, unreachable.

The corollary is specific and mechanical: **before adding an error sentinel, grep the mapper for
structural matches** — `errors.As` against an anonymous interface, `interface{` in a case — not just
for the sentinels it names by identity. Then place the new case above every shape match it satisfies.
One grep would have caught this before it shipped.

## 2. The tier that could see it was not the tier the rule pointed at

`AGENTS.md` says a test must live at the tier its mechanism does. That rule was **applied, and
correctly**: the defect was a database CHECK surfacing as an unmapped error, which no fake
reproduces, so the test was a store smoke test against a real database. It asserted:

```go
if !errors.Is(err, ErrSeatSetInvalid) { … }
```

True, and green, throughout. The misrouting happened one tier up, in code the store test never
executes.

**The mechanism and the classification are two different questions with two different tiers.** The
store produces an error; the API maps it to a status. Asserting the sentinel where it is produced
proves the store's half. Asserting the status requires calling the mapper.

The tell is in the acceptance criterion. This ticket's COS said *"refused with 400"* — a statement
about the API — and the test asserted a statement about the store, on the assumption that one
implied the other. Whenever a COS names a **status**, a **response body**, or **what a client sees**,
the assertion belongs where that is decided, however deep the mechanism producing it lives.

## 3. An over-broad condition can be the only thing holding a correctness property

Separately, the consumer retried on:

```go
if e.Schema == 1 || errors.Is(err, errResolveUnavailable) {
```

The first arm reads as laziness — *retry the whole schema, someone could not be bothered to
classify*. Removing it is the obvious cleanup, and it was the ticket's stated item.

It was load-bearing. `errResolveUnavailable` was wrapped in exactly **one** other place, on a
different schema's path; the schema-1 resolver returned transport failures, bad statuses and decode
errors raw. So `e.Schema == 1` was the only thing making a real catalog outage retry. Narrowing to
the second arm alone would have sent every outage to `Term()` — **permanently losing publications and
leaving slots with no inventory**, which is strictly worse than the bounded resource leak being
fixed, and is the exact failure the condition's own comment was written to prevent.

**Before narrowing a disjunction, evaluate each arm's coverage separately and ask what is reachable
*only* through the arm being removed.** If the answer is non-empty, the fix is upstream — classify
properly at the source, then narrow — not at the condition.

What made this catchable: the plan's risk section said *"confirm what the resolver actually returns
before narrowing"*. Writing the risk down is what turned it into a step.

## The pattern across the review passes

Three adversarial passes, six findings, 3 → 2 → 1. **Every finding after the first was a sentence
written while fixing the previous pass**, not broken code:

- a comment claiming schema 5 called a function it never calls;
- a comment claiming a failed queue write was recovered by the next sync, when the recovery query
  filters on a state that write is what sets;
- a contract promising a field the same commit's regression test proves absent;
- a design justified by "so the editor can put the message beside the field" — for an editor that
  does not render that field at all.

They cluster in a recognisable place: **one layer away from the code under the cursor.** What the
other schema calls. What the UI renders. What the contract requires. The edited lines were right
every time; the sentences about their surroundings were not.

Nothing gates prose — not types, not tests, not mutation, not the gate. The habit that catches it is
to **re-open the file each new sentence is about**, and it is cheap precisely because fix-momentum is
when it feels least necessary. This is the second consecutive ticket to record the pattern (see
[a mutation your generator cannot reach](2026-08-31-a-mutation-your-generator-cannot-reach.md) §
corollary), which is what makes it a habit rather than an anecdote.
