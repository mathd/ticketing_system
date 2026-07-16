# A/B stages — `plan` and `aiReview` as harness/model experiments

Read this when `config.models` gives `plan` or `aiReview` an **array** of model values
(e.g. `"aiReview": ["gpt-5.6-sol", "gpt-5.6-sol@claudex"]`). An array means: run every arm
on the same input, score them, and record the comparison. A single string keeps today's
behavior exactly — arrays are opt-in per stage, per repo.

Ground rules for any A/B stage:

- **Only `plan` and `aiReview` accept arrays.** An array on `planReview` or `implement` is a
  config error — stop and report (implementation racing belongs to a dedicated racing skill
  with worktree isolation, not this pipeline).
- **Every arm independently satisfies the cross-model invariant** (reviewer ≠ author, planner
  ≠ plan-reviewer). Check each element as if it were the sole value.
- **Each arm runs by its own runner rules** — bare `gpt-*` per `codex-runner.md`, `@claudex`
  per `claudex-runner.md`, Claude models as subagents. Same brief, same input artifact,
  launched in parallel (all arms are read-only, so no isolation is needed).
- **Arms may vary effort** via the `:effort` suffix (SKILL.md § config.models) — e.g.
  `["gpt-5.6-sol:high@claudex", "gpt-5.6-sol:low@claudex"]` asks "is low effort enough?".
  Keep it **one variable per experiment**: arms that differ in both harness *and* effort
  (or model *and* effort) produce a comparison you can't attribute — pick one axis.
  For `aiReview`, effort-varied arms must all be `@claudex` (codex's `adversarial-review`
  has no effort flag). Record each arm's effort in the tally and in the log line's
  `model` string.
- **One arm failing doesn't abort the stage.** The surviving arm's output drives the pipeline;
  the failure is logged as that arm's result (a harness failure is a finding about the harness).
- **Record everything twice:** the comparison always goes in the **stage comment** (the
  pipeline's own record — the skill must work without any personal setup), and additionally
  append one line to `~/.claude/ab-tests/results.jsonl` *if that file exists* (schema in
  `~/.claude/ab-tests/README.md`; use the arm string as the `model` field, `type` =
  `"review"` or `"plan"`). Log aborted/failed arms too.

## `aiReview` — score by verified findings

1. Run all arms in parallel on the same diff. Each is primed for guilt and gets the diff
   only (SKILL.md § Hard rules) — identical briefs except the runner mechanics.
2. Merge findings, tagging each with its arm. **Dedup by defect** (same file/behavior/root
   cause), crediting every arm that found it.
3. Verify the merged set once, mechanically, per the existing hard rule (against the revision
   under review, verdict per finding). This verification is the judge — it is grounded in
   code and blind to which arm produced the finding. Do not add an LLM judge on top.
4. Tally per arm: confirmed / refuted / unique-confirmed (found by this arm alone). Put the
   table in the stage comment. Fixes proceed from the union of confirmed findings, exactly
   as a single review's would.
5. **Second passes are never A/B'd.** A re-review of a fix diff runs on the **first array
   element only** (the control). One variable, measured once, at the first pass — A/B'ing
   the second pass would score arms on different diffs.

## `plan` — selection by the human gate

1. Run all arms in parallel from the same brief (read-only drafters, per their runner docs).
2. Present the drafts to the human at the plan-approval gate **anonymized** — labeled A/B,
   no model or harness names in the presented artifact. Assign labels deterministically:
   even ticket number → first arm is A; odd → first arm is B. (No randomness available —
   and don't need it; alternation kills position bias across tickets.)
3. The human picks (or asks for a merge — the merged plan counts as a win for the arm that
   contributed its spine; say which and why in the comment). Reveal the mapping in the stage
   comment only *after* recording the decision.
4. The winner becomes *the* plan: `planReview` and everything downstream run on it alone,
   unchanged. Log winner + the human's one-line reason.
5. **`risk:low` tickets don't A/B the plan** — the approval gate is skipped there, so there
   is no judge. Run the first array element only and note the skip in the stage comment.

## Cost note

Every A/B'd stage is N× that stage's spend. That is the price of the measurement — but don't
pay it silently: the stage comment states that an A/B ran and how many arms. If budget or
time is tight, drop back to a single value; a half-run A/B (one arm rushed, one thorough)
is worse than none because it logs a biased comparison as if it were data.
