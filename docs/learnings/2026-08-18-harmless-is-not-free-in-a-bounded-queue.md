# Harmless is not free: a fix that leaves work in a bounded queue must be checked against its ORDERING

**TKT-259, ai-review passes 1 → 2.** The first fix made a class of rows harmless to process and
stopped there. The second pass showed harmless is not the same as free, because the queue they sat in
was bounded and ordered.

## The sequence

A sweep claims outstanding exchange obligations. Some of them — those awaiting a confirmation only
another service can give — are ones commerce can do nothing about.

**Pass 1 found:** those rows were claimed and *charged a retry attempt*, so after ten passes they
**parked**. Since the claim predicate excludes parked rows, a later confirmation whose own follow-up
failed could never be swept. The mechanism added to prevent stranded capacity would have stranded it.

**The fix:** stop charging them. Release the lease, keep the budget, never park. Correct, and it
closed the finding exactly.

**Pass 2 found two things the fix did not close.** One was a leftover write (an error string on a row
that never attempted anything, which blocked the migration's rollback guard). The other is the
interesting one:

> the claim is `ORDER BY reversal_next_attempt_at` with a `LIMIT`, inside a pass bounded at
> `MaxBatchesPerPass` batches — so a large backlog of these rows fills every batch with work that does
> nothing and pushes genuinely actionable rows past the bound.

Head-of-line blocking. The rows were harmless *individually* and expensive *collectively*, and the
cost was paid by exactly the work the sweep exists to do.

**The real fix** was to remove them from the actionable set entirely — in the claim predicate and the
matching partial index — and observe them through a gauge instead. That was always the better design;
pass 1's fix had made them cheap enough that the design question looked settled.

## The rule

When a fix's shape is *"this row is now harmless to process"*, ask the second question:

**Does it still occupy a slot in something bounded?** Bounded means a `LIMIT`, a batch size, a
per-pass cap, a worker pool, a rate limit, a fixed-size buffer. If yes, then:

- **What is the ordering?** With `ORDER BY` + `LIMIT`, *older* harmless rows outrank *newer*
  actionable ones. The harmless rows win the queue precisely because nothing ever resolves them.
- **What is the worst-case population?** These backlogs are created by outages, so the worst case is
  not the steady state — it is the moment recovery matters most.
- **Can they be excluded rather than tolerated?** If the processor does nothing with them, excluding
  them costs nothing and removes the question permanently. Visibility is a **gauge's** job, not the
  work queue's.

## The meta-lesson

Two correct fixes composed into a new defect — the shape
[already recorded](2026-08-03-two-correct-fixes-can-compose-into-a-new-defect.md) — with a specific
tell worth naming: **pass 1's fix answered the question that was asked, and made the better answer
harder to see.** Once the rows were cheap, "should they be here at all?" stopped feeling urgent. When
a review pass fixes a symptom by making something cheap, ask whether the thing should exist in that
position at all, before the cheapness hides the question.
