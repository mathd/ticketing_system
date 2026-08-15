# A precondition that cannot fail is worse than no precondition

**2026-08-15 · TKT-250 · inventory allocation-set revision**

We already have a rule for [a green test that cannot reach the failing
state](2026-08-10-a-green-test-that-cannot-reach-the-failing-state.md). This is the same
failure one level up: not a *test* that cannot fail, but a **mechanism** that cannot fail —
while every test around it passes honestly, because the tests are right and the thing they
are testing is inert.

## The shape

TKT-250 added optimistic concurrency to inventory's full-set channel-allocation replace: the
caller presents the revision it believes it is replacing, and the server compares it under the
pool row lock.

The back-office editor has **two** reads of inventory's allocation set, and they exist for
opposite reasons:

- a **fresh read taken during the POST**, which is what makes the merge safe — it is the only
  trustworthy source for `sold_by`, `requires_code` and the sales window, the fields TKT-244
  deliberately removed from the form so a crafted submission could not choose them;
- the **revision the page carried when it was rendered**, minutes earlier.

Send the first as the precondition and the server compares its own current value against
itself. It matches **every time**. The save always proceeds, the editor reports that it is
protected against concurrent edits, the coded 409 exists and is wired end to end, and the
protection is worth exactly nothing.

Nothing local catches it:

- the store test passes — it calls the store directly and controls the revision itself;
- the API test passes — it never renders a page;
- the wire test passes — it constructs its own request;
- a mutation check passes — flip the comparison and the store tests go red, because the
  *store* is correct. The defect is in which value the caller chose to send.

Only a browser tier can see it, because the bug lives in the seam between what the page
renders and what the handler submits, and **that seam only exists in a real request**.

## Why it is worse than nothing

An absent precondition is a known gap: ADR-024 wrote it down for months, and TKT-250 exists
because someone read that sentence. A precondition that cannot fail is a gap that **looks
closed** — in the ADR, in the tests, in the UI copy telling the operator to reload. The next
person to touch allocation editing reads "stale writes are refused" and builds on it.

## The check

When adding an optimistic-concurrency token, ask one question:

> **Where does the value I compare against come from, and could it have changed since the
> client last saw it?**

If the answer traces back to a read the *server* took during the same request, the
precondition is decorative. The token must originate from the state the client actually acted
on — the rendered page, the ETag it was given, the row version it read — and travel back
unchanged.

Two corollaries, both of which cost a cycle here:

- **After a refusal, re-render the SUBMITTED token, not the current one.** Re-render the fresh
  one and the operator's second click silently applies the very set the refusal just stopped.
  The refusal becomes a speed bump.
- **A fixture that writes the guarded table directly does not move the counter.** The browser
  spec seeds `channel_allocations` with SQL, and only `ReplaceChannelAllocations` advances
  `allocation_revision`. Without an explicit bump, the "stale" revision still matches, the save
  succeeds, and the test passes while proving nothing — the fixture rule again, in a new place.

## And a note on the ticket itself

TKT-250 was filed as an **authorization** defect: a stale save was said to overwrite `sold_by`
and return a reseller's bound stock to the public pool. Shaping checked it against the code and
it does not hold — the fresh read above is exactly what prevents it, and it landed in TKT-244's
own later review passes, *after* the finding was written.

The remedy survived; the justification did not. Had the ticket entered at Ready, the ADR would
now claim an authorization guarantee this mechanism does not provide — and per
[ADR-021](../adr/ADR-021-ticket-lifecycle-trail-integrity.md), a security claim written in good
faith and never executed is exactly the kind that survives review. The DoR item that caught it
(`approach`: verify a proposed remedy against the code before inheriting it) is the one that
most often feels like ceremony.
