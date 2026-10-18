# T3 runner — delegated stages when the session runs in T3 Code

Read this when the session runs in **T3 Code** (the `delegate_task` tool is available, often as
`mcp__t3-code__delegate_task`) and `config.models` resolves a stage to anything but `main-agent`.
In T3, this runner is **preferred** for every model, Claude included, over `codex-runner.md`, the
`agy` CLI and the `Agent` tool.

Why it is preferred: each stage becomes a child thread of the ticket's thread in the T3 UI, so the
owner sees the lineage (which stage, which round and lens, which model) and can open the worker's full
transcript. A native `Agent` subagent does not show up that way. So give every task a `title` that
names the ticket, the stage, the round and the lens. A child with a vague title loses the benefit this runner
exists for.

## Resolve the target — from the live catalog

Call `orchestrator_capabilities` once per run (the result is large: query it with `jq`). Map the
model value to a provider and a catalog id:

| Model value | `providerInstanceId` | Catalog `model` id |
|---|---|---|
| `gpt-*` | `codex` | the value (`gpt-6.1-sol`) |
| `gemini-*` | `antigravity` | the value (`gemini-3.8-flash-high`) |
| `<vendor>/<model>` (e.g. `opencode/kimi-k2.7-code`) | `opencode` | the value |
| `opus-5.5`, `sonnet-5.5`, `haiku-5.5` | `claudeAgent` | `claude-<family>-<major>-<minor>` (`claude-opus-5-5`) |

The model id must be in that provider's `models` list, and the provider must have
`canRunChildTask: true`. If not, **stop and report** — never substitute. An unknown id fails
loudly with `model_unavailable` (observed 2026-10-16).

## The call

```json
{
  "task": "<the brief>",
  "target": { "providerInstanceId": "codex", "model": "gpt-6.1-sol",
              "options": { "reasoningEffort": "high" } },
  "interactionMode": "plan",
  "mode": "async",
  "role": "review",
  "title": "TKT-123 aiReview round 1 (initial)",
  "clientRequestId": "TKT-123-aiReview-r1-initial"
}
```

- **Always set `providerInstanceId`, `model` and the effort.** Without a provider, the child
  inherits this session's provider and model, so a cross-model review runs silently on the
  author's model. Without an effort, T3 uses its catalog default, and that default is `low` for
  `gpt-6.1-sol` and `medium` for `claude-opus-5-5` (catalog, 2026-10-16). The effort is the
  `:effort` suffix, else `high`. The option id comes from the catalog: `reasoningEffort` for
  codex, `effort` for `claudeAgent`. `gemini-*` has no option: the effort is in the id.
- **`interactionMode: "plan"` on every read-only stage** (all stages except `implement`). It is
  not a sandbox — see below.
- **Do not set `runtimeMode`.** A narrower mode makes the child wait for an approval that this
  thread cannot give.
- **One `clientRequestId` per stage, round and lens**, stable across retries of that call. A
  research call and the reviewer's re-run after it get their own (`…-r1-initial-research`,
  `…-r1-initial-2`).
- **Every review lens is a new `delegate_task` call with the full brief.** Never continue a child
  with `t3_thread_send`: the fresh context is what keeps the blind pass blind.
- **`mode: "async"`.** The completion wakes this thread: end the turn, do not poll. Launch A/B
  arms in one message.

## The brief — always name the ticket worktree by absolute path

The `task` string is the brief. There is no shell, so no interpolation risk. Inline the plan or
the diff. The two-lens round, the brief contents and the JSON result are the same as
`codex-runner.md` § Reviews and `reviews.md`.

**The child starts in the T3 thread's checkout, not in the ticket worktree.** A probe on
2026-10-16 showed a child starting in the main checkout on `main`, and reading and writing
absolute paths outside it with no approval prompt. So a brief with no path sends the worker to the
wrong tree, and it works there without complaint. Every brief names the ticket worktree's absolute
path and tells the worker to run every command with that directory as its working directory.
(Whether `EnterWorktree` moves the T3 binding is not verified. The absolute path makes the answer
irrelevant.)

## Read-only is NOT enforced

`interactionMode: "plan"` is an instruction to the child, not a sandbox. A probe on 2026-10-16
asked a codex child in plan mode to write a file: it declined "by instruction, before any tool
call", and no tool was refused. So: write READ-ONLY in the brief, set plan mode, and run the
post-stage tree check (SKILL.md § Hard rules) on the ticket worktree **and** on the T3 thread's
checkout.

## Write stages (`implement`)

Use `interactionMode: "default"` and `role: "implementation"`. The brief names the absolute
worktree path and forbids commits. After the run, `git status` the T3 thread's checkout too: a
stray edit there means the worker ignored the path. Then verify as for any delegated
implementation: read the diff, commit it yourself before you mutation-test it, run the tests,
mutate them, re-run the gate, and label that evidence as yours. Whether a T3 child can run Docker
or the gate is not verified: brief it to report which tests it ran and which it could not.

## Judging the output

Read the completion notification or `task_status`. `summary` is the result; a failed run puts the
provider error there.

- **The `providerInstanceId` and `model` in the result must equal the request.** Record them, the
  effort, and `harness: t3` in the stage comment.
- **The summary must be non-empty and contain a conclusive verdict.** The same rules as
  `codex-runner.md` § Judging the output apply. A researcher call and a reviewer's open research
  requests have no verdict: judge them per `reviews.md` § Researcher.
- **`hasPendingQuestion`**: the child asked something. If the ticket or the plan answers it,
  reply with `t3_pending_request_respond` (`threadId` = `childThreadId`). A human decision is
  yours to take to the human, per the gates.
- **`waitTimedOut` is not a failure.** Keep the `taskId` and read `task_status` later.
- When the output is doubtful, read the child's transcript in its T3 thread (`t3_thread_read` on
  `childThreadId`). It shows what the worker actually read.

## Escalation ladder

1. `delegate_task`.
2. The non-T3 runner for the same model (SKILL.md § config.models, "Outside T3" column):
   `codex-runner.md` for `gpt-*`, the `agy` CLI for `gemini-*`, the `Agent` tool for Claude
   models. Record the runner substitution in the stage comment. A model with no other runner
   (`<vendor>/<model>`) gets one `delegate_task` retry instead.
3. **Stop on the current `agent:*` label and report to the human.**

A **provider-level** failure (quota, revoked credentials) probably hits the other runner too: skip
rung 2.

## Continuation ownership

The primary agent keeps each task ID and owns the next step after its completion. Prefer the
app completion notification over a polling loop. Record the pending operation before yielding.
If independent work remains, continue it. If only a child result is pending, yield and resume on its
notification. Read the final message with `task_status`; a plan-mode provider may not write files.
A completed reviewer task is evidence to process, not a completed ticket or sprint.

Do not launch a second orchestrator to watch the first one. On handoff, name the new owner, pending
task IDs, current gate or CI watcher and exact next operation. Never leave two agents writing the
same board records or working tree.
