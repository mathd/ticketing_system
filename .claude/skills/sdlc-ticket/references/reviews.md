# Independent reviews — rounds, cap, refutations

Read this before any `agent:plan-review` or `agent:ai-review` work, and before any `sprint-review`
round (the `sdlc-sprint-review` skill). It is the single source for review mechanics: round
structure, when a round repeats, the cap, refutations and the round record. *Who* reviews is
`config.models` (SKILL.md § config.models); *how* to call the model is the runner doc. Triage
vocabulary and the review-guide are in `quality-practices.md` §2–3.

Adopted from the upstream sdlc-ticket review procedure (2026-10-16). It replaces the former
"second pass iff the fixes were non-trivial" rule, the churn cap, the convergence exception and the
docs-only merge-at-cap carve-out. The `sprint-review` stage was added on 2026-10-17 (§ Sprint
review). On 2026-11-09 the upstream scope rule, the bounded-review rules and the convergence rules
were adopted: finding origin, `priorFinding`, delta inspection, the approach check before round 3,
optional review guidance and the optional security pass.

## Inputs and identity

The reviewer's model must differ from **every** author of the artifact: the plan drafter for
`plan-review`; the implementer, and the author of every fix, for `ai-review`; the implementer and
the fix authors of every member ticket, the implementer of the closure task and the report author
for `sprint-review`. When you fixed
findings yourself, you are an author too — `main-agent` and a named model can be the same model
(SKILL.md § config.models). Different runner names, providers or effort settings do not prove
independence. Normalize model aliases to the actual full model ID before recording authors and invoking the contract. For example, `opus-5.5` and `claude-opus-5-5` are one identity. An unresolved identity blocks the call. Keep historical comments unchanged; record the normalized identity and its source in the new review record.

For a restarted implementation, retain all earlier rounds and their findings. Derive the author set from contributions to the reviewed artifact. An abandoned, unmerged implementation contributes no author only when none of its code or fixes is reused. Record that exclusion and its evidence. A copied fix makes its original author part of the current author set. This rule never resets a round cap.

The **initial** lens never sees author rationale:

- `ai-review`: the complete branch diff against the current default branch, the relevant code, the
  COS and the verification facts. No plan, no decision log. From round 2, the initial inspection
  can cover only the changes since the last reviewed revision (§ Delta inspection).
- `plan-review`: the proposed steps, scope, test strategy and COS. Withhold the rationale and the
  decision-log sections.
- `sprint-review`: the frozen release packet: the release criteria, the scope, the complete diff from
  the base to the candidate, the relevant code and the verification facts. No brief, no story plans,
  no decision logs.

Prime it for guilt: "assume the artifact is wrong and find where", not "check whether it is ok".

Also tell every lens to hold the artifact to the **smallest change that meets the COS**: report
code, tests, guards, options or abstractions that the COS do not need, with evidence of their cost
or of a requirement they break; report tests that repeat another test's assertion or test behaviour
the change does not touch; compare the diff with the plan's size estimate. "Fewer tests" never
covers the tests that this repository's rules require: one test per guard predicate, a mutation per
fixture, both readings of a green mutation (`AGENTS.md`). Do not report style preferences or
cleanup that has no concrete consequence; lint and the review guidance cover those.

**Review guidance (`config.review.guidance`, optional).** When it names a repository file, give it
to both lenses with its revision and digest. Use the version on the default branch. If the change
under review edits that file, give the default-branch version and say that the PR changes it.
Guidance can say what a failure looks like in this repository, which paths need less attention and
which conventions apply. It cannot waive a required check, remove a security path from review or
change this procedure. A configured file that cannot be read blocks review. Author rationale stays
audit-only.

## One round

1. **Freeze and identify the artifact.** `ai-review`: `base:<sha>/head:<sha>`. `plan-review`:
   `<TICKET-KEY>:plan:sha256:<digest>`, where the digest is the SHA-256 of the complete plan text
   you give the reviewer (draft with every amendment applied — `plan-final` stays a delta in the
   thread, but the reviewer gets the whole plan). `sprint-review`: `<SPRINT-KEY>:packet:sha256:<digest>`,
   where the digest is the SHA-256 of the body of the packet record (`stage=sprint-review
   kind=summary`) as the board returns it. Board comments carry no ID, so the digest is the
   identity; quote it in the stage comment. Record the actual author models.
