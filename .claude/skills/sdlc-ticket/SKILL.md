---
name: sdlc-ticket
description: Drive one unit of work through the agentic SDLC pipeline on JIRA — a 6-status board (Backlog→Ready→Planning→Building→PO Review→Done, + transversal BLOCKED) with label-driven sub-states, plus the code repo for branch/PR/merge. Four gates - priority at entry, plan approval (skipped for risk:low), review+merge, PO acceptance at exit; held by a human or taken by the agent per config.gates ("human" | "autonomous"). Use when the user wants to run a task/ticket through the pipeline, says "run this through the SDLC pipeline", "take this ticket through the flow", "resume <ISSUE-KEY>", wants a PRD/spec decomposed into pipeline tickets, wants to explore a feature idea into tickets, or references the "SDLC agentique" board. V0 is manual - the agent performs each stage on the user's command in Claude Code via the Atlassian MCP; there is no event automation.
---

# Agentic SDLC pipeline (Jira)

A unit of work flows through **6 statuses** on the **"SDLC agentique"** Jira project. The state machine lives in **labels**; the board statuses are the coarse human view.

**The symmetry rule:** humans move statuses (Jira transitions), agents move labels. Every transition expresses a human decision; every label change is agent progress within a status.

**V0 is manual.** Everything is triggered by a dev/PO in Claude Code via the **Atlassian MCP**. No Jira automation, no webhooks — those come later (see *Future automation*). Don't wire any rule that auto-moves tickets.

## What this pipeline is for — and what it isn't

**Tickets are for product work. Internal maintenance goes straight to the default branch — no ticket, no gates, no PR.**

Commit directly to `config.code.defaultBranch` when the change is:

- a **skill / process / tooling** improvement (this skill, its references, `.sdlc/`, CI config),
- an **ADR addition or amendment**,
- a **documentation** update (`docs/`, `AGENTS.md`, READMEs).

There is no gate to run because there is no product risk to gate: the local gate (`config.code.localGate`) is the whole check, and if it's green the change is done. Routing this class through the board buys ceremony, not safety.

**The line is intent, not file extension.** *"Write down the decision we just made"* → direct. *"Decide whether we should adopt X"* → a ticket, even when its only deliverable turns out to be an ADR — the plan gate is where a wrong recommendation gets reversed (TKT-62). If the change needs a **decision** or a **plan**, it's a ticket. If it only needs **writing down**, it's a commit.

Still applies to direct commits: the local gate must be green, and the commit message carries the reasoning.

## Two systems, two jobs

| System | Owns | Tools |
|---|---|---|
| **Jira** (+ Confluence) | ticket lifecycle (statuses), the thread/memory (comments), dependencies (issue links), the hidden context-mémo (entity property), the PRD (Confluence/description) | Atlassian MCP |
| **Code repo** (GitHub) | branch, commits, PR, CI, the **merge gate** | `git`, `gh`, `codex` |

The Jira ticket carries the key in the branch/PR (`<ISSUE-KEY>`); the PR carries `<ISSUE-KEY>` in its title/body so the two stay linked.

## Gates

Four decision points. **Who decides is a config binding** — `config.gates`: `"human"` (the default when the key is absent — every ⛔ line in this skill and its references waits for the human, exactly as written) or `"autonomous"` (the agent takes gates 2–4 itself and runs Ready → Done in one continuous run — see § Autonomous mode). Everything else in the skill applies identically in both modes.

1. ⛔ **Priority** — human transitions `Backlog → Ready`. **Human in both modes** — the agent never self-promotes out of Backlog: shaping + the DoR still gate entry, and priority is the owner's call.
2. ⛔ **Plan** — human transitions `Planning → Building`. **Skipped when the issue carries `risk:low`.**
3. ⛔ **Review + merge** — human reviews the PR and **merges (squash) in the code repo**, then transitions `Building → PO Review`. Agents never merge in human mode.
4. ⛔ **PO acceptance** — the PO functionally validates and transitions `PO Review → Done`.

### Autonomous mode (`config.gates: "autonomous"`)

The four human gates are replaced by agent judgement; **every other rule is unchanged and non-negotiable** — `config.models` routing (never substitute), cross-model review for both plan and code, TDD with tests observed red first, local gate green before every push and after the merge, all marker comments at each stage, no AI attribution anywhere.

State the mode in the **claim comment** ("running gateless per `config.gates: autonomous`") so the board history shows the ticket ran without human gates.

**Gate replacements:**

- **Gate 1 (priority):** not replaced — the ticket must already be in `Ready`. Verify that against the board (§ Before you start); a ticket really in Backlog gets shaped and **stops there** for the human.
- **Gate 2 (plan approval):** after the cross-model plan critique, resolve every open decision yourself: prefer the option the critique recommends; if it makes no recommendation, pick the **most reversible** option. Record each self-made material decision as a `kind=decision` comment, reference its ID from `kind=plan-final`, then transition `Planning → Building` yourself.
- **Gate 3 (review + merge):** after the adversarial ai-review converges (same triage, second-pass, and churn-cap rules as always), post the **review-guide on the PR first** — it becomes the audit record — then verify `git rev-parse HEAD` == `git rev-parse origin/<branch>` (Hard rules), squash-merge the PR yourself, delete the branch, and transition `Building → PO Review`.
- **Gate 4 (PO acceptance):** self-validate **each COS against test evidence on the merged code**, post the validation note, transition to `Done`, and close with the full `kind=metrics` comment — the `overrides:` list (§ Memory & metrics) is mandatory on every autonomous run, `none` earned not assumed.

**Label mechanics:** at a replaced gate, don't set `needs:human` — post the stage's marker comment, then move straight to the next label/transition. `human-wait` in the metrics comment is `n/a (autonomous)`.

