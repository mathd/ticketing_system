# Sprint lifecycle

Start and close are human requests in both gate modes. The agent runs one only when the human's
request names the sprint. A short run request can approve the prepared brief and scope by
reference, under `sdlc-sprint/SKILL.md` § Short run requests. Record the exact digests even when the
owner does not type them. Dates, the last Done ticket and a label never start or close a sprint.

## Choose review and publication separately

- `review: "full"` is the default for implementation sprints. It requires a closure task, four
  complete review areas and a verified committed HTML report, with or without a tag.
- `review: "none"` is an explicit owner-approved exception. Record `reviewReason` in the brief.
  It has no closure task and cannot publish a tag.
- `release: true` publishes the approved Git tag after full review and exact owner approval.
- `release: false` publishes no tag. It does not skip review or the report.

A changed review policy, budget or tag plan needs an approved brief amendment. An old brief with
no `review` value needs that amendment before start or close. Historical closed records stay intact.

## Prepare

Start from the human's theme and from the outcome that people can observe.

1. Read `GET /sprints`. At most one sprint is `planned` and one is `active`. If a planned sprint
   exists, use it. Never make a second one.
2. Create the sprint with `POST /sprint/new`. Give a name, a goal and the planned dates that the
   human agreed, each with an offset.
3. Select vertical stories in dependency order. Do not take a whole epic with no bound. An epic can
   span several sprints. Record the exclusions, the acceptance criteria, the useful subsets and the
   shared architecture choices. For full review, record the base SHA, named approver and verification budget. Record a proposed tag only when `release: true`.
4. Assign each ticket with `POST /ticket/sprint` (`board.md`). A reason is optional while the sprint
   is planned. A member is a Story, a Task, a Bug or a Spike. Assign the children of an epic, never
   the epic. A Backlog ticket can be a member. Gate 1 and the blocker check still decide when a
   ticket can be claimed.
5. Look at dependency and package updates at every opening. Select the justified updates in one
   scoped maintenance ticket, or record that none is justified. Do not upgrade everything. Do not
   make an empty maintenance ticket. An urgent update can join an active sprint, with a reason.
6. Plan missing real-integration and end-to-end suites as tickets in the sprint. They are work, not
   wishes.
7. For `review: "full"`, create the closure task:
   - `POST /ticket/new` with `type: "Task"`. Its description holds the criteria: the report is
     committed, the release evidence is complete, the exact close approval is recorded, and any planned tag is verified.
   - Shape it like any ticket (`sdlc-ticket` `references/shaping.md`).
   - Assign it to the sprint.
8. Read `GET /sprint` again. Check the members against the plan. Build a snapshot and run
   `sprint_state.py progress`. Take `scopeDigest` from its output.
9. Write the brief from [the template](../assets/brief.md). Show it to the human with the digest.
   The human can approve those exact targets explicitly or by a short run request that resolves
   to this prepared brief and matching scope. Then post the `sprint-brief` record. Reuse an
   approval that already names this target. Ask only for missing information, with a recommendation.

The human prioritizes every member, including the closure task. The owner may approve the exact named scope once with `sprint-priority`; the agent records each Ready transition only when the DoR and blockers pass.
Preparing the sprint does not start it.

## Start and track

1. Read the named sprint and resolve the request. For a short run request, select the latest
   unambiguous current prepared or approved brief on its note, including a clearly identified
   preparation summary. Respect amendments and supersession; never use an older proposal in
   place of a newer approved brief.
   Retain that comment's returned body and digest. It must contain enough information to form the
   approved brief; do not invent missing choices. Ask with a recommended resolution for gaps.
2. Check that no other sprint is `active`. If the named sprint is already active, resume it using
   its actual state and records; do not call start again. A closed sprint cannot be restarted.
3. Build the snapshot and run `progress`. Compare the complete members and scope digest with the
   prepared brief and any explicit targets in the request. A mismatch needs an owner decision.
   Do not silently approve the board's changed scope. Report the difference and recommend what to do.
4. Record the owner's words, identity and request time, the source brief reference and the exact
   resolved targets. A run request approves the prepared brief and all listed member priorities.
   Post the whole `sprint-brief`, including the requested continuation, and read its returned body
   and digest. Reuse an existing approval only if it covers the same effective brief and scope.
   Post `sprint-priority` bound to that brief digest, scope and sorted member list, and read it back.
   A bare start request still needs explicit priority approval. State the scope, continuation and
   `config.gates` in commentary; a matching short run request needs no second confirmation.
5. For a planned sprint, post `sprint-start` bound to the approved brief digest. Read the sprint
   again for the new `rev`. For an active sprint, record this run or resume authorization in a
   `sprint` / `summary` comment and preserve the original start record and timestamps. If earlier
   approval records are missing, record only what this request now approves; do not backdate them.
