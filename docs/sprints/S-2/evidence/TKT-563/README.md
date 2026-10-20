# TKT-563 combined verification probe

This directory holds report-only evidence for the S-2 closure review. It does
not change product code, maintained tests, the active browser runner, lockfiles,
workflow files or the TKT-559 historical timing file. The probe runs the real
scanner UI in host Chrome against the real stack. It records named checks,
readbacks and receipts.

## Files

| File | Purpose |
|---|---|
| `inputs.json` | Immutable input manifest. It pins the helper, product, runtime and lockfile bytes that the probe uses. It also holds the variant recipe and the allowed check sets. It holds no run result. |
| `run-probe.sh` | Runner. It checks the preflight, takes the lock, starts the probe-only stack, runs `probe.mjs` and keeps every receipt. |
| `probe.mjs` | Probe entry point. It runs `entryCore` unchanged, the journeys for the variant, and writes `probe-result.json`. |
| `lib/denial.mjs` | Denial-copy journey (plan section 2.1). |
| `lib/mixed.mjs` | Seven-row mixed sync journey (plan section 2.2), with the AX01 and AX02 request changes. |
| `lib/fixtures.mjs` | Synthetic ticket fixtures and the Access readbacks. |
| `lib/assets.mjs` | Served-asset checks and the diagnostic asset route. |
| `lib/checks.mjs` | Named check recorder and secret scrubbing. |
| `lib/kit.mjs` | Shared browser helpers, the native sync observer, polling and canonical comparison. |
| `lib/mutate-source.mjs` | Applies one presenter or counter mutation to a disposable copy. |
| `lib/redact.mjs` | Redacts credential values from every log the runner keeps. |
| `lib/verify-inputs.mjs` | Checks the pinned inputs before any stack starts. |
| `run-make-browser.sh` | Guarded `make browser` run. It archives and restores the TKT-559 timing bytes. |

## Preconditions

Root runs these steps in order, from the committed candidate:

1. Commit the exact evidence files. The runner refuses if any file here is
   uncommitted or untracked.
2. Run `make deps` with the frozen lockfile. Record the result.
3. Run `make build-gate-linux` and `make build-ts`. The runner does not build.
4. Confirm that `.gate.lock` is absent. Confirm that no probe lock exists under
   `${TMPDIR:-/tmp}`. Confirm that no timing restore is pending.

The runner needs Docker, Go, Node, `shasum`, and the host Google Chrome. It does
not need ZBar, because the probe does not use the checkout spec.

## Commands

Run every command from the checkout root. The output directory must be new and
must sit outside the checkout.

The five diagnostic variants run first. The eight restored candidate
repetitions run after them. The full `make browser` run comes last.

```sh
docs/sprints/S-2/evidence/TKT-563/run-probe.sh --variant COPY-EXIT --repeat 1 --output /tmp/tkt563-copy-exit-<id>
docs/sprints/S-2/evidence/TKT-563/run-probe.sh --variant COPY-LIMIT --repeat 1 --output /tmp/tkt563-copy-limit-<id>
docs/sprints/S-2/evidence/TKT-563/run-probe.sh --variant COUNT-REJECTED --repeat 1 --output /tmp/tkt563-count-rejected-<id>
docs/sprints/S-2/evidence/TKT-563/run-probe.sh --variant AX01 --repeat 1 --output /tmp/tkt563-ax01-<id>
docs/sprints/S-2/evidence/TKT-563/run-probe.sh --variant AX02 --repeat 1 --output /tmp/tkt563-ax02-<id>
docs/sprints/S-2/evidence/TKT-563/run-probe.sh --variant candidate --repeat 8 --output /tmp/tkt563-candidate-<id>
docs/sprints/S-2/evidence/TKT-563/run-make-browser.sh --historical-backup <file> --receipts /tmp/tkt563-make-browser-<id>
docs/sprints/S-2/evidence/TKT-563/run-make-browser.sh --recover-only
```