**Epics:** if the issue is an epic, decompose it per `references/decomposition.md`, then run each child story Ready → Done **one at a time, in dependency order** — full pipeline per story, no batching.

**Drive style:** no ask — continuous run. Report at the end (and at each story boundary when running an epic).

**⛔ STOP and report to the human instead of proceeding only if:**

- (a) the local gate or CI fails **twice on the same cause** with no clear fix;
- (b) implementation would touch **auth, money paths, data migrations, or CI/deploy config** in a way the ticket and approved plan did not anticipate;
- (c) a **blocking** review finding survives the churn cap unresolved;
- (d) a configured model is **unreachable** after the documented escalation ladder;
- (e) an action would be **destructive or hard to reverse** and the right choice is ambiguous.

Otherwise make the call yourself, record it (`kind=decision`, stage comments, `overrides:`), and keep going.

> **Custom workflow, deliberately richer than the org standard.** The agent stages (`Planning`, `Building`) are **first-class Jira statuses**, not just labels — the label axis alone isn't visible enough for human + AI co-work. The org's admin route creates the bare Jira project + Confluence space; **you** (project admin) build the workflow/board yourself. **PO Review** (gate 4) and transversal **BLOCKED** are kept from the standard because the business wants them. Details: `references/setup.md` §1.

## References (load on demand)

Read the relevant file **before** executing the step — don't improvise the how:

| File | Read when |
|---|---|
| `references/setup.md` | **First run in a project** — `.claude/sdlc.config.json` missing: bootstrap the Jira project/board/statuses + Confluence space, then write the config |
| `references/decomposition.md` | Intake is a PRD/spec or an epic bigger than one ticket — epic + child issues, sizing, dependency links, **context-mémo bake**, readiness verdict |
| `references/exploration.md` | Intake is a raw idea, not a spec — coached brainstorm / brief / PRFAQ → brief in the epic |
| `references/shaping.md` | Ticket in Backlog needs preparing for Gate 1 — the 8-item DoR (`readiness`, incl. the context-mémo bake), spikes, human decisions |
| `references/quality-practices.md` | Pre-mortem on plans (Planning), findings triage + review-guide hand-off (Building), correct-course when a human pushes back |
| `references/codex-runner.md` | **Any stage resolves to a bare `gpt-*` model** — companion script, prompt files, raw fallback, exit-0 traps, escalation ladder |
| `references/claudex-runner.md` | **Any stage resolves to `gpt-*@claudex`** — same model via the Claude Code harness + local proxy: env contract, read-only allowlist, review caveat, fallback to codex |
| `references/ab-stages.md` | **`plan` or `aiReview` is an array** — parallel arms on the same input: finding-tally scoring (aiReview), anonymized human selection (plan), logging |
| `references/local-tracker.md` | `config.tracker == "local"`, a demo, or no Jira available — vault-backed backend (one note per ticket in a Fast Note Sync vault + live board at `~/sources/sdlc-board`). Same labels/markers/gates; only storage changes |

## Before you start

- **First run in this project?** If `.claude/sdlc.config.json` is missing, read `references/setup.md`, bootstrap, write the config, then continue.
- **Local backend?** If `config.tracker` is `"local"`, the user asks for a demo, or Jira isn't ready — read `references/local-tracker.md` and drive the FNS vault instead of Atlassian. Same rules; only storage changes. **Writes go through `POST /ticket` on the board, always with `_rev` set to the status you are moving FROM.** The older git-backed board (`.sdlc/server.py`, state on `sdlc-state`) is gone; that branch is now a read-only archive.
- **Read the ticket's actual status before starting anywhere.** When the run is described as starting from a status ("it's in Ready", "run it Ready to Done"), that is the requester's belief, not an observation — **verify it against the board and re-shape if it doesn't hold**, rather than entering at the named step. A ticket that is really still in Backlog has no `readiness` and no context-mémo, and starting at Ready silently skips the shaping those represent. The skip is invisible and self-confirming: the run proceeds normally and produces a plan, just one built on whatever the ticket text asserted. TKT-233 was handed over as "already in Ready" while sitting in Backlog with `readiness: null` and its own comment reading *"Needs shaping before Gate 1"* — and the shaping it would have skipped is what caught the ticket's proposed fix being unimplementable (`shaping.md` § approach). This check costs one read and protects every stage after it.
- **Resuming?** If the ticket is in flight (or the user says "resume <KEY>"), reconstruct state from the board status + label + the marker comments — never from conversation memory — and continue from the step the label indicates.
- **Drive style:** in `gates: "human"`, V0 is interactive — ask **once at the start of the run**: *every step* (stop and report after each transition) or *gates only* (pause at the human gates); apply for the rest of the ticket, don't re-ask. In `gates: "autonomous"`, don't ask — continuous run per § Autonomous mode.
- **Risk:** agree on `risk:low` (skips the plan gate) for trivial, isolated work.

## Config (per project) — this skill is generic

Project-specific bindings live in **`.claude/sdlc.config.json`** in the code repo, not in this skill. Read it at the start of every run; anything below shaped like `config.jira.projectKey` comes from it.

```json
{
  "tracker":    "jira | local (see references/local-tracker.md; jira keys below apply only to jira)",
  "gates":      "human | autonomous (default human when absent — see § Gates)",
  "jira":       { "projectKey": "<KEY>",
                  "statuses": { "backlog": "Backlog", "ready": "Ready", "planning": "Planning", "building": "Building", "poReview": "PO Review", "done": "Done", "blocked": "BLOCKED" },
                  "transitions": { "ready->planning": "<resolved at setup>", "building->poReview": "<...>" },
                  "blocksLinkType": "Blocks" },
  "confluence": { "spaceKey": "<SPACE>" },
  "code":       { "repo": "<owner/repo>", "defaultBranch": "main",
                  "localGate": "<project's local gate cmd, mirrors CI>",
                  "gateVerdict": "<optional: path the gate writes its own verdict to; omit if it doesn't>" },
  "models":     { "plan": "main-agent", "planReview": "gpt-5.6-sol",
                  "implement": "main-agent", "aiReview": "gpt-5.6-sol" },
  "registry":   { "bindingPath": "docs/decisions/", "referenceLocation": "confluence" }
}
```

