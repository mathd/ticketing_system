# When a new check fails 27 tests, ask which side is wrong

**TKT-123, PR #133.** Adding a two-line contract check broke 27 existing subtests. The obvious
reading — *the fixtures predate the contract, update them* — was correct. It was also
indistinguishable, from inside the test suite, from the reading that would have caused an outage.

## What happened

ADR-009 §5 makes `type == subject` part of the domain-event envelope contract. Inventory dispatched
purely on the NATS subject and never read `type`, so it accepted envelopes violating it. The fix is
two lines beside an existing `id` check.

It failed 27 subtests. Every inventory fixture omits `type`, because nothing had ever required it.

**The tempting move is to add `type` to the fixtures and move on.** The failing tests are obviously
pre-contract artifacts; the check is obviously right; the ADR is obviously on your side.

But the test suite cannot tell you the difference between:

- *the fixtures are stale* → update them, ship a contract fix; and
- *the producer never sets this field either* → the same check terminates **every real event**,

because in both worlds the symptom is identical: N tests fail because a field is absent. The fixtures
were written by the same people, with the same assumptions, as the producer.

The check that separates them is one grep at the **producer**:

```
services/catalog/internal/events/events.go:313   Type: SubjectPerformancePublished
services/catalog/internal/events/events.go:420   Type: SubjectPerformanceArchived
services/catalog/internal/events/events.go:361   ID: …, Type: subject, …     // closed + reopened
```

Catalog does set it, for all four consumed subjects. Fixtures stale, check safe. Two minutes.

## The rule

**A new validation that fails existing tests is telling you one of two very different things. Find
out which at the producer, before you touch a fixture.**

The question to ask is not *"are these fixtures right?"* but *"what does the real writer emit?"* —
and the answer lives outside the suite that is failing. Fixtures inherit the assumptions of the code
they were written against, so a fixture omitting a field is **evidence about the author's beliefs**,
not evidence about production.

Three signals that you are in this situation:
- the new check is a **contract** or **schema** assertion (something a producer must satisfy),
- the failures are **broad and uniform** — many tests, one missing field, one message,
- the failing fixtures are **older than the contract** you are enforcing.

Then: grep the producer, and say in the commit what you found. Had catalog omitted `type`, the
correct outcome was not "fix 27 fixtures" but "stop, this ticket changes what production accepts".

## Corollary: how to update fixtures once you know they are stale

Not necessarily one literal at a time. `type` was not the variable those 27 tests were exploring —
they vary the *subject* and the payload — so stamping the subject's `type` at the few message
construction sites keeps every field they actually test literal, and leaves the one test where
`type` **is** the variable deliberately unstamped. Editing 27 literals by hand would have been more
churn and more chances to weaken an assertion by accident.

And when a test asserts bytes are stored *verbatim*, compare against what was **delivered**, not
against the bare literal — the property is "we store what arrived", and what arrived is now stamped.