6. For a planned sprint, if the approved continuation is autonomous and any member is Backlog or
   BLOCKED, require a matching `sprint-priority` record. This does not move that member yet.
   Build the snapshot again, with the `sprint-start` record in it. Run `sprint_state.py start`. It
   must print `START`. `START` is not proof that anything started.
7. Send `POST /sprint/start` only for a planned sprint, with the approved resolved scope digest.
   Read the sprint again. Check
   `state: active`, `startedAt`, and that `startScope` is the approved member list.

Run the stories through `sdlc-ticket`, one at a time, in dependency order. Preparation alone is not
Gate 1 approval. An explicit named-scope priority approval or a resolved short run request supplies
that decision; record `sprint-priority`. Reuse it only for those members after fresh readiness and
blocker checks. Otherwise ask for the missing priority decision with a recommendation. While the vault is in
sprint mode, `sdlc-ticket` claims only from the active sprint. A sprint does not waive a gate.
`config.gates` still controls who takes ticket gates, including the closure task's gates. A run
request does not turn human gates into autonomous gates.

On a progress request, and on resume, read the sprint and every member. Report:

- the goal, and the accepted and the total members. Show the stories apart from the closure task,
- the remaining and the blocked tickets,
- the scope changes since the start (`sinceStart` and `GET /metrics?sprint=`),
- the state of the closure task,
- any `historyGaps`, and `endScopeMismatch` on a closed sprint.

Counts describe tickets. They are not effort. Do not give a percent of effort or a date forecast.
Being newly unblocked does not authorize a Ready move. Apply `sdlc-ticket/references/sprints.md` § Gate 1 from a named owner decision. A recorded approval of the exact scope permits the owner's move only after fresh DoR and blocker checks.

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
in the report. Before adding that ticket, prepare its scope and revised brief, then ask the owner to approve the added member and its priority. This is a scope change, even during autonomous continuation. The original named-member priority approval does not cover it. Do not change Done product code in the report PR to avoid this gate. A correction receipt records verification; it does not authorize a new member.

## Final validation and close with full review

1. With `continuation: "autonomous"`, as soon as all functional members are Done, keep the sprint active and start the closure task.
   This continuation needs no further owner prompt. With `continuation: "manual"`, wait for the owner request.
2. Invoke `sdlc-sprint-review`. Freeze the candidate and material inputs, run the four review pairs,
   verify the combined outcome and merge the report PR after its required `check` passes.
3. Resolve findings under the existing repeat policy. Do not defer a correctness, security or
   acceptance defect to close the sprint. Reuse check evidence only for the exact artifact and
   environment it proves. The required pre-push and post-merge gates still apply.
4. Build the closure snapshot from the actual review records and committed report read-back.
   Require all four areas on one packet and round, a verified current report pointer, and a ready
   candidate. Missing review or report evidence blocks even when `release: false`.
5. Show the human the committed HTML path and digest, candidate SHA, brief digest, scope digest,
   sprint key, checks and residual issues. Ask for the exact close approval. Name a tag only when
   the brief plans one. Keep the closure task in progress while this approval is pending.
6. Post the `close-approval`. With `release: true`, publish and verify the approved tag under
   `release.md`. With `release: false`, record no tag publication.
7. Complete closure-task PO validation and metrics, then move it to Done. Its four review pairs
   replace an extra generic AI code-review round. Its report PR still needs the normal gate and CI.
8. Read the sprint again, build the snapshot and require `sprint_state.py close` to print `CLOSE`.
   Then send `POST /sprint/close` with the approved scope digest and fresh revision. Read back
   `closed`, the close digest and `endScope`, and post the `release` record with `event: closed`.

A material candidate or input change needs a fresh packet and round. A permitted correction below the repeat threshold needs a verified amendment and fresh checks under the review policy. Either changed candidate needs a new close approval. A report-only
merge does not change the frozen product candidate. Verify its diff contains only report artifacts. A renderer runtime archived under `docs/sprints/<S-n>/evidence/` for reproducibility is report evidence, not a change to production code or the repository's active renderer.
A production change during report publication invalidates that hand-off.

If the human closes in the board view, report missing gates. Do not infer review or approval.

## Close an explicitly approved no-review sprint

Every member must be Done. Require the owner-approved `review: "none"` brief and `reviewReason`,
then record close approval for the exact brief and scope. Leave candidate, report and tag targets
out. Run the local checker, close through the board and read the result back. This exception does
not allow a Git tag. Moving unfinished work out still requires the human's agreement.

## After the close

At most one sprint is planned. It is the next sprint. A human request creates it. Unfinished work
that the human carried over is already in it.