Resolve transition names (`getTransitionsForJiraIssue`) and the blocks link type (`getIssueLinkTypes`) at runtime — both vary by instance — and store them in `config.jira.transitions` (keyed `<from>-><to>`) and `config.jira.blocksLinkType` so later runs don't re-discover them.

### `config.models` — who does each stage

Four stages are model-assignable. **The config declares it; the agent never infers it.** Read the value, resolve it via the table, run it. Never detect which model is driving the session and branch on it — a different planner is a config edit, not a runtime decision. This routing is **the single source**: the transition steps below say *what* each stage does; *who runs it and how* is always this table.

| Value | How to run that stage |
|---|---|
| `main-agent` | **You** do it — whichever model drives this Claude Code session. Never hardcode which one that is. |
| `gpt-5.6-sol` (any `gpt-*`) | Codex, via the plugin's companion script — **read `references/codex-runner.md` first.** Reviews → `adversarial-review`; free-form work (plan drafting, implementation) → `task`. |
| `gpt-5.6-sol@claudex` (any `gpt-*@claudex`) | Same model, different harness: headless Claude Code through a local proxy — **read `references/claudex-runner.md` first.** Read-only stages only; requires the env contract that doc defines, else stop and report. |
| `gemini-3.8-flash-high` (any `gemini-*`) | The Antigravity CLI: `agy --model <value> --add-dir <repo path> -p '<brief>'`. Write stages also need `--dangerously-skip-permissions`. **Without `--add-dir` it works in its own scratch directory and never touches the repo** — the shell's cwd is not its workspace. Reasoning effort is part of the model id (`-low`/`-medium`/`-high`), so an `:effort` suffix is a config error. |
| `fable-5`, `opus-4.8`, `sonnet-5` (any Claude model) | A subagent: the `Agent` tool with `model:` set to it. Brief it from the ticket + the approved plan — it does **not** inherit this session's context. |
| **an array of values** (`plan` and `aiReview` only) | An A/B experiment: run every arm on the same input, score, record — **read `references/ab-stages.md` first.** Arrays on `planReview`/`implement` are a config error: stop and report. |

Any `gpt-*` value (single or array arm) may carry an **`:effort` suffix** — `gpt-5.6-sol:low`,
`gpt-5.6-sol:low@claudex` (order: `model[:effort][@harness]`) — passed to the runner's `--effort`
flag; omitted = the runner's default. **Exception:** the codex `adversarial-review` command has no
effort flag, so an effort suffix on a bare `gpt-*` `aiReview` value is a config error — effort-varied
review arms require `@claudex`.

**You are always the orchestrator.** Whatever `config.models` says, *you* read the ticket, brief the worker, **talk to the human**, revise, post every comment, and own the outcome. These keys assign **who does the thinking for a stage** — never who drives the board or holds the conversation. A delegated worker **cannot ask the human anything**: one brief in, one answer out. Any step needing a human decision (grilling on open decisions, a gate hand-off) is **yours**, in this session.

**⛔ The reviewer must never be the author.** Cross-model review is the pipeline's load-bearing invariant, constraining **two pairs**: `plan` ≠ `planReview` and `implement` ≠ `aiReview`. Check both when you read the config; if either matches, **stop and report** — don't run a degraded flow. A model reviewing its own output re-confirms rather than falsifies. Note `main-agent` and a named Claude model can *collide*: if the session model is the named one, that's the same violation wearing two labels. When unsure, ask.

Swapping `plan` ↔ `planReview` is how the "Codex drafts, main agent reviews" variant is expressed — a config flip, not a fork of this skill. **Cross-model review is a prerequisite, not an option:** if an environment forbids sending code to an external model, this SDLC doesn't apply there — don't build a degraded variant.

## Labels (the state machine — Jira labels)

| Label | Meaning |
|---|---|
| `agent:planning` | Drafting the execution plan against the real code |
| `agent:plan-review` | Cross-model critique of the plan; agent revising |
| `agent:coding` | TDD in progress (tests first → implement → local gate green) |
| `agent:ai-review` | PR open; adversarial review; agent fixing findings |
| `needs:human` | **Agent has stopped — ball is in the human's court.** Status disambiguates: in `Planning` = plan approval; in `Building` = PR review + merge; in `PO Review` = PO validation |
| `risk:low` | Orthogonal modifier set at creation: skip the plan gate. **Challengeable, not gospel** — see invariants |

**Invariants:**
- Exactly **one** pipeline label (`agent:*` or `needs:human`) on an in-flight ticket (`Planning`/`Building`). None in `Backlog`, `Ready`, `Done`.
- Once `needs:human` is set, the agent **stops pushing commits**. Never make the human review a moving target. On resume (changes requested, merge conflict), swap back to the relevant `agent:*` label first.
- **`risk:low` is challengeable.** The claim comment must restate *why* the risk is low. If the work turns out to touch auth, data migrations, CI/deploy config, or >5 files, drop the label (say so in a comment) — the plan gate comes back. A mislabeled ticket must never bypass human judgment silently.
- **`risk:low` is about BLAST RADIUS, not difficulty — and the two come apart.** The carve-outs above all ask *"what breaks if this is wrong?"*, so a ticket can pass every one of them and still have a hard, **wrong-by-default** design. Keep the plan gate when the ticket's central question is a design question with a non-obvious answer — above all **"how do I construct a test that can actually fail?"**, which is the shape that recurs here. TKT-266 was tests-only, two files, zero production code, and correctly `risk:low` by blast radius; it then spent three build cycles on fixture design, because both obvious fixtures were unfalsifiable for structural reasons (an FK-to-PK join yields *zero rows*, not a wrong one; a multi-valued mutation returns the right row *beside* the wrong one). Each was rejected only by running a mutation. A plan critique costs one delegated read and catches that class cheaply — which is exactly what the plan gate is for, and skipping it bought nothing.
- **Never claim a ticket with open blockers**, even if a human moved it to `Ready` — the status is priority, not feasibility. Surface the conflict instead (`references/decomposition.md` covers links).
- **The Jira comment thread is the memory.** Every `agent:*` stage ends with a marker-tagged comment (`<!-- sdlc:stage=… kind=… -->` on line 1) *before* the label moves; every stage and every resume starts by reading the board + thread, not conversation memory. Comments without a marker are human steering — incorporate and acknowledge them.
- Per-stage durations come from the **Jira issue changelog** (status + label history), never hand-tracked.

