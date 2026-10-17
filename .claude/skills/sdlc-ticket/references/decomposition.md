# Decomposition — PRD/epic → pipeline tickets

Read this when the intake is a PRD/spec, or an epic bigger than one ticket. Output: an **epic** holding the lineage (brief → PRD), **child tickets** sized for the pipeline, wired with **`blocked-by` links**, classified simple/complex, each carrying a baked **context-mémo**, closed by a **readiness verdict** before Gate 1.

The agent **proposes** the decomposition; the human **validates** it (the classification filter + the priority gate).

## Where the PRD lives

- **Epic** (feature, possibly multi-sprint) → a **PRD file in `docs/product/`** (e.g. `docs/product/prd-<feature>.md`), committed as internal maintenance (SKILL.md § What this pipeline is for) and linked from the epic. The epic body holds `## Brief` (if exploration ran) + the PRD path.
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

## Conversion procedure (board)

1. **Epic** — `POST /ticket/new` (`type: "Epic"`, summary `Epic: <feature>`, description = brief + PRD path). Write and commit the PRD file first, so the path resolves.
2. **Children** — one `POST /ticket/new` per story (`type: "Story"` or `"Task"`), summary keeps the ID (`US-001: <title>`), description = story + AC + quality gates, `parent` = the epic's key. Keep the US-ID → key map the server returns; step 3 needs it.
3. **Dependencies** — for each `Depends on:`, put `{type: "blocked-by", key: <blocker key>}` in the **dependent** child's `links` (set it at creation when the blocker already exists, else author it on the note — `local-tracker.md` § Operations). The board reads only `blocked-by`. After wiring, read the children back and run the **cycle check**: walk each child's `blocked-by` edges transitively; if you return to the start, remove the offending edge and re-derive from layer order.
4. **Classification** — set each child's `classification` field to **simple** (clear, isolated, low-risk, obvious COS → machine alone) or **complex** (ambiguous, cross-cutting, risk/security/architecture → co-work). Suggest `risk:low` per trivial child.
5. **Context-mémo bake** (below) — per child.
6. **Readiness verdict** (below) — on the epic.

The epic carries no pipeline label and never enters Planning/Building. The board has no legal transition that closes it; once its last child is Done, close it by roll-up (`local-tracker.md` § Epics).

## Context-mémo bake (the pull-once, cache-as-push step)

For each child, **once at creation**, resolve relevance from the services and packages the ticket touches and query the **decision registry** (`registry.bindingPath`) + `docs/`. Write the result to the ticket's `context` object so the executing agent (Building) never re-reads the docs each iteration:

```json
{
  "binding":      [{ "id": "DEC-0007", "rule": "<recommandation verbatim>", "date": "2026-06-18" }],
  "governingAdrs": [{ "id": "ADR-008", "why": "<1 line: what it constrains for THIS ticket>" }],
  "governingTests": [{ "test": "<pkg/file.go:TestName>", "rule": "<1 line: the invariant it enforces>" }],
  "reference":    [{ "summary": "<1 line>", "link": "<repo path or URL>" }],
  "premises":     [{ "claim": "<a load-bearing fact the plan will rely on>", "evidence": "<file:line or command + output>" }],
  "capturedAt": "2026-06-18",
  "capturedCommit": "<full SHA of origin/main read at shaping>"
}
```

