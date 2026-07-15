---
name: sdlc-ticket
description: Drive one unit of work through the agentic SDLC pipeline on JIRA — a 6-status board (Backlog→Ready→Planning→Building→PO Review→Done, + transversal BLOCKED) with label-driven sub-states, plus the code repo for branch/PR/merge. Human gates - priority at entry, plan approval (skipped for risk:low), review+merge, PO acceptance at exit. Use when the user wants to run a task/ticket through the pipeline, says "run this through the SDLC pipeline", "take this ticket through the flow", "resume <ISSUE-KEY>", wants a PRD/spec decomposed into pipeline tickets, wants to explore a feature idea into tickets, or references the "SDLC agentique" board. V0 is manual - the agent performs each stage on the user's command in Claude Code via the Atlassian MCP; there is no event automation.
---

# Agentic SDLC pipeline (Jira)

A unit of work flows through **6 statuses** on the **"SDLC agentique"** Jira project. The state machine lives in **labels**; the board statuses are the coarse human view.

**The symmetry rule:** humans move statuses (Jira transitions), agents move labels. Every transition expresses a human decision; every label change is agent progress within a status.

**V0 is manual.** Everything is triggered by a dev/PO in Claude Code via the **Atlassian MCP** (« crée un epic », « prends le prochain ticket », « fais le découpage »). No Jira automation, no webhooks — those come later (see *Future automation*). Don't wire any rule that auto-moves tickets.

## What this pipeline is for — and what it isn't

**Tickets are for product work. Internal maintenance goes straight to the default branch — no ticket, no gates, no PR.**

Commit directly to `config.code.defaultBranch` when the change is:

- a **skill / process / tooling** improvement (this skill, its references, `.sdlc/`, CI config),
- an **ADR addition or amendment**,
- a **documentation** update (`docs/`, `AGENTS.md`, READMEs).

There is no gate to run because there is no product risk to gate: the local gate (`config.code.localGate`) is the whole check, and if it's green the change is done. Routing this class through Backlog→Ready→Planning→Building→PO Review buys ceremony, not safety — the PO has nothing to functionally validate on a prose diff.

**The line is intent, not file extension.** *"Write down the decision we just made"* → direct. *"Decide whether we should adopt X"* → a ticket, even when its only deliverable turns out to be an ADR (TKT-62 shipped exactly that, and the plan gate is where the recommendation got reversed — that gate earned its keep). If the change needs a **decision** or a **plan**, it's a ticket. If it only needs **writing down**, it's a commit.

Still applies to direct commits: the local gate must be green, and the commit message carries the reasoning.

## Two systems, two jobs

Don't conflate them — the old GitHub-only skill did, and it hid this:

| System | Owns | Tools |
|---|---|---|
| **Jira** (+ Confluence) | ticket lifecycle (statuses), the thread/memory (comments), dependencies (issue links), the hidden context-mémo (entity property), the PRD (Confluence/description) | Atlassian MCP |
| **Code repo** (GitHub) | branch, commits, PR, CI, the **merge gate** | `git`, `gh`, `codex` |

The Jira ticket carries the key in the branch/PR (`<ISSUE-KEY>`); the PR carries `<ISSUE-KEY>` in its title/body so the two stay linked.

## Human gates

1. ⛔ **Priority** — human transitions `Backlog → Ready`.
2. ⛔ **Plan** — human transitions `Planning → Building`. **Skipped when the issue carries `risk:low`.**
3. ⛔ **Review + merge** — human reviews the PR and **merges (squash) in the code repo**, then transitions `Building → PO Review`. Agents never merge.
4. ⛔ **PO acceptance** — the PO functionally validates and transitions `PO Review → Done`.