## Decision log artifacts

Shaping records the product and scope choices that make a ticket ready. Planning and Building each
produce a two-part review bundle. The planning bundle is the draft plus its final amendments,
paired with the planning decision log. The development bundle is the diff, tests and verification
evidence, paired with the development decision log. The ticket thread owns all three logs through
append-only `kind=decision` comments, so the board can show them without another state store.

Log a decision when the governing artifacts did not settle it and the choice:

- changes observable behaviour, security, data, money, concurrency, failure handling or the test seam;
- selects between meaningful alternatives;
- changes the approved plan or leaves follow-up work;
- would help a reviewer explain why the result has this shape.

Do not log routine mechanics, facts already dictated by the spec, or private reasoning. Record the
decision when it is made, before more work makes the choice expensive to reverse. Never edit an old
entry. When a later fact changes one, put `supersedes D<n>` in the new Decision line. IDs are
sequential within the ticket: `D1`, `D2`, and so on across shaping, planning and development.

Use this exact body shape so humans and tools can scan the log consistently:

```markdown
<!-- sdlc:stage=<backlog|planning|plan-review|coding|ai-review> kind=decision -->
Decision: D<n>. <the choice in one sentence; add `Supersedes D<x>.` here when needed>

Why: <the evidence and the rejected option in one or two sentences>

Consequences: <new constraint, risk or follow-up, or `none`>

Artifacts: <plan section, files, tests, ADR or PR location>
```

`Artifacts` may name planned locations during Planning and must name actual locations during
Building. The shaping `kind=readiness` comment and planning `kind=plan-final` comment each say
`Decision log: <IDs>` or `Decision log: none` for their phase. Building puts that line in the
`agent:ai-review` `kind=summary` comment, not the earlier coding summary or later PO summary. The
list is a completeness check, not a duplicate rationale.

## The 6 statuses

