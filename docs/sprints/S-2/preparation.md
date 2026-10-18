# S-2 preparation

Prepared on October 18, 2026. This is the proposed start brief. S-2 remains planned. No ticket was
prioritized or claimed by this preparation. The owner has approved the dates, full final review,
HTML report, no tag, the TKT-399 review extension and the host-Chrome measurement limit.
The exact scope and this brief still need start approval. The owner approved the one-record named-scope priority mechanism in S-2 D1. The start prompt gives the actual priority approval for these five members. Record it as `sprint-priority`, bound to the approved brief and scope digests and the exact member list. Record each Ready move only after fresh DoR and blocker checks. Preparation does not approve or start the sprint.

Planned dates are October 19 at 09:00 Toronto time through November 2 at 17:00 Toronto time.
The end uses UTC−05:00 after the November clock change.

The scope digest is `475b7c77f414e3205adb736c856fb913675f56cbdd7f58603ca28b46ae061186`. The baseline is `5e3bee1db8019621fb5cdc343872b9c6159dd50d`.
There are four functional tickets and one closure task.

| Order | Ticket | Outcome | Current dependency |
|---|---|---|---|
| 1 | TKT-559 | Timed Entry, two clocks, late/stalled response handling and host strict-commit measurements | TKT-558 is Done |
| 2 | TKT-399 | Re-planned Pass Exit mode on the new core and time contract | TKT-559 must be Done |
| 3 | TKT-551 | Remaining aggregate rejection count and mixed-result regression evidence | TKT-558 is Done |
| 4 | TKT-552 | Entry pass-denial instructions in the current presenter | TKT-558 is Done |
| 5 | TKT-563 | Full final review, committed English HTML report and exact close request | All four functional members |

TKT-551 and TKT-552 can run before Exit if blocked work makes that useful. They no longer depend
on the abandoned Exit branch. Re-read changed seams at claim. Do not start two writers on the scanner.

## Readiness and binding decisions

All eight readiness questions are filled on each member. Their code premises and test paths were
checked on the baseline. TKT-399 stays BLOCKED until TKT-559 is Done. This is a planned dependency,
not a reason to resume its old AI review label. Return through the board's legal Ready/Planning
path, replace the stale pipeline label and build a fresh plan. PR #451 stays closed. Implement TKT-399 from current origin/main without taking code or fixes from branch `TKT-399-pass-exit-mode`, commit `24f1c209` or PR #451. Use that history to retain findings, not as a source of implementation. Record this exclusion in the current author set. Any reused code would require its actual author and an independent reviewer before proceeding.

Owner decision TKT-399 D40 authorizes AI review rounds 8–11 for the new implementation. Retain
rounds 1–7 and their findings. Do not run four rounds by default; repeat only under the existing
Critical/High/material-change policy. The plan-review cap and the sprint's four-round cap are separate.

TKT-559 D1 allows this host's real Chrome for strict IndexedDB measurements. Record the device,
Chrome version, fixture, distribution and relation to the 3 s budget. These results do not qualify
a venue device. TKT-560 remains outside S-2 and mandatory before venue use, including the owner's
pre-decision bound and acceptable offline-spill rate decisions. Do not deploy the scanner in this run.

ADRs 085, 025, 066 and 021 govern the scanner work. Preserve the accepted residuals, transaction
completion, page ownership, status-before-body classification and actual-page-loss semantics.

## Verification and review budget

Use local Compose, the repository databases and this host Chrome with synthetic test credentials.
No production data, venue use, external paid API calls, release tag, GitHub release or deployment.
LLM application evaluation is not applicable to this scanner slice. Reviewer model calls are part
of the authorized development workflow.

Each functional ticket keeps plan review and code review with the existing ordered initial and
audit lenses. Focus on COS, correctness, security, affected integration and material decisions.
No routine work-unit checkpoint. An extra checkpoint needs a concrete risk and missing evidence.
Run focused tests and typechecks first. Mutate the actual mechanism and run each changed fixture
6–8 times after restoration. Run required `make check` before every push and after every merge.
Run `make browser` for changed write controls and the combined timed Entry/Exit journeys.
A green gate never substitutes for actual browser submission.

When the four functional tickets are Done, continue automatically into TKT-563. Freeze the product
candidate and material inputs, run the combined journeys, then all four final review areas. Each area
has initial inspection followed by decision audit. Implementation, verification and retrospective can run concurrently on the same frozen product packet. Documentation initial inspection can run concurrently. Its decision audit runs after the other three areas finish and the report draft exists, with those results and draft digests recorded as audit supplements. Use the normal four-round sprint budget, with no extra generic code review of the report.

