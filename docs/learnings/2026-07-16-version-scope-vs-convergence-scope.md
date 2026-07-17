# Per-entity version counters do not survive convergence onto a shared resource

**Ticket:** TKT-75 (PR #56) · **Caught by:** adversarial ai-review, pass 1 · **Missed by:** plan
draft (gpt-5.6-sol), plan critique (main-agent), the implementer, and every test written before
the review.

## What happened

Catalog emits `performance.closed`/`.reopened` with a `closure_version` that is **monotonic per
performance**. Inventory converges grouped festival days onto **one shared pool** (the capacity
group), and the first implementation stored a single `closure_version` on the pool, applying an
event only when its version exceeded the stored one.

Two days, two independent counters, one pool:

1. Day A: `closed(v1)` → pool closed, version 1.
2. Day A: `reopened(v2)` → pool open, version 2.
3. Day B: **first ever** `closed(v1)` → `1 <= 2` → judged a stale replay and discarded.
   The pool stays open; a weather-closed day keeps selling.

The inverse interleaving can also pin a fully reopened group closed. Every individual piece was
correct — monotonic counters, dedupe, lock ordering — and the composition was still wrong.

## The rule

**A version/sequence number orders events only within the scope that issues it.** The moment a
consumer converges multiple source entities onto one row (shared pool, aggregate, materialized
view), comparing their versions against a single stored counter compares unrelated clocks.

Order **per source entity** (a row keyed by the entity, carrying its own version), and **derive**
the shared resource's state from the entity rows, under the same lock that guards the resource
(`pool_slot_closures` + derived `closure_status`, ADR-010 pool lock).

Corollary from the same ticket: **a state-lookup during event processing must classify not-found
by domain invariant, not as transient.** Closure transitions only exist while a slot is published
(TKT-50 §Case 3), so a resolver 404 while handling `closed` means "archived since" — the event is
moot and must be acked; parking it retries a lookup that can never succeed.

## How to spot it at planning time

When shaping or reviewing a consumer, ask: *does any consumed event carry a version or sequence,
and does the consumer converge multiple emitters onto one row?* If both, the version's scope and
the convergence scope differ, and a pool-level comparison is a bug that no single-entity test
will ever catch. The regression test needs **two** entities with overlapping version sequences
(`TestGroupedPoolOrdersClosuresPerSlot`).
