# S-2 autonomous start prompt

Open a Codex thread with gpt-6.1-sol at high effort, then copy the text below into it. Using it approves the named scope and starts the sprint; this preparation has not done so. It does not approve sprint closure in advance.

```text
First record your actual provider, model and effort. This run requires codex/gpt-6.1-sol at high effort. Stop before board mutations if that is not your actual identity. A prompt cannot change your session model.

Use the prepared sdlc-sprint, sdlc-ticket and sdlc-sprint-review skills in /home/mathieu/sources/ticketing_system/.claude/skills/.

I approve the proposed S-2 brief in board record S-2 sha256:1da0a6907729adc8a98c0eca2522cfaa9119e86a4a8b3b84cbecf002b455a5e0, mirrored in docs/sprints/S-2/preparation.md (file SHA-256 ac48fe538669aa84149fa9514d00d81a37385fe890c46a138804a6a21ed158c1), for scope digest 475b7c77f414e3205adb736c856fb913675f56cbdd7f58603ca28b46ae061186. Verify that the board still matches before starting. Dates: October 19–November 2, 2026. Full final sprint review and a committed English HTML report are required. Publish no tag and do not deploy.

Start S-2 and own the autonomous run through all five members: TKT-559, TKT-399, TKT-551, TKT-552, then TKT-563. Reuse the recorded owner decisions for TKT-399 rounds 8–11 and host Chrome measurements, keeping venue qualification in TKT-560. Implement TKT-399 fresh from origin/main. Take no code or fixes from 24f1c209, its abandoned branch or PR #451. Keep their review history. Normalize actual author identities and include every contributor to the reviewed code; never treat two aliases of Opus as independent models.

I approve the priorities of these exact five members once. Record my sprint-priority approval against the approved brief digest, scope digest and member list. You may record each move to Ready only after its readiness checks pass and its blockers are clear. This approval includes the closure task and excludes extra members. A new correction ticket or scope change needs my approval.

Use gpt-6.1-sol high for orchestration, planning and all implementation in this run, including UI and the Entry-copy changes. For this run, I explicitly assign that work to Codex instead of my default Claude routing. Use claude-opus-5-5 high for independent reviews through T3 delegate_task, provider claudeAgent, interactionMode plan, role review. Record these per-run overrides and keep each actual reviewer different from every actual author.

Preserve the primary checkout and unrelated local commits. Use wt worktrees from current origin/main for product work, but use the prepared canonical skills and their scripts from the primary checkout. Recheck gh auth status for mathd before gh operations. Pin the renderer, template and labels at start and record their hashes. If they remain uncommitted, publish the exact runtime under the report evidence directory and verify the committed report can be rendered with it.

I authorize the scoped commits, pushes, PRs and merges needed for these five members after required checks pass. Never use --admin. Follow the existing review repeat thresholds and caps. Fix blocking findings; do not override High/Critical findings. Do not add routine work-unit checkpoints. Record evidence, decisions and observed time in /tmp/sdlc-S-2-operations.jsonl and on the board at hand-offs. Reuse authorization for routine reversible steps.

After the four functional tickets are Done, continue directly into TKT-563. Run the combined journeys and all four final review areas, with the documentation audit after the other areas and report draft. Then publish and verify docs/sprints/S-2/report.en.html through its report PR, and post matching board pointers. Do not stop with only ticket closeouts or a chat summary.

Stop only for an owner-only decision, an exhausted authorized cap, an unfixable failed check, an unauthorized destructive action, or the final exact sprint close approval. At that final request, provide the committed report link and digest, candidate, current approved brief and scope digests, checks, review verdicts and residual issues. Keep S-2 active and TKT-563 open while approval is pending. After I approve those exact targets, finish PO validation and metrics, move TKT-563 to Done, run the closure checker, close S-2 and verify the board read-back.
```
