# Scheduled workflow failures

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## When a scheduled workflow fails (TKT-213)

Two workflows run on a weekly cron and are **not** part of the per-PR gate:

- `hermetic-smoke` (Mondays 06:00 UTC) — the full in-Docker build path. `make check` uses the fast
  host-built path, so this is what keeps the two honest.
- `security` (Mondays 07:00 UTC) — re-scans every dependency even when nothing changed, which is how
  a newly-disclosed CVE in untouched code gets caught.

Both used to fail into the void. `hermetic-smoke` was red on `main` for nine days in August 2026 and
was found only because an unrelated PR happened to touch `compose.yaml` and trip its path filter.

**Now a failed scheduled run opens a GitHub issue** labelled `scheduled-workflow-failure`, carrying a
link to the failed run. Repeated failures comment on the same issue rather than opening new ones — one
issue per outage, not per run and not per matrix leg (`security` has eight `govulncheck` legs). A
later scheduled run that is genuinely green comments and closes it.

**PR-triggered failures do not open issues.** A red check on a PR is already in front of its author.

### Things worth knowing before you edit either workflow

- **`permissions:` at job level replaces the workflow-level block; it does not merge.** This repo's
  `default_workflow_permissions` is `read`, so the notifier jobs declare `issues: write` explicitly.
  Omit it and the step 403s — a notifier that "runs" and tells nobody, which is the failure this whole
  mechanism exists to prevent. If you add a checkout step to a notifier job in `security.yaml`, add
  `contents: read` back too.
- **Adding a top-level job? Add it to both notifier jobs' `needs:`.** The recovery job does not trust
  `needs:` alone — it asks the run how many jobs concluded `failure`, `cancelled` or `timed_out`
  (`gh run view --json jobs`) and closes the issue only when **exactly one job is still in flight**
  (itself) **and exactly one finished job did not succeed** (the sibling notifier, which is skipped on
  every run where the other one fires).
  Anything else — `failure`, `cancelled`, `timed_out`, `action_required`, `stale`, a conclusion GitHub
  adds later, a *second* unfinished job, or a *second* skipped job — blocks the close. The last two
  are what catch a top-level job omitted from `needs:`: still queued, or skipped by a false `if` or a
  missing input. Allowing skips freely would have closed the outage while a scan that was supposed to
  run never did — in `security.yaml`, a silently-unperformed CVE sweep reported as clean.

  **Known gap:** the guard takes one snapshot and does not retry. If a job row is transiently
  unsettled when it looks, this run declines to close and the issue stays open until the next
  scheduled run clears it. Self-correcting, and preferred over retry logic inside an alarm path.

  It reads `status`, not just `conclusion`, and that is not cosmetic: **`gh run view --json jobs`
  renders a running job's conclusion as the empty string, not JSON null**, so a guard written against
  `null` treats its own still-running job as unacceptable and never closes anything. Four versions of
  this guard were wrong before this one — by display-name prefix, by counting only failures, by
  requiring exactly one JSON null, and by that empty-string confusion. Change it carefully.
- **Both notifier jobs carry `!cancelled()`.** GitHub re-evaluates a running job's `if` during
  cancellation and keeps the job when it still holds, so without it a cancelled run could still write
  to the issue — contradicting the rule that a cancelled run reports nothing.
- **The notifier is inlined in each workflow rather than shared as a script**, deliberately. A shared
  script needs `actions/checkout`, which would run the notifier *from the commit under test* — so a
  bad `main` would break the alarm whose job is to report that `main` is bad. The cost is a
  near-duplicate block in the two files: change one, change the other.
- **The issue is matched by a marker keyed on the workflow's file path**, not its display name, so
  renaming a workflow does not orphan its open issue.

### Verifying a change to it

```bash
bash scripts/verify-scheduled-notifier.sh all        # both workflows
bash scripts/verify-scheduled-notifier.sh hermetic   # just one
```

Extracts the `run:` blocks verbatim from the named workflow and drives them against the real GitHub
API with a throwaway label — create, dedupe over three failures, the guard **refusing** to close
while a job failed, close, and the no-op path — cleaning up after itself.

It also exercises the guard's predicate directly against **synthetic job payloads**, because the live
state cannot be staged from real run data: a completed historical run has nothing in flight, while the
guard always has exactly one job running when it asks. That gap hid a real defect for a review round.
The synthetic cases cover each blocking conclusion, an unknown future conclusion, a second unfinished
job, and an empty jobs array — and the suite fails if its copy of the predicate drifts from either
workflow's.

**Run `all`, not one.** The two workflows carry independent inlined copies, so a suite that reads
only `hermetic.yaml` says nothing about `security.yaml` — which is exactly the coupling the
change-both-copies rule above creates. Every assertion is fatal: an earlier version used
`[ x = y ] && echo ok`, which under `set -e` cannot fail the script, and it printed a success banner
while the issue body was missing its link to the failed run.

**What it does not cover, and nothing local does:** GitHub's own evaluation of `if:`, `needs:`,
`permissions:` and matrix aggregation. Nothing in `make check` parses or executes workflow YAML, so a
green gate says nothing about this mechanism. The wiring is confirmed by watching a real run: on a PR
touching these files, both notifier jobs must appear as **skipped**.
