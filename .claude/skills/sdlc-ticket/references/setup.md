# Setup — bootstrap a project for the SDLC pipeline

Read this on the **first run** in a project (no `.claude/sdlc.config.json`). One-time, idempotent: if the config already exists, **validate** it instead of recreating. Output: a Jira project on the custom agentic workflow + Confluence space + a written `.claude/sdlc.config.json`.

## What the agent can vs can't do

The Atlassian MCP creates issues, links, comments, and Confluence pages — but **not Jira projects, workflows, boards, components, or Confluence spaces**. So setup splits three ways: the **org's Jira admin route** creates the empty Jira project + Confluence space; the **project lead** (project admin) configures the workflow, board, components, access, and connections; then the **agent** resolves IDs and writes the config.

> **Client-owned environment?** The provisioning path below assumes your own org owns the Atlassian instance. When the *client* owns Jira/Confluence/the repo, confirm the **prerequisites** up front, with a client admin partner if the consultant lacks admin: project/workflow creation, entity-property write access (else the description fallback), branch protection, cross-model review allowed, and who owns each gate (PO is often client-side; if the client owns the repo, the merge gate may be theirs). If a prerequisite can't be met — notably cross-model review — this SDLC isn't a fit for that engagement; don't run a degraded variant.

## 1. Jira project + Confluence space (admin provisioning, then your board setup)

**Source of truth:** your org's Jira project-creation process. The essentials:

- **The admin route creates the empty shells** — it provisions **both** the Jira project and the Confluence space, with the **name + key you give in the request**. That request is its *entire* role; **everything else (project settings, access, connections, workflow, issue types, columns) is yours** to configure afterward as project admin.
- **One Jira project = one client mandate.** Company-managed. Name conforms to the SOW; a **simple, recognizable project key**.
- **Issue types:** Epic, User Story, Task, Sub-Task, Bug (if applicable).
  - **Epic** = the feature. Carries the PRD link; **no pipeline label**; reaches Done when its last child closes.
  - **User Story / Task / Sub-Task** = the pipeline units that flow through the workflow and carry the `agent:*` / `needs:human` labels.

### Access (§2 of the page)
- Add the project team (**DE / DS / DA**).
- **Team Lead** as project **admin**.
- **PO** with read + validation access (owns the **PO REVIEW** gate).

### Connections (§3 of the page)
- **GitHub** connected to the project (if the mandate has a repo).
- The mandate's **Confluence space** connected to the project.

### Workflow — custom agentic (6 statuses + BLOCKED)

`Backlog → Ready → Planning → Building → PO Review → Done`, plus transversal **BLOCKED** (reachable from any active status; returns to Ready/Planning/Building by context). The agent stages are **first-class statuses** so human + AI co-work is visible on the board — deliberately richer than a standard delivery template, which only has the status axis.

| Status | Entry | Exit gate | vs. standard |
|---|---|---|---|
| **Backlog** | intake | ⛔ Gate 1 priority → Ready | ~ TO DO |
| **Ready** | prioritized queue | agent claims → Planning | ~ TO DO |
| **Planning** | agent claimed, drafting plan | ⛔ Gate 2 plan → Building (skip if `risk:low`) | **added** — no standard equivalent |
| **Building** | coding + PR + review | ⛔ Gate 3 review + **merge** → PO Review | ~ IN PROGRESS / IN REVIEW |
| **PO Review** | PO functional validation | ⛔ Gate 4 PO accepts → Done | **kept from standard** |
| **Done** | closure (metrics, learnings, branch delete) | — | ~ DONE |
| **BLOCKED** | transversal (open blocker) | ← returns by context | **kept from standard** |

Rework loops (Building / PO Review → Planning or Ready) and the rule "a ticket never moves backward without an explicit, observable reason" carry over from the standard. Record the real transition names in the config (they are not the status names).

**Who builds this:** you do. The admin route creates only the bare Jira project + Confluence space — provisioning carries no workflow/board options — so the custom statuses, transitions, board columns, and components above are **yours to configure** as project admin. The provisioning step never touches the workflow.

### Org delivery standard — sources of truth

This pipeline sits **inside** the org's standard delivery workflow. Defer to the org's canonical pages; don't duplicate or contradict them — typically:

- The **delivery workflow** page — the canonical sprint flow and the authoritative Jira status table.
- The **Definition of Done (DoD)** page — **`Done` is defined there, not in this skill.** The local gate is necessary but not sufficient; a ticket is Done only when the DoD is met.
- The **ticket-creation conventions** page.
- The **project delivery frame** (cadre) + its template and kickoff checklist.

**Semantics the agent must respect** (from the delivery-workflow standard):
- **COS = Conditions of Success** (this pipeline's term for acceptance criteria). Write COS on every ticket; the PO gate validates them. This skill's "AC" = COS.
- **Sprints are 4 weeks** (3 if a client constraint); the board is sprint-based. **Gate 1 (priority) ≈ the ticket being committed to a sprint** at planning/refinement. **WIP = one task in progress per person**, so the agent claims one ticket at a time.
- **`IN REVIEW`** (the `Building` tail here) = code deployed to **DEV** + PR reviewed by **another dev** — the approver is a dev, not the PO.
- **`PO REVIEW`** = the PO validates the **COS are met from the user's output** (via code if not user-demonstrable) and gives **final approval before the sprint work deploys to the client's TEST env**. That is Gate 4.

**Where things live — the "3 houses" rule**: code + technical standards → the **team standards registry** (via PR); process/org/project docs → the **team wiki**; solution definition + client deliverables → the **document store**. So promote reusable **technical** learnings to the registry and **process** learnings to the wiki — not just a Jira memo.

**V0: no workflow conditions** — gates are enforced by process (the agent never does a human-gate transition). Later: reserve the ⛔ transitions to a role via workflow conditions.

**Story Points** field on the pipeline units (V0 ticket sizing). Calibrate so a "one PR, reviewable in one sitting" ticket (`decomposition.md` sizing rule) ≈ small (1–3 pts); anything larger should be split, not pointed bigger. *(Cost/token estimation for sales is out of V0 scope.)*

**Components** — create a scheme of **5–12 coarse functional areas, aligned 1:1 with the decision-registry tags** so the context-mémo bake (`decomposition.md`) can route relevance by component. Example for this repo: `auth`, `api`, `db`, `projects`, `llms`, `mcp`, `dashboard-ui`, `uploads`, `infra`. Generic rule: *components = your registry's tags.*

**Labels** — free-form in Jira, **nothing to pre-create**: `agent:planning`, `agent:plan-review`, `agent:coding`, `agent:ai-review`, `needs:human`, `risk:low`.

## 2. Confluence space (provisioned with the project)

The admin route provisions the space alongside the Jira project (§1). Record its key and connect it to the Jira project. Epic PRDs become pages here, linked from the epic; story/task "PRDs" stay in the issue description.

## 3. Board views (agent or admin)

- **Quick filter `needs:human`** — the "is the bottleneck me?" view (every ticket waiting on a human gate).
- **Quick filter `risk:low`**.
- **Swimlanes by Epic** — see each feature's children together.

## 4. Code repo branch protection (agent, via `gh`)

Default branch: PR required, the CI check required, no direct push.

## 5. Resolve + write config (agent)

- **Blocks link type:** `getIssueLinkTypes` → the "blocks / is blocked by" name on this instance.
- **Transition names:** create a throwaway test issue, `getTransitionsForJiraIssue`, record the name for each hop (they are **not** the status names).
- **Component keys** as created above.
- Optionally create the **"🧠 Agent memory"** issue and/or point `registry.bindingPath` at the team decision registry.
- Write **`.claude/sdlc.config.json`** (schema in `SKILL.md`) with the resolved values.

## 6. Smoke test

Run **one trivial `risk:low` ticket** end to end (it skips Gate 2) to confirm statuses, transitions, labels, issue links, components, comments, and the PR→merge→Done handoff all work before onboarding real work.

## Notes

- **Entity properties** (the `agent-context` context-mémo, `decomposition.md`) are written via the Jira **REST** API, not the MCP. Confirm a token can `PUT /rest/api/3/issue/{KEY}/properties/agent-context`; else use the fallback defined in `SKILL.md` § Notes (an `<!-- sdlc:context -->` block at the end of the description) until that access exists.
- **Estimation scope:** Story Points (ticket size) are in V0. Dev cost/token estimation to help sales is **out of V0** — hard to calibrate before the pipeline has run; revisit once metrics accumulate.
