# Implementation and architecture review

Inspect the complete release diff, from the base to the candidate. Inspect how it fits the code that
was already there. Story reviews do not show that the stories agree with each other.

Trace these across the stories:

- the promised user journey,
- the shared contracts,
- permissions and data handling,
- failure behavior,
- the interactions between stories.

Check that the release outcome can be observed. Check that unfinished work that was left out is
really isolated from the candidate.

Review the architecture against the governing decisions and against the system as built. Find:

- layers that nothing needs,
- two mechanisms that do the same job,
- contracts that disagree,
- choices that limit later work.

Check each diagram against the real data flow and control flow. Propose an ADR for a lasting choice
that crosses systems or is costly to reverse. A routine mechanic does not need an ADR. A needed
correction goes through the normal ticket, PR and gate path.

## What each lens receives

- **Initial inspection:** the requirements, the actual diff and code, and the verification facts. No
  plan, no decision log, no author rationale.
- **Decision audit:** the approved brief, the story plans, the effective decision entries and the
  initial findings. It checks that the code follows them and that no material choice is missing from
  the logs.

Separate verified defects from preferences. Support each finding with evidence that names the
revision, a consequence and a correction.

## What you return

Return the shared result for the area `implementation`, as `assets/review-result.example.json` in
`sdlc-ticket` shows it. Set `stage` to `sprint-review` and `area` to `implementation` on the record
and on both lens results. State the files and interfaces you inspected, and your limits.

A correctness, security or acceptance failure in the current release blocks readiness. It blocks even
when it is below the severity that repeats a round. Do not turn an incidental refactor into
required release work without evidence that a release criterion fails.