| # | Status | Entry means | Agent does (label sequence) | Exit gate |
|---|--------|-------------|------------------------------|-----------|
| 1 | `Backlog` | issue created | Write **COS** (Conditions of Success — this pipeline's term for acceptance criteria) + scope; suggest `risk:low` if trivial. **Every pipeline ticket gets the context-mémo bake** (`decomposition.md` § context-mémo) — standalone tickets too. **Raw idea →** explore first (`exploration.md`). **PRD/multi-ticket →** decompose (`decomposition.md`). **Then shape** (`shaping.md`): fill the 8-item DoR (`readiness` field — `context_memo` is one of them, so the bake is gate-enforced), spawn spikes, flag human decisions (`owner: "human"`), and record each settled material choice as a `kind=decision` entry. | ⛔ human → `Ready` — **hard-blocked while any DoR item is `open` or a blocker is open** (`deferred` passes) |
| 2 | `Ready` | approved queue | Verify **zero open blockers**, then claim: assign self, create branch `<ISSUE-KEY>-<slug>` off `origin/main`, post a claim comment with selection reason. | agent claims → `Planning` |
| 3 | `Planning` | agent claimed | `agent:planning`: **`config.models.plan`** reads the real code + the context-mémo, picks the **test seam**, drafts the plan and records material `kind=decision` entries → `agent:plan-review`: **`config.models.planReview`** critiques the plan and planning decision log, pre-mortem pass → `needs:human` (skip if `risk:low`). | ⛔ human reviews the plan bundle → `Building` |
| 4 | `Building` | plan approved | `agent:coding`: **`config.models.implement`** does TDD from the approved plan and records new or changed material decisions, local gate green, push, open PR (`<ISSUE-KEY>` in title/body) → `agent:ai-review`: **`config.models.aiReview`** first reviews the diff blind, then audits it with both decision logs, triage, fix, rebase if behind, re-green, second pass if fixes were non-trivial → `needs:human`: post the review-guide on the PR, stop. | ⛔ human reviews the development bundle + **merges** → `PO Review` |
| 5 | `PO Review` | human merged (deployed to DEV) | `needs:human`: post a validation note showing the **COS are met** from the user's output (via code if not user-demonstrable), + any preview link; stop pushing. | ⛔ PO validates COS + gives final go → `Done` |
| 6 | `Done` | PO accepted | Remove `needs:human`; verify PR merged + the **project DoD** (see `references/setup.md`); post the metrics comment; promote reusable learnings; delete branch. | — |

**`BLOCKED`** is transversal, not a row: a human moves any active ticket there when it hits an open blocker, and back to `Ready`/`Planning`/`Building` by context. The agent keeps its prior label and stops until it returns.

## Transitions (V0 — manual, via MCP + code repo)

Each step is an agent action on the user's command. **Jira ops = Atlassian MCP tools; code ops = `git`/`gh`/`codex`.** Model-assignable stages route per § config.models — the steps below don't repeat the how. The ⛔ GATE lines below (and in the references) are the **human-mode** behavior; in `gates: "autonomous"`, apply the corresponding replacement from § Autonomous mode instead — gates 2–4 only, Gate 1 always waits.

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
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=ready kind=claim --> + selection reason
#     + an EXPLICIT risk verdict: "risk:low claimed / not claimed, because …". State it even when
#     the answer is obvious — silence is what lets a mislabeled ticket through, and the label's
#     carve-outs (auth, data migrations, CI/deploy config, >5 files) are exactly the tickets whose
#     risk is obvious to the claimer and invisible in the thread afterwards (TKT-190).
#     + REQUIRED: "premises verified at HEAD" — walk the `readiness` object's load-bearing facts
#     and give each a verdict. Shaping is a SNAPSHOT; the ticket sat in Ready while siblings
#     merged, and a stale premise is invisible because the run proceeds normally and produces a
#     plan, just one built on something no longer true. The existing rule to re-resolve
#     `governingAdrs` at claim time is this rule for one field; the falsified claims land in all of
#     them. Three consecutive tickets in one 2026-09-01 run: TKT-145's central COS ("no test pins
#     this") was satisfied by TKT-280 three days after shaping; TKT-165 was told to write ADR-069
#     (taken by TKT-301 two days after shaping) and to name an adversary from a code comment whose
#     surrounding sentence said the opposite; TKT-234's COS-3 asked for a mutation the signed
#     canonical form makes impossible. Each cost minutes to check and would have cost a wrong
#     deliverable. Cite file:line or a command for each verdict — "verified" without evidence is
#     the claim being checked, restated.
#   agent transitions Ready → Planning (claim is the agent's move).

# 3 Plan (Jira)
#   MCP editJiraIssue add-label agent:planning
#   3a DRAFT — by config.models.plan. Read the ticket + agent-context property YOURSELF first —
#      you brief the drafter; don't make it guess the mémo. Whoever drafts: read the real code,
#      pick the test seam, produce DoD / files / test plan / risks. Delegated drafts are
#      READ-ONLY — the drafter never touches the tree. Brief file rules: codex-runner.md.
#      REQUIRED brief SECTION — "governing ADRs": the decisions constraining the touched area and
#      what each constrains *for this ticket* (seed from the mémo's governingAdrs, then re-resolve
#      against registry.bindingPath at claim time — the mémo's list is a shaping-time snapshot and
#      goes stale when ADRs land between shaping and claim (TKT-73, TKT-79) — do not skip). The
#      drafter reads code, not decision
#      history: an ADR that makes the obvious solution wrong is invisible to it (TKT-62).
#      REQUIRED brief SECTION — "governing tests": the cross-cutting invariants enforced by a TEST
#      rather than an ADR, from the mémo's governingTests (decomposition.md § context-mémo). An ADR
#      is discoverable by reading; these are discoverable by failing the gate. TKT-235's plan was
#      correct against all ten of its governing ADRs and still failed the gate twice — on catalog's
#      safe-must-be-public contract rule and on the cachetier tier audit's closed allowlist.
#      Post the draft attributed to the model that wrote it.
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=planning kind=plan -->
#   As material choices are made, append one <!-- sdlc:stage=planning kind=decision --> comment
#   per choice using § Decision log artifacts. Do not reconstruct the log at the end.
#   MCP editJiraIssue: -agent:planning +agent:plan-review
#   3b CRITIQUE — by config.models.planReview (≠ plan, enforced). Adversarial, not a rubber stamp.
#      Verify every file/symbol/test seam the draft names actually EXISTS (`git grep -n` at HEAD) —
#      a drafter that couldn't run the gate will hallucinate seams. Check against the real code,
#      the COS, the registry; pre-mortem lens (quality-practices.md §1). Review the plan first
#      without its rationale, then audit the plan together with every planning decision entry.
#      Accept / amend / reject each part of the draft with a stated reason.
#      After reviewing the delegate's output, YOU append a stage=plan-review kind=decision entry
#      for any material choice you introduce while amending the plan.
#   3c FINALIZE — YOU, always (a delegated worker cannot talk to the human): grill the human on
#      the open decisions (complex tickets, one question at a time), revise, post kind=plan-final
#      as a DELTA, not a restatement: amendments + decisions + the required sections, with "the
#      kind=plan draft as amended above is the plan". Restating the full plan doubles the thread
#      for zero audit value (TKT-84 retro); resume already reads draft + delta together.
#      REQUIRED SECTION — "introduced at plan-review, reviewed by no second model": every design
#      element 3b *added* rather than accepted/amended/rejected from the draft. Those reach Gate 2
#      reviewed by nobody but their author — the cross-model guarantee covers the draft and the
#      code, not 3b's own inventions (TKT-57). Name them again in the Gate 2 hand-off. If the
#      list is empty, write `none`.
#      REQUIRED SECTION — `Decision log: <IDs>` or `Decision log: none`. Confirm the listed
#      entries cover every material choice introduced while drafting or revising the plan.
#   If risk:low → skip to step 4.
#   MCP editJiraIssue: -agent:plan-review +needs:human
#   ⛔ GATE 2 — wait for the human to transition Planning → Building.

# 4 Réalisation (code repo + Jira)
#   MCP editJiraIssue: -needs:human +agent:coding
#   TDD, by config.models.implement. The approved kind=plan-final IS the brief — if it isn't
#   enough to hand over, that's a plan bug; fix the plan, don't paper over it in-session.
#   Append <!-- sdlc:stage=coding kind=decision --> when implementation exposes a material choice
#   the approved plan did not settle, or when evidence forces a plan decision to change. Link the
#   affected code/test in Artifacts; use Supersedes when replacing an earlier decision.
#   One implementer, no worktree — WIP is one ticket, and a TDD loop doesn't parallelize.
#   Local gate (mirrors CI): run config.code.localGate. If you delegated, VERIFY don't trust —
#   re-run the gate yourself on the committed tree (quality-practices.md §2.b).
#   **`config.code.gateVerdict` set?** The project's gate writes its own verdict and that FILE is
#   the authority. Run `localGate` as the SOLE command in its shell call, let the process finish,
#   then read the configured path: first token `PASS` or `FAIL`, everything after it is
#   provenance (the revision and tree it tested — check it describes the tree you mean). Missing,
#   malformed, or not newer than the moment you launched the run = FAIL. Believe it over the exit
#   code, over the harness's completion status, and over the log. Build no sentinel, grep no log
#   body, poll for no process — the project already solved this and a second protocol beside it
#   is a second thing that can disagree.
#   **Not set?** Judge it yourself, and every trap below is live. Run the gate as the SOLE command
#   in its shell call: no trailing chains AND no prefix chains (`verify && gate > log`
#   short-circuits on the verify and reports the verify's failure as the gate's — TKT-87), and
#   re-check cwd (a mis-cwd'd `make check` exits 2 on "No rule to make target" — TKT-71's shape).
#   Never chain commit/push onto the gate's shell call: chained, a gate that failed to even start
#   ships the push anyway (TKT-71). Don't pipe it (`gate | tail; echo $?` reads tail's exit, not
#   the gate's — a failed gate reported exit=0 on TKT-94): redirect to a file and read the file.
#   For a BACKGROUND run judge PASS/FAIL from the log body (`grep -E 'Error [0-9]+|FAIL|drifted'`),
#   not the harness-reported exit code — they diverged on TKT-101 (three runs reported exit 0 that
#   the log showed failing) and again on TKT-235 ("exit code 0" while the log said
#   `make: *** [lint-go] Error 1`). Judge DONE by an explicit exit-code sentinel
#   (`gate > log 2>&1; echo EXIT=$? > done`; wait with `until [ -f done ]; do sleep N; done`),
#   NEVER by `pgrep -f "<gate cmd>"` — `-f` matches the watcher's own command line, so the poll
#   self-matches and never returns (TKT-106;
#   docs/learnings/2026-07-21-pgrep-watchers-self-match.md). Believe no single one alone.
#   Code: verify `git branch --show-current` is the ticket branch FIRST (a session's branch can
#   be switched under it — TKT-84 briefly committed to local main), then commit (no AI
#   attribution), push; gh pr create --base main --title "<ISSUE-KEY> …" --body "…<ISSUE-KEY>…"
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=coding kind=summary --> (+ stage YAML; name the implementer)
#   MCP editJiraIssue: -agent:coding +agent:ai-review
#   REVIEW — by config.models.aiReview. First review the branch diff vs base blind, primed for
#   guilt. Then run the decision audit with the approved plan plus both decision logs (Hard rules).
#   Triage findings (quality-practices.md §2): blocking→fix in PR; incidental→new backlog ticket;
#   rejected→stated reason. Rebase on origin/main if behind; re-green.
#   After reviewing the delegate's output, YOU append a stage=ai-review kind=decision entry when
#   a finding fix introduces or changes a material choice. A fix is implementation too; do not
#   hide its decisions in the summary.
#   SECOND PASS iff the fixes were non-trivial (Hard rules). Trivial → one pass, say so in the
#   stage comment. No third on a stable diff — still churning → stop, hand to the human.
#   MCP comment kind=summary (ai-review): per-finding verdicts, what was fixed, decision-audit
#   verdicts, `Decision log: <development IDs>` or `Decision log: none`, second-pass call + why,
#   and `decision-audit: <runner/harness duration>; yield: <N> proposed, <B> blocking,
#   <I> incidental, <R> rejected; <short names or none>`. Record the duration reported by the
#   runner or host harness when collecting the audit result; this is a timed sub-operation, not a
#   hand-tracked stage duration. `N` equals `B + I + R` and counts only findings absent from the
#   blind pass.
#   MCP editJiraIssue: -agent:ai-review +needs:human ; post the review-guide on the PR. STOP pushing.
#   ⛔ GATE 3 — human reviews + merges (squash) in the code repo, then transitions Building → PO Review.
#     Do NOT merge yourself. Merge conflict? swap back to agent:coding, rebase, re-green,
#     back to needs:human (re-review the new SHA).

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

- **Thread = memory.** Markers on line 1: `<!-- sdlc:stage=<backlog|ready|planning|plan-review|coding|ai-review|po-review|done> kind=<claim|plan|plan-final|decision|summary|blocker|metrics|readiness> -->`. The `review-guide` lives on the **PR**, not Jira. Resume = read the board status + label + the latest of each marker kind + every `kind=decision` comment + any human comment after the last agent comment.
- **Cross-ticket memory.** Reusable learnings (codebase patterns, gotchas) go to a dedicated **"🧠 Agent memory" Jira issue** (or the team decision registry). When a pattern proves out, promote it per the **"3 houses" rule** (`references/setup.md`): **technical** standards → the team's shared standards registry (via PR), **process** learnings → the team wiki, repo-local guidance → `AGENTS.md` via a normal gated ticket. Working memory ≠ gospel.
- **Promotion is a mandatory Done step, not best-effort.** The `kind=metrics` closing comment must include a `learnings:` section — either concrete promotable items (each with its target house) or the explicit word `none`. Skipping it is the registry cold-start failure mode.
- **Metrics.** Durations from the Jira changelog (status/label transitions); `needs:human` time-in-state is the Gate 2 / Gate 3 human wait — a first-class metric, the likely bottleneck. Diff size from `gh pr view`. The `kind=metrics` comment has **required fields** — a table `stage | duration` (one row per status + per `agent:*` label), `human-wait total`, `diff: +x/−y (n files)`, the `learnings:` section, and a `retro:` section. All five, every ticket; missing data is written `n/a`, not omitted.
- **Gates waived? The closeout names every objection the agent overrode.** On any run where a human gate is replaced by agent judgement — every `gates: "autonomous"` run, or an ad-hoc waiver — add an `overrides:` list to `kind=metrics`: each point where a review pass (or the human, earlier) stated a **blocking** objection and the agent proceeded anyway — finding, reviewer's reason, agent's reason, and the ticket carrying the residual risk. `none` is a valid answer and must be earned. Rationale: on a gateless run the machinery catches the ordinary defects; what it cannot settle is a genuine disagreement about whether something should ship. That list is the smallest thing an owner must read to re-take the decisions that were taken for them — and it is unrecoverable later, because a fixed finding and an overridden one look identical in a merged diff (TKT-190 overrode one [high] "do not ship").
- **`retro:` = introspection on the collaboration, not the code.** Copy the `decision-audit:` duration and yield line from the `agent:ai-review` summary; do not reconstruct it at Gate 4. At Gate 4, read the metrics comments on the newest completed, non-spike tickets. If the current ticket and the previous nine all record an audit with `blocking: 0`, add a retro proposal to remove the informed pass. A missing measurement or non-zero blocking count breaks the streak. The proposal does not change the workflow until a human accepts it. Then answer two fixed questions honestly (`nothing` is allowed but must be earned): **(1)** *What would have made this ticket faster or better — missing context in the mémo, ambiguous COS, a gap in this skill?* **(2)** *What should the humans change — in the process, the config, or how the ticket was written?* Each retro item is a **concrete, appliable change** (which file/section, what edit) — not an observation. V0 is interactive: after posting the retro, offer to apply the changes now; if approved, patch in the same session and note `applied`. Unapplied items land in the weekly registry review; a recurring one is a signal to stop re-noting and patch.
- **High bar for skill edits.** This skill carries *rules*, not knowledge. A retro item may patch the skill only if it would change agent behavior in a future session **and** isn't already readable from the repo when needed — learnings (`docs/LEARNINGS.md`, `docs/learnings/`) and ADRs (`docs/adr/`) are accessible to every session; **cite them, never restate them**. Incident narratives go to `docs/learnings/`; the skill gets the rule, a one-line why, and the ticket citation. When in doubt, it's a learning, not a skill edit.
  **A learning that asserts a fact about a tool or a database records HOW it was established.** Those files are cited as authority and nothing gates them, so a plausible-but-wrong sentence in one is read as settled and propagates: TKT-234 found that `2026-08-09-a-total-order-is-not-a-meaningful-one.md` had claimed for three weeks that an unconditional `BEFORE INSERT` trigger governs logical-replication apply — it does not, and the same false claim had by then been copied into an ADR, a migration comment and a test comment. One `psql` session settled it. Write the command and its output beside the claim, or write that you have not run it. Corollary when correcting one: **grep for the CLAIM, not the symbol**, across `docs/` and comments — the copies do not share a spelling, and three consecutive review passes each found one of them.

## Hard rules

- **Never merge at a gate the config gives to the human.** In `gates: "human"`, Gate 3 is the human's, in the code repo — agents stop at `needs:human`. In `gates: "autonomous"`, the agent merges per § Autonomous mode, review-guide posted first and the pre-merge push check done.
- **Model routing is declared, never inferred.** Every model decision comes from `config.models` (§ Config). Never detect which model is driving the session and branch on it, and never silently substitute a model: if the configured one is unreachable, **stop and report**. Who did the work is a thing this pipeline measures — a stage that quietly ran on the wrong model is a corrupted measurement. **This rule is deliberately self-contained:** the skill ships to whoever installs it and must not depend on any individual's personal model preferences or global config.
- **Delegated ≠ done.** When a stage resolves to anything but `main-agent`, you still own the outcome: verify the output against the real code, re-run the gate yourself (`quality-practices.md` §2.b), and attribute the work in the stage comment. A delegated run reports a *claim*; the evidence is what you check.
- **`gpt-*` stage mechanics live in `references/codex-runner.md`** — companion script (not raw `codex exec`, not slash commands), prompt-file materialization, the raw fallback, judging output (exit 0 ≠ success), and the fixed escalation ladder. Read it before running any such stage. A `@claudex` suffix on the model swaps the harness, not the model — those mechanics live in `references/claudex-runner.md` instead.
- **Verify reviewer findings against the revision under review — not the base branch.** The review pass is a *lead generator*, not an oracle. Check each finding with `git grep -n "<sym>" HEAD` (or the working tree); use `origin/main` **only** to ask whether something pre-existed the change — grepping the base for a symbol the PR *introduces* looks like a refuted finding. Make sure your checkout is current first (a stale local `main` is what makes this go wrong). Record a per-finding verdict in the stage comment.
- **Review through two lenses, in order.** First prime the reviewer for guilt and give it the diff only. Tell it to **assume the diff is wrong and find where**, not to "check whether it's ok". This blind pass keeps the author's rationale from persuading the reviewer before it inspects reality. After the blind findings are fixed or triaged, give the reviewer the approved plan and both decision logs for a **decision audit**. The audit checks that each choice is supported by the code and evidence, that the implementation follows the approved plan, and that no material choice is missing from the log. Never replace the blind pass with the informed pass. A good explanation can still defend bad code.
- **A second `ai-review` pass when the fixes were non-trivial — otherwise one.** The second pass exists because **the fix diff is code no one has reviewed** — judge the *fix diff*, not the original PR diff. **Non-trivial = any of:** a blocking finding was fixed (correctness/security/AC — fixing one changes logic by construction, so a real review round almost always earns a second pass); aggregate volume, even when each fix is small; new/changed control flow, signature/API/schema/migration, a money/auth/concurrency path, a new or rewritten test, or fixes spilling into files the findings didn't name. A rebase that materially changed the diff also triggers. Pure trivia (typos, comments, formatting, a compiler-verified rename) → one pass. Record the call in the stage comment either way.
  **Cap: two passes per *stable* diff — but a pass that invalidates the previous pass's fix resets the counter (absolute cap 4).** The cap stops **churn** (re-litigated style, resurfaced already-triaged findings, findings shrinking each round → stop, hand to the human), not **convergence** (a pass proved the previous fix wrong at the root and a structural rewrite followed — that rewrite is the branch's least-reviewed, riskiest commit, so run one more; TKT-61). **The absolute cap of 4 may be exceeded only when the run is demonstrably converging**, which means BOTH: every pass so far invalidated the previous pass's fix at the root (never re-litigated settled ground), AND the finding count is **strictly decreasing** each round. Name each extra pass and this justification in `overrides:`. Absent either condition the cap holds — a run that keeps finding *new* defects in *unchanged* code is not converging, it is a diff nobody understands yet, and that belongs with a human. TKT-244 is the boundary case: four passes, findings 3→2→1→1, each refuting the prior fix, each fix structurally smaller than the last (trim → hidden state → server merge → **remove the capability**) — and the fourth still found a real [high]. It stopped at the cap by rule, with the residual honestly recorded as *unquantified*. If you do stop at the cap with an unreviewed fix diff, **say so explicitly in the stage comment *and* the review-guide**, and label your own verification as your own.
  **Carve-out for a diff touching no production or test code** (an ADR, docs, comments): the cap still stops the *review*, but the agent **may merge at the cap** provided every finding is fixed, the gate is green, and the unreviewed fix diff is named in `overrides:` and the review-guide. The rule's rationale is that *a diff nobody understands yet* belongs with a human; a prose diff's failure mode is a misled reader, corrigible by editing prose with no migration, deploy or rollback. And on such tickets the count measures the wrong thing: TKT-234 ran 1→2→3→2 with the *mechanism* settled after pass 2 — what kept the count up was four copies of one fact (an ADR, a test comment, a migration comment, a `docs/learnings/` entry) disagreeing with each other, which is a consistency problem, not an understanding problem. TKT-145 was the same shape at 2→5→3→2. **This does not touch the convergence exception above**, which remains the only route past the absolute cap for a diff that changes code.
  **A Gate-3 human review round is not a codex trigger:** the human review *is* the authoritative adversarial pass — address its changes under `agent:coding`, re-green, return to `needs:human` for re-review of the new SHA.
