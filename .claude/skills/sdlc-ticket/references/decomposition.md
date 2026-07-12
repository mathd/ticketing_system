# Decomposition (Jira) — PRD/epic → pipeline tickets

Read this when the intake is a PRD/spec, or an epic bigger than one ticket. Output: an **epic** holding the lineage (brief → PRD), **child issues** sized for the pipeline, wired with **Jira issue links**, classified simple/complex, each carrying a baked **context-mémo**, closed by a **readiness verdict** before Gate 1.

The agent **proposes** the decomposition; the human **validates** it (the classification filter + the priority gate).

## Where the PRD lives

- **Epic** (feature, possibly multi-sprint) → a **Confluence page** (`createConfluencePage` in `CONFLUENCE_SPACE`), linked from the epic. The epic body holds `## Brief` (if exploration ran) + a link to the PRD page.
- **Story / simple task** → no separate page. The "PRD" is the enriched **description** (problem + COS). Granularity is flexible — not everything is an epic; G1's weight scales with size (a trivial task can carry `risk:low` and skip the plan gate downstream).

## PRD format (so decomposition is mechanical, not interpretive)

```markdown
# PRD: <Feature>
<2-3 paragraphs: problem, outcome, non-goals.>

## Quality Gates (every story)
- <project's local gate — see config.code.localGate>
- UI stories: verify in browser (screenshot in the PR)

## User Stories
### US-001: <Title>
**As** <role>, **I want** <capability>.   **Priority:** P1   **Depends on:** —
**Acceptance Criteria:**
- [ ] <verifiable criterion>
### US-002: …   **Depends on:** US-001
```

- Story IDs `US-001…` = stable references for dependency wiring. Priority `P0`–`P4` (default P2). `Depends on:` references **earlier** stories only.

## Sizing — the #1 rule

**One ticket = one vertical slice a human can review in one sitting.** Each ticket is a *tracer bullet*: a thin path through **all** the layers it touches (schema → backend → UI → tests) that is demoable/verifiable on its own — **not** a horizontal slice of one layer. This is what lets a ticket pass **Gate 4 (PO Review)**, whose exit needs the COS shown from user-visible output; a schema-only ticket reaches PO Review with nothing to demo.

The pipeline's unit of cost is still Gate 3 (a human reading a diff): target ~≤ 400 changed lines; if you can't describe the slice in 2–3 sentences, split it into a **narrower** slice that is still end-to-end — never into a horizontal layer. Don't over-split either — changes only verifiable together (a column + its migration) stay in one ticket.

## Ordering & prefactoring

- **Prefactor first.** "Make the change easy, then make the easy change." If the slices need groundwork (extract a seam, reshape an interface), that's the **first** ticket — its own slice, `risk:low` if mechanical.
- **Order by dependency, not by layer.** A slice never depends on a later slice. Layer order (**schema → backend → UI → aggregate/dashboard**) is only the tie-breaker for splitting *within* one slice that's grown too big — it is not a reason to carve a feature's schema into a standalone ticket.

## Acceptance criteria — verifiable only

Each child's AC = story-specific criteria + the quality gates appended (+ UI gate for UI). Good: "Add `investorType` column, default 'cold'". Bad: "works correctly", "good UX", "handles edge cases".

## Conversion procedure (Atlassian MCP)

1. **Epic** — `createJiraIssue` (type Epic, summary `Epic: <feature>`, body = brief + PRD link). For an epic, `createConfluencePage` for the PRD and link it.
2. **Children** — one `createJiraIssue` per story (type Story/Sub-task), summary keeps the ID (`US-001: <title>`), description = story + AC + quality gates, parent = the epic.
3. **Dependencies** — for each `Depends on:`, `createIssueLink` with the "blocks / is blocked by" type (resolve the exact name via `getIssueLinkTypes`). After wiring, **cycle check**: walk each child's "is blocked by" edges transitively; if you return to the start, remove the offending edge and re-derive from layer order.
4. **Classification** — tag each child **simple** (clear, isolated, low-risk, obvious COS → machine alone) or **complex** (ambiguous, cross-cutting, risk/security/architecture → co-work). Suggest `risk:low` per trivial child.
5. **Context-mémo bake** (below) — per child.
6. **Readiness verdict** (below) — on the epic.

The epic carries no pipeline label and never enters Planning/Building; it reaches Done when its last child closes.

## Context-mémo bake (the pull-once, cache-as-push step)

For each child, **once at creation**, resolve relevance from the ticket's components/tags and query the **decision registry** + **Confluence**. Write the result to the ticket's entity property `agent-context` so the executing agent (Building) never re-reads the docs each iteration:

```json
{
  "binding":   [{ "id": "DEC-0007", "rule": "<recommandation verbatim>", "date": "2026-06-18" }],
  "reference": [{ "summary": "<1 line>", "link": "<confluence url>" }],
  "capturedAt": "2026-06-18"
}
```

- **Binding → verbatim** (never summarized — a dropped constraint is the exact failure we avoid). **Reference → summary + link.**
- **Staleness:** decomposition is pre-sprint; execution comes later. The `id`+`date`+`capturedAt` let the executing agent **revalidate cheaply at pickup** (has any `binding.id` been superseded since `capturedAt`?) instead of re-reading everything. A hidden payload fails silently otherwise — the dates make it detectable.
- **Write** via Jira REST `PUT /rest/api/3/issue/{KEY}/properties/agent-context` (not exposed by the MCP); read back via `getJiraIssue` (properties). Don't put secrets — any authenticated user can read it.

## Dependency-aware claiming (used at Ready → Planning)

A ticket is **claimable** only when it sits in `Ready` (priority) **and** has zero open blockers. **Never claim with open blockers**, even if a human moved it to `Ready` — surface the conflict in a `kind=blocker` comment and leave it. Selection when several are claimable: in-flight first (resume), then priority (P0→), then unblocking power (blocks the most others), tie-break lowest key. State the reason in the claim comment.

At closure, recompute claimability for everything this ticket blocked; list the **newly unblocked** in the metrics comment. Don't auto-move them — `Backlog → Ready` stays human.

## Readiness verdict — the last step before Gate 1

Decomposition ends with a **PASS / CONCERNS / FAIL** verdict comment on the epic. By prioritizing the children, the human approves brief + PRD + decomposition in one decision — so the verdict must make any weakness visible.

Self-audit (honestly): quality gates extracted · each child a vertical slice sized to one reviewable PR · prefactoring split out first · ordered by dependency, no forward dependency · AC verifiable everywhere · children linked + dependency edges + cycle check passed · context-mémo baked per child · no unvalidated assumption a child silently depends on.

- **PASS** — all hold → ask the human to prioritize (Gate 1).
- **CONCERNS** — proceedable with named risks (oversized story, soft dependency, unvalidated assumption) → list each with the affected child; the human decides eyes-open.
- **FAIL** — structural problems (unverifiable AC, dependency cycle, missing gates) → **do not request Gate 1**; fix and re-verdict.

```markdown
<!-- sdlc:stage=backlog kind=readiness -->
### ✅ Implementation readiness — PASS | CONCERNS | FAIL
| Criterion | Status | Note |
|---|---|---|
| Sizing | ✅ | — |
| Dependencies | ⚠️ | US-004→US-002 soft — could parallelize |
**Concerns:** <numbered, each naming the affected child — or "none">
**Verdict:** <PASS/CONCERNS/FAIL + one sentence>
```
