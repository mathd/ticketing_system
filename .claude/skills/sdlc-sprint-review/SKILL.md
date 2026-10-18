---
name: sdlc-sprint-review
description: Validate a complete sprint outcome, with or without a Git tag with four independent area reviews, real integration and LLM evidence, a cross-story retrospective, and a French or English HTML report that is committed to the repository. Use when you prepare a sprint closure or review a release candidate. Requires the sdlc-ticket and sdlc-sprint skills.
---

# Sprint release review and report

Validate the combined outcome of the release. Produce the report that the human reads. With an approved `continuation: "autonomous"` brief, invoke this skill as soon as all functional members are Done. With `continuation: "manual"`, wait for the owner request. A manual close request also invokes it through `sdlc-sprint`. This skill does not start or close a sprint, merge product code,
publish a Git tag or deploy.

## Inputs and ownership

Read these files first. They are in `.claude/skills/`. A missing file is a blocker.

- `sdlc-ticket/SKILL.md` (§ config.models and § Hard rules), `references/reviews.md` and
  `references/quality-practices.md`, and the runner file for each reviewer
  (`references/t3-runner.md` or `references/codex-runner.md`).
- `sdlc-sprint/SKILL.md` and its `references/board.md` and `references/records.md`.

Read from the board: the sprint (vault and key) with its complete member list, the approved brief, the
closure task, and the closeout (`kind=metrics` comment) of every member **except the closure task**:
its closeout comes after the release, so read its current evidence instead and record its closeout
when the release is done. Read the actual candidate code.

You, the primary agent, gather the evidence, run the tests and measurements that were authorized,
render and commit the report, and talk to the human. Reviewers have read-only access and return
findings. The actual model of a reviewer must differ from every author of the work it reviews. The
report author is one of them, and it is you. Do not let the writer of the report approve it.

## Freeze the review packet

The packet is one text. Collect these facts for it:

- the sprint (vault and key), the closure task and the approved release criteria,
- the original scope (`startScope`), the current members and the exclusions with their destinations,
- the base and the exact candidate commit, and every change between them,
- the identity of every author,
- the prompts, the model versions, the runtime configuration, the datasets and the other material
  inputs, each with a version or a digest. Say which inputs are not pinned.

Run these on the candidate. Do it before you post the packet, because the packet holds the results:

- the affected integration checks,
- the broader critical user journeys,
- the real LLM evaluations, where they apply,
- the local gate (`config.code.localGate`). The file named by `config.code.gateVerdict` is the
  authority.

Record the commands, the results, the run receipts, the fixtures, the costs and the checks that are
missing. Stay inside the budget and the environments that the brief approved. A missing suite needs
planned work to create it. A required check that was skipped, failed or could not run prevents
readiness.

Then post the packet as a `sprint-review` / `summary` record on the sprint note. Its identity is
`<S-n>:packet:sha256:<digest>`. The digest is the SHA-256 of the record's body as the board returns
it. Give the reviewers that same body. A material candidate or input change needs a new packet and round. After a permitted non-material correction below the repeat threshold, preserve the original packet and append a correction-verification record naming both commits, the exact correction diff digest and fresh required check receipts. Do not rewrite the reviewed packet.

## Specialized review

Prepare four separate briefs. Each has its own mission.

| Area | Instructions |
|---|---|
| `implementation` | [Whole-feature code and architecture](references/implementation.md) |
| `verification` | [Test relevance, integration, LLM evaluations and measurements](references/verification.md) |
| `documentation` | [Documentation and report fidelity](references/documentation.md) |
| `retrospective` | [Cross-story learning and ADR or process candidates](references/retrospective.md) |

**Who reviews.** Resolve the reviewer of each area:

- `config.models.sprintReview` is an object with the four areas: use the value of the area.
- It is one value: use it for all four areas.
- It is absent: use `config.models.aiReview`. If that is an array, stop and report. Ask for a
  `sprintReview` value.
- An array inside `sprintReview` is a configuration error. Stop and report.

A value follows the table in `sdlc-ticket/SKILL.md` § `config.models`: the runner for the value, the
`:effort` suffix, and `high` when there is no suffix. One independent model can serve several areas,
each in its own context. Before the first call, check every area. Its model must differ from:

- `models.implement`,
- every author contributing member code or fixes to the reviewed candidate (read the stage comments and trace the actual changes; apply `sdlc-ticket/references/reviews.md` § Inputs and identity for discarded attempts),
- the implementer of the closure task,
- you, the report author.

`main-agent` and a named Claude model can be the same model. If an area fails the check, stop and
report. Never substitute a model.

**How a lens runs.** For each area, run the initial inspection in a fresh context. It gets the
packet and no author rationale. Then run the decision audit on the same frozen packet. It also gets
the approved brief, the member plans, the effective decisions and the initial findings. Use a new
runner call for each lens. Name the stage, the sprint, the round, the area and the lens in the call:
for T3, `S-3 sprintReview round 1 implementation (initial)`; for a brief file,
`.brief.S-3-sprintreview-r1-implementation-initial.md`. Researchers can give bounded evidence. They
never approve.

**The result.** Each lens returns one JSON object, as `sdlc-ticket/assets/review-result.example.json`
shows. Set `stage` to `sprint-review` and `area` to the area on the record and on both lens results.
Set `artifact` to the packet identity. Prefix every finding ID with the area (`IMPL-1`, `VER-1`,
`DOC-1`, `RETRO-1`).

