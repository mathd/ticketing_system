# Codex runner — mechanics for stages that resolve to a `gpt-*` model

Read this when `config.models` resolves a stage to a `gpt-*` model. *Which* stage that is
is the config's call (`SKILL.md` § config.models); this file is only *how* to run it.

## The companion script — not `codex exec`, not slash commands

Where `codex@openai-codex` is installed, call the plugin's **companion script** directly — it is
agent-invocable and works every time. The `/codex:*` slash commands are
`disable-model-invocation: true` (human-only) and trigger inconsistently; raw `codex exec` is
the fallback of last resort (§ below).

```bash
CODEX=~/.claude/plugins/marketplaces/openai-codex/plugins/codex/scripts/codex-companion.mjs
# free-form work (plan draft / plan critique / implementation) — `task` takes a model.
# Read-only stages: NO --write. Implementation: --write.
# (The raw-fallback equivalent of "no --write" is `-s read-only` — see § Raw codex exec (b).)
node "$CODEX" task --prompt-file .brief.$$.md --model gpt-5.6-sol --effort high --background
node "$CODEX" status <job-id> ; node "$CODEX" result <job-id>
# ai-review — the PR diff vs the base branch
node "$CODEX" adversarial-review --base origin/main --scope branch "<focus: what to attack>"
```

- **`adversarial-review`** is the review command: it takes focus text and challenges the
  approach. Plain `review` is native-review only and **rejects focus text entirely**. Both map
  to Codex's built-in reviewer — no `-m` flag to get wrong; args `--base <ref>`,
  `--scope auto|working-tree|branch`.
- **`task` is the only command that takes a model** — pass `--model gpt-5.6-sol` explicitly.
  **The literal is `gpt-5.6-sol`; plain `gpt-5.6` does not work.** Don't "correct" it.
- ⚠ **`--background` is a lie for reviews** — parsed and then ignored (verified in the script:
  `review`/`adversarial-review` unconditionally call `runForegroundCommand`; only `task` honours
  it). `status`/`result` polling does **not** work for review jobs, and a stalled review blocks
  the caller. Instead: background the review through the harness
  (`Bash(..., run_in_background: true)`) or wrap it in `timeout` so a hang is caught — but note
  **`timeout` is not on macOS** (it is GNU coreutils; `gtimeout` only if the user installed it), and
  a missing binary makes the whole call exit 127 having never reached the model. Prefer harness
  backgrounding, which works everywhere; that is a harness failure, not a rung failure, so re-launch
  rather than escalating the ladder (TKT-99). `--wait`
  is equally redundant — foreground is the only mode.

## Prompt files — materialized, never interpolated

Write the brief with a **file-writing tool or a quoted heredoc**, never by interpolating
ticket/plan text into a shell command — ticket text and inlined code routinely contain backticks
and `$(…)`, which a double-quoted `printf`/`echo` will **execute**. Use a collision-resistant
name (`.plan-brief.$$.md`), delete only what this run created, and keep the file lean (plan + the
code actually under discussion) — a huge file risks being dropped from context.

## Raw `codex exec` — fallback only, when the plugin is absent

- **(a)** Pass `-m gpt-5.6-sol` explicitly — the CLI default errors `requires a newer Codex`.
- **(b)** Pass **`-s read-only`** on every read-only stage — which is every stage this fallback
  serves: plan drafting and all reviews. The companion enforces read-only by *omitting* `--write`;
  raw `codex exec` enforces nothing and takes the sandbox from `~/.codex/config.toml`. So a
  fallback that drops this flag hands the substitute MORE privilege than the command it stands in
  for, and a reviewer that can edit the tree it is reviewing is not a reviewer. (Restored after
  TKT-67: the flag was in the pre-`codex-runner.md` SKILL.md and was lost when these mechanics were
  extracted. Nothing caught it, because the stage it protects had already failed for an unrelated
  reason.)
- **(c)** Feed the prompt via **stdin redirect** (`< prompt.txt`), never as a positional arg — a
  large arg hangs at `Reading additional input from stdin…`.
