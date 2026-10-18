# Sprint handoff

The `sdlc-sprint` skill owns the sprint lifecycle. The `sdlc-sprint-review` skill owns the validation
of a whole release, the four-area review and the French or English HTML report. Both skills are
beside this one, in `.claude/skills/`. Do not copy their procedures into a ticket. Do not replace a
missing skill with a generic code-review brief.

The board keeps the sprint: its key (`S-<n>`), its state and its members. The sprint's comments keep
the approved brief, the human decisions and the review records. The design is in the board
repository (`docs/sprints.md`). The board mechanics are in `sdlc-sprint/references/board.md`.

## Claim rule

`GET /board` reports `sprintMode` and the list of sprints. When `sprintMode` is `true`:

- Claim only a `Ready` ticket whose `sprint` is the key of the **active** sprint.
- If no sprint is active, claim nothing. Report that to the human.
- A sprint never waives a gate. Gate 1, the blocker check, the plan gate, the review, the merge and the
  acceptance apply as always.
- Starting and closing a sprint are human requests. A Done ticket, an end date or a label never start
  or close one.

When `sprintMode` is `false`, none of this applies. When it is `true` and the vault has no sprint
yet, no sprint is active: claim nothing, as above.

## A story that is a member

Read the `sprint` field of the ticket. Read the current brief: the newest `sprint-brief` record on the
sprint note. The board owns the list of members and the state. An epic or a coordination ticket is not
a sprint. An Epic cannot be a member. Its children can.

Keep the correctness, the security, the acceptance and the affected integration checks of the story
inside its ticket. Plan and code reviews repeat for Critical and High findings, and for a material
change. A Medium correction needs a disposition and verification. The broader criteria of the sprint belong to
`sdlc-sprint-review`. A story's Done does not mean that the release is validated or tagged.

Keep the closeout factual. Its `learnings:` and `retro:` sections feed the sprint retrospective. A
local choice does not become an ADR by itself. The choice of retrospective items, and the approval of
the release, the tag and the close, stay with the two sprint skills and with the human.

Do not move a ticket into or out of a sprint yourself. `sdlc-sprint` does it. `POST /ticket` ignores a
`sprint` field. `POST /ticket/new` refuses one.

## The closure task

A sprint with `release: true` has one closure task. It is a member that holds the release work. It
follows the normal ticket flow: shaping, planning, plan review, the plan gate, labels, the PR and
acceptance. `config.gates` decides who takes each gate. The task's deliverable is the release packet
and the report under `docs/sprints/<S-n>/`, delivered on the task's own PR.

- The four review pairs of `sdlc-sprint-review` are the independent review of this task. Do not add a
  second review of a diff, and do not add a second review budget.
- The report is ticketed work, so it lands like any ticket's deliverable: on the task's branch and
  PR, merged only after `check` passes on the PR head (SKILL.md § Hard rules). The merged PR is what
  lets the board move the task to `PO Review`; no `override` is needed. A correction that changes
  product code or its tests uses a linked ticket with the usual gates and PR.
- The human's decision for this task is the **close approval** that is recorded on the sprint. It
  names the exact report digest, the candidate, the digests of the brief and of the scope, the tag and
  the sprint key. Both gate modes need it from a human, in addition to the task's normal gates.
- After the approval, `sdlc-sprint` publishes and verifies the tag. That is the only action after the
  hand-off. A material change to the deliverable goes back to review and to a new approval.
- The task is Done when its PR is merged and the report read back on the default branch, the
  evidence is complete, the close approval is recorded and the tag is verified. Post the `kind=metrics` closeout before the move to Done,
  as for any ticket. `sdlc-sprint` then reads the members again and closes the sprint.