Reviewers must differ from every actual author. Proposed per-run routing is gpt-6.1-sol for planning
and implementation, with claude-opus-5-5 high for all independent reviews. Record these overrides;
do not silently use the repository's older implement/aiReview defaults. Product-copy authorship is
explicitly assigned to gpt-6.1-sol for this proposed run, as one author model for S-2. This is a proposed owner assignment that becomes effective only when the start prompt is used. It does not follow from the independence rule; Claude Sonnet and Opus are also different models.

Report paths are `docs/sprints/S-2/report.en.json` and `docs/sprints/S-2/report.en.html`. Verify the
HTML, merge the report PR only after required CI `check` passes, compare committed bytes with the
render receipt, and post matching report-artifact pointers on the sprint and TKT-563. The report must state that no tag is planned and venue qualification is incomplete. Store the frozen review packet digest in `report.packetDigest`, covered by the source digest. Derive both board pointers from committed source read-back. A product correction to a Done member needs a new ticket and owner approval of its added scope and priority before it starts. Prepare that decision first; the original priority approval covers only the five named members.

The run ends at a concrete owner close request naming sprint, scope digest, current approved brief
digest, candidate, committed HTML path and digest, checks and residual issues. Keep TKT-563 and
the sprint open while that approval is pending. After approval, finish PO validation, metrics and
Done, then require the local closure contract before closing and reading the board back.

## Dependency inspection and elapsed time

Read `.github/dependabot.yml` and listed all open PRs on October 18. The result contains 13 Dependabot
PRs among 16 total open PRs. This inspection covered update titles and configured ecosystems, not
security advisory triage or the contents of all update diffs. No maintenance update was selected
for this bounded scanner sprint. Do not infer that no security updates are needed.

Keep timestamped operation observations in `/tmp/sdlc-S-2-operations.jsonl` and summarize them on the board at hand-offs. Report implementation, review, verification, CI wait,
unexplained idle, review rounds, extra checkpoints, gate runs and gate failures. Use `n/a` where
observations cannot separate durations. Label dwell time is not active work time.

## Working-copy constraint

The primary checkout has owner maintenance commits that diverge from origin/main, plus these local
skill edits. Preserve them. Product worktrees must use the actual current origin/main via `wt`.
Use the prepared skills from `/home/mathieu/sources/ticketing_system/.claude/skills/`, including
their scripts and references, rather than an older copy in a product worktree. Do not reset the
primary checkout or push its unrelated local commits. Revalidate these skill edits before the run. At start, pin the report renderer script, HTML template and label file in the run directory and record all three SHA-256 values. Use that bundle for the documentation check. If the active renderer is still uncommitted when publishing the report, archive those three runtime files under `docs/sprints/S-2/evidence/report-renderer/`, with the scripts/assets layout, and render from that archive. Commit the archive with the report evidence, record its command and file hashes, and reproduce the exact HTML after merge. This needs no production or active-renderer change.

## Skill-change validation

The local skill suites passed 184 tests: 47 ticket, 55 sprint and 82 report tests. All 38 targeted mutations were detected, followed by eight restored runs. The independent Opus initial and audit lenses ran for two maintenance rounds. Both rounds returned `REVISE`. Round 1 required checker and contract fixes; round 2 confirmed those mechanisms and identified prose and preparation gaps. The final batch resolves the ordered approval records, stale Gate 1 references, supplement kinds, runtime pinning, exact start identity and abandoned-code authorship. Focused verification checked the final file references, record order, unchanged executable-file hashes, exact board read-back and pinned-runtime reproduction. These remaining corrections are below the repeat threshold and use focused verification, not another full review round. This is maintenance review evidence, not the S-2 sprint review. No product gate, browser run or CI was run for these local uncommitted skill changes.

## Proposed machine-readable brief

```json
{
  "sprint": "S-2",
  "review": "full",
  "release": false,
  "closureTask": "TKT-563",
  "members": [
    "TKT-399",
    "TKT-551",
    "TKT-552",
    "TKT-559",
    "TKT-563"
  ],
  "scopeDigest": "475b7c77f414e3205adb736c856fb913675f56cbdd7f58603ca28b46ae061186",
  "startDate": "2026-10-19T09:00:00-04:00",
  "endDate": "2026-11-02T17:00:00-05:00",
  "reportLanguage": "en",
  "baseline": "5e3bee1db8019621fb5cdc343872b9c6159dd50d",
  "approver": "Mathieu Dupuis",
  "status": "proposed",
  "approvedBy": null,
  "approvedAt": null,
  "supersedes": null,
  "executionOrder": [
    "TKT-559",
    "TKT-399",
    "TKT-551",
    "TKT-552",
    "TKT-563"
  ],
  "modelOverrides": {
    "plan": "gpt-6.1-sol",
    "implement": "gpt-6.1-sol",
    "planReview": "claude-opus-5-5",
    "aiReview": "claude-opus-5-5",
    "sprintReview": "claude-opus-5-5"
  },
  "continuation": "autonomous"
}
```
