---
name: sdlc-ticket
description: Drive one unit of work through the agentic SDLC pipeline on the sdlc board — a 6-status board (Backlog→Ready→Planning→Building→PO Review→Done, + transversal BLOCKED) with label-driven sub-states, one note per ticket in a Fast Note Sync vault served by ~/sources/sdlc-board, plus the code repo for branch/PR/merge. Four gates - priority at entry, plan approval (skipped for risk:low), review+merge, PO acceptance at exit; held by a human or taken by the agent per config.gates ("human" | "autonomous"). Use when the user wants to run a task/ticket through the pipeline, says "run this through the SDLC pipeline", "take this ticket through the flow", "resume <TICKET-KEY>", wants a PRD/spec decomposed into pipeline tickets, or wants to explore a feature idea into tickets. V0 is manual - the agent performs each stage on the user's command through the board's HTTP API; there is no event automation.
---

# Agentic SDLC pipeline

A unit of work flows through **6 statuses** on the **sdlc board** — one note per ticket in a Fast Note Sync vault, served by `~/sources/sdlc-board` (`references/local-tracker.md`). The state machine lives in **labels**; the board statuses are the coarse human view.

**The symmetry rule:** humans move statuses (board transitions), agents move labels. Every transition expresses a human decision; every label change is agent progress within a status.

**V0 is manual.** Everything is triggered by a dev/PO in Claude Code through the **board's HTTP API**. No board automation, no webhooks — those come later (see *Future automation*). Don't wire any rule that auto-moves tickets.

## What this pipeline is for — and what it isn't

**Tickets are for product work. Internal maintenance goes straight to the default branch — no ticket, no gates, no PR.**

Commit directly to `config.code.defaultBranch` when the change is:

- a **skill / process / tooling** improvement (this skill, its references, CI config),
- an **ADR addition or amendment**,
- a **documentation** update (`docs/`, `AGENTS.md`, READMEs).

There is no gate to run because there is no product risk to gate: the local gate (`config.code.localGate`) is the whole check, and if it's green the change is done. Routing this class through the board buys ceremony, not safety.

**The line is intent, not file extension.** *"Write down the decision we just made"* → direct. *"Decide whether we should adopt X"* → a ticket, even when its only deliverable turns out to be an ADR — the plan gate is where a wrong recommendation gets reversed (TKT-62). If the change needs a **decision** or a **plan**, it's a ticket. If it only needs **writing down**, it's a commit.

Still applies to direct commits: the local gate must be green, and the commit message carries the reasoning.

## Two systems, two jobs

| System | Owns | Tools |
|---|---|---|
| **sdlc board** (FNS vault) | ticket lifecycle (statuses), the thread/memory (comments), dependencies (`blocked-by` links), the context-mémo (`context` on the note), `readiness` | the board's HTTP API (`references/local-tracker.md`) |
| **Repo docs** | PRDs and briefs (`docs/product/`), ADRs (`registry.bindingPath`), learnings (`docs/LEARNINGS.md`, `docs/learnings/`) | `git`, through the normal commit or PR path |
| **Code repo** (GitHub) | branch, commits, PR, CI, the **merge gate** | `git`, `gh` |

The ticket key (`<TICKET-KEY>`, e.g. `TKT-42`) names the branch; the PR carries it in its title/body and the ticket's `pr` field carries the PR, so the two stay linked.

## Gates

Four decision points. **Who decides is a config binding** — `config.gates`: `"human"` (the default when the key is absent — every ⛔ line in this skill and its references waits for the human, exactly as written) or `"autonomous"` (the agent takes gates 2–4 itself and runs Ready → Done in one continuous run — see § Autonomous mode). Everything else in the skill applies identically in both modes.

1. ⛔ **Priority** — human transitions `Backlog → Ready`. **Human in both modes** — the agent never self-promotes out of Backlog: shaping + the DoR still gate entry, and priority is the owner's call.
2. ⛔ **Plan** — human transitions `Planning → Building`. **Skipped when the ticket carries `risk:low`.**
3. ⛔ **Review + merge** — human reviews the PR and **merges (squash) in the code repo**, then transitions `Building → PO Review`. Agents never merge in human mode.
4. ⛔ **PO acceptance** — the PO functionally validates and transitions `PO Review → Done`.

### Autonomous mode (`config.gates: "autonomous"`)

The four human gates are replaced by agent judgement; **every other rule is unchanged and non-negotiable** — `config.models` routing (never substitute), cross-model review for both plan and code, TDD with tests observed red first, local gate green before every push and after the merge, all marker comments at each stage, no AI attribution anywhere.

State the mode in the **claim comment** ("running gateless per `config.gates: autonomous`") so the board history shows the ticket ran without human gates.

**Gate replacements:**