Run implementation, verification and retrospective concurrently where the runner supports it. Documentation initial inspection can run concurrently. Run its decision audit after the other three areas have final results and the report draft is assembled. Give it those results and the source and HTML draft as audit supplements, while both documentation lenses retain the same frozen product packet. Do not edit that packet or the candidate while any lens is running. Record the supplement digests in the documentation area record.

**One round is four pairs.** A round holds the four complete area pairs on one packet. It has one round
number. The cap is four rounds for the whole sprint. Count the distinct `round` values among the
`sprint-review` / `review` records on the sprint note. A session change, a rebase, a model failure or
an area change never resets it. A failed or missing area blocks the hand-off.

Post one `sprint-review` / `review` record for each area (`sdlc-ticket/references/reviews.md` §
Round record, with `area`). After a `FIX`, post the completed record again with the same round and the
same area. The latest record of a round and an area is the one that counts. Write each record to
`.review-S-3-sprint-review-r1-<area>.json` and the four of a round to one array in
`.review-S-3-sprint-review-r1.json`. `sdlc-ticket/assets/sprint-round.example.json` shows a full round.
Both names match `.gitignore`. Validate them as below, post the records, and only then delete the
files:

```sh
python3 .claude/skills/sdlc-ticket/scripts/review_round.py review .review-S-3-sprint-review-r1-implementation.json \
  --config .claude/sdlc.config.json --artifact <packet identity> --reviewer <actual model> \
  --author <actual author> --area implementation
python3 .claude/skills/sdlc-ticket/scripts/review_round.py sprint-round .review-S-3-sprint-review-r1.json \
  --config .claude/sdlc.config.json --artifact <packet identity> \
  --reviewer implementation=<model> --reviewer verification=<model> \
  --reviewer documentation=<model> --reviewer retrospective=<model> --author <actual author>
```

Repeat `--author` for every author of the code, of the packet and of the report. Take the expected
values from the frozen packet and from the runners' own records, never from a returned result. After a
human authorization for extra rounds, add `--authorized-through N` and `--authorization` with the
reference of the human's comment (the note key and the digest, for example `"S-3 sha256:<digest>"`).
`sprint-round` checks the four areas, one round and one packet, independent reviewers
and the combined decision. The combined decision is the most restrictive of the four: `STOP`, then
`FIX`, then `REVIEW_AGAIN`, then `HUMAN_REVIEW`. Act on the output:

- `STOP` (or `FAIL:`) blocks. Post a `sprint-review` / `blocker` record. Ask the human.
- `FIX` needs verified dispositions. `REVIEW_AGAIN` needs another full sprint round.
- `HUMAN_REVIEW` allows the hand-off checks. It is not a human approval.
- An area that did not run is not an approval.

**Fixes.** A defect in correctness, in security or in release acceptance must be fixed. An
incidental improvement becomes a follow-up ticket (`POST /ticket/new`, with a minimal context memo). A product correction to a Done story needs a new member ticket. Prepare the correction and revised brief, then stop for owner approval of the new scope and priority before adding or starting it. The initial named-scope approval does not cover extra tickets. Report-only factual fixes stay on the closure task.
A Critical or High finding, or a material change, needs another full round. A Medium correction needs
verification only. Use the refutation and cap rules of `reviews.md`, and keep earlier refusals.
Compare the whole diff from the reviewed packet to the final candidate with the exact diff of each
correction. Any further merge or change of material input needs another round. Old evidence never proves new checks on another candidate. A permitted correction amendment must name the reviewed and final candidates and contain fresh check receipts for the final candidate.

## Report and hand-off

Use the [report format](references/report-format.md) and the
[publication rules](references/publication.md). `sprints.reportLanguage` selects `fr` or `en`
(default `en`). Write the narrative in that language. Do not suggest that the renderer translates it.

Put these in the report:

- the release outcome,
- the planned, delivered and deferred work,
- the conclusion of every area,
- the tests and the measurements,
- the unresolved risks and the follow-ups,
- the retrospective selections and the observed cost of implementation, reviews, verification, CI wait and unexplained idle; put these observations in `retrospective.summary`, give the data cutoff and exclude closure-task work after that cutoff; use `n/a` when timing cannot be separated,
- the exact candidate,
- the actual state of the human decision and of the tag.

Keep the technical readiness apart from the release approval. Check that the source and the HTML show
all four review results faithfully. A material factual change needs the affected review again.
Assembling the report faithfully does not reset the round budget.

Commit the verified report under `docs/sprints/<S-n>/` and record its path, its commit and its
digests in the sprint note and on the closure task. Return to `sdlc-sprint`: the commit, the path, the
two digests, the candidate, the four area results, the readiness, the blockers and the choices that
the human must make. The human reads this report and approves the exact release target before any tag
or close.

Run the tests of the renderer after any edit of it, and break the rule you edited in a copy to see a
test fail:

```sh
python3 -B -m unittest discover -s .claude/skills/sdlc-sprint-review/tests -v
```

They are not part of `make check`.

## Full review without a tag

`review: "full", release: false` runs this entire review and publication procedure. Set
`release.publishTag: false`, `release.tag: null` and `decision.tagPublication.status: "not_required"`
in the report source. Keep candidate, report and close approval targets. Do not invent a tag or
skip the report because the sprint publishes none. The final close approval still belongs to the owner.

Freeze the product candidate before report publication. A report-only merge can follow it. Verify
that intervening changes are only the cited report artifacts, including an inert renderer runtime archived under the report evidence directory when needed; a production change needs a new packet.
