# Sprint lifecycle

Start and close are human requests in both gate modes. The agent runs one only when the human's
request names the sprint. A request to start also names the scope digest. Dates, the last Done
ticket and a label never start or close a sprint.

## Decide the kind of sprint

- `release: true` is a finite release. It has a closure task, a report and a Git tag.
- `release: false` is a planning-only sprint. It needs Done members and the human's close request.
  It has no report, no tag and no closure task.

The brief states which one. A sprint does not change kind after the human approved the brief. To
change it, post a new brief and get a new approval.

## Prepare

Start from the human's theme and from the outcome that people can observe.

1. Read `GET /sprints`. At most one sprint is `planned` and one is `active`. If a planned sprint
   exists, use it. Never make a second one.
2. Create the sprint with `POST /sprint/new`. Give a name, a goal and the planned dates that the
   human agreed, each with an offset.
3. Select vertical stories in dependency order. Do not take a whole epic with no bound. An epic can
   span several sprints. Record the exclusions, the acceptance criteria, the useful subsets and the
   shared architecture choices. For a release, also record the previous release tag and base SHA,
   the proposed next tag and the named release approver.
4. Assign each ticket with `POST /ticket/sprint` (`board.md`). A reason is optional while the sprint
   is planned. A member is a Story, a Task, a Bug or a Spike. Assign the children of an epic, never
   the epic. A Backlog ticket can be a member. Gate 1 and the blocker check still decide when a
   ticket can be claimed.
5. Look at dependency and package updates at every opening. Select the justified updates in one
   scoped maintenance ticket, or record that none is justified. Do not upgrade everything. Do not
   make an empty maintenance ticket. An urgent update can join an active sprint, with a reason.
6. Plan missing real-integration and end-to-end suites as tickets in the sprint. They are work, not
   wishes.
7. For `release: true`, create the closure task:
   - `POST /ticket/new` with `type: "Task"`. Its description holds the criteria: the report is
     committed, the release evidence is complete, the close approval is recorded and the tag is
     verified.
   - Shape it like any ticket (`sdlc-ticket` `references/shaping.md`).
   - Assign it to the sprint.
8. Read `GET /sprint` again. Check the members against the plan. Build a snapshot and run
   `sprint_state.py progress`. Take `scopeDigest` from its output.
9. Write the brief from [the template](../assets/brief.md). Show it to the human with the digest.
   The human approves the exact brief and the exact digest. Then post the `sprint-brief` record.
   Reuse an approval that already names this target. Ask only for what is missing.

The human prioritizes the story work separately: Gate 1 moves each ticket from Backlog to Ready.
Preparing the sprint does not start it.

## Start and track

1. The human asks to start, and names the sprint and the digest.
2. Check that no other sprint is `active`.
3. Read `GET /sprint`. Build the snapshot. Run `progress`. If its `scopeDigest` is not the digest
   in the request, stop. The scope changed after the approval. Ask the human.
4. Post the `sprint-start` record. Read the sprint again for the new `rev`.
5. Build the snapshot again, with the `sprint-start` record in it. Run `sprint_state.py start`. It
   must print `START`. `START` is not proof that anything started.
6. Send `POST /sprint/start` with the digest from the request. Read the sprint again. Check
   `state: active`, `startedAt`, and that `startScope` is the approved member list.

Run the stories through `sdlc-ticket`, one at a time, in dependency order. While the vault is in
sprint mode, `sdlc-ticket` claims only from the active sprint. A sprint does not waive a gate.

On a progress request, and on resume, read the sprint and every member. Report:

- the goal, and the accepted and the total members. Show the stories apart from the closure task,
- the remaining and the blocked tickets,
- the scope changes since the start (`sinceStart` and `GET /metrics?sprint=`),
- the state of the closure task,
- any `historyGaps`, and `endScopeMismatch` on a closed sprint.

Counts describe tickets. They are not effort. Do not give a percent of effort or a date forecast.
Do not move a ticket, and do not change a priority, only because it is newly unblocked.

Reconstruct approvals and digests from the sprint's records, never from memory.

## Change the scope of an active sprint

A change into or out of an active sprint needs a `reason` of at least 10 characters. The board logs
it. Record the human's scope decision.

A change of the members changes the scope digest. A changed goal or dates change it too. The old
approval no longer matches. Post a new `sprint-brief` that restates the brief, names the digest it
supersedes and carries the new digest, after the human approved it. A new release criterion or a
material change of the solution also needs a new approval.

The board keeps the original member list as `startScope`. Removed work stays visible in the final
report because of it.

## Unfinished work

Never move unfinished tickets by silence. Never mark them Done to let a release pass. For each one,
decide with the human: finish it, or move it out. Move it with `POST /ticket/sprint`, to the planned
sprint or to the backlog, with a reason. A Done ticket cannot move out.

Removing a ticket from a sprint does not remove its merged code from the candidate. A release
needs the real code disposition: a revert, a switch that turns the feature off, or an agreed
partial release. Record it as the ticket's `note` in the report.

A correction to a Done story is a new ticket. Add it to the sprint with a reason, and keep its origin
in the report.

## Close a release sprint

The human's close request begins validation. It does not authorize an unchecked release.

1. Keep the sprint `active`. Keep the closure task in progress while you collect evidence.
2. Invoke `sdlc-sprint-review`. It freezes the candidate and the inputs, runs the four reviews,
   verifies the whole feature, and puts the report on the closure task's PR (`sdlc-sprint-review`
   `references/publication.md`).
3. Story acceptance alone does not make the release ready. Corrections use linked tickets and PRs.
   Before the approval, every retained story must be Done.
4. Show the human: the report and its HTML digest, the candidate SHA, the current brief digest and
   scope digest, the tag, the residual issues and the sprint key. A report that is incomplete or
   blocked does not become ready when you change its label.
5. Ask the named human to approve the release, the tag and the close for those targets. Post the
   `close-approval` record.
6. Publish and verify the tag (`release.md`).
7. Finish the closure task through the pipeline. Then read the sprint again and run
   `sprint_state.py close`. It must print `CLOSE`.
8. Read the sprint again. Send `POST /sprint/close` with the `scopeDigest` that the close approval
   names. The board refuses it (`close-scope`) if the members changed since. Read the sprint again.
   Post the `release` record with `event: closed`.

More work, a new budget or a new release scope needs the human's direction and a new run of the
affected checks. A changed candidate needs a new report and a new approval.

If the human closes the sprint in the board view, report the difference and the gates that are
missing. Do not infer a release approval.

## Close a planning-only sprint

1. The human asks to close, and names the sprint.
2. Every member is Done. Move the unfinished members out first, with a reason.
3. If the members changed after the approved brief, the human approves the new digest. Post the new
   brief.
4. The human approves the close for the exact scope digest. Post the `close-approval` record with
   `closeSprint: true`. Leave out the candidate, the report digest and the tag.
5. Run `sprint_state.py close`. It must print `CLOSE`.
6. Send `POST /sprint/close` with the `scopeDigest` that the close approval names. Read the sprint
   again. Post the `release` record with `event: closed`.

## After the close

At most one sprint is planned. It is the next sprint. A human request creates it. Unfinished work
that the human carried over is already in it.
