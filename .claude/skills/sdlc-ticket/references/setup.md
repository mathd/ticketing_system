# Setup — bootstrap a project for the SDLC pipeline

Read this on the **first run** in a project (no `.claude/sdlc.config.json`). One-time, idempotent:
if the config already exists, **validate** it instead of recreating. Output: a vault and board
serving the project's tickets, a written `.claude/sdlc.config.json`, verified branch protection,
and one smoke-tested ticket.

> **Prerequisite — cross-model review.** If the environment forbids sending code to an external
> model, this SDLC is not a fit; do not run a degraded variant (SKILL.md § config.models).

## 1. Vault and board

The board is its own repository, `~/sources/sdlc-board`. Mechanics: `local-tracker.md`.

1. Clone `~/sources/sdlc-board` once per machine. One install serves several projects — `?vault=`
   selects between them.
2. Create the project's vault in FNS and add it to the API token's vault allowlist.
3. Write `_board/config.md` in the vault: schema, project name, statuses, transversal statuses, WIP
   limits. The board renders its columns from it and refuses a write to a status it does not list.
4. Start the server (`local-tracker.md` § Architecture) and open `board.html`.

### Statuses — 6 plus transversal BLOCKED

`Backlog → Ready → Planning → Building → PO Review → Done`, plus **BLOCKED** (reachable from any
active status; returns to Ready/Planning/Building by context). The agent stages are first-class
statuses so human + AI co-work is visible on the board.

| Status | Entry | Exit gate |
|---|---|---|
| **Backlog** | intake | ⛔ Gate 1 priority → Ready (board hard-blocks while `readiness` is open or a blocker is open) |
| **Ready** | prioritized queue | agent claims → Planning |
| **Planning** | agent claimed, drafting plan | ⛔ Gate 2 plan → Building (skip if `risk:low`) |
| **Building** | coding + PR + review | ⛔ Gate 3 review + **merge** → PO Review |
| **PO Review** | PO functional validation | ⛔ Gate 4 PO accepts → Done (board refuses Done without `kind=metrics`) |
| **Done** | closure (metrics, learnings, branch delete) | — |
| **BLOCKED** | transversal (open blocker) | ← returns by context |

A ticket never moves backward without an explicit, observable reason in its thread.

**Labels** are free-form on the note, nothing to pre-create: `agent:planning`, `agent:plan-review`,
`agent:coding`, `agent:ai-review`, `needs:human`, `risk:low`.

**Ticket types:** `Epic` (the feature; carries the PRD link; no pipeline label; closed by roll-up once
its last child is Done — `local-tracker.md` § Epics), `Story` / `Task` (the pipeline units), `Spike` (`shaping.md` § Spikes).

### Semantics the agent must respect

- **COS = Conditions of Success** — this pipeline's term for acceptance criteria. Every ticket has
  them; Gate 4 validates them.
- **WIP = one ticket in progress** per implementer, so the agent claims one ticket at a time.
- **PO Review** = the PO validates the **COS are met from the user's output** (via code if not
  user-demonstrable) before the work is accepted. That is Gate 4.
- **`Done` is defined by the project's Definition of Done**, not by this skill. The local gate is
  necessary, not sufficient. Name the DoD location in the project's instructions.

### Where things live — the "3 houses" rule

Documentation is in the repository (see the project's `AGENTS.md`):

| House | What | Where |
|---|---|---|
| Technical | binding decisions, standards | ADRs in `registry.bindingPath`; repo-wide rules in `AGENTS.md` |
| Process | how we work, lessons | `docs/LEARNINGS.md` + `docs/learnings/`; process rules in `AGENTS.md` or this skill |
| Product | solution definition, PRDs, briefs | `docs/product/` |

Promote a reusable **technical** learning to an ADR and a **process** learning to `AGENTS.md` or
this skill — not just a ticket comment.

## 2. Code repo branch protection (agent, via `gh`)

Default branch: require a PR and the CI check on the host (on GitHub, a repository ruleset; commit
its payload to the repo). Record any bypass actors in the project's instructions. Verify with
`gh api repos/<o>/<r>/rules/branches/<b>` and by attempting a forbidden operation as an identity
without bypass.

## 3. Write the config (agent)

Write **`.claude/sdlc.config.json`** (schema in `SKILL.md` § Config): `tracker: "local"`, `gates`,
`code`, `models`, `review.severityMap`, `registry`. Check the cross-model pairs (SKILL.md
§ config.models) before the first run.

## 4. Smoke test

Run **one trivial `risk:low` ticket** end to end (it skips Gate 2) to confirm statuses, transitions,
labels, `blocked-by` links, comments, the transition log, and the PR → merge → Done handoff all work
before onboarding real work.
