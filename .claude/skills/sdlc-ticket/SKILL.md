---
name: sdlc-ticket
description: Drive one unit of work through the agentic SDLC pipeline on JIRA — a 6-status board (Backlog→Ready→Planning→Building→PO Review→Done, + transversal BLOCKED) with label-driven sub-states, plus the code repo for branch/PR/merge. Human gates - priority at entry, plan approval (skipped for risk:low), review+merge, PO acceptance at exit. Use when the user wants to run a task/ticket through the pipeline, says "run this through the SDLC pipeline", "take this ticket through the flow", "resume <ISSUE-KEY>", wants a PRD/spec decomposed into pipeline tickets, wants to explore a feature idea into tickets, or references the "SDLC agentique" board. V0 is manual - the agent performs each stage on the user's command in Claude Code via the Atlassian MCP; there is no event automation.
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

## Human gates

1. ⛔ **Priority** — human transitions `Backlog → Ready`.
2. ⛔ **Plan** — human transitions `Planning → Building`. **Skipped when the issue carries `risk:low`.**
3. ⛔ **Review + merge** — human reviews the PR and **merges (squash) in the code repo**, then transitions `Building → PO Review`. Agents never merge.
4. ⛔ **PO acceptance** — the PO functionally validates and transitions `PO Review → Done`.

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
| `references/local-tracker.md` | `config.tracker == "local"`, a demo, or no Jira available — repo-contained backend (JSON per ticket on `sdlc-state` branch + live board). Same labels/markers/gates; only storage changes |

## Before you start

- **First run in this project?** If `.claude/sdlc.config.json` is missing, read `references/setup.md`, bootstrap, write the config, then continue.
- **Local backend?** If `config.tracker` is `"local"`, the user asks for a demo, or Jira isn't ready — read `references/local-tracker.md` and drive the repo-contained store instead of Atlassian. Same rules; only storage changes.
- **Read the ticket's actual status before starting anywhere.** When the run is described as starting from a status ("it's in Ready", "run it Ready to Done"), that is the requester's belief, not an observation — **verify it against the board and re-shape if it doesn't hold**, rather than entering at the named step. A ticket that is really still in Backlog has no `readiness` and no context-mémo, and starting at Ready silently skips the shaping those represent. The skip is invisible and self-confirming: the run proceeds normally and produces a plan, just one built on whatever the ticket text asserted. TKT-233 was handed over as "already in Ready" while sitting in Backlog with `readiness: null` and its own comment reading *"Needs shaping before Gate 1"* — and the shaping it would have skipped is what caught the ticket's proposed fix being unimplementable (`shaping.md` § approach). This check costs one read and protects every stage after it.
- **Resuming?** If the ticket is in flight (or the user says "resume <KEY>"), reconstruct state from the board status + label + the marker comments — never from conversation memory — and continue from the step the label indicates.
- **Drive style:** V0 is interactive. Ask **once at the start of the run**: *every step* (stop and report after each transition) or *gates only* (pause at the human gates). Apply for the rest of the ticket; don't re-ask.
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

Resolve transition names (`getTransitionsForJiraIssue`) and the blocks link type (`getIssueLinkTypes`) at runtime — both vary by instance — and store them in `config.jira.transitions` (keyed `<from>-><to>`) and `config.jira.blocksLinkType` so later runs don't re-discover them.

### `config.models` — who does each stage

Four stages are model-assignable. **The config declares it; the agent never infers it.** Read the value, resolve it via the table, run it. Never detect which model is driving the session and branch on it — a different planner is a config edit, not a runtime decision. This routing is **the single source**: the transition steps below say *what* each stage does; *who runs it and how* is always this table.

| Value | How to run that stage |
|---|---|
| `main-agent` | **You** do it — whichever model drives this Claude Code session. Never hardcode which one that is. |
| `gpt-5.6-sol` (any `gpt-*`) | Codex, via the plugin's companion script — **read `references/codex-runner.md` first.** Reviews → `adversarial-review`; free-form work (plan drafting, implementation) → `task`. |
| `gpt-5.6-sol@claudex` (any `gpt-*@claudex`) | Same model, different harness: headless Claude Code through a local proxy — **read `references/claudex-runner.md` first.** Read-only stages only; requires the env contract that doc defines, else stop and report. |
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
- **Never claim a ticket with open blockers**, even if a human moved it to `Ready` — the status is priority, not feasibility. Surface the conflict instead (`references/decomposition.md` covers links).
- **The Jira comment thread is the memory.** Every `agent:*` stage ends with a marker-tagged comment (`<!-- sdlc:stage=… kind=… -->` on line 1) *before* the label moves; every stage and every resume starts by reading the board + thread, not conversation memory. Comments without a marker are human steering — incorporate and acknowledge them.
- Per-stage durations come from the **Jira issue changelog** (status + label history), never hand-tracked.

