# Codex runner — `gpt-*` stages outside T3

Read this when `config.models` resolves a stage to a `gpt-*` model **and the session does not run
in T3 Code** (no `delegate_task` tool). In T3, use `t3-runner.md` instead (SKILL.md § config.models).
*Which* stage is the config's call; this file is only *how* to run it.

## Invocation — `/codex:rescue`, for every stage

Run every `gpt-*` stage through the codex plugin's **`/codex:rescue`** command (Skill tool, skill
`codex:rescue`). It forwards to the companion's `task` command. Do not call the `codex` CLI,
`codex exec` or the companion script yourself. `/codex:review` and `/codex:adversarial-review`
are human-only: do not use them.

```
/codex:rescue --fresh --background --model gpt-6.1-sol --effort high Read the brief at <abs path>/.brief.TKT-123-aireview-1.md and follow it. READ-ONLY: do not edit, create or delete any file. Do not commit.
```

- **`--fresh` always.** Each stage and each review lens starts a new Codex thread. A resumed
  thread carries the previous pass into a pass that must be blind.
- **`--model` and `--effort` always.** The model literal is the config value left of `:`
  (`gpt-6.1-sol`; plain `gpt-6.1` does not resolve — don't "correct" it). The effort is the
  `:effort` suffix, else `high`. The forwarder leaves both unset when you omit them, and the run
  then takes whatever `~/.codex/config.toml` says.
- **Say READ-ONLY in the request text, not only in the brief.** The forwarder adds `--write` unless
  the request asks for read-only work. Every stage except `implement` is read-only.
- **`--background`**: the result arrives as a notification. Do not poll.

**Check the job record before you trust the output.** The companion writes
`~/.claude/plugins/data/codex-openai-codex/state/<workspace>-<hash>/jobs/<id>.json`. It holds
`write`, `request.model`, `request.effort` and `request.cwd`. A read-only stage with
`"write": true`, a different model, or a different effort is a failed stage.

## Briefs — a file, never interpolated

Write the brief with the Write tool, never through a shell command: ticket text and code contain
backticks and `$(…)`, which a shell executes. Name it `.brief.<ticket>-<stage>[-<pass>].md` in the
ticket worktree root, and delete only the files this run created. Inline the plan or the diff in
the brief, and give absolute paths. Keep it lean: a huge brief can be dropped from context.

## Reviews — a defect-hunting `task`, two separate runs per round

A review is a read-only run with a brief that primes for guilt: "assume the diff is wrong; find
where". Each round (`reviews.md` § One round) runs it twice, with separate briefs, on the same
frozen artifact:

1. **Initial inspection** — the diff only, no plan, no rationale. Record its findings before step 2.
2. **Decision audit** — the approved plan, every `kind=decision` comment, the diff, the initial
   findings and the verification evidence, with the questions in `quality-practices.md` §2.

Every brief names the stage, round, lens, artifact ID and the actual author models, and asks for
one JSON object shaped like `assets/review-result.example.json` (inline the shape: the worker
cannot open skill files). If `config.models.research` is set, the brief also says the reviewer may
return `researchRequests` with `complete: false` (`reviews.md` § Researcher). Name the brief
`.brief.<ticket>-<stage>-r<round>-<lens>.md`.

Reusing one brief for both lenses destroys the blind pass. (Why `task` and not `adversarial-review`: the
latter cannot pin a model (TKT-144) and returned zero bytes twice on TKT-267 where a `task` brief
returned code-cited findings.)

## Implementation by Codex — the orchestrator finishes it

The Codex `--write` sandbox denies Docker, local listeners (`httptest`) and often `.git` writes.
Brief the implementer to write all the tests, report which ones it could not run, and NOT commit.
Then read the diff, commit it yourself BEFORE you mutation-test it, run the tests, mutate them, and
label that evidence as yours (TKT-144, TKT-147).

## Judging the output — completed ≠ ran

- **Assert the output is non-empty and contains a verdict before you read it for findings.** (A
  researcher call returns evidence, not a verdict, and a reviewer may return open research
  requests instead: judge those per `reviews.md` § Researcher.) An
  empty result is a review that did not run, and it looks exactly like a clean review (TKT-267:
  `Phase: done` with zero bytes). Output that ends mid-sentence is truncated: a failure, not a
  partial pass.
- **Pass = the reviewer ran against the intended scope and returned a conclusive verdict**, not
  "it found defects". A conclusive "no findings" on a clean diff is a pass. A rubric template with
  no findings is a failure.
- **A local skill can hijack the prompt and cannot be disabled.** Codex loads the machine's skills
  on "review" or "critique" prompts, possibly an unrelated client's. A hijacked run that returns
  concrete, code-cited findings is a pass (TKT-67). Do not blame the hijack for an unrelated hang.
- `status` can say **"No job found"** while the job file says `running`: trust the file and the
  log's mtime (TKT-84). **"Selected model is at capacity"** means the model was never reached: a
  rung failure, never a reason to substitute a model.
- **Never substitute a self-review.** A skipped review that looks done is worse than a stalled
  ticket.

## Escalation ladder — fixed, not improvised

One attempt per rung, in order:

1. The `/codex:rescue` run.
2. On empty, truncated, hung or mis-configured output (job record wrong) → one `--fresh` retry.
3. Still no conclusive verdict → **stop on the current `agent:*` label and report to the human**.

No rung twice, no fourth rung (TKT-62: ~1h lost to improvised retries). If a failed run left
legible reasoning, quote it in the stage comment and verify its open threads **yourself, labelled
as your own work** — that is authorship, not a substituted review.
