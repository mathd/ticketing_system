# Seven review passes mean the plan has no model

**2026-10-18 — TKT-399, TKT-556**

When code review keeps finding new defects in the same area after each fix, the defect is usually
in the plan layer, not the code. TKT-399 (scanner exit mode) failed seven ai-review passes. Each pass
found a new combination of local scanner outcome and failure point that the code handled wrongly,
and each fix was correct on its own.

## How it was established

The plan had no model of the space it covered. Nothing listed every local outcome against every
place a request can fail (browser, gateway, Access runtime, validator, store). Reviewers sampled
that space one cell at a time. The owner had set a limit in advance: if the seventh pass failed, the
team would stop and re-plan from what it had learned. The work stopped, and a spike (TKT-556)
produced the model as ADR-085: a client-knowledge axis, a producer-by-layer matrix, outcome tables
and invariants. The ADR split the work into ordered tickets. The first of them, TKT-557, then needed
only one ai-review round.

## The rule

- After two review rounds in a row that find new cases of one kind, stop fixing. Ask whether the
  plan enumerates the space that the cases come from.
- If it does not, write the model first, as a spike with an ADR. Then re-plan from the model.
- Set the stop point in advance (here, pass seven). Each fix looks reasonable, so it is easy to
  keep going without one.