- **(d)** **Inline the plan/diff in the prompt file** rather than telling codex to `git diff`
  itself.
- **(e)** Run it **backgrounded + poll**.
- **(f)** **A local skill will hijack the prompt and cannot be stopped** — "review"/"critique"
  loads the machine's skills (possibly an *unrelated client's*); neither `--disable plugins` nor
  `-c features.skills=false` prevents it (`--disable skills` isn't a valid flag). The hijack also
  happens *through the plugin*. Survivable, not preventable — judge the output (§ below).
  **Believe "survivable" literally**: on TKT-67 the two runs that *completed* were hijacked exactly
  as hard as the two that died, and produced 1 critical + 4 important findings through the foreign
  rubric. A hijacked run is not a failed run, and blaming the hijack for an unrelated hang is the
  easy mistake — it is the loudest thing in the log.

## Judging the output — exit 0 ≠ success

**`$CODEX` is NOT set in a fresh shell.** Shell state does not persist between `Bash` calls, so
`node "$CODEX" …` expands to `node ""` — which **exits 0 and does nothing**, printing not one byte.
Invoke the companion by absolute path instead, resolving it once per session:
`node /home/mathieu/.claude/plugins/cache/openai-codex/codex/<version>/scripts/codex-companion.mjs …`
(`find ~/.claude/plugins -name codex-companion.mjs` if the version moved). A completely empty
tool result is the signature of this failure, not of a quiet success.

**A codex invocation is the ONLY command in its `Bash` call.** Never chain it after anything — no
`python3 … && node "$CODEX" …`, no `git push; node "$CODEX" …`, no leading `cd`. When chained, the
first command runs and **the codex call silently does not**: no error, exit 0, an output file a few
bytes long. This happened **three times in one ticket** (TKT-238), each time caught only by reading
the artifact — and a review that never ran is indistinguishable from one that found nothing, which
is the single most dangerous failure this pipeline has, because the whole cross-model invariant
rests on it having executed. Set the working directory with the tool's own facility or absolute
paths, and put every preparation step in a separate call *before* the one that launches codex.

**Read the output; never judge by exit status** (raw `exec` exits 0 on both a zero-findings
hijack and a bad flag that never reached the model). Pass = **the reviewer actually ran against
the intended scope and returned a conclusive verdict** — *not* whether it found defects. A
conclusive "no findings" on a genuinely clean diff is a PASS; a run that reviewed the wrong
scope, or returned only a rubric template, is a failure however many sections it printed. A run
that announces a foreign skill but returns concrete, code-cited findings is likewise a pass — the
rubric is irrelevant. A hang counts as a failure: kill it fast. Never substitute a self-review; a
skipped review that looks done is worse than a stalled ticket.

## Escalation ladder when a delegated stage fails — fixed, not improvised

One attempt per rung, in order:

1. The plugin command (`adversarial-review` / `task`).
2. On empty, truncated, or hung output → the documented raw fallback above.
3. Still no conclusive verdict → **stop on the current `agent:*` label and report to the human**.

No rung twice, no fourth rung — improvised retries cost wall-clock (TKT-62: ~1h, three attempts).
Two more shapes (TKT-84): the companion's `status`/`result` can report **"No job found"
while the job's JSON state file still says `running`** — trust the file
(`…/state/<repo-hash>/jobs/<id>.json`) plus the log's mtime, not the status command; and raw
`codex exec` can fail with **"Selected model is at capacity"** — the model was never reached,
which is a rung failure, not a reason to substitute a model.
Known failure shapes, all exiting 0: the plugin returning **zero bytes**; the companion
**truncating** the captured assistant message mid-sentence (the verdict survives, the findings
don't); `codex exec` **dying mid-investigation** with no final answer. If a partial run left
legible reasoning, salvage it — quote it in the stage comment and verify its open threads
**yourself, labelled as your own work** (that's authorship, not a substituted review).
