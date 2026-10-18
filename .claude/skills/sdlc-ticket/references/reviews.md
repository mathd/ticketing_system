# Independent reviews — rounds, cap, refutations

Read this before any `agent:plan-review` or `agent:ai-review` work, and before any `sprint-review`
round (the `sdlc-sprint-review` skill). It is the single source for review mechanics: round
structure, when a round repeats, the cap, refutations and the round record. *Who* reviews is
`config.models` (SKILL.md § config.models); *how* to call the model is the runner doc. Triage
vocabulary and the review-guide are in `quality-practices.md` §2–3.

Adopted from the upstream sdlc-ticket review procedure (2026-10-16). It replaces the former
"second pass iff the fixes were non-trivial" rule, the churn cap, the convergence exception and the
docs-only merge-at-cap carve-out. The `sprint-review` stage was added on 2026-10-17 (§ Sprint
review).

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
  COS and the verification facts. No plan, no decision log.
- `plan-review`: the proposed steps, scope, test strategy and COS. Withhold the rationale and the
  decision-log sections.
- `sprint-review`: the frozen release packet: the release criteria, the scope, the complete diff from
  the base to the candidate, the relevant code and the verification facts. No brief, no story plans,
  no decision logs.

Prime it for guilt: "assume the artifact is wrong and find where", not "check whether it is ok".

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
   severity and the evidence for each disposition.
5. **Fix** accepted findings. A correctness, security or COS defect in the current deliverable
   must be fixed — never deferred, whatever its severity. Incidental issues become linked Backlog
   tickets; refuted claims get evidence. Log a `kind=decision` when a fix makes a material choice.
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

Ticket reviews use this threshold to reduce repeat rounds for smaller corrections. It applies to
all tickets. In sprint mode, sprint review checks documentation, test quality and other issues
across the completed tickets through its four review areas (`sdlc-sprint-review`).

Why a fix diff deserves care even below the threshold: in TKT-22, 30 of 42 findings were defects in
code written to fix an earlier finding (`quality-practices.md` §2). Judge `materialChange` on the
**fix diff**, honestly — it is the only route to a re-review for a Medium fix.

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

A `sprint-review` result also carries `area` (`implementation`, `verification`, `documentation` or
`retrospective`), and so does its record. Prefix finding IDs with the area (`IMPL-1`, `VER-1`,
`DOC-1`, `RETRO-1`).

`APPROVE` cannot carry a Critical, High or Medium finding. `BLOCKED` always stops. An empty or
truncated result, a wrong revision or a missing verdict is a failed invocation, whatever the exit
status (the runner docs' "completed ≠ ran").

## Round record

Assemble it after both lenses, shaped like `assets/review-round.example.json`: `stage`, `round`,
`area` (`sprint-review` only), `artifact`, `reviewer`, optional `researcher`, optional `arm` (A/B only), `results` (the two lens objects, in order),
`dispositions` (`fixed` | `rejected` | `deferred` per finding ID), `dispositionEvidence` (fix SHA and
verification, refutation evidence, or follow-up ticket and scope reason), `verificationPassed`,
`materialChange`, `refutationChecks`. Carry still-unresolved findings from earlier rounds into the
new results; a new round cannot hide them by omission.

Post it as `<!-- sdlc:stage=<plan-review|ai-review|sprint-review> kind=review -->` with the record
in a JSON code block (a `sprint-review` record goes on the sprint note, the others on the ticket),
then run, from the repository root (comments are append-only: after a `FIX`, post the completed
record again with the **same** `round` — for one stage, round and arm (or area), the latest record is
the one that counts):

```sh
python3 .claude/skills/sdlc-ticket/scripts/review_round.py review .review-<ticket>-<stage>-r<n>.json \
  --config .claude/sdlc.config.json --artifact <expected artifact> \
  --reviewer <actual reviewer model> --author <actual author> [--author <another author>] \
  [--area <implementation|verification|documentation|retrospective>]
```

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
