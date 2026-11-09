# Technical delivery standards

[AGENTS.md](../AGENTS.md) sets the working agreement. Accepted ADRs bind design.
The [documentation map](README.md#authority-and-freshness) distinguishes requirements,
implementation, and historical evidence.

## Local and CI gate

Run `make deps` on a clean clone, then `make check` as a standalone command. The wrapper owns
`.gate.lock`, `.gate.log`, and `.gate.verdict`. Do not edit tracked files during the run.
Never call `make check-all` directly. The verdict checks both exit status and log content, and
records its commit/tree. [Testing](testing.md) owns the stage description; the executable stage
list is `check-all` in [Makefile](../Makefile).

CI calls the same gate plus `scripts/gate-selftest.sh`. Scheduled/path-triggered hermetic smoke
and advisory scans have separate roles. See [CI policy](conventions/continuous-delivery.md).
For changed web write forms, add a submitting browser spec and run `make browser`; smoke alone
does not verify browser actions, Origin, cookies, or JavaScript.

## Engineering and review

Start from written requirements. Keep service ownership, integer money, tenant scoping,
append-only histories, explicit public/internal routes, and contract compatibility intact.
Read [engineering rules](conventions/engineering-rules.md) for test tiers, reachability,
mutations, fixture repetitions, security evidence, and review of interacting fixes.
Record design changes as ADRs and update the owning documentation in the same change.
[Maintenance](documentation/maintenance.md) names update triggers.

Ticket work uses [.claude/sdlc.config.json](../.claude/sdlc.config.json) and the selected
[workflow skill](../.claude/skills/README.md). Human and autonomous modes take gates differently;
owner priority and unresolved blocking findings remain owner decisions. Documentation maintenance
uses the skill's exception unless the session asks for a PR. See [PR conventions](conventions/pull-requests.md).

## Tool hooks and limits

[.claude/settings.json](../.claude/settings.json) registers Claude `PreToolUse` hooks only.
The Bash guard refuses commands outside its narrow wait/read forms while the gate is locked.
For a recognized `gh pr merge`, it refuses a dirty tree, a missing/stale PASS verdict, a different
current tree digest, or a local HEAD that differs from the forge's PR head. It also detects some
Claude-attribution patterns in commit command text. The Edit/Write guard refuses edits inside a
repo with a gate lock. `make check-hooks` runs `.claude/hooks/selftest.sh` with seeded violations.

These guards are limited recognizers, not a full shell parser or universal enforcement. They do
not automatically run for Codex or every execution tool. The attribution policy covers all models
even though this guard recognizes only some strings. No hook currently refuses `--admin`;
TKT-518 tracks that gap. The gate lock also has structural enforcement in Makefile, independent
of hook wiring. Respect the working agreement when a tool can bypass a guard.

## Runtime baseline

Docker Compose is the only runtime. Each service has health/readiness, structured logs, and
OpenTelemetry export. [Architecture](architecture.md), [configuration](configuration.md),
and [known limits](known-limits.md) own the details. Local measured NFR targets do not create
production SLO commitments. Cloud release infrastructure and production operations need separate
design and authorization.