The runner refuses `--repeat` values other than 8 for the candidate and 1 for
each diagnostic.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | `PASS` for the candidate, or `DETECTED` for a diagnostic. |
| 1 | `FAIL` for the candidate, or `NOT_DETECTED` for a diagnostic. |
| 2 | Setup error inside the run. The stack, the browser or Access failed before the journeys ran. |
| 3 | Refused before the stack started. Read `refused.txt` in the output directory. |

The runner returns the probe exit code unchanged. A diagnostic is detected only
when every intended named check fails, no check outside the allowed set fails,
and the mutation reached the system. The probe records the served asset hashes.
The runner records the anchor match count of each presenter mutation in
`mutation-<variant>.json`.

## Output

Each run writes a new directory with these files:

- `runner-receipt.txt`: commit, tree, variant, ports, container IDs, tool
  versions, and the SHA-256 of each probe file.
- `input-pins.txt`: the result of the pinned input check.
- `probe-result.json`: every named check, the journey evidence, the served
  asset digests and the verdict.
- `probe.log` and `compose-logs.redacted.log`: redacted logs.
- For presenter diagnostics only: `mutation-<variant>.json`,
  `mutation-<variant>.diff` and `disposable-build-<variant>.log`.
- `compose-prestart.log`, `compose-up.log` and `compose-down.log`: stack logs,
  redacted. `refused.txt` appears only when the runner refuses a run.

Root records the candidate HEAD, the tree and every per-file hash in an external
receipt. This directory does not pin its own probe files.

## What the journeys check

The denial journey submits Entry and Exit through the rendered controls. It
checks the exact `exit_required` and `entry_limit_reached` alert text and the
server reason on the wire. It checks the terminal local row and the final
`actuated` value. It also reads the probe's write history for the denied
occurrence. A write of `ACTUATED` that succeeded or completed fails the check,
even when the final row is clean. The journey checks the Access readbacks after
each step.

Each denial keeps the wire status and the sanitized request body and response.
It also keeps the exact alert text, the observed local rows, the write history,
and the lifecycle rows before and after the denial. Each accepted scan keeps
its wire record and its readbacks.

The mixed journey creates seven queued rows through the rendered controls. It
then runs one real reconciliation batch. It checks the wire body, the response
pairs, the summary text, the two rejection warnings, each saved row against
its final row, the Access facts, the scanner refusal row, the conflict alarm
payload and the absence of other alarms. It keeps the Access facts, lifecycle
rows, local decisions, quarantine counts and alarm envelopes, including their
delivery-state columns. It keeps these before the batch, after the batch and
after the online event. It also keeps the original and the sent batch, and the
pre-apply request and response. Each retained value sits beside its check. A
failed wire check does not stop the readbacks.

The no-resend check, `MIX-NO-RESEND-SETTLED`, uses native calls that the page
makes. The probe wraps the page's IndexedDB `transaction` method and these store
methods: `add`, `clear`, `count`, `delete`, `get`, `getAll`, `getAllKeys`,
`getKey` and `put`. Each wrapper forwards the call and the result unchanged. The
probe records the scope, the mode, the operations in order, the completion
order, and the occurrence IDs and states that each read returns. It records no
arguments and no credentials.

The probe dispatches the `online` event only after no transaction on the
occurrence or legacy classification store is pending. After the dispatch, the
check passes only when these reads complete in the epoch that starts at the
dispatch:

- Two recovery-list reads. Each one runs `occurrences.getAll`, then
  `legacy_classes.getAllKeys`, in one readonly transaction.
- Two `queued()` reads. Each one runs `occurrences.getAllKeys`,
  `occurrences.getAll`, then `legacy_classes.getAll`, in one readonly
  transaction.

`App.tsx` starts both pairs on the `online` event, from `refreshQueued` and from
`syncBatch`. The operation sequences above match `occurrence-store.ts` at the
pinned digest. At least one recovery-list read must complete before a queued
read completes. Every row that these reads return must have the state `SYNCED`
or `RESOLVED`. Every legacy classification read must be empty. The IDs must be
exactly the seven batch IDs. No observed transaction may abort or remain
pending.

