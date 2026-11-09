# Simplification phase

Each story stays small, but a sprint still collects duplicated code, dead code and extra tests across
stories. This phase cleans, refactors and simplifies the sprint's code before the review packet is
frozen. Every `review: "full"` sprint needs it. It runs on the sprint's simplification task, after
every other functional member is Done (accepted and merged), and before final validation. Adopted
from the upstream sdlc skills on 2026-11-09.

The task is a normal member: shaping, planning, plan review, the plan gate, labels, the PR and
acceptance, with `config.gates` deciding who takes each gate (`sdlc-ticket` `references/sprints.md`
§ The simplification task).

## Find the cuts

Read the complete diff from the sprint's baseline SHA (the brief) to the current `origin/main`.
Measure it first with `git diff --shortstat <baseline> origin/main`, code and test lines apart. Look
for:

- Code that two stories added to do the same thing. Keep one.
- Dead code: unused functions, parameters, options, configuration keys, flags and branches, and
  leftover experiment or debug code.
- Abstractions with one caller, layers that only pass calls through, and options that no caller sets.
- Guards and fallbacks for cases that no COS names and the system cannot reach. Before you cut a
  guard, ask *why* it is unreachable (`AGENTS.md`, "inert for two different reasons"): a guard made
  unreachable by a policy someone plans to reverse stays, and gets a test that reaches it.
- Tests that repeat an assertion another test makes, test behaviour the sprint did not change, test a
  mock instead of the code, or mirror the implementation line by line. Name the test that still
  protects the behaviour. Never cut a test that makes a rule observable: one test per guard
  predicate, the test at the tier where the mechanism lives, and the mutation evidence for a fixture
  all stay (`AGENTS.md`).
- Comments and documents that the sprint made stale.

Prefer deletion to rewriting. A refactor that adds lines needs a reason that the human accepts.

## Plan and apply

The list of cuts is the task's plan. For each cut, give the location, why it can go, the estimated
lines removed, and the test or check that still protects the behaviour. The plan gate approves the
plan as usual and can drop cuts.

If the pass finds nothing worth cutting, the plan says so with the measured diff and the
`origin/main` SHA it measured. The task then has no PR. The plan-gate approval of that plan, for that
SHA, is the task's acceptance. The plan gate moves it `Planning → Building` as usual. Then post the
`kind=metrics` closeout, move it `Building → PO Review` with
`override:"simplification: no cuts; plan approved at <sha>"` (the only board check it bypasses is the
merged PR), and then to `Done`. If `origin/main` moves before the approval, measure again.

Apply only the approved cuts. Each cut keeps observable behaviour, public interfaces, data handling
and permissions the same. A cut that changes one of them is a material change: move it to a separate
ticket. Do not add features. Add a test only where a cut removes the only protection of a COS.

Proof: `make check` passes on the result (the gate verdict file is the authority), the browser specs
pass for any form a cut touched (`make browser`), and `git diff --shortstat <baseline> origin/main`
shows the net change. A task with cuts follows the normal ticket review, merge and acceptance gates.

## Record

Post the diff sizes before and after the phase in the task's `kind=metrics` closeout. The release
report records them as one `measurements` entry: metric `Sprint diff size`, scope
`<baseline>..candidate`, `baseline` the size before the phase and `candidate` the size after. The
`implementation` review area checks the result and reports the cuts that the phase missed.