> **Custom agentic workflow — deliberately richer than the org standard.** The agent stages (`Planning`, `Building`) are **first-class Jira statuses**, not just labels, because the label axis alone isn't visible enough for human + AI co-work — that visibility is the point of this pipeline. **Your org's admin route** creates the bare Jira project + Confluence space (you can't self-create them); **you** (project admin) then build this workflow/board yourself — provisioning carries no workflow options, so nothing to negotiate. Two things are kept from the standard because the business wants them: the **PO Review** status (gate 4) and the transversal **BLOCKED** status. `references/setup.md` §1 has the details.

## References (load on demand)

Read the relevant file **before** executing the step — don't improvise the how:

| File | Read when |
|---|---|
| `references/setup.md` | **First run in a project** — `.claude/sdlc.config.json` missing: bootstrap the Jira project/board/statuses + Confluence space, then write the config |
| `references/decomposition.md` | Intake is a PRD/spec or an epic bigger than one ticket — epic + child issues, sizing, dependency links, **context-mémo bake**, readiness verdict |
| `references/exploration.md` | Intake is a raw idea, not a spec — coached brainstorm / brief / PRFAQ → brief in the epic |
| `references/shaping.md` | Ticket in Backlog needs preparing for Gate 1 — the 8-item DoR (`readiness`, incl. the context-mémo bake), the shaping pass, spikes (timeboxed investigation tickets), human decisions |
| `references/quality-practices.md` | Pre-mortem on plans (Planning), findings triage + review-guide hand-off (Building), correct-course when a human pushes back |
| `references/local-tracker.md` | `config.tracker == "local"`, a demo, or no Jira available — repo-contained backend: one JSON file per ticket on a dedicated `sdlc-state` branch (auto-bootstrapped worktree) + live board. Same labels/markers/gates; only the storage changes |

## Before you start

- **First run in this project?** If `.claude/sdlc.config.json` is missing, the Jira board / Confluence space don't exist yet — read `references/setup.md`, bootstrap them, write the config, then continue.
- **Local backend?** If `config.tracker` is `"local"`, the user asks for a demo, or Jira isn't ready — read `references/local-tracker.md` and drive the repo-contained store instead of Atlassian (tickets live on the `sdlc-state` branch, mutations via the board server's `POST /ticket`). Same rules; only storage changes.
- **Resuming?** If the ticket is in flight (or the user says "resume <KEY>"), reconstruct state from the Jira board status + label + the marker comments — never from conversation memory — and continue from the step the label indicates.
- **Drive style:** V0 is interactive — the user drives, in Claude Code. Ask **once at the start of the run**: *every step* (stop and report after each transition) or *gates only* (run through, pause at the human gates). Apply the answer for the rest of the ticket; don't re-ask at each stage.
- **Risk:** agree on `risk:low` (skips the plan gate) for trivial, isolated work.

## Config (per project) — this skill is generic

Project-specific bindings live in **`.claude/sdlc.config.json`** in the code repo, not in this skill. Read it at the start of every run; anything below shaped like `config.jira.projectKey` comes from it.

```json
{
  "tracker":    "jira | local (see references/local-tracker.md; jira keys below apply only to jira)",
  "jira":       { "projectKey": "<KEY>",
                  "statuses": { "backlog": "Backlog", "ready": "Ready", "planning": "Planning", "building": "Building", "poReview": "PO Review", "done": "Done", "blocked": "BLOCKED" },
                  "transitions": { "ready->planning": "<resolved at setup>", "building->poReview": "<...>" },
                  "blocksLinkType": "Blocks" },
  "confluence": { "spaceKey": "<SPACE>" },
  "code":       { "repo": "<owner/repo>", "defaultBranch": "main",
                  "localGate": "<project's local gate cmd, mirrors CI>" },
  "models":     { "plan": "main-agent", "planReview": "gpt-5.6-sol",
                  "implement": "main-agent", "aiReview": "gpt-5.6-sol" },
  "registry":   { "bindingPath": "docs/decisions/", "referenceLocation": "confluence" }
}
```

### `config.models` — who does each stage

Four stages are model-assignable. **The config declares it; the agent never infers it.** Read the value, resolve it via the table below, run it. Do not detect which model is driving the session, and do not branch on it — if a project wants a different planner, that is a config edit, not a runtime decision.

**You are always the orchestrator.** Whatever `config.models` says, *you* read the ticket, brief the worker, **talk to the human**, revise, post every comment, and own the outcome. These four keys assign **who does the thinking for a stage** — never who drives the board or who holds the conversation. A `gpt-*` or subagent worker **cannot ask the human anything**: it gets one brief and returns one answer. So any step that needs a human decision (grilling on open decisions, a gate hand-off) is **yours**, in this session, no matter what the config says.

| Value | How to run that stage |
|---|---|
| `main-agent` | **You** do it — whichever model drives this Claude Code session. Never hardcode which one that is. |
| `gpt-5.6-sol` (any `gpt-*`) | Codex. Reviews → the companion script's `adversarial-review`; free-form work (plan drafting, implementation) → `task --prompt-file … --model gpt-5.6-sol`. See Hard rules. |
| `fable-5`, `opus-4.8`, `sonnet-5` (any Claude model) | A subagent: the `Agent` tool with `model:` set to it. Brief it from the ticket + the approved plan — it does **not** inherit this session's context. |

**⛔ The reviewer must never be the author.** Cross-model review is the pipeline's load-bearing invariant, and it constrains **two pairs**:

- `plan` ≠ `planReview`
- `implement` ≠ `aiReview`

