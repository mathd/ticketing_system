# Say what a check establishes, not what it is named after

**TKT-172, TKT-179, TKT-182 (PRs #146, #149, #152) — 2026-08-03**

## What happened

Three checks in one epic proved something narrower than their names implied, and in each case the
gap was invisible while every individual statement was true.

- **TKT-182.** A guard named for fail-closed behaviour was fail-closed **for requested seats** —
  while the query it protected also read each *candidate's* row. A later review found that
  provisioning could establish **internal consistency** (complete and reciprocal) but not
  **fidelity** to the seat map: a projection whose seats all name no neighbours is perfectly
  reciprocal, and nothing in the consuming service can tell it apart from a map of one-seat rows.
  The fix was to separate the three claims — fidelity at the derivation, internal consistency at
  provisioning, reachable-slice soundness at claim time — and to name what none of them covers.
- **TKT-179.** Making a response field **required** is a *shape* guarantee. It broke a TypeScript
  fixture at compile time and gave zero protection against the handler returning the wrong value.
- **TKT-172.** "The index name appears in the plan AND there is no Seq Scan" is jointly satisfiable
  by a **full scan of a partial index**. The binding claim is the access condition
  (`Index Cond: (pool_id = $1)`).

## What to do

Before writing "fail-closed", "verified", "tamper-evident" or "scoped", write the sentence that
says **which inputs** and **which adversary or failure mode**. If that sentence is hard to write,
the check is doing less than its name claims.

This is [ADR-021](../adr/ADR-021-ticket-lifecycle-trail-integrity.md)'s discipline applied to
ordinary correctness, and it works the same way: name the adversary — or the input set — first.
