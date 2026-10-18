# Classify by state you own, not by how the driver spells the error

**2026-10-18 — TKT-557**

When a status depends on *when* something failed, decide it from state the server owns: a phase
flag under a mutex. Do not decide it from the error text or type that the driver returns. Two
round-3 plan-review findings and three fixture findings came from one root cause: rules keyed on
driver error spelling, and short contexts that reach driver I/O.

## How it was established

- One blocked dial with a 500 ms context returned `context.DeadlineExceeded` on some runs and
  `net.OpError → poll.DeadlineExceededError` on others. The spelling depends on a race between the
  socket deadline and the context timer (TKT-557 probe3). A rule keyed on `errors.Is(err,
  context.DeadlineExceeded)` gave the same expiry 503 on some runs and 500 on others.
- A 50 ms Touch context, meant to bound a lock wait, also reached `database/sql`'s reset ping and
  the dial, so it discarded pooled connections it was never meant to touch (PR3-2).
- The begin site already used a server-owned flag (timer fired while pre-decision, parent alive,
  phase closed under the mutex before handoff), and reviewers found it sound every round.

The round-3 audit said that more local patches would not converge within the review cap. Moving
every site to the begin site's design closed all five findings in one revision, and round 4
approved it. Probe: 32/32 blocked-dial classifications gave 503 from timer state.

## The rule

- If a status depends on a phase ("before the decision"), record the phase yourself and classify
  from it. Keep the driver error only as the logged cause.
- A context sets a bound on everything below it. Before you shorten one, list what it reaches:
  pool acquisition, reset ping, dial, socket deadlines.
- When review rounds keep finding new local defects with one shared cause, simplify the design. Do
  not add another fixture rule.