2. **Initial inspection** in a fresh reviewer context — a new runner call, never a continued
   thread, so an earlier audit's rationale does not carry over. Require a complete result
   (§ Results). Post its findings before you expose any rationale.
3. **Decision audit** on the **same frozen artifact**. Give the reviewer the approved plan where
   applicable, every `kind=decision` comment, the initial findings and the verification evidence.
   It checks plan compliance, unsupported choices, omitted material decisions, and invalidated
   decisions with no superseding entry (`quality-practices.md` §2). For `plan-review`, it audits
   the draft against the governing ADRs and the shaping decisions, and in the last round also the
   plan recap (`plan-recap.md`): it must state the plan faithfully, keep every COS and add
   nothing. **Do not fix between the two
   lenses**; if the artifact changes, discard the incomplete round and review the new artifact.
4. **Validate both results, merge duplicate defects, triage every finding** against the reviewed
   revision (SKILL.md § Hard rules — verify against the revision under review). Keep the original
   severity and the evidence for each disposition. Check every `pre-existing` origin claim against
   the base revision before you defer on it (§ Pre-existing findings).
   Then compare the findings with this stage's earlier rounds. When a finding is the same defect as
   an earlier one, set `priorFinding` to `R<round>:<id>` and keep its history in one place rather
   than posting it again. When code changes or new evidence make an earlier resolution wrong, reopen
   the finding with `priorFinding` and the new evidence. Never give the initial lens the earlier
   dispositions as a suppression list; they go to the audit.
5. **Fix** accepted findings. § Stage policy decides what must be fixed, what can be deferred and
   what needs a refutation check. Refuted claims get evidence. Log a `kind=decision` when a fix
   makes a material choice. Fix with the smallest change, and fix every in-scope occurrence of the
   finding's `bugClass` (§ Keep review bounded).
   **Who fixes matters if another round follows:** the fixer becomes an author, and the next
   round's reviewer must differ from it. When the session model is the reviewer's model (for
   example `planReview: opus-5.5` in an Opus session), send the accepted findings back to the
   original author's model (`config.models.plan` / `implement`) to revise, rather than amending
   yourself.
6. **Verify** — gate, tests, mutation check on any test written for a fix (SKILL.md § Hard rules).
   Then apply § Stage policy. A fix that exceeds the finding's scope, or materially changes the
   approved plan, architecture, permissions or data handling, sets `materialChange: true` and
   requires another round regardless of severity.
7. **Record** the round (§ Round record) as a `kind=review` comment and run the contract script.
   Act on its output.
8. **Before you start round 3, stop and diagnose why findings continue.** Record in a
   `kind=decision` comment whether the remaining failures come from intent, plan or implementation,
   and whether to continue, reshape or stop (`quality-practices.md` §4). A material plan change
   returns to Planning and the plan gate. This check does not reset the round count, and its
   rationale stays out of the next initial lens.

Initial inspection plus audit is **one round**. A failed invocation gets the runner doc's
escalation ladder; invocation retries do not consume a round and never authorize a model
substitution. Research calls (§ Researcher) are not rounds.

## Stage policy

| Stage | Findings that require another full round after fixes |
|---|---|
| `plan-review` | Critical, High |
| `ai-review` | Critical, High |
| `sprint-review` | Critical, High |

Use the original normalized severity, never an author's reclassification. Findings below the
threshold still need a disposition and verification; their fixes are handed off **not re-reviewed**,
and the review-guide says so. A material change repeats the round at any severity.

**Severity says how bad a defect is; scope says whether this deliverable must fix it.** The
reviewer gives both: the severity, and whether the defect is in the behaviour the COS cover or
outside it.

- **Must fix, at any severity:** a security defect wherever it is, and a correctness defect in the
  behaviour the COS cover.
- **Incidental, at any severity:** a defect outside that behaviour — another code path, a rare case
  the COS do not name, a small performance cost, extra compatibility. Defer it to a linked Backlog
  ticket with a written scope reason. Keep its original severity. Never defer it "to final review":
  a deferral always names a ticket.