## The 6 statuses

| # | Status | Entry means | Agent does (label sequence) | Exit gate |
|---|--------|-------------|------------------------------|-----------|
| 1 | `Backlog` | issue created | Write **COS** (Conditions of Success — this pipeline's term for acceptance criteria) + scope; suggest `risk:low` if trivial. **Every pipeline ticket gets the context-mémo bake** (`decomposition.md` § context-mémo) — standalone tickets too. **Raw idea →** explore first (`exploration.md`). **PRD/multi-ticket →** decompose (`decomposition.md`). **Then shape** (`shaping.md`): fill the 8-item DoR (`readiness` field — `context_memo` is one of them, so the bake is gate-enforced), spawn spikes, flag human decisions (`owner: "human"`). | ⛔ human → `Ready` — **hard-blocked while any DoR item is `open` or a blocker is open** (`deferred` passes) |
| 2 | `Ready` | approved queue | Verify **zero open blockers**, then claim: assign self, create branch `<ISSUE-KEY>-<slug>` off `origin/main`, post a claim comment with selection reason. | agent claims → `Planning` |
| 3 | `Planning` | agent claimed | `agent:planning`: **`config.models.plan`** reads the real code + the context-mémo, picks the **test seam**, drafts the plan (DoD, files, test plan) → `agent:plan-review`: **`config.models.planReview`** critiques it adversarially, pre-mortem pass → `needs:human` (skip if `risk:low`). | ⛔ human → `Building` |
| 4 | `Building` | plan approved | `agent:coding`: **`config.models.implement`** does TDD from the approved plan, local gate green, push, open PR (`<ISSUE-KEY>` in title/body) → `agent:ai-review`: **`config.models.aiReview`** adversarially reviews the diff, triage, fix, rebase if behind, re-green, second pass if fixes were non-trivial → `needs:human`: post the review-guide on the PR, stop. | ⛔ human reviews + **merges** → `PO Review` |
| 5 | `PO Review` | human merged (deployed to DEV) | `needs:human`: post a validation note showing the **COS are met** from the user's output (via code if not user-demonstrable), + any preview link; stop pushing. | ⛔ PO validates COS + gives final go → `Done` |
| 6 | `Done` | PO accepted | Remove `needs:human`; verify PR merged + the **project DoD** (see `references/setup.md`); post the metrics comment; promote reusable learnings; delete branch. | — |

**`BLOCKED`** is transversal, not a row: a human moves any active ticket there when it hits an open blocker, and back to `Ready`/`Planning`/`Building` by context. The agent keeps its prior label and stops until it returns.

## Transitions (V0 — manual, via MCP + code repo)

Each step is an agent action on the user's command. **Jira ops = Atlassian MCP tools; code ops = `git`/`gh`/`codex`.** Model-assignable stages route per § config.models — the steps below don't repeat the how.

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
#      Post the draft attributed to the model that wrote it.
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=planning kind=plan -->
#   MCP editJiraIssue: -agent:planning +agent:plan-review
#   3b CRITIQUE — by config.models.planReview (≠ plan, enforced). Adversarial, not a rubber stamp.
#      Verify every file/symbol/test seam the draft names actually EXISTS (`git grep -n` at HEAD) —
#      a drafter that couldn't run the gate will hallucinate seams. Check against the real code,
#      the COS, the registry; pre-mortem lens (quality-practices.md §1).
#      Accept / amend / reject each part of the draft with a stated reason.
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
#   If risk:low → skip to step 4.
#   MCP editJiraIssue: -agent:plan-review +needs:human
#   ⛔ GATE 2 — wait for the human to transition Planning → Building.

# 4 Réalisation (code repo + Jira)
#   MCP editJiraIssue: -needs:human +agent:coding
#   TDD, by config.models.implement. The approved kind=plan-final IS the brief — if it isn't
#   enough to hand over, that's a plan bug; fix the plan, don't paper over it in-session.
#   One implementer, no worktree — WIP is one ticket, and a TDD loop doesn't parallelize.
#   Local gate (mirrors CI): run config.code.localGate. If you delegated, VERIFY don't trust —
#   re-run the gate yourself on the committed tree (quality-practices.md §2.b).
#   READ the gate's exit code before pushing — run the gate as the SOLE command in its shell
#   call: no trailing chains AND no prefix chains (`verify && gate > log` short-circuits on the
#   verify and reports the verify's failure as the gate's — TKT-87 closeout), and re-check cwd
#   (a mis-cwd'd `make check` exits 2 on "No rule to make target" — TKT-71's shape). Never chain
#   commit/push onto the same shell command as the gate: `gate; echo exit=$?` then push in a later call. Chained, a gate that
#   failed to even start (mis-cwd'd, missing target) ships the push anyway (TKT-71). And don't
#   pipe the gate (`gate | tail; echo $?` reads tail's exit, not the gate's — a failed gate
#   reported exit=0 on TKT-94): redirect to a file and read the file. And when the gate runs as a
#   BACKGROUND job, judge PASS/FAIL from the log body (`grep -E 'Error [0-9]+|FAIL|drifted'`), not
#   the harness-reported exit code — they diverged on TKT-101 (bg wrapper said exit 0 across three
#   runs the log showed failing: errcheck, a .dockerignore miss, a migration-version collision).
#   And judge a bg gate DONE by an explicit exit-code sentinel (`gate > log 2>&1; echo EXIT=$? > done`;
#   wait on the sentinel file), NEVER by `pgrep -f "<gate cmd>"` — `-f` matches the watcher's own
#   command line, so the poll self-matches and reports a false "still running" long after the gate
#   exited (TKT-106; docs/learnings/2026-07-21-pgrep-watchers-self-match.md).
#   Code: verify `git branch --show-current` is the ticket branch FIRST (a session's branch can
#   be switched under it — TKT-84 briefly committed to local main), then commit (no AI
#   attribution), push; gh pr create --base main --title "<ISSUE-KEY> …" --body "…<ISSUE-KEY>…"
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=coding kind=summary --> (+ stage YAML; name the implementer)
#   MCP editJiraIssue: -agent:coding +agent:ai-review
#   REVIEW — by config.models.aiReview, on the branch diff vs base, primed for guilt (Hard rules).
#   Triage findings (quality-practices.md §2): blocking→fix in PR; incidental→new backlog ticket;
#   rejected→stated reason. Rebase on origin/main if behind; re-green.
#   SECOND PASS iff the fixes were non-trivial (Hard rules). Trivial → one pass, say so in the
#   stage comment. No third on a stable diff — still churning → stop, hand to the human.
#   MCP comment kind=summary (ai-review): per-finding verdicts, what was fixed, second-pass call + why.
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

- **Thread = memory.** Markers on line 1: `<!-- sdlc:stage=<backlog|ready|planning|plan-review|coding|ai-review|po-review|done> kind=<claim|plan|plan-final|summary|blocker|metrics|readiness> -->`. The `review-guide` lives on the **PR**, not Jira. Resume = read the board status + label + the latest of each marker kind + any human comment after the last agent comment.
- **Cross-ticket memory.** Reusable learnings (codebase patterns, gotchas) go to a dedicated **"🧠 Agent memory" Jira issue** (or the team decision registry). When a pattern proves out, promote it per the **"3 houses" rule** (`references/setup.md`): **technical** standards → the team's shared standards registry (via PR), **process** learnings → the team wiki, repo-local guidance → `AGENTS.md` via a normal gated ticket. Working memory ≠ gospel.
- **Promotion is a mandatory Done step, not best-effort.** The `kind=metrics` closing comment must include a `learnings:` section — either concrete promotable items (each with its target house) or the explicit word `none`. Skipping it is the registry cold-start failure mode.
- **Metrics.** Durations from the Jira changelog (status/label transitions); `needs:human` time-in-state is the Gate 2 / Gate 3 human wait — a first-class metric, the likely bottleneck. Diff size from `gh pr view`. The `kind=metrics` comment has **required fields** — a table `stage | duration` (one row per status + per `agent:*` label), `human-wait total`, `diff: +x/−y (n files)`, the `learnings:` section, and a `retro:` section. All five, every ticket; missing data is written `n/a`, not omitted.
- **Gates waived? The closeout names every objection the agent overrode.** On a run where a human gate is replaced by agent judgement, add an `overrides:` list to `kind=metrics`: each point where a review pass (or the human, earlier) stated a **blocking** objection and the agent proceeded anyway — finding, reviewer's reason, agent's reason, and the ticket carrying the residual risk. `none` is a valid answer and must be earned. Rationale: on a gateless run the machinery catches the ordinary defects; what it cannot settle is a genuine disagreement about whether something should ship. That list is the smallest thing an owner must read to re-take the decisions that were taken for them — and it is unrecoverable later, because a fixed finding and an overridden one look identical in a merged diff (TKT-190 overrode one [high] "do not ship").
- **`retro:` = introspection on the collaboration, not the code.** Two fixed questions, answered honestly (`nothing` is allowed but must be earned): **(1)** *What would have made this ticket faster or better — missing context in the mémo, ambiguous COS, a gap in this skill?* **(2)** *What should the humans change — in the process, the config, or how the ticket was written?* Each retro item is a **concrete, appliable change** (which file/section, what edit) — not an observation. V0 is interactive: after posting the retro, offer to apply the changes now; if approved, patch in the same session and note `applied`. Unapplied items land in the weekly registry review; a recurring one is a signal to stop re-noting and patch.
- **High bar for skill edits.** This skill carries *rules*, not knowledge. A retro item may patch the skill only if it would change agent behavior in a future session **and** isn't already readable from the repo when needed — learnings (`docs/LEARNINGS.md`, `docs/learnings/`) and ADRs (`docs/adr/`) are accessible to every session; **cite them, never restate them**. Incident narratives go to `docs/learnings/`; the skill gets the rule, a one-line why, and the ticket citation. When in doubt, it's a learning, not a skill edit.

## Hard rules

- **Never merge for the human.** Gate 3 is the human's, in the code repo. Agents stop at `needs:human`.
- **Model routing is declared, never inferred.** Every model decision comes from `config.models` (§ Config). Never detect which model is driving the session and branch on it, and never silently substitute a model: if the configured one is unreachable, **stop and report**. Who did the work is a thing this pipeline measures — a stage that quietly ran on the wrong model is a corrupted measurement. **This rule is deliberately self-contained:** the skill ships to whoever installs it and must not depend on any individual's personal model preferences or global config.
- **Delegated ≠ done.** When a stage resolves to anything but `main-agent`, you still own the outcome: verify the output against the real code, re-run the gate yourself (`quality-practices.md` §2.b), and attribute the work in the stage comment. A delegated run reports a *claim*; the evidence is what you check.
- **`gpt-*` stage mechanics live in `references/codex-runner.md`** — companion script (not raw `codex exec`, not slash commands), prompt-file materialization, the raw fallback, judging output (exit 0 ≠ success), and the fixed escalation ladder. Read it before running any such stage. A `@claudex` suffix on the model swaps the harness, not the model — those mechanics live in `references/claudex-runner.md` instead.
- **Verify reviewer findings against the revision under review — not the base branch.** The review pass is a *lead generator*, not an oracle. Check each finding with `git grep -n "<sym>" HEAD` (or the working tree); use `origin/main` **only** to ask whether something pre-existed the change — grepping the base for a symbol the PR *introduces* looks like a refuted finding. Make sure your checkout is current first (a stale local `main` is what makes this go wrong). Record a per-finding verdict in the stage comment.
- **Prime the reviewer for guilt, and give it the diff only.** The reviewer is told to **assume the diff is wrong and find where** — not to "check whether it's ok". Withholding the plan and the reasoning is **deliberate**: a reviewer that has read the justification evaluates the code against the author's intent instead of against reality. Don't "help" it by pasting in the plan.
- **A second `ai-review` pass when the fixes were non-trivial — otherwise one.** The second pass exists because **the fix diff is code no one has reviewed** — judge the *fix diff*, not the original PR diff. **Non-trivial = any of:** a blocking finding was fixed (correctness/security/AC — fixing one changes logic by construction, so a real review round almost always earns a second pass); aggregate volume, even when each fix is small; new/changed control flow, signature/API/schema/migration, a money/auth/concurrency path, a new or rewritten test, or fixes spilling into files the findings didn't name. A rebase that materially changed the diff also triggers. Pure trivia (typos, comments, formatting, a compiler-verified rename) → one pass. Record the call in the stage comment either way.
  **Cap: two passes per *stable* diff — but a pass that invalidates the previous pass's fix resets the counter (absolute cap 4).** The cap stops **churn** (re-litigated style, resurfaced already-triaged findings, findings shrinking each round → stop, hand to the human), not **convergence** (a pass proved the previous fix wrong at the root and a structural rewrite followed — that rewrite is the branch's least-reviewed, riskiest commit, so run one more; TKT-61). If you do stop at the cap with an unreviewed fix diff, **say so explicitly in the stage comment *and* the review-guide**, and label your own verification as your own. **A Gate-3 human review round is not a codex trigger:** the human review *is* the authoritative adversarial pass — address its changes under `agent:coding`, re-green, return to `needs:human` for re-review of the new SHA.
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