After the completed reads, the page yields one task and waits 300 ms. The quiet
period adds evidence. It never replaces a completed read. Then the journey checks
that the page sent no reconciliation request.

A refund for ticket G goes through the real endpoint. The probe waits for a
completed revocation pull that lists that ticket before it scans.

An uncaught page exception fails the run. Console errors are kept as evidence
only. A recorded Exit does not prove that the holder has left the venue.

Existing `entryCore` self-checks run first in every repetition, with
`SCANNER_MUTATION` absent. Some of those checks use substituted responses, so
their results belong to the existing test suite. The new journeys use real
responses only.

## What the probe does not prove

- Host Chrome results describe one host. They do not qualify a venue. TKT-560
  owns venue qualification.
- The denial checks cover the two exact alert texts. Broader Entry copy is
  TKT-565.
- The mixed journey proves one real batch. Cross-batch accumulation, partial
  failure and retry replacement stay with the existing App tests. Those tests
  use mocked responses.
- The conflict alarm check proves that a durable alarm row is owed. It does not
  prove delivery to an operator.
- The observer sees the page's IndexedDB calls only. It does not see the
  network. The no-request check relies on the page's request events and on the
  quiet period.
- The probe is one-time browser evidence. It is not maintained regression
  coverage. TKT-566 holds the promotion into the durable browser suite.
- Fixture data is synthetic. Host scheduling is not pinned.
- LLM application evaluation does not apply to this scanner slice.

## Serialization and locks

Every probe run takes `${TMPDIR:-/tmp}/tkt563-probe-<digest>.lock`. The digest
is the first 16 hex digits of the SHA-256 of the checkout root. `run-make-browser.sh`
takes the same lock. A gate run uses `.gate.lock` in the checkout, and the probe
refuses while that file exists. The gate does not check the probe lock, so root
must not start a gate while a probe or a browser run is active.

A killed run can leave its lock behind. Remove a stale lock only after you
confirm that no run is active. Remove `${TMPDIR:-/tmp}/tkt563-timing-guard`
only after the timing file is restored and verified.

## Credentials and redaction

The stack creates new credentials for each run. The runner passes them to the
probe through the probe's environment. Neither the runner nor the probe prints
them. Every log the runner keeps passes through `lib/redact.mjs`. The probe
scrubs each detail string.

The probe keeps the SHA-256 digest of each credential-bearing field that it
retains. Local scanner rows keep the QR payload in every state, so each retained
local row holds the digest of its payload instead of the payload. The denial and
mixed journeys apply the same rule to request bodies, responses and rows.

## The timing file

`make browser` runs the TKT-559 timing spec. The spec writes
`docs/sprints/S-2/evidence/TKT-559-strict-commits.json` in the checkout.
`run-make-browser.sh` keeps a verified copy of the historical bytes outside the
checkout. On every trappable exit, it archives the timing bytes that the run
left, even when they equal the historical bytes. It records whether the writer
ran. Then it restores the historical bytes and checks the digest. It also checks
that no other tracked file changed.

If the archive, the restore or the tracked-status check fails, the wrapper exits
with code 4. It keeps the pending marker and prints the next step. The
`make browser` exit code still appears in the output and in `timing-result.txt`.
When every cleanup step passes, the wrapper exits with the `make browser` code.

After an untrappable kill, the next `run-make-browser.sh --recover-only` run from
the same checkout archives the timing bytes that the killed run left. Then it
restores and verifies the historical bytes. The archive goes to the pending
receipts directory. If that directory is missing, the wrapper creates a new
recovery directory outside the checkout and prints its path. The archive directory also goes
to `${TMPDIR:-/tmp}/tkt563-timing-guard/last-recovery-dir.txt`. The marker names
its checkout. A recovery from another checkout is refused. `run-probe.sh`
refuses to start while the marker exists.

## Status of this directory

At implementation time, the files were written and checked by reading and by a
syntax check only. No probe, mutation, build, gate, Docker command or browser
run had happened. The repaired observer, the denial history check and the
wrapper's recovery path had not run either. This section describes the files at
that time. Root owns the later runs and keeps their receipts outside the
repository.
