# Two correct fixes can compose into a new defect

**TKT-174, TKT-180, TKT-181, TKT-182 (PRs #148, #150, #151, #152) — 2026-08-03**

## What happened

Four tickets in one epic hit the same shape: a review pass found a defect **created by the
previous pass's fix**, and each fix was correct in isolation.

- **TKT-174.** Bounding the read with a deadline was right. Making the poll defer to an
  authoritative read was right. Together, a stalled response body held the guard for ever and
  polling stopped.
- **TKT-181.** Three passes on a single retry-vs-terminate boolean; passes 2 and 3 each found a
  defect the previous fix introduced.
- **TKT-182.** Pass 1 replaced a full-pool scan with a bounded query — which **failed open** on a
  missing adjacency row. Pass 2's coverage guard then checked only the *requested* seats, while
  the query also reads each *candidate's* row. Both replacements looked strictly safer than what
  they replaced.
- **TKT-180.** Four passes on a four-line diff, same pattern.

## Why it is easy to miss

A fix is reviewed against the finding it answers, not against the other fixes in the same round.
Each one is a local improvement, so a per-finding review approves all of them; the defect lives in
the interaction.

## What to do

- After a round of fixes to a state machine or a query, **review the interaction of the fixes**,
  not each in isolation.
- Treat a bounded/optimized replacement as a **new implementation**, not a narrowing: ask what the
  version it replaced would have caught that it now cannot. TKT-182's fail-open was exactly this —
  the pool-wide scan would have caught the asymmetric case.
- This is the argument for the churn cap's **reset clause**. Applied mechanically as "two passes
  and stop", every ticket above would have merged a defect. Where a pass invalidates the previous
  pass's fix, the counter resets.