- **A test written while fixing a review finding gets the same mutation check as a test written from the plan** — break what it covers, observe red, restore — and the stage comment records the result. This is where the discipline actually fails: it holds for planned tests and lapses under fix-momentum, when a test is a means to closing a finding rather than the point of the work. Four assertions across TKT-22 could not fail, and every one was written mid-fix; a reviewer found all four. Also run the TS/typecheck build before invoking the full gate — `vitest` does not typecheck, and a type error found in twenty seconds is cheaper than one found three minutes into a gate run.
- **V0 is manual** — no Jira automation / webhooks. Movement is agent-driven on the user's command.
- **Never push after setting `needs:human`** without first swapping back to an `agent:*` label.
- **Before any merge, verify your last commit actually reached the PR head.** A commit made locally after your last `git push` is invisible to the PR — the reviewer approved, and the merge captures, a SHA that predates it. Whenever you merge (Gate 3, or a waived/gateless run where the agent merges), first assert `git rev-parse HEAD` == `git rev-parse origin/<branch>`; if they differ, push, then re-merge. This is the write-side of the stacked-PR/squash trap: the badge and the reviewed artifact can both be stale relative to your working tree. TKT-97 squash-merged a fix that was committed but never pushed — the pre-fix version merged, caught only by verifying `origin/main` *content* after the merge; recovery meant recovering the orphaned commit and cherry-picking it onto the default branch.
- **When a human pushes back** — diagnose the failing layer first (**intent / plan / implementation**) and regenerate from that layer, never patch locally at a lower one (`quality-practices.md` §4).
- **TDD in `Building`** — tests first; local gate = `config.code.localGate` (mirrors CI). Required CI checks are the project's configured required checks on `config.code.defaultBranch`.
- Branch protection on `config.code.defaultBranch` — **for ticketed product work**: PR required, required checks must pass, no direct push. **Internal maintenance is exempt** (see § What this pipeline is for). Verify what the host actually enforces rather than trusting this line — on GitHub, `gh api repos/<owner>/<repo>/branches/<branch>/protection`; a 403 means the feature isn't on the repo's plan and *nothing* is enforced.