- A deferred finding at the repeat threshold still requires the next round, unless § Pre-existing
  findings exempts it.
- **Disagreeing with the reviewer's scope call on a Critical or High finding is an override.** In
  `gates: "autonomous"` it is a STOP (SKILL.md § Autonomous mode (d), `AGENTS.md`): escalate, never
  reclassify a reviewer's in-scope High as incidental yourself. Deferring a High the reviewer itself
  placed outside the COS is not an override.
- At `sprint-review`, a deferred Critical or High finding still blocks release readiness: the report
  contract treats every unresolved Critical or High risk as a blocker. Fix it, or remove its work
  from the release through the sprint scope-change process. A sprint never defers a story's required
  verification until closure.

Ticket reviews use this threshold to reduce repeat rounds for smaller corrections. It applies to
all tickets. In sprint mode, sprint review checks documentation, test quality and other issues
across the completed tickets through its four review areas (`sdlc-sprint-review`).

Why a fix diff deserves care even below the threshold: in TKT-22, 30 of 42 findings were defects in
code written to fix an earlier finding (`quality-practices.md` §2). Judge `materialChange` on the
**fix diff**, honestly — it is the only route to a re-review for a Medium fix.

### Pre-existing findings

A finding is `pre-existing` only when the defect is present at the **base revision** of the change:
the default-branch commit the branch starts from. A defect that the first reviewed revision
introduced is `introduced`, even if an earlier round missed it. The reviewer sets `origin` to
`introduced`, `pre-existing` or `unknown`. A `pre-existing` finding needs `originEvidence` that cites
the base revision with file and line, and `affectedByChange`: `true` when this change reaches,
exposes or worsens the defect.

A pre-existing finding that this change does not affect, that is marked `security: false`, and that
is deferred to a linked Backlog ticket does not start another `ai-review` round, whatever its
severity. Every other finding follows the table above. `unknown` origin or a missing `security` field
gets no exemption. A pre-existing security defect, or one in the behaviour the COS cover, is still a
required fix. At `sprint-review`, an unresolved Critical or High finding blocks release readiness
whatever its origin. You check the origin claim against the base revision (`git show
<base>:<path>`) before you defer the finding; an origin you did not check is `unknown`.

## Delta inspection

After a complete first `ai-review` round, the next initial inspection can cover only the changes
since the last reviewed revision, plus the callers, contracts and integrations those changes reach.
Set `inspection: "delta"` and `previousArtifact` to the artifact of the round before. The decision
audit stays complete: it sees the full effective diff, the plan and every decision.

A delta inspection needs `base:<sha>/head:<sha>` artifacts with the same base on both sides. Use a
full inspection when round 1 has not completed, when the previous round recorded
`materialChange: true`, when the base changed (a rebase onto a newer `origin/main`), or when earlier
round records are missing. `plan-review` and `sprint-review` always use full inspections. Pass the
earlier records with `--history` (§ Round record): the contract checks each one as a complete round
and checks the chain back to a full round 1.

## Security pass (`config.review.securityPass`, optional)

When `review.securityPass.enabled` is true, compare the changed paths with
`review.securityPass.paths` in each `ai-review` round. On a match, run a separate read-only security
inspection of the same frozen artifact before the audit, with a model independent of all authors.
Merge its findings into the initial result with their origin and `security: true`, and list their IDs
under `securityPass.findingIds`. The audit then sees all findings. Record `securityPass` on the
round: `required`, and when required, `complete`, `artifact`, `reviewer`, `summary` and `findingIds`;
when not required, a `reason`. The pass is not a third lens and has no round budget of its own.
Ticket review covers security in every round either way.

## Cap — four rounds per stage

Each stage (`plan-review`, `ai-review`) has its own budget of **four rounds per ticket**: the
number of **distinct `round` values** among that stage's `kind=review` comments. Not the number of
comments — A/B arms post one record each for the same round, and a record re-posted after a `FIX`
reuses its round number. A new session, a rebase, a reviewer retry, a renamed
PR or a model failure never resets it.

`sprint-review` has one budget of four rounds **per sprint**, not per ticket. The count is the
number of distinct `round` values among the `stage=sprint-review kind=review` records on the sprint
note, over all four areas. One round holds the four area pairs. So an area cannot use a round that
the other areas do not use, and a change of area never resets the count.