- **Gate 1 (priority):** not replaced — the ticket must already be in `Ready`. Verify that against the board (§ Before you start); a ticket really in Backlog gets shaped and **stops there** for the human.
- **Gate 2 (plan approval):** after the cross-model plan critique, resolve every open decision yourself: prefer the option the critique recommends; if it makes no recommendation, pick the **most reversible** option. Record each self-made material decision as a `kind=decision` comment. If a resolution changes the plan materially (`references/reviews.md` § Stage policy), it goes back through another plan-review round — amended by the drafter's model, not by you, when you share the reviewer's model (`reviews.md` § One round step 5). Post `kind=plan-final` only after the contract returns `HUMAN_REVIEW` on the final plan, reference the decision IDs from it, then transition `Planning → Building` yourself.
- **Gate 3 (review + merge):** after the review-round contract returns `HUMAN_REVIEW` for `ai-review` (`references/reviews.md` — same rounds, stage policy and cap as always), post the **review-guide on the PR first** — it becomes the audit record — then verify `git rev-parse HEAD` == `git rev-parse origin/<branch>` (Hard rules), squash-merge the PR yourself **once the required checks have passed on the PR head, without a host bypass**, delete the branch, and transition `Building → PO Review`. **If the plan chose a rebase merge** (every commit lands on the default branch), gate each code commit alone before the merge, in a temporary worktree at that commit, and record each verdict in the review-guide (TKT-523).
- **Gate 4 (PO acceptance):** self-validate **each COS against test evidence on the merged code**, post the validation note, post the full `kind=metrics` comment, **then** transition to `Done` — the board refuses Done for a non-spike ticket with no `kind=metrics` comment (or send the comment in the same `POST /ticket` as the move). The `overrides:` list (§ Memory & metrics) is mandatory on every autonomous run, `none` earned not assumed.

**Label mechanics:** at a replaced gate, don't set `needs:human` — post the stage's marker comment, then move straight to the next label/transition. `human-wait` in the metrics comment is `n/a (autonomous)`.

**Epics:** if the ticket is an epic, decompose it per `references/decomposition.md`, then run each child story Ready → Done **one at a time, in dependency order** — full pipeline per story, no batching.

**Drive style:** no ask — continuous run. Report at the end (and at each story boundary when running an epic).

**⛔ STOP and report to the human instead of proceeding only if:**

- (a) the local gate or CI fails **twice on the same cause** with no clear fix;
- (b) implementation would touch **auth, money paths, data migrations, or CI/deploy config** in a way the ticket and approved plan did not anticipate;
- (c) the review-round contract returns **`STOP`** (`references/reviews.md`): the four-round cap is reached with a review still required, a reviewer returned `BLOCKED`, or a rejected finding is disputed by its reviewer. Only the owner can authorize extra rounds;
- (d) proceeding with a **High or Critical review finding** would require an agent override. Fixing it is allowed. STOP and escalate to the owner; proceed only if the owner explicitly authorizes it after escalation. Silence, timeout, or an agent decision does not authorize proceeding. This STOP applies in every round, including at the cap;
- (e) a configured model is **unreachable** after the documented escalation ladder;
- (f) an action would be **destructive or hard to reverse** and the right choice is ambiguous.

Otherwise make the call yourself, record it (`kind=decision`, stage comments, `overrides:`), and keep going.

> **The agent stages are statuses, not just labels.** `Planning` and `Building` are **first-class board statuses** — the label axis alone isn't visible enough for human + AI co-work. **PO Review** (gate 4) and transversal **BLOCKED** complete the set. The statuses come from the vault's `_board/config.md`, so the board's columns and this skill cannot drift. Setup: `references/setup.md`.

## References (load on demand)

Read the relevant file **before** executing the step — don't improvise the how:

| File | Read when |
|---|---|
| `references/setup.md` | **First run in a project** — `.claude/sdlc.config.json` missing: bootstrap the vault + board, write the config, verify branch protection, smoke-test |
| `references/decomposition.md` | Intake is a PRD/spec or an epic bigger than one ticket — epic + child tickets, sizing, dependency links, **context-mémo bake**, readiness verdict |
| `references/exploration.md` | Intake is a raw idea, not a spec — coached brainstorm / brief / PRFAQ → brief in the epic |
| `references/shaping.md` | Ticket in Backlog needs preparing for Gate 1 — the 8-item DoR (`readiness`, incl. the context-mémo bake), spikes, human decisions |
| `references/quality-practices.md` | Pre-mortem on plans (Planning), findings triage + review-guide hand-off (Building), correct-course when a human pushes back |
| `references/plan-recap.md` | **Planning, before the last plan-review audit** — the one-minute plan page for the human (`kind=plan-recap`, shown in the board's Plan tab): format, when to post and refresh it |
| `references/reviews.md` | **Before any `plan-review` or `ai-review` work** — round structure (initial inspection + decision audit), stage repeat policy, four-round cap, refutation checks, researcher, round record + `scripts/review_round.py` |
| `references/t3-runner.md` | **The session runs in T3 Code and a stage resolves to anything but `main-agent`** — `delegate_task` target, explicit effort, read-only caveat, output checks, escalation |
| `references/codex-runner.md` | **A stage resolves to a `gpt-*` model outside T3** — `/codex:rescue`, brief files, job-record check, judging output, escalation ladder |
| `references/ab-stages.md` | **`plan` or `aiReview` is an array** — parallel arms on the same input: finding-tally scoring (aiReview), anonymized human selection (plan), logging |
| `references/sprints.md` | **`GET /board` shows `sprintMode: true`, or the ticket has a `sprint`, or it is a sprint closure task** — the claim rule, the member rules and the hand-off to `sdlc-sprint` and `sdlc-sprint-review` |
| `references/local-tracker.md` | **Before the first board read or write of a run** — endpoints, `_rev` compare-and-swap, creating tickets (`POST /ticket/new`), comments, links, authoring note fields, movement |

## Before you start

- **First run in this project?** If `.claude/sdlc.config.json` is missing, read `references/setup.md`, bootstrap, write the config, then continue.
- **Board first.** Read `references/local-tracker.md` before the first board read or write. **Writes go through `POST /ticket` on the board, always with `_rev` set to the status you are moving FROM and `role` set to your pipeline role**; new tickets go through `POST /ticket/new`. The older git-backed board (`.sdlc/server.py`, state on `sdlc-state`) is gone; that branch is now a read-only archive.
- **Read the ticket's actual status before starting anywhere.** When the run is described as starting from a status ("it's in Ready", "run it Ready to Done"), that is the requester's belief, not an observation — **verify it against the board and re-shape if it doesn't hold**, rather than entering at the named step. A ticket that is really still in Backlog has no `readiness` and no context-mémo, and starting at Ready silently skips the shaping those represent. The skip is invisible and self-confirming: the run proceeds normally and produces a plan, just one built on whatever the ticket text asserted. TKT-233 was handed over as "already in Ready" while sitting in Backlog with `readiness: null` and its own comment reading *"Needs shaping before Gate 1"* — and the shaping it would have skipped is what caught the ticket's proposed fix being unimplementable (`shaping.md` § approach). This check costs one read and protects every stage after it.
- **Resuming?** If the ticket is in flight (or the user says "resume <KEY>"), reconstruct state from the board status + label + the marker comments — never from conversation memory — and continue from the step the label indicates. **Coming back from BLOCKED?** Re-resolve the governing ADRs against `registry.bindingPath`, and re-check each shaping and plan decision against ADRs accepted since it was made. A decision made before a new ADR can send the whole resume down a path that ADR closed (TKT-504: D2 "never waive" predated ADR-081 and cost D3–D40 on a patch the app never needed).
- **Drive style:** in `gates: "human"`, V0 is interactive — ask **once at the start of the run**: *every step* (stop and report after each transition) or *gates only* (pause at the human gates); apply for the rest of the ticket, don't re-ask. In `gates: "autonomous"`, don't ask — continuous run per § Autonomous mode.
- **Risk:** agree on `risk:low` (skips the plan gate) for trivial, isolated work.

## Config (per project) — this skill is generic

Project-specific bindings live in **`.claude/sdlc.config.json`** in the code repo, not in this skill. Read it at the start of every run; anything below shaped like `config.code.localGate` comes from it.

```json
{
  "tracker":    "local (the sdlc board — references/local-tracker.md; the only supported value)",
  "gates":      "human | autonomous (default human when absent — see § Gates)",
  "code":       { "repo": "<owner/repo>", "defaultBranch": "main",
                  "localGate": "<project's local gate cmd, mirrors CI>",
                  "gateVerdict": "<optional: path the gate writes its own verdict to; omit if it doesn't>" },
  "models":     { "plan": "main-agent", "planReview": "gpt-6.1-sol",
                  "implement": "main-agent", "aiReview": "gpt-6.1-sol",
                  "sprintReview": "<optional: a model value, or an object that maps the four sprint review areas to model values — absent: aiReview>",
                  "research": "<optional: cheaper model for reviewer lookups — references/reviews.md § Researcher>" },
  "review":     { "severityMap": { "P0": "Critical", "P1": "High", "P2": "Medium", "P3": "Low", "P4": "Nit" } },
  "registry":   { "bindingPath": "docs/adr/", "referenceLocation": "local" },
  "sprints":    { "vault": "<optional: board vault name for the sprint routes>",
                  "reportLanguage": "fr | en (default en)" }
}
```

### `config.models` — who does each stage

Four stages are model-assignable, plus the optional `research` helper for the review stages (a single value; an array is a config error; mechanics in `references/reviews.md` § Researcher) and the optional `sprintReview` key (below). **The config declares it; the agent never infers it.** Read the value, resolve it via the table, run it. Never detect which model is driving the session and branch on it — a different planner is a config edit, not a runtime decision. This routing is **the single source**: the transition steps below say *what* each stage does; *who runs it and how* is always this table.

**The config picks the model; the environment picks the runner.** If the session runs in T3 Code
(the `delegate_task` tool is available), run every delegated stage — Claude models included — through
`references/t3-runner.md`. If not, use the CLI runner in the second column. Record which runner
ran in the stage comment. T3 is preferred for **visibility, not reliability**: each delegated stage
becomes a child thread linked to the ticket's thread, so the owner can see the lineage and read
what the worker did in the UI. The CLI runners stay valid; a fallback to one is not a degraded run.

| Value | Outside T3 | In T3 |
|---|---|---|
| `main-agent` | **You** do it — whichever model drives this Claude Code session. Never hardcode which one that is. | Same. |
| `gpt-6.1-sol` (any `gpt-*`) | Codex through `/codex:rescue` — **read `references/codex-runner.md` first.** | **Read `references/t3-runner.md` first.** |
| `gemini-3.8-flash-high` (any `gemini-*`) | The Antigravity CLI: `agy --model <value> --add-dir <repo path> -p '<brief>'`. Write stages also need `--dangerously-skip-permissions`. **Without `--add-dir` it works in its own scratch directory and never touches the repo** — the shell's cwd is not its workspace. | **Read `references/t3-runner.md` first.** |
| `<vendor>/<model>` (e.g. `opencode/kimi-k2.7-code`) | No runner: stop and report. | **Read `references/t3-runner.md` first.** |
| `opus-5.5`, `sonnet-5.5`, `haiku-5.5` (any Claude model) | A subagent: the `Agent` tool with `model:` and `effort:` set. Brief it from the ticket + the approved plan — it does **not** inherit this session's context. | **Read `references/t3-runner.md` first.** Same brief rule. |
| **an array of values** (`plan` and `aiReview` only) | An A/B experiment: run every arm on the same input, score, record — **read `references/ab-stages.md` first.** Arrays on `planReview`/`implement` are a config error: stop and report. | Same. |

Any value (single or array arm) may carry an **`:effort` suffix** — `gpt-6.1-sol:medium`,
`opus-5.5:xhigh`. The runner passes it as the model's effort setting. **No suffix means `high`**,
and the runner always passes it explicitly: a runner default can be lower (T3 defaults
`gpt-6.1-sol` to `low`). **Exception:** `gemini-*` carries its effort in the id
(`-low`/`-medium`/`-high`), so an `:effort` suffix there is a config error.

**`config.models.sprintReview` (optional).** It names the reviewer of each area of the `sprint-review` stage (`references/reviews.md` § Sprint review, `sdlc-sprint-review`). It is one model value, or an object that maps `implementation`, `verification`, `documentation` and `retrospective` to one model value each. Each value follows the table above, `:effort` suffix included. An array is a config error: stop and report. When the key is absent, every area uses `models.aiReview`. If `aiReview` is an array and `sprintReview` is absent, the sprint stage stops until you set `sprintReview`.

**`config.sprints` (optional).** Only the two sprint skills read it. `vault` is the board vault name that they put on every sprint route (`sdlc-sprint/references/board.md`). `reportLanguage` is `fr` or `en`, and the default is `en`.

**You are always the orchestrator.** Whatever `config.models` says, *you* read the ticket, brief the worker, **talk to the human**, revise, post every comment, and own the outcome. These keys assign **who does the thinking for a stage** — never who drives the board or holds the conversation. A delegated worker **cannot ask the human anything**: one brief in, one answer out. Any step needing a human decision (grilling on open decisions, a gate hand-off) is **yours**, in this session.

**⛔ The reviewer must never be the author.** Cross-model review is the pipeline's load-bearing invariant, constraining **two pairs**: `plan` ≠ `planReview` and `implement` ≠ `aiReview`. Check both when you read the config; if either matches, **stop and report** — don't run a degraded flow. The optional sprint stage adds a third pair, which the sprint skills check at run time: each area's `sprintReview` model differs from every author of the release and from the report author (`references/sprints.md`). A model reviewing its own output re-confirms rather than falsifies. Note `main-agent` and a named Claude model can *collide*: if the session model is the named one, that's the same violation wearing two labels. When unsure, ask. The reviewer must also differ from the author of every **fix** it re-reviews — if you fixed findings yourself, you are an author of the next round's artifact. `research` is exempt: it returns evidence, never a verdict, so it may share a model with an author.

Swapping `plan` ↔ `planReview` is how the "Codex drafts, main agent reviews" variant is expressed — a config flip, not a fork of this skill. **Cross-model review is a prerequisite, not an option:** if an environment forbids sending code to an external model, this SDLC doesn't apply there — don't build a degraded variant.

## Labels (the state machine)

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
- **In sprint mode, claim only from the active sprint.** When `GET /board` reports `sprintMode: true`, claim only a `Ready` ticket whose `sprint` is the key of the active sprint. With no active sprint, claim nothing and report that. Membership never waives a gate. Starting and closing a sprint are human requests (`references/sprints.md`).
- **The ticket's comment thread is the memory.** Every `agent:*` stage ends with a marker-tagged comment (`<!-- sdlc:stage=… kind=… -->` on line 1) *before* the label moves; every stage and every resume starts by reading the board + thread, not conversation memory. Comments without a marker are human steering — incorporate and acknowledge them.
- Per-stage durations come from the vault's **transition log** (`_log/transitions.md`, served as `GET /metrics?key=K`), never hand-tracked.

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
entry. When a later fact changes one, put `Supersedes D<n>` in the new Decision line. IDs are
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
| 1 | `Backlog` | ticket created | Write **COS** (Conditions of Success — this pipeline's term for acceptance criteria) + scope; suggest `risk:low` if trivial. **Every pipeline ticket gets the context-mémo bake** (`decomposition.md` § context-mémo) — standalone tickets too. **Raw idea →** explore first (`exploration.md`). **PRD/multi-ticket →** decompose (`decomposition.md`). **Then shape** (`shaping.md`): fill the 8-item DoR (`readiness` field — `context_memo` is one of them, so the bake is gate-enforced), spawn spikes, flag human decisions (`owner: "human"`), and record each settled material choice as a `kind=decision` entry. | ⛔ human → `Ready` — **hard-blocked while any DoR item is `open` or a blocker is open** (`deferred` passes) |
| 2 | `Ready` | approved queue | Verify **zero open blockers**, then claim: assign self, create branch `<TICKET-KEY>-<slug>` off `origin/main`, post a claim comment with selection reason. | agent claims → `Planning` |
| 3 | `Planning` | agent claimed | `agent:planning`: **`config.models.plan`** reads the real code + the context-mémo, picks the **test seam**, drafts the plan and records material `kind=decision` entries → `agent:plan-review`: **`config.models.planReview`** critiques the plan and planning decision log in review rounds (`references/reviews.md`), pre-mortem pass → `needs:human` (skip if `risk:low`). | ⛔ human reviews the plan bundle → `Building` |
| 4 | `Building` | plan approved | `agent:coding`: **`config.models.implement`** does TDD from the approved plan and records new or changed material decisions, local gate green, push, open PR (`<TICKET-KEY>` in title/body) → `agent:ai-review`: **`config.models.aiReview`** reviews in rounds (`references/reviews.md`): the diff blind, then a decision audit with both decision logs on the same head; triage, fix, rebase if behind, re-green; another round for Critical/High findings or a material change, four max → `needs:human`: post the review-guide on the PR, stop. | ⛔ human reviews the development bundle + **merges** → `PO Review` |
| 5 | `PO Review` | human merged (deployed to DEV) | `needs:human`: post a validation note showing the **COS are met** from the user's output (via code if not user-demonstrable), + any preview link; stop pushing. | ⛔ PO validates COS + gives final go → `Done` |
| 6 | `Done` | PO accepted | The metrics comment is posted **before** the move (the board refuses Done without it). Remove `needs:human`; verify PR merged + the **project DoD** (see `references/setup.md`); promote reusable learnings; delete branch. | — |

**`BLOCKED`** is transversal, not a row: a human moves any active ticket there when it hits an open blocker, and back to `Ready`/`Planning`/`Building` by context. The agent keeps its prior label and stops until it returns.

## Transitions (V0 — manual, via the board API + code repo)

Each step is an agent action on the user's command. **Board ops = the board's HTTP API (`references/local-tracker.md` § Operations); code ops = `git`/`gh`.** Model-assignable stages route per § config.models — the steps below don't repeat the how. The ⛔ GATE lines below (and in the references) are the **human-mode** behavior; in `gates: "autonomous"`, apply the corresponding replacement from § Autonomous mode instead — gates 2–4 only, Gate 1 always waits.

```
# 1 Intake (board)
#   POST /ticket/new {summary, type=Epic|Story|Task|Spike, description (problem + COS), role}
#   — the server allocates the key. Add label risk:low only if agreed. Status starts at Backlog.
#   If PRD/spec → references/decomposition.md (epic + children + links + context-mémo + verdict).
#   ⛔ GATE 1 — wait for the human to transition Backlog → Ready.

# 2 Queue + claim
#   Verify zero open blockers (GET /ticket?key=K → every `blocked-by` link's ticket is Done).
#   Sprint mode on (`GET /board` → `sprintMode: true`)? Claim only a member of the active sprint (§ Labels).
#   POST assignee=self (no pipeline label yet — the first one is set in step 3).
#   Code repo:  git fetch && git switch -c <TICKET-KEY>-<slug> origin/main
#   POST comment <!-- sdlc:stage=ready kind=claim --> + selection reason
#     + an EXPLICIT risk verdict: "risk:low claimed / not claimed, because …". State it even when
#     the answer is obvious — silence is what lets a mislabeled ticket through, and the label's
#     carve-outs (auth, data migrations, CI/deploy config, >5 files) are exactly the tickets whose
#     risk is obvious to the claimer and invisible in the thread afterwards (TKT-190).
#     + REQUIRED: "premises verified at HEAD" — walk the context-mémo's `premises` list and the
#     `readiness` object's load-bearing facts, and give each a verdict. Diff from the mémo's
#     `capturedCommit` (`git log <capturedCommit>..origin/main`), not from its date. Shaping is a SNAPSHOT; the ticket sat in Ready while siblings
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

# 3 Plan (board)
#   POST labels +agent:planning
#   3a DRAFT — by config.models.plan. Read the ticket + its `context` object (GET /ticket?key=K) YOURSELF first —
#      you brief the drafter; don't make it guess the mémo. Whoever drafts: read the real code,
#      pick the test seam, produce DoD / files / test plan / risks. Delegated drafts are
#      READ-ONLY — the drafter never touches the tree. Brief rules: the runner doc.
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
#   POST comment <!-- sdlc:stage=planning kind=plan -->
#   As material choices are made, append one <!-- sdlc:stage=planning kind=decision --> comment
#   per choice using § Decision log artifacts. Do not reconstruct the log at the end.
#   POST labels -agent:planning +agent:plan-review
#   3b CRITIQUE — by config.models.planReview (≠ plan, enforced). Adversarial, not a rubber stamp.
#      Verify every file/symbol/test seam the draft names actually EXISTS (`git grep -n` at HEAD) —
#      a drafter that couldn't run the gate will hallucinate seams. Check against the real code,
#      the COS, the registry; pre-mortem lens (quality-practices.md §1).
#      Run it in ROUNDS per references/reviews.md: each round = initial inspection of the steps,
#      scope and test strategy without rationale or decision log, then the decision audit with
#      every planning decision entry, on the same frozen plan. Post kind=review, run
#      scripts/review_round.py, act on its output. Plan review repeats for Critical/High
#      findings or a material change; four rounds max.
#      Accept / amend / reject each finding with a stated reason (its disposition).
#      After reviewing the delegate's output, append a stage=plan-review kind=decision entry
#      for any material choice introduced while amending the plan — and route amendments per
#      reviews.md § One round step 5 when another round follows.
#   3c FINALIZE — YOU, always (a delegated worker cannot talk to the human): grill the human on
#      the open decisions (complex tickets, one question at a time). An answer that changes the
#      plan MATERIALLY (reviews.md § Stage policy) is not finalizing — it goes back to 3b for
#      another round, amended per reviews.md § One round step 5. Only when the last round's
#      contract output is HUMAN_REVIEW on the plan as it now stands, post kind=plan-final
#      as a DELTA, not a restatement: amendments + decisions + the required sections, with "the
#      kind=plan draft as amended above is the plan". Restating the full plan doubles the thread
#      for zero audit value (TKT-84 retro); resume already reads draft + delta together.
#      REQUIRED SECTION — "introduced at plan-review, reviewed by no second model": every
#      NON-material design element added after the last completed review round, or by a fix
#      below the repeat threshold. A material one cannot be on this list — it needed a round. Those reach Gate 2 reviewed by nobody but their author — the
#      cross-model guarantee covers what a round saw, not later inventions (TKT-57). Name them
#      again in the Gate 2 hand-off. If the list is empty, write `none`.
#      REQUIRED SECTION — `Review rounds: <n> of 4; final contract output: HUMAN_REVIEW`.
#      REQUIRED SECTION — `Decision log: <IDs>` or `Decision log: none`. Confirm the listed
#      entries cover every material choice introduced while drafting or revising the plan.
#      With it, post <!-- sdlc:stage=planning kind=plan-recap --> (references/plan-recap.md): the
#      complete plan in plain language, written by config.models.plan and checked by
#      config.models.planReview (never by its author): audited in the last round — or, when that
#      round's fixes changed what it says, rewritten and fidelity-checked (plan-recap.md step 2). It is what the
#      human reads at Gate 2, in the board's Plan tab.
#   If risk:low → skip to step 4.
#   POST labels -agent:plan-review +needs:human
#   ⛔ GATE 2 — wait for the human to transition Planning → Building.

# 4 Build (code repo + board)
#   POST labels -needs:human +agent:coding
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
#   attribution), push; gh pr create --base main --title "<TICKET-KEY> …" --body "…<TICKET-KEY>…"; POST pr
#   POST comment <!-- sdlc:stage=coding kind=summary --> (+ stage YAML; name the implementer)
#   POST labels -agent:coding +agent:ai-review
#   REVIEW — by config.models.aiReview, in ROUNDS per references/reviews.md. Each round: freeze
#   base/head, initial inspection of the branch diff vs base without rationale (primed for
#   guilt), then the decision audit on the SAME head with the approved plan plus both decision
#   logs (Hard rules). No fixes between the two lenses.
#   Triage findings (quality-practices.md §2): blocking→fix in PR (disposition `fixed`);
#   incidental→new backlog ticket (`deferred`); rejected→stated reason, refutation check at the
#   repeat threshold (`rejected`). Rebase on origin/main if behind; re-green.
#   Never start a fix round in a worktree while a gate you launched there holds `.gate.lock` —
#   the gate reads the tree as it goes. Wait for `.gate.verdict` first (TKT-504).
#   After reviewing the delegate's output, YOU append a stage=ai-review kind=decision entry when
#   a finding fix introduces or changes a material choice. A fix is implementation too; do not
#   hide its decisions in the summary.
#   Post kind=review (the round record), run scripts/review_round.py, act on its output:
#   REVIEW_AGAIN → next round (Critical/High finding or material change); FIX → finish
#   dispositions/verification; STOP → kind=blocker, keep agent:ai-review, ask the human;
#   HUMAN_REVIEW → continue. Four rounds max; the count lives in the thread and never resets.
#   POST comment kind=summary (ai-review): per-finding verdicts, what was fixed, decision-audit
#   verdicts, `Decision log: <development IDs>` or `Decision log: none`, `Review rounds: <n> of
#   4` with the final contract output, fixes handed off not re-reviewed (below the threshold),
#   and `decision-audit: yield: <N> proposed, <B> blocking, <I> incidental, <R> rejected;
#   <short names or none>`. `N` equals `B + I + R` and counts, across all rounds, only findings
#   the audit lens raised that its round's initial inspection did not.
#   POST labels -agent:ai-review +needs:human ; post the review-guide on the PR. STOP pushing.
#   ⛔ GATE 3 — human reviews + merges (squash) in the code repo, then transitions Building → PO Review.
#     Do NOT merge yourself. Merge conflict? swap back to agent:coding, rebase, re-green,
#     back to needs:human (re-review the new SHA). Changes requested? swap to agent:coding, fix,
#     re-green; a material change (reviews.md) runs the next AI round in the same budget first.

# 5 PO validation (board)
#   needs:human stays set (ball is with the PO).
#   POST comment <!-- sdlc:stage=po-review kind=summary --> validation note (what to check, preview/deploy link),
#   answering the current plan recap's "Done when" list item by item.
#   ⛔ GATE 4 — the PO functionally validates and transitions PO Review → Done.
#     Changes requested? swap needs:human back to agent:coding, fix in the PR (re-open if needed), re-green.

# 6 Close (board)
#   The board refuses Done for a non-spike ticket without a kind=metrics comment, so post it
#   BEFORE the PO clears Gate 4 (local-tracker.md § The one-ticket run):
#   POST comment <!-- sdlc:stage=done kind=metrics --> (durations from GET /metrics?key=K).
#   after PO accepts (status = Done): POST labels -needs:human.
#   promote reusable learnings (see Memory below); code: delete the merged branch.
#   Last open child of an epic? Close the epic (local-tracker.md § Epics).
```

## Memory & metrics

- **Thread = memory.** Markers on line 1: `<!-- sdlc:stage=<backlog|ready|planning|plan-review|coding|ai-review|po-review|done> kind=<claim|plan|plan-final|plan-recap|decision|review|summary|blocker|metrics|readiness> -->`. `kind=plan-recap` is the human-readable current plan (`references/plan-recap.md`); the newest one is current. `kind=review` is one review round record (`references/reviews.md` § Round record); the round count per stage is the number of **distinct `round` numbers** among them, not the number of comments (A/B arms and re-posted records share a round). The `review-guide` lives on the **PR**, not the board. Resume = read the board status + label + the latest of each marker kind + every `kind=decision` and `kind=review` comment + any human comment after the last agent comment. Sprint comments live on the sprint note and use `stage=sprint` and `stage=sprint-review` (`sdlc-sprint/references/records.md`).
- **Cross-ticket memory.** Reusable learnings (codebase patterns, gotchas) go to `docs/learnings/` with a one-line entry in `docs/LEARNINGS.md`. When a pattern proves out, promote it per the **"3 houses" rule** (`references/setup.md`): **technical** decisions → an ADR in `registry.bindingPath`, **process** rules → `AGENTS.md` or this skill (internal maintenance, direct commit), **product** definition → `docs/product/`. Working memory ≠ gospel.
- **Promotion is a mandatory Done step, not best-effort.** The `kind=metrics` closing comment must include a `learnings:` section — either concrete promotable items (each with its target house) or the explicit word `none`. Skipping it is the registry cold-start failure mode.
- **Metrics.** Durations from the vault's transition log (`GET /metrics?key=K`); `needs:human` time-in-state is the Gate 2 / Gate 3 human wait — a first-class metric, the likely bottleneck. Diff size from `gh pr view`. The `kind=metrics` comment has **required fields** — a table `stage | duration` (one row per status + per `agent:*` label), `human-wait total`, `diff: +x/−y (n files)`, the `learnings:` section, and a `retro:` section. All five, every ticket; missing data is written `n/a`, not omitted.
- **Gates waived? The closeout names every objection the agent overrode.** On any run where a human gate is replaced by agent judgement — every `gates: "autonomous"` run, or an ad-hoc waiver — add an `overrides:` list to `kind=metrics`: each point where a review pass (or the human, earlier) stated a **blocking** objection and the agent proceeded anyway — finding, reviewer's reason, agent's reason, and the ticket carrying the residual risk. `none` is a valid answer and must be earned. On an autonomous run, an agent must not override a High or Critical finding. If proceeding would require an agent override, STOP and escalate to the owner. If the owner explicitly authorizes proceeding after escalation, record the finding, the owner's decision, and the residual risk in the `kind=metrics` closeout. Silence, timeout, or an agent decision does not authorize proceeding. Fixing the finding is allowed. The round cap does not change this rule. Also list every owner authorization of extra review rounds, and every fix handed off not re-reviewed. Rationale: on a gateless run the machinery catches the ordinary defects; what it cannot settle is a genuine disagreement about whether something should ship. That list is the smallest thing an owner must read to re-take the decisions that were taken for them — and it is unrecoverable later, because a fixed finding and an overridden one look identical in a merged diff (TKT-190 overrode one [high] "do not ship").
- **`retro:` = introspection on the collaboration, not the code.** Copy the `decision-audit:` yield from the `agent:ai-review` summary; do not reconstruct it at Gate 4. Add `audit-streak: <n>` to that line. Order completed, non-spike tickets by `(Done-transition timestamp, ticket key)` from history; the previous ticket is the one immediately before this ticket. Read its metrics comment once. If this audit has `blocking: 0`, increment the previous streak; start at 1 when the previous comment has no streak. If this audit is missing or has a blocking finding, write 0. At 10, add a retro proposal to remove the informed pass. The proposal does not change the workflow until a human accepts it. Then answer two fixed questions honestly (`nothing` is allowed but must be earned): **(1)** *What would have made this ticket faster or better — missing context in the mémo, ambiguous COS, a gap in this skill?* **(2)** *What should the humans change — in the process, the config, or how the ticket was written?* Each retro item is a **concrete, appliable change** (which file/section, what edit) — not an observation. V0 is interactive: after posting the retro, offer to apply the changes now; if approved, patch in the same session and note `applied`. Unapplied items land in the weekly registry review; a recurring one is a signal to stop re-noting and patch.
- **High bar for skill edits.** This skill carries *rules*, not knowledge. A retro item may patch the skill only if it would change agent behavior in a future session **and** isn't already readable from the repo when needed — learnings (`docs/LEARNINGS.md`, `docs/learnings/`) and ADRs (`docs/adr/`) are accessible to every session; **cite them, never restate them**. Incident narratives go to `docs/learnings/`; the skill gets the rule, a one-line why, and the ticket citation. When in doubt, it's a learning, not a skill edit.
  **A learning that asserts a fact about a tool or a database records HOW it was established.** Those files are cited as authority and nothing gates them, so a plausible-but-wrong sentence in one is read as settled and propagates: TKT-234 found that `2026-08-09-a-total-order-is-not-a-meaningful-one.md` had claimed for three weeks that an unconditional `BEFORE INSERT` trigger governs logical-replication apply — it does not, and the same false claim had by then been copied into an ADR, a migration comment and a test comment. One `psql` session settled it. Write the command and its output beside the claim, or write that you have not run it. Corollary when correcting one: **grep for the CLAIM, not the symbol**, across `docs/` and comments — the copies do not share a spelling, and three consecutive review passes each found one of them.

## Hard rules

- **Never merge at a gate the config gives to the human.** In `gates: "human"`, Gate 3 is the human's, in the code repo — agents stop at `needs:human`. In `gates: "autonomous"`, the agent merges per § Autonomous mode, review-guide posted first and the pre-merge push check done.
- **Model routing is declared, never inferred.** Every model decision comes from `config.models` (§ Config). Never detect which model is driving the session and branch on it, and never silently substitute a model: if the configured one is unreachable, **stop and report**. Who did the work is a thing this pipeline measures — a stage that quietly ran on the wrong model is a corrupted measurement. **This rule is deliberately self-contained:** the skill ships to whoever installs it and must not depend on any individual's personal model preferences or global config.
- **Delegated ≠ done.** When a stage resolves to anything but `main-agent`, you still own the outcome: verify the output against the real code, re-run the gate yourself (`quality-practices.md` §2.b), and attribute the work in the stage comment. A delegated run reports a *claim*; the evidence is what you check.
- **Delegated-stage mechanics live in the runner docs** — `references/t3-runner.md` in T3, `references/codex-runner.md` for `gpt-*` outside it. They cover the call, explicit model and effort, briefs, judging output (completed ≠ ran) and the fixed escalation ladder. Read the right one before running any such stage.
- **A delegated read-only stage must leave the tree unchanged — check it.** No runner enforces read-only by itself: T3 plan mode is an instruction to the model, and a worker once wrote a 22KB file under a read-only tool allowlist (TKT-104). Run `git status --porcelain` on the ticket worktree before and after each read-only stage. Revert any file the worker created and record the breach in the stage comment. Never let a stray write reach a commit.
- **Verify reviewer findings against the revision under review — not the base branch.** The review pass is a *lead generator*, not an oracle. Check each finding with `git grep -n "<sym>" HEAD` (or the working tree); use `origin/main` **only** to ask whether something pre-existed the change — grepping the base for a symbol the PR *introduces* looks like a refuted finding. Make sure your checkout is current first (a stale local `main` is what makes this go wrong). Record a per-finding verdict in the stage comment.
- **Review through two lenses, in order.** First prime the reviewer for guilt and give it the diff only. Tell it to **assume the diff is wrong and find where**, not to "check whether it's ok". This blind pass keeps the author's rationale from persuading the reviewer before it inspects reality. Record the blind findings, then — on the **same frozen revision**, with no fixes in between — give the reviewer the approved plan and both decision logs for a **decision audit**. The audit checks that each choice is supported by the code and evidence, that the implementation follows the approved plan, and that no material choice is missing from the log. Never replace the blind pass with the informed pass. A good explanation can still defend bad code.
- **Review rounds, the repeat policy and the cap live in `references/reviews.md`.** One round = initial inspection + decision audit on one frozen artifact. `plan-review` repeats for Critical/High findings, `ai-review` for Critical/High, and both for a material change. Four rounds per stage per ticket; more only with the human's explicit authorization. `scripts/review_round.py` decides the next action from the round record — act on its output, never on your own reading of the findings. Fixes below the threshold are handed off **not re-reviewed**, and the review-guide says so.
- **A test written while fixing a review finding gets the same mutation check as a test written from the plan** — break what it covers, observe red, restore — and the stage comment records the result. This is where the discipline actually fails: it holds for planned tests and lapses under fix-momentum, when a test is a means to closing a finding rather than the point of the work. Four assertions across TKT-22 could not fail, and every one was written mid-fix; a reviewer found all four. Also run the TS/typecheck build before invoking the full gate — `vitest` does not typecheck, and a type error found in twenty seconds is cheaper than one found three minutes into a gate run.
- **V0 is manual** — no board automation / webhooks. Movement is agent-driven on the user's command.
- **Never push after setting `needs:human`** without first swapping back to an `agent:*` label.
- **Before any merge, verify your last commit actually reached the PR head.** A commit made locally after your last `git push` is invisible to the PR — the reviewer approved, and the merge captures, a SHA that predates it. Whenever you merge (Gate 3, or a waived/gateless run where the agent merges), first assert `git rev-parse HEAD` == `git rev-parse origin/<branch>`; if they differ, push, then re-merge. This is the write-side of the stacked-PR/squash trap: the badge and the reviewed artifact can both be stale relative to your working tree. TKT-97 squash-merged a fix that was committed but never pushed — the pre-fix version merged, caught only by verifying `origin/main` *content* after the merge; recovery meant recovering the orphaned commit and cherry-picking it onto the default branch.
- **When a human pushes back** — diagnose the failing layer first (**intent / plan / implementation**) and regenerate from that layer, never patch locally at a lower one (`quality-practices.md` §4).
- **TDD in `Building`** — tests first; local gate = `config.code.localGate` (mirrors CI). Required CI checks are the project's configured required checks on `config.code.defaultBranch`.
- Default branch (`config.code.defaultBranch`): **ticketed product work** goes through a PR, and the project's required checks must pass on the PR head before the merge. **Internal maintenance is exempt** (see § What this pipeline is for). This is a pipeline rule. It applies even when the host would let you skip it. **Never merge ticketed work with a host bypass** (for example `gh pr merge --admin`, or the UI's bypass checkbox). If the merge is blocked, wait for the checks or fix them. What the host enforces, and for whom, is a project binding: record it in the project's instructions. Verify it rather than trusting it: on GitHub, read both `gh api repos/<o>/<r>/branches/<b>/protection` and `gh api repos/<o>/<r>/rules/branches/<b>`. An endpoint you cannot access does not prove that nothing is enforced.

## Future automation (design intent, not implemented)

When V0 has proven out: a watcher on the vault's transition log becomes the trigger surface; a human gate becomes a status transition (or an `/approve` comment a rule converts). The merge gate stays human, in the code repo. Nothing in this skill should depend on automation — the manual path is the source of truth.

## Notes

- This supersedes the GitHub-based 9-stage and 5-column versions. The GitHub variant lives on the `sdlc-pipeline-5-columns` worktree branch for reference.
- Lighter than the repo's metaswarm workflow — don't auto-invoke metaswarm gates here unless asked.