## Future automation (design intent, not implemented)

When V0 has proven out: Jira automation rules / webhooks become the trigger surface; a human gate becomes a status transition (or an `/approve` comment a rule converts). The merge gate stays human, in the code repo. Nothing in this skill should depend on automation — the manual path is the source of truth.

## Notes

- Atlassian MCP tools: `createJiraIssue`, `editJiraIssue`, `transitionJiraIssue`, `getTransitionsForJiraIssue`, `getJiraIssue`, `searchJiraIssuesUsingJql`, `addCommentToJiraIssue`, `createIssueLink`, `getIssueLinkTypes`, `createConfluencePage`. **Entity-property writes** (the context-mémo `agent-context` key) are **not exposed by the MCP** — write via the Jira REST API (`PUT /rest/api/3/issue/{key}/properties/agent-context`); read back via `getJiraIssue` (properties). **Fallback when REST property writes are unavailable**: store the same payload as an HTML-comment block at the *end* of the issue description — `<!-- sdlc:context\n{…json…}\n-->` — via `editJiraIssue`. Readers check the entity property first, then the description block; whichever exists is the mémo.
- This supersedes the GitHub-based 9-stage and 5-column versions. The GitHub variant lives on the `sdlc-pipeline-5-columns` worktree branch for reference.
- Lighter than the repo's metaswarm workflow — don't auto-invoke metaswarm gates here unless asked.
