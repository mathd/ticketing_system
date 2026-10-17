# Cross-story retrospective

Read the closeout of every member ticket except the closure task, whose closeout comes after the
release (the `kind=metrics` comment with its `learnings:` and `retro:` sections), the review findings, the material decisions and the human's later steering. Group
the repeated causes, the late discoveries, the duplicated work, the tests and reviews that cost too
much, and the controls that caught or missed real defects. Name the stories and the observations that
support each pattern. One agent's view of its own task does not show a problem for the whole project.

Read the board's data too: `GET /metrics?sprint=` for the counts and for the time per member. A member
with `attributed: null` has unknown time. Do not read it as zero. A sprint with `historyGaps` has
unreliable history and metrics. Say so in the retrospective.

For each useful candidate, state:

- the evidence,
- the consequence,
- the expected benefit,
- the destination,
- the proposed action: adopt, reject, trial or no action.

Keep a local choice in the story's records. Join related decisions into one ADR candidate only when
they set a lasting constraint, touch several parts of the system or are costly to reverse. Do not make
an ADR for a routine name or a file mechanic. A shared decision that constrained dependent work
should have been reviewed when it was made. Show a late discovery as a process observation.

Propose short changes to a skill or to the process, with their trade-offs and an idea to evaluate
them. Keep the incident details in the retrospective. An agent's suggestion is evidence. It is not
authority to change a rule, a permission or the gates of this sprint. The human selects the
improvements. A selected change becomes reviewed follow-up work. Finishing future improvements is not
a release condition.

## What you return

Return the shared result for the area `retrospective`. Set `stage` to `sprint-review` and `area` to
`retrospective` on the record and on both lens results. Show the candidates apart from the human's
actual selections. "No repeated pattern" and "no worthwhile change" are valid results when the
inspected records support them. Do not invent a lesson to fill the report.