- **Binding → verbatim** (never summarized — a dropped constraint is the exact failure we avoid). **Reference → summary + link.**
- **Ticket touches auth, credentials or a trust boundary? `reference` names every shared credential in play and what it opens.** Not the credential's value — its **blast radius**: which other services or surfaces the same secret unlocks. This is the class of fact that is *true in the deployment config and stated in no ADR*, so the drafter cannot reach it: it reads code and decision history, and `compose.yaml` is neither. TKT-190 is the worked example — the plan draft proposed handing the back office `INTERNAL_SERVICE_TOKEN` to reach one endpoint, correct in every detail except that the same token is one shared value opening commerce's refunds and inventory's operational holds. Caught at plan-review; a mémo entry would have meant it was never drafted.
- **`governingAdrs` → the ADRs that constrain the area this ticket touches**, each with one line on *what it constrains here* — not a bare link. This is the field the planning drafter needs most and the one it cannot discover: it reads the code, not the decision history, so an ADR that makes the obvious solution wrong is invisible to it. TKT-62 is the worked example — the drafter had every fact right and still recommended `CREATE INDEX CONCURRENTLY`, because ADR-008 (migrations run at service startup) is what makes CIC pointless there, and nothing in the code says so. It was fed in by hand at brief time; had the mémo carried it, the draft would have started from the real constraint. Populate it from `registry.bindingPath` by area, not by keyword search.
- **`governingTests` → cross-cutting invariants enforced by a TEST rather than by an ADR.** Same job as `governingAdrs`, different hiding place. An ADR is discoverable by reading `registry.bindingPath`; a rule that lives only in a spec-derived test is discoverable by *failing the gate*, which is the expensive way to learn it. Name the test and the rule in one line each. TKT-235 is the worked example: the plan was correct against every one of its ten governing ADRs and still failed the gate twice on invariants no ADR states — catalog's `write_credential_test.go` derives *"every safe operation must opt out of the staff credential with `security: []`"* from the spec, which makes a staff-authenticated GET **inexpressible** in that contract (the plan had resolved an open decision the other way), and `shared/go/cachetier`'s audit refuses a `Cache-Control` declared without an enum unless the operation sits in a **closed** legacy allowlist. Both cost a full gate run. Find them the way a reviewer would: for each package the ticket touches, skim its `*_test.go` for tests that walk the whole spec/router/schema rather than one behaviour — those are the invariants. **Resolve every entry to a real symbol at HEAD** (`git grep -n "func <TestName>"`) and record the path you found, exactly as the `governingAdrs` rule above requires for ADR files — same failure, different artifact. TKT-251's mémo cited a test in `write_credential_test.go` that lives in `channels_test.go`, copied from an ADR's "tests that pin this" list, which names tests without paths; the drafter caught it and said so, but only after going looking. One grep per test at bake time, when there is time to read. **When the ticket adds a dependency to an event consumer, also grep `smoke/` for tests that PUBLISH that event with synthetic ids** — each one rides the old consumer behaviour and is a governing test (TKT-164: a pinned security-gap test and a poison-event test both published `order.completed` for orders commerce did not know, and both failed the first gate run).
- **`premises` → the load-bearing facts the ticket text and the approach assume**, each with the evidence that established it at bake time (file:line, a command and its output). It gives the claim comment's "premises verified at HEAD" a fixed list to walk, instead of re-deriving it from prose. Shaping is a snapshot; these are the parts of it most likely to go stale.
- **Staleness:** decomposition is pre-sprint; execution comes later. The `id`+`date`+`capturedAt`+`capturedCommit` let the executing agent **revalidate cheaply at pickup** (has any `binding.id` been superseded since `capturedAt`?) instead of re-reading everything. A hidden payload fails silently otherwise — the dates make it detectable.
- **Revalidating means DIFFING the registry, not re-reading the list.** Checking whether the listed ADRs still hold only catches supersession; it cannot catch **an ADR that landed after `capturedAt` and was never listed** — and that one is more dangerous, because the mémo reads complete. At claim time, list every file under `registry.bindingPath` added or changed since the bake — `git log --name-status <capturedCommit>..origin/main -- <registry.bindingPath>` — and say in the claim comment which are relevant and which are not. Diff from the commit, not the date: a date cannot tell an ADR merged before the bake from one merged later the same day. Mémos baked before `capturedCommit` existed fall back to an inclusive date scan (`--since=<capturedAt>` including that day); say so. TKT-244 is the worked example: its mémo was baked 2026-08-10 and **ADR-053 landed that same day**, answering the ticket's one open question (a new per-service credential vs. reusing an existing one) *the opposite way* to what the ticket text asserted as settled. Nothing in the listed ADRs was superseded, so a supersession-only check passes clean while the single most relevant precedent stays invisible. The ticket description is not a safeguard here — it was written before the ADR existed and still asserted the pre-ADR answer.
- **Write** it with the ticket at `POST /ticket/new`, or author it on the note afterwards (`local-tracker.md` § Operations); read it back from `GET /ticket?key=K`. Don't put secrets — everyone with vault access can read it.
- **Tickets created from review findings get a minimal mémo at creation too** — source ticket, code pointers, suspected `governingAdrs`. They bypass normal shaping (born mid-review, often human-moved straight to Ready), so without this the planning brief re-derives everything from scratch — cheap on harness code, risky on product tickets (TKT-92).

## Dependency-aware claiming (used at Ready → Planning)

A ticket is **claimable** only when it sits in `Ready` (priority) **and** has zero open blockers. **Never claim with open blockers**, even if a human moved it to `Ready` — surface the conflict in a `kind=blocker` comment and leave it. Selection when several are claimable: in-flight first (resume), then priority (P0→), then unblocking power (blocks the most others), tie-break lowest key. State the reason in the claim comment.

At closure, recompute claimability for everything this ticket blocked; list the **newly unblocked** in the metrics comment. Don't auto-move them — `Backlog → Ready` stays human.

## Spike-amended scope — re-sync the dependent ticket's ACs

When a child is blocked by a **spike** (timeboxed investigation) and that spike lands a decision, the spike frequently **narrows or amends** the dependent ticket's original scope — a case moved to another service, a sub-feature carved out to a later ticket, an AC that the decision makes moot. Before the dependent ticket reaches **Ready**, **rewrite its Acceptance Criteria to match the spike's decided scope** (cite the spike). Do not leave pre-spike ACs that contradict the spike: Planning then has to re-litigate the contradiction (and a plan-review/AI-review will flag it), burning a cycle. The spike's "concrete output for the dependent ticket" section is the source of truth for the rewritten ACs.

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