Check both when you read the config; if either matches, **stop and report** — don't run a degraded flow. A model reviewing its own output re-confirms rather than falsifies, which is the one thing this pipeline cannot afford to lose. Note `main-agent` and a named Claude model can *collide*: if the session model is the named one, that's the same violation wearing two labels. When unsure, ask rather than assume.

Swapping `plan` ↔ `planReview` is how the "Codex drafts, main agent reviews" variant is expressed — a config flip, not a fork of this skill.

**Cross-model review is a prerequisite, not an option.** It backs two stages (`agent:plan-review`, `agent:ai-review`). If an environment forbids sending code to an external model, this SDLC doesn't apply there — don't build a degraded variant.

Resolve transition names (`getTransitionsForJiraIssue`) and the blocks link type (`getIssueLinkTypes`) at runtime — both vary by instance — and store them in `config.jira.transitions` (keyed `<from>-><to>`) and `config.jira.blocksLinkType` so later runs don't re-discover them.

## Labels (the state machine — Jira labels)

| Label | Meaning |
|---|---|
| `agent:planning` | Drafting the execution plan against the real code |
| `agent:plan-review` | Codex cross-model critique of the plan; agent revising |
| `agent:coding` | TDD in progress (tests first → implement → local gate green) |
| `agent:ai-review` | PR open; Codex adversarial review; agent fixing findings |
| `needs:human` | **Agent has stopped — ball is in the human's court.** Status disambiguates: in `Planning` = plan approval; in `Building` = PR review + merge; in `PO Review` = PO functional validation |
| `risk:low` | Orthogonal modifier set at creation: skip the plan gate. **Challengeable, not gospel** — see invariants |

**Invariants:**
- Exactly **one** pipeline label (`agent:*` or `needs:human`) on an in-flight ticket (`Planning`/`Building`). None in `Backlog`, `Ready`, `Done`.
- Once `needs:human` is set, the agent **stops pushing commits**. Never make the human review a moving target. On resume (changes requested, merge conflict), swap back to the relevant `agent:*` label first.
- **`risk:low` is challengeable.** The claim comment must restate *why* the risk is low. If the work turns out to touch auth, data migrations, CI/deploy config, or >5 files, drop the label (say so in a comment) — the plan gate comes back. A mislabeled trivial ticket must never bypass human judgment silently.
- **Never claim a ticket with open blockers**, even if a human moved it to `Ready` — the status is priority, not feasibility. Surface the conflict instead (`references/decomposition.md` covers links).
- **The Jira comment thread is the memory.** Every `agent:*` stage ends with a marker-tagged comment (`<!-- sdlc:stage=… kind=… -->` on line 1) *before* the label moves; every stage and every resume starts by reading the board + thread, not conversation memory. Comments without a marker are human steering — incorporate and acknowledge them.
- Per-stage durations come from the **Jira issue changelog** (status + label history), never hand-tracked.

## The 6 statuses

