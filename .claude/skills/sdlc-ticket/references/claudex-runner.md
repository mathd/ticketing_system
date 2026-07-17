# Claudex runner — mechanics for stages that resolve to `<gpt-*>@claudex`

Read this when `config.models` resolves a stage to a model carrying the `@claudex` suffix
(e.g. `gpt-5.6-sol@claudex`). Same model as the codex runner — **only the harness differs**:
the stage runs headless Claude Code (`claude -p`) pointed at a local proxy that holds
**OpenAI credentials only**. Bare `gpt-*` values keep meaning the codex runner
(`codex-runner.md`); `@claudex` is opt-in, per stage, per repo. The point of the suffix is a
one-line A/B between harnesses on the same model — treat *which harness ran* as part of the
measurement and record it in the stage comment.

## Environment contract

The machine must provide `~/.config/claudex/env` (export lines for `ANTHROPIC_BASE_URL` and
`ANTHROPIC_AUTH_TOKEN`, pointing at a **loopback** proxy) and the proxy must answer:

```bash
( set -a; source ~/.config/claudex/env; set +a
  curl -fsS --max-time 5 "$ANTHROPIC_BASE_URL/v1/models" \
    -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" >/dev/null ) && echo up
```

If the file is missing or the check fails, **stop and report** (SKILL.md § Hard rules: never
silently substitute). In particular, never "fall back" to running the stage without the env —
that silently runs it on this session's native credentials and model, which is both a corrupted
measurement and the wrong bill.

## Invocation — headless, allowlisted

The model literal is **`gpt-5.6-sol`** (plain `gpt-5.6` does not resolve — don't "correct" it).
Write the brief to a file (same materialization rules as codex-runner § Prompt files: file-writing
tool or quoted heredoc, never shell interpolation; collision-resistant name; delete what you created).

```bash
# read-only stages: plan drafting, plan review, ai-review
( set -a; source ~/.config/claudex/env; set +a
  claude -p --model gpt-5.6-sol \
    --effort high \
    --allowedTools "Read,Grep,Glob" \
    < .brief.$$.md )
```

`--effort` takes `low|medium|high|xhigh|max`. When the config value carries an `:effort`
suffix (`gpt-5.6-sol:low@claudex`), pass it through; otherwise use `high`.

**Fix-diff re-review passes may run one notch lower** (e.g. `medium` when the first pass ran
`high`): the second-plus passes judge a small fix diff, not the whole change, and at `high`
each pass costs ~10–20 min of wall clock regardless of diff size (TKT-92: four passes on
diffs down to 30 lines). Keep the *first* full-diff pass at the configured effort — only
re-reviews of fix commits get the discount, and say in the stage comment which effort ran.

- **The allowlist IS the sandbox.** Headless `claude -p` denies tools that aren't pre-approved,
  so `--allowedTools "Read,Grep,Glob"` is what makes the stage read-only. Omitting the flag is
  the claudex equivalent of the codex runner's dropped `-s read-only` — the substitute gets more
  privilege than the command it stands in for, and a reviewer that can edit the tree it is
  reviewing is not a reviewer.
- **Inline the plan/diff in the brief.** No Bash in the allowlist means the worker cannot run
  `git diff` itself — same rule as codex-runner (d), enforced here by construction.
- **Background + poll** via the harness (`run_in_background: true`), same as codex `task`.

## Reviews under @claudex

There is no claudex equivalent of Codex's built-in `adversarial-review`: a review here is
5.6 + your review brief in the Claude Code harness. All SKILL.md review rules apply unchanged —
prime for guilt, diff only, no plan, verify findings against the revision under review. Flag in
the stage comment that the reviewer ran `@claudex`: swapping the built-in reviewer for a
prompt-driven one is a semantic change to the experiment, not just plumbing.

## Scope and fallback

- **v1 covers read-only stages only** (`plan`, `planReview`, `aiReview`). `implement@claudex`
  is undefined — don't improvise a writable allowlist; extend this doc first.
- On failure or hang, fall back to the **codex runner** for the same stage and say so in the
  stage comment. A substituted harness is a measurement note, never a silent detail.
- **Provider-level failure ≠ harness failure.** If the *credentials or quota* are what died
  (OpenAI tokens exhausted, key revoked), the codex fallback hits the same wall — don't burn a
  retry proving it. Escalate to the owner for a directed substitute model; run it preserving the
  reviewer ≠ author invariant, and record the substitution in the stage comment *and* the
  review-guide as part of the measurement. Note the env check above proves the proxy answers,
  not that quota remains — a mid-stage credit exhaustion passes the pre-flight (TKT-68).

## Known gotchas (observed July 2026)

- Subagent **effort inherits** from the main invocation — pin effort deliberately; ultra-level
  effort cascades to every subagent and burns tokens for nothing.
- Long-running 5.6 subagents have been seen dying **out of context without compacting** (the
  harness may assume a 200k window). Keep briefs lean; prefer one-shot headless calls over
  long interactive sessions.
- 5.6 occasionally fumbles Claude Code's markdown/link formatting conventions — cosmetic,
  but don't mistake it for a failed step (verify the artifact, not the rendering).
