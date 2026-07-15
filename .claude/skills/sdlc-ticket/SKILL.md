---
name: sdlc-ticket
description: Drive one unit of work through the agentic SDLC pipeline on JIRA — a 6-status board (Backlog→Ready→Planning→Building→PO Review→Done, + transversal BLOCKED) with label-driven sub-states, plus the code repo for branch/PR/merge. Human gates - priority at entry, plan approval (skipped for risk:low), review+merge, PO acceptance at exit. Use when the user wants to run a task/ticket through the pipeline, says "run this through the SDLC pipeline", "take this ticket through the flow", "resume <ISSUE-KEY>", wants a PRD/spec decomposed into pipeline tickets, wants to explore a feature idea into tickets, or references the "SDLC agentique" board. V0 is manual - the agent performs each stage on the user's command in Claude Code via the Atlassian MCP; there is no event automation.
---

# Agentic SDLC pipeline (Jira)

A unit of work flows through **6 statuses** on the **"SDLC agentique"** Jira project. The state machine lives in **labels**; the board statuses are the coarse human view.

**The symmetry rule:** humans move statuses (Jira transitions), agents move labels. Every transition expresses a human decision; every label change is agent progress within a status.

**V0 is manual.** Everything is triggered by a dev/PO in Claude Code via the **Atlassian MCP** (« crée un epic », « prends le prochain ticket », « fais le découpage »). No Jira automation, no webhooks — those come later (see *Future automation*). Don't wire any rule that auto-moves tickets.

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
| `references/shaping.md` | Ticket in Backlog needs preparing for Gate 1 — the 7-item DoR (`readiness`), the shaping pass, spikes (timeboxed investigation tickets), human decisions |
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
  "registry":   { "bindingPath": "docs/decisions/", "referenceLocation": "confluence" }
}
```

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
| 1 | `Backlog` | issue created | Write **COS** (Conditions of Success — this pipeline's term for acceptance criteria) + scope; suggest `risk:low` if trivial. **Every pipeline ticket gets the context-mémo bake** (`decomposition.md` § context-mémo) — standalone tickets too, not just PRD children. **Raw idea →** explore first (`exploration.md`). **PRD/multi-ticket →** decompose (`decomposition.md`): epic + children + dependency links + context-mémo bake, ending with the readiness verdict. **Then shape** (`shaping.md`): fill the 7-item DoR (`readiness` field), spawn spikes for investigations, flag human decisions (`owner: "human"`). | ⛔ human → `Ready` — **hard-blocked while any DoR item is `open` or a blocker is open** (`deferred` passes) |
| 2 | `Ready` | approved queue | Verify **zero open blockers**, then claim: assign self, create branch `<ISSUE-KEY>-<slug>` off `origin/main` in the code repo, post a claim comment with selection reason. | agent claims → `Planning` |
| 3 | `Planning` | agent claimed | `agent:planning`: read the real code + the ticket's context-mémo, pick the **test seam**, draft the plan (DoD, files, test plan), grill the human on open decisions (complex tickets), pre-mortem pass → `agent:plan-review`: Codex critique, revise, post final plan → `needs:human` (skip if `risk:low`). | ⛔ human → `Building` |
| 4 | `Building` | plan approved | `agent:coding`: TDD, local gate green, push, open PR (`<ISSUE-KEY>` in title/body) → `agent:ai-review`: Codex adversarial review of the diff, triage findings, rebase if behind, re-green → `needs:human`: post the review-guide on the PR, stop. | ⛔ human reviews + **merges** → `PO Review` |
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
#   read code + the ticket's agent-context property; draft plan; pre-mortem (quality-practices.md §1);
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=planning kind=plan -->
#   MCP editJiraIssue: -agent:planning +agent:plan-review
#   codex (plugin, agent-invocable): the plan is NOT in git — materialize it first, else the
#   reviewer sees nothing and "passes" vacuously (see Hard rules):
#     Write .plan-review.$$.md with the Write tool (NOT printf/echo — plan text runs shell
#     metacharacters), plan + only the code under discussion, kept lean.
#     node "$CODEX" adversarial-review --scope working-tree \
#       "Critique this plan for feasibility, missing scope, risk. Attack the approach."
#     confirm the plan file is in the review context; then delete the file this run created.
#   ($CODEX = ~/.claude/plugins/marketplaces/openai-codex/plugins/codex/scripts/codex-companion.mjs)
#   NOTE: --background is ignored for reviews — background via the harness or wrap in `timeout`.
#   revise; post kind=plan-final.  If risk:low → skip to step 4.
#   MCP editJiraIssue: -agent:plan-review +needs:human
#   ⛔ GATE 2 — wait for the human to transition Planning → Building.

# 4 Réalisation (code repo + Jira)
#   MCP editJiraIssue: -needs:human +agent:coding
#   Code: TDD. Local gate (mirrors CI): run config.code.localGate.
#   Code: commit (no AI attribution), push; gh pr create --base main --title "<ISSUE-KEY> …" --body "…<ISSUE-KEY>…"
#   MCP addCommentToJiraIssue: <!-- sdlc:stage=coding kind=summary --> (+ stage YAML)
#   MCP editJiraIssue: -agent:coding +agent:ai-review
#   codex (plugin, agent-invocable) — reviews the branch diff directly, no prompt file needed:
#     node "$CODEX" adversarial-review --base origin/main --scope branch \
#       "Correctness, security, coverage. Challenge the approach, not just defects."
#   (--background is IGNORED for reviews: background via the harness or wrap in `timeout`)
#   triage findings (quality-practices.md §2): blocking→fix in PR; incidental→new backlog ticket; rejected→stated reason.
#   rebase on origin/main if behind; re-green. MCP comment kind=summary (ai-review).
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
- **Reaching Codex — call the plugin's companion script directly; it is agent-invocable.** Where `codex@openai-codex` is installed, run the **companion script**, not raw `codex exec`. The `/codex:*` slash commands are `disable-model-invocation: true` (human-only), but the script they wrap is not — so the agent automates both review stages itself:
  ```bash
  CODEX=~/.claude/plugins/marketplaces/openai-codex/plugins/codex/scripts/codex-companion.mjs
  # ai-review — the PR diff vs the base branch
  node "$CODEX" adversarial-review --base origin/main --scope branch "<focus: what to attack>"
  # plan-review — the plan is NOT in git, so materialize it first (see next bullet)
  node "$CODEX" adversarial-review --scope working-tree "<focus>"
  ```
  Use **`adversarial-review`** for both stages: it takes focus text and challenges the approach. Plain `review` is native-review only and **rejects focus text entirely**. Both map to Codex's built-in reviewer — **no `-m` flag to get wrong**. Args: `--base <ref>`, `--scope auto|working-tree|branch`.
- **⚠ `--background` is a lie for reviews — verified in the companion script.** It is parsed and then **ignored**: `review`/`adversarial-review` unconditionally call `runForegroundCommand` (only `task` honours it), so `status`/`result` polling does **not** work for review jobs and a stalled review blocks the caller. The plugin's own command docs are wrong about this. **Instead: run the review through the harness's own backgrounding** (`Bash(..., run_in_background: true)`) or wrap it in `timeout`, so a hang is caught rather than waited out. `--wait` is equally redundant — foreground is the only mode.
- **`plan-review` needs the plan materialized — the reviewer only reads git, never the board.** Both commands diff a working tree or branch; a plan living in a board comment is invisible, so a bare invocation reviews *nothing* and "passes" vacuously. **A plan-review whose scope contained no plan is a failed stage, not a pass** — confirm the plan file appears in the review's context before accepting the verdict. Write it with a **file-writing tool or a quoted heredoc**, never by interpolating plan text into a shell command: ticket text and inlined code routinely contain backticks and `$(…)`, which a double-quoted `printf`/`echo` will **execute**. Use a collision-resistant name (`.plan-review.$$.md`), and only delete what this run created — never blindly `rm` a path a human might own. Keep the file lean (plan + only the code under discussion); a very large untracked file risks being dropped from the review context, which silently reproduces the vacuous pass.
- **Raw `codex exec` — fallback only, when the plugin is absent.** (a) **Pass `-m` explicitly** — the CLI default errors `requires a newer Codex`. The value is **environment-specific**: `-m gpt-5.6-sol` on a ChatGPT-account box (plain `gpt-5.6` is rejected there), `-m gpt-5.6` where TKT-50/TKT-54 learned it (macOS, codex-cli 0.14x). Read `~/.codex/config.toml` for the local default rather than trusting either literal. (b) Feed the prompt via **STDIN redirect** (`< prompt.txt`), never as a positional arg — a large arg hangs at `Reading additional input from stdin…`. (c) **Inline the plan/diff in the prompt file** rather than telling codex to `git diff` itself. (d) Run it **backgrounded + poll**. (e) **A local skill will hijack the prompt and cannot be stopped** — "review"/"critique" loads the machine's skills (possibly an *unrelated client's*); neither `--disable plugins` nor `-c features.skills=false` prevents it (`--disable skills` isn't a valid flag). This hijack also happens *through the plugin*. Survivable, not preventable.
- **Codex failing ≠ stage done — and exit 0 ≠ success.** **Read the output; never judge by exit status** (raw `exec` exits 0 on both a zero-findings hijack and a bad flag that never reached the model). Judge by **whether the reviewer actually ran against the intended scope and returned a conclusive verdict** — *not* by whether it found defects. **A conclusive "no findings" on a genuinely clean diff is a PASS**; a run that reviewed the wrong scope, or returned only a rubric template, is a failure however many sections it printed. A run that announces a foreign skill but returns concrete, code-cited findings is likewise a pass — the rubric is irrelevant. On failure, retry once, then **stop with the current `agent:*` label and report**. Never substitute a self-review; a skipped review that looks done is worse than a stalled ticket.
- **Verify Codex's findings against the revision under review — not the base branch.** The pass is a *lead generator*, not an oracle. Check each finding with `git grep -n "<sym>" HEAD` (or the working tree); use `origin/<default-branch>` **only** to ask whether something pre-existed the change. Grepping the base for a symbol the PR introduces finds nothing and looks like a refuted finding — it would have "disproved" `ErrGroupedSlotLifecycle` during TKT-58's own review. Record a per-finding verdict in the stage comment; surviving findings are the review's value, the rest is noise you'd otherwise promote into the codebase.
- **One codex pass per stage.** After triaging and fixing findings, re-green the local gate and post — don't re-run codex on your own fixes. A second pass happens only if the human asks or a rebase materially changed the diff. **A Gate-3 human review round is not a trigger:** the human review *is* the authoritative adversarial pass, so address its changes under `agent:coding`, re-green, and return to `needs:human` for re-review of the new SHA — without a new codex run.
- **V0 is manual** — no Jira automation / webhooks. Movement is agent-driven on the user's command.
- **Never push after setting `needs:human`** without first swapping back to an `agent:*` label.
- **When a human pushes back** — diagnose the failing layer first (**intent / plan / implementation**) and regenerate from that layer, never patch locally at a lower one (`quality-practices.md` §4).
- **TDD in `Building`** — tests first; local gate = `config.code.localGate` (mirrors CI). Required CI checks are the project's configured required checks on `config.code.defaultBranch`.
- Branch protection on `config.code.defaultBranch`: PR required, required checks must pass, no direct push.

## Future automation (design intent, not implemented)

When V0 has proven out: Jira **automation rules / webhooks** become the trigger surface. A human gate becomes a status transition (or an `/approve` comment a rule converts); a rule notifies the agent runner. The merge gate stays human, in the code repo. Nothing in this skill should depend on automation — the manual path is the source of truth.

## Notes

- Atlassian MCP tools: `createJiraIssue`, `editJiraIssue`, `transitionJiraIssue`, `getTransitionsForJiraIssue`, `getJiraIssue`, `searchJiraIssuesUsingJql`, `addCommentToJiraIssue`, `createIssueLink`, `getIssueLinkTypes`, `createConfluencePage`. **Entity-property writes** (the context-mémo `agent-context` key) are **not exposed by the MCP** — write via the Jira REST API (`PUT /rest/api/3/issue/{key}/properties/agent-context`); read back via `getJiraIssue` (properties). Confirm on your instance. **Fallback when REST property writes are unavailable** (client blocks API tokens): store the same payload as an HTML-comment block at the *end* of the issue description — `<!-- sdlc:context\n{…json…}\n-->` — via `editJiraIssue`. Readers check the entity property first, then the description block; whichever exists is the mémo.
- This supersedes the GitHub-based 9-stage and 5-column versions. The GitHub variant lives on the `sdlc-pipeline-5-columns` worktree branch for reference.
- Lighter than the repo's metaswarm workflow — don't auto-invoke metaswarm gates here unless asked.