When round four still requires another review, the contract returns `STOP`: list the unresolved
findings and every unreviewed fix in a `kind=blocker` comment, keep the current `agent:*` label and
stop. **Only the human can authorize more rounds**, in a comment that names the stage and the new
limit; pass it with `--authorized-through N --authorization <comment ref>` (for a sprint, the key of
the note and the digest of the comment body). In `gates: "autonomous"`
this is a STOP condition (SKILL.md § Autonomous mode) — the agent never authorizes extra rounds for
itself, and silence or a timeout is not authorization.

## Refutations

A rejected finding below the repeat threshold needs recorded counter-evidence, without a reviewer
refutation check.

For a finding at the stage's repeat threshold that you reject, ask the **same reviewer** to check
the refutation against the **original frozen artifact** before deciding whether to repeat the round.
Give it the finding, your counter-evidence and the relevant code. Do not repeat the whole inspection
to verify one refutation. Record the answer under `refutationChecks[<finding id>]`:
`{artifact, reviewer, complete: true, refuted: <bool>, evidence}`. Keep the original finding and
severity. Only `refuted: true` waives the repeat, and only for that finding.

One refutation check per finding per round. Read the earlier `kind=review` comments first and
append the result, including `refuted: false`; never replace a refusal with another attempt. If the
reviewer does not confirm, fix the defect and follow § Stage policy, or stop for human direction on
the disagreement. A disputed rejection makes the contract return `STOP`. If the human supports the
rejection, record their exact instruction and run the next full round: the negative check stays in
its round, and the new reviewer result must resolve the finding explicitly.

## Researcher (`config.models.research`)

Optional. A cheaper model for bounded lookups: caller searches, test discovery, data-flow traces,
documentation lookups. It returns evidence, never findings, severities or verdicts. The reviewer
verifies any evidence it uses and owns the verdict. It may share a model with an author; it never
substitutes for the independent reviewer. Absent key: the reviewer researches by itself. Missing
research capacity never waives a review.

Our runners give a worker one brief and take one answer, so research is **orchestrator-mediated**:

1. The reviewer returns `researchRequests: [{id, question, scope}]` in its result, beside whatever
   findings it can already support. A result with open requests is not complete: `complete: false`.
2. You run the researcher once, with all the requests, under the **same context boundary as the
   current lens** — no rationale to a researcher serving the initial inspection. Read-only, tree
   check before and after (SKILL.md § Hard rules).
3. You re-run the reviewer in a fresh call with the same lens brief plus the research evidence. Its
   result must be complete; it may not ask again.