| # | Status | Entry means | Agent does (label sequence) | Exit gate |
|---|--------|-------------|------------------------------|-----------|
| 1 | `Backlog` | issue created | Write **COS** (Conditions of Success — this pipeline's term for acceptance criteria) + scope; suggest `risk:low` if trivial. **Every pipeline ticket gets the context-mémo bake** (`decomposition.md` § context-mémo) — standalone tickets too, not just PRD children. **Raw idea →** explore first (`exploration.md`). **PRD/multi-ticket →** decompose (`decomposition.md`): epic + children + dependency links + context-mémo bake, ending with the readiness verdict. **Then shape** (`shaping.md`): fill the 8-item DoR (`readiness` field — `context_memo` is one of them, so the bake is gate-enforced, not merely instructed), spawn spikes for investigations, flag human decisions (`owner: "human"`). | ⛔ human → `Ready` — **hard-blocked while any DoR item is `open` or a blocker is open** (`deferred` passes) |
| 2 | `Ready` | approved queue | Verify **zero open blockers**, then claim: assign self, create branch `<ISSUE-KEY>-<slug>` off `origin/main` in the code repo, post a claim comment with selection reason. | agent claims → `Planning` |
| 3 | `Planning` | agent claimed | `agent:planning`: **`config.models.plan`** reads the real code + the ticket's context-mémo, picks the **test seam**, drafts the plan (DoD, files, test plan) → `agent:plan-review`: **`config.models.planReview`** critiques it adversarially, grills the human on open decisions (complex tickets), pre-mortem pass, posts the final plan → `needs:human` (skip if `risk:low`). | ⛔ human → `Building` |
| 4 | `Building` | plan approved | `agent:coding`: **`config.models.implement`** does TDD from the approved plan, local gate green, push, open PR (`<ISSUE-KEY>` in title/body) → `agent:ai-review`: **`config.models.aiReview`** adversarially reviews the diff, triage findings, fix, rebase if behind, re-green, second pass if the fixes were non-trivial → `needs:human`: post the review-guide on the PR, stop. | ⛔ human reviews + **merges** → `PO Review` |
| 5 | `PO Review` | human merged (deployed to DEV) | `needs:human`: post a validation note showing the **COS are met** from the user's output (via code if not user-demonstrable), + any preview link; stop pushing. | ⛔ PO validates COS + gives final go before client TEST deploy → `Done` |
| 6 | `Done` | PO accepted | Remove `needs:human`; verify PR merged + the **project DoD** is satisfied (see `references/setup.md`); post the metrics comment (durations from the Jira changelog); promote reusable learnings; delete branch. | — |

**`BLOCKED`** is transversal, not a row: a human moves any active ticket there when it hits an open blocker, and back to `Ready`/`Planning`/`Building` by context. The agent keeps its prior `agent:*`/`needs:human` label and stops until it returns.

## Transitions (V0 — manual, via MCP + code repo)

Each step is an agent action on the user's command. **Jira ops = Atlassian MCP tools; code ops = `git`/`gh`/`codex`.**

```
# 1 Entrée (Jira)
#   MCP createJiraIssue (project=JIRA_PROJECT_KEY, type=Epic|Story|Task, summary, description, AC)
#   add label risk:low only if agreed.  Status starts at Backlog.
#   If PRD/spec → references/decomposition.md (epic + children + links + context-mémo + verdict).
#   ⛔ GATE 1 — wait for the human to transition Backlog → Ready.

# 2 File + prise en charge
#   Verify zero open blockers (MCP getJiraIssue → issuelinks "is blocked by", all resolved).
#   MCP editJiraIssue: assign self (no pipeline label yet — the first one is set in step 3).
#   Code repo:  git fetch && git switch -c <ISSUE-KEY>-<slug> origin/main
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=ready kind=claim --> + selection reason.
#   agent transitions Ready → Planning (claim is the agent's move).

# 3 Plan (Jira)
#   MCP editJiraIssue add-label agent:planning
#   3a DRAFT — by config.models.plan. Read the ticket + agent-context property yourself first
#      (you brief the drafter; don't make it guess the mémo). Whoever drafts: read the real code,
#      pick the test seam, produce DoD / files / test plan / risks.
#      If plan == main-agent: draft it yourself.
#      Else: brief it. Write .plan-brief.$$.md with the Write tool (NOT printf/echo — ticket text
#      runs shell metacharacters): COS, scope, context-mémo, constraints, the required plan shape.
#      REQUIRED SECTION — "governing ADRs": name the decisions constraining the touched area and
#      what each constrains *for this ticket* (from the mémo's governingAdrs; if absent, resolve
#      them from registry.bindingPath yourself — do not skip). The drafter reads code, not decision
#      history: an ADR that makes the obvious solution wrong is invisible to it, so it recommends
#      the wrong thing with every fact correct (TKT-62 — ADR-008 is what made CIC pointless).
#        gpt-*  → node "$CODEX" task --prompt-file .plan-brief.$$.md --model <plan> --effort high \
#                   --background     # read-only: NO --write, the drafter never touches the tree
#                 node "$CODEX" status <job-id> ; node "$CODEX" result <job-id>   # task honours --background
#                 ($CODEX = ~/.claude/plugins/marketplaces/openai-codex/plugins/codex/scripts/codex-companion.mjs)
#        claude → Agent tool, model:<plan>, read-only brief; it does NOT inherit this session.
#      Delete only the file this run created. Post the draft attributed to the model that wrote it.
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=planning kind=plan -->
#   MCP editJiraIssue: -agent:planning +agent:plan-review
#   3b CRITIQUE — by config.models.planReview (≠ plan, enforced). Adversarial, not a rubber stamp.
#      Verify every file/symbol/test seam the draft names actually EXISTS (`git grep -n` at HEAD) —
#      a drafter that couldn't run the gate will hallucinate seams. Check it against the real code,
#      the COS, and the registry; pre-mortem lens (quality-practices.md §1).
#      Accept / amend / reject each part of the draft with a stated reason.
#      Same routing as 3a: main-agent → you; gpt-* → codex `task`; claude → Agent tool.
#   3c FINALIZE — YOU, always, whatever config.models says (a delegated worker cannot talk to the
#      human): grill the human on the open decisions (complex tickets, one question at a time),
#      revise, post kind=plan-final (final plan + what changed from the draft and why).
#      REQUIRED SECTION — "introduced at plan-review, reviewed by no second model": list every
#      design element 3b *added* rather than accepted/amended/rejected from the draft, and say the
#      words. The cross-model guarantee covers the draft (3a reviewed by 3b) and the code (step 4
#      reviewed by aiReview) — it does NOT cover 3b's own inventions, which reach Gate 2 unreviewed
#      by anyone but their author. Name them, and name them again in the Gate 2 hand-off, so the
#      human knows which parts are load-bearing AND unreviewed. If the list is empty, write `none`.
#      (TKT-57: planReview invented the whole async-checkpoint scheme at 3b; Gate 2 approved it in
#      19 seconds; both aiReview passes then found it materially wrong. It was caught only because
#      the ADR *was* the deliverable — on a code ticket the premise ships and aiReview reviews the
#      code, not the premise. This section is the cheapest thing that would have surfaced it.)
#   If risk:low → skip to step 4.
#   MCP editJiraIssue: -agent:plan-review +needs:human
#   ⛔ GATE 2 — wait for the human to transition Planning → Building.

# 4 Réalisation (code repo + Jira)
#   MCP editJiraIssue: -needs:human +agent:coding
#   Code: TDD, by config.models.implement. The approved kind=plan-final IS the brief — if it isn't
#   enough to hand over, that's a plan bug; fix the plan, don't paper over it in-session.
#     main-agent → you implement.
#     gpt-*      → node "$CODEX" task --prompt-file .impl-brief.$$.md --model <implement> --write
#     claude     → Agent tool, model:<implement>, on the ticket's branch in this checkout.
#   One implementer, no worktree — WIP is one ticket, and a TDD loop doesn't parallelize.
#   Local gate (mirrors CI): run config.code.localGate. If you delegated, VERIFY don't trust —
#   re-run the gate yourself on the committed tree (quality-practices.md §2.b).
#   Code: commit (no AI attribution), push; gh pr create --base main --title "<ISSUE-KEY> …" --body "…<ISSUE-KEY>…"
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=coding kind=summary --> (+ stage YAML; name the implementer)
#   MCP editJiraIssue: -agent:coding +agent:ai-review
#   REVIEW — by config.models.aiReview. gpt-* reviews the branch diff directly, no prompt file:
#     node "$CODEX" adversarial-review --base origin/main --scope branch \
#       "Assume this diff is wrong and find where. Correctness, security, coverage. \
#        Challenge the approach, not just defects. You have the diff only — that is deliberate."
#   (--background is IGNORED for reviews: background via the harness or wrap in `timeout`)
#   (claude → Agent tool with the diff; main-agent → only valid if implement was NOT main-agent)
#   triage findings (quality-practices.md §2): blocking→fix in PR; incidental→new backlog ticket; rejected→stated reason.
#   rebase on origin/main if behind; re-green.
#   SECOND PASS — re-run the same adversarial-review iff the fixes were NON-TRIVIAL (see Hard rules).
#   Trivial → one pass, say so in the stage comment. Second pass findings are triaged the same way;
#   there is no third — if it's still churning, stop and hand to the human.
#   MCP comment kind=summary (ai-review): per-finding verdicts, what was fixed, and whether the
#   second pass ran + why.
#   MCP editJiraIssue: -agent:ai-review +needs:human ; post the review-guide on the PR. STOP pushing.
#   ⛔ GATE 3 — human reviews + merges (squash) in the code repo, then transitions Building → PO Review. Do NOT merge yourself.
#     Merge conflict? swap back to agent:coding, rebase, re-green, back to needs:human (re-review the new SHA).

# 5 Validation PO (Jira)
#   needs:human stays set (ball is with the PO).
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=po-review kind=summary --> validation note (what to check, preview/deploy link).
#   ⛔ GATE 4 — the PO functionally validates and transitions PO Review → Done.
#     Changes requested? swap needs:human back to agent:coding, fix in the PR (re-open if needed), re-green.

# 6 Clôture (Jira)
#   after PO accepts (status = Done):
#   MCP editJiraIssue: -needs:human.
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=done kind=metrics --> (durations from changelog).
#   promote reusable learnings (see Memory below); code: delete the merged branch.
```

## Memory & metrics (Jira)

- **Thread = memory.** Markers on line 1: `<!-- sdlc:stage=<backlog|ready|planning|plan-review|coding|ai-review|po-review|done> kind=<claim|plan|plan-final|summary|blocker|metrics|readiness> -->`. The `review-guide` lives on the **PR**, not Jira. Resume = read the board status + label + the latest of each marker kind + any human comment after the last agent comment.
- **Cross-ticket memory.** Reusable learnings (codebase patterns, gotchas) go to a dedicated **"🧠 Agent memory" Jira issue** (or the team decision registry). When a pattern proves out, promote it per the **"3 houses" rule** (`references/setup.md`): **technical** standards → the team's shared standards registry (via PR), **process** learnings → the team wiki, repo-local guidance → `AGENTS.md` via a normal gated ticket. Working memory ≠ gospel.
- **Promotion is a mandatory Done step, not best-effort.** The `kind=metrics` closing comment must include a `learnings:` section — either concrete promotable items (each with its target house) or the explicit word `none`. This is what feeds the registry; skipping it is the registry cold-start failure mode.
- **Metrics.** Durations from the Jira changelog (status/label transitions); `needs:human` time-in-state is the Gate 2 / Gate 3 human wait — a first-class metric, the likely bottleneck. Diff size from `gh pr view`. The `kind=metrics` comment has **required fields** — a table `stage | duration` (one row per status + per `agent:*` label), `human-wait total`, `diff: +x/−y (n files)`, the `learnings:` section, and a `retro:` section. All five, every ticket; missing data is written `n/a`, not omitted.
- **`retro:` = introspection on the collaboration, not the code.** Two fixed questions, answered honestly (`nothing` is allowed but must be earned): **(1)** *What would have made this ticket faster or better — missing context in the mémo, ambiguous COS, a gap in this skill?* **(2)** *What should the humans change — in the process, the config, or how the ticket was written?* Each retro item is phrased as a **concrete, appliable change** (which file/section of the skill, config, or ticket template, and what edit) — not an observation. **V0 is interactive: after posting the retro, offer to apply the changes now**; if the user approves, patch the skill/config/template in the same session and note `applied` next to the item. Unapplied items land in the weekly registry review; a recurring one is a signal to stop re-noting and patch.

## Hard rules

- **Never merge for the human.** Gate 3 is the human's, in the code repo. Agents stop at `needs:human`.
- **Model routing is declared, never inferred.** Every model decision comes from `config.models` (§ Config). Resolve by prefix — `main-agent` → you; `gpt-*` → codex; any Claude model → the `Agent` tool with `model:`. **Never detect which model is driving the session and branch on it**, and never silently substitute a different model than the config names: if the configured one is unreachable, **stop and report**. Who did the work is a thing this pipeline measures — a stage that quietly ran on the wrong model is a corrupted measurement, not a shortcut. **This rule is the skill's own, deliberately self-contained:** it ships to whoever installs the skill, so it must not depend on any individual's personal model preferences or global config.
- **Delegated ≠ done.** When a stage resolves to anything but `main-agent`, you still own the outcome: verify the output against the real code, re-run the gate yourself (`quality-practices.md` §2.b), and attribute the work in the stage comment. A subagent or codex run reports a *claim*; the evidence is what you check.
- **Reaching Codex — call the plugin's companion script directly; it is agent-invocable.** Where `codex@openai-codex` is installed, run the **companion script**, not raw `codex exec` and not the `/codex:*` slash commands. The slash commands are `disable-model-invocation: true` (human-only) and the agent triggers them inconsistently; the script they wrap is agent-invocable and **works every time** — that reliability is the whole reason to prefer it.
  ```bash
  CODEX=~/.claude/plugins/marketplaces/openai-codex/plugins/codex/scripts/codex-companion.mjs
  # free-form work (plan draft / plan critique / implementation) — `task` takes a model.
  # Read-only stages: NO --write. Implementation: --write.
  node "$CODEX" task --prompt-file .brief.$$.md --model gpt-5.6-sol --effort high --background
  node "$CODEX" status <job-id> ; node "$CODEX" result <job-id>
  # ai-review (step 4) — the PR diff vs the base branch
  node "$CODEX" adversarial-review --base origin/main --scope branch "<focus: what to attack>"
  ```
  Which stage uses this at all is `config.models`' call, not this rule's — the mechanics below apply whenever a stage resolves to a `gpt-*` model.
  **`adversarial-review`** is the review command: it takes focus text and challenges the approach. Plain `review` is native-review only and **rejects focus text entirely**. Both map to Codex's built-in reviewer — **no `-m` flag to get wrong**; args `--base <ref>`, `--scope auto|working-tree|branch`. **`task` is the only command that takes a model** — pass `--model gpt-5.6-sol` explicitly. **The literal is `gpt-5.6-sol`; plain `gpt-5.6` does not work.** Don't "correct" it.
- **⚠ `--background` is a lie for reviews — verified in the companion script.** It is parsed and then **ignored**: `review`/`adversarial-review` unconditionally call `runForegroundCommand` (only `task` honours it), so `status`/`result` polling does **not** work for review jobs and a stalled review blocks the caller. The plugin's own command docs are wrong about this. **Instead: run the review through the harness's own backgrounding** (`Bash(..., run_in_background: true)`) or wrap it in `timeout`, so a hang is caught rather than waited out. `--wait` is equally redundant — foreground is the only mode.
- **A prompt file must be materialized, never interpolated.** Write it with a **file-writing tool or a quoted heredoc**, never by interpolating ticket/plan text into a shell command — ticket text and inlined code routinely contain backticks and `$(…)`, which a double-quoted `printf`/`echo` will **execute**. Use a collision-resistant name (`.plan-critique.$$.md`), delete only what this run created, and keep it lean (plan + the code actually under discussion) — a huge file risks being dropped from context.
- **Raw `codex exec` — fallback only, when the plugin is absent.** (a) **Pass `-m gpt-5.6-sol` explicitly** — the CLI default errors `requires a newer Codex`. (b) Feed the prompt via **STDIN redirect** (`< prompt.txt`), never as a positional arg — a large arg hangs at `Reading additional input from stdin…`. (c) **Inline the plan/diff in the prompt file** rather than telling codex to `git diff` itself. (d) Run it **backgrounded + poll**. (e) **A local skill will hijack the prompt and cannot be stopped** — "review"/"critique" loads the machine's skills (possibly an *unrelated client's*); neither `--disable plugins` nor `-c features.skills=false` prevents it (`--disable skills` isn't a valid flag). This hijack also happens *through the plugin*. Survivable, not preventable.
- **Codex failing ≠ stage done — and exit 0 ≠ success.** **Read the output; never judge by exit status** (raw `exec` exits 0 on both a zero-findings hijack and a bad flag that never reached the model). Judge by **whether the reviewer actually ran against the intended scope and returned a conclusive verdict** — *not* by whether it found defects. **A conclusive "no findings" on a genuinely clean diff is a PASS**; a run that reviewed the wrong scope, or returned only a rubric template, is a failure however many sections it printed. A run that announces a foreign skill but returns concrete, code-cited findings is likewise a pass — the rubric is irrelevant. Never substitute a self-review; a skipped review that looks done is worse than a stalled ticket. A hang counts as a failure: kill it fast.
- **The escalation ladder when a delegated stage fails — fixed, not improvised.** "Retry once" is ambiguous once there are two ways to reach the same model, and the ambiguity costs wall-clock (TKT-62 burned ~1h on three attempts before stopping). The ladder, in order, **one attempt each**: **(1)** the plugin command (`adversarial-review` / `task`); **(2)** on empty, truncated, or hung output → the documented raw fallback (`codex exec -m <model> -s read-only`, prompt via **stdin redirect**, diff/plan **inlined**, backgrounded); **(3)** still no conclusive verdict → **stop on the current `agent:*` label and report to the human**. No rung twice, no fourth rung. Known failure shapes, all exiting 0: the plugin returning **zero bytes**; the companion **truncating** the captured assistant message mid-sentence (the verdict survives, the findings don't); `codex exec` **dying mid-investigation** with no final answer. If a partial run left legible reasoning, salvage it — quote it in the stage comment and verify its open threads **yourself, labelled as your own work**, which is authorship, not a substituted review.
- **Verify Codex's findings against the revision under review — not the base branch.** The pass is a *lead generator*, not an oracle. Check each finding with `git grep -n "<sym>" HEAD` (or the working tree); use `origin/main` **only** to ask whether something pre-existed the change. **Make sure your checkout is current first** — a stale local `main` (no `git pull` after the last merge) is what makes this go wrong. Grepping the base for a symbol the PR *introduces* finds nothing and looks like a refuted finding. Record a per-finding verdict in the stage comment; surviving findings are the review's value, the rest is noise you'd otherwise promote into the codebase.
- **Prime the reviewer for guilt, and give it the diff only.** The reviewer is told to **assume the diff is wrong and find where** — not to "check whether it's ok". An author-model wants its code accepted; a reviewer-model must want to find problems, and the framing is what buys that. Withholding the plan and the reasoning is **deliberate, not an oversight**: a reviewer that has read the justification evaluates the code against the author's intent instead of against reality. Don't "help" it by pasting in the plan.
- **A second `ai-review` pass when the fixes were non-trivial — otherwise one.** The second pass exists because **the fix diff is code no one has reviewed** — the first pass reviewed what the fixes replaced. So judge the *fix diff*, not the original PR diff. **Non-trivial = any of:**
  - **Any blocking finding was fixed** — blocking means correctness, security, or an AC violation, and you cannot fix one of those without changing logic. **This is non-trivial by construction**, so a real review round almost always earns a second pass. The one-pass path exists only for cosmetic findings.
  - **Aggregate volume**, even when each fix was individually small — many small edits still add up to a fix diff worth reviewing on its own. Judge the total, not the per-fix verdict.
  - New/changed control flow or logic; a changed function signature / public API / schema / migration; a touched money, auth, or concurrency path; a new or materially rewritten test; or fixes spilling into files the findings didn't name.

  Any one of those → **re-run the same `adversarial-review`** on the new diff and triage it identically. Pure trivia (typos, comments, formatting, a rename the compiler verifies) → one pass; record the call in the stage comment either way. A rebase that materially changed the diff is also a trigger.

  **Cap: two passes per *stable* diff — but a pass that invalidates the previous pass's fix resets the counter (absolute cap 4).** The cap exists to stop **churn**: one diff going round in circles. It must not fire while the diff is **converging on a bug nobody had found yet**. Tell them apart by what the new pass actually did:
  - **Churn** (cap applies — stop, hand to the human): re-litigating style, resurfacing already-triaged findings, findings that shrink in severity each round.
  - **Convergence** (reset the counter, run one more): a pass showed the previous fix was **wrong at the root**, and the response was a **structural rewrite**. That rewrite is new, unreviewed code — typically the largest and riskiest commit on the branch. Stopping there hands the human the **worst-reviewed commit in the ticket**, which is exactly backwards.

  TKT-61 is the worked example: pass 2 proved pass 1's fix was hollow (it parked only the future variants that happened to be shaped like the present one), the rewrite that followed was the biggest commit on the branch, and the old flat cap sent it to Gate 3 with **no AI pass at all**. Whenever you do stop at the cap with an unreviewed fix diff, **say so explicitly in the stage comment *and* the review-guide**, and label your own verification as your own — an undisclosed unreviewed rewrite is the failure this rule exists to prevent. **A Gate-3 human review round is not:** the human review *is* the authoritative adversarial pass, so address its changes under `agent:coding`, re-green, and return to `needs:human` for re-review of the new SHA — without a new codex run.
- **V0 is manual** — no Jira automation / webhooks. Movement is agent-driven on the user's command.
- **Never push after setting `needs:human`** without first swapping back to an `agent:*` label.
- **When a human pushes back** — diagnose the failing layer first (**intent / plan / implementation**) and regenerate from that layer, never patch locally at a lower one (`quality-practices.md` §4).
- **TDD in `Building`** — tests first; local gate = `config.code.localGate` (mirrors CI). Required CI checks are the project's configured required checks on `config.code.defaultBranch`.
- Branch protection on `config.code.defaultBranch` — **for ticketed product work**: PR required, required checks must pass, no direct push. **Internal maintenance is exempt** (skill/process/tooling, ADRs, docs — see § What this pipeline is for): commit it straight to the default branch with the local gate green. Verify what the host actually enforces rather than trusting this line — on GitHub, `gh api repos/<owner>/<repo>/branches/<branch>/protection`; a 403 means the feature isn't on the repo's plan and *nothing* is enforced, whatever this skill claims.

## Future automation (design intent, not implemented)

When V0 has proven out: Jira **automation rules / webhooks** become the trigger surface. A human gate becomes a status transition (or an `/approve` comment a rule converts); a rule notifies the agent runner. The merge gate stays human, in the code repo. Nothing in this skill should depend on automation — the manual path is the source of truth.

## Notes

- Atlassian MCP tools: `createJiraIssue`, `editJiraIssue`, `transitionJiraIssue`, `getTransitionsForJiraIssue`, `getJiraIssue`, `searchJiraIssuesUsingJql`, `addCommentToJiraIssue`, `createIssueLink`, `getIssueLinkTypes`, `createConfluencePage`. **Entity-property writes** (the context-mémo `agent-context` key) are **not exposed by the MCP** — write via the Jira REST API (`PUT /rest/api/3/issue/{key}/properties/agent-context`); read back via `getJiraIssue` (properties). Confirm on your instance. **Fallback when REST property writes are unavailable** (client blocks API tokens): store the same payload as an HTML-comment block at the *end* of the issue description — `<!-- sdlc:context\n{…json…}\n-->` — via `editJiraIssue`. Readers check the entity property first, then the description block; whichever exists is the mémo.
- This supersedes the GitHub-based 9-stage and 5-column versions. The GitHub variant lives on the `sdlc-pipeline-5-columns` worktree branch for reference.
- Lighter than the repo's metaswarm workflow — don't auto-invoke metaswarm gates here unless asked.
