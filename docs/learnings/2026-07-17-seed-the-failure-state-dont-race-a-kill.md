# Seed the failure state; don't race a kill

**Ticket:** TKT-70 (smoke.sh pre-clean), PR #68.

## The lesson

When the defect is "an interrupted run leaves state behind that the next run silently reuses",
the obvious repro — start the real thing, `sleep N`, SIGKILL it mid-run — is slow and
timing-flaky: N is a guess, the window moves with machine load, and each attempt costs a full
run. The deterministic equivalent is to **construct the leftover state directly** and only
simulate the *interruption semantics* (kill the container, not the script):

1. Bring up just the stateful piece under the same compose project (`compose up -d postgres` —
   seconds, and the volume gets the proper project name/labels).
2. Plant an observable marker in the state (a file in the volume).
3. `docker kill` the container — the script-level trap never existed, so nothing cleans up,
   which is exactly what a SIGKILL/daemon-crash/killed-runner leaves behind.
4. Run the system under test and observe the marker: present in the next run = red;
   wiped before use = green. Each observation lands in seconds, not minutes.

On TKT-70 this replaced a two-full-smoke-run + `sleep 60` draft repro and made TDD's
"observed red first" cheap enough to actually do for a shell/teardown change.

## When it applies

Any cleanup/teardown/idempotency defect where the claim is about *leftover state*, not about
the interruption itself: trap ordering, pre-clean steps, lock files, stale PID/queue entries.
The interruption is only the delivery mechanism for the dirty state — so fabricate the state,
don't re-enact the interruption.