Judging these outputs (the runner docs' "completed ≠ ran" rule, applied per kind):

- **Reviewer result with open `researchRequests`** (`complete: false`): not a failed invocation and
  not a verdict. Valid when every request has an `id`, a question and a scope. Run step 2.
- **Researcher output:** never has a verdict. Valid when it answers **every** request ID with
  evidence (file:line, command and output) or an explicit `not found` plus what was searched. An
  empty, truncated or partial answer is a failed invocation — the runner's escalation ladder
  applies.
- **Completed reviewer result:** as in § Results. `scripts/review_round.py` refuses a complete
  result that still carries research requests.

One research cycle per lens. Whether a T3 child can delegate to the researcher itself is **not
verified**; do not rely on it until a probe shows it. Record the researcher's model and effort in
the round record (`researcher`) and the stage comment.

## Results

The reviewer returns one JSON object per lens, shaped like `assets/review-result.example.json`:
`stage`, `round`, `lens` (`initial` | `audit`), `artifact`, `reviewer` (actual model),
`complete`, `verdict` (`APPROVE` | `REVISE` | `BLOCKED`), `summary`, `findings`, and optionally
`researchRequests`. Each finding has `id`, `severity`, `evidence`, `impact`, `correction`. Canonical
severities are `Critical`, `High`, `Medium`, `Low`, `Nit`; other labels map through
`config.review.severityMap`, and an unknown label stops triage — never shift a numbering by
assumption. Finding IDs are unique across both lenses; the audit references a duplicate rather than
repeating its ID.

Tell the reviewer to give, for each finding:

- `evidence` with file and line at the reviewed revision, or the exact plan or document section —
  never a guess from a name;
- `origin` (`introduced`, `pre-existing` or `unknown`), and for `pre-existing` also
  `originEvidence` at the base revision and `affectedByChange` (§ Pre-existing findings);
- `bugClass`: the defect class in one line, with the other occurrences it found in the change;
- `security`: `true` for a security defect, `false` otherwise;
- for a defect fix under review: whether the correction removes the cause of the failing mechanism,
  not only the symptom.

You add `priorFinding` during triage (§ One round step 4). Start `summary` with the number of
findings; with none, write `0 findings. Nothing to fix.`

A `sprint-review` result also carries `area` (`implementation`, `verification`, `documentation` or
`retrospective`), and so does its record. Prefix finding IDs with the area (`IMPL-1`, `VER-1`,
`DOC-1`, `RETRO-1`).

`APPROVE` cannot carry a Critical, High or Medium finding. `BLOCKED` always stops. An empty or
truncated result, a wrong revision or a missing verdict is a failed invocation, whatever the exit
status (the runner docs' "completed ≠ ran").

## Round record

Assemble it after both lenses, shaped like `assets/review-round.example.json`: `stage`, `round`,
`area` (`sprint-review` only), `artifact`, `inspection` (`full` | `delta`) and, for a delta,
`previousArtifact`, `reviewer`, optional `researcher`, optional `arm` (A/B only), `results` (the two lens objects, in order),
`dispositions` (`fixed` | `rejected` | `deferred` per finding ID), `dispositionEvidence` (fix SHA and
verification, refutation evidence, or follow-up ticket and scope reason), `verificationPassed`,
`materialChange`, `refutationChecks`, and `securityPass` when `config.review.securityPass` is
enabled. Carry still-unresolved findings from earlier rounds into the new results; a new round
cannot hide them by omission. Keep each round's complete record in its comment: a later delta round
validates against it.

Post it as `<!-- sdlc:stage=<plan-review|ai-review|sprint-review> kind=review -->` with the record
in a JSON code block (a `sprint-review` record goes on the sprint note, the others on the ticket),
then run, from the repository root (comments are append-only: after a `FIX`, post the completed
record again with the **same** `round` — for one stage, round and arm (or area), the latest record is
the one that counts):

```sh
python3 .claude/skills/sdlc-ticket/scripts/review_round.py review .review-<ticket>-<stage>-r<n>.json \
  --config .claude/sdlc.config.json --artifact <expected artifact> \
  --reviewer <actual reviewer model> --author <actual author> [--author <another author>] \
  [--area <implementation|verification|documentation|retrospective>] \
  [--history .review-<ticket>-<stage>-history.json]
```

`--history` takes a JSON array of this stage's earlier round records, rounds 1 to N-1, copied from
the `kind=review` comments (the latest record of each round). It is required for a delta inspection
and checks every `priorFinding` reference when given. A record cannot prove its own history. The
script normalizes the known Claude aliases (`opus-5.5` = `claude-opus-5-5`), a provider prefix and
an `:effort` suffix before it compares the reviewer with the authors; it cannot prove which model
actually ran.

`--area` is required for a `sprint-review` record. It must equal the `area` on the record and on both
lens results.

Write the record with the Write tool to `.review-<ticket>-<stage>-r<round>.json` in the worktree
root — `.gitignore` covers `.review-*.json` — and delete it after the comment is posted. The expected
artifact, reviewer and authors come from the frozen artifact and the runner's own record (job file,
T3 result), never from the returned result. Read the output, not just the exit code:

| Output | Exit | Action |
|---|---|---|
| `HUMAN_REVIEW` | 0 | Finish the handoff checks, post the review-guide, continue to the gate. Not human approval |
| `FIX` | 1 | Complete missing dispositions or verification, then evaluate again. Not a new AI round |
| `REVIEW_AGAIN` | 1 | Run the next round (fresh initial inspection, then audit) after the required fixes |
| `STOP` | 1 | Keep the stage label, post `kind=blocker` with the unresolved items, ask the human |
| `FAIL: <reason>` | 1 | The record or its inputs are invalid. Correct them; it is not a verdict |

Never retry a valid `FIX`, `REVIEW_AGAIN` or `STOP` as if the script had crashed.

## Keep review bounded

Review loops grow a deliverable one cheap-looking fix at a time. Keep it at the size the COS need:

- Fix an accepted finding with the smallest change. Prefer a fix that deletes or narrows code to a
  new lock, cache, helper, option or abstraction.
- Fix every occurrence of the finding's `bugClass` in the reviewed change that has evidence and is
  in scope, so the next round does not find the next one. Record occurrences outside the change as
  follow-up tickets. Do not add guards for hypothetical cases.
- Do not accept a finding only to end the loop. Reject it with evidence or defer it with a scope
  reason.
- Record `git diff --shortstat <base>` at the first reviewed revision and at the final revision, code
  and test lines apart, beside the plan's estimate. If review grew the diff, look for what to cut
  before the hand-off.

## Sprint review — four areas, one round

`sprint-review` validates a whole release. The `sdlc-sprint-review` skill owns the missions of the
four areas, the packet and the report. This section holds the review contract.

- **One round is four area pairs.** Each of `implementation`, `verification`, `documentation` and
  `retrospective` runs its own initial inspection and decision audit on the **same** frozen packet.
  All four share one `round` number and one packet identity. The reviewer may be the same model for
  several areas, each in its own context.
- **One record per area.** Check each one:

  ```sh
  python3 .claude/skills/sdlc-ticket/scripts/review_round.py review .review-S-3-sprint-review-r1-implementation.json \
    --config .claude/sdlc.config.json --artifact <packet identity> --reviewer <actual model> \
    --author <actual author> --area implementation
  ```

- **One aggregate.** Save the four records of the round as one JSON array, shaped like
  `assets/sprint-round.example.json`. Then run:

  ```sh
  python3 .claude/skills/sdlc-ticket/scripts/review_round.py sprint-round .review-S-3-sprint-review-r1.json \
    --config .claude/sdlc.config.json --artifact <packet identity> \
    --reviewer implementation=<model> --reviewer verification=<model> \
    --reviewer documentation=<model> --reviewer retrospective=<model> \
    --author <actual author> [--author <another author>]
  ```

  Repeat `--author` for every author of the code, of the packet and of the report. Take the expected
  values from the frozen packet and from the runners' own records, never from a returned result. The
  command needs exactly four areas, one round, one packet, a complete pair for each area and a
  reviewer that is not an author. It returns the most restrictive decision of the four: `STOP`, then
  `FIX`, then `REVIEW_AGAIN`, then `HUMAN_REVIEW`. Exit 0 allows the hand-off checks only. Extra rounds
  use `--authorized-through` and `--authorization` as `review` does.
- **One shared cap.** The sprint has four rounds in all (§ Cap). `STOP` at round four blocks the
  sprint's closure until the human authorizes more rounds.
- **A failed or missing area blocks the hand-off.** A `FAIL:` is a tool error, not a verdict. An area
  that did not run is not an approval.

## A/B review arms

When `aiReview` is an array (`ab-stages.md`), every arm runs round 1 on the same frozen artifact,
each with its own initial and audit lens and its own round record (an `arm` field naming the arm's model, finding IDs prefixed by arm). All arms' records carry `round: 1` and consume one round. The
stage acts on the most restrictive output across arms: `STOP` before `FIX` before `REVIEW_AGAIN`
before `HUMAN_REVIEW`. Rounds 2 and later run on the first array element only. A/B arms do not apply to `sprint-review`:
`models.sprintReview` is one value, or one value for each area.

## Validating this procedure

The contract's tests run without external services:

```sh
python3 -B -m unittest discover -s .claude/skills/sdlc-ticket/tests -v
```

They are not part of `make check`. Run them after any edit to `scripts/review_round.py`, and run a
mutation of the edited rule to confirm a test goes red.

These tests check record shape and the decision rule, not review quality. Fewer review rounds alone
is not evidence of a better review. Judge a change to this procedure, a reviewer prompt or a reviewer
model by the defects it caught or missed: a reverted merge, an incident, or a later finding that an
earlier round should have raised. Record such a miss in the ticket's closeout as a defect class with
a proposed control (SKILL.md § Memory & metrics).
