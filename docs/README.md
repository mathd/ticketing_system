# Documentation map

Load this index, then the file for the task. Read relevant sections of long ADRs; do not load the
whole decision log or evidence tree. Start implementation from the written requirement and verify
current behavior in code.

## Where to look

| Task | Canonical entry |
|---|---|
| Start the stack or run checks | [Root README](../README.md), [development](development.md), [testing](testing.md) |
| Understand ownership and flows | [Architecture](architecture.md), [solution design](solution-design.md) |
| Find a binding decision | [ADR status and topic index](adr/README.md) |
| Recover an event, order, payment, or scanner | [Runbook index](runbooks/README.md) |
| Configure a process or credentials | [Configuration](configuration.md), [Compose](../compose.yaml), [Docker](docker.md) |
| Change code or review tests | [Engineering rules](conventions/engineering-rules.md), [delivery standards](technical-delivery-standards.md) |
| Contribute or check CI policy | [PRs](conventions/pull-requests.md), [commits](conventions/commits-and-branching.md), [CI](conventions/continuous-delivery.md) |
| Check security or dependency policy | [Security](conventions/security.md), [dependencies](conventions/dependencies-and-versions.md) |
| Find product requirements and limits | [Brief](product/brief.md), [PRD](product/prd-v1.md), [later owner decisions](product/backlog-decisions-2026-10-10.md), [roadmap](ROADMAP.md) |
| Find a past failure or proof | [Learnings](LEARNINGS.md), [record index](records.md) |
| Run the ticket workflow | [Workflow entry](../.claude/skills/README.md), [config](../.claude/sdlc.config.json) |
| Evaluate or maintain these docs | [Fixed scorecard](documentation/evaluation.md), [evaluation results](documentation/results.md), [maintenance](documentation/maintenance.md) |

## Authority and freshness

- Follow session instructions and [AGENTS.md](../AGENTS.md). Ticket execution follows the selected
  skill and `.claude/sdlc.config.json`; human and autonomous gates differ.
- Product documents define intent. Their planning checkboxes and dated drafts are not delivery
  status. The board owns ticket state; [roadmap](ROADMAP.md) is a capability summary.
- Accepted ADRs bind design within their stated scope and amendments. Proposed ADRs authorize no
  implementation. Acceptance can define future requirements without proving delivery.
- Code, OpenAPI sources, migrations, manifests, and executable build configuration establish
  implemented behavior. If they contradict an accepted requirement, report the discrepancy;
  neither silently cancels the other.
- Runbooks describe current local Compose operation. Dated investigations, sprint reports, logs,
  screenshots, and probes establish only their recorded baseline. They do not grant permission to
  replay mutations or prove the present system works.

All documentation stays in this repo. Ticket state lives in the Fast Note Sync board served by
`~/sources/sdlc-board`; see the [tracker protocol](../.claude/skills/sdlc-ticket/references/local-tracker.md).
There is no external wiki. Keep one canonical procedure and link to it.
