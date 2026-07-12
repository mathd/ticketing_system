# Quality practices — plan, review, hand-off, course-correction

Four practices applied at specific points of the pipeline. Each is cheap; skipping them is how plans bounce, PRs bloat, and reviews stall. (Comment-marker formats are in `SKILL.md` § Memory & metrics.)

## 1. Pre-mortem pass on plans (in `agent:planning`, before the Codex critique)

**Pick the test seam first.** Before drafting steps, decide *where* this feature is exercised: prefer an **existing** seam over a new one, the **highest** seam that still isolates the change, and the **fewest** seams possible (ideal: one). Record the chosen seam in the plan and, if it's a new one, why no existing seam fit — the executing agent tests through it, so a wrong seam is a rewrite. (Seam = a place you can alter behaviour without editing there — Feathers.)

**Grill the human on the open decisions (complex tickets, when interactive).** For anything not `risk:low`, before the pre-mortem, put the plan's unresolved *decisions* to the user **one question at a time**, each with your recommended answer, and wait for each. Look **facts** up in the code yourself — only genuine decisions go to the human. This extracts human context the pre-mortem (author-internal) and Codex (fresh-context) both miss. Skip it for `risk:low` or when the user chose *gates only* drive style.

After drafting the plan, re-examine it through one **named** reasoning lens — a generic "double-check it" re-confirms; a named lens forces an angle of attack.

Default lens — **pre-mortem**: *assume this ticket bounced — the plan was rejected at Gate 2, or the PR at Gate 3. Work backward: what went wrong?* List the 3–5 most plausible failure causes (wrong integration point, missing migration, untestable AC, hidden coupling, scope too big…), then patch the plan where a cause is credible.

If pre-mortem fits poorly: **inversion** (what would guarantee failure? avoid that), **first principles** (strip assumptions, rebuild), **stakeholder mapping** (re-read as the reviewer/user/on-call). One lens, one pass — a second look, not a committee.

Record one line in the `plan-final` comment: lens applied + what it changed (or "no changes").

Why both this and Codex: the pre-mortem catches wrong-approach risk from inside the author's full context; the Codex critique attacks feasibility from fresh context with no access to the reasoning. They miss different things.

## 2. Findings triage (in `agent:ai-review` and at Gate 3 feedback)

Every finding — from the Codex adversarial review or the human reviewer — gets classified before acting:

- **Blocking** — correctness, security, or AC violation *in this diff* → fix now, in the PR.
- **Incidental** — real but out of scope (pre-existing issue, adjacent improvement, nice-to-have) → **create a Backlog issue** (`createJiraIssue`, status Backlog), link the finding in its body, note the deferral in the review reply.
- **Rejected** — false positive or nitpick → say so explicitly, with the reason. Adversarial reviewers are instructed to find problems, so some won't exist; filtering is part of the job.

Invariant: **no finding is silently dropped.** Each one changes the diff, produces a ticket, or gets a stated rejection. Fixing incidental findings in the PR is scope creep — it bloats the diff, slows review, and the SHA churn invalidates earlier review effort.

## 3. Ready-for-review walkthrough (the Gate 3 hand-off, on the PR)

The "ready for review" comment **on the PR** (code repo, not Jira) is a guided walkthrough, not a pointer at the diff. The reviewer's time is the pipeline's scarcest resource; structure it (marker on line 1 — see `SKILL.md`):

```markdown
<!-- sdlc:stage=ai-review kind=review-guide -->
### 🔎 Review guide — <ISSUE-KEY>

**Scope:** <n> files, +<x>/−<y> — <one sentence: what this PR does>.

**Walkthrough** (by design concern, not by file):
1. <concern> — `src/foo.ts:42` — what to check and why it's shaped this way
2. <concern> — `db/…` — …

**High-risk spots:** <each tagged `[security]` `[schema]` `[data]` `[concurrency]` `[perf]` + location — or "none">
**Manual checks:** <1–3 observable verifications the reviewer can run>
**Deferred (triage):** <Backlog tickets from incidental findings, or "none">
```

Order the walkthrough by risk: the thing most likely to be wrong goes first.

## 4. Correct-course: regenerate from the failing layer

When a human pushes back (plan rejected at Gate 2, changes requested at Gate 3) or a stage keeps failing, **diagnose which layer broke before touching anything**:

| Layer | Symptom | Action |
|---|---|---|
| **Intent** | the issue/AC itself is wrong, stale, or ambiguous | Back to scope: fix the issue with the user (Backlog work), then re-plan |
| **Plan** | AC fine, approach wrong | Swap to `agent:planning`, regenerate the plan — don't patch code toward a broken plan |
| **Implementation** | approach fine, code wrong | Swap to `agent:coding`, fix in place |

Corrections regenerate from the source layer — never patched locally at a lower one (patching code to satisfy feedback that's really a plan problem produces code that fights its own design). The label/transition mechanics for moving backward are in `SKILL.md`; this is the decision rule for *how far back*. State the diagnosis in the comment when resuming: "treating this as a plan-layer issue: <why>".
